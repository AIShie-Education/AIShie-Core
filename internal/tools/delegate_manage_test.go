package tools_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// A person's agent may be given member_manage, as far as its principal holds
// it, and then manages the course's members for its principal: every seat
// but its principal's own and its principal's other agents'.

// helper seats an agent of Sato's as his delegate with the instructor preset,
// clipped to what he holds, and member_manage at level.
func (b *built) helper(t *testing.T, name, level string) (agent, seat uuid.UUID) {
	t.Helper()
	agent = b.agent(t, b.sato, name)
	return agent, b.delegate(t, b.sato, agent, m{"preset": "instructor", "perms": m{"member_manage": level}})
}

// mori seats a second instructor, who decides what Sato's agents propose and
// changes Sato's own seat.
func (b *built) mori(t *testing.T) uuid.UUID {
	t.Helper()
	mori := b.person(t, "Mori", "")
	b.do(t, b.admin, "course.seat_instructor", m{"course_id": b.course, "actor_id": mori})
	return mori
}

// notYours insists a call was refused as acting on the caller's principal or
// its other agents.
func notYours(t *testing.T, what string, out pipeline.Outcome) {
	t.Helper()
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || reason(out) != "not_your_principal" {
		t.Fatalf("%s: %+v", what, out)
	}
}

func TestADelegateGivenMemberManageManagesTheCoursesMembers(t *testing.T) {
	b := build(t)
	helper, seat := b.helper(t, "Sato's enrolment helper", "autonomous")
	v := b.memberView(t, seat)
	if v.Perms["member_manage"] != "autonomous" || v.Perms["agent_delegate"] != "denied" || v.Perms["member_invite"] != "denied" ||
		v.StudentScope != "all" {
		t.Fatalf("the helper's seat: %+v", v)
	}
	if me := b.membership(t, helper); me.Perms["member_manage"] != "autonomous" {
		t.Fatalf("the helper's own membership: %+v", me.Perms)
	}

	// It seats a student, as the student preset gives the seat — a student
	// may ask to bring an agent of their own, which Sato may allow, and so
	// may his agent for him — and changes, pauses, resumes, rescopes and
	// removes an ordinary member.
	hana := b.person(t, "Hana", "")
	hanaM := testkit.Result[tools.MemberIDOut](t, b.do(t, helper, "member.add",
		m{"course_id": b.course, "actor_id": hana, "preset": "student"})).MemberID
	if v := b.memberView(t, hanaM); v.Role != "student" || v.Perms["agent_delegate"] != "confirm_required" {
		t.Fatalf("the student it seated: %+v", v)
	}
	b.do(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": hanaM, "perms": m{"conversation_ask": "denied"}})
	b.do(t, helper, "member.pause", m{"course_id": b.course, "member_id": hanaM})
	b.do(t, helper, "member.resume", m{"course_id": b.course, "member_id": hanaM})
	b.do(t, helper, "member.rescope", m{"course_id": b.course, "member_id": hanaM, "expires_at": time.Now().Add(30 * 24 * time.Hour)})
	b.do(t, helper, "member.remove", m{"course_id": b.course, "member_id": hanaM})
	b.do(t, helper, "member.update_perms_bulk", m{"course_id": b.course, "role": "student", "perms": m{"conversation_ask": "confirm_required"}})
	if got := b.memberView(t, b.yukiM).Perms["conversation_ask"]; got != "confirm_required" {
		t.Fatalf("after the helper's bulk change: %q", got)
	}
	// Someone else's agent is an ordinary member to it.
	yukis := b.agent(t, b.yuki, "Yuki's helper")
	yukiSeat := b.delegate(t, b.yuki, yukis, m{})
	b.do(t, helper, "member.pause", m{"course_id": b.course, "member_id": yukiSeat})
	// It still brings in no agents of its own.
	if out := b.MustCall(helper, "member.add_delegate", m{"course_id": b.course, "actor_id": yukis}, "helper-brings"); out.Status != domain.StatusDenied {
		t.Fatalf("a delegate bringing an agent: %+v", out)
	}
}

func TestADelegateManagesMembersWithApprovalWhenThatIsItsLevel(t *testing.T) {
	b := build(t)
	mori := b.mori(t)
	helper, _ := b.helper(t, "Sato's enrolment helper", "confirm_required")
	hana := b.person(t, "Hana", "")
	out := b.MustCall(helper, "member.add", m{"course_id": b.course, "actor_id": hana, "preset": "student"}, "helper-asks")
	if out.Status != domain.StatusProposed {
		t.Fatalf("with member_manage at confirm_required: %+v", out)
	}
	// Its owner seats students without anyone's confirmation, so he may
	// decide what his agent proposes to do the same (docs/schema.md §2.6).
	if d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide",
		m{"course_id": b.course, "action_id": out.ActionID, "decision": "approve"})); d.Outcome != domain.StatusExecuted || !d.ByOwner {
		t.Fatalf("Sato approving his agent's proposal: %+v", d)
	}
	// Were his own seating to wait for a confirmation, his agent's would be
	// someone else's to decide.
	b.Exec(`UPDATE course_member SET perm_member_manage = 'confirm_required' WHERE id = $1`, b.satoM)
	ren := b.person(t, "Ren", "")
	out = b.MustCall(helper, "member.add", m{"course_id": b.course, "actor_id": ren, "preset": "student"}, "helper-asks-again")
	if out.Status != domain.StatusProposed {
		t.Fatalf("with member_manage at confirm_required: %+v", out)
	}
	if d := b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": out.ActionID, "decision": "approve"}, "sato-decides"); d.Status == domain.StatusExecuted {
		t.Fatalf("Sato approved his own agent's proposal while his own seating waits for a confirmation: %+v", d)
	}
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, mori, "action.decide", m{"course_id": b.course, "action_id": out.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusExecuted {
		t.Fatalf("Mori approving: %+v", d)
	}
}

// Whatever its row says, a delegate manages as its principal may: no more
// than he holds, over no more than he reaches, for no longer than he lasts,
// and from the moment he holds less.
func TestADelegatesManagingIsCappedByItsPrincipal(t *testing.T) {
	b := build(t)
	mori := b.mori(t)
	helper, seat := b.helper(t, "Sato's enrolment helper", "autonomous")
	sato := func(perms m) {
		t.Helper()
		b.do(t, mori, "member.update_perms", m{"course_id": b.course, "member_id": b.satoM, "perms": perms})
	}

	// No level above his.
	sato(m{"grade_post": "confirm_required"})
	out := b.MustCall(helper, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"grade_post": "autonomous"}}, "above")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["permission"] != "grade_post" {
		t.Fatalf("granting above its principal: %+v", out)
	}
	b.do(t, helper, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"grade_post": "confirm_required"}})
	// Nor member_manage itself.
	sato(m{"member_manage": "pending_review"})
	out = b.MustCall(helper, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{"member_manage": "autonomous"}}, "manage")
	if out.Status != domain.StatusFailed || out.Error.Details["permission"] != "member_manage" {
		t.Fatalf("granting member_manage above its principal: %+v", out)
	}

	// His member_manage lowered, the helper's is, at once: its row still
	// says autonomous.
	sato(m{"member_manage": "confirm_required"})
	if v := b.memberView(t, seat); v.Perms["member_manage"] != "autonomous" {
		t.Fatalf("the helper's row: %+v", v.Perms)
	}
	if me := b.membership(t, helper); me.Perms["member_manage"] != "confirm_required" {
		t.Fatalf("the helper's membership: %+v", me.Perms)
	}
	out = b.MustCall(helper, "member.pause", m{"course_id": b.course, "member_id": b.kenM}, "capped")
	if out.Status != domain.StatusProposed {
		t.Fatalf("with its principal at confirm_required: %+v", out)
	}
	sato(m{"member_manage": "denied"})
	out = b.MustCall(helper, "member.pause", m{"course_id": b.course, "member_id": b.yukiM}, "none")
	if out.Status != domain.StatusDenied || reason(out) != "permission_denied" {
		t.Fatalf("with its principal at denied: %+v", out)
	}
	sato(m{"member_manage": "autonomous"})

	// Nor further than he reaches: his list binds it, though its own row,
	// never narrowed with his, says the whole class.
	b.do(t, mori, "member.rescope", m{"course_id": b.course, "member_id": b.satoM, "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	hana := b.person(t, "Hana", "")
	out = b.MustCall(helper, "member.add", m{"course_id": b.course, "actor_id": hana, "preset": "student"}, "listed")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || !strings.Contains(out.Error.Message, "principal's student scope") {
		t.Fatalf("seating a student its principal does not reach: %+v", out)
	}
	b.do(t, mori, "member.rescope", m{"course_id": b.course, "member_id": b.satoM, "student_scope": "all"})

	// Nor for longer than he lasts.
	ends := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
	b.do(t, mori, "member.rescope", m{"course_id": b.course, "member_id": b.satoM, "expires_at": ends})
	out = b.MustCall(helper, "member.add", m{"course_id": b.course, "actor_id": hana, "preset": "student"}, "forever")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || !strings.Contains(out.Error.Message, "membership ends") {
		t.Fatalf("seating a student for longer than its principal lasts: %+v", out)
	}
	b.do(t, helper, "member.add", m{"course_id": b.course, "actor_id": hana, "preset": "student", "expires_at": ends})
}

// An agent that manages the course's members for its principal never acts on
// its principal's seat, nor on its principal's other agents', whichever tool
// it calls: it must not unmake the authority it acts with, nor reshape its
// owner's other agents.
func TestADelegateManagesNeitherItsPrincipalNorItsPrincipalsOtherAgents(t *testing.T) {
	b := build(t)
	helper, seat := b.helper(t, "Sato's enrolment helper", "autonomous")
	other := b.agent(t, b.sato, "Sato's course tutor")
	otherSeat := b.delegate(t, b.sato, other, m{"preset": "course_tutor"})

	for _, target := range []struct {
		name string
		id   uuid.UUID
	}{{"its principal", b.satoM}, {"its principal's other agent", otherSeat}} {
		for _, call := range []struct {
			tool string
			args m
		}{
			{"member.update_perms", m{"perms": m{"grade_read": "denied"}}},
			{"member.rescope", m{"expires_at": time.Now().Add(24 * time.Hour)}},
			{"member.pause", m{}},
			{"member.remove", m{}},
		} {
			call.args["course_id"], call.args["member_id"] = b.course, target.id
			notYours(t, call.tool+" on "+target.name, b.MustCall(helper, call.tool, call.args, "on-"+uuid.NewString()))
		}
	}
	// Resuming the other agent, paused by Sato: refused as well. (A paused
	// principal pauses the helper, which then does nothing at all.)
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": otherSeat})
	notYours(t, "member.resume on its principal's other agent",
		b.MustCall(helper, "member.resume", m{"course_id": b.course, "member_id": otherSeat}, "resume-other"))
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": otherSeat})

	// A change to every seat of a role refuses whole when the role takes in
	// his seat or another of his agents', and changes nothing; the students'
	// it changes.
	before := b.memberView(t, b.graderM).Perms["grade_submit"]
	for _, role := range []string{"instructor", "assistant"} {
		notYours(t, "member.update_perms_bulk on every "+role, b.MustCall(helper, "member.update_perms_bulk",
			m{"course_id": b.course, "role": role, "perms": m{"grade_submit": "denied"}}, "bulk-"+role))
	}
	if got := b.memberView(t, b.graderM).Perms["grade_submit"]; got != before {
		t.Fatalf("a refused bulk change changed the grader: %s → %s", before, got)
	}
	if bulk := testkit.Result[tools.MemberUpdatePermsBulkOut](t, b.do(t, helper, "member.update_perms_bulk",
		m{"course_id": b.course, "role": "student", "perms": m{"grade_read": "pending_review"}})); bulk.Updated != 2 {
		t.Fatalf("the students: %+v", bulk)
	}

	// Nothing of his, or of his other agent's, changed; and the helper's
	// own seat is its principal's to change, never its own.
	if v := b.memberView(t, b.satoM); v.Status != domain.MemberActive || v.Perms["grade_read"] != "autonomous" || v.ExpiresAt != nil {
		t.Fatalf("Sato's seat: %+v", v)
	}
	if v := b.memberView(t, otherSeat); v.Status != domain.MemberActive || v.ExpiresAt != nil {
		t.Fatalf("the other agent's seat: %+v", v)
	}
	if out := b.MustCall(helper, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"grade_read": "denied"}}, "own"); out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden {
		t.Fatalf("on its own seat: %+v", out)
	}
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"member_manage": "denied"}})
}

// Whatever its preset carries, an agent is seated without member_manage, as
// without member_invite: its owner names it, and never above what they hold.
func TestADelegateHoldsMemberManageOnlyWhenNamed(t *testing.T) {
	b := build(t)
	for _, preset := range []string{"delegate", "course_tutor", "instructor"} {
		bot := b.agent(t, b.sato, "Sato's "+preset)
		seat := b.delegate(t, b.sato, bot, m{"preset": preset})
		if got := b.memberView(t, seat).Perms["member_manage"]; got != "denied" {
			t.Errorf("a delegate seated with %s: member_manage %q", preset, got)
		}
		defaults := testkit.Result[tools.DelegateDefaultsOut](t, b.do(t, b.sato, "member.delegate_defaults",
			m{"course_id": b.course, "preset": preset}))
		if got := defaults.Perms["member_manage"]; got != "denied" {
			t.Errorf("member.delegate_defaults with %s: member_manage %q", preset, got)
		}
	}
	// A student manages nobody, and names it for no agent of hers.
	yukis := b.agent(t, b.yuki, "Yuki's helper")
	out := b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": yukis, "perms": m{"member_manage": "confirm_required"}}, "yuki")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["permission"] != "member_manage" {
		t.Fatalf("a student naming member_manage for her agent: %+v", out)
	}
	// And agent_delegate is never a delegate's.
	bot := b.agent(t, b.sato, "Sato's other")
	out = b.MustCall(b.sato, "member.add_delegate", m{"course_id": b.course, "actor_id": bot, "perms": m{"agent_delegate": "autonomous"}}, "sato")
	if out.Status != domain.StatusFailed || out.Error.Details["permission"] != "agent_delegate" {
		t.Fatalf("naming agent_delegate for an agent: %+v", out)
	}
}
