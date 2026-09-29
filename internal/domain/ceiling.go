package domain

// A seat's ceiling for a permission is the most it may hold of it at all,
// whatever its row says and whoever grants it: the same for every seat of
// its kind, before "nobody hands out more than they hold" asks what the
// granter holds. Ceiling is the one rule. Authorization applies it to a
// delegate on every call (Member.Perm, through DelegateCap); seating a
// member and every change that widens a seat hold the row to it, refusing a
// level named above it and cutting a preset's down to it; and the member
// views show it, so that a front end offers only what may be given.
//
// A person answers no conversation: conversation_answer is denied to a
// person's seat. The database writes a person's row with it denied whatever
// it is told (migration 0018), as it writes an agent's action_decide at
// confirm_required at most (0014).

// CeilingReason says why a ceiling is below autonomous. The codes are what
// a refusal to go above it says as its reason, and what the member views
// say beside the ceiling.
type CeilingReason string

const (
	CeilingNone CeilingReason = ""
	// A delegate never brings agents of its own: agent_delegate is denied
	// to it.
	CeilingAgentNever CeilingReason = "agent_never"
	// An agent, owned or not, decides and reviews only by proposal: its
	// action_decide is confirm_required at most, so that each decision of
	// its, and each review, waits for a person to confirm it.
	CeilingAgentDecidesByProposal CeilingReason = "agent_decides_by_proposal"
	// The agent of someone who does not manage the course's members — a
	// student's — does only by proposal what the built-in delegate preset
	// does not give (DelegatePresetLevels).
	CeilingStudentAgentByProposal CeilingReason = "student_agent_by_proposal"
	// A delegate holds no more than its principal: the principal's own
	// level, or for conversation_answer its conversation_ask.
	CeilingPrincipalLevel CeilingReason = "principal_level"
	// Conversations are between a person and an agent: a person asks, an
	// agent answers, and people talk to people elsewhere. A person's seat
	// holds conversation_answer at denied. It is also the reason a person
	// is refused as a conversation's respondent, and refused answering.
	CeilingConversationsAreWithAgents CeilingReason = "conversations_are_with_agents"
)

// Ceiling is the most a seat may hold of p, and why, when that is below
// autonomous. agent says the seat's actor is an agent, and false that it is
// a person: whoever seats or changes a member reads it from actor.kind, to
// limit and never to grant, as the refusals of ownership read it;
// authorization never does, and needs not, since the database holds an
// agent's rows and a person's to what they may hold, and a delegate's
// principal, which authorization loads, says it is one. principal is a
// delegate's principal's seat, as it stands; a delegate is always an agent.
//
// Where two rules bound a permission at the same level, the one that holds
// whatever the principal holds is the reason given: the agent's own rules
// first, then the student-agent rule, then the principal's level.
func Ceiling(agent bool, principal *Member, p Perm) (Level, CeilingReason) {
	level, why := Autonomous, CeilingNone
	lower := func(l Level, r CeilingReason) {
		if l < level {
			level, why = l, r
		}
	}
	if principal != nil {
		agent = true
		if p == PermAgentDelegate {
			lower(Denied, CeilingAgentNever)
		}
	}
	if agent && p == PermActionDecide {
		lower(ConfirmRequired, CeilingAgentDecidesByProposal)
	}
	if !agent && p == PermConversationAnswer {
		lower(Denied, CeilingConversationsAreWithAgents)
	}
	if principal == nil {
		return level, why
	}
	if p != PermMemberManage && p != PermMemberInvite && !principal.Perm(PermMemberManage).Allowed() {
		lower(max(DelegatePresetLevels[p], ConfirmRequired), CeilingStudentAgentByProposal)
	}
	if p == PermConversationAnswer {
		lower(principal.Perm(PermConversationAsk), CeilingPrincipalLevel)
	} else {
		lower(principal.Perm(p), CeilingPrincipalLevel)
	}
	return level, why
}
