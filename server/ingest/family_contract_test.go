package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFamilyStoreRefusesUnvalidatedPersonalBatch(t *testing.T) {
	batch, err := ParseBatch(strings.NewReader(probeLine + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	batch.Header.DeviceTokenID = 1
	if _, err := NewStore(nil).InsertBatch(context.Background(), batch, 0); err == nil {
		t.Fatal("personal store accepted an unvalidated batch")
	}
}

func TestFamilyProtocolMatchesPublishedContract(t *testing.T) {
	contract, err := familyProtocol()
	if err != nil {
		t.Fatal(err)
	}
	if len(contract.schemas) != 11 {
		t.Fatalf("compiled schemas=%d, want 11", len(contract.schemas))
	}
	entries, err := protocolFiles.ReadDir("protocol")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run(entry.Name(), func(t *testing.T) {
			embedded, err := protocolFiles.ReadFile("protocol/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			dir := "../../docs/protocol/schema"
			if entry.Name() == "catalog.json" {
				dir = "../../docs/protocol"
			}
			published, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(embedded, published) {
				t.Fatal("embedded contract differs from the published source; run go generate")
			}
		})
	}
}

func TestParseFamilyBatchPublishedCorpus(t *testing.T) {
	paths, err := filepath.Glob("../../docs/protocol/fixtures/*.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 8 {
		t.Fatalf("fixtures=%d, want 8", len(paths))
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			batch, err := ParseFamilyBatch(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if !batch.familyValidated || len(batch.bodyDigest) != 32 {
				t.Fatal("complete source batch was not validated and hashed")
			}
		})
	}
}

func TestParseFamilyBatchSourceContract(t *testing.T) {
	for _, tc := range []struct {
		name, sample string
		valid        bool
	}{
		{"canonical quantity", hrSample, true},
		{"wrong unit", strings.Replace(hrSample, "count/min", "kg", 1), false},
		{"missing value", strings.Replace(hrSample, `"value":62.5,`, "", 1), false},
		{"null value", strings.Replace(hrSample, `"value":62.5`, `"value":null`, 1), false},
		{"wrong catalog kind", strings.Replace(hrSample, `"kind":"quantity"`, `"kind":"category"`, 1), false},
		{"unknown type", strings.Replace(hrSample, "HKQuantityTypeIdentifierHeartRate", "HKFutureUnverifiedType", 1), false},
		{"missing workout duration", strings.Replace(workoutSample, `"duration":3600.5,`, "", 1), false},
		{"missing event start", strings.Replace(workoutSampleRich, `"start":1718001800000`, `"future":1718001800000`, 1), false},
		{"full workout", workoutSampleRich, true},
		{"category", catSample, true},
		{"heartbeat", hbSample, true},
		{"ECG", ecgSample, true},
		{"mind", somSample, true},
		{"medication", medSample, true},
		{"unknown valid field", strings.TrimSuffix(hrSample, "}") + `,"future":{"safe":true}}`, true},
		{"oversized source fact", strings.Replace(hrSample, "Apple Watch", strings.Repeat("a", 257), 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch, err := ParseFamilyBatch(strings.NewReader(ndjson(t, 1, 0, tc.sample)))
			if (err == nil) != tc.valid {
				t.Fatalf("accepted=%v, want %v; err=%v", err == nil, tc.valid, err)
			}
			if err != nil && batch != nil {
				t.Fatal("invalid source returned a usable batch")
			}
		})
	}
}

func TestFamilyTimeFacts(t *testing.T) {
	for _, tc := range []struct {
		name, zone, source, confidence string
		offset                         int32
		valid                          bool
	}{
		{"explicit UTC", "Etc/UTC", "healthkit_metadata", "recorded", 0, true},
		{"contradictory UTC", "Etc/UTC", "healthkit_metadata", "recorded", 3600, false},
		{"summer", "America/Los_Angeles", "device_current", "inferred", -25200, true},
		{"wrong seasonal offset", "America/Los_Angeles", "device_current", "inferred", -28800, false},
		{"unknown provenance", "Etc/UTC", "unknown", "unavailable", 3600, true},
		{"unknown source zone", "Future/Unknown", "healthkit_metadata", "recorded", 3600, true},
		{"host zone never substituted", "Local", "healthkit_metadata", "recorded", 3600, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			context := &TemporalContext{TimeZoneID: tc.zone, Source: tc.source, Confidence: tc.confidence, UTCOffsetSeconds: tc.offset}
			if got := familyTimeAgrees(1718000000123.456, context); got != tc.valid {
				t.Fatalf("consistent=%v, want %v", got, tc.valid)
			}
		})
	}
}
