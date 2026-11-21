-- Persist configured purge origins so offline/cascading SQL deletions can
-- enqueue the same URLs as application deletions, without a running web process.
CREATE TABLE cdn_purge_origins (base_url TEXT PRIMARY KEY);

CREATE TRIGGER image_physical_delete_purge BEFORE DELETE ON images
WHEN OLD.deleted_at IS NULL
BEGIN
 INSERT OR IGNORE INTO cdn_purges(url,created_at)
 SELECT base_url || '/' || OLD.object_key,unixepoch() FROM cdn_purge_origins;
 INSERT OR IGNORE INTO cdn_purges(url,created_at)
 SELECT o.base_url || '/' || j.preview_key,unixepoch()
 FROM cdn_purge_origins o,media_jobs j
 WHERE j.image_id=OLD.id AND j.preview_key<>'';
END;
