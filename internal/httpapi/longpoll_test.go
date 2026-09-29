package httpapi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/mcpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
)

// waitingAPI is the API as serve builds it, on a pipeline whose reads may
// wait for news (wait_s).
func waitingAPI(t *testing.T, adjust func(*httpapi.Deps)) (*api, *wake.Hub) {
	t.Helper()
	c, hub := testkit.NewCS101Waking(t, 1, wake.DefaultConfig)
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewAuthenticator(c.Pool, time.Hour)
	deps := httpapi.Deps{Pool: c.Pool, LatestSchema: latest, Pipeline: c.P, Auth: authn,
		MCP: mcpapi.NewHandler(mcpapi.Deps{Pipeline: c.P, Auth: authn})}
	if adjust != nil {
		adjust(&deps)
	}
	srv := httptest.NewServer(httpapi.NewHandler(deps))
	t.Cleanup(srv.Close)
	return &api{t: t, c: c, srv: srv}, hub
}

// feedPath is the course's feed from far past its end: nothing to read.
func feedPath(a *api, waitS int) string {
	return fmt.Sprintf("/v1/courses/%s/events?since_seq=%d&wait_s=%d", a.c.Course, int64(1)<<40, waitS)
}

// A request is bounded (BodyTimeout), and a call that waits for news is let
// wait out its time and answer, however short the bound.
func TestAWaitOutlastsTheRequestsBound(t *testing.T) {
	a, _ := waitingAPI(t, func(d *httpapi.Deps) { d.BodyTimeout = 300 * time.Millisecond })
	start := time.Now()
	r := a.do(nil, "GET", feedPath(a, 1), a.tokenFor(a.c.Sato), nil)
	if waited := time.Since(start); r.Status != http.StatusOK || waited < time.Second {
		t.Fatalf("a wait of 1s under a bound of 300ms: %d after %v: %s", r.Status, waited, r.Raw)
	}
}

// However often a call that waits reads again, it is one call to the rate
// limit.
func TestAWaitIsOneCall(t *testing.T) {
	now := time.Now()
	calls := ratelimit.New(60, 2)
	calls.SetClock(func() time.Time { return now })
	a, hub := waitingAPI(t, func(d *httpapi.Deps) { d.Calls = calls })
	token := a.tokenFor(a.c.Sato)
	done := make(chan response, 1)
	go func() { done <- a.do(nil, "GET", feedPath(a, 2), token, nil) }()
	for hub.Waiting() != 1 {
		time.Sleep(5 * time.Millisecond)
	}
	for range 3 { // news that is none to the call: it reads again, and waits on
		hub.Publish(wake.Note{CourseID: a.c.Course, Kind: "course.updated"})
		time.Sleep(50 * time.Millisecond)
	}
	if r := <-done; r.Status != http.StatusOK {
		t.Fatalf("the wait: %d %s", r.Status, r.Raw)
	}
	if r := a.do(nil, "GET", feedPath(a, 0), token, nil); r.Status != http.StatusOK {
		t.Fatalf("the second call: %d %s", r.Status, r.Raw)
	}
	if r := a.do(nil, "GET", feedPath(a, 0), token, nil); r.Status != http.StatusTooManyRequests {
		t.Fatalf("the third call, past a burst of two: %d %s", r.Status, r.Raw)
	}
}
