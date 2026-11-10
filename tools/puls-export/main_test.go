package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noEnv is an empty environment, so a test never picks up the developer's own
// PULS_API_URL or PULS_API_TOKEN.
func noEnv(string) string { return "" }

func credentialFile(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "personal-api-token")
	if err := os.WriteFile(path, []byte(value+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// exportRecorder is a fake product API: it records the request it received
// and answers with body.
type exportRecorder struct {
	server *httptest.Server
	// Set by the handler before it answers.
	path   string
	query  url.Values
	header http.Header

	status int
	body   string
	// Called after the first chunk of body has been flushed, if set.
	afterFirstChunk func()
}

func newExportRecorder(t *testing.T, status int, body string) *exportRecorder {
	t.Helper()
	rec := &exportRecorder{status: status, body: body}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.query = r.URL.Query()
		rec.header = r.Header.Clone()
		if rec.status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.status)
			_, _ = io.WriteString(w, rec.body)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if rec.afterFirstChunk == nil {
			_, _ = io.WriteString(w, rec.body)
			return
		}
		// Two chunks with a rendezvous in between, so a test can prove the
		// client wrote the first one out before the response ended.
		first, second, _ := strings.Cut(rec.body, "\n")
		_, _ = io.WriteString(w, first+"\n")
		http.NewResponseController(w).Flush()
		rec.afterFirstChunk()
		_, _ = io.WriteString(w, second)
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func TestRunStreamsAnExportToAFile(t *testing.T) {
	t.Parallel()

	body := "date,moveKcal\n2026-01-01,410\n2026-01-02,505\n"
	api := newExportRecorder(t, http.StatusOK, body)
	out := filepath.Join(t.TempDir(), "activity.csv")

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"--url", api.server.URL, "--token-file", credentialFile(t, "secret"),
		"--dataset", "activity", "--format", "csv",
		"--start", "1767225600000", "--end", "1767312000000",
		"-o", out,
	}, &stdout, &stderr, noEnv)
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}

	if api.path != "/v1/export" {
		t.Errorf("path = %q", api.path)
	}
	if got := api.header.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q", got)
	}
	want := url.Values{
		"dataset": {"activity"}, "format": {"csv"},
		"start": {"1767225600000"}, "end": {"1767312000000"},
	}
	if api.query.Encode() != want.Encode() {
		t.Errorf("query = %q, want %q", api.query.Encode(), want.Encode())
	}

	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	if string(written) != body {
		t.Errorf("file = %q, want %q", written, body)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing when -o names a file", stdout.String())
	}
}

func TestRunWritesToStdoutWithoutAnOutputFile(t *testing.T) {
	t.Parallel()

	body := "{\"date\":\"2026-01-01\"}\n"
	api := newExportRecorder(t, http.StatusOK, body)

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"--url", api.server.URL, "--token-file", credentialFile(t, "secret"),
		"--dataset", "sleep", "--format", "jsonl",
		"--start", "2026-01-01", "--end", "2026-02-01",
	}, &stdout, &stderr, noEnv)
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if stdout.String() != body {
		t.Errorf("stdout = %q, want %q", stdout.String(), body)
	}
	// A YYYY-MM-DD bound is midnight in the zone, and the range is half-open.
	if got := api.query.Get("start"); got != "1767225600000" {
		t.Errorf("start = %q, want midnight UTC on 2026-01-01", got)
	}
	if got := api.query.Get("end"); got != "1769904000000" {
		t.Errorf("end = %q, want midnight UTC on 2026-02-01", got)
	}
}

// The bytes reach the destination while the response is still open: the CLI
// copies the stream through rather than reading the export into memory first.
func TestRunCopiesTheBodyBeforeTheResponseEnds(t *testing.T) {
	t.Parallel()

	var (
		firstChunkOut = make(chan struct{})
		timedOut      atomic.Bool
	)
	api := newExportRecorder(t, http.StatusOK, "date,moveKcal\n2026-01-01,410\n")
	written := &signalWriter{seen: firstChunkOut}
	api.afterFirstChunk = func() {
		select {
		case <-firstChunkOut:
		case <-time.After(10 * time.Second):
			// The client never passed the first chunk on while the response
			// was still open. Recorded rather than failed here, because this
			// runs on the server's goroutine.
			timedOut.Store(true)
		}
	}

	opts := options{
		baseURL: api.server.URL, token: "secret",
		dataset: "activity", format: "csv", start: 1, end: 2,
	}
	if err := download(context.Background(), http.DefaultClient, opts, written); err != nil {
		t.Fatalf("download: %v", err)
	}
	if timedOut.Load() {
		t.Fatal("the first chunk never reached the destination while the response was open: " +
			"the body was buffered instead of streamed")
	}
	if !strings.HasPrefix(written.String(), "date,moveKcal\n") {
		t.Errorf("output = %q", written.String())
	}
}

// signalWriter closes seen on its first write, so a test can hold the server
// until the client has passed a chunk on to its destination.
type signalWriter struct {
	buf    bytes.Buffer
	seen   chan struct{}
	closed bool
}

func (w *signalWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if !w.closed && n > 0 {
		w.closed = true
		close(w.seen)
	}
	return n, err
}

func (w *signalWriter) String() string { return w.buf.String() }

// A rejection keeps yesterday's export and omits the external response body.
func TestRunReportsTheAPIErrorAndLeavesTheOutputFileAlone(t *testing.T) {
	t.Parallel()

	api := newExportRecorder(t, http.StatusBadRequest,
		`{"error":"range must not exceed 31 days"}`)
	out := filepath.Join(t.TempDir(), "samples.csv")
	if err := os.WriteFile(out, []byte("the previous export\n"), 0o600); err != nil {
		t.Fatalf("seed the output file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"--url", api.server.URL, "--token-file", credentialFile(t, "secret"),
		"--dataset", "samples", "--type", "HKQuantityTypeIdentifierHeartRate",
		"--start", "0", "--end", "9999999999",
		"-o", out,
	}, &stdout, &stderr, noEnv)
	if err == nil {
		t.Fatal("run returned nil for a 400")
	}
	if errors.Is(err, errUsage) {
		t.Errorf("err = %v, want a download failure (exit 1), not a usage error", err)
	}
	if !strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "range must not exceed 31 days") {
		t.Errorf("err = %v, want only the refusal status without the external body", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "the previous export\n" {
		t.Errorf("the output file was touched: %q", got)
	}
	// The filter for this dataset is sent, the others are not.
	if api.query.Get("type") != "HKQuantityTypeIdentifierHeartRate" {
		t.Errorf("type = %q", api.query.Get("type"))
	}
	if api.query.Has("types") || api.query.Has("activityType") {
		t.Errorf("query carries another dataset's filters: %q", api.query.Encode())
	}
}

func TestRunSaysTheTokenWasRejected(t *testing.T) {
	t.Parallel()

	api := newExportRecorder(t, http.StatusUnauthorized, `{"error":"unauthorized"}`)
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"--url", api.server.URL, "--token-file", credentialFile(t, "wrong"),
		"--dataset", "sleep", "--start", "1", "--end", "2",
	}, &stdout, &stderr, noEnv)
	if err == nil || !strings.Contains(err.Error(), "personal credential") {
		t.Fatalf("err = %v, want it to name the token", err)
	}
}

func TestRunRejectsBadCommandLines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no dataset", []string{"--token-file", credentialFile(t, "t"), "--start", "1", "--end", "2"}, "--dataset is required"},
		{"no token", []string{"--dataset", "sleep", "--start", "1", "--end", "2"}, "PULS_API_TOKEN_FILE"},
		{"no start", []string{"--dataset", "sleep", "--token-file", credentialFile(t, "t"), "--end", "2"}, "--start is required"},
		{"no end", []string{"--dataset", "sleep", "--token-file", credentialFile(t, "t"), "--start", "1"}, "--end is required"},
		{
			"a bound that is neither a date nor milliseconds",
			[]string{"--dataset", "sleep", "--token-file", credentialFile(t, "t"), "--start", "last tuesday", "--end", "2"},
			"neither YYYY-MM-DD nor epoch milliseconds",
		},
		{
			"a backwards range",
			[]string{"--dataset", "sleep", "--token-file", credentialFile(t, "t"), "--start", "2026-02-01", "--end", "2026-01-01"},
			"--end must be after --start",
		},
		{
			"a base URL that is not absolute",
			[]string{"--dataset", "sleep", "--token-file", credentialFile(t, "t"), "--start", "1", "--end", "2", "--url", "localhost:8081"},
			"absolute http(s) URL",
		},
		{
			"an unknown time zone",
			[]string{"--dataset", "sleep", "--token-file", credentialFile(t, "t"), "--start", "1", "--end", "2", "--time-zone", "Not/AZone"},
			"not a valid IANA time zone",
		},
		{
			"a stray positional argument",
			[]string{"--dataset", "sleep", "--token-file", credentialFile(t, "t"), "--start", "1", "--end", "2", "sleep.csv"},
			`unexpected argument "sleep.csv"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), tc.args, &stdout, &stderr, noEnv)
			if err == nil {
				t.Fatal("run returned nil")
			}
			if !errors.Is(err, errUsage) {
				t.Errorf("err = %v, want a usage error (exit 2)", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRunReadsTheEnvironment(t *testing.T) {
	t.Parallel()

	api := newExportRecorder(t, http.StatusOK, "date\n2026-01-01\n")
	env := map[string]string{
		"PULS_API_URL":        api.server.URL,
		"PULS_API_TOKEN_FILE": credentialFile(t, "from-the-environment"),
		"PULS_TIME_ZONE":      "Europe/Berlin",
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"--dataset", "activity", "--start", "2026-01-01", "--end", "2026-01-02",
	}, &stdout, &stderr, func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if got := api.header.Get("Authorization"); got != "Bearer from-the-environment" {
		t.Errorf("Authorization = %q", got)
	}
	// Midnight in Berlin is an hour before midnight UTC.
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, berlin).UnixMilli()
	if got := api.query.Get("start"); got != strconv.FormatInt(want, 10) {
		t.Errorf("start = %q, want %d (midnight in Europe/Berlin)", got, want)
	}
}

// An unknown flag is a command-line mistake like any other (exit 2), and the
// flag package has already written the complaint and the usage block, so main
// must not print it a second time.
func TestRunUnknownFlagIsAUsageErrorReportedOnce(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"--nonsense"}, &stdout, &stderr, noEnv)
	if err == nil {
		t.Fatal("run returned nil for an unknown flag")
	}
	if !errors.Is(err, errUsage) {
		t.Errorf("err = %v, want a usage error (exit 2)", err)
	}
	if err.Error() != "" {
		t.Errorf("err message = %q, want empty so main does not reprint what flag already wrote", err.Error())
	}
	if got := stderr.String(); !strings.Contains(got, "flag provided but not defined: -nonsense") ||
		!strings.Contains(got, "usage: puls-export") {
		t.Errorf("stderr = %q, want the flag package's complaint and the usage block", got)
	}
	if n := strings.Count(stderr.String(), "not defined"); n != 1 {
		t.Errorf("the complaint appears %d times, want 1", n)
	}
}

func TestRunHelpAndVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, &stdout, &stderr, noEnv); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h err = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(stderr.String(), "usage: puls-export") {
		t.Errorf("-h printed %q", stderr.String())
	}

	stdout.Reset()
	if err := run(context.Background(), []string{"--version"}, &stdout, &stderr, noEnv); err != nil {
		t.Fatalf("--version: %v", err)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Error("--version printed nothing")
	}
}

func TestExportURLKeepsABasePath(t *testing.T) {
	t.Parallel()

	got, err := exportURL(options{
		baseURL: "https://health.example.net/puls/", dataset: "workouts", format: "csv",
		start: 1, end: 2, activityType: "HKWorkoutActivityTypeRunning",
	})
	if err != nil {
		t.Fatalf("exportURL: %v", err)
	}
	want := "https://health.example.net/puls/v1/export?" +
		"activityType=HKWorkoutActivityTypeRunning&dataset=workouts&end=2&format=csv&start=1"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}

func TestRunHasNoMemberSelection(t *testing.T) {
	t.Parallel()
	api := newExportRecorder(t, http.StatusOK, "date\n2026-01-01\n")
	var stdout, stderr bytes.Buffer
	args := []string{"--url", api.server.URL, "--token-file", credentialFile(t, "personal"), "--dataset", "activity", "--start", "1", "--end", "2"}
	if err := run(context.Background(), args, &stdout, &stderr, noEnv); err != nil {
		t.Fatal(err)
	}
	if api.query.Has("user") {
		t.Fatal("request selected a member")
	}
	for _, name := range []string{"PULS_API_TOKEN", "PULS_USER_ID", "PULS_MULTI_USER"} {
		getenv := func(key string) string {
			if key == name {
				return "synthetic-private-value"
			}
			return ""
		}
		err := run(context.Background(), args, &stdout, &stderr, getenv)
		if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "synthetic-private-value") {
			t.Fatal("retired setting accepted or disclosed")
		}
	}
	for _, retired := range [][]string{{"--token", "synthetic-private-value"}, {"--user", "other"}} {
		if err := run(context.Background(), append(append([]string{}, args...), retired...), &stdout, &stderr, noEnv); err == nil {
			t.Fatal("retired flag accepted")
		}
	}
}

func TestRunExplainsPersonalScopeRefusal(t *testing.T) {
	t.Parallel()
	api := newExportRecorder(t, http.StatusForbidden, "{\"error\":\"synthetic-private-response\"}")
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"--url", api.server.URL, "--token-file", credentialFile(t, "personal"),
		"--dataset", "sleep", "--start", "1", "--end", "2"}, &stdout, &stderr, noEnv)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "personal credential") || strings.Contains(err.Error(), "synthetic-private-response") {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if errors.Is(err, errUsage) {
		t.Fatal("server refusal is not a usage error")
	}
}

func TestDownloadRefusesCredentialRedirect(t *testing.T) {
	t.Parallel()
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	var stdout bytes.Buffer
	err := download(context.Background(), http.DefaultClient, options{baseURL: redirect.URL, token: "personal", dataset: "sleep", format: "csv", start: 1, end: 2}, &stdout)
	if err == nil || received.Load() != 0 || stdout.Len() != 0 {
		t.Fatal("redirect forwarded a personal credential or wrote an export")
	}
}
