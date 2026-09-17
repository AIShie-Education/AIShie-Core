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
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func submissionTools() []tool.Tool {
	return []tool.Tool{submissionList(), submissionGet(), submissionCreate(), submissionUpdateDraft(), submissionSubmit(), submissionSetLateness()}
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
	ID                    uuid.UUID  `json:"id"`
	AssignmentID          uuid.UUID  `json:"assignment_id"`
	StudentMemberID       uuid.UUID  `json:"student_member_id"`
	Attempt               int32      `json:"attempt"`
	Body                  *string    `json:"body,omitempty"`
	InstructionsVersionID *uuid.UUID `json:"instructions_version_id,omitempty" jsonschema:"the exact version of the instructions in force when it was submitted"`
	State                 string     `json:"state" jsonschema:"draft, submitted, late or missing"`
	SubmittedAt           *time.Time `json:"submitted_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	Files                 []FileRef  `json:"files,omitempty" jsonschema:"submitted files; read each with document.get"`
}

type SubmissionListIn struct {
	tool.InCourse
	AssignmentID    *uuid.UUID `json:"assignment_id,omitempty"`
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty"`
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
				AssignmentID: in.AssignmentID, StudentMemberID: in.StudentMemberID,
				StudentAll: rc.Scope.StudentAll, AssignmentAll: rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID,
			})
			out := SubmissionListOut{Submissions: make([]SubmissionView, 0, len(rows))}
			for _, r := range rows {
				out.Submissions = append(out.Submissions, SubmissionView{ID: r.ID, AssignmentID: r.AssignmentID,
					StudentMemberID: r.StudentMemberID, Attempt: r.Attempt, State: r.State, SubmittedAt: r.SubmittedAt, CreatedAt: r.CreatedAt})
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, err
		},
	})
}

type SubmissionIDIn struct {
	tool.InCourse
	SubmissionID uuid.UUID `json:"submission_id"`
}

func submissionTarget(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID) (tool.Target, error) {
	s, err := q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: id, CourseID: courseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such submission in this course")
	}
	if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: courseID, Type: "submission", ID: &id,
		Scope: authz.Target{StudentMemberIDs: []uuid.UUID{s.StudentMemberID}, AssignmentIDs: []uuid.UUID{s.AssignmentID}}}, nil
}

func submissionGet() tool.Tool {
	return tool.Define(tool.Spec[SubmissionIDIn, SubmissionView]{
		Name:        "submission.get",
		Description: "One submission in full, with its text and the version of the instructions it was submitted under.",
		Kind:        tool.Read, Gate: readSubmissions,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionIDIn) (tool.Target, error) {
			return submissionTarget(ctx, q, in.CourseID, in.SubmissionID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in SubmissionIDIn) (SubmissionView, error) {
			s, err := rc.Q.GetSubmissionFull(ctx, dbq.GetSubmissionFullParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionView{}, err
			}
			v := SubmissionView{ID: s.ID, AssignmentID: s.AssignmentID, StudentMemberID: s.StudentMemberID, Attempt: s.Attempt,
				Body: s.Body, InstructionsVersionID: s.InstructionsVersionID, State: s.State, SubmittedAt: s.SubmittedAt, CreatedAt: s.CreatedAt}
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
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty" jsonschema:"whose work this is; defaults to the caller"`
	Body            *string    `json:"body,omitempty"`
}

type SubmissionCreateOut struct {
	SubmissionID uuid.UUID `json:"submission_id"`
	Attempt      int32     `json:"attempt"`
}

// studentOf: the caller, unless they name someone else. Whether they may act
// for someone else is for scope to decide, like everything else.
func (in SubmissionCreateIn) studentOf(m *domain.Member) uuid.UUID {
	if in.StudentMemberID != nil {
		return *in.StudentMemberID
	}
	return m.ID
}

func submissionCreate() tool.Tool {
	return tool.Define(tool.Spec[SubmissionCreateIn, SubmissionCreateOut]{
		Name: "submission.create",
		Description: "Start a draft submission to a published assignment. A draft can be edited freely; nothing is handed in " +
			"until submission.submit. Once an attempt has been submitted it never changes, and submitting again means " +
			"creating a new attempt here.",
		Kind: tool.Write, Gate: writeSubmissions,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionCreateIn) (tool.Target, error) {
			t, err := assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
			if err != nil {
				return t, err
			}
			t.Type, t.ID = "submission", nil
			if in.StudentMemberID != nil {
				t.Scope.StudentMemberIDs = []uuid.UUID{*in.StudentMemberID}
			}
			// When the student is the caller, who that is is not known until
			// the call runs; Execute checks their scope itself.
			return t, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionCreateIn) (SubmissionCreateOut, error) {
			student := in.studentOf(ec.Member)
			if in.StudentMemberID == nil {
				// A student's scope normally lists themselves. If it has been
				// emptied, it reaches nobody, themselves included: scope
				// fails closed here as everywhere.
				if reason, err := authz.CheckScope(ctx, ec.Q, ec.Member, authz.Target{StudentMemberIDs: []uuid.UUID{student}}); err != nil {
					return SubmissionCreateOut{}, err
				} else if reason != authz.ReasonNone {
					return SubmissionCreateOut{}, apperr.Forbid("you are outside your own student scope").With("reason", string(reason))
				}
			}
			// The composite key proves the member is in this course. That
			// they are a student is ours to check: work is handed in by, and
			// grades are given to, people on the roster as students.
			entry, err := ec.Q.GetRosterEntry(ctx, dbq.GetRosterEntryParams{ID: student, CourseID: in.CourseID})
			if errors.Is(err, pgx.ErrNoRows) {
				return SubmissionCreateOut{}, apperr.Missing("no such member in this course")
			}
			if err != nil {
				return SubmissionCreateOut{}, err
			}
			if entry.Role != "student" || entry.Status != domain.MemberActive {
				return SubmissionCreateOut{}, apperr.Precondition("work is submitted by, or for, a current student of the course")
			}
			a, err := ec.Q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionCreateOut{}, err
			}
			if a.PublishedAt == nil {
				return SubmissionCreateOut{}, apperr.Missing("no such assignment in this course")
			}

			prior, err := ec.Q.LockSubmissionsOf(ctx, dbq.LockSubmissionsOfParams{AssignmentID: a.ID, StudentMemberID: student})
			if err != nil {
				return SubmissionCreateOut{}, err
			}
			attempt := int32(1)
			if len(prior) > 0 {
				switch latest := prior[0]; latest.State {
				case stateDraft:
					return SubmissionCreateOut{}, apperr.Conflicts("there is already an open draft; edit or submit that one").With("submission_id", latest.ID)
				case stateMissing:
					if err := ec.Q.ReopenMissingSubmission(ctx, dbq.ReopenMissingSubmissionParams{ID: latest.ID, Body: in.Body}); err != nil {
						return SubmissionCreateOut{}, err
					}
					return SubmissionCreateOut{SubmissionID: latest.ID, Attempt: latest.Attempt}, nil
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

type SubmissionUpdateDraftIn struct {
	tool.InCourse
	SubmissionID uuid.UUID `json:"submission_id"`
	Body         string    `json:"body"`
}

func submissionUpdateDraft() tool.Tool {
	return tool.Define(tool.Spec[SubmissionUpdateDraftIn, OK]{
		Name:        "submission.update_draft",
		Description: "Replace the text of a draft submission. Only drafts change; a submitted attempt is frozen.",
		Kind:        tool.Write, Gate: writeSubmissions,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionUpdateDraftIn) (tool.Target, error) {
			return submissionTarget(ctx, q, in.CourseID, in.SubmissionID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionUpdateDraftIn) (OK, error) {
			n, err := ec.Q.UpdateSubmissionDraft(ctx, dbq.UpdateSubmissionDraftParams{ID: in.SubmissionID, Body: &in.Body})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the submission is no longer a draft; start a new attempt to submit again")
			}
			return OK{OK: true}, nil
		},
	})
}

type SubmissionSubmitOut struct {
	State       string    `json:"state" jsonschema:"submitted, or late if the due date had passed"`
	SubmittedAt time.Time `json:"submitted_at"`
}

func submissionSubmit() tool.Tool {
	return tool.Define(tool.Spec[SubmissionIDIn, SubmissionSubmitOut]{
		Name: "submission.submit",
		Description: "Hand a draft in. It is marked late if the due date has passed, the version of the instructions in " +
			"force right now is recorded with it, and from this moment it never changes.",
		Kind: tool.Write, Gate: writeSubmissions,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}/submit"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionIDIn) (tool.Target, error) {
			return submissionTarget(ctx, q, in.CourseID, in.SubmissionID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionIDIn) (SubmissionSubmitOut, error) {
			s, err := ec.Q.GetSubmissionFull(ctx, dbq.GetSubmissionFullParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionSubmitOut{}, err
			}
			a, err := ec.Q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: s.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionSubmitOut{}, err
			}
			files, err := ec.Q.CountSubmissionDocuments(ctx, &s.ID)
			if err != nil {
				return SubmissionSubmitOut{}, err
			}
			if (s.Body == nil || *s.Body == "") && files == 0 {
				return SubmissionSubmitOut{}, apperr.Precondition("there is nothing to hand in: the draft has no text and no files")
			}
			out := SubmissionSubmitOut{State: stateSubmitted, SubmittedAt: ec.Now}
			if a.DueAt != nil && ec.Now.After(*a.DueAt) {
				out.State = stateLate
			}
			// Pin what the student was told. If the instructions are edited
			// tomorrow, a dispute can still show exactly what they said today.
			var pinned *uuid.UUID
			if a.InstructionsDocumentID != nil {
				if pinned, err = ec.Q.GetDocumentPublishedVersion(ctx, *a.InstructionsDocumentID); err != nil {
					return SubmissionSubmitOut{}, err
				}
			}
			n, err := ec.Q.SubmitSubmission(ctx, dbq.SubmitSubmissionParams{ID: s.ID, State: out.State, SubmittedAt: &ec.Now, InstructionsVersionID: pinned})
			if err != nil {
				return SubmissionSubmitOut{}, err
			}
			if n == 0 {
				return SubmissionSubmitOut{}, apperr.Conflicts("the submission is %s, not a draft", s.State)
			}
			ec.Emit(events.Event{Type: EventSubmissionSubmitted, CourseID: &in.CourseID, SubjectType: "submission", SubjectID: &s.ID,
				StudentMemberID: &s.StudentMemberID, AssignmentID: &s.AssignmentID, Payload: map[string]any{"state": out.State, "attempt": s.Attempt}})
			return out, nil
		},
	})
}

type SubmissionSetLatenessIn struct {
	tool.InCourse
	SubmissionID uuid.UUID `json:"submission_id"`
	State        string    `json:"state" jsonschema:"submitted or late"`
}

// Correcting lateness is the one change a submitted attempt allows. It is
// gated by perm_grade_submit, not perm_submission_write: otherwise a student,
// who may write their own submissions, could un-late themselves.
func submissionSetLateness() tool.Tool {
	return tool.Define(tool.Spec[SubmissionSetLatenessIn, OK]{
		Name: "submission.set_lateness",
		Description: "Correct whether a submitted attempt counts as late — an extension granted, a clock that was wrong. " +
			"It is the only thing about a submitted attempt that can change, and it is for graders, not for the student.",
		Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}/lateness"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionSetLatenessIn) (tool.Target, error) {
			return submissionTarget(ctx, q, in.CourseID, in.SubmissionID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionSetLatenessIn) (OK, error) {
			if in.State != stateSubmitted && in.State != stateLate {
				return OK{}, apperr.Invalid("state must be submitted or late")
			}
			s, err := ec.Q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return OK{}, err
			}
			n, err := ec.Q.SetSubmissionLateness(ctx, dbq.SetSubmissionLatenessParams{ID: s.ID, State: in.State})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the submission is %s; only a submitted or late attempt can be switched, and only to the other", s.State)
			}
			ec.Emit(events.Event{Type: EventSubmissionLateness, CourseID: &in.CourseID, SubjectType: "submission", SubjectID: &s.ID,
				StudentMemberID: &s.StudentMemberID, AssignmentID: &s.AssignmentID, Payload: map[string]any{"state": in.State}})
			return OK{OK: true}, nil
		},
	})
}
