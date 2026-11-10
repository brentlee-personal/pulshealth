package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Bulk export: GET /v1/export streams one dataset as CSV or JSONL instead of
// a JSON document, so a whole range lands in a spreadsheet, a notebook, or a
// chat attachment in one request.
//
// Every row is written to the socket as it is produced — the response has no
// Content-Length, so net/http frames it with Transfer-Encoding: chunked and
// the file starts arriving before the query has finished. Nothing accumulates
// in a buffer beyond one flush window: the header row is pushed on its own,
// and after that every exportFlushRows rows.
//
// The two unbounded datasets (raw samples, workouts) read through the store's
// streaming variants (StreamSamples, StreamWorkouts) and hold one row at a
// time. The four day-grained ones (daily metrics, activity rings, sleep,
// State of Mind) reuse the same store calls their JSON endpoints use: their
// row counts are already bounded by the 366-day cap, and reading them up
// front means a bad request is still answered with a clean 400 instead of a
// truncated download.

const (
	// Rows written between pushes to the client. Small enough that a slow
	// query still shows progress, large enough that a million-row export is
	// not a million syscalls.
	exportFlushRows = 256
	// The range cap for every dataset but samples, which keeps the 31 days
	// GET /v1/samples enforces (a busy type runs to hundreds of thousands of
	// rows a month). 366 is what /v1/sleep/daily and /v1/state-of-mind
	// already apply; for daily metrics, activity rings and workouts it is
	// deliberately *stricter* than the JSON endpoints, which have no cap at
	// all — a page is bounded by its page size, a file only by its range.
	//
	// This measures the instant span. The day-grained stores additionally
	// reject a range touching more than 366 local calendar days, so a span
	// that squeaks under here can still be a 400 from the store — before any
	// bytes either way, with its own message.
	maxExportRange = 366 * 24 * time.Hour
	// How many exports may be in flight at once. samples and workouts hold a
	// pooled database connection for as long as the client takes to read the
	// file — minutes on a slow link — and the pool is small (pgxpool defaults
	// to max(4, NumCPU)), so without a bound a few stalled downloads starve
	// every other endpoint. Over the bound is an immediate 503 with
	// Retry-After, never a queue: waiting would hold the very connection the
	// limit exists to protect.
	maxConcurrentExports = 2
	// How long one write to the client may block. A client that stops
	// reading fills the socket buffer and the next write never returns;
	// without a deadline it held its slot (and, for samples and workouts, a
	// pooled connection) until the process restarted. Renewed before every
	// write, so a slow query between rows is never mistaken for a stalled
	// client.
	exportWriteTimeout = 60 * time.Second
	// The absolute ceiling on one export, for a client that keeps reading
	// just fast enough to beat exportWriteTimeout. Far beyond what the range
	// caps need on any link that can carry the file at all.
	exportMaxDuration = 30 * time.Minute
)

// exportDatasets names every value of the dataset parameter, in the order
// the error message lists them.
var exportDatasets = []string{"daily_metrics", "samples", "workouts", "sleep", "activity", "state_of_mind"}

// exportDataset is a resolved GET /v1/export request: what the rows are
// called and how to produce them. Everything that can fail with a 400 has
// already happened by the time one exists, so the handler can commit to a
// 200 before it writes the first byte.
type exportDataset struct {
	Name string
	// The range the filename carries, in epoch milliseconds.
	StartMS, EndMS int64
	// Field names, in order. The CSV header row and the JSONL object keys
	// are both this list, so the two formats cannot drift apart.
	Columns []string
	// Rows calls emit once per row with values lined up with Columns.
	Rows func(ctx context.Context, emit func(values []any) error) error
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if !requireAPIPermission(w, r, "data:export", nil, nil, nil) {
		return
	}
	// The client's own context, kept to tell a hang-up from the ceiling.
	client := r.Context()
	ctx, cancel := context.WithTimeout(client, exportMaxDuration)
	defer cancel()
	// Shutdown ends it too, once its grace has run out (see run): like the
	// ceiling, that is not the client's doing, so a stream cut short by it
	// is aborted below, never closed as if the file were complete.
	stopAtShutdown := context.AfterFunc(s.lifetimeContext(), cancel)
	defer stopAtShutdown()
	r = r.WithContext(ctx)

	format, err := exportFormatFor(r.URL.Query().Get("format"))
	if err != nil {
		s.writeStoreError(w, err, "export")
		return
	}
	dataset, err := s.exportDatasetFor(r)
	if err != nil {
		s.writeStoreError(w, err, "export")
		return
	}
	check, ok := ctx.Value(readRecheckKey{}).(func(context.Context) error)
	if !ok {
		s.writeStoreError(w, errAPICredential, "export")
		return
	}
	if err := check(ctx); err != nil {
		s.writeStoreError(w, err, "export")
		return
	}

	// The slot is taken here — after everything that can 400, before the
	// first byte — and held to the last one. A rejected request must not
	// consume one, or a burst of malformed requests would 503 the real ones;
	// what the limit protects is the connection a download holds while the
	// client reads, and only a request that gets this far ever holds one.
	if !s.acquireExport() {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": fmt.Sprintf("at most %d exports may run at once; retry shortly", maxConcurrentExports),
		})
		return
	}
	defer s.releaseExport()

	w.Header().Set("Content-Type", format.contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q",
		fmt.Sprintf("puls-%s-%d-%d.%s", dataset.Name, dataset.StartMS, dataset.EndMS, format.extension)))
	// A downloaded file is never a page to render.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// No Content-Length, so the response is chunked and the rows go out as
	// they are read.
	w.WriteHeader(http.StatusOK)

	out := &exportWriter{ctx: ctx, w: w, control: http.NewResponseController(w)}
	encoder := format.newEncoder(out)
	rows := 0
	err = encoder.Begin(dataset.Columns)
	if err == nil {
		// Push the header row (CSV) and the status line on their own, before
		// the first row is read: the download then starts — and a proxy
		// commits to the 200 — even when the scan is slow to produce a row.
		err = flush(encoder, out)
	}
	if err == nil {
		err = dataset.Rows(ctx, func(values []any) error {
			if rows%exportFlushRows == 0 {
				if err := check(ctx); err != nil {
					return err
				}
			}
			if err := encoder.Row(values); err != nil {
				return err
			}
			rows++
			if rows%exportFlushRows == 0 {
				return flush(encoder, out)
			}
			return nil
		})
	}
	if err == nil {
		err = check(ctx)
	}
	if err == nil {
		err = flush(encoder, out)
	}
	if err != nil {
		if client.Err() != nil {
			// The client hung up part-way through — a Ctrl-C on a long
			// download, which is a normal thing to do. There is nobody left
			// to signal, and nothing here went wrong. err is logged anyway:
			// a genuine store failure can race a disconnect, and this is the
			// only place it would ever be seen.
			s.log.Info("export abandoned by the client",
				"dataset", dataset.Name, "rows", rows, "err", err.Error())
			return
		}
		// The 200 and some rows are already on the wire (a write that timed
		// out, the ceiling and a shutdown past its grace end up here too),
		// so the only honest signal left is an incomplete transfer: abort
		// the response rather than close the chunked body cleanly on a
		// short file. net/http recognises ErrAbortHandler, drops the
		// connection without a stack trace, and the client's read fails.
		//
		// A test that drives this handler through httptest.NewRecorder() with
		// a store that fails *after* the first row will see that panic
		// escape, because there is no net/http in the way to recover it. Use
		// httptest.NewServer for a mid-stream failure, as
		// TestExportStreamsChunkedAndAbortsOnAMidStreamFailure does.
		s.log.Error("export failed mid-stream", "dataset", dataset.Name, "rows", rows, "err", err.Error())
		panic(http.ErrAbortHandler)
	}
}

// acquireExport takes one of the maxConcurrentExports slots, or reports that
// they are all busy. The channel is created on first use so a Server built as
// a struct literal — which every test does — needs no constructor.
func (s *Server) acquireExport() bool {
	s.exportOnce.Do(func() { s.exportSlots = make(chan struct{}, maxConcurrentExports) })
	select {
	case s.exportSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseExport() { <-s.exportSlots }

// flush empties the encoder's buffer into the response and pushes the chunk
// to the client.
func flush(encoder exportEncoder, out *exportWriter) error {
	if err := encoder.Flush(); err != nil {
		return err
	}
	return out.Flush()
}

// exportWriter is the response as the export writes it: every write and
// flush first checks the export's ceiling (ctx), then gives itself
// exportWriteTimeout to complete. A ResponseWriter that cannot set a
// deadline or flush (a test recorder, a wrapper) is not an error: the bytes
// are written either way.
type exportWriter struct {
	ctx     context.Context
	w       io.Writer
	control *http.ResponseController
}

func (e *exportWriter) arm() error {
	if err := e.ctx.Err(); err != nil {
		return err
	}
	err := e.control.SetWriteDeadline(time.Now().Add(exportWriteTimeout))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (e *exportWriter) Write(p []byte) (int, error) {
	if err := e.arm(); err != nil {
		return 0, err
	}
	return e.w.Write(p)
}

func (e *exportWriter) Flush() error {
	if err := e.arm(); err != nil {
		return err
	}
	if err := e.control.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// exportDatasetFor parses and validates the dataset parameter and everything
// that dataset needs, running the query for the bounded ones.
func (s *Server) exportDatasetFor(r *http.Request) (*exportDataset, error) {
	name := strings.TrimSpace(r.URL.Query().Get("dataset"))
	switch name {
	case "":
		return nil, badRequestf("missing dataset: use one of %s", strings.Join(exportDatasets, ", "))
	case "daily_metrics":
		return s.exportDailyMetrics(r)
	case "samples":
		return s.exportSamples(r)
	case "workouts":
		return s.exportWorkouts(r)
	case "sleep":
		return s.exportSleep(r)
	case "activity":
		return s.exportActivity(r)
	case "state_of_mind":
		return s.exportStateOfMind(r)
	}
	return nil, badRequestf("invalid dataset %q: use one of %s", name, strings.Join(exportDatasets, ", "))
}

func (s *Server) exportDailyMetrics(r *http.Request) (*exportDataset, error) {
	// No paging: the whole capped range in one query (Limit zero).
	filters, err := dailyTypesAndRange(r)
	if err != nil {
		return nil, asBadRequest(err)
	}
	if err := capExportRange(filters.Start, filters.End, maxExportRange); err != nil {
		return nil, err
	}
	if err := apiPermission(r.Context(), "data:export", filters.Types, &filters.Start, &filters.End); err != nil {
		return nil, err
	}
	metrics, err := s.store.DailyMetrics(r.Context(), s.requestUser(r), filters)
	if err != nil {
		return nil, err
	}
	return &exportDataset{
		Name:    "daily_metrics",
		StartMS: filters.Start.UnixMilli(),
		EndMS:   filters.End.UnixMilli(),
		// The JSON endpoint nests days under their metric; a flat file
		// repeats the identifier and unit on every row instead.
		Columns: []string{"identifier", "unit", "date", "value"},
		Rows: func(_ context.Context, emit func([]any) error) error {
			for _, metric := range metrics {
				for _, day := range metric.Days {
					if err := emit([]any{metric.Identifier, metric.Unit, day.Date, day.Value}); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}, nil
}

func (s *Server) exportSamples(r *http.Request) (*exportDataset, error) {
	// Only the type and the range: limit and offset are the JSON endpoint's
	// paging and mean nothing to an export, so they are neither read nor
	// validated here — a zero Limit is "the whole range" downstream.
	filters, err := sampleTypeAndRange(r)
	if err != nil {
		return nil, asBadRequest(err)
	}
	if err := apiPermission(r.Context(), "data:export", []string{filters.Type}, &filters.Start, &filters.End); err != nil {
		return nil, err
	}
	// Resolve the type before the response starts, so an identifier the
	// database has never seen is a 400 and not a one-line file.
	meta, err := s.store.SampleType(r.Context(), filters.Type)
	if err != nil {
		return nil, err
	}
	user := s.requestUser(r)
	return &exportDataset{
		Name:    "samples",
		StartMS: filters.Start.UnixMilli(),
		EndMS:   filters.End.UnixMilli(),
		// type and unit live in the page envelope of the JSON endpoint; a
		// flat file has to carry them per row to stay self-describing.
		Columns: []string{"type", "unit", "uuid", "start", "end", "value", "label", "source"},
		Rows: func(ctx context.Context, emit func([]any) error) error {
			return s.store.StreamSamples(ctx, user, meta, filters, func(sample Sample) error {
				return emit([]any{
					meta.Type, meta.Unit, sample.UUID, sample.Start, sample.End,
					sample.Value, sample.Label, sample.Source,
				})
			})
		},
	}, nil
}

func (s *Server) exportWorkouts(r *http.Request) (*exportDataset, error) {
	start, end, err := parseRange(r)
	if err != nil {
		return nil, asBadRequest(err)
	}
	if err := capExportRange(start, end, maxExportRange); err != nil {
		return nil, err
	}
	if err := apiPermission(r.Context(), "data:export", []string{"HKWorkoutTypeIdentifier"}, &start, &end); err != nil {
		return nil, err
	}
	// Unlike GET /v1/workouts the range is required and the whole of it is
	// returned: an export is bounded by its range, not by a page size.
	filters := WorkoutFilters{Start: &start, End: &end, ActivityType: r.URL.Query().Get("activityType")}
	user := s.requestUser(r)
	return &exportDataset{
		Name:    "workouts",
		StartMS: start.UnixMilli(),
		EndMS:   end.UnixMilli(),
		Columns: []string{
			"uuid", "activityType", "start", "end",
			"durationS", "distanceM", "energyKcal", "hasRoute", "availableMetrics",
		},
		Rows: func(ctx context.Context, emit func([]any) error) error {
			return s.store.StreamWorkouts(ctx, user, filters, func(workout WorkoutSummary) error {
				return emit([]any{
					workout.UUID, workout.ActivityType, workout.Start, workout.End,
					workout.DurationS, workout.DistanceM, workout.EnergyKcal,
					workout.HasRoute, workout.AvailableMetrics,
				})
			})
		},
	}, nil
}

func (s *Server) exportSleep(r *http.Request) (*exportDataset, error) {
	start, end, err := parseRange(r)
	if err != nil {
		return nil, asBadRequest(err)
	}
	if err := capExportRange(start, end, maxExportRange); err != nil {
		return nil, err
	}
	if err := apiPermission(r.Context(), "data:export", []string{sleepTypeIdentifier}, &start, &end); err != nil {
		return nil, err
	}
	nights, err := s.store.SleepDaily(r.Context(), s.requestUser(r), start, end)
	if err != nil {
		return nil, err
	}
	return &exportDataset{
		Name:    "sleep",
		StartMS: start.UnixMilli(),
		EndMS:   end.UnixMilli(),
		// The endpoint nests the stage minutes; a flat file names them with
		// the path they have in the JSON.
		Columns: []string{
			"date", "start", "end", "inBedMinutes", "asleepMinutes",
			"stages.core", "stages.deep", "stages.rem", "stages.unspecified", "stages.awake",
			"sources",
		},
		Rows: func(_ context.Context, emit func([]any) error) error {
			for _, night := range nights {
				if err := emit([]any{
					night.Date, night.Start, night.End, night.InBedMinutes, night.AsleepMinutes,
					night.Stages.Core, night.Stages.Deep, night.Stages.REM,
					night.Stages.Unspecified, night.Stages.Awake, night.Sources,
				}); err != nil {
					return err
				}
			}
			return nil
		},
	}, nil
}

func (s *Server) exportActivity(r *http.Request) (*exportDataset, error) {
	start, end, err := parseRange(r)
	if err != nil {
		return nil, asBadRequest(err)
	}
	if err := capExportRange(start, end, maxExportRange); err != nil {
		return nil, err
	}
	if err := apiPermission(r.Context(), "data:export", []string{"HKActivitySummaryTypeIdentifier"}, &start, &end); err != nil {
		return nil, err
	}
	days, err := s.store.ActivitySummary(r.Context(), s.requestUser(r), start, end)
	if err != nil {
		return nil, err
	}
	return &exportDataset{
		Name:    "activity",
		StartMS: start.UnixMilli(),
		EndMS:   end.UnixMilli(),
		Columns: []string{
			"date", "moveKcal", "moveGoalKcal", "exerciseMin", "exerciseGoalMin",
			"standHours", "standGoalHours", "moveMode", "moveTimeMin", "moveTimeGoalMin",
		},
		Rows: func(_ context.Context, emit func([]any) error) error {
			for _, day := range days {
				if err := emit([]any{
					day.Date, day.MoveKcal, day.MoveGoalKcal, day.ExerciseMin, day.ExerciseGoalMin,
					day.StandHours, day.StandGoalHours, day.MoveMode, day.MoveTimeMin, day.MoveTimeGoalMin,
				}); err != nil {
					return err
				}
			}
			return nil
		},
	}, nil
}

func (s *Server) exportStateOfMind(r *http.Request) (*exportDataset, error) {
	start, end, err := parseRange(r)
	if err != nil {
		return nil, asBadRequest(err)
	}
	if err := capExportRange(start, end, maxExportRange); err != nil {
		return nil, err
	}
	if err := apiPermission(r.Context(), "data:export", []string{"HKDataTypeIdentifierStateOfMind"}, &start, &end); err != nil {
		return nil, err
	}
	entries, err := s.store.StateOfMind(r.Context(), s.requestUser(r), start, end)
	if err != nil {
		return nil, err
	}
	return &exportDataset{
		Name:    "state_of_mind",
		StartMS: start.UnixMilli(),
		EndMS:   end.UnixMilli(),
		Columns: []string{
			"uuid", "date", "timestamp", "kind", "valence", "valenceClassification",
			"labels", "associations",
		},
		Rows: func(_ context.Context, emit func([]any) error) error {
			for _, entry := range entries {
				if err := emit([]any{
					entry.UUID, entry.Date, entry.Timestamp, entry.Kind,
					entry.Valence, entry.ValenceClassification, entry.Labels, entry.Associations,
				}); err != nil {
					return err
				}
			}
			return nil
		},
	}, nil
}

// capExportRange rejects a range longer than max. The instant span is what
// is measured, the same test GET /v1/samples applies, so the answer does not
// depend on where the calendar days fall.
func capExportRange(start, end time.Time, max time.Duration) error {
	if end.Sub(start) > max {
		return badRequestf("range must not exceed %d days", int(max.Hours()/24))
	}
	return nil
}

// asBadRequest marks a parameter error as the caller's fault, so
// writeStoreError answers 400 rather than logging a 500.
func asBadRequest(err error) error {
	if err == nil {
		return nil
	}
	var reqErr *requestError
	if errors.As(err, &reqErr) {
		return err
	}
	return &requestError{msg: err.Error()}
}

// exportFormat is one output format of GET /v1/export.
type exportFormat struct {
	name        string
	extension   string
	contentType string
	newEncoder  func(io.Writer) exportEncoder
}

func exportFormatFor(name string) (exportFormat, error) {
	switch strings.TrimSpace(name) {
	case "":
		return exportFormat{}, badRequestf("missing format: use csv or jsonl")
	case "csv":
		return exportFormat{
			name:        "csv",
			extension:   "csv",
			contentType: "text/csv; charset=utf-8",
			newEncoder:  func(w io.Writer) exportEncoder { return &csvEncoder{w: csv.NewWriter(w)} },
		}, nil
	case "jsonl":
		return exportFormat{
			name:      "jsonl",
			extension: "jsonl",
			// The same media type the sync protocol uses for NDJSON.
			contentType: "application/x-ndjson",
			newEncoder:  func(w io.Writer) exportEncoder { return &jsonlEncoder{w: bufio.NewWriter(w)} },
		}, nil
	}
	return exportFormat{}, badRequestf("invalid format %q: use csv or jsonl", name)
}

// exportEncoder writes rows in one format. Begin runs once before any row,
// Flush pushes the encoder's own buffer into the ResponseWriter.
type exportEncoder interface {
	Begin(columns []string) error
	Row(values []any) error
	Flush() error
}

// csvEncoder writes the column names as the header row and then one record
// per row.
type csvEncoder struct {
	w       *csv.Writer
	columns int
	cells   []string
}

func (e *csvEncoder) Begin(columns []string) error {
	e.columns = len(columns)
	e.cells = make([]string, 0, len(columns))
	return e.w.Write(columns)
}

func (e *csvEncoder) Row(values []any) error {
	// The same check jsonlEncoder makes: a dataset whose values drifted from
	// its Columns must fail, not ship a ragged file that reads as CSV.
	if len(values) != e.columns {
		return fmt.Errorf("export row has %d values for %d columns", len(values), e.columns)
	}
	e.cells = e.cells[:0]
	for _, value := range values {
		e.cells = append(e.cells, csvCell(value))
	}
	return e.w.Write(e.cells)
}

func (e *csvEncoder) Flush() error {
	e.w.Flush()
	return e.w.Error()
}

// csvCell renders one value as a CSV cell: a null as the empty cell, a float
// without an exponent or trailing zeros, a list comma-joined inside the
// (then quoted) cell.
func csvCell(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case *string:
		if v == nil {
			return ""
		}
		return *v
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case *int:
		if v == nil {
			return ""
		}
		return strconv.Itoa(*v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case *float64:
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'f', -1, 64)
	case []string:
		return strings.Join(v, ",")
	}
	return fmt.Sprint(value)
}

// jsonlEncoder writes one JSON object per line, keys in column order, so a
// line reads like the object the JSON endpoint would have returned. JSONL has
// no header line, so Begin only prepares the keys.
type jsonlEncoder struct {
	w *bufio.Writer
	// The column names already quoted as JSON strings, so a million-row
	// export encodes each key once rather than once per row.
	keys [][]byte
	line []byte
}

func (e *jsonlEncoder) Begin(columns []string) error {
	e.keys = make([][]byte, len(columns))
	for i, column := range columns {
		key, err := json.Marshal(column)
		if err != nil {
			return err
		}
		e.keys[i] = key
	}
	return nil
}

func (e *jsonlEncoder) Row(values []any) error {
	if len(values) != len(e.keys) {
		return fmt.Errorf("export row has %d values for %d columns", len(values), len(e.keys))
	}
	e.line = append(e.line[:0], '{')
	for i, value := range values {
		if i > 0 {
			e.line = append(e.line, ',')
		}
		encoded, err := json.Marshal(jsonValue(value))
		if err != nil {
			return err
		}
		e.line = append(e.line, e.keys[i]...)
		e.line = append(e.line, ':')
		e.line = append(e.line, encoded...)
	}
	e.line = append(e.line, '}', '\n')
	_, err := e.w.Write(e.line)
	return err
}

func (e *jsonlEncoder) Flush() error { return e.w.Flush() }

// jsonValue keeps an absent list an empty array rather than null, so every
// line of a dataset has the same shape.
func jsonValue(value any) any {
	if list, ok := value.([]string); ok && list == nil {
		return []string{}
	}
	return value
}
