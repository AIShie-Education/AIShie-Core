package tools_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// tree is testkit.DeptTree with the checks these tests make of a call.
type tree struct {
	*testkit.DeptTree
	t *testing.T
}

func newTree(t *testing.T) tree { return tree{testkit.NewDeptTree(t), t} }

// do calls a tool and insists it executed.
func (w tree) do(actor uuid.UUID, name string, args m) pipeline.Outcome {
	w.t.Helper()
	out := w.MustCall(actor, name, args, "k-"+uuid.NewString())
	if out.Status != domain.StatusExecuted {
		w.t.Fatalf("%s: %+v", name, out)
	}
	return out
}

// denied calls a tool and insists it was denied for reason, and that a write
// was denied on the record, in no capacity.
func (w tree) denied(actor uuid.UUID, name string, args m, reason string) {
	w.t.Helper()
	out := w.MustCall(actor, name, args, "k-"+uuid.NewString())
	if out.Status != domain.StatusDenied || out.Error == nil || out.Error.Details["reason"] != reason {
		w.t.Fatalf("%s: %+v, want denied %s", name, out, reason)
	}
	if out.ActionID != nil && w.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'denied'
		AND authz_result = 'denied' AND authority IS NULL AND authority_dept_id IS NULL`, *out.ActionID) != 1 {
		w.t.Fatalf("%s: the denial is not on record as one", name)
	}
}

// fails calls a tool and insists it was refused by a rule, with code and,
// unless reason is empty, that details.reason, and recorded as failed.
func (w tree) fails(actor uuid.UUID, name string, args m, code apperr.Code, reason string) {
	w.t.Helper()
	out := w.MustCall(actor, name, args, "k-"+uuid.NewString())
	if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != code ||
		reason != "" && out.Error.Details["reason"] != reason {
		w.t.Fatalf("%s: %+v, want failed %s (%s)", name, out, code, reason)
	}
}

// capacity is what an action's row says it was allowed in.
func (w tree) capacity(out pipeline.Outcome) (authority string, dept uuid.UUID) {
	w.t.Helper()
	var a *string
	var d *uuid.UUID
	if err := w.Pool.QueryRow(w.t.Context(), `SELECT authority, authority_dept_id FROM action WHERE id = $1`, *out.ActionID).Scan(&a, &d); err != nil {
		w.t.Fatal(err)
	}
	if a != nil {
		authority = *a
	}
	if d != nil {
		dept = *d
	}
	return authority, dept
}

func (w tree) wantCapacity(out pipeline.Outcome, authority string, dept uuid.UUID) {
	w.t.Helper()
	if a, d := w.capacity(out); a != authority || d != dept {
		w.t.Fatalf("recorded as made in the capacity %q of %v, want %q of %v", a, d, authority, dept)
	}
}

// A platform administrator makes the tree and staffs it, anywhere, and is
// recorded as having done so by a platform role.
func TestAPlatformAdministratorBuildsTheTree(t *testing.T) {
	w := newTree(t)
	made := w.do(w.Admin, "department.create", m{"name": "  Medicine  "})
	w.wantCapacity(made, domain.AuthorityPlatform, uuid.Nil)
	med := testkit.Result[tools.IDOut](t, made).ID
	nursing := testkit.Result[tools.IDOut](t, w.do(w.Admin, "department.create", m{"name": "Nursing", "parent_id": med})).ID
	if n := w.Count(`SELECT count(*) FROM department WHERE id = $1 AND name = 'Medicine' AND parent_id IS NULL`, med); n != 1 {
		t.Fatal("the department at the top was not made as asked, its name trimmed")
	}
	if n := w.Count(`SELECT count(*) FROM event WHERE type = 'department.created' AND subject_id = $1
		AND course_id IS NULL AND payload->>'parent_id' = $2`, nursing, med.String()); n != 1 {
		t.Fatal("no department.created, outside any course, naming the parent")
	}

	appointed := w.do(w.Admin, "department.add_admin", m{"dept_id": med, "actor_id": w.Dan})
	w.wantCapacity(appointed, domain.AuthorityPlatform, uuid.Nil)
	out := testkit.Result[tools.DepartmentAddAdminOut](t, appointed)
	if out.CoveredAbove {
		t.Fatal("an appointment at the top is covered by nothing above it")
	}
	if n := w.Count(`SELECT count(*) FROM department_admin WHERE id = $1 AND dept_id = $2 AND actor_id = $3
		AND appointed_by_actor_id = $4 AND removed_at IS NULL`, out.AppointmentID, med, w.Dan, w.Admin); n != 1 {
		t.Fatal("the appointment is not recorded as made")
	}
	// Dan now administers Medicine, and makes a department beneath it.
	mine := testkit.Result[tools.MeOut](t, w.do(w.Dan, "me.get", m{}))
	if len(mine.Administers) != 1 || mine.Administers[0].DeptID != med || mine.Administers[0].Name != "Medicine" ||
		mine.Administers[0].AppointmentID != out.AppointmentID {
		t.Fatalf("Dan's me.get: %+v", mine.Administers)
	}
	w.wantCapacity(w.do(w.Dan, "department.create", m{"name": "Pharmacy", "parent_id": med}), domain.AuthorityDepartment, med)

	// Beneath a department nested as deep as the tree goes, no further.
	parent := nursing
	for _, name := range []string{"3", "4", "5", "6", "7", "8"} {
		parent = testkit.Result[tools.IDOut](t, w.do(w.Admin, "department.create", m{"name": name, "parent_id": parent})).ID
	}
	out9 := w.MustCall(w.Admin, "department.create", m{"name": "9", "parent_id": parent}, "nine")
	if out9.Status != domain.StatusFailed || out9.Error.Code != apperr.FailedPrecondition ||
		out9.Error.Details["reason"] != "too_deep" || out9.Error.Details["max_depth"] != domain.MaxDepartmentDepth {
		t.Fatalf("a ninth level: %+v", out9)
	}
	// Sibling names are unique, in any case; the same name elsewhere is not a sibling's.
	w.fails(w.Admin, "department.create", m{"name": "medicine"}, apperr.Conflict, "name_taken")
	w.fails(w.Admin, "department.create", m{"name": "NURSING", "parent_id": med}, apperr.Conflict, "name_taken")
	w.do(w.Admin, "department.create", m{"name": "Nursing", "parent_id": w.F})
	w.fails(w.Admin, "department.create", m{"name": " "}, apperr.InvalidArgument, "")
	if _, err := w.Call(w.Admin, "department.create", m{"name": "Orphan", "parent_id": uuid.New()}, "orphan"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("under a department that does not exist: %v", err)
	}
	w.everyEmittedTypeHasARule()
}

// A department administrator makes departments anywhere beneath an
// appointment of theirs, and is recorded as doing it by the nearest one; at
// the top of the tree, or anywhere they do not cover, they are refused.
func TestADepartmentAdministratorCreatesBeneathTheirAppointment(t *testing.T) {
	w := newTree(t)
	underD := w.do(w.Ada, "department.create", m{"name": "Robotics", "parent_id": w.D})
	w.wantCapacity(underD, domain.AuthorityDepartment, w.F)
	underF := w.do(w.Ada, "department.create", m{"name": "Materials", "parent_id": w.F})
	w.wantCapacity(underF, domain.AuthorityDepartment, w.F)
	w.wantCapacity(w.do(w.Bob, "department.create", m{"name": "Vision", "parent_id": w.D}), domain.AuthorityDepartment, w.S)
	// With an appointment of her own nearer, that is the one she relies on.
	w.Appoint(w.S, w.Ada, w.Root)
	w.wantCapacity(w.do(w.Ada, "department.create", m{"name": "Speech", "parent_id": w.D}), domain.AuthorityDepartment, w.S)

	w.denied(w.Ada, "department.create", m{"name": "Linguistics", "parent_id": w.F2}, "department_out_of_scope")
	w.denied(w.Bob, "department.create", m{"name": "Ceramics", "parent_id": w.S2}, "department_out_of_scope")
	w.denied(w.Bob, "department.create", m{"name": "Chemical", "parent_id": w.F}, "department_out_of_scope")
	w.denied(w.Ada, "department.create", m{"name": "Law"}, "platform_role_required")
	// Nobody else gets as far as asking which department.
	w.denied(w.Yuki, "department.create", m{"name": "Vision 2", "parent_id": w.D}, "platform_role_required")
	w.denied(w.Robo, "department.create", m{"name": "Vision 3", "parent_id": w.D}, "platform_role_required")
	w.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, w.Bob)
	w.denied(w.Bob, "department.create", m{"name": "Vision 4", "parent_id": w.D}, "actor_not_active")
	w.everyEmittedTypeHasARule()
}

// docs/schema.md §2.10: a department administrator is nobody outside the
// tree. Everything that stays a platform administrator's refuses them as it
// refuses anyone, and a write's refusal is on record.
func TestADepartmentAdministratorManagesNoAccountAndNothingPlatformWide(t *testing.T) {
	w := newTree(t)
	var credential uuid.UUID
	if err := w.Pool.QueryRow(t.Context(), `INSERT INTO credential (actor_id, kind, secret_hash, token_prefix)
		VALUES ($1, 'api_token', 'h', 'dan-1') RETURNING id`, w.Dan).Scan(&credential); err != nil {
		t.Fatal(err)
	}
	var preset uuid.UUID
	if err := w.Pool.QueryRow(t.Context(), `INSERT INTO permission_preset (dept_id, name, role, student_scope, assignment_scope)
		VALUES ($1, 'lab-ta', 'ta', 'all', 'all') RETURNING id`, w.F).Scan(&preset); err != nil {
		t.Fatal(err)
	}
	body := m{"role": "ta", "student_scope": "all", "assignment_scope": "all", "perms": m{}}
	for name, args := range map[string]m{
		"actor.suspend":           {"actor_id": w.Dan},
		"actor.reactivate":        {"actor_id": w.Dan},
		"actor.update":            {"actor_id": w.Dan, "display_name": "Daniel"},
		"actor.set_owner":         {"actor_id": w.Robo, "owner_actor_id": w.Dan},
		"actor.issue_token":       {"actor_id": w.Dan, "label": "mine now"},
		"actor.link_sso":          {"actor_id": w.Dan, "provider": "polyu-adfs", "subject": "dan@example.edu"},
		"actor.list":              {},
		"actor.get":               {"actor_id": w.Dan},
		"actor.list_credentials":  {"actor_id": w.Dan},
		"actor.revoke_credential": {"actor_id": w.Dan, "credential_id": credential},
		"actor.register":          {"kind": "human", "display_name": "Pat"},
		"term.create":             {"name": "2027 Spring", "starts_on": "2027-01-10", "ends_on": "2027-05-20"},
		"preset.create":           merge(m{"dept_id": w.F, "name": "lab-tutor"}, body),
		"preset.update":           merge(m{"preset_id": preset}, body),
		"department.create":       {"name": "Law"},
	} {
		t.Run(name, func(t *testing.T) {
			w := tree{w.DeptTree, t}
			w.denied(w.Ada, name, args, "platform_role_required")
		})
	}
	// Staffing her own appointment's department is not hers either: that is
	// whoever administers the department above it.
	w.denied(w.Ada, "department.add_admin", m{"dept_id": w.F, "actor_id": w.Dan}, "department_out_of_scope")
}

func merge(a, b m) m {
	out := m{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// §9.3 of the spec, "Appointments": an administrator staffs what is strictly
// beneath an appointment of theirs, a platform administrator anywhere, and
// nobody their own department, one above it, or themselves.
func TestAppointmentsAreMadeStrictlyBeneath(t *testing.T) {
	w := newTree(t)

	danS := w.do(w.Ada, "department.add_admin", m{"dept_id": w.S, "actor_id": w.Dan})
	w.wantCapacity(danS, domain.AuthorityDepartment, w.F)
	if out := testkit.Result[tools.DepartmentAddAdminOut](t, danS); out.CoveredAbove {
		t.Fatal("Dan was already covered above Computing")
	}
	w.fails(w.Ada, "department.add_admin", m{"dept_id": w.S, "actor_id": w.Bob}, apperr.Conflict, "already_admin")

	w.wantCapacity(w.do(w.Dan, "department.add_admin", m{"dept_id": w.D, "actor_id": w.Eve}), domain.AuthorityDepartment, w.S)
	w.denied(w.Dan, "department.add_admin", m{"dept_id": w.S, "actor_id": w.Eve}, "department_out_of_scope")

	w.denied(w.Bob, "department.remove_admin", m{"dept_id": w.F, "actor_id": w.Ada}, "department_out_of_scope")
	w.denied(w.Bob, "department.remove_admin", m{"dept_id": w.S, "actor_id": w.Chan}, "department_out_of_scope")

	w.denied(w.Ada, "department.add_admin", m{"dept_id": w.F, "actor_id": w.Dan}, "department_out_of_scope")
	w.wantCapacity(w.do(w.Admin, "department.add_admin", m{"dept_id": w.F, "actor_id": w.Dan}), domain.AuthorityPlatform, uuid.Nil)
	w.do(w.Admin, "department.add_admin", m{"dept_id": w.U, "actor_id": w.Dan})

	removed := w.do(w.Ada, "department.remove_admin", m{"dept_id": w.S, "actor_id": w.Dan})
	w.wantCapacity(removed, domain.AuthorityDepartment, w.F)
	if n := w.Count(`SELECT count(*) FROM department_admin WHERE dept_id = $1 AND actor_id = $2
		AND removed_at IS NOT NULL AND removed_by_actor_id = $3`, w.S, w.Dan, w.Ada); n != 1 {
		t.Fatal("the ended appointment is not kept, saying who ended it")
	}
	if n := w.Count(`SELECT count(*) FROM event WHERE type = 'department.admin_removed' AND subject_id = $1
		AND payload->>'actor_id' = $2`, w.S, w.Dan.String()); n != 1 {
		t.Fatal("no department.admin_removed")
	}
	w.fails(w.Ada, "department.remove_admin", m{"dept_id": w.S, "actor_id": w.Dan}, apperr.NotFound, "not_admin")

	w.fails(w.Ada, "department.add_admin", m{"dept_id": w.S, "actor_id": w.Ada}, apperr.Forbidden, "self_appointment")
	w.fails(w.Ada, "department.add_admin", m{"dept_id": w.S, "actor_id": w.Robo}, apperr.FailedPrecondition, "not_a_person")
	w.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, w.Eve)
	w.fails(w.Ada, "department.add_admin", m{"dept_id": w.S2, "actor_id": w.Eve}, apperr.FailedPrecondition, "actor_suspended")
	if _, err := w.Call(w.Ada, "department.add_admin", m{"dept_id": w.S, "actor_id": uuid.New()}, "nobody"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("appointing an actor who does not exist: %v", err)
	}

	// Appointed where they already administer from above, and told so.
	again := testkit.Result[tools.DepartmentAddAdminOut](t, w.do(w.Admin, "department.add_admin", m{"dept_id": w.D, "actor_id": w.Bob}))
	if !again.CoveredAbove {
		t.Fatal("Bob administers AI through Computing, and was not told so")
	}
	w.everyEmittedTypeHasARule()
}

// Ending an appointment takes effect on the very next call.
func TestAnEndedAppointmentAllowsNothingMore(t *testing.T) {
	w := newTree(t)
	w.do(w.Ada, "department.create", m{"name": "Robotics", "parent_id": w.D})
	w.do(w.Admin, "department.remove_admin", m{"dept_id": w.F, "actor_id": w.Ada})
	w.denied(w.Ada, "department.create", m{"name": "Vision", "parent_id": w.D}, "platform_role_required")
	if me := testkit.Result[tools.MeOut](t, w.do(w.Ada, "me.get", m{})); len(me.Administers) != 0 {
		t.Fatalf("Ada's me.get after her appointment ended: %+v", me.Administers)
	}
	// Another appointment still stands on its own.
	w.Appoint(w.S2, w.Ada, w.Root)
	w.denied(w.Ada, "department.create", m{"name": "Vision", "parent_id": w.D}, "department_out_of_scope")
	w.do(w.Ada, "department.create", m{"name": "Ceramics", "parent_id": w.S2})
}

func TestWhoAdministersADepartment(t *testing.T) {
	w := newTree(t)
	names := func(actor uuid.UUID, args m) []string {
		t.Helper()
		var got []string
		for _, a := range testkit.Result[tools.DepartmentAdminsOut](t, w.do(actor, "department.list_admins", args)).Admins {
			got = append(got, a.DisplayName+"@"+a.DeptName)
		}
		return got
	}
	if got, want := names(w.Bob, m{"dept_id": w.S}), []string{"Bob@Computing", "Chan@Computing"}; !slices.Equal(got, want) {
		t.Fatalf("Computing's own: %v, want %v", got, want)
	}
	// Nearest first, then by name; one's own included.
	w.Appoint(w.U, w.Eve, w.Root)
	if got, want := names(w.Chan, m{"dept_id": w.D, "inherited": true}),
		[]string{"Bob@Computing", "Chan@Computing", "Ada@Engineering", "Eve@University"}; !slices.Equal(got, want) {
		t.Fatalf("AI's, inherited: %v, want %v", got, want)
	}
	if got := names(w.Ada, m{"dept_id": w.D}); got != nil {
		t.Fatalf("AI's own: %v, want none", got)
	}
	// Those above see it; a sibling's, or one below, does not.
	w.denied(w.Bob, "department.list_admins", m{"dept_id": w.F}, "department_out_of_scope")
	w.denied(w.Carol, "department.list_admins", m{"dept_id": w.S}, "department_out_of_scope")
	w.denied(w.Yuki, "department.list_admins", m{"dept_id": w.S}, "platform_role_required")
	w.denied(w.Robo, "department.list_admins", m{"dept_id": w.S}, "platform_role_required")

	// Past appointments, with who ended them, only when asked.
	w.do(w.Ada, "department.remove_admin", m{"dept_id": w.S, "actor_id": w.Chan})
	if got := names(w.Bob, m{"dept_id": w.S}); !slices.Equal(got, []string{"Bob@Computing"}) {
		t.Fatalf("after Chan's ended: %v", got)
	}
	all := testkit.Result[tools.DepartmentAdminsOut](t, w.do(w.Admin, "department.list_admins", m{"dept_id": w.S, "include_removed": true})).Admins
	if len(all) != 2 || all[1].DisplayName != "Chan" || all[1].RemovedAt == nil || all[1].RemovedByName == nil ||
		*all[1].RemovedByName != "Ada" || all[1].AppointedByName != "root" || all[0].RemovedAt != nil {
		t.Fatalf("with the past: %+v", all)
	}
	// Nobody's email is shown, to anyone.
	if raw, _ := json.Marshal(all); strings.Contains(string(raw), "email") || strings.Contains(string(raw), "example.edu") {
		t.Fatalf("an appointment shows an email: %s", raw)
	}
}

// The tree is everyone's to read; what one may do with each department is
// one's own.
func TestTheTreeSaysWhatTheCallerMayDo(t *testing.T) {
	w := newTree(t)
	nodes := func(actor uuid.UUID, args m) map[uuid.UUID]tools.DepartmentNode {
		t.Helper()
		out := testkit.Result[tools.DepartmentTreeOut](t, w.do(actor, "department.list_tree", args))
		if out.MaxDepth != domain.MaxDepartmentDepth {
			t.Fatalf("max_depth %d", out.MaxDepth)
		}
		got := map[uuid.UUID]tools.DepartmentNode{}
		var order []uuid.UUID
		for _, n := range out.Departments {
			got[n.ID] = n
			order = append(order, n.ID)
		}
		if len(order) == 7 && !slices.Equal(order, []uuid.UUID{w.U, w.F, w.S, w.D, w.S2, w.F2, w.S3}) {
			t.Fatalf("not each before those beneath it, siblings by name: %v", order)
		}
		return got
	}
	type flags struct{ administers, manages, appointed, counted bool }
	of := func(n tools.DepartmentNode) flags {
		return flags{n.Administers, n.Manages, n.Appointed, n.CourseCount != nil && n.AdminCount != nil}
	}

	ada := nodes(w.Ada, m{})
	for dept, want := range map[uuid.UUID]flags{
		w.U: {}, w.F2: {}, w.S3: {},
		w.F: {true, false, true, true},
		w.S: {true, true, false, true}, w.D: {true, true, false, true}, w.S2: {true, true, false, true},
	} {
		if got := of(ada[dept]); got != want {
			t.Errorf("Ada, %s: %+v, want %+v", ada[dept].Name, got, want)
		}
	}
	if s := ada[w.S]; *s.CourseCount != 1 || *s.AdminCount != 2 || s.Depth != 3 || s.ParentID == nil || *s.ParentID != w.F {
		t.Errorf("Computing, for Ada: %+v", s)
	}
	if d := ada[w.D]; *d.CourseCount != 2 || *d.AdminCount != 0 {
		t.Errorf("AI, for Ada: courses %d, administrators %d", *d.CourseCount, *d.AdminCount)
	}

	for _, n := range nodes(w.Admin, m{}) {
		if got := of(n); got != (flags{true, true, false, true}) {
			t.Errorf("a platform administrator, %s: %+v", n.Name, got)
		}
	}
	for _, who := range []uuid.UUID{w.Robo, w.Yuki} {
		for _, n := range nodes(who, m{}) {
			if got := of(n); got != (flags{}) {
				t.Errorf("someone who administers nothing, %s: %+v", n.Name, got)
			}
		}
	}

	sub := nodes(w.Bob, m{"root_id": w.S})
	if len(sub) != 2 || of(sub[w.S]) != (flags{true, false, true, true}) || of(sub[w.D]) != (flags{true, true, false, true}) || sub[w.D].Depth != 4 {
		t.Fatalf("Computing's subtree, for Bob: %+v", sub)
	}
	if _, err := w.Call(w.Bob, "department.list_tree", m{"root_id": uuid.New()}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("the subtree of a department that does not exist: %v", err)
	}

	// department.list names each one's parent.
	list := testkit.Result[tools.DepartmentListOut](t, w.do(w.Yuki, "department.list", m{}))
	for _, d := range list.Departments {
		if d.ID == w.D && (d.ParentID == nil || *d.ParentID != w.S) || d.ID == w.U && d.ParentID != nil {
			t.Fatalf("department.list: %+v", d)
		}
	}
}

// me.get names what a person is appointed to administer, and says nothing
// new of anyone else: an agent's answer is as it was.
func TestMeSaysWhatOneAdministers(t *testing.T) {
	w := newTree(t)
	w.Appoint(w.S2, w.Ada, w.Root)
	me := testkit.Result[tools.MeOut](t, w.do(w.Ada, "me.get", m{}))
	var got []string
	for _, a := range me.Administers {
		got = append(got, a.Name)
	}
	if !slices.Equal(got, []string{"Design", "Engineering"}) {
		t.Fatalf("Ada administers %v", got)
	}
	for _, who := range []uuid.UUID{w.Robo, w.Yuki, w.Admin} {
		raw := w.do(who, "me.get", m{}).Result
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["administers"]; ok {
			t.Fatalf("me.get of someone who administers nothing names it: %s", raw)
		}
	}
}

// What a call was allowed in is on its row: a platform role, a department's
// appointment, or neither, for a seat's call and one's own account's.
func TestAnActionSaysInWhatCapacityItWasAllowed(t *testing.T) {
	w := newTree(t)
	w.wantCapacity(w.do(w.Admin, "term.create", m{"name": "2027 Spring", "starts_on": "2027-01-10", "ends_on": "2027-05-20"}),
		domain.AuthorityPlatform, uuid.Nil)
	w.wantCapacity(w.do(w.Ada, "credential.issue_token", m{"label": "laptop"}), "", uuid.Nil)
	w.wantCapacity(w.do(w.Chan, "assignment.create", m{"course_id": w.CS, "title": "HW1", "points_possible": 10}), "", uuid.Nil)
	// A department administrator seated in a course acts there by the seat,
	// and nothing else.
	w.wantCapacity(w.do(w.Ada, "submission.create", m{"course_id": w.CS2, "assignment_id": w.GradedAssignment(w.CS2,
		w.RootComponent(w.CS2), "Essay", "10"), "body": "mine"}), "", uuid.Nil)
}

// Every event type the department tools emit has a visibility rule.
func (w tree) everyEmittedTypeHasARule() {
	w.t.Helper()
	known := map[string]bool{}
	for _, k := range tools.KnownEventTypes() {
		known[k] = true
	}
	rows, err := w.Pool.Query(w.t.Context(), `SELECT DISTINCT type FROM event WHERE type LIKE 'department.%'`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			w.t.Fatal(err)
		}
		if !known[typ] {
			w.t.Errorf("event type %q is emitted but has no visibility rule", typ)
		}
	}
	if err := rows.Err(); err != nil {
		w.t.Fatal(err)
	}
}
