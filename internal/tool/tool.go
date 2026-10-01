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

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/authz"
	"github.com/AIShie-Education/AIShie-Core/internal/canon"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/wake"
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
//
// An Ephemeral tool changes state that is not worth an action: short-lived
// and lost without harm, as an answer's draft is while it is written
// (conversation.draft), or a claim on a text version to transcribe, which
// lapses (document_text.queue). It is authorized as a Write is, scope
// included, its caller's seat held and an archived course refused, and is
// then carried out at once, in a transaction of its own, whatever level
// above denied its caller holds: nothing it does waits for anyone's
// decision. It is recorded nowhere: no action row, no idempotency key, no
// proposal, no event. One written many times a second bounds its own rate
// (Spec.BoundsOwnRate), and a call of it that is carried out is not counted
// against its caller's rate limit.
type Kind int

const (
	Read Kind = iota
	Write
	Ephemeral
)

// Gate says who may call a tool. Exactly one of Perms, Platform, Admin, Self
// and Service is set.
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
	// Admin is for operations outside any course that a platform
	// administrator (root, admin) may make anywhere, and a department
	// administrator only within what they cover. The tool's Resolve names
	// the department the call is about (Target.DeptID). Like Platform, it is
	// allowed outright or not at all.
	Admin bool
	// Self marks a tool where an actor acts on its own account.
	Self bool
	// Service names the site service whose tool this is
	// (domain.ServiceDocumentText): only that service calls it, with a live
	// credential of its own, and a service calls nothing that is not its own
	// (authz.Services). Like Platform, it is allowed outright or not at all.
	// A service's tools are called over REST only: the agents' door takes no
	// service's credential, and does not offer them.
	Service string
	// OwnAgents, with Perms, is what an agent's owner may do about their own
	// agents whatever Perms give them: once the target is resolved, the
	// pipeline asks it, for a caller whose seat counts and whom Perms deny
	// or hold below autonomous, and a level it returns above theirs is the
	// call's. It never lowers a level. A caller it gives nothing keeps the
	// denial Perms gave, recorded as ever, and a target that is not found
	// for such a caller is that denial too: it learns nothing of ids.
	OwnAgents OwnAgentsFunc
	// Refusal, with Perms, says why a caller whom Perms deny is refused,
	// where the tool knows a reason that tells more than the permission:
	// asked only once Perms have denied a seat that counts
	// (permission_denied), it returns the refusal to give in its place, whose
	// details carry the reason, or nil to leave it as it is. It never allows
	// anything, so it may read what authorization does not, actor.kind among
	// it: a person answering in a conversation is told that conversations are
	// with agents.
	Refusal RefusalFunc
}

// OwnAgentsFunc says what caller, from seat, may do about target because it
// concerns their own agents: domain.Denied for nothing.
type OwnAgentsFunc func(ctx context.Context, q dbq.Querier, caller domain.Actor, seat *domain.Member, target Target, now time.Time) (domain.Level, error)

// RefusalFunc says why caller, from seat, is refused a call its permissions
// deny, or nil for permission_denied.
type RefusalFunc func(ctx context.Context, q dbq.Querier, caller domain.Actor, seat *domain.Member) (*apperr.Error, error)

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
	// DeptID and AnyDept are what an Admin-gated call is about, for a
	// department administrator. DeptID set: they must cover it (an
	// appointment at it or above). DeptID nil and AnyDept false: only a
	// platform administrator may make the call (a department at the top of
	// the tree). AnyDept: any department administrator may, and the tool
	// limits what it reads or writes to ExecCtx.Admin / ReadCtx.Admin.
	DeptID  *uuid.UUID
	AnyDept bool
}

// Route places a tool in the REST API.
type Route struct {
	Method  string // GET, POST, ...
	Pattern string // /v1/courses/{course_id}/grades
	// IfMatch names the input field that an If-Match header carries, for a
	// write made over the version its caller read: If-Match: "3" is the
	// field given as 3. The field may be in the body as well, if it says the
	// same.
	IfMatch string
}

// ExecCtx is what a Write tool runs with. Everything it does goes through Tx,
// inside a savepoint: if the tool returns an error, all of it is undone and
// the attempt is recorded as failed.
//
// An Ephemeral tool runs with it too, in a transaction of its own that an
// error undoes whole, with no action: ActionID is uuid.Nil and Emit is nil,
// since nothing it does is recorded or in the feed.
type ExecCtx struct {
	Tx pgx.Tx
	Q  *dbq.Queries
	// Actor and Member are who the action is attributed to. When a proposal
	// is approved these are the proposer's, not the approver's. Member is nil
	// for platform, self and system calls.
	Actor    domain.Actor
	Member   *domain.Member
	ActionID uuid.UUID
	// CredentialID is the token or session the call was made with, for a
	// tool that asks which one (me.site_chat, a service's tools). uuid.Nil when there is
	// none to speak of: a proposal carried out on approval, whose credential
	// was the proposer's and is not kept, a sweep, a call made without one.
	CredentialID uuid.UUID
	// Admin is who makes an Admin-gated call, for the tool to limit itself
	// by. The zero value, which every other call has, covers nothing.
	Admin authz.AdminScope
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
	// Admin is who makes an Admin-gated call, as in ExecCtx.
	Admin authz.AdminScope
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
	// Unlisted tools are exposed by neither adapter either, and have no route
	// of their own: a handler of this server calls them, for a caller it has
	// authenticated, with Pipeline.InvokeUnlisted. They are gated like any
	// other tool, and recorded like any other. Joining a course by a link is
	// the case: a person's, in a browser, at an endpoint of its own, and no
	// agent's tool.
	Unlisted bool
	// OnArchived lets a Write act on an archived course. Nothing may, except
	// what changes whether it is archived.
	OnArchived bool
	// BoundsOwnRate marks an Ephemeral tool written many times a second,
	// which bounds its own rate: a call of it that is carried out is given
	// back to its caller's rate limit by the adapters.
	BoundsOwnRate bool
	// MaxRequestBytes, when set, is the most a REST request for the tool may
	// carry, beyond the default, for a tool that takes a long text.
	MaxRequestBytes int64
	// SetsOwnPassword marks the one tool a person whose password someone
	// else set may call before they have set their own: setting it. Every
	// other call of theirs is refused (password_change_required).
	SetsOwnPassword bool
	// OwnerJudgedBy, for a tool whose Perms no person holds, is what an
	// agent's owner is measured by in their place when they judge an action
	// of this tool their agent made (pipeline ownerJudges): they decide it,
	// or review it, where they hold these at autonomous. A person answers no
	// conversation, so an owner is measured for their agent's answers by
	// what judging them is, action_decide.
	OwnerJudgedBy []domain.Perm
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
	// an upload too old to outlast it. It runs in the call's transaction,
	// so it may also clear, as it succeeds, what the proposal takes the
	// place of: conversation.answer's draft.
	Pin func(ctx context.Context, q dbq.Querier, m *domain.Member, now time.Time, in In) (In, error)
	// Execute is set for a Write and an Ephemeral tool, Query for a Read.
	Execute func(ctx context.Context, ec *ExecCtx, in In) (Out, error)
	Query   func(ctx context.Context, rc *ReadCtx, in In) (Out, error)
	// Wait, for a Read or an Ephemeral tool whose input embeds CanWait, is
	// what a call that asks to wait (wait_s) waits for when it finds nothing
	// new: nothing to read, or nothing to claim.
	Wait *Waiting[In, Out]
}

// MaxWaitSeconds is the longest a call may wait for news (wait_s): well
// inside what a reverse proxy, or a client, gives a request before it gives
// up on it.
const MaxWaitSeconds = 25

// CanWait is embedded by the input of a Read that can wait for news: one a
// caller would otherwise poll. With wait_s above zero, a call that finds
// nothing new waits, holding no connection to the database, until something
// it would read is committed, or wait_s is up; it then reads again, as it
// read the first time, and answers with that. A call that finds something
// answers at once, and so does one past the server's bounds on how many calls
// wait (package wake), whatever wait_s says. An Ephemeral tool that takes
// what waits for it, a claim on the queue, waits the same way when there is
// nothing to take, and is carried out again, as the first time, once there
// may be.
type CanWait struct {
	WaitS int `json:"wait_s,omitempty" jsonschema:"seconds to wait, 0 to 25, when there is nothing new: the call answers as soon as there is, or when the time is up, with whatever there is then; 0, the default, answers at once"`
}

func (c CanWait) waitSeconds() int { return c.WaitS }

type waitable interface{ waitSeconds() int }

// Waiting is what a call of a Read, or an Ephemeral tool, that can wait
// waits for.
type Waiting[In, Out any] struct {
	// For is the news that wakes the call: rc is the caller's, as the first
	// call authorized it.
	For func(rc *ReadCtx, in In) wake.Filter
	// Nothing says whether what the call read, now, is nothing new to its
	// caller: nothing past the cursor in, and nothing changed since the
	// call first read, first. A call waits only while it is.
	Nothing func(in In, first, now Out) bool
}

// Tool is a Spec with its types erased.
type Tool struct {
	Name        string
	Description string
	Kind        Kind
	Gate        Gate
	HTTP        Route
	Internal    bool
	Unlisted    bool
	OnArchived  bool
	// BoundsOwnRate and MaxRequestBytes are Spec's.
	BoundsOwnRate   bool
	MaxRequestBytes int64
	// SetsOwnPassword is Spec.SetsOwnPassword.
	SetsOwnPassword bool
	// OwnerJudgedBy is Spec.OwnerJudgedBy.
	OwnerJudgedBy []domain.Perm
	SecretIn      []string
	SecretOut     []string

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
	// WaitSeconds, set for a tool that can wait, is what the call asks for
	// (wait_s); WaitFor and WaitNothing are Spec.Wait's.
	WaitSeconds func(in any) int
	WaitFor     func(rc *ReadCtx, in any) wake.Filter
	WaitNothing func(in any, first, now any) bool
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
	if s.Gate.Admin {
		gates++
	}
	if s.Gate.Self {
		gates++
	}
	if s.Gate.Service != "" {
		gates++
	}
	if gates != 1 && !s.Internal {
		fail("exactly one of Gate.Perms, Gate.Platform, Gate.Admin, Gate.Self and Gate.Service must be set")
	}
	if s.Unlisted && (s.Internal || s.HTTP.Pattern != "") {
		fail("an Unlisted tool is not Internal, and has no route of its own")
	}
	for _, p := range s.Gate.Perms {
		if !p.Valid() {
			fail("unknown permission %q", p)
		}
	}
	if s.Gate.OwnAgents != nil && (!s.Gate.CourseScoped() || s.Gate.Any) {
		fail("Gate.OwnAgents goes with Gate.Perms, and not with Gate.Any")
	}
	if s.Gate.Refusal != nil && !s.Gate.CourseScoped() {
		fail("Gate.Refusal goes with Gate.Perms")
	}
	for _, p := range s.OwnerJudgedBy {
		if !p.Valid() {
			fail("unknown permission %q in OwnerJudgedBy", p)
		}
	}
	if len(s.OwnerJudgedBy) > 0 && (s.Kind != Write || !s.Gate.CourseScoped()) {
		fail("OwnerJudgedBy is for a Write gated by course permissions")
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
	case Ephemeral:
		// Nothing of it is recorded, proposed or replayed, so nothing that
		// is about those applies; and nothing but a caller makes one.
		if s.Execute == nil || s.Query != nil || s.Validate != nil || s.Pin != nil {
			fail("an Ephemeral tool has Execute, and neither Query, Validate nor Pin")
		}
		if s.Internal || s.Unlisted || len(s.SecretIn) > 0 || len(s.SecretOut) > 0 {
			fail("an Ephemeral tool is neither Internal nor Unlisted, and records no secret to keep out")
		}
	default:
		fail("unknown kind %d", s.Kind)
	}
	if s.BoundsOwnRate && s.Kind != Ephemeral {
		fail("only an Ephemeral tool bounds its own rate")
	}
	if s.Resolve == nil {
		fail("Resolve is required")
	}
	var zero In
	_, hasCourse := any(zero).(courseScoped)
	if s.Gate.CourseScoped() != hasCourse {
		fail("input must embed tool.InCourse exactly when the tool is gated by course permissions")
	}
	_, canWait := any(zero).(waitable)
	if canWait != (s.Wait != nil) {
		fail("input must embed tool.CanWait exactly when the tool says what it waits for (Wait)")
	}
	if s.Wait != nil && (s.Kind == Write || s.Wait.For == nil || s.Wait.Nothing == nil) {
		fail("only a Read or an Ephemeral tool waits, and its Wait says both For and Nothing")
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
	if canWait {
		// The schema holds wait_s to its bounds, so that a call asking for
		// more is refused as it is asked, not cut short without a word.
		p := inSchema.Properties["wait_s"]
		if p == nil {
			fail("input schema: no wait_s")
		}
		lo, hi := 0.0, float64(MaxWaitSeconds)
		p.Minimum, p.Maximum = &lo, &hi
	}
	if f := s.HTTP.IfMatch; f != "" {
		if p := inSchema.Properties[f]; p == nil || (p.Type != "integer" && !slices.Contains(p.Types, "integer")) {
			fail("HTTP.IfMatch names %q, which is not an integer field of the input", f)
		}
	}
	resolved, err := inSchema.Resolve(nil)
	if err != nil {
		fail("input schema: %v", err)
	}

	t := Tool{
		Name: s.Name, Description: s.Description, Kind: s.Kind, Gate: s.Gate, HTTP: s.HTTP,
		Internal: s.Internal, Unlisted: s.Unlisted, OnArchived: s.OnArchived, SetsOwnPassword: s.SetsOwnPassword,
		BoundsOwnRate: s.BoundsOwnRate, MaxRequestBytes: s.MaxRequestBytes,
		OwnerJudgedBy: s.OwnerJudgedBy, SecretIn: s.SecretIn, SecretOut: s.SecretOut,
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
	if s.Wait != nil {
		t.WaitSeconds = func(in any) int { return in.(waitable).waitSeconds() }
		t.WaitFor = func(rc *ReadCtx, in any) wake.Filter { return s.Wait.For(rc, in.(In)) }
		t.WaitNothing = func(in any, first, now any) bool { return s.Wait.Nothing(in.(In), first.(Out), now.(Out)) }
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
		// An agent's hosting is one of two, and a schema says which.
		reflect.TypeFor[domain.Hosting](): {Type: "string", Enum: []any{string(domain.HostingRuntime), string(domain.HostingMCP)}},
	},
}
