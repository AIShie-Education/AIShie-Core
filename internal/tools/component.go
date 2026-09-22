package tools

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

// The grading scheme is a tree per course. There is no permission column for
// "edit the grading scheme"; it is gated by perm_assignment_write, since the
// scheme is where assignments hang and is set up by whoever sets them up.

func componentTools() []tool.Tool {
	return []tool.Tool{componentTree(), componentCreate(), componentUpdate(), componentMove()}
}

var writeScheme = tool.Gate{Perms: []domain.Perm{domain.PermAssignmentWrite}}

const (
	EventComponentCreated = "component.created"
	EventComponentUpdated = "component.updated"
	EventComponentMoved   = "component.moved"
)

type ComponentView struct {
	ID             uuid.UUID        `json:"id"`
	ParentID       *uuid.UUID       `json:"parent_id,omitempty" jsonschema:"absent for the root, which is the course total"`
	Name           string           `json:"name"`
	Weight         decimal.Decimal  `json:"weight" jsonschema:"relative to its siblings"`
	DropLowest     int32            `json:"drop_lowest"`
	PointsPossible *decimal.Decimal `json:"points_possible,omitempty" jsonschema:"set on a component graded directly, such as an exam"`
	SortOrder      int32            `json:"sort_order"`
}

type ComponentTreeOut struct {
	Components []ComponentView `json:"components" jsonschema:"every component of the course, parents before children"`
}

func componentTree() tool.Tool {
	return tool.Define(tool.Spec[tool.InCourse, ComponentTreeOut]{
		Name:        "component.tree",
		Description: "The course's grading scheme: the tree of components with their weights, from the course total down.",
		Kind:        tool.Read, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/components"},
		Resolve: func(_ context.Context, _ dbq.Querier, in tool.InCourse) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "grade_component"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in tool.InCourse) (ComponentTreeOut, error) {
			rows, err := rc.Q.ListComponents(ctx, in.CourseID)
			if err != nil {
				return ComponentTreeOut{}, err
			}
			byParent := map[uuid.UUID][]ComponentView{}
			var out ComponentTreeOut
			var root *ComponentView
			for _, r := range rows {
				v := ComponentView{ID: r.ID, ParentID: r.ParentID, Name: r.Name, Weight: r.Weight, DropLowest: r.DropLowest, SortOrder: r.SortOrder}
				if r.PointsPossible.Valid {
					v.PointsPossible = &r.PointsPossible.Decimal
				}
				if r.ParentID == nil {
					root = &v
				} else {
					byParent[*r.ParentID] = append(byParent[*r.ParentID], v)
				}
			}
			var walk func(v ComponentView)
			walk = func(v ComponentView) {
				out.Components = append(out.Components, v)
				for _, ch := range byParent[v.ID] {
					walk(ch)
				}
			}
			if root != nil {
				walk(*root)
			}
			return out, nil
		},
	})
}

func componentTarget(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID) (tool.Target, error) {
	if _, err := q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: id, CourseID: courseID}); errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such grade component in this course")
	} else if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: courseID, Type: "grade_component", ID: &id}, nil
}

// canHaveChildren: a parent is rolled up from its children, so it holds
// neither assignments nor points of its own.
func canHaveChildren(ctx context.Context, q *dbq.Queries, parent dbq.GetComponentInCourseRow) error {
	if parent.PointsPossible.Valid {
		return apperr.Precondition("%q is graded directly and cannot have sub-components", parent.Name)
	}
	if n, err := q.CountComponentAssignments(ctx, &parent.ID); err != nil {
		return err
	} else if n > 0 {
		return apperr.Precondition("%q holds assignments and cannot also have sub-components", parent.Name)
	}
	return nil
}

type ComponentCreateIn struct {
	tool.InCourse
	ParentID       uuid.UUID        `json:"parent_id" jsonschema:"the component this one sits under; the root for a top-level bucket"`
	Name           string           `json:"name"`
	Weight         *decimal.Decimal `json:"weight,omitempty" jsonschema:"relative to its siblings; default 1"`
	DropLowest     int32            `json:"drop_lowest,omitempty"`
	PointsPossible *decimal.Decimal `json:"points_possible,omitempty" jsonschema:"set for a component graded directly, such as an exam"`
	SortOrder      int32            `json:"sort_order,omitempty"`
}

func componentCreate() tool.Tool {
	return tool.Define(tool.Spec[ComponentCreateIn, IDOut]{
		Name: "component.create",
		Description: "Add a component to the grading scheme under a parent: a bucket that assignments will hang from, " +
			"or, with points_possible, something graded directly such as an exam.",
		Kind: tool.Write, Gate: writeScheme,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/components"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ComponentCreateIn) (tool.Target, error) {
			t, err := componentTarget(ctx, q, in.CourseID, in.ParentID)
			t.ID = nil
			return t, err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ComponentCreateIn) (IDOut, error) {
			if err := ec.Q.LockCourseComponents(ctx, in.CourseID); err != nil {
				return IDOut{}, err
			}
			parent, err := ec.Q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: in.ParentID, CourseID: in.CourseID})
			if err != nil {
				return IDOut{}, err
			}
			if err := canHaveChildren(ctx, ec.Q, parent); err != nil {
				return IDOut{}, err
			}
			weight := one
			if in.Weight != nil {
				weight = *in.Weight
			}
			if err := checkComponent(in.Name, weight, in.DropLowest, in.PointsPossible); err != nil {
				return IDOut{}, err
			}
			id := ids.New()
			if err := ec.Q.InsertComponent(ctx, dbq.InsertComponentParams{
				ID: id, CourseID: in.CourseID, ParentID: &in.ParentID, Name: in.Name, Weight: weight,
				DropLowest: in.DropLowest, PointsPossible: nullDecimal(in.PointsPossible), SortOrder: in.SortOrder, CreatedAt: ec.Now,
			}); err != nil {
				return IDOut{}, err
			}
			ec.Emit(events.Event{Type: EventComponentCreated, CourseID: &in.CourseID, SubjectType: "grade_component", SubjectID: &id})
			return IDOut{ID: id}, nil
		},
	})
}

func checkComponent(name string, weight decimal.Decimal, dropLowest int32, points *decimal.Decimal) error {
	switch {
	case strings.TrimSpace(name) == "":
		return apperr.Invalid("name is required")
	case weight.IsNegative():
		return apperr.Invalid("weight cannot be negative")
	case dropLowest < 0:
		return apperr.Invalid("drop_lowest cannot be negative")
	case points != nil && points.IsNegative():
		return apperr.Invalid("points_possible cannot be negative")
	}
	return nil
}

func nullDecimal(d *decimal.Decimal) decimal.NullDecimal {
	if d == nil {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{Decimal: *d, Valid: true}
}

type ComponentUpdateIn struct {
	tool.InCourse
	ComponentID         uuid.UUID        `json:"component_id"`
	Name                *string          `json:"name,omitempty"`
	Weight              *decimal.Decimal `json:"weight,omitempty"`
	DropLowest          *int32           `json:"drop_lowest,omitempty"`
	PointsPossible      *decimal.Decimal `json:"points_possible,omitempty"`
	ClearPointsPossible bool             `json:"clear_points_possible,omitempty" jsonschema:"turn a directly graded component back into a bucket"`
	SortOrder           *int32           `json:"sort_order,omitempty"`
}

func componentUpdate() tool.Tool {
	return tool.Define(tool.Spec[ComponentUpdateIn, OK]{
		Name: "component.update",
		Description: "Change a component's name, weight, drop_lowest, points or order. Posted totals are not rewritten: " +
			"what a student was shown stays as it was until grades beneath it are next posted or regraded.",
		Kind: tool.Write, Gate: writeScheme,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/components/{component_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ComponentUpdateIn) (tool.Target, error) {
			return componentTarget(ctx, q, in.CourseID, in.ComponentID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ComponentUpdateIn) (OK, error) {
			if err := ec.Q.LockCourseComponents(ctx, in.CourseID); err != nil {
				return OK{}, err
			}
			c, err := ec.Q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: in.ComponentID, CourseID: in.CourseID})
			if err != nil {
				return OK{}, err
			}
			rows, err := ec.Q.ListComponents(ctx, in.CourseID)
			if err != nil {
				return OK{}, err
			}
			var sortOrder int32
			for _, r := range rows {
				if r.ID == c.ID {
					sortOrder = r.SortOrder
				}
			}
			if in.Name != nil {
				c.Name = *in.Name
			}
			if in.Weight != nil {
				c.Weight = *in.Weight
			}
			if in.DropLowest != nil {
				c.DropLowest = *in.DropLowest
			}
			if in.SortOrder != nil {
				sortOrder = *in.SortOrder
			}
			switch {
			case in.ClearPointsPossible && in.PointsPossible != nil:
				return OK{}, apperr.Invalid("give points_possible or clear_points_possible, not both")
			case in.ClearPointsPossible:
				if has, err := ec.Q.ComponentHasGrades(ctx, &c.ID); err != nil {
					return OK{}, err
				} else if has {
					return OK{}, apperr.Precondition("%q has grades entered on it and cannot stop being graded directly", c.Name)
				}
				c.PointsPossible = decimal.NullDecimal{}
			case in.PointsPossible != nil && c.PointsPossible.Valid && !c.PointsPossible.Decimal.Equal(*in.PointsPossible):
				// Already graded directly, and worth something else now. As
				// for an assignment: once a grade has been entered against
				// the points possible, they no longer change.
				if has, err := ec.Q.ComponentHasLiveGrades(ctx, &c.ID); err != nil {
					return OK{}, err
				} else if has {
					return OK{}, apperr.Precondition("grades have been entered for %q; its points possible no longer change", c.Name)
				}
				c.PointsPossible = nullDecimal(in.PointsPossible)
			case in.PointsPossible != nil:
				// Becoming directly graded: it must be a leaf with nothing
				// hanging from it.
				if n, err := ec.Q.CountComponentChildren(ctx, &c.ID); err != nil {
					return OK{}, err
				} else if n > 0 {
					return OK{}, apperr.Precondition("%q has sub-components and cannot be graded directly", c.Name)
				}
				if n, err := ec.Q.CountComponentAssignments(ctx, &c.ID); err != nil {
					return OK{}, err
				} else if n > 0 {
					return OK{}, apperr.Precondition("%q holds assignments and cannot be graded directly", c.Name)
				}
				if c.ParentID == nil {
					return OK{}, apperr.Precondition("the course total is rolled up, never graded directly")
				}
				// A former parent may still carry the totals that were written
				// down for it when grades beneath it were posted. Those are live
				// posted grades on this component: an entered grade could then
				// never be posted over them, nor regraded, and there would be no
				// way back through the tools.
				if !c.PointsPossible.Valid {
					if has, err := ec.Q.ComponentHasLivePostedGrades(ctx, &c.ID); err != nil {
						return OK{}, err
					} else if has {
						return OK{}, apperr.Precondition("%q has posted totals from when it was rolled up; make a new component for what is graded directly", c.Name)
					}
				}
				c.PointsPossible = nullDecimal(in.PointsPossible)
			}
			if err := checkComponent(c.Name, c.Weight, c.DropLowest, in.PointsPossible); err != nil {
				return OK{}, err
			}
			if err := ec.Q.UpdateComponent(ctx, dbq.UpdateComponentParams{ID: c.ID, Name: c.Name, Weight: c.Weight,
				DropLowest: c.DropLowest, PointsPossible: c.PointsPossible, SortOrder: sortOrder}); err != nil {
				return OK{}, err
			}
			ec.Emit(events.Event{Type: EventComponentUpdated, CourseID: &in.CourseID, SubjectType: "grade_component", SubjectID: &c.ID})
			return OK{OK: true}, nil
		},
	})
}

type ComponentMoveIn struct {
	tool.InCourse
	ComponentID uuid.UUID `json:"component_id"`
	NewParentID uuid.UUID `json:"new_parent_id"`
}

func componentMove() tool.Tool {
	return tool.Define(tool.Spec[ComponentMoveIn, OK]{
		Name: "component.move",
		Description: "Move a component, with everything beneath it, under a different parent in the same course. " +
			"Once any grade has been entered beneath it, its place in the scheme is fixed.",
		Kind: tool.Write, Gate: writeScheme,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/components/{component_id}/move"},
		Resolve: func(ctx context.Context, q dbq.Querier, in ComponentMoveIn) (tool.Target, error) {
			if _, err := componentTarget(ctx, q, in.CourseID, in.NewParentID); err != nil {
				return tool.Target{}, err
			}
			return componentTarget(ctx, q, in.CourseID, in.ComponentID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ComponentMoveIn) (OK, error) {
			// Two moves could each pass the cycle check and between them
			// make a cycle; the lock makes the check and the move one step.
			if err := ec.Q.LockCourseComponents(ctx, in.CourseID); err != nil {
				return OK{}, err
			}
			c, err := ec.Q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: in.ComponentID, CourseID: in.CourseID})
			if err != nil {
				return OK{}, err
			}
			if c.ParentID == nil {
				return OK{}, apperr.Precondition("the course total is the root and stays there")
			}
			// A score is a score in the scheme it was given under. Once a
			// grade has been entered anywhere beneath a component, moving it
			// would change what every one of those grades counts toward.
			if graded, err := ec.Q.ComponentSubtreeHasLiveGrades(ctx, c.ID); err != nil {
				return OK{}, err
			} else if graded {
				return OK{}, apperr.Precondition("grades have been entered beneath %q; its place in the scheme no longer changes", c.Name)
			}
			parent, err := ec.Q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: in.NewParentID, CourseID: in.CourseID})
			if err != nil {
				return OK{}, err
			}
			if err := canHaveChildren(ctx, ec.Q, parent); err != nil {
				return OK{}, err
			}
			// The database blocks only a component being its own parent.
			// Anything longer — A under B under A — is caught here: walk up
			// from the new parent, and if the walk meets the component, the
			// move would put it beneath itself.
			for at, steps := &in.NewParentID, 0; at != nil; steps++ {
				if *at == c.ID {
					return OK{}, apperr.Precondition("that would put %q beneath itself", c.Name)
				}
				if steps > 1000 {
					return OK{}, errors.New("component tree is deeper than 1000 levels or already cyclic")
				}
				if at, err = ec.Q.GetComponentParent(ctx, *at); err != nil {
					return OK{}, err
				}
			}
			if err := ec.Q.SetComponentParent(ctx, dbq.SetComponentParentParams{ID: c.ID, ParentID: &in.NewParentID}); err != nil {
				return OK{}, err
			}
			ec.Emit(events.Event{Type: EventComponentMoved, CourseID: &in.CourseID, SubjectType: "grade_component", SubjectID: &c.ID})
			return OK{OK: true}, nil
		},
	})
}
