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

func checkRuntimeAsk(t *testing.T, b *built, args runtimeAskArgs) (bool, map[string]any) {
	t.Helper()
	out := b.asRuntime(t, "agent_runtime.check_ask", m{"actor_id": args.actor, "agent_id": args.agent, "course_id": args.course})
	if out.Status != domain.StatusExecuted {
		t.Fatalf("check_ask: %+v", out)
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Result, &raw); err != nil {
		t.Fatal(err)
	}
	return testkit.Result[tools.RuntimeCheckAskOut](t, out).MayAsk, raw
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
	if !allowed || len(raw) != 1 || raw["may_ask"] != true {
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
			allowed, raw := checkRuntimeAsk(t, b, args)
			if allowed || len(raw) != 1 || raw["may_ask"] != false {
				t.Fatalf("refusal leaked or was allowed: %v, %v", allowed, raw)
			}
		})
	}
}
