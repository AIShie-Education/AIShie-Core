package mcpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// Every call pays for what screened and bounded do, so neither may cost
// much more than the request and its answer already do. A request is read
// once, into a buffer of its size, and looked at where it lies; a result is
// passed on as the SDK writes it, not held and read again. Only what may be
// a refusal is held.
func TestTheScreensCostLittleMoreThanWhatTheyScreen(t *testing.T) {
	body := []byte(`{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "submission_create", "arguments": {"body": "` +
		strings.Repeat("a", maxBody-200) + `"}}}`)
	h := screened(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serve := func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body)))
	}
	serve()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const runs = 4
	for range runs {
		serve()
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > uint64(len(body))*3/2 {
		t.Errorf("%d bytes allocated to screen a request of %d", per, len(body))
	}

	result := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"` + strings.Repeat("a", 1<<20) + `"}]}}`)
	refusal := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"` + strings.Repeat("a", 1<<20) + `"}}`)
	for _, tc := range []struct {
		what   string
		answer []byte
		held   bool
	}{
		{"a result", result, false},
		{"a refusal", refusal, true},
	} {
		rec := httptest.NewRecorder()
		bounded(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(tc.answer)
			if held := rec.Body.Len() == 0; held != tc.held {
				t.Errorf("%s: held %v, want %v", tc.what, held, tc.held)
			}
		})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
		if got := rec.Body.Bytes(); rec.Code != http.StatusOK || !tc.held && !bytes.Equal(got, tc.answer) || tc.held && len(got) > 1<<10 {
			t.Errorf("%s: %d, %d bytes: %.100s", tc.what, rec.Code, len(got), got)
		}
	}
}
