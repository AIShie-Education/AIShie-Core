package blob

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Signer makes and checks the two kinds of signed token this package uses.
// Both are a JSON claim and an HMAC-SHA256 over it, base64url, joined by a
// dot. There is no table of pending uploads: the token is the record.
type Signer struct {
	secret []byte
}

// NewSigner takes the installation's signing key. With an empty key it makes
// a random one, which works for a single process until it restarts: tokens
// signed before a restart stop verifying. Production sets BLOB_SIGNING_KEY.
func NewSigner(key string) (*Signer, error) {
	if key != "" {
		if len(key) < 32 {
			return nil, errors.New("blob: the signing key must be at least 32 characters")
		}
		return &Signer{secret: []byte(key)}, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Signer{secret: b}, nil
}

// UploadClaim says: this member of this course was given this storage key, to
// hold a file for this purpose.
//
// It is checked twice. When the bytes are PUT, it must not have expired: the
// upload window is short. When the upload is attached to a document, expiry is
// not checked: a proposal carrying a feedback file may be approved days later,
// and the file it names is no less the proposer's for that. What stops reuse
// is the database — a storage key is unique across document versions.
type UploadClaim struct {
	Key         string    `json:"k"`
	CourseID    uuid.UUID `json:"c"`
	MemberID    uuid.UUID `json:"m"`
	Purpose     string    `json:"p"`
	ContentType string    `json:"t"`
	Expires     int64     `json:"e"`
}

var (
	ErrBadToken     = errors.New("blob: the token is not valid")
	ErrExpiredToken = errors.New("blob: the token has expired")
)

func (s *Signer) sign(claim any) string {
	body, _ := json.Marshal(claim)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(body)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(body) + "." + enc.EncodeToString(mac.Sum(nil))
}

func (s *Signer) open(token string, claim any) error {
	body64, mac64, ok := strings.Cut(token, ".")
	if !ok {
		return ErrBadToken
	}
	enc := base64.RawURLEncoding
	body, err1 := enc.DecodeString(body64)
	got, err2 := enc.DecodeString(mac64)
	if err1 != nil || err2 != nil {
		return ErrBadToken
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrBadToken
	}
	if json.Unmarshal(body, claim) != nil {
		return ErrBadToken
	}
	return nil
}

// SignUpload issues an upload token.
func (s *Signer) SignUpload(c UploadClaim) string { return s.sign(c) }

// VerifyUpload checks an upload token's signature. It does not check expiry;
// see UploadClaim.
func (s *Signer) VerifyUpload(token string) (UploadClaim, error) {
	var c UploadClaim
	if err := s.open(token, &c); err != nil {
		return UploadClaim{}, err
	}
	if c.Key == "" {
		return UploadClaim{}, ErrBadToken
	}
	return c, nil
}

// urlClaim is what a filesystem-store URL carries: one method, one key, for
// a while.
type urlClaim struct {
	Key         string `json:"k"`
	Method      string `json:"v"`
	ContentType string `json:"t,omitempty"`
	Expires     int64  `json:"e"`
}

func (s *Signer) signURL(key, method, contentType string, ttl time.Duration, now time.Time) string {
	return s.sign(urlClaim{Key: key, Method: method, ContentType: contentType, Expires: now.Add(ttl).Unix()})
}

func (s *Signer) verifyURL(token, method string, now time.Time) (urlClaim, error) {
	var c urlClaim
	if err := s.open(token, &c); err != nil {
		return urlClaim{}, err
	}
	if c.Method != method || c.Key == "" {
		return urlClaim{}, ErrBadToken
	}
	if now.Unix() > c.Expires {
		return urlClaim{}, ErrExpiredToken
	}
	return c, nil
}
