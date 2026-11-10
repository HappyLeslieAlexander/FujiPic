// Package store contains every SQL query used by the application. Keeping
// SQL in one package makes it easy to move to Postgres later.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"

	"fujipic/internal/model"
)

// NormalizeEmail canonicalizes CLI/bootstrap and web registration emails by
// trimming whitespace, lowercasing and accepting one mailbox address.
func NormalizeEmail(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if addr, err := mail.ParseAddress(trimmed); err == nil && addr.Address != "" {
		trimmed = addr.Address
	}
	return strings.ToLower(strings.TrimSpace(trimmed))
}

var (
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("conflict")
	ErrQuotaExceeded      = errors.New("quota exceeded")
	ErrInviteInvalid      = errors.New("invite invalid")
	ErrLastAdmin          = errors.New("cannot disable the last active admin")
	ErrTokenLimitExceeded = errors.New("token limit exceeded")
	ErrBucketInUse        = errors.New("storage bucket still in use")
	ErrBucketCapacity     = errors.New("storage bucket capacity exhausted")
	ErrDefaultBucket      = errors.New("default storage bucket cannot be removed")
)

const MaxActiveTokensPerUser = 50

type Store struct {
	db *sql.DB

	// In-memory write throttles for identity activity updates. These maps
	// merely suppress duplicate SQL writes from ordinary reads.
	touchMu          sync.Mutex
	lastSessionTouch map[string]int64
	lastTokenTouch   map[string]int64
}

func New(db *sql.DB) *Store {
	return &Store{
		db:               db,
		lastSessionTouch: make(map[string]int64),
		lastTokenTouch:   make(map[string]int64),
	}
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func now() int64 { return time.Now().Unix() }

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

func (s *Store) CreateUser(ctx context.Context, u *model.User) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users(id, email, password_hash, role, status, quota_bytes, used_bytes, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.PasswordHash, u.Role, u.Status, u.QuotaBytes, u.UsedBytes, u.CreatedAt, u.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// CreateUserWithInvite creates a user and consumes an invite in one
// transaction. An empty code is allowed only when open registration is on.
func (s *Store) CreateUserWithInvite(ctx context.Context, u *model.User, inviteCode string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if inviteCode != "" {
		var used sql.NullString
		var expires sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT used_by, expires_at FROM invites WHERE code = ?`, inviteCode).Scan(&used, &expires)
		if isNoRows(err) {
			return ErrInviteInvalid
		}
		if err != nil {
			return err
		}
		if used.Valid || (expires.Valid && expires.Int64 <= now()) {
			return ErrInviteInvalid
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO users(id, email, password_hash, role, status, quota_bytes, used_bytes, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.PasswordHash, u.Role, u.Status, u.QuotaBytes, u.UsedBytes, u.CreatedAt, u.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return fmt.Errorf("create user: %w", err)
	}
	if inviteCode != "" {
		res, err := tx.ExecContext(ctx,
			`UPDATE invites SET used_by = ? WHERE code = ? AND used_by IS NULL`, u.ID, inviteCode)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrInviteInvalid
		}
	}
	return tx.Commit()
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*model.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, `
		SELECT id, email, password_hash, role, status, quota_bytes, used_bytes, created_at, updated_at
		FROM users WHERE email = ? COLLATE NOCASE`, email))
}

func (s *Store) UserByID(ctx context.Context, id string) (*model.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, `
		SELECT id, email, password_hash, role, status, quota_bytes, used_bytes, created_at, updated_at
		FROM users WHERE id = ?`, id))
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanUser(row rowScanner) (*model.User, error) {
	var u model.User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Status,
		&u.QuotaBytes, &u.UsedBytes, &u.CreatedAt, &u.UpdatedAt)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) UpdatePassword(ctx context.Context, userID, hash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, now(), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdatePasswordAndRevokeCredentials changes a password and removes all API
// tokens and other web sessions in one transaction. keepSessionID lets a
// web password change preserve the current browser session.
func (s *Store) UpdatePasswordAndRevokeCredentials(ctx context.Context, userID, hash, keepSessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, now(), userID)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id <> ?`, userID, keepSessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateUserStatus(ctx context.Context, userID, status string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var role, current string
	if err := tx.QueryRowContext(ctx,
		`SELECT role, status FROM users WHERE id = ?`, userID).Scan(&role, &current); err != nil {
		if isNoRows(err) {
			return ErrNotFound
		}
		return err
	}
	if role == "admin" && current == "active" && status == "disabled" {
		var admins int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&admins); err != nil {
			return err
		}
		if admins <= 1 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET status = ?, updated_at = ? WHERE id = ?`, status, now(), userID); err != nil {
		return err
	}
	if status == "disabled" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ?`, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateUserQuota(ctx context.Context, userID string, quota int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET quota_bytes = ?, updated_at = ? WHERE id = ?`, quota, now(), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReserveQuota atomically reserves bytes for a pending upload.
func (s *Store) ReserveQuota(ctx context.Context, userID string, size int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET used_bytes = used_bytes + ?, updated_at = ?
		WHERE id = ? AND status = 'active' AND used_bytes + ? <= quota_bytes`,
		size, now(), userID, size)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrQuotaExceeded
	}
	return nil
}

func (s *Store) ReleaseQuota(ctx context.Context, userID string, size int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users SET used_bytes = MAX(0, used_bytes - ?), updated_at = ? WHERE id = ?`,
		size, now(), userID)
	return err
}

type UserWithCount struct {
	model.User
	ImageCount int
}

func (s *Store) ListUsers(ctx context.Context, limit int) ([]UserWithCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.email, u.role, u.status, u.quota_bytes, u.used_bytes, u.created_at,
		       (SELECT COUNT(*) FROM images i WHERE i.user_id = u.id AND i.deleted_at IS NULL)
		FROM users u ORDER BY u.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserWithCount
	for rows.Next() {
		var u UserWithCount
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.Status, &u.QuotaBytes,
			&u.UsedBytes, &u.CreatedAt, &u.ImageCount); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CreateSession(ctx context.Context, sess *model.Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, user_id, token_hash, ip, user_agent, created_at, last_seen_at, expires_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.UserID, sess.TokenHash, sess.IP, sess.UserAgent,
		sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (s *Store) SessionUser(ctx context.Context, tokenHash string) (*model.User, string, error) {
	var u model.User
	var sessionID string
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.password_hash, u.role, u.status, u.quota_bytes, u.used_bytes,
		       u.created_at, u.updated_at, s.id
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, now()).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Status, &u.QuotaBytes,
			&u.UsedBytes, &u.CreatedAt, &u.UpdatedAt, &sessionID)
	if isNoRows(err) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if !u.IsActive() {
		return nil, "", ErrNotFound
	}
	return &u, sessionID, nil
}

func (s *Store) TouchSession(ctx context.Context, sessionID string) {
	current := now()
	s.touchMu.Lock()
	last := s.lastSessionTouch[sessionID]
	s.lastSessionTouch[sessionID] = current
	s.touchMu.Unlock()
	if last != 0 && current-last < 300 {
		return
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, current, sessionID)
}

func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
	return err
}

func (s *Store) DeleteUserSessionsExcept(ctx context.Context, userID, keepSessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND id <> ?`, userID, keepSessionID)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) CreateToken(ctx context.Context, t *model.APIToken) error {
	// Enforce the active-token cap in the same statement: only unexpired
	// rows count, and a concurrent insert cannot squeeze an extra row in.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO api_tokens(id, user_id, name, token_hash, token_prefix, scopes, created_at, expires_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?
		WHERE (
			SELECT COUNT(*) FROM api_tokens
			WHERE user_id = ? AND (expires_at IS NULL OR expires_at > ?)
		) < ?`,
		t.ID, t.UserID, t.Name, t.TokenHash, t.TokenPrefix, t.Scopes, t.CreatedAt, t.ExpiresAt,
		t.UserID, now(), MaxActiveTokensPerUser)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return fmt.Errorf("create token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTokenLimitExceeded
	}
	return nil
}

func (s *Store) TokenUser(ctx context.Context, tokenHash string) (*model.User, *model.APIToken, error) {
	var u model.User
	var t model.APIToken
	var expires sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.password_hash, u.role, u.status, u.quota_bytes, u.used_bytes,
		       u.created_at, u.updated_at,
		       t.id, t.user_id, t.name, t.token_hash, t.token_prefix, t.scopes, t.created_at, t.expires_at
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ?`, tokenHash).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Status, &u.QuotaBytes,
			&u.UsedBytes, &u.CreatedAt, &u.UpdatedAt,
			&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.TokenPrefix, &t.Scopes, &t.CreatedAt, &expires)
	if isNoRows(err) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if expires.Valid {
		t.ExpiresAt = &expires.Int64
		if expires.Int64 <= now() {
			return nil, nil, ErrNotFound
		}
	}
	if !u.IsActive() {
		return nil, nil, ErrNotFound
	}
	return &u, &t, nil
}

func (s *Store) TouchToken(ctx context.Context, tokenID string) {
	current := now()
	s.touchMu.Lock()
	last := s.lastTokenTouch[tokenID]
	s.lastTokenTouch[tokenID] = current
	s.touchMu.Unlock()
	if last != 0 && current-last < 300 {
		return
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, current, tokenID)
}

func (s *Store) DeleteExpiredTokens(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE expires_at IS NOT NULL AND expires_at <= ?`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) ListTokens(ctx context.Context, userID string) ([]model.APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, token_prefix, scopes, created_at, last_used_at, expires_at
		FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC LIMIT 100`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.APIToken
	for rows.Next() {
		var t model.APIToken
		var lastUsed, expires sql.NullInt64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenPrefix, &t.Scopes,
			&t.CreatedAt, &lastUsed, &expires); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			t.LastUsedAt = &lastUsed.Int64
		}
		if expires.Valid {
			t.ExpiresAt = &expires.Int64
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(ctx context.Context, userID, tokenID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, tokenID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) InsertImage(ctx context.Context, img *model.Image) error {
	var bucketID any
	if img.BucketID != "" {
		bucketID = img.BucketID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO images(id, user_id, bucket_id, object_key, original_name, mime, size_bytes, width, height,
		                   sha256, visibility, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		img.ID, img.UserID, bucketID, img.ObjectKey, img.OriginalName, img.MIME, img.SizeBytes,
		img.Width, img.Height, img.SHA256, img.Visibility, img.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return fmt.Errorf("insert image: %w", err)
	}
	return nil
}

func (s *Store) ImageBySHA(ctx context.Context, userID, sha string) (*model.Image, error) {
	return s.scanImage(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(bucket_id, ''), object_key, COALESCE(original_name, ''), mime, size_bytes, width, height,
		       sha256, visibility, created_at, deleted_at
		FROM images WHERE user_id = ? AND sha256 = ? AND deleted_at IS NULL`, userID, sha))
}

func (s *Store) ImageByID(ctx context.Context, userID, imageID string) (*model.Image, error) {
	return s.scanImage(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(bucket_id, ''), object_key, COALESCE(original_name, ''), mime, size_bytes, width, height,
		       sha256, visibility, created_at, deleted_at
		FROM images WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, imageID, userID))
}

func (s *Store) ImageByObjectKey(ctx context.Context, objectKey string) (*model.Image, error) {
	return s.scanImage(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(bucket_id, ''), object_key, COALESCE(original_name, ''), mime, size_bytes, width, height,
		       sha256, visibility, created_at, deleted_at
		FROM images WHERE object_key = ? AND deleted_at IS NULL`, objectKey))
}

func (s *Store) scanImage(row rowScanner) (*model.Image, error) {
	var img model.Image
	var deleted sql.NullInt64
	err := row.Scan(&img.ID, &img.UserID, &img.BucketID, &img.ObjectKey, &img.OriginalName, &img.MIME,
		&img.SizeBytes, &img.Width, &img.Height, &img.SHA256, &img.Visibility,
		&img.CreatedAt, &deleted)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if deleted.Valid {
		img.DeletedAt = &deleted.Int64
	}
	return &img, nil
}

func (s *Store) ListImages(ctx context.Context, userID string, limit int, beforeCreated int64, beforeID string) ([]model.Image, error) {
	query := `
		SELECT id, user_id, COALESCE(bucket_id, ''), object_key, COALESCE(original_name, ''), mime, size_bytes, width, height,
		       sha256, visibility, created_at, deleted_at
		FROM images WHERE user_id = ? AND deleted_at IS NULL`
	args := []any{userID}
	if beforeCreated > 0 {
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, beforeCreated, beforeCreated, beforeID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Image
	for rows.Next() {
		var img model.Image
		var deleted sql.NullInt64
		if err := rows.Scan(&img.ID, &img.UserID, &img.BucketID, &img.ObjectKey, &img.OriginalName, &img.MIME,
			&img.SizeBytes, &img.Width, &img.Height, &img.SHA256, &img.Visibility,
			&img.CreatedAt, &deleted); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// SoftDeleteImage marks the image deleted, releases quota and returns the
// image that must be removed from the bucket. It is idempotent.
func (s *Store) SoftDeleteImage(ctx context.Context, userID, imageID string) (*model.Image, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var img model.Image
	var deleted sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(bucket_id, ''), object_key, COALESCE(original_name, ''), mime, size_bytes, width, height,
		       sha256, visibility, created_at, deleted_at
		FROM images WHERE id = ? AND user_id = ?`, imageID, userID).
		Scan(&img.ID, &img.UserID, &img.BucketID, &img.ObjectKey, &img.OriginalName, &img.MIME,
			&img.SizeBytes, &img.Width, &img.Height, &img.SHA256, &img.Visibility,
			&img.CreatedAt, &deleted)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if deleted.Valid {
		return &img, nil
	}
	ts := now()
	res, err := tx.ExecContext(ctx,
		`UPDATE images SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, ts, imageID)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		// A racing transaction soft-deleted this image without observing
		// the `deleted_at` value in this transaction's snapshot.
		return &img, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET used_bytes = MAX(0, used_bytes - ?), updated_at = ? WHERE id = ?`,
		img.SizeBytes, ts, userID); err != nil {
		return nil, err
	}
	bucketID := img.BucketID
	if bucketID == "" {
		bucketID = model.DefaultBucketID
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE storage_buckets SET used_bytes = MAX(0, used_bytes - ?), updated_at = ?
		WHERE id = ?`, img.SizeBytes, ts, bucketID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	img.DeletedAt = &ts
	return &img, nil
}

func (s *Store) CountUserImages(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM images WHERE user_id = ? AND deleted_at IS NULL`, userID).Scan(&n)
	return n, err
}

func (s *Store) CreateInvite(ctx context.Context, inv *model.Invite) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO invites(code, created_by, created_at, expires_at) VALUES(?, ?, ?, ?)`,
		inv.Code, inv.CreatedBy, inv.CreatedAt, inv.ExpiresAt)
	return err
}

// ValidateInvite reports whether an invite code is currently available
// without consuming it. It supports cheap registration pre-validation so
// deliberately malformed invite codes do not trigger an Argon2id hash.
func (s *Store) ValidateInvite(ctx context.Context, code string) error {
	var used sql.NullString
	var expires sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT used_by, expires_at FROM invites WHERE code = ?`, code).Scan(&used, &expires)
	if isNoRows(err) {
		return ErrInviteInvalid
	}
	if err != nil {
		return err
	}
	if used.Valid || (expires.Valid && expires.Int64 <= now()) {
		return ErrInviteInvalid
	}
	return nil
}

// ConsumeInvite atomically validates and marks an invite as used.
func (s *Store) ConsumeInvite(ctx context.Context, code, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var used sql.NullString
	var expires sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT used_by, expires_at FROM invites WHERE code = ?`, code).Scan(&used, &expires)
	if isNoRows(err) {
		return ErrInviteInvalid
	}
	if err != nil {
		return err
	}
	if used.Valid {
		return ErrInviteInvalid
	}
	if expires.Valid && expires.Int64 <= now() {
		return ErrInviteInvalid
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE invites SET used_by = ? WHERE code = ? AND used_by IS NULL`, userID, code)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrInviteInvalid
	}
	return tx.Commit()
}

func (s *Store) ListInvites(ctx context.Context, limit int) ([]model.Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT code, created_by, used_by, created_at, expires_at
		FROM invites ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Invite
	for rows.Next() {
		var inv model.Invite
		var createdBy, usedBy sql.NullString
		var expires sql.NullInt64
		if err := rows.Scan(&inv.Code, &createdBy, &usedBy, &inv.CreatedAt, &expires); err != nil {
			return nil, err
		}
		if createdBy.Valid {
			inv.CreatedBy = &createdBy.String
		}
		if usedBy.Valid {
			inv.UsedBy = &usedBy.String
		}
		if expires.Valid {
			inv.ExpiresAt = &expires.Int64
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *Store) AddAudit(ctx context.Context, a *model.AuditLog) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_logs(id, actor_id, action, target, ip, meta, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ActorID, a.Action, a.Target, a.IP, a.Meta, a.CreatedAt)
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]model.AuditLog, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, actor_id, action, target, ip, meta, created_at
		FROM audit_logs ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditLog
	for rows.Next() {
		var a model.AuditLog
		var actor sql.NullString
		if err := rows.Scan(&a.ID, &actor, &a.Action, &a.Target, &a.IP, &a.Meta, &a.CreatedAt); err != nil {
			return nil, err
		}
		if actor.Valid {
			a.ActorID = &actor.String
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddCDNPurge(ctx context.Context, url string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO cdn_purges(url, created_at) VALUES(?, ?)`, url, now())
	return err
}

func (s *Store) PendingCDNPurges(ctx context.Context, limit int) ([]model.CDNPurge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT url, created_at, attempts, COALESCE(last_error, '')
		FROM cdn_purges WHERE attempts < 10 ORDER BY created_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CDNPurge
	for rows.Next() {
		var p model.CDNPurge
		if err := rows.Scan(&p.URL, &p.CreatedAt, &p.Attempts, &p.LastError); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) MarkCDNPurgeFailure(ctx context.Context, url, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE cdn_purges SET attempts = attempts + 1, last_error = ? WHERE url = ?`, lastErr, url)
	return err
}

func (s *Store) DeleteCDNPurge(ctx context.Context, url string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cdn_purges WHERE url = ?`, url)
	return err
}

func (s *Store) AddPendingDelete(ctx context.Context, objectKey, bucketID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO pending_deletes(object_key, bucket_id, created_at) VALUES(?, ?, ?)`,
		objectKey, bucketID, now())
	return err
}

func (s *Store) MarkPendingDeleteFailure(ctx context.Context, key, bucketID, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pending_deletes(object_key, bucket_id, created_at, last_error) VALUES(?, ?, ?, ?)
		ON CONFLICT(object_key, bucket_id) DO UPDATE SET attempts = attempts + 1, last_error = excluded.last_error`,
		key, bucketID, now(), lastErr)
	return err
}

func (s *Store) PendingDeletes(ctx context.Context, limit int) ([]model.PendingDelete, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT object_key, bucket_id, created_at, attempts, COALESCE(last_error, '')
		 FROM pending_deletes WHERE attempts < 10 ORDER BY created_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PendingDelete
	for rows.Next() {
		var p model.PendingDelete
		if err := rows.Scan(&p.ObjectKey, &p.BucketID, &p.CreatedAt, &p.Attempts, &p.LastError); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) RemovePendingDelete(ctx context.Context, objectKey, bucketID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_deletes WHERE object_key = ? AND bucket_id = ?`, objectKey, bucketID)
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// EnsureDefaultStorageBucket creates or refreshes the fixed row for the
// S3_* bucket. Admin-managed capacity, priority, and enabled state survive
// restarts; endpoint credentials continue to come from the environment.
func (s *Store) EnsureDefaultStorageBucket(ctx context.Context, b *model.StorageBucket) error {
	ts := now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO storage_buckets(id, name, endpoint, region, bucket, access_key_id,
		                            secret_ciphertext, force_path_style, max_bytes, used_bytes,
		                            priority, enabled, is_default, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, '', '', ?, 0, 0, ?, 1, 1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  name = excluded.name,
		  endpoint = excluded.endpoint,
		  region = excluded.region,
		  bucket = excluded.bucket,
		  force_path_style = excluded.force_path_style,
		  is_default = 1,
		  updated_at = excluded.updated_at`,
		model.DefaultBucketID, b.Name, b.Endpoint, b.Region, b.Bucket,
		boolInt(b.ForcePathStyle), b.Priority, ts, ts)
	if err != nil {
		return fmt.Errorf("ensure default storage bucket: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE images SET bucket_id = ? WHERE bucket_id IS NULL OR bucket_id = ''`,
		model.DefaultBucketID); err != nil {
		return fmt.Errorf("normalize image bucket ids: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE pending_deletes SET bucket_id = ? WHERE bucket_id = ''`,
		model.DefaultBucketID); err != nil {
		return fmt.Errorf("normalize pending delete bucket ids: %w", err)
	}
	return nil
}

// ReconcileStorageBucketUsage repairs the cached per-bucket byte counters from
// the active image rows. It runs at startup before uploads are accepted.
func (s *Store) ReconcileStorageBucketUsage(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE storage_buckets
		SET used_bytes = COALESCE((
		  SELECT SUM(i.size_bytes) FROM images i
		  WHERE i.deleted_at IS NULL
		    AND (i.bucket_id = storage_buckets.id
		         OR (storage_buckets.is_default = 1 AND (i.bucket_id IS NULL OR i.bucket_id = '')))
		), 0),
		updated_at = ?`, now())
	if err != nil {
		return fmt.Errorf("reconcile storage bucket usage: %w", err)
	}
	return nil
}

// CreateStorageBucket stores an administrator-configured bucket. The caller
// encrypts the secret access key before calling this method.
func (s *Store) CreateStorageBucket(ctx context.Context, b *model.StorageBucket) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO storage_buckets(id, name, endpoint, region, bucket, access_key_id,
		                            secret_ciphertext, force_path_style, max_bytes, used_bytes,
		                            priority, enabled, is_default, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 0, ?, ?)`,
		b.ID, b.Name, b.Endpoint, b.Region, b.Bucket, b.AccessKeyID,
		b.SecretCiphertext, boolInt(b.ForcePathStyle), b.MaxBytes, b.Priority,
		boolInt(b.Enabled), b.CreatedAt, b.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return fmt.Errorf("create storage bucket: %w", err)
	}
	return nil
}

func (s *Store) StorageBucketByID(ctx context.Context, bucketID string) (*model.StorageBucket, error) {
	return s.scanStorageBucket(s.db.QueryRowContext(ctx, `
		SELECT id, name, endpoint, region, bucket, access_key_id, secret_ciphertext,
		       force_path_style, max_bytes, used_bytes, priority, enabled, is_default,
		       created_at, updated_at
		FROM storage_buckets WHERE id = ?`, bucketID))
}

// ListStorageBuckets returns the global pool in selection order. Secrets are
// never included in results used by templates.
func (s *Store) ListStorageBuckets(ctx context.Context) ([]model.StorageBucket, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, endpoint, region, bucket, access_key_id, secret_ciphertext,
		       force_path_style, max_bytes, used_bytes, priority, enabled, is_default,
		       created_at, updated_at
		FROM storage_buckets ORDER BY priority ASC, created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.StorageBucket
	for rows.Next() {
		var b model.StorageBucket
		var forcePathStyle, enabled, isDefault int
		if err := rows.Scan(&b.ID, &b.Name, &b.Endpoint, &b.Region, &b.Bucket,
			&b.AccessKeyID, &b.SecretCiphertext, &forcePathStyle, &b.MaxBytes, &b.UsedBytes,
			&b.Priority, &enabled, &isDefault, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.ForcePathStyle = forcePathStyle != 0
		b.Enabled = enabled != 0
		b.IsDefault = isDefault != 0
		b.SecretCiphertext = ""
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) scanStorageBucket(row rowScanner) (*model.StorageBucket, error) {
	var b model.StorageBucket
	var forcePathStyle, enabled, isDefault int
	err := row.Scan(&b.ID, &b.Name, &b.Endpoint, &b.Region, &b.Bucket,
		&b.AccessKeyID, &b.SecretCiphertext, &forcePathStyle, &b.MaxBytes, &b.UsedBytes,
		&b.Priority, &enabled, &isDefault, &b.CreatedAt, &b.UpdatedAt)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.ForcePathStyle = forcePathStyle != 0
	b.Enabled = enabled != 0
	b.IsDefault = isDefault != 0
	return &b, nil
}

// UpdateStorageBucket changes pool settings without touching credentials.
func (s *Store) UpdateStorageBucket(ctx context.Context, b *model.StorageBucket) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE storage_buckets
		SET name = ?, max_bytes = ?, priority = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		b.Name, b.MaxBytes, b.Priority, boolInt(b.Enabled), now(), b.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return fmt.Errorf("update storage bucket: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReserveStorageBucket atomically reserves size bytes in the first enabled
// bucket with free capacity, ordered by priority. A max_bytes of zero means
// unlimited. It returns the full row so the caller can build a client.
func (s *Store) ReserveStorageBucket(ctx context.Context, size int64) (*model.StorageBucket, error) {
	if size < 0 {
		return nil, fmt.Errorf("reserve storage bucket: negative size")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM storage_buckets
		WHERE enabled = 1 AND (max_bytes = 0 OR used_bytes + ? <= max_bytes)
		ORDER BY priority ASC, created_at ASC, id ASC`, size)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, id := range ids {
		res, err := s.db.ExecContext(ctx, `
			UPDATE storage_buckets SET used_bytes = used_bytes + ?, updated_at = ?
			WHERE id = ? AND enabled = 1 AND (max_bytes = 0 OR used_bytes + ? <= max_bytes)`,
			size, now(), id, size)
		if err != nil {
			return nil, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			continue // another upload filled it first
		}
		return s.StorageBucketByID(ctx, id)
	}
	return nil, ErrBucketCapacity
}

func (s *Store) ReleaseStorageBucket(ctx context.Context, bucketID string, size int64) error {
	if bucketID == "" {
		bucketID = model.DefaultBucketID
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE storage_buckets SET used_bytes = MAX(0, used_bytes - ?), updated_at = ?
		WHERE id = ?`, size, now(), bucketID)
	return err
}

// DeleteStorageBucket removes an empty non-default bucket registration.
// Soft-deleted images are detached first; active images, queued object
// deletes, or in-flight capacity reservations block removal so no live data
// is orphaned.
func (s *Store) DeleteStorageBucket(ctx context.Context, bucketID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Detach soft-deleted images first. This write also acquires SQLite's
	// write lock, so the checks below cannot race with an upload that is
	// reserving capacity for this bucket.
	if _, err := tx.ExecContext(ctx,
		`UPDATE images SET bucket_id = NULL WHERE bucket_id = ? AND deleted_at IS NOT NULL`, bucketID); err != nil {
		return err
	}
	var isDefault int
	var usedBytes int64
	err = tx.QueryRowContext(ctx,
		`SELECT is_default, used_bytes FROM storage_buckets WHERE id = ?`, bucketID).
		Scan(&isDefault, &usedBytes)
	if isNoRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if isDefault != 0 {
		return ErrDefaultBucket
	}
	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM images WHERE bucket_id = ? AND deleted_at IS NULL`, bucketID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrBucketInUse
	}
	var pending int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_deletes WHERE bucket_id = ?`, bucketID).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return ErrBucketInUse
	}
	// A positive counter with no active images means an upload reserved
	// capacity but has not inserted its image row yet.
	if usedBytes > 0 {
		return ErrBucketInUse
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM storage_buckets WHERE id = ?`, bucketID); err != nil {
		return err
	}
	return tx.Commit()
}

type Stats struct {
	Users        int
	Images       int
	TotalBytes   int64
	TodayUploads int
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	startOfDay := time.Now().UTC().Truncate(24 * time.Hour).Unix()
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM users),
		  (SELECT COUNT(*) FROM images WHERE deleted_at IS NULL),
		  (SELECT COALESCE(SUM(size_bytes),0) FROM images WHERE deleted_at IS NULL),
		  (SELECT COUNT(*) FROM images WHERE deleted_at IS NULL AND created_at >= ?)`, startOfDay).
		Scan(&st.Users, &st.Images, &st.TotalBytes, &st.TodayUploads)
	return st, err
}
