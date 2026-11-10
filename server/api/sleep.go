package main

import (
	"context"
	"sort"
	"time"
)

// Sleep nights.
//
// HealthKit stores sleep as HKCategoryTypeIdentifierSleepAnalysis category
// samples: one interval per stage — in bed, asleep (core, deep, REM, or the
// stage-less "asleep unspecified" that older devices and third-party apps
// write) and awake. Apple Health presents them as one session per night,
// attributed to the day the person wakes up; GET /v1/sleep/daily does the
// same. The web viewer's daily sleep series (web/lib/queries.ts,
// categoryAggregation) is the model: asleep time is the sum of the asleep
// stages, and because an iPhone, an Apple Watch and a third-party app can
// all record the same night, each measure is the highest single-source
// total rather than a sum across sources. Here that rule is applied per
// night instead of per day, so a row also carries when the night began and
// ended and the stage breakdown of the source that won it.

const (
	sleepTypeIdentifier = "HKCategoryTypeIdentifierSleepAnalysis"
	// Two samples further apart than this belong to different nights (or to
	// a night and a nap, which then gets its own row).
	sleepNightGap = 3 * time.Hour
	// A night can begin the evening before the first requested day, and a
	// night that ends on the last requested day can have samples after it
	// that decide whether it continues. The query is padded on both sides
	// so nights are segmented on complete data, then filtered by wake-up
	// date. 36 h covers any plausible night plus the gap.
	sleepQueryPadding = 36 * time.Hour
	// The endpoint reads raw category samples, so bound the span (in local
	// calendar days) the way the MCP tools bound theirs.
	maxSleepDays = 366
)

// SleepStages is the minutes spent in each stage during one night, from the
// single source that recorded the most asleep time.
type SleepStages struct {
	Core        float64 `json:"core"`
	Deep        float64 `json:"deep"`
	REM         float64 `json:"rem"`
	Unspecified float64 `json:"unspecified"`
	Awake       float64 `json:"awake"`
}

// SleepNight is one row of GET /v1/sleep/daily.
type SleepNight struct {
	// The local calendar day (PULS_TIME_ZONE) the night ended on: the
	// wake-up day.
	Date string `json:"date"`
	// Epoch milliseconds of the night's first and last sample, any source.
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	// Minutes in bed: the highest single-source total of in-bed samples.
	InBedMinutes float64 `json:"inBedMinutes"`
	// Minutes asleep (core + deep + REM + unspecified) of the source that
	// recorded the most; Stages breaks the same source down.
	AsleepMinutes float64     `json:"asleepMinutes"`
	Stages        SleepStages `json:"stages"`
	// Distinct sources that contributed samples to the night.
	Sources int `json:"sources"`
}

type sleepStage int

const (
	sleepStageUnknown sleepStage = iota
	sleepStageInBed
	sleepStageAsleepUnspecified
	sleepStageAwake
	sleepStageCore
	sleepStageDeep
	sleepStageREM
)

// sleepStageFromEnum decodes a sleep sample's value through its
// category_labels.enum_name (HKCategoryValueSleepAnalysis*), so the integer
// values never appear here. Unknown names — a value this seed does not
// know — map to sleepStageUnknown and count towards nothing.
func sleepStageFromEnum(enumName string) sleepStage {
	switch enumName {
	case "HKCategoryValueSleepAnalysisInBed":
		return sleepStageInBed
	case "HKCategoryValueSleepAnalysisAsleepUnspecified":
		return sleepStageAsleepUnspecified
	case "HKCategoryValueSleepAnalysisAwake":
		return sleepStageAwake
	case "HKCategoryValueSleepAnalysisAsleepCore":
		return sleepStageCore
	case "HKCategoryValueSleepAnalysisAsleepDeep":
		return sleepStageDeep
	case "HKCategoryValueSleepAnalysisAsleepREM":
		return sleepStageREM
	}
	return sleepStageUnknown
}

// sleepSample is one HKCategoryTypeIdentifierSleepAnalysis row, its value
// already decoded through category_labels.
type sleepSample struct {
	Start, End time.Time
	Stage      sleepStage
	// sources.source_id; 0 when the sample carried none.
	SourceID int
}

// SleepDaily returns one row per night whose wake-up day (in the store's
// zone) falls on a local calendar day overlapping [start, end), ordered by
// date then start.
func (st *Store) SleepDaily(ctx context.Context, userID string, start, end time.Time) ([]SleepNight, error) {
	loc, err := st.UserLocation(ctx, userID)
	if err != nil {
		return nil, err
	}
	firstDay, afterLastDay, err := localDayBounds(start, end, loc)
	if err != nil {
		return nil, err
	}
	if days := calendarDays(firstDay, afterLastDay); days > maxSleepDays {
		return nil, badRequestf("range covers %d days; at most %d days per request", days, maxSleepDays)
	}

	rows, err := st.readDB(ctx).Query(ctx, `
		SELECT c.start_ts, c.end_ts, cl.enum_name, COALESCE(c.source_id, 0)::int
		FROM category_samples c
		JOIN sample_types st ON st.type_id = c.type_id
		JOIN category_labels cl ON cl.type_identifier = st.identifier AND cl.value = c.value
		WHERE c.user_id = $1
		  AND st.identifier = $2
		  AND c.end_ts > $3
		  AND c.start_ts < $4
		ORDER BY c.start_ts, c.end_ts`,
		userID, sleepTypeIdentifier,
		firstDay.Add(-sleepQueryPadding), afterLastDay.Add(sleepQueryPadding))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var samples []sleepSample
	for rows.Next() {
		var (
			s        sleepSample
			enumName string
		)
		if err := rows.Scan(&s.Start, &s.End, &enumName, &s.SourceID); err != nil {
			return nil, err
		}
		s.Stage = sleepStageFromEnum(enumName)
		samples = append(samples, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]SleepNight, 0)
	for _, night := range segmentNights(samples, loc) {
		wake := time.UnixMilli(night.End).In(loc)
		if wake.Before(firstDay) || !wake.Before(afterLastDay) {
			continue
		}
		out = append(out, night)
	}
	return out, nil
}

// segmentNights splits sleep samples of every source into nights — a new
// night starts when a sample begins more than sleepNightGap after the
// latest end seen so far — and summarises each. Nights come back ordered by
// start.
func segmentNights(samples []sleepSample, loc *time.Location) []SleepNight {
	if loc == nil {
		loc = time.UTC
	}
	sorted := make([]sleepSample, len(samples))
	copy(sorted, samples)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Start.Equal(sorted[j].Start) {
			return sorted[i].Start.Before(sorted[j].Start)
		}
		return sorted[i].End.Before(sorted[j].End)
	})

	var (
		groups [][]sleepSample
		end    time.Time
	)
	for _, s := range sorted {
		if len(groups) == 0 || s.Start.Sub(end) > sleepNightGap {
			groups = append(groups, nil)
			end = s.End
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], s)
		if s.End.After(end) {
			end = s.End
		}
	}

	nights := make([]SleepNight, 0, len(groups))
	for _, g := range groups {
		nights = append(nights, summarizeNight(g, loc))
	}
	return nights
}

// sourceTotals is one source's minutes per stage within a night.
type sourceTotals struct {
	sourceID int
	inBed    float64
	stages   SleepStages
}

func (t sourceTotals) asleep() float64 {
	return t.stages.Core + t.stages.Deep + t.stages.REM + t.stages.Unspecified
}

// staged is the minutes with a real stage: the tie-breaker that prefers a
// Watch's stage samples over a phone app's stage-less "asleep".
func (t sourceTotals) staged() float64 {
	return t.stages.Core + t.stages.Deep + t.stages.REM
}

// summarizeNight reduces one night's samples (non-empty, any order) to a
// row. In-bed time is the highest single-source total; asleep time and the
// stage breakdown come together from the source with the most asleep time
// (ties: the more stage detail, then the lower source id), so a night is
// never the sum of two devices that recorded the same hours.
func summarizeNight(samples []sleepSample, loc *time.Location) SleepNight {
	var (
		start, end time.Time
		bySource   = map[int]*sourceTotals{}
		order      []int
	)
	for i, s := range samples {
		if i == 0 || s.Start.Before(start) {
			start = s.Start
		}
		if s.End.After(end) {
			end = s.End
		}
		t, ok := bySource[s.SourceID]
		if !ok {
			t = &sourceTotals{sourceID: s.SourceID}
			bySource[s.SourceID] = t
			order = append(order, s.SourceID)
		}
		minutes := s.End.Sub(s.Start).Minutes()
		if minutes < 0 {
			minutes = 0
		}
		switch s.Stage {
		case sleepStageInBed:
			t.inBed += minutes
		case sleepStageAsleepUnspecified:
			t.stages.Unspecified += minutes
		case sleepStageAwake:
			t.stages.Awake += minutes
		case sleepStageCore:
			t.stages.Core += minutes
		case sleepStageDeep:
			t.stages.Deep += minutes
		case sleepStageREM:
			t.stages.REM += minutes
		}
	}

	night := SleepNight{
		Date:    end.In(loc).Format("2006-01-02"),
		Start:   start.UTC().UnixMilli(),
		End:     end.UTC().UnixMilli(),
		Sources: len(bySource),
	}
	var primary *sourceTotals
	for _, id := range order {
		t := bySource[id]
		if t.inBed > night.InBedMinutes {
			night.InBedMinutes = t.inBed
		}
		if primary == nil || better(t, primary) {
			primary = t
		}
	}
	if primary != nil {
		night.AsleepMinutes = primary.asleep()
		night.Stages = primary.stages
	}
	return night
}

// better says whether a should win a night over b.
func better(a, b *sourceTotals) bool {
	if a.asleep() != b.asleep() {
		return a.asleep() > b.asleep()
	}
	if a.staged() != b.staged() {
		return a.staged() > b.staged()
	}
	return a.sourceID < b.sourceID
}
