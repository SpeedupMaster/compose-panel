package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"compose-panel/internal/auth"
	"compose-panel/internal/backup"
)

// ---------- Google Drive OAuth 2.0 helper ----------

const gdriveOAuthTTL = 10 * time.Minute

type gdriveOAuthState struct {
	UserID       int64
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Origin       string
	Verifier     string
	CreatedAt    time.Time
	Processing   bool
	Done         bool
	RefreshToken string
	Err          string
}

func (s *Server) pruneGDriveAuthLocked(now time.Time) {
	for k, v := range s.gdriveAuth {
		if now.Sub(v.CreatedAt) > gdriveOAuthTTL {
			delete(s.gdriveAuth, k)
		}
	}
}

func (s *Server) handleGDriveAuthURL(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		ID           int64  `json:"id"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		RedirectURI  string `json:"redirectUri"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.ID > 0 {
		cfg := backup.TargetConfig{ClientSecret: req.ClientSecret}
		s.mergeSavedStorageSecrets(req.ID, &cfg)
		req.ClientSecret = cfg.ClientSecret
	}
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.ClientSecret = strings.TrimSpace(req.ClientSecret)
	req.RedirectURI = strings.TrimSpace(req.RedirectURI)
	if req.ClientID == "" || req.ClientSecret == "" {
		return bad("请先填写 Client ID 和 Client Secret")
	}
	ru, err := url.Parse(req.RedirectURI)
	if err != nil || (ru.Scheme != "http" && ru.Scheme != "https") || ru.Host == "" || !strings.HasSuffix(ru.Path, "/api/storage/gdrive/callback") {
		return bad("回调地址无效")
	}

	state := auth.RandomToken(32)
	verifier := auth.RandomToken(32)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	s.gdriveMu.Lock()
	now := time.Now()
	s.pruneGDriveAuthLocked(now)
	s.gdriveAuth[state] = &gdriveOAuthState{
		UserID:       u.ID,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		RedirectURI:  req.RedirectURI,
		Origin:       ru.Scheme + "://" + ru.Host,
		Verifier:     verifier,
		CreatedAt:    now,
	}
	s.gdriveMu.Unlock()

	return ok(w, map[string]any{
		"url":   backup.GDriveAuthURL(req.ClientID, req.RedirectURI, state, challenge),
		"state": state,
	})
}

func (s *Server) handleGDriveCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	code := strings.TrimSpace(q.Get("code"))
	errParam := strings.TrimSpace(q.Get("error"))
	errDesc := strings.TrimSpace(q.Get("error_description"))

	s.gdriveMu.Lock()
	now := time.Now()
	s.pruneGDriveAuthLocked(now)
	entry, okEntry := s.gdriveAuth[state]
	if !okEntry || entry.Done || entry.Processing {
		s.gdriveMu.Unlock()
		writeGDriveCallbackHTML(w, http.StatusBadRequest, false, "授权会话无效或已过期，请返回面板重新发起授权。", "", "")
		return
	}
	entry.Processing = true
	clientID := entry.ClientID
	clientSecret := entry.ClientSecret
	redirectURI := entry.RedirectURI
	verifier := entry.Verifier
	origin := entry.Origin
	s.gdriveMu.Unlock()

	var refreshToken, errMsg string
	switch {
	case errParam != "":
		if errParam == "access_denied" {
			errMsg = "已取消 Google 授权"
		} else if errDesc != "" {
			errMsg = fmt.Sprintf("Google 授权失败：%s", errDesc)
		} else {
			errMsg = fmt.Sprintf("Google 授权失败：%s", errParam)
		}
	case code == "":
		errMsg = "Google 未返回授权码"
	default:
		exchange := s.gdriveExchange
		if exchange == nil {
			exchange = backup.GDriveExchangeCode
		}
		tok, err := exchange(r.Context(), clientID, clientSecret, code, redirectURI, verifier)
		if err != nil {
			errMsg = err.Error()
		} else {
			refreshToken = tok
		}
	}

	s.gdriveMu.Lock()
	if cur, exists := s.gdriveAuth[state]; exists {
		cur.RefreshToken = refreshToken
		cur.Err = errMsg
		cur.Done = true
		cur.Processing = false
	}
	s.gdriveMu.Unlock()

	if errMsg != "" {
		writeGDriveCallbackHTML(w, http.StatusOK, false, errMsg, state, origin)
		return
	}
	writeGDriveCallbackHTML(w, http.StatusOK, true, "Google Drive 授权成功，正在返回面板…", state, origin)
}

func (s *Server) handleGDriveAuthResult(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		State string `json:"state"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return bad("缺少授权状态参数")
	}

	s.gdriveMu.Lock()
	defer s.gdriveMu.Unlock()
	s.pruneGDriveAuthLocked(time.Now())
	entry, exists := s.gdriveAuth[state]
	if !exists || entry.UserID != u.ID {
		return errNotFound
	}
	if !entry.Done {
		return ok(w, map[string]any{"status": "pending"})
	}
	delete(s.gdriveAuth, state)
	if entry.Err != "" {
		return ok(w, map[string]any{"status": "error", "error": entry.Err})
	}
	return ok(w, map[string]any{"status": "ok", "refreshToken": entry.RefreshToken})
}

func writeGDriveCallbackHTML(w http.ResponseWriter, status int, success bool, msg, state, origin string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	title := "Google Drive 授权失败"
	color := "#A41E14"
	if success {
		title = "Google Drive 授权完成"
		color = "#166534"
	}
	var script string
	if state != "" {
		stateJS, _ := json.Marshal(state)
		originJS, _ := json.Marshal(origin)
		script = fmt.Sprintf(`<script>
(function() {
  var payload = { type: "gdrive-oauth-done", state: %s };
  try {
    if ("BroadcastChannel" in window) {
      var ch = new BroadcastChannel("compose-panel-gdrive-oauth");
      ch.postMessage(payload);
      ch.close();
    }
  } catch (e) {}
  try {
    if (window.opener && %s) {
      window.opener.postMessage(payload, %s);
    }
  } catch (e) {}
  setTimeout(function() { window.close(); }, 600);
})();
</script>`, stateJS, originJS, originJS)
	}
	fmt.Fprintf(w, `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<style>body{font-family:system-ui,-apple-system,sans-serif;background:#F4F4F2;color:#18191B;display:grid;place-items:center;min-height:100vh;margin:0;padding:24px}.box{background:#fff;border:1px solid #E2E2DE;border-radius:10px;padding:24px 28px;max-width:440px;width:100%%;box-shadow:0 8px 24px rgba(0,0,0,.06)}h1{margin:0 0 10px;font-size:18px;color:%s}p{margin:0 0 16px;font-size:14px;line-height:1.5;color:#5F6268}button{height:34px;padding:0 14px;border-radius:6px;border:1px solid #D6D6D1;background:#fff;cursor:pointer;font-size:14px}</style>
</head>
<body><div class="box"><h1>%s</h1><p>%s</p><button onclick="window.close()">关闭窗口</button></div>%s</body>
</html>`, html.EscapeString(title), color, html.EscapeString(title), html.EscapeString(msg), script)
}
