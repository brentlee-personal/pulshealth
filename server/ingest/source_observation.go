package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/gowebpki/jcs"
	"github.com/jackc/pgx/v5"
)

var errSourceQuery = errors.New("invalid source observation query")

type sourceSnapshotQuery struct {
	Kind          string `json:"kind"`
	Type          string `json:"type,omitempty"`
	Func          string `json:"func,omitempty"`
	IntervalValue int    `json:"intervalValue,omitempty"`
	IntervalUnit  string `json:"intervalUnit,omitempty"`
	DeviceFilter  string `json:"deviceFilter,omitempty"`
}

func (q sourceSnapshotQuery) key() (string, error) {
	switch q.Kind {
	case "profile", "activity_summary":
		if q.Type != "" || q.Func != "" || q.IntervalValue != 0 || q.IntervalUnit != "" || q.DeviceFilter != "" {
			return "", errSourceQuery
		}
		return familyCompositeID([]any{q.Kind})
	case "aggregate":
		contract, err := familyProtocol()
		if err != nil {
			return "", err
		}
		item, ok := contract.types[q.Type]
		if !ok || !slices.Contains(item.AllowedAggregateFunctions, q.Func) || q.IntervalValue < 1 || q.IntervalValue > 1<<31-1 ||
			!slices.Contains([]string{"minute", "hour", "day", "week", "month"}, q.IntervalUnit) ||
			!slices.Contains([]string{"all", "watch", "iphone"}, q.DeviceFilter) {
			return "", errSourceQuery
		}
		return familyCompositeID([]any{q.Kind, q.Type, q.Func, q.IntervalValue, q.IntervalUnit, q.DeviceFilter})
	default:
		return "", errSourceQuery
	}
}

type familyWriter struct {
	sourceID     string
	credentialID string
	generation   int64
}

func lockFamilyWriter(ctx context.Context, tx pgx.Tx, userID string, tokenID int64) (familyWriter, error) {
	var writer familyWriter
	var owner string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE id=$1 FOR KEY SHARE`, userID).Scan(&owner); err != nil {
		return writer, fmt.Errorf("lock authenticated user: %w", err)
	}
	var id int64
	var source *string
	if err := tx.QueryRow(ctx, `SELECT id,device_generation,source_connection_id::text,credential_id::text
        FROM device_tokens WHERE id=$1 AND user_id=$2 AND status='active' FOR SHARE`, tokenID, userID).
		Scan(&id, &writer.generation, &source, &writer.credentialID); err != nil {
		return writer, fmt.Errorf("revalidate device token: %w", err)
	}
	if source == nil {
		return writer, errSourceBinding
	}
	writer.sourceID = *source
	var active bool
	if err := tx.QueryRow(ctx, `SELECT family_apple_source_active($1,$2)`, userID, writer.sourceID).Scan(&active); err != nil {
		return writer, fmt.Errorf("revalidate source connection: %w", err)
	}
	if !active {
		return writer, errSourceBinding
	}
	return writer, nil
}

func (st *Store) BeginSourceObservation(ctx context.Context, userID string, tokenID int64, queryKey string) (string, error) {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	writer, err := lockFamilyWriter(ctx, tx, userID, tokenID)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `WITH issued AS (SELECT clock_timestamp() AS at)
        INSERT INTO family_source_observations
        (user_id,source_connection_id,query_key,credential_id,issued_at,expires_at)
        SELECT $1,$2,$3,$4,at,at+interval '5 minutes' FROM issued
        ON CONFLICT(user_id,source_connection_id,query_key) DO UPDATE SET
          observation_id=gen_random_uuid(),credential_id=EXCLUDED.credential_id,
          sequence_number=family_source_observations.sequence_number+1,
          issued_at=EXCLUDED.issued_at,expires_at=EXCLUDED.expires_at
        RETURNING observation_id::text`, userID, writer.sourceID, queryKey, writer.credentialID).Scan(&id)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Server) handleSourceObservation(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		status := http.StatusBadRequest
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, map[string]string{"error": errSourceQuery.Error()})
		return
	}
	if _, err := jcs.Transform(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errSourceQuery.Error()})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var query sourceSnapshotQuery
	if err := decoder.Decode(&query); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errSourceQuery.Error()})
		return
	}
	key, err := query.key()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errSourceQuery.Error()})
		return
	}
	p, _ := principalFrom(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id, err := s.store.BeginSourceObservation(ctx, p.userID, p.tokenID, key)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, errSourceBinding) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": "source observation unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"observation_id": id})
}
