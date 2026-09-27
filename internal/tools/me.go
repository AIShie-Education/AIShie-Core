package tools

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func meTools() []tool.Tool {
	return []tool.Tool{meGet(), meMemberships(), credentialList(), credentialIssueToken(), credentialSetPassword(), credentialRevoke()}
}

var self = tool.Gate{Self: true}

type Empty struct{}

func noTarget[In any](typ string) func(context.Context, dbq.Querier, In) (tool.Target, error) {
	return func(context.Context, dbq.Querier, In) (tool.Target, error) { return tool.Target{Type: typ}, nil }
}

// ---------------------------------------------------------------------------
// me.*
// ---------------------------------------------------------------------------

type MeOut struct {
	ID           uuid.UUID `json:"id"`
	Kind         string    `json:"kind" jsonschema:"human, agent or system; for display only"`
	DisplayName  string    `json:"display_name"`
	Email        *string   `json:"email,omitempty"`
	Status       string    `json:"status"`
	PlatformRole *string   `json:"platform_role,omitempty"`
}

func meGet() tool.Tool {
	return tool.Define(tool.Spec[Empty, MeOut]{
		Name:        "me.get",
		Description: "Who the caller is: the actor this credential belongs to.",
		Kind:        tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/me"},
		Resolve: noTarget[Empty]("actor"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ Empty) (MeOut, error) {
			a, err := rc.Q.GetActor(ctx, rc.Actor.ID)
			if err != nil {
				return MeOut{}, err
			}
			return MeOut{ID: a.ID, Kind: a.Kind, DisplayName: a.DisplayName, Email: a.Email, Status: a.Status, PlatformRole: a.PlatformRole}, nil
		},
	})
}

type Membership struct {
	MemberID        uuid.UUID  `json:"member_id" jsonschema:"the stable handle for this actor in this course; agents key their own memory on it"`
	CourseID        uuid.UUID  `json:"course_id"`
	Code            string     `json:"code"`
	Section         string     `json:"section"`
	Title           string     `json:"title"`
	CourseStatus    string     `json:"course_status"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	StudentScope    string     `json:"student_scope"`
	AssignmentScope string     `json:"assignment_scope"`
	// A member may always know their own seat, as authorization reads it.
	PrincipalMemberID *uuid.UUID `json:"principal_member_id,omitempty" jsonschema:"when you are someone's delegate, their seat in the course: you hold nothing they do not"`
	Perms             PermLevels `json:"perms" jsonschema:"what you may do in the course now, before scope: your own levels, capped by your principal's if you are a delegate; all denied while the seat does not count"`
	AnswersCourse     bool       `json:"answers_course" jsonschema:"when you are someone's delegate: true if you answer the course — other members may ask you, and you keep what each tells you from the others — false if you answer your principal alone"`
}

type MembershipsOut struct {
	Memberships []Membership `json:"memberships"`
}

func meMemberships() tool.Tool {
	return tool.Define(tool.Spec[Empty, MembershipsOut]{
		Name: "me.memberships",
		Description: "The courses the caller is seated in, with the member id for each and what the caller may do there. " +
			"An agent starting cold begins here: every other tool takes a course_id.",
		Kind: tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/me/memberships"},
		Resolve: noTarget[Empty]("course_member"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ Empty) (MembershipsOut, error) {
			rows, err := rc.Q.ListMembershipsForActor(ctx, rc.Actor.ID)
			if err != nil {
				return MembershipsOut{}, err
			}
			out := MembershipsOut{Memberships: make([]Membership, 0, len(rows))}
			for _, r := range rows {
				// One more lookup a seat: an actor holds few, and what each
				// allows is worked out as authorization works it out.
				m, err := authz.LoadMember(ctx, rc.Q, r.MemberID)
				if err != nil {
					return MembershipsOut{}, err
				}
				out.Memberships = append(out.Memberships, Membership{
					MemberID: r.MemberID, CourseID: r.CourseID, Code: r.Code, Section: r.Section, Title: r.Title,
					CourseStatus: r.CourseStatus, Role: r.Role, Status: r.Status, ExpiresAt: r.ExpiresAt,
					StudentScope: r.StudentScope, AssignmentScope: r.AssignmentScope,
					PrincipalMemberID: r.PrincipalMemberID, Perms: effectivePerms(m, rc.Now), AnswersCourse: m.AnswersOthers(),
				})
			}
			return out, nil
		},
	})
}

// ---------------------------------------------------------------------------
// credential.*
// ---------------------------------------------------------------------------

type CredentialView struct {
	ID          uuid.UUID  `json:"id"`
	Kind        string     `json:"kind"`
	Provider    *string    `json:"provider,omitempty"`
	Subject     *string    `json:"subject,omitempty"`
	TokenPrefix *string    `json:"token_prefix,omitempty" jsonschema:"the public part of a token, enough to tell which one it is"`
	Label       *string    `json:"label,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	IssuedByID  *uuid.UUID `json:"issued_by_actor_id,omitempty" jsonschema:"who issued a token: the actor themself or an administrator; absent for other kinds, for tokens made on the command line, and for tokens issued by a release before this field"`
	IssuedBy    *string    `json:"issued_by_name,omitempty" jsonschema:"the issuer's display name"`
}

func viewCredentials(rows []dbq.ListCredentialsForActorRow) CredentialListOut {
	out := CredentialListOut{Credentials: make([]CredentialView, 0, len(rows))}
	for _, r := range rows {
		out.Credentials = append(out.Credentials, CredentialView{
			ID: r.ID, Kind: r.Kind, Provider: r.Provider, Subject: r.Subject, TokenPrefix: r.TokenPrefix,
			Label: r.Label, LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt, CreatedAt: r.CreatedAt,
			IssuedByID: r.IssuedByActorID, IssuedBy: r.IssuedByName,
		})
	}
	return out
}

type CredentialListOut struct {
	Credentials []CredentialView `json:"credentials"`
}

func credentialList() tool.Tool {
	return tool.Define(tool.Spec[Empty, CredentialListOut]{
		Name:        "credential.list",
		Description: "The caller's own credentials: tokens, sessions, password, SSO identity. Secrets are never shown.",
		Kind:        tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/me/credentials"},
		Resolve: noTarget[Empty]("credential"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ Empty) (CredentialListOut, error) {
			rows, err := rc.Q.ListCredentialsForActor(ctx, rc.Actor.ID)
			return viewCredentials(rows), err
		},
	})
}

type IssueTokenIn struct {
	Label         string `json:"label" jsonschema:"what this token is for, so it can be recognised later"`
	ExpiresInDays *int   `json:"expires_in_days,omitempty" jsonschema:"omit for a token that does not expire"`
}

type IssueTokenOut struct {
	CredentialID uuid.UUID  `json:"credential_id"`
	Token        string     `json:"token" jsonschema:"shown once; it is not stored, and a replay of this call comes back without it"`
	TokenPrefix  string     `json:"token_prefix"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

func credentialIssueToken() tool.Tool {
	return tool.Define(tool.Spec[IssueTokenIn, IssueTokenOut]{
		Name: "credential.issue_token",
		Description: "Create an API token for the caller's own account. The token is returned once and only its " +
			"hash is kept: retrying this call returns the credential but not the token again.",
		Kind: tool.Write, Gate: self,
		HTTP:      tool.Route{Method: "POST", Pattern: "/v1/me/credentials/tokens"},
		SecretOut: []string{"token"},
		Resolve:   noTarget[IssueTokenIn]("credential"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in IssueTokenIn) (IssueTokenOut, error) {
			expires, err := tokenExpiry(in.Label, in.ExpiresInDays, ec.Now)
			if err != nil {
				return IssueTokenOut{}, err
			}
			tok, id, err := auth.IssueToken(ctx, ec.Q, ec.Actor.ID, &ec.Actor.ID, in.Label, expires, ec.Now)
			if err != nil {
				return IssueTokenOut{}, err
			}
			return IssueTokenOut{CredentialID: id, Token: tok.Full, TokenPrefix: tok.Prefix, ExpiresAt: expires}, nil
		},
	})
}

type SetPasswordIn struct {
	Password string `json:"password"`
}

type OK struct {
	OK bool `json:"ok"`
}

func credentialSetPassword() tool.Tool {
	return tool.Define(tool.Spec[SetPasswordIn, OK]{
		Name:        "credential.set_password",
		Description: "Set or replace the caller's own password. The previous password stops working at once.",
		Kind:        tool.Write, Gate: self,
		HTTP:     tool.Route{Method: "POST", Pattern: "/v1/me/password"},
		SecretIn: []string{"password"},
		Resolve:  noTarget[SetPasswordIn]("credential"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SetPasswordIn) (OK, error) {
			if err := auth.SetPassword(ctx, ec.Q, ec.Actor.ID, in.Password, ec.Now); err != nil {
				return OK{}, err
			}
			return OK{OK: true}, nil
		},
	})
}

type RevokeCredentialIn struct {
	CredentialID uuid.UUID `json:"credential_id"`
}

func credentialRevoke() tool.Tool {
	return tool.Define(tool.Spec[RevokeCredentialIn, OK]{
		Name: "credential.revoke",
		Description: "Revoke one of the caller's own credentials: a token stops working, a session is signed out. " +
			"It takes effect on the credential's next use.",
		Kind: tool.Write, Gate: self,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/me/credentials/{credential_id}/revoke"},
		Resolve: func(_ context.Context, _ dbq.Querier, in RevokeCredentialIn) (tool.Target, error) {
			return tool.Target{Type: "credential", ID: &in.CredentialID}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in RevokeCredentialIn) (OK, error) {
			n, err := ec.Q.RevokeCredential(ctx, dbq.RevokeCredentialParams{ID: in.CredentialID, ActorID: ec.Actor.ID, RevokedAt: &ec.Now})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				// Someone else's, unknown, or already revoked: all the same
				// to the caller.
				return OK{}, apperr.Missing("no such live credential on this account")
			}
			return OK{OK: true}, nil
		},
	})
}
