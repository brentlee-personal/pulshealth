package main

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultUserID is the seeded default user (see db/migrations/000_users.sql). Batches
// that arrive without an X-User-ID header are attributed to it, and it is the
// column DEFAULT on every data table.
const defaultUserID = "5ea4d000-0000-4000-8000-000000000001"

// Store wraps the pgx pool with batch-ingest logic.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// IngestResult mirrors the wire response.
type IngestResult struct {
	receipt           *committedBatchReceipt
	FamilyReceipt     *FamilyBatchReceipt `json:"receipt,omitempty"`
	Accepted          int64               `json:"accepted"`
	Deleted           int64               `json:"deleted"`
	Duplicates        int64               `json:"duplicates"`
	RoutePoints       int64               `json:"routePoints"`
	SeriesPoints      int64               `json:"seriesPoints"`
	AggregateSamples  int64               `json:"aggregateSamples"`
	ActivitySummaries int64               `json:"activitySummaries"`
	DuplicateBatch    bool                `json:"-"`
	// Server-side re-runs after a lost deadlock/serialization race (see InsertBatch).
	Retries int `json:"-"`
}

// IngestRejection is the durable operational record for an authenticated batch
// request that failed before a row could be committed to batches.
type IngestRejection struct {
	BatchID         string
	UserID          string
	WakeID          string
	Trigger         string
	Status          int
	Stage           string
	ErrorMessage    string
	Bytes           int64
	ContentEncoding string
}

// RecordRejection persists failure metadata only; it never stores request-body
// health data. Best-effort callers must preserve the original HTTP response if
// this operational write itself fails.
func (st *Store) RecordRejection(ctx context.Context, r IngestRejection) error {
	// A request can finish parsing after its account has been erased. Keep
	// diagnostics only for an existing owner, and hold the same user lock as
	// ingestion so a concurrent purge either removes this row or precedes it.
	// Invalid/unknown identity failures remain in the bounded service log.
	if !isUUID(r.UserID) {
		return nil
	}
	_, err := st.pool.Exec(ctx, `
		WITH owner AS MATERIALIZED (
		    SELECT id FROM users WHERE id = $2::uuid FOR KEY SHARE
		)
		INSERT INTO ingest_rejections
		    (batch_id, user_id, wake_id, trigger, status, stage,
		     error_message, bytes, content_encoding)
		SELECT NULLIF($1, ''), owner.id::text, NULLIF($3, ''), NULLIF($4, ''),
		       $5, $6, $7, $8, NULLIF($9, '') FROM owner`,
		r.BatchID, r.UserID, r.WakeID, r.Trigger, r.Status, r.Stage,
		r.ErrorMessage, r.Bytes, r.ContentEncoding)
	if err != nil {
		return fmt.Errorf("record ingest rejection: %w", err)
	}
	return nil
}

// RouteFilters bounds the workout route list endpoint.
type RouteFilters struct {
	Start        *time.Time
	End          *time.Time
	ActivityType string
	MinDistanceM *float64
	MaxDistanceM *float64
	Limit        int
	Offset       int
}

// RouteSummary is the compact workout route shape consumed by external route tools.
type RouteSummary struct {
	UUID             string   `json:"uuid"`
	StartTs          string   `json:"startTs"`
	EndTs            string   `json:"endTs"`
	ActivityType     string   `json:"activityType"`
	DurationS        *float64 `json:"durationS"`
	DistanceM        *float64 `json:"distanceM"`
	EnergyKcal       *float64 `json:"energyKcal"`
	RoutePointCount  int64    `json:"routePointCount"`
	Bounds           Bounds   `json:"bounds"`
	AvailableMetrics []string `json:"availableMetrics"`
}

type Bounds struct {
	MinLat float64 `json:"minLat"`
	MaxLat float64 `json:"maxLat"`
	MinLon float64 `json:"minLon"`
	MaxLon float64 `json:"maxLon"`
}

type RouteGPSPoint struct {
	Ts        string   `json:"ts"`
	Lat       float64  `json:"lat"`
	Lon       float64  `json:"lon"`
	AltitudeM *float64 `json:"altitudeM,omitempty"`
	HAccM     *float64 `json:"hAccM,omitempty"`
	VAccM     *float64 `json:"vAccM,omitempty"`
	SpeedMps  *float64 `json:"speedMps,omitempty"`
	CourseDeg *float64 `json:"courseDeg,omitempty"`
}

type RouteDetail struct {
	Workout RouteSummary    `json:"workout"`
	Points  []RouteGPSPoint `json:"points"`
}

type MetricPoint struct {
	Ts    string  `json:"ts"`
	Value float64 `json:"value"`
}

type RouteMetricSeries struct {
	Identifier string        `json:"identifier"`
	Unit       *string       `json:"unit"`
	Points     []MetricPoint `json:"points"`
}

// maxInsertRetries bounds server-side re-runs of a batch whose transaction
// lost a deadlock or serialization race (see InsertBatch).
const maxInsertRetries = 3

var errSourceBinding = errors.New("source connection requires verified pairing")

// InsertBatch applies one batch atomically: lazily registers types and
// sources, bulk-inserts samples via unnest with ON CONFLICT DO NOTHING
// (idempotent for client retries), applies deletions, and records the batch.
//
// Concurrent batches can still deadlock (SQLSTATE 40P01) on shared lookup
// rows or overlapping sample sets despite the deterministic lock ordering in
// ensureTypes/ensureSources — the phone's post-reboot backlog on 2026-09-05
// produced five in two minutes. Each was a 500 the phone had to resend (and a
// row toward the rejection alert) for a transient the server can resolve
// itself: the loser is rolled back cleanly and the whole batch is one
// idempotent transaction, so it is simply run again after a short jittered
// pause. Bounded by the handler's insert deadline via ctx.
func (st *Store) InsertBatch(ctx context.Context, b *Batch, bodyBytes int64) (IngestResult, error) {
	if b.Header.DeviceTokenID != 0 && !b.familyValidated {
		return IngestResult{}, parseErrf("personal batch requires family protocol validation")
	}
	for attempt := 0; ; attempt++ {
		res, err := st.insertBatchOnce(ctx, b, bodyBytes)
		res.Retries = attempt
		if err == nil || attempt >= maxInsertRetries || !isRetryableTxError(err) {
			return res, err
		}
		delay := time.Duration(50<<attempt)*time.Millisecond + time.Duration(rand.IntN(50))*time.Millisecond
		select {
		case <-ctx.Done():
			return res, err
		case <-time.After(delay):
		}
	}
}

// isRetryableTxError reports whether err (possibly wrapped) is a Postgres
// deadlock_detected or serialization_failure, i.e. a race the transaction lost
// through no fault of its input and would not hit again when re-run alone.
func isRetryableTxError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40P01" || pgErr.Code == "40001"
}

func (st *Store) insertBatchOnce(ctx context.Context, b *Batch, bodyBytes int64) (IngestResult, error) {
	var res IngestResult

	tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// quantity_samples chunks older than 30 days are compressed; DML touching
	// them decompresses columnstore batches, and TimescaleDB aborts the
	// transaction once that exceeds a tuple limit (default 100k, SQLSTATE
	// 53400). Deletes are kept prunable below, but lift the cap so an
	// unusually wide batch degrades to slow instead of erroring.
	if _, err := tx.Exec(ctx,
		`SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0`); err != nil {
		return res, fmt.Errorf("set decompression limit: %w", err)
	}

	// Reserve the batch id before touching health data. Concurrent retries block
	// on this insert; after the first transaction commits they observe the
	// conflict and exit without replaying upserts, deletions, or profile changes.
	userID := b.Header.UserID
	var generation int64
	var sourceConnectionID *string
	var credentialID string
	if userID == "" {
		userID = defaultUserID
	}
	if b.Header.DeviceTokenID != 0 {
		// Prevent account deletion during authenticated writes without serializing
		// concurrent uploads or ordinary profile updates. Never create
		// a missing device-token owner: authentication may have happened before
		// deletion committed while the request body was still uploading.
		writer, err := lockFamilyWriter(ctx, tx, userID, b.Header.DeviceTokenID)
		if err != nil {
			return res, err
		}
		generation, credentialID, sourceConnectionID = writer.generation, writer.credentialID, &writer.sourceID
	} else if err := ensureUser(ctx, tx, userID); err != nil {
		// Self-hosted shared tokens retain their legacy create-on-first-upload behavior.
		return res, fmt.Errorf("ensure user: %w", err)
	}
	cached, err := reserveBatchReceipt(ctx, tx, b, userID, generation)
	if err != nil {
		return res, err
	}
	if cached != nil {
		if err := tx.Commit(ctx); err != nil {
			return res, fmt.Errorf("commit replay: %w", err)
		}
		return *cached, nil
	}
	var wakeID, trigger, deviceTokenID any
	if b.Header.WakeID != "" {
		wakeID = b.Header.WakeID
	}
	if b.Header.Trigger != "" {
		trigger = b.Header.Trigger
	}
	if b.Header.DeviceTokenID != 0 {
		deviceTokenID = b.Header.DeviceTokenID
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO batches (batch_id, device_id, user_id, type_identifier, reason,
		                     sample_count, deletion_count, aggregate_count,
		                     activity_summary_count, bytes, exported_at,
		                     wake_id, trigger, parse_ms, insert_ms, device_token_id, device_generation)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NULL, $15, $16)
		ON CONFLICT (user_id, device_generation, batch_id) DO NOTHING`,
		b.Header.BatchID, b.Header.DeviceID, userID, b.Header.Type, b.Header.Reason,
		b.Header.SampleCount, b.Header.DeletionCount, b.Header.AggregateCount,
		b.Header.ActivitySummaryCount, bodyBytes,
		msToTime(b.Header.ExportedAt), wakeID, trigger, b.ParseMs, deviceTokenID, generation)
	if err != nil {
		return res, fmt.Errorf("reserve batch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// An older batch has no verifiable body/result. Never invent its receipt.
		return res, errBatchConflict
	}

	// Initialize only newly approved hosted accounts. PostgreSQL and the phone
	// may have different tzdb releases; an unknown zone leaves initialization
	// pending instead of rejecting otherwise healthy data. Later uploads (for
	// example while traveling) never move an established account calendar.
	if b.Header.TimeZoneID != "" {
		if _, err := tx.Exec(ctx, `UPDATE users
			SET time_zone = $2, time_zone_auto_initialize = false
			WHERE id = $1 AND time_zone IS NULL AND time_zone_auto_initialize
			AND EXISTS (SELECT 1 FROM pg_timezone_names WHERE name = $2)`,
			userID, b.Header.TimeZoneID); err != nil {
			return res, fmt.Errorf("initialize account time zone: %w", err)
		}
	}

	insertStart := time.Now()
	if sourceConnectionID != nil {
		prepared, err := prepareFamilyRecords(ctx, tx, b, userID, *sourceConnectionID, credentialID)
		if err != nil {
			return res, err
		}
		b = prepared
	}

	typeIDs, err := ensureTypes(ctx, tx, b)
	if err != nil {
		return res, fmt.Errorf("ensure types: %w", err)
	}
	sourceIDs, err := ensureSources(ctx, tx, b.Samples)
	if err != nil {
		return res, fmt.Errorf("ensure sources: %w", err)
	}
	temporalContextIDs, err := ensureTemporalContexts(ctx, tx, b)
	if err != nil {
		return res, fmt.Errorf("ensure temporal contexts: %w", err)
	}

	var quantity, category, workout, heartbeat, ecg, mind, dose []Sample
	for i := range b.Samples {
		switch b.Samples[i].Kind {
		case "quantity":
			quantity = append(quantity, b.Samples[i])
		case "category":
			category = append(category, b.Samples[i])
		case "workout":
			workout = append(workout, b.Samples[i])
		case "heartbeatSeries":
			heartbeat = append(heartbeat, b.Samples[i])
		case "ecg":
			ecg = append(ecg, b.Samples[i])
		case "stateOfMind":
			mind = append(mind, b.Samples[i])
		case "medicationDose":
			dose = append(dose, b.Samples[i])
		}
	}

	if n, err := insertQuantity(ctx, tx, quantity, typeIDs, sourceIDs, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("insert quantity: %w", err)
	} else {
		res.Accepted += n
	}
	if n, err := insertCategory(ctx, tx, category, typeIDs, sourceIDs, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("insert category: %w", err)
	} else {
		res.Accepted += n
	}
	if n, err := insertWorkouts(ctx, tx, workout, sourceIDs, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("insert workouts: %w", err)
	} else {
		res.Accepted += n
	}
	if n, err := insertHeartbeatSeries(ctx, tx, heartbeat, sourceIDs, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("insert heartbeat series: %w", err)
	} else {
		res.Accepted += n
	}
	if n, err := insertECG(ctx, tx, ecg, sourceIDs, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("insert ecg: %w", err)
	} else {
		res.Accepted += n
	}
	if n, err := insertStateOfMind(ctx, tx, mind, sourceIDs, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("insert state of mind: %w", err)
	} else {
		res.Accepted += n
	}
	if n, err := insertMedicationDoses(ctx, tx, dose, sourceIDs, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("insert medication doses: %w", err)
	} else {
		res.Accepted += n
	}
	res.Duplicates = int64(len(b.Samples)+b.suppressedSamples) - res.Accepted

	if n, err := insertRoutePoints(ctx, tx, b.Routes, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("insert route points: %w", err)
	} else {
		res.RoutePoints = n
	}

	if n, err := insertWorkoutSeriesPoints(ctx, tx, b.Series, typeIDs, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("insert workout series points: %w", err)
	} else {
		res.SeriesPoints = n
	}

	if err := upsertUser(ctx, tx, userID, b.Profile); err != nil {
		return res, fmt.Errorf("upsert user: %w", err)
	}

	seriesIDs, err := ensureAggregateSeries(ctx, tx, b.Aggregates, typeIDs)
	if err != nil {
		return res, fmt.Errorf("ensure aggregate series: %w", err)
	}
	if n, err := upsertAggregateSamples(ctx, tx, b.Aggregates, typeIDs, seriesIDs, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("upsert aggregate samples: %w", err)
	} else {
		res.AggregateSamples = n
	}

	if n, err := upsertActivitySummaries(ctx, tx, b.ActivitySummaries, temporalContextIDs, userID); err != nil {
		return res, fmt.Errorf("upsert activity summaries: %w", err)
	} else {
		res.ActivitySummaries = n
	}

	if n, err := applyDeletions(ctx, tx, b.Deletions, userID); err != nil {
		return res, fmt.Errorf("apply deletions: %w", err)
	} else {
		res.Deleted = n + b.sourceDeleted
	}

	// Server-measured insert time covers the health-data mutations above. The
	// reservation and commit are intentionally excluded. parse_ms is measured in
	// the handler and was stored on the reserved row.
	insertMs := time.Since(insertStart).Milliseconds()
	_, err = tx.Exec(ctx, `UPDATE batches SET insert_ms = $2 WHERE batch_id = $1 AND user_id = $3 AND device_generation = $4`,
		b.Header.BatchID, insertMs, userID, generation)
	if err != nil {
		return res, fmt.Errorf("update batch telemetry: %w", err)
	}
	if err := finishBatchReceipt(ctx, tx, b, userID, generation, &res); err != nil {
		return res, fmt.Errorf("finish receipt: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// Routes returns workout summaries for workouts with GPS route points.
func (st *Store) Routes(ctx context.Context, userID string, filters RouteFilters) ([]RouteSummary, error) {
	args := []any{userID}
	where := []string{"w.user_id = $1"}

	if filters.Start != nil {
		args = append(args, *filters.Start)
		where = append(where, fmt.Sprintf("w.start_ts >= $%d", len(args)))
	}
	if filters.End != nil {
		args = append(args, *filters.End)
		where = append(where, fmt.Sprintf("w.start_ts < $%d", len(args)))
	}
	if filters.ActivityType != "" {
		args = append(args, filters.ActivityType)
		where = append(where, fmt.Sprintf("w.activity_type = $%d", len(args)))
	}
	if filters.MinDistanceM != nil {
		args = append(args, *filters.MinDistanceM)
		where = append(where, fmt.Sprintf("w.distance_m >= $%d", len(args)))
	}
	if filters.MaxDistanceM != nil {
		args = append(args, *filters.MaxDistanceM)
		where = append(where, fmt.Sprintf("w.distance_m <= $%d", len(args)))
	}

	limit := filters.Limit
	if limit <= 0 {
		limit = 48
	}
	if limit > 200 {
		limit = 200
	}
	args = append(args, limit)
	limitParam := fmt.Sprintf("$%d", len(args))

	offset := filters.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, offset)
	offsetParam := fmt.Sprintf("$%d", len(args))

	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}

	rows, err := st.pool.Query(ctx, fmt.Sprintf(`
		SELECT
			w.uuid::text,
			w.start_ts,
			w.end_ts,
			w.activity_type,
			w.duration_s::float8,
			w.distance_m::float8,
			w.energy_kcal::float8,
			count(r.*)::bigint AS route_point_count,
			min(r.lat)::float8 AS min_lat,
			max(r.lat)::float8 AS max_lat,
			min(r.lon)::float8 AS min_lon,
			max(r.lon)::float8 AS max_lon,
			COALESCE(metric_streams.available_metrics, ARRAY[]::text[]) AS available_metrics
		FROM workouts w
		JOIN workout_route_points r ON r.workout_uuid = w.uuid AND r.user_id = w.user_id
		LEFT JOIN LATERAL (
			SELECT array_agg(DISTINCT st.identifier ORDER BY st.identifier) AS available_metrics
			FROM workout_series_points wsp
			JOIN sample_types st ON st.type_id = wsp.type_id
			WHERE wsp.workout_uuid = w.uuid AND wsp.user_id = w.user_id
		) metric_streams ON TRUE
		%s
		GROUP BY w.user_id, w.uuid, metric_streams.available_metrics
		HAVING count(r.*) > 1
		ORDER BY w.start_ts DESC
		LIMIT %s
		OFFSET %s`, whereSQL, limitParam, offsetParam), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RouteSummary{}
	for rows.Next() {
		summary, err := scanRouteSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}

// Route returns one workout and all route points, or nil when it is absent.
func (st *Store) Route(ctx context.Context, userID, uuid string) (*RouteDetail, error) {
	row := st.pool.QueryRow(ctx, `
		SELECT
			w.uuid::text,
			w.start_ts,
			w.end_ts,
			w.activity_type,
			w.duration_s::float8,
			w.distance_m::float8,
			w.energy_kcal::float8,
			count(r.*)::bigint AS route_point_count,
			min(r.lat)::float8 AS min_lat,
			max(r.lat)::float8 AS max_lat,
			min(r.lon)::float8 AS min_lon,
			max(r.lon)::float8 AS max_lon,
			COALESCE(metric_streams.available_metrics, ARRAY[]::text[]) AS available_metrics,
			jsonb_agg(
				jsonb_build_object(
					'ts', to_jsonb(r.ts),
					'lat', r.lat,
					'lon', r.lon,
					'altitudeM', r.altitude_m,
					'hAccM', r.h_acc_m,
					'vAccM', r.v_acc_m,
					'speedMps', r.speed_mps,
					'courseDeg', r.course_deg
				)
				ORDER BY r.ts
			) AS points
		FROM workouts w
		JOIN workout_route_points r ON r.workout_uuid = w.uuid AND r.user_id = w.user_id
		LEFT JOIN LATERAL (
			SELECT array_agg(DISTINCT st.identifier ORDER BY st.identifier) AS available_metrics
			FROM workout_series_points wsp
			JOIN sample_types st ON st.type_id = wsp.type_id
			WHERE wsp.workout_uuid = w.uuid AND wsp.user_id = w.user_id
		) metric_streams ON TRUE
		WHERE w.uuid = $1 AND w.user_id = $2
		GROUP BY w.user_id, w.uuid, metric_streams.available_metrics
		HAVING count(r.*) > 1`, uuid, userID)

	var (
		summary    RouteSummary
		pointsJSON []byte
	)
	if err := scanRouteSummaryWithPoints(row, &summary, &pointsJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var points []RouteGPSPoint
	if err := json.Unmarshal(pointsJSON, &points); err != nil {
		return nil, err
	}
	return &RouteDetail{Workout: summary, Points: normalizeRoutePoints(points)}, nil
}

// RouteMetrics returns the intra-workout metric streams for one workout.
func (st *Store) RouteMetrics(ctx context.Context, userID, uuid string) ([]RouteMetricSeries, error) {
	rows, err := st.pool.Query(ctx, `
		SELECT
			st.identifier,
			st.unit,
			jsonb_agg(
				jsonb_build_object('ts', to_jsonb(wsp.ts), 'value', wsp.value)
				ORDER BY wsp.ts
			) AS points
		FROM workout_series_points wsp
		JOIN workouts w ON w.uuid = wsp.workout_uuid AND w.user_id = wsp.user_id
		JOIN sample_types st ON st.type_id = wsp.type_id
		WHERE w.uuid = $1 AND w.user_id = $2
		GROUP BY st.identifier, st.unit
		ORDER BY st.identifier`, uuid, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RouteMetricSeries{}
	for rows.Next() {
		var (
			series     RouteMetricSeries
			pointsJSON []byte
		)
		if err := rows.Scan(&series.Identifier, &series.Unit, &pointsJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(pointsJSON, &series.Points); err != nil {
			return nil, err
		}
		series.Points = normalizeMetricPoints(series.Points)
		out = append(out, series)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRouteSummary(row rowScanner) (RouteSummary, error) {
	var summary RouteSummary
	err := row.Scan(
		&summary.UUID,
		scanRouteTime(&summary.StartTs),
		scanRouteTime(&summary.EndTs),
		&summary.ActivityType,
		&summary.DurationS,
		&summary.DistanceM,
		&summary.EnergyKcal,
		&summary.RoutePointCount,
		&summary.Bounds.MinLat,
		&summary.Bounds.MaxLat,
		&summary.Bounds.MinLon,
		&summary.Bounds.MaxLon,
		&summary.AvailableMetrics,
	)
	if summary.AvailableMetrics == nil {
		summary.AvailableMetrics = []string{}
	}
	return summary, err
}

func scanRouteSummaryWithPoints(row rowScanner, summary *RouteSummary, pointsJSON *[]byte) error {
	err := row.Scan(
		&summary.UUID,
		scanRouteTime(&summary.StartTs),
		scanRouteTime(&summary.EndTs),
		&summary.ActivityType,
		&summary.DurationS,
		&summary.DistanceM,
		&summary.EnergyKcal,
		&summary.RoutePointCount,
		&summary.Bounds.MinLat,
		&summary.Bounds.MaxLat,
		&summary.Bounds.MinLon,
		&summary.Bounds.MaxLon,
		&summary.AvailableMetrics,
		pointsJSON,
	)
	if summary.AvailableMetrics == nil {
		summary.AvailableMetrics = []string{}
	}
	return err
}

type routeTimeScanner struct {
	target *string
}

func scanRouteTime(target *string) routeTimeScanner {
	return routeTimeScanner{target: target}
}

func (s routeTimeScanner) Scan(src any) error {
	switch v := src.(type) {
	case time.Time:
		*s.target = v.UTC().Format(time.RFC3339Nano)
		return nil
	case string:
		*s.target = v
		return nil
	case []byte:
		*s.target = string(v)
		return nil
	case nil:
		*s.target = ""
		return nil
	default:
		return fmt.Errorf("cannot scan %T as route time", src)
	}
}

func normalizeRoutePoints(points []RouteGPSPoint) []RouteGPSPoint {
	for i := range points {
		points[i].Ts = normalizeTimeString(points[i].Ts)
	}
	if points == nil {
		return []RouteGPSPoint{}
	}
	return points
}

func normalizeMetricPoints(points []MetricPoint) []MetricPoint {
	for i := range points {
		points[i].Ts = normalizeTimeString(points[i].Ts)
	}
	if points == nil {
		return []MetricPoint{}
	}
	return points
}

func normalizeTimeString(value string) string {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return value
}

// typeInfo captures the kind/unit observed for a type identifier.
// typeInfo is what a batch says about one type identifier. canonicalUnit
// marks a unit that came from a sample or workout-series line — the client's
// catalog unit for the type — as opposed to an aggregate line, whose unit is
// the series' own.
type typeInfo struct {
	kind          string
	unit          *string
	canonicalUnit bool
}

// batchTypeInfos collects every type identifier a batch references with the
// kind/unit to register it under. Sample and series lines win over aggregate
// lines for the same identifier, and a `duration` aggregate contributes no
// unit at all: its "s" is the series' unit, not the type's. sample_types.unit
// used to be set from whichever line registered the identifier first, so a
// duration aggregate enabled before any raw sample arrived registered e.g.
// HKQuantityTypeIdentifierAppleExerciseTime as "s" and every consumer of
// sample_types.unit (route metrics, the product API catalog, Grafana)
// mislabelled the type from then on.
//
// That is not a rare ordering any more. The client covers a recent aggregate
// window before its raw sweep (`syncRecentAggregates`), so on a first backfill
// the first line this server sees for a type with an aggregate configured is
// normally an aggregate line. This precedence rule and the canonical-unit
// UPDATE in ensureTypes are what keep that from mislabelling every such type —
// neither is redundant.
func batchTypeInfos(b *Batch) map[string]typeInfo {
	infos := map[string]typeInfo{}
	for i := range b.Samples {
		s := &b.Samples[i]
		if _, ok := infos[s.Type]; !ok {
			infos[s.Type] = typeInfo{kind: s.Kind, unit: s.Unit, canonicalUnit: s.Unit != nil}
		}
	}
	// Workout-series lines reference quantity types that may not appear as
	// sample lines in the same batch; register them so series points have a
	// type_id to reference.
	for i := range b.Series {
		s := &b.Series[i].Series
		if _, ok := infos[s.Type]; !ok {
			infos[s.Type] = typeInfo{kind: "quantity", unit: s.Unit, canonicalUnit: s.Unit != nil}
		}
	}
	// Aggregate lines may reference types that appear in no sample line
	// (aggregate-only types). Register those too so aggregate_series has a
	// type_id to reference.
	for i := range b.Aggregates {
		a := &b.Aggregates[i].Aggregate
		if _, ok := infos[a.Type]; !ok {
			unit := a.Unit
			if a.Func == "duration" {
				unit = nil
			}
			infos[a.Type] = typeInfo{kind: "quantity", unit: unit}
		}
	}
	return infos
}

// ensureTypes lazily registers every type identifier seen in the batch and
// returns identifier -> type_id.
func ensureTypes(ctx context.Context, tx pgx.Tx, b *Batch) (map[string]int16, error) {
	infos := batchTypeInfos(b)
	if len(infos) == 0 {
		return map[string]int16{}, nil
	}

	// Deterministic insert order avoids lock-ordering deadlocks between
	// concurrent batches registering the same types (see ensureSources).
	idents := make([]string, 0, len(infos))
	for ident := range infos {
		idents = append(idents, ident)
	}
	slices.Sort(idents)

	kinds := make([]string, 0, len(idents))
	units := make([]*string, 0, len(idents))
	for _, ident := range idents {
		info := infos[ident]
		kinds = append(kinds, info.kind)
		units = append(units, info.unit)
	}

	// nextval fires for every row the SELECT feeds the INSERT, including the
	// ones ON CONFLICT then throws away — so re-registering already-known
	// identifiers burns identity values. type_id is a smallint (ceiling
	// 32767); at a few hundred batches a day that exhausted the sequence in
	// two months and took ingest down with SQLSTATE 2200H. Filter known
	// identifiers out first so only genuinely new types consume a value.
	// ON CONFLICT stays as the backstop for concurrent registrations. The
	// explicit ORDER BY preserves the deterministic lock ordering above,
	// which the anti-join is otherwise free to permute.
	_, err := tx.Exec(ctx, `
		INSERT INTO sample_types (identifier, kind, unit)
		SELECT v.identifier, v.kind, v.unit
		FROM unnest($1::text[], $2::text[], $3::text[])
			AS v(identifier, kind, unit)
		WHERE NOT EXISTS (
			SELECT 1 FROM sample_types st WHERE st.identifier = v.identifier
		)
		ORDER BY v.identifier
		ON CONFLICT (identifier) DO NOTHING`,
		idents, kinds, units)
	if err != nil {
		return nil, err
	}

	// The anti-join above never touches an existing row, so a unit registered
	// wrongly (or a canonical unit that later changed in the client catalog)
	// stayed wrong forever. Let canonical units win: correct rows whose stored
	// unit differs from what a sample/series line says. This is the repair for
	// the aggregate-registers-the-type-first ordering described on
	// batchTypeInfos, which a first backfill now hits by design — do not drop
	// it as a no-op. Only rows that
	// actually differ are written (or locked), and an UPDATE burns no identity
	// values, so on the steady-state path this is a no-op.
	var fixIdents, fixUnits []string
	for _, ident := range idents {
		if info := infos[ident]; info.canonicalUnit {
			fixIdents = append(fixIdents, ident)
			fixUnits = append(fixUnits, *info.unit)
		}
	}
	if len(fixIdents) > 0 {
		_, err = tx.Exec(ctx, `
			UPDATE sample_types st
			   SET unit = v.unit
			  FROM unnest($1::text[], $2::text[]) AS v(identifier, unit)
			 WHERE st.identifier = v.identifier
			   AND st.unit IS DISTINCT FROM v.unit`,
			fixIdents, fixUnits)
		if err != nil {
			return nil, err
		}
	}

	rows, err := tx.Query(ctx,
		`SELECT identifier, type_id FROM sample_types WHERE identifier = ANY($1)`, idents)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[string]int16, len(idents))
	for rows.Next() {
		var ident string
		var id int16
		if err := rows.Scan(&ident, &id); err != nil {
			return nil, err
		}
		ids[ident] = id
	}
	return ids, rows.Err()
}

type sourceKey struct{ name, bundle, version string }

func sampleSourceKey(s *Sample) sourceKey {
	return sourceKey{name: deref(s.SourceName), bundle: deref(s.SourceBundleID), version: deref(s.SourceVersion)}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ensureSources lazily registers distinct (name, bundle_id, version) triples
// and returns triple -> source_id. NULLs are normalized to ” so the unique
// constraint actually deduplicates.
func ensureSources(ctx context.Context, tx pgx.Tx, samples []Sample) (map[sourceKey]int16, error) {
	keys := map[sourceKey]struct{}{}
	for i := range samples {
		keys[sampleSourceKey(&samples[i])] = struct{}{}
	}
	if len(keys) == 0 {
		return map[sourceKey]int16{}, nil
	}

	// Insert in a deterministic key order. Go's map iteration is randomized, so
	// without this two concurrent batches registering the same source rows take
	// their row locks in opposite orders and deadlock (SQLSTATE 40P01); the
	// loser's whole batch fails and only succeeds on client retry.
	sorted := make([]sourceKey, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	slices.SortFunc(sorted, func(a, b sourceKey) int {
		return cmp.Or(
			cmp.Compare(a.name, b.name),
			cmp.Compare(a.bundle, b.bundle),
			cmp.Compare(a.version, b.version),
		)
	})

	names := make([]string, 0, len(sorted))
	bundles := make([]string, 0, len(sorted))
	versions := make([]string, 0, len(sorted))
	for _, k := range sorted {
		names = append(names, k.name)
		bundles = append(bundles, k.bundle)
		versions = append(versions, k.version)
	}

	if err := registerSources(ctx, tx, names, bundles, versions); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx,
		`SELECT source_id, name, bundle_id, version FROM sources WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[sourceKey]int16, len(keys))
	for rows.Next() {
		var id int16
		var k sourceKey
		if err := rows.Scan(&id, &k.name, &k.bundle, &k.version); err != nil {
			return nil, err
		}
		ids[k] = id
	}
	return ids, rows.Err()
}

// sourceRegistrationLock is the transaction-level advisory lock that
// serialises source registration: ('puls' as an int4, 2), beside migrate.sh's
// ('puls', 1).
const sourceRegistrationLock = "1886743667, 2"

// sourceIDCeiling is smallint's maximum, the most source_id values there are.
const sourceIDCeiling = 32767

// registerSources inserts the triples sources does not have yet, reusing
// free source_id values below the identity sequence before drawing new ones.
//
// source_id is a smallint referenced as smallint by quantity_samples (and its
// quantity_rollups aggregate) and six more tables; widening it means
// decompressing every columnstore chunk (TimescaleDB refuses ALTER TYPE on a
// hypertable with compressed chunks) and rebuilding the aggregate. Before the
// 2026-08 anti-join fix, ON CONFLICT burned an identity value per known
// triple per batch, leaving the sequence at 29521 of 32767 with only 536 rows
// (2026-10-06): ~29k ids that no row holds and nextval will never return. So
// new triples take the lowest of those first; only when none is left below
// the sequence does nextval run. That makes the capacity 32767 rows, not
// 32767 nextval calls.
//
// Every gap id is at most the sequence's last value, so it never collides
// with a later nextval. The advisory lock makes concurrent registrations
// take gaps one transaction at a time (it is held to commit, so the next one
// sees the rows); a writer outside this path that holds an uncommitted id
// (a test fixture) is met by ON CONFLICT DO NOTHING and the loop below
// tries again with the next gap.
func registerSources(ctx context.Context, tx pgx.Tx, names, bundles, versions []string) error {
	// Steady state: every triple is known, so take no lock and write nothing.
	if n, err := countMissingSources(ctx, tx, names, bundles, versions); err != nil || n == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+sourceRegistrationLock+`)`); err != nil {
		return fmt.Errorf("source registration lock: %w", err)
	}

	for attempt := 0; attempt < 3; attempt++ {
		// Fill gaps first. pg_sequence_last_value is NULL until the sequence
		// is first called, and then nothing below it is a gap.
		_, err := tx.Exec(ctx, `
			WITH want AS (
				SELECT v.name, v.bundle_id, v.version,
				       row_number() OVER (ORDER BY v.name, v.bundle_id, v.version) AS n
				FROM unnest($1::text[], $2::text[], $3::text[])
					AS v(name, bundle_id, version)
				WHERE NOT EXISTS (
					SELECT 1 FROM sources s
					WHERE s.name = v.name
					  AND s.bundle_id = v.bundle_id
					  AND s.version = v.version
				)
			), free AS (
				SELECT g AS source_id, row_number() OVER (ORDER BY g) AS n
				FROM generate_series(1, COALESCE(pg_sequence_last_value(
					pg_get_serial_sequence('public.sources', 'source_id')::regclass), 0)) AS g
				WHERE NOT EXISTS (SELECT 1 FROM sources s WHERE s.source_id = g)
				ORDER BY g
				LIMIT (SELECT count(*) FROM want)
			)
			INSERT INTO sources (source_id, name, bundle_id, version)
			OVERRIDING SYSTEM VALUE
			SELECT f.source_id, w.name, w.bundle_id, w.version
			FROM want w JOIN free f USING (n)
			ORDER BY w.n
			ON CONFLICT DO NOTHING`,
			names, bundles, versions)
		if err != nil {
			return err
		}

		// No gap left for the rest: draw new values. The anti-join keeps
		// known triples from burning one (see ensureTypes for the outage
		// that caused).
		if err := insertSourcesFromSequence(ctx, tx, names, bundles, versions); err != nil {
			return err
		}

		n, err := countMissingSources(ctx, tx, names, bundles, versions)
		if err != nil {
			return err
		}
		if n == 0 {
			warnIfSourcesNearCeiling(ctx, tx)
			return nil
		}
	}
	return fmt.Errorf("sources: triples still unregistered after 3 attempts")
}

func countMissingSources(ctx context.Context, tx pgx.Tx, names, bundles, versions []string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM unnest($1::text[], $2::text[], $3::text[]) AS v(name, bundle_id, version)
		WHERE NOT EXISTS (
			SELECT 1 FROM sources s
			WHERE s.name = v.name
			  AND s.bundle_id = v.bundle_id
			  AND s.version = v.version
		)`, names, bundles, versions).Scan(&n)
	return n, err
}

// warnIfSourcesNearCeiling logs once sources holds 80% of the ids there are,
// the point to widen source_id (see registerSources). Grafana's "Lookup
// sequence near exhaustion" rule alerts on the same measure.
func warnIfSourcesNearCeiling(ctx context.Context, tx pgx.Tx) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sources`).Scan(&n); err != nil {
		return
	}
	if n*5 >= sourceIDCeiling*4 {
		slog.WarnContext(ctx, "sources is near the smallint ceiling; widen source_id",
			"rows", n, "ceiling", sourceIDCeiling)
	}
}

func insertSourcesFromSequence(ctx context.Context, tx pgx.Tx, names, bundles, versions []string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sources (name, bundle_id, version)
		SELECT v.name, v.bundle_id, v.version
		FROM unnest($1::text[], $2::text[], $3::text[])
			AS v(name, bundle_id, version)
		WHERE NOT EXISTS (
			SELECT 1 FROM sources s
			WHERE s.name = v.name
			  AND s.bundle_id = v.bundle_id
			  AND s.version = v.version
		)
		ORDER BY v.name, v.bundle_id, v.version
		ON CONFLICT (name, bundle_id, version) DO NOTHING`,
		names, bundles, versions)
	return err
}

type temporalContextKey struct {
	timeZoneID string
	offset     int32
	source     string
	confidence string
	tzdb       string
}

func keyForTemporalContext(tc *TemporalContext) (temporalContextKey, bool) {
	if tc == nil {
		return temporalContextKey{}, false
	}
	return temporalContextKey{
		timeZoneID: tc.TimeZoneID,
		offset:     tc.UTCOffsetSeconds,
		source:     tc.Source,
		confidence: tc.Confidence,
		tzdb:       tc.TZDBVersion,
	}, true
}

func addTemporalContextKey(keys map[temporalContextKey]struct{}, tc *TemporalContext) {
	if k, ok := keyForTemporalContext(tc); ok {
		keys[k] = struct{}{}
	}
}

func temporalContextID(ids map[temporalContextKey]int32, tc *TemporalContext) *int32 {
	k, ok := keyForTemporalContext(tc)
	if !ok {
		return nil
	}
	id, ok := ids[k]
	if !ok {
		return nil
	}
	return &id
}

func ensureTemporalContexts(ctx context.Context, tx pgx.Tx, b *Batch) (map[temporalContextKey]int32, error) {
	keys := map[temporalContextKey]struct{}{}
	for i := range b.Samples {
		s := &b.Samples[i]
		addTemporalContextKey(keys, s.StartContext)
		addTemporalContextKey(keys, s.EndContext)
		if s.MedicationDose != nil {
			addTemporalContextKey(keys, s.MedicationDose.ScheduledAtContext)
		}
	}
	for i := range b.Routes {
		for j := range b.Routes[i].Route.Points {
			addTemporalContextKey(keys, b.Routes[i].Route.Points[j].TemporalContext)
		}
	}
	for i := range b.Series {
		for j := range b.Series[i].Series.Points {
			addTemporalContextKey(keys, b.Series[i].Series.Points[j].TemporalContext)
		}
	}
	for i := range b.Aggregates {
		a := &b.Aggregates[i].Aggregate
		addTemporalContextKey(keys, a.BucketStartContext)
		addTemporalContextKey(keys, a.BucketEndContext)
	}
	for i := range b.ActivitySummaries {
		addTemporalContextKey(keys, b.ActivitySummaries[i].ActivitySummary.TemporalContext)
	}
	if len(keys) == 0 {
		return map[temporalContextKey]int32{}, nil
	}

	sorted := make([]temporalContextKey, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	slices.SortFunc(sorted, func(a, b temporalContextKey) int {
		return cmp.Or(
			cmp.Compare(a.timeZoneID, b.timeZoneID),
			cmp.Compare(a.offset, b.offset),
			cmp.Compare(a.source, b.source),
			cmp.Compare(a.confidence, b.confidence),
			cmp.Compare(a.tzdb, b.tzdb),
		)
	})

	zones := make([]string, 0, len(sorted))
	offsets := make([]int32, 0, len(sorted))
	sources := make([]string, 0, len(sorted))
	confidences := make([]string, 0, len(sorted))
	tzdbs := make([]string, 0, len(sorted))
	for _, k := range sorted {
		zones = append(zones, k.timeZoneID)
		offsets = append(offsets, k.offset)
		sources = append(sources, k.source)
		confidences = append(confidences, k.confidence)
		tzdbs = append(tzdbs, k.tzdb)
	}

	// Skip contexts we already know. temporal_context_id is an integer so the
	// ceiling is far off, but the same nextval-on-conflict leak applies (see
	// ensureTypes) and there is no reason to keep paying it.
	_, err := tx.Exec(ctx, `
		INSERT INTO temporal_contexts (time_zone_id, utc_offset_seconds, source, confidence, tzdb_version)
		SELECT v.time_zone_id, v.utc_offset_seconds, v.source, v.confidence, v.tzdb_version
		FROM unnest($1::text[], $2::integer[], $3::text[], $4::text[], $5::text[])
			AS v(time_zone_id, utc_offset_seconds, source, confidence, tzdb_version)
		WHERE NOT EXISTS (
			SELECT 1 FROM temporal_contexts tc
			WHERE tc.time_zone_id = v.time_zone_id
			  AND tc.utc_offset_seconds = v.utc_offset_seconds
			  AND tc.source = v.source
			  AND tc.confidence = v.confidence
			  AND tc.tzdb_version = v.tzdb_version
		)
		ORDER BY v.time_zone_id, v.utc_offset_seconds, v.source, v.confidence, v.tzdb_version
		ON CONFLICT (time_zone_id, utc_offset_seconds, source, confidence, tzdb_version) DO NOTHING`,
		zones, offsets, sources, confidences, tzdbs)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT temporal_context_id, time_zone_id, utc_offset_seconds, source, confidence, tzdb_version
		FROM temporal_contexts
		WHERE time_zone_id = ANY($1) AND source = ANY($2) AND confidence = ANY($3)`,
		zones, sources, confidences)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[temporalContextKey]int32, len(keys))
	for rows.Next() {
		var id int32
		var k temporalContextKey
		if err := rows.Scan(&id, &k.timeZoneID, &k.offset, &k.source, &k.confidence, &k.tzdb); err != nil {
			return nil, err
		}
		if _, wanted := keys[k]; wanted {
			ids[k] = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) != len(keys) {
		return nil, fmt.Errorf("temporal_contexts lookup returned %d/%d rows", len(ids), len(keys))
	}
	return ids, nil
}

// aggSeriesKey is the natural key of one aggregate_series row.
type aggSeriesKey struct {
	typeID        int16
	fn            string
	intervalValue int
	intervalUnit  string
	deviceFilter  string
}

func aggregateSeriesKey(a *AggregateLine, typeIDs map[string]int16) aggSeriesKey {
	agg := &a.Aggregate
	return aggSeriesKey{
		typeID:        typeIDs[agg.Type],
		fn:            agg.Func,
		intervalValue: agg.IntervalValue,
		intervalUnit:  agg.IntervalUnit,
		deviceFilter:  agg.DeviceFilter,
	}
}

// ensureAggregateSeries lazily registers distinct
// (type_id, agg_func, interval_value, interval_unit, device_filter) keys and
// returns key -> series_id. The unit of the first line seen for a new key is
// recorded; existing rows are never overwritten.
func ensureAggregateSeries(ctx context.Context, tx pgx.Tx, aggs []AggregateLine,
	typeIDs map[string]int16) (map[aggSeriesKey]int16, error) {
	units := map[aggSeriesKey]*string{}
	for i := range aggs {
		k := aggregateSeriesKey(&aggs[i], typeIDs)
		if _, ok := units[k]; !ok {
			units[k] = aggs[i].Aggregate.Unit
		}
	}
	if len(units) == 0 {
		return map[aggSeriesKey]int16{}, nil
	}

	// Deterministic insert order avoids lock-ordering deadlocks between
	// concurrent batches registering the same series (see ensureSources).
	sorted := make([]aggSeriesKey, 0, len(units))
	for k := range units {
		sorted = append(sorted, k)
	}
	slices.SortFunc(sorted, func(a, b aggSeriesKey) int {
		return cmp.Or(
			cmp.Compare(a.typeID, b.typeID),
			cmp.Compare(a.fn, b.fn),
			cmp.Compare(a.intervalValue, b.intervalValue),
			cmp.Compare(a.intervalUnit, b.intervalUnit),
			cmp.Compare(a.deviceFilter, b.deviceFilter),
		)
	})

	tids := make([]int16, 0, len(sorted))
	fns := make([]string, 0, len(sorted))
	ivals := make([]int32, 0, len(sorted))
	iunits := make([]string, 0, len(sorted))
	filters := make([]string, 0, len(sorted))
	us := make([]*string, 0, len(sorted))
	for _, k := range sorted {
		tids = append(tids, k.typeID)
		fns = append(fns, k.fn)
		ivals = append(ivals, int32(k.intervalValue))
		iunits = append(iunits, k.intervalUnit)
		filters = append(filters, k.deviceFilter)
		us = append(us, units[k])
	}

	// Skip series we already know: series_id is a smallint and ON CONFLICT
	// does not give back the identity value (see ensureTypes).
	_, err := tx.Exec(ctx, `
		INSERT INTO aggregate_series (type_id, agg_func, interval_value, interval_unit, device_filter, unit)
		SELECT v.type_id, v.agg_func, v.interval_value, v.interval_unit, v.device_filter, v.unit
		FROM unnest($1::smallint[], $2::text[], $3::int[], $4::text[], $5::text[], $6::text[])
			AS v(type_id, agg_func, interval_value, interval_unit, device_filter, unit)
		WHERE NOT EXISTS (
			SELECT 1 FROM aggregate_series a
			WHERE a.type_id = v.type_id
			  AND a.agg_func = v.agg_func
			  AND a.interval_value = v.interval_value
			  AND a.interval_unit = v.interval_unit
			  AND a.device_filter = v.device_filter
		)
		ORDER BY v.type_id, v.agg_func, v.interval_value, v.interval_unit, v.device_filter
		ON CONFLICT (type_id, agg_func, interval_value, interval_unit, device_filter) DO NOTHING`,
		tids, fns, ivals, iunits, filters, us)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT series_id, type_id, agg_func, interval_value, interval_unit, device_filter
		FROM aggregate_series WHERE type_id = ANY($1::smallint[])`, tids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[aggSeriesKey]int16, len(units))
	for rows.Next() {
		var id int16
		var k aggSeriesKey
		if err := rows.Scan(&id, &k.typeID, &k.fn, &k.intervalValue, &k.intervalUnit, &k.deviceFilter); err != nil {
			return nil, err
		}
		ids[k] = id
	}
	return ids, rows.Err()
}

// upsertAggregateSamples bulk-upserts aggregate rows via unnest. Unlike raw
// samples, these are recomputed and re-sent by the client, so conflicts
// overwrite value/bucket_end (including overwriting with NULL for an explicit
// null value) and bump updated_at.
func upsertAggregateSamples(ctx context.Context, tx pgx.Tx, aggs []AggregateLine,
	typeIDs map[string]int16, seriesIDs map[aggSeriesKey]int16,
	temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	if len(aggs) == 0 {
		return 0, nil
	}

	// Dedupe on (series_id, bucket_start), last line wins: a single INSERT
	// ... ON CONFLICT DO UPDATE errors if it touches the same row twice.
	type bucketKey struct {
		seriesID int16
		startNS  int64
	}
	type aggRow struct {
		sid            int16
		start          time.Time
		end            time.Time
		startContextID *int32
		endContextID   *int32
		value          *float64
	}
	idx := map[bucketKey]int{}
	rows := make([]aggRow, 0, len(aggs))
	for i := range aggs {
		a := &aggs[i].Aggregate
		sid := seriesIDs[aggregateSeriesKey(&aggs[i], typeIDs)]
		start := msToTime(a.BucketStart)
		bk := bucketKey{seriesID: sid, startNS: start.UnixNano()}
		if j, ok := idx[bk]; ok {
			rows[j].end = msToTime(a.BucketEnd)
			rows[j].startContextID = temporalContextID(temporalContextIDs, a.BucketStartContext)
			rows[j].endContextID = temporalContextID(temporalContextIDs, a.BucketEndContext)
			rows[j].value = a.Value
			continue
		}
		idx[bk] = len(rows)
		rows = append(rows, aggRow{
			sid: sid, start: start, end: msToTime(a.BucketEnd),
			startContextID: temporalContextID(temporalContextIDs, a.BucketStartContext),
			endContextID:   temporalContextID(temporalContextIDs, a.BucketEndContext),
			value:          a.Value,
		})
	}

	// Upsert in a deterministic (series_id, bucket_start) order so concurrent
	// DO UPDATE conflicts lock rows consistently and don't deadlock (see
	// ensureSources).
	slices.SortFunc(rows, func(a, b aggRow) int {
		return cmp.Or(cmp.Compare(a.sid, b.sid), a.start.Compare(b.start))
	})

	sids := make([]int16, len(rows))
	starts := make([]time.Time, len(rows))
	ends := make([]time.Time, len(rows))
	startContextIDs := make([]*int32, len(rows))
	endContextIDs := make([]*int32, len(rows))
	values := make([]*float64, len(rows))
	for i, r := range rows {
		sids[i] = r.sid
		starts[i] = r.start
		ends[i] = r.end
		startContextIDs[i] = r.startContextID
		endContextIDs[i] = r.endContextID
		values[i] = r.value
	}

	tag, err := tx.Exec(ctx, `
			INSERT INTO aggregate_samples (series_id, bucket_start, bucket_end,
			                               bucket_start_temporal_context_id,
			                               bucket_end_temporal_context_id,
			                               value, user_id)
			SELECT x.series_id, x.bucket_start, x.bucket_end,
			       x.bucket_start_temporal_context_id, x.bucket_end_temporal_context_id,
			       x.value, $7::uuid
			FROM unnest($1::smallint[], $2::timestamptz[], $3::timestamptz[],
			            $4::integer[], $5::integer[], $6::float8[])
			     AS x(series_id, bucket_start, bucket_end,
			          bucket_start_temporal_context_id, bucket_end_temporal_context_id, value)
			ON CONFLICT (series_id, bucket_start, user_id)
			DO UPDATE SET value = EXCLUDED.value, bucket_end = EXCLUDED.bucket_end,
			              bucket_start_temporal_context_id = EXCLUDED.bucket_start_temporal_context_id,
			              bucket_end_temporal_context_id = EXCLUDED.bucket_end_temporal_context_id,
			              updated_at = now()`,
		sids, starts, ends, startContextIDs, endContextIDs, values, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// upsertActivitySummaries bulk-upserts daily activity-summary rows via unnest,
// keyed on date. Like aggregates, summaries are recomputed and re-sent (today's
// rings keep changing), so conflicts overwrite every column (including
// overwriting with NULL for explicit nulls) and bump updated_at.
func upsertActivitySummaries(ctx context.Context, tx pgx.Tx, summaries []ActivitySummaryLine,
	temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	if len(summaries) == 0 {
		return 0, nil
	}

	// Dedupe on date, last line wins: a single INSERT ... ON CONFLICT DO UPDATE
	// errors if it touches the same row twice.
	type asRow struct {
		date         string
		contextID    *int32
		moveKcal     *float64
		moveGoal     *float64
		exerciseMin  *float64
		exerciseGoal *float64
		standHours   *float64
		standGoal    *float64
		moveMode     *int16
		moveTimeMin  *float64
		moveTimeGoal *float64
	}
	idx := map[string]int{}
	rows := make([]asRow, 0, len(summaries))
	for i := range summaries {
		a := &summaries[i].ActivitySummary
		d, err := activitySummaryDateKey(a)
		if err != nil {
			return 0, err
		}
		r := asRow{
			date: d, contextID: temporalContextID(temporalContextIDs, a.TemporalContext),
			moveKcal: a.MoveKcal, moveGoal: a.MoveGoalKcal,
			exerciseMin: a.ExerciseMin, exerciseGoal: a.ExerciseGoalMin,
			standHours: a.StandHours, standGoal: a.StandGoalHours,
			moveMode: a.MoveMode, moveTimeMin: a.MoveTimeMin, moveTimeGoal: a.MoveTimeGoalMin,
		}
		if j, ok := idx[d]; ok {
			rows[j] = r
			continue
		}
		idx[d] = len(rows)
		rows = append(rows, r)
	}

	// Upsert in deterministic date order so concurrent DO UPDATE conflicts lock
	// rows consistently and don't deadlock (see ensureSources).
	slices.SortFunc(rows, func(a, b asRow) int { return cmp.Compare(a.date, b.date) })

	n := len(rows)
	dates := make([]string, n)
	contextIDs := make([]*int32, n)
	moveKcal := make([]*float64, n)
	moveGoal := make([]*float64, n)
	exerciseMin := make([]*float64, n)
	exerciseGoal := make([]*float64, n)
	standHours := make([]*float64, n)
	standGoal := make([]*float64, n)
	moveMode := make([]*int16, n)
	moveTimeMin := make([]*float64, n)
	moveTimeGoal := make([]*float64, n)
	for i, r := range rows {
		dates[i] = r.date
		contextIDs[i] = r.contextID
		moveKcal[i] = r.moveKcal
		moveGoal[i] = r.moveGoal
		exerciseMin[i] = r.exerciseMin
		exerciseGoal[i] = r.exerciseGoal
		standHours[i] = r.standHours
		standGoal[i] = r.standGoal
		moveMode[i] = r.moveMode
		moveTimeMin[i] = r.moveTimeMin
		moveTimeGoal[i] = r.moveTimeGoal
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO activity_summaries (date, user_id, move_kcal, move_goal_kcal, exercise_min,
		                                exercise_goal_min, stand_hours, stand_goal_hours,
		                                move_mode, move_time_min, move_time_goal_min,
		                                temporal_context_id)
		SELECT x.date::date, $12::uuid, x.move_kcal, x.move_goal_kcal, x.exercise_min,
		       x.exercise_goal_min, x.stand_hours, x.stand_goal_hours,
		       x.move_mode, x.move_time_min, x.move_time_goal_min,
		       x.temporal_context_id
		FROM unnest($1::text[], $2::integer[], $3::float8[], $4::float8[], $5::float8[],
		            $6::float8[], $7::float8[], $8::float8[], $9::smallint[],
		            $10::float8[], $11::float8[])
		     AS x(date, temporal_context_id, move_kcal, move_goal_kcal, exercise_min, exercise_goal_min,
		          stand_hours, stand_goal_hours, move_mode, move_time_min, move_time_goal_min)
		ON CONFLICT (user_id, date) DO UPDATE SET
			move_kcal = EXCLUDED.move_kcal, move_goal_kcal = EXCLUDED.move_goal_kcal,
			exercise_min = EXCLUDED.exercise_min, exercise_goal_min = EXCLUDED.exercise_goal_min,
			stand_hours = EXCLUDED.stand_hours, stand_goal_hours = EXCLUDED.stand_goal_hours,
			move_mode = EXCLUDED.move_mode, move_time_min = EXCLUDED.move_time_min,
			move_time_goal_min = EXCLUDED.move_time_goal_min,
			temporal_context_id = EXCLUDED.temporal_context_id,
			updated_at = now()`,
		dates, contextIDs, moveKcal, moveGoal, exerciseMin, exerciseGoal, standHours, standGoal,
		moveMode, moveTimeMin, moveTimeGoal, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func activitySummaryDateKey(a *struct {
	Date            float64          `json:"date"`
	LocalDate       string           `json:"localDate"`
	TemporalContext *TemporalContext `json:"temporalContext"`
	MoveKcal        *float64         `json:"moveKcal"`
	MoveGoalKcal    *float64         `json:"moveGoalKcal"`
	ExerciseMin     *float64         `json:"exerciseMin"`
	ExerciseGoalMin *float64         `json:"exerciseGoalMin"`
	StandHours      *float64         `json:"standHours"`
	StandGoalHours  *float64         `json:"standGoalHours"`
	MoveMode        *int16           `json:"moveMode"`
	MoveTimeMin     *float64         `json:"moveTimeMin"`
	MoveTimeGoalMin *float64         `json:"moveTimeGoalMin"`
}) (string, error) {
	if a.LocalDate != "" {
		if !validLocalDate(a.LocalDate) {
			return "", fmt.Errorf("invalid activity summary localDate %q", a.LocalDate)
		}
		return a.LocalDate, nil
	}
	// No localDate: a client from before it existed. date is the start of
	// the phone's local day as an instant, and the ring belongs to that local
	// day, never to the UTC one (east of UTC, local midnight is the previous
	// UTC day, and the row would land on the wrong date). The batch header
	// carries no offset, so the line's own temporalContext is the only one
	// there is: shift by it when present.
	if tc := a.TemporalContext; tc != nil {
		return msToTime(a.Date).Add(time.Duration(tc.UTCOffsetSeconds) * time.Second).Format("2006-01-02"), nil
	}
	// Neither: the protocol's documented legacy rule (docs/protocol/README.md,
	// fixture 03), the UTC date of date. Rejecting the line instead would 400
	// the batch, which the app never retries, so such a client's rings would
	// stop syncing for good; and the spec, the fixture corpus and the
	// reference receiver all accept it. Wrong east of UTC, as documented
	// there, but consistently so: one row per day, shifted.
	return msToTime(a.Date).UTC().Format("2006-01-02"), nil
}

// metadataJSON renders sample metadata as a JSON text or nil for NULL.
func metadataJSON(m map[string]any) (*string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	s := string(buf)
	return &s, nil
}

// quantityDuplicateTolerance is how far apart two start times of one sample
// UUID may be and still be the same sample to the near-duplicate guard.
const quantityDuplicateTolerance = time.Millisecond

// withoutStoredQuantity drops the quantity samples that are already stored —
// same owner and UUID, start within quantityDuplicateTolerance — and repeats of a UUID
// within the batch, so insertQuantity counts them as duplicates.
//
// quantity_samples' primary key is (user_id, uuid, start_ts), not uuid: it is a
// hypertable partitioned on start_ts, and a unique index must include the
// partitioning column. So ON CONFLICT only catches a re-send whose start lands
// on the stored microsecond. Wire times are float epoch milliseconds, and two
// client encoder versions can round the same HealthKit instant a fraction of a
// microsecond apart, across a microsecond boundary: on 2026-09-04 a history
// re-send stored 106,919 second copies whose start_ts was 1 µs off the first
// (removed 2026-10-06). A HealthKit UUID names one immutable sample, so a
// stored row with this UUID and nearly the same start is this sample.
//
// The lookup is bounded to the batch's start range (± the tolerance) and its
// types so TimescaleDB excludes chunks at plan time and, in compressed chunks,
// skips columnstore batches by type segment, start_ts min/max and the uuid
// bloom filter. Measured on production (7.1M rows, 107 monthly chunks, 105
// compressed) with 1,000-sample batches: ~1 ms for a recent page, ~0.3–2.5 ms
// for a historical page in a compressed chunk; a pathological batch of 1,000
// random samples spread over seven years scans every chunk and takes ~0.6 s.
//
// Two concurrent batches carrying one sample a microsecond apart can still
// both insert; the client never uploads the same type from two runs at once.
func withoutStoredQuantity(ctx context.Context, tx pgx.Tx, samples []Sample,
	typeIDs map[string]int16, userID string) ([]Sample, error) {
	if len(samples) == 0 {
		return samples, nil
	}
	seen := make(map[string]bool, len(samples))
	kept := make([]Sample, 0, len(samples))
	uuids := make([]string, 0, len(samples))
	var tids []int16
	var lo, hi time.Time
	for i := range samples {
		u := strings.ToLower(samples[i].UUID)
		if seen[u] {
			continue
		}
		seen[u] = true
		kept = append(kept, samples[i])
		uuids = append(uuids, u)
		if tid := typeIDs[samples[i].Type]; !slices.Contains(tids, tid) {
			tids = append(tids, tid)
		}
		start := msToTime(samples[i].Start)
		if len(kept) == 1 || start.Before(lo) {
			lo = start
		}
		if len(kept) == 1 || start.After(hi) {
			hi = start
		}
	}

	rows, err := tx.Query(ctx, `
		SELECT uuid::text, start_ts FROM quantity_samples
		WHERE uuid = ANY($1::uuid[]) AND type_id = ANY($2::smallint[])
		  AND start_ts BETWEEN $3 AND $4 AND user_id = $5`,
		uuids, tids, lo.Add(-quantityDuplicateTolerance), hi.Add(quantityDuplicateTolerance), userID)
	if err != nil {
		return nil, err
	}
	stored := make(map[string][]time.Time)
	for rows.Next() {
		var u string
		var ts time.Time
		if err := rows.Scan(&u, &ts); err != nil {
			rows.Close()
			return nil, err
		}
		stored[u] = append(stored[u], ts)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return kept, nil
	}

	fresh := kept[:0]
	for i := range kept {
		start := msToTime(kept[i].Start)
		if !slices.ContainsFunc(stored[strings.ToLower(kept[i].UUID)], func(ts time.Time) bool {
			d := ts.Sub(start)
			return d >= -quantityDuplicateTolerance && d <= quantityDuplicateTolerance
		}) {
			fresh = append(fresh, kept[i])
		}
	}
	return fresh, nil
}

func insertQuantity(ctx context.Context, tx pgx.Tx, samples []Sample,
	typeIDs map[string]int16, sourceIDs map[sourceKey]int16,
	temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	samples, err := withoutStoredQuantity(ctx, tx, samples, typeIDs, userID)
	if err != nil {
		return 0, fmt.Errorf("near-duplicate guard: %w", err)
	}
	if len(samples) == 0 {
		return 0, nil
	}
	n := len(samples)
	uuids := make([]string, n)
	tids := make([]int16, n)
	starts := make([]time.Time, n)
	ends := make([]time.Time, n)
	startContextIDs := make([]*int32, n)
	endContextIDs := make([]*int32, n)
	values := make([]*float64, n)
	srcs := make([]int16, n)
	metas := make([]*string, n)
	for i := range samples {
		s := &samples[i]
		uuids[i] = s.UUID
		tids[i] = typeIDs[s.Type]
		starts[i] = msToTime(s.Start)
		ends[i] = msToTime(s.End)
		startContextIDs[i] = temporalContextID(temporalContextIDs, s.StartContext)
		endContextIDs[i] = temporalContextID(temporalContextIDs, s.EndContext)
		values[i] = s.Value
		srcs[i] = sourceIDs[sampleSourceKey(s)]
		m, err := metadataJSON(s.Metadata)
		if err != nil {
			return 0, err
		}
		metas[i] = m
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO quantity_samples (uuid, type_id, start_ts, end_ts,
		                              start_temporal_context_id, end_temporal_context_id,
		                              value, source_id, metadata, user_id)
		SELECT x.uuid, x.type_id, x.start_ts, x.end_ts,
		       x.start_temporal_context_id, x.end_temporal_context_id,
		       x.value, x.source_id, x.metadata::jsonb, $10::uuid
		FROM unnest($1::uuid[], $2::smallint[], $3::timestamptz[], $4::timestamptz[],
		            $5::integer[], $6::integer[], $7::float8[], $8::smallint[], $9::text[])
		     AS x(uuid, type_id, start_ts, end_ts,
		          start_temporal_context_id, end_temporal_context_id, value, source_id, metadata)
		ON CONFLICT DO NOTHING`,
		uuids, tids, starts, ends, startContextIDs, endContextIDs, values, srcs, metas, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func insertCategory(ctx context.Context, tx pgx.Tx, samples []Sample,
	typeIDs map[string]int16, sourceIDs map[sourceKey]int16,
	temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	if len(samples) == 0 {
		return 0, nil
	}
	n := len(samples)
	uuids := make([]string, n)
	tids := make([]int16, n)
	starts := make([]time.Time, n)
	ends := make([]time.Time, n)
	startContextIDs := make([]*int32, n)
	endContextIDs := make([]*int32, n)
	values := make([]int16, n)
	srcs := make([]int16, n)
	metas := make([]*string, n)
	for i := range samples {
		s := &samples[i]
		uuids[i] = s.UUID
		tids[i] = typeIDs[s.Type]
		starts[i] = msToTime(s.Start)
		ends[i] = msToTime(s.End)
		startContextIDs[i] = temporalContextID(temporalContextIDs, s.StartContext)
		endContextIDs[i] = temporalContextID(temporalContextIDs, s.EndContext)
		values[i] = *s.Category // validated non-nil in parser
		srcs[i] = sourceIDs[sampleSourceKey(s)]
		m, err := metadataJSON(s.Metadata)
		if err != nil {
			return 0, err
		}
		metas[i] = m
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO category_samples (uuid, type_id, start_ts, end_ts,
		                              start_temporal_context_id, end_temporal_context_id,
		                              value, source_id, metadata, user_id)
		SELECT x.uuid, x.type_id, x.start_ts, x.end_ts,
		       x.start_temporal_context_id, x.end_temporal_context_id,
		       x.value, x.source_id, x.metadata::jsonb, $10::uuid
		FROM unnest($1::uuid[], $2::smallint[], $3::timestamptz[], $4::timestamptz[],
		            $5::integer[], $6::integer[], $7::smallint[], $8::smallint[], $9::text[])
		     AS x(uuid, type_id, start_ts, end_ts,
		          start_temporal_context_id, end_temporal_context_id, value, source_id, metadata)
		ON CONFLICT DO NOTHING`,
		uuids, tids, starts, ends, startContextIDs, endContextIDs, values, srcs, metas, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func insertWorkouts(ctx context.Context, tx pgx.Tx, samples []Sample,
	sourceIDs map[sourceKey]int16, temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	if len(samples) == 0 {
		return 0, nil
	}
	n := len(samples)
	uuids := make([]string, n)
	activities := make([]string, n)
	starts := make([]time.Time, n)
	ends := make([]time.Time, n)
	startContextIDs := make([]*int32, n)
	endContextIDs := make([]*int32, n)
	durations := make([]float64, n)
	energies := make([]*float64, n)
	distances := make([]*float64, n)
	stats := make([]*string, n)
	statsDetail := make([]*string, n)
	events := make([]*string, n)
	acts := make([]*string, n)
	srcs := make([]int16, n)
	metas := make([]*string, n)
	for i := range samples {
		s := &samples[i]
		w := s.Workout // validated non-nil in parser
		uuids[i] = s.UUID
		activities[i] = w.ActivityType
		starts[i] = msToTime(s.Start)
		ends[i] = msToTime(s.End)
		startContextIDs[i] = temporalContextID(temporalContextIDs, s.StartContext)
		endContextIDs[i] = temporalContextID(temporalContextIDs, s.EndContext)
		durations[i] = w.Duration
		energies[i] = w.TotalEnergyKcal
		distances[i] = w.TotalDistanceMeters
		var err error
		if stats[i], err = jsonOrNil(len(w.Statistics) > 0, w.Statistics); err != nil {
			return 0, err
		}
		if statsDetail[i], err = jsonOrNil(len(w.StatisticsDetail) > 0, w.StatisticsDetail); err != nil {
			return 0, err
		}
		if events[i], err = jsonOrNil(len(w.Events) > 0, w.Events); err != nil {
			return 0, err
		}
		if acts[i], err = jsonOrNil(len(w.Activities) > 0, w.Activities); err != nil {
			return 0, err
		}
		srcs[i] = sourceIDs[sampleSourceKey(s)]
		m, err := metadataJSON(s.Metadata)
		if err != nil {
			return 0, err
		}
		metas[i] = m
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO workouts (uuid, activity_type, start_ts, end_ts, duration_s,
		                      start_temporal_context_id, end_temporal_context_id,
		                      energy_kcal, distance_m, stats, stats_detail, events,
		                      activities, source_id, metadata, user_id)
		SELECT x.uuid, x.activity_type, x.start_ts, x.end_ts, x.duration_s,
		       x.start_temporal_context_id, x.end_temporal_context_id,
		       x.energy_kcal, x.distance_m, x.stats::jsonb, x.stats_detail::jsonb,
		       x.events::jsonb, x.activities::jsonb, x.source_id, x.metadata::jsonb, $16::uuid
		FROM unnest($1::uuid[], $2::text[], $3::timestamptz[], $4::timestamptz[],
		            $5::float8[], $6::integer[], $7::integer[], $8::float8[], $9::float8[],
		            $10::text[], $11::text[], $12::text[], $13::text[], $14::smallint[], $15::text[])
		     AS x(uuid, activity_type, start_ts, end_ts, duration_s,
		          start_temporal_context_id, end_temporal_context_id,
		          energy_kcal, distance_m, stats, stats_detail, events,
		          activities, source_id, metadata)
		ON CONFLICT DO NOTHING`,
		uuids, activities, starts, ends, durations, startContextIDs, endContextIDs,
		energies, distances, stats, statsDetail, events, acts, srcs, metas, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// jsonOrNil marshals v to a *string when include is true, else returns nil
// (so the column stays SQL NULL).
func jsonOrNil(include bool, v any) (*string, error) {
	if !include {
		return nil, nil
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	str := string(buf)
	return &str, nil
}

// insertWorkoutSeriesPoints flattens all workout-series lines into one unnest
// insert and returns the number of newly inserted points.
func insertWorkoutSeriesPoints(ctx context.Context, tx pgx.Tx, series []SeriesLine,
	typeIDs map[string]int16, temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	total := 0
	for i := range series {
		total += len(series[i].Series.Points)
	}
	if total == 0 {
		return 0, nil
	}
	wuuids := make([]string, 0, total)
	tids := make([]int16, 0, total)
	ts := make([]time.Time, 0, total)
	contextIDs := make([]*int32, 0, total)
	values := make([]float64, 0, total)
	for i := range series {
		s := &series[i].Series
		tid := typeIDs[s.Type]
		for j := range s.Points {
			p := &s.Points[j]
			wuuids = append(wuuids, s.WorkoutUUID)
			tids = append(tids, tid)
			ts = append(ts, msToTime(p.T))
			contextIDs = append(contextIDs, temporalContextID(temporalContextIDs, p.TemporalContext))
			values = append(values, p.Value)
		}
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO workout_series_points (workout_uuid, type_id, ts, temporal_context_id, value, user_id)
		SELECT x.workout_uuid, x.type_id, x.ts, x.temporal_context_id, x.value, $6::uuid
		FROM unnest($1::uuid[], $2::smallint[], $3::timestamptz[], $4::integer[], $5::float8[])
		     AS x(workout_uuid, type_id, ts, temporal_context_id, value)
		ON CONFLICT DO NOTHING`,
		wuuids, tids, ts, contextIDs, values, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ensureUser inserts the owning user row if it is not already present, so the
// data tables' user_id foreign keys resolve. Identity fields (name/email/dob/
// sex) are filled in later by upsertUser from the profile line, if any.
func ensureUser(ctx context.Context, tx pgx.Tx, userID string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO users (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`, userID)
	return err
}

// upsertUser replaces the user's complete profile snapshot when a profile line
// is present. Nil fields deliberately clear stored values; an absent profile
// line is the only way to leave the profile unchanged.
const upsertUserSQL = `
	INSERT INTO users (id, name, email, dob, biological_sex, updated_at)
	VALUES ($1, $2, $3, $4, $5, now())
	ON CONFLICT (id) DO UPDATE SET
		name           = EXCLUDED.name,
		email          = EXCLUDED.email,
		dob            = EXCLUDED.dob,
		biological_sex = EXCLUDED.biological_sex,
		updated_at     = now()`

func upsertUser(ctx context.Context, tx pgx.Tx, userID string, p *ProfileLine) error {
	if p == nil {
		return nil
	}
	if p.Profile == nil {
		return errors.New("profile line missing profile object")
	}
	var dob *time.Time
	if p.Profile.DateOfBirth != nil {
		t := msToTime(*p.Profile.DateOfBirth)
		dob = &t
	}
	_, err := tx.Exec(ctx, upsertUserSQL,
		userID, p.Profile.Name, p.Profile.Email, dob, p.Profile.BiologicalSex)
	return err
}

func insertHeartbeatSeries(ctx context.Context, tx pgx.Tx, samples []Sample,
	sourceIDs map[sourceKey]int16, temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	if len(samples) == 0 {
		return 0, nil
	}
	n := len(samples)
	uuids := make([]string, n)
	starts := make([]time.Time, n)
	ends := make([]time.Time, n)
	startContextIDs := make([]*int32, n)
	endContextIDs := make([]*int32, n)
	counts := make([]int32, n)
	beats := make([]string, n)
	srcs := make([]int16, n)
	metas := make([]*string, n)
	for i := range samples {
		s := &samples[i]
		uuids[i] = s.UUID
		starts[i] = msToTime(s.Start)
		ends[i] = msToTime(s.End)
		startContextIDs[i] = temporalContextID(temporalContextIDs, s.StartContext)
		endContextIDs[i] = temporalContextID(temporalContextIDs, s.EndContext)
		counts[i] = int32(len(s.Heartbeats))
		buf, err := json.Marshal(s.Heartbeats) // validated non-nil in parser
		if err != nil {
			return 0, err
		}
		beats[i] = string(buf)
		srcs[i] = sourceIDs[sampleSourceKey(s)]
		m, err := metadataJSON(s.Metadata)
		if err != nil {
			return 0, err
		}
		metas[i] = m
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO heartbeat_series (uuid, start_ts, end_ts,
		                              start_temporal_context_id, end_temporal_context_id,
		                              beat_count, beats, source_id, metadata, user_id)
		SELECT x.uuid, x.start_ts, x.end_ts,
		       x.start_temporal_context_id, x.end_temporal_context_id,
		       x.beat_count, x.beats::jsonb, x.source_id, x.metadata::jsonb, $10::uuid
		FROM unnest($1::uuid[], $2::timestamptz[], $3::timestamptz[],
		            $4::integer[], $5::integer[], $6::int[], $7::text[], $8::smallint[], $9::text[])
		     AS x(uuid, start_ts, end_ts,
		          start_temporal_context_id, end_temporal_context_id, beat_count, beats, source_id, metadata)
		ON CONFLICT DO NOTHING`,
		uuids, starts, ends, startContextIDs, endContextIDs, counts, beats, srcs, metas, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// insertECG inserts row by row: voltage_uv is real[], which cannot ride the
// unnest pattern, and ECGs arrive at most a few per batch.
func insertECG(ctx context.Context, tx pgx.Tx, samples []Sample,
	sourceIDs map[sourceKey]int16, temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	var inserted int64
	for i := range samples {
		s := &samples[i]
		e := s.ECG // validated non-nil in parser
		m, err := metadataJSON(s.Metadata)
		if err != nil {
			return inserted, err
		}
		tag, err := tx.Exec(ctx, `
				INSERT INTO ecg_samples (uuid, start_ts, end_ts, classification, average_hr_bpm,
				                         start_temporal_context_id, end_temporal_context_id,
				                         sampling_hz, symptoms_status, voltage_uv, source_id, metadata, user_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13::uuid)
				ON CONFLICT DO NOTHING`,
			s.UUID, msToTime(s.Start), msToTime(s.End), e.Classification, e.AverageHeartRateBpm,
			temporalContextID(temporalContextIDs, s.StartContext),
			temporalContextID(temporalContextIDs, s.EndContext),
			e.SamplingFrequencyHz, e.SymptomsStatus, e.VoltagesUV,
			sourceIDs[sampleSourceKey(s)], m, userID)
		if err != nil {
			return inserted, err
		}
		inserted += tag.RowsAffected()
	}
	return inserted, nil
}

// insertStateOfMind inserts row by row: labels/associations are text[],
// which cannot ride the unnest pattern, and entries are rare.
func insertStateOfMind(ctx context.Context, tx pgx.Tx, samples []Sample,
	sourceIDs map[sourceKey]int16, temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	var inserted int64
	for i := range samples {
		s := &samples[i]
		som := s.StateOfMind // validated non-nil in parser
		m, err := metadataJSON(s.Metadata)
		if err != nil {
			return inserted, err
		}
		tag, err := tx.Exec(ctx, `
				INSERT INTO state_of_mind (uuid, start_ts, end_ts, kind, valence, valence_class,
				                           start_temporal_context_id, end_temporal_context_id,
				                           labels, associations, source_id, metadata, user_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13::uuid)
				ON CONFLICT DO NOTHING`,
			s.UUID, msToTime(s.Start), msToTime(s.End), som.Kind, som.Valence,
			som.ValenceClassification,
			temporalContextID(temporalContextIDs, s.StartContext),
			temporalContextID(temporalContextIDs, s.EndContext),
			som.Labels, som.Associations,
			sourceIDs[sampleSourceKey(s)], m, userID)
		if err != nil {
			return inserted, err
		}
		inserted += tag.RowsAffected()
	}
	return inserted, nil
}

func insertMedicationDoses(ctx context.Context, tx pgx.Tx, samples []Sample,
	sourceIDs map[sourceKey]int16, temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	if len(samples) == 0 {
		return 0, nil
	}
	n := len(samples)
	uuids := make([]string, n)
	starts := make([]time.Time, n)
	ends := make([]time.Time, n)
	startContextIDs := make([]*int32, n)
	endContextIDs := make([]*int32, n)
	meds := make([]*string, n)
	medicationIDs := make([]*string, n)
	statuses := make([]string, n)
	scheds := make([]*time.Time, n)
	schedContextIDs := make([]*int32, n)
	qtys := make([]*float64, n)
	units := make([]*string, n)
	srcs := make([]int16, n)
	metas := make([]*string, n)
	for i := range samples {
		s := &samples[i]
		d := s.MedicationDose // validated non-nil in parser
		uuids[i] = s.UUID
		starts[i] = msToTime(s.Start)
		ends[i] = msToTime(s.End)
		startContextIDs[i] = temporalContextID(temporalContextIDs, s.StartContext)
		endContextIDs[i] = temporalContextID(temporalContextIDs, s.EndContext)
		meds[i] = d.Medication
		medicationIDs[i] = d.MedicationID
		statuses[i] = d.Status
		if d.ScheduledAt != nil {
			t := msToTime(*d.ScheduledAt)
			scheds[i] = &t
			schedContextIDs[i] = temporalContextID(temporalContextIDs, d.ScheduledAtContext)
		}
		qtys[i] = d.DoseQuantity
		units[i] = d.DoseUnit
		srcs[i] = sourceIDs[sampleSourceKey(s)]
		m, err := metadataJSON(s.Metadata)
		if err != nil {
			return 0, err
		}
		metas[i] = m
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO medication_dose_events (uuid, start_ts, end_ts, medication, status,
		                                    start_temporal_context_id, end_temporal_context_id,
		                                    scheduled_ts, scheduled_temporal_context_id,
		                                    dose_quantity, dose_unit, source_id, metadata, medication_id, user_id)
		SELECT x.uuid, x.start_ts, x.end_ts, x.medication, x.status,
		       x.start_temporal_context_id, x.end_temporal_context_id,
		       x.scheduled_ts, x.scheduled_temporal_context_id,
		       x.dose_quantity, x.dose_unit, x.source_id, x.metadata::jsonb, x.medication_id, $15::uuid
		FROM unnest($1::uuid[], $2::timestamptz[], $3::timestamptz[], $4::text[], $5::text[],
		            $6::integer[], $7::integer[], $8::timestamptz[], $9::integer[],
		            $10::float8[], $11::text[], $12::smallint[], $13::text[], $14::text[])
		     AS x(uuid, start_ts, end_ts, medication, status,
		          start_temporal_context_id, end_temporal_context_id,
		          scheduled_ts, scheduled_temporal_context_id,
		          dose_quantity, dose_unit, source_id, metadata, medication_id)
		ON CONFLICT DO NOTHING`,
		uuids, starts, ends, meds, statuses, startContextIDs, endContextIDs,
		scheds, schedContextIDs, qtys, units, srcs, metas, medicationIDs, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// insertRoutePoints flattens all route lines into one unnest insert and
// returns the number of newly inserted points.
func insertRoutePoints(ctx context.Context, tx pgx.Tx, routes []RouteLine,
	temporalContextIDs map[temporalContextKey]int32, userID string) (int64, error) {
	total := 0
	for i := range routes {
		total += len(routes[i].Route.Points)
	}
	if total == 0 {
		return 0, nil
	}
	wuuids := make([]string, 0, total)
	ts := make([]time.Time, 0, total)
	contextIDs := make([]*int32, 0, total)
	lats := make([]float64, 0, total)
	lons := make([]float64, 0, total)
	alts := make([]*float64, 0, total)
	haccs := make([]*float64, 0, total)
	vaccs := make([]*float64, 0, total)
	speeds := make([]*float64, 0, total)
	courses := make([]*float64, 0, total)
	for i := range routes {
		r := &routes[i].Route
		for j := range r.Points {
			p := &r.Points[j]
			wuuids = append(wuuids, r.WorkoutUUID)
			ts = append(ts, msToTime(p.T))
			contextIDs = append(contextIDs, temporalContextID(temporalContextIDs, p.TemporalContext))
			lats = append(lats, p.Lat)
			lons = append(lons, p.Lon)
			alts = append(alts, p.Alt)
			haccs = append(haccs, p.HAcc)
			vaccs = append(vaccs, p.VAcc)
			speeds = append(speeds, p.Speed)
			courses = append(courses, p.Course)
		}
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO workout_route_points (workout_uuid, ts, temporal_context_id, lat, lon, altitude_m,
		                                  h_acc_m, v_acc_m, speed_mps, course_deg, user_id)
		SELECT x.workout_uuid, x.ts, x.temporal_context_id, x.lat, x.lon, x.altitude_m,
		       x.h_acc_m, x.v_acc_m, x.speed_mps, x.course_deg, $11::uuid
		FROM unnest($1::uuid[], $2::timestamptz[], $3::integer[], $4::float8[], $5::float8[],
		            $6::float8[], $7::float8[], $8::float8[], $9::float8[], $10::float8[])
		     AS x(workout_uuid, ts, temporal_context_id, lat, lon, altitude_m,
		          h_acc_m, v_acc_m, speed_mps, course_deg)
		ON CONFLICT DO NOTHING`,
		wuuids, ts, contextIDs, lats, lons, alts, haccs, vaccs, speeds, courses, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// applyDeletions removes tombstoned samples from every sample table and
// records the tombstones. Returns the number of rows actually removed.
func applyDeletions(ctx context.Context, tx pgx.Tx, dels []Deletion, userID string) (int64, error) {
	if len(dels) == 0 {
		return 0, nil
	}
	uuids := make([]string, len(dels))
	types := make([]string, len(dels))
	for i, d := range dels {
		uuids[i] = d.Deleted.UUID
		types[i] = d.Deleted.Type
	}

	var deleted int64
	// quantity_samples is a compressed hypertable: a bare uuid predicate can't
	// prune columnstore batches (segmentby type_id, orderby start_ts), so a
	// direct DELETE decompresses the whole table. deleteQuantitySamples
	// (quantity_delete.go) resolves the rows on the read path and deletes them
	// in a few range-bounded statements that keep chunk and batch pruning.
	n, err := deleteQuantitySamples(ctx, tx, uuids, userID)
	if err != nil {
		return deleted, fmt.Errorf("delete from quantity_samples: %w", err)
	}
	deleted += n

	// Deletions only ever touch the issuing user's rows (UUIDs are globally
	// unique per HealthKit store, but scoping by user_id keeps one user from
	// ever removing another's data).
	for _, table := range []string{"category_samples", "workouts",
		"heartbeat_series", "ecg_samples", "state_of_mind", "medication_dose_events"} {
		tag, err := tx.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE uuid = ANY($1::uuid[]) AND user_id = $2`, table), uuids, userID)
		if err != nil {
			return deleted, fmt.Errorf("delete from %s: %w", table, err)
		}
		deleted += tag.RowsAffected()
	}

	// A deleted workout takes its route and series streams along; neither are
	// samples, so they don't count toward the deleted total. workout_uuid is the
	// columnstore segmentby key, so these deletes prune compressed batches.
	if _, err := tx.Exec(ctx,
		`DELETE FROM workout_route_points WHERE workout_uuid = ANY($1::uuid[]) AND user_id = $2`, uuids, userID); err != nil {
		return deleted, fmt.Errorf("delete route points: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM workout_series_points WHERE workout_uuid = ANY($1::uuid[]) AND user_id = $2`, uuids, userID); err != nil {
		return deleted, fmt.Errorf("delete series points: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO deleted_samples (uuid, type_identifier, user_id)
		SELECT x.uuid, x.type_identifier, $3::uuid
		FROM unnest($1::uuid[], $2::text[]) AS x(uuid, type_identifier)
		ON CONFLICT (user_id, uuid) DO NOTHING`,
		uuids, types, userID)
	if err != nil {
		return deleted, fmt.Errorf("record tombstones: %w", err)
	}
	return deleted, nil
}

// TypeStats is one element of the GET /v1/stats response.
type TypeStats struct {
	Type        string `json:"type"`
	Rows        int64  `json:"rows"`
	Earliest    *int64 `json:"earliest"`
	Latest      *int64 `json:"latest"`
	LastBatchAt *int64 `json:"lastBatchAt"`
	Batches     int64  `json:"batches"`
}

// Stats aggregates per-type row counts and batch bookkeeping so the app can
// reconcile server-side state with on-device anchors.
func (st *Store) Stats(ctx context.Context, scope recordScope) ([]TypeStats, error) {
	var sourceID any
	if scope.sourceConnectionID != "" {
		sourceID = scope.sourceConnectionID
	}
	rows, err := st.pool.Query(ctx, `
		WITH scoped_records AS (
			SELECT physical_uuid,physical_table FROM family_record_identity WHERE user_id=$1 AND source_connection_id=$2
		), per_table AS (
			SELECT t.identifier, count(*) AS rows, min(q.start_ts) AS earliest, max(q.start_ts) AS latest
			FROM quantity_samples q JOIN sample_types t USING (type_id)
			WHERE q.user_id = $1 AND ($2::uuid IS NULL OR q.uuid IN
				(SELECT physical_uuid FROM scoped_records WHERE physical_table='quantity_samples')) GROUP BY 1
			UNION ALL
			SELECT t.identifier, count(*), min(c.start_ts), max(c.start_ts)
			FROM category_samples c JOIN sample_types t USING (type_id)
			WHERE c.user_id = $1 AND ($2::uuid IS NULL OR c.uuid IN
				(SELECT physical_uuid FROM scoped_records WHERE physical_table='category_samples')) GROUP BY 1
			UNION ALL
			SELECT 'HKWorkoutTypeIdentifier', count(*), min(start_ts), max(start_ts)
			FROM workouts WHERE user_id = $1 AND ($2::uuid IS NULL OR uuid IN
				(SELECT physical_uuid FROM scoped_records WHERE physical_table='workouts')) HAVING count(*) > 0
			UNION ALL
			SELECT 'HKDataTypeIdentifierHeartbeatSeries', count(*), min(start_ts), max(start_ts)
			FROM heartbeat_series WHERE user_id = $1 AND ($2::uuid IS NULL OR uuid IN
				(SELECT physical_uuid FROM scoped_records WHERE physical_table='heartbeat_series')) HAVING count(*) > 0
			UNION ALL
			SELECT 'HKDataTypeIdentifierElectrocardiogram', count(*), min(start_ts), max(start_ts)
			FROM ecg_samples WHERE user_id = $1 AND ($2::uuid IS NULL OR uuid IN
				(SELECT physical_uuid FROM scoped_records WHERE physical_table='ecg_samples')) HAVING count(*) > 0
			UNION ALL
			SELECT 'HKDataTypeIdentifierStateOfMind', count(*), min(start_ts), max(start_ts)
			FROM state_of_mind WHERE user_id = $1 AND ($2::uuid IS NULL OR uuid IN
				(SELECT physical_uuid FROM scoped_records WHERE physical_table='state_of_mind')) HAVING count(*) > 0
			UNION ALL
			SELECT 'HKMedicationDoseEventTypeIdentifierMedicationDoseEvent', count(*), min(start_ts), max(start_ts)
			FROM medication_dose_events WHERE user_id = $1 AND ($2::uuid IS NULL OR uuid IN
				(SELECT physical_uuid FROM scoped_records WHERE physical_table='medication_dose_events')) HAVING count(*) > 0
			UNION ALL
			SELECT 'HKActivitySummaryTypeIdentifier', count(*), min(date)::timestamptz, max(date)::timestamptz
			FROM activity_summaries WHERE user_id = $1 AND $2::uuid IS NULL HAVING count(*) > 0
		), samples AS (
			SELECT identifier, sum(rows)::bigint AS rows, min(earliest) AS earliest, max(latest) AS latest
			FROM per_table GROUP BY 1
		), batch_agg AS (
			SELECT type_identifier, max(received_at) AS last_batch_at, count(*)::bigint AS batches
			FROM batches WHERE user_id = $1 AND ($2::uuid IS NULL OR device_token_id IN
				(SELECT id FROM device_tokens WHERE user_id=$1 AND source_connection_id=$2)) GROUP BY 1
		)
		SELECT coalesce(s.identifier, b.type_identifier) AS type,
		       coalesce(s.rows, 0),
		       (extract(epoch FROM s.earliest) * 1000)::bigint,
		       (extract(epoch FROM s.latest) * 1000)::bigint,
		       (extract(epoch FROM b.last_batch_at) * 1000)::bigint,
		       coalesce(b.batches, 0)
		FROM samples s
		FULL OUTER JOIN batch_agg b ON b.type_identifier = s.identifier
		ORDER BY 1`, scope.userID, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TypeStats{}
	for rows.Next() {
		var ts TypeStats
		if err := rows.Scan(&ts.Type, &ts.Rows, &ts.Earliest, &ts.Latest, &ts.LastBatchAt, &ts.Batches); err != nil {
			return nil, err
		}
		out = append(out, ts)
	}
	return out, rows.Err()
}

// tableForKind maps a sample_types kind to the table holding its rows.
// byType reports whether the table is shared and must be filtered by type_id.
func tableForKind(kind string) (table string, byType bool, ok bool) {
	switch kind {
	case "quantity":
		return "quantity_samples", true, true
	case "category":
		return "category_samples", true, true
	case "workout":
		return "workouts", false, true
	case "heartbeatSeries":
		return "heartbeat_series", false, true
	case "ecg":
		return "ecg_samples", false, true
	case "stateOfMind":
		return "state_of_mind", false, true
	case "medicationDose":
		return "medication_dose_events", false, true
	}
	return "", false, false
}

// resolveType looks up a type identifier in sample_types and returns the
// table to query plus a type_id filter (nil for single-type tables).
// ok is false for identifiers (or kinds) this server has never ingested.
func (st *Store) resolveType(ctx context.Context, identifier string) (table string, typeID *int16, ok bool, err error) {
	var id int16
	var kind *string
	err = st.pool.QueryRow(ctx,
		`SELECT type_id, kind FROM sample_types WHERE identifier = $1`, identifier).Scan(&id, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	if kind == nil {
		return "", nil, false, nil
	}
	table, byType, ok := tableForKind(*kind)
	if !ok {
		return "", nil, false, nil
	}
	if byType {
		return table, &id, true, nil
	}
	return table, nil, true, nil
}

// typeRangeQuery builds the shared "rows of this type in [from, to)" query
// used by Digest and UUIDs.
func typeRangeQuery(columns, table string, scope recordScope, typeID *int16, from, to time.Time) (string, []any) {
	columns = strings.ReplaceAll(strings.ReplaceAll(columns, "uuid", "h.uuid"), "start_ts", "h.start_ts")
	join := ""
	if scope.sourceConnectionID != "" {
		columns = strings.ReplaceAll(columns, "h.uuid", "r.source_record_id")
		join = " JOIN family_record_identity r ON r.user_id=h.user_id AND r.physical_uuid=h.uuid AND r.physical_start=h.start_ts AND r.physical_table='" + table + "'"
	}
	query := fmt.Sprintf(`SELECT %s FROM %s h%s WHERE h.user_id = $1 AND h.start_ts >= $2 AND h.start_ts < $3`, columns, table, join)
	args := []any{scope.userID, from, to}
	if typeID != nil {
		query += ` AND h.type_id = $4`
		args = append(args, *typeID)
	}
	if scope.sourceConnectionID != "" {
		args = append(args, scope.sourceConnectionID)
		query += fmt.Sprintf(" AND r.source_connection_id=$%d", len(args))
	}
	return query + ` ORDER BY h.start_ts`, args
}

// uuidRawBytes decodes a canonical hyphenated UUID into its raw 16 bytes.
func uuidRawBytes(s string) ([16]byte, error) {
	var b [16]byte
	hexStr := strings.ReplaceAll(s, "-", "")
	if len(hexStr) != 32 {
		return b, fmt.Errorf("malformed uuid %q", s)
	}
	if _, err := hex.Decode(b[:], []byte(hexStr)); err != nil {
		return b, fmt.Errorf("malformed uuid %q: %w", s, err)
	}
	return b, nil
}

// monthStartMS returns the epoch ms of the first instant of t's UTC month.
func monthStartMS(t time.Time) int64 {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).UnixMilli()
}

// DigestWindow is one element of the GET /v1/digest response: the XOR of the
// raw UUID bytes of every sample in one UTC month.
type DigestWindow struct {
	Window int64  `json:"window"`
	Rows   int64  `json:"rows"`
	Digest string `json:"digest"`
}

// Digest computes per-UTC-month reconciliation digests over [from, to) by
// start_ts. Rows arrive ordered by start_ts and are folded month by month so
// large windows never materialize in memory. Unknown types yield [].
func (st *Store) Digest(ctx context.Context, scope recordScope, identifier string, from, to time.Time) ([]DigestWindow, error) {
	out := []DigestWindow{}
	table, typeID, ok, err := st.resolveType(ctx, identifier)
	if err != nil {
		return nil, err
	}
	if !ok {
		return out, nil
	}

	query, args := typeRangeQuery("uuid, start_ts", table, scope, typeID, from, to)
	rows, err := st.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		cur    DigestWindow
		digest [16]byte
		open   bool
	)
	flush := func() {
		if open {
			cur.Digest = hex.EncodeToString(digest[:])
			out = append(out, cur)
		}
	}
	for rows.Next() {
		var u string
		var ts time.Time
		if err := rows.Scan(&u, &ts); err != nil {
			return nil, err
		}
		raw, err := uuidRawBytes(u)
		if err != nil {
			return nil, err
		}
		if w := monthStartMS(ts); !open || w != cur.Window {
			flush()
			cur = DigestWindow{Window: w}
			digest = [16]byte{}
			open = true
		}
		for i := range raw {
			digest[i] ^= raw[i]
		}
		cur.Rows++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}

// UUIDs returns the uuids of all samples of the given type with start_ts in
// [from, to). The handler bounds the range, so the result stays small.
func (st *Store) UUIDs(ctx context.Context, scope recordScope, identifier string, from, to time.Time) ([]string, error) {
	out := []string{}
	table, typeID, ok, err := st.resolveType(ctx, identifier)
	if err != nil {
		return nil, err
	}
	if !ok {
		return out, nil
	}

	query, args := typeRangeQuery("uuid", table, scope, typeID, from, to)
	rows, err := st.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Ping verifies database connectivity for /healthz.
func (st *Store) Ping(ctx context.Context) error {
	return st.pool.Ping(ctx)
}
