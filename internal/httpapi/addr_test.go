package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientAddr(t *testing.T) {
	s := &server{}
	for _, cidr := range []string{"10.0.0.0/8", "::1/128"} {
		_, n, _ := parseCIDR(cidr)
		s.proxies = append(s.proxies, n)
	}
	for _, tc := range []struct {
		remote, forwarded string
		want              string
		known             bool
	}{
		// Reached directly: the peer, and nothing a header says.
		{"203.0.113.5:4242", "", "203.0.113.5", true},
		{"203.0.113.5:4242", "1.2.3.4", "203.0.113.5", true},
		// Through a trusted proxy: the hop it appended.
		{"10.0.0.9:80", "198.51.100.7", "198.51.100.7", true},
		{"10.0.0.9:80", "1.2.3.4, 198.51.100.7", "198.51.100.7", true},
		// Two trusted proxies in a row: the hop before them.
		{"10.0.0.9:80", "198.51.100.7, 10.0.0.2", "198.51.100.7", true},
		// A forged chain of nothing but trusted addresses, or no header at
		// all: the client is unknown, and no limit is keyed on it.
		{"10.0.0.9:80", "10.0.0.3", "", false},
		{"10.0.0.9:80", "", "", false},
		{"10.0.0.9:80", "not-an-address", "", false},
		{"[::1]:80", "2001:db8::7", "2001:db8::7", true},
		// A proxy that appends a line of its own after the client's, as
		// HAProxy does: the header is every line, and its hop is the last.
		{"10.0.0.9:80", "6.6.6.6\n198.51.100.7", "198.51.100.7", true},
		{"10.0.0.9:80", "6.6.6.6, 1.2.3.4\n198.51.100.7, 10.0.0.2", "198.51.100.7", true},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		if tc.forwarded != "" {
			for _, line := range strings.Split(tc.forwarded, "\n") {
				r.Header.Add("X-Forwarded-For", line)
			}
		}
		got, known := s.clientAddr(r)
		if got != tc.want || known != tc.known {
			t.Errorf("from %s via %q: %q, %v; want %q, %v", tc.remote, tc.forwarded, got, known, tc.want, tc.known)
		}
	}
}

// The sign-in limit keys an email by what it names, in a few dozen bytes
// however long it came.
func TestEmailKey(t *testing.T) {
	if emailKey(" Sato@Example.EDU ") != emailKey("sato@example.edu") {
		t.Error("one account, typed two ways, has two keys")
	}
	if emailKey("sato@example.edu") == emailKey("yuki@example.edu") {
		t.Error("two accounts share a key")
	}
	if k := emailKey(strings.Repeat("a", 1<<20) + "@example.edu"); len(k) > 80 {
		t.Errorf("a megabyte of email is a %d-byte key", len(k))
	}
}

// A panic in a handler is answered like any other fault of ours: logged in
// full, a JSON 500 to the caller, the connection kept.
func TestAPanicIsAnsweredNotDropped(t *testing.T) {
	var log strings.Builder
	s := &server{Deps: Deps{Log: slog.New(slog.NewTextHandler(&log, nil))}}
	h := s.recovered(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("assignment to entry in nil map") // the classic
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/tools/x", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "on our side") || strings.Contains(rec.Body.String(), "nil map") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(log.String(), "nil map") || !strings.Contains(log.String(), "addr_test.go") {
		t.Fatalf("the panic and its stack are not in the log: %s", log.String())
	}
}
