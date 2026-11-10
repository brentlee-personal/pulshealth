package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntegrationRetiredSharedUploadCredential(t *testing.T) {
	f := familyTestSetup(t)
	if status, _ := f.post(t, f.actors[0], f.body(f.uuid(0x710), f.uuid(0x711), 60)); status != http.StatusOK {
		t.Fatalf("personal device setup status = %d, want 200", status)
	}
	const retired = "synthetic-retired-shared-upload"
	server := httptest.NewServer(newServer(f.store, f.store, retired, true, false,
		slog.New(slog.NewJSONHandler(io.Discard, nil))).routes())
	t.Cleanup(server.Close)
	for i, actor := range f.actors {
		t.Run([]string{"owner", "other"}[i], func(t *testing.T) {
			request, err := http.NewRequestWithContext(f.ctx, http.MethodGet, server.URL+"/v1/stats", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+retired)
			request.Header.Set("X-User-ID", actor.user)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Errorf("retired shared credential status = %d, want 401", response.StatusCode)
			}
		})
	}
	t.Run("forged_write", func(t *testing.T) {
		request, err := http.NewRequestWithContext(f.ctx, http.MethodPost, server.URL+"/v1/batches",
			strings.NewReader(f.body(f.uuid(0x712), f.uuid(0x713), 70)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+retired)
		request.Header.Set("X-User-ID", f.actors[1].user)
		request.Header.Set("Content-Encoding", "identity")
		request.Header.Set("X-Puls-Protocol", "1")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("retired shared write status = %d, want 401", response.StatusCode)
		}
		var rows int
		if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM public.quantity_samples WHERE user_id=$1", f.actors[1].user).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Errorf("retired shared credential wrote %d rows for another member, want 0", rows)
		}
	})
}
