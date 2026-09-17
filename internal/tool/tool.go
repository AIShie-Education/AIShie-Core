// Package tool defines what a tool is. There is one tool layer: the REST API
// and the MCP server are both generated from the Registry, and every call
// from either goes through the same pipeline. A human's browser and an
// agent's MCP session end in the same function.
//
// A tool is declared with Define, which takes typed functions and erases them
// into a Tool the registry, the pipeline and the adapters can handle
// uniformly.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/events"
)

func init() {
	// Scores travel as JSON numbers, not strings. A decimal prints as a plain
	// decimal literal, so nothing is lost on the way out; on the way in both
	// forms are accepted and neither passes through a float.
	decimal.MarshalJSONWithoutQuotes = true
}

// Kind separates tools that change state from tools that only look.
//
// A Write is an action: it is recorded before it happens, denied attempts
// included, needs an idempotency key, and may become a proposal. A Read is
// authorized, scope included, and leaves no action row.
type Kind int

const (
	Read Kind = iota
	Write
)

// Gate says who may call a tool. Exactly one of the three is set.
type Gate struct {
	// Perms are course permissions; the call runs at the lowest of their
	// levels on the caller's membership. The tool's input carries course_id.
	Perms []domain.Perm
	// Platform lists platform roles, for the few operations outside any
	// course. No ladder applies: allowed outright, or not at all.
	Platform []string
	// Self marks a tool where an actor acts on its own account.
	Self bool
}

func (g Gate) CourseScoped() bool { return len(g.Perms) > 0 }

// Target is what a call acts on, as far as authorization and the action log
// need to know.
type Target struct {
	// CourseID is the course the target belongs to. For a course-scoped tool
	// it must equal the course_id in the input; a tool's Resolve looks its
	// target up within that course, so an id from another course is simply
	// not found.
	CourseID uuid.UUID
	// Type and ID are recorded on the action row.
	Type string
	ID   *uuid.UUID
	// Scope is steps 4 and 5 of authorize().
	Scope authz.Target
	// Perms, when set, replaces Gate.Perms for this call. Reading a document
	// is gated by a different permission depending on what kind it is.
	Perms []domain.Perm
}

// Route places a tool in the REST API.
type Route struct {
	Method  string // GET, POST, ...
	Pattern string // /v1/courses/{course_id}/grades
}

// ExecCtx is what a Write tool runs with. Everything it does goes through Tx,
// inside a savepoint: if the tool returns an error, all of it is undone and
// the attempt is recorded as failed.
type ExecCtx struct {
	Tx pgx.Tx
	Q  *dbq.Queries
	// Actor and Member are who the action is attributed to. When a proposal
	// is approved these are the proposer's, not the approver's. Member is nil
	// for platform, self and system calls.
	Actor    domain.Actor
	Member   *domain.Member
	ActionID uuid.UUID
	Now      time.Time
	// Emit queues an event. It is written, with ActionID filled in, only if
	// the action executes.
	Emit func(events.Event)
}

// ReadCtx is what a Read tool runs with.
type ReadCtx struct {
	Q      *dbq.Queries
	Actor  domain.Actor
	Member *domain.Member
	// Scope is the member's scope in the shape list queries take it, so that
	// filtering happens in SQL.
	Scope authz.ScopeFilter
	Now   time.Time
}

// Spec declares a tool with its real types.
type Spec[In, Out any] struct {
	// Name is noun.verb, and is the action_type recorded for a Write.
	Name        string
	Description string
	Kind        Kind
	Gate        Gate
	HTTP        Route
	// Internal tools are called only by the system actor's background jobs
	// and are exposed by neither adapter.
	Internal bool
	// SecretIn and SecretOut name top-level fields that must never be
	// stored: they are removed from the recorded payload (and so from the
	// payload hash) and from the recorded result.
	SecretIn  []string
	SecretOut []string

	// Resolve finds the target. Returning an apperr not_found ends the call
	// with no action row: there was nothing to attempt.
	Resolve func(ctx context.Context, q dbq.Querier, in In) (Target, error)
	// Validate checks the domain's rules without writing anything. It runs
	// before Execute, and before a proposal is queued, so that nobody is
	// asked to approve something that could never run.
	Validate func(ctx context.Context, q dbq.Querier, m *domain.Member, in In) error
	// Execute is set for a Write, Query for a Read.
	Execute func(ctx context.Context, ec *ExecCtx, in In) (Out, error)
	Query   func(ctx context.Context, rc *ReadCtx, in In) (Out, error)
}

// Tool is a Spec with its types erased.
type Tool struct {
	Name        string
	Description string
	Kind        Kind
	Gate        Gate
	HTTP        Route
	Internal    bool
	SecretIn    []string
	SecretOut   []string

	InputSchema  *jsonschema.Schema
	OutputSchema *jsonschema.Schema

	// Decode validates raw arguments against InputSchema and unmarshals
	// them. The value it returns is what the other functions take.
	Decode   func(raw []byte) (any, error)
	CourseID func(in any) uuid.UUID
	Resolve  func(ctx context.Context, q dbq.Querier, in any) (Target, error)
	Validate func(ctx context.Context, q dbq.Querier, m *domain.Member, in any) error
	Execute  func(ctx context.Context, ec *ExecCtx, in any) (any, error)
	Query    func(ctx context.Context, rc *ReadCtx, in any) (any, error)
}

// InCourse is embedded by the input of every course-scoped tool, so that the
// pipeline can find the course before it knows anything else.
type InCourse struct {
	CourseID uuid.UUID `json:"course_id" jsonschema:"the course this call is about"`
}

func (c InCourse) GetCourseID() uuid.UUID { return c.CourseID }

type courseScoped interface{ GetCourseID() uuid.UUID }

var nameRE = regexp.MustCompile(`^[a-z][a-z_]*\.[a-z][a-z_]*$`)

// Define checks a Spec and erases its types. It panics on a malformed
// declaration: tools are declared at start-up, and a server with a broken
// tool should not come up.
func Define[In, Out any](s Spec[In, Out]) Tool {
	fail := func(format string, args ...any) {
		panic(fmt.Sprintf("tool %q: %s", s.Name, fmt.Sprintf(format, args...)))
	}
	if !nameRE.MatchString(s.Name) {
		fail("name must be noun.verb")
	}
	gates := 0
	if len(s.Gate.Perms) > 0 {
		gates++
	}
	if len(s.Gate.Platform) > 0 {
		gates++
	}
	if s.Gate.Self {
		gates++
	}
	if gates != 1 && !s.Internal {
		fail("exactly one of Gate.Perms, Gate.Platform and Gate.Self must be set")
	}
	for _, p := range s.Gate.Perms {
		if !p.Valid() {
			fail("unknown permission %q", p)
		}
	}
	switch s.Kind {
	case Write:
		if s.Execute == nil || s.Query != nil {
			fail("a Write tool has Execute and no Query")
		}
	case Read:
		if s.Query == nil || s.Execute != nil || s.Validate != nil {
			fail("a Read tool has Query, and neither Execute nor Validate")
		}
	}
	if s.Resolve == nil {
		fail("Resolve is required")
	}
	var zero In
	_, hasCourse := any(zero).(courseScoped)
	if s.Gate.CourseScoped() != hasCourse {
		fail("input must embed tool.InCourse exactly when the tool is gated by course permissions")
	}

	inSchema, err := jsonschema.For[In](schemaOptions)
	if err != nil {
		fail("input schema: %v", err)
	}
	outSchema, err := jsonschema.For[Out](schemaOptions)
	if err != nil {
		fail("output schema: %v", err)
	}
	resolved, err := inSchema.Resolve(nil)
	if err != nil {
		fail("input schema: %v", err)
	}

	t := Tool{
		Name: s.Name, Description: s.Description, Kind: s.Kind, Gate: s.Gate, HTTP: s.HTTP,
		Internal: s.Internal, SecretIn: s.SecretIn, SecretOut: s.SecretOut,
		InputSchema: inSchema, OutputSchema: outSchema,
	}
	t.Decode = func(raw []byte) (any, error) {
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		var instance any
		if err := json.Unmarshal(raw, &instance); err != nil {
			return nil, apperr.Invalid("arguments are not valid JSON: %v", err)
		}
		if err := resolved.Validate(instance); err != nil {
			return nil, apperr.Invalid("arguments do not match the schema of %s: %v", s.Name, err)
		}
		var in In
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, apperr.Invalid("arguments: %v", err)
		}
		return in, nil
	}
	t.CourseID = func(in any) uuid.UUID {
		if c, ok := in.(courseScoped); ok {
			return c.GetCourseID()
		}
		return uuid.Nil
	}
	t.Resolve = func(ctx context.Context, q dbq.Querier, in any) (Target, error) {
		return s.Resolve(ctx, q, in.(In))
	}
	if s.Validate != nil {
		t.Validate = func(ctx context.Context, q dbq.Querier, m *domain.Member, in any) error {
			return s.Validate(ctx, q, m, in.(In))
		}
	}
	if s.Execute != nil {
		t.Execute = func(ctx context.Context, ec *ExecCtx, in any) (any, error) {
			return s.Execute(ctx, ec, in.(In))
		}
	}
	if s.Query != nil {
		t.Query = func(ctx context.Context, rc *ReadCtx, in any) (any, error) {
			return s.Query(ctx, rc, in.(In))
		}
	}
	return t
}

// schemaOptions teaches schema inference the types that do not look like
// what they are: a UUID is a [16]byte and a Decimal is a struct.
var schemaOptions = &jsonschema.ForOptions{
	TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[uuid.UUID](): {Type: "string", Format: "uuid"},
		reflect.TypeFor[decimal.Decimal](): {
			Types:       []string{"number", "string"},
			Description: "a decimal number; a string is accepted where exactness matters",
		},
		reflect.TypeFor[json.RawMessage](): {},
	},
}
