package tools_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/blob"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// Renditions (docs/schema.md §2.4): every Office or OpenDocument file Core
// keeps, of a document's version of any kind or carried by a message, is
// queued for a PDF as it is recorded; the site's agent runtime claims it,
// reads the file, uploads the PDF and says what became of it; whoever may
// read the file reads its PDF, and nobody else.

const (
	docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	pptxType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	xlsxType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// pdfOf is the PDF the runtime makes of a file: what Core takes is what
// begins %PDF-.
func pdfOf(what string) []byte { return []byte("%PDF-1.7\n% the rendition of " + what + "\n%%EOF\n") }

// claimRenditions claims as the site's agent runtime, and insists it was
// carried out.
func (b *built) claimRenditions(t *testing.T, args m) []tools.ClaimedRendition {
	t.Helper()
	out := b.asRuntime(t, "agent_runtime.rendition_claim", args)
	if out.Status != domain.StatusExecuted {
		t.Fatalf("agent_runtime.rendition_claim: %+v", out)
	}
	return testkit.Result[tools.RenditionClaimOut](t, out).Claimed
}

// uploadPDF asks for somewhere to put a claim's PDF and PUTs body there, as
// the runtime does, held to what the URL takes; it returns the token.
func (b *built) uploadPDF(t *testing.T, c tools.ClaimedRendition, body []byte) string {
	t.Helper()
	out := b.asRuntime(t, "agent_runtime.rendition_upload_url", m{"rendition_id": c.RenditionID, "lease_id": c.LeaseID})
	if out.Status != domain.StatusExecuted {
		t.Fatalf("agent_runtime.rendition_upload_url: %+v", out)
	}
	u := testkit.Result[tools.RenditionUploadURLOut](t, out)
	if u.Headers["Content-Type"] != "application/pdf" || u.MaxBytes <= 0 || u.UploadToken == "" {
		t.Fatalf("the upload URL: %+v", u)
	}
	grant, err := b.Blob.Grant(strings.TrimPrefix(u.UploadURL, "http://lms.test"+blob.BlobPath), "PUT")
	if err != nil || !strings.HasPrefix(grant.Key, tools.RenditionPrefix) || grant.ContentType != "application/pdf" || grant.MaxBytes != u.MaxBytes {
		t.Fatalf("the upload URL grants %+v %v", grant, err)
	}
	if _, err := b.Blob.Put(context.Background(), grant.Key, grant.ContentType, bytes.NewReader(body), grant.MaxBytes); err != nil {
		t.Fatal(err)
	}
	return u.UploadToken
}

// completeRendition says what became of a claim, as the runtime does.
func (b *built) completeRendition(t *testing.T, c tools.ClaimedRendition, args m) pipeline.Outcome {
	t.Helper()
	args["rendition_id"], args["lease_id"] = c.RenditionID, c.LeaseID
	return b.asRuntime(t, "agent_runtime.rendition_complete", args)
}

// convert converts a claim's file: the PDF uploaded and the claim completed
// done.
func (b *built) convert(t *testing.T, c tools.ClaimedRendition, pdf []byte, pages int) tools.RenditionCompleteOut {
	t.Helper()
	out := b.completeRendition(t, c, m{"status": "done", "upload_token": b.uploadPDF(t, c, pdf), "page_count": pages})
	if out.Status != domain.StatusExecuted {
		t.Fatalf("agent_runtime.rendition_complete: %+v", out)
	}
	return testkit.Result[tools.RenditionCompleteOut](t, out)
}

// view follows a rendition's URL the way a browser would: what it serves,
// as what, under what name, and whether to be shown where it is opened.
func (b *built) view(t *testing.T, url string) (body []byte, grant blob.Grant) {
	t.Helper()
	grant, err := b.Blob.Grant(strings.TrimPrefix(url, "http://lms.test"+blob.BlobPath), "GET")
	if err != nil {
		t.Fatalf("the rendition's URL does not redeem: %v", err)
	}
	return b.download(t, url), grant
}

// renditionState is where a file's rendition stands, and why, in the
// database.
func (b *built) renditionState(t *testing.T, column string, file uuid.UUID) (status, reason string, attempts int) {
	t.Helper()
	var why *string
	if err := b.Pool.QueryRow(context.Background(), `SELECT status, reason, attempts FROM file_rendition WHERE `+column+` = $1`, file).
		Scan(&status, &why, &attempts); err != nil {
		t.Fatalf("the rendition of %s: %v", file, err)
	}
	if why != nil {
		reason = *why
	}
	return status, reason, attempts
}

// handout is a material of Sato's with a Word file, a PDF and a picture.
func (b *built) handout(t *testing.T, publish bool) tools.DocumentCreateOut {
	t.Helper()
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course,
		"kind": "material", "title": "Week 4", "files": []m{
			{"upload_token": b.upload(t, b.sato, "material", docxType, []byte("PK the handout")), "filename": "第四週 handout.docx"},
			{"upload_token": b.upload(t, b.sato, "material", "application/pdf", []byte("%PDF slides")), "filename": "slides.pdf"},
			{"upload_token": b.upload(t, b.sato, "material", "image/png", []byte("\x89PNG")), "filename": "chart.png"},
		}}))
	if publish {
		b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": made.DocumentID})
	}
	return made
}

// An Office file is queued for its PDF as it is recorded, whatever kind of
// document holds it, and so is one a message carries; nothing else is.
func TestAnOfficeFileIsQueuedForItsPDF(t *testing.T) {
	b := build(t)
	made := b.handout(t, false)
	files := b.get(t, b.sato, m{"document_id": made.DocumentID}).Version.Files
	if len(files) != 3 || files[0].Rendition == nil || files[0].Rendition.State != "queued" || files[0].Rendition.DownloadURL != nil ||
		files[1].Rendition != nil || files[2].Rendition != nil {
		t.Fatalf("the handout's files: %+v", files)
	}

	// A student's file and a grader's are converted as staff's are; a .csv
	// a browser calls an Excel file is not.
	draft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "see the files"})).SubmissionID
	essay := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.yuki, "document.create", m{"course_id": b.course, "kind": "submission",
		"title": "essay", "submission_id": draft, "files": []m{
			{"upload_token": b.upload(t, b.yuki, "submission", "application/octet-stream", []byte("PK essay")), "filename": "essay.DOCX"},
			{"upload_token": b.upload(t, b.yuki, "submission", "application/vnd.ms-excel", []byte("a,b")), "filename": "data.csv"},
		}}))
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": draft})
	g := testkit.Result[tools.GradeSubmitOut](t, b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": draft,
		"score": 80, "feedback_files": []m{{"title": "Marks", "filename": "marks.xlsx",
			"upload_token": b.upload(t, b.sato, "feedback", xlsxType, []byte("PK marks"))}}}))
	feedback := testkit.Result[tools.GradeView](t, b.do(t, b.sato, "grade.get", m{"course_id": b.course, "grade_id": g.GradeID})).FeedbackFiles
	if s, _, _ := b.renditionState(t, "file_id", essay.FileIDs[0]); s != "queued" {
		t.Fatalf("the essay's rendition is %s", s)
	}
	if n := b.Count(`SELECT count(*) FROM file_rendition WHERE file_id = $1`, essay.FileIDs[1]); n != 0 {
		t.Fatal("a .csv was queued to be converted")
	}
	if len(feedback) != 1 {
		t.Fatalf("feedback: %+v", feedback)
	}
	marks := b.get(t, b.sato, m{"document_id": feedback[0].DocumentID}).Version.Files
	if len(marks) != 1 || marks[0].Rendition == nil || marks[0].Rendition.State != "queued" {
		t.Fatalf("the feedback file: %+v", marks)
	}

	// A message's file is queued as the message is written; an upload no
	// message came to carry is not.
	b.attachment(t, b.yuki, pptxType, []byte("PK never sent"))
	opened := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open", m{"course_id": b.course,
		"respondent_member_id": b.tutorM, "body": "My slides?", "attachments": []m{
			{"upload_token": b.attachment(t, b.yuki, pptxType, []byte("PK slides")), "filename": "talk.pptx"},
			{"upload_token": b.attachment(t, b.yuki, "image/png", []byte("\x89PNG")), "filename": "chart.png"}}}))
	msgs := b.messages(t, b.tutor, opened.ConversationID)
	if len(msgs) != 1 || len(msgs[0].Attachments) != 2 || msgs[0].Attachments[0].Rendition == nil ||
		msgs[0].Attachments[0].Rendition.State != "queued" || msgs[0].Attachments[1].Rendition != nil {
		t.Fatalf("the message's files: %+v", msgs)
	}
	if n := b.Count(`SELECT count(*) FROM file_rendition WHERE NOT backfill AND status = 'queued'`); n != 4 {
		t.Fatalf("%d renditions queued, want the handout's, the essay's, the marks' and the talk's", n)
	}
	// Queueing tells nobody's feed.
	if n := b.Count(`SELECT count(*) FROM event WHERE type LIKE '%rendition%'`); n != 0 {
		t.Fatalf("%d events of renditions", n)
	}
}

// The runtime claims a file, reads it, uploads its PDF and says it is done;
// the file's readers are then shown the PDF, inline, named as the file.
func TestTheRuntimeConvertsAFileOnce(t *testing.T) {
	b := build(t)
	made := b.handout(t, true)
	claimed := b.claimRenditions(t, m{"max": 5, "lease_s": 120})
	if len(claimed) != 1 {
		t.Fatalf("claimed %+v", claimed)
	}
	c := claimed[0]
	if c.Source != "document_file" || c.FileID == nil || *c.FileID != made.FileIDs[0] || c.AttachmentID != nil || c.Attempt != 1 ||
		c.Backfill || c.CourseID != b.course || c.Filename != "第四週 handout.docx" || c.ContentType != docxType ||
		c.MaxBytes != tools.DefaultRenditionMaxBytes || c.LeaseExpiresAt.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("the claim: %+v", c)
	}
	if got := b.download(t, c.DownloadURL); string(got) != "PK the handout" {
		t.Fatalf("the runtime read %q", got)
	}
	if len(b.claimRenditions(t, m{})) != 0 {
		t.Fatal("a claimed rendition was claimed again")
	}
	if s, _, _ := b.renditionState(t, "file_id", made.FileIDs[0]); s != "claimed" {
		t.Fatalf("claimed, it is %s", s)
	}
	// Readers see it being converted.
	if r := b.get(t, b.yuki, m{"document_id": made.DocumentID}).Version.Files[0].Rendition; r == nil || r.State != "claimed" {
		t.Fatalf("while it is converted: %+v", r)
	}

	file := testkit.Result[tools.RenditionFileOut](t, b.asRuntime(t, "agent_runtime.rendition_file",
		m{"rendition_id": c.RenditionID, "lease_id": c.LeaseID}))
	if file.Filename != c.Filename || string(b.download(t, file.DownloadURL)) != "PK the handout" {
		t.Fatalf("another URL for the file: %+v", file)
	}
	renewed := testkit.Result[tools.RenditionRenewOut](t, b.asRuntime(t, "agent_runtime.rendition_renew",
		m{"rendition_id": c.RenditionID, "lease_id": c.LeaseID, "lease_s": 900}))
	if !renewed.LeaseExpiresAt.After(c.LeaseExpiresAt) {
		t.Fatalf("renewed until %v, was %v", renewed.LeaseExpiresAt, c.LeaseExpiresAt)
	}

	pdf := pdfOf("the handout")
	done := b.convert(t, c, pdf, 3)
	if done.State != "done" || done.ByteSize == nil || *done.ByteSize != int64(len(pdf)) || done.Checksum == nil ||
		!strings.HasPrefix(*done.Checksum, "sha256:") {
		t.Fatalf("done: %+v", done)
	}

	// Its readers: the student reads the published version's file, and its
	// PDF, shown where it is opened, as a PDF, named as the file.
	for _, who := range []uuid.UUID{b.yuki, b.sato} {
		r := b.get(t, who, m{"document_id": made.DocumentID}).Version.Files[0].Rendition
		if r == nil || r.State != "done" || r.PageCount == nil || *r.PageCount != 3 || r.ByteSize == nil || *r.ByteSize != int64(len(pdf)) ||
			r.DownloadURL == nil || r.DownloadExpiresAt == nil || r.DownloadExpiresAt.Before(time.Now().Add(10*time.Minute)) || r.Reason != nil {
			t.Fatalf("the rendition, as document.get gives it: %+v", r)
		}
		body, grant := b.view(t, *r.DownloadURL)
		if !bytes.Equal(body, pdf) || !grant.Inline || grant.ContentType != "application/pdf" || grant.Filename != "第四週 handout.pdf" {
			t.Fatalf("the PDF served: %q %+v", body, grant)
		}
	}
	one := testkit.Result[tools.DocumentFileOut](t, b.do(t, b.yuki, "document.file",
		m{"course_id": b.course, "document_id": made.DocumentID, "file_id": made.FileIDs[0]}))
	if one.Rendition == nil || one.Rendition.State != "done" || one.Rendition.DownloadURL == nil {
		t.Fatalf("document.file's rendition: %+v", one.Rendition)
	}
	if body, _ := b.view(t, *one.Rendition.DownloadURL); !bytes.Equal(body, pdf) {
		t.Fatalf("document.file's PDF: %q", body)
	}
	// The original downloads as it was, never inline.
	if _, name := b.fetch(t, one.DownloadURL); name != "第四週 handout.docx" {
		t.Fatalf("the original downloads as %q", name)
	}
	// The version list says where it stands, and nothing more.
	versions := testkit.Result[tools.DocumentVersionsOut](t, b.do(t, b.sato, "document.versions",
		m{"course_id": b.course, "document_id": made.DocumentID})).Versions
	if r := versions[0].Files[0].Rendition; r == nil || r.State != "done" || r.DownloadURL != nil || r.PageCount != nil || r.ByteSize != nil ||
		versions[0].Files[1].Rendition != nil {
		t.Fatalf("document.versions' renditions: %+v", versions[0].Files)
	}

	// Done is for good: the claim no longer holds, and nothing is converted
	// again.
	refusedAs(t, "completing it again", b.completeRendition(t, c, m{"status": "failed", "reason": "timeout"}), apperr.Conflict, "lease_lost")
	if len(b.claimRenditions(t, m{})) != 0 {
		t.Fatal("a done rendition was claimed again")
	}
	// The service's completion is its action, recorded without the upload
	// token; it is news to nobody.
	var payload string
	if err := b.Pool.QueryRow(context.Background(), `SELECT payload::text FROM action
		WHERE action_type = 'agent_runtime.rendition_complete' AND status = 'executed'`).Scan(&payload); err != nil ||
		strings.Contains(payload, "upload_token") {
		t.Fatalf("the completion's record: %s %v", payload, err)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type LIKE '%rendition%'`); n != 0 {
		t.Fatalf("%d events of renditions", n)
	}
}

// Whoever may read a file reads its rendition, by the same checks, and to
// anyone else it does not exist: an unpublished material's to students, a
// student's essay to another student, a message's file to whoever does not
// read the conversation, a retracted message's file to everyone.
func TestARenditionIsReadAsItsFileIs(t *testing.T) {
	b := build(t)
	made := b.handout(t, false)
	b.convert(t, b.claimRenditions(t, m{})[0], pdfOf("the handout"), 2)

	// Unpublished: staff read it; a student finds neither the file nor its PDF.
	if r := b.get(t, b.sato, m{"document_id": made.DocumentID}).Version.Files[0].Rendition; r == nil || r.DownloadURL == nil {
		t.Fatalf("the instructor reading the draft's rendition: %+v", r)
	}
	b.try(t, b.yuki, "document.get", m{"course_id": b.course, "document_id": made.DocumentID}, apperr.NotFound)
	b.try(t, b.yuki, "document.file", m{"course_id": b.course, "document_id": made.DocumentID, "file_id": made.FileIDs[0]}, apperr.NotFound)
	// Published, she reads it.
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": made.DocumentID})
	if r := b.get(t, b.yuki, m{"document_id": made.DocumentID}).Version.Files[0].Rendition; r == nil || r.DownloadURL == nil {
		t.Fatalf("the student reading the published rendition: %+v", r)
	}
	// Archived, it is withdrawn from her with its document.
	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": made.DocumentID})
	b.try(t, b.yuki, "document.file", m{"course_id": b.course, "document_id": made.DocumentID, "file_id": made.FileIDs[0]}, apperr.NotFound)
	b.do(t, b.sato, "document.unarchive", m{"course_id": b.course, "document_id": made.DocumentID})

	// Ken's essay: Ken and his instructor read its PDF; Yuki does not.
	draft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.ken, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "see the file"})).SubmissionID
	essay := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.ken, "document.create", m{"course_id": b.course, "kind": "submission",
		"title": "essay", "submission_id": draft, "files": []m{
			{"upload_token": b.upload(t, b.ken, "submission", docxType, []byte("PK Ken's essay")), "filename": "ken.docx"}}}))
	b.convert(t, b.claimRenditions(t, m{})[0], pdfOf("Ken's essay"), 5)
	for _, who := range []uuid.UUID{b.ken, b.sato} {
		got := testkit.Result[tools.DocumentFileOut](t, b.do(t, who, "document.file",
			m{"course_id": b.course, "document_id": essay.DocumentID, "file_id": essay.FileIDs[0]}))
		if got.Rendition == nil || got.Rendition.DownloadURL == nil || *got.Rendition.PageCount != 5 {
			t.Fatalf("the essay's rendition for its readers: %+v", got.Rendition)
		}
	}
	if out := b.MustCall(b.yuki, "document.file", m{"course_id": b.course, "document_id": essay.DocumentID, "file_id": essay.FileIDs[0]},
		"yuki-reads-ken"); out.Status == domain.StatusExecuted {
		t.Fatalf("another student read the essay's rendition: %s", out.Result)
	}
	if out, err := b.Call(b.yuki, "document.get", m{"course_id": b.course, "document_id": essay.DocumentID}, ""); err == nil &&
		out.Status == domain.StatusExecuted {
		t.Fatalf("another student read the essay: %s", out.Result)
	}

	// A message's file: the conversation's readers, and nobody else.
	opened := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open", m{"course_id": b.course,
		"respondent_member_id": b.tutorM, "body": "My talk?", "attachments": []m{
			{"upload_token": b.attachment(t, b.yuki, pptxType, []byte("PK talk")), "filename": "talk.pptx"}}}))
	c := b.claimRenditions(t, m{})[0]
	if c.Source != "attachment" || c.AttachmentID == nil || c.FileID != nil || c.Filename != "talk.pptx" {
		t.Fatalf("the claim of a message's file: %+v", c)
	}
	b.convert(t, c, pdfOf("the talk"), 12)
	attached := *c.AttachmentID
	for _, who := range []uuid.UUID{b.yuki, b.tutor} {
		got := b.attachmentOf(t, who, attached)
		if got.Rendition == nil || got.Rendition.State != "done" || got.Rendition.DownloadURL == nil || *got.Rendition.PageCount != 12 {
			t.Fatalf("conversation.attachment's rendition: %+v", got.Rendition)
		}
		body, grant := b.view(t, *got.Rendition.DownloadURL)
		if string(body) != string(pdfOf("the talk")) || grant.Filename != "talk.pdf" || !grant.Inline {
			t.Fatalf("the talk's PDF: %q %+v", body, grant)
		}
	}
	if msgs := b.messages(t, b.yuki, opened.ConversationID); msgs[0].Attachments[0].Rendition == nil ||
		msgs[0].Attachments[0].Rendition.State != "done" || msgs[0].Attachments[0].Rendition.DownloadURL != nil {
		t.Fatalf("the message lists its file's rendition as %+v", msgs[0].Attachments[0].Rendition)
	}
	b.refusedAs(t, b.ken, "conversation.attachment", m{"course_id": b.course, "attachment_id": attached}, apperr.NotFound, "")
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": *opened.MessageID, "reason": "wrong file"})
	b.refusedAs(t, b.tutor, "conversation.attachment", m{"course_id": b.course, "attachment_id": attached}, apperr.NotFound, "retracted")

	// The runtime reads none of it as a reader: its credential opens its
	// own tools alone.
	if out := b.asRuntime(t, "document.get", m{"course_id": b.course, "document_id": made.DocumentID}); out.Status != domain.StatusDenied ||
		out.Error.Details["reason"] != "not_for_services" {
		t.Fatalf("the runtime reading a document: %+v", out)
	}
}

// The runtime's tools are its own; and every way a call of it can be wrong
// is refused, writing nothing: another's claim, a lapsed one, an upload
// that is not there, not a PDF, too large, or another claim's, and a
// completion of the wrong shape.
func TestTheRuntimesRenditionCallsAreRefused(t *testing.T) {
	b := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.Renditions.MaxBytes = 256 }))
	made := b.handout(t, true)

	// Nobody else claims: not a person, not the transcriber.
	if out := b.MustCall(b.sato, "agent_runtime.rendition_claim", m{}, "sato-claims"); out.Status != domain.StatusDenied ||
		out.Error.Details["reason"] != "service_only" {
		t.Fatalf("a person claiming: %+v", out)
	}
	if out := b.MustCallWith(b.transcriber(t), "agent_runtime.rendition_claim", m{}); out.Status != domain.StatusDenied ||
		out.Error.Details["reason"] != "not_for_services" {
		t.Fatalf("the transcriber claiming: %+v", out)
	}
	for _, args := range []m{{"max": 11}, {"max": -1}, {"lease_s": 30}, {"lease_s": 7200}, {"wait_s": 26}} {
		if out, err := b.CallWith(b.AsRuntime(), "agent_runtime.rendition_claim", args, ""); err == nil && out.Status == domain.StatusExecuted {
			t.Fatalf("claimed with %v: %+v", args, out)
		}
	}
	c := b.claimRenditions(t, m{})[0]
	if c.MaxBytes != 256 {
		t.Fatalf("the claim says the largest PDF is %d", c.MaxBytes)
	}

	// Another lease is not this claim.
	other := m{"rendition_id": c.RenditionID, "lease_id": uuid.New()}
	if _, err := b.CallWith(b.AsRuntime(), "agent_runtime.rendition_file", other, ""); !apperr.Is(err, apperr.Conflict) {
		t.Fatalf("the file, by another lease: %v", err)
	}
	if _, err := b.CallWith(b.AsRuntime(), "agent_runtime.rendition_upload_url", other, ""); !apperr.Is(err, apperr.Conflict) {
		t.Fatalf("an upload URL, by another lease: %v", err)
	}
	if _, err := b.CallWith(b.AsRuntime(), "agent_runtime.rendition_renew", other, ""); !apperr.Is(err, apperr.Conflict) {
		t.Fatalf("renewing another lease: %v", err)
	}
	refusedAs(t, "completing another lease", b.asRuntime(t, "agent_runtime.rendition_complete",
		m{"rendition_id": c.RenditionID, "lease_id": uuid.New(), "status": "failed", "reason": "timeout"}), apperr.Conflict, "lease_lost")
	if _, err := b.CallWith(b.AsRuntime(), "agent_runtime.rendition_complete",
		m{"rendition_id": uuid.New(), "lease_id": c.LeaseID, "status": "failed", "reason": "timeout"}, "no-such"); !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("completing no rendition: %v", err)
	}

	// A completion of the wrong shape.
	token := b.uploadPDF(t, c, pdfOf("the handout"))
	for what, args := range map[string]m{
		"done without its upload":      {"status": "done", "page_count": 1},
		"done without its page count":  {"status": "done", "upload_token": token},
		"done with no pages":           {"status": "done", "upload_token": token, "page_count": 0},
		"done with a reason":           {"status": "done", "upload_token": token, "page_count": 1, "reason": "timeout"},
		"failed without a reason":      {"status": "failed"},
		"failed with a reason of none": {"status": "failed", "reason": "it was hard"},
		"skipped with a page count":    {"status": "skipped", "reason": "unsupported", "page_count": 1},
		"skipped with an upload":       {"status": "skipped", "reason": "unsupported", "upload_token": token},
		"nothing in particular":        {"status": "maybe"},
	} {
		out, err := b.CallWith(b.AsRuntime(), "agent_runtime.rendition_complete", merge(m{"rendition_id": c.RenditionID,
			"lease_id": c.LeaseID}, args), "shape-"+uuid.NewString())
		if !apperr.Is(err, apperr.InvalidArgument) && (err != nil || out.Status != domain.StatusFailed || out.Error.Code != apperr.InvalidArgument) {
			t.Fatalf("%s: %+v %v", what, out, err)
		}
	}
	// An upload token that is not one, or another claim's.
	refusedAs(t, "a token that is not one", b.completeRendition(t, c, m{"status": "done", "upload_token": "nonsense", "page_count": 1}),
		apperr.InvalidArgument, "bad_upload_token")
	doc := b.upload(t, b.sato, "material", "application/pdf", pdfOf("a document's upload"))
	refusedAs(t, "a document's upload token", b.completeRendition(t, c, m{"status": "done", "upload_token": doc, "page_count": 1}),
		apperr.InvalidArgument, "bad_upload_token")
	// The essay's claim's upload, named by the handout's.
	draft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "see the file"})).SubmissionID
	b.do(t, b.yuki, "document.create", m{"course_id": b.course, "kind": "submission", "title": "essay", "submission_id": draft,
		"files": []m{{"upload_token": b.upload(t, b.yuki, "submission", docxType, []byte("PK essay")), "filename": "essay.docx"}}})
	essay := b.claimRenditions(t, m{})[0]
	refusedAs(t, "another claim's upload", b.completeRendition(t, c, m{"status": "done", "upload_token": b.uploadPDF(t, essay, pdfOf("essay")),
		"page_count": 1}), apperr.Forbidden, "not_your_upload")

	// Nothing uploaded yet.
	u := testkit.Result[tools.RenditionUploadURLOut](t, b.asRuntime(t, "agent_runtime.rendition_upload_url",
		m{"rendition_id": c.RenditionID, "lease_id": c.LeaseID}))
	refusedAs(t, "nothing uploaded", b.completeRendition(t, c, m{"status": "done", "upload_token": u.UploadToken, "page_count": 1}),
		apperr.FailedPrecondition, "not_uploaded")
	// Not a PDF: refused, and discarded; the claim holds.
	notPDF := b.uploadPDF(t, c, []byte("<html>not a PDF</html>"))
	refusedAs(t, "not a PDF", b.completeRendition(t, c, m{"status": "done", "upload_token": notPDF, "page_count": 1}),
		apperr.FailedPrecondition, "not_a_pdf")
	if n := b.Count(`SELECT count(*) FROM file_rendition WHERE id = $1 AND status = 'claimed' AND lease_id = $2`, c.RenditionID, c.LeaseID); n != 1 {
		t.Fatal("a refused upload ended the claim")
	}
	// Too large: this server's disk stops it as it arrives; an object
	// store's URL takes it, and it is refused, and discarded, when it is
	// named. The runtime says it was skipped.
	long := append(pdfOf("a long handout"), bytes.Repeat([]byte("x"), 256)...)
	u = testkit.Result[tools.RenditionUploadURLOut](t, b.asRuntime(t, "agent_runtime.rendition_upload_url",
		m{"rendition_id": c.RenditionID, "lease_id": c.LeaseID}))
	grant, err := b.Blob.Grant(strings.TrimPrefix(u.UploadURL, "http://lms.test"+blob.BlobPath), "PUT")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Blob.Put(context.Background(), grant.Key, grant.ContentType, bytes.NewReader(long), grant.MaxBytes); err == nil {
		t.Fatal("the disk took a PDF larger than its URL says")
	}
	if _, err := b.Blob.Put(context.Background(), grant.Key, grant.ContentType, bytes.NewReader(long), 1<<30); err != nil {
		t.Fatal(err)
	}
	out := b.completeRendition(t, c, m{"status": "done", "upload_token": u.UploadToken, "page_count": 1})
	refusedAs(t, "too large", out, apperr.FailedPrecondition, "rendition_too_large")
	if fmt.Sprint(out.Error.Details["max_bytes"]) != "256" {
		t.Fatalf("too large, it says %+v", out.Error.Details)
	}
	if n := b.Count(`SELECT count(*) FROM file_rendition WHERE status = 'done'`); n != 0 {
		t.Fatal("something refused was taken")
	}
	skipped := b.completeRendition(t, c, m{"status": "skipped", "reason": "too_large"})
	if skipped.Status != domain.StatusExecuted {
		t.Fatalf("skipped: %+v", skipped)
	}
	r := b.get(t, b.yuki, m{"document_id": made.DocumentID}).Version.Files[0].Rendition
	if r == nil || r.State != "skipped" || r.Reason == nil || *r.Reason != "too_large" || r.DownloadURL != nil || r.PageCount != nil {
		t.Fatalf("the student is told %+v", r)
	}
	// The refused uploads are gone from the store; nothing was left under
	// renditions/ but the essay's, which its claim never named.
	var left []string
	if err := b.Blob.List(context.Background(), tools.RenditionPrefix, "", func(key string, _ time.Time) error {
		left = append(left, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("left under renditions/: %v, want the essay's upload and the PDF uploaded first and never named", left)
	}
}

// What the runtime says went wrong is shown, in its words; a rendition
// claimed five times and never finished is failed by Core, and staff send
// a failed one back to be converted again.
func TestAFailedRenditionIsTriedAgainAsStaffSay(t *testing.T) {
	b := build(t)
	made := b.handout(t, true)
	file := made.FileIDs[0]
	c := b.claimRenditions(t, m{"lease_s": 60})[0]
	if out := b.completeRendition(t, c, m{"status": "failed", "reason": "password_protected"}); out.Status != domain.StatusExecuted {
		t.Fatalf("failed: %+v", out)
	}
	if s, why, _ := b.renditionState(t, "file_id", file); s != "failed" || why != "password_protected" {
		t.Fatalf("after failing: %s %s", s, why)
	}
	retry := m{"course_id": b.course, "document_id": made.DocumentID, "file_id": file}

	// A student may not send it back; staff may, once.
	if out := b.MustCall(b.yuki, "document.rendition_retry", retry, "yuki-retries"); out.Status != domain.StatusDenied {
		t.Fatalf("a student retrying: %+v", out)
	}
	got := testkit.Result[tools.RenditionRetryOut](t, b.do(t, b.sato, "document.rendition_retry", retry))
	if !got.Changed || got.State != "queued" {
		t.Fatalf("retried: %+v", got)
	}
	if s, why, attempts := b.renditionState(t, "file_id", file); s != "queued" || why != "" || attempts != 0 {
		t.Fatalf("after the retry: %s %q %d", s, why, attempts)
	}
	if got := testkit.Result[tools.RenditionRetryOut](t, b.do(t, b.sato, "document.rendition_retry", retry)); got.Changed {
		t.Fatalf("retrying what waits: %+v", got)
	}
	b.refusedAs(t, b.sato, "document.rendition_retry", merge(retry, m{"file_id": made.FileIDs[1]}), apperr.NotFound, "no_rendition")
	b.refusedAs(t, b.sato, "document.rendition_retry", merge(retry, m{"file_id": uuid.New()}), apperr.NotFound, "")

	// Claimed five times and never finished, it is failed, attempts_exhausted.
	later := time.Now()
	defer b.P.SetClock(time.Now)
	for i := range tools.MaxRenditionAttempts {
		at := later.Add(time.Duration(i+1) * time.Hour)
		b.P.SetClock(func() time.Time { return at })
		if c := b.claimRenditions(t, m{"lease_s": 60}); len(c) != 1 || int(c[0].Attempt) != i+1 {
			t.Fatalf("claim %d: %+v", i+1, c)
		}
	}
	end := later.Add(10 * time.Hour)
	b.P.SetClock(func() time.Time { return end })
	if c := b.claimRenditions(t, m{}); len(c) != 0 {
		t.Fatalf("claimed a sixth time: %+v", c)
	}
	if s, why, attempts := b.renditionState(t, "file_id", file); s != "failed" || why != "attempts_exhausted" || attempts != 5 {
		t.Fatalf("after five claims: %s %s %d", s, why, attempts)
	}
	if r := b.get(t, b.yuki, m{"document_id": made.DocumentID}).Version.Files[0].Rendition; r == nil || r.State != "failed" ||
		r.Reason == nil || *r.Reason != "attempts_exhausted" {
		t.Fatalf("the student is told %+v", r)
	}

	// Sent back, it is converted; done, it is sent back no more.
	b.do(t, b.sato, "document.rendition_retry", retry)
	b.convert(t, b.claimRenditions(t, m{})[0], pdfOf("the handout"), 1)
	b.refusedAs(t, b.sato, "document.rendition_retry", retry, apperr.FailedPrecondition, "rendition_done")
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'document.rendition_retry' AND status = 'executed'`); n != 3 {
		t.Fatalf("%d retries recorded", n)
	}

	// A student sends back her own file's, whoever's message's file it is
	// its author's, or of staff who decide for the opener; not another's.
	draft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create",
		m{"course_id": b.course, "assignment_id": b.hw3, "body": "see the file"})).SubmissionID
	essay := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.yuki, "document.create", m{"course_id": b.course, "kind": "submission",
		"title": "essay", "submission_id": draft, "files": []m{
			{"upload_token": b.upload(t, b.yuki, "submission", docxType, []byte("PK essay")), "filename": "essay.docx"}}}))
	b.completeRendition(t, b.claimRenditions(t, m{})[0], m{"status": "failed", "reason": "conversion_failed"})
	essayRetry := m{"course_id": b.course, "document_id": essay.DocumentID, "file_id": essay.FileIDs[0]}
	if out := b.MustCall(b.ken, "document.rendition_retry", essayRetry, "ken-retries"); out.Status != domain.StatusDenied {
		t.Fatalf("another student retrying: %+v", out)
	}
	if got := testkit.Result[tools.RenditionRetryOut](t, b.do(t, b.yuki, "document.rendition_retry", essayRetry)); !got.Changed {
		t.Fatalf("the student retrying her own: %+v", got)
	}

	opened := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open", m{"course_id": b.course,
		"respondent_member_id": b.tutorM, "body": "My talk?", "attachments": []m{
			{"upload_token": b.attachment(t, b.yuki, pptxType, []byte("PK talk")), "filename": "talk.pptx"}}}))
	_ = opened
	var talk tools.ClaimedRendition
	for _, c := range b.claimRenditions(t, m{"max": 10}) {
		if c.Source == "attachment" {
			talk = c
		} else {
			b.completeRendition(t, c, m{"status": "skipped", "reason": "unsupported"})
		}
	}
	b.completeRendition(t, talk, m{"status": "skipped", "reason": "unsupported"})
	attachmentRetry := m{"course_id": b.course, "attachment_id": *talk.AttachmentID}
	b.refusedAs(t, b.ken, "conversation.rendition_retry", attachmentRetry, apperr.NotFound, "")
	b.refusedAs(t, b.tutor, "conversation.rendition_retry", attachmentRetry, apperr.Forbidden, "not_your_message")
	if got := testkit.Result[tools.RenditionRetryOut](t, b.do(t, b.yuki, "conversation.rendition_retry", attachmentRetry)); !got.Changed {
		t.Fatalf("its author retrying: %+v", got)
	}
	b.completeRendition(t, b.claimRenditions(t, m{})[0], m{"status": "failed", "reason": "timeout"})
	if got := testkit.Result[tools.RenditionRetryOut](t, b.do(t, b.sato, "conversation.rendition_retry", attachmentRetry)); !got.Changed {
		t.Fatalf("staff who decide for the opener retrying: %+v", got)
	}
}

// The queue gives each rendition to one claim, those queued as their files
// were recorded first, the oldest first, then the backfill, the newest
// first; a lapsed claim is claimed again and writes nothing back.
func TestTheRenditionQueueGivesEachFileToOneClaim(t *testing.T) {
	b := build(t)
	var files []uuid.UUID
	for i := range 12 {
		made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course,
			"kind": "material", "title": "Deck " + string(rune('A'+i)), "files": []m{
				{"upload_token": b.upload(t, b.sato, "material", pptxType, []byte("PK deck")), "filename": "deck.pptx"}}}))
		files = append(files, made.FileIDs[0])
	}
	// Four were there before renditions: the backfill, dated as their files.
	base := time.Now().Add(-48 * time.Hour)
	for i, f := range files[:4] {
		b.Exec(`UPDATE file_rendition SET backfill = true, queued_at = $2 WHERE file_id = $1`, f, base.Add(time.Duration(i)*time.Hour))
	}

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
				out, err := b.CallWith(b.AsRuntime(), "agent_runtime.rendition_claim", m{"max": 3}, "")
				if err != nil || out.Status != domain.StatusExecuted {
					t.Errorf("a claim: %+v %v", out, err)
					return
				}
				got := testkit.Result[tools.RenditionClaimOut](t, out).Claimed
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, c := range got {
					claimed[*c.FileID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != len(files) {
		t.Fatalf("%d files claimed, want %d", len(claimed), len(files))
	}
	for f, n := range claimed {
		if n != 1 {
			t.Fatalf("file %s was claimed %d times", f, n)
		}
	}

	// Uploads first, oldest first; then the backfill, newest first.
	b.Exec(`UPDATE file_rendition SET status = 'queued', lease_id = NULL, claimed_until = NULL, attempts = 0`)
	var order []uuid.UUID
	for range 2 {
		for _, c := range b.claimRenditions(t, m{"max": 10}) {
			order = append(order, *c.FileID)
		}
	}
	want := append(append([]uuid.UUID{}, files[4:]...), files[3], files[2], files[1], files[0])
	if len(order) != len(want) {
		t.Fatalf("claimed %d, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("claim %d is %s, want %s", i, order[i], want[i])
		}
	}

	// A lapsed claim is claimed again, and can write nothing back.
	b.Exec(`UPDATE file_rendition SET status = 'queued', lease_id = NULL, claimed_until = NULL, attempts = 0 WHERE file_id = $1`, files[4])
	first := b.claimRenditions(t, m{"lease_s": 60})[0]
	later := time.Now().Add(3 * time.Minute)
	b.P.SetClock(func() time.Time { return later })
	defer b.P.SetClock(time.Now)
	second := b.claimRenditions(t, m{})
	if len(second) != 1 || second[0].RenditionID != first.RenditionID || second[0].Attempt != 2 || second[0].LeaseID == first.LeaseID {
		t.Fatalf("the lapsed claim claimed again: %+v", second)
	}
	refusedAs(t, "the lapsed claim completing", b.completeRendition(t, first, m{"status": "failed", "reason": "timeout"}),
		apperr.Conflict, "lease_lost")
	if out := b.completeRendition(t, second[0], m{"status": "done", "upload_token": b.uploadPDF(t, second[0], pdfOf("deck")),
		"page_count": 9}); out.Status != domain.StatusExecuted {
		t.Fatalf("the claim that holds: %+v", out)
	}
}

// A claim that waits is woken by a file queued anywhere, a version's or a
// message's; revoking the runtime's credential gives back what it claimed,
// that claim not counted, and the list of its credentials counts its claims.
func TestTheRuntimeWaitsForFilesAndGivesThemBackWhenRevoked(t *testing.T) {
	b, hub := waking(t, wake.DefaultConfig)
	waitingClaim := func() <-chan read {
		c := make(chan read, 1)
		go func() {
			out, err := b.P.Invoke(context.Background(), b.AsRuntime(), "agent_runtime.rendition_claim", []byte(`{"wait_s": 10}`), "")
			c <- read{out, err, time.Now()}
		}()
		waitingNow(t, hub, 1)
		return c
	}
	waiting := waitingClaim()
	notYet(t, waiting, 200*time.Millisecond)
	made := b.handout(t, false)
	if got := resultOf[tools.RenditionClaimOut](t, answered(t, waiting, 5*time.Second)).Claimed; len(got) != 1 ||
		*got[0].FileID != made.FileIDs[0] {
		t.Fatalf("the waiting claim took %+v", got)
	}
	waiting = waitingClaim()
	b.do(t, b.yuki, "conversation.open", m{"course_id": b.course, "respondent_member_id": b.tutorM, "body": "My talk?",
		"attachments": []m{{"upload_token": b.attachment(t, b.yuki, pptxType, []byte("PK talk")), "filename": "talk.pptx"}}})
	if got := resultOf[tools.RenditionClaimOut](t, answered(t, waiting, 5*time.Second)).Claimed; len(got) != 1 || got[0].Source != "attachment" {
		t.Fatalf("the waiting claim took %+v", got)
	}

	// A second runtime credential claims a third file; root revokes it.
	second := testkit.Result[tools.ServiceIssueCredentialOut](t, b.do(t, b.Root, "service.issue_credential",
		m{"scope": "agent_runtime", "label": "second runtime"}))
	other := pipeline.Caller{ActorID: second.ServiceActorID, CredentialID: second.CredentialID}
	essay := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{"course_id": b.course, "kind": "material",
		"title": "Week 5", "files": []m{{"upload_token": b.upload(t, b.sato, "material", docxType, []byte("PK")), "filename": "w5.docx"}}}))
	held := testkit.Result[tools.RenditionClaimOut](t, b.as(t, other, "agent_runtime.rendition_claim", m{})).Claimed
	if len(held) != 1 || *held[0].FileID != essay.FileIDs[0] {
		t.Fatalf("the second credential claimed %+v", held)
	}
	listed := testkit.Result[tools.ServiceListCredentialsOut](t, b.do(t, b.Root, "service.list_credentials", m{"scope": "agent_runtime"}))
	claims := map[uuid.UUID]int32{}
	for _, c := range listed.Credentials {
		claims[c.ID] = c.ClaimsHeld
	}
	if claims[second.CredentialID] != 1 || claims[b.AsRuntime().CredentialID] != 2 {
		t.Fatalf("the claims held: %v", claims)
	}
	revoked := testkit.Result[tools.ServiceRevokeCredentialOut](t, b.do(t, b.Root, "service.revoke_credential",
		m{"scope": "agent_runtime", "credential_id": second.CredentialID}))
	if s, _, attempts := b.renditionState(t, "file_id", essay.FileIDs[0]); revoked.ClaimsReleased != 1 || s != "queued" || attempts != 0 {
		t.Fatalf("revoked %+v: the rendition is %s, %d attempts", revoked, s, attempts)
	}
	if again := b.claimRenditions(t, m{}); len(again) != 1 || *again[0].FileID != essay.FileIDs[0] || again[0].Attempt != 1 {
		t.Fatalf("given back, it is claimed as %+v", again)
	}
}

// A rendition goes with its file: a version purged takes its files' PDFs
// with it, from the database and the store.
func TestAPurgedVersionTakesItsRenditions(t *testing.T) {
	b := build(t)
	made := b.handout(t, true)
	b.convert(t, b.claimRenditions(t, m{})[0], pdfOf("the handout"), 2)
	var key string
	if err := b.Pool.QueryRow(context.Background(), `SELECT storage_key FROM file_rendition WHERE file_id = $1`, made.FileIDs[0]).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, tools.RenditionPrefix+b.course.String()+"/") {
		t.Fatalf("the PDF is kept under %s", key)
	}
	out := testkit.Result[tools.DocumentPurgeOut](t, b.do(t, b.admin, "document.purge", m{"course_id": b.course,
		"document_id": made.DocumentID, "version_id": *made.VersionID, "reason": "personal data"}))
	if out.FilesRemoved != 4 {
		t.Fatalf("purged %+v, want its three files and the PDF removed", out)
	}
	if n := b.Count(`SELECT count(*) FROM file_rendition`); n != 0 {
		t.Fatalf("%d renditions left", n)
	}
	if _, err := b.Blob.Stat(context.Background(), key); err == nil {
		t.Fatal("the PDF of a purged file is still in the store")
	}
}
