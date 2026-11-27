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
		"idx_cla_project_service",
		"idx_cla_name",
		"idx_cla_archived_at",
	}
	for _, idx := range wantIndexes {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name); err != nil {
			t.Errorf("missing expected index %s: %v", idx, err)
		}
	}

	// Verify every migrated schema column exists on a fresh database.
	for _, tc := range []struct{ table, col string }{
		{"sites", "maintenance"},
		{"sites", "maintenance_msg"},
		{"sites", "maintenance_deploy"},
		{"sites", "access_log"},
		{"sites", "load_balance"},
		{"sites", "health_path"},
		{"sites", "grpc"},
		{"backups", "verified_at"},
		{"backups", "origin"},
		{"db_instances", "image"},
		{"users", "all_projects"},
		{"users", "last_totp_step"},
		{"deployments", "files_json"},
		{"image_checks", "last_auto_digest"},
		{"container_log_archives", "id"},
		{"container_log_archives", "project"},
		{"container_log_archives", "service"},
		{"container_log_archives", "container_id"},
		{"container_log_archives", "container_name"},
		{"container_log_archives", "image"},
		{"container_log_archives", "reason"},
		{"container_log_archives", "started_at"},
		{"container_log_archives", "archived_at"},
		{"container_log_archives", "size_bytes"},
		{"container_log_archives", "raw_bytes"},
		{"container_log_archives", "line_count"},
		{"container_log_archives", "truncated"},
	} {
		var dummy any
		if err := db.QueryRow(`SELECT `+tc.col+` FROM `+tc.table+` LIMIT 0`).Scan(&dummy); err != nil && err != sql.ErrNoRows {
			t.Errorf("column %s.%s check failed: %v", tc.table, tc.col, err)
		}
	}

	// Verify all versioned migrations are recorded in schema_migrations.
	var count, maxVer int
	var minApplied int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(version), 0), COALESCE(MIN(applied_at), 0) FROM schema_migrations`).Scan(&count, &maxVer, &minApplied); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	wantLatest := migrations[len(migrations)-1].version
	if count != len(migrations) || maxVer != wantLatest {
		t.Errorf("schema_migrations count=%d max=%d, want count=%d max=%d", count, maxVer, len(migrations), wantLatest)
	}
	if minApplied <= 0 {
		t.Errorf("schema_migrations min applied_at = %d, want > 0", minApplied)
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

func TestOpenMigratesLegacyV010DBAndRunsBackfillOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v010.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Simulate a legacy v0.1.0 database without schema_migrations and missing columns
	// across sites, backups, db_instances, users, deployments, and image_checks.
	// Include one already-added column (sites.maintenance) to verify duplicate-column
	// tolerance when upgrading a pre-schema_migrations production database.
	v010Schema := `
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
CREATE TABLE deployments (
  id INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  number INTEGER NOT NULL,
  status TEXT NOT NULL,
  trigger TEXT NOT NULL,
  actor TEXT NOT NULL DEFAULT '',
  commit_sha TEXT NOT NULL DEFAULT '',
  commit_msg TEXT NOT NULL DEFAULT '',
  compose TEXT NOT NULL DEFAULT '',
  env TEXT NOT NULL DEFAULT '',
  job_id TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE db_instances (
  id INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL UNIQUE REFERENCES projects(id) ON DELETE CASCADE,
  engine TEXT NOT NULL,
  version TEXT NOT NULL,
  root_user TEXT NOT NULL,
  root_password TEXT NOT NULL,
  port INTEGER NOT NULL,
  exposure TEXT NOT NULL DEFAULT 'internal',
  created_at INTEGER NOT NULL
);
CREATE TABLE backups (
  id INTEGER PRIMARY KEY,
  policy_id INTEGER,
  source TEXT NOT NULL,
  label TEXT NOT NULL,
  storage_id INTEGER NOT NULL,
  object_key TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT '',
  encrypted INTEGER NOT NULL DEFAULT 0,
  format TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  tiers TEXT NOT NULL DEFAULT '',
  job_id TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE sites (
  id INTEGER PRIMARY KEY,
  domains TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'proxy',
  project_id INTEGER,
  service TEXT NOT NULL DEFAULT '',
  port INTEGER NOT NULL DEFAULT 0,
  upstream TEXT NOT NULL DEFAULT '',
  hsts INTEGER NOT NULL DEFAULT 0,
  compress INTEGER NOT NULL DEFAULT 1,
  basic_user TEXT NOT NULL DEFAULT '',
  basic_hash TEXT NOT NULL DEFAULT '',
  ip_allow TEXT NOT NULL DEFAULT '',
  max_body TEXT NOT NULL DEFAULT '',
  headers TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  maintenance INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE image_checks (
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  service TEXT NOT NULL,
  image TEXT NOT NULL,
  local_digest TEXT NOT NULL DEFAULT '',
  remote_digest TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, service)
);
INSERT INTO projects(id,name,created_at,updated_at) VALUES(1,'app1',1,1);
INSERT INTO users(id,username,password_hash,role,created_at) VALUES(1,'admin','h','admin',1),(2,'op','h','operator',1);
INSERT INTO user_projects(user_id,project_id) VALUES(2,1);
INSERT INTO sites(id,domains,created_at,maintenance) VALUES(1,'example.com',1,1);
INSERT INTO backups(id,source,label,storage_id,status,started_at) VALUES(1,'{}','b1',1,'success',1);
`
	if _, err := raw.Exec(v010Schema); err != nil {
		t.Fatalf("seed v0.1.0 schema: %v", err)
	}
	raw.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(v0.1.0): %v", err)
	}

	// Verify all migrated columns exist and preserve existing row data/defaults.
	var maint int
	var grpc int
	if err := db.QueryRow(`SELECT maintenance, grpc FROM sites WHERE id=1`).Scan(&maint, &grpc); err != nil {
		t.Fatalf("select migrated sites columns: %v", err)
	}
	if maint != 1 || grpc != 0 {
		t.Errorf("sites row maintenance=%d grpc=%d, want 1 and 0", maint, grpc)
	}

	var origin string
	var verifiedAt int64
	if err := db.QueryRow(`SELECT origin, verified_at FROM backups WHERE id=1`).Scan(&origin, &verifiedAt); err != nil {
		t.Fatalf("select migrated backups columns: %v", err)
	}
	if origin != "backup" || verifiedAt != 0 {
		t.Errorf("backups row origin=%q verified_at=%d, want 'backup' and 0", origin, verifiedAt)
	}

	for _, q := range []string{
		`SELECT image FROM db_instances LIMIT 0`,
		`SELECT last_totp_step, all_projects FROM users LIMIT 0`,
		`SELECT files_json FROM deployments LIMIT 0`,
		`SELECT last_auto_digest FROM image_checks LIMIT 0`,
	} {
		var dummy any
		if err := db.QueryRow(q).Scan(&dummy); err != nil && err != sql.ErrNoRows {
			t.Errorf("query %q failed: %v", q, err)
		}
	}

	// Verify backfill ran on first upgrade.
	var allProjects int
	if err := db.QueryRow(`SELECT all_projects FROM users WHERE id=2`).Scan(&allProjects); err != nil {
		t.Fatalf("query op all_projects: %v", err)
	}
	if allProjects != 0 {
		t.Fatalf("op all_projects after upgrade = %d, want 0", allProjects)
	}

	// Manually flip op back to all_projects=1 while keeping user_projects row,
	// then re-open DB: versioned migration 21 must NOT re-run on subsequent boots.
	if _, err := db.Exec(`UPDATE users SET all_projects=1 WHERE id=2`); err != nil {
		t.Fatalf("update op all_projects: %v", err)
	}
	db.Close()

	db, err = Open(dbPath)
	if err != nil {
		t.Fatalf("second Open(v0.1.0): %v", err)
	}
	defer db.Close()

	if err := db.QueryRow(`SELECT all_projects FROM users WHERE id=2`).Scan(&allProjects); err != nil {
		t.Fatalf("query op all_projects after reopen: %v", err)
	}
	if allProjects != 1 {
		t.Errorf("op all_projects after reopen = %d, want 1 (backfill migration should only run once)", allProjects)
	}
}

func TestOpenReadOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "panel.db")
	rw, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	if _, err := rw.Exec(`INSERT INTO settings(key,value) VALUES('probe','ok')`); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	defer rw.Close()

	ro, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly() error: %v", err)
	}
	defer ro.Close()

	var queryOnly int
	if err := ro.QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil {
		t.Fatalf("PRAGMA query_only: %v", err)
	}
	if queryOnly != 1 {
		t.Errorf("PRAGMA query_only = %d, want 1", queryOnly)
	}

	var busyTimeout int
	if err := ro.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("PRAGMA busy_timeout = %d, want 5000", busyTimeout)
	}

	var val string
	if err := ro.QueryRow(`SELECT value FROM settings WHERE key='probe'`).Scan(&val); err != nil {
		t.Fatalf("read via OpenReadOnly: %v", err)
	}
	if val != "ok" {
		t.Errorf("got %q, want %q", val, "ok")
	}

	if _, err := ro.Exec(`INSERT INTO settings(key,value) VALUES('write','nope')`); err == nil {
		t.Errorf("expected write via OpenReadOnly to fail, succeeded")
	}
}

func TestOpenMigratesContainerLogArchives(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v21.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL); INSERT INTO schema_migrations(version, applied_at) VALUES(21, 1700000000);`); err != nil {
		raw.Close()
		t.Fatalf("seed v21 schema_migrations: %v", err)
	}
	// Apply migrations directly without re-running schema.sql so v22 itself must create the table & indexes.
	if err := applyMigrations(raw); err != nil {
		raw.Close()
		t.Fatalf("applyMigrations(v21): %v", err)
	}
	var maxVer int
	if err := raw.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&maxVer); err != nil {
		raw.Close()
		t.Fatalf("query max version: %v", err)
	}
	if maxVer < 22 {
		raw.Close()
		t.Fatalf("schema_migrations max version = %d, want >= 22", maxVer)
	}
	for _, idx := range []string{"idx_cla_project_service", "idx_cla_name", "idx_cla_archived_at"} {
		var name string
		if err := raw.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name); err != nil {
			raw.Close()
			t.Fatalf("migration v22 missing index %s: %v", idx, err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO container_log_archives(id, project, service, container_id, container_name, image, reason, started_at, archived_at, size_bytes, raw_bytes, line_count, truncated) VALUES('cla_1','demo','web','cid1','demo-web-1','nginx:latest','deploy',100,200,50,120,3,0)`); err != nil {
		raw.Close()
		t.Fatalf("insert into migrated container_log_archives: %v", err)
	}
	raw.Close()

	// Full Open on the upgraded DB must stay idempotent and preserve existing archive rows.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open after v22 migration: %v", err)
	}
	defer db.Close()
	var gotReason string
	if err := db.QueryRow(`SELECT reason FROM container_log_archives WHERE id='cla_1'`).Scan(&gotReason); err != nil {
		t.Fatalf("select preserved archive row: %v", err)
	}
	if gotReason != "deploy" {
		t.Errorf("reason = %q, want %q", gotReason, "deploy")
	}
}
