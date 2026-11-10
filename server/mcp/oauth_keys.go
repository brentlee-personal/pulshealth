package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
)

var errOAuthKeys = errors.New("OAuth public keys unavailable or invalid")
var oauthKeyID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// The local key set is trusted configuration; a token never selects a URL or file.
func readOAuthPublicKeys(path string) (map[string]ed25519.PublicKey, error) {
	stat, err := os.Stat(path)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > 16384 {
		return nil, errOAuthKeys
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 16384 {
		return nil, errOAuthKeys
	}
	var document struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Alg string `json:"alg"`
			Use string `json:"use"`
		} `json:"keys"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || len(document.Keys) < 1 || len(document.Keys) > 8 {
		return nil, errOAuthKeys
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errOAuthKeys
	}
	keys := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, key := range document.Keys {
		if !oauthKeyID.MatchString(key.Kid) || keys[key.Kid] != nil || key.Kty != "OKP" || key.Crv != "Ed25519" || key.Alg != "EdDSA" || key.Use != "sig" || len(key.X) != 43 {
			return nil, errOAuthKeys
		}
		publicKey, err := b64.DecodeString(key.X)
		if err != nil || len(publicKey) != ed25519.PublicKeySize {
			return nil, errOAuthKeys
		}
		keys[key.Kid] = ed25519.PublicKey(publicKey)
	}
	return keys, nil
}
