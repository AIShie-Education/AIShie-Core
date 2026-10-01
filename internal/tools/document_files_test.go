package tools_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/blob"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Files of a version (docs/schema.md §2.4): a version holds several files,
// each uploaded first and named, in order, in the call that writes the
// version; whoever may read the version downloads each under its name; each
// file of material, instructions or a rubric has a text version of its own.

// uploadNamed is upload with the file's name given to document.upload_url.
func (b *built) uploadNamed(t *testing.T, actor uuid.UUID, kind, contentType, filename string, body []byte) string {
	t.Helper()
	out := testkit.Result[tools.UploadURLOut](t, b.do(t, actor, "document.upload_url",
		m{"course_id": b.course, "kind": kind, "content_type": contentType, "filename": filename}))
	b.put(t, out, body)
	return out.UploadToken
}

// week3 is a file of Sato's lecture: its bytes, its type and its name.
type week3 struct {
	body        []byte
	contentType string
	filename    string
}

var week3Files = []week3{
	{[]byte("%PDF week 3 slides"), "application/pdf", "week3-slides.pdf"},
	{[]byte("PK the handout"), "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "handout.docx"},
	{[]byte("for i in range(3):\n    print(i)\n"), "text/x-python", "loops.py"},
}

// lecture is Sato's material of three files and text, published: the
// handout named when it was uploaded, the others as they are attached.
func (b *built) lecture(t *testing.T) tools.DocumentCreateOut {
	t.Helper()
	files := make([]m, len(week3Files))
	for i, f := range week3Files {
		if i == 1 {
			files[i] = m{"upload_token": b.uploadNamed(t, b.sato, "material", f.contentType, f.filename, f.body)}
		} else {
			files[i] = m{"upload_token": b.upload(t, b.sato, "material", f.contentType, f.body), "filename": f.filename}
		}
	}
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course,
		"kind": "material", "title": "Week 3: Loops", "body_md": "Read the slides, then run the program.", "files": files}))
	if made.VersionID == nil || len(made.FileIDs) != len(week3Files) {
		t.Fatalf("the lecture: %+v", made)
	}
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": made.DocumentID})
	return made
}

// A material of three files and text is created, read and downloaded file
// by file, by a student, in the order and under the names it was given; the
// fields the release before read say its first file.
func TestAVersionHoldsSeveralFiles(t *testing.T) {
	b := build(t)
	made := b.lecture(t)

	got := b.get(t, b.yuki, m{"document_id": made.DocumentID}).Version
	if got == nil || got.BodyMD == nil || *got.BodyMD != "Read the slides, then run the program." || len(got.Files) != 3 {
		t.Fatalf("Yuki reads %+v", got)
	}
	for i, f := range got.Files {
		want := week3Files[i]
		if f.ID != made.FileIDs[i] || f.Position != int32(i+1) || f.Filename != want.filename || f.ContentType != want.contentType ||
			f.ByteSize != int64(len(want.body)) || f.Checksum == nil || !strings.HasPrefix(*f.Checksum, "sha256:") || f.DownloadURL == nil {
			t.Fatalf("file %d: %+v", i+1, f)
		}
		if body, name := b.fetch(t, *f.DownloadURL); !bytes.Equal(body, want.body) || name != want.filename {
			t.Fatalf("file %d downloads as %q: %q", i+1, name, body)
		}
		if f.Text == nil || f.Text.Status != "pending" || f.Text.Body != nil {
			t.Fatalf("file %d's text version: %+v", i+1, f.Text)
		}
	}
	// What the release before read is the first file's.
	first := got.Files[0]
	if got.DownloadURL == nil || *got.DownloadURL != *first.DownloadURL || *got.ContentType != first.ContentType ||
		*got.ByteSize != first.ByteSize || *got.Checksum != *first.Checksum || got.Text == nil || got.Text.Status != "pending" {
		t.Fatalf("the version's own file fields: %+v", got)
	}
	if n := b.Count(`SELECT count(*) FROM document_version_text WHERE version_id = $1 AND status = 'pending'`, *made.VersionID); n != 3 {
		t.Fatalf("%d files queued for their text, want 3", n)
	}
	if n := b.Count(`SELECT count(*) FROM document_version_file WHERE version_id = $1 AND storage_key LIKE 'documents/' || $2 || '/%'`,
		*made.VersionID, b.course.String()); n != 3 {
		t.Fatal("the files are not kept under documents/, in the course")
	}

	// One file at a time, by its id, for whoever reads the version.
	for i, id := range made.FileIDs {
		out := testkit.Result[tools.DocumentFileOut](t, b.do(t, b.yuki, "document.file", m{"course_id": b.course,
			"document_id": made.DocumentID, "file_id": id}))
		if out.ID != id || out.VersionID != *made.VersionID || out.Seq != 1 || !out.Published || out.Filename != week3Files[i].filename ||
			out.Text == nil || out.Text.Status != "pending" || out.ExpiresAt.IsZero() {
			t.Fatalf("document.file %d: %+v", i+1, out)
		}
		if body, name := b.fetch(t, out.DownloadURL); !bytes.Equal(body, week3Files[i].body) || name != week3Files[i].filename {
			t.Fatalf("document.file %d downloads as %q: %q", i+1, name, body)
		}
	}
	b.refusedAs(t, b.yuki, "document.file", m{"course_id": b.course, "document_id": made.DocumentID, "file_id": uuid.New()},
		apperr.NotFound, "")

	// A new version with two files is a draft: Yuki reads the first still,
	// and finds none of the new files; Sato reads them.
	slides2, notes := []byte("%PDF week 3 slides, corrected"), []byte("Bring a laptop.")
	v2 := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version", m{"course_id": b.course,
		"document_id": made.DocumentID, "files": []m{
			{"upload_token": b.upload(t, b.sato, "material", "application/pdf", slides2), "filename": "week3-slides.pdf"},
			{"upload_token": b.uploadNamed(t, b.sato, "material", "text/plain", "notes.txt", notes)},
		}}))
	if v2.Seq != 2 || v2.Published || len(v2.FileIDs) != 2 {
		t.Fatalf("the second version: %+v", v2)
	}
	if got := b.get(t, b.yuki, m{"document_id": made.DocumentID}).Version; got.ID != *made.VersionID || len(got.Files) != 3 {
		t.Fatalf("Yuki reads the draft: %+v", got)
	}
	for _, id := range v2.FileIDs {
		b.refusedAs(t, b.yuki, "document.file", m{"course_id": b.course, "document_id": made.DocumentID, "file_id": id}, apperr.NotFound, "")
	}
	draft := b.get(t, b.sato, m{"document_id": made.DocumentID}).Version
	if draft.ID != v2.VersionID || len(draft.Files) != 2 || draft.Files[1].Filename != "notes.txt" || draft.BodyMD != nil {
		t.Fatalf("Sato reads %+v", draft)
	}
	if body, _ := b.fetch(t, *draft.Files[1].DownloadURL); !bytes.Equal(body, notes) {
		t.Fatalf("the notes: %q", body)
	}
	versions := testkit.Result[tools.DocumentVersionsOut](t, b.do(t, b.sato, "document.versions",
		m{"course_id": b.course, "document_id": made.DocumentID})).Versions
	if len(versions) != 2 || len(versions[0].Files) != 3 || len(versions[1].Files) != 2 || !versions[0].HasFile ||
		versions[1].Files[0].ID != v2.FileIDs[0] || versions[1].Files[1].Text == nil || versions[1].Files[1].Text.Status != "pending" ||
		versions[0].Files[0].DownloadURL != nil || *versions[1].ContentType != "application/pdf" || versions[1].Text == nil {
		t.Fatalf("the versions: %+v", versions)
	}
	// Published, it is Yuki's to read.
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": made.DocumentID})
	b.do(t, b.yuki, "document.file", m{"course_id": b.course, "document_id": made.DocumentID, "file_id": v2.FileIDs[1]})
	// Its news says how many files it holds.
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'document.version_added' AND subject_id = $1 AND (payload->>'files')::int = 2`,
		made.DocumentID); n != 1 {
		t.Fatal("the new version's news does not say it holds two files")
	}
}

// What a version is given is held to what one may hold, and a call refused
// records no file.
func TestAVersionsFilesAreHeldToItsLimits(t *testing.T) {
	b := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) {
		d.Documents = tools.DocumentLimits{FilesPerVersion: 2, VersionBytes: 100}
	}))
	u := testkit.Result[tools.UploadURLOut](t, b.do(t, b.sato, "document.upload_url",
		m{"course_id": b.course, "kind": "material", "content_type": "application/pdf"}))
	if u.MaxFiles != 2 || u.MaxVersionBytes != 100 || u.MaxBytes != testkit.MaxUploadBytes {
		t.Fatalf("the limits the upload URL says: %+v", u)
	}
	create := func(content m) m {
		content["course_id"], content["kind"], content["title"] = b.course, "material", "Week 4"
		return content
	}
	small := func() string { return b.upload(t, b.sato, "material", "application/pdf", []byte("%PDF small")) }
	sixty := func() string {
		return b.upload(t, b.sato, "material", "application/pdf", bytes.Repeat([]byte("x"), 60))
	}

	b.refusedAs(t, b.sato, "document.create", create(m{"files": []m{
		{"upload_token": small(), "filename": "a.pdf"}, {"upload_token": small(), "filename": "b.pdf"}, {"upload_token": small(), "filename": "c.pdf"},
	}}), apperr.InvalidArgument, "too_many_files")
	b.refusedAs(t, b.sato, "document.create", create(m{"files": []m{
		{"upload_token": sixty(), "filename": "a.pdf"}, {"upload_token": sixty(), "filename": "b.pdf"},
	}}), apperr.FailedPrecondition, "version_too_large")
	one := small()
	b.refusedAs(t, b.sato, "document.create", create(m{"upload_token": one, "files": []m{{"upload_token": small(), "filename": "a.pdf"}}}),
		apperr.InvalidArgument, "files_and_upload_token")
	b.refusedAs(t, b.sato, "document.create", create(m{"files": []m{{"upload_token": one, "filename": "a.pdf"}, {"upload_token": one, "filename": "b.pdf"}}}),
		apperr.InvalidArgument, "duplicate_file")
	b.refusedAs(t, b.sato, "document.create", create(m{"files": []m{{"upload_token": one}}}), apperr.InvalidArgument, "filename_required")
	b.refusedAs(t, b.sato, "document.create", create(m{"files": []m{{"upload_token": one, "filename": "../../etc/passwd"}}}),
		apperr.InvalidArgument, "bad_filename")
	b.refusedAs(t, b.sato, "document.upload_url", m{"course_id": b.course, "kind": "material", "content_type": "application/pdf",
		"filename": "a\nb.pdf"}, apperr.InvalidArgument, "bad_filename")
	b.refusedAs(t, b.sato, "document.create", create(m{"files": []m{{"upload_token": "not a token", "filename": "a.pdf"}}}),
		apperr.InvalidArgument, "bad_upload_token")
	// A file larger than a file may be is refused as ever.
	big := b.upload(t, b.sato, "material", "application/pdf", bytes.Repeat([]byte("x"), testkit.MaxUploadBytes+1))
	b.refusedAs(t, b.sato, "document.create", create(m{"files": []m{{"upload_token": big, "filename": "big.pdf"}}}),
		apperr.FailedPrecondition, "file_too_large")
	// So is a new version, as a document is created.
	doc := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", create(m{"body_md": "Week 4"}))).DocumentID
	b.refusedAs(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": doc, "files": []m{
		{"upload_token": sixty(), "filename": "a.pdf"}, {"upload_token": sixty(), "filename": "b.pdf"},
	}}, apperr.FailedPrecondition, "version_too_large")
	if n := b.Count(`SELECT count(*) FROM document_version_file`); n != 0 {
		t.Fatalf("%d files recorded by calls that were refused", n)
	}

	// Within them, it is written.
	made := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": doc,
		"files": []m{{"upload_token": sixty(), "filename": "a.pdf"}, {"upload_token": one, "filename": "b.pdf"}}}))
	if len(made.FileIDs) != 2 {
		t.Fatalf("within the limits: %+v", made)
	}
}

// A proposal keeps its files by their tokens, attaches them, named as they
// were given, when it is approved, and is not made about an upload it could
// outlive.
func TestAProposedVersionKeepsItsFilesUntilItIsDecided(t *testing.T) {
	b := build(t)
	editor := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "editor"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": editor, "preset": "ta", "perms": m{"document_write": "confirm_required"}})
	doc := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Week 5", "body_md": "v1"})).DocumentID

	old := b.upload(t, editor, "material", "application/pdf", []byte("%PDF long ago"))
	b.P.SetClock(func() time.Time { return time.Now().Add(tools.OrphanGrace + time.Hour) })
	fresh := b.upload(t, editor, "material", "application/pdf", []byte("%PDF new"))
	out := b.MustCall(editor, "document.add_version", m{"course_id": b.course, "document_id": doc,
		"files": []m{{"upload_token": fresh, "filename": "new.pdf"}, {"upload_token": old, "filename": "old.pdf"}}}, "old")
	b.P.SetClock(time.Now)
	if out.Status != domain.StatusFailed || reason(out) != "upload_too_old" {
		t.Fatalf("a proposal naming an old upload: %+v", out)
	}
	// Nor about files no version could hold.
	out = b.MustCall(editor, "document.add_version", m{"course_id": b.course, "document_id": doc,
		"files": []m{{"upload_token": fresh}}}, "unnamed")
	if out.Status != domain.StatusFailed || reason(out) != "filename_required" {
		t.Fatalf("a proposal naming a file with no name: %+v", out)
	}

	slides, program := []byte("%PDF week 5"), []byte("print('hello')\n")
	proposed := b.MustCall(editor, "document.add_version", m{"course_id": b.course, "document_id": doc, "files": []m{
		{"upload_token": b.upload(t, editor, "material", "application/pdf", slides), "filename": "week5.pdf"},
		{"upload_token": b.uploadNamed(t, editor, "material", "text/x-python", "hello.py", program)},
	}}, "fresh")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the editor's version: %+v", proposed)
	}
	if n := b.Count(`SELECT count(*) FROM document_version_file WHERE document_id = $1`, doc); n != 0 {
		t.Fatal("a proposed version's files were recorded before anyone decided")
	}
	decided := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide",
		m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}))
	if decided.Outcome != domain.StatusExecuted {
		t.Fatalf("approval: %+v", decided)
	}
	got := b.get(t, b.sato, m{"document_id": doc}).Version
	if got.Seq != 2 || len(got.Files) != 2 || got.Files[0].Filename != "week5.pdf" || got.Files[1].Filename != "hello.py" {
		t.Fatalf("the approved version: %+v", got)
	}
	if body, _ := b.fetch(t, *got.Files[1].DownloadURL); !bytes.Equal(body, program) {
		t.Fatalf("the program: %q", body)
	}
}

// Each file of a version has a text version of its own: the service claims
// them one file at a time, in order, and writes each back; readers read each
// by its file; staff write one by its file, and are asked which when a
// version has several.
func TestEachFileHasATextVersionOfItsOwn(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	made := b.lecture(t)
	version := *made.VersionID

	claimed := b.claim(t, svc, m{"max": 5})
	if len(claimed) != 3 {
		t.Fatalf("claimed %d, want the lecture's three files", len(claimed))
	}
	for i, c := range claimed {
		if c.VersionID != version || c.FileID != made.FileIDs[i] || c.Position != int32(i+1) || c.Filename != week3Files[i].filename ||
			c.ContentType != week3Files[i].contentType || c.ByteSize != int64(len(week3Files[i].body)) {
			t.Fatalf("claim %d: %+v", i+1, c)
		}
		if body, _ := b.fetch(t, c.DownloadURL); !bytes.Equal(body, week3Files[i].body) {
			t.Fatalf("claim %d's file: %q", i+1, body)
		}
	}
	// The file of a claim, again, by its file or by its lease alone.
	again := testkit.Result[tools.TextFileOut](t, b.as(t, svc, "document_text.file", m{"version_id": version, "lease_id": claimed[1].LeaseID,
		"file_id": claimed[1].FileID}))
	if again.FileID != claimed[1].FileID || again.Filename != "handout.docx" {
		t.Fatalf("document_text.file: %+v", again)
	}
	if byLease := testkit.Result[tools.TextFileOut](t, b.as(t, svc, "document_text.file", m{"version_id": version,
		"lease_id": claimed[2].LeaseID})); byLease.FileID != claimed[2].FileID {
		t.Fatalf("document_text.file by its lease: %+v", byLease)
	}
	// Another file's lease is not this file's.
	if _, err := b.CallWith(svc, "document_text.file", m{"version_id": version, "lease_id": claimed[0].LeaseID,
		"file_id": claimed[1].FileID}, ""); !apperr.Is(err, apperr.Conflict) {
		t.Fatalf("a file with another's lease: %v", err)
	}
	b.as(t, svc, "document_text.renew", m{"version_id": version, "file_id": claimed[0].FileID, "lease_id": claimed[0].LeaseID})

	// Each written back: the first by its file, the second by its lease
	// alone, the third failed.
	bodies := []string{"## Slide 1\n\nLoops.", "## Page 1\n\nThe handout."}
	for i, body := range bodies {
		args := m{"version_id": version, "lease_id": claimed[i].LeaseID, "status": "done", "body": body, "pages": 1, "model": "A model"}
		if i == 0 {
			args["file_id"] = claimed[i].FileID
		}
		out := testkit.Result[tools.TextCompleteOut](t, b.as(t, svc, "document_text.complete", args))
		if out.FileID != claimed[i].FileID || out.Status != "done" {
			t.Fatalf("complete %d: %+v", i+1, out)
		}
	}
	b.as(t, svc, "document_text.complete", m{"version_id": version, "file_id": claimed[2].FileID, "lease_id": claimed[2].LeaseID,
		"status": "skipped", "reason": "unsupported_format"})

	// Readers read each by its file; the first when none is named.
	for i, body := range bodies {
		got, err := b.readText(t, b.yuki, m{"document_id": made.DocumentID, "file_id": made.FileIDs[i]})
		if err != nil || got.FileID != made.FileIDs[i] || got.Position != int32(i+1) || got.Filename != week3Files[i].filename ||
			got.Text.Body == nil || *got.Text.Body != body || got.VersionID != version {
			t.Fatalf("the text of file %d: %+v, %v", i+1, got, err)
		}
	}
	if got, err := b.readText(t, b.yuki, m{"document_id": made.DocumentID}); err != nil || got.FileID != made.FileIDs[0] {
		t.Fatalf("the text with no file named: %+v, %v", got, err)
	}
	if got, err := b.readText(t, b.yuki, m{"document_id": made.DocumentID, "file_id": made.FileIDs[2]}); err != nil ||
		got.Text.Status != "skipped" || got.Parts != 0 {
		t.Fatalf("the skipped file's text: %+v, %v", got, err)
	}
	view := b.get(t, b.yuki, m{"document_id": made.DocumentID}).Version
	if *view.Files[0].Text.Body != bodies[0] || *view.Files[1].Text.Body != bodies[1] || view.Files[2].Text.Status != "skipped" ||
		*view.Text.Body != bodies[0] {
		t.Fatalf("document.get's texts: %+v", view)
	}
	// Each file's news names it.
	for i := range bodies {
		if n := b.Count(`SELECT count(*) FROM event WHERE type = 'document.text_updated' AND subject_id = $1
		                 AND payload->>'file_id' = $2 AND payload->>'source' = 'ai'`, made.DocumentID, made.FileIDs[i].String()); n != 1 {
			t.Fatalf("file %d's text is not news", i+1)
		}
	}

	// Staff write one file's text by its file; a version of several asks which.
	b.refusedAs(t, b.sato, "document.text_update", m{"course_id": b.course, "document_id": made.DocumentID, "version_id": version,
		"body": "## Slide 1\n\nLoops, for and while."}, apperr.InvalidArgument, "file_id_required")
	b.refusedAs(t, b.sato, "document.text_retranscribe", m{"course_id": b.course, "document_id": made.DocumentID, "version_id": version},
		apperr.InvalidArgument, "file_id_required")
	edited := testkit.Result[tools.DocumentTextChangeOut](t, b.do(t, b.sato, "document.text_update", m{"course_id": b.course,
		"document_id": made.DocumentID, "version_id": version, "file_id": made.FileIDs[0], "body": "## Slide 1\n\nLoops, for and while."}))
	if !edited.Changed || edited.FileID != made.FileIDs[0] {
		t.Fatalf("the edit: %+v", edited)
	}
	sent := testkit.Result[tools.DocumentTextChangeOut](t, b.do(t, b.sato, "document.text_retranscribe", m{"course_id": b.course,
		"document_id": made.DocumentID, "version_id": version, "file_id": made.FileIDs[2]}))
	if !sent.Changed || sent.FileID != made.FileIDs[2] || sent.Status != "pending" {
		t.Fatalf("sent back: %+v", sent)
	}
	texts := map[uuid.UUID]string{}
	rows, err := b.Pool.Query(t.Context(), `SELECT file_id, status || coalesce(':' || source, '') FROM document_version_text WHERE version_id = $1`, version)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var s string
		if err := rows.Scan(&id, &s); err != nil {
			t.Fatal(err)
		}
		texts[id] = s
	}
	rows.Close()
	if texts[made.FileIDs[0]] != "done:staff" || texts[made.FileIDs[1]] != "done:ai" || texts[made.FileIDs[2]] != "pending" {
		t.Fatalf("each file's text: %v", texts)
	}
	// A file of another version is not this version's.
	b.refusedAs(t, b.sato, "document.text_update", m{"course_id": b.course, "document_id": made.DocumentID, "version_id": version,
		"file_id": uuid.New(), "body": "x"}, apperr.NotFound, "")

	// Sent back, the file is claimed again, alone, and its completion
	// without a file named finds it by its lease.
	again2 := b.claim(t, svc, m{"max": 5})
	if len(again2) != 1 || again2[0].FileID != made.FileIDs[2] {
		t.Fatalf("claimed again: %+v", again2)
	}
	b.as(t, svc, "document_text.complete", m{"version_id": version, "lease_id": again2[0].LeaseID, "status": "done",
		"body": "## loops.py\n\n```python\nfor i in range(3):\n```", "pages": 1, "model": "A model"})
	// A lease that no longer holds, with no file named, is lost.
	if _, err := b.CallWith(svc, "document_text.renew", m{"version_id": version, "lease_id": again2[0].LeaseID}, ""); !apperr.Is(err, apperr.Conflict) {
		t.Fatalf("renewing a finished claim: %v", err)
	}
	// A proposal of an edit names the file it is about.
	editor := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "editor"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": editor, "preset": "ta", "perms": m{"document_write": "confirm_required"}})
	proposed := b.MustCall(editor, "document.text_update", m{"course_id": b.course, "document_id": made.DocumentID, "version_id": version,
		"file_id": made.FileIDs[1], "body": "## Page 1\n\nThe handout, read."}, "edit")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the proposed edit: %+v", proposed)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND payload->>'file_id' = $2 AND payload->>'base_revision' IS NOT NULL`,
		*proposed.ActionID, made.FileIDs[1].String()); n != 1 {
		t.Fatal("the proposal does not name the file and the revision it was made about")
	}
}

// The calls of before a version held several files work as they did: one
// upload_token is one file, named as it was uploaded or after its document;
// a version of one file needs no file named, by staff or by the service.
func TestTheCallsOfOneFileWorkAsBefore(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	pdf := []byte("%PDF week 1")
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": "material",
		"title": "Week 1: Variables", "upload_token": b.upload(t, b.sato, "material", "application/pdf", pdf)}))
	if len(made.FileIDs) != 1 {
		t.Fatalf("made: %+v", made)
	}
	got := b.get(t, b.sato, m{"document_id": made.DocumentID}).Version
	if len(got.Files) != 1 || got.Files[0].Filename != "Week 1: Variables.pdf" || got.DownloadURL == nil || *got.ContentType != "application/pdf" {
		t.Fatalf("the one file: %+v", got)
	}
	if body, name := b.fetch(t, *got.DownloadURL); !bytes.Equal(body, pdf) || name != "Week 1: Variables.pdf" {
		t.Fatalf("the version's download_url gives %q: %q", name, body)
	}
	// Named when it was uploaded, it keeps that name.
	named := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version", m{"course_id": b.course,
		"document_id": made.DocumentID, "upload_token": b.uploadNamed(t, b.sato, "material", "application/pdf", "variables.pdf", pdf)}))
	if f := b.get(t, b.sato, m{"document_id": made.DocumentID}).Version.Files; len(f) != 1 || f[0].ID != named.FileIDs[0] || f[0].Filename != "variables.pdf" {
		t.Fatalf("named at upload: %+v", f)
	}

	c := b.claim(t, svc, m{"max": 1})[0]
	if c.VersionID != *made.VersionID || c.FileID != made.FileIDs[0] {
		t.Fatalf("the claim: %+v", c)
	}
	b.as(t, svc, "document_text.file", m{"version_id": c.VersionID, "lease_id": c.LeaseID})
	b.as(t, svc, "document_text.renew", m{"version_id": c.VersionID, "lease_id": c.LeaseID})
	if out := b.complete(t, svc, c, "## Page 1"); out.Status != domain.StatusExecuted {
		t.Fatalf("complete with no file named: %+v", out)
	}
	args := m{"course_id": b.course, "document_id": made.DocumentID, "version_id": made.VersionID}
	if got, err := b.readText(t, b.sato, m{"document_id": made.DocumentID, "version_id": *made.VersionID}); err != nil || *got.Text.Body != "## Page 1" {
		t.Fatalf("read with no file named: %+v, %v", got, err)
	}
	args["body"] = "## Page 1\n\nCorrected."
	if out := testkit.Result[tools.DocumentTextChangeOut](t, b.do(t, b.sato, "document.text_update", args)); !out.Changed || out.FileID != made.FileIDs[0] {
		t.Fatalf("an edit with no file named: %+v", out)
	}
	delete(args, "body")
	args["discard_edit"] = true
	if out := testkit.Result[tools.DocumentTextChangeOut](t, b.do(t, b.sato, "document.text_retranscribe", args)); !out.Changed {
		t.Fatalf("sent back with no file named: %+v", out)
	}
}

// A version of several files purged loses every one of them, from the store
// and the record, and their texts; what it was stays a tombstone.
func TestAPurgedVersionLosesAllItsFiles(t *testing.T) {
	b := build(t)
	made := b.lecture(t)
	var keys []string
	rows, err := b.Pool.Query(t.Context(), `SELECT storage_key FROM document_version_file WHERE version_id = $1`, *made.VersionID)
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
		t.Fatalf("keys: %v", keys)
	}
	out := testkit.Result[tools.DocumentPurgeOut](t, b.do(t, b.admin, "document.purge", m{"course_id": b.course,
		"document_id": made.DocumentID, "version_id": made.VersionID, "reason": "A student's name was in the handout."}))
	if out.PurgedVersions != 1 || out.FilesRemoved != 3 {
		t.Fatalf("purged: %+v", out)
	}
	for _, k := range keys {
		if _, err := b.Blob.Stat(context.Background(), k); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("%s is still in the store: %v", k, err)
		}
	}
	if n := b.Count(`SELECT count(*) FROM document_version_file WHERE version_id = $1`, *made.VersionID) +
		b.Count(`SELECT count(*) FROM document_version_text WHERE version_id = $1`, *made.VersionID); n != 0 {
		t.Fatal("the purged version's files or texts are still recorded")
	}
	got := b.get(t, b.sato, m{"document_id": made.DocumentID, "version_id": *made.VersionID}).Version
	if got.Purged == nil || len(got.Files) != 0 || got.DownloadURL != nil || got.BodyMD != nil || got.Text != nil {
		t.Fatalf("the tombstone: %+v", got)
	}
	b.refusedAs(t, b.sato, "document.file", m{"course_id": b.course, "document_id": made.DocumentID, "file_id": made.FileIDs[0]},
		apperr.NotFound, "")
}

// A submitted document holds several files as material does, handed in with
// its submission, and a feedback file is named as it is given.
func TestASubmittedDocumentAndFeedbackHoldFilesAsAVersionDoes(t *testing.T) {
	b := build(t)
	sub := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3})).SubmissionID
	essay, data := []byte("%PDF my essay"), []byte("a,b\n1,2\n")
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.yuki, "document.create", m{"course_id": b.course, "kind": "submission",
		"submission_id": sub, "title": "HW3", "files": []m{
			{"upload_token": b.upload(t, b.yuki, "submission", "application/pdf", essay), "filename": "essay.pdf"},
			{"upload_token": b.upload(t, b.yuki, "submission", "text/csv", data), "filename": "data.csv"},
		}}))
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": sub})
	got := b.get(t, b.sato, m{"document_id": made.DocumentID}).Version
	if len(got.Files) != 2 || got.Files[1].Filename != "data.csv" || got.Files[0].Text != nil {
		t.Fatalf("the submitted document: %+v", got)
	}
	if body, _ := b.fetch(t, *got.Files[1].DownloadURL); !bytes.Equal(body, data) {
		t.Fatalf("the data: %q", body)
	}
	if n := b.Count(`SELECT count(*) FROM document_version_text WHERE document_id = $1`, made.DocumentID); n != 0 {
		t.Fatal("a student's files were queued to be transcribed")
	}

	marked := []byte("%PDF marked")
	g := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": sub, "score": 80,
		"feedback_files": []m{
			{"title": "Marked essay", "upload_token": b.upload(t, b.sato, "feedback", "application/pdf", marked), "filename": "essay-marked.pdf"},
			{"title": "Comments", "upload_token": b.upload(t, b.sato, "feedback", "text/plain", []byte("Good."))},
		}}))
	view := testkit.Result[tools.GradeView](t, b.do(t, b.sato, "grade.get", m{"course_id": b.course, "grade_id": g.GradeID}))
	if len(view.FeedbackFiles) != 2 {
		t.Fatalf("feedback files: %+v", view.FeedbackFiles)
	}
	var names []string
	for _, f := range view.FeedbackFiles {
		names = append(names, b.get(t, b.sato, m{"document_id": f.DocumentID}).Version.Files[0].Filename)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"Comments.txt", "essay-marked.pdf"}) {
		t.Fatalf("the feedback files are named %v", names)
	}
}

// The application names a file after its document's title as the database
// does, for the file of a release that knows one file to a version.
func TestAFileIsNamedAfterItsDocumentAsTheDatabaseNamesIt(t *testing.T) {
	w := testkit.NewWorld(t)
	for _, c := range []struct{ title, contentType string }{
		{"Week 1", "application/pdf"},
		{"Week 2/3\thandout", "application/vnd.openxmlformats-officedocument.wordprocessingml.document; charset=binary"},
		{"essay.PDF", "application/pdf"},
		{"  ", "image/png"},
		{"notes", "application/x-unknown"},
		{"a\\b" + string(rune(0x202e)) + "gpj.exe", "image/jpeg"},
		{strings.Repeat("長", 300), "text/plain"},
		{"\x7f" + string(rune(0x85)) + " trailing /", "text/markdown"},
	} {
		var want string
		if err := w.Pool.QueryRow(context.Background(), `SELECT document_file_name($1, $2)`, c.title, c.contentType).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if got := tools.LegacyFilename(c.title, c.contentType); got != want {
			t.Fatalf("%q, %q: the application says %q, the database %q", c.title, c.contentType, got, want)
		}
	}
}
