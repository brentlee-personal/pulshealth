// Command puls-export downloads one dataset from a PulsHealth deployment as a
// CSV or JSONL file.
//
// It is a thin client for GET /v1/export on the product API (server/api): it
// builds the query, sends the bearer token, and copies the response body
// straight to a file or to standard output without ever holding the whole
// export in memory. The server validates the personal credential and range.
// Errors report status only, keeping external response bodies out of logs.
//
//	puls-export --url https://health.example.net --token-file /protected/personal-api-token \
//	  --dataset sleep --format csv --start 2026-01-01 --end 2026-02-01 -o sleep.csv
//
// See docs/export.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	// Embed the IANA zone database so --time-zone resolves on a machine
	// without /usr/share/zoneinfo.
	_ "time/tzdata"
)

const defaultAPIURL = "http://api:8081"

// version reports the module version a `go install` build carries, or "dev".
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	case errors.Is(err, errUsage):
		// An empty message means the flag package has already written the
		// complaint and the usage block; saying it again helps nobody.
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, "puls-export:", msg)
		}
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "puls-export:", err)
		os.Exit(1)
	}
}

// errUsage marks a mistake in the command line (exit status 2) rather than a
// failed download (exit status 1). It is never printed itself: usageError
// unwraps to it so errors.Is recognises the class while the user still reads
// only the specific complaint.
var errUsage = errors.New("bad usage")

// errFlagReported is the usage error for a mistake the flag package has
// already reported in full.
var errFlagReported = &usageError{}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }
func (e *usageError) Unwrap() error { return errUsage }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// options is one parsed command line.
type options struct {
	baseURL      string
	token        string
	dataset      string
	format       string
	start, end   int64
	types        string
	sampleType   string
	activityType string
	output       string
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("puls-export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: puls-export --dataset NAME --start WHEN --end WHEN [flags]

Downloads one dataset from a PulsHealth product API as a file. The range is
half-open: --start 2026-01-01 --end 2026-02-01 is the whole of January.

`)
		fs.PrintDefaults()
		fmt.Fprint(stderr, `
Datasets: daily_metrics, samples, workouts, sleep, activity, state_of_mind.
daily_metrics needs --types, samples needs --type. The server is the authority
on both lists and reports what it accepts.

The credential selects its owner and allowed types, operations and time range.
The API is internal. Use a protected personal credential file; member selection
and shared credentials are refused.

Examples:
  puls-export --dataset sleep --start 2026-01-01 --end 2026-02-01 -o sleep.csv
  puls-export --dataset samples --type HKQuantityTypeIdentifierHeartRate \
    --format jsonl --start 2026-01-01 --end 2026-01-08 > hr.jsonl
`)
	}

	var (
		baseURL      = fs.String("url", "", "product API base URL (default $PULS_API_URL, else "+defaultAPIURL+")")
		tokenFile    = fs.String("token-file", "", "protected personal credential file (default $PULS_API_TOKEN_FILE)")
		dataset      = fs.String("dataset", "", "dataset to export (required)")
		format       = fs.String("format", "csv", "csv or jsonl")
		start        = fs.String("start", "", "range start: YYYY-MM-DD or epoch milliseconds (required)")
		end          = fs.String("end", "", "range end, exclusive: YYYY-MM-DD or epoch milliseconds (required)")
		types        = fs.String("types", "", "daily_metrics: comma-separated HealthKit identifiers")
		sampleType   = fs.String("type", "", "samples: one HealthKit identifier")
		activityType = fs.String("activity-type", "", "workouts: keep one activity type")
		zoneName     = fs.String("time-zone", "", "IANA zone the YYYY-MM-DD bounds are read in (default $PULS_TIME_ZONE, else UTC)")
		output       = fs.String("o", "", "write to this file instead of standard output")
		showVersion  = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err // -h: the usage block is already on stderr, exit 0
		}
		// The flag package has printed the complaint and the whole usage
		// block. Classify it so the exit status is 2 like every other
		// command-line mistake, without printing the message a second time.
		return errFlagReported
	}
	if *showVersion {
		fmt.Fprintln(stdout, version())
		return nil
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q", fs.Arg(0))
	}

	for _, name := range []string{"PULS_API_TOKEN", "PULS_USER_ID", "PULS_MULTI_USER"} {
		if getenv(name) != "" {
			return usagef("%s is retired; use a personal credential file without member selection", name)
		}
	}
	opts := options{
		baseURL:      firstNonEmpty(*baseURL, getenv("PULS_API_URL"), defaultAPIURL),
		dataset:      strings.TrimSpace(*dataset),
		format:       strings.TrimSpace(*format),
		types:        strings.TrimSpace(*types),
		sampleType:   strings.TrimSpace(*sampleType),
		activityType: strings.TrimSpace(*activityType),
		output:       *output,
	}
	if opts.dataset == "" {
		return usagef("--dataset is required")
	}
	var err error
	opts.token, err = readTokenFile(firstNonEmpty(*tokenFile, getenv("PULS_API_TOKEN_FILE")))
	if err != nil {
		return err
	}

	loc, err := loadTimeZone(firstNonEmpty(*zoneName, getenv("PULS_TIME_ZONE")))
	if err != nil {
		return usagef("%v", err)
	}
	if opts.start, err = parseInstant(*start, "start", loc); err != nil {
		return err
	}
	if opts.end, err = parseInstant(*end, "end", loc); err != nil {
		return err
	}
	if opts.end <= opts.start {
		return usagef("--end must be after --start")
	}

	// No client timeout: an export of a busy type is minutes of streaming, and
	// http.Client.Timeout covers the body as well as the request.
	return download(ctx, http.DefaultClient, opts, stdout)
}

// download performs the request and copies the body to the destination. The
// output file is created only once the server has answered 200, so a rejected
// request never truncates the file from a previous run.
func download(ctx context.Context, client *http.Client, opts options, stdout io.Writer) error {
	endpoint, err := exportURL(opts)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+opts.token)

	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := requestClient.Do(req)
	if err != nil {
		return fmt.Errorf("product API unreachable at %s: %w", opts.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}

	var (
		out  = stdout
		file *os.File
	)
	if opts.output != "" && opts.output != "-" {
		f, err := os.Create(opts.output)
		if err != nil {
			return err
		}
		// The safety net for an early return. After the explicit close below
		// it returns os.ErrClosed, which is discarded — closing a closed
		// *os.File cannot touch a recycled descriptor.
		defer f.Close()
		file, out = f, f
	}
	// io.Copy streams: the export is never held in memory, however long it is.
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("the download stopped early — the file is incomplete: %w", err)
	}
	if file != nil {
		// Only the file this call opened is closed — never the caller's
		// stdout — and its error is reported: a last block that failed to
		// reach the disk would otherwise be a silently short export.
		return file.Close()
	}
	return nil
}

// exportURL builds GET /v1/export for these options.
func exportURL(opts options) (string, error) {
	base, err := url.Parse(strings.TrimSpace(opts.baseURL))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return "", usagef("--url %q must be an absolute http(s) URL such as %s", opts.baseURL, defaultAPIURL)
	}
	query := url.Values{
		"dataset": {opts.dataset},
		"format":  {opts.format},
		"start":   {strconv.FormatInt(opts.start, 10)},
		"end":     {strconv.FormatInt(opts.end, 10)},
	}
	// Each filter belongs to one dataset; sending it with another would be a
	// 400 from a stricter server later, so only the relevant one goes out.
	for name, value := range map[string]string{
		"types":        opts.types,
		"type":         opts.sampleType,
		"activityType": opts.activityType,
	} {
		if value != "" {
			query.Set(name, value)
		}
	}
	endpoint := *base
	endpoint.Path = strings.TrimRight(base.Path, "/") + "/v1/export"
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

// apiError omits external response bodies, which can contain private values.
func apiError(resp *http.Response) error {
	err := fmt.Errorf("the product API answered %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		err = fmt.Errorf("%w (the personal credential is invalid, expired or revoked; authorize again)", err)
	case http.StatusForbidden:
		err = fmt.Errorf("%w (the export is outside the personal credential's permitted operation, types or time range)", err)
	}
	return err
}

// parseInstant reads a range bound: epoch milliseconds, or a YYYY-MM-DD date
// taken as midnight in loc.
func parseInstant(raw, name string, loc *time.Location) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, usagef("--%s is required (YYYY-MM-DD or epoch milliseconds)", name)
	}
	if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if ms < 0 {
			return 0, usagef("--%s must not be negative", name)
		}
		return ms, nil
	}
	day, err := time.ParseInLocation("2006-01-02", raw, loc)
	if err != nil {
		return 0, usagef("--%s %q is neither YYYY-MM-DD nor epoch milliseconds", name, raw)
	}
	return day.UnixMilli(), nil
}

// loadTimeZone resolves an IANA zone name; empty means UTC.
func loadTimeZone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("--time-zone %q is not a valid IANA time zone: %w", name, err)
	}
	return loc, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			return v
		}
	}
	return ""
}
