// Package secrets seals the few secrets Core keeps at rest, and must be able
// to read back: an identity provider's client secret, which it sends to the
// provider at each sign-in. A password or a token is hashed, not sealed:
// Core never needs it back.
//
// It is a small envelope, AES-256-GCM under the installation's secrets key
// (SECRETS_KEY, 32 random bytes), written as text:
//
//	v1.<key id>.<base64url of the 12-byte nonce, then the ciphertext and its tag>
//
// Its parts:
//
//   - v1 names the envelope: AES-256-GCM, a random 96-bit nonce, and the
//     additional data below. Another envelope would be v2, and v1 would
//     still open.
//   - The key id is the first 8 bytes of SHA-256 over a label and the key,
//     in hex: it says which key sealed a secret, so that the key can be
//     rotated, and nothing of the key. SECRETS_KEY seals; it and every key
//     in SECRETS_KEY_PREVIOUS open. `aishie-core secrets rewrap` seals
//     again, under SECRETS_KEY, whatever an older key sealed, after which the
//     older key can go.
//   - The additional data is the envelope's label, then what the secret is
//     (its purpose) and whose (the row), each length-prefixed: a sealed value
//     copied to another row, or to another column, does not open there.
//
// Plaintext exists only in the caller's hands: Seal takes it and Open gives
// it back. No error of this package holds a secret, a key or plaintext.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// KeySize is the size of a secrets key: AES-256.
const KeySize = 32

// version is the envelope this package seals with.
const version = "v1"

// label begins the additional data of every v1 envelope.
const label = "aishie/sealed/v1"

var (
	// ErrNoKey is sealing or opening with no secrets key: SECRETS_KEY is not
	// set.
	ErrNoKey = errors.New("secrets: this server has no secrets key (SECRETS_KEY)")
	// ErrUnknownKey is opening what a key this server does not hold sealed:
	// neither SECRETS_KEY nor one of SECRETS_KEY_PREVIOUS.
	ErrUnknownKey = errors.New("secrets: sealed under a key this server does not hold")
	// ErrNotOpened is every failure of AES-GCM to open: the bytes, the key
	// or what the secret is bound to are not those it was sealed with.
	ErrNotOpened = errors.New("secrets: it does not open: its bytes, or what it is bound to, are not those it was sealed with")
	// ErrMalformed is a value that is not a sealed secret at all.
	ErrMalformed = errors.New("secrets: not a sealed secret")
)

// Binding is what a secret is and whose: sealed under one binding, it opens
// under that binding alone.
type Binding struct {
	// Purpose names the column: "sso_provider.client_secret".
	Purpose string
	// Owner names the row: the provider's id.
	Owner string
}

func (b Binding) aad() []byte {
	var out []byte
	for _, f := range []string{label, b.Purpose, b.Owner} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f))) //nolint:gosec // a purpose and an id are short
		out = append(out, f...)
	}
	return out
}

// Keyring holds the secrets key that seals, and the keys that open. A nil
// Keyring holds none: sealing and opening fail with ErrNoKey.
type Keyring struct {
	current string
	keys    map[string][]byte
	rand    io.Reader
}

// NewKeyring is a keyring that seals with current, and opens what current
// or any of previous sealed. Each is KeySize bytes.
func NewKeyring(current []byte, previous ...[]byte) (*Keyring, error) {
	k := &Keyring{keys: map[string][]byte{}, rand: rand.Reader}
	for i, key := range append([][]byte{current}, previous...) {
		if len(key) != KeySize {
			return nil, fmt.Errorf("secrets: a key of %d bytes, not %d", len(key), KeySize)
		}
		id := KeyID(key)
		if i == 0 {
			k.current = id
		}
		k.keys[id] = append([]byte(nil), key...)
	}
	return k, nil
}

// KeyID is the id of a key, as a sealed value names it: 16 hex digits, which
// say which key it is and nothing of it.
func KeyID(key []byte) string {
	h := sha256.New()
	h.Write([]byte("aishie/secrets-key-id/v1"))
	h.Write([]byte{0})
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// KeyID is the id of the key that seals, or "" for a nil keyring.
func (k *Keyring) KeyID() string {
	if k == nil {
		return ""
	}
	return k.current
}

// CanSeal says whether the keyring holds a key to seal with.
func (k *Keyring) CanSeal() bool { return k != nil && k.current != "" }

// ParseKey reads a secrets key from its base64, standard or URL-safe, padded
// or not.
func ParseKey(v string) ([]byte, error) {
	v = strings.TrimRight(strings.TrimSpace(v), "=")
	key, err := base64.RawStdEncoding.DecodeString(v)
	if err != nil {
		key, err = base64.RawURLEncoding.DecodeString(v)
	}
	if err != nil {
		return nil, errors.New("is not base64")
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("is %d bytes, not %d (openssl rand -base64 32 makes one)", len(key), KeySize)
	}
	return key, nil
}

var sealedRE = regexp.MustCompile(`^(v[0-9]+)\.([0-9a-f]{16})\.([A-Za-z0-9_-]+)$`)

// Seal encrypts plaintext, bound to b, under the key that seals.
func (k *Keyring) Seal(b Binding, plaintext string) (string, error) {
	if !k.CanSeal() {
		return "", ErrNoKey
	}
	if plaintext == "" {
		return "", errors.New("secrets: nothing to seal")
	}
	aead, err := gcm(k.keys[k.current])
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(k.rand, nonce); err != nil {
		return "", fmt.Errorf("secrets: a nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), b.aad())
	return version + "." + k.current + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open decrypts what Seal sealed under b.
func (k *Keyring) Open(b Binding, sealed string) (string, error) {
	if k == nil || len(k.keys) == 0 {
		return "", ErrNoKey
	}
	m := sealedRE.FindStringSubmatch(sealed)
	if m == nil || m[1] != version {
		return "", ErrMalformed
	}
	key, ok := k.keys[m[2]]
	if !ok {
		return "", ErrUnknownKey
	}
	raw, err := base64.RawURLEncoding.DecodeString(m[3])
	if err != nil {
		return "", ErrMalformed
	}
	aead, err := gcm(key)
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize()+aead.Overhead() {
		return "", ErrNotOpened
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], b.aad())
	if err != nil {
		return "", ErrNotOpened
	}
	return string(plain), nil
}

// SealedKeyID is the id of the key a sealed value was sealed under.
func SealedKeyID(sealed string) (string, error) {
	m := sealedRE.FindStringSubmatch(sealed)
	if m == nil {
		return "", ErrMalformed
	}
	return m[2], nil
}

// Current says whether sealed was sealed by the key that seals now, in the
// envelope this package seals with: whether a rewrap would leave it as it is.
func (k *Keyring) Current(sealed string) bool {
	m := sealedRE.FindStringSubmatch(sealed)
	return k.CanSeal() && m != nil && m[1] == version && m[2] == k.current
}

// Rewrap opens sealed and seals it again under the key that seals now, bound
// as it was.
func (k *Keyring) Rewrap(b Binding, sealed string) (string, error) {
	plain, err := k.Open(b, sealed)
	if err != nil {
		return "", err
	}
	return k.Seal(b, plain)
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Ellipsis stands for what a hint leaves out.
const Ellipsis = "…"

// A hint shows a secret's last four characters only when it has at least
// minTailed characters: of a shorter one, four would be too much of it.
const minTailed = 20

// Hint is what may be shown of a secret, and never more: an ellipsis and its
// last four characters (…3f9a), or the ellipsis alone for a secret shorter
// than 20 characters.
func Hint(secret string) string {
	secret = strings.TrimSpace(secret)
	if utf8.RuneCountInString(secret) < minTailed {
		return Ellipsis
	}
	r := []rune(secret)
	return Ellipsis + string(r[len(r)-4:])
}
