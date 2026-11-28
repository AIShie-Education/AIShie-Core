package tools_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

type runtimeChatDecisionArgs struct {
	actor, agent, course, opener uuid.UUID
	subject, operation           string
}

func defaultRuntimeChatDecisionArgs(b *built) runtimeChatDecisionArgs {
	return runtimeChatDecisionArgs{
		actor: b.sato, agent: b.tutor, course: b.course, opener: b.yuki,
		subject: tools.RuntimeChatSubjectQuestion, operation: tools.RuntimeChatOperationDecide,
	}
}

func checkRuntimeChatDecision(t *testing.T, b *built, args runtimeChatDecisionArgs) (bool, map[string]any) {
	t.Helper()
	out := b.asRuntime(t, "agent_runtime.check_chat_decision", m{
		"actor_id": args.actor, "agent_id": args.agent, "course_id": args.course,
		"opener_actor_id": args.opener, "subject": args.subject, "operation": args.operation,
	})
	if out.Status != domain.StatusExecuted {
		t.Fatalf("check_chat_decision: %+v", out)
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Result, &raw); err != nil {
		t.Fatal(err)
	}
	result := testkit.Result[tools.RuntimeCheckChatDecisionOut](t, out)
	if result.Allowed {
		if result.ActionDecideLevel == "" || len(raw) != 2 || raw["allowed"] != true ||
			raw["action_decide_level"] != result.ActionDecideLevel {
			t.Fatalf("an allowed result must contain exactly allowed and its effective level: %+v, %v", result, raw)
		}
	} else if result.ActionDecideLevel != "" || len(raw) != 1 || raw["allowed"] != false {
		t.Fatalf("a denied result leaked a level or other detail: %+v, %v", result, raw)
	}
	return result.Allowed, raw
}

func wantRuntimeChatDecision(t *testing.T, b *built, args runtimeChatDecisionArgs, want bool) {
	t.Helper()
	got, raw := checkRuntimeChatDecision(t, b, args)
	if got != want {
		t.Fatalf("%s/%s: allowed %v, want %v (%v)", args.subject, args.operation, got, want, raw)
	}
}

func wantRuntimeChatDecisionLevel(t *testing.T, b *built, args runtimeChatDecisionArgs, want string) {
	t.Helper()
	got, raw := checkRuntimeChatDecision(t, b, args)
	if !got || raw["action_decide_level"] != want {
		t.Fatalf("%s/%s: allowed %v with level %v, want allowed at %s", args.subject, args.operation, got, raw["action_decide_level"], want)
	}
}

func allRuntimeChatJudgments(args runtimeChatDecisionArgs) []runtimeChatDecisionArgs {
	out := make([]runtimeChatDecisionArgs, 0, 4)
	for _, subject := range []string{tools.RuntimeChatSubjectQuestion, tools.RuntimeChatSubjectAnswer} {
		for _, operation := range []string{tools.RuntimeChatOperationDecide, tools.RuntimeChatOperationReview} {
			a := args
			a.subject, a.operation = subject, operation
			out = append(out, a)
		}
	}
	return out
}

func TestRuntimeCheckChatDecisionIsReadOnlyAndServiceOnly(t *testing.T) {
	b := build(t)
	before := []int{
		b.Count(`SELECT count(*) FROM conversation`),
		b.Count(`SELECT count(*) FROM conversation_message`),
		b.Count(`SELECT count(*) FROM action`),
		b.Count(`SELECT count(*) FROM event`),
	}
	for _, args := range allRuntimeChatJudgments(defaultRuntimeChatDecisionArgs(b)) {
		wantRuntimeChatDecision(t, b, args, true)
	}
	after := []int{
		b.Count(`SELECT count(*) FROM conversation`),
		b.Count(`SELECT count(*) FROM conversation_message`),
		b.Count(`SELECT count(*) FROM action`),
		b.Count(`SELECT count(*) FROM event`),
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("check_chat_decision changed table %d: before %d, after %d", i, before[i], after[i])
		}
	}

	tl, ok := b.P.Registry().Get("agent_runtime.check_chat_decision")
	if !ok || tl.Kind != tool.Read || tl.Gate.Service != domain.ServiceAgentRuntime {
		t.Fatalf("the check_chat_decision declaration: %+v, %v", tl, ok)
	}
	const path = "/v1/services/agent_runtime/chat-decisions/check"
	if tl.HTTP.Method != "POST" || tl.HTTP.Pattern != path {
		t.Fatalf("check_chat_decision's route: %+v", tl.HTTP)
	}
	level, hasLevel := tl.OutputSchema.Properties["action_decide_level"]
	if _, ok := tl.OutputSchema.Properties["allowed"]; !ok || !hasLevel || len(tl.OutputSchema.Properties) != 2 ||
		len(tl.OutputSchema.Required) != 1 || tl.OutputSchema.Required[0] != "allowed" {
		t.Fatalf("check_chat_decision's output schema: %+v", tl.OutputSchema)
	}
	if level == nil {
		t.Fatal("action_decide_level has no schema")
	}
	for _, args := range allRuntimeChatJudgments(defaultRuntimeChatDecisionArgs(b)) {
		wantRuntimeChatDecisionLevel(t, b, args, domain.Autonomous.String())
	}
	for _, name := range []string{"actor_id", "agent_id", "course_id", "opener_actor_id", "subject", "operation"} {
		if _, ok := tl.InputSchema.Properties[name]; !ok {
			t.Errorf("check_chat_decision's input has no %s: %+v", name, tl.InputSchema)
		}
	}

	args := defaultRuntimeChatDecisionArgs(b)
	payload := m{"actor_id": args.actor, "agent_id": args.agent, "course_id": args.course,
		"opener_actor_id": args.opener, "subject": args.subject, "operation": args.operation}
	out, err := b.Call(b.sato, "agent_runtime.check_chat_decision", payload, "")
	if err != nil || out.Status != domain.StatusDenied || reason(out) != "service_only" {
		t.Fatalf("a person called the service tool: %+v %v", out, err)
	}
}

func TestRuntimeCheckChatDecisionValidatesItsClosedEnums(t *testing.T) {
	b := build(t)
	for _, tc := range []struct{ subject, operation string }{
		{"message", tools.RuntimeChatOperationDecide},
		{"", tools.RuntimeChatOperationReview},
		{tools.RuntimeChatSubjectQuestion, "approve"},
		{tools.RuntimeChatSubjectAnswer, ""},
	} {
		args := defaultRuntimeChatDecisionArgs(b)
		args.subject, args.operation = tc.subject, tc.operation
		out, err := b.CallWith(b.AsRuntime(), "agent_runtime.check_chat_decision", m{
			"actor_id": args.actor, "agent_id": args.agent, "course_id": args.course,
			"opener_actor_id": args.opener, "subject": args.subject, "operation": args.operation,
		}, "")
		if err == nil || !apperr.Is(err, apperr.InvalidArgument) || out.Status != "" {
			t.Fatalf("invalid subject/operation %q/%q: %+v %v", tc.subject, tc.operation, out, err)
		}
	}
}

func TestRuntimeCheckChatDecisionReportsEveryAllowedReviewerLevel(t *testing.T) {
	b := build(t)
	for _, level := range []domain.Level{domain.ConfirmRequired, domain.PendingReview, domain.Autonomous} {
		b.Exec(`UPDATE course_member SET perm_action_decide = $2 WHERE id = $1`, b.satoM, level.String())
		for _, args := range allRuntimeChatJudgments(defaultRuntimeChatDecisionArgs(b)) {
			wantRuntimeChatDecisionLevel(t, b, args, level.String())
		}
	}
}

func TestRuntimeCheckChatDecisionUsesStaffAndPrincipalScope(t *testing.T) {
	b := build(t)
	mori := b.person(t, "Mori", "")
	moriM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{
		"course_id": b.course, "actor_id": mori, "preset": "instructor",
		"student_scope": "listed", "listed_students": []uuid.UUID{b.kenM},
	})).MemberID
	respondent := b.runtimeAgent(t, b.sato, "Course tutor")
	b.delegate(t, b.sato, respondent, m{"preset": "course_tutor"})
	b.Host(respondent)

	base := defaultRuntimeChatDecisionArgs(b)
	base.actor, base.agent = mori, respondent
	for _, args := range allRuntimeChatJudgments(base) {
		args.opener = b.ken
		wantRuntimeChatDecision(t, b, args, true)
		args.opener = b.yuki
		wantRuntimeChatDecision(t, b, args, false)
	}

	// A delegate's own all scope does not widen its principal's listed scope.
	// Its effective action_decide is confirm_required, which is enough for an
	// unrelated staff judge just as it is for action.decide/review themselves.
	watcher := b.runtimeAgent(t, mori, "Mori's watcher")
	watcherM := b.delegate(t, mori, watcher, m{
		"preset": "ta", "student_scope": "listed", "listed_students": []uuid.UUID{b.kenM},
		"perms": m{"action_decide": "confirm_required"},
	})
	// Model a principal narrowed after seating: nothing updates the
	// delegate's own scope row, but every authorization still intersects it.
	b.Exec(`UPDATE course_member SET student_scope = 'all', perm_action_decide = 'autonomous' WHERE id = $1`, watcherM)
	if got := b.memberView(t, watcherM); got.StudentScope != "all" || got.Perms["action_decide"] != "confirm_required" {
		t.Fatalf("the delegated watcher's effective seat: %+v", got)
	}
	if got := b.memberView(t, moriM); got.StudentScope != "listed" {
		t.Fatalf("the watcher's principal: %+v", got)
	}
	base.actor = watcher
	for _, args := range allRuntimeChatJudgments(base) {
		args.opener = b.ken
		wantRuntimeChatDecisionLevel(t, b, args, domain.ConfirmRequired.String())
		args.opener = b.yuki
		wantRuntimeChatDecision(t, b, args, false)
	}
}

func TestRuntimeCheckChatDecisionQuestionFourEyes(t *testing.T) {
	b := build(t)
	// Yuki otherwise oversees her own seat once action_decide is granted.
	// That does not let her decide or review her own question.
	b.Exec(`UPDATE course_member SET perm_action_decide = 'autonomous' WHERE id = $1`, b.yukiM)
	for _, operation := range []string{tools.RuntimeChatOperationDecide, tools.RuntimeChatOperationReview} {
		args := defaultRuntimeChatDecisionArgs(b)
		args.actor, args.opener, args.operation = b.yuki, b.yuki, operation
		wantRuntimeChatDecision(t, b, args, false)
	}

	// The opener's agent is the same party too, even with its own scope and
	// an allowed (delegate-capped) action_decide level.
	bot := b.runtimeAgent(t, b.yuki, "Yuki's assistant")
	botM := b.delegate(t, b.yuki, bot, m{"perms": m{"action_decide": "confirm_required"}})
	b.Exec(`UPDATE course_member SET student_scope = 'all' WHERE id = $1`, botM)
	if got := b.memberView(t, botM); got.Perms["action_decide"] != "confirm_required" {
		t.Fatalf("Yuki's assistant: %+v", got)
	}
	for _, operation := range []string{tools.RuntimeChatOperationDecide, tools.RuntimeChatOperationReview} {
		args := defaultRuntimeChatDecisionArgs(b)
		args.actor, args.opener, args.operation = bot, b.yuki, operation
		wantRuntimeChatDecision(t, b, args, false)
	}
}

func TestRuntimeCheckChatDecisionAnswerOwnerAndPartyRules(t *testing.T) {
	c := newCast(t)
	b := c.built
	respondent := b.seatActor(t, c.courseTutor)
	base := defaultRuntimeChatDecisionArgs(b)
	base.actor, base.agent, base.opener = b.sato, respondent, b.ken

	// The respondent's owner may judge through the owner relationship even
	// without overseeing Ken. No staff scope check is smuggled into that
	// existing owner exception.
	b.Exec(`UPDATE course_member SET student_scope = 'listed' WHERE id = $1`, b.satoM)
	b.Exec(`DELETE FROM member_student_scope WHERE member_id = $1`, b.satoM)
	b.Exec(`INSERT INTO member_student_scope (member_id, student_member_id) VALUES ($1, $2)`, b.satoM, b.yukiM)
	for _, args := range allRuntimeChatJudgments(base) {
		if args.subject == tools.RuntimeChatSubjectAnswer {
			wantRuntimeChatDecision(t, b, args, true)
		}
	}

	// The owner exception alone requires autonomous action_decide. This is
	// the same decidesAnswers rule used for Core answer drafts.
	for _, level := range []domain.Level{domain.Denied, domain.ConfirmRequired, domain.PendingReview} {
		b.Exec(`UPDATE course_member SET perm_action_decide = $2 WHERE id = $1`, b.satoM, level.String())
		for _, operation := range []string{tools.RuntimeChatOperationDecide, tools.RuntimeChatOperationReview} {
			args := base
			args.subject, args.operation = tools.RuntimeChatSubjectAnswer, operation
			wantRuntimeChatDecision(t, b, args, false)
		}
	}
	b.Exec(`UPDATE course_member SET perm_action_decide = 'autonomous', student_scope = 'all' WHERE id = $1`, b.satoM)

	// The respondent and a sibling of it never satisfy four eyes. Give both
	// enough scope and action_decide that party membership is what denies them.
	b.Exec(`UPDATE course_member SET student_scope = 'all', perm_action_decide = 'autonomous' WHERE id = $1`, c.courseTutor)
	sibling := b.runtimeAgent(t, b.sato, "Sato's other tutor")
	siblingM := b.delegate(t, b.sato, sibling, m{
		"preset": "course_tutor", "student_scope": "all", "perms": m{"action_decide": "confirm_required"},
	})
	if got := b.memberView(t, siblingM); got.Perms["action_decide"] != "confirm_required" {
		t.Fatalf("the sibling's effective permission: %+v", got)
	}
	for _, actor := range []uuid.UUID{respondent, sibling} {
		for _, operation := range []string{tools.RuntimeChatOperationDecide, tools.RuntimeChatOperationReview} {
			args := base
			args.actor, args.subject, args.operation = actor, tools.RuntimeChatSubjectAnswer, operation
			wantRuntimeChatDecision(t, b, args, false)
		}
	}

	// An unrelated staff member is not held to the owner's autonomous-only
	// exception: every allowed action_decide level can decide/review, while
	// denied cannot.
	for _, level := range []domain.Level{domain.ConfirmRequired, domain.PendingReview, domain.Autonomous} {
		b.Exec(`UPDATE course_member SET perm_action_decide = $2 WHERE id = $1`, c.taM, level.String())
		for _, operation := range []string{tools.RuntimeChatOperationDecide, tools.RuntimeChatOperationReview} {
			args := base
			args.actor, args.subject, args.operation = c.ta, tools.RuntimeChatSubjectAnswer, operation
			wantRuntimeChatDecision(t, b, args, true)
		}
	}
	b.Exec(`UPDATE course_member SET perm_action_decide = 'denied' WHERE id = $1`, c.taM)
	for _, operation := range []string{tools.RuntimeChatOperationDecide, tools.RuntimeChatOperationReview} {
		args := base
		args.actor, args.subject, args.operation = c.ta, tools.RuntimeChatSubjectAnswer, operation
		wantRuntimeChatDecision(t, b, args, false)
	}
}

func TestRuntimeCheckChatDecisionOwnAgentOwnerNeedsAnswerAutonomy(t *testing.T) {
	c := newCast(t)
	b := c.built
	for _, level := range []domain.Level{domain.Denied, domain.ConfirmRequired, domain.PendingReview, domain.Autonomous} {
		b.Exec(`UPDATE course_member SET perm_action_decide = $2 WHERE id = $1`, b.yukiM, level.String())
		for _, operation := range []string{tools.RuntimeChatOperationDecide, tools.RuntimeChatOperationReview} {
			args := defaultRuntimeChatDecisionArgs(b)
			args.actor, args.agent, args.opener = b.yuki, c.bot, b.yuki
			args.subject, args.operation = tools.RuntimeChatSubjectAnswer, operation
			wantRuntimeChatDecision(t, b, args, level == domain.Autonomous)
		}
	}
}

func TestRuntimeCheckChatDecisionRefusalsAreFalseAndIndistinguishable(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(*testing.T, *built) runtimeChatDecisionArgs
	}{
		{"unknown actor", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			args := defaultRuntimeChatDecisionArgs(b)
			args.actor = uuid.New()
			return args
		}},
		{"unknown agent", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			args := defaultRuntimeChatDecisionArgs(b)
			args.agent = uuid.New()
			return args
		}},
		{"unknown course", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			args := defaultRuntimeChatDecisionArgs(b)
			args.course = uuid.New()
			return args
		}},
		{"unknown opener", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			args := defaultRuntimeChatDecisionArgs(b)
			args.opener = uuid.New()
			return args
		}},
		{"actor is not seated", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			args := defaultRuntimeChatDecisionArgs(b)
			args.actor = b.admin
			return args
		}},
		{"actor is suspended", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.do(t, b.admin, "actor.suspend", m{"actor_id": b.sato})
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"actor seat is paused", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, b.satoM)
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"actor seat is removed", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, b.satoM)
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"actor seat is expired", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE course_member SET expires_at = $2 WHERE id = $1`, b.satoM, time.Now().Add(-time.Hour))
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"actor must change a temporary password", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE actor SET email = 'sato-chat@example.edu' WHERE id = $1`, b.sato)
			b.Exec(`INSERT INTO credential (actor_id, kind, secret_hash, must_change, issued_by_actor_id) VALUES ($1, 'password', 'unused', true, $2)`, b.sato, b.admin)
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"action_decide is denied", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE course_member SET perm_action_decide = 'denied' WHERE id = $1`, b.satoM)
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"opener is not seated", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			args := defaultRuntimeChatDecisionArgs(b)
			args.opener = b.admin
			return args
		}},
		{"opener is suspended", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.do(t, b.admin, "actor.suspend", m{"actor_id": b.yuki})
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"opener seat is paused", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, b.yukiM)
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"opener seat is removed", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, b.yukiM)
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"agent is not seated", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			args := defaultRuntimeChatDecisionArgs(b)
			args.agent = b.runtimeAgent(t, b.sato, "Unseated agent")
			return args
		}},
		{"agent is a person", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			args := defaultRuntimeChatDecisionArgs(b)
			args.agent = b.ken
			return args
		}},
		{"agent is suspended", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.do(t, b.admin, "actor.suspend", m{"actor_id": b.tutor})
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"agent seat is paused", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, b.tutorM)
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"agent seat is removed", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, b.tutorM)
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"agent seat is expired", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.Exec(`UPDATE course_member SET expires_at = $2 WHERE id = $1`, b.tutorM, time.Now().Add(-time.Hour))
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"course is archived", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			b.do(t, b.admin, "course.archive", m{"course_id": b.course})
			return defaultRuntimeChatDecisionArgs(b)
		}},
		{"agent principal is paused", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			respondent := b.runtimeAgent(t, b.sato, "Owned course tutor")
			b.delegate(t, b.sato, respondent, m{"preset": "course_tutor"})
			mori := b.person(t, "Mori", "")
			b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "ta",
				"perms": m{"action_decide": "autonomous"}})
			b.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, b.satoM)
			args := defaultRuntimeChatDecisionArgs(b)
			args.actor, args.agent, args.opener = mori, respondent, b.ken
			return args
		}},
		{"agent owner is suspended", func(t *testing.T, b *built) runtimeChatDecisionArgs {
			respondent := b.runtimeAgent(t, b.sato, "Owned course tutor")
			b.delegate(t, b.sato, respondent, m{"preset": "course_tutor"})
			mori := b.person(t, "Mori", "")
			b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "ta",
				"perms": m{"action_decide": "autonomous"}})
			b.do(t, b.admin, "actor.suspend", m{"actor_id": b.sato})
			args := defaultRuntimeChatDecisionArgs(b)
			args.actor, args.agent, args.opener = mori, respondent, b.ken
			return args
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := build(t)
			args := tc.arrange(t, b)
			for _, judgment := range allRuntimeChatJudgments(args) {
				got, raw := checkRuntimeChatDecision(t, b, judgment)
				if got || len(raw) != 1 || raw["allowed"] != false {
					t.Fatalf("refusal leaked or was allowed: %v, %v", got, raw)
				}
			}
		})
	}
}
