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

func newTestUser(t *testing.T, st *store.Store, email string) *model.User {
	t.Helper()
	now := time.Now().Unix()
	u := &model.User{
		ID: "usr_" + strings.ReplaceAll(email, "@", "_"), Email: email,
		PasswordHash: "placeholder", Role: "user", Status: "active",
		QuotaBytes: 1 << 20, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func memoryFactory(seen *string) Factory {
	return func(ctx context.Context, b model.StorageBucket, secret string) (storage.Store, error) {
		if seen != nil {
			*seen = secret
		}
		return storage.NewMemory(), nil
	}
}

func TestAddResolveAndEncryptAtRest(t *testing.T) {
	st := newTestStore(t)
	user := newTestUser(t, st, "bucket@example.com")
	var seenSecret string
	reg, err := NewRegistry(st, storage.NewMemory(), testKey(), memoryFactory(&seenSecret), false)
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Enabled() {
		t.Fatal("registry with a key should be enabled")
	}
	ctx := context.Background()
	b, err := reg.Add(ctx, user.ID, AddInput{
		Name: "OCI Tokyo", Region: "ap-tokyo-1", Bucket: "my-private-bucket",
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "super-secret",
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenSecret != "super-secret" {
		t.Fatalf("factory secret = %q", seenSecret)
	}
	row, err := st.StorageBucketByID(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.SecretCiphertext == "" || strings.Contains(row.SecretCiphertext, "super-secret") {
		t.Fatalf("secret was not encrypted at rest: %q", row.SecretCiphertext)
	}
	if !row.ForcePathStyle {
		t.Fatal("force path style was not persisted")
	}

	// A fresh registry must decrypt the stored secret to build the client.
	seenSecret = ""
	fresh, err := NewRegistry(st, storage.NewMemory(), testKey(), memoryFactory(&seenSecret), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Resolve(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if seenSecret != "super-secret" {
		t.Fatalf("decrypted secret = %q", seenSecret)
	}

	// List results must not expose ciphertext.
	list, err := fresh.List(ctx, user.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if list[0].SecretCiphertext != "" {
		t.Fatal("List exposed secret ciphertext")
	}
}

func TestResolveForUploadEnforcesOwnership(t *testing.T) {
	st := newTestStore(t)
	owner := newTestUser(t, st, "owner@example.com")
	other := newTestUser(t, st, "other@example.com")
	reg, err := NewRegistry(st, storage.NewMemory(), testKey(), memoryFactory(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b, err := reg.Add(ctx, owner.ID, AddInput{
		Name: "owner-bucket", Region: "us-east-1", Bucket: "owner-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.ResolveForUpload(ctx, owner.ID, b.ID); err != nil {
		t.Fatalf("owner resolve: %v", err)
	}
	if _, _, err := reg.ResolveForUpload(ctx, other.ID, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user resolve = %v, want ErrNotFound", err)
	}
}

func TestDeleteRules(t *testing.T) {
	st := newTestStore(t)
	user := newTestUser(t, st, "delete@example.com")
	reg, err := NewRegistry(st, storage.NewMemory(), testKey(), memoryFactory(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b, err := reg.Add(ctx, user.ID, AddInput{
		Name: "delete-bucket", Region: "us-east-1", Bucket: "delete-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	img := &model.Image{
		ID: "img_bucket", UserID: user.ID, BucketID: b.ID,
		ObjectKey: "i/2026/11/bucket.png", OriginalName: "bucket.png",
		MIME: "image/png", SizeBytes: 10, Width: 1, Height: 1,
		SHA256: "bucket-sha", Visibility: "public", CreatedAt: time.Now().Unix(),
	}
	if err := st.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := reg.Delete(ctx, user.ID, b.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete with active image = %v, want ErrInUse", err)
	}
	if _, err := st.SoftDeleteImage(ctx, user.ID, img.ID); err != nil {
		t.Fatal(err)
	}
	if err := reg.Delete(ctx, user.ID, b.ID); err != nil {
		t.Fatalf("delete after soft delete: %v", err)
	}
	if _, err := st.StorageBucketByID(ctx, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bucket still exists: %v", err)
	}
}

func TestAddRejectsPrivateEndpoint(t *testing.T) {
	st := newTestStore(t)
	user := newTestUser(t, st, "ssrf@example.com")
	reg, err := NewRegistry(st, storage.NewMemory(), testKey(), memoryFactory(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "https://10.0.0.1"} {
		_, err := reg.Add(context.Background(), user.ID, AddInput{
			Name: "private", Endpoint: endpoint, Region: "us-east-1", Bucket: "private-bucket",
			AccessKeyID: "key", SecretAccessKey: "secret",
		})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("endpoint %q = %v, want ErrInvalid", endpoint, err)
		}
	}
}

func TestAddRequiresEncryptionKey(t *testing.T) {
	st := newTestStore(t)
	user := newTestUser(t, st, "nokey@example.com")
	reg, err := NewRegistry(st, storage.NewMemory(), "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Enabled() {
		t.Fatal("registry without a key should be disabled")
	}
	_, err = reg.Add(context.Background(), user.ID, AddInput{
		Name: "nokey", Region: "us-east-1", Bucket: "nokey-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret",
	})
	if !errors.Is(err, ErrNoEncryptionKey) {
		t.Fatalf("Add without key = %v, want ErrNoEncryptionKey", err)
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1", "fe80::1"} {
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
