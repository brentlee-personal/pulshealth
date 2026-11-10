package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIntegrationServiceCredentialCannotReadMemberHealth(t *testing.T) {
	store, admin, ctx, cleanup := writeIntegrationStore(t)
	defer cleanup()
	nonce := time.Now().UnixNano()
	member := fixtureUUID("aadd1000", nonce, 0)
	workout := fixtureUUID("aadd2000", nonce, 0)
	if _, err := admin.Exec(ctx, "INSERT INTO users(id) VALUES($1)", member); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec(ctx, "DELETE FROM workouts WHERE user_id=$1", member)
		_, _ = admin.Exec(ctx, "DELETE FROM users WHERE id=$1", member)
	}()
	if _, err := admin.Exec(ctx,
		"INSERT INTO workouts(uuid,activity_type,start_ts,end_ts,duration_s,user_id) VALUES($1,'HKWorkoutActivityTypeRunning',now()-interval '1 hour',now()-interval '30 minutes',1800,$2)",
		workout, member); err != nil {
		t.Fatal(err)
	}
	serviceToken := "synthetic-family-shared-service-token"
	server := &Server{store: store, authorize: func(ctx context.Context, bearer string) (*apiAuthority, error) {
		return store.authorizeAPI(ctx, bearer, "https://viewer.example.com", apiTestKeys(t))
	}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/v1/workouts", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+serviceToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	exposed := response.StatusCode == http.StatusOK && strings.Contains(string(body), workout)
	if exposed {
		t.Fatal("service credential alone exposed a synthetic member workout; personal authorization is required")
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("service-only health request status=%d, want 401", response.StatusCode)
	}
}
