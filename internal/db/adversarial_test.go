package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCustomPragmaPreservesRequiredPragmas(t *testing.T) {
	database, err := Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "custom.db")+"?_pragma=cache_size(100)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// Force a new connection, rather than relying on migration's FK reset.
	database.SetMaxIdleConns(0)
	var foreignKeys, busyTimeout int
	var journal string
	if err := database.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 || journal != "wal" {
		t.Fatalf("foreign_keys=%d busy_timeout=%d journal=%s", foreignKeys, busyTimeout, journal)
	}
}

func TestMigrationBracketIdentifiersAndAdjacentComments(t *testing.T) {
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "quoted.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = execScriptTx(context.Background(), tx, `
CREATE TABLE [odd; table] ([select] TEXT);
CREATE TRIGGER quoted_trigger AFTER INSERT ON [odd; table]
BEGIN/* adjacent comment */
  UPDATE [odd; table] SET [select] = 'updated';
END;
INSERT INTO [odd; table] VALUES ('initial');`)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := tx.QueryRow(`SELECT [select] FROM [odd; table]`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "updated" {
		t.Fatalf("trigger result=%q", got)
	}
}
