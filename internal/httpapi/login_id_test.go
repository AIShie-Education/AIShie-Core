package httpapi_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
)

// A student with no email registers through a join link with their student
// number, signs in with it, forgets their password, is given a temporary one
// by their instructor, signs in with that, is made to set their own, and
// works as before; over HTTP, as the front end does it.
func TestAStudentNumberOverHTTP(t *testing.T) {
	var log bytes.Buffer
	a := joinAPI(t, nil, nil, slog.New(slog.NewJSONHandler(&log, nil)))
	c := a.c
	token, _ := a.link(m{})
	course := "/v1/courses/" + c.Course.String()

	// The page that opens the link is told no email is needed.
	if pv := a.do(nil, "GET", httpapi.JoinPath+token, "", nil); pv.Status != 200 || pv.Body["email_required"] != false {
		t.Fatalf("preview: %d %s", pv.Status, pv.Raw)
	}
	reg := a.register(nil, token, m{"display_name": "Wei", "login_id": " 20230001 ", "password": "weis own password"})
	if reg.Status != 200 {
		t.Fatalf("registering with a login ID and no email: %d %s", reg.Status, reg.Raw)
	}
	weiM := reg.str("member_id")
	// Someone else with the same number, in any case, is told to sign in.
	taken := a.register(nil, token, m{"display_name": "Impostor", "login_id": "20230001", "password": "another long password"})
	if taken.Status != 409 || taken.str("error", "details", "reason") != "login_id_taken" || taken.Header.Get("Set-Cookie") != "" {
		t.Fatalf("a login ID registered already: %d %s", taken.Status, taken.Raw)
	}
	// Neither a login ID nor an email: nothing to sign in with.
	if none := a.register(nil, token, m{"display_name": "Nobody", "password": "a long enough password"}); none.Status != 400 {
		t.Fatalf("registering with neither: %d %s", none.Status, none.Raw)
	}

	// Signing in: login takes the login ID, and email, which old clients
	// send, takes it too. Wrong, it is refused as a wrong email is.
	login := func(body m) response { return a.do(nil, "POST", "/v1/auth/login", "", body) }
	for _, body := range []m{
		{"login": "20230001", "password": "weis own password"},
		{"email": "20230001", "password": "weis own password"},
		{"login": "20230001", "email": "20230001", "password": "weis own password"},
	} {
		if r := login(body); r.Status != 200 || r.Body["password_change_required"] != false || r.str("actor_id") != reg.str("actor_id") {
			t.Fatalf("signing in with %v: %d %s", body, r.Status, r.Raw)
		}
	}
	wrong, unknown := login(m{"login": "20230001", "password": "not the password!"}), login(m{"email": "nobody@example.edu", "password": "x"})
	if wrong.Status != 401 || unknown.Status != 401 || wrong.str("error", "message") != unknown.str("error", "message") {
		t.Fatalf("wrong: %d %s; unknown: %d %s", wrong.Status, wrong.Raw, unknown.Status, unknown.Raw)
	}
	if r := login(m{"login": "20230001", "email": "wei@example.edu", "password": "weis own password"}); r.Status != 400 {
		t.Fatalf("login and email saying different things: %d %s", r.Status, r.Raw)
	}

	// Her instructor sees her number on her seat, and resets her password;
	// the answer carries the temporary one, once.
	sato := a.tokenFor(c.Sato)
	if seat := a.do(nil, "GET", course+"/members/"+weiM, sato, nil); seat.str("result", "login_id") != "20230001" {
		t.Fatalf("her seat: %d %s", seat.Status, seat.Raw)
	}
	reset := a.do(nil, "POST", course+"/members/"+weiM+"/reset-password", sato, nil, "Idempotency-Key", "reset-wei")
	temporary := reset.str("result", "temporary_password")
	if reset.Status != 200 || len(temporary) != 19 || reset.str("result", "login_id") != "20230001" {
		t.Fatalf("the reset: %d %s", reset.Status, reset.Raw)
	}
	if again := a.do(nil, "POST", course+"/members/"+weiM+"/reset-password", sato, nil, "Idempotency-Key", "reset-wei"); again.Status != 200 ||
		again.Header.Get(httpapi.HeaderReplayed) != "true" || strings.Contains(again.Raw, temporary) || strings.Contains(again.Raw, "temporary_password") {
		t.Fatalf("the reset, again: %d %s", again.Status, again.Raw)
	}

	// She signs in with it, and is told to change it; until she does, every
	// call is refused, as the page that makes her is told.
	if r := login(m{"login": "20230001", "password": "weis own password"}); r.Status != 401 {
		t.Fatalf("her old password: %d %s", r.Status, r.Raw)
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	in := a.do(browser, "POST", "/v1/auth/login", "", m{"login": "20230001", "password": temporary}, "Origin", frontEnd)
	if in.Status != 200 || in.Body["password_change_required"] != true {
		t.Fatalf("signing in with the temporary password: %d %s", in.Status, in.Raw)
	}
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/me", nil}, {"GET", "/v1/me/memberships", nil}, {"GET", course + "/documents", nil},
		{"POST", course + "/submissions", m{"assignment_id": c.HW4, "body": "my essay"}}, {"POST", httpapi.JoinPath + token, nil},
	} {
		r := a.do(browser, call.method, call.path, "", call.body, "Idempotency-Key", "blocked-"+uuid.NewString(), "Origin", frontEnd)
		if r.Status != http.StatusForbidden || r.str("error", "details", "reason") != "password_change_required" {
			t.Fatalf("%s %s before she sets her own: %d %s", call.method, call.path, r.Status, r.Raw)
		}
	}
	if r := a.do(browser, "POST", "/v1/me/password", "", m{"password": temporary}, "Idempotency-Key", "same", "Origin", frontEnd); r.Status != 400 ||
		r.str("error", "details", "reason") != "password_unchanged" {
		t.Fatalf("the temporary password as her own: %d %s", r.Status, r.Raw)
	}
	if r := a.do(browser, "POST", "/v1/me/password", "", m{"password": "weis new password"}, "Idempotency-Key", "own", "Origin", frontEnd); r.Status != 200 {
		t.Fatalf("setting her own: %d %s", r.Status, r.Raw)
	}
	if r := a.do(browser, "GET", "/v1/me", "", nil); r.Status != 200 || r.str("result", "login_id") != "20230001" {
		t.Fatalf("me, after: %d %s", r.Status, r.Raw)
	}
	if r := login(m{"login": "20230001", "password": "weis new password"}); r.Status != 200 || r.Body["password_change_required"] != false {
		t.Fatalf("signing in with her own: %d %s", r.Status, r.Raw)
	}

	// Nothing of it is in the log.
	for _, secret := range []string{temporary, "weis own password", "weis new password"} {
		if strings.Contains(log.String(), secret) {
			t.Fatal("the log carries a password")
		}
	}

	// A TA's password is not the instructor's to reset.
	ta := a.do(nil, "POST", "/v1/actors", a.tokenFor(c.Root), m{"kind": "human", "display_name": "Tanaka", "login_id": "T0042"},
		"Idempotency-Key", "tanaka")
	taM := a.do(nil, "POST", course+"/members", sato, m{"actor_id": ta.str("result", "actor_id"), "preset": "ta"}, "Idempotency-Key", "seat-tanaka")
	if r := a.do(nil, "POST", course+"/members/"+taM.str("result", "member_id")+"/reset-password", sato, nil, "Idempotency-Key", "reset-ta"); r.Status != 403 ||
		r.str("error", "details", "reason") != "not_a_student" {
		t.Fatalf("a TA's password: %d %s", r.Status, r.Raw)
	}
}

// A sign-in by login ID is limited as one by email is: by address, and by
// the name it is aimed at, each name its own.
func TestSignInsByLoginIDAreLimitedAsByEmail(t *testing.T) {
	a := hardenedWith(t, nil, ratelimit.New(1, 3), nil, func(d *httpapi.Deps) {
		d.TrustedProxies = []string{"127.0.0.0/8", "::1/128"}
	})
	a.c.Exec(`UPDATE actor SET login_id = '20230001' WHERE id = $1`, a.c.Sato)
	login := func(name, from string) int {
		return a.do(nil, "POST", "/v1/auth/login", "", m{"login": name, "password": "guess"}, "X-Forwarded-For", from).Status
	}
	// Three guesses at one number, in any case, from three addresses.
	for i, name := range []string{"20230001", "20230001", "20230001 "} {
		if got := login(name, "203.0.113."+string(rune('1'+i))); got != 401 {
			t.Fatalf("guess %d: %d", i+1, got)
		}
	}
	if got := login("20230001", "203.0.113.9"); got != http.StatusTooManyRequests {
		t.Fatalf("a fourth guess at one login ID: %d, want 429", got)
	}
	// Another number, and a number no account has, are their own.
	if got := login("20230002", "203.0.113.9"); got != 401 {
		t.Fatalf("a guess at another login ID: %d, want 401", got)
	}
}
