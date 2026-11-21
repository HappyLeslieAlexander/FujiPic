package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fujipic/internal/model"
)

func adminImageFixture(t *testing.T) (*Store, string, []string) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	admin := newUser(t, s, "admin@test", "admin")
	users := []string{newUser(t, s, "a@test", "user").ID, newUser(t, s, "b@test", "user").ID}
	for _, bid := range []string{model.DefaultBucketID, "bkt_other", "bkt_empty"} {
		b := &model.StorageBucket{ID: bid, Name: bid, Bucket: bid, Region: "test", Enabled: true}
		var err error
		if bid == model.DefaultBucketID {
			err = s.EnsureDefaultStorageBucket(ctx, b)
		} else {
			err = s.CreateStorageBucket(ctx, b)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 64; i++ {
		bid := model.DefaultBucketID
		if i%2 == 1 {
			bid = "bkt_other"
		}
		if i == 0 {
			bid = ""
		} // legacy NULL bucket
		img := &model.Image{ID: fmt.Sprintf("img_%03d", i), UserID: users[i%2], BucketID: bid,
			ObjectKey: fmt.Sprintf("i/test/%03d.png", i), MIME: "image/png", SizeBytes: 10, Width: 1, Height: 1,
			SHA256: fmt.Sprint(i), Visibility: "public", CreatedAt: int64(i + 1)}
		if err := s.InsertImage(ctx, img); err != nil {
			t.Fatal(err)
		}
	}
	for _, uid := range users {
		if _, err := s.db.Exec(`UPDATE users SET used_bytes=320 WHERE id=?`, uid); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReconcileStorageBucketUsage(ctx); err != nil {
		t.Fatal(err)
	}
	return s, admin.ID, users
}

func TestAdminImageMultiFiltersAndPagination(t *testing.T) {
	s, _, users := adminImageFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		f     AdminImageFilter
		count int
	}{
		{"all", AdminImageFilter{}, 64},
		{"users-or", AdminImageFilter{UserIDs: users}, 64},
		{"buckets-or", AdminImageFilter{BucketIDs: []string{model.DefaultBucketID, "bkt_other"}}, 64},
		{"intersection", AdminImageFilter{UserIDs: users[:1], BucketIDs: []string{model.DefaultBucketID, "bkt_other"}}, 32},
		{"empty-intersection", AdminImageFilter{UserIDs: users[:1], BucketIDs: []string{"bkt_other"}}, 0},
		{"legacy-default", AdminImageFilter{BucketIDs: []string{model.DefaultBucketID}}, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count, _, err := s.AdminImageSummary(ctx, tc.f)
			if err != nil || count != tc.count {
				t.Fatalf("summary=%d %v", count, err)
			}
			rows, err := s.ListFilteredAdminImages(ctx, tc.f, 100, 0)
			if err != nil || len(rows) != tc.count {
				t.Fatalf("list=%d %v", len(rows), err)
			}
		})
	}
	first, _ := s.ListAdminImages(ctx, 60, 0)
	second, err := s.ListAdminImages(ctx, 60, 60)
	if err != nil || len(first) != 60 || len(second) != 4 || first[59].ID == second[0].ID {
		t.Fatal("pagination", err)
	}
}

func TestAdminBatchDeletionQueuesAndQuotas(t *testing.T) {
	s, admin, users := adminImageFixture(t)
	ctx := context.Background()
	// A restrictive bucket filter matches 64 images, across the 60-image page.
	f := AdminImageFilter{BucketIDs: []string{model.DefaultBucketID, "bkt_other"}}
	_, fingerprint, _ := s.AdminImageSummary(ctx, f)
	n, err := s.QueueAdminImageDeletion(ctx, admin, "127.0.0.1", f, nil, true, fingerprint, "https://images.test", "https://alias.test")
	if err != nil || n != 64 {
		t.Fatalf("delete=%d %v", n, err)
	}
	for _, uid := range users {
		u, _ := s.UserByID(ctx, uid)
		if u.UsedBytes != 0 {
			t.Fatal("quota not released", u)
		}
	}
	for _, bid := range []string{model.DefaultBucketID, "bkt_other"} {
		b, _ := s.StorageBucketByID(ctx, bid)
		if b.UsedBytes != 0 {
			t.Fatal("bucket usage not released", b)
		}
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM images WHERE deleted_at IS NOT NULL`:                             64,
		`SELECT COUNT(*) FROM pending_deletes`:                                                 64,
		`SELECT COUNT(*) FROM pending_deletes WHERE bucket_id='bkt_default'`:                   32,
		`SELECT COUNT(*) FROM cdn_purges`:                                                      128,
		`SELECT COUNT(*) FROM audit_logs WHERE action='admin.image_delete' AND ip='127.0.0.1'`: 64,
	} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: %d %v", query, got, err)
		}
	}
	if _, err := s.QueueAdminImageDeletion(ctx, admin, "", f, nil, true, fingerprint); !errors.Is(err, ErrImageSelectionChanged) {
		t.Fatal("replay", err)
	}
}

func TestAdminBatchDeletionRejectsInvalidAndRollsBack(t *testing.T) {
	s, admin, users := adminImageFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, actor string
		f           AdminImageFilter
		ids         []string
		all         bool
		fp          string
		want        error
	}{
		{"empty", admin, AdminImageFilter{}, nil, false, "", ErrInvalidImageSelection},
		{"unfiltered-all", admin, AdminImageFilter{}, nil, true, "x", ErrInvalidImageSelection},
		{"all-options", admin, AdminImageFilter{UserIDs: append([]string{admin}, users...), BucketIDs: []string{model.DefaultBucketID, "bkt_other", "bkt_empty"}}, nil, true, "x", ErrInvalidImageSelection},
		{"non-admin", users[0], AdminImageFilter{}, []string{"img_000"}, false, "", ErrAdminRequired},
		{"missing", admin, AdminImageFilter{}, []string{"img_000", "missing"}, false, "", ErrInvalidImageSelection},
		{"outside-filter", admin, AdminImageFilter{UserIDs: users[:1]}, []string{"img_000", "img_001"}, false, "", ErrInvalidImageSelection},
		{"stale", admin, AdminImageFilter{UserIDs: users[:1]}, nil, true, "stale", ErrImageSelectionChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if n, err := s.QueueAdminImageDeletion(ctx, tc.actor, "", tc.f, tc.ids, tc.all, tc.fp); n != 0 || !errors.Is(err, tc.want) {
				t.Fatalf("%d %v", n, err)
			}
		})
	}
	// Fail on the second audit write, after the first image's quota/queues changed.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_batch BEFORE INSERT ON audit_logs WHEN NEW.target='img_001' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueAdminImageDeletion(ctx, admin, "", AdminImageFilter{}, []string{"img_000", "img_001"}, false, "", "https://images.test"); err == nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	for _, table := range []string{"audit_logs", "pending_deletes", "cdn_purges"} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback %s: %d %v", table, count, err)
		}
	}
	if n, _, _ := s.AdminImageSummary(ctx, AdminImageFilter{}); n != 64 {
		t.Fatal("partial deletion", n)
	}
	for _, uid := range users {
		u, _ := s.UserByID(ctx, uid)
		if u.UsedBytes != 320 {
			t.Fatal("quota rollback", u.UsedBytes)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_batch`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueAdminImageDeletion(ctx, admin, "", AdminImageFilter{}, []string{"img_000", "img_001"}, false, "", "https://images.test"); err != nil || n != 2 {
		t.Fatalf("selected: %d %v", n, err)
	}
	if _, err := s.db.Exec(`UPDATE users SET status='disabled' WHERE id=?`, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueAdminImageDeletion(ctx, admin, "", AdminImageFilter{}, []string{"img_002"}, false, ""); !errors.Is(err, ErrAdminRequired) {
		t.Fatal("disabled admin", err)
	}
}

func TestAdminBatchFingerprintRejectsNewUploads(t *testing.T) {
	s, admin, users := adminImageFixture(t)
	ctx := context.Background()
	f := AdminImageFilter{UserIDs: users[:1]}
	_, fingerprint, _ := s.AdminImageSummary(ctx, f)
	img := &model.Image{ID: "img_new", UserID: users[0], BucketID: model.DefaultBucketID, ObjectKey: "i/new.png", MIME: "image/png", SizeBytes: 1, Width: 1, Height: 1, SHA256: "new", Visibility: "public", CreatedAt: 100}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueAdminImageDeletion(ctx, admin, "", f, nil, true, fingerprint); n != 0 || !errors.Is(err, ErrImageSelectionChanged) {
		t.Fatalf("new upload: %d %v", n, err)
	}
	if n, _, _ := s.AdminImageSummary(ctx, f); n != 33 {
		t.Fatal("new result set changed", n)
	}
}
