package testkit

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ids"
)

// DeptTree is the world the department administrators' tests start from, on
// a Platform so that the tools can be called in it:
//
//	University (U, top) ── Engineering (F) ─┬─ Computing (S) ── AI (D)
//	                                        └─ Design (S2)
//	                    ── Humanities (F2) ── History (S3)
//
// Courses: CD in AI, active; CArch in AI, archived; CS in Computing, active;
// CS2 in Design; CS3 in History. Each has an instructor of its own; CS's is
// Chan.
//
// Appointments, all made by root: Ada at Engineering, Bob and Chan at
// Computing, Carol at Humanities. Admin is a platform administrator. Dan and
// Eve hold nothing. Ada is also a TA in CS listed for Yuki and Ken, a student
// in CS2, and owns the agent Robo. Yuki, Ken and Lin are students in CS, CS2
// and CD; Lin owns an agent too. Everyone has an email, their name in lower
// case at example.edu.
//
// S is the World's own department, Computing, placed under Engineering.
type DeptTree struct {
	*Platform

	U, F, S, D, S2, F2, S3  uuid.UUID
	CD, CArch, CS, CS2, CS3 uuid.UUID

	Admin                           uuid.UUID
	Ada, Bob, Carol, Chan, Dan, Eve uuid.UUID
	Robo                            uuid.UUID // Ada's agent
	Yuki, Ken, Lin, LinsAgent       uuid.UUID
	// Instructor is each course's instructor, by course.
	Instructor map[uuid.UUID]uuid.UUID

	// The appointments, by who holds them and where.
	AdaF, BobS, ChanS, CarolF2 uuid.UUID
}

func NewDeptTree(t testing.TB) *DeptTree {
	t.Helper()
	w := &DeptTree{Platform: NewPlatform(t), Instructor: map[uuid.UUID]uuid.UUID{}}

	dept := func(name string, parent *uuid.UUID) uuid.UUID {
		id := ids.New()
		w.Exec(`INSERT INTO department (id, name, parent_id) VALUES ($1, $2, $3)`, id, name, parent)
		return id
	}
	w.U = dept("University", nil)
	w.F = dept("Engineering", &w.U)
	w.S = w.Dept
	w.Exec(`UPDATE department SET parent_id = $2 WHERE id = $1`, w.S, w.F)
	w.D = dept("AI", &w.S)
	w.S2 = dept("Design", &w.F)
	w.F2 = dept("Humanities", &w.U)
	w.S3 = dept("History", &w.F2)

	w.Admin = w.person("Admin")
	w.Exec(`UPDATE actor SET platform_role = 'admin' WHERE id = $1`, w.Admin)
	w.Ada, w.Bob, w.Carol, w.Chan = w.person("Ada"), w.person("Bob"), w.person("Carol"), w.person("Chan")
	w.Dan, w.Eve = w.person("Dan"), w.person("Eve")
	w.Yuki, w.Ken, w.Lin = w.person("Yuki"), w.person("Ken"), w.person("Lin")
	w.Robo = w.OwnedAgent(w.Ada, "Robo")
	w.LinsAgent = w.OwnedAgent(w.Lin, "Lin's agent")

	w.CD = w.courseIn(w.D, "AI101", "active")
	w.CArch = w.courseIn(w.D, "AI100", "archived")
	w.CS = w.courseIn(w.S, "CS101", "active")
	w.CS2 = w.courseIn(w.S2, "DES101", "active")
	w.CS3 = w.courseIn(w.S3, "HIS101", "active")
	for course, code := range map[uuid.UUID]string{w.CD: "AI101", w.CArch: "AI100", w.CS2: "DES101", w.CS3: "HIS101"} {
		w.Instructor[course] = w.person("Instructor of " + code)
		w.Member(course, w.Instructor[course], "instructor")
	}
	w.Instructor[w.CS] = w.Chan
	w.Member(w.CS, w.Chan, "instructor")

	yukiCS, kenCS := w.Member(w.CS, w.Yuki, "student"), w.Member(w.CS, w.Ken, "student")
	w.Member(w.CS, w.Ada, "ta", ListedStudents(yukiCS, kenCS))
	w.Member(w.CS2, w.Ada, "student")
	for _, course := range []uuid.UUID{w.CS2, w.CD} {
		for _, s := range []uuid.UUID{w.Yuki, w.Ken, w.Lin} {
			w.Member(course, s, "student")
		}
	}
	w.Member(w.CS, w.Lin, "student")

	w.AdaF = w.Appoint(w.F, w.Ada, w.Root)
	w.BobS = w.Appoint(w.S, w.Bob, w.Root)
	w.ChanS = w.Appoint(w.S, w.Chan, w.Root)
	w.CarolF2 = w.Appoint(w.F2, w.Carol, w.Root)
	return w
}

// Appoint makes actor an administrator of dept, as by, and returns the
// appointment's id.
func (w *World) Appoint(dept, actor, by uuid.UUID) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`INSERT INTO department_admin (id, dept_id, actor_id, appointed_by_actor_id) VALUES ($1, $2, $3, $4)`,
		id, dept, actor, by)
	return id
}

// person registers a person whose email is their name in lower case, spaces
// taken out, at example.edu.
func (w *DeptTree) person(name string) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	email := strings.ToLower(strings.ReplaceAll(name, " ", "")) + "@example.edu"
	w.Exec(`INSERT INTO actor (id, kind, display_name, email, created_by_actor_id) VALUES ($1, 'human', $2, $3, $4)`,
		id, name, email, w.Root)
	return id
}

// courseIn creates a course in dept, with its root grade component.
func (w *DeptTree) courseIn(dept uuid.UUID, code, status string) uuid.UUID {
	w.T.Helper()
	id := ids.New()
	w.Exec(`INSERT INTO course (id, dept_id, term_id, code, title, status, created_by_actor_id)
	        VALUES ($1, $2, $3, $4, $4, $5, $6)`, id, dept, w.Term, code, status, w.Root)
	w.Exec(`INSERT INTO grade_component (id, course_id, name, sort_order) VALUES ($1, $2, 'Total', 0)`, ids.New(), id)
	return id
}
