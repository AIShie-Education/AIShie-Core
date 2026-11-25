package tools_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

func readCourse(t *testing.T, b *built, actor uuid.UUID) tools.CourseGetOut {
	t.Helper()
	return testkit.Result[tools.CourseGetOut](t, b.do(t, actor, "course.get", m{"course_id": b.course}))
}

func TestCourseGetListsCurrentInstructorsWithoutMemberRead(t *testing.T) {
	b := build(t)
	// This is deliberately not member_read: a student may know who teaches
	// the course without being given the roster or anyone's account details.
	b.Exec(`UPDATE course_member SET perm_member_read = 'denied' WHERE id = $1`, b.yukiM)

	aoi := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Aoi"})).ActorID
	mori := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Mori"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": aoi, "preset": "instructor"})
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "ta"})

	got := readCourse(t, b, b.yuki)
	want := []tools.CourseInstructor{{DisplayName: "Aoi"}, {DisplayName: "Sato"}}
	if !slices.Equal(got.Instructors, want) {
		t.Fatalf("instructors: %+v, want %+v; a TA is not an instructor", got.Instructors, want)
	}

	// The JSON contract has one field per instructor: a display name, not an
	// actor/member id, email, login id or any other account detail.
	out := b.do(t, b.yuki, "course.get", m{"course_id": b.course})
	var raw map[string]any
	if err := json.Unmarshal(out.Result, &raw); err != nil {
		t.Fatal(err)
	}
	listed, ok := raw["instructors"].([]any)
	if !ok || len(listed) != 2 {
		t.Fatalf("instructors JSON: %s", out.Result)
	}
	for _, item := range listed {
		entry, ok := item.(map[string]any)
		if !ok || len(entry) != 1 || entry["display_name"] == nil {
			t.Fatalf("an instructor exposes more than a display name: %s", out.Result)
		}
	}
}

func TestCourseGetLeavesOutInactiveInstructors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*testing.T, *built)
	}{
		{"paused", func(t *testing.T, b *built) {
			b.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, b.satoM)
		}},
		{"removed", func(t *testing.T, b *built) {
			b.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, b.satoM)
		}},
		{"expired", func(t *testing.T, b *built) {
			b.Exec(`UPDATE course_member SET expires_at = $2 WHERE id = $1`, b.satoM, time.Now().Add(-time.Hour))
		}},
		{"suspended", func(t *testing.T, b *built) {
			b.do(t, b.admin, "actor.suspend", m{"actor_id": b.sato})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := build(t)
			tc.arrange(t, b)
			got := readCourse(t, b, b.yuki)
			if got.Instructors == nil || len(got.Instructors) != 0 {
				t.Fatalf("%s instructor is still shown: %+v", tc.name, got.Instructors)
			}
		})
	}
}

func TestCourseGetInstructorsAreNotAPublicMemberList(t *testing.T) {
	b := build(t)
	out, err := b.Call(b.admin, "course.get", m{"course_id": b.course}, "")
	if err != nil || out.Status != domain.StatusDenied || len(out.Result) != 0 {
		t.Fatalf("an administrator with no seat reads the course and its instructors: %+v, %v", out, err)
	}
	out, err = b.Call(uuid.New(), "course.get", m{"course_id": b.course}, "")
	if err == nil || len(out.Result) != 0 {
		t.Fatalf("an unknown actor reads the course and its instructors: %+v, %v", out, err)
	}
	if out, err := b.Call(b.yuki, "course.get", m{"course_id": uuid.New()}, ""); err == nil || out.Status == domain.StatusExecuted {
		t.Fatalf("an unknown course gives a student something: %+v, %v", out, err)
	}
}
