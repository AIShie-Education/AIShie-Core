package memory

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
)

// CheckText holds an entry's text to what memory keeps, and returns it as it
// is kept: line breaks made LF, and trimmed. Every tool that writes memory
// text goes through it: an agent's, an owner's, course staff's.
//
// It refuses text that is empty, that holds a control character other than
// a line break or a tab, that is longer than MaxChars characters or MaxBytes
// bytes, or that holds something shaped like a secret (secretKinds). A
// refusal never repeats the text: the tools declare it SecretIn so that the
// action log holds none of it, and the refusal is stored there too.
// Passwords, health and the like cannot be recognised: the tools'
// descriptions forbid them, and people can delete what slips through.
func CheckText(text string) (string, *apperr.Error) {
	t := strings.ReplaceAll(text, "\r\n", "\n")
	t = strings.TrimSpace(strings.ReplaceAll(t, "\r", "\n"))
	if t == "" {
		return "", apperr.Invalid("the text is empty: write something to remember").With("reason", "empty").With("field", "text")
	}
	for _, r := range t {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", apperr.Invalid("the text holds a control character (%U); only line breaks and tabs are kept", r).With("field", "text")
		}
	}
	if n, b := utf8.RuneCountInString(t), len(t); n > MaxChars || b > MaxBytes {
		return "", apperr.Invalid("the text is %d characters (%d bytes) long; the most is %d characters and %d bytes", n, b, MaxChars, MaxBytes).
			With("reason", "too_long").With("field", "text").With("chars", n).With("bytes", b)
	}
	for _, s := range secretKinds {
		for _, re := range s.patterns {
			if re.MatchString(t) {
				return "", apperr.Invalid("the text looks as if it holds a secret (%s); memory never keeps passwords, tokens or keys", s.kind).
					With("reason", "holds_secret").With("kind", s.kind)
			}
		}
	}
	return t, nil
}

// secretKinds are the shapes of secret memory refuses, by the kind a refusal
// names. A Core token or invitation is matched as the runtime matches it
// (its config.HoldsCoreToken), byte for byte; the rest are the providers'
// keys, JSON Web Tokens (Core's own assertions among them) and private keys.
var secretKinds = []struct {
	kind     string
	patterns []*regexp.Regexp
}{
	{"core_token", []*regexp.Regexp{regexp.MustCompile(`ais(?:inv)?_[a-z2-7]{12}_[A-Za-z0-9_-]{16,}`)}},
	{"provider_key", []*regexp.Regexp{
		regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),
		regexp.MustCompile(`\b(?:ghp|gho|ghs|ghu|github_pat)_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	}},
	{"cloud_key", []*regexp.Regexp{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)}},
	{"jwt", []*regexp.Regexp{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)}},
	{"private_key", []*regexp.Regexp{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)}},
}

var tagRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// CheckTags holds an entry's tags to at most MaxTags, each a lower-case word
// of up to 32 letters, digits, '-' and '_' (preference, goal, difficulty,
// progress, fact, ...). A tag given twice is kept once. None is an empty
// list, never nil: the column holds a list.
func CheckTags(tags []string) ([]string, *apperr.Error) {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !tagRE.MatchString(t) {
			return nil, apperr.Invalid("a tag is a lower-case word of at most 32 letters, digits, '-' and '_', such as preference or goal").
				With("reason", "bad_tags").With("field", "tags")
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	if len(out) > MaxTags {
		return nil, apperr.Invalid("at most %d tags", MaxTags).With("reason", "bad_tags").With("field", "tags")
	}
	return out, nil
}
