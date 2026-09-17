package authz_test

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
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
