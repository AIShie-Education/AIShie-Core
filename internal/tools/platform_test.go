package tools_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// An administrator who registered someone last week has only actor.list to
// find them again: actor.get needs the id, and nothing else reaches an actor
// outside a course.
func TestActorList(t *testing.T) {
	b := build(t)
	// Bootstrap makes the system actor; the test platform does not.
	system := b.Actor("system", "system")
	register := func(args m) uuid.UUID {
		return testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", args)).ActorID
	}
	huang := register(m{"kind": "human", "display_name": "HUANG Xiao (黄晓)", "email": "hx2026@example.edu"})
	odd := register(m{"kind": "agent", "display_name": "tutor_50%"})
	slash := register(m{"kind": "human", "display_name": `Back\slash`})
	everyone := []uuid.UUID{b.Root, b.admin, b.sato, b.yuki, b.ken, b.grader, b.tutor, system, huang, odd, slash}

	list := func(actor uuid.UUID, args m) tools.ActorListOut {
		t.Helper()
		return testkit.Result[tools.ActorListOut](t, b.do(t, actor, "actor.list", args))
	}
	ids := func(out tools.ActorListOut) []uuid.UUID {
		got := make([]uuid.UUID, len(out.Actors))
		for i, a := range out.Actors {
			got[i] = a.ID
		}
		return got
	}
	want := func(what string, args m, want ...uuid.UUID) {
		t.Helper()
		out := list(b.admin, args)
		if got := ids(out); !slices.Equal(got, want) {
			t.Fatalf("%s: %v, want %v", what, got, want)
		}
		if out.Next != nil {
			t.Fatalf("%s: a next page after %d of at most 50", what, len(out.Actors))
		}
	}

	// Root and admins see everyone, the system actor included, in the order
	// they were registered.
	want("an admin, unfiltered", m{}, everyone...)
	if got := ids(list(b.Root, m{})); !slices.Equal(got, everyone) {
		t.Fatalf("root sees %v, want %v", got, everyone)
	}
	// Nobody else: seated or not, person or agent, a course is no standing
	// here.
	for name, actor := range map[string]uuid.UUID{"an instructor": b.sato, "a student": b.yuki, "an agent": b.grader} {
		t.Run(name, func(t *testing.T) { b.try(t, actor, "actor.list", m{}, apperr.Forbidden) })
	}

	// Each entry is what actor.get says of the actor.
	all := list(b.admin, m{})
	for _, a := range all.Actors {
		one, _ := json.Marshal(testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": a.ID})))
		listed, _ := json.Marshal(a)
		if string(one) != string(listed) {
			t.Fatalf("listed as %s, actor.get says %s", listed, one)
		}
	}
	h := all.Actors[slices.Index(everyone, huang)]
	if h.Kind != "human" || h.Email == nil || *h.Email != "hx2026@example.edu" || h.Status != "active" ||
		h.PlatformRole != nil || h.CreatedByActorID == nil || *h.CreatedByActorID != b.admin || h.CreatedAt.IsZero() {
		t.Fatalf("HUANG Xiao is listed as %+v", h)
	}
	if r := all.Actors[0]; r.PlatformRole == nil || *r.PlatformRole != "root" || r.CreatedByActorID != nil {
		t.Fatalf("root is listed as %+v", r)
	}

	// Filters.
	want("kind human", m{"kind": "human"}, b.Root, b.admin, b.sato, b.yuki, b.ken, huang, slash)
	want("kind agent", m{"kind": "agent"}, b.grader, b.tutor, odd)
	want("kind system", m{"kind": "system"}, system)
	want("platform_role root", m{"platform_role": "root"}, b.Root)
	want("platform_role admin", m{"platform_role": "admin"}, b.admin)
	want("platform_role none", m{"platform_role": "none"}, everyone[2:]...)
	want("status active", m{"status": "active"}, everyone...)

	// A suspended actor is still listed, and says so.
	b.do(t, b.admin, "actor.suspend", m{"actor_id": b.yuki})
	want("status suspended", m{"status": "suspended"}, b.yuki)
	want("status active, after", m{"status": "active"}, slices.DeleteFunc(slices.Clone(everyone), func(id uuid.UUID) bool { return id == b.yuki })...)
	if y := list(b.admin, m{"q": "yuki"}).Actors; len(y) != 1 || y[0].Status != "suspended" {
		t.Fatalf("Yuki, suspended, is listed as %+v", y)
	}

	// q is part of a name or an address, in any case, and means what it
	// says: % and _ are not wildcards, nor is \ an escape.
	want("q in the name, other case", m{"q": "huang"}, huang)
	want("q with spaces around it", m{"q": "  Huang X  "}, huang)
	want("q in Chinese", m{"q": "黄晓"}, huang)
	want("q in the address, other case", m{"q": "HX2026@EXAMPLE"}, huang)
	want("q %", m{"q": "%"}, odd)
	want("q _", m{"q": "_"}, odd)
	want("q 50%", m{"q": "50%"}, odd)
	want(`q \`, m{"q": `\`}, slash)
	want("q and kind", m{"q": "TUTOR", "kind": "agent"}, b.tutor, odd)
	want("q and another kind", m{"q": "tutor", "kind": "human"})
	want("q empty", m{"q": ""}, everyone...)
	want("q blank", m{"q": "   "}, everyone...)
	want("q at its longest", m{"q": strings.Repeat("黄", 254)})
	// Nothing found is an empty list, not a missing one.
	if raw := string(b.do(t, b.admin, "actor.list", m{"q": "nobody"}).Result); !strings.Contains(raw, `"actors":[]`) {
		t.Fatalf("nothing found: %s", raw)
	}

	for name, args := range map[string]m{
		"q too long":           {"q": strings.Repeat("黄", 255)},
		"an unknown kind":      {"kind": "robot"},
		"an empty kind":        {"kind": ""},
		"an unknown status":    {"status": "deleted"},
		"an unknown role":      {"platform_role": "owner"},
		"a role left empty":    {"platform_role": ""},
		"an unknown parameter": {"role": "admin"},
	} {
		t.Run(name, func(t *testing.T) { b.try(t, b.admin, "actor.list", args, apperr.InvalidArgument) })
	}

	// Paging: next is the last id of a full page, and after it the list goes
	// on where it stopped, filters and all.
	var paged []uuid.UUID
	var after *uuid.UUID
	for pages := 1; ; pages++ {
		args := m{"limit": 4}
		if after != nil {
			args["after"] = *after
		}
		page := list(b.admin, args)
		paged = append(paged, ids(page)...)
		if page.Next == nil {
			if pages != 3 || len(page.Actors) != 3 {
				t.Fatalf("page %d of %d actors ends the list, want the third of three", pages, len(page.Actors))
			}
			break
		}
		if len(page.Actors) != 4 || *page.Next != page.Actors[3].ID {
			t.Fatalf("page %d: %d actors, next %v", pages, len(page.Actors), page.Next)
		}
		after = page.Next
	}
	if !slices.Equal(paged, everyone) {
		t.Fatalf("paged through %v, want %v", paged, everyone)
	}
	first := list(b.admin, m{"kind": "agent", "limit": 2})
	if got := ids(first); !slices.Equal(got, []uuid.UUID{b.grader, b.tutor}) || first.Next == nil || *first.Next != b.tutor {
		t.Fatalf("first page of agents: %v, next %v", got, first.Next)
	}
	want("second page of agents", m{"kind": "agent", "limit": 2, "after": *first.Next}, odd)
	// A page that happens to end the list is full, so it still names a
	// next; the page after it is empty and does not.
	full := list(b.admin, m{"kind": "agent", "limit": 3})
	if len(full.Actors) != 3 || full.Next == nil || *full.Next != odd {
		t.Fatalf("a full last page: %v, next %v", ids(full), full.Next)
	}
	want("after the last", m{"kind": "agent", "limit": 3, "after": odd})
}
