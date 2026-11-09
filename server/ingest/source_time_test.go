package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestFamilyRecordPreservesSubmicrosecondSourceTime(t *testing.T) {
	var digests [][]byte
	var samples []Sample
	for _, tc := range []struct{ number, stamp string }{
		{"1718000000000.0002", "2024-06-10T06:13:20.0000002Z"},
		{"1718000000000.0005", "2024-06-10T06:13:20.0000005Z"},
	} {
		line := strings.Replace(hrSample, `"start":1718000000000`, `"start":`+tc.number, 1)
		batch, err := ParseFamilyBatch(strings.NewReader(ndjson(t, 1, 0, line)))
		if err != nil { t.Fatal(err) }
		sample := batch.Samples[0]
		content, digest, _, err := normalizeFamilySample(&sample)
		if err != nil { t.Fatal(err) }
		if content.StartAt != tc.stamp { t.Errorf("source time=%s, want %s", content.StartAt, tc.stamp) }
		digests = append(digests, digest)
		samples = append(samples, sample)
	}
	if samples[0].Start == samples[1].Start || !msToTime(samples[0].Start).Equal(msToTime(samples[1].Start)) {
		t.Fatal("fixture must contain distinct wire times in one stored microsecond")
	}
	if bytes.Equal(digests[0],digests[1]) { t.Error("distinct source instants collapsed to the same immutable digest") }
}
