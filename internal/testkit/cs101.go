package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// Component adds a grade component under parent. points is "" for a
// component that is rolled up rather than graded directly.
func (w *World) Component(course uuid.UUID, parent uuid.UUID, name, weight, points string, dropLowest int) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	var pts any
	if points != "" {
		pts = points
	}
	w.Exec(`INSERT INTO grade_component (id, course_id, parent_id, name, weight, points_possible, drop_lowest)
	        VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7)`, id, course, parent, name, weight, pts, dropLowest)
	return id
}

func (w *World) RootComponent(course uuid.UUID) uuid.UUID {
	w.T.Helper()
	var id uuid.UUID
	if err := w.Pool.QueryRow(context.Background(),
		`SELECT id FROM grade_component WHERE course_id = $1 AND parent_id IS NULL`, course).Scan(&id); err != nil {
		w.T.Fatalf("fixture: root component: %v", err)
	}
	return id
}

// GradedAssignment adds a published assignment that counts toward component.
func (w *World) GradedAssignment(course, component uuid.UUID, title, points string) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`INSERT INTO assignment (id, course_id, component_id, title, points_possible, published_at)
	        VALUES ($1, $2, $3, $4, $5::numeric, now())`, id, course, component, title, points)
	return id
}

// Rubric gives an assignment a rubric document with one published version,
// and returns that version's id.
func (w *World) Rubric(course, assignment, author uuid.UUID) uuid.UUID {
	w.T.Helper()
	doc, ver := ids.New(), ids.New()
	w.Exec(`INSERT INTO document (id, course_id, kind, title) VALUES ($1, $2, 'rubric', 'Rubric')`, doc, course)
	w.Exec(`INSERT INTO document_version (id, document_id, seq, body_md, author_member_id)
	        VALUES ($1, $2, 1, 'Thesis 4 · Evidence 4 · Style 2', $3)`, ver, doc, author)
	w.Exec(`UPDATE document SET published_version_id = $1 WHERE id = $2`, ver, doc)
	w.Exec(`UPDATE assignment SET rubric_document_id = $1 WHERE id = $2`, doc, assignment)
	return ver
}

// Submission records submitted work.
func (w *World) Submission(course, assignment, student uuid.UUID) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`INSERT INTO submission (id, assignment_id, course_id, student_member_id, body, state, submitted_at)
	        VALUES ($1, $2, $3, $4, 'my essay', 'submitted', now())`, id, assignment, course, student)
	return id
}

// Student is one enrolled student with their HW3 submission.
type Student struct {
	Actor, Member, HW3 uuid.UUID
}

// CS101 is the course from docs/schema.md §5:
//
//	Total ─┬─ Assignments (weight 40) ── HW3 (100 pts, rubric), HW4 (100 pts)
//	       └─ Midterm     (weight 30, 100 pts, graded directly)
//
// Sato is the instructor. grader-v2 is an agent seated with the grader
// preset — perm_grade_submit = confirm_required — and listed for HW3 only.
// Every student has submitted HW3. Yuki is Students[0], Ken is Students[1].
type CS101 struct {
	*Platform

	Course                      uuid.UUID
	Total, Assignments, Midterm uuid.UUID
	HW3, HW4, RubricVersion     uuid.UUID
	Sato, SatoM                 uuid.UUID
	Grader, GraderM             uuid.UUID
	Students                    []Student
}

func NewCS101(t testing.TB, students int) *CS101 {
	t.Helper()
	return NewCS101WithStore(t, students, func(fs *blob.FSStore) blob.Store { return fs })
}

// NewCS101WithStore is NewCS101 with the tools talking to whatever wrap makes
// of the filesystem store; see NewPlatformWithStore.
func NewCS101WithStore(t testing.TB, students int, wrap func(*blob.FSStore) blob.Store) *CS101 {
	t.Helper()
	return cs101On(NewPlatformWithStore(t, wrap), students)
}

// NewCS101WithDeps is NewCS101 with the tools' settings adjusted as adjust
// says: memory switched on, say.
func NewCS101WithDeps(t testing.TB, students int, adjust func(*tools.Deps)) *CS101 {
	t.Helper()
	return cs101On(NewPlatformWithDeps(t, adjust), students)
}

func cs101On(p *Platform, students int) *CS101 {
	p.T.Helper()
	w := p.World
	c := &CS101{Platform: p}

	c.Course = w.Course("CS101")
	c.Total = w.RootComponent(c.Course)
	c.Assignments = w.Component(c.Course, c.Total, "Assignments", "40", "", 0)
	c.Midterm = w.Component(c.Course, c.Total, "Midterm", "30", "100", 0)
	c.HW3 = w.GradedAssignment(c.Course, c.Assignments, "HW3", "100")
	c.HW4 = w.GradedAssignment(c.Course, c.Assignments, "HW4", "100")

	c.Sato = w.Actor("human", "Sato")
	c.SatoM = w.Member(c.Course, c.Sato, "instructor")
	c.RubricVersion = w.Rubric(c.Course, c.HW3, c.SatoM)
	c.Grader = w.Actor("agent", "grader-v2")
	c.GraderM = w.Member(c.Course, c.Grader, "grader", ListedAssignments(c.HW3))

	names := []string{"Yuki", "Ken"}
	for i := range students {
		name := fmt.Sprintf("Student %02d", i+1)
		if i < len(names) {
			name = names[i]
		}
		s := Student{Actor: w.Actor("human", name)}
		s.Member = w.Member(c.Course, s.Actor, "student")
		s.HW3 = w.Submission(c.Course, c.HW3, s.Member)
		c.Students = append(c.Students, s)
	}
	return c
}

// Platform is a fresh database with the whole tool catalogue on a pipeline,
// and nothing in it but root, a term and a department. Tests that build their
// world through the tools themselves start here.
type Platform struct {
	*World
	P *pipeline.Pipeline
	// Blob keeps files in the test's own temporary directory.
	Blob    *blob.FSStore
	Uploads *blob.Signer
}

// MaxUploadBytes is small, so that a test can exceed it.
const MaxUploadBytes = 1 << 16

func NewPlatform(t testing.TB) *Platform {
	t.Helper()
	return NewPlatformWithStore(t, func(fs *blob.FSStore) blob.Store { return fs })
}

// NewPlatformWithStore is NewPlatform with the tools talking to whatever wrap
// makes of the filesystem store. Platform.Blob is still the filesystem store
// underneath, so a test can PUT and read bytes the way a client would.
func NewPlatformWithStore(t testing.TB, wrap func(*blob.FSStore) blob.Store) *Platform {
	t.Helper()
	return newPlatform(t, wrap, pipeline.Config{ProposalTTL: pipeline.DefaultProposalTTL})
}

// NewPlatformWithConfig is NewPlatform with the pipeline set up as cfg says.
func NewPlatformWithConfig(t testing.TB, cfg pipeline.Config) *Platform {
	t.Helper()
	return newPlatform(t, func(fs *blob.FSStore) blob.Store { return fs }, cfg)
}

// NewPlatformWithDeps is NewPlatform with the tools' settings adjusted as
// adjust says: agent self-service turned off, say.
func NewPlatformWithDeps(t testing.TB, adjust func(*tools.Deps)) *Platform {
	t.Helper()
	return newPlatform(t, func(fs *blob.FSStore) blob.Store { return fs }, pipeline.Config{ProposalTTL: pipeline.DefaultProposalTTL}, adjust)
}

func newPlatform(t testing.TB, wrap func(*blob.FSStore) blob.Store, cfg pipeline.Config, adjust ...func(*tools.Deps)) *Platform {
	t.Helper()
	w := NewWorld(t)
	signer, err := blob.NewSigner("")
	if err != nil {
		t.Fatal(err)
	}
	store, err := blob.NewFSStore(t.TempDir(), "http://lms.test", signer)
	if err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry()
	p := &Platform{World: w, Blob: store, Uploads: signer, P: pipeline.New(w.Pool, reg, cfg)}
	deps := tools.Deps{Pipeline: p.P, Blob: wrap(store), Uploads: signer, MaxUploadBytes: MaxUploadBytes}
	for _, a := range adjust {
		a(&deps)
	}
	tools.RegisterAll(reg, deps)
	return p
}

// Call invokes a tool as actor. args is marshalled to JSON.
func (c *Platform) Call(actor uuid.UUID, name string, args any, key string) (pipeline.Outcome, error) {
	c.T.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		c.T.Fatalf("marshal args: %v", err)
	}
	return c.P.Invoke(context.Background(), pipeline.Caller{ActorID: actor}, name, raw, key)
}

// MustCall is Call for calls that are expected to be attempted: it fails the
// test on an error (as opposed to a recorded denial or failure).
func (c *Platform) MustCall(actor uuid.UUID, name string, args any, key string) pipeline.Outcome {
	c.T.Helper()
	out, err := c.Call(actor, name, args, key)
	if err != nil {
		c.T.Fatalf("%s: %v", name, err)
	}
	return out
}

// Count runs a SELECT count(*) style query.
func (w *World) Count(sql string, args ...any) int {
	w.T.Helper()
	var n int
	if err := w.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		w.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// Result unmarshals an outcome's result into v.
func Result[T any](t testing.TB, out pipeline.Outcome) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(out.Result, &v); err != nil {
		t.Fatalf("result %s: %v", out.Result, err)
	}
	return v
}
