package store

import (
	"context"
	"errors"
	"fmt"
	"fujipic/internal/model"
	"testing"
)

func TestNullImageDimensionsAcrossReadAndDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "null@example.test", "admin")
	img := &model.Image{ID: "null", UserID: u.ID, ObjectKey: "i/null.png", MIME: "image/png", SHA256: "null", Visibility: "public"}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE images SET width=NULL,height=NULL`); err != nil {
		t.Fatal(err)
	}
	for _, get := range []func() (*model.Image, error){func() (*model.Image, error) { return s.ImageByID(ctx, u.ID, img.ID) }, func() (*model.Image, error) { return s.AdminImageByID(ctx, img.ID) }, func() (*model.Image, error) { return s.ImageByObjectKey(ctx, img.ObjectKey) }} {
		v, err := get()
		if err != nil || v.Width != 0 || v.Height != 0 {
			t.Fatalf("image=%+v %v", v, err)
		}
	}
	if rows, err := s.ListImages(ctx, u.ID, 10, 0, ""); err != nil || len(rows) != 1 {
		t.Fatalf("user images=%v %v", rows, err)
	}
	if rows, err := s.ListAdminImages(ctx, 10, 0); err != nil || len(rows) != 1 {
		t.Fatalf("admin images=%v %v", rows, err)
	}
	if _, err := s.SoftDeleteImage(ctx, u.ID, img.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePendingDelete(ctx, img.ObjectKey, model.DefaultBucketID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM images WHERE id=?`, img.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.PendingDeletes(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatal("hard-delete of cleaned tombstone requeued default cleanup")
	}
}

func TestOversizedAtomicBatchRejectedWithoutMutation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin := newUser(t, s, "admin@example.test", "admin")
	u := newUser(t, s, "user@example.test", "user")
	for i := 0; i <= MaxAdminDeleteImages; i++ {
		if err := s.InsertImage(ctx, &model.Image{ID: fmt.Sprint(i), UserID: u.ID, ObjectKey: fmt.Sprint(i), MIME: "image/png", SHA256: fmt.Sprint(i), Visibility: "public"}); err != nil {
			t.Fatal(err)
		}
	}
	f := AdminImageFilter{UserIDs: []string{u.ID}}
	count, fp, err := s.AdminImageSummary(ctx, f)
	if err != nil || count != 1001 || fp != "" {
		t.Fatalf("summary=%d %q %v", count, fp, err)
	}
	if _, err := s.QueueAdminImageDeletion(ctx, admin.ID, "", f, nil, true, "forged", "https://img.test"); !errors.Is(err, ErrImageSelectionTooLarge) {
		t.Fatalf("delete=%v", err)
	}
	if n, _ := s.CountUserImages(ctx, u.ID); n != 1001 {
		t.Fatalf("partial deletion: %d", n)
	}
}

func TestSessionCapPreservesCurrentLogin(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "sessions@example.test", "user")
	for i := 0; i < 55; i++ {
		if err := s.CreateSession(ctx, &model.Session{ID: fmt.Sprint(i), UserID: u.ID, TokenHash: fmt.Sprint(i), CreatedAt: int64(i), LastSeenAt: int64(i), ExpiresAt: now() + 3600}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListUserSessions(ctx, u.ID)
	if err != nil || len(rows) != 50 {
		t.Fatalf("sessions=%d %v", len(rows), err)
	}
	if _, _, err := s.SessionUser(ctx, "54"); err != nil {
		t.Fatal("new session evicted", err)
	}
	if _, _, err := s.SessionUser(ctx, "0"); !errors.Is(err, ErrNotFound) {
		t.Fatal("oldest session retained")
	}
}

func TestUserFilterSearchIsBoundedAndValidatesAgainstAllUsers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 205; i++ {
		newUser(t, s, fmt.Sprintf("%03d@example.test", i), "user")
	}
	rows, err := s.AdminImageUsers(ctx)
	if err != nil || len(rows) != 200 {
		t.Fatalf("options=%d %v", len(rows), err)
	}
	selected := []string{"usr_204@example.test"}
	rows, err = s.SearchAdminImageUsers(ctx, "000", selected)
	if err != nil || len(rows) != 2 {
		t.Fatalf("selected search=%+v %v", rows, err)
	}
	if ids, err := s.ValidateAdminImageUsers(ctx, selected); err != nil || len(ids) != 1 {
		t.Fatalf("selected=%v %v", ids, err)
	}
	if _, err := s.ValidateAdminImageUsers(ctx, []string{"unknown"}); !errors.Is(err, ErrInvalidImageSelection) {
		t.Fatal("unknown option accepted")
	}
}
