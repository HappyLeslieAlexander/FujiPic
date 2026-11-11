package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/db"
	"fujipic/internal/model"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database)
}

func newUser(t *testing.T, s *Store, email, role string) *model.User {
	t.Helper()
	now := time.Now().Unix()
	u := &model.User{
		ID:           "usr_" + email,
		Email:        email,
		PasswordHash: "hash",
		Role:         role,
		Status:       "active",
		QuotaBytes:   100,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestNormalizeEmail(t *testing.T) {
	for input, want := range map[string]string{
		"  Alice@Example.COM  ":     "alice@example.com",
		"Alice <Alice@Example.COM>": "alice@example.com",
		"not-an-email":              "not-an-email",
		"   ":                       "",
	} {
		if got := NormalizeEmail(input); got != want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUserCRUDAndCaseInsensitiveEmail(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "Alice@Example.com", "user")
	if _, err := s.UserByEmail(ctx, "alice@example.com"); err != nil {
		t.Fatalf("case-insensitive lookup: %v", err)
	}
	if err := s.CreateUser(ctx, u); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create = %v, want ErrConflict", err)
	}
	if err := s.UpdatePassword(ctx, u.ID, "new-hash"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.PasswordHash != "new-hash" {
		t.Fatalf("password hash = %q", got.PasswordHash)
	}
	if err := s.UpdateUserQuota(ctx, u.ID, 2048); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UserByID(ctx, u.ID)
	if got.QuotaBytes != 2048 {
		t.Fatalf("quota = %d", got.QuotaBytes)
	}
}

func TestUpdatePasswordAndRevokeCredentials(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "cred@example.com", "user")
	now := time.Now().Unix()
	keep := &model.Session{ID: "ses_keep", UserID: u.ID, TokenHash: "keep-hash", CreatedAt: now, LastSeenAt: now, ExpiresAt: now + 3600}
	other := &model.Session{ID: "ses_other", UserID: u.ID, TokenHash: "other-hash", CreatedAt: now, LastSeenAt: now, ExpiresAt: now + 3600}
	if err := s.CreateSession(ctx, keep); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(ctx, &model.APIToken{ID: "tok_1", UserID: u.ID, Name: "test", TokenHash: "token-hash", TokenPrefix: "fp_test", Scopes: "upload,read", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdatePasswordAndRevokeCredentials(ctx, u.ID, "new-hash", keep.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.PasswordHash != "new-hash" {
		t.Fatalf("password hash = %q", got.PasswordHash)
	}
	if _, _, err := s.SessionUser(ctx, "keep-hash"); err != nil {
		t.Fatalf("kept session revoked: %v", err)
	}
	if _, _, err := s.SessionUser(ctx, "other-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other session error = %v, want ErrNotFound", err)
	}
	tokens, err := s.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 0 {
		t.Fatalf("tokens after password change = %d, want 0", len(tokens))
	}
}

func TestTouchMapsStayBounded(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	current := time.Now().Unix()
	s.touchMu.Lock()
	for i := 0; i < touchMapMaxEntries+100; i++ {
		s.lastSessionTouch["ses_"+strconv.Itoa(i)] = current
		s.lastTokenTouch["tok_"+strconv.Itoa(i)] = current
	}
	s.touchMu.Unlock()

	s.TouchSession(ctx, "ses_new", time.Hour)
	s.TouchToken(ctx, "tok_new")

	s.touchMu.Lock()
	defer s.touchMu.Unlock()
	if len(s.lastSessionTouch) > touchMapMaxEntries/2 {
		t.Fatalf("session touch map grew to %d entries", len(s.lastSessionTouch))
	}
	if len(s.lastTokenTouch) > touchMapMaxEntries/2 {
		t.Fatalf("token touch map grew to %d entries", len(s.lastTokenTouch))
	}
}

func TestTouchSessionSlidesExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "slide@example.com", "user")
	now := time.Now().Unix()
	sess := &model.Session{
		ID: "ses_slide", UserID: u.ID, TokenHash: "slide-hash",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now + 60,
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	expiresAt, renewed := s.TouchSession(ctx, sess.ID, 2*time.Hour)
	if !renewed {
		t.Fatal("first touch did not renew the session")
	}
	if delta := expiresAt - now; delta < 7195 || delta > 7205 {
		t.Fatalf("renewed expiry is %ds from now, want about 7200", delta)
	}
	var lastSeen, storedExpiry int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT last_seen_at, expires_at FROM sessions WHERE id = ?`, sess.ID).
		Scan(&lastSeen, &storedExpiry); err != nil {
		t.Fatal(err)
	}
	if storedExpiry != expiresAt {
		t.Fatalf("stored expiry = %d, want %d", storedExpiry, expiresAt)
	}
	if lastSeen < now {
		t.Fatalf("last_seen_at = %d, want at least %d", lastSeen, now)
	}

	// A throttled touch must not advance the throttle timestamp, otherwise
	// steady traffic would postpone the database write forever.
	stamp := time.Now().Unix() - 1
	s.touchMu.Lock()
	s.lastSessionTouch[sess.ID] = stamp
	s.touchMu.Unlock()
	if _, renewed := s.TouchSession(ctx, sess.ID, 2*time.Hour); renewed {
		t.Fatal("second touch was not throttled")
	}
	s.touchMu.Lock()
	gotStamp := s.lastSessionTouch[sess.ID]
	s.touchMu.Unlock()
	if gotStamp != stamp {
		t.Fatalf("throttled touch advanced timestamp to %d, want %d", gotStamp, stamp)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT expires_at FROM sessions WHERE id = ?`, sess.ID).Scan(&storedExpiry); err != nil {
		t.Fatal(err)
	}
	if storedExpiry != expiresAt {
		t.Fatalf("throttled touch changed expiry to %d, want %d", storedExpiry, expiresAt)
	}
}

func TestTouchTokenThrottleDoesNotStarveWrites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "touch-token@example.com", "user")
	now := time.Now().Unix()
	tok := &model.APIToken{
		ID: "tok_touch", UserID: u.ID, Name: "touch", TokenHash: "touch-hash",
		TokenPrefix: "fp_touch", Scopes: "read", CreatedAt: now,
	}
	if err := s.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}

	s.TouchToken(ctx, tok.ID)
	var lastUsed sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT last_used_at FROM api_tokens WHERE id = ?`, tok.ID).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if !lastUsed.Valid || lastUsed.Int64 < now {
		t.Fatalf("last_used_at = %+v, want a fresh timestamp", lastUsed)
	}

	stamp := time.Now().Unix() - 1
	s.touchMu.Lock()
	s.lastTokenTouch[tok.ID] = stamp
	s.touchMu.Unlock()
	s.TouchToken(ctx, tok.ID)
	s.touchMu.Lock()
	gotStamp := s.lastTokenTouch[tok.ID]
	s.touchMu.Unlock()
	if gotStamp != stamp {
		t.Fatalf("throttled touch advanced timestamp to %d, want %d", gotStamp, stamp)
	}
}

func TestQuotaReservation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "q@example.com", "user")
	if err := s.ReserveQuota(ctx, u.ID, 60); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveQuota(ctx, u.ID, 60); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second reserve = %v, want ErrQuotaExceeded", err)
	}
	if err := s.ReleaseQuota(ctx, u.ID, 60); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.UsedBytes != 0 {
		t.Fatalf("used = %d", got.UsedBytes)
	}
	// Releasing more than used never goes negative.
	if err := s.ReleaseQuota(ctx, u.ID, 999); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UserByID(ctx, u.ID)
	if got.UsedBytes != 0 {
		t.Fatalf("used after over-release = %d", got.UsedBytes)
	}
}

func TestImageDedupAndSoftDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "img@example.com", "user")
	if err := s.ReserveQuota(ctx, u.ID, 40); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	img := &model.Image{
		ID: "img_1", UserID: u.ID, ObjectKey: "i/2026/11/key.png",
		OriginalName: "key.png", MIME: "image/png", SizeBytes: 40,
		Width: 2, Height: 2, SHA256: "abc", Visibility: "public", CreatedAt: now,
	}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	dup := *img
	dup.ID = "img_2"
	dup.ObjectKey = "i/2026/11/key2.png"
	if err := s.InsertImage(ctx, &dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate sha = %v, want ErrConflict", err)
	}
	if found, err := s.ImageBySHA(ctx, u.ID, "abc"); err != nil || found.ID != img.ID {
		t.Fatalf("ImageBySHA = %+v, %v", found, err)
	}
	if found, err := s.ImageByObjectKey(ctx, img.ObjectKey); err != nil || found.ID != img.ID {
		t.Fatalf("ImageByObjectKey = %+v, %v", found, err)
	}

	deleted, err := s.SoftDeleteImage(ctx, u.ID, img.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.DeletedAt == nil {
		t.Fatal("deleted_at not set")
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.UsedBytes != 0 {
		t.Fatalf("used after delete = %d", got.UsedBytes)
	}
	if _, err := s.ImageByObjectKey(ctx, img.ObjectKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted image lookup = %v", err)
	}
	// A new upload of the same bytes is allowed after the old one is deleted.
	img2 := *img
	img2.ID = "img_3"
	img2.ObjectKey = "i/2026/11/key3.png"
	if err := s.InsertImage(ctx, &img2); err != nil {
		t.Fatalf("insert after delete: %v", err)
	}
}

func TestSoftDeleteImageQueuesObjectCleanup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "soft-delete-queue@example.com", "user")
	if err := s.ReserveQuota(ctx, u.ID, 12); err != nil {
		t.Fatal(err)
	}
	img := &model.Image{
		ID: "img_queue", UserID: u.ID, ObjectKey: "i/2026/11/queue.png",
		OriginalName: "queue.png", MIME: "image/png", SizeBytes: 12,
		Width: 1, Height: 1, SHA256: "queue-sha", Visibility: "public", CreatedAt: time.Now().Unix(),
	}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SoftDeleteImage(ctx, u.ID, img.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingDeletes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ObjectKey != img.ObjectKey || pending[0].BucketID != model.DefaultBucketID {
		t.Fatalf("pending deletes = %+v, want one entry for %s in %s", pending, img.ObjectKey, model.DefaultBucketID)
	}
}

func TestImageQueriesTolerateNullOriginalName(t *testing.T) {
	// Simulate a pre-fix database: Store's read-side COALESCE protects it
	// even though the current migration forbids new NULL values.
	database, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "legacy.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	database.SetMaxOpenConns(1)
	if _, err := database.ExecContext(context.Background(), `
CREATE TABLE users (
  id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
  role TEXT NOT NULL, status TEXT NOT NULL, quota_bytes INTEGER NOT NULL,
  used_bytes INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE images (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, object_key TEXT NOT NULL UNIQUE,
  bucket_id TEXT, original_name TEXT, mime TEXT NOT NULL, size_bytes INTEGER NOT NULL,
  width INTEGER, height INTEGER, sha256 TEXT NOT NULL, visibility TEXT NOT NULL,
  created_at INTEGER NOT NULL, deleted_at INTEGER
);
CREATE TABLE storage_buckets (
  id TEXT PRIMARY KEY, used_bytes INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE pending_deletes (
  object_key TEXT NOT NULL, bucket_id TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT, PRIMARY KEY (object_key, bucket_id)
);
INSERT INTO storage_buckets(id, used_bytes, updated_at) VALUES('bkt_default', 12, 0);
`); err != nil {
		t.Fatal(err)
	}
	s := New(database)
	ctx := context.Background()
	u := newUser(t, s, "null-name@example.com", "user")
	if _, err := database.ExecContext(ctx, `UPDATE users SET used_bytes = 12 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	img := &model.Image{
		ID: "img_null", UserID: u.ID, ObjectKey: "i/2026/11/null.png",
		OriginalName: "", MIME: "image/png", SizeBytes: 12,
		Width: 1, Height: 1, SHA256: "null-sha", Visibility: "public", CreatedAt: time.Now().Unix(),
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO images(id, user_id, object_key, original_name, mime, size_bytes, width, height, sha256, visibility, created_at)
		VALUES(?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?)`, img.ID, img.UserID, img.ObjectKey, img.MIME, img.SizeBytes, img.Width, img.Height, img.SHA256, img.Visibility, img.CreatedAt); err != nil {
		t.Fatal(err)
	}

	got, err := s.ImageBySHA(ctx, u.ID, img.SHA256)
	if err != nil {
		t.Fatalf("ImageBySHA with NULL original_name: %v", err)
	}
	if got.OriginalName != "" {
		t.Fatalf("NULL original_name coalesced to %q", got.OriginalName)
	}
	if _, err := s.ImageByID(ctx, u.ID, img.ID); err != nil {
		t.Fatalf("ImageByID with NULL original_name: %v", err)
	}
	listed, err := s.ListImages(ctx, u.ID, 10, 0, "")
	if err != nil || len(listed) != 1 || listed[0].OriginalName != "" {
		t.Fatalf("ListImages with NULL original_name = %+v, %v", listed, err)
	}
	if _, err := s.SoftDeleteImage(ctx, u.ID, img.ID); err != nil {
		t.Fatalf("SoftDeleteImage with NULL original_name: %v", err)
	}
}

func TestPhysicalDeleteEnqueuesObjectCleanup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "physical-delete@example.com", "user")
	img := &model.Image{
		ID: "img_physical", UserID: u.ID, ObjectKey: "i/2026/11/physical.png",
		OriginalName: "physical.png", MIME: "image/png", SizeBytes: 12,
		Width: 1, Height: 1, SHA256: "physical-sha", Visibility: "public", CreatedAt: time.Now().Unix(),
	}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM images WHERE id = ?`, img.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingDeletes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ObjectKey != img.ObjectKey {
		t.Fatalf("pending deletes = %v, want [%s]", pending, img.ObjectKey)
	}
}

func TestStorageBucketPoolCRUDAndDeleteRules(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "bucket-owner@example.com", "user")
	now := time.Now().Unix()

	if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{
		Name: "服务器默认存储", Endpoint: "https://s3.example.com",
		Region: "us-east-1", Bucket: "default-bucket",
		ForcePathStyle: true, Priority: 0,
	}); err != nil {
		t.Fatal(err)
	}
	def, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	if !def.IsDefault || !def.Enabled || def.MaxBytes != 0 || def.Priority != 0 {
		t.Fatalf("default bucket = %+v", def)
	}
	if !def.ForcePathStyle || def.Endpoint != "https://s3.example.com" {
		t.Fatalf("default environment settings lost: %+v", def)
	}

	// EnsureDefault refreshes environment fields but preserves capacity,
	// priority, and enabled state set by an administrator.
	def.MaxBytes = 1000
	def.Priority = 42
	def.Enabled = false
	if err := s.UpdateStorageBucket(ctx, def); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{
		Name: "服务器默认存储", Region: "eu-west-1", Bucket: "new-default", Priority: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	def, err = s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	if def.MaxBytes != 1000 || def.Priority != 42 || def.Enabled || def.Region != "eu-west-1" {
		t.Fatalf("EnsureDefault overwrote admin settings: %+v", def)
	}

	b := &model.StorageBucket{
		ID: "bkt_owner", Name: "OCI Tokyo",
		Endpoint: "https://objectstorage.example.com", Region: "ap-tokyo-1",
		Bucket: "owner-bucket", AccessKeyID: "access-key",
		SecretCiphertext: "ciphertext", ForcePathStyle: true,
		MaxBytes: 100, Priority: 10, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateStorageBucket(ctx, b); err != nil {
		t.Fatal(err)
	}
	dup := *b
	dup.ID, dup.Name = "bkt_dup", "OCI Tokyo"
	if err := s.CreateStorageBucket(ctx, &dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name = %v, want ErrConflict", err)
	}
	list, err := s.ListStorageBuckets(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListStorageBuckets = %+v, %v", list, err)
	}
	if list[0].ID != b.ID || list[1].ID != model.DefaultBucketID {
		t.Fatalf("bucket order = %q, %q; want priority order", list[0].ID, list[1].ID)
	}
	if list[0].SecretCiphertext != "" || !list[0].ForcePathStyle {
		t.Fatalf("list result leaked secret or lost path style: %+v", list[0])
	}

	b.Name, b.MaxBytes, b.Priority = "OCI Tokyo 2", 200, 5
	if err := s.UpdateStorageBucket(ctx, b); err != nil {
		t.Fatal(err)
	}
	updated, err := s.StorageBucketByID(ctx, b.ID)
	if err != nil || updated.Name != b.Name || updated.MaxBytes != 200 || updated.Priority != 5 {
		t.Fatalf("updated bucket = %+v, %v", updated, err)
	}
	missing := *b
	missing.ID = "bkt_missing"
	if err := s.UpdateStorageBucket(ctx, &missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing bucket = %v, want ErrNotFound", err)
	}
	if err := s.DeleteStorageBucket(ctx, model.DefaultBucketID); !errors.Is(err, ErrDefaultBucket) {
		t.Fatalf("delete default bucket = %v, want ErrDefaultBucket", err)
	}

	reserved, err := s.ReserveStorageBucket(ctx, 40)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.ID != b.ID || reserved.UsedBytes != 40 {
		t.Fatalf("reserved bucket = %+v, want %s with 40 bytes", reserved, b.ID)
	}
	img := &model.Image{
		ID: "img_bucket_rule", UserID: u.ID, BucketID: b.ID,
		ObjectKey: "i/2026/11/rule.png", OriginalName: "rule.png",
		MIME: "image/png", SizeBytes: 40, Width: 1, Height: 1,
		SHA256: "bucket-rule-sha", Visibility: "public", CreatedAt: now,
	}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStorageBucket(ctx, b.ID); !errors.Is(err, ErrBucketInUse) {
		t.Fatalf("delete with active image = %v, want ErrBucketInUse", err)
	}
	if _, err := s.SoftDeleteImage(ctx, u.ID, img.ID); err != nil {
		t.Fatal(err)
	}
	released, err := s.StorageBucketByID(ctx, b.ID)
	if err != nil || released.UsedBytes != 0 {
		t.Fatalf("bucket usage after soft delete = %+v, %v", released, err)
	}
	if err := s.AddPendingDelete(ctx, img.ObjectKey, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStorageBucket(ctx, b.ID); !errors.Is(err, ErrBucketInUse) {
		t.Fatalf("delete with pending object = %v, want ErrBucketInUse", err)
	}
	if err := s.RemovePendingDelete(ctx, img.ObjectKey, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStorageBucket(ctx, b.ID); err != nil {
		t.Fatalf("delete empty bucket: %v", err)
	}
	if _, err := s.StorageBucketByID(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bucket still exists: %v", err)
	}
	var bucketID sql.NullString
	if err := s.DB().QueryRowContext(ctx,
		`SELECT bucket_id FROM images WHERE id = ?`, img.ID).Scan(&bucketID); err != nil {
		t.Fatal(err)
	}
	if bucketID.Valid {
		t.Fatalf("soft-deleted image kept bucket_id %q after bucket deletion", bucketID.String)
	}
}

func TestStorageBucketCapacityReservationAndSpillover(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Explicitly move the default bucket behind the custom buckets for this
	// spillover scenario; new installations now prefer the default at 0.
	defRow, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	defRow.Priority = 1000
	if err := s.UpdateStorageBucket(ctx, defRow); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{
		Name: "服务器默认存储", Region: "us-east-1", Bucket: "default-bucket", Priority: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	a := &model.StorageBucket{
		ID: "bkt_a", Name: "US", Region: "us-east-1", Bucket: "a",
		AccessKeyID: "key", SecretCiphertext: "cipher", MaxBytes: 100,
		Priority: 10, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	b := &model.StorageBucket{
		ID: "bkt_b", Name: "Germany", Region: "eu-central-1", Bucket: "b",
		AccessKeyID: "key", SecretCiphertext: "cipher", MaxBytes: 100,
		Priority: 20, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	for _, bucket := range []*model.StorageBucket{a, b} {
		if err := s.CreateStorageBucket(ctx, bucket); err != nil {
			t.Fatal(err)
		}
	}

	reserve := func(size int64) *model.StorageBucket {
		t.Helper()
		bucket, err := s.ReserveStorageBucket(ctx, size)
		if err != nil {
			t.Fatalf("reserve %d bytes: %v", size, err)
		}
		return bucket
	}
	if got := reserve(80); got.ID != a.ID || got.UsedBytes != 80 {
		t.Fatalf("first reservation = %+v, want %s at 80", got, a.ID)
	}
	if got := reserve(50); got.ID != b.ID || got.UsedBytes != 50 {
		t.Fatalf("spillover reservation = %+v, want %s at 50", got, b.ID)
	}
	if got := reserve(20); got.ID != a.ID || got.UsedBytes != 100 {
		t.Fatalf("fill reservation = %+v, want %s at 100", got, a.ID)
	}
	if got := reserve(50); got.ID != b.ID || got.UsedBytes != 100 {
		t.Fatalf("fill spillover = %+v, want %s at 100", got, b.ID)
	}
	if got := reserve(1); got.ID != model.DefaultBucketID {
		t.Fatalf("unlimited default reservation = %+v, want %s", got, model.DefaultBucketID)
	}

	def, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	def.Enabled = false
	if err := s.UpdateStorageBucket(ctx, def); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveStorageBucket(ctx, 1); !errors.Is(err, ErrBucketCapacity) {
		t.Fatalf("all buckets full = %v, want ErrBucketCapacity", err)
	}

	if err := s.ReleaseStorageBucket(ctx, a.ID, 20); err != nil {
		t.Fatal(err)
	}
	if got := reserve(20); got.ID != a.ID || got.UsedBytes != 100 {
		t.Fatalf("reservation after release = %+v, want %s", got, a.ID)
	}
	if err := s.ReleaseStorageBucket(ctx, b.ID, 50); err != nil {
		t.Fatal(err)
	}

	c := &model.StorageBucket{
		ID: "bkt_c", Name: "Tokyo", Region: "ap-tokyo-1", Bucket: "c",
		AccessKeyID: "key", SecretCiphertext: "cipher", MaxBytes: 0,
		Priority: 30, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateStorageBucket(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got := reserve(30); got.ID != b.ID || got.UsedBytes != 80 {
		t.Fatalf("second spillover = %+v, want %s at 80", got, b.ID)
	}
	if got := reserve(30); got.ID != c.ID || got.UsedBytes != 30 {
		t.Fatalf("unlimited bucket reservation = %+v, want %s at 30", got, c.ID)
	}
}

func TestDeleteStorageBucketRejectsInflightReservation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	def, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	def.Enabled = false
	if err := s.UpdateStorageBucket(ctx, def); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	b := &model.StorageBucket{
		ID: "bkt_inflight", Name: "Inflight", Region: "us-east-1", Bucket: "inflight",
		AccessKeyID: "key", SecretCiphertext: "cipher", MaxBytes: 100,
		Priority: 10, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateStorageBucket(ctx, b); err != nil {
		t.Fatal(err)
	}
	reserved, err := s.ReserveStorageBucket(ctx, 40)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.ID != b.ID {
		t.Fatalf("reserved bucket = %s, want %s", reserved.ID, b.ID)
	}
	if err := s.DeleteStorageBucket(ctx, b.ID); !errors.Is(err, ErrBucketInUse) {
		t.Fatalf("delete with in-flight reservation = %v, want ErrBucketInUse", err)
	}
	if err := s.ReleaseStorageBucket(ctx, b.ID, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStorageBucket(ctx, b.ID); err != nil {
		t.Fatalf("delete after release: %v", err)
	}
}

func TestReconcileStorageBucketUsage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "reconcile@example.com", "user")
	if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{
		Name: "服务器默认存储", Region: "us-east-1", Bucket: "default-bucket", Priority: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	custom := &model.StorageBucket{
		ID: "bkt_reconcile", Name: "Tokyo", Region: "ap-tokyo-1", Bucket: "tokyo",
		AccessKeyID: "key", SecretCiphertext: "cipher", MaxBytes: 1000,
		Priority: 10, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateStorageBucket(ctx, custom); err != nil {
		t.Fatal(err)
	}
	images := []*model.Image{
		{ID: "img_default", UserID: u.ID, BucketID: model.DefaultBucketID, ObjectKey: "i/2026/11/d.png", OriginalName: "d.png", MIME: "image/png", SizeBytes: 10, SHA256: "sha-d", Visibility: "public", CreatedAt: now},
		{ID: "img_custom", UserID: u.ID, BucketID: custom.ID, ObjectKey: "i/2026/11/c.png", OriginalName: "c.png", MIME: "image/png", SizeBytes: 20, SHA256: "sha-c", Visibility: "public", CreatedAt: now},
		{ID: "img_deleted", UserID: u.ID, BucketID: custom.ID, ObjectKey: "i/2026/11/x.png", OriginalName: "x.png", MIME: "image/png", SizeBytes: 100, SHA256: "sha-x", Visibility: "public", CreatedAt: now},
	}
	for _, img := range images {
		if err := s.InsertImage(ctx, img); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE images SET deleted_at = unixepoch() WHERE id = 'img_deleted'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE storage_buckets SET used_bytes = 9999`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileStorageBucketUsage(ctx); err != nil {
		t.Fatal(err)
	}
	def, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	if def.UsedBytes != 10 {
		t.Fatalf("default used_bytes = %d, want 10", def.UsedBytes)
	}
	reconciled, err := s.StorageBucketByID(ctx, custom.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.UsedBytes != 20 {
		t.Fatalf("custom used_bytes = %d, want 20", reconciled.UsedBytes)
	}
}

func TestReconcileUserUsage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	leaked := newUser(t, s, "leaked@example.com", "user")
	clean := newUser(t, s, "clean@example.com", "user")
	now := time.Now().Unix()
	images := []*model.Image{
		{ID: "img_active", UserID: leaked.ID, ObjectKey: "i/2026/11/a.png", OriginalName: "a.png", MIME: "image/png", SizeBytes: 10, SHA256: "sha-a", Visibility: "public", CreatedAt: now},
		{ID: "img_deleted", UserID: leaked.ID, ObjectKey: "i/2026/11/d.png", OriginalName: "d.png", MIME: "image/png", SizeBytes: 100, SHA256: "sha-d", Visibility: "public", CreatedAt: now},
	}
	for _, img := range images {
		if err := s.InsertImage(ctx, img); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE images SET deleted_at = unixepoch() WHERE id = 'img_deleted'`); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after reservation but before InsertImage: used_bytes
	// is higher than the sum of active images.
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE users SET used_bytes = 9999 WHERE id IN (?, ?)`, leaked.ID, clean.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileUserUsage(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.UserByID(ctx, leaked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedBytes != 10 {
		t.Fatalf("leaked user used_bytes = %d, want 10", got.UsedBytes)
	}
	got, err = s.UserByID(ctx, clean.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedBytes != 0 {
		t.Fatalf("clean user used_bytes = %d, want 0", got.UsedBytes)
	}
}

func TestPhysicalDeletePreservesBucketID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "bucket-trigger@example.com", "user")
	now := time.Now().Unix()
	if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{
		Name: "服务器默认存储", Region: "us-east-1", Bucket: "default-bucket", Priority: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	b := &model.StorageBucket{
		ID: "bkt_trigger", Name: "trigger", Region: "us-east-1",
		Bucket: "trigger-bucket", AccessKeyID: "key", SecretCiphertext: "cipher",
		Priority: 10, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateStorageBucket(ctx, b); err != nil {
		t.Fatal(err)
	}
	img := &model.Image{
		ID: "img_trigger", UserID: u.ID, BucketID: b.ID,
		ObjectKey: "i/2026/11/trigger.png", OriginalName: "trigger.png",
		MIME: "image/png", SizeBytes: 10, Width: 1, Height: 1,
		SHA256: "trigger-sha", Visibility: "public", CreatedAt: now,
	}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM images WHERE id = ?`, img.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingDeletes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].BucketID != b.ID {
		t.Fatalf("pending deletes = %+v, want bucket %s", pending, b.ID)
	}
}

func TestInvites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := newUser(t, s, "a@example.com", "user")
	b := newUser(t, s, "b@example.com", "user")
	d := newUser(t, s, "d@example.com", "user")
	inv := &model.Invite{Code: "inv1", CreatedAt: time.Now().Unix()}
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeInvite(ctx, "inv1", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeInvite(ctx, "inv1", b.ID); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("reuse = %v", err)
	}
	if err := s.ConsumeInvite(ctx, "missing", b.ID); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("missing = %v", err)
	}
	expired := time.Now().Add(-time.Hour).Unix()
	inv2 := &model.Invite{Code: "inv2", CreatedAt: time.Now().Unix(), ExpiresAt: &expired}
	if err := s.CreateInvite(ctx, inv2); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeInvite(ctx, "inv2", d.ID); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("expired = %v", err)
	}
}

// TestTokenExpiry pins the expiry enforcement in TokenUser (surfaced as
// 401 by the HTTP layer): expired tokens never authenticate.
func TestTokenExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "tok@example.com", "user")

	past := time.Now().Add(-time.Hour).Unix()
	future := time.Now().Add(time.Hour).Unix()
	make := func(exp *int64) string {
		plain, hash, _ := auth.NewToken()
		if err := s.CreateToken(ctx, &model.APIToken{
			ID: "tok_" + plain[4:12], UserID: u.ID, Name: "test",
			TokenHash: hash, TokenPrefix: plain[:10],
			Scopes: "upload,read,delete", CreatedAt: time.Now().Unix(), ExpiresAt: exp,
		}); err != nil {
			t.Fatal(err)
		}
		return hash
	}

	if _, _, err := s.TokenUser(ctx, make(&past)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired token = %v, want ErrNotFound", err)
	}
	got, tok, err := s.TokenUser(ctx, make(&future))
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if got.ID != u.ID || tok.Scopes != "upload,read,delete" {
		t.Fatalf("valid token identity = %+v / %+v", got, tok)
	}
	if _, _, err := s.TokenUser(ctx, make(nil)); err != nil {
		t.Fatalf("permanent token: %v", err)
	}
}

func TestCreateUserWithInvite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inv := &model.Invite{Code: "invite", CreatedAt: time.Now().Unix()}
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	u := &model.User{ID: "usr_inv", Email: "inv@example.com", PasswordHash: "h", Role: "user",
		Status: "active", QuotaBytes: 100, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserWithInvite(ctx, u, "invite"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeInvite(ctx, "invite", "other"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("invite should be consumed: %v", err)
	}
	u2 := *u
	u2.ID = "usr_inv2"
	u2.Email = "inv2@example.com"
	if err := s.CreateUserWithInvite(ctx, &u2, "invite"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("reused invite = %v", err)
	}
	// The failed transaction must not leave a user behind.
	if _, err := s.UserByEmail(ctx, "inv2@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan user: %v", err)
	}
}

func TestLastAdminProtection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := newUser(t, s, "admin1@example.com", "admin")
	b := newUser(t, s, "admin2@example.com", "admin")
	if err := s.UpdateUserStatus(ctx, a.ID, "disabled"); err != nil {
		t.Fatalf("disable one of two admins: %v", err)
	}
	if err := s.UpdateUserStatus(ctx, b.ID, "disabled"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disable last admin = %v, want ErrLastAdmin", err)
	}
	if err := s.UpdateUserStatus(ctx, b.ID, "active"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateTokenEnforcesActiveLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "token-cap@example.com", "user")
	now := time.Now().Unix()
	for i := 0; i < MaxActiveTokensPerUser; i++ {
		tok := &model.APIToken{
			ID: fmt.Sprintf("tok_cap_%03d", i), UserID: u.ID, Name: "cap",
			TokenHash: fmt.Sprintf("hash-%03d", i), TokenPrefix: "fp_cap",
			Scopes: "upload", CreatedAt: now + int64(i),
		}
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Fatalf("create token %d: %v", i, err)
		}
	}
	if err := s.CreateToken(ctx, &model.APIToken{
		ID: "tok_cap_over", UserID: u.ID, Name: "over",
		TokenHash: "hash-over", TokenPrefix: "fp_over", Scopes: "upload", CreatedAt: now + MaxActiveTokensPerUser,
	}); !errors.Is(err, ErrTokenLimitExceeded) {
		t.Fatalf("51st active token = %v, want ErrTokenLimitExceeded", err)
	}
}

func TestCDNPurgeQueue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.AddCDNPurge(ctx, "https://img.example.com/i/a.png"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCDNPurge(ctx, "https://img.example.com/i/a.png"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingCDNPurges(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %d, %v", len(pending), err)
	}
	if err := s.MarkCDNPurgeFailure(ctx, pending[0].URL, "boom"); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingCDNPurges(ctx, 10)
	if pending[0].Attempts != 1 || pending[0].LastError != "boom" {
		t.Fatalf("after failure = %+v", pending[0])
	}
	if err := s.DeleteCDNPurge(ctx, pending[0].URL); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingCDNPurges(ctx, 10)
	if len(pending) != 0 {
		t.Fatalf("pending after delete = %d", len(pending))
	}
}

func TestPendingCDNPurgesSkipsDeadLetters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		url := fmt.Sprintf("https://img.example.com/i/dead-%02d.png", i)
		if err := s.AddCDNPurge(ctx, url); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 10; attempt++ {
			if err := s.MarkCDNPurgeFailure(ctx, url, "boom"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.AddCDNPurge(ctx, "https://img.example.com/i/new.png"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingCDNPurges(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].URL != "https://img.example.com/i/new.png" {
		t.Fatalf("pending after dead letters = %+v, want only the fresh purge", pending)
	}
}

func TestValidateInvite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ValidateInvite(ctx, "missing"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("missing invite = %v, want ErrInviteInvalid", err)
	}
	inv := &model.Invite{Code: "precheck-code", CreatedAt: time.Now().Unix()}
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateInvite(ctx, "precheck-code"); err != nil {
		t.Fatalf("valid invite rejected: %v", err)
	}
	expired := time.Now().Add(-time.Hour).Unix()
	if err := s.CreateInvite(ctx, &model.Invite{Code: "expired-code", CreatedAt: time.Now().Unix(), ExpiresAt: &expired}); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateInvite(ctx, "expired-code"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("expired invite = %v, want ErrInviteInvalid", err)
	}
}

func TestPendingDeletesSkipsPoisonRecords(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("i/2026/11/poison-%02d.png", i)
		if err := s.AddPendingDelete(ctx, key, ""); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 10; attempt++ {
			if err := s.MarkPendingDeleteFailure(ctx, key, "", "boom"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.AddPendingDelete(ctx, "i/2026/11/fresh.png", ""); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingDeletes(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ObjectKey != "i/2026/11/fresh.png" {
		t.Fatalf("pending deletes = %v, want only the fresh key", pending)
	}
}

func TestListImagesCursor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "list@example.com", "user")
	base := time.Now().Unix()
	for i := 0; i < 5; i++ {
		img := &model.Image{
			ID: "img_" + string(rune('a'+i)), UserID: u.ID,
			ObjectKey: "i/2026/11/k" + string(rune('a'+i)) + ".png",
			MIME:      "image/png", SizeBytes: 1, SHA256: "sha" + string(rune('a'+i)),
			Visibility: "public", CreatedAt: base + int64(i),
		}
		if err := s.InsertImage(ctx, img); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListImages(ctx, u.ID, 2, 0, "")
	if err != nil || len(first) != 2 {
		t.Fatalf("first page = %d, %v", len(first), err)
	}
	last := first[len(first)-1]
	second, err := s.ListImages(ctx, u.ID, 2, last.CreatedAt, last.ID)
	if err != nil || len(second) != 2 {
		t.Fatalf("second page = %d, %v", len(second), err)
	}
	if second[0].ID == first[0].ID || second[0].ID == first[1].ID {
		t.Fatal("pages overlap")
	}
}

// TestStatsTodayUploadsUsesLocalMidnight pins that the admin "today" counter
// follows the same timezone as the timestamps rendered in the web UI.
func TestStatsTodayUploadsUsesLocalMidnight(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "stats@example.com", "user")

	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	for i, created := range []int64{start - 1, start} {
		img := &model.Image{
			ID: fmt.Sprintf("img_stats_%d", i), UserID: u.ID,
			ObjectKey: fmt.Sprintf("i/2026/11/stats%d.png", i),
			MIME:      "image/png", SizeBytes: 1, SHA256: fmt.Sprintf("stats-%d", i),
			Visibility: "public", CreatedAt: created,
		}
		if err := s.InsertImage(ctx, img); err != nil {
			t.Fatal(err)
		}
	}

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.TodayUploads != 1 {
		t.Fatalf("TodayUploads = %d, want 1 (one image before local midnight)", st.TodayUploads)
	}
}

// TestQuotaReservationNearMaxInt64 pins the overflow-safe comparison in
// ReserveQuota: used_bytes + size would wrap and let a second reservation
// through when the quota itself is close to math.MaxInt64.
func TestQuotaReservationNearMaxInt64(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "quota-overflow@example.com", "user")
	if err := s.UpdateUserQuota(ctx, u.ID, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveQuota(ctx, u.ID, math.MaxInt64); err != nil {
		t.Fatalf("reserve full quota: %v", err)
	}
	if err := s.ReserveQuota(ctx, u.ID, 1); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("reserve past MaxInt64 quota = %v, want ErrQuotaExceeded", err)
	}
	got, err := s.UserByID(ctx, u.ID)
	if err != nil || got.UsedBytes != math.MaxInt64 {
		t.Fatalf("used_bytes = %d, %v; want MaxInt64", got.UsedBytes, err)
	}
}

// TestStorageBucketCapacityNearMaxInt64 is the bucket-capacity counterpart of
// TestQuotaReservationNearMaxInt64.
func TestStorageBucketCapacityNearMaxInt64(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	def, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	def.Enabled = false
	if err := s.UpdateStorageBucket(ctx, def); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	b := &model.StorageBucket{
		ID: "bkt_max", Name: "Max", Region: "us-east-1", Bucket: "max",
		AccessKeyID: "key", SecretCiphertext: "cipher", MaxBytes: math.MaxInt64,
		Priority: 10, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateStorageBucket(ctx, b); err != nil {
		t.Fatal(err)
	}
	reserved, err := s.ReserveStorageBucket(ctx, math.MaxInt64)
	if err != nil || reserved.ID != b.ID {
		t.Fatalf("reserve full bucket = %+v, %v", reserved, err)
	}
	if _, err := s.ReserveStorageBucket(ctx, 1); !errors.Is(err, ErrBucketCapacity) {
		t.Fatalf("reserve past MaxInt64 capacity = %v, want ErrBucketCapacity", err)
	}
}
