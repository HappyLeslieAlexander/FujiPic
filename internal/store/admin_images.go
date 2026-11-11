package store

import (
	"context"

	"fujipic/internal/model"
)

type AdminImage struct {
	model.Image
	OwnerEmail string
}

// ListAdminImages is only for the authenticated admin console. User-facing
// lists must continue using ListImages, which always filters by owner.
func (s *Store) ListAdminImages(ctx context.Context, limit, offset int) ([]AdminImage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.user_id, COALESCE(i.bucket_id, ''), i.object_key,
		       COALESCE(i.original_name, ''), i.mime, i.size_bytes, i.width, i.height,
		       i.sha256, i.visibility, i.created_at, u.email
		FROM images i JOIN users u ON u.id = i.user_id
		WHERE i.deleted_at IS NULL
		ORDER BY i.created_at DESC, i.id DESC LIMIT ? OFFSET ?`, limit, offset)
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

// AdminImageByID includes deleted rows so repeated admin deletions remain
// idempotent through the normal transactional deletion pipeline.
func (s *Store) AdminImageByID(ctx context.Context, imageID string) (*model.Image, error) {
	return s.scanImage(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(bucket_id, ''), object_key, COALESCE(original_name, ''),
		       mime, size_bytes, width, height, sha256, visibility, created_at, deleted_at
		FROM images WHERE id = ?`, imageID))
}
