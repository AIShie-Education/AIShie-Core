package apperr_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
)

// A message that repeats a megabyte is cut short, between two characters,
// and says how long it was, in 400 bytes all told.
func TestALongMessageIsCutBetweenCharacters(t *testing.T) {
	for _, r := range []string{"<", "é", "あ", "😀"} {
		long := strings.Repeat(r, 1<<20)
		msg := apperr.Invalid("the value %q does not parse", long).Message
		if len(msg) > 400 || !utf8.ValidString(msg) || !strings.HasPrefix(msg, `the value "`+r) || !strings.HasSuffix(msg, " bytes)") {
			t.Errorf("%s: %d bytes: %q", r, len(msg), msg)
		}
	}
	if msg := apperr.Invalid("title is required").Message; msg != "title is required" {
		t.Errorf("a short message was changed: %q", msg)
	}
}
