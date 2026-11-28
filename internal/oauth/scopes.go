package oauth

import (
	"errors"
	"slices"
	"strings"
)

const (
	ScopeRead  = "mcp.read"
	ScopeWrite = "mcp.write"
	Resource   = "/mcp"
)

var supportedScopes = []string{ScopeRead, ScopeWrite}

// SupportedScopes returns the OAuth scopes Core offers, in their canonical
// order. The returned slice is independent of the package's own.
func SupportedScopes() []string { return slices.Clone(supportedScopes) }

// ParseScope parses an OAuth scope field. An omitted or blank field means the
// client's own scopes, not every supported scope.
func ParseScope(field string) []string {
	scopes := strings.Fields(field)
	if len(scopes) == 0 {
		return nil
	}
	return scopes
}

// ClientScopes validates a client's configured scopes. Nil selects the
// default, mcp.read. Unknown, empty and duplicate scopes are refused.
func ClientScopes(scopes []string) ([]string, error) {
	if scopes == nil {
		return []string{ScopeRead}, nil
	}
	if len(scopes) == 0 {
		return nil, ErrInvalidScope
	}
	return canonicalScopes(scopes)
}

// RequestedScopes intersects a token request with the client's configured
// scopes. Nil means all of the client's scopes; it never widens them.
func RequestedScopes(requested, client []string) ([]string, error) {
	if requested == nil {
		return canonicalScopes(client)
	}
	wanted, err := canonicalScopes(requested)
	if err != nil {
		return nil, err
	}
	for _, scope := range wanted {
		if !slices.Contains(client, scope) {
			return nil, ErrInvalidScope
		}
	}
	return wanted, nil
}

func canonicalScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, ErrInvalidScope
	}
	out := make([]string, 0, len(supportedScopes))
	for _, candidate := range supportedScopes {
		if slices.Contains(scopes, candidate) {
			out = append(out, candidate)
		}
	}
	if len(out) != len(scopes) {
		return nil, ErrInvalidScope
	}
	return out, nil
}

// ScopeString is the form OAuth token responses and challenges carry.
func ScopeString(scopes []string) string { return strings.Join(scopes, " ") }

// HasScope reports whether an OAuth token carries scope.
func HasScope(scopes []string, scope string) bool { return slices.Contains(scopes, scope) }

var (
	// ErrInvalidClient deliberately covers unknown, revoked, suspended and bad
	// secret alike, so token responses do not disclose which one occurred.
	ErrInvalidClient = errors.New("invalid_client")
	// ErrInvalidGrant covers a resource other than this Core's canonical /mcp.
	ErrInvalidGrant = errors.New("invalid_grant")
	// ErrInvalidScope covers an unknown or non-subset scope request.
	ErrInvalidScope = errors.New("invalid_scope")
	// ErrInvalidToken deliberately covers malformed, expired, revoked and
	// otherwise unusable access tokens alike.
	ErrInvalidToken = errors.New("invalid_token")
)
