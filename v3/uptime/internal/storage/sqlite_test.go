package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureSQLiteDirCreatesParentForFileURI(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "nested", "uptime.db")
	uri := "file:" + filepath.ToSlash(dbPath) + "?cache=shared"

	if err := ensureSQLiteDir(uri); err != nil {
		t.Fatalf("ensureSQLiteDir() error = %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dbPath)); err != nil {
		t.Fatalf("parent dir was not created: %v", err)
	}
}
