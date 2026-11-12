// Package worker runs background maintenance: bucket deletion retries and
// CDN cache purges.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fujipic/internal/cdn"
	"fujipic/internal/storage"
	"fujipic/internal/store"
)

type BucketResolver interface {
	Default() storage.Store
	Resolve(ctx context.Context, bucketID string) (storage.Store, error)
}

type Maintenance struct {
	Store    *store.Store
	Buckets  BucketResolver
	Purger   cdn.Purger
	Interval time.Duration
	TempDir  string
	Log      *slog.Logger
}

func (m *Maintenance) Run(ctx context.Context) {
	interval := m.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runOnce(ctx)
		}
	}
}

func (m *Maintenance) runOnce(ctx context.Context) {
	m.retryDeletes(ctx)
	m.purgeCDN(ctx)
	m.cleanupTemp(ctx)
	m.cleanupExpired(ctx)
}

func (m *Maintenance) cleanupTemp(ctx context.Context) {
	if m.TempDir == "" {
		return
	}
	entries, err := os.ReadDir(m.TempDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.Log.Error("list temp upload dir", "err", err)
		}
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "fujipic-upload-") {
			continue
		}
		path := filepath.Join(m.TempDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil {
				m.Log.Info("removed stale temp file", "path", path)
			}
		}
	}
}

func (m *Maintenance) cleanupExpired(ctx context.Context) {
	if sessions, err := m.Store.DeleteExpiredSessions(ctx); err != nil {
		m.Log.Error("delete expired sessions", "err", err)
	} else if sessions > 0 {
		m.Log.Info("deleted expired sessions", "count", sessions)
	}
	if tokens, err := m.Store.DeleteExpiredTokens(ctx); err != nil {
		m.Log.Error("delete expired tokens", "err", err)
	} else if tokens > 0 {
		m.Log.Info("deleted expired tokens", "count", tokens)
	}
}

func (m *Maintenance) retryDeletes(ctx context.Context) {
	pending, err := m.Store.PendingDeletes(ctx, 20)
	if err != nil {
		m.Log.Error("load pending deletes", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	if m.Buckets == nil {
		m.Log.Error("pending deletes need a bucket resolver")
		return
	}
	for _, item := range pending {
		objects := m.Buckets.Default()
		if item.BucketID != "" {
			objects, err = m.Buckets.Resolve(ctx, item.BucketID)
			if err != nil {
				m.Log.Warn("pending delete bucket resolve failed", "key", item.ObjectKey, "bucket", item.BucketID, "err", err)
				if markErr := m.Store.MarkPendingDeleteFailure(ctx, item.ObjectKey, item.BucketID, err.Error()); markErr != nil {
					m.Log.Error("record pending delete failure", "key", item.ObjectKey, "err", markErr)
				}
				continue
			}
		}
		if err := objects.Delete(ctx, item.ObjectKey); err != nil {
			m.Log.Warn("pending delete retry failed", "key", item.ObjectKey, "err", err)
			if markErr := m.Store.MarkPendingDeleteFailure(ctx, item.ObjectKey, item.BucketID, err.Error()); markErr != nil {
				m.Log.Error("record pending delete failure", "key", item.ObjectKey, "err", markErr)
			}
			continue
		}
		if err := m.Store.RemovePendingDelete(ctx, item.ObjectKey, item.BucketID); err != nil {
			m.Log.Error("remove pending delete", "key", item.ObjectKey, "err", err)
		}
	}
}

func (m *Maintenance) purgeCDN(ctx context.Context) {
	if m.Purger == nil || !m.Purger.Enabled() {
		return
	}
	pending, err := m.Store.PendingCDNPurges(ctx, 30)
	if err != nil {
		m.Log.Error("load cdn purges", "err", err)
		return
	}
	eligible := pending[:0]
	for _, p := range pending {
		if p.Attempts >= 10 {
			continue
		}
		eligible = append(eligible, p)
	}
	if len(eligible) == 0 {
		return
	}
	markFailures := func(cause error) {
		for _, p := range eligible {
			m.Log.Warn("cdn purge failed", "url", p.URL, "attempts", p.Attempts+1, "err", cause)
			if err := m.Store.MarkCDNPurgeFailure(ctx, p.URL, cause.Error()); err != nil {
				m.Log.Error("record cdn purge failure", "err", err)
			}
		}
	}

	if batch, ok := m.Purger.(cdn.BatchPurger); ok {
		urls := make([]string, len(eligible))
		for i, p := range eligible {
			urls[i] = p.URL
		}
		if err := batch.PurgeBatch(ctx, urls); err != nil {
			markFailures(err)
			return
		}
		for _, p := range eligible {
			if err := m.Store.DeleteCDNPurge(ctx, p.URL); err != nil {
				m.Log.Error("remove cdn purge", "url", p.URL, "err", err)
			}
		}
		return
	}

	for _, p := range eligible {
		if err := m.Purger.Purge(ctx, p.URL); err != nil {
			m.Log.Warn("cdn purge failed", "url", p.URL, "attempts", p.Attempts+1, "err", err)
			if err := m.Store.MarkCDNPurgeFailure(ctx, p.URL, err.Error()); err != nil {
				m.Log.Error("record cdn purge failure", "err", err)
			}
			continue
		}
		if err := m.Store.DeleteCDNPurge(ctx, p.URL); err != nil {
			m.Log.Error("remove cdn purge", "url", p.URL, "err", err)
		}
	}
}
