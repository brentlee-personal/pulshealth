package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestFamilyReceiptCountsBusinessLines(t *testing.T) {
	batch := &Batch{Header: BatchHeader{BatchID: "00000000-0000-4000-8000-000000000002", SampleCount: 1, DeletionCount: 1, RouteCount: 1, SeriesCount: 1},
		bodyDigest: bytes.Repeat([]byte{0x55}, 32), familyCounts: familyLineCounts{accepted: 2, deleted: 1, routePoints: 2, seriesPoints: 2}}
	result := IngestResult{Accepted: 1, RoutePoints: 2, SeriesPoints: 2, Retries: 12}
	commit := &committedBatchReceipt{TransactionID: "00000000-0000-4000-8000-000000000003",
		CommittedAt: time.Date(2026, 1, 1, 8, 0, 0, 0, time.FixedZone("source-independent", 8*3600))}
	receipt, err := buildFamilyReceipt(batch, "00000000-0000-4000-8000-000000000001", 7, &result, commit)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RecordCount != 4 || receipt.Accepted != 3 || receipt.Deleted != 1 || receipt.Duplicate != 0 || receipt.Suppressed != 0 || receipt.CommittedAt.Format(time.RFC3339) != "2026-01-01T00:00:00Z" {
		t.Fatal("receipt used point counts, omitted lines, or inherited a local time zone")
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 12 {
		t.Fatalf("receipt fields=%d, want 12", len(fields))
	}
	contract, err := familyProtocol()
	if err != nil {
		t.Fatal(err)
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if err := contract.schemas["family-batch-receipt"].Validate(raw); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("retries")) {
		t.Fatal("debug retry count escaped into a stable receipt")
	}
	for _, tc := range []struct {
		name   string
		change func(*Batch, *IngestResult)
	}{
		{"missing digest", func(b *Batch, _ *IngestResult) { b.bodyDigest = nil }},
		{"negative count", func(_ *Batch, r *IngestResult) { r.Accepted = -1 }},
		{"incomplete count", func(_ *Batch, r *IngestResult) { r.Accepted = 0 }},
		{"wrong point writes", func(_ *Batch, r *IngestResult) { r.RoutePoints = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, r := *batch, result
			tc.change(&b, &r)
			if _, err := buildFamilyReceipt(&b, receipt.MemberID, 7, &r, commit); err == nil {
				t.Fatal("invalid acknowledgement accepted")
			}
		})
	}
}
