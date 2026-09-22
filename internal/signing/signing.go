// Package signing makes and checks small signed tokens: a JSON claim and an
// HMAC-SHA256 over it. Upload tokens, the filesystem store's URLs and the
// single-sign-on state cookie are all this.
//
// Every token is signed for a purpose, and the purpose is part of what is
// signed. A token made for one thing therefore does not verify as another,
// whatever its claim happens to look like: an upload token is not a download
// URL, and neither is a sign-in state. Without that, the only thing keeping
// them apart would be which JSON fields each one happens to have.
package signing

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

var (
	ErrBadToken     = errors.New("signing: the token is not valid")
	ErrExpiredToken = errors.New("signing: the token has expired")
)

// MinKeyLen is the shortest key accepted.
const MinKeyLen = 32

type Signer struct {
	secret []byte
}

// New takes the installation's signing key. With an empty key it makes a
// random one, which works for a single process until it restarts: tokens
// signed before a restart stop verifying. More than one instance, or a
// production deployment, sets the key.
func New(key string) (*Signer, error) {
	if key != "" {
		if len(key) < MinKeyLen {
			return nil, errors.New("signing: the key must be at least 32 characters")
		}
		return &Signer{secret: []byte(key)}, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Signer{secret: b}, nil
}

func (s *Signer) mac(purpose string, body []byte) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(purpose))
	m.Write([]byte{0}) // a purpose cannot contain it, so "ab"+"c" is never "a"+"bc"
	m.Write(body)
	return m.Sum(nil)
}

// Digest is a keyed digest of body for purpose, in hex. Where a plain hash
// of a secret would let anyone holding the database try guesses against it
// at hashing speed, this needs the key as well.
func (s *Signer) Digest(purpose string, body []byte) string {
	return hex.EncodeToString(s.mac(purpose, body))
}

// Sign returns a token carrying claim, good only for purpose.
func (s *Signer) Sign(purpose string, claim any) string {
	body, err := json.Marshal(claim)
	if err != nil {
		panic("signing: claim does not marshal: " + err.Error())
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(body) + "." + enc.EncodeToString(s.mac(purpose, body))
}

// Open checks that token was signed by this key for this purpose, and only
// then decodes its claim. Expiry is the claim's own business.
func (s *Signer) Open(purpose, token string, claim any) error {
	body64, mac64, ok := strings.Cut(token, ".")
	if !ok {
		return ErrBadToken
	}
	enc := base64.RawURLEncoding
	body, err1 := enc.DecodeString(body64)
	got, err2 := enc.DecodeString(mac64)
	if err1 != nil || err2 != nil || !hmac.Equal(got, s.mac(purpose, body)) {
		return ErrBadToken
	}
	if json.Unmarshal(body, claim) != nil {
		return ErrBadToken
	}
	return nil
}
