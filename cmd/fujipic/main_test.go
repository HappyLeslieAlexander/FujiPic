package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunBackupRestrictsPermissions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "fujipic.db")
	t.Setenv("DATABASE_PATH", dbPath)

	out := filepath.Join(dir, "backup", "fujipic.db")
	if err := runBackup([]string{"--out", out}); err != nil {
		t.Fatalf("runBackup: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("backup permissions = %o, want 0600", perm)
	}
	if info.Size() == 0 {
		t.Fatal("backup file is empty")
	}
}

func TestRunBackupRequiresOut(t *testing.T) {
	if err := runBackup(nil); err == nil {
		t.Fatal("expected error without --out")
	}
}
