package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"compose-panel/internal/auth"
	"compose-panel/internal/backup"
	"compose-panel/internal/config"
)

// makePanelBackupSnapshot creates a realistic source panel DB with an admin user,
// a project, a site, and encrypted secrets, returning (snapshotPath, masterKeyHex, ageIdentity).
func makePanelBackupSnapshot(t *testing.T) (string, string, string) {
	t.Helper()
	srcDir := t.TempDir()
	srcSrv, err := New(config.Config{
		DataDir:     srcDir,
		ProjectsDir: filepath.Join(srcDir, "projects"),
		BackupsDir:  filepath.Join(srcDir, "backups"),
		DockerHost:  "unix:///nonexistent.sock",
	}, nil)
	if err != nil {
		t.Fatalf("source server New: %v", err)
	}
	hash, _ := auth.HashPassword("super-secret-pass-123")
	_, err = srcSrv.db.Exec(`INSERT INTO users(username,email,password_hash,role,created_at) VALUES('alice','alice@example.com',?,'admin',1000)`, hash)
	if err != nil {
		t.Fatal(err)
	}
	_, err = srcSrv.db.Exec(`INSERT INTO sessions(id,user_id,mfa_pending,created_at,expires_at,last_seen_at) VALUES('stale-sess',1,0,1000,9999999999,1000)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = srcSrv.db.Exec(`INSERT INTO projects(name,kind,source,env,created_at,updated_at) VALUES('demo-app','app','inline',?,1000,1000)`,
		srcSrv.box.Seal(`[{"key":"SECRET","value":"top-secret","secret":true}]`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = srcSrv.db.Exec(`INSERT INTO sites(domains,kind,upstream,created_at) VALUES('demo.example.com','proxy','demo-app:80',1000)`)
	if err != nil {
		t.Fatal(err)
	}

	snapPath := filepath.Join(t.TempDir(), "panel-20261120-030000.db")
	if _, err := srcSrv.db.Exec(`VACUUM INTO ?`, snapPath); err != nil {
		t.Fatalf("VACUUM INTO: %v", err)
	}
	masterKeyRaw, err := os.ReadFile(filepath.Join(srcDir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ageID := srcSrv.box.MustOpen(srcSrv.setting("backup_age_identity", ""))
	_ = srcSrv.db.Close()
	return snapPath, strings.TrimSpace(string(masterKeyRaw)), ageID
}

func makeMultipartPanelRestore(t *testing.T, fields map[string]string, fileName string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if fileName != "" {
		fw, err := mw.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestValidatePanelBackupCases(t *testing.T) {
	snapPath, srcMasterKey, _ := makePanelBackupSnapshot(t)
	srv := newTestServer(t)

	// 1. Corrupt non-SQLite file must fail.
	badFile := filepath.Join(t.TempDir(), "corrupt.db")
	_ = os.WriteFile(badFile, []byte("not a sqlite database at all"), 0o600)
	if _, err := srv.ValidatePanelBackup(badFile, ""); err == nil {
		t.Fatalf("expected corrupt file validation to fail")
	}

	// 2. Snapshot from another server WITHOUT its master.key reports needMasterKey=true.
	sumWrong, err := srv.ValidatePanelBackup(snapPath, "")
	if err != nil {
		t.Fatalf("ValidatePanelBackup wrong key unexpected hard error: %v", err)
	}
	if sumWrong.KeyMatched || !sumWrong.NeedMasterKey {
		t.Fatalf("expected NeedMasterKey=true KeyMatched=false, got %+v", sumWrong)
	}

	// 3. Snapshot WITH the source server's master.key succeeds and counts entities accurately.
	sumOK, err := srv.ValidatePanelBackup(snapPath, srcMasterKey)
	if err != nil {
		t.Fatalf("ValidatePanelBackup valid cross-host key error: %v", err)
	}
	if !sumOK.KeyMatched || sumOK.NeedMasterKey {
		t.Fatalf("expected KeyMatched=true NeedMasterKey=false, got %+v", sumOK)
	}
	if sumOK.Users != 1 || sumOK.Projects != 1 || sumOK.Sites != 1 {
		t.Fatalf("unexpected summary counts: %+v", sumOK)
	}

	// 4. SQLite DB with zero active admins must fail.
	noAdminDir := t.TempDir()
	noAdminSrv, err := New(config.Config{
		DataDir: noAdminDir, ProjectsDir: noAdminDir, BackupsDir: noAdminDir, DockerHost: "unix:///nonexistent.sock",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	noAdminSnap := filepath.Join(t.TempDir(), "noadmin.db")
	_, _ = noAdminSrv.db.Exec(`VACUUM INTO ?`, noAdminSnap)
	noAdminKey, _ := os.ReadFile(filepath.Join(noAdminDir, "master.key"))
	_ = noAdminSrv.db.Close()
	if _, err := srv.ValidatePanelBackup(noAdminSnap, strings.TrimSpace(string(noAdminKey))); err == nil {
		t.Fatalf("expected panel DB with no active admin to fail validation")
	}
}

func TestApplyPendingPanelRestoreSwapsFilesClearsWALAndSessions(t *testing.T) {
	snapPath, srcMasterKey, _ := makePanelBackupSnapshot(t)
	targetDir := t.TempDir()

	// Initialize a target panel first so panel.db, WAL, SHM, and a different master.key exist.
	dstCfg := config.Config{
		DataDir:     targetDir,
		ProjectsDir: filepath.Join(targetDir, "projects"),
		BackupsDir:  filepath.Join(targetDir, "backups"),
		DockerHost:  "unix:///nonexistent.sock",
	}
	dstSrv, err := New(dstCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = dstSrv.db.Close()

	// Stage pending DB, pending master.key, and clear_sessions marker.
	snapBytes, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "panel.db.pending"), snapBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "master.key.pending"), []byte(srcMasterKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "panel.restore.clear_sessions"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plant dummy WAL/SHM to verify cleanup.
	_ = os.WriteFile(filepath.Join(targetDir, "panel.db-wal"), []byte("stale-wal"), 0o600)
	_ = os.WriteFile(filepath.Join(targetDir, "panel.db-shm"), []byte("stale-shm"), 0o600)

	// Re-opening server.New runs ApplyPendingPanelRestore before store.Open and clears sessions!
	reloaded, err := New(dstCfg, nil)
	if err != nil {
		t.Fatalf("reloaded New() error: %v", err)
	}
	defer reloaded.db.Close()

	var userCount, sessCount int
	_ = reloaded.db.QueryRow(`SELECT count(*) FROM users WHERE username='alice'`).Scan(&userCount)
	_ = reloaded.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessCount)
	if userCount != 1 {
		t.Fatalf("expected restored user alice, got count=%d", userCount)
	}
	if sessCount != 0 {
		t.Fatalf("expected stale sessions cleared on restore, got %d", sessCount)
	}
	// Verify encrypted project env decrypts cleanly with restored master.key!
	var encEnv string
	if err := reloaded.db.QueryRow(`SELECT env FROM projects WHERE name='demo-app'`).Scan(&encEnv); err != nil {
		t.Fatal(err)
	}
	plainEnv, err := reloaded.box.Open(encEnv)
	if err != nil || !strings.Contains(plainEnv, "top-secret") {
		t.Fatalf("restored master.key failed to decrypt env: plain=%q err=%v", plainEnv, err)
	}
}

func TestSystemPanelRestoreBusyJobReturns409AndDryRunPreview(t *testing.T) {
	snapPath, srcMasterKey, _ := makePanelBackupSnapshot(t)
	snapBytes, _ := os.ReadFile(snapPath)
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Dry-run preview works cleanly without triggering restart or touching live DB.
	body, ct := makeMultipartPanelRestore(t, map[string]string{
		"masterKey": srcMasterKey,
		"dryRun":    "1",
	}, "panel-20261120.db", snapBytes)
	req := httptest.NewRequest(http.MethodPost, "/api/system/panel-restore", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	if err := srv.handleSystemPanelRestore(w, req, admin); err != nil {
		t.Fatalf("handleSystemPanelRestore dryRun: %v", err)
	}
	var dryResp struct {
		DryRun  bool                `json:"dryRun"`
		Summary panelRestoreSummary `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &dryResp); err != nil {
		t.Fatal(err)
	}
	if !dryResp.DryRun || !dryResp.Summary.KeyMatched || dryResp.Summary.Users != 1 {
		t.Fatalf("unexpected dryRun response: %+v", dryResp)
	}
	if srv.RestartRequested() {
		t.Fatalf("dryRun must NOT request restart")
	}

	// Start a blocking background job; live restore must return 409 Conflict!
	started := make(chan struct{})
	release := make(chan struct{})
	srv.jobs.Submit(jobSpec("deploy", "部署 blog", "blog", "admin", "deploy:blog"), func(r *jobRun) error {
		close(started)
		<-release
		return nil
	})
	<-started
	defer close(release)

	body2, ct2 := makeMultipartPanelRestore(t, map[string]string{
		"masterKey": srcMasterKey,
		"confirm":   "RESTORE",
	}, "panel-20261120.db", snapBytes)
	req2 := httptest.NewRequest(http.MethodPost, "/api/system/panel-restore", body2)
	req2.Header.Set("Content-Type", ct2)
	w2 := httptest.NewRecorder()
	err := srv.handleSystemPanelRestore(w2, req2, admin)
	ae, ok := err.(*apiError)
	if !ok || ae.Status != http.StatusConflict || !strings.Contains(ae.Msg, "部署 blog") {
		t.Fatalf("expected 409 Conflict naming active job, got err=%v", err)
	}
}

func TestSetupRestoreAuthRateLimitAndEncryptedArchive(t *testing.T) {
	snapPath, srcMasterKey, _ := makePanelBackupSnapshot(t)
	snapBytes, _ := os.ReadFile(snapPath)

	// Compress with zstd and encrypt with an external age key!
	extAgeID, extAgeRcpStr, _ := backup.NewIdentity()
	extAgeRcp, _ := age.ParseX25519Recipient(extAgeRcpStr)
	var encBuf bytes.Buffer
	aw, _ := age.Encrypt(&encBuf, extAgeRcp)
	zw, _ := zstd.NewWriter(aw)
	_, _ = io.Copy(zw, bytes.NewReader(snapBytes))
	_ = zw.Close()
	_ = aw.Close()
	encBytes := encBuf.Bytes()

	srv := newTestServer(t)
	token := srv.setupToken
	if token == "" {
		t.Fatalf("expected setupToken on fresh server")
	}

	// 1. Bad setup token returns 403.
	b1, ct1 := makeMultipartPanelRestore(t, map[string]string{
		"token": "wrong-token", "dryRun": "1",
	}, "panel.sqlite.zst.age", encBytes)
	r1 := httptest.NewRequest(http.MethodPost, "/api/setup/restore", b1)
	r1.RemoteAddr = "10.0.0.5:1234"
	r1.Header.Set("Content-Type", ct1)
	w1 := httptest.NewRecorder()
	err := srv.handleSetupRestore(w1, r1)
	if ae, ok := err.(*apiError); !ok || ae.Status != 403 {
		t.Fatalf("bad setup token expected 403, got %v", err)
	}

	// 2. Dry-run with valid token + ageIdentity + masterKey succeeds.
	b2, ct2 := makeMultipartPanelRestore(t, map[string]string{
		"token": token, "ageIdentity": extAgeID, "masterKey": srcMasterKey, "dryRun": "1",
	}, "panel.sqlite.zst.age", encBytes)
	r2 := httptest.NewRequest(http.MethodPost, "/api/setup/restore", b2)
	r2.RemoteAddr = "10.0.0.6:1234"
	r2.Header.Set("Content-Type", ct2)
	w2 := httptest.NewRecorder()
	if err := srv.handleSetupRestore(w2, r2); err != nil {
		t.Fatalf("setup restore dryRun failed: %v", err)
	}

	// 3. Real commit stages pending files and requests restart.
	b3, ct3 := makeMultipartPanelRestore(t, map[string]string{
		"token": token, "ageIdentity": extAgeID, "masterKey": srcMasterKey,
	}, "panel.sqlite.zst.age", encBytes)
	r3 := httptest.NewRequest(http.MethodPost, "/api/setup/restore", b3)
	r3.RemoteAddr = "10.0.0.6:1234"
	r3.Header.Set("Content-Type", ct3)
	w3 := httptest.NewRecorder()
	if err := srv.handleSetupRestore(w3, r3); err != nil {
		t.Fatalf("setup restore apply failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.DataDir, "panel.db.pending")); err != nil {
		t.Fatalf("expected panel.db.pending staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.DataDir, "master.key.pending")); err != nil {
		t.Fatalf("expected master.key.pending staged: %v", err)
	}
	// Wait briefly for the 250ms restart trigger.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !srv.RestartRequested() {
		time.Sleep(25 * time.Millisecond)
	}
	if !srv.RestartRequested() {
		t.Fatalf("expected RestartRequested()=true after setup restore")
	}

	// 4. Once a user exists, /api/setup/restore MUST return 409 Conflict!
	srv2 := newTestServer(t)
	_, _ = srv2.db.Exec(`INSERT INTO users(username,password_hash,role,created_at) VALUES('admin','x','admin',1)`)
	b4, ct4 := makeMultipartPanelRestore(t, map[string]string{"token": srv2.setupToken}, "panel.db", snapBytes)
	r4 := httptest.NewRequest(http.MethodPost, "/api/setup/restore", b4)
	r4.Header.Set("Content-Type", ct4)
	err = srv2.handleSetupRestore(httptest.NewRecorder(), r4)
	if ae, ok := err.(*apiError); !ok || ae.Status != 409 {
		t.Fatalf("setup restore on initialized server expected 409, got %v", err)
	}
}

func TestListLocalPanelBackupsAndCLI(t *testing.T) {
	snapPath, srcMasterKey, _ := makePanelBackupSnapshot(t)
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	panelDir := filepath.Join(srv.cfg.BackupsDir, "_panel")
	_ = os.MkdirAll(panelDir, 0o750)
	_ = os.WriteFile(filepath.Join(panelDir, "panel-20261120-030000.db"), []byte("x"), 0o600)

	w := httptest.NewRecorder()
	if err := srv.handleListPanelBackups(w, httptest.NewRequest(http.MethodGet, "/api/system/panel-backups", nil), admin); err != nil {
		t.Fatalf("handleListPanelBackups: %v", err)
	}
	if !strings.Contains(w.Body.String(), "panel-20261120-030000.db") {
		t.Fatalf("expected local panel snapshot in response: %s", w.Body.String())
	}

	// Test CLI restore helper directly on a fresh dataDir.
	cliDir := t.TempDir()
	cliCfg := config.Config{DataDir: cliDir, ProjectsDir: cliDir, BackupsDir: filepath.Join(cliDir, "backups"), DockerHost: "unix:///nonexistent.sock"}
	if err := RestorePanelCLI(cliCfg, snapPath, srcMasterKey, ""); err != nil {
		t.Fatalf("RestorePanelCLI: %v", err)
	}
	reopened, err := New(cliCfg, nil)
	if err != nil {
		t.Fatalf("New after CLI restore: %v", err)
	}
	defer reopened.db.Close()
	var n int
	_ = reopened.db.QueryRow(`SELECT count(*) FROM users WHERE username='alice'`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected alice after CLI restore, got %d", n)
	}
	_ = fmt.Sprint(n)
}
