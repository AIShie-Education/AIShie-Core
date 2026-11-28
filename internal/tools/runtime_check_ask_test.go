package tools_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

type runtimeAskArgs struct {
	actor, agent, course uuid.UUID
}

func checkRuntimeAskResult(t *testing.T, b *built, args runtimeAskArgs) (tools.RuntimeCheckAskOut, map[string]any) {
	t.Helper()
	out := b.asRuntime(t, "agent_runtime.check_ask", m{"actor_id": args.actor, "agent_id": args.agent, "course_id": args.course})
	if out.Status != domain.StatusExecuted {
		t.Fatalf("check_ask: %+v", out)
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Result, &raw); err != nil {
		t.Fatal(err)
	}
	return testkit.Result[tools.RuntimeCheckAskOut](t, out), raw
}

func checkRuntimeAsk(t *testing.T, b *built, args runtimeAskArgs) (bool, map[string]any) {
	t.Helper()
	result, raw := checkRuntimeAskResult(t, b, args)
	return result.MayAsk, raw
}

func TestRuntimeCheckAskIsAReadOnlyCatalogueContract(t *testing.T) {
	b := build(t)
	before := []int{
		b.Count(`SELECT count(*) FROM conversation`),
		b.Count(`SELECT count(*) FROM conversation_message`),
		b.Count(`SELECT count(*) FROM action`),
		b.Count(`SELECT count(*) FROM event`),
	}
	allowed, raw := checkRuntimeAsk(t, b, runtimeAskArgs{b.yuki, b.tutor, b.course})
	if !allowed || len(raw) != 2 || raw["may_ask"] != true || raw["grant"] == nil {
		t.Fatalf("Yuki may ask the course tutor: %v, %v", allowed, raw)
	}
	after := []int{
		b.Count(`SELECT count(*) FROM conversation`),
		b.Count(`SELECT count(*) FROM conversation_message`),
		b.Count(`SELECT count(*) FROM action`),
		b.Count(`SELECT count(*) FROM event`),
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("check_ask changed table %d: before %d, after %d", i, before[i], after[i])
		}
	}

	tl, ok := b.P.Registry().Get("agent_runtime.check_ask")
	if !ok || tl.Kind != tool.Read || tl.Gate.Service != domain.ServiceAgentRuntime {
		t.Fatalf("the check_ask declaration: %+v, %v", tl, ok)
	}
	const path = "/v1/services/agent_runtime/actors/{actor_id}/courses/{course_id}/agents/{agent_id}/may-ask"
	if tl.HTTP.Method != "GET" || tl.HTTP.Pattern != path {
		t.Fatalf("check_ask's route: %+v", tl.HTTP)
	}
	if _, ok := tl.OutputSchema.Properties["may_ask"]; !ok || len(tl.OutputSchema.Required) != 1 || tl.OutputSchema.Required[0] != "may_ask" {
		t.Fatalf("check_ask's output schema: %+v", tl.OutputSchema)
	}
	grant, ok := tl.OutputSchema.Properties["grant"]
	if !ok {
		t.Fatalf("check_ask has no optional grant in its output schema: %+v", tl.OutputSchema)
	}
	for _, name := range []string{"opener_member_id", "respondent_member_id", "respondent_principal_member_id", "conversation_ask_level", "action_decide_level", "conversation_answer_level", "answers_course"} {
		if _, ok := grant.Properties[name]; !ok {
			t.Errorf("grant's schema has no %s: %+v", name, grant)
		}
	}
	for _, name := range grant.Required {
		if name == "respondent_principal_member_id" {
			t.Errorf("a principal is required even for an agent without one: %+v", grant.Required)
		}
	}
}

func TestRuntimeCheckAskGrantNamesTheSeatsAndTheirEffectivePolicy(t *testing.T) {
	t.Run("an agent with no principal", func(t *testing.T) {
		b := build(t)
		result, raw := checkRuntimeAskResult(t, b, runtimeAskArgs{b.yuki, b.tutor, b.course})
		grant := result.Grant
		if !result.MayAsk || grant == nil {
			t.Fatalf("no grant with the allowed ask: %+v", result)
		}
		if grant.OpenerMemberID != b.yukiM || grant.RespondentMemberID != b.tutorM || grant.RespondentPrincipalMemberID != nil ||
			grant.ConversationAskLevel != domain.Autonomous.String() || grant.ActionDecideLevel != domain.Denied.String() ||
			grant.ConversationAnswerLevel != domain.Autonomous.String() || grant.AnswersCourse {
			t.Fatalf("the unowned tutor's grant: %+v", grant)
		}
		// Success adds only grant; within it, only the optional principal is
		// absent. No refusal-only, actor, model or provider detail appears.
		fields, ok := raw["grant"].(map[string]any)
		if !ok || len(raw) != 2 || len(fields) != 6 {
			t.Fatalf("the successful response or grant has extra information: %v", raw)
		}
	})

	t.Run("a delegate's principal and capped levels", func(t *testing.T) {
		b := build(t)
		agent := b.runtimeAgent(t, b.yuki, "Yuki's assistant")
		seat := b.delegate(t, b.yuki, agent, m{"preset": "delegate"})
		b.Host(agent)
		// Make both policy levels visible through the grant. The asker needs
		// the same action_decide level for the addressing rule to allow it.
		b.Exec(`UPDATE course_member SET perm_action_decide = 'confirm_required' WHERE id = $1`, b.yukiM)
		b.Exec(`UPDATE course_member SET perm_action_decide = 'confirm_required', perm_conversation_answer = 'pending_review' WHERE id = $1`, seat)

		result, raw := checkRuntimeAskResult(t, b, runtimeAskArgs{b.yuki, agent, b.course})
		grant := result.Grant
		if !result.MayAsk || grant == nil {
			t.Fatalf("no grant with the allowed delegated ask: %+v (%v)", result, raw)
		}
		if grant.OpenerMemberID != b.yukiM || grant.RespondentMemberID != seat ||
			grant.RespondentPrincipalMemberID == nil || *grant.RespondentPrincipalMemberID != b.yukiM ||
			grant.ConversationAskLevel != domain.Autonomous.String() || grant.ActionDecideLevel != domain.ConfirmRequired.String() ||
			grant.ConversationAnswerLevel != domain.PendingReview.String() || grant.AnswersCourse {
			t.Fatalf("the delegate's grant: %+v", grant)
		}
		fields, ok := raw["grant"].(map[string]any)
		if !ok || len(fields) != 7 || fields["respondent_principal_member_id"] != b.yukiM.String() {
			t.Fatalf("the delegate's principal is missing or leaks more: %v", raw)
		}
	})

	t.Run("a course tutor answers the course", func(t *testing.T) {
		b := build(t)
		agent := b.runtimeAgent(t, b.sato, "Course tutor")
		b.delegate(t, b.sato, agent, m{"preset": "course_tutor"})
		b.Host(agent)
		result, _ := checkRuntimeAskResult(t, b, runtimeAskArgs{b.yuki, agent, b.course})
		if !result.MayAsk || result.Grant == nil || !result.Grant.AnswersCourse {
			t.Fatalf("the manager's course_tutor delegate does not report answers_course: %+v", result)
		}
	})
}

func TestRuntimeCheckAskAllowsConfirmationAndOnlyThePrincipalOfAnOwnAgent(t *testing.T) {
	t.Run("confirmation is still permission to ask", func(t *testing.T) {
		b := build(t)
		b.Exec(`UPDATE course_member SET perm_conversation_ask = 'confirm_required' WHERE id = $1`, b.yukiM)
		if allowed, _ := checkRuntimeAsk(t, b, runtimeAskArgs{b.yuki, b.tutor, b.course}); !allowed {
			t.Fatal("confirm_required was treated as no permission")
		}
	})

	t.Run("an own agent answers its principal alone", func(t *testing.T) {
		b := build(t)
		agent := b.runtimeAgent(t, b.yuki, "Yuki's assistant")
		b.delegate(t, b.yuki, agent, m{"preset": "delegate"})
		b.Host(agent)
		if allowed, _ := checkRuntimeAsk(t, b, runtimeAskArgs{b.yuki, agent, b.course}); !allowed {
			t.Fatal("the principal may not ask their own hosted agent")
		}
		for _, actor := range []uuid.UUID{b.ken, b.sato} {
			if allowed, raw := checkRuntimeAsk(t, b, runtimeAskArgs{actor, agent, b.course}); allowed || len(raw) != 1 || raw["may_ask"] != false {
				t.Fatalf("someone else may ask the principal's own agent: %v, %v", allowed, raw)
			}
		}
	})
}

func TestRuntimeCheckAskReportsEveryAllowedConversationAskLevel(t *testing.T) {
	for _, level := range []domain.Level{domain.ConfirmRequired, domain.PendingReview, domain.Autonomous} {
		t.Run(level.String(), func(t *testing.T) {
			b := build(t)
			b.Exec(`UPDATE course_member SET perm_conversation_ask = $2 WHERE id = $1`, b.yukiM, level.String())
			result, raw := checkRuntimeAskResult(t, b, runtimeAskArgs{b.yuki, b.tutor, b.course})
			if !result.MayAsk || result.Grant == nil || result.Grant.ConversationAskLevel != level.String() {
				t.Fatalf("the effective ask level was not reported: %+v, %v", result, raw)
			}
			if fields := raw["grant"].(map[string]any); fields["conversation_ask_level"] != level.String() {
				t.Fatalf("the JSON grant has the wrong conversation_ask_level: %v", raw)
			}
		})
	}
}

func TestRuntimeCheckAskRefusalsAreFalseAndIndistinguishable(t *testing.T) {
	base := func(b *built) runtimeAskArgs { return runtimeAskArgs{b.yuki, b.tutor, b.course} }
	tests := []struct {
		name    string
		arrange func(*built) runtimeAskArgs
	}{
		{"unknown actor", func(b *built) runtimeAskArgs {
			args := base(b)
			args.actor = uuid.New()
			return args
		}},
		{"unknown agent", func(b *built) runtimeAskArgs {
			args := base(b)
			args.agent = uuid.New()
			return args
		}},
		{"unknown course", func(b *built) runtimeAskArgs {
			args := base(b)
			args.course = uuid.New()
			return args
		}},
		{"actor is not seated", func(b *built) runtimeAskArgs {
			args := base(b)
			args.actor = b.admin
			return args
		}},
		{"actor is suspended", func(b *built) runtimeAskArgs {
			b.do(t, b.admin, "actor.suspend", m{"actor_id": b.yuki})
			return base(b)
		}},
		{"actor seat is paused", func(b *built) runtimeAskArgs {
			b.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, b.yukiM)
			return base(b)
		}},
		{"actor seat is expired", func(b *built) runtimeAskArgs {
			b.Exec(`UPDATE course_member SET expires_at = $2 WHERE id = $1`, b.yukiM, time.Now().Add(-time.Hour))
			return base(b)
		}},
		{"actor seat is removed", func(b *built) runtimeAskArgs {
			b.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, b.yukiM)
			return base(b)
		}},
		{"ask permission is denied", func(b *built) runtimeAskArgs {
			b.Exec(`UPDATE course_member SET perm_conversation_ask = 'denied' WHERE id = $1`, b.yukiM)
			return base(b)
		}},
		{"course is archived", func(b *built) runtimeAskArgs {
			b.do(t, b.admin, "course.archive", m{"course_id": b.course})
			return base(b)
		}},
		{"a temporary password must be changed", func(b *built) runtimeAskArgs {
			b.Exec(`UPDATE actor SET email = 'yuki@example.edu' WHERE id = $1`, b.yuki)
			b.Exec(`INSERT INTO credential (actor_id, kind, secret_hash, must_change, issued_by_actor_id) VALUES ($1, 'password', 'unused', true, $2)`, b.yuki, b.sato)
			return base(b)
		}},
		{"respondent is a person", func(b *built) runtimeAskArgs {
			return runtimeAskArgs{b.ken, b.yuki, b.course}
		}},
		{"agent is not seated", func(b *built) runtimeAskArgs {
			agent := b.runtimeAgent(t, b.sato, "Unseated tutor")
			b.Host(agent)
			return runtimeAskArgs{b.yuki, agent, b.course}
		}},
		{"runtime agent is not hosted", func(b *built) runtimeAskArgs {
			agent := b.runtimeAgent(t, b.sato, "Offline tutor")
			b.delegate(t, b.sato, agent, m{"preset": "course_tutor"})
			return runtimeAskArgs{b.yuki, agent, b.course}
		}},
		{"mcp agent", func(b *built) runtimeAskArgs {
			agent := b.agent(t, b.yuki, "Yuki's script")
			b.delegate(t, b.yuki, agent, m{"preset": "delegate"})
			return runtimeAskArgs{b.yuki, agent, b.course}
		}},
		{"agent is suspended", func(b *built) runtimeAskArgs {
			b.do(t, b.admin, "actor.suspend", m{"actor_id": b.tutor})
			return base(b)
		}},
		{"agent owner is suspended", func(b *built) runtimeAskArgs {
			agent := b.runtimeAgent(t, b.sato, "Sato's tutor")
			b.delegate(t, b.sato, agent, m{"preset": "course_tutor"})
			b.Host(agent)
			b.do(t, b.admin, "actor.suspend", m{"actor_id": b.sato})
			return runtimeAskArgs{b.yuki, agent, b.course}
		}},
		{"respondent seat is paused", func(b *built) runtimeAskArgs {
			b.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, b.tutorM)
			return base(b)
		}},
		{"respondent seat is expired", func(b *built) runtimeAskArgs {
			b.Exec(`UPDATE course_member SET expires_at = $2 WHERE id = $1`, b.tutorM, time.Now().Add(-time.Hour))
			return base(b)
		}},
		{"respondent seat is removed", func(b *built) runtimeAskArgs {
			b.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, b.tutorM)
			return base(b)
		}},
		{"respondent does not answer", func(b *built) runtimeAskArgs {
			b.Exec(`UPDATE course_member SET perm_conversation_answer = 'denied' WHERE id = $1`, b.tutorM)
			return base(b)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := build(t)
			args := tc.arrange(b)
			result, raw := checkRuntimeAskResult(t, b, args)
			if result.MayAsk || result.Grant != nil || len(raw) != 1 || raw["may_ask"] != false {
				t.Fatalf("refusal leaked or was allowed: %+v, %v", result, raw)
			}
		})
	}
}
