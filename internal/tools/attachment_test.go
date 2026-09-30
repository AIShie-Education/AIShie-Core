package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
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

// Files a message of a conversation carries: uploaded first, named by their
// tokens in the call that writes the message, read by whoever reads the
// conversation and found by nobody else, withheld with the text of a
// message retracted, and held to how many, how large, and how much in one
// conversation.

// attachment uploads a file for a message as actor does: a URL from
// conversation.upload_url, the bytes PUT to it, the token kept.
func (b *built) attachment(t *testing.T, actor uuid.UUID, contentType string, body []byte) string {
	t.Helper()
	u := testkit.Result[tools.AttachmentUploadURLOut](t, b.do(t, actor, "conversation.upload_url", m{"course_id": b.course, "content_type": contentType}))
	b.put(t, tools.UploadURLOut{UploadURL: u.UploadURL}, body)
	return u.UploadToken
}

// fetch follows a file's download_url the way a client would, and says the
// name it is saved under.
func (b *built) fetch(t *testing.T, url string) (body []byte, filename string) {
	t.Helper()
	_, name, err := b.Blob.RedeemDownload(strings.TrimPrefix(url, "http://lms.test"+blob.BlobPath))
	if err != nil {
		t.Fatalf("the download URL does not redeem: %v", err)
	}
	return b.download(t, url), name
}

func (b *built) attachmentOf(t *testing.T, actor, file uuid.UUID) tools.ConversationAttachmentOut {
	t.Helper()
	return testkit.Result[tools.ConversationAttachmentOut](t, b.do(t, actor, "conversation.attachment", m{"course_id": b.course, "attachment_id": file}))
}

// refusedAs insists a call failed, or was refused before it was attempted,
// with code and, when given, the reason its details say.
func (b *built) refusedAs(t *testing.T, actor uuid.UUID, name string, args m, code apperr.Code, why string) {
	t.Helper()
	b.key++
	out, err := b.Call(actor, name, args, "refused-"+uuid.NewString())
	var e *apperr.Error
	switch {
	case err != nil:
		var ok bool
		if e, ok = apperr.As(err); !ok {
			t.Fatalf("%s: %v", name, err)
		}
	case out.Error != nil:
		e = out.Error
	default:
		t.Fatalf("%s went through (%s), want %s %s", name, out.Status, code, why)
	}
	if e.Code != code || (why != "" && e.Details["reason"] != why) {
		t.Fatalf("%s: %+v, want %s %s", name, e, code, why)
	}
}

func files(views []tools.AttachmentView) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.Filename
	}
	return out
}

func TestAQuestionsFilesAreReadByWhoeverReadsTheConversation(t *testing.T) {
	b := build(t)
	essay, chart := []byte("%PDF-1.7 my essay"), []byte("\x89PNG a chart")
	opened := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open", m{"course_id": b.course,
		"respondent_member_id": b.tutorM, "body": "Is my essay on track?", "attachments": []m{
			{"upload_token": b.attachment(t, b.yuki, "application/pdf", essay), "filename": " essay draft.pdf "},
			{"upload_token": b.attachment(t, b.yuki, "image/png", chart), "filename": "圖 1.png"}}}))
	conv, question := opened.ConversationID, *opened.MessageID

	// The tutor it was asked reads the question and its files, in order.
	msgs := b.messages(t, b.tutor, conv)
	if len(msgs) != 1 || strings.Join(files(msgs[0].Attachments), "|") != "essay draft.pdf|圖 1.png" {
		t.Fatalf("the tutor reads %+v", msgs)
	}
	pdf := msgs[0].Attachments[0]
	if pdf.ContentType != "application/pdf" || pdf.ByteSize != int64(len(essay)) || pdf.Checksum == nil ||
		!strings.HasPrefix(*pdf.Checksum, "sha256:") || !pdf.CreatedAt.Equal(msgs[0].CreatedAt) {
		t.Fatalf("the essay, as the tutor is shown it: %+v", pdf)
	}
	got := b.attachmentOf(t, b.tutor, pdf.ID)
	if got.MessageID != question || got.ConversationID != conv || got.AuthorMemberID != b.yukiM || got.MessageSeq != 1 ||
		got.ExpiresAt.Before(time.Now().Add(10*time.Minute)) {
		t.Fatalf("conversation.attachment: %+v", got)
	}
	if body, name := b.fetch(t, got.DownloadURL); !bytes.Equal(body, essay) || name != "essay draft.pdf" {
		t.Fatalf("the tutor downloaded %q as %q", body, name)
	}

	// Its opener, and the instructor who oversees her, read it too.
	for _, reader := range []uuid.UUID{b.yuki, b.sato} {
		if body, _ := b.fetch(t, b.attachmentOf(t, reader, msgs[0].Attachments[1].ID).DownloadURL); !bytes.Equal(body, chart) {
			t.Fatal("a reader of the conversation downloaded something else")
		}
	}
	// To anyone else neither the conversation nor its files exist, and a
	// file of another course is not found in this one.
	for _, outsider := range []uuid.UUID{b.ken, b.grader} {
		b.refusedAs(t, outsider, "conversation.attachment", m{"course_id": b.course, "attachment_id": pdf.ID}, apperr.NotFound, "")
		b.try(t, outsider, "conversation.messages", m{"course_id": b.course, "conversation_id": conv}, apperr.NotFound)
	}
	b.refusedAs(t, b.yuki, "conversation.attachment", m{"course_id": b.course, "attachment_id": uuid.New()}, apperr.NotFound, "")

	// The tutor answers with a file, and she asks again with one.
	answer := []byte("worked example")
	b.do(t, b.tutor, "conversation.answer", m{"course_id": b.course, "conversation_id": conv, "in_reply_to_message_id": question,
		"body": "Mostly. See the worked example.", "attachments": []m{
			{"upload_token": b.attachment(t, b.tutor, "text/plain", answer), "filename": "example.txt"}}})
	b.do(t, b.yuki, "conversation.ask", m{"course_id": b.course, "conversation_id": conv, "body": "And this one?",
		"attachments": []m{{"upload_token": b.attachment(t, b.yuki, "application/pdf", essay), "filename": "essay v2.pdf"}}})
	msgs = b.messages(t, b.yuki, conv)
	if len(msgs) != 3 || strings.Join(files(msgs[1].Attachments), "|") != "example.txt" ||
		strings.Join(files(msgs[2].Attachments), "|") != "essay v2.pdf" {
		t.Fatalf("Yuki reads %+v", msgs)
	}
	if body, name := b.fetch(t, b.attachmentOf(t, b.yuki, msgs[1].Attachments[0].ID).DownloadURL); !bytes.Equal(body, answer) || name != "example.txt" {
		t.Fatalf("Yuki downloaded %q as %q", body, name)
	}
	// A message with no files lists none.
	b.ask(t, b.yuki, conv, "Thanks.")
	if msgs = b.messages(t, b.yuki, conv); msgs[3].Attachments != nil {
		t.Fatalf("a message with no files lists %+v", msgs[3].Attachments)
	}

	// The news of each message says what it carries, and nothing of where.
	var payload map[string]any
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload FROM event WHERE type = 'conversation.message_posted'
		AND payload->>'message_id' = $1`, question.String()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	news, _ := payload["attachments"].([]any)
	if len(news) != 2 {
		t.Fatalf("the question's news: %+v", payload)
	}
	first := news[0].(map[string]any)
	if first["id"] != pdf.ID.String() || first["filename"] != "essay draft.pdf" || first["content_type"] != "application/pdf" ||
		first["byte_size"] != float64(len(essay)) || len(first) != 4 {
		t.Fatalf("the news of the essay: %+v", first)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'conversation.message_posted' AND payload ? 'attachments'`); n != 3 {
		t.Fatalf("%d messages' news speak of files, want the three that carry them", n)
	}
	// Each file is where no upload URL reaches it, under conversations/.
	if n := b.Count(`SELECT count(*) FROM conversation_attachment WHERE conversation_id = $1 AND storage_key LIKE 'conversations/%'`, conv); n != 4 {
		t.Fatalf("%d files kept under conversations/", n)
	}
}

func TestARetractedMessagesFilesAreWithheldAndKept(t *testing.T) {
	b := build(t)
	opened := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open", m{"course_id": b.course,
		"respondent_member_id": b.tutorM, "body": "Oops, wrong file", "attachments": []m{
			{"upload_token": b.attachment(t, b.yuki, "application/pdf", []byte("my medical note")), "filename": "note.pdf"}}}))
	conv, question := opened.ConversationID, *opened.MessageID
	file := b.messages(t, b.yuki, conv)[0].Attachments[0].ID
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": question, "reason": "wrong file"})

	msgs := b.messages(t, b.tutor, conv)
	if msgs[0].Retracted == nil || msgs[0].Body != nil || msgs[0].Attachments != nil {
		t.Fatalf("the retracted message, as the tutor reads it: %+v", msgs[0])
	}
	// Its readers are told it was retracted; anyone else, that there is no such file.
	for _, reader := range []uuid.UUID{b.yuki, b.tutor, b.sato} {
		b.refusedAs(t, reader, "conversation.attachment", m{"course_id": b.course, "attachment_id": file}, apperr.NotFound, "retracted")
	}
	b.refusedAs(t, b.ken, "conversation.attachment", m{"course_id": b.course, "attachment_id": file}, apperr.NotFound, "")
	// The row and the file are kept, as the text is kept in its action.
	var key string
	if err := b.Pool.QueryRow(t.Context(), `SELECT storage_key FROM conversation_attachment WHERE id = $1`, file).Scan(&key); err != nil {
		t.Fatalf("the retracted message's file is not kept: %v", err)
	}
	if _, err := b.Blob.Stat(context.Background(), key); err != nil {
		t.Fatalf("the file is gone from the store: %v", err)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE action_type = 'conversation.open' AND payload->'attachments'->0->>'filename' = 'note.pdf'`); n != 1 {
		t.Fatal("the action that wrote the message does not record what it carried")
	}
}

func TestAMessagesFilesAreHeldToWhatAMessageMayCarry(t *testing.T) {
	b := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) {
		d.Attachments = tools.AttachmentLimits{MaxBytes: 16, PerMessage: 2, ConversationBytes: 40}
	}))
	conv, _ := b.open(t, b.yuki, b.tutorM, "A question")
	ask := func(files ...m) m {
		return m{"course_id": b.course, "conversation_id": conv, "body": "With files", "attachments": files}
	}
	file := func(size int, name string) m {
		return m{"upload_token": b.attachment(t, b.yuki, "application/octet-stream", bytes.Repeat([]byte("x"), size)), "filename": name}
	}
	u := testkit.Result[tools.AttachmentUploadURLOut](t, b.do(t, b.yuki, "conversation.upload_url", m{"course_id": b.course, "content_type": "text/plain"}))
	if u.MaxBytes != 16 || u.MaxFiles != 2 || u.MaxConversationBytes != 40 {
		t.Fatalf("the limits a front end is told: %+v", u)
	}

	messages := func() int {
		return b.Count(`SELECT count(*) FROM conversation_message WHERE conversation_id = $1`, conv)
	}
	before := messages()
	b.refusedAs(t, b.yuki, "conversation.ask", ask(file(1, "a"), file(1, "b"), file(1, "c")), apperr.InvalidArgument, "too_many_attachments")
	big := file(17, "big.bin")
	b.refusedAs(t, b.yuki, "conversation.ask", ask(big), apperr.FailedPrecondition, "file_too_large")
	// A file too large is removed as it is refused, and its token attaches nothing after.
	b.refusedAs(t, b.yuki, "conversation.ask", ask(big), apperr.FailedPrecondition, "not_uploaded")
	for _, name := range []string{"", "  ", "notes/today.txt", `C:\notes.txt`, "two\nlines.txt", "\u202egpj.exe", strings.Repeat("x", 256)} {
		b.refusedAs(t, b.yuki, "conversation.ask", ask(file(1, name)), apperr.InvalidArgument, "bad_filename")
	}
	same := file(1, "a")
	b.refusedAs(t, b.yuki, "conversation.ask", ask(same, same), apperr.InvalidArgument, "duplicate_attachment")
	b.refusedAs(t, b.yuki, "conversation.open", m{"course_id": b.course, "respondent_member_id": b.tutorM,
		"attachments": []m{file(1, "a")}}, apperr.InvalidArgument, "attachments_need_body")
	if messages() != before {
		t.Fatal("a refused message was written")
	}

	// What is not the caller's own upload for a message is not attached.
	kens := m{"upload_token": b.attachment(t, b.ken, "text/plain", []byte("Ken's")), "filename": "kens.txt"}
	b.refusedAs(t, b.yuki, "conversation.ask", ask(kens), apperr.Forbidden, "not_your_upload")
	draft := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3})).SubmissionID
	essay := b.upload(t, b.yuki, "submission", "application/pdf", []byte("essay"))
	b.refusedAs(t, b.yuki, "conversation.ask", ask(m{"upload_token": essay, "filename": "essay.pdf"}), apperr.Forbidden, "not_your_upload")
	b.refusedAs(t, b.yuki, "document.create", m{"course_id": b.course, "kind": "submission", "title": "essay.pdf", "submission_id": draft,
		"upload_token": file(1, "x")["upload_token"]}, apperr.Forbidden, "not_your_upload")
	b.refusedAs(t, b.yuki, "conversation.ask", ask(m{"upload_token": "not-a-token", "filename": "x"}), apperr.InvalidArgument, "bad_upload_token")
	never := testkit.Result[tools.AttachmentUploadURLOut](t, b.do(t, b.yuki, "conversation.upload_url", m{"course_id": b.course, "content_type": "text/plain"}))
	b.refusedAs(t, b.yuki, "conversation.ask", ask(m{"upload_token": never.UploadToken, "filename": "x"}), apperr.FailedPrecondition, "not_uploaded")

	// Two of 16 bytes; a third would take the conversation past 40.
	sent := file(16, "one")
	b.do(t, b.yuki, "conversation.ask", ask(sent, file(16, "two")))
	b.refusedAs(t, b.yuki, "conversation.ask", ask(sent), apperr.Conflict, "already_attached")
	b.refusedAs(t, b.yuki, "conversation.ask", ask(file(16, "three")), apperr.FailedPrecondition, "conversation_attachments_full")
	b.do(t, b.yuki, "conversation.ask", ask(file(8, "fits")))
	// A retracted message's files are kept, and still count.
	last := b.messages(t, b.yuki, conv)
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": last[len(last)-1].ID})
	b.refusedAs(t, b.yuki, "conversation.ask", ask(file(1, "one more")), apperr.FailedPrecondition, "conversation_attachments_full")
	// A new conversation starts with room of its own.
	b.do(t, b.yuki, "conversation.open", m{"course_id": b.course, "respondent_member_id": b.tutorM, "body": "Once more",
		"attachments": []m{file(16, "a"), file(16, "b")}})
}

// The limit on a file is never more than what this server's own disk takes
// as a file arrives, MAX_UPLOAD_BYTES.
func TestAFileIsNeverLargerThanAnUpload(t *testing.T) {
	b := build(t)
	u := testkit.Result[tools.AttachmentUploadURLOut](t, b.do(t, b.yuki, "conversation.upload_url", m{"course_id": b.course, "content_type": "text/plain"}))
	if u.MaxBytes != testkit.MaxUploadBytes || u.MaxFiles != tools.DefaultAttachmentsPerMessage ||
		u.MaxConversationBytes != tools.DefaultAttachmentConversationBytes {
		t.Fatalf("the limits, by default: %+v", u)
	}
	if !strings.HasPrefix(u.Headers["Content-Type"], "text/plain") {
		t.Fatalf("the headers the PUT must carry: %+v", u.Headers)
	}
}

func TestWhoGetsSomewhereToUploadAFileForAMessage(t *testing.T) {
	b := build(t)
	// Whoever asks or answers: a student, an agent that answers.
	for _, who := range []uuid.UUID{b.yuki, b.tutor} {
		b.do(t, who, "conversation.upload_url", m{"course_id": b.course, "content_type": "application/pdf"})
	}
	// Not someone who does neither, nor anyone outside the course.
	b.refusedAs(t, b.grader, "conversation.upload_url", m{"course_id": b.course, "content_type": "application/pdf"}, apperr.Forbidden, "permission_denied")
	outsider := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Outsider"})).ActorID
	if out := b.MustCall(outsider, "conversation.upload_url", m{"course_id": b.course, "content_type": "application/pdf"}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("someone with no seat: %+v", out)
	}
	b.refusedAs(t, b.yuki, "conversation.upload_url", m{"course_id": b.course, "content_type": " "}, apperr.InvalidArgument, "")
	// An archived course takes no new files.
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	b.refusedAs(t, b.yuki, "conversation.upload_url", m{"course_id": b.course, "content_type": "application/pdf"}, apperr.Forbidden, "course_archived")

	// And an installation with nowhere to keep files takes none.
	none := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.Blob = nil }))
	none.refusedAs(t, none.yuki, "conversation.upload_url", m{"course_id": none.course, "content_type": "application/pdf"}, apperr.FailedPrecondition, "no_file_storage")
}

// An answer that waits for a person's approval carries its files into the
// proposal by their tokens, and attaches them when it is approved; a
// proposal is not made about an upload it could outlive.
func TestAProposedAnswerKeepsItsFilesUntilItIsDecided(t *testing.T) {
	b := build(t)
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.tutorM, "perms": m{"conversation_answer": "confirm_required"}})
	conv, question := b.open(t, b.yuki, b.tutorM, "Can you mark this up?")
	answer := func(key string, token string) pipeline.Outcome {
		t.Helper()
		return b.MustCall(b.tutor, "conversation.answer", m{"course_id": b.course, "conversation_id": conv, "in_reply_to_message_id": question,
			"body": "Marked up.", "attachments": []m{{"upload_token": token, "filename": "marked.pdf"}}}, key)
	}

	// An upload more than two days old may be gone before anyone decides.
	old := b.attachment(t, b.tutor, "application/pdf", []byte("marked, long ago"))
	b.P.SetClock(func() time.Time { return time.Now().Add(tools.OrphanGrace + time.Hour) })
	out := answer("old", old)
	b.P.SetClock(time.Now)
	if out.Status != domain.StatusFailed || reason(out) != "upload_too_old" {
		t.Fatalf("a proposal naming an old upload: %+v", out)
	}

	marked := []byte("%PDF marked up")
	fresh := b.attachment(t, b.tutor, "application/pdf", marked)
	proposed := answer("fresh", fresh)
	if proposed.Status != domain.StatusProposed {
		t.Fatalf("the tutor's answer: %+v", proposed)
	}
	if n := b.Count(`SELECT count(*) FROM conversation_attachment`); n != 0 {
		t.Fatal("a proposed answer's file was attached before anyone decided")
	}
	decided := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": proposed.ActionID, "decision": "approve"}))
	if decided.Outcome != domain.StatusExecuted {
		t.Fatalf("approving the answer: %+v", decided)
	}
	msgs := b.messages(t, b.yuki, conv)
	if len(msgs) != 2 || len(msgs[1].Attachments) != 1 {
		t.Fatalf("Yuki reads %+v", msgs)
	}
	if body, name := b.fetch(t, b.attachmentOf(t, b.yuki, msgs[1].Attachments[0].ID).DownloadURL); !bytes.Equal(body, marked) || name != "marked.pdf" {
		t.Fatalf("Yuki downloaded %q as %q", body, name)
	}
	// Replayed by its key, the answer attaches nothing twice.
	if again := answer("fresh", fresh); !again.Replayed || again.Status != domain.StatusExecuted {
		t.Fatalf("the answer replayed: %+v", again)
	}
	if n := b.Count(`SELECT count(*) FROM conversation_attachment`); n != 1 {
		t.Fatalf("%d files after a replay", n)
	}
}

// A question the opener asks by way of a proposal carries its files the same
// way, and replaying the call that wrote a message attaches nothing twice.
func TestAMessagesFilesAreOneWithItsAction(t *testing.T) {
	b := build(t)
	conv, _ := b.open(t, b.yuki, b.tutorM, "First")
	args := m{"course_id": b.course, "conversation_id": conv, "body": "With a file",
		"attachments": []m{{"upload_token": b.attachment(t, b.yuki, "text/plain", []byte("notes")), "filename": "notes.txt"}}}
	first := b.MustCall(b.yuki, "conversation.ask", args, "ask-once")
	again := b.MustCall(b.yuki, "conversation.ask", args, "ask-once")
	if first.Status != domain.StatusExecuted || !again.Replayed ||
		testkit.Result[tools.MessageIDOut](t, first).MessageID != testkit.Result[tools.MessageIDOut](t, again).MessageID {
		t.Fatalf("the ask and its replay: %+v %+v", first, again)
	}
	if n := b.Count(`SELECT count(*) FROM conversation_attachment WHERE conversation_id = $1`, conv); n != 1 {
		t.Fatalf("%d files after a replay", n)
	}
	var recorded struct {
		Attachments []tools.AttachmentIn `json:"attachments"`
	}
	var payload []byte
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload FROM action WHERE id = $1`, first.ActionID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &recorded); err != nil || len(recorded.Attachments) != 1 || recorded.Attachments[0].Filename != "notes.txt" {
		t.Fatalf("the action records %s", payload)
	}

	// The same key with other files is another request, and refused.
	args["attachments"] = []m{{"upload_token": b.attachment(t, b.yuki, "text/plain", []byte("other")), "filename": "other.txt"}}
	if _, err := b.Call(b.yuki, "conversation.ask", args, "ask-once"); !apperr.Is(err, apperr.IdempotencyConflict) {
		t.Fatalf("the key reused with other files: %v", err)
	}
}

// In the site's chat, a person asks their own agent, which what runs it says
// takes conversations there, with a file; the agent reads it, and it is in
// her panel's conversation as in any other.
func TestAPersonsOwnAgentReadsTheFileItIsAskedWith(t *testing.T) {
	b := build(t)
	helper := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, helper, m{})
	b.SiteChat(helper)
	plan := []byte("week 1: loops")
	opened := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open", m{"course_id": b.course,
		"respondent_member_id": seat, "body": "Plan my week", "attachments": []m{
			{"upload_token": b.attachment(t, b.yuki, "text/plain", plan), "filename": "plan.txt"}}}))
	if mine := testkit.Result[tools.MyConversationsOut](t, b.do(t, b.yuki, "me.conversations", m{})).Conversations; len(mine) != 1 ||
		mine[0].ConversationID != opened.ConversationID {
		t.Fatalf("Yuki's panel: %+v", mine)
	}
	msgs := b.messages(t, helper, opened.ConversationID)
	if len(msgs) != 1 || len(msgs[0].Attachments) != 1 {
		t.Fatalf("her agent reads %+v", msgs)
	}
	if body, name := b.fetch(t, b.attachmentOf(t, helper, msgs[0].Attachments[0].ID).DownloadURL); !bytes.Equal(body, plan) || name != "plan.txt" {
		t.Fatalf("her agent downloaded %q as %q", body, name)
	}
	// Her instructor, who oversees what she asks, reads it; another student does not.
	b.attachmentOf(t, b.sato, msgs[0].Attachments[0].ID)
	b.refusedAs(t, b.ken, "conversation.attachment", m{"course_id": b.course, "attachment_id": msgs[0].Attachments[0].ID}, apperr.NotFound, "")
}
