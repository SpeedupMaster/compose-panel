// Package dbmgr knows how to provision and operate database engines through docker exec.
package dbmgr

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type Engine struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Versions []string `json:"versions"`
	Port     int      `json:"port"`
	RootUser string   `json:"rootUser"`
	Tools    string   `json:"tools"`
	SQL      bool     `json:"sql"`
	DumpExt  string   `json:"dumpExt"`
}

var Engines = []Engine{
	{ID: "postgres", Name: "PostgreSQL", Versions: []string{"17", "16", "15"}, Port: 5432, RootUser: "postgres", Tools: "pg_dump · psql", SQL: true, DumpExt: "pgdump"},
	{ID: "mysql", Name: "MySQL", Versions: []string{"8.4", "8.0"}, Port: 3306, RootUser: "root", Tools: "mysqldump · mysql", SQL: true, DumpExt: "sql"},
	{ID: "mariadb", Name: "MariaDB", Versions: []string{"11.4", "10.11"}, Port: 3306, RootUser: "root", Tools: "mariadb-dump", SQL: true, DumpExt: "sql"},
	{ID: "mongo", Name: "MongoDB", Versions: []string{"8.0", "7.0"}, Port: 27017, RootUser: "root", Tools: "mongodump · mongosh", DumpExt: "archive"},
	{ID: "redis", Name: "Redis", Versions: []string{"7.4", "7.2"}, Port: 6379, RootUser: "default", Tools: "RDB 快照 · redis-cli", DumpExt: "rdb"},
}

func EngineByID(id string) (Engine, bool) {
	for _, e := range Engines {
		if e.ID == id {
			return e, true
		}
	}
	return Engine{}, false
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

func ValidIdent(s string) bool { return identRe.MatchString(s) }

// Compose renders the compose file for a new instance. Secrets come from .env.
func Compose(engine, version, alias, exposure, dbNetwork string, port int, memLimit string) (string, error) {
	var b strings.Builder
	b.WriteString("services:\n  db:\n")
	switch engine {
	case "postgres":
		fmt.Fprintf(&b, "    image: postgres:%s-alpine\n", version)
		// pg_stat_statements powers the slow-query view; it ships with the image.
		b.WriteString("    command: [\"postgres\", \"-c\", \"shared_preload_libraries=pg_stat_statements\"]\n")
		b.WriteString("    environment:\n      POSTGRES_PASSWORD: ${DB_ROOT_PASSWORD}\n")
		b.WriteString("    volumes:\n      - data:/var/lib/postgresql/data\n")
		b.WriteString("    healthcheck:\n      test: [\"CMD-SHELL\", \"pg_isready -U postgres\"]\n      interval: 10s\n      timeout: 5s\n      retries: 5\n")
	case "mysql":
		fmt.Fprintf(&b, "    image: mysql:%s\n", version)
		b.WriteString("    environment:\n      MYSQL_ROOT_PASSWORD: ${DB_ROOT_PASSWORD}\n")
		b.WriteString("    volumes:\n      - data:/var/lib/mysql\n")
		b.WriteString("    healthcheck:\n      test: [\"CMD-SHELL\", \"mysqladmin ping -h 127.0.0.1 -uroot -p$$MYSQL_ROOT_PASSWORD --silent\"]\n      interval: 10s\n      timeout: 5s\n      retries: 10\n")
	case "mariadb":
		fmt.Fprintf(&b, "    image: mariadb:%s\n", version)
		b.WriteString("    environment:\n      MARIADB_ROOT_PASSWORD: ${DB_ROOT_PASSWORD}\n")
		b.WriteString("    volumes:\n      - data:/var/lib/mysql\n")
		b.WriteString("    healthcheck:\n      test: [\"CMD\", \"healthcheck.sh\", \"--connect\", \"--innodb_initialized\"]\n      interval: 10s\n      timeout: 5s\n      retries: 10\n")
	case "mongo":
		fmt.Fprintf(&b, "    image: mongo:%s\n", version)
		b.WriteString("    environment:\n      MONGO_INITDB_ROOT_USERNAME: root\n      MONGO_INITDB_ROOT_PASSWORD: ${DB_ROOT_PASSWORD}\n")
		b.WriteString("    volumes:\n      - data:/data/db\n")
	case "redis":
		fmt.Fprintf(&b, "    image: redis:%s-alpine\n", version)
		b.WriteString("    command: [\"redis-server\", \"--appendonly\", \"yes\", \"--requirepass\", \"${DB_ROOT_PASSWORD}\"]\n")
		b.WriteString("    volumes:\n      - data:/data\n")
		b.WriteString("    healthcheck:\n      test: [\"CMD-SHELL\", \"redis-cli -a \\\"$$REDIS_PASSWORD\\\" --no-auth-warning ping | grep PONG\"]\n      interval: 10s\n      timeout: 5s\n      retries: 5\n")
		b.WriteString("    environment:\n      REDIS_PASSWORD: ${DB_ROOT_PASSWORD}\n")
	default:
		return "", fmt.Errorf("unknown engine %q", engine)
	}
	b.WriteString("    restart: unless-stopped\n")
	if memLimit != "" {
		fmt.Fprintf(&b, "    mem_limit: %s\n", memLimit)
	}
	switch exposure {
	case "local":
		fmt.Fprintf(&b, "    ports:\n      - \"127.0.0.1:%d:%d\"\n", port, port)
	case "public":
		fmt.Fprintf(&b, "    ports:\n      - \"%d:%d\"\n", port, port)
	}
	fmt.Fprintf(&b, "    networks:\n      default: {}\n      shared:\n        aliases: [%q]\n", alias)
	fmt.Fprintf(&b, "volumes:\n  data: {}\nnetworks:\n  shared:\n    external: true\n    name: %q\n", dbNetwork)
	return b.String(), nil
}

// Exec runs a command inside a container through the docker CLI.
type Exec struct {
	Container string
	Env       []string // passed with -e
}

func (e Exec) Command(ctx context.Context, stdin bool, args ...string) *exec.Cmd {
	a := []string{"exec"}
	if stdin {
		a = append(a, "-i")
	}
	for _, kv := range e.Env {
		a = append(a, "-e", kv)
	}
	a = append(a, e.Container)
	a = append(a, args...)
	return exec.CommandContext(ctx, "docker", a...)
}

func (e Exec) Output(ctx context.Context, stdin string, args ...string) (string, error) {
	c := e.Command(ctx, stdin != "", args...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("%s", msg)
	}
	return out.String(), nil
}

// Client operates one database instance.
type Client struct {
	Engine    string
	Container string
	Password  string
}

func (c Client) exec() Exec {
	switch c.Engine {
	case "mysql", "mariadb":
		return Exec{Container: c.Container, Env: []string{"MYSQL_PWD=" + c.Password}}
	case "postgres":
		return Exec{Container: c.Container, Env: []string{"PGPASSWORD=" + c.Password}}
	}
	return Exec{Container: c.Container}
}

func (c Client) mysqlBin() string {
	if c.Engine == "mariadb" {
		return "mariadb"
	}
	return "mysql"
}

// Result is a tabular query result.
type Result struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

// Query runs SQL and returns the last result set. readOnly wraps it in a read-only transaction.
func (c Client) Query(ctx context.Context, database, sql string, readOnly bool) (*Result, error) {
	switch c.Engine {
	case "postgres":
		input := sql
		if readOnly {
			input = "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY;\n" + sql
		}
		out, err := c.exec().Output(ctx, input, "psql", "-U", "postgres", "-d", database, "-X", "-q", "--csv", "-v", "ON_ERROR_STOP=1", "-P", "pager=off")
		if err != nil {
			return nil, err
		}
		return parseCSV(out, ',')
	case "mysql", "mariadb":
		input := sql
		if readOnly {
			input = "SET SESSION TRANSACTION READ ONLY;\nSTART TRANSACTION;\n" + strings.TrimRight(strings.TrimSpace(sql), ";") + ";\nROLLBACK;\n"
		}
		out, err := c.exec().Output(ctx, input, c.mysqlBin(), "-uroot", "--batch", "--raw", "-D", database)
		if err != nil {
			return nil, err
		}
		return parseTSV(out), nil
	}
	return nil, fmt.Errorf("%s 不支持 SQL 控制台", c.Engine)
}

func parseCSV(s string, sep rune) (*Result, error) {
	r := csv.NewReader(strings.NewReader(s))
	r.Comma = sep
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	res := &Result{Columns: []string{}, Rows: [][]string{}}
	if len(recs) == 0 {
		return res, nil
	}
	res.Columns = recs[0]
	res.Rows = recs[1:]
	return res, nil
}

func parseTSV(s string) *Result {
	res := &Result{Columns: []string{}, Rows: [][]string{}}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return res
	}
	res.Columns = strings.Split(lines[0], "\t")
	for _, l := range lines[1:] {
		res.Rows = append(res.Rows, strings.Split(l, "\t"))
	}
	return res
}

func (c Client) rows(ctx context.Context, database, sql string) ([][]string, error) {
	r, err := c.Query(ctx, database, sql, false)
	if err != nil {
		return nil, err
	}
	return r.Rows, nil
}

type Database struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Size  int64  `json:"size"`
}

func (c Client) adminDB() string {
	if c.Engine == "postgres" {
		return "postgres"
	}
	return "mysql"
}

func (c Client) ListDatabases(ctx context.Context) ([]Database, error) {
	var out []Database
	switch c.Engine {
	case "postgres":
		rows, err := c.rows(ctx, "postgres", `SELECT datname, pg_get_userbyid(datdba), pg_database_size(datname) FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres' ORDER BY 1`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			size, _ := strconv.ParseInt(r[2], 10, 64)
			out = append(out, Database{Name: r[0], Owner: r[1], Size: size})
		}
	case "mysql", "mariadb":
		rows, err := c.rows(ctx, "mysql", `SELECT s.schema_name, COALESCE(SUM(t.data_length + t.index_length), 0) FROM information_schema.schemata s LEFT JOIN information_schema.tables t ON t.table_schema = s.schema_name WHERE s.schema_name NOT IN ('mysql','information_schema','performance_schema','sys') GROUP BY s.schema_name ORDER BY 1`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			size, _ := strconv.ParseFloat(r[1], 64)
			out = append(out, Database{Name: r[0], Size: int64(size)})
		}
	case "mongo":
		s, err := c.exec().Output(ctx, "", "mongosh", "--quiet", "-u", "root", "-p", c.Password, "--authenticationDatabase", "admin", "--eval",
			`JSON.stringify(db.adminCommand({listDatabases:1}).databases)`)
		if err != nil {
			return nil, err
		}
		var dbs []struct {
			Name       string  `json:"name"`
			SizeOnDisk float64 `json:"sizeOnDisk"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &dbs); err != nil {
			return nil, err
		}
		for _, d := range dbs {
			if d.Name == "admin" || d.Name == "local" || d.Name == "config" {
				continue
			}
			out = append(out, Database{Name: d.Name, Size: int64(d.SizeOnDisk)})
		}
	case "redis":
		s, err := c.exec().Output(ctx, "", "redis-cli", "-a", c.Password, "--no-auth-warning", "INFO", "keyspace")
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(s, "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "db") {
				name, _, _ := strings.Cut(l, ":")
				out = append(out, Database{Name: name})
			}
		}
	}
	return out, nil
}

// CreateDatabase creates a database and, when user is set, an owner with that password.
func (c Client) CreateDatabase(ctx context.Context, name, user, password string) error {
	if !ValidIdent(name) || (user != "" && !ValidIdent(user)) {
		return fmt.Errorf("名称只能包含字母、数字和下划线，且不能以数字开头")
	}
	switch c.Engine {
	case "postgres":
		if user != "" {
			if _, err := c.rows(ctx, "postgres", fmt.Sprintf(`CREATE ROLE "%s" LOGIN PASSWORD '%s'`, user, password)); err != nil && !strings.Contains(err.Error(), "already exists") {
				return err
			}
			_, err := c.rows(ctx, "postgres", fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, name, user))
			return err
		}
		_, err := c.rows(ctx, "postgres", fmt.Sprintf(`CREATE DATABASE "%s"`, name))
		return err
	case "mysql", "mariadb":
		sql := fmt.Sprintf("CREATE DATABASE `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;", name)
		if user != "" {
			sql += fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'; GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%'; FLUSH PRIVILEGES;", user, password, name, user)
		}
		_, err := c.rows(ctx, "mysql", sql)
		return err
	}
	return fmt.Errorf("%s 不支持在面板中创建库", c.Engine)
}

func (c Client) DropDatabase(ctx context.Context, name string) error {
	if !ValidIdent(name) {
		return fmt.Errorf("invalid name")
	}
	switch c.Engine {
	case "postgres":
		_, err := c.rows(ctx, "postgres", fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, name))
		return err
	case "mysql", "mariadb":
		_, err := c.rows(ctx, "mysql", fmt.Sprintf("DROP DATABASE `%s`", name))
		return err
	}
	return fmt.Errorf("不支持")
}

type DBUser struct {
	Name      string `json:"name"`
	Host      string `json:"host,omitempty"`
	Superuser bool   `json:"superuser"`
	Databases string `json:"databases"`
}

func (c Client) ListUsers(ctx context.Context) ([]DBUser, error) {
	var out []DBUser
	switch c.Engine {
	case "postgres":
		rows, err := c.rows(ctx, "postgres", `SELECT r.rolname, r.rolsuper, COALESCE((SELECT string_agg(datname, ', ') FROM pg_database d WHERE d.datdba = r.oid AND NOT datistemplate), '') FROM pg_roles r WHERE r.rolcanlogin ORDER BY 1`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, DBUser{Name: r[0], Superuser: r[1] == "t", Databases: r[2]})
		}
	case "mysql", "mariadb":
		rows, err := c.rows(ctx, "mysql", `SELECT u.user, u.host, IF(u.super_priv='Y','t','f'), COALESCE((SELECT GROUP_CONCAT(DISTINCT db) FROM mysql.db d WHERE d.user=u.user AND d.host=u.host), '') FROM mysql.user u WHERE u.user NOT LIKE 'mysql.%' AND u.user <> 'mariadb.sys' ORDER BY 1`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, DBUser{Name: r[0], Host: r[1], Superuser: r[2] == "t", Databases: r[3]})
		}
	default:
		return nil, fmt.Errorf("%s 不支持用户管理", c.Engine)
	}
	return out, nil
}

// CreateUser creates a login and grants it access to databases (readOnly limits it to SELECT).
func (c Client) CreateUser(ctx context.Context, user, password string, databases []string, readOnly bool) error {
	if !ValidIdent(user) {
		return fmt.Errorf("用户名不合法")
	}
	for _, d := range databases {
		if !ValidIdent(d) {
			return fmt.Errorf("库名不合法")
		}
	}
	switch c.Engine {
	case "postgres":
		if _, err := c.rows(ctx, "postgres", fmt.Sprintf(`CREATE ROLE "%s" LOGIN PASSWORD '%s'`, user, password)); err != nil {
			return err
		}
		for _, d := range databases {
			grant := fmt.Sprintf(`GRANT CONNECT ON DATABASE "%s" TO "%s"; GRANT USAGE ON SCHEMA public TO "%s";`, d, user, user)
			if readOnly {
				grant += fmt.Sprintf(`GRANT SELECT ON ALL TABLES IN SCHEMA public TO "%s"; ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO "%s";`, user, user)
			} else {
				grant += fmt.Sprintf(`GRANT ALL ON ALL TABLES IN SCHEMA public TO "%s"; GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO "%s"; GRANT CREATE ON SCHEMA public TO "%s";`, user, user, user)
			}
			if _, err := c.rows(ctx, d, grant); err != nil {
				return err
			}
		}
		return nil
	case "mysql", "mariadb":
		sql := fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s';", user, password)
		priv := "ALL PRIVILEGES"
		if readOnly {
			priv = "SELECT, SHOW VIEW"
		}
		for _, d := range databases {
			sql += fmt.Sprintf("GRANT %s ON `%s`.* TO '%s'@'%%';", priv, d, user)
		}
		sql += "FLUSH PRIVILEGES;"
		_, err := c.rows(ctx, "mysql", sql)
		return err
	}
	return fmt.Errorf("不支持")
}

func (c Client) SetPassword(ctx context.Context, user, host, password string) error {
	if !ValidIdent(user) {
		return fmt.Errorf("用户名不合法")
	}
	switch c.Engine {
	case "postgres":
		_, err := c.rows(ctx, "postgres", fmt.Sprintf(`ALTER ROLE "%s" PASSWORD '%s'`, user, password))
		return err
	case "mysql", "mariadb":
		if host == "" {
			host = "%"
		}
		_, err := c.rows(ctx, "mysql", fmt.Sprintf("ALTER USER '%s'@'%s' IDENTIFIED BY '%s'; FLUSH PRIVILEGES;", user, strings.ReplaceAll(host, "'", ""), password))
		return err
	}
	return fmt.Errorf("不支持")
}

func (c Client) DropUser(ctx context.Context, user, host string) error {
	if !ValidIdent(user) {
		return fmt.Errorf("用户名不合法")
	}
	switch c.Engine {
	case "postgres":
		_, err := c.rows(ctx, "postgres", fmt.Sprintf(`DROP ROLE "%s"`, user))
		return err
	case "mysql", "mariadb":
		if host == "" {
			host = "%"
		}
		_, err := c.rows(ctx, "mysql", fmt.Sprintf("DROP USER '%s'@'%s'", user, strings.ReplaceAll(host, "'", "")))
		return err
	}
	return fmt.Errorf("不支持")
}

type Session struct {
	PID      string `json:"pid"`
	User     string `json:"user"`
	Database string `json:"database"`
	Client   string `json:"client"`
	State    string `json:"state"`
	Seconds  string `json:"seconds"`
	Query    string `json:"query"`
}

func (c Client) Sessions(ctx context.Context) ([]Session, error) {
	var out []Session
	switch c.Engine {
	case "postgres":
		rows, err := c.rows(ctx, "postgres", `SELECT pid, COALESCE(usename,''), COALESCE(datname,''), COALESCE(client_addr::text,'local'), COALESCE(state,''), COALESCE(EXTRACT(EPOCH FROM now()-COALESCE(query_start, backend_start))::int, 0), COALESCE(query,'') FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid() ORDER BY 6 DESC`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, Session{PID: r[0], User: r[1], Database: r[2], Client: r[3], State: r[4], Seconds: r[5], Query: r[6]})
		}
	case "mysql", "mariadb":
		rows, err := c.rows(ctx, "mysql", `SELECT id, user, COALESCE(db,''), host, command, time, COALESCE(info,'') FROM information_schema.processlist WHERE id <> CONNECTION_ID() AND user <> 'event_scheduler' ORDER BY time DESC`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, Session{PID: r[0], User: r[1], Database: r[2], Client: r[3], State: r[4], Seconds: r[5], Query: r[6]})
		}
	default:
		return nil, fmt.Errorf("%s 不支持会话查看", c.Engine)
	}
	return out, nil
}

func (c Client) KillSession(ctx context.Context, pid string) error {
	if _, err := strconv.Atoi(pid); err != nil {
		return fmt.Errorf("invalid pid")
	}
	switch c.Engine {
	case "postgres":
		_, err := c.rows(ctx, "postgres", "SELECT pg_terminate_backend("+pid+")")
		return err
	case "mysql", "mariadb":
		_, err := c.rows(ctx, "mysql", "KILL "+pid)
		return err
	}
	return fmt.Errorf("不支持")
}

type Table struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
	Size int64  `json:"size"`
}

func (c Client) Tables(ctx context.Context, database string) ([]Table, error) {
	var rows [][]string
	var err error
	switch c.Engine {
	case "postgres":
		rows, err = c.rows(ctx, database, `SELECT relname, GREATEST(reltuples,0)::bigint, pg_total_relation_size(c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind = 'r' AND n.nspname = 'public' ORDER BY 1`)
	case "mysql", "mariadb":
		rows, err = c.rows(ctx, database, `SELECT table_name, COALESCE(table_rows,0), COALESCE(data_length+index_length,0) FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY 1`)
	default:
		return nil, fmt.Errorf("不支持")
	}
	if err != nil {
		return nil, err
	}
	out := []Table{}
	for _, r := range rows {
		n, _ := strconv.ParseInt(r[1], 10, 64)
		s, _ := strconv.ParseInt(r[2], 10, 64)
		out = append(out, Table{Name: r[0], Rows: n, Size: s})
	}
	return out, nil
}

// Metrics returns a few headline numbers.
func (c Client) Metrics(ctx context.Context) map[string]string {
	m := map[string]string{}
	switch c.Engine {
	case "postgres":
		rows, err := c.rows(ctx, "postgres", `SELECT (SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend'), current_setting('max_connections'), COALESCE(round(100.0*sum(blks_hit)/NULLIF(sum(blks_hit)+sum(blks_read),0),1)::text,'—'), (SELECT COALESCE(max(EXTRACT(EPOCH FROM now()-xact_start))::int,0) FROM pg_stat_activity WHERE xact_start IS NOT NULL), version() FROM pg_stat_database`)
		if err == nil && len(rows) > 0 {
			m["connections"], m["maxConnections"], m["cacheHit"], m["longestTx"], m["version"] = rows[0][0], rows[0][1], rows[0][2], rows[0][3], rows[0][4]
		}
	case "mysql", "mariadb":
		rows, err := c.rows(ctx, "mysql", `SELECT (SELECT count(*) FROM information_schema.processlist), @@max_connections, VERSION()`)
		if err == nil && len(rows) > 0 {
			m["connections"], m["maxConnections"], m["version"] = rows[0][0], rows[0][1], rows[0][2]
		}
	}
	return m
}

// DumpCmd returns the command that writes a logical backup of database to stdout.
func (c Client) DumpCmd(ctx context.Context, database string) (*exec.Cmd, string, error) {
	e := c.exec()
	switch c.Engine {
	case "postgres":
		return e.Command(ctx, false, "pg_dump", "-U", "postgres", "-Fc", "-Z", "0", database), "pgdump", nil
	case "mysql":
		return e.Command(ctx, false, "mysqldump", "-uroot", "--single-transaction", "--routines", "--triggers", "--events", "--set-gtid-purged=OFF", database), "sql", nil
	case "mariadb":
		return e.Command(ctx, false, "mariadb-dump", "-uroot", "--single-transaction", "--routines", "--triggers", "--events", database), "sql", nil
	case "mongo":
		return e.Command(ctx, false, "mongodump", "--archive", "-u", "root", "-p", c.Password, "--authenticationDatabase", "admin", "--db", database), "archive", nil
	case "redis":
		return e.Command(ctx, false, "sh", "-c", `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning --rdb /tmp/panel-dump.rdb >/dev/null && cat /tmp/panel-dump.rdb && rm -f /tmp/panel-dump.rdb`), "rdb", nil
	}
	return nil, "", fmt.Errorf("unknown engine")
}

// RestoreCmd returns the command that reads a dump from stdin into database.
func (c Client) RestoreCmd(ctx context.Context, sourceDB, database string, clean bool) (*exec.Cmd, error) {
	e := c.exec()
	switch c.Engine {
	case "postgres":
		args := []string{"pg_restore", "-U", "postgres", "-d", database, "--no-owner", "--no-privileges"}
		if clean {
			args = append(args, "--clean", "--if-exists")
		}
		return e.Command(ctx, true, args...), nil
	case "mysql", "mariadb":
		return e.Command(ctx, true, c.mysqlBin(), "-uroot", database), nil
	case "mongo":
		args := []string{"mongorestore", "--archive", "-u", "root", "-p", c.Password, "--authenticationDatabase", "admin",
			"--nsFrom", sourceDB + ".*", "--nsTo", database + ".*"}
		if clean {
			args = append(args, "--drop")
		}
		return e.Command(ctx, true, args...), nil
	}
	return nil, fmt.Errorf("%s 暂不支持在面板中恢复，请下载备份手动恢复", c.Engine)
}

// Wait runs cmd with stdin from r, returning stderr text on failure.
func RunWithInput(cmd *exec.Cmd, r io.Reader, log io.Writer) error {
	cmd.Stdin = r
	var errb bytes.Buffer
	cmd.Stdout = log
	cmd.Stderr = io.MultiWriter(&errb, log)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
