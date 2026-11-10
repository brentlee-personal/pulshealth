package main

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeResolver stands in for device_tokens: a map from hash to row, an
// optional error, and a call counter so a test can prove the lookup never
// happened.
type fakeResolver struct {
	tokens map[string]deviceToken // keyed by hex(hash)
	err    error
	calls  atomic.Int64
}

func newFakeResolver() *fakeResolver { return &fakeResolver{tokens: map[string]deviceToken{}} }

func (f *fakeResolver) add(plaintext string, d deviceToken) {
	d.TokenPrefix = tokenPrefix(plaintext)
	f.tokens[hex.EncodeToString(hashToken(plaintext))] = d
}

func (f *fakeResolver) ResolveDeviceToken(_ context.Context, hash []byte) (deviceToken, error) {
	f.calls.Add(1)
	if f.err != nil {
		return deviceToken{}, f.err
	}
	d, ok := f.tokens[hex.EncodeToString(hash)]
	if !ok {
		return deviceToken{}, errTokenNotFound
	}
	return d, nil
}

const (
	deviceUser  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	otherUser   = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	activeTok   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	revokedTok  = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	unknownTok  = "1111111111111111111111111111111111111111111111111111111111111111"
	devicePeer  = "203.0.113.50:5000"
	deviceOther = "203.0.113.51:5000"
)

// newDeviceTestServer is newTestServer plus a resolver holding one active and
// one revoked token, both for deviceUser.
func newDeviceTestServer(fs *fakeStore) (*Server, *fakeResolver) {
	fr := newFakeResolver()
	fr.add(activeTok, deviceToken{ID: 7, UserID: deviceUser, Status: "active", SourceConnectionID: "00000000-0000-4000-8000-000000000010", SourceActive: true})
	fr.add(revokedTok, deviceToken{ID: 8, UserID: deviceUser, Status: "revoked"})
	srv := newServer(fs, fr, false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	return srv, fr
}

func TestRetiredSharedTokenCannotSelectMember(t *testing.T) {
	fs := &fakeStore{}
	srv, _ := newDeviceTestServer(fs)
	for _, headers := range []map[string]string{nil, {"X-User-ID": otherUser}, {"X-User-ID": "nope"}} {
		if code, _ := authAttempt(t, srv, "secret", devicePeer, headers); code != http.StatusUnauthorized {
			t.Errorf("retired shared credential status = %d, want 401", code)
		}
	}
	if fs.gotUserID != "" {
		t.Fatal("an unknown credential selected a member")
	}
}

func TestDeviceTokenIsBoundToItsUser(t *testing.T) {
	fs := &fakeStore{}
	srv, _ := newDeviceTestServer(fs)

	// Header absent: the token's user, not the default one.
	if code, _ := authAttempt(t, srv, activeTok, devicePeer, nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if fs.gotUserID != deviceUser {
		t.Fatalf("store user = %q, want the token's user %q", fs.gotUserID, deviceUser)
	}

	// Header equal (any case): fine.
	fs.gotUserID = ""
	if code, _ := authAttempt(t, srv, activeTok, devicePeer,
		map[string]string{"X-User-ID": strings.ToUpper(deviceUser)}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a matching X-User-ID", code)
	}
	if fs.gotUserID != deviceUser {
		t.Fatalf("store user = %q, want %q", fs.gotUserID, deviceUser)
	}

	// Header naming someone else: refused, handler never runs, nothing charged.
	fs.gotUserID = ""
	if code, _ := authAttempt(t, srv, activeTok, devicePeer, map[string]string{"X-User-ID": otherUser}); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a different X-User-ID", code)
	}
	if fs.gotUserID != "" {
		t.Fatalf("handler ran with user %q after a 403", fs.gotUserID)
	}
	if n := srv.authFailures.size(); n != 0 {
		t.Fatalf("limiter tracks %d addresses after a user mismatch, want 0: a valid token is not a guess", n)
	}
	// Malformed still 400, as for the shared token.
	if code, _ := authAttempt(t, srv, activeTok, devicePeer, map[string]string{"X-User-ID": "nope"}); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed X-User-ID", code)
	}
}

func TestUnknownAndRevokedDeviceTokensAreCharged(t *testing.T) {
	cases := map[string]string{"unknown": unknownTok, "revoked": revokedTok, "missing bearer": ""}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			fs := &fakeStore{}
			srv, _ := newDeviceTestServer(fs)
			if code, _ := authAttempt(t, srv, token, devicePeer, nil); code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", code)
			}
			if fs.gotUserID != "" {
				t.Fatalf("handler ran with user %q", fs.gotUserID)
			}
			if n := srv.authFailures.size(); n != 1 {
				t.Fatalf("limiter tracks %d addresses, want 1: a wrong credential is charged", n)
			}
		})
	}
}

// The app retries 5xx and treats 401 as terminal, so a database outage must
// look like an outage, and it must not spend the phone's failure budget.
func TestResolverErrorIs503AndNotCharged(t *testing.T) {
	fs := &fakeStore{}
	srv, fr := newDeviceTestServer(fs)
	fr.err = errors.New("connection refused")

	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+activeTok)
	req.RemoteAddr = devicePeer
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication unavailable") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if n := srv.authFailures.size(); n != 0 {
		t.Fatalf("limiter tracks %d addresses after a lookup failure, want 0", n)
	}
	if code, _ := authAttempt(t, srv, "secret", devicePeer, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("unknown credential during an outage: status = %d, want 503", code)
	}
}

func TestUnknownUploadCredentialIsRefused(t *testing.T) {
	fs := &fakeStore{}
	srv, _ := newDeviceTestServer(fs)
	if code, _ := authAttempt(t, srv, "secret", devicePeer, nil); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: the shared value is no longer a credential", code)
	}
	if n := srv.authFailures.size(); n != 1 {
		t.Fatalf("limiter tracks %d addresses, want 1", n)
	}
	if code, _ := authAttempt(t, srv, activeTok, deviceOther, nil); code != http.StatusOK {
		t.Fatalf("device token: status = %d, want 200", code)
	}
	empty := newServer(fs, newFakeResolver(), false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code, _ := authAttempt(t, empty, "", deviceOther, nil); code != http.StatusUnauthorized {
		t.Fatalf("empty bearer status = %d, want 401", code)
	}
}

// Refusal happens before any comparison — including the database lookup.
func TestThrottledIPNeverReachesTheResolver(t *testing.T) {
	fs := &fakeStore{}
	srv, fr := newDeviceTestServer(fs)
	// Exhaust the budget with missing bearers, which never consult the resolver.
	for range authFailureBurst {
		authAttempt(t, srv, "", devicePeer, nil)
	}
	if n := fr.calls.Load(); n != 0 {
		t.Fatalf("resolver called %d times for missing bearers, want 0", n)
	}
	if code, _ := authAttempt(t, srv, activeTok, devicePeer, nil); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 even with a valid device token", code)
	}
	if n := fr.calls.Load(); n != 0 {
		t.Fatalf("resolver called %d times for a throttled request, want 0", n)
	}
}

func TestBatchRecordsTheDeviceToken(t *testing.T) {
	fs := &fakeStore{}
	srv, _ := newDeviceTestServer(fs)

	req := httptest.NewRequest(http.MethodPost, "/v1/batches", strings.NewReader(ndjson(t, 0, 0)))
	req.Header.Set("Authorization", "Bearer "+activeTok)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if fs.gotBatch == nil {
		t.Fatal("batch not inserted")
	}
	if fs.gotBatch.Header.UserID != deviceUser || fs.gotBatch.Header.DeviceTokenID != 7 {
		t.Fatalf("batch user/token = %q/%d, want %q/7", fs.gotBatch.Header.UserID, fs.gotBatch.Header.DeviceTokenID, deviceUser)
	}

	fs.gotBatch = nil
	req = httptest.NewRequest(http.MethodPost, "/v1/batches", strings.NewReader(ndjson(t, 0, 0)))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || fs.gotBatch != nil {
		t.Fatalf("retired shared batch status = %d, want 401 without insertion", rec.Code)
	}
}

// A batch refused for a user mismatch never reaches the parser, so the
// middleware writes the durable rejection row itself, after the response.
func TestBatchUserMismatchIsRecordedAsRejection(t *testing.T) {
	fs := &fakeStore{}
	srv, _ := newDeviceTestServer(fs)
	rec := httptest.NewRecorder()
	fs.onRejection = func() {
		if rec.Code != http.StatusForbidden {
			t.Errorf("rejection recorded before the response (recorder code %d)", rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/batches", strings.NewReader(ndjson(t, 0, 0)))
	req.Header.Set("Authorization", "Bearer "+activeTok)
	req.Header.Set("X-User-ID", otherUser)
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if fs.gotBatch != nil {
		t.Fatal("batch inserted despite the mismatch")
	}
	if len(fs.rejections) != 1 || fs.rejections[0].Stage != "identity" || fs.rejections[0].Status != http.StatusForbidden {
		t.Fatalf("rejections = %+v, want one identity/403 row", fs.rejections)
	}
	if fs.rejections[0].UserID != deviceUser {
		t.Errorf("rejection owner = %q, want authenticated user %q", fs.rejections[0].UserID, deviceUser)
	}
	// Reads are refused the same way but leave no batch-rejection row.
	fs.rejections = nil
	if code, _ := authAttempt(t, srv, activeTok, devicePeer, map[string]string{"X-User-ID": otherUser}); code != http.StatusForbidden {
		t.Fatalf("read status = %d, want 403", code)
	}
	if len(fs.rejections) != 0 {
		t.Fatalf("a refused read recorded %d batch rejections", len(fs.rejections))
	}
}

// An absent resolver never supplies an implicit credential.
func TestNoResolverFailsClosed(t *testing.T) {
	srv := newServer(&fakeStore{}, nil, false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code, _ := authAttempt(t, srv, activeTok, devicePeer, nil); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestPrincipalRoundTripsThroughContext(t *testing.T) {
	ctx := withPrincipal(context.Background(), principal{userID: deviceUser, tokenID: 3})
	p, ok := principalFrom(ctx)
	if !ok || p.userID != deviceUser || p.tokenID != 3 {
		t.Fatalf("principal = %+v, %v", p, ok)
	}
	if _, ok := principalFrom(context.Background()); ok {
		t.Fatal("empty context yielded a principal")
	}
}

// Identity comes only from the auth middleware. A handler reached without
// it (a route someone forgot to wrap) must not fall back to X-User-ID —
// that would be unauthenticated tenant selection — and serves nothing.
func TestHandlersWithoutAuthNeverTrustXUserID(t *testing.T) {
	fs := &fakeStore{}
	srv := newTestServer(fs)
	for name, handler := range map[string]http.HandlerFunc{
		"stats":  srv.handleStats,
		"routes": srv.handleRoutes,
		"batch":  srv.handleBatch,
	} {
		method := http.MethodGet
		var body io.Reader
		if name == "batch" {
			method = http.MethodPost
			body = strings.NewReader(ndjson(t, 1, 0, hrSample))
		}
		req := httptest.NewRequest(method, "/v1/"+name, body)
		req.Header.Set("X-User-ID", otherUser)
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s without auth: status = %d, want 500", name, rec.Code)
		}
		if fs.gotUserID != "" || fs.gotBatch != nil {
			t.Fatalf("%s without auth reached the store as %q", name, fs.gotUserID)
		}
	}
}
