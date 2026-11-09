package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	hdrLine = `{"batchID":"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f","deviceID":"dev-1","type":"HKQuantityTypeIdentifierHeartRate","reason":"incremental","exportedAt":1718000000000,"sampleCount":%d,"deletionCount":%d}`

	hrSample = `{"uuid":"11111111-1111-4111-8111-111111111111","type":"HKQuantityTypeIdentifierHeartRate","kind":"quantity","start":1718000000000,"end":1718000005000,"value":62.5,"unit":"count/min","category":null,"sourceName":"Apple Watch","sourceBundleID":"com.apple.health","sourceVersion":"10.0","device":"Apple Watch","metadata":{"HKMetadataKeyHeartRateMotionContext":1},"workout":null}`

	catSample = `{"uuid":"22222222-2222-4222-8222-222222222222","type":"HKCategoryTypeIdentifierSleepAnalysis","kind":"category","start":1718000000000,"end":1718003600000,"category":3,"sourceName":"Apple Watch"}`

	workoutSample = `{"uuid":"33333333-3333-4333-8333-333333333333","type":"HKWorkoutTypeIdentifier","kind":"workout","start":1718000000000,"end":1718003600500,"workout":{"activityType":"running","duration":3600.5,"totalEnergyKcal":450.2,"totalDistanceMeters":8046.7,"statistics":{"HKQuantityTypeIdentifierHeartRate":152.0}}}`

	delLine = `{"deleted":{"uuid":"44444444-4444-4444-8444-444444444444","type":"HKQuantityTypeIdentifierHeartRate"}}`

	hdrLineRoutes = `{"batchID":"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f","deviceID":"dev-1","type":"HKWorkoutTypeIdentifier","reason":"incremental","exportedAt":1718000000000,"sampleCount":%d,"deletionCount":%d,"routeCount":%d}`

	hbSample = `{"uuid":"55555555-5555-4555-8555-555555555555","type":"HKDataTypeIdentifierHeartbeatSeries","kind":"heartbeatSeries","start":1718000000000,"end":1718000060000,"sourceName":"Apple Watch","heartbeats":[[0.5,false],[1.2,true]]}`

	ecgSample = `{"uuid":"66666666-6666-4666-8666-666666666666","type":"HKDataTypeIdentifierElectrocardiogram","kind":"ecg","start":1718000000000,"end":1718000030000,"sourceName":"Apple Watch","ecg":{"classification":"sinusRhythm","averageHeartRateBpm":61.0,"samplingFrequencyHz":512.0,"symptomsStatus":"none","voltagesUV":[1.5,-2.25,3.0]}}`

	somSample = `{"uuid":"77777777-7777-4777-8777-777777777777","type":"HKDataTypeIdentifierStateOfMind","kind":"stateOfMind","start":1718000000000,"end":1718000000000,"sourceName":"iPhone","stateOfMind":{"kind":"momentaryEmotion","valence":0.42,"valenceClassification":"pleasant","labels":["happy"],"associations":["fitness"]}}`

	medSample = `{"uuid":"88888888-8888-4888-8888-888888888888","type":"HKMedicationDoseEventTypeIdentifierMedicationDoseEvent","kind":"medicationDose","start":1718000000000,"end":1718000000000,"sourceName":"iPhone","medicationDose":{"medication":"Ibuprofen","status":"taken","scheduledAt":1717999200000,"doseQuantity":200,"doseUnit":"mg"}}`

	routeLine = `{"route":{"workoutUUID":"33333333-3333-4333-8333-333333333333","points":[{"t":1718000001000,"lat":37.3349,"lon":-122.009,"alt":12.5,"hAcc":3.2,"vAcc":4.1,"speed":2.8,"course":181.0},{"t":1718000002000,"lat":37.335,"lon":-122.0091,"alt":null,"hAcc":null,"vAcc":null,"speed":null,"course":null}]}}`

	hdrLineAggs = `{"batchID":"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f","deviceID":"dev-1","type":"HKQuantityTypeIdentifierHeartRate","reason":"incremental","exportedAt":1718000000000,"sampleCount":%d,"deletionCount":%d,"routeCount":%d,"aggregateCount":%d}`

	aggLine = `{"aggregate":{"type":"HKQuantityTypeIdentifierHeartRate","func":"average","intervalValue":1,"intervalUnit":"hour","deviceFilter":"watch","bucketStart":1718000000000,"bucketEnd":1718003600000,"value":62.4,"unit":"count/min"}}`

	aggLineNull = `{"aggregate":{"type":"HKQuantityTypeIdentifierStepCount","func":"sum","intervalValue":1,"intervalUnit":"day","deviceFilter":"all","bucketStart":1718000000000,"bucketEnd":1718086400000,"value":null,"unit":"count"}}`

	hdrLineAS = `{"batchID":"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f","deviceID":"dev-1","type":"HKActivitySummaryTypeIdentifier","reason":"incremental","exportedAt":1718000000000,"sampleCount":%d,"deletionCount":%d,"routeCount":%d,"aggregateCount":%d,"activitySummaryCount":%d}`

	asLine = `{"activitySummary":{"date":1718000000000,"localDate":"2024-06-10","temporalContext":{"timeZoneID":"America/Los_Angeles","utcOffsetSeconds":-25200,"source":"device_current","confidence":"inferred"},"moveKcal":420.5,"moveGoalKcal":600.0,"exerciseMin":25.0,"exerciseGoalMin":30.0,"standHours":9.0,"standGoalHours":12.0,"moveMode":0,"moveTimeMin":null,"moveTimeGoalMin":null}}`

	asLineNull = `{"activitySummary":{"date":1718086400000,"moveKcal":null,"moveGoalKcal":600.0,"exerciseMin":null,"exerciseGoalMin":30.0,"standHours":null,"standGoalHours":12.0,"moveMode":1,"moveTimeMin":0.0,"moveTimeGoalMin":30.0}}`

	// Enhanced workout: richer per-type stats, events, sub-activities.
	workoutSampleRich = `{"uuid":"33333333-3333-4333-8333-333333333333","type":"HKWorkoutTypeIdentifier","kind":"workout","start":1718000000000,"end":1718003600500,"workout":{"activityType":"running","duration":3600.5,"totalEnergyKcal":450.2,"totalDistanceMeters":8046.7,"statistics":{"HKQuantityTypeIdentifierHeartRate":152.0},"statisticsDetail":{"HKQuantityTypeIdentifierHeartRate":{"min":98.0,"avg":152.0,"max":178.0},"HKQuantityTypeIdentifierActiveEnergyBurned":{"sum":450.2}},"events":[{"type":"lap","start":1718001800000},{"type":"segment","start":1718000000000,"end":1718001800000}],"activities":[{"activityType":"running","start":1718000000000,"end":1718003600500,"duration":3600.5,"statistics":{"HKQuantityTypeIdentifierHeartRate":{"avg":152.0,"max":178.0}}}]}}`

	// Full header with every optional count, as a versioned client sends it.
	hdrLineFull = `{"batchID":"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f","deviceID":"dev-1","type":"HKWorkoutTypeIdentifier","reason":"incremental","exportedAt":1718000000000,"schemaVersion":1,"clientVersion":"0.1.0 (1)","sampleCount":%d,"deletionCount":%d,"routeCount":%d,"aggregateCount":%d,"seriesCount":%d,"profileCount":%d}`

	// Versioned header with the schemaVersion left to the test.
	hdrLineVersioned = `{"batchID":"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f","deviceID":"dev-1","type":"HKQuantityTypeIdentifierHeartRate","reason":"incremental","exportedAt":1718000000000,"schemaVersion":%s,"clientVersion":"0.1.0 (1)","sampleCount":%d,"deletionCount":%d}`

	// The app's connection probe for a receiver without /v1/capabilities: a
	// header-only batch, every count zero, reason "manual".
	probeLine = `{"batchID":"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f","deviceID":"dev-1","type":"HKQuantityTypeIdentifierHeartRate","reason":"manual","exportedAt":1718000000000,"schemaVersion":1,"clientVersion":"0.1.0 (1)","sampleCount":0,"deletionCount":0,"routeCount":0,"seriesCount":0,"aggregateCount":0,"activitySummaryCount":0,"profileCount":0}`

	seriesLine = `{"series":{"workoutUUID":"33333333-3333-4333-8333-333333333333","type":"HKQuantityTypeIdentifierHeartRate","unit":"count/min","points":[{"t":1718000001000,"value":120.0},{"t":1718000002000,"value":135.5}]}}`

	profileLine = `{"profile":{"dateOfBirth":631152000000,"biologicalSex":"male"}}`
)

func ndjson(t *testing.T, sampleCount, deletionCount int, lines ...string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(hdrLine, sampleCount, deletionCount))
	for _, l := range lines {
		sb.WriteString("\n")
		sb.WriteString(l)
	}
	sb.WriteString("\n")
	return sb.String()
}

func ndjsonRoutes(t *testing.T, sampleCount, deletionCount, routeCount int, lines ...string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(hdrLineRoutes, sampleCount, deletionCount, routeCount))
	for _, l := range lines {
		sb.WriteString("\n")
		sb.WriteString(l)
	}
	sb.WriteString("\n")
	return sb.String()
}

func ndjsonAggs(t *testing.T, sampleCount, deletionCount, routeCount, aggregateCount int, lines ...string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(hdrLineAggs, sampleCount, deletionCount, routeCount, aggregateCount))
	for _, l := range lines {
		sb.WriteString("\n")
		sb.WriteString(l)
	}
	sb.WriteString("\n")
	return sb.String()
}

func ndjsonAS(t *testing.T, sampleCount, deletionCount, routeCount, aggregateCount, activitySummaryCount int, lines ...string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(hdrLineAS, sampleCount, deletionCount, routeCount, aggregateCount, activitySummaryCount))
	for _, l := range lines {
		sb.WriteString("\n")
		sb.WriteString(l)
	}
	sb.WriteString("\n")
	return sb.String()
}

func ndjsonFull(t *testing.T, sampleCount, deletionCount, routeCount, aggregateCount, seriesCount, profileCount int, lines ...string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(hdrLineFull, sampleCount, deletionCount, routeCount, aggregateCount, seriesCount, profileCount))
	for _, l := range lines {
		sb.WriteString("\n")
		sb.WriteString(l)
	}
	sb.WriteString("\n")
	return sb.String()
}

func TestParseBatch_EnhancedWorkout(t *testing.T) {
	// Order on the wire: samples, deletions, routes, series, aggregates, profile.
	body := ndjsonFull(t, 1, 0, 1, 0, 1, 1, workoutSampleRich, routeLine, seriesLine, profileLine)
	b, err := ParseBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Samples) != 1 || len(b.Routes) != 1 || len(b.Series) != 1 || b.Profile == nil {
		t.Fatalf("counts: samples=%d routes=%d series=%d profile=%v",
			len(b.Samples), len(b.Routes), len(b.Series), b.Profile != nil)
	}
	if b.Header.SchemaVersion == nil || *b.Header.SchemaVersion != 1 || b.Header.ClientVersion != "0.1.0 (1)" {
		t.Errorf("schemaVersion = %v, clientVersion = %q", b.Header.SchemaVersion, b.Header.ClientVersion)
	}

	w := b.Samples[0].Workout
	if w == nil {
		t.Fatal("workout payload nil")
	}
	if hr := w.StatisticsDetail["HKQuantityTypeIdentifierHeartRate"]; hr.Max == nil || *hr.Max != 178.0 || hr.Avg == nil || *hr.Avg != 152.0 {
		t.Errorf("statisticsDetail HR = %+v", hr)
	}
	if en := w.StatisticsDetail["HKQuantityTypeIdentifierActiveEnergyBurned"]; en.Sum == nil || *en.Sum != 450.2 {
		t.Errorf("statisticsDetail energy = %+v", en)
	}
	if len(w.Events) != 2 || w.Events[0].Type != "lap" || w.Events[1].Type != "segment" || w.Events[1].End == nil {
		t.Errorf("events = %+v", w.Events)
	}
	if len(w.Activities) != 1 || w.Activities[0].ActivityType != "running" || w.Activities[0].Duration != 3600.5 {
		t.Errorf("activities = %+v", w.Activities)
	}

	s := b.Series[0].Series
	if s.Type != "HKQuantityTypeIdentifierHeartRate" || len(s.Points) != 2 || s.Points[1].Value != 135.5 {
		t.Errorf("series = %+v", s)
	}
	if b.Profile.Profile.BiologicalSex == nil || *b.Profile.Profile.BiologicalSex != "male" || b.Profile.Profile.DateOfBirth == nil {
		t.Errorf("profile = %+v", b.Profile.Profile)
	}
}

func TestParseBatch_ProfileWrapperMustBePresentAndNonNull(t *testing.T) {
	for _, line := range []string{`{}`, `{"profile":null}`} {
		body := ndjsonFull(t, 0, 0, 0, 0, 0, 1, line)
		_, err := ParseBatch(strings.NewReader(body))
		if err == nil {
			t.Fatalf("profile line %s: expected error", line)
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("profile line %s: error %T, want ParseError", line, err)
		}
		if !strings.Contains(err.Error(), `missing or null "profile" object`) {
			t.Fatalf("profile line %s: error = %q", line, err)
		}
	}
}

func TestParseBatch_EmptyProfileObjectIsClearAllSnapshot(t *testing.T) {
	body := ndjsonFull(t, 0, 0, 0, 0, 0, 1, `{"profile":{}}`)
	batch, err := ParseBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if batch.Profile == nil || batch.Profile.Profile == nil {
		t.Fatalf("profile = %#v, want non-nil empty snapshot", batch.Profile)
	}
	profile := batch.Profile.Profile
	if profile.Name != nil || profile.Email != nil || profile.DateOfBirth != nil || profile.BiologicalSex != nil {
		t.Fatalf("empty profile snapshot = %#v, want all fields nil", profile)
	}
}

func TestParseBatch_SeriesTooManyPoints(t *testing.T) {
	var pts strings.Builder
	for i := 0; i <= maxSeriesPoints; i++ {
		if i > 0 {
			pts.WriteString(",")
		}
		fmt.Fprintf(&pts, `{"t":%d,"value":1.0}`, 1718000000000+i)
	}
	line := `{"series":{"workoutUUID":"33333333-3333-4333-8333-333333333333","type":"HKQuantityTypeIdentifierHeartRate","points":[` + pts.String() + `]}}`
	body := ndjsonFull(t, 0, 0, 0, 0, 1, 0, line)
	if _, err := ParseBatch(strings.NewReader(body)); err == nil {
		t.Fatal("expected error for too many series points")
	}
}

func TestParseBatch_MixedKinds(t *testing.T) {
	body := ndjson(t, 3, 1, hrSample, catSample, workoutSample, delLine)
	b, err := ParseBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Header.BatchID != "6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f" {
		t.Errorf("batchID = %q", b.Header.BatchID)
	}
	if b.Header.Reason != "incremental" || b.Header.DeviceID != "dev-1" {
		t.Errorf("header = %+v", b.Header)
	}
	if len(b.Samples) != 3 || len(b.Deletions) != 1 {
		t.Fatalf("got %d samples, %d deletions", len(b.Samples), len(b.Deletions))
	}

	hr := b.Samples[0]
	if hr.Kind != "quantity" || hr.Value == nil || *hr.Value != 62.5 || hr.Unit == nil || *hr.Unit != "count/min" {
		t.Errorf("quantity sample = %+v", hr)
	}
	if got := hr.Metadata["HKMetadataKeyHeartRateMotionContext"]; got != float64(1) {
		t.Errorf("metadata = %v", hr.Metadata)
	}

	cat := b.Samples[1]
	if cat.Kind != "category" || cat.Category == nil || *cat.Category != 3 {
		t.Errorf("category sample = %+v", cat)
	}

	wk := b.Samples[2]
	if wk.Workout == nil || wk.Workout.ActivityType != "running" || wk.Workout.Duration != 3600.5 {
		t.Fatalf("workout sample = %+v", wk)
	}
	if wk.Workout.TotalEnergyKcal == nil || *wk.Workout.TotalEnergyKcal != 450.2 {
		t.Errorf("energy = %v", wk.Workout.TotalEnergyKcal)
	}
	if wk.Workout.Statistics["HKQuantityTypeIdentifierHeartRate"] != 152.0 {
		t.Errorf("statistics = %v", wk.Workout.Statistics)
	}

	if b.Deletions[0].Deleted.UUID != "44444444-4444-4444-8444-444444444444" {
		t.Errorf("deletion = %+v", b.Deletions[0])
	}
}

func TestParseBatch_NewKinds(t *testing.T) {
	body := ndjson(t, 4, 0, hbSample, ecgSample, somSample, medSample)
	b, err := ParseBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Samples) != 4 {
		t.Fatalf("got %d samples", len(b.Samples))
	}

	hb := b.Samples[0]
	if hb.Kind != "heartbeatSeries" || len(hb.Heartbeats) != 2 {
		t.Fatalf("heartbeat sample = %+v", hb)
	}
	if hb.Heartbeats[0] != (Heartbeat{Seconds: 0.5, PrecededByGap: false}) ||
		hb.Heartbeats[1] != (Heartbeat{Seconds: 1.2, PrecededByGap: true}) {
		t.Errorf("heartbeats = %+v", hb.Heartbeats)
	}

	ecg := b.Samples[1]
	if ecg.Kind != "ecg" || ecg.ECG == nil {
		t.Fatalf("ecg sample = %+v", ecg)
	}
	if ecg.ECG.Classification != "sinusRhythm" || ecg.ECG.SymptomsStatus != "none" {
		t.Errorf("ecg = %+v", ecg.ECG)
	}
	if ecg.ECG.AverageHeartRateBpm == nil || *ecg.ECG.AverageHeartRateBpm != 61.0 ||
		ecg.ECG.SamplingFrequencyHz == nil || *ecg.ECG.SamplingFrequencyHz != 512.0 {
		t.Errorf("ecg rates = %+v", ecg.ECG)
	}
	if len(ecg.ECG.VoltagesUV) != 3 || ecg.ECG.VoltagesUV[1] != -2.25 {
		t.Errorf("voltages = %v", ecg.ECG.VoltagesUV)
	}

	som := b.Samples[2]
	if som.Kind != "stateOfMind" || som.StateOfMind == nil {
		t.Fatalf("stateOfMind sample = %+v", som)
	}
	if som.StateOfMind.Kind != "momentaryEmotion" || som.StateOfMind.Valence != 0.42 ||
		som.StateOfMind.ValenceClassification != "pleasant" {
		t.Errorf("stateOfMind = %+v", som.StateOfMind)
	}
	if len(som.StateOfMind.Labels) != 1 || som.StateOfMind.Labels[0] != "happy" ||
		len(som.StateOfMind.Associations) != 1 || som.StateOfMind.Associations[0] != "fitness" {
		t.Errorf("stateOfMind arrays = %+v", som.StateOfMind)
	}

	med := b.Samples[3]
	if med.Kind != "medicationDose" || med.MedicationDose == nil {
		t.Fatalf("medicationDose sample = %+v", med)
	}
	d := med.MedicationDose
	if d.Medication == nil || *d.Medication != "Ibuprofen" || d.Status != "taken" {
		t.Errorf("medicationDose = %+v", d)
	}
	if d.ScheduledAt == nil || *d.ScheduledAt != 1717999200000 ||
		d.DoseQuantity == nil || *d.DoseQuantity != 200 || d.DoseUnit == nil || *d.DoseUnit != "mg" {
		t.Errorf("medicationDose dose = %+v", d)
	}
}

func TestParseBatch_EmptyHeartbeatsAllowed(t *testing.T) {
	s := strings.Replace(hbSample, `[[0.5,false],[1.2,true]]`, `[]`, 1)
	b, err := ParseBatch(strings.NewReader(ndjson(t, 1, 0, s)))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Samples[0].Heartbeats == nil || len(b.Samples[0].Heartbeats) != 0 {
		t.Errorf("heartbeats = %v", b.Samples[0].Heartbeats)
	}
}

func TestParseBatch_Routes(t *testing.T) {
	body := ndjsonRoutes(t, 1, 1, 1, workoutSample, delLine, routeLine)
	b, err := ParseBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Header.RouteCount != 1 || len(b.Routes) != 1 {
		t.Fatalf("routeCount = %d, routes = %d", b.Header.RouteCount, len(b.Routes))
	}
	r := b.Routes[0].Route
	if r.WorkoutUUID != "33333333-3333-4333-8333-333333333333" || len(r.Points) != 2 {
		t.Fatalf("route = %+v", r)
	}
	p := r.Points[0]
	if p.T != 1718000001000 || p.Lat != 37.3349 || p.Lon != -122.009 {
		t.Errorf("point 0 = %+v", p)
	}
	if p.Alt == nil || *p.Alt != 12.5 || p.HAcc == nil || *p.HAcc != 3.2 ||
		p.Speed == nil || *p.Speed != 2.8 || p.Course == nil || *p.Course != 181.0 {
		t.Errorf("point 0 optionals = %+v", p)
	}
	q := r.Points[1]
	if q.Alt != nil || q.HAcc != nil || q.VAcc != nil || q.Speed != nil || q.Course != nil {
		t.Errorf("point 1 optionals should be nil: %+v", q)
	}
}

func TestParseBatch_NoRouteCountDefaultsToZero(t *testing.T) {
	b, err := ParseBatch(strings.NewReader(ndjson(t, 1, 0, hrSample)))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Header.RouteCount != 0 || len(b.Routes) != 0 {
		t.Errorf("routeCount = %d, routes = %d", b.Header.RouteCount, len(b.Routes))
	}
}

func TestParseBatch_Aggregates(t *testing.T) {
	body := ndjsonAggs(t, 1, 0, 0, 2, hrSample, aggLine, aggLineNull)
	b, err := ParseBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Header.AggregateCount != 2 || len(b.Aggregates) != 2 {
		t.Fatalf("aggregateCount = %d, aggregates = %d", b.Header.AggregateCount, len(b.Aggregates))
	}

	a := b.Aggregates[0].Aggregate
	if a.Type != "HKQuantityTypeIdentifierHeartRate" || a.Func != "average" ||
		a.IntervalValue != 1 || a.IntervalUnit != "hour" || a.DeviceFilter != "watch" {
		t.Errorf("aggregate 0 = %+v", a)
	}
	if a.BucketStart != 1718000000000 || a.BucketEnd != 1718003600000 {
		t.Errorf("aggregate 0 bucket = [%v, %v]", a.BucketStart, a.BucketEnd)
	}
	if a.Value == nil || *a.Value != 62.4 {
		t.Errorf("aggregate 0 value = %v, want 62.4", a.Value)
	}
	if a.Unit == nil || *a.Unit != "count/min" {
		t.Errorf("aggregate 0 unit = %v", a.Unit)
	}

	// Explicit JSON null parses to nil: "the bucket is empty".
	n := b.Aggregates[1].Aggregate
	if n.Type != "HKQuantityTypeIdentifierStepCount" || n.Func != "sum" ||
		n.IntervalUnit != "day" || n.DeviceFilter != "all" {
		t.Errorf("aggregate 1 = %+v", n)
	}
	if n.Value != nil {
		t.Errorf("aggregate 1 value = %v, want nil", n.Value)
	}
}

func TestParseBatch_NoAggregateCountDefaultsToZero(t *testing.T) {
	// Old-style header without aggregateCount still parses.
	b, err := ParseBatch(strings.NewReader(ndjson(t, 1, 0, hrSample)))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Header.AggregateCount != 0 || len(b.Aggregates) != 0 {
		t.Errorf("aggregateCount = %d, aggregates = %d", b.Header.AggregateCount, len(b.Aggregates))
	}
}

func TestParseBatch_ActivitySummaries(t *testing.T) {
	body := ndjsonAS(t, 0, 0, 0, 0, 2, asLine, asLineNull)
	b, err := ParseBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Header.ActivitySummaryCount != 2 || len(b.ActivitySummaries) != 2 {
		t.Fatalf("activitySummaryCount = %d, activitySummaries = %d",
			b.Header.ActivitySummaryCount, len(b.ActivitySummaries))
	}

	a := b.ActivitySummaries[0].ActivitySummary
	if a.Date != 1718000000000 {
		t.Errorf("date = %v", a.Date)
	}
	if a.LocalDate != "2024-06-10" {
		t.Errorf("localDate = %q", a.LocalDate)
	}
	if a.TemporalContext == nil || a.TemporalContext.TimeZoneID != "America/Los_Angeles" ||
		a.TemporalContext.UTCOffsetSeconds != -25200 {
		t.Errorf("temporalContext = %+v", a.TemporalContext)
	}
	if a.MoveKcal == nil || *a.MoveKcal != 420.5 || a.MoveGoalKcal == nil || *a.MoveGoalKcal != 600.0 {
		t.Errorf("move = %v / %v", a.MoveKcal, a.MoveGoalKcal)
	}
	if a.StandHours == nil || *a.StandHours != 9.0 || a.MoveMode == nil || *a.MoveMode != 0 {
		t.Errorf("stand/mode = %v / %v", a.StandHours, a.MoveMode)
	}

	// Explicit JSON null parses to nil: the value must overwrite to NULL.
	n := b.ActivitySummaries[1].ActivitySummary
	if n.MoveKcal != nil || n.ExerciseMin != nil || n.StandHours != nil {
		t.Errorf("nulls not nil: %+v", n)
	}
	if n.MoveMode == nil || *n.MoveMode != 1 || n.MoveTimeMin == nil || *n.MoveTimeMin != 0.0 {
		t.Errorf("move-mode row = %+v", n)
	}
}

func TestActivitySummaryDateKeyPrefersExplicitLocalDate(t *testing.T) {
	line := ActivitySummaryLine{}
	line.ActivitySummary.Date = 1717959600000 // 2024-06-10 00:00:00 Asia/Tokyo, 2024-06-09 UTC
	line.ActivitySummary.LocalDate = "2024-06-10"

	got, err := activitySummaryDateKey(&line.ActivitySummary)
	if err != nil {
		t.Fatalf("activitySummaryDateKey: %v", err)
	}
	if got != "2024-06-10" {
		t.Fatalf("date key = %q, want 2024-06-10", got)
	}
}

// R21: a line without localDate (a client from before it) is the phone's
// local day, never the UTC one. The header has no offset, so the line's own
// temporalContext decides; without one, the protocol's documented legacy
// rule (the UTC date of date) applies, as fixture 03 pins.
func TestActivitySummaryDateKeyWithoutLocalDate(t *testing.T) {
	cases := []struct {
		name   string
		date   float64
		offset *int32
		want   string
	}{
		// 2024-06-10 00:00 in Tokyo is 2024-06-09 15:00Z: the UTC date
		// would put the ring on the previous day.
		{"east of UTC", 1717945200000, ptrInt32(9 * 3600), "2024-06-10"},
		// 2024-06-10 00:00 in Los Angeles (PDT) is 07:00Z the same day.
		{"west of UTC", 1718002800000, ptrInt32(-7 * 3600), "2024-06-10"},
		// Chatham, +12:45: local midnight is 11:15Z the day before.
		{"far east", 1717931700000, ptrInt32(12*3600 + 45*60), "2024-06-10"},
		{"no context: the documented UTC date", 1717945200000, nil, "2024-06-09"},
	}
	for _, tc := range cases {
		line := ActivitySummaryLine{}
		line.ActivitySummary.Date = tc.date
		if tc.offset != nil {
			line.ActivitySummary.TemporalContext = &TemporalContext{
				TimeZoneID: "Fixture/Zone", UTCOffsetSeconds: *tc.offset, Source: "device_current", Confidence: "inferred",
			}
		}
		got, err := activitySummaryDateKey(&line.ActivitySummary)
		if err != nil || got != tc.want {
			t.Errorf("%s: date key = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func ptrInt32(v int32) *int32 { return &v }

func TestParseBatch_NoActivitySummaryCountDefaultsToZero(t *testing.T) {
	// Old-style header without activitySummaryCount still parses.
	b, err := ParseBatch(strings.NewReader(ndjson(t, 1, 0, hrSample)))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Header.ActivitySummaryCount != 0 || len(b.ActivitySummaries) != 0 {
		t.Errorf("activitySummaryCount = %d, activitySummaries = %d",
			b.Header.ActivitySummaryCount, len(b.ActivitySummaries))
	}
}

func TestParseBatch_Errors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"header not json", "not json\n"},
		{"bad batch uuid", strings.Replace(ndjson(t, 0, 0), "6f1c1f1e", "zzzzzzzz", 1)},
		{"missing sample line", ndjson(t, 2, 0, hrSample)},
		{"trailing line", ndjson(t, 1, 0, hrSample, hrSample)},
		{"bad sample uuid", ndjson(t, 1, 0, strings.Replace(hrSample, "11111111", "nothexno", 1))},
		{"bad kind", ndjson(t, 1, 0, strings.Replace(hrSample, `"kind":"quantity"`, `"kind":"mystery"`, 1))},
		{"category missing value", ndjson(t, 1, 0, strings.Replace(catSample, `"category":3,`, "", 1))},
		{"workout missing payload", ndjson(t, 1, 0,
			`{"uuid":"33333333-3333-4333-8333-333333333333","type":"HKWorkoutTypeIdentifier","kind":"workout","start":1718000000000,"end":1718003600500}`)},
		{"deletion bad uuid", ndjson(t, 0, 1, strings.Replace(delLine, "44444444", "xxxxxxxx", 1))},
		{"heartbeatSeries missing payload", ndjson(t, 1, 0, strings.Replace(hbSample, `,"heartbeats":[[0.5,false],[1.2,true]]`, "", 1))},
		{"heartbeat not a pair", ndjson(t, 1, 0, strings.Replace(hbSample, `[1.2,true]`, `[1.2,true,9]`, 1))},
		{"ecg missing payload", ndjson(t, 1, 0, strings.Replace(ecgSample, `,"ecg":{"classification":"sinusRhythm","averageHeartRateBpm":61.0,"samplingFrequencyHz":512.0,"symptomsStatus":"none","voltagesUV":[1.5,-2.25,3.0]}`, "", 1))},
		{"stateOfMind missing payload", ndjson(t, 1, 0, strings.Replace(somSample, `,"stateOfMind":{"kind":"momentaryEmotion","valence":0.42,"valenceClassification":"pleasant","labels":["happy"],"associations":["fitness"]}`, "", 1))},
		{"medicationDose missing payload", ndjson(t, 1, 0, strings.Replace(medSample, `,"medicationDose":{"medication":"Ibuprofen","status":"taken","scheduledAt":1717999200000,"doseQuantity":200,"doseUnit":"mg"}`, "", 1))},
		{"negative routeCount", ndjsonRoutes(t, 0, 0, -1)},
		{"missing route line", ndjsonRoutes(t, 0, 0, 1)},
		{"route bad workout uuid", ndjsonRoutes(t, 0, 0, 1, strings.Replace(routeLine, "33333333", "nothexno", 1))},
		{"route point zero t", ndjsonRoutes(t, 0, 0, 1, strings.Replace(routeLine, `"t":1718000002000`, `"t":0`, 1))},
		{"trailing route line", ndjsonRoutes(t, 0, 0, 1, routeLine, routeLine)},
		{"negative aggregateCount", ndjsonAggs(t, 0, 0, 0, -1)},
		{"aggregate unknown func", ndjsonAggs(t, 0, 0, 0, 1, strings.Replace(aggLine, `"func":"average"`, `"func":"median"`, 1))},
		{"aggregate unknown intervalUnit", ndjsonAggs(t, 0, 0, 0, 1, strings.Replace(aggLine, `"intervalUnit":"hour"`, `"intervalUnit":"fortnight"`, 1))},
		{"aggregate unknown deviceFilter", ndjsonAggs(t, 0, 0, 0, 1, strings.Replace(aggLine, `"deviceFilter":"watch"`, `"deviceFilter":"ipad"`, 1))},
		{"aggregate intervalValue zero", ndjsonAggs(t, 0, 0, 0, 1, strings.Replace(aggLine, `"intervalValue":1`, `"intervalValue":0`, 1))},
		{"aggregate missing type", ndjsonAggs(t, 0, 0, 0, 1, strings.Replace(aggLine, `"type":"HKQuantityTypeIdentifierHeartRate"`, `"type":""`, 1))},
		{"aggregate bucketEnd equals bucketStart", ndjsonAggs(t, 0, 0, 0, 1, strings.Replace(aggLine, `"bucketEnd":1718003600000`, `"bucketEnd":1718000000000`, 1))},
		{"aggregate bucketEnd before bucketStart", ndjsonAggs(t, 0, 0, 0, 1, strings.Replace(aggLine, `"bucketEnd":1718003600000`, `"bucketEnd":1717996400000`, 1))},
		{"missing aggregate line", ndjsonAggs(t, 0, 0, 0, 2, aggLine)},
		{"trailing aggregate line", ndjsonAggs(t, 0, 0, 0, 1, aggLine, aggLine)},
		{"negative activitySummaryCount", ndjsonAS(t, 0, 0, 0, 0, -1)},
		{"activity summary missing date", ndjsonAS(t, 0, 0, 0, 0, 1, strings.Replace(asLine, `"date":1718000000000`, `"date":0`, 1))},
		// Out-of-range epoch ms must be a 400, not a garbage-but-valid timestamptz.
		{"sample start beyond year 9999", ndjson(t, 1, 0, strings.Replace(hrSample, `"start":1718000000000`, `"start":253402300800000`, 1))},
		{"sample end before year 1", ndjson(t, 1, 0, strings.Replace(hrSample, `"end":1718000005000`, `"end":-62135596800001`, 1))},
		{"aggregate bucketEnd overflow", ndjsonAggs(t, 0, 0, 0, 1, strings.Replace(aggLine, `"bucketEnd":1718003600000`, `"bucketEnd":1e19`, 1))},
		{"activity summary date overflow", ndjsonAS(t, 0, 0, 0, 0, 1, strings.Replace(asLine, `"date":1718000000000`, `"date":9.3e18`, 1))},
		{"route point t overflow", ndjsonRoutes(t, 0, 0, 1, strings.Replace(routeLine, `"t":1718000002000`, `"t":9.3e18`, 1))},
		{"series point t overflow", ndjsonFull(t, 0, 0, 0, 0, 1, 0, strings.Replace(seriesLine, `"t":1718000002000`, `"t":9.3e18`, 1))},
		{"profile dob overflow", ndjsonFull(t, 0, 0, 0, 0, 0, 1, strings.Replace(profileLine, `631152000000`, `-9.3e18`, 1))},
		{"medication scheduledAt overflow", ndjson(t, 1, 0, strings.Replace(medSample, `"scheduledAt":1717999200000`, `"scheduledAt":9.3e18`, 1))},
		{"activity summary bad moveMode", ndjsonAS(t, 0, 0, 0, 0, 1, strings.Replace(asLine, `"moveMode":0`, `"moveMode":7`, 1))},
		{"missing activity summary line", ndjsonAS(t, 0, 0, 0, 0, 2, asLine)},
		{"trailing activity summary line", ndjsonAS(t, 0, 0, 0, 0, 1, asLine, asLine)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBatch(strings.NewReader(tc.body))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("expected ParseError, got %T: %v", err, err)
			}
		})
	}
}

func TestParseBatch_ToleratesBlankLines(t *testing.T) {
	body := strings.Replace(ndjson(t, 1, 1, hrSample, delLine), "\n"+delLine, "\n\n"+delLine, 1)
	b, err := ParseBatch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Samples) != 1 || len(b.Deletions) != 1 {
		t.Fatalf("got %d samples, %d deletions", len(b.Samples), len(b.Deletions))
	}
}

func TestParseBatch_EndDefaultsToStart(t *testing.T) {
	s := strings.Replace(hrSample, `"end":1718000005000,`, "", 1)
	b, err := ParseBatch(strings.NewReader(ndjson(t, 1, 0, s)))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if b.Samples[0].End != b.Samples[0].Start {
		t.Errorf("end = %v, want %v", b.Samples[0].End, b.Samples[0].Start)
	}
}

func TestMsToTime(t *testing.T) {
	got := msToTime(1718000000000)
	want := time.UnixMilli(1718000000000).UTC()
	if !got.Equal(want) {
		t.Errorf("msToTime = %v, want %v", got, want)
	}
	// Fractional milliseconds survive (within float64 precision at epoch scale).
	frac := msToTime(1718000000000.25)
	if d := frac.Sub(want) - 250*time.Microsecond; d < -10*time.Microsecond || d > 10*time.Microsecond {
		t.Errorf("fractional ms delta = %v, want ~250µs", frac.Sub(want))
	}
	if math.Abs(float64(timeToMS(got)-1718000000000)) > 0 {
		t.Errorf("timeToMS roundtrip = %d", timeToMS(got))
	}
	// The whole accepted range converts without int64 overflow: the old
	// ms*1e6 form wrapped past ±292 years and produced year 1677 for
	// Date.distantPast.
	if y := msToTime(minWireMS).Year(); y != 1 {
		t.Errorf("msToTime(minWireMS).Year() = %d, want 1", y)
	}
	if y := msToTime(maxWireMS).Year(); y != 9999 {
		t.Errorf("msToTime(maxWireMS).Year() = %d, want 9999", y)
	}
	if got := msToTime(-1500); !got.Equal(time.UnixMilli(-1500).UTC()) {
		t.Errorf("negative fractional second = %v", got)
	}
}

// TestMsToTimeTruncatesToStoredMicrosecond pins msToTime to the microsecond
// timestamptz keeps, truncated the way every stored row was (pgx drops the
// nanoseconds): an identical re-send must land on the row's exact start_ts.
func TestMsToTimeTruncatesToStoredMicrosecond(t *testing.T) {
	base := time.Date(2026, 9, 4, 15, 56, 51, 0, time.UTC)
	for _, tc := range []struct {
		ms   float64
		want time.Duration // past base
	}{
		// Float64 resolves ~0.24 µs at this epoch; stay clear of boundaries.
		{float64(base.UnixMilli()) + 776.0006, 776000 * time.Microsecond},
		{float64(base.UnixMilli()) + 776.0015, 776001 * time.Microsecond},
		{float64(base.UnixMilli()) + 776.0025, 776002 * time.Microsecond},
		{float64(base.UnixMilli()) + 776, 776000 * time.Microsecond},
	} {
		got := msToTime(tc.ms)
		if got.Nanosecond()%1000 != 0 {
			t.Errorf("msToTime(%f) = %v carries sub-microsecond nanoseconds", tc.ms, got)
		}
		if want := base.Add(tc.want); !got.Equal(want) {
			t.Errorf("msToTime(%f) = %v, want %v", tc.ms, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
	}
	// Before 1970 truncation is still a floor, like timestamptz's encoding.
	if got, want := msToTime(-0.0015), time.Unix(0, -2000).UTC(); !got.Equal(want) {
		t.Errorf("msToTime(-0.0015) = %v, want %v", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestValidWireMS(t *testing.T) {
	for _, ms := range []float64{0, 1718000000000, minWireMS, maxWireMS, -1} {
		if !validWireMS(ms) {
			t.Errorf("validWireMS(%v) = false, want true", ms)
		}
	}
	for _, ms := range []float64{minWireMS - 1, maxWireMS + 1, -6.2e13 * 1e3, 1e300, math.NaN(), math.Inf(1)} {
		if validWireMS(ms) {
			t.Errorf("validWireMS(%v) = true, want false", ms)
		}
	}
}

func TestIsUUID(t *testing.T) {
	if !isUUID("6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f") {
		t.Error("valid uuid rejected")
	}
	for _, s := range []string{"", "6f1c1f1e", "6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5g",
		"6f1c1f1e_2a3b-4c5d-8e9f-0a1b2c3d4e5f"} {
		if isUUID(s) {
			t.Errorf("invalid uuid accepted: %q", s)
		}
	}
}

// fakeStore lets handler tests run without a database.
type fakeStore struct {
	// Invoked from RecordRejection so a test can observe handler state at
	// the moment the rejection is persisted.
	onRejection  func()
	insertRes    IngestResult
	insertErr    error
	gotBatch     *Batch
	gotBytes     int64
	uuidsRes     []string
	routesRes    []RouteSummary
	routeRes     *RouteDetail
	metricsRes   []RouteMetricSeries
	gotType      string
	gotUserID    string
	gotUUID      string
	gotFilters   RouteFilters
	gotFrom      time.Time
	gotTo        time.Time
	rangeCalls   int
	routeCalls   int
	rejections   []IngestRejection
	rejectionErr error
	// Counts database liveness probes, so a test can assert /healthz does not
	// make one per request (see health.go).
	pings atomic.Int64
	// When set, InsertBatch returns what this does with the context it was
	// given, so a test can block an insert or inspect its deadline.
	onInsert func(ctx context.Context) error
}

func (f *fakeStore) InsertBatch(ctx context.Context, b *Batch, n int64) (IngestResult, error) {
	f.gotBatch, f.gotBytes = b, n
	if f.onInsert != nil {
		return f.insertRes, f.onInsert(ctx)
	}
	return f.insertRes, f.insertErr
}
func (f *fakeStore) RecordRejection(_ context.Context, rejection IngestRejection) error {
	if f.onRejection != nil {
		f.onRejection()
	}
	f.rejections = append(f.rejections, rejection)
	return f.rejectionErr
}
func (f *fakeStore) BeginSourceObservation(context.Context, string, int64, string) (string, error) {
	return "", errors.New("source observations require the real database store")
}
func (f *fakeStore) Stats(_ context.Context, scope recordScope) ([]TypeStats, error) {
	f.gotUserID = scope.userID
	return []TypeStats{}, nil
}
func (f *fakeStore) Digest(_ context.Context, scope recordScope, typ string, from, to time.Time) ([]DigestWindow, error) {
	f.gotUserID, f.gotType, f.gotFrom, f.gotTo = scope.userID, typ, from, to
	f.rangeCalls++
	return []DigestWindow{}, nil
}
func (f *fakeStore) UUIDs(_ context.Context, scope recordScope, typ string, from, to time.Time) ([]string, error) {
	f.gotUserID, f.gotType, f.gotFrom, f.gotTo = scope.userID, typ, from, to
	f.rangeCalls++
	return f.uuidsRes, nil
}
func (f *fakeStore) Routes(_ context.Context, userID string, filters RouteFilters) ([]RouteSummary, error) {
	f.gotUserID, f.gotFilters = userID, filters
	f.routeCalls++
	return f.routesRes, nil
}
func (f *fakeStore) Route(_ context.Context, userID, uuid string) (*RouteDetail, error) {
	f.gotUserID, f.gotUUID = userID, uuid
	f.routeCalls++
	return f.routeRes, nil
}
func (f *fakeStore) RouteMetrics(_ context.Context, userID, uuid string) ([]RouteMetricSeries, error) {
	f.gotUserID, f.gotUUID = userID, uuid
	f.routeCalls++
	return f.metricsRes, nil
}
func (f *fakeStore) Ping(ctx context.Context) error {
	f.pings.Add(1)
	if err := ctx.Err(); err != nil {
		return err // a real pool refuses a cancelled context
	}
	return nil
}

func newTestServer(fs *fakeStore) *Server {
	return newServer(fs, nil, "secret", true, false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func gzipBody(t *testing.T, s string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestParseBatch_RouteTooManyPoints(t *testing.T) {
	var pts strings.Builder
	for i := 0; i <= maxRoutePoints; i++ {
		if i > 0 {
			pts.WriteString(",")
		}
		fmt.Fprintf(&pts, `{"t":%d,"lat":37.0,"lon":-122.0}`, 1718000000000+i)
	}
	line := `{"route":{"workoutUUID":"33333333-3333-4333-8333-333333333333","points":[` + pts.String() + `]}}`
	_, err := ParseBatch(strings.NewReader(ndjsonRoutes(t, 0, 0, 1, line)))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("expected ParseError, got %T: %v", err, err)
	}
}

func TestParseBatch_RejectsExcessiveDeclaredCounts(t *testing.T) {
	tests := []string{
		fmt.Sprintf(`{"batchID":"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f","type":"x","sampleCount":%d,"deletionCount":0}`, maxDeclaredItems+1),
		fmt.Sprintf(`{"batchID":"6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f","type":"x","sampleCount":%d,"deletionCount":%d,"routeCount":1}`, maxDeclaredItems, maxDeclaredItems),
	}
	for _, body := range tests {
		if _, err := ParseBatch(strings.NewReader(body)); err == nil {
			t.Fatalf("expected declared-count error for %s", body)
		}
	}
}

func TestParseBatch_PreservesMaxBytesError(t *testing.T) {
	body := io.NopCloser(strings.NewReader(ndjson(t, 1, 0, hrSample)))
	limited := http.MaxBytesReader(httptest.NewRecorder(), body, 32)
	_, err := ParseBatch(limited)
	if err == nil {
		t.Fatal("expected size error")
	}
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		t.Fatalf("error %T (%v) does not preserve *http.MaxBytesError", err, err)
	}
	if got := batchParseStatus(err); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}
}

func TestHandleBatch_GzipRoundTrip(t *testing.T) {
	fs := &fakeStore{insertRes: IngestResult{Accepted: 1, Deleted: 1, Duplicates: 0}}
	srv := newTestServer(fs)

	body := gzipBody(t, ndjson(t, 1, 1, hrSample, delLine))
	req := httptest.NewRequest("POST", "/v1/batches", body)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Batch-ID", "6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["accepted"] != 1 || resp["deleted"] != 1 || resp["duplicates"] != 0 {
		t.Errorf("resp = %v", resp)
	}
	if fs.gotBatch == nil || len(fs.gotBatch.Samples) != 1 || len(fs.gotBatch.Deletions) != 1 {
		t.Errorf("store got batch = %+v", fs.gotBatch)
	}
	if fs.gotBytes == 0 {
		t.Error("expected nonzero wire bytes")
	}
}

func TestHandleBatch_PersistsParseRejection(t *testing.T) {
	fs := &fakeStore{}
	srv := newTestServer(fs)

	body := gzipBody(t, hdrLine[:len(hdrLine)-1])
	req := httptest.NewRequest("POST", "/v1/batches", body)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Batch-ID", "6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f")
	req.Header.Set("X-Wake-ID", "11111111-1111-4111-8111-111111111111")
	req.Header.Set("X-Wake-Trigger", "observer")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(fs.rejections) != 1 {
		t.Fatalf("rejections = %d, want 1", len(fs.rejections))
	}
	got := fs.rejections[0]
	if got.Stage != "parse" || got.Status != http.StatusBadRequest || got.Trigger != "observer" || got.Bytes == 0 {
		t.Fatalf("rejection = %+v", got)
	}
}

func TestHandleBatch_RoutePointsInResponse(t *testing.T) {
	fs := &fakeStore{insertRes: IngestResult{Accepted: 1, RoutePoints: 2}}
	srv := newTestServer(fs)

	body := gzipBody(t, ndjsonRoutes(t, 1, 0, 1, workoutSample, routeLine))
	req := httptest.NewRequest("POST", "/v1/batches", body)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Encoding", "gzip")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["accepted"] != 1 || resp["routePoints"] != 2 {
		t.Errorf("resp = %v", resp)
	}
	if fs.gotBatch == nil || len(fs.gotBatch.Routes) != 1 {
		t.Errorf("store got batch = %+v", fs.gotBatch)
	}
}

func TestHandleBatch_SeriesPointsInResponse(t *testing.T) {
	fs := &fakeStore{insertRes: IngestResult{SeriesPoints: 2}}
	srv := newTestServer(fs)

	body := gzipBody(t, ndjsonFull(t, 0, 0, 0, 0, 1, 0, seriesLine))
	req := httptest.NewRequest("POST", "/v1/batches", body)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Encoding", "gzip")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["seriesPoints"] != 2 {
		t.Errorf("resp = %v", resp)
	}
	if fs.gotBatch == nil || len(fs.gotBatch.Series) != 1 {
		t.Errorf("store got batch = %+v", fs.gotBatch)
	}
}

func TestHandleBatch_AggregateSamplesInResponse(t *testing.T) {
	fs := &fakeStore{insertRes: IngestResult{AggregateSamples: 2}}
	srv := newTestServer(fs)

	body := gzipBody(t, ndjsonAggs(t, 0, 0, 0, 2, aggLine, aggLineNull))
	req := httptest.NewRequest("POST", "/v1/batches", body)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Encoding", "gzip")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["aggregateSamples"] != 2 {
		t.Errorf("resp = %v", resp)
	}
	if fs.gotBatch == nil || len(fs.gotBatch.Aggregates) != 2 {
		t.Errorf("store got batch = %+v", fs.gotBatch)
	}
}

func TestHandleBatch_ActivitySummariesInResponse(t *testing.T) {
	fs := &fakeStore{insertRes: IngestResult{ActivitySummaries: 2}}
	srv := newTestServer(fs)

	body := gzipBody(t, ndjsonAS(t, 0, 0, 0, 0, 2, asLine, asLineNull))
	req := httptest.NewRequest("POST", "/v1/batches", body)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Encoding", "gzip")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["activitySummaries"] != 2 {
		t.Errorf("resp = %v", resp)
	}
	if fs.gotBatch == nil || len(fs.gotBatch.ActivitySummaries) != 2 {
		t.Errorf("store got batch = %+v", fs.gotBatch)
	}
}

func TestHandleDigest_Params(t *testing.T) {
	fs := &fakeStore{}
	srv := newTestServer(fs)

	do := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec
	}

	for _, target := range []string{
		"/v1/digest",
		"/v1/digest?type=HKQuantityTypeIdentifierHeartRate",
		"/v1/digest?type=HKQuantityTypeIdentifierHeartRate&from=abc&to=1718000000000",
		"/v1/digest?type=HKQuantityTypeIdentifierHeartRate&from=1718000000000&to=1718000000000",
	} {
		if rec := do(target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}

	rec := do("/v1/digest?type=HKQuantityTypeIdentifierHeartRate&from=1700000000000&to=1718000000000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %q, want []", rec.Body.String())
	}
	if fs.gotType != "HKQuantityTypeIdentifierHeartRate" ||
		fs.gotFrom.UnixMilli() != 1700000000000 || fs.gotTo.UnixMilli() != 1718000000000 {
		t.Errorf("store got type=%q from=%v to=%v", fs.gotType, fs.gotFrom, fs.gotTo)
	}
	if fs.gotUserID != defaultUserID {
		t.Errorf("store got user=%q, want default %q", fs.gotUserID, defaultUserID)
	}
}

func TestHandleUUIDs_RangeGuard(t *testing.T) {
	fs := &fakeStore{uuidsRes: []string{"11111111-1111-4111-8111-111111111111"}}
	srv := newTestServer(fs)

	do := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec
	}

	from := int64(1700000000000)
	over := from + (35*24*3600*1000 + 1)
	rec := do(fmt.Sprintf("/v1/uuids?type=HKQuantityTypeIdentifierHeartRate&from=%d&to=%d", from, over))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-range status = %d, want 400", rec.Code)
	}
	if fs.rangeCalls != 0 {
		t.Errorf("store called despite range guard")
	}

	exact := from + 35*24*3600*1000
	rec = do(fmt.Sprintf("/v1/uuids?type=HKQuantityTypeIdentifierHeartRate&from=%d&to=%d", from, exact))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string][]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp["uuids"]) != 1 || resp["uuids"][0] != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("resp = %v", resp)
	}
}

func TestHandleRoutes_ParamsAndResponse(t *testing.T) {
	fs := &fakeStore{routesRes: []RouteSummary{{
		UUID:            "11111111-1111-4111-8111-111111111111",
		StartTs:         "2026-06-01T15:00:00Z",
		EndTs:           "2026-06-01T15:30:00Z",
		ActivityType:    "HKWorkoutActivityTypeRunning",
		RoutePointCount: 42,
		Bounds:          Bounds{MinLat: 37, MaxLat: 38, MinLon: -123, MaxLon: -122},
	}}}
	srv := newTestServer(fs)

	req := httptest.NewRequest("GET", "/v1/routes?start=2026-06-01&limit=12&offset=3&minDistanceM=1000", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Routes []RouteSummary `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Routes) != 1 || resp.Routes[0].UUID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("resp = %+v", resp)
	}
	if fs.gotFilters.Start == nil || fs.gotFilters.Start.Format("2006-01-02") != "2026-06-01" ||
		fs.gotFilters.Limit != 12 || fs.gotFilters.Offset != 3 ||
		fs.gotFilters.MinDistanceM == nil || *fs.gotFilters.MinDistanceM != 1000 {
		t.Errorf("filters = %+v", fs.gotFilters)
	}

	req = httptest.NewRequest("GET", "/v1/routes?start=not-a-date", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad start status = %d, want 400", rec.Code)
	}
}

func TestHandleRouteAndMetrics(t *testing.T) {
	uuid := "11111111-1111-4111-8111-111111111111"
	fs := &fakeStore{
		routeRes: &RouteDetail{Workout: RouteSummary{UUID: uuid}, Points: []RouteGPSPoint{{Ts: "2026-06-01T15:00:00Z", Lat: 37, Lon: -122}}},
		metricsRes: []RouteMetricSeries{{
			Identifier: "HKQuantityTypeIdentifierHeartRate",
			Points:     []MetricPoint{{Ts: "2026-06-01T15:00:00Z", Value: 140}},
		}},
	}
	srv := newTestServer(fs)

	req := httptest.NewRequest("GET", "/v1/routes/"+uuid, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fs.gotUUID != uuid {
		t.Errorf("detail uuid = %q", fs.gotUUID)
	}

	req = httptest.NewRequest("GET", "/v1/routes/"+uuid+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Metrics []RouteMetricSeries `json:"metrics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Metrics) != 1 || resp.Metrics[0].Identifier != "HKQuantityTypeIdentifierHeartRate" {
		t.Errorf("metrics = %+v", resp.Metrics)
	}
}

func TestAuthenticatedReadsUseRequestedUser(t *testing.T) {
	userID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	uuid := "11111111-1111-4111-8111-111111111111"
	targets := []string{
		"/v1/stats",
		"/v1/digest?type=x&from=1700000000000&to=1700000001000",
		"/v1/uuids?type=x&from=1700000000000&to=1700000001000",
		"/v1/routes",
		"/v1/routes/" + uuid,
		"/v1/routes/" + uuid + "/metrics",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			fs := &fakeStore{}
			srv := newTestServer(fs)
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.Header.Set("Authorization", "Bearer secret")
			req.Header.Set("X-User-ID", userID)
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)
			if rec.Code >= 500 {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if fs.gotUserID != userID {
				t.Fatalf("store user = %q, want %q", fs.gotUserID, userID)
			}
		})
	}
}

func TestAuthenticatedReadsRejectMalformedUser(t *testing.T) {
	fs := &fakeStore{}
	srv := newTestServer(fs)
	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-User-ID", "not-a-uuid")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if fs.gotUserID != "" {
		t.Fatalf("store called with user %q", fs.gotUserID)
	}
}

func TestHandleDigestAndUUIDs_RequireAuth(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	for _, target := range []string{
		"/v1/digest?type=x&from=0&to=1",
		"/v1/uuids?type=x&from=0&to=1",
		"/v1/routes",
	} {
		req := httptest.NewRequest("GET", target, nil)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", target, rec.Code)
		}
	}
}

func TestHandleBatch_IdentityEncoding(t *testing.T) {
	srv := newTestServer(&fakeStore{insertRes: IngestResult{Accepted: 1}})
	req := httptest.NewRequest("POST", "/v1/batches", strings.NewReader(ndjson(t, 1, 0, hrSample)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleBatch_BadToken(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	for _, auth := range []string{"", "Bearer wrong", "secret"} {
		req := httptest.NewRequest("POST", "/v1/batches", strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status = %d, want 401", auth, rec.Code)
		}
	}
}

func TestHandleBatch_MalformedBody(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	req := httptest.NewRequest("POST", "/v1/batches", strings.NewReader("definitely not ndjson"))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleBatch_BadGzip(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	req := httptest.NewRequest("POST", "/v1/batches", strings.NewReader("not gzip at all"))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleBatch_InsertFailureIs500(t *testing.T) {
	srv := newTestServer(&fakeStore{insertErr: errors.New("db down")})
	req := httptest.NewRequest("POST", "/v1/batches", gzipBody(t, ndjson(t, 1, 0, hrSample)))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHealthz_NoAuthRequired(t *testing.T) {
	srv := newTestServer(&fakeStore{})
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Errorf("resp = %v", resp)
	}
}

// The rejection row is observability; it must never delay the error response.
// With the database down, RecordRejection blocks on pool acquire for its full
// 2 s budget, and a retrying client paid that on every attempt when the
// server was already struggling. Every rejection path writes first.
func TestHandleBatch_RespondsBeforeRecordingRejection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		build  func(t *testing.T, fs *fakeStore) *http.Request
	}{
		{"parse", http.StatusBadRequest, func(t *testing.T, _ *fakeStore) *http.Request {
			req := httptest.NewRequest("POST", "/v1/batches", gzipBody(t, hdrLine[:len(hdrLine)-1]))
			req.Header.Set("Content-Encoding", "gzip")
			return req
		}},
		{"gzip", http.StatusBadRequest, func(t *testing.T, _ *fakeStore) *http.Request {
			req := httptest.NewRequest("POST", "/v1/batches", strings.NewReader("not gzip"))
			req.Header.Set("Content-Encoding", "gzip")
			return req
		}},
		{"encoding", http.StatusBadRequest, func(t *testing.T, _ *fakeStore) *http.Request {
			req := httptest.NewRequest("POST", "/v1/batches", strings.NewReader(ndjson(t, 0, 0)))
			req.Header.Set("Content-Encoding", "br")
			return req
		}},
		{"wake", http.StatusBadRequest, func(t *testing.T, _ *fakeStore) *http.Request {
			req := httptest.NewRequest("POST", "/v1/batches", strings.NewReader(ndjson(t, 0, 0)))
			req.Header.Set("X-Wake-ID", "not-a-uuid")
			return req
		}},
		{"protocol", http.StatusBadRequest, func(t *testing.T, _ *fakeStore) *http.Request {
			req := httptest.NewRequest("POST", "/v1/batches", strings.NewReader(ndjson(t, 0, 0)))
			req.Header.Set("X-Puls-Protocol", "2")
			return req
		}},
		{"insert", http.StatusInternalServerError, func(t *testing.T, fs *fakeStore) *http.Request {
			fs.insertErr = errors.New("boom")
			return httptest.NewRequest("POST", "/v1/batches", strings.NewReader(ndjson(t, 0, 0)))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			rec := httptest.NewRecorder()
			fs.onRejection = func() {
				if rec.Code != tc.status {
					t.Errorf("rejection recorded before the response was written (recorder code %d)", rec.Code)
				}
			}
			req := tc.build(t, fs)
			req.Header.Set("Authorization", "Bearer secret")
			newTestServer(fs).routes().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if len(fs.rejections) != 1 || fs.rejections[0].Stage != tc.name {
				t.Fatalf("rejections = %+v, want one at stage %q", fs.rejections, tc.name)
			}
		})
	}
}

// sample_types.unit is the type's canonical unit. A duration aggregate's "s"
// is the series' unit, not the type's, and must not register the type; a
// sample line's unit wins over any aggregate line's for the same identifier.
func TestBatchTypeInfos_UnitPrecedence(t *testing.T) {
	sec, cpm, cnt := "s", "count/min", "count"
	b := &Batch{}
	b.Samples = []Sample{{Type: "HKQuantityTypeIdentifierHeartRate", Kind: "quantity", Unit: &cpm}}
	b.Aggregates = make([]AggregateLine, 3)
	b.Aggregates[0].Aggregate.Type = "HKQuantityTypeIdentifierHeartRate"
	b.Aggregates[0].Aggregate.Func = "duration"
	b.Aggregates[0].Aggregate.Unit = &sec
	b.Aggregates[1].Aggregate.Type = "HKQuantityTypeIdentifierAppleExerciseTime"
	b.Aggregates[1].Aggregate.Func = "duration"
	b.Aggregates[1].Aggregate.Unit = &sec
	b.Aggregates[2].Aggregate.Type = "HKQuantityTypeIdentifierStepCount"
	b.Aggregates[2].Aggregate.Func = "sum"
	b.Aggregates[2].Aggregate.Unit = &cnt

	infos := batchTypeInfos(b)
	if got := infos["HKQuantityTypeIdentifierHeartRate"]; got.unit == nil || *got.unit != cpm || !got.canonicalUnit {
		t.Errorf("heart rate = %+v, want canonical count/min from the sample line", got)
	}
	if got := infos["HKQuantityTypeIdentifierAppleExerciseTime"]; got.unit != nil || got.canonicalUnit || got.kind != "quantity" {
		t.Errorf("exercise time = %+v, want a quantity row with no unit from a duration aggregate", got)
	}
	if got := infos["HKQuantityTypeIdentifierStepCount"]; got.unit == nil || *got.unit != cnt || got.canonicalUnit {
		t.Errorf("steps = %+v, want non-canonical count from the sum aggregate", got)
	}
}

// A lost deadlock (40P01) or serialization race (40001) is re-run server-side;
// everything else — including other Postgres errors — is not.
func TestIsRetryableTxError(t *testing.T) {
	deadlock := fmt.Errorf("ensure types: %w", &pgconn.PgError{Code: "40P01", Message: "deadlock detected"})
	serialization := fmt.Errorf("insert quantity: %w", &pgconn.PgError{Code: "40001"})
	for _, err := range []error{deadlock, serialization} {
		if !isRetryableTxError(err) {
			t.Errorf("isRetryableTxError(%v) = false, want true", err)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("context canceled"),
		fmt.Errorf("reserve batch: %w", &pgconn.PgError{Code: "23505"}),
		fmt.Errorf("x: %w", &pgconn.PgError{Code: "53400"}),
		context.Canceled,
	} {
		if isRetryableTxError(err) {
			t.Errorf("isRetryableTxError(%v) = true, want false", err)
		}
	}
}

// --- Protocol version negotiation (PROTO-1) and capabilities (PROTO-6) ---

func intPtr(v int) *int { return &v }

// versioned builds a one-sample-slot body whose header declares schemaVersion
// as the given JSON literal.
func versioned(t *testing.T, schemaVersion string, lines ...string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(hdrLineVersioned, schemaVersion, len(lines), 0))
	for _, l := range lines {
		sb.WriteString("\n")
		sb.WriteString(l)
	}
	sb.WriteString("\n")
	return sb.String()
}

func TestParseBatch_SchemaVersion(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    *int
		wantErr string // substring of the ProtocolVersionError; "" means accepted
	}{
		{"legacy header without schemaVersion", ndjson(t, 0, 0), nil, ""},
		{"explicit null is legacy", versioned(t, "null"), nil, ""},
		{"version 1", versioned(t, "1", hrSample), intPtr(1), ""},
		{"version 2", versioned(t, "2", hrSample), nil, "schemaVersion 2"},
		{"version 0", versioned(t, "0"), nil, "schemaVersion 0"},
		// The version is checked before anything else in the header: a
		// future client whose header no longer carries today's required
		// fields still gets the negotiation error, not "missing batchID".
		{"version checked before the rest of the header", `{"schemaVersion":7}` + "\n", nil, "schemaVersion 7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := ParseBatch(strings.NewReader(tc.body))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				var pve *ProtocolVersionError
				if !errors.As(err, &pve) {
					t.Fatalf("error %T (%v), want *ProtocolVersionError", err, err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "supported versions [1]") {
					t.Fatalf("error = %q, want it to mention %q and the supported set", err, tc.wantErr)
				}
				if got := batchParseStatus(err); got != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBatch: %v", err)
			}
			switch {
			case tc.want == nil && b.Header.SchemaVersion != nil:
				t.Fatalf("schemaVersion = %d, want absent", *b.Header.SchemaVersion)
			case tc.want != nil && (b.Header.SchemaVersion == nil || *b.Header.SchemaVersion != *tc.want):
				t.Fatalf("schemaVersion = %v, want %d", b.Header.SchemaVersion, *tc.want)
			}
		})
	}
}

func TestParseProtocolHeader(t *testing.T) {
	cases := []struct {
		in      string
		want    *int
		wantErr bool
	}{
		{"", nil, false},
		{"1", intPtr(1), false},
		{" 1 ", intPtr(1), false},
		{"2", nil, true},
		{"0", nil, true},
		{"-1", nil, true},
		{"abc", nil, true},
		{"1.0", nil, true},
	}
	for _, tc := range cases {
		got, err := parseProtocolHeader(tc.in)
		if tc.wantErr {
			var pve *ProtocolVersionError
			if !errors.As(err, &pve) {
				t.Errorf("%q: error %T (%v), want *ProtocolVersionError", tc.in, err, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
			continue
		}
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%q: got %d, want nil", tc.in, *got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("%q: got %v, want %d", tc.in, got, *tc.want)
		}
	}
}

func TestReconcileProtocolVersions(t *testing.T) {
	cases := []struct {
		header, body *int
		want         int
		wantErr      bool
	}{
		{nil, nil, 1, false},
		{intPtr(1), nil, 1, false},
		{nil, intPtr(1), 1, false},
		{intPtr(1), intPtr(1), 1, false},
		{intPtr(1), intPtr(2), 0, true},
		{intPtr(2), intPtr(1), 0, true},
	}
	for _, tc := range cases {
		got, err := reconcileProtocolVersions(tc.header, tc.body)
		if tc.wantErr {
			var pve *ProtocolVersionError
			if !errors.As(err, &pve) {
				t.Errorf("header=%v body=%v: error %T (%v), want *ProtocolVersionError", tc.header, tc.body, err, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("header=%v body=%v: got %d, %v; want %d", tc.header, tc.body, got, err, tc.want)
		}
	}
}

func postBatch(t *testing.T, srv *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/batches", gzipBody(t, body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Batch-ID", "6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// A version this server does not speak — in the body, in the request header,
// or the two contradicting each other — is a 400 with the fixed negotiation
// body, costs no database work, and is recorded like any other rejection.
func TestHandleBatch_UnsupportedProtocolVersion(t *testing.T) {
	const wantBody = `{"error":"unsupported protocol version","supportedVersions":[1]}`
	cases := []struct {
		name       string
		body       string
		header     string // X-Puls-Protocol; "" omits it
		bodyUnread bool   // rejected on the request header alone
	}{
		{"schemaVersion 2 in body", versioned(t, "2", hrSample), "", false},
		{"X-Puls-Protocol 2", ndjson(t, 1, 0, hrSample), "2", true},
		{"body 1 header 2", versioned(t, "1", hrSample), "2", true},
		{"body 2 header 1", versioned(t, "2", hrSample), "1", false},
		{"header not an integer", ndjson(t, 1, 0, hrSample), "1.0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{insertRes: IngestResult{Accepted: 1}}
			headers := map[string]string{}
			if tc.header != "" {
				headers["X-Puls-Protocol"] = tc.header
			}
			rec := postBatch(t, newTestServer(fs), tc.body, headers)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got := strings.TrimSpace(rec.Body.String()); got != wantBody {
				t.Fatalf("body = %s, want %s", got, wantBody)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			if fs.gotBatch != nil {
				t.Fatal("InsertBatch was called for a rejected protocol version")
			}
			if len(fs.rejections) != 1 {
				t.Fatalf("rejections = %d, want 1", len(fs.rejections))
			}
			rej := fs.rejections[0]
			if rej.Stage != "protocol" || rej.Status != http.StatusBadRequest {
				t.Errorf("rejection = %+v, want stage protocol / 400", rej)
			}
			if rej.ErrorMessage != "unsupported protocol version" {
				t.Errorf("rejection message = %q, want the fixed protocol category", rej.ErrorMessage)
			}
			if tc.bodyUnread && rej.Bytes != 0 {
				t.Errorf("rejection bytes = %d, want 0: the body must not be read when the request header is refused", rej.Bytes)
			}
		})
	}
}

// Every combination that names version 1 — or names nothing, as clients that
// predate versioning do — is accepted.
func TestHandleBatch_ProtocolVersionOneAccepted(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		header string
	}{
		{"legacy body, no header", ndjson(t, 1, 0, hrSample), ""},
		{"schemaVersion 1, no header", versioned(t, "1", hrSample), ""},
		{"legacy body, header 1", ndjson(t, 1, 0, hrSample), "1"},
		{"schemaVersion 1, header 1", versioned(t, "1", hrSample), "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{insertRes: IngestResult{Accepted: 1}}
			headers := map[string]string{}
			if tc.header != "" {
				headers["X-Puls-Protocol"] = tc.header
			}
			rec := postBatch(t, newTestServer(fs), tc.body, headers)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if fs.gotBatch == nil || len(fs.gotBatch.Samples) != 1 {
				t.Fatalf("store got batch = %+v", fs.gotBatch)
			}
			if len(fs.rejections) != 0 {
				t.Fatalf("rejections = %+v, want none", fs.rejections)
			}
		})
	}
}

// The app's fallback connection probe (for receivers without
// /v1/capabilities) is a header-only batch: every count zero, reason
// "manual". It must parse, reach the store, and come back 200 with all-zero
// counts so the app can tell "reachable and authenticated" from a 4xx.
func TestHandleBatch_ConnectionProbe(t *testing.T) {
	fs := &fakeStore{}
	rec := postBatch(t, newTestServer(fs), probeLine+"\n", map[string]string{"X-Puls-Protocol": "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"accepted", "deleted", "duplicates", "routePoints", "seriesPoints", "aggregateSamples", "activitySummaries"} {
		v, ok := resp[key]
		if !ok || v != 0 {
			t.Errorf("resp[%q] = %d (present %v), want 0", key, v, ok)
		}
	}
	if len(resp) != 7 {
		t.Errorf("resp = %v, want exactly the seven count keys", resp)
	}
	b := fs.gotBatch
	if b == nil {
		t.Fatal("InsertBatch was not called")
	}
	if len(b.Samples)+len(b.Deletions)+len(b.Routes)+len(b.Series)+len(b.Aggregates)+len(b.ActivitySummaries) != 0 || b.Profile != nil {
		t.Errorf("probe batch carried data lines: %+v", b)
	}
	if b.Header.Reason != "manual" || b.Header.SchemaVersion == nil || *b.Header.SchemaVersion != 1 || b.Header.ClientVersion != "0.1.0 (1)" {
		t.Errorf("probe header = %+v", b.Header)
	}
	if len(fs.rejections) != 0 {
		t.Errorf("rejections = %+v, want none", fs.rejections)
	}
}

func TestHandleCapabilities(t *testing.T) {
	srv := newTestServer(&fakeStore{})

	for _, auth := range []string{"", "Bearer wrong", "secret"} {
		req := httptest.NewRequest("GET", "/v1/capabilities", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status = %d, want 401", auth, rec.Code)
		}
	}

	req := httptest.NewRequest("GET", "/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	const want = `{"protocolVersions":[1],"features":["batches","stats","digest","uuids","aggregates","activitySummaries","routes","series","profile"],"server":"puls-ingest","version":"dev"}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
}

func TestServerVersion(t *testing.T) {
	old := buildVersion
	t.Cleanup(func() { buildVersion = old })

	buildVersion = ""
	if got := serverVersion(); got != "dev" {
		t.Errorf("serverVersion() with no build commit = %q, want dev", got)
	}
	buildVersion = "0123abcd"
	if got := serverVersion(); got != "0123abcd" {
		t.Errorf("serverVersion() = %q, want the build commit", got)
	}
}

func TestBatchHeaderTimeZone(t *testing.T) {
	for _, zone := range []string{"", "UTC", "America/Los_Angeles", "Asia/Kolkata", "Local", "Unknown/Nowhere", strings.Repeat("x", 101)} {
		t.Run(zone, func(t *testing.T) {
			body := ndjson(t, 0, 0)
			encoded, _ := json.Marshal(zone)
			body = strings.Replace(body, "{", `{"timeZoneID":`+string(encoded)+`,`, 1)
			batch, err := ParseBatch(strings.NewReader(body))
			valid := zone == "" || zone == "UTC" || zone == "America/Los_Angeles" || zone == "Asia/Kolkata"
			if valid {
				if err != nil {
					t.Fatal(err)
				}
				if batch.Header.TimeZoneID != zone {
					t.Fatalf("zone = %q", batch.Header.TimeZoneID)
				}
			} else if err == nil {
				t.Fatal("invalid zone accepted")
			}
		})
	}
}

func TestParseBatch_MedicationIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		valid       bool
	}{
		{"legacy omitted", "", true},
		{"explicit null", `"medicationID":null,`, true},
		{"UUID", `"medicationID":"d1000000-0000-4000-8000-000000000001",`, true},
		{"display name is not identity", `"medicationID":"Ibuprofen",`, false},
		{"empty string", `"medicationID":"",`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample := strings.Replace(medSample, `"medication":"Ibuprofen",`, `"medication":"Ibuprofen",`+tc.field, 1)
			b, err := ParseBatch(strings.NewReader(ndjson(t, 1, 0, sample)))
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid medication identity accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			id := b.Samples[0].MedicationDose.MedicationID
			if tc.name == "UUID" {
				if id == nil || *id != "d1000000-0000-4000-8000-000000000001" {
					t.Fatalf("identity = %v", id)
				}
			} else if id != nil {
				t.Fatalf("legacy identity = %v", *id)
			}
		})
	}
}
