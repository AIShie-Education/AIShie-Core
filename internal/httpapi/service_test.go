package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// A site service's credential over HTTP: issued by root, taken at the
// service's REST routes, where it claims a file, reads it and writes its
// text back, and nowhere else: not at another tool, not at the other
// endpoints that take a credential, not at the agents' door. A text longer
// than the API takes of any other call is taken where a text is written.
func TestAServiceCredentialOverHTTP(t *testing.T) {
	a := hardened(t, nil, nil, nil)
	c := a.c
	root, sato, yuki := a.tokenFor(c.Root), a.tokenFor(c.Sato), a.tokenFor(c.Students[0].Actor)
	course := "/v1/courses/" + c.Course.String()

	issued := a.do(nil, "POST", "/v1/services/document_text/credentials", root, m{"label": "runtime"}, "Idempotency-Key", "issue")
	svc := issued.str("result", "token")
	if issued.Status != 200 || !strings.HasPrefix(svc, "aissvc_") {
		t.Fatalf("issued: %d %s", issued.Status, issued.Raw)
	}
	if got := a.do(nil, "POST", "/v1/services/document_text/credentials", sato, m{"label": "mine"}, "Idempotency-Key", "sato"); got.Status != 403 {
		t.Fatalf("the instructor issuing one: %d %s", got.Status, got.Raw)
	}

	ask := a.do(nil, "GET", course+"/upload-url?kind=material&content_type=application/pdf", sato, nil)
	if res, _ := a.raw("PUT", a.here(ask.str("result", "upload_url")), "application/pdf", []byte("%PDF slides")); res.StatusCode != 200 {
		t.Fatalf("PUT: %d", res.StatusCode)
	}
	made := a.do(nil, "POST", course+"/documents", sato, m{"kind": "material", "title": "Week 1",
		"files": []m{{"upload_token": ask.str("result", "upload_token"), "filename": "week1.pdf"}}}, "Idempotency-Key", "slides")
	doc := made.str("result", "document_id")
	a.do(nil, "POST", course+"/documents/"+doc+"/publish", sato, nil, "Idempotency-Key", "publish")

	// Nobody but the service claims.
	if got := a.do(nil, "POST", "/v1/services/document_text/queue", sato, m{}); got.Status != 403 || got.str("error", "details", "reason") != "service_only" {
		t.Fatalf("the instructor claiming: %d %s", got.Status, got.Raw)
	}
	claimed := a.do(nil, "POST", "/v1/services/document_text/queue", svc, m{"max": 2})
	claims, _ := claimed.Body["result"].(map[string]any)["claimed"].([]any)
	if claimed.Status != 200 || len(claims) != 1 {
		t.Fatalf("the service claiming: %d %s", claimed.Status, claimed.Raw)
	}
	claim := claims[0].(map[string]any)
	version, fileID, lease := claim["version_id"].(string), claim["file_id"].(string), claim["lease_id"].(string)
	if res, body := a.raw("GET", a.here(claim["download_url"].(string)), "", nil); res.StatusCode != 200 || string(body) != "%PDF slides" {
		t.Fatalf("the claimed file: %d %q", res.StatusCode, body)
	}
	file := a.do(nil, "GET", "/v1/services/document_text/versions/"+version+"/file?lease_id="+lease+"&file_id="+fileID, svc, nil)
	if file.Status != 200 || file.str("result", "download_url") == "" {
		t.Fatalf("the file again: %d %s", file.Status, file.Raw)
	}
	// More than a megabyte of text, which no other call carries.
	text := "## Page 1\n\n" + strings.Repeat("字", 500_000)
	done := a.do(nil, "POST", "/v1/services/document_text/versions/"+version+"/complete", svc,
		m{"lease_id": lease, "file_id": fileID, "status": "done", "body": text, "pages": 1, "model": "A model"}, "Idempotency-Key", "done")
	if done.Status != 200 {
		t.Fatalf("done: %d %.300s", done.Status, done.Raw)
	}
	read := a.do(nil, "GET", course+"/documents/"+doc+"/text?part=1&file_id="+fileID, yuki, nil)
	if read.Status != 200 || read.str("result", "text", "source") != "ai" || !strings.HasPrefix(read.str("result", "text", "body"), "## Page 1") {
		t.Fatalf("the student reading the text: %d %.300s", read.Status, read.Raw)
	}
	edit := a.do(nil, "POST", course+"/documents/"+doc+"/versions/"+version+"/text", sato, m{"body": text + "\n", "file_id": fileID},
		"Idempotency-Key", "edit")
	if edit.Status != 200 {
		t.Fatalf("the instructor writing a long text: %d %.300s", edit.Status, edit.Raw)
	}
	if got := a.do(nil, "POST", course+"/documents", sato, m{"kind": "material", "title": "Long", "body_md": text},
		"Idempotency-Key", "long"); got.Status != 400 || !strings.Contains(got.str("error", "message"), "larger than") {
		t.Fatalf("a document as long, where no long text is taken: %d %.300s", got.Status, got.Raw)
	}

	// Nothing else takes the service's credential.
	if got := a.do(nil, "GET", "/v1/me", svc, nil); got.Status != 403 || got.str("error", "details", "reason") != "not_for_services" {
		t.Fatalf("the service's /v1/me: %d %s", got.Status, got.Raw)
	}
	if got := a.do(nil, "GET", course+"/documents/"+doc, svc, nil); got.Status != 403 {
		t.Fatalf("the service reading a document: %d %s", got.Status, got.Raw)
	}
	if got := a.do(nil, "POST", "/v1/auth/logout", svc, nil); got.Status != 403 || got.str("error", "details", "reason") != "not_for_services" {
		t.Fatalf("the service signing out: %d %s", got.Status, got.Raw)
	}
	req, _ := http.NewRequest("POST", a.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+svc)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("the service at the agents' door: %d", res.StatusCode)
	}

	// Revoked, it is no credential, at once.
	id := issued.str("result", "credential_id")
	if got := a.do(nil, "POST", "/v1/services/document_text/credentials/"+id+"/revoke", root, nil, "Idempotency-Key", "revoke"); got.Status != 200 {
		t.Fatalf("revoking it: %d %s", got.Status, got.Raw)
	}
	if got := a.do(nil, "POST", "/v1/services/document_text/queue", svc, m{}); got.Status != 401 {
		t.Fatalf("a revoked credential claiming: %d %s", got.Status, got.Raw)
	}
}
