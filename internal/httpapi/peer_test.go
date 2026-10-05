package httpapi_test

import (
	"testing"
	"time"
)

// Peer evaluation over REST: Sato sets HW4's peer form, a share form, and
// changes it over the version he read (If-Match), a change over none, or a
// stale one, refused (409 version_mismatch); a student's task is not open
// until the group hands in; the shares not adding up to 100 are refused
// (400 bad_share_total); the results are Sato's to read and not a student's
// (403); counting it is refused while the window is open (422 window_open)
// and a sheet once it has closed (422 window_closed); counted, each member
// reads their own peer adjustment.
func TestPeerEvaluationOverHTTP(t *testing.T) {
	a := newAPI(t, 3)
	c := a.c
	sato := a.tokenFor(c.Sato)
	tokens := []string{a.tokenFor(c.Students[0].Actor), a.tokenFor(c.Students[1].Actor), a.tokenFor(c.Students[2].Actor)}
	s0, s1, s2 := c.Students[0].Member, c.Students[1].Member, c.Students[2].Member
	course := "/v1/courses/" + c.Course.String()
	hw4 := course + "/assignments/" + c.HW4.String()
	n := 0
	key := func() []string { n++; return []string{"Idempotency-Key", "peer-" + string(rune('a'+n))} }
	ok := func(r response, what string) response {
		t.Helper()
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", what, r.Status, r.Raw)
		}
		return r
	}
	refused := func(r response, status int, reason, what string) {
		t.Helper()
		if r.Status != status || (reason != "" && r.str("error", "details", "reason") != reason) {
			t.Fatalf("%s: %d %s, want %d %s", what, r.Status, r.Raw, status, reason)
		}
	}

	set := ok(a.do(nil, "POST", course+"/group-sets", sato, m{"name": "Projects"}, key()...), "the set").str("result", "id")
	made := ok(a.do(nil, "POST", course+"/group-sets/"+set+"/groups", sato, m{"groups": []m{{"name": "Team"}}}, key()...), "the group")
	team := made.Body["result"].(m)["group_ids"].([]any)[0].(string)
	ok(a.do(nil, "POST", course+"/group-sets/"+set+"/members", sato, m{"placements": []m{{"student_member_id": s0, "group_id": team},
		{"student_member_id": s1, "group_id": team}, {"student_member_id": s2, "group_id": team}}}, key()...), "placing the three")
	ok(a.do(nil, "POST", hw4, sato, m{"group_set_id": set}, key()...), "HW4 made a group assignment")

	form := m{"kind": "share", "opens": "on_hand_in", "closes_at": time.Now().Add(time.Hour), "weight": 30}
	made = ok(a.do(nil, "POST", hw4+"/peer-form", sato, form, key()...), "the form")
	if made.Body["result"].(m)["version"] != 1.0 {
		t.Fatalf("the form made: %s", made.Raw)
	}
	form["weight"] = 20
	refused(a.do(nil, "POST", hw4+"/peer-form", sato, form, key()...), 409, "version_mismatch", "a change over no version")
	refused(a.do(nil, "POST", hw4+"/peer-form", sato, form, append(key(), "If-Match", `"7"`)...), 409, "version_mismatch", "a change over a stale one")
	changed := ok(a.do(nil, "POST", hw4+"/peer-form", sato, form, append(key(), "If-Match", `"1"`)...), "a change over the version read")
	if changed.Body["result"].(m)["version"] != 2.0 || changed.Body["result"].(m)["weight"] != 20.0 {
		t.Fatalf("the form changed: %s", changed.Raw)
	}

	read := ok(a.do(nil, "GET", hw4+"/peer-form", tokens[0], nil), "the form as a student reads it")
	if read.str("result", "task", "window", "state") != "not_open" {
		t.Fatalf("the task before the hand-in: %s", read.Raw)
	}
	work := ok(a.do(nil, "POST", course+"/submissions", tokens[0], m{"assignment_id": c.HW4, "body": "Ours"}, key()...), "the work").str("result", "submission_id")
	ok(a.do(nil, "POST", course+"/submissions/"+work+"/submit", tokens[0], nil, key()...), "handed in")

	sheet := func(shares ...any) m {
		entries := []m{}
		for i := 0; i < len(shares); i += 2 {
			entries = append(entries, m{"student_member_id": shares[i], "share": shares[i+1]})
		}
		return m{"entries": entries}
	}
	refused(a.do(nil, "POST", hw4+"/peer-reviews", tokens[0], sheet(s1, 50, s2, 40), key()...), 400, "bad_share_total", "shares adding up to 90")
	ok(a.do(nil, "POST", hw4+"/peer-reviews", tokens[0], sheet(s1, 50, s2, 50), key()...), "the first sheet")
	ok(a.do(nil, "POST", hw4+"/peer-reviews", tokens[1], sheet(s0, 60, s2, 40), key()...), "the second")
	ok(a.do(nil, "POST", hw4+"/peer-reviews", tokens[2], sheet(s0, 70, s1, 30), key()...), "the third")

	refused(a.do(nil, "GET", hw4+"/peer-results", tokens[0], nil), 403, "", "a student reading the results")
	results := ok(a.do(nil, "GET", hw4+"/peer-results", sato, nil), "the results")
	members := results.Body["result"].(m)["groups"].([]any)[0].(m)["members"].([]any)
	factors := map[string]any{}
	for _, mm := range members {
		factors[mm.(m)["member_id"].(string)] = mm.(m)["factor"]
	}
	if factors[s0.String()] != 1.3 || factors[s1.String()] != 0.8 || factors[s2.String()] != 0.9 {
		t.Fatalf("the factors: %v", factors)
	}

	graded := ok(a.do(nil, "POST", course+"/grades", sato, m{"submission_id": work, "score": 60}, key()...), "graded while open")
	if graded.str("result", "peer") != "window_open" {
		t.Fatalf("graded while open: %s", graded.Raw)
	}
	refused(a.do(nil, "POST", hw4+"/apply-peer", sato, nil, key()...), 422, "window_open", "counting it while open")
	form["closes_at"] = time.Now().Add(-time.Minute)
	ok(a.do(nil, "POST", hw4+"/peer-form", sato, form, append(key(), "If-Match", `"2"`)...), "closed")
	refused(a.do(nil, "POST", hw4+"/peer-reviews", tokens[0], sheet(s1, 50, s2, 50), key()...), 422, "window_closed", "a sheet once closed")
	applied := ok(a.do(nil, "POST", hw4+"/apply-peer", sato, nil, key()...), "counted")
	if written, _ := applied.Body["result"].(m)["written"].([]any); len(written) != 3 {
		t.Fatalf("counted: %s", applied.Raw)
	}
	ok(a.do(nil, "POST", course+"/grades/post", sato, m{"assignment_id": c.HW4}, key()...), "posted")
	mine := ok(a.do(nil, "GET", course+"/grades?assignment_id="+c.HW4.String(), tokens[0], nil), "the first student's grade")
	g := mine.Body["result"].(m)["grades"].([]any)[0].(m)
	adj := g["group"].(m)["adjustment"].(m)
	detail := adj["detail"].(m)
	if g["score"] != 63.6 || adj["kind"] != "peer" || adj["points"] != 3.6 || detail["factor"] != 1.3 || detail["weight"] != 20.0 ||
		detail["raters"] != nil || adj["reason"] != nil {
		t.Fatalf("the first student's grade: %s", mine.Raw)
	}
}
