package tools_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Exporting conversations for audit: root and the platform's
// administrators export any, a department's administrators those of the
// courses beneath their appointments, and nobody else, never an agent. An
// export holds every message of what it chooses, a retracted one with its
// text, marked; what files each carries, never their bytes; the answers and
// questions proposed and never posted; and no token. It is an action, its
// filters and counts on record, and its files are given by URL alone.

// What is said in the conversations audit makes.
const (
	essayQuestion   = "我的論文方向對嗎？"
	essayFile       = "論文 草稿.pdf"
	formulaAnswer   = "=SUM(A1) 方向正確，再補一個例子。"
	wrongQuestion   = "第二個問題：考試範圍？"
	retractedWhy    = "問錯了"
	kenQuestion     = "What does HW3 ask for?"
	rejectedAnswer  = "A very long answer nobody approved."
	rejectedWhy     = "too long"
	waitingAnswer   = "An essay with a thesis; see the notes."
	waitingFile     = "notes.txt"
	botQuestion     = "Remind me what I was working on."
	botAnswer       = "Your HW3 essay."
	historyQuestion = "When was the treaty signed?"
)

// essayBytes are the bytes of Yuki's file, which no export holds.
var essayBytes = []byte("%PDF-1.7 the bytes of Yuki's essay, which are hers")

// audit is a site with conversations to export. In CS101, in Computer
// Science under Engineering: Yuki's with the tutor (c1) ten days ago, her
// essay attached, in which she retracts a second question; Ken's with Sato's
// course tutor (c2) five days ago, whose answers wait for approval, one
// rejected and one waiting with a file; and Yuki's with her own agent (c3)
// yesterday. In HIS101, in History, a department of its own, Lin's with its
// tutor (c4). Carol administers Computer Science, Ada Engineering, Dan
// History.
type audit struct {
	*built
	engineering, history            uuid.UUID
	carol, ada, dan                 uuid.UUID
	courseTutor, courseTutorM       uuid.UUID
	bot, botM                       uuid.UUID
	his101, lin                     uuid.UUID
	c1, c2, c3, c4                  uuid.UUID
	q1, q2, kenQ                    uuid.UUID
	rejected, waiting               uuid.UUID
	uploadTokens                    []string
	tenDaysAgo, fiveDaysAgo, oneAgo time.Time
	clock                           *time.Time
}

func newAudit(t *testing.T) *audit {
	t.Helper()
	return auditOn(t, testkit.NewPlatform(t))
}

func auditOn(t *testing.T, p *testkit.Platform) *audit {
	t.Helper()
	a := &audit{built: buildOn(t, p)}
	b := a.built
	now := time.Now()
	clock := now
	a.clock = &clock
	b.P.SetClock(func() time.Time { return *a.clock })
	a.tenDaysAgo, a.fiveDaysAgo, a.oneAgo = now.Add(-10*24*time.Hour), now.Add(-5*24*time.Hour), now.Add(-24*time.Hour)

	// The departments, and who administers which.
	a.engineering = testkit.Result[tools.IDOut](t, b.do(t, b.admin, "department.create", m{"name": "Engineering"})).ID
	b.do(t, b.admin, "department.move", m{"dept_id": b.dept, "parent_id": a.engineering})
	a.history = testkit.Result[tools.IDOut](t, b.do(t, b.admin, "department.create", m{"name": "History"})).ID
	person := func(name string) uuid.UUID {
		return testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": name})).ActorID
	}
	a.carol, a.ada, a.dan = person("Carol"), person("Ada"), person("Dan")
	b.Appoint(b.dept, a.carol, b.Root)
	b.Appoint(a.engineering, a.ada, b.Root)
	b.Appoint(a.history, a.dan, b.Root)

	// Sato's course tutor, whose answers wait for a person's approval, and
	// Yuki's own agent; something runs each, and says so.
	a.courseTutor = b.agent(t, b.sato, "Course tutor")
	a.courseTutorM = b.delegate(t, b.sato, a.courseTutor, m{"preset": "course_tutor", "perms": m{"conversation_answer": "confirm_required"}})
	a.bot = b.agent(t, b.yuki, "Yuki's helper")
	a.botM = b.delegate(t, b.yuki, a.bot, m{})
	b.SiteChat(a.courseTutor)
	b.SiteChat(a.bot)

	// c1, ten days ago: Yuki asks the tutor, her essay attached; it
	// answers; she asks again, and takes it back.
	*a.clock = a.tenDaysAgo
	token := b.attachment(t, b.yuki, "application/pdf", essayBytes)
	a.uploadTokens = append(a.uploadTokens, token)
	opened := testkit.Result[tools.ConversationOpenOut](t, b.do(t, b.yuki, "conversation.open", m{"course_id": b.course,
		"respondent_member_id": b.tutorM, "body": essayQuestion, "attachments": []m{{"upload_token": token, "filename": essayFile}}}))
	a.c1, a.q1 = opened.ConversationID, *opened.MessageID
	b.do(t, b.tutor, "conversation.answer", answerArgs(b, a.c1, a.q1, formulaAnswer))
	a.q2 = b.ask(t, b.yuki, a.c1, wrongQuestion)
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": a.q2, "reason": retractedWhy})

	// c2, five days ago: Ken asks the course tutor. Its first answer is
	// rejected; its second, with a file, an hour later, waits.
	*a.clock = a.fiveDaysAgo
	a.c2, a.kenQ = b.open(t, b.ken, a.courseTutorM, kenQuestion)
	first := b.MustCall(a.courseTutor, "conversation.answer", answerArgs(b, a.c2, a.kenQ, rejectedAnswer), "answer-1")
	if first.Status != domain.StatusProposed {
		t.Fatalf("the course tutor's answer: %+v", first)
	}
	a.rejected = *first.ActionID
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": a.rejected, "decision": "reject", "reason": rejectedWhy})
	*a.clock = a.fiveDaysAgo.Add(time.Hour)
	notes := b.attachment(t, a.courseTutor, "text/plain", []byte("notes for Ken"))
	a.uploadTokens = append(a.uploadTokens, notes)
	args := answerArgs(b, a.c2, a.kenQ, waitingAnswer)
	args["attachments"] = []m{{"upload_token": notes, "filename": waitingFile}}
	second := b.MustCall(a.courseTutor, "conversation.answer", args, "answer-2")
	if second.Status != domain.StatusProposed {
		t.Fatalf("the course tutor's second answer: %+v", second)
	}
	a.waiting = *second.ActionID

	// c3, yesterday: Yuki asks her own agent, which answers.
	*a.clock = a.oneAgo
	var botQ uuid.UUID
	a.c3, botQ = b.open(t, b.yuki, a.botM, botQuestion)
	b.do(t, a.bot, "conversation.answer", answerArgs(b, a.c3, botQ, botAnswer))

	// c4, yesterday, in History.
	a.his101 = testkit.Result[tools.CourseCreateOut](t, b.do(t, b.admin, "course.create",
		m{"dept_id": a.history, "term_id": b.term, "code": "HIS101", "title": "Modern History"})).CourseID
	b.do(t, b.admin, "course.activate", m{"course_id": a.his101})
	a.lin = person("Lin")
	linM := b.Member(a.his101, a.lin, "student")
	hisTutor := b.Actor("agent", "History tutor")
	hisTutorM := b.Member(a.his101, hisTutor, "course_tutor")
	b.SiteChat(hisTutor)
	a.c4 = testkit.Result[tools.ConversationOpenOut](t, b.do(t, a.lin, "conversation.open",
		m{"course_id": a.his101, "respondent_member_id": hisTutorM, "body": historyQuestion})).ConversationID
	_ = linM

	*a.clock = now
	return a
}

// export exports as actor and insists it was carried out.
func (a *audit) export(t *testing.T, actor uuid.UUID, args m) (tools.ConversationExportOut, pipeline.Outcome) {
	t.Helper()
	out := a.do(t, actor, "conversation.export", args)
	return testkit.Result[tools.ConversationExportOut](t, out), out
}

// exported is what an export's two files hold: the conversations, by id,
// as the JSON Lines file has them, and the CSV file's rows, its header
// first, as a spreadsheet reads them.
type exported struct {
	lines []map[string]any
	byID  map[string]map[string]any
	rows  [][]string
	jsonl []byte
	csv   []byte
}

func (a *audit) files(t *testing.T, out tools.ConversationExportOut) exported {
	t.Helper()
	if len(out.Downloads) != 2 || len(out.Files) != 2 {
		t.Fatalf("an export's files: %+v", out)
	}
	var x exported
	for i, d := range out.Downloads {
		body, name := a.fetch(t, d.DownloadURL)
		file := out.Files[i]
		if file.Format != d.Format || name != file.Filename || file.ByteSize != int64(len(body)) || !strings.HasPrefix(file.Checksum, "sha256:") ||
			d.ExpiresAt.Before(time.Now().Add(10*time.Minute)) {
			t.Fatalf("file %d, %q downloaded as %q: %+v %+v", i, d.Format, name, file, d)
		}
		switch d.Format {
		case tools.ExportJSONL:
			x.jsonl = body
		case tools.ExportCSV:
			x.csv = body
		}
	}
	if out.Files[0].Filename != "conversations-"+out.ExportID.String()+".jsonl" || out.Files[1].Filename != "messages-"+out.ExportID.String()+".csv" ||
		out.Files[0].ContentType != "application/x-ndjson" || !strings.HasPrefix(out.Files[1].ContentType, "text/csv") {
		t.Fatalf("the files are named and typed %+v", out.Files)
	}
	x.byID = map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSuffix(string(x.jsonl), "\n"), "\n") {
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("a line that is no JSON: %v\n%s", err, line)
		}
		x.lines = append(x.lines, v)
		x.byID[v["id"].(string)] = v
	}
	if !bytes.HasPrefix(x.csv, []byte("\xef\xbb\xbf")) {
		t.Fatalf("the CSV file does not begin with a byte order mark: %q", x.csv[:min(len(x.csv), 16)])
	}
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(x.csv, []byte("\xef\xbb\xbf"))))
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("the CSV file: %v", err)
	}
	x.rows = rows
	return x
}

// ids are the conversations an export holds, in its order.
func (x exported) ids() []uuid.UUID {
	out := make([]uuid.UUID, len(x.lines))
	for i, l := range x.lines {
		out[i] = uuid.MustParse(l["id"].(string))
	}
	return out
}

// column is the named column of a CSV row.
func (x exported) column(row []string, name string) string {
	for i, h := range x.rows[0] {
		if h == name {
			return row[i]
		}
	}
	return "<no column " + name + ">"
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func obj(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}

func TestAnExportHoldsEveryMessageRetractedOnesMarkedAndNoBytesOrTokens(t *testing.T) {
	a := newAudit(t)
	out, done := a.export(t, a.Root, m{"course_id": a.course})
	if out.ExportID != *done.ActionID || out.Conversations != 3 || out.Messages != 6 || out.Retracted != 1 || out.Attachments != 1 ||
		out.Proposals != 2 || !out.ExpiresAt.Equal(out.AsOf.Add(tools.DefaultExportTTL)) {
		t.Fatalf("the export: %+v", out)
	}
	x := a.files(t, out)
	if got, want := x.ids(), []uuid.UUID{a.c1, a.c2, a.c3}; !slicesEqual(got, want) {
		t.Fatalf("the conversations exported: %v, want %v", got, want)
	}

	// c1: the course, the two who took part, every message in order.
	c1 := x.byID[a.c1.String()]
	if course := obj(c1["course"]); course["id"] != a.course.String() || course["code"] != "CS101" || course["section"] != "A" ||
		course["dept_id"] != a.dept.String() {
		t.Fatalf("c1's course: %v", course)
	}
	if o := obj(c1["opener"]); o["actor_id"] != a.yuki.String() || o["member_id"] != a.yukiM.String() || o["name"] != "Yuki" ||
		o["kind"] != "human" || o["role"] != "student" {
		t.Fatalf("c1's opener: %v", o)
	}
	if r := obj(c1["respondent"]); r["actor_id"] != a.tutor.String() || r["kind"] != "agent" || r["owner"] != nil || r["answers_course"] != false {
		t.Fatalf("c1's respondent: %v", r)
	}
	if c1["status"] != "open" || c1["closed_at"] != nil || c1["title"] != nil {
		t.Fatalf("c1 as it stands: %v", c1)
	}
	msgs := list(c1["messages"])
	if len(msgs) != 3 {
		t.Fatalf("c1's messages: %v", msgs)
	}
	q1, a1, q2 := obj(msgs[0]), obj(msgs[1]), obj(msgs[2])
	if q1["id"] != a.q1.String() || q1["seq"] != 1.0 || q1["body"] != essayQuestion || q1["retracted"] != nil ||
		obj(q1["author"])["actor_id"] != a.yuki.String() {
		t.Fatalf("c1's question: %v", q1)
	}
	if a1["body"] != formulaAnswer || a1["in_reply_to_message_id"] != a.q1.String() || obj(a1["author"])["kind"] != "agent" ||
		len(list(a1["attachments"])) != 0 {
		t.Fatalf("c1's answer: %v", a1)
	}
	// The retracted question with its text, saying who took it back, when
	// and why.
	gone := obj(q2["retracted"])
	if q2["id"] != a.q2.String() || q2["body"] != wrongQuestion || gone == nil || gone["reason"] != retractedWhy ||
		obj(gone["by"])["actor_id"] != a.yuki.String() || gone["at"] == nil || gone["action_id"] == nil {
		t.Fatalf("the retracted question: %v", q2)
	}
	// Her file, described, never its bytes.
	files := list(q1["attachments"])
	if len(files) != 1 {
		t.Fatalf("the question's files: %v", q1["attachments"])
	}
	essay := obj(files[0])
	if essay["filename"] != essayFile || essay["content_type"] != "application/pdf" || essay["byte_size"] != float64(len(essayBytes)) ||
		!strings.HasPrefix(essay["checksum"].(string), "sha256:") || essay["id"] == nil {
		t.Fatalf("the essay, as the export describes it: %v", essay)
	}
	for _, body := range [][]byte{x.jsonl, x.csv} {
		if bytes.Contains(body, essayBytes[:12]) {
			t.Fatal("an export holds a file's bytes")
		}
		for _, token := range a.uploadTokens {
			if bytes.Contains(body, []byte(token)) {
				t.Fatal("an export holds an upload token")
			}
		}
		if bytes.Contains(body, []byte("storage_key")) || bytes.Contains(body, []byte("conversations/"+a.course.String())) {
			t.Fatal("an export says where a file is kept")
		}
	}

	// c2: the answers proposed and never posted, as what became of them.
	c2 := x.byID[a.c2.String()]
	if len(list(c2["messages"])) != 1 {
		t.Fatalf("c2's messages: %v", c2["messages"])
	}
	props := list(c2["proposals"])
	if len(props) != 2 {
		t.Fatalf("c2's proposals: %v", props)
	}
	no, wait := obj(props[0]), obj(props[1])
	if no["action_id"] != a.rejected.String() || no["type"] != "conversation.answer" || no["status"] != "rejected" ||
		no["body"] != rejectedAnswer || no["reason"] != rejectedWhy || obj(no["decided_by"])["actor_id"] != a.sato.String() ||
		no["in_reply_to_message_id"] != a.kenQ.String() || obj(no["proposed_by"])["actor_id"] != a.courseTutor.String() {
		t.Fatalf("the rejected answer: %v", no)
	}
	if wait["action_id"] != a.waiting.String() || wait["status"] != "proposed" || wait["body"] != waitingAnswer ||
		wait["decided_by"] != nil || wait["reason"] != nil || len(list(wait["attachment_filenames"])) != 1 ||
		list(wait["attachment_filenames"])[0] != waitingFile {
		t.Fatalf("the answer waiting: %v", wait)
	}
	if r := obj(c2["respondent"]); obj(r["owner"])["actor_id"] != a.sato.String() || r["answers_course"] != true || r["principal_member_id"] != a.satoM.String() {
		t.Fatalf("the course tutor, Sato's: %v", r)
	}
	// c3: Yuki's own agent, hers.
	if r := obj(x.byID[a.c3.String()]["respondent"]); obj(r["owner"])["actor_id"] != a.yuki.String() || r["principal_member_id"] != a.yukiM.String() {
		t.Fatalf("Yuki's agent: %v", r)
	}

	// The CSV file: one row a message or proposal, the conversation's
	// columns on each, in UTF-8 that a spreadsheet shows as written, and a
	// formula shown and never run.
	if len(x.rows) != 1+6+2 || len(x.rows[0]) != 25 || x.rows[0][0] != "conversation_id" {
		t.Fatalf("the CSV file: %d rows, header %v", len(x.rows), x.rows[0])
	}
	byText := map[string][]string{}
	for _, row := range x.rows[1:] {
		byText[strings.TrimPrefix(x.column(row, "body"), "'")] = row
	}
	if row := byText[essayQuestion]; x.column(row, "status") != "posted" || x.column(row, "conversation_id") != a.c1.String() ||
		x.column(row, "course_code") != "CS101" || x.column(row, "author_name") != "Yuki" || x.column(row, "seq") != "1" ||
		x.column(row, "attachment_filenames") != essayFile || x.column(row, "body") != essayQuestion {
		t.Fatalf("the question's row: %v", row)
	}
	if row := byText[formulaAnswer]; x.column(row, "body") != "'"+formulaAnswer {
		t.Fatalf("a cell a spreadsheet would run: %q", x.column(row, "body"))
	}
	if row := byText[wrongQuestion]; x.column(row, "status") != "retracted" || x.column(row, "reason") != retractedWhy ||
		x.column(row, "retracted_by_name") != "Yuki" || x.column(row, "retracted_at") == "" {
		t.Fatalf("the retracted question's row: %v", row)
	}
	if row := byText[rejectedAnswer]; x.column(row, "status") != "rejected" || x.column(row, "message_id") != "" ||
		x.column(row, "action_id") != a.rejected.String() || x.column(row, "decided_by_name") != "Sato" || x.column(row, "reason") != rejectedWhy {
		t.Fatalf("the rejected answer's row: %v", row)
	}
	if row := byText[waitingAnswer]; x.column(row, "status") != "proposed" || x.column(row, "attachment_filenames") != waitingFile {
		t.Fatalf("the waiting answer's row: %v", row)
	}
	if !bytes.Contains(x.csv, []byte("\r\n")) {
		t.Fatal("the CSV file's lines do not end as a spreadsheet's do")
	}
}

func slicesEqual(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Every export is recorded: who, when, over what, and how much it held. The
// record keeps no URL, a replay gives none, and the one who made it is
// given its files again, and nobody else, until they are removed.
func TestAnExportIsOnRecordAndItsFilesAreItsMakers(t *testing.T) {
	a := newAudit(t)
	args := m{"course_id": a.course, "participant_actor_id": a.ken}
	made := a.MustCall(a.admin, "conversation.export", args, "export-ken")
	out := testkit.Result[tools.ConversationExportOut](t, made)
	if made.Status != domain.StatusExecuted || out.Conversations != 1 || out.Messages != 1 || out.Proposals != 2 {
		t.Fatalf("Ken's conversations: %+v %+v", made, out)
	}
	var row struct {
		ActorID   uuid.UUID
		CourseID  *uuid.UUID
		Authority *string
		Payload   map[string]any
		Result    map[string]any
		Raw       string
	}
	if err := a.Pool.QueryRow(t.Context(), `SELECT actor_id, course_id, authority, payload, result, result::text FROM action
		WHERE id = $1 AND action_type = 'conversation.export' AND status = 'executed' AND authz_result = 'autonomous'
		  AND target_type = 'conversation_export' AND executed_at IS NOT NULL`, *made.ActionID).
		Scan(&row.ActorID, &row.CourseID, &row.Authority, &row.Payload, &row.Result, &row.Raw); err != nil {
		t.Fatalf("the export's action: %v", err)
	}
	if row.ActorID != a.admin || row.CourseID != nil || row.Authority == nil || *row.Authority != domain.AuthorityPlatform ||
		row.Payload["course_id"] != a.course.String() || row.Payload["participant_actor_id"] != a.ken.String() ||
		row.Result["conversations"] != 1.0 || row.Result["messages"] != 1.0 || row.Result["proposals"] != 2.0 ||
		len(list(row.Result["files"])) != 2 {
		t.Fatalf("the export's record: %+v", row)
	}
	if _, kept := row.Result["downloads"]; kept || strings.Contains(row.Raw, "/v1/blobs/") {
		t.Fatalf("the record keeps a URL to the files: %s", row.Raw)
	}
	if n := a.Count(`SELECT count(*) FROM event WHERE type = 'conversation.exported' AND course_id IS NULL AND action_id = $1
		AND subject_id = $1 AND payload->>'conversations' = '1'`, *made.ActionID); n != 1 {
		t.Fatal("the export's news is not in the record, of no course")
	}

	// Replayed, it is the same export, and gives no URL.
	again := a.MustCall(a.admin, "conversation.export", args, "export-ken")
	if !again.Replayed || *again.ActionID != *made.ActionID || testkit.Result[tools.ConversationExportOut](t, again).Downloads != nil {
		t.Fatalf("the export replayed: %+v", again)
	}
	// Its maker is given each file again.
	for _, format := range []string{tools.ExportJSONL, tools.ExportCSV} {
		file := testkit.Result[tools.ConversationExportFileOut](t, a.do(t, a.admin, "conversation.export_file",
			m{"export_id": out.ExportID, "format": format}))
		body, name := a.fetch(t, file.DownloadURL)
		if name != file.Filename || int64(len(body)) != file.ByteSize || file.Format != format ||
			!file.ExportExpiresAt.Equal(out.ExpiresAt) {
			t.Fatalf("%s again: %+v", format, file)
		}
	}
	// Nobody else is: not root, not a department's administrator, and an
	// export that is not one is the same nothing.
	for _, who := range []uuid.UUID{a.Root, a.carol} {
		a.refusedAs(t, who, "conversation.export_file", m{"export_id": out.ExportID, "format": "csv"}, apperr.NotFound, "")
	}
	a.refusedAs(t, a.admin, "conversation.export_file", m{"export_id": a.waiting, "format": "csv"}, apperr.NotFound, "")
	a.refusedAs(t, a.admin, "conversation.export_file", m{"export_id": out.ExportID, "format": "xlsx"}, apperr.InvalidArgument, "")
	a.refusedAs(t, a.sato, "conversation.export_file", m{"export_id": out.ExportID, "format": "csv"}, apperr.Forbidden, "platform_role_required")

	// A day on, its files are gone to it, whether or not the sweep has come.
	*a.clock = time.Now().Add(tools.DefaultExportTTL + time.Minute)
	a.refusedAs(t, a.admin, "conversation.export_file", m{"export_id": out.ExportID, "format": "jsonl"}, apperr.NotFound, "export_expired")
}

// Who may export what: root and platform administrators anything; a
// department's administrators what is beneath their appointments, naming
// it; nobody else; and never an agent, whatever role it holds.
func TestWhoMayExportWhat(t *testing.T) {
	a := newAudit(t)
	// An agent nobody owns, given a platform role by root.
	adminAgent := testkit.Result[tools.ActorOut](t, a.do(t, a.Root, "actor.register",
		m{"kind": "agent", "display_name": "Audit bot", "platform_role": "admin"})).ActorID

	allowed := []struct {
		name     string
		actor    uuid.UUID
		args     m
		convs    []uuid.UUID
		capacity string
		dept     uuid.UUID
	}{
		{"root, the whole site", a.Root, m{}, []uuid.UUID{a.c1, a.c2, a.c3, a.c4}, domain.AuthorityPlatform, uuid.Nil},
		{"a platform administrator, a course", a.admin, m{"course_id": a.his101}, []uuid.UUID{a.c4}, domain.AuthorityPlatform, uuid.Nil},
		{"Carol, her department's course", a.carol, m{"course_id": a.course}, []uuid.UUID{a.c1, a.c2, a.c3}, domain.AuthorityDepartment, a.dept},
		{"Carol, her department", a.carol, m{"within_dept_id": a.dept}, []uuid.UUID{a.c1, a.c2, a.c3}, domain.AuthorityDepartment, a.dept},
		{"Ada, a course beneath her", a.ada, m{"course_id": a.course}, []uuid.UUID{a.c1, a.c2, a.c3}, domain.AuthorityDepartment, a.engineering},
		{"Ada, everything beneath her", a.ada, m{"within_dept_id": a.engineering}, []uuid.UUID{a.c1, a.c2, a.c3}, domain.AuthorityDepartment, a.engineering},
		{"Dan, History", a.dan, m{"within_dept_id": a.history}, []uuid.UUID{a.c4}, domain.AuthorityDepartment, a.history},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			out, done := a.export(t, tc.actor, tc.args)
			if got := a.files(t, out).ids(); !slicesEqual(got, tc.convs) {
				t.Fatalf("exported %v, want %v", got, tc.convs)
			}
			var authority *string
			var dept *uuid.UUID
			if err := a.Pool.QueryRow(t.Context(), `SELECT authority, authority_dept_id FROM action WHERE id = $1`, *done.ActionID).
				Scan(&authority, &dept); err != nil {
				t.Fatal(err)
			}
			if authority == nil || *authority != tc.capacity || (tc.dept == uuid.Nil) != (dept == nil) || (dept != nil && *dept != tc.dept) {
				t.Fatalf("made in the capacity %v of %v, want %s of %v", authority, dept, tc.capacity, tc.dept)
			}
		})
	}

	refused := []struct {
		name   string
		actor  uuid.UUID
		args   m
		reason string
	}{
		{"Carol, the whole site", a.carol, m{}, "platform_role_required"},
		{"Carol, another department's course", a.carol, m{"course_id": a.his101}, "department_out_of_scope"},
		{"Carol, the department above hers", a.carol, m{"within_dept_id": a.engineering}, "department_out_of_scope"},
		{"Dan, a course of Computer Science", a.dan, m{"course_id": a.course}, "department_out_of_scope"},
		{"the instructor", a.sato, m{"course_id": a.course}, "platform_role_required"},
		{"a student", a.yuki, m{"course_id": a.course}, "platform_role_required"},
		{"a student, her own conversations", a.yuki, m{"participant_actor_id": a.yuki}, "platform_role_required"},
		{"the tutor agent", a.tutor, m{"course_id": a.course}, "platform_role_required"},
		{"Yuki's agent", a.bot, m{}, "platform_role_required"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			out := a.MustCall(tc.actor, "conversation.export", tc.args, "refused-"+uuid.NewString())
			if out.Status != domain.StatusDenied || out.Error == nil || out.Error.Details["reason"] != tc.reason || out.ActionID == nil {
				t.Fatalf("%+v, want denied %s, on record", out, tc.reason)
			}
		})
	}
	t.Run("an agent with a platform role", func(t *testing.T) {
		out := a.MustCall(adminAgent, "conversation.export", m{}, "agent-export")
		if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.Forbidden || out.Error.Details["reason"] != "people_only" {
			t.Fatalf("%+v, want refused people_only", out)
		}
		if n := a.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'failed'`, *out.ActionID); n != 1 {
			t.Fatal("the agent's attempt is not on record")
		}
	})
	t.Run("not both a course and a department", func(t *testing.T) {
		a.refusedAs(t, a.Root, "conversation.export", m{"course_id": a.course, "within_dept_id": a.dept}, apperr.InvalidArgument, "")
		a.refusedAs(t, a.Root, "conversation.export", m{"course_id": uuid.New()}, apperr.NotFound, "")
		a.refusedAs(t, a.Root, "conversation.export", m{"participant_actor_id": uuid.New()}, apperr.NotFound, "")
		now := time.Now()
		a.refusedAs(t, a.Root, "conversation.export", m{"from": now, "before": now.Add(-time.Hour)}, apperr.InvalidArgument, "")
	})
}

// The filters: a participant, a span of time, an archived course. A span
// keeps the conversations with something written in it, and of them what
// was written in it.
func TestAnExportsFiltersChooseWhatItHolds(t *testing.T) {
	a := newAudit(t)
	cases := []struct {
		name  string
		args  m
		convs []uuid.UUID
		msgs  int
	}{
		{"Yuki's", m{"participant_actor_id": a.yuki}, []uuid.UUID{a.c1, a.c3}, 5},
		{"her agent's", m{"participant_actor_id": a.bot}, []uuid.UUID{a.c3}, 2},
		{"Sato's, who asked nothing", m{"participant_actor_id": a.sato}, nil, 0},
		{"the last week", m{"course_id": a.course, "from": a.fiveDaysAgo.Add(-time.Hour), "before": a.oneAgo.Add(-time.Hour)},
			[]uuid.UUID{a.c2}, 1},
		{"from yesterday", m{"from": a.oneAgo.Add(-time.Hour)}, []uuid.UUID{a.c3, a.c4}, 3},
		{"before last week", m{"before": a.fiveDaysAgo.Add(-time.Hour)}, []uuid.UUID{a.c1}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := a.export(t, a.Root, tc.args)
			x := a.files(t, out)
			if got := x.ids(); !slicesEqual(got, tc.convs) || out.Messages != tc.msgs || out.Conversations != len(tc.convs) {
				t.Fatalf("exported %v, %d messages; want %v, %d", got, out.Messages, tc.convs, tc.msgs)
			}
			n := 0
			for _, l := range x.lines {
				n += len(list(l["messages"]))
			}
			if n != tc.msgs {
				t.Fatalf("the file holds %d messages, the export says %d", n, out.Messages)
			}
		})
	}
	// An answer proposed in a span, to a question asked before it, brings
	// its conversation along, with nothing written in the span.
	out, _ := a.export(t, a.Root, m{"from": a.fiveDaysAgo.Add(30 * time.Minute), "before": a.fiveDaysAgo.Add(2 * time.Hour)})
	if x := a.files(t, out); !slicesEqual(x.ids(), []uuid.UUID{a.c2}) || out.Messages != 0 || out.Proposals != 1 ||
		len(list(x.lines[0]["proposals"])) != 1 || obj(list(x.lines[0]["proposals"])[0])["action_id"] != a.waiting.String() {
		t.Fatalf("an answer proposed alone in the span: %+v", out)
	}
	// An archived course is exported as any: what was said in it is still
	// on record.
	a.do(t, a.admin, "course.archive", m{"course_id": a.his101})
	if out, _ := a.export(t, a.dan, m{"course_id": a.his101}); out.Conversations != 1 {
		t.Fatalf("an archived course's export: %+v", out)
	}
	// A conversation closed says when.
	a.do(t, a.yuki, "conversation.close", m{"course_id": a.course, "conversation_id": a.c3})
	out, _ = a.export(t, a.Root, m{"participant_actor_id": a.bot})
	if c3 := a.files(t, out).byID[a.c3.String()]; c3["status"] != "closed" || c3["closed_at"] == nil {
		t.Fatalf("a closed conversation: %v", c3)
	}
}

// Past its limits an export is refused, saying how much it would hold, and
// leaves nothing behind but its refusal on record.
func TestAnExportPastItsLimitsIsRefused(t *testing.T) {
	a := auditOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.Exports = tools.ExportLimits{MaxMessages: 7, MaxBytes: 100} }))
	for _, tc := range []struct {
		name                string
		args                m
		conversations, msgs int64
	}{
		// Eight messages and proposals, past seven.
		{"messages", m{"course_id": a.course}, 3, 8},
		// Five messages, but 152 bytes of text, past 100.
		{"bytes", m{"participant_actor_id": a.yuki}, 2, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := a.MustCall(a.Root, "conversation.export", tc.args, "too-much-"+tc.name)
			if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.FailedPrecondition ||
				out.Error.Details["reason"] != "export_too_large" || out.Error.Details["messages"] != tc.msgs ||
				out.Error.Details["conversations"] != tc.conversations || out.Error.Details["max_messages"] != 7 ||
				out.Error.Details["max_bytes"] != int64(100) {
				t.Fatalf("%+v, want export_too_large", out.Error)
			}
			if n := a.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'failed' AND result->'error'->'details'->>'reason' = 'export_too_large'`,
				*out.ActionID); n != 1 {
				t.Fatal("the refusal is not on record")
			}
		})
	}
	if n := a.Count(`SELECT count(*) FROM event WHERE type = 'conversation.exported'`); n != 0 {
		t.Fatal("an export refused is told as made")
	}
	if kept, _ := filepath.Glob(filepath.Join(a.Blob.Root(), "exports", "*")); len(kept) != 0 {
		t.Fatalf("an export refused left files: %v", kept)
	}
	// Narrowed, it goes.
	if out, _ := a.export(t, a.Root, m{"participant_actor_id": a.bot}); out.Messages != 2 {
		t.Fatalf("narrowed: %+v", out)
	}
}
