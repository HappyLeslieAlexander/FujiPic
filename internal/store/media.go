package store

import (
	"context"
	"database/sql"
	"strings"

	"fujipic/internal/id"
	"fujipic/internal/model"
)

type MediaStatus struct {
	State        string `json:"state"`
	PreviewKey   string `json:"-"`
	PreviewBytes int64  `json:"size"`
}

type MediaJob struct {
	ImageID, Lease string
	Attempts       int
}

func (s *Store) MediaStatus(ctx context.Context, imageID string) (MediaStatus, error) {
	var m MediaStatus
	err := s.db.QueryRowContext(ctx, `SELECT state,preview_key,preview_bytes FROM media_jobs WHERE image_id=?`, imageID).Scan(&m.State, &m.PreviewKey, &m.PreviewBytes)
	if isNoRows(err) {
		return MediaStatus{State: "ready"}, nil
	}
	return m, err
}

func (s *Store) PreviewImage(ctx context.Context, key string) (*model.Image, error) {
	var imageID string
	err := s.db.QueryRowContext(ctx, `SELECT image_id FROM media_jobs WHERE preview_key=? AND state='ready'`, key).Scan(&imageID)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	img, err := s.AdminImageByID(ctx, imageID)
	if err != nil {
		return nil, err
	}
	if img.DeletedAt != nil {
		return nil, ErrNotFound
	}
	m, err := s.MediaStatus(ctx, imageID)
	if err != nil {
		return nil, err
	}
	if m.State != "ready" || m.PreviewKey != key {
		return nil, ErrNotFound
	}
	img.ObjectKey, img.MIME, img.SizeBytes = key, "image/png", m.PreviewBytes
	return img, nil
}

// A lease is longer than the entire bounded conversion operation. Every write
// checks its token; an expired worker can never publish a newer worker's output.
func (s *Store) ClaimMedia(ctx context.Context) (*MediaJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE media_jobs SET next_attempt=next_attempt WHERE image_id=(SELECT image_id FROM media_jobs WHERE (state='pending' AND next_attempt<=?) OR (state='processing' AND lease_until<?) LIMIT 1)`, now(), now()); err != nil {
		return nil, err
	}
	var j MediaJob
	err = tx.QueryRowContext(ctx, `SELECT j.image_id,j.attempts FROM media_jobs j JOIN images i ON i.id=j.image_id WHERE i.deleted_at IS NULL AND ((j.state='pending' AND j.next_attempt<=?) OR (j.state='processing' AND j.lease_until<?)) ORDER BY j.next_attempt,j.image_id LIMIT 1`, now(), now()).Scan(&j.ImageID, &j.Attempts)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := releaseMediaTx(ctx, tx, j.ImageID); err != nil {
		return nil, err
	}
	if j.Attempts >= 3 {
		_, err = tx.ExecContext(ctx, `UPDATE media_jobs SET state='failed',error='attempt limit',lease='',lease_until=0 WHERE image_id=?`, j.ImageID)
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	j.Lease = id.Random(24)
	j.Attempts++
	_, err = tx.ExecContext(ctx, `UPDATE media_jobs SET state='processing',lease=?,lease_until=?,attempts=attempts+1 WHERE image_id=?`, j.Lease, now()+300, j.ImageID)
	if err != nil {
		return nil, err
	}
	return &j, tx.Commit()
}

// Release an abandoned reservation and durably queue its unique object key.
func releaseMediaTx(ctx context.Context, tx *sql.Tx, imageID string) error {
	var key, user, bucket string
	var size int64
	err := tx.QueryRowContext(ctx, `SELECT j.preview_key,j.preview_bytes,i.user_id,COALESCE(NULLIF(i.bucket_id,''),'bkt_default') FROM media_jobs j JOIN images i ON i.id=j.image_id WHERE j.image_id=?`, imageID).Scan(&key, &size, &user, &bucket)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if key != "" {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO pending_deletes(object_key,bucket_id,created_at) VALUES(?,?,?)`, key, bucket, now()); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET used_bytes=MAX(0,used_bytes-?) WHERE id=?`, size, user); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE storage_buckets SET used_bytes=MAX(0,used_bytes-?) WHERE id=?`, size, bucket); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE media_jobs SET preview_key='',preview_bytes=0 WHERE image_id=?`, imageID)
	return err
}

func (s *Store) ReservePreview(ctx context.Context, j MediaJob, key string, size int64) error {
	if size <= 0 {
		return ErrQuotaExceeded
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE media_jobs SET preview_key=?,preview_bytes=? WHERE image_id=? AND lease=? AND lease_until>? AND state='processing' AND preview_key='' AND EXISTS(SELECT 1 FROM images WHERE id=? AND deleted_at IS NULL)`, key, size, j.ImageID, j.Lease, now(), j.ImageID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE users SET used_bytes=used_bytes+? WHERE id=(SELECT user_id FROM images WHERE id=?) AND status='active' AND used_bytes<=quota_bytes-?`, size, j.ImageID, size)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrQuotaExceeded
	}
	res, err = tx.ExecContext(ctx, `UPDATE storage_buckets SET used_bytes=used_bytes+? WHERE id=(SELECT COALESCE(NULLIF(bucket_id,''),'bkt_default') FROM images WHERE id=?) AND enabled=1 AND (max_bytes=0 OR used_bytes<=max_bytes-?)`, size, j.ImageID, size)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrBucketCapacity
	}
	return tx.Commit()
}

func (s *Store) CompletePreview(ctx context.Context, j MediaJob) error {
	res, err := s.db.ExecContext(ctx, `UPDATE media_jobs SET state='ready',lease='',lease_until=0,error='' WHERE image_id=? AND lease=? AND state='processing' AND lease_until>? AND preview_key<>'' AND EXISTS(SELECT 1 FROM images WHERE id=? AND deleted_at IS NULL)`, j.ImageID, j.Lease, now(), j.ImageID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) FailPreview(ctx context.Context, j MediaJob, permanent bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE media_jobs SET error='preview unavailable' WHERE image_id=? AND lease=? AND state='processing'`, j.ImageID, j.Lease)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	if err = releaseMediaTx(ctx, tx, j.ImageID); err != nil {
		return err
	}
	state := "pending"
	if permanent || j.Attempts >= 3 {
		state = "failed"
	}
	_, err = tx.ExecContext(ctx, `UPDATE media_jobs SET state=?,next_attempt=?,lease='',lease_until=0 WHERE image_id=?`, state, now()+int64(j.Attempts*60), j.ImageID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Called inside the normal deletion transaction for both individual and batch
// deletion. Reservations are released once, and a converter loses its lease.
func deleteMediaTx(ctx context.Context, tx *sql.Tx, imageID string, bases []string) error {
	var key string
	err := tx.QueryRowContext(ctx, `SELECT preview_key FROM media_jobs WHERE image_id=?`, imageID).Scan(&key)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, base := range bases {
		if base != "" && key != "" {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO cdn_purges(url,created_at) VALUES(?,?)`, strings.TrimRight(base, "/")+"/"+key, now()); err != nil {
				return err
			}
		}
	}
	if err := releaseMediaTx(ctx, tx, imageID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM media_jobs WHERE image_id=?`, imageID)
	return err
}
