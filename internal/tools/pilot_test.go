package tools_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// What a first pilot asked for: a leaked token revoked alone, a person found
// to seat without asking an administrator for an id, the students who have
// not started, and an assignment published by mistake taken back.

func TestAdminRevokesOneOfAnAgentsTokens(t *testing.T) {
	b := build(t)
	issue := func(label string) tools.IssueTokenOut {
		return testkit.Result[tools.IssueTokenOut](t, b.do(t, b.admin, "actor.issue_token", m{"actor_id": b.grader, "label": label}))
	}
	leaked, kept := issue("laptop"), issue("server")

	list := func() []tools.CredentialView {
		return testkit.Result[tools.CredentialListOut](t, b.do(t, b.admin, "actor.list_credentials", m{"actor_id": b.grader})).Credentials
	}
	creds := list()
	if len(creds) != 2 {
		t.Fatalf("credentials: %+v", creds)
	}
	for _, c := range creds {
		if c.Kind != "api_token" || c.TokenPrefix == nil || c.IssuedByID == nil || *c.IssuedByID != b.admin ||
			c.IssuedBy == nil || *c.IssuedBy != "Admin" || c.RevokedAt != nil {
			t.Fatalf("a token as listed: %+v", c)
		}
	}
	// Newest first.
	if creds[0].ID != kept.CredentialID || *creds[0].TokenPrefix != kept.TokenPrefix {
		t.Fatalf("order: %+v", creds)
	}

	b.do(t, b.admin, "actor.revoke_credential", m{"actor_id": b.grader, "credential_id": leaked.CredentialID})
	for _, c := range list() {
		if (c.ID == leaked.CredentialID) != (c.RevokedAt != nil) {
			t.Fatalf("after revoking the leaked one: %+v", c)
		}
	}
	// Once only, and only under the actor it belongs to.
	b.try(t, b.admin, "actor.revoke_credential", m{"actor_id": b.grader, "credential_id": leaked.CredentialID}, apperr.NotFound)
	b.try(t, b.admin, "actor.revoke_credential", m{"actor_id": b.tutor, "credential_id": kept.CredentialID}, apperr.NotFound)
	// The agent itself is untouched: it still works with the other token.
	if n := b.Count(`SELECT count(*) FROM actor WHERE id = $1 AND status = 'active'`, b.grader); n != 1 {
		t.Fatal("revoking a token suspended the agent")
	}

	// Root's are root's; an instructor has no platform role at all.
	rootTok := testkit.Result[tools.IssueTokenOut](t, b.do(t, b.Root, "credential.issue_token", m{"label": "cli"}))
	b.try(t, b.admin, "actor.revoke_credential", m{"actor_id": b.Root, "credential_id": rootTok.CredentialID}, apperr.Forbidden)
	if out := b.MustCall(b.sato, "actor.list_credentials", m{"actor_id": b.grader}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("an instructor listing an agent's tokens: %+v", out)
	}
	// A token one issues oneself is one's own.
	mine := testkit.Result[tools.CredentialListOut](t, b.do(t, b.Root, "credential.list", m{}))
	for _, c := range mine.Credentials {
		if c.ID == rootTok.CredentialID && (c.IssuedByID == nil || *c.IssuedByID != b.Root) {
			t.Fatalf("self-issued: %+v", c)
		}
	}
}

func TestInstructorFindsWhomToSeatByEmail(t *testing.T) {
	b := build(t)
	mei := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register",
		m{"kind": "human", "display_name": "Mei", "email": "mei@example.edu"})).ActorID
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "email": "yuki@example.edu"})

	look := func(email string) tools.MemberLookupActorOut {
		return testkit.Result[tools.MemberLookupActorOut](t, b.do(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "email": email}))
	}
	if got := look(" MEI@example.edu "); got.ActorID != mei || got.DisplayName != "Mei" || got.Kind != "human" || got.MemberID != nil {
		t.Fatalf("Mei: %+v", got)
	}
	// Someone already seated comes with their seat.
	if got := look("yuki@example.edu"); got.ActorID != b.yuki || got.MemberID == nil || *got.MemberID != b.yukiM {
		t.Fatalf("Yuki: %+v", got)
	}
	// The whole address or nothing: it lists nobody.
	b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "email": "mei@"}, apperr.NotFound)
	b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "email": "example.edu"}, apperr.NotFound)
	b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "email": "  "}, apperr.InvalidArgument)
	// For whoever seats members, and nobody else.
	if out := b.MustCall(b.yuki, "member.lookup_actor", m{"course_id": b.course, "email": "mei@example.edu"}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a student looking people up: %+v", out)
	}
}

func TestRosterShowsWhoHasNotStartedAndMissingIsRecordedByHand(t *testing.T) {
	b := build(t)
	// HW3 has no due date: the sweep would never mark anyone missing.
	yukiWork := b.submit(t, b.yuki, "Yuki's essay")

	roster := func(actor uuid.UUID) map[uuid.UUID]tools.RosterEntry {
		out := testkit.Result[tools.SubmissionRosterOut](t, b.do(t, actor, "submission.roster", m{"course_id": b.course, "assignment_id": b.hw3}))
		byStudent := map[uuid.UUID]tools.RosterEntry{}
		for _, e := range out.Students {
			byStudent[e.StudentMemberID] = e
		}
		return byStudent
	}
	all := roster(b.sato)
	if len(all) != 2 || all[b.yukiM].State != "submitted" || all[b.yukiM].SubmissionID == nil || *all[b.yukiM].SubmissionID != yukiWork ||
		all[b.kenM].State != "not_started" || all[b.kenM].SubmissionID != nil || all[b.kenM].DisplayName != "Ken" {
		t.Fatalf("the instructor's roster: %+v", all)
	}
	// Scope applies: the tutor is listed for Yuki alone, and a student sees
	// themselves.
	if got := roster(b.tutor); len(got) != 1 || got[b.yukiM].State != "submitted" {
		t.Fatalf("the tutor's roster: %+v", got)
	}
	if got := roster(b.ken); len(got) != 1 || got[b.kenM].State != "not_started" {
		t.Fatalf("Ken's roster: %+v", got)
	}

	args := m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.kenM}
	// Not for the student to declare, nor for anyone about someone who has
	// handed something in.
	if out := b.MustCall(b.ken, "submission.record_missing", args, uuid.NewString()); out.Status != domain.StatusDenied {
		t.Fatalf("a student recording themselves missing: %+v", out)
	}
	b.try(t, b.sato, "submission.record_missing", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.yukiM}, apperr.Conflict)
	b.try(t, b.sato, "submission.record_missing", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.graderM}, apperr.FailedPrecondition)

	missing := testkit.Result[tools.SubmissionIDOut](t, b.do(t, b.sato, "submission.record_missing", args)).SubmissionID
	if got := roster(b.sato)[b.kenM]; got.State != "missing" || got.SubmissionID == nil || *got.SubmissionID != missing {
		t.Fatalf("Ken after being recorded missing: %+v", got)
	}
	b.try(t, b.sato, "submission.record_missing", args, apperr.Conflict)
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'submission.missing' AND subject_id = $1`, missing); n != 1 {
		t.Fatalf("%d submission.missing events", n)
	}
	// Late work takes the row over, as it does after a due date.
	late := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "sorry"}))
	if late.SubmissionID != missing || roster(b.sato)[b.kenM].State != "draft" {
		t.Fatalf("late work after missing: %+v", late)
	}

	// An unpublished assignment has nobody missing from it.
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW4", "points_possible": 10})).ID
	b.try(t, b.sato, "submission.record_missing", m{"course_id": b.course, "assignment_id": hw4, "student_member_id": b.kenM}, apperr.FailedPrecondition)
}

func TestUnpublishOnlyBeforeAnyoneStarts(t *testing.T) {
	b := build(t)
	hw := m{"course_id": b.course}
	hw["assignment_id"] = testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "Oops", "points_possible": 10})).ID

	b.try(t, b.sato, "assignment.unpublish", hw, apperr.Conflict) // not published yet
	b.do(t, b.sato, "assignment.publish", hw)
	if out := b.MustCall(b.yuki, "assignment.unpublish", hw, uuid.NewString()); out.Status != domain.StatusDenied {
		t.Fatalf("a student unpublishing: %+v", out)
	}
	b.do(t, b.sato, "assignment.unpublish", hw)
	// Gone again for students, as though it had never been published.
	b.try(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw["assignment_id"]}, apperr.NotFound)
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'assignment.unpublished' AND subject_id = $1`, hw["assignment_id"]); n != 1 {
		t.Fatalf("%d assignment.unpublished events", n)
	}

	// Published again, and a student starts: from then on it stays.
	b.do(t, b.sato, "assignment.publish", hw)
	b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw["assignment_id"]})
	b.try(t, b.sato, "assignment.unpublish", hw, apperr.FailedPrecondition)
}

// A student who opens a draft while the assignment is being unpublished
// waits for it, and then finds nothing to submit to.
func TestUnpublishHoldsOffASubmissionUnderWay(t *testing.T) {
	b := build(t)
	id := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "Oops", "points_possible": 10})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": id})

	release := b.hold(t, `WITH l AS (SELECT id FROM assignment WHERE id = $1 FOR UPDATE)
		UPDATE assignment SET published_at = NULL WHERE id IN (SELECT id FROM l)`, id)
	done := make(chan pipeline.Outcome, 1)
	b.start(t, done, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": id})
	b.blocked(t, 1, done)
	release()
	if out := <-done; out.Status == domain.StatusExecuted {
		t.Fatalf("a submission was made to an assignment unpublished under it: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM submission WHERE assignment_id = $1`, id); n != 0 {
		t.Fatalf("%d submissions to an unpublished assignment", n)
	}
}
