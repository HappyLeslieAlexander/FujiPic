package store

import (
	"context"

	"fujipic/internal/model"
)

// ListUserSessions returns active sessions without their authentication hashes.
func (s *Store) ListUserSessions(ctx context.Context, userID string) ([]model.Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, ip, user_agent, created_at, last_seen_at, expires_at
		FROM sessions WHERE user_id = ? AND expires_at > ?
		ORDER BY last_seen_at DESC, created_at DESC, id`, userID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []model.Session
	for rows.Next() {
		var sess model.Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.IP, &sess.UserAgent, &sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// DeleteOtherUserSession scopes deletion to the owner and preserves this login.
func (s *Store) DeleteOtherUserSession(ctx context.Context, userID, sessionID, currentSessionID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id = ? AND id <> ?`, userID, sessionID, currentSessionID)
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
