CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
);

CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user','admin')),
  status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  quota_bytes   INTEGER NOT NULL DEFAULT 1073741824,
  used_bytes    INTEGER NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash   TEXT NOT NULL UNIQUE,
  ip           TEXT,
  user_agent   TEXT,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE api_tokens (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  token_prefix TEXT NOT NULL,
  scopes       TEXT NOT NULL DEFAULT 'upload,read,delete',
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  expires_at   INTEGER
);
CREATE INDEX idx_tokens_user ON api_tokens(user_id);

CREATE TABLE images (
  id            TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  object_key    TEXT NOT NULL UNIQUE,
  original_name TEXT NOT NULL DEFAULT '',
  mime          TEXT NOT NULL,
  size_bytes    INTEGER NOT NULL,
  width         INTEGER,
  height        INTEGER,
  sha256        TEXT NOT NULL,
  visibility    TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','private')),
  created_at    INTEGER NOT NULL,
  deleted_at    INTEGER
);
CREATE INDEX idx_images_user_created ON images(user_id, created_at DESC);
CREATE UNIQUE INDEX idx_images_user_sha_active
  ON images(user_id, sha256) WHERE deleted_at IS NULL;

CREATE TABLE invites (
  code       TEXT PRIMARY KEY,
  created_by TEXT REFERENCES users(id),
  used_by    TEXT REFERENCES users(id),
  created_at INTEGER NOT NULL,
  expires_at INTEGER
);

CREATE TABLE pending_deletes (
  object_key TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL
);

CREATE TABLE cdn_purges (
  url        TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT
);
CREATE INDEX idx_cdn_purges_created ON cdn_purges(created_at);

CREATE TABLE audit_logs (
  id         TEXT PRIMARY KEY,
  actor_id   TEXT REFERENCES users(id),
  action     TEXT NOT NULL,
  target     TEXT,
  ip         TEXT,
  meta       TEXT,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_audit_created ON audit_logs(created_at DESC);
