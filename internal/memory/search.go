package memory

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Memory is searched with PostgreSQL's full text search, in its 'simple'
// configuration: no stemming, no stop words, the same for every language.
// That configuration splits words at spaces and punctuation, and Chinese
// and Japanese are written without spaces, so the application splits the
// text itself before the database sees it: SearchText is what the entry's
// search column is made from, and QueryTerms what a search looks for, both
// by one tokenisation.
//
// A run of letters and digits of a script written with spaces is one word.
// A run of Han, Hiragana, Katakana or Hangul becomes its overlapping pairs
// of characters (bigrams), or the one character of a run of one: 記憶力
// becomes 記憶 憶力, and a search for 記憶 finds it. Everything else
// separates. Text is NFKC-normalised and lower-cased first, so that full-
// and half-width forms, and upper and lower case, find each other.

// maxTermBytes bounds one term of a search.
const maxTermBytes = 64

// SearchText is what memory_entry.search_text holds for an entry's text: its
// words, as the tokenisation above makes them, separated by single spaces.
func SearchText(text string) string {
	return strings.Join(tokens(text), " ")
}

// Terms is what a search for q looks for: its words, as SearchText makes
// them, each once, in the order they first come, each cut to at most 64
// bytes. memory.search refuses a query of more than MaxQueryTerms.
func Terms(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tokens(q) {
		t = cut(t, maxTermBytes)
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// QueryTerms is the argument to to_tsquery('simple', …) that finds entries
// holding any of the first MaxQueryTerms of q's Terms: the terms joined with
// "|", a word of four characters or more as a prefix (word:*), so that
// "recurs" finds "recursion". A term holds only letters, digits and marks,
// none of which means anything to to_tsquery, so the argument is never a
// syntax error, whatever q holds. An empty result means there is nothing to
// look for: rank by recency alone.
func QueryTerms(q string) string {
	terms := Terms(q)
	if len(terms) > MaxQueryTerms {
		terms = terms[:MaxQueryTerms]
	}
	for i, t := range terms {
		if r, _ := utf8.DecodeRuneInString(t); !isCJK(r) && utf8.RuneCountInString(t) >= 4 {
			terms[i] = t + ":*"
		}
	}
	return strings.Join(terms, " | ")
}

// tokens splits text as the package comment says.
func tokens(text string) []string {
	var out []string
	var word, cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		switch len(cjk) {
		case 0:
		case 1:
			out = append(out, string(cjk))
		default:
			for i := 0; i+1 < len(cjk); i++ {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range strings.ToLower(norm.NFKC.String(text)) {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			flushCJK()
			word = append(word, r)
		case unicode.Is(unicode.M, r) && len(word) > 0:
			// A mark belongs to the letter before it: many scripts write
			// their vowels so. It never starts a word.
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

// isCJK reports whether r is written without spaces between words: Han,
// Hiragana, Katakana, Hangul, and the long-vowel mark that Katakana words
// carry (ー), which is of neither script.
func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) || r == 'ー'
}

// cut shortens s to at most n bytes, at the end of a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
