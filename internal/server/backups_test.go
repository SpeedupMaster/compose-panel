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
	"strings"
	"testing"

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
