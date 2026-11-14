package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"fujipic/internal/id"
	"fujipic/internal/model"
)

type AdminImage struct {
	model.Image
	OwnerEmail string
}

// Empty dimensions mean all; values within each dimension are ORed and the
// dimensions are ANDed. Empty/NULL legacy bucket IDs belong to bkt_default.
type AdminImageFilter struct {
	UserIDs   []string
	BucketIDs []string
}

func (f AdminImageFilter) Active() bool { return len(f.UserIDs) > 0 || len(f.BucketIDs) > 0 }

func adminImageWhere(f AdminImageFilter, imageIDs []string) (string, []any) {
	where := "i.deleted_at IS NULL"
	var args []any
	add := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		where += " AND " + column + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") + ")"
		for _, value := range values {
			args = append(args, value)
		}
	}
	add("i.user_id", f.UserIDs)
	add("COALESCE(NULLIF(i.bucket_id, ''), 'bkt_default')", f.BucketIDs)
	add("i.id", imageIDs)
	return where, args
}

type AdminFilterOption struct {
	ID   string
	Name string
}

func (s *Store) AdminImageUsers(ctx context.Context) ([]AdminFilterOption, error) {
	return s.SearchAdminImageUsers(ctx, "", nil)
}

// Include checked options even when they fall outside the current search page.
func (s *Store) SearchAdminImageUsers(ctx context.Context, search string, selected []string) ([]AdminFilterOption, error) {
	query := `SELECT id, email FROM users WHERE id IN (SELECT id FROM users WHERE instr(lower(email), lower(?)) > 0 ORDER BY created_at DESC,id DESC LIMIT 20)`
	args := []any{search}
	if len(selected) > 0 {
		query += ` OR id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(selected)), ",") + `)`
		for _, value := range selected {
			args = append(args, value)
		}
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at DESC,id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []AdminFilterOption
	for rows.Next() {
		var user AdminFilterOption
		if err := rows.Scan(&user.ID, &user.Name); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) ValidateAdminImageUsers(ctx context.Context, values []string) ([]string, error) {
	if len(values) > 1000 {
		return nil, ErrInvalidImageSelection
	}
	if len(values) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var args []any
	var ids []string
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			ids = append(ids, v)
			args = append(args, v)
		}
	}
	var matched, total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...).Scan(&matched); err != nil {
		return nil, err
	}
	if matched != len(ids) {
		return nil, ErrInvalidImageSelection
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&total); err != nil {
		return nil, err
	}
	if matched == total {
		return nil, nil
	}
	return ids, nil
}

// ListAdminImages is only for the authenticated admin console. User-facing
// lists must continue using ListImages, which always filters by owner.
func (s *Store) ListAdminImages(ctx context.Context, limit, offset int) ([]AdminImage, error) {
	return s.ListFilteredAdminImages(ctx, AdminImageFilter{}, limit, offset)
}

func (s *Store) ListFilteredAdminImages(ctx context.Context, filter AdminImageFilter, limit, offset int) ([]AdminImage, error) {
	where, args := adminImageWhere(filter, nil)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.user_id, COALESCE(i.bucket_id, ''), i.object_key,
		       COALESCE(i.original_name, ''), i.mime, i.size_bytes, COALESCE(i.width, 0), COALESCE(i.height, 0),
		       i.sha256, i.visibility, i.created_at, u.email
		FROM images i JOIN users u ON u.id = i.user_id
		WHERE `+where+`
		ORDER BY i.created_at DESC, i.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminImage
	for rows.Next() {
		var img AdminImage
		if err := rows.Scan(&img.ID, &img.UserID, &img.BucketID, &img.ObjectKey, &img.OriginalName,
			&img.MIME, &img.SizeBytes, &img.Width, &img.Height, &img.SHA256, &img.Visibility,
			&img.CreatedAt, &img.OwnerEmail); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

type adminImageQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// The fingerprint fixes the visible result set: if uploads/deletions change
// it before confirmation, the administrator must review the new result count.
type adminImageID struct{ ID, UserID string }

func adminImageSelection(ctx context.Context, q adminImageQuerier, f AdminImageFilter, imageIDs []string, retain bool) ([]adminImageID, int, string, error) {
	where, args := adminImageWhere(f, imageIDs)
	rows, err := q.QueryContext(ctx, `SELECT i.id, i.user_id FROM images i WHERE `+where+` ORDER BY i.id`, args...)
	if err != nil {
		return nil, 0, "", err
	}
	defer rows.Close()
	hash := sha256.New()
	var images []adminImageID
	count := 0
	for rows.Next() {
		var img adminImageID
		if err := rows.Scan(&img.ID, &img.UserID); err != nil {
			return nil, 0, "", err
		}
		fmt.Fprintf(hash, "%d:%s", len(img.ID), img.ID)
		count++
		if retain {
			images = append(images, img)
		}
	}
	return images, count, hex.EncodeToString(hash.Sum(nil)), rows.Err()
}

func (s *Store) AdminImageSummary(ctx context.Context, f AdminImageFilter) (int, string, error) {
	where, args := adminImageWhere(f, nil)
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM images i WHERE `+where, args...).Scan(&count)
	if err != nil || !f.Active() || count > MaxAdminDeleteImages {
		return count, "", err
	}
	_, count, fingerprint, err := adminImageSelection(ctx, s.db, f, nil, false)
	return count, fingerprint, err
}

var (
	ErrImageSelectionTooLarge = errors.New("image selection exceeds atomic batch limit")
	ErrAdminRequired          = errors.New("active administrator required")
	ErrInvalidImageSelection  = errors.New("invalid image selection")
	ErrImageSelectionChanged  = errors.New("image selection changed")
)

const MaxAdminDeleteImages = 1000

// QueueAdminImageDeletion atomically deletes metadata and queues all external
// work. No cloud I/O takes place inside the transaction. Explicit selections
// must all exist and match the filter; there is no silent partial deletion.
func (s *Store) QueueAdminImageDeletion(ctx context.Context, actorID, ip string, f AdminImageFilter, imageIDs []string, all bool, fingerprint string, publicBases ...string) (int, error) {
	if len(f.UserIDs) > 1000 || len(f.BucketIDs) > 1000 || (all && (!f.Active() || len(imageIDs) != 0 || fingerprint == "")) || (!all && (len(imageIDs) == 0 || len(imageIDs) > 1000)) {
		return 0, ErrInvalidImageSelection
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Acquire the write lock before checking the actor and result set.
	res, err := tx.ExecContext(ctx, `UPDATE users SET updated_at = updated_at WHERE id = ? AND role = 'admin' AND status = 'active'`, actorID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrAdminRequired
	}
	// Recheck the all-values == no-filter invariant under the same lock.
	if all {
		coversAll := func(table string, ids []string) (bool, error) {
			if len(ids) == 0 {
				return true, nil
			}
			args := make([]any, len(ids))
			for i := range ids {
				args[i] = ids[i]
			}
			var count int
			err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id NOT IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...).Scan(&count)
			return count == 0, err
		}
		usersAll, err := coversAll("users", f.UserIDs)
		if err != nil {
			return 0, err
		}
		bucketsAll, err := coversAll("storage_buckets", f.BucketIDs)
		if err != nil {
			return 0, err
		}
		if usersAll && bucketsAll {
			return 0, ErrInvalidImageSelection
		}
		where, args := adminImageWhere(f, nil)
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM images i WHERE `+where+` LIMIT ?)`, append(args, MaxAdminDeleteImages+1)...).Scan(&count); err != nil {
			return 0, err
		}
		if count > MaxAdminDeleteImages {
			return 0, ErrImageSelectionTooLarge
		}
	}
	images, _, actualFingerprint, err := adminImageSelection(ctx, tx, f, imageIDs, true)
	if err != nil {
		return 0, err
	}
	if all && actualFingerprint != fingerprint {
		return 0, ErrImageSelectionChanged
	}
	if !all && len(images) != len(imageIDs) {
		return 0, ErrInvalidImageSelection
	}
	for _, img := range images {
		deleted, err := softDeleteImageTx(ctx, tx, img.UserID, img.ID, publicBases...)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(id, actor_id, action, target, ip, meta, created_at) VALUES(?, ?, 'admin.image_delete', ?, ?, ?, ?)`, id.New("aud"), actorID, img.ID, ip, deleted.ObjectKey, now()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(images), nil
}

// AdminImageByID includes deleted rows so repeated admin deletions remain
// idempotent through the normal transactional deletion pipeline.
func (s *Store) AdminImageByID(ctx context.Context, imageID string) (*model.Image, error) {
	return s.scanImage(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(bucket_id, ''), object_key, COALESCE(original_name, ''),
		       mime, size_bytes, COALESCE(width, 0), COALESCE(height, 0), sha256, visibility, created_at, deleted_at
		FROM images WHERE id = ?`, imageID))
}
