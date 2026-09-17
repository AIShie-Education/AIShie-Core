// Package apperr is the one error type that crosses the tool layer's
// boundary. A tool returns an *Error for anything that is the caller's doing;
// the adapters map Code to an HTTP status or an MCP tool error. Any other
// error is an internal fault: the transaction rolls back and the caller sees
// a generic 500.
package apperr

import (
	"errors"
	"fmt"
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

func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
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
