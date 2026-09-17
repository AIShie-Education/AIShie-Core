package tools_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/blob"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// upload does what a client does: ask for a URL, PUT the bytes to it, keep
// the token. The PUT goes straight to the store, as it would to S3.
func (b *built) upload(t *testing.T, actor uuid.UUID, kind, contentType string, body []byte) string {
	t.Helper()
	out := testkit.Result[tools.UploadURLOut](t, b.do(t, actor, "document.upload_url", m{"course_id": b.course, "kind": kind, "content_type": contentType}))
	b.put(t, out, body)
	return out.UploadToken
}

func (b *built) put(t *testing.T, u tools.UploadURLOut, body []byte) {
	t.Helper()
	key, ct, err := b.Blob.Redeem(strings.TrimPrefix(u.UploadURL, "http://lms.test"+blob.BlobPath), "PUT")
	if err != nil {
		t.Fatalf("the upload URL does not redeem: %v", err)
	}
	if _, err := b.Blob.Put(context.Background(), key, ct, bytes.NewReader(body), 1<<30); err != nil {
		t.Fatal(err)
	}
}

// download follows a download_url the way a client would.
func (b *built) download(t *testing.T, url string) []byte {
	t.Helper()
	key, _, err := b.Blob.Redeem(strings.TrimPrefix(url, "http://lms.test"+blob.BlobPath), "GET")
	if err != nil {
		t.Fatalf("the download URL does not redeem: %v", err)
	}
	rc, _, err := b.Blob.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	return got
}

func (b *built) get(t *testing.T, actor uuid.UUID, args m) tools.DocumentGetOut {
	t.Helper()
	args["course_id"] = b.course
	return testkit.Result[tools.DocumentGetOut](t, b.do(t, actor, "document.get", args))
}

// "Material is published by moving published_version_id. Students read the
// published version; instructors read the latest. A half-edited lecture is
// invisible until the pointer moves."
func TestMaterialIsInvisibleUntilPublished(t *testing.T) {
	b := build(t)
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Lecture 1", "body_md": "# Draft"}))
	doc := made.DocumentID

	if _, err := b.Call(b.yuki, "document.get", m{"course_id": b.course, "document_id": doc}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a student reading an unpublished lecture: %v", err)
	}
	listed := func(actor uuid.UUID) int {
		return len(testkit.Result[tools.DocumentListOut](t, b.do(t, actor, "document.list", m{"course_id": b.course})).Documents)
	}
	if listed(b.yuki) != 0 || listed(b.sato) != 1 {
		t.Fatalf("lists before publishing: student %d, instructor %d", listed(b.yuki), listed(b.sato))
	}

	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc})
	if got := b.get(t, b.yuki, m{"document_id": doc}); got.Version == nil || *got.Version.BodyMD != "# Draft" || !got.Version.Published {
		t.Fatalf("student after publishing: %+v", got.Version)
	}

	// An edit is a new version, and changes nothing for students yet.
	v2 := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": doc, "body_md": "# Rewritten"}))
	if v2.Seq != 2 || v2.Published {
		t.Fatalf("second version: %+v", v2)
	}
	if got := b.get(t, b.yuki, m{"document_id": doc}); *got.Version.BodyMD != "# Draft" {
		t.Fatalf("students see the half-edited lecture: %q", *got.Version.BodyMD)
	}
	if got := b.get(t, b.sato, m{"document_id": doc}); *got.Version.BodyMD != "# Rewritten" || got.Version.Published {
		t.Fatalf("the instructor should read the latest, as a draft: %+v", got.Version)
	}
	// A student cannot reach the draft by naming it, either.
	if _, err := b.Call(b.yuki, "document.get", m{"course_id": b.course, "document_id": doc, "version_id": v2.VersionID}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a student fetching a draft version by id: %v", err)
	}
	if out := b.MustCall(b.yuki, "document.versions", m{"course_id": b.course, "document_id": doc}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a student listing versions: %+v", out)
	}

	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc})
	if got := b.get(t, b.yuki, m{"document_id": doc}); *got.Version.BodyMD != "# Rewritten" {
		t.Fatal("publishing did not move what students read")
	}
	b.try(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc}, apperr.Conflict) // already published
	// Rolling back is publishing an earlier version.
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc, "version_id": made.VersionID})
	versions := testkit.Result[tools.DocumentVersionsOut](t, b.do(t, b.sato, "document.versions", m{"course_id": b.course, "document_id": doc})).Versions
	if len(versions) != 2 || !versions[0].Published || versions[1].Published {
		t.Fatalf("versions: %+v", versions)
	}
	// Versions are never changed, by anyone.
	if _, err := b.Pool.Exec(t.Context(), `UPDATE document_version SET body_md = 'tampered'`); err == nil {
		t.Fatal("the database let a version change")
	}

	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": doc})
	b.try(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": doc, "body_md": "x"}, apperr.Conflict)
	if listed(b.yuki) != 0 {
		t.Fatal("an archived document is still listed")
	}
}

// Which permission governs depends on what kind of document it is.
func TestDocumentPermissionFollowsKind(t *testing.T) {
	b := build(t)
	mk := func(kind, title string) uuid.UUID {
		id := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": kind, "title": title, "body_md": title})).DocumentID
		b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": id})
		return id
	}
	lecture, rubric := mk("material", "Lecture"), mk("rubric", "HW3 rubric")

	// The student preset reads material and not rubrics; the grader both.
	b.get(t, b.yuki, m{"document_id": lecture})
	if out := b.MustCall(b.yuki, "document.get", m{"course_id": b.course, "document_id": rubric}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a student reading the rubric: %+v", out)
	}
	b.get(t, b.grader, m{"document_id": rubric})

	kinds := func(actor uuid.UUID) map[string]int {
		got := map[string]int{}
		for _, d := range testkit.Result[tools.DocumentListOut](t, b.do(t, actor, "document.list", m{"course_id": b.course})).Documents {
			got[d.Kind]++
		}
		return got
	}
	if got := kinds(b.yuki); got["material"] != 1 || got["rubric"] != 0 {
		t.Fatalf("student's list: %v", got)
	}
	if got := kinds(b.grader); got["material"] != 1 || got["rubric"] != 1 {
		t.Fatalf("grader's list: %v", got)
	}
	if out := b.MustCall(b.yuki, "document.list", m{"course_id": b.course, "kind": "rubric"}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a student asking for rubrics by name: %+v", out)
	}
	// Writing follows the same table. A student may write submission files,
	// and nothing else.
	if out := b.MustCall(b.yuki, "document.create", m{"course_id": b.course, "kind": "material", "title": "my notes", "body_md": "x"}, "y1"); out.Status != domain.StatusDenied {
		t.Fatalf("a student creating material: %+v", out)
	}
	if out := b.MustCall(b.yuki, "document.add_version", m{"course_id": b.course, "document_id": lecture, "body_md": "defaced"}, "y2"); out.Status != domain.StatusDenied {
		t.Fatalf("a student editing a lecture: %+v", out)
	}
	// The assignment can now point at the rubric; a lecture is not a rubric.
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "rubric_document_id": rubric})
	b.try(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "rubric_document_id": lecture}, apperr.FailedPrecondition)
}

func TestFilesRoundTrip(t *testing.T) {
	b := build(t)
	pdf := []byte("%PDF-1.7 slides")
	token := b.upload(t, b.sato, "material", "application/pdf", pdf)
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": "Slides", "upload_token": token, "body_md": "See the slides."}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": made.DocumentID})

	got := b.get(t, b.yuki, m{"document_id": made.DocumentID}).Version
	if got.DownloadURL == nil || *got.ContentType != "application/pdf" || *got.ByteSize != int64(len(pdf)) || !strings.HasPrefix(*got.Checksum, "sha256:") {
		t.Fatalf("version: %+v", got)
	}
	if !bytes.Equal(b.download(t, *got.DownloadURL), pdf) {
		t.Fatal("what came back is not what went up")
	}
	if n := b.Count(`SELECT count(*) FROM document_version WHERE document_id = $1 AND author_member_id = $2 AND storage_key LIKE 'courses/' || $3 || '/%'`,
		made.DocumentID, b.satoM, b.course.String()); n != 1 {
		t.Fatal("the version row does not name its author and a key inside the course")
	}

	// An upload belongs to whoever asked for it, for what they asked.
	b.try(t, b.sato, "document.create", m{"course_id": b.course, "kind": "material", "title": "Again", "upload_token": token}, apperr.Conflict) // attached once
	mine := b.upload(t, b.sato, "material", "text/plain", []byte("x"))
	for name, args := range map[string]m{
		"as a rubric instead":  {"kind": "rubric", "title": "x", "upload_token": mine},
		"a forged token":       {"kind": "material", "title": "x", "upload_token": mine[:len(mine)-2] + "AA"},
		"a token that is none": {"kind": "material", "title": "x", "upload_token": "nonsense"},
	} {
		args["course_id"] = b.course
		if out, err := b.Call(b.sato, "document.create", args, uuid.NewString()); err == nil && out.Status == domain.StatusExecuted {
			t.Errorf("%s: attached", name)
		}
	}
	other := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Co-instructor"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": other, "preset": "instructor"})
	b.try(t, other, "document.create", m{"course_id": b.course, "kind": "material", "title": "x", "upload_token": mine}, apperr.Forbidden) // someone else's upload

	// Asked for, never uploaded.
	empty := testkit.Result[tools.UploadURLOut](t, b.do(t, b.sato, "document.upload_url", m{"course_id": b.course, "kind": "material", "content_type": "text/plain"}))
	b.try(t, b.sato, "document.create", m{"course_id": b.course, "kind": "material", "title": "x", "upload_token": empty.UploadToken}, apperr.FailedPrecondition)
	// Too big: refused, and not kept.
	big := b.upload(t, b.sato, "material", "application/zip", bytes.Repeat([]byte("z"), testkit.MaxUploadBytes+1))
	b.try(t, b.sato, "document.create", m{"course_id": b.course, "kind": "material", "title": "x", "upload_token": big}, apperr.FailedPrecondition)
	claim, _ := b.Uploads.VerifyUpload(big)
	if _, err := b.Blob.Stat(context.Background(), claim.Key); err == nil {
		t.Fatal("an oversized upload was kept after being refused")
	}
	if out := b.MustCall(b.yuki, "document.upload_url", m{"course_id": b.course, "kind": "material", "content_type": "text/plain"}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a student asking to upload course material: %+v", out)
	}
}

func TestSubmissionFiles(t *testing.T) {
	b := build(t)
	draft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3})).SubmissionID
	essay := []byte("PK zip of an essay")
	token := b.upload(t, b.yuki, "submission", "application/zip", essay)
	file := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.yuki, "document.create",
		m{"course_id": b.course, "kind": "submission", "title": "essay.zip", "submission_id": draft, "upload_token": token})).DocumentID

	// A file is enough to hand in: no text needed.
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": draft})
	// Once handed in, the files are frozen with it.
	late := b.upload(t, b.yuki, "submission", "text/plain", []byte("one more thing"))
	b.try(t, b.yuki, "document.create", m{"course_id": b.course, "kind": "submission", "title": "ps.txt", "submission_id": draft, "upload_token": late}, apperr.Conflict)
	b.try(t, b.yuki, "document.archive", m{"course_id": b.course, "document_id": file}, apperr.Conflict)
	b.try(t, b.yuki, "document.add_version", m{"course_id": b.course, "document_id": file, "body_md": "v2"}, apperr.FailedPrecondition)

	// The grader finds the file through the submission and reads it.
	sub := testkit.Result[tools.SubmissionView](t, b.do(t, b.grader, "submission.get", m{"course_id": b.course, "submission_id": draft}))
	if len(sub.Files) != 1 || sub.Files[0].DocumentID != file || sub.Files[0].Title != "essay.zip" {
		t.Fatalf("submission files: %+v", sub.Files)
	}
	got := b.get(t, b.grader, m{"document_id": file})
	if !bytes.Equal(b.download(t, *got.Version.DownloadURL), essay) {
		t.Fatal("the grader did not get the student's bytes")
	}
	// Another student does not, and neither does a student attach to another's draft.
	if out := b.MustCall(b.ken, "document.get", m{"course_id": b.course, "document_id": file}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("Ken reading Yuki's file: %+v", out)
	}
	kens := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3})).SubmissionID
	planted := b.upload(t, b.yuki, "submission", "text/plain", []byte("not yours"))
	if out := b.MustCall(b.yuki, "document.create", m{"course_id": b.course, "kind": "submission", "title": "x", "submission_id": kens, "upload_token": planted}, "plant"); out.Status != domain.StatusDenied {
		t.Fatalf("Yuki attaching a file to Ken's draft: %+v", out)
	}
}

// A proposal has no grade yet for a file to hang from, so the file travels
// inside the proposal, and is attached when the proposal executes — possibly
// long after the upload window has closed.
func TestFeedbackFilesTravelWithAProposal(t *testing.T) {
	b := build(t)
	work := b.submit(t, b.yuki, "essay")
	marked := []byte("%PDF marked-up essay")
	token := b.upload(t, b.grader, "feedback", "application/pdf", marked)

	proposed := b.MustCall(b.grader, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 85,
		"feedback_files": []m{{"title": "essay-marked.pdf", "upload_token": token}}}, "p")
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("%+v", proposed)
	}
	if n := b.Count(`SELECT count(*) FROM document WHERE kind = 'feedback'`); n != 0 {
		t.Fatal("a proposal wrote a document")
	}
	// Three days pass. The upload URL expired long ago; the file is still
	// the proposer's.
	b.P.SetClock(func() time.Time { return time.Now().Add(72 * time.Hour) })
	decided := b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"})
	v := testkit.Result[pipeline.DecideOut](t, decided)
	if v.Outcome != domain.StatusExecuted {
		t.Fatalf("approval: %+v", v)
	}
	gradeID := testkit.Result[tools.GradeSubmitOut](t, pipeline.Outcome{Result: v.Result}).GradeID
	if n := b.Count(`SELECT count(*) FROM document d JOIN document_version dv ON dv.document_id = d.id
		WHERE d.kind = 'feedback' AND d.grade_id = $1 AND d.title = 'essay-marked.pdf' AND dv.author_member_id = $2`, gradeID, b.graderM); n != 1 {
		t.Fatal("the feedback file was not attached to the grade, authored by the agent")
	}
	b.P.SetClock(time.Now)

	// Yuki cannot read feedback on a grade she cannot see yet.
	var file uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT id FROM document WHERE grade_id = $1`, gradeID).Scan(&file); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(b.yuki, "document.get", m{"course_id": b.course, "document_id": file}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("feedback on an unposted grade: %v", err)
	}
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "grade_ids": []uuid.UUID{gradeID}})
	g := testkit.Result[tools.GradeView](t, b.do(t, b.yuki, "grade.get", m{"course_id": b.course, "grade_id": gradeID}))
	if len(g.FeedbackFiles) != 1 {
		t.Fatalf("grade.get lists %d feedback files", len(g.FeedbackFiles))
	}
	if got := b.get(t, b.yuki, m{"document_id": g.FeedbackFiles[0].DocumentID}); !bytes.Equal(b.download(t, *got.Version.DownloadURL), marked) {
		t.Fatal("Yuki did not get her marked-up essay")
	}
	if out := b.MustCall(b.ken, "document.get", m{"course_id": b.course, "document_id": file}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("Ken reading Yuki's feedback: %+v", out)
	}
	// A proposal naming a file that was never uploaded is refused at once,
	// not queued for someone to approve.
	never := testkit.Result[tools.UploadURLOut](t, b.do(t, b.grader, "document.upload_url", m{"course_id": b.course, "kind": "feedback", "content_type": "text/plain"}))
	kenWork := b.submit(t, b.ken, "essay")
	b.try(t, b.grader, "grade.submit", m{"course_id": b.course, "submission_id": kenWork, "score": 1,
		"feedback_files": []m{{"title": "x", "upload_token": never.UploadToken}}}, apperr.FailedPrecondition)
}

// "submission pins instructions_version_id ... so a dispute can show exactly
// what the student was told."
func TestPinnedVersionsStayReadable(t *testing.T) {
	b := build(t)
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "instructions", "title": "HW3", "body_md": "Write 1000 words."}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": made.DocumentID})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": made.DocumentID})

	yukiWork := b.submit(t, b.yuki, "1000 words") // pinned to v1
	b.do(t, b.sato, "document.add_version", m{"course_id": b.course, "document_id": made.DocumentID, "body_md": "Write 2000 words.", "publish": true})
	b.submit(t, b.ken, "2000 words") // pinned to v2

	sub := testkit.Result[tools.SubmissionView](t, b.do(t, b.yuki, "submission.get", m{"course_id": b.course, "submission_id": yukiWork}))
	if sub.InstructionsVersionID == nil || *sub.InstructionsVersionID != *made.VersionID {
		t.Fatalf("Yuki's submission is pinned to %v, want v1", sub.InstructionsVersionID)
	}
	// What is published now says 2000. Yuki can still read what she was told.
	if got := b.get(t, b.yuki, m{"document_id": made.DocumentID}); *got.Version.BodyMD != "Write 2000 words." {
		t.Fatalf("published: %q", *got.Version.BodyMD)
	}
	if got := b.get(t, b.yuki, m{"document_id": made.DocumentID, "version_id": made.VersionID}); *got.Version.BodyMD != "Write 1000 words." || got.Version.Published {
		t.Fatalf("pinned: %+v", got.Version)
	}
	// Ken was never told 1000 words; v1 is not his to fetch.
	if _, err := b.Call(b.ken, "document.get", m{"course_id": b.course, "document_id": made.DocumentID, "version_id": made.VersionID}, ""); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("Ken fetching a version he was never pinned to: %v", err)
	}
	// The tutor listed for Yuki can, because Yuki's work is within its scope.
	b.get(t, b.tutor, m{"document_id": made.DocumentID, "version_id": made.VersionID})
	// An assignment is published only with instructions students can read.
	draftOnly := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": "instructions", "title": "HW4", "body_md": "tbd"})).DocumentID
	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW4", "points_possible": 10, "instructions_document_id": draftOnly})).ID
	b.try(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw4}, apperr.FailedPrecondition)
}
