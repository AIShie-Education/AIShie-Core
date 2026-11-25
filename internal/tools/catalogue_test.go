package tools_test

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// Both adapters are generated from the registry, so what every tool must get
// right for them is checked once, here.
func TestCatalogueIsWellFormed(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	routes := map[string]string{}
	placeholder := regexp.MustCompile(`\{([a-z_]+)\}`)

	for _, tl := range c.P.Registry().Exposed() {
		if len(tl.Description) < 20 {
			t.Errorf("%s: an agent chooses tools by their description; this one says %q", tl.Name, tl.Description)
		}
		if tl.HTTP.Method == "" || !strings.HasPrefix(tl.HTTP.Pattern, "/v1/") {
			t.Errorf("%s: no REST route", tl.Name)
			continue
		}
		wantMethod := "POST"
		if tl.Kind == tool.Read {
			wantMethod = "GET"
		}
		if tl.HTTP.Method != wantMethod {
			t.Errorf("%s: %s for a %v tool, want %s", tl.Name, tl.HTTP.Method, tl.Kind, wantMethod)
		}
		route := tl.HTTP.Method + " " + placeholder.ReplaceAllString(tl.HTTP.Pattern, "{}")
		if other, dup := routes[route]; dup {
			t.Errorf("%s and %s share the route %s", tl.Name, other, route)
		}
		routes[route] = tl.Name

		// Every path placeholder must be a field of the input.
		for _, mm := range placeholder.FindAllStringSubmatch(tl.HTTP.Pattern, -1) {
			if _, ok := tl.InputSchema.Properties[mm[1]]; !ok {
				t.Errorf("%s: route names {%s}, which is not an input field", tl.Name, mm[1])
			}
		}
		if tl.Gate.CourseScoped() && !strings.Contains(tl.HTTP.Pattern, "/courses/{course_id}") {
			t.Errorf("%s: a course-scoped tool's route must carry the course", tl.Name)
		}
	}
}

func TestRespondentActorIDIsInTheCatalogue(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	for _, name := range []string{"conversation.respondents", "conversation.get", "conversation.list", "conversation.inbox", "me.conversations"} {
		tl, ok := c.P.Registry().Get(name)
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		raw, err := json.Marshal(tl.OutputSchema)
		if err != nil {
			t.Fatalf("%s's output schema: %v", name, err)
		}
		if !strings.Contains(string(raw), `"actor_id"`) {
			t.Errorf("%s's output schema does not describe respondent actor_id: %s", name, raw)
		}
	}
}

func TestCourseGetInstructorsAreInTheCatalogue(t *testing.T) {
	c := testkit.NewCS101(t, 0)
	get, ok := c.P.Registry().Get("course.get")
	if !ok {
		t.Fatal("course.get is not registered")
	}
	if _, ok := get.OutputSchema.Properties["instructors"]; !ok || !slices.Contains(get.OutputSchema.Required, "instructors") {
		t.Fatalf("course.get must always answer instructors: %+v", get.OutputSchema)
	}
	raw, err := json.Marshal(get.OutputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"display_name"`) || strings.Contains(string(raw), `"actor_id"`) ||
		strings.Contains(string(raw), `"email"`) || strings.Contains(string(raw), `"login_id"`) {
		t.Fatalf("course.get's instructor summary is not display names only: %s", raw)
	}
	// The administrators' course.list is not silently given an unpopulated
	// field merely because it shares CourseView.
	list, _ := c.P.Registry().Get("course.list")
	raw, err = json.Marshal(list.OutputSchema)
	if err != nil || strings.Contains(string(raw), `"instructors"`) {
		t.Fatalf("course.list unexpectedly advertises instructors: %s (%v)", raw, err)
	}
}
