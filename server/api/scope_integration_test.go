package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Per-request scoping against a real database: two users' rows in the same
// tables, and every store read returning only the rows of the user it was
// asked about. Gated like the other write-fixture integration tests
// (DATABASE_URL plus PULS_API_WRITE_INTEGRATION_TESTS=1). Run with
// DATABASE_URL at api_reader and ADMIN_DATABASE_URL at the superuser, the
// users test also proves the role's SELECT on batches.

// scopeFixture is one user's rows: a users row, one quantity sample, one
// activity day, one workout and two batches.
type scopeFixture struct {
	userID      string
	name        string
	sampleUUID  string
	workoutUUID string
	sampleValue float64
	moveKcal    float64
	batches     int64
	samples     int64
}

func TestIntegrationReadsAreScopedByUser(t *testing.T) {
	store, admin, ctx, cleanup := writeIntegrationStore(t)
	defer cleanup()

	loc, err := losAngelesLocation()
	if err != nil {
		t.Fatalf("losAngelesLocation: %v", err)
	}
	suffix := time.Now().UTC().UnixNano()
	unit := "count/min"
	identifier := fmt.Sprintf("HKQuantityTypeIdentifierScopeTest%d", suffix)
	typeID, dropType := ensureSampleType(t, ctx, admin, identifier, "quantity", &unit)
	defer dropType()

	day := fixtureDay(2083, loc, suffix)
	nextDay := day.AddDate(0, 0, 1)
	at := day.Add(9 * time.Hour)

	users := []scopeFixture{
		{userID: fixtureUUID("aaaa0001", suffix, 1), name: "Scope A", sampleValue: 61, moveKcal: 310, batches: 2, samples: 150},
		{userID: fixtureUUID("aaaa0002", suffix, 2), name: "Scope B", sampleValue: 92, moveKcal: 520, batches: 1, samples: 40},
	}
	for i := range users {
		u := &users[i]
		// Identical raw IDs must not make the database or a read choose the other owner.
		u.sampleUUID = fixtureUUID("aaaa1111", suffix, 0)
		u.workoutUUID = fixtureUUID("aaaa2222", suffix, i)
		if _, err := admin.Exec(ctx, `INSERT INTO users (id, name) VALUES ($1, $2)`, u.userID, u.name); err != nil {
			t.Fatalf("insert user %d: %v", i, err)
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO quantity_samples (uuid, type_id, start_ts, end_ts, value, user_id)
			VALUES ($1, $2, $3, $3, $4, $5)`, u.sampleUUID, typeID, at, u.sampleValue, u.userID); err != nil {
			t.Fatalf("insert sample %d: %v", i, err)
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO activity_summaries (user_id, date, move_kcal)
			VALUES ($1, $2::date, $3)`, u.userID, day.Format("2006-01-02"), u.moveKcal); err != nil {
			t.Fatalf("insert activity %d: %v", i, err)
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO workouts (uuid, activity_type, start_ts, end_ts, duration_s, user_id)
			VALUES ($1, 'HKWorkoutActivityTypeRunning', $2, $3, 1800, $4)`,
			u.workoutUUID, at, at.Add(30*time.Minute), u.userID); err != nil {
			t.Fatalf("insert workout %d: %v", i, err)
		}
		for b := int64(0); b < u.batches; b++ {
			if _, err := admin.Exec(ctx, `
				INSERT INTO batches (batch_id, device_id, user_id, type_identifier, reason, sample_count, received_at)
				VALUES ($1, 'scope-test', $2, $3, 'manual', $4, $5)`,
				fixtureUUID("aaaa3300", suffix, int(b)), u.userID, identifier,
				u.samples/u.batches, at.Add(time.Duration(b)*time.Minute)); err != nil {
				t.Fatalf("insert batch %d/%d: %v", i, b, err)
			}
		}
	}
	defer func() {
		bg := context.Background()
		for _, u := range users {
			_, _ = admin.Exec(bg, `DELETE FROM batches WHERE user_id = $1`, u.userID)
			_, _ = admin.Exec(bg, `DELETE FROM workouts WHERE user_id = $1 AND uuid = $2`, u.userID, u.workoutUUID)
			_, _ = admin.Exec(bg, `DELETE FROM activity_summaries WHERE user_id = $1`, u.userID)
			_, _ = admin.Exec(bg, `DELETE FROM quantity_samples WHERE uuid = $1 AND type_id = $2 AND start_ts >= $3 AND user_id=$4`, u.sampleUUID, typeID, at, u.userID)
			_, _ = admin.Exec(bg, `DELETE FROM users WHERE id = $1`, u.userID)
		}
	}()

	for i, u := range users {
		other := users[1-i]
		t.Run(u.name, func(t *testing.T) {
			profile, err := store.Profile(ctx, u.userID)
			if err != nil || profile == nil || deref(profile.Name) != u.name {
				t.Fatalf("Profile = %+v, %v; want name %q", profile, err, u.name)
			}

			page, err := store.Samples(ctx, u.userID, SampleFilters{Type: identifier, Start: day, End: nextDay, Limit: 10})
			if err != nil {
				t.Fatalf("Samples: %v", err)
			}
			if len(page.Samples) != 1 || page.Samples[0].UUID != u.sampleUUID || *page.Samples[0].Value != u.sampleValue {
				t.Fatalf("Samples = %+v, want only %s (value %v)", page.Samples, u.sampleUUID, u.sampleValue)
			}

			latest, err := store.LatestMetrics(ctx, u.userID, []string{identifier})
			if err != nil || len(latest) != 1 || *latest[0].Value != u.sampleValue {
				t.Fatalf("LatestMetrics = %+v, %v; want value %v", latest, err, u.sampleValue)
			}

			days, err := store.ActivitySummary(ctx, u.userID, day, nextDay)
			if err != nil || len(days) != 1 || *days[0].MoveKcal != u.moveKcal {
				t.Fatalf("ActivitySummary = %+v, %v; want one day at %v kcal", days, err, u.moveKcal)
			}

			workouts, err := store.Workouts(ctx, u.userID, WorkoutFilters{Start: &day, End: &nextDay, Limit: 10})
			if err != nil || len(workouts) != 1 || workouts[0].UUID != u.workoutUUID {
				t.Fatalf("Workouts = %+v, %v; want only %s", workouts, err, u.workoutUUID)
			}
			// The other user's workout exists, but not for this user.
			if detail, err := store.Workout(ctx, u.userID, other.workoutUUID); err != nil || detail != nil {
				t.Fatalf("Workout(other's uuid) = %+v, %v; want nil, nil", detail, err)
			}
			if series, err := store.WorkoutSeries(ctx, u.userID, other.workoutUUID, nil, 10); err != nil || series != nil {
				t.Fatalf("WorkoutSeries(other's uuid) = %+v, %v; want nil, nil", series, err)
			}

			catalog, err := store.CatalogTypes(ctx, u.userID)
			if err != nil {
				t.Fatalf("CatalogTypes: %v", err)
			}
			found := false
			for _, ct := range catalog {
				if ct.Identifier == identifier {
					found = true
					if ct.RawRows != 1 {
						t.Fatalf("catalog rawRows = %d, want 1 — the count leaked another user's rows", ct.RawRows)
					}
				}
			}
			if !found {
				t.Fatalf("catalog for %s lacks %s", u.name, identifier)
			}
		})
	}

	// A user nobody has heard of reads as a user with no data, never as an
	// error and never as the default user.
	t.Run("unknown user", func(t *testing.T) {
		ghost := fixtureUUID("aaaa9999", suffix, 9)
		page, err := store.Samples(ctx, ghost, SampleFilters{Type: identifier, Start: day, End: nextDay, Limit: 10})
		if err != nil || len(page.Samples) != 0 {
			t.Fatalf("Samples(ghost) = %+v, %v; want an empty page", page, err)
		}
		if profile, err := store.Profile(ctx, ghost); err != nil || profile != nil {
			t.Fatalf("Profile(ghost) = %+v, %v; want nil, nil", profile, err)
		}
	})

	t.Run("users endpoint", func(t *testing.T) {
		testUsersEndpoint(t, ctx, store, users)
	})
}

// TestIntegrationUsersEndpoint is the store half of GET /v1/users on its own:
// Users reads batches, which api_reader gained SELECT on for it, so this is
// the test that fails as that role if the grant is missing. The full shape
// with fixtures is checked from TestIntegrationReadsAreScopedByUser.
func TestIntegrationUsersEndpoint(t *testing.T) {
	store, ctx, cleanup := integrationStore(t)
	defer cleanup()

	users, err := store.Users(ctx, defaultUserID)
	if err != nil {
		t.Fatalf("Users: %v (as api_reader this means the batches grant is missing)", err)
	}
	if users == nil {
		t.Fatalf("Users = nil, want a non-nil slice")
	}
	// The seeded default user is always there.
	for _, u := range users {
		if u.UserID == defaultUserID {
			return
		}
	}
	t.Fatalf("Users = %+v, want the seeded default user among them", users)
}

// testUsersEndpoint serves GET /v1/users over store with the gate on and
// off and checks the two fixture users' rows.
func testUsersEndpoint(t *testing.T, ctx context.Context, store *Store, fixtures []scopeFixture) {
	t.Helper()

	get := func(t *testing.T, srv *Server) UsersResponse {
		t.Helper()
		server := httptest.NewServer(srv.routes())
		defer server.Close()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/users", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer scope-integration")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, body %s", resp.StatusCode, body)
		}
		var out UsersResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))

	for _, fixture := range fixtures {
		response := get(t, &Server{store: store, authorize: testAuthorize("scope-integration", fixture.userID), log: logger})
		if response.MultiUser || response.Default != fixture.userID || len(response.Users) != 1 || response.Users[0].UserID != fixture.userID {
			t.Fatalf("response is not restricted to its credential: %+v", response)
		}
		u := response.Users[0]
		if u.Name != nil || u.Email != nil || u.CreatedAt != 0 || u.LastSync != nil || u.Batches != 0 || u.UploadedSamples != 0 {
			t.Fatal("identity metadata exposed health profile or upload history")
		}
	}
}
