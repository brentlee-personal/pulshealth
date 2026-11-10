package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// authAttempt sends one request through the real route table and reports the
// status code plus the Retry-After header, if any.
func authAttempt(t *testing.T, srv *Server, token, remoteAddr string, headers map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Retry-After")
}

func TestAuthFailuresAreThrottledPerIP(t *testing.T) {
	srv := newTestServer(&fakeStore{})

	for i := range authFailureBurst {
		if code, _ := authAttempt(t, srv, "wrong", "203.0.113.7:5000", nil); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i+1, code)
		}
	}
	code, retryAfter := authAttempt(t, srv, "wrong", "203.0.113.7:5000", nil)
	if code != http.StatusTooManyRequests {
		t.Fatalf("status after %d failures = %d, want 429", authFailureBurst, code)
	}
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q, want a positive integer", retryAfter)
	}
	// Six seconds per token at ten per minute, plus the rounding-up second.
	if seconds > 10 {
		t.Fatalf("Retry-After = %d s, want the next token within ~7 s", seconds)
	}
}

// The whole point of refusing before the comparison: once an address is out of
// budget, even the correct token is not evaluated, so guessing buys nothing.
func TestThrottledIPIsRefusedBeforeTheTokenIsChecked(t *testing.T) {
	fs := &fakeStore{}
	srv := newTestServer(fs)
	for range authFailureBurst {
		authAttempt(t, srv, "wrong", "203.0.113.8:5000", nil)
	}
	if code, _ := authAttempt(t, srv, "secret", "203.0.113.8:5000", nil); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 even with the right token", code)
	}
	if fs.gotUserID != "" {
		t.Fatalf("handler ran for a throttled request (store user %q)", fs.gotUserID)
	}
}

// A backfill is thousands of authenticated requests in a row; none of them may
// cost the client anything.
func TestSuccessfulAuthIsNeverThrottled(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	for i := range authFailureBurst * 20 {
		if code, _ := authAttempt(t, srv, "secret", "203.0.113.9:5000", nil); code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, code)
		}
	}
	if n := srv.authFailures.size(); n != 0 {
		t.Fatalf("limiter tracks %d addresses after only successful requests, want 0", n)
	}
}

func TestAuthFailureBucketsAreIndependentPerIP(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	for range authFailureBurst + 3 {
		authAttempt(t, srv, "wrong", "203.0.113.10:5000", nil)
	}
	if code, _ := authAttempt(t, srv, "wrong", "203.0.113.10:5000", nil); code != http.StatusTooManyRequests {
		t.Fatalf("first address: status = %d, want 429", code)
	}
	if code, _ := authAttempt(t, srv, "wrong", "198.51.100.4:5000", nil); code != http.StatusUnauthorized {
		t.Fatalf("second address: status = %d, want 401 (its own bucket)", code)
	}
	if code, _ := authAttempt(t, srv, "secret", "198.51.100.4:5000", nil); code != http.StatusOK {
		t.Fatalf("second address with the right token: status = %d, want 200", code)
	}
}

// X-Forwarded-For is attacker-controlled. Unless a proxy is declared, two
// requests with different forwarded addresses must share one bucket.
func TestForwardedHeaderIsIgnoredUnlessTrusted(t *testing.T) {
	srv := newTestServer(&fakeStore{}) // trustProxyHeaders = false
	for i := range authFailureBurst {
		spoofed := map[string]string{"X-Forwarded-For": "10.0.0." + strconv.Itoa(i)}
		if code, _ := authAttempt(t, srv, "wrong", "203.0.113.11:5000", spoofed); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i+1, code)
		}
	}
	spoofed := map[string]string{"X-Forwarded-For": "10.0.0.99"}
	if code, _ := authAttempt(t, srv, "wrong", "203.0.113.11:5000", spoofed); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: a spoofed X-Forwarded-For must not mint a new bucket", code)
	}
}

func TestForwardedHeaderIsUsedWhenTrusted(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	srv.trustProxyHeaders = true
	// Every request arrives from the proxy's address; the clients differ.
	for range authFailureBurst {
		authAttempt(t, srv, "wrong", "172.17.0.2:5000", map[string]string{"X-Forwarded-For": "198.51.100.20"})
	}
	if code, _ := authAttempt(t, srv, "wrong", "172.17.0.2:5000",
		map[string]string{"X-Forwarded-For": "198.51.100.20"}); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 for the exhausted forwarded client", code)
	}
	// Prepending an entry of its own does not buy the exhausted client a
	// fresh bucket: the proxy appends what it saw, and that is the key.
	if code, _ := authAttempt(t, srv, "wrong", "172.17.0.2:5000",
		map[string]string{"X-Forwarded-For": "203.0.113.99, 198.51.100.20"}); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: a client-written first entry must not mint a new bucket", code)
	}
	// A second client behind the same proxy still has its own budget.
	if code, _ := authAttempt(t, srv, "secret", "172.17.0.2:5000",
		map[string]string{"X-Forwarded-For": "198.51.100.21"}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a different forwarded client", code)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		forwarded  string
		trust      bool
		want       string
	}{
		{"remote addr by default", "203.0.113.5:41234", "", false, "203.0.113.5"},
		{"forwarded ignored by default", "203.0.113.5:41234", "198.51.100.1", false, "203.0.113.5"},
		{"forwarded honoured when trusted", "172.17.0.2:41234", "198.51.100.1", true, "198.51.100.1"},
		{"last forwarded entry wins", "172.17.0.2:41234", " 10.0.0.1 , 198.51.100.1 ", true, "198.51.100.1"},
		{"client-written first entry is ignored", "172.17.0.2:41234", "evil, real", true, "real"},
		{"trailing blank entry is skipped", "172.17.0.2:41234", "198.51.100.1, ", true, "198.51.100.1"},
		{"empty forwarded falls back", "172.17.0.2:41234", "", true, "172.17.0.2"},
		{"blank forwarded entries fall back", "172.17.0.2:41234", " , ", true, "172.17.0.2"},
		{"ipv6 remote addr loses the port", "[2001:db8::1]:41234", "", false, "2001:db8::1"},
		{"unparsable remote addr is used whole", "not-host-port", "", false, "not-host-port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if got := clientIP(req, tc.trust); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFailureLimiterRefills(t *testing.T) {
	l := newFailureLimiterWith(3, 60, time.Minute, 100) // one token per second
	start := time.Unix(1_700_000_000, 0)

	for range 3 {
		l.recordFailure("a", start)
	}
	if ok, wait := l.allow("a", start); ok {
		t.Fatal("bucket should be empty after three failures")
	} else if wait <= 0 {
		t.Fatalf("Retry-After hint = %v, want > 0", wait)
	}
	// Still empty a fraction of a token later.
	if ok, _ := l.allow("a", start.Add(500*time.Millisecond)); ok {
		t.Fatal("bucket refilled too early")
	}
	if ok, _ := l.allow("a", start.Add(2*time.Second)); !ok {
		t.Fatal("bucket did not refill after two seconds")
	}
	// And it never exceeds the burst, however long it idles.
	for range 3 {
		l.recordFailure("a", start.Add(time.Hour))
	}
	if ok, _ := l.allow("a", start.Add(time.Hour)); ok {
		t.Fatal("a long idle period must not raise the ceiling above the burst")
	}
}

func TestFailureLimiterForgetsIdleAddresses(t *testing.T) {
	l := newFailureLimiterWith(2, 60, time.Minute, 100)
	start := time.Unix(1_700_000_000, 0)

	l.recordFailure("stale", start)
	if l.size() != 1 {
		t.Fatalf("size = %d, want 1", l.size())
	}
	// Long enough for "stale" to refill completely and go idle past the TTL.
	l.recordFailure("fresh", start.Add(2*time.Hour))
	if l.size() != 1 {
		t.Fatalf("size = %d after the sweep, want 1 (only the fresh address)", l.size())
	}
	if ok, _ := l.allow("stale", start.Add(2*time.Hour)); !ok {
		t.Fatal("a forgotten address must start allowed")
	}
}

func TestFailureLimiterIsBoundedUnderAddressRotation(t *testing.T) {
	const maxKeys = 40
	l := newFailureLimiterWith(1, 60, time.Hour, maxKeys)
	now := time.Unix(1_700_000_000, 0)
	// Every address stays hostile (empty bucket, recent), so nothing is
	// forgettable by TTL: only the hard cap can hold the map down.
	for i := range maxKeys * 10 {
		now = now.Add(time.Millisecond)
		l.recordFailure("10.1."+strconv.Itoa(i/256)+"."+strconv.Itoa(i%256), now)
	}
	if n := l.size(); n > maxKeys {
		t.Fatalf("limiter tracks %d addresses, want at most %d", n, maxKeys)
	}
}

// The unauthenticated liveness probe must never be throttled: it is what the
// container health check and scripts/bootstrap.sh poll.
func TestHealthzIsNotRateLimited(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	for range authFailureBurst + 5 {
		authAttempt(t, srv, "wrong", "203.0.113.12:5000", nil)
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "203.0.113.12:5000"
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
}

func TestThrottledResponseIsJSON(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	for range authFailureBurst {
		authAttempt(t, srv, "wrong", "203.0.113.13:5000", nil)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/batches", strings.NewReader("{}"))
	req.RemoteAddr = "203.0.113.13:5000"
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "too many failed authentications") {
		t.Fatalf("body = %q", body)
	}
}
