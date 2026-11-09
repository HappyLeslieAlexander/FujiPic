// Package worker runs background maintenance: bucket deletion retries and
// CDN cache purges.
package worker

import (
	"context"
	"log/slog"
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
	pending, err := m.Store.PendingCDNPurges(ctx, 20)
	if err != nil {
		m.Log.Error("load cdn purges", "err", err)
		return
	}
	for _, p := range pending {
		if p.Attempts >= 10 {
			continue
		}
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
