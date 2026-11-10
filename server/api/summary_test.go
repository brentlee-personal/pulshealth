package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

func TestSummaryRange(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]int{"7d": 7, " 14d ": 14, "30d": 30, "90d": 90} {
		label, days, err := summaryRange(raw)
		if err != nil || days != want || label != strings.TrimSpace(raw) {
			t.Errorf("summaryRange(%q) = %q, %d, %v; want %d days", raw, label, days, err, want)
		}
	}
	if label, _, err := summaryRange(""); err != nil || label != "7d" {
		t.Errorf("the default range is %q, %v; want 7d", label, err)
	}
	for _, raw := range []string{"1d", "7", "365d", "week", "7D"} {
		if _, _, err := summaryRange(raw); err == nil || !strings.Contains(err.Error(), "7d, 14d, 30d, 90d") {
			t.Errorf("summaryRange(%q) err = %v; want one naming the accepted values", raw, err)
		}
	}
}

func TestSummaryStatAndSleepAndTopTypes(t *testing.T) {
	t.Parallel()

	if got := newSummaryStat("count", summarySourceDaily, nil, true); got != nil {
		t.Errorf("no values should give no stat, got %+v", got)
	}
	stat := newSummaryStat("count", summarySourceDaily, []float64{10, 20, 60}, true)
	if stat.Days != 3 || stat.Mean != 30 || stat.Min != 10 || stat.Max != 60 || stat.Total == nil || *stat.Total != 90 {
		t.Errorf("cumulative stat = %+v", stat)
	}
	if discrete := newSummaryStat("count/min", summarySourceDaily, []float64{55}, false); discrete.Total != nil {
		t.Errorf("a discrete stat must not carry a total: %+v", discrete)
	}

	// Two sessions on the 21st: the night and a nap. The night counts.
	nights := []SleepNight{
		{Date: "2026-09-20", AsleepMinutes: 400},
		{Date: "2026-09-21", AsleepMinutes: 30},
		{Date: "2026-09-21", AsleepMinutes: 440},
		{Date: "2026-09-22", AsleepMinutes: 0},
	}
	sleep := summarizeSleep(nights)
	if sleep == nil || sleep.Nights != 2 || sleep.MeanAsleepMinutes != 420 || sleep.MinAsleepMinutes != 400 || sleep.MaxAsleepMinutes != 440 {
		t.Errorf("sleep = %+v", sleep)
	}
	if got := summarizeSleep([]SleepNight{{Date: "2026-09-22", AsleepMinutes: 0}}); got != nil {
		t.Errorf("sessions with no sleep should give no section, got %+v", got)
	}

	top := topActivityTypes(map[string]int{"yoga": 1, "running": 3, "cycling": 2, "walking": 2}, 3)
	if len(top) != 3 || top[0].ActivityType != "running" || top[1].ActivityType != "cycling" || top[2].ActivityType != "walking" {
		t.Errorf("top = %+v", top)
	}
}

func TestSummaryFormatting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		v        float64
		decimals int
		want     string
	}{
		{8412.4, 0, "8,412"}, {8412.5, 0, "8,413"}, {999, 0, "999"}, {1234567.891, 1, "1,234,567.9"},
		{82.46, 1, "82.5"}, {0, 0, "0"}, {-1500.25, 2, "-1,500.25"},
	} {
		if got := fmtNumber(tc.v, tc.decimals); got != tc.want {
			t.Errorf("fmtNumber(%v, %d) = %q, want %q", tc.v, tc.decimals, got, tc.want)
		}
	}
	for minutes, want := range map[float64]string{432.4: "7 h 12 min", 42: "42 min", 120: "2 h", 59.6: "1 h", 0: "0 min", 65: "1 h 05 min"} {
		if got := fmtMinutes(minutes); got != want {
			t.Errorf("fmtMinutes(%v) = %q, want %q", minutes, got, want)
		}
	}
}

// emptySummary is a range with nothing in it: the header and the coverage
// line are all that renders.
func emptySummary() SummaryData {
	return SummaryData{
		UserID: defaultUserID, Range: "7d", Days: 7,
		StartDate: "2026-09-10", EndDate: "2026-09-16",
		GeneratedAt: time.Date(2026, 9, 16, 12, 3, 0, 0, time.UTC).UnixMilli(),
		TimeZone:    "Europe/Berlin",
	}
}

func TestRenderSummaryMarkdown_EmptyRendersHeaderAndCoverageOnly(t *testing.T) {
	t.Parallel()

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	got := renderSummaryMarkdown(emptySummary(), berlin)
	want := "# Health summary — last 7 days\n\n" +
		"2026-09-10 to 2026-09-16, 7 calendar days in Europe/Berlin (the account's reporting time zone). Generated 2026-09-16 14:03.\n" +
		"\n## Coverage\n" +
		"- No data in this range. Last sync: never.\n"
	if got != want {
		t.Errorf("empty summary renders:\n%s\nwant:\n%s", got, want)
	}
	for _, section := range []string{"## Activity", "## Heart", "## Sleep", "## Workouts", "## Body", "deduplicated"} {
		if strings.Contains(got, section) {
			t.Errorf("an empty summary must not mention %q", section)
		}
	}
}

func TestRenderSummaryMarkdown_EverySectionWhenDataExists(t *testing.T) {
	t.Parallel()

	name := "Test Person"
	data := emptySummary()
	data.Name = &name
	data.Activity = &SummaryActivity{
		Steps:        &SummaryStat{Unit: "count", Days: 7, Mean: 8412.4, Min: 3000, Max: 14000, Total: f64(58886.8), Source: summarySourceDaily},
		ActiveEnergy: &SummaryStat{Unit: "kcal", Days: 6, Mean: 512.2, Min: 300, Max: 800, Total: f64(3073.2), Source: summarySourceDaily},
		Exercise:     &SummaryStat{Unit: "min", Days: 7, Mean: 34, Min: 0, Max: 70, Total: f64(238), Source: summarySourceRings},
		Stand:        &SummaryStat{Unit: "count", Days: 7, Mean: 11.34, Min: 9, Max: 13, Source: summarySourceRings},
	}
	data.Heart = &SummaryHeart{
		RestingHeartRate: &SummaryStat{Unit: "count/min", Days: 7, Mean: 56.4, Min: 53, Max: 60.2, Source: summarySourceDaily},
		HRVSDNN:          &SummaryStat{Unit: "ms", Days: 5, Mean: 42.49, Min: 30, Max: 55, Source: summarySourceDaily},
	}
	data.Sleep = &SummarySleep{Nights: 6, MeanAsleepMinutes: 432.4, MinAsleepMinutes: 340, MaxAsleepMinutes: 485}
	data.Workouts = &SummaryWorkouts{
		Count: 5, TotalMinutes: 252.4, TotalDistanceM: f64(31460),
		ByActivityType: []SummaryWorkoutType{{"running", 3}, {"cycling", 1}, {"yoga", 1}},
	}
	data.Body = &SummaryBody{
		Weight:  &SummaryReading{Value: 82.46, Unit: "kg", Timestamp: time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC).UnixMilli()},
		BodyFat: &SummaryReading{Value: 0.1824, Unit: "%", Timestamp: time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC).UnixMilli()},
	}
	lastSync := time.Date(2026, 9, 16, 11, 58, 0, 0, time.UTC).UnixMilli()
	data.Coverage = SummaryCoverage{LastSync: &lastSync, DaysWithData: 7}

	got := renderSummaryMarkdown(data, time.UTC)
	for _, line := range []string{
		"# Health summary for Test Person — last 7 days\n",
		"\n## Activity\n",
		"- Steps: 8,412 per day on average (58,887 in total; 7 days with data)\n",
		"- Active energy: 512 kcal per day on average (3,073 kcal in total; 6 days with data)\n",
		"- Exercise: 34 min per day on average (238 min in total; 7 days with data, from the Activity rings)\n",
		"- Stand: 11.3 hours per day on average (fewest 9, most 13; 7 days with data, from the Activity rings)\n",
		"\n## Heart\n",
		"- Resting heart rate: 56 bpm on average (lowest day 53, highest day 60; 7 days with data)\n",
		"- Heart rate variability (SDNN): 42 ms on average (5 days with data)\n",
		"\n## Sleep\n",
		"- Asleep: 7 h 12 min per night on average (shortest 5 h 40 min, longest 8 h 05 min; 6 nights with data)\n",
		"\n## Workouts\n",
		"- 5 workouts, 4 h 12 min in total, 31.5 km\n",
		"- Most frequent: running (3), cycling (1), yoga (1)\n",
		"\n## Body\n",
		"- Weight: 82.5 kg (latest reading, 2026-09-15)\n",
		"- Body fat: 18.2 % (latest reading, 2026-09-01)\n",
		"\n## Coverage\n",
		"- Last sync: 2026-09-16 11:58. 7 of 7 days have data.\n",
		"deduplicated daily values",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("summary lacks %q:\n%s", line, got)
		}
	}
	if lines := strings.Count(got, "\n"); lines > 60 {
		t.Errorf("summary is %d lines, want under 60", lines)
	}

	// Exercise from metric_daily carries no rings note; a workout list with
	// no distance carries no kilometres.
	data.Activity.Exercise.Source = summarySourceDaily
	data.Workouts.TotalDistanceM = nil
	data.Workouts.Count, data.Workouts.TotalMinutes = 1, 30
	got = renderSummaryMarkdown(data, time.UTC)
	if !strings.Contains(got, "- Exercise: 34 min per day on average (238 min in total; 7 days with data)\n") {
		t.Errorf("metric_daily exercise line is wrong:\n%s", got)
	}
	if !strings.Contains(got, "- 1 workout, 30 min in total\n") {
		t.Errorf("single workout line is wrong:\n%s", got)
	}
}

func TestSummaryEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("markdown by default over the last 7 days", func(t *testing.T) {
		t.Parallel()
		store := &fakeStore{}
		srv := testServer(t, store)
		rec := serveAuthorized(t, srv, http.MethodGet, "/v1/summary", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
			t.Errorf("Content-Type = %q", ct)
		}
		if store.lastSummaryDays != 7 || store.lastUser != defaultUserID {
			t.Errorf("store asked for %d days of %s; want 7 days of the default user", store.lastSummaryDays, store.lastUser)
		}
		if body := rec.Body.String(); !strings.HasPrefix(body, "# Health summary — last 7 days\n") || !strings.Contains(body, "## Coverage") {
			t.Errorf("body:\n%s", body)
		}
	})

	t.Run("range selects the day count", func(t *testing.T) {
		t.Parallel()
		store := &fakeStore{}
		srv := testServer(t, store)
		if rec := serveAuthorized(t, srv, http.MethodGet, "/v1/summary?range=30d", nil); rec.Code != http.StatusOK || store.lastSummaryDays != 30 {
			t.Fatalf("status = %d, days = %d", rec.Code, store.lastSummaryDays)
		}
	})

	t.Run("a range outside the set is a 400", func(t *testing.T) {
		t.Parallel()
		store := &fakeStore{}
		srv := testServer(t, store)
		rec := serveAuthorized(t, srv, http.MethodGet, "/v1/summary?range=365d", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
		assertJSONError(t, rec.Body.Bytes(), "invalid range: must be one of 7d, 14d, 30d, 90d")
		if store.lastSummaryDays != 0 {
			t.Errorf("an invalid range reached the store")
		}
	})

	t.Run("an unknown format is a 400", func(t *testing.T) {
		t.Parallel()
		srv := testServer(t, &fakeStore{})
		rec := serveAuthorized(t, srv, http.MethodGet, "/v1/summary?format=xml", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
		assertJSONError(t, rec.Body.Bytes(), "invalid format: must be markdown or json")
	})

	t.Run("json returns the structured form", func(t *testing.T) {
		t.Parallel()
		lastSync := int64(1_700_000_000_000)
		store := &fakeStore{summary: &SummaryData{
			UserID: defaultUserID, Range: "14d", Days: 14, StartDate: "2026-09-03", EndDate: "2026-09-16", TimeZone: "UTC",
			Activity: &SummaryActivity{Steps: &SummaryStat{Unit: "count", Days: 14, Mean: 8000, Min: 1, Max: 2, Total: f64(112000), Source: summarySourceDaily}},
			Coverage: SummaryCoverage{LastSync: &lastSync, DaysWithData: 14},
		}}
		srv := testServer(t, store)
		rec := serveAuthorized(t, srv, http.MethodGet, "/v1/summary?range=14d&format=json", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var got SummaryData
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, rec.Body.String())
		}
		if got.Range != "14d" || got.Activity == nil || got.Activity.Steps == nil || *got.Activity.Steps.Total != 112000 || got.Coverage.DaysWithData != 14 {
			t.Errorf("summary = %+v", got)
		}
		// Absent sections are absent, not null blocks.
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &raw)
		for _, absent := range []string{"heart", "sleep", "workouts", "body"} {
			if _, present := raw[absent]; present {
				t.Errorf("json carries %q although there is no data for it", absent)
			}
		}
	})

	t.Run("user selects whose summary", func(t *testing.T) {
		t.Parallel()
		store := &fakeStore{}
		srv := testServer(t, store)
		other := "0badf00d-0000-4000-8000-000000000002"
		srv.authorize = testAuthorize("secret", other)
		if rec := serveAuthorized(t, srv, http.MethodGet, "/v1/summary?user="+other, nil); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if store.lastUser != other {
			t.Errorf("store asked about %s, want %s", store.lastUser, other)
		}
	})

	t.Run("a store failure is a 500", func(t *testing.T) {
		t.Parallel()
		srv := testServer(t, &fakeStore{err: errors.New("boom")})
		rec := serveAuthorized(t, srv, http.MethodGet, "/v1/summary", nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
		assertJSONError(t, rec.Body.Bytes(), "summary failed")
	})

	t.Run("needs the token", func(t *testing.T) {
		t.Parallel()
		srv := testServer(t, &fakeStore{})
		req := httptest.NewRequest(http.MethodGet, "/v1/summary", nil)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}
