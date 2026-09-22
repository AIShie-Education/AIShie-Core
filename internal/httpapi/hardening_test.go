package httpapi_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/httpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/mcpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ratelimit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
)

// hardened is the API as serve builds it: limits, logging, MCP and all.
func hardened(t *testing.T, calls, signIns *ratelimit.Limiter, log *slog.Logger) *api {
	t.Helper()
	return hardenedWith(t, calls, signIns, log, nil)
}

// hardenedWith is hardened with the Deps adjusted first.
func hardenedWith(t *testing.T, calls, signIns *ratelimit.Limiter, log *slog.Logger, adjust func(*httpapi.Deps)) *api {
	t.Helper()
	c := testkit.NewCS101(t, 1)
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewAuthenticator(c.Pool, time.Hour)
	deps := httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: authn, Log: log,
		Calls: calls, SignIns: signIns, Blob: c.Blob, MaxUploadBytes: testkit.MaxUploadBytes,
		MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: c.P, Auth: authn, Calls: calls}),
	}
	if adjust != nil {
		adjust(&deps)
	}
	srv := httptest.NewServer(httpapi.NewHandler(deps))
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

// Behind a reverse proxy every request arrives from the proxy's address.
// Keyed on that, the per-address sign-in limit would be one bucket for the
// whole installation: ten guesses a minute, from anyone, and nobody can sign
// in. With the proxy named as trusted, the client is the one it forwarded.
func TestSignInLimitSeesThroughATrustedProxy(t *testing.T) {
	// The test server is reached from 127.0.0.1; 10.0.0.0/8 stands for a
	// second proxy in the chain, whose own hop is skipped over.
	a := hardenedWith(t, nil, ratelimit.New(1, 3), nil, func(d *httpapi.Deps) {
		d.TrustedProxies = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8"}
	})
	a.c.Exec(`UPDATE actor SET email = 'sato@example.edu' WHERE id = $1`, a.c.Sato)
	if err := auth.SetPassword(context.Background(), dbq.New(a.c.Pool), a.c.Sato, "a long enough password", time.Now()); err != nil {
		t.Fatal(err)
	}
	login := func(email, password, from string) int {
		var headers []string
		if from != "" {
			headers = []string{"X-Forwarded-For", from}
		}
		return a.do(nil, "POST", "/v1/auth/login", "", m{"email": email, "password": password}, headers...).Status
	}
	// Six strangers, six addresses, three guesses each at accounts that do
	// not exist: none of it is Sato's business.
	for i := range 6 {
		addr := "203.0.113." + strconv.Itoa(i+1)
		for j := range 3 {
			if got := login("nobody"+strconv.Itoa(i)+"@example.edu", "guess", addr+", 10.0.0.9"); got != 401 {
				t.Fatalf("stranger %d, guess %d: %d", i, j, got)
			}
		}
	}
	if got := login("sato@example.edu", "a long enough password", "198.51.100.7"); got != 200 {
		t.Fatalf("Sato, from her own address, after strangers guessed: %d, want 200", got)
	}
	// One address is still held to its three, whoever it guesses at.
	for i := range 3 {
		login("victim"+strconv.Itoa(i)+"@example.edu", "guess", "203.0.113.99")
	}
	if got := login("victim3@example.edu", "guess", "203.0.113.99"); got != http.StatusTooManyRequests {
		t.Fatalf("a fourth guess from one address: %d, want 429", got)
	}
	// The proxy naming nobody: no address to key on, and the per-email
	// bucket is what holds.
	for range 3 {
		login("someone@example.edu", "guess", "")
	}
	if got := login("someone@example.edu", "guess", ""); got != http.StatusTooManyRequests {
		t.Fatalf("a fourth guess at one account: %d, want 429", got)
	}
	if got := login("another@example.edu", "guess", ""); got != 401 {
		t.Fatalf("a guess at another account, with no address known: %d, want 401", got)
	}
}

// A null body is not an object, and says so — rather than taking the handler
// down with it and the connection too.
func TestANullBodyIsRefused(t *testing.T) {
	a := newAPI(t, 1)
	tok := a.tokenFor(a.c.Sato)
	res, body := a.raw("POST", a.srv.URL+"/v1/courses/"+a.c.Course.String()+"/grades", "application/json", []byte("null"),
		"Authorization", "Bearer "+tok, "Idempotency-Key", "null-body")
	if res.StatusCode != 400 || !strings.Contains(string(body), "JSON object") {
		t.Fatalf("a null body: %d %s", res.StatusCode, body)
	}
}

// A store that cannot take the file is our fault: logged in full, and the
// holder of an upload URL — who is nobody we know — is told nothing of it,
// least of all where on the disk we tried.
func TestAStoreFaultIsNotShownToTheUploader(t *testing.T) {
	var buf bytes.Buffer
	a := hardened(t, nil, nil, slog.New(slog.NewJSONHandler(&buf, nil)))
	c := a.c
	sato := a.tokenFor(c.Sato)
	ask := a.do(nil, "GET", "/v1/courses/"+c.Course.String()+"/upload-url?kind=material&content_type=text/plain", sato, nil)
	if ask.Status != 200 {
		t.Fatalf("upload-url: %d %s", ask.Status, ask.Raw)
	}
	// The store's root is made unwritable underneath it.
	root := c.Blob.Root()
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	res, body := a.raw("PUT", a.here(ask.str("result", "upload_url")), "text/plain", []byte("hello"))
	if res.StatusCode != 500 || !strings.Contains(string(body), "on our side") || strings.Contains(string(body), root) {
		t.Fatalf("a store fault: %d %s", res.StatusCode, body)
	}
	if !strings.Contains(buf.String(), "permission denied") {
		t.Fatalf("the fault is not in the log: %s", buf.String())
	}
}

// A client that sends a body a byte at a time, or never finishes it, holds a
// connection for as long as the body timeout, not for ever.
func TestATrickledBodyIsCutOff(t *testing.T) {
	a := hardenedWith(t, nil, nil, nil, func(d *httpapi.Deps) { d.BodyTimeout = 300 * time.Millisecond })
	conn, err := net.Dial("tcp", strings.TrimPrefix(a.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "POST /v1/auth/login HTTP/1.1\r\nHost: lms.test\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	buf := make([]byte, 4096)
	n, _ := conn.Read(buf) // an answer, or the connection closed; either is fine
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the server waited %s for a body that never came", took)
	}
	if n > 0 && !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 4") {
		t.Fatalf("answered: %s", buf[:n])
	}
}

// A front end on another site needs SameSite=None, with Secure.
func TestCookiesForACrossSiteFrontEnd(t *testing.T) {
	a := hardenedWith(t, nil, nil, nil, func(d *httpapi.Deps) { d.CookieSameSite = http.SameSiteNoneMode })
	a.c.Exec(`UPDATE actor SET email = 'sato@example.edu' WHERE id = $1`, a.c.Sato)
	if err := auth.SetPassword(context.Background(), dbq.New(a.c.Pool), a.c.Sato, "a long enough password", time.Now()); err != nil {
		t.Fatal(err)
	}
	login := a.do(nil, "POST", "/v1/auth/login", "", m{"email": "sato@example.edu", "password": "a long enough password"})
	cookie := login.Header.Get("Set-Cookie")
	if login.Status != 200 || !strings.Contains(cookie, "SameSite=None") || !strings.Contains(cookie, "Secure") {
		t.Fatalf("session cookie for a cross-site front end: %d %q", login.Status, cookie)
	}
}
