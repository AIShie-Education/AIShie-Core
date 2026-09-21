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

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/gradecalc"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
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

// loadScores collects one student's grades that count: live, posted, entered.
func loadScores(ctx context.Context, q dbq.Querier, courseID, student uuid.UUID) (gradecalc.Scores, error) {
	s := gradecalc.Scores{Assignment: map[uuid.UUID]decimal.Decimal{}, Component: map[uuid.UUID]decimal.Decimal{}}
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
	root, _, err := loadTree(ctx, ec.Q, courseID)
	if err != nil {
		return 0, err
	}
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
		scores, err := loadScores(ctx, ec.Q, courseID, student)
		if err != nil {
			return 0, err
		}
		// Final is final. Once a student's totals have been written with
		// ungraded work counted as zero, a later post or regrade beneath
		// them — made without saying so again — must not quietly turn them
		// back into a grade so far. The policy travels with the snapshot.
		studentPolicy := policy
		if !studentPolicy.UngradedAsZero {
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
				r := results[componentID]
				if r.Fraction == nil {
					continue
				}
				wrote, err := writeSnapshot(ctx, ec, courseID, student, componentID, r, studentPolicy)
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

func writeSnapshot(ctx context.Context, ec *tool.ExecCtx, courseID, student, componentID uuid.UUID, r gradecalc.Result, policy gradecalc.Policy) (bool, error) {
	score := gradecalc.Percent(*r.Fraction)
	newID := ids.New()

	live, err := ec.Q.GetLiveComputedGrade(ctx, dbq.GetLiveComputedGradeParams{ComponentID: &componentID, StudentMemberID: student})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return false, err
	case live.Score.Equal(score) && storedPolicy(live.Breakdown) == policy:
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
	}
	working, err := json.Marshal(snapshotWorking{Result: r, UngradedAsZero: policy.UngradedAsZero})
	if err != nil {
		return false, err
	}
	if err := ec.Q.InsertGrade(ctx, dbq.InsertGradeParams{
		ID: newID, StudentMemberID: student, ComponentID: &componentID,
		Origin: "computed", Score: score, Breakdown: working,
		GraderMemberID: ec.Member.ID, CreatedByActionID: ec.ActionID,
		PostedAt: &ec.Now, PostedByMemberID: &ec.Member.ID, CreatedAt: ec.Now,
	}); err != nil {
		return false, err
	}
	ec.Emit(events.Event{
		Type: events.GradeTotalUpdated, CourseID: &courseID,
		SubjectType: "grade", SubjectID: &newID, StudentMemberID: &student,
		Payload: map[string]any{"component_id": componentID, "complete": r.Complete},
	})
	return true, nil
}
