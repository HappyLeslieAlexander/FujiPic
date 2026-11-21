package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	database, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}
