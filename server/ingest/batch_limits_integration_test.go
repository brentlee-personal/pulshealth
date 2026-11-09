package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
)

func paddedBatch(body string, size int64) io.Reader {
	readers := []io.Reader{strings.NewReader(body)}
	chunk := strings.Repeat(" ", (1<<20)-1) + "\n"
	for left := size - int64(len(body)); left > 0; {
		n := min(left, int64(len(chunk)))
		readers = append(readers, strings.NewReader(chunk[:n]))
		left -= n
	}
	return io.MultiReader(readers...)
}

func TestIntegration_FamilyHealthBodyByteBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int64
		gzip   bool
		status int
	}{
		{"wire exact", 8 << 20, false, http.StatusOK},
		{"wire over", (8 << 20) + 1, false, http.StatusRequestEntityTooLarge},
		{"decoded exact", 64 << 20, true, http.StatusOK},
		{"decoded over", (64 << 20) + 1, true, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := familyTestSetup(t)
			batch, sample := f.uuid(0x690), f.uuid(0x691)
			var body io.Reader = paddedBatch(f.body(batch, sample, 62.5), tc.size)
			encoding := "identity"
			if tc.gzip {
				var compressed bytes.Buffer
				w := gzip.NewWriter(&compressed)
				if _, err := io.Copy(w, body); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				body, encoding = &compressed, "gzip"
			}
			req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.server.URL+"/v1/batches", body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+f.actors[0].token)
			req.Header.Set("Content-Encoding", encoding)
			response, err := f.server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("status=%d, want %d", response.StatusCode, tc.status)
			}
			for _, table := range []string{"batches", "family_batch_receipts", "quantity_samples"} {
				var count int
				if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1", f.actors[0].user).Scan(&count); err != nil {
					t.Fatal(err)
				}
				want := 0
				if tc.status == http.StatusOK {
					want = 1
				}
				if count != want {
					t.Errorf("%s has %d rows, want %d", table, count, want)
				}
			}
		})
	}
}
