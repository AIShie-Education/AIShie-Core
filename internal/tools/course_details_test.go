package tools_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// A course's instructors name and describe it from their seat; what makes it
// the offering it is, where it sits and whether it is open stay with its
// administrators.
func TestInstructorsRetitleTheirCourse(t *testing.T) {
	b := build(t)
	details := func(args m) m {
		args["course_id"] = b.course
		return args
	}
	out := b.do(t, b.sato, "course.update_details", details(m{"title": "Computing for Everyone", "description": "No experience needed."}))
	if !testkit.Result[tools.CourseUpdateDetailsOut](t, out).Changed {
		t.Fatal("the change says it changed nothing")
	}
	got := testkit.Result[tools.CourseView](t, b.do(t, b.yuki, "course.get", m{"course_id": b.course}))
	if got.Title != "Computing for Everyone" || got.Description == nil || *got.Description != "No experience needed." || got.Code != "CS101" {
		t.Fatalf("the course reads %+v", got)
	}
	// On record, from Sato's seat, and in the feed of those who read the course.
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND action_type = 'course.update_details' AND member_id = $2
		AND target_type = 'course' AND target_id = $3 AND status = 'executed'`, *out.ActionID, b.satoM, b.course); n != 1 {
		t.Fatal("the change is not on record as Sato's")
	}
	updated := func() int {
		n := 0
		for _, e := range feed(t, b, b.yuki) {
			if e.Type == "course.updated" {
				n++
			}
		}
		return n
	}
	if updated() != 1 {
		t.Fatal("a student is not told the course was renamed")
	}
	// What it already says changes nothing, and says so; one field alone
	// leaves the other as it is.
	if testkit.Result[tools.CourseUpdateDetailsOut](t, b.do(t, b.sato, "course.update_details", details(m{"title": "Computing for Everyone"}))).Changed {
		t.Fatal("the same title again was a change")
	}
	b.do(t, b.sato, "course.update_details", details(m{"description": "Bring a laptop."}))
	got = testkit.Result[tools.CourseView](t, b.do(t, b.sato, "course.get", m{"course_id": b.course}))
	if got.Title != "Computing for Everyone" || *got.Description != "Bring a laptop." {
		t.Fatalf("the course reads %+v", got)
	}
	if updated() != 2 {
		t.Fatal("want one event for each change, and none for the change that was none")
	}

	// Only those who manage the course's members.
	for name, actor := range map[string]uuid.UUID{"a student": b.yuki, "a grading agent": b.grader} {
		if out := b.MustCall(actor, "course.update_details", details(m{"title": "Mine"}), "x-"+name); out.Status != domain.StatusDenied {
			t.Fatalf("%s: %+v", name, out)
		}
	}
	// An administrator, from outside the course, uses course.update: here,
	// without a seat, they are nobody.
	if out := b.MustCall(b.admin, "course.update_details", details(m{"title": "Mine"}), "admin"); reason(out) != "not_a_member" {
		t.Fatalf("an administrator without a seat: %+v", out)
	}
	// The rest stays with administrators: there are no such fields here.
	for _, field := range []string{"code", "section", "term_id", "dept_id", "status"} {
		if _, err := b.Call(b.sato, "course.update_details", details(m{field: "x"}), "f-"+field); !apperr.Is(err, apperr.InvalidArgument) {
			t.Fatalf("%s: %v", field, err)
		}
	}
	b.try(t, b.sato, "course.update_details", details(m{"title": "  "}), apperr.InvalidArgument)
	b.try(t, b.sato, "course.update_details", details(m{}), apperr.InvalidArgument)
	// An archived course takes no writes, this one included.
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	if out := b.MustCall(b.sato, "course.update_details", details(m{"title": "Too late"}), "archived"); reason(out) != "course_archived" {
		t.Fatalf("an archived course: %+v", out)
	}
}
