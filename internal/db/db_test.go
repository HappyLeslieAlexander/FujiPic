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
	if version != 1 {
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

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("empty path accepted")
	}
}
