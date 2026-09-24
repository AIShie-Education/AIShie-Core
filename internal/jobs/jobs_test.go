package jobs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/jobs"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

type m = map[string]any

type fixture struct {
	*testkit.CS101
	system uuid.UUID
	runner *jobs.Runner
	now    time.Time
}

func setup(t *testing.T, students int) *fixture {
	t.Helper()
	c := testkit.NewCS101(t, students)
	f := &fixture{CS101: c, system: c.Actor("system", "system"), now: time.Now()}
	f.runner = jobs.New(c.Pool, c.P, f.system, jobs.Config{}, nil)
	c.P.SetClock(func() time.Time { return f.now })
	return f
}

func (f *fixture) sweep(t *testing.T) jobs.Report {
	t.Helper()
	rep, err := f.runner.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ran {
		t.Fatal("the sweep stood by, with nobody else sweeping")
	}
	return rep
}

func (f *fixture) propose(t *testing.T, s testkit.Student, key string) uuid.UUID {
	t.Helper()
	out := f.MustCall(f.Grader, "grade.submit", m{"course_id": f.Course, "submission_id": s.HW3, "score": 85}, key)
	if out.Status != domain.StatusProposed {
		t.Fatalf("%+v", out)
	}
	return *out.ActionID
}

func TestStaleProposalsAreCancelled(t *testing.T) {
	f := setup(t, 2)
	old := f.propose(t, f.Students[0], "old")

	if rep := f.sweep(t); rep.ProposalsExpired != 0 {
		t.Fatalf("a fresh proposal was expired: %+v", rep)
	}
	f.now = f.now.Add(pipeline.DefaultProposalTTL - time.Hour)
	recent := f.propose(t, f.Students[1], "recent")
	f.now = f.now.Add(2 * time.Hour) // the first is now past the TTL; the second is not

	if rep := f.sweep(t); rep.ProposalsExpired != 1 {
		t.Fatalf("%+v, want exactly the old proposal expired", rep)
	}
	if n := f.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled' AND decided_by_member_id IS NULL
		AND result->'error'->'details'->>'reason' = 'proposal_expired'`, old); n != 1 {
		t.Fatal("the stale proposal is not cancelled with its reason")
	}
	if n := f.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'proposed'`, recent); n != 1 {
		t.Fatal("the recent proposal was touched")
	}
	// The sweep is itself on record: the system actor, no membership, no
	// authorization to speak of, and the proposal as its target.
	if n := f.Count(`SELECT count(*) FROM action WHERE actor_id = $1 AND action_type = 'action.expire' AND target_id = $2
		AND member_id IS NULL AND authz_result = 'autonomous' AND status = 'executed' AND course_id = $3`, f.system, old, f.Course); n != 1 {
		t.Fatal("the sweep left no action row of its own")
	}
	// The agent finds out from its own feed, as it would from a rejection.
	if n := f.Count(`SELECT count(*) FROM event WHERE type = 'action.cancelled' AND action_id = $1 AND payload->>'reason' = 'proposal_expired'`, old); n != 1 {
		t.Fatal("no action.cancelled event filed under the proposal")
	}
	feed := testkit.Result[tools.EventListOut](t, f.MustCall(f.Grader, "event.list", m{"course_id": f.Course}, ""))
	seen := false
	for _, e := range feed.Events {
		seen = seen || (e.Type == "action.cancelled" && *e.ActionID == old)
	}
	if !seen {
		t.Fatal("the proposer cannot see that its proposal expired")
	}
	if rep := f.sweep(t); rep.ProposalsExpired != 0 {
		t.Fatalf("a second sweep expired it again: %+v", rep)
	}
	// Approving it now is too late, and says so.
	decided := f.MustCall(f.Sato, "action.decide", m{"course_id": f.Course, "action_id": old, "decision": "approve"}, "late")
	if decided.Status != domain.StatusFailed || decided.Error.Code != apperr.Conflict {
		t.Fatalf("approving an expired proposal: %+v", decided)
	}
}

func TestExpiredMembersAreRemoved(t *testing.T) {
	f := setup(t, 1)
	proposal := f.propose(t, f.Students[0], "p")
	f.Exec(`UPDATE course_member SET expires_at = $2 WHERE id = $1`, f.GraderM, f.now.Add(time.Hour))
	f.Exec(`UPDATE course_member SET expires_at = $2 WHERE id = $1`, f.SatoM, f.now.Add(1000*time.Hour))

	if rep := f.sweep(t); rep.MembersExpired != 0 {
		t.Fatalf("a membership with time left was removed: %+v", rep)
	}
	f.now = f.now.Add(2 * time.Hour)
	// authorize() does not wait for the sweep.
	if out := f.MustCall(f.Grader, "grade.submit", m{"course_id": f.Course, "submission_id": f.Students[0].HW3, "score": 1}, "after"); out.Status != domain.StatusDenied {
		t.Fatalf("an expired member, before any sweep: %+v", out)
	}
	if rep := f.sweep(t); rep.MembersExpired != 1 {
		t.Fatalf("%+v, want the grader removed and nobody else", rep)
	}
	if n := f.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND status = 'removed'`, f.GraderM); n != 1 {
		t.Fatal("the expired member is not removed")
	}
	if n := f.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND status = 'active'`, f.SatoM); n != 1 {
		t.Fatal("a member with time left was touched")
	}
	if n := f.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled'
		AND result->'error'->'details'->>'reason' = 'member_removed'`, proposal); n != 1 {
		t.Fatal("the expired member's pending proposal was not cancelled")
	}
	if n := f.Count(`SELECT count(*) FROM event WHERE type = 'member.removed' AND subject_id = $1 AND payload->>'reason' = 'expired'`, f.GraderM); n != 1 {
		t.Fatal("no member.removed event saying why")
	}
	if rep := f.sweep(t); rep.MembersExpired != 0 {
		t.Fatalf("removed twice: %+v", rep)
	}
}

func TestPastDueAssignments(t *testing.T) {
	f := setup(t, 3)
	yuki := f.Students[0]
	due := f.now.Add(time.Hour)
	f.Exec(`UPDATE assignment SET due_at = $2 WHERE id = $1`, f.HW4, due)
	// Yuki has started; the other two have not.
	f.MustCall(yuki.Actor, "submission.create", m{"course_id": f.Course, "assignment_id": f.HW4, "body": "in progress"}, "draft")

	if rep := f.sweep(t); rep.AssignmentsClosed != 0 {
		t.Fatalf("swept before the due date: %+v", rep)
	}
	f.now = due.Add(time.Minute)
	rep := f.sweep(t)
	if rep.AssignmentsClosed != 1 || rep.SubmissionsMissing != 2 {
		t.Fatalf("%+v, want HW4 closed and two students missing", rep)
	}
	if n := f.Count(`SELECT count(*) FROM submission WHERE assignment_id = $1 AND state = 'missing'`, f.HW4); n != 2 {
		t.Fatalf("%d missing rows", n)
	}
	if n := f.Count(`SELECT count(*) FROM submission WHERE assignment_id = $1 AND student_member_id = $2 AND state = 'draft'`, f.HW4, yuki.Member); n != 1 {
		t.Fatal("a student with a draft in progress was marked missing")
	}
	if n := f.Count(`SELECT count(*) FROM event WHERE type = 'assignment.due_passed' AND assignment_id = $1`, f.HW4); n != 1 {
		t.Fatalf("%d due_passed events, want 1", n)
	}
	if n := f.Count(`SELECT count(*) FROM event WHERE type = 'submission.missing' AND assignment_id = $1 AND student_member_id IS NOT NULL`, f.HW4); n != 2 {
		t.Fatalf("%d submission.missing events, want one per student", n)
	}
	// HW3 has no due date and is never swept; nor is anything swept twice.
	if rep := f.sweep(t); rep.AssignmentsClosed != 0 || rep.SubmissionsMissing != 0 {
		t.Fatalf("swept again: %+v", rep)
	}
	// A 'missing' row can be graded — a zero, with a reason.
	var missing uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM submission WHERE assignment_id = $1 AND state = 'missing' LIMIT 1`, f.HW4).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if out := f.MustCall(f.Sato, "grade.submit", m{"course_id": f.Course, "submission_id": missing, "score": 0, "feedback": "Not handed in."}, "zero"); out.Status != domain.StatusExecuted {
		t.Fatalf("grading a missing submission: %+v", out)
	}

	// An extension: the due date moves, and when it passes the assignment is
	// swept again — without marking anyone twice.
	extended := f.now.Add(24 * time.Hour)
	f.Exec(`UPDATE assignment SET due_at = $2 WHERE id = $1`, f.HW4, extended)
	if rep := f.sweep(t); rep.AssignmentsClosed != 0 {
		t.Fatalf("swept during the extension: %+v", rep)
	}
	f.now = extended.Add(time.Minute)
	if rep := f.sweep(t); rep.AssignmentsClosed != 1 || rep.SubmissionsMissing != 0 {
		t.Fatalf("after the extension: %+v, want the assignment closed again and nobody newly missing", rep)
	}
	// An archived or draft course is left alone.
	f.Exec(`UPDATE assignment SET due_at = $2 WHERE id = $1`, f.HW3, f.now.Add(-time.Hour))
	f.Exec(`UPDATE course SET status = 'archived' WHERE id = $1`, f.Course)
	if rep := f.sweep(t); rep.AssignmentsClosed != 0 {
		t.Fatalf("an archived course was swept: %+v", rep)
	}
}

// Every instance may run the sweeps; only one does at a time, and even if two
// did, each thing is swept once.
func TestOnlyOneInstanceSweeps(t *testing.T) {
	f := setup(t, 1)
	proposal := f.propose(t, f.Students[0], "p")
	f.now = f.now.Add(pipeline.DefaultProposalTTL + time.Hour)

	// Someone else is sweeping: this instance stands by and does nothing.
	other, err := f.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := other.QueryRow(context.Background(), `SELECT pg_try_advisory_lock($1)`, int64(0x4149534A4F4253)).Scan(&got); err != nil || !got {
		t.Fatalf("could not take the jobs lock: %v %v", got, err)
	}
	rep, err := f.runner.Sweep(context.Background())
	if err != nil || rep.Ran || rep.ProposalsExpired != 0 {
		t.Fatalf("swept while another instance held the lock: %+v %v", rep, err)
	}
	if _, err := other.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(0x4149534A4F4253)); err != nil {
		t.Fatal(err)
	}
	other.Release()

	// Several instances at once: the proposal is expired exactly once.
	var wg sync.WaitGroup
	reports := make([]jobs.Report, 6)
	for i := range reports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := jobs.New(f.Pool, f.P, f.system, jobs.Config{}, nil)
			reports[i], _ = r.Sweep(context.Background())
		}()
	}
	wg.Wait()
	expired := 0
	for _, r := range reports {
		expired += r.ProposalsExpired
	}
	if expired != 1 {
		t.Fatalf("expired %d times across six instances, want 1", expired)
	}
	if n := f.Count(`SELECT count(*) FROM event WHERE type = 'action.cancelled' AND action_id = $1`, proposal); n != 1 {
		t.Fatalf("%d cancellation events", n)
	}
	// The lock is given back: the next sweep runs.
	if rep := f.sweep(t); !rep.Ran {
		t.Fatal("the lock was not released")
	}
}

// A sweep that loses a race with a person does nothing, and does it quietly.
func TestASweepLosesRacesQuietly(t *testing.T) {
	f := setup(t, 1)
	proposal := f.propose(t, f.Students[0], "p")
	f.MustCall(f.Sato, "action.decide", m{"course_id": f.Course, "action_id": proposal, "decision": "approve"}, "ok")

	// The sweep had already picked the proposal when Sato approved it.
	out, err := f.P.InvokeSystem(context.Background(), f.system, tools.ToolActionExpire,
		tools.ActionExpireIn{ActionID: proposal, CreatedBefore: f.now.Add(time.Hour)}, "job:action.expire:"+proposal.String())
	if err != nil || out.Status != domain.StatusExecuted || testkit.Result[tools.SweepOut](t, out).Done {
		t.Fatalf("%+v %v, want a no-op", out, err)
	}
	if n := f.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'executed'`, proposal); n != 1 {
		t.Fatal("the sweep overwrote a decision")
	}
	if n := f.Count(`SELECT count(*) FROM grade`); n != 1 {
		t.Fatal("the approved grade is gone")
	}
	// A membership extended while the sweep was looking is left alone.
	f.Exec(`UPDATE course_member SET expires_at = $2 WHERE id = $1`, f.GraderM, f.now.Add(time.Hour))
	out, err = f.P.InvokeSystem(context.Background(), f.system, tools.ToolMemberExpire, tools.MemberExpireIn{CourseID: f.Course, MemberID: f.GraderM}, "job:member.expire:x")
	if err != nil || testkit.Result[tools.MemberExpireOut](t, out).Done {
		t.Fatalf("%+v %v, want a no-op", out, err)
	}
}

// Internal tools are for the system actor. Nobody else can reach them, by
// any door, whatever their standing.
func TestInternalToolsCannotBeCalled(t *testing.T) {
	f := setup(t, 1)
	proposal := f.propose(t, f.Students[0], "p")
	args := m{"action_id": proposal, "created_before": f.now.Add(time.Hour)}
	for name, actor := range map[string]uuid.UUID{"root": f.Root, "the instructor": f.Sato, "the system actor itself, from outside": f.system} {
		if _, err := f.Call(actor, tools.ToolActionExpire, args, "sneaky-"+name); !apperr.Is(err, apperr.NotFound) {
			t.Errorf("%s calling action.expire: %v, want not_found", name, err)
		}
	}
	for _, tl := range f.P.Registry().Exposed() {
		if tl.Internal {
			t.Errorf("%s is internal and exposed", tl.Name)
		}
	}
	// And InvokeSystem runs nothing else.
	if _, err := f.P.InvokeSystem(context.Background(), f.system, "grade.post", m{"course_id": f.Course, "assignment_id": f.HW3}, "k"); err == nil {
		t.Fatal("InvokeSystem ran an ordinary tool with no authorization")
	}
}

func TestStaleSessionsAreDeleted(t *testing.T) {
	f := setup(t, 0)
	add := func(prefix string, expires time.Time) {
		f.Exec(`INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at) VALUES ($1, 'session', 'h', $2, $3)`, f.Sato, prefix, expires)
	}
	add("long-dead", f.now.Add(-30*24*time.Hour))
	add("just-dead", f.now.Add(-time.Hour))
	add("alive", f.now.Add(time.Hour))
	f.Exec(`INSERT INTO credential (actor_id, kind, secret_hash, token_prefix, expires_at) VALUES ($1, 'api_token', 'h', 'old-token', $2)`, f.Sato, f.now.Add(-30*24*time.Hour))

	if rep := f.sweep(t); rep.SessionsDeleted != 1 {
		t.Fatalf("%+v, want only the long-dead session deleted", rep)
	}
	if n := f.Count(`SELECT count(*) FROM credential WHERE token_prefix IN ('just-dead', 'alive', 'old-token')`); n != 3 {
		t.Fatal("something other than a long-dead session was deleted")
	}
}

// A file is uploaded first and attached afterwards, so some never are. They
// are removed — but never one that a version points at, and never one that a
// proposal still waiting for its decision may yet attach.
func TestOrphanFilesAreRemoved(t *testing.T) {
	f := setup(t, 3)
	f.runner = jobs.New(f.Pool, f.P, f.system, jobs.Config{Blob: f.Blob}, nil)
	ctx := context.Background()
	upload := func(actor uuid.UUID, kind, body string) (token, key string) {
		t.Helper()
		out := f.MustCall(actor, "document.upload_url", m{"course_id": f.Course, "kind": kind, "content_type": "text/plain"}, "")
		u := testkit.Result[tools.UploadURLOut](t, out)
		key, ct, err := f.Blob.Redeem(strings.TrimPrefix(u.UploadURL, "http://lms.test"+blob.BlobPath), "PUT")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Blob.Put(ctx, key, ct, strings.NewReader(body), 1<<20); err != nil {
			t.Fatal(err)
		}
		return u.UploadToken, key
	}
	exists := func(key string) bool {
		_, err := f.Blob.Stat(ctx, key)
		return err == nil
	}
	yuki, ken, mei := f.Students[0], f.Students[1], f.Students[2]

	// Attached: a file on Yuki's HW4 draft.
	draft := testkit.Result[tools.SubmissionCreateOut](t, f.MustCall(yuki.Actor, "submission.create", m{"course_id": f.Course, "assignment_id": f.HW4}, "d")).SubmissionID
	essayToken, essay := upload(yuki.Actor, "submission", "essay")
	if out := f.MustCall(yuki.Actor, "document.create", m{"course_id": f.Course, "kind": "submission", "title": "essay.txt",
		"submission_id": draft, "upload_token": essayToken}, "attach"); out.Status != domain.StatusExecuted {
		t.Fatalf("%+v", out)
	}
	// Travelling inside proposals: one that will be approved late, one that
	// will be rejected.
	approvedToken, approved := upload(f.Grader, "feedback", "well argued")
	rejectedToken, rejected := upload(f.Grader, "feedback", "see me")
	propose := func(s testkit.Student, token, key string) uuid.UUID {
		t.Helper()
		out := f.MustCall(f.Grader, "grade.submit", m{"course_id": f.Course, "submission_id": s.HW3, "score": 70,
			"feedback_files": []m{{"title": "notes.txt", "upload_token": token}}}, key)
		if out.Status != domain.StatusProposed {
			t.Fatalf("%+v", out)
		}
		return *out.ActionID
	}
	toApprove, toReject := propose(ken, approvedToken, "a"), propose(mei, rejectedToken, "r")
	// Never attached to anything.
	_, abandoned := upload(ken.Actor, "submission", "closed the tab")

	if rep := f.sweep(t); rep.OrphanFilesRemoved != 0 {
		t.Fatalf("fresh uploads were removed: %+v", rep)
	}
	decide := func(action uuid.UUID, decision string) {
		t.Helper()
		if out := f.MustCall(f.Sato, "action.decide", m{"course_id": f.Course, "action_id": action, "decision": decision}, decision); out.Status != domain.StatusExecuted {
			t.Fatalf("%s: %+v", decision, out)
		}
	}
	decide(toReject, "reject")
	// Sato approves on the last day. The file has been waiting all that time.
	f.now = f.now.Add(pipeline.DefaultProposalTTL - time.Hour)
	if rep := f.sweep(t); rep.OrphanFilesRemoved != 0 {
		t.Fatalf("removed within the TTL, when a proposal could still attach it: %+v", rep)
	}
	decide(toApprove, "approve")

	f.now = f.now.Add(jobs.OrphanGrace + 2*time.Hour)
	if rep := f.sweep(t); rep.OrphanFilesRemoved != 2 {
		t.Fatalf("%+v, want the abandoned upload and the rejected proposal's file removed", rep)
	}
	if exists(abandoned) || exists(rejected) {
		t.Fatal("an orphan is still there")
	}
	if !exists(essay) || !exists(approved) {
		t.Fatal("a file that a document version points at was removed")
	}
	if n := f.Count(`SELECT count(*) FROM document_version WHERE storage_key = ANY($1)`, []string{essay, approved}); n != 2 {
		t.Fatalf("%d of the two attached files are recorded", n)
	}

	// The store is gone through once an hour, not once a tick.
	_, another := upload(ken.Actor, "submission", "another")
	if rep := f.sweep(t); rep.OrphanFilesRemoved != 0 || !exists(another) {
		t.Fatalf("the store was listed again at once: %+v", rep)
	}
	f.now = f.now.Add(2 * time.Hour)
	if rep := f.sweep(t); rep.OrphanFilesRemoved != 1 || exists(another) {
		t.Fatalf("an hour later: %+v", rep)
	}

	// Where proposals never expire, a file may be waited on for ever, and
	// nothing is removed by age.
	_, kept := upload(ken.Actor, "submission", "kept")
	patient := pipeline.New(f.Pool, f.P.Registry(), pipeline.Config{})
	patient.SetClock(func() time.Time { return f.now.Add(1000 * time.Hour) })
	rep, err := jobs.New(f.Pool, patient, f.system, jobs.Config{Blob: f.Blob}, nil).Sweep(ctx)
	if err != nil || rep.OrphanFilesRemoved != 0 || !exists(kept) {
		t.Fatalf("with no proposal TTL: %+v, %v", rep, err)
	}
}

// The bucket or directory the files are kept in may hold other things: a
// backup, another program's objects, a file somebody dropped among ours. The
// sweep removes the server's own files that nothing points at, and nothing
// the server did not write, however old — whether the store keeps a file
// where it was uploaded or, like S3, moves it on attaching.
func TestTheSweepRemovesOnlyTheServersOwnFiles(t *testing.T) {
	for name, wrap := range map[string]func(*blob.FSStore) blob.Store{
		"on disk":            func(fs *blob.FSStore) blob.Store { return fs },
		"in an object store": func(fs *blob.FSStore) blob.Store { return testkit.ObjectStore{FSStore: fs} },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, 1)
			store := wrap(f.Blob)
			f.runner = jobs.New(f.Pool, f.P, f.system, jobs.Config{Blob: store}, nil)
			ctx := context.Background()
			upload := func() string {
				t.Helper()
				out := f.MustCall(f.Sato, "document.upload_url", m{"course_id": f.Course, "kind": "material", "content_type": "text/plain"}, "")
				u := testkit.Result[tools.UploadURLOut](t, out)
				key, ct, err := f.Blob.Redeem(strings.TrimPrefix(u.UploadURL, "http://lms.test"+blob.BlobPath), "PUT")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Blob.Put(ctx, key, ct, strings.NewReader("never attached"), 1<<20); err != nil {
					t.Fatal(err)
				}
				return key
			}
			// Ours: an upload never attached, and one moved to its final
			// key by an attach that then did not commit.
			staged, moved := upload(), upload()
			if _, err := store.Finalize(ctx, moved); err != nil {
				t.Fatal(err)
			}
			moved = store.FinalKey(moved)
			// Not ours, beside our files and among them.
			course := f.Course.String()
			foreign := []string{"backups/nightly.sql.gz", "README", "courses/syllabus.pdf", "courses/" + course + "/cover.png",
				"courses/" + course + "/" + strings.ToUpper(uuid.NewString()), "attached/courses/" + course + "/cover.png"}
			for _, key := range foreign {
				p := filepath.Join(f.Blob.Root(), filepath.FromSlash(key))
				if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("not the server's"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			f.now = f.now.Add(pipeline.DefaultProposalTTL + jobs.OrphanGrace + time.Hour)
			if rep := f.sweep(t); rep.OrphanFilesRemoved != 2 {
				t.Fatalf("%+v, want the two old files of ours removed", rep)
			}
			for _, key := range []string{staged, moved} {
				if _, err := f.Blob.Stat(ctx, key); !errors.Is(err, blob.ErrNotFound) {
					t.Errorf("%s is still there: %v", key, err)
				}
			}
			for _, key := range foreign {
				if _, err := os.Stat(filepath.Join(f.Blob.Root(), filepath.FromSlash(key))); err != nil {
					t.Errorf("%s, which the server never wrote, was removed: %v", key, err)
				}
			}
		})
	}
}

// The sweep names what it swept in its idempotency key, and the query that
// finds what is newly due looks for that key. The two must spell the due
// date the same way — Go floors to the second, SQL must not round — or an
// assignment due at half past a second is found again on every tick, and
// with enough of them the batch is nothing but repeats.
func TestADueDateWithAFractionOfASecondIsSweptOnce(t *testing.T) {
	f := setup(t, 2)
	for i, fraction := range []time.Duration{750 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond} {
		due := f.now.Add(time.Duration(i+1) * time.Hour).Truncate(time.Second).Add(fraction)
		f.Exec(`UPDATE assignment SET due_at = $2 WHERE id = $1`, f.HW4, due)
		f.now = due.Add(time.Minute)
		if rep := f.sweep(t); rep.AssignmentsClosed != 1 {
			t.Fatalf("fraction %s: %+v, want HW4 closed", fraction, rep)
		}
		still, err := f.Q.ListAssignmentsNewlyPastDue(context.Background(), dbq.ListAssignmentsNewlyPastDueParams{Now: &f.now, SystemActorID: f.system, MaxRows: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(still) != 0 {
			t.Fatalf("fraction %s: HW4 is still found as newly due after being swept", fraction)
		}
	}
}
