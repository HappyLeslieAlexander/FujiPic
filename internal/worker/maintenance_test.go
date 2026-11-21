package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fujipic/internal/bucket"
	"fujipic/internal/cdn"
	"fujipic/internal/db"
	"fujipic/internal/storage"
	"fujipic/internal/store"
)

type fakePurger struct {
	called  []string
	fail    bool
	enabled bool
}

type batchPurger struct {
	batches [][]string
	enabled bool
}

func (f *batchPurger) Purge(ctx context.Context, url string) error {
	f.batches = append(f.batches, []string{url})
	return nil
}
func (f *batchPurger) PurgeBatch(ctx context.Context, urls []string) error {
	cp := make([]string, len(urls))
	copy(cp, urls)
	f.batches = append(f.batches, cp)
	return nil
}
func (f *batchPurger) Enabled() bool { return f.enabled }
func (f *batchPurger) Name() string  { return "batch-fake" }

func (f *fakePurger) Purge(ctx context.Context, url string) error {
	if f.fail {
		return context.DeadlineExceeded
	}
	f.called = append(f.called, url)
	return nil
}
func (f *fakePurger) Enabled() bool { return f.enabled }
func (f *fakePurger) Name() string  { return "fake" }

type mapBuckets struct {
	def    storage.Store
	stores map[string]storage.Store
}

type blockingDelete struct{ storage.Store }

func (b blockingDelete) Delete(ctx context.Context, key string) error { <-ctx.Done(); return ctx.Err() }

func TestMaintenanceDeleteTimeoutAllowsPurgeAndRecovery(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	objects := storage.NewMemory()
	if err := st.AddPendingDelete(ctx, "blocked", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCDNPurge(ctx, "https://img.test/blocked"); err != nil {
		t.Fatal(err)
	}
	purger := &fakePurger{enabled: true}
	m := &Maintenance{Store: st, Buckets: &mapBuckets{def: blockingDelete{objects}}, Purger: purger, ItemTimeout: 10 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.runOnce(ctx)
	if len(purger.called) != 1 {
		t.Fatal("delete timeout blocked purge")
	}
	var attempts int
	if err := database.QueryRow(`SELECT attempts FROM pending_deletes WHERE object_key='blocked'`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempts=%d %v", attempts, err)
	}
	if _, err := database.Exec(`UPDATE pending_deletes SET attempts=12,next_attempt_at=0`); err != nil {
		t.Fatal(err)
	}
	m.Buckets = &mapBuckets{def: objects}
	m.retryDeletes(ctx)
	if pending, err := st.HasPendingDelete(ctx, "blocked", ""); err != nil || pending {
		t.Fatalf("post-outage delete stuck: %v %v", pending, err)
	}
}

func (m *mapBuckets) Default() storage.Store { return m.def }
func (m *mapBuckets) Resolve(ctx context.Context, bucketID string) (storage.Store, error) {
	s, ok := m.stores[bucketID]
	if !ok {
		return nil, errors.New("unknown bucket")
	}
	return s, nil
}

func TestMaintenanceCleanupTempHandlesGlobMetacharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tmp[1]")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "fujipic-upload-stale")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := &Maintenance{
		TempDir: dir,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	m.cleanupTemp(context.Background())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp file was not removed: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated file was removed: %v", err)
	}
}

func TestMaintenanceRetriesCustomBucketDeletes(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	ctx := context.Background()

	defaultStore := storage.NewMemory()
	customStore := storage.NewMemory()
	if err := customStore.Put(ctx, "i/2026/11/custom.png", bytes.NewReader([]byte("abc")), 3, "image/png"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPendingDelete(ctx, "i/2026/11/custom.png", "bkt_custom"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPendingDelete(ctx, "i/2026/11/missing.png", "bkt_missing"); err != nil {
		t.Fatal(err)
	}
	m := &Maintenance{
		Store: st,
		Buckets: &mapBuckets{def: defaultStore, stores: map[string]storage.Store{
			"bkt_custom": customStore,
		}},
		Purger: &fakePurger{enabled: false},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	m.retryDeletes(ctx)

	if customStore.Len() != 0 {
		t.Fatalf("custom bucket still has %d objects", customStore.Len())
	}
	if _, err := database.Exec(`UPDATE pending_deletes SET next_attempt_at=0`); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingDeletes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].BucketID != "bkt_missing" || pending[0].Attempts != 1 {
		t.Fatalf("pending deletes = %+v, want only the unknown bucket with one attempt", pending)
	}
}

func TestMaintenanceRetriesDeletesAndPurges(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	objects := storage.NewMemory()
	ctx := context.Background()

	if err := objects.Put(ctx, "i/2026/11/a.png", bytes.NewReader([]byte("abc")), 3, "image/png"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPendingDelete(ctx, "i/2026/11/a.png", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCDNPurge(ctx, "https://img.example.com/i/2026/11/a.png"); err != nil {
		t.Fatal(err)
	}

	purger := &fakePurger{enabled: true}
	buckets, err := bucket.NewRegistry(st, objects, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	m := &Maintenance{Store: st, Buckets: buckets, Purger: purger, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.runOnce(ctx)

	if objects.Len() != 0 {
		t.Fatal("pending delete was not retried")
	}
	if pending, _ := st.PendingDeletes(ctx, 10); len(pending) != 0 {
		t.Fatalf("pending deletes left: %v", pending)
	}
	if len(purger.called) != 1 {
		t.Fatalf("purger called %d times", len(purger.called))
	}
	if pending, _ := st.PendingCDNPurges(ctx, 10); len(pending) != 0 {
		t.Fatalf("cdn purges left: %v", pending)
	}
}

func TestMaintenancePurgesCDNInOneBatch(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	ctx := context.Background()
	for _, url := range []string{"https://img.example.com/i/a.png", "https://img.example.com/i/b.png"} {
		if err := st.AddCDNPurge(ctx, url); err != nil {
			t.Fatal(err)
		}
	}
	purger := &batchPurger{enabled: true}
	buckets, _ := bucket.NewRegistry(st, storage.NewMemory(), "", nil, false)
	m := &Maintenance{Store: st, Buckets: buckets, Purger: purger, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.runOnce(ctx)
	if len(purger.batches) != 1 || len(purger.batches[0]) != 2 {
		t.Fatalf("batches = %v", purger.batches)
	}
}

func TestMaintenanceRecordsPurgeFailure(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	ctx := context.Background()
	if err := st.AddCDNPurge(ctx, "https://img.example.com/i/a.png"); err != nil {
		t.Fatal(err)
	}
	purger := &fakePurger{enabled: true, fail: true}
	buckets, _ := bucket.NewRegistry(st, storage.NewMemory(), "", nil, false)
	m := &Maintenance{Store: st, Buckets: buckets, Purger: purger, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.runOnce(ctx)
	if _, err := database.Exec(`UPDATE cdn_purges SET next_attempt_at=0`); err != nil {
		t.Fatal(err)
	}
	pending, _ := st.PendingCDNPurges(ctx, 10)
	if len(pending) != 1 || pending[0].Attempts != 1 || pending[0].LastError == "" {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestMaintenanceRetainsUnconfirmedCloudflarePurge(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	const imageURL = "https://img.example.com/i/a.png"
	if err := st.AddCDNPurge(ctx, imageURL); err != nil {
		t.Fatal(err)
	}
	responses := make(chan string, 2)
	responses <- `<html>upstream error</html>`
	responses <- `{"success":true}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, <-responses)
	}))
	defer ts.Close()
	m := &Maintenance{
		Store:  st,
		Purger: &cdn.Cloudflare{ZoneID: "zone", Token: "token", Endpoint: ts.URL, Client: ts.Client()},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	m.purgeCDN(ctx)
	if _, err := database.Exec(`UPDATE cdn_purges SET next_attempt_at=0`); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingCDNPurges(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].URL != imageURL || pending[0].Attempts != 1 || pending[0].LastError == "" {
		t.Fatalf("unconfirmed purge must remain queued with a failure: %+v", pending)
	}
	m.purgeCDN(ctx)
	pending, err = st.PendingCDNPurges(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("confirmed retry must remove the queue entry: %+v", pending)
	}
}

func TestMaintenanceSkipsDisabledPurger(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	ctx := context.Background()
	if err := st.AddCDNPurge(ctx, "https://img.example.com/i/a.png"); err != nil {
		t.Fatal(err)
	}
	buckets, _ := bucket.NewRegistry(st, storage.NewMemory(), "", nil, false)
	m := &Maintenance{Store: st, Buckets: buckets, Purger: &fakePurger{enabled: false}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.runOnce(ctx)
	pending, _ := st.PendingCDNPurges(ctx, 10)
	if len(pending) != 1 {
		t.Fatalf("disabled purger removed queue entry: %+v", pending)
	}
}
