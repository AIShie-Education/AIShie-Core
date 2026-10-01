package tools_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// Every agent is hosted one way, chosen when it is registered and never
// changed (docs/schema.md §2.1, Agents' hosting). A runtime agent is run by
// the site's own agent runtime, a site service, which alone is issued its
// token, by the agent's id, one at a time; people in the site ask it while
// that token lives, and nothing is declared. An mcp agent is its owner's own
// tools', with tokens the owner issues, and nobody asks it in the site.

// with calls as agent with the token credential, as whatever runs it does.
func (b *built) with(t *testing.T, agent, credential uuid.UUID, name string, args m) pipeline.Outcome {
	t.Helper()
	out, err := b.CallWith(pipeline.Caller{ActorID: agent, CredentialID: credential}, name, args, "as-"+uuid.NewString())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// asRuntime calls as the site's agent runtime, with its credential.
func (b *built) asRuntime(t *testing.T, name string, args m) pipeline.Outcome {
	t.Helper()
	out, err := b.CallWith(b.AsRuntime(), name, args, "rt-"+uuid.NewString())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// siteChat is whether owner's agent may be asked in the site, as agent.get
// and agent.list both say it; they must agree, and on its hosting too.
func (b *built) siteChat(t *testing.T, owner, agent uuid.UUID) bool {
	t.Helper()
	got := testkit.Result[tools.AgentGetOut](t, b.do(t, owner, "agent.get", m{"actor_id": agent}))
	for _, a := range testkit.Result[tools.AgentListOut](t, b.do(t, owner, "agent.list", m{})).Agents {
		if a.ActorID == agent && (a.SiteChat != got.SiteChat || a.Hosting != got.Hosting) {
			t.Fatalf("agent.get says site_chat %v, hosting %s; agent.list %v, %s", got.SiteChat, got.Hosting, a.SiteChat, a.Hosting)
		}
	}
	return got.SiteChat
}

// seatSiteChat is what member.get and member.list say of a seat's site
// chat and hosting, which must agree: nil for a person's seat.
func (b *built) seatSiteChat(t *testing.T, seat uuid.UUID) (siteChat *bool, hosting *string) {
	t.Helper()
	got := b.memberView(t, seat)
	say := func(v tools.MemberView) string {
		chat, host := "nothing", "nothing"
		if v.SiteChat != nil {
			chat = fmt.Sprint(*v.SiteChat)
		}
		if v.Hosting != nil {
			host = *v.Hosting
		}
		return chat + " " + host
	}
	for _, v := range testkit.Result[tools.MemberListOut](t, b.do(t, b.sato, "member.list", m{"course_id": b.course, "limit": 200})).Members {
		if v.ID == seat && say(v) != say(got) {
			t.Fatalf("member.get says %s, member.list %s", say(got), say(v))
		}
	}
	return got.SiteChat, got.Hosting
}

// refusedAs insists a call failed, with a code and a reason.
func refusedAs(t *testing.T, what string, out pipeline.Outcome, code apperr.Code, why string) {
	t.Helper()
	if (out.Status != domain.StatusFailed && out.Status != domain.StatusDenied) || out.Error == nil || out.Error.Code != code ||
		reason(out) != why {
		t.Fatalf("%s: %+v, want %s %s", what, out, code, why)
	}
}

// liveRuntimeTokens is how many tokens issued to the runtime of agent's are
// live.
func (b *built) liveRuntimeTokens(agent uuid.UUID) int {
	return b.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND issued_to_service = 'agent_runtime'
		AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`, agent)
}

func TestAnAgentIsHostedOneWayForGood(t *testing.T) {
	b := build(t)

	// The person chooses, and nothing chooses for them.
	if out, err := b.Call(b.yuki, "agent.create", m{"display_name": "Helper"}, "no-hosting"); err == nil || !apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("an agent registered without its hosting: %+v %v", out, err)
	}
	if out, err := b.Call(b.yuki, "agent.create", m{"display_name": "Helper", "hosting": "self_hosted"}, "self"); err == nil ||
		!apperr.Is(err, apperr.InvalidArgument) {
		t.Fatalf("an agent hosted by the person's own runtime: %+v %v", out, err)
	}
	tutor, script := b.runtimeAgent(t, b.yuki, "Tutor"), b.agent(t, b.yuki, "Script")
	for agent, want := range map[uuid.UUID]string{tutor: "runtime", script: "mcp"} {
		if got := testkit.Result[tools.AgentGetOut](t, b.do(t, b.yuki, "agent.get", m{"actor_id": agent})).Hosting; got != want {
			t.Fatalf("agent.get says hosting %s, want %s", got, want)
		}
		b.siteChat(t, b.yuki, agent)
		var payload string
		if err := b.Pool.QueryRow(t.Context(), `SELECT payload->>'hosting' FROM event WHERE type = 'agent.created' AND subject_id = $1`,
			agent).Scan(&payload); err != nil || payload != want {
			t.Fatalf("its news says hosting %q (%v), want %s", payload, err, want)
		}
	}

	// It never changes: refused, and held by the database too.
	refusedAs(t, "making a runtime agent an mcp agent", b.MustCall(b.yuki, "agent.update", m{"actor_id": tutor, "hosting": "mcp"}, "to-mcp"),
		apperr.FailedPrecondition, "hosting_fixed")
	refusedAs(t, "making an mcp agent a runtime agent", b.MustCall(b.yuki, "agent.update", m{"actor_id": script, "hosting": "runtime"},
		"to-runtime"), apperr.FailedPrecondition, "hosting_fixed")
	b.do(t, b.yuki, "agent.update", m{"actor_id": tutor, "hosting": "runtime", "display_name": "Essay tutor"})
	if _, err := b.Pool.Exec(t.Context(), `UPDATE actor SET hosting = 'mcp' WHERE id = $1`, tutor); err == nil ||
		!strings.Contains(err.Error(), "hosting_fixed") {
		t.Fatalf("the database changed an agent's hosting: %v", err)
	}
	if got := testkit.Result[tools.AgentGetOut](t, b.do(t, b.yuki, "agent.get", m{"actor_id": tutor})); got.Hosting != "runtime" ||
		got.DisplayName != "Essay tutor" {
		t.Fatalf("after all that: %+v", got)
	}
	// Nor does an owner say whether it is asked in the site: that follows.
	for _, on := range []bool{false, true} {
		refusedAs(t, fmt.Sprintf("site_chat %v", on), b.MustCall(b.yuki, "agent.update", m{"actor_id": tutor, "site_chat": on},
			fmt.Sprint("site-chat-", on)), apperr.InvalidArgument, "site_chat_follows_hosting")
	}

	// An administrator registers an agent saying how it is hosted, and a
	// person saying nothing of it.
	for what, args := range map[string]m{
		"an agent registered without its hosting": {"kind": "agent", "display_name": "Bot"},
		"a person registered with a hosting":      {"kind": "human", "display_name": "Mori", "hosting": "mcp"},
	} {
		// Refused as the arguments are read: nothing is recorded.
		out, err := b.Call(b.admin, "actor.register", args, what)
		if e, ok := apperr.As(err); !ok || e.Code != apperr.InvalidArgument || e.Details["field"] != "hosting" {
			t.Fatalf("%s: %+v %v", what, out, err)
		}
	}
	bot := testkit.Result[tools.ActorOut](t, b.do(t, b.admin, "actor.register",
		m{"kind": "agent", "display_name": "Enrolment bot", "hosting": "mcp"})).ActorID
	for actor, want := range map[uuid.UUID]string{bot: "mcp", b.tutor: "runtime", tutor: "runtime"} {
		got := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": actor})).Hosting
		if got == nil || *got != want {
			t.Fatalf("actor.get says hosting %v, want %s", got, want)
		}
		me := testkit.Result[tools.MeOut](t, b.do(t, actor, "me.get", m{}))
		if me.Hosting == nil || *me.Hosting != want {
			t.Fatalf("me.get, as the agent, says hosting %v, want %s", me.Hosting, want)
		}
	}
	if got := testkit.Result[tools.ActorView](t, b.do(t, b.admin, "actor.get", m{"actor_id": b.yuki})).Hosting; got != nil {
		t.Fatalf("a person's hosting: %s", *got)
	}
	if got := testkit.Result[tools.MeOut](t, b.do(t, b.yuki, "me.get", m{})).Hosting; got != nil {
		t.Fatalf("a person's own hosting: %s", *got)
	}
	var payload string
	if err := b.Pool.QueryRow(t.Context(), `SELECT payload->>'hosting' FROM event WHERE type = 'actor.registered' AND subject_id = $1`,
		bot).Scan(&payload); err != nil || payload != "mcp" {
		t.Fatalf("its registration's news says hosting %q (%v)", payload, err)
	}
}

func TestARuntimeAgentsOwnerHoldsNoTokenForIt(t *testing.T) {
	b := build(t)
	tutor, script := b.runtimeAgent(t, b.sato, "Tutor"), b.agent(t, b.sato, "Script")

	refusedAs(t, "its owner issuing it a token", b.MustCall(b.sato, "agent.issue_token", m{"actor_id": tutor, "label": "laptop"}, "own"),
		apperr.Forbidden, auth.ReasonHostedByRuntime)
	refusedAs(t, "an administrator issuing it one", b.MustCall(b.admin, "actor.issue_token", m{"actor_id": tutor, "label": "ops"}, "admin"),
		apperr.Forbidden, auth.ReasonHostedByRuntime)
	if _, _, err := auth.IssueToken(t.Context(), b.Q, tutor, nil, "cli", nil, time.Now()); !errors.Is(err, auth.ErrHostedByRuntime) {
		t.Fatal("the command line's way issued it one")
	}
	// The runtime's own token issues it no other.
	runtime := b.Host(tutor)
	refusedAs(t, "the agent issuing itself another", b.with(t, tutor, runtime, "credential.issue_token", m{"label": "spare"}),
		apperr.Forbidden, auth.ReasonHostedByRuntime)
	if n := b.Count(`SELECT count(*) FROM credential WHERE actor_id = $1`, tutor); n != 1 {
		t.Fatalf("the runtime agent holds %d tokens", n)
	}

	// The database holds it too, whoever writes.
	if _, err := b.Pool.Exec(t.Context(), `INSERT INTO credential (actor_id, kind, secret_hash, token_prefix) VALUES ($1, 'api_token', 'h', 'sneaky')`,
		tutor); err == nil || !strings.Contains(err.Error(), "hosted_by_runtime") {
		t.Fatalf("the database took an owner's token for a runtime agent: %v", err)
	}

	// An mcp agent's owner issues as many as they like, as ever.
	for _, label := range []string{"laptop", "editor"} {
		tok := testkit.Result[tools.IssueTokenOut](t, b.do(t, b.sato, "agent.issue_token", m{"actor_id": script, "label": label}))
		if !strings.HasPrefix(tok.Token, "ais_") {
			t.Fatalf("the mcp agent's token: %+v", tok)
		}
	}
	b.do(t, b.admin, "actor.issue_token", m{"actor_id": script, "label": "ops"})
}

func TestTheSiteRuntimeIsIssuedOneTokenForEachRuntimeAgent(t *testing.T) {
	b := build(t)
	tutor, script := b.runtimeAgent(t, b.sato, "Tutor"), b.agent(t, b.sato, "Script")
	service, _, _ := b.RuntimeService()

	first := testkit.Result[tools.RuntimeIssueTokenOut](t, b.asRuntime(t, "agent_runtime.issue_token", m{"agent_id": tutor}))
	if first.AgentID != tutor || !strings.HasPrefix(first.Token, "ais_") || len(first.Replaced) != 0 {
		t.Fatalf("the first token: %+v", first)
	}
	p, err := auth.NewAuthenticator(b.Pool, 0).Authenticate(t.Context(), first.Token)
	if err != nil || p.ActorID != tutor || p.CredentialID != first.CredentialID || p.ExpiresAt != nil {
		t.Fatalf("the token authenticates %+v (%v), not the agent, for good", p, err)
	}
	// It acts as the agent, which is told how it is hosted.
	if me := testkit.Result[tools.MeOut](t, b.with(t, tutor, first.CredentialID, "me.get", m{})); me.Hosting == nil || *me.Hosting != "runtime" {
		t.Fatalf("me.get with the runtime's token: %+v", me)
	}
	// Its owner sees it listed, issued by the runtime and to it.
	creds := testkit.Result[tools.CredentialListOut](t, b.do(t, b.sato, "agent.list_credentials", m{"actor_id": tutor})).Credentials
	if len(creds) != 1 || creds[0].ID != first.CredentialID || creds[0].IssuedTo == nil || *creds[0].IssuedTo != "agent_runtime" ||
		creds[0].IssuedByID == nil || *creds[0].IssuedByID != service || creds[0].ExpiresAt != nil {
		t.Fatalf("the tutor's credentials: %+v", creds)
	}

	// Another revokes the first: one token at a time.
	second := testkit.Result[tools.RuntimeIssueTokenOut](t, b.asRuntime(t, "agent_runtime.issue_token",
		m{"agent_id": tutor, "label": "runtime, replica 2"}))
	if len(second.Replaced) != 1 || second.Replaced[0] != first.CredentialID || b.liveRuntimeTokens(tutor) != 1 {
		t.Fatalf("the second token: %+v; %d live", second, b.liveRuntimeTokens(tutor))
	}
	if _, err := auth.NewAuthenticator(b.Pool, 0).Authenticate(t.Context(), first.Token); err == nil {
		t.Fatal("the first token still works")
	}
	if n := b.Count(`SELECT count(*) FROM credential WHERE id = $1 AND label = 'runtime, replica 2'`, second.CredentialID); n != 1 {
		t.Fatal("the label was not kept")
	}
	// Kept for the release before, which reads who is asked there.
	if n := b.Count(`SELECT count(*) FROM actor WHERE id = $1 AND site_chat_credential_id = $2`, tutor, second.CredentialID); n != 1 {
		t.Fatal("the site chat credential does not name the runtime's token")
	}

	// Each is an action of the service's, and the token is in none of what
	// is kept; a replay comes back without it.
	key := "host-" + uuid.NewString()
	issued, err := b.CallWith(b.AsRuntime(), "agent_runtime.issue_token", m{"agent_id": tutor}, key)
	if err != nil || issued.Status != domain.StatusExecuted {
		t.Fatalf("a third: %+v %v", issued, err)
	}
	third := testkit.Result[tools.RuntimeIssueTokenOut](t, issued)
	var actor uuid.UUID
	var payload, result string
	if err := b.Pool.QueryRow(t.Context(), `SELECT actor_id, payload::text, coalesce(result::text, '') FROM action WHERE id = $1`,
		issued.ActionID).Scan(&actor, &payload, &result); err != nil || actor != service ||
		strings.Contains(payload+result, third.Token) || strings.Contains(result, `"token"`) {
		t.Fatalf("the action: %s %s %s %v", actor, payload, result, err)
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE payload::text LIKE '%' || $1 || '%'`, third.Token); n != 0 {
		t.Fatal("the token is in the news")
	}
	replay, err := b.CallWith(b.AsRuntime(), "agent_runtime.issue_token", m{"agent_id": tutor}, key)
	if err != nil || !replay.Replayed || testkit.Result[tools.RuntimeIssueTokenOut](t, replay).Token != "" || b.liveRuntimeTokens(tutor) != 1 {
		t.Fatalf("the replay: %+v %v", replay, err)
	}
	if news := b.Count(`SELECT count(*) FROM event WHERE type = 'agent_runtime.token_issued' AND subject_id = $1`, tutor); news != 3 {
		t.Fatalf("%d issues in the news", news)
	}

	// Refused for an mcp agent, a suspended one, one whose owner is
	// suspended, and anyone who is no agent.
	refusedAs(t, "a token for an mcp agent", b.asRuntime(t, "agent_runtime.issue_token", m{"agent_id": script}),
		apperr.FailedPrecondition, "not_runtime_hosted")
	b.do(t, b.sato, "agent.suspend", m{"actor_id": tutor})
	refusedAs(t, "a token for a suspended agent", b.asRuntime(t, "agent_runtime.issue_token", m{"agent_id": tutor}),
		apperr.FailedPrecondition, "agent_suspended")
	b.do(t, b.sato, "agent.reactivate", m{"actor_id": tutor})
	b.do(t, b.admin, "actor.suspend", m{"actor_id": b.sato})
	refusedAs(t, "a token for a suspended person's agent", b.asRuntime(t, "agent_runtime.issue_token", m{"agent_id": tutor}),
		apperr.FailedPrecondition, "owner_suspended")
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": b.sato})
	for who, id := range map[string]uuid.UUID{"a person": b.sato, "nobody": uuid.New(), "the service itself": service} {
		if out, err := b.CallWith(b.AsRuntime(), "agent_runtime.issue_token", m{"agent_id": id}, "who-"+who); err == nil ||
			!apperr.Is(err, apperr.NotFound) {
			t.Fatalf("a token for %s: %+v %v", who, out, err)
		}
	}
	// Of all those, the tutor's alone, and build's tutor's, which the runtime hosts.
	if n := b.Count(`SELECT count(*) FROM credential WHERE issued_to_service IS NOT NULL AND actor_id NOT IN ($1, $2)`, tutor, b.tutor); n != 0 {
		t.Fatalf("%d runtime tokens for others", n)
	}

	// The runtime stops hosting it: revoked, and revoking none is no error.
	revoked := testkit.Result[tools.RuntimeRevokeTokenOut](t, b.asRuntime(t, "agent_runtime.revoke_token", m{"agent_id": tutor}))
	if len(revoked.Revoked) != 1 || revoked.Revoked[0] != third.CredentialID || b.liveRuntimeTokens(tutor) != 0 {
		t.Fatalf("revoked %+v; %d live", revoked, b.liveRuntimeTokens(tutor))
	}
	if n := b.Count(`SELECT count(*) FROM actor WHERE id = $1 AND site_chat_credential_id IS NULL`, tutor); n != 1 {
		t.Fatal("the site chat credential still names a revoked token")
	}
	for _, agent := range []uuid.UUID{tutor, script} {
		if again := testkit.Result[tools.RuntimeRevokeTokenOut](t, b.asRuntime(t, "agent_runtime.revoke_token", m{"agent_id": agent})); len(again.Revoked) != 0 {
			t.Fatalf("revoking none: %+v", again)
		}
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = 'agent_runtime.token_revoked' AND subject_id = $1`, tutor); n != 1 {
		t.Fatalf("%d revocations in the news", n)
	}
}

func TestTheRuntimeReadsAgentsAndTheirOwners(t *testing.T) {
	b := build(t)
	tutor, script := b.runtimeAgent(t, b.sato, "Tutor"), b.agent(t, b.sato, "Script")
	b.delegate(t, b.sato, tutor, m{"preset": "course_tutor"})

	view := testkit.Result[tools.RuntimeAgentView](t, b.asRuntime(t, "agent_runtime.agent", m{"agent_id": tutor}))
	if view.AgentID != tutor || view.DisplayName != "Tutor" || view.Hosting != "runtime" || view.Status != "active" ||
		view.OwnerActorID == nil || *view.OwnerActorID != b.sato || view.OwnerStatus == nil || *view.OwnerStatus != "active" ||
		view.LiveSeats != 1 || !view.Hostable || view.Reason != nil || view.RuntimeToken != nil || view.SiteChat {
		t.Fatalf("the tutor, not hosted: %+v", view)
	}
	credential := b.Host(tutor)
	view = testkit.Result[tools.RuntimeAgentView](t, b.asRuntime(t, "agent_runtime.agent", m{"agent_id": tutor}))
	if view.RuntimeToken == nil || view.RuntimeToken.CredentialID != credential || !view.SiteChat {
		t.Fatalf("the tutor, hosted: %+v", view)
	}
	view = testkit.Result[tools.RuntimeAgentView](t, b.asRuntime(t, "agent_runtime.agent", m{"agent_id": script}))
	if view.Hosting != "mcp" || view.Hostable || view.Reason == nil || *view.Reason != "not_runtime_hosted" || view.SiteChat {
		t.Fatalf("the script: %+v", view)
	}
	view = testkit.Result[tools.RuntimeAgentView](t, b.asRuntime(t, "agent_runtime.agent", m{"agent_id": b.tutor}))
	if view.OwnerActorID != nil || view.OwnerStatus != nil || !view.Hostable || !view.SiteChat {
		t.Fatalf("the tutor nobody owns: %+v", view)
	}
	if out, err := b.CallWith(b.AsRuntime(), "agent_runtime.agent", m{"agent_id": b.yuki}, ""); err == nil || !apperr.Is(err, apperr.NotFound) {
		t.Fatalf("a person, read as an agent: %+v %v", out, err)
	}

	check := func(actor, agent uuid.UUID) tools.RuntimeCheckOwnerOut {
		t.Helper()
		return testkit.Result[tools.RuntimeCheckOwnerOut](t, b.asRuntime(t, "agent_runtime.check_owner", m{"actor_id": actor, "agent_id": agent}))
	}
	if got := check(b.sato, tutor); !got.Owns || got.Agent == nil || got.Agent.AgentID != tutor || !got.Agent.Hostable {
		t.Fatalf("Sato's tutor: %+v", got)
	}
	if got := check(b.sato, script); !got.Owns || got.Agent == nil || got.Agent.Hostable || *got.Agent.Reason != "not_runtime_hosted" {
		t.Fatalf("Sato's script: %+v", got)
	}
	b.do(t, b.sato, "agent.suspend", m{"actor_id": tutor})
	if got := check(b.sato, tutor); !got.Owns || got.Agent.Hostable || *got.Agent.Reason != "agent_suspended" || got.Agent.SiteChat {
		t.Fatalf("Sato's tutor, suspended: %+v", got)
	}
	for who, args := range map[string][2]uuid.UUID{
		"someone else's":       {b.yuki, tutor},
		"nobody's":             {b.sato, b.tutor},
		"no agent's":           {b.sato, b.yuki},
		"an id nobody has":     {b.sato, uuid.New()},
		"by someone who isn't": {uuid.New(), tutor},
	} {
		if got := check(args[0], args[1]); got.Owns || got.Agent != nil {
			t.Fatalf("%s: %+v", who, got)
		}
	}
}

// The runtime's tools are its alone, and it calls nothing else: not a
// person's, an agent's, an administrator's or the transcriber's; a
// credential of its revoked calls nothing.
func TestOnlyTheSiteRuntimeCallsItsTools(t *testing.T) {
	b := build(t)
	tutor := b.runtimeAgent(t, b.sato, "Tutor")
	runtime := b.Host(tutor)
	transcriber := testkit.Result[tools.ServiceIssueCredentialOut](t, b.do(t, b.admin, "service.issue_credential",
		m{"scope": "document_text", "label": "transcriber"}))

	calls := map[string]m{
		"agent_runtime.agent":        {"agent_id": tutor},
		"agent_runtime.check_owner":  {"actor_id": b.sato, "agent_id": tutor},
		"agent_runtime.issue_token":  {"agent_id": tutor},
		"agent_runtime.revoke_token": {"agent_id": tutor},
	}
	for name, args := range calls {
		for who, caller := range map[string]pipeline.Caller{
			"its owner":       {ActorID: b.sato},
			"root":            {ActorID: b.Root},
			"an admin":        {ActorID: b.admin},
			"the agent":       {ActorID: tutor, CredentialID: runtime},
			"the transcriber": {ActorID: transcriber.ServiceActorID, CredentialID: transcriber.CredentialID},
		} {
			out, err := b.CallWith(caller, name, args, "nope-"+uuid.NewString())
			if err != nil || out.Status != domain.StatusDenied {
				t.Fatalf("%s calling %s: %+v %v", who, name, out, err)
			}
			want := "service_only"
			if who == "the transcriber" {
				want = "not_for_services"
			}
			if reason(out) != want {
				t.Fatalf("%s calling %s: %+v, want %s", who, name, out.Error, want)
			}
		}
	}
	for name, args := range map[string]m{
		"me.get":                   {},
		"agent.list":               {},
		"document_text.queue":      {},
		"service.list_credentials": {"scope": "agent_runtime"},
		"conversation.respondents": {"course_id": b.course},
	} {
		out, err := b.CallWith(b.AsRuntime(), name, args, "")
		if err != nil || out.Status != domain.StatusDenied || reason(out) != "not_for_services" {
			t.Fatalf("the runtime calling %s: %+v %v", name, out, err)
		}
	}

	// Its credential is the platform's to list and revoke, as any service's.
	list := testkit.Result[tools.ServiceListCredentialsOut](t, b.do(t, b.admin, "service.list_credentials", m{"scope": "agent_runtime"}))
	if len(list.Credentials) != 1 || !list.Credentials[0].Live {
		t.Fatalf("the runtime's credentials: %+v", list)
	}
	b.do(t, b.admin, "service.revoke_credential", m{"scope": "agent_runtime", "credential_id": list.Credentials[0].ID})
	out, err := b.CallWith(b.AsRuntime(), "agent_runtime.issue_token", m{"agent_id": tutor}, "revoked")
	if err != nil || out.Status != domain.StatusDenied || reason(out) != "service_only" {
		t.Fatalf("with its credential revoked: %+v %v", out, err)
	}
	// The agents' tokens are theirs, and stay.
	if b.liveRuntimeTokens(tutor) != 1 {
		t.Fatal("revoking the runtime's credential revoked an agent's token")
	}
}

func TestPeopleAskARuntimeAgentWhileTheRuntimeHostsIt(t *testing.T) {
	b := build(t)
	tutor := b.runtimeAgent(t, b.sato, "Course tutor")
	seat := b.delegate(t, b.sato, tutor, m{"preset": "course_tutor"})
	opening := m{"course_id": b.course, "respondent_member_id": seat, "body": "What does HW3 ask for?"}
	asked := func(want bool, when string) {
		t.Helper()
		if got := b.siteChat(t, b.sato, tutor); got != want {
			t.Fatalf("%s: agent.get says site_chat %v, want %v", when, got, want)
		}
		if chat, hosting := b.seatSiteChat(t, seat); chat == nil || *chat != want || hosting == nil || *hosting != "runtime" {
			t.Fatalf("%s: its seat says site_chat %v, hosting %v", when, chat, hosting)
		}
		r, ok := b.respondents(t, b.yuki)[seat]
		if ok != want || (ok && r.Hosting != "runtime") {
			t.Fatalf("%s: offered to Yuki %v (%+v), want %v", when, ok, r, want)
		}
	}

	// Not hosted yet: not offered, and a question waits for nothing.
	asked(false, "not hosted")
	refusedAs(t, "opening a conversation with it", b.MustCall(b.yuki, "conversation.open", opening, "before"),
		apperr.FailedPrecondition, "agent_not_hosted")
	if chat, hosting := b.seatSiteChat(t, b.yukiM); chat != nil || hosting != nil {
		t.Fatalf("a person's seat says site_chat %v, hosting %v", chat, hosting)
	}

	// The runtime hosts it: offered, asked, and it answers. Nothing is
	// declared.
	runtime := b.Host(tutor)
	asked(true, "hosted")
	conv, first := b.open(t, b.yuki, seat, "What does HW3 ask for?")
	if in := testkit.Result[tools.ConversationInboxOut](t, b.with(t, tutor, runtime, "conversation.inbox", m{"course_id": b.course})).Conversations; len(in) != 1 || in[0].ID != conv {
		t.Fatalf("the tutor's inbox: %+v", in)
	}
	b.with(t, tutor, runtime, "conversation.answer", answerArgs(b, conv, first, "An essay with a thesis."))
	second := b.ask(t, b.yuki, conv, "How long should it be?")

	// The runtime stops hosting it.
	b.asRuntime(t, "agent_runtime.revoke_token", m{"agent_id": tutor})
	asked(false, "its runtime's token revoked")
	refusedAs(t, "asking it more", b.MustCall(b.yuki, "conversation.ask", m{"course_id": b.course, "conversation_id": conv, "body": "Are you there?"},
		"after-ask"), apperr.FailedPrecondition, "agent_not_hosted")
	refusedAs(t, "opening another", b.MustCall(b.yuki, "conversation.open", opening, "after-open"), apperr.FailedPrecondition, "agent_not_hosted")
	// What was written stays: read by both, retracted, closed.
	if msgs := b.messages(t, b.yuki, conv); len(msgs) != 3 || msgs[2].ID != second {
		t.Fatalf("the conversation, as Yuki reads it: %+v", msgs)
	}
	if got := b.conversation(t, tutor, conv); got.State != tools.StateAwaitingAnswer {
		t.Fatalf("the conversation, as the tutor reads it: %+v", got)
	}

	// Hosted again; its owner revokes the runtime's token, as they may any
	// of their agent's.
	runtime = b.Host(tutor)
	asked(true, "hosted again")
	b.do(t, b.sato, "agent.revoke_credential", m{"actor_id": tutor, "credential_id": runtime})
	asked(false, "its owner revoked the runtime's token")

	// It holds only while the agent and its owner are active.
	b.Host(tutor)
	asked(true, "hosted once more")
	b.do(t, b.sato, "agent.suspend", m{"actor_id": tutor})
	asked(false, "the agent suspended")
	refusedAs(t, "asking it, suspended", b.MustCall(b.yuki, "conversation.ask", m{"course_id": b.course, "conversation_id": conv, "body": "Hello?"},
		"suspended"), apperr.Forbidden, "not_addressable")
	b.do(t, b.sato, "agent.reactivate", m{"actor_id": tutor})
	asked(true, "the agent reactivated, its runtime's token still working")
	b.ask(t, b.yuki, conv, "And the word count?")
	b.do(t, b.yuki, "conversation.retract", m{"course_id": b.course, "message_id": second})
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})

	// Yuki's own runtime agent, while she is suspended, as the instructor
	// sees its seat.
	helper := b.runtimeAgent(t, b.yuki, "Yuki's helper")
	helperM := b.delegate(t, b.yuki, helper, m{})
	b.Host(helper)
	if chat, _ := b.seatSiteChat(t, helperM); chat == nil || !*chat {
		t.Fatalf("Yuki's helper, hosted: %v", chat)
	}
	b.do(t, b.admin, "actor.suspend", m{"actor_id": b.yuki})
	if chat, _ := b.seatSiteChat(t, helperM); chat == nil || *chat {
		t.Fatalf("its owner suspended, the agent's seat says site_chat %v", chat)
	}
	b.do(t, b.admin, "actor.reactivate", m{"actor_id": b.yuki})
	if !b.siteChat(t, b.yuki, helper) {
		t.Fatal("its owner reactivated, the agent is not asked in the site")
	}
}

// An mcp agent is its owner's tools', and nobody asks it in the site: not
// offered, not opened, not asked in a conversation it was given before. It
// does everything else an agent does in its seats, with its owner's token.
func TestAnMCPAgentIsNeverAskedInTheSite(t *testing.T) {
	b := build(t)
	script := b.agent(t, b.sato, "Sato's assistant")
	seat := b.delegate(t, b.sato, script, m{"preset": "course_tutor"})
	tok := testkit.Result[tools.IssueTokenOut](t, b.do(t, b.sato, "agent.issue_token", m{"actor_id": script, "label": "editor"}))
	b.Exec(`UPDATE credential SET last_used_at = now() WHERE id = $1`, tok.CredentialID)

	if b.siteChat(t, b.sato, script) {
		t.Fatal("an mcp agent is asked in the site")
	}
	if chat, hosting := b.seatSiteChat(t, seat); chat == nil || *chat || hosting == nil || *hosting != "mcp" {
		t.Fatalf("its seat says site_chat %v, hosting %v", chat, hosting)
	}
	for _, who := range []uuid.UUID{b.yuki, b.sato} {
		if _, ok := b.respondents(t, who)[seat]; ok {
			t.Fatal("an mcp agent is offered")
		}
	}
	refusedAs(t, "opening a conversation with it", b.MustCall(b.yuki, "conversation.open",
		m{"course_id": b.course, "respondent_member_id": seat, "body": "Hello"}, "open"), apperr.FailedPrecondition, "mcp_agent")
	refusedAs(t, "its owner opening one", b.MustCall(b.sato, "conversation.open",
		m{"course_id": b.course, "respondent_member_id": seat, "body": "Hello"}, "open-own"), apperr.FailedPrecondition, "mcp_agent")
	// A conversation with it from before, asked nothing more.
	conv := uuid.Must(uuid.NewV7())
	b.Exec(`INSERT INTO conversation (id, course_id, opener_member_id, respondent_member_id) VALUES ($1, $2, $3, $4)`,
		conv, b.course, b.yukiM, seat)
	refusedAs(t, "asking it", b.MustCall(b.yuki, "conversation.ask", m{"course_id": b.course, "conversation_id": conv, "body": "Hello?"}, "ask"),
		apperr.FailedPrecondition, "mcp_agent")
	b.do(t, b.yuki, "conversation.close", m{"course_id": b.course, "conversation_id": conv})

	// Everything else, as ever, with its owner's token.
	if me := testkit.Result[tools.MeOut](t, b.with(t, script, tok.CredentialID, "me.get", m{})); me.Hosting == nil || *me.Hosting != "mcp" {
		t.Fatalf("me.get, as the mcp agent: %+v", me)
	}
	if out := b.with(t, script, tok.CredentialID, "document.list", m{"course_id": b.course}); out.Status != domain.StatusExecuted {
		t.Fatalf("reading the course, as the mcp agent: %+v", out)
	}
	refusedAs(t, "it saying it answers in the site", b.with(t, script, tok.CredentialID, "me.site_chat", m{"on": true}),
		apperr.FailedPrecondition, "not_runtime_hosted")
	// And the runtime does not host it.
	refusedAs(t, "the runtime hosting it", b.asRuntime(t, "agent_runtime.issue_token", m{"agent_id": script}),
		apperr.FailedPrecondition, "not_runtime_hosted")
}

// me.site_chat is kept for one release: the runtime's token may call it,
// which changes nothing and says whether the agent is asked now; anything
// else is refused.
func TestMeSiteChatIsKeptForOneReleaseAndChangesNothing(t *testing.T) {
	b := build(t)
	tutor := b.runtimeAgent(t, b.sato, "Course tutor")
	b.delegate(t, b.sato, tutor, m{"preset": "course_tutor"})
	runtime := b.Host(tutor)
	events := b.Count(`SELECT count(*) FROM event`)

	for _, on := range []bool{true, false, true} {
		out := b.with(t, tutor, runtime, "me.site_chat", m{"on": on})
		if out.Status != domain.StatusExecuted || !testkit.Result[tools.SiteChatOut](t, out).SiteChat {
			t.Fatalf("on %v, with the runtime's token: %+v", on, out)
		}
		if !b.siteChat(t, b.sato, tutor) {
			t.Fatalf("on %v changed whether it is asked", on)
		}
	}
	if n := b.Count(`SELECT count(*) FROM actor WHERE id = $1 AND site_chat_credential_id = $2`, tutor, runtime); n != 1 {
		t.Fatal("the site chat credential moved")
	}
	if n := b.Count(`SELECT count(*) FROM event`); n != events {
		t.Fatalf("%d events for changing nothing", n-events)
	}
	b.do(t, b.sato, "agent.suspend", m{"actor_id": tutor})
	if out := b.with(t, tutor, runtime, "me.site_chat", m{"on": true}); out.Status != domain.StatusDenied {
		t.Fatalf("a suspended agent's call: %+v", out)
	}
	b.do(t, b.sato, "agent.reactivate", m{"actor_id": tutor})

	refusedAs(t, "with no credential", b.MustCall(tutor, "me.site_chat", m{"on": true}, "none"), apperr.FailedPrecondition, "not_runtime_hosted")
	old := runtime
	b.Host(tutor)
	refusedAs(t, "with the runtime's token before", b.with(t, tutor, old, "me.site_chat", m{"on": true}), apperr.FailedPrecondition,
		"not_runtime_hosted")
	refusedAs(t, "a person", b.with(t, b.yuki, b.session(t, b.yuki), "me.site_chat", m{"on": true}), apperr.FailedPrecondition, "not_an_agent")
}

// The catalogue says the two modes where an owner, an agent or the runtime
// reads it: agent.create asks for one, and nothing else chooses it.
func TestTheCatalogueSaysHowAgentsAreHosted(t *testing.T) {
	b := build(t)
	create, ok := b.P.Registry().Get("agent.create")
	if !ok {
		t.Fatal("no agent.create")
	}
	raw, err := json.Marshal(create.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Enum    []string `json:"enum"`
			Default any      `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	h := schema.Properties["hosting"]
	if !strings.Contains(strings.Join(schema.Required, " "), "hosting") || strings.Join(h.Enum, " ") != "runtime mcp" || h.Default != nil {
		t.Fatalf("agent.create's hosting: required %v, %+v", schema.Required, h)
	}
	for _, name := range []string{"agent.create", "agent.issue_token", "conversation.respondents", "me.site_chat", "agent_runtime.issue_token"} {
		tl, ok := b.P.Registry().Get(name)
		if !ok || !strings.Contains(tl.Description, "runtime") {
			t.Fatalf("%s says nothing of how agents are hosted: %q", name, tl.Description)
		}
	}
}
