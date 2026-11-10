package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func readDatabaseURL(getenv func(string) string) (string, error) {
	if getenv("DATABASE_URL") != "" {
		return "", errors.New("use DATABASE_URL_FILE; plaintext DATABASE_URL is not accepted")
	}
	path := getenv("DATABASE_URL_FILE")
	if path == "" {
		return "", errors.New("DATABASE_URL_FILE must be set")
	}
	unavailable := errors.New("DATABASE_URL_FILE is unavailable or is not a private secret file")
	before, err := os.Lstat(path)
	if err != nil || !filepath.IsAbs(path) || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Mode().Perm()&0400 == 0 {
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
