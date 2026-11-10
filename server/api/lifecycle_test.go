package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// R20: PULS_TIME_ZONE must be the database's zone, or every daily answer is
// cut on different days than metric_daily without an error anywhere. The API
// refuses to start on a confirmed mismatch and only logs what it cannot
// check.
func TestVerifyTimeZone(t *testing.T) {
	t.Parallel()

	berlin := mustLoadZone(t, "Europe/Berlin")
	dbSays := func(name string, err error) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("the database read has no deadline")
			}
			return name, err
		}
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if err := verifyTimeZone(context.Background(), dbSays("Europe/Berlin", nil), berlin, logger); err != nil {
		t.Fatalf("matching zones: %v", err)
	}
	// The spellings of UTC are one zone: the default PULS_TIME_ZONE (empty,
	// read as UTC) against a database that stored Etc/UTC is no mismatch.
	if err := verifyTimeZone(context.Background(), dbSays("Etc/UTC", nil), time.UTC, logger); err != nil {
		t.Fatalf("UTC against Etc/UTC: %v", err)
	}

	err := verifyTimeZone(context.Background(), dbSays("America/Los_Angeles", nil), berlin, logger)
	if err == nil {
		t.Fatal("a mismatch was accepted")
	}
	for _, want := range []string{"PULS_TIME_ZONE", `"Europe/Berlin"`, `"America/Los_Angeles"`, "puls_time_zone()", "docker compose up -d"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mismatch error %q does not mention %s", err, want)
		}
	}

	// A database from before the function existed cannot be checked: say
	// so and start, rather than refuse a stack whose migrate has not run.
	logs.Reset()
	undefined := &pgconn.PgError{Code: sqlstateUndefinedFunction, Message: "function puls_time_zone() does not exist"}
	if err := verifyTimeZone(context.Background(), dbSays("", undefined), berlin, logger); err != nil {
		t.Fatalf("missing function: %v, want a warning and a start", err)
	}
	if !strings.Contains(logs.String(), "puls_time_zone()") || !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("missing function was not logged as a warning: %s", logs.String())
	}
	// So does any other failed read: only a confirmed mismatch stops startup.
	logs.Reset()
	if err := verifyTimeZone(context.Background(), dbSays("", errors.New("connection reset")), berlin, logger); err != nil {
		t.Fatalf("failed read: %v, want a warning and a start", err)
	}
	if !strings.Contains(logs.String(), "unchecked") {
		t.Errorf("failed read was not logged: %s", logs.String())
	}
}

func TestSameTimeZone(t *testing.T) {
	t.Parallel()

	for _, pair := range [][2]string{
		{"", "UTC"}, {"UTC", "Etc/UTC"}, {" UTC ", "Etc/Universal"}, {"Etc/GMT", "UTC"},
		{"Europe/Berlin", "Europe/Berlin"}, {" Europe/Berlin", "Europe/Berlin "},
	} {
		if !sameTimeZone(pair[0], pair[1]) {
			t.Errorf("sameTimeZone(%q, %q) = false, want true", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{
		{"UTC", "Europe/London"}, {"Europe/Berlin", "Europe/Paris"}, {"europe/berlin", "Europe/Berlin"}, {"", "Asia/Tokyo"},
	} {
		if sameTimeZone(pair[0], pair[1]) {
			t.Errorf("sameTimeZone(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
}

func mustLoadZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// F1: once shutdown's grace has run out, an export still streaming is
// cancelled and its response aborted — never closed as if the file were
// complete — so its pooled connection is returned before Compose's SIGKILL.
func TestExportIsAbortedWhenTheServerLifetimeEnds(t *testing.T) {
	t.Parallel()

	lifetime, endLifetime := context.WithCancel(context.Background())
	defer endLifetime()
	scanEnded := make(chan error, 1)
	srv := exportServer(t, &streamingStore{
		workoutRows: func(ctx context.Context, fn func(WorkoutSummary) error) error {
			if err := fn(exportTestWorkout(0)); err != nil {
				return err
			}
			// A long scan, as on a slow database: it ends only when the
			// export's context does.
			<-ctx.Done()
			scanEnded <- ctx.Err()
			return ctx.Err()
		},
	})
	srv.lifetime = lifetime
	server := httptest.NewServer(srv.routes())
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/export?format=csv&dataset=workouts&"+exportRange, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	endLifetime()
	select {
	case err := <-scanEnded:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the scan ended with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the export's scan was not cancelled when the server's lifetime ended")
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("the body read to a clean end; an export cut short by shutdown must break the transfer")
	}
}

// F1, the rest of the router: every other handler's context ends with the
// server's lifetime too, so its query gives its connection back.
func TestHandlersAreCancelledWhenTheServerLifetimeEnds(t *testing.T) {
	t.Parallel()

	lifetime, endLifetime := context.WithCancel(context.Background())
	store := &blockingCatalogStore{release: make(chan struct{})}
	srv := testServer(t, store)
	srv.lifetime = lifetime

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil) }()
	store.waitForCalls(t, 1)
	endLifetime()
	select {
	case rec := <-done:
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500 for a query cancelled by shutdown", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler's query was not cancelled when the server's lifetime ended")
	}
}

// blockingCatalogStore is a fakeStore whose CatalogTypes blocks until its
// context ends or release is closed, counting calls race-free.
type blockingCatalogStore struct {
	fakeStore
	release chan struct{}
	calls   atomic.Int64
	answer  atomic.Int64 // returned as the first type's Rows
	fail    atomic.Bool
}

func (b *blockingCatalogStore) CatalogTypes(ctx context.Context, _ string) ([]CatalogType, error) {
	b.calls.Add(1)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
	}
	if b.fail.Load() {
		return nil, errors.New("database went away")
	}
	return []CatalogType{{Identifier: "HKQuantityTypeIdentifierStepCount", Rows: b.answer.Load()}}, nil
}

func (b *blockingCatalogStore) waitForCalls(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for b.calls.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("CatalogTypes called %d times, want %d", b.calls.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// R19: the catalog query is a decompressing scan, so only a user's first
// request waits for it. A stale answer is served at once while exactly one
// background refresh replaces it; a failed refresh keeps the old answer; an
// answer past catalogMaxStale is not served at all.
func TestCatalogTypesServesStaleWhileOneRefreshRuns(t *testing.T) {
	t.Parallel()

	store := &blockingCatalogStore{release: make(chan struct{})}
	store.answer.Store(1)
	srv := testServer(t, store)
	rows := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// The first request blocks on the query.
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil) }()
	store.waitForCalls(t, 1)
	select {
	case <-first:
		t.Fatal("the first request answered before its query did")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	if body := rows(<-first); !strings.Contains(body, `"rows":1`) {
		t.Fatalf("first answer = %s", body)
	}

	// Past its TTL the answer is served as it is, without waiting, while a
	// refresh runs; however many stale requests arrive, one refresh runs.
	store.release = make(chan struct{})
	store.answer.Store(2)
	ageCatalog(srv, defaultUserID, catalogTTL+time.Second)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if body := rows(serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil)); !strings.Contains(body, `"rows":1`) {
				t.Errorf("stale answer = %s, want the cached one", body)
			}
		}()
	}
	wg.Wait() // every stale request answered while the refresh is blocked
	store.waitForCalls(t, 2)
	close(store.release)
	srv.catalogRefreshes.Wait()
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("CatalogTypes ran %d times, want 2 (one first call, one refresh)", got)
	}
	if body := rows(serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil)); !strings.Contains(body, `"rows":2`) {
		t.Fatalf("after the refresh = %s, want the new answer", body)
	}

	// A refresh that fails keeps the stale answer, and the next stale
	// request tries again.
	store.fail.Store(true)
	ageCatalog(srv, defaultUserID, catalogTTL+time.Second)
	if body := rows(serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil)); !strings.Contains(body, `"rows":2`) {
		t.Fatalf("stale answer = %s", body)
	}
	srv.catalogRefreshes.Wait()
	if body := rows(serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil)); !strings.Contains(body, `"rows":2`) {
		t.Fatalf("after a failed refresh = %s, want the stale answer kept", body)
	}
	srv.catalogRefreshes.Wait()
	if got := store.calls.Load(); got != 4 {
		t.Fatalf("CatalogTypes ran %d times, want 4 (each stale request after a failure retries)", got)
	}

	// Too old to serve: the request waits for the query again.
	store.fail.Store(false)
	store.answer.Store(3)
	ageCatalog(srv, defaultUserID, catalogTTL+catalogMaxStale+time.Second)
	if body := rows(serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil)); !strings.Contains(body, `"rows":3`) {
		t.Fatalf("past the stale window = %s, want a fresh answer", body)
	}
}

// ageCatalog moves user's cached catalog age into the past.
func ageCatalog(srv *Server, user string, age time.Duration) {
	user = catalogKey(user, [32]byte{})
	srv.catalogMu.Lock()
	defer srv.catalogMu.Unlock()
	entry := srv.catalog[user]
	entry.expires = time.Now().Add(catalogTTL - age)
	srv.catalog[user] = entry
}
