package tools

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

func submissionTools() []tool.Tool {
	return []tool.Tool{submissionList(), submissionRoster(), submissionGet(), submissionCreate(), submissionUpdateDraft(),
		submissionSubmit(), submissionSetLateness(), submissionRecordMissing(), submissionSetMembers()}
}

var (
	readSubmissions  = tool.Gate{Perms: []domain.Perm{domain.PermSubmissionRead}}
	writeSubmissions = tool.Gate{Perms: []domain.Perm{domain.PermSubmissionWrite}}
)

const (
	EventSubmissionSubmitted = "submission.submitted"
	EventSubmissionLateness  = "submission.lateness_changed"
	EventSubmissionMissing   = "submission.missing"

	stateDraft, stateSubmitted, stateLate, stateMissing = "draft", "submitted", "late", "missing"
)

type SubmissionView struct {
	ID                    uuid.UUID    `json:"id"`
	AssignmentID          uuid.UUID    `json:"assignment_id"`
	StudentMemberID       *uuid.UUID   `json:"student_member_id,omitempty" jsonschema:"whose work it is, for a student's own; absent for a group's"`
	GroupID               *uuid.UUID   `json:"group_id,omitempty" jsonschema:"the group whose work it is, on a group assignment"`
	GroupName             *string      `json:"group_name,omitempty"`
	Members               []WorkMember `json:"members,omitempty" jsonschema:"a group's work: whose it is — its group's members now while it is a draft, and those it was handed in or recorded missing for once it is not"`
	Attempt               int32        `json:"attempt"`
	Body                  *string      `json:"body,omitempty"`
	InstructionsVersionID *uuid.UUID   `json:"instructions_version_id,omitempty" jsonschema:"the exact version of the instructions in force when it was submitted"`
	State                 string       `json:"state" jsonschema:"draft, submitted, late or missing"`
	SubmittedAt           *time.Time   `json:"submitted_at,omitempty"`
	SubmittedByMemberID   *uuid.UUID   `json:"submitted_by_member_id,omitempty" jsonschema:"the seat whose hand-in it was"`
	Revision              int32        `json:"revision" jsonschema:"counts each change of a draft's text: name it as base_revision in submission.update_draft"`
	RevisedAt             *time.Time   `json:"revised_at,omitempty"`
	RevisedByMemberID     *uuid.UUID   `json:"revised_by_member_id,omitempty"`
	CreatedAt             time.Time    `json:"created_at"`
	Files                 []FileRef    `json:"files,omitempty" jsonschema:"submitted files; read each with document.get"`
}

type SubmissionListIn struct {
	tool.InCourse
	AssignmentID    *uuid.UUID `json:"assignment_id,omitempty"`
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty" jsonschema:"the work this student is part of: their own, or a group's"`
	GroupID         *uuid.UUID `json:"group_id,omitempty" jsonschema:"a group's work"`
	Page
}

type SubmissionListOut struct {
	Submissions []SubmissionView `json:"submissions"`
	Next        *uuid.UUID       `json:"next,omitempty"`
}

func submissionList() tool.Tool {
	return tool.Define(tool.Spec[SubmissionListIn, SubmissionListOut]{
		Name: "submission.list",
		Description: "Submissions in the course that fall within the caller's scope: a student sees their own, a tutor those " +
			"of the students they are listed for, a grader those for the assignments they are listed for. Bodies are left " +
			"out of the list; read one with submission.get.",
		Kind: tool.Read, Gate: readSubmissions,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/submissions"},
		Resolve: func(_ context.Context, _ dbq.Querier, in SubmissionListIn) (tool.Target, error) {
			// No scope on the target: the list is filtered row by row in SQL
			// instead, so asking for "everything I may see" is never denied.
			return tool.Target{CourseID: in.CourseID, Type: "submission"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in SubmissionListIn) (SubmissionListOut, error) {
			rows, err := rc.Q.ListSubmissions(ctx, dbq.ListSubmissionsParams{
				CourseID: in.CourseID, After: in.after(), MaxRows: in.limit(),
				AssignmentID: in.AssignmentID, StudentMemberID: in.StudentMemberID, GroupID: in.GroupID,
				StudentAll: rc.Scope.StudentAll, AssignmentAll: rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID,
				PrincipalID: rc.Scope.PrincipalID, PrincipalStudentAll: rc.Scope.PrincipalStudentAll, PrincipalAssignmentAll: rc.Scope.PrincipalAssignmentAll,
			})
			if err != nil {
				return SubmissionListOut{}, err
			}
			out := SubmissionListOut{Submissions: make([]SubmissionView, 0, len(rows))}
			for _, r := range rows {
				v := SubmissionView{ID: r.ID, AssignmentID: r.AssignmentID, StudentMemberID: r.StudentMemberID, GroupID: r.GroupID,
					GroupName: r.GroupName, Attempt: r.Attempt, State: r.State, SubmittedAt: r.SubmittedAt, CreatedAt: r.CreatedAt,
					SubmittedByMemberID: r.SubmittedByMemberID, Revision: r.Revision, RevisedAt: r.RevisedAt, RevisedByMemberID: r.RevisedByMemberID}
				if r.GroupID != nil {
					if v.Members, err = workMembers(ctx, rc.Q, rc.Member, r.ID); err != nil {
						return SubmissionListOut{}, err
					}
				}
				out.Submissions = append(out.Submissions, v)
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, err
		},
	})
}

type SubmissionRosterIn struct {
	tool.InCourse
	AssignmentID uuid.UUID `json:"assignment_id"`
	Page
}

type RosterEntry struct {
	StudentMemberID uuid.UUID  `json:"student_member_id"`
	DisplayName     *string    `json:"display_name,omitempty" jsonschema:"only for a caller who may read the member list (perm_member_read)"`
	MemberStatus    *string    `json:"member_status,omitempty" jsonschema:"active or paused; only for a caller who may read the member list"`
	GroupID         *uuid.UUID `json:"group_id,omitempty" jsonschema:"on a group assignment, the student's group in its set now"`
	State           string     `json:"state" jsonschema:"not_started (no submission at all), draft, submitted, late or missing: the latest attempt's; on a group assignment, of the work the student is part of, or their group's draft, and no_group for a student in no group of its set"`
	SubmissionID    *uuid.UUID `json:"submission_id,omitempty" jsonschema:"the latest attempt; absent when not started"`
	Attempt         *int32     `json:"attempt,omitempty"`
	SubmittedAt     *time.Time `json:"submitted_at,omitempty"`
}

// RosterGroup is where one group stands on a group assignment.
type RosterGroup struct {
	GroupID             uuid.UUID    `json:"group_id"`
	Name                string       `json:"name"`
	Members             []WorkMember `json:"members" jsonschema:"its members now that the caller's student scope reaches; names to those who may read the member list"`
	State               string       `json:"state" jsonschema:"not_started, draft, submitted, late or missing: its latest attempt's that the caller may read, not_started for none"`
	SubmissionID        *uuid.UUID   `json:"submission_id,omitempty"`
	Attempt             *int32       `json:"attempt,omitempty"`
	SubmittedAt         *time.Time   `json:"submitted_at,omitempty"`
	SubmittedByMemberID *uuid.UUID   `json:"submitted_by_member_id,omitempty"`
}

type SubmissionRosterOut struct {
	Students []RosterEntry `json:"students"`
	// Groups is a group assignment's, on the first page.
	Groups []RosterGroup `json:"groups,omitempty" jsonschema:"on a group assignment, the first page only: each group of its set, not archived, with a member the caller's scope reaches, and where its latest work the caller may read stands; a member is shown the group's draft and the attempts they are part of"`
	Next   *uuid.UUID    `json:"next,omitempty"`
}

const (
	stateNotStarted = "not_started"
	stateNoGroup    = "no_group"
)

func submissionRoster() tool.Tool {
	return tool.Define(tool.Spec[SubmissionRosterIn, SubmissionRosterOut]{
		Name: "submission.roster",
		Description: "Where every student stands on one assignment: each current student within the caller's scope, with " +
			"their latest attempt's state, including those who have not started, whom submission.list cannot show. " +
			"A student who has not started can be recorded as having handed in nothing with submission.record_missing. " +
			"Names and seat status come only to a caller who may read the member list; to anyone else a student is " +
			"their member id, as in submission.list.",
		Kind: tool.Read, Gate: readSubmissions,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/roster"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionRosterIn) (tool.Target, error) {
			// The assignment is checked against the caller's assignment
			// scope here; the students are filtered by student scope in SQL.
			return assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in SubmissionRosterIn) (SubmissionRosterOut, error) {
			a, err := rc.Q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionRosterOut{}, goneIfNoRows(ctx, rc.Q, in.CourseID, in.AssignmentID, err)
			}
			if a.PublishedAt == nil && !canSeeUnpublished(rc.Member) {
				// As in assignment.get: to this caller it does not exist yet.
				return SubmissionRosterOut{}, apperr.Missing("no such assignment in this course")
			}
			if a.GroupSetID != nil {
				return groupRoster(ctx, rc, in, *a.GroupSetID)
			}
			rows, err := rc.Q.ListAssignmentRoster(ctx, dbq.ListAssignmentRosterParams{
				CourseID: in.CourseID, AssignmentID: in.AssignmentID, After: in.after(), MaxRows: in.limit(),
				StudentAll: rc.Scope.StudentAll, MemberID: rc.Scope.MemberID,
				PrincipalID: rc.Scope.PrincipalID, PrincipalStudentAll: rc.Scope.PrincipalStudentAll,
			})
			out := SubmissionRosterOut{Students: make([]RosterEntry, 0, len(rows))}
			// Names and seat status are the member list's: submission_read
			// alone gets member ids, as submission.list gives.
			members := rc.Member.Perm(domain.PermMemberRead).Allowed()
			for _, r := range rows {
				e := RosterEntry{StudentMemberID: r.StudentMemberID,
					State: stateNotStarted, SubmissionID: r.SubmissionID, Attempt: r.Attempt, SubmittedAt: r.SubmittedAt}
				if members {
					e.DisplayName, e.MemberStatus = &r.DisplayName, &r.MemberStatus
				}
				if r.State != nil {
					e.State = *r.State
				}
				out.Students = append(out.Students, e)
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].StudentMemberID
			}
			return out, err
		},
	})
}

// groupRoster is submission.roster on a group assignment of set: each
// student with their group and where the work they are part of stands, and,
// on the first page, each group.
func groupRoster(ctx context.Context, rc *tool.ReadCtx, in SubmissionRosterIn, set uuid.UUID) (SubmissionRosterOut, error) {
	rows, err := rc.Q.ListGroupAssignmentRoster(ctx, dbq.ListGroupAssignmentRosterParams{
		SetID: set, CourseID: in.CourseID, AssignmentID: in.AssignmentID, After: in.after(), MaxRows: in.limit(),
		StudentAll: rc.Scope.StudentAll, MemberID: rc.Scope.MemberID,
		PrincipalID: rc.Scope.PrincipalID, PrincipalStudentAll: rc.Scope.PrincipalStudentAll,
	})
	if err != nil {
		return SubmissionRosterOut{}, err
	}
	members := rc.Member.Perm(domain.PermMemberRead).Allowed()
	out := SubmissionRosterOut{Students: make([]RosterEntry, 0, len(rows))}
	for _, r := range rows {
		e := RosterEntry{StudentMemberID: r.StudentMemberID, GroupID: r.GroupID, State: stateNotStarted}
		if members {
			e.DisplayName, e.MemberStatus = &r.DisplayName, &r.MemberStatus
		}
		// The work they are part of, unless their group has a newer attempt
		// open: a draft is theirs to write now.
		newerDraft := r.DraftSubmissionID != nil && r.PartSubmissionID != nil && sameID(r.PartGroupID, r.GroupID) &&
			*r.DraftAttempt > *r.PartAttempt
		switch {
		case r.PartSubmissionID != nil && !newerDraft:
			e.SubmissionID, e.Attempt, e.State, e.SubmittedAt = r.PartSubmissionID, r.PartAttempt, *r.PartState, r.PartSubmittedAt
		case r.DraftSubmissionID != nil:
			e.SubmissionID, e.Attempt, e.State = r.DraftSubmissionID, r.DraftAttempt, stateDraft
		case r.GroupID == nil:
			e.State = stateNoGroup
		}
		out.Students = append(out.Students, e)
	}
	if len(rows) > 0 && len(rows) == int(in.limit()) {
		out.Next = &rows[len(rows)-1].StudentMemberID
	}
	if in.After != nil {
		return out, nil
	}
	// Where each group's work stands, of the work the caller may read: a
	// member is shown the group's draft and the attempts they are part of,
	// and not one handed in before they joined.
	groups, err := rc.Q.ListGroupsWithLatestWork(ctx, dbq.ListGroupsWithLatestWorkParams{AssignmentID: in.AssignmentID, SetID: set,
		StudentAll: rc.Scope.StudentAll, MemberID: rc.Scope.MemberID,
		PrincipalStudentAll: rc.Scope.PrincipalStudentAll, PrincipalID: rc.Scope.PrincipalID})
	if err != nil || len(groups) == 0 {
		return out, err
	}
	ids := make([]uuid.UUID, len(groups))
	for i, g := range groups {
		ids[i] = g.ID
	}
	live, err := rc.Q.ListLiveMembersOfGroups(ctx, dbq.ListLiveMembersOfGroupsParams{GroupIds: ids,
		StudentAll: rc.Scope.StudentAll, MemberID: rc.Scope.MemberID,
		PrincipalStudentAll: rc.Scope.PrincipalStudentAll, PrincipalID: rc.Scope.PrincipalID})
	if err != nil {
		return out, err
	}
	byGroup := map[uuid.UUID][]WorkMember{}
	for _, l := range live {
		wm := WorkMember{MemberID: l.MemberID}
		if members {
			name := l.DisplayName
			wm.DisplayName = &name
		}
		byGroup[l.GroupID] = append(byGroup[l.GroupID], wm)
	}
	every := rc.Scope.StudentAll && rc.Scope.PrincipalStudentAll
	for _, g := range groups {
		if len(byGroup[g.ID]) == 0 && !every {
			continue
		}
		rg := RosterGroup{GroupID: g.ID, Name: g.Name, Members: byGroup[g.ID], State: stateNotStarted, SubmissionID: g.SubmissionID,
			Attempt: g.Attempt, SubmittedAt: g.SubmittedAt, SubmittedByMemberID: g.SubmittedByMemberID}
		if rg.Members == nil {
			rg.Members = []WorkMember{}
		}
		if g.State != nil {
			rg.State = *g.State
		}
		out.Groups = append(out.Groups, rg)
	}
	return out, nil
}

type SubmissionIDIn struct {
	tool.InCourse
	SubmissionID uuid.UUID `json:"submission_id"`
}

// submissionTarget is a submission as the target of reading or writing it: a
// student's, its student's; a group's, any of its students' (workScope).
// every makes it every student's instead, for what lands on each of them:
// grading it, correcting its lateness.
func submissionTarget(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID, every ...bool) (tool.Target, error) {
	s, err := q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: id, CourseID: courseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such submission in this course")
	}
	if err != nil {
		return tool.Target{}, err
	}
	var scope authz.Target
	if len(every) > 0 && every[0] {
		scope, err = everyStudentScope(ctx, q, s)
	} else {
		scope, err = workScope(ctx, q, s)
	}
	return tool.Target{CourseID: courseID, Type: "submission", ID: &id, Scope: scope}, err
}

func submissionGet() tool.Tool {
	return tool.Define(tool.Spec[SubmissionIDIn, SubmissionView]{
		Name: "submission.get",
		Description: "One submission in full, with its text and the version of the instructions it was submitted under. A " +
			"group's work says its group and whose work it is (members), and every draft its revision, to name when " +
			"editing it.",
		Kind: tool.Read, Gate: readSubmissions,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionIDIn) (tool.Target, error) {
			return submissionTarget(ctx, q, in.CourseID, in.SubmissionID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in SubmissionIDIn) (SubmissionView, error) {
			s, err := rc.Q.GetSubmissionFull(ctx, dbq.GetSubmissionFullParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionView{}, err
			}
			v := SubmissionView{ID: s.ID, AssignmentID: s.AssignmentID, StudentMemberID: s.StudentMemberID, GroupID: s.GroupID,
				Attempt: s.Attempt, Body: s.Body, InstructionsVersionID: s.InstructionsVersionID, State: s.State, SubmittedAt: s.SubmittedAt,
				SubmittedByMemberID: s.SubmittedByMemberID, Revision: s.Revision, RevisedAt: s.RevisedAt, RevisedByMemberID: s.RevisedByMemberID,
				CreatedAt: s.CreatedAt}
			if s.GroupID != nil {
				g, err := loadGroup(ctx, rc.Q, in.CourseID, *s.GroupID)
				if err != nil {
					return SubmissionView{}, err
				}
				v.GroupName = &g.Name
				if v.Members, err = workMembers(ctx, rc.Q, rc.Member, s.ID); err != nil {
					return SubmissionView{}, err
				}
			}
			files, err := rc.Q.ListSubmissionDocuments(ctx, &s.ID)
			for _, f := range files {
				v.Files = append(v.Files, FileRef{DocumentID: f.ID, Title: f.Title})
			}
			return v, err
		},
	})
}

type SubmissionCreateIn struct {
	tool.InCourse
	AssignmentID    uuid.UUID  `json:"assignment_id"`
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty" jsonschema:"whose work this is; defaults to the caller. On a group assignment, the work of this student's group"`
	GroupID         *uuid.UUID `json:"group_id,omitempty" jsonschema:"on a group assignment, the group whose work this starts; defaults to the group of the student named, or of the caller"`
	Body            *string    `json:"body,omitempty"`
}

type SubmissionCreateOut struct {
	SubmissionID uuid.UUID  `json:"submission_id"`
	Attempt      int32      `json:"attempt"`
	GroupID      *uuid.UUID `json:"group_id,omitempty" jsonschema:"the group whose work it is, on a group assignment"`
}

// studentOf: the caller, unless they name someone else. Whether they may act
// for someone else is for scope to decide, like everything else.
func (in SubmissionCreateIn) studentOf(m *domain.Member) uuid.UUID {
	if in.StudentMemberID != nil {
		return *in.StudentMemberID
	}
	return m.ID
}

var errNoAssignment = apperr.Missing("no such assignment in this course")

// errOpenDraft refuses a new attempt while draft is open.
func errOpenDraft(draft uuid.UUID) error {
	return apperr.Conflicts("there is already an open draft; edit or submit that one").With("submission_id", draft)
}

// submitter is whose work a new attempt in starts, made by m, if that is a
// current student m's scope reaches. submission.create asks it before a
// proposal is queued (Validate), and again as it starts it.
func submitter(ctx context.Context, q dbq.Querier, m *domain.Member, in SubmissionCreateIn) (uuid.UUID, error) {
	student := in.studentOf(m)
	if in.StudentMemberID == nil {
		// A student's scope normally lists themselves. If it has been
		// emptied, it reaches nobody, themselves included: scope
		// fails closed here as everywhere.
		if reason, err := authz.CheckScope(ctx, q, m, authz.Target{StudentMemberIDs: []uuid.UUID{student}}); err != nil {
			return student, err
		} else if reason != authz.ReasonNone {
			return student, apperr.Forbid("you are outside your own student scope").With("reason", string(reason))
		}
	}
	// The composite key proves the member is in this course. That
	// they are a student is ours to check: work is handed in by, and
	// grades are given to, people on the roster as students.
	entry, err := q.GetRosterEntry(ctx, dbq.GetRosterEntryParams{ID: student, CourseID: in.CourseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return student, apperr.Missing("no such member in this course")
	}
	if err != nil {
		return student, err
	}
	if entry.Role != "student" || entry.Status != domain.MemberActive {
		return student, apperr.Precondition("work is submitted by, or for, a current student of the course")
	}
	return student, nil
}

// workGroup is the group whose work a new attempt in starts, made by m, on
// an assignment of set: the group named, or the group of the student named,
// or of the caller's own (a student's agent's, its student's). It is not
// archived, has members, and m's scope reaches one of them. submission.create
// asks it before a proposal is queued, and again as it starts it.
func workGroup(ctx context.Context, q dbq.Querier, m *domain.Member, in SubmissionCreateIn, setID uuid.UUID) (dbq.GetGroupRow, []uuid.UUID, error) {
	var g dbq.GetGroupRow
	set, err := loadSet(ctx, q, in.CourseID, setID)
	if err != nil {
		return g, nil, err
	}
	id := in.GroupID
	if id == nil {
		student := selfOf(m)
		if in.StudentMemberID != nil {
			student = *in.StudentMemberID
		}
		mine, err := groupOf(ctx, q, setID, student)
		if err != nil {
			return g, nil, err
		}
		if mine == nil {
			return g, nil, errNoGroup(set)
		}
		id = &mine.ID
	}
	if g, err = loadGroup(ctx, q, in.CourseID, *id); err != nil {
		return g, nil, err
	}
	if g.SetID != setID {
		return g, nil, apperr.Missing("no such group in this assignment's group set")
	}
	if g.ArchivedAt != nil {
		return g, nil, errGroupArchived(g.ID)
	}
	members, err := q.LiveMembersOf(ctx, g.ID)
	if err != nil {
		return g, nil, err
	}
	if len(members) == 0 {
		return g, nil, errGroupEmpty(g.ID)
	}
	if reason, err := authz.CheckScope(ctx, q, m, authz.AnyOf(members)); err != nil {
		return g, nil, err
	} else if reason != authz.ReasonNone {
		return g, nil, apperr.Forbid("the group's members are outside your scope").With("reason", string(reason))
	}
	return g, members, nil
}

func submissionCreate() tool.Tool {
	return tool.Define(tool.Spec[SubmissionCreateIn, SubmissionCreateOut]{
		Name: "submission.create",
		Description: "Start a draft submission to a published assignment. A draft can be edited freely; nothing is handed in " +
			"until submission.submit. Once an attempt has been submitted it never changes, and submitting again means " +
			"creating a new attempt here. On a group assignment it starts the group's work: the caller's group, or the " +
			"group named; a student in no group of its set is refused (no_group), saying whether they may sign up.",
		Kind: tool.Write, Gate: writeSubmissions,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions"},
		Check: func(in SubmissionCreateIn) error {
			if in.GroupID != nil && in.StudentMemberID != nil {
				return apperr.Invalid("give group_id or student_member_id, not both")
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionCreateIn) (tool.Target, error) {
			t, err := assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
			if err != nil {
				return t, err
			}
			t.Type, t.ID = "submission", nil
			a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return t, err
			}
			switch {
			case a.GroupSetID != nil && in.GroupID != nil:
				// The group's work: any of its members.
				members, err := q.LiveMembersOf(ctx, *in.GroupID)
				if err != nil {
					return t, err
				}
				t.Scope.AnyStudents, t.Scope.AnyStudentMemberIDs = true, members
			case in.StudentMemberID != nil:
				t.Scope.StudentMemberIDs = []uuid.UUID{*in.StudentMemberID}
			}
			// When the student is the caller, who that is is not known until
			// the call runs; Execute checks their scope itself.
			return t, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, _ time.Time, in SubmissionCreateIn) error {
			a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return goneIfNoRows(ctx, q, in.CourseID, in.AssignmentID, err)
			}
			if a.PublishedAt == nil {
				return errNoAssignment
			}
			if a.GroupSetID == nil {
				if in.GroupID != nil {
					return errNotAGroupAssignment
				}
				student, err := submitter(ctx, q, m, in)
				if err != nil {
					return err
				}
				prior, err := q.ListSubmissionsOf(ctx, dbq.ListSubmissionsOfParams{AssignmentID: a.ID, StudentMemberID: student})
				if err != nil {
					return err
				}
				if len(prior) > 0 && prior[0].State == stateDraft {
					return errOpenDraft(prior[0].ID)
				}
				return nil
			}
			g, _, err := workGroup(ctx, q, m, in, *a.GroupSetID)
			if err != nil {
				return err
			}
			prior, err := q.ListGroupSubmissionsOf(ctx, dbq.ListGroupSubmissionsOfParams{AssignmentID: a.ID, GroupID: g.ID})
			if err != nil {
				return err
			}
			if len(prior) > 0 && prior[0].State == stateDraft {
				return errOpenDraft(prior[0].ID)
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionCreateIn) (SubmissionCreateOut, error) {
			a, err := ec.Q.GetAssignmentForSubmission(ctx, dbq.GetAssignmentForSubmissionParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionCreateOut{}, goneIfNoRows(ctx, ec.Q, in.CourseID, in.AssignmentID, err)
			}
			if a.PublishedAt == nil {
				return SubmissionCreateOut{}, errNoAssignment
			}
			if a.GroupSetID != nil {
				return createGroupWork(ctx, ec, in, dbq.GetAssignmentInCourseRow(a))
			}
			if in.GroupID != nil {
				return SubmissionCreateOut{}, errNotAGroupAssignment
			}
			student, err := submitter(ctx, ec.Q, ec.Member, in)
			if err != nil {
				return SubmissionCreateOut{}, err
			}

			prior, err := ec.Q.LockSubmissionsOf(ctx, dbq.LockSubmissionsOfParams{AssignmentID: a.ID, StudentMemberID: student})
			if err != nil {
				return SubmissionCreateOut{}, err
			}
			attempt := int32(1)
			if len(prior) > 0 {
				switch latest := prior[0]; latest.State {
				case stateDraft:
					return SubmissionCreateOut{}, errOpenDraft(latest.ID)
				case stateMissing:
					// Late work takes the placeholder over — unless someone has
					// graded the placeholder, or proposed a grade for it that
					// nobody has decided yet. A zero "for handing in nothing"
					// is a grade of that nothing; the work a grade was given
					// for never changes underneath it. Then the placeholder
					// stays as it is, as history, and the late work is a new
					// attempt with a grade of its own to come. grade.submit
					// holds the placeholder's lock while it makes a proposal,
					// so one being made now is seen here.
					graded, err := ec.Q.SubmissionHasGrades(ctx, &latest.ID)
					if err != nil {
						return SubmissionCreateOut{}, err
					}
					if !graded {
						if err := ec.Q.ReopenMissingSubmission(ctx, dbq.ReopenMissingSubmissionParams{ID: latest.ID, Body: in.Body}); err != nil {
							return SubmissionCreateOut{}, err
						}
						return SubmissionCreateOut{SubmissionID: latest.ID, Attempt: latest.Attempt}, nil
					}
					attempt = latest.Attempt + 1
				default:
					attempt = latest.Attempt + 1
				}
			}
			id := ids.New()
			err = ec.Q.InsertSubmission(ctx, dbq.InsertSubmissionParams{ID: id, AssignmentID: a.ID, CourseID: in.CourseID,
				StudentMemberID: student, Attempt: attempt, Body: in.Body, CreatedAt: ec.Now})
			return SubmissionCreateOut{SubmissionID: id, Attempt: attempt}, err
		},
	})
}

// createGroupWork is submission.create on a group assignment: the group's
// next attempt, or its 'missing' row taken over, as a student's is.
func createGroupWork(ctx context.Context, ec *tool.ExecCtx, in SubmissionCreateIn, a dbq.GetAssignmentInCourseRow) (SubmissionCreateOut, error) {
	seen, _, err := workGroup(ctx, ec.Q, ec.Member, in, *a.GroupSetID)
	if err != nil {
		return SubmissionCreateOut{}, err
	}
	// The group FOR SHARE, as a hand-in takes it: placing students and a
	// split hold the groups they touch FOR UPDATE, and look for work under
	// it, so a move and the start of the work are one after the other.
	// Whose group it is, and whether the caller reaches its members, are
	// worked out again under it: one moved out meanwhile starts nothing in
	// the group they left.
	if _, err := ec.Q.ShareGroup(ctx, dbq.ShareGroupParams{ID: seen.ID, CourseID: in.CourseID}); err != nil {
		return SubmissionCreateOut{}, err
	}
	g, _, err := workGroup(ctx, ec.Q, ec.Member, in, *a.GroupSetID)
	if err != nil {
		return SubmissionCreateOut{}, err
	}
	if g.ID != seen.ID {
		return SubmissionCreateOut{}, errGroupChanged
	}
	out := SubmissionCreateOut{GroupID: &g.ID}
	prior, err := ec.Q.LockGroupSubmissionsOf(ctx, dbq.LockGroupSubmissionsOfParams{AssignmentID: a.ID, GroupID: g.ID})
	if err != nil {
		return out, err
	}
	out.Attempt = 1
	if len(prior) > 0 {
		switch latest := prior[0]; latest.State {
		case stateDraft:
			return out, errOpenDraft(latest.ID)
		case stateMissing:
			// As a student's: taken over unless graded or a grade is
			// proposed; taken over, it is a draft again, and whose it is is
			// its group's members now, until it is handed in.
			graded, err := ec.Q.SubmissionHasGrades(ctx, &latest.ID)
			if err != nil {
				return out, err
			}
			if !graded {
				if err := ec.Q.ReopenMissingSubmission(ctx, dbq.ReopenMissingSubmissionParams{ID: latest.ID, Body: in.Body}); err != nil {
					return out, err
				}
				if err := ec.Q.DeleteAllSubmissionMembers(ctx, latest.ID); err != nil {
					return out, err
				}
				out.SubmissionID, out.Attempt = latest.ID, latest.Attempt
				return out, nil
			}
			out.Attempt = latest.Attempt + 1
		default:
			out.Attempt = latest.Attempt + 1
		}
	}
	out.SubmissionID = ids.New()
	err = ec.Q.InsertGroupSubmission(ctx, dbq.InsertGroupSubmissionParams{ID: out.SubmissionID, AssignmentID: a.ID, CourseID: in.CourseID,
		GroupID: g.ID, Attempt: out.Attempt, Body: in.Body, CreatedAt: ec.Now})
	return out, err
}

type SubmissionUpdateDraftIn struct {
	tool.InCourse
	SubmissionID uuid.UUID `json:"submission_id"`
	Body         string    `json:"body"`
	BaseRevision *int32    `json:"base_revision,omitempty" jsonschema:"the revision of the draft this text was written over (revision, in submission.get). Required for a group's draft, which its members write together: if it has changed since, the edit is refused (draft_changed), saying what it is now. Optional on a student's own draft"`
}

type SubmissionUpdateDraftOut struct {
	OK       bool  `json:"ok"`
	Revision int32 `json:"revision" jsonschema:"the draft's revision now"`
}

// editable refuses in's edit of s: not a draft any more, or, for a group's
// draft, made over another revision than the one it is at, or over none.
func (in SubmissionUpdateDraftIn) editable(s dbq.GetSubmissionFullRow) error {
	switch {
	case s.State != stateDraft:
		return errNoLongerDraft
	case in.BaseRevision == nil && s.GroupID != nil:
		return apperr.Invalid("a group's draft is written by its members together: say which revision this edit was made over (base_revision)").
			With("reason", ReasonBaseRevisionRequired)
	case in.BaseRevision != nil && *in.BaseRevision != s.Revision:
		e := apperr.Conflicts("the draft has changed since that revision: load it, and edit what it is now").
			With("reason", ReasonDraftChanged).With("current_revision", s.Revision)
		if s.RevisedAt != nil {
			e = e.With("revised_at", *s.RevisedAt)
		}
		if s.RevisedByMemberID != nil {
			e = e.With("revised_by_member_id", *s.RevisedByMemberID)
		}
		return e
	}
	return nil
}

func submissionUpdateDraft() tool.Tool {
	return tool.Define(tool.Spec[SubmissionUpdateDraftIn, SubmissionUpdateDraftOut]{
		Name: "submission.update_draft",
		Description: "Replace the text of a draft submission. Only drafts change; a submitted attempt is frozen. Each change " +
			"counts a revision. A group's draft is written by its members together, so an edit to it names the revision " +
			"it was made over (base_revision), and is refused if the draft has changed since (draft_changed), saying what " +
			"it is now and who changed it.",
		Kind: tool.Write, Gate: writeSubmissions,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionUpdateDraftIn) (tool.Target, error) {
			return submissionTarget(ctx, q, in.CourseID, in.SubmissionID)
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in SubmissionUpdateDraftIn) error {
			sub, err := q.GetSubmissionFull(ctx, dbq.GetSubmissionFullParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return workGone(err)
			}
			return in.editable(sub)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionUpdateDraftIn) (SubmissionUpdateDraftOut, error) {
			// A group's draft with its group held, and the editor still one
			// of it (or reaching one): a member moved out meanwhile writes
			// nothing of the group they left.
			s, err := holdDraft(ctx, ec, in.CourseID, in.SubmissionID)
			if err != nil {
				return SubmissionUpdateDraftOut{}, err
			}
			if err := in.editable(s); err != nil {
				return SubmissionUpdateDraftOut{}, err
			}
			revision, err := ec.Q.UpdateSubmissionDraft(ctx, dbq.UpdateSubmissionDraftParams{ID: in.SubmissionID, Body: &in.Body,
				RevisedByMemberID: &ec.Member.ID, RevisedAt: &ec.Now})
			if errors.Is(err, pgx.ErrNoRows) {
				return SubmissionUpdateDraftOut{}, errNoLongerDraft
			}
			return SubmissionUpdateDraftOut{OK: true, Revision: revision}, err
		},
	})
}

var errNoLongerDraft = apperr.Conflicts("the submission is no longer a draft; start a new attempt to submit again")

// SubmissionSubmitIn names the draft to hand in, and may say what it is
// being handed in as. A proposal always says: Pin records it as it stands
// when the proposal is made, so that approving it hands in what was asked
// for, as of when it was asked.
type SubmissionSubmitIn struct {
	SubmissionIDIn
	Body                  *string     `json:"body,omitempty" jsonschema:"the text being handed in; if given, the draft must hold exactly this. A proposal records it, and is refused on approval if the draft has changed since"`
	Files                 []uuid.UUID `json:"files,omitzero" jsonschema:"the submitted files being handed in, by document id; as for body"`
	InstructionsVersionID *uuid.UUID  `json:"instructions_version_id,omitempty" jsonschema:"the version of the instructions the work is handed in under; if given, it must be the one students read now. A proposal records it, and is handed in under it when approved"`
	Members               []uuid.UUID `json:"members,omitzero" jsonschema:"a group's draft: the members it is handed in for, its group's now; if given, they must be. A proposal records them, and is refused on approval if the group's members have changed since (members_changed)"`
}

type SubmissionSubmitOut struct {
	State       string      `json:"state" jsonschema:"submitted, or late if the due date had passed"`
	SubmittedAt time.Time   `json:"submitted_at"`
	Members     []uuid.UUID `json:"members" jsonschema:"whose work it is, as it was handed in: its student, or the group's members now"`
	LeftOut     []LeftOut   `json:"left_out" jsonschema:"members of the group it was not handed in for: another group's work for the assignment names them. That work is named only to a caller who may read it"`
}

// sameDraft refuses to hand in anything but what the call says it is
// handing in. What it does not say is not checked. The refusal is worded
// for both ways of coming to it: a direct call that names other text or
// other files than the draft's, and an approval of a hand-in whose draft
// has changed since it was asked for.
func (in SubmissionSubmitIn) sameDraft(body *string, files []uuid.UUID) error {
	if in.Body != nil && *in.Body != textOf(body) || in.Files != nil && !sameFiles(in.Files, files) {
		return apperr.Precondition("the draft does not hold what this call says it hands in; if the hand-in waited for approval, " +
			"the draft has changed since it was asked for. Look at it, and hand it in again")
	}
	return nil
}

// sameInstructions refuses to hand the work in under instructions other than
// those students read now.
func (in SubmissionSubmitIn) sameInstructions(inForce *uuid.UUID) error {
	if in.InstructionsVersionID != nil && !sameID(in.InstructionsVersionID, inForce) {
		return apperr.Precondition("instructions_version_id is not the version of the instructions students read now")
	}
	return nil
}

// handIn refuses to hand s in as in asks: s is not a draft, holds nothing,
// or does not hold what in says it hands in. submission.submit asks it
// before a proposal is queued (Validate), and again, under the submission's
// lock, as it hands it in. It returns the draft's files.
func (in SubmissionSubmitIn) handIn(ctx context.Context, q dbq.Querier, s dbq.GetSubmissionFullRow) ([]uuid.UUID, error) {
	if s.State != stateDraft {
		return nil, apperr.Conflicts("the submission is %s, not a draft", s.State)
	}
	if s.GroupID != nil {
		members, _, err := membersFor(ctx, q, s.AssignmentID, *s.GroupID)
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			return nil, errGroupEmpty(*s.GroupID)
		}
		if in.Members != nil {
			live, err := q.LiveMembersOf(ctx, *s.GroupID)
			if err != nil {
				return nil, err
			}
			if !sameMembers(in.Members, live) {
				return nil, errMembersChanged
			}
		}
	}
	files, err := draftFiles(ctx, q, s.ID)
	if err != nil {
		return nil, err
	}
	if (s.Body == nil || *s.Body == "") && len(files) == 0 {
		return nil, apperr.Precondition("there is nothing to hand in: the draft has no text and no files")
	}
	return files, in.sameDraft(s.Body, files)
}

func textOf(body *string) string {
	if body == nil {
		return ""
	}
	return *body
}

// draftFiles is the ids of a draft's files. It is never nil: a proposal
// records "no files" as an empty list, which is not the same as not saying.
func draftFiles(ctx context.Context, q dbq.Querier, id uuid.UUID) ([]uuid.UUID, error) {
	docs, err := q.ListSubmissionDocuments(ctx, &id)
	files := make([]uuid.UUID, 0, len(docs))
	for _, d := range docs {
		files = append(files, d.ID)
	}
	return files, err
}

// sameFiles: the same documents, in any order.
func sameFiles(said, has []uuid.UUID) bool {
	said = dedupe(said)
	if len(said) != len(has) {
		return false
	}
	in := make(map[uuid.UUID]bool, len(has))
	for _, id := range has {
		in[id] = true
	}
	for _, id := range said {
		if !in[id] {
			return false
		}
	}
	return true
}

// instructionsInForce is the version of an assignment's instructions that
// students read now: the published one, or none.
func instructionsInForce(ctx context.Context, q dbq.Querier, a dbq.GetAssignmentInCourseRow) (*uuid.UUID, error) {
	if a.InstructionsDocumentID == nil {
		return nil, nil
	}
	return q.GetDocumentPublishedVersion(ctx, *a.InstructionsDocumentID)
}

func submissionSubmit() tool.Tool {
	return tool.Define(tool.Spec[SubmissionSubmitIn, SubmissionSubmitOut]{
		Name: "submission.submit",
		Description: "Hand a draft in. It is marked late if the due date has passed, the version of the instructions in " +
			"force right now is recorded with it, and from this moment it never changes. A hand-in that waits for " +
			"approval counts from when it was asked for: it is judged late or not, and recorded under the instructions " +
			"then in force, as of that moment. It hands in the draft as it was then, and is refused on approval if the " +
			"draft has changed meanwhile, so propose it only after any change to the draft that is waiting for approval " +
			"has been decided. A group's draft is handed in by any member, for the group's members now (members), " +
			"leaving out and naming (left_out) any whom another group's work for the assignment names already, and that " +
			"work only to a caller who may read it; a proposal of it is refused on approval if the members have changed.",
		Kind: tool.Write, Gate: writeSubmissions,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}/submit"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionSubmitIn) (tool.Target, error) {
			return submissionTarget(ctx, q, in.CourseID, in.SubmissionID)
		},
		// The student asks to hand in now, under the instructions they are
		// reading now. Approved on Thursday, it was still handed in on
		// Tuesday, so it must still be what it was on Tuesday.
		Pin: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in SubmissionSubmitIn) (SubmissionSubmitIn, error) {
			s, err := q.GetSubmissionFull(ctx, dbq.GetSubmissionFullParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return in, workGone(err)
			}
			if s.State != stateDraft {
				return in, apperr.Conflicts("the submission is %s, not a draft", s.State)
			}
			a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: s.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return in, goneIfNoRows(ctx, q, in.CourseID, s.AssignmentID, err)
			}
			files, err := draftFiles(ctx, q, s.ID)
			if err != nil {
				return in, err
			}
			inForce, err := instructionsInForce(ctx, q, a)
			if err != nil {
				return in, err
			}
			if err := in.sameDraft(s.Body, files); err != nil {
				return in, err
			}
			if err := in.sameInstructions(inForce); err != nil {
				return in, err
			}
			text := textOf(s.Body)
			in.Body, in.Files, in.InstructionsVersionID = &text, files, inForce
			if s.GroupID != nil {
				// Whom it is handed in for is the group's members as they are
				// now: approving it after they change hands it in for others.
				live, err := q.LiveMembersOf(ctx, *s.GroupID)
				if err != nil {
					return in, err
				}
				if in.Members != nil && !sameMembers(in.Members, live) {
					return in, errMembersChanged
				}
				in.Members = live
				if in.Members == nil {
					in.Members = []uuid.UUID{}
				}
			}
			return in, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in SubmissionSubmitIn) error {
			sub, err := q.GetSubmissionFull(ctx, dbq.GetSubmissionFullParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return workGone(err)
			}
			_, err = in.handIn(ctx, q, sub)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionSubmitIn) (SubmissionSubmitOut, error) {
			// The locks, in order: the caller's seat (the pipeline's), the
			// assignment KEY SHARE, a group's work's group FOR SHARE, which a
			// move of one of its members waits for, the submission FOR
			// UPDATE, and, for a group's work, the assignment's work lock,
			// so that one student comes to be part of one group's work. The
			// draft may have gone with its assignment, deleted for good while
			// the call waited for a lock (assignment.delete).
			seen, err := ec.Q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionSubmitOut{}, workGone(err)
			}
			locked, err := ec.Q.GetAssignmentForSubmission(ctx, dbq.GetAssignmentForSubmissionParams{ID: seen.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionSubmitOut{}, goneIfNoRows(ctx, ec.Q, in.CourseID, seen.AssignmentID, err)
			}
			a := dbq.GetAssignmentInCourseRow(locked)
			if seen.GroupID != nil {
				if _, err := ec.Q.ShareGroup(ctx, dbq.ShareGroupParams{ID: *seen.GroupID, CourseID: in.CourseID}); err != nil {
					return SubmissionSubmitOut{}, err
				}
			}
			locks, err := ec.Q.GetSubmissionFullForUpdate(ctx, dbq.GetSubmissionFullForUpdateParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionSubmitOut{}, workGone(err)
			}
			s := dbq.GetSubmissionFullRow(locks)
			if s.GroupID != nil {
				if err := ec.Q.LockWorkMembersOfAssignment(ctx, s.AssignmentID); err != nil {
					return SubmissionSubmitOut{}, err
				}
			}
			// Who hands a group's draft in is one of its members now, under
			// the group's lock, or reaches one: not one moved out meanwhile.
			if err := draftStillReached(ctx, ec, s); err != nil {
				return SubmissionSubmitOut{}, err
			}
			if _, err := in.handIn(ctx, ec.Q, s); err != nil {
				return SubmissionSubmitOut{}, err
			}
			// Pin what the student was told. If the instructions are edited
			// tomorrow, a dispute can still show exactly what they said today.
			pinned, err := instructionsInForce(ctx, ec.Q, a)
			if err != nil {
				return SubmissionSubmitOut{}, err
			}
			at := ec.Now
			switch {
			case !ec.Approved:
				if err := in.sameInstructions(pinned); err != nil {
					return SubmissionSubmitOut{}, err
				}
			case in.Body != nil && in.Files != nil:
				// An approved proposal counts from when it was asked for,
				// under the instructions it recorded as then in force: the
				// student is not made late by the approver's delay, nor held
				// to instructions published afterwards. That is safe only
				// because the draft has just been found to hold what the
				// proposal recorded; a draft changed after it was proposed
				// would otherwise be handed in as of before the change. Every
				// proposal records it; one made before proposals did is
				// handed in as of now.
				at, pinned = ec.ActionCreatedAt, in.InstructionsVersionID
			}
			out := SubmissionSubmitOut{State: stateSubmitted, SubmittedAt: at, LeftOut: []LeftOut{}}
			if a.DueAt != nil && at.After(*a.DueAt) {
				out.State = stateLate
			}
			// Whom it is handed in for, as it is now, under the locks; of
			// those left out, the work that names them as the caller (for a
			// proposal, the proposer) may read it, which is what the action's
			// result keeps.
			if s.GroupID != nil {
				var left []LeftOut
				if out.Members, left, err = membersFor(ctx, ec.Q, s.AssignmentID, *s.GroupID); err != nil {
					return SubmissionSubmitOut{}, err
				}
				if out.LeftOut, err = readableLeftOut(ctx, ec.Q, ec.Member, s.AssignmentID, left); err != nil {
					return SubmissionSubmitOut{}, err
				}
			} else {
				out.Members = []uuid.UUID{*s.StudentMemberID}
			}
			n, err := ec.Q.SubmitSubmission(ctx, dbq.SubmitSubmissionParams{ID: s.ID, State: out.State, SubmittedAt: &at, InstructionsVersionID: pinned,
				SubmittedByMemberID: &ec.Member.ID})
			if err != nil {
				return SubmissionSubmitOut{}, err
			}
			if n == 0 {
				return SubmissionSubmitOut{}, apperr.Conflicts("the submission is %s, not a draft", s.State)
			}
			if s.GroupID != nil {
				// Frozen now: whose work it is from here on, whatever becomes
				// of the group.
				if err := ec.Q.InsertSubmissionMembers(ctx, dbq.InsertSubmissionMembersParams{SubmissionID: s.ID, CourseID: in.CourseID,
					AssignmentID: s.AssignmentID, AddedAt: at, AddedHow: addedHandIn, AddedByMemberID: &ec.Member.ID,
					MemberIds: out.Members}); err != nil {
					return SubmissionSubmitOut{}, err
				}
			}
			ev := events.Event{Type: EventSubmissionSubmitted, CourseID: &in.CourseID, SubjectType: "submission", SubjectID: &s.ID,
				AssignmentID: &s.AssignmentID, Payload: map[string]any{"state": out.State, "attempt": s.Attempt}}
			if s.GroupID != nil {
				ev.Payload["group_id"] = *s.GroupID
			}
			emitToStudents(ec, out.Members, ev)
			return out, nil
		},
	})
}

type SubmissionSetLatenessIn struct {
	tool.InCourse
	SubmissionID uuid.UUID `json:"submission_id"`
	State        string    `json:"state" jsonschema:"submitted or late"`
}

// errLateness refuses switching the lateness of an attempt that is state.
func errLateness(state string) error {
	return apperr.Conflicts("the submission is %s; only a submitted or late attempt can be switched, and only to the other", state)
}

// Correcting lateness is the one change a submitted attempt allows. It is
// gated by perm_grade_submit, not perm_submission_write: otherwise a student,
// who may write their own submissions, could un-late themselves.
func submissionSetLateness() tool.Tool {
	return tool.Define(tool.Spec[SubmissionSetLatenessIn, OK]{
		Name: "submission.set_lateness",
		Description: "Correct whether a submitted attempt counts as late — an extension granted, a clock that was wrong. " +
			"It is the only thing about a submitted attempt that can change, and it is for graders, not for the student. " +
			"A group's work is corrected for the group, reaching every member of it.",
		Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}/lateness"},
		Check: func(in SubmissionSetLatenessIn) error {
			if in.State != stateSubmitted && in.State != stateLate {
				return apperr.Invalid("state must be submitted or late")
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionSetLatenessIn) (tool.Target, error) {
			// A group's: every member, whose work it is.
			return submissionTarget(ctx, q, in.CourseID, in.SubmissionID, true)
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in SubmissionSetLatenessIn) error {
			sub, err := q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return workGone(err)
			}
			if (sub.State != stateSubmitted && sub.State != stateLate) || sub.State == in.State {
				return errLateness(sub.State)
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionSetLatenessIn) (OK, error) {
			s, err := ec.Q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return OK{}, workGone(err)
			}
			n, err := ec.Q.SetSubmissionLateness(ctx, dbq.SetSubmissionLatenessParams{ID: s.ID, State: in.State})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, errLateness(s.State)
			}
			return OK{OK: true}, emitWork(ctx, ec, s.ID, s.GroupID, events.Event{Type: EventSubmissionLateness, CourseID: &in.CourseID,
				SubjectType: "submission", SubjectID: &s.ID, AssignmentID: &s.AssignmentID, Payload: map[string]any{"state": in.State}})
		},
	})
}

type SubmissionRecordMissingIn struct {
	tool.InCourse
	AssignmentID    uuid.UUID  `json:"assignment_id"`
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty" jsonschema:"the student, on an individual assignment"`
	GroupID         *uuid.UUID `json:"group_id,omitempty" jsonschema:"the group, on a group assignment"`
}

type SubmissionIDOut struct {
	SubmissionID uuid.UUID `json:"submission_id"`
}

// missable refuses recording that in's student handed nothing in for an
// assignment published at publishedAt: one not published, which a caller m
// who may not see it does not see at all, or someone who is not a current
// student. submission.record_missing asks it before a proposal is queued
// (Validate), and again as it records it.
func missable(ctx context.Context, q dbq.Querier, m *domain.Member, in SubmissionRecordMissingIn, publishedAt *time.Time) error {
	if publishedAt == nil {
		if !canSeeUnpublished(m) {
			return errNoAssignment
		}
		return apperr.Precondition("the assignment is not published; nobody can have missed it")
	}
	if in.StudentMemberID == nil {
		return nil
	}
	entry, err := q.GetRosterEntry(ctx, dbq.GetRosterEntryParams{ID: *in.StudentMemberID, CourseID: in.CourseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.Missing("no such member in this course")
	}
	if err != nil {
		return err
	}
	if entry.Role != "student" || entry.Status == domain.MemberRemoved {
		return apperr.Precondition("only a current student of the course hands work in")
	}
	return nil
}

// errHasSubmission refuses a 'missing' row for a student, or a group (who),
// who has a submission already, latest, in state.
func errHasSubmission(who, state string, latest uuid.UUID) error {
	return apperr.Conflicts("the %s already has a submission (%s) for this assignment", who, state).With("submission_id", latest)
}

// missingFor checks in against assignment a: a group assignment names a
// group of its set, an individual one a student. It returns the group, for
// a group assignment.
func (in SubmissionRecordMissingIn) missingFor(ctx context.Context, q dbq.Querier, a dbq.GetAssignmentInCourseRow) (*dbq.GetGroupRow, error) {
	switch {
	case a.GroupSetID == nil && in.GroupID != nil:
		return nil, errNotAGroupAssignment
	case a.GroupSetID == nil:
		return nil, nil
	case in.StudentMemberID != nil:
		return nil, errGroupAssignment
	}
	g, err := loadGroup(ctx, q, in.CourseID, *in.GroupID)
	if err != nil {
		return nil, err
	}
	if g.SetID != *a.GroupSetID {
		return nil, apperr.Missing("no such group in this assignment's group set")
	}
	return &g, nil
}

// submissionRecordMissing is by hand what the sweep does when a due date
// passes, for one student or one group: for an assignment with no due date,
// or work the grader need not wait for. It is gated like set_lateness, by
// perm_grade_submit: what a student has handed in is not theirs to declare.
func submissionRecordMissing() tool.Tool {
	return tool.Define(tool.Spec[SubmissionRecordMissingIn, SubmissionIDOut]{
		Name: "submission.record_missing",
		Description: "Record that a student, or on a group assignment a group (group_id), has handed in nothing for a " +
			"published assignment: they get a 'missing' submission, which can be graded (a zero, say). Only where there " +
			"is no submission at all, not even a draft. A group's is recorded for its members now, less any another " +
			"group's work for the assignment names. If work is handed in afterwards it takes the missing row over, as it " +
			"does after a due date passes, unless a grade has been entered or proposed for it: then the work is a new " +
			"attempt, and the missing row keeps its grade.",
		Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/missing"},
		Check: func(in SubmissionRecordMissingIn) error {
			if (in.StudentMemberID == nil) == (in.GroupID == nil) {
				return apperr.Invalid("give student_member_id or, on a group assignment, group_id: one of them")
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionRecordMissingIn) (tool.Target, error) {
			t, err := assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
			if err != nil {
				return t, err
			}
			t.Type, t.ID = "submission", nil
			if in.StudentMemberID != nil {
				t.Scope.StudentMemberIDs = []uuid.UUID{*in.StudentMemberID}
				return t, nil
			}
			// Every member it is recorded for.
			members, err := q.LiveMembersOf(ctx, *in.GroupID)
			t.Scope.StudentMemberIDs = members
			return t, err
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, _ time.Time, in SubmissionRecordMissingIn) error {
			a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return goneIfNoRows(ctx, q, in.CourseID, in.AssignmentID, err)
			}
			if err := missable(ctx, q, m, in, a.PublishedAt); err != nil {
				return err
			}
			g, err := in.missingFor(ctx, q, a)
			if err != nil {
				return err
			}
			if g != nil {
				prior, err := q.ListGroupSubmissionsOf(ctx, dbq.ListGroupSubmissionsOfParams{AssignmentID: a.ID, GroupID: g.ID})
				if err != nil {
					return err
				}
				if len(prior) > 0 {
					return errHasSubmission(subjectGroup, prior[0].State, prior[0].ID)
				}
				members, _, err := membersFor(ctx, q, a.ID, g.ID)
				if err != nil {
					return err
				}
				if len(members) == 0 {
					return errGroupEmpty(g.ID)
				}
				return nil
			}
			prior, err := q.ListSubmissionsOf(ctx, dbq.ListSubmissionsOfParams{AssignmentID: a.ID, StudentMemberID: *in.StudentMemberID})
			if err != nil {
				return err
			}
			if len(prior) > 0 {
				return errHasSubmission("student", prior[0].State, prior[0].ID)
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionRecordMissingIn) (SubmissionIDOut, error) {
			// The assignment first: to a caller who may not see an
			// unpublished one, it is not there, whoever the student is.
			locked, err := ec.Q.GetAssignmentForSubmission(ctx, dbq.GetAssignmentForSubmissionParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionIDOut{}, goneIfNoRows(ctx, ec.Q, in.CourseID, in.AssignmentID, err)
			}
			a := dbq.GetAssignmentInCourseRow(locked)
			if err := missable(ctx, ec.Q, ec.Member, in, a.PublishedAt); err != nil {
				return SubmissionIDOut{}, err
			}
			g, err := in.missingFor(ctx, ec.Q, a)
			if err != nil {
				return SubmissionIDOut{}, err
			}
			if g != nil {
				return recordGroupMissing(ctx, ec, a, g.ID)
			}
			prior, err := ec.Q.LockSubmissionsOf(ctx, dbq.LockSubmissionsOfParams{AssignmentID: a.ID, StudentMemberID: *in.StudentMemberID})
			if err != nil {
				return SubmissionIDOut{}, err
			}
			if len(prior) > 0 {
				return SubmissionIDOut{}, errHasSubmission("student", prior[0].State, prior[0].ID)
			}
			id := ids.New()
			n, err := ec.Q.InsertMissingSubmission(ctx, dbq.InsertMissingSubmissionParams{ID: id, AssignmentID: a.ID,
				CourseID: in.CourseID, StudentMemberID: *in.StudentMemberID, CreatedAt: ec.Now})
			if err != nil {
				return SubmissionIDOut{}, err
			}
			if n == 0 {
				return SubmissionIDOut{}, apperr.Conflicts("the student started a submission just now")
			}
			student := *in.StudentMemberID
			ec.Emit(events.Event{Type: EventSubmissionMissing, CourseID: &in.CourseID, SubjectType: "submission",
				SubjectID: &id, StudentMemberID: &student, AssignmentID: &a.ID})
			return SubmissionIDOut{SubmissionID: id}, nil
		},
	})
}

// recordGroupMissing records that group handed nothing in for a: its
// 'missing' row, for its members now less those another group's work for a
// names, under the group's share lock and the assignment's work lock, as a
// hand-in takes them. In the feed, each of them is told.
func recordGroupMissing(ctx context.Context, ec *tool.ExecCtx, a dbq.GetAssignmentInCourseRow, group uuid.UUID) (SubmissionIDOut, error) {
	if _, err := ec.Q.ShareGroup(ctx, dbq.ShareGroupParams{ID: group, CourseID: a.CourseID}); err != nil {
		return SubmissionIDOut{}, err
	}
	prior, err := ec.Q.LockGroupSubmissionsOf(ctx, dbq.LockGroupSubmissionsOfParams{AssignmentID: a.ID, GroupID: group})
	if err != nil {
		return SubmissionIDOut{}, err
	}
	if len(prior) > 0 {
		return SubmissionIDOut{}, errHasSubmission(subjectGroup, prior[0].State, prior[0].ID)
	}
	id, err := writeGroupMissing(ctx, ec, a, group)
	if err != nil {
		return SubmissionIDOut{}, err
	}
	if id == nil {
		return SubmissionIDOut{}, errGroupEmpty(group)
	}
	return SubmissionIDOut{SubmissionID: *id}, nil
}

// writeGroupMissing writes group's 'missing' row for a and whose it is, under
// the assignment's work lock, and tells each member; nil, writing nothing,
// when the group has nobody it would be for, or a submission just now.
func writeGroupMissing(ctx context.Context, ec *tool.ExecCtx, a dbq.GetAssignmentInCourseRow, group uuid.UUID) (*uuid.UUID, error) {
	if err := ec.Q.LockWorkMembersOfAssignment(ctx, a.ID); err != nil {
		return nil, err
	}
	members, _, err := membersFor(ctx, ec.Q, a.ID, group)
	if err != nil || len(members) == 0 {
		return nil, err
	}
	id := ids.New()
	n, err := ec.Q.InsertGroupMissingSubmission(ctx, dbq.InsertGroupMissingSubmissionParams{ID: id, AssignmentID: a.ID,
		CourseID: a.CourseID, GroupID: group, CreatedAt: ec.Now})
	if err != nil || n == 0 {
		return nil, err
	}
	var by *uuid.UUID
	if ec.Member != nil {
		by = &ec.Member.ID
	}
	if err := ec.Q.InsertSubmissionMembers(ctx, dbq.InsertSubmissionMembersParams{SubmissionID: id, CourseID: a.CourseID,
		AssignmentID: a.ID, AddedAt: ec.Now, AddedHow: addedMissing, AddedByMemberID: by, MemberIds: members}); err != nil {
		return nil, err
	}
	emitToStudents(ec, members, events.Event{Type: EventSubmissionMissing, CourseID: &a.CourseID, SubjectType: "submission",
		SubjectID: &id, AssignmentID: &a.ID, Payload: map[string]any{"group_id": group}})
	return &id, nil
}
