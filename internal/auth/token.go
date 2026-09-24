// Package auth establishes who is calling. It knows nothing about what they
// may do: that is authz, per call, from the membership row.
//
// An API token and a browser session are the same thing with different
// lifetimes: a bearer secret, stored as a hash beside a public prefix that
// finds the row. There is one verification path for both.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// A token looks like
//
//	ais_k7v2m4qhx3ab_9Jx2...43 characters...
//
// "ais_" makes a leaked token recognisable to secret scanners. The 12
// characters after it are the public prefix, stored in credential.token_prefix
// and safe to show in a list of tokens. The rest is 256 bits of secret.
const (
	tokenScheme = "ais"
	prefixLen   = 12
	secretBytes = 32
)

var prefixEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Token is a freshly made bearer secret. Full is shown to its owner once and
// never stored; Prefix and Hash are what the credential row keeps.
type Token struct {
	Full   string
	Prefix string
	Hash   string
}

func NewToken() (Token, error) {
	var p [8]byte // 8 bytes → 13 base32 characters; 12 are kept
	var s [secretBytes]byte
	if _, err := rand.Read(p[:]); err != nil {
		return Token{}, err
	}
	if _, err := rand.Read(s[:]); err != nil {
		return Token{}, err
	}
	prefix := strings.ToLower(prefixEncoding.EncodeToString(p[:]))[:prefixLen]
	full := tokenScheme + "_" + prefix + "_" + base64.RawURLEncoding.EncodeToString(s[:])
	return Token{Full: full, Prefix: prefix, Hash: hashToken(full)}, nil
}

// hashToken is a plain SHA-256. The secret is 256 random bits, so there is
// nothing for a slow hash to protect: no dictionary contains it. Passwords,
// which people choose, get argon2id instead.
func hashToken(full string) string {
	sum := sha256.Sum256([]byte(full))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// parsePrefix extracts the public prefix from a presented token, or reports
// that the string is not shaped like one of ours.
func parsePrefix(full string) (string, bool) {
	// Three parts and no more: the secret is base64url and may itself
	// contain underscores.
	parts := strings.SplitN(full, "_", 3)
	if len(parts) != 3 || parts[0] != tokenScheme || len(parts[1]) != prefixLen || len(parts[2]) < 40 {
		return "", false
	}
	// The prefix is looked up, so it is held first to the alphabet it is
	// made in. Nothing outside it could find a row, and some of what a
	// header may carry, bytes that are not UTF-8, the database would refuse
	// as a fault of ours: a 500 to retry, before any limit on calls.
	if strings.Trim(parts[1], prefixAlphabet) != "" {
		return "", false
	}
	return parts[1], true
}

// prefixAlphabet is what a prefix is written in: base32, lower-cased.
const prefixAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// tokenMatches compares in constant time.
func tokenMatches(full, storedHash string) bool {
	return subtle.ConstantTimeCompare([]byte(hashToken(full)), []byte(storedHash)) == 1
}
