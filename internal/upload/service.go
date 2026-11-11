// Package upload implements the image upload, deduplication, quota and
// deletion business logic.
package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"fujipic/internal/imagefile"
	"fujipic/internal/model"
	"fujipic/internal/storage"
	"fujipic/internal/store"
)

var (
	ErrOnlyPublic = errors.New("only public visibility is supported in v1")
	ErrStorage    = errors.New("storage error")
	ErrNoCapacity = errors.New("storage capacity exhausted")
)

// BucketResolver selects the global storage bucket for an upload and resolves
// existing image bucket IDs for reads and deletes.
type BucketResolver interface {
	Resolve(ctx context.Context, bucketID string) (storage.Store, error)
	SelectForUpload(ctx context.Context, size int64) (storage.Store, *model.StorageBucket, error)
	Release(ctx context.Context, bucketID string, size int64) error
}

type Service struct {
	Store      *store.Store
	Buckets    BucketResolver
	PublicBase string
	TmpDir     string
	MaxBytes   int64
	MaxPixels  int64
	MaxDim     int
	Log        *slog.Logger
}

type Result struct {
	Image        *model.Image
	Deduplicated bool
	URL          string
}

func (s *Service) URL(img *model.Image) string {
	return s.PublicBase + "/" + img.ObjectKey
}

func (s *Service) Upload(ctx context.Context, user *model.User, r io.Reader, filename, visibility string) (*Result, error) {
	if visibility == "" {
		visibility = "public"
	}
	if visibility != "public" {
		return nil, ErrOnlyPublic
	}
	info, err := imagefile.Process(r, s.TmpDir, s.MaxBytes, s.MaxPixels, s.MaxDim)
	if err != nil {
		return nil, err
	}
	defer info.Cleanup()

	// Per-user deduplication: the same bytes uploaded twice return the first URL.
	if existing, err := s.Store.ImageBySHA(ctx, user.ID, info.SHA256); err == nil {
		return &Result{Image: existing, Deduplicated: true, URL: s.URL(existing)}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	if err := s.Store.ReserveQuota(ctx, user.ID, info.Size); err != nil {
		return nil, err
	}

	objects, bucketMeta, err := s.Buckets.SelectForUpload(ctx, info.Size)
	if err != nil {
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		s.releaseQuota(cleanupCtx, user.ID, info.Size)
		cancelCleanup()
		if errors.Is(err, store.ErrBucketCapacity) {
			return nil, ErrNoCapacity
		}
		return nil, err
	}
	bucketID := model.DefaultBucketID
	if bucketMeta != nil && bucketMeta.ID != "" {
		bucketID = bucketMeta.ID
	}

	now := time.Now().UTC()
	objectKey := fmt.Sprintf("i/%04d/%02d/%s%s", now.Year(), int(now.Month()), randomKey(), info.Ext)
	img := &model.Image{
		ID:           newImageID(),
		UserID:       user.ID,
		BucketID:     bucketID,
		ObjectKey:    objectKey,
		OriginalName: imagefile.BaseName(filename),
		MIME:         info.MIME,
		SizeBytes:    info.Size,
		Width:        info.Width,
		Height:       info.Height,
		SHA256:       info.SHA256,
		Visibility:   visibility,
		CreatedAt:    now.Unix(),
	}

	f, err := os.Open(info.TempPath)
	if err != nil {
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		s.release(cleanupCtx, user.ID, bucketID, info.Size)
		cancelCleanup()
		return nil, fmt.Errorf("open temp file: %w", err)
	}
	err = objects.Put(ctx, objectKey, f, info.Size, info.MIME)
	f.Close()
	if err != nil {
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		// PUT may have committed remotely before the response was lost.
		s.cleanupObject(cleanupCtx, objects, objectKey, bucketID)
		s.release(cleanupCtx, user.ID, bucketID, info.Size)
		cancelCleanup()
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}

	if err := s.Store.InsertImage(ctx, img); err != nil {
		// Roll back the object, quota, and bucket reservation. If the unique
		// index fired, another request won the race: return that image instead.
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		s.cleanupObject(cleanupCtx, objects, objectKey, bucketID)
		s.release(cleanupCtx, user.ID, bucketID, info.Size)
		cancelCleanup()
		if errors.Is(err, store.ErrConflict) {
			existing, lookupErr := s.Store.ImageBySHA(ctx, user.ID, info.SHA256)
			if lookupErr == nil {
				return &Result{Image: existing, Deduplicated: true, URL: s.URL(existing)}, nil
			}
		}
		return nil, err
	}

	return &Result{Image: img, URL: s.URL(img)}, nil
}

// Delete soft-deletes an image, removes the object and enqueues a CDN purge.
func (s *Service) Delete(ctx context.Context, userID, imageID, ip string) error {
	return s.deleteImage(ctx, userID, userID, imageID, ip, "image.delete")
}

var ErrAdminRequired = errors.New("active administrator required")

// DeleteAsAdmin rechecks the actor against the database, then uses the same
// quota, object-cleanup and CDN-purge transaction as an owner's deletion.
func (s *Service) DeleteAsAdmin(ctx context.Context, actorID, imageID, ip string) error {
	actor, err := s.Store.UserByID(ctx, actorID)
	if err != nil {
		return err
	}
	if !actor.IsAdmin() || !actor.IsActive() {
		return ErrAdminRequired
	}
	img, err := s.Store.AdminImageByID(ctx, imageID)
	if err != nil {
		return err
	}
	return s.deleteImage(ctx, img.UserID, actorID, imageID, ip, "admin.image_delete")
}

func (s *Service) deleteImage(ctx context.Context, userID, actorID, imageID, ip, action string) error {
	img, err := s.Store.SoftDeleteImageWithPurge(ctx, userID, imageID, s.PublicBase)
	if err != nil {
		return err
	}
	bucketID := img.BucketID
	if bucketID == "" {
		bucketID = model.DefaultBucketID
	}
	objects, resolveErr := s.Buckets.Resolve(ctx, bucketID)
	if resolveErr != nil {
		s.Log.Warn("resolve bucket for delete failed; queued for retry", "bucket", bucketID, "key", img.ObjectKey, "err", resolveErr)
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		if err := s.Store.AddPendingDelete(cleanupCtx, img.ObjectKey, bucketID); err != nil {
			s.Log.Error("queue pending delete", "key", img.ObjectKey, "err", err)
		}
		cancelCleanup()
	} else if err := objects.Delete(ctx, img.ObjectKey); err != nil {
		s.Log.Warn("delete object failed; queued for retry", "key", img.ObjectKey, "err", err)
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		if err := s.Store.AddPendingDelete(cleanupCtx, img.ObjectKey, bucketID); err != nil {
			s.Log.Error("queue pending delete", "key", img.ObjectKey, "err", err)
		}
		cancelCleanup()
	} else {
		// SoftDeleteImage queued the object transactionally. The object is
		// gone now, so drop the retry entry; otherwise every deletion would
		// leave a pending row that the worker later fails to resolve.
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		if err := s.Store.RemovePendingDelete(cleanupCtx, img.ObjectKey, bucketID); err != nil {
			s.Log.Error("remove pending delete", "key", img.ObjectKey, "err", err)
		}
		cancelCleanup()
	}
	cleanupCtx, cancelCleanup := cleanupContext(ctx)
	defer cancelCleanup()
	audit := &model.AuditLog{
		ID:        newAuditID(),
		ActorID:   &actorID,
		Action:    action,
		Target:    imageID,
		IP:        ip,
		Meta:      img.ObjectKey,
		CreatedAt: time.Now().Unix(),
	}
	if err := s.Store.AddAudit(cleanupCtx, audit); err != nil {
		s.Log.Error("write audit log", "err", err)
	}
	return nil
}

// cleanupContext detaches compensating work from a client-canceled request
// context while retaining a hard deadline for the cleanup operations.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}

func (s *Service) cleanupObject(ctx context.Context, objects storage.Store, key, bucketID string) {
	// Persist the retry before network I/O; a failed or interrupted delete
	// must not leave an object with neither metadata nor a cleanup task.
	if err := s.Store.AddPendingDelete(ctx, key, bucketID); err != nil {
		s.Log.Error("queue upload cleanup", "key", key, "err", err)
	}
	if err := objects.Delete(ctx, key); err != nil {
		s.Log.Warn("upload cleanup queued for retry", "key", key, "err", err)
		return
	}
	if err := s.Store.RemovePendingDelete(ctx, key, bucketID); err != nil {
		s.Log.Error("remove upload cleanup", "key", key, "err", err)
	}
}

func (s *Service) releaseQuota(ctx context.Context, userID string, size int64) {
	if err := s.Store.ReleaseQuota(ctx, userID, size); err != nil {
		s.Log.Error("release quota", "user", userID, "bytes", size, "err", err)
	}
}

func (s *Service) release(ctx context.Context, userID, bucketID string, size int64) {
	s.releaseQuota(ctx, userID, size)
	if err := s.Buckets.Release(ctx, bucketID, size); err != nil {
		s.Log.Error("release bucket capacity", "bucket", bucketID, "bytes", size, "err", err)
	}
}
