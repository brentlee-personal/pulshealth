package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gowebpki/jcs"
	"github.com/jackc/pgx/v5"
)

var errSourceSnapshot = errors.New("mutable source snapshot requires a verified observation")

func familyWorkoutReference(ctx context.Context, tx pgx.Tx, userID, sourceID, rawID string) (string, error) {
	_, err := tx.Exec(ctx, `INSERT INTO family_workout_references
		(user_id,source_connection_id,source_workout_uuid,physical_uuid)
		VALUES($1,$2,$3,coalesce((SELECT physical_uuid FROM family_record_identity
			WHERE user_id=$1 AND source_connection_id=$2 AND record_type='HKWorkoutTypeIdentifier'
			AND source_record_id=$4),gen_random_uuid()))
		ON CONFLICT(user_id,source_connection_id,source_workout_uuid) DO NOTHING`,
		userID, sourceID, rawID, strings.ToLower(rawID))
	if err != nil {
		return "", err
	}
	var physical string
	err = tx.QueryRow(ctx, `SELECT physical_uuid::text FROM family_workout_references
		WHERE user_id=$1 AND source_connection_id=$2 AND source_workout_uuid=$3`, userID, sourceID, rawID).Scan(&physical)
	return physical, err
}

func familySourceDeleted(ctx context.Context, tx pgx.Tx, userID, sourceID, rawID string) (bool, error) {
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM family_source_tombstones
		WHERE user_id=$1 AND source_connection_id=$2 AND source_record_uuid=$3)`, userID, sourceID, rawID).Scan(&deleted)
	return deleted, err
}

func prepareFamilyRecords(ctx context.Context, tx pgx.Tx, original *Batch, userID, sourceID, credentialID string) (*Batch, error) {
	if len(original.Aggregates) > 0 || len(original.ActivitySummaries) > 0 || original.Profile != nil {
		return nil, errSourceSnapshot
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,360006))", userID+":"+sourceID); err != nil {
		return nil, err
	}
	prepared := *original
	prepared.Samples = nil
	prepared.Routes = nil
	prepared.Series = nil
	prepared.Deletions = nil
	for _, deletion := range original.Deletions {
		n, err := deleteFamilySourceRecord(ctx, tx, userID, sourceID, original.Header.BatchID, deletion.Deleted.UUID)
		if err != nil {
			return nil, err
		}
		prepared.sourceDeleted += n
	}
	for i := range original.Samples {
		sample := original.Samples[i]
		deleted, err := familySourceDeleted(ctx, tx, userID, sourceID, sample.UUID)
		if err != nil {
			return nil, err
		}
		if deleted {
			prepared.suppressedSamples++
			continue
		}
		content, digest, facts, err := normalizeFamilySample(&sample)
		if err != nil {
			return nil, err
		}
		table, _, ok := tableForKind(sample.Kind)
		if !ok {
			return nil, errFamilyRecord
		}
		var unbound bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+` h
			WHERE h.user_id=$1 AND h.uuid=$2 AND NOT EXISTS(SELECT 1 FROM family_record_identity r
				WHERE r.user_id=h.user_id AND r.physical_table=$3 AND r.physical_uuid=h.uuid))`,
			userID, sample.UUID, table).Scan(&unbound); err != nil {
			return nil, err
		}
		if unbound {
			return nil, errSourceBinding
		}
		position := familyRecordPosition{table: table, rawSourceID: sample.UUID, start: msToTime(sample.Start), end: msToTime(sample.End)}
		if sample.Kind == "workout" {
			parent, err := familyWorkoutReference(ctx, tx, userID, sourceID, sample.UUID)
			if err != nil {
				return nil, err
			}
			position.uuid = &parent
		}
		physical, err := claimFamilyRecord(ctx, tx, original.Header.BatchID, userID, sourceID, credentialID, content, digest, facts, position)
		if err != nil {
			return nil, err
		}
		if physical == nil {
			return nil, errRecordConflict
		}
		sample.UUID = *physical
		prepared.Samples = append(prepared.Samples, sample)
	}
	for _, line := range original.Routes {
		rawParent := line.Route.WorkoutUUID
		deleted, err := familySourceDeleted(ctx, tx, userID, sourceID, rawParent)
		if err != nil {
			return nil, err
		}
		if deleted {
			continue
		}
		parent, err := familyWorkoutReference(ctx, tx, userID, sourceID, rawParent)
		if err != nil {
			return nil, err
		}
		for _, point := range line.Route.Points {
			if err := refuseUnboundFamilyPoint(ctx, tx, userID, sourceID, parent, "workout_route_points", "", point.T); err != nil {
				return nil, err
			}
			id, err := familyCompositeID([]any{strings.ToLower(rawParent), point.T})
			if err != nil {
				return nil, err
			}
			value := struct {
				WorkoutUUID string     `json:"workoutUUID"`
				Point       RoutePoint `json:"point"`
			}{strings.ToLower(rawParent), point}
			content, digest, facts, err := familyPointRecord("puls.v1.route", id, value, nil, point.T, point.TemporalContext)
			if err != nil {
				return nil, err
			}
			position := familyRecordPosition{table: "workout_route_points", rawSourceID: id, parentUUID: &parent, start: msToTime(point.T), end: msToTime(point.T)}
			if _, err := claimFamilyRecord(ctx, tx, original.Header.BatchID, userID, sourceID, credentialID, content, digest, facts, position); err != nil {
				return nil, err
			}
		}
		line.Route.WorkoutUUID = parent
		prepared.Routes = append(prepared.Routes, line)
	}
	for _, line := range original.Series {
		rawParent := line.Series.WorkoutUUID
		deleted, err := familySourceDeleted(ctx, tx, userID, sourceID, rawParent)
		if err != nil {
			return nil, err
		}
		if deleted {
			continue
		}
		parent, err := familyWorkoutReference(ctx, tx, userID, sourceID, rawParent)
		if err != nil {
			return nil, err
		}
		for _, point := range line.Series.Points {
			if err := refuseUnboundFamilyPoint(ctx, tx, userID, sourceID, parent, "workout_series_points", line.Series.Type, point.T); err != nil {
				return nil, err
			}
			id, err := familyCompositeID([]any{strings.ToLower(rawParent), line.Series.Type, point.T})
			if err != nil {
				return nil, err
			}
			value := struct {
				WorkoutUUID string      `json:"workoutUUID"`
				Type        string      `json:"type"`
				Point       SeriesPoint `json:"point"`
			}{strings.ToLower(rawParent), line.Series.Type, point}
			content, digest, facts, err := familyPointRecord("puls.v1.series", id, value, line.Series.Unit, point.T, point.TemporalContext)
			if err != nil {
				return nil, err
			}
			position := familyRecordPosition{table: "workout_series_points", rawSourceID: id, parentUUID: &parent, typeIdentifier: &line.Series.Type, start: msToTime(point.T), end: msToTime(point.T)}
			if _, err := claimFamilyRecord(ctx, tx, original.Header.BatchID, userID, sourceID, credentialID, content, digest, facts, position); err != nil {
				return nil, err
			}
		}
		line.Series.WorkoutUUID = parent
		prepared.Series = append(prepared.Series, line)
	}
	return &prepared, nil
}

func refuseUnboundFamilyPoint(ctx context.Context, tx pgx.Tx, userID, sourceID, parent, table, identifier string, instant float64) error {
	var query string
	switch table {
	case "workout_route_points":
		query = `SELECT EXISTS(SELECT 1 FROM workout_route_points p WHERE p.user_id=$1 AND p.workout_uuid=$3 AND p.ts=$4
			AND NOT EXISTS(SELECT 1 FROM family_record_identity r WHERE r.user_id=p.user_id AND r.source_connection_id=$2
				AND r.physical_table='workout_route_points' AND r.physical_parent_uuid=p.workout_uuid AND r.physical_start=p.ts))`
	case "workout_series_points":
		query = `SELECT EXISTS(SELECT 1 FROM workout_series_points p JOIN sample_types t USING(type_id)
			WHERE p.user_id=$1 AND p.workout_uuid=$3 AND p.ts=$4 AND t.identifier=$5
			AND NOT EXISTS(SELECT 1 FROM family_record_identity r WHERE r.user_id=p.user_id AND r.source_connection_id=$2
				AND r.physical_table='workout_series_points' AND r.physical_parent_uuid=p.workout_uuid
				AND r.physical_start=p.ts AND r.physical_type=t.identifier))`
	default:
		return errFamilyRecord
	}
	args := []any{userID, sourceID, parent, msToTime(instant)}
	if table == "workout_series_points" {
		args = append(args, identifier)
	}
	var unbound bool
	if err := tx.QueryRow(ctx, query, args...).Scan(&unbound); err != nil {
		return err
	}
	if unbound {
		return errSourceBinding
	}
	return nil
}

func familyCompositeID(parts []any) (string, error) {
	data, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	canonical, err := jcs.Transform(data)
	return string(canonical), err
}

func familyPointRecord(recordType, id string, value any, unit *string, instant float64, context *TemporalContext) (familyRecordContent, []byte, []byte, error) {
	missing := unavailableFamilyFact("not_provided")
	content := familyRecordContent{SchemaVersion: "family-health-record-v1", Provider: "apple_health", SourceRecordID: id, RecordType: recordType,
		SourceDevice: missing, SourceApp: missing, OriginalValue: value, Value: value, OriginalUnit: unit, Unit: unit,
		StartAt: familySourceTime(instant), EndAt: familySourceTime(instant),
		SourceRevision: missing, Operation: "upsert"}
	content.SourceTimeZone, content.SourceOffset = familyTimeFacts(context)
	content.SourceEndTimeZone, content.SourceEndOffset = familyTimeFacts(context)
	return fingerprintFamilyRecord(content, nil)
}
