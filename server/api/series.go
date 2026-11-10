package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Intra-workout streams: GET /v1/workouts/{uuid}/series serves the
// per-second curves the app records during a workout (heart rate, running
// power, speed, cadence, ...) from workout_series_points, downsampled to a
// bounded number of points per series.

const (
	defaultSeriesPoints = 500
	maxSeriesPoints     = 5000
)

// SeriesPoint is one [t, value] pair: epoch milliseconds and the value in
// the series' canonical unit. It marshals as a two-element JSON array to
// keep a 5,000-point series compact.
type SeriesPoint struct {
	T int64
	V float64
}

func (p SeriesPoint) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]any{p.T, p.V})
}

func (p *SeriesPoint) UnmarshalJSON(b []byte) error {
	var raw []float64
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("series point %s: want [t, value]", string(b))
	}
	p.T = int64(raw[0])
	p.V = raw[1]
	return nil
}

// WorkoutSeries is one type's stream within a workout.
type WorkoutSeries struct {
	Type string  `json:"type"`
	Unit *string `json:"unit"`
	// Points recorded before downsampling; len(Points) after.
	TotalPoints int           `json:"totalPoints"`
	Points      []SeriesPoint `json:"points"`
}

// WorkoutSeriesResponse is GET /v1/workouts/{uuid}/series.
type WorkoutSeriesResponse struct {
	UUID      string          `json:"uuid"`
	Start     int64           `json:"start"`
	End       int64           `json:"end"`
	MaxPoints int             `json:"maxPoints"`
	Series    []WorkoutSeries `json:"series"`
}

// WorkoutSeries returns the workout's streams, optionally only those of the
// given types, each downsampled to at most maxPoints. A nil response means
// the workout does not exist for this user.
func (st *Store) WorkoutSeries(ctx context.Context, userID, uuid string, types []string, maxPoints int) (*WorkoutSeriesResponse, error) {
	var start, end time.Time
	err := st.readDB(ctx).QueryRow(ctx, `
		SELECT start_ts, end_ts FROM workouts WHERE user_id = $1 AND uuid = $2`, userID, uuid).
		Scan(&start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := &WorkoutSeriesResponse{
		UUID:      uuid,
		Start:     start.UTC().UnixMilli(),
		End:       end.UTC().UnixMilli(),
		MaxPoints: maxPoints,
		Series:    make([]WorkoutSeries, 0),
	}

	// workout_series_points is columnstore-compressed by workout_uuid for
	// old chunks; the equality on it keeps the scan to one segment.
	rows, err := st.readDB(ctx).Query(ctx, `
		SELECT st.identifier, st.unit, p.ts, p.value::float8
		FROM workout_series_points p
		JOIN sample_types st ON st.type_id = p.type_id
		WHERE p.workout_uuid = $1
		  AND p.user_id = $2
		  AND ($3::text[] IS NULL OR st.identifier = ANY($3::text[]))
		ORDER BY st.identifier, p.ts`, uuid, userID, nullableTextArray(types))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var current *WorkoutSeries
	for rows.Next() {
		var (
			identifier string
			unit       *string
			ts         time.Time
			value      float64
		)
		if err := rows.Scan(&identifier, &unit, &ts, &value); err != nil {
			return nil, err
		}
		if current == nil || current.Type != identifier {
			out.Series = append(out.Series, WorkoutSeries{Type: identifier, Unit: unit})
			current = &out.Series[len(out.Series)-1]
		}
		current.Points = append(current.Points, SeriesPoint{T: ts.UTC().UnixMilli(), V: value})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out.Series {
		out.Series[i].TotalPoints = len(out.Series[i].Points)
		out.Series[i].Points = downsample(out.Series[i].Points, maxPoints)
	}
	return out, nil
}

// nullableTextArray passes an empty filter as SQL NULL so the query can
// skip the ANY() test instead of matching nothing.
func nullableTextArray(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return values
}

// downsample reduces points (ordered by time) to at most max by keeping the
// first and last point as they are and replacing the ones between with the
// average time and value of max-2 equal-count buckets. Fewer points than
// max come back untouched.
func downsample(points []SeriesPoint, max int) []SeriesPoint {
	n := len(points)
	if max <= 0 || n <= max {
		return points
	}
	if max == 1 {
		return points[:1]
	}
	out := make([]SeriesPoint, 0, max)
	out = append(out, points[0])
	inner := points[1 : n-1]
	buckets := max - 2
	base := points[0].T
	for b := 0; b < buckets; b++ {
		lo := b * len(inner) / buckets
		hi := (b + 1) * len(inner) / buckets
		if hi <= lo {
			continue
		}
		var (
			sumT int64
			sumV float64
		)
		for _, p := range inner[lo:hi] {
			sumT += p.T - base
			sumV += p.V
		}
		k := int64(hi - lo)
		out = append(out, SeriesPoint{
			T: base + (sumT+k/2)/k,
			V: sumV / float64(k),
		})
	}
	return append(out, points[n-1])
}
