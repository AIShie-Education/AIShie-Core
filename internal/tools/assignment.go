package tools

import (
	"context"
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
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

func assignmentTools() []tool.Tool {
	return []tool.Tool{assignmentList(), assignmentGet(), assignmentCreate(), assignmentUpdate(), assignmentPublish(), assignmentUnpublish()}
}

var writeAssignments = tool.Gate{Perms: []domain.Perm{domain.PermAssignmentWrite}}

const (
	EventAssignmentCreated     = "assignment.created"
	EventAssignmentUpdated     = "assignment.updated"
	EventAssignmentPublished   = "assignment.published"
	EventAssignmentUnpublished = "assignment.unpublished"
	EventAssignmentDuePassed   = "assignment.due_passed"
)

type AssignmentView struct {
	ID                     uuid.UUID       `json:"id"`
	ComponentID            *uuid.UUID      `json:"component_id,omitempty" jsonschema:"the bucket it counts toward; absent for practice work"`
	Title                  string          `json:"title"`
	InstructionsDocumentID *uuid.UUID      `json:"instructions_document_id,omitempty"`
	RubricDocumentID       *uuid.UUID      `json:"rubric_document_id,omitempty"`
	PointsPossible         decimal.Decimal `json:"points_possible"`
	DueAt                  *time.Time      `json:"due_at,omitempty"`
	PublishedAt            *time.Time      `json:"published_at,omitempty" jsonschema:"absent while students cannot see it yet"`
}

func viewAssignment(a dbq.GetAssignmentInCourseRow) AssignmentView {
	return AssignmentView{ID: a.ID, ComponentID: a.ComponentID, Title: a.Title, InstructionsDocumentID: a.InstructionsDocumentID,
		RubricDocumentID: a.RubricDocumentID, PointsPossible: a.PointsPossible, DueAt: a.DueAt, PublishedAt: a.PublishedAt}
}

type AssignmentListIn struct {
	tool.InCourse
	Page
}

type AssignmentListOut struct {
	Assignments []AssignmentView `json:"assignments"`
	Next        *uuid.UUID       `json:"next,omitempty"`
}

// canSeeUnpublished: an assignment that is not published yet is visible to
// those who write assignments, and to nobody else.
func canSeeUnpublished(m *domain.Member) bool { return m.Perm(domain.PermAssignmentWrite).Allowed() }

func assignmentList() tool.Tool {
	return tool.Define(tool.Spec[AssignmentListIn, AssignmentListOut]{
		Name: "assignment.list",
		Description: "The course's assignments, within the caller's assignment scope. Assignments that are not published " +
			"yet are shown only to members who can write assignments.",
		Kind: tool.Read, Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/assignments"},
		Resolve: func(_ context.Context, _ dbq.Querier, in AssignmentListIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "assignment"}, nil
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in AssignmentListIn) (AssignmentListOut, error) {
			rows, err := rc.Q.ListAssignments(ctx, dbq.ListAssignmentsParams{
				CourseID: in.CourseID, After: in.after(), MaxRows: in.limit(),
				IncludeUnpublished: canSeeUnpublished(rc.Member),
				AssignmentAll:      rc.Scope.AssignmentAll, MemberID: rc.Scope.MemberID,
				PrincipalID: rc.Scope.PrincipalID, PrincipalAssignmentAll: rc.Scope.PrincipalAssignmentAll,
			})
			out := AssignmentListOut{Assignments: make([]AssignmentView, 0, len(rows))}
			for _, r := range rows {
				out.Assignments = append(out.Assignments, AssignmentView{ID: r.ID, ComponentID: r.ComponentID, Title: r.Title,
					InstructionsDocumentID: r.InstructionsDocumentID, RubricDocumentID: r.RubricDocumentID,
					PointsPossible: r.PointsPossible, DueAt: r.DueAt, PublishedAt: r.PublishedAt})
			}
			if len(rows) > 0 && len(rows) == int(in.limit()) {
				out.Next = &rows[len(rows)-1].ID
			}
			return out, err
		},
	})
}

type AssignmentIDIn struct {
	tool.InCourse
	AssignmentID uuid.UUID `json:"assignment_id"`
}

func assignmentTarget(ctx context.Context, q dbq.Querier, courseID, id uuid.UUID) (tool.Target, error) {
	if _, err := q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: id, CourseID: courseID}); errors.Is(err, pgx.ErrNoRows) {
		return tool.Target{}, apperr.Missing("no such assignment in this course")
	} else if err != nil {
		return tool.Target{}, err
	}
	return tool.Target{CourseID: courseID, Type: "assignment", ID: &id, Scope: authz.Target{AssignmentIDs: []uuid.UUID{id}}}, nil
}

func assignmentGet() tool.Tool {
	return tool.Define(tool.Spec[AssignmentIDIn, AssignmentView]{
		Name:        "assignment.get",
		Description: "One assignment: what it is worth, when it is due, and the documents holding its instructions and rubric.",
		Kind:        tool.Read, Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in AssignmentIDIn) (tool.Target, error) {
			return assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in AssignmentIDIn) (AssignmentView, error) {
			a, err := rc.Q.GetAssignmentInCourse(ctx, dbq.GetAssignmentInCourseParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return AssignmentView{}, err
			}
			if a.PublishedAt == nil && !canSeeUnpublished(rc.Member) {
				// To this caller an unpublished assignment does not exist yet.
				return AssignmentView{}, apperr.Missing("no such assignment in this course")
			}
			return viewAssignment(a), nil
		},
	})
}

// AssignmentBody is what creating and updating share. On update every field
// is optional and what is absent stays as it is.
type AssignmentBody struct {
	Title                  *string          `json:"title,omitempty"`
	PointsPossible         *decimal.Decimal `json:"points_possible,omitempty"`
	ComponentID            *uuid.UUID       `json:"component_id,omitempty" jsonschema:"the bucket this counts toward; omit for practice work that is not part of the grade"`
	InstructionsDocumentID *uuid.UUID       `json:"instructions_document_id,omitempty" jsonschema:"a document of kind instructions, in this course"`
	RubricDocumentID       *uuid.UUID       `json:"rubric_document_id,omitempty" jsonschema:"a document of kind rubric, in this course"`
	DueAt                  *time.Time       `json:"due_at,omitempty"`
}

// checkAssignment holds the same-course rules no foreign key covers: the two
// documents are documents of this course and of the right kind, and the
// component is a bucket of this course.
func checkAssignment(ctx context.Context, q dbq.Querier, courseID uuid.UUID, a dbq.GetAssignmentInCourseRow) error {
	if strings.TrimSpace(a.Title) == "" {
		return apperr.Invalid("title is required")
	}
	if a.PointsPossible.IsNegative() {
		return apperr.Invalid("points_possible cannot be negative")
	}
	for _, d := range []struct {
		id   *uuid.UUID
		kind string
	}{{a.InstructionsDocumentID, "instructions"}, {a.RubricDocumentID, "rubric"}} {
		if d.id == nil {
			continue
		}
		doc, err := q.GetDocumentInCourse(ctx, dbq.GetDocumentInCourseParams{ID: *d.id, CourseID: courseID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Precondition("the %s document is not a document of this course", d.kind)
		}
		if err != nil {
			return err
		}
		if doc.Kind != d.kind {
			return apperr.Precondition("that document is of kind %s, not %s", doc.Kind, d.kind)
		}
	}
	// Published means students can read it. assignment.publish insists the
	// instructions have a published version; pointing an already published
	// assignment at instructions that do not would get to the same place by
	// another door, and every submission from then on would pin nothing.
	if a.PublishedAt != nil && a.InstructionsDocumentID != nil {
		v, err := q.GetDocumentPublishedVersion(ctx, *a.InstructionsDocumentID)
		if err != nil {
			return err
		}
		if v == nil {
			return apperr.Precondition("the assignment is published, and those instructions have no published version for students to read")
		}
	}
	if a.ComponentID != nil {
		// The same lock component.create, update and move take. Without it
		// this check and theirs each see the tree as it was before the other
		// committed, and a component ends up with both assignments and
		// children — whose assignments then count toward nothing.
		if err := q.LockCourseComponents(ctx, courseID); err != nil {
			return err
		}
		c, err := q.GetComponentInCourse(ctx, dbq.GetComponentInCourseParams{ID: *a.ComponentID, CourseID: courseID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Precondition("the component is not part of this course's grading scheme")
		}
		if err != nil {
			return err
		}
		if c.PointsPossible.Valid {
			return apperr.Precondition("%q is graded directly; assignments hang from a bucket", c.Name)
		}
		if n, err := q.CountComponentChildren(ctx, &c.ID); err != nil {
			return err
		} else if n > 0 {
			return apperr.Precondition("%q has sub-components; assignments hang from a bucket at the bottom of the tree", c.Name)
		}
	}
	return nil
}

func (b AssignmentBody) applyTo(a *dbq.GetAssignmentInCourseRow) {
	if b.Title != nil {
		a.Title = *b.Title
	}
	if b.PointsPossible != nil {
		a.PointsPossible = *b.PointsPossible
	}
	if b.ComponentID != nil {
		a.ComponentID = b.ComponentID
	}
	if b.InstructionsDocumentID != nil {
		a.InstructionsDocumentID = b.InstructionsDocumentID
	}
	if b.RubricDocumentID != nil {
		a.RubricDocumentID = b.RubricDocumentID
	}
	if b.DueAt != nil {
		a.DueAt = b.DueAt
	}
}

type AssignmentCreateIn struct {
	tool.InCourse
	AssignmentBody
}

func assignmentCreate() tool.Tool {
	return tool.Define(tool.Spec[AssignmentCreateIn, IDOut]{
		Name: "assignment.create",
		Description: "Create an assignment. It starts unpublished: students do not see it, and cannot submit to it, until " +
			"assignment.publish. title and points_possible are required.",
		Kind: tool.Write, Gate: writeAssignments,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments"},
		Resolve: func(_ context.Context, _ dbq.Querier, in AssignmentCreateIn) (tool.Target, error) {
			return tool.Target{CourseID: in.CourseID, Type: "assignment"}, nil
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AssignmentCreateIn) (IDOut, error) {
			if in.Title == nil || in.PointsPossible == nil {
				return IDOut{}, apperr.Invalid("title and points_possible are required")
			}
			a := dbq.GetAssignmentInCourseRow{ID: ids.New(), CourseID: in.CourseID}
			in.applyTo(&a)
			if err := checkAssignment(ctx, ec.Q, in.CourseID, a); err != nil {
				return IDOut{}, err
			}
			if err := ec.Q.InsertAssignment(ctx, dbq.InsertAssignmentParams{
				ID: a.ID, CourseID: a.CourseID, ComponentID: a.ComponentID, Title: a.Title,
				InstructionsDocumentID: a.InstructionsDocumentID, RubricDocumentID: a.RubricDocumentID,
				PointsPossible: a.PointsPossible, DueAt: a.DueAt, CreatedAt: ec.Now,
			}); err != nil {
				return IDOut{}, err
			}
			// A member limited to listed assignments who creates one can go
			// on working on it: it joins their list.
			if ec.Member.AssignmentScope == domain.ScopeListed {
				if err := ec.Q.AddAssignmentScope(ctx, dbq.AddAssignmentScopeParams{MemberID: ec.Member.ID, AssignmentID: a.ID}); err != nil {
					return IDOut{}, err
				}
			}
			ec.Emit(events.Event{Type: EventAssignmentCreated, CourseID: &in.CourseID, SubjectType: "assignment", SubjectID: &a.ID, AssignmentID: &a.ID})
			return IDOut{ID: a.ID}, nil
		},
	})
}

type AssignmentUpdateIn struct {
	tool.InCourse
	AssignmentID uuid.UUID `json:"assignment_id"`
	AssignmentBody
	ClearDueAt     bool `json:"clear_due_at,omitempty"`
	ClearComponent bool `json:"clear_component,omitempty" jsonschema:"take it out of the grade"`
}

func assignmentUpdate() tool.Tool {
	return tool.Define(tool.Spec[AssignmentUpdateIn, OK]{
		Name: "assignment.update",
		Description: "Change an assignment. Once any grade has been entered for it, what it is worth and where it counts " +
			"are fixed: a score is a score out of the points possible when it was given.",
		Kind: tool.Write, Gate: writeAssignments,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}"},
		Resolve: func(ctx context.Context, q dbq.Querier, in AssignmentUpdateIn) (tool.Target, error) {
			return assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AssignmentUpdateIn) (OK, error) {
			// Read under the row's lock. Every column is written back, and a
			// copy read before another change committed would quietly undo
			// it: a rename made alongside a new due date would put the old
			// due date back, with both calls reporting success. The row is
			// locked before checkAssignment takes the component-tree lock,
			// and lockAssignment says why that order is safe.
			a, err := lockAssignment(ctx, ec.Q, in.CourseID, in.AssignmentID)
			if err != nil {
				return OK{}, err
			}
			before := a
			in.applyTo(&a)
			if in.ClearDueAt {
				a.DueAt = nil
			}
			if in.ClearComponent {
				a.ComponentID = nil
			}
			// A score is a score out of the points possible at the time it was
			// given. Once any grade has been entered — posted, or a draft
			// waiting to be — what the assignment is worth, and where it
			// counts, no longer change: a 95 entered out of 100 would
			// otherwise be posted out of 50, with nobody having said so.
			movedInScheme := !a.PointsPossible.Equal(before.PointsPossible) || !sameID(a.ComponentID, before.ComponentID)
			if movedInScheme {
				if graded, err := ec.Q.AssignmentHasLiveGrades(ctx, a.ID); err != nil {
					return OK{}, err
				} else if graded {
					return OK{}, apperr.Precondition("grades have been entered for this assignment; its points and component no longer change")
				}
			}
			if err := checkAssignment(ctx, ec.Q, in.CourseID, a); err != nil {
				return OK{}, err
			}
			if err := ec.Q.UpdateAssignment(ctx, dbq.UpdateAssignmentParams{
				ID: a.ID, ComponentID: a.ComponentID, Title: a.Title, InstructionsDocumentID: a.InstructionsDocumentID,
				RubricDocumentID: a.RubricDocumentID, PointsPossible: a.PointsPossible, DueAt: a.DueAt,
			}); err != nil {
				return OK{}, err
			}
			ec.Emit(events.Event{Type: EventAssignmentUpdated, CourseID: &in.CourseID, SubjectType: "assignment", SubjectID: &a.ID, AssignmentID: &a.ID,
				Payload: map[string]any{"due_at_changed": !sameTime(a.DueAt, before.DueAt)}})
			return OK{OK: true}, nil
		},
	})
}

func assignmentPublish() tool.Tool {
	return tool.Define(tool.Spec[AssignmentIDIn, OK]{
		Name: "assignment.publish",
		Description: "Publish an assignment: students can now see it and submit to it. If it has an instructions document, " +
			"that document must have a published version for them to read.",
		Kind: tool.Write, Gate: writeAssignments,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/publish"},
		Resolve: func(ctx context.Context, q dbq.Querier, in AssignmentIDIn) (tool.Target, error) {
			return assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AssignmentIDIn) (OK, error) {
			// Under the row's lock too, so that the instructions checked are
			// the ones it is published with. An update pointing them at a
			// draft brief meanwhile either commits first and is seen here, or
			// waits, sees the assignment published and holds it to the same
			// rule.
			a, err := lockAssignment(ctx, ec.Q, in.CourseID, in.AssignmentID)
			if err != nil {
				return OK{}, err
			}
			if a.InstructionsDocumentID != nil {
				v, err := ec.Q.GetDocumentPublishedVersion(ctx, *a.InstructionsDocumentID)
				if err != nil {
					return OK{}, err
				}
				if v == nil {
					return OK{}, apperr.Precondition("the instructions have no published version yet; students would see an assignment with nothing to read")
				}
			}
			n, err := ec.Q.PublishAssignment(ctx, dbq.PublishAssignmentParams{ID: a.ID, PublishedAt: &ec.Now})
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the assignment is already published")
			}
			ec.Emit(events.Event{Type: EventAssignmentPublished, CourseID: &in.CourseID, SubjectType: "assignment", SubjectID: &a.ID, AssignmentID: &a.ID})
			return OK{OK: true}, nil
		},
	})
}

func assignmentUnpublish() tool.Tool {
	return tool.Define(tool.Spec[AssignmentIDIn, OK]{
		Name: "assignment.unpublish",
		Description: "Take back an assignment published by mistake: students no longer see it and cannot submit to it. " +
			"Only while nobody has a submission of any kind for it, not even a draft, and no 'missing' row has been " +
			"recorded (a due date that has passed records them); after that it stays published. " +
			"What the activity feed has already shown stays there.",
		Kind: tool.Write, Gate: writeAssignments,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/courses/{course_id}/assignments/{assignment_id}/unpublish"},
		Resolve: func(ctx context.Context, q dbq.Querier, in AssignmentIDIn) (tool.Target, error) {
			return assignmentTarget(ctx, q, in.CourseID, in.AssignmentID)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in AssignmentIDIn) (OK, error) {
			a, err := ec.Q.LockAssignmentForUnpublish(ctx, dbq.LockAssignmentForUnpublishParams{ID: in.AssignmentID, CourseID: in.CourseID})
			if err != nil {
				return OK{}, err
			}
			if a.PublishedAt == nil {
				return OK{}, apperr.Conflicts("the assignment is not published")
			}
			// Once someone has started, their work hangs on the assignment
			// being there; so does a grade for a 'missing' placeholder.
			if started, err := ec.Q.AssignmentHasSubmissions(ctx, a.ID); err != nil {
				return OK{}, err
			} else if started {
				return OK{}, apperr.Precondition("it already has submissions — a draft, a hand-in, or the 'missing' rows recorded by hand or when its due date passed — so it can no longer be unpublished")
			}
			n, err := ec.Q.UnpublishAssignment(ctx, a.ID)
			if err != nil {
				return OK{}, err
			}
			if n == 0 {
				return OK{}, apperr.Conflicts("the assignment is not published")
			}
			ec.Emit(events.Event{Type: EventAssignmentUnpublished, CourseID: &in.CourseID, SubjectType: "assignment", SubjectID: &a.ID, AssignmentID: &a.ID})
			return OK{OK: true}, nil
		},
	})
}

// lockAssignment reads an assignment and holds it until the transaction
// ends, for a tool that reads it, checks it and writes it back.
//
// The row lock (FOR NO KEY UPDATE) is the first lock the tool takes. For
// assignment.update the order is therefore the assignment row and then the
// course's component-tree lock, which checkAssignment takes when the
// assignment counts toward a component. Nothing takes the two the other way
// round: what holds the tree lock — component.create, update and move,
// assignment.create, and grade.submit for a component — takes no lock on an
// existing assignment row that this one holds off, so the two cannot
// deadlock. It does mean that while an update holds the row and waits for
// the tree, whoever grades that assignment's submissions waits behind it:
// grade.submit's checkSubject takes the row FOR SHARE
// (ShareAssignmentForGrading), which this lock holds off.
func lockAssignment(ctx context.Context, q *dbq.Queries, courseID, id uuid.UUID) (dbq.GetAssignmentInCourseRow, error) {
	a, err := q.GetAssignmentInCourseForUpdate(ctx, dbq.GetAssignmentInCourseForUpdateParams{ID: id, CourseID: courseID})
	return dbq.GetAssignmentInCourseRow(a), err
}

func sameID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
