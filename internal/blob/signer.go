package blob

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/signing"
)

// Signer makes and checks the two kinds of signed token this package uses:
// upload tokens, and the filesystem store's URLs. There is no table of
// pending uploads: the token is the record. Each kind is signed for its own
// purpose, so neither can ever be taken for the other.
type Signer struct {
	s *signing.Signer
}

const (
	purposeUpload    = "blob.upload"
	purposeURL       = "blob.url"
	purposeRendition = "blob.rendition"
)

var (
	ErrBadToken     = signing.ErrBadToken
	ErrExpiredToken = signing.ErrExpiredToken
)

// NewSigner takes the installation's signing key; see signing.New.
func NewSigner(key string) (*Signer, error) {
	s, err := signing.New(key)
	if err != nil {
		return nil, errors.New("blob: the signing key must be at least 32 characters")
	}
	return &Signer{s: s}, nil
}

// SignerFrom signs with the installation's one signer. Purposes keep this
// package's tokens apart from whatever else it signs.
func SignerFrom(s *signing.Signer) *Signer { return &Signer{s: s} }

// UploadClaim says: this member of this course was given this storage key, to
// hold a file for this purpose.
//
// It is checked twice. When the bytes are PUT, it must not have expired: the
// upload window is short. When the upload is attached to a document or a
// message, expiry is not checked: a proposal carrying a feedback file may be
// approved days later, and the file it names is no less the proposer's for
// that. What stops reuse is the database: an upload is not attached while the
// key it was uploaded under, or the key it would be attached under, is any
// version's or message's file (claimUpload, in package tools). What stops an
// upload lying about for ever is the orphan sweep.
type UploadClaim struct {
	Key         string    `json:"k"`
	CourseID    uuid.UUID `json:"c"`
	MemberID    uuid.UUID `json:"m"`
	Purpose     string    `json:"p"`
	ContentType string    `json:"t"`
	Expires     int64     `json:"e"`
	// Filename is the name the uploader gave the file when asking where to
	// upload it, if any: what it is called when it is attached without one.
	Filename string `json:"f,omitempty"`
}

// SignUpload issues an upload token.
func (s *Signer) SignUpload(c UploadClaim) string { return s.s.Sign(purposeUpload, c) }

// VerifyUpload checks an upload token's signature. It does not check expiry;
// see UploadClaim.
func (s *Signer) VerifyUpload(token string) (UploadClaim, error) {
	var c UploadClaim
	if err := s.s.Open(purposeUpload, token, &c); err != nil {
		return UploadClaim{}, err
	}
	if c.Key == "" {
		return UploadClaim{}, ErrBadToken
	}
	return c, nil
}

// RenditionUpload says: this key was given to this claim (Lease) of this
// rendition, for the agent runtime to upload the PDF it made to. It is
// signed for its purpose alone, so that it is never taken for a member's
// upload token, nor one of those for it. Like an upload token, it is
// checked without its expiry when the PDF is recorded: the claim is what
// must still hold then.
type RenditionUpload struct {
	Key       string    `json:"k"`
	Rendition uuid.UUID `json:"r"`
	Lease     uuid.UUID `json:"l"`
	Expires   int64     `json:"e"`
}

// SignRendition issues a rendition's upload token.
func (s *Signer) SignRendition(c RenditionUpload) string { return s.s.Sign(purposeRendition, c) }

// VerifyRendition checks a rendition's upload token's signature, and not its
// expiry; see RenditionUpload.
func (s *Signer) VerifyRendition(token string) (RenditionUpload, error) {
	var c RenditionUpload
	if err := s.s.Open(purposeRendition, token, &c); err != nil {
		return RenditionUpload{}, err
	}
	if c.Key == "" || c.Rendition == uuid.Nil || c.Lease == uuid.Nil {
		return RenditionUpload{}, ErrBadToken
	}
	return c, nil
}

// urlClaim is what a filesystem-store URL carries: one method, one key, for
// a while; for a download, the name it is saved under, if it has one; for a
// view, that it is one and the type it is served as; for an upload larger
// than an upload is otherwise, how large.
type urlClaim struct {
	Key         string `json:"k"`
	Method      string `json:"v"`
	ContentType string `json:"t,omitempty"`
	Filename    string `json:"f,omitempty"`
	MaxBytes    int64  `json:"x,omitempty"`
	Inline      bool   `json:"i,omitempty"`
	Expires     int64  `json:"e"`
}

func (s *Signer) signURL(key, method, contentType string, ttl time.Duration, now time.Time) string {
	return s.s.Sign(purposeURL, urlClaim{Key: key, Method: method, ContentType: contentType, Expires: now.Add(ttl).Unix()})
}

func (s *Signer) signPutUpTo(key, contentType string, maxBytes int64, ttl time.Duration, now time.Time) string {
	return s.s.Sign(purposeURL, urlClaim{Key: key, Method: "PUT", ContentType: contentType, MaxBytes: maxBytes,
		Expires: now.Add(ttl).Unix()})
}

func (s *Signer) signDownload(key, filename string, ttl time.Duration, now time.Time) string {
	return s.s.Sign(purposeURL, urlClaim{Key: key, Method: "GET", Filename: filename, Expires: now.Add(ttl).Unix()})
}

func (s *Signer) signView(key, filename, contentType string, ttl time.Duration, now time.Time) string {
	return s.s.Sign(purposeURL, urlClaim{Key: key, Method: "GET", Filename: filename, ContentType: contentType, Inline: true,
		Expires: now.Add(ttl).Unix()})
}

func (s *Signer) verifyURL(token, method string, now time.Time) (urlClaim, error) {
	var c urlClaim
	if err := s.s.Open(purposeURL, token, &c); err != nil {
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
