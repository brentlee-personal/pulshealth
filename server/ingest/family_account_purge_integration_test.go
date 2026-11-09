package main

import (
	"net/http"
	"reflect"
	"testing"
)

func TestIntegration_FamilyHealthAccountPurgeIncludesFamilyMetadata(t *testing.T) {
	f := familyTestSetup(t)
	body := f.body(f.uuid(0x820), f.uuid(0x821), 62.5)
	for _, actor := range f.actors {
		if status, _ := f.post(t, actor, body); status != http.StatusOK {
			t.Fatalf("prepare purge sample status=%d", status)
		}
		if status, _ := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`); status != http.StatusOK {
			t.Fatalf("prepare purge observation status=%d", status)
		}
	}
	_, otherReceipt := f.post(t, f.actors[1], body)
	var counts []byte
	if err := f.admin.QueryRow(f.ctx, `SELECT auth.purge_user_data($1)`, f.actors[0].user).Scan(&counts); err != nil {
		var remaining int
		if probe := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM quantity_samples WHERE user_id=$1`, f.actors[0].user).Scan(&remaining); probe != nil {
			t.Fatal(probe)
		}
		if remaining != 1 {
			t.Fatal("failed account purge left a partial health-data deletion")
		}
		t.Fatalf("family-backed account purge was rejected: %v", err)
	}
	for _, table := range []string{
		"family_record_identity", "family_workout_references", "family_source_tombstones",
		"family_batch_receipts", "family_source_observations", "family_device_generations",
		"family_source_connections", "quantity_samples", "batches", "device_tokens",
	} {
		var remaining int
		if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1", f.actors[0].user).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatalf("purged account retained owned rows in %s", table)
		}
	}
	if status, _ := f.post(t, f.actors[0], body); status != http.StatusUnauthorized {
		t.Fatalf("purged device credential status=%d", status)
	}
	if status, replay := f.post(t, f.actors[1], body); status != http.StatusOK || !reflect.DeepEqual(otherReceipt, replay) {
		t.Fatal("one member's purge changed another member's original receipt")
	}
	var otherRows int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM quantity_samples WHERE user_id=$1`, f.actors[1].user).Scan(&otherRows); err != nil {
		t.Fatal(err)
	}
	if otherRows != 1 {
		t.Fatal("one member's purge erased another member's same-ID sample")
	}
}
