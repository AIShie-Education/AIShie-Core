package tools

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// Platform tools are the few operations outside any course. They check
// actor.platform_role and nothing else: there is no ladder here, an admin may
// or may not. Everything inside a course is governed by membership instead —
// an admin who wants to grade has to be seated like anyone else.

func platformTools() []tool.Tool {
	return []tool.Tool{
		actorRegister(), actorGet(), actorList(), actorUpdate(), actorSuspend(), actorReactivate(),
		actorIssueToken(), actorListCredentials(), actorRevokeCredential(), actorInvite(), actorLinkSSO(),
		termCreate(), termList(), departmentCreate(), departmentList(),
		presetList(), presetCreate(), presetUpdate(),
	}
}

// admins gates every platform tool. What only root may do — make another
// admin, act on a holder of a platform role — is checked inside the tool,
// because it depends on the arguments and not on the tool.
var admins = tool.Gate{Platform: []string{domain.PlatformRoot, domain.PlatformAdmin}}

const (
	EventActorRegistered        = "actor.registered"
	EventActorUpdated           = "actor.updated"
	EventActorInvited           = "actor.invited"
	EventActorSuspended         = "actor.suspended"
	EventActorReactivated       = "actor.reactivated"
	EventActorCredentialRevoked = "actor.credential_revoked" //nolint:gosec // an event name, not a credential
)

// ---------------------------------------------------------------------------
// actor.*
// ---------------------------------------------------------------------------

type ActorRegisterIn struct {
	Kind         string  `json:"kind" jsonschema:"human or agent; recorded for display and audit, and read by nothing else"`
	DisplayName  string  `json:"display_name"`
	Email        *string `json:"email,omitempty" jsonschema:"needed for a person to sign in with a password"`
	PlatformRole *string `json:"platform_role,omitempty" jsonschema:"admin; only root may grant it"`
}

type ActorOut struct {
	ActorID uuid.UUID `json:"actor_id"`
}

func actorRegister() tool.Tool {
	return tool.Define(tool.Spec[ActorRegisterIn, ActorOut]{
		Name: "actor.register",
		Description: "Register a person or an agent. An actor can do nothing until it is seated in a course. " +
			"A person signs in once they have a password, which they choose through actor.invite, or through " +
			"single sign-on (actor.link_sso). " +
			"An agent is registered here and runs elsewhere: no endpoint, model or prompt is stored. " +
			"Give it a token with actor.issue_token.",
		Kind: tool.Write, Gate: admins,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/actors"},
		Resolve: noTarget[ActorRegisterIn]("actor"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActorRegisterIn) (ActorOut, error) {
			if in.Kind != "human" && in.Kind != "agent" {
				return ActorOut{}, apperr.Invalid("kind must be human or agent")
			}
			if strings.TrimSpace(in.DisplayName) == "" {
				return ActorOut{}, apperr.Invalid("display_name is required")
			}
			if in.PlatformRole != nil {
				if *in.PlatformRole != domain.PlatformAdmin {
					return ActorOut{}, apperr.Invalid("platform_role can only be admin; root is created once, at bootstrap")
				}
				if ec.Actor.PlatformRole != domain.PlatformRoot {
					return ActorOut{}, apperr.Forbid("only root makes an admin")
				}
			}
			if in.Email != nil {
				if taken, err := ec.Q.EmailTaken(ctx, *in.Email); err != nil {
					return ActorOut{}, err
				} else if taken {
					return ActorOut{}, apperr.Conflicts("that email already belongs to an actor")
				}
			}
			id := ids.New()
			if err := ec.Q.InsertActor(ctx, dbq.InsertActorParams{
				ID: id, Kind: in.Kind, DisplayName: in.DisplayName, Email: in.Email,
				PlatformRole: in.PlatformRole, CreatedByActorID: &ec.Actor.ID, CreatedAt: ec.Now,
			}); err != nil {
				return ActorOut{}, err
			}
			ec.Emit(events.Event{Type: EventActorRegistered, SubjectType: "actor", SubjectID: &id})
			return ActorOut{ActorID: id}, nil
		},
	})
}

type ActorIDIn struct {
	ActorID uuid.UUID `json:"actor_id"`
}

type ActorView struct {
	ID               uuid.UUID  `json:"id"`
	Kind             string     `json:"kind"`
	DisplayName      string     `json:"display_name"`
	Email            *string    `json:"email,omitempty"`
	Status           string     `json:"status"`
	PlatformRole     *string    `json:"platform_role,omitempty"`
	CreatedByActorID *uuid.UUID `json:"created_by_actor_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	// How they can sign in. A person with neither a password nor single
	// sign-on cannot yet: actor.invite is how they get a password.
	HasPassword     bool       `json:"has_password"`
	HasSSO          bool       `json:"has_sso" jsonschema:"an identity at the identity provider is linked (actor.link_sso)"`
	InviteExpiresAt *time.Time `json:"invite_expires_at,omitempty" jsonschema:"when the invitation not yet taken up expires, which may have passed; absent when there is none"`
}

func viewActor(a dbq.GetActorViewRow) ActorView {
	return ActorView{ID: a.ID, Kind: a.Kind, DisplayName: a.DisplayName, Email: a.Email, Status: a.Status,
		PlatformRole: a.PlatformRole, CreatedByActorID: a.CreatedByActorID, CreatedAt: a.CreatedAt,
		HasPassword: a.HasPassword, HasSSO: a.HasSso, InviteExpiresAt: a.InviteExpiresAt}
}

func resolveActor(ctx context.Context, q dbq.Querier, in ActorIDIn) (tool.Target, error) {
	if _, err := q.GetActor(ctx, in.ActorID); errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such actor")
	} else if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{Type: "actor", ID: &in.ActorID}, nil
}

func actorGet() tool.Tool {
	return tool.Define(tool.Spec[ActorIDIn, ActorView]{
		Name:        "actor.get",
		Description: "One actor's registration: who they are, their standing, who registered them, and how they can sign in.",
		Kind:        tool.Read, Gate: admins,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/actors/{actor_id}"},
		Resolve: resolveActor,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActorIDIn) (ActorView, error) {
			a, err := rc.Q.GetActorView(ctx, in.ActorID)
			if err != nil {
				return ActorView{}, err
			}
			return viewActor(a), nil
		},
	})
}

type ActorListIn struct {
	Kind   *string `json:"kind,omitempty" jsonschema:"human or agent"`
	Status *string `json:"status,omitempty" jsonschema:"active or suspended"`
	Search *string `json:"search,omitempty" jsonschema:"a piece of the name or of the email, in any case"`
	Page
}

type ActorListOut struct {
	Actors []ActorView `json:"actors"`
	Next   *uuid.UUID  `json:"next,omitempty"`
}

func actorList() tool.Tool {
	return tool.Define(tool.Spec[ActorListIn, ActorListOut]{
		Name: "actor.list",
		Description: "Everyone registered, people and agents, oldest first, with how each can sign in. " +
			"The system actor, which runs the background jobs, is not listed.",
		Kind: tool.Read, Gate: admins,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/actors"},
		Resolve: noTarget[ActorListIn]("actor"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActorListIn) (ActorListOut, error) {
			if in.Kind != nil && *in.Kind != "human" && *in.Kind != "agent" {
				return ActorListOut{}, apperr.Invalid("kind must be human or agent")
			}
			if in.Status != nil && *in.Status != domain.ActorActive && *in.Status != domain.ActorSuspended {
				return ActorListOut{}, apperr.Invalid("status must be active or suspended")
			}
			if in.Search != nil {
				if search := strings.TrimSpace(*in.Search); search != "" {
					in.Search = &search
				} else {
					in.Search = nil
				}
			}
			rows, err := rc.Q.ListActors(ctx, dbq.ListActorsParams{After: in.after(), Kind: in.Kind, Status: in.Status,
				Search: in.Search, MaxRows: in.limit()})
			out := ActorListOut{Actors: make([]ActorView, 0, len(rows))}
			for _, r := range rows {
				out.Actors = append(out.Actors, viewActor(dbq.GetActorViewRow(r)))
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, err
		},
	})
}

type ActorUpdateIn struct {
	ActorID     uuid.UUID `json:"actor_id"`
	DisplayName *string   `json:"display_name,omitempty"`
	Email       *string   `json:"email,omitempty" jsonschema:"what a person signs in with; it can be changed, not removed"`
}

func actorUpdate() tool.Tool {
	return tool.Define(tool.Spec[ActorUpdateIn, ActorView]{
		Name: "actor.update",
		Description: "Correct an actor's display name or email, or give an email to a person registered without one, " +
			"so that they can sign in with a password. What is left out stays as it is. A change of email " +
			"withdraws an invitation waiting (actor.invite): it went to the old one.",
		Kind: tool.Write, Gate: admins,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/actors/{actor_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ActorUpdateIn) (tool.Target, error) {
			return resolveActor(ctx, q, ActorIDIn{ActorID: in.ActorID})
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActorUpdateIn) (ActorView, error) {
			if in.DisplayName == nil && in.Email == nil {
				return ActorView{}, apperr.Invalid("give display_name, email or both")
			}
			if in.DisplayName != nil && strings.TrimSpace(*in.DisplayName) == "" {
				return ActorView{}, apperr.Invalid("display_name cannot be empty")
			}
			if in.Email != nil {
				email := strings.TrimSpace(*in.Email)
				if email == "" {
					return ActorView{}, apperr.Invalid("email cannot be empty; it can be changed, not removed")
				}
				in.Email = &email
			}
			// An administrator's own name and email are theirs to correct;
			// anyone else's are held to the rule for acting on them.
			if in.ActorID != ec.Actor.ID {
				if err := mayActOn(ctx, ec, in.ActorID); err != nil {
					return ActorView{}, err
				}
			}
			before, err := ec.Q.GetActor(ctx, in.ActorID)
			if err != nil {
				return ActorView{}, err
			}
			if in.Email != nil {
				if taken, err := ec.Q.EmailTakenByAnother(ctx, dbq.EmailTakenByAnotherParams{Email: *in.Email, ID: in.ActorID}); err != nil {
					return ActorView{}, err
				} else if taken {
					return ActorView{}, apperr.Conflicts("that email already belongs to an actor")
				}
			}
			if err := ec.Q.UpdateActor(ctx, dbq.UpdateActorParams{ID: in.ActorID, DisplayName: in.DisplayName, Email: in.Email}); err != nil {
				return ActorView{}, err
			}
			// An invitation waiting went to the email as it was. Once that
			// changes, it may have gone to the wrong person: it is withdrawn,
			// and inviting again sends one that is good.
			if in.Email != nil && (before.Email == nil || !strings.EqualFold(*before.Email, *in.Email)) {
				if err := ec.Q.RevokeInvites(ctx, dbq.RevokeInvitesParams{ActorID: in.ActorID, RevokedAt: &ec.Now}); err != nil {
					return ActorView{}, err
				}
			}
			ec.Emit(events.Event{Type: EventActorUpdated, SubjectType: "actor", SubjectID: &in.ActorID})
			a, err := ec.Q.GetActorView(ctx, in.ActorID)
			if err != nil {
				return ActorView{}, err
			}
			return viewActor(a), nil
		},
	})
}

// mayActOn: an admin manages ordinary actors; only root touches another
// holder of a platform role. Nobody suspends themselves into a lockout.
func mayActOn(ctx context.Context, ec *tool.ExecCtx, target uuid.UUID) error {
	if target == ec.Actor.ID {
		return apperr.Forbid("not on your own account")
	}
	return mayReach(ctx, ec.Q, ec.Actor, target)
}

// mayReach is mayActOn without the rule about oneself, for a read that shows
// what only acting on the actor would need: only root reaches another holder
// of a platform role, and nobody the system actor.
func mayReach(ctx context.Context, q *dbq.Queries, caller domain.Actor, target uuid.UUID) error {
	a, err := q.GetActor(ctx, target)
	if err != nil {
		return err
	}
	if a.PlatformRole != nil && caller.PlatformRole != domain.PlatformRoot {
		return apperr.Forbid("only root acts on an actor who holds a platform role")
	}
	if a.Kind == "system" {
		return apperr.Forbid("the system actor is not managed this way")
	}
	return nil
}

func actorSetStatus(name, desc, path, status, event string) tool.Tool {
	return tool.Define(tool.Spec[ActorIDIn, OK]{
		Name: name, Description: desc, Kind: tool.Write, Gate: admins,
		HTTP:    tool.Route{Method: "POST", Pattern: path},
		Resolve: resolveActor,
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActorIDIn) (OK, error) {
			if err := mayActOn(ctx, ec, in.ActorID); err != nil {
				return OK{}, err
			}
			n, err := ec.Q.SetActorStatus(ctx, dbq.SetActorStatusParams{ID: in.ActorID, Status: status})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the actor is already %s", status)
			}
			ec.Emit(events.Event{Type: event, SubjectType: "actor", SubjectID: &in.ActorID})
			return OK{OK: true}, nil
		},
	})
}

func actorSuspend() tool.Tool {
	return actorSetStatus("actor.suspend",
		"Suspend an actor everywhere at once: every call it makes from now on is denied, in every course, "+
			"and it cannot sign in. Its memberships, history and credentials are kept.",
		"/v1/actors/{actor_id}/suspend", domain.ActorSuspended, EventActorSuspended)
}

func actorReactivate() tool.Tool {
	return actorSetStatus("actor.reactivate",
		"Lift a suspension. The actor's memberships and credentials work again as they were.",
		"/v1/actors/{actor_id}/reactivate", domain.ActorActive, EventActorReactivated)
}

type ActorIssueTokenIn struct {
	ActorID       uuid.UUID `json:"actor_id"`
	Label         string    `json:"label"`
	ExpiresInDays *int      `json:"expires_in_days,omitempty"`
}

func actorIssueToken() tool.Tool {
	return tool.Define(tool.Spec[ActorIssueTokenIn, IssueTokenOut]{
		Name: "actor.issue_token",
		Description: "Issue an API token for another actor — how a newly registered agent gets its first credential, " +
			"since it cannot sign in to ask for one. The token is returned once and only its hash is kept.",
		Kind: tool.Write, Gate: admins,
		HTTP:      tool.Route{Method: "POST", Pattern: "/v1/actors/{actor_id}/tokens"},
		SecretOut: []string{"token"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ActorIssueTokenIn) (tool.Target, error) {
			return resolveActor(ctx, q, ActorIDIn{ActorID: in.ActorID})
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActorIssueTokenIn) (IssueTokenOut, error) {
			if in.ActorID != ec.Actor.ID {
				if err := mayActOn(ctx, ec, in.ActorID); err != nil {
					return IssueTokenOut{}, err
				}
			}
			expires, err := tokenExpiry(in.Label, in.ExpiresInDays, ec.Now)
			if err != nil {
				return IssueTokenOut{}, err
			}
			tok, id, err := auth.IssueToken(ctx, ec.Q, in.ActorID, &ec.Actor.ID, in.Label, expires, ec.Now)
			if err != nil {
				return IssueTokenOut{}, err
			}
			return IssueTokenOut{CredentialID: id, Token: tok.Full, TokenPrefix: tok.Prefix, ExpiresAt: expires}, nil
		},
	})
}

func actorListCredentials() tool.Tool {
	return tool.Define(tool.Spec[ActorIDIn, CredentialListOut]{
		Name: "actor.list_credentials",
		Description: "An actor's credentials, newest first: tokens with their label, prefix, issuer, expiry and last use, " +
			"sessions, invitations, password and linked identities, revoked ones included. Secrets are never shown. " +
			"Revoke one, a pending invitation included, with actor.revoke_credential. Held to the rule for acting on " +
			"the actor: only root lists the credentials of another holder of a platform role.",
		Kind: tool.Read, Gate: admins,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/actors/{actor_id}/credentials"},
		Resolve: resolveActor,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActorIDIn) (CredentialListOut, error) {
			// What is listed is there to be revoked, so it is shown to those
			// who could revoke it; one's own included.
			if in.ActorID != rc.Actor.ID {
				if err := mayReach(ctx, rc.Q, rc.Actor, in.ActorID); err != nil {
					return CredentialListOut{}, err
				}
			}
			rows, err := rc.Q.ListCredentialsForActor(ctx, in.ActorID)
			return viewCredentials(rows), err
		},
	})
}

type ActorRevokeCredentialIn struct {
	ActorID      uuid.UUID `json:"actor_id"`
	CredentialID uuid.UUID `json:"credential_id"`
}

func actorRevokeCredential() tool.Tool {
	return tool.Define(tool.Spec[ActorRevokeCredentialIn, OK]{
		Name: "actor.revoke_credential",
		Description: "Revoke one of an actor's credentials — a token that has leaked, a session left signed in — without " +
			"suspending the actor: everything else it holds keeps working. It takes effect on the credential's next use.",
		Kind: tool.Write, Gate: admins,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/actors/{actor_id}/credentials/{credential_id}/revoke"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ActorRevokeCredentialIn) (tool.Target, error) {
			return resolveActor(ctx, q, ActorIDIn{ActorID: in.ActorID})
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActorRevokeCredentialIn) (OK, error) {
			// Taking a credential away is held to the rule for issuing one; an
			// administrator's own are theirs, as credential.revoke has them.
			if in.ActorID != ec.Actor.ID {
				if err := mayActOn(ctx, ec, in.ActorID); err != nil {
					return OK{}, err
				}
			}
			n, err := ec.Q.RevokeCredential(ctx, dbq.RevokeCredentialParams{ID: in.CredentialID, ActorID: in.ActorID, RevokedAt: &ec.Now})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Missing("no such live credential on this actor")
			}
			ec.Emit(events.Event{Type: EventActorCredentialRevoked, SubjectType: "actor", SubjectID: &in.ActorID,
				Payload: map[string]any{"credential_id": in.CredentialID}})
			return OK{OK: true}, nil
		},
	})
}

type ActorInviteIn struct {
	ActorID       uuid.UUID `json:"actor_id"`
	ExpiresInDays *int      `json:"expires_in_days,omitempty" jsonschema:"default 7, at most 30"`
}

type ActorInviteOut struct {
	Token     string    `json:"token" jsonschema:"what the invitation link carries; shown once, and a replay of this call comes back without it"`
	Email     string    `json:"email" jsonschema:"what the person will sign in with"`
	ExpiresAt time.Time `json:"expires_at"`
}

const (
	defaultInviteDays = 7
	maxInviteDays     = 30
)

func actorInvite() tool.Tool {
	return tool.Define(tool.Spec[ActorInviteIn, ActorInviteOut]{
		Name: "actor.invite",
		Description: "Invite a registered person to choose their password. The token is for the front end's page " +
			"that takes invitations; the person opens it, chooses a password there (POST /v1/auth/invite) and is signed in. " +
			"It works once, until it expires, and only the newest invitation works: inviting again replaces it. " +
			"Taken up by someone who has a password already, it replaces that password. It is withdrawn when the " +
			"person sets a password some other way, and when their email changes. " +
			"The person needs an email, which is what they will sign in with (actor.update gives one). " +
			"An agent is given a token instead (actor.issue_token).",
		Kind: tool.Write, Gate: admins,
		HTTP:      tool.Route{Method: "POST", Pattern: "/v1/actors/{actor_id}/invite"},
		SecretOut: []string{"token"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ActorInviteIn) (tool.Target, error) {
			return resolveActor(ctx, q, ActorIDIn{ActorID: in.ActorID})
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActorInviteIn) (ActorInviteOut, error) {
			// Setting someone's password is taking over their account, so it
			// is held to the rule for issuing them a token; and nobody
			// invites themselves, who can set their own password.
			if err := mayActOn(ctx, ec, in.ActorID); err != nil {
				return ActorInviteOut{}, err
			}
			days := defaultInviteDays
			if in.ExpiresInDays != nil {
				days = *in.ExpiresInDays
			}
			if days < 1 || days > maxInviteDays {
				return ActorInviteOut{}, apperr.Invalid("expires_in_days must be between 1 and %d", maxInviteDays)
			}
			a, err := ec.Q.GetActor(ctx, in.ActorID)
			if err != nil {
				return ActorInviteOut{}, err
			}
			// An agent needs no password: it is given a token. That is the
			// front end's to steer by, not a rule here, which reads the email
			// the actor would sign in with, and not their kind.
			switch {
			case a.Email == nil:
				return ActorInviteOut{}, apperr.Precondition("the actor has no email to sign in with: give them one with actor.update first")
			case a.Status != domain.ActorActive:
				return ActorInviteOut{}, apperr.Precondition("the actor is suspended: reactivate them first")
			}
			expires := ec.Now.AddDate(0, 0, days)
			tok, _, err := auth.IssueInvite(ctx, ec.Q, in.ActorID, "invited by "+ec.Actor.DisplayName, expires, ec.Now)
			if err != nil {
				return ActorInviteOut{}, err
			}
			ec.Emit(events.Event{Type: EventActorInvited, SubjectType: "actor", SubjectID: &in.ActorID})
			return ActorInviteOut{Token: tok.Full, Email: *a.Email, ExpiresAt: expires}, nil
		},
	})
}

type ActorLinkSSOIn struct {
	ActorID  uuid.UUID `json:"actor_id"`
	Provider string    `json:"provider" jsonschema:"this installation's name for the identity provider, e.g. polyu-adfs"`
	Subject  string    `json:"subject" jsonschema:"the account at the provider: for ADFS, the UPN, e.g. yuki@connect.polyu.hk"`
}

type CredentialIDOut struct {
	CredentialID uuid.UUID `json:"credential_id"`
}

func actorLinkSSO() tool.Tool {
	return tool.Define(tool.Spec[ActorLinkSSOIn, CredentialIDOut]{
		Name: "actor.link_sso",
		Description: "Let a registered person sign in through the identity provider, by linking their account there to their " +
			"actor here. Accounts are not created on first sign-in: until this is done, someone the provider vouches for is " +
			"still nobody here. One identity links to one actor.",
		Kind: tool.Write, Gate: admins,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/actors/{actor_id}/sso"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ActorLinkSSOIn) (tool.Target, error) {
			return resolveActor(ctx, q, ActorIDIn{ActorID: in.ActorID})
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ActorLinkSSOIn) (CredentialIDOut, error) {
			provider, subject := strings.TrimSpace(in.Provider), auth.NormalizeSubject(in.Subject)
			if provider == "" || subject == "" {
				return CredentialIDOut{}, apperr.Invalid("provider and subject are required")
			}
			// Linking an identity is handing over the keys to the account, so
			// it is held to the same rule as issuing a token for it.
			if in.ActorID != ec.Actor.ID {
				if err := mayActOn(ctx, ec, in.ActorID); err != nil {
					return CredentialIDOut{}, err
				}
			}
			// One identity, one row, for good: the database keeps (provider,
			// subject) unique across revoked links as well, so that an
			// identity which once opened one account can never quietly come
			// to open another. Linking it again to the same actor revives
			// the row.
			switch held, err := ec.Q.GetSSOCredential(ctx, dbq.GetSSOCredentialParams{Provider: &provider, Subject: &subject}); {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return CredentialIDOut{}, err
			case held.ActorID == in.ActorID && held.RevokedAt != nil:
				return CredentialIDOut{CredentialID: held.ID}, ec.Q.ReviveSSOCredential(ctx, held.ID)
			case held.ActorID == in.ActorID:
				return CredentialIDOut{}, apperr.Conflicts("that identity is already linked to this actor")
			default:
				return CredentialIDOut{}, apperr.Conflicts("that identity is, or once was, linked to another actor; identities are not reassigned")
			}
			id := ids.New()
			label := "linked by " + ec.Actor.DisplayName
			return CredentialIDOut{CredentialID: id}, ec.Q.InsertCredential(ctx, dbq.InsertCredentialParams{
				ID: id, ActorID: in.ActorID, Kind: auth.KindSSO, Provider: &provider, Subject: &subject, Label: &label, CreatedAt: ec.Now})
		},
	})
}

func tokenExpiry(label string, days *int, now time.Time) (*time.Time, error) {
	if strings.TrimSpace(label) == "" {
		return nil, apperr.Invalid("label is required")
	}
	if days == nil {
		return nil, nil
	}
	if *days < 1 || *days > 3650 {
		return nil, apperr.Invalid("expires_in_days must be between 1 and 3650")
	}
	t := now.AddDate(0, 0, *days)
	return &t, nil
}

// ---------------------------------------------------------------------------
// term.*, department.*
// ---------------------------------------------------------------------------

type TermCreateIn struct {
	Name     string `json:"name"`
	StartsOn string `json:"starts_on" jsonschema:"YYYY-MM-DD"`
	EndsOn   string `json:"ends_on" jsonschema:"YYYY-MM-DD"`
}

type IDOut struct {
	ID uuid.UUID `json:"id"`
}

func termCreate() tool.Tool {
	return tool.Define(tool.Spec[TermCreateIn, IDOut]{
		Name:        "term.create",
		Description: "Create a term: a named span of dates that course offerings belong to.",
		Kind:        tool.Write, Gate: admins,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/terms"},
		Resolve: noTarget[TermCreateIn]("term"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in TermCreateIn) (IDOut, error) {
			start, err1 := time.Parse(time.DateOnly, in.StartsOn)
			end, err2 := time.Parse(time.DateOnly, in.EndsOn)
			switch {
			case strings.TrimSpace(in.Name) == "":
				return IDOut{}, apperr.Invalid("name is required")
			case err1 != nil || err2 != nil:
				return IDOut{}, apperr.Invalid("starts_on and ends_on are dates, YYYY-MM-DD")
			case end.Before(start):
				return IDOut{}, apperr.Invalid("a term cannot end before it starts")
			}
			id := ids.New()
			return IDOut{ID: id}, ec.Q.InsertTerm(ctx, dbq.InsertTermParams{ID: id, Name: in.Name, StartsOn: start, EndsOn: end})
		},
	})
}

type TermView struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	StartsOn string    `json:"starts_on"`
	EndsOn   string    `json:"ends_on"`
}

type TermListOut struct {
	Terms []TermView `json:"terms"`
}

func termList() tool.Tool {
	return tool.Define(tool.Spec[Empty, TermListOut]{
		Name:        "term.list",
		Description: "Every term, most recent first. Any signed-in actor may read this.",
		Kind:        tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/terms"},
		Resolve: noTarget[Empty]("term"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ Empty) (TermListOut, error) {
			rows, err := rc.Q.ListTerms(ctx)
			out := TermListOut{Terms: make([]TermView, 0, len(rows))}
			for _, r := range rows {
				out.Terms = append(out.Terms, TermView{ID: r.ID, Name: r.Name,
					StartsOn: r.StartsOn.Format(time.DateOnly), EndsOn: r.EndsOn.Format(time.DateOnly)})
			}
			return out, err
		},
	})
}

type NameIn struct {
	Name string `json:"name"`
}

func departmentCreate() tool.Tool {
	return tool.Define(tool.Spec[NameIn, IDOut]{
		Name:        "department.create",
		Description: "Create a department. Departments group courses and may define their own permission presets; they play no part in authorization.",
		Kind:        tool.Write, Gate: admins,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/departments"},
		Resolve: noTarget[NameIn]("department"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in NameIn) (IDOut, error) {
			if strings.TrimSpace(in.Name) == "" {
				return IDOut{}, apperr.Invalid("name is required")
			}
			id := ids.New()
			return IDOut{ID: id}, ec.Q.InsertDepartment(ctx, dbq.InsertDepartmentParams{ID: id, Name: in.Name, CreatedAt: ec.Now})
		},
	})
}

type DepartmentView struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type DepartmentListOut struct {
	Departments []DepartmentView `json:"departments"`
}

func departmentList() tool.Tool {
	return tool.Define(tool.Spec[Empty, DepartmentListOut]{
		Name:        "department.list",
		Description: "Every department, by name. Any signed-in actor may read this.",
		Kind:        tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/departments"},
		Resolve: noTarget[Empty]("department"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ Empty) (DepartmentListOut, error) {
			rows, err := rc.Q.ListDepartments(ctx)
			out := DepartmentListOut{Departments: make([]DepartmentView, 0, len(rows))}
			for _, r := range rows {
				out.Departments = append(out.Departments, DepartmentView{ID: r.ID, Name: r.Name})
			}
			return out, err
		},
	})
}

// ---------------------------------------------------------------------------
// preset.*
// ---------------------------------------------------------------------------

type PresetView struct {
	ID              uuid.UUID  `json:"id"`
	DeptID          *uuid.UUID `json:"dept_id,omitempty" jsonschema:"absent for a built-in"`
	Name            string     `json:"name"`
	Description     *string    `json:"description,omitempty"`
	Role            string     `json:"role"`
	StudentScope    string     `json:"student_scope"`
	AssignmentScope string     `json:"assignment_scope"`
	Perms           PermLevels `json:"perms"`
}

func viewPreset(p dbq.PermissionPreset) PresetView {
	return PresetView{ID: p.ID, DeptID: p.DeptID, Name: p.Name, Description: p.Description, Role: p.Role,
		StudentScope: p.StudentScope, AssignmentScope: p.AssignmentScope, Perms: presetPerms(p).view()}
}

type PresetListIn struct {
	DeptID *uuid.UUID `json:"dept_id,omitempty" jsonschema:"also list this department's own presets"`
}

type PresetListOut struct {
	Presets []PresetView `json:"presets"`
}

func presetList() tool.Tool {
	return tool.Define(tool.Spec[PresetListIn, PresetListOut]{
		Name: "preset.list",
		Description: "Permission presets: the six built-ins, and a department's own when dept_id is given. " +
			"A preset is a starting point copied onto a member when they are added; editing one changes nobody already seated.",
		Kind: tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/presets"},
		Resolve: noTarget[PresetListIn]("permission_preset"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in PresetListIn) (PresetListOut, error) {
			rows, err := rc.Q.ListPresets(ctx, in.DeptID)
			out := PresetListOut{Presets: make([]PresetView, 0, len(rows))}
			for _, r := range rows {
				out.Presets = append(out.Presets, viewPreset(r))
			}
			return out, err
		},
	})
}

type PresetBody struct {
	Description     *string    `json:"description,omitempty"`
	Role            string     `json:"role" jsonschema:"the roster role members get by default: student, instructor, ta, observer or assistant"`
	StudentScope    string     `json:"student_scope" jsonschema:"all or listed"`
	AssignmentScope string     `json:"assignment_scope" jsonschema:"all or listed"`
	Perms           PermLevels `json:"perms" jsonschema:"permission name to level; anything not named is denied"`
}

func (b PresetBody) check() (permSet, error) {
	if !validRoles[b.Role] {
		return nil, apperr.Invalid("role must be student, instructor, ta, observer or assistant")
	}
	if !validScope(b.StudentScope) || !validScope(b.AssignmentScope) {
		return nil, apperr.Invalid("student_scope and assignment_scope are each all or listed")
	}
	ps := permSet{}
	return ps, ps.apply(b.Perms)
}

type PresetCreateIn struct {
	DeptID uuid.UUID `json:"dept_id" jsonschema:"the department the preset belongs to; built-ins are not created here"`
	Name   string    `json:"name"`
	PresetBody
}

func presetCreate() tool.Tool {
	return tool.Define(tool.Spec[PresetCreateIn, IDOut]{
		Name:        "preset.create",
		Description: "Define a permission preset for a department, alongside the built-ins. It may share a built-in's name.",
		Kind:        tool.Write, Gate: admins,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/presets"},
		Resolve: noTarget[PresetCreateIn]("permission_preset"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in PresetCreateIn) (IDOut, error) {
			ps, err := in.check()
			if err != nil {
				return IDOut{}, err
			}
			if strings.TrimSpace(in.Name) == "" {
				return IDOut{}, apperr.Invalid("name is required")
			}
			if ok, err := ec.Q.DepartmentExists(ctx, in.DeptID); err != nil {
				return IDOut{}, err
			} else if !ok {
				return IDOut{}, apperr.Missing("no such department")
			}
			if _, err := ec.Q.GetDeptPresetByName(ctx, dbq.GetDeptPresetByNameParams{Name: in.Name, DeptID: &in.DeptID}); err == nil {
				return IDOut{}, apperr.Conflicts("the department already has a preset named %q", in.Name)
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return IDOut{}, err
			}
			id := ids.New()
			return IDOut{ID: id}, ec.Q.InsertPreset(ctx, dbq.InsertPresetParams{
				ID: id, DeptID: &in.DeptID, Name: in.Name, Description: in.Description, Role: in.Role,
				StudentScope: in.StudentScope, AssignmentScope: in.AssignmentScope,
				PermDocumentRead: ps.col(domain.PermDocumentRead), PermDocumentReadDraft: ps.col(domain.PermDocumentReadDraft),
				PermDocumentWrite: ps.col(domain.PermDocumentWrite), PermRubricRead: ps.col(domain.PermRubricRead),
				PermAssignmentWrite: ps.col(domain.PermAssignmentWrite), PermSubmissionRead: ps.col(domain.PermSubmissionRead),
				PermSubmissionWrite: ps.col(domain.PermSubmissionWrite), PermGradeRead: ps.col(domain.PermGradeRead),
				PermGradeSubmit: ps.col(domain.PermGradeSubmit), PermGradePost: ps.col(domain.PermGradePost),
				PermMemberRead: ps.col(domain.PermMemberRead), PermMemberManage: ps.col(domain.PermMemberManage),
				PermActionDecide: ps.col(domain.PermActionDecide),
				CreatedByActorID: &ec.Actor.ID, CreatedAt: ec.Now,
			})
		},
	})
}

type PresetUpdateIn struct {
	PresetID uuid.UUID `json:"preset_id"`
	PresetBody
}

func presetUpdate() tool.Tool {
	return tool.Define(tool.Spec[PresetUpdateIn, OK]{
		Name: "preset.update",
		Description: "Replace a department preset's role, scope and permissions. Members already seated from it are " +
			"not affected: their permissions were copied when they were added. Built-ins are not edited here.",
		Kind: tool.Write, Gate: admins,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/presets/{preset_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in PresetUpdateIn) (tool.Target, error) {
			if _, err := q.GetPreset(ctx, in.PresetID); errors.Is(err, pgx.ErrNoRows) {
				return tool.Target{}, apperr.Missing("no such preset")
			} else if err != nil {
				return tool.Target{}, err
			}
			return tool.Target{Type: "permission_preset", ID: &in.PresetID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in PresetUpdateIn) (OK, error) {
			ps, err := in.check()
			if err != nil {
				return OK{}, err
			}
			n, err := ec.Q.UpdatePreset(ctx, dbq.UpdatePresetParams{
				ID: in.PresetID, Description: in.Description, Role: in.Role,
				StudentScope: in.StudentScope, AssignmentScope: in.AssignmentScope,
				PermDocumentRead: ps.col(domain.PermDocumentRead), PermDocumentReadDraft: ps.col(domain.PermDocumentReadDraft),
				PermDocumentWrite: ps.col(domain.PermDocumentWrite), PermRubricRead: ps.col(domain.PermRubricRead),
				PermAssignmentWrite: ps.col(domain.PermAssignmentWrite), PermSubmissionRead: ps.col(domain.PermSubmissionRead),
				PermSubmissionWrite: ps.col(domain.PermSubmissionWrite), PermGradeRead: ps.col(domain.PermGradeRead),
				PermGradeSubmit: ps.col(domain.PermGradeSubmit), PermGradePost: ps.col(domain.PermGradePost),
				PermMemberRead: ps.col(domain.PermMemberRead), PermMemberManage: ps.col(domain.PermMemberManage),
				PermActionDecide: ps.col(domain.PermActionDecide),
			})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Precondition("built-in presets are part of the installation and are not edited through the API")
			}
			return OK{OK: true}, nil
		},
	})
}
