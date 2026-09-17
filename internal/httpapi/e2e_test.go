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

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/httpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
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
	}))
	t.Cleanup(srv.Close)
	return &api{t: t, c: c, srv: srv}
}

// tokenFor issues an API token the way the operator's command line does.
func (a *api) tokenFor(actor uuid.UUID) string {
	a.t.Helper()
	tok, _, err := auth.IssueToken(context.Background(), dbq.New(a.c.Pool), actor, "test", nil, time.Now())
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
	if len(raw) > 0 {
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

func TestStatusCodes(t *testing.T) {
	a := newAPI(t, 1)
	c, yuki := a.c, a.c.Students[0]
	sato := a.tokenFor(c.Sato)
	course := "/v1/courses/" + c.Course.String()
	key := func(k string) []string { return []string{"Idempotency-Key", k} }

	cases := []struct {
		name         string
		method, path string
		token        string
		body         any
		headers      []string
		status       int
		code         string
	}{
		{"no credential", "GET", "/v1/me", "", nil, nil, 401, "unauthenticated"},
		{"bad credential", "GET", "/v1/me", "ais_aaaaaaaaaaaa_" + strings.Repeat("A", 43), nil, nil, 401, "unauthenticated"},
		{"write without a key", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1}, nil, 400, "invalid_argument"},
		{"schema violation", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3}, key("a"), 400, "invalid_argument"},
		{"unknown field", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1, "x": 1}, key("b"), 400, "invalid_argument"},
		{"body is not JSON", "POST", course + "/grades", sato, "not-an-object", key("c"), 400, "invalid_argument"},
		{"path and body disagree", "POST", course + "/grades", sato, m{"course_id": uuid.New(), "submission_id": yuki.HW3, "score": 1}, key("d"), 400, "invalid_argument"},
		{"unknown course", "POST", "/v1/courses/" + uuid.NewString() + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1}, key("e"), 404, "not_found"},
		{"unknown submission", "POST", course + "/grades", sato, m{"submission_id": uuid.New(), "score": 1}, key("f"), 404, "not_found"},
		{"a rule of the domain", "POST", course + "/grades", sato, m{"submission_id": yuki.HW3, "score": 1000}, key("g"), 422, "failed_precondition"},
		{"unknown tool by name", "POST", "/v1/tools/grade.obliterate", sato, m{}, key("h"), 404, "not_found"},
		{"bad query parameter", "GET", course + "/actions/proposed?limit=lots", sato, nil, nil, 400, "invalid_argument"},
		{"unknown query parameter", "GET", course + "/actions/proposed?sudo=1", sato, nil, nil, 400, "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := a.do(nil, tc.method, tc.path, tc.token, tc.body, tc.headers...)
			code := r.str("error", "code")
			if r.Status != tc.status || code != tc.code {
				t.Fatalf("%d %q, want %d %q\n%s", r.Status, code, tc.status, tc.code, r.Raw)
			}
			if tc.status == 401 && r.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("401 without WWW-Authenticate")
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
	evil := a.do(browser, "POST", "/v1/me/credentials/tokens", "", m{"label": "stolen"},
		"Idempotency-Key", "x", "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site")
	if evil.Status != http.StatusForbidden {
		t.Fatalf("cross-origin POST with the session cookie: %d %s", evil.Status, evil.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM credential WHERE label = 'stolen'`); n != 0 {
		t.Fatal("the cross-origin request went through")
	}
	// The front end's own origin is allowed, and gets CORS headers.
	ours := a.do(browser, "POST", "/v1/me/credentials/tokens", "", m{"label": "laptop"},
		"Idempotency-Key", "y", "Origin", frontEnd, "Sec-Fetch-Site", "cross-site")
	if ours.Status != 200 || ours.Header.Get("Access-Control-Allow-Origin") != frontEnd || ours.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("trusted origin: %d %v %s", ours.Status, ours.Header, ours.Raw)
	}
	// The token is in the response once, and never in the log.
	issued := ours.str("result", "token")
	if !strings.HasPrefix(issued, "ais_") {
		t.Fatalf("no token in the response: %s", ours.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM action WHERE result::text LIKE '%' || $1 || '%'`, issued[17:]); n != 0 {
		t.Fatal("the issued token reached the action log")
	}
	replay := a.do(browser, "POST", "/v1/me/credentials/tokens", "", m{"label": "laptop"}, "Idempotency-Key", "y", "Origin", frontEnd)
	if replay.Status != 200 || replay.str("result", "token") != "" || replay.str("result", "credential_id") != ours.str("result", "credential_id") {
		t.Fatalf("replayed issue_token: %s", replay.Raw)
	}
	if me := a.do(nil, "GET", "/v1/me", issued, nil); me.Status != 200 {
		t.Fatalf("the issued token does not work: %d", me.Status)
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

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
