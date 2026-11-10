package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fujipic/internal/config"
	"fujipic/internal/db"
	"fujipic/internal/store"
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

	// A second backup must replace the first atomically and keep the mode.
	if err := runBackup([]string{"--out", out}); err != nil {
		t.Fatalf("second runBackup: %v", err)
	}
	info, err = os.Stat(out)
	if err != nil {
		t.Fatalf("stat replaced backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("replaced backup permissions = %o, want 0600", perm)
	}
}

func TestRunBackupRejectsLiveDatabasePath(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "fujipic.db")
	t.Setenv("DATABASE_PATH", dbPath)

	database, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()

	for _, out := range []string{dbPath, filepath.Join(dir, "data", "..", "data", "fujipic.db")} {
		if err := runBackup([]string{"--out", out}); err == nil {
			t.Fatalf("runBackup accepted the live database path %q", out)
		}
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("live database was removed: %v", err)
	}
}

func TestSameDatabasePathResolvesSymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	// The database does not exist yet, so EvalSymlinks fails on the file and
	// the parent directory must be resolved instead.
	dbPath := filepath.Join(real, "fujipic.db")
	if !sameDatabasePath(dbPath, filepath.Join(link, "fujipic.db")) {
		t.Fatal("same file through a symlinked parent was not detected")
	}
	if sameDatabasePath(dbPath, filepath.Join(real, "other.db")) {
		t.Fatal("different files were reported as the same")
	}
}

func TestCleanupTempHandlesGlobMetacharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tmp[1]")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "fujipic-upload-stale")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanupTemp(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp file was not removed: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated file was removed: %v", err)
	}
}

func TestRunAdminRejectsInvalidQuota(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "fujipic.db")
	t.Setenv("DATABASE_PATH", path)
	t.Setenv("FUJIPIC_ADMIN_PASSWORD", "admin-password-123")

	for _, quota := range []string{"0", "-1", "1048577"} {
		if err := runAdmin([]string{"create", "--email", "admin@example.com", "--quota-mb", quota}); err == nil {
			t.Fatalf("quota %s was accepted", quota)
		}
	}
	if err := runAdmin([]string{"create", "--email", "admin@example.com", "--quota-mb", "1"}); err != nil {
		t.Fatalf("valid quota rejected: %v", err)
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

func TestBootstrapAdminRequiresStrongPasswordForFirstUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "fujipic.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		BootstrapAdminEmail:    "admin@example.com",
		BootstrapAdminPassword: "short",
		DefaultQuotaBytes:      1 << 30,
	}

	if err := bootstrapAdmin(context.Background(), cfg, st, logger); err == nil {
		t.Fatal("short bootstrap password was accepted on an empty database")
	}

	cfg.BootstrapAdminPassword = "admin-password-123"
	if err := bootstrapAdmin(context.Background(), cfg, st, logger); err != nil {
		t.Fatalf("valid bootstrap password rejected: %v", err)
	}

	// Existing deployments may still have a legacy short password in the
	// environment; startup must not fail once an admin exists.
	cfg.BootstrapAdminPassword = "short"
	if err := bootstrapAdmin(context.Background(), cfg, st, logger); err != nil {
		t.Fatalf("short password broke startup after bootstrap: %v", err)
	}
}
