package tools

import (
	"context"
	"slices"
	"time"

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

// Counting peer evaluation in grades (docs/schema.md §2.5b). A member's grade
// given from a group grade is written with a peer adjustment whenever it is
// written while the form counts and its window has closed (grade.submit,
// .regrade, .adjust, a rescale); grade.apply_peer writes again, at once,
// every live grade of an assignment whose peer adjustment would change: after
// the window closes, or the weight changes.

// PinnedFactor is a grade counting peer evaluation writes again, and the
// member's factor: what a proposal of it records.
type PinnedFactor struct {
	GradeID         uuid.UUID        `json:"grade_id"`
	StudentMemberID uuid.UUID        `json:"student_member_id"`
	Factor          *decimal.Decimal `json:"factor,omitempty" jsonschema:"absent: the member's peer adjustment taken away, nobody having rated them"`
}

type GradeApplyPeerIn struct {
	tool.InCourse
	AssignmentID uuid.UUID `json:"assignment_id"`
	// Recorded by a proposal.
	FormVersion *int32         `json:"form_version,omitempty" jsonschema:"filled in when it is proposed, never by a call: the form's version then"`
	Grades      []PinnedFactor `json:"grades,omitempty" jsonschema:"filled in when it is proposed, never by a call: each grade it writes again, and the member's factor; approving it is refused if any has changed since, or the form has (grades_changed)"`
}

// PeerGradeWritten is one member's grade written again.
type PeerGradeWritten struct {
	StudentMemberID uuid.UUID       `json:"student_member_id"`
	GradeID         uuid.UUID       `json:"grade_id"`
	Replaces        uuid.UUID       `json:"replaces"`
	Before          decimal.Decimal `json:"before" jsonschema:"the score it replaced"`
	Score           decimal.Decimal `json:"score"`
	Adjustment      *AdjustmentView `json:"adjustment,omitempty" jsonschema:"its peer adjustment; absent when it has none now"`
	Posted          bool            `json:"posted" jsonschema:"a posted grade written posted, the member's totals written again; otherwise a draft written as a draft"`
}

type GradeApplyPeerOut struct {
	Written   []PeerGradeWritten `json:"written"`
	Snapshots int                `json:"snapshots" jsonschema:"how many of the members' posted totals were written down again"`
}

// peerWrite is one grade counting peer evaluation writes again: the grade,
// its new adjustment and score.
type peerWrite struct {
	g     dbq.ListLiveGradesFromGroupGradesRow
	adj   adjustment
	score decimal.Decimal
}

func (w peerWrite) factor() *decimal.Decimal {
	if w.adj.detail == nil {
		return nil
	}
	f := w.adj.detail.Factor
	return &f
}

// peerPlan is what counting peer evaluation on form f writes again of
// assignment a's grades: every live grade given from a group grade whose
// peer adjustment, worked out now, differs from the one it has, but one a
// grader adjusted, whose adjustment wins.
func peerPlan(ctx context.Context, q dbq.Querier, a dbq.GetAssignmentInCourseRow, f dbq.PeerForm) ([]peerWrite, error) {
	rows, err := q.ListLiveGradesFromGroupGrades(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	counts := map[uuid.UUID]peerCount{}
	var out []peerWrite
	for _, g := range rows {
		now := adjustmentOf(g.AdjustKind, g.AdjustPoints, g.AdjustReason, g.AdjustByMemberID).withDetail(g.AdjustDetail)
		if !now.manual().none() {
			continue
		}
		pc, ok := counts[g.GroupID]
		if !ok {
			if pc, err = countFor(ctx, q, f, g.GroupID); err != nil {
				return nil, err
			}
			counts[g.GroupID] = pc
		}
		next := pc.adjust(g.StudentMemberID, adjustment{}, g.GroupScore, a.PointsPossible, g.AllowExtra)
		score, err := memberScore(g.GroupScore, next, a.PointsPossible, g.AllowExtra)
		if err != nil {
			return nil, err
		}
		if next.same(now) && score.Equal(g.Score) {
			continue
		}
		out = append(out, peerWrite{g: g, adj: next, score: score})
	}
	return out, nil
}

// counted refuses counting peer evaluation on f, at now: no form, one that
// does not count, or one whose window is open.
func counted(f *dbq.PeerForm, now time.Time) error {
	switch {
	case f == nil:
		return errNoPeerForm()
	case !formCounts(f):
		return apperr.Precondition("the peer form does not count in grades: it is switched off, or its weight is 0").
			With("reason", ReasonPeerNotCounted)
	case !formClosed(*f, now):
		return apperr.Precondition("peer evaluation is open until %s; it counts once it has closed", f.ClosesAt.Format(time.RFC3339)).
			With("reason", ReasonWindowOpen).With("closes_at", f.ClosesAt)
	}
	return nil
}

// errPeerGradesChanged refuses approving grade.apply_peer once a grade it
// would write again, or the form, has changed since it was proposed.
func errPeerGradesChanged() error {
	return apperr.Conflicts("the grades, or the peer form, have changed since this was proposed; look again, and propose it again").
		With("reason", ReasonGradesChanged)
}

// samePlan says whether plan writes what in, a proposal, recorded.
func (in GradeApplyPeerIn) samePlan(f dbq.PeerForm, plan []peerWrite) bool {
	if in.FormVersion == nil || *in.FormVersion != f.Version || len(in.Grades) != len(plan) {
		return false
	}
	pinned := map[uuid.UUID]PinnedFactor{}
	for _, p := range in.Grades {
		pinned[p.GradeID] = p
	}
	for _, w := range plan {
		p, ok := pinned[w.g.ID]
		if !ok || p.StudentMemberID != w.g.StudentMemberID {
			return false
		}
		got := w.factor()
		if (got == nil) != (p.Factor == nil) || (got != nil && !got.Equal(*p.Factor)) {
			return false
		}
	}
	return true
}

func planStudents(plan []peerWrite) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(plan))
	for _, w := range plan {
		out = append(out, w.g.StudentMemberID)
	}
	return dedupe(out)
}

func gradeApplyPeer() tool.Tool {
	load := func(ctx context.Context, q dbq.Querier, in GradeApplyPeerIn) (dbq.GetAssignmentInCourseRow, *dbq.PeerForm, error) {
		a, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
		if err != nil {
			return a, nil, goneIfNoRows(ctx, q, in.CourseID, in.AssignmentID, err)
		}
		f, err := loadPeerForm(ctx, q, in.CourseID, a.ID)
		return a, f, err
	}
	return tool.Define(tool.Spec[GradeApplyPeerIn, GradeApplyPeerOut]{
		Name: "grade.apply_peer",
		Description: "Count peer evaluation in an assignment's grades now: every live grade given from a group grade whose peer " +
			"adjustment would change is written again — a draft as a draft, a posted grade posted, the old kept and the " +
			"member's totals written again — with what the member received, against an even share, moving their score from " +
			"the group's at the form's weight. A grader's own adjustment (replace or delta) is left as it is: it wins. For " +
			"after the window closes, or the weight changes; refused while it is open (window_open), or when the form does " +
			"not count (peer_not_counted: switched off, or a weight of 0). Gated as a regrade, the lower of grade_submit and " +
			"grade_post; it reaches every member whose grade it writes. A proposal records each member's factor, and approving " +
			"it is refused if a grade it would replace, or the form, has changed since (grades_changed).",
		Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}},
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/apply-peer"},
		CheckCall: func(in GradeApplyPeerIn) error {
			if in.FormVersion != nil || in.Grades != nil {
				return apperr.Invalid("form_version and grades are what a proposal records; a call gives neither")
			}
			return nil
		},
		Resolve: func(ctx context.Context, q dbq.Querier, in GradeApplyPeerIn) (tool.Target, error) {
			t, err := assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
			if err != nil {
				return t, err
			}
			a, f, err := load(ctx, q, in)
			if err != nil || !formCounts(f) {
				return t, err
			}
			plan, err := peerPlan(ctx, q, a, *f)
			t.Scope.StudentMemberIDs = planStudents(plan)
			return t, err
		},
		Validate: func(ctx context.Context, q dbq.Querier, _ *domain.Member, now time.Time, in GradeApplyPeerIn) error {
			_, f, err := load(ctx, q, in)
			if err != nil {
				return err
			}
			return counted(f, now)
		},
		Pin: func(ctx context.Context, q dbq.Querier, _ *domain.Member, _ time.Time, in GradeApplyPeerIn) (GradeApplyPeerIn, error) {
			a, f, err := load(ctx, q, in)
			if err != nil {
				return in, err
			}
			if f == nil {
				return in, errNoPeerForm()
			}
			plan, err := peerPlan(ctx, q, a, *f)
			if err != nil {
				return in, err
			}
			if len(plan) == 0 {
				return in, apperr.Precondition("every grade counts peer evaluation as it stands already: there is nothing to write")
			}
			v := f.Version
			in.FormVersion, in.Grades = &v, make([]PinnedFactor, len(plan))
			for i, w := range plan {
				in.Grades[i] = PinnedFactor{GradeID: w.g.ID, StudentMemberID: w.g.StudentMemberID, Factor: w.factor()}
			}
			return in, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in GradeApplyPeerIn) (GradeApplyPeerOut, error) {
			// What the work is worth held still first, as a regrade holds
			// it; then the form, as a change of it holds it FOR UPDATE; then
			// the grades, in id order, as grade.post holds them.
			sa, err := ec.Q.ShareAssignmentForGrading(ctx, dbq.ShareAssignmentForGradingParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return GradeApplyPeerOut{}, goneIfNoRows(ctx, ec.Q, in.CourseID, in.AssignmentID, err)
			}
			a := dbq.GetAssignmentInCourseRow(sa)
			f, err := sharePeerForm(ctx, ec.Q, in.CourseID, a.ID)
			if err != nil {
				return GradeApplyPeerOut{}, err
			}
			if err := counted(f, ec.Now); err != nil {
				return GradeApplyPeerOut{}, err
			}
			rows, err := ec.Q.ListLiveGradesFromGroupGrades(ctx, a.ID)
			if err != nil {
				return GradeApplyPeerOut{}, err
			}
			held := make([]uuid.UUID, len(rows))
			for i, r := range rows {
				held[i] = r.ID
			}
			if _, err := ec.Q.LockGradesInCourse(ctx, dbq.LockGradesInCourseParams{Ids: held, CourseID: in.CourseID}); err != nil {
				return GradeApplyPeerOut{}, err
			}
			plan, err := peerPlan(ctx, ec.Q, a, *f)
			if err != nil {
				return GradeApplyPeerOut{}, err
			}
			for _, w := range plan {
				if !slices.Contains(held, w.g.ID) {
					return GradeApplyPeerOut{}, apperr.Conflicts("a grade of this assignment was written just now; call again")
				}
			}
			if ec.Approved && !in.samePlan(*f, plan) {
				return GradeApplyPeerOut{}, errPeerGradesChanged()
			}
			// Every member whose grade it writes, as they are now.
			if reason, err := authz.CheckScope(ctx, ec.Q, ec.Member, authz.Target{StudentMemberIDs: planStudents(plan),
				AssignmentIDs: []uuid.UUID{a.ID}}); err != nil {
				return GradeApplyPeerOut{}, err
			} else if reason != authz.ReasonNone {
				return GradeApplyPeerOut{}, apperr.Forbid("a member whose grade it writes is outside your scope").With("reason", string(reason))
			}
			out := GradeApplyPeerOut{Written: []PeerGradeWritten{}}
			changed := map[uuid.UUID][]uuid.UUID{}
			for _, w := range plan {
				written, err := writePeerGrade(ctx, ec, in.CourseID, a.ID, w)
				if err != nil {
					return GradeApplyPeerOut{}, err
				}
				out.Written = append(out.Written, written)
				if written.Posted {
					changed[w.g.StudentMemberID] = []uuid.UUID{a.ID}
				}
			}
			out.Snapshots, err = snapshot(ctx, ec, in.CourseID, changed, gradecalc.Policy{})
			return out, err
		},
	})
}

// writePeerGrade writes w's grade again with its new adjustment: a draft as
// a draft, a posted grade posted, the old superseded and its feedback files
// moved to the new.
func writePeerGrade(ctx context.Context, ec *tool.ExecCtx, courseID, assignment uuid.UUID, w peerWrite) (PeerGradeWritten, error) {
	g := w.g
	id := ids.New()
	// The old row first, as a regrade does: the deferred key lets it name a
	// row that is not there yet.
	if n, err := ec.Q.SupersedeGrade(ctx, dbq.SupersedeGradeParams{ID: g.ID, NewID: &id}); err != nil {
		return PeerGradeWritten{}, err
	} else if n == 0 {
		return PeerGradeWritten{}, apperr.Conflicts("grade %s was replaced by someone else just now; call again", g.ID)
	}
	groupGrade := g.GroupGradeID
	row := dbq.InsertGradeParams{ID: id, StudentMemberID: g.StudentMemberID, SubmissionID: g.SubmissionID, Origin: "entered",
		Score: w.score, Feedback: g.Feedback, Breakdown: g.Breakdown, RubricVersionID: g.RubricVersionID,
		GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID, CreatedAt: ec.Now, GroupGradeID: &groupGrade}
	typ := events.GradeCreated
	if g.PostedAt != nil {
		row.PostedAt, row.PostedByMemberID, typ = &ec.Now, &ec.Member.ID, events.GradeRegraded
	}
	w.adj.columns(&row)
	if err := ec.Q.InsertGrade(ctx, row); err != nil {
		return PeerGradeWritten{}, err
	}
	if err := ec.Q.MoveFeedbackFiles(ctx, dbq.MoveFeedbackFilesParams{OldGradeID: &g.ID, NewGradeID: &id}); err != nil {
		return PeerGradeWritten{}, err
	}
	student := g.StudentMemberID
	ec.Emit(events.Event{Type: typ, CourseID: &courseID, SubjectType: "grade", SubjectID: &id, StudentMemberID: &student,
		AssignmentID: &assignment, Payload: map[string]any{"replaces": g.ID, "group_grade_id": groupGrade, "peer": true}})
	return PeerGradeWritten{StudentMemberID: student, GradeID: id, Replaces: g.ID, Before: g.Score, Score: w.score,
		Adjustment: w.adj.view(), Posted: g.PostedAt != nil}, nil
}
