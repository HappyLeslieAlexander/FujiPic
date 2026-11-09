// Package worker runs background maintenance: bucket deletion retries and
// CDN cache purges.
package worker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"fujipic/internal/cdn"
	"fujipic/internal/storage"
	"fujipic/internal/store"
)

type Maintenance struct {
	Store    *store.Store
	Objects  storage.Store
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
	entries, err := filepath.Glob(filepath.Join(m.TempDir, "fujipic-upload-*"))
	if err != nil {
		m.Log.Error("list temp upload dir", "err", err)
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, path := range entries {
		info, err := os.Stat(path)
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
	keys, err := m.Store.PendingDeletes(ctx, 20)
	if err != nil {
		m.Log.Error("load pending deletes", "err", err)
		return
	}
	for _, key := range keys {
		if err := m.Objects.Delete(ctx, key); err != nil {
			m.Log.Warn("pending delete retry failed", "key", key, "err", err)
			if markErr := m.Store.MarkPendingDeleteFailure(ctx, key, err.Error()); markErr != nil {
				m.Log.Error("record pending delete failure", "key", key, "err", markErr)
			}
			continue
		}
		if err := m.Store.RemovePendingDelete(ctx, key); err != nil {
			m.Log.Error("remove pending delete", "key", key, "err", err)
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
