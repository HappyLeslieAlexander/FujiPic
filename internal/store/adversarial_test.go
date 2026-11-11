package store

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"fujipic/internal/model"
)

func TestBucketCapacityUpdateRejectsStaleUsage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	stale, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveStorageBucket(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stale.MaxBytes = 50
	if err := s.UpdateStorageBucket(ctx, stale); !errors.Is(err, ErrBucketCapacity) {
		t.Fatalf("stale capacity update = %v", err)
	}
	got, err := s.StorageBucketByID(ctx, stale.ID)
	if err != nil || got.UsedBytes != 100 || got.MaxBytes != 0 {
		t.Fatalf("bucket=%+v err=%v", got, err)
	}
}

func TestUnlimitedBucketCounterCannotOverflow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.ReserveStorageBucket(ctx, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveStorageBucket(ctx, 1); !errors.Is(err, ErrBucketCapacity) {
		t.Fatalf("overflow reservation=%v", err)
	}
	b, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil || b.UsedBytes != math.MaxInt64 {
		t.Fatalf("counter corrupted: %+v %v", b, err)
	}
}

func TestFailedActivityWriteCanRetryImmediately(t *testing.T) {
	s := newTestStore(t)
	u := newUser(t, s, "retry@example.com", "user")
	ctx := context.Background()
	now := time.Now().Unix()
	sess := &model.Session{ID: "session", UserID: u.ID, TokenHash: "session-hash", CreatedAt: now, LastSeenAt: now, ExpiresAt: now + 60}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(ctx, &model.APIToken{ID: "token", UserID: u.ID, Name: "test", TokenHash: "token-hash", TokenPrefix: "fp_test", Scopes: "read", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	s.TouchSession(canceled, sess.ID, time.Hour)
	s.TouchToken(canceled, "token")
	if _, renewed := s.TouchSession(ctx, sess.ID, time.Hour); !renewed {
		t.Error("failed renewal poisoned session throttle")
	}
	s.TouchToken(ctx, "token")
	tokens, err := s.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].LastUsedAt == nil {
		t.Errorf("failed touch poisoned token throttle: %+v", tokens)
	}
}
