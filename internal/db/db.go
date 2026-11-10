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
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		dsn += separator + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
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
			if err := applyMigration(ctx, database, string(body), 1); err != nil {
				return fmt.Errorf("apply migration %s: %w", name, err)
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
		if err := applyMigration(ctx, database, string(body), version); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

func migrationVersion(name string) int {
	var v int
	fmt.Sscanf(name, "%d", &v)
	return v
}

func applyMigration(ctx context.Context, database *sql.DB, script string, version int) (err error) {
	// Table rebuilds cannot drop a parent table while foreign keys are
	// enforced. Pin one connection, disable enforcement for the duration of
	// the migration, and verify the final schema before committing.
	conn, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		// The connection returns to the pool, so never leave enforcement off.
		if _, fkErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys=ON`); fkErr != nil && err == nil {
			err = fmt.Errorf("re-enable foreign keys after migration %d: %w", version, fkErr)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := execScriptTx(ctx, tx, script); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, time.Now().Unix()); err != nil {
		return err
	}
	var violations int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return err
	}
	if violations > 0 {
		return fmt.Errorf("migration %d leaves %d foreign key violations", version, violations)
	}
	return tx.Commit()
}

func execScriptTx(ctx context.Context, tx *sql.Tx, script string) error {
	for _, stmt := range splitSQLStatements(script) {
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

func stripSQLComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "--") {
			newline := strings.IndexByte(s, '\n')
			if newline < 0 {
				return ""
			}
			s = s[newline+1:]
			continue
		}
		if strings.HasPrefix(s, "/*") {
			end := strings.Index(s[2:], "*/")
			if end < 0 {
				return ""
			}
			s = s[2+end+2:]
			continue
		}
		return s
	}
}

// splitSQLStatements splits a migration script into SQL statements without
// treating semicolons inside quoted literals, comments or trigger bodies as
// statement terminators.
func splitSQLStatements(script string) []string {
	var statements []string
	var current strings.Builder
	runes := []rune(script)
	var quote rune
	inLineComment := false
	inBlockComment := false
	triggerDepth := 0
	caseDepth := 0
	wordStart := -1

	flushWord := func(index int) {
		if wordStart < 0 {
			return
		}
		word := strings.ToUpper(string(runes[wordStart:index]))
		statement := strings.TrimSpace(stripSQLComments(current.String()))
		if strings.HasPrefix(strings.ToUpper(statement), "CREATE TRIGGER") ||
			strings.HasPrefix(strings.ToUpper(statement), "CREATE TEMP TRIGGER") ||
			strings.HasPrefix(strings.ToUpper(statement), "CREATE TEMPORARY TRIGGER") {
			switch {
			case word == "BEGIN":
				triggerDepth++
			case word == "CASE" && triggerDepth > 0:
				// CASE ... END expressions inside the body use END too, so
				// they must not close the trigger's BEGIN block.
				caseDepth++
			case word == "END" && triggerDepth > 0:
				if caseDepth > 0 {
					caseDepth--
				} else {
					triggerDepth--
				}
			}
		}
		wordStart = -1
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if inLineComment {
			current.WriteRune(r)
			if r == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			current.WriteRune(r)
			if r == '*' && i+1 < len(runes) && runes[i+1] == '/' {
				current.WriteRune(runes[i+1])
				i++
				inBlockComment = false
			}
			continue
		}
		if quote != 0 {
			current.WriteRune(r)
			if r == quote {
				if i+1 < len(runes) && runes[i+1] == quote {
					current.WriteRune(runes[i+1])
					i++
					continue
				}
				quote = 0
			}
			continue
		}

		if r == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			inLineComment = true
			current.WriteRune(r)
			continue
		}
		if r == '/' && i+1 < len(runes) && runes[i+1] == '*' {
			inBlockComment = true
			current.WriteRune(r)
			continue
		}
		if r == '\'' || r == '"' || r == '`' {
			wordStart = -1
			quote = r
			current.WriteRune(r)
			continue
		}
		if r == '[' {
			wordStart = -1
			for i++; i < len(runes); i++ {
				current.WriteRune(runes[i])
				if runes[i] == ']' {
					if i+1 < len(runes) && runes[i+1] == ']' {
						current.WriteRune(runes[i+1])
						i++
						continue
					}
					break
				}
			}
			continue
		}

		if r == ';' {
			flushWord(i)
			current.WriteRune(r)
			if triggerDepth == 0 {
				stmt := strings.TrimSpace(current.String())
				if stmt != "" {
					statements = append(statements, stmt)
				}
				current.Reset()
			}
			continue
		}

		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			if wordStart < 0 {
				wordStart = i
			}
			current.WriteRune(r)
			continue
		}
		flushWord(i)
		current.WriteRune(r)
	}

	flushWord(len(runes))
	if stmt := strings.TrimSpace(current.String()); stmt != "" {
		statements = append(statements, stmt)
	}
	return statements
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	runes := []rune(s)
	if len(runes) > 80 {
		return string(runes[:80])
	}
	return s
}
