// Command fujipic is the single binary for the FujiPic service.
//
// Usage:
//
//	fujipic serve
//	fujipic backup --out /backup/fujipic.db
//	FUJIPIC_ADMIN_PASSWORD='...' fujipic admin create --email admin@example.com
//	FUJIPIC_ADMIN_PASSWORD='...' fujipic admin reset-password --email admin@example.com
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/bucket"
	"fujipic/internal/cdn"
	"fujipic/internal/config"
	"fujipic/internal/db"
	"fujipic/internal/id"
	"fujipic/internal/model"
	"fujipic/internal/server"
	"fujipic/internal/storage"
	"fujipic/internal/store"
	"fujipic/internal/upload"
	"fujipic/internal/worker"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := runServe(); err != nil {
			fmt.Fprintln(os.Stderr, "fujipic:", err)
			os.Exit(1)
		}
	case "backup":
		if err := runBackup(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "fujipic backup:", err)
			os.Exit(1)
		}
	case "admin":
		if err := runAdmin(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "fujipic admin:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println("fujipic 1.0.0")
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`FujiPic - lightweight multi-user image host

Usage:
  fujipic serve                  Start the HTTP server (default)
  fujipic backup --out FILE      Online SQLite backup using VACUUM INTO
  fujipic admin create --email EMAIL [--quota-mb N]
  fujipic admin reset-password --email EMAIL
  # Both commands read the password from FUJIPIC_ADMIN_PASSWORD.
  fujipic version

Configuration is read from environment variables; see .env.example.
`)
}

func runServe() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	st := store.New(database)

	objects, err := newStorage(ctx, cfg)
	if err != nil {
		return err
	}
	buckets, err := bucket.NewRegistry(st, objects, cfg.StorageEncryptionKey, func(ctx context.Context, b model.StorageBucket, secret string) (storage.Store, error) {
		return storage.NewS3(ctx, storage.S3Options{
			Endpoint:        b.Endpoint,
			Region:          b.Region,
			Bucket:          b.Bucket,
			AccessKeyID:     b.AccessKeyID,
			SecretAccessKey: secret,
			ForcePathStyle:  b.ForcePathStyle,
			HTTPClient:      bucket.PublicHTTPClient(),
		})
	}, cfg.IsProduction())
	if err != nil {
		return err
	}
	if err := buckets.EnsureDefault(ctx, cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3ForcePathStyle); err != nil {
		return err
	}
	purger := newPurger(cfg)

	if err := bootstrapAdmin(ctx, cfg, st, log); err != nil {
		return err
	}
	cleanupTemp(cfg.TmpDir, log)

	uploads := &upload.Service{
		Store:      st,
		Buckets:    buckets,
		PublicBase: cfg.PublicBaseURL,
		TmpDir:     cfg.TmpDir,
		MaxBytes:   cfg.MaxUploadBytes,
		MaxPixels:  cfg.MaxImagePixels,
		MaxDim:     cfg.MaxImageDimension,
		Log:        log,
	}
	srv, err := server.New(cfg, st, buckets, uploads, purger, log)
	if err != nil {
		return err
	}

	maint := &worker.Maintenance{Store: st, Buckets: buckets, Purger: purger, Interval: 30 * time.Second, TempDir: cfg.TmpDir, Log: log}
	var workerWG sync.WaitGroup
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		maint.Run(ctx)
	}()

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("fujipic listening", "addr", cfg.ListenAddr, "env", cfg.AppEnv,
			"storage", cfg.StorageDriver, "public_base", cfg.PublicBaseURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		stop()
		workerWG.Wait()
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := httpServer.Shutdown(shutdownCtx)
		workerWG.Wait()
		return err
	}
}

func newStorage(ctx context.Context, cfg *config.Config) (storage.Store, error) {
	switch cfg.StorageDriver {
	case "memory":
		return storage.NewMemory(), nil
	case "s3":
		return storage.NewS3(ctx, storage.S3Options{
			Endpoint:        cfg.S3Endpoint,
			Region:          cfg.S3Region,
			Bucket:          cfg.S3Bucket,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
			ForcePathStyle:  cfg.S3ForcePathStyle,
		})
	default:
		return nil, fmt.Errorf("unsupported storage driver %q", cfg.StorageDriver)
	}
}

func newPurger(cfg *config.Config) cdn.Purger {
	switch cfg.CDNPurgeProvider {
	case "cloudflare":
		return &cdn.Cloudflare{ZoneID: cfg.CDNZoneID, Token: cfg.CDNPurgeToken}
	case "generic":
		return &cdn.Generic{Endpoint: cfg.CDNPurgeEndpoint, Token: cfg.CDNPurgeToken}
	default:
		return cdn.None{}
	}
}

func bootstrapAdmin(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) error {
	bootstrapEmail := store.NormalizeEmail(cfg.BootstrapAdminEmail)
	if bootstrapEmail == "" {
		return nil
	}
	count, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	hash, err := auth.HashPassword(cfg.BootstrapAdminPassword)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	user := &model.User{
		ID:           id.New("usr"),
		Email:        bootstrapEmail,
		PasswordHash: hash,
		Role:         "admin",
		Status:       "active",
		QuotaBytes:   cfg.DefaultQuotaBytes,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := st.CreateUser(ctx, user); err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	log.Info("bootstrap admin created", "email", bootstrapEmail)
	return nil
}

func cleanupTemp(dir string, log *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warn("list temp dir", "dir", dir, "err", err)
		}
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "fujipic-upload-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil {
				log.Info("removed stale temp file", "path", path)
			}
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	out := fs.String("out", "", "output database file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	path := os.Getenv("DATABASE_PATH")
	if path == "" {
		path = "./data/fujipic.db"
	}
	if sameDatabasePath(path, *out) {
		return fmt.Errorf("backup destination %q is the live database", *out)
	}
	ctx := context.Background()
	database, err := db.Open(ctx, path)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
		return err
	}

	// VACUUM INTO refuses to overwrite, and writing straight to the
	// destination would destroy the previous backup if the operation failed.
	// Write a sibling temp file first, then rename it into place atomically.
	tmp, err := os.CreateTemp(filepath.Dir(*out), ".fujipic-backup-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)
	if _, err := database.ExecContext(ctx, `VACUUM INTO ?`, tmpPath); err != nil {
		return err
	}
	// Backups contain password/token hashes: owner-only access on top of the
	// parent directory's 0750. Static encryption is left to filesystem or
	// backup tooling and documented in README.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restrict backup permissions: %w", err)
	}
	if err := os.Rename(tmpPath, *out); err != nil {
		return err
	}
	fmt.Println("backup written to", *out)
	return nil
}

// sameDatabasePath reports whether out refers to the live SQLite database.
// It compares cleaned absolute paths and, when both exist, the underlying
// file identity so hard links and symlinks cannot slip through.
func sameDatabasePath(databasePath, out string) bool {
	databasePath = strings.TrimPrefix(databasePath, "file:")
	if i := strings.IndexByte(databasePath, '?'); i >= 0 {
		databasePath = databasePath[:i]
	}
	if databasePath == "" || databasePath == ":memory:" {
		return false
	}
	normalize := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		p = filepath.Clean(p)
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return resolved
		}
		// The file may not exist yet (runBackup checks before opening the
		// database). Resolve the directory so two paths that reach the same
		// not-yet-created file compare equal.
		if resolved, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			return filepath.Join(resolved, filepath.Base(p))
		}
		return p
	}
	if normalize(databasePath) == normalize(out) {
		return true
	}
	dbInfo, dbErr := os.Stat(databasePath)
	outInfo, outErr := os.Stat(out)
	return dbErr == nil && outErr == nil && os.SameFile(dbInfo, outInfo)
}

func runAdmin(args []string) error {
	if len(args) == 0 {
		return errors.New("expected 'create' or 'reset-password'")
	}
	sub := args[0]
	fs := flag.NewFlagSet("admin "+sub, flag.ContinueOnError)
	email := fs.String("email", "", "admin email")
	quotaMB := fs.Int64("quota-mb", 1024, "quota in MiB")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	*email = store.NormalizeEmail(*email)
	password := os.Getenv("FUJIPIC_ADMIN_PASSWORD")
	if *email == "" || password == "" {
		return errors.New("--email and FUJIPIC_ADMIN_PASSWORD are required")
	}
	if len(password) < 10 {
		return errors.New("FUJIPIC_ADMIN_PASSWORD must be at least 10 characters")
	}
	if sub == "create" && (*quotaMB < 1 || *quotaMB > 1<<20) {
		return errors.New("--quota-mb must be between 1 and 1048576")
	}
	path := os.Getenv("DATABASE_PATH")
	if path == "" {
		path = "./data/fujipic.db"
	}
	ctx := context.Background()
	database, err := db.Open(ctx, path)
	if err != nil {
		return err
	}
	defer database.Close()
	st := store.New(database)
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	switch sub {
	case "create":
		now := time.Now().Unix()
		user := &model.User{
			ID:           id.New("usr"),
			Email:        *email,
			PasswordHash: hash,
			Role:         "admin",
			Status:       "active",
			QuotaBytes:   *quotaMB << 20,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := st.CreateUser(ctx, user); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("user %s already exists", *email)
			}
			return err
		}
		fmt.Println("admin created:", *email)
		return nil
	case "reset-password":
		user, err := st.UserByEmail(ctx, *email)
		if err != nil {
			return fmt.Errorf("user %s not found", *email)
		}
		if err := st.UpdatePasswordAndRevokeCredentials(ctx, user.ID, hash, ""); err != nil {
			return err
		}
		fmt.Println("password reset for:", *email)
		return nil
	default:
		return fmt.Errorf("unknown admin subcommand %q", sub)
	}
}
