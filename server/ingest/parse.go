package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gowebpki/jcs"
)

// Wire protocol types. Timestamps arrive as epoch milliseconds (JSON numbers,
// possibly fractional). Newer clients also send temporalContext fields so the
// server can reconstruct the source local wall time without changing the
// canonical UTC instant columns.

// BatchHeader is the first NDJSON line of every batch.
type BatchHeader struct {
	BatchID  string `json:"batchID"`
	DeviceID string `json:"deviceID"`
	// UserID is not sent in the body: the handler fills it from the verified
	// principal before InsertBatch runs. Shared-token mode retains legacy scoping.
	UserID string `json:"-"`
	// DeviceTokenID is the device_tokens row the request authenticated with,
	// filled by the handler from the auth principal; 0 for the shared token.
	DeviceTokenID int64 `json:"-"`
	// WakeID/Trigger are not sent in the body either: the handler fills them from
	// the X-Wake-ID / X-Wake-Trigger headers (the iOS wake that produced this
	// upload). Both optional — absent for older clients, curl, and work outside a
	// wake (e.g. reconciliation).
	WakeID               string   `json:"-"`
	Trigger              string   `json:"-"`
	Type                 string   `json:"type"`
	Reason               string   `json:"reason"`
	ExportedAt           float64  `json:"exportedAt"`
	SampleCount          int      `json:"sampleCount"`
	DeletionCount        int      `json:"deletionCount"`
	RouteCount           int      `json:"routeCount"`           // optional; 0 for old clients
	AggregateCount       int      `json:"aggregateCount"`       // optional; 0 for old clients
	SeriesCount          int      `json:"seriesCount"`          // optional; 0 for old clients
	ActivitySummaryCount int      `json:"activitySummaryCount"` // optional; 0 for old clients
	ProfileCount         int      `json:"profileCount"`         // optional; 0 or 1
	SourceObservations   []string `json:"sourceObservations,omitempty"`
	// SchemaVersion is the wire-format version the client encoded this batch
	// with (PROTO-1). Nil (absent or null) is a client that predates
	// versioning and is read with version-1 semantics; any value outside
	// supportedProtocolVersions fails ParseBatch with a *ProtocolVersionError
	// before a single data line is read.
	SchemaVersion *int `json:"schemaVersion"`
	// ClientVersion is free text identifying the sender, e.g. "0.1.0 (1)".
	// Optional client metadata; never stored or copied to operational batch logs.
	ClientVersion string `json:"clientVersion"`
	// Optional current device zone; used only to initialize new hosted accounts.
	TimeZoneID string `json:"timeZoneID"`
}

type TemporalContext struct {
	TimeZoneID       string `json:"timeZoneID"`
	UTCOffsetSeconds int32  `json:"utcOffsetSeconds"`
	Source           string `json:"source"`
	Confidence       string `json:"confidence"`
	TZDBVersion      string `json:"tzdbVersion"`
}

// WorkoutStat is one quantity type's min/avg/max/sum over a workout (or one of
// its activities), in the type's canonical unit. Fields are absent when the
// aggregation style doesn't produce them (sum for cumulative, avg/min/max for
// discrete).
type WorkoutStat struct {
	Min *float64 `json:"min"`
	Avg *float64 `json:"avg"`
	Max *float64 `json:"max"`
	Sum *float64 `json:"sum"`
}

// WorkoutEvent is one pause/resume/lap/segment/marker event. End is nil for
// point events, set for spans.
type WorkoutEvent struct {
	Type         string           `json:"type"`
	Start        float64          `json:"start"` // epoch ms
	End          *float64         `json:"end"`   // epoch ms
	StartContext *TemporalContext `json:"startContext"`
	EndContext   *TemporalContext `json:"endContext"`
	Metadata     map[string]any   `json:"metadata"`
}

// WorkoutActivity is one sub-activity of a multi-sport / interval workout.
type WorkoutActivity struct {
	ActivityType string                 `json:"activityType"`
	Start        float64                `json:"start"` // epoch ms
	End          *float64               `json:"end"`   // epoch ms
	StartContext *TemporalContext       `json:"startContext"`
	EndContext   *TemporalContext       `json:"endContext"`
	Duration     float64                `json:"duration"`
	Statistics   map[string]WorkoutStat `json:"statistics"`
}

// Workout carries workout-specific fields for kind=workout samples.
type Workout struct {
	ActivityType        string                 `json:"activityType"`
	Duration            float64                `json:"duration"`
	TotalEnergyKcal     *float64               `json:"totalEnergyKcal"`
	TotalDistanceMeters *float64               `json:"totalDistanceMeters"`
	Statistics          map[string]float64     `json:"statistics"`
	StatisticsDetail    map[string]WorkoutStat `json:"statisticsDetail"`
	Events              []WorkoutEvent         `json:"events"`
	Activities          []WorkoutActivity      `json:"activities"`
}

// Heartbeat is one [secondsSinceSeriesStart, precededByGap] pair from a
// kind=heartbeatSeries sample.
type Heartbeat struct {
	Seconds       float64
	PrecededByGap bool
}

func (h *Heartbeat) UnmarshalJSON(data []byte) error {
	pair := []any{&h.Seconds, &h.PrecededByGap}
	if err := json.Unmarshal(data, &pair); err != nil {
		return err
	}
	if len(pair) != 2 {
		return fmt.Errorf("heartbeat must be a [seconds, precededByGap] pair, got %d elements", len(pair))
	}
	return nil
}

func (h Heartbeat) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]any{h.Seconds, h.PrecededByGap})
}

// ECG carries electrocardiogram fields for kind=ecg samples.
type ECG struct {
	Classification      string    `json:"classification"`
	AverageHeartRateBpm *float64  `json:"averageHeartRateBpm"`
	SamplingFrequencyHz *float64  `json:"samplingFrequencyHz"`
	SymptomsStatus      string    `json:"symptomsStatus"`
	VoltagesUV          []float32 `json:"voltagesUV"`
}

// StateOfMind carries mood fields for kind=stateOfMind samples.
type StateOfMind struct {
	Kind                  string   `json:"kind"`
	Valence               float64  `json:"valence"`
	ValenceClassification string   `json:"valenceClassification"`
	Labels                []string `json:"labels"`
	Associations          []string `json:"associations"`
}

// MedicationDose carries dose-event fields for kind=medicationDose samples.
type MedicationDose struct {
	MedicationID       *string          `json:"medicationID"`
	Medication         *string          `json:"medication"`
	Status             string           `json:"status"`
	ScheduledAt        *float64         `json:"scheduledAt"` // epoch ms
	ScheduledAtContext *TemporalContext `json:"scheduledAtContext"`
	DoseQuantity       *float64         `json:"doseQuantity"`
	DoseUnit           *string          `json:"doseUnit"`
}

// Sample is one HealthKit sample line.
type Sample struct {
	UUID           string           `json:"uuid"`
	Type           string           `json:"type"`
	Kind           string           `json:"kind"`
	Start          float64          `json:"start"`
	End            float64          `json:"end"`
	StartContext   *TemporalContext `json:"startContext"`
	EndContext     *TemporalContext `json:"endContext"`
	Value          *float64         `json:"value"`
	Unit           *string          `json:"unit"`
	Category       *int16           `json:"category"`
	SourceName     *string          `json:"sourceName"`
	SourceBundleID *string          `json:"sourceBundleID"`
	SourceVersion  *string          `json:"sourceVersion"`
	Device         *string          `json:"device"`
	Metadata       map[string]any   `json:"metadata"`
	Workout        *Workout         `json:"workout"`
	// nil means absent; an empty heartbeat series arrives as [].
	Heartbeats     []Heartbeat     `json:"heartbeats"`
	ECG            *ECG            `json:"ecg"`
	StateOfMind    *StateOfMind    `json:"stateOfMind"`
	MedicationDose *MedicationDose `json:"medicationDose"`
}

// RoutePoint is one GPS fix inside a route line.
type RoutePoint struct {
	T               float64          `json:"t"` // epoch ms
	TemporalContext *TemporalContext `json:"temporalContext"`
	Lat             float64          `json:"lat"`
	Lon             float64          `json:"lon"`
	Alt             *float64         `json:"alt"`
	HAcc            *float64         `json:"hAcc"`
	VAcc            *float64         `json:"vAcc"`
	Speed           *float64         `json:"speed"`
	Course          *float64         `json:"course"`
}

// RouteLine is one workout-route line. A single workout's route may be split
// across several lines by the client.
type RouteLine struct {
	Route struct {
		WorkoutUUID string       `json:"workoutUUID"`
		Points      []RoutePoint `json:"points"`
	} `json:"route"`
}

// SeriesPoint is one datum inside a workout-series line (canonical unit).
type SeriesPoint struct {
	T               float64          `json:"t"` // epoch ms
	TemporalContext *TemporalContext `json:"temporalContext"`
	Value           float64          `json:"value"`
}

// SeriesLine is one intra-workout quantity-series line. A single (workout, type)
// stream may be split across several lines by the client.
type SeriesLine struct {
	Series struct {
		WorkoutUUID string        `json:"workoutUUID"`
		Type        string        `json:"type"`
		Unit        *string       `json:"unit"`
		Points      []SeriesPoint `json:"points"`
	} `json:"series"`
}

// ProfileSnapshot is the complete replaceable set of user profile fields.
// Null or omitted inner fields deliberately clear stored values.
type ProfileSnapshot struct {
	Name          *string  `json:"name"`
	Email         *string  `json:"email"`
	DateOfBirth   *float64 `json:"dateOfBirth"` // epoch ms
	BiologicalSex *string  `json:"biologicalSex"`
	CapturedAt    *float64 `json:"capturedAt,omitempty"`
}

// ProfileLine carries a required, non-null profile wrapper. A pointer lets the
// parser distinguish {"profile":{}} (intentional clear-all) from a missing or
// null outer wrapper, which is malformed rather than a profile update.
type ProfileLine struct {
	Profile *ProfileSnapshot `json:"profile"`
}

// AggregateLine is one on-device statistics bucket
// (HKStatisticsCollectionQuery) line. Buckets are recomputed and re-sent by
// the client, so the store upserts them rather than insert-only.
type AggregateLine struct {
	Aggregate struct {
		Type               string           `json:"type"`
		Func               string           `json:"func"`
		IntervalValue      int              `json:"intervalValue"`
		IntervalUnit       string           `json:"intervalUnit"`
		DeviceFilter       string           `json:"deviceFilter"`
		BucketStart        float64          `json:"bucketStart"` // epoch ms
		BucketEnd          float64          `json:"bucketEnd"`   // epoch ms
		BucketStartContext *TemporalContext `json:"bucketStartContext"`
		BucketEndContext   *TemporalContext `json:"bucketEndContext"`
		// nil means an explicit JSON null: the bucket is empty and any
		// previously stored value must be overwritten with NULL.
		Value *float64 `json:"value"`
		Unit  *string  `json:"unit"`
	} `json:"aggregate"`
}

// ActivitySummaryLine is one daily activity-summary (HKActivitySummary) line.
// Like aggregates, summaries are recomputed and re-sent by the client (today's
// rings change all day), so the store upserts them keyed on date rather than
// insert-only. Nil value/goal fields are explicit JSON nulls that overwrite any
// previously stored value.
type ActivitySummaryLine struct {
	ActivitySummary struct {
		Date            float64          `json:"date"`      // legacy epoch ms at the start of the local day
		LocalDate       string           `json:"localDate"` // YYYY-MM-DD in the source local calendar
		TemporalContext *TemporalContext `json:"temporalContext"`
		MoveKcal        *float64         `json:"moveKcal"`
		MoveGoalKcal    *float64         `json:"moveGoalKcal"`
		ExerciseMin     *float64         `json:"exerciseMin"`
		ExerciseGoalMin *float64         `json:"exerciseGoalMin"`
		StandHours      *float64         `json:"standHours"`
		StandGoalHours  *float64         `json:"standGoalHours"`
		MoveMode        *int16           `json:"moveMode"` // 0 = activeEnergy, 1 = appleMoveTime
		MoveTimeMin     *float64         `json:"moveTimeMin"`
		MoveTimeGoalMin *float64         `json:"moveTimeGoalMin"`
	} `json:"activitySummary"`
}

// Deletion is one tombstone line.
type Deletion struct {
	Deleted struct {
		UUID string `json:"uuid"`
		Type string `json:"type"`
	} `json:"deleted"`
}

// Batch is a fully parsed NDJSON batch.
type Batch struct {
	familyCounts      familyLineCounts
	suppressedSamples int
	sourceDeleted     int64
	familyValidated   bool
	bodyDigest        []byte
	Header            BatchHeader
	Samples           []Sample
	Deletions         []Deletion
	Routes            []RouteLine
	Series            []SeriesLine
	Aggregates        []AggregateLine
	ActivitySummaries []ActivitySummaryLine
	Profile           *ProfileLine
	// ParseMs is the server-measured time to read+parse this batch off the wire.
	// Set by the handler after ParseBatch returns and persisted with the batch
	// row; not part of the wire format.
	ParseMs int64
}

const (
	scannerInitBuf = 64 * 1024
	// Samples are ~2KB lines; the largest lines are ECGs (~15k voltages,
	// roughly 200-400KB) and 4000-point route lines (~400KB), so 4MB leaves
	// an order of magnitude of headroom.
	scannerMaxLine = 4 * 1024 * 1024
	// maxRoutePoints caps points per route line; clients split longer routes.
	maxRoutePoints = 4000
	// maxSeriesPoints caps points per workout-series line; clients split longer streams.
	maxSeriesPoints = 4000
	// Check client counts before they become slice capacities.
	maxDeclaredItems = 5_000
	maxBatchLines    = 5_000
	// Per-line point caps keep individual JSON lines small; this per-batch cap
	// prevents many valid-sized lines from creating an unbounded transaction.
	maxBatchPoints = 100_000
)

type BatchLimitError struct{ msg string }

func (e *BatchLimitError) Error() string { return e.msg }

func batchLimitErrf(format string, args ...any) error {
	return &BatchLimitError{msg: fmt.Sprintf(format, args...)}
}

// ParseError marks client-side (HTTP 400) protocol violations.
type ParseError struct {
	msg   string
	cause error
}

func (e *ParseError) Error() string { return e.msg }
func (e *ParseError) Unwrap() error { return e.cause }

func parseErrf(format string, args ...any) error {
	return &ParseError{msg: fmt.Sprintf(format, args...)}
}

func wrapParseErr(err error, format string, args ...any) error {
	return &ParseError{msg: fmt.Sprintf(format, args...), cause: err}
}

// ProtocolVersion is the wire-format version this server speaks: the NDJSON
// header's schemaVersion and the X-Puls-Protocol request header both name it.
// Bump it (and extend supportedProtocolVersions) only together with the
// protocol spec; a client that declares a version outside the supported set
// is refused before any decompression or database work with a fixed
// negotiation body so it can explain the mismatch instead of stalling.
const ProtocolVersion = 1

// supportedProtocolVersions is what GET /v1/capabilities advertises and what
// the 400 negotiation body reports as supportedVersions.
var supportedProtocolVersions = []int{ProtocolVersion}

// ProtocolVersionError marks a request whose declared wire-format version this
// server does not speak. It is a client-side failure like ParseError (400),
// but the handler answers it with the fixed negotiation body rather than the
// error text; raw error text stays internal to the parser.
type ProtocolVersionError struct {
	// Source names where the version came from: the "schemaVersion" header
	// field or the "X-Puls-Protocol" request header.
	Source  string
	Version string
}

func (e *ProtocolVersionError) Error() string {
	return fmt.Sprintf("unsupported protocol version: %s %s (supported versions %v)",
		e.Source, e.Version, supportedProtocolVersions)
}

// checkProtocolVersion accepts v when this server speaks it.
func checkProtocolVersion(source string, v int) error {
	for _, s := range supportedProtocolVersions {
		if v == s {
			return nil
		}
	}
	return &ProtocolVersionError{Source: source, Version: strconv.Itoa(v)}
}

// parseProtocolHeader interprets an X-Puls-Protocol request header. Empty is
// a client that predates versioning (nil, nil). Anything this server does not
// speak — a non-integer included — is a *ProtocolVersionError.
func parseProtocolHeader(value string) (*int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	v, err := strconv.Atoi(value)
	if err != nil {
		return nil, &ProtocolVersionError{Source: "X-Puls-Protocol", Version: strconv.Quote(value)}
	}
	if err := checkProtocolVersion("X-Puls-Protocol", v); err != nil {
		return nil, err
	}
	return &v, nil
}

// reconcileProtocolVersions returns the version a batch was sent under once
// both the request header and the body header (each optional, each already
// checked against the supported set) are known. Both present and disagreeing
// is a client bug and is refused like an unsupported version; either alone
// wins; neither is a legacy client, read as ProtocolVersion.
func reconcileProtocolVersions(header, body *int) (int, error) {
	switch {
	case header != nil && body != nil && *header != *body:
		return 0, &ProtocolVersionError{
			Source:  "X-Puls-Protocol",
			Version: fmt.Sprintf("%d disagrees with schemaVersion %d", *header, *body),
		}
	case body != nil:
		return *body, nil
	case header != nil:
		return *header, nil
	}
	return ProtocolVersion, nil
}

// Accepted epoch-millisecond range for every wire timestamp: 0001-01-01 to
// 9999-12-31T23:59:59.999Z. HealthKit cannot produce anything outside it,
// Postgres date/timestamptz casts accept all of it, and rejecting the rest
// as a 400 beats storing it: the old msToTime multiplied into an int64 and
// overflowed past ±292 years, turning e.g. Date.distantPast into an
// implementation-defined but *valid* timestamptz (MinInt64 on amd64 → year
// 1677) that sorted first in every range query and digest window.
const (
	minWireMS = -62135596800000.0
	maxWireMS = 253402300799999.0
)

func validWireMS(ms float64) bool {
	return !math.IsNaN(ms) && ms >= minWireMS && ms <= maxWireMS
}

// msToTime converts fractional epoch milliseconds to time.Time (UTC) without
// overflowing: whole seconds and the sub-second fraction are split first.
//
// The result is truncated to whole microseconds, which is exactly what
// timestamptz stores (pgx drops the sub-microsecond nanoseconds on encode), so
// the value the Go code holds is the value the row gets. Truncation, not
// rounding, on purpose: every stored row was written that way, so the same
// wire value keeps landing on the same microsecond and an identical re-send
// still hits the (uuid, start_ts) primary key. Rounding would buy nothing —
// HealthKit instants carry sub-microsecond precision (stored microsecond
// digits are uniform), so float noise between two encoders straddles a
// microsecond boundary equally often either way — and it would move about
// half of all existing rows a microsecond away from their next re-send. That
// residual 1 µs split is what insertQuantity's near-duplicate guard is for.
func msToTime(ms float64) time.Time {
	sec, frac := math.Modf(ms / 1000)
	return time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC().Truncate(time.Microsecond)
}

// timeToMS converts time.Time back to epoch milliseconds.
func timeToMS(t time.Time) int64 { return t.UnixMilli() }

func isHexN(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < n; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// isUUID validates the canonical 8-4-4-4-12 form so malformed input fails
// with a 400 instead of a database error mid-transaction.
func isUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	return isHexN(s[0:8], 8) && isHexN(s[9:13], 4) && isHexN(s[14:18], 4) &&
		isHexN(s[19:23], 4) && isHexN(s[24:36], 12)
}

func validKind(k string) bool {
	switch k {
	case "quantity", "category", "workout",
		"heartbeatSeries", "ecg", "stateOfMind", "medicationDose":
		return true
	}
	return false
}

func validAggFunc(f string) bool {
	switch f {
	case "sum", "average", "min", "max", "mostRecent", "duration":
		return true
	}
	return false
}

func validIntervalUnit(u string) bool {
	switch u {
	case "minute", "hour", "day", "week", "month":
		return true
	}
	return false
}

func validDeviceFilter(d string) bool {
	switch d {
	case "all", "watch", "iphone":
		return true
	}
	return false
}

func validLocalDate(s string) bool {
	if len(s) != len("2006-01-02") {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// ParseBatch stream-parses a gunzipped NDJSON body line by line.
func ParseBatch(r io.Reader) (*Batch, error) {
	return parseBatch(r, false)
}

func ParseFamilyBatch(r io.Reader) (*Batch, error) {
	return parseBatch(r, true)
}

func parseBatch(r io.Reader, family bool) (*Batch, error) {
	decodeLine := func(sc *bufio.Scanner, data []byte, value any) error {
		if err := scanUnmarshal(sc, data, value); err != nil {
			return err
		}
		if family {
			return validateFamilyLine(data, value)
		}
		return nil
	}
	digest := sha256.New()
	sc := bufio.NewScanner(io.TeeReader(r, digest))
	sc.Buffer(make([]byte, scannerInitBuf), scannerMaxLine+2)
	sc.Split(batchLine)

	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, wrapParseErr(err, "reading header line: %v", err)
		}
		return nil, parseErrf("empty body: missing batch header")
	}
	var b Batch
	if err := decodeLine(sc, sc.Bytes(), &b.Header); err != nil {
		return nil, wrapParseErr(err, "invalid batch header: %v", err)
	}
	// Version before anything else: a header from a future version may not
	// even carry the fields validated below, and the negotiation error is the
	// one such a client can act on.
	if v := b.Header.SchemaVersion; v != nil {
		if err := checkProtocolVersion("schemaVersion", *v); err != nil {
			return nil, err
		}
	}
	if !isUUID(b.Header.BatchID) {
		return nil, parseErrf("invalid batch header: batchID %q is not a UUID", b.Header.BatchID)
	}
	if zone := b.Header.TimeZoneID; zone != "" {
		if len(zone) > 100 || zone == "Local" {
			return nil, parseErrf("invalid batch header: invalid timeZoneID")
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return nil, parseErrf("invalid batch header: invalid timeZoneID")
		}
	}
	if b.Header.Type == "" {
		return nil, parseErrf("invalid batch header: missing type")
	}
	if b.Header.SampleCount < 0 || b.Header.DeletionCount < 0 ||
		b.Header.RouteCount < 0 || b.Header.AggregateCount < 0 ||
		b.Header.SeriesCount < 0 || b.Header.ActivitySummaryCount < 0 ||
		b.Header.ProfileCount < 0 {
		return nil, parseErrf("invalid batch header: negative counts")
	}
	if b.Header.ProfileCount > 1 {
		return nil, parseErrf("invalid batch header: profileCount %d must be 0 or 1", b.Header.ProfileCount)
	}
	counts := []struct {
		name  string
		value int
	}{
		{"sampleCount", b.Header.SampleCount},
		{"deletionCount", b.Header.DeletionCount},
		{"routeCount", b.Header.RouteCount},
		{"seriesCount", b.Header.SeriesCount},
		{"aggregateCount", b.Header.AggregateCount},
		{"activitySummaryCount", b.Header.ActivitySummaryCount},
	}
	totalLines := b.Header.ProfileCount
	for _, count := range counts {
		if count.value > maxDeclaredItems {
			return nil, batchLimitErrf("invalid batch header: %s %d exceeds limit %d",
				count.name, count.value, maxDeclaredItems)
		}
		totalLines += count.value
	}
	if totalLines > maxBatchLines {
		return nil, batchLimitErrf("invalid batch header: %d declared lines exceeds limit %d",
			totalLines, maxBatchLines)
	}

	b.Samples = make([]Sample, 0, b.Header.SampleCount)
	for i := 0; i < b.Header.SampleCount; i++ {
		line, err := nextLine(sc)
		if err != nil {
			return nil, wrapParseErr(err, "sample %d/%d: %v", i+1, b.Header.SampleCount, err)
		}
		var s Sample
		if err := decodeLine(sc, line, &s); err != nil {
			return nil, wrapParseErr(err, "sample %d: invalid JSON: %v", i+1, err)
		}
		if err := validateSample(&s); err != nil {
			return nil, parseErrf("sample %d (%s): %v", i+1, s.UUID, err)
		}
		b.Samples = append(b.Samples, s)
	}

	b.Deletions = make([]Deletion, 0, b.Header.DeletionCount)
	for i := 0; i < b.Header.DeletionCount; i++ {
		line, err := nextLine(sc)
		if err != nil {
			return nil, wrapParseErr(err, "deletion %d/%d: %v", i+1, b.Header.DeletionCount, err)
		}
		var d Deletion
		if err := decodeLine(sc, line, &d); err != nil {
			return nil, wrapParseErr(err, "deletion %d: invalid JSON: %v", i+1, err)
		}
		if !isUUID(d.Deleted.UUID) {
			return nil, parseErrf("deletion %d: uuid %q is not a UUID", i+1, d.Deleted.UUID)
		}
		if d.Deleted.Type == "" {
			return nil, parseErrf("deletion %d: missing type", i+1)
		}
		b.Deletions = append(b.Deletions, d)
	}

	b.Routes = make([]RouteLine, 0, b.Header.RouteCount)
	routePoints := 0
	for i := 0; i < b.Header.RouteCount; i++ {
		line, err := nextLine(sc)
		if err != nil {
			return nil, wrapParseErr(err, "route %d/%d: %v", i+1, b.Header.RouteCount, err)
		}
		var rl RouteLine
		if err := decodeLine(sc, line, &rl); err != nil {
			return nil, wrapParseErr(err, "route %d: invalid JSON: %v", i+1, err)
		}
		if err := validateRoute(&rl); err != nil {
			return nil, parseErrf("route %d: %v", i+1, err)
		}
		routePoints += len(rl.Route.Points)
		if routePoints > maxBatchPoints {
			return nil, batchLimitErrf("route %d: batch route points %d exceeds limit %d",
				i+1, routePoints, maxBatchPoints)
		}
		b.Routes = append(b.Routes, rl)
	}

	b.Series = make([]SeriesLine, 0, b.Header.SeriesCount)
	seriesPoints := 0
	for i := 0; i < b.Header.SeriesCount; i++ {
		line, err := nextLine(sc)
		if err != nil {
			return nil, wrapParseErr(err, "series %d/%d: %v", i+1, b.Header.SeriesCount, err)
		}
		var sl SeriesLine
		if err := decodeLine(sc, line, &sl); err != nil {
			return nil, wrapParseErr(err, "series %d: invalid JSON: %v", i+1, err)
		}
		if err := validateSeries(&sl); err != nil {
			return nil, parseErrf("series %d: %v", i+1, err)
		}
		seriesPoints += len(sl.Series.Points)
		if seriesPoints > maxBatchPoints {
			return nil, batchLimitErrf("series %d: batch series points %d exceeds limit %d",
				i+1, seriesPoints, maxBatchPoints)
		}
		b.Series = append(b.Series, sl)
	}

	b.Aggregates = make([]AggregateLine, 0, b.Header.AggregateCount)
	for i := 0; i < b.Header.AggregateCount; i++ {
		line, err := nextLine(sc)
		if err != nil {
			return nil, wrapParseErr(err, "aggregate %d/%d: %v", i+1, b.Header.AggregateCount, err)
		}
		var al AggregateLine
		if err := decodeLine(sc, line, &al); err != nil {
			return nil, wrapParseErr(err, "aggregate %d: invalid JSON: %v", i+1, err)
		}
		if err := validateAggregate(&al); err != nil {
			return nil, parseErrf("aggregate %d: %v", i+1, err)
		}
		b.Aggregates = append(b.Aggregates, al)
	}

	b.ActivitySummaries = make([]ActivitySummaryLine, 0, b.Header.ActivitySummaryCount)
	for i := 0; i < b.Header.ActivitySummaryCount; i++ {
		line, err := nextLine(sc)
		if err != nil {
			return nil, wrapParseErr(err, "activity summary %d/%d: %v", i+1, b.Header.ActivitySummaryCount, err)
		}
		var asl ActivitySummaryLine
		if err := decodeLine(sc, line, &asl); err != nil {
			return nil, wrapParseErr(err, "activity summary %d: invalid JSON: %v", i+1, err)
		}
		if err := validateActivitySummary(&asl); err != nil {
			return nil, parseErrf("activity summary %d: %v", i+1, err)
		}
		b.ActivitySummaries = append(b.ActivitySummaries, asl)
	}

	if b.Header.ProfileCount == 1 {
		line, err := nextLine(sc)
		if err != nil {
			return nil, wrapParseErr(err, "profile: %v", err)
		}
		var pl ProfileLine
		if err := decodeLine(sc, line, &pl); err != nil {
			return nil, wrapParseErr(err, "profile: invalid JSON: %v", err)
		}
		if pl.Profile == nil {
			return nil, parseErrf(`profile: missing or null "profile" object`)
		}
		if dob := pl.Profile.DateOfBirth; dob != nil && !validWireMS(*dob) {
			return nil, parseErrf("profile: dateOfBirth out of range")
		}
		b.Profile = &pl
	}

	// Trailing non-blank content means the counts in the header were wrong.
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			return nil, parseErrf("unexpected trailing line after %d samples, %d deletions, %d routes, %d series, %d aggregates, %d activity summaries and %d profile",
				b.Header.SampleCount, b.Header.DeletionCount, b.Header.RouteCount,
				b.Header.SeriesCount, b.Header.AggregateCount, b.Header.ActivitySummaryCount,
				b.Header.ProfileCount)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, wrapParseErr(err, "reading body: %v", err)
	}
	b.bodyDigest = digest.Sum(nil)
	b.familyValidated = family
	return &b, nil
}

func batchLine(data []byte, atEOF bool) (int, []byte, error) {
	advance, line, err := bufio.ScanLines(data, atEOF)
	if len(line) > scannerMaxLine || (advance == 0 && len(data) > scannerMaxLine+1) {
		return 0, nil, batchLimitErrf("line exceeds limit %d", scannerMaxLine)
	}
	return advance, line, err
}

func nextLine(sc *bufio.Scanner) ([]byte, error) {
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue // tolerate stray blank lines
		}
		return sc.Bytes(), nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("unexpected end of body")
}

// scanUnmarshal decodes one line. Deliberately not strict: unknown fields are
// ignored, because new optional fields are additive in protocol v1 (a v1
// receiver must accept them; see docs/protocol/README.md).
//
// Scanner may return a final partial token together with an underlying reader
// error. Prefer and preserve that error (notably *http.MaxBytesError) over the
// secondary JSON syntax error caused by the truncated token.
func scanUnmarshal(sc *bufio.Scanner, data []byte, v any) error {
	if scanErr := sc.Err(); scanErr != nil {
		return scanErr
	}
	if _, err := jcs.Transform(data); err != nil {
		return err
	}
	// Decode the source bytes so validation cannot double-round ECG float32 values.
	return json.Unmarshal(data, v)
}

func validateSample(s *Sample) error {
	if !isUUID(s.UUID) {
		return fmt.Errorf("uuid %q is not a UUID", s.UUID)
	}
	if s.Type == "" {
		return fmt.Errorf("missing type")
	}
	if !validKind(s.Kind) {
		return fmt.Errorf("invalid kind %q", s.Kind)
	}
	if s.Start == 0 {
		return fmt.Errorf("missing start")
	}
	if s.End == 0 {
		s.End = s.Start
	}
	if !validWireMS(s.Start) || !validWireMS(s.End) {
		return fmt.Errorf("start/end out of range")
	}
	if err := validateTemporalContext(s.StartContext); err != nil {
		return fmt.Errorf("startContext: %w", err)
	}
	if err := validateTemporalContext(s.EndContext); err != nil {
		return fmt.Errorf("endContext: %w", err)
	}
	switch s.Kind {
	case "category":
		if s.Category == nil {
			return fmt.Errorf("category sample missing category value")
		}
	case "workout":
		if s.Workout == nil {
			return fmt.Errorf("workout sample missing workout payload")
		}
	case "heartbeatSeries":
		if s.Heartbeats == nil {
			return fmt.Errorf("heartbeatSeries sample missing heartbeats payload")
		}
	case "ecg":
		if s.ECG == nil {
			return fmt.Errorf("ecg sample missing ecg payload")
		}
	case "stateOfMind":
		if s.StateOfMind == nil {
			return fmt.Errorf("stateOfMind sample missing stateOfMind payload")
		}
	case "medicationDose":
		if s.MedicationDose == nil {
			return fmt.Errorf("medicationDose sample missing medicationDose payload")
		}
		if id := s.MedicationDose.MedicationID; id != nil && !isUUID(*id) {
			return fmt.Errorf("medicationDose medicationID is not a UUID")
		}
		if at := s.MedicationDose.ScheduledAt; at != nil && !validWireMS(*at) {
			return fmt.Errorf("medicationDose scheduledAt out of range")
		}
		if err := validateTemporalContext(s.MedicationDose.ScheduledAtContext); err != nil {
			return fmt.Errorf("scheduledAtContext: %w", err)
		}
	}
	return nil
}

func validateTemporalContext(tc *TemporalContext) error {
	if tc == nil {
		return nil
	}
	if tc.TimeZoneID == "" {
		return fmt.Errorf("missing timeZoneID")
	}
	if tc.UTCOffsetSeconds < -86400 || tc.UTCOffsetSeconds > 86400 {
		return fmt.Errorf("utcOffsetSeconds %d out of range", tc.UTCOffsetSeconds)
	}
	if tc.Source == "" {
		return fmt.Errorf("missing source")
	}
	if tc.Confidence == "" {
		return fmt.Errorf("missing confidence")
	}
	return nil
}

func validateAggregate(al *AggregateLine) error {
	a := &al.Aggregate
	if a.Type == "" {
		return fmt.Errorf("missing type")
	}
	if !validAggFunc(a.Func) {
		return fmt.Errorf("invalid func %q", a.Func)
	}
	if a.IntervalValue < 1 {
		return fmt.Errorf("intervalValue %d must be >= 1", a.IntervalValue)
	}
	if !validIntervalUnit(a.IntervalUnit) {
		return fmt.Errorf("invalid intervalUnit %q", a.IntervalUnit)
	}
	if !validDeviceFilter(a.DeviceFilter) {
		return fmt.Errorf("invalid deviceFilter %q", a.DeviceFilter)
	}
	if a.BucketStart == 0 {
		return fmt.Errorf("missing bucketStart")
	}
	if a.BucketEnd <= a.BucketStart {
		return fmt.Errorf("bucketEnd must be after bucketStart")
	}
	if !validWireMS(a.BucketStart) || !validWireMS(a.BucketEnd) {
		return fmt.Errorf("bucketStart/bucketEnd out of range")
	}
	if err := validateTemporalContext(a.BucketStartContext); err != nil {
		return fmt.Errorf("bucketStartContext: %w", err)
	}
	if err := validateTemporalContext(a.BucketEndContext); err != nil {
		return fmt.Errorf("bucketEndContext: %w", err)
	}
	return nil
}

func validateActivitySummary(asl *ActivitySummaryLine) error {
	a := &asl.ActivitySummary
	if a.Date == 0 {
		return fmt.Errorf("missing date")
	}
	if !validWireMS(a.Date) {
		return fmt.Errorf("date out of range")
	}
	if a.LocalDate != "" && !validLocalDate(a.LocalDate) {
		return fmt.Errorf("invalid localDate %q", a.LocalDate)
	}
	if err := validateTemporalContext(a.TemporalContext); err != nil {
		return fmt.Errorf("temporalContext: %w", err)
	}
	if a.MoveMode != nil && *a.MoveMode != 0 && *a.MoveMode != 1 {
		return fmt.Errorf("invalid moveMode %d", *a.MoveMode)
	}
	return nil
}

func validateRoute(rl *RouteLine) error {
	r := &rl.Route
	if !isUUID(r.WorkoutUUID) {
		return fmt.Errorf("workoutUUID %q is not a UUID", r.WorkoutUUID)
	}
	if len(r.Points) > maxRoutePoints {
		return fmt.Errorf("%d points exceeds the per-line limit of %d", len(r.Points), maxRoutePoints)
	}
	for i := range r.Points {
		if r.Points[i].T == 0 {
			return fmt.Errorf("point %d: missing t", i+1)
		}
		if !validWireMS(r.Points[i].T) {
			return fmt.Errorf("point %d: t out of range", i+1)
		}
		if err := validateTemporalContext(r.Points[i].TemporalContext); err != nil {
			return fmt.Errorf("point %d temporalContext: %w", i+1, err)
		}
	}
	return nil
}

func validateSeries(sl *SeriesLine) error {
	s := &sl.Series
	if !isUUID(s.WorkoutUUID) {
		return fmt.Errorf("workoutUUID %q is not a UUID", s.WorkoutUUID)
	}
	if s.Type == "" {
		return fmt.Errorf("missing type")
	}
	if len(s.Points) > maxSeriesPoints {
		return fmt.Errorf("%d points exceeds the per-line limit of %d", len(s.Points), maxSeriesPoints)
	}
	for i := range s.Points {
		if s.Points[i].T == 0 {
			return fmt.Errorf("point %d: missing t", i+1)
		}
		if !validWireMS(s.Points[i].T) {
			return fmt.Errorf("point %d: t out of range", i+1)
		}
		if err := validateTemporalContext(s.Points[i].TemporalContext); err != nil {
			return fmt.Errorf("point %d temporalContext: %w", i+1, err)
		}
	}
	return nil
}
