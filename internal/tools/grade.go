package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/gradecalc"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

func gradeTools(d Deps) []tool.Tool {
	return []tool.Tool{gradeSubmit(d), gradePost(), gradeRegrade(d), gradebookGet(),
		gradeOverrideTotal(), gradeClearOverride(), gradeCommentTotal(), gradeUndoUngradedAsZero()}
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
//
// NoRubric is pinned when a grade is proposed and there is no rubric version
// to pin, as ForMissing is: an empty rubric_version_id would otherwise mean
// whatever is published when the proposal is approved.
type GradeContent struct {
	Score           decimal.Decimal  `json:"score"`
	Feedback        *string          `json:"feedback,omitempty"`
	Breakdown       []BreakdownItem  `json:"breakdown,omitempty"`
	RubricVersionID *uuid.UUID       `json:"rubric_version_id,omitempty" jsonschema:"the rubric version the grader was shown; defaults to the published one. A proposal records it, or no_rubric if none was published when it was made"`
	NoRubric        bool             `json:"no_rubric,omitempty" jsonschema:"that the grader was shown no rubric; filled in when the grade is proposed and no rubric is published, and the grade then records none, whatever is published before it is approved. A call giving it is refused if a rubric is published"`
	OutOf           *decimal.Decimal `json:"out_of,omitempty" jsonschema:"the points possible the score is out of; defaults to what the work is worth now. A proposal records it, and is refused on approval if the work has been rescaled since"`
	AllowExtra      bool             `json:"allow_extra,omitempty" jsonschema:"permit a score above the points possible"`
	FeedbackFiles   []FeedbackFile   `json:"feedback_files,omitempty" jsonschema:"files to return with the grade, uploaded beforehand"`
}

// uploads are the feedback files' upload tokens, for checkUploadAge.
func (c GradeContent) uploads() []string {
	tokens := make([]string, len(c.FeedbackFiles))
	for i, f := range c.FeedbackFiles {
		tokens[i] = f.UploadToken
	}
	return tokens
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
// published version, as they stand now, or no_rubric where there is none.
// It is a Pin, run when a proposal is made. A proposal approved after the
// rubric has moved on still records the version the grader was shown, and
// one made with no rubric in force records none, though one has been
// published or attached since; one approved after the work was rescaled is
// refused, rather than its 95 out of 100 being carried out as 95 out of 200.
func pinContent(ctx context.Context, q dbq.Querier, s gradeSubject, c *GradeContent) error {
	if c.OutOf == nil {
		max := s.pointsPossible()
		c.OutOf = &max
	}
	if c.RubricVersionID != nil || s.assignment == nil {
		return nil
	}
	if c.NoRubric {
		// Given by the caller. Approving it records no rubric whatever is
		// in force then, so it is held to what is in force now, as a call
		// giving it is.
		_, err := checkContent(ctx, q, s, *c, false)
		return err
	}
	var v *uuid.UUID
	if s.assignment.RubricDocumentID != nil {
		var err error
		if v, err = q.GetDocumentPublishedVersion(ctx, *s.assignment.RubricDocumentID); err != nil {
			return err
		}
	}
	c.RubricVersionID, c.NoRubric = v, v == nil
	return nil
}

// checkContent holds the rules about the grade itself, and returns the rubric
// version to pin: the one named, none where no_rubric says the grader was
// shown none, or the rubric's published version. approved says the grade is
// a proposal being carried out, whose no_rubric was pinned when it was made:
// it records none, whatever has been published or attached since. A call's
// no_rubric says no rubric is in force now, and is refused if one is.
// Validate cannot tell the two apart and passes true; Execute, straight
// after it, holds a call to it, and Pin a proposal as it is made.
func checkContent(ctx context.Context, q dbq.Querier, s gradeSubject, c GradeContent, approved bool) (*uuid.UUID, error) {
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
	if c.NoRubric && c.RubricVersionID != nil {
		return nil, apperr.Invalid("give rubric_version_id or no_rubric, not both")
	}
	if s.assignment == nil || s.assignment.RubricDocumentID == nil {
		if c.RubricVersionID != nil {
			return nil, apperr.Precondition("there is no rubric here for rubric_version_id to be a version of")
		}
		return nil, nil
	}
	switch {
	case c.NoRubric && approved:
		return nil, nil
	case c.RubricVersionID == nil:
		v, err := q.GetDocumentPublishedVersion(ctx, *s.assignment.RubricDocumentID)
		if err == nil && v != nil && c.NoRubric {
			return nil, apperr.Precondition("no_rubric says there is no rubric in force, and this assignment's rubric is published; grade against it")
		}
		return v, err
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
			// Validate cannot tell an approval from a call: Execute holds a
			// call's no_rubric to the rubric in force.
			_, err = checkContent(ctx, q, s, in.GradeContent, true)
			return err
		},
		Pin: func(ctx context.Context, q dbq.Querier, _ *domain.Member, now time.Time, in GradeSubmitIn) (GradeSubmitIn, error) {
			if err := checkUploadAge(ctx, d, now, in.uploads()...); err != nil {
				return in, err
			}
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
			rubric, err := checkContent(ctx, ec.Q, s, in.GradeContent, ec.Approved)
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
			// Dated when the call was made, not when it was approved: the
			// next proposal measures itself against this draft as this one
			// was measured, and one made after this was proposed replaces it
			// even if it is approved after this was.
			row := dbq.InsertGradeParams{
				ID: id, StudentMemberID: s.student, Origin: "entered", Score: in.Score,
				Feedback: in.Feedback, Breakdown: breakdown, RubricVersionID: rubric,
				GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID, CreatedAt: ec.ActionCreatedAt,
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

// holdWorth holds still, to the end of the call, what the work a grade is
// for is worth: the assignment FOR SHARE, as grade.submit takes it
// (lockGradeTarget), which assignment.update's row lock waits for and holds
// off; or, for a component graded directly, the course's tree lock, which
// component.update takes before it changes the points.
func holdWorth(ctx context.Context, q dbq.Querier, courseID uuid.UUID, s gradeSubject) error {
	if s.assignment != nil {
		_, err := q.ShareAssignmentForGrading(ctx, dbq.ShareAssignmentForGradingParams{ID: s.assignment.ID, CourseID: courseID})
		return err
	}
	return q.LockCourseComponents(ctx, courseID)
}

// noNewerDraft refuses to replace a draft entered after this call was made.
// A direct call is as new as anything: it applies to a proposal being
// carried out on its approval.
func noNewerDraft(ctx context.Context, ec *tool.ExecCtx, s gradeSubject) error {
	if !ec.Approved {
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
	GradeIDs            []uuid.UUID `json:"grade_ids,omitempty" jsonschema:"the draft grades to post; or give assignment_id. Approving a proposal to post them posts those still waiting, passes over any posted since, and fails if one has been replaced"`
	AssignmentID        *uuid.UUID  `json:"assignment_id,omitempty" jsonschema:"post every draft grade waiting for this assignment. A proposal records the drafts that were waiting when it was made, as grade_ids beside this: approving it posts those of them still waiting, passes over any posted since, and fails if one has been replaced"`
	TreatUngradedAsZero bool        `json:"treat_ungraded_as_zero,omitempty" jsonschema:"for final grades: count ungraded work as zero in the totals. Once a student's totals have been written this way they stay final: later posts and regrades keep counting ungraded work as zero, until grade.undo_ungraded_as_zero. It decides the course total, so it needs an assignment scope of the whole course"`
}

type GradePostOut struct {
	Posted    []uuid.UUID `json:"posted"`
	Snapshots int         `json:"snapshots" jsonschema:"how many rolled-up totals were written or changed"`
}

// pinned reports whether in is what a proposal to post an assignment
// stores: the assignment, and beside it the drafts that were waiting for it
// when the proposal was made. Pin is the only thing that writes both; a call
// gives one or the other.
func (in GradePostIn) pinned() bool {
	return in.AssignmentID != nil && len(in.GradeIDs) > 0
}

// gradesToPost finds the drafts a post call is about. For a pinned proposal
// that is every draft it names, posted since or not: whatever it goes on to
// post, it is authorized over all of them, as it was when it was made.
func gradesToPost(ctx context.Context, q dbq.Querier, in GradePostIn) ([]dbq.GetGradesInCourseRow, error) {
	if len(in.GradeIDs) == 0 && in.AssignmentID == nil {
		return nil, apperr.Invalid("give exactly one of grade_ids and assignment_id")
	}
	idsToPost := in.GradeIDs
	if in.AssignmentID != nil {
		if _, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: *in.AssignmentID, CourseID: in.CourseID}); errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.Missing("no such assignment in this course")
		} else if err != nil {
			return nil, err
		}
	}
	if len(idsToPost) == 0 {
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

// stillWaiting is what approving a proposal posts: the drafts it names,
// less any posted by hand while it waited. Those are out already, as
// what was proposed, and the rest were in front of whoever proposed
// releasing them as they are now. A draft replaced meanwhile fails the
// approval instead. Its replacement has been in front of nobody who asked
// for it to be released, and posting the others without it is not what was
// proposed either.
func stillWaiting(rows []dbq.GetGradesInCourseRow) ([]dbq.GetGradesInCourseRow, error) {
	waiting := make([]dbq.GetGradesInCourseRow, 0, len(rows))
	for _, g := range rows {
		switch {
		case g.SupersededBy != nil:
			return nil, apperr.Conflicts("grade %s, one of the drafts this proposal was made about, has been replaced since it was proposed; propose posting again", g.ID)
		case g.PostedAt != nil:
			continue
		}
		waiting = append(waiting, g)
	}
	if len(waiting) == 0 {
		return nil, apperr.Precondition("every draft this proposal was made about has been posted since it was proposed; there is nothing left for it to post")
	}
	return waiting, nil
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
			// Drafts named by id, alone or beside the assignment as Pin
			// records them, are checked in Execute: it is the one place
			// that can tell an approval, which passes over one posted
			// meanwhile, from a call, which is told that it is posted or
			// refused for giving both. Pin checks them for a proposal as it
			// is made. Execute runs straight after this, for a call and an
			// approval alike, and refuses there what would have been
			// refused here.
			if len(in.GradeIDs) > 0 {
				return nil
			}
			if len(rows) == 0 {
				return apperr.Precondition("there are no draft grades to post")
			}
			return checkPostable(ctx, q, rows)
		},
		// A proposal to post an assignment is about the drafts waiting when
		// it was made. One entered while it waits has been in front of
		// nobody who could release it, so the proposal names the drafts it
		// was made about, beside the assignment; stillWaiting is what
		// approving it posts of them.
		Pin: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in GradePostIn) (GradePostIn, error) {
			if in.AssignmentID == nil {
				// Validate left the drafts named to Execute. A proposal
				// being made is not an approval, and is held to them as a
				// call is: nobody is asked to approve posting what could
				// not be posted.
				rows, err := gradesToPost(ctx, q, in)
				if err != nil {
					return in, err
				}
				return in, checkPostable(ctx, q, rows)
			}
			if len(in.GradeIDs) > 0 {
				return in, apperr.Invalid("give exactly one of grade_ids and assignment_id")
			}
			rows, err := gradesToPost(ctx, q, in)
			if err != nil {
				return in, err
			}
			// Validate found some, but they may have been posted since. A
			// proposal about none would name no draft, and approving it
			// would post whatever was waiting by then.
			if len(rows) == 0 {
				return in, apperr.Precondition("there are no draft grades to post")
			}
			in.GradeIDs = make([]uuid.UUID, len(rows))
			for i, g := range rows {
				in.GradeIDs[i] = g.ID
			}
			return in, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradePostIn) (GradePostOut, error) {
			// Grade ids beside the assignment are what Pin stores, and a
			// call giving both is refused. Approving a proposal, whether it
			// named its drafts or had them pinned, passes over one posted
			// meanwhile; a call naming a grade that is already posted is
			// told so by checkPostable.
			if in.pinned() && !ec.Approved {
				return GradePostOut{}, apperr.Invalid("give exactly one of grade_ids and assignment_id")
			}
			rows, err := gradesToPost(ctx, ec.Q, in)
			if err != nil {
				return GradePostOut{}, err
			}
			if ec.Approved {
				if rows, err = stillWaiting(rows); err != nil {
					return GradePostOut{}, err
				}
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
			if ec.Approved {
				if rows, err = stillWaiting(rows); err != nil {
					return GradePostOut{}, err
				}
			}
			if err := checkPostable(ctx, ec.Q, rows); err != nil {
				return GradePostOut{}, err
			}
			// With assignment_id alone — a direct call; a proposal names its
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
// assignment's ungraded work becomes a zero, until it is undone — so it spans
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
			return apperr.Precondition("grade %s is a computed total; totals are written by posting, not posted, and overridden with grade.override_total", g.ID)
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
			return apperr.Precondition("a computed total is not regraded; regrade what is beneath it, or override the total with grade.override_total")
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
			// As for grade.submit: Execute holds a call's no_rubric to the
			// rubric in force.
			if _, err = checkContent(ctx, q, s, in.GradeContent, true); err != nil {
				return err
			}
			return checkFeedbackFiles(ctx, d, q, m, in.CourseID, in.FeedbackFiles)
		},
		Pin: func(ctx context.Context, q dbq.Querier, _ *domain.Member, now time.Time, in GradeRegradeIn) (GradeRegradeIn, error) {
			if err := checkUploadAge(ctx, d, now, in.uploads()...); err != nil {
				return in, err
			}
			_, s, err := load(ctx, q, in)
			if err != nil {
				return in, err
			}
			return in, pinContent(ctx, q, s, &in.GradeContent)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradeRegradeIn) (GradeRegradeOut, error) {
			// What the work is worth is held still first, as grade.submit
			// holds it, and then the grade: a change of points takes the
			// work and then its grades, and rescales every grade it finds.
			// A regrade that came first is finished by then and its grade is
			// found; one that comes second finds the work worth what it is
			// now, and its grade replaced if it was rescaled.
			if _, s, err := load(ctx, ec.Q, in); err != nil {
				return GradeRegradeOut{}, err
			} else if err := holdWorth(ctx, ec.Q, in.CourseID, s); err != nil {
				return GradeRegradeOut{}, err
			}
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
			rubric, err := checkContent(ctx, ec.Q, s, in.GradeContent, ec.Approved)
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
	Percent     *decimal.Decimal `json:"percent" jsonschema:"out of 100, as the scheme works it out; null when nothing beneath it has a posted grade"`
	// A person's override of the total, beside what the scheme works out.
	OverridePercent *decimal.Decimal `json:"override_percent,omitempty" jsonschema:"out of 100: a person's override of this total, which counts in its place in everything rolled up above it"`
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
			"course with its percentage and the working behind it, and, beside a total a person has overridden, the " +
			"override, which counts in its place in everything above it. Nothing is stored by reading this.",
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
				if f, ok := scores.Override[c.ID]; ok {
					pct := gradecalc.Percent(f)
					line.OverridePercent = &pct
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

// ---------------------------------------------------------------------------
// A total a person overrides, or comments on
// ---------------------------------------------------------------------------

// TotalIn names one student's total on one rolled-up component.
type TotalIn struct {
	tool.InCourse
	StudentMemberID uuid.UUID `json:"student_member_id"`
	ComponentID     uuid.UUID `json:"component_id" jsonschema:"a component rolled up from what is beneath it, the course total included; a component graded directly is regraded instead"`
}

// resolveTotal: a total belongs to its student and to no single assignment,
// like a grade on a component (step 5's extra case).
func resolveTotal(ctx context.Context, q dbq.Querier, in TotalIn) (tool.Target, error) {
	if _, err := q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: in.ComponentID, CourseID: in.CourseID}); errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such grade component in this course")
	} else if err != nil {
		return tool.Target{}, err
	}
	if _, err := q.GetRosterEntry(ctx, dbq.GetRosterEntryParams{ID: in.StudentMemberID, CourseID: in.CourseID}); errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such member in this course")
	} else if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: in.CourseID, Type: "grade_component", ID: &in.ComponentID,
		Scope: authz.Target{StudentMemberIDs: []uuid.UUID{in.StudentMemberID}, SpansAssignments: true}}, nil
}

// liveTotal takes the student's totals, as every writer of them does, and
// reads the one on the component as it stands under that: it must be there
// to be overridden or commented on.
func liveTotal(ctx context.Context, ec *tool.ExecCtx, in TotalIn) (dbq.GetLiveComputedGradeRow, error) {
	if err := ec.Q.LockStudentTotals(ctx, dbq.LockStudentTotalsParams{CourseID: in.CourseID, StudentMemberID: in.StudentMemberID}); err != nil {
		return dbq.GetLiveComputedGradeRow{}, err
	}
	c, err := ec.Q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: in.ComponentID, CourseID: in.CourseID})
	if err != nil {
		return dbq.GetLiveComputedGradeRow{}, err
	}
	if c.PointsPossible.Valid {
		return dbq.GetLiveComputedGradeRow{}, apperr.Precondition("%q is graded directly: regrade its grade instead", c.Name).With("reason", "graded_directly")
	}
	live, err := ec.Q.GetLiveComputedGrade(ctx, dbq.GetLiveComputedGradeParams{ComponentID: &in.ComponentID, StudentMemberID: in.StudentMemberID})
	if errors.Is(err, pgx.ErrNoRows) {
		return live, apperr.Precondition("no total has been written down here for this student yet: one is written when a grade beneath it is posted").
			With("reason", "no_total")
	}
	return live, err
}

// rewriteTotal writes the live total again as it is but for what change
// does to the copy, superseding it with history, its feedback files carried
// on; the copy is made by the caller's action and posted now.
func rewriteTotal(ctx context.Context, ec *tool.ExecCtx, in TotalIn, live dbq.GetLiveComputedGradeRow, change func(*dbq.InsertGradeParams)) (uuid.UUID, error) {
	id := ids.New()
	if n, err := ec.Q.SupersedeGrade(ctx, dbq.SupersedeGradeParams{ID: live.ID, NewID: &id}); err != nil {
		return uuid.Nil, err
	} else if n == 0 {
		return uuid.Nil, apperr.Conflicts("the student's totals were written by someone else just now; try again")
	}
	row := dbq.InsertGradeParams{
		ID: id, StudentMemberID: in.StudentMemberID, ComponentID: &in.ComponentID, Origin: "computed",
		Score: live.Score, Feedback: live.Feedback, Breakdown: live.Breakdown,
		GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID,
		PostedAt: &ec.Now, PostedByMemberID: &ec.Member.ID, CreatedAt: ec.Now,
		OverrideScore: live.OverrideScore, OverrideReason: live.OverrideReason,
		OverrideByMemberID: live.OverrideByMemberID, OverriddenAt: live.OverriddenAt,
	}
	change(&row)
	if err := ec.Q.InsertGrade(ctx, row); err != nil {
		return uuid.Nil, err
	}
	return id, ec.Q.MoveFeedbackFiles(ctx, dbq.MoveFeedbackFilesParams{OldGradeID: &live.ID, NewGradeID: &id})
}

type GradeOverrideTotalIn struct {
	TotalIn
	Score  decimal.Decimal `json:"score" jsonschema:"out of 100, as a total's own score is"`
	Reason string          `json:"reason" jsonschema:"why, 1 to 500 characters: shown to those who grade, and kept"`
}

type TotalOut struct {
	GradeID   uuid.UUID `json:"grade_id" jsonschema:"the total as it stands now"`
	Changed   bool      `json:"changed" jsonschema:"false when the total already said this: nothing was done"`
	Snapshots int       `json:"snapshots" jsonschema:"how many totals above it were written down again"`
}

// gradeOverrideTotal puts a person's number in place of a total worked out.
// The total's own score stays what the scheme works out, beside the override,
// and every total written for it afterwards — a grade beneath posted, the
// scheme changed — carries the override on, so working the number out again
// never quietly takes a person's decision away; only grade.clear_override
// does. Above it, the override is what counts.
func gradeOverrideTotal() tool.Tool {
	return tool.Define(tool.Spec[GradeOverrideTotalIn, TotalOut]{
		Name: "grade.override_total",
		Description: "Override one student's total on a rolled-up component, the course total included, with a score out of " +
			"100 and a reason. The total worked out stays beside the override, and every total written for it later carries " +
			"the override on; in everything rolled up above it, the override counts in its place, and those totals are " +
			"written again now. A total is overridden once something beneath it has been posted. Like a regrade it writes " +
			"a grade and makes it visible at once, so it takes grade_submit and grade_post and runs at the lower of the " +
			"two; a total spans assignments, so it needs an assignment scope of the whole course. The reason and who made " +
			"the override are shown to those who grade. Overriding with what is already there changes nothing.",
		Kind: tool.Write,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/gradebook/{student_member_id}/totals/{component_id}/override"},
		Resolve: func(ctx context.Context, q dbq.Querier, in GradeOverrideTotalIn) (tool.Target, error) {
			return resolveTotal(ctx, q, in.TotalIn)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradeOverrideTotalIn) (TotalOut, error) {
			reason := strings.TrimSpace(in.Reason)
			switch {
			case in.Score.IsNegative():
				return TotalOut{}, apperr.Invalid("score cannot be negative")
			case reason == "" || utf8.RuneCountInString(reason) > 500:
				return TotalOut{}, apperr.Invalid("reason is 1 to 500 characters")
			}
			live, err := liveTotal(ctx, ec, in.TotalIn)
			if err != nil {
				return TotalOut{}, err
			}
			if live.OverrideScore.Valid && live.OverrideScore.Decimal.Equal(in.Score) && live.OverrideReason != nil && *live.OverrideReason == reason {
				return TotalOut{GradeID: live.ID}, nil
			}
			id, err := rewriteTotal(ctx, ec, in.TotalIn, live, func(row *dbq.InsertGradeParams) {
				row.OverrideScore = decimal.NullDecimal{Decimal: in.Score, Valid: true}
				row.OverrideReason, row.OverrideByMemberID, row.OverriddenAt = &reason, &ec.Member.ID, &ec.Now
			})
			if err != nil {
				return TotalOut{}, err
			}
			ec.Emit(events.Event{Type: events.GradeTotalOverridden, CourseID: &in.CourseID, SubjectType: "grade", SubjectID: &id,
				StudentMemberID: &in.StudentMemberID, Payload: map[string]any{"component_id": in.ComponentID, "replaces": live.ID}})
			return totalsAbove(ctx, ec, in.TotalIn)
		},
	})
}

// totalsAbove writes down again what is rolled up above a total a person has
// just changed, and says where the total stands.
func totalsAbove(ctx context.Context, ec *tool.ExecCtx, in TotalIn) (TotalOut, error) {
	n, err := snapshot(ctx, ec, in.CourseID, map[uuid.UUID][]uuid.UUID{in.StudentMemberID: {in.ComponentID}}, gradecalc.Policy{})
	if err != nil {
		return TotalOut{}, err
	}
	live, err := ec.Q.GetLiveComputedGrade(ctx, dbq.GetLiveComputedGradeParams{ComponentID: &in.ComponentID, StudentMemberID: in.StudentMemberID})
	return TotalOut{GradeID: live.ID, Changed: true, Snapshots: n}, err
}

func gradeClearOverride() tool.Tool {
	return tool.Define(tool.Spec[TotalIn, TotalOut]{
		Name: "grade.clear_override",
		Description: "Take a person's override off one student's total: the total worked out counts again, and what is " +
			"rolled up above it is written again now. The override stays on record in the total's history. Gated as " +
			"grade.override_total is. A total with no override is left as it is.",
		Kind: tool.Write,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/gradebook/{student_member_id}/totals/{component_id}/clear-override"},
		Resolve: func(ctx context.Context, q dbq.Querier, in TotalIn) (tool.Target, error) {
			return resolveTotal(ctx, q, in)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in TotalIn) (TotalOut, error) {
			live, err := liveTotal(ctx, ec, in)
			if err != nil {
				return TotalOut{}, err
			}
			if !live.OverrideScore.Valid {
				return TotalOut{GradeID: live.ID}, nil
			}
			id, err := rewriteTotal(ctx, ec, in, live, func(row *dbq.InsertGradeParams) {
				row.OverrideScore, row.OverrideReason, row.OverrideByMemberID, row.OverriddenAt = decimal.NullDecimal{}, nil, nil, nil
			})
			if err != nil {
				return TotalOut{}, err
			}
			ec.Emit(events.Event{Type: events.GradeTotalOverrideCleared, CourseID: &in.CourseID, SubjectType: "grade", SubjectID: &id,
				StudentMemberID: &in.StudentMemberID, Payload: map[string]any{"component_id": in.ComponentID, "replaces": live.ID}})
			return totalsAbove(ctx, ec, in)
		},
	})
}

type GradeCommentTotalIn struct {
	TotalIn
	Feedback string `json:"feedback" jsonschema:"what the student is told about this total; empty takes a comment away"`
}

// gradeCommentTotal gives a total what an entered grade has: feedback for the
// student. A comment is a release, as feedback on a posted grade is, so it is
// gated as a regrade is; the totals written for it later carry it on.
func gradeCommentTotal() tool.Tool {
	return tool.Define(tool.Spec[GradeCommentTotalIn, TotalOut]{
		Name: "grade.comment_total",
		Description: "Write feedback for a student on one of their totals on a rolled-up component, the course total " +
			"included, or take it away with an empty one. The student reads it with the total; totals written for it later " +
			"carry it on. Feedback files go on a total too, with document.create. Gated as grade.override_total is.",
		Kind: tool.Write,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/gradebook/{student_member_id}/totals/{component_id}/comment"},
		Resolve: func(ctx context.Context, q dbq.Querier, in GradeCommentTotalIn) (tool.Target, error) {
			return resolveTotal(ctx, q, in.TotalIn)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradeCommentTotalIn) (TotalOut, error) {
			live, err := liveTotal(ctx, ec, in.TotalIn)
			if err != nil {
				return TotalOut{}, err
			}
			var feedback *string
			if strings.TrimSpace(in.Feedback) != "" {
				feedback = &in.Feedback
			}
			if (feedback == nil && live.Feedback == nil) || (feedback != nil && live.Feedback != nil && *feedback == *live.Feedback) {
				return TotalOut{GradeID: live.ID}, nil
			}
			id, err := rewriteTotal(ctx, ec, in.TotalIn, live, func(row *dbq.InsertGradeParams) { row.Feedback = feedback })
			if err != nil {
				return TotalOut{}, err
			}
			ec.Emit(events.Event{Type: events.GradeTotalCommented, CourseID: &in.CourseID, SubjectType: "grade", SubjectID: &id,
				StudentMemberID: &in.StudentMemberID, Payload: map[string]any{"component_id": in.ComponentID, "replaces": live.ID}})
			return TotalOut{GradeID: id, Changed: true}, nil
		},
	})
}

// ---------------------------------------------------------------------------
// grade.undo_ungraded_as_zero
// ---------------------------------------------------------------------------

type GradeUndoUngradedAsZeroIn struct {
	tool.InCourse
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty" jsonschema:"the student whose totals count ungraded work as zero; or give all_students"`
	AllStudents     bool       `json:"all_students,omitempty" jsonschema:"every student of the course whose totals count ungraded work as zero"`
}

type GradeUndoUngradedAsZeroOut struct {
	Students  int `json:"students" jsonschema:"how many students' totals no longer count ungraded work as zero"`
	Snapshots int `json:"snapshots" jsonschema:"how many totals were written down again"`
}

// countedAsZero is who the call is about: the one student named, or every
// student whose totals count ungraded work as zero now.
func countedAsZero(ctx context.Context, q dbq.Querier, in GradeUndoUngradedAsZeroIn) ([]uuid.UUID, error) {
	if (in.StudentMemberID == nil) == !in.AllStudents {
		return nil, apperr.Invalid("give student_member_id or all_students, one of them")
	}
	if in.StudentMemberID != nil {
		if _, err := q.GetRosterEntry(ctx, dbq.GetRosterEntryParams{ID: *in.StudentMemberID, CourseID: in.CourseID}); errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.Missing("no such member in this course")
		} else if err != nil {
			return nil, err
		}
		return []uuid.UUID{*in.StudentMemberID}, nil
	}
	return q.ListStudentsCountedAsZero(ctx, in.CourseID)
}

// gradeUndoUngradedAsZero takes back posting as final: a student's totals,
// written with ungraded work counted as zero, which every post and regrade
// beneath them then kept counting so, are written again as a grade so far,
// and from then on they are what posts and regrades make them, as before
// anything was posted as final. It is posting's own undo, so it is gated as
// posting as final is: grade_post, over every student it reaches, with an
// assignment scope of the whole course. A total left with nothing to go on —
// a bucket whose only grades were those zeros — is written as having none.
func gradeUndoUngradedAsZero() tool.Tool {
	return tool.Define(tool.Spec[GradeUndoUngradedAsZeroIn, GradeUndoUngradedAsZeroOut]{
		Name: "grade.undo_ungraded_as_zero",
		Description: "Undo treat_ungraded_as_zero for one student, or for every student it was applied to: their totals " +
			"are written again, at once, leaving ungraded work out as a grade so far, and later posts and regrades no longer " +
			"count it as zero until someone posts as final again. A total with nothing left beneath it says it has none. " +
			"Gated as posting as final is: grade_post, reaching every student it is about, with an assignment scope of " +
			"the whole course. Grades themselves are not touched, and the totals it replaces stay in the history.",
		Kind: tool.Write,
		Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradePost}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/grades/undo-ungraded-as-zero"},
		Resolve: func(ctx context.Context, q dbq.Querier, in GradeUndoUngradedAsZeroIn) (tool.Target, error) {
			students, err := countedAsZero(ctx, q, in)
			if err != nil {
				return tool.Target{}, err
			}
			t := tool.Target{CourseID: in.CourseID, Type: "gradebook", Scope: authz.Target{StudentMemberIDs: students, SpansAssignments: true}}
			if in.StudentMemberID != nil {
				t.ID = in.StudentMemberID
			}
			return t, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradeUndoUngradedAsZeroIn) (GradeUndoUngradedAsZeroOut, error) {
			students, err := countedAsZero(ctx, ec.Q, in)
			if err != nil {
				return GradeUndoUngradedAsZeroOut{}, err
			}
			// Everyone it is about now, some perhaps made final since the
			// call was authorized.
			if reason, err := authz.CheckScope(ctx, ec.Q, ec.Member, authz.Target{StudentMemberIDs: students, SpansAssignments: true}); err != nil {
				return GradeUndoUngradedAsZeroOut{}, err
			} else if reason != authz.ReasonNone {
				return GradeUndoUngradedAsZeroOut{}, apperr.Forbid("a student made final since this call was authorized is outside your scope; call again").
					With("reason", string(reason))
			}
			sort.Slice(students, func(i, j int) bool { return students[i].String() < students[j].String() })
			var out GradeUndoUngradedAsZeroOut
			for _, student := range students {
				// Under the student's totals lock, which the rewrite takes
				// again: whether they are final is read as it is now.
				if err := ec.Q.LockStudentTotals(ctx, dbq.LockStudentTotalsParams{CourseID: in.CourseID, StudentMemberID: student}); err != nil {
					return GradeUndoUngradedAsZeroOut{}, err
				}
				final, err := ec.Q.StudentCountedAsZero(ctx, student)
				if err != nil {
					return GradeUndoUngradedAsZeroOut{}, err
				}
				if !final {
					if in.StudentMemberID != nil {
						return GradeUndoUngradedAsZeroOut{}, apperr.Conflicts("the student's totals do not count ungraded work as zero").
							With("reason", "not_counted_as_zero")
					}
					continue
				}
				components, err := ec.Q.ListLiveTotalComponents(ctx, student)
				if err != nil {
					return GradeUndoUngradedAsZeroOut{}, err
				}
				items := make([]uuid.UUID, 0, len(components))
				for _, c := range components {
					items = append(items, *c)
				}
				n, err := snapshotUnder(ctx, ec, in.CourseID, map[uuid.UUID][]uuid.UUID{student: items}, gradecalc.Policy{}, true)
				if err != nil {
					return GradeUndoUngradedAsZeroOut{}, err
				}
				out.Students++
				out.Snapshots += n
				s := student
				ec.Emit(events.Event{Type: events.GradeUngradedAsZeroUndone, CourseID: &in.CourseID, SubjectType: "gradebook", SubjectID: &s,
					StudentMemberID: &s})
			}
			if out.Students == 0 {
				return out, apperr.Precondition("no student's totals count ungraded work as zero").With("reason", "not_counted_as_zero")
			}
			return out, nil
		},
	})
}
