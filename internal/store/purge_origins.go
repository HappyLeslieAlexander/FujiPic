package store

import (
	"context"
	"strings"
)

// ConfigurePurgeOrigins persists the trusted runtime configuration for SQL
// triggers. Call before serving or starting workers. Existing queue URLs remain
// intact when configuration changes; disabling purge stops creating new work.
func (s *Store) ConfigurePurgeOrigins(ctx context.Context, bases []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM cdn_purge_origins`); err != nil {
		return err
	}
	for _, base := range bases {
		base = strings.TrimRight(base, "/")
		if base == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO cdn_purge_origins(base_url) VALUES(?)`, base); err != nil {
			return err
		}
	}
	return tx.Commit()
}
