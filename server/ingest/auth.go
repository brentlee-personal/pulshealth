package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Authentication (SRV-7 rate limiting, SRV-8 per-device tokens).
//
// Every /v1 route accepts two kinds of bearer value, checked in this order:
//
//  1. The shared PULS_TOKEN, compared in constant time and in memory. Nothing
//     is looked up, so a phone on the shared token never touches the database
//     to authenticate. Off when PULS_ALLOW_SHARED_TOKEN=false or the variable
//     is empty.
//  2. A per-device token: the presented value is SHA-256'd and looked up in
//     device_tokens (one indexed probe, which also stamps last_seen_at). An
//     active row yields a principal bound to that row's user.
//
// The failure limiter (ratelimit.go) is consulted before either comparison,
// and only things that mean "wrong credential" charge it: a missing bearer,
// a value that matches nothing, a revoked token. A database error during the
// lookup is a 503, not a 401, and is not charged — the app retries 5xx and
// treats 401 as terminal, so answering an outage with 401 would tell the
// user their token is wrong and stall syncing until they retyped it.
//
// A device principal also settles X-User-ID before any handler runs: the
// header may be absent (the token's user is used) or equal to the token's
// user; any other value is 403, uncharged, because it is a configuration
// mistake on a phone that holds a valid credential, not a guess. The shared
// token keeps exactly the old semantics — the header is unauthenticated
// tenant selection, which is the hole device tokens exist to close.

// tokenResolveTimeout bounds the device-token lookup. A lookup that takes
// longer than this is a database in trouble, and the request should fail as
// 503 quickly rather than hold the connection until the client gives up.
const tokenResolveTimeout = 2 * time.Second

// principal is who a request authenticated as. Handlers read the resolved
// user through readUserID; the token id is recorded on the batch.
type principal struct {
	// userID is the user every row of this request belongs to: the token's
	// user for a device token, else the X-User-ID header or the default.
	userID string
	// tokenID and tokenPrefix identify the device_tokens row; zero/empty
	// for the shared token.
	tokenID     int64
	tokenPrefix string
	shared      bool
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
		next(w, r)
	}
}

// authenticate turns the Authorization header into a principal: the shared
// token first (in memory), then a device-token lookup.
func (s *Server) authenticate(ctx context.Context, header string) (principal, *authRejection) {
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || got == "" {
		return principal{}, rejectUnauthorized
	}
	if s.allowShared && s.sharedToken != "" &&
		subtle.ConstantTimeCompare([]byte(got), []byte(s.sharedToken)) == 1 {
		return principal{shared: true}, nil
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
	case !d.active():
		s.log.Warn("revoked device token presented", "token_id", d.ID, "token_prefix", d.TokenPrefix)
		return principal{}, rejectUnauthorized
	}
	return principal{userID: d.UserID, tokenID: d.ID, tokenPrefix: d.TokenPrefix}, nil
}

// resolveUser settles which user the request acts as. The shared token keeps
// the old contract (header or default, malformed is 400); a device token is
// bound to one user, and a header naming a different one is refused.
func (s *Server) resolveUser(p principal, r *http.Request) (string, *authRejection) {
	header := r.Header.Get("X-User-ID")
	if header != "" && !isUUID(header) {
		return "", &authRejection{status: http.StatusBadRequest, body: "X-User-ID is not a UUID"}
	}
	if p.shared {
		if header == "" {
			return defaultUserID, nil
		}
		return header, nil
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
// auth middleware settled from the credential (and, for the shared token,
// X-User-ID). Every /v1 route is wrapped in auth (see routes). A handler
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

// logAuthMode is the startup line that says which credentials this server
// accepts, next to the rate-limit line.
func logAuthMode(log *slog.Logger, allowShared bool) {
	mode := "device tokens only"
	if allowShared {
		mode = "shared PULS_TOKEN + device tokens"
	}
	log.Info("auth mode", "mode", mode, "shared_token", allowShared)
}
