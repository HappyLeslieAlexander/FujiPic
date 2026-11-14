-- Retry indefinitely with bounded exponential backoff. Old exhausted tasks
-- become eligible again immediately after upgrading.
ALTER TABLE pending_deletes ADD COLUMN next_attempt_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cdn_purges ADD COLUMN next_attempt_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_pending_deletes_due ON pending_deletes(next_attempt_at, created_at);
CREATE INDEX idx_cdn_purges_due ON cdn_purges(next_attempt_at, created_at);
CREATE INDEX idx_tokens_expires ON api_tokens(expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX idx_images_active_user ON images(user_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_images_active_bucket ON images(COALESCE(NULLIF(bucket_id, ''), 'bkt_default'), id) WHERE deleted_at IS NULL;

UPDATE images SET width = 0 WHERE width IS NULL;
UPDATE images SET height = 0 WHERE height IS NULL;

-- A soft-deleted row already queued cleanup transactionally. In particular,
-- detached rows from removed custom buckets must not create default deletes.
DROP TRIGGER IF EXISTS trg_images_pending_delete;
CREATE TRIGGER trg_images_pending_delete
BEFORE DELETE ON images
FOR EACH ROW WHEN OLD.deleted_at IS NULL
BEGIN
  INSERT OR IGNORE INTO pending_deletes(object_key, bucket_id, created_at)
  VALUES (OLD.object_key, COALESCE(NULLIF(OLD.bucket_id, ''), 'bkt_default'), unixepoch());
END;
