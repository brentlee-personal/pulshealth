package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIntegration_FamilyHealthDisconnectBeforeCommit(t *testing.T) {
	for _, partial := range []bool{true, false} {
		t.Run(fmt.Sprintf("partial_body=%t", partial), func(t *testing.T) {
			f := familyTestSetup(t)
			completed := make(chan struct{})
			handler := f.server.Config.Handler
			observed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(w, r)
				close(completed)
			}))
			t.Cleanup(observed.Close)
			lock, err := f.admin.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(f.ctx)
			if _, err := lock.Exec(f.ctx, "LOCK TABLE family_batch_receipts IN EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			connection, err := net.DialTimeout("tcp", strings.TrimPrefix(observed.URL, "http://"), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			batch := f.uuid(0x6e0)
			body := f.body(batch, f.uuid(0x6e1), 62.5)
			if _, err := fmt.Fprintf(connection, "POST /v1/batches HTTP/1.1\r\nHost: localhost\r\nContent-Length: %d\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n", len(body), f.actors[0].token); err != nil {
				t.Fatal(err)
			}
			payload := body
			if partial {
				payload = body[:len(body)/2]
			}
			if _, err := io.WriteString(connection, payload); err != nil {
				t.Fatal(err)
			}
			if !partial {
				waitReceiptWriters(t, f, 1)
			}
			connection.Close()
			if err := lock.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-completed:
			case <-f.ctx.Done():
				t.Fatal("disconnected request did not finish")
			}
			want := 1
			if partial {
				want = 0
			}
			for _, table := range []string{"batches", "family_batch_receipts", "quantity_samples"} {
				var count int
				if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE user_id=$1", f.actors[0].user).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != want {
					t.Fatalf("%s has %d rows before retry, want %d", table, count, want)
				}
			}
			var originalTransaction string
			if !partial {
				if err := f.admin.QueryRow(f.ctx, "SELECT transaction_id::text FROM family_batch_receipts WHERE user_id=$1 AND batch_id=$2", f.actors[0].user, batch).Scan(&originalTransaction); err != nil {
					t.Fatal(err)
				}
			}
			if status, _ := f.post(t, f.actors[0], body); status != http.StatusOK {
				t.Fatalf("retry status=%d", status)
			}
			var count int
			var transaction string
			if err := f.admin.QueryRow(f.ctx, `SELECT count(*), max(transaction_id::text) FROM family_batch_receipts
				WHERE user_id=$1 AND batch_id=$2`, f.actors[0].user, batch).Scan(&count, &transaction); err != nil {
				t.Fatal(err)
			}
			if count != 1 || (!partial && transaction != originalTransaction) {
				t.Fatal("retry after disconnect changed or duplicated the committed transaction")
			}
		})
	}
}
