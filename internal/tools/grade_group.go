package tools

import (
	"context"
	"encoding/json"
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
//
// A third kind, peer, is peer evaluation's (docs/schema.md §2.5b): what the
// member received from their group, counted at the form's weight, worked out
// whenever their grade is written while the form counts and its window has
// closed, never carried, and never in place of a grader's own adjustment.

const adjustReplace, adjustDelta, adjustNone, adjustPeer = "replace", "delta", "none", "peer"

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
// kind of "" is none. A peer adjustment has no reason and nobody who made
// it, and says what it was worked out from (detail).
type adjustment struct {
	kind   string
	points decimal.Decimal
	reason string
	by     uuid.UUID
	detail *PeerDetail
}

func (a adjustment) none() bool { return a.kind == "" }

// manual is the adjustment a grader made: a peer adjustment is none of
// theirs, and is worked out again, never carried.
func (a adjustment) manual() adjustment {
	if a.kind == adjustPeer {
		return adjustment{}
	}
	return a
}

// same: the same adjustment, whoever made it; a peer adjustment, the same
// points from the same factor at the same weight.
func (a adjustment) same(b adjustment) bool {
	switch {
	case a.kind != b.kind:
		return false
	case a.none():
		return true
	case !a.points.Equal(b.points) || a.reason != b.reason:
		return false
	case a.kind == adjustPeer:
		return a.detail != nil && b.detail != nil && a.detail.Factor.Equal(b.detail.Factor) && a.detail.Weight == b.detail.Weight
	}
	return true
}

// columns are the grade's columns for it.
func (a adjustment) columns(row *dbq.InsertGradeParams) {
	if a.none() {
		return
	}
	kind := a.kind
	row.AdjustKind, row.AdjustPoints = &kind, decimal.NullDecimal{Decimal: a.points, Valid: true}
	if a.kind == adjustPeer {
		if a.detail != nil {
			row.AdjustDetail, _ = json.Marshal(a.detail)
		}
		return
	}
	reason, by := a.reason, a.by
	row.AdjustReason, row.AdjustByMemberID = &reason, &by
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

// withDetail is a with what a peer adjustment was worked out from, as the
// grade row keeps it (adjust_detail); one the release before carried on
// keeps none.
func (a adjustment) withDetail(raw []byte) adjustment {
	if a.kind != adjustPeer || len(raw) == 0 {
		return a
	}
	var d PeerDetail
	if json.Unmarshal(raw, &d) == nil {
		a.detail = &d
	}
	return a
}

// AdjustmentView is a member's adjustment as a grade shows it: what, by how
// much and why to the member too; who made it to those who grade.
type AdjustmentView struct {
	Kind       string          `json:"kind" jsonschema:"replace, delta, or peer: peer evaluation, counted at the form's weight"`
	Points     decimal.Decimal `json:"points" jsonschema:"replace: their score; delta and peer: what was added to the group's score"`
	Reason     *string         `json:"reason,omitempty"`
	ByMemberID *uuid.UUID      `json:"by_member_id,omitempty" jsonschema:"who made it; for those who grade"`
	Detail     *PeerDetail     `json:"detail,omitempty" jsonschema:"peer: what it was worked out from"`
}

// PeerDetail is what a peer adjustment was worked out from: the member's
// factor, what they received from their group against an even share, and
// the form's weight; to those who grade, how many raters rated them and the
// form's version as it was.
type PeerDetail struct {
	Factor      decimal.Decimal `json:"factor" jsonschema:"what the member received against an even share: 1 is even"`
	Weight      int32           `json:"weight" jsonschema:"the percentage of the grade peer evaluation moved"`
	Raters      *int            `json:"raters,omitempty" jsonschema:"how many raters rated them; for those who grade"`
	FormVersion *int32          `json:"form_version,omitempty" jsonschema:"the peer form's version it was worked out under; for those who grade"`
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
	if a.detail != nil {
		d := *a.detail
		v.Detail = &d
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
	case adjustDelta, adjustPeer:
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
// from a group grade on the work: their draft if they have one, or their
// posted grade. A peer adjustment is not carried: it is worked out again.
func carriedAdjustments(ctx context.Context, q dbq.Querier, submission uuid.UUID) (map[uuid.UUID]adjustment, error) {
	rows, err := q.ListLiveMemberGrades(ctx, &submission)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]adjustment{}
	for _, r := range rows {
		if _, seen := out[r.StudentMemberID]; seen {
			continue
		}
		if r.GroupGradeID == nil {
			out[r.StudentMemberID] = adjustment{}
			continue
		}
		out[r.StudentMemberID] = adjustmentOf(r.AdjustKind, r.AdjustPoints, r.AdjustReason, r.AdjustByMemberID).manual()
	}
	return out, nil
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
// grade it replaces (a posted one, on a regrade), and their adjustment.
type memberWrite struct {
	student  uuid.UUID
	replaces *uuid.UUID
	adj      adjustment
}

// writeGroupGrade writes the group grade c gives s's work, and each member's
// grade from it: a draft replacing their earlier drafts, or, posted, the
// grade it replaces (replaces). The feedback files go to the group grade.
// Each member is told by an event of their own. A member a grader did not
// adjust is given peer evaluation's adjustment where it counts now; it
// returns how peer evaluation stands (counted, window_open, or "").
func writeGroupGrade(ctx context.Context, d Deps, ec *tool.ExecCtx, courseID uuid.UUID, s gradeSubject, c GradeContent,
	rubric *uuid.UUID, writes []memberWrite, posted bool, createdAt time.Time) (uuid.UUID, []MemberGradeOut, string, error) {
	breakdown, err := breakdownJSON(c.Breakdown)
	if err != nil {
		return uuid.Nil, nil, "", err
	}
	max := s.pointsPossible()
	pc, err := loadPeerCount(ctx, ec.Q, courseID, s.assignment.ID, s.submission.GroupID, ec.Now)
	if err != nil {
		return uuid.Nil, nil, "", err
	}
	gg := ids.New()
	if err := ec.Q.InsertGroupGrade(ctx, dbq.InsertGroupGradeParams{ID: gg, CourseID: courseID, SubmissionID: s.submission.ID,
		Score: c.Score, OutOf: max, AllowExtra: c.AllowExtra, Feedback: c.Feedback, Breakdown: breakdown, RubricVersionID: rubric,
		GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID, CreatedAt: createdAt}); err != nil {
		return uuid.Nil, nil, "", err
	}
	out := make([]MemberGradeOut, 0, len(writes))
	for _, w := range writes {
		w.adj = pc.adjust(w.student, w.adj, c.Score, max, c.AllowExtra)
		score, err := memberScore(c.Score, w.adj, max, c.AllowExtra)
		if err != nil {
			return uuid.Nil, nil, "", err
		}
		id := ids.New()
		typ := events.GradeCreated
		payload := map[string]any{"group_grade_id": gg}
		if posted {
			// The old row first: the deferred key lets it name a row that is
			// not there yet, and the other order would be two live grades.
			if n, err := ec.Q.SupersedeGrade(ctx, dbq.SupersedeGradeParams{ID: *w.replaces, NewID: &id}); err != nil {
				return uuid.Nil, nil, "", err
			} else if n == 0 {
				return uuid.Nil, nil, "", apperr.Conflicts("grade %s was replaced by someone else just now", *w.replaces)
			}
			typ, payload["replaces"] = events.GradeRegraded, *w.replaces
		} else if err := ec.Q.SupersedeSubmissionDrafts(ctx, dbq.SupersedeSubmissionDraftsParams{NewID: &id, SubmissionID: &s.submission.ID,
			StudentMemberID: w.student}); err != nil {
			return uuid.Nil, nil, "", err
		}
		row := dbq.InsertGradeParams{ID: id, StudentMemberID: w.student, SubmissionID: &s.submission.ID, Origin: "entered", Score: score,
			Feedback: c.Feedback, Breakdown: breakdown, RubricVersionID: rubric, GraderMemberID: ec.Member.ID,
			CreatedByActionID: ec.ActionID, CreatedAt: createdAt, GroupGradeID: &gg}
		if posted {
			row.PostedAt, row.PostedByMemberID = &ec.Now, &ec.Member.ID
		}
		w.adj.columns(&row)
		if err := ec.Q.InsertGrade(ctx, row); err != nil {
			return uuid.Nil, nil, "", err
		}
		student := w.student
		ec.Emit(events.Event{Type: typ, CourseID: &courseID, SubjectType: "grade", SubjectID: &id, StudentMemberID: &student,
			AssignmentID: &s.assignment.ID, Payload: payload})
		out = append(out, MemberGradeOut{StudentMemberID: w.student, GradeID: id, Score: score, Adjustment: w.adj.view()})
	}
	return gg, out, pc.state, attachGroupFeedbackFiles(ctx, d, ec, courseID, gg, c.FeedbackFiles)
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
	Kind    string           `json:"kind" jsonschema:"replace: a score of their own; delta: plus or minus the group's; none: no adjustment of yours, their score the group's, moved by peer evaluation where it counts"`
	Points  *decimal.Decimal `json:"points,omitempty" jsonschema:"replace: their score; delta: what is added to the group's score, below zero to take away"`
	Reason  *string          `json:"reason,omitempty" jsonschema:"why, 1 to 500 characters: shown to the member, and kept"`
}

type GradeAdjustOut struct {
	GradeID    uuid.UUID       `json:"grade_id" jsonschema:"the member's grade now"`
	Replaces   *uuid.UUID      `json:"replaces,omitempty" jsonschema:"the grade it replaced; absent when nothing changed"`
	Score      decimal.Decimal `json:"score"`
	Adjustment *AdjustmentView `json:"adjustment,omitempty" jsonschema:"the member's adjustment now: yours, or, with none, peer evaluation's where it counts"`
	Changed    bool            `json:"changed" jsonschema:"false when the grade already said this: nothing was done"`
	Snapshots  int             `json:"snapshots" jsonschema:"how many of the member's posted totals were written down again"`
	Peer       string          `json:"peer,omitempty" jsonschema:"with a peer form that counts: counted, or window_open, its window still open and nothing counted yet"`
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
			"and grade_post). The score is held to zero and, unless the group grade allows extra, to the points possible. " +
			"Your adjustment wins over peer evaluation's; with none, peer evaluation counts where its form counts and its " +
			"window has closed. A grade not given from a group grade is refused (not_from_a_group_grade).",
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
			_, adj, err := a.check(in, ec.Member.ID)
			if err != nil {
				return GradeAdjustOut{}, err
			}
			pc, err := loadPeerCount(ctx, ec.Q, in.CourseID, a.subject.assignment.ID, a.subject.submission.GroupID, ec.Now)
			if err != nil {
				return GradeAdjustOut{}, err
			}
			adj = pc.adjust(a.g.StudentMemberID, adj, a.gg.Score, a.subject.pointsPossible(), a.gg.AllowExtra)
			score, err := memberScore(a.gg.Score, adj, a.subject.pointsPossible(), a.gg.AllowExtra)
			if err != nil {
				return GradeAdjustOut{}, err
			}
			full, err := ec.Q.GetGradeFull(ctx, dbq.GetGradeFullParams{ID: a.g.ID, CourseID: in.CourseID})
			if err != nil {
				return GradeAdjustOut{}, err
			}
			if was := adjustmentOf(full.AdjustKind, full.AdjustPoints, full.AdjustReason, full.AdjustByMemberID).withDetail(full.AdjustDetail); was.same(adj) && score.Equal(full.Score) {
				return GradeAdjustOut{GradeID: a.g.ID, Score: a.g.Score, Adjustment: was.view(), Peer: pc.state}, nil
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
			out := GradeAdjustOut{GradeID: id, Replaces: &a.g.ID, Score: score, Adjustment: adj.view(), Changed: true, Peer: pc.state}
			if a.g.PostedAt != nil {
				out.Snapshots, err = snapshot(ctx, ec, in.CourseID, map[uuid.UUID][]uuid.UUID{student: {a.subject.changedItem()}}, gradecalc.Policy{})
			}
			return out, err
		},
	})
}

// memberAdjustments is each member's adjustment a grade of s, as in asks,
// is written with, made by by: refused on a student's own work, for a member
// not of the work, for a score out of bounds, and, where in names the
// members, when they are not the work's.
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
	carried, err := carriedAdjustments(ctx, q, s.submission.ID)
	if err != nil {
		return nil, err
	}
	adjs, err := adjustmentsFor(s.members, carried, in.Adjustments, by)
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
	gg, grades, peer, err := writeGroupGrade(ctx, d, ec, in.CourseID, s, in.GradeContent, rubric, writes, false, ec.ActionCreatedAt)
	if err != nil {
		return GradeSubmitOut{}, err
	}
	return GradeSubmitOut{GroupGradeID: &gg, MemberGrades: grades, Peer: peer}, nil
}
