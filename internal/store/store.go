// Package store opens the SQLite database and applies the schema.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type migration struct {
	version int
	sql     string
}

// migrations is the ordered list of versioned schema migrations.
// Each migration executes at most once and is recorded atomically in schema_migrations.
var migrations = []migration{
	{1, `ALTER TABLE sites ADD COLUMN maintenance INTEGER NOT NULL DEFAULT 0`},
	{2, `ALTER TABLE sites ADD COLUMN maintenance_msg TEXT NOT NULL DEFAULT ''`},
	{3, `ALTER TABLE sites ADD COLUMN maintenance_deploy INTEGER NOT NULL DEFAULT 0`},
	{4, `ALTER TABLE sites ADD COLUMN access_log INTEGER NOT NULL DEFAULT 0`},
	{5, `ALTER TABLE sites ADD COLUMN load_balance INTEGER NOT NULL DEFAULT 0`},
	{6, `ALTER TABLE sites ADD COLUMN health_path TEXT NOT NULL DEFAULT ''`},
	{7, `ALTER TABLE sites ADD COLUMN grpc INTEGER NOT NULL DEFAULT 0`},
	{8, `ALTER TABLE backups ADD COLUMN verified_at INTEGER NOT NULL DEFAULT 0`},
	{9, `ALTER TABLE backups ADD COLUMN origin TEXT NOT NULL DEFAULT 'backup'`},
	{10, `ALTER TABLE db_instances ADD COLUMN image TEXT NOT NULL DEFAULT ''`},
	{11, `ALTER TABLE users ADD COLUMN all_projects INTEGER NOT NULL DEFAULT 1`},
	{12, `ALTER TABLE users ADD COLUMN last_totp_step INTEGER NOT NULL DEFAULT 0`},
	{13, `ALTER TABLE deployments ADD COLUMN files_json TEXT NOT NULL DEFAULT ''`},
	{14, `ALTER TABLE image_checks ADD COLUMN last_auto_digest TEXT NOT NULL DEFAULT ''`},
	{15, `CREATE INDEX IF NOT EXISTS idx_projects_webhook ON projects(webhook_token)`},
	{16, `CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`},
	{17, `CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at)`},
	{18, `CREATE INDEX IF NOT EXISTS idx_metrics_project_ts ON metrics(project, ts)`},
	{19, `CREATE INDEX IF NOT EXISTS idx_backups_storage ON backups(storage_id)`},
	{20, `CREATE INDEX IF NOT EXISTS idx_backups_started ON backups(started_at)`},
	{21, `UPDATE users SET all_projects=0 WHERE role<>'admin' AND id IN (SELECT DISTINCT user_id FROM user_projects)`},
	{22, `CREATE TABLE IF NOT EXISTS container_log_archives (
  id             TEXT PRIMARY KEY,
  project        TEXT NOT NULL DEFAULT '',
  service        TEXT NOT NULL DEFAULT '',
  container_id   TEXT NOT NULL,
  container_name TEXT NOT NULL,
  image          TEXT NOT NULL DEFAULT '',
  reason         TEXT NOT NULL,
  started_at     INTEGER NOT NULL DEFAULT 0,
  archived_at    INTEGER NOT NULL,
  size_bytes     INTEGER NOT NULL DEFAULT 0,
  raw_bytes      INTEGER NOT NULL DEFAULT 0,
  line_count     INTEGER NOT NULL DEFAULT 0,
  truncated      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_cla_project_service ON container_log_archives(project, service, archived_at DESC);
CREATE INDEX IF NOT EXISTS idx_cla_name ON container_log_archives(container_name, archived_at DESC);
CREATE INDEX IF NOT EXISTS idx_cla_archived_at ON container_log_archives(archived_at);`},
}

// Open opens (and creates if needed) the panel database in WAL mode.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-1000)&_pragma=journal_size_limit(4194304)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(2 * time.Minute)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := applyMigrations(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenReadOnly opens an existing SQLite database in query-only mode without running migrations.
func OpenReadOnly(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func applyMigrations(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	var maxApplied int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&maxApplied); err != nil {
		return fmt.Errorf("read schema_migrations version: %w", err)
	}
	for _, m := range migrations {
		if m.version <= maxApplied {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration v%d: %w", m.version, err)
		}
		if _, err := tx.Exec(m.sql); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = tx.Rollback()
			return fmt.Errorf("migrate v%d (%q): %w", m.version, m.sql, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, m.version, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration v%d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v%d: %w", m.version, err)
		}
	}
	return nil
}
