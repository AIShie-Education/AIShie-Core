package tools_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Deleting an assignment for good (docs/schema.md §2.5, An assignment is
// deleted for good): what goes with it is counted first, and confirmed as it
// was counted; a person deletes it with everything that is its, an agent
// only one nobody has started on; what pointed at it reads that it was
// deleted, and the totals it counted in are worked out again without it.

// deletionCounts is confirm with every count n: what a caller who was shown
// at least as much as there is sends.
func deletionCounts(n int) m {
	return m{"submissions": n, "handed_in": n, "drafts": n, "missing": n, "grades": n, "posted": n, "files": n,
		"proposals": n, "totals": n}
}

// deletionPreview is what deleting assignment would take, as actor is shown it.
func (b *built) deletionPreview(t *testing.T, actor, assignment uuid.UUID) tools.AssignmentDeletePreviewOut {
	t.Helper()
	return testkit.Result[tools.AssignmentDeletePreviewOut](t, b.do(t, actor, "assignment.delete_preview",
		m{"course_id": b.course, "assignment_id": assignment}))
}

// deleteArgs deletes assignment, confirming what was counted.
func deleteArgs(b *built, assignment uuid.UUID, confirm any) m {
	return m{"course_id": b.course, "assignment_id": assignment, "confirm": confirm}
}

// assignment makes an assignment as Sato, published or not, counting toward
// the Assignments bucket or toward nothing.
func (b *built) assignment(t *testing.T, title string, published, graded bool) uuid.UUID {
	t.Helper()
	args := m{"course_id": b.course, "title": title, "points_possible": 10}
	if graded {
		args["component_id"] = b.bucket
	}
	id := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", args)).ID
	if published {
		b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": id})
	}
	return id
}

// fileOn uploads a file of kind as actor and gives it as the one file of a
// version.
func (b *built) fileOn(t *testing.T, actor uuid.UUID, kind, filename string) []m {
	t.Helper()
	return []m{{"upload_token": b.upload(t, actor, kind, "application/pdf", []byte("%PDF "+filename)), "filename": filename}}
}

func refusal(p tools.AssignmentDeletePreviewOut) string {
	if p.Refusal == nil {
		return ""
	}
	return *p.Refusal
}

// HW3, with its brief, Yuki's work with its file graded and posted with
// feedback, Ken's work waiting for the grader's proposal, and Aoi's draft,
// goes for good, with everything that is its; what is not its stays, its
// brief among it, what pointed at it says it was deleted, and the totals it
// counted in are worked out again without it.
func TestAnAssignmentIsDeletedForGoodWithEverythingThatIsIts(t *testing.T) {
	b := build(t)
	ctx := context.Background()

	// HW3's brief, a file and text, published, and HW3 pointing at it.
	brief := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course,
		"kind": "instructions", "title": "HW3 brief", "body_md": "Write 500 words.", "files": b.fileOn(t, b.sato, "instructions", "brief.pdf")}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": brief.DocumentID})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": brief.DocumentID})

	// Yuki's work, with a file; the grader grades it, Sato approves and
	// posts, and adds a file of feedback.
	yukiWork := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "My essay."})).SubmissionID
	b.do(t, b.yuki, "document.create", m{"course_id": b.course, "kind": "submission", "submission_id": yukiWork, "title": "essay.pdf",
		"files": b.fileOn(t, b.yuki, "submission", "essay.pdf")})
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": yukiWork})
	graded := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": yukiWork, "score": 90}, "grader-yuki")
	decision := b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": graded.ActionID, "decision": "approve"})
	yukiGrade, _, _ := b.liveGrade(t, &yukiWork, nil, b.yukiM)
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3})
	b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": "feedback", "grade_id": yukiGrade, "title": "Comments",
		"files": b.fileOn(t, b.sato, "feedback", "comments.pdf")})
	if total, _ := b.totalOf(t, b.total, b.yukiM); !total.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("Yuki's total before: %s", total)
	}

	// Ken's work, which the grader proposes to grade; Aoi's draft.
	kenWork := b.submit(t, b.ken, "Ken's essay")
	waiting := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70}, "grader-ken")
	if waiting.Status != domain.StatusProposed {
		t.Fatalf("the grader's proposal: %+v", waiting)
	}
	aoi := b.person(t, "Aoi", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": aoi, "preset": "student"})
	b.do(t, aoi, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "A start."})

	// The tutor answers Yuki relying on the brief's file.
	conv, question := b.open(t, b.yuki, b.tutorM, "What does the brief ask for?")
	answer := b.sourced(t, conv, question, []m{{"document_id": brief.DocumentID, "version_id": *brief.VersionID, "file_id": brief.FileIDs[0]}})

	// The files that are HW3's work's, and its brief's, as the store holds
	// them.
	keysOf := func(where string, args ...any) []string {
		t.Helper()
		var keys []string
		rows, err := b.Pool.Query(ctx, `SELECT f.storage_key FROM document_version_file f JOIN document d ON d.id = f.document_id
			WHERE `+where+` ORDER BY 1`, args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
		}
		rows.Close()
		return keys
	}
	keys, briefKeys := keysOf(`d.submission_id = $1 OR d.grade_id = $2`, yukiWork, yukiGrade), keysOf(`d.id = $1`, brief.DocumentID)
	if len(keys) != 2 || len(briefKeys) != 1 {
		t.Fatalf("HW3's work's files: %v; its brief's: %v", keys, briefKeys)
	}
	briefBefore, _ := b.courseDocuments(t)

	// What goes, counted, never named; Sato may.
	p := b.deletionPreview(t, b.sato, b.hw3)
	want := tools.DeletionCounts{Submissions: 3, HandedIn: 2, Drafts: 1, Grades: 1, Posted: 1, Files: 2, Proposals: 1, Totals: 1}
	if !reflect.DeepEqual(p.Counts, want) || p.Title != "HW3" || !p.Published || !p.InGrade || p.Refusal != nil {
		t.Fatalf("the preview: %+v, want counts %+v", p, want)
	}

	out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "delete-hw3")
	if out.Status != domain.StatusExecuted {
		t.Fatalf("Sato deleting HW3: %+v", out)
	}
	deletion := *out.ActionID
	got := testkit.Result[tools.AssignmentDeleteOut](t, out)
	if !got.Deleted || got.AssignmentID != b.hw3 || got.Title != "HW3" ||
		got.Removed != (tools.DeletionRemoved{Submissions: 3, Grades: 1, Files: 2}) ||
		got.ProposalsCancelled != 1 || got.FilesQueued != 2 || got.Snapshots != 2 {
		t.Fatalf("what the deletion says it did: %+v", got)
	}

	// Every row that was HW3's is gone.
	for what, n := range map[string]int{
		"the assignment": b.Count(`SELECT count(*) FROM assignment WHERE id = $1`, b.hw3),
		"its submissions": b.Count(`SELECT count(*) FROM submission WHERE assignment_id = $1 OR id = ANY($2)`, b.hw3,
			[]uuid.UUID{yukiWork, kenWork}),
		"its grades": b.Count(`SELECT count(*) FROM grade WHERE submission_id = ANY($1)`, []uuid.UUID{yukiWork, kenWork}),
		"the files handed in and given": b.Count(`SELECT count(*) FROM document WHERE submission_id = ANY($1) OR grade_id = $2`,
			[]uuid.UUID{yukiWork, kenWork}, yukiGrade),
		"their files' rows":   b.Count(`SELECT count(*) FROM document_version_file WHERE storage_key = ANY($1)`, keys),
		"its events":          b.Count(`SELECT count(*) FROM event WHERE assignment_id = $1`, b.hw3),
		"the scope rows":      b.Count(`SELECT count(*) FROM member_assignment_scope WHERE assignment_id = $1`, b.hw3),
		"anything un-emptied": b.Count(`SELECT count(*) FROM action WHERE redacted_by_action_id IS NOT NULL AND (payload <> '{}' OR result IS NOT NULL)`),
	} {
		if n != 0 {
			t.Errorf("%s: %d rows left", what, n)
		}
	}
	// Its work's files are queued to leave the store, and are there until
	// they do.
	if n := b.Count(`SELECT count(*) FROM blob_deletion WHERE storage_key = ANY($1) AND queued_by_action_id = $2 AND attempts = 0`,
		keys, deletion); n != 2 {
		t.Fatalf("%d of its work's 2 files are queued", n)
	}
	for _, k := range append(slices.Clone(keys), briefKeys...) {
		if _, err := b.Blob.Stat(ctx, k); err != nil {
			t.Fatalf("a file went before the job runner took it: %v", err)
		}
	}

	// Its brief is left in the course as it was: neither purged nor
	// archived, its file neither queued nor gone, and what was done to it
	// not emptied. An answer that relied on it reads as before, and an
	// export made now names its file.
	if after, _ := b.courseDocuments(t); after != briefBefore {
		t.Fatalf("deleting HW3 changed the course's documents:\nbefore %s\nafter  %s", briefBefore, after)
	}
	if n := b.Count(`SELECT count(*) FROM blob_deletion WHERE storage_key = ANY($1)`, briefKeys); n != 0 {
		t.Fatal("the brief's file is queued to leave the store")
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE target_id = $1 OR result->>'document_id' = $1::text`, brief.DocumentID); n == 0 ||
		b.Count(`SELECT count(*) FROM action WHERE (target_id = $1 OR result->>'document_id' = $1::text) AND redacted_by_action_id IS NOT NULL`,
			brief.DocumentID) != 0 {
		t.Fatal("what was done to the brief was emptied with HW3")
	}
	if got := b.sourcesOf(t, b.sato, conv, answer); len(got) != 1 || got[0].Restricted || got[0].DocumentID == nil ||
		*got[0].DocumentID != brief.DocumentID || got[0].FileID == nil || *got[0].FileID != brief.FileIDs[0] {
		t.Fatalf("Sato reads the answer's source as %+v", got)
	}
	a := &audit{built: b}
	exportOut, _ := a.export(t, b.Root, m{"course_id": b.course})
	x := a.files(t, exportOut)
	var sources []any
	for _, msg := range list(x.byID[conv.String()]["messages"]) {
		if obj(msg)["id"] == answer.String() {
			sources = list(obj(msg)["sources"])
		}
	}
	if len(sources) != 1 || obj(sources[0])["purged"] == true || obj(sources[0])["file_id"] != brief.FileIDs[0].String() {
		t.Fatalf("the answer's source, exported after the deletion: %v", sources)
	}

	// The grader's proposal waiting on Ken's work is cancelled, saying why,
	// and the grader is told as of any cancellation.
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled' AND redacted_by_action_id = $2`,
		waiting.ActionID, deletion); n != 1 {
		t.Fatal("the waiting proposal is not cancelled and emptied")
	}
	told := false
	for _, e := range feed(t, b, b.grader) {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		told = told || (e.Type == "action.cancelled" && *e.ActionID == *waiting.ActionID && p["reason"] == "target_deleted" &&
			p["by_action_id"] == deletion.String())
	}
	if !told {
		t.Fatal("the grader is not told its proposal was cancelled")
	}

	// What was done to HW3 is emptied, kept as who did what and when; what
	// was not (the conversation) is as it was. The deletion is kept whole.
	for what, id := range map[string]uuid.UUID{"the grader's approved grade": *graded.ActionID, "Sato's decision about it": *decision.ActionID} {
		if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND redacted_by_action_id = $2 AND payload = '{}' AND result IS NULL`,
			id, deletion); n != 1 {
			t.Errorf("%s is not emptied", what)
		}
	}
	for what, typ := range map[string]string{"HW3's creation": "assignment.create", "its publishing": "assignment.publish",
		"its update": "assignment.update", "Yuki's file": "document.create", "Yuki's draft": "submission.create",
		"her hand-in": "submission.submit", "the post": "grade.post"} {
		if n := b.Count(`SELECT count(*) FROM action WHERE course_id = $1 AND action_type = $2 AND redacted_by_action_id = $3`,
			b.course, typ, deletion); n == 0 {
			t.Errorf("%s (%s) is not emptied", what, typ)
		}
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type LIKE 'conversation.%' AND redacted_by_action_id IS NOT NULL`); n != 0 {
		t.Fatalf("%d actions of the conversation were emptied", n)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND redacted_by_action_id IS NULL AND payload->'confirm'->>'files' = '2'
		AND result->>'title' = 'HW3'`, deletion); n != 1 {
		t.Fatal("the deletion's own action is not kept whole")
	}
	stub := testkit.Result[tools.ActionView](t, b.do(t, b.sato, "action.get", m{"course_id": b.course, "action_id": graded.ActionID}))
	if stub.Redacted == nil || stub.Redacted.ByActionID != deletion || stub.Redacted.At == nil || string(stub.Payload) != "{}" {
		t.Fatalf("action.get of an emptied action: %+v", stub)
	}
	if n := b.Count(`SELECT count(*) FROM assignment_deletion WHERE assignment_id = $1 AND title = 'HW3' AND was_published
		AND action_id = $2 AND deleted_by_actor_id = $3 AND deleted_by_member_id = $4 AND submissions = 3 AND grades = 1
		AND files = 2 AND proposals = 1 AND totals = 1`, b.hw3, deletion, b.sato, b.satoM); n != 1 {
		t.Fatal("the record of the deletion is not as it went")
	}

	// The feed's one record of it, which everyone who reads the course is
	// shown, with its title and no counts.
	for who, reader := range map[string]uuid.UUID{"Yuki": b.yuki, "Ken": b.ken, "Sato": b.sato} {
		n := 0
		for _, e := range feed(t, b, reader) {
			if e.Type == "assignment.deleted" {
				var p map[string]any
				_ = json.Unmarshal(e.Payload, &p)
				if e.AssignmentID != nil || *e.SubjectID != b.hw3 || p["title"] != "HW3" || len(p) != 1 || *e.ActionID != deletion {
					t.Fatalf("%s is told %+v %v", who, e, p)
				}
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s is told of the deletion %d times", who, n)
		}
	}

	// Yuki's totals are worked out again without it, the change recorded;
	// what she was shown before keeps its number, its line of HW3 saying it
	// was deleted.
	if total, none := b.totalOf(t, b.total, b.yukiM); !none || !total.IsZero() {
		t.Fatalf("Yuki's total, with nothing beneath it any more: %s", total)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE student_member_id = $1 AND component_id = $2 AND origin = 'computed'
		AND superseded_by IS NOT NULL AND score = 90
		AND breakdown->'items' @> jsonb_build_array(jsonb_build_object('id', $3::text, 'kind', 'assignment', 'deleted', true))`,
		b.yukiM, b.bucket, b.hw3); n != 1 {
		t.Fatalf("%d of Yuki's earlier Assignments totals say HW3 was deleted, want the one", n)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE origin = 'computed' AND breakdown::text LIKE '%' || $1::text || '%'
		AND NOT breakdown->'items' @> jsonb_build_array(jsonb_build_object('id', $1::text, 'deleted', true))`, b.hw3); n != 0 {
		t.Fatalf("%d totals still say what Yuki scored on HW3", n)
	}

	// The grader, listed for HW3 alone, now reaches no assignment.
	if got := testkit.Result[tools.AssignmentListOut](t, b.do(t, b.grader, "assignment.list", m{"course_id": b.course})); len(got.Assignments) != 0 {
		t.Fatalf("the grader still reaches %+v", got.Assignments)
	}

	// Asked about again, it was deleted, when and by which action.
	_, err := b.Call(b.sato, "assignment.get", m{"course_id": b.course, "assignment_id": b.hw3}, "")
	if e, ok := apperr.As(err); !ok || e.Code != apperr.NotFound || e.Details["reason"] != "deleted" || e.Details["by_action_id"] != deletion {
		t.Fatalf("assignment.get of HW3: %v", err)
	}
	// The same call again, under its key: what it did, nothing done again;
	// the same key for another call is refused; a new key finds nothing.
	again := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "delete-hw3")
	if !again.Replayed || *again.ActionID != deletion || testkit.Result[tools.AssignmentDeleteOut](t, again) != got {
		t.Fatalf("the deletion replayed: %+v", again)
	}
	if _, err := b.Call(b.sato, "assignment.delete", deleteArgs(b, b.hw3, deletionCounts(9)), "delete-hw3"); !apperr.Is(err, apperr.IdempotencyConflict) {
		t.Fatalf("the key reused for another call: %v", err)
	}
	actions := b.Count(`SELECT count(*) FROM action`)
	_, err = b.Call(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "delete-hw3-again")
	if e, ok := apperr.As(err); !ok || e.Code != apperr.NotFound || e.Details["reason"] != "deleted" {
		t.Fatalf("deleting it again: %v", err)
	}
	if n := b.Count(`SELECT count(*) FROM action`); n != actions {
		t.Fatal("deleting a deleted assignment was recorded")
	}
	// A call whose record was emptied, retried, is told what became of its
	// target, whatever it sends.
	_, err = b.Call(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 70}, "grader-ken")
	if e, ok := apperr.As(err); !ok || e.Code != apperr.NotFound || e.Details["reason"] != "target_deleted" || e.Details["by_action_id"] != deletion {
		t.Fatalf("the grader's proposal retried: %v", err)
	}
}

// Who deletes an assignment, and how: a person with assignment_write at any
// level allowed, as that level says; an agent only one nobody has started
// on, refused on the call, before anything waits, and again when a proposal
// is approved; nobody beyond their reach; nobody without the permission.
func TestWhoDeletesAnAssignment(t *testing.T) {
	b := build(t)
	empty := func(title string, published bool) uuid.UUID { return b.assignment(t, title, published, false) }

	// Sato's assistant, an agent of his, deleting without anyone's
	// confirmation: one nobody has started on goes; one with nothing but a
	// 'missing' row recorded does not, refused as people_only and recorded.
	bot := b.agent(t, b.sato, "Sato's assistant")
	b.delegate(t, b.sato, bot, m{"perms": m{"assignment_write": "autonomous"}, "student_scope": "all"})
	draft := empty("Draft quiz", false)
	p := b.deletionPreview(t, bot, draft)
	if p.Refusal != nil || !reflect.DeepEqual(p.Counts, tools.DeletionCounts{}) {
		t.Fatalf("the agent's preview of an empty assignment: %+v", p)
	}
	if out := b.MustCall(bot, "assignment.delete", deleteArgs(b, draft, p.Counts), "bot-draft"); out.Status != domain.StatusExecuted {
		t.Fatalf("the agent deleting an empty assignment: %+v", out)
	}
	// Nobody could see it: it is news for those who write assignments.
	for who, n := range map[uuid.UUID]int{b.sato: 1, b.yuki: 0} {
		seen := 0
		for _, e := range feed(t, b, who) {
			if e.Type == "assignment.deleted_unreleased" && *e.SubjectID == draft {
				seen++
			}
		}
		if seen != n {
			t.Fatalf("%s is told of the unreleased deletion %d times, want %d", who, seen, n)
		}
	}
	quiz := empty("Quiz", true)
	b.do(t, b.sato, "submission.record_missing", m{"course_id": b.course, "assignment_id": quiz, "student_member_id": b.kenM})
	if p := b.deletionPreview(t, bot, quiz); refusal(p) != tools.DeletePeopleOnly || p.Counts.Missing != 1 {
		t.Fatalf("the agent's preview of an assignment with a missing row: %+v", p)
	}
	out := b.MustCall(bot, "assignment.delete", deleteArgs(b, quiz, deletionCounts(1)), "bot-quiz")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Forbidden || reason(out) != "people_only" {
		t.Fatalf("the agent deleting an assignment with work: %+v", out)
	}
	if p := b.deletionPreview(t, b.sato, quiz); p.Refusal != nil {
		t.Fatalf("Sato's preview of it: %+v", p)
	}

	// Another of his agents, deleting by proposal: refused on the call where
	// there is work, before anything waits; an empty one waits, and work
	// coming meanwhile refuses it as it is approved — Sato, its owner, is
	// told so and not let decide it, and Tanaka, who approves, sees it fail.
	asker := b.agent(t, b.sato, "Sato's other assistant")
	b.delegate(t, b.sato, asker, m{"perms": m{"assignment_write": "confirm_required"}, "student_scope": "all"})
	if out := b.MustCall(asker, "assignment.delete", deleteArgs(b, quiz, deletionCounts(1)), "asker-quiz"); out.Status != domain.StatusFailed ||
		reason(out) != "people_only" {
		t.Fatalf("the agent proposing to delete an assignment with work: %+v", out)
	}
	open := empty("Open quiz", true)
	proposed := b.MustCall(asker, "assignment.delete", deleteArgs(b, open, b.deletionPreview(t, asker, open).Counts), "asker-open")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the agent proposing to delete an empty assignment: %+v", proposed)
	}
	b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": open, "body": "A start."})
	owner := b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}, "owner-approves")
	refused, _ := owner.Error.Details["refusal"].(*apperr.Error)
	if owner.Status != domain.StatusFailed || reason(owner) != "owner_would_be_refused" || refused == nil || refused.Details["reason"] != "people_only" {
		t.Fatalf("Sato deciding his agent's deletion once there is work: %+v", owner)
	}
	tanaka := b.person(t, "Tanaka", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": tanaka, "preset": "instructor",
		"perms": m{"assignment_write": "pending_review"}})
	decided := testkit.Result[pipeline.DecideOut](t, b.do(t, tanaka, "action.decide",
		m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}))
	if decided.Outcome != domain.StatusFailed || decided.Error == nil || decided.Error.Details["reason"] != "people_only" {
		t.Fatalf("Tanaka approving it: %+v", decided)
	}
	if n := b.Count(`SELECT count(*) FROM assignment WHERE id = $1`, open); n != 1 {
		t.Fatal("the assignment went, with Yuki's draft")
	}

	// A person whose deletions are reviewed afterwards deletes at once.
	reviewed := empty("Reviewed quiz", true)
	out = b.MustCall(tanaka, "assignment.delete", deleteArgs(b, reviewed, b.deletionPreview(t, tanaka, reviewed).Counts), "tanaka")
	if out.Status != domain.StatusExecuted || out.ReviewState != domain.ReviewPending {
		t.Fatalf("Tanaka deleting, reviewed afterwards: %+v", out)
	}

	// A person whose deletions wait for a confirmation proposes; approved,
	// it goes as the proposer, held to what they confirmed: more added while
	// it waits fails it.
	mori := b.person(t, "Mori", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": mori, "preset": "instructor",
		"perms": m{"assignment_write": "confirm_required"}})
	waited := empty("Waited quiz", true)
	proposal := b.MustCall(mori, "assignment.delete", deleteArgs(b, waited, b.deletionPreview(t, mori, waited).Counts), "mori-1")
	if proposal.Status != domain.StatusProposed {
		t.Fatalf("Mori deleting by proposal: %+v", proposal)
	}
	if d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide",
		m{"course_id": b.course, "action_id": proposal.ActionID, "decision": "approve"})); d.Outcome != domain.StatusExecuted {
		t.Fatalf("Sato approving Mori's deletion: %+v", d)
	}
	if n := b.Count(`SELECT count(*) FROM assignment_deletion WHERE assignment_id = $1 AND action_id = $2 AND deleted_by_actor_id = $3`,
		waited, proposal.ActionID, mori); n != 1 {
		t.Fatal("the approved deletion is not Mori's, under the proposal")
	}
	stale := empty("Stale quiz", true)
	proposal = b.MustCall(mori, "assignment.delete", deleteArgs(b, stale, b.deletionPreview(t, mori, stale).Counts), "mori-2")
	b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": stale, "body": "Mine."})
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide",
		m{"course_id": b.course, "action_id": proposal.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusFailed || d.Error.Details["reason"] != "confirm_stale" {
		t.Fatalf("approving a deletion more was added to while it waited: %+v", d)
	}

	// Nobody without assignment_write; nobody beyond their reach.
	b.try(t, b.yuki, "assignment.delete", deleteArgs(b, stale, deletionCounts(9)), apperr.Forbidden)
	ito := b.person(t, "Ito", "")
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": ito, "preset": "ta", "perms": m{"assignment_write": "autonomous"},
		"student_scope": "listed", "listed_students": []uuid.UUID{b.yukiM}})
	if p := b.deletionPreview(t, ito, stale); refusal(p) != "student_out_of_scope" {
		t.Fatalf("Ito's preview of an assignment with Ken's work: %+v", p)
	}
	out = b.MustCall(ito, "assignment.delete", deleteArgs(b, stale, deletionCounts(9)), "ito")
	if out.Status != domain.StatusDenied || reason(out) != "student_out_of_scope" {
		t.Fatalf("Ito deleting Ken's work: %+v", out)
	}
}

// The deletion is held to what its caller was shown: more now than was
// confirmed is refused, saying what there is; less is not.
func TestADeletionIsHeldToWhatItWasShown(t *testing.T) {
	b := build(t)
	b.submit(t, b.yuki, "Yuki's essay")
	p := b.deletionPreview(t, b.sato, b.hw3)
	b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "body": "Ken starts."})

	out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "shown-one")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Conflict || reason(out) != "confirm_stale" {
		t.Fatalf("deleting with a stale confirmation: %+v", out)
	}
	current, _ := json.Marshal(out.Error.Details["current"])
	var now tools.DeletionCounts
	if err := json.Unmarshal(current, &now); err != nil || now.Submissions != 2 || now.Drafts != 1 {
		t.Fatalf("the refusal says there is now %s", current)
	}
	if n := b.Count(`SELECT count(*) FROM assignment WHERE id = $1`, b.hw3); n != 1 {
		t.Fatal("refused, it went all the same")
	}
	// Shown more than there is: what is there goes.
	if out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, deletionCounts(9)), "shown-more"); out.Status != domain.StatusExecuted {
		t.Fatalf("deleting with a confirmation of more than there is: %+v", out)
	}
	// Without a confirmation, or one that is not counts, it is not a call.
	for _, args := range []m{{"course_id": b.course, "assignment_id": b.hw4(t)},
		{"course_id": b.course, "assignment_id": b.hw4(t), "confirm": m{"submissions": 1}},
		{"course_id": b.course, "assignment_id": b.hw4(t), "confirm": deletionCounts(-1)}} {
		if _, err := b.Call(b.sato, "assignment.delete", args, "malformed-"+uuid.NewString()); !apperr.Is(err, apperr.InvalidArgument) {
			t.Fatalf("a deletion confirming %v: %v", args["confirm"], err)
		}
	}
}

// hw4 is an assignment nobody has started on.
func (b *built) hw4(t *testing.T) uuid.UUID {
	t.Helper()
	return b.assignment(t, "HW4", true, true)
}

// An archived course takes no write, a deletion included, and its preview
// says so.
func TestAnArchivedCourseKeepsItsAssignments(t *testing.T) {
	b := build(t)
	b.submit(t, b.yuki, "Yuki's essay")
	p := b.deletionPreview(t, b.sato, b.hw3)
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	if got := b.deletionPreview(t, b.sato, b.hw3); refusal(got) != "course_archived" {
		t.Fatalf("the preview in an archived course: %+v", got)
	}
	out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "archived")
	if out.Status != domain.StatusDenied || reason(out) != "course_archived" {
		t.Fatalf("deleting in an archived course: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM submission WHERE assignment_id = $1`, b.hw3); n != 1 {
		t.Fatal("Yuki's work went")
	}
}

// An assignment taken out of the grade before it is deleted leaves the
// totals it once counted in as an assignment in the grade does: those
// written while it counted keep their number, and their line of it says
// only that it was deleted, never what the student scored on it.
func TestAnAssignmentTakenOutOfTheGradeLeavesNoScoreInTheTotalsOnceDeleted(t *testing.T) {
	b := build(t)
	b.gradeHW3(t, b.yuki, 90, true)
	if total, _ := b.totalOf(t, b.bucket, b.yukiM); !total.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("Yuki's Assignments total before: %s", total)
	}
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "clear_component": true})
	scored := `SELECT count(*) FROM grade WHERE origin = 'computed' AND breakdown::text LIKE '%' || $1::text || '%'
		AND NOT breakdown->'items' @> jsonb_build_array(jsonb_build_object('id', $1::text, 'deleted', true))`
	if n := b.Count(scored, b.hw3); n == 0 {
		t.Fatal("no total written while HW3 counted names it: the test shows nothing")
	}

	p := b.deletionPreview(t, b.sato, b.hw3)
	if p.InGrade || p.Counts.Totals != 0 {
		t.Fatalf("the preview of an assignment out of the grade: %+v", p)
	}
	got := testkit.Result[tools.AssignmentDeleteOut](t, b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "delete-moved-out"))
	if !got.Deleted || got.Snapshots != 0 {
		t.Fatalf("deleting it: %+v", got)
	}
	if n := b.Count(scored, b.hw3); n != 0 {
		t.Fatalf("%d totals still say what Yuki scored on HW3", n)
	}
	if n := b.Count(`SELECT count(*) FROM grade WHERE student_member_id = $1 AND component_id = $2 AND origin = 'computed'
		AND superseded_by IS NOT NULL AND score = 90
		AND breakdown->'items' @> jsonb_build_array(jsonb_build_object('id', $3::text, 'kind', 'assignment', 'deleted', true))`,
		b.yukiM, b.bucket, b.hw3); n != 1 {
		t.Fatalf("%d of Yuki's earlier Assignments totals say HW3 was deleted, keeping their number; want the one", n)
	}
}

// courseDocuments is everything a purge (document.purge) changes of the
// course's material, instructions and rubrics, as the database holds it: the
// documents, their versions, their files, the files' texts and PDFs, the
// answers' sources that name them, and what was done to them; and the keys
// of their files and PDFs in the store. Deleting an assignment leaves all of
// it as it was.
func (b *built) courseDocuments(t *testing.T) (state string, keys []string) {
	t.Helper()
	const docs = `SELECT id FROM document WHERE course_id = $1 AND kind IN ('material', 'instructions', 'rubric')`
	if err := b.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
		'documents', (SELECT jsonb_agg(to_jsonb(d) ORDER BY d.id) FROM document d WHERE d.id IN (`+docs+`)),
		'versions', (SELECT jsonb_agg(to_jsonb(v) ORDER BY v.id) FROM document_version v WHERE v.document_id IN (`+docs+`)),
		'files', (SELECT jsonb_agg(to_jsonb(f) ORDER BY f.id) FROM document_version_file f WHERE f.document_id IN (`+docs+`)),
		'texts', (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM document_version_text x WHERE x.document_id IN (`+docs+`)),
		'renditions', (SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id) FROM file_rendition r
		               JOIN document_version_file f ON f.id = r.file_id WHERE f.document_id IN (`+docs+`)),
		'sources', (SELECT jsonb_agg(to_jsonb(s) ORDER BY s.message_id, s.position) FROM conversation_message_source s
		            WHERE s.document_id IN (`+docs+`)),
		'actions', (SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM action a
		            WHERE a.target_id IN (`+docs+`) OR a.result->>'document_id' IN (SELECT id::text FROM (`+docs+`) d))
		)::text`, b.course).Scan(&state); err != nil {
		t.Fatal(err)
	}
	rows, err := b.Pool.Query(t.Context(), `SELECT f.storage_key FROM document_version_file f WHERE f.document_id IN (`+docs+`)
		UNION SELECT r.storage_key FROM file_rendition r JOIN document_version_file f ON f.id = r.file_id
		WHERE f.document_id IN (`+docs+`) AND r.storage_key IS NOT NULL ORDER BY 1`, b.course)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	return state, keys
}

// leftAsTheyWere fails t unless the course's material, instructions and
// rubrics are as before, and every file of theirs is in the store, queued
// to leave it by nothing.
func (b *built) leftAsTheyWere(t *testing.T, what, before string, keys []string) {
	t.Helper()
	if after, _ := b.courseDocuments(t); after != before {
		t.Errorf("%s changed the course's documents:\nbefore %s\nafter  %s", what, before, after)
	}
	if n := b.Count(`SELECT count(*) FROM blob_deletion WHERE storage_key = ANY($1)`, keys); n != 0 {
		t.Errorf("%s queued %d of the course's documents' files to leave the store", what, n)
	}
	for _, k := range keys {
		if _, err := b.Blob.Stat(t.Context(), k); err != nil {
			t.Errorf("%s took %s from the store: %v", what, k, err)
		}
	}
}

// publishedDoc is a document of kind Sato writes and publishes, with text
// and a file.
func (b *built) publishedDoc(t *testing.T, kind, title string) tools.DocumentCreateOut {
	t.Helper()
	out := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": kind,
		"title": title, "body_md": title + ", in full.", "files": b.fileOn(t, b.sato, kind, kind+".pdf")}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": out.DocumentID})
	return out
}

// Deleting an assignment leaves its instructions and rubric in the course as
// they are, whatever was done under them. HW3, pointed at a published brief
// and a published rubric, with Yuki's and Ken's work handed in under the
// brief's first version, Sato's draft grades on it under each of the
// rubric's two versions, and the tutor's answer relying on the brief and on
// a lecture, goes; the brief and the rubric, and every version of each, are
// neither purged nor archived, Sato reads every version with its text and
// its file, their files stay in the store, queued by nothing, the answer's
// sources read as before, and once HW4 names them Yuki reads the brief
// again. Material is no assignment's to name.
func TestDeletingAnAssignmentLeavesItsInstructionsAndRubricInTheCourse(t *testing.T) {
	b := build(t)
	brief, rubric, lecture := b.publishedDoc(t, "instructions", "HW3 brief"), b.publishedDoc(t, "rubric", "HW3 rubric"),
		b.publishedDoc(t, "material", "Lecture 3")
	out := b.MustCall(b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3,
		"instructions_document_id": lecture.DocumentID}, "name-material")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.FailedPrecondition {
		t.Fatalf("naming a lecture as HW3's instructions: %+v", out)
	}
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3,
		"instructions_document_id": brief.DocumentID, "rubric_document_id": rubric.DocumentID})

	// Yuki and Ken hand in under the brief's first version, and the brief is
	// written again; Sato grades Yuki under the rubric's first version and
	// Ken under its second, neither posted.
	yuki, ken := b.submit(t, b.yuki, "Yuki's essay"), b.submit(t, b.ken, "Ken's essay")
	add := func(doc uuid.UUID, body string) uuid.UUID {
		return testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version",
			m{"course_id": b.course, "document_id": doc, "body_md": body, "publish": true})).VersionID
	}
	add(brief.DocumentID, "HW3 brief, corrected.")
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": yuki, "score": 8})
	add(rubric.DocumentID, "HW3 rubric, corrected.")
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": ken, "score": 6})
	if n := b.Count(`SELECT count(*) FROM submission s JOIN document_version v ON v.id = s.instructions_version_id
		WHERE s.id = ANY($1) AND v.document_id = $2`, []uuid.UUID{yuki, ken}, brief.DocumentID); n != 2 {
		t.Fatalf("%d of the two hand-ins are pinned to the brief: the test shows nothing", n)
	}
	if n := b.Count(`SELECT count(DISTINCT g.rubric_version_id) FROM grade g JOIN document_version v ON v.id = g.rubric_version_id
		WHERE g.submission_id = ANY($1) AND g.posted_at IS NULL AND v.document_id = $2`, []uuid.UUID{yuki, ken}, rubric.DocumentID); n != 2 {
		t.Fatalf("the draft grades pin %d of the rubric's two versions: the test shows nothing", n)
	}
	conv, question := b.open(t, b.yuki, b.tutorM, "What does the brief ask for?")
	answer := b.sourced(t, conv, question, []m{
		{"document_id": brief.DocumentID, "version_id": *brief.VersionID, "file_id": brief.FileIDs[0]},
		{"document_id": lecture.DocumentID, "version_id": *lecture.VersionID, "file_id": lecture.FileIDs[0]}})
	sourcesBefore := b.sourcesOf(t, b.sato, conv, answer)

	before, keys := b.courseDocuments(t)
	if len(keys) < 3 {
		t.Fatalf("the course's documents' files: %v", keys)
	}
	p := b.deletionPreview(t, b.sato, b.hw3)
	if p.Counts.Files != 0 || p.Counts.HandedIn != 2 || p.Counts.Grades != 2 {
		t.Fatalf("the preview: %+v", p)
	}
	if out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "delete-hw3"); out.Status != domain.StatusExecuted ||
		testkit.Result[tools.AssignmentDeleteOut](t, out).FilesQueued != 0 {
		t.Fatalf("deleting HW3: %+v", out)
	}
	b.leftAsTheyWere(t, "deleting HW3", before, keys)

	for _, doc := range []uuid.UUID{brief.DocumentID, rubric.DocumentID} {
		versions := testkit.Result[tools.DocumentVersionsOut](t, b.do(t, b.sato, "document.versions",
			m{"course_id": b.course, "document_id": doc})).Versions
		if len(versions) != 2 {
			t.Fatalf("document %s has %d versions", doc, len(versions))
		}
		for _, v := range versions {
			got := b.get(t, b.sato, m{"document_id": doc, "version_id": v.ID})
			if got.Purged != nil || got.Status != "active" || got.Version == nil || got.Version.Purged != nil || got.Version.BodyMD == nil {
				t.Fatalf("Sato reads version %d of %s as %+v", v.Seq, doc, got)
			}
			if v.Seq == 1 && len(got.Version.Files) != 1 {
				t.Fatalf("the first version of %s has no file: %+v", doc, got.Version)
			}
		}
	}
	if got := b.sourcesOf(t, b.sato, conv, answer); !reflect.DeepEqual(got, sourcesBefore) {
		t.Fatalf("Sato reads the answer's sources as %+v, before %+v", got, sourcesBefore)
	}
	// Named again, they are an assignment's as before.
	hw4 := b.hw4(t)
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": hw4,
		"instructions_document_id": brief.DocumentID, "rubric_document_id": rubric.DocumentID})
	if got := b.get(t, b.yuki, m{"document_id": brief.DocumentID}); got.Version == nil || got.Version.BodyMD == nil ||
		*got.Version.BodyMD != "HW3 brief, corrected." {
		t.Fatalf("Yuki reads the brief, named by HW4, as %+v", got.Version)
	}
}

// No seat without an administrator's rights purges anything of the course's
// documents by deleting an assignment, by any route the reviews of the
// deletion found. Sato and his assistant, neither of whom may purge a
// document, delete assignments that name a spare brief and a spare rubric
// and nobody's work (by the assistant and by Sato); that have a missing
// record graded under them; that have work Sato handed in for Yuki, graded;
// that have Yuki's own work, graded under a rubric named after she handed
// it in; that have work Yuki's agent handed in for her, graded; and that
// were pointed at another brief after the preview was read. After each the
// course's material, instructions and rubrics are as they were, and their
// files in the store, queued by nothing.
func TestNoSeatPurgesADocumentByDeletingAnAssignment(t *testing.T) {
	b := build(t)
	brief, rubric := b.publishedDoc(t, "instructions", "Spare brief").DocumentID, b.publishedDoc(t, "rubric", "Spare rubric").DocumentID
	other, lecture := b.publishedDoc(t, "instructions", "Another brief"), b.publishedDoc(t, "material", "Lecture 1")
	conv, question := b.open(t, b.yuki, b.tutorM, "Where do I start?")
	b.sourced(t, conv, question, []m{{"document_id": lecture.DocumentID, "version_id": *lecture.VersionID, "file_id": lecture.FileIDs[0]}})
	bot := b.agent(t, b.sato, "Sato's assistant")
	b.delegate(t, b.sato, bot, m{"perms": m{"assignment_write": "autonomous"}, "student_scope": "all"})
	for _, who := range []uuid.UUID{b.sato, bot} {
		for _, d := range []uuid.UUID{brief, rubric, other.DocumentID, lecture.DocumentID} {
			b.try(t, who, "document.purge", m{"course_id": b.course, "document_id": d, "reason": "Not wanted."}, apperr.Forbidden)
		}
	}
	named := func(by uuid.UUID, title string, instructions, rubric uuid.UUID) uuid.UUID {
		t.Helper()
		args := m{"course_id": b.course, "title": title, "points_possible": 10, "instructions_document_id": instructions}
		if rubric != uuid.Nil {
			args["rubric_document_id"] = rubric
		}
		id := testkit.Result[tools.IDOut](t, b.do(t, by, "assignment.create", args)).ID
		b.do(t, by, "assignment.publish", m{"course_id": b.course, "assignment_id": id})
		return id
	}
	deleted := func(by, assignment uuid.UUID, route string, confirm *tools.DeletionCounts) {
		t.Helper()
		before, keys := b.courseDocuments(t)
		if confirm == nil {
			p := b.deletionPreview(t, by, assignment)
			if p.Refusal != nil {
				t.Fatalf("%s: the preview refuses it: %+v", route, p)
			}
			confirm = &p.Counts
		}
		out := b.MustCall(by, "assignment.delete", deleteArgs(b, assignment, *confirm), "delete-"+uuid.NewString())
		if out.Status != domain.StatusExecuted {
			t.Fatalf("%s: deleting it: %+v", route, out)
		}
		b.leftAsTheyWere(t, route, before, keys)
	}
	pinned := func(route, query string, args ...any) {
		t.Helper()
		if b.Count(query, args...) == 0 {
			t.Fatalf("%s: nothing is pinned to the document: the test shows nothing", route)
		}
	}
	const briefPinned = `SELECT count(*) FROM submission s JOIN document_version v ON v.id = s.instructions_version_id
		WHERE s.assignment_id = $1 AND v.document_id = $2`
	const rubricPinned = `SELECT count(*) FROM grade g JOIN submission s ON s.id = g.submission_id
		JOIN document_version v ON v.id = g.rubric_version_id WHERE s.assignment_id = $1 AND v.document_id = $2`

	// Named, and nobody's work: by the assistant, and by Sato.
	deleted(bot, named(bot, "Named by the assistant", brief, rubric), "the assistant's assignment naming them", nil)
	deleted(b.sato, named(b.sato, "Named by Sato", brief, rubric), "Sato's assignment naming them", nil)

	// A missing record, graded.
	missing := named(b.sato, "Missing", brief, rubric)
	placeholder := testkit.Result[tools.SubmissionIDOut](t, b.do(t, b.sato, "submission.record_missing",
		m{"course_id": b.course, "assignment_id": missing, "student_member_id": b.kenM})).SubmissionID
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": placeholder, "score": 0})
	pinned("a missing record", rubricPinned, missing, rubric)
	deleted(b.sato, missing, "a graded missing record", nil)

	// Work Sato hands in for Yuki, graded.
	forYuki := named(b.sato, "For Yuki", brief, rubric)
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.sato, "submission.create",
		m{"course_id": b.course, "assignment_id": forYuki, "student_member_id": b.yukiM, "body": "Typed in for her."})).SubmissionID
	b.do(t, b.sato, "submission.submit", m{"course_id": b.course, "submission_id": work})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 5})
	pinned("work handed in for Yuki", briefPinned, forYuki, brief)
	pinned("work handed in for Yuki", rubricPinned, forYuki, rubric)
	deleted(b.sato, forYuki, "work handed in for a student, graded", nil)

	// Yuki's own work, handed in under another brief, then graded under the
	// spare rubric, named after she handed it in.
	own := named(b.sato, "Yuki's own", other.DocumentID, uuid.Nil)
	work = testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": own, "body": "Mine."})).SubmissionID
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": work})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": own, "rubric_document_id": rubric})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 7})
	pinned("Yuki's own work", briefPinned, own, other.DocumentID)
	pinned("Yuki's own work", rubricPinned, own, rubric)
	deleted(b.sato, own, "a student's own work, graded under a rubric named afterwards", nil)

	// Work Yuki's agent hands in for her, by proposals Sato approves,
	// graded.
	helper := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, helper, m{})
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"submission_write": "confirm_required"}})
	approve := func(out pipeline.Outcome) json.RawMessage {
		t.Helper()
		if out.Status != domain.StatusProposed {
			t.Fatalf("the agent's call: %+v", out)
		}
		d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": out.ActionID, "decision": "approve"}))
		if d.Outcome != domain.StatusExecuted {
			t.Fatalf("approving the agent's call: %+v", d)
		}
		return d.Result
	}
	byAgent := named(b.sato, "By Yuki's helper", brief, rubric)
	var draft tools.SubmissionCreateOut
	if err := json.Unmarshal(approve(b.MustCall(helper, "submission.create", m{"course_id": b.course, "assignment_id": byAgent,
		"student_member_id": b.yukiM, "body": "My answers."}, "helper-draft")), &draft); err != nil {
		t.Fatal(err)
	}
	approve(b.MustCall(helper, "submission.submit", m{"course_id": b.course, "submission_id": draft.SubmissionID}, "helper-submit"))
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": draft.SubmissionID, "score": 9})
	pinned("the helper's hand-in", briefPinned, byAgent, brief)
	pinned("the helper's hand-in", rubricPinned, byAgent, rubric)
	deleted(b.sato, byAgent, "work a student's agent handed in, graded", nil)

	// Ken's own work, handed in under the spare brief, and the assignment
	// pointed at another brief once the preview was read.
	moved := named(b.sato, "Moved", brief, uuid.Nil)
	work = testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create",
		m{"course_id": b.course, "assignment_id": moved, "body": "Ken's."})).SubmissionID
	b.do(t, b.ken, "submission.submit", m{"course_id": b.course, "submission_id": work})
	p := b.deletionPreview(t, b.sato, moved)
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": moved, "instructions_document_id": other.DocumentID})
	pinned("Ken's work", briefPinned, moved, brief)
	deleted(b.sato, moved, "an assignment pointed at another brief after its preview", &p.Counts)

	for _, d := range []uuid.UUID{brief, rubric, other.DocumentID, lecture.DocumentID} {
		if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND purged_at IS NULL AND status = 'active'`, d); n != 1 {
			t.Errorf("document %s was purged or archived", d)
		}
	}
	if n := b.Count(`SELECT count(*) FROM document_version WHERE purged_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d versions are purged", n)
	}
}

// A deletion needs somewhere to delete its files from.
func TestADeletionWithFilesNeedsAFileStore(t *testing.T) {
	b := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.Blob = nil }))
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "My essay."})).SubmissionID
	// A file recorded as an installation with a store recorded it, before
	// the store went.
	doc, version := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	b.Exec(`INSERT INTO document (id, course_id, kind, title, submission_id) VALUES ($1, $2, 'submission', 'essay.pdf', $3)`, doc, b.course, work)
	b.Exec(`WITH v AS (INSERT INTO document_version (id, document_id, seq, author_member_id) VALUES ($1, $2, 1, $3)
		RETURNING id, document_id, created_at)
		INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
		SELECT id, document_id, 1, 'essay.pdf', 'documents/x/essay', 'application/pdf', 10, created_at FROM v`, version, doc, b.yukiM)
	p := b.deletionPreview(t, b.sato, b.hw3)
	if refusal(p) != "no_file_storage" || p.Counts.Files != 1 {
		t.Fatalf("the preview: %+v", p)
	}
	out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "no-store")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.FailedPrecondition || reason(out) != "no_file_storage" {
		t.Fatalf("deleting with no file store: %+v", out)
	}
}

// A call racing a deletion, held up by its locks, is refused as one made
// afterwards would be, never with a fault: handing in a draft, grading
// work, and attaching feedback to a grade.
func TestACallRacingADeletionIsRefusedCleanly(t *testing.T) {
	for _, race := range []string{"submission.submit", "grade.submit", "document.create"} {
		t.Run(race, func(t *testing.T) {
			b := build(t)
			yukiWork, grade := b.gradeHW3(t, b.yuki, 80, false)
			notes := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course,
				"kind": "feedback", "grade_id": grade, "title": "Notes", "body_md": "Well argued."})).DocumentID
			kenDraft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create",
				m{"course_id": b.course, "assignment_id": b.hw3, "body": "Almost."})).SubmissionID
			p := b.deletionPreview(t, b.sato, b.hw3)

			// The deletion holds HW3, its work and its grades, and waits for
			// the feedback on Yuki's grade, which a new version is being
			// added to.
			release := heldBy(t, b, `SELECT 1 FROM document WHERE id = $1 FOR UPDATE`, notes)
			deleting := b.inFlight(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "racing-delete")
			b.waitingFor(t, 1, deleting)
			var racing <-chan callResult
			switch race {
			case "submission.submit":
				racing = b.inFlight(b.ken, race, m{"course_id": b.course, "submission_id": kenDraft}, "racing")
			case "grade.submit":
				racing = b.inFlight(b.sato, race, m{"course_id": b.course, "submission_id": yukiWork, "score": 85}, "racing")
			case "document.create":
				racing = b.inFlight(b.sato, race, m{"course_id": b.course, "kind": "feedback", "grade_id": grade, "title": "More notes",
					"body_md": "Good."}, "racing")
			}
			b.waitingFor(t, 2, racing)
			release()

			if out := settled(t, deleting); out.Status != domain.StatusExecuted {
				t.Fatalf("the deletion: %+v", out)
			}
			r := <-racing
			e := r.out.Error
			if r.err != nil {
				var ok bool
				if e, ok = apperr.As(r.err); !ok {
					t.Fatalf("%s racing the deletion failed with a fault: %v", race, r.err)
				}
			}
			if e == nil || !slices.Contains([]apperr.Code{apperr.NotFound, apperr.Conflict}, e.Code) {
				t.Fatalf("%s racing the deletion: %+v %v", race, r.out, r.err)
			}
			t.Logf("%s racing the deletion: %s %v (%s), recorded %v", race, e.Code, e.Details["reason"], e.Message, r.out.ActionID != nil)
			if n := b.Count(`SELECT count(*) FROM assignment WHERE id = $1`, b.hw3); n != 0 {
				t.Fatal("HW3 is still there")
			}
		})
	}
}
