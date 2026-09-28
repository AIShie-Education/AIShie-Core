package memory_test

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/memory"
)

// Words of scripts written with spaces are words; Chinese, Japanese and
// Korean are split into pairs of characters, since nothing else marks where
// their words end.
func TestSearchText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"English", "Prefers worked examples in Python, not C++!", "prefers worked examples in python not c"},
		{"Chinese", "我喜欢递归", "我喜 喜欢 欢递 递归"},
		{"one Chinese character", "我", "我"},
		{"Japanese", "再帰が苦手です。", "再帰 帰が が苦 苦手 手で です"},
		{"Katakana with its long vowel", "コーヒーが好き", "コー ーヒ ヒー ーが が好 好き"},
		{"half-width Katakana", "ｺｰﾋｰ", "コー ーヒ ヒー"},
		{"Korean", "재귀 함수", "재귀 함수"},
		{"mixed", "HW3は金曜日17:00まで", "hw3 は金 金曜 曜日 17 00 まで"},
		{"full-width letters and digits", "ＰＹＴＨＯＮ３", "python3"},
		{"accents", "Café crème", "café crème"},
		{"a script whose vowels are marks", "नमस्ते दुनिया", "नमस्ते दुनिया"},
		{"a mark with nothing before it", "́x", "x"},
		{"punctuation and symbols alone", "--- :) ***", ""},
		{"empty", "", ""},
	} {
		if got := memory.SearchText(tc.in); got != tc.want {
			t.Errorf("%s: SearchText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestQueryTerms(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"words of four characters or more as prefixes", "Recursion help", "recursion:* | help:*"},
		{"short words whole", "is it due", "is | it | due"},
		{"each word once", "Python python PYTHON", "python:*"},
		{"Chinese pairs whole", "递归函数", "递归 | 归函 | 函数"},
		{"Japanese", "再帰", "再帰"},
		{"mixed", "HW3の締め切り", "hw3 | の締 | 締め | め切 | 切り"},
		{"what to_tsquery reads as syntax, dropped", `a & b | !(c) <-> d:* 'e' \f`, "a | b | c | d | e | f"},
		{"nothing to look for", " ?! ", ""},
		{"empty", "", ""},
	} {
		if got := memory.QueryTerms(tc.in); got != tc.want {
			t.Errorf("%s: QueryTerms(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	// At most 24 terms, and a term at most 64 bytes, cut at a character.
	many := strings.Repeat("a b c d e f g h i j k l m n o p q r s t u v w x y z ", 2)
	if n := len(memory.Terms(many)); n != 26 {
		t.Fatalf("Terms counts %d words in the alphabet", n)
	}
	if got := memory.QueryTerms(many); strings.Count(got, "|") != memory.MaxQueryTerms-1 || !strings.HasSuffix(got, "x") {
		t.Fatalf("QueryTerms of 26 words: %q", got)
	}
	long := memory.QueryTerms(strings.Repeat("é", 40))
	if !strings.HasSuffix(long, ":*") || len(strings.TrimSuffix(long, ":*")) != 64 || !utf8.ValidString(long) {
		t.Fatalf("a long word: %q", long)
	}
}

// queryShape is what QueryTerms may give: terms of letters, digits and
// marks, a prefix mark on some, joined by " | ".
var queryShape = regexp.MustCompile(`^([\p{L}\p{N}\p{M}]+(:\*)?( \| [\p{L}\p{N}\p{M}]+(:\*)?)*)?$`)

// Whatever is searched for, QueryTerms gives an argument to_tsquery takes
// (see also TestQueryTermsInTheDatabase).
func FuzzQueryTermsShape(f *testing.F) {
	for _, s := range []string{"", "Recursion help", "递归函数", `a & b | !(c) <-> d:* 'e' \f`, "\xff\xfe", "ｶﾞｰ", "́́"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q := memory.QueryTerms(s)
		if !queryShape.MatchString(q) {
			t.Fatalf("QueryTerms(%q) = %q", s, q)
		}
		if q == "" {
			return
		}
		terms := strings.Split(q, " | ")
		if len(terms) > memory.MaxQueryTerms {
			t.Fatalf("%d terms", len(terms))
		}
		for _, term := range terms {
			if len(strings.TrimSuffix(term, ":*")) > 64 {
				t.Fatalf("a term of %d bytes", len(term))
			}
		}
	})
}
