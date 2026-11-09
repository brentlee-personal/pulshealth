package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gowebpki/jcs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var errRecordConflict = errors.New("record identity conflicts with source history")

type familySourceFact struct {
	Status     string `json:"status"`
	Value      any    `json:"value,omitempty"`
	Origin     string `json:"origin,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func unavailableFamilyFact(reason string) familySourceFact {
	return familySourceFact{Status: "unavailable", Reason: reason}
}

func providedFamilyFact(value any, origin, confidence string) familySourceFact {
	return familySourceFact{Status: "provided", Value: value, Origin: origin, Confidence: confidence}
}

func familyStringFact(value *string) familySourceFact {
	if value == nil || *value == "" {
		return unavailableFamilyFact("not_provided")
	}
	return providedFamilyFact(*value, "source_record", "explicit")
}

func familyTimeFacts(context *TemporalContext) (familySourceFact, familySourceFact) {
	missing := unavailableFamilyFact("not_provided")
	if context == nil {
		return missing, missing
	}
	origin, confidence := "source_record", "explicit"
	switch {
	case context.Source == "healthkit_metadata" && context.Confidence == "recorded":
	case context.Source == "device_current" && context.Confidence == "inferred":
		origin, confidence = "device_current", "inferred"
	default:
		unknown := unavailableFamilyFact("not_verifiable")
		return unknown, unknown
	}
	offset := providedFamilyFact(context.UTCOffsetSeconds, origin, confidence)
	if context.TimeZoneID == "Local" {
		return unavailableFamilyFact("not_verifiable"), offset
	}
	if _, err := time.LoadLocation(context.TimeZoneID); err != nil {
		return unavailableFamilyFact("not_verifiable"), offset
	}
	return providedFamilyFact(context.TimeZoneID, origin, confidence), offset
}

// Authentication and receipt fields are excluded from record content hashes.
type familyRecordContent struct {
	SchemaVersion     string           `json:"schema_version"`
	Provider          string           `json:"provider"`
	SourceRecordID    string           `json:"source_record_id"`
	RecordType        string           `json:"record_type"`
	SourceDevice      familySourceFact `json:"source_device"`
	SourceApp         familySourceFact `json:"source_app"`
	OriginalValue     any              `json:"original_value"`
	OriginalUnit      *string          `json:"original_unit"`
	Value             any              `json:"value"`
	Unit              *string          `json:"unit"`
	StartAt           string           `json:"start_at"`
	EndAt             string           `json:"end_at"`
	SourceTimeZone    familySourceFact `json:"source_time_zone"`
	SourceOffset      familySourceFact `json:"source_offset"`
	SourceEndTimeZone familySourceFact `json:"source_end_time_zone"`
	SourceEndOffset   familySourceFact `json:"source_end_offset"`
	SourceRevision    familySourceFact `json:"source_revision"`
	Operation         string           `json:"operation"`
}

func normalizeFamilySample(sample *Sample) (familyRecordContent, []byte, []byte, error) {
	var value any
	var unit *string
	switch sample.Kind {
	case "quantity":
		value, unit = *sample.Value, sample.Unit
	case "category":
		value = *sample.Category
	case "workout":
		value = sample.Workout
	case "heartbeatSeries":
		value = sample.Heartbeats
	case "ecg":
		value = sample.ECG
	case "stateOfMind":
		value = sample.StateOfMind
	case "medicationDose":
		value = sample.MedicationDose
	default:
		return familyRecordContent{}, nil, nil, errFamilyRecord
	}
	app := sample.SourceBundleID
	if app == nil || *app == "" {
		app = sample.SourceName
	}
	content := familyRecordContent{
		SchemaVersion: "family-health-record-v1", Provider: "apple_health",
		SourceRecordID: strings.ToLower(sample.UUID), RecordType: sample.Type,
		SourceDevice: familyStringFact(sample.Device), SourceApp: familyStringFact(app),
		OriginalValue: value, Value: value, OriginalUnit: unit, Unit: unit,
		StartAt:        msToTime(sample.Start).UTC().Format(time.RFC3339Nano),
		EndAt:          msToTime(sample.End).UTC().Format(time.RFC3339Nano),
		SourceRevision: unavailableFamilyFact("not_provided"), Operation: "upsert",
	}
	content.SourceTimeZone, content.SourceOffset = familyTimeFacts(sample.StartContext)
	content.SourceEndTimeZone, content.SourceEndOffset = familyTimeFacts(sample.EndContext)
	data, err := json.Marshal(content)
	if err != nil {
		return content, nil, nil, err
	}
	canonical, err := jcs.Transform(data)
	if err != nil {
		return content, nil, nil, err
	}
	digest := sha256.Sum256(canonical)
	var facts map[string]any
	if err := json.Unmarshal(data, &facts); err != nil {
		return content, nil, nil, err
	}
	delete(facts, "value")
	delete(facts, "original_value")
	// Creator app versions are trace facts, not provider record revisions.
	facts["source_app_version"] = familyStringFact(sample.SourceVersion)
	factsJSON, err := json.Marshal(facts)
	return content, digest[:], factsJSON, err
}

func claimFamilySamples(ctx context.Context, tx pgx.Tx, batch *Batch, userID, sourceID, credentialID string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 360006))", userID+":"+sourceID); err != nil {
		return err
	}
	for i := range batch.Samples {
		sample := &batch.Samples[i]
		content, digest, facts, err := normalizeFamilySample(sample)
		if err != nil {
			return err
		}
		table, _, ok := tableForKind(sample.Kind)
		if !ok {
			return errFamilyRecord
		}
		var unbound bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+` h
			WHERE h.user_id=$1 AND h.uuid=$2
			AND NOT EXISTS(SELECT 1 FROM family_record_identity r
				WHERE r.user_id=h.user_id AND r.physical_table=$3 AND r.physical_uuid=h.uuid))`,
			userID, sample.UUID, table).Scan(&unbound); err != nil {
			return err
		}
		if unbound {
			return errSourceBinding
		}
		// shortcut: raw UUIDs remain physical IDs until source-scoped remapping;
		// collisions fail closed instead of silently dropping another source.
		_, err = tx.Exec(ctx, `INSERT INTO family_record_identity
			(user_id,source_connection_id,record_type,source_record_id,raw_source_record_id,
			 payload_digest,physical_table,physical_uuid,physical_start,physical_end,
			 source_facts,first_credential_id,first_batch_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13)
			ON CONFLICT(user_id,source_connection_id,record_type,source_record_id) DO NOTHING`,
			userID, sourceID, sample.Type, content.SourceRecordID, sample.UUID, digest, table,
			sample.UUID, msToTime(sample.Start), msToTime(sample.End), facts, credentialID, batch.Header.BatchID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return errRecordConflict
			}
			return err
		}
		var stored []byte
		if err := tx.QueryRow(ctx, `SELECT payload_digest FROM family_record_identity
			WHERE user_id=$1 AND source_connection_id=$2 AND record_type=$3 AND source_record_id=$4`,
			userID, sourceID, sample.Type, content.SourceRecordID).Scan(&stored); err != nil {
			return err
		}
		if !bytes.Equal(stored, digest) {
			return errRecordConflict
		}
	}
	return nil
}
