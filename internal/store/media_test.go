package store

import (
	"context"
	"errors"
	"testing"

	"fujipic/internal/model"
)

func mediaFixture(t *testing.T) (*Store, *model.Image) {
	t.Helper()
	s := newTestStore(t)
	u := newUser(t, s, "media@example.test", "user")
	if _, err := s.db.Exec(`UPDATE users SET quota_bytes=1000 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{ID: model.DefaultBucketID, Name: "default", Enabled: true, IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	img := &model.Image{ID: "img_media", UserID: u.ID, BucketID: model.DefaultBucketID, ObjectKey: "i/2026/11/aaaaaaaaaaaaaaaaaaaaaaaaaa.svg", MIME: "image/svg+xml", SizeBytes: 100, Width: 10, Height: 10, SHA256: "media", Visibility: "public", CreatedAt: 1}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileUserUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileStorageBucketUsage(ctx); err != nil {
		t.Fatal(err)
	}
	return s, img
}

func TestPreviewLifecycleAndAtomicDeletion(t *testing.T) {
	s, img := mediaFixture(t)
	ctx := context.Background()
	j, err := s.ClaimMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := "i/2026/11/preview.png"
	if err := s.ReservePreview(ctx, *j, key, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewImage(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatal("unpublished preview visible", err)
	}
	if err := s.CompletePreview(ctx, *j); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileUserUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileStorageBucketUsage(ctx); err != nil {
		t.Fatal(err)
	}
	u, _ := s.UserByID(ctx, img.UserID)
	b, _ := s.StorageBucketByID(ctx, img.BucketID)
	if u.UsedBytes != 130 || b.UsedBytes != 130 {
		t.Fatalf("usage %d/%d", u.UsedBytes, b.UsedBytes)
	}
	p, err := s.PreviewImage(ctx, key)
	if err != nil || p.MIME != "image/png" || p.SizeBytes != 30 {
		t.Fatal(p, err)
	}
	for range 2 {
		if _, err := s.SoftDeleteImageWithPurge(ctx, img.UserID, img.ID, "https://images.test", "https://alias.test"); err != nil {
			t.Fatal(err)
		}
	}
	u, _ = s.UserByID(ctx, img.UserID)
	b, _ = s.StorageBucketByID(ctx, img.BucketID)
	if u.UsedBytes != 0 || b.UsedBytes != 0 {
		t.Fatalf("deletion usage %d/%d", u.UsedBytes, b.UsedBytes)
	}
	if _, err := s.PreviewImage(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted preview visible", err)
	}
	pending, err := s.PendingDeletes(ctx, 20)
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	purges, err := s.PendingCDNPurges(ctx, 20)
	if err != nil || len(purges) != 4 {
		t.Fatal(purges, err)
	}
}

func TestPreviewLeaseRecoveryAndDeletionRace(t *testing.T) {
	s, img := mediaFixture(t)
	ctx := context.Background()
	j, err := s.ClaimMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReservePreview(ctx, *j, "old-preview", 40); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE media_jobs SET lease_until=0`); err != nil {
		t.Fatal(err)
	}
	newJob, err := s.ClaimMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newJob.Lease == j.Lease || newJob.Attempts != 2 {
		t.Fatal("lease not rotated")
	}
	if err := s.CompletePreview(ctx, *j); !errors.Is(err, ErrConflict) {
		t.Fatal("stale worker published", err)
	}
	u, _ := s.UserByID(ctx, img.UserID)
	if u.UsedBytes != 100 {
		t.Fatal("abandoned reservation leaked")
	}
	if pending, _ := s.HasPendingDelete(ctx, "old-preview", img.BucketID); !pending {
		t.Fatal("abandoned object not queued")
	}
	if err := s.ReservePreview(ctx, *newJob, "new-preview", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SoftDeleteImage(ctx, img.UserID, img.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompletePreview(ctx, *newJob); !errors.Is(err, ErrConflict) {
		t.Fatal("deleted image resurrected", err)
	}
	u, _ = s.UserByID(ctx, img.UserID)
	if u.UsedBytes != 0 {
		t.Fatal("reservation not released on deletion")
	}
}

func TestPreviewQuotaAndFailedJobs(t *testing.T) {
	s, img := mediaFixture(t)
	ctx := context.Background()
	j, err := s.ClaimMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET quota_bytes=110 WHERE id=?`, img.UserID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReservePreview(ctx, *j, "too-large", 20); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatal(err)
	}
	m, _ := s.MediaStatus(ctx, img.ID)
	if m.PreviewBytes != 0 || m.PreviewKey != "" {
		t.Fatal("failed reservation persisted")
	}
	if err := s.FailPreview(ctx, *j, true); err != nil {
		t.Fatal(err)
	}
	m, _ = s.MediaStatus(ctx, img.ID)
	if m.State != "failed" {
		t.Fatal(m)
	}
	if _, err := s.ClaimMedia(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("permanent failure retried")
	}
	if _, err := s.ImageByID(ctx, img.UserID, img.ID); err != nil {
		t.Fatal("original lost", err)
	}
}
