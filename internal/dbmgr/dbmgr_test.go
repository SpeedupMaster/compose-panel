package dbmgr

import (
	"context"
	"strings"
	"testing"
)

func TestPostgresMajorFromImage(t *testing.T) {
	for in, want := range map[string]string{
		"postgres:18-alpine":                            "18",
		"postgres:17.2":                                 "17",
		"docker.io/library/postgres:16":                 "16",
		"pgvector/pgvector:pg17":                        "17",
		"pgvector/pgvector:0.8.0-pg18":                  "18",
		"postgis/postgis:17-3.5-alpine":                 "17",
		"timescale/timescaledb:2.17.2-pg16":             "16",
		"registry.local:5000/team/pg:15-ext":            "15",
		"postgres:latest":                               "",
		"postgres":                                      "",
		"registry.local:5000/postgres":                  "",
		"postgres:18@sha256:" + strings.Repeat("a", 64): "18",
	} {
		if got := PostgresMajorFromImage(in); got != want {
			t.Errorf("PostgresMajorFromImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidImage(t *testing.T) {
	for in, want := range map[string]bool{
		"pgvector/pgvector:pg17":                       true,
		"registry.example.com:5000/team/postgres:17.2": true,
		"postgres@sha256:" + strings.Repeat("0", 64):   true,
		"Postgres:17":                       false,
		"postgres:17\n    privileged: true": false,
		"postgres:17 --flag":                false,
		"//evil":                            false,
		"-postgres":                         false,
		strings.Repeat("a", 256):            false,
	} {
		if got := ValidImage(in); got != want {
			t.Errorf("ValidImage(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestComposePostgresDataMount(t *testing.T) {
	cases := []struct{ version, image, wantImage, wantMount string }{
		{"17", "", "postgres:17-alpine", "data:/var/lib/postgresql/data\n"},
		{"18", "", "postgres:18-alpine", "data:/var/lib/postgresql\n"},
		{"18", "pgvector/pgvector:pg18", "pgvector/pgvector:pg18", "data:/var/lib/postgresql\n"},
		{"16", "postgis/postgis:16-3.5", "postgis/postgis:16-3.5", "data:/var/lib/postgresql/data\n"},
	}
	for _, c := range cases {
		out, err := Compose("postgres", c.version, c.image, "pg", "internal", "dbnet", 5432, "512m")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "    image: "+c.wantImage+"\n") || !strings.Contains(out, c.wantMount) {
			t.Errorf("version %s image %q:\n%s", c.version, c.image, out)
		}
		if strings.Count(out, "image:") != 1 {
			t.Errorf("expected exactly one image line:\n%s", out)
		}
	}
}

func TestComposeDefaultImages(t *testing.T) {
	for _, e := range Engines {
		out, err := Compose(e.ID, e.Versions[0], "", "x", "internal", "dbnet", e.Port, "")
		if err != nil {
			t.Fatal(err)
		}
		if want := "    image: " + e.DefaultImage(e.Versions[0]) + "\n"; !strings.Contains(out, want) || strings.Count(out, "image:") != 1 {
			t.Errorf("%s: want %q once in\n%s", e.ID, want, out)
		}
	}
	if _, err := Compose("oracle", "1", "", "x", "internal", "dbnet", 1, ""); err == nil {
		t.Error("unknown engine accepted")
	}
}

func TestMongoRestoreCmdNamespaceMapping(t *testing.T) {
	cl := Client{Engine: "mongo", Container: "mongo-1", Password: "pw"}

	// Explicit sourceDB maps sourceDB.* -> target.*
	cmd, err := cl.RestoreCmd("olddb", "newdb", true)
	if err != nil {
		t.Fatalf("RestoreCmd explicit sourceDB: %v", err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--nsFrom olddb.*") || !strings.Contains(joined, "--nsTo newdb.*") || !strings.Contains(joined, "--drop") {
		t.Fatalf("unexpected explicit mongo restore args: %s", joined)
	}

	// Unknown/empty sourceDB maps $db$.$coll$ -> target.$coll$
	cmd2, err := cl.RestoreCmd("", "imported_db", false)
	if err != nil {
		t.Fatalf("RestoreCmd empty sourceDB: %v", err)
	}
	joined2 := strings.Join(cmd2.Args, " ")
	if !strings.Contains(joined2, "--nsFrom $db$.$coll$") || !strings.Contains(joined2, "--nsTo imported_db.$coll$") {
		t.Fatalf("unexpected wildcard mongo restore args: %s", joined2)
	}
}

func TestPostgresUserSchemasQueryEscapesPGPrefix(t *testing.T) {
	if strings.Contains(pgUserSchemasQuery, "LIKE 'pg_%'") {
		t.Fatalf("pgUserSchemasQuery uses unescaped LIKE 'pg_%%' wildcard: %s", pgUserSchemasQuery)
	}
	if !strings.Contains(pgUserSchemasQuery, `!~ '^pg_'`) {
		t.Fatalf("expected regex ^pg_ filter in pgUserSchemasQuery, got: %s", pgUserSchemasQuery)
	}
}

func TestDangerGuardrail(t *testing.T) {
	hasReason := func(reasons []string, want string) bool {
		for _, r := range reasons {
			if r == want {
				return true
			}
		}
		return false
	}

	cases := []struct {
		name   string
		sql    string
		engine string // "" uses Danger(), otherwise DangerForEngine()
		want   []string
	}{
		{
			name: "plain drop",
			sql:  "DROP TABLE users;",
			want: []string{"包含 DROP 语句"},
		},
		{
			name: "plain truncate",
			sql:  "TRUNCATE TABLE logs;",
			want: []string{"包含 TRUNCATE 语句"},
		},
		{
			name: "delete without where",
			sql:  "delete from t",
			want: []string{"DELETE 没有 WHERE 条件，将删除整表数据"},
		},
		{
			name: "delete with top-level where",
			sql:  "DELETE FROM t WHERE id = 1",
		},
		{
			name: "update without where",
			sql:  "UPDATE t SET a = 1",
			want: []string{"UPDATE 没有 WHERE 条件，将修改整表数据"},
		},
		{
			name: "update with top-level where",
			sql:  "UPDATE t SET a = 1 WHERE id = 2",
		},
		{
			name: "alter table drop column",
			sql:  "ALTER TABLE users DROP COLUMN bio",
			want: []string{"ALTER TABLE 包含 DROP"},
		},
		{
			name: "alter table add drop_date is safe",
			sql:  `ALTER TABLE users ADD COLUMN drop_date text; ALTER TABLE users RENAME COLUMN "drop" TO old_drop;`,
		},
		{
			name: "comment with apostrophe before drop",
			sql:  "-- don't forget\nDELETE FROM t; -- '",
			want: []string{"DELETE 没有 WHERE 条件，将删除整表数据"},
		},
		{
			name: "hash comment with quote before drop",
			sql:  "# it's a comment\nDROP TABLE users; # '",
			want: []string{"包含 DROP 语句"},
		},
		{
			name: "block comment with quote before truncate",
			sql:  "/* ' */ TRUNCATE t; /* ' */",
			want: []string{"包含 TRUNCATE 语句"},
		},
		{
			name: "keywords inside string literals ignored",
			sql:  "SELECT '-- not comment; DROP TABLE x', '/* TRUNCATE t */', $$DELETE FROM t$$, $tag$UPDATE t SET a=1$tag$",
		},
		{
			name: "backslash escaped quote followed by drop",
			sql:  `SELECT 'a\''; DROP TABLE users; -- '`,
			want: []string{"包含 DROP 语句"},
		},
		{
			name: "backslash escaped quote inside benign string",
			sql:  `SELECT 'don\'t; DROP TABLE fake'`,
		},
		{
			name:   "postgres plain string backslash does not escape quote",
			engine: "postgres",
			sql:    `SELECT 'a\'; DROP TABLE users;`,
			want:   []string{"包含 DROP 语句"},
		},
		{
			name: "with cte followed by delete without where",
			sql:  "WITH c AS (SELECT id FROM u WHERE active = 0) DELETE FROM t",
			want: []string{"DELETE 没有 WHERE 条件，将删除整表数据"},
		},
		{
			name: "with cte followed by delete with where",
			sql:  "WITH c AS (SELECT id FROM u WHERE active = 0) DELETE FROM t WHERE id IN (SELECT id FROM c)",
		},
		{
			name: "with cte followed by update without where",
			sql:  "WITH c AS (SELECT 1) UPDATE t SET x = 2",
			want: []string{"UPDATE 没有 WHERE 条件，将修改整表数据"},
		},
		{
			name: "with data-modifying delete cte without where",
			sql:  "WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d WHERE id = 1",
			want: []string{"DELETE 没有 WHERE 条件，将删除整表数据"},
		},
		{
			name: "with data-modifying update cte without where",
			sql:  "WITH u AS (UPDATE t SET a = 1 RETURNING *) SELECT * FROM u WHERE id = 1",
			want: []string{"UPDATE 没有 WHERE 条件，将修改整表数据"},
		},
		{
			name: "with data-modifying delete cte with where",
			sql:  "WITH d AS (DELETE FROM t WHERE id = 1 RETURNING *) SELECT * FROM d",
		},
		{
			name: "update with where only inside subquery",
			sql:  "UPDATE t SET x = (SELECT y FROM u WHERE u.id = 1)",
			want: []string{"UPDATE 没有 WHERE 条件，将修改整表数据"},
		},
		{
			name: "update with subquery where and outer where",
			sql:  "UPDATE t SET x = (SELECT y FROM u WHERE u.id = 1) WHERE t.id = 2",
		},
		{
			name: "delete with where only inside subquery",
			sql:  "DELETE FROM t USING (SELECT id FROM u WHERE active = 0) s",
			want: []string{"DELETE 没有 WHERE 条件，将删除整表数据"},
		},
		{
			name: "delete with subquery where and outer where",
			sql:  "DELETE FROM t USING (SELECT id FROM u WHERE active = 0) s WHERE t.id = s.id",
		},
		{
			name: "mysql conditional comment drop",
			sql:  "/*!50000 DROP TABLE users */",
			want: []string{"包含 DROP 语句"},
		},
		{
			name: "mariadb conditional comment truncate",
			sql:  "/*M!100000 TRUNCATE TABLE logs */",
			want: []string{"包含 TRUNCATE 语句"},
		},
		{
			name: "explain analyze delete without where",
			sql:  "EXPLAIN ANALYZE DELETE FROM t",
			want: []string{"DELETE 没有 WHERE 条件，将删除整表数据"},
		},
		{
			name: "bare CR terminates dash line comment before drop",
			sql:  "-- x\rDROP TABLE users\nSELECT 1",
			want: []string{"包含 DROP 语句"},
		},
		{
			name: "bare CR terminates hash line comment before delete",
			sql:  "# x\rDELETE FROM users\nSELECT 1",
			want: []string{"DELETE 没有 WHERE 条件，将删除整表数据"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			if tc.engine != "" {
				got = DangerForEngine(tc.engine, tc.sql)
			} else {
				got = Danger(tc.sql)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Danger(%q) = %v, want %v", tc.sql, got, tc.want)
			}
			for _, w := range tc.want {
				if !hasReason(got, w) {
					t.Errorf("Danger(%q) = %v, missing %q", tc.sql, got, w)
				}
			}
		})
	}
}

func TestConsoleBlocksRCEAndReadOnlyBypass(t *testing.T) {
	// Using a Client with Docker == nil proves validation rejects dangerous SQL
	// BEFORE Docker.Exec is ever reached, while allowed SQL reaches the nil-docker check.
	blocked := []struct {
		name     string
		engine   string
		readOnly bool
		sql      string
	}{
		// PostgreSQL psql meta-command RCE
		{"pg shell bang", "postgres", false, `\! id`},
		{"pg shell bang after select", "postgres", true, `SELECT 1; \! id`},
		{"pg shell bang inline", "postgres", true, `SELECT 1 \! whoami`},
		{"pg include file", "postgres", false, `\i /etc/passwd`},
		{"pg output file", "postgres", false, `\o /tmp/pwn`},
		{"pg backslash copy", "postgres", false, `\copy users TO '/tmp/x'`},
		{"pg hash is not comment before bang", "postgres", true, "# \\! id\nSELECT 1"},
		{"pg plain string backslash does not escape quote", "postgres", true, `SELECT 'foo\' \! id`},
		{"pg backtick is not quote", "postgres", true, "SELECT `\\! id`"},
		{"pg bare CR escapes dash comment to bang", "postgres", true, "-- x\r\\! id\nSELECT 1"},
		{"pg bare CR escapes dash comment to drop", "postgres", true, "-- x\rDROP TABLE users;\nSELECT 1"},
		{"pg copy to program rw", "postgres", false, `COPY (SELECT 1) TO PROGRAM 'id'`},
		{"pg copy from program rw", "postgres", false, `COPY t FROM PROGRAM 'id'`},

		// MySQL / MariaDB client commands and file I/O
		{"mysql shell bang", "mysql", false, `\! id`},
		{"mysql source file", "mysql", false, `SELECT 1; \. /etc/passwd`},
		{"mysql use cmd", "mysql", true, `\u mysql`},
		{"mysql reconnect cmd", "mysql", true, `\r`},
		{"mysql dash without space is not comment", "mysql", true, "--\\! id\nSELECT 1"},
		{"mysql bare CR escapes dash comment to bang", "mysql", true, "-- x\r\\! id\nSELECT 1"},
		{"mysql bare CR escapes hash comment to drop", "mysql", true, "# x\rDROP TABLE users;\nSELECT 1"},
		{"mysql long-form source rw", "mysql", false, `source /etc/passwd`},
		{"mysql long-form source after stmt rw", "mysql", false, `SELECT 1; source /etc/passwd`},
		{"mysql long-form source at line start rw", "mysql", false, "SELECT 1\nsource /etc/passwd"},
		{"mysql long-form delimiter at line start rw", "mysql", false, "SELECT 1\n  delimiter //"},
		{"mysql long-form system rw", "mysql", false, `system id`},
		{"mysql long-form tee rw", "mysql", false, `tee /tmp/pwn`},
		{"mysql long-form notee rw", "mysql", false, `notee`},
		{"mysql long-form pager rw", "mysql", false, `pager sh -c id`},
		{"mysql long-form nopager rw", "mysql", false, `nopager`},
		{"mysql long-form edit rw", "mysql", false, `edit`},
		{"mysql long-form connect rw", "mysql", false, `connect mysql`},
		{"mariadb long-form rehash rw", "mariadb", false, `rehash`},
		{"mysql dollar is not quote", "mysql", true, `SELECT $a$ \! id $a$`},
		{"mysql exec comment bang", "mysql", true, `/*!50000 \! id */`},
		{"mysql load data rw", "mysql", false, `LOAD DATA INFILE '/etc/passwd' INTO TABLE t`},
		{"mysql into outfile rw", "mysql", false, `SELECT 1 INTO OUTFILE '/tmp/pwn'`},
		{"mariadb into dumpfile rw", "mariadb", false, `SELECT 1 INTO DUMPFILE '/tmp/pwn'`},

		// Strict read-only enforcement (PG & MySQL/MariaDB)
		{"pg ro set read write override", "postgres", true, `SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE; DELETE FROM t`},
		{"pg ro reset read only", "postgres", true, `RESET default_transaction_read_only; DROP TABLE t`},
		{"pg ro rollback escape", "postgres", true, `ROLLBACK; SET default_transaction_read_only = off; DELETE FROM t`},
		{"pg ro copy to program", "postgres", true, `COPY (SELECT 1) TO PROGRAM 'id'`},
		{"pg ro with modifying delete cte", "postgres", true, `WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d`},
		{"pg ro with modifying insert cte", "postgres", true, `WITH i AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM i`},
		{"pg ro explain analyze delete", "postgres", true, `EXPLAIN ANALYZE DELETE FROM t`},
		{"pg ro select into table", "postgres", true, `SELECT * INTO new_t FROM t`},
		{"pg ro select for update", "postgres", true, `SELECT * FROM t FOR UPDATE`},
		{"pg ro do block", "postgres", true, `DO $$ BEGIN PERFORM 1; END $$`},
		{"pg ro create table", "postgres", true, `CREATE TABLE x(id int)`},
		{"mysql ro implicit commit drop", "mysql", true, `DROP TABLE t`},
		{"mysql ro rollback set rw", "mysql", true, `ROLLBACK; SET SESSION TRANSACTION READ WRITE; DELETE FROM t`},
		{"mysql ro conditional comment drop", "mysql", true, `/*!50000 DROP TABLE t */`},
		{"mysql ro non-nested block comment escape", "mysql", true, `/* a /* b */ DROP TABLE t; */`},
		{"mysql ro replace into", "mysql", true, `REPLACE INTO t VALUES (1)`},
		{"mysql ro rename table", "mysql", true, `RENAME TABLE t TO t2`},
		{"mysql ro handler", "mysql", true, `HANDLER t OPEN`},
		{"mysql ro call", "mysql", true, `CALL evil_proc()`},
		{"mysql ro lock tables", "mysql", true, `LOCK TABLES t WRITE`},

		// Lexical errors
		{"pg unclosed string", "postgres", true, `SELECT 'unclosed`},
		{"pg unclosed dollar quote", "postgres", true, `SELECT $a$unclosed`},
		{"pg unclosed nested block comment", "postgres", true, `/* outer /* inner */ SELECT 1`},
		{"mysql unclosed block comment", "mysql", true, `/* unclosed`},
		{"only comment empty", "postgres", true, `-- nothing here`},
	}

	for _, tc := range blocked {
		t.Run("blocked/"+tc.name, func(t *testing.T) {
			cl := Client{Engine: tc.engine}
			_, _, err := cl.Console(context.Background(), "app", tc.sql, tc.readOnly, 100, 10)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if strings.Contains(err.Error(), "docker client is nil") {
				t.Fatalf("SQL was NOT blocked before Docker exec: %q", tc.sql)
			}
		})
	}

	allowed := []struct {
		name     string
		engine   string
		readOnly bool
		sql      string
	}{
		{"pg select count", "postgres", true, `select count(*) from t`},
		{"pg generate_series", "postgres", true, `select g from generate_series(1,5000) g`},
		{"pg backslash in plain string", "postgres", true, `SELECT 'hello\nworld'`},
		{"pg backslash in escape string", "postgres", true, `SELECT E'it\'s \\! fine'`},
		{"pg backslash in dollar string", "postgres", true, `SELECT $$ \! not a command $$`},
		{"pg backslash in line comment", "postgres", true, "-- \\! comment\nSELECT 1"},
		{"pg backslash in nested comment", "postgres", true, `/* outer /* \! inner */ still comment */ SELECT 1`},
		{"pg recursive cte", "postgres", true, `WITH RECURSIVE c(n) AS (VALUES (1) UNION ALL SELECT n+1 FROM c WHERE n < 5) SELECT * FROM c`},
		{"pg replace func and comment col and fetch", "postgres", true, `SELECT REPLACE(name, 'a', 'b'), comment FROM posts FETCH FIRST 10 ROWS ONLY`},
		{"pg explain select", "postgres", true, `EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM t`},
		{"pg values", "postgres", true, `VALUES (1, 'ok')`},
		{"pg table", "postgres", true, `TABLE users`},
		{"pg paren select", "postgres", true, `(SELECT 1)`},
		{"pg rw delete", "postgres", false, `DELETE FROM t WHERE id = 1`},
		{"mysql show tables", "mysql", true, `SHOW TABLES`},
		{"mysql describe", "mysql", true, `DESCRIBE users`},
		{"mysql desc", "mariadb", true, `DESC users`},
		{"mysql hash comment with bang", "mysql", true, "# \\! comment\nSELECT 1"},
		{"mysql dash space comment with bang", "mysql", true, "-- \\! comment\nSELECT 1"},
		{"mysql backslash in quoted string", "mysql", true, `SELECT 'it\'s \\! ok'`},
		{"mysql column named source allowed", "mysql", true, `SELECT source, system FROM logs WHERE source = 'web'`},
		{"mysql rw update", "mysql", false, `UPDATE t SET a = 1 WHERE id = 1`},
	}

	for _, tc := range allowed {
		t.Run("allowed/"+tc.name, func(t *testing.T) {
			cl := Client{Engine: tc.engine}
			_, _, err := cl.Console(context.Background(), "app", tc.sql, tc.readOnly, 100, 10)
			if err == nil || !strings.Contains(err.Error(), "docker client is nil") {
				t.Fatalf("valid SQL was unexpectedly rejected: %v (sql=%q)", err, tc.sql)
			}
		})
	}
}
