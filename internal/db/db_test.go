package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"fujipic/migrations"
)

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "fujipic.db")
	ctx := context.Background()
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := database.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 6 {
		t.Fatalf("schema version = %d", version)
	}
	for _, table := range []string{"users", "sessions", "api_tokens", "images", "storage_buckets", "invites", "pending_deletes", "cdn_purges", "audit_logs"} {
		var name string
		err := database.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
	database.Close()

	// Re-opening must not try to recreate the schema.
	database, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	database.Close()
}

func TestExecScriptTxHandlesComplexSQL(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "complex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	script := `
CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT DEFAULT 'a;b');
-- a semicolon; in comment
CREATE TRIGGER notes_trigger AFTER INSERT ON notes
BEGIN
  UPDATE notes SET body = body || ';x' WHERE id = NEW.id;
END;
INSERT INTO notes DEFAULT VALUES;
`
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := execScriptTx(ctx, tx, script); err != nil {
		tx.Rollback()
		t.Fatalf("execScriptTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var triggerName string
	if err := database.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='trigger' AND name='notes_trigger'`).Scan(&triggerName); err != nil {
		t.Fatalf("trigger missing: %v", err)
	}
	var body string
	if err := database.QueryRowContext(ctx, `SELECT body FROM notes WHERE id = 1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "a;b;x" {
		t.Fatalf("triggered insert body = %q, want %q", body, "a;b;x")
	}
}

func TestOpenAcceptsExistingQueryParams(t *testing.T) {
	path := "file:" + filepath.Join(t.TempDir(), "shared.db") + "?cache=shared"
	database, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open with existing query params: %v", err)
	}
	defer database.Close()
	if err := database.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestDefaultBucketPriorityMigration(t *testing.T) {
	for _, oldPriority := range []int{1000, 42, 0} {
		t.Run(fmt.Sprint(oldPriority), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "priority.db")
			database, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			// Recreate the pre-0006 state without rewriting shipped migrations.
			if _, err := database.Exec(`DELETE FROM schema_migrations WHERE version = 6`); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE storage_buckets SET priority = ? WHERE id = 'bkt_default'`, oldPriority); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`INSERT INTO storage_buckets(id, name, created_at, updated_at)
				VALUES('bkt_custom', 'Custom', unixepoch(), unixepoch())`); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				database, err := Open(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				for id, want := range map[string]int{"bkt_default": oldPriority, "bkt_custom": 100} {
					if id == "bkt_default" && oldPriority == 1000 {
						want = 0
					}
					var got int
					if err := database.QueryRow(`SELECT priority FROM storage_buckets WHERE id = ?`, id).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != want {
						t.Errorf("%s priority = %d, want %d", id, got, want)
					}
				}
			}
			check()
			check() // Reopening does not reapply the migration.
		})
	}
}

// TestExecScriptTxHandlesCaseInTrigger ensures CASE ... END expressions inside
// a trigger body are not mistaken for the trigger's closing END. Otherwise the
// splitter would terminate the CREATE TRIGGER statement at the next semicolon
// and leave an orphaned END behind.
func TestExecScriptTxHandlesCaseInTrigger(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "case-trigger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	script := `
CREATE TABLE flags (id INTEGER PRIMARY KEY, state TEXT);
CREATE TRIGGER flags_trigger AFTER INSERT ON flags
BEGIN
  UPDATE flags
     SET state = CASE
                   WHEN NEW.id % 2 = 0 THEN CASE WHEN NEW.id = 0 THEN 'zero' ELSE 'even' END
                   ELSE 'odd'
                 END
   WHERE id = NEW.id;
END;
INSERT INTO flags(state) VALUES ('new');
`
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := execScriptTx(ctx, tx, script); err != nil {
		tx.Rollback()
		t.Fatalf("execScriptTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var state string
	if err := database.QueryRowContext(ctx, `SELECT state FROM flags WHERE id = 1`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "odd" {
		t.Fatalf("trigger state = %q, want %q", state, "odd")
	}
}

// TestGlobalPoolMigrationPreservesLegacyBuckets applies the pre-pool schema,
// seeds per-user buckets (including colliding and reserved names), then runs
// migration 0005 and checks that no image or retry row is orphaned.
func TestGlobalPoolMigrationPreservesLegacyBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-pool.db")
	ctx := context.Background()
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	for i, name := range []string{
		"0001_init.sql",
		"0002_indexes.sql",
		"0003_pending_delete_on_image_delete.sql",
		"0004_multi_bucket.sql",
	} {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := raw.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := execScriptTx(ctx, tx, string(body)); err != nil {
			tx.Rollback()
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, i+1, time.Now().Unix()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	if _, err := raw.ExecContext(ctx, `
INSERT INTO users(id, email, password_hash, role, status, quota_bytes, used_bytes, created_at, updated_at)
VALUES
  ('usr_1', 'one@example.com', 'hash', 'user', 'active', 1000, 0, ?, ?),
  ('usr_2', 'two@example.com', 'hash', 'user', 'active', 1000, 0, ?, ?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
INSERT INTO storage_buckets(id, user_id, name, endpoint, region, bucket, access_key_id,
                            secret_ciphertext, force_path_style, created_at, updated_at)
VALUES
  ('bkt_old_a', 'usr_1', 'shared', '', 'us-east-1', 'bucket-a', 'key', 'cipher', 0, ?, ?),
  ('bkt_old_b', 'usr_2', 'shared', '', 'eu-central-1', 'bucket-b', 'key', 'cipher', 0, ?, ?),
  ('bkt_old_c', 'usr_1', '服务器默认存储', '', 'ap-tokyo-1', 'bucket-c', 'key', 'cipher', 0, ?, ?),
  ('bkt_old_d', 'usr_2', 'shared [bkt_old_b]', '', 'us-west-2', 'bucket-d', 'key', 'cipher', 0, ?, ?)`,
		now, now, now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
INSERT INTO images(id, user_id, bucket_id, object_key, original_name, mime, size_bytes,
                   width, height, sha256, visibility, created_at)
VALUES
  ('img_a', 'usr_1', 'bkt_old_a', 'i/2026/11/a.png', 'a.png', 'image/png', 10, 1, 1, 'sha-a', 'public', ?),
  ('img_b', 'usr_2', 'bkt_old_b', 'i/2026/11/b.png', 'b.png', 'image/png', 20, 1, 1, 'sha-b', 'public', ?),
  ('img_default', 'usr_1', NULL, 'i/2026/11/d.png', 'd.png', 'image/png', 30, 1, 1, 'sha-d', 'public', ?),
  ('img_d', 'usr_2', 'bkt_old_d', 'i/2026/11/e.png', 'e.png', 'image/png', 40, 1, 1, 'sha-e', 'public', ?)`,
		now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
INSERT INTO pending_deletes(object_key, bucket_id, created_at) VALUES('i/2026/11/legacy.png', '', ?)`, now); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	database, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrate to global pool: %v", err)
	}
	defer database.Close()

	var version int
	if err := database.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 6 {
		t.Fatalf("schema version = %d, want 6", version)
	}
	var names int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT name) FROM storage_buckets`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_buckets`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 5 || names != total {
		t.Fatalf("buckets = %d, distinct names = %d; want 5 unique rows", total, names)
	}
	var defaultName string
	if err := database.QueryRowContext(ctx,
		`SELECT name FROM storage_buckets WHERE id = 'bkt_default' AND is_default = 1`).Scan(&defaultName); err != nil {
		t.Fatal(err)
	}
	var renamed int
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM storage_buckets
		WHERE id IN ('bkt_old_b', 'bkt_old_c', 'bkt_old_d') AND name LIKE '%[bkt_old_%'`).Scan(&renamed); err != nil {
		t.Fatal(err)
	}
	if renamed != 3 {
		t.Fatalf("colliding/reserved/generated legacy names renamed = %d, want 3", renamed)
	}
	var bucketID string
	if err := database.QueryRowContext(ctx,
		`SELECT bucket_id FROM images WHERE id = 'img_a'`).Scan(&bucketID); err != nil {
		t.Fatal(err)
	}
	if bucketID != "bkt_old_a" {
		t.Fatalf("image bucket = %q, want bkt_old_a", bucketID)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT bucket_id FROM images WHERE id = 'img_d'`).Scan(&bucketID); err != nil {
		t.Fatal(err)
	}
	if bucketID != "bkt_old_d" {
		t.Fatalf("renamed bucket image = %q, want bkt_old_d", bucketID)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT bucket_id FROM images WHERE id = 'img_default'`).Scan(&bucketID); err != nil {
		t.Fatal(err)
	}
	if bucketID != "bkt_default" {
		t.Fatalf("default image bucket = %q, want bkt_default", bucketID)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT bucket_id FROM pending_deletes WHERE object_key = 'i/2026/11/legacy.png'`).Scan(&bucketID); err != nil {
		t.Fatal(err)
	}
	if bucketID != "bkt_default" {
		t.Fatalf("pending delete bucket = %q, want bkt_default", bucketID)
	}
	var violations int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign key violations after migration = %d", violations)
	}
	var fkEnabled int
	if err := database.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fkEnabled); err != nil {
		t.Fatal(err)
	}
	if fkEnabled != 1 {
		t.Fatal("foreign keys were not re-enabled after migration")
	}
}
