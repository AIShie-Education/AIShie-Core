package mcpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/httpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/mcpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testkit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
)

type m = map[string]any

// bearer puts a token on every request, as an agent's MCP client is
// configured to.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

type fixture struct {
	c   *testkit.CS101
	srv *httptest.Server
}

// serve mounts MCP the way the real server does: on the same mux as REST,
// behind the same guard.
func serve(t *testing.T, students int) *fixture {
	t.Helper()
	c := testkit.NewCS101(t, students)
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewAuthenticator(c.Pool, time.Hour)
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: authn,
		MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: c.P, Auth: authn}),
	}))
	t.Cleanup(srv.Close)
	return &fixture{c: c, srv: srv}
}

func (f *fixture) token(t *testing.T, actor uuid.UUID) string {
	t.Helper()
	tok, _, err := auth.IssueToken(context.Background(), dbq.New(f.c.Pool), actor, "mcp", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok.Full
}

// connect opens a real MCP session with the SDK's own client.
func (f *fixture) connect(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.srv.URL + httpapi.MCPPath, HTTPClient: &http.Client{Transport: bearer{token}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

type envelope struct {
	Status   string          `json:"status"`
	ActionID *uuid.UUID      `json:"action_id"`
	Replayed bool            `json:"replayed"`
	Note     string          `json:"note"`
	Result   json.RawMessage `json:"result"`
	Error    *struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func call(t *testing.T, s *mcp.ClientSession, name string, args m) (envelope, *mcp.CallToolResult) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d content blocks", name, len(res.Content))
	}
	text := res.Content[0].(*mcp.TextContent).Text
	var env envelope
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("%s: text content is not the envelope: %s", name, text)
	}
	// The structured form and the text form are the same thing.
	structured, _ := json.Marshal(res.StructuredContent)
	var again envelope
	if json.Unmarshal(structured, &again) != nil || again.Status != env.Status {
		t.Fatalf("%s: structured content %s disagrees with text %s", name, structured, text)
	}
	return env, res
}

// Arguments name each key once, at the top level too, where an MCP write's
// idempotency key is taken out of them.
func TestArgumentsNameEachKeyOnce(t *testing.T) {
	f := serve(t, 1)
	c, yuki := f.c, f.c.Students[0]
	sato := f.connect(t, f.token(t, c.Sato))
	raw := json.RawMessage(`{"course_id": "` + c.Course.String() + `", "submission_id": "` + yuki.HW3.String() +
		`", "score": 90, "score": 10, "idempotency_key": "twice"}`)
	res, err := sato.CallTool(context.Background(), &mcp.CallToolParams{Name: "grade_submit", Arguments: raw})
	if err != nil {
		t.Fatal(err)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !res.IsError || !strings.Contains(text, "invalid_argument") {
		t.Fatalf("a repeated key: %s", text)
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 0 {
		t.Fatal("something was recorded")
	}
}

// post sends body to the MCP endpoint as it is, header pairs after it, as
// a client that is not the SDK's might, and returns the status and body.
func post(t *testing.T, f *fixture, token, body string, headers ...string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+httpapi.MCPPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

// One request, one message. The per-actor limit counts requests, so a batch
// would be as many calls as it held for the price of one: it is refused
// whole, and nothing in it is attempted.
func TestARequestCarriesOneMessage(t *testing.T) {
	f := serve(t, 1)
	c, yuki := f.c, f.c.Students[0]
	grade := func(id, key string) string {
		return `{"jsonrpc": "2.0", "id": ` + id + `, "method": "tools/call", "params": {"name": "grade_submit", "arguments": {"course_id": "` +
			c.Course.String() + `", "submission_id": "` + yuki.HW3.String() + `", "score": 90, "idempotency_key": "` + key + `"}}}`
	}
	status, out := post(t, f, f.token(t, c.Sato), " ["+grade("1", "one")+", "+grade("2", "two")+"]")
	if status != http.StatusBadRequest || !strings.Contains(string(out), "a batch is not taken") {
		t.Fatalf("a batch: %d %s", status, out)
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 0 {
		t.Fatalf("%d actions recorded", n)
	}
	if status, out := post(t, f, f.token(t, c.Sato), grade("1", "one")); status != http.StatusOK || !strings.Contains(string(out), `\"status\":\"executed\"`) {
		t.Fatalf("the same call alone: %d %s", status, out)
	}
}

// A request the SDK would refuse for its headers is refused for them, as
// the SDK words it, however large it is; one that is only too large is
// refused in the SDK's words too. That the body is read before the SDK
// sees it changes neither.
func TestARequestIsRefusedAsTheSDKWouldRefuseIt(t *testing.T) {
	f := serve(t, 1)
	token := f.token(t, f.c.Sato)
	big := `{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {"x": "` + strings.Repeat("a", 5<<20) + `"}}`
	for _, tc := range []struct {
		what    string
		headers []string
		status  int
		says    string
	}{
		{"not JSON", []string{"Content-Type", "text/plain"}, http.StatusUnsupportedMediaType, "Content-Type must be"},
		{"no stream accepted", []string{"Accept", "application/json"}, http.StatusBadRequest, "Accept must contain"},
		{"a version there is not", []string{"Mcp-Protocol-Version", "2000-01-01"}, http.StatusBadRequest, "Unsupported protocol version"},
		{"too large", nil, http.StatusRequestEntityTooLarge, "request body exceeds"},
	} {
		if status, out := post(t, f, token, big, tc.headers...); status != tc.status || !strings.Contains(string(out), tc.says) {
			t.Errorf("%s: %d %.300s", tc.what, status, out)
		}
	}
}

// recorder is bearer, noting the method of each message the client posts.
type recorder struct {
	bearer
	mu   sync.Mutex
	sent []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		r.mu.Lock()
		r.sent = append(r.sent, msg.Method)
		r.mu.Unlock()
	}
	return r.bearer.RoundTrip(req)
}

// Nothing is pushed from here, and nothing says otherwise. Tools are all
// that is offered, and their list is not said to change, so a client that
// would listen for that does not ask; a listen asked for all the same is
// answered as SEP-2575 answers a method a server does not have: not found,
// to the id that asked.
func TestNothingIsOfferedThatIsNotPushed(t *testing.T) {
	f := serve(t, 1)
	token := f.token(t, f.c.Sato)
	rec := &recorder{bearer: bearer{token}}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0"},
		&mcp.ClientOptions{ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {}})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.srv.URL + httpapi.MCPPath, HTTPClient: &http.Client{Transport: rec},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	if caps, _ := json.Marshal(session.InitializeResult().Capabilities); string(caps) != `{"tools":{}}` {
		t.Errorf("offered: %s", caps)
	}
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	if slices.Contains(rec.sent, "subscriptions/listen") {
		t.Errorf("the client listened: %v", rec.sent)
	}
	rec.mu.Unlock()

	status, out := post(t, f, token, `{"jsonrpc": "2.0", "id": 7, "method": "subscriptions/listen", "params": {"notifications": {"toolsListChanged": true}, `+
		`"_meta": {"io.modelcontextprotocol/protocolVersion": "2026-07-28"}}}`, "Mcp-Protocol-Version", "2026-07-28")
	var answer struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &answer); err != nil || status != http.StatusNotFound || string(answer.ID) != "7" || answer.Error.Code != -32601 {
		t.Errorf("a listen: %d %s", status, out)
	}
	// The id goes back as it came, not escaped for HTML.
	id := `"` + strings.Repeat("<", 200) + `"`
	status, out = post(t, f, token, `{"jsonrpc": "2.0", "id": `+id+`, "method": "subscriptions/listen", "params": {"notifications": {"toolsListChanged": true}, `+
		`"_meta": {"io.modelcontextprotocol/protocolVersion": "2026-07-28"}}}`, "Mcp-Protocol-Version", "2026-07-28")
	if err := json.Unmarshal(out, &answer); err != nil || status != http.StatusNotFound || string(answer.ID) != id {
		t.Errorf("a listen with an id of 200 '<': %d %.300s", status, out)
	}
}

// Over MCP a refusal of ours is written twice, as text and as structured
// content, and the text is escaped once more on the way: a megabyte of input
// would come back as thirteen. The SDK words some refusals itself — a tool
// or a method it does not know, params that do not decode, a protocol
// version the header does not match — quoting what it refuses with %q, which
// makes each DEL four bytes before JSON escapes the backslash. Every one is
// held to a few kilobytes all the same and still says what was wrong; what
// could not be cut short — an id, which every answer repeats, or a listen,
// whose answer is a stream — is not taken.
func TestARefusalOverMCPRepeatsLittleOfWhatItRefuses(t *testing.T) {
	f := serve(t, 1)
	c, yuki := f.c, f.c.Students[0]
	token := f.token(t, yuki.Actor)
	del := strings.Repeat("\x7f", 1<<20)
	for _, tc := range []struct {
		what, body, version string
		status              int
		says                string
	}{
		{"a score that is no number", `{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "grade_submit", "arguments": {"course_id": "` +
			c.Course.String() + `", "submission_id": "` + yuki.HW3.String() + `", "score": "` + strings.Repeat("<", 1<<20) + `", "idempotency_key": "big"}}}`,
			"", http.StatusOK, "/properties/score"},
		{"a tool there is not", `{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "` + del + `", "arguments": {}}}`,
			"", http.StatusOK, "unknown tool"},
		{"a method there is not", `{"jsonrpc": "2.0", "id": 1, "method": "` + del + `", "params": {}}`,
			"", http.StatusBadRequest, "JSON RPC not handled"},
		{"params that do not decode", `{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": 5, "arguments": {"x": "` + del + `"}}}`,
			"", http.StatusOK, "unmarshaling"},
		{"a version the header does not match", `{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {"_meta": {"io.modelcontextprotocol/protocolVersion": "` + del + `"}}}`,
			"2025-06-18", http.StatusBadRequest, "does not match"},
		{"an id", `{"jsonrpc": "2.0", "id": "` + del + `", "method": "tools/list"}`,
			"", http.StatusBadRequest, "the id is longer than"},
		{"a listen", `{"jsonrpc": "2.0", "id": 1, "method": "subscriptions/listen", "params": {"notifications": 5, "x": "` + del + `"}}`,
			"", http.StatusNotFound, "poll event_list"},
	} {
		var headers []string
		if tc.version != "" {
			headers = []string{"Mcp-Protocol-Version", tc.version}
		}
		status, out := post(t, f, token, tc.body, headers...)
		if status != tc.status || len(out) > 8<<10 || !strings.Contains(string(out), tc.says) {
			t.Errorf("%s: %d, %d bytes: %.300q", tc.what, status, len(out), out)
		}
	}
}

// docs/schema.md §5 once more, this time as the agent actually lives it: an
// MCP session, a bearer token, and tool calls.
func TestAnAgentGradesAnEssayOverMCP(t *testing.T) {
	f := serve(t, 1)
	c, yuki := f.c, f.c.Students[0]
	agent := f.connect(t, f.token(t, c.Grader))
	sato := f.connect(t, f.token(t, c.Sato))

	// Cold start: where am I?
	me, _ := call(t, agent, "me_memberships", nil)
	if me.Status != "executed" || !strings.Contains(string(me.Result), c.GraderM.String()) {
		t.Fatalf("me_memberships: %+v", me)
	}

	grade := m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 85, "feedback": "Clear thesis.", "idempotency_key": "yuki-hw3"}
	proposed, res := call(t, agent, "grade_submit", grade)
	// A proposal is a normal result, not an error: the model must not be
	// told it failed, or it will try again some other way.
	if proposed.Status != "proposed" || res.IsError || proposed.ActionID == nil {
		t.Fatalf("grade_submit: %+v isError=%v", proposed, res.IsError)
	}
	if !strings.Contains(proposed.Note, "event_list") || !strings.Contains(proposed.Note, "do not retry") {
		t.Fatalf("the proposal does not tell the agent what to do next: %q", proposed.Note)
	}
	if n := c.Count(`SELECT count(*) FROM grade`); n != 0 {
		t.Fatal("a proposal wrote a grade")
	}
	// The action log records the tool's real name and none of the transport.
	if n := c.Count(`SELECT count(*) FROM action WHERE id = $1 AND action_type = 'grade.submit' AND idempotency_key = 'yuki-hw3'
		AND NOT payload ? 'idempotency_key' AND payload->>'score' = '85'`, *proposed.ActionID); n != 1 {
		t.Fatal("the action row is not what REST would have written for the same call")
	}

	// A timeout and a retry: the same answer, nothing done twice.
	retry, res := call(t, agent, "grade_submit", grade)
	if !retry.Replayed || retry.Status != "proposed" || *retry.ActionID != *proposed.ActionID || res.IsError {
		t.Fatalf("retry: %+v", retry)
	}
	// The same key for different content: an error the model can read.
	grade["score"] = 60
	clash, res := call(t, agent, "grade_submit", grade)
	if clash.Status != "error" || clash.Error.Code != "idempotency_conflict" || !res.IsError {
		t.Fatalf("same key, different content: %+v", clash)
	}
	// The agent may not decide. Denied is an error result, and on record.
	denied, res := call(t, agent, "action_decide", m{"course_id": c.Course, "action_id": proposed.ActionID, "decision": "approve", "idempotency_key": "self"})
	if denied.Status != "denied" || !res.IsError || denied.ActionID == nil || denied.Error.Details["reason"] != "permission_denied" {
		t.Fatalf("agent deciding: %+v", denied)
	}

	// A person approves — here over MCP too; the door does not care who.
	decided, res := call(t, sato, "action_decide", m{"course_id": c.Course, "action_id": proposed.ActionID, "decision": "approve", "idempotency_key": "ok"})
	if decided.Status != "executed" || res.IsError || !strings.Contains(string(decided.Result), `"outcome":"executed"`) {
		t.Fatalf("decide: %+v", decided)
	}
	if n := c.Count(`SELECT count(*) FROM grade WHERE score = 85 AND created_by_action_id = $1 AND grader_member_id = $2`, *proposed.ActionID, c.GraderM); n != 1 {
		t.Fatal("the approved grade was not written as the agent's, under its proposal")
	}

	// The agent learns of it by asking. Nothing was pushed.
	feed, _ := call(t, agent, "event_list", m{"course_id": c.Course})
	var events struct {
		Events []struct {
			Type     string    `json:"type"`
			ActionID uuid.UUID `json:"action_id"`
		} `json:"events"`
	}
	if err := json.Unmarshal(feed.Result, &events); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events.Events {
		found = found || (e.Type == "action.approved" && e.ActionID == *proposed.ActionID)
	}
	if !found {
		t.Fatalf("no action.approved for its proposal in the agent's feed: %+v", events.Events)
	}
	// Its original call, replayed now, says executed.
	grade["score"] = 85
	if after, _ := call(t, agent, "grade_submit", grade); after.Status != "executed" || !after.Replayed {
		t.Fatalf("replay after approval: %+v", after)
	}
}

func TestErrorsAModelCanCorrect(t *testing.T) {
	f := serve(t, 1)
	c, yuki := f.c, f.c.Students[0]
	sato := f.connect(t, f.token(t, c.Sato))

	cases := []struct {
		name string
		tool string
		args m
		code string
	}{
		{"no idempotency key", "grade_submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 1}, "invalid_argument"},
		{"missing field", "grade_submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "idempotency_key": "a"}, "invalid_argument"},
		{"unknown field", "grade_submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 1, "sudo": true, "idempotency_key": "b"}, "invalid_argument"},
		{"key is not a string", "grade_submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 1, "idempotency_key": 7}, "invalid_argument"},
		{"unknown submission", "grade_submit", m{"course_id": c.Course, "submission_id": uuid.New(), "score": 1, "idempotency_key": "c"}, "not_found"},
		{"a key on a read", "me_get", m{"idempotency_key": "d"}, "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, res := call(t, sato, tc.tool, tc.args)
			if env.Status != "error" || env.Error == nil || env.Error.Code != tc.code || !res.IsError {
				t.Fatalf("%+v (isError=%v), want error %s", env, res.IsError, tc.code)
			}
		})
	}
	if n := c.Count(`SELECT count(*) FROM action`); n != 0 {
		t.Fatalf("%d actions recorded for calls that were never attempts", n)
	}
	// A rule of the domain: recorded, and explained.
	over, res := call(t, sato, "grade_submit", m{"course_id": c.Course, "submission_id": yuki.HW3, "score": 1000, "idempotency_key": "e"})
	if over.Status != "failed" || over.Error.Code != "failed_precondition" || !res.IsError || over.ActionID == nil {
		t.Fatalf("1000 out of 100: %+v", over)
	}
	// An unknown tool is the protocol's business, not a result.
	if _, err := sato.CallTool(context.Background(), &mcp.CallToolParams{Name: "grade_obliterate"}); err == nil {
		t.Fatal("calling a tool that does not exist returned a result")
	}
}

func TestOnlyAuthenticatedAgentsConnect(t *testing.T) {
	f := serve(t, 0)
	for name, token := range map[string]string{"no token": "", "not a token": "hunter2", "unknown token": "ais_aaaaaaaaaaaa_" + strings.Repeat("A", 43)} {
		client := mcp.NewClient(&mcp.Implementation{Name: "intruder", Version: "0"}, nil)
		if _, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
			Endpoint: f.srv.URL + httpapi.MCPPath, HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil); err == nil {
			t.Errorf("%s: connected", name)
		}
	}
	// Revocation takes effect on the very next call: there is no session to
	// keep a revoked token alive.
	token := f.token(t, f.c.Sato)
	s := f.connect(t, token)
	if env, _ := call(t, s, "me_get", nil); env.Status != "executed" {
		t.Fatalf("before revocation: %+v", env)
	}
	f.c.Exec(`UPDATE credential SET revoked_at = now() WHERE actor_id = $1`, f.c.Sato)
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "me_get"}); err == nil {
		t.Fatal("a revoked token still works on an open session")
	}
	// A suspended actor still authenticates, and is then denied — on record.
	f.c.Exec(`UPDATE actor SET status = 'suspended' WHERE id = $1`, f.c.Grader)
	g := f.connect(t, f.token(t, f.c.Grader))
	if env, res := call(t, g, "me_get", nil); env.Status != "denied" || !res.IsError {
		t.Fatalf("a suspended agent: %+v", env)
	}
}

// The catalogue an agent sees is the registry, no more and no less.
func TestToolsListIsTheRegistry(t *testing.T) {
	f := serve(t, 0)
	s := f.connect(t, f.token(t, f.c.Grader))

	if got := s.InitializeResult().Instructions; !strings.Contains(got, "idempotency_key") || !strings.Contains(got, "proposed") || !strings.Contains(got, "me_memberships") {
		t.Fatalf("the server's instructions do not explain the essentials:\n%s", got)
	}
	listed := map[string]*mcp.Tool{}
	for tl, err := range s.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		listed[tl.Name] = tl
	}
	exposed := f.c.P.Registry().Exposed()
	if len(listed) != len(exposed) {
		t.Fatalf("MCP lists %d tools, the registry exposes %d", len(listed), len(exposed))
	}
	// What several model APIs accept as a tool name.
	valid := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	for _, reg := range exposed {
		tl := listed[mcpapi.ToolName(reg.Name)]
		if tl == nil {
			t.Errorf("%s is not offered over MCP", reg.Name)
			continue
		}
		if !valid.MatchString(tl.Name) {
			t.Errorf("%q is not a tool name every model API accepts", tl.Name)
		}
		if tl.Description != reg.Description {
			t.Errorf("%s: description differs from the registry's", tl.Name)
		}
		schema, _ := json.Marshal(tl.InputSchema)
		var in struct {
			Required   []string       `json:"required"`
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(schema, &in)
		_, hasKey := in.Properties[mcpapi.IdempotencyKey]
		required := false
		for _, r := range in.Required {
			required = required || r == mcpapi.IdempotencyKey
		}
		write := reg.Kind == tool.Write
		if hasKey != write || required != write {
			t.Errorf("%s: write=%v but idempotency_key present=%v required=%v", tl.Name, write, hasKey, required)
		}
		if tl.Annotations == nil || tl.Annotations.ReadOnlyHint == write {
			t.Errorf("%s: readOnlyHint is wrong", tl.Name)
		}
		if tl.OutputSchema == nil {
			t.Errorf("%s: no output schema", tl.Name)
		}
	}
}

// A replay of a call that returned a secret comes back without it, and the
// schema the tool is listed with allows that: a client that checks every
// result against it, as the TypeScript and Python SDKs do, takes the replay
// as it took the first answer. The Go SDK's client does not check, so the
// test does.
func TestAReplayWithoutItsSecretMatchesTheListedSchema(t *testing.T) {
	f := serve(t, 0)
	s := f.connect(t, f.token(t, f.c.Sato))
	var listed *mcp.Tool
	for tl, err := range s.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tl.Name == "credential_issue_token" {
			listed = tl
		}
	}
	if listed == nil {
		t.Fatal("credential_issue_token is not listed")
	}
	raw, err := json.Marshal(listed.OutputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}

	args := m{"label": "ci", "idempotency_key": "once"}
	first, res := call(t, s, "credential_issue_token", args)
	if first.Status != "executed" || !strings.Contains(string(first.Result), `"token":"ais_`) {
		t.Fatalf("first call: %+v", first)
	}
	if err := resolved.Validate(res.StructuredContent); err != nil {
		t.Fatalf("the first answer does not match the listed schema: %v", err)
	}
	again, res := call(t, s, "credential_issue_token", args)
	if again.Status != "executed" || !again.Replayed || strings.Contains(string(again.Result), `"token":`) {
		t.Fatalf("replay: %+v", again)
	}
	if err := resolved.Validate(res.StructuredContent); err != nil {
		t.Fatalf("the replay does not match the listed schema: %v", err)
	}
}

// When the credential cannot be checked — the database is away — the agent
// gets a 500 with a fixed sentence, not the text of our error, which names
// hosts and databases; and not "invalid token", which would have it throw a
// perfectly good token away.
func TestAFaultCheckingTheCredentialIsNotDescribedToTheAgent(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	away, err := pgxpool.New(context.Background(), c.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	away.Close()
	var log strings.Builder
	authn := auth.NewAuthenticator(away, time.Hour)
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: authn,
		MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: c.P, Auth: authn, Log: slog.New(slog.NewTextHandler(&log, nil))}),
	}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+httpapi.MCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer ais_aaaaaaaaaaaa_"+strings.Repeat("A", 43))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusInternalServerError || strings.Contains(strings.ToLower(string(body)), "pool") || strings.Contains(string(body), "closed") || !strings.Contains(string(body), "could not be checked") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if !strings.Contains(log.String(), "closed pool") {
		t.Fatalf("the cause is not in the log: %s", log.String())
	}
}
