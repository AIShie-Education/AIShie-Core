package authz_test

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
)

// cs101 is the course from docs/schema.md §5, plus the awkward members.
type cs101 struct {
	w                          *testkit.World
	course, archived           uuid.UUID
	hw3, hw4                   uuid.UUID
	sato, yuki, ken            uuid.UUID // actors
	grader, tutor              uuid.UUID // actors (agents)
	stranger, suspended        uuid.UUID // actors
	paused, expired, nobodyYet uuid.UUID // actors
	satoM, yukiM, kenM         uuid.UUID // members
	graderM, tutorM, emptyM    uuid.UUID // members
}

func newCS101(t *testing.T) *cs101 {
	w := testkit.NewWorld(t)
	c := &cs101{w: w}
	c.course = w.Course("CS101")
	c.archived = w.Course("CS100")
	w.Exec(`UPDATE course SET status = 'archived' WHERE id = $1`, c.archived)
	c.hw3 = w.Assignment(c.course, "HW3")
	c.hw4 = w.Assignment(c.course, "HW4")

	c.sato = w.Actor("human", "Sato")
	c.yuki = w.Actor("human", "Yuki")
	c.ken = w.Actor("human", "Ken")
	c.grader = w.Actor("agent", "grader-v2")
	c.tutor = w.Actor("agent", "tutor")
	c.stranger = w.Actor("human", "Stranger")
	c.suspended = w.Actor("human", "Suspended")
	c.paused = w.Actor("human", "Paused")
	c.expired = w.Actor("agent", "Expired")
	c.nobodyYet = w.Actor("agent", "Listed for nobody")

	c.satoM = w.Member(c.course, c.sato, "instructor")
	w.Member(c.archived, c.sato, "instructor")
	c.yukiM = w.Member(c.course, c.yuki, "student")
	c.kenM = w.Member(c.course, c.ken, "student")
	c.graderM = w.Member(c.course, c.grader, "grader", testkit.ListedAssignments(c.hw3))
	c.tutorM = w.Member(c.course, c.tutor, "tutor", testkit.ListedStudents(c.yukiM))
	c.emptyM = w.Member(c.course, c.nobodyYet, "tutor") // listed, with nothing listed

	w.Member(c.course, c.suspended, "instructor")
	w.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, c.suspended)
	w.Member(c.course, c.paused, "instructor", testkit.WithStatus("paused"))
	w.Member(c.course, c.expired, "grader", testkit.Expires(time.Now().Add(-time.Minute)))
	return c
}

type check struct {
	name   string
	actor  uuid.UUID
	course uuid.UUID
	perm   domain.Perm
	write  bool
	target authz.Target
	want   domain.Level
	reason authz.Reason
}

func (c *cs101) checks() []check {
	yuki := authz.Target{StudentMemberIDs: []uuid.UUID{c.yukiM}}
	ken := authz.Target{StudentMemberIDs: []uuid.UUID{c.kenM}}
	yukiHW3 := authz.Target{StudentMemberIDs: []uuid.UUID{c.yukiM}, AssignmentIDs: []uuid.UUID{c.hw3}}
	yukiHW4 := authz.Target{StudentMemberIDs: []uuid.UUID{c.yukiM}, AssignmentIDs: []uuid.UUID{c.hw4}}
	both := authz.Target{StudentMemberIDs: []uuid.UUID{c.yukiM, c.kenM}, AssignmentIDs: []uuid.UUID{c.hw3}}
	batch := authz.Target{StudentMemberIDs: []uuid.UUID{c.yukiM, c.kenM, c.yukiM}, AssignmentIDs: []uuid.UUID{c.hw3, c.hw3}}
	none := authz.Target{}
	yukiMidterm := authz.Target{StudentMemberIDs: []uuid.UUID{c.yukiM}, SpansAssignments: true}

	return []check{
		// step 1
		{"suspended actor", c.suspended, c.course, domain.PermGradePost, true, none, domain.Denied, authz.ReasonActorNotActive},
		{"archived course refuses the instructor's write", c.sato, c.archived, domain.PermDocumentWrite, true, none, domain.Denied, authz.ReasonCourseArchived},
		{"archived course still lets the instructor read", c.sato, c.archived, domain.PermDocumentRead, false, none, domain.Autonomous, ""},
		// step 2
		{"not a member", c.stranger, c.course, domain.PermDocumentRead, false, none, domain.Denied, authz.ReasonNotAMember},
		{"member of another course only", c.yuki, c.archived, domain.PermDocumentRead, false, none, domain.Denied, authz.ReasonNotAMember},
		{"paused member", c.paused, c.course, domain.PermDocumentRead, false, none, domain.Denied, authz.ReasonMemberNotLive},
		{"expired member, before any sweep has run", c.expired, c.course, domain.PermDocumentRead, false, none, domain.Denied, authz.ReasonMemberNotLive},
		// step 3
		{"instructor posts grades", c.sato, c.course, domain.PermGradePost, true, both, domain.Autonomous, ""},
		{"student cannot grade", c.yuki, c.course, domain.PermGradeSubmit, true, yukiHW3, domain.Denied, authz.ReasonPermDenied},
		{"grader agent proposes, it does not decide", c.grader, c.course, domain.PermActionDecide, true, none, domain.Denied, authz.ReasonPermDenied},
		// step 4
		{"student reads own grades", c.yuki, c.course, domain.PermGradeRead, false, yuki, domain.Autonomous, ""},
		{"student cannot read another's grades", c.yuki, c.course, domain.PermGradeRead, false, ken, domain.Denied, authz.ReasonStudentScope},
		{"student reads course material; scope does not apply", c.yuki, c.course, domain.PermDocumentRead, false, none, domain.Autonomous, ""},
		{"tutor reads the listed student's work", c.tutor, c.course, domain.PermSubmissionRead, false, yukiHW4, domain.Autonomous, ""},
		{"tutor cannot read an unlisted student's work", c.tutor, c.course, domain.PermSubmissionRead, false, ken, domain.Denied, authz.ReasonStudentScope},
		{"a batch is in scope only if all of it is", c.tutor, c.course, domain.PermSubmissionRead, false, both, domain.Denied, authz.ReasonStudentScope},
		{"listed with nothing listed means nobody", c.nobodyYet, c.course, domain.PermSubmissionRead, false, yuki, domain.Denied, authz.ReasonStudentScope},
		// step 5
		{"grader proposes for the listed assignment", c.grader, c.course, domain.PermGradeSubmit, true, yukiHW3, domain.ConfirmRequired, ""},
		{"grader cannot touch an unlisted assignment", c.grader, c.course, domain.PermGradeSubmit, true, yukiHW4, domain.Denied, authz.ReasonAssignmentScope},
		{"grader: whole class, listed assignment, ids repeated", c.grader, c.course, domain.PermGradeSubmit, true, batch, domain.ConfirmRequired, ""},
		{"grader listed for HW3 cannot touch a component grade", c.grader, c.course, domain.PermSubmissionRead, false, yukiMidterm, domain.Denied, authz.ReasonAssignmentScope},
		{"instructor can", c.sato, c.course, domain.PermGradeSubmit, true, yukiMidterm, domain.Autonomous, ""},
		{"student sees own component grade: assignment scope is all", c.yuki, c.course, domain.PermGradeRead, false, yukiMidterm, domain.Autonomous, ""},
	}
}

func run(t *testing.T, c *cs101, ck check) authz.Decision {
	t.Helper()
	d, err := authz.Authorize(context.Background(), c.w.Q, ck.actor, ck.course, []domain.Perm{ck.perm}, ck.write, ck.target, time.Now())
	if err != nil {
		t.Fatalf("%s: %v", ck.name, err)
	}
	return d
}

func TestAuthorize(t *testing.T) {
	c := newCS101(t)
	for _, ck := range c.checks() {
		t.Run(ck.name, func(t *testing.T) {
			d := run(t, c, ck)
			if d.Level != ck.want || d.Reason != ck.reason {
				t.Fatalf("got %s (%q), want %s (%q)", d.Level, d.Reason, ck.want, ck.reason)
			}
		})
	}
}

// schema.md §4: actor.kind and course_member.role are never read by
// authorization. Turn every human into an agent and every agent into a
// human, shuffle every roster role, and nothing may change.
func TestAuthorizeIgnoresActorKindAndMemberRole(t *testing.T) {
	c := newCS101(t)
	before := map[string]authz.Decision{}
	for _, ck := range c.checks() {
		before[ck.name] = run(t, c, ck)
	}

	c.w.Exec(`UPDATE actor SET kind = CASE kind WHEN 'human' THEN 'agent' WHEN 'agent' THEN 'human' ELSE kind END`)
	c.w.Exec(`UPDATE course_member SET role = CASE role
		WHEN 'student' THEN 'instructor' WHEN 'instructor' THEN 'assistant'
		WHEN 'assistant' THEN 'observer' ELSE 'student' END`)

	for _, ck := range c.checks() {
		after := run(t, c, ck)
		if after.Level != before[ck.name].Level || after.Reason != before[ck.name].Reason {
			t.Errorf("%s: %s (%q) became %s (%q) after swapping kinds and roles",
				ck.name, before[ck.name].Level, before[ck.name].Reason, after.Level, after.Reason)
		}
	}
}

// The same guarantee from the other side: the queries cannot branch on a
// column they never select.
func TestAuthzQueriesNeverSelectKindOrRole(t *testing.T) {
	src, err := os.ReadFile("../db/queries/authz.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := regexp.MustCompile(`(?m)--.*$`).ReplaceAllString(string(src), "")
	for _, word := range []string{"kind", "role"} {
		if regexp.MustCompile(`(?i)\b` + word + `\b`).MatchString(sql) {
			t.Errorf("queries/authz.sql names the column %q; authorization must not read it", word)
		}
	}
}

func TestUnknownCourseIsNotFoundNotDenied(t *testing.T) {
	c := newCS101(t)
	_, err := authz.ForActor(context.Background(), c.w.Q, c.sato, uuid.New(), []domain.Perm{domain.PermDocumentRead}, false, time.Now())
	if !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("err = %v, want not_found", err)
	}
}

// Re-authorizing a proposal checks the membership row it was made under.
func TestForMemberUsesTheOriginalRow(t *testing.T) {
	c := newCS101(t)
	ctx := context.Background()
	submit := []domain.Perm{domain.PermGradeSubmit}

	d, err := authz.ForMember(ctx, c.w.Q, c.graderM, submit, true, time.Now())
	if err != nil || d.Level != domain.ConfirmRequired {
		t.Fatalf("live row: %s %v", d.Level, err)
	}

	// Remove the grader and seat it again: a new row, a fresh start.
	c.w.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, c.graderM)
	readded := c.w.Member(c.course, c.grader, "grader", testkit.ListedAssignments(c.hw3))

	d, err = authz.ForMember(ctx, c.w.Q, c.graderM, submit, true, time.Now())
	if err != nil || d.Level != domain.Denied || d.Reason != authz.ReasonMemberNotLive {
		t.Fatalf("removed row: %s (%q) %v; want denied", d.Level, d.Reason, err)
	}
	d, err = authz.ForMember(ctx, c.w.Q, readded, submit, true, time.Now())
	if err != nil || d.Level != domain.ConfirmRequired {
		t.Fatalf("re-added row: %s %v", d.Level, err)
	}
	d, err = authz.ForMember(ctx, c.w.Q, uuid.New(), submit, true, time.Now())
	if err != nil || d.Level != domain.Denied {
		t.Fatalf("unknown row: %s %v; want denied", d.Level, err)
	}
}

// Nothing is cached: a change to the row applies to the very next call.
func TestChangesApplyOnTheNextCall(t *testing.T) {
	c := newCS101(t)
	ck := check{"", c.grader, c.course, domain.PermGradeSubmit, true,
		authz.Target{AssignmentIDs: []uuid.UUID{c.hw3}}, 0, ""}

	if d := run(t, c, ck); d.Level != domain.ConfirmRequired {
		t.Fatalf("before: %s", d.Level)
	}
	c.w.Exec(`UPDATE course_member SET perm_grade_submit = 'autonomous' WHERE id = $1`, c.graderM)
	if d := run(t, c, ck); d.Level != domain.Autonomous {
		t.Fatalf("after raising the level: %s", d.Level)
	}
	c.w.Exec(`DELETE FROM member_assignment_scope WHERE member_id = $1`, c.graderM)
	if d := run(t, c, ck); d.Reason != authz.ReasonAssignmentScope {
		t.Fatalf("after unlisting HW3: %s (%q)", d.Level, d.Reason)
	}
	c.w.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, c.graderM)
	if d := run(t, c, ck); d.Reason != authz.ReasonNotAMember {
		t.Fatalf("after removal: %s (%q)", d.Level, d.Reason)
	}
}

// The permission catalogue in code and the perm_* columns on both tables are
// the same list. Adding an action type to one and not the others fails here.
func TestPermCatalogueMatchesColumns(t *testing.T) {
	w := testkit.NewWorld(t)
	var want []string
	for _, p := range domain.AllPerms {
		want = append(want, p.Column())
	}
	sort.Strings(want)

	for _, table := range []string{"course_member", "permission_preset"} {
		rows, err := w.Pool.Query(context.Background(), `
			SELECT column_name::text FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name LIKE 'perm\_%'
			ORDER BY 1`, table)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			got = append(got, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s columns:\n  %v\ndomain.AllPerms:\n  %v", table, got, want)
		}
	}
}

// Every permission in the catalogue is actually read off the row. A preset
// that is autonomous everywhere must come back autonomous everywhere.
func TestEveryPermIsLoaded(t *testing.T) {
	c := newCS101(t)
	for _, p := range domain.AllPerms {
		d, err := authz.ForActor(context.Background(), c.w.Q, c.sato, c.course, []domain.Perm{p}, false, time.Now())
		if err != nil || d.Level != domain.Autonomous {
			t.Errorf("%s: instructor got %s (%v), want autonomous", p, d.Level, err)
		}
	}
}

// A delegate is authorized as its own seat capped by its principal's, read
// with it: levels, liveness, reach, and the owner behind it.
func TestADelegateHoldsNoMoreThanItsPrincipal(t *testing.T) {
	c := newCS101(t)
	w := c.w
	agent := w.OwnedAgent(c.yuki, "Yuki's agent")
	d := w.Delegate(c.course, agent, c.yukiM, "delegate", testkit.ListedStudents(c.yukiM))
	yuki := authz.Target{StudentMemberIDs: []uuid.UUID{c.yukiM}, AssignmentIDs: []uuid.UUID{c.hw3}}
	ken := authz.Target{StudentMemberIDs: []uuid.UUID{c.kenM}}
	expect := func(name string, perm domain.Perm, write bool, target authz.Target, want domain.Level, reason authz.Reason) {
		t.Helper()
		got := run(t, c, check{name, agent, c.course, perm, write, target, 0, ""})
		if got.Level != want || got.Reason != reason {
			t.Fatalf("%s: got %s (%q), want %s (%q)", name, got.Level, got.Reason, want, reason)
		}
		m, err := authz.ForMember(context.Background(), w.Q, d, []domain.Perm{perm}, write, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if reason == "" || reason == authz.ReasonPrincipalNotActive || reason == authz.ReasonPermDenied {
			if m.Level != want || m.Reason != reason {
				t.Fatalf("%s, by its row: got %s (%q), want %s (%q)", name, m.Level, m.Reason, want, reason)
			}
		}
	}

	expect("reads its principal's work", domain.PermSubmissionRead, false, yuki, domain.Autonomous, "")
	expect("and for a write", domain.PermSubmissionRead, true, yuki, domain.Autonomous, "")
	// A write holds its principal's seat with its own, KEY SHARE, to the end
	// of its transaction: removing or changing the principal waits for it.
	func() {
		ctx := context.Background()
		tx, err := w.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if got, err := authz.Authorize(ctx, dbq.New(tx), agent, c.course, []domain.Perm{domain.PermSubmissionRead}, true, yuki, time.Now()); err != nil || got.Level != domain.Autonomous {
			t.Fatalf("a delegate's write: %+v %v", got, err)
		}
		for seat, what := range map[uuid.UUID]string{d: "its own seat", c.yukiM: "its principal's seat"} {
			_, err := w.Pool.Exec(ctx, `SELECT 1 FROM course_member WHERE id = $1 FOR UPDATE NOWAIT`, seat)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("a delegate's write does not hold %s: %v", what, err)
			}
		}
	}()
	expect("not another student's", domain.PermSubmissionRead, false, ken, domain.Denied, authz.ReasonStudentScope)
	// Its own row widened behind everyone's back, as the previous release
	// could: still no further than its principal.
	w.Exec(`UPDATE course_member SET student_scope = 'all', perm_grade_submit = 'autonomous', perm_member_manage = 'autonomous' WHERE id = $1`, d)
	expect("its principal's reach caps its own", domain.PermSubmissionRead, false, ken, domain.Denied, authz.ReasonStudentScope)
	expect("its principal's level caps its own", domain.PermGradeSubmit, true, yuki, domain.Denied, authz.ReasonPermDenied)
	expect("it never manages the course", domain.PermMemberManage, true, authz.Target{}, domain.Denied, authz.ReasonPermDenied)
	w.Exec(`UPDATE course_member SET perm_submission_read = 'confirm_required' WHERE id = $1`, c.yukiM)
	expect("a principal's level lowered lowers the delegate's", domain.PermSubmissionRead, true, yuki, domain.ConfirmRequired, "")
	w.Exec(`UPDATE course_member SET perm_submission_read = 'autonomous' WHERE id = $1`, c.yukiM)

	w.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, c.yukiM)
	expect("paused with its principal", domain.PermDocumentRead, false, authz.Target{}, domain.Denied, authz.ReasonPrincipalNotActive)
	w.Exec(`UPDATE course_member SET status = 'active' WHERE id = $1`, c.yukiM)
	w.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, c.yuki)
	expect("nothing while its owner is suspended", domain.PermDocumentRead, false, authz.Target{}, domain.Denied, authz.ReasonPrincipalNotActive)
	w.Exec(`UPDATE actor SET status = 'active' WHERE id = $1`, c.yuki)
	expect("back with its owner", domain.PermDocumentRead, false, authz.Target{}, domain.Autonomous, "")
	w.Exec(`UPDATE actor SET owner_actor_id = $2 WHERE id = $1`, agent, c.ken)
	expect("nothing once another owns it", domain.PermDocumentRead, false, authz.Target{}, domain.Denied, authz.ReasonPrincipalNotActive)
	w.Exec(`UPDATE actor SET owner_actor_id = NULL WHERE id = $1`, agent)
	expect("nor once nobody does", domain.PermDocumentRead, false, authz.Target{}, domain.Denied, authz.ReasonPrincipalNotActive)

	// An agent seated before anyone owned it counts for nothing once someone
	// does: it must be brought in again, by its owner.
	w.Exec(`UPDATE actor SET owner_actor_id = $2 WHERE id = $1`, c.tutor, c.sato)
	got := run(t, c, check{"", c.tutor, c.course, domain.PermDocumentRead, false, authz.Target{}, 0, ""})
	if got.Level != domain.Denied || got.Reason != authz.ReasonPrincipalNotActive {
		t.Fatalf("an owned agent's seat with no principal: %s (%q)", got.Level, got.Reason)
	}
}

// SeatFor is steps 1 and 2 without step 3: whether an actor's seat counts,
// whatever it may do there, and why not if it does not.
func TestSeatForSaysWhetherASeatCounts(t *testing.T) {
	c := newCS101(t)
	ctx := context.Background()
	now := time.Now()
	helper := c.w.OwnedAgent(c.yuki, "Yuki's helper")
	helperM := c.w.Delegate(c.course, helper, c.yukiM, "delegate")
	for _, tc := range []struct {
		name   string
		actor  uuid.UUID
		course uuid.UUID
		write  bool
		seat   uuid.UUID
		reason authz.Reason
	}{
		{"a live seat, even one that may do nothing", c.nobodyYet, c.course, true, c.emptyM, authz.ReasonNone},
		{"a delegate's, its principal live", helper, c.course, true, helperM, authz.ReasonNone},
		{"no seat", c.stranger, c.course, false, uuid.Nil, authz.ReasonNotAMember},
		{"a paused seat", c.paused, c.course, false, uuid.Nil, authz.ReasonMemberNotLive},
		{"an expired seat", c.expired, c.course, true, uuid.Nil, authz.ReasonMemberNotLive},
		{"an actor suspended", c.suspended, c.course, false, uuid.Nil, authz.ReasonActorNotActive},
		{"a write to an archived course", c.sato, c.archived, true, uuid.Nil, authz.ReasonCourseArchived},
		{"a read of one", c.sato, c.archived, false, uuid.Nil, authz.ReasonNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, reason, err := authz.SeatFor(ctx, c.w.Q, tc.actor, tc.course, tc.write, now)
			if err != nil || reason != tc.reason || (tc.seat != uuid.Nil && (m == nil || m.ID != tc.seat)) {
				t.Fatalf("%v %q %v", m, reason, err)
			}
		})
	}
	c.w.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, c.yukiM)
	if _, reason, err := authz.SeatFor(ctx, c.w.Q, helper, c.course, false, now); err != nil || reason != authz.ReasonPrincipalNotActive {
		t.Fatalf("a delegate whose principal is paused: %q %v", reason, err)
	}
	if _, _, err := authz.SeatFor(ctx, c.w.Q, c.sato, uuid.New(), false, now); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("no such course: %v", err)
	}
}

// An administrator's Admin-gated write holds the appointment it relies on,
// the nearest, to its end: ending that appointment waits for the write. A
// read holds nothing, and a platform administrator relies on no appointment.
func TestAnAdministratorsWriteHoldsTheAppointmentItReliesOn(t *testing.T) {
	w := testkit.NewDeptTree(t)
	ctx := context.Background()
	held := func(appointment uuid.UUID) bool {
		t.Helper()
		_, err := w.Pool.Exec(ctx, `SELECT 1 FROM department_admin WHERE id = $1 FOR UPDATE NOWAIT`, appointment)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return true
		}
		if err != nil {
			t.Fatal(err)
		}
		return false
	}
	within := func(actor uuid.UUID, write bool, dept uuid.UUID, check func(*domain.Authority, bool)) {
		t.Helper()
		tx, err := w.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		a, err := authz.LoadActor(ctx, dbq.New(tx), actor)
		if err != nil {
			t.Fatal(err)
		}
		scope, d := authz.Admin(a, write)
		if !d.Level.Allowed() {
			t.Fatalf("%v is no administrator: %+v", actor, d)
		}
		auth, ok, err := scope.Covers(ctx, dbq.New(tx), dept)
		if err != nil {
			t.Fatal(err)
		}
		check(auth, ok)
	}

	within(w.Bob, true, w.D, func(auth *domain.Authority, ok bool) {
		if !ok || auth.AppointmentID != w.BobS || auth.DeptID != w.S {
			t.Fatalf("Bob on AI: %+v %v", auth, ok)
		}
		if !held(w.BobS) {
			t.Fatal("a write does not hold the appointment it relies on")
		}
		if held(w.ChanS) || held(w.AdaF) {
			t.Fatal("a write holds an appointment it does not rely on")
		}
	})
	within(w.Bob, false, w.D, func(_ *domain.Authority, ok bool) {
		if !ok || held(w.BobS) {
			t.Fatalf("a read: covered %v, and holds the appointment", ok)
		}
	})
	within(w.Bob, true, w.S2, func(auth *domain.Authority, ok bool) {
		if ok || auth != nil {
			t.Fatalf("Bob on Design, a sibling of his: %+v %v", auth, ok)
		}
	})
	within(w.Admin, true, w.S3, func(auth *domain.Authority, ok bool) {
		if !ok || auth != nil {
			t.Fatalf("a platform administrator: %+v %v", auth, ok)
		}
	})
}
