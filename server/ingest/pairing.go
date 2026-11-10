package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The pairing payload — what the app's QR scanner reads:
//
//	puls://pair?url=<percent-encoded>&token=<percent-encoded>&user=<uuid>
//
// `ingest devices issue` builds it with the member-bound device token, shown
// only once. PairingPayload.swift in PulsHealthSync parses it.

// pairingEscape percent-encodes everything but the RFC 3986 unreserved
// characters, byte by byte with upper-case hex — the same output as
// bootstrap.sh's urlencode. Not url.QueryEscape: that writes a space as "+",
// which the app's URLComponents-based parser hands back as a literal plus.
func pairingEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

func pairingPayload(serverURL, token, userID string) string {
	return "puls://pair?url=" + pairingEscape(serverURL) +
		"&token=" + pairingEscape(token) +
		"&user=" + pairingEscape(userID)
}

// normalizePairingURL applies the app's own rules (ServerURLValidation.swift)
// to the URL a pairing code will carry, so a code the scanner would refuse is
// refused here instead — before a token is minted for it: http or https, a
// host, and plain http only for a host on the local network, which is all the
// app's ATS exception allows. Trailing slashes are dropped, as the app does.
func normalizePairingURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("empty URL")
	}
	if !strings.Contains(trimmed, "://") {
		return "", fmt.Errorf("%q has no scheme; start it with https:// (or http:// for a local-network host)", trimmed)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid URL", trimmed)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %s://; use https:// (or http:// for a local-network host)", scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("%q has no host name", trimmed)
	}
	if scheme == "http" && !isLocalNetworkHost(u.Hostname()) {
		return "", fmt.Errorf("the app refuses plain http:// outside the local network (localhost, *.local, 10.x, 172.16-31.x, 192.168.x, 169.254.x); use https:// for %s", u.Hostname())
	}
	return strings.TrimRight(trimmed, "/"), nil
}

// isLocalNetworkHost mirrors ServerURLValidation.isLocalNetworkHost: localhost,
// *.local, loopback, RFC 1918, link-local, and the IPv6 counterparts (::1,
// fe80::/10, fc00::/7). host comes from url.Hostname(), so an IPv6 literal
// has already lost its brackets.
func isLocalNetworkHost(host string) bool {
	name := strings.TrimRight(strings.ToLower(host), ".")
	if name == "localhost" || strings.HasSuffix(name, ".localhost") || strings.HasSuffix(name, ".local") {
		return true
	}
	if parts := strings.Split(name, "."); len(parts) == 4 {
		var octets [4]int
		numeric := true
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 || n > 255 || strings.TrimLeft(part, "0123456789") != "" {
				numeric = false
				break
			}
			octets[i] = n
		}
		if numeric {
			switch {
			case octets[0] == 10, octets[0] == 127,
				octets[0] == 192 && octets[1] == 168,
				octets[0] == 169 && octets[1] == 254,
				octets[0] == 172 && octets[1] >= 16 && octets[1] <= 31:
				return true
			}
			return false
		}
	}
	if strings.Contains(name, ":") {
		// IPv6: drop a zone index (fe80::1%en0) before classifying.
		address, _, _ := strings.Cut(name, "%")
		if address == "::1" {
			return true
		}
		for _, prefix := range []string{"fe8", "fe9", "fea", "feb", "fc", "fd"} {
			if strings.HasPrefix(address, prefix) {
				return true
			}
		}
	}
	return false
}
