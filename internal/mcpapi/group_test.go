package mcpapi_test

import (
	"testing"

	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// An agent that sets assignments forms groups over MCP: a set, a random
// split by a seed it names, and the set as it reads it; the same seed in
// another set deals the same way. A student's own call to split is denied.
func TestAnAgentFormsGroupsOverMCP(t *testing.T) {
	f := serve(t, 4)
	c := f.c
	bot := c.Actor("agent", "bot")
	c.Member(c.Course, bot, "instructor")
	s := f.connect(t, f.token(t, bot))

	var labs tools.IDOut
	split := func(name string) tools.GroupSplitOut {
		t.Helper()
		env, _ := call(t, s, "group_set_create", m{"course_id": c.Course, "name": name, "idempotency_key": "set-" + name})
		var set tools.IDOut
		if env.Status != "executed" || undecodable(env.Result, &set) {
			t.Fatalf("the set: %+v", env)
		}
		if name == "Labs" {
			labs = set
		}
		env, _ = call(t, s, "group_split", m{"course_id": c.Course, "set_id": set.ID, "by": "size", "n": 2, "from": "unassigned",
			"seed": "mcp", "idempotency_key": "split-" + name})
		var out tools.GroupSplitOut
		if env.Status != "executed" || undecodable(env.Result, &out) {
			t.Fatalf("the split: %+v %s", env, env.Result)
		}
		if out.Seed != "mcp" || len(out.Created) != 2 || len(out.Placed) != 4 {
			t.Fatalf("the split: %+v", out)
		}
		env, _ = call(t, s, "group_set_get", m{"course_id": c.Course, "set_id": set.ID})
		var view tools.GroupSetView
		if env.Status != "executed" || undecodable(env.Result, &view) || len(view.Groups) != 2 || len(view.Groups[0].Members) != 2 {
			t.Fatalf("the set as the agent reads it: %+v %s", env, env.Result)
		}
		return out
	}
	one, two := split("Labs"), split("Labs again")
	for i := range one.Placed {
		if one.Placed[i].StudentMemberID != two.Placed[i].StudentMemberID {
			t.Fatalf("the same seed dealt two ways: %+v and %+v", one.Placed, two.Placed)
		}
	}

	yuki := f.connect(t, f.token(t, c.Students[0].Actor))
	env, _ := call(t, yuki, "group_split", m{"course_id": c.Course, "set_id": labs.ID, "by": "size", "n": 2,
		"from": "all", "idempotency_key": "yuki"})
	if env.Status != "denied" {
		t.Fatalf("a student's split: %+v", env)
	}
}
