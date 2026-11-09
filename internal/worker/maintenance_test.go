package worker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

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
	if err := st.AddPendingDelete(ctx, "i/2026/11/a.png"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCDNPurge(ctx, "https://img.example.com/i/2026/11/a.png"); err != nil {
		t.Fatal(err)
	}

	purger := &fakePurger{enabled: true}
	m := &Maintenance{Store: st, Objects: objects, Purger: purger, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
	m := &Maintenance{Store: st, Objects: storage.NewMemory(), Purger: purger, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
	m := &Maintenance{Store: st, Objects: storage.NewMemory(), Purger: purger, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.runOnce(ctx)
	pending, _ := st.PendingCDNPurges(ctx, 10)
	if len(pending) != 1 || pending[0].Attempts != 1 || pending[0].LastError == "" {
		t.Fatalf("pending = %+v", pending)
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
	m := &Maintenance{Store: st, Objects: storage.NewMemory(), Purger: &fakePurger{enabled: false}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.runOnce(ctx)
	pending, _ := st.PendingCDNPurges(ctx, 10)
	if len(pending) != 1 {
		t.Fatalf("disabled purger removed queue entry: %+v", pending)
	}
}
