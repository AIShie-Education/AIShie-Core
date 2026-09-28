package tools

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/members"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func memberTools() []tool.Tool {
	return []tool.Tool{memberList(), memberGet(), memberLookupActor(), memberAdd(), memberUpdatePerms(), memberRescope(),
		memberPause(), memberResume(), memberRemove(), memberAddDelegate(), memberDelegateDefaults(), memberUpdatePermsBulk()}
}

var (
	readMembers   = tool.Gate{Perms: []domain.Perm{domain.PermMemberRead}}
	manageMembers = tool.Gate{Perms: []domain.Perm{domain.PermMemberManage}}
)

// MemberView is a seat in a course. kind and role are there to be shown; the
// permission check reads neither.
type MemberView struct {
	ID                uuid.UUID   `json:"id"`
	ActorID           uuid.UUID   `json:"actor_id"`
	DisplayName       string      `json:"display_name"`
	Kind              string      `json:"kind" jsonschema:"human or agent; for display only"`
	Role              string      `json:"role" jsonschema:"roster fact; the gradebook is the members whose role is student"`
	Status            string      `json:"status"`
	PresetID          *uuid.UUID  `json:"preset_id,omitempty" jsonschema:"where the permissions were copied from; not read again"`
	ExpiresAt         *time.Time  `json:"expires_at,omitempty"`
	StudentScope      string      `json:"student_scope"`
	AssignmentScope   string      `json:"assignment_scope"`
	Perms             PermLevels  `json:"perms"`
	ListedStudents    []uuid.UUID `json:"listed_students,omitempty"`
	ListedAssignments []uuid.UUID `json:"listed_assignments,omitempty"`
	CreatedAt         time.Time   `json:"created_at"`
	// A delegate's seat: an agent someone owns, seated as their delegate.
	PrincipalMemberID *uuid.UUID `json:"principal_member_id,omitempty" jsonschema:"for a delegate, its principal's seat: its owner's in this course, which caps everything it holds"`
	OwnerActorID      *uuid.UUID `json:"owner_actor_id,omitempty" jsonschema:"for an agent a person owns, that person"`
	OwnerName         *string    `json:"owner_name,omitempty"`
	AnswersCourse     bool       `json:"answers_course" jsonschema:"for a delegate, whether its seat answers the course, and not its principal alone; it does while its principal manages the course's members"`
	// An agent's seat: whether it is asked in the site at all. Absent for a
	// person's, who is.
	SiteChat *bool `json:"site_chat,omitempty" jsonschema:"for an agent's seat: whether people in the site may start conversations with it and ask it, since what runs it, an agent runtime that answers on its own, says so (me.site_chat); false for an agent operated from an external tool. Absent for a person's seat"`
}

// withSiteChat says in each view of an agent's seat whether it takes
// conversations in the site now.
func withSiteChat(ctx context.Context, rc *tool.ReadCtx, views []MemberView) error {
	actors := make([]uuid.UUID, 0, len(views))
	for _, v := range views {
		actors = append(actors, v.ActorID)
	}
	chat, err := siteChatOf(ctx, rc.Q, rc.Now, actors)
	if err != nil {
		return err
	}
	for i := range views {
		if c, ok := chat[views[i].ActorID]; ok && c.Agent {
			views[i].SiteChat = &c.SiteChat
		}
	}
	return nil
}

func viewMember(m dbq.GetMemberInCourseRow) MemberView {
	return MemberView{ID: m.ID, ActorID: m.ActorID, DisplayName: m.DisplayName, Kind: m.ActorKind, Role: m.Role,
		Status: m.Status, PresetID: m.PresetID, ExpiresAt: m.ExpiresAt, StudentScope: m.StudentScope,
		AssignmentScope: m.AssignmentScope, Perms: memberPerms(m).view(), CreatedAt: m.CreatedAt,
		PrincipalMemberID: m.PrincipalMemberID, OwnerActorID: m.OwnerActorID, OwnerName: m.OwnerName,
		AnswersCourse: m.AnswersCourse}
}

type MemberListIn struct {
	tool.InCourse
	Role           *string `json:"role,omitempty" jsonschema:"only members with this roster role, e.g. student"`
	IncludeRemoved bool    `json:"include_removed,omitempty"`
	Page
}

type MemberListOut struct {
	Members []MemberView `json:"members"`
	Next    *uuid.UUID   `json:"next,omitempty"`
}

func memberList() tool.Tool {
	return tool.Define(tool.Spec[MemberListIn, MemberListOut]{
		Name:        "member.list",
		Description: "The members of a course — people and agents alike — with their roster role, status, permissions and scope.",
		Kind:        tool.Read, Gate: readMembers,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/members"},
		Resolve: func(_ context.Context, _ dbq.Querier, in MemberListIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course_member"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in MemberListIn) (MemberListOut, error) {
			rows, err := rc.Q.ListMembers(ctx, dbq.ListMembersParams{CourseID: in.CourseID, After: in.after(),
				Role: in.Role, IncludeRemoved: in.IncludeRemoved, MaxRows: in.limit()})
			out := MemberListOut{Members: make([]MemberView, 0, len(rows))}
			for _, r := range rows {
				out.Members = append(out.Members, viewMember(dbq.GetMemberInCourseRow(r)))
			}
			if err == nil {
				err = withSiteChat(ctx, rc, out.Members)
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, err
		},
	})
}

type MemberIDIn struct {
	tool.InCourse
	MemberID uuid.UUID `json:"member_id"`
}

func resolveMember(ctx context.Context, q dbq.Querier, courseID, memberID uuid.UUID) (tool.Target, error) {
	if _, err := q.GetMemberInCourse(ctx, dbq.GetMemberInCourseParams{ID: memberID, CourseID: courseID}); errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such member in this course")
	} else if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: courseID, Type: "course_member", ID: &memberID}, nil
}

func memberGet() tool.Tool {
	return tool.Define(tool.Spec[MemberIDIn, MemberView]{
		Name:        "member.get",
		Description: "One member in full, including exactly which students and assignments a 'listed' scope lists.",
		Kind:        tool.Read, Gate: readMembers,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/members/{member_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in MemberIDIn) (tool.Target, error) {
			return resolveMember(ctx, q, in.CourseID, in.MemberID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in MemberIDIn) (MemberView, error) {
			m, err := rc.Q.GetMemberInCourse(ctx, dbq.GetMemberInCourseParams{ID: in.MemberID, CourseID: in.CourseID})
			if err != nil {
				return MemberView{}, err
			}
			views := []MemberView{viewMember(m)}
			if err := withSiteChat(ctx, rc, views); err != nil {
				return MemberView{}, err
			}
			v := views[0]
			if v.ListedStudents, err = rc.Q.ListStudentScope(ctx, m.ID); err != nil {
				return MemberView{}, err
			}
			v.ListedAssignments, err = rc.Q.ListAssignmentScope(ctx, m.ID)
			return v, err
		},
	})
}

type MemberLookupActorIn struct {
	tool.InCourse
	Email   *string    `json:"email,omitempty" jsonschema:"the person's whole email address, in any case"`
	ActorID *uuid.UUID `json:"actor_id,omitempty" jsonschema:"or the actor id an administrator gave, to see whom it names"`
}

type MemberLookupActorOut struct {
	ActorID     uuid.UUID  `json:"actor_id" jsonschema:"what member.add takes"`
	DisplayName string     `json:"display_name"`
	Kind        string     `json:"kind" jsonschema:"human or agent; for display only"`
	Status      string     `json:"status" jsonschema:"active or suspended"`
	MemberID    *uuid.UUID `json:"member_id,omitempty" jsonschema:"their seat in this course, when they already have one"`
	// An agent someone owns is seated by them, as their delegate, and not
	// with member.add.
	OwnerActorID *uuid.UUID `json:"owner_actor_id,omitempty" jsonschema:"for an agent a person owns, that person: only they seat it, with member.add_delegate"`
	OwnerName    *string    `json:"owner_name,omitempty"`
}

// memberLookupActor lets whoever seats members find the actor to seat without
// asking an administrator for an id, and see whom an id they were given
// names. It lists nobody: the whole address or the whole id must be given, so
// it tells the caller only about someone they could already name.
func memberLookupActor() tool.Tool {
	return tool.Define(tool.Spec[MemberLookupActorIn, MemberLookupActorOut]{
		Name: "member.lookup_actor",
		Description: "Find the registered person an email address belongs to, to seat them with member.add, or see whom " +
			"an actor id names (an agent has no email: it is seated by the id an administrator gives). Give one of " +
			"email or actor_id. The whole address must match, in any case; there is no partial search.",
		Kind: tool.Read, Gate: manageMembers,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/actor-lookup"},
		Resolve: func(_ context.Context, _ dbq.Querier, in MemberLookupActorIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "actor"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in MemberLookupActorIn) (MemberLookupActorOut, error) {
			var email *string
			if in.Email != nil {
				if e := strings.TrimSpace(*in.Email); e != "" {
					email = &e
				}
			}
			if (email == nil) == (in.ActorID == nil) {
				return MemberLookupActorOut{}, apperr.Invalid("give one of email or actor_id")
			}
			a, err := rc.Q.LookupActorForSeating(ctx, dbq.LookupActorForSeatingParams{CourseID: in.CourseID, ActorID: in.ActorID, Email: email})
			if errors.Is(err, pgx.ErrNoRows) {
				return MemberLookupActorOut{}, apperr.Missing("nobody is registered with that email or id")
			}
			if err != nil {
				return MemberLookupActorOut{}, err
			}
			return MemberLookupActorOut{ActorID: a.ID, DisplayName: a.DisplayName, Kind: a.Kind, Status: a.Status, MemberID: a.MemberID,
				OwnerActorID: a.OwnerActorID, OwnerName: a.OwnerName}, nil
		},
	})
}

// ---------------------------------------------------------------------------
// member.add
// ---------------------------------------------------------------------------

type MemberAddIn struct {
	tool.InCourse
	ActorID  uuid.UUID  `json:"actor_id"`
	Preset   *string    `json:"preset,omitempty" jsonschema:"a preset by name: student, observer, ta, instructor, tutor, grader, delegate, course_tutor, or one of the department's own"`
	PresetID *uuid.UUID `json:"preset_id,omitempty" jsonschema:"or a preset by id"`
	// Everything below overrides what the preset says.
	Role              *string     `json:"role,omitempty"`
	Perms             PermLevels  `json:"perms,omitempty" jsonschema:"individual permissions to set differently from the preset"`
	StudentScope      *string     `json:"student_scope,omitempty" jsonschema:"all or listed"`
	ListedStudents    []uuid.UUID `json:"listed_students,omitempty" jsonschema:"member ids of the students in scope; listed with none means nobody"`
	AssignmentScope   *string     `json:"assignment_scope,omitempty" jsonschema:"all or listed"`
	ListedAssignments []uuid.UUID `json:"listed_assignments,omitempty"`
	ExpiresAt         *time.Time  `json:"expires_at,omitempty" jsonschema:"the membership removes itself at this moment"`
}

func memberAdd() tool.Tool {
	return tool.Define(tool.Spec[MemberAddIn, MemberIDOut]{
		Name: "member.add",
		Description: "Seat an actor — a person or an agent — in the course. A preset gives the starting role, permissions " +
			"and scope, and any of them can be overridden here. You cannot grant more than you hold yourself: no permission " +
			"above your own level, and no scope wider than your own. An agent someone owns is not seated here: its owner " +
			"brings it in as their delegate, with member.add_delegate.",
		Kind: tool.Write, Gate: manageMembers,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/members"},
		Resolve: func(_ context.Context, _ dbq.Querier, in MemberAddIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course_member"}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberAddIn) (MemberIDOut, error) {
			preset, err := findPreset(ctx, ec.Q, in.CourseID, in.Preset, in.PresetID)
			if err != nil {
				return MemberIDOut{}, err
			}
			s := seating{courseID: in.CourseID, actorID: in.ActorID, preset: &preset, perms: presetPerms(preset),
				role: preset.Role, studentScope: preset.StudentScope, assignmentScope: preset.AssignmentScope,
				listedStudents: in.ListedStudents, listedAssignments: in.ListedAssignments, expiresAt: asStored(in.ExpiresAt)}
			if in.Role != nil {
				s.role = *in.Role
			}
			if in.StudentScope != nil {
				s.studentScope = *in.StudentScope
			}
			if in.AssignmentScope != nil {
				s.assignmentScope = *in.AssignmentScope
			}
			if !validRoles[s.role] || !validScope(s.studentScope) || !validScope(s.assignmentScope) {
				return MemberIDOut{}, apperr.Invalid("role or scope is not one of the allowed values")
			}
			if err := s.perms.apply(in.Perms); err != nil {
				return MemberIDOut{}, err
			}
			if err := withinGranter(ctx, ec.Q, ec.Member, s.perms, s.listsItself(), s.studentScope, s.listedStudents, s.assignmentScope, s.listedAssignments); err != nil {
				return MemberIDOut{}, err
			}
			if err := outlastsGranter(ec.Member, s.expiresAt); err != nil {
				return MemberIDOut{}, err
			}
			id, err := seat(ctx, ec, s)
			return MemberIDOut{MemberID: id}, err
		},
	})
}

// findPreset looks a preset up by id or by name. A name means the course's
// department's own preset of that name if there is one, else the built-in.
func findPreset(ctx context.Context, q dbq.Querier, courseID uuid.UUID, name *string, id *uuid.UUID) (dbq.PermissionPreset, error) {
	course, err := q.GetCourse(ctx, courseID)
	if err != nil {
		return dbq.PermissionPreset{}, err
	}
	var p dbq.PermissionPreset
	switch {
	case (name == nil) == (id == nil):
		return p, apperr.Invalid("give exactly one of preset and preset_id")
	case id != nil:
		p, err = q.GetPreset(ctx, *id)
	default:
		if p, err = q.GetDeptPresetByName(ctx, dbq.GetDeptPresetByNameParams{Name: *name, DeptID: &course.DeptID}); errors.Is(err, pgx.ErrNoRows) {
			p, err = q.GetBuiltinPresetByName(ctx, *name)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return p, apperr.Missing("no such preset")
	}
	if err != nil {
		return p, err
	}
	if p.DeptID != nil && *p.DeptID != course.DeptID {
		return p, apperr.Precondition("that preset belongs to another department")
	}
	return p, nil
}

// withinGranter refuses to hand out more than the granter holds. Without it
// perm_member_manage would quietly be every permission: seat a second
// account as instructor and use that.
//
// Permissions: none above the granter's own level. Scope: a granter limited
// to listed students may only give 'listed', from within their own list; the
// same for assignments. Lifetime is outlastsGranter, beside it.
//
// The three are measured together, on the whole of what the member will
// hold, because a level is held over a scope for a time: raising a level on
// a member who reaches the whole class, or widening the reach of a member
// who holds a level, or extending the life of either, hands out the product,
// and the product is what must be within the granter's own.
//
// A seat that lists itself reaches itself, like any other listed student:
// levels on a student's seat are levels over that student's work. There is
// no self-access special case here either, or a manager listed for Yuki
// could give Ken grade_post, over Ken, by raising it on Ken's own seat.
// listsItself says the list is about to gain the new seat's own id, which no
// granter can have listed yet.
func withinGranter(ctx context.Context, q dbq.Querier, g *domain.Member, perms permSet, listsItself bool, studentScope string, students []uuid.UUID, assignmentScope string, assignments []uuid.UUID) error {
	if p, over := perms.exceeds(g); over {
		return apperr.Forbid("you hold %s at %s and cannot grant it at %s", p, g.Perm(p), perms[p]).With("permission", string(p))
	}
	if g.StudentScope == domain.ScopeListed {
		if studentScope != domain.ScopeListed {
			return apperr.Forbid("your own student scope is a list; you cannot grant the whole class")
		}
		if listsItself {
			return apperr.Forbid("your own student scope is a list; a new student's seat reaches that student, who is not on it")
		}
		if reason, err := authz.CheckScope(ctx, q, g, authz.Target{StudentMemberIDs: students}); err != nil {
			return err
		} else if reason != authz.ReasonNone {
			return apperr.Forbid("you can only give a seat that reaches students who are in your own scope")
		}
	}
	if g.AssignmentScope == domain.ScopeListed {
		if assignmentScope != domain.ScopeListed {
			return apperr.Forbid("your own assignment scope is a list; you cannot grant every assignment")
		}
		if reason, err := authz.CheckScope(ctx, q, g, authz.Target{AssignmentIDs: assignments}); err != nil {
			return err
		} else if reason != authz.ReasonNone {
			return apperr.Forbid("you can only list assignments that are in your own scope")
		}
	}
	return nil
}

// outlastsGranter refuses a seat that would still be there after the
// granter's own has ended: a manager seated until the end of term hands out
// nothing that lasts longer.
func outlastsGranter(granter *domain.Member, expiresAt *time.Time) error {
	if g := granter.ExpiresAt; g != nil && (expiresAt == nil || expiresAt.After(*g)) {
		return apperr.Forbid("your own membership ends at %s; you cannot give one that lasts longer", g.UTC().Format(time.RFC3339))
	}
	return nil
}

// asStored drops what a timestamptz cannot hold. An expiry the caller gives
// is measured against expiries read back from the database — the granter's
// own, the one it replaces — which keep microseconds; kept to the nanosecond,
// the caller's would outlast the very same moment by what the database was
// about to drop.
func asStored(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	s := t.Truncate(time.Microsecond)
	return &s
}

// ---------------------------------------------------------------------------
// Changing a member
// ---------------------------------------------------------------------------

// loadOther loads the member a management tool acts on, locked for the rest
// of the action so that two managers editing the same seat take turns, and
// refuses the caller acting on their own seat: nobody raises, narrows, pauses
// or removes themselves by accident, or on purpose.
//
// A seat whose expiry has passed is as good as removed, whether or not the
// sweep has got to it yet: authorize() stopped honouring it at that moment,
// and what a removed seat needs is a fresh one. Otherwise whether it could
// be revived — with everything it held — would depend on when the sweep last
// ran.
func loadOther(ctx context.Context, ec *tool.ExecCtx, courseID, memberID uuid.UUID) (dbq.GetMemberInCourseRow, error) {
	// Refused before the seat is locked: the caller's own seat is already
	// held, shared, by this call, and upgrading that would deadlock with
	// another call of theirs doing the same.
	if memberID == ec.Member.ID {
		return dbq.GetMemberInCourseRow{}, apperr.Forbid("not on your own membership")
	}
	if err := holdPrincipalOf(ctx, ec.Q, memberID); err != nil {
		return dbq.GetMemberInCourseRow{}, err
	}
	row, err := ec.Q.GetMemberInCourseForUpdate(ctx, dbq.GetMemberInCourseForUpdateParams{ID: memberID, CourseID: courseID})
	m := dbq.GetMemberInCourseRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, apperr.Missing("no such member in this course")
	}
	if err != nil {
		return m, err
	}
	if m.Status == domain.MemberRemoved || (m.ExpiresAt != nil && !m.ExpiresAt.After(ec.Now)) {
		return m, apperr.Conflicts("the member has been removed; seat the actor again for a fresh start")
	}
	return m, nil
}

// holdPrincipalOf takes the KEY SHARE of a delegate's principal's seat, for
// a call about to lock the delegate's seat FOR UPDATE: whoever locks a
// delegate's seat to change or remove it takes its principal's first. A
// removal of the principal holds the principal FOR UPDATE and then updates
// the delegate's row, so the two wait for each other at the principal,
// before either holds the other's row; and a delegate's own call, which
// holds its seat and then takes its principal's KEY SHARE, is not blocked
// by this one. Whose delegate a seat is never changes, so it is read before
// anything is locked. A seat that is nobody's delegate, or no seat, takes
// nothing.
func holdPrincipalOf(ctx context.Context, q *dbq.Queries, seat uuid.UUID) error {
	principal, err := q.GetSeatPrincipal(ctx, seat)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && principal == nil) {
		return nil
	}
	if err != nil {
		return err
	}
	return q.ShareSeats(ctx, []uuid.UUID{*principal})
}

// shape is what a seat amounts to: levels, held over a reach, for a time.
type shape struct {
	perms                         permSet
	studentScope, assignmentScope string
	students, assignments         []uuid.UUID // a student's own seat is on its list: it reaches itself
	expiresAt                     *time.Time
	// principal is set for a delegate's seat, which is held to its
	// principal's as well as to the granter's.
	principal *uuid.UUID
}

func shapeOf(ctx context.Context, q *dbq.Queries, m dbq.GetMemberInCourseRow) (shape, error) {
	s := shape{perms: memberPerms(m), studentScope: m.StudentScope, assignmentScope: m.AssignmentScope, expiresAt: m.ExpiresAt,
		principal: m.PrincipalMemberID}
	var err error
	if s.students, err = q.ListStudentScope(ctx, m.ID); err != nil {
		return s, err
	}
	s.assignments, err = q.ListAssignmentScope(ctx, m.ID)
	return s, err
}

// widens reports whether after reaches anything before did not: a level
// raised, a scope opened or a list added to, a life extended. Such a change
// is a grant, and is measured like one; any other is a narrowing, which
// anyone who manages members may do, whatever they hold themselves.
func (after shape) widens(before shape) bool {
	for p, l := range after.perms {
		if l > before.perms[p] {
			return true
		}
	}
	if opens(before.studentScope, before.students, after.studentScope, after.students) ||
		opens(before.assignmentScope, before.assignments, after.assignmentScope, after.assignments) {
		return true
	}
	return before.expiresAt != nil && (after.expiresAt == nil || after.expiresAt.After(*before.expiresAt))
}

func opens(fromKind string, from []uuid.UUID, toKind string, to []uuid.UUID) bool {
	if fromKind == domain.ScopeAll {
		return false
	}
	if toKind == domain.ScopeAll {
		return true
	}
	had := map[uuid.UUID]bool{}
	for _, id := range from {
		had[id] = true
	}
	for _, id := range to {
		if !had[id] {
			return true
		}
	}
	return false
}

// grant is the one check every change to a seat goes through: if the change
// widens anything, the whole of what the member will then hold must be
// within the granter's own, and, for a delegate's seat, within its
// principal's as well (withinPrincipal).
func grant(ctx context.Context, ec *tool.ExecCtx, before, after shape) error {
	if !after.widens(before) {
		return nil
	}
	if err := withinGranter(ctx, ec.Q, ec.Member, after.perms, false, after.studentScope, after.students, after.assignmentScope, after.assignments); err != nil {
		return err
	}
	if err := outlastsGranter(ec.Member, after.expiresAt); err != nil {
		return err
	}
	if after.principal != nil {
		return withinPrincipal(ctx, ec.Q, *after.principal, after)
	}
	return nil
}

// withinPrincipal holds a widening change to a delegate's seat to its
// principal's: what the delegate may hold at most is what the principal
// holds, as domain.Member.Perm measures it, over no more than the
// principal's reach, for no longer than the principal's seat lasts; and never
// member_manage or agent_delegate. Authorization caps a delegate by its
// principal on every call regardless, so this keeps the row saying what the
// delegate can actually do. A change to one seat holds the principal's KEY
// SHARE, taken before the delegate's was locked (holdPrincipalOf), so it is
// not removed meanwhile; member.update_perms_bulk reads it as it stands.
// Either way a principal narrowed meanwhile narrows the delegate all the
// same, since authorization caps it.
func withinPrincipal(ctx context.Context, q *dbq.Queries, principalID uuid.UUID, after shape) error {
	p, err := authz.LoadMember(ctx, q, principalID)
	if err != nil {
		return err
	}
	for _, perm := range domain.AllPerms {
		if limit := domain.DelegateCap(p, perm); after.perms[perm] > limit {
			return apperr.Forbid("the delegate's principal holds %s at %s, so the delegate cannot hold it at %s", perm, limit, after.perms[perm]).
				With("permission", string(perm))
		}
	}
	if err := withinGranter(ctx, q, p, permSet{}, false, after.studentScope, after.students, after.assignmentScope, after.assignments); err != nil {
		if e, ok := apperr.As(err); ok {
			return apperr.Forbid("a delegate reaches no further than its principal, whose scope is narrower than that").With("detail", e.Message)
		}
		return err
	}
	if outlastsGranter(p, after.expiresAt) != nil {
		return apperr.Forbid("a delegate lasts no longer than its principal, whose membership ends at %s", p.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

type MemberUpdatePermsIn struct {
	tool.InCourse
	MemberID uuid.UUID  `json:"member_id"`
	Perms    PermLevels `json:"perms" jsonschema:"the permissions to change, by name; the rest stay as they are"`
}

func memberUpdatePerms() tool.Tool {
	return tool.Define(tool.Spec[MemberUpdatePermsIn, OK]{
		Name: "member.update_perms",
		Description: "Change individual permissions on a member. It takes effect on their next call: nothing is cached. " +
			"Raising one is a grant: everything the member will then hold — every permission, over their whole scope, " +
			"for as long as their seat lasts — must be within what you hold yourself. Lowering is always allowed.",
		Kind: tool.Write, Gate: manageMembers,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/members/{member_id}/perms"},
		Resolve: func(ctx context.Context, q dbq.Querier, in MemberUpdatePermsIn) (tool.Target, error) {
			return resolveMember(ctx, q, in.CourseID, in.MemberID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberUpdatePermsIn) (OK, error) {
			m, err := loadOther(ctx, ec, in.CourseID, in.MemberID)
			if err != nil {
				return OK{}, err
			}
			if len(in.Perms) == 0 {
				return OK{}, apperr.Invalid("perms is empty: nothing to change")
			}
			before, err := shapeOf(ctx, ec.Q, m)
			if err != nil {
				return OK{}, err
			}
			after := before
			after.perms = memberPerms(m)
			if err := after.perms.apply(in.Perms); err != nil {
				return OK{}, err
			}
			if err := grant(ctx, ec, before, after); err != nil {
				return OK{}, err
			}
			if err := setPerms(ctx, ec.Q, m.ID, after.perms); err != nil {
				return OK{}, err
			}
			ec.Emit(events.Event{Type: members.EventUpdated, CourseID: &in.CourseID, SubjectType: "course_member", SubjectID: &m.ID})
			return OK{OK: true}, nil
		},
	})
}

type MemberRescopeIn struct {
	tool.InCourse
	MemberID          uuid.UUID   `json:"member_id"`
	StudentScope      *string     `json:"student_scope,omitempty"`
	ListedStudents    []uuid.UUID `json:"listed_students,omitempty" jsonschema:"replaces the list; omit to keep it"`
	AssignmentScope   *string     `json:"assignment_scope,omitempty"`
	ListedAssignments []uuid.UUID `json:"listed_assignments,omitempty" jsonschema:"replaces the list; omit to keep it"`
	ExpiresAt         *time.Time  `json:"expires_at,omitempty"`
	ClearExpiry       bool        `json:"clear_expiry,omitempty"`
}

func memberRescope() tool.Tool {
	return tool.Define(tool.Spec[MemberRescopeIn, OK]{
		Name: "member.rescope",
		Description: "Change which students and assignments a member's permissions reach, or when the membership expires. " +
			"'listed' with an empty list reaches nobody. Widening a scope, or extending or clearing an expiry, is a grant: " +
			"everything the member will then hold must be within what you hold yourself. Narrowing is always allowed.",
		Kind: tool.Write, Gate: manageMembers,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/members/{member_id}/scope"},
		Resolve: func(ctx context.Context, q dbq.Querier, in MemberRescopeIn) (tool.Target, error) {
			return resolveMember(ctx, q, in.CourseID, in.MemberID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberRescopeIn) (OK, error) {
			m, err := loadOther(ctx, ec, in.CourseID, in.MemberID)
			if err != nil {
				return OK{}, err
			}
			switch {
			case in.ClearExpiry && in.ExpiresAt != nil:
				return OK{}, apperr.Invalid("give expires_at or clear_expiry, not both")
			case in.ExpiresAt != nil && !in.ExpiresAt.After(ec.Now):
				return OK{}, apperr.Invalid("expires_at is in the past; to end a membership now, remove it")
			}
			before, err := shapeOf(ctx, ec.Q, m)
			if err != nil {
				return OK{}, err
			}
			after := before
			if in.StudentScope != nil {
				after.studentScope = *in.StudentScope
			}
			if in.AssignmentScope != nil {
				after.assignmentScope = *in.AssignmentScope
			}
			if !validScope(after.studentScope) || !validScope(after.assignmentScope) {
				return OK{}, apperr.Invalid("a scope is all or listed")
			}
			// A list that was not sent is kept as it is — not re-checked, so
			// a student who has since left does not make an unrelated change
			// fail — unless the scope it belongs to is no longer a list.
			students, assignments := in.ListedStudents, in.ListedAssignments
			if students != nil || after.studentScope != domain.ScopeListed {
				after.students = dedupe(students)
			}
			if assignments != nil || after.assignmentScope != domain.ScopeListed {
				after.assignments = dedupe(assignments)
			}
			switch {
			case in.ClearExpiry:
				after.expiresAt = nil
			case in.ExpiresAt != nil:
				after.expiresAt = asStored(in.ExpiresAt)
			}
			if err := grant(ctx, ec, before, after); err != nil {
				return OK{}, err
			}

			if err := ec.Q.SetMemberScopeKinds(ctx, dbq.SetMemberScopeKindsParams{ID: m.ID, StudentScope: after.studentScope, AssignmentScope: after.assignmentScope}); err != nil {
				return OK{}, err
			}
			if students != nil || after.studentScope != domain.ScopeListed {
				if err := writeStudentScope(ctx, ec.Q, in.CourseID, m.ID, after.studentScope, students); err != nil {
					return OK{}, err
				}
			}
			if assignments != nil || after.assignmentScope != domain.ScopeListed {
				if err := writeAssignmentScope(ctx, ec.Q, in.CourseID, m.ID, after.assignmentScope, assignments); err != nil {
					return OK{}, err
				}
			}
			if in.ClearExpiry || in.ExpiresAt != nil {
				if err := ec.Q.SetMemberExpiry(ctx, dbq.SetMemberExpiryParams{ID: m.ID, ExpiresAt: after.expiresAt}); err != nil {
					return OK{}, err
				}
			}
			ec.Emit(events.Event{Type: members.EventRescoped, CourseID: &in.CourseID, SubjectType: "course_member", SubjectID: &m.ID})
			return OK{OK: true}, nil
		},
	})
}

func memberSetStatus(name, desc, path, from, to, event string) tool.Tool {
	return tool.Define(tool.Spec[MemberIDIn, OK]{
		Name: name, Description: desc, Kind: tool.Write, Gate: manageMembers,
		HTTP: tool.Route{Method: "POST", Pattern: path},
		Resolve: func(ctx context.Context, q dbq.Querier, in MemberIDIn) (tool.Target, error) {
			return resolveMember(ctx, q, in.CourseID, in.MemberID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberIDIn) (OK, error) {
			m, err := loadOther(ctx, ec, in.CourseID, in.MemberID)
			if err != nil {
				return OK{}, err
			}
			if to == domain.MemberActive {
				// Resuming gives back everything the seat holds: a grant of
				// the whole of it.
				held, err := shapeOf(ctx, ec.Q, m)
				if err != nil {
					return OK{}, err
				}
				if err := grant(ctx, ec, shape{}, held); err != nil {
					return OK{}, err
				}
			}
			n, err := ec.Q.SetMemberStatus(ctx, dbq.SetMemberStatusParams{ID: m.ID, Status: to, FromStatus: from})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the member is %s, not %s", m.Status, from)
			}
			ec.Emit(events.Event{Type: event, CourseID: &in.CourseID, SubjectType: "course_member", SubjectID: &m.ID})
			return OK{OK: true}, nil
		},
	})
}

func memberPause() tool.Tool {
	return memberSetStatus("member.pause",
		"Pause a member: every call they make is denied from now until they are resumed. The same membership, "+
			"with its id, permissions and history, carries on afterwards — for an agent, the same relationship. "+
			"Their pending proposals stay queued but cannot be approved while they are paused.",
		"/v1/courses/{course_id}/members/{member_id}/pause", domain.MemberActive, domain.MemberPaused, members.EventPaused)
}

func memberResume() tool.Tool {
	return memberSetStatus("member.resume", "Resume a paused member, exactly as they were. That is a grant of everything they hold, "+
		"which must be within what you hold yourself.",
		"/v1/courses/{course_id}/members/{member_id}/resume", domain.MemberPaused, domain.MemberActive, members.EventResumed)
}

type MemberRemoveOut struct {
	CancelledProposals int `json:"cancelled_proposals"`
}

func memberRemove() tool.Tool {
	return tool.Define(tool.Spec[MemberIDIn, MemberRemoveOut]{
		Name: "member.remove",
		Description: "Remove a member from the course. Everything they did stays on record, and whatever they had proposed " +
			"that nobody has decided yet is cancelled. Seating the same actor again later is a new membership with a new id: " +
			"for an agent, a fresh start.",
		Kind: tool.Write, Gate: manageMembers,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/members/{member_id}/remove"},
		Resolve: func(ctx context.Context, q dbq.Querier, in MemberIDIn) (tool.Target, error) {
			return resolveMember(ctx, q, in.CourseID, in.MemberID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberIDIn) (MemberRemoveOut, error) {
			m, err := loadOther(ctx, ec, in.CourseID, in.MemberID)
			if err != nil {
				return MemberRemoveOut{}, err
			}
			n, err := members.Remove(ctx, ec.Q, ec.Emit, in.CourseID, m.ID, members.ReasonRemoved)
			return MemberRemoveOut{CancelledProposals: n}, err
		},
	})
}

// setPerms writes every level of a seat.
func setPerms(ctx context.Context, q *dbq.Queries, id uuid.UUID, ps permSet) error {
	return q.SetMemberPerms(ctx, dbq.SetMemberPermsParams{ID: id,
		PermDocumentRead: ps.col(domain.PermDocumentRead), PermDocumentReadDraft: ps.col(domain.PermDocumentReadDraft),
		PermDocumentWrite: ps.col(domain.PermDocumentWrite), PermRubricRead: ps.col(domain.PermRubricRead),
		PermAssignmentWrite: ps.col(domain.PermAssignmentWrite), PermSubmissionRead: ps.col(domain.PermSubmissionRead),
		PermSubmissionWrite: ps.col(domain.PermSubmissionWrite), PermGradeRead: ps.col(domain.PermGradeRead),
		PermGradeSubmit: ps.col(domain.PermGradeSubmit), PermGradePost: ps.col(domain.PermGradePost),
		PermMemberRead: ps.col(domain.PermMemberRead), PermMemberManage: ps.col(domain.PermMemberManage),
		PermActionDecide: ps.col(domain.PermActionDecide), PermAgentDelegate: ps.col(domain.PermAgentDelegate),
		PermConversationAsk: ps.col(domain.PermConversationAsk), PermConversationAnswer: ps.col(domain.PermConversationAnswer),
		PermMemberInvite: ps.col(domain.PermMemberInvite),
	})
}

type MemberUpdatePermsBulkIn struct {
	tool.InCourse
	Role  string     `json:"role" jsonschema:"every seat with this roster role: student, instructor, ta, observer or assistant"`
	Perms PermLevels `json:"perms" jsonschema:"the permissions to change, by name; the rest stay as they are"`
}

type MemberUpdatePermsBulkOut struct {
	Updated int `json:"updated" jsonschema:"how many seats were changed"`
}

// memberUpdatePermsBulk is member.update_perms for a whole roster role at
// once: "students may bring their own agents only with approval" is one
// call, not one per student. Every seat goes through grant() on its own, and
// all of them change or none does. Choosing the seats by role is what the
// manager asked for, as member.list filters by it; it is not authorization,
// which reads neither role nor kind.
func memberUpdatePermsBulk() tool.Tool {
	return tool.Define(tool.Spec[MemberUpdatePermsBulkIn, MemberUpdatePermsBulkOut]{
		Name: "member.update_perms_bulk",
		Description: "Change individual permissions on every seat with one roster role — every student, say — other than " +
			"your own, removed and expired seats left out. Each change is held to the rules of member.update_perms: " +
			"raising a level is a grant that must be within what you hold yourself, over that member's whole scope. " +
			"If any one seat cannot be changed, none is. It changes the seats there are now: a seat added later takes its " +
			"preset's levels, so repeat the call, give the levels to member.add, or use a department preset.",
		Kind: tool.Write, Gate: manageMembers,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/members/bulk-perms"},
		Resolve: func(_ context.Context, _ dbq.Querier, in MemberUpdatePermsBulkIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "course_member"}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in MemberUpdatePermsBulkIn) (MemberUpdatePermsBulkOut, error) {
			if !validRoles[in.Role] {
				return MemberUpdatePermsBulkOut{}, apperr.Invalid("role must be student, instructor, ta, observer or assistant")
			}
			if len(in.Perms) == 0 {
				return MemberUpdatePermsBulkOut{}, apperr.Invalid("perms is empty: nothing to change")
			}
			if err := (permSet{}).apply(in.Perms); err != nil {
				return MemberUpdatePermsBulkOut{}, err
			}
			// In id order, so that two of these at once take the seats in the
			// same order and one waits for the other.
			seats, err := ec.Q.LockLiveSeatsByRole(ctx, dbq.LockLiveSeatsByRoleParams{
				CourseID: in.CourseID, Role: in.Role, Now: &ec.Now, ExceptMemberID: ec.Member.ID})
			if err != nil {
				return MemberUpdatePermsBulkOut{}, err
			}
			out := MemberUpdatePermsBulkOut{}
			for _, id := range seats {
				m, err := ec.Q.GetMemberInCourse(ctx, dbq.GetMemberInCourseParams{ID: id, CourseID: in.CourseID})
				if err != nil {
					return MemberUpdatePermsBulkOut{}, err
				}
				before, err := shapeOf(ctx, ec.Q, m)
				if err != nil {
					return MemberUpdatePermsBulkOut{}, err
				}
				after := before
				after.perms = memberPerms(m)
				if err := after.perms.apply(in.Perms); err != nil {
					return MemberUpdatePermsBulkOut{}, err
				}
				if err := grant(ctx, ec, before, after); err != nil {
					if e, ok := apperr.As(err); ok {
						return MemberUpdatePermsBulkOut{}, e.With("member_id", id)
					}
					return MemberUpdatePermsBulkOut{}, err
				}
				if err := setPerms(ctx, ec.Q, m.ID, after.perms); err != nil {
					return MemberUpdatePermsBulkOut{}, err
				}
				ec.Emit(events.Event{Type: members.EventUpdated, CourseID: &in.CourseID, SubjectType: "course_member", SubjectID: &m.ID})
				out.Updated++
			}
			return out, nil
		},
	})
}
