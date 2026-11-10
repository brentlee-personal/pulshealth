package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"
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
	check := func(want error) {
		t.Helper()
		got, err := store.authorizeAPI(ctx, bearer, claims.Issuer, path)
		if !errors.Is(err, want) {
			t.Fatalf("authorization error=%v, want %v", err, want)
		}
		if want == nil && (got == nil || !got.Principal.equals(principal) || got.TokenHash != hash) {
			t.Fatal("resolved authority lost its owning member or opaque token binding")
		}
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
