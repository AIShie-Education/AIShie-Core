package mcpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Core/internal/mcpapi"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

// The new search is one registry tool served by both public doors, with the
// MCP spelling and read-only annotation the runner expects.
func TestCourseMaterialsSearchOverMCPAndREST(t *testing.T) {
	f := serve(t, 1)
	made := testkit.Result[tools.DocumentCreateOut](t, f.c.MustCall(f.c.Sato, "document.create", m{
		"course_id": f.c.Course, "kind": "material", "title": "第三週", "body_md": "合併排序與 merge sort。",
	}, "search-document"))
	f.c.MustCall(f.c.Sato, "document.publish", m{"course_id": f.c.Course, "document_id": made.DocumentID,
		"version_id": *made.VersionID}, "search-publish")

	token := f.token(t, f.c.Grader)
	s := f.connect(t, token)
	if got := s.InitializeResult().Instructions; !strings.Contains(got, "course_materials_search") ||
		!strings.Contains(got, "exactly the arguments") || !strings.Contains(got, "document_text") {
		t.Fatalf("MCP instructions do not explain material search:\n%s", got)
	}
	listed, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tl := range listed.Tools {
		if tl.Name == mcpapi.ToolName("course_materials.search") {
			found = true
			if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
				t.Fatalf("search is not read-only: %+v", tl.Annotations)
			}
		}
	}
	if !found {
		t.Fatal("course_materials_search is not listed")
	}
	env, _ := call(t, s, "course_materials_search", m{"course_id": f.c.Course, "query": "合併排序"})
	if env.Status != "executed" {
		t.Fatalf("MCP search: %+v", env)
	}
	var out tools.CourseMaterialsSearchOut
	if err := json.Unmarshal(env.Result, &out); err != nil || len(out.Hits) != 1 || out.Hits[0].Read.Tool != "document_get" {
		t.Fatalf("MCP result %s: %+v, %v", env.Result, out, err)
	}

	// The same token and registry declaration serve the REST route.
	u := f.srv.URL + "/v1/courses/" + f.c.Course.String() + "/materials/search?query=" + url.QueryEscape("merge")
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{Transport: bearer{token}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("REST search returned %s", res.Status)
	}
	var rest envelope
	if err := json.NewDecoder(res.Body).Decode(&rest); err != nil || rest.Status != "executed" {
		t.Fatalf("REST envelope: %+v, %v", rest, err)
	}
	out = tools.CourseMaterialsSearchOut{}
	if err := json.Unmarshal(rest.Result, &out); err != nil || len(out.Hits) != 1 || out.Hits[0].DocumentID != made.DocumentID {
		t.Fatalf("REST result %s: %+v, %v", rest.Result, out, err)
	}
}
