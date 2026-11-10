package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Every health request needs a current device credential bound to its member.
// A client header may confirm that member, never select another one.
// Database failure is 503; an unknown or revoked credential is 401.

func validateIngestAuthConfig(getenv func(string) string) error {
	for _, name := range []string{"PULS_TOKEN", "PULS_ALLOW_SHARED_TOKEN", "PULS_USER_ID"} {
		if getenv(name) != "" {
			return errors.New(name + " is retired; use member-bound device credentials")
		}
	}
	return nil
}

// tokenResolveTimeout bounds the device-token lookup. A lookup that takes
// longer than this is a database in trouble, and the request should fail as
// 503 quickly rather than hold the connection until the client gives up.
const tokenResolveTimeout = 2 * time.Second

// principal is who a request authenticated as. Handlers read the resolved
// user through readUserID; the token id is recorded on the batch.
type principal struct {
	sourceConnectionID string
	sourceActive       bool
	// userID is the member bound to the verified device credential.
	userID string
	// tokenID and tokenPrefix identify the verified device_tokens row.
	tokenID     int64
	tokenPrefix string
}

// tokenResolver is the one Store method the middleware needs, kept apart
// from ingester so handler tests can inject a map without touching fakeStore.
type tokenResolver interface {
	ResolveDeviceToken(ctx context.Context, hash []byte) (deviceToken, error)
}

// principalKey is the request-context key the middleware stashes the
// principal under.
type principalKey struct{}

func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalKey{}).(principal)
	return p, ok
}

// authRejection is a refusal the middleware writes itself.
type authRejection struct {
	status int
	body   string
	// charge says whether this failure draws from the client's auth-failure
	// budget. Only wrong-credential outcomes do.
	charge bool
}

var (
	rejectUnauthorized = &authRejection{status: http.StatusUnauthorized, body: "unauthorized", charge: true}
	rejectUnavailable  = &authRejection{status: http.StatusServiceUnavailable, body: "authentication unavailable"}
	rejectUserMismatch = &authRejection{status: http.StatusForbidden, body: "X-User-ID does not match the token's user"}
)

// auth gates every /v1 route on a bearer token, and gates the token check
// itself on the caller's auth-failure budget. A client that presents a valid
// token is never throttled, however many requests it makes — a backfill is
// thousands of them. A client that keeps getting it wrong runs out of budget
// and is refused before any comparison happens, which is what makes this a
// brute-force limit rather than a different error code.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, s.trustProxyHeaders)
		if ok, retryAfter := s.authFailures.allow(ip, time.Now()); !ok {
			seconds := int(retryAfter.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			s.log.Warn("auth attempts throttled",
				"ip", ip, "path", r.URL.Path, "retry_after_s", seconds)
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed authentications"})
			return
		}

		p, rej := s.authenticate(r.Context(), r.Header.Get("Authorization"))
		if rej == nil {
			var userID string
			userID, rej = s.resolveUser(p, r)
			if rej == nil {
				p.userID = userID
			}
			r = r.WithContext(withPrincipal(r.Context(), p))
		}
		if rej != nil {
			if rej.charge {
				s.authFailures.recordFailure(ip, time.Now())
			}
			writeJSON(w, rej.status, map[string]string{"error": rej.body})
			// A batch refused here never reached the parser, so the handler's
			// own rejection record does not exist; keep the durable log honest.
			if rej.status != http.StatusUnauthorized && rej.status != http.StatusServiceUnavailable &&
				r.Method == http.MethodPost && r.URL.Path == "/v1/batches" {
				s.recordBatchRejection(r, rej.status, "identity", 0)
			}
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/routes") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "device credential is limited to synchronization"})
			return
		}
		next(w, r)
	}
}

// authenticate resolves the current member-bound device credential.
func (s *Server) authenticate(ctx context.Context, header string) (principal, *authRejection) {
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || got == "" {
		return principal{}, rejectUnauthorized
	}
	if s.tokens == nil {
		return principal{}, rejectUnauthorized
	}
	lookupCtx, cancel := context.WithTimeout(ctx, tokenResolveTimeout)
	defer cancel()
	d, err := s.tokens.ResolveDeviceToken(lookupCtx, hashToken(got))
	switch {
	case errors.Is(err, errTokenNotFound):
		return principal{}, rejectUnauthorized
	case err != nil:
		s.log.Error("device token lookup failed", "err", err.Error())
		return principal{}, rejectUnavailable
	case !d.active() || d.ID <= 0 || !isUUID(d.UserID):
		s.log.Warn("revoked device token presented", "token_id", d.ID, "token_prefix", d.TokenPrefix)
		return principal{}, rejectUnauthorized
	}
	return principal{userID: d.UserID, tokenID: d.ID, tokenPrefix: d.TokenPrefix, sourceConnectionID: d.SourceConnectionID, sourceActive: d.SourceActive}, nil
}

// resolveUser accepts only the credential owner, with an optional matching header.
func (s *Server) resolveUser(p principal, r *http.Request) (string, *authRejection) {
	header := r.Header.Get("X-User-ID")
	if header != "" && !isUUID(header) {
		return "", &authRejection{status: http.StatusBadRequest, body: "X-User-ID is not a UUID"}
	}
	if header == "" || strings.EqualFold(header, p.userID) {
		return p.userID, nil
	}
	s.log.Warn("X-User-ID does not match the token's user",
		"token_id", p.tokenID, "token_prefix", p.tokenPrefix,
		"token_user", p.userID, "header_user", header, "path", r.URL.Path)
	return "", rejectUserMismatch
}

// readUserID is what every handler asks for the request's user: the one the
// auth middleware settled from the credential. Every /v1 route is wrapped
// in auth (see routes). A handler
// reached without it answers 500 and serves nothing: reading X-User-ID here
// instead, as this once did, would be unauthenticated tenant selection on
// any route someone forgot to wrap.
func readUserID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if p, ok := principalFrom(r.Context()); ok && p.userID != "" {
		return p.userID, true
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no authenticated user"})
	return "", false
}

type recordScope struct {
	userID             string
	sourceConnectionID string
}

func readRecordScope(w http.ResponseWriter, r *http.Request) (recordScope, bool) {
	userID, ok := readUserID(w, r)
	if !ok {
		return recordScope{}, false
	}
	p, _ := principalFrom(r.Context())
	if p.sourceConnectionID == "" || !p.sourceActive {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "source connection requires verified pairing"})
		return recordScope{}, false
	}
	return recordScope{userID: userID, sourceConnectionID: p.sourceConnectionID}, true
}

// logAuthMode is the startup line that says which credentials this server
// accepts, next to the rate-limit line.
func logAuthMode(log *slog.Logger) {
	log.Info("auth mode", "mode", "device tokens only")
}
