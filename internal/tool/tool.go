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
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/authz"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/canon"
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
	// Any changes what Perms means: holding any one of them is enough to get
	// as far as looking the target up, and the target then names the
	// permission that actually governs (Target.Perms, which becomes
	// required). Reading a document is like this — which permission applies
	// depends on whether it turns out to be a lecture, a rubric or someone's
	// submission.
	Any bool
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
	// Now is when this is being executed. ActionCreatedAt is when the call
	// was made: the same moment for a direct call, and the moment of the
	// proposal for an approval, which may be days later. A tool that must
	// not overwrite what was done in between compares against it.
	Now             time.Time
	ActionCreatedAt time.Time
	// Approved says the call is carrying out a proposal that has just been
	// approved, rather than being made directly. A tool tells the two apart
	// by this, never by ActionCreatedAt being earlier than Now: the proposal
	// was dated by whichever instance stored it, and that instance's clock
	// may run ahead of this one's.
	Approved bool
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
	// OnArchived lets a Write act on an archived course. Nothing may, except
	// what changes whether it is archived.
	OnArchived bool
	// SecretIn and SecretOut name top-level fields that must never be
	// stored: they are removed from the recorded payload and from the
	// recorded result. A SecretIn field still counts in the payload hash, as
	// a keyed digest, so that a key reused with another secret is caught. A
	// replay returns the recorded result, so the output schema does not
	// require a SecretOut field.
	SecretIn  []string
	SecretOut []string

	// Resolve finds the target. Returning an apperr not_found ends the call
	// with no action row: there was nothing to attempt.
	Resolve func(ctx context.Context, q dbq.Querier, in In) (Target, error)
	// Validate checks the domain's rules without writing anything. It runs
	// before Execute, and before a proposal is queued, so that nobody is
	// asked to approve something that could never run.
	Validate func(ctx context.Context, q dbq.Querier, m *domain.Member, in In) error
	// Pin fills in defaults that must be fixed when a proposal is made rather
	// than when it is approved — the rubric version a grade is against, say,
	// which may have moved on by then. It runs only for a call that is being
	// queued as a proposal, by the member m proposing it, and now is when
	// that is; what it returns is the payload stored with it. It may refuse
	// the call instead, for what could not wait as long as a proposal may:
	// an upload too old to outlast it.
	Pin func(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in In) (In, error)
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
	OnArchived  bool
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
	Pin      func(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in any) (any, error)
	Execute  func(ctx context.Context, ec *ExecCtx, in any) (any, error)
	Query    func(ctx context.Context, rc *ReadCtx, in any) (any, error)
}

// hasNUL walks a decoded JSON value looking for a string with U+0000 in it.
func hasNUL(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.ContainsRune(x, 0)
	case []any:
		for _, e := range x {
			if hasNUL(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if strings.ContainsRune(k, 0) || hasNUL(e) {
				return true
			}
		}
	}
	return false
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
	// A replay returns the result as it was stored, without its secrets, and
	// a client that checks it against this schema must not be told to expect
	// them.
	outSchema.Required = slices.DeleteFunc(outSchema.Required, func(name string) bool {
		return slices.Contains(s.SecretOut, name)
	})
	resolved, err := inSchema.Resolve(nil)
	if err != nil {
		fail("input schema: %v", err)
	}

	t := Tool{
		Name: s.Name, Description: s.Description, Kind: s.Kind, Gate: s.Gate, HTTP: s.HTTP,
		Internal: s.Internal, OnArchived: s.OnArchived, SecretIn: s.SecretIn, SecretOut: s.SecretOut,
		InputSchema: inSchema, OutputSchema: outSchema,
	}
	t.Decode = func(raw []byte) (any, error) {
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		// Before anything below parses a number into a decimal, which is
		// quadratic in its digits, or sees a repeated key differently from
		// the parse after it.
		if err := canon.Check(raw); err != nil {
			return nil, apperr.Invalid("arguments: %v", err)
		}
		var instance any
		if err := json.Unmarshal(raw, &instance); err != nil {
			return nil, apperr.Invalid("arguments are not valid JSON: %v", err)
		}
		if err := resolved.Validate(instance); err != nil {
			return nil, apperr.Invalid("arguments do not match the schema of %s: %v", s.Name, err)
		}
		if hasNUL(instance) {
			// The database cannot hold it, in a payload or in a column, so
			// it is refused here, as any other malformed argument is.
			return nil, apperr.Invalid("arguments cannot contain U+0000")
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
	if s.Pin != nil {
		t.Pin = func(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in any) (any, error) {
			return s.Pin(ctx, q, m, now, in.(In))
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

// decimalString is what a decimal given as a string may look like. canon
// bounds number literals and never looks inside strings, and "1e2000000000"
// is twelve bytes that the first comparison would expand into two billion
// digits. A score, a weight or a number of points needs nothing like canon's
// generous bounds, and a string held well within them stays within them
// wherever it goes next: pinned into a proposal as a number, canonicalized
// again, decoded again on approval.
const decimalString = `^[-+]?([0-9]{1,40}(\.[0-9]{0,40})?|\.[0-9]{1,40})([eE][-+]?[0-9]{1,2})?$`

// schemaOptions teaches schema inference the types that do not look like
// what they are: a UUID is a [16]byte and a Decimal is a struct.
var schemaOptions = &jsonschema.ForOptions{
	TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[uuid.UUID](): {Type: "string", Format: "uuid"},
		reflect.TypeFor[decimal.Decimal](): {
			Types:   []string{"number", "string"},
			Pattern: decimalString, // applies to the string form only
			Description: "a decimal number; a string is accepted where exactness matters, " +
				"with at most 40 digits either side of the point and an exponent of at most two digits",
		},
		reflect.TypeFor[json.RawMessage](): {},
	},
}
