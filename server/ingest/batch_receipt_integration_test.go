package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func assertStoredBatchReplay(t *testing.T, first, replay IngestResult) {
	t.Helper()
	if !replay.DuplicateBatch || first.receipt == nil || replay.receipt == nil {
		t.Fatal("replay must carry the original durable receipt")
	}
	if first.receipt.TransactionID != replay.receipt.TransactionID || !first.receipt.CommittedAt.Equal(replay.receipt.CommittedAt) {
		t.Fatal("replay changed the commit identity or time")
	}
	first.receipt, replay.receipt = nil, nil
	first.DuplicateBatch, replay.DuplicateBatch = false, false
	first.Retries, replay.Retries = 0, 0
	if !reflect.DeepEqual(first, replay) {
		t.Fatal("replay changed committed counts, transaction or timestamp")
	}
}

func TestIntegration_FamilyHealthReceiptIgnoresGzipMetadata(t *testing.T) {
	f := familyTestSetup(t)
	body := f.body(f.uuid(0x660), f.uuid(0x661), 62.5)
	status, original := f.post(t, f.actors[0], body)
	if status != http.StatusOK {
		t.Fatalf("original status=%d", status)
	}
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	w.Name, w.ModTime = "synthetic-retry", time.Unix(1700000000, 0)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.server.URL+"/v1/batches", &compressed)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.actors[0].token)
	req.Header.Set("Content-Encoding", "gzip")
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var retry map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&retry); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !reflect.DeepEqual(original, retry) {
		t.Fatal("compression metadata changed the committed receipt")
	}
}

func TestIntegration_FamilyHealthFailedWriteLeavesNoReceipt(t *testing.T) {
	f := familyTestSetup(t)
	name := fmt.Sprintf("fhd_test_failure_%x", f.run)
	_, err := f.admin.Exec(f.ctx, fmt.Sprintf(`ALTER TABLE quantity_samples ADD CONSTRAINT %s
		CHECK (user_id <> '%s'::uuid OR value IS DISTINCT FROM 62.5)`, name, f.actors[0].user))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.admin.Exec(f.ctx, "ALTER TABLE quantity_samples DROP CONSTRAINT IF EXISTS "+name); err != nil {
			t.Errorf("remove owned fault constraint: %v", err)
		}
	})
	batch, sample := f.uuid(0x670), f.uuid(0x671)
	body := f.body(batch, sample, 62.5)
	if status, _ := f.post(t, f.actors[0], body); status != http.StatusInternalServerError {
		t.Fatalf("injected database failure status=%d", status)
	}
	for _, table := range []string{"batches", "family_batch_receipts"} {
		var count int
		if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1 AND batch_id=$2", f.actors[0].user, batch).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("failed write left %d rows in %s", count, table)
		}
	}
	if _, err := f.admin.Exec(f.ctx, "ALTER TABLE quantity_samples DROP CONSTRAINT "+name); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.post(t, f.actors[0], body); status != http.StatusOK {
		t.Fatalf("retry after actual rollback status=%d", status)
	}
}

func TestIntegration_FamilyHealthIncompleteReceiptCannotCommit(t *testing.T) {
	f := familyTestSetup(t)
	batch := f.uuid(0x680)
	tx, err := f.admin.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := tx.Exec(f.ctx, "INSERT INTO batches(user_id,batch_id) VALUES($1,$2)", f.actors[0].user, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `INSERT INTO family_batch_receipts(user_id,device_generation,batch_id,body_digest)
		VALUES($1,0,$2,$3)`, f.actors[0].user, batch, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err == nil {
		t.Fatal("incomplete receipt committed")
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM batches WHERE user_id=$1 AND batch_id=$2", f.actors[0].user, batch).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed deferred check did not roll back the batch")
	}
}
