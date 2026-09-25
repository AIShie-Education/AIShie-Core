package testkit

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Forged returns a signed token, or a URL that ends in one, with one bit of
// its signature flipped: the claim reads as it did, and no key signed it.
//
// Writing over the signature's last characters looks like the same thing and
// is not. They carry only its last few bits, and whenever those are what goes
// over them the "forged" token is the genuine one. "AA" in place of the last
// two did that about one run in 1024, and a test expecting a refusal failed.
func Forged(t testing.TB, token string) string {
	t.Helper()
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		t.Fatalf("not a signed token: %q", token)
	}
	mac, err := base64.RawURLEncoding.DecodeString(token[i+1:])
	if err != nil || len(mac) == 0 {
		t.Fatalf("not a signed token: %q", token)
	}
	mac[0] ^= 1
	return token[:i+1] + base64.RawURLEncoding.EncodeToString(mac)
}
