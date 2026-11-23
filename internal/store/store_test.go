package store

import (
	"path/filepath"
	"testing"
)

func TestOpenConfiguresLowMemoryPragmasAndPool(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer db.Close()

	var cacheSize int
	if err := db.QueryRow(`PRAGMA cache_size`).Scan(&cacheSize); err != nil {
		t.Fatalf("PRAGMA cache_size: %v", err)
	}
	if cacheSize != -1000 {
		t.Errorf("PRAGMA cache_size = %d, want -1000", cacheSize)
	}

	var journalLimit int64
	if err := db.QueryRow(`PRAGMA journal_size_limit`).Scan(&journalLimit); err != nil {
		t.Fatalf("PRAGMA journal_size_limit: %v", err)
	}
	if journalLimit != 4194304 {
		t.Errorf("PRAGMA journal_size_limit = %d, want 4194304", journalLimit)
	}

	if got := db.Stats().MaxOpenConnections; got != 4 {
		t.Errorf("MaxOpenConnections = %d, want 4", got)
	}
}
