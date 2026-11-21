-- Multi-bucket storage. The server default bucket still comes from S3_*;
-- storage_buckets holds user-owned buckets with encrypted credentials.

CREATE TABLE storage_buckets (
  id                TEXT PRIMARY KEY,
  user_id           TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,
  endpoint          TEXT NOT NULL DEFAULT '',
  region            TEXT NOT NULL,
  bucket            TEXT NOT NULL,
  access_key_id     TEXT NOT NULL,
  secret_ciphertext TEXT NOT NULL,
  force_path_style  INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_storage_buckets_user_name ON storage_buckets(user_id, name);
CREATE INDEX idx_storage_buckets_user ON storage_buckets(user_id);

ALTER TABLE images ADD COLUMN bucket_id TEXT REFERENCES storage_buckets(id) ON DELETE RESTRICT;
CREATE INDEX idx_images_bucket ON images(bucket_id);

-- pending_deletes needs a composite key so the same object key in two buckets
-- can be retried independently. SQLite cannot change a primary key in place.
CREATE TABLE pending_deletes_v2 (
  object_key TEXT NOT NULL,
  bucket_id  TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  PRIMARY KEY (object_key, bucket_id)
);
INSERT INTO pending_deletes_v2(object_key, bucket_id, created_at, attempts, last_error)
SELECT object_key, '', created_at, attempts, last_error FROM pending_deletes;
DROP TRIGGER IF EXISTS trg_images_pending_delete;
DROP TABLE pending_deletes;
ALTER TABLE pending_deletes_v2 RENAME TO pending_deletes;
CREATE INDEX idx_pending_deletes_created ON pending_deletes(created_at ASC);

CREATE TRIGGER trg_images_pending_delete
BEFORE DELETE ON images
FOR EACH ROW
BEGIN
  INSERT OR IGNORE INTO pending_deletes(object_key, bucket_id, created_at)
  VALUES (OLD.object_key, COALESCE(OLD.bucket_id, ''), unixepoch());
END;
