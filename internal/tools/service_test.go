package tools_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/pipeline"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// A site service (docs/schema.md §2.1, Services) calls its own tools and
// nothing else, and nobody else calls them; its credentials are the
// platform administrators' to issue and revoke, and a revoked one stops at
// once.

// A service's credential opens nothing but its service's tools: not a read,
// not a write, not its own account, not a course.
func TestAServiceCallsItsToolsAndNothingElse(t *testing.T) {
	b := build(t)
	svc := b.transcriber(t)
	doc, _ := b.slides(t, "Week 1")
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc})
	for name, args := range map[string]m{
		"me.get":                     {},
		"me.memberships":             {},
		"credential.list":            {},
		"credential.issue_token":     {"label": "more"},
		"course.list":                {},
		"actor.list":                 {},
		"document.get":               {"course_id": b.course, "document_id": doc},
		"document.text":              {"course_id": b.course, "document_id": doc, "file_id": uuid.New()},
		"document.list":              {"course_id": b.course},
		"document.upload_url":        {"course_id": b.course, "kind": "material", "content_type": "application/pdf"},
		"document.text_update":       {"course_id": b.course, "document_id": doc, "version_id": uuid.New(), "file_id": uuid.New(), "body": "x"},
		"event.list":                 {"course_id": b.course},
		"member.list":                {"course_id": b.course},
		"conversation.inbox":         {"course_id": b.course},
		"service.issue_credential":   {"scope": "document_text", "label": "another"},
		"service.list_credentials":   {"scope": "document_text"},
		"memory.search":              {"query": "x"},
		"department.list_tree":       {},
		"submission.list":            {"course_id": b.course},
		"grade.list":                 {"course_id": b.course},
		"action.list_pending_review": {"course_id": b.course},
	} {
		out, err := b.CallWith(svc, name, args, "svc-other-"+name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.Status != domain.StatusDenied || out.Error.Details["reason"] != "not_for_services" {
			t.Fatalf("the service calling %s: %+v", name, out)
		}
	}
	// Nobody else calls the service's tools: not root, not the course's
	// instructor, not an agent.
	for _, who := range []uuid.UUID{b.Root, b.admin, b.sato, b.tutor} {
		out := b.MustCall(who, "document_text.queue", m{}, "")
		if out.Status != domain.StatusDenied || out.Error.Details["reason"] != "service_only" {
			t.Fatalf("%s claiming from the queue: %+v", who, out)
		}
		out = b.MustCall(who, "document_text.complete", m{"version_id": uuid.New(), "file_id": uuid.New(), "lease_id": uuid.New(),
			"status": "failed", "reason": "no"}, "complete-"+who.String())
		if out.Status != domain.StatusDenied || out.Error.Details["reason"] != "service_only" {
			t.Fatalf("%s writing back a text: %+v", who, out)
		}
	}
	// Nor does the service call them without its credential, or with one
	// that is not its own.
	for _, caller := range []pipeline.Caller{{ActorID: svc.ActorID}, {ActorID: svc.ActorID, CredentialID: uuid.New()}} {
		if out := b.MustCallWith(caller, "document_text.queue", m{}); out.Status != domain.StatusDenied || out.Error.Details["reason"] != "service_only" {
			t.Fatalf("the service without its credential: %+v", out)
		}
	}
	// It has no account to manage, no seat to be given, and no token.
	b.try(t, b.admin, "actor.suspend", m{"actor_id": svc.ActorID}, apperr.Forbidden)
	b.try(t, b.admin, "actor.issue_token", m{"actor_id": svc.ActorID, "label": "x"}, apperr.Forbidden)
	b.try(t, b.admin, "actor.list_credentials", m{"actor_id": svc.ActorID}, apperr.Forbidden)
	b.try(t, b.sato, "member.add", m{"course_id": b.course, "actor_id": svc.ActorID, "preset": "tutor"}, apperr.FailedPrecondition)
	for _, a := range testkit.Result[tools.ActorListOut](t, b.do(t, b.admin, "actor.list", m{"limit": 200})).Actors {
		if a.ID == svc.ActorID {
			t.Fatal("actor.list lists the service")
		}
	}
	if _, _, err := auth.IssueToken(context.Background(), b.Q, svc.ActorID, nil, "cli", nil, time.Now()); !apperr.Is(err, apperr.Forbidden) {
		t.Fatalf("a token for the service from the command line: %v", err)
	}
}

func (b *built) MustCallWith(caller pipeline.Caller, name string, args m) pipeline.Outcome {
	b.T.Helper()
	out, err := b.CallWith(caller, name, args, "with-"+uuid.NewString())
	if err != nil {
		b.T.Fatalf("%s: %v", name, err)
	}
	return out
}

// Only the platform's administrators issue, list and revoke a service's
// credentials; each is recorded.
func TestOnlyAdministratorsManageAServicesCredentials(t *testing.T) {
	b := build(t)
	for _, who := range []uuid.UUID{b.sato, b.tutor, b.yuki} {
		out := b.MustCall(who, "service.issue_credential", m{"scope": "document_text", "label": "mine"}, "issue-"+who.String())
		if out.Status != domain.StatusDenied || out.Error.Details["reason"] != "platform_role_required" {
			t.Fatalf("%s issuing a service credential: %+v", who, out)
		}
		if out := b.MustCall(who, "service.list_credentials", m{"scope": "document_text"}, ""); out.Status != domain.StatusDenied {
			t.Fatalf("%s listing a service's credentials: %+v", who, out)
		}
	}
	b.try(t, b.admin, "service.issue_credential", m{"scope": "grading", "label": "x"}, apperr.InvalidArgument)
	listed := testkit.Result[tools.ServiceListCredentialsOut](t, b.do(t, b.admin, "service.list_credentials", m{"scope": "document_text"}))
	if listed.ServiceActorID != nil || len(listed.Credentials) != 0 {
		t.Fatalf("before any is issued: %+v", listed)
	}

	first := b.MustCall(b.admin, "service.issue_credential", m{"scope": "document_text", "label": "runtime"}, "issue-1")
	issued := testkit.Result[tools.ServiceIssueCredentialOut](t, first)
	// The token is shown once: not recorded, not replayed.
	replay := b.MustCall(b.admin, "service.issue_credential", m{"scope": "document_text", "label": "runtime"}, "issue-1")
	if !replay.Replayed || testkit.Result[tools.ServiceIssueCredentialOut](t, replay).Token != "" {
		t.Fatalf("the issue replayed: %s", replay.Result)
	}
	if n := b.Count(`SELECT count(*) FROM action WHERE id = $1 AND result::text LIKE '%aissvc_%'`, first.ActionID); n != 0 {
		t.Fatal("the token is in the action log")
	}
	p, err := auth.NewAuthenticator(b.Pool, 0).Authenticate(context.Background(), issued.Token)
	if err != nil || p.ActorID != issued.ServiceActorID || !p.Service() {
		t.Fatalf("the credential authenticates %+v %v", p, err)
	}
	// Under the scheme of an API token it is no credential at all.
	if _, err := auth.NewAuthenticator(b.Pool, 0).Authenticate(context.Background(), "ais"+issued.Token[len("aissvc"):]); err == nil {
		t.Fatal("a service credential passed as an API token")
	}

	for i := 2; i <= tools.MaxServiceCredentials; i++ {
		b.do(t, b.admin, "service.issue_credential", m{"scope": "document_text", "label": "spare"})
	}
	out := b.MustCall(b.admin, "service.issue_credential", m{"scope": "document_text", "label": "one too many"}, "too-many")
	if out.Status != domain.StatusFailed || out.Error.Details["reason"] != "too_many_credentials" {
		t.Fatalf("a sixth credential: %+v", out)
	}
	replaced := testkit.Result[tools.ServiceIssueCredentialOut](t, b.do(t, b.admin, "service.issue_credential",
		m{"scope": "document_text", "label": "new runtime", "replace": true}))
	if len(replaced.Revoked) != tools.MaxServiceCredentials || replaced.ServiceActorID != issued.ServiceActorID {
		t.Fatalf("replacing them: %+v", replaced)
	}
	listed = testkit.Result[tools.ServiceListCredentialsOut](t, b.do(t, b.admin, "service.list_credentials", m{"scope": "document_text"}))
	live := 0
	for _, c := range listed.Credentials {
		if c.Live {
			live++
		}
	}
	if live != 1 || len(listed.Credentials) != tools.MaxServiceCredentials+1 || listed.Credentials[0].ID != replaced.CredentialID {
		t.Fatalf("after replacing: %+v", listed)
	}
	if _, err := auth.NewAuthenticator(b.Pool, 0).Authenticate(context.Background(), issued.Token); err == nil {
		t.Fatal("a replaced credential still authenticates")
	}
	if n := b.Count(`SELECT count(*) FROM event WHERE type = $1`, tools.EventServiceCredentialRevoked); n != tools.MaxServiceCredentials {
		t.Fatalf("%d revocations told", n)
	}
}

// Revoking a service's credential stops it at once: its next call, and a
// call of it waiting on the queue, and what it had claimed goes back in the
// queue for another.
func TestRevokingAServicesCredentialStopsItAtOnce(t *testing.T) {
	b, hub := waking(t, wake.DefaultConfig)
	svc := b.transcriber(t)
	_, v := b.slides(t, "Week 1")
	c := b.claim(t, svc, m{})[0]

	// A second credential waits on the queue, with nothing to claim.
	other := b.transcriber(t)
	waiting := make(chan read, 1)
	go func() {
		out, err := b.P.Invoke(context.Background(), other, "document_text.queue", []byte(`{"wait_s": 10}`), "")
		waiting <- read{out, err, time.Now()}
	}()
	waitingNow(t, hub, 1)

	revoked := testkit.Result[tools.ServiceRevokeCredentialOut](t, b.do(t, b.admin, "service.revoke_credential",
		m{"scope": "document_text", "credential_id": svc.CredentialID}))
	if revoked.ClaimsReleased != 1 || textStatus(t, b, v) != "pending" {
		t.Fatalf("revoked: %+v, the text %s", revoked, textStatus(t, b, v))
	}
	// Its claim is gone, and so is the credential.
	if out := b.MustCallWith(svc, "document_text.complete", m{"version_id": v, "file_id": c.FileID, "lease_id": c.LeaseID,
		"status": "done", "body": "x", "pages": 1, "model": "M"}); out.Status != domain.StatusDenied || out.Error.Details["reason"] != "service_only" {
		t.Fatalf("the revoked credential writing back: %+v", out)
	}
	// What it held went back, and woke the other credential's claim, which
	// took it.
	r := answered(t, waiting, 5*time.Second)
	if got := resultOf[tools.TextQueueOut](t, r).Claimed; len(got) != 1 || got[0].VersionID != v || got[0].Attempt != 1 {
		t.Fatalf("the other credential's waiting claim: %+v", got)
	}

	// A claim waiting when its own credential is revoked stops there.
	_, v2 := b.slides(t, "Week 2")
	b.Exec(`UPDATE document_version_text SET status = 'skipped', reason = 'held back' WHERE version_id = $1`, v2)
	waiting = make(chan read, 1)
	go func() {
		out, err := b.P.Invoke(context.Background(), other, "document_text.queue", []byte(`{"wait_s": 10}`), "")
		waiting <- read{out, err, time.Now()}
	}()
	waitingNow(t, hub, 1)
	b.do(t, b.admin, "service.revoke_credential", m{"scope": "document_text", "credential_id": other.CredentialID})
	_, v3 := b.slides(t, "Week 3")
	r = answered(t, waiting, 5*time.Second)
	if r.out.Status != domain.StatusDenied || textStatus(t, b, v3) != "pending" {
		t.Fatalf("a waiting claim of a revoked credential: %+v, the text %s", r.out, textStatus(t, b, v3))
	}
	b.try(t, b.admin, "service.revoke_credential", m{"scope": "document_text", "credential_id": other.CredentialID}, apperr.NotFound)
}
