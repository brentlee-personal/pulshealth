package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOAuthPublicKeyFileRefusals(t *testing.T) {
	c := vectorConfig(t)
	original, err := os.ReadFile(c.publicKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"", "[]", `{"keys":[]}`, strings.Repeat(" ", 16385),
		strings.Replace(string(original), `"EdDSA"`, `"HS256"`, 1),
		strings.Replace(string(original), `"Ed25519"`, `"Ed448"`, 1),
		strings.Replace(string(original), `"use":"sig"`, `"use":"sig","d":"synthetic-private-input"`, 1),
		strings.Replace(string(original), `"kid":"rfc8032-test-1"`, `"kid":"../key"`, 1),
		strings.Replace(string(original), `"keys":`, `"untrusted_url":"https://invalid.example/keys","keys":`, 1),
	} {
		if err := os.WriteFile(c.publicKeyFile, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readOAuthPublicKeys(c.publicKeyFile); !errors.Is(err, errOAuthKeys) {
			t.Fatalf("invalid key configuration did not fail closed: %v", err)
		}
	}
}

func TestOAuthPublicKeyRotationWithoutCachedRetirement(t *testing.T) {
	c := vectorConfig(t)
	if _, err := c.verifyAccessToken(vectorToken, vectorNow); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kid": "rotated", "kty": "OKP", "crv": "Ed25519", "x": b64.EncodeToString(publicKey), "alg": "EdDSA", "use": "sig",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.publicKeyFile+".new", document, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(c.publicKeyFile+".new", c.publicKeyFile); err != nil {
		t.Fatal(err)
	}
	if _, err := c.verifyAccessToken(vectorToken, vectorNow); !errors.Is(err, errTokenSignature) {
		t.Fatalf("retired key accepted: %v", err)
	}
	rotated := signJWT(hex.EncodeToString(privateKey.Seed()), `{"alg":"EdDSA","typ":"at+jwt","kid":"rotated"}`, claimsJSON(nil))
	if _, err := c.verifyAccessToken(rotated, vectorNow); err != nil {
		t.Fatalf("active key rejected: %v", err)
	}
}

func TestOAuthKeyFailureReturns503WithoutStaticFallback(t *testing.T) {
	c := vectorConfig(t)
	if err := os.Remove(c.publicKeyFile); err != nil {
		t.Fatal(err)
	}
	called := false
	handler := bearerAuth(authenticator{oauth: c, now: func() time.Time { return vectorNow }}, newFailureLimiter(), false,
		slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+vectorToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || called || strings.Contains(response.Body.String(), c.publicKeyFile) {
		t.Fatalf("unavailable verification must return a fixed 503 before the handler: %d, called=%v", response.Code, called)
	}
}
