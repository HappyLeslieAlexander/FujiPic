package upload

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"fujipic/internal/db"
	"fujipic/internal/model"
	"fujipic/internal/storage"
	"fujipic/internal/store"
)

// failDeleteStore injects storage delete failures to exercise the
// pending-delete retry queue in Service.Delete.
type failDeleteStore struct {
	storage.Store
	fail bool
}

func (f *failDeleteStore) Delete(ctx context.Context, key string) error {
	if f.fail {
		return errors.New("injected storage failure")
	}
	return f.Store.Delete(ctx, key)
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{200, 10, 10, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestService(t *testing.T) (*Service, *store.Store, *storage.Memory) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	objs := storage.NewMemory()
	svc := &Service{
		Store:      st,
		Objects:    objs,
		PublicBase: "https://img.example.com",
		TmpDir:     filepath.Join(dir, "tmp"),
		MaxBytes:   1 << 20,
		MaxPixels:  50_000_000,
		MaxDim:     12000,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return svc, st, objs
}

func newTestUser(t *testing.T, st *store.Store) *model.User {
	t.Helper()
	now := time.Now().Unix()
	user := &model.User{
		ID: "usr_test", Email: "t@example.com", PasswordHash: "placeholder",
		Role: "user", Status: "active", QuotaBytes: 1 << 20,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	return user
}

// cancelOnPutStore blocks until the request context is canceled, simulating
// a client connection disappearing in the middle of the storage write.
type cancelOnPutStore struct {
	storage.Store
}

func (s *cancelOnPutStore) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestUploadReleaseSurvivesContextCancellation verifies CONC-02: once a
// client's context has been canceled, the compensating quota release still
// completes rather than locking the reserved bytes forever.
func TestUploadReleaseSurvivesContextCancellation(t *testing.T) {
	svc, st, objs := newTestService(t)
	user := newTestUser(t, st)
	svc.Objects = &cancelOnPutStore{Store: objs}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := svc.Upload(ctx, user, bytes.NewReader(testPNG(t)), "test.png", "public")
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("Upload = %v, want wrapped ErrStorage", err)
	}
	got, lookupErr := st.UserByID(context.Background(), user.ID)
	if lookupErr != nil {
		t.Fatal(lookupErr)
	}
	if got.UsedBytes != 0 {
		t.Fatalf("used_bytes = %d after canceled upload, want 0", got.UsedBytes)
	}
}

// TestDeleteQueuesRetryOnStorageFailure pins PLAN 1216: when the bucket
// delete fails, deletion still succeeds logically and both the object
// delete and the CDN purge are queued for the maintenance worker.
func TestDeleteQueuesRetryOnStorageFailure(t *testing.T) {
	svc, st, objs := newTestService(t)
	ctx := context.Background()
	user := newTestUser(t, st)

	res, err := svc.Upload(ctx, user, bytes.NewReader(testPNG(t)), "test.png", "public")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if objs.Len() != 1 {
		t.Fatalf("bucket objects = %d, want 1", objs.Len())
	}

	svc.Objects = &failDeleteStore{Store: objs, fail: true}
	if err := svc.Delete(ctx, user.ID, res.Image.ID, "127.0.0.1"); err != nil {
		t.Fatalf("Delete = %v, want nil (failure is queued, not returned)", err)
	}

	// Object remains (delete failed) but metadata is soft-deleted.
	if objs.Len() != 1 {
		t.Fatalf("bucket objects = %d, want 1 (delete failed, object kept)", objs.Len())
	}
	if _, err := st.ImageByObjectKey(ctx, res.Image.ObjectKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("soft-deleted image lookup = %v, want ErrNotFound", err)
	}

	// Retry queues hold both the object key and the CDN URL.
	dels, err := st.PendingDeletes(ctx, 10)
	if err != nil || len(dels) != 1 || dels[0] != res.Image.ObjectKey {
		t.Fatalf("pending deletes = %v, %v; want [%s]", dels, err, res.Image.ObjectKey)
	}
	purges, err := st.PendingCDNPurges(ctx, 10)
	if err != nil || len(purges) != 1 || purges[0].URL != svc.URL(res.Image) {
		t.Fatalf("pending cdn purges = %+v, %v; want one entry for %s", purges, err, svc.URL(res.Image))
	}

	// Quota is released even though the object delete failed.
	got, err := st.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedBytes != 0 {
		t.Fatalf("used_bytes = %d after delete, want 0", got.UsedBytes)
	}
}
