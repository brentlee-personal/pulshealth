package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var (
	errAPICredential = errors.New("invalid personal API credential")
	errAPIAuthority  = errors.New("personal authorization unavailable")
	apiBase64        = base64.RawURLEncoding.Strict()
	apiKeyID         = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	apiUTC           = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`)
)

type apiTimeRange struct {
	From  string `json:"from"`
	Until string `json:"until"`
}

type apiPrincipal struct {
	MemberID             string       `json:"member_id"`
	CredentialID         string       `json:"credential_id"`
	CredentialKind       string       `json:"credential_kind"`
	AllowedOperations    []string     `json:"allowed_operations"`
	AllowedTypes         []string     `json:"allowed_types"`
	AllowedTimeRange     apiTimeRange `json:"allowed_time_range"`
	AuthorizationVersion int64        `json:"authorization_version"`
}

type apiClaims struct {
	Issuer    string       `json:"iss"`
	Audience  string       `json:"aud"`
	MemberID  string       `json:"sub"`
	ClientID  string       `json:"client_id"`
	IssuedAt  int64        `json:"iat"`
	ExpiresAt int64        `json:"exp"`
	ID        string       `json:"jti"`
	GrantID   *string      `json:"grant_id,omitempty"`
	Principal apiPrincipal `json:"principal"`
}

type apiAuthority struct {
	Principal apiPrincipal
	TokenHash [32]byte
}

func (p apiPrincipal) valid() bool {
	if !isUUID(p.MemberID) || p.MemberID != strings.ToLower(p.MemberID) ||
		!isUUID(p.CredentialID) || p.CredentialID != strings.ToLower(p.CredentialID) ||
		p.AuthorizationVersion < 1 || p.AuthorizationVersion > 9007199254740991 ||
		(p.CredentialKind != "session" && p.CredentialKind != "exchange") ||
		len(p.AllowedOperations) == 0 || len(p.AllowedTypes) == 0 {
		return false
	}
	seen := make(map[string]bool)
	for _, op := range p.AllowedOperations {
		if seen[op] || (op != "metrics:read" && op != "samples:read" && (op != "data:export" || p.CredentialKind != "session")) {
			return false
		}
		seen[op] = true
	}
	clear(seen)
	for _, recordType := range p.AllowedTypes {
		if !utf8.ValidString(recordType) || utf8.RuneCountInString(recordType) < 1 || utf8.RuneCountInString(recordType) > 256 || seen[recordType] {
			return false
		}
		seen[recordType] = true
	}
	from, e1 := time.Parse(time.RFC3339Nano, p.AllowedTimeRange.From)
	until, e2 := time.Parse(time.RFC3339Nano, p.AllowedTimeRange.Until)
	return apiUTC.MatchString(p.AllowedTimeRange.From) && apiUTC.MatchString(p.AllowedTimeRange.Until) &&
		e1 == nil && e2 == nil && from.Year() >= 1 && until.Year() >= 1 && from.Before(until)
}

func (p apiPrincipal) equals(other apiPrincipal) bool {
	return p.MemberID == other.MemberID && p.CredentialID == other.CredentialID && p.CredentialKind == other.CredentialKind &&
		p.AuthorizationVersion == other.AuthorizationVersion && p.AllowedTimeRange == other.AllowedTimeRange &&
		slices.Equal(p.AllowedOperations, other.AllowedOperations) && slices.Equal(p.AllowedTypes, other.AllowedTypes)
}

// The database supplies live authority; a valid signature cannot revive its parent.
func (st *Store) authorizeAPI(ctx context.Context, bearer, issuer, keyFile string) (*apiAuthority, error) {
	if len(bearer) != 43 {
		return nil, errAPICredential
	}
	secret, err := apiBase64.DecodeString(bearer)
	if err != nil || len(secret) != 32 || apiBase64.EncodeToString(secret) != bearer {
		return nil, errAPICredential
	}
	hash := sha256.Sum256(secret)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var assertion, clientID string
	var principalJSON []byte
	var grantID *string
	var expiresAt time.Time
	err = st.pool.QueryRow(ctx, `SELECT assertion, principal, grant_id::text, client_id, expires_at
		FROM auth.resolve_api_credential($1)`, hash[:]).Scan(&assertion, &principalJSON, &grantID, &clientID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errAPICredential
	}
	if err != nil {
		return nil, errAPIAuthority
	}
	claims, err := verifyAPIAssertion(assertion, issuer, keyFile, time.Now())
	if err != nil {
		return nil, err
	}
	var principal apiPrincipal
	if strictAuthorityJSON(principalJSON, &principal) != nil || !principal.valid() || !principal.equals(claims.Principal) ||
		claims.ClientID != clientID || expiresAt.Nanosecond() != 0 || expiresAt.Unix() != claims.ExpiresAt ||
		(grantID == nil) != (claims.GrantID == nil) || (grantID != nil && *grantID != *claims.GrantID) {
		return nil, errAPICredential
	}
	return &apiAuthority{Principal: principal, TokenHash: hash}, nil
}

func verifyAPIAssertion(assertion, issuer, keyFile string, now time.Time) (*apiClaims, error) {
	if len(assertion) > 16384 || issuer == "" {
		return nil, errAPICredential
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return nil, errAPICredential
	}
	decoded := make([][]byte, 3)
	for i, part := range parts {
		b, err := apiBase64.DecodeString(part)
		if err != nil || apiBase64.EncodeToString(b) != part {
			return nil, errAPICredential
		}
		decoded[i] = b
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}
	if strictAuthorityJSON(decoded[0], &header) != nil || !apiKeyID.MatchString(header.KeyID) || header.Algorithm != "EdDSA" || header.Type != "at+jwt" ||
		string(decoded[0]) != fmt.Sprintf(`{"alg":"EdDSA","typ":"at+jwt","kid":%q}`, header.KeyID) {
		return nil, errAPICredential
	}
	keys, err := readAPIPublicKeys(keyFile)
	if err != nil {
		return nil, err
	}
	key := keys[header.KeyID]
	if len(key) != ed25519.PublicKeySize || len(decoded[2]) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), decoded[2]) {
		return nil, errAPICredential
	}
	var claims apiClaims
	if strictAuthorityJSON(decoded[1], &claims) != nil || !claims.Principal.valid() || claims.Issuer != issuer || claims.Audience != "family-health-api" ||
		claims.MemberID != claims.Principal.MemberID || claims.ID != claims.Principal.CredentialID ||
		claims.ClientID == "" || len(claims.ClientID) > 256 || claims.IssuedAt <= 0 || claims.IssuedAt > now.Unix()+60 ||
		claims.ExpiresAt <= now.Unix() || claims.ExpiresAt <= claims.IssuedAt {
		return nil, errAPICredential
	}
	maxLifetime := int64(1800)
	if claims.Principal.CredentialKind == "exchange" {
		maxLifetime = 60
		if claims.GrantID == nil || !isUUID(*claims.GrantID) || *claims.GrantID != strings.ToLower(*claims.GrantID) {
			return nil, errAPICredential
		}
	} else if claims.GrantID != nil || claims.ClientID != "family-health-web" {
		return nil, errAPICredential
	}
	if claims.ExpiresAt-claims.IssuedAt > maxLifetime {
		return nil, errAPICredential
	}
	return &claims, nil
}

func readAPIPublicKeys(path string) (map[string]ed25519.PublicKey, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errAPIAuthority
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > 16384 {
		return nil, errAPIAuthority
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, errAPIAuthority
	}
	var document struct {
		Keys []struct {
			ID    string `json:"kid"`
			Type  string `json:"kty"`
			Curve string `json:"crv"`
			X     string `json:"x"`
			Alg   string `json:"alg"`
			Use   string `json:"use"`
		} `json:"keys"`
	}
	if strictAuthorityJSON(data, &document) != nil || len(document.Keys) < 1 || len(document.Keys) > 8 {
		return nil, errAPIAuthority
	}
	keys := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, key := range document.Keys {
		public, err := apiBase64.DecodeString(key.X)
		if err != nil || len(public) != ed25519.PublicKeySize || apiBase64.EncodeToString(public) != key.X ||
			!apiKeyID.MatchString(key.ID) || keys[key.ID] != nil || key.Type != "OKP" || key.Curve != "Ed25519" || key.Alg != "EdDSA" || key.Use != "sig" {
			return nil, errAPIAuthority
		}
		keys[key.ID] = ed25519.PublicKey(public)
	}
	return keys, nil
}

func strictAuthorityJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errAPICredential
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return errAPICredential
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if token == json.Delim('{') {
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return errAPICredential
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
		} else if token == json.Delim('[') {
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
		}
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errAPICredential
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
