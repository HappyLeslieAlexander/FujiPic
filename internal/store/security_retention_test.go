package store

import (
	"context"
	"testing"
)

func TestDeletedReplayDoesNotRecreatePurges(t *testing.T) {
	s, img := mediaFixture(t)
	ctx := context.Background()
	first, err := s.SoftDeleteImageWithPurge(ctx, img.UserID, img.ID, "https://img.test")
	if err != nil || first.AlreadyDeleted {
		t.Fatal(first, err)
	}
	if _, err := s.db.Exec(`DELETE FROM cdn_purges`); err != nil {
		t.Fatal(err)
	}
	again, err := s.SoftDeleteImageWithPurge(ctx, img.UserID, img.ID, "https://img.test")
	if err != nil || !again.AlreadyDeleted {
		t.Fatal(again, err)
	}
	purges, err := s.PendingCDNPurges(ctx, 10)
	if err != nil || len(purges) != 0 {
		t.Fatal(purges, err)
	}
}

func TestRetentionPreservesCleanupObligations(t *testing.T) {
	s, img := mediaFixture(t)
	ctx := context.Background()
	if _, err := s.SoftDeleteImageWithPurge(ctx, img.UserID, img.ID, "https://img.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE images SET deleted_at=1`); err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		if err := s.PruneHistory(ctx); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM images`).Scan(&count); err != nil || count != want {
			t.Fatal(count, err)
		}
	}
	check(1)
	if _, err := s.db.Exec(`DELETE FROM pending_deletes`); err != nil {
		t.Fatal(err)
	}
	check(1)
	if _, err := s.db.Exec(`DELETE FROM cdn_purges`); err != nil {
		t.Fatal(err)
	}
	check(0)
	pending, err := s.PendingDeletes(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("pruning requeued objects", pending, err)
	}
}

func TestBusyPreviewDoesNotSpendAttempt(t *testing.T) {
	s, _ := mediaFixture(t)
	ctx := context.Background()
	j, err := s.ClaimMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeferPreview(ctx, *j); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := s.db.QueryRow(`SELECT attempts FROM media_jobs`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal(attempts, err)
	}
	if _, err := s.ClaimMedia(ctx); err != ErrNotFound {
		t.Fatal("busy-loop retry", err)
	}
}
