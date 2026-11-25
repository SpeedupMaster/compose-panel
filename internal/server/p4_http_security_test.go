package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"compose-panel/internal/config"
)

func newP4HTTPTestServer(t *testing.T) *Server {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.Config{
		DataDir:     filepath.Join(tmp, "data"),
		ProjectsDir: filepath.Join(tmp, "projects"),
		BackupsDir:  filepath.Join(tmp, "backups"),
	}
	webFS := fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><html><body>ok</body></html>")},
	}
	s, err := New(cfg, webFS)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { _ = s.db.Close() })
	return s
}

func TestSecurityHeadersIncludeCSP(t *testing.T) {
	s := newP4HTTPTestServer(t)
	h := s.Handler()

	for _, target := range []string{"/", "/api/setup/status"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
			t.Errorf("GET %s CSP = %q, want %q", target, got, contentSecurityPolicy)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q, want nosniff", target, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("GET %s X-Frame-Options = %q, want DENY", target, got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("GET %s Referrer-Policy = %q, want same-origin", target, got)
		}
	}
}

func TestCrossOriginProtection(t *testing.T) {
	s := newP4HTTPTestServer(t)
	h := s.Handler()

	decodeErr := func(t *testing.T, body []byte) string {
		t.Helper()
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode JSON %q: %v", string(body), err)
		}
		msg, _ := payload["error"].(string)
		return msg
	}

	// 1. Public & authed mutating /api/ routes reject cross-site & same-site (subdomain) CSRF with JSON 403.
	publicEndpoints := []string{
		"/api/auth/login",
		"/api/auth/logout",
		"/api/auth/totp",
		"/api/setup",
		"/api/setup/restore",
	}
	for _, ep := range publicEndpoints {
		for _, tc := range []struct {
			name   string
			sec    string
			origin string
		}{
			{"cross-site", "cross-site", "https://evil.example.com"},
			{"subdomain-same-site", "same-site", "https://sub.panel.local"},
			{"origin-mismatch-no-sec", "", "https://evil.example.com"},
		} {
			req := httptest.NewRequest(http.MethodPost, "https://panel.local"+ep, strings.NewReader(`{}`))
			req.Host = "panel.local"
			if tc.sec != "" {
				req.Header.Set("Sec-Fetch-Site", tc.sec)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s (%s) status = %d, want 403 (body=%s)", ep, tc.name, rec.Code, rec.Body.String())
				continue
			}
			if got := decodeErr(t, rec.Body.Bytes()); got != "跨源请求被拒绝" {
				t.Errorf("POST %s (%s) error = %q, want %q", ep, tc.name, got, "跨源请求被拒绝")
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
				t.Errorf("POST %s (%s) missing CSP header on 403", ep, tc.name)
			}
		}
	}

	// 2. Same-origin POST is allowed through CrossOriginProtection to the route handler.
	sameOriginReq := httptest.NewRequest(http.MethodPost, "https://panel.local/api/auth/logout", nil)
	sameOriginReq.Host = "panel.local"
	sameOriginReq.Header.Set("Sec-Fetch-Site", "same-origin")
	sameOriginReq.Header.Set("Origin", "https://panel.local")
	sameOriginRec := httptest.NewRecorder()
	h.ServeHTTP(sameOriginRec, sameOriginReq)
	if sameOriginRec.Code != http.StatusOK {
		t.Errorf("same-origin POST /api/auth/logout status = %d, want 200 (body=%s)", sameOriginRec.Code, sameOriginRec.Body.String())
	}

	// 3. Safe GET method is allowed even with Sec-Fetch-Site: cross-site.
	getReq := httptest.NewRequest(http.MethodGet, "https://panel.local/api/setup/status", nil)
	getReq.Host = "panel.local"
	getReq.Header.Set("Sec-Fetch-Site", "cross-site")
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Errorf("cross-site GET /api/setup/status status = %d, want 200", getRec.Code)
	}

	// 4. Webhook endpoint /hooks/p/{token} bypasses CrossOriginProtection.
	hookReq := httptest.NewRequest(http.MethodPost, "https://panel.local/hooks/p/unknown-token", strings.NewReader(`{}`))
	hookReq.Host = "panel.local"
	hookReq.Header.Set("Sec-Fetch-Site", "cross-site")
	hookReq.Header.Set("Origin", "https://github.com")
	hookRec := httptest.NewRecorder()
	h.ServeHTTP(hookRec, hookReq)
	if hookRec.Code == http.StatusForbidden {
		t.Errorf("POST /hooks/p/unknown-token returned 403, want webhook bypass (body=%s)", hookRec.Body.String())
	}

	// 5. Requests with non-empty Authorization: Bearer token bypass CrossOriginProtection.
	bearerReq := httptest.NewRequest(http.MethodPost, "https://panel.local/api/auth/logout", nil)
	bearerReq.Host = "panel.local"
	bearerReq.Header.Set("Authorization", "Bearer cp_test_api_token")
	bearerReq.Header.Set("Sec-Fetch-Site", "cross-site")
	bearerReq.Header.Set("Origin", "https://ci.example.com")
	bearerRec := httptest.NewRecorder()
	h.ServeHTTP(bearerRec, bearerReq)
	if bearerRec.Code != http.StatusOK {
		t.Errorf("Bearer cross-site POST /api/auth/logout status = %d, want 200 (body=%s)", bearerRec.Code, bearerRec.Body.String())
	}

	// 6. Blank Authorization: Bearer header does NOT bypass CrossOriginProtection.
	emptyBearerReq := httptest.NewRequest(http.MethodPost, "https://panel.local/api/auth/logout", nil)
	emptyBearerReq.Host = "panel.local"
	emptyBearerReq.Header.Set("Authorization", "Bearer   ")
	emptyBearerReq.Header.Set("Sec-Fetch-Site", "cross-site")
	emptyBearerRec := httptest.NewRecorder()
	h.ServeHTTP(emptyBearerRec, emptyBearerReq)
	if emptyBearerRec.Code != http.StatusForbidden {
		t.Errorf("blank Bearer cross-site POST status = %d, want 403", emptyBearerRec.Code)
	}
}

func TestWriteErrSanitizes500AndMapsMaxBytesTo413(t *testing.T) {
	decodeErr := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode JSON %q: %v", rec.Body.String(), err)
		}
		msg, _ := payload["error"].(string)
		return msg
	}

	// 1. Untyped internal error returns sanitized 500 and logs full detail via slog.Error.
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	rec := httptest.NewRecorder()
	secretErr := errors.New("sqlite: /var/lib/compose-panel/panel.db: disk I/O error")
	writeErr(rec, secretErr)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("untyped err status = %d, want 500", rec.Code)
	}
	if got := decodeErr(t, rec); got != "服务器内部错误，请查看面板日志" {
		t.Errorf("untyped err message = %q, want %q", got, "服务器内部错误，请查看面板日志")
	}
	if strings.Contains(rec.Body.String(), "panel.db") || strings.Contains(rec.Body.String(), "sqlite") {
		t.Errorf("500 response leaked raw error details: %s", rec.Body.String())
	}
	if !strings.Contains(logBuf.String(), "api internal error") || !strings.Contains(logBuf.String(), "panel.db") {
		t.Errorf("expected slog.Error to record full error, got %q", logBuf.String())
	}

	// 2. Direct and wrapped *http.MaxBytesError map to 413.
	for _, err := range []error{
		&http.MaxBytesError{Limit: 1024},
		fmt.Errorf("read upload: %w", &http.MaxBytesError{Limit: 2048}),
	} {
		rec := httptest.NewRecorder()
		writeErr(rec, err)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("MaxBytesError status = %d, want 413", rec.Code)
		}
		if got := decodeErr(t, rec); got != "上传内容超过大小限制" {
			t.Errorf("MaxBytesError message = %q, want %q", got, "上传内容超过大小限制")
		}
	}

	// 3. Overflowing http.MaxBytesReader through readJSON maps to 413 via writeErr.
	req := httptest.NewRequest(http.MethodPost, "/api/test", strings.NewReader(`{"data":"0123456789abcdef"}`))
	recMax := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(recMax, req.Body, 8)
	var body map[string]string
	err := readJSON(req, &body)
	if err == nil {
		t.Fatalf("readJSON on overflowing MaxBytesReader returned nil")
	}
	writeErr(recMax, err)
	if recMax.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("readJSON MaxBytesReader status = %d, want 413 (body=%s)", recMax.Code, recMax.Body.String())
	}
	if got := decodeErr(t, recMax); got != "上传内容超过大小限制" {
		t.Errorf("readJSON MaxBytesReader message = %q, want %q", got, "上传内容超过大小限制")
	}

	// Also verify raw io.ReadAll MaxBytesError maps to 413.
	req2 := httptest.NewRequest(http.MethodPost, "/api/upload", strings.NewReader("0123456789"))
	recMax2 := httptest.NewRecorder()
	req2.Body = http.MaxBytesReader(recMax2, req2.Body, 4)
	_, rerr := io.ReadAll(req2.Body)
	writeErr(recMax2, rerr)
	if recMax2.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("io.ReadAll MaxBytesReader status = %d, want 413", recMax2.Code)
	}

	// 4. Typed *apiError statuses (400/401/403/404/409/429) preserve exact message.
	for _, tc := range []struct {
		status int
		msg    string
	}{
		{400, "参数无效"},
		{401, "未登录"},
		{403, "权限不足"},
		{404, "项目不存在"},
		{409, "名称冲突"},
		{429, "请求过于频繁"},
	} {
		rec := httptest.NewRecorder()
		writeErr(rec, errStatus(tc.status, "%s", tc.msg))
		if rec.Code != tc.status {
			t.Errorf("apiError %d status = %d", tc.status, rec.Code)
		}
		if got := decodeErr(t, rec); got != tc.msg {
			t.Errorf("apiError %d message = %q, want %q", tc.status, got, tc.msg)
		}
	}

	// 5. sql.ErrNoRows still maps to 404.
	recNoRows := httptest.NewRecorder()
	writeErr(recNoRows, fmt.Errorf("lookup: %w", sql.ErrNoRows))
	if recNoRows.Code != http.StatusNotFound {
		t.Errorf("sql.ErrNoRows status = %d, want 404", recNoRows.Code)
	}
	if got := decodeErr(t, recNoRows); got != "未找到" {
		t.Errorf("sql.ErrNoRows message = %q, want %q", got, "未找到")
	}
}
