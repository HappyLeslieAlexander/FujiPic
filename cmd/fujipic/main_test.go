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

func TestRunAdminRequiresPasswordEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "fujipic.db")
	t.Setenv("DATABASE_PATH", path)
	t.Setenv("FUJIPIC_ADMIN_PASSWORD", "")

	if err := runAdmin([]string{"create", "--email", "admin@example.com"}); err == nil {
		t.Fatal("expected missing password error")
	}
	t.Setenv("FUJIPIC_ADMIN_PASSWORD", "admin-password-123")
	if err := runAdmin([]string{"create", "--email", "admin@example.com", "--password", "should-not-be-a-flag"}); err == nil {
		t.Fatal("plaintext --password flag was accepted")
	}
	if err := runAdmin([]string{"create", "--email", "admin@example.com"}); err != nil {
		t.Fatalf("create through environment password: %v", err)
	}
}

func TestRunBackupRequiresOut(t *testing.T) {
	if err := runBackup(nil); err == nil {
		t.Fatal("expected error without --out")
	}
}
