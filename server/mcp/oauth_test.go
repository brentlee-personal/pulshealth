package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The shared contract's test vector (the web viewer's tests sign the same).
const (
	vectorSeed   = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	vectorIssuer = "https://viewer.example.com"
	vectorURL    = "https://mcp.example.com/mcp"
	vectorSub    = "5ea4d000-0000-4000-8000-000000000001"
	vectorToken  = "eyJhbGciOiJFZERTQSIsInR5cCI6ImF0K2p3dCIsImtpZCI6InJmYzgwMzItdGVzdC0xIn0.eyJpc3MiOiJodHRwczovL3ZpZXdlci5leGFtcGxlLmNvbSIsImF1ZCI6Imh0dHBzOi8vbWNwLmV4YW1wbGUuY29tL21jcCIsInN1YiI6IjVlYTRkMDAwLTAwMDAtNDAwMC04MDAwLTAwMDAwMDAwMDAwMSIsImNsaWVudF9pZCI6InBjX3Rlc3QiLCJzY29wZSI6ImhlYWx0aDpyZWFkIiwiaWF0IjoxNzkwMDAwMDAwLCJleHAiOjE3OTAwMDE4MDAsImp0aSI6IkFBQUFBQUFBQUFBQUFBQUFBQUFBQUEifQ.jkcgRYALjcIxpK9zgBjCn2BzUTqYCHFkiCsdKuBmZbhizq6JWvgUIja1pFnb_Ci4W09ZFtT-ZXUv5NsblWRJCg"
	vectorIat    = 1790000000
	vectorExp    = 1790001800
)

func vectorEnv(t *testing.T) map[string]string {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "public.json")
	if err := os.WriteFile(keyFile, []byte(`{"keys":[{"kid":"rfc8032-test-1","kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo","alg":"EdDSA","use":"sig"}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"PULS_API_TOKEN":             "t",
		"PULS_AUTH_PUBLIC_KEYS_FILE": keyFile,
		"PULS_MCP_URL":               vectorURL,
		"PULS_MCP_OAUTH_ISSUER":      vectorIssuer,
	}
}

func vectorConfig(t *testing.T) *oauthConfig {
	t.Helper()
	m := vectorEnv(t)
	c, err := loadOAuthConfig(func(k string) string { return m[k] })
	if err != nil || c == nil {
		t.Fatalf("loadOAuthConfig: %v, %v", c, err)
	}
	return c
}

// signJWT signs with a public test seed; no deployment key is used.
func signJWT(seed, header, payload string) string {
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString([]byte(payload))
	bytes, err := hex.DecodeString(seed)
	if err != nil || len(bytes) != ed25519.SeedSize {
		panic("invalid test seed")
	}
	return signing + "." + enc.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(bytes), []byte(signing)))
}

const goodHeader = `{"alg":"EdDSA","typ":"at+jwt","kid":"rfc8032-test-1"}`

// claimsJSON renders the contract's claims in its key order, with overrides
// (a nil value drops the key).
func claimsJSON(over map[string]any) string {
	keys := []string{"iss", "aud", "sub", "client_id", "scope", "iat", "exp", "jti"}
	vals := map[string]any{
		"iss": vectorIssuer, "aud": vectorURL, "sub": vectorSub, "client_id": "pc_test",
		"scope": "health:read", "iat": vectorIat, "exp": vectorExp, "jti": "AAAAAAAAAAAAAAAAAAAAAA",
	}
	for k, v := range over {
		if _, ok := vals[k]; !ok {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for _, k := range keys {
		v, ok := vals[k]
		if !ok || v == nil {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(k)
		var vb []byte
		if raw, isRaw := v.(json.RawMessage); isRaw {
			vb = raw
		} else {
			vb, _ = json.Marshal(v)
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}

var vectorNow = time.Unix(vectorIat+100, 0)

func TestAccessTokenVector(t *testing.T) {
	// The test helper signs exactly the contract's token.
	if got := signJWT(vectorSeed, goodHeader, claimsJSON(nil)); got != vectorToken {
		t.Fatalf("signJWT(vector) =\n%s\nwant\n%s", got, vectorToken)
	}
	c := vectorConfig(t)
	claims, err := c.verifyAccessToken(vectorToken, vectorNow)
	if err != nil {
		t.Fatalf("vector rejected: %v", err)
	}
	if claims.Sub != vectorSub || claims.ClientID != "pc_test" || *claims.Exp != vectorExp {
		t.Errorf("claims = %+v", claims)
	}
	// Leeway: valid to exp+60 s, and an iat up to 60 s ahead.
	if _, err := c.verifyAccessToken(vectorToken, time.Unix(vectorExp+59, 0)); err != nil {
		t.Errorf("within the leeway after exp: %v", err)
	}
	if _, err := c.verifyAccessToken(vectorToken, time.Unix(vectorIat-59, 0)); err != nil {
		t.Errorf("iat within the leeway ahead: %v", err)
	}
}

func TestAccessTokenRejections(t *testing.T) {
	c := vectorConfig(t)
	good := claimsJSON(nil)
	flip := func(tok string) string {
		b := []byte(tok)
		i := len(b) - 5
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return string(b)
	}
	enc := base64.RawURLEncoding.EncodeToString
	cases := map[string]struct {
		token string
		now   time.Time
	}{
		"alg none, unsigned":  {enc([]byte(`{"alg":"none","typ":"at+jwt"}`)) + "." + enc([]byte(good)) + ".", vectorNow},
		"alg none, signed":    {signJWT(vectorSeed, `{"alg":"none","typ":"at+jwt"}`, good), vectorNow},
		"alg RS256":           {signJWT(vectorSeed, `{"alg":"RS256","typ":"at+jwt"}`, good), vectorNow},
		"alg HS512":           {signJWT(vectorSeed, `{"alg":"HS512","typ":"at+jwt"}`, good), vectorNow},
		"alg hs256":           {signJWT(vectorSeed, `{"alg":"hs256","typ":"at+jwt"}`, good), vectorNow},
		"typ JWT":             {signJWT(vectorSeed, `{"alg":"EdDSA","typ":"JWT","kid":"rfc8032-test-1"}`, good), vectorNow},
		"typ missing":         {signJWT(vectorSeed, `{"alg":"EdDSA","kid":"rfc8032-test-1"}`, good), vectorNow},
		"unknown kid":         {signJWT(vectorSeed, `{"alg":"EdDSA","typ":"at+jwt","kid":"unknown"}`, good), vectorNow},
		"untrusted key URL":   {signJWT(vectorSeed, `{"alg":"EdDSA","typ":"at+jwt","kid":"rfc8032-test-1","jku":"https://invalid.example"}`, good), vectorNow},
		"duplicate algorithm": {signJWT(vectorSeed, `{"alg":"EdDSA","alg":"EdDSA","typ":"at+jwt","kid":"rfc8032-test-1"}`, good), vectorNow},
		"header not object":   {signJWT(vectorSeed, `["HS256"]`, good), vectorNow},
		"bad signature":       {flip(vectorToken), vectorNow},
		"other secret":        {signJWT(strings.Repeat("00", 32), goodHeader, good), vectorNow},
		"signature stripped":  {vectorToken[:strings.LastIndex(vectorToken, ".")+1], vectorNow},
		"wrong issuer":        {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"iss": "https://evil.example.com"})), vectorNow},
		"issuer trailing /":   {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"iss": vectorIssuer + "/"})), vectorNow},
		"wrong audience":      {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"aud": "https://other.example.com/mcp"})), vectorNow},
		"audience array":      {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"aud": []string{vectorURL}})), vectorNow},
		"expired":             {vectorToken, time.Unix(vectorExp+61, 0)},
		"iat in the future":   {vectorToken, time.Unix(vectorIat-61, 0)},
		"exp missing":         {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"exp": nil})), vectorNow},
		"iat missing":         {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"iat": nil})), vectorNow},
		"exp fractional":      {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"exp": json.RawMessage("1790001800.5")})), vectorNow},
		"exp string":          {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"exp": "1790001800"})), vectorNow},
		"sub not a UUID":      {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"sub": "alice"})), vectorNow},
		"sub upper case":      {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"sub": strings.ToUpper(vectorSub)})), vectorNow},
		"sub missing":         {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"sub": nil})), vectorNow},
		"scope missing":       {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"scope": nil})), vectorNow},
		"scope other":         {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"scope": "health:write"})), vectorNow},
		"scope prefix":        {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"scope": "health:readx"})), vectorNow},
		"payload not object":  {signJWT(vectorSeed, goodHeader, `"x"`), vectorNow},
		"trailing json":       {signJWT(vectorSeed, goodHeader, good+"{}"), vectorNow},
		"two parts":           {vectorToken[:strings.LastIndex(vectorToken, ".")], vectorNow},
		"four parts":          {vectorToken + ".x", vectorNow},
		"empty":               {"", vectorNow},
		"padded base64":       {strings.Replace(vectorToken, ".", "=.", 1), vectorNow},
		"oversize":            {signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"pad": strings.Repeat("a", maxAccessTokenLen)})), vectorNow},
		"static-looking junk": {"mcp-secret", vectorNow},
	}
	for name, tc := range cases {
		if claims, err := c.verifyAccessToken(tc.token, tc.now); err == nil {
			t.Errorf("%s: accepted (%+v)", name, claims)
		}
	}
	// Scope among others is fine.
	if _, err := c.verifyAccessToken(signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"scope": "openid health:read"})), vectorNow); err != nil {
		t.Errorf("scope list containing health:read: %v", err)
	}
}

func TestLoadOAuthConfig(t *testing.T) {
	load := func(over map[string]string) (*oauthConfig, error) {
		m := vectorEnv(t)
		for k, v := range over {
			m[k] = v
		}
		return loadOAuthConfig(func(k string) string { return m[k] })
	}
	c, err := load(map[string]string{"PULS_MCP_OAUTH_ISSUER": vectorIssuer + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.issuer != vectorIssuer || c.resource != vectorURL ||
		c.metadataURL != "https://mcp.example.com/.well-known/oauth-protected-resource/mcp" ||
		c.metadataPath != "/.well-known/oauth-protected-resource/mcp" {
		t.Errorf("config = %+v", c)
	}
	if c, err := loadOAuthConfig(func(string) string { return "" }); c != nil || err != nil {
		t.Errorf("none set = %v, %v; want off", c, err)
	}
	// The issuer alone (Compose passes WEB_PUBLIC_URL to every accounts-mode
	// install) leaves OAuth off rather than failing startup.
	if c, err := load(map[string]string{"PULS_AUTH_PUBLIC_KEYS_FILE": "", "PULS_MCP_URL": ""}); c != nil || err != nil {
		t.Errorf("issuer only = %v, %v; want off", c, err)
	}
	bad := map[string]map[string]string{
		"public keys only":  {"PULS_MCP_URL": "", "PULS_MCP_OAUTH_ISSUER": ""},
		"url only":          {"PULS_AUTH_PUBLIC_KEYS_FILE": "", "PULS_MCP_OAUTH_ISSUER": ""},
		"no issuer":         {"PULS_MCP_OAUTH_ISSUER": ""},
		"no url":            {"PULS_MCP_URL": " "},
		"no public keys":    {"PULS_AUTH_PUBLIC_KEYS_FILE": ""},
		"placeholder":       {"PULS_MCP_OAUTH_SECRET": "change-me"},
		"short secret":      {"PULS_MCP_OAUTH_SECRET": strings.Repeat("a", 31)},
		"private key":       {"PULS_AUTH_PRIVATE_KEY_FILE": "synthetic-forbidden-private-file"},
		"http url":          {"PULS_MCP_URL": "http://mcp.example.com/mcp"},
		"url without path":  {"PULS_MCP_URL": "https://mcp.example.com"},
		"url with query":    {"PULS_MCP_URL": "https://mcp.example.com/mcp?x=1"},
		"url relative":      {"PULS_MCP_URL": "/mcp"},
		"issuer with path":  {"PULS_MCP_OAUTH_ISSUER": "https://viewer.example.com/app"},
		"issuer ftp":        {"PULS_MCP_OAUTH_ISSUER": "ftp://viewer.example.com"},
		"issuer http":       {"PULS_MCP_OAUTH_ISSUER": "http://viewer.example.com"},
		"issuer with creds": {"PULS_MCP_OAUTH_ISSUER": "https://u:p@viewer.example.com"},
	}
	for name, over := range bad {
		if c, err := load(over); err == nil {
			t.Errorf("%s: accepted %+v", name, c)
		}
	}
	// Plain http is allowed for loopback testing.
	if _, err := load(map[string]string{"PULS_MCP_URL": "http://127.0.0.1:8082/mcp", "PULS_MCP_OAUTH_ISSUER": "http://localhost:3001"}); err != nil {
		t.Errorf("loopback http: %v", err)
	}
	// loadConfig carries it, and a partial setting fails startup.
	m := vectorEnv(t)
	cfg, err := loadConfig(func(k string) string { return m[k] })
	if err != nil || cfg.oauth == nil {
		t.Fatalf("loadConfig with OAuth: %+v, %v", cfg, err)
	}
	delete(m, "PULS_MCP_URL")
	if _, err := loadConfig(func(k string) string { return m[k] }); err == nil || !strings.Contains(err.Error(), "PULS_MCP_URL") {
		t.Errorf("partial OAuth config: err = %v", err)
	}
}

// oauthHarness serves s over HTTP with the static token "mcp-secret" and
// OAuth on, at the contract's URLs (the token's aud is the configured URL,
// not the test server's).
func oauthHarness(t *testing.T, s *service) (*httptest.Server, authenticator) {
	t.Helper()
	authn := authenticator{staticToken: "mcp-secret", oauth: vectorConfig(t), now: time.Now}
	ts := httptest.NewServer(s.httpHandler(s.newServer("test"), authn, false, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	return ts, authn
}

// tokenFor mints a currently valid access token for sub.
func tokenFor(sub string) string {
	now := time.Now().Unix()
	return signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"sub": sub, "iat": now, "exp": now + 1800}))
}

func TestProtectedResourceMetadata(t *testing.T) {
	f := newFakeAPI(t)
	ts, _ := oauthHarness(t, f.service(t, "UTC"))
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		err = json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d, %v", path, resp.StatusCode, err)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: no CORS header", path)
		}
		want := map[string]any{
			"resource": vectorURL, "authorization_servers": []any{vectorIssuer}, "scopes_supported": []any{"health:read"},
			"bearer_methods_supported": []any{"header"}, "resource_name": "PulsHealth",
		}
		got, _ := json.Marshal(doc)
		wantJSON, _ := json.Marshal(want)
		if string(got) != string(wantJSON) {
			t.Errorf("%s = %s, want %s", path, got, wantJSON)
		}
	}

	// OAuth off: no metadata, and the 401 is as before.
	off := httptest.NewServer(f.service(t, "UTC").httpHandler(mcp.NewServer(&mcp.Implementation{Name: "x"}, nil), staticAuth("mcp-secret"), false, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer off.Close()
	resp, err := http.Get(off.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("metadata with OAuth off = %d, want 404", resp.StatusCode)
	}
}

func TestOAuthChallengeAndLimiter(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	authn := authenticator{staticToken: "mcp-secret", oauth: vectorConfig(t), now: func() time.Time { return vectorNow }}
	reached := 0
	h := bearerAuth(authn, newFailureLimiter(), false, logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))
	const meta = `resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"`

	rec := mcpAttempt(t, h, "", "192.0.2.1:1", "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), meta) ||
		strings.Contains(rec.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Errorf("no token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	rec = mcpAttempt(t, h, "Bearer "+flipLast(vectorToken), "192.0.2.1:1", "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), meta+`, error="invalid_token"`) {
		t.Errorf("bad token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// Valid tokens never charge the limiter.
	for i := 0; i < authFailureBurst*2; i++ {
		if rec := mcpAttempt(t, h, "Bearer "+vectorToken, "192.0.2.2:1", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("valid token %d = %d", i, rec.Code)
		}
	}
	// A signed but expired token guesses nothing (forging it needs the
	// secret): it is a 401 invalid_token, never charged, however often.
	expired := signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"exp": vectorIat}))
	wrongAud := signJWT(vectorSeed, goodHeader, claimsJSON(map[string]any{"aud": "https://other.example.com/mcp"}))
	for i := 0; i < authFailureBurst*3; i++ {
		for _, tok := range []string{expired, wrongAud} {
			rec := mcpAttempt(t, h, "Bearer "+tok, "192.0.2.3:1", "")
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
				t.Fatalf("signed invalid token %d = %d %q, want 401 invalid_token", i, rec.Code, rec.Header().Get("WWW-Authenticate"))
			}
		}
	}
	if rec := mcpAttempt(t, h, "Bearer "+vectorToken, "192.0.2.3:1", ""); rec.Code != http.StatusNoContent {
		t.Errorf("valid token after many expired ones = %d, want it served", rec.Code)
	}
	// Bearer-less discovery requests are never charged nor refused.
	for i := 0; i < authFailureBurst*3; i++ {
		rec := mcpAttempt(t, h, "", "192.0.2.4:1", "")
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), meta) {
			t.Fatalf("discovery %d = %d %q, want 401 with the challenge", i, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
	if rec := mcpAttempt(t, h, "Bearer "+vectorToken, "192.0.2.4:1", ""); rec.Code != http.StatusNoContent {
		t.Errorf("valid token after discovery requests = %d", rec.Code)
	}

	// Guesses — garbage, a forged signature, a wrong static token — are
	// still charged and, once the budget is spent, refused before compare.
	guesses := []string{"garbage", flipLast(vectorToken), "mcp-secreu", signJWT(strings.Repeat("00", 32), goodHeader, claimsJSON(nil))}
	for i := 0; i < authFailureBurst; i++ {
		if rec := mcpAttempt(t, h, "Bearer "+guesses[i%len(guesses)], "192.0.2.5:1", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d, want 401", i, rec.Code)
		}
	}
	for _, g := range append(guesses, "mcp-secret") {
		if rec := mcpAttempt(t, h, "Bearer "+g, "192.0.2.5:1", ""); rec.Code != http.StatusTooManyRequests {
			t.Errorf("guess or static token after the burst = %d, want 429", rec.Code)
		}
	}
	// An exhausted address still admits a correctly signed valid token (its
	// HMAC is checked before the limiter), still answers discovery with the
	// challenge, and still says invalid_token to a signed expired one.
	if rec := mcpAttempt(t, h, "Bearer "+vectorToken, "192.0.2.5:1", ""); rec.Code != http.StatusNoContent {
		t.Errorf("valid access token from an exhausted address = %d, want it served", rec.Code)
	}
	if rec := mcpAttempt(t, h, "", "192.0.2.5:1", ""); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), meta) {
		t.Errorf("discovery from an exhausted address = %d, want 401 with the challenge", rec.Code)
	}
	if rec := mcpAttempt(t, h, "Bearer "+expired, "192.0.2.5:1", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed expired token from an exhausted address = %d, want 401", rec.Code)
	}

	if want := authFailureBurst*2 + 3; reached != want {
		t.Errorf("reached %d, want %d", reached, want)
	}
	out := logs.String()
	for _, want := range []string{"expired", "audience", "no bearer", "bad signature", "auth throttled"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q not logged: %s", want, out)
		}
	}
	if strings.Contains(out, vectorToken[:40]) || strings.Contains(out, expired[40:80]) || strings.Contains(out, "garbage") {
		t.Errorf("a token reached the log: %s", out)
	}
}

func flipLast(tok string) string {
	b := []byte(tok)
	if b[len(b)-3] == 'A' {
		b[len(b)-3] = 'B'
	} else {
		b[len(b)-3] = 'A'
	}
	return string(b)
}

func connectHTTP(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: token}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// A signed-in person reads as themselves only: every API call names their
// sub, a user argument naming anyone else is refused before the API is
// asked, and list_users shows their row alone. The static token keeps its
// old behaviour on the same server.
func TestOAuthActsOnlyForSub(t *testing.T) {
	f := newFakeAPI(t)
	f.respond("/v1/catalog/types", http.StatusOK, fixtureCatalog)
	f.respond("/v1/users", http.StatusOK, fixtureUsers)
	f.respond("/v1/profile", http.StatusOK, fixtureProfile)
	ts, _ := oauthHarness(t, f.service(t, "UTC"))
	ctx := context.Background()

	alice := connectHTTP(t, ts.URL, tokenFor(otherUserID))
	res, err := alice.CallTool(ctx, &mcp.CallToolParams{Name: "list_available_types", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_available_types: %v %+v", err, res)
	}
	if got := f.lastQuery(t, "/v1/catalog/types").Get("user"); got != otherUserID {
		t.Errorf("API asked for user %q, want the token's sub %q", got, otherUserID)
	}
	// Naming herself is fine; naming anyone else is refused.
	res, err = alice.CallTool(ctx, &mcp.CallToolParams{Name: "list_available_types", Arguments: map[string]any{"user": strings.ToUpper(otherUserID)}})
	if err != nil || res.IsError {
		t.Errorf("naming herself: %v %+v", err, res)
	}
	before := len(f.callsTo("/v1/profile"))
	res, err = alice.CallTool(ctx, &mcp.CallToolParams{Name: "get_profile", Arguments: map[string]any{"user": defaultUserID}})
	if err != nil || !res.IsError || !strings.Contains(textOf(t, res), "signed in as user "+otherUserID) {
		t.Errorf("naming another user: %v %+v", err, res)
	}
	if len(f.callsTo("/v1/profile")) != before {
		t.Error("the refused call reached the API")
	}
	res, err = alice.CallTool(ctx, &mcp.CallToolParams{Name: "list_users", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_users: %v %+v", err, res)
	}
	var users usersOutput
	if err := json.Unmarshal([]byte(textOf(t, res)), &users); err != nil {
		t.Fatal(err)
	}
	if len(users.Users) != 1 || users.Users[0].UserID != otherUserID || users.PinnedUserID != otherUserID ||
		users.DefaultUserID != otherUserID || !users.Users[0].IsDefault {
		t.Errorf("list_users for a signed-in person = %+v (the API's default must not leak)", users)
	}
	// The types resource is hers too.
	if _, err := alice.ReadResource(ctx, &mcp.ReadResourceParams{URI: typesURI}); err != nil {
		t.Fatal(err)
	}
	if got := f.lastQuery(t, "/v1/catalog/types").Get("user"); got != otherUserID {
		t.Errorf("types resource asked for user %q, want %q", got, otherUserID)
	}

	// The static token: unpinned, the API's default unless a call names one.
	static := connectHTTP(t, ts.URL, "mcp-secret")
	if res, err := static.CallTool(ctx, &mcp.CallToolParams{Name: "list_available_types", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("static list_available_types: %v %+v", err, res)
	}
	if q := f.lastQuery(t, "/v1/catalog/types"); q.Has("user") {
		t.Errorf("static token named user %q, want none", q.Get("user"))
	}
	res, err = static.CallTool(ctx, &mcp.CallToolParams{Name: "list_users", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatal(err, res)
	}
	users = usersOutput{}
	_ = json.Unmarshal([]byte(textOf(t, res)), &users)
	if len(users.Users) != 2 || users.PinnedUserID != "" {
		t.Errorf("static list_users = %+v, want everyone", users)
	}
}

// An access token for a PULS_USER_ID-pinned instance still acts for its own
// sub: the pin is the static token's, not a ceiling over signed-in people
// (their own sub is the pin for them).
func TestOAuthIgnoresStaticPin(t *testing.T) {
	f := newFakeAPI(t)
	f.respond("/v1/catalog/types", http.StatusOK, fixtureCatalog)
	s := f.service(t, "UTC")
	s.api = s.api.ForUser(defaultUserID)
	ts, _ := oauthHarness(t, s)
	alice := connectHTTP(t, ts.URL, tokenFor(otherUserID))
	if res, err := alice.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_available_types", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatal(err, res)
	}
	if got := f.lastQuery(t, "/v1/catalog/types").Get("user"); got != otherUserID {
		t.Errorf("user = %q, want %q", got, otherUserID)
	}
}

// postMCP sends one JSON-RPC message to /mcp.
func postMCP(t *testing.T, url, token, sessionID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A session belongs to the identity that opened it: neither another
// signed-in person nor the static token can ride it, and a static-token
// session is closed to signed-in people.
func TestSessionBoundToIdentity(t *testing.T) {
	f := newFakeAPI(t)
	f.respond("/v1/catalog/types", http.StatusOK, fixtureCatalog)
	ts, _ := oauthHarness(t, f.service(t, "UTC"))
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	const listTools = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	const initialized = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

	open := func(token string) string {
		resp := postMCP(t, ts.URL, token, "", initialize)
		id := resp.Header.Get("Mcp-Session-Id")
		if resp.StatusCode != http.StatusOK || id == "" {
			t.Fatalf("initialize = %d, session %q", resp.StatusCode, id)
		}
		if r := postMCP(t, ts.URL, token, id, initialized); r.StatusCode/100 != 2 {
			t.Fatalf("initialized = %d", r.StatusCode)
		}
		return id
	}
	alice, bob := tokenFor(otherUserID), tokenFor(defaultUserID)

	aliceSession := open(alice)
	if r := postMCP(t, ts.URL, alice, aliceSession, listTools); r.StatusCode != http.StatusOK {
		t.Fatalf("owner on her session = %d", r.StatusCode)
	}
	for name, token := range map[string]string{"another person": bob, "the static token": "mcp-secret"} {
		if r := postMCP(t, ts.URL, token, aliceSession, listTools); r.StatusCode != http.StatusForbidden {
			t.Errorf("%s on a signed-in session = %d, want 403", name, r.StatusCode)
		}
	}
	staticSession := open("mcp-secret")
	if r := postMCP(t, ts.URL, alice, staticSession, listTools); r.StatusCode != http.StatusForbidden {
		t.Errorf("a signed-in person on the static token's session = %d, want 403", r.StatusCode)
	}
	if r := postMCP(t, ts.URL, "mcp-secret", staticSession, listTools); r.StatusCode != http.StatusOK {
		t.Errorf("static token on its own session = %d", r.StatusCode)
	}
}

// Over stdio there is no token: the configured client, as always.
func TestCallerAPIWithoutToken(t *testing.T) {
	f := newFakeAPI(t)
	s := f.service(t, "UTC")
	api, signedIn, err := s.callerAPI(nil)
	if err != nil || signedIn || api != s.api {
		t.Errorf("callerAPI(nil) = %v %v %v", api, signedIn, err)
	}
}
