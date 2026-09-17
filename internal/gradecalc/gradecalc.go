// Package gradecalc rolls grades up a course's component tree. The schema
// stores only parameters (weight, drop_lowest, points_possible); the scheme
// itself is this code, so a new scheme is a code change, not a migration.
//
// It is pure: a tree and one student's scores in, a result per component out.
// Rollups are computed on read and written down only when grades are posted.
//
// The scheme:
//
//   - An item's fraction is score / points_possible.
//   - A bucket (a component with assignments) drops its drop_lowest lowest
//     fractions, never all of them, then is Σscore / Σpoints over what is
//     left: points-weighted, so a 100-point project outweighs a 10-point quiz.
//   - A directly graded component (an exam, with points_possible on the
//     component itself) is its one grade over its points.
//   - Any other component is the weight-averaged fraction of its children,
//     after dropping drop_lowest of them.
//   - Whatever has no grade yet is left out and the rest re-normalised, which
//     gives a "grade so far". Result.Complete says whether anything was left
//     out. Policy.UngradedAsZero counts it as zero instead, for final grades.
package gradecalc

import (
	"sort"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Component is one node of the tree. A node is exactly one of: directly
// graded (PointsPossible set), a bucket (Assignments), or a parent
// (Children). A parent's own Assignments, if any, are ignored; the tools that
// edit the tree do not allow that shape.
type Component struct {
	ID             uuid.UUID
	Weight         decimal.Decimal
	DropLowest     int
	PointsPossible *decimal.Decimal
	Children       []*Component
	Assignments    []Assignment
}

type Assignment struct {
	ID             uuid.UUID
	PointsPossible decimal.Decimal
}

// Scores are one student's grades that count: live, posted, entered.
type Scores struct {
	// Assignment holds, per assignment, the grade on the highest attempt
	// that has one.
	Assignment map[uuid.UUID]decimal.Decimal
	// Component holds grades entered directly on a component.
	Component map[uuid.UUID]decimal.Decimal
}

type Policy struct {
	UngradedAsZero bool
}

// Result is one component's rollup for one student.
type Result struct {
	// Fraction is nil when there is nothing to go on: no grade anywhere
	// beneath the component.
	Fraction *decimal.Decimal `json:"fraction"`
	// Complete is false when something beneath had no grade and was left out.
	Complete bool   `json:"complete"`
	Items    []Item `json:"items"`
}

// Item is one line of the working: an assignment in a bucket, or a child.
type Item struct {
	ID       uuid.UUID        `json:"id"`
	Kind     string           `json:"kind"` // assignment | component
	Fraction *decimal.Decimal `json:"fraction"`
	// Weight is the child's weight, or the assignment's points.
	Weight  decimal.Decimal `json:"weight"`
	Dropped bool            `json:"dropped,omitempty"`
}

const (
	KindAssignment = "assignment"
	KindComponent  = "component"
)

// Percent is a fraction as a score out of 100, to two places: the form a
// computed snapshot is stored in.
func Percent(f decimal.Decimal) decimal.Decimal {
	return f.Mul(decimal.NewFromInt(100)).Round(2)
}

// Compute returns a Result for every component in the tree.
func Compute(root *Component, s Scores, p Policy) map[uuid.UUID]Result {
	out := map[uuid.UUID]Result{}
	compute(root, s, p, out)
	return out
}

func compute(c *Component, s Scores, p Policy, out map[uuid.UUID]Result) Result {
	var r Result
	switch {
	case c.PointsPossible != nil:
		r = direct(c, s, p)
	case len(c.Children) > 0:
		r = parent(c, s, p, out)
	default:
		r = bucket(c, s, p)
	}
	out[c.ID] = r
	return r
}

func direct(c *Component, s Scores, p Policy) Result {
	score, graded := s.Component[c.ID]
	switch {
	case c.PointsPossible.IsZero():
		return Result{Complete: true}
	case graded:
		f := score.Div(*c.PointsPossible)
		return Result{Fraction: &f, Complete: true}
	case p.UngradedAsZero:
		zero := decimal.Zero
		return Result{Fraction: &zero, Complete: true}
	default:
		return Result{Complete: false}
	}
}

func bucket(c *Component, s Scores, p Policy) Result {
	r := Result{Complete: true}
	type line struct {
		idx           int
		score, points decimal.Decimal
		fraction      decimal.Decimal
	}
	var lines []line
	for _, a := range c.Assignments {
		if a.PointsPossible.IsZero() {
			continue // worth nothing: neither counted nor missed
		}
		score, graded := s.Assignment[a.ID]
		if !graded && p.UngradedAsZero {
			score, graded = decimal.Zero, true
		}
		item := Item{ID: a.ID, Kind: KindAssignment, Weight: a.PointsPossible}
		if graded {
			f := score.Div(a.PointsPossible)
			item.Fraction = &f
			lines = append(lines, line{idx: len(r.Items), score: score, points: a.PointsPossible, fraction: f})
		} else {
			r.Complete = false
		}
		r.Items = append(r.Items, item)
	}
	if len(lines) == 0 {
		return r
	}

	// Lowest fraction first. Between equals, drop the one worth more, then
	// fall back to id so the choice never depends on input order.
	sort.SliceStable(lines, func(i, j int) bool {
		if c := lines[i].fraction.Cmp(lines[j].fraction); c != 0 {
			return c < 0
		}
		if c := lines[i].points.Cmp(lines[j].points); c != 0 {
			return c > 0
		}
		a, b := r.Items[lines[i].idx].ID, r.Items[lines[j].idx].ID
		return a.String() < b.String()
	})
	drop := min(c.DropLowest, len(lines)-1)
	sumScore, sumPoints := decimal.Zero, decimal.Zero
	for i, l := range lines {
		if i < drop {
			r.Items[l.idx].Dropped = true
			continue
		}
		sumScore = sumScore.Add(l.score)
		sumPoints = sumPoints.Add(l.points)
	}
	f := sumScore.Div(sumPoints)
	r.Fraction = &f
	return r
}

func parent(c *Component, s Scores, p Policy, out map[uuid.UUID]Result) Result {
	r := Result{Complete: true}
	type line struct {
		idx      int
		fraction decimal.Decimal
		weight   decimal.Decimal
	}
	var lines []line
	for _, ch := range c.Children {
		cr := compute(ch, s, p, out)
		item := Item{ID: ch.ID, Kind: KindComponent, Weight: ch.Weight, Fraction: cr.Fraction}
		if ch.Weight.IsZero() {
			// Shown in the working, counts for nothing, and its gaps are not
			// this component's gaps.
			r.Items = append(r.Items, item)
			continue
		}
		if !cr.Complete || cr.Fraction == nil {
			r.Complete = false
		}
		if cr.Fraction != nil {
			lines = append(lines, line{idx: len(r.Items), fraction: *cr.Fraction, weight: ch.Weight})
		}
		r.Items = append(r.Items, item)
	}
	if len(lines) == 0 {
		return r
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if c := lines[i].fraction.Cmp(lines[j].fraction); c != 0 {
			return c < 0
		}
		if c := lines[i].weight.Cmp(lines[j].weight); c != 0 {
			return c > 0
		}
		a, b := r.Items[lines[i].idx].ID, r.Items[lines[j].idx].ID
		return a.String() < b.String()
	})
	drop := min(c.DropLowest, len(lines)-1)
	sumWF, sumW := decimal.Zero, decimal.Zero
	for i, l := range lines {
		if i < drop {
			r.Items[l.idx].Dropped = true
			continue
		}
		sumWF = sumWF.Add(l.weight.Mul(l.fraction))
		sumW = sumW.Add(l.weight)
	}
	f := sumWF.Div(sumW)
	r.Fraction = &f
	return r
}

// Ancestors returns the ids from the component holding id up to the root,
// nearest first. id may be an assignment or a component; a component is its
// own first ancestor only if it is rolled up (a directly graded component's
// own grade is entered, not computed). Unknown ids return nil.
func Ancestors(root *Component, id uuid.UUID) []uuid.UUID {
	var path []uuid.UUID
	var walk func(c *Component, trail []uuid.UUID) bool
	walk = func(c *Component, trail []uuid.UUID) bool {
		here := append(trail[:len(trail):len(trail)], c.ID)
		if c.ID == id {
			if c.PointsPossible != nil {
				path = trail
			} else {
				path = here
			}
			return true
		}
		for _, a := range c.Assignments {
			if a.ID == id {
				path = here
				return true
			}
		}
		for _, ch := range c.Children {
			if walk(ch, here) {
				return true
			}
		}
		return false
	}
	if !walk(root, nil) {
		return nil
	}
	// trail is root-first; callers want nearest-first.
	out := make([]uuid.UUID, len(path))
	for i, p := range path {
		out[len(path)-1-i] = p
	}
	return out
}
