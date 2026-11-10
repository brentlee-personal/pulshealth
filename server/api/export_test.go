package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// exportServer is a Server over store, with a token the helpers below send.
func exportServer(t *testing.T, store apiStore) *Server {
	t.Helper()
	return &Server{
		store:     store,
		authorize: testAuthorize("secret", defaultUserID),
		log:       slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}
}

// getExport runs one GET /v1/export?query through the router.
func getExport(t *testing.T, srv *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/export?"+query, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// The fixture range: one day of 2026, in epoch milliseconds.
const (
	exportStartMS = 1767225600000
	exportEndMS   = 1767312000000
	exportRange   = "start=1767225600000&end=1767312000000"
)

func TestExportCSVWritesAHeaderRowAndOneRowPerDay(t *testing.T) {
	t.Parallel()

	unit := "count"
	store := &fakeStore{
		daily: []DailyMetric{{
			Identifier: "HKQuantityTypeIdentifierStepCount",
			Unit:       &unit,
			Days: []DailyPoint{
				{Date: "2026-01-01", Value: ptrFloat64(8123.5)},
				{Date: "2026-01-02", Value: nil},
			},
		}},
	}
	srv := exportServer(t, store)

	rec := getExport(t, srv, "format=csv&dataset=daily_metrics&types=HKQuantityTypeIdentifierStepCount&limit=1&"+exportRange)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	// A file is the whole range: the JSON endpoint's page size does not
	// apply, even when the query names one.
	if store.lastDaily.Limit != 0 || store.lastDaily.Offset != 0 {
		t.Errorf("export paged the store: %+v", store.lastDaily)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	want := `attachment; filename="puls-daily_metrics-1767225600000-1767312000000.csv"`
	if got := rec.Header().Get("Content-Disposition"); got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}

	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("the body is not CSV: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %#v, want a header and two rows", records)
	}
	header := strings.Join(records[0], ",")
	if header != "identifier,unit,date,value" {
		t.Errorf("header = %q", header)
	}
	if row := strings.Join(records[1], ","); row != "HKQuantityTypeIdentifierStepCount,count,2026-01-01,8123.5" {
		t.Errorf("first row = %q", row)
	}
	// A null value is an empty cell, not the string "null" or a zero.
	if row := strings.Join(records[2], ","); row != "HKQuantityTypeIdentifierStepCount,count,2026-01-02," {
		t.Errorf("second row = %q", row)
	}
}

func TestExportJSONLKeysAreTheCSVColumns(t *testing.T) {
	t.Parallel()

	unit := "count/min"
	srv := exportServer(t, &fakeStore{
		samples: &SamplesPage{
			Type: "HKQuantityTypeIdentifierHeartRate",
			Kind: "quantity",
			Unit: &unit,
			Samples: []Sample{
				{UUID: "11111111-1111-4111-8111-111111111111", Start: exportStartMS, End: exportStartMS, Value: ptrFloat64(61), Source: ptrString("Watch")},
				{UUID: "22222222-2222-4222-8222-222222222222", Start: exportStartMS + 1000, End: exportStartMS + 1000, Value: nil, Source: nil},
			},
		},
	})

	query := "dataset=samples&type=HKQuantityTypeIdentifierHeartRate&" + exportRange
	jsonl := getExport(t, srv, "format=jsonl&"+query)
	if jsonl.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", jsonl.Code, jsonl.Body.String())
	}
	if got := jsonl.Header().Get("Content-Type"); got != "application/x-ndjson" {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.HasSuffix(jsonl.Header().Get("Content-Disposition"), `.jsonl"`) {
		t.Errorf("Content-Disposition = %q", jsonl.Header().Get("Content-Disposition"))
	}

	lines := strings.Split(strings.TrimSuffix(jsonl.Body.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %#v, want one object per sample and no header line", lines)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	if first["type"] != "HKQuantityTypeIdentifierHeartRate" || first["unit"] != "count/min" || first["value"] != 61.0 {
		t.Errorf("line 1 = %v", first)
	}
	if first["source"] != "Watch" {
		t.Errorf("source = %v", first["source"])
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 2 is not JSON: %v", err)
	}
	// A null stays null rather than disappearing, so every line has the
	// same keys.
	if value, ok := second["value"]; !ok || value != nil {
		t.Errorf("line 2 value = %v (present %v), want an explicit null", value, ok)
	}

	// The two formats describe the same rows: the CSV header row is exactly
	// the JSONL object's keys, in order.
	csvRec := getExport(t, srv, "format=csv&"+query)
	records, err := csv.NewReader(strings.NewReader(csvRec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("the CSV body is not CSV: %v", err)
	}
	keys := jsonKeysInOrder(t, lines[0])
	if strings.Join(records[0], ",") != strings.Join(keys, ",") {
		t.Errorf("CSV header %q and JSONL keys %q disagree", records[0], keys)
	}
}

// jsonKeysInOrder reads the keys of a JSON object in the order they appear.
func jsonKeysInOrder(t *testing.T, line string) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(line))
	if _, err := decoder.Token(); err != nil { // the opening brace
		t.Fatalf("decode %q: %v", line, err)
	}
	var keys []string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		keys = append(keys, key.(string))
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
	}
	return keys
}

func TestExportFlattensNestedFieldsAndJoinsLists(t *testing.T) {
	t.Parallel()

	srv := exportServer(t, &fakeStore{
		nights: []SleepNight{{
			Date: "2026-01-02", Start: exportStartMS, End: exportEndMS,
			InBedMinutes: 480, AsleepMinutes: 430,
			Stages:  SleepStages{Core: 220, Deep: 60, REM: 100, Unspecified: 50, Awake: 20},
			Sources: 2,
		}},
		moods: []StateOfMindEntry{{
			UUID: "33333333-3333-4333-8333-333333333333", Date: "2026-01-01", Timestamp: exportStartMS,
			Kind: "momentaryEmotion", Valence: ptrFloat64(0.25), ValenceClassification: ptrString("slightlyPleasant"),
			Labels: []string{"calm", "content"}, Associations: []string{"family"},
		}},
	})

	sleep := getExport(t, srv, "format=csv&dataset=sleep&"+exportRange)
	records, err := csv.NewReader(strings.NewReader(sleep.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("sleep body is not CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("sleep records = %#v", records)
	}
	if got := strings.Join(records[0], ","); !strings.Contains(got, "stages.core,stages.deep,stages.rem,stages.unspecified,stages.awake") {
		t.Errorf("sleep header = %q, want the stage minutes flattened by path", got)
	}
	if got := strings.Join(records[1], ","); got != "2026-01-02,1767225600000,1767312000000,480,430,220,60,100,50,20,2" {
		t.Errorf("sleep row = %q", got)
	}

	// A list is comma-joined inside its (quoted) CSV cell, and stays a JSON
	// array in JSONL.
	moodCSV := getExport(t, srv, "format=csv&dataset=state_of_mind&"+exportRange)
	if !strings.Contains(moodCSV.Body.String(), `"calm,content"`) {
		t.Errorf("state of mind CSV = %q, want the labels joined in one quoted cell", moodCSV.Body.String())
	}
	moodJSONL := getExport(t, srv, "format=jsonl&dataset=state_of_mind&"+exportRange)
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(moodJSONL.Body.String())), &entry); err != nil {
		t.Fatalf("state of mind JSONL: %v", err)
	}
	labels, ok := entry["labels"].([]any)
	if !ok || len(labels) != 2 || labels[0] != "calm" {
		t.Errorf("labels = %v, want a JSON array", entry["labels"])
	}
}

func TestExportWorkoutsStreamsTheWholeRangeUnpaged(t *testing.T) {
	t.Parallel()

	store := &fakeStore{workouts: []WorkoutSummary{{
		UUID: "44444444-4444-4444-8444-444444444444", ActivityType: "HKWorkoutActivityTypeRunning",
		Start: exportStartMS, End: exportEndMS, DurationS: ptrFloat64(3600), DistanceM: ptrFloat64(10000),
		EnergyKcal: ptrFloat64(620), HasRoute: true,
		AvailableMetrics: []string{"HKQuantityTypeIdentifierHeartRate", "HKQuantityTypeIdentifierRunningPower"},
	}}}
	srv := exportServer(t, store)

	rec := getExport(t, srv, "format=csv&dataset=workouts&activityType=HKWorkoutActivityTypeRunning&"+exportRange)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	// The export asks for the whole range: no page size, and both bounds
	// pushed into the filter.
	if store.lastWorkouts.Limit != 0 || store.lastWorkouts.Offset != 0 {
		t.Errorf("filters = %+v, want no paging", store.lastWorkouts)
	}
	if store.lastWorkouts.Start == nil || store.lastWorkouts.Start.UnixMilli() != exportStartMS {
		t.Errorf("start = %v", store.lastWorkouts.Start)
	}
	if store.lastWorkouts.ActivityType != "HKWorkoutActivityTypeRunning" {
		t.Errorf("activityType = %q", store.lastWorkouts.ActivityType)
	}
	if !strings.Contains(rec.Body.String(), `"HKQuantityTypeIdentifierHeartRate,HKQuantityTypeIdentifierRunningPower"`) {
		t.Errorf("body = %q, want availableMetrics joined in one cell", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ",true,") {
		t.Errorf("body = %q, want hasRoute as a boolean", rec.Body.String())
	}
}

func TestExportRejectsBadRequests(t *testing.T) {
	t.Parallel()

	srv := exportServer(t, &fakeStore{})
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"no format", "dataset=sleep&" + exportRange, "missing format"},
		{"unknown format", "format=parquet&dataset=sleep&" + exportRange, "invalid format"},
		{"no dataset", "format=csv&" + exportRange, "missing dataset"},
		{"unknown dataset", "format=csv&dataset=heartbeats&" + exportRange, "invalid dataset"},
		{"no range", "format=csv&dataset=sleep", "missing start"},
		{"backwards range", "format=csv&dataset=sleep&start=2&end=1", "end must be after start"},
		{"daily metrics without types", "format=csv&dataset=daily_metrics&" + exportRange, "missing types"},
		{"samples without a type", "format=csv&dataset=samples&" + exportRange, "missing type"},
		{
			"samples over 31 days",
			"format=csv&dataset=samples&type=HKQuantityTypeIdentifierHeartRate&start=0&end=" +
				msString(32*24*time.Hour),
			"31 days",
		},
		{"sleep over 366 days", "format=csv&dataset=sleep&start=0&end=" + msString(367*24*time.Hour), "366 days"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := getExport(t, srv, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not the usual error JSON: %v", err)
			}
			if !strings.Contains(body["error"], tc.want) {
				t.Errorf("error = %q, want it to mention %q", body["error"], tc.want)
			}
		})
	}
}

// msString renders a duration as the epoch-millisecond bound a range
// parameter takes.
func msString(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

// limit and offset belong to the JSON endpoint's paging, not to an export.
// They are therefore neither honoured nor validated: a limit /v1/samples
// would reject is ignored here, and the whole range still comes back.
func TestExportIgnoresThePagingParameters(t *testing.T) {
	t.Parallel()

	unit := "count/min"
	store := &fakeStore{samples: &SamplesPage{
		Type: "HKQuantityTypeIdentifierHeartRate", Kind: "quantity", Unit: &unit,
		Samples: []Sample{
			{UUID: "11111111-1111-4111-8111-111111111111", Start: exportStartMS, End: exportStartMS, Value: ptrFloat64(61)},
			{UUID: "22222222-2222-4222-8222-222222222222", Start: exportStartMS + 1, End: exportStartMS + 1, Value: ptrFloat64(62)},
		},
	}}
	srv := exportServer(t, store)

	rec := getExport(t, srv,
		"format=csv&dataset=samples&type=HKQuantityTypeIdentifierHeartRate&limit=0&offset=-5&"+exportRange)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s — paging must not be validated here", rec.Code, rec.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("the body is not CSV: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %#v, want a header and both samples", records)
	}
	// A zero Limit is what the store reads as "the whole range".
	if store.lastSamples.Limit != 0 || store.lastSamples.Offset != 0 {
		t.Errorf("filters = %+v, want no paging", store.lastSamples)
	}
}

// A bad type is the store's 400, and it must arrive before the download
// starts: the response has to be a JSON error, not a CSV file with a header
// row and nothing under it.
func TestExportUnknownSampleTypeIsA400BeforeAnyRows(t *testing.T) {
	t.Parallel()

	srv := exportServer(t, &fakeStore{sampleTypeErr: badRequestf("unknown type %q: it has never been synced", "HKNope")})
	rec := getExport(t, srv, "format=csv&dataset=samples&type=HKNope&"+exportRange)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want the error JSON", got)
	}
	if strings.Contains(rec.Body.String(), "uuid,start") {
		t.Errorf("body = %q, want no CSV header", rec.Body.String())
	}
}

func TestExportStoreFailureBeforeTheStreamIsA500(t *testing.T) {
	t.Parallel()

	srv := exportServer(t, &fakeStore{err: errors.New("boom")})
	rec := getExport(t, srv, "format=csv&dataset=sleep&"+exportRange)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestExportRequiresTheToken(t *testing.T) {
	t.Parallel()

	srv := exportServer(t, &fakeStore{})
	req := httptest.NewRequest(http.MethodGet, "/v1/export?format=csv&dataset=sleep&"+exportRange, nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// The response is chunked and it really streams — that is the whole point of
// the endpoint. The scan is held at two points, so each assertion can only
// pass if the bytes left the server while the handler was still inside it:
// the header row before a single data row exists, then a flush window of rows
// before the scan has finished. Finally, a failure once the body is on the
// wire aborts the transfer rather than closing a short file cleanly.
func TestExportStreamsChunkedAndAbortsOnAMidStreamFailure(t *testing.T) {
	t.Parallel()

	var (
		headerRead = make(chan struct{}) // closed once the test holds the header row
		rowsRead   = make(chan struct{}) // closed once it holds the first flush window
	)
	failing := &streamingStore{
		fakeStore: fakeStore{},
		workoutRows: func(_ context.Context, fn func(WorkoutSummary) error) error {
			// Not one row is produced until the test has read the header, so
			// a header on the wire can only have come from the flush that
			// follows it.
			<-headerRead
			for i := 0; i < exportFlushRows; i++ {
				if err := fn(exportTestWorkout(i)); err != nil {
					return err
				}
			}
			<-rowsRead
			return errors.New("the database went away")
		},
	}
	srv := exportServer(t, failing)
	server := httptest.NewServer(srv.routes())
	defer server.Close()
	// The blocked scan must not outlive the test if an assertion fails early.
	defer func() {
		safeClose(headerRead)
		safeClose(rowsRead)
	}()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/export?format=csv&dataset=workouts&"+exportRange, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.ContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1: an export must not be buffered to a known length", resp.ContentLength)
	}
	if len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" {
		t.Errorf("TransferEncoding = %v, want chunked", resp.TransferEncoding)
	}

	// The header row arrives before the scan has produced anything.
	reader := csv.NewReader(resp.Body)
	header, err := reader.Read()
	if err != nil {
		t.Fatalf("reading the header row before the first data row exists: %v", err)
	}
	if header[0] != "uuid" {
		t.Errorf("header = %v", header)
	}
	close(headerRead)

	// One flush window of rows arrives while the handler is still inside the
	// scan, in order.
	for i := 0; i < exportFlushRows; i++ {
		row, err := reader.Read()
		if err != nil {
			t.Fatalf("reading row %d while the scan is still running: %v", i, err)
		}
		if want := exportTestWorkout(i).UUID; row[0] != want {
			t.Fatalf("row %d = %v, want uuid %s", i, row, want)
		}
	}
	close(rowsRead)

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("the rest of the body read cleanly; a mid-stream failure must break the transfer")
	}
}

// An export holds a database connection for as long as the download takes,
// so only maxConcurrentExports may be in flight; the next one is refused at
// once rather than queued behind them.
func TestExportRefusesMoreThanTheConcurrencyLimit(t *testing.T) {
	t.Parallel()

	var (
		inFlight    = make(chan struct{}, maxConcurrentExports)
		release     = make(chan struct{})
		releaseOnce sync.Once
	)
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	srv := exportServer(t, &streamingStore{
		workoutRows: func(context.Context, func(WorkoutSummary) error) error {
			inFlight <- struct{}{}
			<-release
			return nil
		},
	})

	// Fill every slot and leave the handlers parked inside their scans.
	done := make(chan int, maxConcurrentExports)
	for i := 0; i < maxConcurrentExports; i++ {
		go func() {
			done <- getExport(t, srv, "format=csv&dataset=workouts&"+exportRange).Code
		}()
	}
	for i := 0; i < maxConcurrentExports; i++ {
		<-inFlight
	}

	rec := getExport(t, srv, "format=csv&dataset=workouts&"+exportRange)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while every slot is busy; body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on the 503")
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the usual error JSON: %v", err)
	}
	if !strings.Contains(body["error"], "exports may run at once") {
		t.Errorf("error = %q", body["error"])
	}

	// A request that cannot stream never takes a slot, so a burst of
	// malformed ones cannot 503 the real ones.
	if bad := getExport(t, srv, "format=parquet&dataset=workouts&"+exportRange); bad.Code != http.StatusBadRequest {
		t.Errorf("a bad request while every slot is busy = %d, want 400", bad.Code)
	}

	// A slot freed by a finished export is reusable.
	releaseAll()
	for i := 0; i < maxConcurrentExports; i++ {
		if code := <-done; code != http.StatusOK {
			t.Fatalf("a parked export finished with %d", code)
		}
	}
	if rec := getExport(t, srv, "format=csv&dataset=workouts&"+exportRange); rec.Code != http.StatusOK {
		t.Fatalf("status = %d after the slots were released, want 200", rec.Code)
	}
}

// A client that hangs up mid-download — a Ctrl-C on a long export — is
// normal, not a failure: the handler returns quietly instead of aborting the
// response and logging an error nobody caused.
func TestExportClientDisconnectIsNotAFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	srv := exportServer(t, &streamingStore{
		workoutRows: func(_ context.Context, fn func(WorkoutSummary) error) error {
			if err := fn(exportTestWorkout(0)); err != nil {
				return err
			}
			cancel()
			return ctx.Err()
		},
	})

	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/v1/export?format=csv&dataset=workouts&"+exportRange, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	// A panic(http.ErrAbortHandler) here would fail the test: nothing between
	// this call and net/http would recover it.
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// What was already flushed stands; the buffered tail is dropped, because
	// there is nobody to send it to.
	if !strings.HasPrefix(rec.Body.String(), "uuid,activityType,") {
		t.Errorf("body = %q, want the header row that was already on the wire", rec.Body.String())
	}
}

// exportTestWorkout is the n-th row of the streaming fixture, with a uuid
// that names its position.
func exportTestWorkout(n int) WorkoutSummary {
	return WorkoutSummary{
		UUID:         fmt.Sprintf("55555555-5555-4555-8555-%012d", n),
		ActivityType: "HKWorkoutActivityTypeWalking",
		Start:        exportStartMS, End: exportEndMS,
		AvailableMetrics: []string{},
	}
}

// safeClose closes ch unless it is closed already, so a deferred release
// cannot panic after the test closed the channel itself.
func safeClose(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// streamingStore is a fakeStore whose workout scan is a function, so a test
// can make it block or fail part-way through.
type streamingStore struct {
	fakeStore
	workoutRows func(ctx context.Context, fn func(WorkoutSummary) error) error
}

func (s *streamingStore) StreamWorkouts(ctx context.Context, _ string, _ WorkoutFilters, fn func(WorkoutSummary) error) error {
	return s.workoutRows(ctx, fn)
}

// deadlineRecorder is a ResponseRecorder that also records the write
// deadlines set through http.ResponseController.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

// A client that stops reading must not hold an export slot, and a pooled
// connection, forever: every write and flush renews a deadline of
// exportWriteTimeout, and the whole export runs under exportMaxDuration.
func TestExportBoundsEveryWriteAndTheWholeExport(t *testing.T) {
	t.Parallel()

	var ceiling time.Time
	srv := exportServer(t, &streamingStore{
		workoutRows: func(ctx context.Context, fn func(WorkoutSummary) error) error {
			ceiling, _ = ctx.Deadline()
			for i := 0; i < 2*exportFlushRows; i++ {
				if err := fn(exportTestWorkout(i)); err != nil {
					return err
				}
			}
			return nil
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/export?format=csv&dataset=workouts&"+exportRange, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	srv.routes().ServeHTTP(rec, req)
	after := time.Now()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	// The header flush, one per window, the final one, and the writes the
	// encoder's buffer makes in between.
	if len(rec.deadlines) < 4 {
		t.Fatalf("%d write deadlines set, want one before every write and flush", len(rec.deadlines))
	}
	for i, d := range rec.deadlines {
		if d.Before(before.Add(exportWriteTimeout)) || d.After(after.Add(exportWriteTimeout)) {
			t.Fatalf("deadline %d = %v, want %v after the write", i, d, exportWriteTimeout)
		}
	}
	if ceiling.Before(before.Add(exportMaxDuration)) || ceiling.After(after.Add(exportMaxDuration)) {
		t.Errorf("the scan ran with deadline %v, want %v after the request", ceiling, exportMaxDuration)
	}
}

// Past the ceiling the writer refuses every write and flush, which the
// handler turns into an aborted transfer rather than a clean short file.
func TestExportPastItsCeilingWritesNothingMore(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := &exportWriter{ctx: ctx, w: io.Discard, control: http.NewResponseController(httptest.NewRecorder())}
	if _, err := out.Write([]byte("row\n")); !errors.Is(err, context.Canceled) {
		t.Errorf("Write = %v, want the context's error", err)
	}
	if err := out.Flush(); !errors.Is(err, context.Canceled) {
		t.Errorf("Flush = %v, want the context's error", err)
	}
}
