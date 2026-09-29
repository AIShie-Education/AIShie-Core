package tools_test

import (
	"context"
	"encoding/json"
	"hash/fnv"
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
	rootSession := b.session(t, b.Root)
	b.try(t, b.admin, "actor.revoke_credential", m{"actor_id": b.Root, "credential_id": rootSession}, apperr.Forbidden)
	// Nor are they listed to an admin: what is listed is there to be revoked.
	b.try(t, b.admin, "actor.list_credentials", m{"actor_id": b.Root}, apperr.Forbidden)
	if got := testkit.Result[tools.CredentialListOut](t, b.do(t, b.Root, "actor.list_credentials", m{"actor_id": b.admin})); len(got.Credentials) != 0 {
		t.Fatalf("root listing the admin's credentials: %+v", got)
	}
	// An administrator's own are theirs, through this door too.
	own := b.session(t, b.admin)
	b.do(t, b.admin, "actor.list_credentials", m{"actor_id": b.admin})
	b.do(t, b.admin, "actor.revoke_credential", m{"actor_id": b.admin, "credential_id": own})
	if out := b.MustCall(b.sato, "actor.list_credentials", m{"actor_id": b.grader}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("an instructor listing an agent's tokens: %+v", out)
	}
	// A token an agent issues itself is its own.
	self := testkit.Result[tools.IssueTokenOut](t, b.do(t, b.grader, "credential.issue_token", m{"label": "rotated"}))
	mine := testkit.Result[tools.CredentialListOut](t, b.do(t, b.grader, "credential.list", m{}))
	for _, c := range mine.Credentials {
		if c.ID == self.CredentialID && (c.IssuedByID == nil || *c.IssuedByID != b.grader) {
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
	b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course}, apperr.InvalidArgument)
	b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "email": "mei@example.edu", "actor_id": mei}, apperr.InvalidArgument)
	// An id an administrator handed over says whom it names: an agent, here,
	// which has no email to be found by.
	if got := testkit.Result[tools.MemberLookupActorOut](t, b.do(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "actor_id": b.tutor})); got.ActorID != b.tutor || got.Kind != "agent" || got.MemberID == nil || *got.MemberID != b.tutorM {
		t.Fatalf("the tutor by id: %+v", got)
	}
	b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "actor_id": uuid.New()}, apperr.NotFound)
	b.try(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "actor_id": b.Actor("system", "sweeps")}, apperr.NotFound)
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
		all[b.kenM].State != "not_started" || all[b.kenM].SubmissionID != nil ||
		all[b.kenM].DisplayName == nil || *all[b.kenM].DisplayName != "Ken" || *all[b.kenM].MemberStatus != "active" {
		t.Fatalf("the instructor's roster: %+v", all)
	}
	// Names and seat status are the member list's: the grader, who may not
	// read it, gets the students by member id, as submission.list gives them.
	for _, e := range roster(b.grader) {
		if e.DisplayName != nil || e.MemberStatus != nil {
			t.Fatalf("the grader was told a name or a seat's status: %+v", e)
		}
	}
	// A paused student is a row still, and says so.
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": b.kenM})
	if got := roster(b.sato)[b.kenM]; got.MemberStatus == nil || *got.MemberStatus != "paused" {
		t.Fatalf("a paused student: %+v", got)
	}
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": b.kenM})
	// Assignment scope is the target's: the grader is listed for HW3 alone.
	hw5 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW5", "points_possible": 10})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw5})
	if out := b.MustCall(b.grader, "submission.roster", m{"course_id": b.course, "assignment_id": hw5}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("the grader reading another assignment's roster: %+v", out)
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
	// Student scope is the target's: a TA listed for Yuki cannot record Ken.
	ta := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "TA"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": ta, "preset": "ta", "student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	if out := b.MustCall(ta, "submission.record_missing", args, uuid.NewString()); out.Status != domain.StatusDenied {
		t.Fatalf("a TA recording a student outside their scope: %+v", out)
	}

	// The grader grades only with approval, and so records missing work:
	// a proposal, which Sato approves.
	proposed := b.MustCall(b.grader, "submission.record_missing", args, uuid.NewString())
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the grader recording Ken missing: %+v", proposed)
	}
	decided := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}))
	var recorded tools.SubmissionIDOut
	if decided.Outcome != domain.StatusExecuted || json.Unmarshal(decided.Result, &recorded) != nil {
		t.Fatalf("approving the grader's proposal: %+v", decided)
	}
	missing := recorded.SubmissionID
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

	// An unpublished assignment has nobody missing from it; and to whoever
	// may not see it yet, it is not there at all.
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW4", "points_possible": 10})).ID
	b.try(t, b.sato, "submission.record_missing", m{"course_id": b.course, "assignment_id": hw4, "student_member_id": b.kenM}, apperr.FailedPrecondition)
	b.try(t, ta, "submission.record_missing", m{"course_id": b.course, "assignment_id": hw4, "student_member_id": b.yukiM}, apperr.NotFound)
	if _, err := b.Call(b.ken, "submission.roster", m{"course_id": b.course, "assignment_id": hw4}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a student reading an unpublished assignment's roster: %v", err)
	}
	if _, err := b.Call(b.tutor, "submission.roster", m{"course_id": b.course, "assignment_id": hw4}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("the tutor reading an unpublished assignment's roster: %v", err)
	}
	b.do(t, b.sato, "submission.roster", m{"course_id": b.course, "assignment_id": hw4})
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

// A change to an assignment's instructions made while the assignment is being
// unpublished neither deadlocks with it nor tells students about it. The
// unpublish holds the assignment FOR UPDATE and takes the course's event
// stream last; the document change must therefore wait for the assignment
// before it takes the stream, and then see it unpublished.
func TestInstructionsChangedDuringAnUnpublish(t *testing.T) {
	b := build(t)
	doc := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "Oops", "body_md": "v1"})).DocumentID
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc})
	id := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create",
		m{"course_id": b.course, "title": "Oops", "points_possible": 10, "instructions_document_id": doc})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": id})

	// What assignment.unpublish does, up to its event.
	ctx := context.Background()
	tx, err := b.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `WITH l AS (SELECT id FROM assignment WHERE id = $1 FOR UPDATE)
		UPDATE assignment SET published_at = NULL WHERE id IN (SELECT id FROM l)`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan pipeline.Outcome, 1)
	b.start(t, done, b.sato, "document.add_version", m{"course_id": b.course, "document_id": doc, "body_md": "v2", "publish": true})
	b.blocked(t, 1, done)
	// The change waits for the assignment, holding no stream lock: had it
	// taken one, the unpublish would now wait for it, and it for the unpublish.
	if n := b.Count(`SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.objsubid = 2 AND l.classid = $1 AND l.granted AND a.datname = current_database()`,
		0x41495345); n != 0 {
		t.Fatalf("the document change holds %d event-stream locks while it waits for the assignment", n)
	}
	h := fnv.New32a()
	_, _ = h.Write(b.course[:])
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, int32(0x41495345), int32(h.Sum32())); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if out := <-done; out.Status != domain.StatusExecuted {
		t.Fatalf("the document change: %+v", out)
	}
	// Told under its unreleased name: students are not told about it.
	if n := b.Count(`SELECT count(*) FROM event WHERE subject_id = $1 AND type = 'document.version_added' AND assignment_id = $2`, doc, id); n != 0 {
		t.Fatal("news of an unpublished assignment's instructions went out under its released name")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE subject_id = $1 AND type = 'document.version_added_unreleased'`, doc); n != 1 {
		t.Fatalf("%d unreleased version events", n)
	}
}
