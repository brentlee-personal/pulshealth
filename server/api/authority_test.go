package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const apiVectorSeed = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
const apiVectorToken = "eyJhbGciOiJFZERTQSIsInR5cCI6ImF0K2p3dCIsImtpZCI6InJmYzgwMzItdGVzdC0xIn0.eyJpc3MiOiJodHRwczovL3ZpZXdlci5leGFtcGxlLmNvbSIsImF1ZCI6ImZhbWlseS1oZWFsdGgtYXBpIiwic3ViIjoiNWVhNGQwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAxIiwiY2xpZW50X2lkIjoicGNfdGVzdCIsImlhdCI6MTc5MDAwMDAwMCwiZXhwIjoxNzkwMDAwMDYwLCJqdGkiOiI1ZWE0ZDAwMC0wMDAwLTQwMDAtODAwMC0wMDAwMDAwMDAwMDIiLCJncmFudF9pZCI6IjVlYTRkMDAwLTAwMDAtNDAwMC04MDAwLTAwMDAwMDAwMDAwMyIsInByaW5jaXBhbCI6eyJtZW1iZXJfaWQiOiI1ZWE0ZDAwMC0wMDAwLTQwMDAtODAwMC0wMDAwMDAwMDAwMDEiLCJjcmVkZW50aWFsX2lkIjoiNWVhNGQwMDAtMDAwMC00MDAwLTgwMDAtMDAwMDAwMDAwMDAyIiwiYWxsb3dlZF90eXBlcyI6WyJIS1F1YW50aXR5VHlwZUlkZW50aWZpZXJTdGVwQ291bnQiXSwiYWxsb3dlZF90aW1lX3JhbmdlIjp7ImZyb20iOiIyMDI2LTAxLTAxVDAwOjAwOjAwLjAwMDAwMDAwMVoiLCJ1bnRpbCI6IjIwMjYtMDItMDFUMDA6MDA6MDBaIn0sImF1dGhvcml6YXRpb25fdmVyc2lvbiI6MSwiY3JlZGVudGlhbF9raW5kIjoiZXhjaGFuZ2UiLCJhbGxvd2VkX29wZXJhdGlvbnMiOlsibWV0cmljczpyZWFkIl19fQ.sWyBGrw60ZzqW8YGvB4oI7ZCI2jGdXl7eg1ZMRXvlaIcVeMjEPuX7d1EIRMJpnT7TyTsCwSixGM6MSzf1YQ2DQ"
const apiVectorClaims = `{"iss":"https://viewer.example.com","aud":"family-health-api","sub":"5ea4d000-0000-4000-8000-000000000001","client_id":"pc_test","iat":1790000000,"exp":1790000060,"jti":"5ea4d000-0000-4000-8000-000000000002","grant_id":"5ea4d000-0000-4000-8000-000000000003","principal":{"member_id":"5ea4d000-0000-4000-8000-000000000001","credential_id":"5ea4d000-0000-4000-8000-000000000002","allowed_types":["HKQuantityTypeIdentifierStepCount"],"allowed_time_range":{"from":"2026-01-01T00:00:00.000000001Z","until":"2026-02-01T00:00:00Z"},"authorization_version":1,"credential_kind":"exchange","allowed_operations":["metrics:read"]}}`
const apiVectorKeys = `{"keys":[{"kid":"rfc8032-test-1","kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo","alg":"EdDSA","use":"sig"}]}`

func apiTestKeys(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "public.json")
	if err := os.WriteFile(path, []byte(apiVectorKeys), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func apiTestSign(t *testing.T, payload, keyID string) string {
	t.Helper()
	seed, err := hex.DecodeString(apiVectorSeed)
	if err != nil {
		t.Fatal(err)
	}
	input := apiBase64.EncodeToString([]byte(`{"alg":"EdDSA","typ":"at+jwt","kid":"`+keyID+`"}`)) + "." + apiBase64.EncodeToString([]byte(payload))
	return input + "." + apiBase64.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), []byte(input)))
}

func TestAPIAuthorityNodeVector(t *testing.T) {
	path := apiTestKeys(t)
	claims, err := verifyAPIAssertion(apiVectorToken, "https://viewer.example.com", path, time.Unix(1790000001, 0))
	if err != nil || claims == nil || claims.Principal.MemberID != "5ea4d000-0000-4000-8000-000000000001" {
		t.Fatalf("Node assertion rejected: %v", err)
	}
	if apiTestSign(t, apiVectorClaims, "rfc8032-test-1") != apiVectorToken {
		t.Fatal("Go signing disagrees with the fixed Node 26 assertion")
	}
	if claims.Principal.AllowedTimeRange.From != "2026-01-01T00:00:00.000000001Z" {
		t.Fatal("nanosecond permission was lost")
	}
}

func TestAPIAuthorityRejectsInvalidClaims(t *testing.T) {
	path := apiTestKeys(t)
	cases := map[string]string{
		"issuer":               strings.Replace(apiVectorClaims, "viewer.example.com", "other.example.com", 1),
		"audience":             strings.Replace(apiVectorClaims, "family-health-api", "https://reader.example.com/mcp", 1),
		"legacy scope":         strings.Replace(apiVectorClaims, `"aud":`, `"scope":"health:read","aud":`, 1),
		"missing principal":    strings.Replace(apiVectorClaims, `"principal":`, `"other":`, 1),
		"fractional expiry":    strings.Replace(apiVectorClaims, "1790000060", "1790000060.5", 1),
		"long exchange":        strings.Replace(apiVectorClaims, "1790000060", "1790000061", 1),
		"future issued":        strings.Replace(apiVectorClaims, "1790000000", "1790001000", 1),
		"expired":              strings.Replace(apiVectorClaims, "1790000060", "1790000001", 1),
		"device kind":          strings.Replace(apiVectorClaims, `"exchange"`, `"device"`, 1),
		"grant kind":           strings.Replace(apiVectorClaims, `"exchange"`, `"grant"`, 1),
		"grant export":         strings.Replace(apiVectorClaims, `"metrics:read"`, `"data:export"`, 1),
		"write":                strings.Replace(apiVectorClaims, `"metrics:read"`, `"data:delete"`, 1),
		"duplicate operations": strings.Replace(apiVectorClaims, `["metrics:read"]`, `["metrics:read","metrics:read"]`, 1),
		"no types":             strings.Replace(apiVectorClaims, `["HKQuantityTypeIdentifierStepCount"]`, `[]`, 1),
		"duplicate types":      strings.Replace(apiVectorClaims, `["HKQuantityTypeIdentifierStepCount"]`, `["x","x"]`, 1),
		"long Unicode type":    strings.Replace(apiVectorClaims, `HKQuantityTypeIdentifierStepCount`, strings.Repeat("步", 257), 1),
		"version zero":         strings.Replace(apiVectorClaims, `"authorization_version":1`, `"authorization_version":0`, 1),
		"unsafe version":       strings.Replace(apiVectorClaims, `"authorization_version":1`, `"authorization_version":9007199254740992`, 1),
		"duplicate version":    strings.Replace(apiVectorClaims, `"authorization_version":1`, `"authorization_version":1,"authorization_version":1`, 1),
		"offset time":          strings.Replace(apiVectorClaims, "2026-02-01T00:00:00Z", "2026-02-01T00:00:00+00:00", 1),
		"reverse nanos":        strings.Replace(apiVectorClaims, "2026-02-01T00:00:00Z", "2026-01-01T00:00:00.000000000Z", 1),
		"invalid date":         strings.Replace(apiVectorClaims, "2026-02-01", "2026-02-30", 1),
		"another member":       strings.Replace(apiVectorClaims, `"sub":"5ea4d000-0000-4000-8000-000000000001"`, `"sub":"5ea4d000-0000-4000-8000-000000000004"`, 1),
		"another credential":   strings.Replace(apiVectorClaims, `"jti":"5ea4d000-0000-4000-8000-000000000002"`, `"jti":"5ea4d000-0000-4000-8000-000000000004"`, 1),
		"null grant":           strings.Replace(apiVectorClaims, `"grant_id":"5ea4d000-0000-4000-8000-000000000003"`, `"grant_id":null`, 1),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := verifyAPIAssertion(apiTestSign(t, payload, "rfc8032-test-1"), "https://viewer.example.com", path, time.Unix(1790000001, 0))
			if !errors.Is(err, errAPICredential) {
				t.Fatalf("invalid assertion accepted or misclassified: %v", err)
			}
		})
	}
	for _, identifier := range []string{"oura.sleep", strings.Repeat("步", 256)} {
		payload := strings.Replace(apiVectorClaims, "HKQuantityTypeIdentifierStepCount", identifier, 1)
		if _, err := verifyAPIAssertion(apiTestSign(t, payload, "rfc8032-test-1"), "https://viewer.example.com", path, time.Unix(1790000001, 0)); err != nil {
			t.Fatalf("valid generic record type rejected: %v", err)
		}
	}
}

func TestAPIAuthorityKeyRotationAndFailure(t *testing.T) {
	path := apiTestKeys(t)
	now := time.Unix(1790000001, 0)
	if _, err := verifyAPIAssertion(apiVectorToken, "https://viewer.example.com", path, now); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rotated := strings.Replace(apiVectorKeys, "rfc8032-test-1", "rotated", 1)
	rotated = strings.Replace(rotated, "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo", apiBase64.EncodeToString(public), 1)
	if err := os.WriteFile(path, []byte(rotated), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyAPIAssertion(apiVectorToken, "https://viewer.example.com", path, now); !errors.Is(err, errAPICredential) {
		t.Fatalf("retired key remained active: %v", err)
	}
	parts := strings.Split(apiTestSign(t, apiVectorClaims, "rotated"), ".")
	input := parts[0] + "." + parts[1]
	newAssertion := input + "." + apiBase64.EncodeToString(ed25519.Sign(private, []byte(input)))
	if _, err := verifyAPIAssertion(newAssertion, "https://viewer.example.com", path, now); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "{}", apiVectorKeys + "{}", strings.Replace(apiVectorKeys, `"alg":"EdDSA"`, `"alg":"EdDSA","d":"private"`, 1),
		strings.Replace(apiVectorKeys, `"keys":[`, `"keys":[], "keys":[`, 1), strings.Replace(apiVectorKeys, `"kty":"OKP"`, `"kty":"RSA"`, 1)} {
		if err := os.WriteFile(path, []byte(invalid), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyAPIAssertion(apiVectorToken, "https://viewer.example.com", path, now); !errors.Is(err, errAPIAuthority) {
			t.Fatalf("bad key file did not fail closed: %v", err)
		}
	}
	if _, err := verifyAPIAssertion(apiVectorToken, "https://viewer.example.com", filepath.Join(t.TempDir(), "missing"), now); !errors.Is(err, errAPIAuthority) {
		t.Fatalf("missing keys did not report unavailable: %v", err)
	}
}

func TestAPIAuthorityRejectsNonOpaqueBearer(t *testing.T) {
	store := &Store{}
	for _, bearer := range []string{"", "shared-service-token", apiVectorToken, strings.Repeat("A", 42) + "B", strings.Repeat("A", 100000)} {
		if _, err := store.authorizeAPI(t.Context(), bearer, "https://viewer.example.com", "unused"); !errors.Is(err, errAPICredential) {
			t.Fatalf("non-opaque credential reached database: %v", err)
		}
	}
}

func TestAPIAuthorityStrictJSON(t *testing.T) {
	for _, payload := range []string{`{"a":1,"a":1}`, `{"a":[{"b":1,"b":1}]}`, `{"a":1}{}`, `{"a":`, string([]byte{'"', 255, '"'})} {
		var decoded any
		if strictAuthorityJSON([]byte(payload), &decoded) == nil {
			t.Fatalf("invalid JSON accepted: %q", payload)
		}
	}
	var decoded map[string]any
	if err := strictAuthorityJSON([]byte(`{"a":[1,true,null,{"b":"ok"}]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(decoded)
	if !strings.Contains(string(data), `"b":"ok"`) {
		t.Fatal("valid nested JSON lost")
	}
}
