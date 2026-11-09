package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestBatchDiagnosticsExcludeClientValues(t *testing.T) {
	const marker = "synthetic-private-value"
	valid := ndjson(t, 1, 0, hrSample)
	badKind := ndjson(t, 1, 0, strings.Replace(hrSample, `"kind":"quantity"`, `"kind":"`+marker+`"`, 1))
	cases := []struct {
		name    string
		body    string
		headers map[string]string
		insert  error
		status  int
	}{
		{"sample kind", badKind, nil, nil, http.StatusBadRequest},
		{"encoding", valid, map[string]string{"Content-Encoding": marker}, nil, http.StatusBadRequest},
		{"protocol", valid, map[string]string{"X-Puls-Protocol": marker}, nil, http.StatusBadRequest},
		{"batch correlation", valid, map[string]string{"X-Batch-ID": marker}, nil, http.StatusOK},
		{"wake correlation", valid, map[string]string{"X-Wake-ID": marker}, nil, http.StatusBadRequest},
		{"trigger", badKind, map[string]string{"X-Wake-Trigger": marker}, nil, http.StatusBadRequest},
		{"insert error", valid, nil, errors.New(marker), http.StatusInternalServerError},
		{"successful client labels", strings.Replace(valid, `"deviceID":"dev-1"`, `"deviceID":"`+marker+`","clientVersion":"`+marker+`"`, 1), nil, nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{insertErr: tc.insert}
			srv := newTestServer(fs)
			var logs bytes.Buffer
			srv.log = slog.New(slog.NewJSONHandler(&logs, nil))
			rec := postBatch(t, srv, tc.body, tc.headers)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			encoded, err := json.Marshal(fs.rejections)
			if err != nil {
				t.Fatal(err)
			}
			for name, surface := range map[string]string{
				"response": rec.Body.String(), "log": logs.String(), "diagnostic": string(encoded),
			} {
				if strings.Contains(surface, marker) {
					t.Errorf("client value leaked into %s", name)
				}
			}
		})
	}
}
