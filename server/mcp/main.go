// Command mcp is the PulsHealth MCP server: read-only access to one person's
// Apple Health data for AI assistants, over the product API only.
//
// It runs in two modes. By default it speaks the Model Context Protocol on
// stdin/stdout for local clients (Claude Desktop, Claude Code, Cursor, ...).
// With --http it serves the streamable HTTP transport at /mcp for remote
// clients, behind a bearer token. See README.md and docs/ai.md.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	// Embed the IANA zone database so PULS_TIME_ZONE resolves in the
	// distroless image, which has no /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultAPIURL   = "http://127.0.0.1:8081"
	shutdownTimeout = 15 * time.Second
	healthzTimeout  = 2 * time.Second
	// Idle HTTP sessions (no request from the client) are dropped after this.
	sessionTimeout = 30 * time.Minute
	// What server/.env.example ships every secret as; see refusePlaceholder.
	placeholderSecret = "change-me"
)

// buildVersion is set by the Dockerfile via -ldflags "-X main.buildVersion=…".
// A `go install` build reports the module version from the build info; a
// plain `go build` reports "dev".
var buildVersion string

func version() string {
	if buildVersion != "" {
		return buildVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// config is everything read from the environment.
type config struct {
	apiURL   string
	apiToken string
	mcpToken string
	// loc is PULS_TIME_ZONE; nil (unset) means the product API's own zone,
	// learned from GET /v1/users (zone.go).
	loc *time.Location
	// Whether X-Forwarded-For may be believed for the auth-failure limiter
	// (TRUST_PROXY_HEADERS; the same switch and rule as ingest's and the
	// API's).
	trustProxyHeaders bool
	// userID pins this instance to one person: every request to the API
	// names it, and a tool call naming anyone else is refused before the
	// API is asked. Empty means unpinned — the API's own default user
	// unless a call names one. It applies to the static token (and stdio);
	// an OAuth access token always acts for its own sub.
	userID string
	// oauth is non-nil when PULS_AUTH_PUBLIC_KEYS_FILE, PULS_MCP_URL and
	// PULS_MCP_OAUTH_ISSUER are set (oauth.go).
	oauth *oauthConfig
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		apiURL:   strings.TrimSpace(getenv("PULS_API_URL")),
		apiToken: getenv("PULS_API_TOKEN"),
		mcpToken: getenv("PULS_MCP_TOKEN"),
	}
	if cfg.apiURL == "" {
		cfg.apiURL = defaultAPIURL
	}
	if cfg.apiToken == "" {
		return cfg, errors.New("PULS_API_TOKEN must be set (the product API's bearer token, from server/.env)")
	}
	userID, err := loadUserID(getenv("PULS_USER_ID"))
	if err != nil {
		return cfg, err
	}
	cfg.userID = userID
	if name := strings.TrimSpace(getenv("PULS_TIME_ZONE")); name != "" {
		loc, err := loadTimeZone(name)
		if err != nil {
			return cfg, err
		}
		cfg.loc = loc
	}
	trust, err := parseBoolEnv("TRUST_PROXY_HEADERS", getenv("TRUST_PROXY_HEADERS"), false)
	if err != nil {
		return cfg, err
	}
	cfg.trustProxyHeaders = trust
	oauth, err := loadOAuthConfig(getenv)
	if err != nil {
		return cfg, err
	}
	cfg.oauth = oauth
	return cfg, nil
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

// loadUserID validates the optional PULS_USER_ID (a UUID; empty means the
// instance is not pinned). It is normalised to lower case, the form the
// product API renders ids in, so a pinned id compares equal to one a tool
// call copies out of list_users.
func loadUserID(raw string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if id == "" {
		return "", nil
	}
	if !isUUID(id) {
		return "", fmt.Errorf("PULS_USER_ID %q is not a UUID (leave it empty to serve the product API's default user)", strings.TrimSpace(raw))
	}
	return id, nil
}

func main() {
	// `mcp healthcheck [addr]` is the Compose healthcheck (healthcheck.go):
	// the image is distroless, so there is no curl. addr defaults to the
	// image's --http address.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Args[2:], ":8082", os.Stderr))
	}

	httpAddr := flag.String("http", "", "serve the streamable HTTP transport on this address (e.g. 127.0.0.1:8082) instead of stdio; requires PULS_MCP_TOKEN or OAuth (PULS_AUTH_PUBLIC_KEYS_FILE, PULS_MCP_URL, PULS_MCP_OAUTH_ISSUER)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version())
		return
	}

	// stdout belongs to the stdio transport: every log line goes to stderr.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if err := run(*httpAddr, logger); err != nil {
		logger.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run(httpAddr string, logger *slog.Logger) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	if httpAddr != "" {
		if cfg.mcpToken == "" && cfg.oauth == nil {
			return errors.New("PULS_MCP_TOKEN (or OAuth: PULS_AUTH_PUBLIC_KEYS_FILE, PULS_MCP_URL, PULS_MCP_OAUTH_ISSUER) must be set to serve --http: it is the only thing between the network and the health data")
		}
		if err := refusePlaceholder("PULS_MCP_TOKEN", cfg.mcpToken); err != nil {
			return err
		}
	}
	api, err := NewAPIClient(cfg.apiURL, cfg.apiToken, nil)
	if err != nil {
		return err
	}
	api = api.ForUser(cfg.userID)
	svc := newService(api, cfg.loc)
	svc.log = logger
	server := svc.newServer(version())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.loc != nil {
		// Set by hand: warn (once the API answers) if it is not the API's.
		go svc.watchZone(ctx, 30*time.Second)
	}

	if httpAddr == "" {
		logger.Info("serving stdio", "api", api.BaseURL(), "time_zone", zoneLogValue(cfg.loc), "user", userLogValue(cfg.userID), "version", version())
		return server.Run(ctx, &mcp.StdioTransport{})
	}

	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           svc.httpHandler(server, authenticator{staticToken: cfg.mcpToken, oauth: cfg.oauth, now: time.Now}, cfg.trustProxyHeaders, logger),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the streamable transport holds SSE streams open.
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", httpAddr, "api", api.BaseURL(), "time_zone", zoneLogValue(cfg.loc), "user", userLogValue(cfg.userID),
			"trust_proxy_headers", cfg.trustProxyHeaders, "static_token", cfg.mcpToken != "", "oauth", oauthLogValue(cfg.oauth), "auth_failure_burst", authFailureBurst, "auth_failures_per_minute", authFailurePerMinute,
			"version", version())
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
	return httpSrv.Shutdown(shCtx)
}

// userLogValue renders the pin for the startup line.
func userLogValue(userID string) string {
	if userID == "" {
		return "api default"
	}
	return userID
}

// oauthLogValue renders the OAuth setting for the startup line (never the
// secret).
func oauthLogValue(c *oauthConfig) string {
	if c == nil {
		return "off"
	}
	return c.resource + " (issuer " + c.issuer + ")"
}

// zoneLogValue renders the zone for the startup line.
func zoneLogValue(loc *time.Location) string {
	if loc == nil {
		return "the product API's"
	}
	return loc.String()
}

// httpHandler serves /mcp behind the bearer token and /healthz without it,
// plus, with OAuth on, the protected-resource metadata (also without it).
func (s *service) httpHandler(server *mcp.Server, authn authenticator, trustProxyHeaders bool, logger *slog.Logger) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		SessionTimeout: sessionTimeout,
		Logger:         logger,
		// The SDK's DNS-rebinding guard rejects requests that reach a
		// loopback listener with a non-loopback Host header — which is
		// exactly what a TLS reverse proxy on the same host (Tailscale
		// Serve, Caddy, nginx) forwards to 127.0.0.1:8082. The bearer token
		// is the access control here, and a rebinding page cannot present
		// it, so the guard buys nothing and breaks the supported setup.
		DisableLocalhostProtection: true,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/mcp", bearerAuth(authn, newFailureLimiter(), trustProxyHeaders, logger, withSDKTokenInfo(mcpHandler)))
	if authn.oauth != nil {
		// RFC 9728: the bare document and the one for this resource's path
		// (what the 401's resource_metadata names) are the same.
		meta := authn.oauth.metadataHandler()
		mux.Handle(protectedResourcePath, meta)
		if authn.oauth.metadataPath != protectedResourcePath {
			mux.Handle(authn.oauth.metadataPath, meta)
		}
	}
	return mux
}

// bearerAuth admits only requests carrying the static token or (OAuth on) a
// valid access token, and records who on the request (tokenInfoCtxKey) for
// withSDKTokenInfo. Guesses sit behind the auth-failure limiter
// (ratelimit.go) exactly as on ingest and the API: a client that has spent
// its budget is refused with 429 *before* the token is compared, only
// failures are charged, and a success never is — a connected assistant makes
// many requests a conversation. Every failure is logged (never the token,
// present or absent) so a brute force leaves a trace.
//
// Only a bearer that could be a guess is gated or charged. Hosted connectors
// (claude.ai, Claude mobile) reach this server from their operator's shared
// egress addresses, so charging their normal traffic would spend the budget
// and lock out every person behind those addresses, valid tokens included:
//
//   - No bearer at all is the discovery request every OAuth client makes to
//     receive the 401 and its resource_metadata. It guesses nothing and the
//     challenge holds no secret, so it is never refused by the limiter nor
//     charged.
//   - A bearer whose Ed25519 signature an active public key verifies (verifySigned) cannot
//     be a guess. Its bounded verification runs before the limiter, so a valid access token is
//     admitted even from an exhausted address, and a signed but invalid one
//     (expired, which every connector presents each time its token lapses;
//     wrong audience; ...) is a 401 invalid_token, uncharged.
//   - Anything else — a wrong static token, garbage, a forged or unsigned
//     JWT — is gated and charged as before.
func bearerAuth(authn authenticator, limiter *failureLimiter, trustProxyHeaders bool, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ip := clientIP(r, trustProxyHeaders)
		admit := func(info *auth.TokenInfo) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenInfoCtxKey{}, info)))
		}
		refuse := func(presented bool, reason string) {
			logger.Warn("auth failed", "ip", ip, "path", r.URL.Path, "had_bearer", presented, "reason", reason)
			w.Header().Set("WWW-Authenticate", authn.challenge(presented))
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		}

		got, presented := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !presented {
			refuse(false, "no bearer")
			return
		}
		info, verificationError, signed := authn.verifySigned(got)
		if errors.Is(verificationError, errOAuthKeys) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization unavailable"})
			return
		}
		reason := ""
		if verificationError != nil {
			reason = verificationError.Error()
		}
		if signed {
			if info != nil {
				admit(info)
			} else {
				refuse(true, reason)
			}
			return
		}

		if ok, wait := limiter.allow(ip, time.Now()); !ok {
			seconds := int(wait.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			logger.Warn("auth throttled", "ip", ip, "path", r.URL.Path, "retry_after_s", seconds)
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed authentications"})
			return
		}
		if authn.staticMatches(got) {
			admit(&auth.TokenInfo{UserID: staticTokenIdentity})
			return
		}
		if reason == "" { // OAuth off: verifySigned had nothing to say
			reason = "wrong token"
		}
		limiter.recordFailure(ip, time.Now())
		refuse(true, reason)
	})
}

// handleHealthz reports whether this process is up and the product API (and
// through its own /healthz, its database) answers.
func (s *service) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthzTimeout)
	defer cancel()
	if err := s.api.Healthz(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "api": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "api": true})
}

// constantTimeEqual compares two secrets without leaking where they differ.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
