package tool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/apperr"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/db/dbq"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/domain"
	"github.com/AIShiteru-LMS/AIShiteru-Core/internal/tool"
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
		"read with execute":           func(s *tool.Spec[in, out]) { s.Kind = tool.Read },
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

func TestRegistry(t *testing.T) {
	r := tool.NewRegistry()
	a, b := valid(), valid()
	b.Name, b.Internal = "thing.sweep", true
	r.Register(tool.Define(a), tool.Define(b))

	if _, ok := r.Get("thing.do"); !ok {
		t.Fatal("registered tool not found")
	}
	if got := len(r.All()); got != 2 {
		t.Fatalf("All() = %d tools", got)
	}
	if ex := r.Exposed(); len(ex) != 1 || ex[0].Name != "thing.do" {
		t.Fatalf("Exposed() = %v; internal tools must not be offered", ex)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("registering a name twice was accepted")
		}
	}()
	r.Register(tool.Define(a))
}
