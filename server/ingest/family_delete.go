package main

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func deleteFamilySourceRecord(ctx context.Context, tx pgx.Tx, userID, sourceID, batchID, rawID string) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO family_source_tombstones
		(user_id,source_connection_id,source_record_uuid,first_batch_id) VALUES($1,$2,$3,$4)
		ON CONFLICT(user_id,source_connection_id,source_record_uuid) DO NOTHING`, userID, sourceID, rawID, batchID); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT physical_table,physical_uuid::text,physical_start
		FROM family_record_identity WHERE user_id=$1 AND source_connection_id=$2
		AND source_record_id=$3 AND physical_uuid IS NOT NULL`, userID, sourceID, strings.ToLower(rawID))
	if err != nil {
		return 0, err
	}
	type location struct {
		table, uuid string
		start       time.Time
	}
	var positions []location
	for rows.Next() {
		var p location
		if err := rows.Scan(&p.table, &p.uuid, &p.start); err != nil {
			rows.Close()
			return 0, err
		}
		positions = append(positions, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var deleted int64
	for _, p := range positions {
		switch p.table {
		case "quantity_samples", "category_samples", "workouts", "heartbeat_series", "ecg_samples", "state_of_mind", "medication_dose_events":
		default:
			return 0, errRecordConflict
		}
		tag, err := tx.Exec(ctx, "DELETE FROM "+p.table+" WHERE user_id=$1 AND uuid=$2 AND start_ts=$3", userID, p.uuid, p.start)
		if err != nil {
			return 0, err
		}
		deleted += tag.RowsAffected()
	}
	var parent *string
	if err := tx.QueryRow(ctx, `SELECT (SELECT physical_uuid::text FROM family_workout_references
		WHERE user_id=$1 AND source_connection_id=$2 AND source_workout_uuid=$3)`,
		userID, sourceID, rawID).Scan(&parent); err != nil {
		return 0, err
	}
	if parent != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM workout_route_points p USING family_record_identity r
			WHERE p.user_id=$1 AND p.workout_uuid=$3 AND r.user_id=p.user_id AND r.source_connection_id=$2
			AND r.physical_table='workout_route_points' AND r.physical_parent_uuid=p.workout_uuid
			AND r.physical_start=p.ts`, userID, sourceID, *parent); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workout_series_points p USING family_record_identity r,sample_types t
			WHERE p.user_id=$1 AND p.workout_uuid=$3 AND r.user_id=p.user_id AND r.source_connection_id=$2
			AND r.physical_table='workout_series_points' AND r.physical_parent_uuid=p.workout_uuid
			AND r.physical_start=p.ts AND t.identifier=r.physical_type AND t.type_id=p.type_id`,
			userID, sourceID, *parent); err != nil {
			return 0, err
		}
	}
	return deleted, nil
}
