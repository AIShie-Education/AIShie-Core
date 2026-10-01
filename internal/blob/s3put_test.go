package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// multipartBucket is as much of an S3 bucket as a multipart upload needs:
// it is begun, its parts are taken one by one, and it is completed into an
// object or abandoned. It keeps how large each part was, which is what the
// store held in memory at a time.
type multipartBucket struct {
	mu        sync.Mutex
	uploads   map[string]map[int][]byte
	objects   map[string][]byte
	partSizes []int
	aborted   int
}

func (b *multipartBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/aishie/")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := fmt.Sprintf("upload-%d", len(b.uploads)+1)
		b.uploads[id] = map[int][]byte{}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>aishie</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		parts, ok := b.uploads[q.Get("uploadId")]
		if !ok {
			http.Error(w, "no such upload", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "aws-chunked" {
			body = unchunk(body)
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		parts[n] = body
		b.partSizes = append(b.partSizes, len(body))
		sum := sha256.Sum256(body)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts, ok := b.uploads[q.Get("uploadId")]
		if !ok {
			http.Error(w, "no such upload", http.StatusNotFound)
			return
		}
		var done struct {
			Parts []struct {
				PartNumber int
			} `xml:"Part"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := xml.Unmarshal(body, &done); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var object []byte
		for _, p := range done.Parts {
			object = append(object, parts[p.PartNumber]...)
		}
		b.objects[key] = object
		delete(b.uploads, q.Get("uploadId"))
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>aishie</Bucket><Key>%s</Key><ETag>"done"</ETag></CompleteMultipartUploadResult>`, key)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(b.uploads, q.Get("uploadId"))
		b.aborted++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "not here", http.StatusNotImplemented)
	}
}

// unchunk is a body sent aws-chunked, as a request signed chunk by chunk is
// sent over plain HTTP, without its chunks' sizes and signatures:
// <size in hex>;chunk-signature=<sig>\r\n<bytes>\r\n, to a chunk of none.
func unchunk(body []byte) []byte {
	var out []byte
	for len(body) > 0 {
		head, rest, ok := bytes.Cut(body, []byte("\r\n"))
		if !ok {
			break
		}
		size, _, _ := strings.Cut(string(head), ";")
		n, err := strconv.ParseInt(size, 16, 64)
		if err != nil || n == 0 || int64(len(rest)) < n {
			break
		}
		out = append(out, rest[:n]...)
		body = bytes.TrimPrefix(rest[n:], []byte("\r\n"))
	}
	return out
}

// Put streams what it is given to the store a part at a time, holding one
// part in memory, never the whole: an export of conversations is written as
// it is read. Past its limit nothing is kept: the upload is abandoned.
func TestS3StorePutStreamsAPartAtATime(t *testing.T) {
	bucket := &multipartBucket{uploads: map[string]map[int][]byte{}, objects: map[string][]byte{}}
	srv := httptest.NewServer(bucket)
	defer srv.Close()
	s, err := NewS3Store(S3Config{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Bucket: "aishie", Region: "us-east-1",
		AccessKey: "access", SecretKey: "secret", BucketLookup: "path"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := bytes.Repeat([]byte("對話紀錄,"), (putPartSize+putPartSize/2)/13)
	info, err := s.Put(ctx, "exports/a.csv", "text/csv; charset=utf-8", io.MultiReader(bytes.NewReader(body)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if info.Size != int64(len(body)) || info.Checksum != "sha256:"+hex.EncodeToString(sum[:]) || info.ContentType != "text/csv; charset=utf-8" {
		t.Fatalf("put: %+v", info)
	}
	if !bytes.Equal(bucket.objects["exports/a.csv"], body) {
		t.Fatal("the object is not what was put")
	}
	if len(bucket.partSizes) != 2 || bucket.partSizes[0] != putPartSize {
		t.Fatalf("parts of %v bytes, want one of %d and the rest", bucket.partSizes, putPartSize)
	}

	if _, err := s.Put(ctx, "exports/b.csv", "text/csv", bytes.NewReader(body), putPartSize+1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("past its limit: %v, want ErrTooLarge", err)
	}
	if _, kept := bucket.objects["exports/b.csv"]; kept || len(bucket.uploads) != 0 || bucket.aborted != 1 {
		t.Fatalf("past its limit, the store kept %v, %d uploads open, %d abandoned", kept, len(bucket.uploads), bucket.aborted)
	}
}
