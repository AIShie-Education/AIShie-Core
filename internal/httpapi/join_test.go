package httpapi_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
)

// The join endpoints, over HTTP: the page that opens a join link asks what
// it may show, a person signed in joins through it, and someone with no
// account registers through it and is signed in.

// joinAPI is the API as serve builds it, for a browser on the front end,
// with the limits given.
func joinAPI(t *testing.T, signIns, registrations *ratelimit.Limiter, log *slog.Logger) *api {
	t.Helper()
	return hardenedWith(t, nil, signIns, log, func(d *httpapi.Deps) {
		d.TrustedOrigins, d.InsecureCookies, d.Registrations = []string{frontEnd}, true, registrations
	})
}

// link makes a join link to CS101 as Sato, over REST, and returns its token
// and id.
func (a *api) link(body m) (token, id string) {
	a.t.Helper()
	res := a.do(nil, "POST", "/v1/courses/"+a.c.Course.String()+"/join-links", a.tokenFor(a.c.Sato), body, "Idempotency-Key", "link-"+uuid.NewString())
	if res.Status != 200 || !strings.HasPrefix(res.str("result", "token"), "aisjoin_") {
		a.t.Fatalf("course.join_link_create: %d %s", res.Status, res.Raw)
	}
	return res.str("result", "token"), res.str("result", "link_id")
}

func (a *api) register(client *http.Client, token string, body m, headers ...string) response {
	a.t.Helper()
	return a.do(client, "POST", httpapi.JoinPath+token+"/register", "", body, append([]string{"Origin", frontEnd}, headers...)...)
}

func keysOf(v map[string]any) string {
	var k []string
	for key := range v {
		k = append(k, key)
	}
	sort.Strings(k)
	return strings.Join(k, ",")
}

func TestAJoinLinkOverHTTP(t *testing.T) {
	var log bytes.Buffer
	a := joinAPI(t, nil, nil, slog.New(slog.NewJSONHandler(&log, nil)))
	c := a.c
	token, linkID := a.link(m{"allowed_email_domains": []string{"example.edu"}})

	// The management tools are tools like any other; joining is not one.
	tools := a.do(nil, "GET", "/v1/tools", "", nil)
	for _, want := range []string{"course.join_link_create", "course.join_link_list", "course.join_link_revoke"} {
		if !strings.Contains(tools.Raw, `"name":"`+want+`"`) {
			t.Fatalf("GET /v1/tools does not list %s", want)
		}
	}
	if strings.Contains(tools.Raw, `"name":"course.join"`) {
		t.Fatal("GET /v1/tools lists course.join")
	}
	if r := a.do(nil, "POST", "/v1/tools/course.join", a.tokenFor(c.Sato), m{"token": token}, "Idempotency-Key", "by-name"); r.Status != 404 {
		t.Fatalf("course.join by name: %d %s", r.Status, r.Raw)
	}

	// What the page may show, to anyone: the course, and whether it may be
	// joined, and by registering; nothing else.
	pv := a.do(nil, "GET", httpapi.JoinPath+token, "", nil)
	if pv.Status != 200 || keysOf(pv.Body) != "allowed_email_domains,course,email_required,expires_at,joinable,registration" ||
		pv.Body["email_required"] != true ||
		keysOf(pv.Body["course"].(map[string]any)) != "code,section,title" || pv.Body["joinable"] != true || pv.Body["registration"] != true ||
		pv.str("course", "code") != "CS101" || pv.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("preview: %d %v %s", pv.Status, pv.Header, pv.Raw)
	}
	// A token that finds no link: one 404, however it is wrong.
	var missing []string
	for _, bad := range []string{"nothing", token + "x", token[:len(token)-2] + "zz", "aisjoin_abcdefghijkl_" + strings.Repeat("A", 43)} {
		r := a.do(nil, "GET", httpapi.JoinPath+bad, "", nil)
		if r.Status != 404 {
			t.Fatalf("preview of %q: %d %s", bad, r.Status, r.Raw)
		}
		missing = append(missing, r.Raw)
		if reg := a.register(nil, bad, m{"display_name": "X", "email": "x@example.edu", "password": "a long enough password"}); reg.Status != 404 || reg.Raw != r.Raw {
			t.Fatalf("registering through %q: %d %s", bad, reg.Status, reg.Raw)
		}
	}
	for _, raw := range missing {
		if raw != missing[0] {
			t.Fatalf("two not-founds differ: %s and %s", missing[0], raw)
		}
	}

	// Someone with no account registers, and is signed in, seated and
	// recorded as having joined.
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	reg := a.register(browser, token, m{"display_name": "Aoi", "email": "aoi@example.edu", "password": "aoi's own password"})
	if reg.Status != 200 || keysOf(reg.Body) != "action_id,actor_id,course_id,expires_at,member_id" || reg.str("course_id") != c.Course.String() {
		t.Fatalf("register: %d %s", reg.Status, reg.Raw)
	}
	if cookie := reg.Header.Get("Set-Cookie"); !strings.HasPrefix(cookie, httpapi.SessionCookie+"=") || !strings.Contains(cookie, "HttpOnly") {
		t.Fatalf("register set no session: %q", cookie)
	}
	me := a.do(browser, "GET", "/v1/me", "", nil)
	if me.Status != 200 || me.str("result", "id") != reg.str("actor_id") || me.Body["result"].(map[string]any)["email_verified"] != false {
		t.Fatalf("me after registering: %d %s", me.Status, me.Raw)
	}
	if mine := a.do(browser, "GET", "/v1/me/memberships", "", nil); !strings.Contains(mine.Raw, reg.str("member_id")) {
		t.Fatalf("memberships after registering: %s", mine.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM course_member WHERE id = $1 AND join_link_id = $2 AND role = 'student'`, reg.str("member_id"), linkID); n != 1 {
		t.Fatal("the seat does not name the link")
	}
	if login := a.do(nil, "POST", "/v1/auth/login", "", m{"email": "AOI@example.edu", "password": "aoi's own password"}); login.Status != 200 {
		t.Fatalf("signing in with what was registered: %d %s", login.Status, login.Raw)
	}
	// Opening the link again, signed in: the seat she has.
	again := a.do(browser, "POST", httpapi.JoinPath+token, "", nil, "Idempotency-Key", "again", "Origin", frontEnd)
	if again.Status != 200 || again.str("status") != "executed" || again.Body["result"].(map[string]any)["already_member"] != true ||
		again.str("result", "member_id") != reg.str("member_id") {
		t.Fatalf("joining again: %d %s", again.Status, again.Raw)
	}

	// Registered already: told to sign in; the account is left as it was.
	sessions := c.Count(`SELECT count(*) FROM credential WHERE actor_id = $1`, reg.str("actor_id"))
	taken := a.register(nil, token, m{"display_name": "Impostor", "email": "Aoi@Example.edu", "password": "another long password"})
	if taken.Status != 409 || taken.str("error", "details", "reason") != "email_taken" || !strings.Contains(taken.str("error", "message"), "sign in") ||
		taken.Header.Get("Set-Cookie") != "" {
		t.Fatalf("an email registered already: %d %s", taken.Status, taken.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM credential WHERE actor_id = $1`, reg.str("actor_id")); n != sessions {
		t.Fatal("refusing an email registered already touched the account that has it")
	}
	// Refused before anything is looked up: the fields.
	for name, body := range map[string]m{
		"a short password": {"display_name": "Ren", "email": "ren@example.edu", "password": "short"},
		"not an email":     {"display_name": "Ren", "email": "ren", "password": "a long enough password"},
		"no name":          {"display_name": " ", "email": "ren@example.edu", "password": "a long enough password"},
		"another field":    {"display_name": "Ren", "email": "ren@example.edu", "password": "a long enough password", "role": "instructor"},
	} {
		if r := a.register(nil, token, body); r.Status != 400 {
			t.Fatalf("%s: %d %s", name, r.Status, r.Raw)
		}
	}
	// At a domain the link does not take.
	other := a.register(nil, token, m{"display_name": "Ren", "email": "ren@other.edu", "password": "a long enough password"})
	if other.Status != 422 || other.str("error", "details", "reason") != "email_domain_not_allowed" {
		t.Fatalf("another domain: %d %s", other.Status, other.Raw)
	}
	// From a page on another site: refused before it reaches anything.
	evil := a.do(nil, "POST", httpapi.JoinPath+token+"/register", "", m{"display_name": "Ren", "email": "ren@example.edu", "password": "a long enough password"},
		"Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site")
	if evil.Status != 403 || c.Count(`SELECT count(*) FROM actor WHERE email = 'ren@example.edu'`) != 0 {
		t.Fatalf("a cross-site registration: %d %s", evil.Status, evil.Raw)
	}

	// A person signed in joins, with a key as every write takes one.
	mei := c.Actor("human", "Mei")
	c.Exec(`UPDATE actor SET email = 'mei@example.edu' WHERE id = $1`, mei)
	meiToken := a.tokenFor(mei)
	if r := a.do(nil, "POST", httpapi.JoinPath+token, meiToken, nil); r.Status != 400 {
		t.Fatalf("a join with no key: %d %s", r.Status, r.Raw)
	}
	join := a.do(nil, "POST", httpapi.JoinPath+token, meiToken, nil, "Idempotency-Key", "mei-joins")
	if join.Status != 200 || join.str("action_id") == "" || join.Body["result"].(map[string]any)["already_member"] != false {
		t.Fatalf("joining: %d %s", join.Status, join.Raw)
	}
	if r := a.do(nil, "POST", httpapi.JoinPath+token, meiToken, nil, "Idempotency-Key", "mei-joins"); r.Header.Get(httpapi.HeaderReplayed) != "true" ||
		r.str("result", "member_id") != join.str("result", "member_id") {
		t.Fatalf("a retry: %d %s", r.Status, r.Raw)
	}
	if r := a.do(nil, "POST", httpapi.JoinPath+token, "", nil, "Idempotency-Key", "nobody"); r.Status != 401 {
		t.Fatalf("a join signed in as nobody: %d %s", r.Status, r.Raw)
	}
	// An agent is refused, on the record.
	agent := a.do(nil, "POST", httpapi.JoinPath+token, a.tokenFor(c.Grader), nil, "Idempotency-Key", "agent-joins")
	if agent.Status != 422 || agent.str("action_id") == "" || agent.str("error", "details", "reason") != "people_only" {
		t.Fatalf("an agent joining: %d %s", agent.Status, agent.Raw)
	}
	// Whoever manages the course sees who joined through which link.
	course := "/v1/courses/" + c.Course.String()
	list := a.do(nil, "GET", course+"/join-links", a.tokenFor(c.Sato), nil)
	if list.Status != 200 || !strings.Contains(list.Raw, `"uses":2`) || strings.Contains(list.Raw, "aisjoin_") {
		t.Fatalf("join-links: %d %s", list.Status, list.Raw)
	}
	who := a.do(nil, "GET", course+"/members?join_link_id="+linkID, a.tokenFor(c.Sato), nil)
	if who.Status != 200 || !strings.Contains(who.Raw, reg.str("member_id")) || !strings.Contains(who.Raw, join.str("result", "member_id")) ||
		strings.Contains(who.Raw, c.Students[0].Member.String()) {
		t.Fatalf("members through the link: %d %s", who.Status, who.Raw)
	}

	// Revoked, it seats nobody, and says why.
	if r := a.do(nil, "POST", course+"/join-links/"+linkID+"/revoke", a.tokenFor(c.Sato), nil, "Idempotency-Key", "revoke"); r.Status != 200 {
		t.Fatalf("revoke: %d %s", r.Status, r.Raw)
	}
	if pv := a.do(nil, "GET", httpapi.JoinPath+token, "", nil); pv.Status != 200 || pv.Body["joinable"] != false || pv.str("reason") != "revoked" ||
		pv.Body["registration"] != false {
		t.Fatalf("preview of a revoked link: %d %s", pv.Status, pv.Raw)
	}
	if r := a.register(nil, token, m{"display_name": "Ren", "email": "ren@example.edu", "password": "a long enough password"}); r.Status != 422 ||
		r.str("error", "details", "reason") != "revoked" {
		t.Fatalf("registering through a revoked link: %d %s", r.Status, r.Raw)
	}
	late := c.Actor("human", "Late")
	c.Exec(`UPDATE actor SET email = 'late@example.edu' WHERE id = $1`, late)
	if r := a.do(nil, "POST", httpapi.JoinPath+token, a.tokenFor(late), nil, "Idempotency-Key", "late"); r.Status != 422 || r.str("error", "details", "reason") != "revoked" {
		t.Fatalf("joining through a revoked link: %d %s", r.Status, r.Raw)
	}

	// The log names the endpoints, and never the token.
	secret := strings.SplitN(token, "_", 3)[2]
	if strings.Contains(log.String(), secret) || strings.Contains(log.String(), strings.SplitN(token, "_", 3)[1]) {
		t.Fatalf("the token is in the log:\n%s", log.String())
	}
	if !strings.Contains(log.String(), `"path":"/v1/join/…/register"`) || !strings.Contains(log.String(), `"path":"/v1/join/…"`) {
		t.Fatalf("the log does not name the join endpoints:\n%s", log.String())
	}
}

// Registering is limited per address, as signing in is, and per link: a
// link that leaks makes accounts no faster than it allows.
func TestRegistrationsThroughALinkAreLimited(t *testing.T) {
	t.Run("per link", func(t *testing.T) {
		// One a minute, so that nothing refills while the test runs.
		a := joinAPI(t, nil, ratelimit.New(1, 2), nil)
		token, _ := a.link(m{})
		for i, email := range []string{"a@example.edu", "b@example.edu"} {
			if r := a.register(nil, token, m{"display_name": "A", "email": email, "password": "a long enough password"}); r.Status != 200 {
				t.Fatalf("registration %d: %d %s", i+1, r.Status, r.Raw)
			}
		}
		r := a.register(nil, token, m{"display_name": "C", "email": "c@example.edu", "password": "a long enough password"})
		if r.Status != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" || a.c.Count(`SELECT count(*) FROM actor WHERE email = 'c@example.edu'`) != 0 {
			t.Fatalf("a third registration through one link: %d %s", r.Status, r.Raw)
		}
		// Another link has its own allowance.
		other, _ := a.link(m{"max_uses": 5})
		if r := a.register(nil, other, m{"display_name": "C", "email": "c@example.edu", "password": "a long enough password"}); r.Status != 200 {
			t.Fatalf("through another link: %d %s", r.Status, r.Raw)
		}
	})
	t.Run("per address", func(t *testing.T) {
		a := joinAPI(t, ratelimit.New(1, 2), nil, nil)
		token, _ := a.link(m{})
		// A registration that succeeds has its address's allowance back.
		for i, email := range []string{"a@example.edu", "b@example.edu", "c@example.edu"} {
			if r := a.register(nil, token, m{"display_name": "A", "email": email, "password": "a long enough password"}); r.Status != 200 {
				t.Fatalf("registration %d: %d %s", i+1, r.Status, r.Raw)
			}
		}
		// One refused keeps it spent: guessing which emails are registered
		// costs as signing in does.
		for i := range 2 {
			if r := a.register(nil, token, m{"display_name": "A", "email": "a@example.edu", "password": "a long enough password"}); r.Status != 409 {
				t.Fatalf("guess %d: %d %s", i+1, r.Status, r.Raw)
			}
		}
		if r := a.register(nil, token, m{"display_name": "D", "email": "d@example.edu", "password": "a long enough password"}); r.Status != http.StatusTooManyRequests {
			t.Fatalf("after two guesses: %d %s", r.Status, r.Raw)
		}
		// And so do sign-in attempts from it: one allowance for both.
		if r := a.do(nil, "POST", "/v1/auth/login", "", m{"email": "a@example.edu", "password": "a long enough password"}); r.Status != http.StatusTooManyRequests {
			t.Fatalf("a sign-in after two guesses: %d %s", r.Status, r.Raw)
		}
	})
}

// With registration off (JOIN_LINK_REGISTRATION=off), nobody registers
// through a link: the page is told so, and people sign in — here by single
// sign-on — and join, as anyone signed in does.
func TestWithRegistrationOffPeopleSignInAndThenJoin(t *testing.T) {
	a := newSSOWith(t, ratelimit.New(0, 0), func(d *httpapi.Deps) { d.NoJoinRegistration = true })
	c := a.c
	token, linkID := a.api.link(m{})
	pv := a.do(nil, "GET", httpapi.JoinPath+token, "", nil)
	if pv.Status != 200 || pv.Body["joinable"] != true || pv.Body["registration"] != false {
		t.Fatalf("preview with registration off: %d %s", pv.Status, pv.Raw)
	}
	reg := a.register(nil, token, m{"display_name": "Aoi", "email": "aoi@example.edu", "password": "aoi's own password"})
	if reg.Status != 422 || reg.str("error", "details", "reason") != "registration_disabled" || !strings.Contains(reg.str("error", "message"), "sign in") ||
		c.Count(`SELECT count(*) FROM actor WHERE email = 'aoi@example.edu'`) != 0 {
		t.Fatalf("registering with registration off: %d %s", reg.Status, reg.Raw)
	}
	// However the link is wrong, the answer is the same: nothing is read.
	if r := a.register(nil, "nothing", m{"display_name": "Aoi"}); r.Status != 422 || r.str("error", "details", "reason") != "registration_disabled" {
		t.Fatalf("registering through no link with registration off: %d %s", r.Status, r.Raw)
	}

	// Mei, registered by an administrator and linked to the provider,
	// signs in by single sign-on and joins.
	mei := c.Actor("human", "Mei")
	c.Exec(`UPDATE actor SET email = 'mei@campus.example.edu' WHERE id = $1`, mei)
	a.link(mei, "mei@campus.example.edu")
	b := browser()
	q := a.start(b, "/join")
	if done := a.callback(b, a.idp.grant("mei@campus.example.edu", q.Get("nonce"), nil), q.Get("state")); !hasSession(done) {
		t.Fatalf("Mei's sign-in: %d %s", done.Status, done.Raw)
	}
	join := a.do(b, "POST", httpapi.JoinPath+token, "", nil, "Idempotency-Key", "mei-joins", "Origin", frontEnd)
	if join.Status != 200 || join.str("status") != "executed" || join.Body["result"].(map[string]any)["already_member"] != false {
		t.Fatalf("Mei joining, signed in by single sign-on: %d %s", join.Status, join.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM course_member m JOIN actor a ON a.id = m.actor_id
		WHERE m.actor_id = $1 AND m.join_link_id = $2 AND m.role = 'student' AND a.email_verified`, mei, linkID); n != 1 {
		t.Fatal("Mei's seat through the link, her email as an administrator gave it")
	}
}
