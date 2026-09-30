// Package sso is single sign-on's providers as a sign-in finds them: the one
// the server's operator sets in the environment (OIDC_ISSUER and the rest),
// and those the site's administrators set up from the front end, kept in the
// database (sso_provider, migration 0022; docs/schema.md §2.1, Single
// sign-on). The operator's is read-only to administrators, is offered first,
// and wins over a provider of the site's with its id.
//
// A sign-in reads the site's providers from the database each time, so that
// a provider added, changed, switched off or removed is so on every instance
// at once, with no restart. What it reads of a provider over the network —
// its discovery document and its keys — is kept, per instance, until the
// provider's settings change or an hour has passed.
package sso

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/secrets"
)

// Where a provider comes from.
const (
	// SourceOperator is the provider the server's operator sets in the
	// environment: read-only to administrators.
	SourceOperator = "operator"
	// SourceSite is a provider the site's administrators set up.
	SourceSite = "site"
)

// CallbackPath is where every provider sends the browser back, after the
// server's public URL: what to register with a provider as its redirect URI.
const CallbackPath = "/v1/auth/sso/callback"

// SecretPurpose is what a provider's sealed client secret is bound to,
// with the provider's id.
const SecretPurpose = "sso_provider.client_secret" //nolint:gosec // a column's name, not a credential

// SecretBinding is what provider id's client secret is sealed under.
func SecretBinding(id string) secrets.Binding {
	return secrets.Binding{Purpose: SecretPurpose, Owner: id}
}

// Operator is the provider the server's operator sets in the environment,
// discovered when the server starts.
type Operator struct {
	// ID is OIDC_PROVIDER_NAME: what credential.provider records.
	ID string
	// DisplayName is OIDC_DISPLAY_NAME, or "" for the front end's own words.
	DisplayName  string
	Issuer       string
	ClientID     string
	SecretHint   string
	Scopes       []string
	SubjectClaim string
	// IdP is the provider, discovered.
	IdP auth.IdentityProvider
}

// Config is what a Registry reads providers with.
type Config struct {
	// Pool reads the site's providers; nil means there are none.
	Pool *pgxpool.Pool
	// Operator is the operator's provider; nil when OIDC_ISSUER is not set.
	Operator *Operator
	// Keys open the site's providers' client secrets, and seal new ones;
	// nil when the server has no SECRETS_KEY.
	Keys *secrets.Keyring
	// PublicURL is how browsers reach this server: the redirect URI is it
	// and CallbackPath.
	PublicURL string
	// Client fetches providers' documents and keys and exchanges codes; nil
	// is one with a ten-second timeout.
	Client *http.Client
	// Log says what goes wrong with a provider, which a sign-in's answer
	// does not.
	Log *slog.Logger
	// Rediscover is how long what was read of a provider over the network
	// is used before it is read again; zero is an hour.
	Rediscover time.Duration
	// Discover makes a provider from its settings, reading its discovery
	// document; nil is auth.NewOIDC.
	Discover func(context.Context, auth.OIDCConfig) (auth.IdentityProvider, error)
}

// Registry finds the providers a sign-in may go through.
type Registry struct {
	cfg Config

	mu     sync.Mutex
	cache  map[string]*discovered
	flight map[string]*sync.Mutex
}

// discovered is a site's provider as discovered, with the settings it was
// discovered with.
type discovered struct {
	fingerprint [32]byte
	idp         auth.IdentityProvider
	at          time.Time
}

// DefaultTimeout bounds each request to a provider.
const DefaultTimeout = 10 * time.Second

// New is a registry.
func New(cfg Config) *Registry {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: DefaultTimeout}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Rediscover <= 0 {
		cfg.Rediscover = time.Hour
	}
	if cfg.Discover == nil {
		cfg.Discover = auth.NewOIDC
	}
	return &Registry{cfg: cfg, cache: map[string]*discovered{}, flight: map[string]*sync.Mutex{}}
}

// Operator is the operator's provider, or nil.
func (r *Registry) Operator() *Operator {
	if r == nil {
		return nil
	}
	return r.cfg.Operator
}

// Keys are the server's secrets keys, or nil.
func (r *Registry) Keys() *secrets.Keyring {
	if r == nil {
		return nil
	}
	return r.cfg.Keys
}

// Client is what providers are reached with.
func (r *Registry) Client() *http.Client {
	if r == nil {
		return &http.Client{Timeout: DefaultTimeout}
	}
	return r.cfg.Client
}

// RedirectURL is the redirect URI every provider is registered with.
func (r *Registry) RedirectURL() string {
	base := "http://localhost"
	if r != nil && r.cfg.PublicURL != "" {
		base = r.cfg.PublicURL
	}
	return strings.TrimRight(base, "/") + CallbackPath
}

// Offer is a provider a sign-in may go through now, as the sign-in page is
// told of it.
type Offer struct {
	ID     string
	Label  *string
	Source string
}

// Offered lists the providers a sign-in may go through now, in the sign-in
// page's order: the operator's first, then the site's that are switched
// on, by position. A provider of the site's with the operator's id is not
// offered: the operator's is. Nor is one whose client secret this server
// cannot open, which could sign nobody in: that is logged.
func (r *Registry) Offered(ctx context.Context) ([]Offer, error) {
	var out []Offer
	if op := r.Operator(); op != nil {
		o := Offer{ID: op.ID, Source: SourceOperator}
		if op.DisplayName != "" {
			o.Label = &op.DisplayName
		}
		out = append(out, o)
	}
	rows, err := r.enabled(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		if r.shadowed(p.ID) {
			continue
		}
		if _, err := r.cfg.Keys.Open(SecretBinding(p.ID), p.ClientSecretSealed); err != nil {
			r.cfg.Log.Warn("an identity provider is switched on, but its client secret does not open with this server's keys; it is not offered",
				"provider", p.ID, "err", err)
			continue
		}
		label := p.DisplayName
		out = append(out, Offer{ID: p.ID, Label: &label, Source: SourceSite})
	}
	return out, nil
}

func (r *Registry) enabled(ctx context.Context) ([]dbq.ListEnabledSSOProvidersRow, error) {
	if r == nil || r.cfg.Pool == nil {
		return nil, nil
	}
	rows, err := dbq.New(r.cfg.Pool).ListEnabledSSOProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity providers: %w", err)
	}
	return rows, nil
}

// shadowed reports whether id is the operator's provider's.
func (r *Registry) shadowed(id string) bool {
	op := r.Operator()
	return op != nil && op.ID == id
}

// Resolved is a provider a sign-in goes through: its id and source, whether
// it links an identity to an account by the email it vouches for, the
// domains it may, and the provider itself.
type Resolved struct {
	ID                  string
	Source              string
	LinkByEmail         bool
	AllowedEmailDomains []string
	IdP                 auth.IdentityProvider
}

var (
	// ErrNotOffered is a provider nobody may sign in through now: none has
	// the id, or the site's is switched off.
	ErrNotOffered = apperr.Missing("there is no identity provider of that name to sign in through here").
			With("reason", "sso_provider_not_found")
	// ErrUnavailable is a provider that is switched on, and cannot be used
	// now: its secret does not open, or it cannot be reached. Why is in the
	// log.
	ErrUnavailable = apperr.Precondition("that identity provider cannot be reached now; try again later, or sign in another way").
			With("reason", "sso_provider_unavailable")
)

// Resolve finds the provider id, to sign in through: the operator's, or one
// of the site's that is switched on, discovered with its settings as they
// are now.
func (r *Registry) Resolve(ctx context.Context, id string) (Resolved, error) {
	if op := r.Operator(); op != nil && op.ID == id {
		return Resolved{ID: op.ID, Source: SourceOperator, IdP: op.IdP}, nil
	}
	rows, err := r.enabled(ctx)
	if err != nil {
		return Resolved{}, err
	}
	for _, p := range rows {
		if p.ID != id {
			continue
		}
		secret, err := r.cfg.Keys.Open(SecretBinding(p.ID), p.ClientSecretSealed)
		if err != nil {
			r.cfg.Log.Error("an identity provider's client secret does not open with this server's keys", "provider", p.ID, "err", err)
			return Resolved{}, ErrUnavailable
		}
		cfg := auth.OIDCConfig{Name: p.ID, Issuer: p.Issuer, ClientID: p.ClientID, ClientSecret: secret,
			RedirectURL: r.RedirectURL(), SubjectClaim: p.SubjectClaim, Scopes: p.Scopes, HTTPClient: r.cfg.Client}
		if p.EmailClaim != nil {
			cfg.EmailClaim = *p.EmailClaim
		}
		idp, err := r.discover(ctx, cfg)
		if err != nil {
			r.cfg.Log.Warn("an identity provider could not be discovered", "provider", p.ID, "issuer", p.Issuer, "err", err)
			return Resolved{}, ErrUnavailable
		}
		return Resolved{ID: p.ID, Source: SourceSite, LinkByEmail: p.LinkByEmail && cfg.EmailClaim != "",
			AllowedEmailDomains: p.AllowedEmailDomains, IdP: idp}, nil
	}
	return Resolved{}, ErrNotOffered
}

// fingerprint says whether two settings are the same, secret included,
// without keeping the secret.
func fingerprint(c auth.OIDCConfig) [32]byte {
	h := sha256.New()
	for _, f := range append([]string{c.Name, c.Issuer, c.ClientID, c.ClientSecret, c.RedirectURL, c.SubjectClaim, c.EmailClaim},
		c.Scopes...) {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(f), f)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// discover is cfg's provider, as discovered before when its settings were
// the same and not long ago, or discovered now. One discovery of a provider
// at a time: a lecture hall starting to sign in at once waits for one.
func (r *Registry) discover(ctx context.Context, cfg auth.OIDCConfig) (auth.IdentityProvider, error) {
	fp := fingerprint(cfg)
	fresh := func() auth.IdentityProvider {
		r.mu.Lock()
		defer r.mu.Unlock()
		if d := r.cache[cfg.Name]; d != nil && d.fingerprint == fp && time.Since(d.at) < r.cfg.Rediscover {
			return d.idp
		}
		return nil
	}
	if idp := fresh(); idp != nil {
		return idp, nil
	}
	r.mu.Lock()
	one := r.flight[cfg.Name]
	if one == nil {
		one = &sync.Mutex{}
		r.flight[cfg.Name] = one
	}
	r.mu.Unlock()
	one.Lock()
	defer one.Unlock()
	if idp := fresh(); idp != nil {
		return idp, nil
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	idp, err := r.cfg.Discover(ctx, cfg)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache[cfg.Name] = &discovered{fingerprint: fp, idp: idp, at: time.Now()}
	r.mu.Unlock()
	return idp, nil
}

// Status is a provider's standing on the sign-in page, as administrators
// read it.
const (
	// StatusOffered: a sign-in may go through it.
	StatusOffered = "offered"
	// StatusDisabled: it is switched off.
	StatusDisabled = "disabled"
	// StatusIDTaken: the operator's provider has its id, and is offered in
	// its place.
	StatusIDTaken = "id_taken"
	// StatusSecretUnavailable: its client secret does not open with this
	// server's keys (SECRETS_KEY and SECRETS_KEY_PREVIOUS).
	StatusSecretUnavailable = "secret_unavailable"
)

// SiteStatus is the standing of the site's provider id, switched on or not,
// its secret sealed as sealed.
func (r *Registry) SiteStatus(id string, enabled bool, sealed string) string {
	switch {
	case r.shadowed(id):
		return StatusIDTaken
	case !enabled:
		return StatusDisabled
	}
	if _, err := r.Keys().Open(SecretBinding(id), sealed); err != nil {
		return StatusSecretUnavailable
	}
	return StatusOffered
}

// IsUnavailable reports whether err says a provider cannot be used now.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }
