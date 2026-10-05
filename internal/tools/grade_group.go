package tools

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
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

// Grading a group's work (docs/schema.md §2.7, A group's grade). It is graded
// once: one group grade (group_grade), the shared record of what the group
// was given, by whom, with its feedback files; and each member of the work is
// given an ordinary grade from it, which is what counts — drafts, posting,
// regrades and totals are per student, as for any grade. A member's grade
// may carry an adjustment: replace (a score of their own) or delta (plus or
// minus the group's), with a reason and who made it. An adjustment is carried
// from the member's previous live grade on the same work whenever their grade
// is written again, until a call changes it; kind none takes it away.

const adjustReplace, adjustDelta, adjustNone = "replace", "delta", "none"

// maxAdjustReason is the longest reason for an adjustment, in characters.
const maxAdjustReason = 500

// AdjustmentIn is one member's adjustment, as a call names it.
type AdjustmentIn struct {
	StudentMemberID uuid.UUID        `json:"student_member_id"`
	Kind            string           `json:"kind" jsonschema:"replace: a score of their own; delta: plus or minus the group's; none: no adjustment, taking away one carried from their earlier grade"`
	Points          *decimal.Decimal `json:"points,omitempty" jsonschema:"replace: their score; delta: what is added to the group's score, below zero to take away"`
	Reason          *string          `json:"reason,omitempty" jsonschema:"why, 1 to 500 characters: shown to the member, and kept"`
}

func errBadAdjustment(format string, args ...any) error {
	return apperr.Invalid(format, args...).With("reason", ReasonBadAdjustment)
}

// checkAdjustment holds an adjustment to what it may say alone: a kind, with
// points and a reason for replace and delta, and neither for none; a
// replaced score not below zero.
func checkAdjustment(kind string, points *decimal.Decimal, reason *string) error {
	switch kind {
	case adjustNone:
		if points != nil || reason != nil {
			return errBadAdjustment("an adjustment of kind none takes no points and no reason")
		}
		return nil
	case adjustReplace, adjustDelta:
	default:
		return errBadAdjustment("an adjustment is replace, delta or none")
	}
	if points == nil {
		return errBadAdjustment("a %s adjustment says by how much: points", kind)
	}
	if kind == adjustReplace && points.IsNegative() {
		return errBadAdjustment("a replaced score cannot be negative")
	}
	if reason == nil || strings.TrimSpace(*reason) == "" || utf8.RuneCountInString(strings.TrimSpace(*reason)) > maxAdjustReason {
		return errBadAdjustment("an adjustment says why, in 1 to %d characters: reason", maxAdjustReason)
	}
	return nil
}

func checkAdjustments(adjs []AdjustmentIn) error {
	seen := map[uuid.UUID]bool{}
	for _, a := range adjs {
		if seen[a.StudentMemberID] {
			return errBadAdjustment("member %s is adjusted twice", a.StudentMemberID)
		}
		seen[a.StudentMemberID] = true
		if err := checkAdjustment(a.Kind, a.Points, a.Reason); err != nil {
			return err
		}
	}
	return nil
}

// adjustment is a member's adjustment as it is written on their grade; a
// kind of "" is none.
type adjustment struct {
	kind   string
	points decimal.Decimal
	reason string
	by     uuid.UUID
}

func (a adjustment) none() bool { return a.kind == "" }

// same: the same adjustment, whoever made it.
func (a adjustment) same(b adjustment) bool {
	return a.kind == b.kind && (a.none() || (a.points.Equal(b.points) && a.reason == b.reason))
}

// columns are the grade's columns for it.
func (a adjustment) columns(row *dbq.InsertGradeParams) {
	if a.none() {
		return
	}
	kind, reason, by := a.kind, a.reason, a.by
	row.AdjustKind, row.AdjustPoints, row.AdjustReason, row.AdjustByMemberID = &kind, decimal.NullDecimal{Decimal: a.points, Valid: true}, &reason, &by
}

// named is the adjustment a call names, made by by; none for kind none.
func named(a AdjustmentIn, by uuid.UUID) adjustment {
	if a.Kind == adjustNone {
		return adjustment{}
	}
	return adjustment{kind: a.Kind, points: *a.Points, reason: strings.TrimSpace(*a.Reason), by: by}
}

// adjustmentOf is the adjustment a grade row carries.
func adjustmentOf(kind *string, points decimal.NullDecimal, reason *string, by *uuid.UUID) adjustment {
	if kind == nil || !points.Valid {
		return adjustment{}
	}
	a := adjustment{kind: *kind, points: points.Decimal}
	if reason != nil {
		a.reason = *reason
	}
	if by != nil {
		a.by = *by
	}
	return a
}

// AdjustmentView is a member's adjustment as a grade shows it: what, by how
// much and why to the member too; who made it to those who grade.
type AdjustmentView struct {
	Kind       string          `json:"kind" jsonschema:"replace or delta"`
	Points     decimal.Decimal `json:"points" jsonschema:"replace: their score; delta: what was added to the group's score"`
	Reason     *string         `json:"reason,omitempty"`
	ByMemberID *uuid.UUID      `json:"by_member_id,omitempty" jsonschema:"who made it; for those who grade"`
}

func (a adjustment) view() *AdjustmentView {
	if a.none() {
		return nil
	}
	v := &AdjustmentView{Kind: a.kind, Points: a.points}
	if a.reason != "" {
		r := a.reason
		v.Reason = &r
	}
	if a.by != uuid.Nil {
		by := a.by
		v.ByMemberID = &by
	}
	return v
}

// memberScore is a member's score from group score g: g, a replaced score,
// or g with a delta, never below zero, and above max only where the grade
// allows extra credit.
func memberScore(g decimal.Decimal, a adjustment, max decimal.Decimal, allowExtra bool) (decimal.Decimal, error) {
	score := g
	switch a.kind {
	case adjustReplace:
		score = a.points
	case adjustDelta:
		score = g.Add(a.points)
	}
	if score.IsNegative() {
		return score, apperr.Precondition("the adjustment would give a score of %s, below zero", score).With("reason", ReasonAdjustedBelowZero)
	}
	if !a.none() && score.GreaterThan(max) && !allowExtra {
		return score, apperr.Precondition("the adjustment would give a score of %s, above the %s points possible; set allow_extra to permit it", score, max).
			With("reason", ReasonAdjustedAbovePoints)
	}
	return score, nil
}

// carriedAdjustments are the adjustments of each member's latest live grade
// from a group grade on the work, of its live grades (ListLiveMemberGrades):
// their draft if they have one, or their posted grade.
func carriedAdjustments(live []dbq.ListLiveMemberGradesRow) map[uuid.UUID]adjustment {
	out := map[uuid.UUID]adjustment{}
	for _, r := range live {
		if _, seen := out[r.StudentMemberID]; seen {
			continue
		}
		if r.GroupGradeID == nil {
			out[r.StudentMemberID] = adjustment{}
			continue
		}
		out[r.StudentMemberID] = adjustmentOf(r.AdjustKind, r.AdjustPoints, r.AdjustReason, r.AdjustByMemberID)
	}
	return out
}

// errGroupGradePosted refuses a new group grade for work with a grade posted
// on it, which a regrade changes: a draft beside a posted grade could never
// be posted (checkPostable), and would hold up posting the assignment.
func errGroupGradePosted(grade uuid.UUID) error {
	return apperr.Precondition("the group's grade on this work is posted: regrade it with grade.regrade, which gives a member "+
		"added to the work since a grade from it as well, or change one member's with grade.adjust").
		With("reason", ReasonGroupGradePosted).With("grade_id", grade)
}

// errDraftChangedMeanwhile refuses a group grade entered while a member's
// draft was replaced by another call (grade.adjust): the adjustment this one
// would carry is the one it found before, not the one written meanwhile, and
// its draft would be a second beside that one.
var errDraftChangedMeanwhile = apperr.Conflicts("a member's draft grade on this work was replaced while this call was being "+
	"made; look again, and call again").With("reason", ReasonGradesChanged)

// noGradeLeft refuses writing a member's new draft from a group grade beside
// a live grade of theirs on the work, asked once their drafts are
// superseded. memberAdjustments looked under the work's lock, but grade.post
// and grade.adjust take the grades and not the work: a draft being posted
// then is passed over by the supersede, which waits for the post and then
// finds it posted, and the draft grade.adjust writes in place of one is
// never seen by it. Either would be left live beside the new draft, and the
// first could never be posted.
func noGradeLeft(ctx context.Context, q dbq.Querier, submission, student uuid.UUID) error {
	live, err := q.ListLiveGradesOfMemberOnWork(ctx, dbq.ListLiveGradesOfMemberOnWorkParams{SubmissionID: &submission, StudentMemberID: student})
	switch {
	case err != nil || len(live) == 0:
		return err
	case live[0].PostedAt != nil:
		return errGroupGradePosted(live[0].ID)
	}
	return errDraftChangedMeanwhile
}

// adjustmentsFor is each member's adjustment: the one the call names, or
// else the one carried; one named the same as the one carried keeps who made
// it. A member named who is not a member of the work is refused.
func adjustmentsFor(members []uuid.UUID, carried map[uuid.UUID]adjustment, call []AdjustmentIn, by uuid.UUID) (map[uuid.UUID]adjustment, error) {
	out := make(map[uuid.UUID]adjustment, len(members))
	for _, m := range members {
		out[m] = carried[m]
	}
	for _, a := range call {
		if !slices.Contains(members, a.StudentMemberID) {
			return nil, apperr.Precondition("%s is not a member of this work", a.StudentMemberID).
				With("reason", ReasonNotAMemberOfWork).With("member_id", a.StudentMemberID)
		}
		n := named(a, by)
		if !n.same(carried[a.StudentMemberID]) {
			out[a.StudentMemberID] = n
		}
	}
	return out, nil
}

// pinnedAdjustments names every member's adjustment, as it will be written:
// what a proposal stores, so that approving it writes what was proposed.
func pinnedAdjustments(members []uuid.UUID, adjs map[uuid.UUID]adjustment) []AdjustmentIn {
	out := make([]AdjustmentIn, 0, len(members))
	for _, m := range members {
		a := adjs[m]
		in := AdjustmentIn{StudentMemberID: m, Kind: adjustNone}
		if !a.none() {
			points, reason := a.points, a.reason
			in.Kind, in.Points, in.Reason = a.kind, &points, &reason
		}
		out = append(out, in)
	}
	return out
}

// MemberGradeOut is one member's grade given from a group grade.
type MemberGradeOut struct {
	StudentMemberID uuid.UUID       `json:"student_member_id"`
	GradeID         uuid.UUID       `json:"grade_id"`
	Score           decimal.Decimal `json:"score"`
	Adjustment      *AdjustmentView `json:"adjustment,omitempty"`
}

// checkMemberScores holds each member's score from group score g to its
// bounds.
func checkMemberScores(g decimal.Decimal, adjs map[uuid.UUID]adjustment, max decimal.Decimal, allowExtra bool) error {
	for _, a := range adjs {
		if _, err := memberScore(g, a, max, allowExtra); err != nil {
			return err
		}
	}
	return nil
}

// memberWrite is one member's grade a group grade writes: for whom, the
// grade it replaces (a posted one, on a regrade; none for a member added to
// the work since it was graded), and their adjustment.
type memberWrite struct {
	student  uuid.UUID
	replaces *uuid.UUID
	adj      adjustment
}

// writeGroupGrade writes the group grade c gives s's work, and each member's
// grade from it: a draft replacing their earlier drafts, or, posted, the
// grade it replaces (replaces), or the first they are given on the work.
// The feedback files go to the group grade. Each member is told by an event
// of their own.
func writeGroupGrade(ctx context.Context, d Deps, ec *tool.ExecCtx, courseID uuid.UUID, s gradeSubject, c GradeContent,
	rubric *uuid.UUID, writes []memberWrite, posted bool, createdAt time.Time) (uuid.UUID, []MemberGradeOut, error) {
	breakdown, err := breakdownJSON(c.Breakdown)
	if err != nil {
		return uuid.Nil, nil, err
	}
	max := s.pointsPossible()
	gg := ids.New()
	if err := ec.Q.InsertGroupGrade(ctx, dbq.InsertGroupGradeParams{ID: gg, CourseID: courseID, SubmissionID: s.submission.ID,
		Score: c.Score, OutOf: max, AllowExtra: c.AllowExtra, Feedback: c.Feedback, Breakdown: breakdown, RubricVersionID: rubric,
		GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID, CreatedAt: createdAt}); err != nil {
		return uuid.Nil, nil, err
	}
	out := make([]MemberGradeOut, 0, len(writes))
	for _, w := range writes {
		score, err := memberScore(c.Score, w.adj, max, c.AllowExtra)
		if err != nil {
			return uuid.Nil, nil, err
		}
		id := ids.New()
		typ := events.GradeCreated
		payload := map[string]any{"group_grade_id": gg}
		switch {
		case posted && w.replaces == nil:
			// Added to the work since it was graded: given the group's
			// grade, posted at once, as the others' are.
			typ = events.GradePosted
		case posted:
			// The old row first: the deferred key lets it name a row that is
			// not there yet, and the other order would be two live grades.
			if n, err := ec.Q.SupersedeGrade(ctx, dbq.SupersedeGradeParams{ID: *w.replaces, NewID: &id}); err != nil {
				return uuid.Nil, nil, err
			} else if n == 0 {
				return uuid.Nil, nil, apperr.Conflicts("grade %s was replaced by someone else just now", *w.replaces)
			}
			typ, payload["replaces"] = events.GradeRegraded, *w.replaces
		default:
			if err := ec.Q.SupersedeSubmissionDrafts(ctx, dbq.SupersedeSubmissionDraftsParams{NewID: &id, SubmissionID: &s.submission.ID,
				StudentMemberID: w.student}); err != nil {
				return uuid.Nil, nil, err
			}
			if err := noGradeLeft(ctx, ec.Q, s.submission.ID, w.student); err != nil {
				return uuid.Nil, nil, err
			}
		}
		row := dbq.InsertGradeParams{ID: id, StudentMemberID: w.student, SubmissionID: &s.submission.ID, Origin: "entered", Score: score,
			Feedback: c.Feedback, Breakdown: breakdown, RubricVersionID: rubric, GraderMemberID: ec.Member.ID,
			CreatedByActionID: ec.ActionID, CreatedAt: createdAt, GroupGradeID: &gg}
		if posted {
			row.PostedAt, row.PostedByMemberID = &ec.Now, &ec.Member.ID
		}
		w.adj.columns(&row)
		if err := ec.Q.InsertGrade(ctx, row); err != nil {
			return uuid.Nil, nil, err
		}
		student := w.student
		ec.Emit(events.Event{Type: typ, CourseID: &courseID, SubjectType: "grade", SubjectID: &id, StudentMemberID: &student,
			AssignmentID: &s.assignment.ID, Payload: payload})
		out = append(out, MemberGradeOut{StudentMemberID: w.student, GradeID: id, Score: score, Adjustment: w.adj.view()})
	}
	return gg, out, attachGroupFeedbackFiles(ctx, d, ec, courseID, gg, c.FeedbackFiles)
}

// attachGroupFeedbackFiles records each file as a feedback document of the
// group grade: the group's shared feedback.
func attachGroupFeedbackFiles(ctx context.Context, d Deps, ec *tool.ExecCtx, courseID, groupGradeID uuid.UUID, files []FeedbackFile) error {
	for i, f := range files {
		doc := ids.New()
		if err := ec.Q.InsertDocument(ctx, dbq.InsertDocumentParams{ID: doc, CourseID: courseID, Kind: kindFeedback,
			Title: f.Title, GroupGradeID: &groupGradeID, SortOrder: int32(i), CreatedAt: ec.Now}); err != nil {
			return err
		}
		v, _, err := insertVersion(ctx, d, ec, courseID, doc, kindFeedback, f.Title, 1, f.content())
		if err != nil {
			return err
		}
		if err := ec.Q.SetPublishedVersion(ctx, dbq.SetPublishedVersionParams{ID: doc, PublishedVersionID: &v}); err != nil {
			return err
		}
	}
	return nil
}

// sortedMembers are a work's members in a fixed order, so that a group grade
// writes its members' grades, and takes their locks, the same way each time.
func sortedMembers(members []uuid.UUID) []uuid.UUID {
	out := slices.Clone(members)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// ---------------------------------------------------------------------------
// grade.adjust
// ---------------------------------------------------------------------------

type GradeAdjustIn struct {
	tool.InCourse
	GradeID uuid.UUID        `json:"grade_id" jsonschema:"a member's live grade given from a group grade, a draft or posted"`
	Kind    string           `json:"kind" jsonschema:"replace: a score of their own; delta: plus or minus the group's; none: no adjustment, their score the group's"`
	Points  *decimal.Decimal `json:"points,omitempty" jsonschema:"replace: their score; delta: what is added to the group's score, below zero to take away"`
	Reason  *string          `json:"reason,omitempty" jsonschema:"why, 1 to 500 characters: shown to the member, and kept"`
}

type GradeAdjustOut struct {
	GradeID   uuid.UUID       `json:"grade_id" jsonschema:"the member's grade now"`
	Replaces  *uuid.UUID      `json:"replaces,omitempty" jsonschema:"the grade it replaced; absent when nothing changed"`
	Score     decimal.Decimal `json:"score"`
	Changed   bool            `json:"changed" jsonschema:"false when the grade already said this: nothing was done"`
	Snapshots int             `json:"snapshots" jsonschema:"how many of the member's posted totals were written down again"`
}

// adjustable is the grade grade.adjust names, the group grade it was given
// from and its work, refused when it is not a member's live grade from a
// group grade.
type adjustable struct {
	g       dbq.GetGradesInCourseRow
	gg      dbq.GroupGrade
	subject gradeSubject
}

func loadAdjustable(ctx context.Context, q dbq.Querier, in GradeAdjustIn) (adjustable, error) {
	var a adjustable
	rows, err := q.GetGradesInCourse(ctx, dbq.GetGradesInCourseParams{Ids: []uuid.UUID{in.GradeID}, CourseID: in.CourseID})
	if err != nil {
		return a, err
	}
	if len(rows) == 0 {
		return a, apperr.Missing("no such grade in this course")
	}
	a.g = rows[0]
	if a.g.GroupGradeID == nil || a.g.SubmissionID == nil {
		return a, apperr.Precondition("the grade was not given from a group grade: change it with grade.submit or grade.regrade").
			With("reason", ReasonNotFromAGroupGrade)
	}
	if a.gg, err = q.GetGroupGrade(ctx, dbq.GetGroupGradeParams{ID: *a.g.GroupGradeID, CourseID: in.CourseID}); err != nil {
		return a, workGone(err)
	}
	a.subject, err = loadSubject(ctx, q, in.CourseID, a.g.SubmissionID, nil, nil)
	return a, err
}

// errPostedMeanwhile refuses grade.adjust of a grade that was a draft when
// the call was decided, and has been posted since.
var errPostedMeanwhile = apperr.Conflicts("the grade was posted while this call was being made, and changing a posted grade "+
	"takes grade_post as well; call again").With("reason", ReasonPostedMeanwhile)

// check refuses adjusting a's grade as in says: it has been replaced, or is
// a computed total, or the score would go out of bounds. It returns the new
// score and the adjustment.
func (a adjustable) check(in GradeAdjustIn, by uuid.UUID) (decimal.Decimal, adjustment, error) {
	if a.g.SupersededBy != nil {
		return decimal.Zero, adjustment{}, apperr.Conflicts("the grade has been replaced; adjust the grade that replaced it")
	}
	adj := named(AdjustmentIn{StudentMemberID: a.g.StudentMemberID, Kind: in.Kind, Points: in.Points, Reason: in.Reason}, by)
	score, err := memberScore(a.gg.Score, adj, a.subject.pointsPossible(), a.gg.AllowExtra)
	return score, adj, err
}

func gradeAdjust() tool.Tool {
	return tool.Define(tool.Spec[GradeAdjustIn, GradeAdjustOut]{
		Name: "grade.adjust",
		Description: "Adjust one member's grade given from a group grade: a score of their own (replace), plus or minus the " +
			"group's (delta), with a reason, which the member reads with their grade, or none, taking an adjustment " +
			"away. A draft gets a new draft in its place (gated as grade.submit); a posted grade a new posted grade, the " +
			"old kept as history and the member's totals written again (gated as grade.regrade, the lower of grade_submit " +
			"and grade_post); a draft posted while the call is being made is refused (posted_meanwhile), and called again " +
			"is gated as a regrade. The score is held to zero and, unless the group grade allows extra, to the points " +
			"possible. A grade not given from a group grade is refused (not_from_a_group_grade).",
		Kind:  tool.Write,
		Gate:  tool.Gate{Any: true, Perms: []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}},
		HTTP:  tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/grades/{grade_id}/adjust"},
		Check: func(in GradeAdjustIn) error { return checkAdjustment(in.Kind, in.Points, in.Reason) },
		Resolve: func(ctx context.Context, q dbq.Querier, in GradeAdjustIn) (tool.Target, error) {
			rows, err := q.GetGradesInCourse(ctx, dbq.GetGradesInCourseParams{Ids: []uuid.UUID{in.GradeID}, CourseID: in.CourseID})
			if err != nil {
				return tool.Target{}, err
			}
			if len(rows) == 0 {
				return tool.Target{}, apperr.Missing("no such grade in this course")
			}
			g := rows[0]
			t := tool.Target{CourseID: in.CourseID, Type: "grade", ID: &g.ID, Perms: []domain.Perm{domain.PermGradeSubmit},
				Scope: authz.Target{StudentMemberIDs: []uuid.UUID{g.StudentMemberID}}}
			if g.AssignmentID != nil {
				t.Scope.AssignmentIDs = []uuid.UUID{*g.AssignmentID}
			} else {
				t.Scope.SpansAssignments = true
			}
			if g.PostedAt != nil {
				// Changing a posted grade writes one and shows it at once.
				t.Perms = []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}
			}
			return t, nil
		},
		Validate: func(ctx context.Context, q dbq.Querier, m *domain.Member, _ time.Time, in GradeAdjustIn) error {
			a, err := loadAdjustable(ctx, q, in)
			if err != nil {
				return err
			}
			_, _, err = a.check(in, m.ID)
			return err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradeAdjustIn) (GradeAdjustOut, error) {
			// What the work is worth is held still first, then the grade, as
			// a regrade takes them.
			a, err := loadAdjustable(ctx, ec.Q, in)
			if err != nil {
				return GradeAdjustOut{}, err
			}
			if err := holdWorth(ctx, ec.Q, in.CourseID, a.subject); err != nil {
				return GradeAdjustOut{}, err
			}
			if _, err := ec.Q.LockGradesInCourse(ctx, dbq.LockGradesInCourseParams{Ids: []uuid.UUID{in.GradeID}, CourseID: in.CourseID}); err != nil {
				return GradeAdjustOut{}, err
			}
			if a, err = loadAdjustable(ctx, ec.Q, in); err != nil {
				return GradeAdjustOut{}, err
			}
			// Which permissions govern was decided from the grade as Resolve
			// found it, before the lock. A draft posted since is changed as a
			// regrade is, and the call was not decided as one: otherwise a
			// seat that grades but does not post would replace a posted
			// grade. Called again, it is decided as one; an approval is
			// decided again as it is carried out.
			if a.g.PostedAt != nil && !slices.Contains(ec.Perms, domain.PermGradePost) {
				return GradeAdjustOut{}, errPostedMeanwhile
			}
			score, adj, err := a.check(in, ec.Member.ID)
			if err != nil {
				return GradeAdjustOut{}, err
			}
			full, err := ec.Q.GetGradeFull(ctx, dbq.GetGradeFullParams{ID: a.g.ID, CourseID: in.CourseID})
			if err != nil {
				return GradeAdjustOut{}, err
			}
			if adjustmentOf(full.AdjustKind, full.AdjustPoints, full.AdjustReason, full.AdjustByMemberID).same(adj) {
				return GradeAdjustOut{GradeID: a.g.ID, Score: a.g.Score}, nil
			}
			id := ids.New()
			if n, err := ec.Q.SupersedeGrade(ctx, dbq.SupersedeGradeParams{ID: a.g.ID, NewID: &id}); err != nil {
				return GradeAdjustOut{}, err
			} else if n == 0 {
				return GradeAdjustOut{}, apperr.Conflicts("the grade was replaced by someone else just now")
			}
			row := dbq.InsertGradeParams{ID: id, StudentMemberID: a.g.StudentMemberID, SubmissionID: a.g.SubmissionID, Origin: "entered",
				Score: score, Feedback: full.Feedback, Breakdown: full.Breakdown, RubricVersionID: full.RubricVersionID,
				GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID, CreatedAt: ec.Now, GroupGradeID: a.g.GroupGradeID}
			typ := events.GradeCreated
			if a.g.PostedAt != nil {
				row.PostedAt, row.PostedByMemberID, typ = &ec.Now, &ec.Member.ID, events.GradeRegraded
			}
			adj.columns(&row)
			if err := ec.Q.InsertGrade(ctx, row); err != nil {
				return GradeAdjustOut{}, err
			}
			// Feedback files of a member's own grade go with it.
			if err := ec.Q.MoveFeedbackFiles(ctx, dbq.MoveFeedbackFilesParams{OldGradeID: &a.g.ID, NewGradeID: &id}); err != nil {
				return GradeAdjustOut{}, err
			}
			student := a.g.StudentMemberID
			ec.Emit(events.Event{Type: typ, CourseID: &in.CourseID, SubjectType: "grade", SubjectID: &id, StudentMemberID: &student,
				AssignmentID: a.g.AssignmentID, Payload: map[string]any{"replaces": a.g.ID, "group_grade_id": a.gg.ID, "adjusted": true}})
			out := GradeAdjustOut{GradeID: id, Replaces: &a.g.ID, Score: score, Changed: true}
			if a.g.PostedAt != nil {
				out.Snapshots, err = snapshot(ctx, ec, in.CourseID, map[uuid.UUID][]uuid.UUID{student: {a.subject.changedItem()}}, gradecalc.Policy{})
			}
			return out, err
		},
	})
}

// memberAdjustments is each member's adjustment a grade of s, as in asks,
// is written with, made by by: refused on a student's own work, once a
// grade on the work is posted, for a member not of the work, for a score
// out of bounds, and, where in names the members, when they are not the
// work's.
func (in GradeSubmitIn) memberAdjustments(ctx context.Context, q dbq.Querier, s gradeSubject, by uuid.UUID) (map[uuid.UUID]adjustment, error) {
	if !s.group() {
		if len(in.Adjustments) > 0 || in.Members != nil {
			return nil, errNotAGroupAssignment
		}
		return nil, nil
	}
	if in.Members != nil && !sameMembers(in.Members, s.members) {
		return nil, errMembersChanged
	}
	live, err := q.ListLiveMemberGrades(ctx, &s.submission.ID)
	if err != nil {
		return nil, err
	}
	for _, g := range live {
		if g.PostedAt != nil {
			return nil, errGroupGradePosted(g.ID)
		}
	}
	adjs, err := adjustmentsFor(s.members, carriedAdjustments(live), in.Adjustments, by)
	if err != nil {
		return nil, err
	}
	return adjs, checkMemberScores(in.Score, adjs, s.pointsPossible(), in.AllowExtra)
}

// gradeGroupWork is grade.submit on a group's work, under the work's lock:
// its group grade, and a draft for each member of the work from it.
func (in GradeSubmitIn) gradeGroupWork(ctx context.Context, d Deps, ec *tool.ExecCtx, s gradeSubject, rubric *uuid.UUID) (GradeSubmitOut, error) {
	// Whose work it is, as it is under the lock: a correction of it takes
	// the same lock.
	var err error
	if s.members, err = workStudents(ctx, ec.Q, s.submission.ID); err != nil {
		return GradeSubmitOut{}, err
	}
	adjs, err := in.memberAdjustments(ctx, ec.Q, s, ec.Member.ID)
	if err != nil {
		return GradeSubmitOut{}, err
	}
	writes := make([]memberWrite, 0, len(s.members))
	for _, m := range sortedMembers(s.members) {
		writes = append(writes, memberWrite{student: m, adj: adjs[m]})
	}
	gg, grades, err := writeGroupGrade(ctx, d, ec, in.CourseID, s, in.GradeContent, rubric, writes, false, ec.ActionCreatedAt)
	if err != nil {
		return GradeSubmitOut{}, err
	}
	return GradeSubmitOut{GroupGradeID: &gg, MemberGrades: grades}, nil
}
