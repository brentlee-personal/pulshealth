package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// GET /v1/export against a real database: the two streaming datasets and one
// of the buffered ones, in both formats, over more rows than one flush window
// so the batching is exercised. Gated like the other write-fixture
// integration tests (DATABASE_URL plus PULS_API_WRITE_INTEGRATION_TESTS=1).

// exportIntegrationRows is deliberately more than exportFlushRows, so the
// response goes out in several chunks rather than one.
const exportIntegrationRows = exportFlushRows + 7

func TestIntegrationReferenceExportEndpoint(t *testing.T) {
	store, admin, ctx, cleanup := writeReferenceIntegrationStore(t)
	defer cleanup()

	loc, err := losAngelesLocation()
	if err != nil {
		t.Fatalf("losAngelesLocation: %v", err)
	}

	suffix := time.Now().UTC().UnixNano()
	unit := "count/min"
	quantityType := fmt.Sprintf("HKQuantityTypeIdentifierExportSamples%d", suffix)
	quantityID, dropQuantity := ensureSampleType(t, ctx, admin, quantityType, "quantity", &unit)
	defer dropQuantity()
	sourceID, dropSource := insertSource(t, ctx, admin, fmt.Sprintf("ExportSource%d", suffix))
	defer dropSource()

	sampleStart := fixtureDay(2081, time.UTC, suffix).Add(10 * time.Hour)
	uuids := make([]string, 0, exportIntegrationRows)
	for i := 0; i < exportIntegrationRows; i++ {
		uuid := fixtureUUID("abcdefab", suffix, i)
		uuids = append(uuids, uuid)
		at := sampleStart.Add(time.Duration(i) * time.Second)
		if _, err := admin.Exec(ctx, `
			INSERT INTO quantity_samples (uuid, type_id, start_ts, end_ts, value, source_id, user_id)
			VALUES ($1, $2, $3, $3, $4, $5, $6)`,
			uuid, quantityID, at, 60.0+float64(i), sourceID, defaultUserID,
		); err != nil {
			t.Fatalf("insert quantity sample %d: %v", i, err)
		}
	}
	defer func() {
		_, _ = admin.Exec(context.Background(),
			`DELETE FROM quantity_samples WHERE uuid = ANY($1) AND start_ts >= $2`, uuids, sampleStart)
	}()

	ringDay := time.Date(2081, 6, 1, 0, 0, 0, 0, loc).AddDate(0, 0, int(suffix%80))
	nextRingDay := ringDay.AddDate(0, 0, 1)
	if _, err := admin.Exec(ctx, `
		INSERT INTO activity_summaries (user_id, date, move_kcal, exercise_min)
		VALUES ($1, $2::date, 410, 32), ($1, $3::date, 505, 41)`,
		defaultUserID, ringDay.Format("2006-01-02"), nextRingDay.Format("2006-01-02"),
	); err != nil {
		t.Fatalf("insert activity summaries: %v", err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(),
			`DELETE FROM activity_summaries WHERE user_id = $1 AND date IN ($2::date, $3::date)`,
			defaultUserID, ringDay.Format("2006-01-02"), nextRingDay.Format("2006-01-02"))
	}()

	srv := &Server{store: referenceHTTPStore{store}, authorize: testAuthorize("export-integration", defaultUserID, quantityType), log: slog.New(slog.NewTextHandler(testWriter{t}, nil))}
	server := httptest.NewServer(srv.routes())
	defer server.Close()

	get := func(t *testing.T, query string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/export?"+query, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer export-integration")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		return resp
	}

	sampleRange := fmt.Sprintf("start=%d&end=%d",
		sampleStart.Add(-time.Minute).UnixMilli(), sampleStart.Add(time.Hour).UnixMilli())

	t.Run("samples as CSV", func(t *testing.T) {
		resp := get(t, "format=csv&dataset=samples&type="+quantityType+"&"+sampleRange)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, body %s", resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/csv; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, `attachment; filename="puls-samples-`) ||
			!strings.HasSuffix(got, `.csv"`) {
			t.Errorf("Content-Disposition = %q", got)
		}
		// Not buffered to a known length: the rows were framed as chunks as
		// the scan produced them.
		if resp.ContentLength != -1 {
			t.Errorf("ContentLength = %d, want -1", resp.ContentLength)
		}
		if len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" {
			t.Errorf("TransferEncoding = %v, want chunked", resp.TransferEncoding)
		}

		reader := csv.NewReader(resp.Body)
		header, err := reader.Read()
		if err != nil {
			t.Fatalf("reading the header row: %v", err)
		}
		if got := strings.Join(header, ","); got != "type,unit,uuid,start,end,value,label,source" {
			t.Fatalf("header = %q", got)
		}
		rows, err := reader.ReadAll()
		if err != nil {
			t.Fatalf("reading the rows: %v", err)
		}
		if len(rows) != exportIntegrationRows {
			t.Fatalf("rows = %d, want %d — the export must not be paged", len(rows), exportIntegrationRows)
		}
		first := rows[0]
		if first[0] != quantityType || first[1] != unit || first[5] != "60" {
			t.Errorf("first row = %v", first)
		}
		if !strings.HasPrefix(first[7], "ExportSource") {
			t.Errorf("source cell = %q", first[7])
		}
		// Ordered by start time, so the values run 60, 61, 62, ...
		last := rows[len(rows)-1]
		if want := strconv.Itoa(60 + exportIntegrationRows - 1); last[5] != want {
			t.Errorf("last value = %q, want %q", last[5], want)
		}
	})

	t.Run("samples as JSONL", func(t *testing.T) {
		resp := get(t, "format=jsonl&dataset=samples&type="+quantityType+"&"+sampleRange)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, body %s", resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != "application/x-ndjson" {
			t.Errorf("Content-Type = %q", got)
		}

		lines := 0
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			var row map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
				t.Fatalf("line %d is not JSON: %v", lines+1, err)
			}
			if row["type"] != quantityType || row["unit"] != unit {
				t.Fatalf("line %d = %v", lines+1, row)
			}
			if want := 60.0 + float64(lines); row["value"] != want {
				t.Fatalf("line %d value = %v, want %v", lines+1, row["value"], want)
			}
			lines++
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("reading the body: %v", err)
		}
		if lines != exportIntegrationRows {
			t.Fatalf("lines = %d, want %d and no header line", lines, exportIntegrationRows)
		}
	})

	t.Run("activity rings as CSV and JSONL", func(t *testing.T) {
		ringRange := fmt.Sprintf("start=%d&end=%d",
			ringDay.Add(23*time.Hour+30*time.Minute).UnixMilli(),
			nextRingDay.Add(time.Hour).UnixMilli())

		resp := get(t, "format=csv&dataset=activity&"+ringRange)
		defer resp.Body.Close()
		records, err := csv.NewReader(resp.Body).ReadAll()
		if err != nil {
			t.Fatalf("reading the CSV: %v", err)
		}
		if len(records) != 3 {
			t.Fatalf("records = %#v, want a header and both touched local days", records)
		}
		if got := strings.Join(records[0], ","); !strings.HasPrefix(got, "date,moveKcal,moveGoalKcal,exerciseMin") {
			t.Errorf("header = %q", got)
		}
		if records[1][0] != ringDay.Format("2006-01-02") || records[1][1] != "410" {
			t.Errorf("first day = %v", records[1])
		}
		// A goal nobody recorded is an empty cell, not a zero.
		if records[1][2] != "" {
			t.Errorf("moveGoalKcal = %q, want an empty cell for NULL", records[1][2])
		}

		jsonlResp := get(t, "format=jsonl&dataset=activity&"+ringRange)
		defer jsonlResp.Body.Close()
		body, err := io.ReadAll(jsonlResp.Body)
		if err != nil {
			t.Fatalf("reading the JSONL: %v", err)
		}
		lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("lines = %#v, want one object per day", lines)
		}
		var day map[string]any
		if err := json.Unmarshal([]byte(lines[1]), &day); err != nil {
			t.Fatalf("line 2 is not JSON: %v", err)
		}
		if day["date"] != nextRingDay.Format("2006-01-02") || day["moveKcal"] != 505.0 || day["exerciseMin"] != 41.0 {
			t.Fatalf("line 2 = %v", day)
		}
		// A NULL stays an explicit null so every line has the same keys.
		if value, ok := day["moveGoalKcal"]; !ok || value != nil {
			t.Fatalf("moveGoalKcal = %v (present %v), want an explicit null", value, ok)
		}
	})

	t.Run("the range caps reject an over-long export", func(t *testing.T) {
		cases := []struct {
			name  string
			query string
			want  string
		}{
			{
				"samples over 31 days",
				fmt.Sprintf("format=csv&dataset=samples&type=%s&start=%d&end=%d",
					quantityType, sampleStart.UnixMilli(), sampleStart.Add(32*24*time.Hour).UnixMilli()),
				"31 days",
			},
			{
				"activity over 366 days",
				fmt.Sprintf("format=jsonl&dataset=activity&start=%d&end=%d",
					ringDay.UnixMilli(), ringDay.Add(367*24*time.Hour).UnixMilli()),
				"366 days",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				resp := get(t, tc.query)
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("status = %d, body %s", resp.StatusCode, body)
				}
				var body map[string]string
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("body is not the usual error JSON: %v", err)
				}
				if !strings.Contains(body["error"], tc.want) {
					t.Fatalf("error = %q, want it to mention %q", body["error"], tc.want)
				}
			})
		}
	})

	t.Run("an unknown type is a 400 before the download starts", func(t *testing.T) {
		resp := get(t, "format=csv&dataset=samples&type=HKQuantityTypeIdentifierNoSuchThing&"+sampleRange)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want the error JSON", got)
		}
	})
}
