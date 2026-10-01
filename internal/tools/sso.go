package tools

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/secrets"
	"github.com/AIShie-Education/AIShie-Core/internal/sso"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Single sign-on's providers, as the site's administrators set them up
// (docs/schema.md §2.1, Single sign-on): root and the platform's
// administrators, and no department's, since a provider signs people in to
// the whole site. Each write is an action, recorded without the client
// secret, which is sealed before it is kept and never shown again but as its
// hint. The provider the server's operator sets in the environment is listed
// with them, read-only.

func ssoTools(d Deps) []tool.Tool {
	return []tool.Tool{ssoList(d), ssoGet(d), ssoCreate(d), ssoUpdate(d), ssoSetEnabled(d), ssoDelete(d), ssoTest(d),
		ssoLinkByEmail(d)}
}

// ToolSSOLinkByEmail is the call a sign-in makes, as the person signing in,
// to link their identity at a provider that links by email: neither adapter
// offers it.
const ToolSSOLinkByEmail = "sso.link_by_email"

// SSOActor is who made or changed a provider.
type SSOActor struct {
	ActorID     uuid.UUID `json:"actor_id"`
	DisplayName string    `json:"display_name"`
}

// SSOProviderView is a provider as administrators read it. Never its client
// secret: a hint of it.
type SSOProviderView struct {
	ID                  string     `json:"id" jsonschema:"the provider's id, such as polyu-adfs: what actor.link_sso names, and what a sign-in through it starts with; it never changes"`
	Source              string     `json:"source" jsonschema:"site: set up by the site's administrators; operator: set by the server's operator in its environment (OIDC_*), read-only here"`
	ReadOnly            bool       `json:"read_only" jsonschema:"true for the operator's provider, which only the operator changes"`
	DisplayName         *string    `json:"display_name" jsonschema:"the name on the sign-in button; null for the operator's when OIDC_DISPLAY_NAME is not set, and the front end then uses words of its own"`
	Issuer              string     `json:"issuer"`
	ClientID            string     `json:"client_id"`
	ClientSecretHint    string     `json:"client_secret_hint" jsonschema:"what may be shown of the client secret: an ellipsis and its last four characters, or the ellipsis alone for a secret shorter than 20 characters"`
	ClientSecretKeyID   *string    `json:"client_secret_key_id" jsonschema:"the id of the secrets key that sealed it (SECRETS_KEY); null for the operator's, which is not sealed"`
	Scopes              []string   `json:"scopes"`
	SubjectClaim        string     `json:"subject_claim" jsonschema:"the claim an account is known by, as actor.link_sso's subject"`
	EmailClaim          *string    `json:"email_claim" jsonschema:"the claim holding the person's email, or null for none"`
	AllowedEmailDomains []string   `json:"allowed_email_domains"`
	LinkByEmail         bool       `json:"link_by_email" jsonschema:"whether someone the provider vouches for, whose identity is linked to nobody, is linked at sign-in to the account whose email the provider vouches for, within allowed_email_domains; otherwise only accounts already linked sign in"`
	Enabled             bool       `json:"enabled"`
	Position            int        `json:"position" jsonschema:"its place on the sign-in page, lowest first; the operator's is always first"`
	Status              string     `json:"status" jsonschema:"offered: a sign-in may go through it; disabled: switched off; id_taken: the operator's provider has its id, and is offered in its place; secret_unavailable: its client secret does not open with this server's keys, so give it again"`
	LinkedAccounts      int        `json:"linked_accounts" jsonschema:"the accounts that sign in through it: identities linked at it and not unlinked"`
	Version             *int32     `json:"version" jsonschema:"moves on with every change; give it to sso.update, sso.set_enabled and sso.delete (or in If-Match) to change only what you read. Null for the operator's"`
	CreatedAt           *time.Time `json:"created_at"`
	CreatedBy           *SSOActor  `json:"created_by"`
	UpdatedAt           *time.Time `json:"updated_at"`
	UpdatedBy           *SSOActor  `json:"updated_by"`
	RedirectURI         string     `json:"redirect_uri" jsonschema:"what to register with the provider as the redirect URI: this server's public URL and /v1/auth/sso/callback, the same for every provider"`
}

// ssoRow is a provider row as either query reads it.
type ssoRow = dbq.GetSSOProviderRow

func siteView(d Deps, r ssoRow) SSOProviderView {
	name, version, pos := r.DisplayName, r.Version, int(r.Position)
	created, updated := r.CreatedAt, r.UpdatedAt
	v := SSOProviderView{ID: r.ID, Source: sso.SourceSite, DisplayName: &name, Issuer: r.Issuer, ClientID: r.ClientID,
		ClientSecretHint: r.ClientSecretHint, Scopes: strs(r.Scopes), SubjectClaim: r.SubjectClaim, EmailClaim: r.EmailClaim,
		AllowedEmailDomains: strs(r.AllowedEmailDomains), LinkByEmail: r.LinkByEmail, Enabled: r.Enabled, Position: pos,
		Status: d.SSO.SiteStatus(r.ID, r.Enabled, r.ClientSecretSealed), LinkedAccounts: int(r.LinkedAccounts), Version: &version,
		CreatedAt: &created, CreatedBy: &SSOActor{ActorID: r.CreatedByActorID, DisplayName: r.CreatedByName},
		UpdatedAt: &updated, UpdatedBy: &SSOActor{ActorID: r.UpdatedByActorID, DisplayName: r.UpdatedByName},
		RedirectURI: d.SSO.RedirectURL()}
	if kid, err := secrets.SealedKeyID(r.ClientSecretSealed); err == nil {
		v.ClientSecretKeyID = &kid
	}
	if v.Status == sso.StatusIDTaken {
		// The identities recorded under its id are the operator's
		// provider's, which signs them in.
		v.LinkedAccounts = 0
	}
	return v
}

func operatorView(ctx context.Context, d Deps, q dbq.Querier) (SSOProviderView, error) {
	op := d.SSO.Operator()
	linked, err := q.CountSSOLinks(ctx, &op.ID)
	if err != nil {
		return SSOProviderView{}, err
	}
	v := SSOProviderView{ID: op.ID, Source: sso.SourceOperator, ReadOnly: true, Issuer: op.Issuer, ClientID: op.ClientID,
		ClientSecretHint: op.SecretHint, Scopes: strs(op.Scopes), SubjectClaim: op.SubjectClaim, AllowedEmailDomains: []string{},
		Enabled: true, Status: sso.StatusOffered, LinkedAccounts: int(linked), RedirectURI: d.SSO.RedirectURL()}
	if op.DisplayName != "" {
		name := op.DisplayName
		v.DisplayName = &name
	}
	return v, nil
}

func strs(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func isOperators(d Deps, id string) bool {
	op := d.SSO.Operator()
	return op != nil && op.ID == id
}

var (
	errSetByOperator = apperr.Precondition("that provider is set by the server's operator, in its environment (OIDC_*), and changes only there").
				With("reason", "set_by_operator")
	errNoSSOProvider = apperr.Missing("there is no identity provider with that id").With("reason", "sso_provider_not_found")
	errNoSecretsKey  = apperr.Precondition("this server has no secrets key (SECRETS_KEY) to seal a client secret with, so no identity provider "+
		"is added or given a secret here; its operator sets one (openssl rand -base64 32), and the provider the operator sets "+
		"in the environment signs people in meanwhile").With("reason", "secrets_key_missing")
)

func errVersion(current int32) *apperr.Error {
	return apperr.Conflicts("the provider has changed since you read it (version %d now); read it again", current).
		With("reason", "version_mismatch").With("current_version", current)
}

func errIDTaken(operator bool) *apperr.Error {
	if operator {
		return apperr.Conflicts("the server's operator's provider has that id; choose another").With("reason", "id_taken")
	}
	return apperr.Conflicts("a provider with that id is there already; change it (sso.update), or choose another id").With("reason", "id_taken")
}

// ---------------------------------------------------------------------------
// sso.list, sso.get
// ---------------------------------------------------------------------------

type SSOListIn struct{}

type SSOListOut struct {
	Providers []SSOProviderView `json:"providers" jsonschema:"the operator's first, if there is one, then the site's by position"`
	// CanAdd says whether sso.create may succeed at all here.
	CanAdd          bool    `json:"can_add" jsonschema:"whether a provider may be added here: false when the server has no secrets key (SECRETS_KEY) to seal its client secret with"`
	CannotAddReason *string `json:"cannot_add_reason" jsonschema:"secrets_key_missing when can_add is false; null otherwise"`
	SecretsKeyID    *string `json:"secrets_key_id" jsonschema:"the id of the key that seals client secrets now; a provider whose client_secret_key_id differs was sealed by an older key"`
	RedirectURI     string  `json:"redirect_uri" jsonschema:"what to register with every provider as its redirect URI"`
}

func ssoList(d Deps) tool.Tool {
	return tool.Define(tool.Spec[SSOListIn, SSOListOut]{
		Name: "sso.list",
		Description: "List the identity providers a person may sign in through (single sign-on): the one the server's operator " +
			"sets in its environment, read-only, and those the site's administrators set up, each with its status, how many " +
			"accounts are linked at it, and the redirect URI to register with it. Never a client secret: a hint of it. " +
			"Root and platform administrators only.",
		Kind: tool.Read, Gate: admins,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/sso/providers"},
		Resolve: noTarget[SSOListIn]("sso_provider"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ SSOListIn) (SSOListOut, error) {
			out := SSOListOut{Providers: []SSOProviderView{}, CanAdd: d.SSO.Keys().CanSeal(), RedirectURI: d.SSO.RedirectURL()}
			if !out.CanAdd {
				why := "secrets_key_missing"
				out.CannotAddReason = &why
			} else {
				kid := d.SSO.Keys().KeyID()
				out.SecretsKeyID = &kid
			}
			if d.SSO.Operator() != nil {
				v, err := operatorView(ctx, d, rc.Q)
				if err != nil {
					return SSOListOut{}, err
				}
				out.Providers = append(out.Providers, v)
			}
			rows, err := rc.Q.ListSSOProviders(ctx)
			if err != nil {
				return SSOListOut{}, err
			}
			for _, r := range rows {
				out.Providers = append(out.Providers, siteView(d, ssoRow(r)))
			}
			return out, nil
		},
	})
}

type SSOProviderIDIn struct {
	ProviderID string `json:"provider_id" jsonschema:"the provider's id, such as polyu-adfs"`
}

func ssoGet(d Deps) tool.Tool {
	return tool.Define(tool.Spec[SSOProviderIDIn, SSOProviderView]{
		Name: "sso.get",
		Description: "Read one identity provider, the operator's or the site's: its settings, status, version and who last " +
			"changed it, and the redirect URI to register with it. Never its client secret: a hint of it. Root and platform " +
			"administrators only.",
		Kind: tool.Read, Gate: admins,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/sso/providers/{provider_id}"},
		Resolve: noTarget[SSOProviderIDIn]("sso_provider"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in SSOProviderIDIn) (SSOProviderView, error) {
			if isOperators(d, in.ProviderID) {
				return operatorView(ctx, d, rc.Q)
			}
			return readSSOProvider(ctx, d, rc.Q, in.ProviderID)
		},
	})
}

func readSSOProvider(ctx context.Context, d Deps, q dbq.Querier, id string) (SSOProviderView, error) {
	r, err := q.GetSSOProvider(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return SSOProviderView{}, errNoSSOProvider
	}
	if err != nil {
		return SSOProviderView{}, err
	}
	return siteView(d, r), nil
}

// ---------------------------------------------------------------------------
// sso.create
// ---------------------------------------------------------------------------

type SSOCreateIn struct {
	ID                  string   `json:"id" jsonschema:"the provider's id: 1 to 64 lower-case letters, digits and hyphens, such as hainanu-cas; what actor.link_sso names, and what it is known by for good"`
	DisplayName         string   `json:"display_name" jsonschema:"the name on the sign-in button, such as PolyU NetID: 1 to 64 printable characters"`
	Issuer              string   `json:"issuer" jsonschema:"the provider's issuer, exactly as its discovery document writes it: an https URL, such as https://adfs.example.edu/adfs, at a public address: localhost, or an address on this machine or a private or link-local one, is refused (issuer_address_not_allowed) unless the server's operator sets SSO_ALLOW_PRIVATE_ISSUERS, and http is taken only then, for this machine; a name that resolves to such an address is taken, and sso.test reports it"`
	ClientID            string   `json:"client_id" jsonschema:"the client id the provider gave this site"`
	ClientSecret        string   `json:"client_secret" jsonschema:"the client secret the provider gave this site: sealed before it is kept, never recorded and never shown again but as its hint"`
	Scopes              []string `json:"scopes,omitempty" jsonschema:"what a sign-in asks for, openid among them; default openid profile email"`
	SubjectClaim        string   `json:"subject_claim,omitempty" jsonschema:"the claim an account is known by, as actor.link_sso's subject; default sub (ADFS: upn)"`
	EmailClaim          *string  `json:"email_claim,omitempty" jsonschema:"the claim holding the person's email, for link_by_email; default none, or email with link_by_email"`
	AllowedEmailDomains []string `json:"allowed_email_domains,omitempty" jsonschema:"the domains an email may be linked from, such as polyu.edu.hk; required with link_by_email"`
	LinkByEmail         bool     `json:"link_by_email,omitempty" jsonschema:"link someone the provider vouches for, whose identity is linked to nobody, to the active person whose email here is the one the provider vouches for (email_verified), in allowed_email_domains; never an account with a platform role. Default false: only accounts already linked (actor.link_sso) sign in"`
	Enabled             bool     `json:"enabled,omitempty" jsonschema:"offer it on the sign-in page at once; default false, so that it can be tested (sso.test) and switched on (sso.set_enabled)"`
	Position            *int     `json:"position,omitempty" jsonschema:"its place on the sign-in page, 0 to 10000, lowest first; default after every other"`
}

// ssoSettings is a provider's settings, held to their rules.
type ssoSettings struct {
	displayName, issuer, clientID, subjectClaim string
	scopes, domains                             []string
	emailClaim                                  *string
	linkByEmail                                 bool
	position                                    int
}

// check holds s to the rules settings are held to together: linking by
// email reads an email from a claim, email unless said, and needs domains.
func (s *ssoSettings) check() error {
	if s.linkByEmail {
		if s.emailClaim == nil {
			email := "email"
			s.emailClaim = &email
		}
		if len(s.domains) == 0 {
			return apperr.Invalid("allowed_email_domains: linking by email needs the domains an email may be linked from").
				With("field", "allowed_email_domains")
		}
	}
	return sso.CheckPosition(s.position)
}

func ssoCreate(d Deps) tool.Tool {
	return tool.Define(tool.Spec[SSOCreateIn, SSOProviderView]{
		Name: "sso.create",
		Description: "Set up an identity provider (OpenID Connect) a person may sign in through. Register the redirect URI " +
			"sso.list gives with the provider first, and test its issuer (sso.test). The client secret is sealed with the server's " +
			"secrets key before it is kept, never recorded, and never shown again but as its hint; without a secrets key nothing " +
			"is added (secrets_key_missing). It is created switched off unless enabled is true. An id the operator's provider " +
			"or another has is refused (id_taken). Accounts sign in through it once linked at it (actor.link_sso, with its id), " +
			"or by the email it vouches for with link_by_email. Root and platform administrators only.",
		Kind: tool.Write, Gate: admins,
		HTTP:     tool.Route{Method: "POST", Pattern: "/v1/sso/providers"},
		SecretIn: []string{"client_secret"},
		Resolve:  noTarget[SSOCreateIn]("sso_provider"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SSOCreateIn) (SSOProviderView, error) {
			if err := sso.CheckID(in.ID); err != nil {
				return SSOProviderView{}, err
			}
			var s ssoSettings
			var err error
			if s.displayName, err = sso.CheckDisplayName(in.DisplayName); err != nil {
				return SSOProviderView{}, err
			}
			if s.issuer, err = sso.CheckIssuer(in.Issuer, d.SSO.PrivateIssuers()); err != nil {
				return SSOProviderView{}, err
			}
			if s.clientID, err = sso.CheckClientID(in.ClientID); err != nil {
				return SSOProviderView{}, err
			}
			secret, err := sso.CheckClientSecret(in.ClientSecret)
			if err != nil {
				return SSOProviderView{}, err
			}
			if s.scopes, err = sso.CheckScopes(in.Scopes); err != nil {
				return SSOProviderView{}, err
			}
			s.subjectClaim = "sub"
			if in.SubjectClaim != "" {
				if s.subjectClaim, err = sso.CheckClaim("subject_claim", in.SubjectClaim); err != nil {
					return SSOProviderView{}, err
				}
			}
			if in.EmailClaim != nil && strings.TrimSpace(*in.EmailClaim) != "" {
				c, err := sso.CheckClaim("email_claim", *in.EmailClaim)
				if err != nil {
					return SSOProviderView{}, err
				}
				s.emailClaim = &c
			}
			if s.domains, err = sso.CheckDomains(in.AllowedEmailDomains); err != nil {
				return SSOProviderView{}, err
			}
			s.linkByEmail = in.LinkByEmail
			if in.Position != nil {
				s.position = *in.Position
			} else {
				n, err := ec.Q.NextSSOPosition(ctx)
				if err != nil {
					return SSOProviderView{}, err
				}
				s.position = int(n)
			}
			if err := s.check(); err != nil {
				return SSOProviderView{}, err
			}
			if isOperators(d, in.ID) {
				return SSOProviderView{}, errIDTaken(true)
			}
			if taken, err := ec.Q.SSOProviderExists(ctx, in.ID); err != nil {
				return SSOProviderView{}, err
			} else if taken {
				return SSOProviderView{}, errIDTaken(false)
			}
			if !d.SSO.Keys().CanSeal() {
				return SSOProviderView{}, errNoSecretsKey
			}
			sealed, err := d.SSO.Keys().Seal(sso.SecretBinding(in.ID), secret)
			if err != nil {
				return SSOProviderView{}, err
			}
			err = ec.Q.InsertSSOProvider(ctx, dbq.InsertSSOProviderParams{ID: in.ID, DisplayName: s.displayName, Issuer: s.issuer,
				ClientID: s.clientID, ClientSecretSealed: sealed, ClientSecretHint: secrets.Hint(secret), Scopes: s.scopes,
				SubjectClaim: s.subjectClaim, EmailClaim: s.emailClaim, AllowedEmailDomains: s.domains, LinkByEmail: s.linkByEmail,
				Enabled: in.Enabled, Position: int32(s.position), ActorID: ec.Actor.ID, Now: ec.Now}) //nolint:gosec // held to 0..10000
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return SSOProviderView{}, errIDTaken(false)
			}
			if err != nil {
				return SSOProviderView{}, err
			}
			return readSSOProvider(ctx, d, ec.Q, in.ID)
		},
	})
}

// ---------------------------------------------------------------------------
// sso.update
// ---------------------------------------------------------------------------

type SSOUpdateIn struct {
	ProviderID          string   `json:"provider_id" jsonschema:"the provider's id; it never changes"`
	Version             int32    `json:"version" jsonschema:"the version you read (sso.get, sso.list): the change is made only over it, and is refused (version_mismatch) if the provider changed since. Over REST the If-Match header may carry it instead"`
	DisplayName         *string  `json:"display_name,omitempty"`
	Issuer              *string  `json:"issuer,omitempty" jsonschema:"the identities linked at the provider stay linked: whoever the new issuer vouches for under the same subject signs in as them"`
	ClientID            *string  `json:"client_id,omitempty"`
	ClientSecret        *string  `json:"client_secret,omitempty" jsonschema:"a new client secret, sealed as sso.create seals one; left out, the secret is kept as it is"`
	Scopes              []string `json:"scopes,omitempty" jsonschema:"left out, kept; empty, the default openid profile email"`
	SubjectClaim        *string  `json:"subject_claim,omitempty"`
	EmailClaim          *string  `json:"email_claim,omitempty" jsonschema:"an empty string for none"`
	AllowedEmailDomains []string `json:"allowed_email_domains,omitempty" jsonschema:"left out, kept; empty, none"`
	LinkByEmail         *bool    `json:"link_by_email,omitempty"`
	Position            *int     `json:"position,omitempty"`
}

func ssoUpdate(d Deps) tool.Tool {
	return tool.Define(tool.Spec[SSOUpdateIn, SSOProviderView]{
		Name: "sso.update",
		Description: "Change a site's identity provider, over the version you read (version, or If-Match): what you give is " +
			"changed, and what you leave out is kept, the client secret included, which is replaced only when you give a new " +
			"one. A change since you read it is refused (version_mismatch, with current_version). Its id never changes. " +
			"The operator's provider is refused (set_by_operator). It takes effect at the next sign-in, on every instance. " +
			"Root and platform administrators only.",
		Kind: tool.Write, Gate: admins,
		HTTP:     tool.Route{Method: "POST", Pattern: "/v1/sso/providers/{provider_id}", IfMatch: "version"},
		SecretIn: []string{"client_secret"},
		Resolve:  noTarget[SSOUpdateIn]("sso_provider"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SSOUpdateIn) (SSOProviderView, error) {
			if isOperators(d, in.ProviderID) {
				return SSOProviderView{}, errSetByOperator
			}
			locked, err := lockSSOProvider(ctx, ec.Q, in.ProviderID, &in.Version)
			if err != nil {
				return SSOProviderView{}, err
			}
			cur, err := ec.Q.GetSSOProvider(ctx, in.ProviderID)
			if err != nil {
				return SSOProviderView{}, err
			}
			s := ssoSettings{displayName: cur.DisplayName, issuer: cur.Issuer, clientID: cur.ClientID, subjectClaim: cur.SubjectClaim,
				scopes: cur.Scopes, domains: strs(cur.AllowedEmailDomains), emailClaim: cur.EmailClaim, linkByEmail: cur.LinkByEmail,
				position: int(cur.Position)}
			if in.DisplayName != nil {
				if s.displayName, err = sso.CheckDisplayName(*in.DisplayName); err != nil {
					return SSOProviderView{}, err
				}
			}
			if in.Issuer != nil {
				if s.issuer, err = sso.CheckIssuer(*in.Issuer, d.SSO.PrivateIssuers()); err != nil {
					return SSOProviderView{}, err
				}
			}
			if in.ClientID != nil {
				if s.clientID, err = sso.CheckClientID(*in.ClientID); err != nil {
					return SSOProviderView{}, err
				}
			}
			if in.Scopes != nil {
				if s.scopes, err = sso.CheckScopes(in.Scopes); err != nil {
					return SSOProviderView{}, err
				}
			}
			if in.SubjectClaim != nil {
				if s.subjectClaim, err = sso.CheckClaim("subject_claim", *in.SubjectClaim); err != nil {
					return SSOProviderView{}, err
				}
			}
			if in.EmailClaim != nil {
				s.emailClaim = nil
				if strings.TrimSpace(*in.EmailClaim) != "" {
					c, err := sso.CheckClaim("email_claim", *in.EmailClaim)
					if err != nil {
						return SSOProviderView{}, err
					}
					s.emailClaim = &c
				}
			}
			if in.AllowedEmailDomains != nil {
				if s.domains, err = sso.CheckDomains(in.AllowedEmailDomains); err != nil {
					return SSOProviderView{}, err
				}
			}
			if in.LinkByEmail != nil {
				s.linkByEmail = *in.LinkByEmail
			}
			if in.Position != nil {
				s.position = *in.Position
			}
			if err := s.check(); err != nil {
				return SSOProviderView{}, err
			}
			sealed, hint := locked.ClientSecretSealed, cur.ClientSecretHint
			if in.ClientSecret != nil {
				secret, err := sso.CheckClientSecret(*in.ClientSecret)
				if err != nil {
					return SSOProviderView{}, err
				}
				if !d.SSO.Keys().CanSeal() {
					return SSOProviderView{}, errNoSecretsKey
				}
				if sealed, err = d.SSO.Keys().Seal(sso.SecretBinding(in.ProviderID), secret); err != nil {
					return SSOProviderView{}, err
				}
				hint = secrets.Hint(secret)
			}
			if _, err := ec.Q.UpdateSSOProvider(ctx, dbq.UpdateSSOProviderParams{ID: in.ProviderID, Version: locked.Version,
				DisplayName: s.displayName, Issuer: s.issuer, ClientID: s.clientID, ClientSecretSealed: sealed, ClientSecretHint: hint,
				Scopes: s.scopes, SubjectClaim: s.subjectClaim, EmailClaim: s.emailClaim, AllowedEmailDomains: s.domains,
				LinkByEmail: s.linkByEmail, Position: int32(s.position), ActorID: ec.Actor.ID, Now: ec.Now}); err != nil { //nolint:gosec // held to 0..10000
				return SSOProviderView{}, err
			}
			return readSSOProvider(ctx, d, ec.Q, in.ProviderID)
		},
	})
}

// lockSSOProvider holds the site's provider id for a change made over
// version, when one is given.
func lockSSOProvider(ctx context.Context, q *dbq.Queries, id string, version *int32) (dbq.LockSSOProviderRow, error) {
	locked, err := q.LockSSOProvider(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return locked, errNoSSOProvider
	}
	if err != nil {
		return locked, err
	}
	if version != nil && *version != locked.Version {
		return locked, errVersion(locked.Version)
	}
	return locked, nil
}

// ---------------------------------------------------------------------------
// sso.set_enabled
// ---------------------------------------------------------------------------

type SSOSetEnabledIn struct {
	ProviderID string `json:"provider_id"`
	Enabled    bool   `json:"enabled" jsonschema:"true offers it on the sign-in page and lets a sign-in through it; false stops both, at once, and unlinks nobody"`
	Version    *int32 `json:"version,omitempty" jsonschema:"the version you read; the change is then made only over it (version_mismatch). Over REST the If-Match header may carry it"`
}

func ssoSetEnabled(d Deps) tool.Tool {
	return tool.Define(tool.Spec[SSOSetEnabledIn, SSOProviderView]{
		Name: "sso.set_enabled",
		Description: "Switch a site's identity provider on or off. Off, it is not offered on the sign-in page, a sign-in through it " +
			"is refused, even one already under way, and nobody is unlinked: switched on again, everyone linked signs in as " +
			"before. One whose client secret does not open with this server's keys is not switched on (secret_unavailable): " +
			"give the secret again (sso.update). Already so, nothing changes. The operator's provider is refused " +
			"(set_by_operator). Root and platform administrators only.",
		Kind: tool.Write, Gate: admins,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/sso/providers/{provider_id}/enabled", IfMatch: "version"},
		Resolve: noTarget[SSOSetEnabledIn]("sso_provider"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SSOSetEnabledIn) (SSOProviderView, error) {
			if isOperators(d, in.ProviderID) {
				return SSOProviderView{}, errSetByOperator
			}
			locked, err := lockSSOProvider(ctx, ec.Q, in.ProviderID, in.Version)
			if err != nil {
				return SSOProviderView{}, err
			}
			if locked.Enabled != in.Enabled {
				if in.Enabled {
					if _, err := d.SSO.Keys().Open(sso.SecretBinding(in.ProviderID), locked.ClientSecretSealed); err != nil {
						return SSOProviderView{}, apperr.Precondition("its client secret does not open with this server's keys "+
							"(SECRETS_KEY, SECRETS_KEY_PREVIOUS); give it again (sso.update) before switching it on").
							With("reason", sso.StatusSecretUnavailable)
					}
				}
				if _, err := ec.Q.SetSSOProviderEnabled(ctx, dbq.SetSSOProviderEnabledParams{ID: in.ProviderID, Enabled: in.Enabled,
					Version: locked.Version, ActorID: ec.Actor.ID, Now: ec.Now}); err != nil {
					return SSOProviderView{}, err
				}
			}
			return readSSOProvider(ctx, d, ec.Q, in.ProviderID)
		},
	})
}

// ---------------------------------------------------------------------------
// sso.delete
// ---------------------------------------------------------------------------

type SSODeleteIn struct {
	ProviderID string `json:"provider_id"`
	Force      bool   `json:"force,omitempty" jsonschema:"delete it although accounts are linked at it, unlinking them: they no longer sign in through it, and sign in otherwise, or not at all. Without it, a provider accounts are linked at is refused (provider_in_use, with linked_accounts)"`
	Version    *int32 `json:"version,omitempty" jsonschema:"the version you read; the deletion is then made only over it (version_mismatch). Over REST the If-Match header may carry it"`
}

type SSODeleteOut struct {
	ID               string `json:"id"`
	Deleted          bool   `json:"deleted"`
	UnlinkedAccounts int    `json:"unlinked_accounts" jsonschema:"the accounts whose identity at it was unlinked: with force, as many as were linked; otherwise 0"`
}

func ssoDelete(d Deps) tool.Tool {
	return tool.Define(tool.Spec[SSODeleteIn, SSODeleteOut]{
		Name: "sso.delete",
		Description: "Remove a site's identity provider. While accounts are linked at it, it is refused (provider_in_use), saying " +
			"how many (details.linked_accounts) would no longer sign in through it; with force it is removed and they are " +
			"unlinked (revoked, kept for the record: linking the same identity to the same account again brings it back, and to " +
			"another account never). Switching it off (sso.set_enabled) keeps everyone linked. The operator's provider is " +
			"refused (set_by_operator). Root and platform administrators only.",
		Kind: tool.Write, Gate: admins,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/sso/providers/{provider_id}/delete", IfMatch: "version"},
		Resolve: noTarget[SSODeleteIn]("sso_provider"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SSODeleteIn) (SSODeleteOut, error) {
			if isOperators(d, in.ProviderID) {
				// A site's provider with the operator's id is not offered,
				// and its identities are the operator's: it cannot be
				// removed here without them, and waits for the operator.
				return SSODeleteOut{}, errSetByOperator
			}
			locked, err := lockSSOProvider(ctx, ec.Q, in.ProviderID, in.Version)
			if err != nil {
				return SSODeleteOut{}, err
			}
			linked, err := ec.Q.CountSSOLinks(ctx, &in.ProviderID)
			if err != nil {
				return SSODeleteOut{}, err
			}
			out := SSODeleteOut{ID: in.ProviderID, Deleted: true}
			if linked > 0 {
				if !in.Force {
					return SSODeleteOut{}, apperr.Conflicts("%d accounts are linked at it and would no longer sign in through it; "+
						"delete it with force to unlink them too, or switch it off (sso.set_enabled) to keep them linked", linked).
						With("reason", "provider_in_use").With("linked_accounts", linked)
				}
				n, err := ec.Q.RevokeSSOLinks(ctx, dbq.RevokeSSOLinksParams{Provider: &in.ProviderID, RevokedAt: &ec.Now})
				if err != nil {
					return SSODeleteOut{}, err
				}
				out.UnlinkedAccounts = int(n)
			}
			if n, err := ec.Q.DeleteSSOProvider(ctx, dbq.DeleteSSOProviderParams{ID: in.ProviderID, Version: locked.Version}); err != nil {
				return SSODeleteOut{}, err
			} else if n != 1 {
				return SSODeleteOut{}, errVersion(locked.Version)
			}
			return out, nil
		},
	})
}

// ---------------------------------------------------------------------------
// sso.test
// ---------------------------------------------------------------------------

type SSOTestIn struct {
	ProviderID   *string  `json:"provider_id,omitempty" jsonschema:"a provider already set up, the operator's or the site's, switched on or not: its issuer, scopes and claims are tested"`
	Issuer       *string  `json:"issuer,omitempty" jsonschema:"an issuer to test before setting it up; give this or provider_id"`
	Scopes       []string `json:"scopes,omitempty" jsonschema:"with issuer: the scopes a sign-in would ask for; default openid profile email"`
	SubjectClaim *string  `json:"subject_claim,omitempty" jsonschema:"with issuer: the claim an account would be known by; default sub"`
	EmailClaim   *string  `json:"email_claim,omitempty" jsonschema:"with issuer: the claim an email would be read from"`
}

func ssoTest(d Deps) tool.Tool {
	return tool.Define(tool.Spec[SSOTestIn, sso.Report]{
		Name: "sso.test",
		Description: "Test an identity provider's issuer without signing anyone in: read its discovery document " +
			"(<issuer>/.well-known/openid-configuration) and its key set, check them as a sign-in would use them, and say what " +
			"was found — its endpoints, its signing keys, the scopes and claims it supports — with problems (what stops a " +
			"sign-in: ok is false) and warnings (what may). Give provider_id for a provider set up, or issuer for one to be. " +
			"It sends no secret, follows no redirect and changes nothing. A provider of the site's is fetched only from a public address, " +
			"checked on the address each connection is made to, and its token endpoint, which is not fetched, is resolved: one on this " +
			"machine, or on a private or link-local address, is a problem (issuer_address_not_allowed) unless the server's operator sets " +
			"SSO_ALLOW_PRIVATE_ISSUERS. Root and platform administrators only.",
		Kind: tool.Read, Gate: admins,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/sso/test"},
		Resolve: noTarget[SSOTestIn]("sso_provider"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in SSOTestIn) (sso.Report, error) {
			var issuer string
			var want sso.Want
			// The site's providers are held to public addresses, unless the
			// operator says otherwise; the operator's provider is not.
			client, private := d.SSO.Client(), d.SSO.PrivateIssuers()
			switch {
			case (in.ProviderID == nil) == (in.Issuer == nil):
				return sso.Report{}, apperr.Invalid("give provider_id or issuer, one of them")
			case in.ProviderID != nil && isOperators(d, *in.ProviderID):
				op := d.SSO.Operator()
				issuer, want = op.Issuer, sso.Want{Scopes: op.Scopes, SubjectClaim: op.SubjectClaim}
				client, private = d.SSO.OperatorClient(), true
			case in.ProviderID != nil:
				r, err := rc.Q.GetSSOProvider(ctx, *in.ProviderID)
				if errors.Is(err, pgx.ErrNoRows) {
					return sso.Report{}, errNoSSOProvider
				}
				if err != nil {
					return sso.Report{}, err
				}
				issuer, want = r.Issuer, sso.Want{Scopes: r.Scopes, SubjectClaim: r.SubjectClaim}
				if r.EmailClaim != nil {
					want.EmailClaim = *r.EmailClaim
				}
			default:
				issuer = strings.TrimSpace(*in.Issuer)
				scopes, err := sso.CheckScopes(in.Scopes)
				if err != nil {
					return sso.Report{}, err
				}
				want = sso.Want{Scopes: scopes, SubjectClaim: "sub"}
				if in.SubjectClaim != nil {
					if want.SubjectClaim, err = sso.CheckClaim("subject_claim", *in.SubjectClaim); err != nil {
						return sso.Report{}, err
					}
				}
				if in.EmailClaim != nil && strings.TrimSpace(*in.EmailClaim) != "" {
					if want.EmailClaim, err = sso.CheckClaim("email_claim", *in.EmailClaim); err != nil {
						return sso.Report{}, err
					}
				}
			}
			return sso.Inspect(ctx, client, private, issuer, want), nil
		},
	})
}

// ---------------------------------------------------------------------------
// sso.link_by_email: a sign-in's, as the person signing in
// ---------------------------------------------------------------------------

type SSOLinkByEmailIn struct {
	ProviderID string `json:"provider_id"`
	Subject    string `json:"subject"`
	Email      string `json:"email"`
}

// errNoEmailLink is every refusal of linking by email: the sign-in it is
// made for answers as for anyone not registered, whichever it is.
func errNoEmailLink(why string) *apperr.Error {
	return apperr.Precondition("that identity is not linked by email here: %s", why).With("reason", "not_linked_by_email")
}

func ssoLinkByEmail(d Deps) tool.Tool {
	return tool.Define(tool.Spec[SSOLinkByEmailIn, CredentialIDOut]{
		Name: ToolSSOLinkByEmail,
		Description: "Link the identity someone signs in with, at a provider that links by email, to their own account: " +
			"called by a sign-in, as the person whose email the provider vouches for, and by nothing else.",
		Kind: tool.Write, Gate: tool.Gate{Self: true}, Unlisted: true,
		Resolve: noTarget[SSOLinkByEmailIn]("credential"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SSOLinkByEmailIn) (CredentialIDOut, error) {
			subject, email := auth.NormalizeSubject(in.Subject), strings.ToLower(strings.TrimSpace(in.Email))
			if subject == "" || email == "" {
				return CredentialIDOut{}, apperr.Invalid("provider_id, subject and email are required")
			}
			if isOperators(d, in.ProviderID) {
				return CredentialIDOut{}, errNoEmailLink("the operator's provider links nobody by email")
			}
			p, err := ec.Q.GetSSOProvider(ctx, in.ProviderID)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return CredentialIDOut{}, errNoEmailLink("no such provider")
			case err != nil:
				return CredentialIDOut{}, err
			case !p.Enabled || !p.LinkByEmail || p.EmailClaim == nil:
				return CredentialIDOut{}, errNoEmailLink("the provider is switched off, or links nobody by email")
			case !sso.InDomains(email, p.AllowedEmailDomains):
				return CredentialIDOut{}, errNoEmailLink("the email is of none of the provider's domains")
			}
			who, err := ec.Q.GetSSOLinkByEmailCandidate(ctx, email)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return CredentialIDOut{}, errNoEmailLink("nobody has that email")
			case err != nil:
				return CredentialIDOut{}, err
			case who.ID != ec.Actor.ID:
				return CredentialIDOut{}, errNoEmailLink("the email is someone else's")
			case who.Kind != "human" || who.Status != "active":
				return CredentialIDOut{}, errNoEmailLink("only an active person is linked by email")
			case !who.EmailVerified:
				return CredentialIDOut{}, errNoEmailLink("nobody here vouches for the person's email")
			case who.PlatformRole != nil:
				return CredentialIDOut{}, errNoEmailLink("an account with a platform role is linked by an administrator, never by email")
			}
			if known, err := ec.Q.SSOIdentityKnown(ctx, dbq.SSOIdentityKnownParams{Provider: &in.ProviderID, Subject: &subject}); err != nil {
				return CredentialIDOut{}, err
			} else if known {
				return CredentialIDOut{}, errNoEmailLink("the identity is, or was, linked already")
			}
			if has, err := ec.Q.HasLiveSSOLinkAt(ctx, dbq.HasLiveSSOLinkAtParams{Provider: &in.ProviderID, ActorID: ec.Actor.ID}); err != nil {
				return CredentialIDOut{}, err
			} else if has {
				return CredentialIDOut{}, errNoEmailLink("the account is linked at the provider under another identity")
			}
			id := ids.New()
			label := "linked at sign-in by the email the provider vouches for"
			if err := ec.Q.InsertCredential(ctx, dbq.InsertCredentialParams{ID: id, ActorID: ec.Actor.ID, Kind: auth.KindSSO,
				Provider: &in.ProviderID, Subject: &subject, Label: &label, CreatedAt: ec.Now}); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					return CredentialIDOut{}, errNoEmailLink("the identity was linked just now")
				}
				return CredentialIDOut{}, err
			}
			return CredentialIDOut{CredentialID: id}, nil
		},
	})
}
