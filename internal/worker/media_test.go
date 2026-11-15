package worker

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"fujipic/internal/db"
	"fujipic/internal/imagefile"
	"fujipic/internal/model"
	"fujipic/internal/storage"
	"fujipic/internal/store"
)

type previewConverter struct{}

func (previewConverter) Convert(_ context.Context, _, _, tmp string, _ int64, _ int) (*imagefile.Info, error) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		return nil, err
	}
	return imagefile.Process(&b, tmp, 1<<20, 100, 100)
}

type deletingPut struct {
	storage.Store
	beforePut func()
}

func (s deletingPut) Put(ctx context.Context, key string, r io.Reader, size int64, mime string) error {
	if strings.HasSuffix(key, ".preview.png") {
		s.beforePut()
	}
	return s.Store.Put(ctx, key, r, size, mime)
}

func TestPreviewWorkerDeletionDuringPut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	u := &model.User{ID: "u", Email: "worker@example.test", PasswordHash: "hash", Role: "user", Status: "active", QuotaBytes: 1 << 20}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{ID: model.DefaultBucketID, Enabled: true, IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	img := &model.Image{ID: "i", UserID: u.ID, BucketID: model.DefaultBucketID, ObjectKey: "i/2026/11/aaaaaaaaaaaaaaaaaaaaaaaaaa.svg", MIME: "image/svg+xml", SizeBytes: 3, Width: 2, Height: 2, SHA256: "test", Visibility: "public"}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	s.ReconcileUserUsage(ctx)
	s.ReconcileStorageBucketUsage(ctx)
	objects := storage.NewMemory()
	if err := objects.Put(ctx, img.ObjectKey, strings.NewReader("svg"), 3, img.MIME); err != nil {
		t.Fatal(err)
	}
	wrapper := deletingPut{Store: objects, beforePut: func() {
		if _, err := s.SoftDeleteImage(ctx, u.ID, img.ID); err != nil {
			t.Fatal(err)
		}
		// Simulate the cleanup worker finishing before the in-flight PUT commits.
		pending, err := s.PendingDeletes(ctx, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pending {
			objects.Delete(ctx, p.ObjectKey)
			s.RemovePendingDelete(ctx, p.ObjectKey, p.BucketID)
		}
	}}
	m := Media{Store: s, Buckets: &mapBuckets{def: wrapper, stores: map[string]storage.Store{model.DefaultBucketID: wrapper}}, Client: previewConverter{}, TempDir: dir, MaxBytes: 1 << 20, MaxPixels: 100, MaxDim: 100, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.RunOnce(ctx)
	pending, err := s.PendingDeletes(ctx, 20)
	if err != nil || len(pending) != 1 || !strings.HasSuffix(pending[0].ObjectKey, ".preview.png") {
		t.Fatalf("late PUT not queued: %+v %v", pending, err)
	}
	u, err = s.UserByID(ctx, u.ID)
	if err != nil || u.UsedBytes != 0 {
		t.Fatal("quota leaked", u, err)
	}
}
