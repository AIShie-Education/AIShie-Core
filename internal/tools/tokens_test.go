package tools_test

import (
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

// People sign in and hold no API token; agents hold API tokens and nothing
// else (docs/schema.md §2.1). Each tool that would give the wrong one
// refuses, whoever asks, saying which rule in details.reason.

// session signs actor in, as a password or single sign-on does, and returns
// the session's credential.
func (b *built) session(t *testing.T, actor uuid.UUID) uuid.UUID {
	t.Helper()
	authn := auth.NewAuthenticator(b.Pool, time.Hour)
	sess, err := authn.StartSession(t.Context(), actor, "password login")
	if err != nil {
		t.Fatal(err)
	}
	p, err := authn.Authenticate(t.Context(), sess.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p.CredentialID
}

// refusedFor insists a call was attempted and failed, forbidden, for why.
func refusedFor(t *testing.T, what string, out pipeline.Outcome, why string) {
	t.Helper()
	if out.Status != domain.StatusFailed || out.Error == nil || out.Error.Code != apperr.Forbidden || reason(out) != why {
		t.Fatalf("%s: %+v, want failed forbidden (%s)", what, out, why)
	}
}

func TestAPersonIsIssuedNoToken(t *testing.T) {
	b := build(t)
	// Not their own, whoever they are: a student, an instructor, an
	// administrator, root; and not for lack of a label.
	for name, who := range map[string]uuid.UUID{"a student": b.yuki, "an instructor": b.sato, "an administrator": b.admin, "root": b.Root} {
		out := b.MustCall(who, "credential.issue_token", m{"label": "my script"}, "own-"+who.String())
		refusedFor(t, name+"'s own token", out, auth.ReasonTokensForAgents)
		if !strings.Contains(out.Error.Message, "signs in with a password or single sign-on") ||
			!strings.Contains(out.Error.Message, "agent.issue_token") {
			t.Fatalf("the refusal does not say what to do instead: %s", out.Error.Message)
		}
	}
	refusedFor(t, "a token with no label", b.MustCall(b.yuki, "credential.issue_token", m{"label": ""}, "no-label"),
		auth.ReasonTokensForAgents)
	// Nor issued them by an administrator, themselves included, or by root.
	for name, who := range map[string]uuid.UUID{"a student": b.yuki, "the administrator himself": b.admin} {
		refusedFor(t, "an administrator's token for "+name, b.MustCall(b.admin, "actor.issue_token",
			m{"actor_id": who, "label": "for them"}, "for-"+who.String()), auth.ReasonTokensForAgents)
	}
	refusedFor(t, "root's token for an administrator", b.MustCall(b.Root, "actor.issue_token",
		m{"actor_id": b.admin, "label": "for him"}, "root-for-admin"), auth.ReasonTokensForAgents)
	if n := b.Count(`SELECT count(*) FROM credential c JOIN actor a ON a.id = c.actor_id WHERE a.kind = 'human' AND c.kind = 'api_token'`); n != 0 {
		t.Fatalf("%d people hold API tokens", n)
	}

	// An agent is issued one: by itself, by an administrator, by its owner.
	b.do(t, b.grader, "credential.issue_token", m{"label": "rotated"})
	b.do(t, b.admin, "actor.issue_token", m{"actor_id": b.grader, "label": "server"})
	bot := b.agent(t, b.yuki, "Yuki's helper")
	tok := testkit.Result[tools.IssueTokenOut](t, b.do(t, b.yuki, "agent.issue_token", m{"actor_id": bot, "label": "laptop"}))
	if !strings.HasPrefix(tok.Token, "ais_") {
		t.Fatalf("the owner's agent's token: %+v", tok)
	}
}

func TestAnAgentIsGivenNoWayToSignIn(t *testing.T) {
	b := build(t)
	b.do(t, b.admin, "actor.update", m{"actor_id": b.grader, "email": "grader@example.edu"})
	bot := b.agent(t, b.yuki, "Yuki's helper")

	// No invitation, whoever invites it, and with an email to sign in with.
	refusedFor(t, "an administrator inviting an agent", b.MustCall(b.admin, "actor.invite", m{"actor_id": b.grader}, "inv-admin"),
		auth.ReasonAgentsUseTokens)
	refusedFor(t, "root inviting an agent", b.MustCall(b.Root, "actor.invite", m{"actor_id": b.grader}, "inv-root"),
		auth.ReasonAgentsUseTokens)
	// No identity at a provider.
	refusedFor(t, "an agent's identity linked", b.MustCall(b.admin, "actor.link_sso",
		m{"actor_id": b.grader, "provider": "polyu-adfs", "subject": "grader@example.edu"}, "sso"), auth.ReasonAgentsUseTokens)
	// No password of its own, whoever owns it.
	for name, agent := range map[string]uuid.UUID{"an agent nobody owns": b.grader, "an agent Yuki owns": bot} {
		refusedFor(t, name+" setting a password", b.MustCall(agent, "credential.set_password",
			m{"password": "an agent's password"}, "pw-"+agent.String()), auth.ReasonAgentsUseTokens)
	}
	if n := b.Count(`SELECT count(*) FROM credential WHERE actor_id IN ($1, $2) AND kind <> 'api_token'`, b.grader, bot); n != 0 {
		t.Fatalf("the agents were given %d credentials other than tokens", n)
	}
	// Found by its email, it is not one to invite.
	found := testkit.Result[tools.ActorLookupOut](t, b.do(t, b.admin, "actor.lookup_by_email", m{"email": "grader@example.edu"}))
	if found.ActorID != b.grader || found.Invitable || found.CanSignIn {
		t.Fatalf("the agent, looked up: %+v", found)
	}

	// A person is invited, linked, and sets a password, as ever.
	b.do(t, b.admin, "actor.update", m{"actor_id": b.yuki, "email": "yuki@example.edu"})
	if found := testkit.Result[tools.ActorLookupOut](t, b.do(t, b.admin, "actor.lookup_by_email", m{"email": "yuki@example.edu"})); !found.Invitable {
		t.Fatalf("Yuki, looked up: %+v", found)
	}
	b.do(t, b.admin, "actor.invite", m{"actor_id": b.yuki})
	b.do(t, b.admin, "actor.link_sso", m{"actor_id": b.yuki, "provider": "polyu-adfs", "subject": "yuki@example.edu"})
	b.do(t, b.yuki, "credential.set_password", m{"password": "yukis own password"})
}
