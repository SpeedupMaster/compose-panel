package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"compose-panel/internal/auth"
	"compose-panel/internal/backup"
	"compose-panel/internal/dbmgr"
	"compose-panel/internal/notify"
)

// BackupSource identifies what a backup contains.
type BackupSource struct {
	Kind       string `json:"kind"` // database | volume | dir | panel
	InstanceID int64  `json:"instanceId,omitempty"`
	Database   string `json:"database,omitempty"`
	Volume     string `json:"volume,omitempty"`
	ProjectID  int64  `json:"projectId,omitempty"`
	Path       string `json:"path,omitempty"`
}

func (s *Server) sourceLabel(src BackupSource) string {
	switch src.Kind {
	case "database":
		if d, err := s.getDB(src.InstanceID); err == nil {
			return d.Name + "/" + src.Database
		}
		return fmt.Sprintf("db%d/%s", src.InstanceID, src.Database)
	case "volume":
		return "volume/" + src.Volume
	case "dir":
		if p, err := s.getProject(src.ProjectID); err == nil {
			return p.Name + "/" + strings.Trim(src.Path, "/")
		}
	case "panel":
		return "panel"
	}
	return src.Kind
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func slug(s string) string { return strings.Trim(slugRe.ReplaceAllString(s, "_"), "_") }

func (s *Server) validateSource(src *BackupSource) error {
	switch src.Kind {
	case "database":
		if _, err := s.getDB(src.InstanceID); err != nil {
			return bad("数据库实例不存在")
		}
		if !dbmgr.ValidIdent(src.Database) && !strings.HasPrefix(src.Database, "db") {
			return bad("库名无效")
		}
	case "volume":
		if src.Volume == "" {
			return bad("请选择数据卷")
		}
	case "dir":
		if _, err := s.getProject(src.ProjectID); err != nil {
			return bad("项目不存在")
		}
		src.Path = strings.Trim(filepath.ToSlash(filepath.Clean("/"+src.Path)), "/")
		if src.Path == "" || strings.Contains(src.Path, "..") {
			return bad("目录无效")
		}
	case "panel":
	default:
		return bad("备份来源无效")
	}
	return nil
}

type storageTarget struct {
	ID           int64               `json:"id"`
	Name         string              `json:"name"`
	Kind         string              `json:"kind"`
	Config       backup.TargetConfig `json:"config"`
	CreatedAt    int64               `json:"createdAt"`
	LastCheckAt  int64               `json:"lastCheckAt"`
	LastCheckOK  bool                `json:"lastCheckOk"`
	LastCheckMsg string              `json:"lastCheckMsg"`
}

func (s *Server) getStorage(id int64) (*storageTarget, backup.Storage, error) {
	t := &storageTarget{}
	var enc string
	err := s.db.QueryRow(`SELECT id, name, kind, config, created_at, last_check_at, last_check_ok, last_check_msg FROM storage_targets WHERE id=?`, id).
		Scan(&t.ID, &t.Name, &t.Kind, &enc, &t.CreatedAt, &t.LastCheckAt, &t.LastCheckOK, &t.LastCheckMsg)
	if err != nil {
		return nil, nil, errNotFound
	}
	raw, err := s.box.Open(enc)
	if err != nil {
		return nil, nil, err
	}
	t.Config, _ = backup.ParseTargetConfig(raw)
	st, err := backup.Open(t.Kind, t.Config)
	return t, st, err
}

func (s *Server) recipients(encrypt bool, extra string) ([]age.Recipient, error) {
	if !encrypt {
		return nil, nil
	}
	return backup.ParseRecipients(s.setting("backup_age_recipient", ""), extra)
}

// ---------- running backups ----------

type backupRequest struct {
	PolicyID   int64
	Source     BackupSource
	StorageID  int64
	Encrypt    bool
	Recipients string
	Keep       *backup.Keep
	Actor      string
}

// submitBackup runs a backup as a job and returns its job id.
func (s *Server) submitBackup(req backupRequest) string {
	label := s.sourceLabel(req.Source)
	return s.jobs.Submit(jobSpec("backup", "备份 "+label, label, req.Actor, "backup:"+label), func(run *jobRun) error {
		_, err := s.runBackup(run, req)
		return err
	})
}

func (s *Server) runBackup(run *jobRun, req backupRequest) (int64, error) {
	ctx := run.Context()
	label := s.sourceLabel(req.Source)
	srcJSON, _ := json.Marshal(req.Source)
	var policy any
	if req.PolicyID > 0 {
		policy = req.PolicyID
	}
	now := time.Now()
	res, err := s.db.Exec(`INSERT INTO backups(policy_id,source,label,storage_id,status,job_id,started_at,encrypted) VALUES(?,?,?,?,?,?,?,?)`,
		policy, string(srcJSON), label, req.StorageID, "running", run.ID, now.Unix(), boolInt(req.Encrypt))
	if err != nil {
		return 0, err
	}
	bid, _ := res.LastInsertId()
	fail := func(err error) (int64, error) {
		_, _ = s.db.Exec(`UPDATE backups SET status='failed', error=?, finished_at=? WHERE id=?`, err.Error(), time.Now().Unix(), bid)
		s.notify.Emit(notify.Event{Type: "backup_failed", Title: "备份失败：" + label, Body: err.Error(), Target: label})
		return bid, err
	}
	target, st, err := s.getStorage(req.StorageID)
	if err != nil {
		return fail(fmt.Errorf("存储目标不可用：%w", err))
	}
	rcp, err := s.recipients(req.Encrypt, req.Recipients)
	if err != nil {
		return fail(err)
	}
	run.Logf("来源：%s → 存储：%s（%s）", label, target.Name, target.Kind)
	src, format, err := s.openSource(ctx, run, req.Source)
	if err != nil {
		return fail(err)
	}
	ext := format + ".zst"
	if len(rcp) > 0 {
		ext += ".age"
		run.Logf("使用 age 加密（%d 个接收方）", len(rcp))
	}
	key := fmt.Sprintf("%s/%s.%s", slug(label), now.In(s.location()).Format("20060102-150405"), ext)
	run.Logf("写入 %s", key)
	start := time.Now()
	out, err := backup.Store(ctx, st, key, src, rcp)
	cerr := src.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		_ = st.Delete(context.Background(), key)
		return fail(err)
	}
	run.Logf("完成：%s，耗时 %s，SHA-256 %s", humanBytes(out.Size), time.Since(start).Round(time.Millisecond), out.SHA256[:16])
	_, _ = s.db.Exec(`UPDATE backups SET status='success', object_key=?, size=?, sha256=?, format=?, finished_at=? WHERE id=?`,
		key, out.Size, out.SHA256, format, time.Now().Unix(), bid)
	if req.PolicyID > 0 && req.Keep != nil {
		s.applyRetention(run, req.PolicyID, *req.Keep)
	}
	return bid, nil
}

// openSource returns a plaintext stream of the source and its format name.
func (s *Server) openSource(ctx context.Context, run *jobRun, src BackupSource) (io.ReadCloser, string, error) {
	switch src.Kind {
	case "database":
		d, err := s.getDB(src.InstanceID)
		if err != nil {
			return nil, "", err
		}
		cmd, format, err := d.client().DumpCmd(ctx, src.Database)
		if err != nil {
			return nil, "", err
		}
		run.Logf("$ docker exec %s %s ...", d.Container, cmd.Args[len(cmd.Args)-1])
		rc, err := backup.CmdReader(cmd, nil)
		return rc, format, err
	case "volume":
		cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "-v", src.Volume+":/v:ro", s.cfg.HelperImage, "tar", "-C", "/v", "-cf", "-", ".")
		run.Logf("$ docker run --rm -v %s:/v:ro %s tar -C /v -cf - .", src.Volume, s.cfg.HelperImage)
		rc, err := backup.CmdReader(cmd, nil)
		return rc, "tar", err
	case "dir":
		p, err := s.getProject(src.ProjectID)
		if err != nil {
			return nil, "", err
		}
		dir := filepath.Join(s.projectDir(p), filepath.FromSlash(src.Path))
		if _, err := os.Stat(dir); err != nil {
			return nil, "", fmt.Errorf("目录不存在：%s", dir)
		}
		return backup.TarDir(dir), "tar", nil
	case "panel":
		tmp := filepath.Join(s.cfg.DataDir, fmt.Sprintf("panel-snapshot-%d.db", time.Now().UnixNano()))
		if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
			return nil, "", err
		}
		f, err := os.Open(tmp)
		if err != nil {
			return nil, "", err
		}
		return &removeOnClose{File: f}, "sqlite", nil
	}
	return nil, "", fmt.Errorf("unknown source")
}

type removeOnClose struct{ *os.File }

func (r *removeOnClose) Close() error {
	err := r.File.Close()
	os.Remove(r.File.Name())
	return err
}

func (s *Server) applyRetention(run *jobRun, policyID int64, keep backup.Keep) {
	rows, err := s.db.Query(`SELECT id, started_at FROM backups WHERE policy_id=? AND status='success'`, policyID)
	if err != nil {
		return
	}
	var items []backup.Item
	for rows.Next() {
		var id, ts int64
		if rows.Scan(&id, &ts) == nil {
			items = append(items, backup.Item{ID: id, Time: time.Unix(ts, 0)})
		}
	}
	rows.Close()
	tiers := backup.Retain(items, keep, s.location())
	removed := 0
	for _, it := range items {
		if t, ok := tiers[it.ID]; ok {
			_, _ = s.db.Exec(`UPDATE backups SET tiers=? WHERE id=?`, t, it.ID)
			continue
		}
		if err := s.deleteBackup(it.ID, false); err != nil {
			run.Logf("清理旧备份 %d 失败：%v", it.ID, err)
		} else {
			removed++
		}
	}
	// Failed attempts older than 30 days are only noise.
	_, _ = s.db.Exec(`DELETE FROM backups WHERE policy_id=? AND status='failed' AND started_at<?`, policyID, time.Now().Add(-30*24*time.Hour).Unix())
	run.Logf("保留策略：保留 %d 份，清理 %d 份", len(tiers), removed)
}

func (s *Server) deleteBackup(id int64, keepFile bool) error {
	var sid int64
	var key string
	if err := s.db.QueryRow(`SELECT storage_id, object_key FROM backups WHERE id=?`, id).Scan(&sid, &key); err != nil {
		return err
	}
	if !keepFile && key != "" {
		if _, st, err := s.getStorage(sid); err == nil {
			if err := st.Delete(context.Background(), key); err != nil {
				return err
			}
		}
	}
	_, err := s.db.Exec(`DELETE FROM backups WHERE id=?`, id)
	return err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---------- policies ----------

type policy struct {
	ID           int64        `json:"id"`
	Name         string       `json:"name"`
	Source       BackupSource `json:"source"`
	Label        string       `json:"label"`
	Schedule     string       `json:"schedule"`
	KeepDaily    int          `json:"keepDaily"`
	KeepWeekly   int          `json:"keepWeekly"`
	KeepMonthly  int          `json:"keepMonthly"`
	KeepYearly   int          `json:"keepYearly"`
	StorageID    int64        `json:"storageId"`
	StorageName  string       `json:"storageName"`
	Encrypt      bool         `json:"encrypt"`
	AgeRecipient string       `json:"ageRecipient"`
	Enabled      bool         `json:"enabled"`
	CreatedAt    int64        `json:"createdAt"`
	LastRun      *backupRow   `json:"lastRun"`
	NextRun      int64        `json:"nextRun"`
	Count        int          `json:"count"`
	TotalSize    int64        `json:"totalSize"`
}

const policyCols = `p.id, p.name, p.source, p.schedule, p.keep_daily, p.keep_weekly, p.keep_monthly, p.keep_yearly, p.storage_id, COALESCE(t.name,''), p.encrypt, p.age_recipient, p.enabled, p.created_at`

func (s *Server) scanPolicy(row interface{ Scan(...any) error }) (*policy, error) {
	p := &policy{}
	var src string
	if err := row.Scan(&p.ID, &p.Name, &src, &p.Schedule, &p.KeepDaily, &p.KeepWeekly, &p.KeepMonthly, &p.KeepYearly, &p.StorageID, &p.StorageName, &p.Encrypt, &p.AgeRecipient, &p.Enabled, &p.CreatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(src), &p.Source)
	p.Label = s.sourceLabel(p.Source)
	return p, nil
}

func (s *Server) getPolicy(id int64) (*policy, error) {
	p, err := s.scanPolicy(s.db.QueryRow(`SELECT `+policyCols+` FROM backup_policies p LEFT JOIN storage_targets t ON t.id=p.storage_id WHERE p.id=?`, id))
	if err == sql.ErrNoRows {
		return nil, errNotFound
	}
	return p, err
}

func (p *policy) keep() backup.Keep {
	return backup.Keep{Daily: p.KeepDaily, Weekly: p.KeepWeekly, Monthly: p.KeepMonthly, Yearly: p.KeepYearly}
}

func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request, u *User) error {
	rows, err := s.db.Query(`SELECT ` + policyCols + ` FROM backup_policies p LEFT JOIN storage_targets t ON t.id=p.storage_id ORDER BY p.id`)
	if err != nil {
		return err
	}
	var list []*policy
	for rows.Next() {
		p, err := s.scanPolicy(rows)
		if err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	for _, p := range list {
		b := s.listBackups(`WHERE b.policy_id=? ORDER BY b.started_at DESC LIMIT 1`, p.ID)
		if len(b) > 0 {
			p.LastRun = &b[0]
		}
		_ = s.db.QueryRow(`SELECT count(*), COALESCE(sum(size),0) FROM backups WHERE policy_id=? AND status='success'`, p.ID).Scan(&p.Count, &p.TotalSize)
		p.NextRun = s.nextRun("policy:" + fmt.Sprint(p.ID))
	}
	return ok(w, nonNil(list))
}

type policyReq struct {
	Name         string       `json:"name"`
	Source       BackupSource `json:"source"`
	Schedule     string       `json:"schedule"`
	KeepDaily    int          `json:"keepDaily"`
	KeepWeekly   int          `json:"keepWeekly"`
	KeepMonthly  int          `json:"keepMonthly"`
	KeepYearly   int          `json:"keepYearly"`
	StorageID    int64        `json:"storageId"`
	Encrypt      bool         `json:"encrypt"`
	AgeRecipient string       `json:"ageRecipient"`
	Enabled      bool         `json:"enabled"`
}

func (s *Server) checkPolicyReq(req *policyReq) error {
	if err := s.validateSource(&req.Source); err != nil {
		return err
	}
	if _, err := cronParser.Parse(req.Schedule); err != nil {
		return bad("Cron 表达式无效：%v", err)
	}
	if _, _, err := s.getStorage(req.StorageID); err != nil {
		return bad("存储目标无效")
	}
	if _, err := backup.ParseRecipients(req.AgeRecipient); err != nil {
		return bad("%v", err)
	}
	if req.KeepDaily+req.KeepWeekly+req.KeepMonthly+req.KeepYearly == 0 {
		return bad("至少保留一份备份")
	}
	if req.Name == "" {
		req.Name = s.sourceLabel(req.Source)
	}
	return nil
}

func (s *Server) handleCreatePolicy(w http.ResponseWriter, r *http.Request, u *User) error {
	var req policyReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := s.checkPolicyReq(&req); err != nil {
		return err
	}
	src, _ := json.Marshal(req.Source)
	res, err := s.db.Exec(`INSERT INTO backup_policies(name,source,schedule,keep_daily,keep_weekly,keep_monthly,keep_yearly,storage_id,encrypt,age_recipient,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.Name, string(src), req.Schedule, req.KeepDaily, req.KeepWeekly, req.KeepMonthly, req.KeepYearly, req.StorageID, boolInt(req.Encrypt), req.AgeRecipient, boolInt(req.Enabled), time.Now().Unix())
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	s.reloadSchedules()
	s.audit(r, u, "backup.policy_create", req.Name, req.Schedule, nil)
	return ok(w, map[string]any{"id": id})
}

func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req policyReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := s.checkPolicyReq(&req); err != nil {
		return err
	}
	src, _ := json.Marshal(req.Source)
	_, err = s.db.Exec(`UPDATE backup_policies SET name=?, source=?, schedule=?, keep_daily=?, keep_weekly=?, keep_monthly=?, keep_yearly=?, storage_id=?, encrypt=?, age_recipient=?, enabled=? WHERE id=?`,
		req.Name, string(src), req.Schedule, req.KeepDaily, req.KeepWeekly, req.KeepMonthly, req.KeepYearly, req.StorageID, boolInt(req.Encrypt), req.AgeRecipient, boolInt(req.Enabled), id)
	s.reloadSchedules()
	s.audit(r, u, "backup.policy_update", req.Name, req.Schedule, err)
	return done(w, err)
}

func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	p, err := s.getPolicy(id)
	if err != nil {
		return err
	}
	if r.URL.Query().Get("backups") == "1" {
		rows, _ := s.db.Query(`SELECT id FROM backups WHERE policy_id=?`, id)
		var ids []int64
		if rows != nil {
			for rows.Next() {
				var b int64
				_ = rows.Scan(&b)
				ids = append(ids, b)
			}
			rows.Close()
		}
		for _, b := range ids {
			_ = s.deleteBackup(b, false)
		}
	}
	_, err = s.db.Exec(`DELETE FROM backup_policies WHERE id=?`, id)
	s.reloadSchedules()
	s.audit(r, u, "backup.policy_delete", p.Name, "", err)
	return done(w, err)
}

func (s *Server) runPolicy(p *policy, actor string) string {
	k := p.keep()
	return s.submitBackup(backupRequest{PolicyID: p.ID, Source: p.Source, StorageID: p.StorageID, Encrypt: p.Encrypt, Recipients: p.AgeRecipient, Keep: &k, Actor: actor})
}

func (s *Server) handleRunPolicy(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	p, err := s.getPolicy(id)
	if err != nil {
		return err
	}
	jobID := s.runPolicy(p, u.Username)
	s.audit(r, u, "backup.run", p.Name, "手动执行", nil)
	return ok(w, map[string]any{"jobId": jobID})
}

// handleBackupNow runs an ad-hoc backup without a policy.
func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Source    BackupSource `json:"source"`
		StorageID int64        `json:"storageId"`
		Encrypt   bool         `json:"encrypt"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := s.validateSource(&req.Source); err != nil {
		return err
	}
	if req.StorageID == 0 {
		_ = s.db.QueryRow(`SELECT id FROM storage_targets ORDER BY id LIMIT 1`).Scan(&req.StorageID)
	}
	jobID := s.submitBackup(backupRequest{Source: req.Source, StorageID: req.StorageID, Encrypt: req.Encrypt, Actor: u.Username})
	s.audit(r, u, "backup.run", s.sourceLabel(req.Source), "临时备份", nil)
	return ok(w, map[string]any{"jobId": jobID})
}

// ---------- backup records ----------

type backupRow struct {
	ID          int64        `json:"id"`
	PolicyID    int64        `json:"policyId"`
	Source      BackupSource `json:"source"`
	Label       string       `json:"label"`
	StorageID   int64        `json:"storageId"`
	StorageName string       `json:"storageName"`
	ObjectKey   string       `json:"objectKey"`
	Size        int64        `json:"size"`
	SHA256      string       `json:"sha256"`
	Encrypted   bool         `json:"encrypted"`
	Format      string       `json:"format"`
	Status      string       `json:"status"`
	Error       string       `json:"error"`
	Tiers       string       `json:"tiers"`
	JobID       string       `json:"jobId"`
	StartedAt   int64        `json:"startedAt"`
	FinishedAt  int64        `json:"finishedAt"`
	VerifiedAt  int64        `json:"verifiedAt"` // last successful restore drill
	Origin      string       `json:"origin"`     // backup | upload | scan
}

func (s *Server) listBackups(where string, args ...any) []backupRow {
	rows, err := s.db.Query(`SELECT b.id, COALESCE(b.policy_id,0), b.source, b.label, b.storage_id, COALESCE(t.name,''), b.object_key, b.size, b.sha256, b.encrypted, b.format, b.status, b.error, b.tiers, b.job_id, b.started_at, b.finished_at, b.verified_at, COALESCE(b.origin,'backup')
		FROM backups b LEFT JOIN storage_targets t ON t.id=b.storage_id `+where, args...)
	if err != nil {
		return []backupRow{}
	}
	defer rows.Close()
	out := []backupRow{}
	for rows.Next() {
		var b backupRow
		var src string
		if rows.Scan(&b.ID, &b.PolicyID, &src, &b.Label, &b.StorageID, &b.StorageName, &b.ObjectKey, &b.Size, &b.SHA256, &b.Encrypted, &b.Format, &b.Status, &b.Error, &b.Tiers, &b.JobID, &b.StartedAt, &b.FinishedAt, &b.VerifiedAt, &b.Origin) == nil {
			_ = json.Unmarshal([]byte(src), &b.Source)
			out = append(out, b)
		}
	}
	return out
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request, u *User) error {
	where, args := "WHERE 1=1", []any{}
	q := r.URL.Query()
	if v := q.Get("policy"); v != "" {
		where += " AND b.policy_id=?"
		args = append(args, v)
	}
	if v := q.Get("status"); v != "" {
		where += " AND b.status=?"
		args = append(args, v)
	}
	if v := q.Get("instance"); v != "" {
		where += " AND json_extract(b.source,'$.kind')='database' AND json_extract(b.source,'$.instanceId')=?"
		args = append(args, v)
	}
	if v := q.Get("q"); v != "" {
		where += " AND b.label LIKE ?"
		args = append(args, "%"+v+"%")
	}
	limit := min(max(queryInt(r, "limit", 100), 1), 500)
	return ok(w, s.listBackups(where+" ORDER BY b.started_at DESC LIMIT ?", append(args, limit)...))
}

func (s *Server) backupByID(r *http.Request) (*backupRow, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	list := s.listBackups("WHERE b.id=?", id)
	if len(list) == 0 {
		return nil, errNotFound
	}
	return &list[0], nil
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	b, err := s.backupByID(r)
	if err != nil {
		return err
	}
	keepFile := r.URL.Query().Get("keepFile") == "1"
	err = s.deleteBackup(b.ID, keepFile)
	detail := b.ObjectKey
	if keepFile {
		detail += "（保留存储文件）"
	}
	s.audit(r, u, "backup.delete", b.Label, detail, err)
	return done(w, err)
}

func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	b, err := s.backupByID(r)
	if err != nil {
		return err
	}
	if b.Status != "success" {
		return bad("备份未成功完成")
	}
	_, st, err := s.getStorage(b.StorageID)
	if err != nil {
		return err
	}
	plain := r.URL.Query().Get("plain") == "1"
	var rc io.ReadCloser
	name := filepath.Base(b.ObjectKey)
	if plain {
		ids, ierr := s.allAgeIdentities("")
		if ierr != nil {
			return ierr
		}
		rc, err = backup.OpenObject(r.Context(), st, b.ObjectKey, b.Encrypted, ids...)
		name = strings.TrimSuffix(strings.TrimSuffix(name, ".age"), ".zst")
	} else {
		rc, err = st.Get(r.Context(), b.ObjectKey)
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	s.audit(r, u, "backup.download", b.Label, name, nil)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s"`, slug(b.Label), name))
	if !plain && b.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(b.Size))
	}
	_, _ = io.Copy(w, rc)
	return nil
}

func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	b, err := s.backupByID(r)
	if err != nil {
		return err
	}
	var req struct {
		AgeIdentity string `json:"ageIdentity"`
	}
	if r.Body != nil {
		_ = readJSON(r, &req)
	}
	ids, err := s.allAgeIdentities(req.AgeIdentity)
	if err != nil {
		return err
	}
	jobID := s.jobs.Submit(jobSpec("backup.verify", "校验备份 "+b.Label, b.Label, u.Username, ""), func(run *jobRun) error {
		_, st, err := s.getStorage(b.StorageID)
		if err != nil {
			return err
		}
		run.Logf("下载并计算 SHA-256：%s", b.ObjectKey)
		rcRaw, err := st.Get(run.Context(), b.ObjectKey)
		if err != nil {
			return fmt.Errorf("下载失败：%w", err)
		}
		h := sha256.New()
		gotSize, copyErr := io.Copy(h, rcRaw)
		rcRaw.Close()
		if copyErr != nil {
			return fmt.Errorf("读取失败：%w", copyErr)
		}
		gotSHA := hex.EncodeToString(h.Sum(nil))
		if b.Size > 0 && gotSize != b.Size {
			return fmt.Errorf("大小不一致：期望 %d，实际 %d", b.Size, gotSize)
		}
		if b.SHA256 != "" && gotSHA != b.SHA256 {
			return fmt.Errorf("SHA-256 校验失败")
		}
		if b.SHA256 != "" {
			run.Logf("校验和一致（%s）", gotSHA[:16])
		} else {
			run.Logf("SHA-256：%s，大小 %s", gotSHA[:16], humanBytes(gotSize))
		}
		rc, err := backup.OpenObject(run.Context(), st, b.ObjectKey, b.Encrypted, ids...)
		if err != nil {
			return err
		}
		n, err := io.Copy(io.Discard, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("解压/解密失败：%w", err)
		}
		if b.SHA256 == "" || b.Size == 0 {
			_, _ = s.db.Exec(`UPDATE backups SET sha256=?, size=? WHERE id=?`, gotSHA, gotSize, b.ID)
			run.Logf("已回填 SHA-256 与文件大小")
		}
		run.Logf("解密并解压成功，原始大小 %s", humanBytes(n))
		return nil
	})
	return ok(w, map[string]any{"jobId": jobID})
}

type restorePlan struct {
	Kind           string
	Mode           string // new | overwrite
	InstanceID     int64
	Database       string
	SourceDatabase string
	Volume         string
	ProjectID      int64
	Path           string
	TargetLabel    string
}

func formatCompatibleWithEngine(format, engine string) bool {
	switch format {
	case "", "any":
		return true
	case "pgdump":
		return engine == "postgres"
	case "sql":
		return engine == "postgres" || engine == "mysql" || engine == "mariadb"
	case "archive":
		return engine == "mongo"
	case "rdb":
		return engine == "redis"
	default:
		return false
	}
}

func pathOverlaps(a, b string) bool {
	ca := filepath.Clean(a)
	cb := filepath.Clean(b)
	if ca == cb {
		return true
	}
	sep := string(filepath.Separator)
	aPfx := ca
	if !strings.HasSuffix(aPfx, sep) {
		aPfx += sep
	}
	bPfx := cb
	if !strings.HasSuffix(bPfx, sep) {
		bPfx += sep
	}
	return strings.HasPrefix(cb, aPfx) || strings.HasPrefix(ca, bPfx)
}

func (s *Server) runningVolumeHolders(ctx context.Context, volume string) ([]string, error) {
	cs, err := s.dk.ListContainers(ctx, false, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, c := range cs {
		if c.State != "" && c.State != "running" {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name == volume {
				names = append(names, c.Name())
				break
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *Server) runningBindHolders(ctx context.Context, targetDir string) ([]string, error) {
	cs, err := s.dk.ListContainers(ctx, false, nil)
	if err != nil {
		return nil, err
	}
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		absTarget = filepath.Clean(targetDir)
	}
	var names []string
	for _, c := range cs {
		if c.State != "" && c.State != "running" {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == "bind" && m.Source != "" && pathOverlaps(absTarget, m.Source) {
				names = append(names, c.Name())
				break
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	b, err := s.backupByID(r)
	if err != nil {
		return err
	}
	if b.Status != "success" {
		return bad("备份未成功完成")
	}
	var req struct {
		Kind           string `json:"kind"`
		Mode           string `json:"mode"` // new | overwrite
		InstanceID     int64  `json:"instanceId"`
		Database       string `json:"database"`
		SourceDatabase string `json:"sourceDatabase"`
		Volume         string `json:"volume"`
		ProjectID      int64  `json:"projectId"`
		Path           string `json:"path"`
		Target         string `json:"target"` // legacy field: new database / volume / directory name
		Confirm        string `json:"confirm"`
		AgeIdentity    string `json:"ageIdentity"`
		MasterKey      string `json:"masterKey"`
		DryRun         bool   `json:"dryRun"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}

	identities, err := s.allAgeIdentities(req.AgeIdentity)
	if err != nil {
		return err
	}

	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = b.Source.Kind
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "new"
	}
	if mode != "new" && mode != "overwrite" {
		return bad("恢复方式无效")
	}

	plan := restorePlan{Kind: kind, Mode: mode}
	switch kind {
	case "database":
		instID := req.InstanceID
		if instID == 0 {
			instID = b.Source.InstanceID
		}
		d, err := s.getDB(instID)
		if err != nil {
			return bad("请选择有效的目标数据库实例")
		}
		if !formatCompatibleWithEngine(b.Format, d.Engine) {
			return bad("备份格式 %s 与目标实例引擎 %s 不兼容", b.Format, d.Engine)
		}
		plan.InstanceID = d.ID
		plan.SourceDatabase = strings.TrimSpace(req.SourceDatabase)
		if plan.SourceDatabase == "" {
			plan.SourceDatabase = b.Source.Database
		}
		if d.Engine == "redis" {
			if req.Confirm != d.Name {
				return bad("Redis 只能整库覆盖恢复，请输入实例名 %s 确认", d.Name)
			}
			plan.Mode = "overwrite"
			plan.Database = "all"
			plan.TargetLabel = d.Name
			break
		}
		targetDB := strings.TrimSpace(req.Database)
		if targetDB == "" {
			targetDB = strings.TrimSpace(req.Target)
		}
		if targetDB == "" && mode == "overwrite" {
			targetDB = b.Source.Database
		}
		if !dbmgr.ValidIdent(targetDB) {
			return bad("目标库名无效")
		}
		if mode == "overwrite" && req.Confirm != targetDB {
			return bad("请输入库名 %s 确认覆盖", targetDB)
		}
		plan.Database = targetDB
		plan.TargetLabel = d.Name + "/" + targetDB

	case "volume":
		vol := strings.TrimSpace(req.Volume)
		if vol == "" {
			vol = strings.TrimSpace(req.Target)
		}
		if vol == "" && mode == "overwrite" {
			vol = b.Source.Volume
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`).MatchString(vol) {
			return bad("数据卷名无效")
		}
		if mode == "overwrite" {
			if req.Confirm != vol {
				return bad("请输入数据卷名 %s 确认覆盖", vol)
			}
			holders, herr := s.runningVolumeHolders(r.Context(), vol)
			if herr != nil {
				return fmt.Errorf("检查容器挂载状态失败：%w", herr)
			}
			if len(holders) > 0 {
				return bad("数据卷 %s 正被运行中的容器挂载（%s），请先停止相关项目或容器后再覆盖恢复", vol, strings.Join(holders, ", "))
			}
		}
		plan.Volume = vol
		plan.TargetLabel = "volume/" + vol

	case "dir":
		pid := req.ProjectID
		if pid == 0 {
			pid = b.Source.ProjectID
		}
		p, err := s.getProject(pid)
		if err != nil || p.Kind != "app" {
			return bad("请选择有效的目标项目")
		}
		rel := strings.TrimSpace(req.Path)
		if rel == "" {
			rel = strings.TrimSpace(req.Target)
		}
		if rel == "" && mode == "overwrite" {
			rel = b.Source.Path
		}
		rel = strings.Trim(filepath.ToSlash(filepath.Clean("/"+rel)), "/")
		if rel == "" || strings.Contains(rel, "..") || rel == ".panel" || strings.HasPrefix(rel, ".panel/") {
			return bad("目标目录无效")
		}
		if mode == "overwrite" {
			fullLabel := p.Name + "/" + rel
			if req.Confirm != fullLabel && req.Confirm != rel {
				return bad("请输入 %s 确认覆盖目录", fullLabel)
			}
			targetDir := filepath.Join(s.projectDir(p), filepath.FromSlash(rel))
			holders, herr := s.runningBindHolders(r.Context(), targetDir)
			if herr != nil {
				return fmt.Errorf("检查容器挂载状态失败：%w", herr)
			}
			if len(holders) > 0 {
				return bad("目录 %s 正被运行中的容器挂载（%s），请先停止相关项目或容器后再覆盖恢复", fullLabel, strings.Join(holders, ", "))
			}
		}
		plan.ProjectID = p.ID
		plan.Path = rel
		plan.TargetLabel = p.Name + "/" + rel

	case "panel":
		return bad("面板数据恢复尚未启用")

	default:
		return bad("该类型备份暂不支持恢复")
	}

	jobID := s.jobs.Submit(jobSpec("backup.restore", "恢复 "+b.Label+" → "+plan.TargetLabel, plan.TargetLabel, u.Username, "backup:"+plan.TargetLabel), func(run *jobRun) error {
		return s.runRestore(run, b, plan, identities, u.Username)
	})
	s.audit(r, u, "backup.restore", b.Label, plan.Mode+" → "+plan.TargetLabel, nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) preBackupStorageID(preferred int64) int64 {
	if preferred > 0 {
		if _, _, err := s.getStorage(preferred); err == nil {
			return preferred
		}
	}
	var sid int64
	_ = s.db.QueryRow(`SELECT id FROM storage_targets ORDER BY id LIMIT 1`).Scan(&sid)
	return sid
}

func (s *Server) runRestore(run *jobRun, b *backupRow, plan restorePlan, identities []string, actor string) error {
	ctx := run.Context()
	_, st, err := s.getStorage(b.StorageID)
	if err != nil {
		return err
	}
	run.Logf("==> 校验备份完整性")
	if b.SHA256 != "" || b.Size > 0 {
		if err := backup.Verify(ctx, st, b.ObjectKey, b.SHA256, b.Size); err != nil {
			return err
		}
	} else {
		run.Logf("（扫描导入的备份尚未记录 SHA-256，跳过哈希比对，直接进入解密解包）")
	}
	run.SetProgress(20)
	safetySID := s.preBackupStorageID(b.StorageID)

	switch plan.Kind {
	case "database":
		d, err := s.getDB(plan.InstanceID)
		if err != nil {
			return err
		}
		cl := d.client()
		if d.Engine == "redis" {
			run.Logf("==> 覆盖前先备份当前数据")
			curSrc := BackupSource{Kind: "database", InstanceID: d.ID, Database: "all"}
			if _, err := s.runBackup(run, backupRequest{Source: curSrc, StorageID: safetySID, Encrypt: b.Encrypted, Actor: actor}); err != nil {
				return fmt.Errorf("覆盖前备份失败，已中止：%w", err)
			}
			run.SetProgress(40)
			rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
			if err != nil {
				return err
			}
			defer rc.Close()
			if err := s.restoreRedis(run, d, rc); err != nil {
				return fmt.Errorf("恢复失败：%w", err)
			}
			break
		}

		dbExists := false
		if list, lerr := cl.ListDatabases(ctx); lerr == nil {
			for _, item := range list {
				if item.Name == plan.Database {
					dbExists = true
					break
				}
			}
		}

		if plan.Mode == "overwrite" {
			if dbExists {
				run.Logf("==> 覆盖前先备份当前库 %s", plan.Database)
				curSrc := BackupSource{Kind: "database", InstanceID: d.ID, Database: plan.Database}
				if _, err := s.runBackup(run, backupRequest{Source: curSrc, StorageID: safetySID, Encrypt: b.Encrypted, Actor: actor}); err != nil {
					return fmt.Errorf("覆盖前备份失败，已中止：%w", err)
				}
				if d.Engine == "mysql" || d.Engine == "mariadb" {
					if err := cl.DropDatabase(ctx, plan.Database); err != nil {
						return err
					}
					if err := cl.CreateDatabase(ctx, plan.Database, "", ""); err != nil {
						return err
					}
				}
			} else if d.Engine != "mongo" {
				run.Logf("==> 目标库 %s 尚不存在，自动创建", plan.Database)
				if err := cl.CreateDatabase(ctx, plan.Database, "", ""); err != nil {
					return err
				}
			}
		} else {
			run.Logf("==> 创建新库 %s", plan.Database)
			if d.Engine != "mongo" {
				if err := cl.CreateDatabase(ctx, plan.Database, "", ""); err != nil {
					return err
				}
			}
		}
		run.SetProgress(40)
		run.Logf("==> 导入数据到 %s", plan.TargetLabel)
		rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
		if err != nil {
			return err
		}
		defer rc.Close()

		var input io.Reader = rc
		var cmd *exec.Cmd
		if d.Engine == "postgres" {
			br := bufio.NewReaderSize(rc, 64<<10)
			magic, _ := br.Peek(5)
			input = br
			if string(magic) == "PGDMP" {
				cmd, err = cl.RestoreCmd(ctx, plan.SourceDatabase, plan.Database, plan.Mode == "overwrite")
			} else {
				if plan.Mode == "overwrite" && dbExists {
					run.Logf("==> 检测到 PostgreSQL 明文 SQL，正在断开其他连接并重置用户 Schema")
					if err := cl.ResetPostgresSchemas(ctx, plan.Database); err != nil {
						return err
					}
				}
				cmd, err = cl.ImportCmd(ctx, plan.Database, "sql")
			}
		} else {
			cmd, err = cl.RestoreCmd(ctx, plan.SourceDatabase, plan.Database, plan.Mode == "overwrite")
		}
		if err != nil {
			return err
		}
		if err := dbmgr.RunWithInput(cmd, input, run); err != nil {
			return fmt.Errorf("导入失败：%w", err)
		}

	case "volume":
		volExists := false
		if vols, _ := s.dk.ListVolumes(ctx); vols != nil {
			for _, v := range vols {
				if v.Name == plan.Volume {
					volExists = true
					break
				}
			}
		}
		if plan.Mode == "overwrite" {
			if !volExists {
				return fmt.Errorf("数据卷 %s 不存在，无法覆盖", plan.Volume)
			}
			holders, herr := s.runningVolumeHolders(ctx, plan.Volume)
			if herr != nil {
				return fmt.Errorf("检查容器挂载状态失败：%w", herr)
			}
			if len(holders) > 0 {
				return fmt.Errorf("数据卷 %s 正被运行中的容器挂载（%s），请先停止相关容器", plan.Volume, strings.Join(holders, ", "))
			}
			run.Logf("==> 覆盖前先备份当前数据卷 %s", plan.Volume)
			curSrc := BackupSource{Kind: "volume", Volume: plan.Volume}
			if _, err := s.runBackup(run, backupRequest{Source: curSrc, StorageID: safetySID, Encrypt: b.Encrypted, Actor: actor}); err != nil {
				return fmt.Errorf("覆盖前备份失败，已中止：%w", err)
			}
			run.SetProgress(40)
			rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
			if err != nil {
				return err
			}
			defer rc.Close()
			run.Logf("==> 清空并解包到数据卷 %s", plan.Volume)
			cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "-v", plan.Volume+":/v", s.cfg.HelperImage,
				"sh", "-c", "rm -rf /v/* /v/.[!.]* /v/..?* 2>/dev/null || true; tar -C /v -xf -")
			if err := dbmgr.RunWithInput(cmd, rc, run); err != nil {
				return err
			}
		} else {
			if volExists {
				return fmt.Errorf("数据卷 %s 已存在，请换一个名字或选择覆盖模式", plan.Volume)
			}
			if err := s.dk.CreateVolume(ctx, plan.Volume, map[string]string{"panel.restored-from": b.ObjectKey}); err != nil {
				return err
			}
			rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
			if err != nil {
				return err
			}
			defer rc.Close()
			cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "-v", plan.Volume+":/v", s.cfg.HelperImage, "tar", "-C", "/v", "-xf", "-")
			if err := dbmgr.RunWithInput(cmd, rc, run); err != nil {
				return err
			}
		}

	case "dir":
		p, err := s.getProject(plan.ProjectID)
		if err != nil {
			return err
		}
		dir := filepath.Join(s.projectDir(p), filepath.FromSlash(plan.Path))
		_, statErr := os.Stat(dir)
		exists := statErr == nil
		if plan.Mode == "overwrite" {
			holders, herr := s.runningBindHolders(ctx, dir)
			if herr != nil {
				return fmt.Errorf("检查容器挂载状态失败：%w", herr)
			}
			if len(holders) > 0 {
				return fmt.Errorf("目录 %s 正被运行中的容器挂载（%s），请先停止相关容器", plan.TargetLabel, strings.Join(holders, ", "))
			}
			if exists {
				run.Logf("==> 覆盖前先备份当前目录 %s", plan.TargetLabel)
				curSrc := BackupSource{Kind: "dir", ProjectID: p.ID, Path: plan.Path}
				if _, err := s.runBackup(run, backupRequest{Source: curSrc, StorageID: safetySID, Encrypt: b.Encrypted, Actor: actor}); err != nil {
					return fmt.Errorf("覆盖前备份失败，已中止：%w", err)
				}
			}
			run.SetProgress(40)
			rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
			if err != nil {
				return err
			}
			defer rc.Close()
			run.Logf("==> 安全解包并原子替换目录 %s", plan.TargetLabel)
			if err := s.restoreDirOverwrite(rc, dir); err != nil {
				return err
			}
		} else {
			if exists {
				return fmt.Errorf("目录 %s 已存在，请换一个名字或选择覆盖模式", plan.Path)
			}
			rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
			if err != nil {
				return err
			}
			defer rc.Close()
			if err := backup.UntarDir(rc, dir); err != nil {
				return err
			}
		}
	}
	run.Logf("==> 恢复完成：%s", plan.TargetLabel)
	return nil
}

// restoreDirOverwrite extracts rc into a sibling temporary directory and swaps it
// into dir atomically, rolling back if moving the new directory into place fails.
func (s *Server) restoreDirOverwrite(rc io.Reader, dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	tmpDir := filepath.Join(parent, ".panel-restore-tmp-"+stamp)
	oldDir := filepath.Join(parent, ".panel-restore-old-"+stamp)
	if err := backup.UntarDir(rc, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return err
	}
	hadOld := false
	if _, err := os.Stat(dir); err == nil {
		if err := os.Rename(dir, oldDir); err != nil {
			_ = os.RemoveAll(tmpDir)
			return fmt.Errorf("移走旧目录失败：%w", err)
		}
		hadOld = true
	}
	if err := os.Rename(tmpDir, dir); err != nil {
		if hadOld {
			_ = os.Rename(oldDir, dir)
		}
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("替换目标目录失败：%w", err)
	}
	if hadOld {
		_ = os.RemoveAll(oldDir)
	}
	return nil
}

// handleBackupSources lists what can be backed up.
func (s *Server) handleBackupSources(w http.ResponseWriter, r *http.Request, u *User) error {
	out := map[string]any{}
	dbs := []map[string]any{}
	rows, _ := s.db.Query(`SELECT id FROM db_instances`)
	var ids []int64
	if rows != nil {
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
	}
	for _, id := range ids {
		d, err := s.getDB(id)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		list, _ := d.client().ListDatabases(ctx)
		cancel()
		names := []string{}
		for _, x := range list {
			names = append(names, x.Name)
		}
		if d.Engine == "redis" {
			names = []string{"all"}
		}
		dbs = append(dbs, map[string]any{"id": d.ID, "name": d.Name, "engine": d.Engine, "databases": names})
	}
	out["databases"] = dbs
	vols, _ := s.dk.ListVolumes(r.Context())
	vn := []map[string]any{}
	for _, v := range vols {
		vn = append(vn, map[string]any{"name": v.Name, "project": v.Labels["com.docker.compose.project"]})
	}
	out["volumes"] = vn
	ps := []map[string]any{}
	prow, _ := s.db.Query(`SELECT ` + projectCols + ` FROM projects WHERE kind='app' ORDER BY name`)
	if prow != nil {
		for prow.Next() {
			p, err := s.scanProject(prow)
			if err != nil {
				continue
			}
			ps = append(ps, map[string]any{"id": p.ID, "name": p.Name, "dirs": s.detectProjectDirs(p)})
		}
		prow.Close()
	}
	out["projects"] = ps
	return ok(w, out)
}

// ---------- storage targets ----------

func (s *Server) handleListStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	rows, err := s.db.Query(`SELECT id FROM storage_targets ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := []map[string]any{}
	for _, id := range ids {
		t, _, err := s.getStorage(id)
		if err != nil && t == nil {
			continue
		}
		t.Config.SecretKey = maskSecret(t.Config.SecretKey)
		t.Config.Password = maskSecret(t.Config.Password)
		t.Config.ClientSecret = maskSecret(t.Config.ClientSecret)
		t.Config.RefreshToken = maskSecret(t.Config.RefreshToken)
		if t.Config.PrivateKey != "" {
			t.Config.PrivateKey = "••••••"
		}
		var count int
		var size int64
		_ = s.db.QueryRow(`SELECT count(*), COALESCE(sum(size),0) FROM backups WHERE storage_id=? AND status='success'`, id).Scan(&count, &size)
		var policies int
		_ = s.db.QueryRow(`SELECT count(*) FROM backup_policies WHERE storage_id=?`, id).Scan(&policies)
		out = append(out, map[string]any{"target": t, "backups": count, "size": size, "policies": policies})
	}
	return ok(w, out)
}

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	return "••••••"
}

type storageReq struct {
	ID     int64               `json:"id,omitempty"`
	Name   string              `json:"name"`
	Kind   string              `json:"kind"`
	Config backup.TargetConfig `json:"config"`
}

func (s *Server) mergeSavedStorageSecrets(id int64, cfg *backup.TargetConfig) {
	if id <= 0 {
		return
	}
	old, _, _ := s.getStorage(id)
	if old == nil {
		return
	}
	keep := func(v *string, o string) {
		if *v == "" || strings.Contains(*v, "••••••") {
			*v = o
		}
	}
	keep(&cfg.SecretKey, old.Config.SecretKey)
	keep(&cfg.Password, old.Config.Password)
	keep(&cfg.PrivateKey, old.Config.PrivateKey)
	keep(&cfg.ClientSecret, old.Config.ClientSecret)
	keep(&cfg.RefreshToken, old.Config.RefreshToken)
}

func (s *Server) handleSaveStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	var req storageReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Name == "" {
		return bad("名称不能为空")
	}
	id, _ := pathID(r, "id")
	if id > 0 {
		s.mergeSavedStorageSecrets(id, &req.Config)
	}
	if req.Kind == "gdrive" {
		req.Config.FolderID = backup.NormalizeGDriveFolderID(req.Config.FolderID)
	}
	if _, err := backup.Open(req.Kind, req.Config); err != nil {
		return bad("%v", err)
	}
	conf, _ := json.Marshal(req.Config)
	if id > 0 {
		_, err := s.db.Exec(`UPDATE storage_targets SET name=?, kind=?, config=? WHERE id=?`, req.Name, req.Kind, s.box.Seal(string(conf)), id)
		s.audit(r, u, "storage.update", req.Name, req.Kind, err)
		return done(w, err)
	}
	res, err := s.db.Exec(`INSERT INTO storage_targets(name,kind,config,created_at) VALUES(?,?,?,?)`, req.Name, req.Kind, s.box.Seal(string(conf)), time.Now().Unix())
	s.audit(r, u, "storage.create", req.Name, req.Kind, err)
	if err != nil {
		return err
	}
	nid, _ := res.LastInsertId()
	return ok(w, map[string]any{"id": nid})
}

func (s *Server) handleTestStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	var st backup.Storage
	var id int64
	if r.PathValue("id") != "" {
		var err error
		id, err = pathID(r, "id")
		if err != nil {
			return err
		}
		_, st, err = s.getStorage(id)
		if err != nil {
			return err
		}
	} else {
		var req storageReq
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if req.ID > 0 {
			s.mergeSavedStorageSecrets(req.ID, &req.Config)
		}
		var err error
		if st, err = backup.Open(req.Kind, req.Config); err != nil {
			return bad("%v", err)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	err := st.Test(ctx)
	msg := "连接正常"
	if err != nil {
		msg = err.Error()
	}
	if id > 0 {
		_, _ = s.db.Exec(`UPDATE storage_targets SET last_check_at=?, last_check_ok=?, last_check_msg=? WHERE id=?`, time.Now().Unix(), boolInt(err == nil), msg, id)
	}
	return ok(w, map[string]any{"ok": err == nil, "message": msg, "elapsedMs": time.Since(start).Milliseconds()})
}

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

func (s *Server) handleDeleteStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM backup_policies WHERE storage_id=?`, id).Scan(&n)
	if n > 0 {
		return bad("仍有 %d 个备份策略使用该存储", n)
	}
	_, err = s.db.Exec(`DELETE FROM storage_targets WHERE id=?`, id)
	s.audit(r, u, "storage.delete", fmt.Sprint(id), "", err)
	return done(w, err)
}

type extraAgeKey struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Identity  string `json:"identity,omitempty"`
	Recipient string `json:"recipient"`
	AddedAt   int64  `json:"addedAt"`
}

func (s *Server) loadExtraAgeKeys() []extraAgeKey {
	enc := s.setting("backup_age_extra_identities", "")
	if enc == "" {
		return nil
	}
	raw, err := s.box.Open(enc)
	if err != nil || raw == "" {
		return nil
	}
	var out []extraAgeKey
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

func (s *Server) saveExtraAgeKeys(keys []extraAgeKey) {
	if len(keys) == 0 {
		s.setSetting("backup_age_extra_identities", "")
		return
	}
	b, _ := json.Marshal(keys)
	s.setSetting("backup_age_extra_identities", s.box.Seal(string(b)))
}

// allAgeIdentities returns decryption identities in priority order:
// optional temporary key -> active primary panel key -> historical keyring.
func (s *Server) allAgeIdentities(tempKey string) ([]string, error) {
	var out []string
	if t := strings.TrimSpace(tempKey); t != "" {
		if _, err := age.ParseX25519Identity(t); err != nil {
			return nil, bad("临时 age 私钥格式无效：%v", err)
		}
		out = append(out, t)
	}
	if primary := strings.TrimSpace(s.box.MustOpen(s.setting("backup_age_identity", ""))); primary != "" {
		out = append(out, primary)
	}
	for _, k := range s.loadExtraAgeKeys() {
		if id := strings.TrimSpace(k.Identity); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *Server) handleBackupKey(w http.ResponseWriter, r *http.Request, u *User) error {
	extra := s.loadExtraAgeKeys()
	pubExtra := make([]extraAgeKey, 0, len(extra))
	for _, k := range extra {
		pubExtra = append(pubExtra, extraAgeKey{
			ID:        k.ID,
			Label:     k.Label,
			Recipient: k.Recipient,
			AddedAt:   k.AddedAt,
		})
	}
	out := map[string]any{
		"recipient": s.setting("backup_age_recipient", ""),
		"extraKeys": pubExtra,
	}
	if r.URL.Query().Get("reveal") == "1" {
		out["identity"] = s.box.MustOpen(s.setting("backup_age_identity", ""))
		s.audit(r, u, "backup.reveal_key", "", "查看备份私钥", nil)
	}
	return ok(w, out)
}

func (s *Server) handleImportBackupKey(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Label      string `json:"label"`
		Identity   string `json:"identity"`
		SetPrimary bool   `json:"setPrimary"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	rawID := strings.TrimSpace(req.Identity)
	parsed, err := age.ParseX25519Identity(rawID)
	if err != nil {
		return bad("age 私钥格式无效（应以 AGE-SECRET-KEY-1 开头）")
	}
	rcp := parsed.Recipient().String()
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "导入的私钥"
	}

	curRcp := strings.TrimSpace(s.setting("backup_age_recipient", ""))
	curID := strings.TrimSpace(s.box.MustOpen(s.setting("backup_age_identity", "")))
	extra := s.loadExtraAgeKeys()

	if req.SetPrimary {
		if rcp == curRcp {
			return bad("该私钥已是当前主密钥")
		}
		// Remove if already in historical list.
		kept := make([]extraAgeKey, 0, len(extra)+1)
		for _, k := range extra {
			if k.Recipient != rcp {
				kept = append(kept, k)
			}
		}
		// Preserve old primary in historical keyring so old backups remain decryptable.
		if curID != "" && curRcp != "" {
			exists := false
			for _, k := range kept {
				if k.Recipient == curRcp {
					exists = true
					break
				}
			}
			if !exists {
				kept = append(kept, extraAgeKey{
					ID:        auth.RandomToken(8),
					Label:     "原主密钥（自动归档）",
					Identity:  curID,
					Recipient: curRcp,
					AddedAt:   time.Now().Unix(),
				})
			}
		}
		s.saveExtraAgeKeys(kept)
		s.setSetting("backup_age_identity", s.box.Seal(rawID))
		s.setSetting("backup_age_recipient", rcp)
		s.audit(r, u, "backup.import_key", rcp, label+"（设为主密钥）", nil)
		return ok(w, map[string]any{"recipient": rcp, "primary": true})
	}

	if rcp == curRcp {
		return bad("该私钥已是当前主密钥，无需重复导入")
	}
	for _, k := range extra {
		if k.Recipient == rcp {
			return bad("该私钥已存在于历史解密密钥列表中")
		}
	}
	entry := extraAgeKey{
		ID:        auth.RandomToken(8),
		Label:     label,
		Identity:  rawID,
		Recipient: rcp,
		AddedAt:   time.Now().Unix(),
	}
	extra = append(extra, entry)
	s.saveExtraAgeKeys(extra)
	s.audit(r, u, "backup.import_key", rcp, label, nil)
	return ok(w, map[string]any{
		"id":        entry.ID,
		"label":     entry.Label,
		"recipient": entry.Recipient,
		"addedAt":   entry.AddedAt,
	})
}

func (s *Server) handleDeleteExtraBackupKey(w http.ResponseWriter, r *http.Request, u *User) error {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		return errNotFound
	}
	extra := s.loadExtraAgeKeys()
	kept := make([]extraAgeKey, 0, len(extra))
	var removed *extraAgeKey
	for i := range extra {
		if extra[i].ID == id {
			k := extra[i]
			removed = &k
			continue
		}
		kept = append(kept, extra[i])
	}
	if removed == nil {
		return errNotFound
	}
	s.saveExtraAgeKeys(kept)
	s.audit(r, u, "backup.delete_extra_key", removed.Recipient, removed.Label, nil)
	return ok(w, nil)
}
