package tools_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
	"github.com/AIShie-Education/AIShie-Core/internal/tools"
)

func searchMaterials(t *testing.T, b *built, actor uuid.UUID, query string, limit, page int) tools.CourseMaterialsSearchOut {
	t.Helper()
	args := m{"course_id": b.course, "query": query}
	if limit != 0 {
		args["limit"] = limit
	}
	if page != 0 {
		args["page"] = page
	}
	return testkit.Result[tools.CourseMaterialsSearchOut](t, b.do(t, actor, "course_materials.search", args))
}

func makeMaterial(t *testing.T, b *built, title, body string, publish bool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create",
		m{"course_id": b.course, "kind": "material", "title": title, "body_md": body}))
	if publish {
		b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": made.DocumentID, "version_id": *made.VersionID})
	}
	return made.DocumentID, *made.VersionID
}

func onlyMaterialHit(t *testing.T, out tools.CourseMaterialsSearchOut) tools.CourseMaterialHit {
	t.Helper()
	if len(out.Hits) != 1 {
		t.Fatalf("got %d hits, want one: %+v", len(out.Hits), out.Hits)
	}
	return out.Hits[0]
}

func TestCourseMaterialsSearchLanguagesVersionsAndLifecycle(t *testing.T) {
	b := build(t)
	doc, v1 := makeMaterial(t, b, "演算法 第三週", "# 排序\n\n合併排序的複雜度是 O(n log n)。\n\nMerge sort uses recursion.", false)
	if got := searchMaterials(t, b, b.yuki, "合併排序", 0, 0); len(got.Hits) != 0 {
		t.Fatalf("a student searched an unpublished version: %+v", got.Hits)
	}
	if hit := onlyMaterialHit(t, searchMaterials(t, b, b.sato, "合併排序", 0, 0)); hit.VersionID != v1 {
		t.Fatalf("instructor's draft hit: %+v", hit)
	}
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc, "version_id": v1})

	for _, query := range []string{"合併排序", "第三週", "recurs"} {
		hit := onlyMaterialHit(t, searchMaterials(t, b, b.yuki, query, 0, 0))
		if hit.DocumentID != doc || hit.VersionID != v1 || hit.TextSource != "body_md" || hit.Where != "body" ||
			hit.Read.Tool != "document_get" || fmt.Sprint(hit.Read.Arguments["version_id"]) != v1.String() || hit.Excerpt == "" {
			t.Errorf("%q: %+v", query, hit)
		}
		if _, hasFile := hit.Read.Arguments["file_id"]; hasFile {
			t.Errorf("a body hit names a file: %+v", hit.Read)
		}
	}

	v2 := testkit.Result[tools.DocumentVersionOut](t, b.do(t, b.sato, "document.add_version", m{
		"course_id": b.course, "document_id": doc,
		"body_md": strings.Repeat("快速排序的草稿段落，包含 pivot。\n\n", 70),
	})).VersionID
	if got := searchMaterials(t, b, b.yuki, "pivot", 0, 0); len(got.Hits) != 0 {
		t.Fatalf("the student saw the new draft: %+v", got.Hits)
	}
	if hit := onlyMaterialHit(t, searchMaterials(t, b, b.sato, "pivot", 1, 1)); hit.VersionID != v2 || !strings.Contains(hit.Excerpt, "快速排序") {
		t.Fatalf("the instructor did not read the latest draft: %+v", hit)
	}
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc, "version_id": v2})
	if got := searchMaterials(t, b, b.yuki, "recursion", 0, 0); len(got.Hits) != 0 {
		t.Fatalf("the superseded published body is still found: %+v", got.Hits)
	}
	page1 := searchMaterials(t, b, b.yuki, "pivot", 1, 1)
	page2 := searchMaterials(t, b, b.yuki, "pivot", 1, 2)
	if len(page1.Hits) != 1 || !page1.HasMore || len(page2.Hits) != 1 || page2.Page != 2 || page1.Hits[0].Part == page2.Hits[0].Part {
		t.Fatalf("paging: page 1 %+v, page 2 %+v", page1, page2)
	}

	// Renaming rebuilds every indexed version with the new searchable title.
	b.do(t, b.sato, "document.update", m{"course_id": b.course, "document_id": doc, "title": "複習 第四週"})
	if got := searchMaterials(t, b, b.yuki, "第三週", 0, 0); len(got.Hits) != 0 {
		t.Fatalf("the old title is still indexed: %+v", got.Hits)
	}
	if hit := onlyMaterialHit(t, searchMaterials(t, b, b.yuki, "第四週", 1, 1)); hit.Title != "複習 第四週" {
		t.Fatalf("the new title is not indexed: %+v", hit)
	}

	// Backfill is idempotent and rebuilds both the published and older bodies.
	b.Exec(`DELETE FROM document_search_passages WHERE document_id = $1`, doc)
	if got := searchMaterials(t, b, b.yuki, "pivot", 0, 0); len(got.Hits) != 0 {
		t.Fatal("deleting the derived index did not remove its hits")
	}
	written, err := tools.BackfillMaterialPassages(context.Background(), b.Pool, 100, time.Now())
	if err != nil || written < 3 {
		t.Fatalf("backfill wrote %d passages: %v", written, err)
	}
	if again, err := tools.BackfillMaterialPassages(context.Background(), b.Pool, 100, time.Now()); err != nil || again != 0 {
		t.Fatalf("a second backfill wrote %d passages: %v", again, err)
	}
	if len(searchMaterials(t, b, b.yuki, "pivot", 1, 1).Hits) != 1 {
		t.Fatal("backfill did not restore the published body")
	}

	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": doc})
	if n := b.Count(`SELECT count(*) FROM document_search_passages WHERE document_id = $1`, doc); n != 0 {
		t.Fatalf("archive left %d passages", n)
	}
	if got := searchMaterials(t, b, b.yuki, "pivot", 0, 0); len(got.Hits) != 0 {
		t.Fatalf("an archived document was searched: %+v", got.Hits)
	}
	b.do(t, b.sato, "document.unarchive", m{"course_id": b.course, "document_id": doc})
	if len(searchMaterials(t, b, b.yuki, "pivot", 1, 1).Hits) != 1 {
		t.Fatal("unarchive did not rebuild the index")
	}
	b.do(t, b.admin, "document.purge", m{"course_id": b.course, "document_id": doc, "reason": "Mistaken upload"})
	if n := b.Count(`SELECT count(*) FROM document_search_passages WHERE document_id = $1`, doc); n != 0 {
		t.Fatalf("purge left %d passages", n)
	}
}

func TestCourseMaterialFileTextIndexAndReadPointer(t *testing.T) {
	b := build(t)
	token := b.upload(t, b.sato, "material", "application/pdf", []byte("%PDF sorting"))
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{
		"course_id": b.course, "kind": "material", "title": "Sorting slides", "files": oneFile(token),
	}))
	doc, version, file := made.DocumentID, *made.VersionID, made.FileIDs[0]
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc, "version_id": version})
	svc := b.transcriber(t)
	claimed := b.claim(t, svc, m{"max": 1})
	if len(claimed) != 1 {
		t.Fatalf("claimed %+v", claimed)
	}
	b.complete(t, svc, claimed[0], "## Slide 1: 合併排序\n合併排序的複雜度。\n\n## Page 2: Comparison\nMerge sort.")

	hit := onlyMaterialHit(t, searchMaterials(t, b, b.yuki, "合併排序", 0, 0))
	if hit.FileID == nil || *hit.FileID != file || hit.Filename == nil || *hit.Filename != "upload" ||
		hit.Where != "slide 1" || hit.TextSource != "ai" || hit.Read.Tool != "document_text" {
		t.Fatalf("file hit: %+v", hit)
	}
	if fmt.Sprint(hit.Read.Arguments["file_id"]) != file.String() || fmt.Sprint(hit.Read.Arguments["part"]) != "1" {
		t.Fatalf("read arguments: %+v", hit.Read.Arguments)
	}
	if byName := searchMaterials(t, b, b.yuki, "upload", 0, 0); len(byName.Hits) == 0 ||
		byName.Hits[0].FileID == nil || *byName.Hits[0].FileID != file {
		t.Fatalf("the filename was not indexed: %+v", byName.Hits)
	}
	readName := strings.ReplaceAll(hit.Read.Tool, "_", ".")
	read := testkit.Result[tools.DocumentTextOut](t, b.do(t, b.yuki, readName, hit.Read.Arguments))
	if read.Part != 1 || read.Text.Body == nil || !strings.Contains(*read.Text.Body, "合併排序") {
		t.Fatalf("read did not give the passage: %+v", read)
	}

	staffBody := "## Page 2: Corrected\nStaff corrected the comparison."
	b.do(t, b.sato, "document.text_update", m{"course_id": b.course, "document_id": doc, "version_id": version,
		"file_id": file, "body": staffBody})
	if got := searchMaterials(t, b, b.yuki, "合併排序", 0, 0); len(got.Hits) != 0 {
		t.Fatalf("the old transcription remains: %+v", got.Hits)
	}
	hit = onlyMaterialHit(t, searchMaterials(t, b, b.yuki, "corrected", 0, 0))
	if hit.TextSource != "staff" || hit.Where != "page 2" {
		t.Fatalf("staff edit: %+v", hit)
	}

	// A stale revision is discovered by the background backfill.
	b.Exec(`UPDATE document_search_passages SET text_revision = text_revision - 1 WHERE version_id = $1 AND file_id = $2`, version, file)
	if _, err := tools.BackfillMaterialPassages(context.Background(), b.Pool, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	var revision int
	if err := b.Pool.QueryRow(context.Background(), `SELECT text_revision FROM document_search_passages
		WHERE version_id = $1 AND file_id = $2`, version, file).Scan(&revision); err != nil || revision != 3 {
		t.Fatalf("backfilled text revision %d, err %v", revision, err)
	}

	b.do(t, b.sato, "document.archive", m{"course_id": b.course, "document_id": doc})
	if n := b.Count(`SELECT count(*) FROM document_search_passages WHERE document_id = $1`, doc); n != 0 {
		t.Fatalf("archive left %d file passages", n)
	}
	b.do(t, b.sato, "document.unarchive", m{"course_id": b.course, "document_id": doc})
	if got := searchMaterials(t, b, b.yuki, "corrected", 0, 0); len(got.Hits) != 1 {
		t.Fatalf("unarchive did not restore staff's text: %+v", got.Hits)
	}
	b.do(t, b.sato, "document.text_retranscribe", m{"course_id": b.course, "document_id": doc, "version_id": version,
		"file_id": file, "discard_edit": true})
	if n := b.Count(`SELECT count(*) FROM document_search_passages WHERE version_id = $1 AND file_id = $2`, version, file); n != 0 {
		t.Fatalf("retranscribe left %d passages of withdrawn text", n)
	}

	claimed = b.claim(t, svc, m{"max": 1})
	b.complete(t, svc, claimed[0], "## Slide 4: Final\nPurge this passage.")
	if got := searchMaterials(t, b, b.yuki, "Purge", 0, 0); len(got.Hits) != 1 {
		t.Fatalf("the second transcription was not indexed: %+v", got.Hits)
	}
	b.do(t, b.admin, "document.purge", m{"course_id": b.course, "document_id": doc, "version_id": version, "reason": "Mistaken upload"})
	if n := b.Count(`SELECT count(*) FROM document_search_passages WHERE version_id = $1`, version); n != 0 {
		t.Fatalf("version purge left %d passages", n)
	}
}

func TestCourseMaterialsSearchUnder200ms(t *testing.T) {
	b := build(t)
	for i := range 40 {
		body := fmt.Sprintf("教材 %02d：needle 與合併排序。\n\n", i) + strings.Repeat("Sorting, recursion and comparison. ", 35)
		makeMaterial(t, b, fmt.Sprintf("Performance %02d", i), body, true)
	}
	started := time.Now()
	out := searchMaterials(t, b, b.yuki, "needle", 20, 1)
	elapsed := time.Since(started)
	if len(out.Hits) != 20 || !out.HasMore {
		t.Fatalf("indexed result page: %d hits, has_more %v", len(out.Hits), out.HasMore)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("indexed search took %s, want < 200ms", elapsed)
	}
	t.Logf("indexed search over 40 documents took %s", elapsed)
}

func TestCourseMaterialsSearchAssignmentScopeAndArguments(t *testing.T) {
	b := build(t)
	made := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{
		"course_id": b.course, "kind": "instructions", "title": "HW3 brief", "body_md": "考試範圍與遞迴。",
	}))
	doc, version := made.DocumentID, *made.VersionID
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": doc, "version_id": version})
	if got := searchMaterials(t, b, b.yuki, "考試範圍", 0, 0); len(got.Hits) != 0 {
		t.Fatalf("unattached instructions leaked: %+v", got.Hits)
	}
	if got := searchMaterials(t, b, b.sato, "考試範圍", 0, 0); len(got.Hits) != 1 {
		t.Fatal("the instructor cannot search instructions")
	}
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "instructions_document_id": doc})
	for _, actor := range []uuid.UUID{b.yuki, b.grader} {
		if got := searchMaterials(t, b, actor, "考試範圍", 0, 0); len(got.Hits) != 1 {
			t.Errorf("actor %s cannot search the in-scope assignment: %+v", actor, got.Hits)
		}
	}

	// Rubric text additionally needs rubric_read, not merely document_read.
	rubric := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{
		"course_id": b.course, "kind": "rubric", "title": "Rubric", "body_md": "評分準則的證據與結構。",
	}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": rubric.DocumentID, "version_id": *rubric.VersionID})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": b.hw3, "rubric_document_id": rubric.DocumentID})
	if got := searchMaterials(t, b, b.yuki, "評分準則", 0, 0); len(got.Hits) != 0 {
		t.Fatalf("a student without rubric_read found the rubric: %+v", got.Hits)
	}
	if got := searchMaterials(t, b, b.grader, "評分準則", 0, 0); len(got.Hits) != 1 {
		t.Fatalf("the in-scope rubric reader found %+v", got.Hits)
	}

	hw4 := testkit.Result[tools.IDOut](t, b.do(t, b.sato, "assignment.create", m{
		"course_id": b.course, "title": "HW4", "points_possible": 100, "component_id": b.bucket,
	})).ID
	b.do(t, b.sato, "assignment.publish", m{"course_id": b.course, "assignment_id": hw4})
	other := testkit.Result[tools.DocumentCreateOut](t, b.do(t, b.sato, "document.create", m{
		"course_id": b.course, "kind": "instructions", "title": "HW4 brief", "body_md": "另一份作業的隱藏內容。",
	}))
	b.do(t, b.sato, "document.publish", m{"course_id": b.course, "document_id": other.DocumentID, "version_id": *other.VersionID})
	b.do(t, b.sato, "assignment.update", m{"course_id": b.course, "assignment_id": hw4, "instructions_document_id": other.DocumentID})
	if got := searchMaterials(t, b, b.yuki, "隱藏內容", 0, 0); len(got.Hits) != 1 {
		t.Fatal("a student's all-assignment scope was not applied")
	}
	if got := searchMaterials(t, b, b.grader, "隱藏內容", 0, 0); len(got.Hits) != 0 {
		t.Fatalf("the grader's listed-assignment scope was bypassed: %+v", got.Hits)
	}

	for name, args := range map[string]m{
		"empty query": {"course_id": b.course, "query": " "},
		"no terms":    {"course_id": b.course, "query": "!!!"},
		"limit low":   {"course_id": b.course, "query": "sort", "limit": -1},
		"limit high":  {"course_id": b.course, "query": "sort", "limit": 21},
		"page low":    {"course_id": b.course, "query": "sort", "page": -1},
		"page high":   {"course_id": b.course, "query": "sort", "page": 21},
	} {
		if _, err := b.Call(b.sato, "course_materials.search", args, ""); !apperr.Is(err, apperr.InvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
