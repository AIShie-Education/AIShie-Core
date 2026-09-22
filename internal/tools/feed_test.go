package tools_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

func feed(t *testing.T, b *built, actor uuid.UUID) []tools.EventView {
	t.Helper()
	var all []tools.EventView
	var since int64
	for {
		out := b.MustCall(actor, "event.list", m{"course_id": b.course, "since_seq": since, "limit": 7}, "")
		if out.Status != domain.StatusExecuted {
			t.Fatalf("event.list: %+v", out)
		}
		page := testkit.Result[tools.EventListOut](t, out)
		all = append(all, page.Events...)
		since = page.NextSeq
		if !page.More {
			return all
		}
	}
}

func types(evs []tools.EventView) map[string]int {
	out := map[string]int{}
	for _, e := range evs {
		out[e.Type]++
	}
	return out
}

// The feed must never show a student, or a tutor listed for one student,
// anything about another. This is the test the event scope columns exist for.
func TestFeedIsolation(t *testing.T) {
	b := build(t)
	yukiWork, kenWork := b.submit(t, b.yuki, "Yuki's essay"), b.submit(t, b.ken, "Ken's essay")

	proposed := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": yukiWork, "score": 85}, "p1")
	rejected := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 5}, "p2")
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"})
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": rejected.ActionID, "decision": "reject", "reason": "too harsh"})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.yukiM, "score": 90})
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3})

	about := func(evs []tools.EventView, student uuid.UUID) int {
		n := 0
		for _, e := range evs {
			if e.StudentMemberID != nil && *e.StudentMemberID == student {
				n++
			}
		}
		return n
	}

	t.Run("a student sees her own posted work and nothing of anyone else's", func(t *testing.T) {
		evs := feed(t, b, b.yuki)
		if about(evs, b.kenM) != 0 {
			t.Fatalf("Yuki's feed has %d events about Ken", about(evs, b.kenM))
		}
		got := types(evs)
		if got["grade.posted"] != 1 || got["grade.total_updated"] != 2 || got["submission.submitted"] != 1 {
			t.Fatalf("Yuki's own events: %v", got)
		}
		// Drafts, the action log and the roster are not hers to see.
		for _, hidden := range []string{"grade.created", "action.proposed", "action.approved", "action.rejected", "member.added", "assignment.created"} {
			if got[hidden] != 0 {
				t.Errorf("Yuki sees %d %s events", got[hidden], hidden)
			}
		}
		if got["assignment.published"] != 1 {
			t.Errorf("Yuki should see that HW3 was published: %v", got)
		}
	})

	t.Run("a tutor listed for one student sees that student only", func(t *testing.T) {
		evs := feed(t, b, b.tutor)
		if about(evs, b.kenM) != 0 || about(evs, b.yukiM) == 0 {
			t.Fatalf("tutor's feed: %d about Yuki, %d about Ken", about(evs, b.yukiM), about(evs, b.kenM))
		}
	})

	t.Run("an agent learns what became of its own proposals, and nothing else of the action log", func(t *testing.T) {
		evs := feed(t, b, b.grader)
		got := types(evs)
		if got["action.proposed"] != 2 || got["action.approved"] != 1 || got["action.rejected"] != 1 {
			t.Fatalf("the grader's own action events: %v", got)
		}
		for _, e := range evs {
			if e.Type == "action.approved" && *e.ActionID != *proposed.ActionID {
				t.Fatalf("approval filed under %s, want the proposal %s", e.ActionID, proposed.ActionID)
			}
		}
		// It grades HW3 for the whole class, so drafts for HW3 are within its
		// scope whoever wrote them: its own, and the one Sato entered for
		// Ken. That is ids only, and it is how the agent knows Ken's work no
		// longer needs grading. The midterm draft is not HW3 and is not here.
		if got["grade.created"] != 2 {
			t.Fatalf("draft grades in the grader's feed: %v", got)
		}
		for _, e := range evs {
			if e.Type == "grade.created" && (e.AssignmentID == nil || *e.AssignmentID != b.hw3) {
				t.Fatalf("the HW3 grader sees a draft outside HW3: %+v", e)
			}
		}
		// The grader preset cannot read grades, so nothing posted reaches it.
		if got["grade.posted"] != 0 || got["grade.total_updated"] != 0 {
			t.Fatalf("grader sees posted grades without perm_grade_read: %v", got)
		}
	})

	t.Run("listed for one assignment: no totals, no component grades, even with grade_read", func(t *testing.T) {
		b.Exec(`UPDATE course_member SET perm_grade_read = 'autonomous' WHERE id = $1`, b.graderM)
		b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{midtermDraft(t, b)}})
		got := types(feed(t, b, b.grader))
		if got["grade.posted"] != 2 { // HW3 for Yuki and Ken; not the midterm
			t.Fatalf("grade.posted events: %v", got)
		}
		if got["grade.total_updated"] != 0 {
			t.Fatalf("a member listed for HW3 alone sees course totals: %v", got)
		}
	})

	t.Run("the instructor sees everything", func(t *testing.T) {
		got := types(feed(t, b, b.sato))
		for _, typ := range []string{"member.added", "assignment.created", "assignment.published", "submission.submitted",
			"action.proposed", "action.approved", "action.rejected", "grade.created", "grade.posted", "grade.total_updated", "component.created"} {
			if got[typ] == 0 {
				t.Errorf("the instructor's feed has no %s", typ)
			}
		}
	})

	t.Run("not a member: denied, and nothing about the course leaks", func(t *testing.T) {
		out := b.MustCall(b.admin, "event.list", m{"course_id": b.course}, "")
		if out.Status != domain.StatusDenied {
			t.Fatalf("a platform admin with no seat reading a course's feed: %+v", out)
		}
	})
}

func midtermDraft(t *testing.T, b *built) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT id FROM grade WHERE component_id = $1 AND origin = 'entered' AND posted_at IS NULL`, b.midterm).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestScopedLists(t *testing.T) {
	b := build(t)
	yukiWork, kenWork := b.submit(t, b.yuki, "Yuki's essay"), b.submit(t, b.ken, "Ken's essay")
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": yukiWork, "score": 85})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70})

	subs := func(actor uuid.UUID) []tools.SubmissionView {
		return testkit.Result[tools.SubmissionListOut](t, b.do(t, actor, "submission.list", m{"course_id": b.course})).Submissions
	}
	if got := subs(b.sato); len(got) != 2 {
		t.Fatalf("instructor sees %d submissions", len(got))
	}
	for name, actor := range map[string]uuid.UUID{"Yuki": b.yuki, "the tutor listed for Yuki": b.tutor} {
		got := subs(actor)
		if len(got) != 1 || got[0].ID != yukiWork {
			t.Fatalf("%s sees %+v, want Yuki's submission alone", name, got)
		}
	}
	// Asking for someone else's by filter is not an error; it is just empty.
	filtered := testkit.Result[tools.SubmissionListOut](t, b.do(t, b.yuki, "submission.list", m{"course_id": b.course, "student_member_id": b.kenM}))
	if len(filtered.Submissions) != 0 {
		t.Fatalf("Yuki filtered her way to Ken's work: %+v", filtered.Submissions)
	}
	// But fetching it by id is denied.
	if out := b.MustCall(b.yuki, "submission.get", m{"course_id": b.course, "submission_id": kenWork}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("Yuki reading Ken's submission: %+v", out)
	}

	// Drafts are for those who grade.
	grades := func(actor uuid.UUID) []tools.GradeView {
		return testkit.Result[tools.GradeListOut](t, b.do(t, actor, "grade.list", m{"course_id": b.course})).Grades
	}
	if got := grades(b.yuki); len(got) != 0 {
		t.Fatalf("Yuki sees %d unposted grades", len(got))
	}
	if got := grades(b.sato); len(got) != 2 {
		t.Fatalf("instructor sees %d grades, want both drafts", len(got))
	}
	draft := grades(b.sato)[0]
	if out, err := b.Call(b.yuki, "grade.get", m{"course_id": b.course, "grade_id": draft.ID}, ""); err == nil && out.Status == domain.StatusExecuted {
		t.Fatalf("a student fetched an unposted grade by id: %+v", out)
	}

	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3})
	got := grades(b.yuki)
	if len(got) != 3 {
		t.Fatalf("after posting Yuki sees %d grades, want HW3 and two totals", len(got))
	}
	// A member listed for HW3 alone, given grade_read: submission grades for
	// HW3, and none of the component totals.
	b.Exec(`UPDATE course_member SET perm_grade_read = 'autonomous' WHERE id = $1`, b.graderM)
	for _, g := range grades(b.grader) {
		if g.ComponentID != nil || g.AssignmentID == nil || *g.AssignmentID != b.hw3 {
			t.Fatalf("an HW3-only member sees %+v", g)
		}
	}
	if n := len(grades(b.grader)); n != 2 {
		t.Fatalf("the HW3-only member sees %d grades, want the two HW3 grades", n)
	}

	// Unpublished assignments exist only for those who write them.
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW4", "points_possible": 50})).ID
	list := func(actor uuid.UUID) int {
		return len(testkit.Result[tools.AssignmentListOut](t, b.do(t, actor, "assignment.list", m{"course_id": b.course})).Assignments)
	}
	if list(b.sato) != 2 || list(b.yuki) != 1 || list(b.grader) != 1 {
		t.Fatalf("assignment lists: instructor %d, student %d, HW3-only grader %d", list(b.sato), list(b.yuki), list(b.grader))
	}
	if _, err := b.Call(b.yuki, "assignment.get", m{"course_id": b.course, "assignment_id": hw4}, ""); err == nil {
		t.Fatal("a student fetched an unpublished assignment")
	}
	if out := b.MustCall(b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw4}, "early"); out.Status != domain.StatusFailed {
		t.Fatalf("submitting to an unpublished assignment: %+v", out)
	}
}

// A file archived from a submission or a grade is that student's business,
// like its creation: the event carries the student and the assignment, so
// scope applies, and it is seen by those who read the submission rather than
// by those who read drafts.
func TestArchivedFileEventsStayInScope(t *testing.T) {
	b := build(t)
	draft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3})).SubmissionID
	file := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.ken, "document.create",
		m{"course_id": b.course, "kind": "submission", "title": "oops.txt", "submission_id": draft, "body_md": "wrong file"})).DocumentID
	b.do(t, b.ken, "document.archive", m{"course_id": b.course, "document_id": file})
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'submission.file_archived' AND subject_id = $1 AND student_member_id = $2 AND assignment_id = $3`, file, b.kenM, b.hw3); n != 1 {
		t.Fatal("the archived-file event does not say whose it is")
	}
	// The tutor is listed for Yuki and may read drafts; Ken's file is not
	// its business. The grader, listed for HW3, reads the submission.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"document_read_draft": "autonomous"}})
	sees := func(actor uuid.UUID) bool {
		for _, ev := range feed(t, b, actor) {
			if ev.SubjectID != nil && *ev.SubjectID == file {
				return true
			}
		}
		return false
	}
	if sees(b.tutor) {
		t.Fatal("the tutor saw Ken's file go, out of its scope")
	}
	if !sees(b.grader) || !sees(b.ken) {
		t.Fatal("those who read the submission did not see its file go")
	}
}
