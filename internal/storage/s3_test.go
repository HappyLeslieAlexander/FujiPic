package storage

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a deliberately small HTTP double for the slice of the S3 API used
// by this adapter. It is not MinIO: it records objects in memory and requires
// an Authorization header so anonymous access is visibly separate from the
// signed adapter calls.
type fakeS3 struct {
	mu       sync.Mutex
	bucket   string
	access   string
	secret   string
	objects  map[string]*fakeS3Object
	verified int
}

type fakeS3Object struct {
	body        []byte
	contentType string
	etag        string
	modified    time.Time
}

func newFakeS3(t *testing.T, bucket string) (*fakeS3, *httptest.Server) {
	t.Helper()
	s := &fakeS3{
		bucket:  bucket,
		access:  "test-access",
		secret:  "test-secret",
		objects: make(map[string]*fakeS3Object),
	}
	ts := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(ts.Close)
	return s, ts
}

func (f *fakeS3) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	if parts[0] != f.bucket {
		writeFakeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket does not exist")
		return
	}

	if r.Header.Get("Authorization") == "" {
		writeFakeS3Error(w, http.StatusForbidden, "AccessDenied", "anonymous access is denied")
		return
	}
	f.verified++

	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}

	switch {
	case key == "" && r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case key == "" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>`+f.bucket+`</Name><IsTruncated>false</IsTruncated></ListBucketResult>`)
	case r.Method == http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeFakeS3Error(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		sum := md5.Sum(body)
		f.mu.Lock()
		f.objects[key] = &fakeS3Object{
			body:        body,
			contentType: r.Header.Get("Content-Type"),
			etag:        `"` + hex.EncodeToString(sum[:]) + `"`,
			modified:    time.Now().UTC(),
		}
		f.mu.Unlock()
		w.Header().Set("ETag", f.objects[key].etag)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodHead:
		obj := f.object(key)
		if obj == nil {
			writeFakeS3Error(w, http.StatusNotFound, "NoSuchKey", "object does not exist")
			return
		}
		f.writeObjectHeaders(w, obj)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		obj := f.object(key)
		if obj == nil {
			writeFakeS3Error(w, http.StatusNotFound, "NoSuchKey", "object does not exist")
			return
		}
		f.writeObjectHeaders(w, obj)
		_, _ = w.Write(obj.body)
	case r.Method == http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeFakeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported method")
	}
}

func (f *fakeS3) object(key string) *fakeS3Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

func (f *fakeS3) writeObjectHeaders(w http.ResponseWriter, obj *fakeS3Object) {
	w.Header().Set("Content-Type", obj.contentType)
	w.Header().Set("ETag", obj.etag)
	w.Header().Set("Last-Modified", obj.modified.Format(http.TimeFormat))
	w.Header().Set("Content-Length", strings.TrimSpace(intToString(int64(len(obj.body)))))
}

func writeFakeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+code+`</Code><Message>`+message+`</Message></Error>`)
}

func intToString(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 20)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestS3AdapterAgainstFakePrivateBucket(t *testing.T) {
	ctx := context.Background()
	fake, ts := newFakeS3(t, "private-bucket")
	store, err := NewS3(ctx, S3Options{
		Endpoint:        ts.URL,
		Region:          "us-east-1",
		Bucket:          "private-bucket",
		AccessKeyID:     fake.access,
		SecretAccessKey: fake.secret,
		ForcePathStyle:  true,
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}

	if err := store.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}

	key := "i/2026/11/test.png"
	body := strings.NewReader("fake image bytes")
	if err := store.Put(ctx, key, body, int64(len("fake image bytes")), "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	head, err := store.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Size != int64(len("fake image bytes")) || head.ContentType != "image/png" || head.ETag == "" {
		t.Fatalf("unexpected Head metadata: %+v", head)
	}

	got, err := store.Get(ctx, key, "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	gotBody, err := io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(gotBody) != "fake image bytes" || got.ContentType != "image/png" || got.Size != int64(len(gotBody)) {
		t.Fatalf("unexpected Get result: body=%q metadata=%+v", gotBody, got)
	}

	anonResp, err := http.Get(ts.URL + "/private-bucket/" + key)
	if err != nil {
		t.Fatalf("anonymous probe: %v", err)
	}
	_, _ = io.Copy(io.Discard, anonResp.Body)
	anonResp.Body.Close()
	if anonResp.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous Get status = %d, want 403", anonResp.StatusCode)
	}

	if fake.verified == 0 {
		t.Fatal("fake S3 received no signed adapter request")
	}

	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, key, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted Get error = %v, want ErrNotFound", err)
	}
}
