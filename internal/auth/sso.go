package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
)

// Identity is who an identity provider says someone is.
type Identity struct {
	// Provider is this installation's name for the provider, as stored in
	// credential.provider: "polyu-adfs".
	Provider string
	// Subject is the provider's stable name for the account, as stored in
	// credential.subject: for ADFS, the UPN.
	Subject string
}

// IdentityProvider is single sign-on, reduced to what this system needs from
// it: somewhere to send the browser, and a way to turn what comes back into
// an Identity. The protocol — OIDC today — stays behind it.
type IdentityProvider interface {
	Name() string
	// AuthCodeURL is where to send the browser. state comes back unchanged;
	// nonce comes back inside the signed token and proves the answer is to
	// this question.
	AuthCodeURL(state, nonce string) string
	// Exchange turns the code the browser came back with into an Identity,
	// having verified everything the protocol says to verify.
	Exchange(ctx context.Context, code, nonce string) (Identity, error)
}

// errNotRegistered is for an identity the provider vouches for and this
// installation does not know. Accounts are not created on first sign-in:
// someone registers the actor and links the identity first
// (actor.register, actor.link_sso).
var errNotRegistered = apperr.Forbid("that account signed in with the identity provider but is not registered here; ask an administrator to add it")

// NormalizeSubject puts a subject in the form it is stored and compared in.
// A UPN or an email address names the same account however it is capitalised,
// and the provider is free to send it either way, so anything of that shape
// is lower-cased. An opaque identifier is left exactly as it is.
func NormalizeSubject(subject string) string {
	subject = strings.TrimSpace(subject)
	if strings.Contains(subject, "@") {
		return strings.ToLower(subject)
	}
	return subject
}

// SignInWithIdentity starts a session for the actor an identity is linked to.
func (a *Authenticator) SignInWithIdentity(ctx context.Context, id Identity) (Session, error) {
	id.Subject = NormalizeSubject(id.Subject)
	if id.Provider == "" || id.Subject == "" {
		return Session{}, errors.New("auth: an identity needs a provider and a subject")
	}
	cred, err := dbq.New(a.pool).GetSSOCredential(ctx, dbq.GetSSOCredentialParams{Provider: &id.Provider, Subject: &id.Subject})
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, errNotRegistered
	}
	if err != nil {
		return Session{}, fmt.Errorf("identity lookup: %w", err)
	}
	if cred.RevokedAt != nil || cred.ActorStatus != domain.ActorActive {
		// Unlinked, or suspended: the same answer as never having been
		// registered, which is all an outsider needs to know.
		return Session{}, errNotRegistered
	}
	// An agent's link, from before migration 0017 revoked them: an agent
	// holds API tokens only, and never signs in.
	if cred.ActorKind == "agent" {
		return Session{}, ErrAgentsUseTokens
	}
	return a.StartSession(ctx, cred.ActorID, "sso: "+id.Provider)
}
