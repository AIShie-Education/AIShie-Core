package mcpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/mcpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// serveWaking is serve on a pipeline whose reads may wait for news, with
// requests bounded as bound says.
func serveWaking(t *testing.T, bound time.Duration) (*fixture, *wake.Hub) {
	t.Helper()
	c, hub := testkit.NewCS101Waking(t, 1, wake.DefaultConfig)
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewAuthenticator(c.Pool, time.Hour)
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{
		Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: authn, BodyTimeout: bound,
		MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: c.P, Auth: authn}),
	}))
	t.Cleanup(srv.Close)
	return &fixture{c: c, srv: srv}, hub
}

// waitOnFeed is a call of event_list from far past the feed's end, which
// waits waitS seconds for news.
func waitOnFeed(f *fixture, waitS int) string {
	return fmt.Sprintf(`{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "event_list", "arguments": `+
		`{"course_id": "%s", "since_seq": %d, "wait_s": %d}}}`, f.c.Course, int64(1)<<40, waitS)
}

// Over MCP a call that waits is let wait out its time, however short the
// request's bound, and stops waiting when its client goes, though the SDK
// would let it run on.
func TestAWaitOverMCP(t *testing.T) {
	f, hub := serveWaking(t, 300*time.Millisecond)
	token := f.token(t, f.c.Grader)

	start := time.Now()
	status, out := post(t, f, token, waitOnFeed(f, 1))
	if waited := time.Since(start); status != http.StatusOK || waited < time.Second || !strings.Contains(string(out), `\"status\":\"executed\"`) {
		t.Fatalf("a wait of 1s under a bound of 300ms: %d after %v: %s", status, waited, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+httpapi.MCPPath, strings.NewReader(waitOnFeed(f, 20)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); hub.Waiting() != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the call never waited")
		}
	}
	cancel()
	<-gone
	for deadline := time.Now().Add(3 * time.Second); hub.Waiting() != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the call waits on for a client that has gone")
		}
	}
}
