package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"compose-panel/internal/auth"
	"compose-panel/internal/notify"
)

const (
	roleViewer   = 1
	roleOperator = 2
	roleAdmin    = 3
)

var roleLevel = map[string]int{"viewer": roleViewer, "operator": roleOperator, "admin": roleAdmin}

type User struct {
	ID         int64   `json:"id"`
	Username   string  `json:"username"`
	Email      string  `json:"email"`
	Role       string  `json:"role"`
	TOTP       bool    `json:"totp"`
	Level      int     `json:"-"`
	Projects   []int64 `json:"projects"` // nil = all projects
	SessionID  string  `json:"-"`
	TokenScope string  `json:"-"`
	mfaPending bool
}

func (u *User) can(level int) bool { return u.Level >= level }

func (u *User) canProject(id int64) bool {
	if u.Level >= roleAdmin || u.Projects == nil {
		return true
	}
	for _, p := range u.Projects {
		if p == id {
			return true
		}
	}
	return false
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, u *User) error

const cookieName = "panel_session"

// authed wraps a handler with authentication, role check and CSRF protection.
func (s *Server) authed(min int, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authenticate(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if u.mfaPending {
			writeErr(w, errStatus(401, "需要两步验证"))
			return
		}
		if u.SessionID != "" && r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Panel") != "1" &&
			!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			writeErr(w, errStatus(403, "缺少 X-Panel 请求头"))
			return
		}
		if u.TokenScope == "deploy" && !deployTokenAllowed(r) {
			writeErr(w, errStatus(403, "此操作超出 deploy 令牌权限范围"))
			return
		}
		if !u.can(min) {
			writeErr(w, errForbidden)
			return
		}
		if err := h(w, r, u); err != nil {
			writeErr(w, err)
		}
	}
}

func deployTokenAllowed(r *http.Request) bool {
	p := r.URL.Path
	m := r.Method
	if m == http.MethodGet && (p == "/api/auth/me" || p == "/api/projects") {
		return true
	}
	if !strings.HasPrefix(p, "/api/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(p, "/api/"), "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	switch len(parts) {
	case 2:
		if m == http.MethodGet && (parts[0] == "projects" || parts[0] == "deployments" || parts[0] == "jobs") {
			return true
		}
	case 3:
		switch parts[0] {
		case "projects":
			if m == http.MethodPost && (parts[2] == "deploy" || parts[2] == "action") {
				return true
			}
			if m == http.MethodGet && (parts[2] == "deployments" || parts[2] == "images") {
				return true
			}
		case "deployments":
			if m == http.MethodPost && parts[2] == "rollback" {
				return true
			}
		case "jobs":
			if m == http.MethodGet && parts[2] == "log" {
				return true
			}
		}
	case 4:
		if m == http.MethodPost && parts[0] == "projects" && parts[2] == "images" && parts[3] == "check" {
			return true
		}
	}
	return false
}

// public wraps a handler that needs no authentication.
func public(h func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeErr(w, err)
		}
	}
}

func (s *Server) loadUser(id int64) (*User, error) {
	u := &User{ID: id}
	var disabled, allProjects int
	err := s.db.QueryRow(`SELECT username, email, role, totp_enabled, disabled, all_projects FROM users WHERE id=?`, id).
		Scan(&u.Username, &u.Email, &u.Role, &u.TOTP, &disabled, &allProjects)
	if err != nil {
		return nil, err
	}
	if disabled == 1 {
		return nil, errStatus(401, "账号已停用")
	}
	u.Level = roleLevel[u.Role]
	if u.Level < roleAdmin && allProjects == 0 {
		u.Projects = []int64{}
		rows, err := s.db.Query(`SELECT project_id FROM user_projects WHERE user_id=?`, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var pid int64
			if err := rows.Scan(&pid); err != nil {
				return nil, err
			}
			u.Projects = append(u.Projects, pid)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func (s *Server) authenticate(r *http.Request) (*User, error) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		var uid, exp, lastUsedAt int64
		var scope string
		var tid int64
		err := s.db.QueryRow(`SELECT id, user_id, scope, expires_at, last_used_at FROM api_tokens WHERE token_hash=?`, auth.SHA256Hex(tok)).Scan(&tid, &uid, &scope, &exp, &lastUsedAt)
		now := time.Now().Unix()
		if err != nil || (exp > 0 && exp < now) {
			return nil, errStatus(401, "API 令牌无效或已过期")
		}
		u, err := s.loadUser(uid)
		if err != nil {
			return nil, errStatus(401, "API 令牌无效")
		}
		u.TokenScope = scope
		switch scope {
		case "read":
			u.Level = min(u.Level, roleViewer)
		case "deploy":
			u.Level = min(u.Level, roleOperator)
		}
		if now-lastUsedAt > 60 {
			_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at=?, last_used_ip=? WHERE id=?`, now, clientIP(r), tid)
		}
		return u, nil
	}
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil, errStatus(401, "未登录")
	}
	sid := auth.SHA256Hex(c.Value)
	var uid, exp, seen int64
	var mfa int
	err = s.db.QueryRow(`SELECT user_id, expires_at, last_seen_at, mfa_pending FROM sessions WHERE id=?`, sid).Scan(&uid, &exp, &seen, &mfa)
	if err != nil || exp < time.Now().Unix() {
		return nil, errStatus(401, "登录已过期")
	}
	u, err := s.loadUser(uid)
	if err != nil {
		return nil, errStatus(401, "未登录")
	}
	u.SessionID = sid
	u.mfaPending = mfa == 1
	if time.Now().Unix()-seen > 60 {
		_, _ = s.db.Exec(`UPDATE sessions SET last_seen_at=?, ip=? WHERE id=?`, time.Now().Unix(), clientIP(r), sid)
	}
	return u, nil
}

// ---------- audit ----------

const (
	defaultAuditKeepDays = 180
	defaultAuditMaxRows  = 50000
)

var auditPruneState = struct {
	mu   sync.Mutex
	last map[string]time.Time
}{last: map[string]time.Time{}}

func (s *Server) shouldPruneAudit(now time.Time) bool {
	key := s.bootID
	if key == "" {
		key = "default"
	}
	auditPruneState.mu.Lock()
	defer auditPruneState.mu.Unlock()
	if len(auditPruneState.last) > 256 {
		cut := now.Add(-2 * time.Hour)
		for k, t := range auditPruneState.last {
			if t.Before(cut) {
				delete(auditPruneState.last, k)
			}
		}
	}
	if last, ok := auditPruneState.last[key]; ok && now.Sub(last) < time.Hour {
		return false
	}
	auditPruneState.last[key] = now
	return true
}

// AuditCLI records a CLI-initiated administrative action into the audit hash chain.
func (s *Server) AuditCLI(action, target, detail string) {
	s.audit(nil, &User{Username: "cli"}, action, target, detail, nil)
}

func (s *Server) audit(r *http.Request, u *User, action, target, detail string, err error) {
	actor := "system"
	if u != nil {
		actor = u.Username
		if u.TokenScope != "" {
			actor += " (token)"
		}
	}
	ip := ""
	if r != nil {
		ip = clientIP(r)
	}
	result := "ok"
	if err != nil {
		result = "failed"
		if detail != "" {
			detail += " · "
		}
		detail += err.Error()
	}
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	tx, txErr := s.db.Begin()
	if txErr != nil {
		slog.Error("audit begin transaction failed", "action", action, "err", txErr)
		return
	}
	defer tx.Rollback()
	var prev string
	scanErr := tx.QueryRow(`SELECT hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&prev)
	if errors.Is(scanErr, sql.ErrNoRows) {
		scanErr = tx.QueryRow(`SELECT value FROM settings WHERE key='audit_anchor_hash'`).Scan(&prev)
		if errors.Is(scanErr, sql.ErrNoRows) {
			prev = ""
			scanErr = nil
		}
	}
	if scanErr != nil {
		slog.Error("audit read previous hash failed", "action", action, "err", scanErr)
		return
	}
	now := time.Now()
	ts := now.Unix()
	h := auditHash(prev, ts, actor, action, target, ip, result, detail)
	if _, execErr := tx.Exec(`INSERT INTO audit_log(ts,actor,action,target,ip,result,detail,prev_hash,hash) VALUES(?,?,?,?,?,?,?,?,?)`,
		ts, actor, action, target, ip, result, detail, prev, h); execErr != nil {
		slog.Error("audit insert failed", "action", action, "err", execErr)
		return
	}
	if commitErr := tx.Commit(); commitErr != nil {
		slog.Error("audit commit failed", "action", action, "err", commitErr)
		return
	}
	if s.shouldPruneAudit(now) {
		if pruneErr := s.pruneAuditLogLocked(defaultAuditKeepDays, defaultAuditMaxRows); pruneErr != nil {
			slog.Error("audit log prune failed", "err", pruneErr)
		}
	}
}

// PruneAuditLog trims old or excess audit_log rows while anchoring the hash chain
// in settings('audit_anchor_hash') so audit verification remains mathematically continuous.
func (s *Server) PruneAuditLog(keepDays, maxRows int) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	return s.pruneAuditLogLocked(keepDays, maxRows)
}

func (s *Server) pruneAuditLogLocked(keepDays, maxRows int) error {
	if keepDays <= 0 && maxRows <= 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var cutoffID int64
	if maxRows > 0 {
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&count); err != nil {
			return err
		}
		if count > maxRows {
			var id int64
			err := tx.QueryRow(`SELECT id FROM audit_log ORDER BY id ASC LIMIT 1 OFFSET ?`, count-maxRows-1).Scan(&id)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if id > cutoffID {
				cutoffID = id
			}
		}
	}
	if keepDays > 0 {
		cutoffTS := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour).Unix()
		var firstKeptID int64
		err := tx.QueryRow(`SELECT id FROM audit_log WHERE ts>=? ORDER BY id ASC LIMIT 1`, cutoffTS).Scan(&firstKeptID)
		var ageCutoffID sql.NullInt64
		switch {
		case err == nil:
			if err := tx.QueryRow(`SELECT MAX(id) FROM audit_log WHERE id<?`, firstKeptID).Scan(&ageCutoffID); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			if err := tx.QueryRow(`SELECT MAX(id) FROM audit_log WHERE ts<?`, cutoffTS).Scan(&ageCutoffID); err != nil {
				return err
			}
		default:
			return err
		}
		if ageCutoffID.Valid && ageCutoffID.Int64 > cutoffID {
			cutoffID = ageCutoffID.Int64
		}
	}
	if cutoffID <= 0 {
		return nil
	}
	var cutoffHash string
	if err := tx.QueryRow(`SELECT hash FROM audit_log WHERE id=?`, cutoffID).Scan(&cutoffHash); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO settings(key,value) VALUES('audit_anchor_hash',?)`, cutoffHash); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM audit_log WHERE id<=?`, cutoffID); err != nil {
		return err
	}
	return tx.Commit()
}

func auditHash(prev string, ts int64, actor, action, target, ip, result, detail string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{prev, strconv.FormatInt(ts, 10), actor, action, target, ip, result, detail}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// ---------- setup & login ----------

func (s *Server) hasUsers() bool {
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n)
	return n > 0
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) error {
	return ok(w, map[string]any{"needed": !s.hasUsers(), "version": Version, "bootId": s.bootID})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Token, Username, Password, Email, Domain, AcmeEmail string
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.hasUsers() {
		return errStatus(409, "已经初始化")
	}
	rateKey := "setup:" + clientIP(r)
	if s.tooManyFailures(rateKey) {
		return errStatus(429, "初始化令牌尝试次数过多，请 15 分钟后再试")
	}
	if s.setupToken == "" || !auth.ConstantEqual(req.Token, s.setupToken) {
		s.recordFailure(rateKey)
		return errStatus(403, "初始化令牌不正确（见面板容器日志）")
	}
	if err := validUsername(req.Username); err != nil {
		return err
	}
	if len(req.Password) < 10 {
		return bad("密码至少 10 位")
	}
	if len(req.Password) > 72 {
		return bad("密码长度不能超过 72 字节")
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`INSERT INTO users(username,email,password_hash,role,created_at) VALUES(?,?,?,'admin',?)`,
		req.Username, req.Email, hash, time.Now().Unix())
	if err != nil {
		return err
	}
	if req.Domain != "" {
		s.setSetting("panel_domain", strings.ToLower(strings.TrimSpace(req.Domain)))
	}
	if req.AcmeEmail != "" {
		s.setSetting("acme_email", strings.TrimSpace(req.AcmeEmail))
	} else if req.Email != "" {
		s.setSetting("acme_email", req.Email)
	}
	s.setupToken = ""
	uid, _ := res.LastInsertId()
	if err := s.startSession(w, r, uid, false); err != nil {
		return err
	}
	u, _ := s.loadUser(uid)
	s.audit(r, u, "setup", req.Username, "创建管理员", nil)
	baseCtx := s.bgCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		cctx, cancel := context.WithTimeout(baseCtx, 15*time.Second)
		defer cancel()
		_ = s.syncCaddy(cctx)
	}()
	return ok(w, u)
}

func validUsername(n string) error {
	if len(n) < 2 || len(n) > 32 {
		return bad("用户名长度 2–32")
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return bad("用户名只能包含字母、数字、_ - .")
		}
	}
	return nil
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, uid int64, mfaPending bool) error {
	tok := auth.RandomToken(32)
	ttl, _ := strconv.Atoi(s.setting("session_ttl_hours", "168"))
	if ttl <= 0 {
		ttl = 168
	}
	now := time.Now()
	exp := now.Add(time.Duration(ttl) * time.Hour)
	if mfaPending {
		exp = now.Add(10 * time.Minute)
	}
	_, err := s.db.Exec(`INSERT INTO sessions(id,user_id,mfa_pending,created_at,expires_at,last_seen_at,ip,user_agent) VALUES(?,?,?,?,?,?,?,?)`,
		auth.SHA256Hex(tok), uid, boolInt(mfaPending), now.Unix(), exp.Unix(), now.Unix(), clientIP(r), truncate(r.UserAgent(), 200))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || (isTrustedProxyAddr(r.RemoteAddr) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")), Expires: exp,
	})
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Server) pruneLoginFailuresLocked(cut time.Time) {
	for k, ts := range s.loginFail {
		keep := ts[:0]
		for _, t := range ts {
			if t.After(cut) {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 {
			delete(s.loginFail, k)
		} else {
			s.loginFail[k] = keep
		}
	}
}

func (s *Server) tooManyFailures(key string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	cut := time.Now().Add(-15 * time.Minute)
	ts := s.loginFail[key]
	keep := ts[:0]
	for _, t := range ts {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(s.loginFail, key)
		return false
	}
	s.loginFail[key] = keep
	return len(keep) >= 5
}

func rateKeyClass(k string) int {
	switch {
	case strings.HasPrefix(k, "totp|"):
		return 2
	case strings.HasPrefix(k, "setup:"):
		return 1
	default:
		return 0
	}
}

func (s *Server) recordFailure(key string) {
	s.loginMu.Lock()
	now := time.Now()
	s.pruneLoginFailuresLocked(now.Add(-15 * time.Minute))
	const maxTrackedLoginKeys = 4096
	if _, exists := s.loginFail[key]; !exists {
		cls := rateKeyClass(key)
		if cls < 2 {
			// Bound non-TOTP rate-limit buckets independently so IP/username
			// flood eviction can never evict totp| (or setup:) keys.
			count := 0
			for k := range s.loginFail {
				if rateKeyClass(k) == cls {
					count++
				}
			}
			if count >= maxTrackedLoginKeys {
				for k := range s.loginFail {
					if rateKeyClass(k) == cls {
						delete(s.loginFail, k)
						break
					}
				}
			}
		}
	}
	s.loginFail[key] = append(s.loginFail[key], now)
	s.loginMu.Unlock()
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) error {
	var req struct{ Username, Password string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	key := strings.ToLower(req.Username) + "|" + clientIP(r)
	if s.tooManyFailures(key) {
		return errStatus(429, "失败次数过多，请 15 分钟后再试")
	}
	var uid int64
	var hash string
	var totp, disabled int
	err := s.db.QueryRow(`SELECT id, password_hash, totp_enabled, disabled FROM users WHERE username=?`, req.Username).Scan(&uid, &hash, &totp, &disabled)
	if err != nil || !auth.CheckPassword(hash, req.Password) || disabled == 1 {
		s.recordFailure(key)
		s.audit(r, &User{Username: req.Username}, "login", req.Username, "", fmt.Errorf("用户名或密码错误"))
		s.notify.Emit(notify.Event{Type: "security_login", Title: "登录失败", Body: fmt.Sprintf("用户 %s 从 %s 登录失败", req.Username, clientIP(r)), Target: key})
		return errStatus(401, "用户名或密码错误")
	}
	if err := s.startSession(w, r, uid, totp == 1); err != nil {
		return err
	}
	if totp == 1 {
		return ok(w, map[string]any{"mfa": true})
	}
	s.afterLogin(r, uid)
	u, _ := s.loadUser(uid)
	return ok(w, map[string]any{"mfa": false, "user": u})
}

func (s *Server) afterLogin(r *http.Request, uid int64) {
	_, _ = s.db.Exec(`UPDATE users SET last_login_at=?, last_login_ip=? WHERE id=?`, time.Now().Unix(), clientIP(r), uid)
	u, _ := s.loadUser(uid)
	s.audit(r, u, "login", "", "", nil)
}

func (s *Server) handleTOTPLogin(w http.ResponseWriter, r *http.Request) error {
	var req struct{ Code string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return errStatus(401, "未登录")
	}
	sid := auth.SHA256Hex(c.Value)
	var uid, exp int64
	var mfa int
	if err := s.db.QueryRow(`SELECT user_id, expires_at, mfa_pending FROM sessions WHERE id=?`, sid).Scan(&uid, &exp, &mfa); err != nil || exp < time.Now().Unix() || mfa != 1 {
		return errStatus(401, "请重新登录")
	}
	key := "totp|" + strconv.FormatInt(uid, 10)
	if s.tooManyFailures(key) {
		return errStatus(429, "失败次数过多，请 15 分钟后再试")
	}
	var secretEnc, codes string
	var lastStep int64
	_ = s.db.QueryRow(`SELECT totp_secret, recovery_codes, last_totp_step FROM users WHERE id=?`, uid).Scan(&secretEnc, &codes, &lastStep)
	code := strings.ReplaceAll(strings.TrimSpace(req.Code), " ", "")
	matchedStep, valid := auth.TOTPVerifyStep(s.box.MustOpen(secretEnc), code, time.Now())
	if valid {
		if matchedStep <= lastStep {
			valid = false
		} else {
			res, err := s.db.Exec(`UPDATE users SET last_totp_step=? WHERE id=? AND last_totp_step<?`, matchedStep, uid, matchedStep)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				valid = false
			}
		}
	}
	if !valid && len(code) > 6 {
		// recovery code
		h := auth.SHA256Hex(strings.ToLower(code))
		lines := strings.Split(codes, "\n")
		for i, l := range lines {
			if l != "" && auth.ConstantEqual(l, h) {
				valid = true
				lines = append(lines[:i], lines[i+1:]...)
				_, _ = s.db.Exec(`UPDATE users SET recovery_codes=? WHERE id=?`, strings.Join(lines, "\n"), uid)
				break
			}
		}
	}
	if !valid {
		s.recordFailure(key)
		return errStatus(401, "验证码不正确")
	}
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE id=?`, sid)
	if err := s.startSession(w, r, uid, false); err != nil {
		return err
	}
	s.afterLogin(r, uid)
	u, _ := s.loadUser(uid)
	return ok(w, map[string]any{"user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(cookieName); err == nil {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE id=?`, auth.SHA256Hex(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return ok(w, nil)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u *User) error {
	return ok(w, map[string]any{
		"user": u, "version": Version,
		"panelDomain": s.setting("panel_domain", ""),
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct{ Current, New string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	var hash string
	_ = s.db.QueryRow(`SELECT password_hash FROM users WHERE id=?`, u.ID).Scan(&hash)
	if !auth.CheckPassword(hash, req.Current) {
		return bad("当前密码不正确")
	}
	if len(req.New) < 10 {
		return bad("新密码至少 10 位")
	}
	if len(req.New) > 72 {
		return bad("密码长度不能超过 72 字节")
	}
	nh, err := auth.HashPassword(req.New)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE users SET password_hash=? WHERE id=?`, nh, u.ID)
	if err == nil {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE user_id=? AND id<>?`, u.ID, u.SessionID)
	}
	s.audit(r, u, "user.password", u.Username, "修改密码", err)
	return ok(w, nil)
}

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request, u *User) error {
	sec := auth.NewTOTPSecret()
	_, err := s.db.Exec(`UPDATE users SET totp_secret=? WHERE id=? AND totp_enabled=0`, s.box.Seal(sec), u.ID)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"secret": sec, "uri": auth.TOTPURI("Compose Panel", u.Username, sec)})
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct{ Code string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	var enc string
	_ = s.db.QueryRow(`SELECT totp_secret FROM users WHERE id=?`, u.ID).Scan(&enc)
	matchedStep, validCode := auth.TOTPVerifyStep(s.box.MustOpen(enc), req.Code, time.Now())
	if enc == "" || !validCode {
		return bad("验证码不正确")
	}
	var codes, hashes []string
	for i := 0; i < 8; i++ {
		c := strings.ToLower(auth.RandomPassword(10))
		codes = append(codes, c)
		hashes = append(hashes, auth.SHA256Hex(c))
	}
	_, err := s.db.Exec(`UPDATE users SET totp_enabled=1, last_totp_step=?, recovery_codes=? WHERE id=?`, matchedStep, strings.Join(hashes, "\n"), u.ID)
	s.audit(r, u, "user.totp", u.Username, "启用两步验证", err)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"recoveryCodes": codes})
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct{ Password string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	var hash string
	_ = s.db.QueryRow(`SELECT password_hash FROM users WHERE id=?`, u.ID).Scan(&hash)
	if !auth.CheckPassword(hash, req.Password) {
		return bad("密码不正确")
	}
	_, err := s.db.Exec(`UPDATE users SET totp_enabled=0, totp_secret='', last_totp_step=0, recovery_codes='' WHERE id=?`, u.ID)
	s.audit(r, u, "user.totp", u.Username, "关闭两步验证", err)
	return ok(w, nil)
}

func (s *Server) handleMySessions(w http.ResponseWriter, r *http.Request, u *User) error {
	rows, err := s.db.Query(`SELECT id, created_at, last_seen_at, ip, user_agent FROM sessions WHERE user_id=? AND mfa_pending=0 AND expires_at>? ORDER BY last_seen_at DESC`, u.ID, time.Now().Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, ip, ua string
		var created, seen int64
		if err := rows.Scan(&id, &created, &seen, &ip, &ua); err != nil {
			return err
		}
		out = append(out, map[string]any{"id": id[:16], "createdAt": created, "lastSeenAt": seen, "ip": ip, "userAgent": ua, "current": id == u.SessionID})
	}
	return ok(w, out)
}

func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request, u *User) error {
	prefix := r.PathValue("id")
	if len(prefix) != 16 {
		return errNotFound
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id=? AND substr(id,1,16)=?`, u.ID, prefix)
	return done(w, err)
}

// ---------- users (admin) ----------

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request, u *User) error {
	rows, err := s.db.Query(`SELECT id, username, email, role, totp_enabled, disabled, all_projects, created_at, last_login_at, last_login_ip FROM users ORDER BY id`)
	if err != nil {
		return err
	}
	type userListRow struct {
		id, created, last             int64
		name, email, role, ip         string
		totp, disabled, allProjects   int
	}
	var list []userListRow
	for rows.Next() {
		var r userListRow
		if err := rows.Scan(&r.id, &r.name, &r.email, &r.role, &r.totp, &r.disabled, &r.allProjects, &r.created, &r.last, &r.ip); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		var projects any
		if item.role != "admin" && item.allProjects == 0 {
			projects = s.userProjects(item.id)
		}
		out = append(out, map[string]any{
			"id": item.id, "username": item.name, "email": item.email, "role": item.role,
			"totp": item.totp == 1, "disabled": item.disabled == 1,
			"createdAt": item.created, "lastLoginAt": item.last, "lastLoginIp": item.ip,
			"projects": projects,
		})
	}
	return ok(w, out)
}

func (s *Server) userProjects(uid int64) []int64 {
	out := []int64{}
	rows, err := s.db.Query(`SELECT project_id FROM user_projects WHERE user_id=?`, uid)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

type userReq struct {
	Username string  `json:"username"`
	Email    string  `json:"email"`
	Role     string  `json:"role"`
	Disabled bool    `json:"disabled"`
	Projects []int64 `json:"projects"`
}

func (s *Server) setUserProjects(uid int64, ids []int64) {
	_, _ = s.db.Exec(`DELETE FROM user_projects WHERE user_id=?`, uid)
	for _, p := range ids {
		_, _ = s.db.Exec(`INSERT OR IGNORE INTO user_projects(user_id,project_id) VALUES(?,?)`, uid, p)
	}
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, u *User) error {
	var req userReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := validUsername(req.Username); err != nil {
		return err
	}
	if _, ok := roleLevel[req.Role]; !ok {
		return bad("角色无效")
	}
	allProjects := req.Role == "admin" || req.Projects == nil
	pw := auth.RandomPassword(16)
	hash, _ := auth.HashPassword(pw)
	res, err := s.db.Exec(`INSERT INTO users(username,email,password_hash,role,all_projects,created_at) VALUES(?,?,?,?,?,?)`,
		req.Username, req.Email, hash, req.Role, boolInt(allProjects), time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return bad("用户名已存在")
		}
		return err
	}
	id, _ := res.LastInsertId()
	if allProjects {
		s.setUserProjects(id, nil)
	} else {
		s.setUserProjects(id, req.Projects)
	}
	s.audit(r, u, "user.create", req.Username, "角色 "+req.Role, nil)
	s.notify.Emit(notify.Event{Type: "security_login", Title: "新增用户", Body: fmt.Sprintf("%s 创建了用户 %s（%s）", u.Username, req.Username, req.Role), Target: req.Username})
	return ok(w, map[string]any{"id": id, "password": pw})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req userReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if _, ok := roleLevel[req.Role]; !ok {
		return bad("角色无效")
	}
	if id == u.ID && (req.Role != "admin" || req.Disabled) {
		return bad("不能降低或停用自己的账号")
	}
	allProjects := req.Role == "admin" || req.Projects == nil
	_, err = s.db.Exec(`UPDATE users SET email=?, role=?, disabled=?, all_projects=? WHERE id=?`,
		req.Email, req.Role, boolInt(req.Disabled), boolInt(allProjects), id)
	if err != nil {
		return err
	}
	if req.Disabled {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	}
	if allProjects {
		s.setUserProjects(id, nil)
	} else {
		s.setUserProjects(id, req.Projects)
	}
	s.audit(r, u, "user.update", fmt.Sprint(id), fmt.Sprintf("角色 %s 停用 %v", req.Role, req.Disabled), nil)
	return ok(w, nil)
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	pw := auth.RandomPassword(16)
	hash, _ := auth.HashPassword(pw)
	res, err := s.db.Exec(`UPDATE users SET password_hash=?, totp_enabled=0, totp_secret='', last_totp_step=0, recovery_codes='' WHERE id=?`, hash, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	s.audit(r, u, "user.reset_password", fmt.Sprint(id), "重置密码并关闭两步验证", nil)
	return ok(w, map[string]any{"password": pw})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if id == u.ID {
		return bad("不能删除自己")
	}
	var name string
	_ = s.db.QueryRow(`SELECT username FROM users WHERE id=?`, id).Scan(&name)
	_, err = s.db.Exec(`DELETE FROM users WHERE id=?`, id)
	s.audit(r, u, "user.delete", name, "", err)
	return done(w, err)
}

// ---------- API tokens ----------

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request, u *User) error {
	q := `SELECT t.id, t.name, t.prefix, t.scope, t.created_at, t.expires_at, t.last_used_at, t.last_used_ip, u.username FROM api_tokens t JOIN users u ON u.id=t.user_id`
	args := []any{}
	if !u.can(roleAdmin) {
		q += ` WHERE t.user_id=?`
		args = append(args, u.ID)
	}
	rows, err := s.db.Query(q+` ORDER BY t.id DESC`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created, exp, used int64
		var name, prefix, scope, ip, owner string
		if err := rows.Scan(&id, &name, &prefix, &scope, &created, &exp, &used, &ip, &owner); err != nil {
			return err
		}
		out = append(out, map[string]any{"id": id, "name": name, "prefix": prefix, "scope": scope, "createdAt": created, "expiresAt": exp, "lastUsedAt": used, "lastUsedIp": ip, "owner": owner})
	}
	return ok(w, out)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request, u *User) error {
	if u.TokenScope != "" {
		return errForbidden
	}
	var req struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
		Days  int    `json:"days"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Name == "" {
		return bad("名称不能为空")
	}
	if req.Scope != "read" && req.Scope != "deploy" && req.Scope != "full" {
		return bad("权限范围无效")
	}
	if req.Scope == "deploy" && !u.can(roleOperator) {
		return errStatus(403, "当前角色无法创建 deploy 权限令牌")
	}
	if req.Scope == "full" && !u.can(roleAdmin) {
		return errStatus(403, "当前角色无法创建 full 权限令牌")
	}
	tok := "cpt_" + auth.RandomToken(24)
	var exp int64
	if req.Days > 0 {
		exp = time.Now().Add(time.Duration(req.Days) * 24 * time.Hour).Unix()
	}
	_, err := s.db.Exec(`INSERT INTO api_tokens(name,prefix,token_hash,user_id,scope,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`,
		req.Name, tok[:10], auth.SHA256Hex(tok), u.ID, req.Scope, time.Now().Unix(), exp)
	s.audit(r, u, "token.create", req.Name, "范围 "+req.Scope, err)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"token": tok})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	q, args := `DELETE FROM api_tokens WHERE id=?`, []any{id}
	if !u.can(roleAdmin) {
		q += ` AND user_id=?`
		args = append(args, u.ID)
	}
	_, err = s.db.Exec(q, args...)
	s.audit(r, u, "token.delete", fmt.Sprint(id), "", err)
	return done(w, err)
}

// ---------- audit log ----------

func (s *Server) auditQuery(r *http.Request) (string, []any) {
	q := `SELECT id, ts, actor, action, target, ip, result, detail, prev_hash, hash FROM audit_log WHERE 1=1`
	var args []any
	v := r.URL.Query()
	if a := v.Get("actor"); a != "" {
		q += ` AND actor LIKE ?`
		args = append(args, a+"%")
	}
	if a := v.Get("action"); a != "" {
		q += ` AND action LIKE ?`
		args = append(args, a+"%")
	}
	if a := v.Get("q"); a != "" {
		q += ` AND (target LIKE ? OR detail LIKE ?)`
		args = append(args, "%"+a+"%", "%"+a+"%")
	}
	if a := v.Get("result"); a != "" {
		q += ` AND result=?`
		args = append(args, a)
	}
	if d := queryInt(r, "days", 0); d > 0 {
		q += ` AND ts>=?`
		args = append(args, time.Now().Add(-time.Duration(d)*24*time.Hour).Unix())
	}
	return q, args
}

type auditRow struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	IP       string `json:"ip"`
	Result   string `json:"result"`
	Detail   string `json:"detail"`
	PrevHash string `json:"-"`
	Hash     string `json:"hash"`
}

func scanAudit(rows *sql.Rows) ([]auditRow, error) {
	defer rows.Close()
	out := []auditRow{}
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.ID, &a.TS, &a.Actor, &a.Action, &a.Target, &a.IP, &a.Result, &a.Detail, &a.PrevHash, &a.Hash); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, u *User) error {
	q, args := s.auditQuery(r)
	limit := min(max(queryInt(r, "limit", 100), 1), 500)
	offset := max(queryInt(r, "offset", 0), 0)
	rows, err := s.db.Query(q+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return err
	}
	list, err := scanAudit(rows)
	if err != nil {
		return err
	}
	var total int
	_ = s.db.QueryRow(`SELECT count(*) FROM (`+q+`)`, args...).Scan(&total)
	return ok(w, map[string]any{"items": list, "total": total})
}

func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request, u *User) error {
	loc := s.location()
	q, args := s.auditQuery(r)
	rows, err := s.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-`+time.Now().Format("20060102")+`.csv"`)
	_, _ = w.Write([]byte("\xEF\xBB\xBF"))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "time", "actor", "action", "target", "ip", "result", "detail", "hash"})
	count := 0
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.ID, &a.TS, &a.Actor, &a.Action, &a.Target, &a.IP, &a.Result, &a.Detail, &a.PrevHash, &a.Hash); err != nil {
			break
		}
		_ = cw.Write([]string{fmt.Sprint(a.ID), time.Unix(a.TS, 0).In(loc).Format(time.RFC3339), a.Actor, a.Action, a.Target, a.IP, a.Result, a.Detail, a.Hash})
		count++
	}
	cw.Flush()
	rows.Close()
	s.audit(r, u, "audit.export", "", fmt.Sprintf("%d 条", count), nil)
	return nil
}

func (s *Server) handleAuditVerify(w http.ResponseWriter, r *http.Request, u *User) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	prev := s.setting("audit_anchor_hash", "")
	rows, err := s.db.Query(`SELECT id, ts, actor, action, target, ip, result, detail, prev_hash, hash FROM audit_log ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.ID, &a.TS, &a.Actor, &a.Action, &a.Target, &a.IP, &a.Result, &a.Detail, &a.PrevHash, &a.Hash); err != nil {
			return err
		}
		count++
		if a.PrevHash != prev || auditHash(a.PrevHash, a.TS, a.Actor, a.Action, a.Target, a.IP, a.Result, a.Detail) != a.Hash {
			return ok(w, map[string]any{"ok": false, "brokenAt": a.ID, "count": count})
		}
		prev = a.Hash
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ok(w, map[string]any{"ok": true, "count": count})
}
