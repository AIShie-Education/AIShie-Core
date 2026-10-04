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

// deletionCounts is confirm with every count n, naming no document to
// purge: what a caller who was shown at least as much as there is, and no
// document, sends.
func deletionCounts(n int) m {
	return m{"submissions": n, "handed_in": n, "drafts": n, "missing": n, "grades": n, "posted": n, "files": n,
		"documents": n, "proposals": n, "totals": n, "document_ids": []uuid.UUID{}}
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
// goes for good, with everything that is its; what is not its stays, what
// pointed at it says it was deleted, and the totals it counted in are worked
// out again without it.
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

	// The files that are HW3's, as the store holds them.
	var keys []string
	rows, err := b.Pool.Query(ctx, `SELECT f.storage_key FROM document_version_file f JOIN document d ON d.id = f.document_id
		WHERE d.id = $1 OR d.submission_id = $2 OR d.grade_id = $3 ORDER BY 1`, brief.DocumentID, yukiWork, yukiGrade)
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
	if len(keys) != 3 {
		t.Fatalf("HW3's files: %v", keys)
	}

	// What goes, counted, never named; Sato may.
	p := b.deletionPreview(t, b.sato, b.hw3)
	want := tools.DeletionCounts{Submissions: 3, HandedIn: 2, Drafts: 1, Grades: 1, Posted: 1, Files: 3, Documents: 1, Proposals: 1, Totals: 1,
		DocumentIDs: []uuid.UUID{brief.DocumentID}}
	if !reflect.DeepEqual(p.Counts, want) || p.Title != "HW3" || !p.Published || !p.InGrade || p.Refusal != nil ||
		len(p.Documents) != 1 || p.Documents[0].ID != brief.DocumentID || p.Documents[0].Kind != "instructions" || len(p.KeptDocuments) != 0 {
		t.Fatalf("the preview: %+v, want counts %+v", p, want)
	}

	out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "delete-hw3")
	if out.Status != domain.StatusExecuted {
		t.Fatalf("Sato deleting HW3: %+v", out)
	}
	deletion := *out.ActionID
	got := testkit.Result[tools.AssignmentDeleteOut](t, out)
	if !got.Deleted || got.AssignmentID != b.hw3 || got.Title != "HW3" ||
		got.Removed != (tools.DeletionRemoved{Submissions: 3, Grades: 1, Files: 3, Documents: 1}) ||
		got.ProposalsCancelled != 1 || got.FilesQueued != 3 || got.Snapshots != 2 {
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
	// Its files are queued to leave the store, and are there until they do.
	if n := b.Count(`SELECT count(*) FROM blob_deletion WHERE storage_key = ANY($1) AND queued_by_action_id = $2 AND attempts = 0`,
		keys, deletion); n != 3 {
		t.Fatalf("%d of its 3 files are queued", n)
	}
	for _, k := range keys {
		if _, err := b.Blob.Stat(ctx, k); err != nil {
			t.Fatalf("a queued file went before the job runner took it: %v", err)
		}
	}

	// Its brief is purged, by Sato, saying why, and an answer that relied on
	// it says only that a source was removed, to everyone; the source's row
	// stays, naming no file.
	if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND status = 'archived' AND purged_at IS NOT NULL
		AND purged_by_actor_id = $2 AND purge_reason = 'assignment_deleted'`, brief.DocumentID, b.sato); n != 1 {
		t.Fatal("the brief is not purged as a purge would leave it")
	}
	if n := b.Count(`SELECT count(*) FROM document_version WHERE document_id = $1 AND (purged_at IS NULL OR body_md IS NOT NULL)`,
		brief.DocumentID); n != 0 {
		t.Fatal("a version of the brief is not purged")
	}
	for who, reader := range map[string]uuid.UUID{"Yuki": b.yuki, "Sato": b.sato} {
		if got := b.sourcesOf(t, reader, conv, answer); len(got) != 1 || !got[0].Restricted || got[0].DocumentID != nil {
			t.Fatalf("%s reads the answer's source as %+v", who, got)
		}
	}
	if n := b.Count(`SELECT count(*) FROM conversation_message_source WHERE message_id = $1 AND file_id IS NULL`, answer); n != 1 {
		t.Fatal("the answer's source is not kept, naming no file")
	}
	// An export made now says so too.
	a := &audit{built: b}
	exportOut, _ := a.export(t, b.Root, m{"course_id": b.course})
	x := a.files(t, exportOut)
	var sources []any
	for _, msg := range list(x.byID[conv.String()]["messages"]) {
		if obj(msg)["id"] == answer.String() {
			sources = list(obj(msg)["sources"])
		}
	}
	if len(sources) != 1 || obj(sources[0])["purged"] != true || obj(sources[0])["file_id"] != nil {
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
		"its update": "assignment.update", "the brief's creation": "document.create", "Yuki's draft": "submission.create",
		"her hand-in": "submission.submit", "the post": "grade.post"} {
		if n := b.Count(`SELECT count(*) FROM action WHERE course_id = $1 AND action_type = $2 AND redacted_by_action_id = $3`,
			b.course, typ, deletion); n == 0 {
			t.Errorf("%s (%s) is not emptied", what, typ)
		}
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type LIKE 'conversation.%' AND redacted_by_action_id IS NOT NULL`); n != 0 {
		t.Fatalf("%d actions of the conversation were emptied", n)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND redacted_by_action_id IS NULL AND payload->'confirm'->>'files' = '3'
		AND result->>'title' = 'HW3'`, deletion); n != 1 {
		t.Fatal("the deletion's own action is not kept whole")
	}
	stub := testkit.Result[tools.ActionView](t, b.do(t, b.sato, "action.get", m{"course_id": b.course, "action_id": graded.ActionID}))
	if stub.Redacted == nil || stub.Redacted.ByActionID != deletion || stub.Redacted.At == nil || string(stub.Payload) != "{}" {
		t.Fatalf("action.get of an emptied action: %+v", stub)
	}
	if n := b.Count(`SELECT count(*) FROM assignment_deletion WHERE assignment_id = $1 AND title = 'HW3' AND was_published
		AND action_id = $2 AND deleted_by_actor_id = $3 AND deleted_by_member_id = $4 AND submissions = 3 AND grades = 1
		AND files = 3 AND documents = 1 AND proposals = 1 AND totals = 1`, b.hw3, deletion, b.sato, b.satoM); n != 1 {
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
				if e.AssignmentID != nil || *e.SubjectID != b.hw3 || p["title"] != "HW3" || p["submissions"] != nil ||
					len(list(p["purged_document_ids"])) != 1 || *e.ActionID != deletion {
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
	_, err = b.Call(b.sato, "assignment.get", m{"course_id": b.course, "assignment_id": b.hw3}, "")
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
	if p.Refusal != nil || !reflect.DeepEqual(p.Counts, tools.DeletionCounts{DocumentIDs: []uuid.UUID{}}) {
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
	// Without a confirmation, or one that is not counts, or that does not
	// say which documents it was shown, it is not a call.
	unnamed := deletionCounts(9)
	delete(unnamed, "document_ids")
	for _, args := range []m{{"course_id": b.course, "assignment_id": b.hw4(t)},
		{"course_id": b.course, "assignment_id": b.hw4(t), "confirm": m{"submissions": 1}},
		{"course_id": b.course, "assignment_id": b.hw4(t), "confirm": unnamed},
		{"course_id": b.course, "assignment_id": b.hw4(t), "confirm": deletionCounts(-1)}} {
		if _, err := b.Call(b.sato, "assignment.delete", args, "malformed-"+uuid.NewString()); !apperr.Is(err, apperr.InvalidArgument) {
			t.Fatalf("a deletion confirming %v: %v", args["confirm"], err)
		}
	}
}

// The deletion purges no document its caller was not shown, even where the
// counts are as they were: HW3, pointed after the preview at another brief
// its work was done under, is refused, and neither brief is purged until
// the preview is read again; a confirmation naming no document purges none.
func TestADeletionPurgesNoDocumentItDidNotShow(t *testing.T) {
	b := build(t)
	brief := func(title string) uuid.UUID {
		id := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course,
			"kind": "instructions", "title": title, "body_md": title + ", in full."})).DocumentID
		b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": id})
		return id
	}
	point := func(at uuid.UUID) {
		b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": at})
	}
	// Yuki hands HW3 in under the final exam brief, Ken under HW3's brief:
	// each is HW3's own.
	exam, hw3 := brief("Final exam brief"), brief("HW3 brief")
	point(exam)
	b.submit(t, b.yuki, "Yuki's essay")
	point(hw3)
	b.submit(t, b.ken, "Ken's essay")

	p := b.deletionPreview(t, b.sato, b.hw3)
	if len(p.Documents) != 1 || p.Documents[0].ID != hw3 || !slices.Equal(p.Counts.DocumentIDs, []uuid.UUID{hw3}) {
		t.Fatalf("the preview: %+v", p)
	}
	point(exam)
	if now := b.deletionPreview(t, b.sato, b.hw3); now.Counts.Documents != p.Counts.Documents || now.Counts.Files != p.Counts.Files ||
		!slices.Equal(now.Counts.DocumentIDs, []uuid.UUID{exam}) {
		t.Fatalf("the preview once HW3 points at the exam brief: %+v", now)
	}
	out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "shown-hw3-brief")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.Conflict || reason(out) != "confirm_stale" {
		t.Fatalf("deleting it, shown another brief than it would purge: %+v", out)
	}
	current, _ := json.Marshal(out.Error.Details["current"])
	var now tools.DeletionCounts
	if err := json.Unmarshal(current, &now); err != nil || !slices.Equal(now.DocumentIDs, []uuid.UUID{exam}) {
		t.Fatalf("the refusal says there is now %s", current)
	}
	if n := b.Count(`SELECT count(*) FROM document WHERE id = ANY($1) AND purged_at IS NOT NULL`, []uuid.UUID{exam, hw3}); n != 0 {
		t.Fatalf("refused, it purged %d briefs all the same", n)
	}
	// Read again, it goes, with the brief now shown.
	p = b.deletionPreview(t, b.sato, b.hw3)
	if out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "shown-exam-brief"); out.Status != domain.StatusExecuted {
		t.Fatalf("deleting it, shown the exam brief: %+v", out)
	}
	for d, purged := range map[uuid.UUID]bool{exam: true, hw3: false} {
		if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND purged_at IS NOT NULL`, d); (n == 1) != purged {
			t.Errorf("document %s purged: %v, want %v", d, n == 1, purged)
		}
	}
	// A confirmation naming no document, however large its counts, purges
	// none: it is refused where there is one to purge.
	hw4 := b.hw4(t)
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": hw4, "instructions_document_id": brief("HW4 brief")})
	work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": hw4, "body": "Mine."})).SubmissionID
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": work})
	if p := b.deletionPreview(t, b.sato, hw4); len(p.Counts.DocumentIDs) != 1 {
		t.Fatalf("HW4's preview: %+v", p)
	}
	for _, ids := range []any{nil, []uuid.UUID{}, []uuid.UUID{exam}} {
		confirm := deletionCounts(9)
		confirm["document_ids"] = ids
		if out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, hw4, confirm), "unnamed-"+uuid.NewString()); out.Status != domain.StatusFailed ||
			reason(out) != "confirm_stale" {
			t.Fatalf("deleting HW4 confirming documents %v: %+v", ids, out)
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

// Its instructions and rubric, which its work was handed in and graded
// under, are purged with it only when nothing else uses them: another
// assignment naming one, or work handed in under one of its versions for
// another assignment, keeps it as it is.
func TestOnlyWhatIsAnAssignmentsAloneIsPurgedWithIt(t *testing.T) {
	b := build(t)
	doc := func(kind, title string) uuid.UUID {
		id := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": kind,
			"title": title, "body_md": title + ", in full."})).DocumentID
		b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": id})
		return id
	}
	name := func(assignment, instructions, rubric uuid.UUID) {
		args := m{"course_id": b.course, "assignment_id": assignment}
		if instructions != uuid.Nil {
			args["instructions_document_id"] = instructions
		}
		if rubric != uuid.Nil {
			args["rubric_document_id"] = rubric
		}
		b.do(t, b.sato, "assignment.update", args)
	}
	shared, own, pinned, other := doc("instructions", "Essay brief"), doc("rubric", "HW3 rubric"), doc("instructions", "Old brief"),
		doc("instructions", "New brief")
	hw4 := b.hw4(t)
	name(b.hw3, shared, own)
	name(hw4, shared, uuid.Nil)
	// HW3's work is handed in under the shared brief, and graded under
	// HW3's rubric.
	b.gradeHW3(t, b.yuki, 80, false)
	// Quiz was handed in under the old brief, which then moved to HW5, and
	// work is handed in to HW5 under it too.
	quiz, hw5 := b.assignment(t, "Quiz", true, false), b.assignment(t, "HW5", true, false)
	handIn := func(student, assignment uuid.UUID) {
		work := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, student, "submission.create",
			m{"course_id": b.course, "assignment_id": assignment, "body": "Done."})).SubmissionID
		b.do(t, student, "submission.submit", m{"course_id": b.course, "submission_id": work})
	}
	name(quiz, pinned, uuid.Nil)
	handIn(b.yuki, quiz)
	name(quiz, other, uuid.Nil)
	name(hw5, pinned, uuid.Nil)
	handIn(b.ken, hw5)

	p := b.deletionPreview(t, b.sato, b.hw3)
	if len(p.Documents) != 1 || p.Documents[0].ID != own || len(p.KeptDocuments) != 1 || p.KeptDocuments[0].ID != shared || p.Counts.Documents != 1 {
		t.Fatalf("HW3's preview: purged %+v, kept %+v", p.Documents, p.KeptDocuments)
	}
	b.do(t, b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts))
	p = b.deletionPreview(t, b.sato, hw5)
	if len(p.Documents) != 0 || len(p.KeptDocuments) != 1 || p.KeptDocuments[0].ID != pinned {
		t.Fatalf("HW5's preview: purged %+v, kept %+v", p.Documents, p.KeptDocuments)
	}
	b.do(t, b.sato, "assignment.delete", deleteArgs(b, hw5, p.Counts))
	for d, purged := range map[uuid.UUID]bool{own: true, shared: false, pinned: false, other: false} {
		if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND purged_at IS NOT NULL`, d); (n == 1) != purged {
			t.Errorf("document %s purged: %v, want %v", d, n == 1, purged)
		}
	}
	// What a purged document was is named by no assignment again.
	out := b.MustCall(b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": hw4, "rubric_document_id": own}, "rename-rubric")
	if out.Status != domain.StatusFailed || reason(out) != "document_purged" {
		t.Fatalf("naming a purged rubric: %+v", out)
	}
}

// Naming a document does not make it an assignment's to purge. A brief
// another assignment has moved off, or a rubric no assignment ever named,
// pointed at by an assignment made for the purpose and nobody's work, stays
// as it is when that assignment is deleted, by an agent or by a person:
// purging is for administrators (document.purge), and a deletion purges
// only what its own work was done under.
func TestNamingADocumentDoesNotMakeItAnAssignmentsToPurge(t *testing.T) {
	b := build(t)
	doc := func(kind, title string) uuid.UUID {
		id := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": kind,
			"title": title, "body_md": title + ", in full."})).DocumentID
		b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": id})
		return id
	}
	old, current, spare := doc("instructions", "Old brief"), doc("instructions", "New brief"), doc("rubric", "Spare rubric")
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": old})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": current})
	// Neither Sato nor his assistant may purge a document.
	bot := b.agent(t, b.sato, "Sato's assistant")
	b.delegate(t, b.sato, bot, m{"perms": m{"assignment_write": "autonomous"}, "student_scope": "all"})
	for _, who := range []uuid.UUID{b.sato, bot} {
		b.try(t, who, "document.purge", m{"course_id": b.course, "document_id": old, "reason": "Not wanted."}, apperr.Forbidden)
	}

	for who, actor := range map[string]uuid.UUID{"the assistant": bot, "Sato": b.sato} {
		scratch := testkit.Result[tools.IDOut](t, b.do(t, actor, "assignment.create", m{"course_id": b.course, "title": "Scratch",
			"points_possible": 10, "instructions_document_id": old, "rubric_document_id": spare})).ID
		p := b.deletionPreview(t, actor, scratch)
		if p.Refusal != nil || len(p.Documents) != 0 || p.Counts.Documents != 0 || len(p.Counts.DocumentIDs) != 0 || len(p.KeptDocuments) != 2 {
			t.Fatalf("%s's preview of an empty assignment naming the old brief and a spare rubric: %+v", who, p)
		}
		out := b.MustCall(actor, "assignment.delete", deleteArgs(b, scratch, p.Counts), "scratch-"+who)
		if out.Status != domain.StatusExecuted || testkit.Result[tools.AssignmentDeleteOut](t, out).Removed.Documents != 0 {
			t.Fatalf("%s deleting it: %+v", who, out)
		}
	}
	for _, d := range []uuid.UUID{old, current, spare} {
		if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND purged_at IS NULL AND status = 'active'`, d); n != 1 {
			t.Errorf("document %s was purged or archived with an assignment that only named it", d)
		}
	}
	if n := b.Count(`SELECT count(*) FROM document_version WHERE purged_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d versions were purged", n)
	}
}

// Nor does work that whoever writes and grades assignments can make for one
// alone. Sato, who may not purge a document, names a spare brief and a spare
// rubric, each with a file, from an assignment made for the purpose,
// records Ken missing on it and grades the placeholder, and hands work in
// for Yuki himself and grades that too: deleting it purges neither, and
// queues none of their files. Work a student hands in does make the brief
// it was handed in under the assignment's own, by an agent of theirs as
// much as from their own seat.
func TestWorkMadeForAStudentDoesNotMakeADocumentAnAssignmentsToPurge(t *testing.T) {
	b := build(t)
	doc := func(kind, title string) uuid.UUID {
		id := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": kind,
			"title": title, "body_md": title + ", in full.", "files": b.fileOn(t, b.sato, kind, kind+".pdf")})).DocumentID
		b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": id})
		return id
	}
	brief, rubric := doc("instructions", "Spare brief"), doc("rubric", "Spare rubric")
	for _, d := range []uuid.UUID{brief, rubric} {
		b.try(t, b.sato, "document.purge", m{"course_id": b.course, "document_id": d, "reason": "Not wanted."}, apperr.Forbidden)
	}
	scratch := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "Scratch",
		"points_possible": 10, "instructions_document_id": brief, "rubric_document_id": rubric})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": scratch})

	// Ken recorded missing, the placeholder graded, a draft never posted.
	missing := testkit.Result[tools.SubmissionIDOut](t, b.do(t, b.sato, "submission.record_missing",
		m{"course_id": b.course, "assignment_id": scratch, "student_member_id": b.kenM})).SubmissionID
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": missing, "score": 0})
	// Work handed in for Yuki, by Sato, under the spare brief, and graded
	// under the spare rubric.
	forYuki := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.sato, "submission.create",
		m{"course_id": b.course, "assignment_id": scratch, "student_member_id": b.yukiM, "body": "Typed in for her."})).SubmissionID
	b.do(t, b.sato, "submission.submit", m{"course_id": b.course, "submission_id": forYuki})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": forYuki, "score": 5})
	if n := b.Count(`SELECT count(*) FROM submission s JOIN document_version v ON v.id = s.instructions_version_id
		WHERE s.id = $1 AND v.document_id = $2`, forYuki, brief); n != 1 {
		t.Fatal("the work handed in for Yuki is not pinned to the spare brief: the test shows nothing")
	}
	if n := b.Count(`SELECT count(*) FROM grade g JOIN document_version v ON v.id = g.rubric_version_id
		WHERE g.submission_id = ANY($1) AND v.document_id = $2`, []uuid.UUID{missing, forYuki}, rubric); n != 2 {
		t.Fatalf("%d of the two grades are pinned to the spare rubric: the test shows nothing", n)
	}

	p := b.deletionPreview(t, b.sato, scratch)
	if p.Refusal != nil || len(p.Documents) != 0 || p.Counts.Documents != 0 || len(p.Counts.DocumentIDs) != 0 || len(p.KeptDocuments) != 2 ||
		p.Counts.HandedIn != 1 || p.Counts.Missing != 1 || p.Counts.Grades != 2 || p.Counts.Files != 0 {
		t.Fatalf("the preview of an assignment with only work Sato made for it: %+v", p)
	}
	out := b.MustCall(b.sato, "assignment.delete", deleteArgs(b, scratch, p.Counts), "scratch")
	if out.Status != domain.StatusExecuted || testkit.Result[tools.AssignmentDeleteOut](t, out).Removed.Documents != 0 {
		t.Fatalf("deleting it: %+v", out)
	}
	for _, d := range []uuid.UUID{brief, rubric} {
		if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND purged_at IS NULL AND status = 'active'`, d); n != 1 {
			t.Errorf("document %s was purged or archived with an assignment only Sato's own work was done for", d)
		}
	}
	if n := b.Count(`SELECT count(*) FROM blob_deletion`); n != 0 {
		t.Fatalf("%d files were queued to leave the store", n)
	}

	// Yuki's own agent hands work in to a quiz, under its brief, by
	// proposals Sato approves: the brief is the quiz's own.
	bot := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, bot, m{})
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"submission_write": "confirm_required"}})
	quizBrief := doc("instructions", "Quiz brief")
	quiz := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "Quiz",
		"points_possible": 10, "instructions_document_id": quizBrief})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": quiz})
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
	var draft tools.SubmissionCreateOut
	if err := json.Unmarshal(approve(b.MustCall(bot, "submission.create", m{"course_id": b.course, "assignment_id": quiz,
		"student_member_id": b.yukiM, "body": "My answers."}, "bot-draft")), &draft); err != nil {
		t.Fatal(err)
	}
	approve(b.MustCall(bot, "submission.submit", m{"course_id": b.course, "submission_id": draft.SubmissionID}, "bot-submit"))
	p = b.deletionPreview(t, b.sato, quiz)
	if len(p.Documents) != 1 || p.Documents[0].ID != quizBrief || len(p.KeptDocuments) != 0 || p.Counts.HandedIn != 1 {
		t.Fatalf("the preview of the quiz Yuki's agent handed work in to: %+v", p)
	}
	b.do(t, b.sato, "assignment.delete", deleteArgs(b, quiz, p.Counts))
	if n := b.Count(`SELECT count(*) FROM document WHERE id = $1 AND purged_at IS NOT NULL AND purged_by_actor_id = $2`, quizBrief, b.sato); n != 1 {
		t.Fatal("the quiz's brief, which Yuki's agent handed her work in under, was not purged with it")
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
			brief := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course,
				"kind": "instructions", "title": "HW3 brief", "body_md": "Write."})).DocumentID
			b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": brief})
			b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": brief})
			yukiWork, grade := b.gradeHW3(t, b.yuki, 80, false)
			kenDraft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create",
				m{"course_id": b.course, "assignment_id": b.hw3, "body": "Almost."})).SubmissionID
			p := b.deletionPreview(t, b.sato, b.hw3)

			// The deletion holds HW3, its work and its grades, and waits for
			// the brief, which a new version is being added to.
			release := heldBy(t, b, `SELECT 1 FROM document WHERE id = $1 FOR UPDATE`, brief)
			deleting := b.inFlight(b.sato, "assignment.delete", deleteArgs(b, b.hw3, p.Counts), "racing-delete")
			b.waitingFor(t, 1, deleting)
			var racing <-chan callResult
			switch race {
			case "submission.submit":
				racing = b.inFlight(b.ken, race, m{"course_id": b.course, "submission_id": kenDraft}, "racing")
			case "grade.submit":
				racing = b.inFlight(b.sato, race, m{"course_id": b.course, "submission_id": yukiWork, "score": 85}, "racing")
			case "document.create":
				racing = b.inFlight(b.sato, race, m{"course_id": b.course, "kind": "feedback", "grade_id": grade, "title": "Notes",
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
