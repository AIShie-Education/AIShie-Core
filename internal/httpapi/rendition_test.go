package httpapi_test

import (
	"bytes"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Core/internal/testkit"
)

// syncBuffer is a log's output, written by the server's goroutines and read
// by the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// firstFile is the first file of the version a document.get answered with.
func firstFile(r response) map[string]any {
	version, _ := r.Body["result"].(map[string]any)["version"].(map[string]any)
	files, _ := version["files"].([]any)
	if len(files) == 0 {
		return map[string]any{}
	}
	f, _ := files[0].(map[string]any)
	return f
}

// renditionOf is a file's rendition, as a view gives it.
func renditionOf(file map[string]any) map[string]any {
	r, _ := file["rendition"].(map[string]any)
	return r
}

// An Office file's PDF, over HTTP all the way: the instructor uploads a Word
// file; the site's agent runtime, with a credential root issues it, claims
// it under /v1/services/agent_runtime/, downloads it, asks where to put the
// PDF, PUTs a PDF larger than any upload the server takes otherwise, and
// says it is done; the student reads the file and opens its PDF, shown
// where it is opened, as a PDF, under the file's name; and nothing of the
// URLs is written to the log.
func TestARenditionOverHTTP(t *testing.T) {
	var logged syncBuffer
	a := hardened(t, nil, nil, slog.New(slog.NewTextHandler(&logged, nil)))
	c := a.c
	root, sato, yuki := a.tokenFor(c.Root), a.tokenFor(c.Sato), a.tokenFor(c.Students[0].Actor)
	course := "/v1/courses/" + c.Course.String()
	const docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

	ask := a.do(nil, "GET", course+"/upload-url?kind=material&content_type="+url.QueryEscape(docx)+"&filename="+
		url.QueryEscape("講義 week 5.docx"), sato, nil)
	if res, _ := a.raw("PUT", a.here(ask.str("result", "upload_url")), docx, []byte("PK the handout")); res.StatusCode != 200 {
		t.Fatalf("PUT the handout: %d", res.StatusCode)
	}
	made := a.do(nil, "POST", course+"/documents", sato, m{"kind": "material", "title": "Week 5",
		"files": []m{{"upload_token": ask.str("result", "upload_token")}}}, "Idempotency-Key", "week5")
	doc := made.str("result", "document_id")
	a.do(nil, "POST", course+"/documents/"+doc+"/publish", sato, nil, "Idempotency-Key", "publish")
	read := a.do(nil, "GET", course+"/documents/"+doc, yuki, nil)
	if r := renditionOf(firstFile(read)); r == nil || r["state"] != "queued" {
		t.Fatalf("before it is converted: %s", read.Raw)
	}

	rt := a.do(nil, "POST", "/v1/services/agent_runtime/credentials", root, m{"label": "runtime"}, "Idempotency-Key", "issue").
		str("result", "token")
	if got := a.do(nil, "POST", "/v1/services/agent_runtime/renditions/claim", sato, m{}); got.Status != 403 ||
		got.str("error", "details", "reason") != "service_only" {
		t.Fatalf("a person claiming: %d %s", got.Status, got.Raw)
	}
	claimed := a.do(nil, "POST", "/v1/services/agent_runtime/renditions/claim", rt, m{"max": 2, "lease_s": 300})
	claims, _ := claimed.Body["result"].(map[string]any)["claimed"].([]any)
	if claimed.Status != 200 || len(claims) != 1 {
		t.Fatalf("the runtime claiming: %d %s", claimed.Status, claimed.Raw)
	}
	claim := claims[0].(map[string]any)
	id, lease := claim["rendition_id"].(string), claim["lease_id"].(string)
	if claim["source"] != "document_file" || claim["filename"] != "講義 week 5.docx" {
		t.Fatalf("the claim: %v", claim)
	}
	if res, body := a.raw("GET", a.here(claim["download_url"].(string)), "", nil); res.StatusCode != 200 || string(body) != "PK the handout" {
		t.Fatalf("the runtime's download: %d %q", res.StatusCode, body)
	}
	again := a.do(nil, "GET", "/v1/services/agent_runtime/renditions/"+id+"/file?lease_id="+lease, rt, nil)
	if again.Status != 200 {
		t.Fatalf("the file again: %d %s", again.Status, again.Raw)
	}
	if got := a.do(nil, "POST", "/v1/services/agent_runtime/renditions/"+id+"/renew", rt, m{"lease_id": lease, "lease_s": 600}); got.Status != 200 {
		t.Fatalf("renewing: %d %s", got.Status, got.Raw)
	}
	up := a.do(nil, "GET", "/v1/services/agent_runtime/renditions/"+id+"/upload-url?lease_id="+lease, rt, nil)
	if up.Status != 200 || up.str("result", "headers", "Content-Type") != "application/pdf" {
		t.Fatalf("the upload URL: %d %s", up.Status, up.Raw)
	}
	// Larger than any upload the server takes otherwise, which a PDF of
	// slides may be.
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("0"), 2*testkit.MaxUploadBytes)...)
	if res, _ := a.raw("PUT", a.here(up.str("result", "upload_url")), "text/html", pdf); res.StatusCode != 400 {
		t.Fatalf("PUT as HTML: %d", res.StatusCode)
	}
	if res, body := a.raw("PUT", a.here(up.str("result", "upload_url")), "application/pdf", pdf); res.StatusCode != 200 {
		t.Fatalf("PUT the PDF: %d %s", res.StatusCode, body)
	}
	done := a.do(nil, "POST", "/v1/services/agent_runtime/renditions/"+id+"/complete", rt, m{"lease_id": lease, "status": "done",
		"upload_token": up.str("result", "upload_token"), "page_count": 4}, "Idempotency-Key", "done")
	if done.Status != 200 || done.str("result", "state") != "done" {
		t.Fatalf("complete: %d %s", done.Status, done.Raw)
	}

	// The student opens it.
	read = a.do(nil, "GET", course+"/documents/"+doc, yuki, nil)
	r := renditionOf(firstFile(read))
	rendition, _ := r["download_url"].(string)
	if r == nil || r["state"] != "done" || rendition == "" || r["page_count"] != float64(4) || r["byte_size"] != float64(len(pdf)) {
		t.Fatalf("after it is converted: %s", read.Raw)
	}
	res, body := a.raw("GET", a.here(rendition), "", nil)
	if res.StatusCode != 200 || !bytes.Equal(body, pdf) || res.Header.Get("Content-Type") != "application/pdf" ||
		res.Header.Get("Content-Disposition") != "inline; filename*=utf-8''%E8%AC%9B%E7%BE%A9%20week%205.pdf" ||
		res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Cache-Control") != "private, no-store" ||
		strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("the PDF: %d %v", res.StatusCode, res.Header)
	}
	// The file itself is still a download.
	res, _ = a.raw("GET", a.here(firstFile(read)["download_url"].(string)), "", nil)
	if !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("the original: %v", res.Header)
	}
	// What the URLs carry is never logged.
	for what, secret := range map[string]string{"the upload URL": up.str("result", "upload_url"), "the PDF's URL": rendition,
		"the upload token": up.str("result", "upload_token"), "the runtime's download": claim["download_url"].(string)} {
		token := secret[strings.LastIndex(secret, "/")+1:]
		if strings.Contains(logged.String(), token) {
			t.Fatalf("%s is in the log", what)
		}
	}
}
