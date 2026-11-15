package dbmgr

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ---------- dangerous statement detection ----------

var (
	commentLineRe  = regexp.MustCompile(`(?m)(--|#)[^\n]*`)
	commentBlockRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	stringLitRe    = regexp.MustCompile(`'(?:[^']|'')*'|"(?:[^"]|"")*"|` + "`[^`]*`")
	wsRe           = regexp.MustCompile(`\s+`)
	whereRe        = regexp.MustCompile(`\bWHERE\b`)
	alterDropRe    = regexp.MustCompile(`^ALTER\s+TABLE\b.*\bDROP\b`)
)

// Danger lists the reasons a script needs explicit confirmation: DROP,
// TRUNCATE, ALTER … DROP, and DELETE/UPDATE without a WHERE clause.
func Danger(sql string) []string {
	s := commentBlockRe.ReplaceAllString(sql, " ")
	s = stringLitRe.ReplaceAllString(s, "''")
	s = commentLineRe.ReplaceAllString(s, " ")
	var out []string
	seen := map[string]bool{}
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, st := range strings.Split(s, ";") {
		st = strings.ToUpper(strings.TrimSpace(wsRe.ReplaceAllString(st, " ")))
		if st == "" {
			continue
		}
		first, _, _ := strings.Cut(st, " ")
		switch first {
		case "DROP":
			add("包含 DROP 语句")
		case "TRUNCATE":
			add("包含 TRUNCATE 语句")
		case "DELETE":
			if !whereRe.MatchString(st) {
				add("DELETE 没有 WHERE 条件，将删除整表数据")
			}
		case "UPDATE":
			if !whereRe.MatchString(st) {
				add("UPDATE 没有 WHERE 条件，将修改整表数据")
			}
		case "ALTER":
			if alterDropRe.MatchString(st) {
				add("ALTER TABLE 包含 DROP")
			}
		}
	}
	return out
}

// ---------- console query with limits ----------

// limitWriter buffers up to max bytes and then cancels the command.
type limitWriter struct {
	buf      bytes.Buffer
	max      int
	overflow bool
	cancel   context.CancelFunc
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.overflow {
		return 0, errors.New("output limit")
	}
	if l.buf.Len()+len(p) > l.max {
		l.buf.Write(p[:l.max-l.buf.Len()])
		l.overflow = true
		l.cancel()
		return 0, errors.New("output limit")
	}
	return l.buf.Write(p)
}

// Console runs user SQL with a statement timeout, keeping at most maxRows rows
// (and a few MB of output). It reports whether the result was cut.
func (c Client) Console(ctx context.Context, database, sql string, readOnly bool, maxRows, timeoutSec int) (*Result, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var cmd *exec.Cmd
	var input string
	switch c.Engine {
	case "postgres":
		input = fmt.Sprintf("SET statement_timeout = '%ds';\n", timeoutSec)
		if readOnly {
			input += "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY;\n"
		}
		input += sql
		cmd = c.exec().Command(ctx, true, "psql", "-U", "postgres", "-d", database, "-X", "-q", "--csv", "-v", "ON_ERROR_STOP=1", "-P", "pager=off")
	case "mysql", "mariadb":
		if c.Engine == "mysql" {
			input = fmt.Sprintf("SET SESSION max_execution_time=%d;\n", timeoutSec*1000)
		} else {
			input = fmt.Sprintf("SET SESSION max_statement_time=%d;\n", timeoutSec)
		}
		body := strings.TrimRight(strings.TrimSpace(sql), ";")
		if readOnly {
			input += "SET SESSION TRANSACTION READ ONLY;\nSTART TRANSACTION;\n" + body + ";\nROLLBACK;\n"
		} else {
			input += body + ";\n"
		}
		cmd = c.exec().Command(ctx, true, c.mysqlBin(), "-uroot", "--batch", "--raw", "-D", database)
	default:
		return nil, false, fmt.Errorf("%s 不支持 SQL 控制台", c.Engine)
	}
	out := &limitWriter{max: 8 << 20, cancel: cancel}
	var errb bytes.Buffer
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout, cmd.Stderr = out, &errb
	err := cmd.Run()
	if err != nil && !out.overflow {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, false, errors.New(msg)
	}
	text := out.buf.String()
	if out.overflow {
		if i := strings.LastIndexByte(text, '\n'); i >= 0 {
			text = text[:i+1]
		}
	}
	var res *Result
	if c.Engine == "postgres" {
		r := csv.NewReader(strings.NewReader(text))
		r.FieldsPerRecord, r.LazyQuotes = -1, true
		recs, perr := r.ReadAll()
		if perr != nil && !out.overflow {
			return nil, false, perr
		}
		res = &Result{Columns: []string{}, Rows: [][]string{}}
		if len(recs) > 0 {
			res.Columns, res.Rows = recs[0], recs[1:]
		}
	} else {
		res = parseTSV(text)
	}
	trunc := out.overflow
	if len(res.Rows) > maxRows {
		res.Rows = res.Rows[:maxRows]
		trunc = true
	}
	return res, trunc, nil
}

// ---------- table structure & data ----------

type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Default  string `json:"default"`
	Key      string `json:"key"`
	Extra    string `json:"extra"`
}

type Index struct {
	Name    string `json:"name"`
	Columns string `json:"columns"`
	Unique  bool   `json:"unique"`
	Def     string `json:"def"`
}

func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (c Client) quoteIdent(s string) string {
	if c.Engine == "postgres" {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// HasTable guards table-name parameters before they are interpolated.
func (c Client) HasTable(ctx context.Context, database, table string) error {
	ts, err := c.Tables(ctx, database)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.Name == table {
			return nil
		}
	}
	return fmt.Errorf("表 %s 不存在", table)
}

func (c Client) Columns(ctx context.Context, database, table string) ([]Column, error) {
	var rows [][]string
	var err error
	switch c.Engine {
	case "postgres":
		rows, err = c.rows(ctx, database, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), CASE WHEN a.attnotnull THEN 'NO' ELSE 'YES' END,
			COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
			CASE WHEN EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = a.attrelid AND i.indisprimary AND a.attnum = ANY(i.indkey)) THEN 'PRI' ELSE '' END, ''
			FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			WHERE a.attrelid = ('public.' || `+sqlString(c.quoteIdent(table))+`)::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`)
	case "mysql", "mariadb":
		rows, err = c.rows(ctx, database, `SELECT column_name, column_type, is_nullable, COALESCE(column_default,''), column_key, extra
			FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = `+sqlString(table)+` ORDER BY ordinal_position`)
	default:
		return nil, fmt.Errorf("不支持")
	}
	if err != nil {
		return nil, err
	}
	out := []Column{}
	for _, r := range rows {
		if len(r) < 6 {
			continue
		}
		out = append(out, Column{Name: r[0], Type: r[1], Nullable: r[2] == "YES", Default: r[3], Key: r[4], Extra: r[5]})
	}
	return out, nil
}

func (c Client) Indexes(ctx context.Context, database, table string) ([]Index, error) {
	var rows [][]string
	var err error
	switch c.Engine {
	case "postgres":
		rows, err = c.rows(ctx, database, `SELECT i.relname, COALESCE((SELECT string_agg(a.attname, ', ' ORDER BY k.n) FROM unnest(x.indkey) WITH ORDINALITY k(attnum, n)
			JOIN pg_attribute a ON a.attrelid = x.indrelid AND a.attnum = k.attnum), ''), CASE WHEN x.indisunique THEN 't' ELSE 'f' END, pg_get_indexdef(x.indexrelid)
			FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid WHERE x.indrelid = ('public.' || `+sqlString(c.quoteIdent(table))+`)::regclass ORDER BY 1`)
	case "mysql", "mariadb":
		rows, err = c.rows(ctx, database, `SELECT index_name, GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ', '), IF(MAX(non_unique)=0,'t','f'), MAX(index_type)
			FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = `+sqlString(table)+` GROUP BY index_name ORDER BY 1`)
	default:
		return nil, fmt.Errorf("不支持")
	}
	if err != nil {
		return nil, err
	}
	out := []Index{}
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		out = append(out, Index{Name: r[0], Columns: r[1], Unique: r[2] == "t", Def: r[3]})
	}
	return out, nil
}

// TableRows returns one page of a table's data.
func (c Client) TableRows(ctx context.Context, database, table string, limit, offset int) (*Result, error) {
	q := fmt.Sprintf("SELECT * FROM %s LIMIT %d OFFSET %d", c.quoteIdent(table), limit, offset)
	res, _, err := c.Console(ctx, database, q, true, limit, 30)
	return res, err
}

// ---------- slow queries ----------

type SlowQuery struct {
	Query   string  `json:"query"`
	Calls   int64   `json:"calls"`
	TotalMs float64 `json:"totalMs"`
	MeanMs  float64 `json:"meanMs"`
	Rows    int64   `json:"rows"`
}

// SlowQueries returns the top statements by total time. hint explains how to
// enable statistics when they are unavailable.
func (c Client) SlowQueries(ctx context.Context) ([]SlowQuery, string, error) {
	var rows [][]string
	var err error
	switch c.Engine {
	case "postgres":
		ext, err := c.rows(ctx, "postgres", `SELECT count(*) FROM pg_extension WHERE extname='pg_stat_statements'`)
		if err != nil {
			return nil, "", err
		}
		if len(ext) == 0 || ext[0][0] == "0" {
			if _, err := c.rows(ctx, "postgres", `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
				return nil, "未启用 pg_stat_statements：需要在启动参数加入 -c shared_preload_libraries=pg_stat_statements 并重启实例。", nil
			}
		}
		rows, err = c.rows(ctx, "postgres", `SELECT left(regexp_replace(query, '\s+', ' ', 'g'), 2000), calls, round(total_exec_time::numeric, 1), round(mean_exec_time::numeric, 2), rows
			FROM pg_stat_statements WHERE dbid IN (SELECT oid FROM pg_database WHERE NOT datistemplate) ORDER BY total_exec_time DESC LIMIT 50`)
		if err != nil {
			if strings.Contains(err.Error(), "shared_preload_libraries") {
				return nil, "pg_stat_statements 扩展已创建，但尚未加载：需要在启动参数加入 -c shared_preload_libraries=pg_stat_statements 并重启实例。", nil
			}
			return nil, "", err
		}
	case "mysql", "mariadb":
		rows, err = c.rows(ctx, "mysql", `SELECT LEFT(REPLACE(REPLACE(digest_text, '\n', ' '), '\t', ' '), 2000), count_star, ROUND(sum_timer_wait/1e9, 1), ROUND(avg_timer_wait/1e9, 2), sum_rows_sent
			FROM performance_schema.events_statements_summary_by_digest WHERE digest_text IS NOT NULL ORDER BY sum_timer_wait DESC LIMIT 50`)
		if err != nil {
			return nil, "无法读取 performance_schema（MariaDB 默认关闭）：需要以 --performance-schema=ON 启动实例。", nil
		}
	default:
		return nil, "", fmt.Errorf("%s 不支持慢查询分析", c.Engine)
	}
	out := []SlowQuery{}
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		q := SlowQuery{Query: r[0]}
		q.Calls, _ = strconv.ParseInt(r[1], 10, 64)
		q.TotalMs, _ = strconv.ParseFloat(r[2], 64)
		q.MeanMs, _ = strconv.ParseFloat(r[3], 64)
		q.Rows, _ = strconv.ParseInt(r[4], 10, 64)
		out = append(out, q)
	}
	return out, "", nil
}

// ---------- import / export ----------

// ExportCmd writes database to stdout. For PostgreSQL plain selects SQL text
// instead of the custom (pg_restore) format.
func (c Client) ExportCmd(ctx context.Context, database string, plain bool) (*exec.Cmd, string, error) {
	if c.Engine == "postgres" && plain {
		return c.exec().Command(ctx, false, "pg_dump", "-U", "postgres", "--no-owner", "--no-privileges", database), "sql", nil
	}
	cmd, format, err := c.DumpCmd(ctx, database)
	if format == "pgdump" {
		format = "dump"
	}
	return cmd, format, err
}

// ImportCmd reads a dump from stdin. format is "sql" (plain SQL) or "dump" (pg_dump custom format).
func (c Client) ImportCmd(ctx context.Context, database, format string) (*exec.Cmd, error) {
	e := c.exec()
	switch c.Engine {
	case "postgres":
		if format == "dump" {
			return e.Command(ctx, true, "pg_restore", "-U", "postgres", "-d", database, "--no-owner", "--no-privileges"), nil
		}
		return e.Command(ctx, true, "psql", "-U", "postgres", "-d", database, "-X", "-q", "-v", "ON_ERROR_STOP=1"), nil
	case "mysql", "mariadb":
		if format == "dump" {
			return nil, fmt.Errorf("MySQL 只能导入 .sql 文件")
		}
		return e.Command(ctx, true, c.mysqlBin(), "-uroot", database), nil
	}
	return nil, fmt.Errorf("%s 不支持在线导入", c.Engine)
}
