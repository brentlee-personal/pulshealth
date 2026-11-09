package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestParseBatch_BusinessLineBoundary(t *testing.T) {
	body := ndjson(t, 5000, 0, strings.Repeat(hrSample+"\n", 5000))
	b, err := ParseBatch(strings.NewReader(body))
	if err != nil || len(b.Samples) != 5000 {
		t.Fatalf("exact line limit: batch=%v error=%v", b != nil, err)
	}
	for _, counts := range [][2]int{{5001, 0}, {2500, 2501}} {
		_, err := ParseBatch(strings.NewReader(ndjson(t, counts[0], counts[1])))
		if err == nil || batchParseStatus(err) != http.StatusRequestEntityTooLarge {
			t.Fatalf("counts %v: error=%v, want 413", counts, err)
		}
	}
}

func TestParseBatch_ExactLineByteBoundary(t *testing.T) {
	base := strings.TrimSuffix(ndjson(t, 0, 0), "\n")
	for _, ending := range []string{"", "\n", "\r\n"} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("ending=%q/extra=%d", ending, extra), func(t *testing.T) {
				body := base + strings.Repeat(" ", scannerMaxLine-len(base)+extra) + ending
				_, err := ParseBatch(strings.NewReader(body))
				if extra == 0 && err != nil {
					t.Fatalf("exact line limit rejected: %v", err)
				}
				if extra == 1 && (err == nil || batchParseStatus(err) != http.StatusRequestEntityTooLarge) {
					t.Fatalf("one byte over limit: error=%v, want 413", err)
				}
			})
		}
	}
}
