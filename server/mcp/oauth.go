package main

// OAuth verifies Ed25519 tokens using public configuration only.
// Signature verification alone does not establish current grant permission.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const (
	// The one scope the viewer grants: read everything this server serves.
	oauthScope = "health:read"
	// Longest bearer the verifier will look at. A real token is ~450 bytes.
	maxAccessTokenLen = 4096
	// Clock skew tolerated between the viewer and this process, on both exp
	// and iat.
	jwtLeeway = 60 * time.Second
	// The protected-resource metadata document's well-known prefix (RFC 9728).
	protectedResourcePath = "/.well-known/oauth-protected-resource"
	// The SDK binds an MCP session to the TokenInfo.UserID that created it
	// and refuses the session to any other. The static PULS_MCP_TOKEN gets
	// this identity — not a UUID, so it can never equal a token's sub — so a
	// session opened with it cannot be taken over by a signed-in person, and
	// the reverse.
	staticTokenIdentity = "puls-mcp-token"
)

// oauthConfig is the resource server's half of the shared contract. nil
// (none of the three variables set) means OAuth is off.
type oauthConfig struct {
	publicKeyFile string
	// resource is PULS_MCP_URL: this server's public URL, path included,
	// and the only audience a token may name.
	resource string
	// issuer is PULS_MCP_OAUTH_ISSUER: the viewer's public origin.
	issuer string
	// metadataURL is where the 401's WWW-Authenticate points: the origin of
	// resource, then the well-known prefix, then resource's path.
	metadataURL string
	// metadataPath is metadataURL's path, served by this process.
	metadataPath string
}

// loadOAuthConfig rejects retired shared secrets and requires a local public key set.
func loadOAuthConfig(getenv func(string) string) (*oauthConfig, error) {
	if strings.TrimSpace(getenv("PULS_MCP_OAUTH_SECRET")) != "" {
		return nil, errors.New("PULS_MCP_OAUTH_SECRET is retired; use PULS_AUTH_PUBLIC_KEYS_FILE")
	}
	if strings.TrimSpace(getenv("PULS_AUTH_PRIVATE_KEY_FILE")) != "" {
		return nil, errors.New("MCP must not receive the authorization signing private key")
	}
	publicKeyFile := strings.TrimSpace(getenv("PULS_AUTH_PUBLIC_KEYS_FILE"))
	rawURL := strings.TrimSpace(getenv("PULS_MCP_URL"))
	rawIssuer := strings.TrimSpace(getenv("PULS_MCP_OAUTH_ISSUER"))
	if publicKeyFile == "" && rawURL == "" {
		return nil, nil
	}
	if publicKeyFile == "" || rawURL == "" || rawIssuer == "" {
		return nil, errors.New("OAuth needs all of PULS_AUTH_PUBLIC_KEYS_FILE, PULS_MCP_URL and PULS_MCP_OAUTH_ISSUER " +
			"(Compose sets the issuer from the viewer's WEB_PUBLIC_URL); set all three, or neither of the first two " +
			"to serve only PULS_MCP_TOKEN")
	}
	if _, err := readOAuthPublicKeys(publicKeyFile); err != nil {
		return nil, err
	}

	resource, err := parsePublicURL("PULS_MCP_URL", rawURL)
	if err != nil {
		return nil, err
	}
	if resource.Path == "" || resource.Path == "/" {
		return nil, fmt.Errorf("PULS_MCP_URL %q must include the endpoint's path, e.g. https://mcp.example.com/mcp", rawURL)
	}
	issuer, err := parsePublicURL("PULS_MCP_OAUTH_ISSUER", rawIssuer)
	if err != nil {
		return nil, err
	}
	if strings.TrimRight(issuer.Path, "/") != "" {
		return nil, fmt.Errorf("PULS_MCP_OAUTH_ISSUER %q must be an origin (the viewer's WEB_PUBLIC_URL), with no path", rawIssuer)
	}

	origin := resource.Scheme + "://" + resource.Host
	metadataPath := protectedResourcePath + strings.TrimRight(resource.Path, "/")
	return &oauthConfig{
		publicKeyFile: publicKeyFile,
		// Compared byte for byte with a token's aud and iss, so kept as
		// written: the viewer signs these from the same variables.
		resource:     rawURL,
		issuer:       strings.TrimRight(rawIssuer, "/"),
		metadataURL:  origin + metadataPath,
		metadataPath: metadataPath,
	}, nil
}

// parsePublicURL accepts an absolute https URL, or plain http for a loopback
// host (local testing). No user info, query or fragment.
func parsePublicURL(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("%s %q is not an absolute URL without query or fragment", name, raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("%s %q must be https:// (plain http only for localhost)", name, raw)
		}
	default:
		return nil, fmt.Errorf("%s %q must be https://", name, raw)
	}
	return u, nil
}

// metadata is the RFC 9728 protected-resource document.
func (c *oauthConfig) metadata() *oauthex.ProtectedResourceMetadata {
	return &oauthex.ProtectedResourceMetadata{
		Resource:               c.resource,
		AuthorizationServers:   []string{c.issuer},
		ScopesSupported:        []string{oauthScope},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "PulsHealth",
	}
}

// metadataHandler serves the document (CORS-open, GET/OPTIONS only).
func (c *oauthConfig) metadataHandler() http.Handler {
	return auth.ProtectedResourceMetadataHandler(c.metadata())
}

// jwtClaims are the claims the contract defines. Typed fields make a claim
// of the wrong JSON type (an aud array, a fractional exp) a decode error;
// pointers make a missing number detectable.
type jwtClaims struct {
	Iss      string `json:"iss"`
	Aud      string `json:"aud"`
	Sub      string `json:"sub"`
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
	Iat      *int64 `json:"iat"`
	Exp      *int64 `json:"exp"`
	Jti      string `json:"jti"`
}

// errTokenMalformed and errTokenSignature mean the signature was never
// verified: the bearer could be a guess. errTokenClaims means it was — the
// token was signed by an active authorization key and is only expired, for
// another audience, unreadable after signing, and so on — so it guesses
// nothing (see tokenSigned and bearerAuth).
var (
	errTokenMalformed = errors.New("malformed token")
	errTokenSignature = errors.New("bad signature")
	errTokenClaims    = errors.New("claims rejected")
)

// tokenSigned reports whether a verifyAccessToken result carries a signature
// this server verified, valid claims or not.
func tokenSigned(err error) bool {
	return err == nil || errors.Is(err, errTokenClaims)
}

// b64 is unpadded base64url, strict about non-canonical trailing bits so
// one token has one spelling.
var b64 = base64.RawURLEncoding.Strict()

// verifyAccessToken checks an access token and returns the person it acts
// for. The checks run in this order: shape and size, header, signature
// (constant time), and only then the claims, so nothing unsigned is
// interpreted beyond the header. Every failure after the signature check is
// errTokenClaims (tokenSigned).
func (c *oauthConfig) verifyAccessToken(token string, now time.Time) (*jwtClaims, error) {
	if len(token) > maxAccessTokenLen {
		return nil, fmt.Errorf("%w: too long", errTokenMalformed)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: %d parts", errTokenMalformed, len(parts))
	}
	headerJSON, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header encoding", errTokenMalformed)
	}
	var header map[string]any
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("%w: header json", errTokenMalformed)
	}
	kid, ok := header["kid"].(string)
	if !ok || !oauthKeyID.MatchString(kid) || len(header) != 3 || header["alg"] != "EdDSA" || header["typ"] != "at+jwt" ||
		string(headerJSON) != fmt.Sprintf(`{"alg":"EdDSA","typ":"at+jwt","kid":%q}`, kid) {
		return nil, fmt.Errorf("%w: header", errTokenMalformed)
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature encoding", errTokenMalformed)
	}
	keys, err := readOAuthPublicKeys(c.publicKeyFile)
	if err != nil {
		return nil, err
	}
	publicKey := keys[kid]
	if len(publicKey) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize || !ed25519.Verify(publicKey, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errTokenSignature
	}

	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload encoding", errTokenClaims)
	}
	var claims jwtClaims
	dec := json.NewDecoder(bytes.NewReader(payload))
	if err := dec.Decode(&claims); err != nil || dec.More() {
		return nil, fmt.Errorf("%w: payload json", errTokenClaims)
	}
	switch {
	case claims.Iss != c.issuer:
		return nil, fmt.Errorf("%w: issuer", errTokenClaims)
	case claims.Aud != c.resource:
		return nil, fmt.Errorf("%w: audience", errTokenClaims)
	case claims.Exp == nil || claims.Iat == nil:
		return nil, fmt.Errorf("%w: missing exp or iat", errTokenClaims)
	case !now.Before(time.Unix(*claims.Exp, 0).Add(jwtLeeway)):
		return nil, fmt.Errorf("%w: expired", errTokenClaims)
	case time.Unix(*claims.Iat, 0).After(now.Add(jwtLeeway)):
		return nil, fmt.Errorf("%w: issued in the future", errTokenClaims)
	case !isUUID(claims.Sub) || claims.Sub != strings.ToLower(claims.Sub):
		return nil, fmt.Errorf("%w: subject", errTokenClaims)
	case !slices.Contains(strings.Fields(claims.Scope), oauthScope):
		return nil, fmt.Errorf("%w: scope", errTokenClaims)
	}
	return &claims, nil
}

// authenticator decides who a bearer token is: the static PULS_MCP_TOKEN
// (when set), else, with OAuth on, a verified access token.
type authenticator struct {
	staticToken string
	oauth       *oauthConfig
	now         func() time.Time
}

// staticAuth is an authenticator that knows only the static token (OAuth off).
func staticAuth(token string) authenticator {
	return authenticator{staticToken: token, now: time.Now}
}

// verifySigned checks a bearer as an access token (OAuth on only). It is
// the step bearerAuth runs *before* the limiter: a signed token proves
// knowledge of nothing an attacker can guess, and the signature check is bounded
// compared in constant time over at most maxAccessTokenLen bytes, so it is
// as cheap to run for a throttled address as the limiter itself. ok is false
// when the bearer is not a token an active public key verifies; the caller
// then treats it as a guess.
func (a authenticator) verifySigned(bearer string) (info *auth.TokenInfo, reason error, ok bool) {
	if a.oauth == nil {
		return nil, nil, false
	}
	claims, err := a.oauth.verifyAccessToken(bearer, a.now())
	if !tokenSigned(err) {
		return nil, err, false
	}
	if err != nil {
		return nil, err, true
	}
	return &auth.TokenInfo{
		UserID:     claims.Sub,
		Scopes:     strings.Fields(claims.Scope),
		Expiration: time.Unix(*claims.Exp, 0),
		Extra:      map[string]any{"client_id": claims.ClientID},
	}, nil, true
}

// staticMatches reports whether a bearer is the static PULS_MCP_TOKEN.
func (a authenticator) staticMatches(bearer string) bool {
	return a.staticToken != "" && constantTimeEqual(bearer, a.staticToken)
}

// challenge is the WWW-Authenticate value of a 401.
func (a authenticator) challenge(presented bool) string {
	if a.oauth == nil {
		return `Bearer realm="pulshealth-mcp"`
	}
	v := fmt.Sprintf(`Bearer realm="pulshealth-mcp", resource_metadata=%q`, a.oauth.metadataURL)
	if presented {
		v += `, error="invalid_token"`
	}
	return v
}

type tokenInfoCtxKey struct{}

// withSDKTokenInfo hands an already-authenticated request's TokenInfo to the
// SDK. Its context key is unexported, so the SDK's own RequireBearerToken is
// the only way in; here its verifier just returns what bearerAuth decided.
// The SDK then records TokenInfo.UserID on the session a request creates and
// refuses that session (403) to any request carrying another identity, and
// passes TokenInfo to every tool call as req.Extra.TokenInfo.
func withSDKTokenInfo(next http.Handler) http.Handler {
	verifier := func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		if ti, ok := ctx.Value(tokenInfoCtxKey{}).(*auth.TokenInfo); ok && ti != nil {
			return ti, nil
		}
		return nil, auth.ErrInvalidToken
	}
	return auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{
		// The static token has no expiry; an access token's was checked
		// (with the same leeway) by verifyAccessToken.
		AllowMissingExpiration: true,
		ClockSkew:              jwtLeeway,
	})(next)
}
