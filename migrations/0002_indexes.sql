CREATE INDEX IF NOT EXISTS idx_images_deleted_created ON images(deleted_at, created_at);
CREATE INDEX IF NOT EXISTS idx_users_created ON users(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_pending_deletes_created ON pending_deletes(created_at ASC);
ALTER TABLE pending_deletes ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pending_deletes ADD COLUMN last_error TEXT;
