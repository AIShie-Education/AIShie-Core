package tools_test

import (
	"testing"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// The agent of someone who does not manage the course's members — a
// student's — may be given anything its principal holds, but what the
// built-in delegate preset does not give it does only by proposal: seated
// so, raised so by anyone, and so at every call, whatever its row says.

// The built-in delegate preset, as the seed makes it, is what domain knows it
// to be: the levels a student's agent holds unconfirmed.
func TestTheDelegatePresetIsWhatDelegateCapKnows(t *testing.T) {
	b := build(t)
	for _, p := range testkit.Result[tools.PresetListOut](t, b.do(t, b.sato, "preset.list", m{})).Presets {
		if p.Name != tools.DelegatePreset {
			continue
		}
		for _, perm := range domain.AllPerms {
			if got, want := p.Perms[string(perm)], domain.DelegatePresetLevels[perm].String(); got != want {
				t.Errorf("the delegate preset's %s is %s; domain.DelegatePresetLevels says %s", perm, got, want)
			}
		}
		return
	}
	t.Fatal("no built-in delegate preset")
}

func TestAStudentsAgentDraftsTheirWorkOnlyByProposal(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	// Yuki may bring it in only with an instructor's approval (b.delegate
	// has Sato approve). The student preset's writes come to it at
	// confirm_required; its reads as they are.
	seat := b.delegate(t, b.yuki, bot, m{"preset": "student"})
	v := b.memberView(t, seat)
	if v.Perms["submission_write"] != "confirm_required" || v.Perms["conversation_ask"] != "confirm_required" ||
		v.Perms["document_read"] != "autonomous" || v.Perms["submission_read"] != "autonomous" || v.Perms["grade_read"] != "autonomous" ||
		v.Perms["agent_delegate"] != "denied" || v.Perms["member_invite"] != "denied" ||
		len(v.ListedStudents) != 1 || v.ListedStudents[0] != b.yukiM {
		t.Fatalf("Yuki's agent: %+v", v)
	}

	// It drafts her HW3: a proposal, nothing written yet.
	out := b.MustCall(bot, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.yukiM,
		"body": "A draft."}, "draft")
	if out.Status != domain.StatusProposed {
		t.Fatalf("the agent drafting Yuki's work: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM submission WHERE student_member_id = $1`, b.yukiM); n != 0 {
		t.Fatalf("%d submissions before anyone confirmed", n)
	}

	// Nobody raises it past confirm_required, however much Yuki holds.
	for _, level := range []string{"pending_review", "autonomous"} {
		out := b.MustCall(b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat,
			"perms": m{"submission_write": level}}, "raise-"+level)
		if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["permission"] != "submission_write" {
			t.Fatalf("raising Yuki's agent to %s: %+v", level, out)
		}
	}
	bot2 := b.agent(t, b.yuki, "Yuki's other helper")
	out = b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": bot2, "perms": m{"submission_write": "autonomous"}}, "named")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || out.Error.Details["permission"] != "submission_write" {
		t.Fatalf("Yuki naming submission_write autonomous for her agent: %+v", out)
	}
	// Nor does its row: authorization asks at every call.
	b.Exec(`UPDATE course_member SET perm_submission_write = 'autonomous' WHERE id = $1`, seat)
	if me := b.membership(t, bot); me.Perms["submission_write"] != "confirm_required" {
		t.Fatalf("the agent's own membership: %+v", me.Perms)
	}
	out = b.MustCall(bot, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.yukiM,
		"body": "Another draft."}, "draft-2")
	if out.Status != domain.StatusProposed {
		t.Fatalf("the agent drafting with its row saying autonomous: %+v", out)
	}

	// Once she manages the course's members, it is capped by her level
	// alone, as an instructor's agent is.
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"member_manage": "confirm_required"}})
	if me := b.membership(t, bot); me.Perms["submission_write"] != "autonomous" {
		t.Fatalf("the agent of a student who manages members: %+v", me.Perms)
	}
}

func TestAnInstructorsAgentIsCappedByTheInstructorAlone(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.sato, "Sato's assistant")
	seat := b.delegate(t, b.sato, bot, m{"preset": "instructor"})
	if v := b.memberView(t, seat); v.Perms["assignment_write"] != "autonomous" || v.Perms["grade_post"] != "autonomous" {
		t.Fatalf("Sato's agent: %+v", v.Perms)
	}
	out := b.MustCall(bot, "assignment.create", m{"course_id": b.course, "title": "HW4", "points_possible": 10}, "hw4")
	if out.Status != domain.StatusExecuted {
		t.Fatalf("Sato's agent writing an assignment: %+v", out)
	}
}
