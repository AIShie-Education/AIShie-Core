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
		submissionSubmit(), submissionSetLateness(), submissionRecordMissing()}
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
				PrincipalID: rc.Scope.PrincipalID, PrincipalStudentAll: rc.Scope.PrincipalStudentAll, PrincipalAssignmentAll: rc.Scope.PrincipalAssignmentAll,
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

type SubmissionRosterIn struct {
	tool.InCourse
	AssignmentID uuid.UUID `json:"assignment_id"`
	Page
}

type RosterEntry struct {
	StudentMemberID uuid.UUID  `json:"student_member_id"`
	DisplayName     *string    `json:"display_name,omitempty" jsonschema:"only for a caller who may read the member list (perm_member_read)"`
	MemberStatus    *string    `json:"member_status,omitempty" jsonschema:"active or paused; only for a caller who may read the member list"`
	State           string     `json:"state" jsonschema:"not_started (no submission at all), draft, submitted, late or missing: the latest attempt's"`
	SubmissionID    *uuid.UUID `json:"submission_id,omitempty" jsonschema:"the latest attempt; absent when not started"`
	Attempt         *int32     `json:"attempt,omitempty"`
	SubmittedAt     *time.Time `json:"submitted_at,omitempty"`
}

type SubmissionRosterOut struct {
	Students []RosterEntry `json:"students"`
	Next     *uuid.UUID    `json:"next,omitempty"`
}

const stateNotStarted = "not_started"

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
				return SubmissionRosterOut{}, err
			}
			if a.PublishedAt == nil && !canSeeUnpublished(rc.Member) {
				// As in assignment.get: to this caller it does not exist yet.
				return SubmissionRosterOut{}, apperr.Missing("no such assignment in this course")
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
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, _ time.Time, in SubmissionCreateIn) error {
			student, err := submitter(ctx, q, m, in)
			if err != nil {
				return err
			}
			a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return err
			}
			if a.PublishedAt == nil {
				return errNoAssignment
			}
			prior, err := q.ListSubmissionsOf(ctx, dbq.ListSubmissionsOfParams{AssignmentID: a.ID, StudentMemberID: student})
			if err != nil {
				return err
			}
			if len(prior) > 0 && prior[0].State == stateDraft {
				return errOpenDraft(prior[0].ID)
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionCreateIn) (SubmissionCreateOut, error) {
			student, err := submitter(ctx, ec.Q, ec.Member, in)
			if err != nil {
				return SubmissionCreateOut{}, err
			}
			a, err := ec.Q.GetAssignmentForSubmission(ctx, dbq.GetAssignmentForSubmissionParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionCreateOut{}, err
			}
			if a.PublishedAt == nil {
				return SubmissionCreateOut{}, errNoAssignment
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
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in SubmissionUpdateDraftIn) error {
			sub, err := q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err == nil && sub.State != stateDraft {
				return errNoLongerDraft
			}
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionUpdateDraftIn) (OK, error) {
			n, err := ec.Q.UpdateSubmissionDraft(ctx, dbq.UpdateSubmissionDraftParams{ID: in.SubmissionID, Body: &in.Body})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, errNoLongerDraft
			}
			return OK{OK: true}, nil
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
}

type SubmissionSubmitOut struct {
	State       string    `json:"state" jsonschema:"submitted, or late if the due date had passed"`
	SubmittedAt time.Time `json:"submitted_at"`
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
func (in SubmissionSubmitIn) handIn(ctx context.Context, q dbq.Querier, s dbq.Submission) ([]uuid.UUID, error) {
	if s.State != stateDraft {
		return nil, apperr.Conflicts("the submission is %s, not a draft", s.State)
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
			"has been decided.",
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
				return in, err
			}
			if s.State != stateDraft {
				return in, apperr.Conflicts("the submission is %s, not a draft", s.State)
			}
			a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: s.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return in, err
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
			return in, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in SubmissionSubmitIn) error {
			sub, err := q.GetSubmissionFull(ctx, dbq.GetSubmissionFullParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return err
			}
			_, err = in.handIn(ctx, q, sub)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionSubmitIn) (SubmissionSubmitOut, error) {
			s, err := ec.Q.GetSubmissionFullForUpdate(ctx, dbq.GetSubmissionFullForUpdateParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionSubmitOut{}, err
			}
			a, err := ec.Q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: s.AssignmentID, CourseID: in.CourseID})
			if err != nil {
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
			out := SubmissionSubmitOut{State: stateSubmitted, SubmittedAt: at}
			if a.DueAt != nil && at.After(*a.DueAt) {
				out.State = stateLate
			}
			n, err := ec.Q.SubmitSubmission(ctx, dbq.SubmitSubmissionParams{ID: s.ID, State: out.State, SubmittedAt: &at, InstructionsVersionID: pinned})
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
			"It is the only thing about a submitted attempt that can change, and it is for graders, not for the student.",
		Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/submissions/{submission_id}/lateness"},
		Check: func(in SubmissionSetLatenessIn) error {
			if in.State != stateSubmitted && in.State != stateLate {
				return apperr.Invalid("state must be submitted or late")
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionSetLatenessIn) (tool.Target, error) {
			return submissionTarget(ctx, q, in.CourseID, in.SubmissionID)
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in SubmissionSetLatenessIn) error {
			sub, err := q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return err
			}
			if (sub.State != stateSubmitted && sub.State != stateLate) || sub.State == in.State {
				return errLateness(sub.State)
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionSetLatenessIn) (OK, error) {
			s, err := ec.Q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: in.SubmissionID, CourseID: in.CourseID})
			if err != nil {
				return OK{}, err
			}
			n, err := ec.Q.SetSubmissionLateness(ctx, dbq.SetSubmissionLatenessParams{ID: s.ID, State: in.State})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, errLateness(s.State)
			}
			ec.Emit(events.Event{Type: EventSubmissionLateness, CourseID: &in.CourseID, SubjectType: "submission", SubjectID: &s.ID,
				StudentMemberID: &s.StudentMemberID, AssignmentID: &s.AssignmentID, Payload: map[string]any{"state": in.State}})
			return OK{OK: true}, nil
		},
	})
}

type SubmissionRecordMissingIn struct {
	tool.InCourse
	AssignmentID    uuid.UUID `json:"assignment_id"`
	StudentMemberID uuid.UUID `json:"student_member_id"`
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
	entry, err := q.GetRosterEntry(ctx, dbq.GetRosterEntryParams{ID: in.StudentMemberID, CourseID: in.CourseID})
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

// errHasSubmission refuses a 'missing' row for a student who has a
// submission already, latest, in state.
func errHasSubmission(state string, latest uuid.UUID) error {
	return apperr.Conflicts("the student already has a submission (%s) for this assignment", state).With("submission_id", latest)
}

// submissionRecordMissing is by hand what the sweep does when a due date
// passes, for one student: for an assignment with no due date, or a student
// the grader need not wait for. It is gated like set_lateness, by
// perm_grade_submit: what a student has handed in is not theirs to declare.
func submissionRecordMissing() tool.Tool {
	return tool.Define(tool.Spec[SubmissionRecordMissingIn, SubmissionIDOut]{
		Name: "submission.record_missing",
		Description: "Record that a student has handed in nothing for a published assignment: they get a 'missing' " +
			"submission, which can be graded (a zero, say). Only for a student with no submission at all, not even a " +
			"draft. If they hand work in afterwards it takes the missing row over, as it does after a due date passes, " +
			"unless a grade has been entered or proposed for it: then the work is a new attempt, and the missing row " +
			"keeps its grade.",
		Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/missing"},
		Resolve: func(ctx context.Context, q dbq.Querier, in SubmissionRecordMissingIn) (tool.Target, error) {
			t, err := assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
			if err != nil {
				return t, err
			}
			t.Type, t.ID = "submission", nil
			t.Scope.StudentMemberIDs = []uuid.UUID{in.StudentMemberID}
			return t, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, _ time.Time, in SubmissionRecordMissingIn) error {
			a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return err
			}
			if err := missable(ctx, q, m, in, a.PublishedAt); err != nil {
				return err
			}
			prior, err := q.ListSubmissionsOf(ctx, dbq.ListSubmissionsOfParams{AssignmentID: a.ID, StudentMemberID: in.StudentMemberID})
			if err != nil {
				return err
			}
			if len(prior) > 0 {
				return errHasSubmission(prior[0].State, prior[0].ID)
			}
			return nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in SubmissionRecordMissingIn) (SubmissionIDOut, error) {
			// The assignment first: to a caller who may not see an
			// unpublished one, it is not there, whoever the student is.
			a, err := ec.Q.GetAssignmentForSubmission(ctx, dbq.GetAssignmentForSubmissionParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return SubmissionIDOut{}, err
			}
			if err := missable(ctx, ec.Q, ec.Member, in, a.PublishedAt); err != nil {
				return SubmissionIDOut{}, err
			}
			prior, err := ec.Q.LockSubmissionsOf(ctx, dbq.LockSubmissionsOfParams{AssignmentID: a.ID, StudentMemberID: in.StudentMemberID})
			if err != nil {
				return SubmissionIDOut{}, err
			}
			if len(prior) > 0 {
				return SubmissionIDOut{}, errHasSubmission(prior[0].State, prior[0].ID)
			}
			id := ids.New()
			n, err := ec.Q.InsertMissingSubmission(ctx, dbq.InsertMissingSubmissionParams{ID: id, AssignmentID: a.ID,
				CourseID: in.CourseID, StudentMemberID: in.StudentMemberID, CreatedAt: ec.Now})
			if err != nil {
				return SubmissionIDOut{}, err
			}
			if n == 0 {
				return SubmissionIDOut{}, apperr.Conflicts("the student started a submission just now")
			}
			student := in.StudentMemberID
			ec.Emit(events.Event{Type: EventSubmissionMissing, CourseID: &in.CourseID, SubjectType: "submission",
				SubjectID: &id, StudentMemberID: &student, AssignmentID: &a.ID})
			return SubmissionIDOut{SubmissionID: id}, nil
		},
	})
}
