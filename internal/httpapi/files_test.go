package httpapi_test

import (
	"net/url"
	"strings"
	"testing"
)

// Several files to a version, over HTTP all the way: each uploaded to a URL
// of its own, the handout named as it is uploaded; the version written with
// them in order; a student reads it and downloads each file, from the view
// and by its id, under its name; the service claims each file's text on its
// own and writes it back by its file; the student reads each; the
// instructor edits one by its file. And what a version may hold is said
// with the upload URL.
func TestSeveralFilesOverHTTP(t *testing.T) {
	a := hardened(t, nil, nil, nil)
	c := a.c
	root, sato, yuki := a.tokenFor(c.Root), a.tokenFor(c.Sato), a.tokenFor(c.Students[0].Actor)
	course := "/v1/courses/" + c.Course.String()

	type file struct{ contentType, filename, body string }
	files := []file{
		{"application/pdf", "week3-slides.pdf", "%PDF slides"},
		{"application/msword", "handout 講義.doc", "the handout"},
		{"text/x-python", "loops.py", "print(1)\n"},
	}
	var named []m
	for i, f := range files {
		q := "?kind=material&content_type=" + url.QueryEscape(f.contentType)
		if i == 1 {
			q += "&filename=" + url.QueryEscape(f.filename)
		}
		ask := a.do(nil, "GET", course+"/upload-url"+q, sato, nil)
		if ask.Status != 200 {
			t.Fatalf("upload-url: %d %s", ask.Status, ask.Raw)
		}
		if ask.Body["result"].(map[string]any)["max_files"] != float64(20) ||
			ask.Body["result"].(map[string]any)["max_version_bytes"] != float64(200<<20) {
			t.Fatalf("the limits: %s", ask.Raw)
		}
		if res, _ := a.raw("PUT", a.here(ask.str("result", "upload_url")), f.contentType, []byte(f.body)); res.StatusCode != 200 {
			t.Fatalf("PUT %d: %d", i+1, res.StatusCode)
		}
		entry := m{"upload_token": ask.str("result", "upload_token")}
		if i != 1 {
			entry["filename"] = f.filename
		}
		named = append(named, entry)
	}
	both := a.do(nil, "POST", course+"/documents", sato, m{"kind": "material", "title": "Week 3", "files": named[:1],
		"upload_token": named[1]["upload_token"]}, "Idempotency-Key", "both")
	if both.Status != 400 || both.str("action_id") != "" || both.str("error", "details", "reason") != "files_and_upload_token" {
		t.Fatalf("files and upload_token: %d %s", both.Status, both.Raw)
	}
	made := a.do(nil, "POST", course+"/documents", sato, m{"kind": "material", "title": "Week 3", "body_md": "Slides first.",
		"files": named}, "Idempotency-Key", "week3")
	if made.Status != 200 {
		t.Fatalf("document.create: %d %s", made.Status, made.Raw)
	}
	doc := made.str("result", "document_id")
	fileIDs, _ := made.Body["result"].(map[string]any)["file_ids"].([]any)
	if len(fileIDs) != 3 {
		t.Fatalf("the file ids: %s", made.Raw)
	}
	a.do(nil, "POST", course+"/documents/"+doc+"/publish", sato, nil, "Idempotency-Key", "publish")

	read := a.do(nil, "GET", course+"/documents/"+doc, yuki, nil)
	version, _ := read.Body["result"].(map[string]any)["version"].(map[string]any)
	got, _ := version["files"].([]any)
	if read.Status != 200 || len(got) != 3 {
		t.Fatalf("document.get: %d %s", read.Status, read.Raw)
	}
	for i, f := range files {
		view := got[i].(map[string]any)
		if view["id"] != fileIDs[i] || view["position"] != float64(i+1) || view["filename"] != f.filename {
			t.Fatalf("file %d: %v", i+1, view)
		}
		res, body := a.raw("GET", a.here(view["download_url"].(string)), "", nil)
		if res.StatusCode != 200 || string(body) != f.body || res.Header.Get("Content-Type") != f.contentType ||
			!strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") {
			t.Fatalf("download %d: %d %v %q", i+1, res.StatusCode, res.Header, body)
		}
		one := a.do(nil, "GET", course+"/documents/"+doc+"/files/"+fileIDs[i].(string), yuki, nil)
		if one.Status != 200 || one.str("result", "filename") != f.filename {
			t.Fatalf("document.file %d: %d %s", i+1, one.Status, one.Raw)
		}
		if res, body := a.raw("GET", a.here(one.str("result", "download_url")), "", nil); res.StatusCode != 200 || string(body) != f.body {
			t.Fatalf("document.file %d's download: %d %q", i+1, res.StatusCode, body)
		}
	}
	// The handout is saved under its name, in any script.
	res, _ := a.raw("GET", a.here(got[1].(map[string]any)["download_url"].(string)), "", nil)
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "filename*=utf-8''handout%20%E8%AC%9B%E7%BE%A9.doc") {
		t.Fatalf("the handout's Content-Disposition: %s", cd)
	}
	// What the release before read is the first file's.
	if version["content_type"] != "application/pdf" || version["download_url"] != got[0].(map[string]any)["download_url"] {
		t.Fatalf("the version's own file fields: %v", version)
	}

	// The service claims each file on its own, and writes each back by its file.
	svc := a.do(nil, "POST", "/v1/services/document_text/credentials", root, m{"label": "runtime"}, "Idempotency-Key", "issue").str("result", "token")
	claimed := a.do(nil, "POST", "/v1/services/document_text/queue", svc, m{"max": 5})
	claims, _ := claimed.Body["result"].(map[string]any)["claimed"].([]any)
	if claimed.Status != 200 || len(claims) != 3 {
		t.Fatalf("the service claiming: %d %s", claimed.Status, claimed.Raw)
	}
	for i, c := range claims {
		claim := c.(map[string]any)
		v, fileID, lease := claim["version_id"].(string), claim["file_id"].(string), claim["lease_id"].(string)
		if fileID != fileIDs[i] || claim["filename"] != files[i].filename {
			t.Fatalf("claim %d: %v", i+1, claim)
		}
		again := a.do(nil, "GET", "/v1/services/document_text/versions/"+v+"/file?lease_id="+lease+"&file_id="+fileID, svc, nil)
		if again.Status != 200 || again.str("result", "file_id") != fileID {
			t.Fatalf("the file again: %d %s", again.Status, again.Raw)
		}
		done := a.do(nil, "POST", "/v1/services/document_text/versions/"+v+"/complete", svc, m{"lease_id": lease, "file_id": fileID,
			"status": "done", "body": "## " + files[i].filename, "pages": 1, "model": "A model"}, "Idempotency-Key", "done-"+lease)
		if done.Status != 200 || done.str("result", "file_id") != fileID {
			t.Fatalf("complete %d: %d %s", i+1, done.Status, done.Raw)
		}
	}
	for i, f := range files {
		text := a.do(nil, "GET", course+"/documents/"+doc+"/text?file_id="+fileIDs[i].(string), yuki, nil)
		if text.Status != 200 || text.str("result", "text", "body") != "## "+f.filename || text.str("result", "file_id") != fileIDs[i] {
			t.Fatalf("the text of file %d: %d %s", i+1, text.Status, text.Raw)
		}
	}

	// The instructor edits one by its file, and is asked which when he names none.
	v := version["id"].(string)
	if got := a.do(nil, "POST", course+"/documents/"+doc+"/versions/"+v+"/text", sato, m{"body": "## Slides"},
		"Idempotency-Key", "which"); got.Status != 400 || got.str("error", "details", "reason") != "file_id_required" {
		t.Fatalf("an edit naming no file: %d %s", got.Status, got.Raw)
	}
	if got := a.do(nil, "POST", course+"/documents/"+doc+"/versions/"+v+"/text", sato, m{"body": "## Slides", "file_id": fileIDs[0]},
		"Idempotency-Key", "edit"); got.Status != 200 || got.str("result", "file_id") != fileIDs[0] {
		t.Fatalf("an edit of the slides: %d %s", got.Status, got.Raw)
	}
}
