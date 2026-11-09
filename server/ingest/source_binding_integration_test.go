package main

import (
	"net/http"
	"strings"
	"testing"
)

func familyDeviceBinding(t *testing.T, f *familyTestFixture, actor familyTestActor) (string, int64) {
	t.Helper()
	var source string
	var generation int64
	if err := f.admin.QueryRow(f.ctx, `SELECT source_connection_id::text, device_generation
		FROM device_tokens WHERE user_id=$1 AND token_hash=$2`, actor.user, hashToken(actor.token)).Scan(&source, &generation); err != nil {
		t.Fatal(err)
	}
	return source, generation
}

func TestIntegration_FamilyHealthRePairPreservesSource(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	source, generation := familyDeviceBinding(t, f, actor)
	var oldID int64
	if err := f.admin.QueryRow(f.ctx, "SELECT id FROM device_tokens WHERE token_hash=$1", hashToken(actor.token)).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	sample := f.uuid(0x6c0)
	if status, _ := f.post(t, actor, f.body(f.uuid(0x6c1), sample, 62.5)); status != http.StatusOK {
		t.Fatalf("initial upload status=%d", status)
	}
	if err := f.store.RevokeDeviceToken(f.ctx, oldID); err != nil {
		t.Fatal(err)
	}
	plain, _, err := f.store.IssueDeviceToken(f.ctx, actor.user, "synthetic reinstalled phone")
	if err != nil {
		t.Fatal(err)
	}
	repaired := familyTestActor{user: actor.user, token: plain}
	newSource, newGeneration := familyDeviceBinding(t, f, repaired)
	if source != newSource || generation == newGeneration {
		t.Fatal("re-pairing must preserve the source and assign a new device generation")
	}
	if status, _ := f.post(t, repaired, f.body(f.uuid(0x6c2), sample, 62.5)); status != http.StatusOK {
		t.Fatalf("re-paired upload status=%d", status)
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM quantity_samples q JOIN family_record_identity r ON r.user_id=q.user_id AND r.physical_uuid=q.uuid WHERE q.user_id=$1 AND r.source_record_id=$2`, actor.user, sample).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("re-pairing duplicated the same source sample")
	}
	if status, _ := f.post(t, actor, f.body(f.uuid(0x6c3), sample, 62.5)); status != http.StatusUnauthorized {
		t.Fatalf("old revoked device status=%d", status)
	}
}

func TestIntegration_FamilyHealthSourceAmbiguityRefusesPairing(t *testing.T) {
	f := familyTestSetup(t)
	if _, err := f.admin.Exec(f.ctx, `INSERT INTO family_source_connections(user_id,provider,binding_method)
		VALUES($1,'apple_health','local_member_pairing')`, f.actors[0].user); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.IssueDeviceToken(f.ctx, f.actors[0].user, "ambiguous synthetic pairing"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal("ambiguous source pairing was not refused")
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM device_tokens WHERE user_id=$1", f.actors[0].user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("failed ambiguous pairing left a credential")
	}
}

func TestIntegration_FamilyHealthSourceBindingCannotCrossMember(t *testing.T) {
	f := familyTestSetup(t)
	sourceB, _ := familyDeviceBinding(t, f, f.actors[1])
	if _, err := f.admin.Exec(f.ctx, "UPDATE device_tokens SET source_connection_id=$1 WHERE user_id=$2", sourceB, f.actors[0].user); err == nil {
		t.Fatal("database allowed a cross-member source binding")
	}
}

func TestIntegration_FamilyHealthGenerationCannotChangeSource(t *testing.T) {
	f := familyTestSetup(t)
	var other string
	if err := f.admin.QueryRow(f.ctx, `INSERT INTO family_source_connections(user_id,provider,binding_method)
		VALUES($1,'apple_health','local_member_pairing') RETURNING id::text`, f.actors[0].user).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(f.ctx, "UPDATE device_tokens SET source_connection_id=$1 WHERE user_id=$2", other, f.actors[0].user); err == nil {
		t.Fatal("database allowed an existing device generation to change source")
	}
}

func TestIntegration_FamilyHealthRevokedSourceCannotWrite(t *testing.T) {
	f := familyTestSetup(t)
	source, _ := familyDeviceBinding(t, f, f.actors[0])
	if _, err := f.admin.Exec(f.ctx, "UPDATE family_source_connections SET status='revoked' WHERE id=$1", source); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.post(t, f.actors[0], f.body(f.uuid(0x6d0), f.uuid(0x6d1), 62.5)); status != http.StatusConflict {
		t.Fatalf("revoked source status=%d", status)
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM batches WHERE user_id=$1", f.actors[0].user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("revoked source left a committed batch")
	}
}

func TestIntegration_FamilyHealthSourceLockDoesNotGrantOtherMembers(t *testing.T) {
	f := familyTestSetup(t)
	source, _ := familyDeviceBinding(t, f, f.actors[0])
	for i, actor := range f.actors {
		var allowed bool
		if err := f.store.pool.QueryRow(f.ctx, "SELECT family_apple_source_active($1,$2)", actor.user, source).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed != (i == 0) {
			t.Fatal("source lock helper expanded source ownership")
		}
	}
	for _, role := range []string{"api_reader", "web_app", "grafana"} {
		var allowed bool
		if err := f.admin.QueryRow(f.ctx, "SELECT has_function_privilege($1, 'family_apple_source_active(uuid,uuid)', 'EXECUTE')", role).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Errorf("source locking function unexpectedly executable by %s", role)
		}
	}
}

func TestIntegration_FamilyHealthBatchForeignKeyBindsCredentialOwner(t *testing.T) {
	f := familyTestSetup(t)
	var tokenID, generation int64
	if err := f.admin.QueryRow(f.ctx, "SELECT id, device_generation FROM device_tokens WHERE user_id=$1", f.actors[1].user).Scan(&tokenID, &generation); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(f.ctx, `INSERT INTO batches(batch_id,user_id,device_token_id,device_generation)
		VALUES($1,$2,$3,$4)`, f.uuid(0x6f0), f.actors[0].user, tokenID, generation); err == nil {
		t.Fatal("batch foreign key accepted another member's credential")
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM batches WHERE user_id=$1", f.actors[0].user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("invalid credential owner left a batch")
	}
}
