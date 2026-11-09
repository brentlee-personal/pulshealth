package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFamilyRecordContentPreservesSourceFacts(t *testing.T) {
	for _, line := range []string{hrSample, catSample, workoutSampleRich, hbSample, ecgSample, somSample, medSample} {
		batch, err := ParseFamilyBatch(strings.NewReader(ndjson(t, 1, 0, line)))
		if err != nil {
			t.Fatal(err)
		}
		sample := &batch.Samples[0]
		t.Run(sample.Kind, func(t *testing.T) {
			content, digest, data, err := normalizeFamilySample(sample)
			if err != nil {
				t.Fatal(err)
			}
			if len(digest) != 32 || content.SourceRecordID != strings.ToLower(sample.UUID) ||
				content.RecordType != sample.Type || content.StartAt != "2024-06-10T06:13:20Z" ||
				!reflect.DeepEqual(content.OriginalValue, content.Value) {
				t.Fatal("source content, value, time or identity was lost")
			}
			if content.SourceRevision != unavailableFamilyFact("not_provided") {
				t.Fatal("creator app version was presented as a record revision")
			}
			var facts map[string]json.RawMessage
			if err := json.Unmarshal(data, &facts); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"value", "original_value", "metadata", "member_id", "credential_id", "batch_id", "received_at"} {
				if _, ok := facts[key]; ok {
					t.Fatalf("index duplicated or hashed excluded field %s", key)
				}
			}
			if sample.Kind != "quantity" && (content.Unit != nil || content.OriginalUnit != nil) {
				t.Fatal("invented a unit for structured or category data")
			}
		})
	}
}

func TestFamilyRecordDigestIgnoresTransportButDetectsSourceChanges(t *testing.T) {
	fingerprint := func(body string) []byte {
		t.Helper()
		batch, err := ParseFamilyBatch(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, digest, _, err := normalizeFamilySample(&batch.Samples[0])
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	body := ndjson(t, 1, 0, hrSample)
	base := fingerprint(body)
	transport := strings.Replace(body, "dev-1", "another-install", 1)
	transport = strings.Replace(transport, "6f1c1f1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f", "00000000-0000-4000-8000-000000000001", 1)
	transport = strings.Replace(transport, `"value":62.5`, `"value":62.500,"future":{"ignored":true}`, 1)
	if !bytes.Equal(base, fingerprint(transport)) {
		t.Fatal("transport or unknown fields changed the record digest")
	}
	for _, changed := range []string{
		strings.Replace(body, `"value":62.5`, `"value":62.6`, 1),
		strings.Replace(body, `"start":1718000000000`, `"start":1718000000001`, 1),
		strings.Replace(body, `"device":"Apple Watch"`, `"device":"Another Watch"`, 1),
	} {
		if bytes.Equal(base, fingerprint(changed)) {
			t.Fatal("changed source content kept the same digest")
		}
	}
}

func TestFamilyRecordIndependentTimeFacts(t *testing.T) {
	batch, err := ParseFamilyBatch(strings.NewReader(ndjson(t, 1, 0, hrSample)))
	if err != nil {
		t.Fatal(err)
	}
	sample := &batch.Samples[0]
	sample.StartContext = &TemporalContext{TimeZoneID: "Etc/UTC", UTCOffsetSeconds: 0, Source: "healthkit_metadata", Confidence: "recorded"}
	sample.EndContext = &TemporalContext{TimeZoneID: "Asia/Shanghai", UTCOffsetSeconds: 28800, Source: "device_current", Confidence: "inferred"}
	content, _, _, err := normalizeFamilySample(sample)
	if err != nil {
		t.Fatal(err)
	}
	if content.SourceOffset.Value != int32(0) || content.SourceOffset.Confidence != "explicit" ||
		content.SourceEndOffset.Value != int32(28800) || content.SourceEndOffset.Confidence != "inferred" {
		t.Fatal("start and end source facts were merged")
	}
	zone, offset := familyTimeFacts(&TemporalContext{TimeZoneID: "Future/Unknown", UTCOffsetSeconds: 3600, Source: "healthkit_metadata", Confidence: "recorded"})
	if zone.Status != "unavailable" || zone.Reason != "not_verifiable" || offset.Value != int32(3600) {
		t.Fatal("unknown zone was replaced with a host fact or lost its recorded offset")
	}
}
