package tools_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// A seat's ceilings: the most it may hold of each permission at all,
// whatever its row says and whoever grants it (domain.Ceiling). An agent,
// owned or not, decides and reviews only by proposal.

// aboveCeiling insists a call was refused for going above a seat's ceiling,
// saying why in the code the views give.
func aboveCeiling(t *testing.T, what string, out pipeline.Outcome, perm, why string) {
	t.Helper()
	if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.Forbidden ||
		out.Error.Details["permission"] != perm || out.Error.Details["reason"] != why {
		t.Fatalf("%s: %+v, want refused %s for %s", what, out, why, perm)
	}
}

func TestAnAgentDecidesAndReviewsOnlyByProposal(t *testing.T) {
	b := build(t)
	mori := b.mori(t)
	triage := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "triage"})).ActorID
	aide := b.person(t, "Aide", "")

	// Seated with a preset that decides on its own, an agent nobody owns
	// decides by proposal; a person seated with it does not.
	triageM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": triage, "preset": "instructor"})).MemberID
	aideM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": aide, "preset": "instructor", "role": "assistant"})).MemberID
	v := b.memberView(t, triageM)
	if v.Perms["action_decide"] != "confirm_required" || v.Perms["grade_post"] != "autonomous" ||
		v.PermCeilings["action_decide"] != "confirm_required" || v.PermCeilingReasons["action_decide"] != "agent_decides_by_proposal" ||
		v.PermCeilings["grade_post"] != "autonomous" || len(v.PermCeilingReasons) != 1 {
		t.Fatalf("the triage agent's seat: %+v", v)
	}
	// A person answers no conversation, whatever the preset says: that is
	// the one ceiling of theirs below autonomous.
	if a := b.memberView(t, aideM); a.Perms["action_decide"] != "autonomous" || a.PermCeilings["action_decide"] != "autonomous" ||
		a.Perms["conversation_answer"] != "denied" || len(a.PermCeilingReasons) != 1 ||
		a.PermCeilingReasons["conversation_answer"] != "conversations_are_with_agents" {
		t.Fatalf("a person seated as an assistant: %+v", a)
	}

	// Named above it, it is refused, however it is asked: seating it,
	// raising it, raising every seat of its role at once.
	other := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "other"})).ActorID
	aboveCeiling(t, "seating an agent to decide on its own", b.MustCall(b.sato, "member.add",
		m{"course_id": b.course, "actor_id": other, "preset": "ta", "perms": m{"action_decide": "autonomous"}}, "add-other"), "action_decide", "agent_decides_by_proposal")
	for _, level := range []string{"pending_review", "autonomous"} {
		aboveCeiling(t, "raising the agent to "+level, b.MustCall(mori, "member.update_perms",
			m{"course_id": b.course, "member_id": triageM, "perms": m{"action_decide": level}}, "raise-"+level), "action_decide", "agent_decides_by_proposal")
	}
	b.do(t, mori, "member.update_perms", m{"course_id": b.course, "member_id": aideM, "perms": m{"action_decide": "confirm_required"}})
	bulk := b.MustCall(mori, "member.update_perms_bulk", m{"course_id": b.course, "role": "assistant", "perms": m{"action_decide": "autonomous"}}, "bulk")
	aboveCeiling(t, "raising every assistant", bulk, "action_decide", "agent_decides_by_proposal")
	if bulk.Error.Details["member_id"] == nil {
		t.Fatalf("the bulk refusal names no seat: %+v", bulk)
	}
	if a := b.memberView(t, aideM); a.Perms["action_decide"] != "confirm_required" {
		t.Fatalf("a person's seat changed by a refused bulk change: %+v", a.Perms)
	}

	// An owner's agent likewise: a preset's level cut down, a named one
	// refused, whatever its owner holds.
	bot := b.agent(t, b.sato, "Sato's triage")
	botM := b.delegate(t, b.sato, bot, m{"preset": "instructor"})
	if v := b.memberView(t, botM); v.Perms["action_decide"] != "confirm_required" || v.PermCeilingReasons["action_decide"] != "agent_decides_by_proposal" {
		t.Fatalf("Sato's agent seated with the instructor preset: %+v", v)
	}
	aboveCeiling(t, "Sato naming his agent to decide on its own", b.MustCall(b.sato, "member.add_delegate",
		m{"course_id": b.course, "actor_id": b.agent(t, b.sato, "Sato's other"), "perms": m{"action_decide": "pending_review"}}, "named"),
		"action_decide", "agent_decides_by_proposal")

	// Whatever its row is told, it is written so, and the agent is told so.
	b.Exec(`UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = ANY($1)`, []uuid.UUID{triageM, botM})
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = ANY($1) AND perm_action_decide = 'confirm_required'`, []uuid.UUID{triageM, botM}); n != 2 {
		t.Fatal("an agent's row holds action_decide above confirm_required")
	}
	for _, agent := range []uuid.UUID{triage, bot} {
		if me := b.membership(t, agent); me.Perms["action_decide"] != "confirm_required" || me.PermCeilings["action_decide"] != "confirm_required" {
			t.Fatalf("what the agent is told of its seat: %+v", me)
		}
	}

	// Its decisions and its reviews are proposals, which a person confirms.
	work := b.submit(t, b.yuki, "Yuki's essay")
	graded := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 80}, "grade")
	approval := b.MustCall(triage, "action.decide", m{"course_id": b.course, "action_id": graded.ActionID, "decision": "approve"}, "triage")
	if approval.Status != domain.StatusProposed {
		t.Fatalf("the agent's approval: %+v", approval)
	}
	if n := b.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatal("the agent's approval was carried out before anyone confirmed it")
	}
	if d := testkit.Result[pipeline.DecideOut](t, b.do(t, mori, "action.decide", m{"course_id": b.course, "action_id": approval.ActionID, "decision": "approve"})); d.Outcome != domain.StatusExecuted {
		t.Fatalf("Mori confirming the agent's approval: %+v", d)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'executed' AND decided_by_member_id = $2`, *graded.ActionID, triageM); n != 1 {
		t.Fatal("the grade was not carried out as the agent approved it")
	}
}

// Every ceiling a view reports is what enforcement allows, over every kind
// of seat — a person's, an agent's nobody owns, and the delegates of
// principals who manage the members and who do not, with levels of every
// kind — and every permission: a change up to it goes through, and one
// above it is refused with the reason the view gave; and a row told more
// than its ceiling, where the database takes it, is worth exactly its
// ceiling at the next call.
func TestEveryCeilingReportedIsWhatEnforcementAllows(t *testing.T) {
	b := build(t)
	mori := b.mori(t) // the granter: holds everything, over the whole class
	seat := func(who uuid.UUID, args m) uuid.UUID {
		args["course_id"], args["actor_id"] = b.course, who
		return testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", args)).MemberID
	}
	// Hana, a TA who manages nobody, some of her levels lowered; Rin, an
	// instructor whose own decisions, posting and asking are held back.
	hana := b.person(t, "Hana", "")
	seat(hana, m{"preset": "ta", "perms": m{"conversation_ask": "confirm_required", "grade_submit": "pending_review", "agent_delegate": "autonomous"}})
	rin := b.person(t, "Rin", "")
	seat(rin, m{"preset": "instructor", "perms": m{"action_decide": "confirm_required", "grade_post": "pending_review", "conversation_ask": "denied"}})

	type subject struct {
		name  string
		actor uuid.UUID
		seat  uuid.UUID
	}
	subjects := []subject{{"a student", b.ken, b.kenM}, {"an agent nobody owns", b.grader, b.graderM}}
	for _, o := range []struct {
		name   string
		owner  uuid.UUID
		preset string
	}{
		{"Sato's agent", b.sato, "instructor"},
		{"Yuki's agent", b.yuki, "delegate"},
		{"Hana's agent", hana, "ta"},
		{"Rin's agent", rin, "instructor"},
	} {
		agent := b.agent(t, o.owner, o.name)
		subjects = append(subjects, subject{o.name, agent, b.delegate(t, o.owner, agent, m{"preset": o.preset})})
	}

	levels := []domain.Level{domain.Denied, domain.ConfirmRequired, domain.PendingReview, domain.Autonomous}
	for _, s := range subjects {
		v := b.memberView(t, s.seat)
		for _, p := range domain.AllPerms {
			ceiling, err := domain.ParseLevel(v.PermCeilings[string(p)])
			if err != nil {
				t.Fatalf("%s: no ceiling for %s: %+v", s.name, p, v.PermCeilings)
			}
			why := v.PermCeilingReasons[string(p)]
			if (why == "") != (ceiling == domain.Autonomous) {
				t.Fatalf("%s, %s: ceiling %s, reason %q", s.name, p, ceiling, why)
			}
			was := v.Perms[string(p)]
			for _, l := range levels {
				out := b.MustCall(mori, "member.update_perms", m{"course_id": b.course, "member_id": s.seat, "perms": m{string(p): l.String()}},
					fmt.Sprintf("%s-%s-%s", s.seat, p, l))
				switch {
				case l <= ceiling && out.Status != domain.StatusExecuted:
					t.Fatalf("%s, %s to %s, within its ceiling %s: %+v", s.name, p, l, ceiling, out)
				case l > ceiling:
					aboveCeiling(t, fmt.Sprintf("%s, %s to %s, above its ceiling %s", s.name, p, l, ceiling), out, string(p), why)
					if out.Error.Details["ceiling"] != ceiling.String() {
						t.Fatalf("%s, %s: the refusal says the ceiling is %v, the view %s", s.name, p, out.Error.Details["ceiling"], ceiling)
					}
				}
				b.Exec(fmt.Sprintf(`UPDATE course_member SET %s = $2 WHERE id = $1`, p.Column()), s.seat, was)
			}
		}
		// Told everything behind everyone's back, the seat may do exactly
		// its ceilings, and says so of itself.
		for _, p := range domain.AllPerms {
			b.Exec(fmt.Sprintf(`UPDATE course_member SET %s = 'autonomous' WHERE id = $1`, p.Column()), s.seat)
		}
		me := b.membership(t, s.actor)
		for _, p := range domain.AllPerms {
			if me.Perms[string(p)] != v.PermCeilings[string(p)] || me.PermCeilings[string(p)] != v.PermCeilings[string(p)] ||
				me.PermCeilingReasons[string(p)] != v.PermCeilingReasons[string(p)] {
				t.Errorf("%s, told autonomous in everything: %s is %s, ceiling %s (%s); the view says %s (%s)", s.name, p,
					me.Perms[string(p)], me.PermCeilings[string(p)], me.PermCeilingReasons[string(p)], v.PermCeilings[string(p)], v.PermCeilingReasons[string(p)])
			}
		}
	}

	// A delegate's ceilings, before it is seated, are what member.add_delegate
	// accepts: what member.delegate_defaults says of them is the same.
	for _, owner := range []uuid.UUID{b.sato, b.yuki, hana, rin} {
		d := testkit.Result[tools.DelegateDefaultsOut](t, b.do(t, owner, "member.delegate_defaults", m{"course_id": b.course}))
		another := b.agent(t, owner, "another")
		for _, p := range domain.AllPerms {
			ceiling, _ := domain.ParseLevel(d.PermCeilings[string(p)])
			if ceiling == domain.Autonomous {
				continue
			}
			above := levels[int(ceiling)+1]
			out := b.MustCall(owner, "member.add_delegate", m{"course_id": b.course, "actor_id": another,
				"perms": m{string(p): above.String()}}, fmt.Sprintf("above-%s-%s", owner, p))
			if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Details["reason"] != d.PermCeilingReasons[string(p)] {
				t.Fatalf("naming %s at %s for an agent whose ceiling is %s (%s): %+v", p, above, ceiling, d.PermCeilingReasons[string(p)], out)
			}
		}
	}
}
