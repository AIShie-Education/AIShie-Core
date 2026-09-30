package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig describes an OpenID Connect provider. For PolyU's ADFS the
// issuer is https://<adfs-host>/adfs and the subject claim is "upn".
type OIDCConfig struct {
	// Name is what this installation calls the provider; it is what
	// credential.provider holds.
	Name         string
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is <PUBLIC_URL>/v1/auth/sso/callback, registered with the
	// provider.
	RedirectURL string
	// SubjectClaim names the claim that identifies the account. "sub" is what
	// the standard says; ADFS's sub is an opaque per-client value, and the
	// UPN is what an administrator can actually type when linking an account.
	SubjectClaim string
	Scopes       []string
	// EmailClaim, when set, names the claim holding the person's email,
	// which Exchange reads into Identity.Email when the provider vouches for
	// it (email_verified). Empty, no email is read.
	EmailClaim string
	// HTTPClient is what discovery, the keys and the code's exchange are
	// fetched with; nil is http.DefaultClient.
	HTTPClient *http.Client
}

type oidcProvider struct {
	cfg      OIDCConfig
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewOIDC discovers the provider's endpoints and keys from its issuer URL.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (IdentityProvider, error) {
	if cfg.Name == "" || cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc: name, issuer, client id and redirect URL are all required")
	}
	if cfg.SubjectClaim == "" {
		cfg.SubjectClaim = "sub"
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, cfg.HTTPClient)
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery at %s: %w", cfg.Issuer, err)
	}
	return &oidcProvider{
		cfg:      cfg,
		oauth:    oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: cfg.RedirectURL, Scopes: cfg.Scopes},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

func (p *oidcProvider) Name() string { return p.cfg.Name }

func (p *oidcProvider) AuthCodeURL(state, nonce string) string {
	return p.oauth.AuthCodeURL(state, oidc.Nonce(nonce))
}

func (p *oidcProvider) Exchange(ctx context.Context, code, nonce string) (Identity, error) {
	if p.cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, p.cfg.HTTPClient)
	}
	token, err := p.oauth.Exchange(ctx, code)
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: code exchange: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, errors.New("oidc: the provider returned no id_token")
	}
	// Verify checks the signature against the provider's keys, the issuer,
	// the audience (our client id) and the expiry.
	idToken, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: id_token: %w", err)
	}
	// The nonce ties this token to the sign-in this browser started. Without
	// it a token issued for another sign-in could be replayed here.
	if nonce == "" || idToken.Nonce != nonce {
		return Identity{}, errors.New("oidc: the id_token does not answer this sign-in (nonce mismatch)")
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("oidc: claims: %w", err)
	}
	subject, _ := claims[p.cfg.SubjectClaim].(string)
	if subject == "" {
		return Identity{}, fmt.Errorf("oidc: the id_token has no %q claim", p.cfg.SubjectClaim)
	}
	id := Identity{Provider: p.cfg.Name, Subject: subject}
	if p.cfg.EmailClaim != "" {
		id.Email = verifiedEmail(claims, p.cfg.EmailClaim)
	}
	return id, nil
}

// verifiedEmail is the email in claim, lower-cased, when the provider
// vouches for it: email_verified is true, as a boolean or, as some providers
// write it, a string. An email it does not vouch for is none.
func verifiedEmail(claims map[string]any, claim string) string {
	email, _ := claims[claim].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	switch v := claims["email_verified"].(type) {
	case bool:
		if v {
			return email
		}
	case string:
		if v == "true" {
			return email
		}
	}
	return ""
}
