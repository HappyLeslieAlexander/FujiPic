package server

import (
	"context"
	"fmt"
	"fujipic/internal/bucket"
	"fujipic/internal/config"
	"fujipic/internal/db"
	"fujipic/internal/storage"
	"fujipic/internal/store"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingHealth struct {
	storage.Store
	calls atomic.Int32
}

func (c *countingHealth) Health(context.Context) error { c.calls.Add(1); return nil }

func TestReadyzCoalescesAndCaches(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	obj := &countingHealth{Store: storage.NewMemory()}
	buckets, err := bucket.NewRegistry(st, obj, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, buckets: buckets}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			s.handleReadyz(w, httptest.NewRequest("GET", "/readyz", nil))
			if w.Code != 200 {
				t.Errorf("status=%d", w.Code)
			}
		}()
	}
	wg.Wait()
	if obj.calls.Load() != 1 {
		t.Fatalf("cloud calls=%d", obj.calls.Load())
	}
}

func TestLimiterHasBoundedCapacity(t *testing.T) {
	l := newLimiter()
	for i := 0; i < 10001; i++ {
		l.allow(fmt.Sprint(i), 1, time.Minute)
	}
	if len(l.entries) != 10000 {
		t.Fatalf("entries=%d", len(l.entries))
	}
	if l.allow("new", 1, time.Minute) {
		t.Fatal("over-capacity key accepted")
	}
}

func TestUploadAdmissionBoundedBeforeBodyParsing(t *testing.T) {
	s := &Server{cfg: &config.Config{TmpDir: t.TempDir(), MaxUploadBytes: 1 << 20}, uploadSlots: make(chan struct{}, 8), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for i := 0; i < 8; i++ {
		if !s.acquireUpload(httptest.NewRecorder()) {
			t.Fatal("slot unavailable")
		}
	}
	w := httptest.NewRecorder()
	if s.acquireUpload(w) || w.Code != 503 {
		t.Fatal("excess upload accepted")
	}
	<-s.uploadSlots
	if !s.acquireUpload(httptest.NewRecorder()) {
		t.Fatal("slot not released")
	}
}

type countedBody struct {
	io.Reader
	read int
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (*countedBody) Close() error { return nil }

func TestChunkedFormParsingStopsAtLimit(t *testing.T) {
	s := &Server{cfg: &config.Config{AppBaseURL: "http://example.test"}}
	body := &countedBody{Reader: strings.NewReader("csrf=test&email=" + strings.Repeat("x", 1<<20))}
	r := httptest.NewRequest("POST", "http://example.test/login", nil)
	r.Body = body
	r.ContentLength = -1
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Cookie", "fujipic_csrf=test")
	r.Header.Set("X-CSRF-Token", "test")
	w := httptest.NewRecorder()
	s.withRequestLimits(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.validCSRF(r) {
			t.Error("oversized partial form accepted")
		}
	})).ServeHTTP(w, r)
	if body.read > (64<<10)+1 {
		t.Fatalf("read %d bytes", body.read)
	}
}
