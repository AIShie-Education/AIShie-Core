package tools_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Agents a person owns: what the owner may do with them, and how they come
// into a course as the owner's delegate and never hold more than the owner's
// seat there.

// agent registers an mcp agent owner owns, as the owner: one their own
// tools reach, with tokens they issue.
func (b *built) agent(t *testing.T, owner uuid.UUID, name string) uuid.UUID {
	t.Helper()
	return testkit.Result[tools.ActorOut](t, b.do(t, owner, "agent.create", m{"display_name": name, "hosting": "mcp"})).ActorID
}

// runtimeAgent registers a runtime agent owner owns, as the owner: one the
// site's agent runtime hosts (Host), and people in the site ask.
func (b *built) runtimeAgent(t *testing.T, owner uuid.UUID, name string) uuid.UUID {
	t.Helper()
	return testkit.Result[tools.ActorOut](t, b.do(t, owner, "agent.create", m{"display_name": name, "hosting": "runtime"})).ActorID
}

// reason is why a call was denied or failed, as its error says.
func reason(out pipeline.Outcome) any {
	if out.Error == nil {
		return nil
	}
	return out.Error.Details["reason"]
}

// seatOf is actor's seat in the course that is not removed.
func (b *built) seatOf(t *testing.T, actor uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := b.Pool.QueryRow(t.Context(), `SELECT id FROM course_member WHERE course_id = $1 AND actor_id = $2 AND status <> 'removed'`,
		b.course, actor).Scan(&id); err != nil {
		t.Fatalf("no seat: %v", err)
	}
	return id
}

// delegate seats Yuki's agent as her delegate: she asks, Sato approves.
func (b *built) delegate(t *testing.T, owner, agent uuid.UUID, args m) uuid.UUID {
	t.Helper()
	args["course_id"], args["actor_id"] = b.course, agent
	b.key++
	out := b.MustCall(owner, "member.add_delegate", args, "del-"+uuid.NewString())
	switch out.Status {
	case domain.StatusExecuted:
		return testkit.Result[tools.MemberIDOut](t, out).MemberID
	case domain.StatusProposed:
		d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": out.ActionID, "decision": "approve"}))
		if d.Outcome != domain.StatusExecuted {
			t.Fatalf("approving the delegate: %+v", d)
		}
		var seat tools.MemberIDOut
		if err := json.Unmarshal(d.Result, &seat); err != nil {
			t.Fatal(err)
		}
		return seat.MemberID
	}
	t.Fatalf("member.add_delegate: %+v", out)
	return uuid.Nil
}

func (b *built) memberView(t *testing.T, id uuid.UUID) tools.MemberView {
	t.Helper()
	return testkit.Result[tools.MemberView](t, b.do(t, b.sato, "member.get", m{"course_id": b.course, "member_id": id}))
}

func (b *built) membership(t *testing.T, actor uuid.UUID) tools.Membership {
	t.Helper()
	for _, s := range testkit.Result[tools.MembershipsOut](t, b.do(t, actor, "me.memberships", m{})).Memberships {
		if s.CourseID == b.course {
			return s
		}
	}
	t.Fatal("no membership in the course")
	return tools.Membership{}
}

func TestPeopleLookAfterTheirOwnAgents(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")

	list := func() []tools.AgentSummary {
		return testkit.Result[tools.AgentListOut](t, b.do(t, b.yuki, "agent.list", m{})).Agents
	}
	if got := list(); len(got) != 1 || got[0].ActorID != bot || got[0].DisplayName != "Yuki's helper" || got[0].Status != "active" ||
		got[0].LiveSeats != 0 || got[0].PendingRequests != 0 || got[0].LastSeenAt != nil {
		t.Fatalf("Yuki's agents: %+v", got)
	}
	// It is registered as an agent she owns, and she made it.
	if n := b.Count(`SELECT count(*) FROM actor WHERE id = $1 AND kind = 'agent' AND owner_actor_id = $2 AND created_by_actor_id = $2`, bot, b.yuki); n != 1 {
		t.Fatal("the agent is not recorded as Yuki's")
	}
	b.do(t, b.yuki, "agent.update", m{"actor_id": bot, "display_name": "Essay coach"})
	tok := testkit.Result[tools.IssueTokenOut](t, b.do(t, b.yuki, "agent.issue_token", m{"actor_id": bot, "label": "laptop"}))
	creds := testkit.Result[tools.CredentialListOut](t, b.do(t, b.yuki, "agent.list_credentials", m{"actor_id": bot})).Credentials
	if len(creds) != 1 || creds[0].ID != tok.CredentialID || creds[0].IssuedByID == nil || *creds[0].IssuedByID != b.yuki {
		t.Fatalf("the agent's credentials: %+v", creds)
	}
	// Seen is when it last used a token that still works.
	b.Exec(`UPDATE credential SET last_used_at = now() WHERE id = $1`, tok.CredentialID)
	got := testkit.Result[tools.AgentGetOut](t, b.do(t, b.yuki, "agent.get", m{"actor_id": bot}))
	if got.DisplayName != "Essay coach" || got.LastSeenAt == nil || len(got.Seats) != 0 || len(got.Requests) != 0 {
		t.Fatalf("the agent: %+v", got)
	}

	// Nobody else's, whether it exists or not: the same answer either way.
	for _, actor := range []uuid.UUID{bot, b.tutor, uuid.New()} {
		b.try(t, b.ken, "agent.get", m{"actor_id": actor}, apperr.NotFound)
		b.try(t, b.ken, "agent.update", m{"actor_id": actor, "display_name": "Mine now"}, apperr.NotFound)
		b.try(t, b.ken, "agent.suspend", m{"actor_id": actor}, apperr.NotFound)
		b.try(t, b.ken, "agent.issue_token", m{"actor_id": actor, "label": "stolen"}, apperr.NotFound)
		b.try(t, b.ken, "agent.list_credentials", m{"actor_id": actor}, apperr.NotFound)
		b.try(t, b.ken, "agent.revoke_credential", m{"actor_id": actor, "credential_id": tok.CredentialID}, apperr.NotFound)
		b.try(t, b.ken, "agent.withdraw", m{"actor_id": actor, "course_id": b.course}, apperr.NotFound)
	}
	b.try(t, b.admin, "agent.get", m{"actor_id": bot}, apperr.NotFound)
	b.do(t, b.yuki, "agent.revoke_credential", m{"actor_id": bot, "credential_id": tok.CredentialID})
	b.try(t, b.yuki, "agent.revoke_credential", m{"actor_id": bot, "credential_id": tok.CredentialID}, apperr.NotFound)
	if got := testkit.Result[tools.AgentGetOut](t, b.do(t, b.yuki, "agent.get", m{"actor_id": bot})); got.LastSeenAt != nil {
		t.Fatal("seen by a token that no longer works")
	}

	// Agents do not own agents: an agent of Yuki's, or one nobody owns.
	b.try(t, b.grader, "agent.create", m{"display_name": "Sub-agent", "hosting": "mcp"}, apperr.Forbidden)
	b.try(t, b.yuki, "agent.create", m{"display_name": "  ", "hosting": "mcp"}, apperr.InvalidArgument)
	b.try(t, b.yuki, "agent.update", m{"actor_id": bot, "display_name": ""}, apperr.InvalidArgument)
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'agent.created' AND subject_id = $1 AND course_id IS NULL`, bot); n != 1 {
		t.Fatal("no platform event for the agent's creation")
	}
}

// An installation may keep agents an administrator's to register.
func TestSelfServiceAgentsCanBeTurnedOff(t *testing.T) {
	b := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.DisableAgentSelfService = true }))
	b.try(t, b.yuki, "agent.create", m{"display_name": "Yuki's helper", "hosting": "mcp"}, apperr.Forbidden)
	bot := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register",
		m{"kind": "agent", "hosting": "mcp", "display_name": "Yuki's helper", "owner_actor_id": b.yuki})).ActorID
	// What is registered is looked after by its owner all the same, and
	// the list says who registers them.
	if got := testkit.Result[tools.AgentListOut](t, b.do(t, b.yuki, "agent.list", m{})); len(got.Agents) != 1 || got.Agents[0].ActorID != bot ||
		got.SelfService || got.Limit != tools.DefaultMaxAgentsPerOwner {
		t.Fatalf("Yuki's agents: %+v", got)
	}
	b.do(t, b.yuki, "agent.issue_token", m{"actor_id": bot, "label": "laptop"})
}

// A person has so many agents that are not suspended at most: suspending one
// makes room, and taking it back is held to the limit too.
func TestAPersonHasALimitedNumberOfAgents(t *testing.T) {
	b := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.MaxAgentsPerOwner = 2 }))
	first, _ := b.agent(t, b.yuki, "one"), b.agent(t, b.yuki, "two")
	b.try(t, b.yuki, "agent.create", m{"display_name": "three", "hosting": "mcp"}, apperr.FailedPrecondition)
	if got := testkit.Result[tools.AgentListOut](t, b.do(t, b.yuki, "agent.list", m{})); got.Limit != 2 || !got.SelfService {
		t.Fatalf("what the list says of the limit: %+v", got)
	}
	b.do(t, b.yuki, "agent.suspend", m{"actor_id": first})
	b.agent(t, b.yuki, "three")
	b.try(t, b.yuki, "agent.reactivate", m{"actor_id": first}, apperr.FailedPrecondition)
	// Agents an administrator registered for someone count as theirs.
	for range 2 {
		b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "issued", "owner_actor_id": b.ken})
	}
	b.try(t, b.ken, "agent.create", m{"display_name": "one more", "hosting": "mcp"}, apperr.FailedPrecondition)
	b.agent(t, b.sato, "Sato's")
}

// Calls at once are counted one after another: the limit holds under a race.
func TestTheAgentLimitHoldsUnderARace(t *testing.T) {
	b := buildOn(t, testkit.NewPlatformWithDeps(t, func(d *tools.Deps) { d.MaxAgentsPerOwner = 2 }))
	const calls = 6
	var wg sync.WaitGroup
	outs := make(chan pipeline.Outcome, calls)
	for i := range calls {
		wg.Go(func() {
			out, err := b.Call(b.yuki, "agent.create", m{"display_name": "racer", "hosting": "mcp"}, "race-"+string(rune('a'+i)))
			if err != nil {
				t.Errorf("agent.create: %v", err)
			}
			outs <- out
		})
	}
	wg.Wait()
	close(outs)
	made := 0
	for out := range outs {
		switch {
		case out.Status == domain.StatusExecuted:
			made++
		case out.Error == nil || out.Error.Code != apperr.FailedPrecondition:
			t.Errorf("a racing call: %+v", out)
		}
	}
	if n := b.Count(`SELECT count(*) FROM actor WHERE owner_actor_id = $1`, b.yuki); made != 2 || n != 2 {
		t.Fatalf("%d calls went through and Yuki owns %d agents, want 2 and 2", made, n)
	}
}

// Who suspended an agent decides who may lift it: its owner lifts their own
// suspension, an administrator anyone's, and an administrator's suspension,
// or one from before who made it was recorded, is not the owner's to lift.
func TestAnOwnersSuspensionIsTheirsAndAnAdministratorsIsNot(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	suspender := func() *uuid.UUID {
		t.Helper()
		return testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": bot})).SuspendedByActorID
	}
	mine := func() bool {
		t.Helper()
		return testkit.Result[tools.AgentListOut](t, b.do(t, b.yuki, "agent.list", m{})).Agents[0].SuspendedByMe
	}

	b.do(t, b.yuki, "agent.suspend", m{"actor_id": bot})
	if s := suspender(); s == nil || *s != b.yuki || !mine() {
		t.Fatalf("suspended by Yuki: %v", s)
	}
	if out := b.MustCall(bot, "me.memberships", m{}, ""); out.Status != domain.StatusDenied {
		t.Fatalf("a suspended agent's call: %+v", out)
	}
	b.try(t, b.yuki, "agent.suspend", m{"actor_id": bot}, apperr.Conflict)
	b.do(t, b.yuki, "agent.reactivate", m{"actor_id": bot})
	b.try(t, b.yuki, "agent.reactivate", m{"actor_id": bot}, apperr.Conflict)
	if s := suspender(); s != nil {
		t.Fatalf("an active agent still says who suspended it: %v", s)
	}

	// An administrator takes the owner's suspension over.
	b.do(t, b.yuki, "agent.suspend", m{"actor_id": bot})
	b.do(t, b.admin, "actor.suspend", m{"actor_id": bot})
	if s := suspender(); s == nil || *s != b.admin || mine() {
		t.Fatalf("taken over by the administrator: %v", s)
	}
	b.try(t, b.yuki, "agent.reactivate", m{"actor_id": bot}, apperr.Forbidden)
	b.try(t, b.admin, "actor.suspend", m{"actor_id": bot}, apperr.Conflict)
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": bot})

	// An administrator's own suspension is theirs from the start.
	b.do(t, b.admin, "actor.suspend", m{"actor_id": bot})
	b.try(t, b.yuki, "agent.suspend", m{"actor_id": bot}, apperr.Conflict)
	b.try(t, b.yuki, "agent.reactivate", m{"actor_id": bot}, apperr.Forbidden)
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": bot})

	// A suspension the previous release made records nobody: fail closed.
	b.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, bot)
	b.try(t, b.yuki, "agent.reactivate", m{"actor_id": bot}, apperr.Forbidden)
	// And the previous release reactivating forgets the owner's name, so
	// that its own next suspension is not taken for the owner's.
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": bot})
	b.do(t, b.yuki, "agent.suspend", m{"actor_id": bot})
	b.Exec(`UPDATE actor SET status = 'active' WHERE id = $1`, bot)
	b.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, bot)
	b.try(t, b.yuki, "agent.reactivate", m{"actor_id": bot}, apperr.Forbidden)
}

// An agent's owner is fixed when it is registered: an administrator's
// registration refuses an owner who is not an active person they may give
// one to, and afterwards nothing changes the owner, takes it away or gives
// one to an agent registered without, whoever asks. No tool offers it, and
// the database refuses it.
func TestAnAgentsOwnerNeverChanges(t *testing.T) {
	b := build(t)
	register := func(args m) pipeline.Outcome {
		b.key++
		return b.MustCall(b.admin, "actor.register", args, "reg-"+uuid.NewString())
	}
	failed := func(what string, out pipeline.Outcome, code apperr.Code) {
		t.Helper()
		if out.Status != domain.StatusFailed || out.Error.Code != code {
			t.Fatalf("%s: %+v", what, out)
		}
	}
	failed("a person with an owner", register(m{"kind": "human", "display_name": "x", "owner_actor_id": b.yuki}), apperr.InvalidArgument)
	failed("an agent owned by an agent", register(m{"kind": "agent", "hosting": "mcp", "display_name": "x", "owner_actor_id": b.grader}), apperr.FailedPrecondition)
	failed("an agent owned by root, by an admin", register(m{"kind": "agent", "hosting": "mcp", "display_name": "x", "owner_actor_id": b.Root}), apperr.Forbidden)
	bot := testkit.Result[tools.ActorOut](t, register(m{"kind": "agent", "hosting": "mcp", "display_name": "Lab bot", "owner_actor_id": b.yuki})).ActorID
	own := b.agent(t, b.ken, "Ken's helper")

	if _, ok := b.P.Registry().Get("actor.set_owner"); ok {
		t.Fatal("a tool still changes an agent's owner")
	}
	b.try(t, b.Root, "actor.set_owner", m{"actor_id": bot, "owner_actor_id": b.ken}, apperr.NotFound)
	for _, tc := range []struct {
		what  string
		agent uuid.UUID
		owner *uuid.UUID
	}{
		{"to another person", bot, &b.ken},
		{"to nobody", bot, nil},
		{"one its owner made, to another person", own, &b.yuki},
		{"to a person, for an agent registered with none", b.grader, &b.sato},
	} {
		_, err := b.Pool.Exec(t.Context(), `UPDATE actor SET owner_actor_id = $2 WHERE id = $1`, tc.agent, tc.owner)
		if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != "23001" {
			t.Fatalf("the database, asked to change an agent's owner %s: %v", tc.what, err)
		}
	}
	for agent, owner := range map[uuid.UUID]uuid.UUID{bot: b.yuki, own: b.ken} {
		v := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": agent}))
		if v.OwnerActorID == nil || *v.OwnerActorID != owner {
			t.Fatalf("the agent as an administrator sees it: %+v", v)
		}
	}
	if v := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": b.grader})); v.OwnerActorID != nil {
		t.Fatalf("the agent registered with no owner: %+v", v)
	}
}

// An owner changed before migration 0014 could leave a seat behind in an
// archived course, which the change could not reach. Once the course is
// opened again it counts for nothing, not being its owner's delegate, and
// its owner brings the agent in afresh over it.
func TestASeatLeftByAnOwnerChangedBefore0014CountsForNothing(t *testing.T) {
	b := build(t)
	bot := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "Lab bot"})).ActorID
	seat := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": bot, "preset": "tutor"})).MemberID
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	b.ChangeOwnerAsBefore0014(bot, &b.yuki)
	b.do(t, b.admin, "course.activate", m{"course_id": b.course})
	if out := b.MustCall(bot, "course.get", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied || reason(out) != "principal_not_active" {
		t.Fatalf("the agent's old seat after it changed hands: %+v", out)
	}
	fresh := b.delegate(t, b.yuki, bot, m{})
	if fresh == seat || b.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND status = 'removed'`, seat) != 1 {
		t.Fatal("the old seat was not removed for the fresh one")
	}
}

// An agent knows who owns it, as me.get says: a service that hosts it checks
// that the person handing it the agent's token is that owner. A person, and
// an agent nobody owns, name nobody; every token of an agent names the same
// owner, which never changes.
func TestAnAgentKnowsWhoOwnsIt(t *testing.T) {
	b := build(t)
	authn := auth.NewAuthenticator(b.Pool, 0)
	// me is me.get as the actor a token belongs to, as a hosting service
	// calls it: the token first, then the tool as whoever it names.
	me := func(token string) (tools.MeOut, string) {
		t.Helper()
		p, err := authn.Authenticate(t.Context(), token)
		if err != nil {
			t.Fatalf("the token does not authenticate: %v", err)
		}
		out := b.do(t, p.ActorID, "me.get", m{})
		return testkit.Result[tools.MeOut](t, out), string(out.Result)
	}
	token := func(issuer uuid.UUID, tool string, agent uuid.UUID) string {
		t.Helper()
		return testkit.Result[tools.IssueTokenOut](t, b.do(t, issuer, tool, m{"actor_id": agent, "label": "runtime"})).Token
	}
	names := func(who string, got tools.MeOut, raw string, want *uuid.UUID) {
		t.Helper()
		switch {
		case want == nil && (got.OwnerActorID != nil || strings.Contains(raw, "owner_actor_id")):
			t.Errorf("%s names an owner: %s", who, raw)
		case want != nil && (got.OwnerActorID == nil || *got.OwnerActorID != *want):
			t.Errorf("%s: owner %v, want %s", who, got.OwnerActorID, *want)
		}
	}

	bot := b.agent(t, b.yuki, "Yuki's helper")
	yukis := token(b.yuki, "agent.issue_token", bot)
	got, raw := me(yukis)
	names("Yuki's own agent", got, raw, &b.yuki)
	if got.ID != bot || got.Kind != "agent" || got.Status != "active" {
		t.Fatalf("the agent's me.get: %s", raw)
	}
	sess, err := authn.StartSession(t.Context(), b.yuki, "password login")
	if err != nil {
		t.Fatal(err)
	}
	got, raw = me(sess.Token)
	names("a person, signed in", got, raw, nil)
	got, raw = me(token(b.admin, "actor.issue_token", b.grader))
	names("an agent registered with no owner", got, raw, nil)
	registered := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register",
		m{"kind": "agent", "hosting": "mcp", "display_name": "Ken's lab bot", "owner_actor_id": b.ken})).ActorID
	kens := token(b.admin, "actor.issue_token", registered)
	got, raw = me(kens)
	names("an agent an administrator registered for Ken", got, raw, &b.ken)

	// A token issued later names the same owner: it never changes.
	got, raw = me(token(b.admin, "actor.issue_token", bot))
	names("Yuki's own agent, by an administrator's token", got, raw, &b.yuki)

	// Its owner suspended, an agent still names him, since he still owns it;
	// Core vouches for no suspended person to a runtime, so he cannot
	// connect it there. Nothing else of his is shown: me.get is the agent's
	// own row, and his id is all it says of him.
	b.do(t, b.admin, "actor.suspend", m{"actor_id": b.ken})
	got, raw = me(kens)
	names("an agent whose owner is suspended", got, raw, &b.ken)
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "owner_actor_id")
	if want := (map[string]any{"id": registered.String(), "kind": "agent", "hosting": "mcp", "display_name": "Ken's lab bot", "status": "active"}); !reflect.DeepEqual(fields, want) {
		t.Fatalf("me.get says more than the agent's own row: %s", raw)
	}

	// The catalogue says so, as a field that may be absent.
	tl, _ := b.P.Registry().Get("me.get")
	if prop, ok := tl.OutputSchema.Properties["owner_actor_id"]; !ok || prop.Description == "" || slices.Contains(tl.OutputSchema.Required, "owner_actor_id") {
		t.Fatalf("me.get's output schema: %+v", tl.OutputSchema)
	}
}

// An agent someone owns holds no platform role: owning it and holding its
// tokens would be more than a seat.
func TestAnOwnedAgentHoldsNoPlatformRole(t *testing.T) {
	b := build(t)
	out := b.MustCall(b.Root, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "Admin bot", "platform_role": "admin", "owner_actor_id": b.yuki}, "reg")
	if out.Status != domain.StatusFailed || out.Error.Code != apperr.InvalidArgument {
		t.Fatalf("an owned agent with a platform role: %+v", out)
	}
	// Registered with a platform role and no owner, it is given none later
	// either (TestAnAgentsOwnerNeverChanges).
	b.do(t, b.Root, "actor.register", m{"kind": "agent", "hosting": "mcp", "display_name": "Admin bot", "platform_role": "admin"})
}

// A student asks to bring her agent in; an instructor approves; the agent is
// seated as her delegate with what was asked for, and reaches her work and
// no one else's.
func TestAStudentBringsHerAgentInWithApproval(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	b.submit(t, b.yuki, "Yuki's essay")
	b.submit(t, b.ken, "Ken's essay")

	asked := b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": bot}, "ask")
	if asked.Status != domain.StatusProposed {
		t.Fatalf("a student bringing her agent: %+v", asked)
	}
	// What waits for a decision is the seat as it would be made, and says
	// whose agent it is.
	var pinned tools.MemberAddDelegateIn
	var raw []byte
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload FROM action WHERE id = $1`, asked.ActionID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &pinned); err != nil {
		t.Fatal(err)
	}
	if pinned.PresetID == nil || pinned.Perms["submission_read"] != "autonomous" || pinned.Perms["grade_submit"] != "denied" ||
		pinned.Perms["conversation_answer"] != "autonomous" || pinned.StudentScope == nil || *pinned.StudentScope != "listed" ||
		pinned.ListedStudents == nil || len(*pinned.ListedStudents) != 1 || (*pinned.ListedStudents)[0] != b.yukiM ||
		pinned.AgentDisplayName == nil || *pinned.AgentDisplayName != "Yuki's helper" || pinned.OwnerDisplayName == nil || *pinned.OwnerDisplayName != "Yuki" {
		t.Fatalf("the proposal: %s", raw)
	}
	got := testkit.Result[tools.AgentGetOut](t, b.do(t, b.yuki, "agent.get", m{"actor_id": bot}))
	if len(got.Requests) != 1 || got.Requests[0].ActionID != *asked.ActionID || got.Requests[0].CourseID != b.course {
		t.Fatalf("the waiting request, from the agent: %+v", got.Requests)
	}
	if l := testkit.Result[tools.AgentListOut](t, b.do(t, b.yuki, "agent.list", m{})).Agents[0]; l.PendingRequests != 1 {
		t.Fatalf("the agent listed: %+v", l)
	}

	d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": asked.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusExecuted {
		t.Fatalf("Sato approving: %+v", d)
	}
	seat := b.seatOf(t, bot)
	v := b.memberView(t, seat)
	if v.PrincipalMemberID == nil || *v.PrincipalMemberID != b.yukiM || v.Role != "assistant" || v.StudentScope != "listed" ||
		len(v.ListedStudents) != 1 || v.ListedStudents[0] != b.yukiM || v.Perms["member_manage"] != "denied" ||
		v.OwnerActorID == nil || *v.OwnerActorID != b.yuki || v.OwnerName == nil || *v.OwnerName != "Yuki" {
		t.Fatalf("the delegate's seat: %+v", v)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'member.added' AND subject_id = $1
		AND payload->>'delegate' = 'true' AND payload->>'principal_member_id' = $2::text`, seat, b.yukiM); n != 1 {
		t.Fatal("the event says nothing of whose delegate it is")
	}
	if got := testkit.Result[tools.AgentGetOut](t, b.do(t, b.yuki, "agent.get", m{"actor_id": bot})); len(got.Seats) != 1 ||
		got.Seats[0].MemberID != seat || got.Seats[0].Preset == nil || *got.Seats[0].Preset != "delegate" ||
		got.Seats[0].Perms["submission_read"] != "autonomous" || len(got.Requests) != 0 {
		t.Fatalf("the agent's seats: %+v", got)
	}

	// It reads Yuki's work, and nobody else's.
	subs := testkit.Result[tools.SubmissionListOut](t, b.do(t, bot, "submission.list", m{"course_id": b.course})).Submissions
	if len(subs) != 1 || subs[0].StudentMemberID != b.yukiM {
		t.Fatalf("what Yuki's agent reads: %+v", subs)
	}
	// And knows what it is.
	me := b.membership(t, bot)
	if me.PrincipalMemberID == nil || *me.PrincipalMemberID != b.yukiM || me.Perms["grade_read"] != "autonomous" ||
		me.Perms["agent_delegate"] != "denied" || me.Perms["grade_submit"] != "denied" {
		t.Fatalf("the agent's own membership: %+v", me)
	}
	// Seated once: asking again is refused before anyone is asked.
	b.try(t, b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": bot}, apperr.Conflict)
}

// What a delegate is seated with is worked out from what its principal
// holds: a preset's levels are cut down to the principal's, and a level or a
// reach named beyond them is refused; someone who does not manage members
// gives their agent, beyond what the delegate preset gives, nothing it may do
// without its every action being confirmed.
func TestADelegateIsSeatedWithinItsPrincipal(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	ask := func(args m) m {
		args["course_id"], args["actor_id"] = b.course, bot
		return args
	}
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM,
		"perms": m{"submission_read": "confirm_required", "conversation_ask": "denied"}})
	until := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.yukiM, "expires_at": until})

	defaults := testkit.Result[tools.DelegateDefaultsOut](t, b.do(t, b.yuki, "member.delegate_defaults", m{"course_id": b.course}))
	if defaults.Preset != "delegate" || defaults.Role != "assistant" || defaults.Perms["submission_read"] != "confirm_required" ||
		defaults.Perms["grade_read"] != "autonomous" || defaults.Perms["conversation_answer"] != "denied" ||
		defaults.StudentScope != "listed" || len(defaults.ListedStudents) != 1 || defaults.ListedStudents[0] != b.yukiM ||
		defaults.ExpiresAt == nil || !defaults.ExpiresAt.Equal(until) || defaults.Level != "confirm_required" {
		t.Fatalf("what Yuki's agent would get: %+v", defaults)
	}

	b.try(t, b.yuki, "member.add_delegate", ask(m{"perms": m{"grade_submit": "autonomous"}}), apperr.Forbidden)
	b.try(t, b.yuki, "member.add_delegate", ask(m{"perms": m{"submission_read": "autonomous"}}), apperr.Forbidden)
	b.try(t, b.yuki, "member.add_delegate", ask(m{"perms": m{"member_manage": "autonomous"}}), apperr.Forbidden)
	// Within Yuki's own, but more than the delegate preset gives.
	b.try(t, b.yuki, "member.add_delegate", ask(m{"perms": m{"submission_write": "autonomous"}}), apperr.Forbidden)
	b.try(t, b.yuki, "member.add_delegate", ask(m{"listed_students": []uuid.UUID{b.kenM}}), apperr.Forbidden)
	b.try(t, b.yuki, "member.add_delegate", ask(m{"student_scope": "all"}), apperr.Forbidden)
	b.try(t, b.yuki, "member.add_delegate", ask(m{"expires_at": until.Add(time.Hour)}), apperr.Forbidden)
	// Another preset is cut down the same way: to confirm_required beyond
	// what the delegate preset gives, and to nothing Yuki does not hold.
	cut := testkit.Result[tools.DelegateDefaultsOut](t, b.do(t, b.yuki, "member.delegate_defaults", m{"course_id": b.course, "preset": "instructor"}))
	if cut.Perms["submission_write"] != "confirm_required" || cut.Perms["document_read"] != "autonomous" ||
		cut.Perms["grade_submit"] != "denied" || cut.Perms["member_manage"] != "denied" || cut.Perms["agent_delegate"] != "denied" {
		t.Fatalf("Yuki's agent with the instructor preset: %+v", cut.Perms)
	}
	// Named lower, it is asked for as named.
	out := b.MustCall(b.yuki, "member.add_delegate", ask(m{"perms": m{"grade_read": "denied"}, "expires_at": until.Add(-time.Hour)}), "lower")
	if out.Status != domain.StatusProposed {
		t.Fatalf("asking for less: %+v", out)
	}
	b.do(t, b.yuki, "action.withdraw", m{"course_id": b.course, "action_id": out.ActionID})

	// Someone else's agent, a suspended one, or none: refused before anyone
	// is asked.
	kens := b.agent(t, b.ken, "Ken's helper")
	b.try(t, b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": kens}, apperr.NotFound)
	b.try(t, b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": b.tutor}, apperr.NotFound)
	b.do(t, b.yuki, "agent.suspend", m{"actor_id": bot})
	b.try(t, b.yuki, "member.add_delegate", ask(m{}), apperr.FailedPrecondition)
	b.do(t, b.yuki, "agent.reactivate", m{"actor_id": bot})

	// An instructor, who reaches the whole class, gives a course agent the
	// material and nobody's work, at once; and may give their own agent more
	// than the delegate preset, within what they hold.
	tutor := b.agent(t, b.sato, "Course tutor")
	seat := b.delegate(t, b.sato, tutor, m{"preset": "course_tutor"})
	if v := b.memberView(t, seat); v.StudentScope != "listed" || len(v.ListedStudents) != 0 || v.Perms["document_read"] != "autonomous" ||
		v.Perms["conversation_answer"] != "autonomous" || v.Perms["submission_read"] != "denied" || v.PrincipalMemberID == nil || *v.PrincipalMemberID != b.satoM {
		t.Fatalf("the course's tutor: %+v", v)
	}
	grader := b.agent(t, b.sato, "Sato's grader")
	if v := b.memberView(t, b.delegate(t, b.sato, grader, m{"perms": m{"grade_submit": "confirm_required"}})); v.Perms["grade_submit"] != "confirm_required" {
		t.Fatalf("Sato's grading agent: %+v", v)
	}
	// A delegate brings nothing of its own.
	own := b.MustCall(tutor, "member.add_delegate", m{"course_id": b.course, "actor_id": tutor}, "own")
	if own.Status != domain.StatusDenied || reason(own) != "permission_denied" {
		t.Fatalf("a delegate bringing an agent: %+v", own)
	}
}

// Asking again for an agent seated already is refused as a conflict before
// anyone is asked, even once its principal's seat has been given an end the
// agent's seat does not share.
func TestAnAgentSeatedAlreadyIsRefusedOnceItsPrincipalHasAnEnd(t *testing.T) {
	b := build(t)
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"agent_delegate": "autonomous"}})
	bot := b.agent(t, b.yuki, "Yuki's helper")
	b.delegate(t, b.yuki, bot, m{})
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.yukiM, "expires_at": time.Now().Add(30 * 24 * time.Hour)})
	b.try(t, b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": bot}, apperr.Conflict)
}

// An approval seats what was asked for only if the proposer still holds it.
func TestAnApprovalSeatsOnlyWhatTheProposerStillHolds(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	asked := b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": bot}, "ask")
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"grade_read": "denied"}})
	d := testkit.Result[pipeline.DecideOut](t, b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": asked.ActionID, "decision": "approve"}))
	if d.Outcome != domain.StatusFailed || d.Error == nil || d.Error.Code != apperr.Forbidden {
		t.Fatalf("approving what Yuki no longer holds: %+v", d)
	}
	if n := b.Count(`SELECT count(*) FROM course_member WHERE actor_id = $1`, bot); n != 0 {
		t.Fatal("the agent was seated with more than Yuki holds")
	}
}

// A proposer takes back what nobody has decided yet, and only their own.
func TestAProposerWithdrawsTheirOwnProposal(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	asked := b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": bot}, "ask")
	b.try(t, b.ken, "action.withdraw", m{"course_id": b.course, "action_id": asked.ActionID}, apperr.Forbidden)
	b.try(t, b.sato, "action.withdraw", m{"course_id": b.course, "action_id": asked.ActionID}, apperr.Forbidden)
	b.do(t, b.yuki, "action.withdraw", m{"course_id": b.course, "action_id": asked.ActionID})
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled' AND result->'error'->'details'->>'reason' = 'withdrawn'`, asked.ActionID); n != 1 {
		t.Fatal("the proposal is not cancelled as withdrawn")
	}
	// Its proposer learns of it the way they learn of any end of a proposal.
	evs := types(feed(t, b, b.yuki))
	if evs["action.cancelled"] != 1 {
		t.Fatalf("Yuki's feed: %v", evs)
	}
	b.try(t, b.yuki, "action.withdraw", m{"course_id": b.course, "action_id": asked.ActionID}, apperr.Conflict)
	d := b.MustCall(b.sato, "action.decide", m{"course_id": b.course, "action_id": asked.ActionID, "decision": "approve"}, "late")
	if d.Status != domain.StatusFailed || d.Error.Code != apperr.Conflict {
		t.Fatalf("approving a withdrawn proposal: %+v", d)
	}
	b.try(t, b.yuki, "action.withdraw", m{"course_id": b.course, "action_id": uuid.New()}, apperr.NotFound)
}

// An agent's owner takes back what it proposed while nobody has decided it,
// as the agent may itself: it acts only as their delegate. Nobody else of
// the party does — another agent of the owner's, the agent for its owner —
// nor anyone outside it, and nobody takes back what has been decided.
func TestAnOwnerWithdrawsTheirAgentsProposal(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.sato, "Sato's helper")
	b.delegate(t, b.sato, bot, m{"perms": m{"grade_submit": "confirm_required"}, "student_scope": "all"})
	sibling := b.agent(t, b.sato, "Sato's other helper")
	b.delegate(t, b.sato, sibling, m{})
	yukiWork, kenWork := b.submit(t, b.yuki, "Yuki's essay"), b.submit(t, b.ken, "Ken's essay")
	propose := func(actor, work uuid.UUID, key string) *uuid.UUID {
		t.Helper()
		out := b.MustCall(actor, "grade.submit", m{"course_id": b.course, "submission_id": work, "score": 70}, key)
		if out.Status != domain.StatusProposed {
			t.Fatalf("%s: %+v", key, out)
		}
		return out.ActionID
	}
	withdraw := func(action *uuid.UUID) m { return m{"course_id": b.course, "action_id": action} }

	asked := propose(bot, yukiWork, "bot-yuki")
	b.try(t, sibling, "action.withdraw", withdraw(asked), apperr.Forbidden)
	b.try(t, b.ken, "action.withdraw", withdraw(asked), apperr.Forbidden)
	b.do(t, b.sato, "action.withdraw", withdraw(asked))
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled'
		AND result->'error'->'details'->>'reason' = 'withdrawn' AND result->'error'->'details'->>'by_owner' = 'true'`, asked); n != 1 {
		t.Fatal("the proposal is not cancelled as withdrawn by its owner")
	}
	// The agent learns of it as of any end of a proposal, and that its
	// owner took it back.
	var seen bool
	for _, e := range feed(t, b, bot) {
		var p struct {
			Reason  string `json:"reason"`
			ByOwner bool   `json:"by_owner"`
		}
		if e.Type == "action.cancelled" && e.ActionID != nil && *e.ActionID == *asked && json.Unmarshal(e.Payload, &p) == nil {
			seen = p.Reason == "withdrawn" && p.ByOwner
		}
	}
	if !seen {
		t.Fatal("the agent's feed does not say its owner withdrew its proposal")
	}
	b.try(t, b.sato, "action.withdraw", withdraw(asked), apperr.Conflict)

	// Decided, it is nobody's to take back.
	rejected := propose(bot, kenWork, "bot-ken")
	b.do(t, b.sato, "action.decide", m{"course_id": b.course, "action_id": rejected, "decision": "reject"})
	b.try(t, b.sato, "action.withdraw", withdraw(rejected), apperr.Conflict)
	b.try(t, bot, "action.withdraw", withdraw(rejected), apperr.Conflict)

	// An agent takes back nothing its owner proposed.
	b.Exec(`UPDATE course_member SET perm_grade_submit = 'confirm_required' WHERE id = $1`, b.satoM)
	own := propose(b.sato, yukiWork, "sato-yuki")
	b.try(t, bot, "action.withdraw", withdraw(own), apperr.Forbidden)
	b.try(t, sibling, "action.withdraw", withdraw(own), apperr.Forbidden)
	b.do(t, b.sato, "action.withdraw", withdraw(own))
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled' AND result->'error'->'details' ? 'by_owner'`, own); n != 0 {
		t.Fatal("a proposer's own withdrawal says an owner made it")
	}
}

// An agent someone owns is seated only as their delegate, by them.
func TestAnOwnedAgentIsSeatedOnlyByItsOwner(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": bot, "preset": "tutor", "listed_students": []uuid.UUID{b.yukiM}},
		apperr.FailedPrecondition)
	other := testkit.Result[tools.CourseCreateOut](t, b.do(t, b.admin, "course.create",
		m{"dept_id": b.dept, "term_id": b.term, "code": "CS102", "title": "More computing"})).CourseID
	b.try(t, b.admin, "course.seat_instructor", m{"course_id": other, "actor_id": bot}, apperr.FailedPrecondition)
	// Whoever seats members is told whose it is.
	look := testkit.Result[tools.MemberLookupActorOut](t, b.do(t, b.sato, "member.lookup_actor", m{"course_id": b.course, "actor_id": bot}))
	if look.OwnerActorID == nil || *look.OwnerActorID != b.yuki || look.OwnerName == nil || *look.OwnerName != "Yuki" || look.MemberID != nil {
		t.Fatalf("looking the agent up: %+v", look)
	}
	// Yuki cannot bring it where she is not seated.
	if out := b.MustCall(b.yuki, "member.add_delegate", m{"course_id": other, "actor_id": bot}, "elsewhere"); out.Status != domain.StatusDenied {
		t.Fatalf("bringing it into a course she is not in: %+v", out)
	}
}

// A delegate's reach is its principal's too, in every list, whatever its own
// row says: the lists are filtered in SQL by both, and lean on nothing a
// write keeps in step.
func TestADelegateSeesNoMoreThanItsPrincipal(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, bot, m{})
	yukis, kens := b.submit(t, b.yuki, "Yuki's essay"), b.submit(t, b.ken, "Ken's essay")
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": yukis, "score": 80})
	b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": kens, "score": 70})
	b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": b.hw3})

	students := func() map[uuid.UUID]int {
		t.Helper()
		out := map[uuid.UUID]int{}
		for _, s := range testkit.Result[tools.SubmissionListOut](t, b.do(t, bot, "submission.list", m{"course_id": b.course})).Submissions {
			out[s.StudentMemberID]++
		}
		for _, g := range testkit.Result[tools.GradeListOut](t, b.do(t, bot, "grade.list", m{"course_id": b.course})).Grades {
			out[g.StudentMemberID]++
		}
		for _, e := range feed(t, b, bot) {
			if e.StudentMemberID != nil {
				out[*e.StudentMemberID]++
			}
		}
		return out
	}
	if got := students(); got[b.yukiM] == 0 || got[b.kenM] != 0 {
		t.Fatalf("what the delegate sees, by student: %v", got)
	}
	// Its own row opened to the whole class behind everyone's back: still
	// nothing of Ken's.
	b.Exec(`UPDATE course_member SET student_scope = 'all' WHERE id = $1`, seat)
	if got := students(); got[b.yukiM] == 0 || got[b.kenM] != 0 {
		t.Fatalf("what a delegate widened by hand sees: %v", got)
	}
	// Its principal's narrowed, as the previous release narrows it, touching
	// nothing of the delegate's: it follows.
	b.Exec(`DELETE FROM member_student_scope WHERE member_id = $1`, b.yukiM)
	if got := students(); len(got) != 0 {
		t.Fatalf("what the delegate of someone who reaches nobody sees: %v", got)
	}
	b.Exec(`INSERT INTO member_student_scope (member_id, student_member_id) VALUES ($1, $1)`, b.yukiM)
	// Paused with its principal, and back with her.
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": b.yukiM})
	if out := b.MustCall(bot, "submission.list", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied || reason(out) != "principal_not_active" {
		t.Fatalf("the delegate of a paused student: %+v", out)
	}
	if me := b.membership(t, bot); me.Perms["document_read"] != "denied" {
		t.Fatalf("the delegate of a paused student is told it may read: %+v", me.Perms)
	}
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": b.yukiM})
	b.do(t, bot, "submission.list", m{"course_id": b.course})
}

// Whoever manages members changes a delegate's seat like any other, and a
// change that widens it is held to its principal's seat as well as to their
// own; narrowing is always allowed.
func TestManagingADelegateIsHeldToItsPrincipal(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, bot, m{})
	perms := func(p m) m { return m{"course_id": b.course, "member_id": seat, "perms": p} }
	scope := func(args m) m {
		args["course_id"], args["member_id"] = b.course, seat
		return args
	}
	b.try(t, b.sato, "member.update_perms", perms(m{"grade_submit": "confirm_required"}), apperr.Forbidden)
	b.try(t, b.sato, "member.update_perms", perms(m{"rubric_read": "autonomous"}), apperr.Forbidden)
	b.try(t, b.sato, "member.update_perms", perms(m{"member_manage": "autonomous"}), apperr.Forbidden)
	b.try(t, b.sato, "member.update_perms", perms(m{"agent_delegate": "confirm_required"}), apperr.Forbidden)
	b.do(t, b.sato, "member.update_perms", perms(m{"grade_read": "denied"}))
	b.do(t, b.sato, "member.update_perms", perms(m{"grade_read": "autonomous", "submission_write": "confirm_required"}))
	// Answering is capped by her asking, not by her answering.
	b.do(t, b.sato, "member.update_perms", perms(m{"conversation_answer": "autonomous"}))

	b.try(t, b.sato, "member.rescope", scope(m{"student_scope": "all"}), apperr.Forbidden)
	b.try(t, b.sato, "member.rescope", scope(m{"listed_students": []uuid.UUID{b.yukiM, b.kenM}}), apperr.Forbidden)
	b.do(t, b.sato, "member.rescope", scope(m{"listed_students": []uuid.UUID{}}))
	b.do(t, b.sato, "member.rescope", scope(m{"listed_students": []uuid.UUID{b.yukiM}}))

	until := time.Now().Add(10 * 24 * time.Hour)
	b.do(t, b.sato, "member.rescope", m{"course_id": b.course, "member_id": b.yukiM, "expires_at": until})
	b.do(t, b.sato, "member.rescope", scope(m{"expires_at": until.Add(-time.Hour)}))
	b.try(t, b.sato, "member.rescope", scope(m{"clear_expiry": true}), apperr.Forbidden)
	b.try(t, b.sato, "member.rescope", scope(m{"expires_at": until.Add(time.Hour)}), apperr.Forbidden)

	// Resuming is a grant of the whole seat, held to the principal's too.
	b.do(t, b.sato, "member.pause", m{"course_id": b.course, "member_id": seat})
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"submission_write": "denied"}})
	b.try(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": seat}, apperr.Forbidden)
	b.do(t, b.sato, "member.update_perms", perms(m{"submission_write": "denied"}))
	b.do(t, b.sato, "member.resume", m{"course_id": b.course, "member_id": seat})
}

// A delegate lives no longer than its principal: removing a member removes
// their delegates, and cancels what those had proposed.
func TestRemovingAPrincipalRemovesTheirDelegates(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, bot, m{})
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"submission_write": "confirm_required"}})
	waiting := b.MustCall(bot, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.yukiM, "body": "draft"}, "bot-draft")
	if waiting.Status != domain.StatusProposed {
		t.Fatalf("the delegate's draft: %+v", waiting)
	}
	removed := testkit.Result[tools.MemberRemoveOut](t, b.do(t, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.yukiM}))
	if removed.CancelledProposals != 1 {
		t.Fatalf("removing Yuki: %+v", removed)
	}
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND status = 'removed'`, seat); n != 1 {
		t.Fatal("Yuki's agent kept its seat")
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND status = 'cancelled'
		AND result->'error'->'details'->>'reason' = 'member_removed'`, waiting.ActionID); n != 1 {
		t.Fatal("the delegate's proposal was not cancelled")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'member.removed' AND subject_id = $1 AND payload->>'reason' = 'principal_removed'`, seat); n != 1 {
		t.Fatal("no event says the delegate went with its principal")
	}

	// The previous release removes a principal knowing nothing of its
	// delegates, and the database removes them with it: the agent counts for
	// nothing at once, and its owner, seated again, brings it in afresh.
	yuki := testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": b.yuki, "preset": "student"})).MemberID
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": yuki, "perms": m{"agent_delegate": "autonomous"}})
	left := b.delegate(t, b.yuki, bot, m{})
	b.Exec(`UPDATE course_member SET status = 'removed' WHERE id = $1`, yuki)
	if n := b.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND status = 'removed'`, left); n != 1 {
		t.Fatal("the delegate outlived its principal's removal by the previous release")
	}
	if out := b.MustCall(bot, "course.get", m{"course_id": b.course}, ""); out.Status != domain.StatusDenied || reason(out) != "not_a_member" {
		t.Fatalf("the delegate of a removed seat: %+v", out)
	}
	yuki = testkit.Result[tools.MemberIDOut](t, b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": b.yuki, "preset": "student"})).MemberID
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": yuki, "perms": m{"agent_delegate": "autonomous"}})
	fresh := b.delegate(t, b.yuki, bot, m{})
	if v := b.memberView(t, fresh); v.PrincipalMemberID == nil || *v.PrincipalMemberID != yuki {
		t.Fatalf("the fresh seat: %+v", v)
	}
}

// An owner takes their agent out of a course: its seat is removed and what it
// proposed is cancelled; an archived course takes no such change.
func TestAnOwnerWithdrawsTheirAgent(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, bot, m{})
	b.try(t, b.ken, "agent.withdraw", m{"actor_id": bot, "course_id": b.course}, apperr.NotFound)
	b.try(t, b.yuki, "agent.withdraw", m{"actor_id": bot, "course_id": uuid.New()}, apperr.NotFound)
	if got := testkit.Result[tools.MemberRemoveOut](t, b.do(t, b.yuki, "agent.withdraw", m{"actor_id": bot, "course_id": b.course})); got.CancelledProposals != 0 {
		t.Fatalf("withdrawing: %+v", got)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'member.removed' AND subject_id = $1 AND payload->>'reason' = 'withdrawn'`, seat); n != 1 {
		t.Fatal("the seat was not removed as withdrawn")
	}
	b.try(t, b.yuki, "agent.withdraw", m{"actor_id": bot, "course_id": b.course}, apperr.NotFound)

	b.delegate(t, b.yuki, bot, m{})
	b.do(t, b.admin, "course.archive", m{"course_id": b.course})
	if out := b.MustCall(b.yuki, "agent.withdraw", m{"actor_id": bot, "course_id": b.course}, "archived"); out.Status != domain.StatusDenied || reason(out) != "course_archived" {
		t.Fatalf("withdrawing from an archived course: %+v", out)
	}
}

// Whether students may bring agents of their own is one call for the whole
// class, each seat held to the rules of a change to it, all or nothing.
func TestStudentAgentsAreSetForTheWholeClass(t *testing.T) {
	b := build(t)
	bot := b.agent(t, b.yuki, "Yuki's helper")
	bulk := func(actor uuid.UUID, role string, perms m) tools.MemberUpdatePermsBulkOut {
		return testkit.Result[tools.MemberUpdatePermsBulkOut](t, b.do(t, actor, "member.update_perms_bulk", m{"course_id": b.course, "role": role, "perms": perms}))
	}
	if got := bulk(b.sato, "student", m{"agent_delegate": "denied"}); got.Updated != 2 {
		t.Fatalf("turning student agents off: %+v", got)
	}
	if out := b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": bot}, "off"); out.Status != domain.StatusDenied {
		t.Fatalf("bringing an agent while they are off: %+v", out)
	}
	bulk(b.sato, "student", m{"agent_delegate": "autonomous"})
	if out := b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": bot}, "on"); out.Status != domain.StatusExecuted {
		t.Fatalf("bringing an agent while they are allowed: %+v", out)
	}
	// Allowed, and looked at afterwards.
	b.do(t, b.yuki, "agent.withdraw", m{"actor_id": bot, "course_id": b.course})
	bulk(b.sato, "student", m{"agent_delegate": "pending_review"})
	if out := b.MustCall(b.yuki, "member.add_delegate", m{"course_id": b.course, "actor_id": bot}, "review"); out.Status != domain.StatusExecuted ||
		out.ReviewState != domain.ReviewPending {
		t.Fatalf("bringing an agent to be reviewed after: %+v", out)
	}
	// Nobody's own seat, and no more than the caller holds, for any seat.
	if got := bulk(b.sato, "instructor", m{"grade_post": "denied"}); got.Updated != 0 {
		t.Fatalf("changing the instructors, as the only instructor: %+v", got)
	}
	helper := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register", m{"kind": "human", "display_name": "Helper"})).ActorID
	b.do(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": helper, "preset": "ta", "perms": m{"member_manage": "autonomous"}})
	b.try(t, helper, "member.update_perms_bulk", m{"course_id": b.course, "role": "student", "perms": m{"agent_delegate": "confirm_required", "grade_post": "autonomous"}},
		apperr.Forbidden)
	if n := b.Count(`SELECT count(*) FROM course_member WHERE role = 'student' AND perm_agent_delegate = 'pending_review' AND status = 'active'`); n != 2 {
		t.Fatal("a refused change to the class changed some of it")
	}
	b.try(t, b.sato, "member.update_perms_bulk", m{"course_id": b.course, "role": "dean", "perms": m{"agent_delegate": "denied"}}, apperr.InvalidArgument)
	b.try(t, b.sato, "member.update_perms_bulk", m{"course_id": b.course, "role": "student", "perms": m{}}, apperr.InvalidArgument)
}

// A member may always know their own seat as authorization reads it.
func TestAMembershipSaysWhatTheSeatMayDo(t *testing.T) {
	b := build(t)
	if me := b.membership(t, b.yuki); me.Perms["agent_delegate"] != "confirm_required" || me.Perms["conversation_ask"] != "autonomous" ||
		me.Perms["grade_submit"] != "denied" || me.PrincipalMemberID != nil {
		t.Fatalf("Yuki's membership: %+v", me)
	}
	bot := b.agent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, bot, m{})
	// Whatever its row says, a delegate is capped by its principal, answers
	// no more than she asks, and manages the course no more than she does.
	b.Exec(`UPDATE course_member SET perm_member_manage = 'autonomous', perm_grade_post = 'autonomous' WHERE id = $1`, seat)
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": b.yukiM, "perms": m{"conversation_ask": "confirm_required"}})
	if me := b.membership(t, bot); me.Perms["member_manage"] != "denied" || me.Perms["grade_post"] != "denied" ||
		me.Perms["conversation_answer"] != "confirm_required" || me.Perms["document_read"] != "autonomous" {
		t.Fatalf("the delegate's membership: %+v", me.Perms)
	}
}

// A principal's removal and a change to its delegate's seat meet at the
// principal, before either holds the other's row: whatever locks the
// delegate's seat takes its principal's KEY SHARE first, and the removal
// holds the principal, then its delegates, and only then the conversations.
// Taken the other way round, a withdrawal of Yuki's agent and her removal
// deadlocked over her conversation with it.
func TestAPrincipalsRemovalAndItsDelegatesMeetAtThePrincipal(t *testing.T) {
	c := newCast(t)
	b := c.built
	conv, _ := b.open(t, b.yuki, c.yukiBot, "Hello, helper")
	notYet := func(done chan pipeline.Outcome, what string) {
		t.Helper()
		select {
		case out := <-done:
			t.Fatalf("%s did not wait for the principal: %+v", what, out)
		default:
		}
	}

	// Yuki's removal under way: her seat is held. Taking her agent out, or
	// removing its seat, waits for it before touching the agent's seat.
	for _, tc := range []struct {
		name  string
		actor uuid.UUID
		args  func() m
	}{
		{"agent.withdraw", b.yuki, func() m { return m{"actor_id": c.bot, "course_id": b.course} }},
		{"member.remove", b.sato, func() m { return m{"course_id": b.course, "member_id": c.yukiBot} }},
	} {
		release := b.hold(t, `SELECT 1 FROM course_member WHERE id = $1 FOR UPDATE`, b.yukiM)
		done := make(chan pipeline.Outcome, 1)
		b.start(t, done, tc.actor, tc.name, tc.args())
		b.blocked(t, 1, done)
		notYet(done, tc.name)
		release()
		if out := <-done; out.Status != domain.StatusExecuted {
			t.Fatalf("%s once Yuki's seat was let go: %+v", tc.name, out)
		}
		// Seated again, for what comes next.
		c.yukiBot = b.delegate(t, b.yuki, c.bot, m{})
		conv, _ = b.open(t, b.yuki, c.yukiBot, "Hello again")
	}

	// The other way round: the agent's seat is held by a change under way,
	// and Yuki's removal waits for it at her delegates, before it has locked
	// a single conversation.
	release := b.hold(t, `SELECT 1 FROM course_member WHERE id = $1 FOR UPDATE`, c.yukiBot)
	done := make(chan pipeline.Outcome, 1)
	b.start(t, done, b.sato, "member.remove", m{"course_id": b.course, "member_id": b.yukiM})
	b.blocked(t, 1, done)
	notYet(done, "Yuki's removal")
	tx, err := b.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM conversation WHERE id = $1 FOR UPDATE NOWAIT`, conv); err != nil {
		t.Fatalf("Yuki's removal holds her conversation while it waits for her agent's seat: %v", err)
	}
	_ = tx.Rollback(t.Context())
	release()
	if out := <-done; out.Status != domain.StatusExecuted {
		t.Fatalf("Yuki's removal: %+v", out)
	}
	if n := b.Count(`SELECT count(*) FROM conversation WHERE id = $1 AND status = 'closed'`, conv); n != 1 {
		t.Fatal("Yuki's conversation with her agent was not closed with her")
	}
}

// A delegate's write, and a decision about a delegate's proposal, take the
// principal's seat as well as the delegate's: pausing the principal under
// way is waited for, and what it did is seen.
func TestADelegatesWriteWaitsForItsPrincipal(t *testing.T) {
	b := build(t)
	bot := b.runtimeAgent(t, b.yuki, "Yuki's helper")
	seat := b.delegate(t, b.yuki, bot, m{})
	b.Host(bot)
	b.do(t, b.sato, "member.update_perms", m{"course_id": b.course, "member_id": seat, "perms": m{"submission_write": "confirm_required"}})
	conv, _ := b.open(t, b.yuki, seat, "Hello, helper")
	pause := `WITH held AS (SELECT id FROM course_member WHERE id = $1 FOR UPDATE)
		UPDATE course_member SET status = 'paused' WHERE id IN (SELECT id FROM held)`
	notYet := func(done chan pipeline.Outcome, what string) {
		t.Helper()
		select {
		case out := <-done:
			t.Fatalf("%s did not wait for the principal: %+v", what, out)
		default:
		}
	}

	done := make(chan pipeline.Outcome, 1)
	release := b.hold(t, pause, b.yukiM)
	b.start(t, done, bot, "conversation.close", m{"course_id": b.course, "conversation_id": conv})
	b.blocked(t, 1, done)
	notYet(done, "the delegate's write")
	release()
	if out := <-done; out.Status != domain.StatusDenied || reason(out) != "principal_not_active" {
		t.Fatalf("the delegate's write once its principal was paused: %+v", out)
	}
	b.Exec(`UPDATE course_member SET status = 'active' WHERE id = $1`, b.yukiM)

	waiting := b.MustCall(bot, "submission.create", m{"course_id": b.course, "assignment_id": b.hw3, "student_member_id": b.yukiM, "body": "draft"}, "bot-draft")
	if waiting.Status != domain.StatusProposed {
		t.Fatalf("the delegate's draft: %+v", waiting)
	}
	release = b.hold(t, pause, b.yukiM)
	b.start(t, done, b.sato, "action.decide", m{"course_id": b.course, "action_id": waiting.ActionID, "decision": "approve"})
	b.blocked(t, 1, done)
	notYet(done, "approving the delegate's proposal")
	release()
	out := <-done
	d := testkit.Result[pipeline.DecideOut](t, out)
	if d.Outcome != domain.StatusCancelled {
		t.Fatalf("approving the delegate's proposal once its principal was paused: %+v", d)
	}
}

// A delegate reaches only what its principal reaches as well, in every list,
// in SQL — assignments as much as students — however far its own row says
// it reaches: its own row can be wider than its principal's, as the
// previous release, which narrows a principal and not its delegates, leaves
// it.
func TestADelegateListsNothingItsPrincipalCannotReach(t *testing.T) {
	c := newCast(t)
	b := c.built
	publish := func(args m) tools.DocumentCreateOut {
		t.Helper()
		args["course_id"] = b.course
		made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", args))
		b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": made.DocumentID})
		return made
	}
	yukis, _ := b.open(t, b.yuki, c.courseTutor, "Yuki's question")
	kens, _ := b.open(t, b.ken, c.courseTutor, "Ken's question")
	// HW3 with instructions Yuki handed in under, since replaced; HW5 with
	// its own. Yuki's work on both, graded and posted.
	brief := publish(m{"kind": "instructions", "title": "HW3", "body_md": "Write 1000 words."})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": brief.DocumentID})
	hw3 := b.submit(t, b.yuki, "1000 words")
	revised := publish(m{"kind": "instructions", "title": "HW3 (revised)", "body_md": "Write 2000 words."})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": revised.DocumentID})
	hw5brief := publish(m{"kind": "instructions", "title": "HW5", "body_md": "Draw a graph."})
	hw5 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{"course_id": b.course, "title": "HW5",
		"points_possible": 10, "component_id": b.bucket, "instructions_document_id": hw5brief.DocumentID})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw5})
	onHW5 := testkit.Result[tools.SubmissionCreateOut](t, b.do(t, b.yuki, "submission.create", m{"course_id": b.course, "assignment_id": hw5, "body": "a graph"})).SubmissionID
	b.do(t, b.yuki, "submission.submit", m{"course_id": b.course, "submission_id": onHW5})
	for _, s := range []uuid.UUID{hw3, onHW5} {
		b.do(t, b.sato, "grade.submit", m{"course_id": b.course, "submission_id": s, "score": 8})
	}
	for _, a := range []uuid.UUID{b.hw3, hw5} {
		b.do(t, b.sato, "grade.post", m{"course_id": b.course, "assignment_id": a})
	}

	// Yuki is narrowed to HW3; her agent's own row reaches every assignment.
	b.Exec(`UPDATE course_member SET assignment_scope = 'listed' WHERE id = $1`, b.yukiM)
	b.Exec(`INSERT INTO member_assignment_scope (member_id, assignment_id) VALUES ($1, $2)`, b.yukiM, b.hw3)
	reached := map[string]bool{}
	for _, a := range testkit.Result[tools.AssignmentListOut](t, b.do(t, c.bot, "assignment.list", m{"course_id": b.course})).Assignments {
		reached["assignment "+a.ID.String()] = true
	}
	for _, s := range testkit.Result[tools.SubmissionListOut](t, b.do(t, c.bot, "submission.list", m{"course_id": b.course})).Submissions {
		reached["submission on "+s.AssignmentID.String()] = true
	}
	for _, g := range testkit.Result[tools.GradeListOut](t, b.do(t, c.bot, "grade.list", m{"course_id": b.course})).Grades {
		if g.AssignmentID != nil {
			reached["grade on "+g.AssignmentID.String()] = true
		}
	}
	for _, e := range feed(t, b, c.bot) {
		if e.AssignmentID != nil {
			reached["event on "+e.AssignmentID.String()] = true
		}
	}
	for _, d := range testkit.Result[tools.DocumentListOut](t, b.do(t, c.bot, "document.list", m{"course_id": b.course})).Documents {
		reached["document "+d.ID.String()] = true
	}
	for _, want := range []string{"assignment " + b.hw3.String(), "submission on " + b.hw3.String(), "grade on " + b.hw3.String(),
		"event on " + b.hw3.String(), "document " + revised.DocumentID.String()} {
		if !reached[want] {
			t.Fatalf("the delegate does not reach %s: %v", want, reached)
		}
	}
	for _, not := range []string{"assignment " + hw5.String(), "submission on " + hw5.String(), "grade on " + hw5.String(),
		"event on " + hw5.String(), "document " + hw5brief.DocumentID.String()} {
		if reached[not] {
			t.Fatalf("the delegate reaches %s, which its principal does not", not)
		}
	}

	// Yuki is narrowed to nobody, not even herself; her agent's own row
	// reaches the whole class. It reads no roster line and no version she
	// handed in under.
	b.Exec(`UPDATE course_member SET student_scope = 'all' WHERE id = $1`, c.yukiBot)
	if got := b.get(t, c.bot, m{"document_id": brief.DocumentID, "version_id": brief.VersionID}); got.Version.BodyMD == nil {
		t.Fatalf("the version Yuki handed in under, read by her agent: %+v", got)
	}
	b.Exec(`DELETE FROM member_student_scope WHERE member_id = $1`, b.yukiM)
	if got := testkit.Result[tools.SubmissionRosterOut](t, b.do(t, c.bot, "submission.roster", m{"course_id": b.course, "assignment_id": b.hw3})).Students; len(got) != 0 {
		t.Fatalf("the roster, to the delegate of someone who reaches nobody: %+v", got)
	}
	if out, err := b.Call(c.bot, "document.get", m{"course_id": b.course, "document_id": brief.DocumentID, "version_id": brief.VersionID}, ""); err == nil && out.Status == domain.StatusExecuted {
		t.Fatal("the delegate of someone who reaches nobody reads a version pinned in her work")
	}

	// An overseer's delegate lists the conversations of the students its
	// principal oversees, and no others.
	watcher := b.agent(t, b.sato, "Sato's watcher")
	watch := b.delegate(t, b.sato, watcher, m{"perms": m{"action_decide": "confirm_required"}})
	b.Exec(`UPDATE course_member SET student_scope = 'all' WHERE id = $1`, watch)
	b.Exec(`UPDATE course_member SET student_scope = 'listed' WHERE id = $1`, b.satoM)
	b.Exec(`INSERT INTO member_student_scope (member_id, student_member_id) VALUES ($1, $2)`, b.satoM, b.kenM)
	got := map[uuid.UUID]bool{}
	for _, v := range b.listConversations(t, watcher, m{"as": "overseer"}) {
		got[v.ID] = true
	}
	if !got[kens] || got[yukis] {
		t.Fatalf("what Sato's watcher oversees, Sato overseeing Ken alone: %v", got)
	}
}
