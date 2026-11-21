-- Global storage pool. Buckets are configured by an administrator, selected
-- automatically by priority, and limited by an optional maximum capacity.
-- The server's S3_* bucket is represented by the fixed row bkt_default.

CREATE TABLE storage_buckets_v2 (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL,
  endpoint          TEXT NOT NULL DEFAULT '',
  region            TEXT NOT NULL DEFAULT '',
  bucket            TEXT NOT NULL DEFAULT '',
  access_key_id     TEXT NOT NULL DEFAULT '',
  secret_ciphertext TEXT NOT NULL DEFAULT '',
  force_path_style  INTEGER NOT NULL DEFAULT 0 CHECK (force_path_style IN (0,1)),
  max_bytes         INTEGER NOT NULL DEFAULT 0 CHECK (max_bytes >= 0),
  used_bytes        INTEGER NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
  priority          INTEGER NOT NULL DEFAULT 100 CHECK (priority >= 0),
  enabled           INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  is_default        INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0,1)),
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);

INSERT INTO storage_buckets_v2(
  id, name, endpoint, region, bucket, access_key_id, secret_ciphertext,
  force_path_style, max_bytes, used_bytes, priority, enabled, is_default,
  created_at, updated_at)
VALUES('bkt_default', '服务器默认存储', '', '', '', '', '', 0, 0, 0, 1000, 1, 1,
       unixepoch(), unixepoch());

-- Preserve buckets created by the earlier per-user design. Their names were
-- unique per user, so append the full bucket ID to names that collide with the
-- reserved default name, with an older bucket, or with a name produced by this
-- same renaming rule.
INSERT INTO storage_buckets_v2(
  id, name, endpoint, region, bucket, access_key_id, secret_ciphertext,
  force_path_style, max_bytes, used_bytes, priority, enabled, is_default,
  created_at, updated_at)
SELECT
  b.id,
  CASE WHEN b.name = '服务器默认存储' OR b.name LIKE '%[bkt_%]' OR EXISTS (
    SELECT 1 FROM storage_buckets older
    WHERE older.name = b.name
      AND (older.created_at < b.created_at OR (older.created_at = b.created_at AND older.id < b.id))
  ) THEN substr(b.name, 1, 40) || ' [' || b.id || ']'
  ELSE b.name END,
  b.endpoint, b.region, b.bucket, b.access_key_id, b.secret_ciphertext,
  b.force_path_style, 0, 0, 100, 1, 0, b.created_at, b.updated_at
FROM storage_buckets b;

DROP TABLE storage_buckets;
ALTER TABLE storage_buckets_v2 RENAME TO storage_buckets;

CREATE UNIQUE INDEX idx_storage_buckets_name ON storage_buckets(name);
CREATE UNIQUE INDEX idx_storage_buckets_default ON storage_buckets(is_default) WHERE is_default = 1;
CREATE INDEX idx_storage_buckets_pool ON storage_buckets(enabled, priority, created_at);

UPDATE images SET bucket_id = 'bkt_default' WHERE bucket_id IS NULL OR bucket_id = '';
UPDATE pending_deletes SET bucket_id = 'bkt_default' WHERE bucket_id = '';

DROP TRIGGER IF EXISTS trg_images_pending_delete;
CREATE TRIGGER trg_images_pending_delete
BEFORE DELETE ON images
FOR EACH ROW
BEGIN
  INSERT OR IGNORE INTO pending_deletes(object_key, bucket_id, created_at)
  VALUES (OLD.object_key, COALESCE(OLD.bucket_id, 'bkt_default'), unixepoch());
END;
