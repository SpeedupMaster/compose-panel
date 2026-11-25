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

// migrations add columns to tables created by earlier versions. Each runs
// once; "duplicate column" means it was already applied.
var migrations = []string{
	`ALTER TABLE sites ADD COLUMN maintenance INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN maintenance_msg TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN maintenance_deploy INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN access_log INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN load_balance INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN health_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN grpc INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE backups ADD COLUMN verified_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE backups ADD COLUMN origin TEXT NOT NULL DEFAULT 'backup'`,
	`ALTER TABLE db_instances ADD COLUMN image TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE users ADD COLUMN all_projects INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE users ADD COLUMN last_totp_step INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE deployments ADD COLUMN files_json TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE image_checks ADD COLUMN last_auto_digest TEXT NOT NULL DEFAULT ''`,
}

var indexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_projects_webhook ON projects(webhook_token)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_metrics_project_ts ON metrics(project, ts)`,
	`CREATE INDEX IF NOT EXISTS idx_backups_storage ON backups(storage_id)`,
	`CREATE INDEX IF NOT EXISTS idx_backups_started ON backups(started_at)`,
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
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate %q: %w", m, err)
		}
	}
	for _, idx := range indexes {
		if _, err := db.Exec(idx); err != nil {
			db.Close()
			return nil, fmt.Errorf("create index %q: %w", idx, err)
		}
	}
	if _, err := db.Exec(`UPDATE users SET all_projects=0 WHERE role<>'admin' AND id IN (SELECT DISTINCT user_id FROM user_projects)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("backfill user all_projects: %w", err)
	}
	return db, nil
}
