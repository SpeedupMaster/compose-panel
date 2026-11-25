package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"compose-panel/internal/auth"
	"compose-panel/internal/compose"
	"compose-panel/internal/config"
	"compose-panel/internal/docker"
)

func waitJob(t *testing.T, srv *Server, jobID string) error {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var status, errMsg string
		if err := srv.db.QueryRow(`SELECT status, error FROM jobs WHERE id=?`, jobID).Scan(&status, &errMsg); err == nil {
			switch status {
			case "success":
				return nil
			case "failed", "canceled":
				return errors.New(errMsg)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s timed out", jobID)
	return nil
}

func genTOTPForStep(t *testing.T, secret string, step int64) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		t.Fatalf("decode totp secret: %v", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1000000
	return fmt.Sprintf("%06d", code)
}

func verifyAuditChain(t *testing.T, srv *Server) (bool, int) {
	t.Helper()
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	w := httptest.NewRecorder()
	if err := srv.handleAuditVerify(w, httptest.NewRequest(http.MethodGet, "/api/audit/verify", nil), admin); err != nil {
		t.Fatalf("handleAuditVerify: %v", err)
	}
	var res struct {
		OK       bool  `json:"ok"`
		Count    int   `json:"count"`
		BrokenAt int64 `json:"brokenAt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal audit verify: %v", err)
	}
	return res.OK, res.Count
}

func TestAuditChainTransactionalAnchoredRetentionAndCLI(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// 1. Concurrent writes preserve an unbroken hash chain.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			srv.audit(nil, admin, "test.event", fmt.Sprintf("t-%d", idx), "detail", nil)
		}(i)
	}
	wg.Wait()

	okChain, count := verifyAuditChain(t, srv)
	if !okChain || count != 20 {
		t.Fatalf("after 20 concurrent audits: ok=%v count=%d, want true, 20", okChain, count)
	}

	// 2. CLI audit enters the hash chain cleanly.
	srv.AuditCLI("user.reset_password", "admin", "CLI 重置密码并关闭两步验证")
	okChain, count = verifyAuditChain(t, srv)
	if !okChain || count != 21 {
		t.Fatalf("after AuditCLI: ok=%v count=%d, want true, 21", okChain, count)
	}
	var cliActor, cliAction string
	if err := srv.db.QueryRow(`SELECT actor, action FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&cliActor, &cliAction); err != nil {
		t.Fatalf("query cli audit: %v", err)
	}
	if cliActor != "cli" || cliAction != "user.reset_password" {
		t.Fatalf("cli audit row = (%q, %q), want (cli, user.reset_password)", cliActor, cliAction)
	}

	// 3. Prune by maxRows preserves mathematical continuity via audit_anchor_hash.
	if err := srv.PruneAuditLog(0, 5); err != nil {
		t.Fatalf("PruneAuditLog(0, 5): %v", err)
	}
	anchor := srv.setting("audit_anchor_hash", "")
	if anchor == "" {
		t.Fatalf("expected non-empty audit_anchor_hash after pruning")
	}
	okChain, count = verifyAuditChain(t, srv)
	if !okChain || count != 5 {
		t.Fatalf("after maxRows prune: ok=%v count=%d, want true, 5", okChain, count)
	}

	// New audits after pruning continue chaining properly.
	srv.audit(nil, admin, "after.prune", "target", "", nil)
	okChain, count = verifyAuditChain(t, srv)
	if !okChain || count != 6 {
		t.Fatalf("after new audit post-prune: ok=%v count=%d, want true, 6", okChain, count)
	}

	// 4. Prune by keepDays when ALL rows are older than keepDays leaves 0 rows + anchor,
	// and subsequent audits chain from the anchor!
	oldTS := time.Now().Add(-200 * 24 * time.Hour).Unix()
	if _, err := srv.db.Exec(`UPDATE audit_log SET ts=?`, oldTS); err != nil {
		t.Fatalf("age audit_log rows: %v", err)
	}
	// Recompute chain with aged timestamps so chain stays valid before pruning.
	if _, err := srv.db.Exec(`DELETE FROM audit_log`); err != nil {
		t.Fatal(err)
	}
	prev := anchor
	for i := 0; i < 4; i++ {
		ts := oldTS + int64(i)
		h := auditHash(prev, ts, "admin", "old.act", "t", "", "ok", "")
		if _, err := srv.db.Exec(`INSERT INTO audit_log(ts,actor,action,target,ip,result,detail,prev_hash,hash) VALUES(?,?,?,?,?,?,?,?,?)`,
			ts, "admin", "old.act", "t", "", "ok", "", prev, h); err != nil {
			t.Fatal(err)
		}
		prev = h
	}
	if err := srv.PruneAuditLog(180, 50000); err != nil {
		t.Fatalf("PruneAuditLog(180, 50000): %v", err)
	}
	if got := srv.setting("audit_anchor_hash", ""); got != prev {
		t.Fatalf("audit_anchor_hash = %q, want %q", got, prev)
	}
	okChain, count = verifyAuditChain(t, srv)
	if !okChain || count != 0 {
		t.Fatalf("after pruning all old rows: ok=%v count=%d, want true, 0", okChain, count)
	}
	srv.audit(nil, admin, "fresh.after.empty", "t", "", nil)
	okChain, count = verifyAuditChain(t, srv)
	if !okChain || count != 1 {
		t.Fatalf("after fresh audit on anchored empty table: ok=%v count=%d, want true, 1", okChain, count)
	}

	// 5. handleAuditExport does not issue a nested query while rows are open (works even with MaxOpenConns=1).
	srv.db.SetMaxOpenConns(1)
	wExp := httptest.NewRecorder()
	if err := srv.handleAuditExport(wExp, httptest.NewRequest(http.MethodGet, "/api/audit/export", nil), admin); err != nil {
		t.Fatalf("handleAuditExport with MaxOpenConns=1: %v", err)
	}
	if !strings.Contains(wExp.Body.String(), "fresh.after.empty") {
		t.Fatalf("expected exported CSV to include fresh.after.empty, got: %s", wExp.Body.String())
	}
	srv.db.SetMaxOpenConns(4)

	// 6. Tampering with anchor breaks verification.
	srv.setSetting("audit_anchor_hash", "tampered")
	okChain, _ = verifyAuditChain(t, srv)
	if okChain {
		t.Fatalf("expected handleAuditVerify to fail when anchor is tampered")
	}
}

func TestBearerTokenLastUsedThrottle(t *testing.T) {
	srv := newTestServer(t)
	_, tok := seedUserWithToken(t, srv, "bot", "admin", true, nil, "full")

	req1 := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.RemoteAddr = "198.51.100.10:1234"
	if _, err := srv.authenticate(req1); err != nil {
		t.Fatalf("authenticate #1: %v", err)
	}
	var usedAt1 int64
	var usedIP1 string
	if err := srv.db.QueryRow(`SELECT last_used_at, last_used_ip FROM api_tokens WHERE token_hash=?`, auth.SHA256Hex(tok)).Scan(&usedAt1, &usedIP1); err != nil {
		t.Fatal(err)
	}
	if usedAt1 == 0 || usedIP1 != "198.51.100.10" {
		t.Fatalf("after #1: last_used=(%d, %q), want non-zero & 198.51.100.10", usedAt1, usedIP1)
	}

	// Immediate second request from another IP must be throttled (within 60s).
	req2 := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.RemoteAddr = "198.51.100.20:1234"
	if _, err := srv.authenticate(req2); err != nil {
		t.Fatalf("authenticate #2: %v", err)
	}
	var usedIP2 string
	_ = srv.db.QueryRow(`SELECT last_used_ip FROM api_tokens WHERE token_hash=?`, auth.SHA256Hex(tok)).Scan(&usedIP2)
	if usedIP2 != "198.51.100.10" {
		t.Fatalf("expected throttled token update to keep old IP 198.51.100.10, got %q", usedIP2)
	}

	// Backdate last_used_at > 60s ago -> next request updates last_used_ip.
	_, _ = srv.db.Exec(`UPDATE api_tokens SET last_used_at=? WHERE token_hash=?`, time.Now().Unix()-65, auth.SHA256Hex(tok))
	if _, err := srv.authenticate(req2); err != nil {
		t.Fatalf("authenticate #3: %v", err)
	}
	var usedIP3 string
	_ = srv.db.QueryRow(`SELECT last_used_ip FROM api_tokens WHERE token_hash=?`, auth.SHA256Hex(tok)).Scan(&usedIP3)
	if usedIP3 != "198.51.100.20" {
		t.Fatalf("expected token update after 60s window to set IP 198.51.100.20, got %q", usedIP3)
	}
}

func TestTOTPReplayProtection(t *testing.T) {
	srv := newTestServer(t)
	uid, _ := seedUserWithToken(t, srv, "mfauser", "admin", true, nil, "")
	sec := auth.NewTOTPSecret()
	if _, err := srv.db.Exec(`UPDATE users SET totp_secret=?, totp_enabled=1, last_totp_step=0 WHERE id=?`, srv.box.Seal(sec), uid); err != nil {
		t.Fatal(err)
	}

	makePendingCookie := func() *http.Cookie {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		if err := srv.startSession(w, req, uid, true); err != nil {
			t.Fatalf("startSession: %v", err)
		}
		return w.Result().Cookies()[0]
	}

	curStep := time.Now().Unix() / 30
	codePrev := genTOTPForStep(t, sec, curStep-1)
	codeCur := genTOTPForStep(t, sec, curStep)

	// First use of step curStep-1 succeeds.
	req1 := httptest.NewRequest(http.MethodPost, "/api/auth/totp", strings.NewReader(fmt.Sprintf(`{"code":%q}`, codePrev)))
	req1.AddCookie(makePendingCookie())
	if err := srv.handleTOTPLogin(httptest.NewRecorder(), req1); err != nil {
		t.Fatalf("first TOTP login (step-1) failed: %v", err)
	}

	// Replaying the exact same step curStep-1 code MUST fail!
	reqReplay := httptest.NewRequest(http.MethodPost, "/api/auth/totp", strings.NewReader(fmt.Sprintf(`{"code":%q}`, codePrev)))
	reqReplay.AddCookie(makePendingCookie())
	if err := srv.handleTOTPLogin(httptest.NewRecorder(), reqReplay); err == nil {
		t.Fatalf("expected replayed TOTP code to be rejected")
	}

	// Advancing to curStep succeeds!
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/totp", strings.NewReader(fmt.Sprintf(`{"code":%q}`, codeCur)))
	req2.AddCookie(makePendingCookie())
	if err := srv.handleTOTPLogin(httptest.NewRecorder(), req2); err != nil {
		t.Fatalf("next-step TOTP login failed: %v", err)
	}

	// Replaying curStep OR older curStep-1 both fail.
	for _, c := range []string{codeCur, codePrev} {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/totp", strings.NewReader(fmt.Sprintf(`{"code":%q}`, c)))
		req.AddCookie(makePendingCookie())
		if err := srv.handleTOTPLogin(httptest.NewRecorder(), req); err == nil {
			t.Fatalf("expected replay/old TOTP code %q to be rejected", c)
		}
	}
}

func TestPasswordMax72BytesAndSetupCaddyBgCtx(t *testing.T) {
	var caddyCalled atomic.Int32
	tmp := t.TempDir()
	caddySock := filepath.Join(tmp, "caddy.sock")
	ln, err := net.Listen("unix", caddySock)
	if err != nil {
		t.Fatalf("listen unix caddy mock: %v", err)
	}
	caddySrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caddyCalled.Add(1)
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = caddySrv.Serve(ln) }()
	defer caddySrv.Close()

	srv, err := New(config.Config{
		DataDir:     tmp,
		ProjectsDir: tmp,
		BackupsDir:  tmp,
		DockerHost:  "unix:///nonexistent.sock",
		CaddyAdmin:  caddySock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.bgCancel()
		srv.bgWG.Wait()
		_ = srv.DB().Close()
	})

	// 1. handleSetup rejects >72-byte password with 400 (not 500).
	longPW := strings.Repeat("a", 73)
	reqLong := httptest.NewRequest(http.MethodPost, "/api/setup",
		strings.NewReader(fmt.Sprintf(`{"token":%q,"username":"admin","password":%q}`, srv.setupToken, longPW)))
	err = srv.handleSetup(httptest.NewRecorder(), reqLong)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("handleSetup(>72 bytes) err = %v, want 400 *apiError", err)
	}

	// 2. handleSetup with valid password triggers syncCaddy on bgCtx even if r.Context() is already canceled!
	rctx, rcancel := context.WithCancel(context.Background())
	reqOK := httptest.NewRequest(http.MethodPost, "/api/setup",
		strings.NewReader(fmt.Sprintf(`{"token":%q,"username":"admin","password":"valid-password-123"}`, srv.setupToken))).WithContext(rctx)
	wOK := httptest.NewRecorder()
	if err := srv.handleSetup(wOK, reqOK); err != nil {
		t.Fatalf("handleSetup valid: %v", err)
	}
	rcancel() // cancel request context immediately, simulating HTTP handler return
	srv.bgWG.Wait()
	if caddyCalled.Load() == 0 {
		t.Fatalf("expected handleSetup to sync Caddy using bgCtx after r.Context() canceled")
	}

	// 3. handleChangePassword rejects >72-byte password with 400.
	var uid int64
	_ = srv.db.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&uid)
	admin := &User{ID: uid, Username: "admin", Role: "admin", Level: roleAdmin}
	reqChg := httptest.NewRequest(http.MethodPost, "/api/auth/password",
		strings.NewReader(fmt.Sprintf(`{"current":"valid-password-123","new":%q}`, longPW)))
	err = srv.handleChangePassword(httptest.NewRecorder(), reqChg, admin)
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("handleChangePassword(>72 bytes) err = %v, want 400 *apiError", err)
	}
}

func TestGitURLValidationSealingMaskingAndUpdatePreservation(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Invalid URLs and branches rejected on create.
	badCases := []struct {
		name, url, branch string
	}{
		{"flag-url", "--upload-pack=sh", "main"},
		{"file-scheme", "file:///etc/passwd", "main"},
		{"local-path", "/var/repo.git", "main"},
		{"git-scheme", "git://example.com/repo.git", "main"},
		{"flag-branch", "https://github.com/org/repo.git", "--exec=id"},
		{"dotdot-branch", "https://github.com/org/repo.git", "main..evil"},
	}
	for _, tc := range badCases {
		body := fmt.Sprintf(`{"name":"p-%s","source":"git","gitUrl":%q,"gitBranch":%q}`, tc.name, tc.url, tc.branch)
		err := srv.handleCreateProject(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(body)), admin)
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != 400 {
			t.Errorf("case %s: got err=%v, want 400", tc.name, err)
		}
	}

	// Valid HTTPS URL with embedded credentials -> sealed at rest, masked on GET, preserved on PUT.
	origURL := "https://alice:s3cr3tToken@github.com/org/repo.git"
	wCreate := httptest.NewRecorder()
	createBody := fmt.Sprintf(`{"name":"mygit","source":"git","gitUrl":%q,"gitBranch":"main"}`, origURL)
	if err := srv.handleCreateProject(wCreate, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(createBody)), admin); err != nil {
		t.Fatalf("handleCreateProject valid git: %v", err)
	}
	var created struct{ ID int64 `json:"id"` }
	_ = json.Unmarshal(wCreate.Body.Bytes(), &created)

	// Check sealed at rest in SQLite.
	var rawDBURL string
	if err := srv.db.QueryRow(`SELECT git_url FROM projects WHERE id=?`, created.ID).Scan(&rawDBURL); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rawDBURL, "v1:") || strings.Contains(rawDBURL, "s3cr3tToken") {
		t.Fatalf("git_url in DB should be sealed (v1:...), got %q", rawDBURL)
	}

	// Check GET masks password.
	reqGet := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/projects/%d", created.ID), nil)
	reqGet.SetPathValue("id", fmt.Sprint(created.ID))
	wGet := httptest.NewRecorder()
	if err := srv.handleGetProject(wGet, reqGet, admin); err != nil {
		t.Fatalf("handleGetProject: %v", err)
	}
	if strings.Contains(wGet.Body.String(), "s3cr3tToken") {
		t.Fatalf("GET /api/projects leaked plaintext git password: %s", wGet.Body.String())
	}
	var getResp struct {
		Project Project `json:"project"`
	}
	_ = json.Unmarshal(wGet.Body.Bytes(), &getResp)
	wantMasked := "https://alice:••••••@github.com/org/repo.git"
	if getResp.Project.GitURL != wantMasked {
		t.Fatalf("GET project.gitUrl = %q, want %q", getResp.Project.GitURL, wantMasked)
	}

	// PUT with masked URL on same host preserves original password.
	updBody := `{"gitUrl":"https://alice:••••••@github.com/org/repo2.git","gitBranch":"release/v2"}`
	reqUpd := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/projects/%d", created.ID), strings.NewReader(updBody))
	reqUpd.SetPathValue("id", fmt.Sprint(created.ID))
	if err := srv.handleUpdateProject(httptest.NewRecorder(), reqUpd, admin); err != nil {
		t.Fatalf("handleUpdateProject preserving masked password: %v", err)
	}
	pAfter, err := srv.getProject(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pAfter.GitURL != "https://alice:s3cr3tToken@github.com/org/repo2.git" {
		t.Fatalf("after masked update, decrypted GitURL = %q, want preserved password", pAfter.GitURL)
	}

	// PUT with masked URL changing host MUST reject (prevent credential exfiltration).
	evilUpd := `{"gitUrl":"https://alice:••••••@evil.example/repo2.git","gitBranch":"main"}`
	reqEvil := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/projects/%d", created.ID), strings.NewReader(evilUpd))
	reqEvil.SetPathValue("id", fmt.Sprint(created.ID))
	if err := srv.handleUpdateProject(httptest.NewRecorder(), reqEvil, admin); err == nil {
		t.Fatalf("expected host-change with masked password to be rejected")
	}

	// Legacy unencrypted git_url in DB still opens transparently.
	if _, err := srv.db.Exec(`UPDATE projects SET git_url='https://legacy.example/org/app.git' WHERE id=?`, created.ID); err != nil {
		t.Fatal(err)
	}
	pLegacy, err := srv.getProject(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pLegacy.GitURL != "https://legacy.example/org/app.git" {
		t.Fatalf("legacy unencrypted git_url = %q, want plaintext passthrough", pLegacy.GitURL)
	}
}

func TestGitEnvSanitizationAndSSHQuoting(t *testing.T) {
	srv := newTestServer(t)
	clean, header := sanitizeGitURL("https://bob:mySecret@git.example.com/team/app.git")
	if clean != "https://git.example.com/team/app.git" {
		t.Fatalf("cleanURL = %q, want stripped userinfo", clean)
	}
	wantHdr := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("bob:mySecret"))
	if header != wantHdr {
		t.Fatalf("extraHeader = %q, want %q", header, wantHdr)
	}

	p := &Project{Name: "sshproj", gitKeyEnc: srv.box.Seal("FAKE-KEY")}
	env, cleanup, err := srv.gitEnv(p, header)
	if err != nil {
		t.Fatalf("gitEnv: %v", err)
	}
	defer cleanup()

	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + wantHdr,
		`GIT_SSH_COMMAND=ssh -i "`,
		`-o UserKnownHostsFile="`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("gitEnv missing %q", want)
		}
	}
}

func TestGitSyncAuthNoLeakDeepRollbackAndActionNoReset(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	execPathOut, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path failed")
	}
	httpBackend := filepath.Join(strings.TrimSpace(string(execPathOut)), "git-http-backend")
	if _, err := os.Stat(httpBackend); err != nil {
		if _, err2 := os.Stat(httpBackend + ".exe"); err2 == nil {
			httpBackend += ".exe"
		} else {
			t.Skip("git-http-backend not available")
		}
	}

	// Build a real bare repo with 55 commits so commit #1 is beyond shallow --depth 50!
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	work := filepath.Join(root, "work")
	runGit := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command(gitBin, args...)
		c.Dir = dir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit(root, "init", "--bare", bare)
	runGit(bare, "config", "http.receivepack", "true")
	runGit(bare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	runGit(bare, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(root, "init", work)
	runGit(work, "checkout", "-b", "main")
	runGit(work, "remote", "add", "origin", bare)

	var firstSHA, lastSHA string
	for i := 1; i <= 55; i++ {
		if err := os.WriteFile(filepath.Join(work, "version.txt"), []byte(fmt.Sprintf("v%d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(work, "add", "version.txt")
		runGit(work, "commit", "-m", fmt.Sprintf("commit %d", i))
		sha := runGit(work, "rev-parse", "HEAD")
		if i == 1 {
			firstSHA = sha
		}
		lastSHA = sha
	}
	runGit(work, "push", "origin", "main")

	// Serve bare repo over HTTP requiring Basic Auth user:supersecret.
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:supersecret"))
	var authSeen atomic.Int32
	gitSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		authSeen.Add(1)
		h := &cgi.Handler{
			Path: httpBackend,
			Env: []string{
				"GIT_PROJECT_ROOT=" + root,
				"GIT_HTTP_EXPORT_ALL=1",
			},
		}
		h.ServeHTTP(w, r)
	}))
	defer gitSrv.Close()

	srv := newTestServer(t)
	authedURL := strings.Replace(gitSrv.URL, "http://", "http://user:supersecret@", 1) + "/repo.git"
	p := &Project{Name: "gitapp", Source: "git", GitURL: authedURL, GitBranch: "main", GitComposePath: "compose.yml"}

	// 1. Initial clone via gitSync: succeeds with Basic Auth header, never leaks password to .git/config or job log!
	jobID := srv.jobs.Submit(jobSpec("test", "git sync", p.Name, "test", ""), func(run *jobRun) error {
		return srv.gitSync(run, p, "")
	})
	if err := waitJob(t, srv, jobID); err != nil {
		t.Fatalf("gitSync initial clone failed: %v", err)
	}
	if authSeen.Load() == 0 {
		t.Fatalf("expected HTTP Basic auth header to be sent to git server")
	}
	srcDir := filepath.Join(srv.projectDir(p), "src")
	gitConfigBytes, err := os.ReadFile(filepath.Join(srcDir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gitConfigBytes), "supersecret") {
		t.Fatalf(".git/config leaked plaintext credential:\n%s", gitConfigBytes)
	}
	logPath := filepath.Join(srv.cfg.DataDir, "jobs", jobID+".log")
	if logBytes, err := os.ReadFile(logPath); err == nil && strings.Contains(string(logBytes), "supersecret") {
		t.Fatalf("job log leaked plaintext credential:\n%s", logBytes)
	}
	head, _ := gitHead(context.Background(), srcDir)
	if head != lastSHA {
		t.Fatalf("HEAD after clone = %s, want %s", head, lastSHA)
	}

	// 2. Deep rollback checkout to commit #1 (outside --depth 50) fetches commit #1 and checks it out!
	rbJob := srv.jobs.Submit(jobSpec("test", "git rollback", p.Name, "test", ""), func(run *jobRun) error {
		return srv.gitSync(run, p, firstSHA)
	})
	if err := waitJob(t, srv, rbJob); err != nil {
		t.Fatalf("gitSync deep rollback to commit #1 failed: %v", err)
	}
	head, _ = gitHead(context.Background(), srcDir)
	if head != firstSHA {
		t.Fatalf("HEAD after deep rollback = %s, want %s", head, firstSHA)
	}

	// 3. Lifecycle action (prepareWorkdir with syncGit=false) MUST NOT reset HEAD back to origin/main!
	// Write a minimal compose file inside srcDir just so we verify git isn't touched before LoadConfig.
	// Note: prepareWorkdir runs LoadConfig (which may invoke docker compose); we only need to verify
	// gitSync is skipped when syncGit=false, so verify HEAD stays at firstSHA regardless of LoadConfig result.
	actJob := srv.jobs.Submit(jobSpec("test", "action prepare", p.Name, "test", ""), func(run *jobRun) error {
		_ = srv.prepareWorkdir(run, p, nil, false)
		return nil
	})
	_ = waitJob(t, srv, actJob)
	headAfterAction, _ := gitHead(context.Background(), srcDir)
	if headAfterAction != firstSHA {
		t.Fatalf("prepareWorkdir(syncGit=false) mutated git HEAD from %s to %s!", firstSHA, headAfterAction)
	}
}

func TestRollbackSnapshotsInlineFilesAndGitEnv(t *testing.T) {
	srv := newTestServer(t)

	// 1. Inline project: snapshot files_json, modify DB + disk, verify deploymentState & restore.
	pid := seedProject(t, srv, "inlineapp", "app")
	origFiles := []projectFile{
		{Path: "conf/nginx.conf", Content: "server { listen 80; }"},
		{Path: "app.ini", Content: "v=1"},
	}
	if err := srv.replaceProjectFiles(pid, origFiles); err != nil {
		t.Fatal(err)
	}
	origEnv := []EnvVar{{Key: "MODE", Value: "v1"}}
	envJSON, _ := json.Marshal(origEnv)
	filesJSON, _ := json.Marshal(origFiles)
	res, err := srv.db.Exec(`INSERT INTO deployments(project_id,number,status,trigger,actor,compose,env,files_json,created_at) VALUES(?,1,'success','manual','admin','services: {web: {image: nginx}}',?,?,1000)`,
		pid, srv.box.Seal(string(envJSON)), string(filesJSON))
	if err != nil {
		t.Fatal(err)
	}
	dep1, _ := res.LastInsertId()

	// Verify deploymentState reads files_json accurately.
	st, err := srv.deploymentState(dep1)
	if err != nil {
		t.Fatalf("deploymentState: %v", err)
	}
	if !st.hasFiles || len(st.files) != 2 || st.files[0].Path != "conf/nginx.conf" {
		t.Fatalf("unexpected deploymentState files: hasFiles=%v files=%#v", st.hasFiles, st.files)
	}

	// Simulate newer state with modified + extra file on DB and disk.
	newFiles := []projectFile{
		{Path: "conf/nginx.conf", Content: "server { listen 8080; }"},
		{Path: "extra.conf", Content: "added-in-v2"},
	}
	if err := srv.replaceProjectFiles(pid, newFiles); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(srv.cfg.ProjectsDir, "inlineapp")
	_ = os.MkdirAll(filepath.Join(dir, "conf"), 0o750)
	_ = os.WriteFile(filepath.Join(dir, "extra.conf"), []byte("added-in-v2"), 0o640)

	// Restore via replaceProjectFiles + removeStaleInlineFiles (same path as runDeploy rollback).
	oldFiles := srv.projectFiles(pid)
	if err := srv.replaceProjectFiles(pid, st.files); err != nil {
		t.Fatal(err)
	}
	removeStaleInlineFiles(dir, oldFiles, st.files)

	restored := srv.projectFiles(pid)
	if len(restored) != 2 || restored[0].Path != "app.ini" || restored[1].Content != "server { listen 80; }" {
		t.Fatalf("restored DB project_files = %#v", restored)
	}
	if _, err := os.Stat(filepath.Join(dir, "extra.conf")); !os.IsNotExist(err) {
		t.Fatalf("expected stale file extra.conf to be removed on rollback")
	}

	// 2. Git project manual rollback updates projects.env in DB even when compose/git fails later!
	gitRes, err := srv.db.Exec(`INSERT INTO projects(name,kind,source,git_url,git_branch,env,created_at,updated_at) VALUES('gitenv','app','git',?,'main',?,1000,1000)`,
		srv.box.Seal("https://example.com/repo.git"), srv.box.Seal(`[{"key":"VER","value":"v2"}]`))
	if err != nil {
		t.Fatal(err)
	}
	gitPID, _ := gitRes.LastInsertId()
	v1EnvJSON, _ := json.Marshal([]EnvVar{{Key: "VER", Value: "v1"}})
	depRes, err := srv.db.Exec(`INSERT INTO deployments(project_id,number,status,trigger,actor,compose,env,commit_sha,created_at) VALUES(?,1,'success','manual','admin','services: {}',?,'deadbeef',1000)`,
		gitPID, srv.box.Seal(string(v1EnvJSON)))
	if err != nil {
		t.Fatal(err)
	}
	gitDep1, _ := depRes.LastInsertId()

	// Trigger rollback deployment (it will fail at gitSync against unreachable example.com,
	// AFTER persisting the rolled-back projects.env!).
	rbJobID, _ := srv.startDeploy(&Project{ID: gitPID, Name: "gitenv"}, "rollback", "admin", gitDep1)
	_ = waitJob(t, srv, rbJobID)

	pGit, err := srv.getProject(gitPID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pGit.Env) != 1 || pGit.Env[0].Key != "VER" || pGit.Env[0].Value != "v1" {
		t.Fatalf("expected git project rollback to persist projects.env=v1, got %#v", pGit.Env)
	}
}

func TestRegistryConfigDockerHubKeys(t *testing.T) {
	srv := newTestServer(t)
	if _, err := srv.db.Exec(`INSERT INTO registries(host,username,password) VALUES('docker.io','myuser',?),('ghcr.io','ghuser',?)`,
		srv.box.Seal("mypw"), srv.box.Seal("ghpw")); err != nil {
		t.Fatal(err)
	}
	var cp compose.Project
	cleanup := srv.registryConfig(&cp)
	defer cleanup()
	if cp.DockerCfg == "" {
		t.Fatalf("expected DockerCfg dir to be created")
	}
	raw, err := os.ReadFile(filepath.Join(cp.DockerCfg, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	wantDockerAuth := base64.StdEncoding.EncodeToString([]byte("myuser:mypw"))
	if cfg.Auths["https://index.docker.io/v1/"].Auth != wantDockerAuth {
		t.Errorf("missing/wrong https://index.docker.io/v1/ auth: %#v", cfg.Auths)
	}
	if cfg.Auths["docker.io"].Auth != wantDockerAuth {
		t.Errorf("missing/wrong docker.io auth: %#v", cfg.Auths)
	}
	if cfg.Auths["ghcr.io"].Auth != base64.StdEncoding.EncodeToString([]byte("ghuser:ghpw")) {
		t.Errorf("missing/wrong ghcr.io auth: %#v", cfg.Auths)
	}
}

func TestAutoUpdateMirrorLagNoInfiniteLoop(t *testing.T) {
	var remoteDigest atomic.Value
	remoteDigest.Store("sha256:newdigest111")

	// Minimal mock Docker daemon returning one running container with old digest
	// and distribution endpoint returning remoteDigest.
	dkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"cid1","Names":["/lagapp-web-1"],"Image":"nginx:latest","ImageID":"sha256:img1","State":"running","Labels":{"com.docker.compose.project":"lagapp","com.docker.compose.service":"web"}}]`))
		case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			_, _ = w.Write([]byte(`{"Id":"sha256:img1","RepoDigests":["nginx@sha256:olddigest000"]}`))
		case strings.Contains(r.URL.Path, "/distribution/"):
			d := remoteDigest.Load().(string)
			_, _ = fmt.Fprintf(w, `{"Descriptor":{"digest":%q}}`, d)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer dkSrv.Close()

	srv := newTestServer(t)
	srv.dk = docker.New(strings.Replace(dkSrv.URL, "http://", "tcp://", 1))

	opts, _ := json.Marshal(ProjectOptions{AutoUpdate: true})
	res, err := srv.db.Exec(`INSERT INTO projects(name,kind,source,compose,options,created_at,updated_at) VALUES('lagapp','app','inline','services: {}',?,1000,1000)`, string(opts))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := res.LastInsertId()

	countDeploys := func() int {
		var n int
		_ = srv.db.QueryRow(`SELECT count(*) FROM deployments WHERE project_id=? AND trigger='image_update'`, pid).Scan(&n)
		return n
	}

	// Run #1: new digest sha256:newdigest111 -> triggers 1 auto-update deploy.
	j1 := srv.runImageChecks("scheduler")
	if err := waitJob(t, srv, j1); err != nil {
		t.Fatalf("runImageChecks #1: %v", err)
	}
	if n := countDeploys(); n != 1 {
		t.Fatalf("after check #1: image_update deploys = %d, want 1", n)
	}

	// Run #2: mirror still lagging (local=olddigest000, remote=newdigest111) -> MUST NOT trigger another deploy!
	j2 := srv.runImageChecks("scheduler")
	if err := waitJob(t, srv, j2); err != nil {
		t.Fatalf("runImageChecks #2: %v", err)
	}
	if n := countDeploys(); n != 1 {
		t.Fatalf("after check #2 with same remote digest: image_update deploys = %d, want still 1", n)
	}

	// Run #3: upstream publishes newer digest sha256:newdigest222 -> triggers 1 new deploy!
	remoteDigest.Store("sha256:newdigest222")
	j3 := srv.runImageChecks("scheduler")
	if err := waitJob(t, srv, j3); err != nil {
		t.Fatalf("runImageChecks #3: %v", err)
	}
	if n := countDeploys(); n != 2 {
		t.Fatalf("after check #3 with new remote digest: image_update deploys = %d, want 2", n)
	}
}

func TestProjectHasEnabledSitesHelper(t *testing.T) {
	srv := newTestServer(t)
	pid := seedProject(t, srv, "siteapp", "app")
	if srv.projectHasEnabledSites(pid) {
		t.Fatalf("expected false with no sites")
	}
	if _, err := srv.db.Exec(`INSERT INTO sites(domains,kind,project_id,service,port,enabled,created_at) VALUES('a.example.com','proxy',?,'web',80,0,1000)`, pid); err != nil {
		t.Fatal(err)
	}
	if srv.projectHasEnabledSites(pid) {
		t.Fatalf("expected false with only disabled site")
	}
	if _, err := srv.db.Exec(`UPDATE sites SET enabled=1 WHERE project_id=?`, pid); err != nil {
		t.Fatal(err)
	}
	if !srv.projectHasEnabledSites(pid) {
		t.Fatalf("expected true with enabled site")
	}
}
