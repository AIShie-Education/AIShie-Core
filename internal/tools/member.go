package tools

import (
	"context"
	"errors"
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
	return []tool.Tool{memberList(), memberGet(), memberAdd(), memberUpdatePerms(), memberRescope(),
		memberPause(), memberResume(), memberRemove()}
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
}

func viewMember(m dbq.GetMemberInCourseRow) MemberView {
	return MemberView{ID: m.ID, ActorID: m.ActorID, DisplayName: m.DisplayName, Kind: m.ActorKind, Role: m.Role,
		Status: m.Status, PresetID: m.PresetID, ExpiresAt: m.ExpiresAt, StudentScope: m.StudentScope,
		AssignmentScope: m.AssignmentScope, Perms: memberPerms(m).view(), CreatedAt: m.CreatedAt}
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
			v := viewMember(m)
			if v.ListedStudents, err = rc.Q.ListStudentScope(ctx, m.ID); err != nil {
				return MemberView{}, err
			}
			v.ListedAssignments, err = rc.Q.ListAssignmentScope(ctx, m.ID)
			return v, err
		},
	})
}

// ---------------------------------------------------------------------------
// member.add
// ---------------------------------------------------------------------------

type MemberAddIn struct {
	tool.InCourse
	ActorID  uuid.UUID  `json:"actor_id"`
	Preset   *string    `json:"preset,omitempty" jsonschema:"a preset by name: student, observer, ta, instructor, tutor, grader, or one of the department's own"`
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
			"above your own level, and no scope wider than your own.",
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
			if err := withinGranter(ctx, ec, s.perms, s.role, s.studentScope, s.listedStudents, s.assignmentScope, s.listedAssignments); err != nil {
				return MemberIDOut{}, err
			}
			if err := outlastsGranter(ec, s.expiresAt); err != nil {
				return MemberIDOut{}, err
			}
			id, err := seat(ctx, ec, s)
			return MemberIDOut{MemberID: id}, err
		},
	})
}

// findPreset looks a preset up by id or by name. A name means the course's
// department's own preset of that name if there is one, else the built-in.
func findPreset(ctx context.Context, q *dbq.Queries, courseID uuid.UUID, name *string, id *uuid.UUID) (dbq.PermissionPreset, error) {
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
func withinGranter(ctx context.Context, ec *tool.ExecCtx, perms permSet, role, studentScope string, students []uuid.UUID, assignmentScope string, assignments []uuid.UUID) error {
	g := ec.Member
	if p, over := perms.exceeds(g); over {
		return apperr.Forbid("you hold %s at %s and cannot grant it at %s", p, g.Perm(p), perms[p]).With("permission", string(p))
	}
	if g.StudentScope == domain.ScopeListed {
		// A student's default list is themselves, which the granter need not
		// have been listed for in advance.
		implicitSelf := role == "student" && studentScope == domain.ScopeListed && len(students) == 0
		if studentScope != domain.ScopeListed {
			return apperr.Forbid("your own student scope is a list; you cannot grant the whole class")
		}
		if !implicitSelf {
			if reason, err := authz.CheckScope(ctx, ec.Q, g, authz.Target{StudentMemberIDs: students}); err != nil {
				return err
			} else if reason != authz.ReasonNone {
				return apperr.Forbid("you can only list students who are in your own scope")
			}
		}
	}
	if g.AssignmentScope == domain.ScopeListed {
		if assignmentScope != domain.ScopeListed {
			return apperr.Forbid("your own assignment scope is a list; you cannot grant every assignment")
		}
		if reason, err := authz.CheckScope(ctx, ec.Q, g, authz.Target{AssignmentIDs: assignments}); err != nil {
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
func outlastsGranter(ec *tool.ExecCtx, expiresAt *time.Time) error {
	if g := ec.Member.ExpiresAt; g != nil && (expiresAt == nil || expiresAt.After(*g)) {
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
	row, err := ec.Q.GetMemberInCourseForUpdate(ctx, dbq.GetMemberInCourseForUpdateParams{ID: memberID, CourseID: courseID})
	m := dbq.GetMemberInCourseRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, apperr.Missing("no such member in this course")
	}
	if err != nil {
		return m, err
	}
	if m.ID == ec.Member.ID {
		return m, apperr.Forbid("not on your own membership")
	}
	if m.Status == domain.MemberRemoved || (m.ExpiresAt != nil && !m.ExpiresAt.After(ec.Now)) {
		return m, apperr.Conflicts("the member has been removed; seat the actor again for a fresh start")
	}
	return m, nil
}

// shape is what a seat amounts to: levels, held over a reach, for a time.
type shape struct {
	perms                         permSet
	studentScope, assignmentScope string
	students, assignments         []uuid.UUID // the student list without the member itself
	expiresAt                     *time.Time
}

func shapeOf(ctx context.Context, q *dbq.Queries, m dbq.GetMemberInCourseRow) (shape, error) {
	s := shape{perms: memberPerms(m), studentScope: m.StudentScope, assignmentScope: m.AssignmentScope, expiresAt: m.ExpiresAt}
	var err error
	if s.students, err = q.ListStudentScope(ctx, m.ID); err != nil {
		return s, err
	}
	s.students = withoutSelf(s.students, m.ID)
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
// within the granter's own.
func grant(ctx context.Context, ec *tool.ExecCtx, before, after shape) error {
	if !after.widens(before) {
		return nil
	}
	if err := withinGranter(ctx, ec, after.perms, "", after.studentScope, after.students, after.assignmentScope, after.assignments); err != nil {
		return err
	}
	return outlastsGranter(ec, after.expiresAt)
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
			if err := ec.Q.SetMemberPerms(ctx, dbq.SetMemberPermsParams{ID: m.ID,
				PermDocumentRead: after.perms.col(domain.PermDocumentRead), PermDocumentReadDraft: after.perms.col(domain.PermDocumentReadDraft),
				PermDocumentWrite: after.perms.col(domain.PermDocumentWrite), PermRubricRead: after.perms.col(domain.PermRubricRead),
				PermAssignmentWrite: after.perms.col(domain.PermAssignmentWrite), PermSubmissionRead: after.perms.col(domain.PermSubmissionRead),
				PermSubmissionWrite: after.perms.col(domain.PermSubmissionWrite), PermGradeRead: after.perms.col(domain.PermGradeRead),
				PermGradeSubmit: after.perms.col(domain.PermGradeSubmit), PermGradePost: after.perms.col(domain.PermGradePost),
				PermMemberRead: after.perms.col(domain.PermMemberRead), PermMemberManage: after.perms.col(domain.PermMemberManage),
				PermActionDecide: after.perms.col(domain.PermActionDecide),
			}); err != nil {
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
				after.students = withoutSelf(dedupe(students), m.ID)
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

// withoutSelf drops a member's own id from its student list before the list
// is measured against the granter's scope: a student listing themselves is
// the normal case, not a grant.
func withoutSelf(ids []uuid.UUID, self uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id != self {
			out = append(out, id)
		}
	}
	return out
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
