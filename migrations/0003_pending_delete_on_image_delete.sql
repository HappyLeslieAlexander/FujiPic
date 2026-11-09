CREATE TRIGGER IF NOT EXISTS trg_images_pending_delete
BEFORE DELETE ON images
FOR EACH ROW
BEGIN
  INSERT OR IGNORE INTO pending_deletes(object_key, created_at)
  VALUES (OLD.object_key, unixepoch());
END;
