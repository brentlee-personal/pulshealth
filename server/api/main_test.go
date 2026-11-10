package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeStore struct {
	profile  Profile
	catalog  []CatalogType
	latest   []LatestMetric
	daily    []DailyMetric
	activity []ActivityDay
	workouts []WorkoutSummary
	workout  *WorkoutDetail
	nights   []SleepNight
	samples  *SamplesPage
	series   *WorkoutSeriesResponse
	moods    []StateOfMindEntry
	users    []User
	summary  *SummaryData
	calls    struct {
		catalog int
	}
	// The user the most recent per-user call was asked about, so handler
	// tests can prove the request user reached the store.
	lastUser string
	// The day count of the most recent Summary call.
	lastSummaryDays int
	// The arguments of the most recent samples / workouts / series call, so
	// handler tests can assert on what the parsers produced.
	lastSamples  SampleFilters
	lastDaily    DailyFilters
	lastWorkouts WorkoutFilters
	lastSeries   struct {
		uuid      string
		types     []string
		maxPoints int
	}
	err error
	// Fails only SampleType, so an export test can make the type lookup a
	// 400 without failing every other call.
	sampleTypeErr error
	// Counts database liveness probes, so a test can assert /healthz does not
	// make one per request (see health.go).
	pings atomic.Int64
}

func (f *fakeStore) SleepDaily(_ context.Context, user string, _, _ time.Time) ([]SleepNight, error) {
	f.lastUser = user
	if f.err != nil {
		return nil, f.err
	}
	return f.nights, nil
}

func (f *fakeStore) Samples(_ context.Context, user string, filters SampleFilters) (*SamplesPage, error) {
	f.lastUser = user
	f.lastSamples = filters
	if f.err != nil {
		return nil, f.err
	}
	if f.samples == nil {
		return &SamplesPage{Type: filters.Type, Kind: "quantity", Samples: []Sample{}, NextOffset: filters.Offset}, nil
	}
	return f.samples, nil
}

func (f *fakeStore) SampleType(_ context.Context, identifier string) (SampleMeta, error) {
	if f.sampleTypeErr != nil {
		return SampleMeta{}, f.sampleTypeErr
	}
	if f.err != nil {
		return SampleMeta{}, f.err
	}
	meta := SampleMeta{Type: identifier, TypeID: 1, Kind: "quantity"}
	if f.samples != nil {
		meta.Kind, meta.Unit = f.samples.Kind, f.samples.Unit
	}
	return meta, nil
}

func (f *fakeStore) StreamSamples(_ context.Context, user string, meta SampleMeta, filters SampleFilters, fn func(Sample) error) error {
	f.lastUser = user
	f.lastSamples = filters
	if f.err != nil {
		return f.err
	}
	if f.samples == nil {
		return nil
	}
	for _, sample := range f.samples.Samples {
		if err := fn(sample); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeStore) StreamWorkouts(_ context.Context, user string, filters WorkoutFilters, fn func(WorkoutSummary) error) error {
	f.lastUser = user
	f.lastWorkouts = filters
	if f.err != nil {
		return f.err
	}
	for _, workout := range f.workouts {
		if err := fn(workout); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeStore) WorkoutSeries(_ context.Context, user, uuid string, types []string, maxPoints int) (*WorkoutSeriesResponse, error) {
	f.lastUser = user
	f.lastSeries.uuid, f.lastSeries.types, f.lastSeries.maxPoints = uuid, types, maxPoints
	if f.err != nil {
		return nil, f.err
	}
	return f.series, nil
}

func (f *fakeStore) StateOfMind(_ context.Context, user string, _, _ time.Time) ([]StateOfMindEntry, error) {
	f.lastUser = user
	if f.err != nil {
		return nil, f.err
	}
	return f.moods, nil
}

func (f *fakeStore) Summary(_ context.Context, user string, days int) (*SummaryData, error) {
	f.lastUser = user
	f.lastSummaryDays = days
	if f.err != nil {
		return nil, f.err
	}
	if f.summary == nil {
		return &SummaryData{UserID: user, Range: fmt.Sprintf("%dd", days), Days: days, TimeZone: "UTC"}, nil
	}
	return f.summary, nil
}

func (f *fakeStore) ReadScope(ctx context.Context, _ *apiAuthority) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (f *fakeStore) Ping(ctx context.Context) error {
	f.pings.Add(1)
	if err := ctx.Err(); err != nil {
		return err // a real pool refuses a cancelled context
	}
	return f.err
}

func (f *fakeStore) Users(_ context.Context, member string) ([]User, error) {
	f.lastUser = member
	if f.err != nil {
		return nil, f.err
	}
	return f.users, nil
}

func (f *fakeStore) Profile(_ context.Context, user string) (*Profile, error) {
	f.lastUser = user
	if f.err != nil {
		return nil, f.err
	}
	profile := f.profile
	return &profile, nil
}

func (f *fakeStore) CatalogTypes(_ context.Context, user string) ([]CatalogType, error) {
	f.lastUser = user
	if f.err != nil {
		return nil, f.err
	}
	f.calls.catalog++
	return f.catalog, nil
}

func (f *fakeStore) LatestMetrics(_ context.Context, user string, _ []string) ([]LatestMetric, error) {
	f.lastUser = user
	if f.err != nil {
		return nil, f.err
	}
	return f.latest, nil
}

func (f *fakeStore) DailyMetrics(_ context.Context, user string, filters DailyFilters) ([]DailyMetric, error) {
	f.lastUser = user
	f.lastDaily = filters
	if f.err != nil {
		return nil, f.err
	}
	return f.daily, nil
}

func (f *fakeStore) ActivitySummary(_ context.Context, user string, _, _ time.Time) ([]ActivityDay, error) {
	f.lastUser = user
	if f.err != nil {
		return nil, f.err
	}
	return f.activity, nil
}

func (f *fakeStore) Workouts(_ context.Context, user string, _ WorkoutFilters) ([]WorkoutSummary, error) {
	f.lastUser = user
	if f.err != nil {
		return nil, f.err
	}
	return f.workouts, nil
}

func (f *fakeStore) Workout(_ context.Context, user, _ string) (*WorkoutDetail, error) {
	f.lastUser = user
	if f.err != nil {
		return nil, f.err
	}
	return f.workout, nil
}

func TestHealthzWithoutAuthReturnsOK(t *testing.T) {
	t.Parallel()

	srv := &Server{
		store:     &fakeStore{},
		authorize: testAuthorize("secret", defaultUserID),
		log:       slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body["ok"] || !body["db"] {
		t.Fatalf("body = %#v, want ok=true db=true", body)
	}
}

func TestIndexWithoutAuthAdvertisesDocs(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["docs"] != "/docs" {
		t.Fatalf("docs = %#v, want /docs", body["docs"])
	}
	if body["openapi"] != "/openapi.json" {
		t.Fatalf("openapi = %#v, want /openapi.json", body["openapi"])
	}
}

func TestUnknownPathsReturnNotFound(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	for _, path := range []string{"/not-found", "/v1/profil"} {
		path := path
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()

			srv.routes().ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
			}
		})
	}
}

func TestDocsWithoutAuthReturnsHTML(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/html; charset=utf-8", got)
	}
	if !strings.Contains(rec.Body.String(), "PulsHealth Product API") {
		t.Fatalf("docs body missing title")
	}
}

func TestOpenAPIWithoutAuthAdvertisesProductPaths(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/openapi+json" {
		t.Fatalf("content-type = %q, want application/openapi+json", got)
	}

	var body struct {
		OpenAPI    string                    `json:"openapi"`
		Paths      map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, path := range []string{
		"/v1/profile", "/v1/catalog/types", "/v1/metrics/latest", "/v1/metrics/daily", "/v1/activity/summary",
		"/v1/workouts", "/v1/workouts/{uuid}", "/v1/workouts/{uuid}/series", "/v1/sleep/daily", "/v1/samples", "/v1/state-of-mind",
		"/v1/users",
	} {
		if _, ok := body.Paths[path]; !ok {
			t.Fatalf("openapi paths missing %s", path)
		}
	}
	for _, schema := range []string{"WorkoutDetail", "SleepNight", "SamplesPage", "WorkoutSeriesResponse", "StateOfMindEntry", "User"} {
		if _, ok := body.Components.Schemas[schema]; !ok {
			t.Fatalf("openapi schemas missing %s", schema)
		}
	}

	// The HTML reference lists the same endpoints.
	docs := serveAuthorized(t, srv, http.MethodGet, "/docs", nil)
	for _, path := range []string{"/v1/sleep/daily", "/v1/samples", "/v1/workouts/{uuid}/series", "/v1/state-of-mind"} {
		if !strings.Contains(docs.Body.String(), path) {
			t.Errorf("docs page does not mention %s", path)
		}
	}
}

func TestSleepDailyReturnsNightsWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{nights: []SleepNight{{
		Date: "2026-09-21", Start: 1789968600000, End: 1789997400000,
		InBedMinutes: 480, AsleepMinutes: 460,
		Stages:  SleepStages{Core: 340, Deep: 60, REM: 60, Awake: 10},
		Sources: 2,
	}}})

	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/sleep/daily?start=1789930800000&end=1790017200000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Nights []map[string]any `json:"nights"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Nights) != 1 || body.Nights[0]["date"] != "2026-09-21" || body.Nights[0]["asleepMinutes"] != 460.0 {
		t.Fatalf("nights = %#v", body.Nights)
	}
	stages, ok := body.Nights[0]["stages"].(map[string]any)
	if !ok || stages["core"] != 340.0 || stages["unspecified"] != 0.0 {
		t.Fatalf("stages = %#v (every stage key must be present, zero included)", body.Nights[0]["stages"])
	}

	empty := serveAuthorized(t, testServer(t, &fakeStore{nights: []SleepNight{}}), http.MethodGet, "/v1/sleep/daily?start=1789930800000&end=1790017200000", nil)
	assertJSONKeyIsArray(t, empty.Body.Bytes(), "nights")

	bad := serveAuthorized(t, testServer(t, &fakeStore{}), http.MethodGet, "/v1/sleep/daily?start=1790017200000&end=1789930800000", nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("reversed range status = %d, want 400", bad.Code)
	}
	assertJSONError(t, bad.Body.Bytes(), "end must be after start")
}

// storeBackedTargets is one valid request to every JSON endpoint that reads
// the store, so a test can assert they all answer a store error alike.
var storeBackedTargets = []string{
	"/v1/users",
	"/v1/profile",
	"/v1/catalog/types",
	"/v1/metrics/latest?types=HKQuantityTypeIdentifierBodyMass",
	"/v1/metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=1751328000000&end=1751414400000",
	"/v1/activity/summary?start=1751328000000&end=1751414400000",
	"/v1/workouts",
	"/v1/workouts/44444444-4444-4444-8444-444444444444",
	"/v1/workouts/44444444-4444-4444-8444-444444444444/series",
	"/v1/sleep/daily?start=1751328000000&end=1751414400000",
	"/v1/samples?type=HKQuantityTypeIdentifierHeartRate&start=1751328000000&end=1751414400000",
	"/v1/state-of-mind?start=1751328000000&end=1751414400000",
	"/v1/summary",
}

func TestStoreRequestErrorsBecome400(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{err: badRequestf("range covers 400 days; at most 366 days per request")})
	for _, target := range storeBackedTargets {
		rec := serveAuthorized(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
		assertJSONError(t, rec.Body.Bytes(), "range covers 400 days; at most 366 days per request")
	}

	// Any other store error stays a generic 500.
	broken := testServer(t, &fakeStore{err: errors.New("connection reset")})
	rec := serveAuthorized(t, broken, http.MethodGet, "/v1/sleep/daily?start=1751328000000&end=1751414400000", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	assertJSONError(t, rec.Body.Bytes(), "sleep failed")
}

// A query that outlives handlerTimeout is for the caller to narrow, not a
// server fault: a 504 that says so, on every endpoint. pgx wraps
// context.DeadlineExceeded whether the pool, the query or the scan ran out.
func TestStoreTimeoutsBecome504(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{err: fmt.Errorf("timeout: %w", context.DeadlineExceeded)})
	for _, target := range storeBackedTargets {
		rec := serveAuthorized(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusGatewayTimeout {
			t.Errorf("%s: status = %d, want 504", target, rec.Code)
		}
		assertJSONError(t, rec.Body.Bytes(), "query took too long; narrow the range")
	}
}

// Every types list names at most maxTypesPerRequest identifiers; each one is
// its own scan.
func TestTypesListsAreCapped(t *testing.T) {
	t.Parallel()

	types := func(n int) string {
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("HKQuantityTypeIdentifierT%d", i)
		}
		return strings.Join(names, ",")
	}
	srv := testServer(t, &fakeStore{})
	for _, target := range []string{
		"/v1/metrics/latest?types=%s",
		"/v1/metrics/daily?types=%s&start=1751328000000&end=1751414400000",
		"/v1/workouts/44444444-4444-4444-8444-444444444444/series?types=%s",
		"/v1/export?format=csv&dataset=daily_metrics&types=%s&start=1751328000000&end=1751414400000",
	} {
		if rec := serveAuthorized(t, srv, http.MethodGet, fmt.Sprintf(target, types(maxTypesPerRequest)), nil); rec.Code == http.StatusBadRequest {
			t.Errorf("%s: %d types refused: %s", target, maxTypesPerRequest, rec.Body.String())
		}
		rec := serveAuthorized(t, srv, http.MethodGet, fmt.Sprintf(target, types(maxTypesPerRequest+1)), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d types: status = %d, want 400", target, maxTypesPerRequest+1, rec.Code)
		}
		assertJSONError(t, rec.Body.Bytes(), "types names 51 identifiers; at most 50 per request")
	}
}

// A by-hand install from .env.example must not come up with a token anyone
// can guess.
func TestRefusePlaceholder(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"change-me", " change-me\n", "CHANGE-ME"} {
		err := refusePlaceholder("PULS_API_TOKEN", value)
		if err == nil {
			t.Fatalf("refusePlaceholder(%q) = nil, want an error", value)
		}
		for _, want := range []string{"PULS_API_TOKEN", "scripts/bootstrap.sh", "openssl rand -hex 32"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	}
	if err := refusePlaceholder("PULS_API_TOKEN", "0123456789abcdef"); err != nil {
		t.Errorf("a real token was refused: %v", err)
	}
}

func TestSamplesHandlerParsesFiltersAndReturnsPage(t *testing.T) {
	t.Parallel()

	store := &fakeStore{samples: &SamplesPage{
		Type: "HKCategoryTypeIdentifierSleepAnalysis", Kind: "category",
		Samples: []Sample{{
			UUID: "11111111-1111-4111-8111-111111111111", Start: 1751562000000, End: 1751565600000,
			Value: ptrFloat64(3), Label: ptrString("Asleep Core"), Source: ptrString("Apple Watch"),
		}},
		NextOffset: 11,
	}}
	srv := testServer(t, store)

	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/samples?type=HKCategoryTypeIdentifierSleepAnalysis&start=1751328000000&end=1751414400000&limit=9999&offset=10", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if store.lastSamples.Type != "HKCategoryTypeIdentifierSleepAnalysis" || store.lastSamples.Limit != maxSampleLimit || store.lastSamples.Offset != 10 {
		t.Errorf("filters = %+v (limit should clamp to %d)", store.lastSamples, maxSampleLimit)
	}
	if store.lastSamples.Start.UnixMilli() != 1751328000000 || store.lastSamples.End.UnixMilli() != 1751414400000 {
		t.Errorf("range = %s .. %s", store.lastSamples.Start, store.lastSamples.End)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	samples := body["samples"].([]any)
	if body["kind"] != "category" || body["nextOffset"] != 11.0 || len(samples) != 1 {
		t.Fatalf("body = %#v", body)
	}
	first := samples[0].(map[string]any)
	if first["label"] != "Asleep Core" || first["value"] != 3.0 || first["source"] != "Apple Watch" {
		t.Errorf("sample = %#v", first)
	}

	// Defaults, and a quantity page has no label keys.
	quantity := &fakeStore{samples: &SamplesPage{Type: "HKQuantityTypeIdentifierHeartRate", Kind: "quantity", Unit: ptrString("count/min"),
		Samples: []Sample{{UUID: "11111111-1111-4111-8111-111111111111", Value: ptrFloat64(68)}}}}
	rec = serveAuthorized(t, testServer(t, quantity), http.MethodGet, "/v1/samples?type=HKQuantityTypeIdentifierHeartRate&start=1751328000000&end=1751414400000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if quantity.lastSamples.Limit != defaultSampleLimit || quantity.lastSamples.Offset != 0 {
		t.Errorf("defaults = %+v", quantity.lastSamples)
	}
	if strings.Contains(rec.Body.String(), `"label"`) {
		t.Errorf("quantity sample carries a label key: %s", rec.Body.String())
	}
}

func TestSamplesHandlerRejectsBadRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		query   string
		wantErr string
	}{
		{name: "missing type", query: "start=1751328000000&end=1751414400000", wantErr: "missing type"},
		{name: "several types", query: "type=a,b&start=1751328000000&end=1751414400000", wantErr: "type must name exactly one HealthKit identifier"},
		{name: "missing range", query: "type=HKQuantityTypeIdentifierHeartRate", wantErr: "missing start"},
		{name: "over 31 days", query: "type=HKQuantityTypeIdentifierHeartRate&start=1751328000000&end=1754006400001", wantErr: "range must not exceed 31 days"},
		{name: "bad limit", query: "type=HKQuantityTypeIdentifierHeartRate&start=1751328000000&end=1751414400000&limit=0", wantErr: "limit must be at least 1"},
		{name: "bad offset", query: "type=HKQuantityTypeIdentifierHeartRate&start=1751328000000&end=1751414400000&offset=-1", wantErr: "offset must be at least 0"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := serveAuthorized(t, testServer(t, &fakeStore{}), http.MethodGet, "/v1/samples?"+tc.query, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			assertJSONError(t, rec.Body.Bytes(), tc.wantErr)
		})
	}
	// Exactly 31 days is fine.
	rec := serveAuthorized(t, testServer(t, &fakeStore{}), http.MethodGet, "/v1/samples?type=HKQuantityTypeIdentifierHeartRate&start=1751328000000&end=1754006400000", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("31-day range status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkoutSeriesHandler(t *testing.T) {
	t.Parallel()

	const uuid = "11111111-1111-4111-8111-111111111111"
	store := &fakeStore{series: &WorkoutSeriesResponse{
		UUID: uuid, Start: 1751562000000, End: 1751565600000, MaxPoints: 3,
		Series: []WorkoutSeries{{
			Type: "HKQuantityTypeIdentifierHeartRate", Unit: ptrString("count/min"), TotalPoints: 900,
			Points: []SeriesPoint{{T: 1751562000000, V: 98}, {T: 1751563800000, V: 151.5}, {T: 1751565600000, V: 120}},
		}},
	}}
	srv := testServer(t, store)

	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/workouts/"+uuid+"/series?types=HKQuantityTypeIdentifierHeartRate,HKQuantityTypeIdentifierRunningPower&maxPoints=3", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if store.lastSeries.uuid != uuid || store.lastSeries.maxPoints != 3 || len(store.lastSeries.types) != 2 {
		t.Errorf("store call = %+v", store.lastSeries)
	}
	if !strings.Contains(rec.Body.String(), `"points":[[1751562000000,98],[1751563800000,151.5],[1751565600000,120]]`) {
		t.Errorf("points are not [t, value] pairs: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"totalPoints":900`) {
		t.Errorf("totalPoints missing: %s", rec.Body.String())
	}

	// Defaults and clamping.
	if _, _, err := workoutSeriesRequest(httptest.NewRequest(http.MethodGet, "/", nil)); err != nil {
		t.Fatal(err)
	}
	rec = serveAuthorized(t, srv, http.MethodGet, "/v1/workouts/"+uuid+"/series", nil)
	if rec.Code != http.StatusOK || store.lastSeries.maxPoints != defaultSeriesPoints || store.lastSeries.types != nil {
		t.Errorf("defaults: status %d, call %+v", rec.Code, store.lastSeries)
	}
	rec = serveAuthorized(t, srv, http.MethodGet, "/v1/workouts/"+uuid+"/series?maxPoints=999999", nil)
	if rec.Code != http.StatusOK || store.lastSeries.maxPoints != maxSeriesPoints {
		t.Errorf("clamp: status %d, maxPoints %d", rec.Code, store.lastSeries.maxPoints)
	}

	for query, wantErr := range map[string]string{
		"maxPoints=0":   "maxPoints must be at least 1",
		"maxPoints=abc": "invalid maxPoints: must be an integer",
		"types=a,":      "types must not contain empty values",
	} {
		rec = serveAuthorized(t, srv, http.MethodGet, "/v1/workouts/"+uuid+"/series?"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, rec.Code)
		}
		assertJSONError(t, rec.Body.Bytes(), wantErr)
	}

	rec = serveAuthorized(t, srv, http.MethodGet, "/v1/workouts/not-a-uuid/series", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad uuid status = %d", rec.Code)
	}
	rec = serveAuthorized(t, testServer(t, &fakeStore{}), http.MethodGet, "/v1/workouts/"+uuid+"/series", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown workout status = %d, want 404", rec.Code)
	}
	assertJSONError(t, rec.Body.Bytes(), "workout not found")
}

func TestStateOfMindHandler(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{moods: []StateOfMindEntry{{
		UUID: "11111111-1111-4111-8111-111111111111", Date: "2026-09-06", Timestamp: 1788987600000,
		Kind: "momentaryEmotion", Valence: ptrFloat64(0.5), ValenceClassification: ptrString("pleasant"),
		Labels: []string{"calm"}, Associations: []string{"family"},
	}}})
	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/state-of-mind?start=1751328000000&end=1751414400000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Entries []StateOfMindEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Entries) != 1 || body.Entries[0].Kind != "momentaryEmotion" || body.Entries[0].Labels[0] != "calm" {
		t.Errorf("entries = %+v", body.Entries)
	}

	empty := serveAuthorized(t, testServer(t, &fakeStore{moods: []StateOfMindEntry{}}), http.MethodGet, "/v1/state-of-mind?start=1751328000000&end=1751414400000", nil)
	assertJSONKeyIsArray(t, empty.Body.Bytes(), "entries")

	bad := serveAuthorized(t, srv, http.MethodGet, "/v1/state-of-mind?start=nope&end=1751414400000", nil)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("status = %d", bad.Code)
	}
}

func TestProfileRejectsMissingBearerToken(t *testing.T) {
	t.Parallel()

	srv := &Server{
		store:     &fakeStore{},
		authorize: testAuthorize("secret", defaultUserID),
		log:       slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/profile", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "unauthorized" {
		t.Fatalf("body error = %q, want %q", body["error"], "unauthorized")
	}
}

func TestProfileReturnsJSONWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := &Server{
		store: &fakeStore{
			profile: Profile{
				UserID:        defaultUserID,
				Name:          ptrString("Ada Example"),
				Email:         ptrString("ada@example.com"),
				DateOfBirth:   ptrInt64(631152000000),
				BiologicalSex: ptrString("male"),
			},
		},
		authorize: testAuthorize("secret", defaultUserID),
		log:       slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/profile", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var body Profile
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.UserID != defaultUserID {
		t.Fatalf("userID = %q, want %q", body.UserID, defaultUserID)
	}
	if got := deref(body.Name); got != "Ada Example" {
		t.Fatalf("name = %q, want %q", got, "Ada Example")
	}
	if body.DateOfBirth == nil || *body.DateOfBirth != 631152000000 {
		t.Fatalf("dateOfBirth = %v, want %d", body.DateOfBirth, int64(631152000000))
	}
}

func TestCatalogTypesReturnsTypesWhenAuthorized(t *testing.T) {
	t.Parallel()

	earliest := int64(1751328000000)
	latest := int64(1751562000000)
	srv := &Server{
		store: &fakeStore{
			catalog: []CatalogType{
				{
					Identifier:    "HKQuantityTypeIdentifierHeartRate",
					Kind:          "quantity",
					Unit:          ptrString("count/min"),
					Rows:          42,
					RawRows:       40,
					AggregateRows: 2,
					Earliest:      &earliest,
					Latest:        &latest,
				},
			},
		},
		authorize: testAuthorize("secret", defaultUserID),
		log:       slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/catalog/types", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Types []CatalogType `json:"types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Types) != 1 {
		t.Fatalf("types len = %d, want 1", len(body.Types))
	}
	if body.Types[0].Identifier != "HKQuantityTypeIdentifierHeartRate" {
		t.Fatalf("identifier = %q, want %q", body.Types[0].Identifier, "HKQuantityTypeIdentifierHeartRate")
	}
	if body.Types[0].Earliest == nil || *body.Types[0].Earliest != earliest {
		t.Fatalf("earliest = %v, want %d", body.Types[0].Earliest, earliest)
	}
	if body.Types[0].Latest == nil || *body.Types[0].Latest != latest {
		t.Fatalf("latest = %v, want %d", body.Types[0].Latest, latest)
	}
	if body.Types[0].Rows != 42 || body.Types[0].RawRows != 40 || body.Types[0].AggregateRows != 2 {
		t.Fatalf("catalog counts = %#v", body.Types[0])
	}
}

func TestCatalogTypesUsesCacheAcrossRequests(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		catalog: []CatalogType{{Identifier: "HKQuantityTypeIdentifierHeartRate", Kind: "quantity"}},
	}
	srv := testServer(t, store)

	rec1 := serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", rec1.Code, http.StatusOK)
	}
	rec2 := serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d", rec2.Code, http.StatusOK)
	}
	if store.calls.catalog != 1 {
		t.Fatalf("catalog calls = %d, want 1", store.calls.catalog)
	}
}

func TestCatalogTypesReturnsEmptyArrayWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{catalog: []CatalogType{}})
	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/catalog/types", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertJSONKeyIsArray(t, rec.Body.Bytes(), "types")
}

func TestHealthzReturns503WhenPingFails(t *testing.T) {
	t.Parallel()

	srv := &Server{
		store:     &fakeStore{err: errors.New("db down")},
		authorize: testAuthorize("secret", defaultUserID),
		log:       slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["ok"] || body["db"] {
		t.Fatalf("body = %#v, want ok=false db=false", body)
	}
}

func TestLatestMetricsReturnsMetricsWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{
		latest: []LatestMetric{
			{
				Identifier: "HKQuantityTypeIdentifierHeartRate",
				Unit:       ptrString("count/min"),
				Value:      ptrFloat64(68),
				Timestamp:  1751562000000,
			},
		},
	})

	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/metrics/latest?types=HKQuantityTypeIdentifierHeartRate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Metrics []LatestMetric `json:"metrics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Metrics) != 1 || body.Metrics[0].Identifier != "HKQuantityTypeIdentifierHeartRate" {
		t.Fatalf("metrics = %#v", body.Metrics)
	}
}

func TestLatestMetricsReturnsEmptyArrayWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{latest: []LatestMetric{}})
	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/metrics/latest?types=HKQuantityTypeIdentifierHeartRate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertJSONKeyIsArray(t, rec.Body.Bytes(), "metrics")
}

func TestDailyMetricsReturnsMetricsWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{
		daily: []DailyMetric{
			{
				Identifier: "HKQuantityTypeIdentifierStepCount",
				Unit:       ptrString("count"),
				Days: []DailyPoint{
					{Date: "2026-07-01", Value: ptrFloat64(1234)},
				},
			},
		},
	})

	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=1751328000000&end=1751414400000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Metrics []DailyMetric `json:"metrics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Metrics) != 1 || body.Metrics[0].Identifier != "HKQuantityTypeIdentifierStepCount" {
		t.Fatalf("metrics = %#v", body.Metrics)
	}
}

func TestDailyMetricsPagesInDaysAcrossTypes(t *testing.T) {
	t.Parallel()

	// A page of three day rows spread over two metrics: nextOffset counts
	// the rows, not the metrics, so the next page continues BodyMass.
	store := &fakeStore{daily: []DailyMetric{
		{Identifier: "HKQuantityTypeIdentifierStepCount", Unit: ptrString("count"), Days: []DailyPoint{
			{Date: "2026-07-01", Value: ptrFloat64(1234)}, {Date: "2026-07-02", Value: nil},
		}},
		{Identifier: "HKQuantityTypeIdentifierBodyMass", Unit: ptrString("kg"), Days: []DailyPoint{
			{Date: "2026-07-01", Value: ptrFloat64(80.5)},
		}},
	}}
	srv := testServer(t, store)

	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/metrics/daily?types=HKQuantityTypeIdentifierStepCount,HKQuantityTypeIdentifierBodyMass&start=1751328000000&end=1751414400000&limit=3&offset=40", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if store.lastDaily.Limit != 3 || store.lastDaily.Offset != 40 || len(store.lastDaily.Types) != 2 {
		t.Errorf("filters = %+v", store.lastDaily)
	}
	if store.lastDaily.Start.UnixMilli() != 1751328000000 || store.lastDaily.End.UnixMilli() != 1751414400000 {
		t.Errorf("range = %s .. %s", store.lastDaily.Start, store.lastDaily.End)
	}
	var body struct {
		Metrics    []DailyMetric `json:"metrics"`
		NextOffset int           `json:"nextOffset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Metrics) != 2 || body.NextOffset != 43 {
		t.Fatalf("body = %+v (nextOffset should be offset + day rows)", body)
	}

	// No limit: the documented default, not the whole range, and the cap
	// clamps rather than rejects.
	rec = serveAuthorized(t, srv, http.MethodGet, "/v1/metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=1751328000000&end=1751414400000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if store.lastDaily.Limit != defaultDailyLimit || store.lastDaily.Offset != 0 {
		t.Errorf("defaults = %+v", store.lastDaily)
	}
	rec = serveAuthorized(t, srv, http.MethodGet, "/v1/metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=1751328000000&end=1751414400000&limit=999999", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if store.lastDaily.Limit != maxDailyLimit {
		t.Errorf("limit = %d, want the cap %d", store.lastDaily.Limit, maxDailyLimit)
	}

	for _, bad := range []string{"limit=0", "limit=ten", "offset=-1", "offset=x"} {
		rec = serveAuthorized(t, srv, http.MethodGet, "/v1/metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=1751328000000&end=1751414400000&"+bad, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", bad, rec.Code, rec.Body.String())
		}
	}
}

func TestDailyMetricsReturnsEmptyArrayWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{daily: []DailyMetric{}})
	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=1751328000000&end=1751414400000&offset=7", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertJSONKeyIsArray(t, rec.Body.Bytes(), "metrics")
	if !strings.Contains(rec.Body.String(), `"nextOffset":7`) {
		t.Errorf("an empty page should still carry nextOffset = offset: %s", rec.Body.String())
	}
}

func TestActivitySummaryReturnsDaysWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{
		activity: []ActivityDay{
			{Date: "2026-07-01", MoveMode: ptrInt(1), MoveKcal: ptrFloat64(400)},
		},
	})

	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/activity/summary?start=1751328000000&end=1751414400000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Days []ActivityDay `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Days) != 1 || body.Days[0].Date != "2026-07-01" {
		t.Fatalf("days = %#v", body.Days)
	}
}

func TestActivitySummaryReturnsEmptyArrayWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{activity: []ActivityDay{}})
	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/activity/summary?start=1751328000000&end=1751414400000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertJSONKeyIsArray(t, rec.Body.Bytes(), "days")
}

func TestWorkoutsReturnsWorkoutsAndNextOffset(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{
		workouts: []WorkoutSummary{
			{UUID: "11111111-1111-4111-8111-111111111111", ActivityType: "HKWorkoutActivityTypeRunning", Start: 1751562000000, End: 1751565600000},
			{UUID: "22222222-2222-4222-8222-222222222222", ActivityType: "HKWorkoutActivityTypeWalking", Start: 1751552000000, End: 1751555600000},
		},
	})

	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/workouts?limit=50&offset=10", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Workouts   []WorkoutSummary `json:"workouts"`
		NextOffset int              `json:"nextOffset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Workouts) != 2 {
		t.Fatalf("workouts len = %d, want 2", len(body.Workouts))
	}
	if body.NextOffset != 12 {
		t.Fatalf("nextOffset = %d, want 12", body.NextOffset)
	}
}

func TestWorkoutsReturnsEmptyArrayWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{workouts: []WorkoutSummary{}})
	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/workouts?limit=50&offset=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertJSONKeyIsArray(t, rec.Body.Bytes(), "workouts")
}

func TestWorkoutReturnsDetailWhenAuthorized(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{
		workout: &WorkoutDetail{
			WorkoutSummary: WorkoutSummary{
				UUID:         "11111111-1111-4111-8111-111111111111",
				ActivityType: "HKWorkoutActivityTypeRunning",
				Start:        1751562000000,
				End:          1751565600000,
			},
			StatisticsDetail: map[string]WorkoutStatDetail{
				"HKQuantityTypeIdentifierHeartRate": {Avg: ptrFloat64(68)},
			},
		},
	})

	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/workouts/11111111-1111-4111-8111-111111111111", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body WorkoutDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.UUID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("uuid = %q", body.UUID)
	}
}

func TestWorkoutReturnsNotFoundWhenStoreReturnsNil(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	rec := serveAuthorized(t, srv, http.MethodGet, "/v1/workouts/11111111-1111-4111-8111-111111111111", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertJSONError(t, rec.Body.Bytes(), "workout not found")
}

func TestResponseContractJSONShapes(t *testing.T) {
	t.Parallel()

	timestamp := int64(1751562000000)
	moveMode := 2
	start := int64(1751562000000)
	end := int64(1751565600000)

	latestMetricJSON := mustMarshal(t, LatestMetric{
		Identifier: "HKQuantityTypeIdentifierHeartRate",
		Unit:       ptrString("count/min"),
		Value:      ptrFloat64(68),
		Timestamp:  timestamp,
	})
	var latestMetricBody map[string]any
	if err := json.Unmarshal(latestMetricJSON, &latestMetricBody); err != nil {
		t.Fatalf("decode latest metric: %v", err)
	}
	if _, ok := latestMetricBody["timestamp"].(float64); !ok {
		t.Fatalf("timestamp type = %T, want numeric", latestMetricBody["timestamp"])
	}
	if got := int64(latestMetricBody["timestamp"].(float64)); got != timestamp {
		t.Fatalf("timestamp = %d, want %d", got, timestamp)
	}

	activityDayJSON := mustMarshal(t, ActivityDay{
		Date:     "2026-07-03",
		MoveMode: ptrInt(2),
	})
	var activityDayBody map[string]any
	if err := json.Unmarshal(activityDayJSON, &activityDayBody); err != nil {
		t.Fatalf("decode activity day: %v", err)
	}
	if _, ok := activityDayBody["moveMode"].(float64); !ok {
		t.Fatalf("moveMode type = %T, want numeric", activityDayBody["moveMode"])
	}
	if got := int(activityDayBody["moveMode"].(float64)); got != moveMode {
		t.Fatalf("moveMode = %d, want %d", got, moveMode)
	}

	workoutSummaryJSON := mustMarshal(t, WorkoutSummary{
		UUID:             "11111111-1111-4111-8111-111111111111",
		ActivityType:     "HKWorkoutActivityTypeRunning",
		Start:            start,
		End:              end,
		AvailableMetrics: []string{"HKQuantityTypeIdentifierHeartRate"},
	})
	var workoutSummaryBody map[string]any
	if err := json.Unmarshal(workoutSummaryJSON, &workoutSummaryBody); err != nil {
		t.Fatalf("decode workout summary: %v", err)
	}
	if _, ok := workoutSummaryBody["start"].(float64); !ok {
		t.Fatalf("start type = %T, want numeric", workoutSummaryBody["start"])
	}
	if got := int64(workoutSummaryBody["start"].(float64)); got != start {
		t.Fatalf("start = %d, want %d", got, start)
	}
	if _, ok := workoutSummaryBody["end"].(float64); !ok {
		t.Fatalf("end type = %T, want numeric", workoutSummaryBody["end"])
	}
	if got := int64(workoutSummaryBody["end"].(float64)); got != end {
		t.Fatalf("end = %d, want %d", got, end)
	}

	workoutDetailJSON := mustMarshal(t, WorkoutDetail{
		WorkoutSummary: WorkoutSummary{
			UUID:         "11111111-1111-4111-8111-111111111111",
			ActivityType: "HKWorkoutActivityTypeRunning",
			Start:        start,
			End:          end,
		},
		StatisticsDetail: map[string]WorkoutStatDetail{
			"HKQuantityTypeIdentifierHeartRate": {
				Avg: ptrFloat64(68),
			},
		},
	})
	var workoutDetailBody map[string]any
	if err := json.Unmarshal(workoutDetailJSON, &workoutDetailBody); err != nil {
		t.Fatalf("decode workout detail: %v", err)
	}
	stats, ok := workoutDetailBody["statisticsDetail"].(map[string]any)
	if !ok {
		t.Fatalf("statisticsDetail type = %T, want object", workoutDetailBody["statisticsDetail"])
	}
	if _, ok := stats["HKQuantityTypeIdentifierHeartRate"]; !ok {
		t.Fatalf("statisticsDetail = %#v, want HKQuantityTypeIdentifierHeartRate key", stats)
	}
	if metric, ok := stats["HKQuantityTypeIdentifierHeartRate"].(map[string]any); !ok {
		t.Fatalf("statisticsDetail metric type = %T, want object", stats["HKQuantityTypeIdentifierHeartRate"])
	} else if got := metric["avg"].(float64); got != 68 {
		t.Fatalf("statisticsDetail avg = %v, want 68", got)
	}

	// Missing upload metadata is unreported, never invented as zero counts.
	userJSON := mustMarshal(t, User{UserID: defaultUserID, CreatedAt: timestamp})
	var userBody map[string]any
	if err := json.Unmarshal(userJSON, &userBody); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	if got, ok := userBody["createdAt"].(float64); !ok || int64(got) != timestamp {
		t.Fatalf("createdAt = %#v, want %d", userBody["createdAt"], timestamp)
	}
	for _, field := range []string{"lastSync", "batches", "uploadedSamples"} {
		if value, present := userBody[field]; present {
			t.Fatalf("unreported metadata %s=%v was present", field, value)
		}
	}
}

func TestParseTypesParam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		query   string
		want    []string
		wantErr string
	}{
		{name: "missing", query: "", wantErr: "missing types"},
		{name: "empty", query: "types=%20%20%20", wantErr: "missing types"},
		{name: "empty member", query: "types=HKQuantityTypeIdentifierHeartRate,", wantErr: "types must not contain empty values"},
		{name: "two types", query: "types=HKQuantityTypeIdentifierHeartRate,HKQuantityTypeIdentifierStepCount", want: []string{"HKQuantityTypeIdentifierHeartRate", "HKQuantityTypeIdentifierStepCount"}},
		{name: "dedupes types", query: "types=HKQuantityTypeIdentifierHeartRate,HKQuantityTypeIdentifierHeartRate,HKQuantityTypeIdentifierStepCount", want: []string{"HKQuantityTypeIdentifierHeartRate", "HKQuantityTypeIdentifierStepCount"}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := "/"
			if tc.query != "" {
				target += "?" + tc.query
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			got, err := parseTypesParam(req)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTypesParam err = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("type[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		query     string
		wantStart int64
		wantEnd   int64
		wantErr   string
	}{
		{name: "valid", query: "start=1751328000000&end=1751414400000", wantStart: 1751328000000, wantEnd: 1751414400000},
		{name: "invalid start", query: "start=nope&end=1751414400000", wantErr: "invalid start: must be epoch milliseconds"},
		{name: "end before start", query: "start=1751414400000&end=1751328000000", wantErr: "end must be after start"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil)
			start, end, err := parseRange(req)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRange err = %v", err)
			}
			if got := start.UnixMilli(); got != tc.wantStart {
				t.Fatalf("start = %d, want %d", got, tc.wantStart)
			}
			if got := end.UnixMilli(); got != tc.wantEnd {
				t.Fatalf("end = %d, want %d", got, tc.wantEnd)
			}
		})
	}
}

func TestParseLimitOffset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
		wantErr    string
	}{
		{name: "defaults", query: "", wantLimit: defaultLimit, wantOffset: 0},
		{name: "negative offset", query: "offset=-1", wantErr: "offset must be at least 0"},
		{name: "zero limit", query: "limit=0", wantErr: "limit must be at least 1"},
		{name: "invalid limit", query: "limit=abc", wantErr: "invalid limit: must be an integer"},
		{name: "clamps overlarge limit", query: "limit=999&offset=2", wantLimit: maxLimit, wantOffset: 2},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := "/"
			if tc.query != "" {
				target += "?" + tc.query
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			limit, offset, err := parseLimitOffset(req)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLimitOffset err = %v", err)
			}
			if limit != tc.wantLimit || offset != tc.wantOffset {
				t.Fatalf("limit,offset = %d,%d want %d,%d", limit, offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestHandleLatestMetricsRejectsMissingTypes(t *testing.T) {
	t.Parallel()

	rec := serveAuthorized(t, testServer(t, &fakeStore{}), http.MethodGet, "/v1/metrics/latest", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertJSONError(t, rec.Body.Bytes(), "missing types")
}

func TestHandleDailyMetricsRejectsInvalidStart(t *testing.T) {
	t.Parallel()

	rec := serveAuthorized(t, testServer(t, &fakeStore{}), http.MethodGet, "/v1/metrics/daily?types=HKQuantityTypeIdentifierHeartRate&start=nope&end=1751414400000", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertJSONError(t, rec.Body.Bytes(), "invalid start: must be epoch milliseconds")
}

func TestHandleWorkoutsRejectsInvalidLimitOffset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		query   string
		wantErr string
	}{
		{name: "bad limit", query: "/v1/workouts?limit=abc", wantErr: "invalid limit: must be an integer"},
		{name: "bad offset", query: "/v1/workouts?offset=-1", wantErr: "offset must be at least 0"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := serveAuthorized(t, testServer(t, &fakeStore{}), http.MethodGet, tc.query, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			assertJSONError(t, rec.Body.Bytes(), tc.wantErr)
		})
	}
}

func TestHandleWorkoutRejectsInvalidUUID(t *testing.T) {
	t.Parallel()

	rec := serveAuthorized(t, testServer(t, &fakeStore{}), http.MethodGet, "/v1/workouts/not-a-uuid", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertJSONError(t, rec.Body.Bytes(), "invalid workout uuid")
}

func ptrString(v string) *string { return &v }

func ptrInt64(v int64) *int64 { return &v }

func ptrFloat64(v float64) *float64 { return &v }

func ptrInt(v int) *int { return &v }

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return b
}

func testServer(t *testing.T, store apiStore) *Server {
	t.Helper()

	return &Server{
		store:     store,
		authorize: testAuthorize("secret", defaultUserID),
		log:       slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}
}

func serveAuthorized(t *testing.T, srv *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func assertJSONError(t *testing.T, body []byte, want string) {
	t.Helper()

	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if payload["error"] != want {
		t.Fatalf("error = %q, want %q", payload["error"], want)
	}
}

func assertJSONKeyIsArray(t *testing.T, body []byte, key string) {
	t.Helper()

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	raw, ok := payload[key]
	if !ok {
		t.Fatalf("missing key %q in %s", key, string(body))
	}
	if string(raw) != "[]" {
		t.Fatalf("%s = %s, want []", key, string(raw))
	}
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

type testWriter struct {
	t *testing.T
}

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	return len(p), nil
}

func TestMsParamRejectsOutOfRangeValues(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"-1", "253402300800000", "9223372036854775807"} {
		if _, err := msParam(raw, "start"); err == nil {
			t.Fatalf("msParam(%q) = nil error, want out-of-range error", raw)
		}
	}
	for _, raw := range []string{"0", "1751500000000", "253402300799999"} {
		if _, err := msParam(raw, "start"); err != nil {
			t.Fatalf("msParam(%q) = %v, want nil", raw, err)
		}
	}
}

func TestLoadTimeZone(t *testing.T) {
	if loc, err := loadTimeZone(""); err != nil || loc != time.UTC {
		t.Fatalf("loadTimeZone(\"\") = %v, %v; want UTC, nil", loc, err)
	}
	if loc, err := loadTimeZone(" Europe/Berlin "); err != nil || loc.String() != "Europe/Berlin" {
		t.Fatalf("loadTimeZone(Europe/Berlin) = %v, %v; want Europe/Berlin, nil", loc, err)
	}
	if _, err := loadTimeZone("Not/AZone"); err == nil || !strings.Contains(err.Error(), "PULS_TIME_ZONE") {
		t.Fatalf("loadTimeZone(Not/AZone) err = %v; want an error naming PULS_TIME_ZONE", err)
	}
}
