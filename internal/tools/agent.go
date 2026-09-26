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
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/members"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// A person may register agents of their own, and look after them: name
// them, give them tokens, suspend them, take them out of courses. An agent a
// person owns acts only as their delegate, seated by them in a course where
// they are seated (member.add_delegate), and never holds more than their
// seat there: owning agents gives nobody more than they hold. These tools act
// on the caller's own account, as me.* and credential.* do, and each one
// answers "no such agent of yours" for an actor the caller does not own,
// whether or not it exists.
//
// Whose an agent is, is looked at when the call runs, never when its target
// is resolved: Resolve does not know who is calling.

func agentTools(d Deps) []tool.Tool {
	return []tool.Tool{agentCreate(d), agentList(), agentGet(), agentUpdate(), agentSuspend(), agentReactivate(d),
		agentIssueToken(), agentListCredentials(), agentRevokeCredential(), agentWithdraw()}
}

// DefaultMaxAgentsPerOwner is how many agents that are not suspended one
// person may have, unless AGENT_MAX_PER_OWNER says otherwise.
const DefaultMaxAgentsPerOwner = 5

const EventAgentCreated = "agent.created"

// ownAgent returns the agent if the caller owns it, and errNotYourAgent if
// not, whether or not it exists.
func ownAgent(ctx context.Context, q dbq.Querier, owner, agent uuid.UUID) (dbq.Actor, error) {
	a, err := q.GetActor(ctx, agent)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (a.OwnerActorID == nil || *a.OwnerActorID != owner)) {
		return a, errNotYourAgent
	}
	return a, err
}

func agentTarget[In any](id func(In) uuid.UUID) func(context.Context, dbq.Querier, In) (tool.Target, error) {
	return func(_ context.Context, _ dbq.Querier, in In) (tool.Target, error) {
		a := id(in)
		return tool.Target{Type: "actor", ID: &a}, nil
	}
}

// withinAgentLimit refuses one more agent that is not suspended when the
// owner has as many as they may. The owner's row is locked first, so that
// two calls of theirs at once are counted one after the other.
func withinAgentLimit(ctx context.Context, q *dbq.Queries, owner uuid.UUID, most int) error {
	if err := q.LockOwnerForAgents(ctx, owner); err != nil {
		return err
	}
	n, err := q.CountActiveAgentsOf(ctx, &owner)
	if err != nil {
		return err
	}
	if n >= int64(most) {
		return apperr.Precondition("you have %d agents that are not suspended, the most one person may have; suspend one first", n).
			With("limit", most)
	}
	return nil
}

type AgentCreateIn struct {
	DisplayName string `json:"display_name" jsonschema:"what the agent is called wherever it appears"`
}

func agentCreate(d Deps) tool.Tool {
	return tool.Define(tool.Spec[AgentCreateIn, ActorOut]{
		Name: "agent.create",
		Description: "Register an agent of your own. It runs elsewhere, on whatever you connect to it with a token " +
			"(agent.issue_token); no endpoint, model or prompt is stored here. It can do nothing until you bring it into a " +
			"course where you are seated (member.add_delegate), and there it acts only as your delegate, never with more " +
			"than your own seat. A person may have a limited number of agents that are not suspended.",
		Kind: tool.Write, Gate: self,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/me/agents"},
		Resolve: noTarget[AgentCreateIn]("actor"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AgentCreateIn) (ActorOut, error) {
			if d.DisableAgentSelfService {
				return ActorOut{}, apperr.Forbid("this installation does not let people register agents of their own; an administrator does")
			}
			name := strings.TrimSpace(in.DisplayName)
			if name == "" {
				return ActorOut{}, apperr.Invalid("display_name is required")
			}
			me, err := ec.Q.GetActor(ctx, ec.Actor.ID)
			if err != nil {
				return ActorOut{}, err
			}
			// A refusal that reads kind, as the database's does: owning is
			// for people. Nothing that grants reads it.
			if me.Kind != "human" {
				return ActorOut{}, apperr.Forbid("an agent does not own agents; its owner does")
			}
			if err := withinAgentLimit(ctx, ec.Q, ec.Actor.ID, d.MaxAgentsPerOwner); err != nil {
				return ActorOut{}, err
			}
			id := ids.New()
			if err := ec.Q.InsertActor(ctx, dbq.InsertActorParams{
				ID: id, Kind: "agent", DisplayName: name, CreatedByActorID: &ec.Actor.ID, CreatedAt: ec.Now, OwnerActorID: &ec.Actor.ID,
			}); err != nil {
				return ActorOut{}, err
			}
			ec.Emit(events.Event{Type: EventAgentCreated, SubjectType: "actor", SubjectID: &id})
			return ActorOut{ActorID: id}, nil
		},
	})
}

// AgentView is one of the caller's agents as its owner sees it.
type AgentView struct {
	ActorID       uuid.UUID  `json:"actor_id"`
	DisplayName   string     `json:"display_name"`
	Status        string     `json:"status" jsonschema:"active or suspended"`
	SuspendedByMe bool       `json:"suspended_by_me" jsonschema:"suspended by you, and so yours to reactivate; a suspension an administrator made is theirs"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty" jsonschema:"when it last used a token that still works, to the minute; absent if never"`
}

type AgentSummary struct {
	AgentView
	LiveSeats       int64 `json:"live_seats" jsonschema:"courses where it is seated and its seat counts now"`
	PendingRequests int64 `json:"pending_requests" jsonschema:"requests to bring it into a course that wait for a decision"`
}

type AgentListOut struct {
	Agents []AgentSummary `json:"agents"`
}

func agentList() tool.Tool {
	return tool.Define(tool.Spec[Empty, AgentListOut]{
		Name:        "agent.list",
		Description: "Your own agents, oldest first: their standing, when each was last seen, how many courses each is seated in, and how many requests to seat one wait for a decision.",
		Kind:        tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/me/agents"},
		Resolve: noTarget[Empty]("actor"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ Empty) (AgentListOut, error) {
			rows, err := rc.Q.ListAgentsOf(ctx, dbq.ListAgentsOfParams{OwnerActorID: &rc.Actor.ID, Now: &rc.Now})
			out := AgentListOut{Agents: make([]AgentSummary, 0, len(rows))}
			for _, r := range rows {
				out.Agents = append(out.Agents, AgentSummary{
					AgentView:       agentView(rc.Actor.ID, r.ID, r.DisplayName, r.Status, r.SuspendedByActorID, r.CreatedAt, r.LastSeenAt),
					LiveSeats:       r.LiveSeats,
					PendingRequests: r.PendingRequests,
				})
			}
			return out, err
		},
	})
}

func agentView(owner, id uuid.UUID, name, status string, suspendedBy *uuid.UUID, created time.Time, lastSeen *time.Time) AgentView {
	return AgentView{ActorID: id, DisplayName: name, Status: status, CreatedAt: created, LastSeenAt: lastSeen,
		SuspendedByMe: status == domain.ActorSuspended && suspendedBy != nil && *suspendedBy == owner}
}

// AgentSeat is a seat one of the caller's agents holds.
type AgentSeat struct {
	MemberID          uuid.UUID  `json:"member_id"`
	CourseID          uuid.UUID  `json:"course_id"`
	Code              string     `json:"code"`
	Section           string     `json:"section"`
	Title             string     `json:"title"`
	CourseStatus      string     `json:"course_status"`
	Status            string     `json:"status" jsonschema:"active or paused"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	Preset            *string    `json:"preset,omitempty" jsonschema:"the preset its permissions were copied from"`
	PrincipalMemberID *uuid.UUID `json:"principal_member_id,omitempty" jsonschema:"your seat in the course, whose delegate it is"`
	Perms             PermLevels `json:"perms" jsonschema:"what it may do there now: its own levels, capped by yours; all denied while its seat or yours does not count"`
	StudentScope      string     `json:"student_scope"`
	AssignmentScope   string     `json:"assignment_scope"`
	AnswersCourse     bool       `json:"answers_course" jsonschema:"it answers the course, not you alone: the students it can see and do no more than may ask it too"`
}

type AgentRequest struct {
	ActionID  uuid.UUID `json:"action_id" jsonschema:"withdraw it with action.withdraw"`
	CourseID  uuid.UUID `json:"course_id"`
	Code      string    `json:"code"`
	Section   string    `json:"section"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

type AgentGetOut struct {
	AgentView
	Seats    []AgentSeat    `json:"seats"`
	Requests []AgentRequest `json:"requests" jsonschema:"requests to bring it into a course that wait for a decision"`
}

type AgentIDIn struct {
	ActorID uuid.UUID `json:"actor_id"`
}

func agentGet() tool.Tool {
	return tool.Define(tool.Spec[AgentIDIn, AgentGetOut]{
		Name: "agent.get",
		Description: "One of your agents: its standing, when it was last seen, the courses it is seated in with what it " +
			"may do in each, and the requests to seat it that wait for a decision.",
		Kind: tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/me/agents/{actor_id}"},
		Resolve: agentTarget(func(in AgentIDIn) uuid.UUID { return in.ActorID }),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in AgentIDIn) (AgentGetOut, error) {
			a, err := ownAgent(ctx, rc.Q, rc.Actor.ID, in.ActorID)
			if err != nil {
				return AgentGetOut{}, err
			}
			seen, err := rc.Q.AgentLastSeen(ctx, dbq.AgentLastSeenParams{ActorID: a.ID, Now: &rc.Now})
			if err != nil {
				return AgentGetOut{}, err
			}
			var lastSeen *time.Time
			if len(seen) > 0 {
				lastSeen = seen[0]
			}
			out := AgentGetOut{AgentView: agentView(rc.Actor.ID, a.ID, a.DisplayName, a.Status, a.SuspendedByActorID, a.CreatedAt, lastSeen),
				Seats: []AgentSeat{}, Requests: []AgentRequest{}}
			seats, err := rc.Q.ListSeatsOfActor(ctx, a.ID)
			if err != nil {
				return AgentGetOut{}, err
			}
			for _, s := range seats {
				m, err := authz.LoadMember(ctx, rc.Q, s.MemberID)
				if err != nil {
					return AgentGetOut{}, err
				}
				out.Seats = append(out.Seats, AgentSeat{MemberID: s.MemberID, CourseID: s.CourseID, Code: s.Code, Section: s.Section,
					Title: s.Title, CourseStatus: s.CourseStatus, Status: s.Status, ExpiresAt: s.ExpiresAt, Preset: s.PresetName,
					PrincipalMemberID: s.PrincipalMemberID, Perms: effectivePerms(m, rc.Now),
					StudentScope: s.StudentScope, AssignmentScope: s.AssignmentScope, AnswersCourse: s.AnswersCourse})
			}
			requests, err := rc.Q.ListDelegateRequestsFor(ctx, &a.ID)
			if err != nil {
				return AgentGetOut{}, err
			}
			for _, r := range requests {
				out.Requests = append(out.Requests, AgentRequest{ActionID: r.ID, CourseID: *r.CourseID, Code: r.Code,
					Section: r.Section, Title: r.Title, CreatedAt: r.CreatedAt})
			}
			return out, nil
		},
	})
}

type AgentUpdateIn struct {
	ActorID     uuid.UUID `json:"actor_id"`
	DisplayName string    `json:"display_name"`
}

func agentUpdate() tool.Tool {
	return tool.Define(tool.Spec[AgentUpdateIn, OK]{
		Name:        "agent.update",
		Description: "Rename one of your agents.",
		Kind:        tool.Write, Gate: self,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/me/agents/{actor_id}"},
		Resolve: agentTarget(func(in AgentUpdateIn) uuid.UUID { return in.ActorID }),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AgentUpdateIn) (OK, error) {
			if _, err := ownAgent(ctx, ec.Q, ec.Actor.ID, in.ActorID); err != nil {
				return OK{}, err
			}
			name := strings.TrimSpace(in.DisplayName)
			if name == "" {
				return OK{}, apperr.Invalid("display_name cannot be empty")
			}
			if err := ec.Q.UpdateActor(ctx, dbq.UpdateActorParams{ID: in.ActorID, DisplayName: &name}); err != nil {
				return OK{}, err
			}
			ec.Emit(events.Event{Type: EventActorUpdated, SubjectType: "actor", SubjectID: &in.ActorID})
			return OK{OK: true}, nil
		},
	})
}

func agentSuspend() tool.Tool {
	return tool.Define(tool.Spec[AgentIDIn, OK]{
		Name: "agent.suspend",
		Description: "Suspend one of your agents: every call it makes is denied, in every course, until you reactivate it. " +
			"Its seats, tokens and history are kept. An agent that is suspended does not count against how many you may have.",
		Kind: tool.Write, Gate: self,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/me/agents/{actor_id}/suspend"},
		Resolve: agentTarget(func(in AgentIDIn) uuid.UUID { return in.ActorID }),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AgentIDIn) (OK, error) {
			if _, err := ownAgent(ctx, ec.Q, ec.Actor.ID, in.ActorID); err != nil {
				return OK{}, err
			}
			n, err := ec.Q.SuspendAgentByOwner(ctx, dbq.SuspendAgentByOwnerParams{ID: in.ActorID, OwnerActorID: &ec.Actor.ID})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the agent is already suspended")
			}
			ec.Emit(events.Event{Type: EventActorSuspended, SubjectType: "actor", SubjectID: &in.ActorID})
			return OK{OK: true}, nil
		},
	})
}

func agentReactivate(d Deps) tool.Tool {
	return tool.Define(tool.Spec[AgentIDIn, OK]{
		Name: "agent.reactivate",
		Description: "Lift a suspension you made of one of your agents. A suspension an administrator made is theirs to " +
			"lift. It counts against how many agents you may have again, and is refused if you have as many already.",
		Kind: tool.Write, Gate: self,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/me/agents/{actor_id}/reactivate"},
		Resolve: agentTarget(func(in AgentIDIn) uuid.UUID { return in.ActorID }),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AgentIDIn) (OK, error) {
			a, err := ownAgent(ctx, ec.Q, ec.Actor.ID, in.ActorID)
			if err != nil {
				return OK{}, err
			}
			if a.Status == domain.ActorActive {
				return OK{}, apperr.Conflicts("the agent is not suspended")
			}
			// Suspending one and making another is not a way past the limit.
			if err := withinAgentLimit(ctx, ec.Q, ec.Actor.ID, d.MaxAgentsPerOwner); err != nil {
				return OK{}, err
			}
			n, err := ec.Q.ReactivateAgentByOwner(ctx, dbq.ReactivateAgentByOwnerParams{ID: in.ActorID, OwnerActorID: &ec.Actor.ID})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Forbid("the agent was suspended by an administrator, and only an administrator lifts that")
			}
			ec.Emit(events.Event{Type: EventActorReactivated, SubjectType: "actor", SubjectID: &in.ActorID})
			return OK{OK: true}, nil
		},
	})
}

type AgentIssueTokenIn struct {
	ActorID       uuid.UUID `json:"actor_id"`
	Label         string    `json:"label" jsonschema:"what this token is for, so it can be recognised later: where the agent runs"`
	ExpiresInDays *int      `json:"expires_in_days,omitempty" jsonschema:"omit for a token that does not expire"`
}

func agentIssueToken() tool.Tool {
	return tool.Define(tool.Spec[AgentIssueTokenIn, IssueTokenOut]{
		Name: "agent.issue_token",
		Description: "Issue an API token for one of your agents, for whatever runs it to connect with (MCP at /mcp, as a " +
			"bearer token). The token is returned once and only its hash is kept. Whoever holds it acts as the agent: as " +
			"your delegate, never with more than your own seat.",
		Kind: tool.Write, Gate: self,
		HTTP:      tool.Route{Method: "POST", Pattern: "/v1/me/agents/{actor_id}/tokens"},
		SecretOut: []string{"token"},
		Resolve:   agentTarget(func(in AgentIssueTokenIn) uuid.UUID { return in.ActorID }),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AgentIssueTokenIn) (IssueTokenOut, error) {
			if _, err := ownAgent(ctx, ec.Q, ec.Actor.ID, in.ActorID); err != nil {
				return IssueTokenOut{}, err
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

func agentListCredentials() tool.Tool {
	return tool.Define(tool.Spec[AgentIDIn, CredentialListOut]{
		Name:        "agent.list_credentials",
		Description: "One of your agents' tokens, newest first, with their label, prefix, issuer, expiry and last use, revoked ones included. Secrets are never shown.",
		Kind:        tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/me/agents/{actor_id}/credentials"},
		Resolve: agentTarget(func(in AgentIDIn) uuid.UUID { return in.ActorID }),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in AgentIDIn) (CredentialListOut, error) {
			if _, err := ownAgent(ctx, rc.Q, rc.Actor.ID, in.ActorID); err != nil {
				return CredentialListOut{}, err
			}
			rows, err := rc.Q.ListCredentialsForActor(ctx, in.ActorID)
			return viewCredentials(rows), err
		},
	})
}

type AgentRevokeCredentialIn struct {
	ActorID      uuid.UUID `json:"actor_id"`
	CredentialID uuid.UUID `json:"credential_id"`
}

func agentRevokeCredential() tool.Tool {
	return tool.Define(tool.Spec[AgentRevokeCredentialIn, OK]{
		Name: "agent.revoke_credential",
		Description: "Revoke one of your agents' tokens — one that has leaked, or a runtime you no longer use — without " +
			"suspending the agent. It takes effect on the token's next use.",
		Kind: tool.Write, Gate: self,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/me/agents/{actor_id}/credentials/{credential_id}/revoke"},
		Resolve: agentTarget(func(in AgentRevokeCredentialIn) uuid.UUID { return in.ActorID }),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AgentRevokeCredentialIn) (OK, error) {
			if _, err := ownAgent(ctx, ec.Q, ec.Actor.ID, in.ActorID); err != nil {
				return OK{}, err
			}
			n, err := ec.Q.RevokeCredential(ctx, dbq.RevokeCredentialParams{ID: in.CredentialID, ActorID: in.ActorID, RevokedAt: &ec.Now})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Missing("no such live credential on this agent")
			}
			ec.Emit(events.Event{Type: EventActorCredentialRevoked, SubjectType: "actor", SubjectID: &in.ActorID,
				Payload: map[string]any{"credential_id": in.CredentialID}})
			return OK{OK: true}, nil
		},
	})
}

type AgentWithdrawIn struct {
	ActorID uuid.UUID `json:"actor_id"`
	// Not tool.InCourse: the owner need hold no permission in the course
	// to take their agent out of it, only be its owner.
	CourseID uuid.UUID `json:"course_id" jsonschema:"the course to take the agent out of"`
}

func agentWithdraw() tool.Tool {
	return tool.Define(tool.Spec[AgentWithdrawIn, MemberRemoveOut]{
		Name: "agent.withdraw",
		Description: "Take one of your agents out of a course: its seat is removed, and whatever it had proposed that " +
			"nobody has decided is cancelled. Everything it did stays on record. Bringing it in again is a new seat: for " +
			"the agent, a fresh start. An archived course takes no changes, this one included.",
		Kind: tool.Write, Gate: self,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/me/agents/{actor_id}/courses/{course_id}/withdraw"},
		Resolve: func(ctx context.Context, q dbq.Querier, in AgentWithdrawIn) (tool.Target, error) {
			// The course is named, so an archived one refuses the call as it
			// refuses every write (pipeline.authorize).
			if _, err := q.GetCourse(ctx, in.CourseID); errors.Is(err, pgx.ErrNoRows) {
				return tool.Target{}, apperr.Missing("no such course")
			} else if err != nil {
				return tool.Target{}, err
			}
			return tool.Target{CourseID: in.CourseID, Type: "course_member"}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AgentWithdrawIn) (MemberRemoveOut, error) {
			if _, err := ownAgent(ctx, ec.Q, ec.Actor.ID, in.ActorID); err != nil {
				return MemberRemoveOut{}, err
			}
			live, err := ec.Q.GetLiveMembership(ctx, dbq.GetLiveMembershipParams{CourseID: in.CourseID, ActorID: in.ActorID})
			if errors.Is(err, pgx.ErrNoRows) {
				return MemberRemoveOut{}, apperr.Missing("the agent has no seat in this course")
			}
			if err != nil {
				return MemberRemoveOut{}, err
			}
			// Locked, as every removal locks the seat it removes, and looked
			// at again: it may have been removed meanwhile.
			locked, err := ec.Q.GetMemberForSweep(ctx, live.ID)
			if err != nil {
				return MemberRemoveOut{}, err
			}
			if locked.Status == domain.MemberRemoved {
				return MemberRemoveOut{}, apperr.Missing("the agent has no seat in this course")
			}
			n, err := members.Remove(ctx, ec.Q, ec.Emit, in.CourseID, live.ID, members.ReasonWithdrawn)
			return MemberRemoveOut{CancelledProposals: n}, err
		},
	})
}

// effectivePerms is what a seat may do now, as authorization would see it
// before scope: its own levels, capped by its principal's for a delegate,
// and every one denied while the seat, or its principal, does not count.
func effectivePerms(m *domain.Member, now time.Time) PermLevels {
	out := make(PermLevels, len(domain.AllPerms))
	usable := m.Live(now) && m.PrincipalLive(now)
	for _, p := range domain.AllPerms {
		l := domain.Denied
		if usable {
			l = m.Perm(p)
		}
		out[string(p)] = l.String()
	}
	return out
}
