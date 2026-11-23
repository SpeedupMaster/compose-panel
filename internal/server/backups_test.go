package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compose-panel/internal/backup"
	"compose-panel/internal/config"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	tmp := t.TempDir()
	srv, err := New(config.Config{
		DataDir:     tmp,
		ProjectsDir: tmp,
		BackupsDir:  tmp,
		DockerHost:  "unix:///nonexistent.sock",
	}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { _ = srv.DB().Close() })
	return srv
}

func TestGDriveStorageSaveMaskAndPreserveSecrets(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	createBody := `{
		"name": "我的云盘",
		"kind": "gdrive",
		"config": {
			"clientId": "cid-123.apps.googleusercontent.com",
			"clientSecret": "real-secret-xyz",
			"refreshToken": "1//real-refresh-token",
			"folderId": "https://drive.google.com/drive/folders/1FolderABC_99?usp=sharing",
			"prefix": "panel/"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/storage", strings.NewReader(createBody))
	w := httptest.NewRecorder()
	if err := srv.handleSaveStorage(w, req, admin); err != nil {
		t.Fatalf("handleSaveStorage(create) error: %v", err)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("unexpected create response: %s", w.Body.String())
	}

	// List must mask clientSecret & refreshToken and expose normalized folderId.
	w = httptest.NewRecorder()
	if err := srv.handleListStorage(w, httptest.NewRequest(http.MethodGet, "/api/storage", nil), admin); err != nil {
		t.Fatalf("handleListStorage() error: %v", err)
	}
	var listed []struct {
		Target storageTarget `json:"target"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	var found *storageTarget
	for i := range listed {
		if listed[i].Target.ID == created.ID {
			found = &listed[i].Target
			break
		}
	}
	if found == nil {
		t.Fatalf("created storage id %d not found in list", created.ID)
	}
	if found.Config.ClientSecret != "••••••" {
		t.Errorf("listed ClientSecret = %q, want masked", found.Config.ClientSecret)
	}
	if found.Config.RefreshToken != "••••••" {
		t.Errorf("listed RefreshToken = %q, want masked", found.Config.RefreshToken)
	}
	if found.Config.FolderID != "1FolderABC_99" {
		t.Errorf("listed FolderID = %q, want normalized %q", found.Config.FolderID, "1FolderABC_99")
	}

	// Update keeping masked clientSecret and blank refreshToken must preserve real secrets.
	updateBody := `{
		"name": "我的云盘-新",
		"kind": "gdrive",
		"config": {
			"clientId": "cid-123.apps.googleusercontent.com",
			"clientSecret": "••••••",
			"refreshToken": "",
			"folderId": "1FolderABC_99",
			"prefix": "prod/"
		}
	}`
	upReq := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/storage/%d", created.ID), strings.NewReader(updateBody))
	upReq.SetPathValue("id", fmt.Sprint(created.ID))
	w = httptest.NewRecorder()
	if err := srv.handleSaveStorage(w, upReq, admin); err != nil {
		t.Fatalf("handleSaveStorage(update) error: %v", err)
	}

	stored, _, err := srv.getStorage(created.ID)
	if err != nil {
		t.Fatalf("getStorage() error: %v", err)
	}
	if stored.Config.ClientSecret != "real-secret-xyz" {
		t.Errorf("preserved ClientSecret = %q, want %q", stored.Config.ClientSecret, "real-secret-xyz")
	}
	if stored.Config.RefreshToken != "1//real-refresh-token" {
		t.Errorf("preserved RefreshToken = %q, want %q", stored.Config.RefreshToken, "1//real-refresh-token")
	}
	if stored.Config.Prefix != "prod/" {
		t.Errorf("updated Prefix = %q, want %q", stored.Config.Prefix, "prod/")
	}
}

func TestGDriveOAuthFullFlow(t *testing.T) {
	srv := newTestServer(t)
	var gotClientID, gotClientSecret, gotCode, gotRedirect, gotVerifier string
	srv.gdriveExchange = func(_ context.Context, clientID, clientSecret, code, redirectURI, verifier string) (string, error) {
		gotClientID, gotClientSecret, gotCode, gotRedirect, gotVerifier = clientID, clientSecret, code, redirectURI, verifier
		return "1//oauth-refresh-token-secret", nil
	}

	admin1 := &User{ID: 10, Username: "admin1", Role: "admin", Level: roleAdmin}
	admin2 := &User{ID: 20, Username: "admin2", Role: "admin", Level: roleAdmin}
	redirectURI := "https://panel.example.com/api/storage/gdrive/callback"

	// 1. Start OAuth flow.
	w := httptest.NewRecorder()
	startReq := httptest.NewRequest(http.MethodPost, "/api/storage/gdrive/auth-url", strings.NewReader(fmt.Sprintf(
		`{"clientId":"cid-1","clientSecret":"csec-1","redirectUri":%q}`, redirectURI,
	)))
	if err := srv.handleGDriveAuthURL(w, startReq, admin1); err != nil {
		t.Fatalf("handleGDriveAuthURL() error: %v", err)
	}
	var startResp struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &startResp); err != nil {
		t.Fatalf("unmarshal startResp: %v", err)
	}
	if startResp.State == "" {
		t.Fatalf("empty state in auth-url response")
	}
	parsedAuth, err := url.Parse(startResp.URL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := parsedAuth.Query()
	if q.Get("state") != startResp.State || q.Get("client_id") != "cid-1" || q.Get("redirect_uri") != redirectURI {
		t.Fatalf("unexpected auth URL query: %v", q)
	}
	challenge := q.Get("code_challenge")
	if challenge == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("missing PKCE challenge in auth URL: %v", q)
	}

	// 2. Polling before callback returns pending.
	w = httptest.NewRecorder()
	pollReq := httptest.NewRequest(http.MethodPost, "/api/storage/gdrive/auth-result", strings.NewReader(fmt.Sprintf(`{"state":%q}`, startResp.State)))
	if err := srv.handleGDriveAuthResult(w, pollReq, admin1); err != nil {
		t.Fatalf("handleGDriveAuthResult(pending) error: %v", err)
	}
	if !strings.Contains(w.Body.String(), `"pending"`) {
		t.Fatalf("expected pending status, got %s", w.Body.String())
	}

	// 3. Google redirects to callback WITHOUT session cookies (SameSite=Strict).
	cbURL := "/api/storage/gdrive/callback?state=" + url.QueryEscape(startResp.State) + "&code=google-code-42"
	cbW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(cbW, httptest.NewRequest(http.MethodGet, cbURL, nil))
	if cbW.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200; body=%s", cbW.Code, cbW.Body.String())
	}
	if strings.Contains(cbW.Body.String(), "1//oauth-refresh-token-secret") {
		t.Fatalf("callback HTML must NOT contain refresh token!")
	}
	if gotClientID != "cid-1" || gotClientSecret != "csec-1" || gotCode != "google-code-42" || gotRedirect != redirectURI {
		t.Fatalf("exchange called with wrong args: cid=%q csec=%q code=%q redirect=%q", gotClientID, gotClientSecret, gotCode, gotRedirect)
	}
	sum := sha256.Sum256([]byte(gotVerifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		t.Fatalf("PKCE verifier does not match code_challenge")
	}

	// 4. Replay of callback with same state must fail (400).
	replayW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(replayW, httptest.NewRequest(http.MethodGet, cbURL, nil))
	if replayW.Code != http.StatusBadRequest {
		t.Fatalf("callback replay status = %d, want 400", replayW.Code)
	}

	// 5. Different user cannot claim the token.
	w = httptest.NewRecorder()
	otherReq := httptest.NewRequest(http.MethodPost, "/api/storage/gdrive/auth-result", strings.NewReader(fmt.Sprintf(`{"state":%q}`, startResp.State)))
	if err := srv.handleGDriveAuthResult(w, otherReq, admin2); err == nil {
		t.Fatalf("other admin claimed state, want error")
	}

	// 6. Original admin claims refresh token once.
	w = httptest.NewRecorder()
	claimReq := httptest.NewRequest(http.MethodPost, "/api/storage/gdrive/auth-result", strings.NewReader(fmt.Sprintf(`{"state":%q}`, startResp.State)))
	if err := srv.handleGDriveAuthResult(w, claimReq, admin1); err != nil {
		t.Fatalf("handleGDriveAuthResult(ok) error: %v", err)
	}
	var res struct {
		Status       string `json:"status"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal claim result: %v", err)
	}
	if res.Status != "ok" || res.RefreshToken != "1//oauth-refresh-token-secret" {
		t.Fatalf("claim result = %+v, want status=ok and refreshToken", res)
	}

	// 7. Second claim with same state fails (single-use).
	w = httptest.NewRecorder()
	againReq := httptest.NewRequest(http.MethodPost, "/api/storage/gdrive/auth-result", strings.NewReader(fmt.Sprintf(`{"state":%q}`, startResp.State)))
	if err := srv.handleGDriveAuthResult(w, againReq, admin1); err == nil {
		t.Fatalf("second claim succeeded, want error")
	}
}

func TestGDriveOAuthAuthURLUsesSavedSecretWhenEditing(t *testing.T) {
	srv := newTestServer(t)
	var exchangedSecret string
	srv.gdriveExchange = func(_ context.Context, _, clientSecret, _, _, _ string) (string, error) {
		exchangedSecret = clientSecret
		return "1//new-tok", nil
	}
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Create saved storage first.
	req := httptest.NewRequest(http.MethodPost, "/api/storage", strings.NewReader(`{
		"name":"gd",
		"kind":"gdrive",
		"config":{"clientId":"cid","clientSecret":"stored-secret","refreshToken":"1//old-tok"}
	}`))
	w := httptest.NewRecorder()
	if err := srv.handleSaveStorage(w, req, admin); err != nil {
		t.Fatalf("save storage: %v", err)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	// Start auth-url with masked clientSecret and storage id.
	w = httptest.NewRecorder()
	startReq := httptest.NewRequest(http.MethodPost, "/api/storage/gdrive/auth-url", strings.NewReader(fmt.Sprintf(
		`{"id":%d,"clientId":"cid","clientSecret":"••••••","redirectUri":"https://panel.example.com/api/storage/gdrive/callback"}`, created.ID,
	)))
	if err := srv.handleGDriveAuthURL(w, startReq, admin); err != nil {
		t.Fatalf("handleGDriveAuthURL(edit) error: %v", err)
	}
	var startResp struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &startResp)

	cbW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(cbW, httptest.NewRequest(http.MethodGet, "/api/storage/gdrive/callback?state="+url.QueryEscape(startResp.State)+"&code=c1", nil))
	if cbW.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", cbW.Code)
	}
	if exchangedSecret != "stored-secret" {
		t.Fatalf("exchanged clientSecret = %q, want %q", exchangedSecret, "stored-secret")
	}
}

func TestGDriveOAuthValidationAndCancel(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Reject redirect URI that does not target callback path.
	req := httptest.NewRequest(http.MethodPost, "/api/storage/gdrive/auth-url", strings.NewReader(
		`{"clientId":"cid","clientSecret":"csec","redirectUri":"https://evil.example.com/steal"}`,
	))
	err := srv.handleGDriveAuthURL(httptest.NewRecorder(), req, admin)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("invalid redirectUri err = %v, want 400 *apiError", err)
	}

	// User cancels consent on Google -> callback records error -> auth-result returns status=error.
	w := httptest.NewRecorder()
	okStart := httptest.NewRequest(http.MethodPost, "/api/storage/gdrive/auth-url", strings.NewReader(
		`{"clientId":"cid","clientSecret":"csec","redirectUri":"https://panel.example.com/api/storage/gdrive/callback"}`,
	))
	if err := srv.handleGDriveAuthURL(w, okStart, admin); err != nil {
		t.Fatalf("auth-url error: %v", err)
	}
	var startResp struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &startResp)

	cbW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(cbW, httptest.NewRequest(http.MethodGet, "/api/storage/gdrive/callback?state="+url.QueryEscape(startResp.State)+"&error=access_denied", nil))
	if cbW.Code != http.StatusOK {
		t.Fatalf("cancel callback status = %d, want 200", cbW.Code)
	}

	resW := httptest.NewRecorder()
	if err := srv.handleGDriveAuthResult(resW, httptest.NewRequest(http.MethodPost, "/api/storage/gdrive/auth-result", strings.NewReader(fmt.Sprintf(`{"state":%q}`, startResp.State))), admin); err != nil {
		t.Fatalf("auth-result error: %v", err)
	}
	var res struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(resW.Body.Bytes(), &res)
	if res.Status != "error" || res.Error == "" {
		t.Fatalf("expected error status after access_denied, got %+v", res)
	}
}

func TestMergeStorageSecretsForEditTest(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Create a gdrive storage target.
	req := httptest.NewRequest(http.MethodPost, "/api/storage", strings.NewReader(`{
		"name":"gd",
		"kind":"gdrive",
		"config":{"clientId":"cid","clientSecret":"real-csec","refreshToken":"1//real-rtok"}
	}`))
	w := httptest.NewRecorder()
	if err := srv.handleSaveStorage(w, req, admin); err != nil {
		t.Fatalf("save storage: %v", err)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	cfg := backup.TargetConfig{
		ClientID:     "cid",
		ClientSecret: "••••••",
		RefreshToken: "",
		FolderID:     "new-folder",
	}
	srv.mergeSavedStorageSecrets(created.ID, &cfg)
	if cfg.ClientSecret != "real-csec" || cfg.RefreshToken != "1//real-rtok" {
		t.Fatalf("merged secrets = (%q, %q), want (real-csec, 1//real-rtok)", cfg.ClientSecret, cfg.RefreshToken)
	}
}

func TestDetectProjectDirsFromComposeAndDisk(t *testing.T) {
	srv := newTestServer(t)
	projDir := filepath.Join(srv.cfg.ProjectsDir, "wechat-bridge")
	for _, d := range []string{
		"data/wechat-token",
		"data/wechat-config",
		"data/wechat-data",
		"data/wechat-scratch",
		"data/bridge-data",
		"uploads",
		".panel",
		".git",
		"node_modules",
	} {
		if err := os.MkdirAll(filepath.Join(projDir, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(projDir, "nginx.conf"), []byte("events {}"), 0o644); err != nil {
		t.Fatalf("write nginx.conf: %v", err)
	}

	p := &Project{
		ID:     1,
		Name:   "wechat-bridge",
		Kind:   "app",
		Source: "inline",
		Compose: `services:
  bridge:
    image: example/bridge:latest
    network_mode: none
    volumes:
      - ./data/wechat-token:/token
      - ./data/wechat-config:/config:ro
      - ./data/wechat-data:/wechat-data
      - ./data/wechat-scratch:/wechat-scratch
      - ./data/bridge-data:/bridge-data
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
      - ./missing.yaml:/app/missing.yaml
      - redis_data:/redis
      - /var/run/docker.sock:/var/run/docker.sock
      - ../outside:/outside
`,
	}

	got := srv.detectProjectDirs(p)
	want := []backupDirOption{
		{Path: "data", Label: "包含 5 个挂载目录", Hint: "推荐"},
		{Path: "data/bridge-data", Label: "挂载 → /bridge-data", Hint: "挂载"},
		{Path: "data/wechat-config", Label: "挂载 → /config", Hint: "挂载"},
		{Path: "data/wechat-data", Label: "挂载 → /wechat-data", Hint: "挂载"},
		{Path: "data/wechat-scratch", Label: "挂载 → /wechat-scratch", Hint: "挂载"},
		{Path: "data/wechat-token", Label: "挂载 → /token", Hint: "挂载"},
		{Path: "uploads", Label: "项目子目录", Hint: "目录"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("detectProjectDirs() =\n%#v\nwant:\n%#v", got, want)
	}
}

func TestDetectProjectDirsLongSyntaxEnvAndGit(t *testing.T) {
	srv := newTestServer(t)

	t.Run("long bind syntax with env expansion and nested parent dedup", func(t *testing.T) {
		projDir := filepath.Join(srv.cfg.ProjectsDir, "long-app")
		for _, d := range []string{"custom/nested/one", "custom/nested/two"} {
			if err := os.MkdirAll(filepath.Join(projDir, filepath.FromSlash(d)), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}
		p := &Project{
			ID:     2,
			Name:   "long-app",
			Kind:   "app",
			Source: "inline",
			Env:    []EnvVar{{Key: "DATA_ROOT", Value: "./custom"}},
			Compose: `services:
  app:
    image: app:latest
    volumes:
      - type: bind
        source: ${DATA_ROOT}/nested/one
        target: /app/one
      - type: bind
        source: ${FALLBACK_DIR:-./custom}/nested/two
        target: /app/two
      - type: volume
        source: named_vol
        target: /app/vol
`,
		}
		got := srv.detectProjectDirs(p)
		want := []backupDirOption{
			{Path: "custom/nested", Label: "包含 2 个挂载目录", Hint: "推荐"},
			{Path: "custom/nested/one", Label: "挂载 → /app/one", Hint: "挂载"},
			{Path: "custom/nested/two", Label: "挂载 → /app/two", Hint: "挂载"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("detectProjectDirs() =\n%#v\nwant:\n%#v", got, want)
		}
	})

	t.Run("git project resolves relative to compose file directory", func(t *testing.T) {
		projDir := filepath.Join(srv.cfg.ProjectsDir, "git-app")
		composeFile := filepath.Join(projDir, "src", "deploy", "compose.yml")
		if err := os.MkdirAll(filepath.Join(projDir, "src", "deploy", "app-data"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(composeFile, []byte("services:\n  web:\n    volumes:\n      - ./app-data:/data\n"), 0o644); err != nil {
			t.Fatalf("write compose: %v", err)
		}
		p := &Project{
			ID:             3,
			Name:           "git-app",
			Kind:           "app",
			Source:         "git",
			GitComposePath: "deploy/compose.yml",
		}
		got := srv.detectProjectDirs(p)
		want := []backupDirOption{
			{Path: "src/deploy/app-data", Label: "挂载 → /data", Hint: "挂载"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("detectProjectDirs(git) =\n%#v\nwant:\n%#v", got, want)
		}
	})
}

func TestHandleBackupSourcesIncludesProjectDirs(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().Unix()
	projDir := filepath.Join(srv.cfg.ProjectsDir, "demo-app")
	if err := os.MkdirAll(filepath.Join(projDir, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	composeYAML := "services:\n  app:\n    volumes:\n      - ./data:/app/data\n"
	if _, err := srv.DB().Exec(`INSERT INTO projects(name, kind, source, compose, created_at, updated_at) VALUES('demo-app', 'app', 'inline', ?, ?, ?)`, composeYAML, now, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	w := httptest.NewRecorder()
	if err := srv.handleBackupSources(w, httptest.NewRequest(http.MethodGet, "/api/backup-sources", nil), admin); err != nil {
		t.Fatalf("handleBackupSources() error: %v", err)
	}
	var resp struct {
		Projects []struct {
			ID   int64             `json:"id"`
			Name string            `json:"name"`
			Dirs []backupDirOption `json:"dirs"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Projects) != 1 || resp.Projects[0].Name != "demo-app" {
		t.Fatalf("unexpected projects: %+v", resp.Projects)
	}
	want := []backupDirOption{{Path: "data", Label: "挂载 → /app/data", Hint: "挂载"}}
	if !reflect.DeepEqual(resp.Projects[0].Dirs, want) {
		t.Fatalf("projects[0].dirs = %#v, want %#v", resp.Projects[0].Dirs, want)
	}
}

func TestGetStorageTargetSkipsClientOpen(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().Unix()
	// Insert a storage target whose config would fail backup.Open (empty bucket/endpoint)
	// so getStorageTarget / handleListStorage prove they do not construct network clients.
	res, err := srv.DB().Exec(
		`INSERT INTO storage_targets(name,kind,config,created_at) VALUES('broken-s3','s3',?,?)`,
		srv.box.Seal(`{"secretKey":"super-secret"}`), now,
	)
	if err != nil {
		t.Fatalf("insert storage_targets: %v", err)
	}
	id, _ := res.LastInsertId()

	if _, _, err := srv.getStorage(id); err == nil {
		t.Fatalf("expected getStorage() to fail backup.Open on empty S3 endpoint")
	}
	got, err := srv.getStorageTarget(id)
	if err != nil {
		t.Fatalf("getStorageTarget() error: %v", err)
	}
	if got.Name != "broken-s3" || got.Config.SecretKey != "super-secret" {
		t.Fatalf("unexpected target: %+v", got)
	}
}
