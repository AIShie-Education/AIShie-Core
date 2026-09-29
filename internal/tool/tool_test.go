package tool_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/wake"
)

type in struct {
	tool.InCourse
	SubmissionID *uuid.UUID      `json:"submission_id,omitempty"`
	Score        decimal.Decimal `json:"score"`
	Note         *string         `json:"note,omitempty"`
}
type out struct {
	ID uuid.UUID `json:"id"`
}

func resolve(_ context.Context, _ dbq.Querier, i in) (tool.Target, error) {
	return tool.Target{CourseID: i.CourseID}, nil
}
func execute(context.Context, *tool.ExecCtx, in) (out, error) { return out{}, nil }

func valid() tool.Spec[in, out] {
	return tool.Spec[in, out]{
		Name: "thing.do", Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		Resolve: resolve, Execute: execute,
	}
}

func TestSchemaSeesThroughEmbeddingAndKnowsOurTypes(t *testing.T) {
	tl := tool.Define(valid())
	b, err := json.Marshal(tl.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
		Additional json.RawMessage            `json:"additionalProperties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Required, ","); got != "course_id,score" {
		t.Errorf("required = %q, want course_id,score (the embedded course_id, and no omitempty fields)", got)
	}
	if p := string(s.Properties["course_id"]); !strings.Contains(p, `"format":"uuid"`) || strings.Contains(p, "array") {
		t.Errorf("course_id schema = %s, want a uuid string, not sixteen integers", p)
	}
	if p := string(s.Properties["score"]); !strings.Contains(p, "number") || !strings.Contains(p, "string") {
		t.Errorf("score schema = %s, want number or string", p)
	}
	if string(s.Additional) == "" || string(s.Additional) == "true" {
		t.Errorf("additionalProperties = %s; unknown fields must be refused", s.Additional)
	}
}

type issued struct {
	ID    uuid.UUID `json:"id"`
	Token string    `json:"token"`
}

// A secret is not required of the result. The result is stored without it,
// and a replay returns what was stored, which a client checking it against
// this schema would otherwise refuse.
func TestAResultNeedNotCarryItsSecret(t *testing.T) {
	tl := tool.Define(tool.Spec[in, issued]{
		Name: "thing.issue", Kind: tool.Write, Gate: tool.Gate{Perms: []domain.Perm{domain.PermGradeSubmit}},
		SecretOut: []string{"token"}, Resolve: resolve,
		Execute: func(context.Context, *tool.ExecCtx, in) (issued, error) { return issued{}, nil },
	})
	if got := strings.Join(tl.OutputSchema.Required, ","); got != "id" {
		t.Errorf("required = %q, want id: a replay comes back without the token", got)
	}
	if _, ok := tl.OutputSchema.Properties["token"]; !ok {
		t.Error("the token is no longer described at all")
	}
}

func TestDecode(t *testing.T) {
	tl := tool.Define(valid())
	course := uuid.New()

	v, err := tl.Decode([]byte(`{"course_id": "` + course.String() + `", "score": "85.50"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := v.(in)
	if got.CourseID != course || tl.CourseID(v) != course || !got.Score.Equal(decimal.RequireFromString("85.5")) {
		t.Fatalf("decoded %+v", got)
	}
	for name, raw := range map[string]string{
		"missing required": `{"score": 1}`,
		"unknown field":    `{"course_id": "` + course.String() + `", "score": 1, "extra": 1}`,
		"wrong type":       `{"course_id": "` + course.String() + `", "score": [1]}`,
		"bad uuid":         `{"course_id": "nope", "score": 1}`,
		"not json":         `{`,
		"not an object":    `[1]`,
		// A decimal is bounded before anything expands it or parses it.
		"decimal string, huge exponent":     `{"course_id": "` + course.String() + `", "score": "1e2000000000"}`,
		"decimal string, 3-digit exponent":  `{"course_id": "` + course.String() + `", "score": "1e-100"}`,
		"decimal string, too many digits":   `{"course_id": "` + course.String() + `", "score": "` + strings.Repeat("9", 41) + `"}`,
		"decimal string, not a number":      `{"course_id": "` + course.String() + `", "score": "lots"}`,
		"number, huge exponent":             `{"course_id": "` + course.String() + `", "score": 1e999999999}`,
		"number, a megabyte of digits":      `{"course_id": "` + course.String() + `", "score": 0.` + strings.Repeat("7", 1<<20) + `}`,
		"a repeated key, hiding a megabyte": `{"course_id": "` + course.String() + `", "score": "` + strings.Repeat("7", 1<<20) + `", "score": 1}`,
	} {
		if _, err := tl.Decode([]byte(raw)); !apperr.Is(err, apperr.InvalidArgument) {
			t.Errorf("%.30s: err = %.80v, want invalid_argument", name, err)
		}
	}
	forty := strings.Repeat("9", 40)
	for _, s := range []string{"-0.5", ".5", "5.", "+5", "1e99", "12E-3", "007", forty + "." + forty} {
		if _, err := tl.Decode([]byte(`{"course_id": "` + course.String() + `", "score": "` + s + `"}`)); err != nil {
			t.Errorf("score %.20q: %v", s, err)
		}
	}
}

func TestDefineRefusesMalformedTools(t *testing.T) {
	cases := map[string]func(*tool.Spec[in, out]){
		"name is not noun.verb":       func(s *tool.Spec[in, out]) { s.Name = "DoThing" },
		"no gate":                     func(s *tool.Spec[in, out]) { s.Gate = tool.Gate{} },
		"two gates":                   func(s *tool.Spec[in, out]) { s.Gate.Self = true },
		"unknown permission":          func(s *tool.Spec[in, out]) { s.Gate.Perms = []domain.Perm{"grade_everything"} },
		"write without execute":       func(s *tool.Spec[in, out]) { s.Execute = nil },
		"no resolve":                  func(s *tool.Spec[in, out]) { s.Resolve = nil },
		"platform gate, course input": func(s *tool.Spec[in, out]) { s.Gate = tool.Gate{Platform: []string{"admin"}} },
		"admin gate, course input":    func(s *tool.Spec[in, out]) { s.Gate = tool.Gate{Admin: true} },
		"read with execute":           func(s *tool.Spec[in, out]) { s.Kind = tool.Read },
		"own agents with any": func(s *tool.Spec[in, out]) {
			s.Gate.Any, s.Gate.OwnAgents = true, func(context.Context, dbq.Querier, domain.Actor, *domain.Member, tool.Target, time.Time) (domain.Level, error) {
				return domain.Denied, nil
			}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Define accepted it")
				}
			}()
			s := valid()
			breakIt(&s)
			tool.Define(s)
		})
	}
}

type outside struct {
	DeptID uuid.UUID `json:"dept_id"`
}

// An operation outside any course that a department's administrators may
// make as well as a platform administrator.
func administered() tool.Spec[outside, out] {
	return tool.Spec[outside, out]{
		Name: "thing.manage", Kind: tool.Write, Gate: tool.Gate{Admin: true},
		Resolve: func(_ context.Context, _ dbq.Querier, o outside) (tool.Target, error) {
			return tool.Target{DeptID: &o.DeptID}, nil
		},
		Execute: func(context.Context, *tool.ExecCtx, outside) (out, error) { return out{}, nil },
	}
}

// The Admin gate is a gate of its own: never beside course permissions,
// platform roles or one's own account, which would each say something else
// about who may call.
func TestDefineTakesTheAdminGateAlone(t *testing.T) {
	tool.Define(administered())
	cases := map[string]func(*tool.Spec[outside, out]){
		"with course permissions": func(s *tool.Spec[outside, out]) { s.Gate.Perms = []domain.Perm{domain.PermMemberManage} },
		"with platform roles":     func(s *tool.Spec[outside, out]) { s.Gate.Platform = []string{"admin"} },
		"with one's own account":  func(s *tool.Spec[outside, out]) { s.Gate.Self = true },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(r.(string), "exactly one of") {
					t.Fatalf("Define: %v", r)
				}
			}()
			s := administered()
			breakIt(&s)
			tool.Define(s)
		})
	}
}

func TestRegistry(t *testing.T) {
	r := tool.NewRegistry()
	a, b, c := valid(), valid(), valid()
	b.Name, b.Internal = "thing.sweep", true
	c.Name, c.Unlisted = "thing.join", true
	r.Register(tool.Define(a), tool.Define(b), tool.Define(c))

	if _, ok := r.Get("thing.do"); !ok {
		t.Fatal("registered tool not found")
	}
	if got := len(r.All()); got != 3 {
		t.Fatalf("All() = %d tools", got)
	}
	if ex := r.Exposed(); len(ex) != 1 || ex[0].Name != "thing.do" {
		t.Fatalf("Exposed() = %v; internal and unlisted tools must not be offered", ex)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("registering a name twice was accepted")
		}
	}()
	r.Register(tool.Define(a))
}

// An unlisted tool is called by a handler of this server, at an endpoint of
// its own: it is no internal tool, which only the sweeps call, and it has no
// route of its own in the catalogue.
func TestDefineKeepsUnlistedToolsOffTheCatalogue(t *testing.T) {
	cases := map[string]func(*tool.Spec[in, out]){
		"internal as well": func(s *tool.Spec[in, out]) { s.Internal = true },
		"with a route":     func(s *tool.Spec[in, out]) { s.HTTP = tool.Route{Method: "POST", Pattern: "/v1/things"} },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(r.(string), "Unlisted") {
					t.Fatalf("Define: %v", r)
				}
			}()
			s := valid()
			s.Unlisted = true
			breakIt(&s)
			tool.Define(s)
		})
	}
}

type waitIn struct {
	tool.InCourse
	After int `json:"after,omitempty"`
	tool.CanWait
}

type waitOut struct {
	Items []int `json:"items"`
}

func waiting() tool.Spec[waitIn, waitOut] {
	return tool.Spec[waitIn, waitOut]{
		Name: "thing.list", Kind: tool.Read, Gate: tool.Gate{Perms: []domain.Perm{domain.PermDocumentRead}},
		Resolve: func(_ context.Context, _ dbq.Querier, i waitIn) (tool.Target, error) {
			return tool.Target{CourseID: i.CourseID}, nil
		},
		Query: func(context.Context, *tool.ReadCtx, waitIn) (waitOut, error) { return waitOut{}, nil },
		Wait: &tool.Waiting[waitIn, waitOut]{
			For:     func(_ *tool.ReadCtx, i waitIn) wake.Filter { return wake.Filter{CourseID: i.CourseID} },
			Nothing: func(_ waitIn, _, now waitOut) bool { return len(now.Items) == 0 },
		},
	}
}

// A read that can wait takes wait_s, from 0 to MaxWaitSeconds, and says what
// it waits for; a tool that says one without the other, or a write that
// would wait, is not a tool.
func TestAToolThatWaits(t *testing.T) {
	tl := tool.Define(waiting())
	course := uuid.New()
	for waitS, ok := range map[string]bool{"0": true, "25": true, "26": false, "-1": false, "1.5": false, `"5"`: false} {
		v, err := tl.Decode([]byte(`{"course_id": "` + course.String() + `", "wait_s": ` + waitS + `}`))
		if ok != (err == nil) {
			t.Errorf("wait_s %s: %v", waitS, err)
		}
		if ok && strconv.Itoa(tl.WaitSeconds(v)) != waitS {
			t.Errorf("wait_s %s: read as %d", waitS, tl.WaitSeconds(v))
		}
	}
	v, _ := tl.Decode([]byte(`{"course_id": "` + course.String() + `"}`))
	if tl.WaitSeconds(v) != 0 || !tl.WaitNothing(v, waitOut{}, waitOut{}) || tl.WaitNothing(v, waitOut{}, waitOut{Items: []int{1}}) {
		t.Fatal("what the tool waits for, erased")
	}
	if f := tl.WaitFor(nil, v); f.CourseID != course {
		t.Fatalf("waits for %+v", f)
	}

	cases := map[string]func(*tool.Spec[waitIn, waitOut]){
		"input that can wait, and nothing to wait for": func(s *tool.Spec[waitIn, waitOut]) { s.Wait = nil },
		"nothing said of what is nothing new":          func(s *tool.Spec[waitIn, waitOut]) { s.Wait.Nothing = nil },
		"a write that waits": func(s *tool.Spec[waitIn, waitOut]) {
			s.Kind, s.Query = tool.Write, nil
			s.Execute = func(context.Context, *tool.ExecCtx, waitIn) (waitOut, error) { return waitOut{}, nil }
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Define accepted it")
				}
			}()
			s := waiting()
			breakIt(&s)
			tool.Define(s)
		})
	}
	t.Run("something to wait for, and input that cannot wait", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("Define accepted it")
			}
		}()
		s := valid()
		s.Kind, s.Execute = tool.Read, nil
		s.Query = func(context.Context, *tool.ReadCtx, in) (out, error) { return out{}, nil }
		s.Wait = &tool.Waiting[in, out]{For: func(*tool.ReadCtx, in) wake.Filter { return wake.Filter{} },
			Nothing: func(in, out, out) bool { return true }}
		tool.Define(s)
	})
}
