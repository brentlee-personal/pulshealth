package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func readTokenFile(path string) (string, error) {
	unavailable := usagef("PULS_API_TOKEN_FILE or --token-file must name an available owner-only credential file")
	if path == "" || !filepath.IsAbs(path) {
		return "", unavailable
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Mode().Perm()&0400 == 0 {
		return "", unavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return "", unavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(before, info) || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0400 == 0 || info.Size() < 1 || info.Size() > 8192 {
		return "", unavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || int64(len(data)) != info.Size() || len(data) > 8192 || !utf8.Valid(data) {
		return "", unavailable
	}
	value := strings.TrimSuffix(string(data), "\n")
	if len(value) != len(data) {
		value = strings.TrimSuffix(value, "\r")
	}
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", unavailable
	}
	return value, nil
}
