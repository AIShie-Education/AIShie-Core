package oauth

import "strings"

// CanonicalIssuer normalizes the configured public Core origin. PUBLIC_URL is
// trusted configuration, never a request's Host or forwarded headers.
func CanonicalIssuer(publicURL string) string { return strings.TrimRight(publicURL, "/") }

func TokenEndpoint(issuer string) string { return CanonicalIssuer(issuer) + "/v1/oauth/token" }
func ResourceURL(issuer string) string   { return CanonicalIssuer(issuer) + Resource }
func ProtectedResourceMetadataURL(issuer string) string {
	return CanonicalIssuer(issuer) + "/.well-known/oauth-protected-resource"
}

// AuthorizationServerMetadata is RFC 8414 metadata for client-credentials
// clients. There is deliberately no authorization or registration endpoint.
type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
}

func NewAuthorizationServerMetadata(issuer string) AuthorizationServerMetadata {
	issuer = CanonicalIssuer(issuer)
	return AuthorizationServerMetadata{
		Issuer: issuer, TokenEndpoint: TokenEndpoint(issuer),
		GrantTypesSupported:               []string{"client_credentials"},
		TokenEndpointAuthMethodsSupported: []string{"client_secret_basic", "client_secret_post"},
		ScopesSupported:                   SupportedScopes(),
	}
}

// ProtectedResourceMetadata is RFC 9728 metadata for the sole OAuth resource.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

func NewProtectedResourceMetadata(issuer string) ProtectedResourceMetadata {
	issuer = CanonicalIssuer(issuer)
	return ProtectedResourceMetadata{Resource: ResourceURL(issuer), AuthorizationServers: []string{issuer},
		ScopesSupported: SupportedScopes(), BearerMethodsSupported: []string{"header"}}
}
