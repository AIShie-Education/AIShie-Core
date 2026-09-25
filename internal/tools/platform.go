package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
		actorRegister(), actorList(), actorGet(), actorSuspend(), actorReactivate(), actorIssueToken(), actorLinkSSO(),
		termCreate(), termList(), departmentCreate(), departmentList(),
		presetList(), presetCreate(), presetUpdate(),
	}
}

// admins gates every platform tool. What only root may do — make another
// admin, act on a holder of a platform role — is checked inside the tool,
// because it depends on the arguments and not on the tool.
var admins = tool.Gate{Platform: []string{domain.PlatformRoot, domain.PlatformAdmin}}

const (
	EventActorRegistered  = "actor.registered"
	EventActorSuspended   = "actor.suspended"
	EventActorReactivated = "actor.reactivated"
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
}

func viewActor(a dbq.Actor) ActorView {
	return ActorView{ID: a.ID, Kind: a.Kind, DisplayName: a.DisplayName, Email: a.Email, Status: a.Status,
		PlatformRole: a.PlatformRole, CreatedByActorID: a.CreatedByActorID, CreatedAt: a.CreatedAt}
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
		Description: "One actor's registration: who they are, their standing, and who registered them.",
		Kind:        tool.Read, Gate: admins,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/actors/{actor_id}"},
		Resolve: resolveActor,
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActorIDIn) (ActorView, error) {
			a, err := rc.Q.GetActor(ctx, in.ActorID)
			return viewActor(a), err
		},
	})
}

type ActorListIn struct {
	Q            *string `json:"q,omitempty" jsonschema:"part of a display name or an email address, matched in any case; % and _ mean themselves. At most 254 characters"`
	Kind         *string `json:"kind,omitempty" jsonschema:"human, agent or system"`
	Status       *string `json:"status,omitempty" jsonschema:"active or suspended"`
	PlatformRole *string `json:"platform_role,omitempty" jsonschema:"root, admin, or none for the actors who hold neither"`
	Page
}

type ActorListOut struct {
	Actors []ActorView `json:"actors"`
	Next   *uuid.UUID  `json:"next,omitempty"`
}

// maxActorQuery bounds q at the longest an email address can be: long
// enough that a pasted address is never refused, short enough that what is
// matched against every row stays a search term.
const maxActorQuery = 254

// likeLiteral escapes what LIKE reads as more than itself — its two
// wildcards, and the backslash that escapes them — so that a search for
// "50%" finds "50%" and not everything that starts with 50.
var likeLiteral = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (in ActorListIn) params() (dbq.ListActorsParams, error) {
	p := dbq.ListActorsParams{After: in.after(), Kind: in.Kind, Status: in.Status, PlatformRole: in.PlatformRole, MaxRows: in.limit()}
	switch {
	case in.Kind != nil && !slices.Contains([]string{"human", "agent", "system"}, *in.Kind):
		return p, apperr.Invalid("kind must be human, agent or system")
	case in.Status != nil && *in.Status != domain.ActorActive && *in.Status != domain.ActorSuspended:
		return p, apperr.Invalid("status must be active or suspended")
	case in.PlatformRole != nil && !slices.Contains([]string{domain.PlatformRoot, domain.PlatformAdmin, "none"}, *in.PlatformRole):
		return p, apperr.Invalid("platform_role must be root, admin or none")
	}
	if in.Q != nil {
		q := strings.TrimSpace(*in.Q)
		if utf8.RuneCountInString(q) > maxActorQuery {
			return p, apperr.Invalid("q is at most %d characters", maxActorQuery)
		}
		// A search box that has been emptied asks for everyone, as no q does.
		if q != "" {
			pattern := "%" + likeLiteral.Replace(q) + "%"
			p.Pattern = &pattern
		}
	}
	return p, nil
}

func actorList() tool.Tool {
	return tool.Define(tool.Spec[ActorListIn, ActorListOut]{
		Name: "actor.list",
		Description: "Every actor on the platform — people, agents and the system actor — in the order they were registered, " +
			"optionally narrowed by q (part of a name or an email address), kind, status or platform role. This is how " +
			"someone registered earlier is found again; for one actor by id, use actor.get.",
		Kind: tool.Read, Gate: admins,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/actors"},
		Resolve: noTarget[ActorListIn]("actor"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ActorListIn) (ActorListOut, error) {
			p, err := in.params()
			if err != nil {
				return ActorListOut{}, err
			}
			rows, err := rc.Q.ListActors(ctx, p)
			out := ActorListOut{Actors: make([]ActorView, 0, len(rows))}
			for _, r := range rows {
				out.Actors = append(out.Actors, viewActor(r))
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, err
		},
	})
}

// mayActOn: an admin manages ordinary actors; only root touches another
// holder of a platform role. Nobody suspends themselves into a lockout.
func mayActOn(ctx context.Context, ec *tool.ExecCtx, target uuid.UUID) error {
	if target == ec.Actor.ID {
		return apperr.Forbid("not on your own account")
	}
	a, err := ec.Q.GetActor(ctx, target)
	if err != nil {
		return err
	}
	if a.PlatformRole != nil && ec.Actor.PlatformRole != domain.PlatformRoot {
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
			tok, id, err := auth.IssueToken(ctx, ec.Q, in.ActorID, in.Label, expires, ec.Now)
			if err != nil {
				return IssueTokenOut{}, err
			}
			return IssueTokenOut{CredentialID: id, Token: tok.Full, TokenPrefix: tok.Prefix, ExpiresAt: expires}, nil
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
