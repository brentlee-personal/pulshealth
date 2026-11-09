package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIntegration_AccountCalendarAndDeletedOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	admin := adminPool(t, ctx)
	store := NewStore(pool)
	run := time.Now().UnixNano()
	user := fmt.Sprintf("%08x-0340-4000-8000-%012x", run>>32, run&0xffffffffffff)
	_, token, err := store.IssueDeviceToken(ctx, user, "calendar test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM batches WHERE user_id=$1`, user)
		_, _ = admin.Exec(context.Background(), `DELETE FROM device_tokens WHERE user_id=$1`, user)
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, user)
	})
	batch := func(n int, zone string) *Batch {
		header := BatchHeader{
			BatchID: fmt.Sprintf("%08x-%04x-4000-8000-%012x", run>>32, 0x340+n, run&0xffffffffffff),
			UserID:  user, DeviceTokenID: token.ID, DeviceID: "calendar-test", Type: "probe", Reason: "manual", TimeZoneID: zone,
		}
		wire, err := json.Marshal(header)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseBatch(strings.NewReader(string(wire) + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		parsed.Header.UserID, parsed.Header.DeviceTokenID = user, token.ID
		return parsed
	}
	assertZone := func(want string, pending bool) {
		t.Helper()
		var zone string
		var flag bool
		if err := admin.QueryRow(ctx, `SELECT coalesce(time_zone,''), time_zone_auto_initialize FROM users WHERE id=$1`, user).Scan(&zone, &flag); err != nil {
			t.Fatal(err)
		}
		if zone != want || flag != pending {
			t.Fatalf("zone=%q pending=%v; want %q %v", zone, flag, want, pending)
		}
	}
	// Legacy/self-hosted user preserves the deployment fallback.
	if _, err := store.InsertBatch(ctx, batch(0, "Asia/Tokyo"), 0); err != nil {
		t.Fatal(err)
	}
	assertZone("", false)
	if _, err := admin.Exec(ctx, `UPDATE users SET time_zone_auto_initialize=true WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	// An old client does not consume the new account's initialization opportunity.
	if _, err := store.InsertBatch(ctx, batch(1, ""), 0); err != nil {
		t.Fatal(err)
	}
	assertZone("", true)
	if _, err := store.InsertBatch(ctx, batch(2, "Asia/Tokyo"), 0); err != nil {
		t.Fatal(err)
	}
	assertZone("Asia/Tokyo", false)
	if _, err := store.InsertBatch(ctx, batch(3, "America/Los_Angeles"), 0); err != nil {
		t.Fatal(err)
	}
	assertZone("Asia/Tokyo", false)
	// One upload may hold these locks for its whole transaction. Another
	// upload must still proceed; account deletion must not acquire its
	// exclusive user lock until all of the uploads have completed.
	reader, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(ctx)
	var lockedUser string
	if err := reader.QueryRow(ctx, `SELECT id::text FROM users WHERE id=$1 FOR KEY SHARE`, user).Scan(&lockedUser); err != nil {
		t.Fatal(err)
	}
	var lockedToken int64
	if err := reader.QueryRow(ctx, `SELECT id FROM device_tokens WHERE id=$1 FOR SHARE`, token.ID).Scan(&lockedToken); err != nil {
		t.Fatal(err)
	}
	parallelCtx, parallelCancel := context.WithTimeout(ctx, 3*time.Second)
	_, parallelErr := store.InsertBatch(parallelCtx, batch(5, "Asia/Tokyo"), 0)
	parallelCancel()
	if parallelErr != nil {
		t.Fatalf("concurrent upload blocked by existing upload read locks: %v", parallelErr)
	}
	lockErr := admin.QueryRow(ctx, `SELECT id::text FROM users WHERE id=$1 FOR UPDATE NOWAIT`, user).Scan(&lockedUser)
	var pgErr *pgconn.PgError
	if !errors.As(lockErr, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("account deletion must not acquire exclusive user lock during upload: %v", lockErr)
	}
	if err := reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// A request authenticated before revocation must not write after it, even
	// when its batch ID would otherwise take the duplicate success path.
	if _, err := admin.Exec(ctx, `UPDATE device_tokens SET status='revoked' WHERE id=$1`, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertBatch(ctx, batch(3, "UTC"), 0); err == nil {
		t.Fatal("revoked in-flight token accepted")
	}
	if _, err := admin.Exec(ctx, `DELETE FROM batches WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM device_tokens WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM users WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertBatch(ctx, batch(4, "UTC"), 0); err == nil {
		t.Fatal("deleted in-flight owner accepted")
	}
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, user).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("deleted user recreated")
	}
}
