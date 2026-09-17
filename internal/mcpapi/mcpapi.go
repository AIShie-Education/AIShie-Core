// Package mcpapi is the MCP adapter: how agents reach the tool layer.
//
// It is the counterpart of httpapi and as thin. Every tool in the registry
// becomes an MCP tool with the same name (dots turned to underscores), the
// same JSON Schema and the same description; every call goes through the same
// pipeline, as the actor the bearer token belongs to. Nothing is decided here.
//
// An agent connects in. Core never dials out, keeps no session state about
// the agent, and pushes nothing: the transport is stateless streamable HTTP,
// and an agent finds work by asking — the approval queues, the event feed.
package mcpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/auth"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/pipeline"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/ratelimit"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/version"
)

// IdempotencyKey is the argument every state-changing MCP tool takes in
// addition to its own. REST carries the same thing in a header.
const IdempotencyKey = "idempotency_key"

// instructions is what a connecting agent is told about the whole server.
// A model reads this once and the tool descriptions many times, so it says
// only what no single tool can.
const instructions = `AIshiteru Core is a learning management system in which you are a member of courses, like the people in them. What you may do is set per course, per kind of action, on your membership; it does not depend on your being an agent.

Start with me_memberships: it lists the courses you are seated in and your member_id in each. Every other tool takes a course_id.

Every tool that changes something takes an idempotency_key: any string you choose, unique to the request. If a call times out, retry it with the SAME key and arguments — you will get the original outcome and nothing will happen twice. Use a NEW key only for a genuinely new request. Reusing a key with different arguments is refused.

Every result has a status:
- executed: done.
- proposed: NOT done. Your permission for this action requires a person's confirmation first, so it has been queued for one. This is normal and is not an error; do not retry it under a new key. Note the action_id and carry on. You learn the decision from event_list (action.approved, action.rejected or action.cancelled carrying that action_id) or action_list_mine.
- denied: you are not permitted to do this here. The attempt is on record. Retrying will not help.
- failed: permitted, but a rule prevented it; the error says which.

Nothing is pushed to you. Poll event_list with the next_seq it last returned to learn what has happened in a course. Events carry ids, not content: fetch what they point to with the read tools.

Files do not travel through tool calls. To attach one, call document_upload_url, PUT the bytes to the URL it returns, then pass the upload_token to the tool that attaches it. To read one, document_get returns a short-lived download_url.

You keep your own memory; this server keeps none for you. member_id is the stable handle for "you in this course" to key it on. If you are removed and seated again you get a new member_id and start afresh.`

type Deps struct {
	Pipeline *pipeline.Pipeline
	Auth     *auth.Authenticator
	Log      *slog.Logger
	// Calls is the per-actor limit, shared with REST: one actor, one
	// allowance, whichever door it uses. Nil means no limit.
	Calls *ratelimit.Limiter
}

// NewHandler returns the handler to mount at /mcp.
func NewHandler(d Deps) http.Handler {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	server := newServer(d)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		// No session survives a request, so any instance can serve any call
		// and nothing needs to be sticky. Nothing is lost by it: this server
		// never initiates anything towards a client.
		Stateless:    true,
		JSONResponse: true,
	})
	// The same verification path as REST. It runs on every request, so a
	// revoked token stops working on the agent's very next call.
	verify := func(ctx context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		p, err := d.Auth.Authenticate(ctx, token)
		if err != nil {
			if apperr.Is(err, apperr.Unauthenticated) {
				return nil, fmt.Errorf("%w: %s", sdkauth.ErrInvalidToken, "the credential is missing or not valid")
			}
			return nil, err
		}
		info := &sdkauth.TokenInfo{UserID: p.ActorID.String(), Extra: map[string]any{"credential_id": p.CredentialID.String()}}
		if p.ExpiresAt != nil {
			info.Expiration = *p.ExpiresAt
		}
		return info, nil
	}
	// An API token need not expire; it is revoked instead.
	return sdkauth.RequireBearerToken(verify, &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(limited(d, handler))
}

// limited refuses an actor that is calling too fast, before anything is
// attempted or recorded. It is what stops an agent stuck in a loop from
// writing a denied action per iteration for as long as it likes.
func limited(d Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info := sdkauth.TokenInfoFromContext(r.Context()); info != nil {
			if ok, wait := d.Calls.Allow(info.UserID); !ok {
				secs := int(wait.Seconds()) + 1
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": apperr.New(apperr.RateLimited,
					"too many calls; try again in %d seconds", secs).With("retry_after_seconds", secs)})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func newServer(d Deps) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "aishiteru-core", Title: "AIshiteru Core", Version: version.Version},
		&mcp.ServerOptions{Instructions: instructions})
	seen := map[string]string{}
	for _, t := range d.Pipeline.Registry().Exposed() {
		name := ToolName(t.Name)
		if other, dup := seen[name]; dup {
			panic(fmt.Sprintf("mcpapi: %s and %s are both %s over MCP", t.Name, other, name))
		}
		seen[name] = t.Name

		readOnly := t.Kind == tool.Read
		closed := false
		server.AddTool(&mcp.Tool{
			Name:         name,
			Description:  t.Description,
			InputSchema:  inputSchema(t),
			OutputSchema: outputSchema(t),
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint: readOnly,
				// True of every write, and guaranteed rather than hinted:
				// that is what the idempotency key is for.
				IdempotentHint: true,
				OpenWorldHint:  &closed,
			},
		}, handle(d, t))
	}
	return server
}

// ToolName is a tool's name over MCP. Several model APIs restrict tool names
// to letters, digits, underscores and hyphens, so grade.submit is offered as
// grade_submit. The action log still says grade.submit: the name on the wire
// is the adapter's business, the action type is not.
func ToolName(registryName string) string { return strings.ReplaceAll(registryName, ".", "_") }

// inputSchema is the tool's own schema; for a Write, plus the idempotency key.
func inputSchema(t tool.Tool) json.RawMessage {
	raw, err := json.Marshal(t.InputSchema)
	if err != nil {
		panic(fmt.Sprintf("mcpapi: %s: input schema: %v", t.Name, err))
	}
	if t.Kind == tool.Read {
		return raw
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		panic(fmt.Sprintf("mcpapi: %s: input schema: %v", t.Name, err))
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props[IdempotencyKey] = map[string]any{
		"type": "string", "minLength": 1, "maxLength": pipeline.MaxIdempotencyKeyLen,
		"description": "Any string unique to this request. Retrying after a timeout with the SAME key and arguments returns " +
			"the original outcome and does nothing twice. Use a new key only for a new request.",
	}
	s["properties"] = props
	required, _ := s["required"].([]any)
	s["required"] = append(required, IdempotencyKey)
	out, _ := json.Marshal(s)
	return out
}

// outputSchema describes the envelope every call returns, with the tool's own
// output as its result.
func outputSchema(t tool.Tool) json.RawMessage {
	result, err := json.Marshal(t.OutputSchema)
	if err != nil {
		panic(fmt.Sprintf("mcpapi: %s: output schema: %v", t.Name, err))
	}
	out, _ := json.Marshal(map[string]any{
		"type":     "object",
		"required": []string{"status"},
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"executed", "proposed", "denied", "failed", "rejected", "cancelled", "error"},
				"description": "executed: done. proposed: queued for a person's confirmation, not done. denied, failed: not done. error: the call was never attempted."},
			"action_id":    map[string]any{"type": "string", "format": "uuid", "description": "the recorded action; absent for reads"},
			"review_state": map[string]any{"type": "string"},
			"replayed":     map[string]any{"type": "boolean", "description": "true when this is the stored outcome of an earlier call with the same idempotency_key"},
			"note":         map[string]any{"type": "string", "description": "what to do next, when that is not obvious"},
			"result":       json.RawMessage(result),
			"error": map[string]any{"type": "object", "properties": map[string]any{
				"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "details": map[string]any{"type": "object"}}},
		},
	})
	return out
}

// envelope is what an agent gets back from every call.
type envelope struct {
	pipeline.Outcome
	Note string `json:"note,omitempty"`
}

const proposedNote = "Not executed. This action needs a person's confirmation and has been queued as the action_id above. " +
	"This is the normal outcome at your permission level, not an error: do not retry it under a new idempotency key. " +
	"Carry on with other work, and look for action.approved, action.rejected or action.cancelled carrying this action_id " +
	"in event_list, or check action_list_mine."

func handle(d Deps, t tool.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// A protocol error is for what the model cannot act on. Everything
		// else comes back as a result it can read and correct itself from.
		if req.Extra == nil || req.Extra.TokenInfo == nil {
			return nil, fmt.Errorf("no authenticated caller")
		}
		actorID, err := uuid.Parse(req.Extra.TokenInfo.UserID)
		if err != nil {
			return nil, fmt.Errorf("no authenticated caller")
		}
		args, key, err := splitKey(req.Params.Arguments, t.Kind == tool.Write)
		if err != nil {
			return failed(apperr.Invalid("%v", err)), nil
		}
		out, err := d.Pipeline.Invoke(ctx, pipeline.Caller{ActorID: actorID}, t.Name, args, key)
		if err != nil {
			e, ok := apperr.As(err)
			if !ok {
				d.Log.Error("internal error", "tool", t.Name, "err", err)
				e = &apperr.Error{Code: "internal", Message: "something went wrong on our side; retry with the same idempotency_key"}
			}
			return failed(e), nil
		}
		env := envelope{Outcome: out}
		if out.Status == domain.StatusProposed {
			env.Note = proposedNote
		}
		return result(env, out.Status != domain.StatusExecuted && out.Status != domain.StatusProposed), nil
	}
}

// splitKey takes the idempotency key out of the arguments, which then match
// the tool's own schema exactly as a REST body would.
func splitKey(raw json.RawMessage, write bool) ([]byte, string, error) {
	// A client calling a tool that takes nothing may send no arguments at all,
	// or an explicit null. Both mean the empty object.
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		raw = json.RawMessage("{}")
	}
	if !write {
		return raw, "", nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, "", fmt.Errorf("arguments must be a JSON object")
	}
	var key string
	if k, ok := args[IdempotencyKey]; ok {
		if err := json.Unmarshal(k, &key); err != nil {
			return nil, "", fmt.Errorf("%s must be a string", IdempotencyKey)
		}
		delete(args, IdempotencyKey)
	}
	rest, err := json.Marshal(args)
	return rest, key, err
}

func failed(e *apperr.Error) *mcp.CallToolResult {
	return result(struct {
		Status string        `json:"status"`
		Error  *apperr.Error `json:"error"`
	}{"error", e}, true)
}

// result carries the envelope twice, as the protocol asks: structured, for
// clients that read it, and as JSON text, for models that read that.
func result(v any, isError bool) *mcp.CallToolResult {
	text, err := json.Marshal(v)
	if err != nil {
		text = []byte(`{"status":"error","error":{"code":"internal","message":"the result could not be encoded"}}`)
		isError = true
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
		StructuredContent: json.RawMessage(text),
		IsError:           isError,
	}
}
