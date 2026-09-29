package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/httpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// An agent that writes to its memory faster than it may is refused with
// 429, like a caller over the per-actor limit, and told when to try again;
// unlike that caller's, its call was attempted, and is on record.
func TestAMemoryWriteTooSoonSaysWhenToTryAgain(t *testing.T) {
	c := testkit.NewCS101WithDeps(t, 0, func(d *tools.Deps) { d.Memory.Enabled, d.Memory.WritesPerHour = true, 1 })
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.NewHandler(httpapi.Deps{Pool: c.Pool, LatestSchema: latest, Pipeline: c.P,
		Auth: auth.NewAuthenticator(c.Pool, time.Hour)}))
	t.Cleanup(srv.Close)
	a := &api{t: t, c: c, srv: srv}
	helper := a.tokenFor(c.OwnedAgent(c.Sato, "Sato's helper"))

	first := a.do(nil, "POST", "/v1/me/memory/entries", helper, m{"scope": "owner", "text": "Sato marks on Sundays."}, "Idempotency-Key", "m1")
	if first.Status != http.StatusOK || first.str("result", "memory_id") == "" || first.str("result", "text") != "" {
		t.Fatalf("a write: %d %s", first.Status, first.Raw)
	}
	for _, key := range []string{"m2", "m2"} { // and its replay, from the record
		again := a.do(nil, "POST", "/v1/me/memory/entries", helper, m{"scope": "owner", "text": "Sato marks in green."}, "Idempotency-Key", key)
		secs, err := strconv.Atoi(again.Header.Get("Retry-After"))
		if again.Status != http.StatusTooManyRequests || again.str("error", "details", "reason") != "memory_write_rate" ||
			again.str("action_id") == "" || err != nil || secs < 1 || secs > 3600 {
			t.Fatalf("a second write within the hour: %d %v %s", again.Status, again.Header, again.Raw)
		}
	}
}
