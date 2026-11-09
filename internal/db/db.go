// Package db opens the SQLite database and applies embedded migrations.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"fujipic/migrations"
)

// Open opens (and creates if needed) the SQLite database, enables WAL and
// foreign keys, then applies any pending migrations.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is empty")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	dsn := path
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	if !strings.Contains(dsn, "_pragma=") {
		dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	database.SetMaxOpenConns(1) // SQLite has a single writer; keep it simple and predictable.
	database.SetConnMaxLifetime(0)
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := Migrate(ctx, database); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

// Migrate applies every embedded migration that has not been recorded yet.
func Migrate(ctx context.Context, database *sql.DB) error {
	var exists int
	err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists)
	if err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if exists == 0 {
			if err := execScript(ctx, database, string(body)); err != nil {
				return fmt.Errorf("apply migration %s: %w", name, err)
			}
			if _, err := database.ExecContext(ctx,
				`INSERT INTO schema_migrations(version, applied_at) VALUES(1, ?)`, time.Now().Unix()); err != nil {
				return fmt.Errorf("record migration %s: %w", name, err)
			}
			exists = 1
			continue
		}
		// 0001_init.sql is idempotent only on a fresh database. Future
		// migrations get a version number and are recorded below.
		if name == "0001_init.sql" {
			continue
		}
		version := migrationVersion(name)
		var applied int
		if err := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := execScriptTx(ctx, tx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, time.Now().Unix()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func migrationVersion(name string) int {
	var v int
	fmt.Sscanf(name, "%d", &v)
	return v
}

func execScript(ctx context.Context, database *sql.DB, script string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := execScriptTx(ctx, tx, script); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func execScriptTx(ctx context.Context, tx *sql.Tx, script string) error {
	for _, stmt := range strings.Split(script, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("statement %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
