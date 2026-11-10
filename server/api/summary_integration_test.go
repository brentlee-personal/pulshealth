package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GET /v1/summary against a real database: two users with a day of steps, a
// workout and an upload each, and each user's summary carrying only their
// own numbers. Gated like the other write-fixture integration tests
// (DATABASE_URL plus PULS_API_WRITE_INTEGRATION_TESTS=1); fixtures go
// through ADMIN_DATABASE_URL.

type summaryFixture struct {
	userID      string
	name        string
	workoutUUID string
	batchUUID   string
	steps       float64
}

func TestIntegrationSummaryIsPerUser(t *testing.T) {
	store, admin, ctx, cleanup := writeIntegrationStore(t)
	defer cleanup()

	loc, err := losAngelesLocation()
	if err != nil {
		t.Fatalf("losAngelesLocation: %v", err)
	}
	suffix := time.Now().UTC().UnixNano()

	// The summary reads the real step-count identifier, so the fixture has to
	// use it too. The type and its daily-sum series may already exist on a
	// populated database; only what this run created is removed afterwards.
	unit := "count"
	typeID, dropType := ensureSampleType(t, ctx, admin, summaryStepsType, "quantity", &unit)
	defer dropType()
	seriesID, dropSeries := ensureDailySumSeries(t, ctx, admin, typeID)
	defer dropSeries()

	// "Today" for the store is a far-future fixture day; the rows sit on it.
	day := fixtureDay(2085, loc, suffix)
	nextDay := day.AddDate(0, 0, 1)
	store.now = func() time.Time { return day.Add(10 * time.Hour) }
	workoutAt := day.Add(9 * time.Hour)
	syncedAt := day.Add(9*time.Hour + 45*time.Minute)

	users := []summaryFixture{
		{userID: fixtureUUID("aaaa0011", suffix, 1), name: "Summary A", steps: 8412},
		{userID: fixtureUUID("aaaa0012", suffix, 2), name: "Summary B", steps: 2300},
	}
	for i := range users {
		u := &users[i]
		u.workoutUUID = fixtureUUID("aaaa4444", suffix, i)
		u.batchUUID = fixtureUUID("aaaa5555", suffix, i)
		if _, err := admin.Exec(ctx, `INSERT INTO users (id, name) VALUES ($1, $2)`, u.userID, u.name); err != nil {
			t.Fatalf("insert user %d: %v", i, err)
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO aggregate_samples (series_id, bucket_start, bucket_end, value, user_id)
			VALUES ($1, $2, $3, $4, $5)`, seriesID, day, nextDay, u.steps, u.userID); err != nil {
			t.Fatalf("insert aggregate %d: %v", i, err)
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO workouts (uuid, activity_type, start_ts, end_ts, duration_s, distance_m, user_id)
			VALUES ($1, 'running', $2, $3, 1800, 5000, $4)`,
			u.workoutUUID, workoutAt, workoutAt.Add(30*time.Minute), u.userID); err != nil {
			t.Fatalf("insert workout %d: %v", i, err)
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO batches (batch_id, device_id, user_id, type_identifier, reason, sample_count, received_at)
			VALUES ($1, 'summary-test', $2, $3, 'manual', 10, $4)`,
			u.batchUUID, u.userID, summaryStepsType, syncedAt); err != nil {
			t.Fatalf("insert batch %d: %v", i, err)
		}
	}
	defer func() {
		bg := context.Background()
		for _, u := range users {
			_, _ = admin.Exec(bg, `DELETE FROM batches WHERE user_id = $1`, u.userID)
			_, _ = admin.Exec(bg, `DELETE FROM workouts WHERE user_id = $1 AND uuid = $2`, u.userID, u.workoutUUID)
			_, _ = admin.Exec(bg, `DELETE FROM aggregate_samples WHERE user_id = $1`, u.userID)
			_, _ = admin.Exec(bg, `DELETE FROM users WHERE id = $1`, u.userID)
		}
	}()

	for _, u := range users {
		t.Run(u.name, func(t *testing.T) {
			data, err := store.Summary(ctx, u.userID, 7)
			if err != nil {
				t.Fatalf("Summary: %v", err)
			}
			if deref(data.Name) != u.name || data.Range != "7d" || data.Days != 7 || data.EndDate != day.Format("2006-01-02") {
				t.Fatalf("header = name %q range %s days %d end %s", deref(data.Name), data.Range, data.Days, data.EndDate)
			}
			if data.StartDate != day.AddDate(0, 0, -6).Format("2006-01-02") || data.TimeZone != loc.String() {
				t.Fatalf("range = %s..%s in %s", data.StartDate, data.EndDate, data.TimeZone)
			}
			if data.Activity == nil || data.Activity.Steps == nil {
				t.Fatalf("no steps in %+v", data)
			}
			steps := data.Activity.Steps
			if steps.Days != 1 || steps.Mean != u.steps || steps.Total == nil || *steps.Total != u.steps || steps.Source != summarySourceDaily {
				t.Fatalf("steps = %+v, want one day of %v — another user's rows leaked in", steps, u.steps)
			}
			if data.Activity.ActiveEnergy != nil || data.Activity.Exercise != nil || data.Activity.Stand != nil {
				t.Fatalf("activity carries sections nothing was seeded for: %+v", data.Activity)
			}
			if data.Heart != nil || data.Sleep != nil || data.Body != nil {
				t.Fatalf("sections without data are present: heart %v sleep %v body %v", data.Heart, data.Sleep, data.Body)
			}
			w := data.Workouts
			if w == nil || w.Count != 1 || w.TotalMinutes != 30 || w.TotalDistanceM == nil || *w.TotalDistanceM != 5000 ||
				len(w.ByActivityType) != 1 || w.ByActivityType[0] != (SummaryWorkoutType{"running", 1}) {
				t.Fatalf("workouts = %+v", w)
			}
			if data.Coverage.DaysWithData != 1 || data.Coverage.LastSync != nil {
				t.Fatalf("coverage = %+v, want 1 day without upload history", data.Coverage)
			}
		})
	}

	t.Run("unknown user", func(t *testing.T) {
		ghost := fixtureUUID("aaaa9998", suffix, 9)
		data, err := store.Summary(ctx, ghost, 7)
		if err != nil {
			t.Fatalf("Summary(ghost): %v", err)
		}
		if data.Name != nil || data.Activity != nil || data.Workouts != nil || data.Coverage.DaysWithData != 0 || data.Coverage.LastSync != nil {
			t.Fatalf("a user nobody has heard of should read as empty, got %+v", data)
		}
	})

	// Over HTTP, ?user= picks whose page is rendered.
	t.Run("endpoint", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))
		srv := &Server{store: store, authorize: func(ctx context.Context, bearer string) (*apiAuthority, error) {
			return testAuthorize(bearer, bearer)(ctx, bearer)
		}, log: logger, loc: loc}
		server := httptest.NewServer(srv.routes())
		defer server.Close()

		for _, u := range users {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/summary?range=14d&user="+u.userID, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+u.userID)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" {
				t.Fatalf("status = %d, type %q, body %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
			}
			page := string(body)
			for _, want := range []string{
				fmt.Sprintf("# Health summary for %s — last 14 days\n", u.name),
				fmt.Sprintf("- Steps: %s per day on average (%s in total; 1 day with data)\n", fmtNumber(u.steps, 0), fmtNumber(u.steps, 0)),
				"- 1 workout, 30 min in total, 5.0 km\n",
				"- Most frequent: running (1)\n",
				"1 of 14 days have data",
			} {
				if !strings.Contains(page, want) {
					t.Errorf("%s's page lacks %q:\n%s", u.name, want, page)
				}
			}
		}
	})
}

// ensureDailySumSeries returns the series_id of the canonical daily sum
// series (1 day, every device) for typeID — the tier metric_daily prefers —
// creating it when absent. The returned func removes it only if this call
// created it.
func ensureDailySumSeries(t *testing.T, ctx context.Context, admin *pgxpool.Pool, typeID int16) (int16, func()) {
	t.Helper()
	var seriesID int16
	err := admin.QueryRow(ctx, `
		SELECT series_id FROM aggregate_series
		WHERE type_id = $1 AND agg_func = 'sum' AND interval_value = 1 AND interval_unit = 'day' AND device_filter = 'all'`,
		typeID).Scan(&seriesID)
	if err == nil {
		return seriesID, func() {}
	}
	if err := admin.QueryRow(ctx, `
		INSERT INTO aggregate_series (type_id, agg_func, interval_value, interval_unit, device_filter, unit)
		VALUES ($1, 'sum', 1, 'day', 'all', 'count')
		RETURNING series_id`, typeID).Scan(&seriesID); err != nil {
		t.Fatalf("insert aggregate_series: %v", err)
	}
	return seriesID, func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM aggregate_series WHERE series_id = $1`, seriesID)
	}
}
