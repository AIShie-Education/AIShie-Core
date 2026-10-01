package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

func meTools() []tool.Tool {
	return []tool.Tool{meGet(), meMemberships(), meSiteChat(), meConversations(), credentialList(), credentialIssueToken(),
		credentialSetPassword(), credentialRevoke()}
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
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind" jsonschema:"human, agent or system; for display only"`
	DisplayName string    `json:"display_name"`
	Email       *string   `json:"email,omitempty"`
	// Absent with no email, as it was before anyone's went unchecked.
	EmailVerified *bool `json:"email_verified,omitempty" jsonschema:"with an email: false when you gave it registering through a join link, and nobody has checked it since; Core sends no email"`
	// A person's other sign-in name: their student or staff number.
	LoginID         *string `json:"login_id,omitempty" jsonschema:"what you sign in with besides your email, if you have one: your student or staff number; only an administrator changes it"`
	LoginIDVerified *bool   `json:"login_id_verified,omitempty" jsonschema:"with a login ID: false when you gave it registering through a join link, and no administrator has set it since"`
	Status          string  `json:"status"`
	PlatformRole    *string `json:"platform_role,omitempty"`
	// An agent may always know who answers for it. A service that hosts the
	// agent compares this with the person who hands it the agent's token.
	// It never changes (docs/schema.md §2.1).
	OwnerActorID *uuid.UUID `json:"owner_actor_id,omitempty" jsonschema:"for an agent a person owns, that person's actor id, the same for as long as the agent exists; absent for a person, and for an agent nobody owns"`
	// How an agent is run, which never changes: what it may expect to be
	// asked, and where.
	Hosting *string `json:"hosting,omitempty" jsonschema:"for an agent, how it is run, for good: runtime, the site's own agent runtime runs you and people in the site ask you; mcp, your owner's own tools reach you over MCP, and nobody asks you in the site. Absent for a person"`
	// Absent for everyone who administers nothing, agents always among them,
	// so that their answer is what it was before there were departments'
	// administrators.
	Administers []Administered `json:"administers,omitempty" jsonschema:"the departments you are appointed to administer; you administer every department beneath them too (department.list_tree)"`
}

type Administered struct {
	DeptID        uuid.UUID `json:"dept_id"`
	Name          string    `json:"name"`
	AppointmentID uuid.UUID `json:"appointment_id"`
	AppointedAt   time.Time `json:"appointed_at"`
}

func meGet() tool.Tool {
	return tool.Define(tool.Spec[Empty, MeOut]{
		Name: "me.get",
		Description: "Who the caller is: the actor this credential belongs to, with the email and the login ID (a student " +
			"or staff number) a person signs in with; for an agent, how it is hosted (runtime or mcp) and, if a person owns " +
			"it, who; and for a department's administrator, the departments they are appointed to administer.",
		Kind: tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/me"},
		Resolve: noTarget[Empty]("actor"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ Empty) (MeOut, error) {
			a, err := rc.Q.GetActor(ctx, rc.Actor.ID)
			if err != nil {
				return MeOut{}, err
			}
			out := MeOut{ID: a.ID, Kind: a.Kind, DisplayName: a.DisplayName, Email: a.Email, LoginID: a.LoginID, Status: a.Status,
				PlatformRole: a.PlatformRole, OwnerActorID: a.OwnerActorID, Hosting: a.Hosting}
			if a.Email != nil {
				out.EmailVerified = &a.EmailVerified
			}
			if a.LoginID != nil {
				out.LoginIDVerified = &a.LoginIDVerified
			}
			if rc.Actor.Administers {
				rows, err := rc.Q.MyAppointments(ctx, rc.Actor.ID)
				if err != nil {
					return MeOut{}, err
				}
				for _, r := range rows {
					out.Administers = append(out.Administers, Administered{DeptID: r.DeptID, Name: r.Name,
						AppointmentID: r.ID, AppointedAt: r.AppointedAt})
				}
			}
			return out, nil
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
	// The most the seat may hold of each permission, whoever gives it.
	Ceilings
}

type MembershipsOut struct {
	Memberships []Membership `json:"memberships"`
}

func meMemberships() tool.Tool {
	return tool.Define(tool.Spec[Empty, MembershipsOut]{
		Name: "me.memberships",
		Description: "The courses the caller is seated in, with the member id for each and what the caller may do there, " +
			"and the most each seat may be given of each permission (perm_ceilings, with perm_ceiling_reasons where below " +
			"autonomous). An agent starting cold begins here: every other tool takes a course_id.",
		Kind: tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/me/memberships"},
		Resolve: noTarget[Empty]("course_member"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, _ Empty) (MembershipsOut, error) {
			rows, err := rc.Q.ListMembershipsForActor(ctx, rc.Actor.ID)
			if err != nil {
				return MembershipsOut{}, err
			}
			// Whether the caller is an agent, for its seats' ceilings: read to
			// limit, never to grant (domain.Ceiling).
			me, err := rc.Q.GetActor(ctx, rc.Actor.ID)
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
					Ceilings: seatCeilings(me.Kind, m),
				})
			}
			return out, nil
		},
	})
}

// ---------------------------------------------------------------------------
// me.site_chat
// ---------------------------------------------------------------------------

// Site chat is whether people in the site may ask an agent (docs/schema.md
// §2.8). It follows from how the agent is hosted, and nobody declares it: a
// runtime agent is asked while the site's agent runtime holds a live token
// for it (agent_runtime.issue_token), it is active and its owner too; an mcp
// agent never is (SiteChatOf). me.site_chat, with which a runtime used to
// declare it, is kept for one release, deprecated: the runtime's token may
// still call it, which changes nothing and says whether the agent is asked
// now; any other credential is refused, not_runtime_hosted.

type SiteChatIn struct {
	On bool `json:"on" jsonschema:"deprecated, and changes nothing either way: people in the site ask a runtime agent while the site's runtime hosts it"`
}

type SiteChatOut struct {
	SiteChat bool `json:"site_chat" jsonschema:"whether people in the site may ask you now: while the site's runtime hosts you, and you and your owner are active"`
}

var (
	errSiteChatNotAgent = apperr.Precondition("site chat is an agent's; a person asks in the site, and is asked nothing: conversations are with agents").
				With("reason", "not_an_agent")
	// errNotRuntimeHosted refuses what only a runtime agent, by the token
	// the site's runtime holds for it, may do or be given.
	errNotRuntimeHosted = apperr.Precondition("only a runtime agent is asked in the site, by the token the site's agent runtime "+
		"holds for it; an mcp agent, reached by its owner's own tools, is asked nothing there").With("reason", "not_runtime_hosted")
)

// siteChatOf is whether people in the site may ask each of the given actors
// now, by id, whether it is an agent, and how it is hosted: the rule is
// SQL's (SiteChatOf).
func siteChatOf(ctx context.Context, q dbq.Querier, now time.Time, actors []uuid.UUID) (map[uuid.UUID]dbq.SiteChatOfRow, error) {
	out := map[uuid.UUID]dbq.SiteChatOfRow{}
	if len(actors) == 0 {
		return out, nil
	}
	rows, err := q.SiteChatOf(ctx, dbq.SiteChatOfParams{Now: &now, ActorIds: actors})
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, err
}

func meSiteChat() tool.Tool {
	return tool.Define(tool.Spec[SiteChatIn, SiteChatOut]{
		Name: "me.site_chat",
		Description: "Deprecated: nothing is declared any more. People in the site ask a runtime agent while the site's " +
			"agent runtime hosts it, and never an mcp agent; me_get says which you are (hosting). Called with the token the " +
			"site's runtime holds for you, it changes nothing, on true or false, and says whether people may ask you now; " +
			"with any other credential it is refused (not_runtime_hosted).",
		Kind: tool.Write, Gate: self,
		HTTP:    tool.Route{Method: "POST", Pattern: "/v1/me/site-chat"},
		Resolve: noTarget[SiteChatIn]("actor"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SiteChatIn) (SiteChatOut, error) {
			me, err := ec.Q.GetActor(ctx, ec.Actor.ID)
			if err != nil {
				return SiteChatOut{}, err
			}
			// A refusal that reads kind, as the database's does: site chat
			// is an agent's. Nothing that grants reads it.
			if me.Kind != "agent" {
				return SiteChatOut{}, errSiteChatNotAgent
			}
			runtime, err := ec.Q.IsLiveRuntimeToken(ctx, dbq.IsLiveRuntimeTokenParams{CredentialID: ec.CredentialID, ActorID: me.ID,
				Now: &ec.Now})
			if err != nil {
				return SiteChatOut{}, err
			}
			if !runtime {
				return SiteChatOut{}, errNotRuntimeHosted
			}
			now, err := siteChatOf(ctx, ec.Q, ec.Now, []uuid.UUID{me.ID})
			return SiteChatOut{SiteChat: now[me.ID].SiteChat}, err
		},
	})
}

// ---------------------------------------------------------------------------
// me.conversations
// ---------------------------------------------------------------------------

// me.conversations is the caller's conversations as the one who asks, in
// every course at once, for a chat panel that is not a course's page: the
// newest activity first. What it lists is what conversation.list lists of
// the caller's own, as their opener, course by course: from each seat of
// theirs that counts now and may read there (document_read, as
// conversation.list is gated), whatever the course's status, since reading
// an archived course is allowed. A seat removed, paused, expired, or a
// delegate's whose principal no longer counts, lists nothing, as it may
// call nothing; a seat removed has its conversations closed besides.

type MyConversationsIn struct {
	CourseID *uuid.UUID `json:"course_id,omitempty" jsonschema:"only this course's"`
	After    *string    `json:"after,omitempty" jsonschema:"next, from the page before: the page after it"`
	Limit    int        `json:"limit,omitempty" jsonschema:"at most this many; default 50, maximum 100"`
}

type MyConversationCourse struct {
	CourseID uuid.UUID `json:"course_id"`
	Code     string    `json:"code"`
	Section  string    `json:"section"`
	Title    string    `json:"title"`
}

type MyConversationRespondent struct {
	MemberID    uuid.UUID `json:"member_id" jsonschema:"the agent's seat in the course"`
	ActorID     uuid.UUID `json:"actor_id" jsonschema:"the agent"`
	DisplayName string    `json:"display_name"`
	Kind        string    `json:"kind" jsonschema:"agent; human only for a conversation closed by migration 0018 (closed_reason conversations_are_with_agents), from when a person could be asked"`
}

type MyConversation struct {
	ConversationID uuid.UUID                `json:"conversation_id"`
	MemberID       uuid.UUID                `json:"member_id" jsonschema:"your seat in the course, which opened it"`
	Course         MyConversationCourse     `json:"course"`
	Respondent     MyConversationRespondent `json:"respondent" jsonschema:"the agent you asked"`
	Title          *string                  `json:"title,omitempty"`
	Status         string                   `json:"status" jsonschema:"open or closed"`
	State          string                   `json:"state" jsonschema:"as conversation.get says: awaiting_answer, reply_pending_approval, answered or closed"`
	ClosedReason   *string                  `json:"closed_reason,omitempty" jsonschema:"as conversation.get says"`
	CreatedAt      time.Time                `json:"created_at"`
	LastActivityAt time.Time                `json:"last_activity_at" jsonschema:"its last message, or its opening while it has none; the list's order, newest first"`
	Unread         bool                     `json:"unread" jsonschema:"whether the agent has written, and not retracted, anything since you last marked it read (conversation.mark_read)"`
	MayAsk         bool                     `json:"may_ask" jsonschema:"whether you may ask in its course now: your seat there holds conversation_ask, and the course is not archived. Whether this conversation takes another question is its state's, and its agent's (conversation.ask says why not)"`
}

type MyConversationsOut struct {
	Conversations []MyConversation `json:"conversations"`
	Next          *string          `json:"next,omitempty" jsonschema:"give it as after for the next page; absent on the last. A conversation that moves while you page, with a new message, moves to the top: read from the top again for the newest"`
}

const (
	defaultMyConversations = 50
	maxMyConversations     = 100
)

// myCursor is me.conversations' cursor: the last row's activity and id,
// which the next page reads on from, both descending. It is opaque to the
// caller.
func myCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + " " + id.String()))
}

func parseMyCursor(s string) (time.Time, uuid.UUID, error) {
	bad := apperr.Invalid("after is not a cursor this tool gave: give next from the page before").With("field", "after")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	at, id, ok := strings.Cut(string(raw), " ")
	if !ok {
		return time.Time{}, uuid.Nil, bad
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	return t, u, nil
}

// mySeat is a seat of the caller's that lists its conversations, with its
// course and whether it may ask there now.
type mySeat struct {
	course MyConversationCourse
	mayAsk bool
}

// mySeats is the caller's seats that may read in their courses now, as
// conversation.list would be let read in each (authz.Evaluate, document_read,
// the gate it borrows), in one course only if course is given.
func mySeats(ctx context.Context, rc *tool.ReadCtx, course *uuid.UUID) (map[uuid.UUID]mySeat, error) {
	rows, err := rc.Q.ListMembershipsForActor(ctx, rc.Actor.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if course == nil || r.CourseID == *course {
			ids = append(ids, r.MemberID)
		}
	}
	seats, err := authz.LoadMembers(ctx, rc.Q, ids)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]mySeat{}
	for _, r := range rows {
		m, ok := seats[r.MemberID]
		if !ok {
			continue
		}
		if !authz.Evaluate(rc.Actor, r.CourseStatus, m, []domain.Perm{domain.PermDocumentRead}, false, rc.Now).Level.Allowed() {
			continue
		}
		out[r.MemberID] = mySeat{
			course: MyConversationCourse{CourseID: r.CourseID, Code: r.Code, Section: r.Section, Title: r.Title},
			mayAsk: authz.Evaluate(rc.Actor, r.CourseStatus, m, []domain.Perm{domain.PermConversationAsk}, true, rc.Now).Level.Allowed(),
		}
	}
	return out, nil
}

func meConversations() tool.Tool {
	return tool.Define(tool.Spec[MyConversationsIn, MyConversationsOut]{
		Name: "me.conversations",
		Description: "Your conversations with agents, as the one who asked, in every course you are seated in (or in " +
			"course_id's), the newest activity first, for a chat panel: each with its course, its agent, its state as " +
			"conversation.get says it, when it was last active, whether the agent has written since you last read it " +
			"(unread; conversation.mark_read), and whether you may ask in its course now. It lists what conversation.list " +
			"lists of yours in each course: nothing from a seat that is removed, paused or expired. Page with after = the " +
			"next of the page before.",
		Kind: tool.Read, Gate: self,
		HTTP:    tool.Route{Method: "GET", Pattern: "/v1/me/conversations"},
		Resolve: noTarget[MyConversationsIn]("conversation"),
		Query: func(ctx context.Context, rc *tool.ReadCtx, in MyConversationsIn) (MyConversationsOut, error) {
			limit := defaultMyConversations
			if in.Limit > 0 {
				limit = min(in.Limit, maxMyConversations)
			}
			arg := dbq.ListMyConversationsParams{MaxRows: int32(limit)}
			if in.After != nil {
				at, id, err := parseMyCursor(*in.After)
				if err != nil {
					return MyConversationsOut{}, err
				}
				arg.AfterAt, arg.AfterID = &at, &id
			}
			out := MyConversationsOut{Conversations: []MyConversation{}}
			seats, err := mySeats(ctx, rc, in.CourseID)
			if err != nil || len(seats) == 0 {
				return out, err
			}
			for id := range seats {
				arg.MemberIds = append(arg.MemberIds, id)
			}
			rows, err := rc.Q.ListMyConversations(ctx, arg)
			if err != nil {
				return out, err
			}
			ids := make([]uuid.UUID, len(rows))
			for i, r := range rows {
				ids[i] = r.ID
			}
			views, err := conversationViews(ctx, rc.Q, rc.Now, ids)
			if err != nil {
				return out, err
			}
			byID := make(map[uuid.UUID]ConversationView, len(views))
			for _, v := range views {
				byID[v.ID] = v
			}
			unread, err := unreadAmong(ctx, rc.Q, arg.MemberIds, ids)
			if err != nil {
				return out, err
			}
			for _, r := range rows { // newest first, not the views' order
				v, ok := byID[r.ID]
				if !ok {
					continue
				}
				seat := seats[r.OpenerMemberID]
				out.Conversations = append(out.Conversations, MyConversation{
					ConversationID: r.ID, MemberID: r.OpenerMemberID, Course: seat.course,
					Respondent: MyConversationRespondent{MemberID: v.Respondent.MemberID, ActorID: v.respondentActor,
						DisplayName: v.Respondent.DisplayName, Kind: v.Respondent.Kind},
					Title: v.Title, Status: v.Status, State: v.State, ClosedReason: v.ClosedReason, CreatedAt: v.CreatedAt,
					LastActivityAt: r.LastActivityAt, Unread: unread[r.ID], MayAsk: seat.mayAsk,
				})
			}
			if len(rows) == limit {
				last := rows[len(rows)-1]
				next := myCursor(last.LastActivityAt, last.ID)
				out.Next = &next
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
	IssuedByID  *uuid.UUID `json:"issued_by_actor_id,omitempty" jsonschema:"who issued a token: the agent itself, its owner or an administrator; who set a temporary password (member.reset_password); absent for other kinds, for tokens made on the command line, and for tokens issued by a release before this field"`
	IssuedBy    *string    `json:"issued_by_name,omitempty" jsonschema:"the issuer's display name"`
	MustChange  bool       `json:"must_change,omitempty" jsonschema:"a password someone else set (member.reset_password): until its person sets their own with credential.set_password, every other call of theirs is refused (password_change_required)"`
	IssuedTo    *string    `json:"issued_to,omitempty" jsonschema:"for a runtime agent's token, agent_runtime: issued to the site's agent runtime, which runs the agent with it, and to nobody else"`
}

func viewCredentials(rows []dbq.ListCredentialsForActorRow) CredentialListOut {
	out := CredentialListOut{Credentials: make([]CredentialView, 0, len(rows))}
	for _, r := range rows {
		out.Credentials = append(out.Credentials, CredentialView{
			ID: r.ID, Kind: r.Kind, Provider: r.Provider, Subject: r.Subject, TokenPrefix: r.TokenPrefix,
			Label: r.Label, LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt, CreatedAt: r.CreatedAt,
			IssuedByID: r.IssuedByActorID, IssuedBy: r.IssuedByName, MustChange: r.MustChange, IssuedTo: r.IssuedToService,
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
		Description: "The caller's own credentials: a person's sessions, password and single sign-on identity; an agent's API tokens. Secrets are never shown.",
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
		Description: "Create an API token for the caller's own account, which must be an mcp agent's. A person is refused " +
			"(api_tokens_are_for_agents): people sign in with a password or single sign-on, and use one of their agents " +
			"for tools and scripts (agent.create, then agent.issue_token). A runtime agent is refused too " +
			"(hosted_by_runtime): its one token is the one the site's agent runtime holds. The token is returned once and " +
			"only its hash is kept: retrying this call returns the credential but not the token again.",
		Kind: tool.Write, Gate: self,
		HTTP:      tool.Route{Method: "POST", Pattern: "/v1/me/credentials/tokens"},
		SecretOut: []string{"token"},
		Resolve:   noTarget[IssueTokenIn]("credential"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in IssueTokenIn) (IssueTokenOut, error) {
			// Whether the caller may hold one at all comes before what they
			// asked for: a person may not, whatever the label.
			me, err := ec.Q.GetActor(ctx, ec.Actor.ID)
			if err != nil {
				return IssueTokenOut{}, err
			}
			if err := auth.MayHoldToken(me.Kind); err != nil {
				return IssueTokenOut{}, err
			}
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

// errSamePassword refuses, as the password someone chooses for themselves,
// the temporary one someone else set and so knows.
var errSamePassword = apperr.Invalid("choose a password of your own: that is the temporary one you were given").
	With("reason", "password_unchanged")

func credentialSetPassword() tool.Tool {
	return tool.Define(tool.Spec[SetPasswordIn, OK]{
		Name: "credential.set_password",
		Description: "Set or replace the caller's own password. The previous password stops working at once. It is the one " +
			"call a person whose password someone else set (member.reset_password) may make: every other is refused " +
			"(password_change_required) until they have set their own here, which may not be the one they were given " +
			"(password_unchanged). For people only: an agent holds API tokens and no password (agents_use_api_tokens).",
		Kind: tool.Write, Gate: self, SetsOwnPassword: true,
		HTTP:     tool.Route{Method: "POST", Pattern: "/v1/me/password"},
		SecretIn: []string{"password"},
		Resolve:  noTarget[SetPasswordIn]("credential"),
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SetPasswordIn) (OK, error) {
			// A refusal that reads kind, as the database's does: an agent
			// never signs in, and a password would be a way to.
			me, err := ec.Q.GetActor(ctx, ec.Actor.ID)
			if err != nil {
				return OK{}, err
			}
			if err := auth.MaySignIn(me.Kind); err != nil {
				return OK{}, err
			}
			if ec.Actor.PasswordChangeRequired {
				// Only then is the hash worth its cost: whoever set the
				// temporary one knows it, and it must not become theirs.
				switch cur, err := ec.Q.GetPasswordCredential(ctx, ec.Actor.ID); {
				case errors.Is(err, pgx.ErrNoRows):
				case err != nil:
					return OK{}, err
				case cur.MustChange && cur.SecretHash != nil:
					same, err := auth.VerifyPassword(in.Password, *cur.SecretHash)
					if err != nil {
						return OK{}, err
					}
					if same {
						return OK{}, errSamePassword
					}
				}
			}
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
