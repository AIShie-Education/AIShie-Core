// Package canon turns a tool call's JSON arguments into one canonical byte
// string and hashes it, so that "the same call again" and "a different call
// under the same idempotency key" can be told apart.
//
// The rule is named ais-canon-1. Only this server ever computes it, so what
// matters is that it is deterministic and that it never changes silently:
// stored hashes would stop matching. A different rule is a new name.
//
//  1. The arguments must be one JSON object. No arguments at all means {}.
//  2. Named top-level fields are removed first: the idempotency key, and any
//     field the tool marks secret (a password must not reach the action log).
//  3. Objects are written with keys sorted by their UTF-8 bytes; arrays keep
//     their order; there is no whitespace.
//  4. Numbers are written as plain decimals: no exponent, no leading or
//     trailing zeros, no "-0". 1, 1.0, 1e0 and 10e-1 are all "1".
//  5. Strings are written as Go's encoding/json writes them with HTML escaping
//     off, so "é" and "é" are the same string.
//  6. null and an absent key are different.
//
// The hash is hex(SHA-256(tool name, "\n", canonical bytes)).
package canon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Rule names the canonicalization in force.
const Rule = "ais-canon-1"

// Numbers beyond these bounds are refused rather than expanded: 1e999999999
// is a short string and a very long decimal.
const (
	maxExponent = 400
	maxDigits   = 400
)

// Canonicalize returns the canonical form of raw with the named top-level
// fields removed.
func Canonicalize(raw []byte, strip ...string) ([]byte, error) {
	return canonicalize(raw, strip, nil)
}

// Sealed returns the canonical form of raw with each named top-level field's
// value replaced by seal(field, canonical value of it) — a string that stands
// for the value without being it. It is how a call that carries a secret gets
// a payload hash that still tells one secret from another: the stored payload
// (Canonicalize) drops the secret; the hash (Sealed) commits to it.
func Sealed(raw []byte, seal func(field string, value []byte) string, fields ...string) ([]byte, error) {
	return canonicalize(raw, fields, seal)
}

func canonicalize(raw []byte, fields []string, seal func(string, []byte) string) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("arguments contain more than one JSON value")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	for _, k := range fields {
		val, present := obj[k]
		if seal == nil || !present {
			delete(obj, k)
			continue
		}
		var one bytes.Buffer
		if err := write(&one, val); err != nil {
			return nil, err
		}
		obj[k] = seal(k, one.Bytes())
	}
	var buf bytes.Buffer
	if err := write(&buf, obj); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Hash is the payload hash stored on an action: 64 lowercase hex characters.
func Hash(tool string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{'\n'})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

func write(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case json.Number:
		n, err := normalizeNumber(string(x))
		if err != nil {
			return err
		}
		buf.WriteString(n)
	case string:
		return writeString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := write(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // Go compares strings bytewise
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := write(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canon: unexpected %T", v)
	}
	return nil
}

func writeString(buf *bytes.Buffer, s string) error {
	// PostgreSQL's jsonb cannot hold U+0000, so a payload with one could
	// never be recorded; refusing it here makes that the caller's mistake
	// rather than a server fault to retry for ever.
	if strings.ContainsRune(s, 0) {
		return fmt.Errorf("strings cannot contain U+0000")
	}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1) // Encode appends a newline
	return nil
}

// normalizeNumber rewrites a JSON number literal as a plain decimal. It works
// on the digits directly, so no value is ever rounded.
func normalizeNumber(lit string) (string, error) {
	s := lit
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	exp := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > maxExponent || e < -maxExponent {
			return "", fmt.Errorf("number %q is out of range", lit)
		}
		exp, s = e, s[:i]
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	digits := intPart + fracPart
	if len(digits) > maxDigits {
		return "", fmt.Errorf("number %q has too many digits", lit)
	}
	// value = digits × 10^-scale
	scale := len(fracPart) - exp

	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}
	for scale > 0 && strings.HasSuffix(digits, "0") {
		digits = digits[:len(digits)-1]
		scale--
	}

	var out string
	switch {
	case scale <= 0:
		out = digits + strings.Repeat("0", -scale)
	case len(digits) <= scale:
		out = "0." + strings.Repeat("0", scale-len(digits)) + digits
	default:
		out = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if neg {
		out = "-" + out
	}
	return out, nil
}
