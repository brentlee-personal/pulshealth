package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func beginFamilyObservation(t *testing.T, f *familyTestFixture, actor familyTestActor, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.server.URL+"/v1/source-observations", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+actor.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result struct {
		ID string `json:"observation_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, result.ID
}

func TestIntegration_FamilyHealthSourceObservationIsMemberBound(t *testing.T) {
	f := familyTestSetup(t)
	ids := map[string]bool{}
	for _, actor := range f.actors {
		status, id := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`)
		if status != http.StatusOK || !isUUID(id) || ids[id] {
			t.Fatalf("observation status=%d; invalid or reused identity", status)
		}
		ids[id] = true
		source, _ := familyDeviceBinding(t, f, actor)
		var valid bool
		if err := f.admin.QueryRow(f.ctx, `SELECT o.user_id=$1 AND o.source_connection_id=$2
            AND o.credential_id=d.credential_id AND o.sequence_number=1
            AND o.expires_at-o.issued_at BETWEEN interval '299 seconds' AND interval '301 seconds'
            FROM family_source_observations o JOIN device_tokens d ON d.user_id=o.user_id AND d.token_hash=$3
            WHERE o.observation_id=$4`, actor.user, source, hashToken(actor.token), id).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Fatal("observation did not bind verified member, source, credential, sequence and deadline")
		}
	}
}

func TestIntegration_FamilyHealthObservationReplacesOnlyItsQueryStream(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	firstStatus, first := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`)
	secondStatus, second := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`)
	otherStatus, other := beginFamilyObservation(t, f, actor, `{"kind":"activity_summary"}`)
	if firstStatus != 200 || secondStatus != 200 || otherStatus != 200 || first == second || second == other {
		t.Fatal("query observations did not replace their own stream")
	}
	var count, old int
	var sequence int64
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*),count(*) FILTER(WHERE observation_id=$2)
        FROM family_source_observations WHERE user_id=$1`, actor.user, first).Scan(&count, &old); err != nil {
		t.Fatal(err)
	}
	if count != 2 || old != 0 {
		t.Fatal("old observations accumulated or a different query stream was overwritten")
	}
	if err := f.admin.QueryRow(f.ctx, `SELECT sequence_number FROM family_source_observations
        WHERE user_id=$1 AND observation_id=$2`, actor.user, second).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if sequence != 2 {
		t.Fatal("query order did not advance independently of arrival timestamps")
	}
}

func TestIntegration_FamilyHealthObservationRejectsInvalidQueries(t *testing.T) {
	f := familyTestSetup(t)
	for _, body := range []string{
		`{"kind":"unknown"}`, `{"kind":"profile","user_id":"someone-else"}`,
		`{"kind":"profile","kind":"activity_summary"}`,
		`{"kind":"profile","type":"HKQuantityTypeIdentifierHeartRate"}`,
		`{"kind":"profile"}{"kind":"profile"}`,
		`{"kind":"aggregate","type":"unknown","func":"average","intervalValue":1,"intervalUnit":"day","deviceFilter":"all"}`,
		`{"kind":"aggregate","type":"HKQuantityTypeIdentifierHeartRate","func":"not-a-function","intervalValue":1,"intervalUnit":"day","deviceFilter":"all"}`,
		`{"kind":"aggregate","type":"HKQuantityTypeIdentifierHeartRate","func":"average","intervalValue":2147483648,"intervalUnit":"day","deviceFilter":"all"}`,
	} {
		if status, _ := beginFamilyObservation(t, f, f.actors[0], body); status != http.StatusBadRequest {
			t.Fatalf("invalid query status=%d", status)
		}
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM family_source_observations WHERE user_id=$1`, f.actors[0].user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("invalid queries left committed observations")
	}
	if status, _ := beginFamilyObservation(t, f, f.actors[0], `{"kind":"profile"}`+strings.Repeat(" ", 4096)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized observation query status=%d", status)
	}
	if status, id := beginFamilyObservation(t, f, f.actors[0], `{"kind":"aggregate","type":"HKQuantityTypeIdentifierHeartRate","func":"average","intervalValue":1,"intervalUnit":"day","deviceFilter":"all"}`); status != http.StatusOK || !isUUID(id) {
		t.Fatalf("valid aggregate query status=%d", status)
	}
}

func TestIntegration_FamilyHealthObservationRechecksSourceAndCredential(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	source, _ := familyDeviceBinding(t, f, actor)
	if _, err := f.admin.Exec(f.ctx, `UPDATE family_source_connections SET status='revoked' WHERE user_id=$1 AND id=$2`, actor.user, source); err != nil {
		t.Fatal(err)
	}
	if status, _ := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`); status != http.StatusConflict {
		t.Fatalf("revoked source observation status=%d", status)
	}
	if _, err := f.admin.Exec(f.ctx, `UPDATE device_tokens SET status='revoked' WHERE user_id=$1`, f.actors[1].user); err != nil {
		t.Fatal(err)
	}
	if status, _ := beginFamilyObservation(t, f, f.actors[1], `{"kind":"profile"}`); status != http.StatusUnauthorized {
		t.Fatalf("revoked credential observation status=%d", status)
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM family_source_observations WHERE user_id=ANY($1::uuid[])`, []string{actor.user, f.actors[1].user}).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("revoked source or credential created an observation")
	}
}
