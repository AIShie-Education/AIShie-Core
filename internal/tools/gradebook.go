package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/gradecalc"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// loadTree builds the course's component tree in the shape gradecalc takes.
func loadTree(ctx context.Context, q dbq.Querier, courseID uuid.UUID) (*gradecalc.Component, map[uuid.UUID]string, error) {
	rows, err := q.ListComponents(ctx, courseID)
	if err != nil {
		return nil, nil, err
	}
	nodes := make(map[uuid.UUID]*gradecalc.Component, len(rows))
	names := make(map[uuid.UUID]string, len(rows))
	for _, r := range rows {
		c := &gradecalc.Component{ID: r.ID, Weight: r.Weight, DropLowest: int(r.DropLowest)}
		if r.PointsPossible.Valid {
			pts := r.PointsPossible.Decimal
			c.PointsPossible = &pts
		}
		nodes[r.ID], names[r.ID] = c, r.Name
	}
	var root *gradecalc.Component
	for _, r := range rows { // rows are in sort order, so children are too
		switch {
		case r.ParentID == nil:
			root = nodes[r.ID]
		case nodes[*r.ParentID] != nil:
			nodes[*r.ParentID].Children = append(nodes[*r.ParentID].Children, nodes[r.ID])
		}
	}
	if root == nil {
		return nil, nil, fmt.Errorf("course %s has no root grade component", courseID)
	}
	assignments, err := q.ListGradedAssignments(ctx, courseID)
	if err != nil {
		return nil, nil, err
	}
	for _, a := range assignments {
		if c := nodes[*a.ComponentID]; c != nil {
			c.Assignments = append(c.Assignments, gradecalc.Assignment{ID: a.ID, PointsPossible: a.PointsPossible})
		}
	}
	return root, names, nil
}

// loadScores collects one student's grades that count: live, posted, entered;
// and the overrides a person put in place of their totals, which count above
// them.
func loadScores(ctx context.Context, q dbq.Querier, courseID, student uuid.UUID) (gradecalc.Scores, error) {
	s := gradecalc.Scores{Assignment: map[uuid.UUID]decimal.Decimal{}, Component: map[uuid.UUID]decimal.Decimal{},
		Override: map[uuid.UUID]decimal.Decimal{}}
	overrides, err := q.ListLiveTotalOverrides(ctx, student)
	if err != nil {
		return s, err
	}
	for _, r := range overrides {
		s.Override[*r.ComponentID] = r.OverrideScore.Decimal.Div(decimal.NewFromInt(100))
	}
	as, err := q.ListLiveAssignmentScores(ctx, dbq.ListLiveAssignmentScoresParams{CourseID: courseID, StudentMemberID: student})
	if err != nil {
		return s, err
	}
	for _, r := range as {
		s.Assignment[r.AssignmentID] = r.Score
	}
	cs, err := q.ListLiveComponentScores(ctx, student)
	if err != nil {
		return s, err
	}
	for _, r := range cs {
		s.Component[*r.ComponentID] = r.Score
	}
	return s, nil
}

// snapshot writes down what each affected student's rolled-up components come
// to, now that grades beneath them have been posted or changed.
//
// Rollups are computed on read; a snapshot is stored only here, at posting.
// The number a student was shown must not drift when a lower grade changes
// later, so changing it is a regrade of the snapshot, with history: the old
// row is superseded, never updated. changed maps each student to the
// assignments and components whose grades moved; only their ancestors are
// looked at.
func snapshot(ctx context.Context, ec *tool.ExecCtx, courseID uuid.UUID, changed map[uuid.UUID][]uuid.UUID, policy gradecalc.Policy) (int, error) {
	return snapshotUnder(ctx, ec, courseID, changed, policy, false)
}

// snapshotUnder is snapshot, with reset saying that policy is to be taken as
// it is: not made final by what a student's totals were written under
// before. Only undoing ungraded-as-zero says so.
func snapshotUnder(ctx context.Context, ec *tool.ExecCtx, courseID uuid.UUID, changed map[uuid.UUID][]uuid.UUID, policy gradecalc.Policy, reset bool) (int, error) {
	// In a fixed order, so that the events come out the same way every time.
	students := make([]uuid.UUID, 0, len(changed))
	for s := range changed {
		students = append(students, s)
	}
	sort.Slice(students, func(i, j int) bool { return students[i].String() < students[j].String() })

	written := 0
	for _, student := range students {
		items := changed[student]
		// One writer of a student's totals at a time: two posts touching the
		// same student would otherwise each read the other's not-yet-written
		// snapshot as absent and collide on the one live total.
		if err := ec.Q.LockStudentTotals(ctx, dbq.LockStudentTotalsParams{CourseID: courseID, StudentMemberID: student}); err != nil {
			return 0, err
		}
		// The scheme is read here, under the lock, as the scores are. A post
		// works through its students one at a time: by the time it reaches
		// this one, a weight may have changed and a regrade have written
		// their totals under it, and a scheme read before the lock would
		// write them over under the old one. The tree lock is not taken:
		// grade.submit on a component holds it while it waits for drafts a
		// post has locked by now.
		root, _, err := loadTree(ctx, ec.Q, courseID)
		if err != nil {
			return 0, err
		}
		scores, err := loadScores(ctx, ec.Q, courseID, student)
		if err != nil {
			return 0, err
		}
		// Final is final until it is undone. Once a student's totals have
		// been written with ungraded work counted as zero, a later post or
		// regrade beneath them — made without saying so again — must not
		// quietly turn them back into a grade so far. The policy travels
		// with the snapshot. Only undoing it, on purpose, does (reset).
		studentPolicy := policy
		if !studentPolicy.UngradedAsZero && !reset {
			live, err := ec.Q.GetLiveComputedGrade(ctx, dbq.GetLiveComputedGradeParams{ComponentID: &root.ID, StudentMemberID: student})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return 0, err
			}
			if err == nil && storedPolicy(live.Breakdown).UngradedAsZero {
				studentPolicy.UngradedAsZero = true
			}
		}
		results := gradecalc.Compute(root, scores, studentPolicy)

		seen := map[uuid.UUID]bool{}
		for _, item := range items {
			for _, componentID := range gradecalc.Ancestors(root, item) {
				if seen[componentID] {
					continue
				}
				seen[componentID] = true
				wrote, err := writeSnapshot(ctx, ec, courseID, student, componentID, results[componentID], studentPolicy)
				if err != nil {
					return 0, err
				}
				if wrote {
					written++
				}
			}
		}
	}
	return written, nil
}

// snapshotWorking is what a computed grade's breakdown holds: the working,
// and the policy it was worked out under.
type snapshotWorking struct {
	gradecalc.Result
	UngradedAsZero bool `json:"ungraded_as_zero,omitempty"`
}

func storedPolicy(breakdown []byte) gradecalc.Policy {
	var w snapshotWorking
	_ = json.Unmarshal(breakdown, &w)
	return gradecalc.Policy{UngradedAsZero: w.UngradedAsZero}
}

// sameWorking reports whether a stored snapshot's working is what r, worked
// out under policy, would store. A snapshot shows its working and whether it
// is complete, not only its number: a post that fills a gap without moving
// the number, or comes after a weight has changed, still writes a new one.
func sameWorking(breakdown []byte, r gradecalc.Result, policy gradecalc.Policy) bool {
	var stored snapshotWorking
	if err := json.Unmarshal(breakdown, &stored); err != nil {
		return false
	}
	return gradecalc.Policy{UngradedAsZero: stored.UngradedAsZero} == policy && stored.Same(r)
}

// writeSnapshot writes a component's total down for a student when it shows
// something other than the live one, superseding that: a new number, a gap
// filled, a line of the working changed. A total with nothing beneath it to
// go on any more — work moved away from under it, ungraded work no longer
// counted as zero — is written down as having none (voidTotal), score 0, so
// that what the student was last shown does not stand for it; one that never
// had a total is left without one. What a person gave the live total besides
// its number — an override, a comment, feedback files — is carried on to the
// new row: working the number out again is not grading it again.
func writeSnapshot(ctx context.Context, ec *tool.ExecCtx, courseID, student, componentID uuid.UUID, r gradecalc.Result, policy gradecalc.Policy) (bool, error) {
	score := decimal.Zero
	if r.Fraction != nil {
		score = gradecalc.Percent(*r.Fraction)
	}
	newID := ids.New()
	row := dbq.InsertGradeParams{
		ID: newID, StudentMemberID: student, ComponentID: &componentID,
		Origin: "computed", Score: score,
		GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID,
		PostedAt: &ec.Now, PostedByMemberID: &ec.Member.ID, CreatedAt: ec.Now,
	}

	live, err := ec.Q.GetLiveComputedGrade(ctx, dbq.GetLiveComputedGradeParams{ComponentID: &componentID, StudentMemberID: student})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if r.Fraction == nil {
			return false, nil
		}
	case err != nil:
		return false, err
	case live.Score.Equal(score) && sameWorking(live.Breakdown, r, policy):
		return false, nil
	default:
		// Old row first, then the new one: the other order would briefly be
		// two live grades for one target, which the unique index refuses.
		n, err := ec.Q.SupersedeGrade(ctx, dbq.SupersedeGradeParams{ID: live.ID, NewID: &newID})
		if err != nil {
			return false, err
		}
		if n == 0 {
			return false, apperr.Conflicts("the student's totals were written by someone else just now; try again")
		}
		row.Feedback = live.Feedback
		row.OverrideScore, row.OverrideReason, row.OverrideByMemberID, row.OverriddenAt =
			live.OverrideScore, live.OverrideReason, live.OverrideByMemberID, live.OverriddenAt
	}
	working, err := json.Marshal(snapshotWorking{Result: r, UngradedAsZero: policy.UngradedAsZero})
	if err != nil {
		return false, err
	}
	row.Breakdown = working
	if err := ec.Q.InsertGrade(ctx, row); err != nil {
		return false, err
	}
	if live.ID != uuid.Nil {
		if err := ec.Q.MoveFeedbackFiles(ctx, dbq.MoveFeedbackFilesParams{OldGradeID: &live.ID, NewGradeID: &newID}); err != nil {
			return false, err
		}
	}
	payload := map[string]any{"component_id": componentID, "complete": r.Complete}
	if r.Fraction == nil {
		payload["no_total"] = true
	}
	if row.OverrideScore.Valid {
		payload["overridden"] = true
	}
	ec.Emit(events.Event{
		Type: events.GradeTotalUpdated, CourseID: &courseID,
		SubjectType: "grade", SubjectID: &newID, StudentMemberID: &student,
		Payload: payload,
	})
	return true, nil
}

// voidTotal reports whether a computed grade's working says it has nothing
// to go on: written down when a total that had one lost everything beneath it.
func voidTotal(breakdown []byte) bool {
	var w snapshotWorking
	return json.Unmarshal(breakdown, &w) == nil && w.Fraction == nil
}

// ---------------------------------------------------------------------------
// When what work is worth, or where it counts, changes under its grades
// ---------------------------------------------------------------------------

// How the grades already entered on work are carried across a change of the
// points it is worth.
const (
	// Each score is converted to the new points, in proportion: 45 of 50 is
	// 90 of 100. A new grade row replaces each, the old kept as superseded,
	// as a regrade does.
	existingRescale = "rescale"
	// Each score stays as it was entered, and is now out of the new points.
	existingKeepScores = "keep_scores"
)

func checkExistingGrades(how *string) error {
	if how != nil && *how != existingRescale && *how != existingKeepScores {
		return apperr.Invalid("existing_grades is rescale or keep_scores")
	}
	return nil
}

// gradeScore is a grade entered on a piece of work, by its score.
type gradeScore struct {
	id    uuid.UUID
	score decimal.Decimal
}

// rebaseRefusal is what rebase refuses of grades entered on a piece of work,
// by their scores, carried from points from to points to as how says,
// before it writes anything. A change of what the work is worth asks it of
// the grades as they stand before it is carried out, proposed or approved
// (Validate: assignment.update, component.update), and rebase again of the
// grades it holds.
func rebaseRefusal(grades []gradeScore, from, to decimal.Decimal, how string) error {
	if len(grades) == 0 {
		return nil
	}
	if how == existingKeepScores {
		for _, g := range grades {
			if g.score.GreaterThan(to) && !g.score.GreaterThan(from) {
				return apperr.Precondition("grade %s is %s, which was within the %s points it was given out of and is above the %s it would be out of; rescale, or regrade it first",
					g.id, g.score, from, to).With("reason", "score_above_points").With("grade_id", g.id)
			}
		}
		return nil
	}
	if from.IsZero() {
		return apperr.Precondition("the work was worth nothing, so there is nothing to rescale its grades from; keep their scores instead").
			With("reason", "nothing_to_rescale")
	}
	return nil
}

// rescaledScore is a score out of from, as a score out of to, to four
// decimal places.
func rescaledScore(score, from, to decimal.Decimal) decimal.Decimal {
	return score.Mul(to).DivRound(from, 4)
}

// rebase carries the grades entered on a piece of work — every live one,
// drafts as much as posted grades, held by the caller in id order — across a
// change of what the work is worth, from points to points, as how says. It
// returns how many grades it wrote again.
//
// keep_scores writes nothing: the scores stand, out of the new points. It is
// refused if a score within what the work was worth would be above what it is
// worth now, as a grade entered above the points possible is refused without
// allow_extra; a score that was extra credit already stays so. rescale writes
// each score again in proportion, in a new row that supersedes the old — a
// posted grade posted, a draft a draft — carrying its feedback, breakdown,
// rubric and feedback files on: the judgement is the same, told in other
// units. The breakdown is the grader's working against the rubric, and is
// kept as it was. A score the conversion leaves as it was (a 0) is left
// alone. Work that was worth nothing has nothing to rescale from.
func rebase(ctx context.Context, ec *tool.ExecCtx, courseID uuid.UUID, assignmentID *uuid.UUID,
	grades []dbq.LockLiveEnteredGradesOfAssignmentRow, from, to decimal.Decimal, how string) (int, error) {
	scores := make([]gradeScore, len(grades))
	for i, g := range grades {
		scores[i] = gradeScore{id: g.ID, score: g.Score}
	}
	if err := rebaseRefusal(scores, from, to, how); err != nil || how == existingKeepScores {
		return 0, err
	}
	written := 0
	for _, g := range grades {
		score := rescaledScore(g.Score, from, to)
		if score.Equal(g.Score) {
			continue
		}
		id := ids.New()
		// The old row first, as a regrade does: the deferred key lets it
		// name a row that is not there yet, and the other order would be
		// two live posted grades for one target.
		n, err := ec.Q.SupersedeGrade(ctx, dbq.SupersedeGradeParams{ID: g.ID, NewID: &id})
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, apperr.Conflicts("grade %s was replaced by someone else just now; try again", g.ID)
		}
		row := dbq.InsertGradeParams{
			ID: id, StudentMemberID: g.StudentMemberID, SubmissionID: g.SubmissionID, ComponentID: g.ComponentID,
			Origin: "entered", Score: score, Feedback: g.Feedback, Breakdown: g.Breakdown, RubricVersionID: g.RubricVersionID,
			GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID, CreatedAt: ec.Now,
		}
		typ := events.GradeCreated
		if g.PostedAt != nil {
			row.PostedAt, row.PostedByMemberID = &ec.Now, &ec.Member.ID
			typ = events.GradeRegraded
		}
		if err := ec.Q.InsertGrade(ctx, row); err != nil {
			return 0, err
		}
		if err := ec.Q.MoveFeedbackFiles(ctx, dbq.MoveFeedbackFilesParams{OldGradeID: &g.ID, NewGradeID: &id}); err != nil {
			return 0, err
		}
		student := g.StudentMemberID
		ec.Emit(events.Event{Type: typ, CourseID: &courseID, SubjectType: "grade", SubjectID: &id,
			StudentMemberID: &student, AssignmentID: assignmentID,
			Payload: map[string]any{"replaces": g.ID, "rescaled": true}})
		written++
	}
	return written, nil
}

// gradedStudents are the students the grades are of.
func gradedStudents(grades []dbq.LockLiveEnteredGradesOfAssignmentRow) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(grades))
	for _, g := range grades {
		out = append(out, g.StudentMemberID)
	}
	return dedupe(out)
}

// schemeScope is what a change to graded work's points or place reaches: it
// rewrites the posted totals of every student who has one, and the grades of
// every student graded there. Like posting final grades, it decides course
// totals, so it spans assignments; and it must reach every one of those
// students.
func schemeScope(ctx context.Context, q dbq.Querier, courseID uuid.UUID, graded []uuid.UUID) (authz.Target, error) {
	withTotals, err := q.ListStudentsWithLiveTotals(ctx, courseID)
	if err != nil {
		return authz.Target{}, err
	}
	return authz.Target{StudentMemberIDs: dedupe(append(graded, withTotals...)), SpansAssignments: true}, nil
}

// checkSchemeScope holds a change, as it is carried out, to the reach
// schemeScope says it needs, over the grades as they are now: some may have
// been entered since the call was authorized.
func checkSchemeScope(ctx context.Context, ec *tool.ExecCtx, courseID uuid.UUID, graded []uuid.UUID) error {
	t, err := schemeScope(ctx, ec.Q, courseID, graded)
	if err != nil {
		return err
	}
	if reason, err := authz.CheckScope(ctx, ec.Q, ec.Member, t); err != nil {
		return err
	} else if reason != authz.ReasonNone {
		return apperr.Forbid("grades have been entered here, and the change rewrites the totals of students outside your scope").
			With("reason", string(reason))
	}
	return nil
}

// rewriteTotals writes down again the totals of every student who has one,
// and of those given, wherever the items changed: the scheme has moved under
// them. Only what now shows something different is written (snapshot).
func rewriteTotals(ctx context.Context, ec *tool.ExecCtx, courseID uuid.UUID, also []uuid.UUID, items ...uuid.UUID) (int, error) {
	students, err := ec.Q.ListStudentsWithLiveTotals(ctx, courseID)
	if err != nil {
		return 0, err
	}
	changed := map[uuid.UUID][]uuid.UUID{}
	for _, s := range dedupe(append(students, also...)) {
		changed[s] = items
	}
	return snapshot(ctx, ec, courseID, changed, gradecalc.Policy{})
}
