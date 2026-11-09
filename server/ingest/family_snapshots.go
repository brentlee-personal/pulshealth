package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type familyObservation struct {
	id       string
	sequence int64
}

type familySnapshotPosition struct {
	seriesID      *int16
	bucketStart   *time.Time
	day           *string
	profileSource *string
}

func familyActivityDate(localDate string, instant float64) string {
	if localDate != "" {
		return localDate
	}
	return msToTime(instant).UTC().Format(time.DateOnly)
}

func loadFamilyObservations(ctx context.Context, tx pgx.Tx, b *Batch, userID, sourceID, credentialID string) (map[string]familyObservation, error) {
	ids := b.Header.SourceObservations
	if len(ids) == 0 || len(ids) > maxBatchLines {
		return nil, errSourceSnapshot
	}
	seen := map[string]bool{}
	for _, id := range ids {
		canonical := strings.ToLower(id)
		if !isUUID(id) || seen[canonical] {
			return nil, errSourceSnapshot
		}
		seen[canonical] = true
	}
	rows, err := tx.Query(ctx, `SELECT query_key,observation_id::text,sequence_number
        FROM family_source_observations WHERE user_id=$1 AND source_connection_id=$2
        AND credential_id=$3 AND observation_id=ANY($4::uuid[]) AND expires_at>clock_timestamp()
        FOR SHARE`, userID, sourceID, credentialID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	observations := map[string]familyObservation{}
	for rows.Next() {
		var key string
		var observation familyObservation
		if err := rows.Scan(&key, &observation.id, &observation.sequence); err != nil {
			return nil, err
		}
		observations[key] = observation
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(observations) != len(ids) {
		return nil, errSourceSnapshot
	}
	return observations, nil
}

func normalizedFamilySnapshot(recordType, id string, value any, unit *string, start, end float64, startContext, endContext *TemporalContext) (familyRecordContent, []byte, []byte, error) {
	missing := unavailableFamilyFact("not_provided")
	content := familyRecordContent{SchemaVersion: "family-health-record-v1", Provider: "apple_health",
		RecordType: recordType, SourceRecordID: id, OriginalValue: value, Value: value, OriginalUnit: unit, Unit: unit,
		SourceDevice: missing, SourceApp: missing, SourceRevision: missing, Operation: "upsert",
		StartAt: familySourceTime(start), EndAt: familySourceTime(end)}
	content.SourceTimeZone, content.SourceOffset = familyTimeFacts(startContext)
	content.SourceEndTimeZone, content.SourceEndOffset = familyTimeFacts(endContext)
	return fingerprintFamilyRecord(content, nil)
}

func prepareFamilySnapshots(ctx context.Context, tx pgx.Tx, b *Batch, userID, sourceID, credentialID string,
	typeIDs map[string]int16, seriesIDs map[aggSeriesKey]int16) error {
	if len(b.Aggregates) == 0 && len(b.ActivitySummaries) == 0 && b.Profile == nil {
		return nil
	}
	observations, err := loadFamilyObservations(ctx, tx, b, userID, sourceID, credentialID)
	if err != nil {
		return err
	}
	readable := map[string]bool{}
	aggregateKeys := make([]string, len(b.Aggregates))
	for i, line := range b.Aggregates {
		a := line.Aggregate
		key, err := (sourceSnapshotQuery{Kind: "aggregate", Type: a.Type, Func: a.Func, IntervalValue: a.IntervalValue,
			IntervalUnit: a.IntervalUnit, DeviceFilter: a.DeviceFilter}).key()
		if err != nil {
			return errSourceSnapshot
		}
		aggregateKeys[i] = key
		if a.Value != nil {
			readable[key] = true
		}
	}
	activityKey, _ := (sourceSnapshotQuery{Kind: "activity_summary"}).key()
	for _, line := range b.ActivitySummaries {
		a := line.ActivitySummary
		if a.MoveKcal != nil || a.MoveGoalKcal != nil || a.ExerciseMin != nil || a.ExerciseGoalMin != nil ||
			a.StandHours != nil || a.StandGoalHours != nil || a.MoveTimeMin != nil || a.MoveTimeGoalMin != nil {
			readable[activityKey] = true
		}
	}
	claim := func(key string, content familyRecordContent, digest, facts []byte, position familySnapshotPosition) error {
		observation, ok := observations[key]
		if !ok {
			return errSourceSnapshot
		}
		accepted, err := claimFamilySnapshot(ctx, tx, b.Header.BatchID, userID, sourceID, credentialID, key, observation, content, digest, facts, position)
		if err != nil {
			return err
		}
		if accepted {
			b.familyCounts.accepted++
		} else {
			b.familyCounts.duplicate++
		}
		return nil
	}
	for i, line := range b.Aggregates {
		a := line.Aggregate
		key := aggregateKeys[i]
		if !readable[key] {
			return errSourceSnapshot
		}
		id, err := familyCompositeID([]any{a.Type, a.Func, a.IntervalValue, a.IntervalUnit, a.DeviceFilter, a.BucketStart})
		if err != nil {
			return err
		}
		content, digest, facts, err := normalizedFamilySnapshot("puls.v1.aggregate", id, a, a.Unit, a.BucketStart, a.BucketEnd, a.BucketStartContext, a.BucketEndContext)
		if err != nil {
			return err
		}
		series := seriesIDs[aggregateSeriesKey(&line, typeIDs)]
		start := msToTime(a.BucketStart)
		if err := claim(key, content, digest, facts, familySnapshotPosition{seriesID: &series, bucketStart: &start}); err != nil {
			return err
		}
	}
	for _, line := range b.ActivitySummaries {
		if !readable[activityKey] {
			return errSourceSnapshot
		}
		a := line.ActivitySummary
		day := familyActivityDate(a.LocalDate, a.Date)
		content, digest, facts, err := normalizedFamilySnapshot("HKActivitySummaryTypeIdentifier", day, a, nil, a.Date, a.Date, a.TemporalContext, a.TemporalContext)
		if err != nil {
			return err
		}
		var provenance map[string]any
		if err := json.Unmarshal(facts, &provenance); err != nil {
			return err
		}
		provenance["source_local_date"] = unavailableFamilyFact("not_provided")
		if a.LocalDate != "" {
			provenance["source_local_date"] = providedFamilyFact(a.LocalDate, "source_record", "explicit")
		}
		facts, err = json.Marshal(provenance)
		if err != nil {
			return err
		}
		if err := claim(activityKey, content, digest, facts, familySnapshotPosition{day: &day}); err != nil {
			return err
		}
	}
	if b.Profile != nil {
		p := b.Profile.Profile
		if p == nil || p.CapturedAt == nil {
			return errSourceSnapshot
		}
		key, _ := (sourceSnapshotQuery{Kind: "profile"}).key()
		value := *p
		value.CapturedAt = nil
		content, digest, facts, err := normalizedFamilySnapshot("puls.v1.profile", "profile", value, nil, *p.CapturedAt, *p.CapturedAt, nil, nil)
		if err != nil {
			return err
		}
		var provenance map[string]any
		if err := json.Unmarshal(facts, &provenance); err != nil {
			return err
		}
		provenance["time_basis"] = "source_snapshot_capture"
		facts, err = json.Marshal(provenance)
		if err != nil {
			return err
		}
		if err := claim(key, content, digest, facts, familySnapshotPosition{profileSource: &sourceID}); err != nil {
			return err
		}
	}
	return nil
}

func claimFamilySnapshot(ctx context.Context, tx pgx.Tx, batchID, userID, sourceID, credentialID, key string,
	observation familyObservation, content familyRecordContent, digest, facts []byte, position familySnapshotPosition) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO family_snapshot_identity
        (user_id,source_connection_id,record_type,source_record_id,payload_digest,query_key,
         observation_id,observation_sequence,source_facts,physical_series_id,physical_bucket_start,
         physical_date,physical_profile_source,first_credential_id,first_batch_id,last_credential_id,last_batch_id)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12::text::date,$13,$14,$15,$14,$15)
        ON CONFLICT(user_id,source_connection_id,record_type,source_record_id) DO NOTHING`,
		userID, sourceID, content.RecordType, content.SourceRecordID, digest, key, observation.id, observation.sequence, facts,
		position.seriesID, position.bucketStart, position.day, position.profileSource, credentialID, batchID)
	if err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23505" {
			return false, errRecordConflict
		}
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var previous []byte
	var sequence int64
	var previousObservation, lastBatch string
	if err := tx.QueryRow(ctx, `SELECT payload_digest,observation_sequence,observation_id::text,last_batch_id::text
        FROM family_snapshot_identity WHERE user_id=$1 AND source_connection_id=$2 AND record_type=$3 AND source_record_id=$4
        FOR UPDATE`, userID, sourceID, content.RecordType, content.SourceRecordID).
		Scan(&previous, &sequence, &previousObservation, &lastBatch); err != nil {
		return false, err
	}
	changed := !bytes.Equal(previous, digest)
	if observation.sequence < sequence || (observation.sequence == sequence &&
		(observation.id != previousObservation || (changed && !strings.EqualFold(lastBatch, batchID)))) {
		return false, errSourceSnapshot
	}
	_, err = tx.Exec(ctx, `UPDATE family_snapshot_identity SET payload_digest=$5,query_key=$6,
        observation_id=$7,observation_sequence=$8,source_facts=$9::jsonb,last_credential_id=$10,last_batch_id=$11,
        last_received_at=clock_timestamp()
        WHERE user_id=$1 AND source_connection_id=$2 AND record_type=$3 AND source_record_id=$4`,
		userID, sourceID, content.RecordType, content.SourceRecordID, digest, key, observation.id, observation.sequence, facts, credentialID, batchID)
	return changed, err
}

func upsertFamilyProfile(ctx context.Context, tx pgx.Tx, userID, sourceID string, line *ProfileLine) error {
	if line == nil {
		return nil
	}
	p := line.Profile
	_, err := tx.Exec(ctx, `INSERT INTO family_source_profiles(user_id,source_connection_id,name,email,dob_epoch_ms,biological_sex)
        VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(user_id,source_connection_id) DO UPDATE SET
        name=EXCLUDED.name,email=EXCLUDED.email,dob_epoch_ms=EXCLUDED.dob_epoch_ms,biological_sex=EXCLUDED.biological_sex`,
		userID, sourceID, p.Name, p.Email, p.DateOfBirth, p.BiologicalSex)
	return err
}
