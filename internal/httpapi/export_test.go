package httpapi_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
)

// An export of conversations over HTTP all the way: root exports a course's,
// is given a URL for each of its two files, and downloads each with no
// credential but the URL, as a download under its name, never a page; the
// retracted question is in it, with its text, marked; replayed, the export
// gives no URL, and its file is given again by its own route; the
// instructor, a student and an agent are refused, on record.
func TestAConversationExportOverHTTP(t *testing.T) {
	a := newAPI(t, 2)
	c := a.c
	tutor := c.Actor("agent", "Course tutor")
	tutorM := c.Member(c.Course, tutor, "course_tutor")
	c.SiteChat(tutor)
	root, sato, yuki, agent := a.tokenFor(c.Root), a.tokenFor(c.Sato), a.tokenFor(c.Students[0].Actor), a.tokenFor(tutor)
	course := "/v1/courses/" + c.Course.String()

	opened := a.do(nil, "POST", course+"/conversations", yuki, m{"respondent_member_id": tutorM, "body": "這份作業怎麼寫？"},
		"Idempotency-Key", "open-1")
	if opened.Status != 200 {
		t.Fatalf("conversation.open: %d %s", opened.Status, opened.Raw)
	}
	conv, question := opened.str("result", "conversation_id"), opened.str("result", "message_id")
	if r := a.do(nil, "POST", course+"/conversations/"+conv+"/answer", agent, m{"in_reply_to_message_id": question, "body": "先讀題目。"},
		"Idempotency-Key", "answer-1"); r.Status != 200 {
		t.Fatalf("conversation.answer: %d %s", r.Status, r.Raw)
	}
	asked := a.do(nil, "POST", course+"/conversations/"+conv+"/ask", yuki, m{"body": "不用了"}, "Idempotency-Key", "ask-1")
	if asked.Status != 200 {
		t.Fatalf("conversation.ask: %d %s", asked.Status, asked.Raw)
	}
	if r := a.do(nil, "POST", course+"/conversation-messages/"+asked.str("result", "message_id")+"/retract", yuki, m{"reason": "問錯了"},
		"Idempotency-Key", "retract-1"); r.Status != 200 {
		t.Fatalf("conversation.retract: %d %s", r.Status, r.Raw)
	}

	made := a.do(nil, "POST", "/v1/conversation-exports", root, m{"course_id": c.Course}, "Idempotency-Key", "export-1")
	if made.Status != 200 || made.str("status") != "executed" || made.str("action_id") != made.str("result", "export_id") {
		t.Fatalf("conversation.export: %d %s", made.Status, made.Raw)
	}
	result := made.Body["result"].(m)
	if result["conversations"] != 1.0 || result["messages"] != 3.0 || result["retracted"] != 1.0 {
		t.Fatalf("what the export holds: %s", made.Raw)
	}
	export := made.str("result", "export_id")
	downloads := result["downloads"].([]any)
	if len(downloads) != 2 {
		t.Fatalf("the export's downloads: %s", made.Raw)
	}
	got := map[string][]byte{}
	for _, d := range downloads {
		format, url := d.(m)["format"].(string), d.(m)["download_url"].(string)
		res, body := a.raw("GET", a.here(url), "", nil)
		if res.StatusCode != 200 {
			t.Fatalf("download %s: %d %s", format, res.StatusCode, body)
		}
		name := map[string]string{"jsonl": "conversations-" + export + ".jsonl", "csv": "messages-" + export + ".csv"}[format]
		if res.Header.Get("Content-Disposition") != `attachment; filename=`+name || res.Header.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") || res.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("download %s headers: %v", format, res.Header)
		}
		got[format] = body
	}
	var line struct {
		ID       string `json:"id"`
		Messages []struct {
			Body      string `json:"body"`
			Retracted *struct {
				Reason string `json:"reason"`
			} `json:"retracted"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(got["jsonl"]), &line); err != nil || line.ID != conv || len(line.Messages) != 3 ||
		line.Messages[2].Body != "不用了" || line.Messages[2].Retracted == nil || line.Messages[2].Retracted.Reason != "問錯了" {
		t.Fatalf("the conversations file: %v %s", err, got["jsonl"])
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(got["csv"], []byte("\xef\xbb\xbf")))).ReadAll()
	if !bytes.HasPrefix(got["csv"], []byte("\xef\xbb\xbf")) || err != nil || len(rows) != 4 || rows[3][6] != "retracted" || rows[3][17] != "不用了" {
		t.Fatalf("the messages file: %v %q", err, got["csv"])
	}

	// Replayed, the same export, and no URL; its file is given again.
	again := a.do(nil, "POST", "/v1/conversation-exports", root, m{"course_id": c.Course}, "Idempotency-Key", "export-1")
	if again.Status != 200 || again.Header.Get("Idempotency-Replayed") != "true" || again.str("result", "export_id") != export ||
		again.Body["result"].(m)["downloads"] != nil {
		t.Fatalf("the export replayed: %d %s", again.Status, again.Raw)
	}
	file := a.do(nil, "GET", "/v1/conversation-exports/"+export+"/csv", root, nil)
	if file.Status != 200 || file.str("result", "format") != "csv" || file.str("result", "download_url") == "" {
		t.Fatalf("conversation.export_file: %d %s", file.Status, file.Raw)
	}
	if res, body := a.raw("GET", a.here(file.str("result", "download_url")), "", nil); res.StatusCode != 200 || !bytes.Equal(body, got["csv"]) {
		t.Fatalf("the messages file again: %d", res.StatusCode)
	}

	// Nobody else exports, and each refusal is on record.
	for who, token := range map[string]string{"the instructor": sato, "a student": yuki, "an agent": agent} {
		r := a.do(nil, "POST", "/v1/conversation-exports", token, m{"course_id": c.Course}, "Idempotency-Key", "export-"+who)
		if r.Status != 403 || r.str("status") != "denied" || r.str("error", "details", "reason") != "platform_role_required" || r.str("action_id") == "" {
			t.Fatalf("%s exporting: %d %s", who, r.Status, r.Raw)
		}
	}
	if r := a.do(nil, "GET", "/v1/conversation-exports/"+export+"/csv", sato, nil); r.Status != 403 {
		t.Fatalf("the instructor fetching root's export: %d %s", r.Status, r.Raw)
	}
}
