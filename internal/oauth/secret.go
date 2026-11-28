package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

const (
	// ClientSecretPrefix identifies a generated client secret to secret scanners.
	ClientSecretPrefix = "aisoc_"
	// AccessTokenPrefix identifies an opaque OAuth access token. It is deliberately
	// not a prefix the legacy credential Authenticator accepts.
	AccessTokenPrefix = "aiso_"
	randomBytes       = 32
)

// Secret is a freshly generated client secret or access token. Value is
// returned to its caller once; only Hash is stored.
type Secret struct {
	Value string
	Hash  string
}

// NewClientSecret returns a client secret with 256 bits of cryptographic
// randomness. Core keeps only its SHA-256 hash.
func NewClientSecret() (Secret, error) { return randomSecret(ClientSecretPrefix) }

// NewAccessToken returns an opaque access token with 256 bits of
// cryptographic randomness. It is not a JWT and carries no readable claims.
func NewAccessToken() (Secret, error) { return randomSecret(AccessTokenPrefix) }

func randomSecret(prefix string) (Secret, error) {
	var b [randomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Secret{}, err
	}
	value := prefix + base64.RawURLEncoding.EncodeToString(b[:])
	return Secret{Value: value, Hash: hashSecret(value)}, nil
}

// hashSecret is a plain SHA-256 because these values are random, not chosen
// passwords. There is no meaningful dictionary to attack.
func hashSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// MatchesClientSecret compares a presented secret with its stored hash in
// constant time, including when the hash is malformed.
func MatchesClientSecret(presented, storedHash string) bool {
	return subtle.ConstantTimeCompare([]byte(hashSecret(presented)), []byte(storedHash)) == 1
}

// ShapedAccessToken reports whether value has the exact opaque-token shape,
// before it reaches a database query.
func ShapedAccessToken(value string) bool {
	if !strings.HasPrefix(value, AccessTokenPrefix) || len(value) != len(AccessTokenPrefix)+43 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, AccessTokenPrefix))
	return err == nil && len(raw) == randomBytes
}
