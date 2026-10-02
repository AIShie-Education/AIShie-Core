package tools

import (
	"sort"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
)

// permSet is a full set of the permissions, as a preset or a member
// row holds them.
type permSet map[domain.Perm]domain.Level

// PermLevels is a permSet on the wire: {"grade_submit": "confirm_required"}.
type PermLevels map[string]string

func (ps permSet) view() PermLevels {
	out := make(PermLevels, len(ps))
	for p, l := range ps {
		out[string(p)] = l.String()
	}
	return out
}

func (ps permSet) col(p domain.Perm) dbq.AutonomyLevel { return dbq.AutonomyLevel(ps[p].String()) }

// apply overlays named levels on the set. Unknown names and levels are
// refused: a typo must not silently leave a permission at its old value.
func (ps permSet) apply(over PermLevels) error {
	names := make([]string, 0, len(over))
	for n := range over {
		names = append(names, n)
	}
	sort.Strings(names) // so the first error reported is always the same one
	for _, n := range names {
		p := domain.Perm(n)
		if !p.Valid() {
			return apperr.Invalid("there is no permission named %q", n)
		}
		l, err := domain.ParseLevel(over[n])
		if err != nil {
			return apperr.Invalid("%q is not a level; use denied, confirm_required, pending_review or autonomous", over[n])
		}
		ps[p] = l
	}
	return nil
}

func level(a dbq.AutonomyLevel) domain.Level {
	l, _ := domain.ParseLevel(string(a))
	return l
}

func presetPerms(p dbq.PermissionPreset) permSet {
	return permSet{
		domain.PermDocumentRead: level(p.PermDocumentRead), domain.PermDocumentReadDraft: level(p.PermDocumentReadDraft),
		domain.PermDocumentWrite: level(p.PermDocumentWrite), domain.PermRubricRead: level(p.PermRubricRead),
		domain.PermAssignmentWrite: level(p.PermAssignmentWrite), domain.PermSubmissionRead: level(p.PermSubmissionRead),
		domain.PermSubmissionWrite: level(p.PermSubmissionWrite), domain.PermGradeRead: level(p.PermGradeRead),
		domain.PermGradeSubmit: level(p.PermGradeSubmit), domain.PermGradePost: level(p.PermGradePost),
		domain.PermMemberRead: level(p.PermMemberRead), domain.PermMemberManage: level(p.PermMemberManage),
		domain.PermActionDecide: level(p.PermActionDecide), domain.PermAgentDelegate: level(p.PermAgentDelegate),
		domain.PermConversationAsk: level(p.PermConversationAsk), domain.PermConversationAnswer: level(p.PermConversationAnswer),
		domain.PermMemberInvite: level(p.PermMemberInvite),
	}
}

func memberPerms(m dbq.GetMemberInCourseRow) permSet {
	return permSet{
		domain.PermDocumentRead: level(m.PermDocumentRead), domain.PermDocumentReadDraft: level(m.PermDocumentReadDraft),
		domain.PermDocumentWrite: level(m.PermDocumentWrite), domain.PermRubricRead: level(m.PermRubricRead),
		domain.PermAssignmentWrite: level(m.PermAssignmentWrite), domain.PermSubmissionRead: level(m.PermSubmissionRead),
		domain.PermSubmissionWrite: level(m.PermSubmissionWrite), domain.PermGradeRead: level(m.PermGradeRead),
		domain.PermGradeSubmit: level(m.PermGradeSubmit), domain.PermGradePost: level(m.PermGradePost),
		domain.PermMemberRead: level(m.PermMemberRead), domain.PermMemberManage: level(m.PermMemberManage),
		domain.PermActionDecide: level(m.PermActionDecide), domain.PermAgentDelegate: level(m.PermAgentDelegate),
		domain.PermConversationAsk: level(m.PermConversationAsk), domain.PermConversationAnswer: level(m.PermConversationAnswer),
		domain.PermMemberInvite: level(m.PermMemberInvite),
	}
}

// exceeds names the first permission in want that is above what the granter
// may hand out. Nobody hands out more than they have: otherwise
// perm_member_manage alone would be every permission, one member.add away.
func (ps permSet) exceeds(granter *domain.Member) (domain.Perm, bool) {
	for _, p := range domain.AllPerms {
		if ps[p] > grantable(granter, p) {
			return p, true
		}
	}
	return "", false
}

// grantable is the most of p a granter may hand out: what it holds
// (domain.Member.Perm), with two exceptions. A delegate never brings agents
// of its own, and so holds no agent_delegate; but a student it seats may
// still ask to bring theirs, as the student preset says, with an
// instructor's approval. That is the principal's to give, and so the
// delegate's to give for it, no higher than the principal holds it: a
// delegate that manages members, or hands out join links, seats students as
// its principal would. And no person holds conversation_answer
// (domain.Ceiling): conversations are with agents. An agent's answers are
// judged by whoever decides actions, so conversation_answer is handed out
// as far as the granter decides actions, or answers itself, whichever is
// more: an instructor seats the course's tutor agent answering on its own
// as they did when they answered themselves, and a TA, who decides nothing,
// gives no agent answers.
func grantable(g *domain.Member, p domain.Perm) domain.Level {
	switch {
	case p == domain.PermAgentDelegate && g.PrincipalID != nil:
		if g.Principal == nil {
			return domain.Denied
		}
		return g.Principal.Perm(p)
	case p == domain.PermConversationAnswer:
		return max(g.Perm(p), g.Perm(domain.PermActionDecide))
	}
	return g.Perm(p)
}

func validScope(s string) bool { return s == domain.ScopeAll || s == domain.ScopeListed }

var validRoles = map[string]bool{"student": true, "instructor": true, "ta": true, "observer": true, "assistant": true}

// errRole refuses a roster role that is not one.
var errRole = apperr.Invalid("role must be student, instructor, ta, observer or assistant")

// checkPermChange holds the levels a change names to what a change is: at
// least one, each a permission there is, at a level there is.
func checkPermChange(over PermLevels) error {
	if len(over) == 0 {
		return apperr.Invalid("perms is empty: nothing to change")
	}
	return (permSet{}).apply(over)
}
