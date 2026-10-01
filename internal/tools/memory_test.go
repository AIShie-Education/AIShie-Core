package tools_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/memory"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// An agent's memory: what it reaches of it, and how, is one rule
// (memoryAccess), measured on every call from its seats; what it writes
// stays out of the action log; how much it keeps and how fast it writes are
// bounded.

// memCast is a course with every kind of agent that keeps memory, each with
// something kept already, made through the tools:
//
//   - the course's tutor, Sato's agent, seated as his delegate to answer the
//     course, keeping something about Sato, about Yuki (who asked it) and a
//     proposal to the course's shared memory;
//   - Yuki's own agent, her delegate, keeping something about her;
//   - build's tutor, which nobody owns, listed for Yuki, keeping something
//     about her.
type memCast struct {
	*built
	tutor, tutorM   uuid.UUID // Sato's course tutor
	helper, helperM uuid.UUID // Yuki's own agent
	// build's b.tutor and b.tutorM: the tutor nobody owns
	yukiTutor, satoTutor, yukiHelper, yukiUnowned uuid.UUID // conversations: who asked whom
	tutorOwner, tutorAsker, tutorShared           uuid.UUID // the course tutor's entries
	helperOwner, unownedAsker                     uuid.UUID
}

func newMemCast(t *testing.T, adjust func(*memory.Config)) *memCast {
	t.Helper()
	cfg := memory.Config{Enabled: true}
	if adjust != nil {
		adjust(&cfg)
	}
	b := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.Memory = cfg }))
	c := &memCast{built: b}
	c.tutor = b.runtimeAgent(t, b.sato, "Course tutor")
	c.tutorM = b.delegate(t, b.sato, c.tutor, m{"preset": "course_tutor"})
	c.helper = b.runtimeAgent(t, b.yuki, "Yuki's helper")
	c.helperM = b.delegate(t, b.yuki, c.helper, m{})
	b.Host(c.tutor)
	b.Host(c.helper)
	c.yukiTutor, _ = b.open(t, b.yuki, c.tutorM, "How do I start HW3?")
	c.satoTutor, _ = b.open(t, b.sato, c.tutorM, "What do students ask most?")
	c.yukiHelper, _ = b.open(t, b.yuki, c.helperM, "Remind me what is due.")
	c.yukiUnowned, _ = b.open(t, b.yuki, b.tutorM, "Can you check my thesis?")
	if !cfg.Enabled {
		return c
	}
	c.tutorOwner = c.write(t, c.tutor, m{"scope": "owner", "text": "Sato prefers short summaries."}).MemoryID
	c.tutorAsker = c.write(t, c.tutor, c.in(c.yukiTutor, m{"scope": "asker", "text": "Yuki finds recursion hard."})).MemoryID
	c.tutorShared = c.write(t, c.tutor, m{"scope": "course", "course_id": b.course, "text": "HW3 is due Friday at 17:00."}).MemoryID
	c.helperOwner = c.write(t, c.helper, m{"scope": "owner", "course_id": b.course, "text": "Yuki is aiming for an A in CS101."}).MemoryID
	c.unownedAsker = c.write(t, b.tutor, c.in(c.yukiUnowned, m{"scope": "asker", "text": "Yuki's thesis is on tides."})).MemoryID
	return c
}

// in adds a conversation, and its course, to args.
func (c *memCast) in(conversation uuid.UUID, args m) m {
	args["course_id"], args["conversation_id"] = c.course, conversation
	return args
}

func (c *memCast) write(t *testing.T, agent uuid.UUID, args m) tools.MemoryWriteOut {
	t.Helper()
	return testkit.Result[tools.MemoryWriteOut](t, c.do(t, agent, "memory.write", args))
}

// outcome is what came of a call: "ok" when it was done, else why not — the
// reason its error gives, or its code when it gives none.
func (c *memCast) outcome(t *testing.T, actor uuid.UUID, name string, args m) string {
	t.Helper()
	out, err := c.Call(actor, name, args, "mem-"+uuid.NewString())
	if err != nil {
		e, ok := apperr.As(err)
		if !ok {
			t.Fatalf("%s: %v", name, err)
		}
		return why(e)
	}
	if out.Status == domain.StatusExecuted {
		return "ok"
	}
	return why(out.Error)
}

func why(e *apperr.Error) string {
	if r, ok := e.Details["reason"].(string); ok {
		return r
	}
	return string(e.Code)
}

// The access matrix: who reaches what of an agent's memory, tool by tool.
// Each row is a fresh course; its cells are the six tools, each with the
// arguments that row calls it with and what must come of it.
func TestMemoryAccessMatrix(t *testing.T) {
	type cell struct {
		args m
		want string
	}
	type row struct {
		name    string
		off     bool // MEMORY=off
		caller  func(c *memCast) uuid.UUID
		setup   func(t *testing.T, c *memCast)
		cells   func(c *memCast) [6]cell // search, list, get, write, update, forget
		comment string
	}
	tool6 := [6]string{"memory.search", "memory.list", "memory.get", "memory.write", "memory.update", "memory.forget"}
	all := func(want string, c *memCast, course, conversation *uuid.UUID, entry uuid.UUID, scope string) [6]cell {
		with := func(a m) m {
			if course != nil {
				a["course_id"] = *course
			}
			if conversation != nil {
				a["conversation_id"] = *conversation
			}
			return a
		}
		return [6]cell{
			{with(m{}), want}, {with(m{"scope": scope}), want}, {with(m{"memory_id": entry}), want},
			{with(m{"scope": scope, "text": "Something new."}), want}, {with(m{"memory_id": entry, "text": "Corrected."}), want},
			{with(m{"memory_id": entry}), want},
		}
	}
	rows := []row{
		{name: "a delegate asked by its owner", caller: func(c *memCast) uuid.UUID { return c.helper },
			cells: func(c *memCast) [6]cell {
				return [6]cell{
					{c.in(c.yukiHelper, m{}), "ok"}, {c.in(c.yukiHelper, m{"scope": "asker"}), "ok"},
					{c.in(c.yukiHelper, m{"memory_id": c.helperOwner}), "ok"},
					{m{"scope": "owner", "course_id": c.course, "text": "Yuki has a quiz on Monday."}, "ok"},
					{m{"memory_id": c.helperOwner, "text": "Yuki is aiming for an A."}, "ok"}, {m{"memory_id": c.helperOwner}, "ok"},
				}
			}},
		{name: "a tutor asked by a student", caller: func(c *memCast) uuid.UUID { return c.tutor },
			cells: func(c *memCast) [6]cell {
				return all("ok", c, &c.course, &c.yukiTutor, c.tutorAsker, "asker")
			}},
		{name: "a tutor asked by its owner", caller: func(c *memCast) uuid.UUID { return c.tutor },
			cells: func(c *memCast) [6]cell {
				return [6]cell{
					{c.in(c.satoTutor, m{}), "ok"}, {c.in(c.satoTutor, m{"scope": "asker"}), "ok"},
					{c.in(c.satoTutor, m{"memory_id": c.tutorOwner}), "ok"}, {m{"scope": "owner", "text": "Sato teaches on Mondays."}, "ok"},
					{m{"memory_id": c.tutorOwner, "text": "Sato prefers bullet points."}, "ok"}, {m{"memory_id": c.tutorOwner}, "ok"},
				}
			}},
		{name: "an unowned tutor", caller: func(c *memCast) uuid.UUID { return c.built.tutor },
			cells: func(c *memCast) [6]cell {
				return [6]cell{
					{c.in(c.yukiUnowned, m{}), "ok"}, {m{"scope": "owner"}, "no_owner"},
					{c.in(c.yukiUnowned, m{"memory_id": c.unownedAsker}), "ok"}, {m{"scope": "owner", "text": "Nobody's."}, "no_owner"},
					{c.in(c.yukiUnowned, m{"memory_id": c.unownedAsker, "text": "Yuki's thesis is on tides and the moon."}), "ok"},
					{c.in(c.yukiUnowned, m{"memory_id": c.unownedAsker}), "ok"},
				}
			}},
		{name: "a person calling", caller: func(c *memCast) uuid.UUID { return c.sato },
			cells: func(c *memCast) [6]cell { return all("not_an_agent", c, nil, nil, c.tutorOwner, "owner") }},
		{name: "the agent suspended", caller: func(c *memCast) uuid.UUID { return c.tutor },
			setup: func(t *testing.T, c *memCast) { c.do(t, c.sato, "agent.suspend", m{"actor_id": c.tutor}) },
			cells: func(c *memCast) [6]cell {
				return all(string(authzActorNotActive), c, &c.course, &c.yukiTutor, c.tutorAsker, "asker")
			}},
		{name: "the owner suspended", caller: func(c *memCast) uuid.UUID { return c.tutor },
			setup: func(t *testing.T, c *memCast) { c.do(t, c.admin, "actor.suspend", m{"actor_id": c.sato}) },
			cells: func(c *memCast) [6]cell {
				return [6]cell{
					{c.in(c.yukiTutor, m{}), "principal_not_active"}, {m{"scope": "owner"}, "owner_not_active"},
					{m{"memory_id": c.tutorOwner}, "not_found"}, {m{"scope": "owner", "text": "Sato is away."}, "owner_not_active"},
					{m{"memory_id": c.tutorOwner, "text": "Sato is away."}, "not_found"}, {m{"memory_id": c.tutorOwner}, "not_found"},
				}
			}},
		{name: "the seat paused", caller: func(c *memCast) uuid.UUID { return c.tutor },
			setup: func(t *testing.T, c *memCast) {
				c.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, c.tutorM)
			},
			cells: func(c *memCast) [6]cell {
				return all("membership_not_active", c, &c.course, &c.yukiTutor, c.tutorAsker, "asker")
			}},
		{name: "the principal paused", caller: func(c *memCast) uuid.UUID { return c.tutor },
			setup: func(t *testing.T, c *memCast) {
				c.Exec(`UPDATE course_member SET status = 'paused' WHERE id = $1`, c.satoM)
			},
			cells: func(c *memCast) [6]cell {
				return all("principal_not_active", c, &c.course, &c.yukiTutor, c.tutorAsker, "asker")
			}},
		{name: "the course archived", caller: func(c *memCast) uuid.UUID { return c.tutor },
			setup: func(t *testing.T, c *memCast) { c.do(t, c.admin, "course.archive", m{"course_id": c.course}) },
			cells: func(c *memCast) [6]cell {
				cells := all("course_archived", c, &c.course, &c.yukiTutor, c.tutorAsker, "asker")
				for i := range 3 { // reading goes on until the sweep freezes it
					cells[i].want = "ok"
				}
				return cells
			}},
		{name: "a conversation not addressed to the caller", caller: func(c *memCast) uuid.UUID { return c.tutor },
			cells: func(c *memCast) [6]cell {
				return all("not_respondent", c, &c.course, &c.yukiUnowned, c.tutorAsker, "asker")
			}},
		{name: "the opener removed", caller: func(c *memCast) uuid.UUID { return c.tutor },
			setup: func(t *testing.T, c *memCast) {
				c.do(t, c.sato, "member.remove", m{"course_id": c.course, "member_id": c.yukiM})
			},
			cells: func(c *memCast) [6]cell {
				return all("not_addressable", c, &c.course, &c.yukiTutor, c.tutorAsker, "asker")
			}},
		{name: "the opener narrowed out of reach", caller: func(c *memCast) uuid.UUID { return c.tutor },
			comment: "the tutor reads the material, which Yuki no longer may: it is not within her seat",
			setup: func(t *testing.T, c *memCast) {
				c.do(t, c.sato, "member.update_perms", m{"course_id": c.course, "member_id": c.yukiM, "perms": m{"document_read": "denied"}})
			},
			cells: func(c *memCast) [6]cell {
				return all("not_addressable", c, &c.course, &c.yukiTutor, c.tutorAsker, "asker")
			}},
		{name: "asker_is_owner", caller: func(c *memCast) uuid.UUID { return c.tutor },
			cells: func(c *memCast) [6]cell {
				cells := all("ok", c, &c.course, &c.satoTutor, c.tutorOwner, "asker")
				cells[3].want = "asker_is_owner"
				return cells
			}},
		{name: "answers_owner_only", caller: func(c *memCast) uuid.UUID { return c.helper },
			cells: func(c *memCast) [6]cell {
				return [6]cell{
					{m{"course_id": c.course}, "ok"}, {m{"scope": "course", "course_id": c.course}, "ok"},
					{m{"memory_id": c.helperOwner}, "ok"}, {m{"scope": "course", "course_id": c.course, "text": "HW3 is easy."}, "answers_owner_only"},
					{m{"memory_id": c.helperOwner, "pinned": true}, "ok"}, {m{"memory_id": c.helperOwner}, "ok"},
				}
			}},
		{name: "memory off", caller: func(c *memCast) uuid.UUID { return c.tutor },
			setup: func(t *testing.T, c *memCast) {
				c.Exec(`INSERT INTO memory_setting (holder_actor_id, enabled, updated_by_actor_id, updated_at) VALUES ($1, false, $2, now())`, c.tutor, c.sato)
			},
			cells: func(c *memCast) [6]cell {
				cells := all("memory_off", c, &c.course, &c.yukiTutor, c.tutorAsker, "asker")
				cells[0].want, cells[1].want, cells[5].want = "ok", "ok", "ok" // reads find nothing; forgetting goes on
				return cells
			}},
		{name: "MEMORY=off", off: true, caller: func(c *memCast) uuid.UUID { return c.tutor },
			cells: func(c *memCast) [6]cell {
				return all("memory_unavailable", c, &c.course, &c.yukiTutor, uuid.New(), "asker")
			}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := newMemCast(t, func(cfg *memory.Config) { cfg.Enabled = !r.off })
			if r.setup != nil {
				r.setup(t, c)
			}
			caller := r.caller(c)
			for i, cl := range r.cells(c) {
				if got := c.outcome(t, caller, tool6[i], cl.args); got != cl.want {
					t.Errorf("%s %v: %s, want %s %s", tool6[i], cl.args, got, cl.want, r.comment)
				}
			}
		})
	}
}

// authzActorNotActive is the pipeline's reason for a caller who is not
// active, as its denial gives it.
const authzActorNotActive = "actor_not_active"

// What each scope is, as the agent finds it: a search names the scopes the
// call reaches; what is about an asker comes back only through their
// conversation; a person is the subject of one scope; the shared memory
// waits for review.
func TestMemoryScopes(t *testing.T) {
	c := newMemCast(t, nil)
	search := func(agent uuid.UUID, args m) tools.MemorySearchOut {
		t.Helper()
		return testkit.Result[tools.MemorySearchOut](t, c.do(t, agent, "memory.search", args))
	}
	ids := func(es []tools.MemoryEntry) []uuid.UUID {
		out := []uuid.UUID{}
		for _, e := range es {
			out = append(out, e.ID)
		}
		return out
	}

	// Answering Yuki, the tutor finds what it keeps about her, and the
	// course's shared memory once it is in force; never what it keeps
	// about Sato.
	got := search(c.tutor, c.in(c.yukiTutor, m{}))
	if strings.Join(got.Searched, ",") != "asker,course" || !equalIDs(ids(got.Entries), []uuid.UUID{c.tutorAsker}) ||
		got.Note != tools.MemoryNote || !got.Enabled {
		t.Fatalf("answering Yuki: %+v", got)
	}
	if e := got.Entries[0]; e.Scope != "asker" || e.Text == nil || *e.Text != "Yuki finds recursion hard." || e.Source != "agent" ||
		e.Version != 1 || e.CourseID == nil || *e.CourseID != c.course {
		t.Fatalf("the entry: %+v", e)
	}
	c.Exec(`UPDATE memory_entry SET status = 'active' WHERE id = $1`, c.tutorShared) // as a review will
	got = search(c.tutor, c.in(c.yukiTutor, m{"query": "HW3 Friday"}))
	if !equalIDs(ids(got.Entries), []uuid.UUID{c.tutorShared}) {
		t.Fatalf("answering Yuki about HW3: %+v", got)
	}
	// Answering Sato, what it keeps about him in place of an asker's.
	got = search(c.tutor, c.in(c.satoTutor, m{}))
	if strings.Join(got.Searched, ",") != "owner,course" || !equalIDs(ids(got.Entries), []uuid.UUID{c.tutorShared, c.tutorOwner}) &&
		!equalIDs(ids(got.Entries), []uuid.UUID{c.tutorOwner, c.tutorShared}) {
		t.Fatalf("answering Sato: %+v", got)
	}
	// With neither, its owner; scopes narrow and never widen.
	if got = search(c.tutor, m{}); strings.Join(got.Searched, ",") != "owner" || !equalIDs(ids(got.Entries), []uuid.UUID{c.tutorOwner}) {
		t.Fatalf("no course: %+v", got)
	}
	if got = search(c.tutor, c.in(c.yukiTutor, m{"scopes": []string{"course"}})); strings.Join(got.Searched, ",") != "course" {
		t.Fatalf("scopes course: %+v", got)
	}
	if got = search(c.tutor, m{"course_id": c.course, "scopes": []string{"asker", "owner"}}); len(got.Searched) != 0 || len(got.Entries) != 0 {
		t.Fatalf("scopes the call does not reach: %+v", got)
	}
	if r := c.outcome(t, c.built.tutor, "memory.search", m{"scopes": []string{"owner"}}); r != "no_owner" {
		t.Fatalf("an unowned agent asking for its owner: %s", r)
	}
	if r := c.outcome(t, c.tutor, "memory.search", m{"query": strings.Repeat("x", 501)}); r != "too_long" {
		t.Fatalf("a long query: %s", r)
	}
	words := []string{}
	for i := range memory.MaxQueryTerms + 1 {
		words = append(words, fmt.Sprintf("w%d", i))
	}
	if r := c.outcome(t, c.tutor, "memory.search", m{"query": strings.Join(words, " ")}); r != "too_long" {
		t.Fatalf("a query of too many words: %s", r)
	}
	if r := c.outcome(t, c.tutor, "memory.search", m{"conversation_id": c.yukiTutor}); r != "scope_args" {
		t.Fatalf("a conversation without its course: %s", r)
	}

	// Yuki's entry is not reached through Sato's conversation, nor without
	// one; nor, by the tutor nobody owns, which is answering her too.
	for _, args := range []m{c.in(c.satoTutor, m{"memory_id": c.tutorAsker}), {"memory_id": c.tutorAsker},
		{"memory_id": c.tutorAsker, "course_id": c.course}} {
		if r := c.outcome(t, c.tutor, "memory.get", args); r != "not_found" {
			t.Fatalf("Yuki's entry with %v: %s", args, r)
		}
	}
	if r := c.outcome(t, c.built.tutor, "memory.get", c.in(c.yukiUnowned, m{"memory_id": c.tutorAsker})); r != "not_found" {
		t.Fatalf("another agent's entry: %s", r)
	}

	// A scope is given with what it needs, and nothing it does not take.
	for _, args := range []m{
		{"scope": "asker", "course_id": c.course, "text": "x"},
		{"scope": "course", "text": "x"},
		{"scope": "course", "course_id": c.course, "conversation_id": c.yukiTutor, "text": "x"},
		{"scope": "owner", "course_id": c.course, "conversation_id": c.satoTutor, "text": "x"},
	} {
		if r := c.outcome(t, c.tutor, "memory.write", args); r != "scope_args" {
			t.Errorf("write %v: %s", args, r)
		}
	}
	if r := c.outcome(t, c.tutor, "memory.list", m{"scope": "owner", "course_id": c.course}); r != "scope_args" {
		t.Errorf("owner memory listed by course: %s", r)
	}
	if r := c.outcome(t, c.tutor, "memory.list", m{"scope": "asker", "course_id": c.course, "conversation_id": c.yukiTutor, "status": "proposed"}); r != "invalid_argument" {
		t.Errorf("asker memory waiting for review: %s", r)
	}

	// The shared memory: a proposal is listed as the agent's until it is
	// decided; a rejection keeps its reason and none of its text.
	proposal := c.write(t, c.tutor, m{"scope": "course", "course_id": c.course, "text": "The midterm covers weeks 1 to 6."})
	if proposal.Status != "proposed" || proposal.Duplicate {
		t.Fatalf("a proposal: %+v", proposal)
	}
	list := testkit.Result[tools.MemoryListOut](t, c.do(t, c.tutor, "memory.list", m{"scope": "course", "course_id": c.course, "status": "proposed"}))
	if list.Total != 1 || len(list.Entries) != 1 || list.Entries[0].ID != proposal.MemoryID {
		t.Fatalf("proposals: %+v", list)
	}
	c.Exec(`UPDATE memory_entry SET status = 'rejected', body = NULL, search_text = '', text_hash = NULL, decided_at = now(),
	        decided_by_member_id = $2, decision_reason = 'Not yet announced.' WHERE id = $1`, proposal.MemoryID, c.satoM)
	list = testkit.Result[tools.MemoryListOut](t, c.do(t, c.tutor, "memory.list", m{"scope": "course", "course_id": c.course, "status": "rejected"}))
	if len(list.Entries) != 1 || list.Entries[0].Text != nil || list.Entries[0].DecisionReason == nil || *list.Entries[0].DecisionReason != "Not yet announced." {
		t.Fatalf("rejections: %+v", list)
	}
	if r := c.outcome(t, c.tutor, "memory.update", m{"memory_id": proposal.MemoryID, "course_id": c.course, "text": "Weeks 1 to 7."}); r != "conflict" {
		t.Fatalf("correcting a rejection: %s", r)
	}
	if r := c.outcome(t, c.tutor, "memory.forget", m{"memory_id": proposal.MemoryID, "course_id": c.course}); r != "ok" {
		t.Fatalf("forgetting a rejection: %s", r)
	}

	// Correcting what is in force proposes a replacement; the entry stays
	// as it is meanwhile. A proposal is corrected in place.
	fix := testkit.Result[tools.MemoryUpdateOut](t, c.do(t, c.tutor, "memory.update",
		m{"memory_id": c.tutorShared, "course_id": c.course, "text": "HW3 is due Friday at 18:00."}))
	if fix.Status != "proposed" || fix.MemoryID == c.tutorShared || fix.ReplacesID == nil || *fix.ReplacesID != c.tutorShared {
		t.Fatalf("a correction: %+v", fix)
	}
	in := testkit.Result[tools.MemoryGetOut](t, c.do(t, c.tutor, "memory.get", m{"memory_id": c.tutorShared, "course_id": c.course}))
	if in.Status != "active" || *in.Text != "HW3 is due Friday at 17:00." || in.Version != 1 {
		t.Fatalf("the entry in force meanwhile: %+v", in)
	}
	again := testkit.Result[tools.MemoryUpdateOut](t, c.do(t, c.tutor, "memory.update",
		m{"memory_id": fix.MemoryID, "course_id": c.course, "text": "HW3 is due Friday at 18:30.", "version": 1}))
	if again.MemoryID != fix.MemoryID || again.Version != 2 || again.Status != "proposed" || again.ReplacesID == nil {
		t.Fatalf("a proposal corrected: %+v", again)
	}
	for args, want := range map[string]m{
		"a correction that changes no text": {"memory_id": c.tutorShared, "course_id": c.course, "pinned": true},
		"a correction to the same text":     {"memory_id": c.tutorShared, "course_id": c.course, "text": "HW3 is due Friday at 17:00."},
	} {
		wantWhy := map[string]string{"a correction that changes no text": "scope_args", "a correction to the same text": "duplicate"}[args]
		if r := c.outcome(t, c.tutor, "memory.update", want); r != wantWhy {
			t.Errorf("%s: %s, want %s", args, r, wantWhy)
		}
	}

	// Owner memory may say where it was learnt, and only a course where the
	// agent has a seat that counts.
	helperOut := testkit.Result[tools.MemoryListOut](t, c.do(t, c.helper, "memory.list", m{"scope": "owner"}))
	if len(helperOut.Entries) != 1 || helperOut.Entries[0].CourseID == nil || *helperOut.Entries[0].CourseID != c.course {
		t.Fatalf("Yuki's helper's memory of her: %+v", helperOut)
	}
	if r := c.outcome(t, c.tutor, "memory.write", m{"scope": "owner", "course_id": uuid.New(), "text": "x"}); r != "not_found" {
		t.Fatalf("a tag of no course: %s", r)
	}
	other := testkit.Result[tools.CourseCreateOut](t, c.do(t, c.admin, "course.create",
		m{"dept_id": c.dept, "term_id": c.term, "code": "CS102", "section": "A", "title": "More computing"})).CourseID
	if r := c.outcome(t, c.tutor, "memory.write", m{"scope": "owner", "course_id": other, "text": "x"}); r != "not_a_member" {
		t.Fatalf("a tag of a course the agent is not in: %s", r)
	}
}

func equalIDs(a, b []uuid.UUID) bool {
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

// Corrections, pins, tags and versions: what memory.update checks.
func TestMemoryUpdate(t *testing.T) {
	c := newMemCast(t, nil)
	upd := func(args m) tools.MemoryUpdateOut {
		t.Helper()
		return testkit.Result[tools.MemoryUpdateOut](t, c.do(t, c.tutor, "memory.update", args))
	}
	out := upd(m{"memory_id": c.tutorOwner, "tags": []string{"preference"}, "pinned": true, "version": 1})
	if out.Version != 2 || out.Status != "active" {
		t.Fatalf("tags and a pin: %+v", out)
	}
	if r := c.outcome(t, c.tutor, "memory.update", m{"memory_id": c.tutorOwner, "text": "Sato likes tables.", "version": 1}); r != "version_mismatch" {
		t.Fatalf("an old version: %s", r)
	}
	other := c.write(t, c.tutor, m{"scope": "owner", "text": "Sato likes tables."})
	if r := c.outcome(t, c.tutor, "memory.update", m{"memory_id": c.tutorOwner, "text": "  Sato likes tables.\r\n"}); r != "duplicate" {
		t.Fatalf("the text of another entry: %s", r)
	}
	if r := c.outcome(t, c.tutor, "memory.update", m{"memory_id": c.tutorOwner}); r != "scope_args" {
		t.Fatalf("nothing to change: %s", r)
	}
	if r := c.outcome(t, c.tutor, "memory.update", m{"memory_id": c.tutorOwner, "tags": []string{"Not A Tag"}}); r != "bad_tags" {
		t.Fatalf("a bad tag: %s", r)
	}
	got := testkit.Result[tools.MemoryGetOut](t, c.do(t, c.tutor, "memory.get", m{"memory_id": c.tutorOwner}))
	if !got.Pinned || strings.Join(got.Tags, ",") != "preference" || got.Version != 2 || *got.Text != "Sato prefers short summaries." {
		t.Fatalf("after: %+v", got)
	}
	// The pinned come first in a search.
	found := testkit.Result[tools.MemorySearchOut](t, c.do(t, c.tutor, "memory.search", m{"query": "tables"})).Entries
	if len(found) != 2 || found[0].ID != c.tutorOwner || found[1].ID != other.MemoryID {
		t.Fatalf("the pinned first, then the match: %+v", found)
	}
	// Twenty are pinned at most in one bucket.
	for i := range 19 {
		c.write(t, c.tutor, m{"scope": "owner", "text": "Pinned note " + string(rune('a'+i)), "pinned": true})
	}
	if r := c.outcome(t, c.tutor, "memory.write", m{"scope": "owner", "text": "One pin too many", "pinned": true}); r != "pinned_full" {
		t.Fatalf("a 21st pin: %s", r)
	}
	if r := c.outcome(t, c.tutor, "memory.update", m{"memory_id": other.MemoryID, "pinned": true}); r != "pinned_full" {
		t.Fatalf("a 21st pin by correction: %s", r)
	}
}

// What memory keeps is kept nowhere else: not in the action log, whatever
// the call, and not in the event feed.
func TestMemoryTextStaysOutOfTheActionLog(t *testing.T) {
	c := newMemCast(t, nil)
	c.write(t, c.tutor, c.in(c.yukiTutor, m{"scope": "asker", "text": "Marker-Alpha: Yuki is left-handed.", "tags": []string{"fact"}}))
	c.write(t, c.tutor, m{"scope": "owner", "text": "Marker-Bravo: Sato drinks tea."})
	w := c.write(t, c.tutor, m{"scope": "course", "course_id": c.course, "text": "Marker-Charlie: labs start in week 2."})
	c.do(t, c.tutor, "memory.update", m{"memory_id": w.MemoryID, "course_id": c.course, "text": "Marker-Delta: labs start in week 3."})
	c.do(t, c.tutor, "memory.update", m{"memory_id": c.tutorOwner, "text": "Marker-Echo: Sato prefers tables."})
	c.do(t, c.tutor, "memory.write", m{"scope": "owner", "text": "Marker-Bravo: Sato drinks tea."}) // a duplicate
	if r := c.outcome(t, c.tutor, "memory.write", m{"scope": "owner", "text": "Marker-Foxtrot " + strings.Repeat("x", 1000)}); r != "too_long" {
		t.Fatalf("too long: %s", r)
	}
	c.do(t, c.tutor, "memory.forget", m{"memory_id": c.tutorOwner})
	if n := c.Count(`SELECT count(*) FROM action WHERE action_type LIKE 'memory.%'`); n < 7 {
		t.Fatalf("%d memory actions recorded", n)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE payload::text LIKE '%Marker-%' OR result::text LIKE '%Marker-%'`); n != 0 {
		t.Fatalf("%d action rows hold memory text", n)
	}
	if n := c.Count(`SELECT count(*) FROM event WHERE payload::text LIKE '%Marker-%' OR type LIKE 'memory.%'`); n != 0 {
		t.Fatalf("%d events about memory", n)
	}
	// The rest of each call is on record.
	if n := c.Count(`SELECT count(*) FROM action WHERE action_type = 'memory.write' AND payload->>'scope' = 'asker'
	                  AND payload->>'conversation_id' = $1 AND payload->'tags' = '["fact"]'::jsonb`, c.yukiTutor.String()); n != 1 {
		t.Fatalf("the asker write's record: %d", n)
	}
}

// A secret is refused, and nothing of it is kept: not in the entry, not in
// the action row, not in the refusal.
func TestMemoryRefusesASecret(t *testing.T) {
	c := newMemCast(t, nil)
	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	out := c.MustCall(c.tutor, "memory.write", c.in(c.yukiTutor, m{"scope": "asker", "text": "Yuki's token is " + tok.Full}), "secret")
	if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.InvalidArgument ||
		out.Error.Details["reason"] != "holds_secret" || out.Error.Details["kind"] != "core_token" || strings.Contains(out.Error.Message, tok.Full) {
		t.Fatalf("a token: %+v", out)
	}
	secret := tok.Full[len(tok.Full)-20:]
	if n := c.Count(`SELECT count(*) FROM action WHERE payload::text LIKE '%' || $1 || '%' OR result::text LIKE '%' || $1 || '%'`, secret); n != 0 {
		t.Fatalf("%d action rows hold the token", n)
	}
	if n := c.Count(`SELECT count(*) FROM memory_entry WHERE body LIKE '%' || $1 || '%'`, secret); n != 0 {
		t.Fatal("the token was kept")
	}
	// A retry is the same refusal, from the record.
	again := c.MustCall(c.tutor, "memory.write", c.in(c.yukiTutor, m{"scope": "asker", "text": "Yuki's token is " + tok.Full}), "secret")
	if !again.Replayed || again.Error == nil || again.Error.Details["reason"] != "holds_secret" {
		t.Fatalf("the retry: %+v", again)
	}
}

// Two writers racing at a bucket's limit leave exactly the limit.
func TestMemoryLimitHoldsUnderRacingWriters(t *testing.T) {
	c := newMemCast(t, func(cfg *memory.Config) { cfg.MaxAsker = 5 })
	for i := range 3 {
		c.write(t, c.tutor, c.in(c.yukiTutor, m{"scope": "asker", "text": "Fact " + string(rune('a'+i))}))
	}
	// Two writers are held at the agent's row, and let go at once: with the
	// hold, and the look at who waits, as many as the test's pool of four
	// connections serves.
	release := c.hold(t, `SELECT 1 FROM actor WHERE id = $1 FOR NO KEY UPDATE`, c.tutor)
	done := make(chan pipeline.Outcome, 2)
	for i := range 2 {
		c.start(t, done, c.tutor, "memory.write", c.in(c.yukiTutor, m{"scope": "asker", "text": "Racing fact " + string(rune('a'+i))}))
	}
	c.blocked(t, 2, done)
	release()
	got := map[string]int{}
	for range 2 {
		out := <-done
		if out.Status == domain.StatusExecuted {
			got["ok"]++
		} else {
			got[why(out.Error)]++
		}
	}
	if got["ok"] != 1 || got["memory_full"] != 1 {
		t.Fatalf("racing writers: %v", got)
	}
	if n := c.Count(`SELECT count(*) FROM memory_entry WHERE holder_actor_id = $1 AND scope = 'asker' AND subject_member_id = $2`, c.tutor, c.yukiM); n != 5 {
		t.Fatalf("%d entries about Yuki, the limit is 5", n)
	}
	full := c.MustCall(c.tutor, "memory.write", c.in(c.yukiTutor, m{"scope": "asker", "text": "One more"}), "full")
	if full.Error == nil || full.Error.Details["scope"] != "asker" || full.Error.Details["limit"] != 5 {
		t.Fatalf("memory_full says what is full: %+v", full.Error)
	}
	// The agent's own total is a limit too.
	d := newMemCast(t, func(cfg *memory.Config) { cfg.MaxPerAgent = 3 }) // the tutor holds three
	out := d.MustCall(d.tutor, "memory.write", m{"scope": "owner", "text": "A fourth entry"}, "fourth")
	if out.Error == nil || out.Error.Details["reason"] != "memory_full" || out.Error.Details["scope"] != "agent" {
		t.Fatalf("an agent's total: %+v", out)
	}
}

// The same text again is the entry already there, under a new key too, and
// counts against nothing. Writes are counted by the hour and the day.
func TestMemoryDuplicatesAndRates(t *testing.T) {
	c := newMemCast(t, func(cfg *memory.Config) { cfg.WritesPerHour, cfg.WritesPerDay = 3, 5 })
	// A month on, at twenty past the hour: what the cast wrote is out of
	// the day's count.
	now := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Hour).Add(20 * time.Minute)
	c.P.SetClock(func() time.Time { return now })
	writes := func() int {
		return c.Count(`SELECT coalesce(sum(n), 0) FROM memory_write_count WHERE holder_actor_id = $1`, c.helper)
	}
	before := writes()
	first := c.write(t, c.helper, m{"scope": "owner", "text": "Yuki studies in the evening."})
	dup := c.write(t, c.helper, m{"scope": "owner", "text": "  Yuki studies in the evening.\r\n"})
	if !dup.Duplicate || dup.MemoryID != first.MemoryID || first.Duplicate || writes() != before+1 {
		t.Fatalf("a duplicate: %+v %+v, %d writes", first, dup, writes()-before)
	}
	c.write(t, c.helper, m{"scope": "owner", "text": "Yuki prefers video."})
	c.write(t, c.helper, m{"scope": "owner", "text": "Yuki has a cat."})
	out := c.MustCall(c.helper, "memory.write", m{"scope": "owner", "text": "Yuki has a dog."}, "rate-1")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.RateLimited || out.Error.Details["reason"] != "memory_write_rate" ||
		out.Error.Details["retry_after_seconds"] != 40*60 {
		t.Fatalf("a fourth write in an hour: %+v", out)
	}
	// A duplicate is still the entry there; another agent has its own
	// allowance; forgetting is not counted.
	if again := c.write(t, c.helper, m{"scope": "owner", "text": "Yuki prefers video."}); !again.Duplicate {
		t.Fatal("a duplicate over the rate")
	}
	extra := c.write(t, c.tutor, m{"scope": "owner", "text": "Sato is left-handed."})
	c.do(t, c.helper, "memory.forget", m{"memory_id": first.MemoryID})
	c.do(t, c.tutor, "memory.forget", m{"memory_id": extra.MemoryID})

	now = now.Add(time.Hour)
	c.write(t, c.helper, m{"scope": "owner", "text": "Yuki has a dog."})
	c.write(t, c.helper, m{"scope": "owner", "text": "Yuki has a fish."})
	out = c.MustCall(c.helper, "memory.write", m{"scope": "owner", "text": "Yuki has a bird."}, "rate-2")
	if out.Error == nil || out.Error.Details["reason"] != "memory_write_rate" {
		t.Fatalf("a sixth write in a day: %+v", out)
	}
	// The day's oldest hour leaves its count 24 hours after it began.
	if secs := out.Error.Details["retry_after_seconds"]; secs != int((22*time.Hour + 40*time.Minute).Seconds()) {
		t.Fatalf("retry after %v", secs)
	}
	// Corrections count too.
	if r := c.outcome(t, c.helper, "memory.update", m{"memory_id": c.helperOwner, "text": "Yuki is aiming for an A+."}); r != "memory_write_rate" {
		t.Fatalf("a correction over the rate: %s", r)
	}
	now = now.Add(23 * time.Hour)
	c.write(t, c.helper, m{"scope": "owner", "text": "Yuki has a bird."})
}

// An entry that does not exist, and one the caller may not reach, are
// refused alike.
func TestMemoryNotFoundIsTheSameWhateverTheReason(t *testing.T) {
	c := newMemCast(t, nil)
	frozen := c.write(t, c.tutor, c.in(c.yukiTutor, m{"scope": "asker", "text": "Yuki moved to section B."})).MemoryID
	c.Exec(`UPDATE memory_entry SET purge_after = now() + interval '30 days', purge_reason = 'seat_removed' WHERE id = $1`, frozen)
	for _, name := range []string{"memory.get", "memory.update", "memory.forget"} {
		var first *apperr.Error
		for label, args := range map[string]m{
			"none such":            c.in(c.yukiTutor, m{"memory_id": uuid.New()}),
			"another agent's":      c.in(c.yukiTutor, m{"memory_id": c.unownedAsker}),
			"out of reach":         c.in(c.satoTutor, m{"memory_id": c.tutorAsker}),
			"frozen":               c.in(c.yukiTutor, m{"memory_id": frozen}),
			"another person's own": {"memory_id": c.helperOwner},
		} {
			if name != "memory.get" {
				args["text"] = "Changed."
				if name == "memory.forget" {
					delete(args, "text")
				}
			}
			out, err := c.Call(c.tutor, name, args, "nf-"+uuid.NewString())
			e, _ := apperr.As(err)
			if err == nil {
				e = out.Error
			}
			if e == nil || e.Code != apperr.NotFound || (first != nil && (e.Message != first.Message || len(e.Details) != len(first.Details))) {
				t.Fatalf("%s, %s: %+v %v", name, label, out, err)
			}
			first = e
		}
	}
	if n := c.Count(`SELECT count(*) FROM memory_entry WHERE id = $1`, frozen); n != 1 {
		t.Fatal("a frozen entry was forgotten by the agent")
	}
}
