package secrets_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Core/internal/secrets"
)

func key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, secrets.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

var adfs = secrets.Binding{Purpose: "sso_provider.client_secret", Owner: "school-adfs"}

func TestASecretOpensWhereItWasSealedAndNowhereElse(t *testing.T) {
	ring, err := secrets.NewKeyring(key(t))
	if err != nil {
		t.Fatal(err)
	}
	const plain = "the-client-secret-of-school-adfs"
	sealed, err := ring.Seal(adfs, plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, plain) || !strings.HasPrefix(sealed, "v1."+ring.KeyID()+".") {
		t.Fatalf("sealed: %q", sealed)
	}
	if id, err := secrets.SealedKeyID(sealed); err != nil || id != ring.KeyID() {
		t.Fatalf("its key id: %q %v", id, err)
	}
	again, err := ring.Seal(adfs, plain)
	if err != nil || again == sealed {
		t.Fatalf("sealed twice the same way: %v", err)
	}
	if got, err := ring.Open(adfs, sealed); err != nil || got != plain {
		t.Fatalf("open: %q %v", got, err)
	}
	for name, b := range map[string]secrets.Binding{
		"another row":     {Purpose: adfs.Purpose, Owner: "google"},
		"another column":  {Purpose: "sso_provider.something_else", Owner: adfs.Owner},
		"split otherwise": {Purpose: adfs.Purpose + "p", Owner: "olyu-adfs"},
	} {
		if _, err := ring.Open(b, sealed); !errors.Is(err, secrets.ErrNotOpened) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A byte changed anywhere, and it does not open.
	raw := []byte(sealed)
	raw[len(raw)-3] ^= 1
	if _, err := ring.Open(adfs, string(raw)); err == nil {
		t.Fatal("a changed byte opened")
	}
	for _, bad := range []string{"", plain, "v2." + ring.KeyID() + ".AAAA", "v1.zz.AAAA", "v1." + ring.KeyID() + ".AA"} {
		if _, err := ring.Open(adfs, bad); err == nil {
			t.Fatalf("%q opened", bad)
		}
	}
	// Another installation's key opens nothing of this one's.
	other, _ := secrets.NewKeyring(key(t))
	if _, err := other.Open(adfs, sealed); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("another key: %v", err)
	}
}

func TestNoKeySealsNothing(t *testing.T) {
	var ring *secrets.Keyring
	if ring.CanSeal() || ring.KeyID() != "" {
		t.Fatal("a nil keyring can seal")
	}
	if _, err := ring.Seal(adfs, "x"); !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("seal: %v", err)
	}
	if _, err := ring.Open(adfs, "v1.0123456789abcdef.AAAA"); !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("open: %v", err)
	}
	if _, err := secrets.NewKeyring(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte key was taken")
	}
}

// Rotation: the new key seals, the old one still opens, and a rewrap moves
// what the old one sealed to the new one, after which the old one can go.
func TestTheKeyIsRotated(t *testing.T) {
	old, next := key(t), key(t)
	before, _ := secrets.NewKeyring(old)
	sealed, err := before.Seal(adfs, "a-secret-sealed-before-the-rotation")
	if err != nil {
		t.Fatal(err)
	}
	during, err := secrets.NewKeyring(next, old)
	if err != nil {
		t.Fatal(err)
	}
	if during.KeyID() == before.KeyID() || during.KeyID() != secrets.KeyID(next) {
		t.Fatal("the new key does not seal")
	}
	if during.Current(sealed) {
		t.Fatal("what the old key sealed is taken for current")
	}
	if got, err := during.Open(adfs, sealed); err != nil || got != "a-secret-sealed-before-the-rotation" {
		t.Fatalf("the old key no longer opens: %v", err)
	}
	moved, err := during.Rewrap(adfs, sealed)
	if err != nil || !during.Current(moved) {
		t.Fatalf("rewrap: %v", err)
	}
	after, _ := secrets.NewKeyring(next)
	if got, err := after.Open(adfs, moved); err != nil || got != "a-secret-sealed-before-the-rotation" {
		t.Fatalf("after the old key went: %v", err)
	}
	if _, err := after.Open(adfs, sealed); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("what was not rewrapped: %v", err)
	}
}

func TestParseKey(t *testing.T) {
	k := key(t)
	for _, form := range []string{
		base64.StdEncoding.EncodeToString(k), base64.RawStdEncoding.EncodeToString(k),
		base64.URLEncoding.EncodeToString(k), " " + base64.RawURLEncoding.EncodeToString(k) + "\n",
	} {
		got, err := secrets.ParseKey(form)
		if err != nil || !bytes.Equal(got, k) {
			t.Fatalf("%q: %v", form, err)
		}
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(k[:16]),
		strings.Repeat("ab", 32)} { // hex is not taken for base64 of 32 bytes
		if _, err := secrets.ParseKey(bad); err == nil {
			t.Fatalf("%q was taken", bad)
		} else if strings.Contains(err.Error(), bad) && bad != "" {
			t.Fatalf("the error repeats the key: %v", err)
		}
	}
}

func TestHint(t *testing.T) {
	for secret, want := range map[string]string{
		"":                                 "…",
		"short":                            "…",
		"nineteen-characters":              "…",
		"twenty-characters-ab":             "…s-ab",
		"s3cret-for-the-token-endpoint":    "…oint",
		"  padded-secret-of-some-length  ": "…ngth",
		"雲雲雲雲雲雲雲雲雲雲雲雲雲雲雲雲雲雲雲雲": "…雲雲雲雲",
	} {
		if got := secrets.Hint(secret); got != want {
			t.Errorf("Hint(%q) = %q, want %q", secret, got, want)
		}
	}
}
