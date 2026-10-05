package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// A group's work (docs/schema.md §2.5, Group work). On a group assignment
// each group hands in one submission per attempt, owned by the group; whose
// work it is — its students — is the group's members now while it is a
// draft, and, once handed in or recorded missing, the members it was handed
// in for, frozen then (submission_member). A student is part of one group's
// work for an assignment: a hand-in leaves out, and names, a member another
// group's work for it names already.

const (
	// How a student came to be part of a group's work.
	addedHandIn, addedMissing, addedCorrected = "hand_in", "missing", "corrected"

	EventSubmissionMembersChanged = "submission.members_changed"
)

// workStudents are the students of a submission (submission_students): its
// student; a group's draft's members now; a group's work's members as it was
// handed in or recorded missing.
func workStudents(ctx context.Context, q dbq.Querier, submission uuid.UUID) ([]uuid.UUID, error) {
	return q.SubmissionStudents(ctx, submission)
}

// workScope is a submission as a target of reading or writing it: a
// student's, its student; a group's, any of its students, so that each
// member's own seat reaches it.
func workScope(ctx context.Context, q dbq.Querier, s dbq.GetSubmissionInCourseRow) (authz.Target, error) {
	t := authz.Target{AssignmentIDs: []uuid.UUID{s.AssignmentID}}
	if s.StudentMemberID != nil {
		t.StudentMemberIDs = []uuid.UUID{*s.StudentMemberID}
		return t, nil
	}
	students, err := workStudents(ctx, q, s.ID)
	t.AnyStudents, t.AnyStudentMemberIDs = true, students
	return t, err
}

// everyStudentScope is a submission as a target of grading it, or of
// correcting what it is: every one of its students, since what is done lands
// on each.
func everyStudentScope(ctx context.Context, q dbq.Querier, s dbq.GetSubmissionInCourseRow) (authz.Target, error) {
	students, err := workStudents(ctx, q, s.ID)
	return authz.Target{StudentMemberIDs: students, AssignmentIDs: []uuid.UUID{s.AssignmentID}}, err
}

// emitToStudents files ev once under each of students: the feed's scope then
// shows each member what is theirs, and no group what is another's.
func emitToStudents(ec *tool.ExecCtx, students []uuid.UUID, ev events.Event) {
	for _, st := range students {
		e := ev
		id := st
		e.StudentMemberID = &id
		ec.Emit(e)
	}
}

// emitWork files ev about submission s under its students: its student, or
// each of a group's, the payload saying the group.
func emitWork(ctx context.Context, ec *tool.ExecCtx, s uuid.UUID, group *uuid.UUID, ev events.Event) error {
	students, err := workStudents(ctx, ec.Q, s)
	if err != nil {
		return err
	}
	if group != nil {
		payload := map[string]any{"group_id": *group}
		for k, v := range ev.Payload {
			payload[k] = v
		}
		ev.Payload = payload
	}
	emitToStudents(ec, students, ev)
	return nil
}

// errNoGroup refuses a student's work on a group assignment while they are in
// no group of its set, saying the set and whether they may sign up.
func errNoGroup(set dbq.GroupSet) error {
	e := apperr.Precondition("you are in no group of this assignment's group set: sign up to one while sign-up is open, or ask the teacher to place you").
		With("reason", ReasonNoGroup).With("set_id", set.ID).With("signup_open", set.SignupOpen && set.ArchivedAt == nil)
	if set.SignupClosesAt != nil {
		e = e.With("signup_closes_at", *set.SignupClosesAt)
	}
	return e
}

var (
	errNotAGroupAssignment = apperr.Precondition("this assignment is not a group assignment: work is each student's own").
				With("reason", ReasonNotAGroupAssignment)
	errGroupAssignment = apperr.Precondition("this is a group assignment: name the group (group_id), whose work it is").
				With("reason", ReasonGroupAssignment)
)

func errGroupEmpty(group uuid.UUID) error {
	return apperr.Precondition("the group has nobody to hand work in for").With("reason", ReasonGroupEmpty).With("group_id", group)
}

// LeftOut is a member a group's work was not handed in for, because another
// group's work for the assignment names them already.
type LeftOut struct {
	MemberID     uuid.UUID `json:"member_id"`
	SubmissionID uuid.UUID `json:"submission_id" jsonschema:"the other group's work that names them"`
}

// membersFor is whom group's work for assignment is handed in, or recorded
// missing, for: its members now, less those another group's work for the
// assignment names, who are left out. Taken under the assignment's work
// lock (LockWorkMembersOfAssignment) it is what is then written.
func membersFor(ctx context.Context, q dbq.Querier, assignment, group uuid.UUID) (members []uuid.UUID, left []LeftOut, err error) {
	live, err := q.LiveMembersOf(ctx, group)
	if err != nil || len(live) == 0 {
		return nil, nil, err
	}
	other, err := q.OtherWorkNaming(ctx, dbq.OtherWorkNamingParams{AssignmentID: assignment, MemberIds: live, GroupID: &group})
	if err != nil {
		return nil, nil, err
	}
	named := map[uuid.UUID]bool{}
	left = []LeftOut{}
	for _, o := range other {
		named[o.MemberID] = true
		left = append(left, LeftOut{MemberID: o.MemberID, SubmissionID: o.SubmissionID})
	}
	for _, m := range live {
		if !named[m] {
			members = append(members, m)
		}
	}
	return members, left, nil
}

// sameMembers: the same students, in any order.
func sameMembers(a, b []uuid.UUID) bool {
	a, b = slices.Clone(dedupe(a)), slices.Clone(dedupe(b))
	cmp := func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) }
	slices.SortFunc(a, cmp)
	slices.SortFunc(b, cmp)
	return slices.Equal(a, b)
}

var errMembersChanged = apperr.Conflicts("the group's members have changed since this was proposed; look again, and propose it again").
	With("reason", ReasonMembersChanged)

// groupOf is the group of the set a student counts as a member of now.
func groupOf(ctx context.Context, q dbq.Querier, set, student uuid.UUID) (*dbq.CurrentGroupOfRow, error) {
	g, err := q.CurrentGroupOf(ctx, dbq.CurrentGroupOfParams{SetID: set, MemberID: student})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// WorkMember is one of the students whose work a submission is.
type WorkMember struct {
	MemberID    uuid.UUID `json:"member_id"`
	DisplayName *string   `json:"display_name,omitempty" jsonschema:"to the work's own members, and to those who may read the member list"`
}

// workMembers is a submission's students as a reader is shown them: their
// names to the work's own members and to readers of the member list.
func workMembers(ctx context.Context, q dbq.Querier, m *domain.Member, submission uuid.UUID) ([]WorkMember, error) {
	students, err := workStudents(ctx, q, submission)
	if err != nil {
		return nil, err
	}
	names := m.Perm(domain.PermMemberRead).Allowed() || slices.Contains(students, selfOf(m))
	out := make([]WorkMember, 0, len(students))
	if !names {
		for _, st := range students {
			out = append(out, WorkMember{MemberID: st})
		}
		return out, nil
	}
	rows, err := q.NamesOfMembers(ctx, students)
	for _, r := range rows {
		name := r.DisplayName
		out = append(out, WorkMember{MemberID: r.ID, DisplayName: &name})
	}
	return out, err
}

// ---------------------------------------------------------------------------
// submission.set_members
// ---------------------------------------------------------------------------

type SubmissionSetMembersIn struct {
	tool.InCourse
	SubmissionID uuid.UUID   `json:"submission_id" jsonschema:"a group's work handed in or recorded missing"`
	Add          []uuid.UUID `json:"add,omitempty" jsonschema:"students to make part of it: current students of the course whom no other group's work for the assignment names"`
	Remove       []uuid.UUID `json:"remove,omitempty" jsonschema:"members to take off it: none with a grade entered or proposed on it"`
}

type SubmissionSetMembersOut struct {
	Members []uuid.UUID `json:"members" jsonschema:"whose work it is now"`
}

// members is whose work it is once in is applied to now's.
func (in SubmissionSetMembersIn) members(now []uuid.UUID) []uuid.UUID {
	out := []uuid.UUID{}
	for _, m := range append(slices.Clone(now), in.Add...) {
		if !slices.Contains(in.Remove, m) && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

func submissionSetMembers() tool.Tool {
	return tool.Define(tool.Spec[SubmissionSetMembersIn, SubmissionSetMembersOut]{
		Name: "submission.set_members",
		Description: "Correct whose work a group's submission is, once it is handed in or recorded missing: add a student " +
			"the group handed it in without (one the teacher forgot to place, say), whom no other group's work for the " +
			"assignment names (part_of_other_work), or take off a member with no grade entered or proposed on it " +
			"(member_graded). At least one member stays (group_empty). Gated as correcting lateness is: who handed work in " +
			"with whom is not a student's to declare. It reaches every member before and after.",
		Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}/members"},
		Check: func(in SubmissionSetMembersIn) error {
			if len(in.Add) == 0 && len(in.Remove) == 0 {
				return apperr.Invalid("give add or remove, or both")
			}
			for _, a := range in.Add {
				if slices.Contains(in.Remove, a) {
					return apperr.Invalid("member %s is both added and removed", a)
				}
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionSetMembersIn) (tool.Target, error) {
			s, err := q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return tool.Target{}, apperr.Missing("no such submission in this course")
			}
			if err != nil {
				return tool.Target{}, err
			}
			scope, err := everyStudentScope(ctx, q, s)
			scope.StudentMemberIDs = dedupe(append(scope.StudentMemberIDs, in.Add...))
			return tool.Target{CourseID: in.CourseID, Type: "submission", ID: &in.SubmissionID, Scope: scope}, err
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in SubmissionSetMembersIn) error {
			s, err := q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return workGone(err)
			}
			_, err = in.correctable(ctx, q, s)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionSetMembersIn) (SubmissionSetMembersOut, error) {
			s, err := ec.Q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionSetMembersOut{}, workGone(err)
			}
			// As a hand-in takes them: the assignment, the work, and then the
			// assignment's work lock.
			if _, err := ec.Q.GetAssignmentForSubmission(ctx, dbq.GetAssignmentForSubmissionParams{ID: s.AssignmentID, CourseID: in.CourseID}); err != nil {
				return SubmissionSetMembersOut{}, goneIfNoRows(ctx, ec.Q, in.CourseID, s.AssignmentID, err)
			}
			if _, err := ec.Q.GetSubmissionFullForUpdate(ctx, dbq.GetSubmissionFullForUpdateParams{ID: s.ID, CourseID: in.CourseID}); err != nil {
				return SubmissionSetMembersOut{}, workGone(err)
			}
			if err := ec.Q.LockWorkMembersOfAssignment(ctx, s.AssignmentID); err != nil {
				return SubmissionSetMembersOut{}, err
			}
			if s, err = ec.Q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID}); err != nil {
				return SubmissionSetMembersOut{}, workGone(err)
			}
			now, err := in.correctable(ctx, ec.Q, s)
			if err != nil {
				return SubmissionSetMembersOut{}, err
			}
			// Every member before and after, as they are now.
			if reason, err := authz.CheckScope(ctx, ec.Q, ec.Member, authz.Target{StudentMemberIDs: dedupe(append(slices.Clone(now), in.Add...))}); err != nil {
				return SubmissionSetMembersOut{}, err
			} else if reason != authz.ReasonNone {
				return SubmissionSetMembersOut{}, apperr.Forbid("a member of the work is outside your scope").With("reason", string(reason))
			}
			var added, removed []uuid.UUID
			for _, a := range in.Add {
				if !slices.Contains(now, a) {
					added = append(added, a)
				}
			}
			for _, r := range in.Remove {
				if slices.Contains(now, r) {
					removed = append(removed, r)
				}
			}
			if len(added) > 0 {
				if err := ec.Q.InsertSubmissionMembers(ctx, dbq.InsertSubmissionMembersParams{SubmissionID: s.ID, CourseID: in.CourseID,
					AssignmentID: s.AssignmentID, AddedAt: ec.Now, AddedHow: addedCorrected, AddedByMemberID: &ec.Member.ID,
					MemberIds: added}); err != nil {
					return SubmissionSetMembersOut{}, err
				}
			}
			if len(removed) > 0 {
				if err := ec.Q.BeginMemberCorrection(ctx, s.ID); err != nil {
					return SubmissionSetMembersOut{}, err
				}
				if _, err := ec.Q.DeleteSubmissionMembers(ctx, dbq.DeleteSubmissionMembersParams{SubmissionID: s.ID, MemberIds: removed}); err != nil {
					return SubmissionSetMembersOut{}, err
				}
				if err := ec.Q.EndMemberCorrection(ctx); err != nil {
					return SubmissionSetMembersOut{}, err
				}
			}
			ev := events.Event{Type: EventSubmissionMembersChanged, CourseID: &in.CourseID, SubjectType: "submission", SubjectID: &s.ID,
				AssignmentID: &s.AssignmentID}
			ev.Payload = map[string]any{"group_id": s.GroupID, "change": "added"}
			emitToStudents(ec, added, ev)
			ev.Payload = map[string]any{"group_id": s.GroupID, "change": "removed"}
			emitToStudents(ec, removed, ev)
			return SubmissionSetMembersOut{Members: in.members(now)}, nil
		},
	})
}

// correctable refuses in's correction of submission s, and returns whose
// work it is now: s must be a group's, handed in or recorded missing; each
// student added a current student whom no other group's work for the
// assignment names; none removed graded, or with a grade proposed; and
// somebody left.
func (in SubmissionSetMembersIn) correctable(ctx context.Context, q dbq.Querier, s dbq.GetSubmissionInCourseRow) ([]uuid.UUID, error) {
	if s.GroupID == nil {
		return nil, apperr.Precondition("a student's own work is theirs alone").With("reason", ReasonNotAGroupAssignment)
	}
	if s.State == stateDraft {
		return nil, apperr.Precondition("the work is a draft: whose it is is its group's members now; place students in the group instead")
	}
	now, err := workStudents(ctx, q, s.ID)
	if err != nil {
		return nil, err
	}
	if len(in.Add) > 0 {
		if err := liveStudents(ctx, q, s.CourseID, in.Add); err != nil {
			return nil, err
		}
		other, err := q.OtherWorkNaming(ctx, dbq.OtherWorkNamingParams{AssignmentID: s.AssignmentID, MemberIds: in.Add, GroupID: s.GroupID})
		if err != nil {
			return nil, err
		}
		if len(other) > 0 {
			return nil, apperr.Precondition("another group's work for this assignment names that student").
				With("reason", ReasonPartOfOtherWork).With("member_id", other[0].MemberID).With("submission_id", other[0].SubmissionID)
		}
	}
	if len(in.Remove) > 0 {
		graded, err := q.MemberGradedOnSubmission(ctx, dbq.MemberGradedOnSubmissionParams{SubmissionID: &s.ID, MemberIds: in.Remove})
		if err != nil {
			return nil, err
		}
		if len(graded) > 0 {
			return nil, apperr.Precondition("that member has a grade on this work, which stays theirs").
				With("reason", ReasonMemberGraded).With("member_id", graded[0])
		}
		proposed, err := q.GradeProposedOnSubmission(ctx, &s.ID)
		if err != nil {
			return nil, err
		}
		if proposed {
			return nil, apperr.Precondition("a grade proposed for this work waits for a decision; decide it first").
				With("reason", ReasonMemberGraded)
		}
	}
	if len(in.members(now)) == 0 {
		return nil, errGroupEmpty(*s.GroupID)
	}
	return now, nil
}
