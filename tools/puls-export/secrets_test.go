package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectedPersonalCredentialFile(t *testing.T) {
	t.Parallel()
	const marker = "synthetic-private-value"
	path := credentialFile(t, marker)
	value, err := readTokenFile(path)
	if err != nil || value != marker {
		t.Fatal("private credential did not load exactly")
	}
	for _, data := range [][]byte{nil, []byte("a\nb"), []byte("a\r"), {0xff}, []byte("a\x00b"), bytes.Repeat([]byte("x"), 8193)} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readTokenFile(path); err == nil || strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), path) {
			t.Fatal("malformed credential accepted or disclosed")
		}
	}
	if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0644, 0640, 0000} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := readTokenFile(path); err == nil {
			t.Fatal("non-private credential accepted")
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "relative", filepath.Dir(path), link, filepath.Join(t.TempDir(), "private-missing")} {
		if _, err := readTokenFile(name); err == nil || strings.Contains(err.Error(), "private-missing") {
			t.Fatal("unsafe credential path accepted or disclosed")
		}
	}
}
