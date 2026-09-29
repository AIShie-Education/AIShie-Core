package memory_test

import (
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/memory"
)

// What memory keeps of a text, and what it refuses: empty, too long, a
// control character, a secret. A refusal never repeats the text.
func TestCheckText(t *testing.T) {
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	invite, err := auth.NewInvite()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, in string
		want     string // kept, when accepted
		reason   string // details.reason, when refused; "-" for a refusal with none
		kind     string // details.kind, for a secret
	}{
		{name: "a sentence, as it is", in: "Prefers worked examples in Python.", want: "Prefers worked examples in Python."},
		{name: "trimmed, its line breaks made LF", in: "  one\r\ntwo\rthree\n\t ", want: "one\ntwo\nthree"},
		{name: "a tab inside kept", in: "a\tb", want: "a\tb"},
		{name: "empty", in: "", reason: "empty"},
		{name: "white space alone", in: " \r\n\t ", reason: "empty"},
		{name: "a control character", in: "ring the bell\x07", reason: "-"},
		{name: "an escape sequence", in: "\x1b[31mred", reason: "-"},
		{name: "a C1 control character", in: "next\u0085line", reason: "-"},
		{name: "1000 characters", in: strings.Repeat("x", 1000), want: strings.Repeat("x", 1000)},
		{name: "1001 characters", in: strings.Repeat("x", 1001), reason: "too_long"},
		{name: "counted once trimmed", in: "\n " + strings.Repeat("x", 1000) + " \r\n", want: strings.Repeat("x", 1000)},
		{name: "1000 Chinese characters, 3000 bytes", in: strings.Repeat("記", 1000), want: strings.Repeat("記", 1000)},
		{name: "1001 Chinese characters", in: strings.Repeat("記", 1001), reason: "too_long"},
		{name: "1000 Japanese characters and an emoji", in: strings.Repeat("す", 999) + "😀", want: strings.Repeat("す", 999) + "😀"},
		{name: "1000 characters of four bytes, 4000 bytes", in: strings.Repeat("😀", 1000), want: strings.Repeat("😀", 1000)},
		{name: "1001 characters of four bytes", in: strings.Repeat("😀", 1001), reason: "too_long"},
		{name: "a Core token", in: "my token is " + token.Full + ", keep it", reason: "holds_secret", kind: "core_token"},
		{name: "a Core invitation", in: invite.Full, reason: "holds_secret", kind: "core_token"},
		{name: "an Anthropic key", in: "key: sk-ant-api03-" + strings.Repeat("Ab3_", 10), reason: "holds_secret", kind: "provider_key"},
		{name: "an OpenAI project key", in: "sk-proj-" + strings.Repeat("x9Y", 10), reason: "holds_secret", kind: "provider_key"},
		{name: "a plain sk- key", in: "use sk-" + strings.Repeat("Q", 24), reason: "holds_secret", kind: "provider_key"},
		{name: "a Google key", in: "AIza" + strings.Repeat("b", 35), reason: "holds_secret", kind: "provider_key"},
		{name: "a GitHub token", in: "ghp_" + strings.Repeat("a1", 18), reason: "holds_secret", kind: "provider_key"},
		{name: "a fine-grained GitHub token", in: "github_pat_" + strings.Repeat("A_1", 10), reason: "holds_secret", kind: "provider_key"},
		{name: "a Slack token", in: "xoxb-1234567890-abcdef", reason: "holds_secret", kind: "provider_key"},
		{name: "an AWS access key", in: "AKIAIOSFODNN7EXAMPLE is the id", reason: "holds_secret", kind: "cloud_key"},
		{name: "an AWS session key", in: "ASIA" + strings.Repeat("Z", 16), reason: "holds_secret", kind: "cloud_key"},
		{name: "a JSON Web Token", in: "Bearer eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiJ5dWtpIn0.c2lnbmF0dXJlLWJ5dGVz", reason: "holds_secret", kind: "jwt"},
		{name: "a private key", in: "-----BEGIN RSA PRIVATE KEY-----\nMIIEow", reason: "holds_secret", kind: "private_key"},
		{name: "an OpenSSH key", in: "-----BEGIN OPENSSH PRIVATE KEY-----", reason: "holds_secret", kind: "private_key"},
		{name: "words that only look a little like keys", in: "Ask about sk-learn, AKIA exams and eyJ.", want: "Ask about sk-learn, AKIA exams and eyJ."},
		{name: "a public key", in: "-----BEGIN PUBLIC KEY-----", want: "-----BEGIN PUBLIC KEY-----"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, e := memory.CheckText(tc.in)
			if tc.reason == "" {
				if e != nil || got != tc.want {
					t.Fatalf("got %q, %v; want %q", got, e, tc.want)
				}
				return
			}
			if e == nil {
				t.Fatalf("kept %q; want it refused (%s)", got, tc.reason)
			}
			if e.Code != apperr.InvalidArgument || e.Details["field"] != nil && e.Details["field"] != "text" {
				t.Fatalf("refused as %+v", e)
			}
			if r, _ := e.Details["reason"].(string); (tc.reason == "-" && r != "") || (tc.reason != "-" && r != tc.reason) {
				t.Fatalf("reason %q, want %q", r, tc.reason)
			}
			if k, _ := e.Details["kind"].(string); k != tc.kind {
				t.Fatalf("kind %q, want %q", k, tc.kind)
			}
			if trimmed := strings.TrimSpace(tc.in); len(trimmed) > 6 && strings.Contains(e.Message, trimmed[len(trimmed)-6:]) {
				t.Fatalf("the refusal repeats the text: %s", e.Message)
			}
		})
	}
}

func TestCheckTags(t *testing.T) {
	if got, e := memory.CheckTags(nil); e != nil || got == nil || len(got) != 0 {
		t.Fatalf("no tags: %#v, %v; want an empty list", got, e)
	}
	if got, e := memory.CheckTags([]string{"goal", "fact", "goal", "week-3", "cs_101"}); e != nil ||
		strings.Join(got, ",") != "goal,fact,week-3,cs_101" {
		t.Fatalf("got %v, %v", got, e)
	}
	for name, tags := range map[string][]string{
		"six":                {"a", "b", "c", "d", "e", "f"},
		"upper case":         {"Goal"},
		"a space":            {"to do"},
		"empty":              {""},
		"33 characters":      {strings.Repeat("t", 33)},
		"a leading hyphen":   {"-x"},
		"a word not English": {"目標"},
	} {
		if _, e := memory.CheckTags(tags); e == nil || e.Details["reason"] != "bad_tags" || e.Details["field"] != "tags" {
			t.Errorf("%s: %v", name, e)
		}
	}
}

func TestLimitsDefault(t *testing.T) {
	c := memory.Config{MaxAsker: 3}.WithDefaults()
	if c.Enabled || c.MaxAsker != 3 || c.MaxOwner != memory.DefaultMaxOwner || c.MaxShared != memory.DefaultMaxShared ||
		c.MaxProposed != memory.DefaultMaxProposed || c.MaxPerAgent != memory.DefaultMaxPerAgent ||
		c.WritesPerHour != memory.DefaultWritesPerHour || c.WritesPerDay != memory.DefaultWritesPerDay {
		t.Fatalf("%+v", c)
	}
	if c.MaxActive(memory.ScopeOwner) != c.MaxOwner || c.MaxActive(memory.ScopeAsker) != 3 || c.MaxActive(memory.ScopeCourse) != c.MaxShared {
		t.Fatal("MaxActive names the wrong limit")
	}
}
