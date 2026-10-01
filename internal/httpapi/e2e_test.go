package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
)

type m = map[string]any

const frontEnd = "https://lms.example.edu"

type api struct {
	t   *testing.T
	c   *testkit.CS101
	srv *httptest.Server
}

func newAPI(t *testing.T, students int) *api {
	t.Helper()
	c := testkit.NewCS101(t, students)
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P,
		Auth:           auth.NewAuthenticator(c.Pool, time.Hour),
		TrustedOrigins: []string{frontEnd}, InsecureCookies: true,
		Blob: c.Blob, MaxUploadBytes: testkit.MaxUploadBytes,
	}))
	t.Cleanup(srv.Close)
	return &api{t: t, c: c, srv: srv}
}

// tokenFor is what actor calls with: an agent's API token, issued the way
// the operator's command line issues one, and a person's session, as signing
// in starts one. A person holds no API token.
func (a *api) tokenFor(actor uuid.UUID) string {
	a.t.Helper()
	ctx := context.Background()
	q := dbq.New(a.c.Pool)
	who, err := q.GetActor(ctx, actor)
	if err != nil {
		a.t.Fatal(err)
	}
	if who.Kind != "agent" {
		sess, err := auth.NewAuthenticator(a.c.Pool, time.Hour).StartSession(ctx, actor, "password login")
		if err != nil {
			a.t.Fatal(err)
		}
		return sess.Token
	}
	tok, _, err := auth.IssueToken(ctx, q, actor, nil, "test", nil, time.Now())
	if err != nil {
		a.t.Fatal(err)
	}
	return tok.Full
}

type response struct {
	Status int
	Header http.Header
	Body   m
	Raw    string
}

func (r response) str(path ...string) string {
	var v any = r.Body
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = mm[p]
	}
	s, _ := v.(string)
	return s
}

func (a *api) do(client *http.Client, method, path, token string, body any, headers ...string) response {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.srv.URL+path, rd)
	if err != nil {
		a.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := response{Status: res.StatusCode, Header: res.Header, Raw: string(raw)}
	// A redirect's body is a courtesy link for browsers that do not follow
	// it; everything else this API says, it says in JSON.
	if len(raw) > 0 && (res.StatusCode < 300 || res.StatusCode >= 400) {
		if err := json.Unmarshal(raw, &out.Body); err != nil {
			a.t.Fatalf("%s %s: body is not JSON: %s", method, path, raw)
		}
	}
	return out
}

// The M3 scenario: token → POST a grade (202, proposed) → decide (200) →
// replay (same body, Idempotency-Replayed).
func TestEndToEndOverHTTP(t *testing.T) {
	a := newAPI(t, 1)
	c, yuki := a.c, a.c.Students[0]
	agent, sato := a.tokenFor(c.Grader), a.tokenFor(c.Sato)
	course := "/v1/courses/" + c.Course.String()

	// The agent finds out where it is seated.
	me := a.do(nil, "GET", "/v1/me/memberships", agent, nil)
	if me.Status != 200 || !strings.Contains(me.Raw, c.GraderM.String()) || !strings.Contains(me.Raw, `"code":"CS101"`) {
		t.Fatalf("memberships: %d %s", me.Status, me.Raw)
	}

	// It grades. confirm_required → 202 Accepted, and an action id to watch.
	grade := m{"submission_id": yuki.HW3, "score": 85, "feedback": "Clear thesis."}
	proposed := a.do(nil, "POST", course+"/grades", agent, grade, "Idempotency-Key", "yuki-hw3")
	if proposed.Status != http.StatusAccepted || proposed.str("status") != "proposed" || proposed.str("action_id") == "" {
		t.Fatalf("grade.submit: %d %s", proposed.Status, proposed.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatal("a proposal wrote a grade")
	}
	actionID := proposed.str("action_id")

	// The timeout-and-retry case: same key, same content → the same answer,
	// marked as a replay. A different body under that key → 409.
	retry := a.do(nil, "POST", course+"/grades", agent, grade, "Idempotency-Key", "yuki-hw3")
	if retry.Status != http.StatusAccepted || retry.str("action_id") != actionID || retry.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("retry: %d %v %s", retry.Status, retry.Header, retry.Raw)
	}
	clash := a.do(nil, "POST", course+"/grades", agent, m{"submission_id": yuki.HW3, "score": 60}, "Idempotency-Key", "yuki-hw3")
	if clash.Status != http.StatusConflict || clash.str("error", "code") != "idempotency_conflict" {
		t.Fatalf("same key, different content: %d %s", clash.Status, clash.Raw)
	}

	// Sato sees the queue and approves.
	queue := a.do(nil, "GET", course+"/actions/proposed?limit=10", sato, nil)
	if queue.Status != 200 || !strings.Contains(queue.Raw, actionID) {
		t.Fatalf("approval queue: %d %s", queue.Status, queue.Raw)
	}
	decide := a.do(nil, "POST", course+"/actions/"+actionID+"/decide", sato, m{"decision": "approve"}, "Idempotency-Key", "approve-1")
	if decide.Status != 200 || decide.str("result", "outcome") != "executed" {
		t.Fatalf("decide: %d %s", decide.Status, decide.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM grade WHERE score = 85 AND created_by_action_id = $1`, actionID); n != 1 {
		t.Fatal("approval did not write the grade under the proposal's id")
	}
	// The agent's original call, replayed now, reports what became of it.
	after := a.do(nil, "POST", course+"/grades", agent, grade, "Idempotency-Key", "yuki-hw3")
	if after.Status != 200 || after.str("status") != "executed" || after.str("result", "grade_id") == "" {
		t.Fatalf("replay after approval: %d %s", after.Status, after.Raw)
	}
	// The agent may not decide, and is told so with a 403 that is on record.
	denied := a.do(nil, "POST", course+"/actions/"+actionID+"/decide", agent, m{"decision": "approve"}, "Idempotency-Key", "self")
	if denied.Status != http.StatusForbidden || denied.str("status") != "denied" || denied.str("action_id") == "" {
		t.Fatalf("agent deciding: %d %s", denied.Status, denied.Raw)
	}

	// Reads: a GET with the student in the path and a flag in the query.
	book := a.do(nil, "GET", course+"/gradebook/"+yuki.Member.String()+"?treat_ungraded_as_zero=true", sato, nil)
	if book.Status != 200 || book.str("status") != "executed" {
		t.Fatalf("gradebook: %d %s", book.Status, book.Raw)
	}
}

// The site's agent runtime hosts an agent by its id, over REST, with its
// own credential: it asks whether the person signed in to it owns the agent,
// is issued the agent's token, and revokes it when the hosting ends. The
// token acts as the agent, which is told it is a runtime agent; its owner
// is issued none; me.site_chat with it changes nothing, and a person's is
// refused. Nobody else calls the runtime's routes, and its credential calls
// nothing else.
func TestTheRuntimeHostsAnAgentByItsIDOverREST(t *testing.T) {
	a := newAPI(t, 1)
	c := a.c
	tutor, script := c.OwnedRuntimeAgent(c.Sato, "Course tutor"), c.OwnedAgent(c.Sato, "Sato's assistant")
	_, _, svc := c.RuntimeService()
	sato := a.tokenFor(c.Sato)
	base := "/v1/services/agent_runtime"

	owns := a.do(nil, "GET", base+"/owners/"+c.Sato.String()+"/agents/"+tutor.String(), svc, nil)
	if owns.Status != http.StatusOK || owns.Body["result"].(m)["owns"] != true || owns.str("result", "agent", "hosting") != "runtime" {
		t.Fatalf("whether Sato owns the tutor: %d %s", owns.Status, owns.Raw)
	}
	notHis := a.do(nil, "GET", base+"/owners/"+c.Students[0].Actor.String()+"/agents/"+tutor.String(), svc, nil)
	if notHis.Status != http.StatusOK || notHis.Body["result"].(m)["owns"] != false || notHis.Body["result"].(m)["agent"] != nil {
		t.Fatalf("whether Yuki owns it: %d %s", notHis.Status, notHis.Raw)
	}

	issued := a.do(nil, "POST", base+"/agents/"+tutor.String()+"/token", svc, m{}, "Idempotency-Key", "host-1")
	token := issued.str("result", "token")
	if issued.Status != http.StatusOK || !strings.HasPrefix(token, "ais_") {
		t.Fatalf("the tutor's token: %d %s", issued.Status, issued.Raw)
	}
	replay := a.do(nil, "POST", base+"/agents/"+tutor.String()+"/token", svc, m{}, "Idempotency-Key", "host-1")
	if replay.Status != http.StatusOK || strings.Contains(replay.Raw, token) || replay.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("the replay: %d %s", replay.Status, replay.Raw)
	}
	me := a.do(nil, "GET", "/v1/me", token, nil)
	if me.Status != http.StatusOK || me.str("result", "id") != tutor.String() || me.str("result", "hosting") != "runtime" {
		t.Fatalf("the tutor, with its token: %d %s", me.Status, me.Raw)
	}
	view := a.do(nil, "GET", base+"/agents/"+tutor.String(), svc, nil)
	if view.Status != http.StatusOK || view.str("result", "runtime_token", "credential_id") != issued.str("result", "credential_id") {
		t.Fatalf("the tutor, as the runtime reads it: %d %s", view.Status, view.Raw)
	}

	// Nothing is declared: me.site_chat is gone (0027).
	if on := a.do(nil, "POST", "/v1/me/site-chat", token, m{"on": true}, "Idempotency-Key", "start-1"); on.Status != http.StatusNotFound {
		t.Fatalf("me.site_chat: %d %s", on.Status, on.Raw)
	}

	// Its owner is issued no token for it; for the mcp agent, as ever.
	own := a.do(nil, "POST", "/v1/me/agents/"+tutor.String()+"/tokens", sato, m{"label": "laptop"}, "Idempotency-Key", "own")
	if own.Status != http.StatusForbidden || own.str("error", "details", "reason") != "hosted_by_runtime" {
		t.Fatalf("Sato's token for the tutor: %d %s", own.Status, own.Raw)
	}
	mcp := a.do(nil, "POST", "/v1/me/agents/"+script.String()+"/tokens", sato, m{"label": "editor"}, "Idempotency-Key", "own-mcp")
	if mcp.Status != http.StatusOK || !strings.HasPrefix(mcp.str("result", "token"), "ais_") {
		t.Fatalf("Sato's token for his assistant: %d %s", mcp.Status, mcp.Raw)
	}
	refused := a.do(nil, "POST", base+"/agents/"+script.String()+"/token", svc, m{}, "Idempotency-Key", "host-mcp")
	if refused.Status != http.StatusUnprocessableEntity || refused.str("error", "details", "reason") != "not_runtime_hosted" {
		t.Fatalf("the runtime hosting an mcp agent: %d %s", refused.Status, refused.Raw)
	}

	// Nobody else calls its routes, and its credential calls nothing else.
	for _, who := range []string{sato, token, a.tokenFor(c.Root)} {
		if r := a.do(nil, "POST", base+"/agents/"+tutor.String()+"/token", who, m{}, "Idempotency-Key", "steal"); r.Status != http.StatusForbidden ||
			r.str("error", "details", "reason") != "service_only" {
			t.Fatalf("someone else issuing the tutor's token: %d %s", r.Status, r.Raw)
		}
	}
	if r := a.do(nil, "GET", "/v1/me", svc, nil); r.Status != http.StatusForbidden || r.str("error", "details", "reason") != "not_for_services" {
		t.Fatalf("the runtime's credential at /v1/me: %d %s", r.Status, r.Raw)
	}

	// The hosting ends: the token is revoked, and opens nothing.
	revoked := a.do(nil, "POST", base+"/agents/"+tutor.String()+"/token/revoke", svc, m{}, "Idempotency-Key", "unhost-1")
	if revoked.Status != http.StatusOK || len(revoked.Body["result"].(m)["revoked"].([]any)) != 1 {
		t.Fatalf("revoking the tutor's token: %d %s", revoked.Status, revoked.Raw)
	}
	if r := a.do(nil, "GET", "/v1/me", token, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("the tutor's revoked token: %d %s", r.Status, r.Raw)
	}
}

// A person asks for an API token in vain, of their own or from an
// administrator: 403. One from before migration 0017 revoked them opens
// nothing, 401, and says why; so does an agent's password at sign-in.
// People sign in, and agents hold tokens.
func TestTokensAreForAgentsAndSigningInForPeople(t *testing.T) {
	a := newAPI(t, 0)
	c := a.c
	root, sato := a.tokenFor(c.Root), a.tokenFor(c.Sato)
	own := a.do(nil, "POST", "/v1/me/credentials/tokens", sato, m{"label": "my script"}, "Idempotency-Key", "own")
	if own.Status != http.StatusForbidden || own.str("error", "details", "reason") != "api_tokens_are_for_agents" {
		t.Fatalf("a person's own token: %d %s", own.Status, own.Raw)
	}
	given := a.do(nil, "POST", "/v1/actors/"+c.Sato.String()+"/tokens", root, m{"label": "for Sato"}, "Idempotency-Key", "given")
	if given.Status != http.StatusForbidden || given.str("error", "details", "reason") != "api_tokens_are_for_agents" {
		t.Fatalf("root's token for a person: %d %s", given.Status, given.Raw)
	}
	agents := a.do(nil, "POST", "/v1/actors/"+c.Grader.String()+"/tokens", root, m{"label": "server"}, "Idempotency-Key", "agents")
	if agents.Status != http.StatusOK || !strings.HasPrefix(agents.str("result", "token"), "ais_") {
		t.Fatalf("root's token for an agent: %d %s", agents.Status, agents.Raw)
	}

	// What the release before could leave: a token of Sato's, and a
	// password of the grader's, which has an email to sign in with.
	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword("the grader's password")
	if err != nil {
		t.Fatal(err)
	}
	c.Exec(`UPDATE actor SET email = 'grader@example.edu' WHERE id = $1`, c.Grader)
	c.Exec(`ALTER TABLE credential DISABLE TRIGGER credential_fits_actor_kind`)
	c.Exec(`INSERT INTO credential (actor_id, kind, secret_hash, token_prefix) VALUES ($1, 'api_token', $2, $3)`, c.Sato, tok.Hash, tok.Prefix)
	c.Exec(`INSERT INTO credential (actor_id, kind, secret_hash) VALUES ($1, 'password', $2)`, c.Grader, hash)
	c.Exec(`ALTER TABLE credential ENABLE TRIGGER credential_fits_actor_kind`)
	me := a.do(nil, "GET", "/v1/me", tok.Full, nil)
	if me.Status != http.StatusUnauthorized || me.str("error", "details", "reason") != "api_tokens_are_for_agents" ||
		me.Header.Get("WWW-Authenticate") == "" || !strings.Contains(me.str("error", "message"), "sign") {
		t.Fatalf("Sato's token: %d %s", me.Status, me.Raw)
	}
	login := a.do(nil, "POST", "/v1/auth/login", "", m{"login": "grader@example.edu", "password": "the grader's password"})
	if login.Status != http.StatusUnauthorized || login.str("error", "details", "reason") != "agents_use_api_tokens" ||
		login.Header.Get("Set-Cookie") != "" {
		t.Fatalf("the grader signing in: %d %s", login.Status, login.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM credential WHERE actor_id = $1 AND kind = 'session'`, c.Grader); n != 0 {
		t.Fatal("the grader was given a session")
	}
}

// What a status says of whether anything was recorded: a call that was
// attempted answers with the action on record as its top-level action_id,
// and a failure there with its error's own status, 400 and 404 included. A
// call that never was, and a key used for another call, name no action of
// their own.
func TestStatusCodes(t *testing.T) {
	a := newAPI(t, 1)
	c, yuki := a.c, a.c.Students[0]
	sato, grader, student := a.tokenFor(c.Sato), a.tokenFor(c.Grader), a.tokenFor(yuki.Actor)
	course := "/v1/courses/" + c.Course.String()
	key := func(k string) []string { return []string{"Idempotency-Key", k} }

	// A TA whose grades wait for someone else's approval, and who approves
	// others'; one of the agent's proposals rejected, and one cancelled when
	// its seat is taken away; a grade of Sato's refused, on record under its
	// key; and HW4 not published after all.
	ta := c.Actor("human", "TA")
	c.Member(c.Course, ta, "ta",
		testkit.WithPerm(domain.PermGradeSubmit, domain.ConfirmRequired),
		testkit.WithPerm(domain.PermActionDecide, domain.Autonomous))
	own := c.MustCall(ta, "grade.submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 50}, "own")
	toReject := m{"submission_id": yuki.HW3, "score": 60}
	rejected := c.MustCall(c.Grader, "grade.submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 60}, "to-reject")
	c.MustCall(c.Sato, "action.decide", m{"course_id": c.Course, "action_id": rejected.ActionID, "decision": "reject"}, "reject")
	toCancel := m{"submission_id": yuki.HW3, "score": 70}
	c.MustCall(c.Grader, "grade.submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 70}, "to-cancel")
	c.MustCall(c.Sato, "member.remove", m{"course_id": c.Course, "member_id": c.GraderM}, "rm-grader")
	c.MustCall(c.Sato, "grade.submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": -1}, "refused")
	c.Exec(`UPDATE assignment SET published_at = NULL WHERE id = $1`, c.HW4)

	cases := []struct {
		name         string
		method, path string
		token        string
		body         any
		headers      []string
		status       int
		code         string
		recorded     bool
	}{
		{"no credential", "GET", "/v1/me", "", nil, nil, 401, "unauthenticated", false},
		{"bad credential", "GET", "/v1/me", "ais_aaaaaaaaaaaa_" + strings.Repeat("A", 43), nil, nil, 401, "unauthenticated", false},
		{"write without a key", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1}, nil, 400, "invalid_argument", false},
		{"schema violation", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3}, key("a"), 400, "invalid_argument", false},
		{"unknown field", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1, "x": 1}, key("b"), 400, "invalid_argument", false},
		{"body is not JSON", "POST", course + "/grades", sato, "not-an-object", key("c"), 400, "invalid_argument", false},
		{"path and body disagree", "POST", course + "/grades", sato, m{"course_id": uuid.New(), "submission_id": yuki.HW3, "score": 1}, key("d"), 400, "invalid_argument", false},
		{"unknown course", "POST", "/v1/courses/" + uuid.NewString() + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1}, key("e"), 404, "not_found", false},
		{"unknown submission", "POST", course + "/grades", sato, m{"submission_id": uuid.New(), "score": 1}, key("f"), 404, "not_found", false},
		{"a rule of the domain", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1000}, key("g"), 422, "failed_precondition", true},
		{"unknown tool by name", "POST", "/v1/tools/grade.obliterate", sato, m{}, key("h"), 404, "not_found", false},
		{"bad query parameter", "GET", course + "/actions/proposed?limit=lots", sato, nil, nil, 400, "invalid_argument", false},
		{"unknown query parameter", "GET", course + "/actions/proposed?sudo=1", sato, nil, nil, 400, "invalid_argument", false},
		{"a page on another site", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1}, append(key("m"), "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"), 403, "forbidden", false},
		{"a method the route does not take", "DELETE", course + "/grades", sato, nil, nil, 405, "method_not_allowed", false},
		{"denied", "POST", course + "/grades", student, m{"submission_id": yuki.HW3, "score": 100}, key("i"), 403, "forbidden", true},
		{"an argument the tool refuses", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3, "score": -1}, key("j"), 400, "invalid_argument", true},
		{"a key used for another call", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1}, key("refused"), 409, "idempotency_conflict", false},
		{"something the tool does not find", "POST", course + "/submissions", student, m{"assignment_id": c.HW4}, key("k"), 404, "not_found", true},
		{"something the tool forbids", "POST", course + "/actions/" + own.ActionID.String() + "/decide", a.tokenFor(ta), m{"decision": "approve"}, key("l"), 403, "forbidden", true},
		{"a rejected proposal, replayed", "POST", course + "/grades", grader, toReject, key("to-reject"), 409, "", true},
		{"a cancelled proposal, replayed", "POST", course + "/grades", grader, toCancel, key("to-cancel"), 422, "failed_precondition", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := c.Count(`SELECT count(*) FROM action`)
			r := a.do(nil, tc.method, tc.path, tc.token, tc.body, tc.headers...)
			code := r.str("error", "code")
			if r.Status != tc.status || code != tc.code {
				t.Fatalf("%d %q, want %d %q\n%s", r.Status, code, tc.status, tc.code, r.Raw)
			}
			if tc.status == 401 && r.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("401 without WWW-Authenticate")
			}
			// The top-level action_id is what says the call is on record.
			id := r.str("action_id")
			switch {
			case tc.recorded && (id == "" || c.Count(`SELECT count(*) FROM action WHERE id = $1`, id) != 1):
				t.Fatalf("recorded, and the answer names no action on record\n%s", r.Raw)
			case !tc.recorded && id != "":
				t.Fatalf("not recorded, and the answer names action %s\n%s", id, r.Raw)
			case !tc.recorded && c.Count(`SELECT count(*) FROM action`) != before:
				t.Fatalf("not recorded, and something was\n%s", r.Raw)
			}
			// A key used for another call names that call's action instead.
			if code == "idempotency_conflict" && r.str("error", "details", "action_id") == "" {
				t.Fatalf("the conflict does not name the earlier action\n%s", r.Raw)
			}
		})
	}
	// The same call by name and by route is the same call.
	byName := a.do(nil, "POST", "/v1/tools/grade.submit", sato, m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 70}, "Idempotency-Key", "same")
	byRoute := a.do(nil, "POST", course+"/grades", sato, m{"submission_id": yuki.HW3, "score": 70}, "Idempotency-Key", "same")
	if byName.Status != 200 || byRoute.Status != 200 || byRoute.Header.Get("Idempotency-Replayed") != "true" || byName.str("action_id") != byRoute.str("action_id") {
		t.Fatalf("by name: %d %s\nby route: %d %s", byName.Status, byName.Raw, byRoute.Status, byRoute.Raw)
	}
}

func TestCatalogueIsPublishedAndEveryRouteIsRegistered(t *testing.T) {
	a := newAPI(t, 0)
	r := a.do(nil, "GET", "/v1/tools", "", nil)
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Raw)
	}
	listed := r.Body["tools"].([]any)
	if len(listed) != len(a.c.P.Registry().Exposed()) {
		t.Fatalf("catalogue lists %d tools, the registry exposes %d", len(listed), len(a.c.P.Registry().Exposed()))
	}
	for _, it := range listed {
		tool := it.(map[string]any)
		if tool["input_schema"] == nil || tool["description"] == "" || tool["path"] == "" {
			t.Errorf("incomplete entry: %v", tool["name"])
		}
	}
}

func TestBrowserSession(t *testing.T) {
	a := newAPI(t, 0)
	ctx := context.Background()
	c := a.c
	c.Exec(`UPDATE actor SET email = 'sato@example.edu' WHERE id = $1`, c.Sato)
	if err := auth.SetPassword(ctx, dbq.New(c.Pool), c.Sato, "a long enough password", time.Now()); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}

	if r := a.do(browser, "POST", "/v1/auth/login", "", m{"email": "sato@example.edu", "password": "wrong password!"}); r.Status != 401 {
		t.Fatalf("wrong password: %d %s", r.Status, r.Raw)
	}
	login := a.do(browser, "POST", "/v1/auth/login", "", m{"email": "sato@example.edu", "password": "a long enough password"})
	if login.Status != 200 || login.str("actor_id") != c.Sato.String() {
		t.Fatalf("login: %d %s", login.Status, login.Raw)
	}
	cookie := login.Header.Get("Set-Cookie")
	if !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Lax") || strings.Contains(login.Raw, "ais_") {
		t.Fatalf("session cookie: %q; body: %s", cookie, login.Raw)
	}
	// The cookie alone authenticates.
	if me := a.do(browser, "GET", "/v1/me", "", nil); me.Status != 200 || me.str("result", "display_name") != "Sato" {
		t.Fatalf("me with cookie: %d %s", me.Status, me.Raw)
	}
	// A hostile page cannot ride the cookie: a cross-origin POST is refused
	// before it reaches anything.
	evil := a.do(browser, "POST", "/v1/me/agents", "", m{"display_name": "stolen", "hosting": "mcp"},
		"Idempotency-Key", "x", "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site")
	if evil.Status != http.StatusForbidden {
		t.Fatalf("cross-origin POST with the session cookie: %d %s", evil.Status, evil.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM actor WHERE display_name = 'stolen'`); n != 0 {
		t.Fatal("the cross-origin request went through")
	}
	// The front end's own origin is allowed, and gets CORS headers.
	bot := a.do(browser, "POST", "/v1/me/agents", "", m{"display_name": "Sato's helper", "hosting": "mcp"},
		"Idempotency-Key", "bot", "Origin", frontEnd, "Sec-Fetch-Site", "cross-site")
	if bot.Status != 200 || bot.Header.Get("Access-Control-Allow-Origin") != frontEnd || bot.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("trusted origin: %d %v %s", bot.Status, bot.Header, bot.Raw)
	}
	// A person is issued no API token of their own, signed in or not: their
	// agent is, for their tools and scripts.
	mine := a.do(browser, "POST", "/v1/me/credentials/tokens", "", m{"label": "laptop"}, "Idempotency-Key", "mine", "Origin", frontEnd)
	if mine.Status != http.StatusForbidden || mine.str("error", "details", "reason") != "api_tokens_are_for_agents" {
		t.Fatalf("a person's own token: %d %s", mine.Status, mine.Raw)
	}
	ours := a.do(browser, "POST", "/v1/me/agents/"+bot.str("result", "actor_id")+"/tokens", "", m{"label": "laptop"},
		"Idempotency-Key", "y", "Origin", frontEnd, "Sec-Fetch-Site", "cross-site")
	if ours.Status != 200 {
		t.Fatalf("a token for Sato's agent: %d %s", ours.Status, ours.Raw)
	}
	// The token is in the response once, and never in the log.
	issued := ours.str("result", "token")
	if !strings.HasPrefix(issued, "ais_") {
		t.Fatalf("no token in the response: %s", ours.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE result::text LIKE '%' || $1 || '%'`, issued[17:]); n != 0 {
		t.Fatal("the issued token reached the action log")
	}
	replay := a.do(browser, "POST", "/v1/me/agents/"+bot.str("result", "actor_id")+"/tokens", "", m{"label": "laptop"},
		"Idempotency-Key", "y", "Origin", frontEnd)
	if replay.Status != 200 || replay.str("result", "token") != "" || replay.str("result", "credential_id") != ours.str("result", "credential_id") {
		t.Fatalf("replayed agent.issue_token: %s", replay.Raw)
	}
	if me := a.do(nil, "GET", "/v1/me", issued, nil); me.Status != 200 || me.str("result", "kind") != "agent" {
		t.Fatalf("the issued token does not work: %d %s", me.Status, me.Raw)
	}
	pre := a.do(browser, "OPTIONS", "/v1/me", "", nil, "Origin", frontEnd, "Access-Control-Request-Method", "GET")
	if pre.Status != http.StatusNoContent || !strings.Contains(pre.Header.Get("Access-Control-Allow-Headers"), "Idempotency-Key") {
		t.Fatalf("preflight: %d %v", pre.Status, pre.Header)
	}

	// Logging out ends the session on the server, not just in the browser.
	var session string
	for _, ck := range jar.Cookies(mustURL(t, a.srv.URL)) {
		if ck.Name == httpapi.SessionCookie {
			session = ck.Value
		}
	}
	if out := a.do(browser, "POST", "/v1/auth/logout", "", nil, "Origin", frontEnd); out.Status != http.StatusNoContent {
		t.Fatalf("logout: %d %s", out.Status, out.Raw)
	}
	if me := a.do(nil, "GET", "/v1/me", session, nil); me.Status != 401 {
		t.Fatalf("the old session still works after logout: %d", me.Status)
	}
}

// docs/schema.md §2.10 over REST: root builds a tree and appoints an
// administrator in it, who makes a department beneath the appointment,
// staffs it, renames and moves it, and is refused what is not theirs, until
// the appointment ends.
func TestATreeIsBuiltAndStaffedOverHTTP(t *testing.T) {
	a := newAPI(t, 1)
	c := a.c
	root, sato := a.tokenFor(c.Root), a.tokenFor(c.Sato)
	yuki := c.Students[0].Actor
	post := func(token, path string, body m, key string, want int) response {
		t.Helper()
		res := a.do(nil, "POST", path, token, body, "Idempotency-Key", key)
		if res.Status != want {
			t.Fatalf("POST %s: %d %s, want %d", path, res.Status, res.Raw, want)
		}
		return res
	}

	eng := post(root, "/v1/departments", m{"name": "Engineering"}, "eng", 200).str("result", "id")
	sw := post(root, "/v1/departments", m{"name": "Software", "parent_id": eng}, "sw", 200).str("result", "id")
	post(root, "/v1/departments/"+sw+"/admins", m{"actor_id": c.Sato}, "sato-sw", 200)
	if admins := a.do(nil, "GET", "/v1/departments/"+sw+"/admins", root, nil); admins.Status != 200 ||
		!strings.Contains(admins.Raw, `"display_name":"Sato"`) || !strings.Contains(admins.Raw, `"appointed_by_name":"root"`) {
		t.Fatalf("department.list_admins: %d %s", admins.Status, admins.Raw)
	}
	if me := a.do(nil, "GET", "/v1/me", sato, nil); me.Status != 200 || !strings.Contains(me.Raw, `"administers":[{"dept_id":"`+sw+`","name":"Software"`) {
		t.Fatalf("me.get: %d %s", me.Status, me.Raw)
	}

	// Beneath the appointment Sato makes departments and staffs them.
	ai := post(sato, "/v1/departments", m{"name": "AI", "parent_id": sw}, "ai", 200).str("result", "id")
	post(sato, "/v1/departments/"+ai+"/admins", m{"actor_id": yuki}, "yuki-ai", 200)
	tree := a.do(nil, "GET", "/v1/departments/tree?root_id="+eng, sato, nil)
	if tree.Status != 200 || !strings.Contains(tree.Raw, `"id":"`+ai+`","name":"AI","parent_id":"`+sw+`","depth":3,"administers":true,"manages":true`) ||
		!strings.Contains(tree.Raw, `"id":"`+sw+`","name":"Software","parent_id":"`+eng+`","depth":2,"administers":true,"manages":false,"appointed":true`) {
		t.Fatalf("department.list_tree: %d %s", tree.Status, tree.Raw)
	}
	// He renames and moves what is beneath his appointment, and only to where it is his as well.
	if renamed := post(sato, "/v1/departments/"+ai, m{"name": "Machine Learning"}, "rename-ai", 200); renamed.str("result", "name") != "Machine Learning" {
		t.Fatalf("department.update: %s", renamed.Raw)
	}
	vision := post(sato, "/v1/departments", m{"name": "Vision", "parent_id": sw}, "vision-1", 200).str("result", "id")
	post(sato, "/v1/departments/"+ai+"/move", m{"parent_id": vision}, "move-ai", 200)
	if top := post(sato, "/v1/departments/"+ai+"/move", m{"parent_id": nil}, "ai-to-top", 403); top.str("error", "details", "reason") != "destination_out_of_scope" {
		t.Fatalf("to the top: %s", top.Raw)
	}
	if away := post(sato, "/v1/departments/"+ai+"/move", m{"parent_id": eng}, "ai-to-eng", 403); away.str("error", "details", "reason") != "destination_out_of_scope" {
		t.Fatalf("out of his reach: %s", away.Raw)
	}
	// Not his own department's staff, not at the top, and nothing of a platform administrator's.
	if own := post(sato, "/v1/departments/"+sw+"/admins", m{"actor_id": yuki}, "yuki-sw", 403); own.str("error", "details", "reason") != "department_out_of_scope" || own.str("action_id") == "" {
		t.Fatalf("his own department's staff: %s", own.Raw)
	}
	if top := post(sato, "/v1/departments", m{"name": "Law"}, "law", 403); top.str("error", "details", "reason") != "platform_role_required" {
		t.Fatalf("at the top: %s", top.Raw)
	}
	if actors := a.do(nil, "GET", "/v1/actors", sato, nil); actors.Status != 403 || actors.str("error", "details", "reason") != "platform_role_required" {
		t.Fatalf("the directory: %d %s", actors.Status, actors.Raw)
	}

	post(root, "/v1/departments/"+sw+"/admins/"+c.Sato.String()+"/remove", m{}, "end-sato", 200)
	if late := post(sato, "/v1/departments", m{"name": "Vision", "parent_id": ai}, "vision", 403); late.str("error", "details", "reason") != "platform_role_required" {
		t.Fatalf("after the appointment ended: %s", late.Raw)
	}
}

// docs/schema.md §2.10 over REST, for courses and people: root invites a
// new person and appoints her in the tree; she makes a course beneath her
// appointment, finds its instructor by their whole email, registers and
// invites another, seats both, moves the course, and is refused the
// directory and whatever lies outside her appointment.
func TestADepartmentAdministratorManagesCoursesOverHTTP(t *testing.T) {
	a := newAPI(t, 0)
	c := a.c
	root := a.tokenFor(c.Root)
	post := func(token, path string, body m, key string, want int) response {
		t.Helper()
		res := a.do(nil, "POST", path, token, body, "Idempotency-Key", key)
		if res.Status != want {
			t.Fatalf("POST %s: %d %s, want %d", path, res.Status, res.Raw, want)
		}
		return res
	}
	eng := post(root, "/v1/departments", m{"name": "Engineering"}, "eng", 200).str("result", "id")
	law := post(root, "/v1/departments", m{"name": "Law"}, "law", 200).str("result", "id")
	invited := post(root, "/v1/actor-invitations", m{"display_name": "Ada", "email": "ada@example.edu"}, "ada", 200)
	ada := invited.str("result", "actor_id")
	acc := a.do(nil, "POST", "/v1/auth/invite", "", m{"token": invited.str("result", "token"), "password": "adas own password"})
	var session string
	for _, ck := range (&http.Response{Header: acc.Header}).Cookies() {
		if ck.Name == httpapi.SessionCookie {
			session = ck.Value
		}
	}
	if acc.Status != 200 || acc.str("actor_id") != ada || session == "" {
		t.Fatalf("Ada takes up root's invitation: %d %s", acc.Status, acc.Raw)
	}
	post(root, "/v1/departments/"+eng+"/admins", m{"actor_id": ada}, "ada-eng", 200)

	design := post(session, "/v1/departments", m{"name": "Design", "parent_id": eng}, "design", 200).str("result", "id")
	course := post(session, "/v1/courses", m{"dept_id": design, "term_id": c.Term, "code": "DES101", "title": "Drawing"},
		"des101", 200).str("result", "course_id")
	cp := "/v1/courses/" + course

	mori := post(root, "/v1/actors", m{"kind": "human", "display_name": "Mori", "email": "mori@example.edu"}, "mori", 200).str("result", "actor_id")
	found := a.do(nil, "GET", "/v1/actor-lookup?email=MORI%40Example.edu", session, nil)
	if found.Status != 200 || found.str("result", "actor_id") != mori || strings.Contains(found.Raw, "example.edu") {
		t.Fatalf("actor.lookup_by_email: %d %s", found.Status, found.Raw)
	}
	if partial := a.do(nil, "GET", "/v1/actor-lookup?email=mori", session, nil); partial.Status != 404 {
		t.Fatalf("a partial email: %d %s", partial.Status, partial.Raw)
	}
	post(session, cp+"/instructors", m{"actor_id": mori}, "seat-mori", 200)
	noor := post(session, "/v1/actor-invitations", m{"display_name": "Noor", "email": "noor@example.edu"}, "noor", 200)
	if !strings.HasPrefix(noor.str("result", "token"), "aisinv_") {
		t.Fatalf("actor.invite_new: %s", noor.Raw)
	}
	post(session, cp+"/instructors", m{"actor_id": noor.str("result", "actor_id")}, "seat-noor", 200)
	if taken := post(session, "/v1/actor-invitations", m{"display_name": "Mori", "email": "mori@example.edu"}, "mori-again", 409); taken.str("error", "details", "actor_id") != mori {
		t.Fatalf("an email already registered: %s", taken.Raw)
	}

	post(session, cp+"/move", m{"dept_id": eng}, "to-eng", 200)
	if away := post(session, cp+"/move", m{"dept_id": law}, "to-law", 403); away.str("error", "details", "reason") != "destination_out_of_scope" {
		t.Fatalf("out of her reach: %s", away.Raw)
	}
	list := a.do(nil, "GET", "/v1/courses?within_dept_id="+eng, session, nil)
	if list.Status != 200 || !strings.Contains(list.Raw, course) || strings.Contains(list.Raw, c.Course.String()) {
		t.Fatalf("course.list: %d %s", list.Status, list.Raw)
	}
	// CS101, in a department she does not administer, and the directory are not hers.
	if other := post(session, "/v1/courses/"+c.Course.String(), m{"title": "Mine"}, "cs101", 403); other.str("error", "details", "reason") != "department_out_of_scope" {
		t.Fatalf("another department's course: %s", other.Raw)
	}
	if inside := a.do(nil, "GET", cp, session, nil); inside.Status != 403 || inside.str("error", "details", "reason") != "not_a_member" {
		t.Fatalf("inside her own department's course, unseated: %d %s", inside.Status, inside.Raw)
	}
	if actors := a.do(nil, "GET", "/v1/actors", session, nil); actors.Status != 403 || actors.str("error", "details", "reason") != "platform_role_required" {
		t.Fatalf("the directory: %d %s", actors.Status, actors.Raw)
	}
}

// A person an administrator registered chooses their password through an
// invitation, in the browser, and is signed in.
func TestInvitationOverHTTP(t *testing.T) {
	a := newAPI(t, 0)
	c := a.c
	root := a.tokenFor(c.Root)
	reg := a.do(nil, "POST", "/v1/actors", root, m{"kind": "human", "display_name": "Mori", "email": "mori@example.edu"}, "Idempotency-Key", "reg")
	mori := reg.str("result", "actor_id")
	if reg.Status != 200 || mori == "" {
		t.Fatalf("register: %d %s", reg.Status, reg.Raw)
	}
	// Found again by a search, with no password yet.
	found := a.do(nil, "GET", "/v1/actors?search=MORI", root, nil)
	if found.Status != 200 || !strings.Contains(found.Raw, mori) || !strings.Contains(found.Raw, `"has_password":false`) {
		t.Fatalf("actor.list: %d %s", found.Status, found.Raw)
	}
	inv := a.do(nil, "POST", "/v1/actors/"+mori+"/invite", root, m{}, "Idempotency-Key", "inv")
	token := inv.str("result", "token")
	if inv.Status != 200 || !strings.HasPrefix(token, "aisinv_") || inv.str("result", "email") != "mori@example.edu" {
		t.Fatalf("invite: %d %s", inv.Status, inv.Raw)
	}
	if replay := a.do(nil, "POST", "/v1/actors/"+mori+"/invite", root, m{}, "Idempotency-Key", "inv"); replay.Status != 200 || replay.str("result", "token") != "" {
		t.Fatalf("a replayed invitation carries its token: %s", replay.Raw)
	}
	// It is no bearer token.
	if me := a.do(nil, "GET", "/v1/me", token, nil); me.Status != 401 {
		t.Fatalf("the invitation as a bearer token: %d", me.Status)
	}

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	// A hostile page cannot take it up on the person's behalf.
	evil := a.do(browser, "POST", "/v1/auth/invite", "", m{"token": token, "password": "chosen by someone else"},
		"Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site")
	if evil.Status != http.StatusForbidden {
		t.Fatalf("cross-origin: %d %s", evil.Status, evil.Raw)
	}
	if weak := a.do(browser, "POST", "/v1/auth/invite", "", m{"token": token, "password": "short"}, "Origin", frontEnd); weak.Status != 400 {
		t.Fatalf("weak password: %d %s", weak.Status, weak.Raw)
	}
	acc := a.do(browser, "POST", "/v1/auth/invite", "", m{"token": token, "password": "moris own password"}, "Origin", frontEnd)
	if acc.Status != 200 || acc.str("actor_id") != mori || acc.str("email") != "mori@example.edu" ||
		!strings.Contains(acc.Header.Get("Set-Cookie"), "HttpOnly") || strings.Contains(acc.Raw, "ais_") {
		t.Fatalf("accept: %d %v %s", acc.Status, acc.Header, acc.Raw)
	}
	if me := a.do(browser, "GET", "/v1/me", "", nil); me.Status != 200 || me.str("result", "display_name") != "Mori" {
		t.Fatalf("me after accepting: %d %s", me.Status, me.Raw)
	}
	if again := a.do(nil, "POST", "/v1/auth/invite", "", m{"token": token, "password": "moris own password"}); again.Status != 401 {
		t.Fatalf("used twice: %d %s", again.Status, again.Raw)
	}
	if login := a.do(nil, "POST", "/v1/auth/login", "", m{"email": "MORI@example.edu", "password": "moris own password"}); login.Status != 200 {
		t.Fatalf("login with the chosen password: %d %s", login.Status, login.Raw)
	}
	if got := a.do(nil, "GET", "/v1/actors/"+mori, root, nil); !strings.Contains(got.Raw, `"has_password":true`) || strings.Contains(got.Raw, "invite_expires_at") {
		t.Fatalf("actor.get after: %s", got.Raw)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// raw sends a request whose body is not JSON, and returns the response as is.
func (a *api) raw(method, url, contentType string, body []byte, headers ...string) (rawResponse, []byte) {
	a.t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		a.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	return rawResponse{StatusCode: res.StatusCode, Header: res.Header}, got
}

// rawResponse is what a test needs of a response once its body has been read
// and closed.
type rawResponse struct {
	StatusCode int
	Header     http.Header
}

// here rewrites a store URL, which names the public host, to the test server.
func (a *api) here(storeURL string) string {
	return a.srv.URL + strings.TrimPrefix(storeURL, "http://lms.test")
}

// A file, over HTTP all the way: ask for a URL, PUT the bytes with no
// credential but the URL, attach it, read it back as a student.
func TestFileUploadOverHTTP(t *testing.T) {
	a := newAPI(t, 1)
	c := a.c
	sato, yuki := a.tokenFor(c.Sato), a.tokenFor(c.Students[0].Actor)
	course := "/v1/courses/" + c.Course.String()

	ask := a.do(nil, "GET", course+"/upload-url?kind=material&content_type=application/pdf", sato, nil)
	if ask.Status != 200 {
		t.Fatalf("upload-url: %d %s", ask.Status, ask.Raw)
	}
	putURL, token := a.here(ask.str("result", "upload_url")), ask.str("result", "upload_token")
	pdf := []byte("%PDF-1.7 slides")

	// The slot was signed for a PDF. It does not take HTML.
	if res, body := a.raw("PUT", putURL, "text/html", []byte("<script>alert(1)</script>")); res.StatusCode != 400 {
		t.Fatalf("wrong content type: %d %s", res.StatusCode, body)
	}
	if res, body := a.raw("PUT", putURL, "application/pdf", pdf); res.StatusCode != 200 || !strings.Contains(string(body), "sha256:") {
		t.Fatalf("PUT: %d %s", res.StatusCode, body)
	}
	// Written once. What a version points at cannot be swapped.
	if res, _ := a.raw("PUT", putURL, "application/pdf", []byte("other bytes")); res.StatusCode != 409 {
		t.Fatalf("a second PUT: %d", res.StatusCode)
	}
	if res, _ := a.raw("PUT", testkit.Forged(t, putURL), "application/pdf", pdf); res.StatusCode != 403 {
		t.Fatalf("a forged upload URL: %d", res.StatusCode)
	}
	if res, _ := a.raw("GET", putURL, "", nil); res.StatusCode != 403 { // a PUT URL is not a GET URL
		t.Fatalf("GET on a PUT URL: %d", res.StatusCode)
	}

	made := a.do(nil, "POST", course+"/documents", sato, m{"kind": "material", "title": "Slides",
		"files": []m{{"upload_token": token, "filename": "Slides.pdf"}}}, "Idempotency-Key", "doc-1")
	if made.Status != 200 {
		t.Fatalf("document.create: %d %s", made.Status, made.Raw)
	}
	doc := made.str("result", "document_id")
	if r := a.do(nil, "POST", course+"/documents/"+doc+"/publish", sato, m{}, "Idempotency-Key", "pub-1"); r.Status != 200 {
		t.Fatalf("publish: %d %s", r.Status, r.Raw)
	}

	read := a.do(nil, "GET", course+"/documents/"+doc, yuki, nil)
	if read.Status != 200 {
		t.Fatalf("document.get: %d %s", read.Status, read.Raw)
	}
	first, _ := read.Body["result"].(map[string]any)["version"].(map[string]any)["files"].([]any)
	if len(first) != 1 {
		t.Fatalf("document.get: %s", read.Raw)
	}
	res, got := a.raw("GET", a.here(first[0].(map[string]any)["download_url"].(string)), "", nil)
	if res.StatusCode != 200 || !bytes.Equal(got, pdf) || res.Header.Get("Content-Type") != "application/pdf" {
		t.Fatalf("download: %d %q %s", res.StatusCode, res.Header.Get("Content-Type"), got)
	}
	// Other people's bytes, from the API's own origin: a download, never a page.
	if res.Header.Get("Content-Disposition") != "attachment; filename=Slides.pdf" || res.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("download headers: %v", res.Header)
	}

	// Too large is refused at the door, and nothing is kept.
	big := a.do(nil, "GET", course+"/upload-url?kind=material&content_type=application/zip", sato, nil)
	if res, _ := a.raw("PUT", a.here(big.str("result", "upload_url")), "application/zip", bytes.Repeat([]byte("z"), testkit.MaxUploadBytes+1)); res.StatusCode != 400 {
		t.Fatalf("oversized PUT: %d", res.StatusCode)
	}
	attach := a.do(nil, "POST", course+"/documents", sato, m{"kind": "material", "title": "Big",
		"files": []m{{"upload_token": big.str("result", "upload_token"), "filename": "big.zip"}}}, "Idempotency-Key", "doc-2")
	if attach.Status != 422 {
		t.Fatalf("attaching an upload that was refused: %d %s", attach.Status, attach.Raw)
	}
}
