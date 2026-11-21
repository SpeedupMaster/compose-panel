package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"compose-panel/internal/auth"
	"compose-panel/internal/backup"
	"compose-panel/internal/config"
	"compose-panel/internal/secret"
)

const (
	panelPendingDBName      = "panel.db.pending"
	panelPendingKeyName     = "master.key.pending"
	panelClearSessionsName  = "panel.restore.clear_sessions"
	maxPanelRestoreBodySize = 512 << 20 // 512 MiB
)

var sqliteHeaderMagic = []byte("SQLite format 3\x00")

type panelRestoreSummary struct {
	Users         int  `json:"users"`
	Projects      int  `json:"projects"`
	Databases     int  `json:"databases"`
	Sites         int  `json:"sites"`
	Storages      int  `json:"storages"`
	Policies      int  `json:"policies"`
	Backups       int  `json:"backups"`
	KeyMatched    bool `json:"keyMatched"`
	NeedMasterKey bool `json:"needMasterKey"`
}

func parseMasterKeyHex(raw string) ([]byte, string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, "", nil
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, "", bad("master.key 格式无效（应为 64 位十六进制字符串）")
	}
	return b, strings.ToLower(s), nil
}

// ValidatePanelBackup verifies SQLite integrity, required schema, at least one active admin,
// and master.key compatibility for encrypted fields inside dbPath.
func (s *Server) ValidatePanelBackup(dbPath, candidateMasterKey string) (panelRestoreSummary, error) {
	return validatePanelDBWithBox(dbPath, s.box, candidateMasterKey)
}

func validatePanelDBWithBox(dbPath string, fallbackBox *secret.Box, candidateMasterKey string) (panelRestoreSummary, error) {
	var sum panelRestoreSummary
	f, err := os.Open(dbPath)
	if err != nil {
		return sum, fmt.Errorf("无法打开备份文件：%w", err)
	}
	hdr := make([]byte, len(sqliteHeaderMagic))
	n, _ := io.ReadFull(f, hdr)
	f.Close()
	if n != len(sqliteHeaderMagic) || !bytes.Equal(hdr, sqliteHeaderMagic) {
		return sum, bad("不是有效的 SQLite 面板数据库文件（如为加密备份请先提供正确的 age 私钥）")
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return sum, fmt.Errorf("打开 SQLite 失败：%w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, _ = db.Exec(`PRAGMA query_only = ON`)

	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return sum, bad("SQLite 完整性检查失败：%v", err)
	}
	if integrity != "ok" {
		return sum, bad("SQLite 完整性检查未通过：%s", integrity)
	}

	requiredTables := []string{"users", "settings", "projects", "storage_targets", "sites", "backups"}
	existing := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return sum, fmt.Errorf("读取数据表失败：%w", err)
	}
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			existing[name] = true
		}
	}
	rows.Close()
	for _, tbl := range requiredTables {
		if !existing[tbl] {
			return sum, bad("备份文件缺少面板核心数据表 %s，可能不是 Compose Panel 备份", tbl)
		}
	}

	var activeAdmins int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE role='admin' AND disabled=0`).Scan(&activeAdmins); err != nil {
		return sum, fmt.Errorf("校验管理员账号失败：%w", err)
	}
	if activeAdmins < 1 {
		return sum, bad("备份中没有可用的管理员账号，已拒绝恢复以防锁死面板")
	}

	_ = db.QueryRow(`SELECT count(*) FROM users`).Scan(&sum.Users)
	_ = db.QueryRow(`SELECT count(*) FROM projects WHERE kind='app'`).Scan(&sum.Projects)
	if existing["db_instances"] {
		_ = db.QueryRow(`SELECT count(*) FROM db_instances`).Scan(&sum.Databases)
	}
	_ = db.QueryRow(`SELECT count(*) FROM sites`).Scan(&sum.Sites)
	_ = db.QueryRow(`SELECT count(*) FROM storage_targets`).Scan(&sum.Storages)
	if existing["backup_policies"] {
		_ = db.QueryRow(`SELECT count(*) FROM backup_policies`).Scan(&sum.Policies)
	}
	_ = db.QueryRow(`SELECT count(*) FROM backups`).Scan(&sum.Backups)

	// Collect sample encrypted ("v1:...") cells to test master.key compatibility.
	var samples []string
	sampleQueries := []string{
		`SELECT value FROM settings WHERE key='backup_age_identity' AND value LIKE 'v1:%' LIMIT 1`,
		`SELECT config FROM storage_targets WHERE config LIKE 'v1:%' LIMIT 3`,
		`SELECT root_password FROM db_instances WHERE root_password LIKE 'v1:%' LIMIT 3`,
		`SELECT env FROM projects WHERE env LIKE 'v1:%' LIMIT 3`,
		`SELECT config FROM notify_channels WHERE config LIKE 'v1:%' LIMIT 3`,
		`SELECT totp_secret FROM users WHERE totp_secret LIKE 'v1:%' LIMIT 2`,
		`SELECT key_pem FROM certificates WHERE key_pem LIKE 'v1:%' LIMIT 2`,
	}
	for _, q := range sampleQueries {
		r, err := db.Query(q)
		if err != nil {
			continue
		}
		for r.Next() {
			var v string
			if r.Scan(&v) == nil && strings.HasPrefix(v, "v1:") {
				samples = append(samples, v)
			}
		}
		r.Close()
	}

	var testBox *secret.Box
	provided := strings.TrimSpace(candidateMasterKey) != ""
	if provided {
		keyBytes, _, err := parseMasterKeyHex(candidateMasterKey)
		if err != nil {
			return sum, err
		}
		testBox, err = secret.New(keyBytes)
		if err != nil {
			return sum, err
		}
	} else {
		testBox = fallbackBox
	}

	if len(samples) == 0 {
		sum.KeyMatched = true
		return sum, nil
	}
	if testBox == nil {
		sum.KeyMatched = false
		sum.NeedMasterKey = true
		return sum, nil
	}
	for _, cipherText := range samples {
		if _, err := testBox.Open(cipherText); err != nil {
			if provided {
				return sum, bad("提供的 master.key 无法解密该备份中的加密数据，请检查密钥是否正确")
			}
			sum.KeyMatched = false
			sum.NeedMasterKey = true
			return sum, nil
		}
	}
	sum.KeyMatched = true
	return sum, nil
}

// ApplyPendingPanelRestore atomically swaps any staged panel.db.pending / master.key.pending
// files into place and cleans stale SQLite WAL/SHM files. Returns whether sessions should be cleared.
func ApplyPendingPanelRestore(dataDir string) (bool, error) {
	pendingDB := filepath.Join(dataDir, panelPendingDBName)
	pendingKey := filepath.Join(dataDir, panelPendingKeyName)
	clearMarker := filepath.Join(dataDir, panelClearSessionsName)

	hasClear := false
	if _, err := os.Stat(clearMarker); err == nil {
		hasClear = true
	}

	if _, err := os.Stat(pendingDB); err != nil {
		if os.IsNotExist(err) {
			_ = os.Remove(pendingKey)
			return hasClear, nil
		}
		return false, err
	}

	liveDB := filepath.Join(dataDir, "panel.db")
	// Swap master.key FIRST while panel.db.pending still exists on disk as the commit marker:
	// if a crash occurs mid-swap, panel.db.pending remains present so reboot finishes the swap.
	if _, err := os.Stat(pendingKey); err == nil {
		liveKey := filepath.Join(dataDir, "master.key")
		if err := os.Rename(pendingKey, liveKey); err != nil {
			return false, fmt.Errorf("替换 master.key 失败：%w", err)
		}
		_ = os.Chmod(liveKey, 0o600)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(liveDB + suffix); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("清理旧 WAL 文件失败：%w", err)
		}
	}
	if err := os.Rename(pendingDB, liveDB); err != nil {
		return false, fmt.Errorf("替换 panel.db 失败：%w", err)
	}
	_ = os.Chmod(liveDB, 0o600)
	return hasClear, nil
}

func copyFileSynced(srcPath, dstPath string, mode os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := dstPath + ".writing"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dstPath)
}

func (s *Server) extractPanelStreamToTemp(rc io.ReadCloser, encrypted bool, identities []string) (string, error) {
	plain, err := backup.OpenStream(rc, encrypted, identities)
	if err != nil {
		return "", err
	}
	defer plain.Close()

	dir := filepath.Join(s.cfg.DataDir, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "panel-restore-*.db")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, io.LimitReader(plain, maxPanelRestoreBodySize+1)); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("解压/解密备份流失败：%w", err)
	}
	info, _ := f.Stat()
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if info != nil && info.Size() > maxPanelRestoreBodySize {
		os.Remove(f.Name())
		return "", bad("面板备份文件过大（超过 512 MiB）")
	}
	return f.Name(), nil
}

func (s *Server) stageAndTriggerPanelRestore(r *http.Request, u *User, tmpDBPath, masterKeyRaw, sourceDesc string, isSetup bool) (panelRestoreSummary, error) {
	_, masterKeyHex, err := parseMasterKeyHex(masterKeyRaw)
	if err != nil {
		return panelRestoreSummary{}, err
	}
	sum, err := s.ValidatePanelBackup(tmpDBPath, masterKeyHex)
	if err != nil {
		return sum, err
	}
	if !sum.KeyMatched {
		return sum, bad("备份中的加密数据无法用当前 master.key 解密，请提供备份来源服务器的 master.key")
	}

	active, okFreeze := s.jobs.TryFreezeForRestore()
	if !okFreeze {
		return sum, errStatus(http.StatusConflict, "当前有正在执行的后台任务（%s），请等待完成或取消后再恢复面板", strings.Join(active, "、"))
	}
	unfreeze := true
	defer func() {
		if unfreeze {
			s.jobs.Unfreeze()
		}
	}()

	stamp := time.Now().In(s.location()).Format("20060102-150405")
	if !isSetup {
		preDir := filepath.Join(s.cfg.BackupsDir, "_panel")
		if err := os.MkdirAll(preDir, 0o750); err == nil {
			preDB := filepath.Join(preDir, "panel-pre-restore-"+stamp+".db")
			if _, err := s.db.ExecContext(r.Context(), `VACUUM INTO ?`, preDB); err != nil {
				return sum, fmt.Errorf("恢复前自动备份当前 panel.db 失败，已中止：%w", err)
			}
			_ = os.Chmod(preDB, 0o600)
		}
		if masterKeyHex != "" {
			curKeyPath := filepath.Join(s.cfg.DataDir, "master.key")
			if _, err := os.Stat(curKeyPath); err == nil {
				_ = copyFileSynced(curKeyPath, filepath.Join(s.cfg.DataDir, "master.key.pre-restore-"+stamp), 0o600)
			}
		}
	}

	// Stage master.key.pending and clear_sessions BEFORE panel.db.pending so panel.db.pending
	// acts as the atomic commit marker for ApplyPendingPanelRestore.
	pendingKey := filepath.Join(s.cfg.DataDir, panelPendingKeyName)
	if masterKeyHex != "" {
		if err := os.WriteFile(pendingKey, []byte(masterKeyHex+"\n"), 0o600); err != nil {
			return sum, fmt.Errorf("暂存 master.key 失败：%w", err)
		}
	} else {
		_ = os.Remove(pendingKey)
	}
	_ = os.WriteFile(filepath.Join(s.cfg.DataDir, panelClearSessionsName), []byte("1\n"), 0o600)
	pendingDB := filepath.Join(s.cfg.DataDir, panelPendingDBName)
	if err := copyFileSynced(tmpDBPath, pendingDB, 0o600); err != nil {
		_ = os.Remove(pendingKey)
		return sum, fmt.Errorf("暂存 panel.db 失败：%w", err)
	}

	if !isSetup {
		detail := fmt.Sprintf("来源 %s（用户 %d / 项目 %d / 站点 %d）", sourceDesc, sum.Users, sum.Projects, sum.Sites)
		if masterKeyHex != "" {
			detail += " · 同步替换 master.key"
		}
		s.audit(r, u, "panel.restore", "panel.db", detail, nil)
	}

	unfreeze = false
	go func() {
		time.Sleep(250 * time.Millisecond)
		s.requestRestart()
	}()
	return sum, nil
}

// handlePanelRestoreFromBackup handles POST /api/backups/{id}/restore for kind="panel".
func (s *Server) handlePanelRestoreFromBackup(w http.ResponseWriter, r *http.Request, u *User, b *backupRow, identities []string, masterKey string, dryRun bool, confirm string) error {
	_, st, err := s.getStorage(b.StorageID)
	if err != nil {
		return err
	}
	rc, err := st.Get(r.Context(), b.ObjectKey)
	if err != nil {
		return fmt.Errorf("下载备份失败：%w", err)
	}
	tmpDB, err := s.extractPanelStreamToTemp(rc, b.Encrypted, identities)
	if err != nil {
		return err
	}
	defer os.Remove(tmpDB)

	if dryRun {
		sum, err := s.ValidatePanelBackup(tmpDB, masterKey)
		if err != nil {
			return err
		}
		return ok(w, map[string]any{"dryRun": true, "summary": sum})
	}
	if confirm != "RESTORE" && confirm != "恢复面板" {
		return bad("请输入 RESTORE 确认覆盖恢复面板数据")
	}
	sum, err := s.stageAndTriggerPanelRestore(r, u, tmpDB, masterKey, b.ObjectKey, false)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"restarting": true, "bootId": s.bootID, "summary": sum})
}

// handleListPanelBackups lists local _panel/panel-*.db snapshots.
func (s *Server) handleListPanelBackups(w http.ResponseWriter, r *http.Request, u *User) error {
	dir := filepath.Join(s.cfg.BackupsDir, "_panel")
	entries, _ := filepath.Glob(filepath.Join(dir, "panel-*.db"))
	type snap struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		ModTime int64  `json:"modTime"`
	}
	out := make([]snap, 0, len(entries))
	for _, p := range entries {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		out = append(out, snap{Name: filepath.Base(p), Size: st.Size(), ModTime: st.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModTime != out[j].ModTime {
			return out[i].ModTime > out[j].ModTime
		}
		return out[i].Name > out[j].Name
	})
	return ok(w, out)
}

func (s *Server) readPanelRestoreInput(r *http.Request, allowSnapshot, checkSetupToken bool) (tmpDB, sourceDesc, masterKey, confirm string, dryRun bool, err error) {
	ct := r.Header.Get("Content-Type")
	var ageIdentity string
	if strings.HasPrefix(ct, "multipart/form-data") {
		r.Body = http.MaxBytesReader(nil, r.Body, maxPanelRestoreBodySize)
		if err = r.ParseMultipartForm(32 << 20); err != nil {
			err = bad("解析上传表单失败：%v", err)
			return
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		if checkSetupToken {
			token := strings.TrimSpace(r.FormValue("token"))
			if s.setupToken == "" || !auth.ConstantEqual(token, s.setupToken) {
				s.recordFailure("setup:" + clientIP(r))
				err = errStatus(http.StatusForbidden, "初始化令牌不正确（见面板容器日志）")
				return
			}
		}
		masterKey = strings.TrimSpace(r.FormValue("masterKey"))
		ageIdentity = strings.TrimSpace(r.FormValue("ageIdentity"))
		confirm = strings.TrimSpace(r.FormValue("confirm"))
		dryRun = r.FormValue("dryRun") == "1" || r.FormValue("dryRun") == "true"

		identities, ierr := s.allAgeIdentities(ageIdentity)
		if ierr != nil {
			err = ierr
			return
		}
		file, hdr, ferr := r.FormFile("file")
		if ferr == nil {
			sourceDesc = hdr.Filename
			encrypted := strings.HasSuffix(strings.ToLower(hdr.Filename), ".age")
			tmpDB, err = s.extractPanelStreamToTemp(file, encrypted, identities)
			return
		}
		if allowSnapshot {
			snapName := strings.TrimSpace(r.FormValue("snapshot"))
			if snapName != "" {
				tmpDB, sourceDesc, err = s.openLocalPanelSnapshot(snapName, identities)
				return
			}
		}
		err = bad("请选择要恢复的面板备份文件")
		return
	}

	// JSON mode (snapshot selection only).
	if !allowSnapshot {
		err = bad("请使用表单上传面板备份文件")
		return
	}
	var req struct {
		Snapshot    string `json:"snapshot"`
		MasterKey   string `json:"masterKey"`
		AgeIdentity string `json:"ageIdentity"`
		Confirm     string `json:"confirm"`
		DryRun      bool   `json:"dryRun"`
	}
	if err = readJSON(r, &req); err != nil {
		return
	}
	masterKey = strings.TrimSpace(req.MasterKey)
	confirm = strings.TrimSpace(req.Confirm)
	dryRun = req.DryRun
	identities, ierr := s.allAgeIdentities(req.AgeIdentity)
	if ierr != nil {
		err = ierr
		return
	}
	tmpDB, sourceDesc, err = s.openLocalPanelSnapshot(req.Snapshot, identities)
	return
}

func (s *Server) openLocalPanelSnapshot(name string, identities []string) (string, string, error) {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == "/" || !strings.HasPrefix(base, "panel-") || !strings.HasSuffix(base, ".db") {
		return "", "", bad("本地快照文件名无效")
	}
	p := filepath.Join(s.cfg.BackupsDir, "_panel", base)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", errNotFound
		}
		return "", "", err
	}
	tmpDB, err := s.extractPanelStreamToTemp(f, false, identities)
	return tmpDB, "_panel/" + base, err
}

// handleSystemPanelRestore handles POST /api/system/panel-restore (admin).
func (s *Server) handleSystemPanelRestore(w http.ResponseWriter, r *http.Request, u *User) error {
	tmpDB, sourceDesc, masterKey, confirm, dryRun, err := s.readPanelRestoreInput(r, true, false)
	if err != nil {
		return err
	}
	defer os.Remove(tmpDB)

	if dryRun {
		sum, err := s.ValidatePanelBackup(tmpDB, masterKey)
		if err != nil {
			return err
		}
		return ok(w, map[string]any{"dryRun": true, "summary": sum})
	}
	if confirm != "RESTORE" && confirm != "恢复面板" {
		return bad("请输入 RESTORE 确认覆盖恢复面板数据")
	}
	sum, err := s.stageAndTriggerPanelRestore(r, u, tmpDB, masterKey, sourceDesc, false)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"restarting": true, "bootId": s.bootID, "summary": sum})
}

// handleSetupRestore handles POST /api/setup/restore on an uninitialized panel using setupToken.
func (s *Server) handleSetupRestore(w http.ResponseWriter, r *http.Request) error {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()

	if s.hasUsers() {
		return errStatus(http.StatusConflict, "已经初始化")
	}
	rateKey := "setup:" + clientIP(r)
	if s.tooManyFailures(rateKey) {
		return errStatus(http.StatusTooManyRequests, "初始化令牌尝试次数过多，请 15 分钟后再试")
	}

	// Reject invalid X-Setup-Token header BEFORE reading or buffering any multipart body!
	headerToken := strings.TrimSpace(r.Header.Get("X-Setup-Token"))
	tokenVerified := false
	if headerToken != "" {
		if s.setupToken == "" || !auth.ConstantEqual(headerToken, s.setupToken) {
			s.recordFailure(rateKey)
			return errStatus(http.StatusForbidden, "初始化令牌不正确（见面板容器日志）")
		}
		tokenVerified = true
	}

	tmpDB, sourceDesc, masterKey, _, dryRun, err := s.readPanelRestoreInput(r, false, !tokenVerified)
	if err != nil {
		return err
	}
	defer os.Remove(tmpDB)

	if dryRun {
		sum, err := s.ValidatePanelBackup(tmpDB, masterKey)
		if err != nil {
			return err
		}
		return ok(w, map[string]any{"dryRun": true, "summary": sum})
	}
	sum, err := s.stageAndTriggerPanelRestore(r, nil, tmpDB, masterKey, sourceDesc, true)
	if err != nil {
		return err
	}
	s.setupToken = ""
	return ok(w, map[string]any{"restarting": true, "bootId": s.bootID, "summary": sum})
}

func loadKeyOrPath(arg string) (string, error) {
	s := strings.TrimSpace(arg)
	if s == "" {
		return "", nil
	}
	if info, err := os.Stat(s); err == nil && !info.IsDir() {
		raw, err := os.ReadFile(s)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return s, nil
}

// RestorePanelCLI restores a panel backup offline (when the panel server is stopped).
func RestorePanelCLI(cfg config.Config, backupFile, masterKeyArg, ageKeyArg string) error {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return err
	}
	masterKeyRaw, err := loadKeyOrPath(masterKeyArg)
	if err != nil {
		return fmt.Errorf("读取 master-key 失败：%w", err)
	}
	_, masterKeyHex, err := parseMasterKeyHex(masterKeyRaw)
	if err != nil {
		return err
	}
	ageKeyRaw, err := loadKeyOrPath(ageKeyArg)
	if err != nil {
		return fmt.Errorf("读取 age-key 失败：%w", err)
	}
	var identities []string
	if ageKeyRaw != "" {
		if _, err := age.ParseX25519Identity(ageKeyRaw); err != nil {
			return fmt.Errorf("age 私钥格式无效：%w", err)
		}
		identities = append(identities, ageKeyRaw)
	}

	f, err := os.Open(backupFile)
	if err != nil {
		return err
	}
	encrypted := strings.HasSuffix(strings.ToLower(backupFile), ".age")
	plain, err := backup.OpenStream(f, encrypted, identities)
	if err != nil {
		return err
	}
	defer plain.Close()

	tmpFile, err := os.CreateTemp(cfg.DataDir, "panel-cli-restore-*.db")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmpFile, plain); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	var existingBox *secret.Box
	liveKeyPath := filepath.Join(cfg.DataDir, "master.key")
	if rawKey, err := os.ReadFile(liveKeyPath); err == nil {
		if kb, _, perr := parseMasterKeyHex(string(rawKey)); perr == nil && len(kb) == 32 {
			existingBox, _ = secret.New(kb)
		}
	}

	sum, err := validatePanelDBWithBox(tmpPath, existingBox, masterKeyHex)
	if err != nil {
		return err
	}
	if !sum.KeyMatched {
		return errors.New("备份中的加密数据无法用当前 master.key 解密，请通过 --master-key 指定原主密钥")
	}

	stamp := time.Now().Format("20060102-150405")
	liveDB := filepath.Join(cfg.DataDir, "panel.db")
	if _, err := os.Stat(liveDB); err == nil {
		_ = copyFileSynced(liveDB, filepath.Join(cfg.DataDir, "panel.db.pre-restore-"+stamp), 0o600)
	}
	if masterKeyHex != "" {
		if _, err := os.Stat(liveKeyPath); err == nil {
			_ = copyFileSynced(liveKeyPath, filepath.Join(cfg.DataDir, "master.key.pre-restore-"+stamp), 0o600)
		}
	}

	if masterKeyHex != "" {
		if err := os.WriteFile(filepath.Join(cfg.DataDir, panelPendingKeyName), []byte(masterKeyHex+"\n"), 0o600); err != nil {
			return err
		}
	}
	_ = os.WriteFile(filepath.Join(cfg.DataDir, panelClearSessionsName), []byte("1\n"), 0o600)
	if err := copyFileSynced(tmpPath, filepath.Join(cfg.DataDir, panelPendingDBName), 0o600); err != nil {
		return err
	}
	_, err = ApplyPendingPanelRestore(cfg.DataDir)
	return err
}

// Keep context import used cleanly for future extensions.
var _ = context.Background
