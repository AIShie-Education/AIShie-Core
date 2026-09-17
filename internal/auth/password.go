package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Passwords are hashed with argon2id and stored as a PHC string, which
// carries its own parameters:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
//
// so the parameters below can be raised later without breaking old hashes.
// These are RFC 9106's second recommended set: 64 MiB, 3 passes, 4 lanes.
type argonParams struct {
	memoryKiB uint32
	passes    uint32
	lanes     uint8
}

var defaultArgon = argonParams{memoryKiB: 64 * 1024, passes: 3, lanes: 4}

const (
	saltLen = 16
	keyLen  = 32

	MinPasswordLen = 10
	MaxPasswordLen = 1024 // argon2 will hash anything; a megabyte-long "password" is an attack
)

var ErrWeakPassword = fmt.Errorf("a password must be %d to %d characters", MinPasswordLen, MaxPasswordLen)

// HashPassword returns the PHC string to store.
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLen || len(password) > MaxPasswordLen {
		return "", ErrWeakPassword
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return encodePHC(defaultArgon, salt, derive(defaultArgon, password, salt, keyLen)), nil
}

func derive(p argonParams, password string, salt []byte, n uint32) []byte {
	return argon2.IDKey([]byte(password), salt, p.passes, p.memoryKiB, p.lanes, n)
}

func encodePHC(p argonParams, salt, key []byte) string {
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.memoryKiB, p.passes, p.lanes, b64(salt), b64(key))
}

// VerifyPassword reports whether password matches the stored PHC string.
func VerifyPassword(password, phc string) (bool, error) {
	if len(password) > MaxPasswordLen {
		return false, nil
	}
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("not an argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.passes, &p.lanes); err != nil {
		return false, errors.New("malformed argon2 parameters")
	}
	// A stored hash is ours, but refuse absurd parameters all the same.
	if p.memoryKiB > 1<<21 || p.passes > 16 || p.lanes == 0 {
		return false, errors.New("argon2 parameters out of range")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("malformed argon2 hash")
	}
	got := derive(p, password, salt, uint32(len(want))) //nolint:gosec // length of a decoded 32-byte key
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// decoyHash is verified against when there is no such account, so that a
// login attempt takes as long whether or not the email exists.
var decoyHash = encodePHC(defaultArgon, make([]byte, saltLen), make([]byte, keyLen))
