package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"compose-panel/internal/docker"
	"compose-panel/internal/notify"
)

// ---------- extra scheduler entries ----------

func (s *Server) addExtraSchedules(add func(string, scheduleMeta, func())) {
	if spec := s.setting("image_check_cron", ""); spec != "" {
		add("system:image-check", scheduleMeta{Kind: "system", Name: "镜像更新检查", Spec: spec, Target: "images"}, func() { s.runImageChecks("scheduler") })
	}
	for _, t := range s.listCronTasks(nil) {
		if !t.Enabled {
			continue
		}
		id := t.ID
		add(fmt.Sprintf("cron:%d", id), scheduleMeta{Kind: "cron", Name: "定时任务：" + t.Name, Spec: t.Schedule, Target: t.ProjectName + "/" + t.Service},
			func() { s.runCronTask(id, "scheduler") })
	}
	s.addDrillSchedules(add)
}

func (s *Server) runExtraSchedule(key, actor string) (string, bool) {
	switch {
	case key == "system:image-check":
		return s.runImageChecks(actor), true
	case strings.HasPrefix(key, "cron:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "cron:"), 10, 64)
		return s.runCronTask(id, actor), true
	case strings.HasPrefix(key, "drill:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "drill:"), 10, 64)
		return s.runDrill(id, actor), true
	}
	return "", false
}

// ---------- image update detection ----------

type ImageCheck struct {
	ProjectID    int64  `json:"projectId"`
	Project      string `json:"project,omitempty"`
	Service      string `json:"service"`
	Image        string `json:"image"`
	LocalDigest  string `json:"localDigest"`
	RemoteDigest string `json:"remoteDigest"`
	Status       string `json:"status"` // current | update | local | pinned | error
	Error        string `json:"error"`
	CheckedAt    int64  `json:"checkedAt"`
}

func registryHost(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	first, _, found := strings.Cut(name, "/")
	if found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "docker.io"
}

func normRegistryHost(h string) string {
	h = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://"), "/")
	h = strings.TrimSuffix(h, "/v1")
	switch h {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return "docker.io"
	}
	return h
}

func (s *Server) registryAuth(host string) *docker.RegistryAuth {
	rows, err := s.db.Query(`SELECT host, username, password FROM registries`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var h, user, enc string
		if rows.Scan(&h, &user, &enc) == nil && normRegistryHost(h) == host {
			return &docker.RegistryAuth{Username: user, Password: s.box.MustOpen(enc), ServerAddress: h}
		}
	}
	return nil
}

type digestCache map[string]struct {
	digest string
	err    error
}

// checkProjectImages compares running image digests with the registry and stores the result.
// It returns the services that newly have an update available.
func (s *Server) checkProjectImages(ctx context.Context, p *Project, cache digestCache) ([]ImageCheck, []string, error) {
	cs, err := s.dk.ProjectContainers(ctx, p.Name)
	if err != nil {
		return nil, nil, err
	}
	old := map[string]ImageCheck{}
	for _, c := range s.imageChecks(p.ID) {
		old[c.Service] = c
	}
	seen := map[string]bool{}
	var out []ImageCheck
	var fresh []string
	now := time.Now().Unix()
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name() < cs[j].Name() })
	for _, c := range cs {
		svc := c.Labels["com.docker.compose.service"]
		if svc == "" || seen[svc] || c.Labels["com.docker.compose.oneoff"] == "True" {
			continue
		}
		seen[svc] = true
		ic := ImageCheck{ProjectID: p.ID, Service: svc, Image: c.Image, CheckedAt: now}
		if strings.HasPrefix(ic.Image, "sha256:") {
			if ci, err := s.dk.InspectContainer(ctx, c.Id); err == nil {
				ic.Image = ci.Config.Image
			}
		}
		img, err := s.dk.InspectImage(ctx, c.ImageID)
		switch {
		case err != nil:
			ic.Status, ic.Error = "error", err.Error()
		case strings.Contains(ic.Image, "@sha256:"):
			ic.Status = "pinned"
			_, ic.LocalDigest, _ = strings.Cut(ic.Image, "@")
		case len(img.RepoDigests) == 0:
			ic.Status = "local" // built locally or loaded from a tarball
		default:
			_, ic.LocalDigest, _ = strings.Cut(img.RepoDigests[0], "@")
			res, okc := cache[ic.Image]
			if !okc {
				dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				res.digest, res.err = s.dk.DistributionDigest(dctx, ic.Image, s.registryAuth(registryHost(ic.Image)))
				cancel()
				cache[ic.Image] = res
			}
			if res.err != nil {
				ic.Status, ic.Error = "error", res.err.Error()
				break
			}
			ic.RemoteDigest = res.digest
			ic.Status = "update"
			for _, d := range img.RepoDigests {
				if strings.HasSuffix(d, "@"+res.digest) {
					ic.Status, ic.LocalDigest = "current", res.digest
				}
			}
			if ic.Status == "update" {
				if o, had := old[svc]; !had || o.Status != "update" || o.RemoteDigest != ic.RemoteDigest {
					fresh = append(fresh, svc)
				}
			}
		}
		out = append(out, ic)
		_, _ = s.db.Exec(`INSERT INTO image_checks(project_id,service,image,local_digest,remote_digest,status,error,checked_at) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(project_id,service) DO UPDATE SET image=excluded.image, local_digest=excluded.local_digest, remote_digest=excluded.remote_digest,
			status=excluded.status, error=excluded.error, checked_at=excluded.checked_at`,
			ic.ProjectID, ic.Service, ic.Image, ic.LocalDigest, ic.RemoteDigest, ic.Status, ic.Error, ic.CheckedAt)
	}
	for svc := range old {
		if !seen[svc] {
			_, _ = s.db.Exec(`DELETE FROM image_checks WHERE project_id=? AND service=?`, p.ID, svc)
		}
	}
	return out, fresh, nil
}

// settleAppliedUpdates flips stored "update" rows to "current" once the service
// runs the remote digest that was found, so an update applied by a deploy, a
// manual `docker compose pull`, or the panel replacing itself stops being
// reported. It only asks the local Docker daemon, never the registry.
func (s *Server) settleAppliedUpdates(ctx context.Context, checks []ImageCheck) {
	running := map[string]map[string]string{} // project -> service -> image ID
	for i := range checks {
		c := &checks[i]
		if c.Status != "update" || c.RemoteDigest == "" {
			continue
		}
		svcs, cached := running[c.Project]
		if !cached {
			svcs = map[string]string{}
			if cs, err := s.dk.ProjectContainers(ctx, c.Project); err == nil {
				sort.Slice(cs, func(i, j int) bool { return cs[i].Name() < cs[j].Name() })
				for _, ct := range cs {
					svc := ct.Labels["com.docker.compose.service"]
					if _, dup := svcs[svc]; svc != "" && !dup && ct.Labels["com.docker.compose.oneoff"] != "True" {
						svcs[svc] = ct.ImageID
					}
				}
			}
			running[c.Project] = svcs
		}
		id := svcs[c.Service]
		if id == "" {
			continue
		}
		img, err := s.dk.InspectImage(ctx, id)
		if err != nil {
			continue
		}
		for _, d := range img.RepoDigests {
			if strings.HasSuffix(d, "@"+c.RemoteDigest) {
				c.Status, c.LocalDigest = "current", c.RemoteDigest
				_, _ = s.db.Exec(`UPDATE image_checks SET status='current', local_digest=? WHERE project_id=? AND service=? AND status='update'`,
					c.RemoteDigest, c.ProjectID, c.Service)
				break
			}
		}
	}
}

// recheckAfterDeploy refreshes a project's image status against the registry
// in the background, so a deploy that pulled new images clears its notice.
func (s *Server) recheckAfterDeploy(projectID int64) {
	s.goBackground(func(bgCtx context.Context) {
		p, err := s.getProject(projectID)
		if err != nil || bgCtx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(bgCtx, 2*time.Minute)
		defer cancel()
		_, _, _ = s.checkProjectImages(ctx, p, digestCache{})
	})
}

func (s *Server) imageChecks(projectID int64) []ImageCheck {
	q := `SELECT c.project_id, p.name, c.service, c.image, c.local_digest, c.remote_digest, c.status, c.error, c.checked_at
		FROM image_checks c JOIN projects p ON p.id=c.project_id`
	var rows *sql.Rows
	var err error
	if projectID > 0 {
		rows, err = s.db.Query(q+` WHERE c.project_id=? ORDER BY c.service`, projectID)
	} else {
		rows, err = s.db.Query(q + ` WHERE c.status='update' ORDER BY p.name, c.service`)
	}
	out := []ImageCheck{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c ImageCheck
		if rows.Scan(&c.ProjectID, &c.Project, &c.Service, &c.Image, &c.LocalDigest, &c.RemoteDigest, &c.Status, &c.Error, &c.CheckedAt) == nil {
			out = append(out, c)
		}
	}
	return out
}

// runImageChecks checks every project, notifies about new versions and
// redeploys projects that opted into automatic updates.
func (s *Server) runImageChecks(actor string) string {
	return s.jobs.Submit(jobSpec("images.check", "镜像更新检查", "images", actor, "images:check"), func(run *jobRun) error {
		ctx := run.Context()
		rows, err := s.db.Query(`SELECT id FROM projects ORDER BY name`)
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
		cache := digestCache{}
		updates := 0
		for i, id := range ids {
			run.SetProgress(i * 100 / max(len(ids), 1))
			p, err := s.getProject(id)
			if err != nil {
				continue
			}
			checks, fresh, err := s.checkProjectImages(ctx, p, cache)
			if err != nil {
				run.Logf("%s：检查失败：%v", p.Name, err)
				continue
			}
			var pending []string
			for _, c := range checks {
				switch c.Status {
				case "update":
					pending = append(pending, c.Service)
					run.Logf("%s/%s：%s 有新版本", p.Name, c.Service, c.Image)
				case "error":
					run.Logf("%s/%s：%s 检查失败：%s", p.Name, c.Service, c.Image, c.Error)
				}
			}
			updates += len(pending)
			if len(fresh) > 0 {
				body := "服务 " + strings.Join(fresh, "、") + " 的镜像有新版本。"
				if p.Options.AutoUpdate {
					body += "已开启自动更新，正在重新部署。"
				}
				s.notify.Emit(notify.Event{Type: "image_update", Title: "镜像有新版本：" + p.Name, Body: body, Target: p.Name, Link: fmt.Sprintf("/projects/%d", p.ID)})
			}
			if len(pending) > 0 && p.Options.AutoUpdate {
				jobID, _ := s.startDeploy(p, "image_update", "system", 0)
				run.Logf("%s：已开启自动更新，提交部署任务 %s", p.Name, jobID)
			}
		}
		run.Logf("==> 完成：检查 %d 个项目，%d 个服务有更新", len(ids), updates)
		return nil
	})
}

func (s *Server) handleProjectImages(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	checks := s.imageChecks(p.ID)
	s.settleAppliedUpdates(r.Context(), checks)
	return ok(w, checks)
}

func (s *Server) handleCheckProjectImages(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if _, _, err := s.checkProjectImages(ctx, p, digestCache{}); err != nil {
		return err
	}
	return ok(w, s.imageChecks(p.ID))
}

func (s *Server) handleImageUpdates(w http.ResponseWriter, r *http.Request, u *User) error {
	checks := s.imageChecks(0)
	s.settleAppliedUpdates(r.Context(), checks)
	out := []ImageCheck{}
	for _, c := range checks {
		if c.Status == "update" && u.canProject(c.ProjectID) {
			out = append(out, c)
		}
	}
	return ok(w, out)
}

// ---------- custom cron tasks (commands run inside a service container) ----------

type CronTask struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	ProjectID   int64  `json:"projectId"`
	ProjectName string `json:"projectName"`
	Service     string `json:"service"`
	Command     string `json:"command"`
	User        string `json:"user"`
	Schedule    string `json:"schedule"`
	Timeout     int    `json:"timeout"`
	Enabled     bool   `json:"enabled"`
	Notify      bool   `json:"notify"`
	LastRunAt   int64  `json:"lastRunAt"`
	LastStatus  string `json:"lastStatus"`
	LastJobID   string `json:"lastJobId"`
	CreatedAt   int64  `json:"createdAt"`
	Next        int64  `json:"next"`
}

func (s *Server) listCronTasks(u *User) []CronTask {
	rows, err := s.db.Query(`SELECT t.id, t.name, t.project_id, p.name, t.service, t.command, t.user, t.schedule, t.timeout, t.enabled, t.notify,
		t.last_run_at, t.last_status, t.last_job_id, t.created_at FROM cron_tasks t JOIN projects p ON p.id=t.project_id ORDER BY p.name, t.name`)
	out := []CronTask{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t CronTask
		var en, nt int
		if rows.Scan(&t.ID, &t.Name, &t.ProjectID, &t.ProjectName, &t.Service, &t.Command, &t.User, &t.Schedule, &t.Timeout, &en, &nt,
			&t.LastRunAt, &t.LastStatus, &t.LastJobID, &t.CreatedAt) != nil {
			continue
		}
		if u != nil && !u.canProject(t.ProjectID) {
			continue
		}
		t.Enabled, t.Notify = en == 1, nt == 1
		out = append(out, t)
	}
	return out
}

func (s *Server) getCronTask(id int64) (*CronTask, error) {
	for _, t := range s.listCronTasks(nil) {
		if t.ID == id {
			return &t, nil
		}
	}
	return nil, errNotFound
}

func (s *Server) handleListCronTasks(w http.ResponseWriter, r *http.Request, u *User) error {
	ts := s.listCronTasks(u)
	if pid := r.URL.Query().Get("project"); pid != "" {
		id, _ := strconv.ParseInt(pid, 10, 64)
		filtered := []CronTask{}
		for _, t := range ts {
			if t.ProjectID == id {
				filtered = append(filtered, t)
			}
		}
		ts = filtered
	}
	for i := range ts {
		ts[i].Next = s.nextRun(fmt.Sprintf("cron:%d", ts[i].ID))
	}
	return ok(w, ts)
}

func (s *Server) handleSaveCronTask(w http.ResponseWriter, r *http.Request, u *User) error {
	var t CronTask
	if err := readJSON(r, &t); err != nil {
		return err
	}
	t.Name, t.Service, t.Command = strings.TrimSpace(t.Name), strings.TrimSpace(t.Service), strings.TrimSpace(t.Command)
	if t.Name == "" || t.Service == "" || t.Command == "" {
		return bad("名称、服务和命令不能为空")
	}
	if _, err := cronParser.Parse(t.Schedule); err != nil {
		return bad("Cron 表达式无效：%v", err)
	}
	if t.Timeout <= 0 {
		t.Timeout = 600
	}
	if t.Timeout > 86400 {
		return bad("超时不能超过 24 小时")
	}
	if !u.canProject(t.ProjectID) {
		return errNotFound
	}
	p, err := s.getProject(t.ProjectID)
	if err != nil {
		return err
	}
	if r.PathValue("id") != "" {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		old, err := s.getCronTask(id)
		if err != nil || !u.canProject(old.ProjectID) {
			return errNotFound
		}
		_, err = s.db.Exec(`UPDATE cron_tasks SET name=?, project_id=?, service=?, command=?, user=?, schedule=?, timeout=?, enabled=?, notify=? WHERE id=?`,
			t.Name, t.ProjectID, t.Service, t.Command, t.User, t.Schedule, t.Timeout, boolInt(t.Enabled), boolInt(t.Notify), id)
		t.ID = id
		s.audit(r, u, "cron.update", p.Name+"/"+t.Name, t.Command, err)
		if err != nil {
			return err
		}
	} else {
		res, err := s.db.Exec(`INSERT INTO cron_tasks(name,project_id,service,command,user,schedule,timeout,enabled,notify,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			t.Name, t.ProjectID, t.Service, t.Command, t.User, t.Schedule, t.Timeout, boolInt(t.Enabled), boolInt(t.Notify), time.Now().Unix())
		s.audit(r, u, "cron.create", p.Name+"/"+t.Name, t.Command, err)
		if err != nil {
			return err
		}
		t.ID, _ = res.LastInsertId()
	}
	s.reloadSchedules()
	return ok(w, map[string]any{"id": t.ID})
}

func (s *Server) handleDeleteCronTask(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	t, err := s.getCronTask(id)
	if err != nil || !u.canProject(t.ProjectID) {
		return errNotFound
	}
	_, err = s.db.Exec(`DELETE FROM cron_tasks WHERE id=?`, id)
	s.audit(r, u, "cron.delete", t.ProjectName+"/"+t.Name, "", err)
	s.reloadSchedules()
	return done(w, err)
}

func (s *Server) handleRunCronTask(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	t, err := s.getCronTask(id)
	if err != nil || !u.canProject(t.ProjectID) {
		return errNotFound
	}
	jobID := s.runCronTask(id, u.Username)
	s.audit(r, u, "cron.run", t.ProjectName+"/"+t.Name, "手动执行", nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) runCronTask(id int64, actor string) string {
	t, err := s.getCronTask(id)
	if err != nil {
		return ""
	}
	jobID := s.jobs.Submit(jobSpec("cron.task", "定时任务："+t.Name, t.ProjectName, actor, fmt.Sprintf("cron:%d", id)), func(run *jobRun) (err error) {
		defer func() {
			st := "success"
			if err != nil {
				st = "failed"
				if t.Notify {
					s.notify.Emit(notify.Event{Type: "cron_failed", Title: "定时任务失败：" + t.Name,
						Body: fmt.Sprintf("项目 %s / 服务 %s：%v", t.ProjectName, t.Service, err), Target: fmt.Sprintf("cron:%d", id)})
				}
			}
			_, _ = s.db.Exec(`UPDATE cron_tasks SET last_status=? WHERE id=?`, st, id)
		}()
		ctx, cancel := context.WithTimeout(run.Context(), time.Duration(t.Timeout)*time.Second)
		defer cancel()
		cs, err := s.dk.ListContainers(ctx, false, map[string][]string{"label": {
			"com.docker.compose.project=" + t.ProjectName, "com.docker.compose.service=" + t.Service}})
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			return fmt.Errorf("服务 %s 没有运行中的容器", t.Service)
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].Name() < cs[j].Name() })
		args := []string{"exec"}
		if t.User != "" {
			args = append(args, "-u", t.User)
		}
		args = append(args, cs[0].Id, "sh", "-c", t.Command)
		run.Logf("$ docker exec %s sh -c %q", cs[0].Name(), t.Command)
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdout, cmd.Stderr = run, run
		start := time.Now()
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("执行超时（%d 秒）", t.Timeout)
			}
			return fmt.Errorf("命令失败：%v", err)
		}
		run.Logf("==> 完成，用时 %s", time.Since(start).Round(time.Millisecond))
		return nil
	})
	_, _ = s.db.Exec(`UPDATE cron_tasks SET last_run_at=?, last_job_id=?, last_status='running' WHERE id=?`, time.Now().Unix(), jobID, id)
	return jobID
}
