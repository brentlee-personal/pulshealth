package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// adminPool connects as a role that owns the schema, for the few setup steps
// the scoped `ingest` role is deliberately not allowed to perform: applying
// db/migrations DDL and compress_chunk (TimescaleDB requires the hypertable owner).
// ADMIN_DATABASE_URL falls back to DATABASE_URL, so running the whole suite as
// the superuser keeps working unchanged; to exercise the production role, set
// DATABASE_URL to the ingest credentials and ADMIN_DATABASE_URL to postgres.
func adminPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("ADMIN_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestIntegration_IngestRoundTrip exercises the full store path against a
// real database. It is skipped unless DATABASE_URL is set and expects the
// schema from server/db/migrations to be applied (the compose `migrate` service
// does that on `docker compose up -d`), e.g.:
//
//	docker compose up -d db
//	DATABASE_URL=postgres://postgres:$POSTGRES_PASSWORD@localhost:5432/postgres go test ./...
//
// To run as the scoped ingest role (what production connects as once opted
// in), point DATABASE_URL at it and ADMIN_DATABASE_URL at the superuser.
func TestIntegration_IngestRoundTrip(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	store := NewStore(pool)

	// Unique IDs per run so the test is rerunnable against a shared DB.
	run := time.Now().UnixNano()
	batchID := fmt.Sprintf("%08x-0000-4000-8000-%012x", run>>32, run&0xffffffffffff)
	hrUUID := fmt.Sprintf("%08x-0001-4000-8000-%012x", run>>32, run&0xffffffffffff)
	catUUID := fmt.Sprintf("%08x-0002-4000-8000-%012x", run>>32, run&0xffffffffffff)
	wkUUID := fmt.Sprintf("%08x-0003-4000-8000-%012x", run>>32, run&0xffffffffffff)

	body := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"HKQuantityTypeIdentifierHeartRate","reason":"manual","exportedAt":1718000000000,"sampleCount":3,"deletionCount":0}
{"uuid":"%s","type":"HKQuantityTypeIdentifierHeartRate","kind":"quantity","start":1718000000000,"end":1718000005000,"value":62.5,"unit":"count/min","sourceName":"Apple Watch","sourceBundleID":"com.apple.health","sourceVersion":"10.0","metadata":{"HKMetadataKeyHeartRateMotionContext":1}}
{"uuid":"%s","type":"HKCategoryTypeIdentifierSleepAnalysis","kind":"category","start":1718000000000,"end":1718003600000,"category":3,"sourceName":"Apple Watch"}
{"uuid":"%s","type":"HKWorkoutTypeIdentifier","kind":"workout","start":1718000000000,"end":1718003600500,"workout":{"activityType":"running","duration":3600.5,"totalEnergyKcal":450.2,"totalDistanceMeters":8046.7,"statistics":{"HKQuantityTypeIdentifierHeartRate":152.0}}}
`, batchID, hrUUID, catUUID, wkUUID)

	batch, err := parseReferenceBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}

	res, err := store.InsertBatch(ctx, batch, int64(len(body)))
	if err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if res.Accepted != 3 || res.Duplicates != 0 || res.Deleted != 0 || res.DuplicateBatch {
		t.Fatalf("first insert: %+v", res)
	}

	// Retried batch returns its first committed outcome without writing again.
	res2, err := store.InsertBatch(ctx, batch, int64(len(body)))
	if err != nil {
		t.Fatalf("retry InsertBatch: %v", err)
	}
	assertStoredBatchReplay(t, res, res2)

	// Deletion batch removes the quantity sample.
	delBatchID := fmt.Sprintf("%08x-0004-4000-8000-%012x", run>>32, run&0xffffffffffff)
	delBody := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"HKQuantityTypeIdentifierHeartRate","reason":"incremental","exportedAt":1718000100000,"sampleCount":0,"deletionCount":1}
{"deleted":{"uuid":"%s","type":"HKQuantityTypeIdentifierHeartRate"}}
`, delBatchID, hrUUID)
	delBatch, err := parseReferenceBatch(strings.NewReader(delBody))
	if err != nil {
		t.Fatalf("ParseBatch deletions: %v", err)
	}
	res3, err := store.InsertBatch(ctx, delBatch, int64(len(delBody)))
	if err != nil {
		t.Fatalf("InsertBatch deletions: %v", err)
	}
	if res3.Deleted != 1 {
		t.Fatalf("deletion batch: %+v", res3)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM quantity_samples WHERE uuid = $1`, hrUUID).Scan(&n); err != nil {
		t.Fatalf("verify delete: %v", err)
	}
	if n != 0 {
		t.Errorf("quantity sample still present after deletion")
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deleted_samples WHERE uuid = $1`, hrUUID).Scan(&n); err != nil {
		t.Fatalf("verify tombstone: %v", err)
	}
	if n != 1 {
		t.Errorf("tombstone missing from deleted_samples")
	}

	// Stats should include our types with sane bounds.
	stats, err := store.Stats(ctx, recordScope{userID: referenceMemberID})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	found := map[string]bool{}
	for _, s := range stats {
		found[s.Type] = true
		if s.Type == "HKWorkoutTypeIdentifier" && s.Rows < 1 {
			t.Errorf("workout stats rows = %d", s.Rows)
		}
	}
	for _, want := range []string{"HKCategoryTypeIdentifierSleepAnalysis", "HKWorkoutTypeIdentifier"} {
		if !found[want] {
			t.Errorf("stats missing type %s (got %v)", want, stats)
		}
	}
}

// TestIntegration_CategoryLabelsJoin verifies that raw HKCategorySample values
// can be joined to their HealthKit labels without changing category_samples.
func TestIntegration_CategoryLabelsJoin(t *testing.T) {
	integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A check of the schema Grafana and the product API read, not of ingest:
	// the scoped ingest role has no grant on category_labels and no UPDATE
	// on sources (099_read_roles.sh), so this runs as the admin connection.
	pool := adminPool(t, ctx)

	ddl, err := os.ReadFile("../db/migrations/010_category_labels.sql")
	if err != nil {
		t.Fatalf("read category labels schema: %v", err)
	}
	if _, err := adminPool(t, ctx).Exec(ctx, string(ddl)); err != nil {
		t.Fatalf("apply category labels schema: %v", err)
	}

	var genericLabel string
	if err := pool.QueryRow(ctx, `
		SELECT label
		FROM category_labels
		WHERE type_identifier = 'HKCategoryTypeIdentifierHighHeartRateEvent'
		  AND value = 0`).Scan(&genericLabel); err != nil {
		t.Fatalf("lookup generic category label: %v", err)
	}
	if genericLabel != "Not Applicable" {
		t.Fatalf("generic label = %q, want Not Applicable", genericLabel)
	}

	var typeID int16
	if err := pool.QueryRow(ctx, `
		INSERT INTO sample_types (identifier, kind, unit)
		VALUES ('HKCategoryTypeIdentifierSleepAnalysis', 'category', NULL)
		ON CONFLICT (identifier) DO UPDATE
		SET kind = EXCLUDED.kind,
		    unit = EXCLUDED.unit
		RETURNING type_id`).Scan(&typeID); err != nil {
		t.Fatalf("upsert sleep sample type: %v", err)
	}
	var sourceID int16
	if err := pool.QueryRow(ctx, `
		INSERT INTO sources (name, bundle_id, version)
		VALUES ('Category Label Test', '', '')
		ON CONFLICT (name, bundle_id, version) DO UPDATE
		SET name = EXCLUDED.name
		RETURNING source_id`).Scan(&sourceID); err != nil {
		t.Fatalf("upsert source: %v", err)
	}

	run := time.Now().UnixNano()
	sampleUUID := fmt.Sprintf("%08x-0101-4000-8000-%012x", run>>32, run&0xffffffffffff)
	if _, err := pool.Exec(ctx, `
		INSERT INTO category_samples (uuid, type_id, start_ts, end_ts, value, source_id, metadata, user_id)
		VALUES ($1, $2, '2024-06-10T00:00:00Z', '2024-06-10T01:00:00Z', 3, $3, '{}'::jsonb, $4)
		ON CONFLICT DO NOTHING`,
		sampleUUID, typeID, sourceID, referenceMemberID); err != nil {
		t.Fatalf("insert category sample: %v", err)
	}

	var sleepLabel, enumName string
	if err := pool.QueryRow(ctx, `
		SELECT cl.label, cl.enum_name
		FROM category_samples c
		JOIN sample_types st USING (type_id)
		JOIN category_labels cl
		  ON cl.type_identifier = st.identifier
		 AND cl.value = c.value
		WHERE c.uuid = $1`,
		sampleUUID).Scan(&sleepLabel, &enumName); err != nil {
		t.Fatalf("join category label: %v", err)
	}
	if sleepLabel != "Asleep Core" {
		t.Fatalf("sleep label = %q, want Asleep Core", sleepLabel)
	}
	if enumName != "HKCategoryValueSleepAnalysisAsleepCore" {
		t.Fatalf("sleep enum = %q, want HKCategoryValueSleepAnalysisAsleepCore", enumName)
	}
}

// TestIntegration_SeriesKindsAndRoutes exercises the new sample kinds
// (heartbeatSeries, ecg, stateOfMind, medicationDose) plus workout routes:
// insert, idempotent retry, stats, and deletion (incl. route points).
// Gated on DATABASE_URL like TestIntegration_IngestRoundTrip.
func TestIntegration_SeriesKindsAndRoutes(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	run := time.Now().UnixNano()
	batchID := fmt.Sprintf("%08x-0010-4000-8000-%012x", run>>32, run&0xffffffffffff)
	wkUUID := fmt.Sprintf("%08x-0011-4000-8000-%012x", run>>32, run&0xffffffffffff)
	hbUUID := fmt.Sprintf("%08x-0012-4000-8000-%012x", run>>32, run&0xffffffffffff)
	ecgUUID := fmt.Sprintf("%08x-0013-4000-8000-%012x", run>>32, run&0xffffffffffff)
	somUUID := fmt.Sprintf("%08x-0014-4000-8000-%012x", run>>32, run&0xffffffffffff)
	medUUID := fmt.Sprintf("%08x-0015-4000-8000-%012x", run>>32, run&0xffffffffffff)

	// One workout whose route is split across two route lines (2 + 1 points),
	// plus an intra-workout HR series (2 points) and a profile line.
	body := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"HKWorkoutTypeIdentifier","reason":"manual","exportedAt":1718000000000,"sampleCount":5,"deletionCount":0,"routeCount":2,"seriesCount":1,"profileCount":1}
{"uuid":"%s","type":"HKWorkoutTypeIdentifier","kind":"workout","start":1718000000000,"end":1718003600500,"workout":{"activityType":"running","duration":3600.5,"totalEnergyKcal":450.2,"totalDistanceMeters":8046.7,"statistics":{"HKQuantityTypeIdentifierHeartRate":152.0},"statisticsDetail":{"HKQuantityTypeIdentifierHeartRate":{"min":98.0,"avg":152.0,"max":178.0}},"events":[{"type":"lap","start":1718001800000}],"activities":[{"activityType":"running","start":1718000000000,"end":1718003600500,"duration":3600.5,"statistics":{"HKQuantityTypeIdentifierHeartRate":{"avg":152.0}}}]}}
{"uuid":"%s","type":"HKDataTypeIdentifierHeartbeatSeries","kind":"heartbeatSeries","start":1718000000000,"end":1718000060000,"sourceName":"Apple Watch","heartbeats":[[0.5,false],[1.2,true],[1.9,false]]}
{"uuid":"%s","type":"HKDataTypeIdentifierElectrocardiogram","kind":"ecg","start":1718000000000,"end":1718000030000,"sourceName":"Apple Watch","ecg":{"classification":"sinusRhythm","averageHeartRateBpm":61.0,"samplingFrequencyHz":512.0,"symptomsStatus":"none","voltagesUV":[1.5,-2.25,3.0,4.5]}}
{"uuid":"%s","type":"HKDataTypeIdentifierStateOfMind","kind":"stateOfMind","start":1718000000000,"end":1718000000000,"sourceName":"iPhone","stateOfMind":{"kind":"momentaryEmotion","valence":0.42,"valenceClassification":"pleasant","labels":["happy"],"associations":["fitness"]}}
{"uuid":"%s","type":"HKMedicationDoseEventTypeIdentifierMedicationDoseEvent","kind":"medicationDose","start":1718000000000,"end":1718000000000,"sourceName":"iPhone","medicationDose":{"medication":"Ibuprofen","medicationID":"d1000000-0000-4000-8000-000000000001","status":"taken","scheduledAt":1717999200000,"doseQuantity":200,"doseUnit":"mg"}}
{"route":{"workoutUUID":"%s","points":[{"t":1718000001000,"lat":37.3349,"lon":-122.009,"alt":12.5,"hAcc":3.2,"vAcc":4.1,"speed":2.8,"course":181.0},{"t":1718000002000,"lat":37.335,"lon":-122.0091}]}}
{"route":{"workoutUUID":"%s","points":[{"t":1718000003000,"lat":37.3351,"lon":-122.0092}]}}
{"series":{"workoutUUID":"%s","type":"HKQuantityTypeIdentifierHeartRate","unit":"count/min","points":[{"t":1718000001000,"value":120.0},{"t":1718000002000,"value":135.5}]}}
{"profile":{"name":"Ada Example","email":"ada@example.com","dateOfBirth":631152000000,"biologicalSex":"male"}}
`, batchID, wkUUID, hbUUID, ecgUUID, somUUID, medUUID, wkUUID, wkUUID, wkUUID)

	batch, err := parseReferenceBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}

	res, err := store.InsertBatch(ctx, batch, int64(len(body)))
	if err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if res.Accepted != 5 || res.Duplicates != 0 || res.RoutePoints != 3 || res.SeriesPoints != 2 || res.DuplicateBatch {
		t.Fatalf("first insert: %+v", res)
	}

	// Retried batch must be idempotent across all tables.
	res2, err := store.InsertBatch(ctx, batch, int64(len(body)))
	if err != nil {
		t.Fatalf("retry InsertBatch: %v", err)
	}
	assertStoredBatchReplay(t, res, res2)

	// Spot-check stored shapes.
	var beatCount, voltLen, ptCount int
	if err := pool.QueryRow(ctx,
		`SELECT beat_count FROM heartbeat_series WHERE uuid = $1`, hbUUID).Scan(&beatCount); err != nil {
		t.Fatalf("verify heartbeat: %v", err)
	}
	if beatCount != 3 {
		t.Errorf("beat_count = %d, want 3", beatCount)
	}
	if err := pool.QueryRow(ctx,
		`SELECT cardinality(voltage_uv) FROM ecg_samples WHERE uuid = $1`, ecgUUID).Scan(&voltLen); err != nil {
		t.Fatalf("verify ecg: %v", err)
	}
	if voltLen != 4 {
		t.Errorf("voltage_uv length = %d, want 4", voltLen)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM workout_route_points WHERE workout_uuid = $1`, wkUUID).Scan(&ptCount); err != nil {
		t.Fatalf("verify route points: %v", err)
	}
	if ptCount != 3 {
		t.Errorf("route points = %d, want 3", ptCount)
	}
	var seriesPts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM workout_series_points WHERE workout_uuid = $1`, wkUUID).Scan(&seriesPts); err != nil {
		t.Fatalf("verify series points: %v", err)
	}
	if seriesPts != 2 {
		t.Errorf("series points = %d, want 2", seriesPts)
	}
	// Richer workout columns persisted as jsonb.
	var hrMax float64
	if err := pool.QueryRow(ctx,
		`SELECT (stats_detail->'HKQuantityTypeIdentifierHeartRate'->>'max')::float8 FROM workouts WHERE uuid = $1`,
		wkUUID).Scan(&hrMax); err != nil {
		t.Fatalf("verify stats_detail: %v", err)
	}
	if hrMax != 178.0 {
		t.Errorf("stats_detail HR max = %v, want 178", hrMax)
	}
	var eventCount, activityCount int
	if err := pool.QueryRow(ctx,
		`SELECT jsonb_array_length(events), jsonb_array_length(activities) FROM workouts WHERE uuid = $1`,
		wkUUID).Scan(&eventCount, &activityCount); err != nil {
		t.Fatalf("verify events/activities: %v", err)
	}
	if eventCount != 1 || activityCount != 1 {
		t.Errorf("events=%d activities=%d, want 1/1", eventCount, activityCount)
	}
	// Profile upserted onto the (default) user; rows are tagged with that user.
	var sex, name string
	if err := pool.QueryRow(ctx,
		`SELECT biological_sex, name FROM users WHERE id = $1`, referenceMemberID).Scan(&sex, &name); err != nil {
		t.Fatalf("verify user: %v", err)
	}
	if sex != "male" {
		t.Errorf("user sex = %q, want male", sex)
	}
	if name != "Ada Example" {
		t.Errorf("user name = %q, want Ada Example", name)
	}
	var rowUser string
	if err := pool.QueryRow(ctx,
		`SELECT user_id FROM workouts WHERE uuid = $1`, wkUUID).Scan(&rowUser); err != nil {
		t.Fatalf("verify workout user_id: %v", err)
	}
	if rowUser != referenceMemberID {
		t.Errorf("workout user_id = %q, want %q", rowUser, referenceMemberID)
	}
	var labels []string
	if err := pool.QueryRow(ctx,
		`SELECT labels FROM state_of_mind WHERE uuid = $1`, somUUID).Scan(&labels); err != nil {
		t.Fatalf("verify state of mind: %v", err)
	}
	if len(labels) != 1 || labels[0] != "happy" {
		t.Errorf("labels = %v", labels)
	}
	var status, medicationID string
	if err := pool.QueryRow(ctx,
		`SELECT status, medication_id FROM medication_dose_events WHERE uuid = $1`, medUUID).Scan(&status, &medicationID); err != nil {
		t.Fatalf("verify medication dose: %v", err)
	}
	if medicationID != "d1000000-0000-4000-8000-000000000001" {
		t.Errorf("medication identity = %q", medicationID)
	}
	if status != "taken" {
		t.Errorf("status = %q", status)
	}

	// Stats must report the hardcoded identifiers for the new tables.
	stats, err := store.Stats(ctx, recordScope{userID: referenceMemberID})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	found := map[string]bool{}
	for _, s := range stats {
		found[s.Type] = true
	}
	for _, want := range []string{
		"HKDataTypeIdentifierHeartbeatSeries",
		"HKDataTypeIdentifierElectrocardiogram",
		"HKDataTypeIdentifierStateOfMind",
		"HKMedicationDoseEventTypeIdentifierMedicationDoseEvent",
	} {
		if !found[want] {
			t.Errorf("stats missing type %s", want)
		}
	}

	// Deleting all five samples must also drop the workout's route points.
	delBatchID := fmt.Sprintf("%08x-0016-4000-8000-%012x", run>>32, run&0xffffffffffff)
	var dels strings.Builder
	fmt.Fprintf(&dels, `{"batchID":"%s","deviceID":"itest","type":"HKWorkoutTypeIdentifier","reason":"incremental","exportedAt":1718000100000,"sampleCount":0,"deletionCount":5}`+"\n", delBatchID)
	for uuid, typ := range map[string]string{
		wkUUID:  "HKWorkoutTypeIdentifier",
		hbUUID:  "HKDataTypeIdentifierHeartbeatSeries",
		ecgUUID: "HKDataTypeIdentifierElectrocardiogram",
		somUUID: "HKDataTypeIdentifierStateOfMind",
		medUUID: "HKMedicationDoseEventTypeIdentifierMedicationDoseEvent",
	} {
		fmt.Fprintf(&dels, `{"deleted":{"uuid":"%s","type":"%s"}}`+"\n", uuid, typ)
	}
	delBatch, err := parseReferenceBatch(strings.NewReader(dels.String()))
	if err != nil {
		t.Fatalf("ParseBatch deletions: %v", err)
	}
	res3, err := store.InsertBatch(ctx, delBatch, int64(dels.Len()))
	if err != nil {
		t.Fatalf("InsertBatch deletions: %v", err)
	}
	if res3.Deleted != 5 {
		t.Fatalf("deletion batch: %+v", res3)
	}
	for _, q := range []struct{ query, uuid string }{
		{`SELECT count(*) FROM workouts WHERE uuid = $1`, wkUUID},
		{`SELECT count(*) FROM heartbeat_series WHERE uuid = $1`, hbUUID},
		{`SELECT count(*) FROM ecg_samples WHERE uuid = $1`, ecgUUID},
		{`SELECT count(*) FROM state_of_mind WHERE uuid = $1`, somUUID},
		{`SELECT count(*) FROM medication_dose_events WHERE uuid = $1`, medUUID},
		{`SELECT count(*) FROM workout_route_points WHERE workout_uuid = $1`, wkUUID},
		{`SELECT count(*) FROM workout_series_points WHERE workout_uuid = $1`, wkUUID},
	} {
		var n int
		if err := pool.QueryRow(ctx, q.query, q.uuid).Scan(&n); err != nil {
			t.Fatalf("verify delete (%s): %v", q.query, err)
		}
		if n != 0 {
			t.Errorf("rows remain after deletion: %s (%s)", q.query, q.uuid)
		}
	}
}

// TestIntegration_AggregateUpsert exercises the aggregate-bucket path against
// a real database: recomputed buckets overwrite previous values (including an
// explicit null), and the natural series key is shared across devices.
// Gated on DATABASE_URL like TestIntegration_IngestRoundTrip. The schema in
// db/migrations may not have reached a shared DB yet, so this test applies the
// idempotent 003_aggregates.sql itself in case the volume predates it.
func TestIntegration_AggregateUpsert(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	ddl, err := os.ReadFile("../db/migrations/003_aggregates.sql")
	if err != nil {
		t.Fatalf("read 003_aggregates.sql: %v", err)
	}
	if _, err := adminPool(t, ctx).Exec(ctx, string(ddl)); err != nil {
		t.Fatalf("apply 003_aggregates.sql: %v", err)
	}

	run := time.Now().UnixNano()
	// Run-unique, aggregate-only type: no sample line ever mentions it, so
	// this also proves ensureTypes registers identifiers seen only in
	// aggregate lines.
	typeIdent := fmt.Sprintf("ITestAggQuantity%016x", run)
	bucketStart := int64(1718000000000)
	bucketEnd := int64(1718003600000)

	send := func(seq int, deviceID, value string) IngestResult {
		t.Helper()
		batchID := fmt.Sprintf("%08x-00%02x-4000-8000-%012x", run>>32, 0x30+seq, run&0xffffffffffff)
		body := fmt.Sprintf(`{"batchID":"%s","deviceID":"%s","type":"%s","reason":"manual","exportedAt":1718000000000,"sampleCount":0,"deletionCount":0,"aggregateCount":1}
{"aggregate":{"type":"%s","func":"average","intervalValue":1,"intervalUnit":"hour","deviceFilter":"watch","bucketStart":%d,"bucketEnd":%d,"value":%s,"unit":"count/min"}}
`, batchID, deviceID, typeIdent, typeIdent, bucketStart, bucketEnd, value)
		batch, err := parseReferenceBatch(strings.NewReader(body))
		if err != nil {
			t.Fatalf("ParseBatch: %v", err)
		}
		res, err := store.InsertBatch(ctx, batch, int64(len(body)))
		if err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
		return res
	}

	readBucket := func() (n int, value *float64, updatedAt time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM aggregate_samples b
			JOIN aggregate_series s USING (series_id)
			JOIN sample_types t USING (type_id)
			WHERE t.identifier = $1`, typeIdent).Scan(&n); err != nil {
			t.Fatalf("count buckets: %v", err)
		}
		if n == 0 {
			return
		}
		if err := pool.QueryRow(ctx, `
			SELECT b.value, b.updated_at FROM aggregate_samples b
			JOIN aggregate_series s USING (series_id)
			JOIN sample_types t USING (type_id)
			WHERE t.identifier = $1
			ORDER BY b.bucket_start LIMIT 1`, typeIdent).Scan(&value, &updatedAt); err != nil {
			t.Fatalf("read bucket: %v", err)
		}
		return
	}

	if res := send(1, "itest-a", "10"); res.AggregateSamples != 1 {
		t.Fatalf("first insert: %+v", res)
	}
	n, v, firstAt := readBucket()
	if n != 1 || v == nil || *v != 10 {
		t.Fatalf("after first insert: rows=%d value=%v, want 1 row with 10", n, v)
	}

	// Recomputed bucket overwrites: still one row, new value, updated_at bumped.
	if res := send(2, "itest-a", "12"); res.AggregateSamples != 1 {
		t.Fatalf("recompute insert: %+v", res)
	}
	n, v, secondAt := readBucket()
	if n != 1 || v == nil || *v != 12 {
		t.Fatalf("after recompute: rows=%d value=%v, want 1 row with 12", n, v)
	}
	if secondAt.Before(firstAt) {
		t.Errorf("updated_at went backwards: %v -> %v", firstAt, secondAt)
	}

	// Explicit null clears the value but keeps the row.
	if res := send(3, "itest-a", "null"); res.AggregateSamples != 1 {
		t.Fatalf("null insert: %+v", res)
	}
	n, v, _ = readBucket()
	if n != 1 || v != nil {
		t.Fatalf("after null upsert: rows=%d value=%v, want 1 row with NULL", n, v)
	}

	// The same natural key from a different device reuses the same series.
	if res := send(4, "itest-b", "11"); res.AggregateSamples != 1 {
		t.Fatalf("other-device insert: %+v", res)
	}
	var seriesCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM aggregate_series s
		JOIN sample_types t USING (type_id)
		WHERE t.identifier = $1 AND s.agg_func = 'average'
		  AND s.interval_value = 1 AND s.interval_unit = 'hour' AND s.device_filter = 'watch'`,
		typeIdent).Scan(&seriesCount); err != nil {
		t.Fatalf("count series: %v", err)
	}
	if seriesCount != 1 {
		t.Errorf("aggregate_series rows = %d, want 1", seriesCount)
	}

	// The aggregate-only type landed in sample_types as a quantity with the
	// unit from the aggregate line.
	var kind, unit *string
	if err := pool.QueryRow(ctx,
		`SELECT kind, unit FROM sample_types WHERE identifier = $1`, typeIdent).Scan(&kind, &unit); err != nil {
		t.Fatalf("verify sample_types: %v", err)
	}
	if kind == nil || *kind != "quantity" || unit == nil || *unit != "count/min" {
		t.Errorf("sample_types kind=%v unit=%v, want quantity/count-per-min", kind, unit)
	}
}

func TestIntegration_MetricDailyUsesCanonicalSeriesOnly(t *testing.T) {
	integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// A check of the metric_daily view the product API reads, with fixtures
	// written straight to the tables: the scoped ingest role may not read
	// the view nor delete the fixtures (099_read_roles.sh), so this runs as
	// the admin connection.
	pool := adminPool(t, ctx)

	run := time.Now().UnixNano()
	userID := fmt.Sprintf("%08x-0200-4000-8000-%012x", run>>32, run&0xffffffffffff)
	cumulative := fmt.Sprintf("ITestMetricDailyCumulative%016x", run)
	discrete := fmt.Sprintf("ITestMetricDailyDiscrete%016x", run)
	if _, err := pool.Exec(ctx, `INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM aggregate_samples WHERE user_id = $1`, userID)
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM aggregate_series
			WHERE type_id IN (SELECT type_id FROM sample_types WHERE identifier IN ($1, $2))`, cumulative, discrete)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sample_types WHERE identifier IN ($1, $2)`, cumulative, discrete)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	}()

	var cumulativeID, discreteID int16
	if err := pool.QueryRow(ctx, `
		INSERT INTO sample_types (identifier, kind, unit)
		VALUES ($1, 'quantity', 'count') RETURNING type_id`, cumulative).Scan(&cumulativeID); err != nil {
		t.Fatalf("insert cumulative type: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO sample_types (identifier, kind, unit)
		VALUES ($1, 'quantity', 'count/min') RETURNING type_id`, discrete).Scan(&discreteID); err != nil {
		t.Fatalf("insert discrete type: %v", err)
	}

	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	dayStart := time.Date(2096, 5, 5, 0, 0, 0, 0, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)

	insertAggregate := func(typeID int16, function string, intervalValue int, intervalUnit, device string, start, end time.Time, value float64) {
		t.Helper()
		var seriesID int16
		if err := pool.QueryRow(ctx, `
			INSERT INTO aggregate_series
			    (type_id, agg_func, interval_value, interval_unit, device_filter)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING series_id`, typeID, function, intervalValue, intervalUnit, device).Scan(&seriesID); err != nil {
			t.Fatalf("insert series %d/%s/%s/%s: %v", typeID, function, intervalUnit, device, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO aggregate_samples
			    (series_id, bucket_start, bucket_end, value, user_id)
			VALUES ($1, $2, $3, $4, $5)`, seriesID, start, end, value, userID); err != nil {
			t.Fatalf("insert aggregate %d/%s/%s/%s: %v", typeID, function, intervalUnit, device, err)
		}
	}

	insertAggregate(cumulativeID, "sum", 1, "day", "all", dayStart, dayEnd, 100)
	insertAggregate(cumulativeID, "sum", 1, "hour", "all", dayStart.Add(time.Hour), dayStart.Add(2*time.Hour), 999)
	insertAggregate(cumulativeID, "sum", 1, "day", "watch", dayStart, dayEnd, 888)
	insertAggregate(cumulativeID, "max", 1, "day", "all", dayStart, dayEnd, 777)

	insertAggregate(discreteID, "average", 1, "day", "all", dayStart, dayEnd, 60)
	insertAggregate(discreteID, "average", 1, "hour", "all", dayStart.Add(time.Hour), dayStart.Add(2*time.Hour), 130)
	insertAggregate(discreteID, "average", 1, "day", "watch", dayStart, dayEnd, 70)
	insertAggregate(discreteID, "max", 1, "day", "all", dayStart, dayEnd, 180)

	rows, err := pool.Query(ctx, `
		SELECT identifier, value::float8, source
		FROM metric_daily
		WHERE user_id = $1 AND day = $2::date
		  AND identifier IN ($3, $4)
		ORDER BY identifier`, userID, dayStart.Format("2006-01-02"), cumulative, discrete)
	if err != nil {
		t.Fatalf("query metric_daily: %v", err)
	}
	defer rows.Close()
	got := map[string]struct {
		value  float64
		source string
	}{}
	for rows.Next() {
		var identifier, source string
		var value float64
		if err := rows.Scan(&identifier, &value, &source); err != nil {
			t.Fatalf("scan metric_daily: %v", err)
		}
		got[identifier] = struct {
			value  float64
			source string
		}{value: value, source: source}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("metric_daily rows: %v", err)
	}
	if row := got[cumulative]; row.value != 100 || row.source != "aggregate" {
		t.Fatalf("cumulative metric_daily = %+v, want value=100 source=aggregate", row)
	}
	if row := got[discrete]; row.value != 60 || row.source != "aggregate" {
		t.Fatalf("discrete metric_daily = %+v, want value=60 source=aggregate", row)
	}
}

func TestIntegration_ProfileSnapshotReplacement(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	run := time.Now().UnixNano()
	userID := fmt.Sprintf("%08x-0300-4000-8000-%012x", run>>32, run&0xffffffffffff)
	// Cleanup as the admin connection: ingest never deletes batches or users.
	admin := adminPool(t, ctx)
	defer func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM batches WHERE user_id = $1`, userID)
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	}()

	send := func(seq int, profile string) {
		t.Helper()
		batchID := fmt.Sprintf("%08x-%04x-4000-8000-%012x", run>>32, 0x301+seq, run&0xffffffffffff)
		body := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"HKUserProfile","reason":"manual","exportedAt":1718000000000,"sampleCount":0,"deletionCount":0,"profileCount":1}
{"profile":%s}
`, batchID, profile)
		batch, err := parseReferenceBatch(strings.NewReader(body))
		if err != nil {
			t.Fatalf("ParseBatch: %v", err)
		}
		batch.Header.UserID = userID
		if _, err := store.InsertBatch(ctx, batch, int64(len(body))); err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
	}

	read := func() (name, email *string, dob *time.Time, sex *string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
			SELECT name, email, dob::timestamp AT TIME ZONE 'UTC', biological_sex
			FROM users WHERE id = $1`, userID).Scan(&name, &email, &dob, &sex); err != nil {
			t.Fatalf("read profile: %v", err)
		}
		return
	}

	send(0, `{"name":"Initial","email":"initial@example.com","dateOfBirth":631152000000,"biologicalSex":"male"}`)
	name, email, dob, sex := read()
	if name == nil || *name != "Initial" || email == nil || *email != "initial@example.com" || dob == nil || sex == nil || *sex != "male" {
		t.Fatalf("initial profile = name:%v email:%v dob:%v sex:%v", name, email, dob, sex)
	}

	// A partial object is still a complete snapshot: omitted email/DOB clear.
	send(1, `{"name":"Updated","biologicalSex":"female"}`)
	name, email, dob, sex = read()
	if name == nil || *name != "Updated" || email != nil || dob != nil || sex == nil || *sex != "female" {
		t.Fatalf("partial replacement = name:%v email:%v dob:%v sex:%v", name, email, dob, sex)
	}

	// Explicit all-null snapshots clear every profile column.
	send(2, `{"name":null,"email":null,"dateOfBirth":null,"biologicalSex":null}`)
	name, email, dob, sex = read()
	if name != nil || email != nil || dob != nil || sex != nil {
		t.Fatalf("all-null replacement = name:%v email:%v dob:%v sex:%v", name, email, dob, sex)
	}
}

// TestIntegration_DigestAndUUIDs inserts quantity samples of a run-unique
// type across two UTC months and checks /v1/digest windows (XOR of raw UUID
// bytes, computed independently here) and /v1/uuids over HTTP, including the
// 35-day range guard.
func TestIntegration_DigestAndUUIDs(t *testing.T) {
	f := familyTestSetup(t)
	ctx, run := f.ctx, f.run
	typeIdent := "HKQuantityTypeIdentifierStepCount"
	batchID, u1, u2, u3 := f.uuid(0x720), f.uuid(0x721), f.uuid(0x722), f.uuid(0x723)

	juneA := time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC).UnixMilli()
	juneB := time.Date(2024, 6, 20, 8, 30, 0, 0, time.UTC).UnixMilli()
	july := time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	body := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"%s","reason":"manual","exportedAt":1718000000000,"sampleCount":3,"deletionCount":0}
{"uuid":"%s","type":"%s","kind":"quantity","start":%d,"end":%d,"value":1.0,"unit":"count"}
{"uuid":"%s","type":"%s","kind":"quantity","start":%d,"end":%d,"value":2.0,"unit":"count"}
{"uuid":"%s","type":"%s","kind":"quantity","start":%d,"end":%d,"value":3.0,"unit":"count"}
`, batchID, typeIdent, u1, typeIdent, juneA, juneA, u2, typeIdent, juneB, juneB, u3, typeIdent, july, july)
	if status, _ := f.post(t, f.actors[0], body); status != http.StatusOK {
		t.Fatalf("personal sample upload status = %d, want 200", status)
	}
	var inserted int
	if err := f.admin.QueryRow(ctx, "SELECT count(*) FROM public.quantity_samples WHERE user_id=$1", f.actors[0].user).Scan(&inserted); err != nil || inserted != 3 {
		t.Fatalf("personal stored rows = %d, error = %v, want 3", inserted, err)
	}
	ts := f.server

	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.actors[0].token)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, buf
	}

	// Digest over both months: two windows with independently computed XORs.
	from := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	to := time.Date(2024, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	resp, buf := get(fmt.Sprintf("/v1/digest?type=%s&from=%d&to=%d", typeIdent, from, to))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("digest status = %d, body = %s", resp.StatusCode, buf)
	}
	var windows []DigestWindow
	if err := json.Unmarshal(buf, &windows); err != nil {
		t.Fatalf("digest body %s: %v", buf, err)
	}
	xor := func(uuids ...string) string {
		t.Helper()
		var acc [16]byte
		for _, u := range uuids {
			raw, err := uuidRawBytes(u)
			if err != nil {
				t.Fatal(err)
			}
			for i := range raw {
				acc[i] ^= raw[i]
			}
		}
		return hex.EncodeToString(acc[:])
	}
	want := []DigestWindow{
		{Window: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), Rows: 2, Digest: xor(u1, u2)},
		{Window: time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), Rows: 1, Digest: xor(u3)},
	}
	if len(windows) != len(want) {
		t.Fatalf("digest windows = %+v, want %+v", windows, want)
	}
	for i := range want {
		if windows[i] != want[i] {
			t.Errorf("digest window %d = %+v, want %+v", i, windows[i], want[i])
		}
	}

	// Unknown type: 200 with empty array.
	resp, buf = get(fmt.Sprintf("/v1/digest?type=NoSuchType%d&from=%d&to=%d", run, from, to))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(buf)) != "[]" {
		t.Errorf("unknown type digest: status = %d, body = %s", resp.StatusCode, buf)
	}

	// UUIDs over July only (well under 35 days).
	resp, buf = get(fmt.Sprintf("/v1/uuids?type=%s&from=%d&to=%d", typeIdent, july,
		time.Date(2024, 7, 15, 0, 0, 0, 0, time.UTC).UnixMilli()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("uuids status = %d, body = %s", resp.StatusCode, buf)
	}
	var uuidResp struct {
		UUIDs []string `json:"uuids"`
	}
	if err := json.Unmarshal(buf, &uuidResp); err != nil {
		t.Fatalf("uuids body %s: %v", buf, err)
	}
	if len(uuidResp.UUIDs) != 1 || uuidResp.UUIDs[0] != u3 {
		t.Errorf("uuids = %v, want [%s]", uuidResp.UUIDs, u3)
	}

	// Range guard: 61 days must be rejected.
	resp, buf = get(fmt.Sprintf("/v1/uuids?type=%s&from=%d&to=%d", typeIdent, from, to))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("range guard: status = %d, body = %s", resp.StatusCode, buf)
	}
}

func TestIntegration_ReadEndpointsAreUserScoped(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	run := time.Now().UnixNano()
	typeIdent := fmt.Sprintf("ITestUserScoped%016x", run)
	userA := fmt.Sprintf("%08x-0100-4000-8000-%012x", run>>32, run&0xffffffffffff)
	userB := fmt.Sprintf("%08x-0101-4000-8000-%012x", run>>32, run&0xffffffffffff)
	start := time.Date(2097, 4, 5, 12, 0, 0, 0, time.UTC)

	type fixture struct {
		userID      string
		batchID     string
		sampleUUID  string
		workoutUUID string
		value       float64
	}
	fixtures := []fixture{
		{
			userID:      userA,
			batchID:     fmt.Sprintf("%08x-0102-4000-8000-%012x", run>>32, run&0xffffffffffff),
			sampleUUID:  fmt.Sprintf("%08x-0103-4000-8000-%012x", run>>32, run&0xffffffffffff),
			workoutUUID: fmt.Sprintf("%08x-0104-4000-8000-%012x", run>>32, run&0xffffffffffff),
			value:       61,
		},
		{
			userID:      userB,
			batchID:     fmt.Sprintf("%08x-0105-4000-8000-%012x", run>>32, run&0xffffffffffff),
			sampleUUID:  fmt.Sprintf("%08x-0106-4000-8000-%012x", run>>32, run&0xffffffffffff),
			workoutUUID: fmt.Sprintf("%08x-0107-4000-8000-%012x", run>>32, run&0xffffffffffff),
			value:       173,
		},
	}

	for _, f := range fixtures {
		body := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"%s","reason":"manual","exportedAt":%d,"sampleCount":2,"deletionCount":0,"routeCount":1,"seriesCount":1}
{"uuid":"%s","type":"%s","kind":"quantity","start":%d,"value":%.1f,"unit":"count/min"}
{"uuid":"%s","type":"HKWorkoutTypeIdentifier","kind":"workout","start":%d,"end":%d,"workout":{"activityType":"running","duration":1800}}
{"route":{"workoutUUID":"%s","points":[{"t":%d,"lat":37.0,"lon":-122.0},{"t":%d,"lat":37.1,"lon":-122.1}]}}
{"series":{"workoutUUID":"%s","type":"%s","unit":"count/min","points":[{"t":%d,"value":%.1f}]}}
`, f.batchID, typeIdent, start.UnixMilli(),
			f.sampleUUID, typeIdent, start.UnixMilli(), f.value,
			f.workoutUUID, start.UnixMilli(), start.Add(30*time.Minute).UnixMilli(),
			f.workoutUUID, start.Add(time.Minute).UnixMilli(), start.Add(2*time.Minute).UnixMilli(),
			f.workoutUUID, typeIdent, start.Add(time.Minute).UnixMilli(), f.value)
		batch, err := parseReferenceBatch(strings.NewReader(body))
		if err != nil {
			t.Fatalf("parseReferenceBatch(%s): %v", f.userID, err)
		}
		batch.Header.UserID = f.userID
		res, err := store.InsertBatch(ctx, batch, int64(len(body)))
		if err != nil {
			t.Fatalf("InsertBatch(%s): %v", f.userID, err)
		}
		if res.Accepted != 2 || res.RoutePoints != 2 || res.SeriesPoints != 1 {
			t.Fatalf("InsertBatch(%s) = %+v", f.userID, res)
		}
	}

	from, to := start.Add(-time.Hour), start.Add(time.Hour)
	for _, f := range fixtures {
		stats, err := store.Stats(ctx, recordScope{userID: f.userID})
		if err != nil {
			t.Fatalf("Stats(%s): %v", f.userID, err)
		}
		var typeRows int64
		for _, stat := range stats {
			if stat.Type == typeIdent {
				typeRows = stat.Rows
			}
		}
		if typeRows != 1 {
			t.Fatalf("Stats(%s) type rows = %d, want 1", f.userID, typeRows)
		}

		digest, err := store.Digest(ctx, recordScope{userID: f.userID}, typeIdent, from, to)
		if err != nil || len(digest) != 1 || digest[0].Rows != 1 {
			t.Fatalf("Digest(%s) = %+v, %v", f.userID, digest, err)
		}
		uuids, err := store.UUIDs(ctx, recordScope{userID: f.userID}, typeIdent, from, to)
		if err != nil || len(uuids) != 1 || uuids[0] != f.sampleUUID {
			t.Fatalf("UUIDs(%s) = %v, %v", f.userID, uuids, err)
		}
		routes, err := store.Routes(ctx, f.userID, RouteFilters{})
		if err != nil || len(routes) != 1 || routes[0].UUID != f.workoutUUID {
			t.Fatalf("Routes(%s) = %+v, %v", f.userID, routes, err)
		}
	}

	if route, err := store.Route(ctx, userA, fixtures[1].workoutUUID); err != nil || route != nil {
		t.Fatalf("user A read of user B route = %+v, %v; want nil", route, err)
	}
	if metrics, err := store.RouteMetrics(ctx, userA, fixtures[1].workoutUUID); err != nil || len(metrics) != 0 {
		t.Fatalf("user A read of user B metrics = %+v, %v; want []", metrics, err)
	}
	if route, err := store.Route(ctx, userB, fixtures[1].workoutUUID); err != nil || route == nil {
		t.Fatalf("user B own route = %+v, %v", route, err)
	}
	if metrics, err := store.RouteMetrics(ctx, userB, fixtures[1].workoutUUID); err != nil || len(metrics) != 1 {
		t.Fatalf("user B own metrics = %+v, %v", metrics, err)
	}
}

// TestIntegration_DeletionsOnCompressedChunk reproduces the production 53400
// failure: quantity_samples chunks past the columnstore policy are compressed,
// and a deletion batch whose tombstones land in many distinct columnstore
// batches used to exceed timescaledb's per-transaction tuple decompression
// limit (default 100k). 150k rows in one segment yield 150+ batches; 200
// spread-out deletions touch enough of them to trip the old code.
func TestIntegration_DeletionsOnCompressedChunk(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	run := time.Now().UnixNano()
	typeIdent := fmt.Sprintf("ITestCompressedDel%d", run)
	// A unique month per run keeps chunks disjoint from earlier runs against
	// the same dev database (chunk_time_interval is 1 month).
	monthStart := time.Date(2000, time.Month(1+run%12), 1, 0, 0, 0, 0, time.UTC).
		AddDate(-int(run/12%50), 0, 0)

	if _, err := pool.Exec(ctx, `
		INSERT INTO sample_types (identifier, kind, unit) VALUES ($1, 'quantity', 'count')
		ON CONFLICT (identifier) DO NOTHING`, typeIdent); err != nil {
		t.Fatalf("seed type: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO quantity_samples (uuid, type_id, start_ts, end_ts, value, user_id)
		SELECT gen_random_uuid(),
		       (SELECT type_id FROM sample_types WHERE identifier = $1),
		       $2::timestamptz + (i || ' seconds')::interval,
		       $2::timestamptz + (i || ' seconds')::interval,
		       i, $3::uuid
		FROM generate_series(1, 150000) i`, typeIdent, monthStart, referenceMemberID); err != nil {
		t.Fatalf("seed samples: %v", err)
	}
	// Compress every chunk holding the seeded rows. This needs the hypertable
	// owner, which the scoped ingest role is not, so it goes through the admin
	// pool. The chunks are selected by range overlap from the information
	// view: show_chunks(newer_than, older_than) returns only chunks that lie
	// *entirely* inside the window, and 1-month chunks are epoch-aligned
	// 30-day spans rather than calendar months, so the earlier exact
	// [monthStart, +1 month) window matched nothing and the deletions below
	// silently ran against row storage.
	admin := adminPool(t, ctx)
	seededEnd := monthStart.Add(150000 * time.Second)
	if _, err := admin.Exec(ctx, `
		SELECT compress_chunk(format('%I.%I', chunk_schema, chunk_name)::regclass,
		                      if_not_compressed => true)
		FROM timescaledb_information.chunks
		WHERE hypertable_schema = 'public' AND hypertable_name = 'quantity_samples'
		  AND range_end > $1::timestamptz AND range_start <= $2::timestamptz`,
		monthStart, seededEnd); err != nil {
		t.Fatalf("compress chunk: %v", err)
	}
	var uncompressed int
	if err := admin.QueryRow(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		WHERE hypertable_schema = 'public' AND hypertable_name = 'quantity_samples'
		  AND range_end > $1::timestamptz AND range_start <= $2::timestamptz
		  AND NOT is_compressed`, monthStart, seededEnd).Scan(&uncompressed); err != nil {
		t.Fatalf("verify compression: %v", err)
	}
	if uncompressed != 0 {
		t.Fatalf("%d chunk(s) holding the seeded rows are still uncompressed; the deletion path under test would not touch the columnstore", uncompressed)
	}

	// Every 750th row by time: ~200 victims, each in its own columnstore batch.
	rows, err := pool.Query(ctx, `
		SELECT uuid FROM (
			SELECT q.uuid, row_number() OVER (ORDER BY q.start_ts) AS rn
			FROM quantity_samples q
			JOIN sample_types st USING (type_id)
			WHERE st.identifier = $1) v
		WHERE rn % 750 = 0`, typeIdent)
	if err != nil {
		t.Fatalf("pick victims: %v", err)
	}
	var victims []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan victim: %v", err)
		}
		victims = append(victims, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("victims: %v", err)
	}
	if len(victims) != 200 {
		t.Fatalf("got %d victims, want 200", len(victims))
	}

	batchID := fmt.Sprintf("%08x-00cd-4000-8000-%012x", run>>32, run&0xffffffffffff)
	var sb strings.Builder
	fmt.Fprintf(&sb, `{"batchID":"%s","deviceID":"itest","type":"%s","reason":"incremental","exportedAt":1718000000000,"sampleCount":0,"deletionCount":%d}`+"\n",
		batchID, typeIdent, len(victims))
	for _, u := range victims {
		fmt.Fprintf(&sb, `{"deleted":{"uuid":"%s","type":"%s"}}`+"\n", u, typeIdent)
	}
	batch, err := parseReferenceBatch(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	res, err := store.InsertBatch(ctx, batch, int64(sb.Len()))
	if err != nil {
		t.Fatalf("InsertBatch over compressed chunk: %v", err)
	}
	if res.Deleted != int64(len(victims)) {
		t.Errorf("deleted = %d, want %d", res.Deleted, len(victims))
	}

	var remaining int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM quantity_samples q
		JOIN sample_types st USING (type_id)
		WHERE st.identifier = $1 AND q.uuid = ANY($2::uuid[])`,
		typeIdent, victims).Scan(&remaining); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d victims still present after deletion batch", remaining)
	}
}

// TestIntegration_WakeTelemetryColumns verifies the wake-correlation and timing
// columns added in 007_wake_telemetry.sql: wake_id/trigger come from the request
// headers (set on the batch header by the handler), parse_ms is passed in via
// b.ParseMs, and insert_ms is measured inside InsertBatch.
func TestIntegration_WakeTelemetryColumns(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	store := NewStore(pool)

	run := time.Now().UnixNano()
	batchID := fmt.Sprintf("%08x-00a0-4000-8000-%012x", run>>32, run&0xffffffffffff)
	hrUUID := fmt.Sprintf("%08x-00a1-4000-8000-%012x", run>>32, run&0xffffffffffff)
	wakeID := fmt.Sprintf("%08x-00a2-4000-8000-%012x", run>>32, run&0xffffffffffff)

	body := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"HKQuantityTypeIdentifierHeartRate","reason":"incremental","exportedAt":1718000000000,"sampleCount":1,"deletionCount":0}
{"uuid":"%s","type":"HKQuantityTypeIdentifierHeartRate","kind":"quantity","start":1718000000000,"end":1718000005000,"value":61.0,"unit":"count/min","sourceName":"Apple Watch"}
`, batchID, hrUUID)

	batch, err := parseReferenceBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	// Headers the handler would set, plus the server-measured parse time.
	batch.Header.WakeID = wakeID
	batch.Header.Trigger = "observer"
	batch.ParseMs = 7

	if _, err := store.InsertBatch(ctx, batch, int64(len(body))); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	var (
		gotWake    string
		gotTrigger string
		gotParse   int
		gotInsert  int
	)
	if err := pool.QueryRow(ctx,
		`SELECT wake_id::text, trigger, parse_ms, insert_ms FROM batches WHERE batch_id = $1`,
		batchID).Scan(&gotWake, &gotTrigger, &gotParse, &gotInsert); err != nil {
		t.Fatalf("read batch row: %v", err)
	}
	if gotWake != wakeID {
		t.Errorf("wake_id = %q, want %q", gotWake, wakeID)
	}
	if gotTrigger != "observer" {
		t.Errorf("trigger = %q, want observer", gotTrigger)
	}
	if gotParse != 7 {
		t.Errorf("parse_ms = %d, want 7", gotParse)
	}
	if gotInsert < 0 {
		t.Errorf("insert_ms = %d, want >= 0", gotInsert)
	}

	// An upload with no wake context (curl, reconciliation) stores NULLs.
	bareID := fmt.Sprintf("%08x-00a3-4000-8000-%012x", run>>32, run&0xffffffffffff)
	bareBody := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"HKQuantityTypeIdentifierHeartRate","reason":"manual","exportedAt":1718000000000,"schemaVersion":1,"clientVersion":"itest","sampleCount":0,"deletionCount":0}
`, bareID)
	bareBatch, err := parseReferenceBatch(strings.NewReader(bareBody))
	if err != nil {
		t.Fatalf("ParseBatch bare: %v", err)
	}
	if _, err := store.InsertBatch(ctx, bareBatch, int64(len(bareBody))); err != nil {
		t.Fatalf("InsertBatch bare: %v", err)
	}
	var wakeNull, triggerNull bool
	if err := pool.QueryRow(ctx,
		`SELECT wake_id IS NULL, trigger IS NULL FROM batches WHERE batch_id = $1`,
		bareID).Scan(&wakeNull, &triggerNull); err != nil {
		t.Fatalf("read bare batch row: %v", err)
	}
	if !wakeNull || !triggerNull {
		t.Errorf("bare batch: wake_id NULL=%v trigger NULL=%v, want both true", wakeNull, triggerNull)
	}
}

// TestIntegration_LookupSequencesDoNotBurnOnRepeat guards the leak that took
// ingest down on 2026-08-13: the ensure* helpers used a bare
// INSERT ... SELECT ... ON CONFLICT DO NOTHING, and Postgres evaluates nextval
// before it detects the conflict. Every batch therefore consumed an identity
// value per already-known type/source/series/context. sample_types.type_id is
// a smallint, so after ~33k batches the sequence hit its 32767 ceiling and
// every insert failed with SQLSTATE 2200H.
//
// A second batch carrying the same types, sources, series and temporal context
// must not advance any of the four lookup sequences.
func TestIntegration_LookupSequencesDoNotBurnOnRepeat(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	seqs := []string{
		"sample_types_type_id_seq",
		"sources_source_id_seq",
		"aggregate_series_series_id_seq",
		"temporal_contexts_temporal_context_id_seq",
	}
	// last_value reads NULL until the sequence has been called at least once;
	// COALESCE to 0 so a fresh database compares cleanly.
	readSeqs := func() map[string]int64 {
		t.Helper()
		out := map[string]int64{}
		for _, s := range seqs {
			var v int64
			if err := pool.QueryRow(ctx,
				`SELECT COALESCE(last_value, 0) FROM pg_sequences WHERE sequencename = $1`, s).Scan(&v); err != nil {
				t.Fatalf("read sequence %s: %v", s, err)
			}
			out[s] = v
		}
		return out
	}

	run := time.Now().UnixNano()
	typeIdent := fmt.Sprintf("ITestSeqQuantity%016x", run)
	aggIdent := fmt.Sprintf("ITestSeqAgg%016x", run)

	// Both batches carry identical type/source/series/temporal-context keys and
	// differ only in batch ID and sample UUID, mirroring back-to-back wakes.
	send := func(seq int) {
		t.Helper()
		batchID := fmt.Sprintf("%08x-00%02x-4000-8000-%012x", run>>32, 0x50+seq, run&0xffffffffffff)
		sampleUUID := fmt.Sprintf("%08x-00%02x-4000-8000-%012x", run>>32, 0x60+seq, run&0xffffffffffff)
		body := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"%s","reason":"incremental","exportedAt":1718000000000,"sampleCount":1,"deletionCount":0,"aggregateCount":1}
{"uuid":"%s","type":"%s","kind":"quantity","start":1718000000000,"end":1718000005000,"value":7,"unit":"count","sourceName":"ITest Source","sourceBundleID":"com.example.itest","sourceVersion":"1.0","startContext":{"timeZoneID":"America/Los_Angeles","utcOffsetSeconds":-25200,"source":"itest","confidence":"high","tzdbVersion":"2026a"}}
{"aggregate":{"type":"%s","func":"sum","intervalValue":1,"intervalUnit":"hour","deviceFilter":"watch","bucketStart":1718000000000,"bucketEnd":1718003600000,"value":%d,"unit":"count"}}
`, batchID, typeIdent, sampleUUID, typeIdent, aggIdent, seq)
		batch, err := parseReferenceBatch(strings.NewReader(body))
		if err != nil {
			t.Fatalf("ParseBatch %d: %v", seq, err)
		}
		if _, err := store.InsertBatch(ctx, batch, int64(len(body))); err != nil {
			t.Fatalf("InsertBatch %d: %v", seq, err)
		}
	}

	// First batch registers everything; the sequences are expected to move.
	send(1)
	before := readSeqs()

	// Second batch reuses all four lookup keys, so nothing new is registered.
	send(2)
	after := readSeqs()

	for _, s := range seqs {
		if after[s] != before[s] {
			t.Errorf("%s advanced %d -> %d on a repeat batch; nextval is being burned on conflicts",
				s, before[s], after[s])
		}
	}
}

// TestIntegration_DeviceTokens runs the per-device token path (SRV-8) end to
// end against the real schema: issue through the Store, authenticate through
// the middleware, land a batch stamped with the token, watch last_seen_at,
// revoke. Gated on DATABASE_URL like TestIntegration_IngestRoundTrip.
func TestIntegration_DeviceTokens(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Cleanup, not defer: the row deletions registered below must run
	// before the pool closes, and t.Cleanup runs last-in first-out.
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	admin := adminPool(t, ctx)

	run := time.Now().UnixNano()
	tokenUser := fmt.Sprintf("%08x-0200-4000-8000-%012x", run>>32, run&0xffffffffffff)
	otherUser := fmt.Sprintf("%08x-0201-4000-8000-%012x", run>>32, run&0xffffffffffff)

	plaintext, issued, err := store.IssueDeviceToken(ctx, tokenUser, "itest phone")
	if err != nil {
		t.Fatalf("IssueDeviceToken: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		// As the admin connection: ingest revokes tokens, it never deletes them.
		_, _ = admin.Exec(cctx, `DELETE FROM batches WHERE device_token_id = $1`, issued.ID)
		_, _ = admin.Exec(cctx, `DELETE FROM device_tokens WHERE id = $1`, issued.ID)
	})
	if len(plaintext) != 64 || issued.ID == 0 || issued.TokenPrefix != plaintext[:8] {
		t.Fatalf("issued = %q / %+v", plaintext, issued)
	}
	// Only the hash is stored, and the user row exists before any sync.
	var stored []byte
	var userExists bool
	if err := pool.QueryRow(ctx, `SELECT token_hash, EXISTS (SELECT 1 FROM users WHERE id = $2)
		FROM device_tokens WHERE id = $1`, issued.ID, tokenUser).Scan(&stored, &userExists); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(stored) == plaintext || !bytes.Equal(stored, hashToken(plaintext)) || !userExists {
		t.Fatalf("stored hash/user wrong: hash matches=%v user=%v", bytes.Equal(stored, hashToken(plaintext)), userExists)
	}
	// The credential table is not for dashboards (099_read_roles.sh).
	var grafanaCanRead bool
	if err := pool.QueryRow(ctx,
		`SELECT has_table_privilege('grafana', 'device_tokens', 'SELECT')`).Scan(&grafanaCanRead); err == nil && grafanaCanRead {
		t.Fatal("grafana can SELECT device_tokens")
	}

	srv := newServer(store, store, false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	do := func(method, path, token, userHeader string, body string) (int, string) {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, ts.URL+path, rdr)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Puls-Protocol", "1")
		if userHeader != "" {
			req.Header.Set("X-User-ID", userHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := do(http.MethodGet, "/v1/capabilities", plaintext, "", ""); code != http.StatusOK {
		t.Fatalf("capabilities with device token: %d %s", code, body)
	}
	if code, body := do(http.MethodGet, "/v1/capabilities", "itest-shared", otherUser, ""); code != http.StatusUnauthorized {
		t.Fatalf("retired shared token status: %d %s, want 401", code, body)
	}

	batchID := fmt.Sprintf("%08x-0202-4000-8000-%012x", run>>32, run&0xffffffffffff)
	batch := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"HKQuantityTypeIdentifierHeartRate","reason":"manual","exportedAt":1718000000000,"sampleCount":0,"deletionCount":0}
`, batchID)

	if code, body := do(http.MethodPost, "/v1/batches", plaintext, otherUser, batch); code != http.StatusForbidden {
		t.Fatalf("batch for another user: %d %s, want 403", code, body)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM batches WHERE batch_id = $1`, batchID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("batches rows after 403 = %d, %v; want 0", n, err)
	}

	if code, body := do(http.MethodPost, "/v1/batches", plaintext, "", batch); code != http.StatusOK {
		t.Fatalf("batch without header: %d %s", code, body)
	}
	var gotUser string
	var gotToken *int64
	if err := pool.QueryRow(ctx, `SELECT user_id::text, device_token_id FROM batches WHERE batch_id = $1`, batchID).
		Scan(&gotUser, &gotToken); err != nil {
		t.Fatalf("batches row: %v", err)
	}
	if gotUser != tokenUser || gotToken == nil || *gotToken != issued.ID {
		t.Fatalf("batches row user/token = %s/%v, want %s/%d", gotUser, gotToken, tokenUser, issued.ID)
	}

	// last_seen_at was stamped by the first request and is not advanced by
	// the next one inside the minute.
	var seen1 *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_seen_at FROM device_tokens WHERE id = $1`, issued.ID).Scan(&seen1); err != nil {
		t.Fatal(err)
	}
	if seen1 == nil {
		t.Fatal("last_seen_at is null after an authenticated request")
	}
	if code, _ := do(http.MethodGet, "/v1/stats", plaintext, tokenUser, ""); code != http.StatusOK {
		t.Fatalf("stats: %d", code)
	}
	var seen2 *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_seen_at FROM device_tokens WHERE id = $1`, issued.ID).Scan(&seen2); err != nil {
		t.Fatal(err)
	}
	if seen2 == nil || !seen2.Equal(*seen1) {
		t.Fatalf("last_seen_at advanced within a minute: %v -> %v", seen1, seen2)
	}

	// Revocation takes effect on the next request; retired credentials stay invalid.
	tokens, err := store.ListDeviceTokens(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, d := range tokens {
		if d.ID == issued.ID && d.Name == "itest phone" && d.LastSeenAt != nil {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("issued token not in the active listing: %+v", tokens)
	}
	if err := store.RenameDeviceToken(ctx, issued.ID, "itest phone (old)"); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeDeviceToken(ctx, issued.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(http.MethodGet, "/v1/capabilities", plaintext, "", ""); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d, want 401", code)
	}
	tokens, err = store.ListDeviceTokens(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range tokens {
		if d.ID == issued.ID {
			t.Fatal("revoked token still in the active listing")
		}
	}
	all, err := store.ListDeviceTokens(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var found *deviceToken
	for i := range all {
		if all[i].ID == issued.ID {
			found = &all[i]
		}
	}
	if found == nil || found.Status != "revoked" || found.RevokedAt == nil || found.Name != "itest phone (old)" {
		t.Fatalf("revoked row = %+v", found)
	}
	if err := store.RevokeDeviceToken(ctx, issued.ID+1_000_000); !errors.Is(err, errTokenNotFound) {
		t.Fatalf("revoking a missing id: %v, want errTokenNotFound", err)
	}
	if code, _ := do(http.MethodGet, "/v1/capabilities", "itest-shared", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("retired shared token after revoke: %d, want 401", code)
	}
}

// TestIntegration_InsertIntoCompressedChunk is R6's guard: a multi-year,
// oldest-first backfill keeps writing into chunks the columnstore policy has
// already compressed (anything older than 30 days). New samples must land
// there, and a re-sent sample — the anchor-after-ack retry, possibly with a
// differently cased UUID — must be a duplicate and a no-op: never an error,
// never a second row, never an overwrite.
func TestIntegration_InsertIntoCompressedChunk(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	run := time.Now().UnixNano()
	typeIdent := fmt.Sprintf("ITestCompressedIns%d", run)
	// A month per run (1971–2000, past every compression horizon) keeps the
	// rows apart from earlier runs against the same dev database; the type is
	// unique per run, so sharing a chunk with another test is harmless.
	monthStart := time.Date(1971+int(run/12%30), time.Month(1+run%12), 1, 0, 0, 0, 0, time.UTC)
	id := func(n int) string {
		return fmt.Sprintf("%08x-%04x-4000-8000-%012x", run>>32, n, run&0xffffffffffff)
	}
	sampleLine := func(uuid string, at time.Time, value float64) string {
		ms := at.UnixMilli()
		return fmt.Sprintf(`{"uuid":"%s","type":"%s","kind":"quantity","start":%d,"end":%d,"value":%g,"unit":"count","sourceName":"itest"}`,
			uuid, typeIdent, ms, ms, value)
	}
	insert := func(batchID string, lines ...string) IngestResult {
		t.Helper()
		body := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"%s","reason":"backfill","exportedAt":1718000000000,"sampleCount":%d,"deletionCount":0}`+"\n",
			batchID, typeIdent, len(lines)) + strings.Join(lines, "\n") + "\n"
		batch, err := parseReferenceBatch(strings.NewReader(body))
		if err != nil {
			t.Fatalf("ParseBatch: %v", err)
		}
		res, err := store.InsertBatch(ctx, batch, int64(len(body)))
		if err != nil {
			t.Fatalf("InsertBatch into a compressed chunk: %v", err)
		}
		return res
	}

	// Seed through the ingest path, then compress every chunk holding it.
	stored := []string{id(1), id(2), id(3)}
	var seed []string
	for i, u := range stored {
		seed = append(seed, sampleLine(u, monthStart.Add(time.Duration(i+1)*time.Hour), float64(i+1)))
	}
	if res := insert(id(0x100), seed...); res.Accepted != 3 {
		t.Fatalf("seed accepted = %d, want 3", res.Accepted)
	}
	admin := adminPool(t, ctx)
	seededEnd := monthStart.Add(24 * time.Hour)
	// Chunks by range overlap, as in TestIntegration_DeletionsOnCompressedChunk
	// (show_chunks' window must contain a chunk entirely).
	if _, err := admin.Exec(ctx, `
		SELECT compress_chunk(format('%I.%I', chunk_schema, chunk_name)::regclass,
		                      if_not_compressed => true)
		FROM timescaledb_information.chunks
		WHERE hypertable_schema = 'public' AND hypertable_name = 'quantity_samples'
		  AND range_end > $1::timestamptz AND range_start <= $2::timestamptz`,
		monthStart, seededEnd); err != nil {
		t.Fatalf("compress chunk: %v", err)
	}
	var uncompressed int
	if err := admin.QueryRow(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		WHERE hypertable_schema = 'public' AND hypertable_name = 'quantity_samples'
		  AND range_end > $1::timestamptz AND range_start <= $2::timestamptz
		  AND NOT is_compressed`, monthStart, seededEnd).Scan(&uncompressed); err != nil {
		t.Fatalf("verify compression: %v", err)
	}
	if uncompressed != 0 {
		t.Fatalf("%d chunk(s) holding the seeded rows are still uncompressed; the insert path under test would not touch the columnstore", uncompressed)
	}

	// Two new samples into the compressed range, plus a re-send of a stored
	// one: upper-cased, with a different value that must not overwrite.
	dup := strings.ToUpper(stored[0])
	page := []string{
		sampleLine(id(4), monthStart.Add(90*time.Minute), 4),
		sampleLine(id(5), monthStart.Add(150*time.Minute), 5),
		sampleLine(dup, monthStart.Add(time.Hour), 99),
	}
	res := insert(id(0x101), page...)
	if res.Accepted != 2 || res.Duplicates != 1 {
		t.Fatalf("into the compressed chunk: accepted %d, duplicates %d; want 2 and 1", res.Accepted, res.Duplicates)
	}
	// The same page again under a new batch id (a retry after a lost ack):
	// all duplicates. Under the same batch id: a duplicate batch.
	if res := insert(id(0x102), page...); res.Accepted != 0 || res.Duplicates != 3 {
		t.Fatalf("re-sent page: accepted %d, duplicates %d; want 0 and 3", res.Accepted, res.Duplicates)
	}
	assertStoredBatchReplay(t, res, insert(id(0x101), page...))

	var rows int
	var dupValue float64
	if err := pool.QueryRow(ctx, `
		SELECT count(*),
		       max(q.value) FILTER (WHERE q.uuid = $2::uuid)
		FROM quantity_samples q
		JOIN sample_types st USING (type_id)
		WHERE st.identifier = $1 AND q.start_ts >= $3 AND q.start_ts < $4`,
		typeIdent, stored[0], monthStart, seededEnd).Scan(&rows, &dupValue); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 5 {
		t.Errorf("rows = %d, want 5 (three seeded, two new, no second copy of the re-sent one)", rows)
	}
	if dupValue != 1 {
		t.Errorf("re-sent sample's value = %v, want the stored 1: a duplicate must never overwrite", dupValue)
	}
}

// TestIntegration_QuantityNearDuplicates is the guard against the 2026-09-04
// re-send: two client encoder versions sent the same samples with start times
// a microsecond apart, the (uuid, start_ts) primary key saw two rows, and
// 106,919 second copies were stored. A re-send whose start is within a
// millisecond of the stored row must be a duplicate — counted as one, never a
// second row — whether the row sits in a plain chunk or a compressed one,
// while new UUIDs in the same batch are still inserted.
func TestIntegration_QuantityNearDuplicates(t *testing.T) {
	url := integrationDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	run := time.Now().UnixNano()
	typeIdent := fmt.Sprintf("ITestNearDup%d", run)
	id := func(n int) string {
		return fmt.Sprintf("%08x-%04x-4000-8000-%012x", run>>32, n, run&0xffffffffffff)
	}
	sampleLine := func(uuid string, startMS float64, value float64) string {
		ms := strconv.FormatFloat(startMS, 'f', -1, 64)
		return fmt.Sprintf(`{"uuid":"%s","type":"%s","kind":"quantity","start":%s,"end":%s,"value":%g,"unit":"count","sourceName":"itest"}`,
			uuid, typeIdent, ms, ms, value)
	}
	insert := func(batchID string, lines ...string) IngestResult {
		t.Helper()
		body := fmt.Sprintf(`{"batchID":"%s","deviceID":"itest","type":"%s","reason":"backfill","exportedAt":1718000000000,"sampleCount":%d,"deletionCount":0}`+"\n",
			batchID, typeIdent, len(lines)) + strings.Join(lines, "\n") + "\n"
		batch, err := parseReferenceBatch(strings.NewReader(body))
		if err != nil {
			t.Fatalf("ParseBatch: %v", err)
		}
		res, err := store.InsertBatch(ctx, batch, int64(len(body)))
		if err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
		return res
	}
	rowsFor := func(uuid string) []time.Time {
		t.Helper()
		rows, err := pool.Query(ctx,
			`SELECT start_ts FROM quantity_samples WHERE uuid = $1 ORDER BY start_ts`, uuid)
		if err != nil {
			t.Fatalf("query rows: %v", err)
		}
		defer rows.Close()
		var out []time.Time
		for rows.Next() {
			var ts time.Time
			if err := rows.Scan(&ts); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, ts)
		}
		return out
	}
	// A microsecond on the wire, as the two encoders differed.
	const usMS = 0.001

	// checkCase seeds one sample at startMS, then re-sends it a microsecond
	// later and earlier (each its own batch, as a retry is) alongside a new
	// UUID, and repeats one UUID within a batch.
	checkCase := func(name string, startMS float64, firstID int, compress func()) {
		t.Run(name, func(t *testing.T) {
			stored := id(firstID)
			if res := insert(id(firstID+0x100), sampleLine(stored, startMS, 1)); res.Accepted != 1 {
				t.Fatalf("seed accepted = %d, want 1", res.Accepted)
			}
			if compress != nil {
				compress()
			}
			before := rowsFor(stored)
			if len(before) != 1 {
				t.Fatalf("seeded rows = %d, want 1", len(before))
			}

			fresh := id(firstID + 1)
			res := insert(id(firstID+0x101),
				sampleLine(strings.ToUpper(stored), startMS+usMS, 99),
				sampleLine(fresh, startMS+60_000, 2))
			if res.Accepted != 1 || res.Duplicates != 1 {
				t.Fatalf("re-send 1 µs later + a new sample: accepted %d, duplicates %d; want 1 and 1", res.Accepted, res.Duplicates)
			}
			if res := insert(id(firstID+0x102), sampleLine(stored, startMS-usMS, 99)); res.Accepted != 0 || res.Duplicates != 1 {
				t.Fatalf("re-send 1 µs earlier: accepted %d, duplicates %d; want 0 and 1", res.Accepted, res.Duplicates)
			}
			repeated := id(firstID + 2)
			if res := insert(id(firstID+0x103),
				sampleLine(repeated, startMS+120_000, 3),
				sampleLine(repeated, startMS+120_000+usMS, 3)); res.Accepted != 1 || res.Duplicates != 1 {
				t.Fatalf("one UUID twice in a batch: accepted %d, duplicates %d; want 1 and 1", res.Accepted, res.Duplicates)
			}

			if after := rowsFor(stored); len(after) != 1 || !after[0].Equal(before[0]) {
				t.Errorf("stored sample rows = %v, want only the original %v", after, before)
			}
			for _, u := range []string{fresh, repeated} {
				if got := len(rowsFor(u)); got != 1 {
					t.Errorf("rows for %s = %d, want 1", u, got)
				}
			}
		})
	}

	// Recent: an uncompressed chunk, start with a sub-millisecond fraction.
	recent := float64(time.Now().Add(-48*time.Hour).UnixMilli()) + 0.776
	checkCase("uncompressed", recent, 0x10, nil)

	// Historical: a chunk the columnstore policy would have compressed. A
	// month per run (1971–2000) keeps runs against one database apart.
	monthStart := time.Date(1971+int(run/12%30), time.Month(1+run%12), 1, 0, 0, 0, 0, time.UTC)
	old := float64(monthStart.Add(time.Hour).UnixMilli()) + 0.776
	admin := adminPool(t, ctx)
	checkCase("compressed", old, 0x20, func() {
		if _, err := admin.Exec(ctx, `
			SELECT compress_chunk(format('%I.%I', chunk_schema, chunk_name)::regclass,
			                      if_not_compressed => true)
			FROM timescaledb_information.chunks
			WHERE hypertable_schema = 'public' AND hypertable_name = 'quantity_samples'
			  AND range_end > $1::timestamptz AND range_start <= $2::timestamptz`,
			monthStart, monthStart.Add(24*time.Hour)); err != nil {
			t.Fatalf("compress chunk: %v", err)
		}
		var uncompressed int
		if err := admin.QueryRow(ctx, `
			SELECT count(*) FROM timescaledb_information.chunks
			WHERE hypertable_schema = 'public' AND hypertable_name = 'quantity_samples'
			  AND range_end > $1::timestamptz AND range_start <= $2::timestamptz
			  AND NOT is_compressed`, monthStart, monthStart.Add(24*time.Hour)).Scan(&uncompressed); err != nil {
			t.Fatalf("verify compression: %v", err)
		}
		if uncompressed != 0 {
			t.Fatalf("%d chunk(s) still uncompressed; the guard would not be tested against the columnstore", uncompressed)
		}
	})
}

// Direct SQL/storage reference fixtures name an owner; they are not authentication proofs.
func parseReferenceBatch(input io.Reader) (*Batch, error) {
	batch, err := ParseBatch(input)
	if batch != nil {
		batch.Header.UserID = referenceMemberID
	}
	return batch, err
}
