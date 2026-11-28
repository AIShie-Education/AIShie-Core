package httpapi

import (
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/oauth"
)

const maxOAuthTokenBody = 16 << 10

// oauthIssuer is the canonical public Core origin. Production always uses
// PUBLIC_URL; deriving only as a test/development fallback never trusts
// X-Forwarded-Host or other client-supplied forwarding headers.
func (s *server) oauthIssuer(r *http.Request) string {
	if s.PublicURL != "" {
		return oauth.CanonicalIssuer(s.PublicURL)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *server) oauthAuthorizationServer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, oauth.NewAuthorizationServerMetadata(s.oauthIssuer(r)))
}

func (s *server) oauthProtectedResource(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, oauth.NewProtectedResourceMetadata(s.oauthIssuer(r)))
}

// oauthError is an OAuth token-endpoint error, never an apperr carrying
// database, provider or credential details.
func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func oauthInvalidClient(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="mcp-client"`)
	oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
}

func (s *server) oauthToken(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOAuthTokenBody))
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the token request body could not be read")
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the token request form is malformed")
		return
	}
	for _, field := range []string{"grant_type", "resource", "scope", "client_id", "client_secret"} {
		if len(form[field]) > 1 {
			oauthError(w, http.StatusBadRequest, "invalid_request", field+" is given more than once")
			return
		}
	}
	if form.Get("grant_type") != "client_credentials" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only client_credentials is supported")
		return
	}

	clientIDText, secret := form.Get("client_id"), form.Get("client_secret")
	if id, pass, ok := r.BasicAuth(); ok {
		// HTTP Basic is preferred. Conflicting body credentials are ambiguous
		// and refused rather than silently choosing one.
		if (clientIDText != "" && clientIDText != id) || (secret != "" && secret != pass) {
			oauthError(w, http.StatusBadRequest, "invalid_request", "Basic and form client credentials disagree")
			return
		}
		clientIDText, secret = id, pass
	}
	if clientIDText == "" || secret == "" {
		oauthInvalidClient(w)
		return
	}
	clientID, parseErr := uuid.Parse(clientIDText)
	if parseErr != nil {
		oauthInvalidClient(w)
		return
	}

	// Bound guessing both per client and per source address, using the same
	// proxy-aware address rule as sign-in. A refusal records no secret detail.
	keys := []string{"oauth-client:" + clientIDText}
	if addr, known := s.clientAddr(r); known {
		keys = append(keys, "oauth-address:"+addr)
	}
	for _, key := range keys {
		if ok, wait := s.OAuthTokenLimit.Allow(key); !ok {
			secs := max(1, int(math.Ceil(wait.Seconds())))
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many token requests")
			return
		}
	}

	issuer := s.oauthIssuer(r)
	issued, err := s.OAuth.IssueToken(r.Context(), clientID, secret, oauth.ParseScope(form.Get("scope")),
		form.Get("resource"), oauth.ResourceURL(issuer))
	switch {
	case err == nil:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": issued.Value, "token_type": "Bearer",
			"expires_in": int(oauth.AccessTokenTTL.Seconds()), "scope": oauth.ScopeString(issued.Scopes),
		})
	case errors.Is(err, oauth.ErrInvalidClient):
		oauthInvalidClient(w)
	case errors.Is(err, oauth.ErrInvalidScope):
		oauthError(w, http.StatusBadRequest, "invalid_scope", "requested scope is not allowed for this client")
	case errors.Is(err, oauth.ErrInvalidGrant):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "resource must be the canonical public /mcp URL")
	default:
		s.Log.Error("OAuth token issue failed", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "the token could not be issued")
	}
}
