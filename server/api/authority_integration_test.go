package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIntegrationAPIAuthorityLiveDatabase(t *testing.T) {
	store, admin, ctx, cleanup := writeIntegrationStore(t)
	defer cleanup()
	var role string
	if err := store.pool.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil || role != "api_reader" {
		t.Fatalf("authority must run as api_reader, got %q: %v", role, err)
	}
	nonce := time.Now().UnixNano()
	member := fixtureUUID("aadd3000", nonce, 0)
	other := fixtureUUID("aadd3000", nonce, 1)
	credential := fixtureUUID("aadd4000", nonce, 0)
	if _, err := admin.Exec(ctx, "INSERT INTO public.users(id) VALUES($1),($2)", member, other); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec(ctx, "DELETE FROM auth.accounts WHERE user_id IN ($1,$2)", member, other)
		_, _ = admin.Exec(ctx, "DELETE FROM public.users WHERE id IN ($1,$2)", member, other)
	}()
	var account string
	if err := admin.QueryRow(ctx, "INSERT INTO auth.accounts(user_id,email,password_hash) VALUES($1,$2,'synthetic-account-hash') RETURNING id::text",
		member, member+"@synthetic.invalid").Scan(&account); err != nil {
		t.Fatal(err)
	}
	session := make([]byte, 32)
	secret := make([]byte, 32)
	if _, err := rand.Read(session); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO auth.sessions(id,account_id,expires_at) VALUES($1,$2,now()+interval '1 hour')", session, account); err != nil {
		t.Fatal(err)
	}
	principal := apiPrincipal{MemberID: member, CredentialID: credential, CredentialKind: "session", AuthorizationVersion: 1,
		AllowedOperations: []string{"metrics:read"}, AllowedTypes: []string{"HKQuantityTypeIdentifierStepCount"},
		AllowedTimeRange: apiTimeRange{From: "2026-01-01T00:00:00.000000001Z", Until: "2026-02-01T00:00:00Z"}}
	now := time.Now().Unix()
	claims := apiClaims{Issuer: "https://viewer.example.com", Audience: "family-health-api", MemberID: member,
		ClientID: "family-health-web", IssuedAt: now, ExpiresAt: now + 120, ID: credential, Principal: principal}
	makeAssertion := func(claims apiClaims) string {
		t.Helper()
		data, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		return apiTestSign(t, string(data), "rfc8032-test-1")
	}
	assertion := makeAssertion(claims)
	permissions, err := json.Marshal(map[string]any{"operations": principal.AllowedOperations, "record_types": principal.AllowedTypes, "time_range": principal.AllowedTimeRange})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(secret)
	if _, err := admin.Exec(ctx, `INSERT INTO auth.api_credentials(id,token_hash,account_id,session_id,permissions,authorization_version,assertion,created_at,expires_at)
		VALUES($1,$2,$3,$4,$5,1,$6,to_timestamp($7),to_timestamp($8))`, credential, hash[:], account, session, permissions, assertion, now, now+120); err != nil {
		t.Fatal(err)
	}
	bearer := apiBase64.EncodeToString(secret)
	path := apiTestKeys(t)
	server := httptest.NewServer((&Server{store: store, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		authorize: func(ctx context.Context, bearer string) (*apiAuthority, error) {
			return store.authorizeAPI(ctx, bearer, claims.Issuer, path)
		}}).routes())
	defer server.Close()
	httpCheck := func(want int) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/users", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("HTTP status=%d, want %d", response.StatusCode, want)
		}
		if want == http.StatusOK {
			var body UsersResponse
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Default != member || body.MultiUser || len(body.Users) != 1 || body.Users[0].UserID != member {
				t.Fatal("HTTP response was not restricted to the credential's member")
			}
		}
	}
	check := func(want error) {
		t.Helper()
		got, err := store.authorizeAPI(ctx, bearer, claims.Issuer, path)
		if !errors.Is(err, want) {
			t.Fatalf("authorization error=%v, want %v", err, want)
		}
		if want == nil && (got == nil || !got.Principal.equals(principal) || got.TokenHash != hash) {
			t.Fatal("resolved authority lost its owning member or opaque token binding")
		}
		status := http.StatusOK
		if errors.Is(want, errAPICredential) {
			status = http.StatusUnauthorized
		} else if errors.Is(want, errAPIAuthority) {
			status = http.StatusServiceUnavailable
		}
		httpCheck(status)
	}
	check(nil)
	for _, statement := range []string{
		"UPDATE auth.api_credentials SET revoked_at=now() WHERE id=$1",
		"UPDATE auth.api_credentials SET authorization_version=2 WHERE id=$1",
		"UPDATE auth.api_credentials SET created_at=statement_timestamp()-interval '60 seconds',expires_at=statement_timestamp() WHERE id=$1",
	} {
		if _, err := admin.Exec(ctx, statement, credential); err != nil {
			t.Fatal(err)
		}
		check(errAPICredential)
		if _, err := admin.Exec(ctx, "UPDATE auth.api_credentials SET revoked_at=NULL,authorization_version=1,created_at=to_timestamp($2),expires_at=to_timestamp($3) WHERE id=$1", credential, now, now+120); err != nil {
			t.Fatal(err)
		}
	}
	misbound := claims
	misbound.MemberID, misbound.Principal.MemberID = other, other
	if _, err := admin.Exec(ctx, "UPDATE auth.api_credentials SET assertion=$2 WHERE id=$1", credential, makeAssertion(misbound)); err != nil {
		t.Fatal(err)
	}
	check(errAPICredential)
	if _, err := admin.Exec(ctx, "UPDATE auth.api_credentials SET assertion=$2 WHERE id=$1", credential, assertion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.authorizeAPI(ctx, bearer, claims.Issuer, path+".missing"); !errors.Is(err, errAPIAuthority) {
		t.Fatalf("missing public keys: %v, want authorization unavailable", err)
	}
	if err := os.Rename(path, path+".retired"); err != nil {
		t.Fatal(err)
	}
	httpCheck(http.StatusServiceUnavailable)
	if err := os.Rename(path+".retired", path); err != nil {
		t.Fatal(err)
	}
	check(nil)
	t.Run("record scope", func(t *testing.T) {
		stepType, dropStep := ensureSampleType(t, ctx, admin, "HKQuantityTypeIdentifierStepCount", "quantity", ptrString("count"))
		defer dropStep()
		heartType, dropHeart := ensureSampleType(t, ctx, admin, "HKQuantityTypeIdentifierHeartRate", "quantity", ptrString("count/min"))
		defer dropHeart()
		if _, err := admin.Exec(ctx, `INSERT INTO public.quantity_samples(uuid,type_id,start_ts,end_ts,value,user_id) VALUES
			($1,$4,'2026-01-15T12:00:00Z','2026-01-15T12:00:00Z',11,$6),
			($2,$4,'2026-03-15T12:00:00Z','2026-03-15T12:00:00Z',999,$6),
			($3,$5,'2026-01-15T12:00:00Z','2026-01-15T12:00:00Z',88,$6)`,
			fixtureUUID("aadd5000", nonce, 0), fixtureUUID("aadd5000", nonce, 1), fixtureUUID("aadd5000", nonce, 2), stepType, heartType, member); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = admin.Exec(ctx, "DELETE FROM public.quantity_samples WHERE user_id=$1", member) }()
		read := func(t *testing.T, target string) *http.Response {
			t.Helper()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+target, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+bearer)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			return response
		}
		for name, target := range map[string]string{
			"raw operation":  "/v1/samples?type=HKQuantityTypeIdentifierStepCount&start=1768435200000&end=1768521600000",
			"another type":   "/v1/metrics/latest?types=HKQuantityTypeIdentifierHeartRate",
			"another period": "/v1/metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=1769904000000&end=1769990400000",
			"another member": "/v1/metrics/latest?types=HKQuantityTypeIdentifierStepCount&user=" + other,
			"profile":        "/v1/profile",
			"export":         "/v1/export?dataset=daily_metrics&format=csv&types=HKQuantityTypeIdentifierStepCount&start=1768435200000&end=1768521600000",
		} {
			t.Run(name, func(t *testing.T) {
				response := read(t, target)
				defer response.Body.Close()
				if response.StatusCode != http.StatusForbidden {
					t.Fatalf("personal credential reached outside its %s: HTTP %d, want 403", name, response.StatusCode)
				}
			})
		}
		t.Run("latest input period", func(t *testing.T) {
			response := read(t, "/v1/metrics/latest?types=HKQuantityTypeIdentifierStepCount")
			defer response.Body.Close()
			var body struct {
				Metrics []LatestMetric `json:"metrics"`
			}
			if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&body) != nil || len(body.Metrics) != 1 || body.Metrics[0].Value == nil || *body.Metrics[0].Value != 11 {
				t.Fatal("latest metric used a synthetic sample outside the signed UTC range")
			}
		})
		t.Run("direct base table", func(t *testing.T) {
			for _, relation := range []string{
				"public.users", "public.batches", "public.sources", "public.sample_types", "public.category_labels",
				"public.quantity_samples", "public.category_samples", "public.workouts", "public.heartbeat_series",
				"public.ecg_samples", "public.state_of_mind", "public.medication_dose_events", "public.activity_summaries",
				"public.aggregate_series", "public.aggregate_samples", "public.metric_daily", "public.workout_route_points",
				"public.workout_series_points", "public.family_record_identity", "public.family_snapshot_identity",
				"public.family_source_profiles", "auth.accounts", "auth.sessions", "auth.oauth_grants", "auth.api_credentials",
			} {
				var value int
				err := store.pool.QueryRow(ctx, "SELECT 1 FROM "+relation+" LIMIT 1").Scan(&value)
				var denied *pgconn.PgError
				if !errors.As(err, &denied) || denied.Code != "42501" {
					t.Fatalf("API database role could read %s directly: %v", relation, err)
				}
			}
		})
		t.Run("catalog cache follows credential scope", func(t *testing.T) {
			heartClaims := claims
			heartClaims.ID = fixtureUUID("aadd4000", nonce, 1)
			heartClaims.Principal.CredentialID = heartClaims.ID
			heartClaims.Principal.AllowedTypes = []string{"HKQuantityTypeIdentifierHeartRate"}
			heartSecret := make([]byte, 32)
			if _, err := rand.Read(heartSecret); err != nil {
				t.Fatal(err)
			}
			heartHash := sha256.Sum256(heartSecret)
			heartPermissions, err := json.Marshal(map[string]any{"operations": heartClaims.Principal.AllowedOperations,
				"record_types": heartClaims.Principal.AllowedTypes, "time_range": heartClaims.Principal.AllowedTimeRange})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := admin.Exec(ctx, `INSERT INTO auth.api_credentials(id,token_hash,account_id,session_id,permissions,authorization_version,assertion,created_at,expires_at)
				VALUES($1,$2,$3,$4,$5,1,$6,to_timestamp($7),to_timestamp($8))`, heartClaims.ID, heartHash[:], account, session,
				heartPermissions, makeAssertion(heartClaims), now, now+120); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = admin.Exec(ctx, "DELETE FROM auth.api_credentials WHERE id=$1", heartClaims.ID) }()
			for _, selected := range []struct{ token, identifier string }{
				{bearer, principal.AllowedTypes[0]},
				{apiBase64.EncodeToString(heartSecret), heartClaims.Principal.AllowedTypes[0]},
				{bearer, principal.AllowedTypes[0]},
			} {
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/catalog/types", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+selected.token)
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				var body struct {
					Types []CatalogType `json:"types"`
				}
				err = json.NewDecoder(response.Body).Decode(&body)
				response.Body.Close()
				if response.StatusCode != http.StatusOK || err != nil || len(body.Types) != 1 || body.Types[0].Identifier != selected.identifier {
					t.Fatal("catalog cache exposed another personal credential's record types")
				}
			}
		})
		t.Run("transaction identity and revocation", func(t *testing.T) {
			config := store.pool.Config()
			config.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			personal := NewStore(pool, time.UTC)
			authority, err := personal.authorizeAPI(ctx, bearer, claims.Issuer, path)
			if err != nil {
				t.Fatal(err)
			}
			requestCtx, cancel := context.WithCancel(ctx)
			scopedCtx, closeScope, err := personal.ReadScope(requestCtx, authority)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			defer closeScope()
			var backend, nextBackend int
			var readOnly string
			if err := personal.readDB(scopedCtx).QueryRow(scopedCtx, "SELECT pg_backend_pid(),current_setting('transaction_read_only')").Scan(&backend, &readOnly); err != nil || readOnly != "on" {
				t.Fatalf("request did not get a read-only transaction: %v", err)
			}
			var rows int
			if err := personal.readDB(scopedCtx).QueryRow(scopedCtx, "SELECT count(*) FROM quantity_samples").Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("scoped input count=%d: %v", rows, err)
			}
			if _, err := admin.Exec(ctx, "UPDATE auth.api_credentials SET revoked_at=statement_timestamp() WHERE id=$1", credential); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_, _ = admin.Exec(ctx, "UPDATE auth.api_credentials SET revoked_at=NULL WHERE id=$1", credential)
			}()
			if err := personal.readDB(scopedCtx).QueryRow(scopedCtx, "SELECT count(*) FROM quantity_samples").Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("next statement retained revoked rows=%d: %v", rows, err)
			}
			httpCheck(http.StatusUnauthorized)
			cancel()
			closeScope()
			var setting string
			if err := pool.QueryRow(ctx, "SELECT pg_backend_pid(),coalesce(current_setting('puls.api_credential_hash',true),'')").Scan(&nextBackend, &setting); err != nil || backend != nextBackend || setting != "" {
				t.Fatalf("cancelled request retained identity or lost its reusable connection: %v", err)
			}
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM quantity_samples").Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("unscoped reused connection returned rows=%d: %v", rows, err)
			}
		})
	})
	if _, err := admin.Exec(ctx, "UPDATE auth.accounts SET disabled_at=now() WHERE id=$1", account); err != nil {
		t.Fatal(err)
	}
	check(errAPICredential)
	if _, err := admin.Exec(ctx, "UPDATE auth.accounts SET disabled_at=NULL WHERE id=$1", account); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "DELETE FROM auth.sessions WHERE id=$1", session); err != nil {
		t.Fatal(err)
	}
	check(errAPICredential)
	store.pool.Close()
	check(errAPIAuthority)
}
