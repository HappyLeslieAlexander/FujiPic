package store

import (
	"context"
	"fmt"
	"fujipic/internal/model"
	"testing"
	"time"
)

func TestPhysicalUserDeletionQueuesAllCDNOrigins(t *testing.T) {
	s, img := mediaFixture(t)
	ctx := context.Background()
	bases := []string{"https://img.test", "https://alias.test"}
	if err := s.ConfigurePurgeOrigins(ctx, bases); err != nil {
		t.Fatal(err)
	}
	j, err := s.ClaimMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReservePreview(ctx, *j, "i/preview.png", 20); err != nil {
		t.Fatal(err)
	}
	if err := s.CompletePreview(ctx, *j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id=?`, img.UserID); err != nil {
		t.Fatal(err)
	}
	purges, err := s.PendingCDNPurges(ctx, 20)
	if err != nil || len(purges) != 4 {
		t.Fatal(purges, err)
	}
	urls := map[string]bool{}
	for _, p := range purges {
		urls[p.URL] = true
	}
	for _, base := range bases {
		for _, key := range []string{img.ObjectKey, "i/preview.png"} {
			if !urls[base+"/"+key] {
				t.Fatal("missing URL", base, key)
			}
		}
	}
	objects, err := s.PendingDeletes(ctx, 20)
	if err != nil || len(objects) != 2 {
		t.Fatal(objects, err)
	}
	// Config changes do not discard obligations already queued.
	if err := s.ConfigurePurgeOrigins(ctx, nil); err != nil {
		t.Fatal(err)
	}
	purges, err = s.PendingCDNPurges(ctx, 20)
	if err != nil || len(purges) != 4 {
		t.Fatal(purges, err)
	}
}

func TestPhysicalDeletionPurgeConfigurationAndTombstones(t *testing.T) {
	for _, mode := range []string{"active", "disabled", "tombstone"} {
		t.Run(mode, func(t *testing.T) {
			s, img := mediaFixture(t)
			ctx := context.Background()
			if err := s.ConfigurePurgeOrigins(ctx, []string{"https://old.test"}); err != nil {
				t.Fatal(err)
			}
			if err := s.ConfigurePurgeOrigins(ctx, []string{"https://new.test/", "https://new.test"}); err != nil {
				t.Fatal(err)
			}
			if mode == "disabled" {
				if err := s.ConfigurePurgeOrigins(ctx, nil); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "tombstone" {
				if _, err := s.SoftDeleteImageWithPurge(ctx, img.UserID, img.ID, "https://new.test"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec(`DELETE FROM cdn_purges`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(`DELETE FROM images WHERE id=?`, img.ID); err != nil {
				t.Fatal(err)
			}
			purges, err := s.PendingCDNPurges(ctx, 20)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "active" {
				if len(purges) != 1 || purges[0].URL != "https://new.test/"+img.ObjectKey {
					t.Fatal(purges)
				}
			} else if len(purges) != 0 {
				t.Fatal("unexpected purge", purges)
			}
		})
	}
}

func TestBatchDeletionReleasesPreviewReservations(t *testing.T) {
	s, img := mediaFixture(t)
	ctx := context.Background()
	admin := newUser(t, s, "batchadmin@test", "admin")
	j, err := s.ClaimMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReservePreview(ctx, *j, "i/pending-preview.png", 20); err != nil {
		t.Fatal(err)
	}
	n, err := s.QueueAdminImageDeletion(ctx, admin.ID, "test", AdminImageFilter{}, []string{img.ID}, false, "", "https://img.test", "https://alias.test")
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	u, err := s.UserByID(ctx, img.UserID)
	if err != nil || u.UsedBytes != 0 {
		t.Fatal(u, err)
	}
	b, err := s.StorageBucketByID(ctx, img.BucketID)
	if err != nil || b.UsedBytes != 0 {
		t.Fatal(b, err)
	}
	purges, err := s.PendingCDNPurges(ctx, 20)
	if err != nil || len(purges) != 4 {
		t.Fatal(purges, err)
	}
	objects, err := s.PendingDeletes(ctx, 20)
	if err != nil || len(objects) != 2 {
		t.Fatal(objects, err)
	}
	if err := s.CompletePreview(ctx, *j); err != ErrConflict {
		t.Fatal("deleted preview published", err)
	}
}

func TestSingleUserFullFilterStillRequiresExplicitSelection(t *testing.T) {
	s, img := mediaFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, img.UserID); err != nil {
		t.Fatal(err)
	}
	f := AdminImageFilter{UserIDs: []string{img.UserID}}
	_, fp, err := s.AdminImageSummary(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueAdminImageDeletion(ctx, img.UserID, "", f, nil, true, fp); err != ErrInvalidImageSelection {
		t.Fatal("all-values rule changed", err)
	}
	if n, err := s.QueueAdminImageDeletion(ctx, img.UserID, "", f, []string{img.ID}, false, ""); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestAtomicThousandImageDeletion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin := newUser(t, s, "admin1000@test", "admin")
	user := newUser(t, s, "owner1000@test", "user")
	if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{ID: model.DefaultBucketID, Enabled: true, IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 1000; i++ {
		_, err = tx.ExecContext(ctx, `INSERT INTO images(id,user_id,bucket_id,object_key,mime,size_bytes,width,height,sha256,visibility,created_at) VALUES(?,?,? ,?,'image/png',10,1,1,?,'public',1)`, fmt.Sprint("img_", i), user.ID, model.DefaultBucketID, fmt.Sprintf("i/%d.png", i), fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileUserUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileStorageBucketUsage(ctx); err != nil {
		t.Fatal(err)
	}
	f := AdminImageFilter{UserIDs: []string{user.ID}}
	_, fp, err := s.AdminImageSummary(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n, err := s.QueueAdminImageDeletion(ctx, admin.ID, "", f, nil, true, fp, "https://img.test", "https://alias.test")
	t.Logf("atomic 1000-image deletion: %s", time.Since(start))
	if err != nil || n != 1000 {
		t.Fatal(n, err)
	}
	for table, want := range map[string]int{"pending_deletes": 1000, "cdn_purges": 2000, "audit_logs": 1000} {
		var got int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil || got != want {
			t.Fatal(table, got, err)
		}
	}
}
