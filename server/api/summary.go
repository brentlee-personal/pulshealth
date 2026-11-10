package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// GET /v1/summary: the last N calendar days as one short markdown page, for
// pasting into a chat that has no MCP connection (docs/ai.md). Everything in
// it comes from the daily and rollup surfaces the other endpoints already
// serve — metric_daily, activity_summaries, the per-night sleep rows, the
// workout summaries, the newest body reading — so it costs a handful of
// small queries and never scans a raw hypertable. The store gathers the
// numbers into SummaryData (the ?format=json form); renderSummaryMarkdown
// turns that into the page, and is pure so the wording is tested without a
// database.

// summaryRanges maps the accepted ?range= values to a day count. The set is
// closed on purpose: the page is meant to stay short, and the reader should
// not have to guess whether "365d" is cheap.
var summaryRanges = map[string]int{"7d": 7, "14d": 14, "30d": 30, "90d": 90}

const (
	defaultSummaryRange = "7d"
	// How many activity types the workouts section names.
	summaryTopActivityTypes = 3
)

// The HealthKit identifiers the summary reads. Steps, energy and exercise
// minutes are cumulative (daily sums); the rest are discrete (daily means).
const (
	summaryStepsType        = "HKQuantityTypeIdentifierStepCount"
	summaryActiveEnergyType = "HKQuantityTypeIdentifierActiveEnergyBurned"
	summaryExerciseType     = "HKQuantityTypeIdentifierAppleExerciseTime"
	summaryRestingHRType    = "HKQuantityTypeIdentifierRestingHeartRate"
	summaryHRVType          = "HKQuantityTypeIdentifierHeartRateVariabilitySDNN"
	summaryBodyMassType     = "HKQuantityTypeIdentifierBodyMass"
	summaryBodyFatType      = "HKQuantityTypeIdentifierBodyFatPercentage"

	// Where a figure came from, named in the JSON so the reader can tell a
	// metric_daily value from an Activity-rings one.
	summarySourceDaily = "metric_daily"
	summarySourceRings = "activity_summaries"
)

// SummaryData is GET /v1/summary?format=json, and the input of the markdown
// renderer. Sections are pointers so a section with no data is absent from
// the JSON rather than a block of zeros.
type SummaryData struct {
	UserID string  `json:"userID"`
	Name   *string `json:"name"`
	// The ?range= value answered, e.g. "7d", and the calendar days it covers
	// in TimeZone: StartDate through EndDate (today) inclusive.
	Range     string `json:"range"`
	Days      int    `json:"days"`
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
	// Epoch milliseconds of the moment the summary was computed, and the
	// zone (PULS_TIME_ZONE) whose calendar cut the days.
	GeneratedAt int64  `json:"generatedAt"`
	TimeZone    string `json:"timeZone"`

	Activity *SummaryActivity `json:"activity,omitempty"`
	Heart    *SummaryHeart    `json:"heart,omitempty"`
	Sleep    *SummarySleep    `json:"sleep,omitempty"`
	Workouts *SummaryWorkouts `json:"workouts,omitempty"`
	Body     *SummaryBody     `json:"body,omitempty"`
	Coverage SummaryCoverage  `json:"coverage"`
}

// SummaryStat describes one daily series over the range: how many days had
// a value, and the mean, minimum and maximum of those days. Total is set
// for cumulative series only (steps, energy, exercise minutes), where the
// sum over the range means something.
type SummaryStat struct {
	Unit   string   `json:"unit"`
	Days   int      `json:"days"`
	Mean   float64  `json:"mean"`
	Min    float64  `json:"min"`
	Max    float64  `json:"max"`
	Total  *float64 `json:"total,omitempty"`
	Source string   `json:"source"`
}

type SummaryActivity struct {
	Steps        *SummaryStat `json:"steps,omitempty"`
	ActiveEnergy *SummaryStat `json:"activeEnergy,omitempty"`
	// Exercise minutes: the Activity ring when the phone synced rings,
	// otherwise the AppleExerciseTime daily metric (Source says which).
	Exercise *SummaryStat `json:"exercise,omitempty"`
	// Stand hours, from the Activity ring only.
	Stand *SummaryStat `json:"stand,omitempty"`
}

type SummaryHeart struct {
	RestingHeartRate *SummaryStat `json:"restingHeartRate,omitempty"`
	HRVSDNN          *SummaryStat `json:"hrvSDNN,omitempty"`
}

// SummarySleep is the main sleep session of each wake-up day in the range:
// where a day has several sessions (a night and a nap) the longest one
// counts, so naps neither inflate nor dilute the per-night mean.
type SummarySleep struct {
	Nights            int     `json:"nights"`
	MeanAsleepMinutes float64 `json:"meanAsleepMinutes"`
	MinAsleepMinutes  float64 `json:"minAsleepMinutes"`
	MaxAsleepMinutes  float64 `json:"maxAsleepMinutes"`
}

type SummaryWorkouts struct {
	Count        int     `json:"count"`
	TotalMinutes float64 `json:"totalMinutes"`
	// Sum of the workouts that recorded a distance; absent when none did.
	TotalDistanceM *float64 `json:"totalDistanceM,omitempty"`
	// The most frequent activity types, most frequent first, at most three.
	ByActivityType []SummaryWorkoutType `json:"byActivityType"`
}

type SummaryWorkoutType struct {
	ActivityType string `json:"activityType"`
	Count        int    `json:"count"`
}

// SummaryReading is the newest raw sample of a body metric, whenever it was
// taken — a weight from before the range is still the current weight.
type SummaryReading struct {
	Value     float64 `json:"value"`
	Unit      string  `json:"unit"`
	Timestamp int64   `json:"timestamp"`
}

type SummaryBody struct {
	Weight  *SummaryReading `json:"weight,omitempty"`
	BodyFat *SummaryReading `json:"bodyFat,omitempty"`
}

// SummaryCoverage says how much of the range the data covers: when the
// phone last uploaded anything (null if never), and on how many of the
// range's days at least one section has a value.
type SummaryCoverage struct {
	LastSync     *int64 `json:"lastSync"`
	DaysWithData int    `json:"daysWithData"`
}

// summaryRange resolves the ?range= parameter (default 7d) to its label
// and day count.
func summaryRange(raw string) (string, int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultSummaryRange
	}
	days, ok := summaryRanges[raw]
	if !ok {
		return "", 0, fmt.Errorf("invalid range: must be one of %s", strings.Join(summaryRangeNames(), ", "))
	}
	return raw, days, nil
}

// summaryRangeNames lists the accepted ranges, shortest first.
func summaryRangeNames() []string {
	names := make([]string, 0, len(summaryRanges))
	for name := range summaryRanges {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return summaryRanges[names[i]] < summaryRanges[names[j]] })
	return names
}

// Summary gathers the last days calendar days (in the store's zone, ending
// today) for one user through the store's existing daily reads.
func (st *Store) Summary(ctx context.Context, userID string, days int) (*SummaryData, error) {
	if days < 1 {
		return nil, badRequestf("range must cover at least one day")
	}
	loc, err := st.UserLocation(ctx, userID)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, userLocationKey{}, userLocation{userID, loc})
	now := st.now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start := today.AddDate(0, 0, -(days - 1))
	end := today.AddDate(0, 0, 1)

	data := &SummaryData{
		UserID:      userID,
		Range:       fmt.Sprintf("%dd", days),
		Days:        days,
		StartDate:   start.Format("2006-01-02"),
		EndDate:     today.Format("2006-01-02"),
		GeneratedAt: now.UnixMilli(),
		TimeZone:    loc.String(),
	}
	// Every day at least one section has a value on, for the coverage line.
	covered := map[string]struct{}{}
	cover := func(date string) { covered[date] = struct{}{} }

	profile, err := st.Profile(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	if profile != nil {
		data.Name = profile.Name
	}

	// At most 90 days of five types: no page boundary to follow (Limit
	// zero is the whole range).
	daily, err := st.DailyMetrics(ctx, userID, DailyFilters{
		Types: []string{
			summaryStepsType, summaryActiveEnergyType, summaryExerciseType,
			summaryRestingHRType, summaryHRVType,
		},
		Start: start, End: end,
	})
	if err != nil {
		return nil, fmt.Errorf("daily metrics: %w", err)
	}
	dailyValues := map[string][]float64{}
	dailyUnits := map[string]string{}
	for _, metric := range daily {
		if metric.Unit != nil {
			dailyUnits[metric.Identifier] = *metric.Unit
		}
		for _, day := range metric.Days {
			if day.Value == nil {
				continue
			}
			dailyValues[metric.Identifier] = append(dailyValues[metric.Identifier], *day.Value)
			cover(day.Date)
		}
	}
	dailyStat := func(identifier string, cumulative bool) *SummaryStat {
		return newSummaryStat(dailyUnits[identifier], summarySourceDaily, dailyValues[identifier], cumulative)
	}

	rings, err := st.ActivitySummary(ctx, userID, start, end)
	if err != nil {
		return nil, fmt.Errorf("activity summary: %w", err)
	}
	var exerciseMin, standHours []float64
	for _, day := range rings {
		if day.ExerciseMin != nil {
			exerciseMin = append(exerciseMin, *day.ExerciseMin)
			cover(day.Date)
		}
		if day.StandHours != nil {
			standHours = append(standHours, *day.StandHours)
			cover(day.Date)
		}
	}

	activity := &SummaryActivity{
		Steps:        dailyStat(summaryStepsType, true),
		ActiveEnergy: dailyStat(summaryActiveEnergyType, true),
		Exercise:     newSummaryStat("min", summarySourceRings, exerciseMin, true),
		Stand:        newSummaryStat("count", summarySourceRings, standHours, false),
	}
	if activity.Exercise == nil {
		activity.Exercise = dailyStat(summaryExerciseType, true)
	}
	if activity.Steps != nil || activity.ActiveEnergy != nil || activity.Exercise != nil || activity.Stand != nil {
		data.Activity = activity
	}

	heart := &SummaryHeart{
		RestingHeartRate: dailyStat(summaryRestingHRType, false),
		HRVSDNN:          dailyStat(summaryHRVType, false),
	}
	if heart.RestingHeartRate != nil || heart.HRVSDNN != nil {
		data.Heart = heart
	}

	nights, err := st.SleepDaily(ctx, userID, start, end)
	if err != nil {
		return nil, fmt.Errorf("sleep: %w", err)
	}
	if sleep := summarizeSleep(nights); sleep != nil {
		data.Sleep = sleep
		for _, night := range nights {
			if night.AsleepMinutes > 0 {
				cover(night.Date)
			}
		}
	}

	workouts := &SummaryWorkouts{ByActivityType: []SummaryWorkoutType{}}
	byType := map[string]int{}
	var distance float64
	var hasDistance bool
	err = st.StreamWorkouts(ctx, userID, WorkoutFilters{Start: &start, End: &end}, func(w WorkoutSummary) error {
		workouts.Count++
		byType[w.ActivityType]++
		if w.DurationS != nil {
			workouts.TotalMinutes += *w.DurationS / 60
		}
		if w.DistanceM != nil {
			distance += *w.DistanceM
			hasDistance = true
		}
		cover(time.UnixMilli(w.Start).In(loc).Format("2006-01-02"))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workouts: %w", err)
	}
	if workouts.Count > 0 {
		if hasDistance {
			workouts.TotalDistanceM = &distance
		}
		workouts.ByActivityType = topActivityTypes(byType, summaryTopActivityTypes)
		data.Workouts = workouts
	}

	latest, err := st.LatestMetrics(ctx, userID, []string{summaryBodyMassType, summaryBodyFatType})
	if err != nil {
		return nil, fmt.Errorf("latest metrics: %w", err)
	}
	body := &SummaryBody{}
	for _, metric := range latest {
		if metric.Value == nil {
			continue
		}
		reading := &SummaryReading{Value: *metric.Value, Timestamp: metric.Timestamp}
		if metric.Unit != nil {
			reading.Unit = *metric.Unit
		}
		switch metric.Identifier {
		case summaryBodyMassType:
			body.Weight = reading
		case summaryBodyFatType:
			body.BodyFat = reading
		}
	}
	if body.Weight != nil || body.BodyFat != nil {
		data.Body = body
	}

	// Upload history is outside the credential's record permissions.
	data.Coverage.DaysWithData = len(covered)
	return data, nil
}

// newSummaryStat reduces one series' daily values; nil when there are none.
func newSummaryStat(unit, source string, values []float64, cumulative bool) *SummaryStat {
	if len(values) == 0 {
		return nil
	}
	stat := &SummaryStat{Unit: unit, Source: source, Days: len(values), Min: values[0], Max: values[0]}
	var sum float64
	for _, v := range values {
		sum += v
		stat.Min = math.Min(stat.Min, v)
		stat.Max = math.Max(stat.Max, v)
	}
	stat.Mean = sum / float64(len(values))
	if cumulative {
		stat.Total = &sum
	}
	return stat
}

// summarizeSleep keeps the longest session of each wake-up day and averages
// those; nil when no session recorded any sleep.
func summarizeSleep(nights []SleepNight) *SummarySleep {
	longest := map[string]float64{}
	for _, night := range nights {
		if night.AsleepMinutes <= 0 {
			continue
		}
		if night.AsleepMinutes > longest[night.Date] {
			longest[night.Date] = night.AsleepMinutes
		}
	}
	if len(longest) == 0 {
		return nil
	}
	values := make([]float64, 0, len(longest))
	for _, minutes := range longest {
		values = append(values, minutes)
	}
	stat := newSummaryStat("min", "", values, false)
	return &SummarySleep{
		Nights:            stat.Days,
		MeanAsleepMinutes: stat.Mean,
		MinAsleepMinutes:  stat.Min,
		MaxAsleepMinutes:  stat.Max,
	}
}

// topActivityTypes orders the counts most frequent first (ties by name)
// and keeps the first n.
func topActivityTypes(counts map[string]int, n int) []SummaryWorkoutType {
	out := make([]SummaryWorkoutType, 0, len(counts))
	for activityType, count := range counts {
		out = append(out, SummaryWorkoutType{ActivityType: activityType, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].ActivityType < out[j].ActivityType
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// renderSummaryMarkdown writes the page. Instants in data are epoch
// milliseconds (the wire convention); loc is the zone to show them in — the
// same zone that cut the days, so the header can name it once.
func renderSummaryMarkdown(data SummaryData, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	var b strings.Builder

	title := fmt.Sprintf("# Health summary — last %d days", data.Days)
	if data.Name != nil && strings.TrimSpace(*data.Name) != "" {
		title = fmt.Sprintf("# Health summary for %s — last %d days", strings.TrimSpace(*data.Name), data.Days)
	}
	b.WriteString(title + "\n\n")
	fmt.Fprintf(&b, "%s to %s, %d calendar days in %s (the account's reporting time zone). Generated %s.\n",
		data.StartDate, data.EndDate, data.Days, data.TimeZone,
		time.UnixMilli(data.GeneratedAt).In(loc).Format("2006-01-02 15:04"))

	if a := data.Activity; a != nil {
		b.WriteString("\n## Activity\n")
		if s := a.Steps; s != nil {
			fmt.Fprintf(&b, "- Steps: %s per day on average (%s in total; %s)\n",
				fmtNumber(s.Mean, 0), fmtNumber(deref64(s.Total), 0), summaryDays(s.Days))
		}
		if s := a.ActiveEnergy; s != nil {
			fmt.Fprintf(&b, "- Active energy: %s kcal per day on average (%s kcal in total; %s)\n",
				fmtNumber(s.Mean, 0), fmtNumber(deref64(s.Total), 0), summaryDays(s.Days))
		}
		if s := a.Exercise; s != nil {
			fmt.Fprintf(&b, "- Exercise: %s min per day on average (%s min in total; %s%s)\n",
				fmtNumber(s.Mean, 0), fmtNumber(deref64(s.Total), 0), summaryDays(s.Days), summarySourceNote(s.Source))
		}
		if s := a.Stand; s != nil {
			fmt.Fprintf(&b, "- Stand: %s hours per day on average (fewest %s, most %s; %s%s)\n",
				fmtNumber(s.Mean, 1), fmtNumber(s.Min, 0), fmtNumber(s.Max, 0), summaryDays(s.Days), summarySourceNote(s.Source))
		}
	}

	if h := data.Heart; h != nil {
		b.WriteString("\n## Heart\n")
		if s := h.RestingHeartRate; s != nil {
			fmt.Fprintf(&b, "- Resting heart rate: %s bpm on average (lowest day %s, highest day %s; %s)\n",
				fmtNumber(s.Mean, 0), fmtNumber(s.Min, 0), fmtNumber(s.Max, 0), summaryDays(s.Days))
		}
		if s := h.HRVSDNN; s != nil {
			fmt.Fprintf(&b, "- Heart rate variability (SDNN): %s ms on average (%s)\n",
				fmtNumber(s.Mean, 0), summaryDays(s.Days))
		}
	}

	if s := data.Sleep; s != nil {
		b.WriteString("\n## Sleep\n")
		fmt.Fprintf(&b, "- Asleep: %s per night on average (shortest %s, longest %s; %d %s with data)\n",
			fmtMinutes(s.MeanAsleepMinutes), fmtMinutes(s.MinAsleepMinutes), fmtMinutes(s.MaxAsleepMinutes),
			s.Nights, plural(s.Nights, "night", "nights"))
	}

	if w := data.Workouts; w != nil {
		b.WriteString("\n## Workouts\n")
		fmt.Fprintf(&b, "- %d %s, %s in total", w.Count, plural(w.Count, "workout", "workouts"), fmtMinutes(w.TotalMinutes))
		if w.TotalDistanceM != nil {
			fmt.Fprintf(&b, ", %s km", fmtNumber(*w.TotalDistanceM/1000, 1))
		}
		b.WriteString("\n")
		if len(w.ByActivityType) > 0 {
			parts := make([]string, 0, len(w.ByActivityType))
			for _, t := range w.ByActivityType {
				parts = append(parts, fmt.Sprintf("%s (%d)", t.ActivityType, t.Count))
			}
			fmt.Fprintf(&b, "- Most frequent: %s\n", strings.Join(parts, ", "))
		}
	}

	if body := data.Body; body != nil {
		b.WriteString("\n## Body\n")
		if r := body.Weight; r != nil {
			fmt.Fprintf(&b, "- Weight: %s %s (latest reading, %s)\n",
				fmtNumber(r.Value, 1), r.Unit, time.UnixMilli(r.Timestamp).In(loc).Format("2006-01-02"))
		}
		if r := body.BodyFat; r != nil {
			fmt.Fprintf(&b, "- Body fat: %s %% (latest reading, %s)\n",
				fmtNumber(r.Value*100, 1), time.UnixMilli(r.Timestamp).In(loc).Format("2006-01-02"))
		}
	}

	b.WriteString("\n## Coverage\n")
	lastSync := "never"
	if data.Coverage.LastSync != nil {
		lastSync = time.UnixMilli(*data.Coverage.LastSync).In(loc).Format("2006-01-02 15:04")
	}
	if data.Coverage.DaysWithData == 0 {
		fmt.Fprintf(&b, "- No data in this range. Last sync: %s.\n", lastSync)
	} else {
		fmt.Fprintf(&b, "- Last sync: %s. %d of %d days have data.\n", lastSync, data.Coverage.DaysWithData, data.Days)
		b.WriteString("- Daily figures are HealthKit's deduplicated daily values (iPhone and Watch overlap already removed), " +
			"never sums of raw samples; each workout is counted once. Days without data are left out of the averages, not counted as zero.\n")
	}
	return b.String()
}

// summaryDays renders "7 days with data".
func summaryDays(n int) string {
	return fmt.Sprintf("%d %s with data", n, plural(n, "day", "days"))
}

// summarySourceNote names the Activity rings as a figure's source; the
// metric_daily default needs no note.
func summarySourceNote(source string) string {
	if source == summarySourceRings {
		return ", from the Activity rings"
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func deref64(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// fmtNumber renders v with the given decimals and thousands separators
// (8,412; 82.4; 1,234.5), rounding half away from zero.
func fmtNumber(v float64, decimals int) string {
	// math.Round first: %f alone rounds half to even (8412.5 -> 8412).
	scale := math.Pow(10, float64(decimals))
	s := fmt.Sprintf("%.*f", decimals, math.Round(v*scale)/scale)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, hasFrac := strings.Cut(s, ".")
	var out strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(c)
	}
	if hasFrac {
		out.WriteByte('.')
		out.WriteString(frac)
	}
	if neg {
		return "-" + out.String()
	}
	return out.String()
}

// fmtMinutes renders a duration in minutes as "7 h 12 min" or "42 min".
func fmtMinutes(minutes float64) string {
	total := int(math.Round(minutes))
	h, m := total/60, total%60
	switch {
	case h == 0:
		return fmt.Sprintf("%d min", m)
	case m == 0:
		return fmt.Sprintf("%d h", h)
	default:
		return fmt.Sprintf("%d h %02d min", h, m)
	}
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if !requireAPIPermission(w, r, "metrics:read", nil, nil, nil) {
		return
	}
	q := r.URL.Query()
	_, days, err := summaryRange(q.Get("range"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	format := strings.TrimSpace(q.Get("format"))
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "json" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid format: must be markdown or json"})
		return
	}

	data, err := s.store.Summary(r.Context(), s.requestUser(r), days)
	if err != nil {
		s.writeStoreError(w, err, "summary")
		return
	}
	if format == "json" {
		writeJSON(w, http.StatusOK, data)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(renderSummaryMarkdown(*data, summaryLocation(data.TimeZone, s.location()))))
}
