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
	"sync"
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

// testBuckets is a small resolver used by unit tests. The default ID maps to
// the in-memory store; named buckets map to additional stores.
type testBuckets struct {
	def       storage.Store
	stores    map[string]storage.Store
	metas     map[string]*model.StorageBucket
	selectID  string
	selectErr error
	released  map[string]int64
}

func (b *testBuckets) Resolve(ctx context.Context, bucketID string) (storage.Store, error) {
	if bucketID == "" || bucketID == model.DefaultBucketID {
		return b.def, nil
	}
	s, ok := b.stores[bucketID]
	if !ok {
		return nil, errors.New("bucket not found")
	}
	return s, nil
}

func (b *testBuckets) SelectForUpload(ctx context.Context, size int64) (storage.Store, *model.StorageBucket, error) {
	if b.selectErr != nil {
		return nil, nil, b.selectErr
	}
	if b.selectID != "" {
		return b.stores[b.selectID], b.metas[b.selectID], nil
	}
	return b.def, &model.StorageBucket{ID: model.DefaultBucketID}, nil
}

func (b *testBuckets) Release(ctx context.Context, bucketID string, size int64) error {
	if b.released != nil {
		b.released[bucketID] += size
	}
	return nil
}

// failPutStore injects storage write failures so upload rollback can be
// checked without touching a real bucket.
type failPutStore struct {
	storage.Store
}

func (f *failPutStore) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	return errors.New("injected put failure")
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
		Store: st,
		Buckets: &testBuckets{
			def: objs, stores: map[string]storage.Store{},
			metas: map[string]*model.StorageBucket{}, released: map[string]int64{},
		},
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
	entered     chan struct{}
	enteredOnce sync.Once
}

func (s *cancelOnPutStore) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	s.enteredOnce.Do(func() { close(s.entered) })
	<-ctx.Done()
	return ctx.Err()
}

// TestUploadReleaseSurvivesContextCancellation verifies CONC-02: once a
// client's context has been canceled, the compensating quota release still
// completes rather than locking the reserved bytes forever.
func TestUploadReleaseSurvivesContextCancellation(t *testing.T) {
	svc, st, objs := newTestService(t)
	user := newTestUser(t, st)
	blocking := &cancelOnPutStore{Store: objs, entered: make(chan struct{})}
	svc.Buckets.(*testBuckets).def = blocking

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-blocking.entered
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

	svc.Buckets.(*testBuckets).def = &failDeleteStore{Store: objs, fail: true}
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
	if err != nil || len(dels) != 1 || dels[0].ObjectKey != res.Image.ObjectKey {
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

// TestDeleteClearsPendingCleanupAfterObjectDelete verifies that the
// transactional pending-delete row added by SoftDeleteImage is removed once
// the object is actually gone. Leaving it behind would make the maintenance
// worker retry a key that no longer exists forever.
func TestDeleteClearsPendingCleanupAfterObjectDelete(t *testing.T) {
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
	if err := svc.Delete(ctx, user.ID, res.Image.ID, "127.0.0.1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if objs.Len() != 0 {
		t.Fatalf("bucket objects = %d after delete, want 0", objs.Len())
	}
	dels, err := st.PendingDeletes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dels) != 0 {
		t.Fatalf("pending deletes = %+v after successful object delete, want none", dels)
	}
}

// TestUploadStoresImageInSelectedBucket pins automatic pool selection: the
// service uses the bucket chosen by the resolver and records its ID.
func TestUploadStoresImageInSelectedBucket(t *testing.T) {
	svc, st, def := newTestService(t)
	ctx := context.Background()
	user := newTestUser(t, st)
	custom := storage.NewMemory()
	now := time.Now().Unix()
	if err := st.CreateStorageBucket(ctx, &model.StorageBucket{
		ID: "bkt_custom", Name: "Tokyo", Region: "ap-tokyo-1", Bucket: "tokyo",
		AccessKeyID: "key", SecretCiphertext: "cipher", MaxBytes: 1 << 20,
		Priority: 10, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	tb := svc.Buckets.(*testBuckets)
	tb.stores["bkt_custom"] = custom
	tb.metas["bkt_custom"] = &model.StorageBucket{ID: "bkt_custom", Name: "Tokyo"}
	tb.selectID = "bkt_custom"

	res, err := svc.Upload(ctx, user, bytes.NewReader(testPNG(t)), "custom.png", "public")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Image.BucketID != "bkt_custom" {
		t.Fatalf("image bucket = %q, want bkt_custom", res.Image.BucketID)
	}
	if custom.Len() != 1 || def.Len() != 0 {
		t.Fatalf("objects custom=%d default=%d, want 1 and 0", custom.Len(), def.Len())
	}

	if err := svc.Delete(ctx, user.ID, res.Image.ID, "127.0.0.1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if custom.Len() != 0 {
		t.Fatalf("custom bucket still has %d objects after delete", custom.Len())
	}
}

// TestUploadNoCapacityReleasesQuota verifies that a full pool rejects the
// upload without charging the user's quota.
func TestUploadNoCapacityReleasesQuota(t *testing.T) {
	svc, st, def := newTestService(t)
	ctx := context.Background()
	user := newTestUser(t, st)
	tb := svc.Buckets.(*testBuckets)
	tb.selectErr = store.ErrBucketCapacity

	if _, err := svc.Upload(ctx, user, bytes.NewReader(testPNG(t)), "full.png", "public"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Upload = %v, want ErrNoCapacity", err)
	}
	got, err := st.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedBytes != 0 {
		t.Fatalf("used_bytes = %d after full-pool rejection, want 0", got.UsedBytes)
	}
	if def.Len() != 0 {
		t.Fatalf("default bucket has %d objects after full-pool rejection", def.Len())
	}
}

// TestUploadStorageFailureReleasesBucketCapacity verifies that reserved bucket
// capacity is returned when the object write fails.
func TestUploadStorageFailureReleasesBucketCapacity(t *testing.T) {
	svc, st, def := newTestService(t)
	ctx := context.Background()
	user := newTestUser(t, st)
	svc.Buckets.(*testBuckets).def = &failPutStore{Store: def}
	data := testPNG(t)

	if _, err := svc.Upload(ctx, user, bytes.NewReader(data), "fail.png", "public"); !errors.Is(err, ErrStorage) {
		t.Fatalf("Upload = %v, want wrapped ErrStorage", err)
	}
	tb := svc.Buckets.(*testBuckets)
	if tb.released[model.DefaultBucketID] != int64(len(data)) {
		t.Fatalf("released bucket bytes = %d, want %d", tb.released[model.DefaultBucketID], len(data))
	}
	got, err := st.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedBytes != 0 {
		t.Fatalf("used_bytes = %d after storage failure, want 0", got.UsedBytes)
	}
}
