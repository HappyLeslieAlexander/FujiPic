package db

import (
	"context"
	"path/filepath"
	"testing"
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
	if version != 3 {
		t.Fatalf("schema version = %d", version)
	}
	for _, table := range []string{"users", "sessions", "api_tokens", "images", "invites", "pending_deletes", "cdn_purges", "audit_logs"} {
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
