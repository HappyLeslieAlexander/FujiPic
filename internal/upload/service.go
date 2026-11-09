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
)

type Service struct {
	Store      *store.Store
	Objects    storage.Store
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

	now := time.Now().UTC()
	objectKey := fmt.Sprintf("i/%04d/%02d/%s%s", now.Year(), int(now.Month()), randomKey(), info.Ext)
	img := &model.Image{
		ID:           newImageID(),
		UserID:       user.ID,
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
		s.release(cleanupCtx, user.ID, info.Size)
		cancelCleanup()
		return nil, fmt.Errorf("open temp file: %w", err)
	}
	err = s.Objects.Put(ctx, objectKey, f, info.Size, info.MIME)
	f.Close()
	if err != nil {
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		s.release(cleanupCtx, user.ID, info.Size)
		cancelCleanup()
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}

	if err := s.Store.InsertImage(ctx, img); err != nil {
		// Roll back the object and quota. If the unique index fired, another
		// request won the race: return that image instead.
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		if delErr := s.Objects.Delete(cleanupCtx, objectKey); delErr != nil {
			_ = s.Store.AddPendingDelete(cleanupCtx, objectKey)
		}
		s.release(cleanupCtx, user.ID, info.Size)
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
	img, err := s.Store.SoftDeleteImage(ctx, userID, imageID)
	if err != nil {
		return err
	}
	if err := s.Objects.Delete(ctx, img.ObjectKey); err != nil {
		s.Log.Warn("delete object failed; queued for retry", "key", img.ObjectKey, "err", err)
		cleanupCtx, cancelCleanup := cleanupContext(ctx)
		if err := s.Store.AddPendingDelete(cleanupCtx, img.ObjectKey); err != nil {
			s.Log.Error("queue pending delete", "key", img.ObjectKey, "err", err)
		}
		cancelCleanup()
	}
	cleanupCtx, cancelCleanup := cleanupContext(ctx)
	defer cancelCleanup()
	if err := s.Store.AddCDNPurge(cleanupCtx, s.URL(img)); err != nil {
		s.Log.Error("queue cdn purge", "url", s.URL(img), "err", err)
	}
	audit := &model.AuditLog{
		ID:        newAuditID(),
		ActorID:   &userID,
		Action:    "image.delete",
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

func (s *Service) release(ctx context.Context, userID string, size int64) {
	if err := s.Store.ReleaseQuota(ctx, userID, size); err != nil {
		s.Log.Error("release quota", "user", userID, "bytes", size, "err", err)
	}
}
