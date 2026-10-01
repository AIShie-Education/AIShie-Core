package mcpapi_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/mcpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// serveMemory is serve on a server that keeps agents' memory (MEMORY=on).
func serveMemory(t *testing.T, students int) *fixture {
	t.Helper()
	c := testkit.NewCS101WithDeps(t, students, func(d *tools.Deps) { d.Memory.Enabled = true })
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewAuthenticator(c.Pool, time.Hour)
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: authn,
		MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: c.P, Auth: authn, Memory: true}),
	}))
	t.Cleanup(srv.Close)
	return &fixture{c: c, srv: srv}
}

// What a connecting agent is told of memory: where it is kept, and how
// each kind is kept apart, when this server keeps it; that it keeps its own
// when it does not.
func TestTheInstructionsSayWhereMemoryIsKept(t *testing.T) {
	kept := serveMemory(t, 0)
	got := kept.connect(t, kept.token(t, kept.c.Grader)).InitializeResult().Instructions
	for _, want := range []string{"memory_write", "never instructions", "memory_search", "from the memory of that conversation's opener alone",
		"idempotency_key", "conversation_inbox", "me_get says which (hosting)"} {
		if !strings.Contains(got, want) {
			t.Errorf("the instructions of a server that keeps memory do not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "this server keeps none for you") {
		t.Error("a server that keeps memory says it keeps none")
	}
	own := serve(t, 0)
	got = own.connect(t, own.token(t, own.c.Grader)).InitializeResult().Instructions
	if !strings.Contains(got, "You keep your own memory; this server keeps none for you.") || strings.Contains(got, "memory_write") ||
		!strings.Contains(got, "Answer each conversation from that conversation alone. ") {
		t.Errorf("the instructions of a server that keeps no memory:\n%s", got)
	}
}

// With memory on, an agent connected over MCP writes to each scope of its
// memory and reads it back: about its owner, about a student who asks it,
// and a proposal to the course's shared memory.
func TestAnAgentKeepsMemoryOverMCP(t *testing.T) {
	f := serveMemory(t, 1)
	c, yuki := f.c, f.c.Students[0]
	tutor := c.OwnedRuntimeAgent(c.Sato, "Course tutor")
	seat := c.Delegate(c.Course, tutor, c.SatoM, "course_tutor")
	c.Exec(`UPDATE course_member SET answers_course = true WHERE id = $1`, seat)
	// The site's runtime hosts it, and connects with the token it was
	// issued; then Yuki asks it.
	_, token := c.HostToken(tutor)
	s := f.connect(t, token)
	opened := testkit.Result[tools.ConversationOpenOut](t, c.MustCall(yuki.Actor, "conversation.open",
		m{"course_id": c.Course, "respondent_member_id": seat, "body": "How do I start HW3?"}, "open"))

	write := func(key string, args m) tools.MemoryWriteOut {
		t.Helper()
		args["idempotency_key"] = key
		env, res := call(t, s, "memory_write", args)
		var out tools.MemoryWriteOut
		if env.Status != "executed" || res.IsError || json.Unmarshal(env.Result, &out) != nil || out.MemoryID == uuid.Nil {
			t.Fatalf("memory_write %v: %+v", args, env)
		}
		return out
	}
	owner := write("owner", m{"scope": "owner", "text": "Sato wants hints, not answers, given to students."})
	asker := write("asker", m{"scope": "asker", "course_id": c.Course, "conversation_id": opened.ConversationID,
		"text": "Yuki finds recursion hard; start from base cases.", "tags": []string{"difficulty"}})
	shared := write("shared", m{"scope": "course", "course_id": c.Course, "text": "HW3 is due Friday at 17:00."})
	if owner.Status != "active" || asker.Status != "active" || shared.Status != "proposed" {
		t.Fatalf("statuses: %s %s %s", owner.Status, asker.Status, shared.Status)
	}

	read := func(name string, args m, into any) {
		t.Helper()
		env, res := call(t, s, name, args)
		if env.Status != "executed" || res.IsError || json.Unmarshal(env.Result, into) != nil {
			t.Fatalf("%s %v: %+v", name, args, env)
		}
	}
	var found tools.MemorySearchOut
	read("memory_search", m{"course_id": c.Course, "conversation_id": opened.ConversationID, "query": "recursion"}, &found)
	if strings.Join(found.Searched, ",") != "asker,course" || len(found.Entries) != 1 || found.Entries[0].ID != asker.MemoryID ||
		found.Note == "" || !found.Enabled {
		t.Fatalf("searching as Yuki's respondent: %+v", found)
	}
	read("memory_search", m{}, &found)
	if len(found.Entries) != 1 || found.Entries[0].ID != owner.MemoryID {
		t.Fatalf("searching for the owner: %+v", found)
	}
	var waiting tools.MemoryListOut
	read("memory_list", m{"scope": "course", "course_id": c.Course, "status": "proposed"}, &waiting)
	if waiting.Total != 1 || waiting.Entries[0].ID != shared.MemoryID || *waiting.Entries[0].Text != "HW3 is due Friday at 17:00." {
		t.Fatalf("the proposal: %+v", waiting)
	}
	var got tools.MemoryGetOut
	read("memory_get", m{"memory_id": owner.MemoryID}, &got)
	if got.Text == nil || *got.Text != "Sato wants hints, not answers, given to students." || got.Scope != "owner" {
		t.Fatalf("the owner entry: %+v", got)
	}

	// Yuki is refused: people keep no memory.
	ys := f.connect(t, f.token(t, yuki.Actor))
	env, res := call(t, ys, "memory_write", m{"scope": "owner", "text": "My own note.", "idempotency_key": "mine"})
	if env.Status != "failed" || !res.IsError || env.Error == nil || env.Error.Details["reason"] != "not_an_agent" {
		t.Fatalf("a person writing memory: %+v", env)
	}
}
