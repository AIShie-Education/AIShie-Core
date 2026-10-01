package httpapi_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// A file a student's question carries, over HTTP all the way: a URL from
// the tool, the bytes PUT with no credential but the URL, the question
// written naming it; the agent asked reads the message, gets a URL for the
// file and downloads it under its name, as a download; another student
// finds nothing; and once the question is withdrawn, its file is withheld.
func TestAMessagesFileOverHTTP(t *testing.T) {
	a := newAPI(t, 2)
	c := a.c
	tutor := c.RuntimeAgent("Course tutor")
	tutorM := c.Member(c.Course, tutor, "course_tutor")
	_, agent := c.HostToken(tutor)
	yuki, ken := a.tokenFor(c.Students[0].Actor), a.tokenFor(c.Students[1].Actor)
	course := "/v1/courses/" + c.Course.String()

	upload := func(contentType string, body []byte) string {
		t.Helper()
		ask := a.do(nil, "GET", course+"/conversations/upload-url?content_type="+contentType, yuki, nil)
		if ask.Status != 200 || ask.Body["result"].(m)["max_files"] != float64(10) {
			t.Fatalf("conversations/upload-url: %d %s", ask.Status, ask.Raw)
		}
		if res, got := a.raw("PUT", a.here(ask.str("result", "upload_url")), contentType, body); res.StatusCode != 200 {
			t.Fatalf("PUT: %d %s", res.StatusCode, got)
		}
		return ask.str("result", "upload_token")
	}
	pdf, notes := []byte("%PDF-1.7 my essay"), []byte("讀書筆記")
	opened := a.do(nil, "POST", course+"/conversations", yuki, m{"respondent_member_id": tutorM, "body": "Is this on track?",
		"attachments": []m{{"upload_token": upload("application/pdf", pdf), "filename": "essay 1.pdf"},
			{"upload_token": upload("text/plain", notes), "filename": "筆記.txt"}}}, "Idempotency-Key", "open-1")
	if opened.Status != 200 {
		t.Fatalf("conversation.open: %d %s", opened.Status, opened.Raw)
	}
	conv, question := opened.str("result", "conversation_id"), opened.str("result", "message_id")

	read := a.do(nil, "GET", course+"/conversations/"+conv+"/messages", agent, nil)
	if read.Status != 200 {
		t.Fatalf("conversation.messages: %d %s", read.Status, read.Raw)
	}
	files := read.Body["result"].(m)["messages"].([]any)[0].(m)["attachments"].([]any)
	if len(files) != 2 || files[0].(m)["filename"] != "essay 1.pdf" || files[0].(m)["byte_size"] != float64(len(pdf)) ||
		files[0].(m)["content_type"] != "application/pdf" || files[0].(m)["storage_key"] != nil || files[0].(m)["download_url"] != nil {
		t.Fatalf("the question's files, as the tutor reads them: %s", read.Raw)
	}
	for i, want := range []struct {
		body        []byte
		contentType string
		disposition string
	}{
		{pdf, "application/pdf", `attachment; filename="essay 1.pdf"`},
		{notes, "text/plain", "attachment; filename*=utf-8''%E7%AD%86%E8%A8%98.txt"},
	} {
		file := files[i].(m)["id"].(string)
		got := a.do(nil, "GET", course+"/conversation-attachments/"+file, agent, nil)
		if got.Status != 200 || got.str("result", "message_id") != question || got.str("result", "conversation_id") != conv {
			t.Fatalf("conversation.attachment: %d %s", got.Status, got.Raw)
		}
		res, body := a.raw("GET", a.here(got.str("result", "download_url")), "", nil)
		if res.StatusCode != 200 || !bytes.Equal(body, want.body) || res.Header.Get("Content-Type") != want.contentType {
			t.Fatalf("download: %d %q %q", res.StatusCode, res.Header.Get("Content-Type"), body)
		}
		// Someone else's bytes from the API's own origin: a download, saved
		// under its name, never a page.
		if res.Header.Get("Content-Disposition") != want.disposition || res.Header.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") {
			t.Fatalf("download headers: %v", res.Header)
		}
	}
	file := files[0].(m)["id"].(string)

	// Another student finds neither the conversation nor its file.
	if r := a.do(nil, "GET", course+"/conversation-attachments/"+file, ken, nil); r.Status != 404 || r.str("error", "code") != "not_found" {
		t.Fatalf("Ken asking for Yuki's file: %d %s", r.Status, r.Raw)
	}
	if r := a.do(nil, "GET", course+"/conversations/"+conv+"/messages", ken, nil); r.Status != 404 {
		t.Fatalf("Ken reading Yuki's conversation: %d %s", r.Status, r.Raw)
	}

	// Past the limits, the refusal says why; past their number, as the
	// call is read, recording nothing.
	var many []m
	for i := range 11 {
		many = append(many, m{"upload_token": upload("text/plain", []byte("x")), "filename": fmt.Sprintf("%d.txt", i)})
	}
	if r := a.do(nil, "POST", course+"/conversations/"+conv+"/ask", yuki, m{"body": "Eleven files", "attachments": many},
		"Idempotency-Key", "ask-11"); r.Status != 400 || r.str("action_id") != "" || r.str("error", "details", "reason") != "too_many_attachments" {
		t.Fatalf("eleven files: %d %s", r.Status, r.Raw)
	}
	big := a.do(nil, "GET", course+"/conversations/upload-url?content_type=application/zip", yuki, nil)
	if res, _ := a.raw("PUT", a.here(big.str("result", "upload_url")), "application/zip", bytes.Repeat([]byte("z"), 1<<16+1)); res.StatusCode != 400 {
		t.Fatalf("an oversized PUT: %d", res.StatusCode)
	}
	if r := a.do(nil, "POST", course+"/conversations/"+conv+"/ask", yuki, m{"body": "The big one",
		"attachments": []m{{"upload_token": big.str("result", "upload_token"), "filename": "big.zip"}}}, "Idempotency-Key", "ask-big"); r.Status != 422 ||
		r.str("error", "details", "reason") != "not_uploaded" {
		t.Fatalf("attaching an upload that was refused: %d %s", r.Status, r.Raw)
	}

	// Withdrawn, the question keeps its file from its readers, as its text.
	if r := a.do(nil, "POST", course+"/conversation-messages/"+question+"/retract", yuki, m{}, "Idempotency-Key", "retract-1"); r.Status != 200 {
		t.Fatalf("conversation.retract: %d %s", r.Status, r.Raw)
	}
	if r := a.do(nil, "GET", course+"/conversation-attachments/"+file, agent, nil); r.Status != 404 || r.str("error", "details", "reason") != "retracted" {
		t.Fatalf("the tutor asking for a retracted message's file: %d %s", r.Status, r.Raw)
	}
}
