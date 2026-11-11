package bucket

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fujipic/internal/db"
	"fujipic/internal/model"
	"fujipic/internal/storage"
	"fujipic/internal/store"
)

func testKey() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return store.New(database)
}

type memoryFactory struct {
	stores map[string]*storage.Memory
	seen   map[string]string
	calls  int
}

func newMemoryFactory() *memoryFactory {
	return &memoryFactory{stores: make(map[string]*storage.Memory), seen: make(map[string]string)}
}

func (f *memoryFactory) build(ctx context.Context, b model.StorageBucket, secret string) (storage.Store, error) {
	f.calls++
	f.seen[b.ID] = secret
	m := storage.NewMemory()
	f.stores[b.ID] = m
	return m, nil
}

func newTestRegistry(t *testing.T, st *store.Store, key string) (*Registry, *storage.Memory, *memoryFactory) {
	t.Helper()
	def := storage.NewMemory()
	factory := newMemoryFactory()
	reg, err := NewRegistry(st, def, key, factory.build, false)
	if err != nil {
		t.Fatal(err)
	}
	return reg, def, factory
}

func TestAddResolveAndEncryptAtRest(t *testing.T) {
	st := newTestStore(t)
	reg, _, factory := newTestRegistry(t, st, testKey())
	if !reg.Enabled() {
		t.Fatal("registry with a key should be enabled")
	}
	ctx := context.Background()
	b, err := reg.Add(ctx, AddInput{
		Name: "OCI Tokyo", Region: "ap-tokyo-1", Bucket: "my-private-bucket",
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "super-secret",
		ForcePathStyle: true, MaxBytes: 1 << 30, Priority: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if factory.seen[b.ID] != "super-secret" {
		t.Fatalf("factory secret = %q", factory.seen[b.ID])
	}
	row, err := st.StorageBucketByID(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.SecretCiphertext == "" || strings.Contains(row.SecretCiphertext, "super-secret") {
		t.Fatalf("secret was not encrypted at rest: %q", row.SecretCiphertext)
	}
	if !row.ForcePathStyle || row.MaxBytes != 1<<30 || row.Priority != 5 || !row.Enabled {
		t.Fatalf("bucket settings not persisted: %+v", row)
	}

	// A fresh registry must decrypt the stored secret to build the client.
	fresh, freshDef, freshFactory := newTestRegistry(t, st, testKey())
	if _, err := fresh.Resolve(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if freshFactory.seen[b.ID] != "super-secret" {
		t.Fatalf("decrypted secret = %q", freshFactory.seen[b.ID])
	}

	// List results must not expose ciphertext. The migration pre-creates the
	// fixed default row, so the pool contains it plus the custom bucket.
	list, err := fresh.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != model.DefaultBucketID || list[1].ID != b.ID {
		t.Fatalf("List = %+v, %v", list, err)
	}
	for _, item := range list {
		if item.SecretCiphertext != "" {
			t.Fatalf("List exposed secret ciphertext for %s", item.ID)
		}
	}

	// Empty and fixed default IDs always resolve to the environment bucket.
	for _, id := range []string{"", model.DefaultBucketID} {
		got, err := fresh.Resolve(ctx, id)
		if err != nil || got != freshDef {
			t.Fatalf("Resolve(%q) = %v, %v; want default store", id, got, err)
		}
	}
	if _, err := fresh.Resolve(ctx, "bkt_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(missing) = %v, want ErrNotFound", err)
	}
}

func TestEnsureDefaultAndAutomaticSelection(t *testing.T) {
	if DefaultPriority != 0 {
		t.Fatalf("default bucket initial priority = %d, want 0", DefaultPriority)
	}
	st := newTestStore(t)
	reg, def, factory := newTestRegistry(t, st, testKey())
	ctx := context.Background()
	if err := reg.EnsureDefault(ctx, "https://s3.example.com", "us-east-1", "default-bucket", true); err != nil {
		t.Fatal(err)
	}
	list, err := reg.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List after EnsureDefault = %+v, %v", list, err)
	}
	if list[0].ID != model.DefaultBucketID || !list[0].IsDefault || !list[0].Enabled ||
		list[0].Priority != DefaultPriority || !list[0].ForcePathStyle {
		t.Fatalf("default bucket = %+v", list[0])
	}

	// EnsureDefault preserves administrator settings on restart.
	if err := reg.Update(ctx, model.DefaultBucketID, UpdateInput{
		MaxBytes: 0, Priority: 42, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.EnsureDefault(ctx, "", "eu-west-1", "new-default", false); err != nil {
		t.Fatal(err)
	}
	defRow, err := st.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	if defRow.Priority != 42 || defRow.Region != "eu-west-1" {
		t.Fatalf("EnsureDefault did not refresh environment fields or preserve priority: %+v", defRow)
	}

	a, err := reg.Add(ctx, AddInput{
		Name: "US", Region: "us-east-1", Bucket: "us-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret", MaxBytes: 100, Priority: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := reg.Add(ctx, AddInput{
		Name: "Germany", Region: "eu-central-1", Bucket: "de-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret", MaxBytes: 100, Priority: 20,
	})
	if err != nil {
		t.Fatal(err)
	}

	selectBucket := func(size int64) (storage.Store, *model.StorageBucket) {
		t.Helper()
		objects, meta, err := reg.SelectForUpload(ctx, size)
		if err != nil {
			t.Fatalf("SelectForUpload(%d) = %v", size, err)
		}
		return objects, meta
	}
	objects, meta := selectBucket(80)
	if meta.ID != a.ID || objects != factory.stores[a.ID] {
		t.Fatalf("first selection = %+v, want bucket %s", meta, a.ID)
	}
	objects, meta = selectBucket(50)
	if meta.ID != b.ID || objects != factory.stores[b.ID] {
		t.Fatalf("spillover selection = %+v, want bucket %s", meta, b.ID)
	}
	objects, meta = selectBucket(20)
	if meta.ID != a.ID || meta.UsedBytes != 100 {
		t.Fatalf("fill selection = %+v, want bucket %s at 100", meta, a.ID)
	}
	objects, meta = selectBucket(50)
	if meta.ID != b.ID || meta.UsedBytes != 100 {
		t.Fatalf("fill spillover = %+v, want bucket %s at 100", meta, b.ID)
	}
	objects, meta = selectBucket(1)
	if meta.ID != model.DefaultBucketID || objects != def {
		t.Fatalf("unlimited default selection = %+v, want %s", meta, model.DefaultBucketID)
	}

	// Disable the unlimited default so a full pool rejects new uploads.
	if err := reg.Update(ctx, model.DefaultBucketID, UpdateInput{
		MaxBytes: 0, Priority: 42, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.SelectForUpload(ctx, 1); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("all buckets full = %v, want ErrNoCapacity", err)
	}

	if err := reg.Release(ctx, a.ID, 20); err != nil {
		t.Fatal(err)
	}
	if _, meta := selectBucket(20); meta.ID != a.ID || meta.UsedBytes != 100 {
		t.Fatalf("selection after release = %+v, want bucket %s", meta, a.ID)
	}
}

func TestDefaultBucketPrecedesNewBucket(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	reg, def, factory := newTestRegistry(t, st, testKey())
	if err := reg.EnsureDefault(ctx, "", "us-east-1", "default-bucket", false); err != nil {
		t.Fatal(err)
	}
	custom, err := reg.Add(ctx, AddInput{
		Name: "New", Region: "us-east-1", Bucket: "new-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret", Priority: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if custom.Priority != 100 {
		t.Fatalf("custom priority = %d, want 100", custom.Priority)
	}
	if err := reg.Update(ctx, model.DefaultBucketID, UpdateInput{MaxBytes: 10, Priority: 0, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	objects, meta, err := reg.SelectForUpload(ctx, 10)
	if err != nil || objects != def || meta.ID != model.DefaultBucketID || meta.Priority != 0 {
		t.Fatalf("first selection = %+v, %v; want default bucket at priority 0", meta, err)
	}
	objects, meta, err = reg.SelectForUpload(ctx, 1)
	if err != nil || objects != factory.stores[custom.ID] || meta.ID != custom.ID || meta.Priority != 100 {
		t.Fatalf("spillover selection = %+v, %v; want custom bucket at priority 100", meta, err)
	}
	t.Log("default priority 0 selected first; full default spills over to custom priority 100")
}

func TestUpdateAndDeleteRules(t *testing.T) {
	st := newTestStore(t)
	reg, _, _ := newTestRegistry(t, st, testKey())
	ctx := context.Background()
	if err := reg.EnsureDefault(ctx, "", "us-east-1", "default-bucket", false); err != nil {
		t.Fatal(err)
	}
	b, err := reg.Add(ctx, AddInput{
		Name: "delete-bucket", Region: "us-east-1", Bucket: "delete-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret", MaxBytes: 1000, Priority: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Update(ctx, b.ID, UpdateInput{
		Name: "renamed", MaxBytes: 1000, Priority: 7, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := st.StorageBucketByID(ctx, b.ID)
	if err != nil || updated.Name != "renamed" || updated.Priority != 7 || updated.Enabled {
		t.Fatalf("updated bucket = %+v, %v", updated, err)
	}

	// The default bucket keeps its reserved name but accepts pool settings.
	if err := reg.Update(ctx, model.DefaultBucketID, UpdateInput{
		Name: "hacked", MaxBytes: 500, Priority: 1000, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	defRow, err := st.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil || defRow.Name != DefaultName || defRow.MaxBytes != 500 {
		t.Fatalf("default update = %+v, %v", defRow, err)
	}
	if err := reg.Update(ctx, "bkt_missing", UpdateInput{Name: "x", Enabled: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing bucket = %v, want ErrNotFound", err)
	}

	// Capacity cannot be lowered below the bytes already reserved.
	if err := reg.Update(ctx, b.ID, UpdateInput{Name: "renamed", MaxBytes: 1000, Priority: 5, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.SelectForUpload(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err := reg.Update(ctx, b.ID, UpdateInput{Name: "renamed", MaxBytes: 10, Priority: 5, Enabled: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("shrink below usage = %v, want ErrInvalid", err)
	}

	img := &model.Image{
		ID: "img_bucket", UserID: "usr_owner", BucketID: b.ID,
		ObjectKey: "i/2026/11/bucket.png", OriginalName: "bucket.png",
		MIME: "image/png", SizeBytes: 100, Width: 1, Height: 1,
		SHA256: "bucket-sha", Visibility: "public", CreatedAt: time.Now().Unix(),
	}
	// The image owner must exist for the foreign key; this test only needs
	// the bucket deletion rules, so insert through raw SQL after creating a
	// minimal user.
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO users(id, email, password_hash, role, status, quota_bytes, used_bytes, created_at, updated_at)
		VALUES('usr_owner', 'owner@example.com', 'hash', 'user', 'active', 1000, 100, unixepoch(), unixepoch())`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := reg.Delete(ctx, b.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete with active image = %v, want ErrInUse", err)
	}
	if _, err := st.SoftDeleteImage(ctx, img.UserID, img.ID); err != nil {
		t.Fatal(err)
	}
	// SoftDeleteImage queues the object cleanup transactionally. A bucket
	// cannot be removed until that retry row is gone, so simulate the
	// successful object delete before retrying the bucket deletion.
	if err := st.RemovePendingDelete(ctx, img.ObjectKey, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := reg.Delete(ctx, b.ID); err != nil {
		t.Fatalf("delete after soft delete: %v", err)
	}
	if _, err := st.StorageBucketByID(ctx, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bucket still exists: %v", err)
	}
	if err := reg.Delete(ctx, model.DefaultBucketID); !errors.Is(err, ErrDefaultBucket) {
		t.Fatalf("delete default = %v, want ErrDefaultBucket", err)
	}
	if err := reg.Delete(ctx, "bkt_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v, want ErrNotFound", err)
	}
}

func TestAddRejectsPrivateEndpoint(t *testing.T) {
	st := newTestStore(t)
	reg, _, _ := newTestRegistry(t, st, testKey())
	for _, endpoint := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "https://10.0.0.1"} {
		_, err := reg.Add(context.Background(), AddInput{
			Name: "private", Endpoint: endpoint, Region: "us-east-1", Bucket: "private-bucket",
			AccessKeyID: "key", SecretAccessKey: "secret",
		})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("endpoint %q = %v, want ErrInvalid", endpoint, err)
		}
	}
}

func TestAddValidation(t *testing.T) {
	st := newTestStore(t)
	reg, _, _ := newTestRegistry(t, st, testKey())
	ctx := context.Background()
	if _, err := reg.Add(ctx, AddInput{
		Name: "existing", Region: "us-east-1", Bucket: "existing-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	tests := map[string]AddInput{
		"empty name":       {Region: "us-east-1", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"},
		"reserved name":    {Name: DefaultName, Region: "us-east-1", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"},
		"missing region":   {Name: "n", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"},
		"invalid bucket":   {Name: "n", Region: "us-east-1", Bucket: "bad bucket!", AccessKeyID: "k", SecretAccessKey: "s"},
		"missing key":      {Name: "n", Region: "us-east-1", Bucket: "b", SecretAccessKey: "s"},
		"missing secret":   {Name: "n", Region: "us-east-1", Bucket: "b", AccessKeyID: "k"},
		"negative size":    {Name: "n", Region: "us-east-1", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", MaxBytes: -1},
		"negative prio":    {Name: "n", Region: "us-east-1", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", Priority: -1},
		"duplicate name":   {Name: "existing", Region: "us-east-1", Bucket: "other", AccessKeyID: "k", SecretAccessKey: "s"},
		"invalid endpoint": {Name: "n", Endpoint: "https://example.com/path", Region: "us-east-1", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"},
	}
	for name, in := range tests {
		if _, err := reg.Add(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Add = %v, want ErrInvalid", name, err)
		}
	}
}

func TestAddRequiresEncryptionKey(t *testing.T) {
	st := newTestStore(t)
	def := storage.NewMemory()
	reg, err := NewRegistry(st, def, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Enabled() {
		t.Fatal("registry without a key should be disabled")
	}
	_, err = reg.Add(context.Background(), AddInput{
		Name: "nokey", Region: "us-east-1", Bucket: "nokey-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret",
	})
	if !errors.Is(err, ErrNoEncryptionKey) {
		t.Fatalf("Add without key = %v, want ErrNoEncryptionKey", err)
	}

	// The environment bucket remains fully usable without a key.
	ctx := context.Background()
	if err := reg.EnsureDefault(ctx, "", "us-east-1", "default-bucket", false); err != nil {
		t.Fatal(err)
	}
	objects, meta, err := reg.SelectForUpload(ctx, 10)
	if err != nil || objects != def || meta.ID != model.DefaultBucketID {
		t.Fatalf("SelectForUpload without key = %v, %+v, %v", objects, meta, err)
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.169.254",
		"100.64.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "192.88.99.1",
		"::1", "::7f00:1", "fc00::1", "fe80::1", "fec0::1",
		"64:ff9b::7f00:1", "2001::1", "2001:db8::1", "2002:7f00:1::",
	} {
		if isPublicIP(net.ParseIP(raw)) {
			t.Fatalf("%s classified as public", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(net.ParseIP(raw)) {
			t.Fatalf("%s classified as private", raw)
		}
	}
}
