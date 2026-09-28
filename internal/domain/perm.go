package domain

// Perm names one action type's permission. The list is the catalogue: each
// one is a perm_<name> column on course_member and permission_preset, and a
// test fails if this list and those columns ever disagree. Adding one is a
// code change plus a migration on both tables.
type Perm string

const (
	PermDocumentRead      Perm = "document_read"       // published material and instructions
	PermDocumentReadDraft Perm = "document_read_draft" // unpublished versions
	PermDocumentWrite     Perm = "document_write"      // new versions, publishing, archiving
	PermRubricRead        Perm = "rubric_read"
	PermAssignmentWrite   Perm = "assignment_write"
	PermSubmissionRead    Perm = "submission_read"  // scoped: student, assignment
	PermSubmissionWrite   Perm = "submission_write" // scoped
	PermGradeRead         Perm = "grade_read"       // scoped
	PermGradeSubmit       Perm = "grade_submit"     // scoped; writes a draft grade
	PermGradePost         Perm = "grade_post"       // scoped
	PermMemberRead        Perm = "member_read"
	PermMemberManage      Perm = "member_manage"
	PermActionDecide      Perm = "action_decide" // approving proposals, reviewing after the fact
	// Bringing an agent one owns into the course as one's delegate.
	PermAgentDelegate Perm = "agent_delegate"
	// Opening conversations, and writing in those one opened.
	PermConversationAsk Perm = "conversation_ask"
	// Being addressed, and answering; the level is the autonomy of the answers.
	PermConversationAnswer Perm = "conversation_answer"
	// Making the course's join links, which seat whoever holds one as a
	// student, and listing and revoking them.
	PermMemberInvite Perm = "member_invite"
)

// AllPerms is in the column order used by the schema and the seed.
var AllPerms = []Perm{
	PermDocumentRead, PermDocumentReadDraft, PermDocumentWrite, PermRubricRead,
	PermAssignmentWrite, PermSubmissionRead, PermSubmissionWrite, PermGradeRead,
	PermGradeSubmit, PermGradePost, PermMemberRead, PermMemberManage, PermActionDecide,
	PermAgentDelegate, PermConversationAsk, PermConversationAnswer, PermMemberInvite,
}

// Column is the database column holding this permission's level.
func (p Perm) Column() string { return "perm_" + string(p) }

func (p Perm) Valid() bool {
	for _, q := range AllPerms {
		if p == q {
			return true
		}
	}
	return false
}
