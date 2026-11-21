CREATE TABLE media_jobs (
 image_id TEXT PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','processing','ready','failed')),
 attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL DEFAULT 0,
 lease TEXT NOT NULL DEFAULT '',
 lease_until INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '',
 preview_key TEXT NOT NULL DEFAULT '',
 preview_bytes INTEGER NOT NULL DEFAULT 0 CHECK(preview_bytes >= 0)
);
CREATE INDEX media_jobs_pending ON media_jobs(state, next_attempt);
CREATE UNIQUE INDEX media_preview_key ON media_jobs(preview_key) WHERE preview_key <> '';
CREATE TRIGGER image_media_job AFTER INSERT ON images
WHEN NEW.mime IN ('image/avif','image/bmp','image/tiff','image/svg+xml','application/x-camera-raw','image/x-nikon-nef','image/x-adobe-dng','image/x-canon-cr2')
BEGIN
 INSERT INTO media_jobs(image_id) VALUES(NEW.id);
END;
CREATE TRIGGER image_media_physical_delete BEFORE DELETE ON images
BEGIN
 INSERT OR IGNORE INTO pending_deletes(object_key,bucket_id,created_at)
 SELECT preview_key,COALESCE(NULLIF(OLD.bucket_id,''),'bkt_default'),unixepoch()
 FROM media_jobs WHERE image_id=OLD.id AND preview_key<>'';
 UPDATE users SET used_bytes=MAX(0,used_bytes-COALESCE((SELECT preview_bytes FROM media_jobs WHERE image_id=OLD.id),0)) WHERE id=OLD.user_id;
 UPDATE storage_buckets SET used_bytes=MAX(0,used_bytes-COALESCE((SELECT preview_bytes FROM media_jobs WHERE image_id=OLD.id),0)) WHERE id=COALESCE(NULLIF(OLD.bucket_id,''),'bkt_default');
END;
