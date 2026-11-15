package store

import "context"

// Retain audit history for 90 days and tombstones for 30 days. Limit each sweep
// to 500 rows; never age out a failed storage deletion or CDN purge obligation.
func (s *Store) PruneHistory(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_logs WHERE id IN (SELECT id FROM audit_logs WHERE created_at<? ORDER BY created_at LIMIT 500)`, now()-90*86400); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE id IN (
	 SELECT i.id FROM images i WHERE i.deleted_at<?
	 AND NOT EXISTS(SELECT 1 FROM media_jobs WHERE image_id=i.id)
	 AND NOT EXISTS(SELECT 1 FROM pending_deletes WHERE object_key=i.object_key)
	 AND NOT EXISTS(SELECT 1 FROM cdn_purges)
	 ORDER BY i.deleted_at LIMIT 500)`, now()-30*86400)
	return err
}
