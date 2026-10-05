package tools_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Group assignments (docs/schema.md §2.5a, Groups; §2.5, Group work; §2.7, A
// group's grade): reusable group sets of a course, formed by hand, by a
// random split and by students signing themselves up; one piece of work per
// group, its draft written by the group's members together, handed in for
// them; graded once for the group, each member given a grade from it,
// adjusted with a reason where the grader says.

// seatStudents seats a student for each name, through the tools, and returns
// their actors and seats by name.
func (b *built) seatStudents(t *testing.T, names ...string) (actors, seats map[string]uuid.UUID) {
	t.Helper()
	actors, seats = map[string]uuid.UUID{}, map[string]uuid.UUID{}
	for _, n := range names {
		actors[n] = b.person(t, n, "")
		seats[n] = testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add",
			m{"course_id": b.course, "actor_id": actors[n], "preset": "student"})).MemberID
	}
	return actors, seats
}

func (b *built) groupSet(t *testing.T, name string, args m) uuid.UUID {
	t.Helper()
	if args == nil {
		args = m{}
	}
	args["course_id"], args["name"] = b.course, name
	return testkit.Result[tools.IDOut](t, b.do(t, b.sato, "group_set.create", args)).ID
}

func (b *built) groupsIn(t *testing.T, set uuid.UUID, groups ...m) []uuid.UUID {
	t.Helper()
	return testkit.Result[tools.GroupCreateOut](t, b.do(t, b.sato, "group.create",
		m{"course_id": b.course, "set_id": set, "groups": groups})).GroupIDs
}

// place puts each student in a group of set by hand, as Sato.
func (b *built) place(t *testing.T, set uuid.UUID, affectsWork bool, pairs ...any) tools.GroupSetMembersOut {
	t.Helper()
	placements := []m{}
	for i := 0; i < len(pairs); i += 2 {
		p := m{"student_member_id": pairs[i]}
		if g, ok := pairs[i+1].(uuid.UUID); ok {
			p["group_id"] = g
		}
		placements = append(placements, p)
	}
	return testkit.Result[tools.GroupSetMembersOut](t, b.do(t, b.sato, "group.set_members",
		m{"course_id": b.course, "set_id": set, "placements": placements, "affects_work": affectsWork}))
}

func (b *built) setView(t *testing.T, actor, set uuid.UUID, history bool) tools.GroupSetView {
	t.Helper()
	return testkit.Result[tools.GroupSetView](t, b.do(t, actor, "group_set.get",
		m{"course_id": b.course, "set_id": set, "include_history": history}))
}

// groupAssignment makes a published group assignment of set, worth 100 and
// counting toward the Assignments bucket.
func (b *built) groupAssignment(t *testing.T, title string, set uuid.UUID) uuid.UUID {
	t.Helper()
	id := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": title,
		"points_possible": 100, "component_id": b.bucket, "group_set_id": set})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": id})
	return id
}

// membersOf is a group's members as a view shows them.
func membersOf(v tools.GroupSetView, group uuid.UUID) []uuid.UUID {
	for _, g := range v.Groups {
		if g.ID == group {
			out := []uuid.UUID{}
			for _, mm := range g.Members {
				out = append(out, mm.MemberID)
			}
			return out
		}
	}
	return nil
}

func sameSet(a, b []uuid.UUID) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	cmp := func(x, y uuid.UUID) int { return compareIDs(x, y) }
	slices.SortFunc(a, cmp)
	slices.SortFunc(b, cmp)
	return slices.Equal(a, b)
}

func compareIDs(x, y uuid.UUID) int {
	for i := range x {
		if x[i] != y[i] {
			return int(x[i]) - int(y[i])
		}
	}
	return 0
}

// Groups are formed by hand: students placed, moved and taken out, all or
// none, capacity not binding the teacher; the history kept; each student
// shown their own group's members and no other's.
func TestGroupsAreFormedByHand(t *testing.T) {
	b := build(t)
	_, s := b.seatStudents(t, "Aoi", "Ren", "Hana")
	set := b.groupSet(t, "Projects", m{"description": "Term project teams"})
	g := b.groupsIn(t, set, m{"name": "Team A"}, m{"name": "Team B", "capacity": 2})
	teamA, teamB := g[0], g[1]

	out := b.place(t, set, false, b.yukiM, teamA, b.kenM, teamA, s["Aoi"], teamB)
	if len(out.Moved) != 3 || len(out.OverCapacity) != 0 {
		t.Fatalf("the first placements: %+v", out)
	}
	// Capacity binds sign-up, not the teacher: the result says so.
	out = b.place(t, set, false, s["Ren"], teamB, s["Hana"], teamB)
	if len(out.Moved) != 2 || !slices.Equal(out.OverCapacity, []uuid.UUID{teamB}) {
		t.Fatalf("over capacity: %+v", out)
	}
	// Placed where they are, nothing moves; all or none.
	if out := b.place(t, set, false, b.yukiM, teamA); len(out.Moved) != 0 {
		t.Fatalf("placing a student where she is: %+v", out)
	}
	b.refusedAs(t, b.sato, "group.set_members", m{"course_id": b.course, "set_id": set, "placements": []m{
		{"student_member_id": s["Hana"], "group_id": teamA}, {"student_member_id": b.graderM, "group_id": teamA}}},
		apperr.FailedPrecondition, tools.ReasonNotAStudent)
	if v := b.setView(t, b.sato, set, false); !sameSet(membersOf(v, teamB), []uuid.UUID{s["Aoi"], s["Ren"], s["Hana"]}) {
		t.Fatalf("a refused batch changed something: %+v", v.Groups)
	}

	// Moved and taken out: the stays end, saying how, and new ones begin.
	b.place(t, set, false, s["Hana"], teamA, s["Ren"], nil)
	v := b.setView(t, b.sato, set, true)
	if !sameSet(membersOf(v, teamA), []uuid.UUID{b.yukiM, b.kenM, s["Hana"]}) || !sameSet(membersOf(v, teamB), []uuid.UUID{s["Aoi"]}) {
		t.Fatalf("the groups after moves: %+v", v.Groups)
	}
	if v.UnassignedCount == nil || *v.UnassignedCount != 1 || len(v.Unassigned) != 1 || v.Unassigned[0].MemberID != s["Ren"] {
		t.Fatalf("the students in no group: %v %+v", v.UnassignedCount, v.Unassigned)
	}
	how := map[string]int{}
	for _, h := range v.History {
		how[h.JoinedHow]++
		if h.LeftHow != nil {
			how["left:"+*h.LeftHow]++
		}
	}
	if len(v.History) != 6 || how["assigned"] != 6 || how["left:moved"] != 1 || how["left:unassigned"] != 1 {
		t.Fatalf("the history: %v %+v", how, v.History)
	}

	// Yuki is shown her own group's members, by name, and nobody else's;
	// the set says which group is hers.
	mine := b.setView(t, b.yuki, set, true)
	if mine.MyGroupID == nil || *mine.MyGroupID != teamA || len(mine.History) != 0 || mine.Unassigned != nil || mine.UnassignedCount != nil {
		t.Fatalf("Yuki's view of the set: %+v", mine)
	}
	for _, gv := range mine.Groups {
		switch {
		case gv.ID == teamA && (len(gv.Members) != 3 || gv.Members[0].DisplayName == nil):
			t.Fatalf("Yuki's own group: %+v", gv)
		case gv.ID != teamA && len(gv.Members) != 0:
			t.Fatalf("Yuki is shown another group's members: %+v", gv)
		case gv.ID == teamB && gv.Size != 1:
			t.Fatalf("a group's size is everyone's: %+v", gv)
		}
	}
	// The tutor, listed for Yuki, reads the member list for her alone.
	if tv := b.setView(t, b.tutor, set, false); len(membersOf(tv, teamA)) > 1 || len(membersOf(tv, teamB)) != 0 {
		t.Fatalf("the tutor's view: %+v", tv.Groups)
	}

	// Archiving: a group with members is not archived; an empty one is.
	b.refusedAs(t, b.sato, "group.update", m{"course_id": b.course, "group_id": teamB, "archived": true},
		apperr.FailedPrecondition, tools.ReasonGroupNotEmpty)
	b.place(t, set, false, s["Aoi"], nil)
	b.do(t, b.sato, "group.update", m{"course_id": b.course, "group_id": teamB, "archived": true})
	b.refusedAs(t, b.sato, "group.set_members", m{"course_id": b.course, "set_id": set,
		"placements": []m{{"student_member_id": s["Aoi"], "group_id": teamB}}}, apperr.FailedPrecondition, tools.ReasonGroupArchived)
	// Its name is free again among those not archived.
	b.groupsIn(t, set, m{"name": "team b"})
	b.refusedAs(t, b.sato, "group.create", m{"course_id": b.course, "set_id": set, "groups": []m{{"name": "TEAM A"}}},
		apperr.Conflict, tools.ReasonNameTaken)
	// An archived set takes no change but being brought back.
	b.do(t, b.sato, "group_set.update", m{"course_id": b.course, "set_id": set, "archived": true})
	b.refusedAs(t, b.sato, "group.set_members", m{"course_id": b.course, "set_id": set,
		"placements": []m{{"student_member_id": s["Aoi"], "group_id": teamA}}}, apperr.FailedPrecondition, tools.ReasonSetArchived)
	list := testkit.Result[tools.GroupSetListOut](t, b.do(t, b.sato, "group_set.list", m{"course_id": b.course}))
	if len(list.Sets) != 0 {
		t.Fatalf("an archived set is listed: %+v", list.Sets)
	}
	b.do(t, b.sato, "group_set.update", m{"course_id": b.course, "set_id": set, "archived": false})

	// A student places nobody: forming groups is the teacher's.
	b.refusedAs(t, b.yuki, "group.set_members", m{"course_id": b.course, "set_id": set,
		"placements": []m{{"student_member_id": b.yukiM}}}, apperr.Forbidden, "")
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'group.member_added' AND student_member_id = $1`, b.kenM); n != 1 {
		t.Fatalf("%d placements of Ken in the feed, want 1", n)
	}
}

// The random split depends only on the seed, the students and the groups:
// the same seed deals the same way; it never touches a group with work,
// and is refused whole when someone would have nowhere to go.
func TestTheRandomSplitDealsBySeed(t *testing.T) {
	b := build(t)
	_, s := b.seatStudents(t, "Aoi", "Ren", "Hana")
	students := []uuid.UUID{b.yukiM, b.kenM, s["Aoi"], s["Ren"], s["Hana"]}

	// By size 2, from those in no group: three groups made, none past two.
	labs := b.groupSet(t, "Labs", nil)
	out := testkit.Result[tools.GroupSplitOut](t, b.do(t, b.sato, "group.split", m{"course_id": b.course, "set_id": labs,
		"by": "size", "n": 2, "from": "unassigned", "seed": "lab-2026", "name_prefix": "Lab "}))
	if out.Seed != "lab-2026" || len(out.Created) != 3 || len(out.Placed) != 5 || len(out.Kept) != 0 {
		t.Fatalf("the split: %+v", out)
	}
	names := []string{}
	sizes := map[uuid.UUID]int{}
	for _, c := range out.Created {
		names = append(names, c.Name)
	}
	for _, p := range out.Placed {
		sizes[p.GroupID]++
	}
	if !slices.Equal(names, []string{"Lab 1", "Lab 2", "Lab 3"}) {
		t.Fatalf("the groups made: %v", names)
	}
	for g, n := range sizes {
		if n > 2 {
			t.Fatalf("group %s was dealt %d, past 2", g, n)
		}
	}
	// The same seed in another set deals the same students together.
	again := b.groupSet(t, "Labs again", nil)
	other := testkit.Result[tools.GroupSplitOut](t, b.do(t, b.sato, "group.split", m{"course_id": b.course, "set_id": again,
		"by": "size", "n": 2, "from": "unassigned", "seed": "lab-2026", "name_prefix": "Lab "}))
	together := func(o tools.GroupSplitOut) map[string][]uuid.UUID {
		byID := map[uuid.UUID]string{}
		for _, c := range o.Created {
			byID[c.GroupID] = c.Name
		}
		out := map[string][]uuid.UUID{}
		for _, p := range o.Placed {
			out[byID[p.GroupID]] = append(out[byID[p.GroupID]], p.StudentMemberID)
		}
		return out
	}
	one, two := together(out), together(other)
	for name, ms := range one {
		if !sameSet(ms, two[name]) {
			t.Fatalf("the same seed dealt %s two ways: %v and %v", name, ms, two[name])
		}
	}

	// By count, made up seed: the set comes to three groups, everyone dealt.
	counted := b.groupSet(t, "Counted", nil)
	b.groupsIn(t, counted, m{"name": "Group 1"})
	c := testkit.Result[tools.GroupSplitOut](t, b.do(t, b.sato, "group.split", m{"course_id": b.course, "set_id": counted,
		"by": "count", "n": 3, "from": "all"}))
	if len(c.Seed) != 12 || len(c.Created) != 2 || len(c.Placed) != 5 || c.Created[0].Name != "Group 2" {
		t.Fatalf("by count: %+v", c)
	}

	// A group with work is kept; from all, the rest are emptied and dealt
	// again, the kept one's members left where they are.
	hw := b.groupAssignment(t, "Lab report", labs)
	first := out.Placed[0]
	b.do(t, b.sato, "submission.create", m{"course_id": b.course, "assignment_id": hw, "group_id": first.GroupID})
	re := testkit.Result[tools.GroupSplitOut](t, b.do(t, b.sato, "group.split", m{"course_id": b.course, "set_id": labs,
		"by": "count", "n": 3, "from": "all", "seed": "second"}))
	if len(re.Kept) != 1 || re.Kept[0].GroupID != first.GroupID || re.Kept[0].Reason != "has_work" {
		t.Fatalf("the group with work: %+v", re.Kept)
	}
	for _, p := range re.Placed {
		if p.GroupID == first.GroupID {
			t.Fatalf("a student was dealt into the group with work: %+v", re)
		}
		if p.StudentMemberID == first.StudentMemberID {
			t.Fatalf("a member of the group with work was dealt again: %+v", re)
		}
	}
	if len(re.Placed) != len(students)-sizes[first.GroupID] {
		t.Fatalf("dealt %d, want everyone but the kept group's %d", len(re.Placed), sizes[first.GroupID])
	}

	// Nowhere to go: refused whole, nothing written.
	full := b.groupSet(t, "Full", nil)
	b.groupsIn(t, full, m{"name": "Only", "capacity": 1})
	before := b.Count(`SELECT count(*) FROM group_membership`)
	b.refusedAs(t, b.sato, "group.split", m{"course_id": b.course, "set_id": full, "by": "count", "n": 1, "from": "unassigned"},
		apperr.FailedPrecondition, tools.ReasonNoRoom)
	if b.Count(`SELECT count(*) FROM group_membership`) != before {
		t.Fatal("a refused split placed someone")
	}
	// A split reaches every student: the grader, listed for HW3 alone, and
	// a student, may not.
	b.refusedAs(t, b.yuki, "group.split", m{"course_id": b.course, "set_id": full, "by": "count", "n": 2, "from": "all"},
		apperr.Forbidden, "")
	// Its proposal records the seed, so that approving it deals as proposed.
	tanaka := b.person(t, "Tanaka", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": tanaka, "preset": "instructor",
		"perms": m{"assignment_write": "confirm_required"}})
	hers := b.groupSet(t, "Tanaka's", nil)
	prop := b.MustCall(tanaka, "group.split", m{"course_id": b.course, "set_id": hers, "by": "size", "n": 3, "from": "all"}, "split-prop")
	if prop.Status != domain.StatusProposed {
		t.Fatalf("Tanaka's split: %+v", prop)
	}
	var seed string
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload->>'seed' FROM action WHERE id = $1`, prop.ActionID).Scan(&seed); err != nil || len(seed) != 12 {
		t.Fatalf("the proposal's seed: %q %v", seed, err)
	}
	approved := b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": prop.ActionID, "decision": "approve"})
	_ = approved
	var dealt string
	if err := b.Pool.QueryRow(t.Context(), `SELECT result->>'seed' FROM action WHERE id = $1`, prop.ActionID).Scan(&dealt); err != nil || dealt != seed {
		t.Fatalf("approved, it dealt with %q, proposed %q: %v", dealt, seed, err)
	}
}

// Students sign themselves up to the groups of a set the teacher opened,
// while it is open and before its deadline, to a group below its capacity,
// never out of or into one that has handed work in.
func TestStudentsSignThemselvesUp(t *testing.T) {
	b := build(t)
	actors, s := b.seatStudents(t, "Aoi")
	aoi := actors["Aoi"]
	set := b.groupSet(t, "Projects", nil)
	g := b.groupsIn(t, set, m{"name": "One", "capacity": 2}, m{"name": "Two", "capacity": 1})
	one, two := g[0], g[1]
	signUp := func(actor uuid.UUID, group *uuid.UUID) tools.GroupSignUpOut {
		args := m{"course_id": b.course, "set_id": set}
		if group != nil {
			args["group_id"] = *group
		}
		return testkit.Result[tools.GroupSignUpOut](t, b.do(t, actor, "group.sign_up", args))
	}

	b.refusedAs(t, b.yuki, "group.sign_up", m{"course_id": b.course, "set_id": set, "group_id": one}, apperr.FailedPrecondition, tools.ReasonSignupClosed)
	list := testkit.Result[tools.GroupSetListOut](t, b.do(t, b.yuki, "group_set.list", m{"course_id": b.course}))
	if list.Sets[0].Signup.Joinable || *list.Sets[0].Signup.Reason != tools.ReasonSignupClosed {
		t.Fatalf("closed sign-up, as Yuki reads it: %+v", list.Sets[0].Signup)
	}
	deadline := time.Now().Add(time.Hour)
	b.do(t, b.sato, "group_set.update", m{"course_id": b.course, "set_id": set, "signup_open": true, "signup_closes_at": deadline})

	if out := signUp(b.yuki, &two); !out.Changed || out.LeftGroupID != nil {
		t.Fatalf("Yuki joining Two: %+v", out)
	}
	b.refusedAs(t, b.ken, "group.sign_up", m{"course_id": b.course, "set_id": set, "group_id": two}, apperr.FailedPrecondition, tools.ReasonGroupFull)
	signUp(b.ken, &one)
	if out := signUp(b.yuki, &one); out.LeftGroupID == nil || *out.LeftGroupID != two || *out.GroupID != one {
		t.Fatalf("Yuki switching to One: %+v", out)
	}
	b.refusedAs(t, aoi, "group.sign_up", m{"course_id": b.course, "set_id": set, "group_id": one}, apperr.FailedPrecondition, tools.ReasonGroupFull)
	if out := signUp(b.yuki, &one); out.Changed {
		t.Fatalf("signing up where she is: %+v", out)
	}
	// Leaving, with no group named.
	if out := signUp(b.ken, nil); !out.Changed || out.GroupID != nil || *out.LeftGroupID != one {
		t.Fatalf("Ken leaving: %+v", out)
	}
	var left string
	if err := b.Pool.QueryRow(t.Context(), `SELECT string_agg(left_how, ',' ORDER BY left_at) FROM group_membership WHERE set_id = $1 AND left_how IS NOT NULL`, set).Scan(&left); err != nil || left != "switched,left" {
		t.Fatalf("how the stays ended: %q %v", left, err)
	}
	// A student signs up nobody else; the teacher's grader, nobody at all.
	b.refusedAs(t, b.yuki, "group.sign_up", m{"course_id": b.course, "set_id": set, "group_id": two, "student_member_id": s["Aoi"]},
		apperr.Forbidden, "")

	// One's work handed in: nobody leaves it, nor joins it, by sign-up.
	hw := b.groupAssignment(t, "Proposal", set)
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw, "body": "Our plan."}))
	if work.GroupID == nil || *work.GroupID != one {
		t.Fatalf("Yuki's group's work: %+v", work)
	}
	signUp(b.ken, &one) // a draft: joining it is fine
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": work.SubmissionID})
	b.refusedAs(t, b.yuki, "group.sign_up", m{"course_id": b.course, "set_id": set, "group_id": two}, apperr.FailedPrecondition, tools.ReasonYourGroupHasWork)
	b.do(t, b.sato, "group.update", m{"course_id": b.course, "group_id": one, "clear_capacity": true})
	b.refusedAs(t, aoi, "group.sign_up", m{"course_id": b.course, "set_id": set, "group_id": one}, apperr.FailedPrecondition, tools.ReasonGroupHasWork)

	// Past the deadline it is the teacher's.
	b.do(t, b.sato, "group_set.update", m{"course_id": b.course, "set_id": set, "signup_closes_at": time.Now().Add(-time.Minute)})
	b.refusedAs(t, aoi, "group.sign_up", m{"course_id": b.course, "set_id": set, "group_id": two}, apperr.FailedPrecondition, tools.ReasonSignupClosed)
}

// A group assignment takes one piece of work from each group: started by any
// member, written by its members together over revisions, handed in by any
// of them for the group's members then, a member another group's work names
// left out; a student in no group hands nothing in.
func TestAGroupHandsInOnePieceOfWork(t *testing.T) {
	b := build(t)
	actors, s := b.seatStudents(t, "Aoi", "Hana")
	aoi, hana := actors["Aoi"], actors["Hana"]
	set := b.groupSet(t, "Projects", m{"signup_open": true})
	g := b.groupsIn(t, set, m{"name": "Team A"}, m{"name": "Team B"})
	teamA, teamB := g[0], g[1]
	b.place(t, set, false, b.yukiM, teamA, b.kenM, teamA, s["Aoi"], teamB)
	hw := b.groupAssignment(t, "Project", set)

	got := testkit.Result[tools.AssignmentView](t, b.do(t, b.yuki, "assignment.get", m{"course_id": b.course, "assignment_id": hw}))
	if got.GroupSetID == nil || *got.GroupSetID != set || got.MyGroup == nil || got.MyGroup.GroupID != teamA || got.MyGroup.Name != "Team A" {
		t.Fatalf("the assignment as Yuki reads it: %+v", got)
	}
	// Hana is in no group: she hands nothing in, and is told she may sign up.
	out, _ := b.Call(hana, "submission.create", m{"course_id": b.course, "assignment_id": hw}, "hana")
	if out.Error == nil || out.Error.Details["reason"] != tools.ReasonNoGroup || out.Error.Details["signup_open"] != true ||
		fmt.Sprint(out.Error.Details["set_id"]) != set.String() {
		t.Fatalf("Hana starting work: %+v", out)
	}

	// Yuki starts Team A's work; Ken, its other member, is told it is open.
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw, "body": "Outline"}))
	if work.GroupID == nil || *work.GroupID != teamA || work.Attempt != 1 {
		t.Fatalf("Team A's work: %+v", work)
	}
	b.refusedAs(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": hw}, apperr.Conflict, "")

	// They write it together, over revisions.
	b.refusedAs(t, b.ken, "submission.update_draft", m{"course_id": b.course, "submission_id": work.SubmissionID, "body": "x"},
		apperr.InvalidArgument, tools.ReasonBaseRevisionRequired)
	edit := testkit.Result[tools.SubmissionUpdateDraftOut](t, b.do(t, b.ken, "submission.update_draft",
		m{"course_id": b.course, "submission_id": work.SubmissionID, "body": "Outline, and a method", "base_revision": 1}))
	if !edit.OK || edit.Revision != 2 {
		t.Fatalf("Ken's edit: %+v", edit)
	}
	stale, _ := b.Call(b.yuki, "submission.update_draft", m{"course_id": b.course, "submission_id": work.SubmissionID,
		"body": "Outline only", "base_revision": 1}, "stale")
	if stale.Error == nil || stale.Error.Code != apperr.Conflict || stale.Error.Details["reason"] != tools.ReasonDraftChanged ||
		fmt.Sprint(stale.Error.Details["current_revision"]) != "2" || fmt.Sprint(stale.Error.Details["revised_by_member_id"]) != b.kenM.String() {
		t.Fatalf("Yuki's stale edit: %+v", stale)
	}
	view := testkit.Result[tools.SubmissionView](t, b.do(t, b.yuki, "submission.get", m{"course_id": b.course, "submission_id": work.SubmissionID}))
	if view.StudentMemberID != nil || view.GroupName == nil || *view.GroupName != "Team A" || view.Revision != 2 ||
		view.RevisedByMemberID == nil || *view.RevisedByMemberID != b.kenM || len(view.Members) != 2 || view.Members[0].DisplayName == nil {
		t.Fatalf("Team A's draft as Yuki reads it: %+v", view)
	}
	// Aoi, of Team B, reads nothing of it.
	b.refusedAs(t, aoi, "submission.get", m{"course_id": b.course, "submission_id": work.SubmissionID}, apperr.Forbidden, "student_out_of_scope")
	if subs := testkit.Result[tools.SubmissionListOut](t, b.do(t, aoi, "submission.list", m{"course_id": b.course})).Submissions; len(subs) != 0 {
		t.Fatalf("Aoi lists %+v", subs)
	}

	// Ken moves to Team B: refused without affects_work, which names the
	// work; then the draft follows the group, and he no longer reaches it.
	moved, _ := b.Call(b.sato, "group.set_members", m{"course_id": b.course, "set_id": set,
		"placements": []m{{"student_member_id": b.kenM, "group_id": teamB}}}, "move-ken")
	if moved.Error == nil || moved.Error.Details["reason"] != tools.ReasonGroupHasWork {
		t.Fatalf("moving Ken without affects_work: %+v", moved)
	}
	b.place(t, set, true, b.kenM, teamB)
	b.refusedAs(t, b.ken, "submission.get", m{"course_id": b.course, "submission_id": work.SubmissionID}, apperr.Forbidden, "student_out_of_scope")

	// Yuki hands it in: for Team A's members now, her alone.
	handed := testkit.Result[tools.SubmissionSubmitOut](t, b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": work.SubmissionID}))
	if !slices.Equal(handed.Members, []uuid.UUID{b.yukiM}) || len(handed.LeftOut) != 0 {
		t.Fatalf("Team A's hand-in: %+v", handed)
	}
	// Yuki moves to Team B as well; Team B's hand-in leaves her out, since
	// Team A's work names her.
	b.place(t, set, true, b.yukiM, teamB)
	bWork := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, aoi, "submission.create", m{"course_id": b.course, "assignment_id": hw, "body": "Team B's"}))
	bHanded := testkit.Result[tools.SubmissionSubmitOut](t, b.do(t, b.ken, "submission.submit", m{"course_id": b.course, "submission_id": bWork.SubmissionID}))
	if !sameSet(bHanded.Members, []uuid.UUID{s["Aoi"], b.kenM}) || len(bHanded.LeftOut) != 1 || bHanded.LeftOut[0].MemberID != b.yukiM ||
		bHanded.LeftOut[0].SubmissionID != work.SubmissionID {
		t.Fatalf("Team B's hand-in: %+v", bHanded)
	}
	// Yuki still reads Team A's work, which is hers; Team B's is not.
	b.do(t, b.yuki, "submission.get", m{"course_id": b.course, "submission_id": work.SubmissionID})
	b.refusedAs(t, b.yuki, "submission.get", m{"course_id": b.course, "submission_id": bWork.SubmissionID}, apperr.Forbidden, "student_out_of_scope")

	// Hana, placed in Team C, which hands nothing in, is recorded missing
	// by group; naming a student is refused on a group assignment.
	teamC := b.groupsIn(t, set, m{"name": "Team C"})[0]
	b.place(t, set, false, s["Hana"], teamC)
	b.refusedAs(t, b.sato, "submission.record_missing", m{"course_id": b.course, "assignment_id": hw, "student_member_id": s["Hana"]},
		apperr.FailedPrecondition, tools.ReasonGroupAssignment)
	missing := testkit.Result[tools.SubmissionIDOut](t, b.do(t, b.sato, "submission.record_missing", m{"course_id": b.course, "assignment_id": hw, "group_id": teamC}))
	mv := testkit.Result[tools.SubmissionView](t, b.do(t, hana, "submission.get", m{"course_id": b.course, "submission_id": missing.SubmissionID}))
	if mv.State != "missing" || len(mv.Members) != 1 || mv.Members[0].MemberID != s["Hana"] {
		t.Fatalf("Team C's missing record: %+v", mv)
	}

	// Where everyone stands.
	roster := testkit.Result[tools.SubmissionRosterOut](t, b.do(t, b.sato, "submission.roster", m{"course_id": b.course, "assignment_id": hw}))
	states := map[uuid.UUID]string{}
	for _, e := range roster.Students {
		states[e.StudentMemberID] = e.State
	}
	if states[b.yukiM] != "submitted" || states[b.kenM] != "submitted" || states[s["Aoi"]] != "submitted" || states[s["Hana"]] != "missing" {
		t.Fatalf("the roster's students: %v", states)
	}
	groupStates := map[uuid.UUID]string{}
	for _, gr := range roster.Groups {
		groupStates[gr.GroupID] = gr.State
	}
	if groupStates[teamA] != "submitted" || groupStates[teamB] != "submitted" || groupStates[teamC] != "missing" {
		t.Fatalf("the roster's groups: %v", groupStates)
	}
	// A student in no group: no_group.
	_, extra := b.seatStudents(t, "Mio")
	roster = testkit.Result[tools.SubmissionRosterOut](t, b.do(t, b.sato, "submission.roster", m{"course_id": b.course, "assignment_id": hw}))
	for _, e := range roster.Students {
		if e.StudentMemberID == extra["Mio"] && e.State != "no_group" {
			t.Fatalf("Mio, in no group: %+v", e)
		}
	}

	// Whether it is group work no longer changes.
	b.refusedAs(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": hw, "clear_group_set": true},
		apperr.FailedPrecondition, tools.ReasonAssignmentHasWork)
	// group_id is for group work only.
	b.refusedAs(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "group_id": teamA},
		apperr.FailedPrecondition, tools.ReasonNotAGroupAssignment)
	// An individual assignment made a group one before anyone starts, and
	// back.
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "group_set_id": set})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "clear_group_set": true})
}

// A group's work is graded once: a group grade, and each member given a
// grade from it, the group's score unless adjusted, with a reason the member
// reads; posted per member; changed for one member alone, or regraded for
// the group, adjustments carried.
func TestAGroupIsGradedOnceAndMembersAdjusted(t *testing.T) {
	b := build(t)
	actors, s := b.seatStudents(t, "Aoi")
	aoi := actors["Aoi"]
	set := b.groupSet(t, "Projects", nil)
	team := b.groupsIn(t, set, m{"name": "Team"})[0]
	b.place(t, set, false, b.yukiM, team, b.kenM, team, s["Aoi"], team)
	hw := b.groupAssignment(t, "Project", set)
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": hw, "body": "Report"}))
	b.do(t, b.ken, "submission.submit", m{"course_id": b.course, "submission_id": work.SubmissionID})
	grade := func(args m) tools.GradeSubmitOut {
		args["course_id"], args["submission_id"] = b.course, work.SubmissionID
		return testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", args))
	}
	score := func(out []tools.MemberGradeOut, who uuid.UUID) (decimal.Decimal, *tools.AdjustmentView, uuid.UUID) {
		for _, mg := range out {
			if mg.StudentMemberID == who {
				return mg.Score, mg.Adjustment, mg.GradeID
			}
		}
		t.Fatalf("no grade for %s in %+v", who, out)
		return decimal.Zero, nil, uuid.Nil
	}

	// Refused: an adjustment of someone not of the work, and one that would
	// go below zero or above the points possible.
	b.refusedAs(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work.SubmissionID, "score": 80,
		"adjustments": []m{{"student_member_id": b.graderM, "kind": "delta", "points": 5, "reason": "x"}}},
		apperr.FailedPrecondition, tools.ReasonNotAMemberOfWork)
	b.refusedAs(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work.SubmissionID, "score": 80,
		"adjustments": []m{{"student_member_id": b.kenM, "kind": "delta", "points": -90, "reason": "Absent"}}},
		apperr.FailedPrecondition, tools.ReasonAdjustedBelowZero)
	b.refusedAs(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work.SubmissionID, "score": 80,
		"adjustments": []m{{"student_member_id": b.kenM, "kind": "delta", "points": 30, "reason": "Led it"}}},
		apperr.FailedPrecondition, tools.ReasonAdjustedAbovePoints)
	b.refusedAs(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": b.submit(t, b.yuki, "essay"), "score": 80,
		"adjustments": []m{{"student_member_id": b.yukiM, "kind": "delta", "points": 3, "reason": "x"}}},
		apperr.FailedPrecondition, tools.ReasonNotAGroupAssignment)

	// 80 for the group, Ken 10 less, with a reason.
	first := grade(m{"score": 80, "feedback": "A clear report.", "adjustments": []m{
		{"student_member_id": b.kenM, "kind": "delta", "points": -10, "reason": "Missed two meetings"}}})
	if first.GroupGradeID == nil || len(first.MemberGrades) != 3 || first.GradeID != uuid.Nil {
		t.Fatalf("the group's grade: %+v", first)
	}
	if sc, adj, _ := score(first.MemberGrades, b.kenM); !sc.Equal(decimal.NewFromInt(70)) || adj == nil || adj.Kind != "delta" {
		t.Fatalf("Ken's grade: %s %+v", sc, adj)
	}
	// A new draft carries Ken's adjustment; Aoi's replaced score is new.
	second := grade(m{"score": 84, "adjustments": []m{{"student_member_id": s["Aoi"], "kind": "replace", "points": 90, "reason": "Wrote most of it"}}})
	kenScore, kenAdj, _ := score(second.MemberGrades, b.kenM)
	aoiScore, _, _ := score(second.MemberGrades, s["Aoi"])
	yukiScore, yukiAdj, _ := score(second.MemberGrades, b.yukiM)
	if !kenScore.Equal(decimal.NewFromInt(74)) || kenAdj == nil || *kenAdj.Reason != "Missed two meetings" ||
		!aoiScore.Equal(decimal.NewFromInt(90)) || !yukiScore.Equal(decimal.NewFromInt(84)) || yukiAdj != nil {
		t.Fatalf("the second draft: Ken %s %+v, Aoi %s, Yuki %s %+v", kenScore, kenAdj, aoiScore, yukiScore, yukiAdj)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE submission_id = $1 AND posted_at IS NULL AND superseded_by IS NULL`, work.SubmissionID); n != 3 {
		t.Fatalf("%d live drafts, want one per member", n)
	}

	// Yuki's posted alone: the group is not regraded while a grade from its
	// group grade is still a draft.
	_, _, yukiDraft := score(second.MemberGrades, b.yukiM)
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{yukiDraft}})
	b.refusedAs(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": yukiDraft, "score": 50},
		apperr.FailedPrecondition, tools.ReasonGroupGradePartlyPosted)

	// Posted, each reads their own: the group's score, their adjustment and
	// why, never who made it; nobody reads another's.
	posted := testkit.Result[tools.GradePostOut](t, b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": hw}))
	if len(posted.Posted) != 2 {
		t.Fatalf("posted %+v", posted)
	}
	mine := testkit.Result[tools.GradeListOut](t, b.do(t, b.ken, "grade.list", m{"course_id": b.course, "assignment_id": hw})).Grades
	if len(mine) != 1 || mine[0].StudentMemberID != b.kenM || mine[0].Group == nil || !mine[0].Group.Score.Equal(decimal.NewFromInt(84)) ||
		mine[0].Group.Adjustment == nil || mine[0].Group.Adjustment.Reason == nil || mine[0].Group.Adjustment.ByMemberID != nil ||
		mine[0].Group.GroupName == nil || *mine[0].Group.GroupName != "Team" {
		t.Fatalf("Ken's grades: %+v", mine)
	}
	_, _, aoiGrade := score(second.MemberGrades, s["Aoi"])
	b.refusedAs(t, b.ken, "grade.get", m{"course_id": b.course, "grade_id": aoiGrade}, apperr.Forbidden, "student_out_of_scope")
	staff := testkit.Result[tools.GradeView](t, b.do(t, b.sato, "grade.get", m{"course_id": b.course, "grade_id": aoiGrade}))
	if staff.Group == nil || staff.Group.Adjustment == nil || staff.Group.Adjustment.ByMemberID == nil || *staff.Group.Adjustment.ByMemberID != b.satoM {
		t.Fatalf("Aoi's grade as Sato reads it: %+v", staff.Group)
	}

	// One member alone: Yuki's posted grade adjusted, a new posted grade,
	// her totals written again.
	_, _, yukiGrade := score(second.MemberGrades, b.yukiM)
	adj := testkit.Result[tools.GradeAdjustOut](t, b.do(t, b.sato, "grade.adjust", m{"course_id": b.course, "grade_id": yukiGrade,
		"kind": "delta", "points": 6, "reason": "Presented it"}))
	if !adj.Changed || !adj.Score.Equal(decimal.NewFromInt(90)) || adj.Replaces == nil || *adj.Replaces != yukiGrade || adj.Snapshots == 0 {
		t.Fatalf("Yuki's adjustment: %+v", adj)
	}
	if again := testkit.Result[tools.GradeAdjustOut](t, b.do(t, b.sato, "grade.adjust", m{"course_id": b.course, "grade_id": adj.GradeID,
		"kind": "delta", "points": 6, "reason": "Presented it"})); again.Changed {
		t.Fatalf("the same adjustment again: %+v", again)
	}
	b.refusedAs(t, b.sato, "grade.adjust", m{"course_id": b.course, "grade_id": yukiGrade, "kind": "none"}, apperr.Conflict, "")
	// A grade not from a group grade is not adjusted.
	plain := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "component_id": b.midterm,
		"student_member_id": b.yukiM, "score": 70})).GradeID
	b.refusedAs(t, b.sato, "grade.adjust", m{"course_id": b.course, "grade_id": plain, "kind": "none"},
		apperr.FailedPrecondition, tools.ReasonNotFromAGroupGrade)
	// The grader, who may not post, adjusts no posted grade.
	b.refusedAs(t, b.grader, "grade.adjust", m{"course_id": b.course, "grade_id": adj.GradeID, "kind": "none"}, apperr.Forbidden, "")

	// Regrading Ken's regrades the group's: a new group grade, each member's
	// posted grade written again, adjustments carried.
	_, _, kenGrade := score(second.MemberGrades, b.kenM)
	re := testkit.Result[tools.GradeRegradeOut](t, b.do(t, b.sato, "grade.regrade", m{"course_id": b.course, "grade_id": kenGrade, "score": 88}))
	if re.GroupGradeID == nil || *re.GroupGradeID == *second.GroupGradeID || len(re.MemberGrades) != 3 || re.Replaces != kenGrade {
		t.Fatalf("the group's regrade: %+v", re)
	}
	kenScore, _, newKen := score(re.MemberGrades, b.kenM)
	aoiScore, _, _ = score(re.MemberGrades, s["Aoi"])
	yukiScore, _, _ = score(re.MemberGrades, b.yukiM)
	if newKen != re.GradeID || !kenScore.Equal(decimal.NewFromInt(78)) || !aoiScore.Equal(decimal.NewFromInt(90)) || !yukiScore.Equal(decimal.NewFromInt(94)) {
		t.Fatalf("after the regrade: Ken %s Aoi %s Yuki %s", kenScore, aoiScore, yukiScore)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE submission_id = $1 AND posted_at IS NOT NULL AND superseded_by IS NULL`, work.SubmissionID); n != 3 {
		t.Fatalf("%d live posted grades, want one per member", n)
	}
	// Aoi reads her grade and the group's feedback, not the others'.
	aoiGrades := testkit.Result[tools.GradeListOut](t, b.do(t, aoi, "grade.list", m{"course_id": b.course, "assignment_id": hw})).Grades
	if len(aoiGrades) != 1 || aoiGrades[0].StudentMemberID != s["Aoi"] || !aoiGrades[0].Score.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("Aoi's grades: %+v", aoiGrades)
	}
}

// Whose work it is is corrected by those who grade: a student the group
// handed it in without is added, and given the group's grade; a member with
// a grade is not taken off.
func TestWhoseWorkItIsIsCorrected(t *testing.T) {
	b := build(t)
	_, s := b.seatStudents(t, "Aoi", "Mio")
	set := b.groupSet(t, "Projects", nil)
	g := b.groupsIn(t, set, m{"name": "Team A"}, m{"name": "Team B"})
	b.place(t, set, false, b.yukiM, g[0], b.kenM, g[0], s["Aoi"], g[1])
	hw := b.groupAssignment(t, "Project", set)
	a := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw, "body": "A"}))
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": a.SubmissionID})
	bw := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.sato, "submission.create", m{"course_id": b.course, "assignment_id": hw, "group_id": g[1], "body": "B"}))
	b.do(t, b.sato, "submission.submit", m{"course_id": b.course, "submission_id": bw.SubmissionID})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": a.SubmissionID, "score": 75})

	// Mio, whom Sato forgot to place, is added to Team A's work.
	out := testkit.Result[tools.SubmissionSetMembersOut](t, b.do(t, b.sato, "submission.set_members",
		m{"course_id": b.course, "submission_id": a.SubmissionID, "add": []uuid.UUID{s["Mio"]}}))
	if !sameSet(out.Members, []uuid.UUID{b.yukiM, b.kenM, s["Mio"]}) {
		t.Fatalf("Team A's work, corrected: %+v", out)
	}
	// Aoi is part of Team B's work: not of Team A's too.
	b.refusedAs(t, b.sato, "submission.set_members", m{"course_id": b.course, "submission_id": a.SubmissionID, "add": []uuid.UUID{s["Aoi"]}},
		apperr.FailedPrecondition, tools.ReasonPartOfOtherWork)
	// Ken has a grade on it: he stays.
	b.refusedAs(t, b.sato, "submission.set_members", m{"course_id": b.course, "submission_id": a.SubmissionID, "remove": []uuid.UUID{b.kenM}},
		apperr.FailedPrecondition, tools.ReasonMemberGraded)
	// Nobody left is refused.
	b.refusedAs(t, b.sato, "submission.set_members", m{"course_id": b.course, "submission_id": bw.SubmissionID, "remove": []uuid.UUID{s["Aoi"]}},
		apperr.FailedPrecondition, tools.ReasonGroupEmpty)
	// A student declares nothing of it.
	b.refusedAs(t, b.yuki, "submission.set_members", m{"course_id": b.course, "submission_id": a.SubmissionID, "remove": []uuid.UUID{s["Mio"]}},
		apperr.Forbidden, "")
	// Mio, taken off again before she is graded; added once more and graded
	// with the group.
	b.do(t, b.sato, "submission.set_members", m{"course_id": b.course, "submission_id": a.SubmissionID, "remove": []uuid.UUID{s["Mio"]}})
	b.do(t, b.sato, "submission.set_members", m{"course_id": b.course, "submission_id": a.SubmissionID, "add": []uuid.UUID{s["Mio"]}})
	graded := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": a.SubmissionID, "score": 75}))
	if len(graded.MemberGrades) != 3 {
		t.Fatalf("Team A's grade after the correction: %+v", graded)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'submission.members_changed' AND student_member_id = $1`, s["Mio"]); n != 3 {
		t.Fatalf("%d changes told Mio, want 3", n)
	}
	// The how of it is kept.
	if n := b.Count(`SELECT count(*) FROM submission_member WHERE submission_id = $1 AND added_how = 'corrected' AND added_by_member_id = $2`,
		a.SubmissionID, b.satoM); n != 1 {
		t.Fatalf("%d corrected rows", n)
	}
}
