package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectedDatabaseCredentialFile(t *testing.T) {
	const marker = "synthetic-private-value"
	directory := t.TempDir()
	path := filepath.Join(directory, "private-location")
	value := "postgres://reader:" + marker + "@db/postgres"
	if err := os.WriteFile(path, []byte(value+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"DATABASE_URL_FILE": path}
	getenv := func(name string) string { return env[name] }
	got, err := readDatabaseURL(getenv)
	if err != nil || got != value {
		t.Fatal("private database file did not load exactly")
	}
	for _, raw := range []string{marker, "false"} {
		env["DATABASE_URL"] = raw
		if _, err := readDatabaseURL(getenv); err == nil || strings.Contains(err.Error(), marker) {
			t.Fatal("plaintext credential accepted or revealed")
		}
	}
	delete(env, "DATABASE_URL")
	for _, data := range [][]byte{nil, []byte("a\nb"), []byte("a\r"), {0xff}, []byte("a\x00b"), bytes.Repeat([]byte("x"), 8193)} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readDatabaseURL(getenv); err == nil || strings.Contains(err.Error(), directory) {
			t.Fatal("malformed file accepted or path revealed")
		}
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0644, 0640, 0000} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := readDatabaseURL(getenv); err == nil {
			t.Fatalf("non-private mode %o accepted", mode)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{link, directory, "relative-path", filepath.Join(directory, "missing")} {
		env["DATABASE_URL_FILE"] = bad
		if _, err := readDatabaseURL(getenv); err == nil || strings.Contains(err.Error(), directory) {
			t.Fatal("unsafe path accepted or revealed")
		}
	}
}

func TestDatabaseParseErrorDoesNotRevealCredential(t *testing.T) {
	var logs bytes.Buffer
	_, err := connectWithRetry(context.Background(), "postgres://synthetic-private-value@%zz",
		slog.New(slog.NewTextHandler(&logs, nil)))
	if err == nil || strings.Contains(err.Error(), "synthetic-private-value") || strings.Contains(logs.String(), "synthetic-private-value") {
		t.Fatal("invalid database credential was accepted or revealed")
	}
}
