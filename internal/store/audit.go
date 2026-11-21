package store

import (
	"context"
	"fmt"
	"fujipic/internal/id"
)

// ClearAudit atomically replaces history with an attributable clear record.
func (s *Store) ClearAudit(ctx context.Context, actorID, ip string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM audit_logs`)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(id,actor_id,action,target,ip,meta,created_at) VALUES(?,?,'admin.audit_clear','audit_logs',?,?,?)`, id.New("aud"), actorID, ip, fmt.Sprintf("deleted=%d", n), now()); err != nil {
		return err
	}
	return tx.Commit()
}
