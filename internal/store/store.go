// Package store opens the SQLite database and applies the schema.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

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
	`ALTER TABLE backups ADD COLUMN verified_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE db_instances ADD COLUMN image TEXT NOT NULL DEFAULT ''`,
}

// Open opens (and creates if needed) the panel database in WAL mode.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
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
	return db, nil
}
