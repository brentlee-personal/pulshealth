package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRetiredUploadConfigurationIsRefused(t *testing.T) {
	for _, name := range []string{"PULS_TOKEN", "PULS_ALLOW_SHARED_TOKEN", "PULS_USER_ID"} {
		for _, value := range []string{"false", "synthetic-private-value"} {
			err := validateIngestAuthConfig(func(key string) string {
				if key == name {
					return value
				}
				return ""
			})
			if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "synthetic-private-value") {
				t.Fatalf("retired setting %s was accepted or revealed", name)
			}
		}
	}
	if err := validateIngestAuthConfig(func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
}

func TestMaxInflightBatches(t *testing.T) {
	for raw, want := range map[string]int{
		"": defaultMaxInflightBatches, "four": defaultMaxInflightBatches,
		"0": 1, "-3": 1, " 2 ": 2, "16": 16,
	} {
		if got := maxInflightBatches(raw); got != want {
			t.Errorf("maxInflightBatches(%q) = %d, want %d", raw, got, want)
		}
	}
}

// batchRequest is an authenticated one-sample batch upload on ctx.
func batchRequest(t *testing.T, ctx context.Context, token string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/batches", gzipBody(t, ndjson(t, 1, 0, hrSample)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Encoding", "gzip")
	return req
}

// Every batch may decode to 64 MiB, so only so many are handled at once.
// Beyond that a request waits briefly for a slot, then gets a 503 the app
// retries — after auth, and before its body is decoded.
func TestBatchSlotsBoundConcurrentBatches(t *testing.T) {
	fs := &fakeStore{}
	srv := newTestServer(fs)
	srv.batchSlots = make(chan struct{}, 1)
	srv.batchSlots <- struct{}{} // another batch holds the only slot
	handler := srv.routes()

	// A wrong token is still a 401: a full house does not skip auth.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, batchRequest(t, context.Background(), "wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token while full: status = %d, want 401", rec.Code)
	}

	// No slot frees up within the wait (cut short by the client's deadline).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, batchRequest(t, ctx, "secret"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while every slot is busy", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Errorf("body = %q, want the usual JSON error", rec.Body.String())
	}
	if fs.gotBatch != nil {
		t.Error("a refused batch reached the store")
	}

	// A slot that frees up during the wait is taken, and given back after.
	go func() {
		time.Sleep(20 * time.Millisecond)
		<-srv.batchSlots
	}()
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, batchRequest(t, context.Background(), "secret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once a slot freed up; body %s", rec.Code, rec.Body.String())
	}
	if n := len(srv.batchSlots); n != 0 {
		t.Errorf("%d slots still held after the batch finished", n)
	}
}

// An insert outlives a client that hangs up, but not the server's shutdown:
// otherwise pool.Close waits out insertDeadline and Compose SIGKILLs.
func TestInsertIsCancelledByShutdownNotByTheClient(t *testing.T) {
	lifetime, endLifetime := context.WithCancel(context.Background())
	defer endLifetime()
	started := make(chan context.Context, 1)
	fs := &fakeStore{onInsert: func(ctx context.Context) error {
		started <- ctx
		<-ctx.Done()
		return ctx.Err()
	}}
	srv := newTestServer(fs)
	srv.lifetime = lifetime

	reqCtx, hangUp := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.routes().ServeHTTP(rec, batchRequest(t, reqCtx, "secret"))
	}()

	insertCtx := <-started
	if deadline, ok := insertCtx.Deadline(); !ok || time.Until(deadline) > insertDeadline {
		t.Errorf("insert deadline = %v (set %v), want at most %v away", deadline, ok, insertDeadline)
	}
	hangUp()
	select {
	case <-insertCtx.Done():
		t.Fatal("the client hanging up cancelled the insert")
	case <-time.After(50 * time.Millisecond):
	}

	endLifetime()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the insert outlived the server's lifetime")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for an insert cut short", rec.Code)
	}
}
