package tools_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// Text versions (docs/schema.md §2.4): a version with a file of material,
// instructions or a rubric is queued to be transcribed as it is added; the
// transcription service claims it, reads its file and writes its text back;
// whoever may read the version reads the text, and whoever may write the
// document edits it or sends it back to be transcribed again.

// transcriber is the service's credential, as root issues it.
func (b *built) transcriber(t *testing.T) pipeline.Caller {
	t.Helper()
	out := testkit.Result[tools.ServiceIssueCredentialOut](t, b.do(t, b.Root, "service.issue_credential",
		m{"scope": "document_text", "label": "runtime"}))
	if out.Token == "" || !strings.HasPrefix(out.Token, "aissvc_") {
		t.Fatalf("the credential issued: %+v", out)
	}
	return pipeline.Caller{ActorID: out.ServiceActorID, CredentialID: out.CredentialID}
}

// as calls a tool as caller, and insists it executed.
func (b *built) as(t *testing.T, caller pipeline.Caller, name string, args m) pipeline.Outcome {
	t.Helper()
	out, err := b.CallWith(caller, name, args, "svc-"+uuid.NewString())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out.Status != domain.StatusExecuted {
		t.Fatalf("%s: %+v", name, out)
	}
	return out
}

func (b *built) claim(t *testing.T, svc pipeline.Caller, args m) []tools.ClaimedText {
	t.Helper()
	return testkit.Result[tools.TextQueueOut](t, b.as(t, svc, "document_text.queue", args)).Claimed
}

// slides uploads a file of material as Sato and returns the document and its
// version.
func (b *built) slides(t *testing.T, title string) (doc, version uuid.UUID) {
	t.Helper()
	token := b.upload(t, b.sato, "material", "application/pdf", []byte("%PDF "+title))
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": title, "files": oneFile(token)}))
	return made.DocumentID, *made.VersionID
}

// readText reads a document's text as actor: of the file args names, or
// else of the first file of the version document.get gives actor, asked
// for as args ask for it.
func (b *built) readText(t *testing.T, actor uuid.UUID, args m) (tools.DocumentTextOut, error) {
	t.Helper()
	args["course_id"] = b.course
	if _, named := args["file_id"]; !named {
		get := m{"course_id": b.course, "document_id": args["document_id"]}
		if v, ok := args["version_id"]; ok {
			get["version_id"] = v
		}
		out, err := b.Call(actor, "document.get", get, "")
		if err != nil {
			return tools.DocumentTextOut{}, err
		}
		if out.Status != domain.StatusExecuted {
			return tools.DocumentTextOut{}, apperr.Forbid("denied: %+v", out.Error)
		}
		// A version of no file names none, and has no text (no_text).
		args["file_id"] = uuid.Nil
		if v := testkit.Result[tools.DocumentGetOut](t, out).Version; v != nil && len(v.Files) > 0 {
			args["file_id"] = v.Files[0].ID
		}
	}
	out, err := b.Call(actor, "document.text", args, "")
	if err != nil {
		return tools.DocumentTextOut{}, err
	}
	if out.Status != domain.StatusExecuted {
		return tools.DocumentTextOut{}, apperr.Forbid("denied: %+v", out.Error)
	}
	return testkit.Result[tools.DocumentTextOut](t, out), nil
}

func (b *built) complete(t *testing.T, svc pipeline.Caller, c tools.ClaimedText, body string) pipeline.Outcome {
	t.Helper()
	out, err := b.CallWith(svc, "document_text.complete", m{"version_id": c.VersionID, "file_id": c.FileID, "lease_id": c.LeaseID,
		"status": "done", "body": body, "pages": 1, "model": "A model"}, "complete-"+c.LeaseID.String())
	if err != nil {
		t.Fatalf("document_text.complete: %v", err)
	}
	return out
}

// fileOf is the first file of a version.
func (b *built) fileOf(t *testing.T, version any) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := b.Pool.QueryRow(context.Background(), `SELECT id FROM document_version_file WHERE version_id = $1 AND position = 1`,
		version).Scan(&id); err != nil {
		t.Fatalf("the first file of %v: %v", version, err)
	}
	return id
}

// firstText is the text version of a version's first file, as a read gives
// it; nil for a version of no file.
func firstText(files []tools.FileView) *tools.TextView {
	if len(files) == 0 {
		return nil
	}
	return files[0].Text
}

func textStatus(t *testing.T, b *built, version uuid.UUID) string {
	t.Helper()
	var status string
	if err := b.Pool.QueryRow(context.Background(), `SELECT status FROM document_version_text WHERE version_id = $1`, version).Scan(&status); err != nil {
		t.Fatalf("text of %s: %v", version, err)
	}
	return status
}

// A file of material is queued as it is added, in the same call; text alone,
// and a student's file, are not.
func TestAFileIsQueuedForItsText(t *testing.T) {
	b := build(t)
	doc, v1 := b.slides(t, "Week 1")
	if got := b.get(t, b.sato, m{"document_id": doc}); got.Version == nil || firstText(got.Version.Files) == nil ||
		firstText(got.Version.Files).Status != "pending" {
		t.Fatalf("the new version's text: %+v", got.Version)
	}
	v2 := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version",
		m{"course_id": b.course, "document_id": doc, "body_md": "# Week 1, as text"})).VersionID
	token := b.upload(t, b.sato, "material", "application/pdf", []byte("%PDF again"))
	v3 := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version",
		m{"course_id": b.course, "document_id": doc, "files": oneFile(token)})).VersionID
	versions := testkit.Result[tools.DocumentVersionsOut](t, b.do(t, b.sato, "document.versions", m{"course_id": b.course, "document_id": doc})).Versions
	if len(versions) != 3 || firstText(versions[0].Files) == nil || firstText(versions[1].Files) != nil || firstText(versions[2].Files) == nil ||
		versions[0].ID != v1 || versions[1].ID != v2 || versions[2].ID != v3 || firstText(versions[2].Files).Status != "pending" {
		t.Fatalf("the versions' texts: %+v", versions)
	}
	if _, err := b.readText(t, b.sato, m{"document_id": doc, "version_id": v2}); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("the text of text alone: %v", err)
	}

	draft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "see the file"})).SubmissionID
	essay := b.upload(t, b.yuki, "submission", "application/pdf", []byte("%PDF essay"))
	file := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.yuki, "document.create",
		m{"course_id": b.course, "kind": "submission", "title": "essay.pdf", "submission_id": draft, "files": oneFile(essay)}))
	if n := b.Count(`SELECT count(*) FROM document_version_text WHERE document_id = $1`, file.DocumentID); n != 0 {
		t.Fatalf("a student's file was queued to be transcribed (%d)", n)
	}
	if n := b.Count(`SELECT count(*) FROM document_version_text WHERE NOT backfill AND status = 'pending'`); n != 2 {
		t.Fatalf("%d text versions pending, want the two files'", n)
	}
}

// Whoever may read a version reads its text, and nobody else: a student the
// published version's, a member who reads drafts any version's, and nobody
// in another course.
func TestATextIsReadAsItsVersionIs(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	doc, v1 := b.slides(t, "Week 1")
	b.complete(t, svc, b.claim(t, svc, m{})[0], "## Page 1\n\nLoops.")

	// Unpublished: the instructor reads it, the student finds nothing.
	if got, err := b.readText(t, b.sato, m{"document_id": doc}); err != nil || got.Text.Body == nil || *got.Text.Body != "## Page 1\n\nLoops." ||
		got.Text.Source == nil || *got.Text.Source != "ai" || got.Parts != 1 || got.Published {
		t.Fatalf("the instructor reading the draft's text: %+v %v", got, err)
	}
	if _, err := b.readText(t, b.yuki, m{"document_id": doc}); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a student reading an unpublished text: %v", err)
	}
	if _, err := b.readText(t, b.yuki, m{"document_id": doc, "version_id": v1}); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a student naming an unpublished version: %v", err)
	}
	// News of a draft's text is for those who read drafts.
	told := func(who uuid.UUID, typ string) int {
		n := 0
		for _, e := range feed(t, b, who) {
			if e.Type == typ && e.SubjectID != nil && *e.SubjectID == doc {
				n++
			}
		}
		return n
	}
	if told(b.sato, tools.EventDraftTextUpdated) != 1 || told(b.yuki, tools.EventDraftTextUpdated) != 0 {
		t.Fatalf("news of the draft's text: the instructor %d, the student %d", told(b.sato, tools.EventDraftTextUpdated),
			told(b.yuki, tools.EventDraftTextUpdated))
	}

	// Published: the student reads it, in document.get too.
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc})
	got, err := b.readText(t, b.yuki, m{"document_id": doc})
	if err != nil || got.VersionID != v1 || got.Text.Body == nil || !got.Published || got.Text.Model == nil || *got.Text.Model != "A model" {
		t.Fatalf("a student reading the published text: %+v %v", got, err)
	}
	if v := firstText(b.get(t, b.yuki, m{"document_id": doc}).Version.Files); v == nil || v.Body == nil || v.Status != "done" ||
		v.Pages == nil || *v.Pages != 1 {
		t.Fatalf("document.get's text for the student: %+v", v)
	}

	// A new version, transcribed and not published: hers stays the first.
	token := b.upload(t, b.sato, "material", "application/pdf", []byte("%PDF v2"))
	v2 := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version",
		m{"course_id": b.course, "document_id": doc, "files": oneFile(token)})).VersionID
	b.complete(t, svc, b.claim(t, svc, m{})[0], "## Page 1\n\nLoops, again.")
	if _, err := b.readText(t, b.yuki, m{"document_id": doc, "version_id": v2}); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a student naming the draft: %v", err)
	}
	if got, err := b.readText(t, b.yuki, m{"document_id": doc}); err != nil || got.VersionID != v1 {
		t.Fatalf("the student reads %+v %v, want the published version's", got, err)
	}
	if got, err := b.readText(t, b.sato, m{"document_id": doc}); err != nil || got.VersionID != v2 {
		t.Fatalf("the instructor reads %+v %v, want the latest", got, err)
	}

	// Archived, it is withdrawn from the student with its document.
	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": doc})
	if _, err := b.readText(t, b.yuki, m{"document_id": doc}); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a student reading an archived document's text: %v", err)
	}
	b.do(t, b.sato, "document.unarchive", m{"course_id": b.course, "document_id": doc})

	// Someone seated in another course reads nothing of this one's.
	other := b.Course("CS102")
	stranger := b.Actor("human", "Stranger")
	b.Member(other, stranger, "instructor")
	if out := b.MustCall(stranger, "document.text", m{"course_id": b.course, "document_id": doc, "file_id": b.fileOf(t, v1)}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a stranger reading the course's text: %+v", out)
	}
	if _, err := b.Call(stranger, "document.text", m{"course_id": other, "document_id": doc, "file_id": b.fileOf(t, v1)}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a stranger reading it through their own course: %v", err)
	}

	// Instructions follow their assignment: nothing until it is published.
	hw := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create",
		m{"course_id": b.course, "title": "HW9", "points_possible": 10})).ID
	examFile := b.upload(t, b.sato, "instructions", "application/pdf", []byte("%PDF exam"))
	exam := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW9", "files": oneFile(examFile)}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": exam.DocumentID})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": hw, "instructions_document_id": exam.DocumentID})
	b.complete(t, svc, b.claim(t, svc, m{})[0], "## Page 1\n\nThe exam.")
	if _, err := b.readText(t, b.yuki, m{"document_id": exam.DocumentID}); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a student reading the instructions of an unpublished assignment: %v", err)
	}
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw})
	if got, err := b.readText(t, b.yuki, m{"document_id": exam.DocumentID}); err != nil || got.Text.Body == nil {
		t.Fatalf("a student reading the published assignment's instructions: %+v %v", got, err)
	}
}

// A long text is read in parts, whole pages where they fit, at one revision.
func TestALongTextIsReadInParts(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	doc, _ := b.slides(t, "Textbook")
	var pages []string
	for i := range 40 {
		pages = append(pages, "## Page "+string(rune('A'+i%26))+"\n\n"+strings.Repeat("字", 2000)+"\n")
	}
	body := strings.Join(pages, "\n")
	b.complete(t, svc, b.claim(t, svc, m{})[0], body)

	if v := firstText(b.get(t, b.sato, m{"document_id": doc}).Version.Files); v.Body != nil || int(v.Bytes) != len(body) {
		t.Fatalf("document.get of a long text: bytes %d, body given %v", v.Bytes, v.Body != nil)
	}
	first, err := b.readText(t, b.sato, m{"document_id": doc})
	if err != nil || first.Part != 1 || first.Parts < 2 {
		t.Fatalf("the first part: %d of %d, %v", first.Part, first.Parts, err)
	}
	var whole strings.Builder
	for part := 1; part <= first.Parts; part++ {
		got, err := b.readText(t, b.sato, m{"document_id": doc, "part": part})
		if err != nil || got.Text.Revision != first.Text.Revision {
			t.Fatalf("part %d: %v", part, err)
		}
		text := *got.Text.Body
		if len(text) > tools.TextPartBytes {
			t.Fatalf("part %d is %d bytes", part, len(text))
		}
		if !strings.HasPrefix(text, "## Page ") {
			t.Fatalf("part %d does not start with a page: %.40q", part, text)
		}
		whole.WriteString(text)
	}
	if whole.String() != body {
		t.Fatal("the parts put together are not the text")
	}
	if _, err := b.readText(t, b.sato, m{"document_id": doc, "part": first.Parts + 1}); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("a part past the last: %v", err)
	}

	// Without pages, a text is cut after whole lines; a line longer than a
	// part, after whole characters.
	for name, text := range map[string]string{
		"lines":     strings.Repeat("一行文字，沒有標題。\n", 12000),
		"one line":  strings.Repeat("字", 50000),
		"odd bytes": "a" + strings.Repeat("字", 40000) + "\n" + strings.Repeat("é", 50000),
	} {
		b.do(t, b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": first.VersionID, "file_id": b.fileOf(t, first.VersionID), "body": text})
		var whole strings.Builder
		for part, parts := 1, 1; part <= parts; part++ {
			got, err := b.readText(t, b.sato, m{"document_id": doc, "part": part})
			if err != nil {
				t.Fatalf("%s, part %d: %v", name, part, err)
			}
			parts = got.Parts
			body := *got.Text.Body
			if len(body) == 0 || len(body) > tools.TextPartBytes || !utf8.ValidString(body) {
				t.Fatalf("%s, part %d of %d: %d bytes, valid %v", name, part, parts, len(body), utf8.ValidString(body))
			}
			if name == "lines" && part < parts && !strings.HasSuffix(body, "\n") {
				t.Fatalf("%s, part %d does not end with a whole line", name, part)
			}
			whole.WriteString(body)
		}
		if whole.String() != text {
			t.Fatalf("%s: the parts put together are not the text", name)
		}
	}
}

// Staff write a text as they write the document: an action, recorded,
// replayed by its key, proposed where their level says so, and told to
// whoever reads the version.
func TestStaffEditATextAsTheyEditTheDocument(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	doc, v1 := b.slides(t, "Week 1")
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc})
	b.complete(t, svc, b.claim(t, svc, m{})[0], "## Page 1\n\nLops.")
	revision := testkit.Result[tools.DocumentTextOut](t, b.do(t, b.sato, "document.text", m{"course_id": b.course, "document_id": doc,
		"file_id": b.fileOf(t, v1)})).Text.Revision

	args := m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1), "body": "## Page 1\n\nLoops.",
		"base_revision": revision}
	first := b.MustCall(b.sato, "document.text_update", args, "fix-typo")
	edited := testkit.Result[tools.DocumentTextChangeOut](t, first)
	if first.Status != domain.StatusExecuted || !edited.Changed || edited.Revision != revision+1 {
		t.Fatalf("the edit: %+v %+v", first, edited)
	}
	again := b.MustCall(b.sato, "document.text_update", args, "fix-typo")
	if !again.Replayed || *again.ActionID != *first.ActionID {
		t.Fatalf("the edit retried: %+v", again)
	}
	if _, err := b.Call(b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1),
		"body": "something else"}, "fix-typo"); !apperr.Is(err, apperr.IdempotencyConflict) {
		t.Fatalf("the key reused for another text: %v", err)
	}
	got, _ := b.readText(t, b.yuki, m{"document_id": doc})
	if *got.Text.Body != "## Page 1\n\nLoops." || *got.Text.Source != "staff" || got.Text.EditedByMemberID == nil ||
		*got.Text.EditedByMemberID != b.satoM || got.Text.EditedByName == nil || *got.Text.EditedByName != "Sato" {
		t.Fatalf("the student reads %+v", got.Text)
	}
	// An edit from a revision that is not there any more is refused.
	stale := b.MustCall(b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1),
		"body": "## Page 1\n\nWhile.", "base_revision": revision}, "stale")
	if stale.Status != domain.StatusFailed || stale.Error.Details["reason"] != "text_changed" {
		t.Fatalf("an edit of a stale revision: %+v", stale)
	}
	// The same text again changes nothing.
	same := testkit.Result[tools.DocumentTextChangeOut](t, b.do(t, b.sato, "document.text_update",
		m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1), "body": "## Page 1\n\nLoops."}))
	if same.Changed {
		t.Fatalf("the same text again: %+v", same)
	}
	// Told to whoever reads the published version, as its news is.
	told := 0
	for _, e := range feed(t, b, b.yuki) {
		if e.Type == tools.EventTextUpdated {
			told++
		}
	}
	if told != 2 { // the transcription, and Sato's edit
		t.Fatalf("the student was told of the text %d times, want 2", told)
	}

	// Neither a student nor a grader writes it.
	for _, who := range []uuid.UUID{b.yuki, b.grader} {
		if out := b.MustCall(who, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1),
			"body": "mine"}, "not-theirs-"+who.String()); out.Status != domain.StatusDenied {
			t.Fatalf("%s writing the text: %+v", who, out)
		}
	}

	// A TA whose edits wait for approval proposes one; Sato approves it, and
	// the text is the TA's.
	ta := b.Actor("human", "Ta")
	taM := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": ta,
		"preset": "ta", "perms": m{"document_write": "confirm_required"}})).MemberID
	proposed := b.MustCall(ta, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1),
		"body": "## Page 1\n\nLoops, by the TA."}, "ta-edit")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the TA's edit: %+v", proposed)
	}
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"})
	got, _ = b.readText(t, b.sato, m{"document_id": doc})
	if *got.Text.Body != "## Page 1\n\nLoops, by the TA." || *got.Text.EditedByMemberID != taM {
		t.Fatalf("the approved edit: %+v", got.Text)
	}

	// A proposal made from a revision is refused on approval once the text
	// has moved on.
	late := b.MustCall(ta, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1),
		"body": "## Page 1\n\nLate."}, "ta-late")
	b.do(t, b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1), "body": "## Page 1\n\nSato's."})
	decided := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide",
		m{"course_id": b.course, "action_id": late.ActionID, "decision": "approve"}))
	if decided.Outcome != domain.StatusFailed || decided.Error == nil || decided.Error.Details["reason"] != "text_changed" {
		t.Fatalf("a stale proposal approved: %+v", decided)
	}
}

// The text's size is bounded, and so is what a request may carry.
func TestATextIsAtMostTwoMebibytes(t *testing.T) {
	b := build(t)
	doc, v1 := b.slides(t, "Big")
	fits := strings.Repeat("a", tools.MaxTextBytes)
	b.do(t, b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1), "body": fits})
	// Refused as it is read: nothing is recorded, nor would be proposed.
	out, err := b.Call(b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1),
		"body": fits + "a"}, "too-long")
	if e, ok := apperr.As(err); !ok || e.Code != apperr.InvalidArgument || e.Details["reason"] != "text_too_long" {
		t.Fatalf("a text over the limit: %+v %v", out, err)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE idempotency_key = 'too-long'`); n != 0 {
		t.Fatal("a text over the limit is recorded")
	}
	if out, err := b.Call(b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1),
		"body": "  \n "}, "empty"); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("an empty text: %+v %v", out, err)
	}
	// The service is held to it as staff are.
	svc := b.transcriber(t)
	b.do(t, b.sato, "document.text_retranscribe", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1), "discard_edit": true})
	c := b.claim(t, svc, m{})[0]
	if out, err := b.CallWith(svc, "document_text.complete", m{"version_id": c.VersionID, "file_id": c.FileID, "lease_id": c.LeaseID,
		"status": "done", "body": fits + "a", "pages": 1, "model": "A model"}, "over"); !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("the service writing a text over the limit: %+v %v", out, err)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE idempotency_key IN ('empty', 'over')`); n != 0 {
		t.Fatalf("%d texts refused for what they say are recorded", n)
	}
}

// Staff's text is discarded only when they say so; sending a text back to the
// queue discards what it said, and a version from before text versions is
// transcribed when staff ask.
func TestRetranscribingKeepsStaffTextUnlessToldTo(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	doc, v1 := b.slides(t, "Week 1")
	b.do(t, b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1), "body": "Mine."})

	args := m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1)}
	out := b.MustCall(b.sato, "document.text_retranscribe", args, "again")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.FailedPrecondition || out.Error.Details["reason"] != "staff_edit" {
		t.Fatalf("retranscribing staff's text without discard_edit: %+v", out)
	}
	args["discard_edit"] = true
	sent := b.MustCall(b.sato, "document.text_retranscribe", args, "again-discarding")
	back := testkit.Result[tools.DocumentTextChangeOut](t, sent)
	if sent.Status != domain.StatusExecuted || !back.Changed || back.Status != "pending" {
		t.Fatalf("retranscribed: %+v", sent)
	}
	if replay := b.MustCall(b.sato, "document.text_retranscribe", args, "again-discarding"); !replay.Replayed || *replay.ActionID != *sent.ActionID {
		t.Fatalf("retranscribing, retried: %+v", replay)
	}
	got, _ := b.readText(t, b.sato, m{"document_id": doc})
	if got.Text.Status != "pending" || got.Text.Body != nil || got.Text.Source != nil || got.Parts != 0 {
		t.Fatalf("the text sent back: %+v", got)
	}
	if again := testkit.Result[tools.DocumentTextChangeOut](t, b.do(t, b.sato, "document.text_retranscribe", args)); again.Changed {
		t.Fatalf("a text waiting its turn sent back again: %+v", again)
	}
	b.complete(t, svc, b.claim(t, svc, m{})[0], "## Page 1")

	// A version from before there were text versions, which nobody queued.
	b.Exec(`ALTER TABLE document_version_file DISABLE TRIGGER document_version_file_text_queued`)
	old := uuid.New()
	b.Exec(`WITH v AS (INSERT INTO document_version (id, document_id, seq, author_member_id) VALUES ($1, $2, 9, $3)
	                   RETURNING id, document_id, created_at)
	        INSERT INTO document_version_file (version_id, document_id, position, filename, storage_key, content_type, byte_size, created_at)
	        SELECT v.id, v.document_id, 1, 'Week 1.pdf', 'courses/old', 'application/pdf', 5, v.created_at FROM v`, old, doc, b.satoM)
	b.Exec(`ALTER TABLE document_version_file ENABLE TRIGGER document_version_file_text_queued`)
	if n := b.Count(`SELECT count(*) FROM document_version_text WHERE version_id = $1`, old); n != 0 {
		t.Fatal("the old version was queued")
	}
	queued := testkit.Result[tools.DocumentTextChangeOut](t, b.do(t, b.sato, "document.text_retranscribe",
		m{"course_id": b.course, "document_id": doc, "version_id": old, "file_id": b.fileOf(t, old)}))
	if !queued.Changed || textStatus(t, b, old) != "pending" {
		t.Fatalf("asking for the old version's text: %+v", queued)
	}
	// Text alone has none to ask for.
	v2 := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version",
		m{"course_id": b.course, "document_id": doc, "body_md": "text"})).VersionID
	b.try(t, b.sato, "document.text_retranscribe", m{"course_id": b.course, "document_id": doc, "version_id": v2, "file_id": uuid.New()}, apperr.NotFound)
}

// The service claims what waits: each version to one claim, uploads before
// the backfill, and a claim that lapses is claimed again, five times at most.
func TestTheQueueGivesEachVersionToOneClaim(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	var versions []uuid.UUID
	for i := range 12 {
		_, v := b.slides(t, "Deck "+string(rune('A'+i)))
		versions = append(versions, v)
	}
	// Two of them were there before text versions: the backfill, after the
	// rest.
	b.Exec(`UPDATE document_version_text SET backfill = true WHERE version_id = ANY($1)`, versions[:2])

	var (
		mu      sync.Mutex
		claimed = map[uuid.UUID]int{}
		wg      sync.WaitGroup
	)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				out, err := b.CallWith(svc, "document_text.queue", m{"max": 3}, "")
				if err != nil || out.Status != domain.StatusExecuted {
					t.Errorf("a claim: %+v %v", out, err)
					return
				}
				got := testkit.Result[tools.TextQueueOut](t, out).Claimed
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, c := range got {
					claimed[c.VersionID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != len(versions) {
		t.Fatalf("%d versions claimed, want %d", len(claimed), len(versions))
	}
	for v, n := range claimed {
		if n != 1 {
			t.Fatalf("version %s was claimed %d times", v, n)
		}
	}

	// Uploads first, oldest first; then the backfill, newest first.
	b.Exec(`UPDATE document_version_text SET status = 'pending', lease_id = NULL, claimed_until = NULL, attempts = 0`)
	var order []uuid.UUID
	for _, c := range b.claim(t, svc, m{"max": 10}) {
		order = append(order, c.VersionID)
	}
	for _, c := range b.claim(t, svc, m{"max": 10}) {
		order = append(order, c.VersionID)
	}
	want := append(append([]uuid.UUID{}, versions[2:]...), versions[1], versions[0])
	if len(order) != len(want) {
		t.Fatalf("claimed %d, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("claim %d is %s, want %s", i, order[i], want[i])
		}
	}
}

// A claim lapses when its lease runs out: the version is claimed again, and
// the lapsed claim can write nothing back.
func TestALapsedClaimIsClaimedAgain(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	_, v := b.slides(t, "Week 1")
	first := b.claim(t, svc, m{"lease_s": 60})[0]
	if first.VersionID != v || first.Attempt != 1 || len(b.claim(t, svc, m{})) != 0 {
		t.Fatalf("the first claim: %+v", first)
	}
	got := b.download(t, first.DownloadURL)
	if string(got) != "%PDF Week 1" {
		t.Fatalf("the file claimed: %q", got)
	}
	renewed := testkit.Result[tools.TextRenewOut](t, b.as(t, svc, "document_text.renew",
		m{"version_id": v, "file_id": first.FileID, "lease_id": first.LeaseID, "lease_s": 120}))
	if !renewed.LeaseExpiresAt.After(first.LeaseExpiresAt) {
		t.Fatalf("renewed until %v, was %v", renewed.LeaseExpiresAt, first.LeaseExpiresAt)
	}

	later := time.Now().Add(3 * time.Minute)
	b.P.SetClock(func() time.Time { return later })
	defer b.P.SetClock(time.Now)
	second := b.claim(t, svc, m{})
	if len(second) != 1 || second[0].VersionID != v || second[0].Attempt != 2 || second[0].LeaseID == first.LeaseID {
		t.Fatalf("the lapsed claim claimed again: %+v", second)
	}
	if out := b.complete(t, svc, first, "## Page 1"); out.Status != domain.StatusFailed || out.Error.Details["reason"] != "lease_lost" {
		t.Fatalf("the lapsed claim writing back: %+v", out)
	}
	if _, err := b.CallWith(svc, "document_text.file", m{"version_id": v, "file_id": first.FileID, "lease_id": first.LeaseID}, ""); !apperr.Is(err, apperr.Conflict) {
		t.Fatalf("the lapsed claim reading the file: %v", err)
	}
	if out := b.complete(t, svc, second[0], "## Page 1"); out.Status != domain.StatusExecuted {
		t.Fatalf("the claim that holds writing back: %+v", out)
	}

	// Claimed and never finished five times over, it is failed.
	b.do(t, b.sato, "document.text_retranscribe", m{"course_id": b.course, "document_id": b.docOf(t, v), "version_id": v, "file_id": b.fileOf(t, v)})
	for i := range tools.MaxTextAttempts {
		at := later.Add(time.Duration(i+1) * time.Hour)
		b.P.SetClock(func() time.Time { return at })
		if c := b.claim(t, svc, m{"lease_s": 60}); len(c) != 1 || int(c[0].Attempt) != i+1 {
			t.Fatalf("claim %d: %+v", i+1, c)
		}
	}
	end := later.Add(10 * time.Hour)
	b.P.SetClock(func() time.Time { return end })
	if c := b.claim(t, svc, m{}); len(c) != 0 {
		t.Fatalf("claimed a sixth time: %+v", c)
	}
	var reason string
	if err := b.Pool.QueryRow(context.Background(), `SELECT reason FROM document_version_text WHERE version_id = $1`, v).Scan(&reason); err != nil ||
		textStatus(t, b, v) != "failed" || reason != "attempts_exhausted" {
		t.Fatalf("after five claims: %s %q %v", textStatus(t, b, v), reason, err)
	}
}

func (b *built) docOf(t *testing.T, version uuid.UUID) uuid.UUID {
	t.Helper()
	var doc uuid.UUID
	if err := b.Pool.QueryRow(context.Background(), `SELECT document_id FROM document_version WHERE id = $1`, version).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// The service writes back only while its claim holds, and never over staff's
// text; what it writes is kept as the text version, not in the action log.
func TestTheServiceWritesOnlyWhatItHolds(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	doc, v1 := b.slides(t, "Week 1")
	c := b.claim(t, svc, m{})[0]
	b.do(t, b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": v1, "file_id": b.fileOf(t, v1), "body": "Staff's."})
	if out := b.complete(t, svc, c, "## Page 1"); out.Status != domain.StatusFailed || out.Error.Details["reason"] != "edited_by_staff" {
		t.Fatalf("the service writing over staff's text: %+v", out)
	}
	if out, err := b.CallWith(svc, "document_text.renew", m{"version_id": v1, "file_id": c.FileID, "lease_id": c.LeaseID}, ""); !apperr.Is(err, apperr.Conflict) {
		t.Fatalf("renewing a claim of staff's text: %+v %v", out, err)
	}
	if got, _ := b.readText(t, b.sato, m{"document_id": doc}); *got.Text.Body != "Staff's." {
		t.Fatalf("staff's text became %q", *got.Text.Body)
	}

	_, v2 := b.slides(t, "Week 2")
	c2 := b.claim(t, svc, m{})[0]
	forged := c2
	forged.LeaseID = uuid.New()
	if out := b.complete(t, svc, forged, "## Page 1"); out.Status != domain.StatusFailed || out.Error.Details["reason"] != "lease_lost" {
		t.Fatalf("writing back without the claim: %+v", out)
	}
	if out := b.MustCall(b.sato, "document.text_retranscribe", m{"course_id": b.course, "document_id": b.docOf(t, v2), "version_id": v2, "file_id": b.fileOf(t, v2)}, "back"); out.Status != domain.StatusExecuted {
		t.Fatalf("sent back while claimed: %+v", out)
	}
	if out := b.complete(t, svc, c2, "## Page 1"); out.Status != domain.StatusFailed || out.Error.Details["reason"] != "lease_lost" {
		t.Fatalf("writing back a claim sent back to the queue: %+v", out)
	}

	c3 := b.claim(t, svc, m{})[0]
	done := b.complete(t, svc, c3, "## Page 1\n\nThe text.")
	if done.Status != domain.StatusExecuted {
		t.Fatalf("done: %+v", done)
	}
	again := b.complete(t, svc, c3, "## Page 1\n\nThe text.")
	if !again.Replayed || *again.ActionID != *done.ActionID {
		t.Fatalf("done, retried: %+v", again)
	}
	var payload string
	if err := b.Pool.QueryRow(context.Background(), `SELECT payload::text FROM action WHERE id = $1`, done.ActionID).Scan(&payload); err != nil ||
		strings.Contains(payload, "The text") || !strings.Contains(payload, c3.LeaseID.String()) {
		t.Fatalf("the action records %s (%v)", payload, err)
	}
	// Failed and skipped say why.
	_, v4 := b.slides(t, "Week 4")
	c4 := b.claim(t, svc, m{})[0]
	// Its arguments are refused as they are read, and nothing is recorded.
	out, err := b.CallWith(svc, "document_text.complete", m{"version_id": v4, "file_id": c4.FileID, "lease_id": c4.LeaseID, "status": "skipped"}, "skip-1")
	if e, ok := apperr.As(err); !ok || e.Code != apperr.InvalidArgument {
		t.Fatalf("skipped without a reason: %+v %v", out, err)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE idempotency_key = 'skip-1'`); n != 0 {
		t.Fatal("skipped without a reason is recorded")
	}
	b.as(t, svc, "document_text.complete", m{"version_id": v4, "file_id": c4.FileID, "lease_id": c4.LeaseID, "status": "skipped", "reason": "too_many_pages"})
	if got := firstText(b.get(t, b.sato, m{"document_id": b.docOf(t, v4)}).Version.Files); got.Status != "skipped" || *got.Reason != "too_many_pages" {
		t.Fatalf("skipped: %+v", got)
	}
}

// A claim that waits for work hears an upload as soon as it is committed.
func TestAClaimThatWaitsHearsAnUpload(t *testing.T) {
	b, hub := waking(t, wake.DefaultConfig)
	svc := b.transcriber(t)
	c := make(chan read, 1)
	go func() {
		raw := []byte(`{"wait_s": 10}`)
		out, err := b.P.Invoke(context.Background(), svc, "document_text.queue", raw, "")
		c <- read{out, err, time.Now()}
	}()
	waitingNow(t, hub, 1)
	notYet(t, c, 300*time.Millisecond)
	_, v := b.slides(t, "Week 1")
	uploaded := time.Now()
	r := answered(t, c, 5*time.Second)
	if latency := r.at.Sub(uploaded); latency > time.Second {
		t.Fatalf("the claim answered %v after the upload", latency)
	}
	got := resultOf[tools.TextQueueOut](t, r).Claimed
	if len(got) != 1 || got[0].VersionID != v {
		t.Fatalf("the waiting claim took %+v", got)
	}
	// An event of the course that queues nothing leaves it waiting.
	c = make(chan read, 1)
	go func() {
		out, err := b.P.Invoke(context.Background(), svc, "document_text.queue", []byte(`{"wait_s": 2}`), "")
		c <- read{out, err, time.Now()}
	}()
	waitingNow(t, hub, 1)
	b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": "material", "title": "Text", "body_md": "text"})
	notYet(t, c, 500*time.Millisecond)
	if got := resultOf[tools.TextQueueOut](t, answered(t, c, 5*time.Second)).Claimed; len(got) != 0 {
		t.Fatalf("claimed %+v", got)
	}
}

// Nothing in an archived course is claimed, and nothing is written there.
func TestAnArchivedCourseIsLeftAlone(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	_, v := b.slides(t, "Week 1")
	c := b.claim(t, svc, m{})[0]
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	if out := b.complete(t, svc, c, "## Page 1"); out.Status != domain.StatusDenied || out.Error.Details["reason"] != "course_archived" {
		t.Fatalf("writing back in an archived course: %+v", out)
	}
	later := time.Now().Add(time.Hour)
	b.P.SetClock(func() time.Time { return later })
	defer b.P.SetClock(time.Now)
	if got := b.claim(t, svc, m{}); len(got) != 0 {
		t.Fatalf("claimed in an archived course: %+v", got)
	}
	b.do(t, b.admin, "course.activate", m{"course_id": b.course})
	if got := b.claim(t, svc, m{}); len(got) != 1 || got[0].VersionID != v {
		t.Fatalf("claimed once the course is open again: %+v", got)
	}
}

// Purging a version deletes its text with its file.
func TestAPurgeTakesTheText(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	doc, v := b.slides(t, "Personal data")
	c := b.claim(t, svc, m{})[0]
	b.complete(t, svc, c, "## Page 1\n\nSomeone's address.")
	b.do(t, b.admin, "document.purge", m{"course_id": b.course, "document_id": doc, "version_id": v, "reason": "personal data"})
	if n := b.Count(`SELECT count(*) FROM document_version_text WHERE version_id = $1`, v); n != 0 {
		t.Fatal("the text of a purged version is still there")
	}
	if got := b.get(t, b.sato, m{"document_id": doc, "version_id": v}).Version; len(got.Files) != 0 {
		t.Fatalf("a purged version's files: %+v", got.Files)
	}
}
