package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/httpapi"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/testdb"
)

func get(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestHealthz(t *testing.T) {
	latest, err := db.LatestEmbedded()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("migrated database is healthy", func(t *testing.T) {
		pool := testdb.New(t)
		code, body := get(t, httpapi.NewMux(httpapi.Deps{Pool: pool, LatestSchema: latest}), "/healthz")
		if code != http.StatusOK || body["status"] != "ok" {
			t.Fatalf("got %d %v", code, body)
		}
		if got := uint(body["schema_version"].(float64)); got != latest {
			t.Fatalf("schema_version = %d, want %d", got, latest)
		}
	})

	t.Run("unmigrated database is not", func(t *testing.T) {
		pool, _ := testdb.NewEmpty(t)
		code, body := get(t, httpapi.NewMux(httpapi.Deps{Pool: pool, LatestSchema: latest}), "/healthz")
		if code != http.StatusServiceUnavailable || body["status"] != "unavailable" {
			t.Fatalf("got %d %v", code, body)
		}
	})

	t.Run("binary older than the schema is not", func(t *testing.T) {
		pool := testdb.New(t)
		code, _ := get(t, httpapi.NewMux(httpapi.Deps{Pool: pool, LatestSchema: latest + 1}), "/healthz")
		if code != http.StatusServiceUnavailable {
			t.Fatalf("got %d, want 503", code)
		}
	})
}
