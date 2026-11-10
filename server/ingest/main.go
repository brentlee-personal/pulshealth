// Command ingest is the PulsHealth HealthKit ingestion server.
//
// It accepts gzipped NDJSON batches from the iOS app over HTTP and stores
// them in PostgreSQL/TimescaleDB. See server/README.md for the wire protocol.
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxWireBody    = 8 << 20  // compressed request body cap
	maxDecodedBody = 64 << 20 // decompressed NDJSON cap
	// Inside Compose's stop_grace_period (30 s), with room left for the
	// inserts still running at the end of it to be cancelled and the pool
	// closed before SIGKILL; see run.
	shutdownTimeout = 20 * time.Second
	maxUUIDRange    = 35 * 24 * time.Hour // /v1/uuids response size guard
	// Upper bound on one batch's database work; see handleBatch.
	insertDeadline = 5 * time.Minute
	// Batches decoded and inserted at once (PULS_MAX_INFLIGHT_BATCHES), how
	// long a request waits for a free slot, and the Retry-After it gets when
	// none frees up. See acquireBatchSlot.
	defaultMaxInflightBatches = 4
	batchSlotWait             = 2 * time.Second
	batchRetryAfterSeconds    = 5
)

// buildVersion identifies this build in GET /v1/capabilities. The Dockerfile
// sets it to the git commit via -ldflags "-X main.buildVersion=…"; a plain
// `go build` / `go run` leaves it empty and reports "dev".
var buildVersion string

func serverVersion() string {
	if buildVersion == "" {
		return "dev"
	}
	return buildVersion
}

// capabilityFeatures lists what this receiver implements beyond accepting a
// batch, so a client (or a person pointing the app at a server) can tell an
// endpoint-complete reference server from a minimal receiver that only takes
// uploads. Names are protocol vocabulary: keep them stable and append only.
var capabilityFeatures = []string{
	"batches", "stats", "digest", "uuids", "aggregates",
	"activitySummaries", "routes", "series", "profile",
}

// capabilities is the GET /v1/capabilities body (PROTO-6).
type capabilities struct {
	ProtocolVersions []int    `json:"protocolVersions"`
	Features         []string `json:"features"`
	Server           string   `json:"server"`
	Version          string   `json:"version"`
}

// protocolRejection is the fixed 400 body for a version this server does not
// speak (PROTO-1). The app never retries a 4xx, so this is the one response
// it can turn into a useful message; keep the shape stable.
type protocolRejection struct {
	Error             string `json:"error"`
	SupportedVersions []int  `json:"supportedVersions"`
}

func main() {
	// `ingest devices …` is the operator CLI for per-device tokens
	// (devices_cli.go); everything else is the server.
	if len(os.Args) > 1 && os.Args[1] == "devices" {
		os.Exit(runDevicesCLI(os.Args[2:], os.Stdout, os.Stderr))
	}
	// `ingest qr` draws the pairing payload on stdin as a terminal QR code
	// (qr.go). No database, no environment: scripts/bootstrap.sh falls back
	// to it when the host has no qrencode.
	if len(os.Args) > 1 && os.Args[1] == "qr" {
		os.Exit(runQRCLI(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}

	// `ingest healthcheck [addr]` is the Compose healthcheck
	// (healthcheck.go): the image is distroless, so there is no curl.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":8080"
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
	if _, err := familyProtocol(); err != nil {
		return fmt.Errorf("load family protocol: %w", err)
	}
	if err := validateIngestAuthConfig(os.Getenv); err != nil {
		return err
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL must be set")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// Off by default: X-Forwarded-For is set by whoever sends the request,
	// so believing it without a proxy in front would let one attacker look
	// like an unlimited number of clients to the auth-failure limiter.
	// Parsed by the one rule all three Go services share (envbool.go).
	trustProxyHeaders, err := parseBoolEnv("TRUST_PROXY_HEADERS", os.Getenv("TRUST_PROXY_HEADERS"), false)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := connectWithRetry(ctx, dbURL, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Inserts outlive their request (see handleBatch) but not the process:
	// this is cancelled once shutdown's grace has run out.
	lifetime, endLifetime := context.WithCancel(context.Background())
	defer endLifetime()

	store := NewStore(pool)
	srv := newServer(store, store, trustProxyHeaders, logger)
	srv.lifetime = lifetime
	srv.batchSlots = make(chan struct{}, maxInflightBatches(os.Getenv("PULS_MAX_INFLIGHT_BATCHES")))
	logAuthMode(logger)
	logger.Info("auth failure limiting",
		"burst", authFailureBurst, "per_minute", authFailurePerMinute,
		"trust_proxy_headers", trustProxyHeaders)
	{
		// Never fatal: the operator may be about to issue one. But an
		// install with no accepted credential at all should say so.
		n, err := store.ActiveDeviceTokenCount(ctx)
		switch {
		case err != nil:
			logger.Warn("could not count device tokens", "err", err.Error())
		case n == 0:
			logger.Warn("no active device tokens: nothing can authenticate",
				"hint", "make devices ARGS='issue --user <uuid> --name <label>'")
		}
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr, "max_inflight_batches", cap(srv.batchSlots))
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
	// A batch still inserting has had its grace. Cancel it (the transaction
	// rolls back and the phone resends; inserts are idempotent), or the
	// deferred pool.Close waits out its insertDeadline and Compose SIGKILLs.
	endLifetime()
	return err
}

// maxInflightBatches reads PULS_MAX_INFLIGHT_BATCHES: unset or not a number
// is the default, and anything below 1 is 1.
func maxInflightBatches(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	switch {
	case err != nil:
		return defaultMaxInflightBatches
	case n < 1:
		return 1
	}
	return n
}

// connectWithRetry waits for the database to come up (compose may start us
// alongside postgres) for up to ~60 seconds.
func connectWithRetry(ctx context.Context, url string, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
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
			return nil, fmt.Errorf("database unreachable: %w", err)
		}
		logger.Warn("waiting for database", "err", err.Error())
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		}
	}
}

// ingester abstracts Store so handlers can be unit-tested without a database.
type ingester interface {
	InsertBatch(ctx context.Context, b *Batch, bodyBytes int64) (IngestResult, error)
	BeginSourceObservation(ctx context.Context, userID string, tokenID int64, queryKey string) (string, error)
	RecordRejection(ctx context.Context, rejection IngestRejection) error
	Stats(ctx context.Context, scope recordScope) ([]TypeStats, error)
	Digest(ctx context.Context, scope recordScope, identifier string, from, to time.Time) ([]DigestWindow, error)
	UUIDs(ctx context.Context, scope recordScope, identifier string, from, to time.Time) ([]string, error)
	Routes(ctx context.Context, userID string, filters RouteFilters) ([]RouteSummary, error)
	Route(ctx context.Context, userID, uuid string) (*RouteDetail, error)
	RouteMetrics(ctx context.Context, userID, uuid string) ([]RouteMetricSeries, error)
	Ping(ctx context.Context) error
}

// Server holds handler dependencies.
type Server struct {
	store ingester
	// tokens resolves per-device bearer tokens (auth.go); nil means none.
	tokens tokenResolver
	log    *slog.Logger

	// Per-client-IP token bucket charged by failed authentications only
	// (see ratelimit.go).
	authFailures *failureLimiter
	// Whether X-Forwarded-For may be believed when identifying a client;
	// false unless TRUST_PROXY_HEADERS says a proxy owns that header.
	trustProxyHeaders bool

	// Last known database status for the unauthenticated /healthz, so its
	// request rate cannot drive pool acquisitions (see health.go).
	health healthCache

	// One slot per batch being decoded and inserted (see acquireBatchSlot).
	batchSlots chan struct{}
	// Parent of every insert's context: cancelled when shutdown's grace runs
	// out, so no insert outlives the process's last seconds.
	lifetime context.Context
}

func newServer(store ingester, tokens tokenResolver, trustProxyHeaders bool, log *slog.Logger) *Server {
	return &Server{
		store:             store,
		tokens:            tokens,
		log:               log,
		authFailures:      newFailureLimiter(),
		trustProxyHeaders: trustProxyHeaders,
		batchSlots:        make(chan struct{}, defaultMaxInflightBatches),
		lifetime:          context.Background(),
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/batches", s.auth(s.handleBatch))
	mux.HandleFunc("POST /v1/source-observations", s.auth(s.handleSourceObservation))
	mux.HandleFunc("GET /v1/stats", s.auth(s.handleStats))
	mux.HandleFunc("GET /v1/digest", s.auth(s.handleDigest))
	mux.HandleFunc("GET /v1/uuids", s.auth(s.handleUUIDs))
	mux.HandleFunc("GET /v1/routes", s.auth(s.handleRoutes))
	mux.HandleFunc("GET /v1/routes/{uuid}", s.auth(s.handleRoute))
	mux.HandleFunc("GET /v1/routes/{uuid}/metrics", s.auth(s.handleRouteMetrics))
	mux.HandleFunc("GET /v1/capabilities", s.auth(s.handleCapabilities))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// countingReader tracks compressed bytes read off the wire.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	headerBatchID := r.Header.Get("X-Batch-ID")

	// Protocol negotiation comes before the body is touched: a client
	// speaking a version this server does not understand costs no
	// decompression, parsing, or database work, and gets the one response it
	// can act on.
	headerVersion, err := parseProtocolHeader(r.Header.Get("X-Puls-Protocol"))
	if err != nil {
		s.rejectProtocol(w, r, 0)
		return
	}

	// Each batch may decode to maxDecodedBody, so only so many are handled at
	// once. The slot is taken after auth (a full house neither charges nor
	// skips the failure limiter) and before the body is read (a waiting
	// request holds no decoded bytes). Not recorded as a rejection: writing
	// to the database is the last thing to add while it is the bottleneck.
	if !s.acquireBatchSlot(r.Context()) {
		s.log.Warn("batch refused: too many in flight",
			"batch_id", diagnosticUUID(headerBatchID), "max_inflight_batches", cap(s.batchSlots))
		w.Header().Set("Retry-After", strconv.Itoa(batchRetryAfterSeconds))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "too many batches in flight; retry shortly"})
		return
	}
	defer func() { <-s.batchSlots }()

	cr := &countingReader{r: http.MaxBytesReader(w, r.Body, maxWireBody)}
	var body io.Reader = cr
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "gzip":
		gz, err := gzip.NewReader(cr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": batchRejectionMessage("gzip")})
			s.recordBatchRejection(r, http.StatusBadRequest, "gzip", cr.n)
			return
		}
		defer gz.Close()
		body = gz
	case "", "identity":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": batchRejectionMessage("encoding")})
		s.recordBatchRejection(r, http.StatusBadRequest, "encoding", cr.n)
		return
	}
	decodedBody := http.MaxBytesReader(w, io.NopCloser(body), maxDecodedBody)
	defer decodedBody.Close()

	batch, err := ParseFamilyBatch(decodedBody)
	parseDur := time.Since(start)
	if err != nil {
		var pve *ProtocolVersionError
		if errors.As(err, &pve) {
			s.rejectProtocol(w, r, cr.n)
			return
		}
		status := batchParseStatus(err)
		s.log.Warn("batch rejected",
			"batch_id", diagnosticUUID(headerBatchID), "stage", "parse", "status", status, "bytes", cr.n)
		writeJSON(w, status, map[string]string{"error": batchRejectionMessage("parse")})
		s.recordBatchRejection(r, status, "parse", cr.n)
		return
	}
	protocol, err := reconcileProtocolVersions(headerVersion, batch.Header.SchemaVersion)
	if err != nil {
		s.rejectProtocol(w, r, cr.n)
		return
	}
	if headerBatchID != "" && !strings.EqualFold(headerBatchID, batch.Header.BatchID) {
		s.log.Warn("X-Batch-ID header does not match body batchID",
			"header", diagnosticUUID(headerBatchID), "body", batch.Header.BatchID)
	}

	// The auth middleware settled the user before the body was read: the
	// device token's user. A client header never selects the member.
	// Every data row is stored under this id and the {"profile":…} line
	// upserts the matching users row. The token id is recorded on the batch.
	userID, ok := readUserID(w, r)
	if !ok {
		s.recordBatchRejection(r, http.StatusInternalServerError, "identity", cr.n)
		return
	}
	batch.Header.UserID = userID
	if p, ok := principalFrom(r.Context()); ok {
		batch.Header.DeviceTokenID = p.tokenID
	}

	// Wake correlation: the iOS wake that produced this upload (HTTP headers, like
	// X-User-ID). Both optional; reject only a malformed wake id.
	if wakeID := r.Header.Get("X-Wake-ID"); wakeID != "" {
		if !isUUID(wakeID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "X-Wake-ID is not a UUID"})
			s.recordBatchRejection(r, http.StatusBadRequest, "wake", cr.n)
			return
		}
		batch.Header.WakeID = wakeID
	}
	batch.Header.Trigger = r.Header.Get("X-Wake-Trigger")
	batch.ParseMs = parseDur.Milliseconds()

	// Bound the database work independently of the client connection. With
	// the bare request context the transaction lived exactly as long as the
	// TCP connection: a wedged Postgres (lock wait, long chunk decompression)
	// held this goroutine and its pool connection forever, and a phone that
	// gave up mid-insert rolled back a nearly-committed batch only to resend
	// it. The batch is idempotent end-to-end, so finishing the commit after a
	// disconnect is strictly better than redoing the work on retry. Only the
	// server's own shutdown (s.lifetime) cuts it short.
	insertCtx, cancelInsert := context.WithTimeout(s.lifetime, insertDeadline)
	defer cancelInsert()
	insertStart := time.Now()
	res, err := s.store.InsertBatch(insertCtx, batch, cr.n)
	insertDur := time.Since(insertStart)
	if err != nil {
		if errors.Is(err, errSourceSnapshot) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "mutable source snapshot requires a verified observation"})
			s.recordBatchRejection(r, http.StatusConflict, "conflict", cr.n)
			return
		}
		if errors.Is(err, errRecordConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "record identity conflicts with source history"})
			s.recordBatchRejection(r, http.StatusConflict, "conflict", cr.n)
			return
		}
		if errors.Is(err, errSourceBinding) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "source connection requires verified pairing"})
			s.recordBatchRejection(r, http.StatusConflict, "conflict", cr.n)
			return
		}
		if errors.Is(err, errBatchConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "batch key conflicts with a committed upload"})
			s.recordBatchRejection(r, http.StatusConflict, "conflict", cr.n)
			return
		}
		s.log.Error("batch insert failed",
			"batch_id", batch.Header.BatchID, "stage", "insert")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "insert failed"})
		s.recordBatchRejection(r, http.StatusInternalServerError, "insert", cr.n)
		return
	}

	s.log.Info("batch ingested",
		"batch_id", batch.Header.BatchID,
		"user_id", batch.Header.UserID,
		"token_id", batch.Header.DeviceTokenID,
		"wake_id", batch.Header.WakeID,
		"trigger", diagnosticTrigger(batch.Header.Trigger),
		"protocol", protocol,
		"samples", len(batch.Samples),
		"deletions", len(batch.Deletions),
		"routes", len(batch.Routes),
		"series", len(batch.Series),
		"aggregates", len(batch.Aggregates),
		"activity_summaries", len(batch.ActivitySummaries),
		"accepted", res.Accepted,
		"deleted", res.Deleted,
		"duplicates", res.Duplicates,
		"route_points", res.RoutePoints,
		"series_points", res.SeriesPoints,
		"aggregate_samples", res.AggregateSamples,
		"activity_summaries_upserted", res.ActivitySummaries,
		"duplicate_batch", res.DuplicateBatch,
		"bytes", cr.n,
		"parse_ms", parseDur.Milliseconds(),
		"insert_ms", insertDur.Milliseconds(),
		"retries", res.Retries,
	)
	if res.FamilyReceipt != nil {
		w.Header().Set("X-Family-Receipt", familyReceiptVersion)
	}
	writeJSON(w, http.StatusOK, res)
}

// acquireBatchSlot takes one of the batch slots, waiting up to batchSlotWait
// (less if the client goes away) for one to free up.
func (s *Server) acquireBatchSlot(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, batchSlotWait)
	defer cancel()
	select {
	case s.batchSlots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// rejectProtocol answers an unsupported or contradictory protocol version with
// the fixed negotiation body. Raw version text never enters diagnostics.
func (s *Server) rejectProtocol(w http.ResponseWriter, r *http.Request, bodyBytes int64) {
	s.log.Warn("batch rejected",
		"batch_id", diagnosticUUID(r.Header.Get("X-Batch-ID")), "stage", "protocol", "bytes", bodyBytes)
	writeJSON(w, http.StatusBadRequest, protocolRejection{
		Error:             "unsupported protocol version",
		SupportedVersions: supportedProtocolVersions,
	})
	s.recordBatchRejection(r, http.StatusBadRequest, "protocol", bodyBytes)
}

func batchRejectionMessage(stage string) string {
	switch stage {
	case "identity":
		return "invalid upload identity"
	case "gzip":
		return "invalid gzip body"
	case "encoding":
		return "unsupported Content-Encoding"
	case "parse":
		return "invalid batch"
	case "wake":
		return "invalid wake identifier"
	case "insert":
		return "insert failed"
	case "protocol":
		return "unsupported protocol version"
	case "conflict":
		return "batch key conflicts with a committed upload"
	default:
		return "batch rejected"
	}
}

func diagnosticUUID(value string) string {
	if isUUID(value) {
		return value
	}
	return ""
}

func diagnosticTrigger(value string) string {
	switch value {
	case "observer", "backgroundProcessing", "backgroundContinued", "foreground", "manual":
		return value
	}
	return ""
}

// recordBatchRejection is intentionally best-effort: observability must never
// replace or delay the original response. Use a short context independent of
// the request because truncated uploads commonly arrive with a canceled client.
func (s *Server) recordBatchRejection(r *http.Request, status int, stage string, bodyBytes int64) {
	p, ok := principalFrom(r.Context())
	if !ok || !isUUID(p.userID) {
		return
	}
	encoding := r.Header.Get("Content-Encoding")
	if encoding != "gzip" && encoding != "identity" {
		encoding = ""
	}
	rejection := IngestRejection{
		BatchID:         diagnosticUUID(r.Header.Get("X-Batch-ID")),
		UserID:          p.userID,
		WakeID:          diagnosticUUID(r.Header.Get("X-Wake-ID")),
		Trigger:         diagnosticTrigger(r.Header.Get("X-Wake-Trigger")),
		Status:          status,
		Stage:           stage,
		ErrorMessage:    batchRejectionMessage(stage),
		Bytes:           bodyBytes,
		ContentEncoding: encoding,
	}
	// Called only after the response has been written: with the database
	// down this blocks on pool acquire for the full 2 s, and a retrying client
	// must not pay that on every attempt exactly when the server is already
	// struggling. Observability never delays the original response.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.store.RecordRejection(ctx, rejection); err != nil {
		s.log.Error("could not persist batch rejection", "stage", stage)
	}
}

func batchParseStatus(err error) int {
	var limit *BatchLimitError
	if errors.As(err, &limit) {
		return http.StatusRequestEntityTooLarge
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge
	}
	var pe *ParseError
	if errors.As(err, &pe) {
		return http.StatusBadRequest
	}
	var pve *ProtocolVersionError
	if errors.As(err, &pve) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	scope, ok := readRecordScope(w, r)
	if !ok {
		return
	}
	stats, err := s.store.Stats(r.Context(), scope)
	if err != nil {
		s.log.Error("stats query failed", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "stats failed"})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// typeRangeParams parses the type/from/to query parameters shared by
// /v1/digest and /v1/uuids. from/to are epoch milliseconds; [from, to).
func typeRangeParams(r *http.Request) (string, time.Time, time.Time, error) {
	q := r.URL.Query()
	typ := q.Get("type")
	if typ == "" {
		return "", time.Time{}, time.Time{}, errors.New("missing type")
	}
	from, err := msParam(q.Get("from"), "from")
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	to, err := msParam(q.Get("to"), "to")
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	if !to.After(from) {
		return "", time.Time{}, time.Time{}, errors.New("to must be after from")
	}
	return typ, from, to, nil
}

func msParam(s, name string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("missing %s", name)
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s: must be epoch milliseconds", name)
	}
	return time.UnixMilli(ms).UTC(), nil
}

func (s *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	scope, ok := readRecordScope(w, r)
	if !ok {
		return
	}
	typ, from, to, err := typeRangeParams(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	windows, err := s.store.Digest(r.Context(), scope, typ, from, to)
	if err != nil {
		s.log.Error("digest query failed", "type", typ, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "digest failed"})
		return
	}
	writeJSON(w, http.StatusOK, windows)
}

func (s *Server) handleUUIDs(w http.ResponseWriter, r *http.Request) {
	scope, ok := readRecordScope(w, r)
	if !ok {
		return
	}
	typ, from, to, err := typeRangeParams(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if to.Sub(from) > maxUUIDRange {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "range exceeds 35 days"})
		return
	}
	uuids, err := s.store.UUIDs(r.Context(), scope, typ, from, to)
	if err != nil {
		s.log.Error("uuids query failed", "type", typ, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "uuids failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"uuids": uuids})
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	userID, ok := readUserID(w, r)
	if !ok {
		return
	}
	filters, err := routeFilters(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	routes, err := s.store.Routes(r.Context(), userID, filters)
	if err != nil {
		s.log.Error("routes query failed", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "routes failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string][]RouteSummary{"routes": routes})
}

func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	userID, ok := readUserID(w, r)
	if !ok {
		return
	}
	uuid := r.PathValue("uuid")
	if !isUUID(uuid) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid route uuid"})
		return
	}
	route, err := s.store.Route(r.Context(), userID, uuid)
	if err != nil {
		s.log.Error("route query failed", "uuid", uuid, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "route failed"})
		return
	}
	if route == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "route not found"})
		return
	}
	writeJSON(w, http.StatusOK, route)
}

func (s *Server) handleRouteMetrics(w http.ResponseWriter, r *http.Request) {
	userID, ok := readUserID(w, r)
	if !ok {
		return
	}
	uuid := r.PathValue("uuid")
	if !isUUID(uuid) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid route uuid"})
		return
	}
	metrics, err := s.store.RouteMetrics(r.Context(), userID, uuid)
	if err != nil {
		s.log.Error("route metrics query failed", "uuid", uuid, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "route metrics failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string][]RouteMetricSeries{"metrics": metrics})
}

func routeFilters(r *http.Request) (RouteFilters, error) {
	q := r.URL.Query()
	var filters RouteFilters

	if value := q.Get("start"); value != "" {
		t, err := routeTimeParam(value, "start")
		if err != nil {
			return filters, err
		}
		filters.Start = &t
	}
	if value := q.Get("end"); value != "" {
		t, err := routeTimeParam(value, "end")
		if err != nil {
			return filters, err
		}
		filters.End = &t
	}
	filters.ActivityType = q.Get("activityType")

	if value := q.Get("minDistanceM"); value != "" {
		n, err := floatParam(value, "minDistanceM")
		if err != nil {
			return filters, err
		}
		filters.MinDistanceM = &n
	}
	if value := q.Get("maxDistanceM"); value != "" {
		n, err := floatParam(value, "maxDistanceM")
		if err != nil {
			return filters, err
		}
		filters.MaxDistanceM = &n
	}
	if value := q.Get("limit"); value != "" {
		n, err := intParam(value, "limit")
		if err != nil {
			return filters, err
		}
		filters.Limit = n
	}
	if value := q.Get("offset"); value != "" {
		n, err := intParam(value, "offset")
		if err != nil {
			return filters, err
		}
		filters.Offset = n
	}
	return filters, nil
}

func routeTimeParam(value, name string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid %s: must be RFC3339 or YYYY-MM-DD", name)
}

func floatParam(value, name string) (float64, error) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: must be a number", name)
	}
	return n, nil
}

func intParam(value, name string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: must be an integer", name)
	}
	return n, nil
}

// handleCapabilities lets a client discover what this receiver speaks before
// it uploads anything. It sits behind bearer auth on purpose: the app's "Test
// connection" step uses it to validate URL and token together, and a 401
// here is the earliest possible signal of a mistyped token.
func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, capabilities{
		ProtocolVersions: supportedProtocolVersions,
		Features:         capabilityFeatures,
		Server:           "puls-ingest",
		Version:          serverVersion(),
	})
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
