package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"fujipic/migrations"
)

func TestInviteUsageMigrationPreservesSpentInvites(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "invites.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE invites(code TEXT PRIMARY KEY, used_by TEXT); INSERT INTO invites VALUES('unused',NULL),('used','usr_old')`); err != nil {
		t.Fatal(err)
	}
	body, err := migrations.FS.ReadFile("0007_invite_usage_limits.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := execScriptTx(context.Background(), tx, string(body)); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for code, want := range map[string]int{"unused": 0, "used": 1} {
		var max, count int
		if err := db.QueryRow(`SELECT max_uses,use_count FROM invites WHERE code=?`, code).Scan(&max, &count); err != nil {
			t.Fatal(err)
		}
		if max != 1 || count != want {
			t.Fatalf("migration revived or changed invite %s: %d/%d", code, count, max)
		}
	}
}
