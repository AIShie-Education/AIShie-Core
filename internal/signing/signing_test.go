package signing

import (
	"errors"
	"strings"
	"testing"
)

type claim struct {
	Key string `json:"k"`
	N   int    `json:"n"`
}

func TestSignAndOpen(t *testing.T) {
	s, err := New(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	token := s.Sign("upload", claim{"courses/c/1", 7})
	var got claim
	if err := s.Open("upload", token, &got); err != nil || got != (claim{"courses/c/1", 7}) {
		t.Fatalf("%+v %v", got, err)
	}
	// The purpose is part of what is signed: the same claim, the same key,
	// another purpose — not valid.
	for _, purpose := range []string{"url", "", "uploa", "upload2", "upload\x00"} {
		if err := s.Open(purpose, token, &got); !errors.Is(err, ErrBadToken) {
			t.Errorf("opened as %q: %v", purpose, err)
		}
	}
	other, _ := New(strings.Repeat("z", 32))
	if err := other.Open("upload", token, &got); !errors.Is(err, ErrBadToken) {
		t.Fatalf("another key opened it: %v", err)
	}
	body, mac, _ := strings.Cut(token, ".")
	forged, _, _ := strings.Cut(s.Sign("upload", claim{"courses/c/someone-elses", 7}), ".")
	for name, bad := range map[string]string{"another claim under this signature": forged + "." + mac, "no signature": body,
		"empty": "", "garbage": "not.a-token", "trailing junk": token + "A"} {
		if err := s.Open("upload", bad, &got); !errors.Is(err, ErrBadToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := New("too short"); err == nil {
		t.Fatal("a short key was accepted")
	}
	a, _ := New("")
	b, _ := New("")
	if err := b.Open("upload", a.Sign("upload", claim{}), &got); !errors.Is(err, ErrBadToken) {
		t.Fatal("two random keys agree")
	}
}
