package store

import "context"

// DeleteInvite removes an invitation regardless of usage or expiry. Existing
// users are not changed; registration consumes invitations in its transaction.
func (s *Store) DeleteInvite(ctx context.Context, code string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM invites WHERE code = ?`, code)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
