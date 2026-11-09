package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIntegration_FamilyHealthConcurrentReplay(t *testing.T) {
	f := familyTestSetup(t)
	lock, err := f.admin.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(f.ctx)
	if _, err := lock.Exec(f.ctx, "LOCK TABLE family_batch_receipts IN EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	body := f.body(f.uuid(0x6a0), f.uuid(0x6a1), 62.5)
	type outcome struct {
		status int
		data   map[string]json.RawMessage
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.server.URL+"/v1/batches", strings.NewReader(body))
			if err != nil {
				results <- outcome{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+f.actors[0].token)
			response, err := f.server.Client().Do(req)
			if err != nil {
				results <- outcome{err: err}
				return
			}
			defer response.Body.Close()
			var data map[string]json.RawMessage
			err = json.NewDecoder(response.Body).Decode(&data)
			results <- outcome{status: response.StatusCode, data: data, err: err}
		}()
	}
	waitReceiptWriters(t, f, 2)
	if err := lock.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.status != http.StatusOK || second.status != http.StatusOK {
		t.Fatalf("concurrent outcomes: %d/%v, %d/%v", first.status, first.err, second.status, second.err)
	}
	if !reflect.DeepEqual(first.data, second.data) {
		t.Fatal("concurrent retries returned different receipts")
	}
	for _, table := range []string{"batches", "family_batch_receipts", "quantity_samples"} {
		var count int
		if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1", f.actors[0].user).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("concurrent retry left %d rows in %s, want 1", count, table)
		}
	}
}

func waitReceiptWriters(t *testing.T, f *familyTestFixture, want int) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting int
		if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM pg_locks
			WHERE relation='family_batch_receipts'::regclass AND mode='RowExclusiveLock' AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == want {
			break
		}
		select {
		case <-tick.C:
		case <-f.ctx.Done():
			t.Fatal("real transactions did not reach the receipt lock")
		}
	}
}

type lostAcknowledgement struct{ http.ResponseWriter }

func (w lostAcknowledgement) WriteHeader(int) {
	connection, _, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		connection.Close()
	}
}

func (w lostAcknowledgement) Write(body []byte) (int, error) { return len(body), nil }

func TestIntegration_FamilyHealthLostAcknowledgement(t *testing.T) {
	f := familyTestSetup(t)
	completed := make(chan struct{})
	handler := f.server.Config.Handler
	drop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(lostAcknowledgement{w}, r)
		close(completed)
	}))
	t.Cleanup(drop.Close)
	batch := f.uuid(0x6b0)
	body := f.body(batch, f.uuid(0x6b1), 62.5)
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, drop.URL+"/v1/batches", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.actors[0].token)
	if response, err := drop.Client().Do(req); err == nil {
		response.Body.Close()
		t.Fatal("client received a response after the connection was dropped")
	}
	select {
	case <-completed:
	case <-f.ctx.Done():
		t.Fatal("original request did not finish")
	}
	var transaction string
	var committedJSON []byte
	if err := f.admin.QueryRow(f.ctx, `SELECT transaction_id::text, result FROM family_batch_receipts
		WHERE user_id=$1 AND batch_id=$2`, f.actors[0].user, batch).Scan(&transaction, &committedJSON); err != nil {
		t.Fatal(err)
	}
	var committed map[string]any
	if err := json.Unmarshal(committedJSON, &committed); err != nil {
		t.Fatal(err)
	}
	status, retry := f.post(t, f.actors[0], body)
	retryJSON, err := json.Marshal(retry)
	if err != nil {
		t.Fatal(err)
	}
	var replay map[string]any
	if err := json.Unmarshal(retryJSON, &replay); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || !reflect.DeepEqual(committed, replay) {
		t.Fatal("retry after lost response did not return the committed receipt")
	}
	var replayTransaction string
	if err := f.admin.QueryRow(f.ctx, `SELECT transaction_id::text FROM family_batch_receipts
		WHERE user_id=$1 AND batch_id=$2`, f.actors[0].user, batch).Scan(&replayTransaction); err != nil {
		t.Fatal(err)
	}
	if replayTransaction != transaction {
		t.Fatal("lost response caused a second committed transaction")
	}
}
