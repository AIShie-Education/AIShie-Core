package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/gradecalc"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func gradeTools(d Deps) []tool.Tool {
	return []tool.Tool{gradeSubmit(d), gradePost(), gradeRegrade(d), gradebookGet()}
}

// FeedbackFile is a file returned with a grade: a marked-up script, a
// recording. It is uploaded first (document.upload_url, kind feedback) and
// named here, so that it travels with the grade — including through a
// proposal, where there is no grade yet for a file to be attached to.
type FeedbackFile struct {
	Title       string `json:"title"`
	UploadToken string `json:"upload_token"`
}

// checkFeedbackFiles verifies each file without recording anything.
func checkFeedbackFiles(ctx context.Context, d Deps, q dbq.Querier, m *domain.Member, courseID uuid.UUID, files []FeedbackFile) error {
	seen := map[string]bool{}
	for _, f := range files {
		if strings.TrimSpace(f.Title) == "" {
			return apperr.Invalid("every feedback file needs a title")
		}
		if seen[f.UploadToken] {
			return apperr.Invalid("the same upload is listed twice")
		}
		seen[f.UploadToken] = true
		if _, err := claimUpload(ctx, d, q, m, courseID, kindFeedback, f.UploadToken, false); err != nil {
			return err
		}
	}
	return nil
}

// attachFeedbackFiles records each file as a feedback document of the grade.
func attachFeedbackFiles(ctx context.Context, d Deps, ec *tool.ExecCtx, courseID, gradeID uuid.UUID, files []FeedbackFile) error {
	for i, f := range files {
		doc := ids.New()
		if err := ec.Q.InsertDocument(ctx, dbq.InsertDocumentParams{ID: doc, CourseID: courseID, Kind: kindFeedback,
			Title: f.Title, GradeID: &gradeID, SortOrder: int32(i), CreatedAt: ec.Now}); err != nil {
			return err
		}
		token := f.UploadToken
		v, err := insertVersion(ctx, d, ec, courseID, doc, kindFeedback, 1, Content{UploadToken: &token})
		if err != nil {
			return err
		}
		if err := ec.Q.SetPublishedVersion(ctx, dbq.SetPublishedVersionParams{ID: doc, PublishedVersionID: &v}); err != nil {
			return err
		}
	}
	return nil
}

// BreakdownItem is one line of a grade's per-criterion detail. The rubric is
// prose the grader reads; the breakdown is what the grader produced from it.
type BreakdownItem struct {
	Criterion string          `json:"criterion"`
	Points    decimal.Decimal `json:"points"`
	Max       decimal.Decimal `json:"max"`
	Comment   *string         `json:"comment,omitempty"`
}

// GradeContent is the part of a grade that submitting and regrading share.
// Exported because it is embedded in tool inputs: schema inference and
// encoding/json both need to see through it.
type GradeContent struct {
	Score           decimal.Decimal  `json:"score"`
	Feedback        *string          `json:"feedback,omitempty"`
	Breakdown       []BreakdownItem  `json:"breakdown,omitempty"`
	RubricVersionID *uuid.UUID       `json:"rubric_version_id,omitempty" jsonschema:"the rubric version the grader was shown; defaults to the published one"`
	OutOf           *decimal.Decimal `json:"out_of,omitempty" jsonschema:"the points possible the score is out of; defaults to what the work is worth now. A proposal records it, and is refused on approval if the work has been rescaled since"`
	AllowExtra      bool             `json:"allow_extra,omitempty" jsonschema:"permit a score above the points possible"`
	FeedbackFiles   []FeedbackFile   `json:"feedback_files,omitempty" jsonschema:"files to return with the grade, uploaded beforehand"`
}

// ---------------------------------------------------------------------------
// grade.submit
// ---------------------------------------------------------------------------

type GradeSubmitIn struct {
	tool.InCourse
	SubmissionID    *uuid.UUID `json:"submission_id,omitempty" jsonschema:"grade this submission; or give component_id and student_member_id"`
	ComponentID     *uuid.UUID `json:"component_id,omitempty" jsonschema:"grade a directly graded component, such as an exam"`
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty" jsonschema:"required with component_id"`
	// ForMissing is pinned when a grade is proposed for a submission, and a
	// direct call may give it too: the grade is refused if the work is no
	// longer what it was.
	ForMissing *bool `json:"for_missing,omitempty" jsonschema:"whether the grade is for a 'missing' placeholder, nothing handed in; filled in when the grade is proposed, and the grade is refused if late work has since taken the placeholder's place"`
	GradeContent
}

type GradeSubmitOut struct {
	GradeID uuid.UUID `json:"grade_id"`
}

// gradeSubject is what a grade is for: a submission, or a component for a
// student. Exactly one of submission and component is set.
type gradeSubject struct {
	student    uuid.UUID
	submission *dbq.GetSubmissionInCourseRow
	assignment *dbq.GetAssignmentInCourseRow
	component  *dbq.GetComponentInCourseRow
}

func (s gradeSubject) pointsPossible() decimal.Decimal {
	if s.assignment != nil {
		return s.assignment.PointsPossible
	}
	return s.component.PointsPossible.Decimal
}

func (s gradeSubject) target(courseID uuid.UUID) tool.Target {
	t := tool.Target{CourseID: courseID, Scope: authz.Target{StudentMemberIDs: []uuid.UUID{s.student}}}
	if s.submission != nil {
		t.Type, t.ID = "submission", &s.submission.ID
		t.Scope.AssignmentIDs = []uuid.UUID{s.assignment.ID}
	} else {
		t.Type, t.ID = "grade_component", &s.component.ID
		t.Scope.SpansAssignments = true
	}
	return t
}

// changedItem is what moved in the tree when this subject's grade changed.
func (s gradeSubject) changedItem() uuid.UUID {
	if s.assignment != nil {
		return s.assignment.ID
	}
	return s.component.ID
}

func loadSubject(ctx context.Context, q dbq.Querier, courseID uuid.UUID, submissionID, componentID, studentID *uuid.UUID) (gradeSubject, error) {
	var s gradeSubject
	switch {
	case (submissionID == nil) == (componentID == nil):
		return s, apperr.Invalid("give exactly one of submission_id and component_id")

	case submissionID != nil:
		if studentID != nil {
			return s, apperr.Invalid("student_member_id goes with component_id; a submission already names its student")
		}
		sub, err := q.GetSubmissionInCourse(ctx, dbq.GetSubmissionInCourseParams{ID: *submissionID, CourseID: courseID})
		if errors.Is(err, pgx.ErrNoRows) {
			return s, apperr.Missing("no such submission in this course")
		}
		if err != nil {
			return s, err
		}
		a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: sub.AssignmentID, CourseID: courseID})
		if err != nil {
			return s, err
		}
		s.student, s.submission, s.assignment = sub.StudentMemberID, &sub, &a

	default:
		if studentID == nil {
			return s, apperr.Invalid("student_member_id is required with component_id")
		}
		c, err := q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: *componentID, CourseID: courseID})
		if errors.Is(err, pgx.ErrNoRows) {
			return s, apperr.Missing("no such grade component in this course")
		}
		if err != nil {
			return s, err
		}
		if _, err := q.GetRosterEntry(ctx, dbq.GetRosterEntryParams{ID: *studentID, CourseID: courseID}); errors.Is(err, pgx.ErrNoRows) {
			return s, apperr.Missing("no such member in this course")
		} else if err != nil {
			return s, err
		}
		s.student, s.component = *studentID, &c
	}
	return s, nil
}

// checkSubject holds the rules about what may be graded at all. It takes the
// grade's locks and reads s again under them, so what is checked here and in
// checkContent after it is what the grade is written against — and a
// proposal made for a 'missing' placeholder is on record before any takeover
// can look for it (SubmissionHasGrades). The work must still be what the
// grade was given for: nothing, if it was a placeholder when first read here
// or, as forMissing says when it was pinned, when the grade was proposed.
func checkSubject(ctx context.Context, q dbq.Querier, courseID uuid.UUID, s *gradeSubject, forMissing *bool) error {
	if s.submission != nil {
		seen := s.submission.State
		if err := lockGradeTarget(ctx, q, courseID, s); err != nil {
			return err
		}
		missing := s.submission.State == stateMissing
		switch {
		case s.submission.State == stateDraft:
			return apperr.Precondition("the submission has not been submitted yet")
		case missing != (seen == stateMissing), forMissing != nil && *forMissing && !missing:
			return apperr.Precondition("this grade was given for nothing handed in, and there is work here now; look at it, and grade it again")
		case forMissing != nil && !*forMissing && missing:
			return apperr.Precondition("this grade was given for work handed in, and nothing was; look at it, and grade it again")
		}
		return nil
	}
	// A component takes a grade directly only if it is a leaf with points of
	// its own: not rolled up from children, not a bucket of assignments. Checked
	// under the tree lock, so that it cannot stop being one while we look.
	if err := q.LockCourseComponents(ctx, courseID); err != nil {
		return err
	}
	fresh, err := q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: s.component.ID, CourseID: courseID})
	if err != nil {
		return err
	}
	s.component = &fresh
	if !s.component.PointsPossible.Valid {
		return apperr.Precondition("this component is rolled up from what is beneath it and takes no grade of its own")
	}
	if n, err := q.CountComponentChildren(ctx, &s.component.ID); err != nil {
		return err
	} else if n > 0 {
		return apperr.Precondition("this component has sub-components and takes no grade of its own")
	}
	if n, err := q.CountComponentAssignments(ctx, &s.component.ID); err != nil {
		return err
	} else if n > 0 {
		return apperr.Precondition("this component holds assignments and takes no grade of its own")
	}
	// Only someone on the roster as a student receives a grade. This is a
	// fact about the roster, not an authorization check.
	entry, err := q.GetRosterEntry(ctx, dbq.GetRosterEntryParams{ID: s.student, CourseID: courseID})
	if err != nil {
		return err
	}
	if entry.Role != "student" || entry.Status == domain.MemberRemoved {
		return apperr.Precondition("grades are given to current students of the course")
	}
	return nil
}

// pinContent fills in what a grade is against when the caller left it to
// default: the points possible the score is out of, and the rubric's
// published version, as they stand now. It is a Pin, run when a proposal is
// made. A proposal approved after the rubric has moved on still records the
// version the grader was shown; one approved after the work was rescaled is
// refused, rather than its 95 out of 100 being carried out as 95 out of 200.
func pinContent(ctx context.Context, q dbq.Querier, s gradeSubject, c *GradeContent) error {
	if c.OutOf == nil {
		max := s.pointsPossible()
		c.OutOf = &max
	}
	if c.RubricVersionID != nil || s.assignment == nil || s.assignment.RubricDocumentID == nil {
		return nil
	}
	v, err := q.GetDocumentPublishedVersion(ctx, *s.assignment.RubricDocumentID)
	if err != nil {
		return err
	}
	c.RubricVersionID = v
	return nil
}

// checkContent holds the rules about the grade itself, and returns the rubric
// version to pin: the one named, or the rubric's published version.
func checkContent(ctx context.Context, q dbq.Querier, s gradeSubject, c GradeContent) (*uuid.UUID, error) {
	if c.Score.IsNegative() {
		return nil, apperr.Invalid("score cannot be negative")
	}
	max := s.pointsPossible()
	if c.OutOf != nil && !c.OutOf.Equal(max) {
		return nil, apperr.Precondition("the score was given out of %s, and the work is worth %s now; grade it again out of what it is worth", *c.OutOf, max)
	}
	if c.Score.GreaterThan(max) && !c.AllowExtra {
		return nil, apperr.Precondition("score %s is above the %s points possible; set allow_extra to permit it", c.Score, max)
	}
	for _, b := range c.Breakdown {
		if b.Points.IsNegative() || b.Max.IsNegative() {
			return nil, apperr.Invalid("breakdown points cannot be negative")
		}
	}
	if s.assignment == nil || s.assignment.RubricDocumentID == nil {
		if c.RubricVersionID != nil {
			return nil, apperr.Precondition("there is no rubric here for rubric_version_id to be a version of")
		}
		return nil, nil
	}
	if c.RubricVersionID == nil {
		return q.GetDocumentPublishedVersion(ctx, *s.assignment.RubricDocumentID)
	}
	owner, err := q.GetDocumentVersionOwner(ctx, *c.RubricVersionID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != *s.assignment.RubricDocumentID) {
		return nil, apperr.Precondition("rubric_version_id is not a version of this assignment's rubric")
	}
	if err != nil {
		return nil, err
	}
	return c.RubricVersionID, nil
}

func breakdownJSON(items []BreakdownItem) ([]byte, error) {
	if len(items) == 0 {
		return nil, nil
	}
	return json.Marshal(items)
}

func gradeSubmit(d Deps) tool.Tool {
	load := func(ctx context.Context, q dbq.Querier, in GradeSubmitIn) (gradeSubject, error) {
		return loadSubject(ctx, q, in.CourseID, in.SubmissionID, in.ComponentID, in.StudentMemberID)
	}
	return tool.Define(tool.Spec[GradeSubmitIn, GradeSubmitOut]{
		Name: "grade.submit",
		Description: "Write a draft grade for a submission, or for a student on a directly graded component. " +
			"A draft is not visible to the student until it is posted with grade.post. " +
			"A new draft replaces any earlier draft for the same work.",
		Kind: tool.Write,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/grades"},

		Resolve: func(ctx context.Context, q dbq.Querier, in GradeSubmitIn) (tool.Target, error) {
			s, err := load(ctx, q, in)
			if err != nil {
				return tool.Target{}, err
			}
			return s.target(in.CourseID), nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, in GradeSubmitIn) error {
			if in.ForMissing != nil && in.SubmissionID == nil {
				return apperr.Invalid("for_missing is for a grade on a submission")
			}
			s, err := load(ctx, q, in)
			if err != nil {
				return err
			}
			// The uploads first: grade.regrade takes their locks before the
			// grade's work, and both take them in that order.
			if err := checkFeedbackFiles(ctx, d, q, m, in.CourseID, in.FeedbackFiles); err != nil {
				return err
			}
			if err := checkSubject(ctx, q, in.CourseID, &s, in.ForMissing); err != nil {
				return err
			}
			_, err = checkContent(ctx, q, s, in.GradeContent)
			return err
		},
		Pin: func(ctx context.Context, q dbq.Querier, in GradeSubmitIn) (GradeSubmitIn, error) {
			s, err := load(ctx, q, in)
			if err != nil {
				return in, err
			}
			if s.submission != nil {
				forMissing := s.submission.State == stateMissing
				in.ForMissing = &forMissing
			}
			return in, pinContent(ctx, q, s, &in.GradeContent)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradeSubmitIn) (GradeSubmitOut, error) {
			s, err := load(ctx, ec.Q, in)
			if err != nil {
				return GradeSubmitOut{}, err
			}
			// One draft at a time for one piece of work: the target is locked
			// before the earlier drafts are looked at, or two graders at once
			// would each see none and leave two live drafts, which nothing
			// could then post. The lock also holds still what the grade is
			// out of, and the score is checked against that.
			if err := lockGradeTarget(ctx, ec.Q, in.CourseID, &s); err != nil {
				return GradeSubmitOut{}, err
			}
			rubric, err := checkContent(ctx, ec.Q, s, in.GradeContent)
			if err != nil {
				return GradeSubmitOut{}, err
			}
			breakdown, err := breakdownJSON(in.Breakdown)
			if err != nil {
				return GradeSubmitOut{}, err
			}
			// A new draft replaces earlier ones — but this draft is as old as
			// the call that made it. A proposal approved on Wednesday must
			// not replace a draft somebody entered on Tuesday: the approver
			// saw the proposal, not the draft.
			if err := noNewerDraft(ctx, ec, s); err != nil {
				return GradeSubmitOut{}, err
			}
			id := ids.New()
			if s.submission != nil {
				err = ec.Q.SupersedeSubmissionDrafts(ctx, dbq.SupersedeSubmissionDraftsParams{NewID: &id, SubmissionID: &s.submission.ID})
			} else {
				err = ec.Q.SupersedeComponentDrafts(ctx, dbq.SupersedeComponentDraftsParams{NewID: &id, ComponentID: &s.component.ID, StudentMemberID: s.student})
			}
			if err != nil {
				return GradeSubmitOut{}, err
			}
			row := dbq.InsertGradeParams{
				ID: id, StudentMemberID: s.student, Origin: "entered", Score: in.Score,
				Feedback: in.Feedback, Breakdown: breakdown, RubricVersionID: rubric,
				GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID, CreatedAt: ec.Now,
			}
			ev := events.Event{
				Type: events.GradeCreated, CourseID: &in.CourseID,
				SubjectType: "grade", SubjectID: &id, StudentMemberID: &s.student,
			}
			if s.submission != nil {
				row.SubmissionID, ev.AssignmentID = &s.submission.ID, &s.assignment.ID
			} else {
				row.ComponentID = &s.component.ID
			}
			if err := ec.Q.InsertGrade(ctx, row); err != nil {
				return GradeSubmitOut{}, err
			}
			if err := attachFeedbackFiles(ctx, d, ec, in.CourseID, id, in.FeedbackFiles); err != nil {
				return GradeSubmitOut{}, err
			}
			ec.Emit(ev)
			return GradeSubmitOut{GradeID: id}, nil
		},
	})
}

// lockGradeTarget serialises the writers of one piece of work's grades, and
// holds still what they are checked against; checkSubject takes it, in
// Validate, and it is held to the end of the call.
//
// For a submission it first holds the assignment still, shared, and reads it
// again into s. The score is checked against the points possible, and they
// must not change between that check and the grade being there for
// assignment.update's own check to find: it locks the row before it looks,
// so one of the two waits for the other, and a 95 is never entered on work
// that has meanwhile become worth 50. Then it locks the submission and reads
// its state into s: late work taking a 'missing' placeholder over takes the
// same lock, so the state is the one the grade is written against. The
// assignment is locked before its submission, never the other way round. A
// component's points are held still by the tree lock checkSubject takes.
func lockGradeTarget(ctx context.Context, q dbq.Querier, courseID uuid.UUID, s *gradeSubject) error {
	if s.submission != nil {
		a, err := q.ShareAssignmentForGrading(ctx, dbq.ShareAssignmentForGradingParams{ID: s.assignment.ID, CourseID: courseID})
		if err != nil {
			return err
		}
		fresh := dbq.GetAssignmentInCourseRow(a)
		s.assignment = &fresh
		state, err := q.LockSubmissionForGrading(ctx, s.submission.ID)
		if err != nil {
			return err
		}
		s.submission.State = state
		return nil
	}
	return q.LockComponentGradeTarget(ctx, dbq.LockComponentGradeTargetParams{ComponentID: s.component.ID, StudentMemberID: s.student})
}

// noNewerDraft refuses to replace a draft entered after this call was made.
// A direct call is as new as anything: it applies to a proposal being
// carried out later than it was made.
func noNewerDraft(ctx context.Context, ec *tool.ExecCtx, s gradeSubject) error {
	if !ec.ActionCreatedAt.Before(ec.Now) {
		return nil
	}
	var newest time.Time
	var err error
	if s.submission != nil {
		newest, err = ec.Q.NewestSubmissionDraftAt(ctx, &s.submission.ID)
	} else {
		newest, err = ec.Q.NewestComponentDraftAt(ctx, dbq.NewestComponentDraftAtParams{ComponentID: &s.component.ID, StudentMemberID: s.student})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if newest.After(ec.ActionCreatedAt) {
		return apperr.Precondition("a newer draft was entered for this work after this grade was proposed; look at it, and propose again if it should still be replaced")
	}
	return nil
}

// ---------------------------------------------------------------------------
// grade.post
// ---------------------------------------------------------------------------

type GradePostIn struct {
	tool.InCourse
	GradeIDs            []uuid.UUID `json:"grade_ids,omitempty" jsonschema:"the draft grades to post; or give assignment_id"`
	AssignmentID        *uuid.UUID  `json:"assignment_id,omitempty" jsonschema:"post every draft grade waiting for this assignment; a proposal posts those that were waiting when it was made"`
	TreatUngradedAsZero bool        `json:"treat_ungraded_as_zero,omitempty" jsonschema:"for final grades: count ungraded work as zero in the totals. Once a student's totals have been written this way they stay final: later posts and regrades keep counting ungraded work as zero. It decides the course total, so it needs an assignment scope of the whole course"`
}

type GradePostOut struct {
	Posted    []uuid.UUID `json:"posted"`
	Snapshots int         `json:"snapshots" jsonschema:"how many rolled-up totals were written or changed"`
}

// gradesToPost finds the drafts a post call is about.
func gradesToPost(ctx context.Context, q dbq.Querier, in GradePostIn) ([]dbq.GetGradesInCourseRow, error) {
	idsToPost := in.GradeIDs
	switch {
	case (len(in.GradeIDs) == 0) == (in.AssignmentID == nil):
		return nil, apperr.Invalid("give exactly one of grade_ids and assignment_id")
	case in.AssignmentID != nil:
		if _, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: *in.AssignmentID, CourseID: in.CourseID}); errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.Missing("no such assignment in this course")
		} else if err != nil {
			return nil, err
		}
		var err error
		if idsToPost, err = q.ListDraftGradeIDsForAssignment(ctx, dbq.ListDraftGradeIDsForAssignmentParams{AssignmentID: *in.AssignmentID, CourseID: in.CourseID}); err != nil {
			return nil, err
		}
		if len(idsToPost) == 0 {
			return nil, nil
		}
	}
	idsToPost = dedupe(idsToPost)
	rows, err := q.GetGradesInCourse(ctx, dbq.GetGradesInCourseParams{Ids: idsToPost, CourseID: in.CourseID})
	if err != nil {
		return nil, err
	}
	if len(rows) != len(idsToPost) {
		return nil, apperr.Missing("one or more of those grades do not exist in this course")
	}
	return rows, nil
}

func gradePost() tool.Tool {
	return tool.Define(tool.Spec[GradePostIn, GradePostOut]{
		Name: "grade.post",
		Description: "Post draft grades so that students can see them, and write down each affected student's " +
			"rolled-up totals as they stand. Every grade in the batch must be within the caller's scope. " +
			"A grade that is already posted is changed with grade.regrade, not posted again.",
		Kind: tool.Write,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradePost}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/grades/post"},

		Resolve: func(ctx context.Context, q dbq.Querier, in GradePostIn) (tool.Target, error) {
			rows, err := gradesToPost(ctx, q, in)
			if err != nil {
				return tool.Target{}, err
			}
			t := tool.Target{CourseID: in.CourseID, Type: "grade", Scope: postScope(rows, in.TreatUngradedAsZero)}
			switch {
			case in.AssignmentID != nil:
				// The assignment is the target whether or not anything is
				// waiting on it: an assignment outside the caller's scope is
				// out of scope before the drafts are counted, so the count
				// tells them nothing.
				t.Type, t.ID = "assignment", in.AssignmentID
				t.Scope.AssignmentIDs = append(t.Scope.AssignmentIDs, *in.AssignmentID)
			case len(rows) == 1:
				t.ID = &rows[0].ID
			}
			return t, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, in GradePostIn) error {
			rows, err := gradesToPost(ctx, q, in)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return apperr.Precondition("there are no draft grades to post")
			}
			return checkPostable(ctx, q, rows)
		},
		// A proposal to post an assignment is about the drafts waiting when
		// it was made. One entered while it waits has been in front of
		// nobody who could release it, so the proposal names the drafts it
		// was made about; one of them replaced since fails the approval, as
		// any named grade that is no longer a draft does.
		Pin: func(ctx context.Context, q dbq.Querier, in GradePostIn) (GradePostIn, error) {
			if in.AssignmentID == nil {
				return in, nil
			}
			rows, err := gradesToPost(ctx, q, in)
			if err != nil {
				return in, err
			}
			in.GradeIDs, in.AssignmentID = make([]uuid.UUID, len(rows)), nil
			for i, g := range rows {
				in.GradeIDs[i] = g.ID
			}
			return in, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradePostIn) (GradePostOut, error) {
			rows, err := gradesToPost(ctx, ec.Q, in)
			if err != nil {
				return GradePostOut{}, err
			}
			if len(rows) == 0 {
				return GradePostOut{}, apperr.Precondition("there are no draft grades to post")
			}
			gradeIDs := make([]uuid.UUID, len(rows))
			for i, g := range rows {
				gradeIDs[i] = g.ID
			}
			// Lock, then look again: what was a draft a moment ago may not be.
			if _, err := ec.Q.LockGradesInCourse(ctx, dbq.LockGradesInCourseParams{Ids: gradeIDs, CourseID: in.CourseID}); err != nil {
				return GradePostOut{}, err
			}
			if rows, err = ec.Q.GetGradesInCourse(ctx, dbq.GetGradesInCourseParams{Ids: gradeIDs, CourseID: in.CourseID}); err != nil {
				return GradePostOut{}, err
			}
			if err := checkPostable(ctx, ec.Q, rows); err != nil {
				return GradePostOut{}, err
			}
			// With assignment_id — a direct call; a proposal names its
			// drafts — the batch is whatever is a draft now, which may be
			// more than what authorize() scope-checked: a draft entered in
			// between, for a student the caller does not reach. Checked
			// again against the rows that are actually about to be posted.
			if reason, err := authz.CheckScope(ctx, ec.Q, ec.Member, postScope(rows, in.TreatUngradedAsZero)); err != nil {
				return GradePostOut{}, err
			} else if reason != authz.ReasonNone {
				return GradePostOut{}, apperr.Forbid("a draft entered since this call was authorized is outside your scope; call again").With("reason", string(reason))
			}

			out := GradePostOut{Posted: make([]uuid.UUID, 0, len(rows))}
			changed := map[uuid.UUID][]uuid.UUID{}
			for _, g := range rows {
				n, err := ec.Q.PostGrade(ctx, dbq.PostGradeParams{ID: g.ID, PostedAt: &ec.Now, PostedByMemberID: &ec.Member.ID})
				if err != nil {
					return GradePostOut{}, err
				}
				if n == 0 {
					return GradePostOut{}, apperr.Conflicts("grade %s was posted or replaced by someone else just now", g.ID)
				}
				id, student := g.ID, g.StudentMemberID
				ec.Emit(events.Event{
					Type: events.GradePosted, CourseID: &in.CourseID,
					SubjectType: "grade", SubjectID: &id, StudentMemberID: &student, AssignmentID: g.AssignmentID,
				})
				out.Posted = append(out.Posted, g.ID)
				item := g.ComponentID
				if g.AssignmentID != nil {
					item = g.AssignmentID
				}
				changed[student] = append(changed[student], *item)
			}
			out.Snapshots, err = snapshot(ctx, ec, in.CourseID, changed, gradecalc.Policy{UngradedAsZero: in.TreatUngradedAsZero})
			return out, err
		},
	})
}

// postScope is steps 4 and 5 for a batch: every student and assignment in it.
// Posting as final is a decision about the course total as well — every other
// assignment's ungraded work becomes a zero, for good — so it spans
// assignments whatever is in the batch.
func postScope(rows []dbq.GetGradesInCourseRow, final bool) authz.Target {
	t := authz.Target{SpansAssignments: final}
	for _, g := range rows {
		t.StudentMemberIDs = append(t.StudentMemberIDs, g.StudentMemberID)
		if g.AssignmentID != nil {
			t.AssignmentIDs = append(t.AssignmentIDs, *g.AssignmentID)
		} else {
			t.SpansAssignments = true
		}
	}
	return t
}

// checkPostable: a grade is posted once. Each must be a live entered draft,
// and its target must not already have a live posted grade.
func checkPostable(ctx context.Context, q dbq.Querier, rows []dbq.GetGradesInCourseRow) error {
	targets := map[string]uuid.UUID{}
	for _, g := range rows {
		// Two drafts for one piece of work in one batch: whichever is
		// posted second would collide with the first. Said plainly instead.
		key := g.StudentMemberID.String()
		if g.SubmissionID != nil {
			key = "s:" + g.SubmissionID.String()
		} else if g.ComponentID != nil {
			key += ":c:" + g.ComponentID.String()
		}
		if other, dup := targets[key]; dup {
			return apperr.Precondition("grades %s and %s are both drafts for the same work; enter one draft for it and post that", other, g.ID)
		}
		targets[key] = g.ID
		switch {
		case g.Origin != "entered":
			return apperr.Precondition("grade %s is a computed total; totals are written by posting, not posted", g.ID)
		case g.SupersededBy != nil:
			return apperr.Conflicts("grade %s has been replaced by a newer draft", g.ID)
		case g.PostedAt != nil:
			return apperr.Conflicts("grade %s is already posted; use grade.regrade to change it", g.ID)
		}
		var exists bool
		var err error
		if g.SubmissionID != nil {
			exists, err = q.LiveSubmissionGradeExists(ctx, g.SubmissionID)
		} else {
			exists, err = q.LiveComponentGradeExists(ctx, dbq.LiveComponentGradeExistsParams{ComponentID: g.ComponentID, StudentMemberID: g.StudentMemberID})
		}
		if err != nil {
			return err
		}
		if exists {
			return apperr.Conflicts("the work grade %s is for already has a posted grade; use grade.regrade to change it", g.ID)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// grade.regrade
// ---------------------------------------------------------------------------

type GradeRegradeIn struct {
	tool.InCourse
	GradeID uuid.UUID `json:"grade_id" jsonschema:"the posted grade to replace"`
	GradeContent
	TreatUngradedAsZero bool `json:"treat_ungraded_as_zero,omitempty"`
}

type GradeRegradeOut struct {
	GradeID   uuid.UUID `json:"grade_id" jsonschema:"the new grade"`
	Replaces  uuid.UUID `json:"replaces"`
	Snapshots int       `json:"snapshots"`
}

func gradeRegrade(d Deps) tool.Tool {
	load := func(ctx context.Context, q dbq.Querier, in GradeRegradeIn) (dbq.GetGradesInCourseRow, gradeSubject, error) {
		rows, err := q.GetGradesInCourse(ctx, dbq.GetGradesInCourseParams{Ids: []uuid.UUID{in.GradeID}, CourseID: in.CourseID})
		if err != nil {
			return dbq.GetGradesInCourseRow{}, gradeSubject{}, err
		}
		if len(rows) == 0 {
			return dbq.GetGradesInCourseRow{}, gradeSubject{}, apperr.Missing("no such grade in this course")
		}
		g := rows[0]
		var student *uuid.UUID
		if g.ComponentID != nil {
			student = &g.StudentMemberID
		}
		s, err := loadSubject(ctx, q, in.CourseID, g.SubmissionID, g.ComponentID, student)
		return g, s, err
	}
	check := func(g dbq.GetGradesInCourseRow) error {
		switch {
		case g.Origin != "entered":
			return apperr.Precondition("a computed total is not regraded; regrade what is beneath it")
		case g.PostedAt == nil:
			return apperr.Precondition("the grade is still a draft; submit a new draft instead")
		case g.SupersededBy != nil:
			return apperr.Conflicts("the grade has already been replaced")
		}
		return nil
	}
	return tool.Define(tool.Spec[GradeRegradeIn, GradeRegradeOut]{
		Name: "grade.regrade",
		Description: "Replace a posted grade. The old grade is kept and marked superseded, the new one is posted " +
			"at once, and the student's totals are written down again if they changed.",
		Kind: tool.Write,
		// Regrading writes a grade and makes it visible in one step, so it
		// takes both permissions and runs at the lower of the two levels.
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/grades/{grade_id}/regrade"},

		Resolve: func(ctx context.Context, q dbq.Querier, in GradeRegradeIn) (tool.Target, error) {
			g, s, err := load(ctx, q, in)
			if err != nil {
				return tool.Target{}, err
			}
			t := s.target(in.CourseID)
			t.Type, t.ID = "grade", &g.ID
			// Regrading as final decides the course total, as posting as
			// final does (postScope).
			if in.TreatUngradedAsZero {
				t.Scope.SpansAssignments = true
			}
			return t, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, in GradeRegradeIn) error {
			g, s, err := load(ctx, q, in)
			if err != nil {
				return err
			}
			if err := check(g); err != nil {
				return err
			}
			if _, err = checkContent(ctx, q, s, in.GradeContent); err != nil {
				return err
			}
			return checkFeedbackFiles(ctx, d, q, m, in.CourseID, in.FeedbackFiles)
		},
		Pin: func(ctx context.Context, q dbq.Querier, in GradeRegradeIn) (GradeRegradeIn, error) {
			_, s, err := load(ctx, q, in)
			if err != nil {
				return in, err
			}
			return in, pinContent(ctx, q, s, &in.GradeContent)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradeRegradeIn) (GradeRegradeOut, error) {
			if _, err := ec.Q.LockGradesInCourse(ctx, dbq.LockGradesInCourseParams{Ids: []uuid.UUID{in.GradeID}, CourseID: in.CourseID}); err != nil {
				return GradeRegradeOut{}, err
			}
			old, s, err := load(ctx, ec.Q, in)
			if err != nil {
				return GradeRegradeOut{}, err
			}
			if err := check(old); err != nil {
				return GradeRegradeOut{}, err
			}
			rubric, err := checkContent(ctx, ec.Q, s, in.GradeContent)
			if err != nil {
				return GradeRegradeOut{}, err
			}
			breakdown, err := breakdownJSON(in.Breakdown)
			if err != nil {
				return GradeRegradeOut{}, err
			}

			// The order is load-bearing. superseded_by is a deferred foreign
			// key, so pointing the old row at a grade that does not exist yet
			// is allowed until the end of the statement batch; inserting the
			// new posted row first would be two live grades for one target,
			// which the partial unique index refuses immediately.
			id := ids.New()
			n, err := ec.Q.SupersedeGrade(ctx, dbq.SupersedeGradeParams{ID: old.ID, NewID: &id})
			if err != nil {
				return GradeRegradeOut{}, err
			}
			if n == 0 {
				return GradeRegradeOut{}, apperr.Conflicts("the grade was replaced by someone else just now")
			}
			if err := ec.Q.InsertGrade(ctx, dbq.InsertGradeParams{
				ID: id, StudentMemberID: s.student, SubmissionID: old.SubmissionID, ComponentID: old.ComponentID,
				Origin: "entered", Score: in.Score, Feedback: in.Feedback, Breakdown: breakdown, RubricVersionID: rubric,
				GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID,
				PostedAt: &ec.Now, PostedByMemberID: &ec.Member.ID, CreatedAt: ec.Now,
			}); err != nil {
				return GradeRegradeOut{}, err
			}
			if err := attachFeedbackFiles(ctx, d, ec, in.CourseID, id, in.FeedbackFiles); err != nil {
				return GradeRegradeOut{}, err
			}
			ec.Emit(events.Event{
				Type: events.GradeRegraded, CourseID: &in.CourseID,
				SubjectType: "grade", SubjectID: &id, StudentMemberID: &s.student, AssignmentID: old.AssignmentID,
				Payload: map[string]any{"replaces": old.ID},
			})
			snaps, err := snapshot(ctx, ec, in.CourseID, map[uuid.UUID][]uuid.UUID{s.student: {s.changedItem()}},
				gradecalc.Policy{UngradedAsZero: in.TreatUngradedAsZero})
			return GradeRegradeOut{GradeID: id, Replaces: old.ID, Snapshots: snaps}, err
		},
	})
}

// ---------------------------------------------------------------------------
// gradebook.get
// ---------------------------------------------------------------------------

type GradebookGetIn struct {
	tool.InCourse
	StudentMemberID     uuid.UUID `json:"student_member_id"`
	TreatUngradedAsZero bool      `json:"treat_ungraded_as_zero,omitempty"`
}

type GradebookLine struct {
	ComponentID uuid.UUID        `json:"component_id"`
	Name        string           `json:"name"`
	Percent     *decimal.Decimal `json:"percent" jsonschema:"out of 100; null when nothing beneath it has a posted grade"`
	gradecalc.Result
}

type GradebookGetOut struct {
	StudentMemberID uuid.UUID       `json:"student_member_id"`
	Components      []GradebookLine `json:"components"`
}

func gradebookGet() tool.Tool {
	return tool.Define(tool.Spec[GradebookGetIn, GradebookGetOut]{
		Name: "gradebook.get",
		Description: "One student's rolled-up grades, computed now from posted grades: every component of the " +
			"course with its percentage and the working behind it. Nothing is stored by reading this.",
		Kind: tool.Read,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/gradebook/{student_member_id}"},

		Resolve: func(ctx context.Context, q dbq.Querier, in GradebookGetIn) (tool.Target, error) {
			if _, err := q.GetRosterEntry(ctx, dbq.GetRosterEntryParams{ID: in.StudentMemberID, CourseID: in.CourseID}); errors.Is(err, pgx.ErrNoRows) {
				return tool.Target{}, apperr.Missing("no such member in this course")
			} else if err != nil {
				return tool.Target{}, err
			}
			return tool.Target{
				CourseID: in.CourseID, Type: "gradebook", ID: &in.StudentMemberID,
				// A gradebook spans every assignment, so it is for members
				// whose assignment scope is the whole course.
				Scope: authz.Target{StudentMemberIDs: []uuid.UUID{in.StudentMemberID}, SpansAssignments: true},
			}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in GradebookGetIn) (GradebookGetOut, error) {
			root, names, err := loadTree(ctx, rc.Q, in.CourseID)
			if err != nil {
				return GradebookGetOut{}, err
			}
			scores, err := loadScores(ctx, rc.Q, in.CourseID, in.StudentMemberID)
			if err != nil {
				return GradebookGetOut{}, err
			}
			results := gradecalc.Compute(root, scores, gradecalc.Policy{UngradedAsZero: in.TreatUngradedAsZero})

			out := GradebookGetOut{StudentMemberID: in.StudentMemberID}
			var walk func(c *gradecalc.Component)
			walk = func(c *gradecalc.Component) {
				line := GradebookLine{ComponentID: c.ID, Name: names[c.ID], Result: results[c.ID]}
				if f := results[c.ID].Fraction; f != nil {
					pct := gradecalc.Percent(*f)
					line.Percent = &pct
				}
				out.Components = append(out.Components, line)
				for _, ch := range c.Children {
					walk(ch)
				}
			}
			walk(root)
			return out, nil
		},
	})
}

func dedupe(in []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(in))
	out := make([]uuid.UUID, 0, len(in))
	for _, id := range in {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}
