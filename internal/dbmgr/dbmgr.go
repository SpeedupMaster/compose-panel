// Package dbmgr knows how to provision and operate database engines through Docker Engine API exec.
package dbmgr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"compose-panel/internal/docker"
)

type Engine struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Versions []string `json:"versions"`
	Image    string   `json:"image"` // official image; {v} is replaced by the version
	Port     int      `json:"port"`
	RootUser string   `json:"rootUser"`
	Tools    string   `json:"tools"`
	SQL      bool     `json:"sql"`
	DumpExt  string   `json:"dumpExt"`
}

var Engines = []Engine{
	{ID: "postgres", Name: "PostgreSQL", Versions: []string{"18", "17", "16", "15"}, Image: "postgres:{v}-alpine", Port: 5432, RootUser: "postgres", Tools: "pg_dump · psql", SQL: true, DumpExt: "pgdump"},
	{ID: "mysql", Name: "MySQL", Versions: []string{"8.4", "8.0"}, Image: "mysql:{v}", Port: 3306, RootUser: "root", Tools: "mysqldump · mysql", SQL: true, DumpExt: "sql"},
	{ID: "mariadb", Name: "MariaDB", Versions: []string{"11.4", "10.11"}, Image: "mariadb:{v}", Port: 3306, RootUser: "root", Tools: "mariadb-dump", SQL: true, DumpExt: "sql"},
	{ID: "mongo", Name: "MongoDB", Versions: []string{"8.0", "7.0"}, Image: "mongo:{v}", Port: 27017, RootUser: "root", Tools: "mongodump · mongosh", DumpExt: "archive"},
	{ID: "redis", Name: "Redis", Versions: []string{"7.4", "7.2"}, Image: "redis:{v}-alpine", Port: 6379, RootUser: "default", Tools: "RDB 快照 · redis-cli", DumpExt: "rdb"},
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

var hostRe = regexp.MustCompile(`^[a-zA-Z0-9._%:-]{1,255}$`)

// ValidHost validates a MySQL/MariaDB user host pattern (e.g. %, localhost, 10.0.0.%, ::1).
func ValidHost(s string) bool { return hostRe.MatchString(s) }

func mysqlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `''`)
	return "'" + r.Replace(s) + "'"
}

// DefaultImage is the official image for this engine at version.
func (e Engine) DefaultImage(version string) string {
	return strings.ReplaceAll(e.Image, "{v}", version)
}

var imageRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*(@sha256:[a-f0-9]{64})?$`)

// ValidImage accepts docker image references (registry/repo:tag or @sha256 digest).
func ValidImage(s string) bool {
	return len(s) <= 255 && imageRe.MatchString(s) && !strings.Contains(s, "//")
}

var (
	pgTagRe      = regexp.MustCompile(`(?i)pg-?(\d{2})(?:\D|$)`)
	leadingNumRe = regexp.MustCompile(`^(\d{2})(?:\D|$)`)
)

// PostgresMajorFromImage reads the PostgreSQL major version from an image tag:
// postgres:18-alpine, pgvector/pgvector:pg17, postgis/postgis:17-3.5,
// timescale/timescaledb:2.17.2-pg16. "" when the tag doesn't say (e.g. latest).
func PostgresMajorFromImage(image string) string {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || i < strings.LastIndex(ref, "/") {
		return ""
	}
	tag := ref[i+1:]
	if m := pgTagRe.FindStringSubmatch(tag); m != nil {
		return m[1]
	}
	if m := leadingNumRe.FindStringSubmatch(tag); m != nil {
		return m[1]
	}
	return ""
}

// PostgresDataMount is where the data volume is mounted. From 18 on the official image
// keeps PGDATA in /var/lib/postgresql/<major>/docker and declares /var/lib/postgresql as
// its volume; it refuses to start with a volume on the old /var/lib/postgresql/data path.
func PostgresDataMount(version string) string {
	if major, err := strconv.Atoi(strings.SplitN(version, ".", 2)[0]); err == nil && major >= 18 {
		return "/var/lib/postgresql"
	}
	return "/var/lib/postgresql/data"
}

// Compose renders the compose file for an instance. Secrets come from .env.
// image overrides the official image (e.g. pgvector/pgvector:pg17); "" uses the default.
func Compose(engine, version, image, alias, exposure, dbNetwork string, port int, memLimit string) (string, error) {
	eng, ok := EngineByID(engine)
	if !ok {
		return "", fmt.Errorf("unknown engine %q", engine)
	}
	if image == "" {
		image = eng.DefaultImage(version)
	}
	var b strings.Builder
	b.WriteString("services:\n  db:\n")
	fmt.Fprintf(&b, "    image: %s\n", image)
	switch engine {
	case "postgres":
		// pg_stat_statements powers the slow-query view; it ships with the image.
		b.WriteString("    command: [\"postgres\", \"-c\", \"shared_preload_libraries=pg_stat_statements\"]\n")
		b.WriteString("    environment:\n      POSTGRES_PASSWORD: ${DB_ROOT_PASSWORD}\n")
		fmt.Fprintf(&b, "    volumes:\n      - data:%s\n", PostgresDataMount(version))
		b.WriteString("    healthcheck:\n      test: [\"CMD-SHELL\", \"pg_isready -U postgres\"]\n      interval: 10s\n      timeout: 5s\n      retries: 5\n")
	case "mysql":
		b.WriteString("    environment:\n      MYSQL_ROOT_PASSWORD: ${DB_ROOT_PASSWORD}\n")
		b.WriteString("    volumes:\n      - data:/var/lib/mysql\n")
		b.WriteString("    healthcheck:\n      test: [\"CMD-SHELL\", \"mysqladmin ping -h 127.0.0.1 -uroot -p$$MYSQL_ROOT_PASSWORD --silent\"]\n      interval: 10s\n      timeout: 5s\n      retries: 10\n")
	case "mariadb":
		b.WriteString("    environment:\n      MARIADB_ROOT_PASSWORD: ${DB_ROOT_PASSWORD}\n")
		b.WriteString("    volumes:\n      - data:/var/lib/mysql\n")
		b.WriteString("    healthcheck:\n      test: [\"CMD\", \"healthcheck.sh\", \"--connect\", \"--innodb_initialized\"]\n      interval: 10s\n      timeout: 5s\n      retries: 10\n")
	case "mongo":
		b.WriteString("    environment:\n      MONGO_INITDB_ROOT_USERNAME: root\n      MONGO_INITDB_ROOT_PASSWORD: ${DB_ROOT_PASSWORD}\n")
		b.WriteString("    volumes:\n      - data:/data/db\n")
	case "redis":
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

// ExecSpec describes a command to run inside a database container via Docker Engine API.
type ExecSpec struct {
	Container string
	Env       []string
	Args      []string
	Stdin     bool
}

// Exec runs a command inside a container through the Docker Engine API.
type Exec struct {
	Docker    *docker.Client
	Container string
	Env       []string
}

func (e Exec) Spec(stdin bool, args ...string) ExecSpec {
	return ExecSpec{Container: e.Container, Env: e.Env, Args: args, Stdin: stdin}
}

func (e Exec) Output(ctx context.Context, stdin string, args ...string) (string, error) {
	if e.Docker == nil {
		return "", fmt.Errorf("docker client is nil")
	}
	var out, errb bytes.Buffer
	opts := docker.ExecOptions{
		Cmd:    args,
		Env:    e.Env,
		Stdout: &out,
		Stderr: &errb,
	}
	if stdin != "" {
		opts.Stdin = strings.NewReader(stdin)
	}
	if err := e.Docker.Exec(ctx, e.Container, opts); err != nil {
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
	Docker    *docker.Client
	Engine    string
	Container string
	Password  string
	Log       io.Writer // optional output sink for streaming operations such as RestoreStream
}

func (c Client) exec() Exec {
	switch c.Engine {
	case "mysql", "mariadb":
		return Exec{Docker: c.Docker, Container: c.Container, Env: []string{"MYSQL_PWD=" + c.Password}}
	case "postgres":
		return Exec{Docker: c.Docker, Container: c.Container, Env: []string{"PGPASSWORD=" + c.Password}}
	case "redis":
		if c.Password != "" {
			return Exec{Docker: c.Docker, Container: c.Container, Env: []string{"REDISCLI_AUTH=" + c.Password}}
		}
	}
	return Exec{Docker: c.Docker, Container: c.Container}
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
		if readOnly {
			if err := validateConsoleSQL(c.Engine, sql, true); err != nil {
				return nil, err
			}
		}
		input := sql
		if readOnly {
			input = "SET standard_conforming_strings = on;\nSET default_transaction_read_only = on;\nSET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY;\n" + sql
		}
		out, err := c.exec().Output(ctx, input, "psql", "-U", "postgres", "-d", database, "-X", "-q", "--csv", "-v", "ON_ERROR_STOP=1", "-P", "pager=off")
		if err != nil {
			return nil, err
		}
		return parseCSV(out, ',')
	case "mysql", "mariadb":
		if readOnly {
			if err := validateConsoleSQL(c.Engine, sql, true); err != nil {
				return nil, err
			}
		}
		input := sql
		if readOnly {
			input = "SET SESSION TRANSACTION READ ONLY;\nSTART TRANSACTION;\n" + strings.TrimRight(strings.TrimSpace(sql), ";") + ";\nROLLBACK;\n"
		}
		out, err := c.exec().Output(ctx, input, c.mysqlBin(), "-uroot", "--batch", "-D", database)
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

func unescapeMySQLTSV(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			case '0':
				b.WriteByte(0)
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseTSV(s string) *Result {
	res := &Result{Columns: []string{}, Rows: [][]string{}}
	trimmed := strings.TrimRight(s, "\n")
	if trimmed == "" {
		return res
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 0 || lines[0] == "" {
		return res
	}
	cols := strings.Split(strings.TrimSuffix(lines[0], "\r"), "\t")
	for i, c := range cols {
		cols[i] = unescapeMySQLTSV(c)
	}
	res.Columns = cols
	for _, l := range lines[1:] {
		fields := strings.Split(strings.TrimSuffix(l, "\r"), "\t")
		for i, f := range fields {
			fields[i] = unescapeMySQLTSV(f)
		}
		res.Rows = append(res.Rows, fields)
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
		s, err := c.exec().Output(ctx, "", "redis-cli", "INFO", "keyspace")
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
	if strings.ContainsRune(password, 0) {
		return fmt.Errorf("密码包含无效字符")
	}
	switch c.Engine {
	case "postgres":
		if user != "" {
			if _, err := c.rows(ctx, "postgres", fmt.Sprintf(`CREATE ROLE "%s" LOGIN PASSWORD %s`, user, sqlString(password))); err != nil && !strings.Contains(err.Error(), "already exists") {
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
			sql += fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY %s; GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%'; FLUSH PRIVILEGES;", user, mysqlString(password), name, user)
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
	if strings.ContainsRune(password, 0) {
		return fmt.Errorf("密码包含无效字符")
	}
	for _, d := range databases {
		if !ValidIdent(d) {
			return fmt.Errorf("库名不合法")
		}
	}
	switch c.Engine {
	case "postgres":
		if _, err := c.rows(ctx, "postgres", fmt.Sprintf(`CREATE ROLE "%s" LOGIN PASSWORD %s`, user, sqlString(password))); err != nil {
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
		sql := fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY %s;", user, mysqlString(password))
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
	if strings.ContainsRune(password, 0) {
		return fmt.Errorf("密码包含无效字符")
	}
	switch c.Engine {
	case "postgres":
		_, err := c.rows(ctx, "postgres", fmt.Sprintf(`ALTER ROLE "%s" PASSWORD %s`, user, sqlString(password)))
		return err
	case "mysql", "mariadb":
		if host == "" {
			host = "%"
		}
		if !ValidHost(host) {
			return fmt.Errorf("主机名不合法")
		}
		_, err := c.rows(ctx, "mysql", fmt.Sprintf("ALTER USER '%s'@'%s' IDENTIFIED BY %s; FLUSH PRIVILEGES;", user, host, mysqlString(password)))
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
		if !ValidHost(host) {
			return fmt.Errorf("主机名不合法")
		}
		_, err := c.rows(ctx, "mysql", fmt.Sprintf("DROP USER '%s'@'%s'", user, host))
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

// DumpCmd returns the exec spec that writes a logical backup of database to stdout.
func (c Client) DumpCmd(database string) (ExecSpec, string, error) {
	e := c.exec()
	switch c.Engine {
	case "postgres":
		return e.Spec(false, "pg_dump", "-U", "postgres", "-Fc", "-Z", "0", database), "pgdump", nil
	case "mysql":
		return e.Spec(false, "mysqldump", "-uroot", "--single-transaction", "--routines", "--triggers", "--events", "--set-gtid-purged=OFF", database), "sql", nil
	case "mariadb":
		return e.Spec(false, "mariadb-dump", "-uroot", "--single-transaction", "--routines", "--triggers", "--events", database), "sql", nil
	case "mongo":
		return e.Spec(false, "mongodump", "--archive", "-u", "root", "-p", c.Password, "--authenticationDatabase", "admin", "--db", database), "archive", nil
	case "redis":
		return e.Spec(false, "sh", "-c", `tmp="/tmp/panel-dump-$$.rdb"; trap 'rm -f "$tmp"' EXIT; redis-cli --rdb "$tmp" >/dev/null && cat "$tmp"`), "rdb", nil
	}
	return ExecSpec{}, "", fmt.Errorf("unknown engine")
}

// RestoreCmd returns the exec spec that reads a dump from stdin into database.
func (c Client) RestoreCmd(sourceDB, database string, clean bool) (ExecSpec, error) {
	e := c.exec()
	switch c.Engine {
	case "postgres":
		args := []string{"pg_restore", "-U", "postgres", "-d", database, "--no-owner", "--no-privileges"}
		if clean {
			args = append(args, "--clean", "--if-exists")
		}
		return e.Spec(true, args...), nil
	case "mysql", "mariadb":
		return e.Spec(true, c.mysqlBin(), "-uroot", database), nil
	case "mongo":
		args := []string{"mongorestore", "--archive", "-u", "root", "-p", c.Password, "--authenticationDatabase", "admin"}
		if strings.TrimSpace(sourceDB) != "" {
			args = append(args, "--nsFrom", sourceDB+".*", "--nsTo", database+".*")
		} else {
			args = append(args, "--nsFrom", "$db$.$coll$", "--nsTo", database+".$coll$")
		}
		if clean {
			args = append(args, "--drop")
		}
		return e.Spec(true, args...), nil
	}
	return ExecSpec{}, fmt.Errorf("%s 暂不支持在面板中恢复，请下载备份手动恢复", c.Engine)
}

const pgUserSchemasQuery = `SELECT nspname FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema'`

// ResetPostgresSchemas terminates other connections to database, drops all non-system
// schemas, and recreates the public schema while preserving database ownership and CONNECT grants.
func (c Client) ResetPostgresSchemas(ctx context.Context, database string) error {
	if c.Engine != "postgres" {
		return fmt.Errorf("仅 PostgreSQL 支持重置 Schema")
	}
	if !ValidIdent(database) {
		return fmt.Errorf("库名无效")
	}
	_, _ = c.rows(ctx, "postgres", fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid <> pg_backend_pid()`,
		database,
	))
	owner := ""
	if rows, err := c.rows(ctx, "postgres", fmt.Sprintf(
		`SELECT r.rolname FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = '%s'`,
		database,
	)); err == nil && len(rows) > 0 && len(rows[0]) > 0 {
		owner = strings.TrimSpace(rows[0][0])
	}
	schemas, err := c.rows(ctx, database, pgUserSchemasQuery)
	if err != nil {
		return fmt.Errorf("读取 Schema 列表失败：%w", err)
	}
	var stmts strings.Builder
	for _, row := range schemas {
		if len(row) == 0 || row[0] == "" {
			continue
		}
		esc := strings.ReplaceAll(row[0], `"`, `""`)
		stmts.WriteString(fmt.Sprintf(`DROP SCHEMA IF EXISTS "%s" CASCADE;`+"\n", esc))
	}
	stmts.WriteString(`CREATE SCHEMA public;` + "\n")
	if owner != "" && owner != "postgres" {
		escOwner := strings.ReplaceAll(owner, `"`, `""`)
		stmts.WriteString(fmt.Sprintf(`ALTER SCHEMA public OWNER TO "%s"; GRANT ALL ON SCHEMA public TO "%s";`+"\n", escOwner, escOwner))
	}
	stmts.WriteString(`GRANT USAGE ON SCHEMA public TO PUBLIC;` + "\n")
	if _, err := c.rows(ctx, database, stmts.String()); err != nil {
		return fmt.Errorf("重置 PostgreSQL Schema 失败：%w", err)
	}
	return nil
}

// RestoreStream restores a database dump stream from r into targetDB across
// PostgreSQL (auto-detecting PGDMP custom format vs plain SQL), MySQL/MariaDB,
// and MongoDB. When overwrite is true, PostgreSQL plain-SQL streams reset user
// schemas before piping into psql, pg_restore uses --clean --if-exists, and
// MongoDB passes --drop.
func (c Client) RestoreStream(ctx context.Context, targetDB, sourceDB string, r io.Reader, overwrite bool) error {
	if r == nil {
		return fmt.Errorf("恢复数据流为空")
	}
	if !ValidIdent(targetDB) {
		return fmt.Errorf("库名无效")
	}

	switch c.Engine {
	case "postgres":
		br, ok := r.(*bufio.Reader)
		if !ok {
			br = bufio.NewReaderSize(r, 64<<10)
		}
		magic, peekErr := br.Peek(5)
		if len(magic) == 0 && peekErr != nil && peekErr != io.EOF {
			return fmt.Errorf("读取备份流失败：%w", peekErr)
		}
		var (
			spec ExecSpec
			err  error
		)
		if string(magic) == "PGDMP" {
			spec, err = c.RestoreCmd(sourceDB, targetDB, overwrite)
		} else {
			if overwrite {
				if err := c.ResetPostgresSchemas(ctx, targetDB); err != nil {
					return err
				}
			}
			spec, err = c.ImportCmd(targetDB, "sql")
		}
		if err != nil {
			return err
		}
		return RunWithInput(ctx, c.Docker, spec, br, c.Log)

	case "mysql", "mariadb":
		br, ok := r.(*bufio.Reader)
		if !ok {
			br = bufio.NewReaderSize(r, 64<<10)
		}
		magic, peekErr := br.Peek(5)
		if len(magic) == 0 && peekErr != nil && peekErr != io.EOF {
			return fmt.Errorf("读取备份流失败：%w", peekErr)
		}
		if string(magic) == "PGDMP" {
			return fmt.Errorf("MySQL 只能导入 .sql 文件")
		}
		spec, err := c.RestoreCmd("", targetDB, overwrite)
		if err != nil {
			return err
		}
		return RunWithInput(ctx, c.Docker, spec, br, c.Log)

	case "mongo":
		spec, err := c.RestoreCmd(sourceDB, targetDB, overwrite)
		if err != nil {
			return err
		}
		return RunWithInput(ctx, c.Docker, spec, r, c.Log)

	default:
		_, err := c.RestoreCmd(sourceDB, targetDB, overwrite)
		return err
	}
}

// RunWithInput runs spec via Docker Engine API with stdin from r, returning stderr text on failure.
func RunWithInput(ctx context.Context, dk *docker.Client, spec ExecSpec, r io.Reader, log io.Writer) error {
	if dk == nil {
		return fmt.Errorf("docker client is nil")
	}
	var errb bytes.Buffer
	stderrW := io.Writer(&errb)
	if log != nil {
		stderrW = io.MultiWriter(&errb, log)
	}
	err := dk.Exec(ctx, spec.Container, docker.ExecOptions{
		Cmd:    spec.Args,
		Env:    spec.Env,
		Stdin:  r,
		Stdout: log,
		Stderr: stderrW,
	})
	if err != nil {
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
