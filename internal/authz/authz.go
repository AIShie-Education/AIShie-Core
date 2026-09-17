// Package authz is authorize() from docs/schema.md §3:
//
//	authorize(actor, course, action_type, target) → autonomy_level
//
//	1. actor.status = 'active' and course.status ≠ 'archived' (for writes), else denied
//	2. member = the actor's course_member in this course with status = 'active'
//	   and (expires_at null or in the future); none → denied
//	3. level = member.perm_<action_type>; 'denied' → denied
//	4. if the target belongs to a student: member.student_scope = 'all', or
//	   that student ∈ member_student_scope; else denied
//	5. if the target belongs to an assignment: same with assignment_scope
//	6. return level
//
// One indexed lookup and at most two existence checks. Nothing is cached, so
// removing a member takes effect on its next call.
//
// Two facts are never read here: whether the actor is a human or an agent,
// and the member's roster role. domain.Actor and domain.Member do not carry
// them, the queries in queries/authz.sql do not select them, and tests hold
// both of those in place.
package authz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
)

// Reason says which step denied. It goes in the action log and in the error
// shown to the caller; it never changes the outcome.
type Reason string

const (
	ReasonNone            Reason = ""
	ReasonActorNotActive  Reason = "actor_not_active"
	ReasonCourseArchived  Reason = "course_archived"
	ReasonNotAMember      Reason = "not_a_member"
	ReasonMemberNotLive   Reason = "membership_not_active" // paused, removed or expired
	ReasonPermDenied      Reason = "permission_denied"
	ReasonStudentScope    Reason = "student_out_of_scope"
	ReasonAssignmentScope Reason = "assignment_out_of_scope"
	ReasonPlatformRole    Reason = "platform_role_required"
)

// Decision is the result of a check. Member is set whenever a membership row
// was found, including when the answer is Denied, so that the attempt can be
// recorded against it.
type Decision struct {
	Level  domain.Level
	Reason Reason
	Member *domain.Member
}

func deny(r Reason, m *domain.Member) Decision {
	return Decision{Level: domain.Denied, Reason: r, Member: m}
}

// Target is what steps 4 and 5 need to know about the thing acted on: which
// students and which assignments it belongs to. Empty means the target
// belongs to none, and that scope does not apply. A batch lists them all, and
// every one must be in scope.
type Target struct {
	StudentMemberIDs []uuid.UUID
	AssignmentIDs    []uuid.UUID
	// SpansAssignments marks a target that belongs to a student but to no
	// single assignment: a grade on a component, a course total. A member
	// limited to listed assignments may not touch it. Without this, a grader
	// listed for HW3 alone could read the whole class's midterm, because a
	// target naming no assignment would skip step 5 entirely.
	SpansAssignments bool
}

// Evaluate is steps 1–3 with everything already loaded. It is pure, so the
// whole decision table can be tested without a database. member is nil when
// the actor has no seat in the course. A tool gated by several permissions
// gets the lowest of their levels; gated by none, it is denied.
func Evaluate(actor domain.Actor, courseStatus string, member *domain.Member, perms []domain.Perm, write bool, now time.Time) Decision {
	if !actor.Active() {
		return deny(ReasonActorNotActive, member)
	}
	if write && courseStatus == domain.CourseArchived {
		return deny(ReasonCourseArchived, member)
	}
	if member == nil {
		return deny(ReasonNotAMember, nil)
	}
	if !member.Live(now) {
		return deny(ReasonMemberNotLive, member)
	}
	levels := make([]domain.Level, len(perms))
	for i, p := range perms {
		levels[i] = member.Perm(p)
	}
	level := domain.MinLevel(levels...)
	if !level.Allowed() {
		return deny(ReasonPermDenied, member)
	}
	return Decision{Level: level, Member: member}
}

// ForActor runs steps 1–3 for an actor in a course. A course that does not
// exist is a not_found error, not a denial.
func ForActor(ctx context.Context, q dbq.Querier, actorID, courseID uuid.UUID, perms []domain.Perm, write bool, now time.Time) (Decision, error) {
	actor, err := LoadActor(ctx, q, actorID)
	if err != nil {
		return Decision{}, err
	}
	course, err := q.GetCourseForAuthz(ctx, courseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, apperr.Missing("course %s does not exist", courseID)
	}
	if err != nil {
		return Decision{}, fmt.Errorf("load course: %w", err)
	}
	var member *domain.Member
	row, err := q.GetLiveMemberForAuthz(ctx, dbq.GetLiveMemberForAuthzParams{CourseID: courseID, ActorID: actorID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Decision{}, fmt.Errorf("load member: %w", err)
	default:
		member = memberFromRow(dbq.GetMemberForAuthzRow(row))
	}
	return Evaluate(actor, course.Status, member, perms, write, now), nil
}

// ForMember runs steps 1–3 against one specific membership row. Approving a
// proposal re-authorizes the membership it was made under: if that member has
// since been removed and re-added, the new row is a fresh start and the old
// proposal does not ride along.
func ForMember(ctx context.Context, q dbq.Querier, memberID uuid.UUID, perms []domain.Perm, write bool, now time.Time) (Decision, error) {
	row, err := q.GetMemberForAuthz(ctx, memberID)
	if errors.Is(err, pgx.ErrNoRows) {
		return deny(ReasonNotAMember, nil), nil
	}
	if err != nil {
		return Decision{}, fmt.Errorf("load member: %w", err)
	}
	member := memberFromRow(row)
	actor, err := LoadActor(ctx, q, member.ActorID)
	if err != nil {
		return Decision{}, err
	}
	course, err := q.GetCourseForAuthz(ctx, member.CourseID)
	if err != nil {
		return Decision{}, fmt.Errorf("load course: %w", err)
	}
	return Evaluate(actor, course.Status, member, perms, write, now), nil
}

// CheckScope is steps 4 and 5. It returns ReasonNone when the whole target is
// within the member's scope. 'listed' with nothing listed matches nothing:
// scope fails closed.
func CheckScope(ctx context.Context, q dbq.Querier, m *domain.Member, t Target) (Reason, error) {
	if students := distinct(t.StudentMemberIDs); len(students) > 0 && m.StudentScope != domain.ScopeAll {
		n, err := q.CountStudentsInScope(ctx, dbq.CountStudentsInScopeParams{MemberID: m.ID, StudentMemberIds: students})
		if err != nil {
			return "", fmt.Errorf("student scope: %w", err)
		}
		if int(n) != len(students) {
			return ReasonStudentScope, nil
		}
	}
	if t.SpansAssignments && m.AssignmentScope != domain.ScopeAll {
		return ReasonAssignmentScope, nil
	}
	if assignments := distinct(t.AssignmentIDs); len(assignments) > 0 && m.AssignmentScope != domain.ScopeAll {
		n, err := q.CountAssignmentsInScope(ctx, dbq.CountAssignmentsInScopeParams{MemberID: m.ID, AssignmentIds: assignments})
		if err != nil {
			return "", fmt.Errorf("assignment scope: %w", err)
		}
		if int(n) != len(assignments) {
			return ReasonAssignmentScope, nil
		}
	}
	return ReasonNone, nil
}

// Authorize is the whole of authorize(): steps 1–3, then 4–5 when allowed.
func Authorize(ctx context.Context, q dbq.Querier, actorID, courseID uuid.UUID, perms []domain.Perm, write bool, t Target, now time.Time) (Decision, error) {
	d, err := ForActor(ctx, q, actorID, courseID, perms, write, now)
	if err != nil || !d.Level.Allowed() {
		return d, err
	}
	reason, err := CheckScope(ctx, q, d.Member, t)
	if err != nil {
		return Decision{}, err
	}
	if reason != ReasonNone {
		return deny(reason, d.Member), nil
	}
	return d, nil
}

// Platform gates the few operations outside any course — creating courses,
// registering actors, seating a first instructor — on actor.platform_role.
// This is the only place that column is read. There is no ladder here: a
// platform operation is allowed outright or not at all.
func Platform(actor domain.Actor, roles ...string) Decision {
	if !actor.Active() {
		return deny(ReasonActorNotActive, nil)
	}
	for _, r := range roles {
		if actor.PlatformRole != "" && actor.PlatformRole == r {
			return Decision{Level: domain.Autonomous}
		}
	}
	return deny(ReasonPlatformRole, nil)
}

// LoadActor fetches what authorization may know about an actor.
func LoadActor(ctx context.Context, q dbq.Querier, id uuid.UUID) (domain.Actor, error) {
	row, err := q.GetActorForAuthz(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Actor{}, apperr.New(apperr.Unauthenticated, "actor %s does not exist", id)
	}
	if err != nil {
		return domain.Actor{}, fmt.Errorf("load actor: %w", err)
	}
	a := domain.Actor{ID: row.ID, DisplayName: row.DisplayName, Status: row.Status}
	if row.PlatformRole != nil {
		a.PlatformRole = *row.PlatformRole
	}
	return a, nil
}

// ScopeFilter is a member's scope in the shape list queries take it, so that
// filtering happens in SQL rather than after the fact. A query applies
//
//	(@student_all OR EXISTS (SELECT 1 FROM member_student_scope
//	                         WHERE member_id = @member_id AND student_member_id = x.student_member_id))
//
// and the same for assignments.
type ScopeFilter struct {
	MemberID      uuid.UUID
	StudentAll    bool
	AssignmentAll bool
}

func FilterFor(m *domain.Member) ScopeFilter {
	return ScopeFilter{
		MemberID:      m.ID,
		StudentAll:    m.StudentScope == domain.ScopeAll,
		AssignmentAll: m.AssignmentScope == domain.ScopeAll,
	}
}

func memberFromRow(r dbq.GetMemberForAuthzRow) *domain.Member {
	lv := func(l dbq.AutonomyLevel) domain.Level {
		// The enum and domain.Level share their names; an unknown value
		// cannot come out of the column, and would be Denied if it did.
		v, _ := domain.ParseLevel(string(l))
		return v
	}
	return &domain.Member{
		ID:              r.ID,
		CourseID:        r.CourseID,
		ActorID:         r.ActorID,
		Status:          r.Status,
		ExpiresAt:       r.ExpiresAt,
		StudentScope:    r.StudentScope,
		AssignmentScope: r.AssignmentScope,
		Perms: map[domain.Perm]domain.Level{
			domain.PermDocumentRead:      lv(r.PermDocumentRead),
			domain.PermDocumentReadDraft: lv(r.PermDocumentReadDraft),
			domain.PermDocumentWrite:     lv(r.PermDocumentWrite),
			domain.PermRubricRead:        lv(r.PermRubricRead),
			domain.PermAssignmentWrite:   lv(r.PermAssignmentWrite),
			domain.PermSubmissionRead:    lv(r.PermSubmissionRead),
			domain.PermSubmissionWrite:   lv(r.PermSubmissionWrite),
			domain.PermGradeRead:         lv(r.PermGradeRead),
			domain.PermGradeSubmit:       lv(r.PermGradeSubmit),
			domain.PermGradePost:         lv(r.PermGradePost),
			domain.PermMemberRead:        lv(r.PermMemberRead),
			domain.PermMemberManage:      lv(r.PermMemberManage),
			domain.PermActionDecide:      lv(r.PermActionDecide),
		},
	}
}

func distinct(ids []uuid.UUID) []uuid.UUID {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}
