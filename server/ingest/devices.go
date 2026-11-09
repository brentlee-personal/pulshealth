package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Per-device tokens (SRV-8): the Store side. A device token is a bearer
// credential bound to one user, stored only as its SHA-256, revocable on its
// own and stamped with when it was last used. See db/migrations/014.

const (
	// A token is this many random bytes, hex-encoded — the same 64-character
	// shape as `openssl rand -hex 32`, so QR payloads, the app's token field
	// and every curl example stay as they are.
	deviceTokenBytes = 32
	// Characters of the plaintext kept next to the hash so an operator can
	// match a `devices list` row to the value on a phone. Far too short to
	// authenticate with.
	deviceTokenPrefixLen = 8
	// Longest operator label; anything past it is refused, not truncated.
	maxDeviceNameLen = 100
)

// errTokenNotFound is ResolveDeviceToken's answer for a hash no row carries.
// It is the one error the middleware turns into 401; everything else is a
// database failure and becomes 503.
var errTokenNotFound = errors.New("device token not found")

// deviceToken is one row of device_tokens, minus the hash.
type deviceToken struct {
	SourceConnectionID string
	SourceActive       bool
	ID                 int64
	UserID             string
	Name               string
	Status             string // "active" | "revoked"
	TokenPrefix        string
	CreatedAt          time.Time
	RevokedAt          *time.Time
	LastSeenAt         *time.Time
}

func (d deviceToken) active() bool { return d.Status == "active" }

// newDeviceToken draws a fresh plaintext token from crypto/rand.
func newDeviceToken() (string, error) {
	buf := make([]byte, deviceTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// hashToken is the stored form of a token: SHA-256 over its ASCII bytes. No
// salt and no KDF on purpose — the preimage is 256 random bits, so the hash
// is a lookup key rather than a password hash, and a salt would only cost the
// UNIQUE index that makes the lookup one indexed probe.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func tokenPrefix(token string) string {
	if len(token) < deviceTokenPrefixLen {
		return token
	}
	return token[:deviceTokenPrefixLen]
}

func validateDeviceName(name string) error {
	if utf8.RuneCountInString(name) > maxDeviceNameLen {
		return fmt.Errorf("name is longer than %d characters", maxDeviceNameLen)
	}
	return nil
}

// ResolveDeviceToken looks one hash up and, in the same round trip, advances
// the row's last_seen_at when the token is active and the stamp is older than
// a minute — so a backfill's thousands of requests cost one UPDATE a minute,
// not one per request. A revoked token is returned as such (status
// "revoked"), never touched; an unknown hash is errTokenNotFound.
func (st *Store) ResolveDeviceToken(ctx context.Context, hash []byte) (deviceToken, error) {
	var d deviceToken
	err := st.pool.QueryRow(ctx, `
		WITH t AS (
			SELECT id, user_id, status, token_prefix, last_seen_at, source_connection_id
			FROM device_tokens WHERE token_hash = $1
		), touch AS (
			UPDATE device_tokens d SET last_seen_at = now() FROM t
			WHERE d.id = t.id AND t.status = 'active'
			  AND (t.last_seen_at IS NULL OR t.last_seen_at < now() - interval '60 seconds')
		)
		SELECT t.id, t.user_id, t.status, t.token_prefix, coalesce(t.source_connection_id::text,''), coalesce(s.status='active',false)
		FROM t LEFT JOIN family_source_connections s ON s.user_id=t.user_id AND s.id=t.source_connection_id`, hash).
		Scan(&d.ID, &d.UserID, &d.Status, &d.TokenPrefix, &d.SourceConnectionID, &d.SourceActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return deviceToken{}, errTokenNotFound
	}
	if err != nil {
		return deviceToken{}, fmt.Errorf("resolve device token: %w", err)
	}
	return d, nil
}

// IssueDeviceToken mints a token for userID, creating the users row first so
// a household member can be issued a token before their phone has synced
// (which also means a mistyped UUID creates a user — the CLI says so). The
// plaintext is returned exactly once and is not recoverable afterwards.
func (st *Store) IssueDeviceToken(ctx context.Context, userID, name string) (string, deviceToken, error) {
	if !isUUID(userID) {
		return "", deviceToken{}, errors.New("user id is not a UUID")
	}
	if err := validateDeviceName(name); err != nil {
		return "", deviceToken{}, err
	}
	plaintext, err := newDeviceToken()
	if err != nil {
		return "", deviceToken{}, err
	}
	tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", deviceToken{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := ensureUser(ctx, tx, userID); err != nil {
		return "", deviceToken{}, fmt.Errorf("ensure user: %w", err)
	}
	d := deviceToken{UserID: userID, Name: name, Status: "active", TokenPrefix: tokenPrefix(plaintext)}
	err = tx.QueryRow(ctx, `
		INSERT INTO device_tokens (token_hash, token_prefix, user_id, name)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`,
		hashToken(plaintext), d.TokenPrefix, userID, name).Scan(&d.ID, &d.CreatedAt)
	if err != nil {
		return "", deviceToken{}, fmt.Errorf("insert device token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", deviceToken{}, fmt.Errorf("commit: %w", err)
	}
	return plaintext, d, nil
}

// RevokeDeviceToken marks a token revoked. It takes effect on the next request
// that presents it: nothing is cached. Revoking twice is not an error.
func (st *Store) RevokeDeviceToken(ctx context.Context, id int64) error {
	tag, err := st.pool.Exec(ctx, `
		UPDATE device_tokens
		SET status = 'revoked', revoked_at = coalesce(revoked_at, now())
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("revoke device token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errTokenNotFound
	}
	return nil
}

// RenameDeviceToken changes the operator label.
func (st *Store) RenameDeviceToken(ctx context.Context, id int64, name string) error {
	if err := validateDeviceName(name); err != nil {
		return err
	}
	tag, err := st.pool.Exec(ctx, `UPDATE device_tokens SET name = $2 WHERE id = $1`, id, name)
	if err != nil {
		return fmt.Errorf("rename device token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errTokenNotFound
	}
	return nil
}

// ListDeviceTokens returns every token, oldest first; revoked ones only when
// asked.
func (st *Store) ListDeviceTokens(ctx context.Context, includeRevoked bool) ([]deviceToken, error) {
	rows, err := st.pool.Query(ctx, `
		SELECT id, user_id, name, status, token_prefix, created_at, revoked_at, last_seen_at
		FROM device_tokens
		WHERE $1 OR status = 'active'
		ORDER BY id`, includeRevoked)
	if err != nil {
		return nil, fmt.Errorf("list device tokens: %w", err)
	}
	defer rows.Close()
	var out []deviceToken
	for rows.Next() {
		var d deviceToken
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Status, &d.TokenPrefix,
			&d.CreatedAt, &d.RevokedAt, &d.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan device token: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ActiveDeviceTokenCount is what startup checks when the shared token is off,
// so an install with no way to authenticate at all is told so in the log.
func (st *Store) ActiveDeviceTokenCount(ctx context.Context) (int64, error) {
	var n int64
	err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM device_tokens WHERE status = 'active'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count device tokens: %w", err)
	}
	return n, nil
}
