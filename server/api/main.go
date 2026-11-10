package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	// Embed the IANA zone database so PULS_TIME_ZONE resolves even in an
	// image without /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	shutdownTimeout = 15 * time.Second
	// Ceiling on a single handler's database work. Generous enough for the
	// widest legitimate query, short enough that a stuck one returns its
	// pool connection. /v1/export is exempt (see routes).
	handlerTimeout = 30 * time.Second
	defaultLimit   = 50
	maxLimit       = 200
	// /v1/metrics/daily pages in days across the requested types (see
	// DailyFilters). The default holds a year of 27 types, or a decade of
	// two, so a request that never names a limit gets what it always did;
	// the cap keeps one answer well under a few megabytes.
	defaultDailyLimit = 10000
	maxDailyLimit     = 50000
	catalogTTL        = 5 * time.Minute
	// How long past catalogTTL a cached catalog is still served while a
	// background refresh replaces it, and how long that refresh may take.
	catalogMaxStale       = time.Hour
	catalogRefreshTimeout = 2 * time.Minute
	// How many users' catalog answers are cached at once (see
	// storeCatalogTypes). Far more than a household; small enough that a
	// caller spraying ?user= values holds nothing worth mentioning.
	catalogCacheMaxUsers = 32
	// Accepted range for epoch-millisecond query parameters: 1970-01-01 to
	// 9999-12-31T23:59:59.999Z, the widest span the `date` casts can carry.
	minEpochMS int64 = 0
	maxEpochMS int64 = 253402300799999
	// How many identifiers one types list may name. The catalog has under
	// 200 types, and each named one is its own scan of the hypertables.
	maxTypesPerRequest = 50
	// What server/.env.example ships every secret as; see refusePlaceholder.
	placeholderSecret = "change-me"
)

type apiStore interface {
	Ping(context.Context) error
	ReadScope(context.Context, *apiAuthority) (context.Context, func(), error)
	Users(context.Context, string) ([]User, error)
	// Every other read takes the user it is for as its second argument —
	// explicit rather than baked into the store, so a handler cannot forget
	// it and a fake can record which user it was asked about.
	Profile(context.Context, string) (*Profile, error)
	CatalogTypes(context.Context, string) ([]CatalogType, error)
	LatestMetrics(context.Context, string, []string) ([]LatestMetric, error)
	DailyMetrics(context.Context, string, DailyFilters) ([]DailyMetric, error)
	ActivitySummary(context.Context, string, time.Time, time.Time) ([]ActivityDay, error)
	Workouts(context.Context, string, WorkoutFilters) ([]WorkoutSummary, error)
	Workout(context.Context, string, string) (*WorkoutDetail, error)
	SleepDaily(context.Context, string, time.Time, time.Time) ([]SleepNight, error)
	Samples(context.Context, string, SampleFilters) (*SamplesPage, error)
	WorkoutSeries(context.Context, string, string, []string, int) (*WorkoutSeriesResponse, error)
	StateOfMind(context.Context, string, time.Time, time.Time) ([]StateOfMindEntry, error)
	// The last N calendar days as one small document (see summary.go).
	Summary(context.Context, string, int) (*SummaryData, error)
	// The export's paths: the type behind /v1/samples resolved on its own,
	// so a bad identifier is a 400 before the download starts, and the two
	// row-at-a-time scans a whole range is streamed through.
	SampleType(context.Context, string) (SampleMeta, error)
	StreamSamples(context.Context, string, SampleMeta, SampleFilters, func(Sample) error) error
	StreamWorkouts(context.Context, string, WorkoutFilters, func(WorkoutSummary) error) error
}

type Server struct {
	store     apiStore
	authorize func(context.Context, string) (*apiAuthority, error)
	log       *slog.Logger

	// The calendar zone (PULS_TIME_ZONE) instants are rendered in where a
	// response is prose rather than JSON — the markdown summary. The store
	// cuts its days in the same zone. Nil means UTC (a Server built as a
	// struct literal, every test).
	loc *time.Location

	// Whether X-Forwarded-* may be believed: for the rate-limit key, and for
	// the host the OpenAPI document advertises. Off unless a trusted proxy
	// (which appends to X-Forwarded-For; the last entry is the key) is the
	// only thing that can reach this port.
	trustProxyHeaders bool

	// Per-client-IP auth-failure buckets, made on first use so a Server built
	// as a struct literal (every test) still has one.
	limiterOnce sync.Once
	failures    *failureLimiter

	// Last known database status for the unauthenticated /healthz, so its
	// request rate cannot drive pool acquisitions (see health.go).
	health healthCache

	// Catalog results belong to one member and one immutable API credential.
	catalogMu sync.Mutex
	catalog   map[string]catalogEntry

	// Whether a background catalog refresh is running (see
	// refreshCatalogTypes; at most one at a time, under catalogMu), and the
	// group tests wait on for it to finish.
	catalogRefreshing bool
	catalogRefreshes  sync.WaitGroup

	// The bounded set of /v1/export slots (see maxConcurrentExports), made on
	// first use so a Server built as a struct literal still has one.
	exportOnce  sync.Once
	exportSlots chan struct{}

	// Cancelled once shutdown's grace has run out (see run): every handler's
	// context and every background catalog refresh ends with it. Nil (a
	// Server built as a struct literal) never ends.
	lifetime context.Context
}

func main() {
	// `api healthcheck [addr]` is the Compose healthcheck (healthcheck.go):
	// the image is distroless, so there is no curl to ask /healthz with.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":8081"
		}
		os.Exit(runHealthcheck(os.Args[2:], addr, os.Stderr))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	authorityConfig, err := loadAPIAuthorityConfig(os.Getenv)
	if err != nil {
		return err
	}
	dbURL, err := readDatabaseURL(os.Getenv)
	if err != nil {
		return err
	}
	// Same switch, same default (off), same meaning and same spelling rule
	// (envbool.go) as ingest's: only behind a trusted proxy, and then the LAST
	// X-Forwarded-For entry. It decides both the rate-limit key and the host
	// the OpenAPI document advertises.
	trustProxyHeaders, err := parseBoolEnv("TRUST_PROXY_HEADERS", os.Getenv("TRUST_PROXY_HEADERS"), false)
	if err != nil {
		return err
	}
	// The calendar zone for the daily endpoints. Same value the database's
	// puls.time_zone setting holds (db/migrations/013_time_zone.sh), so the API's
	// day ranges and metric_daily's day column agree. Fail fast on a typo
	// rather than serve misaligned days; checked against the database below.
	loc, err := loadTimeZone(os.Getenv("PULS_TIME_ZONE"))
	if err != nil {
		return err
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8081"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := connectWithRetry(ctx, dbURL, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	var databaseRole string
	var scopedViews bool
	if err := pool.QueryRow(ctx, "SELECT current_user, to_regprocedure('api.read_scope()') IS NOT NULL").Scan(&databaseRole, &scopedViews); err != nil {
		return fmt.Errorf("personal API database: %w", err)
	}
	if databaseRole != "api_reader" || !scopedViews {
		return errors.New("personal API requires api_reader and credential-scoped views")
	}

	// A zone that disagrees with the database's would cut every daily answer
	// on different days than metric_daily and the dashboards, silently.
	if err := verifyTimeZone(ctx, func(ctx context.Context) (string, error) {
		var name string
		err := pool.QueryRow(ctx, `SELECT puls_time_zone()`).Scan(&name)
		return name, err
	}, loc, logger); err != nil {
		return err
	}

	// Handlers outlive their requests' cancellation only until shutdown's
	// grace has run out: this is cancelled then (see the end of run).
	lifetime, endLifetime := context.WithCancel(context.Background())
	defer endLifetime()

	store := NewStore(pool, loc)
	srv := &Server{
		store: store,
		authorize: func(ctx context.Context, bearer string) (*apiAuthority, error) {
			return store.authorizeAPI(ctx, bearer, authorityConfig.Issuer, authorityConfig.PublicKeyFile)
		},
		log:               logger,
		loc:               loc,
		trustProxyHeaders: trustProxyHeaders,
		lifetime:          lifetime,
	}
	logger.Info("starting",
		"addr", addr,
		"time_zone", loc.String(),
		"trust_proxy_headers", trustProxyHeaders,
	)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = httpSrv.Shutdown(shCtx)
	// Whatever is still running has had its grace. A handler's query is
	// cancelled and its connection returned; an export still streaming is
	// aborted, never ended as if the file were complete (see handleExport).
	// Without this the deferred pool.Close waits on that connection until
	// Compose's SIGKILL.
	endLifetime()
	return err
}

const (
	// databaseTimeZoneTimeout bounds the startup comparison with the database.
	databaseTimeZoneTimeout = 5 * time.Second
	// SQLSTATE undefined_function: puls_time_zone() does not exist yet.
	sqlstateUndefinedFunction = "42883"
)

// verifyTimeZone refuses to start when PULS_TIME_ZONE (loc) names another
// zone than the database's puls_time_zone(): the API's day ranges and
// metric_daily's day column would then disagree on every daily answer,
// without an error anywhere. dbZone runs `SELECT puls_time_zone()`. A
// database without the function (from before 009/013) cannot be checked and
// is logged, as is a failed read: only a confirmed mismatch stops startup.
func verifyTimeZone(ctx context.Context, dbZone func(context.Context) (string, error), loc *time.Location, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeZoneTimeout)
	defer cancel()
	name, err := dbZone(ctx)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == sqlstateUndefinedFunction:
		logger.Warn("the database has no puls_time_zone(), so PULS_TIME_ZONE cannot be checked against it; "+
			"run the migrate service (docker compose up -d) to install it",
			"time_zone", loc.String())
		return nil
	case err != nil:
		logger.Warn("could not read the database's time zone; PULS_TIME_ZONE is unchecked",
			"time_zone", loc.String(), "err", err.Error())
		return nil
	case !sameTimeZone(name, loc.String()):
		return fmt.Errorf("PULS_TIME_ZONE is %q but the database's puls_time_zone() is %q: daily answers would be cut "+
			"on different days than metric_daily and the dashboards. Give every service the same PULS_TIME_ZONE in "+
			".env and run `docker compose up -d` (the migrate service stores it on the database)", loc.String(), name)
	}
	logger.Info("time zone matches the database", "time_zone", loc.String(), "database_time_zone", name)
	return nil
}

// refusePlaceholder fails startup on a token still set to the .env.example
// placeholder: an install made by hand from the example, without
// scripts/bootstrap.sh, would otherwise accept a credential anyone can guess.
func refusePlaceholder(name, value string) error {
	if strings.EqualFold(strings.TrimSpace(value), placeholderSecret) {
		return fmt.Errorf("%s is still the placeholder %q from .env.example: set a random value "+
			"(scripts/bootstrap.sh generates one, or use `openssl rand -hex 32`)", name, placeholderSecret)
	}
	return nil
}

func connectWithRetry(ctx context.Context, url string, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return pool, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			pool.Close()
			return nil, errors.New("database unavailable")
		}
		logger.Warn("waiting for database")
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		}
	}
}

// route is one entry of the router. path is the same route as an OpenAPI
// path, which is what lets TestOpenAPIDescribesTheRouter compare the two:
// an endpoint added here but not to the document in openapi.json (or the other
// way round) fails the build rather than shipping an OpenAPI document that
// lies to a generated client.
type route struct {
	// The http.ServeMux pattern, e.g. "GET /v1/samples".
	pattern string
	// The OpenAPI path, e.g. "/v1/samples".
	path    string
	handler http.HandlerFunc
	// Whether the bearer token is required; the discovery endpoints are open
	// and carry "security": [] in the document.
	auth bool
}

func (s *Server) apiRoutes() []route {
	return []route{
		{"GET /{$}", "/", s.handleIndex, false},
		{"GET /docs", "/docs", s.handleDocs, false},
		{"GET /openapi.json", "/openapi.json", s.handleOpenAPI, false},
		{"GET /healthz", "/healthz", s.handleHealthz, false},
		{"GET /v1/users", "/v1/users", s.handleUsers, true},
		{"GET /v1/profile", "/v1/profile", s.handleProfile, true},
		{"GET /v1/catalog/types", "/v1/catalog/types", s.handleCatalogTypes, true},
		{"GET /v1/metrics/latest", "/v1/metrics/latest", s.handleLatestMetrics, true},
		{"GET /v1/metrics/daily", "/v1/metrics/daily", s.handleDailyMetrics, true},
		{"GET /v1/activity/summary", "/v1/activity/summary", s.handleActivitySummary, true},
		{"GET /v1/workouts", "/v1/workouts", s.handleWorkouts, true},
		{"GET /v1/workouts/{uuid}", "/v1/workouts/{uuid}", s.handleWorkout, true},
		{"GET /v1/workouts/{uuid}/series", "/v1/workouts/{uuid}/series", s.handleWorkoutSeries, true},
		{"GET /v1/sleep/daily", "/v1/sleep/daily", s.handleSleepDaily, true},
		{"GET /v1/samples", "/v1/samples", s.handleSamples, true},
		{"GET /v1/state-of-mind", "/v1/state-of-mind", s.handleStateOfMind, true},
		{"GET /v1/summary", "/v1/summary", s.handleSummary, true},
		{"GET /v1/export", "/v1/export", s.handleExport, true},
	}
}

// limiter returns the auth-failure limiter, creating it on first use.
func (s *Server) limiter() *failureLimiter {
	s.limiterOnce.Do(func() {
		if s.failures == nil {
			s.failures = newFailureLimiter()
		}
	})
	return s.failures
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.apiRoutes() {
		handler := rt.handler
		// The export streams for as long as the download takes (bounded on
		// its own terms, see export.go); every other handler is bounded so
		// one slow query cannot hold a pool connection open indefinitely.
		if rt.path != "/v1/export" {
			handler = s.untilShutdown(withTimeout(handlerTimeout, handler))
		}
		if rt.auth {
			// Token first, then the user the request is about: a caller that
			// cannot authenticate never learns whether ?user= is valid.
			handler = s.auth(s.scopeUser(handler))
		}
		mux.HandleFunc(rt.pattern, handler)
	}
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		// Refusal happens BEFORE the token comparison, exactly as it does in
		// ingest: charging a failure but still answering 401 would let an
		// attacker keep guessing at full speed and read the status code.
		ip := clientIP(r, s.trustProxyHeaders)
		if ok, wait := s.limiter().allow(ip, time.Now()); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
			s.log.Warn("auth throttled", "ip", ip, "path", r.URL.Path, "retry_after_s", int(wait.Seconds()))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed authentications"})
			return
		}

		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var authority *apiAuthority
		err := errAPICredential
		if ok && s.authorize != nil {
			authority, err = s.authorize(r.Context(), got)
		}
		if errors.Is(err, errAPIAuthority) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization unavailable"})
			return
		}
		if err != nil || authority == nil || !authority.Principal.valid() {
			// A successful request never costs a token: a client polling this
			// API legitimately must never be throttled.
			s.limiter().recordFailure(ip, time.Now())
			// Logged so a token brute-force leaves a trace. The token itself is
			// never logged, present or absent.
			s.log.Warn("auth failed", "ip", ip, "path", r.URL.Path, "had_bearer", ok)
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		ctx := context.WithValue(r.Context(), apiAuthorityKey{}, authority)
		ctx = withRequestUser(ctx, authority.Principal.MemberID)
		authorize := s.authorize
		ctx = context.WithValue(ctx, readRecheckKey{}, func(ctx context.Context) error {
			current, err := authorize(ctx, got)
			if err != nil {
				return err
			}
			if current == nil || current.TokenHash != authority.TokenHash || !current.Principal.equals(authority.Principal) {
				return errAPICredential
			}
			return nil
		})
		ctx, closeScope, err := s.store.ReadScope(ctx, authority)
		if err != nil {
			if errors.Is(err, errAPICredential) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			} else {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization unavailable"})
			}
			return
		}
		defer closeScope()
		next(w, r.WithContext(ctx))
	}
}

// withTimeout bounds a handler's database work. Without it a slow query holds
// a pooled connection until the client goes away, and the pool defaults to
// max(4, NumCPU) — so a handful of them starve every other endpoint.
//
// /v1/export is deliberately exempt: it streams for as long as the download
// takes, and is bounded instead by its own concurrency slots, range cap,
// per-write deadline and absolute ceiling (export.go). A handler that runs
// out of time answers 504 through writeStoreError.
func withTimeout(d time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}

// untilShutdown cancels a handler's context when the server's lifetime ends
// (shutdown's grace has run out), so its query returns its pooled
// connection and pool.Close does not wait for Compose's SIGKILL. The export
// does this itself, because it must tell that apart from a client hang-up.
func (s *Server) untilShutdown(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(s.lifetimeContext(), cancel)
		defer stop()
		next(w, r.WithContext(ctx))
	}
}

// lifetimeContext is the server's lifetime, or one that never ends for a
// Server built as a struct literal.
func (s *Server) lifetimeContext() context.Context {
	if s.lifetime == nil {
		return context.Background()
	}
	return s.lifetime
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Cached: this endpoint is unauthenticated, so request rate must not drive
	// pool acquisitions. See health.go.
	if !s.health.status(s.store, time.Now()) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "db": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "db": true})
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	if !requireAPIPermission(w, r, "samples:read", []string{"puls.v1.profile"}, nil, nil) {
		return
	}
	profile, err := s.store.Profile(r.Context(), s.requestUser(r))
	if err != nil {
		s.writeStoreError(w, err, "profile")
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "profile not found"})
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// handleCatalogTypes serves GET /v1/catalog/types from a per-user cache.
// The query behind it counts every row the user has across the hypertables
// (user_id is neither indexed nor the compression segment key, so it is a
// decompressing scan), and it is the MCP's documented first call: so only a
// user's very first request waits for it. After catalogTTL the cached answer
// is still served — up to catalogMaxStale — while one background refresh
// replaces it (see refreshCatalogTypes).
func (s *Server) handleCatalogTypes(w http.ResponseWriter, r *http.Request) {
	if apiPermission(r.Context(), "metrics:read", nil, nil, nil) != nil &&
		!requireAPIPermission(w, r, "samples:read", nil, nil, nil) {
		return
	}
	user := s.requestUser(r)
	authority := r.Context().Value(apiAuthorityKey{}).(*apiAuthority)
	key := catalogKey(user, authority.TokenHash)
	if types, stale, ok := s.cachedCatalogTypes(key, time.Now()); ok {
		if stale {
			s.refreshCatalogTypes(user, key, authority)
		}
		writeJSON(w, http.StatusOK, map[string][]CatalogType{"types": types})
		return
	}

	types, err := s.store.CatalogTypes(r.Context(), user)
	if err != nil {
		s.writeStoreError(w, err, "catalog types")
		return
	}
	types = cloneCatalogTypes(types)
	s.storeCatalogTypes(key, types)
	writeJSON(w, http.StatusOK, map[string][]CatalogType{"types": types})
}

func (s *Server) handleLatestMetrics(w http.ResponseWriter, r *http.Request) {
	types, err := parseTypesParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !requireAPIPermission(w, r, "metrics:read", types, nil, nil) {
		return
	}
	metrics, err := s.store.LatestMetrics(r.Context(), s.requestUser(r), types)
	if err != nil {
		s.writeStoreError(w, err, "latest metrics")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]LatestMetric{"metrics": metrics})
}

func (s *Server) handleDailyMetrics(w http.ResponseWriter, r *http.Request) {
	filters, err := dailyFiltersFromRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !requireAPIPermission(w, r, "metrics:read", filters.Types, &filters.Start, &filters.End) {
		return
	}
	metrics, err := s.store.DailyMetrics(r.Context(), s.requestUser(r), filters)
	if err != nil {
		s.writeStoreError(w, err, "daily metrics")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"metrics":    metrics,
		"nextOffset": filters.Offset + dailyPoints(metrics),
	})
}

// dailyPoints is the number of day rows a daily-metrics answer carries — the
// unit /v1/metrics/daily pages in.
func dailyPoints(metrics []DailyMetric) int {
	n := 0
	for _, m := range metrics {
		n += m.points()
	}
	return n
}

func (s *Server) handleActivitySummary(w http.ResponseWriter, r *http.Request) {
	start, end, err := parseRange(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !requireAPIPermission(w, r, "metrics:read", []string{"HKActivitySummaryTypeIdentifier"}, &start, &end) {
		return
	}
	days, err := s.store.ActivitySummary(r.Context(), s.requestUser(r), start, end)
	if err != nil {
		s.writeStoreError(w, err, "activity summary")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]ActivityDay{"days": days})
}

func (s *Server) handleWorkouts(w http.ResponseWriter, r *http.Request) {
	filters, err := workoutFiltersFromRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !requireAPIPermission(w, r, "metrics:read", []string{"HKWorkoutTypeIdentifier"}, filters.Start, filters.End) {
		return
	}
	workouts, err := s.store.Workouts(r.Context(), s.requestUser(r), filters)
	if err != nil {
		s.writeStoreError(w, err, "workouts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workouts":   workouts,
		"nextOffset": filters.Offset + len(workouts),
	})
}

func (s *Server) handleWorkout(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if !isUUID(uuid) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid workout uuid"})
		return
	}
	if !requireAPIPermission(w, r, "metrics:read", []string{"HKWorkoutTypeIdentifier"}, nil, nil) {
		return
	}
	workout, err := s.store.Workout(r.Context(), s.requestUser(r), uuid)
	if err != nil {
		s.writeStoreError(w, err, "workout", "uuid", uuid)
		return
	}
	if workout == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workout not found"})
		return
	}
	writeJSON(w, http.StatusOK, workout)
}

func (s *Server) handleSleepDaily(w http.ResponseWriter, r *http.Request) {
	start, end, err := parseRange(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !requireAPIPermission(w, r, "metrics:read", []string{sleepTypeIdentifier}, &start, &end) {
		return
	}
	nights, err := s.store.SleepDaily(r.Context(), s.requestUser(r), start, end)
	if err != nil {
		s.writeStoreError(w, err, "sleep")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]SleepNight{"nights": nights})
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	filters, err := sampleFiltersFromRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !requireAPIPermission(w, r, "samples:read", []string{filters.Type}, &filters.Start, &filters.End) {
		return
	}
	page, err := s.store.Samples(r.Context(), s.requestUser(r), filters)
	if err != nil {
		s.writeStoreError(w, err, "samples")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleWorkoutSeries(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if !isUUID(uuid) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid workout uuid"})
		return
	}
	types, maxPoints, err := workoutSeriesRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !requireAPIPermission(w, r, "samples:read", []string{"HKWorkoutTypeIdentifier", "puls.v1.series"}, nil, nil) {
		return
	}
	series, err := s.store.WorkoutSeries(r.Context(), s.requestUser(r), uuid, types, maxPoints)
	if err != nil {
		s.writeStoreError(w, err, "workout series", "uuid", uuid)
		return
	}
	if series == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workout not found"})
		return
	}
	writeJSON(w, http.StatusOK, series)
}

func (s *Server) handleStateOfMind(w http.ResponseWriter, r *http.Request) {
	start, end, err := parseRange(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !requireAPIPermission(w, r, "samples:read", []string{"HKDataTypeIdentifierStateOfMind"}, &start, &end) {
		return
	}
	entries, err := s.store.StateOfMind(r.Context(), s.requestUser(r), start, end)
	if err != nil {
		s.writeStoreError(w, err, "state of mind")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]StateOfMindEntry{"entries": entries})
}

// writeStoreError answers a failed store call: a requestError is the
// caller's fault and comes back as a 400 with its message; a query that ran
// past its deadline (handlerTimeout; pgx wraps context.DeadlineExceeded
// whether it hit the pool, the query or the scan) is a 504 the caller can
// act on; anything else is logged and answered with a generic 500. attrs
// are extra log fields.
func (s *Server) writeStoreError(w http.ResponseWriter, err error, what string, attrs ...any) {
	if errors.Is(err, errAPICredential) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if errors.Is(err, errAPIAuthority) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization unavailable"})
		return
	}
	if errors.Is(err, errAPIScope) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "outside personal permission"})
		return
	}
	var reqErr *requestError
	if errors.As(err, &reqErr) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": reqErr.Error()})
		return
	}
	attrs = append(attrs, "err", err.Error())
	if errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn(what+" query timed out", attrs...)
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "query took too long; narrow the range"})
		return
	}
	s.log.Error(what+" query failed", attrs...)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": what + " failed"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func parseTypesParam(r *http.Request) ([]string, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("types"))
	if raw == "" {
		return nil, errors.New("missing types")
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxTypesPerRequest {
		return nil, fmt.Errorf("types names %d identifiers; at most %d per request", len(parts), maxTypesPerRequest)
	}
	types := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("types must not contain empty values")
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		types = append(types, part)
	}
	return types, nil
}

func msParam(s, name string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("missing %s", name)
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s: must be epoch milliseconds", name)
	}
	// Any int64 parses, but the day-range formatting downstream casts to a
	// Postgres `date`; a value beyond year 9999 blew up there as a 500
	// instead of a 400.
	if ms < minEpochMS || ms > maxEpochMS {
		return time.Time{}, fmt.Errorf("invalid %s: epoch milliseconds out of range", name)
	}
	return time.UnixMilli(ms).UTC(), nil
}

func parseRange(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	start, err := msParam(q.Get("start"), "start")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := msParam(q.Get("end"), "end")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, errors.New("end must be after start")
	}
	return start, end, nil
}

func optionalMSParam(raw, name string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := msParam(raw, name)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func parseLimitOffset(r *http.Request) (limit, offset int, err error) {
	return parseLimitOffsetBounds(r, defaultLimit, maxLimit)
}

// parseLimitOffsetBounds reads limit (default def, clamped to max) and
// offset (default 0) from the query.
func parseLimitOffsetBounds(r *http.Request, def, max int) (limit, offset int, err error) {
	q := r.URL.Query()
	limit = def
	offset = 0

	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, errors.New("invalid limit: must be an integer")
		}
	}
	if limit < 1 {
		return 0, 0, errors.New("limit must be at least 1")
	}
	if limit > max {
		limit = max
	}

	if raw := q.Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, errors.New("invalid offset: must be an integer")
		}
	}
	if offset < 0 {
		return 0, 0, errors.New("offset must be at least 0")
	}

	return limit, offset, nil
}

// dailyTypesAndRange reads the types and range of a daily-metrics request.
// Limit and Offset are left zero, which the store reads as the whole range
// — what the export wants.
func dailyTypesAndRange(r *http.Request) (DailyFilters, error) {
	var f DailyFilters
	types, err := parseTypesParam(r)
	if err != nil {
		return f, err
	}
	start, end, err := parseRange(r)
	if err != nil {
		return f, err
	}
	f.Types, f.Start, f.End = types, start, end
	return f, nil
}

// dailyFiltersFromRequest reads GET /v1/metrics/daily: dailyTypesAndRange
// plus the endpoint's paging (limit defaults to defaultDailyLimit, caps at
// maxDailyLimit; both count days across the requested types).
func dailyFiltersFromRequest(r *http.Request) (DailyFilters, error) {
	f, err := dailyTypesAndRange(r)
	if err != nil {
		return f, err
	}
	limit, offset, err := parseLimitOffsetBounds(r, defaultDailyLimit, maxDailyLimit)
	if err != nil {
		return f, err
	}
	f.Limit, f.Offset = limit, offset
	return f, nil
}

func workoutFiltersFromRequest(r *http.Request) (WorkoutFilters, error) {
	var filters WorkoutFilters

	limit, offset, err := parseLimitOffset(r)
	if err != nil {
		return filters, err
	}
	q := r.URL.Query()
	start, err := optionalMSParam(q.Get("start"), "start")
	if err != nil {
		return filters, err
	}
	end, err := optionalMSParam(q.Get("end"), "end")
	if err != nil {
		return filters, err
	}
	if start != nil && end != nil && !end.After(*start) {
		return filters, errors.New("end must be after start")
	}

	filters.Start = start
	filters.End = end
	filters.ActivityType = q.Get("activityType")
	filters.Limit = limit
	filters.Offset = offset
	return filters, nil
}

// sampleTypeAndRange reads what GET /v1/samples and the samples export have
// in common: exactly one type, and a required [start, end) range of at most
// maxSampleRange. Limit and Offset are left zero, which the store reads as
// "every matching row".
func sampleTypeAndRange(r *http.Request) (SampleFilters, error) {
	var f SampleFilters
	q := r.URL.Query()

	f.Type = strings.TrimSpace(q.Get("type"))
	if f.Type == "" {
		return f, errors.New("missing type")
	}
	if strings.Contains(f.Type, ",") {
		return f, errors.New("type must name exactly one HealthKit identifier")
	}
	start, end, err := parseRange(r)
	if err != nil {
		return f, err
	}
	if end.Sub(start) > maxSampleRange {
		return f, fmt.Errorf("range must not exceed %d days", int(maxSampleRange.Hours()/24))
	}
	f.Start, f.End = start, end
	return f, nil
}

// sampleFiltersFromRequest reads GET /v1/samples: sampleTypeAndRange plus the
// endpoint's paging.
func sampleFiltersFromRequest(r *http.Request) (SampleFilters, error) {
	f, err := sampleTypeAndRange(r)
	if err != nil {
		return f, err
	}
	limit, offset, err := parseLimitOffsetBounds(r, defaultSampleLimit, maxSampleLimit)
	if err != nil {
		return f, err
	}
	f.Limit, f.Offset = limit, offset
	return f, nil
}

// workoutSeriesRequest reads GET /v1/workouts/{uuid}/series: an optional
// comma-separated types filter and maxPoints (default defaultSeriesPoints,
// clamped to maxSeriesPoints).
func workoutSeriesRequest(r *http.Request) ([]string, int, error) {
	var types []string
	if strings.TrimSpace(r.URL.Query().Get("types")) != "" {
		var err error
		if types, err = parseTypesParam(r); err != nil {
			return nil, 0, err
		}
	}
	maxPoints := defaultSeriesPoints
	if raw := r.URL.Query().Get("maxPoints"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, 0, errors.New("invalid maxPoints: must be an integer")
		}
		if n < 1 {
			return nil, 0, errors.New("maxPoints must be at least 1")
		}
		maxPoints = n
	}
	if maxPoints > maxSeriesPoints {
		maxPoints = maxSeriesPoints
	}
	return types, maxPoints, nil
}

func isHexN(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < n; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func isUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	return isHexN(s[0:8], 8) && isHexN(s[9:13], 4) && isHexN(s[14:18], 4) &&
		isHexN(s[19:23], 4) && isHexN(s[24:36], 12)
}

// catalogEntry is one user's cached /v1/catalog/types answer: fresh until
// expires, then served stale (while a refresh runs) until
// expires+catalogMaxStale, after which it is gone.
type catalogEntry struct {
	types   []CatalogType
	expires time.Time
}

// cachedCatalogTypes returns user's cached answer, and whether it is past
// catalogTTL and due a refresh. An entry past catalogMaxStale is no answer:
// a refresh that keeps failing must not serve a months-old catalog forever.
func (s *Server) cachedCatalogTypes(user string, now time.Time) (types []CatalogType, stale, ok bool) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()

	entry, found := s.catalog[user]
	if !found || !now.Before(entry.expires.Add(catalogMaxStale)) {
		return nil, false, false
	}
	return cloneCatalogTypes(entry.types), !now.Before(entry.expires), true
}

// refreshCatalogTypes recomputes user's catalog in the background, unless a
// refresh (for anyone) is already running: one at a time, because each holds
// a pooled connection for the length of a scan, and the pool is small. A
// stale request that finds one running is simply served stale; a later one
// refreshes. The refresh runs on its own context — never the request's,
// which ends as soon as the stale answer is written — bounded by
// catalogRefreshTimeout and the server's lifetime. A failure keeps the stale
// answer and is logged.
func (s *Server) refreshCatalogTypes(user, key string, authority *apiAuthority) {
	s.catalogMu.Lock()
	if s.catalogRefreshing {
		s.catalogMu.Unlock()
		return
	}
	s.catalogRefreshing = true
	s.catalogRefreshes.Add(1)
	s.catalogMu.Unlock()

	go func() {
		defer s.catalogRefreshes.Done()
		ctx, cancel := context.WithTimeout(s.lifetimeContext(), catalogRefreshTimeout)
		defer cancel()
		ctx, closeScope, err := s.store.ReadScope(ctx, authority)
		var types []CatalogType
		if err == nil {
			defer closeScope()
			types, err = s.store.CatalogTypes(ctx, user)
		}
		if err == nil {
			s.storeCatalogTypes(key, cloneCatalogTypes(types))
		} else {
			s.log.Warn("catalog types refresh failed; serving the cached answer", "err", err.Error())
		}
		s.catalogMu.Lock()
		s.catalogRefreshing = false
		s.catalogMu.Unlock()
	}()
}

func catalogKey(user string, hash [32]byte) string {
	return fmt.Sprintf("%s:%x", strings.ToLower(user), hash)
}

// Bound credential entries, evicting expired entries before the oldest live one.
func (s *Server) storeCatalogTypes(user string, types []CatalogType) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()

	if s.catalog == nil {
		s.catalog = make(map[string]catalogEntry, 1)
	}
	now := time.Now()
	if _, ok := s.catalog[user]; !ok && len(s.catalog) >= catalogCacheMaxUsers {
		for key, entry := range s.catalog {
			if !now.Before(entry.expires.Add(catalogMaxStale)) {
				delete(s.catalog, key)
			}
		}
		if len(s.catalog) >= catalogCacheMaxUsers {
			var (
				oldest   string
				soonest  time.Time
				firstKey = true
			)
			for key, entry := range s.catalog {
				if firstKey || entry.expires.Before(soonest) {
					oldest, soonest, firstKey = key, entry.expires, false
				}
			}
			delete(s.catalog, oldest)
		}
	}
	s.catalog[user] = catalogEntry{types: cloneCatalogTypes(types), expires: now.Add(catalogTTL)}
}

// catalogCacheSize is the number of users with a cached catalog (tests).
func (s *Server) catalogCacheSize() int {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	return len(s.catalog)
}

func cloneCatalogTypes(types []CatalogType) []CatalogType {
	if len(types) == 0 {
		return []CatalogType{}
	}
	out := make([]CatalogType, len(types))
	copy(out, types)
	return out
}

// Per-request user scoping.
//
// Every authenticated route answers for exactly one user. Which one is
// settled here, once, before the handler runs: the ?user= query parameter
// when the request carries one, else PULS_USER_ID — so a deployment that
// never sets PULS_MULTI_USER behaves exactly as it always has. The parameter
// is a selector, not a credential: the bearer token is the same for everyone,
// which is why the gate exists. With it off, naming any other user is a 403
// rather than a quiet answer for the default user — silently substituting one
// person's data for another's is the one outcome worse than an error.
//
// Neither refusal charges the auth-failure limiter: the caller holds a valid
// token and mis-addressed a request, which is a configuration mistake, not a
// guess at the secret. There is no existence check either — an unknown id
// reads as a user with no data — because /v1/users is the discovery surface
// and a lookup per request would cost a round trip on every call.

// requestUserKey is the request-context key scopeUser stashes the user under.
type requestUserKey struct{}

func withRequestUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, requestUserKey{}, user)
}

// scopeUser settles the user a request is answered for (see the package
// comment above) and hands it to next through the request context.
func (s *Server) scopeUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := s.requestUser(r)
		if user == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("user")); raw != "" {
			if !isUUID(raw) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid user: must be a UUID"})
				return
			}
			if !sameUser(raw, user) {
				s.log.Warn("refused a request for another member", "route", r.Pattern)
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "member is outside this credential"})
				return
			}
			user = strings.ToLower(raw)
		}
		next(w, r.WithContext(withRequestUser(r.Context(), user)))
	}
}

// requestUser never substitutes a configured or seeded member.
func (s *Server) requestUser(r *http.Request) string {
	user, _ := r.Context().Value(requestUserKey{}).(string)
	return user
}

// location is the zone prose responses render instants in: PULS_TIME_ZONE,
// or UTC for a Server built as a struct literal.
func (s *Server) location() *time.Location {
	if s.loc == nil {
		return time.UTC
	}
	return s.loc
}

// sameUser compares two UUIDs the way Postgres does: case does not matter.
func sameUser(a, b string) bool {
	return strings.EqualFold(a, b)
}
