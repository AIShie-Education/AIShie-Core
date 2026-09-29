package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/AIShie-Education/AIShie-Core/internal/db"
	"github.com/AIShie-Education/AIShie-Core/internal/version"
)

type healthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	SchemaVersion uint   `json:"schema_version"`
	SchemaLatest  uint   `json:"schema_latest"`
	Error         string `json:"error,omitempty"`
}

// healthz is 200 when the database answers and its schema is at least the one
// this binary was built for. A schema that is ahead is what a rolling deploy
// looks like from the old binary's side — migrations keep the previous
// release working — so it is healthy. A schema that is behind is not: the
// binary would issue statements the database does not understand.
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
	case v < s.LatestSchema:
		resp.Status, resp.Error, code = "unavailable", "the schema is behind this binary; run `aishiterud migrate up`", http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}
