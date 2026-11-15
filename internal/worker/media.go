package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"time"

	"fujipic/internal/id"
	"fujipic/internal/imagefile"
	"fujipic/internal/media"
	"fujipic/internal/store"
)

type PreviewConverter interface {
	Convert(context.Context, string, string, string, int64, int) (*imagefile.Info, error)
}

type Media struct {
	Store               *store.Store
	Buckets             BucketResolver
	Client              PreviewConverter
	TempDir             string
	MaxBytes, MaxPixels int64
	MaxDim              int
	Log                 *slog.Logger
}

func (m *Media) Run(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.RunOnce(ctx)
		}
	}
}

func (m *Media) RunOnce(ctx context.Context) {
	j, err := m.Store.ClaimMedia(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		m.Log.Error("claim preview", "err", err)
		return
	}
	jobCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	err = m.process(jobCtx, *j)
	cancel()
	if err != nil {
		m.Log.Warn("preview failed", "image", j.ImageID, "attempt", j.Attempts, "err", err)
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer stop()
		if errors.Is(err, media.ErrBusy) {
			if e := m.Store.DeferPreview(cleanup, *j); e != nil && !errors.Is(e, store.ErrConflict) {
				m.Log.Error("defer busy preview", "err", e)
			}
			return
		}
		permanent := errors.Is(err, imagefile.ErrUnsupported) || errors.Is(err, imagefile.ErrDimensions) || errors.Is(err, store.ErrQuotaExceeded) || errors.Is(err, store.ErrBucketCapacity)
		if e := m.Store.FailPreview(cleanup, *j, permanent); e != nil && !errors.Is(e, store.ErrConflict) {
			m.Log.Error("record preview failure", "err", e)
		}
	}
}

func (m *Media) process(ctx context.Context, j store.MediaJob) error {
	img, err := m.Store.AdminImageByID(ctx, j.ImageID)
	if err != nil {
		return err
	}
	if img.DeletedAt != nil {
		return store.ErrNotFound
	}
	objects, err := m.Buckets.Resolve(ctx, img.BucketID)
	if err != nil {
		return err
	}
	obj, err := objects.Get(ctx, img.ObjectKey, "")
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	f, err := os.CreateTemp(m.TempDir, "fujipic-upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	limit := m.MaxBytes
	if limit < math.MaxInt64 {
		limit++
	}
	n, err := io.Copy(f, io.LimitReader(obj.Body, limit))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if n != img.SizeBytes || n > m.MaxBytes {
		return imagefile.ErrTooLarge
	}
	info, err := m.Client.Convert(ctx, f.Name(), imagefile.ExtendedFormat(img.ObjectKey), m.TempDir, m.MaxPixels, m.MaxDim)
	if err != nil {
		return err
	}
	defer info.Cleanup()
	key := strings.TrimSuffix(img.ObjectKey, "."+imagefile.ExtendedFormat(img.ObjectKey)) + "-" + id.Random(16) + ".preview.png"
	if err := m.Store.ReservePreview(ctx, j, key, info.Size); err != nil {
		return err
	}
	output, err := os.Open(info.TempPath)
	if err != nil {
		return err
	}
	defer output.Close()
	err = objects.Put(ctx, key, output, info.Size, "image/png")
	if err == nil {
		err = m.Store.CompletePreview(ctx, j)
	}
	if err != nil {
		// Deletion may have already consumed its cleanup task while PUT was in
		// flight. Re-enqueue after PUT returns, even when the job was removed.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if e := m.Store.AddPendingDelete(cleanup, key, img.BucketID); e != nil {
			m.Log.Error("queue abandoned preview", "key", key, "err", e)
		}
	}
	return err
}
