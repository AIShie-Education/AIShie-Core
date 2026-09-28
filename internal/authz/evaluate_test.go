package authz

import (
	"context"
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
			ID: uuid.New(), Status: domain.MemberActive, SeatValid: true,
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

// The Admin gate's first steps: a platform role goes anywhere; an
// appointment goes on, to be held to what it covers; nothing else goes.
func TestAdmin(t *testing.T) {
	root := domain.Actor{Status: domain.ActorActive, PlatformRole: domain.PlatformRoot}
	admin := domain.Actor{Status: domain.ActorActive, PlatformRole: domain.PlatformAdmin, Administers: true}
	appointed := domain.Actor{Status: domain.ActorActive, Administers: true}
	nobody := domain.Actor{Status: domain.ActorActive}
	gone := domain.Actor{Status: domain.ActorSuspended, Administers: true}
	goneRoot := domain.Actor{Status: domain.ActorSuspended, PlatformRole: domain.PlatformRoot}

	cases := []struct {
		name     string
		actor    domain.Actor
		want     domain.Level
		reason   Reason
		platform bool
	}{
		{"root", root, domain.Autonomous, ReasonNone, true},
		{"an admin, appointed too, goes as an admin", admin, domain.Autonomous, ReasonNone, true},
		{"a department administrator goes on", appointed, domain.Autonomous, ReasonNone, false},
		{"anyone else is told a platform role is needed", nobody, domain.Denied, ReasonPlatformRole, false},
		{"a suspended administrator", gone, domain.Denied, ReasonActorNotActive, false},
		{"a suspended root", goneRoot, domain.Denied, ReasonActorNotActive, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scope, d := Admin(c.actor, true)
			if d.Level != c.want || d.Reason != c.reason || scope.Platform != c.platform {
				t.Fatalf("got %s (%q), platform %v; want %s (%q), platform %v", d.Level, d.Reason, scope.Platform, c.want, c.reason, c.platform)
			}
			if !d.Level.Allowed() && scope != (AdminScope{}) {
				t.Fatalf("a denied caller is given a scope: %+v", scope)
			}
		})
	}
}

// The zero scope, every call's but an Admin-gated one's, covers nothing.
func TestTheZeroAdminScopeCoversNothing(t *testing.T) {
	auth, ok, err := AdminScope{}.Covers(context.Background(), nil, uuid.New())
	if ok || auth != nil || err != nil {
		t.Fatalf("covers: %v %+v %v", ok, auth, err)
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

// A delegate's seat, as a decision table: steps 1–3 with its principal.
func TestEvaluateDelegate(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	active := domain.Actor{ID: uuid.New(), Status: domain.ActorActive}

	principal := func(mod func(*domain.Member)) *domain.Member {
		p := &domain.Member{
			ID: uuid.New(), Status: domain.MemberActive, SeatValid: true,
			StudentScope: domain.ScopeListed, AssignmentScope: domain.ScopeAll,
			Perms: map[domain.Perm]domain.Level{
				domain.PermDocumentRead:       domain.Autonomous,
				domain.PermSubmissionRead:     domain.PendingReview,
				domain.PermGradeRead:          domain.Autonomous,
				domain.PermMemberManage:       domain.Autonomous,
				domain.PermAgentDelegate:      domain.Autonomous,
				domain.PermConversationAsk:    domain.ConfirmRequired,
				domain.PermConversationAnswer: domain.Denied,
			},
		}
		if mod != nil {
			mod(p)
		}
		return p
	}
	delegate := func(p *domain.Member, mod func(*domain.Member)) *domain.Member {
		d := &domain.Member{
			ID: uuid.New(), Status: domain.MemberActive, SeatValid: true,
			StudentScope: domain.ScopeListed, AssignmentScope: domain.ScopeAll,
			Perms: map[domain.Perm]domain.Level{
				domain.PermDocumentRead:       domain.Autonomous,
				domain.PermSubmissionRead:     domain.Autonomous,
				domain.PermGradeRead:          domain.ConfirmRequired,
				domain.PermGradeSubmit:        domain.Autonomous,
				domain.PermMemberManage:       domain.Autonomous,
				domain.PermAgentDelegate:      domain.Autonomous,
				domain.PermConversationAnswer: domain.Autonomous,
			},
			Principal: p,
		}
		if p != nil {
			d.PrincipalID = &p.ID
		}
		if mod != nil {
			mod(d)
		}
		return d
	}
	one := func(p domain.Perm) []domain.Perm { return []domain.Perm{p} }
	cases := []struct {
		name   string
		member *domain.Member
		perms  []domain.Perm
		want   domain.Level
		reason Reason
	}{
		{"the lower of its own and its principal's", delegate(principal(nil), nil), one(domain.PermSubmissionRead), domain.PendingReview, ReasonNone},
		{"its own when that is lower", delegate(principal(nil), nil), one(domain.PermGradeRead), domain.ConfirmRequired, ReasonNone},
		{"nothing its principal does not hold", delegate(principal(nil), nil), one(domain.PermGradeSubmit), domain.Denied, ReasonPermDenied},
		{"member_manage as far as both hold it", delegate(principal(nil), nil), one(domain.PermMemberManage), domain.Autonomous, ReasonNone},
		{"and no further than its principal", delegate(principal(func(p *domain.Member) { p.Perms[domain.PermMemberManage] = domain.ConfirmRequired }), nil),
			one(domain.PermMemberManage), domain.ConfirmRequired, ReasonNone},
		{"nor than its own row", delegate(principal(nil), func(d *domain.Member) { d.Perms[domain.PermMemberManage] = domain.Denied }),
			one(domain.PermMemberManage), domain.Denied, ReasonPermDenied},
		{"a principal who manages no members: beyond the delegate preset, only by proposal",
			delegate(principal(func(p *domain.Member) {
				p.Perms[domain.PermMemberManage], p.Perms[domain.PermGradeSubmit] = domain.Denied, domain.Autonomous
			}), nil), one(domain.PermGradeSubmit), domain.ConfirmRequired, ReasonNone},
		{"and what the delegate preset gives, as the principal holds it",
			delegate(principal(func(p *domain.Member) { p.Perms[domain.PermMemberManage] = domain.Denied }), nil),
			one(domain.PermDocumentRead), domain.Autonomous, ReasonNone},
		{"never agent_delegate, whatever both hold", delegate(principal(nil), nil), one(domain.PermAgentDelegate), domain.Denied, ReasonPermDenied},
		{"answering is capped by the principal's asking", delegate(principal(nil), nil), one(domain.PermConversationAnswer), domain.ConfirmRequired, ReasonNone},
		{"so a principal who may not ask gets no answers", delegate(principal(func(p *domain.Member) { p.Perms[domain.PermConversationAsk] = domain.Denied }), nil),
			one(domain.PermConversationAnswer), domain.Denied, ReasonPermDenied},
		{"paused principal", delegate(principal(func(p *domain.Member) { p.Status = domain.MemberPaused }), nil), one(domain.PermDocumentRead), domain.Denied, ReasonPrincipalNotActive},
		{"removed principal", delegate(principal(func(p *domain.Member) { p.Status = domain.MemberRemoved }), nil), one(domain.PermDocumentRead), domain.Denied, ReasonPrincipalNotActive},
		{"expired principal, before any sweep", delegate(principal(func(p *domain.Member) { p.ExpiresAt = &past }), nil), one(domain.PermDocumentRead), domain.Denied, ReasonPrincipalNotActive},
		{"a seat that does not match its actor's owner", delegate(principal(nil), func(d *domain.Member) { d.SeatValid = false }), one(domain.PermDocumentRead), domain.Denied, ReasonPrincipalNotActive},
		{"a principal that was not loaded", delegate(nil, func(d *domain.Member) { id := uuid.New(); d.PrincipalID = &id }), one(domain.PermDocumentRead), domain.Denied, ReasonPrincipalNotActive},
		{"its own seat paused comes first", delegate(principal(nil), func(d *domain.Member) { d.Status = domain.MemberPaused }), one(domain.PermDocumentRead), domain.Denied, ReasonMemberNotLive},
		{"an owned agent's seat with no principal", &domain.Member{ID: uuid.New(), Status: domain.MemberActive,
			Perms: map[domain.Perm]domain.Level{domain.PermDocumentRead: domain.Autonomous}}, one(domain.PermDocumentRead), domain.Denied, ReasonPrincipalNotActive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Evaluate(active, domain.CourseActive, c.member, c.perms, true, now)
			if got.Level != c.want || got.Reason != c.reason {
				t.Fatalf("got %s (%q), want %s (%q)", got.Level, got.Reason, c.want, c.reason)
			}
		})
	}
}

// A delegate's list queries take its principal's reach as well as its own;
// any other seat's principal pair reaches everything and adds nothing.
func TestScopeFilterForADelegate(t *testing.T) {
	p := &domain.Member{ID: uuid.New(), StudentScope: domain.ScopeListed, AssignmentScope: domain.ScopeAll}
	d := &domain.Member{ID: uuid.New(), StudentScope: domain.ScopeAll, AssignmentScope: domain.ScopeListed, PrincipalID: &p.ID, Principal: p}
	if f := FilterFor(d); f.MemberID != d.ID || !f.StudentAll || f.AssignmentAll || f.PrincipalID != p.ID || f.PrincipalStudentAll || !f.PrincipalAssignmentAll {
		t.Fatalf("a delegate's filter: %+v", f)
	}
	if f := FilterFor(p); !f.PrincipalStudentAll || !f.PrincipalAssignmentAll {
		t.Fatalf("an ordinary seat's filter narrows by a principal it has not got: %+v", f)
	}
	lost := &domain.Member{ID: uuid.New(), StudentScope: domain.ScopeAll, AssignmentScope: domain.ScopeAll, PrincipalID: &p.ID}
	if f := FilterFor(lost); f.PrincipalStudentAll || f.PrincipalAssignmentAll {
		t.Fatalf("a delegate whose principal was not loaded reaches everything: %+v", f)
	}
}
