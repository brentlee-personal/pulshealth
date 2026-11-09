package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

const familyAggregateQuery = `{"kind":"aggregate","type":"HKQuantityTypeIdentifierHeartRate","func":"average","intervalValue":1,"intervalUnit":"day","deviceFilter":"all"}`

func familySnapshotObservations(t *testing.T, f *familyTestFixture, actor familyTestActor) []string {
	t.Helper()
	var ids []string
	for _, query := range []string{familyAggregateQuery, `{"kind":"activity_summary"}`, `{"kind":"profile"}`} {
		status, id := beginFamilyObservation(t, f, actor, query)
		if status != http.StatusOK || !isUUID(id) {
			t.Fatalf("snapshot query status=%d", status)
		}
		ids = append(ids, id)
	}
	return ids
}

func familySnapshotBatch(t *testing.T, batchID string, observations, aggregates, summaries []string, profile string) string {
	t.Helper()
	header := BatchHeader{BatchID: batchID, DeviceID: "synthetic snapshots", Type: "mixed", Reason: "manual",
		ExportedAt: 1718000000000, AggregateCount: len(aggregates), ActivitySummaryCount: len(summaries), SourceObservations: observations}
	if profile != "" {
		header.ProfileCount = 1
	}
	data, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{string(data)}
	lines = append(lines, aggregates...)
	lines = append(lines, summaries...)
	if profile != "" {
		lines = append(lines, profile)
	}
	return strings.Join(lines, "\n") + "\n"
}

func familyAggregateLine(start int64, value string) string {
	return fmt.Sprintf(`{"aggregate":{"type":"HKQuantityTypeIdentifierHeartRate","func":"average","intervalValue":1,"intervalUnit":"day","deviceFilter":"all","bucketStart":%d,"bucketEnd":%d,"value":%s,"unit":"count/min"}}`, start, start+86400000, value)
}

func familyCompleteSnapshot(t *testing.T, f *familyTestFixture, actor familyTestActor, slot int, value int) string {
	ids := familySnapshotObservations(t, f, actor)
	return familySnapshotBatch(t, f.uuid(slot), ids,
		[]string{familyAggregateLine(1767225600000, fmt.Sprint(value)), familyAggregateLine(1767312000000, "null")},
		[]string{fmt.Sprintf(`{"activitySummary":{"date":1767225600000,"localDate":"2026-01-01","moveKcal":%d,"standHours":8}}`, value)},
		fmt.Sprintf(`{"profile":{"name":"synthetic-source-%d","email":"source-%d@synthetic.invalid","dateOfBirth":315705600000,"biologicalSex":"other","capturedAt":1767225600000}}`, value, value))
}

func TestIntegration_FamilyHealthSnapshotsSeparateMembersAndSources(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	if _, err := f.admin.Exec(f.ctx, `UPDATE users SET name='synthetic account identity',email='account@synthetic.invalid' WHERE id=$1`, actor.user); err != nil {
		t.Fatal(err)
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
		VALUES($1,$2,$3,'explicit synthetic snapshot source',$4)`, hashToken(plain), tokenPrefix(plain), actor.user, otherSource); err != nil {
		t.Fatal(err)
	}
	actors := []familyTestActor{actor, {user: actor.user, token: plain}, f.actors[1]}
	for i, owner := range actors {
		value := 61 + i*10
		body := familyCompleteSnapshot(t, f, owner, 0x840+i, value)
		status, result := f.post(t, owner, body)
		if status != http.StatusOK {
			t.Fatalf("source %d snapshot status=%d", i, status)
		}
		receipt := familyWireReceipt(t, result)
		if receipt.RecordCount != 4 || receipt.Accepted != 4 || receipt.Duplicate != 0 {
			t.Fatal("snapshot receipt did not count four input lines")
		}
		source, _ := familyDeviceBinding(t, f, owner)
		var stored float64
		if err := f.admin.QueryRow(f.ctx, `SELECT value FROM aggregate_samples WHERE user_id=$1 AND source_connection_id=$2
			AND bucket_start=to_timestamp(1767225600)`, owner.user, source).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != float64(value) {
			t.Fatal("aggregate source identity collided")
		}
		if err := f.admin.QueryRow(f.ctx, `SELECT move_kcal FROM activity_summaries WHERE user_id=$1 AND source_connection_id=$2 AND date='2026-01-01'`, owner.user, source).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != float64(value) {
			t.Fatal("activity source identity collided")
		}
		var name string
		if err := f.admin.QueryRow(f.ctx, `SELECT name FROM family_source_profiles WHERE user_id=$1 AND source_connection_id=$2`, owner.user, source).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != fmt.Sprintf("synthetic-source-%d", value) {
			t.Fatal("profile source identity collided")
		}
		if status, replay := f.post(t, owner, body); status != http.StatusOK || !reflect.DeepEqual(result, replay) {
			t.Fatal("snapshot exact replay changed its committed receipt")
		}
	}
	var accountName, accountEmail string
	if err := f.admin.QueryRow(f.ctx, `SELECT name,email FROM users WHERE id=$1`, actor.user).Scan(&accountName, &accountEmail); err != nil {
		t.Fatal(err)
	}
	if accountName != "synthetic account identity" || accountEmail != "account@synthetic.invalid" {
		t.Fatal("source profile changed the account's login identity")
	}
}

func TestIntegration_FamilyHealthNewSnapshotReplacesWholeValues(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	if status, _ := f.post(t, actor, familyCompleteSnapshot(t, f, actor, 0x850, 61)); status != http.StatusOK {
		t.Fatalf("initial snapshot status=%d", status)
	}
	ids := familySnapshotObservations(t, f, actor)
	body := familySnapshotBatch(t, f.uuid(0x851), ids,
		[]string{familyAggregateLine(1767225600000, "72"), familyAggregateLine(1767312000000, "null")},
		[]string{`{"activitySummary":{"date":1767225600000,"localDate":"2026-01-01","moveKcal":72}}`},
		`{"profile":{"capturedAt":1767225601000}}`)
	if status, _ := f.post(t, actor, body); status != http.StatusOK {
		t.Fatalf("replacement status=%d", status)
	}
	source, _ := familyDeviceBinding(t, f, actor)
	var valid bool
	if err := f.admin.QueryRow(f.ctx, `SELECT name IS NULL AND email IS NULL AND dob_epoch_ms IS NULL AND biological_sex IS NULL
		FROM family_source_profiles WHERE user_id=$1 AND source_connection_id=$2`, actor.user, source).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("empty source profile merged previous values")
	}
	if err := f.admin.QueryRow(f.ctx, `SELECT move_kcal=72 AND stand_hours IS NULL AND exercise_min IS NULL
		FROM activity_summaries WHERE user_id=$1 AND source_connection_id=$2`, actor.user, source).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("omitted activity rings retained old values")
	}
	if err := f.admin.QueryRow(f.ctx, `SELECT value IS NULL FROM aggregate_samples WHERE user_id=$1 AND source_connection_id=$2
		AND bucket_start=to_timestamp(1767312000)`, actor.user, source).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("explicit null aggregate was filled or ignored")
	}
	var instant, revision string
	if err := f.admin.QueryRow(f.ctx, `SELECT source_facts->>'start_at',source_facts->'source_revision'->>'status'
		FROM family_snapshot_identity WHERE user_id=$1 AND record_type='puls.v1.profile'`, actor.user).Scan(&instant, &revision); err != nil {
		t.Fatal(err)
	}
	if instant != "2026-01-01T00:00:01Z" || revision != "unavailable" {
		t.Fatal("profile time or revision was replaced by export time or query order")
	}
	duplicate := strings.Replace(body, f.uuid(0x851), f.uuid(0x852), 1)
	status, result := f.post(t, actor, duplicate)
	if status != http.StatusOK {
		t.Fatalf("duplicate snapshot status=%d", status)
	}
	receipt := familyWireReceipt(t, result)
	if receipt.RecordCount != 4 || receipt.Duplicate != 4 || receipt.Accepted != 0 {
		t.Fatal("duplicate snapshot outcomes do not cover business lines")
	}
}

func TestIntegration_FamilyHealthStaleSnapshotCannotOverwriteNewerQuery(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	_, oldID := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`)
	original := familySnapshotBatch(t, f.uuid(0x860), []string{oldID}, nil, nil, `{"profile":{"name":"first","capturedAt":1767225600000}}`)
	status, originalReceipt := f.post(t, actor, original)
	if status != http.StatusOK {
		t.Fatalf("initial profile status=%d", status)
	}
	_, latest := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`)
	stale := familySnapshotBatch(t, f.uuid(0x861), []string{oldID}, nil, nil, `{"profile":{"name":"late-old-query","capturedAt":1767225601000}}`)
	if status, _ := f.post(t, actor, stale); status != http.StatusConflict {
		t.Fatalf("stale query status=%d", status)
	}
	current := familySnapshotBatch(t, f.uuid(0x862), []string{latest}, nil, nil, `{"profile":{"name":"latest-query","capturedAt":1767225601000}}`)
	if status, _ := f.post(t, actor, current); status != http.StatusOK {
		t.Fatalf("new query status=%d", status)
	}
	if status, replay := f.post(t, actor, original); status != http.StatusOK || !reflect.DeepEqual(originalReceipt, replay) {
		t.Fatal("superseded observation changed original replay acknowledgement")
	}
	var name string
	if err := f.admin.QueryRow(f.ctx, `SELECT name FROM family_source_profiles WHERE user_id=$1`, actor.user).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "latest-query" {
		t.Fatal("late or cached upload overwrote the newer source snapshot")
	}
	changed := strings.Replace(current, f.uuid(0x862), f.uuid(0x863), 1)
	changed = strings.Replace(changed, "latest-query", "changed-without-reading", 1)
	if status, _ := f.post(t, actor, changed); status != http.StatusConflict {
		t.Fatalf("changed object reused one observation status=%d", status)
	}
}

func TestIntegration_FamilyHealthUnknownSnapshotReadDoesNotClearHistory(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	if status, _ := f.post(t, actor, familyCompleteSnapshot(t, f, actor, 0x870, 61)); status != http.StatusOK {
		t.Fatalf("initial snapshot status=%d", status)
	}
	ids := familySnapshotObservations(t, f, actor)
	for i, body := range []string{
		familySnapshotBatch(t, f.uuid(0x871), ids, []string{familyAggregateLine(1767225600000, "null")}, nil, ""),
		familySnapshotBatch(t, f.uuid(0x872), ids, nil, []string{`{"activitySummary":{"date":1767225600000,"localDate":"2026-01-01"}}`}, ""),
		familySnapshotBatch(t, f.uuid(0x873), ids, nil, nil, `{"profile":{"name":"missing-source-time"}}`),
	} {
		if status, _ := f.post(t, actor, body); status != http.StatusConflict {
			t.Fatalf("unknown snapshot %d status=%d", i, status)
		}
	}
	var value float64
	if err := f.admin.QueryRow(f.ctx, `SELECT value FROM aggregate_samples WHERE user_id=$1 AND bucket_start=to_timestamp(1767225600)`, actor.user).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 61 {
		t.Fatal("unknown read cleared known aggregate history")
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM family_batch_receipts WHERE user_id=$1`, actor.user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("refused snapshot left a committed receipt")
	}
}

func TestIntegration_FamilyHealthSnapshotRequiresCurrentCredentialScope(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	_, foreign := beginFamilyObservation(t, f, f.actors[1], `{"kind":"profile"}`)
	_, wrongKind := beginFamilyObservation(t, f, actor, `{"kind":"activity_summary"}`)
	_, expired := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`)
	if _, err := f.admin.Exec(f.ctx, `UPDATE family_source_observations SET issued_at=clock_timestamp()-interval '10 minutes',expires_at=clock_timestamp()-interval '5 minutes' WHERE observation_id=$1`, expired); err != nil {
		t.Fatal(err)
	}
	profile := `{"profile":{"name":"refused","capturedAt":1767225600000}}`
	for i, ids := range [][]string{nil, {foreign}, {wrongKind}, {expired}} {
		body := familySnapshotBatch(t, f.uuid(0x880+i), ids, nil, nil, profile)
		if status, _ := f.post(t, actor, body); status != http.StatusConflict {
			t.Fatalf("unverified snapshot scope %d status=%d", i, status)
		}
	}
	_, latest := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`)
	var secondSource string
	if err := f.admin.QueryRow(f.ctx, `INSERT INTO family_source_connections(user_id,provider,binding_method) VALUES($1,'apple_health','local_member_pairing') RETURNING id::text`, actor.user).Scan(&secondSource); err != nil {
		t.Fatal(err)
	}
	plain, err := newDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(f.ctx, `INSERT INTO device_tokens(token_hash,token_prefix,user_id,name,source_connection_id) VALUES($1,$2,$3,'synthetic foreign snapshot source',$4)`, hashToken(plain), tokenPrefix(plain), actor.user, secondSource); err != nil {
		t.Fatal(err)
	}
	other := familyTestActor{user: actor.user, token: plain}
	if status, _ := f.post(t, other, familySnapshotBatch(t, f.uuid(0x885), []string{latest}, nil, nil, profile)); status != http.StatusConflict {
		t.Fatalf("another source reused observation status=%d", status)
	}
	// Re-pairing the same source still requires a fresh read by the new credential.
	plain, err = newDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	source, _ := familyDeviceBinding(t, f, actor)
	if _, err := f.store.pool.Exec(f.ctx, `INSERT INTO device_tokens(token_hash,token_prefix,user_id,name,source_connection_id) VALUES($1,$2,$3,'synthetic replacement credential',$4)`, hashToken(plain), tokenPrefix(plain), actor.user, source); err != nil {
		t.Fatal(err)
	}
	replacement := familyTestActor{user: actor.user, token: plain}
	if status, _ := f.post(t, replacement, familySnapshotBatch(t, f.uuid(0x886), []string{latest}, nil, nil, profile)); status != http.StatusConflict {
		t.Fatalf("replacement credential reused old read status=%d", status)
	}
	_, fresh := beginFamilyObservation(t, f, replacement, `{"kind":"profile"}`)
	if status, _ := f.post(t, replacement, familySnapshotBatch(t, f.uuid(0x887), []string{fresh}, nil, nil, profile)); status != http.StatusOK {
		t.Fatalf("replacement fresh read status=%d", status)
	}
}

func TestIntegration_FamilyHealthSnapshotLastLineWinsAndMixedFailureRollsBack(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	ids := familySnapshotObservations(t, f, actor)
	body := familySnapshotBatch(t, f.uuid(0x890), ids, []string{familyAggregateLine(1767225600000, "61"), familyAggregateLine(1767225600000, "72")}, nil, "")
	status, result := f.post(t, actor, body)
	if status != http.StatusOK {
		t.Fatalf("same-batch replacement status=%d", status)
	}
	receipt := familyWireReceipt(t, result)
	if receipt.RecordCount != 2 || receipt.Accepted != 2 {
		t.Fatal("last-line-wins receipt lost input business lines")
	}
	var count int
	var value float64
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*),max(value) FROM aggregate_samples WHERE user_id=$1`, actor.user).Scan(&count, &value); err != nil {
		t.Fatal(err)
	}
	if count != 1 || value != 72 {
		t.Fatal("duplicate bucket did not keep exactly its last value")
	}
	beginFamilyObservation(t, f, actor, familyAggregateQuery)
	failed := familySnapshotBatch(t, f.uuid(0x891), ids, []string{familyAggregateLine(1767225600000, "99")}, nil, "")
	var header map[string]any
	lines := strings.Split(failed, "\n")
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	header["sampleCount"] = 1
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	failed = string(encoded) + "\n" + strings.Split(f.body(f.uuid(0x891), f.uuid(0x892), 80), "\n")[1] + "\n" + strings.Join(lines[1:], "\n")
	if status, _ := f.post(t, actor, failed); status != http.StatusConflict {
		t.Fatalf("mixed stale snapshot status=%d", status)
	}
	for table, expected := range map[string]int{"quantity_samples": 0, "family_record_identity": 0, "aggregate_samples": 1, "family_snapshot_identity": 1, "family_batch_receipts": 1, "batches": 1} {
		if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1", actor.user).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != expected {
			t.Fatalf("failed mixed snapshot left partial data in %s", table)
		}
	}
	if err := f.admin.QueryRow(f.ctx, `SELECT value FROM aggregate_samples WHERE user_id=$1`, actor.user).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 72 {
		t.Fatal("failed mixed snapshot modified prior value")
	}
}

func TestIntegration_FamilyHealthSnapshotDoesNotAdoptUnknownLegacySource(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	if _, err := f.admin.Exec(f.ctx, `INSERT INTO activity_summaries(user_id,date,move_kcal) VALUES($1,'2026-01-01',25)`, actor.user); err != nil {
		t.Fatal(err)
	}
	_, id := beginFamilyObservation(t, f, actor, `{"kind":"activity_summary"}`)
	// The offset is known but localDate was not supplied: the approved fallback is UTC.
	summary := `{"activitySummary":{"date":1767225600000,"temporalContext":{"timeZoneID":"America/Los_Angeles","utcOffsetSeconds":-28800,"source":"device_current","confidence":"inferred"},"moveKcal":61}}`
	if status, _ := f.post(t, actor, familySnapshotBatch(t, f.uuid(0x8a0), []string{id}, nil, []string{summary}, "")); status != http.StatusOK {
		t.Fatalf("UTC fallback snapshot status=%d", status)
	}
	var old, fresh float64
	if err := f.admin.QueryRow(f.ctx, `SELECT max(move_kcal) FILTER(WHERE source_connection_id IS NULL),max(move_kcal) FILTER(WHERE source_connection_id IS NOT NULL) FROM activity_summaries WHERE user_id=$1 AND date='2026-01-01'`, actor.user).Scan(&old, &fresh); err != nil {
		t.Fatal(err)
	}
	if old != 25 || fresh != 61 {
		t.Fatal("personal snapshot adopted or overwrote an unknown legacy source")
	}
	var state string
	if err := f.admin.QueryRow(f.ctx, `SELECT source_facts->'source_local_date'->>'status' FROM family_snapshot_identity WHERE user_id=$1`, actor.user).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unavailable" {
		t.Fatal("UTC fallback invented a recorded source day")
	}
}

func TestIntegration_FamilyHealthSnapshotCommitOrdersConcurrentQuery(t *testing.T) {
	f := familyTestSetup(t)
	actor := f.actors[0]
	_, old := beginFamilyObservation(t, f, actor, `{"kind":"profile"}`)
	lock, err := f.admin.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(f.ctx)
	if _, err := lock.Exec(f.ctx, `LOCK TABLE family_source_profiles IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	type response struct {
		status int
		data   []byte
		err    error
	}
	request := func(path, body string, result chan<- response) {
		req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.server.URL+path, strings.NewReader(body))
		if err != nil {
			result <- response{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+actor.token)
		req.Header.Set("Content-Type", "application/json")
		r, err := f.server.Client().Do(req)
		if err != nil {
			result <- response{err: err}
			return
		}
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		result <- response{status: r.StatusCode, data: data, err: err}
	}
	upload, query := make(chan response, 1), make(chan response, 1)
	body := familySnapshotBatch(t, f.uuid(0x8b0), []string{old}, nil, nil, `{"profile":{"name":"before-next-query","capturedAt":1767225600000}}`)
	go request("/v1/batches", body, upload)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var uploadPID int
	for uploadPID == 0 {
		if err := f.admin.QueryRow(f.ctx, `SELECT coalesce(max(pid),0) FROM pg_locks WHERE relation='family_source_profiles'::regclass AND mode='RowExclusiveLock' AND NOT granted`).Scan(&uploadPID); err != nil {
			t.Fatal(err)
		}
		if uploadPID == 0 {
			select {
			case <-tick.C:
			case <-f.ctx.Done():
				t.Fatal("upload did not reach the real profile lock")
			}
		}
	}
	go request("/v1/source-observations", `{"kind":"profile"}`, query)
	for {
		var waiting bool
		if err := f.admin.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, uploadPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-tick.C:
		case <-f.ctx.Done():
			t.Fatal("new source query was not ordered behind the active upload")
		}
	}
	if err := lock.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	first, second := <-upload, <-query
	if first.err != nil || second.err != nil || first.status != http.StatusOK || second.status != http.StatusOK {
		t.Fatalf("ordered upload/query %d/%v %d/%v", first.status, first.err, second.status, second.err)
	}
	var result struct {
		ID string `json:"observation_id"`
	}
	if err := json.Unmarshal(second.data, &result); err != nil {
		t.Fatal(err)
	}
	if !isUUID(result.ID) || result.ID == old {
		t.Fatal("completed query did not replace its old observation")
	}
	late := strings.Replace(body, f.uuid(0x8b0), f.uuid(0x8b1), 1)
	if status, _ := f.post(t, actor, late); status != http.StatusConflict {
		t.Fatalf("late upload using old query status=%d", status)
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM family_batch_receipts WHERE user_id=$1`, actor.user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("ordered query produced an extra committed upload")
	}
}
