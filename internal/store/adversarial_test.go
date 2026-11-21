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

func TestDefaultBucketNormalizationMergesPendingDeletes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO pending_deletes(object_key,bucket_id,created_at,attempts) VALUES('i/test.png','',1,2),('i/test.png','bkt_default',2,10)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{Name: "Default"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.PendingDeletes(ctx, 10)
	if err != nil || len(rows) != 1 || rows[0].BucketID != model.DefaultBucketID || rows[0].Attempts != 2 || rows[0].CreatedAt != 1 {
		t.Fatalf("merged queue: %+v %v", rows, err)
	}
}

func TestNullableSessionAndAuditFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "legacy@example.test", "user")
	if _, err := s.db.Exec(`INSERT INTO sessions(id,user_id,token_hash,created_at,last_seen_at,expires_at) VALUES('legacy',?,'hash',1,1,?)`, u.ID, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	sessions, err := s.ListUserSessions(ctx, u.ID)
	if err != nil || len(sessions) != 1 || sessions[0].IP != "" || sessions[0].UserAgent != "" {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	if _, err := s.db.Exec(`INSERT INTO audit_logs(id,action,created_at) VALUES('legacy','legacy.action',1)`); err != nil {
		t.Fatal(err)
	}
	logs, err := s.ListAudit(ctx, 10)
	if err != nil || len(logs) != 1 || logs[0].ActorID != nil || logs[0].Target != "" || logs[0].IP != "" || logs[0].Meta != "" {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
}

func TestDefaultBucketNormalizationRollsBackOnFailure(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	original, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO pending_deletes(object_key,bucket_id,created_at) VALUES('legacy','','1');
		CREATE TRIGGER fail_normalization BEFORE DELETE ON pending_deletes BEGIN SELECT RAISE(ABORT,'injected failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{Name: "Changed"}); err == nil {
		t.Fatal("expected normalization failure")
	}
	got, err := s.StorageBucketByID(ctx, model.DefaultBucketID)
	if err != nil || got.Name != original.Name {
		t.Fatalf("partial bucket update: %+v %v", got, err)
	}
	rows, err := s.PendingDeletes(ctx, 10)
	if err != nil || len(rows) != 1 || rows[0].BucketID != "" {
		t.Fatalf("partial queue update: %+v %v", rows, err)
	}
}
