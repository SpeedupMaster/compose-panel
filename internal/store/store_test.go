package store

import (
	"database/sql"
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

	// Hold 4 concurrent read cursors to open 4 connections, then close them;
	// with SetMaxIdleConns(4), all 4 should remain idle with zero MaxIdleClosed churn.
	var rowsList []*sql.Rows
	for i := 0; i < 4; i++ {
		r, err := db.Query(`SELECT 1 UNION ALL SELECT 2`)
		if err != nil {
			t.Fatalf("concurrent query %d: %v", i, err)
		}
		if !r.Next() {
			t.Fatalf("concurrent query %d returned no row", i)
		}
		rowsList = append(rowsList, r)
	}
	for _, r := range rowsList {
		_ = r.Close()
	}
	stats := db.Stats()
	if stats.Idle < 2 {
		t.Errorf("Idle connections = %d, want >= 2 (SetMaxIdleConns(4))", stats.Idle)
	}
	if stats.MaxIdleClosed != 0 {
		t.Errorf("MaxIdleClosed = %d, want 0", stats.MaxIdleClosed)
	}

	wantIndexes := []string{
		"idx_projects_webhook",
		"idx_sessions_user",
		"idx_sessions_expires",
		"idx_metrics_project_ts",
		"idx_backups_storage",
		"idx_backups_started",
	}
	for _, idx := range wantIndexes {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name); err != nil {
			t.Errorf("missing expected index %s: %v", idx, err)
		}
	}

	// Verify new schema columns exist on fresh database.
	for _, tc := range []struct{ table, col string }{
		{"users", "last_totp_step"},
		{"deployments", "files_json"},
		{"image_checks", "last_auto_digest"},
	} {
		var dummy any
		if err := db.QueryRow(`SELECT `+tc.col+` FROM `+tc.table+` LIMIT 0`).Scan(&dummy); err != nil && err != sql.ErrNoRows {
			t.Errorf("column %s.%s check failed: %v", tc.table, tc.col, err)
		}
	}
}

func TestOpenMigratesAndBackfillsUserAllProjects(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Simulate a pre-migration database without users.all_projects.
	legacySchema := `
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  email TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL,
  totp_secret TEXT NOT NULL DEFAULT '',
  totp_enabled INTEGER NOT NULL DEFAULT 0,
  recovery_codes TEXT NOT NULL DEFAULT '',
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  last_login_at INTEGER NOT NULL DEFAULT 0,
  last_login_ip TEXT NOT NULL DEFAULT ''
);
CREATE TABLE projects (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'app',
  source TEXT NOT NULL DEFAULT 'inline',
  workdir TEXT NOT NULL DEFAULT '',
  git_url TEXT NOT NULL DEFAULT '',
  git_branch TEXT NOT NULL DEFAULT 'main',
  git_compose_path TEXT NOT NULL DEFAULT 'compose.yml',
  git_key TEXT NOT NULL DEFAULT '',
  webhook_token TEXT NOT NULL DEFAULT '',
  webhook_secret TEXT NOT NULL DEFAULT '',
  auto_deploy INTEGER NOT NULL DEFAULT 0,
  compose TEXT NOT NULL DEFAULT '',
  env TEXT NOT NULL DEFAULT '',
  options TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE user_projects (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, project_id)
);
INSERT INTO projects(id,name,created_at,updated_at) VALUES(10,'app1',1,1);
INSERT INTO users(id,username,password_hash,role,created_at) VALUES
  (1,'admin','h','admin',1),
  (2,'restricted_op','h','operator',1),
  (3,'global_op','h','operator',1);
INSERT INTO user_projects(user_id,project_id) VALUES(1,10),(2,10);
`
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	raw.Close()

	// First Open: adds column and backfills restricted non-admin users.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(legacy): %v", err)
	}
	checkAllProjects := func(uid int64, want int) {
		t.Helper()
		var got int
		if err := db.QueryRow(`SELECT all_projects FROM users WHERE id=?`, uid).Scan(&got); err != nil {
			t.Fatalf("query user %d: %v", uid, err)
		}
		if got != want {
			t.Errorf("user %d all_projects = %d, want %d", uid, got, want)
		}
	}
	checkAllProjects(1, 1) // admin stays 1
	checkAllProjects(2, 0) // restricted_op backfilled to 0
	checkAllProjects(3, 1) // global_op stays 1

	// Also insert a 0-project restricted user and verify re-opening stays idempotent.
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,all_projects,created_at) VALUES(4,'zero_op','h','operator',0,1)`); err != nil {
		t.Fatalf("insert zero_op: %v", err)
	}
	db.Close()

	db, err = Open(dbPath)
	if err != nil {
		t.Fatalf("second Open(legacy): %v", err)
	}
	defer db.Close()
	checkAllProjects(1, 1)
	checkAllProjects(2, 0)
	checkAllProjects(3, 1)
	checkAllProjects(4, 0)
}
