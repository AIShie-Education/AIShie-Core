package httpapi_test

import (
	"testing"
)

// Deleting an assignment for good over REST: the preview counts what goes,
// the deletion takes back those counts and is refused, 409, when more would
// go; an agent is refused one with work, 403; a person deletes it, and the
// same request again replays what it did; afterwards the assignment is not
// found, and says it was deleted.
func TestAnAssignmentIsDeletedForGoodOverHTTP(t *testing.T) {
	a := newAPI(t, 1)
	c, yuki := a.c, a.c.Students[0]
	sato := a.tokenFor(c.Sato)
	hw3 := "/v1/courses/" + c.Course.String() + "/assignments/" + c.HW3.String()
	key := func(k string) []string { return []string{"Idempotency-Key", k} }
	// An agent nobody owns, seated to write assignments as an instructor
	// does.
	bot := c.Actor("agent", "bot")
	c.Member(c.Course, bot, "instructor")

	preview := a.do(nil, "GET", hw3+"/delete-preview", sato, nil)
	counts, _ := preview.Body["result"].(map[string]any)["counts"].(map[string]any)
	if preview.Status != 200 || counts["submissions"] != 1.0 || counts["handed_in"] != 1.0 || counts["totals"] != 0.0 {
		t.Fatalf("the preview: %d %s", preview.Status, preview.Raw)
	}
	if r := a.do(nil, "GET", hw3+"/delete-preview", a.tokenFor(yuki.Actor), nil); r.Status != 403 {
		t.Fatalf("a student's preview: %d %s", r.Status, r.Raw)
	}

	none := map[string]any{"submissions": 0, "handed_in": 0, "drafts": 0, "missing": 0, "grades": 0, "posted": 0, "files": 0,
		"proposals": 0, "totals": 0}
	stale := a.do(nil, "POST", hw3+"/delete", sato, m{"confirm": none}, key("stale")...)
	if stale.Status != 409 || stale.str("error", "details", "reason") != "confirm_stale" || stale.str("action_id") == "" {
		t.Fatalf("a stale confirmation: %d %s", stale.Status, stale.Raw)
	}
	if r := a.do(nil, "POST", hw3+"/delete", sato, m{}, key("no-confirm")...); r.Status != 400 {
		t.Fatalf("no confirmation: %d %s", r.Status, r.Raw)
	}
	if r := a.do(nil, "POST", hw3+"/delete", a.tokenFor(bot), m{"confirm": counts}, key("bot")...); r.Status != 403 ||
		r.str("error", "details", "reason") != "people_only" {
		t.Fatalf("an agent deleting work: %d %s", r.Status, r.Raw)
	}

	deleted := a.do(nil, "POST", hw3+"/delete", sato, m{"confirm": counts}, key("delete")...)
	result, _ := deleted.Body["result"].(map[string]any)
	if deleted.Status != 200 || result["deleted"] != true || result["title"] != "HW3" {
		t.Fatalf("Sato deleting HW3: %d %s", deleted.Status, deleted.Raw)
	}
	again := a.do(nil, "POST", hw3+"/delete", sato, m{"confirm": counts}, key("delete")...)
	if again.Status != 200 || again.Header.Get("Idempotency-Replayed") != "true" || again.str("action_id") != deleted.str("action_id") {
		t.Fatalf("the same request again: %d %s", again.Status, again.Raw)
	}
	for name, r := range map[string]response{
		"reading it":         a.do(nil, "GET", hw3, sato, nil),
		"previewing it":      a.do(nil, "GET", hw3+"/delete-preview", sato, nil),
		"deleting it again":  a.do(nil, "POST", hw3+"/delete", sato, m{"confirm": counts}, key("again")...),
		"handing work to it": a.do(nil, "POST", "/v1/courses/"+c.Course.String()+"/submissions", a.tokenFor(yuki.Actor), m{"assignment_id": c.HW3}, key("late")...),
	} {
		if r.Status != 404 || r.str("error", "details", "reason") != "deleted" || r.str("error", "details", "by_action_id") != deleted.str("action_id") {
			t.Fatalf("%s afterwards: %d %s", name, r.Status, r.Raw)
		}
	}
	if n := c.Count(`SELECT count(*) FROM submission WHERE id = $1`, yuki.HW3); n != 0 {
		t.Fatal("Yuki's work is still there")
	}
}
