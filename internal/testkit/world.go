// Package testkit builds course fixtures for tests, straight through SQL.
// It deliberately does not go through the tool layer: tests of authorization
// and of the pipeline need a world to exist before either can run.
package testkit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testdb"
)

// World is one fresh database with a root actor, a term and a department.
type World struct {
	T    testing.TB
	Pool *pgxpool.Pool
	Q    *dbq.Queries
	Root uuid.UUID
	Term uuid.UUID
	Dept uuid.UUID
}

func NewWorld(t testing.TB) *World {
	t.Helper()
	pool := testdb.New(t)
	w := &World{T: t, Pool: pool, Q: dbq.New(pool), Root: ids.New(), Term: ids.New(), Dept: ids.New()}
	w.Exec(`INSERT INTO actor (id, kind, display_name, platform_role) VALUES ($1, 'human', 'root', 'root')`, w.Root)
	w.Exec(`INSERT INTO term (id, name, starts_on, ends_on) VALUES ($1, '2026 Autumn', '2026-09-01', '2026-12-20')`, w.Term)
	w.Exec(`INSERT INTO department (id, name) VALUES ($1, 'Computing')`, w.Dept)
	return w
}

// Exec runs a statement and fails the test on error.
func (w *World) Exec(sql string, args ...any) {
	w.T.Helper()
	if _, err := w.Pool.Exec(context.Background(), sql, args...); err != nil {
		w.T.Fatalf("fixture: %v\n%s", err, sql)
	}
}

// Actor registers an actor of the given kind: human, agent or system.
func (w *World) Actor(kind, name string) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`INSERT INTO actor (id, kind, display_name, created_by_actor_id) VALUES ($1, $2, $3, $4)`, id, kind, name, w.Root)
	return id
}

// Course creates an active course with its root grade component.
func (w *World) Course(code string) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`INSERT INTO course (id, dept_id, term_id, code, title, status, created_by_actor_id)
	        VALUES ($1, $2, $3, $4, $4, 'active', $5)`, id, w.Dept, w.Term, code, w.Root)
	w.Exec(`INSERT INTO grade_component (id, course_id, name, sort_order) VALUES ($1, $2, 'Total', 0)`, ids.New(), id)
	return id
}

func (w *World) Assignment(course uuid.UUID, title string) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`INSERT INTO assignment (id, course_id, title, points_possible, published_at)
	        VALUES ($1, $2, $3, 100, now())`, id, course, title)
	return id
}

// MemberOpt adjusts a membership after the preset has been copied onto it.
type MemberOpt func(w *World, memberID uuid.UUID)

func WithPerm(p domain.Perm, l domain.Level) MemberOpt {
	return func(w *World, m uuid.UUID) {
		// p.Column() comes from a fixed list of constants, never from input.
		w.Exec(fmt.Sprintf(`UPDATE course_member SET %s = $2 WHERE id = $1`, p.Column()), m, l.String())
	}
}

// ListedStudents narrows the member to exactly these students. None listed
// means none in scope.
func ListedStudents(students ...uuid.UUID) MemberOpt {
	return func(w *World, m uuid.UUID) {
		w.Exec(`UPDATE course_member SET student_scope = 'listed' WHERE id = $1`, m)
		w.Exec(`DELETE FROM member_student_scope WHERE member_id = $1`, m)
		for _, s := range students {
			w.Exec(`INSERT INTO member_student_scope (member_id, student_member_id) VALUES ($1, $2)`, m, s)
		}
	}
}

func ListedAssignments(assignments ...uuid.UUID) MemberOpt {
	return func(w *World, m uuid.UUID) {
		w.Exec(`UPDATE course_member SET assignment_scope = 'listed' WHERE id = $1`, m)
		w.Exec(`DELETE FROM member_assignment_scope WHERE member_id = $1`, m)
		for _, a := range assignments {
			w.Exec(`INSERT INTO member_assignment_scope (member_id, assignment_id) VALUES ($1, $2)`, m, a)
		}
	}
}

func WithStatus(status string) MemberOpt {
	return func(w *World, m uuid.UUID) {
		w.Exec(`UPDATE course_member SET status = $2 WHERE id = $1`, m, status)
	}
}

func Expires(at time.Time) MemberOpt {
	return func(w *World, m uuid.UUID) {
		w.Exec(`UPDATE course_member SET expires_at = $2 WHERE id = $1`, m, at)
	}
}

// Member seats an actor in a course by copying a built-in preset onto the
// row, the way member.add does. A member whose preset lists students and
// whose role is student gets the scope row pointing at itself.
func (w *World) Member(course, actor uuid.UUID, preset string, opts ...MemberOpt) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`
		INSERT INTO course_member (
			id, course_id, actor_id, role, preset_id, added_by_actor_id, student_scope, assignment_scope,
			perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
			perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
			perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide,
			perm_agent_delegate, perm_conversation_ask, perm_conversation_answer, perm_member_invite)
		SELECT $1, $2, $3, p.role, p.id, $4, p.student_scope, p.assignment_scope,
			p.perm_document_read, p.perm_document_read_draft, p.perm_document_write, p.perm_rubric_read,
			p.perm_assignment_write, p.perm_submission_read, p.perm_submission_write, p.perm_grade_read,
			p.perm_grade_submit, p.perm_grade_post, p.perm_member_read, p.perm_member_manage, p.perm_action_decide,
			p.perm_agent_delegate, p.perm_conversation_ask, p.perm_conversation_answer, p.perm_member_invite
		FROM permission_preset p WHERE p.name = $5 AND p.dept_id IS NULL`,
		id, course, actor, w.Root, preset)

	var found bool
	if err := w.Pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM course_member WHERE id = $1)`, id).Scan(&found); err != nil || !found {
		w.T.Fatalf("fixture: no built-in preset named %q (err=%v)", preset, err)
	}
	w.Exec(`INSERT INTO member_student_scope (member_id, student_member_id)
	        SELECT id, id FROM course_member WHERE id = $1 AND role = 'student' AND student_scope = 'listed'`, id)
	for _, o := range opts {
		o(w, id)
	}
	return id
}

// OwnedAgent registers an agent that owner owns.
func (w *World) OwnedAgent(owner uuid.UUID, name string) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`INSERT INTO actor (id, kind, display_name, owner_actor_id, created_by_actor_id) VALUES ($1, 'agent', $2, $3, $3)`,
		id, name, owner)
	return id
}

// Delegate seats an owned agent as the delegate of principal, its owner's
// seat, copying a built-in preset as Member does. Its scope is left as the
// preset says, lists empty: opts narrow or widen it.
func (w *World) Delegate(course, agent, principal uuid.UUID, preset string, opts ...MemberOpt) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`
		INSERT INTO course_member (
			id, course_id, actor_id, role, preset_id, added_by_actor_id, student_scope, assignment_scope,
			perm_document_read, perm_document_read_draft, perm_document_write, perm_rubric_read,
			perm_assignment_write, perm_submission_read, perm_submission_write, perm_grade_read,
			perm_grade_submit, perm_grade_post, perm_member_read, perm_member_manage, perm_action_decide,
			perm_agent_delegate, perm_conversation_ask, perm_conversation_answer, perm_member_invite, principal_member_id)
		SELECT $1, $2, $3, p.role, p.id, $4, p.student_scope, p.assignment_scope,
			p.perm_document_read, p.perm_document_read_draft, p.perm_document_write, p.perm_rubric_read,
			p.perm_assignment_write, p.perm_submission_read, p.perm_submission_write, p.perm_grade_read,
			p.perm_grade_submit, p.perm_grade_post, p.perm_member_read, p.perm_member_manage, p.perm_action_decide,
			p.perm_agent_delegate, p.perm_conversation_ask, p.perm_conversation_answer, p.perm_member_invite, $6
		FROM permission_preset p WHERE p.name = $5 AND p.dept_id IS NULL`,
		id, course, agent, w.Root, preset, principal)
	for _, o := range opts {
		o(w, id)
	}
	return id
}
