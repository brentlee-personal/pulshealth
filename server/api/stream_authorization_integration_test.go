package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The barrier commits a real revocation after the first real database row.
type revokingReadStore struct {
	*Store
	revoke func() error
}

func (st revokingReadStore) StreamSamples(ctx context.Context, member string, meta SampleMeta, filters SampleFilters, emit func(Sample) error) error {
	first := true
	return st.Store.StreamSamples(ctx, member, meta, filters, func(sample Sample) error {
		if err := emit(sample); err != nil {
			return err
		}
		if first {
			first = false
			return st.revoke()
		}
		return nil
	})
}

func (st revokingReadStore) StreamWorkouts(ctx context.Context, member string, filters WorkoutFilters, emit func(WorkoutSummary) error) error {
	first := true
	return st.Store.StreamWorkouts(ctx, member, filters, func(workout WorkoutSummary) error {
		if err := emit(workout); err != nil {
			return err
		}
		if first {
			first = false
			return st.revoke()
		}
		return nil
	})
}

func TestIntegrationAPIPoolAndStreamRevocation(t *testing.T) {
	store, admin, ctx, cleanup := writeIntegrationStore(t)
	defer cleanup()
	config := store.pool.Config()
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	personal := NewStore(pool, time.UTC)
	path := apiTestKeys(t)
	nonce := time.Now().UnixNano()
	members := []struct {
		id, credential, bearer string
		rows                   int
	}{
		{id: fixtureUUID("aadd7100", nonce, 0), credential: fixtureUUID("aadd7200", nonce, 0), rows: 600},
		{id: fixtureUUID("aadd7100", nonce, 1), credential: fixtureUUID("aadd7200", nonce, 1), rows: 1},
	}
	typeID, dropType := ensureSampleType(t, ctx, admin, "HKQuantityTypeIdentifierStepCount", "quantity", ptrString("count"))
	defer dropType()
	defer func() {
		for _, member := range members {
			_, _ = admin.Exec(ctx, "DELETE FROM auth.accounts WHERE user_id=$1", member.id)
			_, _ = admin.Exec(ctx, "DELETE FROM public.quantity_samples WHERE user_id=$1", member.id)
			_, _ = admin.Exec(ctx, "DELETE FROM public.workouts WHERE user_id=$1", member.id)
			_, _ = admin.Exec(ctx, "DELETE FROM public.users WHERE id=$1", member.id)
		}
	}()
	for index := range members {
		member := &members[index]
		if _, err := admin.Exec(ctx, "INSERT INTO public.users(id) VALUES($1)", member.id); err != nil {
			t.Fatal(err)
		}
		var account string
		if err := admin.QueryRow(ctx, "INSERT INTO auth.accounts(user_id,email,password_hash) VALUES($1,$2,'synthetic-password-hash') RETURNING id::text", member.id, member.id+"@synthetic.invalid").Scan(&account); err != nil {
			t.Fatal(err)
		}
		session, secret := make([]byte, 32), make([]byte, 32)
		if _, err := rand.Read(session); err != nil {
			t.Fatal(err)
		}
		if _, err := rand.Read(secret); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, "INSERT INTO auth.sessions(id,account_id,expires_at) VALUES($1,$2,now()+interval '1 hour')", session, account); err != nil {
			t.Fatal(err)
		}
		now := time.Now().Unix()
		principal := apiPrincipal{MemberID: member.id, CredentialID: member.credential, CredentialKind: "session", AuthorizationVersion: 1,
			AllowedOperations: []string{"metrics:read", "samples:read", "data:export"},
			AllowedTypes:      []string{"HKQuantityTypeIdentifierStepCount", "HKWorkoutTypeIdentifier"},
			AllowedTimeRange:  apiTimeRange{From: "2026-01-01T00:00:00Z", Until: "2026-02-01T00:00:00Z"}}
		claims := apiClaims{Issuer: "https://viewer.example.com", Audience: "family-health-api", MemberID: member.id,
			ClientID: "family-health-web", IssuedAt: now, ExpiresAt: now + 300, ID: member.credential, Principal: principal}
		claimJSON, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		permissions, err := json.Marshal(map[string]any{"operations": principal.AllowedOperations,
			"record_types": principal.AllowedTypes, "time_range": principal.AllowedTimeRange})
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(secret)
		if _, err := admin.Exec(ctx, `INSERT INTO auth.api_credentials(id,token_hash,account_id,session_id,permissions,authorization_version,assertion,created_at,expires_at)
			VALUES($1,$2,$3,$4,$5,1,$6,to_timestamp($7),to_timestamp($8))`, member.credential, hash[:], account, session,
			permissions, apiTestSign(t, string(claimJSON), "rfc8032-test-1"), now, now+300); err != nil {
			t.Fatal(err)
		}
		member.bearer = apiBase64.EncodeToString(secret)
		if _, err := admin.Exec(ctx, `INSERT INTO public.quantity_samples(uuid,type_id,start_ts,end_ts,value,user_id)
			SELECT md5($1::text||g::text)::uuid,$2,'2026-01-15T12:00:00Z'::timestamptz+g*interval '1 microsecond',
			'2026-01-15T12:00:00Z'::timestamptz+g*interval '1 microsecond',g,$1::uuid FROM generate_series(1,$3::int) g`, member.id, typeID, member.rows); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, `INSERT INTO public.workouts(uuid,activity_type,start_ts,end_ts,duration_s,user_id)
			SELECT md5($1::text||'workout'||g::text)::uuid,'HKWorkoutActivityTypeRunning',
			'2026-01-15T12:00:00Z'::timestamptz+g*interval '1 microsecond',
			'2026-01-15T12:01:00Z'::timestamptz+g*interval '1 microsecond',60,$1::uuid FROM generate_series(1,$2::int) g`, member.id, member.rows); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func(ctx context.Context, bearer string) (*apiAuthority, error) {
		return personal.authorizeAPI(ctx, bearer, "https://viewer.example.com", path)
	}
	t.Run("A B A on one backend", func(t *testing.T) {
		backend := 0
		for _, index := range []int{0, 1, 0} {
			member := members[index]
			authority, err := resolve(ctx, member.bearer)
			if err != nil {
				t.Fatal(err)
			}
			scoped, closeScope, err := personal.ReadScope(ctx, authority)
			if err != nil {
				t.Fatal(err)
			}
			var pid, rows int
			var owner string
			err = personal.readDB(scoped).QueryRow(scoped, "SELECT pg_backend_pid(),count(*),min(user_id::text) FROM quantity_samples").Scan(&pid, &rows, &owner)
			closeScope()
			if err != nil || rows != member.rows || owner != member.id || (backend != 0 && backend != pid) {
				t.Fatalf("shared backend changed member scope: rows=%d owner=%q err=%v", rows, owner, err)
			}
			backend = pid
		}
	})
	for _, dataset := range []string{"samples", "workouts"} {
		t.Run(dataset+" HTTP revocation", func(t *testing.T) {
			revoked := make(chan struct{})
			actual := revokingReadStore{Store: personal, revoke: func() error {
				_, err := admin.Exec(ctx, "UPDATE auth.api_credentials SET revoked_at=statement_timestamp() WHERE id=$1", members[0].credential)
				if err == nil {
					close(revoked)
				}
				return err
			}}
			defer func() {
				_, _ = admin.Exec(ctx, "UPDATE auth.api_credentials SET revoked_at=NULL WHERE id=$1", members[0].credential)
			}()
			server := httptest.NewServer((&Server{store: actual, authorize: resolve, log: slog.New(slog.NewTextHandler(io.Discard, nil))}).routes())
			defer server.Close()
			start := time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)
			target := fmt.Sprintf("/v1/export?format=csv&dataset=%s&start=%d&end=%d", dataset, start.UnixMilli(), start.Add(48*time.Hour).UnixMilli())
			if dataset == "samples" {
				target += "&type=HKQuantityTypeIdentifierStepCount"
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+target, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+members[0].bearer)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			response.Body.Close()
			select {
			case <-revoked:
			default:
				t.Fatal("revocation barrier did not commit")
			}
			if response.StatusCode != http.StatusOK || readError == nil || len(strings.Split(strings.TrimSpace(string(body)), "\n")) > 257 {
				t.Fatalf("revoked HTTP export did not abort within one bounded page: status=%d rows=%d read_error=%v", response.StatusCode, strings.Count(string(body), "\n"), readError)
			}
		})
	}
}
