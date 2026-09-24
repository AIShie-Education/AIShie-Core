package canon

import (
	"regexp"
	"strings"
	"testing"
)

func mustCanon(t *testing.T, raw string, strip ...string) string {
	t.Helper()
	b, err := Canonicalize([]byte(raw), strip...)
	if err != nil {
		t.Fatalf("Canonicalize(%s): %v", raw, err)
	}
	return string(b)
}

func TestSameCallSameBytes(t *testing.T) {
	// Each group is one call written several ways.
	groups := [][]string{
		{
			`{"score": 85, "student": "yuki", "breakdown": [{"points": 8, "max": 10}]}`,
			`{"student":"yuki","breakdown":[{"max":10,"points":8}],"score":85}`,
			"{\n  \"breakdown\": [ { \"max\": 10.0, \"points\": 8e0 } ],\n  \"score\": 8.5e1,\n  \"student\": \"yuki\"\n}",
		},
		{`{"name": "école"}`, `{"name": "école"}`},
		{`{}`, ``, `   `},
		{`{"n": 0}`, `{"n": -0}`, `{"n": 0.000}`, `{"n": 0e5}`, `{"n": -0.0e-3}`},
	}
	for _, g := range groups {
		want := mustCanon(t, g[0])
		for _, raw := range g[1:] {
			if got := mustCanon(t, raw); got != want {
				t.Errorf("not canonical:\n  %s -> %s\n  %s -> %s", g[0], want, raw, got)
			}
		}
	}
}

func TestDifferentCallDifferentBytes(t *testing.T) {
	pairs := [][2]string{
		{`{"score": 85}`, `{"score": 60}`},
		{`{"score": 85}`, `{"score": "85"}`},
		{`{"note": null}`, `{}`},
		{`{"ids": [1, 2]}`, `{"ids": [2, 1]}`},
		{`{"a": {"b": 1}}`, `{"a": {"b": 1, "c": null}}`},
		{`{"s": "<b>"}`, `{"s": "b"}`},
	}
	for _, p := range pairs {
		if a, b := mustCanon(t, p[0]), mustCanon(t, p[1]); a == b {
			t.Errorf("%s and %s both canonicalize to %s", p[0], p[1], a)
		}
	}
}

func TestExactOutput(t *testing.T) {
	got := mustCanon(t, `{"b": [true, false, null], "a": {"z": 1.50, "y": "x<y"}, "c": -2.5E-1}`)
	want := `{"a":{"y":"x<y","z":1.5},"b":[true,false,null],"c":-0.25}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestNumbers(t *testing.T) {
	cases := map[string]string{
		"1": "1", "1.0": "1", "1e0": "1", "10e-1": "1", "0.10": "0.1", "1E2": "100",
		"1.5e1": "15", "1.5e-1": "0.15", "-7": "-7", "-0.50": "-0.5", "100": "100",
		"0.001": "0.001", "1e-3": "0.001", "123.456e2": "12345.6", "120e-1": "12",
		"1e399": "1" + strings.Repeat("0", 399), "-1e-399": "-0." + strings.Repeat("0", 398) + "1",
	}
	for in, want := range cases {
		got, err := normalizeNumber(in)
		if err != nil || got != want {
			t.Errorf("normalizeNumber(%q) = %q, %v; want %q", in, got, err, want)
		}
		// What is accepted once is accepted again, unchanged.
		if again, err := normalizeNumber(got); err != nil || again != got {
			t.Errorf("normalizeNumber(%q) = %q, %v; want it unchanged", got, again, err)
		}
	}
	// 1e400 and 1e-400 are within the bounds as written, but not written out.
	for _, in := range []string{"1e400", "1e-400", "1e401", "1e-401", "1e999999999", strings.Repeat("9", 401)} {
		if got, err := normalizeNumber(in); err == nil {
			t.Errorf("normalizeNumber(%q) = %q; want an error", in, got)
		}
	}
}

// CheckNumbers finds a number wherever it is, and refuses a long one without
// parsing it into anything.
func TestCheckNumbers(t *testing.T) {
	long := "0." + strings.Repeat("7", 1<<20)
	for raw, ok := range map[string]bool{
		`{"a": [1, {"b": 2.5}], "c": "1e999999999"}`: true, // a string is not a number
		`{"a": [1, {"b": 1e999999999}]}`:             false,
		`{"score": ` + long + `}`:                    false,
		`{`:                                          true, // left for the parse that follows
	} {
		if err := CheckNumbers([]byte(raw)); (err == nil) != ok {
			t.Errorf("CheckNumbers(%.40s…) = %v, want ok=%v", raw, err, ok)
		}
	}
}

func TestStrip(t *testing.T) {
	with := mustCanon(t, `{"score": 85, "idempotency_key": "k-1", "password": "hunter2"}`, "idempotency_key", "password")
	without := mustCanon(t, `{"score": 85}`)
	if with != without {
		t.Fatalf("strip left something behind: %s", with)
	}
	// Only the top level is stripped; a nested field of the same name is data.
	nested := mustCanon(t, `{"a": {"password": "x"}}`, "password")
	if nested != `{"a":{"password":"x"}}` {
		t.Fatalf("nested field was stripped: %s", nested)
	}
}

func TestRejects(t *testing.T) {
	for _, raw := range []string{`[1,2]`, `"x"`, `42`, `null`, `{"a":1} {"b":2}`, `{"a":`, `{"n": 1e999}`} {
		if got, err := Canonicalize([]byte(raw)); err == nil {
			t.Errorf("Canonicalize(%s) = %s; want an error", raw, got)
		}
	}
}

func TestHash(t *testing.T) {
	c := []byte(`{"score":85}`)
	h := Hash("grade.submit", c)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) {
		t.Fatalf("hash %q does not match the column's CHECK", h)
	}
	if h != Hash("grade.submit", c) {
		t.Fatal("hash is not deterministic")
	}
	if h == Hash("grade.post", c) {
		t.Fatal("the tool name is not part of the hash")
	}
	// Pinned against `printf 'grade.submit\n{"score":85}' | shasum -a 256`.
	// Stored hashes must keep matching, so a change to the rule has to be
	// deliberate and come with a new Rule name; this is what notices.
	const pinned = "2a272ec1f9187ca9f2a722eed07d75cf099adb27c0b9d7a14844c38ed08b1d4b"
	if h != pinned {
		t.Fatalf("%s changed: hash = %s, pinned %s", Rule, h, pinned)
	}
	if got := Hash("grade.submit", []byte(mustCanon(t, `{ "score": 8.5e1 }`))); got != pinned {
		t.Fatalf("canonicalize + hash = %s, pinned %s", got, pinned)
	}
}
