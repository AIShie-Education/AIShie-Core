package domain

import (
	"testing"

	"github.com/google/uuid"
)

// The ceilings of the kinds of seat there are, and the reason each gives
// where two rules meet at one level: the one that holds whatever the
// principal holds.
func TestCeilingsAndTheirReasons(t *testing.T) {
	seat := func(levels map[Perm]Level) *Member {
		return &Member{ID: uuid.New(), Status: MemberActive, SeatValid: true, Perms: levels}
	}
	instructor := seat(map[Perm]Level{PermMemberManage: Autonomous, PermActionDecide: Autonomous, PermGradePost: PendingReview,
		PermConversationAsk: ConfirmRequired, PermAgentDelegate: Autonomous, PermConversationAnswer: Autonomous})
	cautious := seat(map[Perm]Level{PermMemberManage: Autonomous, PermActionDecide: ConfirmRequired})
	student := seat(map[Perm]Level{PermSubmissionWrite: Autonomous, PermGradeRead: Autonomous, PermConversationAsk: Autonomous})

	for _, tc := range []struct {
		name      string
		agent     bool
		principal *Member
		perm      Perm
		level     Level
		why       CeilingReason
	}{
		{"a person decides as given", false, nil, PermActionDecide, Autonomous, CeilingNone},
		{"a person brings agents as given", false, nil, PermAgentDelegate, Autonomous, CeilingNone},
		{"an agent nobody owns decides by proposal", true, nil, PermActionDecide, ConfirmRequired, CeilingAgentDecidesByProposal},
		{"an agent nobody owns posts as given", true, nil, PermGradePost, Autonomous, CeilingNone},
		{"a delegate brings no agents", false, instructor, PermAgentDelegate, Denied, CeilingAgentNever},
		{"a delegate decides by proposal", false, instructor, PermActionDecide, ConfirmRequired, CeilingAgentDecidesByProposal},
		{"and says so where its principal does too", false, cautious, PermActionDecide, ConfirmRequired, CeilingAgentDecidesByProposal},
		{"a delegate posts as its principal does", false, instructor, PermGradePost, PendingReview, CeilingPrincipalLevel},
		{"a delegate answers as its principal asks", false, instructor, PermConversationAnswer, ConfirmRequired, CeilingPrincipalLevel},
		{"a delegate manages as its principal does", false, instructor, PermMemberManage, Autonomous, CeilingNone},
		{"a student's agent drafts by proposal", false, student, PermSubmissionWrite, ConfirmRequired, CeilingStudentAgentByProposal},
		{"a student's agent reads as she does", false, student, PermGradeRead, Autonomous, CeilingNone},
		{"a student's agent posts nothing she cannot", false, student, PermGradePost, Denied, CeilingPrincipalLevel},
		{"a student's agent decides nothing she cannot", false, student, PermActionDecide, Denied, CeilingPrincipalLevel},
		{"a student's agent manages as she does", false, student, PermMemberManage, Denied, CeilingPrincipalLevel},
	} {
		level, why := Ceiling(tc.agent, tc.principal, tc.perm)
		if level != tc.level || why != tc.why {
			t.Errorf("%s: %s (%q), want %s (%q)", tc.name, level, why, tc.level, tc.why)
		}
		if tc.principal != nil && DelegateCap(tc.principal, tc.perm) != level {
			t.Errorf("%s: DelegateCap says %s, Ceiling %s", tc.name, DelegateCap(tc.principal, tc.perm), level)
		}
	}
}
