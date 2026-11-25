package dbmgr

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"compose-panel/internal/docker"
)

// ---------- SQL lexical scanner & safety checks ----------

type sqlTokKind int

const (
	tokWord sqlTokKind = iota
	tokIdent
	tokString
	tokSemi
	tokLParen
	tokRParen
	tokBackslash
	tokOther
)

type sqlToken struct {
	kind      sqlTokKind
	raw       string
	upper     string
	lineStart bool
}

func (t sqlToken) isWord(w string) bool { return t.kind == tokWord && t.upper == w }

type sqlDialect struct {
	backslashEscapes    bool // \' and \" escape inside '...' and "..."
	pgEscapeStrings     bool // E'...' / e'...' uses backslash escapes
	dollarQuote         bool // PostgreSQL $tag$...$tag$
	hashComment         bool // # ... \n
	dashNeedsSpace      bool // MySQL requires whitespace/EOF after --
	nestedBlockComments bool // PostgreSQL allows nested /* /* */ */
	unwrapExecComments  bool // MySQL /*!50000 ... */ and MariaDB /*M! ... */
	backtickIdent       bool // MySQL `...`
	strictUnclosed      bool // reject unclosed quotes/comments
}

var (
	pgDialect = sqlDialect{
		backslashEscapes:    false,
		pgEscapeStrings:     true,
		dollarQuote:         true,
		hashComment:         false,
		dashNeedsSpace:      false,
		nestedBlockComments: true,
		unwrapExecComments:  false,
		backtickIdent:       false,
		strictUnclosed:      true,
	}
	mysqlDialect = sqlDialect{
		backslashEscapes:    true,
		pgEscapeStrings:     false,
		dollarQuote:         false,
		hashComment:         true,
		dashNeedsSpace:      true,
		nestedBlockComments: false,
		unwrapExecComments:  true,
		backtickIdent:       true,
		strictUnclosed:      true,
	}
	genericDangerDialect = sqlDialect{
		backslashEscapes:    true,
		pgEscapeStrings:     true,
		dollarQuote:         true,
		hashComment:         true,
		dashNeedsSpace:      false,
		nestedBlockComments: false,
		unwrapExecComments:  true,
		backtickIdent:       true,
		strictUnclosed:      false,
	}
)

func dialectForEngine(engine string) sqlDialect {
	switch engine {
	case "postgres":
		return pgDialect
	case "mysql", "mariadb":
		return mysqlDialect
	default:
		return genericDangerDialect
	}
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\f' || b == '\v'
}

func isWordStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b >= 0x80
}

func isWordPart(b byte) bool {
	return isWordStart(b) || (b >= '0' && b <= '9')
}

func scanQuoted(sql string, start int, quote byte, allowBackslash, strict bool) (int, error) {
	n := len(sql)
	for i := start + 1; i < n; i++ {
		ch := sql[i]
		if allowBackslash && ch == '\\' {
			if i+1 >= n {
				if strict {
					return n, errors.New("SQL 语法错误：字符串未闭合")
				}
				return n, nil
			}
			i++
			continue
		}
		if ch == quote {
			if i+1 < n && sql[i+1] == quote {
				i++
				continue
			}
			return i + 1, nil
		}
	}
	if strict {
		return n, errors.New("SQL 语法错误：字符串或标识符未闭合")
	}
	return n, nil
}

func matchDollarTag(sql string, start int) (string, bool) {
	n := len(sql)
	if start >= n || sql[start] != '$' {
		return "", false
	}
	if start+1 < n && sql[start+1] == '$' {
		return "$$", true
	}
	if start+1 >= n || !isWordStart(sql[start+1]) {
		return "", false
	}
	for j := start + 2; j < n; j++ {
		if sql[j] == '$' {
			return sql[start : j+1], true
		}
		if !isWordPart(sql[j]) {
			return "", false
		}
	}
	return "", false
}

func tokenizeSQL(sql string, d sqlDialect) ([]sqlToken, error) {
	var toks []sqlToken
	n := len(sql)
	execCommentDepth := 0
	atLineStart := true
	emit := func(kind sqlTokKind, raw, upper string) {
		toks = append(toks, sqlToken{kind: kind, raw: raw, upper: upper, lineStart: atLineStart})
		atLineStart = false
	}
	for i := 0; i < n; {
		ch := sql[i]
		if isASCIISpace(ch) {
			if ch == '\n' || ch == '\r' {
				atLineStart = true
			}
			i++
			continue
		}
		// Closing delimiter of an unwrapped MySQL/MariaDB executable comment.
		if execCommentDepth > 0 && ch == '*' && i+1 < n && sql[i+1] == '/' {
			execCommentDepth--
			i += 2
			continue
		}
		// Line comment: -- (terminated by \n or bare \r as in PostgreSQL scan.l)
		if ch == '-' && i+1 < n && sql[i+1] == '-' {
			if !d.dashNeedsSpace || i+2 == n || isASCIISpace(sql[i+2]) {
				i += 2
				for i < n && sql[i] != '\n' && sql[i] != '\r' {
					i++
				}
				continue
			}
		}
		// Line comment: # (MySQL/MariaDB, terminated by \n or bare \r)
		if d.hashComment && ch == '#' {
			i++
			for i < n && sql[i] != '\n' && sql[i] != '\r' {
				i++
			}
			continue
		}
		// Block comment: /* ... */ (and MySQL /*! ... */ or MariaDB /*M! ... */)
		if ch == '/' && i+1 < n && sql[i+1] == '*' {
			if d.unwrapExecComments {
				if i+2 < n && sql[i+2] == '!' {
					i += 3
					for i < n && sql[i] >= '0' && sql[i] <= '9' {
						i++
					}
					execCommentDepth++
					continue
				}
				if i+3 < n && sql[i+2] == 'M' && sql[i+3] == '!' {
					i += 4
					for i < n && sql[i] >= '0' && sql[i] <= '9' {
						i++
					}
					execCommentDepth++
					continue
				}
			}
			i += 2
			depth := 1
			for i < n && depth > 0 {
				if d.nestedBlockComments && sql[i] == '/' && i+1 < n && sql[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if sql[i] == '*' && i+1 < n && sql[i+1] == '/' {
					depth--
					i += 2
					continue
				}
				i++
			}
			if depth > 0 && d.strictUnclosed {
				return nil, errors.New("SQL 语法错误：块注释未闭合")
			}
			continue
		}
		// PostgreSQL E'...' / e'...' escape string literal.
		if d.pgEscapeStrings && (ch == 'E' || ch == 'e') && i+1 < n && sql[i+1] == '\'' {
			end, err := scanQuoted(sql, i+1, '\'', true, d.strictUnclosed)
			if err != nil {
				return nil, err
			}
			emit(tokString, sql[i:end], "")
			i = end
			continue
		}
		// Single-quoted string literal.
		if ch == '\'' {
			end, err := scanQuoted(sql, i, '\'', d.backslashEscapes, d.strictUnclosed)
			if err != nil {
				return nil, err
			}
			emit(tokString, sql[i:end], "")
			i = end
			continue
		}
		// Double-quoted identifier / string.
		if ch == '"' {
			end, err := scanQuoted(sql, i, '"', d.backslashEscapes, d.strictUnclosed)
			if err != nil {
				return nil, err
			}
			emit(tokIdent, sql[i:end], "")
			i = end
			continue
		}
		// Backtick identifier (MySQL/MariaDB).
		if d.backtickIdent && ch == '`' {
			end, err := scanQuoted(sql, i, '`', false, d.strictUnclosed)
			if err != nil {
				return nil, err
			}
			emit(tokIdent, sql[i:end], "")
			i = end
			continue
		}
		// PostgreSQL dollar-quoted string $tag$...$tag$.
		if d.dollarQuote && ch == '$' {
			if delim, ok := matchDollarTag(sql, i); ok {
				rest := sql[i+len(delim):]
				idx := strings.Index(rest, delim)
				if idx < 0 {
					if d.strictUnclosed {
						return nil, errors.New("SQL 语法错误：美元符字符串未闭合")
					}
					emit(tokString, sql[i:], "")
					i = n
					continue
				}
				end := i + len(delim) + idx + len(delim)
				emit(tokString, sql[i:end], "")
				i = end
				continue
			}
		}
		switch ch {
		case ';':
			emit(tokSemi, ";", "")
			i++
		case '(':
			emit(tokLParen, "(", "")
			i++
		case ')':
			emit(tokRParen, ")", "")
			i++
		case '\\':
			emit(tokBackslash, `\`, "")
			i++
		default:
			if isWordStart(ch) {
				start := i
				i++
				for i < n && (isWordPart(sql[i]) || sql[i] == '$') {
					i++
				}
				raw := sql[start:i]
				emit(tokWord, raw, strings.ToUpper(raw))
				continue
			}
			emit(tokOther, sql[i:i+1], "")
			i++
		}
	}
	if execCommentDepth > 0 && d.strictUnclosed {
		return nil, errors.New("SQL 语法错误：块注释未闭合")
	}
	return toks, nil
}

func splitStatements(toks []sqlToken) [][]sqlToken {
	var stmts [][]sqlToken
	var cur []sqlToken
	depth := 0
	for _, t := range toks {
		switch t.kind {
		case tokLParen:
			depth++
			cur = append(cur, t)
		case tokRParen:
			if depth > 0 {
				depth--
			}
			cur = append(cur, t)
		case tokSemi:
			if depth == 0 {
				if len(cur) > 0 {
					stmts = append(stmts, cur)
					cur = nil
				}
				continue
			}
			cur = append(cur, t)
		default:
			cur = append(cur, t)
		}
	}
	if len(cur) > 0 {
		stmts = append(stmts, cur)
	}
	return stmts
}

func findMatchingRParen(toks []sqlToken, openIdx int) int {
	depth := 0
	for i := openIdx; i < len(toks); i++ {
		if toks[i].kind == tokLParen {
			depth++
		} else if toks[i].kind == tokRParen {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func hasTopLevelWord(toks []sqlToken, word string) bool {
	depth := 0
	for _, t := range toks {
		switch t.kind {
		case tokLParen:
			depth++
		case tokRParen:
			if depth > 0 {
				depth--
			}
		case tokWord:
			if depth == 0 && t.upper == word {
				return true
			}
		}
	}
	return false
}

func skipExplainPrefix(toks []sqlToken) (inner []sqlToken, hasAnalyze bool) {
	pos := 1 // skip EXPLAIN
	for pos < len(toks) {
		t := toks[pos]
		if t.kind == tokLParen {
			end := findMatchingRParen(toks, pos)
			if end < 0 {
				return nil, hasAnalyze
			}
			for _, opt := range toks[pos+1 : end] {
				if opt.isWord("ANALYZE") || opt.isWord("ANALYSE") {
					hasAnalyze = true
				}
			}
			pos = end + 1
			continue
		}
		if t.kind != tokWord {
			break
		}
		switch t.upper {
		case "ANALYZE", "ANALYSE":
			hasAnalyze = true
			pos++
		case "VERBOSE", "EXTENDED", "PARTITIONS", "TREE":
			pos++
		case "FORMAT":
			pos++
			if pos < len(toks) && toks[pos].raw == "=" {
				pos++
			}
			if pos < len(toks) {
				pos++
			}
		default:
			return toks[pos:], hasAnalyze
		}
	}
	return toks[pos:], hasAnalyze
}

func checkStatementDanger(toks []sqlToken, add func(string)) {
	if len(toks) == 0 {
		return
	}
	// Unwrap leading parentheses, e.g. ((DELETE FROM t))
	for len(toks) >= 2 && toks[0].kind == tokLParen && findMatchingRParen(toks, 0) == len(toks)-1 {
		toks = toks[1 : len(toks)-1]
	}
	if len(toks) == 0 || toks[0].kind != tokWord {
		return
	}
	switch toks[0].upper {
	case "DROP":
		add("包含 DROP 语句")
	case "TRUNCATE":
		add("包含 TRUNCATE 语句")
	case "DELETE":
		if !hasTopLevelWord(toks[1:], "WHERE") {
			add("DELETE 没有 WHERE 条件，将删除整表数据")
		}
	case "UPDATE":
		if !hasTopLevelWord(toks[1:], "WHERE") {
			add("UPDATE 没有 WHERE 条件，将修改整表数据")
		}
	case "ALTER":
		if len(toks) > 1 && toks[1].isWord("TABLE") && hasTopLevelWord(toks[2:], "DROP") {
			add("ALTER TABLE 包含 DROP")
		}
	case "WITH":
		pos := 1
		if pos < len(toks) && toks[pos].isWord("RECURSIVE") {
			pos++
		}
		for pos < len(toks) {
			if toks[pos].kind != tokWord && toks[pos].kind != tokIdent {
				break
			}
			pos++
			// Optional CTE column list: cte(a, b)
			if pos < len(toks) && toks[pos].kind == tokLParen {
				end := findMatchingRParen(toks, pos)
				if end < 0 {
					return
				}
				pos = end + 1
			}
			if pos >= len(toks) || !toks[pos].isWord("AS") {
				break
			}
			pos++
			if pos < len(toks) && toks[pos].isWord("NOT") {
				pos++
			}
			if pos < len(toks) && toks[pos].isWord("MATERIALIZED") {
				pos++
			}
			if pos >= len(toks) || toks[pos].kind != tokLParen {
				break
			}
			end := findMatchingRParen(toks, pos)
			if end < 0 {
				return
			}
			// Recursively inspect data-modifying CTE body (e.g. WITH d AS (DELETE FROM t RETURNING *) ...).
			checkStatementDanger(toks[pos+1:end], add)
			pos = end + 1
			if pos < len(toks) && toks[pos].kind == tokOther && toks[pos].raw == "," {
				pos++
				continue
			}
			break
		}
		if pos < len(toks) {
			checkStatementDanger(toks[pos:], add)
		}
	case "EXPLAIN":
		inner, analyze := skipExplainPrefix(toks)
		if analyze && len(inner) > 0 {
			checkStatementDanger(inner, add)
		}
	}
}

// Danger lists the reasons a script needs explicit confirmation: DROP,
// TRUNCATE, ALTER … DROP, and DELETE/UPDATE without a WHERE clause.
func Danger(sql string) []string {
	return dangerWithDialect(sql, genericDangerDialect)
}

// DangerForEngine checks dangerous statements using the engine-specific SQL dialect.
func DangerForEngine(engine, sql string) []string {
	return dangerWithDialect(sql, dialectForEngine(engine))
}

func dangerWithDialect(sql string, d sqlDialect) []string {
	d.strictUnclosed = false
	toks, _ := tokenizeSQL(sql, d)
	var out []string
	seen := map[string]bool{}
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, st := range splitStatements(toks) {
		checkStatementDanger(st, add)
	}
	return out
}

var readOnlyAllowedStarters = map[string]bool{
	"SELECT":   true,
	"SHOW":     true,
	"DESCRIBE": true,
	"DESC":     true,
	"EXPLAIN":  true,
	"VALUES":   true,
	"TABLE":    true,
	"WITH":     true,
}

var readOnlyForbiddenWords = map[string]bool{
	"INSERT":     true,
	"UPDATE":     true,
	"DELETE":     true,
	"MERGE":      true,
	"UPSERT":     true,
	"DROP":       true,
	"ALTER":      true,
	"TRUNCATE":   true,
	"CREATE":     true,
	"GRANT":      true,
	"REVOKE":     true,
	"SET":        true,
	"RESET":      true,
	"DO":         true,
	"CALL":       true,
	"COPY":       true,
	"BEGIN":      true,
	"COMMIT":     true,
	"ROLLBACK":   true,
	"SAVEPOINT":  true,
	"PREPARE":    true,
	"EXECUTE":    true,
	"DEALLOCATE": true,
	"DISCARD":    true,
	"LOCK":       true,
	"UNLOCK":     true,
	"HANDLER":    true,
	"LOAD":       true,
	"RENAME":     true,
	"VACUUM":     true,
	"REINDEX":    true,
	"CLUSTER":    true,
	"REFRESH":    true,
	"FLUSH":      true,
	"PURGE":      true,
	"KILL":       true,
	"SHUTDOWN":   true,
	"INSTALL":    true,
	"UNINSTALL":  true,
	"OUTFILE":    true,
	"DUMPFILE":   true,
}

func validateReadOnlyStmt(toks []sqlToken) error {
	if len(toks) == 0 {
		return nil
	}
	// Unwrap enclosing parentheses around a query, e.g. (SELECT 1).
	for len(toks) >= 2 && toks[0].kind == tokLParen && findMatchingRParen(toks, 0) == len(toks)-1 {
		toks = toks[1 : len(toks)-1]
	}
	if len(toks) == 0 || toks[0].kind != tokWord {
		return errors.New("只读模式仅允许查询语句（SELECT / SHOW / DESCRIBE / EXPLAIN / WITH / VALUES / TABLE）")
	}
	first := toks[0].upper
	if !readOnlyAllowedStarters[first] {
		return fmt.Errorf("只读模式禁止执行 %s 语句", first)
	}
	if first == "EXPLAIN" {
		inner, _ := skipExplainPrefix(toks)
		if len(inner) == 0 {
			return errors.New("只读模式禁止无效的 EXPLAIN 语句")
		}
		if err := validateReadOnlyStmt(inner); err != nil {
			return err
		}
	}
	if first == "WITH" {
		pos := 1
		if pos < len(toks) && toks[pos].isWord("RECURSIVE") {
			pos++
		}
		for pos < len(toks) {
			if toks[pos].kind != tokWord && toks[pos].kind != tokIdent {
				return errors.New("只读模式仅允许只读 WITH 查询")
			}
			pos++
			if pos < len(toks) && toks[pos].kind == tokLParen {
				end := findMatchingRParen(toks, pos)
				if end < 0 {
					return errors.New("SQL 语法错误：括号未闭合")
				}
				pos = end + 1
			}
			if pos >= len(toks) || !toks[pos].isWord("AS") {
				return errors.New("只读模式仅允许只读 WITH 查询")
			}
			pos++
			if pos < len(toks) && toks[pos].isWord("NOT") {
				pos++
			}
			if pos < len(toks) && toks[pos].isWord("MATERIALIZED") {
				pos++
			}
			if pos >= len(toks) || toks[pos].kind != tokLParen {
				return errors.New("只读模式仅允许只读 WITH 查询")
			}
			end := findMatchingRParen(toks, pos)
			if end < 0 {
				return errors.New("SQL 语法错误：括号未闭合")
			}
			if err := validateReadOnlyStmt(toks[pos+1 : end]); err != nil {
				return err
			}
			pos = end + 1
			if pos < len(toks) && toks[pos].kind == tokOther && toks[pos].raw == "," {
				pos++
				continue
			}
			break
		}
		if pos >= len(toks) {
			return errors.New("只读模式仅允许只读 WITH 查询")
		}
		if err := validateReadOnlyStmt(toks[pos:]); err != nil {
			return err
		}
	}
	depth := 0
	for i, t := range toks {
		switch t.kind {
		case tokLParen:
			depth++
		case tokRParen:
			if depth > 0 {
				depth--
			}
		case tokWord:
			if readOnlyForbiddenWords[t.upper] {
				return fmt.Errorf("只读模式禁止包含 %s 操作", t.upper)
			}
			if t.upper == "INTO" && depth == 0 {
				return errors.New("只读模式禁止 SELECT ... INTO 语句")
			}
			// Guard against START TRANSACTION inside sub-clauses.
			if t.upper == "START" && i+1 < len(toks) && toks[i+1].isWord("TRANSACTION") {
				return errors.New("只读模式禁止事务控制语句")
			}
		}
	}
	return nil
}

var mysqlCLICommands = map[string]bool{
	"SOURCE":    true,
	"SYSTEM":    true,
	"TEE":       true,
	"NOTEE":     true,
	"DELIMITER": true,
	"PAGER":     true,
	"NOPAGER":   true,
	"EDIT":      true,
	"CONNECT":   true,
	"REHASH":    true,
	"CHARSET":   true,
	"PROMPT":    true,
}

func validateConsoleSQL(engine, sql string, readOnly bool) error {
	if strings.TrimSpace(sql) == "" {
		return errors.New("SQL 不能为空")
	}
	toks, err := tokenizeSQL(sql, dialectForEngine(engine))
	if err != nil {
		return err
	}
	// Block all unquoted client meta-commands (\!, \o, \i, \copy, \., \u, \r, etc.).
	for _, t := range toks {
		if t.kind == tokBackslash {
			return errors.New("SQL 控制台禁止执行反斜杠客户端元命令（如 \\!、\\i、\\o、\\.）")
		}
	}
	stmts := splitStatements(toks)
	if len(stmts) == 0 {
		return errors.New("SQL 不能为空")
	}
	for _, st := range stmts {
		switch engine {
		case "mysql", "mariadb":
			for i, t := range st {
				if t.kind == tokWord && (i == 0 || t.lineStart) && mysqlCLICommands[t.upper] {
					return fmt.Errorf("SQL 控制台禁止执行 MySQL 客户端命令 %s", t.upper)
				}
			}
			if len(st) > 0 && st[0].isWord("LOAD") {
				return errors.New("SQL 控制台禁止执行 LOAD DATA / LOAD XML 语句")
			}
			for i := 0; i < len(st)-1; i++ {
				if st[i].isWord("INTO") && (st[i+1].isWord("OUTFILE") || st[i+1].isWord("DUMPFILE")) {
					return errors.New("SQL 控制台禁止执行 INTO OUTFILE / INTO DUMPFILE 语句")
				}
			}
		case "postgres":
			if len(st) > 0 && st[0].isWord("COPY") && hasTopLevelWord(st[1:], "PROGRAM") {
				return errors.New("SQL 控制台禁止执行 COPY ... PROGRAM 语句")
			}
		}
		if readOnly {
			if err := validateReadOnlyStmt(st); err != nil {
				return err
			}
		}
	}
	return nil
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
	switch c.Engine {
	case "postgres", "mysql", "mariadb":
	default:
		return nil, false, fmt.Errorf("%s 不支持 SQL 控制台", c.Engine)
	}
	if err := validateConsoleSQL(c.Engine, sql, readOnly); err != nil {
		return nil, false, err
	}
	if c.Docker == nil {
		return nil, false, fmt.Errorf("docker client is nil")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var spec ExecSpec
	var input string
	switch c.Engine {
	case "postgres":
		input = fmt.Sprintf("SET statement_timeout = '%ds';\nSET standard_conforming_strings = on;\n", timeoutSec)
		if readOnly {
			input += "SET default_transaction_read_only = on;\nSET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY;\n"
		}
		input += sql
		spec = c.exec().Spec(true, "psql", "-U", "postgres", "-d", database, "-X", "-q", "--csv", "-v", "ON_ERROR_STOP=1", "-P", "pager=off")
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
		spec = c.exec().Spec(true, c.mysqlBin(), "-uroot", "--batch", "-D", database)
	}
	out := &limitWriter{max: 8 << 20, cancel: cancel}
	var errb bytes.Buffer
	err := c.Docker.Exec(ctx, spec.Container, docker.ExecOptions{
		Cmd:    spec.Args,
		Env:    spec.Env,
		Stdin:  strings.NewReader(input),
		Stdout: out,
		Stderr: &errb,
	})
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
			FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = `+mysqlString(table)+` ORDER BY ordinal_position`)
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
			FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = `+mysqlString(table)+` GROUP BY index_name ORDER BY 1`)
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
func (c Client) ExportCmd(database string, plain bool) (ExecSpec, string, error) {
	if c.Engine == "postgres" && plain {
		return c.exec().Spec(false, "pg_dump", "-U", "postgres", "--no-owner", "--no-privileges", database), "sql", nil
	}
	spec, format, err := c.DumpCmd(database)
	if format == "pgdump" {
		format = "dump"
	}
	return spec, format, err
}

// ImportCmd reads a dump from stdin. format is "sql" (plain SQL) or "dump" (pg_dump custom format).
func (c Client) ImportCmd(database, format string) (ExecSpec, error) {
	e := c.exec()
	switch c.Engine {
	case "postgres":
		if format == "dump" {
			return e.Spec(true, "pg_restore", "-U", "postgres", "-d", database, "--no-owner", "--no-privileges"), nil
		}
		return e.Spec(true, "psql", "-U", "postgres", "-d", database, "-X", "-q", "-v", "ON_ERROR_STOP=1"), nil
	case "mysql", "mariadb":
		if format == "dump" {
			return ExecSpec{}, fmt.Errorf("MySQL 只能导入 .sql 文件")
		}
		return e.Spec(true, c.mysqlBin(), "-uroot", database), nil
	}
	return ExecSpec{}, fmt.Errorf("%s 不支持在线导入", c.Engine)
}
