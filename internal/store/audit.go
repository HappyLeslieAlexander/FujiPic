package store

import "context"

// ClearAudit removes the current audit history in one atomic statement.
// New activity can append records immediately after this statement completes.
func (s *Store) ClearAudit(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM audit_logs`)
	return err
}
