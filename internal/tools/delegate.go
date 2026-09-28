package tools

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// A person brings an agent they own into a course as their delegate: a seat
// whose principal is their own seat there, and which never holds more than
// that seat does (domain.Member.Perm, authz.CheckScope). What it is seated
// with is worked out by one function, resolveDelegateSeat, for the call, for
// the proposal it may become, for its approval, and for the preview.

// DelegatePreset is the built-in preset a delegate is seated with unless
// another is named, and, for someone who does not manage the course's
// members, the most it may be given.
const DelegatePreset = "delegate"

// CourseTutorPreset is the built-in preset of a course's own
// question-answering agent: seated with it by someone who manages the
// course's members, a delegate answers the course unless told otherwise.
const CourseTutorPreset = "course_tutor"

var bringsAgents = tool.Gate{Perms: []domain.Perm{domain.PermAgentDelegate}}

// namedOnly are what a delegate holds only when the call that seats it names
// them, whatever its preset carries: an agent hands out the course's join
// links because someone decided it should, never because a preset came with
// it (docs/schema.md §2.2).
var namedOnly = []domain.Perm{domain.PermMemberInvite}

type MemberAddDelegateIn struct {
	tool.InCourse
	ActorID  uuid.UUID  `json:"actor_id" jsonschema:"the agent you own to bring in"`
	Preset   *string    `json:"preset,omitempty" jsonschema:"a preset by name: delegate (the default), your own assistant; course_tutor, an agent any student may ask about the material; or any preset the course can use"`
	PresetID *uuid.UUID `json:"preset_id,omitempty" jsonschema:"or a preset by id"`
	// Everything below overrides what the preset, clipped to what you hold,
	// says.
	Perms             PermLevels   `json:"perms,omitempty" jsonschema:"individual permissions to set differently; none above your own level"`
	StudentScope      *string      `json:"student_scope,omitempty" jsonschema:"all or listed"`
	ListedStudents    *[]uuid.UUID `json:"listed_students,omitempty" jsonschema:"member ids of the students in scope, within your own; by default your own list, or nobody if you reach the whole class"`
	AssignmentScope   *string      `json:"assignment_scope,omitempty" jsonschema:"all or listed"`
	ListedAssignments *[]uuid.UUID `json:"listed_assignments,omitempty"`
	ExpiresAt         *time.Time   `json:"expires_at,omitempty" jsonschema:"by default when your own membership ends; no later"`
	AnswersCourse     *bool        `json:"answers_course,omitempty" jsonschema:"true: a course agent, which the students it can see and do no more than may ask, as well as you; false: it answers you alone. Only someone who manages the course's members may make it true; by default true for the course_tutor preset when you do, false otherwise"`
	// Filled in when the call waits for a decision, for whoever makes it.
	AgentDisplayName *string `json:"agent_display_name,omitempty" jsonschema:"set by the server on a proposal, for whoever decides it; ignored"`
	OwnerDisplayName *string `json:"owner_display_name,omitempty" jsonschema:"set by the server on a proposal, for whoever decides it; ignored"`
}

// resolveDelegateSeat works out the seat member m's agent would be given:
//
//   - levels: the preset's, each clipped to what m holds (for
//     conversation_answer, to m's conversation_ask), with member_manage and
//     agent_delegate denied, and member_invite too unless it is named; a
//     level named in the call replaces the preset's, and one above what m
//     holds is refused rather than clipped: the caller asked for it by name;
//   - for someone who does not manage the course's members, no more than
//     the built-in delegate preset gives: a student's agent reads; an
//     instructor may widen it later, within its principal;
//   - reach: the preset's kind of scope, with m's own list when m is
//     limited to one and nobody when m reaches the whole class; a preset
//     that reaches the whole class is narrowed to m's list when m has one.
//     A kind or list named in the call must be within m's own;
//   - life: m's own, unless an earlier end is named;
//   - whom it answers: m alone, unless it answers the course, which only
//     someone who manages the course's members may say, and which the
//     course_tutor preset says for them unless they say otherwise;
//   - role assistant, principal m.
//
// It refuses outright a caller who is a delegate: an agent brings in no
// agents of its own.
func resolveDelegateSeat(ctx context.Context, q dbq.Querier, m *domain.Member, in MemberAddDelegateIn) (seating, error) {
	if m.PrincipalID != nil {
		return seating{}, apperr.Forbid("a delegate brings no agents of its own; its principal does")
	}
	name, id := in.Preset, in.PresetID
	if name == nil && id == nil {
		n := DelegatePreset
		name = &n
	}
	preset, err := findPreset(ctx, q, in.CourseID, name, id)
	if err != nil {
		return seating{}, err
	}

	perms := presetPerms(preset)
	for _, p := range domain.AllPerms {
		perms[p] = min(perms[p], domain.DelegateCap(m, p))
	}
	for _, p := range namedOnly {
		perms[p] = domain.Denied
	}
	named := permSet{}
	if err := named.apply(in.Perms); err != nil {
		return seating{}, err
	}
	for _, p := range domain.AllPerms {
		l, ok := named[p]
		if !ok {
			continue
		}
		if limit := domain.DelegateCap(m, p); l > limit {
			if p == domain.PermMemberManage || p == domain.PermAgentDelegate {
				return seating{}, apperr.Forbid("a delegate never holds %s", p).With("permission", string(p))
			}
			return seating{}, apperr.Forbid("you hold %s at %s and cannot give your agent %s", p, limit, l).With("permission", string(p))
		}
		perms[p] = l
	}
	if !m.Perm(domain.PermMemberManage).Allowed() {
		bound, err := q.GetBuiltinPresetByName(ctx, DelegatePreset)
		if errors.Is(err, pgx.ErrNoRows) {
			return seating{}, apperr.Precondition("the built-in delegate preset is missing; run `aishiterud seed`")
		}
		if err != nil {
			return seating{}, err
		}
		most := presetPerms(bound)
		for _, p := range domain.AllPerms {
			if perms[p] > most[p] {
				return seating{}, apperr.Forbid("your agent may hold %s at %s at most, as the delegate preset gives it; "+
					"someone who manages the course's members may give it more", p, most[p]).With("permission", string(p))
			}
		}
	}

	s := seating{courseID: in.CourseID, actorID: in.ActorID, preset: &preset, perms: perms, role: "assistant", principal: &m.ID}
	manages := m.Perm(domain.PermMemberManage).Allowed()
	s.answersCourse = manages && preset.Name == CourseTutorPreset
	if in.AnswersCourse != nil {
		if *in.AnswersCourse && !manages {
			return seating{}, apperr.Forbid("only someone who manages the course's members brings in an agent that answers the course; "+
				"yours answers you alone").With("field", "answers_course")
		}
		s.answersCourse = *in.AnswersCourse
	}
	if s.studentScope, s.listedStudents, err = delegateReach(m.StudentScope, func() ([]uuid.UUID, error) {
		return q.ListStudentScope(ctx, m.ID)
	}, preset.StudentScope, in.StudentScope, in.ListedStudents); err != nil {
		return seating{}, err
	}
	if s.assignmentScope, s.listedAssignments, err = delegateReach(m.AssignmentScope, func() ([]uuid.UUID, error) {
		return q.ListAssignmentScope(ctx, m.ID)
	}, preset.AssignmentScope, in.AssignmentScope, in.ListedAssignments); err != nil {
		return seating{}, err
	}
	if !validScope(s.studentScope) || !validScope(s.assignmentScope) {
		return seating{}, apperr.Invalid("a scope is all or listed")
	}
	if err := withinGranter(ctx, q, m, permSet{}, false, s.studentScope, s.listedStudents, s.assignmentScope, s.listedAssignments); err != nil {
		return seating{}, err
	}

	s.expiresAt = m.ExpiresAt
	if in.ExpiresAt != nil {
		s.expiresAt = asStored(in.ExpiresAt)
	}
	if err := outlastsGranter(m, s.expiresAt); err != nil {
		return seating{}, err
	}
	return s, nil
}

// delegateReach is one of a delegate's two scopes, before it is held to the
// principal's (withinGranter): a kind and list named in the call as they
// are; otherwise the preset's kind, and for a list the principal's own, or
// nobody if the principal has none. A preset that reaches everything is
// narrowed to the principal's list when the principal has one, as a level
// is clipped to the principal's.
func delegateReach(callerKind string, callerList func() ([]uuid.UUID, error),
	presetKind string, kind *string, list *[]uuid.UUID) (string, []uuid.UUID, error) {
	k := presetKind
	if kind != nil {
		k = *kind
	} else if callerKind == domain.ScopeListed {
		k = domain.ScopeListed
	}
	if list != nil {
		return k, dedupe(*list), nil
	}
	if k != domain.ScopeListed {
		return k, nil, nil
	}
	if callerKind != domain.ScopeListed {
		return k, []uuid.UUID{}, nil
	}
	own, err := callerList()
	return k, own, err
}

// agentOf checks that actor is an agent the caller owns, active, and not
// seated here already, and returns it, holding its row FOR SHARE
// (holdOwnAgent). Someone else's agent, or anyone who is not an agent of
// the caller's, is not found: the caller learns nothing about actors that
// are not theirs.
//
// now is the pipeline's clock when there is one. Validate has none, and
// passes nil: a seat that has an expiry is then left for seat() to judge
// when the call runs, rather than judged by another clock.
func agentOf(ctx context.Context, q dbq.Querier, owner, actor, courseID uuid.UUID, now *time.Time) (dbq.Actor, error) {
	a, err := holdOwnAgent(ctx, q, owner, actor)
	if err != nil {
		return a, err
	}
	if a.Status != domain.ActorActive {
		return a, apperr.Precondition("the agent is suspended: reactivate it first")
	}
	switch live, err := q.GetLiveMembership(ctx, dbq.GetLiveMembershipParams{CourseID: courseID, ActorID: actor}); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return a, err
	default:
		// A seat past its expiry, or orphaned, is removed when the new one
		// is made (seat); any other is in the way.
		if live.ExpiresAt != nil && now == nil {
			break
		}
		orphaned, err := q.SeatOrphaned(ctx, dbq.SeatOrphanedParams{MemberID: live.ID, Now: now})
		if err != nil {
			return a, err
		}
		if !orphaned && (live.ExpiresAt == nil || live.ExpiresAt.After(*now)) {
			return a, errSeated
		}
	}
	return a, nil
}

var errNotYourAgent = apperr.Missing("no such agent of yours")

func memberAddDelegate() tool.Tool {
	return tool.Define(tool.Spec[MemberAddDelegateIn, MemberIDOut]{
		Name: "member.add_delegate",
		Description: "Bring an agent you own into this course as your delegate: its seat's principal is yours, and it can " +
			"never do more than you can here, nor reach further, nor outlast your seat; it is paused while you are, and removed " +
			"with you. It is seated with the delegate preset — your own assistant, which reads and answers you — unless you " +
			"name another, such as course_tutor, clipped to what you hold; member.delegate_defaults shows what it would get. " +
			"It answers you alone unless it answers the course (answers_course), which only someone who manages the course's " +
			"members may choose, and which course_tutor chooses for them: then the students it can see and do no more than " +
			"may ask it too, and whatever anyone tells it, it may repeat to the others it answers. " +
			"Your level of agent_delegate decides whether this needs an instructor's approval first. " +
			"Unless you manage the course's members, your agent holds no more than the delegate preset gives.",
		Kind: tool.Write, Gate: bringsAgents,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/delegates"},
		Resolve: func(_ context.Context, _ dbq.Querier, in MemberAddDelegateIn) (tool.Target, error) {
			// Filed under the agent, so that a request waiting for it can be
			// found from it (agent.get). Whether it is the caller's is for
			// Validate, which knows who is calling.
			return tool.Target{CourseID: in.CourseID, Type: "actor", ID: &in.ActorID}, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, in MemberAddDelegateIn) error {
			if _, err := resolveDelegateSeat(ctx, q, m, in); err != nil {
				return err
			}
			_, err := agentOf(ctx, q, m.ActorID, in.ActorID, in.CourseID, nil)
			return err
		},
		// A proposal stores the seat as it would be made now, every level and
		// list written out, so that what is approved is what was asked for:
		// approving it works the seat out again from those, and a level or a
		// reach the proposer no longer holds is refused, not given. The names
		// are for whoever decides it, read from the database, never taken
		// from the call.
		Pin: func(ctx context.Context, q dbq.Querier, m *domain.Member, _ time.Time, in MemberAddDelegateIn) (MemberAddDelegateIn, error) {
			s, err := resolveDelegateSeat(ctx, q, m, in)
			if err != nil {
				return in, err
			}
			students, assignments := s.listedStudents, s.listedAssignments
			pinned := MemberAddDelegateIn{InCourse: in.InCourse, ActorID: in.ActorID, PresetID: &s.preset.ID,
				Perms: s.perms.view(), StudentScope: &s.studentScope, AssignmentScope: &s.assignmentScope,
				ExpiresAt: s.expiresAt, AnswersCourse: &s.answersCourse}
			if s.studentScope == domain.ScopeListed {
				pinned.ListedStudents = &students
			}
			if s.assignmentScope == domain.ScopeListed {
				pinned.ListedAssignments = &assignments
			}
			agent, err := q.GetActor(ctx, in.ActorID)
			if err != nil {
				return in, err
			}
			owner, err := q.GetActor(ctx, m.ActorID)
			if err != nil {
				return in, err
			}
			pinned.AgentDisplayName, pinned.OwnerDisplayName = &agent.DisplayName, &owner.DisplayName
			return pinned, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberAddDelegateIn) (MemberIDOut, error) {
			s, err := resolveDelegateSeat(ctx, ec.Q, ec.Member, in)
			if err != nil {
				return MemberIDOut{}, err
			}
			if _, err := agentOf(ctx, ec.Q, ec.Actor.ID, in.ActorID, in.CourseID, &ec.Now); err != nil {
				return MemberIDOut{}, err
			}
			id, err := seat(ctx, ec, s)
			return MemberIDOut{MemberID: id}, err
		},
	})
}

type DelegateDefaultsIn struct {
	tool.InCourse
	Preset *string `json:"preset,omitempty" jsonschema:"delegate (the default), course_tutor, or any preset the course can use"`
}

type DelegateDefaultsOut struct {
	PresetID          uuid.UUID   `json:"preset_id"`
	Preset            string      `json:"preset"`
	Role              string      `json:"role"`
	Perms             PermLevels  `json:"perms"`
	StudentScope      string      `json:"student_scope"`
	ListedStudents    []uuid.UUID `json:"listed_students" jsonschema:"when student_scope is listed: whom it reaches; empty is nobody"`
	AssignmentScope   string      `json:"assignment_scope"`
	ListedAssignments []uuid.UUID `json:"listed_assignments"`
	ExpiresAt         *time.Time  `json:"expires_at,omitempty"`
	AnswersCourse     bool        `json:"answers_course" jsonschema:"whether it would answer the course, and not you alone"`
	// Level is the caller's agent_delegate: what member.add_delegate would
	// come to.
	Level string `json:"level" jsonschema:"autonomous: seated at once; pending_review: seated, and reviewed after; confirm_required: a request an instructor approves"`
}

func memberDelegateDefaults() tool.Tool {
	return tool.Define(tool.Spec[DelegateDefaultsIn, DelegateDefaultsOut]{
		Name: "member.delegate_defaults",
		Description: "What member.add_delegate would seat your agent with here if you named nothing but the preset: its " +
			"permissions, which students and assignments it would reach, when it would end, and whether bringing it in " +
			"needs an instructor's approval first, and whether it would answer the course or you alone.",
		Kind: tool.Read, Gate: bringsAgents,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/delegates/defaults"},
		Resolve: func(_ context.Context, _ dbq.Querier, in DelegateDefaultsIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course_member"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in DelegateDefaultsIn) (DelegateDefaultsOut, error) {
			s, err := resolveDelegateSeat(ctx, rc.Q, rc.Member, MemberAddDelegateIn{InCourse: in.InCourse, Preset: in.Preset})
			if err != nil {
				return DelegateDefaultsOut{}, err
			}
			out := DelegateDefaultsOut{PresetID: s.preset.ID, Preset: s.preset.Name, Role: s.role, Perms: s.perms.view(),
				StudentScope: s.studentScope, ListedStudents: nonNil(s.listedStudents),
				AssignmentScope: s.assignmentScope, ListedAssignments: nonNil(s.listedAssignments),
				ExpiresAt: s.expiresAt, AnswersCourse: s.answersCourse, Level: rc.Member.Perm(domain.PermAgentDelegate).String()}
			return out, nil
		},
	})
}

func nonNil(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return slices.Clone(ids)
}
