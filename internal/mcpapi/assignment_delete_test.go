package mcpapi_test

import (
	"encoding/json"
	"testing"

	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// undecodable decodes a result into v, and says whether it could not.
func undecodable(raw json.RawMessage, v any) bool { return json.Unmarshal(raw, v) != nil }

// An agent deletes over MCP an assignment nobody has started on, reading
// what goes first and confirming it; one with work it is refused
// (people_only), recorded, as a person would be told nothing of the kind.
func TestAnAgentDeletesOnlyAnAssignmentNobodyStartedOnOverMCP(t *testing.T) {
	f := serve(t, 1)
	c := f.c
	bot := c.Actor("agent", "bot")
	c.Member(c.Course, bot, "instructor")
	s := f.connect(t, f.token(t, bot))

	// HW3 has Yuki's work: refused.
	env, _ := call(t, s, "assignment_delete_preview", m{"course_id": c.Course, "assignment_id": c.HW3})
	var preview tools.AssignmentDeletePreviewOut
	if env.Status != "executed" || undecodable(env.Result, &preview) || preview.Refusal == nil || *preview.Refusal != "people_only" {
		t.Fatalf("the preview of HW3: %+v %s", env, env.Result)
	}
	env, res := call(t, s, "assignment_delete", m{"course_id": c.Course, "assignment_id": c.HW3, "confirm": preview.Counts,
		"idempotency_key": "hw3"})
	if env.Status != "failed" || env.Error == nil || env.Error.Details["reason"] != "people_only" || env.ActionID == nil || !res.IsError {
		t.Fatalf("the agent deleting HW3: %+v", env)
	}

	// HW4, which nobody has started on: deleted.
	env, _ = call(t, s, "assignment_delete_preview", m{"course_id": c.Course, "assignment_id": c.HW4})
	if env.Status != "executed" || undecodable(env.Result, &preview) || preview.Refusal != nil {
		t.Fatalf("the preview of HW4: %+v %s", env, env.Result)
	}
	env, _ = call(t, s, "assignment_delete", m{"course_id": c.Course, "assignment_id": c.HW4, "confirm": preview.Counts,
		"idempotency_key": "hw4"})
	var out tools.AssignmentDeleteOut
	if env.Status != "executed" || undecodable(env.Result, &out) || !out.Deleted || out.Title != "HW4" {
		t.Fatalf("the agent deleting HW4: %+v %s", env, env.Result)
	}
	env, _ = call(t, s, "assignment_get", m{"course_id": c.Course, "assignment_id": c.HW4})
	if env.Error == nil || env.Error.Code != "not_found" || env.Error.Details["reason"] != "deleted" {
		t.Fatalf("HW4 afterwards: %+v", env)
	}
}
