ALTER TABLE invites ADD COLUMN max_uses INTEGER NOT NULL DEFAULT 1 CHECK(max_uses >= 0);
ALTER TABLE invites ADD COLUMN use_count INTEGER NOT NULL DEFAULT 0 CHECK(use_count >= 0);
UPDATE invites SET use_count = 1 WHERE used_by IS NOT NULL;
