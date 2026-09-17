package tools

import (
	"sort"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
)

// permSet is a full set of the thirteen permissions, as a preset or a member
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
		domain.PermActionDecide: level(p.PermActionDecide),
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
		domain.PermActionDecide: level(m.PermActionDecide),
	}
}

// exceeds names the first permission in want that is above what the granter
// holds. Nobody hands out more than they have: otherwise perm_member_manage
// alone would be every permission, one member.add away.
func (ps permSet) exceeds(granter *domain.Member) (domain.Perm, bool) {
	for _, p := range domain.AllPerms {
		if ps[p] > granter.Perm(p) {
			return p, true
		}
	}
	return "", false
}

func validScope(s string) bool { return s == domain.ScopeAll || s == domain.ScopeListed }

var validRoles = map[string]bool{"student": true, "instructor": true, "ta": true, "observer": true, "assistant": true}
