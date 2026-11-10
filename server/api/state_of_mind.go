package main

import (
	"context"
	"time"
)

// State of Mind: GET /v1/state-of-mind serves the HKStateOfMind entries a
// person logs in the Health or Mindfulness app (iOS 18+): a momentary
// emotion, or a daily mood.

const maxStateOfMindDays = 366

// StateOfMindEntry is one row of GET /v1/state-of-mind.
type StateOfMindEntry struct {
	UUID string `json:"uuid"`
	// The local calendar day (PULS_TIME_ZONE) of the entry, and its instant.
	Date      string `json:"date"`
	Timestamp int64  `json:"timestamp"`
	// momentaryEmotion or dailyMood.
	Kind string `json:"kind"`
	// -1 (very unpleasant) to +1 (very pleasant), and Apple's name for the
	// band it falls in (veryUnpleasant, unpleasant, slightlyUnpleasant,
	// neutral, slightlyPleasant, pleasant, veryPleasant).
	Valence               *float64 `json:"valence"`
	ValenceClassification *string  `json:"valenceClassification"`
	// The feelings picked (e.g. calm, stressed) and what they are about
	// (e.g. work, family), as HealthKit names them.
	Labels       []string `json:"labels"`
	Associations []string `json:"associations"`
}

// StateOfMind returns the entries whose instant falls on a local calendar
// day overlapping [start, end), ordered by time.
func (st *Store) StateOfMind(ctx context.Context, userID string, start, end time.Time) ([]StateOfMindEntry, error) {
	loc, err := st.UserLocation(ctx, userID)
	if err != nil {
		return nil, err
	}
	firstDay, afterLastDay, err := localDayBounds(start, end, loc)
	if err != nil {
		return nil, err
	}
	if days := calendarDays(firstDay, afterLastDay); days > maxStateOfMindDays {
		return nil, badRequestf("range covers %d days; at most %d days per request", days, maxStateOfMindDays)
	}

	rows, err := st.readDB(ctx).Query(ctx, `
		SELECT uuid::text, start_ts, kind, valence::float8, valence_class,
		       COALESCE(labels, ARRAY[]::text[]), COALESCE(associations, ARRAY[]::text[])
		FROM state_of_mind
		WHERE user_id = $1
		  AND start_ts >= $2
		  AND start_ts < $3
		ORDER BY start_ts, uuid`, userID, firstDay, afterLastDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]StateOfMindEntry, 0)
	for rows.Next() {
		var (
			e  StateOfMindEntry
			ts time.Time
		)
		if err := rows.Scan(&e.UUID, &ts, &e.Kind, &e.Valence, &e.ValenceClassification, &e.Labels, &e.Associations); err != nil {
			return nil, err
		}
		e.Date = ts.In(loc).Format("2006-01-02")
		e.Timestamp = ts.UTC().UnixMilli()
		out = append(out, e)
	}
	return out, rows.Err()
}
