package tools_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tools"
)

// People sign in and hold no API token (docs/schema.md §2.1). Each tool that
// would give them one refuses, whoever asks, saying so in details.reason.

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
