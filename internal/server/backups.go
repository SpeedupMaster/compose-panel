package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"filippo.io/age"

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
		if err := s.deleteBackup(it.ID); err != nil {
			run.Logf("清理旧备份 %d 失败：%v", it.ID, err)
		} else {
			removed++
		}
	}
	// Failed attempts older than 30 days are only noise.
	_, _ = s.db.Exec(`DELETE FROM backups WHERE policy_id=? AND status='failed' AND started_at<?`, policyID, time.Now().Add(-30*24*time.Hour).Unix())
	run.Logf("保留策略：保留 %d 份，清理 %d 份", len(tiers), removed)
}

func (s *Server) deleteBackup(id int64) error {
	var sid int64
	var key string
	if err := s.db.QueryRow(`SELECT storage_id, object_key FROM backups WHERE id=?`, id).Scan(&sid, &key); err != nil {
		return err
	}
	if key != "" {
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
			_ = s.deleteBackup(b)
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
}

func (s *Server) listBackups(where string, args ...any) []backupRow {
	rows, err := s.db.Query(`SELECT b.id, COALESCE(b.policy_id,0), b.source, b.label, b.storage_id, COALESCE(t.name,''), b.object_key, b.size, b.sha256, b.encrypted, b.format, b.status, b.error, b.tiers, b.job_id, b.started_at, b.finished_at
		FROM backups b LEFT JOIN storage_targets t ON t.id=b.storage_id `+where, args...)
	if err != nil {
		return []backupRow{}
	}
	defer rows.Close()
	out := []backupRow{}
	for rows.Next() {
		var b backupRow
		var src string
		if rows.Scan(&b.ID, &b.PolicyID, &src, &b.Label, &b.StorageID, &b.StorageName, &b.ObjectKey, &b.Size, &b.SHA256, &b.Encrypted, &b.Format, &b.Status, &b.Error, &b.Tiers, &b.JobID, &b.StartedAt, &b.FinishedAt) == nil {
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
	err = s.deleteBackup(b.ID)
	s.audit(r, u, "backup.delete", b.Label, b.ObjectKey, err)
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
		rc, err = backup.OpenObject(r.Context(), st, b.ObjectKey, b.Encrypted, s.box.MustOpen(s.setting("backup_age_identity", "")))
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
	jobID := s.jobs.Submit(jobSpec("backup.verify", "校验备份 "+b.Label, b.Label, u.Username, ""), func(run *jobRun) error {
		_, st, err := s.getStorage(b.StorageID)
		if err != nil {
			return err
		}
		run.Logf("下载并计算 SHA-256：%s", b.ObjectKey)
		if err := backup.Verify(run.Context(), st, b.ObjectKey, b.SHA256, b.Size); err != nil {
			return err
		}
		run.Logf("校验和一致")
		rc, err := backup.OpenObject(run.Context(), st, b.ObjectKey, b.Encrypted, s.box.MustOpen(s.setting("backup_age_identity", "")))
		if err != nil {
			return err
		}
		n, err := io.Copy(io.Discard, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("解压/解密失败：%w", err)
		}
		run.Logf("解密并解压成功，原始大小 %s", humanBytes(n))
		return nil
	})
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	b, err := s.backupByID(r)
	if err != nil {
		return err
	}
	var req struct {
		Mode    string `json:"mode"`   // new | overwrite
		Target  string `json:"target"` // new database / volume / directory name
		Confirm string `json:"confirm"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	src := b.Source
	switch src.Kind {
	case "database":
		if req.Mode == "overwrite" {
			if req.Confirm != src.Database {
				return bad("请输入库名 %s 确认覆盖", src.Database)
			}
			req.Target = src.Database
		} else if !dbmgr.ValidIdent(req.Target) {
			return bad("新库名无效")
		}
	case "volume":
		req.Mode = "new"
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`).MatchString(req.Target) {
			return bad("新卷名无效")
		}
	case "dir":
		req.Mode = "new"
		req.Target = strings.Trim(filepath.ToSlash(filepath.Clean("/"+req.Target)), "/")
		if req.Target == "" || strings.Contains(req.Target, "..") {
			return bad("目标目录无效")
		}
	default:
		return bad("该类型备份请下载后手动恢复")
	}
	jobID := s.jobs.Submit(jobSpec("backup.restore", "恢复 "+b.Label+" → "+req.Target, b.Label, u.Username, "backup:"+b.Label), func(run *jobRun) error {
		return s.runRestore(run, b, req.Mode, req.Target, u.Username)
	})
	s.audit(r, u, "backup.restore", b.Label, req.Mode+" → "+req.Target, nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) runRestore(run *jobRun, b *backupRow, mode, target, actor string) error {
	ctx := run.Context()
	_, st, err := s.getStorage(b.StorageID)
	if err != nil {
		return err
	}
	run.Logf("==> 校验备份完整性")
	if err := backup.Verify(ctx, st, b.ObjectKey, b.SHA256, b.Size); err != nil {
		return err
	}
	run.SetProgress(20)
	src := b.Source
	identity := s.box.MustOpen(s.setting("backup_age_identity", ""))
	switch src.Kind {
	case "database":
		d, err := s.getDB(src.InstanceID)
		if err != nil {
			return err
		}
		cl := d.client()
		if mode == "overwrite" {
			run.Logf("==> 覆盖前先备份当前数据")
			if _, err := s.runBackup(run, backupRequest{Source: src, StorageID: b.StorageID, Encrypt: b.Encrypted, Actor: actor}); err != nil {
				return fmt.Errorf("覆盖前备份失败，已中止：%w", err)
			}
			if d.Engine == "mysql" || d.Engine == "mariadb" {
				if err := cl.DropDatabase(ctx, target); err != nil {
					return err
				}
				if err := cl.CreateDatabase(ctx, target, "", ""); err != nil {
					return err
				}
			}
		} else {
			run.Logf("==> 创建新库 %s", target)
			if d.Engine != "mongo" {
				if err := cl.CreateDatabase(ctx, target, "", ""); err != nil {
					return err
				}
			}
		}
		run.SetProgress(40)
		run.Logf("==> 导入数据")
		rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identity)
		if err != nil {
			return err
		}
		defer rc.Close()
		cmd, err := cl.RestoreCmd(ctx, src.Database, target, mode == "overwrite")
		if err != nil {
			return err
		}
		if err := dbmgr.RunWithInput(cmd, rc, run); err != nil {
			return fmt.Errorf("导入失败：%w", err)
		}
	case "volume":
		if vols, _ := s.dk.ListVolumes(ctx); vols != nil {
			for _, v := range vols {
				if v.Name == target {
					return fmt.Errorf("数据卷 %s 已存在，请换一个名字", target)
				}
			}
		}
		if err := s.dk.CreateVolume(ctx, target, map[string]string{"panel.restored-from": b.ObjectKey}); err != nil {
			return err
		}
		rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identity)
		if err != nil {
			return err
		}
		defer rc.Close()
		cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "-v", target+":/v", s.cfg.HelperImage, "tar", "-C", "/v", "-xf", "-")
		if err := dbmgr.RunWithInput(cmd, rc, run); err != nil {
			return err
		}
	case "dir":
		p, err := s.getProject(src.ProjectID)
		if err != nil {
			return err
		}
		dir := filepath.Join(s.projectDir(p), filepath.FromSlash(target))
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("目录 %s 已存在，请换一个名字", target)
		}
		rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identity)
		if err != nil {
			return err
		}
		defer rc.Close()
		if err := backup.UntarDir(rc, dir); err != nil {
			return err
		}
	}
	run.Logf("==> 恢复完成：%s", target)
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
	prow, _ := s.db.Query(`SELECT id, name FROM projects WHERE kind='app' ORDER BY name`)
	if prow != nil {
		for prow.Next() {
			var id int64
			var n string
			_ = prow.Scan(&id, &n)
			ps = append(ps, map[string]any{"id": id, "name": n})
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
	Name   string              `json:"name"`
	Kind   string              `json:"kind"`
	Config backup.TargetConfig `json:"config"`
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
	if id > 0 && (req.Config.SecretKey == "" || req.Config.SecretKey == "••••••") {
		if old, _, err := s.getStorage(id); err == nil && old != nil {
			req.Config.SecretKey = old.Config.SecretKey
		}
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

func (s *Server) handleBackupKey(w http.ResponseWriter, r *http.Request, u *User) error {
	out := map[string]any{"recipient": s.setting("backup_age_recipient", "")}
	if r.URL.Query().Get("reveal") == "1" {
		out["identity"] = s.box.MustOpen(s.setting("backup_age_identity", ""))
		s.audit(r, u, "backup.reveal_key", "", "查看备份私钥", nil)
	}
	return ok(w, out)
}
