package upload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"fujipic/internal/model"
	"fujipic/internal/storage"
)

// A failed response does not prove that the storage server rejected the PUT.
type ambiguousPutStore struct {
	storage.Store
	failDelete bool
	cancel     context.CancelFunc
}

func (s *ambiguousPutStore) Put(ctx context.Context, key string, body io.Reader, size int64, mime string) error {
	if err := s.Store.Put(ctx, key, body, size, mime); err != nil {
		return err
	}
	if s.cancel != nil {
		s.cancel()
	}
	return errors.New("response lost after commit")
}

func (s *ambiguousPutStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.failDelete {
		return errors.New("delete unavailable")
	}
	return s.Store.Delete(ctx, key)
}

func TestAmbiguousPutIsCleanedOrQueued(t *testing.T) {
	for _, failDelete := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup", true: "retry"}[failDelete], func(t *testing.T) {
			svc, st, objects := newTestService(t)
			user := newTestUser(t, st)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc.Buckets.(*testBuckets).def = &ambiguousPutStore{Store: objects, failDelete: failDelete, cancel: cancel}
			data := testPNG(t)
			if _, err := svc.Upload(ctx, user, bytes.NewReader(data), "test.png", "public"); !errors.Is(err, ErrStorage) {
				t.Fatalf("Upload = %v", err)
			}
			pending, err := st.PendingDeletes(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if failDelete {
				if len(pending) != 1 || pending[0].BucketID != model.DefaultBucketID || objects.Len() != 1 {
					t.Fatalf("pending=%+v objects=%d", pending, objects.Len())
				}
			} else if len(pending) != 0 || objects.Len() != 0 {
				t.Fatalf("pending=%+v objects=%d", pending, objects.Len())
			}
			got, err := st.UserByID(context.Background(), user.ID)
			if err != nil || got.UsedBytes != 0 {
				t.Fatalf("user=%+v err=%v", got, err)
			}
			if got := svc.Buckets.(*testBuckets).released[model.DefaultBucketID]; got != int64(len(data)) {
				t.Fatalf("released=%d", got)
			}
		})
	}
}

func TestDeletePurgeQueueFailureRollsBack(t *testing.T) {
	svc, st, objects := newTestService(t)
	user := newTestUser(t, st)
	ctx := context.Background()
	res, err := svc.Upload(ctx, user, bytes.NewReader(testPNG(t)), "test.png", "public")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`CREATE TRIGGER fail_purge BEFORE INSERT ON cdn_purges BEGIN SELECT RAISE(ABORT, 'queue unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, user.ID, res.Image.ID, ""); err == nil {
		t.Error("delete succeeded without a durable purge task")
	}
	if _, err := st.ImageByID(ctx, user.ID, res.Image.ID); err != nil {
		t.Errorf("image lost: %v", err)
	}
	if objects.Len() != 1 {
		t.Error("object removed before purge task was committed")
	}
}
