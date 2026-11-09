package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/db"
	"fujipic/internal/model"
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
