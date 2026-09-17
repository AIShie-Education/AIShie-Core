package authz

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
)

// Steps 1–3 as a decision table. No database: Evaluate is pure.
func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	active := domain.Actor{ID: uuid.New(), Status: domain.ActorActive}
	suspended := domain.Actor{ID: uuid.New(), Status: domain.ActorSuspended}

	member := func(mod func(*domain.Member)) *domain.Member {
		m := &domain.Member{
			ID: uuid.New(), Status: domain.MemberActive,
			StudentScope: domain.ScopeAll, AssignmentScope: domain.ScopeAll,
			Perms: map[domain.Perm]domain.Level{
				domain.PermDocumentRead: domain.Autonomous,
				domain.PermGradeSubmit:  domain.ConfirmRequired,
				domain.PermGradePost:    domain.PendingReview,
				domain.PermMemberManage: domain.Denied,
			},
		}
		if mod != nil {
			mod(m)
		}
		return m
	}
	submit := []domain.Perm{domain.PermGradeSubmit}

	cases := []struct {
		name   string
		actor  domain.Actor
		course string
		member *domain.Member
		perms  []domain.Perm
		write  bool
		want   domain.Level
		reason Reason
	}{
		// step 1
		{"suspended actor", suspended, domain.CourseActive, member(nil), submit, true, domain.Denied, ReasonActorNotActive},
		{"suspended actor cannot read either", suspended, domain.CourseActive, member(nil), []domain.Perm{domain.PermDocumentRead}, false, domain.Denied, ReasonActorNotActive},
		{"archived course refuses writes", active, domain.CourseArchived, member(nil), submit, true, domain.Denied, ReasonCourseArchived},
		{"archived course still reads", active, domain.CourseArchived, member(nil), []domain.Perm{domain.PermDocumentRead}, false, domain.Autonomous, ReasonNone},
		{"draft course accepts writes", active, domain.CourseDraft, member(nil), submit, true, domain.ConfirmRequired, ReasonNone},
		// step 2
		{"no membership", active, domain.CourseActive, nil, submit, true, domain.Denied, ReasonNotAMember},
		{"paused", active, domain.CourseActive, member(func(m *domain.Member) { m.Status = domain.MemberPaused }), submit, true, domain.Denied, ReasonMemberNotLive},
		{"removed", active, domain.CourseActive, member(func(m *domain.Member) { m.Status = domain.MemberRemoved }), submit, true, domain.Denied, ReasonMemberNotLive},
		{"expired", active, domain.CourseActive, member(func(m *domain.Member) { m.ExpiresAt = &past }), submit, true, domain.Denied, ReasonMemberNotLive},
		{"expires exactly now", active, domain.CourseActive, member(func(m *domain.Member) { m.ExpiresAt = &now }), submit, true, domain.Denied, ReasonMemberNotLive},
		{"expires later", active, domain.CourseActive, member(func(m *domain.Member) { m.ExpiresAt = &future }), submit, true, domain.ConfirmRequired, ReasonNone},
		// step 3
		{"denied permission", active, domain.CourseActive, member(nil), []domain.Perm{domain.PermMemberManage}, true, domain.Denied, ReasonPermDenied},
		{"permission the row does not carry", active, domain.CourseActive, member(nil), []domain.Perm{domain.PermRubricRead}, false, domain.Denied, ReasonPermDenied},
		{"level is returned as is", active, domain.CourseActive, member(nil), []domain.Perm{domain.PermGradePost}, true, domain.PendingReview, ReasonNone},
		{"two permissions: the lower wins", active, domain.CourseActive, member(nil), []domain.Perm{domain.PermGradeSubmit, domain.PermGradePost}, true, domain.ConfirmRequired, ReasonNone},
		{"two permissions: one denied denies", active, domain.CourseActive, member(nil), []domain.Perm{domain.PermGradePost, domain.PermMemberManage}, true, domain.Denied, ReasonPermDenied},
		{"gated by nothing is denied", active, domain.CourseActive, member(nil), nil, true, domain.Denied, ReasonPermDenied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Evaluate(c.actor, c.course, c.member, c.perms, c.write, now)
			if got.Level != c.want || got.Reason != c.reason {
				t.Fatalf("got %s (%q), want %s (%q)", got.Level, got.Reason, c.want, c.reason)
			}
			if got.Member != c.member {
				t.Fatal("Decision.Member should be the member that was evaluated, denied or not")
			}
		})
	}
}

func TestPlatform(t *testing.T) {
	root := domain.Actor{Status: domain.ActorActive, PlatformRole: domain.PlatformRoot}
	admin := domain.Actor{Status: domain.ActorActive, PlatformRole: domain.PlatformAdmin}
	nobody := domain.Actor{Status: domain.ActorActive}
	goneRoot := domain.Actor{Status: domain.ActorSuspended, PlatformRole: domain.PlatformRoot}

	cases := []struct {
		name  string
		actor domain.Actor
		roles []string
		want  domain.Level
	}{
		{"root where root or admin may", root, []string{domain.PlatformRoot, domain.PlatformAdmin}, domain.Autonomous},
		{"admin where root or admin may", admin, []string{domain.PlatformRoot, domain.PlatformAdmin}, domain.Autonomous},
		{"admin where only root may", admin, []string{domain.PlatformRoot}, domain.Denied},
		{"no platform role", nobody, []string{domain.PlatformRoot, domain.PlatformAdmin}, domain.Denied},
		{"suspended root", goneRoot, []string{domain.PlatformRoot}, domain.Denied},
		{"no roles listed allows nobody", root, nil, domain.Denied},
		{"an empty role never matches an empty requirement", nobody, []string{""}, domain.Denied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Platform(c.actor, c.roles...); got.Level != c.want {
				t.Fatalf("got %s, want %s", got.Level, c.want)
			}
		})
	}
}

func TestLevelOrder(t *testing.T) {
	ladder := []domain.Level{domain.Denied, domain.ConfirmRequired, domain.PendingReview, domain.Autonomous}
	for i := 1; i < len(ladder); i++ {
		if ladder[i-1] >= ladder[i] {
			t.Fatalf("the ladder is out of order: %s is not below %s", ladder[i-1], ladder[i])
		}
	}
	for _, l := range ladder {
		back, err := domain.ParseLevel(l.String())
		if err != nil || back != l {
			t.Fatalf("%s does not round-trip: %v %v", l, back, err)
		}
	}
	if _, err := domain.ParseLevel("maybe"); err == nil {
		t.Fatal("ParseLevel accepted an unknown level")
	}
}
