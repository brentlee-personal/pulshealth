package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Raw samples: GET /v1/samples serves the individual HealthKit records of
// one quantity or category type, exactly as synced — no deduplication
// across an iPhone and an Apple Watch that recorded the same minutes (that
// is what /v1/metrics/daily is for).

const (
	defaultSampleLimit = 1000
	maxSampleLimit     = 5000
	// Raw samples of a busy type (heart rate every few seconds) run to
	// hundreds of thousands a month; the range is bounded so one request
	// cannot ask for years of them.
	maxSampleRange = 31 * 24 * time.Hour
)

// SampleFilters are the query parameters of GET /v1/samples; the range is
// [Start, End) on the sample's start time.
type SampleFilters struct {
	Type   string
	Start  time.Time
	End    time.Time
	Limit  int
	Offset int
}

// SampleMeta is the sample_types row behind a raw-sample query: the numeric
// type id the sample tables key on, plus the kind and canonical unit the
// answer reports. Resolving it is a separate step so a caller that streams
// (GET /v1/export) learns about a bad identifier before it has written a
// single byte of the response.
type SampleMeta struct {
	Type   string
	TypeID int16
	Kind   string
	Unit   *string
}

// Sample is one raw HealthKit sample. Value is the numeric value in the
// type's canonical unit for a quantity type and the integer enum value for a
// category type, where Label (from category_labels) names it.
type Sample struct {
	UUID   string   `json:"uuid"`
	Start  int64    `json:"start"`
	End    int64    `json:"end"`
	Value  *float64 `json:"value"`
	Label  *string  `json:"label,omitempty"`
	Source *string  `json:"source"`
}

// SamplesPage is the envelope of GET /v1/samples.
type SamplesPage struct {
	Type       string   `json:"type"`
	Kind       string   `json:"kind"`
	Unit       *string  `json:"unit"`
	Samples    []Sample `json:"samples"`
	NextOffset int      `json:"nextOffset"`
}

// SampleType resolves a HealthKit identifier to the sample_types row the
// raw-sample queries need. An identifier the database has never seen, or one
// that is not a quantity or category type, is a request error — the caller's
// fault, answered with a 400 rather than logged as a 500.
func (st *Store) SampleType(ctx context.Context, identifier string) (SampleMeta, error) {
	meta := SampleMeta{Type: identifier}
	var kind *string
	err := st.readDB(ctx).QueryRow(ctx, `
		SELECT type_id, kind, unit FROM sample_types WHERE identifier = $1`, identifier).
		Scan(&meta.TypeID, &kind, &meta.Unit)
	if errors.Is(err, pgx.ErrNoRows) {
		return SampleMeta{}, badRequestf("unknown type %q: it has never been synced (see /v1/catalog/types)", identifier)
	}
	if err != nil {
		return SampleMeta{}, err
	}
	if kind == nil {
		return SampleMeta{}, badRequestf("type %q has no kind recorded; only quantity and category types have samples", identifier)
	}
	if *kind != "quantity" && *kind != "category" {
		return SampleMeta{}, badRequestf("type %q is a %s type; only quantity and category samples are served here (workouts have /v1/workouts)", identifier, *kind)
	}
	meta.Kind = *kind
	return meta, nil
}

// StreamSamples bounds each database snapshot and rechecks authority before emission.
func (st *Store) StreamSamples(ctx context.Context, userID string, meta SampleMeta, f SampleFilters, fn func(Sample) error) error {
	var after *readCursor
	sent, offset := 0, f.Offset
	for {
		if err := st.checkReadScope(ctx); err != nil {
			return err
		}
		pageFilters := f
		pageFilters.Limit, pageFilters.Offset = readPageSize, offset
		if f.Limit > 0 {
			if sent >= f.Limit {
				return nil
			}
			pageFilters.Limit = min(readPageSize, f.Limit-sent)
		}
		page, next, err := st.samplePage(ctx, userID, meta, pageFilters, after)
		if err != nil {
			return err
		}
		if err := st.checkReadScope(ctx); err != nil {
			return err
		}
		for _, sample := range page {
			if err := fn(sample); err != nil {
				return err
			}
		}
		sent += len(page)
		if err := st.checkReadScope(ctx); err != nil {
			return err
		}
		if len(page) < pageFilters.Limit {
			return nil
		}
		after, offset = next, 0
	}
}

func (st *Store) samplePage(ctx context.Context, userID string, meta SampleMeta, f SampleFilters, after *readCursor) ([]Sample, *readCursor, error) {
	var afterAt, afterID any
	if after != nil {
		afterAt, afterID = after.at, after.id
	}
	var (
		rows pgx.Rows
		err  error
	)
	switch meta.Kind {
	case "quantity":
		rows, err = st.readDB(ctx).Query(ctx, `
			SELECT q.uuid::text, q.start_ts, q.end_ts, q.value::float8, s.name
			FROM quantity_samples q
			LEFT JOIN sources s ON s.source_id = q.source_id
			WHERE q.user_id = $1
			  AND q.type_id = $2
			  AND q.start_ts >= $3
			  AND q.start_ts < $4
			  AND ($7::timestamptz IS NULL OR (q.start_ts,q.uuid)>($7,$8::uuid))
			ORDER BY q.start_ts, q.uuid
			LIMIT $5 OFFSET $6`,
			userID, meta.TypeID, f.Start, f.End, f.Limit, f.Offset, afterAt, afterID)
	case "category":
		// Category values are only meaningful with their type; the label
		// join is keyed on the identifier so the same integer decodes
		// differently for sleep and for a symptom.
		rows, err = st.readDB(ctx).Query(ctx, `
			SELECT c.uuid::text, c.start_ts, c.end_ts, c.value::float8, s.name, cl.label
			FROM category_samples c
			LEFT JOIN sources s ON s.source_id = c.source_id
			LEFT JOIN category_labels cl ON cl.type_identifier = $7 AND cl.value = c.value
			WHERE c.user_id = $1
			  AND c.type_id = $2
			  AND c.start_ts >= $3
			  AND c.start_ts < $4
			  AND ($8::timestamptz IS NULL OR (c.start_ts,c.uuid)>($8,$9::uuid))
			ORDER BY c.start_ts, c.uuid
			LIMIT $5 OFFSET $6`,
			userID, meta.TypeID, f.Start, f.End, f.Limit, f.Offset, meta.Type, afterAt, afterID)
	default:
		return nil, nil, badRequestf("type %q is a %s type; only quantity and category samples are served here (workouts have /v1/workouts)", meta.Type, meta.Kind)
	}
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	out := make([]Sample, 0, f.Limit)
	var last *readCursor
	for rows.Next() {
		var (
			s          Sample
			start, end time.Time
		)
		dest := []any{&s.UUID, &start, &end, &s.Value, &s.Source}
		if meta.Kind == "category" {
			dest = append(dest, &s.Label)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		s.Start = start.UTC().UnixMilli()
		s.End = end.UTC().UnixMilli()
		out = append(out, s)
		last = &readCursor{at: start, id: s.UUID}
	}
	return out, last, rows.Err()
}

// Samples collects one page of StreamSamples into the endpoint's envelope.
// An identifier the database has never seen, or one that is not a quantity
// or category type, is a request error.
func (st *Store) Samples(ctx context.Context, userID string, f SampleFilters) (*SamplesPage, error) {
	meta, err := st.SampleType(ctx, f.Type)
	if err != nil {
		return nil, err
	}
	page := &SamplesPage{Type: meta.Type, Kind: meta.Kind, Unit: meta.Unit, Samples: make([]Sample, 0)}
	if err := st.StreamSamples(ctx, userID, meta, f, func(s Sample) error {
		page.Samples = append(page.Samples, s)
		return nil
	}); err != nil {
		return nil, err
	}
	page.NextOffset = f.Offset + len(page.Samples)
	return page, nil
}
