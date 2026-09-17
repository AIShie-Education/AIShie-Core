package httpapi_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/httpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/mcpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ratelimit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
)

// hardened is the API as serve builds it: limits, logging, MCP and all.
func hardened(t *testing.T, calls, signIns *ratelimit.Limiter, log *slog.Logger) *api {
	t.Helper()
	c := testkit.NewCS101(t, 1)
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewAuthenticator(c.Pool, time.Hour)
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: authn, Log: log,
		Calls: calls, SignIns: signIns, Blob: c.Blob, MaxUploadBytes: testkit.MaxUploadBytes,
		MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: c.P, Auth: authn, Calls: calls}),
	}))
	t.Cleanup(srv.Close)
	return &api{t: t, c: c, srv: srv}
}

// An agent stuck in a loop must not be able to write a denied action per
// iteration for as long as it likes.
func TestARunawayCallerIsStopped(t *testing.T) {
	now := time.Now()
	calls := ratelimit.New(60, 3)
	calls.SetClock(func() time.Time { return now })
	a := hardened(t, calls, nil, nil)
	c, yuki := a.c, a.c.Students[0]
	student, sato := a.tokenFor(yuki.Actor), a.tokenFor(c.Sato)
	course := "/v1/courses/" + c.Course.String()

	// A student trying to grade: denied, and recorded — three times.
	grade := m{"submission_id": yuki.HW3, "score": 100}
	for i := range 3 {
		if r := a.do(nil, "POST", course+"/grades", student, grade, "Idempotency-Key", "loop-"+string(rune('a'+i))); r.Status != 403 {
			t.Fatalf("attempt %d: %d %s", i+1, r.Status, r.Raw)
		}
	}
	// Then stopped, before anything is attempted or recorded.
	r := a.do(nil, "POST", course+"/grades", student, grade, "Idempotency-Key", "loop-d")
	if r.Status != http.StatusTooManyRequests || r.str("error", "code") != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("fourth attempt: %d %v %s", r.Status, r.Header, r.Raw)
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 3 {
		t.Fatalf("%d actions recorded, want the three that were attempted", n)
	}
	// The same allowance over MCP: one actor, one budget, whichever door.
	res, body := a.raw("POST", a.srv.URL+httpapi.MCPPath, "application/json", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		"Authorization", "Bearer "+student, "Accept", "application/json, text/event-stream")
	if res.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "rate_limited") {
		t.Fatalf("MCP for the same actor: %d %s", res.StatusCode, body)
	}
	// Nobody else is affected, and the wait that was promised is enough.
	if r := a.do(nil, "GET", "/v1/me", sato, nil); r.Status != 200 {
		t.Fatalf("another actor: %d", r.Status)
	}
	now = now.Add(2 * time.Second)
	if r := a.do(nil, "GET", "/v1/me", student, nil); r.Status != 200 {
		t.Fatalf("after waiting: %d %s", r.Status, r.Raw)
	}
	// An unauthenticated caller cannot spend anyone's allowance: it is
	// refused for what it is, however often it asks.
	for range 10 {
		if r := a.do(nil, "GET", "/v1/me", "ais_aaaaaaaaaaaa_"+strings.Repeat("A", 43), nil); r.Status != 401 {
			t.Fatalf("bad token: %d", r.Status)
		}
	}
}

func TestSignInAttemptsAreLimited(t *testing.T) {
	// One a minute, so that nothing refills while the test runs: each guess
	// costs an argon2 hash, which under the race detector is most of a second.
	a := hardened(t, nil, ratelimit.New(1, 3), nil)
	a.c.Exec(`UPDATE actor SET email = 'sato@example.edu' WHERE id = $1`, a.c.Sato)
	guess := func(email string) int {
		return a.do(nil, "POST", "/v1/auth/login", "", m{"email": email, "password": "not the password!"}).Status
	}
	// The per-address bucket is shared by every guess from this client; with
	// burst 3 the fourth is refused whoever it was aimed at.
	for i, email := range []string{"sato@example.edu", "SATO@example.edu", "sato@example.edu"} {
		if got := guess(email); got != 401 {
			t.Fatalf("guess %d: %d", i+1, got)
		}
	}
	if got := guess("someone-else@example.edu"); got != http.StatusTooManyRequests {
		t.Fatalf("fourth guess: %d, want 429", got)
	}
}

// The log says who did what and how it went. It never says anything that
// would let a reader of the log become that person.
func TestTheRequestLogCarriesNoCredentials(t *testing.T) {
	var buf bytes.Buffer
	a := hardened(t, nil, nil, slog.New(slog.NewJSONHandler(&buf, nil)))
	c := a.c
	token := a.tokenFor(c.Sato)
	course := "/v1/courses/" + c.Course.String()

	a.do(nil, "GET", "/v1/me?note=private-query-string", token, nil)
	ask := a.do(nil, "GET", course+"/upload-url?kind=material&content_type=text/plain", token, nil)
	putURL := a.here(ask.str("result", "upload_url"))
	a.raw("PUT", putURL, "text/plain", []byte("student work"))
	a.do(nil, "POST", "/v1/auth/login", "", m{"email": "sato@example.edu", "password": "hunter2-hunter2"})
	a.do(nil, "GET", "/healthz", "", nil)

	log := buf.String()
	for name, secret := range map[string]string{
		"an API token": token, "its secret half": token[17:], "a blob URL token": putURL[strings.LastIndex(putURL, "/")+1:],
		"an upload token": ask.str("result", "upload_token"), "a password": "hunter2", "a query string": "private-query-string",
		"an uploaded file": "student work",
	} {
		if secret != "" && strings.Contains(log, secret) {
			t.Errorf("the log contains %s", name)
		}
	}
	for _, want := range []string{`"path":"/v1/me"`, `"actor":"` + c.Sato.String() + `"`, `"status":200`, `"path":"/v1/blobs/…"`, `"path":"/v1/auth/login"`, `"status":401`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %s\n%s", want, log)
		}
	}
	if strings.Contains(log, "/healthz") {
		t.Error("a healthy probe was logged")
	}
}
