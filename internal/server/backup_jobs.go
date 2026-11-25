package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"filippo.io/age"

	"compose-panel/internal/backup"
	"compose-panel/internal/dbmgr"
	"compose-panel/internal/docker"
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
		spec, format, err := d.client().DumpCmd(src.Database)
		if err != nil {
			return nil, "", err
		}
		run.Logf("$ docker exec %s %s ...", d.Container, spec.Args[0])
		rc, err := s.dk.ExecStdout(ctx, spec.Container, docker.ExecOptions{Cmd: spec.Args, Env: spec.Env})
		return rc, format, err
	case "volume":
		run.Logf("$ docker run --rm -v %s:/v:ro %s tar -C /v -cf - .", src.Volume, s.cfg.HelperImage)
		rc, err := s.dk.RunContainerStdout(ctx, docker.RunOptions{
			Image: s.cfg.HelperImage,
			Binds: []string{src.Volume + ":/v:ro"},
			Cmd:   []string{"tar", "-C", "/v", "-cf", "-", "."},
		})
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

// ---------- backup RBAC & source discovery ----------

func (s *Server) allowedProjectNames(u *User) map[string]bool {
	if u.can(roleAdmin) || u.Projects == nil {
		return nil
	}
	out := map[string]bool{}
	rows, err := s.db.Query(`SELECT id, name FROM projects`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if rows.Scan(&id, &name) == nil && u.canProject(id) {
			out[name] = true
		}
	}
	return out
}

func (s *Server) volumeProjectMap(ctx context.Context) map[string]string {
	out := map[string]string{}
	if s.dk == nil || ctx == nil {
		return out
	}
	vols, err := s.dk.ListVolumes(ctx)
	if err != nil {
		return out
	}
	for _, v := range vols {
		if p := v.Labels["com.docker.compose.project"]; p != "" {
			out[v.Name] = p
		}
	}
	return out
}

func (s *Server) canAccessBackupSource(ctx context.Context, u *User, src BackupSource, volProjects *map[string]string, allowedNames map[string]bool) bool {
	if u.can(roleAdmin) || u.Projects == nil {
		return true
	}
	switch src.Kind {
	case "dir":
		return src.ProjectID > 0 && u.canProject(src.ProjectID)
	case "database":
		if src.InstanceID <= 0 {
			return false
		}
		var pid int64
		if err := s.db.QueryRow(`SELECT project_id FROM db_instances WHERE id=?`, src.InstanceID).Scan(&pid); err != nil {
			return false
		}
		return pid > 0 && u.canProject(pid)
	case "volume":
		if src.Volume == "" {
			return false
		}
		var vp map[string]string
		if volProjects != nil {
			if *volProjects == nil {
				*volProjects = s.volumeProjectMap(ctx)
			}
			vp = *volProjects
		} else {
			vp = s.volumeProjectMap(ctx)
		}
		if allowedNames == nil {
			allowedNames = s.allowedProjectNames(u)
		}
		proj := vp[src.Volume]
		return proj != "" && allowedNames[proj]
	case "panel":
		return false
	default:
		return false
	}
}

// handleBackupSources lists what can be backed up.
func (s *Server) handleBackupSources(w http.ResponseWriter, r *http.Request, u *User) error {
	out := map[string]any{}
	restricted := !u.can(roleAdmin) && u.Projects != nil
	var allowedNames map[string]bool
	if restricted {
		allowedNames = s.allowedProjectNames(u)
	}
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
		if err != nil || !u.canProject(d.ProjectID) {
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
		proj := v.Labels["com.docker.compose.project"]
		if restricted && (proj == "" || !allowedNames[proj]) {
			continue
		}
		vn = append(vn, map[string]any{"name": v.Name, "project": proj})
	}
	out["volumes"] = vn
	ps := []map[string]any{}
	prow, _ := s.db.Query(`SELECT ` + projectCols + ` FROM projects WHERE kind='app' ORDER BY name`)
	if prow != nil {
		for prow.Next() {
			p, err := s.scanProject(prow)
			if err != nil || !u.canProject(p.ID) {
				continue
			}
			ps = append(ps, map[string]any{"id": p.ID, "name": p.Name, "dirs": s.detectProjectDirs(p)})
		}
		prow.Close()
	}
	out["projects"] = ps
	return ok(w, out)
}
