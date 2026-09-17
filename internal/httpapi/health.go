package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/version"
)

type healthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	SchemaVersion uint   `json:"schema_version"`
	SchemaLatest  uint   `json:"schema_latest"`
	Error         string `json:"error,omitempty"`
}

// healthz is 200 only when the database answers and its schema is exactly the
// one this binary was built for.
func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	resp := healthResponse{
		Status:       "ok",
		Version:      version.Version,
		Commit:       version.Commit,
		SchemaLatest: s.LatestSchema,
	}
	code := http.StatusOK

	v, dirty, err := db.SchemaVersion(ctx, s.Pool)
	resp.SchemaVersion = v
	switch {
	case err != nil:
		resp.Status, resp.Error, code = "unavailable", "database unreachable", http.StatusServiceUnavailable
	case dirty:
		resp.Status, resp.Error, code = "unavailable", "schema is dirty", http.StatusServiceUnavailable
	case v != s.LatestSchema:
		resp.Status, resp.Error, code = "unavailable", "schema version mismatch; run `aishiterud migrate up`", http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}
