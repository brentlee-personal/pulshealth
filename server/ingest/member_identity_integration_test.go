package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type familyTestActor struct {
	user  string
	token string
}

type familyTestFixture struct {
	ctx    context.Context
	admin  *pgxpool.Pool
	store  *Store
	server *httptest.Server
	actors [2]familyTestActor
	run    int64
}

func familyTestSetup(t *testing.T) *familyTestFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &familyTestFixture{ctx: ctx, admin: adminPool(t, ctx), run: time.Now().UnixNano()}
	store := NewStore(pool)
	f.store = store
	for i := range f.actors {
		user := f.uuid(0x600 + i)
		plain, _, err := store.IssueDeviceToken(ctx, user, "synthetic family fixture")
		if err != nil {
			t.Fatal(err)
		}
		f.actors[i] = familyTestActor{user: user, token: plain}
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		users := []string{f.actors[0].user, f.actors[1].user}
		for _, table := range []string{
			"workout_route_points", "workout_series_points", "quantity_samples", "category_samples",
			"heartbeat_series", "ecg_samples", "state_of_mind", "medication_dose_events",
			"aggregate_samples", "activity_summaries", "workouts", "batches", "device_tokens", "users",
		} {
			column := "user_id"
			if table == "users" {
				column = "id"
			}
			if _, err := f.admin.Exec(cleanupCtx, "DELETE FROM "+table+" WHERE "+column+"=ANY($1::uuid[])", users); err != nil {
				t.Errorf("clean owned fixture %s: %v", table, err)
			}
		}
		if _, err := f.admin.Exec(cleanupCtx, "DELETE FROM ingest_rejections WHERE user_id=ANY($1::text[])", users); err != nil {
			t.Errorf("clean owned fixture diagnostics: %v", err)
		}
	})
	srv := newServer(store, store, "", false, false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	f.server = httptest.NewServer(srv.routes())
	t.Cleanup(f.server.Close)
	return f
}

func (f *familyTestFixture) uuid(slot int) string {
	return fmt.Sprintf("%08x-%04x-4000-8000-%012x", f.run>>32, slot, f.run&0xffffffffffff)
}

func (f *familyTestFixture) body(batchID, sampleID string, value float64) string {
	return fmt.Sprintf(`{"batchID":%q,"deviceID":"synthetic-phone","type":"HKQuantityTypeIdentifierHeartRate","reason":"manual","exportedAt":1718000000000,"sampleCount":1,"deletionCount":0}
{"uuid":%q,"type":"HKQuantityTypeIdentifierHeartRate","kind":"quantity","start":1718000000000,"end":1718000005000,"value":%g,"unit":"count/min"}
`, batchID, sampleID, value)
}

func (f *familyTestFixture) post(t *testing.T, actor familyTestActor, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.server.URL+"/v1/batches", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+actor.token)
	req.Header.Set("Content-Encoding", "identity")
	req.Header.Set("X-Puls-Protocol", "1")
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var data map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func TestIntegration_FamilyHealthSameSampleIDIsMemberScoped(t *testing.T) {
	f := familyTestSetup(t)
	sample := f.uuid(0x610)
	for i, actor := range f.actors {
		status, _ := f.post(t, actor, f.body(f.uuid(0x611+i), sample, 62.5+float64(i)))
		if status != http.StatusOK {
			t.Fatalf("member %d upload status=%d", i, status)
		}
	}
	var members int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(DISTINCT user_id) FROM quantity_samples WHERE uuid=$1`, sample).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 2 {
		t.Errorf("members retaining identical source sample ID=%d, want 2", members)
	}
}

func TestIntegration_FamilyHealthBatchIDIsMemberScoped(t *testing.T) {
	f := familyTestSetup(t)
	batch := f.uuid(0x620)
	for i, actor := range f.actors {
		status, _ := f.post(t, actor, f.body(batch, f.uuid(0x621+i), 62.5+float64(i)))
		if status != http.StatusOK {
			t.Fatalf("member %d upload status=%d", i, status)
		}
	}
	var members int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(DISTINCT user_id) FROM batches WHERE batch_id=$1`, batch).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 2 {
		t.Errorf("members retaining identical client batch ID=%d, want 2", members)
	}
}

func TestIntegration_FamilyHealthBatchDigestConflictRefused(t *testing.T) {
	f := familyTestSetup(t)
	batch, sample := f.uuid(0x630), f.uuid(0x631)
	if status, _ := f.post(t, f.actors[0], f.body(batch, sample, 62.5)); status != http.StatusOK {
		t.Fatalf("initial upload status=%d", status)
	}
	if status, _ := f.post(t, f.actors[0], f.body(batch, sample, 63.5)); status != http.StatusConflict {
		t.Errorf("different body under committed batch key status=%d, want 409", status)
	}
	var value float64
	if err := f.admin.QueryRow(f.ctx, `SELECT value FROM quantity_samples WHERE uuid=$1 AND user_id=$2`, sample, f.actors[0].user).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 62.5 {
		t.Errorf("committed value=%g, want unchanged synthetic 62.5", value)
	}
}

func TestIntegration_FamilyHealthBatchReplayReturnsOriginalReceipt(t *testing.T) {
	f := familyTestSetup(t)
	body := f.body(f.uuid(0x640), f.uuid(0x641), 62.5)
	firstStatus, first := f.post(t, f.actors[0], body)
	retryStatus, retry := f.post(t, f.actors[0], body)
	if firstStatus != http.StatusOK || retryStatus != http.StatusOK {
		t.Fatalf("upload/retry status=%d/%d", firstStatus, retryStatus)
	}
	if !reflect.DeepEqual(first, retry) {
		t.Error("identical retry changed its committed receipt")
	}
}

func TestIntegration_FamilyHealthBatchDigestIncludesTrailingBytes(t *testing.T) {
	f := familyTestSetup(t)
	body := f.body(f.uuid(0x650), f.uuid(0x651), 62.5)
	if status, _ := f.post(t, f.actors[0], body); status != http.StatusOK {
		t.Fatalf("initial upload status=%d", status)
	}
	if status, _ := f.post(t, f.actors[0], body+"\n"); status != http.StatusConflict {
		t.Errorf("changed decompressed bytes status=%d, want 409", status)
	}
}
