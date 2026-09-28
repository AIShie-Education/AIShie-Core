// Package authz is authorize() from docs/schema.md §3:
//
//	authorize(actor, course, action_type, target) → autonomy_level
//
//	1. actor.status = 'active' and course.status ≠ 'archived' (for writes), else denied
//	2. member = the actor's course_member in this course with status = 'active'
//	   and (expires_at null or in the future); none → denied
//	   a delegate's seat: its principal the same, held by its actor's
//	   active owner; else denied
//	3. level = member.perm_<action_type>; 'denied' → denied
//	   a delegate's: the lower of its own and its principal's
//	4. if the target belongs to a student: member.student_scope = 'all', or
//	   that student ∈ member_student_scope; else denied
//	   a delegate: its principal's scope too
//	5. if the target belongs to an assignment: same with assignment_scope
//	6. return level
//
// One indexed lookup and at most two existence checks, for a delegate twice
// that. Nothing is cached, so removing a member takes effect on its next
// call.
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
	ReasonNone           Reason = ""
	ReasonActorNotActive Reason = "actor_not_active"
	ReasonCourseArchived Reason = "course_archived"
	ReasonNotAMember     Reason = "not_a_member"
	ReasonMemberNotLive  Reason = "membership_not_active" // paused, removed or expired
	// A delegate whose principal is paused, removed or expired, or held by
	// an owner who is suspended or is no longer the delegate's; or an owned
	// agent's seat with no principal at all.
	ReasonPrincipalNotActive Reason = "principal_not_active"
	ReasonPermDenied         Reason = "permission_denied"
	ReasonStudentScope       Reason = "student_out_of_scope"
	ReasonAssignmentScope    Reason = "assignment_out_of_scope"
	ReasonPlatformRole       Reason = "platform_role_required"
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
	if !member.PrincipalLive(now) {
		return deny(ReasonPrincipalNotActive, member)
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
	actor, courseStatus, member, err := seatOf(ctx, q, actorID, courseID, write)
	if err != nil {
		return Decision{}, err
	}
	return Evaluate(actor, courseStatus, member, perms, write, now), nil
}

// SeatFor is steps 1 and 2 without step 3: the actor's seat in a course,
// held for a write as ForActor holds it (the seat KEY SHARE first, a
// delegate's principal second), and the reason it does not count if it does
// not. It is for a call made on the actor's own account that acts through a
// seat without being gated by one of its permissions (an agent's memory),
// and so asks of the seat only that it is there and counts: live, its
// principal live, its course open for a write. A course that does not exist
// is a not_found error.
func SeatFor(ctx context.Context, q dbq.Querier, actorID, courseID uuid.UUID, write bool, now time.Time) (*domain.Member, Reason, error) {
	actor, courseStatus, member, err := seatOf(ctx, q, actorID, courseID, write)
	if err != nil {
		return nil, "", err
	}
	switch {
	case !actor.Active():
		return member, ReasonActorNotActive, nil
	case write && courseStatus == domain.CourseArchived:
		return member, ReasonCourseArchived, nil
	case member == nil:
		return nil, ReasonNotAMember, nil
	case !member.Live(now):
		return member, ReasonMemberNotLive, nil
	case !member.PrincipalLive(now):
		return member, ReasonPrincipalNotActive, nil
	}
	return member, ReasonNone, nil
}

// seatOf loads what steps 1 and 2 look at: the actor, its course's status,
// and its seat there if it has one that is not removed, locked for a write.
func seatOf(ctx context.Context, q dbq.Querier, actorID, courseID uuid.UUID, write bool) (domain.Actor, string, *domain.Member, error) {
	actor, err := LoadActor(ctx, q, actorID)
	if err != nil {
		return domain.Actor{}, "", nil, err
	}
	course, err := q.GetCourseForAuthz(ctx, courseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Actor{}, "", nil, apperr.Missing("course %s does not exist", courseID)
	}
	if err != nil {
		return domain.Actor{}, "", nil, fmt.Errorf("load course: %w", err)
	}
	var member *domain.Member
	var row dbq.GetLiveMemberForAuthzRow
	if write {
		// A write holds its caller's seat to the end, and takes it first;
		// a delegate's principal second, read again once it is held.
		var locked dbq.LockLiveMemberForAuthzRow
		locked, err = q.LockLiveMemberForAuthz(ctx, dbq.LockLiveMemberForAuthzParams{CourseID: courseID, ActorID: actorID})
		row = dbq.GetLiveMemberForAuthzRow(locked)
		if err == nil && row.PrincipalMemberID != nil {
			var p dbq.LockPrincipalForAuthzRow
			if p, err = q.LockPrincipalForAuthz(ctx, *row.PrincipalMemberID); err == nil {
				withPrincipal(&row, p)
			}
		}
	} else {
		row, err = q.GetLiveMemberForAuthz(ctx, dbq.GetLiveMemberForAuthzParams{CourseID: courseID, ActorID: actorID})
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return domain.Actor{}, "", nil, fmt.Errorf("load member: %w", err)
	default:
		member = memberFromRow(dbq.GetMemberForAuthzRow(row))
	}
	return actor, course.Status, member, nil
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

// LoadMember reads one seat by id as authorization sees it, principal and
// all, locking nothing. It is for what shows or measures a seat — what a
// member may do, what a delegate may be given — not for deciding a call.
func LoadMember(ctx context.Context, q dbq.Querier, memberID uuid.UUID) (*domain.Member, error) {
	row, err := q.GetMemberForAuthz(ctx, memberID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.Missing("no such member")
	}
	if err != nil {
		return nil, fmt.Errorf("load member: %w", err)
	}
	return memberFromRow(row), nil
}

// LoadMembers is LoadMember for several seats at once, in one statement:
// what a list shows of what each of its seats may do. A seat that does not
// exist is left out of the map.
func LoadMembers(ctx context.Context, q dbq.Querier, ids []uuid.UUID) (map[uuid.UUID]*domain.Member, error) {
	out := make(map[uuid.UUID]*domain.Member, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.GetMembersForAuthz(ctx, distinct(ids))
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = memberFromRow(dbq.GetMemberForAuthzRow(r))
	}
	return out, nil
}

// CheckScope is steps 4 and 5. It returns ReasonNone when the whole target is
// within the member's scope. 'listed' with nothing listed matches nothing:
// scope fails closed. A delegate reaches only what it and its principal both
// reach: the principal's scope may have narrowed since the delegate was
// seated, and nothing narrows the delegate's with it.
func CheckScope(ctx context.Context, q dbq.Querier, m *domain.Member, t Target) (Reason, error) {
	reason, err := checkOwnScope(ctx, q, m, t)
	if err != nil || reason != ReasonNone || m.PrincipalID == nil {
		return reason, err
	}
	if m.Principal == nil {
		return ReasonPrincipalNotActive, nil
	}
	return checkOwnScope(ctx, q, m.Principal, t)
}

func checkOwnScope(ctx context.Context, q dbq.Querier, m *domain.Member, t Target) (Reason, error) {
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
// and the same for assignments; and the same again with the principal's, for
// a delegate, which reaches only what its principal reaches too. For any
// other seat the principal's pair is "all", and adds nothing. The lists never
// lean on anything a write keeps in step: the previous release narrows a
// principal without touching its delegates.
type ScopeFilter struct {
	MemberID      uuid.UUID
	StudentAll    bool
	AssignmentAll bool

	PrincipalID            uuid.UUID
	PrincipalStudentAll    bool
	PrincipalAssignmentAll bool
}

func FilterFor(m *domain.Member) ScopeFilter {
	f := ScopeFilter{
		MemberID:               m.ID,
		StudentAll:             m.StudentScope == domain.ScopeAll,
		AssignmentAll:          m.AssignmentScope == domain.ScopeAll,
		PrincipalStudentAll:    true,
		PrincipalAssignmentAll: true,
	}
	if m.PrincipalID != nil {
		// A delegate whose principal was not loaded reaches nobody: the
		// principal's lists are then those of no seat at all.
		f.PrincipalID, f.PrincipalStudentAll, f.PrincipalAssignmentAll = *m.PrincipalID, false, false
		if p := m.Principal; p != nil {
			f.PrincipalStudentAll = p.StudentScope == domain.ScopeAll
			f.PrincipalAssignmentAll = p.AssignmentScope == domain.ScopeAll
		}
	}
	return f
}

func memberFromRow(r dbq.GetMemberForAuthzRow) *domain.Member {
	m := &domain.Member{
		ID:              r.ID,
		CourseID:        r.CourseID,
		ActorID:         r.ActorID,
		Status:          r.Status,
		ExpiresAt:       r.ExpiresAt,
		StudentScope:    r.StudentScope,
		AssignmentScope: r.AssignmentScope,
		Perms: levels(r.PermDocumentRead, r.PermDocumentReadDraft, r.PermDocumentWrite, r.PermRubricRead,
			r.PermAssignmentWrite, r.PermSubmissionRead, r.PermSubmissionWrite, r.PermGradeRead,
			r.PermGradeSubmit, r.PermGradePost, r.PermMemberRead, r.PermMemberManage, r.PermActionDecide,
			r.PermAgentDelegate, r.PermConversationAsk, r.PermConversationAnswer),
		PrincipalID:   r.PrincipalMemberID,
		AnswersCourse: r.AnswersCourse,
		SeatValid:     r.OwnerMatches,
	}
	if r.PrincipalMemberID == nil {
		return m
	}
	if r.PrincipalActorID == nil || r.PrincipalStatus == nil || r.PrincipalStudentScope == nil || r.PrincipalAssignmentScope == nil {
		// The composite key makes this impossible; if it happens, the
		// delegate holds nothing.
		m.SeatValid = false
		return m
	}
	m.SeatValid = m.SeatValid && r.PrincipalActorStatus != nil && *r.PrincipalActorStatus == domain.ActorActive
	m.Principal = &domain.Member{
		ID:              *r.PrincipalMemberID,
		CourseID:        r.CourseID,
		ActorID:         *r.PrincipalActorID,
		Status:          *r.PrincipalStatus,
		ExpiresAt:       r.PrincipalExpiresAt,
		StudentScope:    *r.PrincipalStudentScope,
		AssignmentScope: *r.PrincipalAssignmentScope,
		Perms: levels(orDenied(r.PrincipalPermDocumentRead), orDenied(r.PrincipalPermDocumentReadDraft), orDenied(r.PrincipalPermDocumentWrite),
			orDenied(r.PrincipalPermRubricRead), orDenied(r.PrincipalPermAssignmentWrite), orDenied(r.PrincipalPermSubmissionRead),
			orDenied(r.PrincipalPermSubmissionWrite), orDenied(r.PrincipalPermGradeRead), orDenied(r.PrincipalPermGradeSubmit),
			orDenied(r.PrincipalPermGradePost), orDenied(r.PrincipalPermMemberRead), orDenied(r.PrincipalPermMemberManage),
			orDenied(r.PrincipalPermActionDecide), orDenied(r.PrincipalPermAgentDelegate), orDenied(r.PrincipalPermConversationAsk),
			orDenied(r.PrincipalPermConversationAnswer)),
		// A principal is nobody's delegate (course_member_principal_valid),
		// and its actor is a person, whom nobody owns.
		SeatValid: true,
	}
	return m
}

// withPrincipal puts the principal as it was read under its lock in place of
// what the seat's own query read of it before.
func withPrincipal(row *dbq.GetLiveMemberForAuthzRow, p dbq.LockPrincipalForAuthzRow) {
	row.PrincipalActorID, row.PrincipalStatus, row.PrincipalExpiresAt = &p.ActorID, &p.Status, p.ExpiresAt
	row.PrincipalStudentScope, row.PrincipalAssignmentScope = &p.StudentScope, &p.AssignmentScope
	row.PrincipalActorStatus = &p.ActorStatus
	row.PrincipalPermDocumentRead, row.PrincipalPermDocumentReadDraft = &p.PermDocumentRead, &p.PermDocumentReadDraft
	row.PrincipalPermDocumentWrite, row.PrincipalPermRubricRead = &p.PermDocumentWrite, &p.PermRubricRead
	row.PrincipalPermAssignmentWrite, row.PrincipalPermSubmissionRead = &p.PermAssignmentWrite, &p.PermSubmissionRead
	row.PrincipalPermSubmissionWrite, row.PrincipalPermGradeRead = &p.PermSubmissionWrite, &p.PermGradeRead
	row.PrincipalPermGradeSubmit, row.PrincipalPermGradePost = &p.PermGradeSubmit, &p.PermGradePost
	row.PrincipalPermMemberRead, row.PrincipalPermMemberManage = &p.PermMemberRead, &p.PermMemberManage
	row.PrincipalPermActionDecide, row.PrincipalPermAgentDelegate = &p.PermActionDecide, &p.PermAgentDelegate
	row.PrincipalPermConversationAsk, row.PrincipalPermConversationAnswer = &p.PermConversationAsk, &p.PermConversationAnswer
}

// levels maps column values, given in domain.AllPerms order, to the ladder.
// The enum and domain.Level share their names; an unknown value cannot come
// out of the column, and would be Denied if it did.
func levels(cols ...dbq.AutonomyLevel) map[domain.Perm]domain.Level {
	out := make(map[domain.Perm]domain.Level, len(domain.AllPerms))
	for i, p := range domain.AllPerms {
		if i < len(cols) {
			out[p], _ = domain.ParseLevel(string(cols[i]))
		}
	}
	return out
}

// orDenied reads a column of an outer join: absent is denied.
func orDenied(l *dbq.AutonomyLevel) dbq.AutonomyLevel {
	if l == nil {
		return dbq.AutonomyLevel(domain.Denied.String())
	}
	return *l
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
