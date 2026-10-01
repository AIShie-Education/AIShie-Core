package tools_test

import (
	"context"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// try calls a tool expecting it to be recorded as failed with the given code.
func (b *built) try(t *testing.T, actor uuid.UUID, name string, args m, want apperr.Code) {
	t.Helper()
	b.key++
	out, err := b.Call(actor, name, args, "try-"+uuid.NewString())
	switch {
	case err != nil && apperr.Is(err, want):
		return // refused before it was an attempt
	case err != nil:
		t.Fatalf("%s: %v, want %s", name, err, want)
	case out.Status == domain.StatusExecuted || out.Status == domain.StatusProposed:
		t.Fatalf("%s went through (%s), want %s", name, out.Status, want)
	case out.Error == nil || out.Error.Code != want:
		t.Fatalf("%s: %+v, want %s", name, out.Error, want)
	}
}

// start makes a call in the background; its outcome arrives on done.
func (b *built) start(t *testing.T, done chan<- pipeline.Outcome, actor uuid.UUID, name string, args m) {
	go func() {
		out, err := b.Call(actor, name, args, "bg-"+uuid.NewString())
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		done <- out
	}()
}

// hold locks rows the way a change already under way would, until release
// is called; calls made meanwhile queue behind it.
func (b *built) hold(t *testing.T, sql string, args ...any) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := b.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// blocked waits until n calls are waiting for a lock in this test's
// database. It stops early once done holds an outcome: a call that should
// have waited finished instead, and what came of it is for the test to say.
func (b *built) blocked(t *testing.T, n int, done chan pipeline.Outcome) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); len(done) == 0 && b.Count(`SELECT count(*) FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid WHERE NOT l.granted AND a.datname = current_database()`) < n; {
		if time.Now().After(deadline) {
			t.Fatalf("%d calls never came to wait for a lock", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Nobody hands out more than they hold
// ---------------------------------------------------------------------------

func TestMemberManagementCannotEscalate(t *testing.T) {
	b := build(t)
	// A TA who may manage members, but who grades only with confirmation,
	// cannot post, and is limited to Yuki.
	helper := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Helper"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": helper, "preset": "ta",
		"perms":         m{"member_manage": "autonomous", "grade_submit": "confirm_required"},
		"student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	puppet := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "Puppet"})).ActorID

	add := func(args m) m {
		args["course_id"], args["actor_id"] = b.course, puppet
		return args
	}
	// The obvious attack: seat a second account as instructor.
	b.try(t, helper, "member.add", add(m{"preset": "instructor"}), apperr.Forbidden)
	// Subtler: a harmless preset, with one permission raised past your own.
	b.try(t, helper, "member.add", add(m{"preset": "observer", "perms": m{"grade_post": "confirm_required"}, "student_scope": "listed"}), apperr.Forbidden)
	b.try(t, helper, "member.add", add(m{"preset": "observer", "perms": m{"grade_submit": "autonomous"}, "student_scope": "listed"}), apperr.Forbidden)
	// Scope is held to the same rule: not the whole class, not Ken.
	b.try(t, helper, "member.add", add(m{"preset": "observer"}), apperr.Forbidden) // observer's scope is 'all'
	b.try(t, helper, "member.add", add(m{"preset": "observer", "student_scope": "listed", "listed_students": []uuid.UUID{b.kenM}}), apperr.Forbidden)

	// Within what they hold, it works.
	seated := testkit.Result[tools.MemberIDOut](t, b.do(t, helper, "member.add", add(m{"preset": "observer",
		"perms": m{"grade_submit": "confirm_required"}, "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}}))).MemberID

	// And raising it afterwards is held to the same rule.
	b.try(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_submit": "autonomous"}}, apperr.Forbidden)
	b.try(t, helper, "member.rescope", m{"course_id": b.course, "member_id": seated, "student_scope": "all"}, apperr.Forbidden)
	b.try(t, helper, "member.rescope", m{"course_id": b.course, "member_id": seated, "listed_students": []uuid.UUID{b.yukiM, b.kenM}}, apperr.Forbidden)
	// Lowering is always allowed, even of something the manager does not hold.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_post": "autonomous"}})
	b.do(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_post": "denied"}})

	// Nobody manages their own seat.
	var helperM uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT id FROM course_member WHERE actor_id = $1`, helper).Scan(&helperM); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"member.pause", "member.remove"} {
		b.try(t, helper, name, m{"course_id": b.course, "member_id": helperM}, apperr.Forbidden)
	}
	b.try(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": helperM, "perms": m{"grade_post": "autonomous"}}, apperr.Forbidden)

	// Typos are refused, not ignored.
	b.try(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_sumbit": "denied"}}, apperr.InvalidArgument)
	b.try(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_submit": "sometimes"}}, apperr.InvalidArgument)
}

// A student's seat reaches that student, so a level on it is a level over
// that student's work: raising one, resuming the seat or seating a new
// student is held to the granter's own list like any other grant. Otherwise
// a manager listed for Yuki could give Ken grade_post over Ken, and Ken
// would post his own grade.
func TestAStudentsSeatReachesTheStudent(t *testing.T) {
	b := build(t)
	register := func(name string) uuid.UUID {
		return testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": name})).ActorID
	}
	helper := register("Helper")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": helper, "preset": "ta",
		"perms":         m{"member_manage": "autonomous", "grade_post": "autonomous", "submission_write": "autonomous"},
		"student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})

	// Ken is not on the helper's list: nothing that widens his seat.
	b.try(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM,
		"perms": m{"grade_submit": "autonomous", "grade_post": "autonomous"}}, apperr.Forbidden)
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": b.kenM})
	b.try(t, helper, "member.resume", m{"course_id": b.course, "member_id": b.kenM}, apperr.Forbidden)
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": b.kenM})
	// Nor a new student, whom no list can have named yet.
	b.try(t, helper, "member.add", m{"course_id": b.course, "actor_id": register("Newbie"), "preset": "student"}, apperr.Forbidden)

	// Narrowing Ken is allowed, as always; taking it back is a grant again.
	b.do(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"grade_read": "denied"}})
	b.try(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"grade_read": "autonomous"}}, apperr.Forbidden)
	// Nor in three steps: take Ken off his own list, which reaches nobody, raise
	// grade_post while it does, and put him back on it.
	b.do(t, helper, "member.rescope", m{"course_id": b.course, "member_id": b.kenM, "listed_students": []uuid.UUID{}})
	b.do(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"grade_post": "autonomous"}})
	b.try(t, helper, "member.rescope", m{"course_id": b.course, "member_id": b.kenM, "listed_students": []uuid.UUID{b.kenM}}, apperr.Forbidden)
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"grade_post": "denied"}})
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.kenM, "listed_students": []uuid.UUID{b.kenM}})
	// Only a student's seat lists itself; any other would reach nobody.
	b.try(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.graderM, "student_scope": "listed",
		"listed_students": []uuid.UUID{b.graderM}}, apperr.FailedPrecondition)
	// Yuki is on the list, and her seat may be raised within the helper's own.
	b.do(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"rubric_read": "autonomous"}})
}

// A decimal given as a string is bounded like a number, and more tightly: as
// a string "1e2000000000" is twelve bytes that the first comparison made with
// it would expand into two billion digits, on the lowest level that can grade
// at all.
func TestADecimalStringIsBounded(t *testing.T) {
	b := build(t)
	work := uuid.New() // refused before anything is looked up
	for _, args := range []m{
		{"score": "1e2000000000"},
		{"score": "1", "breakdown": []m{{"criterion": "Thesis", "points": "1e-2000000000", "max": "10"}}},
	} {
		args["course_id"], args["submission_id"] = b.course, work
		b.try(t, b.grader, "grade.submit", args, apperr.InvalidArgument)
	}
	b.try(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": b.total, "name": "Quiz", "weight": "9e999999"}, apperr.InvalidArgument)
}

// A zero for handing in nothing is a grade of that nothing. Late work takes a
// 'missing' placeholder over, keeping its id, only while no grade is entered
// or proposed for it; a grade proposed before that stays on the placeholder,
// and one waiting on the lock while it happens is refused. Nothing in it
// depends on a clock.
func TestAGradeForNothingDoesNotLandOnLateWork(t *testing.T) {
	b := build(t)
	missing := func(student uuid.UUID) uuid.UUID {
		id := uuid.New()
		b.Exec(`INSERT INTO submission (id, assignment_id, course_id, student_member_id, state) VALUES ($1, $2, $3, $4, 'missing')`,
			id, b.hw3, b.course, student)
		return id
	}
	nothing := func(work uuid.UUID) m {
		return m{"course_id": b.course, "submission_id": work, "score": 0, "feedback": "Nothing handed in."}
	}

	// Proposed for the placeholder; the late work comes in before approval.
	yuki := missing(b.yukiM)
	prop := b.MustCall(b.grader, "grade.submit", nothing(yuki), "zero-for-nothing")
	if prop.Status != domain.StatusProposed {
		t.Fatalf("proposal: %+v", prop)
	}
	late := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "my late essay"}))
	if late.SubmissionID == yuki || late.Attempt != 2 {
		t.Fatalf("the late work took over a placeholder with a grade proposed for it: %+v", late)
	}
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": late.SubmissionID})
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": prop.ActionID, "decision": "approve"})
	if n := b.Count(`SELECT count(*) FROM grade g JOIN submission s ON s.id = g.submission_id
		WHERE g.created_by_action_id = $1 AND s.id = $2 AND s.state = 'missing'`, prop.ActionID, yuki); n != 1 {
		t.Fatal("the zero proposed for nothing is not on the nothing")
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE submission_id = $1`, late.SubmissionID); n != 0 {
		t.Fatal("the zero proposed for nothing landed on the late work")
	}

	// The proposal says what it was given for, so a placeholder taken over
	// all the same — by an instance still on code that did not look for
	// proposals — is refused at approval rather than graded.
	enrol := func(name string) (actor, member uuid.UUID) {
		actor = testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": name})).ActorID
		member = testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": actor, "preset": "student"})).MemberID
		return actor, member
	}
	mia, miaM := enrol("Mia")
	old := missing(miaM)
	prop = b.MustCall(b.grader, "grade.submit", nothing(old), "zero-then-old-takeover")
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND payload->>'for_missing' = 'true'`, prop.ActionID); n != 1 {
		t.Fatal("the proposal does not say it was given for nothing")
	}
	b.Exec(`UPDATE submission SET state = 'draft', body = 'my late essay' WHERE id = $1 AND state = 'missing'`, old)
	b.do(t, mia, "submission.submit", m{"course_id": b.course, "submission_id": old})
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": prop.ActionID, "decision": "approve"})
	if n := b.Count(`SELECT count(*) FROM grade WHERE submission_id = $1`, old); n != 0 {
		t.Fatal("the zero proposed for nothing landed on late work that took the placeholder over")
	}

	// A direct call that waits on the placeholder's lock while it is taken
	// over — reopened for late work, or reopened and handed in already.
	ctx := t.Context()
	waitWhile := func(name, change string, work uuid.UUID) pipeline.Outcome {
		t.Helper()
		tx, err := b.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, change, work); err != nil {
			t.Fatal(err)
		}
		done := make(chan pipeline.Outcome, 1)
		go func() {
			out, _ := b.Call(b.sato, "grade.submit", nothing(work), name)
			done <- out
		}()
		// Until grade.submit has read the placeholder and waits for its lock.
		for deadline := time.Now().Add(10 * time.Second); b.Count(`SELECT count(*) FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid = l.pid WHERE NOT l.granted AND a.datname = current_database()`) == 0; {
			if len(done) > 0 || time.Now().After(deadline) {
				t.Fatal("grade.submit never waited for the placeholder's lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return <-done
	}
	ken := missing(b.kenM)
	if out := waitWhile("zero-while-reopened", `UPDATE submission SET state = 'draft', body = 'draft' WHERE id = $1`, ken); out.Status == domain.StatusExecuted {
		t.Fatalf("a zero for nothing was entered on a reopened draft: %+v", out)
	}
	// A caller may say what its grade is for, and is held to it, and told
	// which way the work is not that; it means nothing for a component, and
	// is refused there.
	_, noorM := enrol("Noor")
	for _, held := range []struct {
		work       uuid.UUID
		score      int
		forMissing bool
		says       string
	}{
		{late.SubmissionID, 0, true, "given for nothing handed in, and there is work here now"},
		{missing(noorM), 50, false, "given for work handed in, and nothing was"},
	} {
		out, err := b.Call(b.sato, "grade.submit", m{"course_id": b.course, "submission_id": held.work, "score": held.score, "for_missing": held.forMissing}, "held-"+uuid.NewString())
		if err != nil || out.Error == nil || out.Error.Code != apperr.FailedPrecondition || !strings.Contains(out.Error.Message, held.says) {
			t.Fatalf("for_missing %v: %+v %v, want %s saying %q", held.forMissing, out.Error, err, apperr.FailedPrecondition, held.says)
		}
	}
	b.try(t, b.sato, "grade.submit", m{"course_id": b.course, "component_id": b.midterm, "student_member_id": b.kenM, "score": 50, "for_missing": false}, apperr.InvalidArgument)

	_, leoM := enrol("Leo")
	leo := missing(leoM)
	if out := waitWhile("zero-while-handed-in", `UPDATE submission SET state = 'submitted', body = 'handed in', submitted_at = now() WHERE id = $1`, leo); out.Status == domain.StatusExecuted {
		t.Fatalf("a zero for nothing was entered on work handed in while it waited: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE submission_id = ANY($1)`, []uuid.UUID{ken, leo}); n != 0 {
		t.Fatalf("%d grades on work that was not there when they were given", n)
	}
	// Work handed in on an instance whose clock runs ahead is graded like any
	// other: nothing is ordered by comparing two instances' clocks.
	b.P.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	b.do(t, b.ken, "submission.submit", m{"course_id": b.course, "submission_id": ken})
	b.P.SetClock(time.Now)
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": ken, "score": 90})
}

// A write takes its caller's seat before anything else it locks, so locking a
// piece of work in Validate cannot close a cycle with a removal (the seat,
// then the proposals it cancels) and an approval (a proposal, then the work).
// The grader's grade.submit is held after Validate by a stand-in for any
// delay there: an uncommitted row with its idempotency key.
func TestAWriteTakesItsSeatFirst(t *testing.T) {
	b := build(t)
	ctx := t.Context()
	work := b.submit(t, b.yuki, "essay")
	lateness := b.MustCall(b.grader, "submission.set_lateness", m{"course_id": b.course, "submission_id": work, "state": "late"}, "p-late")
	if lateness.Status != domain.StatusProposed {
		t.Fatalf("lateness proposal: %+v", lateness)
	}
	conn, err := pgx.Connect(ctx, b.Pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	gate, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `INSERT INTO action (actor_id, action_type, target_type, payload_hash, idempotency_key, authz_result, status)
		VALUES ($1, 'gate', 'gate', repeat('0', 64), 'p-grade', 'denied', 'denied')`, b.grader); err != nil {
		t.Fatal(err)
	}
	waiting := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); b.Count(`SELECT count(*) FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid = l.pid WHERE NOT l.granted AND a.datname = current_database()`) < n; {
			if time.Now().After(deadline) {
				t.Fatalf("fewer than %d calls ever waited", n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	type result struct {
		out pipeline.Outcome
		err error
	}
	done := make(chan result, 3)
	call := func(actor uuid.UUID, name string, args m, key string) {
		go func() {
			out, err := b.Call(actor, name, args, key)
			done <- result{out, err}
		}()
	}
	call(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 80}, "p-grade")
	waiting(1)
	call(b.sato, "action.decide", m{"course_id": b.course, "action_id": lateness.ActionID, "decision": "approve"}, "d-late")
	waiting(2)
	call(b.sato, "member.remove", m{"course_id": b.course, "member_id": b.graderM}, "rm-grader")
	waiting(3)
	if err := gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case r := <-done:
			if r.err != nil || strings.Contains(string(r.out.Result), "collided") {
				t.Fatalf("a call lost a deadlock: %v %s", r.err, r.out.Result)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the calls never finished")
		}
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'executed'`, lateness.ActionID); n != 1 {
		t.Fatal("the approved lateness correction was not carried out")
	}
}

// A call made while its caller's seat is being removed waits for the removal
// and is then refused, rather than leaving behind a proposal that the
// removal, which could not see it yet, never cancelled.
func TestACallDuringItsCallersRemovalWaitsForIt(t *testing.T) {
	b := build(t)
	ctx := t.Context()
	work := b.submit(t, b.yuki, "essay")
	removal, err := b.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer removal.Rollback(ctx)
	// What member.remove does to the seat, up to its commit.
	if _, err := removal.Exec(ctx, `SELECT 1 FROM course_member WHERE id = $1 FOR UPDATE`, b.graderM); err != nil {
		t.Fatal(err)
	}
	if _, err := removal.Exec(ctx, `UPDATE course_member SET status = 'removed' WHERE id = $1`, b.graderM); err != nil {
		t.Fatal(err)
	}
	done := make(chan pipeline.Outcome, 1)
	go func() {
		out, _ := b.Call(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 70}, "during-removal")
		done <- out
	}()
	for deadline := time.Now().Add(10 * time.Second); b.Count(`SELECT count(*) FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid WHERE NOT l.granted AND a.datname = current_database()`) == 0; {
		if len(done) > 0 || time.Now().After(deadline) {
			t.Fatal("the call never waited for its caller's seat")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := removal.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if out := <-done; out.Status != domain.StatusDenied {
		t.Fatalf("a call made during its caller's removal: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE member_id = $1 AND status = 'proposed'`, b.graderM); n != 0 {
		t.Fatalf("%d proposals outlived the seat they were made from", n)
	}
}

// ---------------------------------------------------------------------------
// docs/schema.md §4, "enforced by the application", one by one
// ---------------------------------------------------------------------------

func TestSameCourseRules(t *testing.T) {
	b := build(t)
	// A second course, with a student and an assignment of its own.
	if n := len(testkit.Result[tools.TermListOut](t, b.do(t, b.sato, "term.list", m{})).Terms); n == 0 {
		t.Fatal("any signed-in actor may list terms")
	}
	term, dept := b.term, b.dept
	cs205 := testkit.Result[tools.CourseCreateOut](t, b.do(t, b.admin, "course.create", m{"dept_id": dept, "term_id": term, "code": "CS205", "title": "Algorithms"}))
	b.do(t, b.admin, "course.seat_instructor", m{"course_id": cs205.CourseID, "actor_id": b.sato})
	foreignStudent := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": cs205.CourseID, "actor_id": b.ken, "preset": "student"})).MemberID
	foreignHW := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": cs205.CourseID, "title": "PS1", "points_possible": 10})).ID

	t.Run("scope rows name members and assignments of the same course", func(t *testing.T) {
		someone := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "x"})).ActorID
		b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": someone, "preset": "tutor", "listed_students": []uuid.UUID{foreignStudent}}, apperr.FailedPrecondition)
		b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": someone, "preset": "grader", "listed_assignments": []uuid.UUID{foreignHW}}, apperr.FailedPrecondition)
		// A listed "student" must be a student: listing the instructor is refused.
		b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": someone, "preset": "tutor", "listed_students": []uuid.UUID{b.satoM}}, apperr.FailedPrecondition)
		if n := b.Count(`SELECT count(*) FROM course_member WHERE actor_id = $1`, someone); n != 0 {
			t.Fatal("a refused member.add left a member behind")
		}
	})

	t.Run("an assignment's documents are documents of its course, of the right kind", func(t *testing.T) {
		foreignDoc, material := uuid.New(), uuid.New()
		b.Exec(`INSERT INTO document (id, course_id, kind, title) VALUES ($1, $2, 'rubric', 'theirs')`, foreignDoc, cs205.CourseID)
		b.Exec(`INSERT INTO document (id, course_id, kind, title) VALUES ($1, $2, 'material', 'a lecture')`, material, b.course)
		b.try(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "rubric_document_id": foreignDoc}, apperr.FailedPrecondition)
		b.try(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "rubric_document_id": material}, apperr.FailedPrecondition)
		b.try(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "x", "points_possible": 1, "component_id": cs205.RootComponentID}, apperr.FailedPrecondition)
		b.try(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "x", "points_possible": 1, "component_id": b.midterm}, apperr.FailedPrecondition)
	})

	t.Run("the submitting member is a student", func(t *testing.T) {
		b.try(t, b.sato, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3}, apperr.FailedPrecondition)                                 // the instructor, for himself
		b.try(t, b.sato, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.graderM}, apperr.FailedPrecondition) // for the agent
		b.try(t, b.sato, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": foreignStudent}, apperr.NotFound)
		// A student may not hand in work as another student.
		if out := b.MustCall(b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.kenM}, "as-ken"); out.Status != domain.StatusDenied {
			t.Fatalf("Yuki submitting as Ken: %+v", out)
		}
	})
}

func TestComponentTreeStaysATree(t *testing.T) {
	b := build(t)
	mk := func(parent uuid.UUID, name string) uuid.UUID {
		return testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": parent, "name": name})).ID
	}
	a := mk(b.total, "A")
	bb := mk(a, "B")
	c := mk(bb, "C")

	move := func(id, under uuid.UUID) m {
		return m{"course_id": b.course, "component_id": id, "new_parent_id": under}
	}
	// The database blocks only A under A. The longer cycles are ours.
	b.try(t, b.sato, "component.move", move(a, a), apperr.FailedPrecondition)
	b.try(t, b.sato, "component.move", move(a, bb), apperr.FailedPrecondition)
	b.try(t, b.sato, "component.move", move(a, c), apperr.FailedPrecondition)
	b.try(t, b.sato, "component.move", move(b.total, a), apperr.FailedPrecondition) // the root stays the root
	b.do(t, b.sato, "component.move", move(c, a))                                   // a legal move still works

	// A parent is rolled up: it has no points and holds no assignments.
	b.try(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": b.midterm, "name": "Part 1"}, apperr.FailedPrecondition)
	b.try(t, b.sato, "component.create", m{"course_id": b.course, "parent_id": b.bucket, "name": "Essays"}, apperr.FailedPrecondition)
	b.try(t, b.sato, "component.update", m{"course_id": b.course, "component_id": a, "points_possible": 50}, apperr.FailedPrecondition)
	b.try(t, b.sato, "component.update", m{"course_id": b.course, "component_id": b.total, "points_possible": 100}, apperr.FailedPrecondition)

	tree := testkit.Result[tools.ComponentTreeOut](t, b.do(t, b.yuki, "component.tree", m{"course_id": b.course}))
	if len(tree.Components) != 6 || tree.Components[0].ID != b.total {
		t.Fatalf("tree: %+v", tree.Components)
	}
	seen := map[uuid.UUID]bool{}
	for _, comp := range tree.Components { // parents come before their children
		if comp.ParentID != nil && !seen[*comp.ParentID] {
			t.Fatalf("%s is listed before its parent", comp.Name)
		}
		seen[comp.ID] = true
	}
}

func TestRemovingAMemberCancelsTheirProposals(t *testing.T) {
	b := build(t)
	yukiWork, kenWork := b.submit(t, b.yuki, "essay"), b.submit(t, b.ken, "essay")
	p1 := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": yukiWork, "score": 85}, "p1")
	p2 := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70}, "p2")

	// Pausing leaves the queue alone: the same membership will carry on.
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": b.graderM})
	if n := b.Count(`SELECT count(*) FROM action WHERE status = 'proposed'`); n != 2 {
		t.Fatalf("%d proposals after a pause, want 2 still queued", n)
	}
	if out := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": yukiWork, "score": 1}, "while-paused"); out.Status != domain.StatusDenied {
		t.Fatalf("a paused member calling: %+v", out)
	}
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": b.graderM})

	removed := testkit.Result[tools.MemberRemoveOut](t, b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.graderM}))
	if removed.CancelledProposals != 2 {
		t.Fatalf("cancelled %d proposals, want 2", removed.CancelledProposals)
	}
	for _, p := range []pipeline.Outcome{p1, p2} {
		if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled'
			AND result->'error'->'details'->>'reason' = 'member_removed'`, *p.ActionID); n != 1 {
			t.Fatal("a proposal was not cancelled with its reason")
		}
	}
	if queue := testkit.Result[tools.ActionListOut](t, b.do(t, b.sato, "action.list_proposed", m{"course_id": b.course})); len(queue.Actions) != 0 {
		t.Fatalf("the approval queue still holds %d proposals nobody could approve", len(queue.Actions))
	}
	// History is kept; the seat is gone; seating again is a fresh start.
	if n := b.Count(`SELECT count(*) FROM action WHERE member_id = $1`, b.graderM); n < 2 {
		t.Fatal("the removed member's history was lost")
	}
	again := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": b.grader, "preset": "grader", "listed_assignments": []uuid.UUID{b.hw3}})).MemberID
	if again == b.graderM {
		t.Fatal("re-adding reused the old membership id")
	}
	b.try(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.graderM}, apperr.Conflict)
	b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": b.grader, "preset": "grader"}, apperr.Conflict) // one live seat per actor
}

// ---------------------------------------------------------------------------
// Platform
// ---------------------------------------------------------------------------

func TestPlatformRules(t *testing.T) {
	b := build(t)
	admin := "admin"

	// Platform tools check platform_role and nothing else. An instructor is
	// nobody here, and the refusal is on record with no course attached.
	out := b.MustCall(b.sato, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "mine"}, "sato-registers")
	if out.Status != domain.StatusDenied {
		t.Fatalf("an instructor registering an actor: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND course_id IS NULL AND member_id IS NULL AND status = 'denied'`, *out.ActionID); n != 1 {
		t.Fatal("the platform denial is not recorded as a platform action")
	}
	// And the other way round: an admin is nobody inside a course.
	if out := b.MustCall(b.admin, "member.list", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("an admin with no seat listing members: %+v", out)
	}

	b.try(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Another admin", "platform_role": admin}, apperr.Forbidden) // only root
	b.try(t, b.Root, "actor.register", m{"kind": "human", "display_name": "x", "platform_role": "root"}, apperr.InvalidArgument)
	b.try(t, b.admin, "actor.register", m{"kind": "system", "display_name": "x"}, apperr.InvalidArgument)
	b.try(t, b.admin, "actor.suspend", m{"actor_id": b.admin}, apperr.Forbidden) // not yourself
	b.try(t, b.admin, "actor.suspend", m{"actor_id": b.Root}, apperr.Forbidden)  // not root, if you are not root

	email := "taken@example.edu"
	b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "First", "email": email})
	b.try(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Second", "email": "TAKEN@example.edu"}, apperr.Conflict)

	// Suspension is everywhere at once, and lifts cleanly.
	b.do(t, b.admin, "actor.suspend", m{"actor_id": b.sato})
	if out := b.MustCall(b.sato, "member.list", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied || out.Error.Details["reason"] != "actor_not_active" {
		t.Fatalf("a suspended instructor: %+v", out)
	}
	if out := b.MustCall(b.sato, "me.get", m{}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a suspended actor reading its own account: %+v", out)
	}
	b.try(t, b.admin, "actor.suspend", m{"actor_id": b.sato}, apperr.Conflict)
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": b.sato})
	b.do(t, b.sato, "member.list", m{"course_id": b.course})

	// An agent's first token comes from an admin, once, and works.
	issued := b.do(t, b.admin, "actor.issue_token", m{"actor_id": b.grader, "label": "grader-v2 production"})
	if tok := testkit.Result[tools.IssueTokenOut](t, issued); len(tok.Token) < 40 {
		t.Fatalf("token: %+v", tok)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND (result ? 'token' OR result::text LIKE '%ais_%')`, *issued.ActionID); n != 0 {
		t.Fatal("the issued token is in the action log")
	}

	// Courses.
	term, dept := b.term, b.dept
	b.try(t, b.admin, "course.create", m{"dept_id": dept, "term_id": term, "code": "CS101", "section": "A", "title": "Again"}, apperr.Conflict)
	b.try(t, b.admin, "course.create", m{"dept_id": uuid.New(), "term_id": term, "code": "X", "title": "X"}, apperr.NotFound)
	b.try(t, b.admin, "term.create", m{"name": "Backwards", "starts_on": "2026-12-01", "ends_on": "2026-01-01"}, apperr.InvalidArgument)

	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	if out := b.MustCall(b.sato, "assignment.create", m{"course_id": b.course, "title": "late idea", "points_possible": 1}, "archived"); out.Error.Details["reason"] != "course_archived" {
		t.Fatalf("writing to an archived course: %+v", out)
	}
	b.do(t, b.sato, "assignment.list", m{"course_id": b.course}) // still readable
	// An admin is no more able to write to it than its instructor is: the
	// platform tools are refused for the same reason, and the refusal is on
	// the record like any other.
	for name, args := range map[string]m{
		"course.update":          {"course_id": b.course, "title": "New title"},
		"course.seat_instructor": {"course_id": b.course, "actor_id": b.grader},
	} {
		out := b.MustCall(b.admin, name, args, "archived-"+name)
		if out.Status != domain.StatusDenied || out.Error.Details["reason"] != "course_archived" {
			t.Fatalf("%s on an archived course: %+v", name, out)
		}
	}
	if n := b.Count(`SELECT count(*) FROM course_member WHERE course_id = $1 AND actor_id = $2 AND role = 'instructor'`, b.course, b.grader); n != 0 {
		t.Fatal("an instructor was seated in an archived course")
	}
	// Archiving it again is a conflict, not a denial; un-archiving is the
	// one write it accepts.
	b.try(t, b.admin, "course.archive", m{"course_id": b.course}, apperr.Conflict)
	b.do(t, b.admin, "course.activate", m{"course_id": b.course})

	// Department presets sit beside the built-ins, and win by name.
	body := m{"dept_id": dept, "name": "grader", "role": "assistant", "student_scope": "all", "assignment_scope": "listed",
		"perms": m{"document_read": "autonomous", "submission_read": "autonomous", "grade_submit": "pending_review"}}
	presetID := testkit.Result[tools.IDOut](t, b.do(t, b.admin, "preset.create", body)).ID
	b.try(t, b.admin, "preset.create", body, apperr.Conflict)
	agent := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "grader-v3"})).ActorID
	seated := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": agent, "preset": "grader", "listed_assignments": []uuid.UUID{b.hw3}})).MemberID
	got := testkit.Result[tools.MemberView](t, b.do(t, b.sato, "member.get", m{"course_id": b.course, "member_id": seated}))
	if got.Perms["grade_submit"] != "pending_review" || got.PresetID == nil || *got.PresetID != presetID || len(got.ListedAssignments) != 1 {
		t.Fatalf("the department's preset was not the one applied: %+v", got)
	}
	// Editing the preset changes nobody already seated.
	body["perms"] = m{"document_read": "autonomous"}
	delete(body, "dept_id")
	delete(body, "name")
	body["preset_id"] = presetID
	b.do(t, b.admin, "preset.update", body)
	after := testkit.Result[tools.MemberView](t, b.do(t, b.sato, "member.get", m{"course_id": b.course, "member_id": seated}))
	if after.Perms["grade_submit"] != "pending_review" {
		t.Fatal("editing a preset changed a member who was already seated")
	}
	var builtin uuid.UUID
	for _, p := range testkit.Result[tools.PresetListOut](t, b.do(t, b.sato, "preset.list", m{})).Presets {
		if p.Name == "instructor" {
			builtin = p.ID
		}
	}
	body["preset_id"] = builtin
	b.try(t, b.admin, "preset.update", body, apperr.FailedPrecondition)
}

// An administrator finds everyone registered, corrects a name or an email,
// and invites a person to choose their password.
func TestPeopleAreListedCorrectedAndInvited(t *testing.T) {
	b := build(t)
	list := func(args m) tools.ActorListOut {
		t.Helper()
		return testkit.Result[tools.ActorListOut](t, b.do(t, b.admin, "actor.list", args))
	}
	names := func(out tools.ActorListOut) string {
		var s []string
		for _, a := range out.Actors {
			s = append(s, a.DisplayName)
		}
		return strings.Join(s, ",")
	}

	// Everyone but the system actor, oldest first: root, the admin root
	// made, and the five the admin registered.
	b.Actor("system", "system")
	if got := names(list(m{})); got != "root,Admin,Sato,Yuki,Ken,grader-v2,tutor" {
		t.Fatalf("everyone: %s", got)
	}
	if got := names(list(m{"kind": "agent"})); got != "grader-v2,tutor" {
		t.Fatalf("agents: %s", got)
	}
	if got := names(list(m{"search": "  YUK "})); got != "Yuki" {
		t.Fatalf("a piece of a name, in another case: %s", got)
	}
	if got := names(list(m{"search": "%"})); got != "" { // no wildcards
		t.Fatalf("%% matched: %s", got)
	}
	first := list(m{"limit": 3})
	if names(first) != "root,Admin,Sato" || first.Next == nil {
		t.Fatalf("first page: %s next %v", names(first), first.Next)
	}
	if got := names(list(m{"limit": 3, "after": *first.Next})); got != "Yuki,Ken,grader-v2" {
		t.Fatalf("second page: %s", got)
	}
	b.try(t, b.admin, "actor.list", m{"kind": "system"}, apperr.InvalidArgument)
	b.try(t, b.admin, "actor.list", m{"status": "gone"}, apperr.InvalidArgument)
	if out := b.MustCall(b.sato, "actor.list", m{}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("an instructor listing everyone: %+v", out)
	}

	// Yuki was registered without an email: she cannot be invited until she
	// has one, and then it is what she will sign in with.
	b.try(t, b.admin, "actor.invite", m{"actor_id": b.yuki}, apperr.FailedPrecondition)
	b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Other", "email": "other@example.edu"})
	b.try(t, b.admin, "actor.update", m{"actor_id": b.yuki, "email": "OTHER@example.edu"}, apperr.Conflict)
	b.try(t, b.admin, "actor.update", m{"actor_id": b.yuki, "email": "  "}, apperr.InvalidArgument)
	b.try(t, b.admin, "actor.update", m{"actor_id": b.yuki}, apperr.InvalidArgument)
	yuki := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "email": " yuki@example.edu "}))
	if yuki.Email == nil || *yuki.Email != "yuki@example.edu" || yuki.DisplayName != "Yuki" || yuki.HasPassword || yuki.InviteExpiresAt != nil {
		t.Fatalf("after update: %+v", yuki)
	}
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "email": "Yuki@example.edu"}) // her own, in another case
	// An administrator corrects their own name; root's is root's.
	b.do(t, b.admin, "actor.update", m{"actor_id": b.admin, "display_name": "Admin Office"})
	b.try(t, b.admin, "actor.update", m{"actor_id": b.Root, "display_name": "Not root"}, apperr.Forbidden)

	// The invitation: for a person, never an agent, which holds tokens and
	// never signs in (agents_use_api_tokens).
	b.try(t, b.admin, "actor.invite", m{"actor_id": b.grader}, apperr.Forbidden)
	b.try(t, b.admin, "actor.invite", m{"actor_id": b.admin}, apperr.Forbidden) // not yourself
	b.try(t, b.admin, "actor.invite", m{"actor_id": b.Root}, apperr.Forbidden)  // root is root's
	b.try(t, b.admin, "actor.invite", m{"actor_id": b.yuki, "expires_in_days": 31}, apperr.InvalidArgument)
	invited := b.do(t, b.admin, "actor.invite", m{"actor_id": b.yuki, "expires_in_days": 3})
	inv := testkit.Result[tools.ActorInviteOut](t, invited)
	if !strings.HasPrefix(inv.Token, "aisinv_") || inv.Email == nil || *inv.Email != "Yuki@example.edu" || inv.LoginID != nil ||
		time.Until(inv.ExpiresAt) < 71*time.Hour {
		t.Fatalf("invitation: %+v", inv)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND (result ? 'token' OR result::text LIKE '%aisinv_%')`, *invited.ActionID); n != 0 {
		t.Fatal("the invitation is in the action log")
	}
	got := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": b.yuki}))
	if got.InviteExpiresAt == nil || got.InviteExpiresAt.Sub(inv.ExpiresAt).Abs() > time.Millisecond || got.HasPassword {
		t.Fatalf("actor.get after the invitation: %+v", got)
	}
	waiting := func(want int, when string) {
		t.Helper()
		if n := b.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'invite' AND revoked_at IS NULL`, b.yuki); n != want {
			t.Fatalf("%s: %d live invitations, want %d", when, n, want)
		}
	}
	// Inviting again replaces it: one live invitation.
	b.do(t, b.admin, "actor.invite", m{"actor_id": b.yuki})
	waiting(1, "invited again")
	// A new email withdraws it, since it went to the old one; the same email
	// in another case does not.
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "email": "YUKI@example.edu"})
	waiting(1, "the same email in another case")
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "email": "yuki@example.org"})
	waiting(0, "a new email")
	// So does a password she sets herself: it was for choosing one.
	b.do(t, b.admin, "actor.invite", m{"actor_id": b.yuki})
	b.do(t, b.yuki, "credential.set_password", m{"password": "yukis own password"})
	waiting(0, "a password set otherwise")
	if got := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": b.yuki})); !got.HasPassword || got.InviteExpiresAt != nil {
		t.Fatalf("actor.get after she set a password: %+v", got)
	}
	b.do(t, b.admin, "actor.suspend", m{"actor_id": b.yuki})
	b.try(t, b.admin, "actor.invite", m{"actor_id": b.yuki}, apperr.FailedPrecondition)
}

// ---------------------------------------------------------------------------
// Submissions
// ---------------------------------------------------------------------------

func TestSubmissionLifecycle(t *testing.T) {
	b := build(t)
	now := time.Now()
	due := now.Add(24 * time.Hour)
	// Instructions, with a published version to pin.
	doc, v1, v2 := uuid.New(), uuid.New(), uuid.New()
	b.Exec(`INSERT INTO document (id, course_id, kind, title) VALUES ($1, $2, 'instructions', 'HW3')`, doc, b.course)
	b.Exec(`INSERT INTO document_version (id, document_id, seq, body_md, author_member_id) VALUES ($1, $2, 1, '1000 words', $3)`, v1, doc, b.satoM)
	b.Exec(`UPDATE document SET published_version_id = $1 WHERE id = $2`, v1, doc)
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": doc, "due_at": due})

	created := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3}))
	if created.Attempt != 1 {
		t.Fatalf("first attempt is %d", created.Attempt)
	}
	sub := m{"course_id": b.course, "submission_id": created.SubmissionID}
	b.try(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3}, apperr.Conflict) // one open draft
	b.try(t, b.yuki, "submission.submit", sub, apperr.FailedPrecondition)                                    // nothing to hand in
	b.do(t, b.yuki, "submission.update_draft", m{"course_id": b.course, "submission_id": created.SubmissionID, "body": "draft one"})
	b.do(t, b.yuki, "submission.update_draft", m{"course_id": b.course, "submission_id": created.SubmissionID, "body": "my essay"})

	if got := testkit.Result[tools.SubmissionSubmitOut](t, b.do(t, b.yuki, "submission.submit", sub)); got.State != "submitted" {
		t.Fatalf("on time: %+v", got)
	}
	// Frozen. By us, and by the database underneath us.
	b.try(t, b.yuki, "submission.update_draft", m{"course_id": b.course, "submission_id": created.SubmissionID, "body": "second thoughts"}, apperr.Conflict)
	b.try(t, b.yuki, "submission.submit", sub, apperr.Conflict)
	if _, err := b.Pool.Exec(t.Context(), `UPDATE submission SET body = 'tampered' WHERE id = $1`, created.SubmissionID); err == nil {
		t.Fatal("the database let a submitted row change")
	}
	// What the student was told is pinned, and stays pinned when the
	// instructions move on.
	b.Exec(`INSERT INTO document_version (id, document_id, seq, body_md, author_member_id) VALUES ($1, $2, 2, '2000 words', $3)`, v2, doc, b.satoM)
	b.Exec(`UPDATE document SET published_version_id = $1 WHERE id = $2`, v2, doc)
	got := testkit.Result[tools.SubmissionView](t, b.do(t, b.yuki, "submission.get", sub))
	if got.InstructionsVersionID == nil || *got.InstructionsVersionID != v1 || got.Body == nil || *got.Body != "my essay" {
		t.Fatalf("submitted row: %+v", got)
	}

	// Lateness is for graders to correct, not for the student.
	late := m{"course_id": b.course, "submission_id": created.SubmissionID, "state": "late"}
	if out := b.MustCall(b.yuki, "submission.set_lateness", late, "self-serve"); out.Status != domain.StatusDenied {
		t.Fatalf("a student changing their own lateness: %+v", out)
	}
	b.do(t, b.sato, "submission.set_lateness", late)
	b.try(t, b.sato, "submission.set_lateness", late, apperr.Conflict) // already late
	late["state"] = "draft"
	b.try(t, b.sato, "submission.set_lateness", late, apperr.InvalidArgument)

	// Resubmitting is a new attempt; after the due date it is late, and it
	// pins the instructions as they stand now.
	b.P.SetClock(func() time.Time { return due.Add(time.Hour) })
	second := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "revised"}))
	if second.Attempt != 2 {
		t.Fatalf("second attempt is %d", second.Attempt)
	}
	if got := testkit.Result[tools.SubmissionSubmitOut](t, b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": second.SubmissionID})); got.State != "late" {
		t.Fatalf("after the due date: %+v", got)
	}
	if n := b.Count(`SELECT count(*) FROM submission WHERE id = $1 AND instructions_version_id = $2`, second.SubmissionID, v2); n != 1 {
		t.Fatal("the second attempt did not pin the instructions in force when it was submitted")
	}

	// A 'missing' placeholder is taken over by late work, not sat beside.
	placeholder := uuid.New()
	b.Exec(`INSERT INTO submission (id, assignment_id, course_id, student_member_id, state) VALUES ($1, $2, $3, $4, 'missing')`,
		placeholder, b.hw3, b.course, b.kenM)
	reused := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "sorry"}))
	if reused.SubmissionID != placeholder || reused.Attempt != 1 {
		t.Fatalf("late work did not take over the missing row: %+v", reused)
	}
}

// A hand-in that waits for approval counts from when it was asked for. Yuki
// asked before the due date, under the instructions then published; the
// approver's delay does not make her late, nor pin instructions published
// afterwards. And what is handed in is what was asked for: Ken, who also
// asked in time, cannot go on rewriting his draft after the due date and
// have that handed in as of before it.
func TestAProposedHandInCountsFromWhenItWasAsked(t *testing.T) {
	b := build(t)
	base := time.Now()
	later := func(d time.Duration) { b.P.SetClock(func() time.Time { return base.Add(d) }) }
	brief := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW3", "body_md": "v1: 1000 words"}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": brief.DocumentID})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3,
		"instructions_document_id": brief.DocumentID, "due_at": base.Add(time.Hour)})
	// Each writes a draft, and then may only ask for it to be handed in.
	handIn := func(student, member uuid.UUID) (uuid.UUID, *uuid.UUID) {
		t.Helper()
		work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, student, "submission.create",
			m{"course_id": b.course, "assignment_id": b.hw3, "body": "my essay"})).SubmissionID
		b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": member, "perms": m{"submission_write": "confirm_required"}})
		out := b.MustCall(student, "submission.submit", m{"course_id": b.course, "submission_id": work}, "hand-in-"+work.String())
		if out.Status != domain.StatusProposed {
			t.Fatalf("%+v", out)
		}
		return work, out.ActionID
	}
	approve := func(action *uuid.UUID) pipeline.DecideOut {
		t.Helper()
		return testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": action, "decision": "approve"}))
	}
	yukis, yukisAsk := handIn(b.yuki, b.yukiM)
	kens, kensAsk := handIn(b.ken, b.kenM)

	later(2 * time.Hour)
	b.do(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": brief.DocumentID, "body_md": "v2: 3000 words", "publish": true})
	later(3 * time.Hour)
	if v := approve(yukisAsk); v.Outcome != domain.StatusExecuted {
		t.Fatalf("%+v", v)
	}
	if n := b.Count(`SELECT count(*) FROM submission s JOIN action a ON a.id = $2
		WHERE s.id = $1 AND s.state = 'submitted' AND s.submitted_at = a.created_at AND s.instructions_version_id = $3`,
		yukis, *yukisAsk, *brief.VersionID); n != 1 {
		t.Fatal("the hand-in was judged, dated or pinned as of its approval, not as of when it was asked for")
	}

	b.do(t, b.sato, "submission.update_draft", m{"course_id": b.course, "submission_id": kens, "body": "rewritten after the due date"})
	if v := approve(kensAsk); v.Outcome != domain.StatusFailed || v.Error == nil || v.Error.Code != apperr.FailedPrecondition {
		t.Fatalf("approving a hand-in of a draft that has changed since: %+v", v)
	}
	if n := b.Count(`SELECT count(*) FROM submission WHERE id = $1 AND state = 'draft'`, kens); n != 1 {
		t.Fatal("a draft changed after it was asked to be handed in was handed in")
	}
	// A direct call may say what it hands in, and is held to it.
	b.try(t, b.sato, "submission.submit", m{"course_id": b.course, "submission_id": kens, "body": "my essay"}, apperr.FailedPrecondition)
	b.try(t, b.sato, "submission.submit", m{"course_id": b.course, "submission_id": kens, "instructions_version_id": brief.VersionID}, apperr.FailedPrecondition)
	if got := testkit.Result[tools.SubmissionSubmitOut](t, b.do(t, b.sato, "submission.submit",
		m{"course_id": b.course, "submission_id": kens, "body": "rewritten after the due date", "files": []uuid.UUID{}})); got.State != "late" {
		t.Fatalf("handed in now, after the due date: %+v", got)
	}
}

// The instances of a server do not share a clock. Yuki asks to hand in on
// one whose clock runs a few minutes ahead, and Sato approves on one whose
// clock is behind, so that by the approver's clock she has not asked yet.
// It is still an approval, not a direct call: it is dated when she asked and
// handed in under the instructions she was reading, not held to those Sato
// published in between.
func TestAHandInApprovedOnASlowerClockIsStillAnApproval(t *testing.T) {
	b := build(t)
	base := time.Now()
	clock := func(d time.Duration) { b.P.SetClock(func() time.Time { return base.Add(d) }) }
	brief := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW3", "body_md": "v1: 1000 words"}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": brief.DocumentID})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": brief.DocumentID})
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "my essay"})).SubmissionID
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"submission_write": "confirm_required"}})

	clock(3 * time.Minute)
	asked := b.MustCall(b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": work}, "hand-in")
	if asked.Status != domain.StatusProposed {
		t.Fatalf("%+v", asked)
	}
	clock(time.Minute)
	b.do(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": brief.DocumentID, "body_md": "v2: 3000 words", "publish": true})
	v := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": asked.ActionID, "decision": "approve"}))
	if v.Outcome != domain.StatusExecuted {
		t.Fatalf("approving on a clock behind the proposer's: %+v", v)
	}
	if n := b.Count(`SELECT count(*) FROM submission s JOIN action a ON a.id = $2
		WHERE s.id = $1 AND s.submitted_at = a.created_at AND s.instructions_version_id = $3`,
		work, *asked.ActionID, *brief.VersionID); n != 1 {
		t.Fatal("the hand-in was carried out as a direct call at the approval, not as of when it was asked for")
	}
}

// A hand-in that waits for approval is of the draft as it was when asked
// for. Yuki, who may only propose, asks for an edit to her draft and then for
// it to be handed in, and Sato approves them in the order they came. The edit
// goes through; the hand-in is then refused, plainly, since the draft is no
// longer what was asked to be handed in, and nothing is handed in. Asked for
// again once the edit has been decided, it hands in the edited draft.
func TestAHandInQueuedBehindAnEditIsOfTheDraftBeforeIt(t *testing.T) {
	b := build(t)
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "draft v1"})).SubmissionID
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"submission_write": "confirm_required"}})
	ask := func(name string, args m, key string) *uuid.UUID {
		t.Helper()
		out := b.MustCall(b.yuki, name, args, key)
		if out.Status != domain.StatusProposed {
			t.Fatalf("%s: %+v", name, out)
		}
		return out.ActionID
	}
	approve := func(action *uuid.UUID) pipeline.DecideOut {
		t.Helper()
		return testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": action, "decision": "approve"}))
	}
	stored := func() (state, body string) {
		t.Helper()
		if err := b.Pool.QueryRow(t.Context(), `SELECT state, coalesce(body, '') FROM submission WHERE id = $1`, work).Scan(&state, &body); err != nil {
			t.Fatal(err)
		}
		return state, body
	}
	handIn := m{"course_id": b.course, "submission_id": work}

	edit := ask("submission.update_draft", m{"course_id": b.course, "submission_id": work, "body": "final"}, "edit")
	asked := ask("submission.submit", handIn, "hand-in")
	if v := approve(edit); v.Outcome != domain.StatusExecuted {
		t.Fatalf("approving the edit: %+v", v)
	}
	if v := approve(asked); v.Outcome != domain.StatusFailed || v.Error == nil || v.Error.Code != apperr.FailedPrecondition ||
		!strings.Contains(v.Error.Message, "the draft does not hold what this call says it hands in") {
		t.Fatalf("approving the hand-in asked for before the edit: %+v", v)
	}
	if state, body := stored(); state != "draft" || body != "final" {
		t.Fatalf("after the refused hand-in the submission is %s with %q, want the edited draft", state, body)
	}

	if v := approve(ask("submission.submit", handIn, "hand-in-again")); v.Outcome != domain.StatusExecuted {
		t.Fatalf("approving the hand-in asked for after the edit: %+v", v)
	}
	if state, body := stored(); state != "submitted" || body != "final" {
		t.Fatalf("the submission is %s with %q, want the edited draft handed in", state, body)
	}
}

func TestAStudentWithAnEmptiedScopeReachesNobody(t *testing.T) {
	b := build(t)
	b.Exec(`DELETE FROM member_student_scope WHERE member_id = $1`, b.yukiM)
	b.try(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "x"}, apperr.Forbidden)
	if out := b.MustCall(b.yuki, "gradebook.get", m{"course_id": b.course, "student_member_id": b.yukiM}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("scope did not fail closed: %+v", out)
	}
}

// A 'missing' row that has been graded is history: the zero, and the reason
// given for it, describe a submission with nothing in it. Late work goes
// beside it as a new attempt rather than appearing underneath the grade.
func TestAGradedMissingRowIsNotTakenOver(t *testing.T) {
	b := build(t)
	placeholder := uuid.New()
	b.Exec(`INSERT INTO submission (id, assignment_id, course_id, student_member_id, state) VALUES ($1, $2, $3, $4, 'missing')`,
		placeholder, b.hw3, b.course, b.yukiM)
	zero := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit",
		m{"course_id": b.course, "submission_id": placeholder, "score": 0, "feedback": "Nothing handed in."})).GradeID

	// Graded is enough; it need not have been posted yet.
	late := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "my late essay"}))
	if late.SubmissionID == placeholder || late.Attempt != 2 {
		t.Fatalf("late work went underneath an existing grade: %+v", late)
	}
	if n := b.Count(`SELECT count(*) FROM submission WHERE id = $1 AND state = 'missing' AND body IS NULL`, placeholder); n != 1 {
		t.Fatal("the graded placeholder changed")
	}
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{zero}})
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": late.SubmissionID})
	// The late attempt is graded in its own right, and is then what counts.
	better := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": late.SubmissionID, "score": 60})).GradeID
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{better}})
	book := testkit.Result[tools.GradebookGetOut](t, b.do(t, b.yuki, "gradebook.get", m{"course_id": b.course, "student_member_id": b.yukiM}))
	for _, c := range book.Components {
		if c.ComponentID == b.bucket && (c.Percent == nil || !c.Percent.Equal(decimal.NewFromInt(60))) {
			t.Fatalf("Assignments is %v, want the later attempt's 60", c.Percent)
		}
	}
}

// The checks that keep a component one thing — a bucket of assignments, a
// parent of other components, or something graded directly — read the tree
// and then write to it. Two of them at once, each passing on what it read,
// could leave a component being two things; its assignments would then
// silently stop counting. They are serialised by the course's tree lock.
func TestAComponentStaysOneThingUnderConcurrency(t *testing.T) {
	b := build(t)
	for round := range 8 {
		target := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "component.create",
			m{"course_id": b.course, "parent_id": b.total, "name": "Coursework " + strconv.Itoa(round), "weight": 1})).ID
		calls := []struct {
			name string
			args m
		}{
			{"component.create", m{"course_id": b.course, "parent_id": target, "name": "Labs"}},
			{"assignment.create", m{"course_id": b.course, "title": "HW", "points_possible": 10, "component_id": target}},
			{"component.update", m{"course_id": b.course, "component_id": target, "points_possible": 50}},
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, c := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := b.Call(b.sato, c.name, c.args, "race-"+strconv.Itoa(round)+"-"+strconv.Itoa(i)); err != nil {
					t.Errorf("%s: %v", c.name, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if n := b.Count(`SELECT (EXISTS (SELECT 1 FROM grade_component WHERE parent_id = $1))::int
			+ (EXISTS (SELECT 1 FROM assignment WHERE component_id = $1))::int
			+ (SELECT (points_possible IS NOT NULL)::int FROM grade_component WHERE id = $1)`, target); n != 1 {
			t.Fatalf("round %d: the component is %d things at once", round, n)
		}
	}
}

// assignment.publish insists on instructions students can read. Changing a
// published assignment's instructions must insist on the same, or everyone
// who hands in afterwards is pinned to nothing.
func TestAPublishedAssignmentKeepsReadableInstructions(t *testing.T) {
	b := build(t)
	unpublished := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW3, rewritten", "body_md": "tbd"})).DocumentID
	change := m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": unpublished}
	b.try(t, b.sato, "assignment.update", change, apperr.FailedPrecondition)
	if n := b.Count(`SELECT count(*) FROM assignment WHERE id = $1 AND instructions_document_id IS NULL`, b.hw3); n != 1 {
		t.Fatal("the refused change was kept")
	}
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": unpublished})
	b.do(t, b.sato, "assignment.update", change)

	// An assignment nobody can see yet may point at a draft brief.
	draftBrief := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW4", "body_md": "tbd"})).DocumentID
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW4", "points_possible": 10})).ID
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": hw4, "instructions_document_id": draftBrief})
}

// Changing an assignment reads it, checks it and writes all of it back. Two
// changes at once take turns on its row, or the second would write back the
// copy it read before the first committed: a rename and a new due date made
// together would leave one of them undone, both reported done. Publishing
// takes the same turn, so that it and a change of instructions cannot each
// pass on what the other has not done yet.
func TestChangesToAnAssignmentTakeTurns(t *testing.T) {
	b := build(t)
	const lockRow = `SELECT 1 FROM assignment WHERE id = $1 FOR UPDATE`
	done := make(chan pipeline.Outcome, 2)

	due := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	release := b.hold(t, lockRow, b.hw3)
	for i, change := range []m{{"title": "HW3 (revised)"}, {"due_at": due}} {
		change["course_id"], change["assignment_id"] = b.course, b.hw3
		b.start(t, done, b.sato, "assignment.update", change)
		b.blocked(t, i+1, done)
	}
	release()
	for range 2 {
		if out := <-done; out.Status != domain.StatusExecuted {
			t.Fatalf("assignment.update: %+v", out)
		}
	}
	if n := b.Count(`SELECT count(*) FROM assignment WHERE id = $1 AND title = 'HW3 (revised)' AND due_at = $2`, b.hw3, due); n != 1 {
		t.Fatal("of two changes made at once, one was undone by the other")
	}

	// HW4 is published while its instructions are pointed at a brief
	// nobody can read yet.
	brief := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW4", "body_md": "Write 1000 words."})).DocumentID
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": brief})
	rewrite := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW4, rewritten", "body_md": "tbd"})).DocumentID
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create",
		m{"course_id": b.course, "title": "HW4", "points_possible": 10, "instructions_document_id": brief})).ID
	release = b.hold(t, lockRow, hw4)
	b.start(t, done, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": hw4, "instructions_document_id": rewrite})
	b.blocked(t, 1, done)
	b.start(t, done, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw4})
	b.blocked(t, 2, done)
	release()
	executed := 0
	for range 2 {
		if out := <-done; out.Status == domain.StatusExecuted {
			executed++
		}
	}
	if n := b.Count(`SELECT count(*) FROM assignment a JOIN document d ON d.id = a.instructions_document_id
		WHERE a.id = $1 AND a.published_at IS NOT NULL AND d.published_version_id IS NULL`, hw4); n != 0 {
		t.Fatal("the assignment was published with instructions students cannot read")
	}
	if executed != 1 {
		t.Fatalf("%d of the two went through; whichever came second should have been refused", executed)
	}
}

// "An archived course refuses every write" includes the writes that touch no
// row of ours: an upload URL is somewhere to put a file for a course that is
// closed.
func TestAnArchivedCourseIssuesNoUploadURLs(t *testing.T) {
	b := build(t)
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	out, err := b.Call(b.sato, "document.upload_url", m{"course_id": b.course, "kind": "material", "content_type": "text/plain"}, "closed")
	if err == nil && (out.Status == domain.StatusExecuted || out.Status == domain.StatusProposed) {
		t.Fatalf("an archived course handed out an upload URL: %+v", out)
	}
}

// A level is held over a scope for a time. Raising a level on a member who
// reaches the whole class, widening the reach of a member who holds a level,
// or extending the life of either, hands out the product — and the product is
// what must be within the granter's own.
func TestAWideningChangeIsAGrantOfTheWhole(t *testing.T) {
	b := build(t)
	register := func(kind, name string) uuid.UUID {
		args := m{"kind": kind, "display_name": name}
		if kind == "agent" {
			args["hosting"] = "mcp"
		}
		return testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", args)).ActorID
	}
	seat := func(as uuid.UUID, actor uuid.UUID, args m) uuid.UUID {
		args["course_id"], args["actor_id"] = b.course, actor
		return testkit.Result[tools.MemberIDOut](t, b.do(t, as, "member.add", args)).MemberID
	}
	denied := func(what string, actor uuid.UUID, name string, args m) {
		t.Helper()
		args["course_id"] = b.course
		if out := b.MustCall(actor, name, args, "denied-"+uuid.NewString()); out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden {
			t.Fatalf("%s: %+v", what, out)
		}
	}
	// Helper: a TA who manages members, grades on their own, cannot post,
	// and is limited to Yuki. Sato also pilots an agent, limited to Yuki.
	helper := register("human", "Helper")
	seat(b.sato, helper, m{"preset": "ta", "perms": m{"member_manage": "autonomous"}, "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	pilot := seat(b.sato, register("agent", "pilot"), m{"preset": "ta", "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})

	// (1) Widening a scope hands the member's levels to more students.
	denied("widening the pilot to the class", helper, "member.rescope", m{"member_id": pilot, "student_scope": "all"})
	denied("adding Ken to the pilot", helper, "member.rescope", m{"member_id": pilot, "listed_students": []uuid.UUID{b.yukiM, b.kenM}})
	// (2) Raising a level on a member whose reach is the class.
	observer := seat(b.sato, register("agent", "observer"), m{"preset": "observer"})
	denied("raising a level on someone who reaches the class", helper, "member.update_perms", m{"member_id": observer, "perms": m{"submission_read": "autonomous"}})
	denied("raising an agent's grading to autonomous for the class", helper, "member.update_perms", m{"member_id": b.graderM, "perms": m{"grade_submit": "autonomous"}})
	// (3) Two steps: give a friend member_manage, have the friend widen you.
	friend := register("human", "Friend")
	friendM := seat(b.sato, friend, m{"preset": "observer", "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	b.do(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": friendM, "perms": m{"member_manage": "autonomous"}})
	var helperM uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT id FROM course_member WHERE actor_id = $1`, helper).Scan(&helperM); err != nil {
		t.Fatal(err)
	}
	if out := b.MustCall(friend, "member.rescope", m{"course_id": b.course, "member_id": helperM, "student_scope": "all"}, "friend"); out.Status != domain.StatusFailed {
		t.Fatalf("the friend widened the helper: %+v", out)
	}
	if out := b.MustCall(helper, "submission.list", m{"course_id": b.course, "assignment_id": b.hw3}, ""); out.Status != domain.StatusExecuted {
		t.Fatalf("%+v", out)
	} else if n := len(testkit.Result[tools.SubmissionListOut](t, out).Submissions); n != 0 {
		t.Fatalf("the helper reaches %d submissions it was never given", n)
	}

	// (4) Narrowing is always allowed, whatever the manager holds: the
	// grader reaches the class, the helper does not, and may still narrow it
	// or shorten its life.
	b.do(t, helper, "member.rescope", m{"course_id": b.course, "member_id": b.graderM, "expires_at": time.Now().Add(time.Hour)})
	b.do(t, helper, "member.rescope", m{"course_id": b.course, "member_id": b.graderM, "assignment_scope": "listed", "listed_assignments": []uuid.UUID{}})
	b.do(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": b.graderM, "perms": m{"grade_submit": "denied"}})
	// But not lengthen it, nor clear it: that gives back what it held.
	denied("extending the grader's life", helper, "member.rescope", m{"member_id": b.graderM, "expires_at": time.Now().Add(48 * time.Hour)})
	denied("making the grader permanent", helper, "member.rescope", m{"member_id": b.graderM, "clear_expiry": true})

	// (5) Nor resume a seat that holds more: pausing and resuming would
	// otherwise be a way to hand out anything at all.
	b.do(t, helper, "member.pause", m{"course_id": b.course, "member_id": observer})
	denied("resuming a seat that reaches the class", helper, "member.resume", m{"member_id": observer})
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": observer})
}

// Time is held to the same rule as everything else: a manager seated until
// Friday hands out nothing that lasts past Friday, and cannot be made to last
// longer by someone they seated.
func TestATemporaryManagerHandsOutNothingPermanent(t *testing.T) {
	b := build(t)
	friday := time.Now().Add(72 * time.Hour)
	temp := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Temp"})).ActorID
	tempM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": temp, "preset": "ta",
		"perms": m{"member_manage": "autonomous"}, "expires_at": friday})).MemberID
	puppet := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "Puppet"})).ActorID

	seat := m{"course_id": b.course, "actor_id": puppet, "preset": "ta", "perms": m{"member_manage": "autonomous"}}
	b.try(t, temp, "member.add", seat, apperr.Forbidden) // no expiry: for ever
	seat["expires_at"] = friday.Add(time.Hour)
	b.try(t, temp, "member.add", seat, apperr.Forbidden)
	seat["expires_at"] = friday
	puppetM := testkit.Result[tools.MemberIDOut](t, b.do(t, temp, "member.add", seat)).MemberID

	// The puppet cannot make its maker permanent, nor itself.
	b.try(t, puppet, "member.rescope", m{"course_id": b.course, "member_id": tempM, "clear_expiry": true}, apperr.Forbidden)
	b.try(t, puppet, "member.rescope", m{"course_id": b.course, "member_id": tempM, "expires_at": friday.Add(time.Hour)}, apperr.Forbidden)
	b.try(t, puppet, "member.rescope", m{"course_id": b.course, "member_id": puppetM, "clear_expiry": true}, apperr.Forbidden)
	// Sato, who lasts, can.
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": tempM, "clear_expiry": true})

	// An expired seat is as good as removed, whether or not the sweep has
	// got to it: it is not revived, and a fresh seat takes its place.
	b.P.SetClock(func() time.Time { return friday.Add(time.Minute) })
	b.try(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": puppetM, "clear_expiry": true}, apperr.Conflict)
	b.try(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": puppetM}, apperr.Conflict)
	b.try(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": puppetM, "perms": m{"grade_post": "denied"}}, apperr.Conflict)
	again := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": puppet, "preset": "observer"})).MemberID
	if again == puppetM {
		t.Fatal("the expired seat was reused")
	}
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND status = 'removed'`, puppetM); n != 1 {
		t.Fatal("the expired seat was not removed when the fresh one was made")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'member.removed' AND subject_id = $1 AND payload->>'reason' = 'expired'`, puppetM); n != 1 {
		t.Fatal("no member.removed event for the expired seat")
	}
}

// An expiry is measured as the database will keep it: to the microsecond. A
// seat given until the very moment its granter's ends does not outlast it by
// the nanoseconds a finer clock sent and the database dropped, and giving the
// same moment again is no extension.
func TestAnExpiryIsMeasuredAsTheDatabaseKeepsIt(t *testing.T) {
	b := build(t)
	// A moment the database cannot hold exactly, whatever this machine's clock.
	friday := time.Now().Add(72 * time.Hour).Truncate(time.Microsecond).Add(789 * time.Nanosecond)
	temp := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Temp"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": temp, "preset": "ta",
		"perms": m{"member_manage": "autonomous"}, "expires_at": friday})
	puppet := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "Puppet"})).ActorID

	puppetM := testkit.Result[tools.MemberIDOut](t, b.do(t, temp, "member.add", m{"course_id": b.course, "actor_id": puppet, "preset": "ta",
		"perms": m{"member_manage": "autonomous"}, "expires_at": friday})).MemberID
	b.do(t, temp, "member.rescope", m{"course_id": b.course, "member_id": puppetM, "expires_at": friday})
	// A microsecond more is longer than the granter's own.
	b.try(t, temp, "member.rescope", m{"course_id": b.course, "member_id": puppetM, "expires_at": friday.Add(time.Microsecond)}, apperr.Forbidden)
}

// Two managers editing the same seat at once take turns, so that a
// revocation that reports executed is not undone by a change that never
// meant to touch it.
func TestConcurrentEditsToASeatTakeTurns(t *testing.T) {
	b := build(t)
	for round := range 6 {
		actor := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "a" + strconv.Itoa(round)})).ActorID
		seated := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": actor, "preset": "ta",
			"perms": m{"grade_post": "autonomous"}, "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM, b.kenM}})).MemberID
		calls := []struct {
			name string
			args m
		}{
			{"member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"grade_post": "denied"}}},
			{"member.update_perms", m{"course_id": b.course, "member_id": seated, "perms": m{"member_read": "autonomous"}}},
			{"member.rescope", m{"course_id": b.course, "member_id": seated, "listed_students": []uuid.UUID{b.yukiM}}},
			{"member.rescope", m{"course_id": b.course, "member_id": seated, "assignment_scope": "listed", "listed_assignments": []uuid.UUID{b.hw3}}},
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, c := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if out, err := b.Call(b.sato, c.name, c.args, "turns-"+strconv.Itoa(round)+"-"+strconv.Itoa(i)); err != nil || out.Status != domain.StatusExecuted {
					t.Errorf("%s: %v %+v", c.name, err, out)
				}
			}()
		}
		close(start)
		wg.Wait()
		got := testkit.Result[tools.MemberView](t, b.do(t, b.sato, "member.get", m{"course_id": b.course, "member_id": seated}))
		if got.Perms["grade_post"] != "denied" || got.Perms["member_read"] != "autonomous" ||
			len(got.ListedStudents) != 1 || got.AssignmentScope != "listed" || len(got.ListedAssignments) != 1 {
			t.Fatalf("round %d: an edit was lost: %+v", round, got)
		}
	}
}

// A change that leaves a list alone does not re-check it: a student who has
// since left the course must not make an unrelated change fail.
func TestAKeptListIsNotReValidated(t *testing.T) {
	b := build(t)
	b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.yukiM})
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.tutorM, "expires_at": time.Now().Add(time.Hour)})
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.tutorM, "assignment_scope": "listed", "listed_assignments": []uuid.UUID{b.hw3}})
	// Sending the list does check it.
	b.try(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.tutorM, "listed_students": []uuid.UUID{b.yukiM}}, apperr.FailedPrecondition)
}

// ---------------------------------------------------------------------------
// Lock order: calls held at a chosen point, and what may happen meanwhile
// ---------------------------------------------------------------------------

type callResult struct {
	out pipeline.Outcome
	err error
}

// inFlight starts a call and returns where its result will arrive.
func (b *built) inFlight(actor uuid.UUID, name string, args m, key string) <-chan callResult {
	done := make(chan callResult, 1)
	go func() {
		out, err := b.Call(actor, name, args, key)
		done <- callResult{out, err}
	}()
	return done
}

// heldBy runs sql in a transaction on a connection of its own, which keeps
// its locks until release is called.
func heldBy(t *testing.T, b *built, sql string, args ...any) (release func()) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, b.Pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Commit(ctx); _ = conn.Close(ctx) }) }
	t.Cleanup(release)
	return release
}

// waitingFor waits until n lock waits are held up in the test's database; a
// call in back that has already returned means it did not wait.
func (b *built) waitingFor(t *testing.T, n int, back ...<-chan callResult) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); b.Count(`SELECT count(*) FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid WHERE NOT l.granted AND a.datname = current_database()`) < n; {
		for _, c := range back {
			if len(c) > 0 {
				r := <-c
				t.Fatalf("a call came back instead of waiting: %v %+v", r.err, r.out)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d calls ever waited", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settled fails if the call lost a deadlock, and returns what it came back with.
func settled(t *testing.T, c <-chan callResult) pipeline.Outcome {
	t.Helper()
	select {
	case r := <-c:
		if r.err != nil || strings.Contains(string(r.out.Result), "collided") || (r.out.Error != nil && strings.Contains(r.out.Error.Message, "collided")) {
			t.Fatalf("a call lost a deadlock: %v %+v", r.err, r.out)
		}
		return r.out
	case <-time.After(30 * time.Second):
		t.Fatal("a call never came back")
	}
	return pipeline.Outcome{}
}

// A retry of a call still in flight waits for it holding nothing, then
// replays it. Here the call is an approval of a change to the approver's own
// seat, which locks that seat FOR UPDATE; a retry holding the seat shared
// while it waited for the first call's key would deadlock with it.
func TestARetryWaitsForItsCallHoldingNothing(t *testing.T) {
	b := build(t)
	helper := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "Helper"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": helper, "preset": "ta", "perms": m{"member_manage": "confirm_required"}})
	prop := b.MustCall(helper, "member.update_perms", m{"course_id": b.course, "member_id": b.satoM, "perms": m{"grade_post": "confirm_required"}}, "narrow-sato")
	if prop.Status != domain.StatusProposed {
		t.Fatalf("proposal: %+v", prop)
	}
	release := heldBy(t, b, `SELECT 1 FROM action WHERE id = $1 FOR UPDATE`, prop.ActionID)
	approve := m{"course_id": b.course, "action_id": prop.ActionID, "decision": "approve"}
	first := b.inFlight(b.sato, "action.decide", approve, "approve")
	b.waitingFor(t, 1, first)
	retry := b.inFlight(b.sato, "action.decide", approve, "approve")
	b.waitingFor(t, 2, first, retry)
	release()
	if out := settled(t, first); out.Status != domain.StatusExecuted || out.Replayed {
		t.Fatalf("the approval: %+v", out)
	}
	if out := settled(t, retry); !out.Replayed {
		t.Fatalf("the retry was not a replay: %+v", out)
	}
}

// Pausing a member waits for an approval of their proposal that is under way,
// and an approval waits for a pause under way and sees it: the proposer's
// seat is taken before the proposal, the order a change to the seat takes.
func TestAnApprovalHoldsTheProposersSeat(t *testing.T) {
	b := build(t)
	work := b.submit(t, b.yuki, "essay")
	prop := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 70}, "p")
	if prop.Status != domain.StatusProposed {
		t.Fatalf("proposal: %+v", prop)
	}
	// The approval is under way, re-checking the proposal, when the pause
	// comes: it has read the proposer's seat and is waiting for the work.
	release := heldBy(t, b, `SELECT 1 FROM submission WHERE id = $1 FOR UPDATE`, work)
	approval := b.inFlight(b.sato, "action.decide", m{"course_id": b.course, "action_id": prop.ActionID, "decision": "approve"}, "approve")
	b.waitingFor(t, 1, approval)
	pause := b.inFlight(b.sato, "member.pause", m{"course_id": b.course, "member_id": b.graderM}, "pause")
	b.waitingFor(t, 2, approval, pause)
	release()
	if out := settled(t, approval); !strings.Contains(string(out.Result), `"outcome":"executed"`) {
		t.Fatalf("the approval: %+v", out)
	}
	if out := settled(t, pause); out.Status != domain.StatusExecuted {
		t.Fatalf("the pause: %+v", out)
	}
}

// Changing a student's seat and writing an event that names the student do
// not deadlock: the event's writer takes the seat before the course's event
// stream, so it never holds the stream while it waits for the seat.
func TestAnEventNamingAStudentTakesTheSeatFirst(t *testing.T) {
	b := build(t)
	work := b.submit(t, b.yuki, "essay")
	// The course's event stream is busy (see events.Flush for its key).
	h := fnv.New32a()
	_, _ = h.Write(b.course[:])
	release := heldBy(t, b, `SELECT pg_advisory_xact_lock($1::int4, $2::int4)`, int32(0x41495345), int32(h.Sum32()))
	lateness := b.inFlight(b.sato, "submission.set_lateness", m{"course_id": b.course, "submission_id": work, "state": "late"}, "late")
	b.waitingFor(t, 1, lateness)
	pause := b.inFlight(b.sato, "member.pause", m{"course_id": b.course, "member_id": b.yukiM}, "pause")
	b.waitingFor(t, 2, lateness, pause)
	release()
	if out := settled(t, lateness); out.Status != domain.StatusExecuted {
		t.Fatalf("the lateness correction: %+v", out)
	}
	if out := settled(t, pause); out.Status != domain.StatusExecuted {
		t.Fatalf("the pause: %+v", out)
	}
}

// A file is added to a submission, or archived from it, only while it is a
// draft, and the state that says so is read under the submission's lock. A
// file that comes while the draft is being handed in waits for the hand-in,
// and then finds the work handed in: it does not land on it afterwards, nor
// take away a file the hand-in counted. Here the hand-in holds the
// submission and waits for the course's event stream.
func TestAFileRacingTheHandInFindsItHandedIn(t *testing.T) {
	b := build(t)
	busy := func() (release func()) {
		h := fnv.New32a()
		_, _ = h.Write(b.course[:])
		return heldBy(t, b, `SELECT pg_advisory_xact_lock($1::int4, $2::int4)`, int32(0x41495345), int32(h.Sum32()))
	}

	// Yuki attaches an appendix as her essay is handed in.
	essay := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "essay"})).SubmissionID
	release := busy()
	submit := b.inFlight(b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": essay}, "submit-essay")
	b.waitingFor(t, 1, submit)
	attach := b.inFlight(b.yuki, "document.create", m{"course_id": b.course, "kind": "submission", "title": "appendix.txt",
		"submission_id": essay, "body_md": "second thoughts"}, "appendix")
	b.waitingFor(t, 2, submit, attach)
	release()
	if out := settled(t, submit); out.Status != domain.StatusExecuted {
		t.Fatalf("the hand-in: %+v", out)
	}
	if out := settled(t, attach); out.Error == nil || out.Error.Code != apperr.Conflict {
		t.Fatalf("a file added as the work was handed in: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM document WHERE submission_id = $1`, essay); n != 0 {
		t.Fatalf("the handed-in work has %d files; it was handed in with none", n)
	}

	// Ken archives his only file as his work is handed in.
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3})).SubmissionID
	file := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.ken, "document.create", m{"course_id": b.course, "kind": "submission",
		"title": "essay.txt", "submission_id": work, "body_md": "Ken's essay"})).DocumentID
	release = busy()
	submit = b.inFlight(b.ken, "submission.submit", m{"course_id": b.course, "submission_id": work}, "submit-work")
	b.waitingFor(t, 1, submit)
	archive := b.inFlight(b.ken, "document.archive", m{"course_id": b.course, "document_id": file}, "archive")
	b.waitingFor(t, 2, submit, archive)
	release()
	if out := settled(t, submit); out.Status != domain.StatusExecuted {
		t.Fatalf("the hand-in: %+v", out)
	}
	if out := settled(t, archive); out.Error == nil || out.Error.Code != apperr.Conflict {
		t.Fatalf("a file archived as the work was handed in: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND status = 'active'`, file); n != 1 {
		t.Fatal("the handed-in work lost the file it was handed in with")
	}
}

// Seating an actor over their expired seat removes that seat as any removal
// does: after the member's calls in flight, cancelling what they proposed. A
// seat the sweep removes meanwhile is simply out of the way; one whose expiry
// is moved meanwhile is still in it.
func TestReseatingOverAnExpiredSeatWaitsForItsCalls(t *testing.T) {
	b := build(t)
	work := b.submit(t, b.yuki, "essay")
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.graderM, "expires_at": time.Now().Add(time.Hour)})
	release := heldBy(t, b, `SELECT 1 FROM submission WHERE id = $1 FOR UPDATE`, work)
	proposing := b.inFlight(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 70}, "p")
	b.waitingFor(t, 1, proposing)
	b.P.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	reseat := b.inFlight(b.sato, "member.add", m{"course_id": b.course, "actor_id": b.grader, "preset": "grader"}, "reseat")
	b.waitingFor(t, 2, proposing, reseat)
	release()
	settled(t, proposing)
	if out := settled(t, reseat); out.Status != domain.StatusExecuted {
		t.Fatalf("re-seating: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE member_id = $1 AND status = 'proposed'`, b.graderM); n != 0 {
		t.Fatalf("%d proposals outlived the expired seat they were made from", n)
	}

	// The sweep removes Ken's expired seat while he is being seated again.
	b.P.SetClock(time.Now)
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.kenM, "expires_at": time.Now().Add(time.Hour)})
	b.P.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	release = heldBy(t, b, `UPDATE course_member SET status = 'removed' WHERE id = $1`, b.kenM)
	again := b.inFlight(b.sato, "member.add", m{"course_id": b.course, "actor_id": b.ken, "preset": "student"}, "ken-again")
	b.waitingFor(t, 1, again)
	release()
	if out := settled(t, again); out.Status != domain.StatusExecuted {
		t.Fatalf("seating Ken again as the sweep removed his seat: %+v", out)
	}

	// The tutor's expired seat is given longer while the tutor is being
	// seated again.
	b.P.SetClock(time.Now)
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.tutorM, "expires_at": time.Now().Add(time.Hour)})
	b.P.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	release = heldBy(t, b, `UPDATE course_member SET expires_at = now() + interval '3 hours' WHERE id = $1`, b.tutorM)
	extended := b.inFlight(b.sato, "member.add", m{"course_id": b.course, "actor_id": b.tutor, "preset": "tutor"}, "tutor-again")
	b.waitingFor(t, 1, extended)
	release()
	if out := settled(t, extended); out.Error == nil || !strings.Contains(out.Error.Message, "already has a seat") {
		t.Fatalf("seating the tutor again as their seat was given longer: %+v", out)
	}
}

// Seating an actor who has a live seat is refused without locking that seat.
// The lock would wait for its member's calls in flight, and deadlock with one
// that is changing the caller's own seat, only to say no. Here Sato seats
// Boss again, by mistake, while Boss pauses Sato.
func TestSeatingSomeoneAlreadySeatedLeavesTheirSeatAlone(t *testing.T) {
	b := build(t)
	boss := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Boss"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": boss, "preset": "instructor"})
	// Both calls hold their callers' seats and are about to record themselves.
	release := heldBy(t, b, `SELECT 1 FROM course WHERE id = $1 FOR UPDATE`, b.course)
	add := b.inFlight(b.sato, "member.add", m{"course_id": b.course, "actor_id": boss, "preset": "ta"}, "add")
	pause := b.inFlight(boss, "member.pause", m{"course_id": b.course, "member_id": b.satoM}, "pause")
	b.waitingFor(t, 2, add, pause)
	release()
	if out := settled(t, add); out.Error == nil || !strings.Contains(out.Error.Message, "already has a seat") {
		t.Fatalf("seating Boss again: %+v", out)
	}
	if out := settled(t, pause); out.Status != domain.StatusExecuted {
		t.Fatalf("Boss pausing Sato: %+v", out)
	}
}

// The same for the caller's own seat: two calls seating themselves at once,
// by mistake, each hold that seat shared, and would deadlock upgrading it.
func TestSeatingYourselfTwiceAtOnceIsRefusedTwice(t *testing.T) {
	b := build(t)
	release := heldBy(t, b, `SELECT 1 FROM course WHERE id = $1 FOR UPDATE`, b.course)
	again := m{"course_id": b.course, "actor_id": b.sato, "preset": "ta"}
	one := b.inFlight(b.sato, "member.add", again, "one")
	two := b.inFlight(b.sato, "member.add", again, "two")
	b.waitingFor(t, 2, one, two)
	release()
	for _, c := range []<-chan callResult{one, two} {
		if out := settled(t, c); out.Error == nil || !strings.Contains(out.Error.Message, "already has a seat") {
			t.Fatalf("Sato seating himself: %+v", out)
		}
	}
}

// An approval that loses a real deadlock while it carries its proposal out
// is made again, in a fresh transaction, and sees what the other did. Here
// two helpers each propose pausing the other, and both proposals are
// approved at once: each approval holds its proposer's seat and wants the
// other's. Which one PostgreSQL stops is its choice; it stops one, and the
// other goes through and pauses that one's proposer. Made again, the
// approval that was stopped finds its proposer paused and cancels the
// proposal, as any approval of a paused member's proposal does. Neither is
// recorded as having collided.
func TestAnApprovalThatLosesADeadlockCarryingItOutIsMadeAgain(t *testing.T) {
	b := build(t)
	register := func(name string) uuid.UUID {
		return testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": name})).ActorID
	}
	boss, helperA, helperB := register("Boss"), register("HelperA"), register("HelperB")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": boss, "preset": "instructor"})
	helper := func(actor uuid.UUID) uuid.UUID {
		return testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": actor,
			"preset": "ta", "perms": m{"member_manage": "confirm_required"}})).MemberID
	}
	helperAM, helperBM := helper(helperA), helper(helperB)

	type approval struct {
		by, proposerM uuid.UUID
		prop          *uuid.UUID
		back          <-chan callResult
	}
	propose := func(by, proposer, proposerM, target uuid.UUID, key string) *approval {
		out := b.MustCall(proposer, "member.pause", m{"course_id": b.course, "member_id": target}, key)
		if out.Status != domain.StatusProposed {
			t.Fatalf("proposal: %+v", out)
		}
		return &approval{by: by, proposerM: proposerM, prop: out.ActionID}
	}
	approvals := []*approval{
		propose(b.sato, helperA, helperAM, helperBM, "a-pauses-b"),
		propose(boss, helperB, helperBM, helperAM, "b-pauses-a"),
	}
	decide := func(a *approval) m {
		return m{"course_id": b.course, "action_id": a.prop, "decision": "approve"}
	}
	// Both approvals hold their proposers' seats and wait for their proposals.
	release := heldBy(t, b, `SELECT 1 FROM action WHERE id = ANY($1) FOR UPDATE`, []uuid.UUID{*approvals[0].prop, *approvals[1].prop})
	for i, a := range approvals {
		a.back = b.inFlight(a.by, "action.decide", decide(a), "approve-"+strconv.Itoa(i))
	}
	b.waitingFor(t, 2, approvals[0].back, approvals[1].back)
	release()

	var won, lost *approval
	for _, a := range approvals {
		out := settled(t, a.back)
		v := testkit.Result[pipeline.DecideOut](t, out)
		switch {
		case out.Status != domain.StatusExecuted:
			t.Fatalf("an approval: %+v", out)
		case v.Outcome == domain.StatusExecuted && won == nil:
			won = a
		case v.Outcome == domain.StatusCancelled && lost == nil:
			if v.Error.Details["reason"] != pipeline.CancelReauthorization || v.Error.Details["authz_reason"] != "membership_not_active" {
				t.Fatalf("the approval made again cancelled its proposal because %v", v.Error.Details)
			}
			lost = a
		default:
			t.Fatalf("an approval came to %s, and one should go through and the other cancel: %+v", v.Outcome, v)
		}
	}
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND status = 'paused'`, lost.proposerM); n != 1 {
		t.Fatal("the proposer of the approval made again is not the one paused")
	}
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND status = 'active'`, won.proposerM); n != 1 {
		t.Fatal("the proposer of the approval that went through was paused as well")
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'action.decide'`); n != 2 {
		t.Fatalf("%d decisions recorded, want the two", n)
	}
}

// A manager naming their own seat is refused before that seat is locked.
// Two such calls at once each hold the seat shared, as every call holds its
// caller's, and would deadlock upgrading it.
func TestEditingYourOwnSeatTwiceAtOnceIsRefusedTwice(t *testing.T) {
	b := build(t)
	release := heldBy(t, b, `SELECT 1 FROM course WHERE id = $1 FOR UPDATE`, b.course)
	pause := b.inFlight(b.sato, "member.pause", m{"course_id": b.course, "member_id": b.satoM}, "pause")
	perms := b.inFlight(b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.satoM, "perms": m{"grade_post": "confirm_required"}}, "perms")
	b.waitingFor(t, 2, pause, perms)
	release()
	for _, c := range []<-chan callResult{pause, perms} {
		if out := settled(t, c); out.Error == nil || out.Error.Message != "not on your own membership" {
			t.Fatalf("Sato changing his own seat: %+v", out)
		}
	}
}

// Two managers changing each other's seats at once each hold their own seat,
// shared, as every call holds its caller's, and wait to lock the other's: a
// deadlock, which PostgreSQL ends by stopping one of them. The one stopped
// is made again in a fresh transaction, after the other, and both changes
// go through; neither is failed with "try again", its key spent on that.
func TestManagersChangingEachOthersSeatsAtOnceBothGoThrough(t *testing.T) {
	b := build(t)
	boss := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Boss"})).ActorID
	bossM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": boss, "preset": "instructor"})).MemberID
	// Both calls hold their callers' seats and are about to record themselves.
	release := heldBy(t, b, `SELECT 1 FROM course WHERE id = $1 FOR UPDATE`, b.course)
	narrow := m{"grade_post": "confirm_required"}
	satos := b.inFlight(b.sato, "member.update_perms", m{"course_id": b.course, "member_id": bossM, "perms": narrow}, "narrow-boss")
	bosss := b.inFlight(boss, "member.update_perms", m{"course_id": b.course, "member_id": b.satoM, "perms": narrow}, "narrow-sato")
	b.waitingFor(t, 2, satos, bosss)
	release()
	for _, c := range []<-chan callResult{satos, bosss} {
		if out := settled(t, c); out.Status != domain.StatusExecuted {
			t.Fatalf("a manager narrowing the other's seat: %+v", out)
		}
	}
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = ANY($1) AND perm_grade_post = 'confirm_required'`, []uuid.UUID{b.satoM, bossM}); n != 2 {
		t.Fatalf("%d of the two seats were narrowed, want both", n)
	}
}
