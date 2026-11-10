package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestSummaryUsesReportedAccountZone(t *testing.T) {
	loc := summaryLocation("Asia/Kathmandu", time.UTC)
	if got := time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC).In(loc).Format("2006-01-02 15:04"); got != "2026-01-02 01:45" {
		t.Fatal(got)
	}
}

// A shared store must keep simultaneous users' zones separate. Include
// fractional offsets and the spring-forward calendar day.
func TestIntegrationAccountTimeZones(t *testing.T) {
	st, admin, ctx, cleanup := writeIntegrationStore(t)
	defer cleanup()
	instant := time.Date(2080, 3, 10, 6, 30, 0, 0, time.UTC)
	st.now = func() time.Time { return instant }
	suffix := time.Now().UnixNano()
	zones := []string{"America/New_York", "America/Los_Angeles", "Asia/Kathmandu"}
	ids := make([]string, len(zones))
	for i, zone := range zones {
		ids[i] = fixtureUUID("aabb0010", suffix, i)
		if _, err := admin.Exec(ctx, `INSERT INTO users(id,time_zone) VALUES($1,$2)`, ids[i], zone); err != nil {
			t.Fatal(err)
		}
		defer admin.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, ids[i])
		loc, _ := time.LoadLocation(zone)
		day := instant.In(loc).Format("2006-01-02")
		if _, err := admin.Exec(ctx, `INSERT INTO activity_summaries(user_id,date,move_kcal) VALUES($1,$2::date,$3)`, ids[i], day, float64(i+1)); err != nil {
			t.Fatal(err)
		}
		defer admin.Exec(context.Background(), `DELETE FROM activity_summaries WHERE user_id=$1`, ids[i])
	}
	var wg sync.WaitGroup
	for i, zone := range zones {
		wg.Add(1)
		go func(id, zone string) {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				loc, err := st.UserLocation(ctx, id)
				if err != nil {
					t.Error(err)
					return
				}
				if loc.String() != zone {
					t.Errorf("%s got %s want %s", id, loc, zone)
				}
				data, err := st.Summary(ctx, id, 7)
				if err != nil {
					t.Error(err)
					return
				}
				if data.TimeZone != zone || data.EndDate != instant.In(loc).Format("2006-01-02") {
					t.Errorf("wrong account summary %+v", data)
				}
				date := instant.In(loc)
				localStart := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, loc)
				rings, err := st.ActivitySummary(ctx, id, localStart, localStart.AddDate(0, 0, 1))
				if err != nil || len(rings) != 1 || rings[0].Date != localStart.Format("2006-01-02") {
					t.Errorf("account rings %+v %v", rings, err)
				}
				profile, err := st.Profile(ctx, id)
				if err != nil || profile == nil || profile.TimeZone != zone {
					t.Errorf("profile %+v %v", profile, err)
				}
				start := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
				end := start.AddDate(0, 0, 1)
				first, after, err := localDayBounds(start, end, loc)
				if err != nil || !first.Equal(start) || !after.Equal(end) {
					t.Errorf("bounds %v %v %v", first, after, err)
				}
				if zone == "America/New_York" && after.Sub(first) != 23*time.Hour {
					t.Errorf("DST day %v", after.Sub(first))
				}
			}
		}(ids[i], zone)
	}
	wg.Wait()
	for i, id := range ids {
		server := &Server{store: st}
		req := httptest.NewRequest("GET", "/v1/users?user="+id, nil)
		rec := httptest.NewRecorder()
		server.scopeUser(server.handleUsers)(rec, req.WithContext(withRequestUser(req.Context(), id)))
		var body UsersResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.TimeZone != zones[i] {
			t.Fatalf("selected user zone %s want %s", body.TimeZone, zones[i])
		}
		found := false
		for _, u := range body.Users {
			if u.UserID == id {
				found = true
				if u.TimeZone != zones[i] {
					t.Fatalf("user zone %+v", u)
				}
			}
		}
		if !found {
			t.Fatalf("missing user %s", id)
		}
	}
}
