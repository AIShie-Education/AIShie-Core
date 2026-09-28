package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
)

// assertionKeyInfo is the HKDF info under which the assertion key is derived
// from SIGNING_KEY. It keeps the derived key apart from every other use of
// SIGNING_KEY; a new version of it is a new key.
const assertionKeyInfo = "aishiteru/runtime-assertion/v1"

// minSigningKeyLen is the shortest SIGNING_KEY a key is derived from: the
// shortest the server takes at all (signing.MinKeyLen).
const minSigningKeyLen = 32

// ErrNoAssertionKey means neither ASSERTION_KEY nor SIGNING_KEY is set, so
// this server has no key to make assertions with.
var ErrNoAssertionKey = errors.New("no assertion key: neither ASSERTION_KEY nor SIGNING_KEY is set")

// AssertionKey is the key assertions are signed with: the Ed25519 key of
// seed (ASSERTION_KEY) when there is one, and otherwise one derived from the
// installation's signing key, HKDF-SHA256 over it with no salt and the info
// "aishiteru/runtime-assertion/v1", so that a server that has SIGNING_KEY
// needs no new secret, and its key is the same after a restart and on every
// instance.
func AssertionKey(seed []byte, signingKey string) (ed25519.PrivateKey, error) {
	switch {
	case len(seed) > 0:
		if len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("ASSERTION_KEY: %d bytes, want an Ed25519 seed of %d", len(seed), ed25519.SeedSize)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	case signingKey != "":
		if len(signingKey) < minSigningKeyLen {
			return nil, fmt.Errorf("SIGNING_KEY: at least %d characters are needed to derive the assertion key from", minSigningKeyLen)
		}
		derived, err := hkdf.Key(sha256.New, []byte(signingKey), nil, assertionKeyInfo, ed25519.SeedSize)
		if err != nil {
			return nil, fmt.Errorf("derive the assertion key: %w", err)
		}
		return ed25519.NewKeyFromSeed(derived), nil
	}
	return nil, ErrNoAssertionKey
}

// AssertionConfig is what an Asserter is made of.
type AssertionConfig struct {
	// Issuer names this server in every assertion: PUBLIC_URL, with no "/"
	// at the end.
	Issuer string
	// Audiences are the services an assertion may be made for. With none,
	// the key is still published, and no assertion is made.
	Audiences []string
	// TTL is how long an assertion lasts at most; the credential it was
	// asked for with bounds it as well.
	TTL time.Duration
	// Key signs them (AssertionKey).
	Key ed25519.PrivateKey
}

// Asserter makes assertions and publishes the key that checks them.
//
// An assertion is this server vouching, to a service that hosts agents (the
// runtime), for the person signed in here: a short-lived JWT, signed with
// Ed25519, whose audience is that service. The runtime checks it against the
// key this server publishes (KeySet), so it needs no sign-in of its own and
// never sees a credential of Core's. Core itself never takes one:
// Authenticate knows only its own "ais_" tokens.
type Asserter struct {
	pool      *pgxpool.Pool
	key       ed25519.PrivateKey
	kid       string
	issuer    string
	audiences []string
	ttl       time.Duration
	now       func() time.Time
}

// NewAsserter checks cfg and makes an Asserter that reads actors from pool.
func NewAsserter(pool *pgxpool.Pool, cfg AssertionConfig) (*Asserter, error) {
	switch {
	case len(cfg.Key) != ed25519.PrivateKeySize:
		return nil, errors.New("assertions: the key is not an Ed25519 private key")
	case cfg.Issuer == "":
		return nil, errors.New("assertions: no issuer")
	case cfg.TTL <= 0:
		return nil, errors.New("assertions: the lifetime must be positive")
	}
	for _, aud := range cfg.Audiences {
		if aud == "" {
			return nil, errors.New("assertions: an audience is empty")
		}
	}
	pub := cfg.Key.Public().(ed25519.PublicKey)
	return &Asserter{
		pool: pool, key: cfg.Key, kid: thumbprint(pub), issuer: cfg.Issuer,
		audiences: slices.Clone(cfg.Audiences), ttl: cfg.TTL, now: time.Now,
	}, nil
}

// SetClock replaces the clock assertions are dated by, for tests.
func (a *Asserter) SetClock(now func() time.Time) { a.now = now }

// Issues reports whether any audience is configured, that is whether this
// server makes assertions at all.
func (a *Asserter) Issues() bool { return len(a.audiences) > 0 }

// KeyID is the id of the key assertions are signed with, as their header
// and the key set name it: its RFC 7638 thumbprint, which is the same for
// the same key wherever and whenever it is worked out.
func (a *Asserter) KeyID() string { return a.kid }

// KeySet is the JSON Web Key Set (RFC 7517) that checks assertions: the
// public half of the one key, and nothing else.
type KeySet struct {
	Keys []PublicKey `json:"keys"`
}

// PublicKey is an Ed25519 public key as a JWK (RFC 8037).
type PublicKey struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// KeySet is the key set that checks this Asserter's assertions.
func (a *Asserter) KeySet() KeySet {
	pub := a.key.Public().(ed25519.PublicKey)
	return KeySet{Keys: []PublicKey{{Kty: "OKP", Crv: "Ed25519", X: b64(pub), Kid: a.kid, Alg: "EdDSA", Use: "sig"}}}
}

// thumbprint is the RFC 7638 thumbprint of an Ed25519 public key: SHA-256
// over its required members, in lexical order, with no white space.
func thumbprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + b64(pub) + `"}`))
	return b64(sum[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Assertion is a signed assertion and when it stops being good.
type Assertion struct {
	Token     string
	ExpiresAt time.Time
}

// assertionClaims are what an assertion says. Times are whole seconds since
// the epoch, as a JWT's are.
type assertionClaims struct {
	Issuer       string  `json:"iss"`
	Audience     string  `json:"aud"`
	Subject      string  `json:"sub"`
	IssuedAt     int64   `json:"iat"`
	NotBefore    int64   `json:"nbf"`
	Expires      int64   `json:"exp"`
	ID           string  `json:"jti"`
	Kind         string  `json:"kind"`
	Name         string  `json:"name"`
	Email        *string `json:"email,omitempty"`
	PlatformRole *string `json:"platform_role,omitempty"`
	// SessionID is the credential the person asked with, so that what the
	// runtime records can be traced back to a sign-in.
	SessionID string `json:"sid"`
}

// jwsHeader is the protected header of every assertion.
type jwsHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

var (
	errNotAnAudience = apperr.Invalid("the audience is not one this server makes assertions for")
	errNotActive     = apperr.Forbid("a suspended account is given no assertion")
	errNotAPerson    = apperr.Forbid("only a person is given an assertion; an agent's credential cannot ask for one")
)

// Assert makes an assertion, for audience, of the person p authenticated.
//
// Authenticate does not look at the actor's standing, so the actor is read
// again here: a suspended one is refused, and so is anyone who is not a
// person — an agent's token vouches for no one a runtime should let in. The
// assertion lasts the configured time, and never longer than the credential
// it was asked with, so that an assertion made in the last minutes of a
// session ends with it.
func (a *Asserter) Assert(ctx context.Context, p Principal, audience string) (Assertion, error) {
	if !slices.Contains(a.audiences, audience) {
		return Assertion{}, errNotAnAudience
	}
	actor, err := dbq.New(a.pool).GetActor(ctx, p.ActorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Assertion{}, errUnauthenticated
	}
	if err != nil {
		return Assertion{}, fmt.Errorf("assertion: actor lookup: %w", err)
	}
	switch {
	case actor.Status != domain.ActorActive:
		return Assertion{}, errNotActive
	case actor.Kind != "human":
		return Assertion{}, errNotAPerson
	}

	now := a.now()
	exp := now.Add(a.ttl)
	if p.ExpiresAt != nil && p.ExpiresAt.Before(exp) {
		exp = *p.ExpiresAt
	}
	// Whole seconds, rounded down: never later than the credential.
	iat, expires := now.Unix(), exp.Unix()
	if expires <= iat {
		// The credential runs out within the second: it is as good as gone.
		return Assertion{}, errUnauthenticated
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return Assertion{}, fmt.Errorf("assertion: jti: %w", err)
	}
	token, err := a.sign(assertionClaims{
		Issuer: a.issuer, Audience: audience, Subject: actor.ID.String(),
		IssuedAt: iat, NotBefore: iat, Expires: expires, ID: b64(jti),
		Kind: actor.Kind, Name: actor.DisplayName, Email: actor.Email, PlatformRole: actor.PlatformRole,
		SessionID: p.CredentialID.String(),
	})
	if err != nil {
		return Assertion{}, err
	}
	return Assertion{Token: token, ExpiresAt: time.Unix(expires, 0).UTC()}, nil
}

// sign makes a JWS in compact serialisation (RFC 7515 §7.1): the header and
// the claims, each JSON in base64url with no padding, joined by a dot, and
// the Ed25519 signature over exactly those bytes after another.
func (a *Asserter) sign(claims assertionClaims) (string, error) {
	header, err := json.Marshal(jwsHeader{Alg: "EdDSA", Typ: "JWT", Kid: a.kid})
	if err != nil {
		return "", fmt.Errorf("assertion header: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("assertion claims: %w", err)
	}
	input := b64(header) + "." + b64(payload)
	return input + "." + b64(ed25519.Sign(a.key, []byte(input))), nil
}
