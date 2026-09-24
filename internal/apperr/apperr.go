// Package apperr is the one error type that crosses the tool layer's
// boundary. A tool returns an *Error for anything that is the caller's doing;
// the adapters map Code to an HTTP status or an MCP tool error. Any other
// error is an internal fault: the transaction rolls back and the caller sees
// a generic 500.
package apperr

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

type Code string

const (
	InvalidArgument     Code = "invalid_argument"     // malformed or failing validation
	Unauthenticated     Code = "unauthenticated"      // no credential, or not a valid one
	Forbidden           Code = "forbidden"            // authenticated, not allowed
	NotFound            Code = "not_found"            //
	Conflict            Code = "conflict"             // the current state does not allow this
	IdempotencyConflict Code = "idempotency_conflict" // same key, different content
	FailedPrecondition  Code = "failed_precondition"  // a rule of the domain says no
	RateLimited         Code = "rate_limited"         // too many calls; never attempted, nothing recorded
)

type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// With returns a copy carrying one more detail.
func (e *Error) With(key string, value any) *Error {
	c := *e
	c.Details = make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		c.Details[k] = v
	}
	c.Details[key] = value
	return &c
}

// New makes an Error, its message held to maxMessage.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: clip(fmt.Sprintf(format, args...))}
}

// maxMessage bounds a message, in bytes. Many repeat something of what they
// refuse — a value the schema did not match, a date that did not parse, a
// name — and a refusal must not be a way to have the server send back a
// megabyte, or several once it is escaped for JSON. The server's own words
// are well under it; a value repeated in full is cut off, its start kept.
const maxMessage = 400

func clip(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	cut := maxMessage
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s… (%d bytes)", s[:cut], len(s))
}

func Invalid(format string, args ...any) *Error      { return New(InvalidArgument, format, args...) }
func Forbid(format string, args ...any) *Error       { return New(Forbidden, format, args...) }
func Missing(format string, args ...any) *Error      { return New(NotFound, format, args...) }
func Conflicts(format string, args ...any) *Error    { return New(Conflict, format, args...) }
func Precondition(format string, args ...any) *Error { return New(FailedPrecondition, format, args...) }

// As unwraps err to an *Error, if it is one.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Is reports whether err is an *Error with the given code.
func Is(err error, code Code) bool {
	e, ok := As(err)
	return ok && e.Code == code
}
