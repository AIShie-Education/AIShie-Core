package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/mcpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
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

// The limit charges a request, so a request is one call. A JSON-RPC batch
// would put a body's worth of tool calls, each attempted and recorded, behind
// the one call it was charged for; it is refused before any is attempted.
func TestABatchIsNotManyCallsForThePriceOfOne(t *testing.T) {
	now := time.Now()
	calls := ratelimit.New(60, 3)
	calls.SetClock(func() time.Time { return now })
	a := hardened(t, calls, nil, nil)
	c, yuki := a.c, a.c.Students[0]
	student := a.tokenFor(yuki.Actor)
	grade := func(id int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"grade_submit","arguments":`+
			`{"course_id":%q,"submission_id":%q,"score":100,"idempotency_key":"batch-%d"}}}`, id, c.Course, yuki.HW3, id)
	}
	// No Mcp-Protocol-Version header: from a client that names no version,
	// the SDK itself would take a batch.
	post := func(body string) (rawResponse, []byte) {
		return a.raw("POST", a.srv.URL+httpapi.MCPPath, "application/json", []byte(body),
			"Authorization", "Bearer "+student, "Accept", "application/json, text/event-stream")
	}
	var batch []string
	for i := range 20 {
		batch = append(batch, grade(i))
	}
	for _, body := range []string{"[" + strings.Join(batch, ",") + "]", " \r\n\t[" + grade(20) + "]"} {
		if res, got := post(body); res.StatusCode != http.StatusBadRequest || !strings.Contains(string(got), "a batch is not taken") {
			t.Fatalf("a batch: %d %.300s", res.StatusCode, got)
		}
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 0 {
		t.Fatalf("%d actions recorded from batches, want none", n)
	}
	// One call to a request is attempted, recorded, and charged as one.
	if res, got := post(" \n" + grade(21)); res.StatusCode != 200 || !strings.Contains(string(got), `"status":"denied"`) {
		t.Fatalf("a single call: %d %s", res.StatusCode, got)
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 1 {
		t.Fatalf("%d actions recorded, want the one call", n)
	}
}

// A reverse proxy on the same machine connects over loopback and forwards the
// public name in Host; so does a page that reaches the server by DNS
// rebinding. Named in TRUSTED_PROXIES, the proxy gets its agents through to
// /mcp. Nothing else that comes over loopback may name a host that is not.
func TestMCPWorksBehindAProxyOnTheSameMachine(t *testing.T) {
	list := func(a *api, host, token string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("POST", a.srv.URL+httpapi.MCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host // the test server listens on 127.0.0.1
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	proxied := hardenedWith(t, nil, nil, nil, func(d *httpapi.Deps) { d.TrustedProxies = []string{"127.0.0.0/8", "::1/128"} })
	if status, body := list(proxied, "lms.example.edu", proxied.tokenFor(proxied.c.Sato)); status != 200 || !strings.Contains(body, `"tools"`) {
		t.Fatalf("through a proxy on the same machine: %d %.300s", status, body)
	}
	direct := hardened(t, nil, nil, nil)
	sato := direct.tokenFor(direct.c.Sato)
	if status, body := list(direct, "rebound.example", sato); status != http.StatusForbidden || !strings.Contains(body, "TRUSTED_PROXIES") {
		t.Fatalf("a public name over loopback, with no proxy named: %d %.300s", status, body)
	}
	if status, body := list(direct, "localhost:8080", sato); status != 200 || !strings.Contains(body, `"tools"`) {
		t.Fatalf("localhost over loopback: %d %.300s", status, body)
	}
	// The check comes before the token is looked at. A rebound page has no
	// token, and its guesses are refused for where they came from without
	// ever reaching the credential store; the same guess from localhost is
	// refused as a guess.
	if status, body := list(direct, "rebound.example", "not-a-token"); status != http.StatusForbidden || !strings.Contains(body, "TRUSTED_PROXIES") {
		t.Fatalf("a guessed token over a rebound name: %d %.300s", status, body)
	}
	if status, body := list(direct, "localhost:8080", "not-a-token"); status != http.StatusUnauthorized {
		t.Fatalf("a guessed token from localhost: %d %.300s", status, body)
	}
	// A proxy named elsewhere says nothing about loopback: what comes in
	// over loopback naming a public host is refused as though none were.
	elsewhere := hardenedWith(t, nil, nil, nil, func(d *httpapi.Deps) { d.TrustedProxies = []string{"10.0.0.0/8"} })
	if status, body := list(elsewhere, "lms.example.edu", elsewhere.tokenFor(elsewhere.c.Sato)); status != http.StatusForbidden || !strings.Contains(body, "TRUSTED_PROXIES") {
		t.Fatalf("a public name over loopback, with only a proxy elsewhere named: %d %.300s", status, body)
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

// A sign-in the address limit refuses leaves nothing behind. Were it to leave
// a bucket keyed on its email — a megabyte of key, if the body was — for ten
// minutes, one address the limit had already stopped could fill the server's
// memory without an account.
func TestARefusedSignInLeavesNothingBehind(t *testing.T) {
	signIns := ratelimit.New(1, 3)
	a := hardened(t, nil, signIns, nil)
	guess := func(email string) int {
		return a.do(nil, "POST", "/v1/auth/login", "", m{"email": email, "password": "not the password!"}).Status
	}
	long := strings.Repeat("a", 64<<10)
	for i := range 3 {
		if got := guess(fmt.Sprintf("%d%s@example.edu", i, long)); got != 401 {
			t.Fatalf("guess %d: %d", i+1, got)
		}
	}
	for i := 3; i < 20; i++ {
		if got := guess(fmt.Sprintf("%d%s@example.edu", i, long)); got != http.StatusTooManyRequests {
			t.Fatalf("guess %d: %d, want 429", i+1, got)
		}
	}
	// One bucket for the address, one for each email it was allowed to try.
	if n := signIns.Len(); n != 4 {
		t.Fatalf("%d sign-in buckets kept, want 4", n)
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
	// An upload given up half way fails, and so does a download of a file
	// whose bytes have gone from the disk, which is ours and has more said of
	// it in the log. Their URLs are credentials all the same, and still good.
	cut := a.here(a.do(nil, "GET", course+"/upload-url?kind=material&content_type=text/plain", token, nil).str("result", "upload_url"))
	a.putHalf(cut, true)
	if err := os.WriteFile(filepath.Join(c.Blob.Root(), "lost.meta"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lost, err := c.Blob.PresignGet(context.Background(), "lost", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	a.raw("GET", a.here(lost), "", nil)

	log := buf.String()
	for name, secret := range map[string]string{
		"an API token": token, "its secret half": token[17:], "a blob URL token": putURL[strings.LastIndex(putURL, "/")+1:],
		"an upload token": ask.str("result", "upload_token"), "a password": "hunter2", "a query string": "private-query-string",
		"an uploaded file": "student work", "a cut-off upload's URL token": cut[strings.LastIndex(cut, "/")+1:],
		"a failed download's URL token": lost[strings.LastIndex(lost, "/")+1:],
	} {
		if secret != "" && strings.Contains(log, secret) {
			t.Errorf("the log contains %s", name)
		}
	}
	for _, want := range []string{`"path":"/v1/me"`, `"actor":"` + c.Sato.String() + `"`, `"status":200`, `"path":"/v1/blobs/…"`, `"path":"/v1/auth/login"`, `"status":401`,
		`"msg":"internal error","method":"GET","path":"/v1/blobs/…"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %s\n%s", want, log)
		}
	}
	if strings.Contains(log, "/healthz") {
		t.Error("a healthy probe was logged")
	}
}

// putHalf starts a PUT of a hundred bytes to url, sends ten, and hangs up,
// or with hangUp false sends nothing more and waits. It returns the answer,
// which is not written until the request is logged.
func (a *api) putHalf(url string, hangUp bool) (int, string) {
	a.t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(a.srv.URL, "http://"))
	if err != nil {
		a.t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: lms.test\r\nContent-Type: text/plain\r\nContent-Length: 100\r\n\r\nhalf a fil", strings.TrimPrefix(url, a.srv.URL))
	if hangUp {
		_ = conn.(*net.TCPConn).CloseWrite()
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		a.t.Fatalf("a PUT cut off half way got no answer: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
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

// Whoever holds an IPv6 /64 may send from any address in it. The sign-in
// limit holds a /64 to the guesses of one address; the /64 next to it is
// another network. An IPv4 address is one address, however written.
func TestAnIPv6NetworkIsOneAddressToTheSignInLimit(t *testing.T) {
	a := hardenedWith(t, nil, ratelimit.New(1, 3), nil, func(d *httpapi.Deps) { d.TrustedProxies = []string{"127.0.0.0/8", "::1/128"} })
	guesses := 0
	login := func(from string) int {
		guesses++
		return a.do(nil, "POST", "/v1/auth/login", "", m{"email": fmt.Sprintf("nobody%d@example.edu", guesses), "password": "guess"},
			"X-Forwarded-For", from).Status
	}
	for i := range 3 {
		if got := login(fmt.Sprintf("2001:db8:1:2::%x", i+1)); got != 401 {
			t.Fatalf("guess %d from one /64: %d", i+1, got)
		}
	}
	if got := login("2001:db8:1:2:ffff:ffff:ffff:ffff"); got != http.StatusTooManyRequests {
		t.Fatalf("a fourth guess from another address in the same /64: %d, want 429", got)
	}
	if got := login("2001:db8:1:3::1"); got != 401 {
		t.Fatalf("a guess from the next /64: %d, want 401", got)
	}
	for i := range 3 {
		if got := login("203.0.113.7"); got != 401 {
			t.Fatalf("guess %d from one IPv4 address: %d", i+1, got)
		}
	}
	if got := login("::ffff:203.0.113.7"); got != http.StatusTooManyRequests {
		t.Fatalf("a fourth guess from the same IPv4 address written as IPv6: %d, want 429", got)
	}
	if got := login("203.0.113.8"); got != 401 {
		t.Fatalf("a guess from the next IPv4 address: %d, want 401", got)
	}
}

// One address may be a whole lecture hall: an IPv4 address behind a NAT, or
// a campus LAN's /64. A sign-in that succeeds was no guess and costs its
// address nothing, so that a class signing in at once is not held to what
// one address is allowed; guesses from there are, all the same. The account
// is still held to its own: every sign-in to it counts.
func TestAClassOnOneNetworkCanAllSignIn(t *testing.T) {
	a := hardenedWith(t, nil, ratelimit.New(1, 3), nil, func(d *httpapi.Deps) { d.TrustedProxies = []string{"127.0.0.0/8", "::1/128"} })
	login := func(email, password, from string) response {
		return a.do(nil, "POST", "/v1/auth/login", "", m{"email": email, "password": password}, "X-Forwarded-For", from)
	}
	for i := range 5 {
		student := a.c.Sato
		if i > 0 {
			student = a.c.Actor("human", fmt.Sprintf("Student %d", i))
		}
		email := fmt.Sprintf("student%d@example.edu", i)
		a.c.Exec(`UPDATE actor SET email = $2 WHERE id = $1`, student, email)
		if err := auth.SetPassword(context.Background(), dbq.New(a.c.Pool), student, "a long enough password", time.Now()); err != nil {
			t.Fatal(err)
		}
		if r := login(email, "a long enough password", fmt.Sprintf("2001:db8:5:6:%x::1", i+1)); r.Status != 200 {
			t.Fatalf("student %d of five, with the right password, from one /64: %d %s", i+1, r.Status, r.Raw)
		}
	}
	for i := range 2 {
		if r := login("student0@example.edu", "a long enough password", "198.51.100.7"); r.Status != 200 {
			t.Fatalf("sign-in %d of three to one account: %d %s", i+2, r.Status, r.Raw)
		}
	}
	if r := login("student0@example.edu", "a long enough password", "198.51.100.7"); r.Status != http.StatusTooManyRequests {
		t.Fatalf("a fourth sign-in to one account: %d, want 429", r.Status)
	}
	for i := range 3 {
		if r := login(fmt.Sprintf("nobody%d@example.edu", i), "guess", "2001:db8:5:6::99"); r.Status != 401 {
			t.Fatalf("guess %d from the class's /64: %d", i+1, r.Status)
		}
	}
	if r := login("nobody3@example.edu", "guess", "2001:db8:5:6::99"); r.Status != http.StatusTooManyRequests {
		t.Fatalf("a fourth guess from the class's /64: %d, want 429", r.Status)
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

// A body is one JSON object, each key given once: what a repeated key or a
// second value means is anybody's guess, and an MCP client is told the same.
func TestABodyIsOneObjectWithEachKeyOnce(t *testing.T) {
	a := newAPI(t, 1)
	tok := a.tokenFor(a.c.Sato)
	work := a.c.Students[0].HW3.String()
	for i, body := range []string{
		`{"submission_id": "` + work + `", "score": 1, "score": 2}`,
		`{"submission_id": "` + work + `", "score": 1, "breakdown": [{"criterion": "a", "points": 1, "points": 2, "max": 2}]}`,
		`{"submission_id": "` + work + `", "score": 1} {"score": 2}`,
	} {
		res, out := a.raw("POST", a.srv.URL+"/v1/courses/"+a.c.Course.String()+"/grades", "application/json", []byte(body),
			"Authorization", "Bearer "+tok, "Idempotency-Key", "twice-"+strconv.Itoa(i))
		if res.StatusCode != 400 {
			t.Fatalf("%s: %d %s", body, res.StatusCode, out)
		}
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

// A file may take longer than the body timeout to arrive; that is what the
// transfer timeout is for. Once it is in, the uploader is told so. A client
// that sent the whole file and heard nothing back would take the upload for
// failed, and its retry would find the URL already used.
func TestASlowUploadIsAnswered(t *testing.T) {
	const bodyTimeout = 300 * time.Millisecond
	a := hardenedWith(t, nil, nil, nil, func(d *httpapi.Deps) { d.BodyTimeout = bodyTimeout })
	ask := a.do(nil, "GET", "/v1/courses/"+a.c.Course.String()+"/upload-url?kind=material&content_type=text/plain", a.tokenFor(a.c.Sato), nil)
	if ask.Status != 200 {
		t.Fatalf("upload-url: %d %s", ask.Status, ask.Raw)
	}
	// A byte at a time, until well past the body timeout: slow, not stuck.
	body, w := io.Pipe()
	sent := make(chan int, 1)
	go func() {
		n := 0
		tick := time.NewTicker(bodyTimeout / 10)
		defer tick.Stop()
		for start := time.Now(); time.Since(start) < 3*bodyTimeout; n++ {
			<-tick.C
			if _, err := w.Write([]byte("x")); err != nil {
				break
			}
		}
		sent <- n
		_ = w.Close()
	}()
	req, err := http.NewRequest("PUT", a.here(ask.str("result", "upload_url")), body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the slow upload got no answer: %v", err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if n := <-sent; res.StatusCode != 200 || !strings.Contains(string(got), fmt.Sprintf(`"byte_size":%d`, n)) {
		t.Fatalf("the slow upload of %d bytes: %d %s", n, res.StatusCode, got)
	}
}

// A file that does not arrive in full is the uploader's to send again, not a
// fault of ours: whether they hung up half way or had not finished when the
// transfer timeout ran out, they are told so, and nothing of it is kept, so
// that the same URL takes the whole file after. It is logged as a warning,
// not an error, saying what went wrong but not whose connection it was.
func TestAnUploadThatDoesNotArriveCanBeSentAgain(t *testing.T) {
	var buf bytes.Buffer
	a := hardenedWith(t, nil, nil, slog.New(slog.NewJSONHandler(&buf, nil)), func(d *httpapi.Deps) { d.TransferTimeout = 300 * time.Millisecond })
	ask := a.do(nil, "GET", "/v1/courses/"+a.c.Course.String()+"/upload-url?kind=material&content_type=text/plain", a.tokenFor(a.c.Sato), nil)
	if ask.Status != 200 {
		t.Fatalf("upload-url: %d %s", ask.Status, ask.Raw)
	}
	putURL := a.here(ask.str("result", "upload_url"))
	for _, hangUp := range []bool{true, false} {
		if status, body := a.putHalf(putURL, hangUp); status != 400 || !strings.Contains(body, "did not arrive in full; upload it again") {
			t.Fatalf("ten bytes of a hundred, hanging up %v: %d %s", hangUp, status, body)
		}
	}
	if log := buf.String(); strings.Contains(log, `"level":"ERROR"`) || strings.Contains(log, "127.0.0.1") ||
		!strings.Contains(log, `"level":"WARN","msg":"an upload did not arrive in full","err":"unexpected EOF"`) ||
		!strings.Contains(log, `"level":"WARN","msg":"an upload did not arrive in full","err":"i/o timeout"`) ||
		!strings.Contains(log, `"level":"INFO","msg":"request","method":"PUT","path":"/v1/blobs/…","status":400`) {
		t.Fatalf("an upload that did not arrive, in the log:\n%s", log)
	}
	if res, body := a.raw("PUT", putURL, "text/plain", []byte("the whole file")); res.StatusCode != 200 || !strings.Contains(string(body), `"byte_size":14`) {
		t.Fatalf("the whole file, to the same URL: %d %s", res.StatusCode, body)
	}
}

// A body can be broken off by more than the clock: here by chunking that
// makes no sense, sent all at once. The uploader is told the file did not
// arrive in full and how long a file has, not that it ran out of time.
func TestAnUploadBrokenOffIsNotBlamedOnTheClock(t *testing.T) {
	a := hardened(t, nil, nil, nil)
	ask := a.do(nil, "GET", "/v1/courses/"+a.c.Course.String()+"/upload-url?kind=material&content_type=text/plain", a.tokenFor(a.c.Sato), nil)
	if ask.Status != 200 {
		t.Fatalf("upload-url: %d %s", ask.Status, ask.Raw)
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(a.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: lms.test\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\nzz\r\n",
		strings.TrimPrefix(a.here(ask.str("result", "upload_url")), a.srv.URL))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	const want = "the file did not arrive in full; upload it again (a file has 10 minutes to arrive)"
	if res.StatusCode != 400 || !strings.Contains(string(body), `"message":"`+want+`"`) {
		t.Fatalf("a chunked body that makes no sense: %d %s", res.StatusCode, body)
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

// A refusal says what was wrong, not all of what was sent. A megabyte of
// input is answered in a few kilobytes whichever check refuses it — the
// schema, the decoding after it, the path, the router or its tidying of a
// path, a header, the identity provider's answer — however early, and so to
// any caller at all; and a failure is recorded no longer than it is told.
func TestARefusalRepeatsLittleOfWhatItRefuses(t *testing.T) {
	a := newAPI(t, 1)
	c := a.c
	student, sato := a.tokenFor(c.Students[0].Actor), a.tokenFor(c.Sato)
	course := a.srv.URL + "/v1/courses/" + c.Course.String()
	work := c.Students[0].HW3.String()
	big := strings.Repeat("<", 1<<20-200) // six bytes each, once JSON has escaped them
	const most = 4 << 10
	small := func(what string, res rawResponse, out []byte, status int, says string) {
		t.Helper()
		if n := len(out) + headerBytes(res.Header); res.StatusCode != status || n > most || !strings.Contains(string(out), says) {
			t.Errorf("%s: %d, %d bytes: %.300s", what, res.StatusCode, n, out)
		}
	}
	for i, tc := range []struct{ what, path, body, says string }{
		{"a score that is no number", "/grades", `{"submission_id": "` + work + `", "score": "` + big + `"}`, "/properties/score"},
		{"a key the schema does not have", "/grades", `{"submission_id": "` + work + `", "score": 1, "` + big + `": 1}`, "unexpected additional properties"},
		{"a course_id that is not the path's", "/grades", `{"submission_id": "` + work + `", "score": 1, "course_id": "` + big + `"}`, "course_id in the request is not the one in the path"},
		{"a date that does not parse", "/assignments", `{"title": "HW5", "due_at": "` + big + `"}`, "parsing time"},
	} {
		res, out := a.raw("POST", course+tc.path, "application/json", []byte(tc.body),
			"Authorization", "Bearer "+student, "Idempotency-Key", "big-"+strconv.Itoa(i))
		small(tc.what, res, out, http.StatusBadRequest, tc.says)
	}

	res, out := a.raw("PATCH", a.srv.URL+"/v1/courses/"+url.PathEscape(big[:1<<18])+"/grades", "", nil)
	small("a method the route does not take", res, out, http.StatusMethodNotAllowed, "method_not_allowed")

	// A path the router would tidy, in raw bytes, which its redirect would
	// escape to three each, twice over. The second tidies to a route. Then
	// a short one with a long query, which the redirect would repeat as it
	// was sent in Location and escaped in the page; the last has no path at
	// all, which tidies to /.
	get := func(target string) (rawResponse, []byte) {
		t.Helper()
		conn, err := net.Dial("tcp", a.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: lms.test\r\nConnection: close\r\n\r\n", target)
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return rawResponse{StatusCode: res.StatusCode, Header: res.Header}, out
	}
	junk, query := strings.Repeat("\x80", 1<<18), strings.Repeat("&", 1<<18)
	for _, target := range []string{"/v1//" + junk, "/v1//courses/" + junk + "/grades", "/v1//tools?" + query, "http://lms.test?" + query} {
		res, out := get(target)
		small("a path the router would tidy", res, out, http.StatusNotFound, "no such route")
	}
	if res, _ := get("/v1//tools?tab=mine"); res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != "/v1/tools?tab=mine" {
		t.Errorf("a short path is no longer tidied: %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	ask := a.do(nil, "GET", "/v1/courses/"+c.Course.String()+"/upload-url?kind=material&content_type=text/plain", sato, nil)
	res, out = a.raw("PUT", a.here(ask.str("result", "upload_url")), big[:1<<19], []byte("hello"))
	small("an upload of another type", res, out, http.StatusBadRequest, "Content-Type")

	// A long path that is tidy is routed as ever: a blob URL's token, which
	// carries the content type, runs past what the router tidies.
	docx := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	ask = a.do(nil, "GET", "/v1/courses/"+c.Course.String()+"/upload-url?kind=material&content_type="+url.QueryEscape(docx), sato, nil)
	if res, out := a.raw("PUT", a.here(ask.str("result", "upload_url")), docx, []byte("hello")); res.StatusCode != http.StatusOK {
		t.Errorf("a long blob URL: %d %s", res.StatusCode, out)
	}

	// A name stored whole, repeated in a failure that is recorded.
	c.Exec(`UPDATE grade_component SET name = $1 WHERE id = $2`, big, c.Midterm)
	res, out = a.raw("POST", course+"/components", "application/json", []byte(`{"parent_id": "`+c.Midterm.String()+`", "name": "Part A"}`),
		"Authorization", "Bearer "+sato, "Idempotency-Key", "under-the-midterm")
	small("a failure that names the component", res, out, http.StatusUnprocessableEntity, "failed_precondition")
	if n := c.Count(`SELECT count(*) FROM action WHERE status = 'failed' AND length(result::text) < 1024`); n != 1 {
		t.Errorf("%d failures recorded in under a kilobyte, want 1", n)
	}

	s := newSSO(t)
	b := browser()
	q := s.start(b, "")
	r := s.do(b, "GET", httpapi.SSOCallbackPath+"?error="+url.QueryEscape(big[:1<<18])+"&state="+q.Get("state"), "", nil)
	if r.Status != http.StatusUnauthorized || len(r.Raw) > most {
		t.Errorf("the identity provider's refusal: %d, %d bytes: %.300s", r.Status, len(r.Raw), r.Raw)
	}
	// Not a refusal, but as early and as open: where to go after signing
	// in, which the start carries in a cookie, escaped for JSON and then in
	// base64.
	r = s.do(browser(), "GET", "/v1/auth/sso/start?return_to=/"+url.QueryEscape(big[:1<<18]), "", nil)
	if n := len(r.Raw) + headerBytes(r.Header); r.Status != http.StatusFound || n > most {
		t.Errorf("the start of a sign-in: %d, %d bytes", r.Status, n)
	}
}

// headerBytes is about what a response's headers take on the wire. They
// are part of what a refusal says as much as its body is.
func headerBytes(h http.Header) int {
	n := 0
	for k, vs := range h {
		for _, v := range vs {
			n += len(k) + len(v) + len(": \r\n")
		}
	}
	return n
}

// What no credential or account could be is answered as not one, at every
// door, and not as a fault of ours to retry: a token whose prefix is not in
// the alphabet prefixes are made in (Go's server passes a header's bytes
// that are not UTF-8, and the database refuses them), and an email holding
// U+0000. A sign-in with such an email is counted like any other guess.
func TestWhatTheDatabaseCannotHoldIsNoCredential(t *testing.T) {
	var log bytes.Buffer
	a := hardened(t, nil, ratelimit.New(1, 3), slog.New(slog.NewJSONHandler(&log, nil)))
	token := "ais_\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff_" + strings.Repeat("A", 43)
	if r := a.do(nil, "GET", "/v1/me", token, nil); r.Status != http.StatusUnauthorized {
		t.Errorf("REST: %d %s", r.Status, r.Raw)
	}
	res, body := a.raw("POST", a.srv.URL+httpapi.MCPPath, "application/json", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		"Authorization", "Bearer "+token, "Accept", "application/json, text/event-stream")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("MCP: %d %s", res.StatusCode, body)
	}
	for i := range 3 {
		if r := a.do(nil, "POST", "/v1/auth/login", "", m{"email": "sato\x00@example.edu", "password": "not the password!"}); r.Status != http.StatusUnauthorized {
			t.Fatalf("sign-in %d: %d %s", i+1, r.Status, r.Raw)
		}
	}
	if r := a.do(nil, "POST", "/v1/auth/login", "", m{"email": "sato\x00@example.edu", "password": "not the password!"}); r.Status != http.StatusTooManyRequests {
		t.Fatalf("a fourth sign-in: %d, want 429", r.Status)
	}
	if strings.Contains(log.String(), `"level":"ERROR"`) {
		t.Errorf("logged as a fault of ours: %s", log.String())
	}
}

// A call carries one idempotency key. Given two, whichever a retry carried
// would decide whether it was the same call, so neither is taken, and
// nothing is attempted.
func TestACallCarriesOneIdempotencyKey(t *testing.T) {
	a := newAPI(t, 1)
	req, err := http.NewRequest("POST", a.srv.URL+"/v1/courses/"+a.c.Course.String()+"/grades",
		strings.NewReader(`{"submission_id": "`+a.c.Students[0].HW3.String()+`", "score": 1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.tokenFor(a.c.Sato))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add(httpapi.HeaderIdempotencyKey, "first")
	req.Header.Add(httpapi.HeaderIdempotencyKey, "second")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "invalid_argument") {
		t.Fatalf("two keys: %d %s", res.StatusCode, body)
	}
	if n := a.c.Count(`SELECT count(*) FROM action`); n != 0 {
		t.Fatalf("%d actions recorded", n)
	}
}

// Agent harnesses that run in a browser or an app send an Origin of their
// own: Claude's custom connectors, say, from https://claude.ai. /mcp takes a
// bearer token and never a cookie, so a page on another site has no
// credential to bring there, and an agent gets through from any origin. The
// REST routes, where a browser's cookie rides along, are still guarded.
func TestMCPTakesAnAgentFromAnyOrigin(t *testing.T) {
	a := hardened(t, nil, nil, nil)
	sato := a.tokenFor(a.c.Sato)
	for _, h := range [][]string{
		{"Origin", "https://claude.ai"},
		{"Origin", "https://claude.ai", "Sec-Fetch-Site", "cross-site"},
		{"Sec-Fetch-Site", "cross-site"},
	} {
		headers := append([]string{"Authorization", "Bearer " + sato, "Accept", "application/json, text/event-stream"}, h...)
		res, body := a.raw("POST", a.srv.URL+httpapi.MCPPath, "application/json",
			[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), headers...)
		if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"tools"`) {
			t.Errorf("MCP with %v: %d %.300s", h, res.StatusCode, body)
		}
	}
	// Without its token, it is refused for that, from anywhere.
	res, body := a.raw("POST", a.srv.URL+httpapi.MCPPath, "application/json",
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), "Origin", "https://claude.ai", "Accept", "application/json, text/event-stream")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("MCP with no token: %d %.300s", res.StatusCode, body)
	}
	// The REST routes still refuse a page on another site.
	if r := a.do(nil, "POST", "/v1/auth/logout", sato, nil, "Origin", "https://claude.ai", "Sec-Fetch-Site", "cross-site"); r.Status != http.StatusForbidden {
		t.Errorf("REST from another site: %d %s", r.Status, r.Raw)
	}
}

// An agent harness that runs in a browser, or in an app's web view, asks
// first whether it may send /mcp its Authorization header (a preflight),
// and reads the answers only if they say it may. /mcp says yes to any
// origin, without credentials: it takes a bearer token alone, so no origin
// gets anything from it without one. The REST routes still answer only
// their own front end.
func TestMCPAnswersABrowsersPreflight(t *testing.T) {
	a := hardened(t, nil, nil, nil)
	sato := a.tokenFor(a.c.Sato)
	req, err := http.NewRequest(http.MethodOptions, a.srv.URL+httpapi.MCPPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://claude.ai")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type, mcp-protocol-version")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	h := res.Header
	if res.StatusCode != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(strings.ToLower(h.Get("Access-Control-Allow-Headers")), "authorization") ||
		!strings.Contains(strings.ToLower(h.Get("Access-Control-Allow-Headers")), "mcp-protocol-version") ||
		!strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") || h.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight: %d %v", res.StatusCode, h)
	}
	// The call itself, from the same origin: its answer may be read.
	got, body := a.raw("POST", a.srv.URL+httpapi.MCPPath, "application/json", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		"Authorization", "Bearer "+sato, "Accept", "application/json, text/event-stream", "Origin", "https://claude.ai")
	if got.StatusCode != http.StatusOK || got.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(got.Header.Get("Access-Control-Expose-Headers"), "Mcp-Session-Id") {
		t.Fatalf("call: %d %v %.200s", got.StatusCode, got.Header, body)
	}
	// A REST route still answers another origin nothing a browser would let it read.
	if r := a.do(nil, "GET", "/v1/me", sato, nil, "Origin", "https://claude.ai"); r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("REST gave another origin CORS headers: %v", r.Header)
	}
}
