package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"compose-panel/internal/notify"
)

// ---------- jobs ----------

type jobRow struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Target     string `json:"target"`
	Status     string `json:"status"`
	Progress   int    `json:"progress"`
	Actor      string `json:"actor"`
	Error      string `json:"error"`
	CreatedAt  int64  `json:"createdAt"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
}

func (s *Server) queryJobs(where string, args ...any) []jobRow {
	rows, err := s.db.Query(`SELECT id, kind, title, target, status, progress, actor, error, created_at, started_at, finished_at FROM jobs `+where, args...)
	if err != nil {
		return []jobRow{}
	}
	defer rows.Close()
	out := []jobRow{}
	for rows.Next() {
		var j jobRow
		if rows.Scan(&j.ID, &j.Kind, &j.Title, &j.Target, &j.Status, &j.Progress, &j.Actor, &j.Error, &j.CreatedAt, &j.StartedAt, &j.FinishedAt) == nil {
			out = append(out, j)
		}
	}
	return out
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request, u *User) error {
	where, args := "WHERE 1=1", []any{}
	q := r.URL.Query()
	if v := q.Get("status"); v != "" {
		if v == "active" {
			where += " AND status IN ('queued','running')"
		} else {
			where += " AND status=?"
			args = append(args, v)
		}
	}
	if v := q.Get("kind"); v != "" {
		where += " AND kind LIKE ?"
		args = append(args, v+"%")
	}
	if v := q.Get("target"); v != "" {
		where += " AND target=?"
		args = append(args, v)
	}
	if q.Has("page") {
		// Paged: {items, total, page, pageSize}. Without page, the plain list (older callers).
		page := max(queryInt(r, "page", 1), 1)
		size := min(max(queryInt(r, "pageSize", 20), 1), 200)
		var total int
		_ = s.db.QueryRow(`SELECT count(*) FROM jobs `+where, args...).Scan(&total)
		if last := max((total+size-1)/size, 1); page > last {
			page = last
		}
		items := s.queryJobs(where+" ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?", append(args, size, (page-1)*size)...)
		return ok(w, map[string]any{"items": nonNil(items), "total": total, "page": page, "pageSize": size})
	}
	limit := min(max(queryInt(r, "limit", 100), 1), 500)
	return ok(w, s.queryJobs(where+" ORDER BY created_at DESC LIMIT ?", append(args, limit)...))
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request, u *User) error {
	list := s.queryJobs("WHERE id=?", r.PathValue("id"))
	if len(list) == 0 {
		return errNotFound
	}
	return ok(w, list[0])
}

// handleJobLog streams a job's log as SSE: the backlog first, then live lines.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request, u *User) error {
	id := r.PathValue("id")
	if len(s.queryJobs("WHERE id=?", id)) == 0 {
		return errNotFound
	}
	backlog, ch, cancel, _ := s.jobs.Subscribe(id)
	defer cancel()
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	wrote := false
	for _, l := range strings.Split(strings.TrimRight(string(backlog), "\n"), "\n") {
		if l != "" {
			writeSSE(w, l)
			wrote = true
		}
	}
	if !wrote {
		_, _ = fmt.Fprint(w, ": ok\n\n")
	}
	if fl != nil {
		fl.Flush()
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case line, ok := <-ch:
			if !ok {
				st := s.queryJobs("WHERE id=?", id)
				status := ""
				if len(st) > 0 {
					status = st[0].Status
				}
				fmt.Fprintf(w, "event: end\ndata: %s\n\n", status)
				if fl != nil {
					fl.Flush()
				}
				return nil
			}
			writeSSE(w, strings.TrimRight(string(line), "\n"))
			if fl != nil {
				fl.Flush()
			}
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request, u *User) error {
	id := r.PathValue("id")
	if !s.jobs.Cancel(id) {
		return bad("任务已结束")
	}
	s.audit(r, u, "job.cancel", id, "", nil)
	return ok(w, nil)
}

// ---------- scheduler ----------

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func (s *Server) startScheduler() {
	s.reloadSchedules()
}

// reloadSchedules rebuilds all cron entries from settings and backup policies.
func (s *Server) reloadSchedules() {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()
	if s.cron != nil {
		s.cron.Stop()
	}
	c := cron.New(cron.WithLocation(s.location()), cron.WithParser(cronParser))
	s.cronIDs = map[string]cron.EntryID{}
	s.cronMeta = map[cron.EntryID]scheduleMeta{}
	add := func(key string, meta scheduleMeta, fn func()) {
		id, err := c.AddFunc(meta.Spec, fn)
		if err != nil {
			slog.Warn("bad schedule", "key", key, "spec", meta.Spec, "err", err)
			return
		}
		s.cronIDs[key] = id
		s.cronMeta[id] = meta
	}
	rows, err := s.db.Query(`SELECT id FROM backup_policies WHERE enabled=1`)
	if err == nil {
		var ids []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			p, err := s.getPolicy(id)
			if err != nil {
				continue
			}
			pid := id
			add(fmt.Sprintf("policy:%d", id), scheduleMeta{Kind: "backup", Name: "备份：" + p.Name, Spec: p.Schedule, Target: p.Label}, func() {
				if p, err := s.getPolicy(pid); err == nil && p.Enabled {
					s.runPolicy(p, "scheduler")
				}
			})
		}
	}
	add("system:cert-check", scheduleMeta{Kind: "system", Name: "证书到期检查", Spec: "@every 6h", Target: "certificates"}, s.checkCerts)
	if spec := s.setting("image_prune_cron", ""); spec != "" {
		add("system:prune", scheduleMeta{Kind: "system", Name: "清理悬空镜像与构建缓存", Spec: spec, Target: "docker"}, s.scheduledPrune)
	}
	if spec := s.setting("panel_backup_cron", ""); spec != "" {
		add("system:panel-backup", scheduleMeta{Kind: "system", Name: "面板自身备份", Spec: spec, Target: "panel.db"}, func() { s.panelBackup("scheduler") })
	}
	s.addExtraSchedules(add)
	c.Start()
	s.cron = c
}

func (s *Server) nextRun(key string) int64 {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()
	if s.cron == nil {
		return 0
	}
	id, ok := s.cronIDs[key]
	if !ok {
		return 0
	}
	e := s.cron.Entry(id)
	if e.Next.IsZero() {
		return 0
	}
	return e.Next.Unix()
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request, u *User) error {
	s.cronMu.Lock()
	type item struct {
		Key    string `json:"key"`
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Spec   string `json:"spec"`
		Target string `json:"target"`
		Next   int64  `json:"next"`
		Prev   int64  `json:"prev"`
	}
	out := []item{}
	for key, id := range s.cronIDs {
		e := s.cron.Entry(id)
		m := s.cronMeta[id]
		it := item{Key: key, Kind: m.Kind, Name: m.Name, Spec: m.Spec, Target: m.Target}
		if !e.Next.IsZero() {
			it.Next = e.Next.Unix()
		}
		if !e.Prev.IsZero() {
			it.Prev = e.Prev.Unix()
		}
		out = append(out, it)
	}
	s.cronMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Next < out[j].Next })
	return ok(w, map[string]any{"items": out, "timezone": s.setting("timezone", "Asia/Shanghai")})
}

func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request, u *User) error {
	key := r.PathValue("key")
	var jobID string
	switch {
	case strings.HasPrefix(key, "policy:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "policy:"), 10, 64)
		p, err := s.getPolicy(id)
		if err != nil {
			return err
		}
		jobID = s.runPolicy(p, u.Username)
	case key == "system:cert-check":
		go s.checkCerts()
	case key == "system:prune":
		jobID = s.scheduledPrune2(u.Username)
	case key == "system:panel-backup":
		jobID = s.panelBackup(u.Username)
	default:
		id, handled := s.runExtraSchedule(key, u.Username)
		if !handled {
			return errNotFound
		}
		jobID = id
	}
	s.audit(r, u, "schedule.run", key, "手动触发", nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) scheduledPrune() { s.scheduledPrune2("scheduler") }

func (s *Server) scheduledPrune2(actor string) string {
	return s.jobs.Submit(jobSpec("docker.prune", "清理悬空镜像与构建缓存", "docker", actor, "docker:prune"), func(run *jobRun) error {
		ctx := run.Context()
		img, err := s.dk.Prune(ctx, "images", map[string][]string{"dangling": {"true"}})
		if err != nil {
			return err
		}
		run.Logf("悬空镜像：删除 %d 个，释放 %s", img.Deleted, humanBytes(img.SpaceReclaimed))
		b, err := s.dk.Prune(ctx, "build", nil)
		if err == nil {
			run.Logf("构建缓存：释放 %s", humanBytes(b.SpaceReclaimed))
		}
		return nil
	})
}

// panelBackup snapshots the panel database into the backups dir and keeps the last 14.
func (s *Server) panelBackup(actor string) string {
	return s.jobs.Submit(jobSpec("panel.backup", "面板自身备份", "panel.db", actor, "panel:backup"), func(run *jobRun) error {
		dir := filepath.Join(s.cfg.BackupsDir, "_panel")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		name := filepath.Join(dir, "panel-"+time.Now().In(s.location()).Format("20060102-150405")+".db")
		if _, err := s.db.ExecContext(run.Context(), `VACUUM INTO ?`, name); err != nil {
			return err
		}
		_ = os.Chmod(name, 0o600)
		st, _ := os.Stat(name)
		if st != nil {
			run.Logf("已写入 %s（%s）", name, humanBytes(st.Size()))
		}
		run.Logf("提示：恢复面板还需要 %s（主密钥），请另行妥善保存", filepath.Join(s.cfg.DataDir, "master.key"))
		entries, _ := filepath.Glob(filepath.Join(dir, "panel-*.db"))
		sort.Strings(entries)
		for len(entries) > 14 {
			_ = os.Remove(entries[0])
			run.Logf("清理旧快照 %s", filepath.Base(entries[0]))
			entries = entries[1:]
		}
		return nil
	})
}

func (s *Server) checkCerts() {
	days, _ := strconv.Atoi(s.setting("cert_alert_days", "14"))
	for _, c := range s.allCerts() {
		if len(c.Domains) == 0 || c.Source == "internal" {
			continue
		}
		if c.DaysLeft <= days {
			s.notify.Emit(notify.Event{Type: "cert_expiring", Title: "证书即将过期：" + c.Domains[0],
				Body: fmt.Sprintf("%s 将在 %d 天后过期（%s）。来源：%s", strings.Join(c.Domains, ", "), c.DaysLeft, c.NotAfter.In(s.location()).Format("2006-01-02"), c.Source), Target: c.Domains[0]})
		}
	}
}

// monitorLoop watches containers and disk usage every 30 seconds.
func (s *Server) monitorLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		cs, err := s.dk.ListContainers(cctx, true, map[string][]string{"label": {"com.docker.compose.project"}})
		cancel()
		if err == nil {
			for _, c := range cs {
				if c.Labels["panel.monitor"] == "false" {
					continue
				}
				bad := ""
				switch {
				case c.State == "restarting":
					bad = "反复重启"
				case containerHealth(c) == "unhealthy":
					bad = "健康检查失败"
				case c.State == "exited" && !strings.Contains(c.Status, "Exited (0)") && c.Labels["com.docker.compose.oneoff"] != "True":
					bad = "异常退出：" + c.Status
				}
				if bad != "" {
					s.notify.Emit(notify.Event{Type: "container_unhealthy", Title: "容器异常：" + c.Name(), Body: bad, Target: c.Name()})
				}
			}
		}
		limit, _ := strconv.Atoi(s.setting("disk_alert_percent", "85"))
		if hs := hostStats(s.cfg.DataDir); hs.DiskTotal > 0 && limit > 0 {
			pct := float64(hs.DiskUsed) * 100 / float64(hs.DiskTotal)
			if pct >= float64(limit) {
				s.notify.Emit(notify.Event{Type: "disk_high", Title: "磁盘空间不足", Body: fmt.Sprintf("已用 %.0f%%（%s / %s）", pct, humanBytes(int64(hs.DiskUsed)), humanBytes(int64(hs.DiskTotal))), Target: "disk"})
			}
		}
	}
}

// ---------- notifications ----------

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request, u *User) error {
	rows, err := s.db.Query(`SELECT id, name, kind, config, enabled, last_status, COALESCE(last_sent_at,0), created_at FROM notify_channels ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, last, created int64
		var name, kind, enc, status string
		var enabled int
		if rows.Scan(&id, &name, &kind, &enc, &enabled, &status, &last, &created) != nil {
			continue
		}
		var c notify.ChannelConfig
		_ = json.Unmarshal([]byte(s.box.MustOpen(enc)), &c)
		c.Secret, c.Password = maskSecret(c.Secret), maskSecret(c.Password)
		if c.BotToken != "" {
			c.BotToken = c.BotToken[:min(6, len(c.BotToken))] + "••••••"
		}
		if kind != "webhook" && c.URL != "" {
			c.URL = maskURL(c.URL)
		}
		out = append(out, map[string]any{"id": id, "name": name, "kind": kind, "config": c, "enabled": enabled == 1, "lastStatus": status, "lastSentAt": last, "createdAt": created})
	}
	rules := map[string][]int64{}
	rrows, err := s.db.Query(`SELECT event, channel_id FROM notify_rules`)
	if err == nil {
		for rrows.Next() {
			var ev string
			var cid int64
			if rrows.Scan(&ev, &cid) == nil {
				rules[ev] = append(rules[ev], cid)
			}
		}
		rrows.Close()
	}
	return ok(w, map[string]any{"channels": out, "rules": rules, "events": notify.Events})
}

var (
	maskQueryRe = regexp.MustCompile(`((?:key|access_token|token)=)[^&]+`)
	maskPathRe  = regexp.MustCompile(`/[A-Za-z0-9_-]{16,}(/?)$`)
)

// maskURL hides tokens in robot webhook URLs (query keys and long trailing path segments).
func maskURL(u string) string {
	base, query, hasQ := strings.Cut(u, "?")
	base = maskPathRe.ReplaceAllString(base, "/••••••$1")
	if hasQ {
		return base + "?" + maskQueryRe.ReplaceAllString(query, "${1}••••••")
	}
	return base
}

func (s *Server) handleSaveChannel(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Name    string               `json:"name"`
		Kind    string               `json:"kind"`
		Config  notify.ChannelConfig `json:"config"`
		Enabled bool                 `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Name == "" {
		return bad("名称不能为空")
	}
	id, _ := pathID(r, "id")
	if id > 0 {
		var enc string
		if s.db.QueryRow(`SELECT config FROM notify_channels WHERE id=?`, id).Scan(&enc) == nil {
			var old notify.ChannelConfig
			_ = json.Unmarshal([]byte(s.box.MustOpen(enc)), &old)
			keep := func(v *string, o string) {
				if *v == "" || strings.Contains(*v, "••••••") {
					*v = o
				}
			}
			keep(&req.Config.Secret, old.Secret)
			keep(&req.Config.Password, old.Password)
			keep(&req.Config.BotToken, old.BotToken)
			keep(&req.Config.URL, old.URL)
		}
	}
	switch req.Kind {
	case "webhook", "wecom", "dingtalk", "feishu", "bark":
		if !strings.HasPrefix(req.Config.URL, "https://") && !strings.HasPrefix(req.Config.URL, "http://") {
			return bad("请填写 URL")
		}
	case "telegram":
		if req.Config.BotToken == "" || req.Config.ChatID == "" {
			return bad("请填写 Bot Token 和 Chat ID")
		}
	case "email":
		if req.Config.Host == "" || req.Config.To == "" {
			return bad("请填写 SMTP 服务器和收件人")
		}
	default:
		return bad("渠道类型无效")
	}
	conf, _ := json.Marshal(req.Config)
	var err error
	if id > 0 {
		_, err = s.db.Exec(`UPDATE notify_channels SET name=?, kind=?, config=?, enabled=? WHERE id=?`, req.Name, req.Kind, s.box.Seal(string(conf)), boolInt(req.Enabled), id)
	} else {
		var res interface{ LastInsertId() (int64, error) }
		res, err = s.db.Exec(`INSERT INTO notify_channels(name,kind,config,enabled,created_at) VALUES(?,?,?,?,?)`, req.Name, req.Kind, s.box.Seal(string(conf)), boolInt(req.Enabled), time.Now().Unix())
		if err == nil {
			id, _ = res.LastInsertId()
			// New channels receive failure events by default.
			for _, ev := range []string{"deploy_failed", "container_unhealthy", "backup_failed", "cert_expiring", "disk_high"} {
				_, _ = s.db.Exec(`INSERT OR IGNORE INTO notify_rules(event,channel_id) VALUES(?,?)`, ev, id)
			}
		}
	}
	s.audit(r, u, "notify.channel_save", req.Name, req.Kind, err)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"id": id})
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM notify_channels WHERE id=?`, id)
	s.audit(r, u, "notify.channel_delete", fmt.Sprint(id), "", err)
	return done(w, err)
}

func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var kind, enc string
	if err := s.db.QueryRow(`SELECT kind, config FROM notify_channels WHERE id=?`, id).Scan(&kind, &enc); err != nil {
		return errNotFound
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	err = s.notify.SendTo(ctx, kind, enc, notify.Event{Type: "test", Title: "测试通知", Body: "这是一条来自 Compose Panel 的测试消息，发送人 " + u.Username, Time: time.Now().Unix()})
	status := "ok"
	if err != nil {
		status = err.Error()
	}
	_, _ = s.db.Exec(`UPDATE notify_channels SET last_sent_at=?, last_status=? WHERE id=?`, time.Now().Unix(), status, id)
	if err != nil {
		return ok(w, map[string]any{"ok": false, "message": err.Error()})
	}
	return ok(w, map[string]any{"ok": true, "message": "发送成功"})
}

func (s *Server) handleSaveRules(w http.ResponseWriter, r *http.Request, u *User) error {
	var req map[string][]int64
	if err := readJSON(r, &req); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM notify_rules`); err != nil {
		return err
	}
	for ev, ids := range req {
		for _, id := range ids {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO notify_rules(event,channel_id) VALUES(?,?)`, ev, id); err != nil {
				return err
			}
		}
	}
	err = tx.Commit()
	s.audit(r, u, "notify.rules", "", "", err)
	return done(w, err)
}

// ---------- settings ----------

var settingKeys = []string{"panel_domain", "acme_email", "timezone", "secure_headers", "session_ttl_hours", "image_prune_cron", "panel_backup_cron", "disk_alert_percent", "cert_alert_days", "public_ip", "image_check_cron"}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request, u *User) error {
	out := map[string]string{}
	for _, k := range settingKeys {
		out[k] = s.setting(k, "")
	}
	return ok(w, out)
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request, u *User) error {
	var req map[string]string
	if err := readJSON(r, &req); err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, k := range settingKeys {
		allowed[k] = true
	}
	if tz, ok := req["timezone"]; ok {
		if _, err := time.LoadLocation(tz); err != nil {
			return bad("时区无效")
		}
	}
	for _, k := range []string{"image_prune_cron", "panel_backup_cron", "image_check_cron"} {
		if v := req[k]; v != "" {
			if _, err := cronParser.Parse(v); err != nil {
				return bad("%s 的 Cron 表达式无效", k)
			}
		}
	}
	if d, ok := req["panel_domain"]; ok && d != "" {
		req["panel_domain"] = strings.ToLower(strings.TrimSpace(d))
		if !domainRe.MatchString(req["panel_domain"]) {
			return bad("面板域名格式不正确")
		}
	}
	var changed []string
	for k, v := range req {
		if !allowed[k] {
			continue
		}
		if s.setting(k, "") != v {
			s.setSetting(k, strings.TrimSpace(v))
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	s.audit(r, u, "settings.update", "", strings.Join(changed, ", "), nil)
	s.reloadSchedules()
	out := map[string]any{"ok": true}
	if err := s.syncCaddy(r.Context()); err != nil {
		out["warning"] = "推送到 Caddy 失败：" + err.Error()
	}
	return ok(w, out)
}

// ---------- overview & system ----------

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request, u *User) error {
	ctx := r.Context()
	out := map[string]any{}
	out["host"] = hostStats(s.cfg.DataDir)
	byProject := s.containersByProject(ctx)
	var running, total, unhealthy int
	all, derr := s.dk.ListContainers(ctx, true, nil)
	for _, c := range all {
		total++
		if c.State == "running" {
			running++
		}
		if containerHealth(c) == "unhealthy" || c.State == "restarting" {
			unhealthy++
		}
	}
	out["containers"] = map[string]int{"running": running, "total": total, "unhealthy": unhealthy}
	out["docker"] = derr == nil
	if derr != nil {
		out["dockerError"] = derr.Error()
	}
	var projects []map[string]any
	rows, err := s.db.Query(`SELECT id, name, kind FROM projects ORDER BY name`)
	if err == nil {
		for rows.Next() {
			var id int64
			var name, kind string
			if rows.Scan(&id, &name, &kind) == nil && u.canProject(id) {
				st, rn, uh := summarizeStatus(byProject[name])
				projects = append(projects, map[string]any{"id": id, "name": name, "kind": kind, "status": st, "running": rn, "total": len(byProject[name]), "unhealthy": uh})
			}
		}
		rows.Close()
	}
	out["projects"] = nonNil(projects)
	// Recent deployments.
	drows, err := s.db.Query(`SELECT d.id, d.number, d.status, d.trigger, d.actor, d.commit_sha, d.commit_msg, d.job_id, d.error, d.created_at, d.finished_at, p.id, p.name
		FROM deployments d JOIN projects p ON p.id=d.project_id ORDER BY d.id DESC LIMIT 8`)
	deps := []map[string]any{}
	if err == nil {
		for drows.Next() {
			var d deploySummary
			var pid int64
			var pname string
			if drows.Scan(&d.ID, &d.Number, &d.Status, &d.Trigger, &d.Actor, &d.CommitSHA, &d.CommitMsg, &d.JobID, &d.Error, &d.CreatedAt, &d.FinishedAt, &pid, &pname) == nil && u.canProject(pid) {
				deps = append(deps, map[string]any{"deployment": d, "projectId": pid, "project": pname})
			}
		}
		drows.Close()
	}
	out["deployments"] = deps
	// Backups in the last 24 hours.
	var bOK, bFail int
	var bSize int64
	since := time.Now().Add(-24 * time.Hour).Unix()
	_ = s.db.QueryRow(`SELECT COALESCE(sum(status='success'),0), COALESCE(sum(status='failed'),0), COALESCE(sum(CASE WHEN status='success' THEN size END),0) FROM backups WHERE started_at>=?`, since).Scan(&bOK, &bFail, &bSize)
	out["backups"] = map[string]any{"success": bOK, "failed": bFail, "size": bSize, "recentFailures": s.listBackups(`WHERE b.status='failed' AND b.started_at>=? ORDER BY b.started_at DESC LIMIT 5`, since)}
	// Certificates expiring soon.
	days, _ := strconv.Atoi(s.setting("cert_alert_days", "14"))
	expiring := []certView{}
	for _, c := range s.allCerts() {
		if c.DaysLeft <= days*2 && c.Source != "internal" {
			expiring = append(expiring, c)
		}
	}
	out["certs"] = expiring
	out["jobs"] = s.queryJobs(`WHERE status IN ('queued','running') OR created_at>=? ORDER BY created_at DESC LIMIT 8`, time.Now().Add(-6*time.Hour).Unix())
	out["unhealthy"] = unhealthyList(all)
	return ok(w, out)
}

func unhealthyList(cs []dockerContainer) []containerDTO {
	out := []containerDTO{}
	for _, c := range cs {
		if containerHealth(c) == "unhealthy" || c.State == "restarting" ||
			(c.State == "exited" && !strings.Contains(c.Status, "Exited (0)") && c.Labels["com.docker.compose.project"] != "") {
			out = append(out, toContainerDTO(c))
		}
	}
	return out
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request, u *User) error {
	out := map[string]any{"version": Version, "go": runtime.Version(), "startedAt": s.startedAt.Unix(),
		"dataDir": s.cfg.DataDir, "projectsDir": s.cfg.ProjectsDir, "backupsDir": s.cfg.BackupsDir,
		"proxyNetwork": s.cfg.ProxyNetwork, "dbNetwork": s.cfg.DBNetwork, "host": hostStats(s.cfg.DataDir)}
	if v, err := s.dk.Version(r.Context()); err == nil {
		out["dockerVersion"] = v["Version"]
		out["dockerApi"] = v["ApiVersion"]
		out["os"] = fmt.Sprint(v["Os"], "/", v["Arch"], " · kernel ", v["KernelVersion"])
	} else {
		out["dockerError"] = err.Error()
	}
	if info, err := s.dk.Info(r.Context()); err == nil {
		out["hostname"] = info["Name"]
		out["cpus"] = info["NCPU"]
		out["memTotal"] = info["MemTotal"]
		out["osName"] = info["OperatingSystem"]
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.caddy.Ping(ctx); err != nil {
		out["caddy"] = err.Error()
	} else {
		out["caddy"] = "ok"
	}
	if st, err := os.Stat(filepath.Join(s.cfg.DataDir, "panel.db")); err == nil {
		out["dbSize"] = st.Size()
	}
	snaps, _ := filepath.Glob(filepath.Join(s.cfg.BackupsDir, "_panel", "panel-*.db"))
	sort.Strings(snaps)
	if len(snaps) > 0 {
		if st, err := os.Stat(snaps[len(snaps)-1]); err == nil {
			out["lastPanelBackup"] = st.ModTime().Unix()
		}
	}
	out["panelBackups"] = len(snaps)
	return ok(w, out)
}

func (s *Server) handlePanelBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	jobID := s.panelBackup(u.Username)
	s.audit(r, u, "panel.backup", "", "", nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) handleMasterKey(w http.ResponseWriter, r *http.Request, u *User) error {
	p := filepath.Join(s.cfg.DataDir, "master.key")
	st, err := os.Stat(p)
	out := map[string]any{"exists": err == nil && !st.IsDir(), "path": p}
	if r.URL.Query().Get("reveal") == "1" {
		raw, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("读取主密钥失败：%w", err)
		}
		out["key"] = strings.TrimSpace(string(raw))
		s.audit(r, u, "system.reveal_master_key", "", "查看面板主密钥", nil)
	}
	return ok(w, out)
}
