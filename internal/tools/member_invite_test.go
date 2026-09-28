package tools_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// member_invite, which makes a course's join links, is a permission like any
// other: on every seat and preset, shown wherever levels are, changed and
// granted as any level is. No preset an agent is seated with carries it, and
// a delegate holds it only when the call that seats it names it, never above
// its principal's.

func TestMemberInviteIsAPermissionLikeAnyOther(t *testing.T) {
	b := build(t)
	invite := string(domain.PermMemberInvite)

	// The built-ins: an instructor's hands out links; nobody else's does,
	// least of all an agent's.
	presets := testkit.Result[tools.PresetListOut](t, b.do(t, b.sato, "preset.list", m{})).Presets
	if len(presets) != 8 {
		t.Fatalf("%d built-in presets", len(presets))
	}
	for _, p := range presets {
		want := "denied"
		if p.Name == "instructor" {
			want = "autonomous"
		}
		if got := p.Perms[invite]; got != want {
			t.Errorf("the built-in %s: member_invite %q, want %s", p.Name, got, want)
		}
	}

	// Shown wherever a seat's levels are.
	if got := b.membership(t, b.sato).Perms[invite]; got != "autonomous" {
		t.Fatalf("Sato's me.memberships: member_invite %q", got)
	}
	if got := b.membership(t, b.yuki).Perms[invite]; got != "denied" {
		t.Fatalf("Yuki's me.memberships: member_invite %q", got)
	}
	if got := b.memberView(t, b.graderM).Perms[invite]; got != "denied" {
		t.Fatalf("the grader's member.get: member_invite %q", got)
	}

	// Changed on one seat, or on a role, as any level is.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{invite: "pending_review"}})
	if got := b.memberView(t, b.yukiM).Perms[invite]; got != "pending_review" {
		t.Fatalf("after member.update_perms: %q", got)
	}
	bulk := testkit.Result[tools.MemberUpdatePermsBulkOut](t, b.do(t, b.sato, "member.update_perms_bulk",
		m{"course_id": b.course, "role": "student", "perms": m{invite: "denied"}}))
	if bulk.Updated != 2 || b.memberView(t, b.yukiM).Perms[invite] != "denied" {
		t.Fatalf("after member.update_perms_bulk: %+v", bulk)
	}

	// Nobody grants more of it than they hold. Tanaka manages members and
	// holds no member_invite: he seats a student, and no instructor, as the
	// preset gives it, unless he names it denied.
	tanaka := b.person(t, "Tanaka", "")
	tanakaM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add",
		m{"course_id": b.course, "actor_id": tanaka, "preset": "instructor", "perms": m{invite: "denied"}})).MemberID
	out := b.MustCall(tanaka, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{invite: "autonomous"}}, "tanaka-raises")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["permission"] != invite {
		t.Fatalf("a manager without member_invite granting it: %+v", out)
	}
	mori := b.person(t, "Mori", "")
	out = b.MustCall(tanaka, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "instructor"}, "tanaka-seats")
	if out.Status != domain.StatusFailed || out.Error.Details["permission"] != invite {
		t.Fatalf("a manager without member_invite seating an instructor: %+v", out)
	}
	b.do(t, tanaka, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "instructor", "perms": m{invite: "denied"}})
	// Given it, he may.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": tanakaM, "perms": m{invite: "autonomous"}})
	b.do(t, tanaka, "member.update_perms", m{"course_id": b.course, "member_id": b.kenM, "perms": m{invite: "autonomous"}})

	// A department's own preset carries it as it is given.
	made := testkit.Result[tools.IDOut](t, b.do(t, b.admin, "preset.create", m{"dept_id": b.dept, "name": "head_ta", "role": "ta",
		"student_scope": "all", "assignment_scope": "all", "perms": m{"member_manage": "autonomous", invite: "autonomous"}})).ID
	for _, p := range testkit.Result[tools.PresetListOut](t, b.do(t, b.sato, "preset.list", m{"dept_id": b.dept})).Presets {
		if p.ID == made && p.Perms[invite] != "autonomous" {
			t.Fatalf("a department's preset: %+v", p)
		}
	}
}

func TestADelegateHoldsMemberInviteOnlyWhenNamedAndNoMoreThanItsPrincipal(t *testing.T) {
	b := build(t)
	invite := string(domain.PermMemberInvite)

	// Whatever the preset carries, an agent is seated without it: the
	// delegate presets carry none, and the instructor's is not handed on.
	for i, preset := range []string{"delegate", "course_tutor", "instructor"} {
		bot := b.agent(t, b.sato, "Sato's "+preset)
		seat := b.delegate(t, b.sato, bot, m{"preset": preset})
		if got := b.memberView(t, seat).Perms[invite]; got != "denied" {
			t.Errorf("%d: a delegate seated with %s: member_invite %q", i, preset, got)
		}
		defaults := testkit.Result[tools.DelegateDefaultsOut](t, b.do(t, b.sato, "member.delegate_defaults",
			m{"course_id": b.course, "preset": preset}))
		if got := defaults.Perms[invite]; got != "denied" {
			t.Errorf("member.delegate_defaults with %s: member_invite %q", preset, got)
		}
	}

	// Named, it is given, up to the principal's own level.
	helper := b.agent(t, b.sato, "Sato's enrolment helper")
	seat := b.delegate(t, b.sato, helper, m{"preset": "delegate", "perms": m{invite: "autonomous"}})
	if got := b.memberView(t, seat).Perms[invite]; got != "autonomous" {
		t.Fatalf("named: member_invite %q", got)
	}
	if got := b.membership(t, helper).Perms[invite]; got != "autonomous" {
		t.Fatalf("the delegate's own me.memberships: %q", got)
	}
	// Its principal holding less, it holds less, at once: its row is not
	// what counts.
	b.Exec(`UPDATE course_member SET perm_member_invite = 'confirm_required' WHERE id = $1`, b.satoM)
	if got := b.membership(t, helper).Perms[invite]; got != "confirm_required" {
		t.Fatalf("capped by its principal: %q", got)
	}
	b.Exec(`UPDATE course_member SET perm_member_invite = 'autonomous' WHERE id = $1`, b.satoM)

	// A student holds none, and names none for an agent of theirs; an
	// instructor raises it on someone else's agent no further than its
	// principal holds.
	yukis := b.agent(t, b.yuki, "Yuki's helper")
	out := b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": yukis, "perms": m{invite: "autonomous"}}, "yuki-names")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["permission"] != invite {
		t.Fatalf("a student naming member_invite for her agent: %+v", out)
	}
	yukiSeat := b.delegate(t, b.yuki, yukis, m{})
	out = b.MustCall(b.sato, "member.update_perms", m{"course_id": b.course, "member_id": yukiSeat, "perms": m{invite: "autonomous"}}, "sato-raises")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["permission"] != invite {
		t.Fatalf("raising a student's agent above the student: %+v", out)
	}
}

// person registers a person, with an email if one is given, as an
// administrator does.
func (b *built) person(t *testing.T, name, email string) uuid.UUID {
	t.Helper()
	args := m{"kind": "human", "display_name": name}
	if email != "" {
		args["email"] = email
	}
	return testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", args)).ActorID
}
