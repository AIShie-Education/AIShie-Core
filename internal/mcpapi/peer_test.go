package mcpapi_test

import (
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// An agent that sets assignments gives a group assignment a peer form over
// MCP and reads the results once two students have evaluated each other;
// it writes no peer evaluation itself, and neither does a student's own
// agent, which may write her work: refused as people_only, and recorded.
func TestAnAgentWritesNoPeerEvaluationOverMCP(t *testing.T) {
	f := serve(t, 2)
	c := f.c
	bot := c.Actor("agent", "bot")
	c.Member(c.Course, bot, "instructor")
	s := f.connect(t, f.token(t, bot))
	s0, s1 := c.Students[0].Member, c.Students[1].Member
	must := func(tool string, args map[string]any, out any) {
		t.Helper()
		env, _ := call(t, s, tool, args)
		if env.Status != "executed" || (out != nil && undecodable(env.Result, out)) {
			t.Fatalf("%s: %+v %s", tool, env, env.Result)
		}
	}
	var set tools.IDOut
	must("group_set_create", m{"course_id": c.Course, "name": "Pairs", "idempotency_key": "set"}, &set)
	var groups tools.GroupCreateOut
	must("group_create", m{"course_id": c.Course, "set_id": set.ID, "groups": []m{{"name": "Pair 1"}}, "idempotency_key": "groups"}, &groups)
	must("group_set_members", m{"course_id": c.Course, "set_id": set.ID, "placements": []m{{"student_member_id": s0, "group_id": groups.GroupIDs[0]},
		{"student_member_id": s1, "group_id": groups.GroupIDs[0]}}, "idempotency_key": "place"}, nil)
	must("assignment_update", m{"course_id": c.Course, "assignment_id": c.HW4, "group_set_id": set.ID, "idempotency_key": "hw4"}, nil)
	var form tools.PeerFormView
	must("peer_form_set", m{"course_id": c.Course, "assignment_id": c.HW4, "kind": "share", "self_evaluation": true, "opens": "at",
		"opens_at": time.Now().Add(-time.Minute), "closes_at": time.Now().Add(time.Hour), "weight": 40, "idempotency_key": "form"}, &form)
	if form.Version != 1 || !form.SelfEvaluation {
		t.Fatalf("the form: %+v", form)
	}

	// The two evaluate each other and themselves.
	for i, shares := range [][2]int{{60, 40}, {50, 50}} {
		student := f.connect(t, f.token(t, c.Students[i].Actor))
		env, _ := call(t, student, "peer_review_submit", m{"course_id": c.Course, "assignment_id": c.HW4, "entries": []m{
			{"student_member_id": s0, "share": shares[0]}, {"student_member_id": s1, "share": shares[1]}},
			"idempotency_key": "sheet"})
		if env.Status != "executed" {
			t.Fatalf("student %d's sheet: %+v", i, env)
		}
	}
	// The first's own agent, which may write her work, writes no sheet; nor
	// does the bot.
	helper := c.OwnedAgent(c.Students[0].Actor, "helper")
	c.Delegate(c.Course, helper, s0, "delegate", testkit.WithPerm(domain.PermSubmissionWrite, domain.ConfirmRequired))
	for who, session := range map[string]*mcp.ClientSession{"her agent": f.connect(t, f.token(t, helper)), "the bot": s} {
		env, res := call(t, session, "peer_review_submit", m{"course_id": c.Course, "assignment_id": c.HW4,
			"entries": []m{{"student_member_id": s0, "share": 50}, {"student_member_id": s1, "share": 50}}, "idempotency_key": "agent-sheet"})
		if env.Status != "failed" || env.Error == nil || env.Error.Details["reason"] != "people_only" || env.ActionID == nil || !res.IsError {
			t.Fatalf("%s's sheet: %+v", who, env)
		}
	}

	var results tools.PeerResultsOut
	must("peer_review_results", m{"course_id": c.Course, "assignment_id": c.HW4}, &results)
	if len(results.Groups) != 1 || len(results.Groups[0].Sheets) != 2 || len(results.Groups[0].Flags) != 0 {
		t.Fatalf("the results: %+v", results.Groups)
	}
	for _, mr := range results.Groups[0].Members {
		want := "1.1"
		if mr.MemberID == s1 {
			want = "0.9"
		}
		if mr.Factor.String() != want || mr.Self == nil {
			t.Fatalf("%s's results: %+v", mr.MemberID, mr)
		}
	}
}
