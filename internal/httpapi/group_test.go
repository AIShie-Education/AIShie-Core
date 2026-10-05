package httpapi_test

import (
	"testing"
)

// Group work over REST: a set is made, its groups opened for sign-up, which
// students take, a full group refusing (422 group_full); a group assignment's
// draft is written by its members over revisions, a stale edit refused (409
// draft_changed) and one naming no revision too (400
// base_revision_required); a student in no group hands nothing in (422
// no_group); the group's work is handed in, graded once with a member
// adjusted, posted, and each member reads their own grade from it.
func TestGroupWorkOverHTTP(t *testing.T) {
	a := newAPI(t, 3)
	c := a.c
	sato := a.tokenFor(c.Sato)
	yuki, ken, hana := a.tokenFor(c.Students[0].Actor), a.tokenFor(c.Students[1].Actor), a.tokenFor(c.Students[2].Actor)
	course := "/v1/courses/" + c.Course.String()
	n := 0
	key := func() []string { n++; return []string{"Idempotency-Key", "group-" + string(rune('a'+n))} }
	ok := func(r response, what string) response {
		t.Helper()
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", what, r.Status, r.Raw)
		}
		return r
	}
	refused := func(r response, status int, reason, what string) {
		t.Helper()
		if r.Status != status || r.str("error", "details", "reason") != reason {
			t.Fatalf("%s: %d %s, want %d %s", what, r.Status, r.Raw, status, reason)
		}
	}

	set := ok(a.do(nil, "POST", course+"/group-sets", sato, m{"name": "Projects", "signup_open": true}, key()...), "the set").str("result", "id")
	made := ok(a.do(nil, "POST", course+"/group-sets/"+set+"/groups", sato, m{"groups": []m{{"name": "Team A", "capacity": 2}, {"name": "Team B"}}}, key()...), "the groups")
	ids, _ := made.Body["result"].(m)["group_ids"].([]any)
	teamA := ids[0].(string)
	ok(a.do(nil, "POST", course+"/group-sets/"+set+"/sign-up", yuki, m{"group_id": teamA}, key()...), "Yuki signing up")
	ok(a.do(nil, "POST", course+"/group-sets/"+set+"/sign-up", ken, m{"group_id": teamA}, key()...), "Ken signing up")
	refused(a.do(nil, "POST", course+"/group-sets/"+set+"/sign-up", hana, m{"group_id": teamA}, key()...), 422, "group_full", "Hana signing up to a full group")
	list := ok(a.do(nil, "GET", course+"/group-sets", yuki, nil), "the sets as Yuki reads them")
	sets, _ := list.Body["result"].(m)["sets"].([]any)
	if len(sets) != 1 || sets[0].(m)["my_group_id"] != teamA {
		t.Fatalf("the sets as Yuki reads them: %s", list.Raw)
	}

	hw4 := course + "/assignments/" + c.HW4.String()
	ok(a.do(nil, "POST", hw4, sato, m{"group_set_id": set}, key()...), "HW4 made a group assignment")
	refused(a.do(nil, "POST", course+"/submissions", hana, m{"assignment_id": c.HW4}, key()...), 422, "no_group", "Hana starting work")
	work := ok(a.do(nil, "POST", course+"/submissions", yuki, m{"assignment_id": c.HW4, "body": "Plan"}, key()...), "Team A's work").str("result", "submission_id")
	sub := course + "/submissions/" + work
	refused(a.do(nil, "POST", sub, ken, m{"body": "Plan and method"}, key()...), 400, "base_revision_required", "Ken's edit naming no revision")
	edit := ok(a.do(nil, "POST", sub, ken, m{"body": "Plan and method", "base_revision": 1}, key()...), "Ken's edit")
	if edit.Body["result"].(m)["revision"] != 2.0 {
		t.Fatalf("Ken's edit: %s", edit.Raw)
	}
	stale := a.do(nil, "POST", sub, yuki, m{"body": "Plan only", "base_revision": 1}, key()...)
	refused(stale, 409, "draft_changed", "Yuki's stale edit")
	if stale.Body["error"].(m)["details"].(m)["current_revision"] != 2.0 {
		t.Fatalf("the stale edit says nothing of the draft now: %s", stale.Raw)
	}
	handed := ok(a.do(nil, "POST", sub+"/submit", ken, nil, key()...), "Ken handing it in")
	if members, _ := handed.Body["result"].(m)["members"].([]any); len(members) != 2 {
		t.Fatalf("handed in for %s", handed.Raw)
	}

	graded := ok(a.do(nil, "POST", course+"/grades", sato, m{"submission_id": work, "score": 80, "adjustments": []m{
		{"student_member_id": c.Students[1].Member, "kind": "delta", "points": -5, "reason": "Missed the presentation"}}}, key()...), "the group's grade")
	memberGrades, _ := graded.Body["result"].(m)["member_grades"].([]any)
	if len(memberGrades) != 2 || graded.str("result", "group_grade_id") == "" {
		t.Fatalf("the group's grade: %s", graded.Raw)
	}
	ok(a.do(nil, "POST", course+"/grades/post", sato, m{"assignment_id": c.HW4}, key()...), "posting")
	kens := ok(a.do(nil, "GET", course+"/grades?assignment_id="+c.HW4.String(), ken, nil), "Ken's grades")
	grades, _ := kens.Body["result"].(m)["grades"].([]any)
	if len(grades) != 1 {
		t.Fatalf("Ken's grades: %s", kens.Raw)
	}
	g := grades[0].(m)
	adj, _ := g["group"].(m)["adjustment"].(m)
	if g["score"] != 75.0 || g["group"].(m)["score"] != 80.0 || adj["reason"] != "Missed the presentation" || adj["by_member_id"] != nil {
		t.Fatalf("Ken's grade: %s", kens.Raw)
	}
	var yukisGrade string
	for _, mg := range memberGrades {
		if mg.(m)["student_member_id"] == c.Students[0].Member.String() {
			yukisGrade = mg.(m)["grade_id"].(string)
		}
	}
	adjusted := ok(a.do(nil, "POST", course+"/grades/"+yukisGrade+"/adjust", sato, m{"kind": "delta", "points": 5, "reason": "Led the team"}, key()...), "Yuki adjusted")
	if adjusted.Body["result"].(m)["score"] != 85.0 {
		t.Fatalf("Yuki adjusted: %s", adjusted.Raw)
	}
	refused(a.do(nil, "POST", course+"/grades/"+yukisGrade+"/adjust", yuki, m{"kind": "none"}, key()...), 403, "permission_denied", "Yuki adjusting herself")

	roster := ok(a.do(nil, "GET", hw4+"/roster", sato, nil), "the roster")
	if groups, _ := roster.Body["result"].(m)["groups"].([]any); len(groups) != 2 {
		t.Fatalf("the roster's groups: %s", roster.Raw)
	}
	history := ok(a.do(nil, "GET", course+"/group-sets/"+set+"?include_history=true", sato, nil), "the set's history")
	if h, _ := history.Body["result"].(m)["history"].([]any); len(h) != 2 {
		t.Fatalf("the set's history: %s", history.Raw)
	}
}
