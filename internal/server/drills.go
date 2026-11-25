package server

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"compose-panel/internal/auth"
	"compose-panel/internal/backup"
	"compose-panel/internal/dbmgr"
	"compose-panel/internal/docker"
	"compose-panel/internal/notify"
)

// Restore drills periodically restore the latest backup of a policy into a
// throwaway container / volume, run a check, record a report and clean up.

type restoreDrill struct {
	ID        int64     `json:"id"`
	PolicyID  int64     `json:"policyId"`
	Policy    string    `json:"policy"`
	Label     string    `json:"label"`
	Kind      string    `json:"kind"`
	Schedule  string    `json:"schedule"`
	CheckSQL  string    `json:"checkSql"`
	MinRows   int64     `json:"minRows"`
	Enabled   bool      `json:"enabled"`
	CreatedAt int64     `json:"createdAt"`
	Next      int64     `json:"next"`
	LastRun   *drillRun `json:"lastRun"`
}

type drillRun struct {
	ID         int64  `json:"id"`
	DrillID    int64  `json:"drillId"`
	BackupID   int64  `json:"backupId"`
	Label      string `json:"label"`
	Status     string `json:"status"`
	Report     string `json:"report"`
	JobID      string `json:"jobId"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
}

func (s *Server) listDrills() []restoreDrill {
	rows, err := s.db.Query(`SELECT d.id, d.policy_id, p.name, p.source, d.schedule, d.check_sql, d.min_rows, d.enabled, d.created_at
		FROM restore_drills d JOIN backup_policies p ON p.id=d.policy_id ORDER BY d.id`)
	out := []restoreDrill{}
	if err != nil {
		return out
	}
	for rows.Next() {
		var d restoreDrill
		var src string
		var en int
		if rows.Scan(&d.ID, &d.PolicyID, &d.Policy, &src, &d.Schedule, &d.CheckSQL, &d.MinRows, &en, &d.CreatedAt) != nil {
			continue
		}
		d.Enabled = en == 1
		out = append(out, d)
	}
	rows.Close()
	for i := range out {
		if p, err := s.getPolicy(out[i].PolicyID); err == nil {
			out[i].Label, out[i].Kind = p.Label, p.Source.Kind
		}
	}
	return out
}

func (s *Server) getDrill(id int64) (*restoreDrill, error) {
	for _, d := range s.listDrills() {
		if d.ID == id {
			return &d, nil
		}
	}
	return nil, errNotFound
}

func (s *Server) drillRuns(where string, args ...any) []drillRun {
	rows, err := s.db.Query(`SELECT id, COALESCE(drill_id,0), backup_id, label, status, report, job_id, started_at, finished_at FROM drill_runs `+where, args...)
	out := []drillRun{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r drillRun
		if rows.Scan(&r.ID, &r.DrillID, &r.BackupID, &r.Label, &r.Status, &r.Report, &r.JobID, &r.StartedAt, &r.FinishedAt) == nil {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) addDrillSchedules(add func(string, scheduleMeta, func())) {
	for _, d := range s.listDrills() {
		if !d.Enabled {
			continue
		}
		id := d.ID
		add(fmt.Sprintf("drill:%d", id), scheduleMeta{Kind: "drill", Name: "恢复演练：" + d.Policy, Spec: d.Schedule, Target: d.Label},
			func() { s.runDrill(id, "scheduler") })
	}
}

func (s *Server) handleListDrills(w http.ResponseWriter, r *http.Request, u *User) error {
	ds := s.listDrills()
	restricted := !u.can(roleAdmin) && u.Projects != nil
	var allowedDrillIDs map[int64]bool
	var allowedNames map[string]bool
	var volProjects map[string]string
	if restricted {
		allowedNames = s.allowedProjectNames(u)
		allowedDrillIDs = map[int64]bool{}
		filtered := make([]restoreDrill, 0, len(ds))
		for _, d := range ds {
			p, err := s.getPolicy(d.PolicyID)
			if err != nil {
				continue
			}
			if s.canAccessBackupSource(r.Context(), u, p.Source, &volProjects, allowedNames) {
				allowedDrillIDs[d.ID] = true
				filtered = append(filtered, d)
			}
		}
		ds = filtered
	}
	for i := range ds {
		ds[i].Next = s.nextRun(fmt.Sprintf("drill:%d", ds[i].ID))
		if runs := s.drillRuns(`WHERE drill_id=? ORDER BY started_at DESC LIMIT 1`, ds[i].ID); len(runs) > 0 {
			ds[i].LastRun = &runs[0]
		}
	}
	runs := s.drillRuns(`ORDER BY started_at DESC LIMIT 50`)
	if restricted {
		filteredRuns := make([]drillRun, 0, len(runs))
		for _, run := range runs {
			if run.DrillID > 0 {
				if allowedDrillIDs[run.DrillID] {
					filteredRuns = append(filteredRuns, run)
				}
				continue
			}
			if run.BackupID > 0 {
				if bs := s.listBackups(`WHERE b.id=?`, run.BackupID); len(bs) > 0 && s.canAccessBackupSource(r.Context(), u, bs[0].Source, &volProjects, allowedNames) {
					filteredRuns = append(filteredRuns, run)
				}
			}
		}
		runs = filteredRuns
	}
	return ok(w, map[string]any{"drills": ds, "runs": runs})
}

func (s *Server) handleSaveDrill(w http.ResponseWriter, r *http.Request, u *User) error {
	if !u.can(roleAdmin) {
		return errForbidden
	}
	var req restoreDrill
	if err := readJSON(r, &req); err != nil {
		return err
	}
	p, err := s.getPolicy(req.PolicyID)
	if err != nil {
		return bad("请选择备份策略")
	}
	if _, err := cronParser.Parse(req.Schedule); err != nil {
		return bad("Cron 表达式无效：%v", err)
	}
	if req.MinRows < 0 {
		req.MinRows = 0
	}
	if r.PathValue("id") != "" {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		_, err = s.db.Exec(`UPDATE restore_drills SET policy_id=?, schedule=?, check_sql=?, min_rows=?, enabled=? WHERE id=?`,
			req.PolicyID, req.Schedule, req.CheckSQL, req.MinRows, boolInt(req.Enabled), id)
		s.audit(r, u, "drill.update", p.Name, req.Schedule, err)
		if err != nil {
			return err
		}
		req.ID = id
	} else {
		res, err := s.db.Exec(`INSERT INTO restore_drills(policy_id,schedule,check_sql,min_rows,enabled,created_at) VALUES(?,?,?,?,?,?)`,
			req.PolicyID, req.Schedule, req.CheckSQL, req.MinRows, boolInt(req.Enabled), time.Now().Unix())
		s.audit(r, u, "drill.create", p.Name, req.Schedule, err)
		if err != nil {
			return err
		}
		req.ID, _ = res.LastInsertId()
	}
	s.reloadSchedules()
	return ok(w, map[string]any{"id": req.ID})
}

func (s *Server) handleDeleteDrill(w http.ResponseWriter, r *http.Request, u *User) error {
	if !u.can(roleAdmin) {
		return errForbidden
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	d, err := s.getDrill(id)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM restore_drills WHERE id=?`, id)
	s.audit(r, u, "drill.delete", d.Policy, "", err)
	s.reloadSchedules()
	return done(w, err)
}

func (s *Server) handleRunDrill(w http.ResponseWriter, r *http.Request, u *User) error {
	if !u.can(roleAdmin) {
		return errForbidden
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	d, err := s.getDrill(id)
	if err != nil {
		return err
	}
	jobID := s.runDrill(id, u.Username)
	s.audit(r, u, "drill.run", d.Policy, "手动执行", nil)
	return ok(w, map[string]any{"jobId": jobID})
}

// runDrill starts the drill job and returns its id.
func (s *Server) runDrill(id int64, actor string) string {
	d, err := s.getDrill(id)
	if err != nil {
		return ""
	}
	return s.jobs.Submit(jobSpec("backup.drill", "恢复演练："+d.Policy, d.Label, actor, fmt.Sprintf("drill:%d", id)), func(run *jobRun) error {
		bs := s.listBackups(`WHERE b.policy_id=? AND b.status='success' ORDER BY b.started_at DESC LIMIT 1`, d.PolicyID)
		if len(bs) == 0 {
			err := fmt.Errorf("策略 %s 还没有成功的备份", d.Policy)
			s.notify.Emit(notify.Event{Type: "restore_drill", Title: "恢复演练失败：" + d.Policy, Body: err.Error(), Target: fmt.Sprintf("drill:%d", id)})
			return err
		}
		b := bs[0]
		start := time.Now()
		res, _ := s.db.Exec(`INSERT INTO drill_runs(drill_id,backup_id,label,status,job_id,started_at) VALUES(?,?,?,?,?,?)`,
			id, b.ID, b.Label, "running", run.ID, start.Unix())
		runID, _ := res.LastInsertId()
		rep := &drillReport{run: run}
		rep.add("备份：%s（%s，%s，%s）", b.Label, time.Unix(b.StartedAt, 0).In(s.location()).Format("2006-01-02 15:04"), humanBytes(b.Size), b.StorageName)
		err := s.drill(run, d, &b, rep)
		status := "success"
		if err != nil {
			status = "failed"
			rep.add("失败：%v", err)
		} else {
			_, _ = s.db.Exec(`UPDATE backups SET verified_at=? WHERE id=?`, time.Now().Unix(), b.ID)
		}
		rep.add("总用时 %s", time.Since(start).Round(time.Second))
		_, _ = s.db.Exec(`UPDATE drill_runs SET status=?, report=?, finished_at=? WHERE id=?`, status, rep.String(), time.Now().Unix(), runID)
		if err != nil {
			s.notify.Emit(notify.Event{Type: "restore_drill", Title: "恢复演练失败：" + d.Policy, Body: rep.String(), Target: fmt.Sprintf("drill:%d", id)})
		}
		return err
	})
}

type drillReport struct {
	run   *jobRun
	lines []string
}

func (r *drillReport) add(format string, a ...any) {
	l := fmt.Sprintf(format, a...)
	r.lines = append(r.lines, l)
	r.run.Logf("[演练] %s", l)
}

func (r *drillReport) String() string { return strings.Join(r.lines, "\n") }

func (s *Server) drill(run *jobRun, d *restoreDrill, b *backupRow, rep *drillReport) error {
	ctx := run.Context()
	_, st, err := s.getStorage(b.StorageID)
	if err != nil {
		return err
	}
	t := time.Now()
	if err := backup.Verify(ctx, st, b.ObjectKey, b.SHA256, b.Size); err != nil {
		return fmt.Errorf("校验和不一致：%w", err)
	}
	rep.add("校验和一致（%s）", time.Since(t).Round(time.Millisecond))
	run.SetProgress(20)
	identities, err := s.allAgeIdentities("")
	if err != nil {
		return err
	}
	open := func() (io.ReadCloser, error) {
		return backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
	}
	tag := fmt.Sprintf("panel-drill-%d-%d", d.ID, time.Now().Unix())
	switch b.Source.Kind {
	case "database":
		return s.drillDatabase(run, d, b, rep, open, tag)
	case "volume":
		return s.drillVolume(run, d, rep, open, tag)
	case "dir":
		return s.drillDir(run, d, rep, open, tag)
	case "panel":
		return s.drillPanel(run, rep, open, tag)
	}
	return fmt.Errorf("不支持的备份类型 %s", b.Source.Kind)
}

func checkMin(rep *drillReport, what string, got, min int64) error {
	rep.add("检查结果：%s = %d（要求 ≥ %d）", what, got, min)
	if got < min {
		return fmt.Errorf("检查未通过：%s = %d，低于要求的 %d", what, got, min)
	}
	return nil
}

func (s *Server) drillDatabase(run *jobRun, d *restoreDrill, b *backupRow, rep *drillReport, open func() (io.ReadCloser, error), tag string) error {
	ctx := run.Context()
	inst, err := s.getDB(b.Source.InstanceID)
	if err != nil {
		return fmt.Errorf("数据库实例不存在：%w", err)
	}
	image := ""
	if ci, err := s.dk.InspectContainer(ctx, inst.Container); err == nil {
		image = ci.Config.Image
	}
	if image == "" {
		return fmt.Errorf("无法确定 %s 的镜像", inst.Container)
	}
	pw := auth.RandomPassword(20)
	db := b.Source.Database
	if inst.Engine == "redis" {
		return s.drillRedis(run, d, rep, open, image, tag)
	}
	var env []string
	switch inst.Engine {
	case "postgres":
		env = []string{"POSTGRES_PASSWORD=" + pw}
	case "mysql":
		env = []string{"MYSQL_ROOT_PASSWORD=" + pw}
	case "mariadb":
		env = []string{"MARIADB_ROOT_PASSWORD=" + pw}
	case "mongo":
		env = []string{"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=" + pw}
	}
	rep.add("启动临时容器 %s（%s）", tag, image)
	if _, err := s.dk.StartDetached(ctx, docker.RunOptions{
		Name:   tag,
		Image:  image,
		Env:    env,
		Labels: map[string]string{"panel.drill": "1", "panel.monitor": "false"},
	}); err != nil {
		return fmt.Errorf("启动临时容器失败：%w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = s.dk.RemoveContainerWithVolumes(cctx, tag, true, true)
		rep.add("已销毁临时容器")
	}()
	cl := dbmgr.Client{Docker: s.dk, Engine: inst.Engine, Container: tag, Password: pw}
	if err := waitReady(ctx, cl, pw); err != nil {
		return err
	}
	run.SetProgress(40)
	if inst.Engine != "mongo" {
		if err := cl.CreateDatabase(ctx, db, "", ""); err != nil {
			return fmt.Errorf("创建库失败：%w", err)
		}
	}
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()
	spec, err := cl.RestoreCmd(db, db, false)
	if err != nil {
		return err
	}
	t := time.Now()
	if err := dbmgr.RunWithInput(ctx, s.dk, spec, rc, io.Discard); err != nil {
		return fmt.Errorf("恢复失败：%w", err)
	}
	rep.add("恢复完成（%s）", time.Since(t).Round(time.Second))
	run.SetProgress(80)
	// Check
	switch inst.Engine {
	case "mongo":
		expr := strings.TrimSpace(d.CheckSQL)
		what := expr
		if expr == "" {
			expr, what = "db.getCollectionNames().length", "集合数量"
		}
		out, err := dbmgr.Exec{Docker: s.dk, Container: tag}.Output(ctx, "", "mongosh", "--quiet", "-u", "root", "-p", pw, "--authenticationDatabase", "admin", db, "--eval", expr)
		if err != nil {
			return fmt.Errorf("检查查询失败：%w", err)
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		return checkMin(rep, what, n, d.MinRows)
	default:
		q := strings.TrimSpace(d.CheckSQL)
		what := q
		if q == "" {
			what = "表数量"
			if inst.Engine == "postgres" {
				q = `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`
			} else {
				q = `SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE()`
			}
		}
		res, err := cl.Query(ctx, db, q, true)
		if err != nil {
			return fmt.Errorf("检查查询失败：%w", err)
		}
		if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
			return fmt.Errorf("检查查询没有返回结果")
		}
		n, err := strconv.ParseInt(strings.TrimSpace(res.Rows[0][0]), 10, 64)
		if err != nil {
			return fmt.Errorf("检查查询的第一列不是数字：%q", res.Rows[0][0])
		}
		return checkMin(rep, truncate(what, 120), n, d.MinRows)
	}
}

// waitReady waits until the temporary server accepts TCP connections on
// localhost (entrypoint init phases only listen on the unix socket) and
// stays up for a few seconds.
func waitReady(ctx context.Context, cl dbmgr.Client, pw string) error {
	var probe []string
	ex := dbmgr.Exec{Docker: cl.Docker, Container: cl.Container}
	switch cl.Engine {
	case "postgres":
		probe = []string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres"}
	case "mysql", "mariadb":
		ex.Env = []string{"MYSQL_PWD=" + pw}
		probe = []string{"sh", "-c", "mysqladmin ping -h127.0.0.1 -uroot --silent 2>/dev/null || mariadb-admin ping -h127.0.0.1 -uroot --silent"}
	case "mongo":
		probe = []string{"mongosh", "--quiet", "--host", "127.0.0.1", "-u", "root", "-p", pw, "--authenticationDatabase", "admin", "--eval", "db.runCommand({ping:1}).ok"}
	}
	deadline := time.Now().Add(4 * time.Minute)
	okCount := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := ex.Output(pctx, "", probe...)
		cancel()
		if err == nil {
			okCount++
			if okCount >= 3 {
				return nil
			}
		} else {
			okCount = 0
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("临时数据库在 4 分钟内没有就绪")
}

func (s *Server) drillRedis(run *jobRun, d *restoreDrill, rep *drillReport, open func() (io.ReadCloser, error), image, tag string) error {
	ctx := run.Context()
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = s.dk.RemoveContainerWithVolumes(cctx, tag, true, true)
		rep.add("已销毁临时容器")
	}()
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()
	script := `set -e
mkdir -p /tmp/drill && cat > /tmp/drill/dump.rdb
redis-server --dir /tmp/drill --dbfilename dump.rdb --appendonly no --save "" --port 6399 --daemonize yes --logfile /tmp/drill/redis.log
i=0; until redis-cli -p 6399 ping 2>/dev/null | grep -q PONG; do i=$((i+1)); [ $i -gt 600 ] && { cat /tmp/drill/redis.log; exit 1; }; sleep 0.5; done
while redis-cli -p 6399 info persistence | grep -q '^loading:1'; do sleep 0.5; done
redis-cli -p 6399 info keyspace | awk -F'[=,]' '/^db[0-9]+:keys=/{s+=$2} END{print s+0}'`
	rep.add("在临时 Redis（%s）中加载 RDB", image)
	var out, stderr bytes.Buffer
	err = s.dk.RunContainer(ctx, docker.RunOptions{
		Name:       tag,
		Image:      image,
		Entrypoint: []string{"sh"},
		Labels:     map[string]string{"panel.drill": "1", "panel.monitor": "false"},
		Cmd:        []string{"-c", script},
		Stdin:      rc,
		Stdout:     &out,
		Stderr:     &stderr,
	})
	if err != nil {
		msg := strings.TrimSpace(stderr.String() + out.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("加载失败：%s", msg)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	n, _ := strconv.ParseInt(strings.TrimSpace(lines[len(lines)-1]), 10, 64)
	return checkMin(rep, "键数量", n, d.MinRows)
}

func (s *Server) drillVolume(run *jobRun, d *restoreDrill, rep *drillReport, open func() (io.ReadCloser, error), tag string) error {
	ctx := run.Context()
	if err := s.dk.CreateVolume(ctx, tag, map[string]string{"panel.drill": "1"}); err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = s.dk.RemoveVolume(cctx, tag)
		rep.add("已删除临时数据卷")
	}()
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()
	t := time.Now()
	err = s.dk.RunContainer(ctx, docker.RunOptions{
		Image:  s.cfg.HelperImage,
		Labels: map[string]string{"panel.drill": "1", "panel.monitor": "false"},
		Binds:  []string{tag + ":/v"},
		Cmd:    []string{"tar", "-C", "/v", "-xf", "-"},
		Stdin:  rc,
	})
	if err != nil {
		return fmt.Errorf("解包失败：%w", err)
	}
	rep.add("解包到临时数据卷 %s（%s）", tag, time.Since(t).Round(time.Second))
	var out bytes.Buffer
	err = s.dk.RunContainer(ctx, docker.RunOptions{
		Image:  s.cfg.HelperImage,
		Labels: map[string]string{"panel.drill": "1", "panel.monitor": "false"},
		Binds:  []string{tag + ":/v:ro"},
		Cmd:    []string{"sh", "-c", "find /v -type f | wc -l; du -sk /v | cut -f1"},
		Stdout: &out,
	})
	if err != nil {
		return err
	}
	f := strings.Fields(out.String())
	var files, kb int64
	if len(f) >= 2 {
		files, _ = strconv.ParseInt(f[0], 10, 64)
		kb, _ = strconv.ParseInt(f[1], 10, 64)
	}
	rep.add("共 %d 个文件，%s", files, humanBytes(kb*1024))
	return checkMin(rep, "文件数", files, d.MinRows)
}

func (s *Server) drillDir(run *jobRun, d *restoreDrill, rep *drillReport, open func() (io.ReadCloser, error), tag string) error {
	dir := filepath.Join(s.cfg.DataDir, "tmp", tag)
	defer os.RemoveAll(dir)
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := backup.UntarDir(rc, dir); err != nil {
		return fmt.Errorf("解包失败：%w", err)
	}
	var files, size int64
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			files++
			size += fi.Size()
		}
		return nil
	})
	rep.add("解包到临时目录：%d 个文件，%s", files, humanBytes(size))
	return checkMin(rep, "文件数", files, d.MinRows)
}

func (s *Server) drillPanel(run *jobRun, rep *drillReport, open func() (io.ReadCloser, error), tag string) error {
	dir := filepath.Join(s.cfg.DataDir, "tmp")
	_ = os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, tag+".db")
	defer os.Remove(p)
	rc, err := open()
	if err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		rc.Close()
		return err
	}
	_, err = io.Copy(f, rc)
	rc.Close()
	f.Close()
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	var projects int64
	_ = db.QueryRow(`SELECT count(*) FROM projects`).Scan(&projects)
	rep.add("SQLite 完整性检查：%s，项目数 %d", res, projects)
	if res != "ok" {
		return fmt.Errorf("完整性检查失败：%s", res)
	}
	return nil
}
