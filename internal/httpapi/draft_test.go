package httpapi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/mcpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// An answer's draft over either door: no Idempotency-Key, no action, and,
// carried out, nothing spent of the caller's rate limit; written faster
// than a conversation's drafts may be, a 429 that says when to try again,
// which counts as any refusal does.
func TestADraftIsNoActionAndCostsNothingOfTheLimit(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	calls := ratelimit.New(60, 3)
	calls.SetClock(clock)
	drafts := ratelimit.New(60*tools.DraftWritesPerSecond, tools.DraftWritesPerSecond)
	drafts.SetClock(clock)
	c := testkit.NewCS101WithDeps(t, 1, func(d *tools.Deps) { d.Drafts = drafts })
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewAuthenticator(c.Pool, time.Hour)
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: authn, Calls: calls,
		MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: c.P, Auth: authn, Calls: calls}),
	}))
	t.Cleanup(srv.Close)
	a := &api{t: t, c: c, srv: srv}

	// A tutor, run by something that answers in the site, asked by Yuki.
	yuki := c.Students[0]
	tutor := c.RuntimeAgent("tutor")
	respondent := c.Member(c.Course, tutor, "tutor", testkit.ListedStudents(yuki.Member))
	_, token := c.HostToken(tutor)
	opened := c.MustCall(yuki.Actor, "conversation.open", m{"course_id": c.Course, "respondent_member_id": respondent, "body": "Where do I start?"}, "open")
	if opened.Status != domain.StatusExecuted {
		t.Fatalf("opening the conversation: %+v", opened)
	}
	conv := testkit.Result[tools.ConversationOpenOut](t, opened).ConversationID
	path := fmt.Sprintf("/v1/courses/%s/conversations/%s/draft", c.Course, conv)
	student := a.tokenFor(yuki.Actor)
	actions := c.Count(`SELECT count(*) FROM action`)

	// Six drafts, twice the caller's allowance, with no key: each carried
	// out, none recorded, none spent.
	for v := 1; v <= 6; v++ {
		r := a.do(nil, "POST", path, token, m{"attempt": "a1", "version": v, "text": strings.Repeat("x", v)})
		if r.Status != 200 || r.Body["action_id"] != nil || r.Body["status"] != "executed" || r.Body["result"].(m)["stored"] != true {
			t.Fatalf("draft %d: %d %s", v, r.Status, r.Raw)
		}
	}
	// And over MCP, as the same actor, with no idempotency_key.
	for v := 7; v <= 9; v++ {
		call := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"conversation_draft","arguments":`+
			`{"course_id":%q,"conversation_id":%q,"attempt":"a1","version":%d}}}`, v, c.Course, conv, v)
		res, body := a.raw("POST", srv.URL+httpapi.MCPPath, "application/json", []byte(call),
			"Authorization", "Bearer "+token, "Accept", "application/json, text/event-stream")
		if res.StatusCode != 200 || !strings.Contains(string(body), `"stored":true`) || strings.Contains(string(body), "action_id") {
			t.Fatalf("draft %d over MCP: %d %s", v, res.StatusCode, body)
		}
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != actions {
		t.Fatalf("%d actions recorded for drafts", n-actions)
	}
	r := a.do(nil, "GET", fmt.Sprintf("/v1/courses/%s/conversations/%s", c.Course, conv), student, nil)
	if d, _ := r.Body["result"].(m)["draft"].(m); r.Status != 200 || d["version"] != float64(9) || d["text"] != "xxxxxx" {
		t.Fatalf("the draft, as Yuki reads it: %d %s", r.Status, r.Raw)
	}

	// The eleventh in a second is refused, saying when to try again, and
	// counted: the caller's allowance of three is two now.
	r = a.do(nil, "POST", path, token, m{"attempt": "a1", "version": 10})
	if r.Status != 200 {
		t.Fatalf("draft 10: %d %s", r.Status, r.Raw)
	}
	r = a.do(nil, "POST", path, token, m{"attempt": "a1", "version": 11})
	if r.Status != http.StatusTooManyRequests || r.str("error", "code") != "rate_limited" || r.Header.Get("Retry-After") != "1" ||
		r.Body["action_id"] != nil {
		t.Fatalf("draft 11: %d %v %s", r.Status, r.Header, r.Raw)
	}
	for i := range 2 {
		if r := a.do(nil, "GET", "/v1/me", token, nil); r.Status != 200 {
			t.Fatalf("call %d after the refused draft: %d", i+1, r.Status)
		}
	}
	if r := a.do(nil, "GET", "/v1/me", token, nil); r.Status != http.StatusTooManyRequests {
		t.Fatalf("a call past the allowance: %d", r.Status)
	}
	// Someone who may not write it is refused, and counted too.
	if r := a.do(nil, "POST", path, student, m{"attempt": "a1", "version": 12}); r.Status != http.StatusForbidden || r.Body["action_id"] != nil {
		t.Fatalf("Yuki writing the draft: %d %s", r.Status, r.Raw)
	}

	// The catalogue says what it is: no write, no read, but ephemeral.
	listed := a.do(nil, "GET", "/v1/tools", "", nil)
	for _, it := range listed.Body["tools"].([]any) {
		if tl := it.(map[string]any); tl["name"] == "conversation.draft" {
			if tl["kind"] != "ephemeral" || tl["method"] != "POST" || tl["path"] != "/v1/courses/{course_id}/conversations/{conversation_id}/draft" {
				t.Fatalf("conversation.draft, as the catalogue lists it: %v", tl)
			}
			return
		}
	}
	t.Fatal("conversation.draft is not in the catalogue")
}
