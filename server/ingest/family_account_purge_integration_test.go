package main

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIntegration_FamilyHealthAccountPurgeIncludesFamilyMetadata(t *testing.T) {
	f := familyTestSetup(t)
	body := f.body(f.uuid(0x820), f.uuid(0x821), 62.5)
	for _, actor := range f.actors {
		if status, _ := f.post(t, actor, body); status != http.StatusOK {
			t.Fatalf("prepare purge sample status=%d", status)
		}
		if status, _ := f.post(t, actor, familyCompleteSnapshot(t, f, actor, 0x822, 61)); status != http.StatusOK {
			t.Fatalf("prepare purge snapshots status=%d", status)
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
		"family_snapshot_identity", "family_source_profiles",
		"family_record_identity", "family_workout_references", "family_source_tombstones",
		"family_batch_receipts", "family_source_observations", "family_device_generations",
		"family_source_connections", "quantity_samples", "aggregate_samples", "activity_summaries", "batches", "device_tokens",
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
	for table, expected := range map[string]int{"family_snapshot_identity": 4, "family_source_profiles": 1, "aggregate_samples": 2, "activity_summaries": 1} {
		if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1", f.actors[1].user).Scan(&otherRows); err != nil {
			t.Fatal(err)
		}
		if otherRows != expected {
			t.Fatalf("one member's purge changed another member's snapshots in %s", table)
		}
	}
}

func TestIntegration_FamilyHealthMetadataCredentialBindingRemainsRequiredAtCommit(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	if status, _ := f.post(t, actor, f.body(f.uuid(0x830), f.uuid(0x831), 62.5)); status != http.StatusOK {
		t.Fatalf("prepare metadata status=%d", status)
	}
	if status, _ := f.post(t, actor, familyCompleteSnapshot(t, f, actor, 0x832, 61)); status != http.StatusOK {
		t.Fatalf("prepare metadata snapshots status=%d", status)
	}
	var otherSource string
	if err := f.admin.QueryRow(f.ctx, `INSERT INTO family_source_connections(user_id,provider,binding_method)
		VALUES($1,'apple_health','local_member_pairing') RETURNING id::text`, actor.user).Scan(&otherSource); err != nil {
		t.Fatal(err)
	}
	plain, err := newDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(f.ctx, `INSERT INTO device_tokens(token_hash,token_prefix,user_id,name,source_connection_id)
		VALUES($1,$2,$3,'explicit synthetic metadata source',$4)`, hashToken(plain), tokenPrefix(plain), actor.user, otherSource); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []struct{ name, token string }{
		{"different member", f.actors[1].token}, {"different source", plain},
	} {
		var credential string
		if err := f.admin.QueryRow(f.ctx, `SELECT credential_id::text FROM device_tokens WHERE token_hash=$1`, hashToken(wrong.token)).Scan(&credential); err != nil {
			t.Fatal(err)
		}
		for _, relation := range []struct{ table, column string }{
			{"family_record_identity", "first_credential_id"}, {"family_source_observations", "credential_id"},
			{"family_snapshot_identity", "first_credential_id"}, {"family_snapshot_identity", "last_credential_id"},
		} {
			t.Run(wrong.name+"/"+relation.table, func(t *testing.T) {
				tx, err := f.admin.Begin(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(f.ctx)
				if _, err := tx.Exec(f.ctx, "UPDATE "+relation.table+" SET "+relation.column+"=$1 WHERE user_id=$2", credential, actor.user); err != nil {
					t.Fatalf("credential reference was not deferred to the transaction boundary: %v", err)
				}
				err = tx.Commit(f.ctx)
				var constraint *pgconn.PgError
				if !errors.As(err, &constraint) || constraint.Code != "23503" {
					t.Fatal("foreign member or source credential committed into family metadata")
				}
				var unchanged bool
				if err := f.admin.QueryRow(f.ctx, "SELECT bool_and("+relation.column+"<>$1) FROM "+relation.table+" WHERE user_id=$2", credential, actor.user).Scan(&unchanged); err != nil {
					t.Fatal(err)
				}
				if !unchanged {
					t.Fatal("failed credential constraint left modified metadata")
				}
			})
		}
	}
}
