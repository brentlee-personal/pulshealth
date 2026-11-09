package main

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIntegration_FamilyHealthRecordConflictRollsBackWholeBatch(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	sample, fresh := f.uuid(0x710), f.uuid(0x711)
	if status, _ := f.post(t, actor, f.body(f.uuid(0x712), sample, 62.5)); status != http.StatusOK {
		t.Fatalf("initial status=%d", status)
	}
	body := strings.Replace(f.body(f.uuid(0x713), fresh, 72.5), `"sampleCount":1`, `"sampleCount":2`, 1)
	body += strings.Split(f.body(f.uuid(0x714), sample, 82.5), "\n")[1] + "\n"
	if status, _ := f.post(t, actor, body); status != http.StatusConflict {
		t.Errorf("changed immutable source record status=%d, want 409", status)
	}
	var count int
	var value float64
	if err := f.admin.QueryRow(f.ctx, "SELECT count(*), min(value) FROM quantity_samples WHERE user_id=$1", actor.user).Scan(&count, &value); err != nil {
		t.Fatal(err)
	}
	if count != 1 || value != 62.5 {
		t.Error("record conflict did not roll back the entire new batch")
	}
}

func TestIntegration_FamilyHealthRecordIdentityCrossesTimePartitions(t *testing.T) {
	f := familyTestSetup(t)
	sample := f.uuid(0x720)
	if status, _ := f.post(t, f.actors[0], f.body(f.uuid(0x721), sample, 62.5)); status != http.StatusOK {
		t.Fatalf("initial status=%d", status)
	}
	body := f.body(f.uuid(0x722), sample, 62.5)
	body = strings.Replace(body, `"start":1718000000000`, `"start":1720678400000`, 1)
	body = strings.Replace(body, `"end":1718000005000`, `"end":1720678405000`, 1)
	if status, _ := f.post(t, f.actors[0], body); status != http.StatusConflict {
		t.Errorf("same source record moved to another partition status=%d, want 409", status)
	}
}

func TestIntegration_FamilyHealthConflictingRecordsInsideOneBatch(t *testing.T) {
	f := familyTestSetup(t)
	sample := f.uuid(0x730)
	body := strings.Replace(f.body(f.uuid(0x731), sample, 62.5), `"sampleCount":1`, `"sampleCount":2`, 1)
	body += strings.Split(f.body(f.uuid(0x732), sample, 72.5), "\n")[1] + "\n"
	if status, _ := f.post(t, f.actors[0], body); status != http.StatusConflict {
		t.Errorf("conflicting records inside one batch status=%d, want 409", status)
	}
	for _, table := range []string{"quantity_samples", "batches", "family_batch_receipts", "family_record_identity"} {
		var count int
		if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1", f.actors[0].user).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("conflicting batch left %d rows in %s", count, table)
		}
	}
}

func TestIntegration_FamilyHealthSameRecordIDAcrossSources(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
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
		VALUES($1,$2,$3,'explicit synthetic source pairing',$4)`, hashToken(plain), tokenPrefix(plain), actor.user, otherSource); err != nil {
		t.Fatal(err)
	}
	sample := f.uuid(0x740)
	for i, credential := range []familyTestActor{actor, {user: actor.user, token: plain}} {
		if status, _ := f.post(t, credential, f.body(f.uuid(0x741+i), sample, 62.5+float64(i)*10)); status != http.StatusOK {
			t.Fatalf("source %d status=%d", i, status)
		}
	}
	var values []float64
	if err := f.admin.QueryRow(f.ctx, "SELECT array_agg(value ORDER BY value) FROM quantity_samples WHERE user_id=$1", actor.user).Scan(&values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, []float64{62.5, 72.5}) {
		t.Error("independent sources did not preserve both records with the same raw ID")
	}
}

func TestIntegration_FamilyHealthBadUnitCannotChangeSharedCatalog(t *testing.T) {
	f := familyTestSetup(t)
	if status, _ := f.post(t, f.actors[0], f.body(f.uuid(0x750), f.uuid(0x751), 62.5)); status != http.StatusOK {
		t.Fatalf("valid control status=%d", status)
	}
	t.Cleanup(func() {
		if _, err := f.admin.Exec(f.ctx, "UPDATE sample_types SET unit='count/min' WHERE identifier='HKQuantityTypeIdentifierHeartRate'"); err != nil {
			t.Errorf("restore synthetic catalog state: %v", err)
		}
	})
	body := strings.Replace(f.body(f.uuid(0x752), f.uuid(0x753), 72.5), `"unit":"count/min"`, `"unit":"kg"`, 1)
	if status, _ := f.post(t, f.actors[1], body); status != http.StatusBadRequest {
		t.Errorf("wrong canonical unit status=%d, want 400", status)
	}
	var unit string
	if err := f.admin.QueryRow(f.ctx, "SELECT unit FROM sample_types WHERE identifier='HKQuantityTypeIdentifierHeartRate'").Scan(&unit); err != nil {
		t.Fatal(err)
	}
	if unit != "count/min" {
		t.Error("one member's invalid unit changed the shared catalog")
	}
}

func TestIntegration_FamilyHealthExplicitZoneOffsetMustAgree(t *testing.T) {
	f := familyTestSetup(t)
	body := f.body(f.uuid(0x760), f.uuid(0x761), 62.5)
	body = strings.Replace(body, `"unit":"count/min"`, `"unit":"count/min","startContext":{"timeZoneID":"Etc/UTC","utcOffsetSeconds":3600,"source":"healthkit_metadata","confidence":"recorded"}`, 1)
	if status, _ := f.post(t, f.actors[0], body); status != http.StatusBadRequest {
		t.Errorf("contradictory explicit source time facts status=%d, want 400", status)
	}
}

func TestIntegration_FamilyHealthCanonicalJSONBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		status      int
	}{
		{"duplicate key", `62.5,"value":72.5`, http.StatusBadRequest},
		{"lone surrogate", `62.5,"future":"\ud800"`, http.StatusBadRequest},
		{"invalid UTF8", "62.5,\"future\":\"" + string([]byte{0xff}) + "\"", http.StatusBadRequest},
		{"infinite unknown number", `62.5,"future":1e999`, http.StatusBadRequest},
		{"valid unknown field", `62.5,"future":{"allowed":[true,"valid"]}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := familyTestSetup(t)
			body := strings.Replace(f.body(f.uuid(0x770), f.uuid(0x771), 62.5), `"value":62.5`, `"value":`+tc.value, 1)
			if status, _ := f.post(t, f.actors[0], body); status != tc.status {
				t.Errorf("canonical JSON boundary status=%d, want %d", status, tc.status)
			}
		})
	}
}

func TestIntegration_FamilyHealthRequiredWorkoutFields(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		status       int
	}{
		{"missing duration", `{"activityType":"running"}`, http.StatusBadRequest},
		{"missing activity duration", `{"activityType":"running","duration":60,"activities":[{"activityType":"running","start":1718000000000}]}`, http.StatusBadRequest},
		{"explicit zero duration", `{"activityType":"running","duration":0}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := familyTestSetup(t)
			body := strings.Split(f.body(f.uuid(0x780), f.uuid(0x781), 62.5), "\n")[0] + "\n"
			body += `{"uuid":"` + f.uuid(0x781) + `","type":"HKWorkoutTypeIdentifier","kind":"workout","start":1718000000000,"end":1718000005000,"workout":` + tc.detail + "}\n"
			if status, _ := f.post(t, f.actors[0], body); status != tc.status {
				t.Errorf("required source field status=%d, want %d", status, tc.status)
			}
		})
	}
}

func TestIntegration_FamilyHealthRecordIndexPreservesFacts(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	rawID := strings.ToUpper(f.uuid(0x7a0))
	if status, _ := f.post(t, actor, f.body(f.uuid(0x7a1), rawID, 62.5)); status != http.StatusOK {
		t.Fatalf("initial status=%d", status)
	}
	plain, _, err := f.store.IssueDeviceToken(f.ctx, actor.user, "synthetic re-pair")
	if err != nil {
		t.Fatal(err)
	}
	if status, body := f.post(t, familyTestActor{user: actor.user, token: plain}, f.body(f.uuid(0x7a2), strings.ToLower(rawID), 62.5)); status != http.StatusOK || string(body["duplicates"]) != "1" {
		t.Fatalf("same source re-pair status=%d, duplicates=%s", status, body["duplicates"])
	}
	var canonical, original, revision string
	var digestBytes, records int
	var noValue bool
	if err := f.admin.QueryRow(f.ctx, `SELECT source_record_id,raw_source_record_id,
		octet_length(payload_digest),source_facts->'source_revision'->>'status',
		NOT (source_facts ? 'value' OR source_facts ? 'original_value'),
		(SELECT count(*) FROM family_record_identity WHERE user_id=$1)
		FROM family_record_identity WHERE user_id=$1`, actor.user).
		Scan(&canonical, &original, &digestBytes, &revision, &noValue, &records); err != nil {
		t.Fatal(err)
	}
	if canonical != strings.ToLower(rawID) || original != rawID || digestBytes != 32 || revision != "unavailable" || !noValue || records != 1 {
		t.Fatal("record identity or source facts changed across re-pairing")
	}
	for _, query := range []string{
		"UPDATE family_record_identity SET payload_digest=sha256('rewritten') WHERE user_id=$1",
		"DELETE FROM family_record_identity WHERE user_id=$1",
	} {
		_, err := f.store.pool.Exec(f.ctx, query, actor.user)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("index mutation permission=%v, want denied", err)
		}
	}
}
