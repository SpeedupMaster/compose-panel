package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"compose-panel/internal/docker"
	"compose-panel/internal/notify"
)

const (
	metricsRetention = 7 * 24 * time.Hour
	jobsRetention    = 30 * 24 * time.Hour
)

var orphanTempMaxAge = 6 * time.Hour

var caddyStartRetryDelays = []time.Duration{
	time.Second,
	3 * time.Second,
	5 * time.Second,
}

func isCaddyContainer(attrs map[string]string) bool {
	return strings.TrimPrefix(attrs["name"], "/") == "panel-caddy" ||
		attrs["com.docker.compose.service"] == "caddy"
}

func (s *Server) expectsCaddyConfig() bool {
	if strings.TrimSpace(s.setting("panel_domain", "")) != "" {
		return true
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM sites WHERE enabled=1`).Scan(&n)
	return n > 0
}

func (s *Server) ensureCaddyLoaded(ctx context.Context) {
	if s.caddy == nil || !s.expectsCaddyConfig() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	loaded, err := s.caddy.IsLoaded(cctx)
	cancel()
	if err != nil || loaded {
		return
	}
	slog.Warn("caddy http config missing, resyncing")
	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	defer scancel()
	if err := s.syncCaddy(sctx); err != nil {
		slog.Warn("caddy auto-resync failed", "err", err)
	} else {
		slog.Info("caddy config restored")
	}
}

func (s *Server) resyncCaddyAfterStart(ctx context.Context) {
	if s.caddy == nil {
		return
	}
	for i := 0; i <= len(caddyStartRetryDelays); i++ {
		if i > 0 {
			timer := time.NewTimer(caddyStartRetryDelays[i-1])
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.syncCaddy(cctx)
		cancel()
		if err == nil {
			slog.Info("caddy config synced after container start")
			return
		}
		slog.Warn("caddy resync after start failed", "attempt", i+1, "err", err)
	}
}

func (s *Server) cleanupOrphans(ctx context.Context) {
	now := time.Now()
	// 1. Delete stale temp files/dirs in DataDir/tmp/* older than orphanTempMaxAge.
	tmpDir := filepath.Join(s.cfg.DataDir, "tmp")
	if entries, err := os.ReadDir(tmpDir); err == nil {
		for _, entry := range entries {
			if ctx.Err() != nil {
				return
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if now.Sub(info.ModTime()) > orphanTempMaxAge {
				_ = os.RemoveAll(filepath.Join(tmpDir, entry.Name()))
			}
		}
	}
	// 2. Delete stale DataDir/panel-snapshot-*.db files older than orphanTempMaxAge.
	if snaps, err := filepath.Glob(filepath.Join(s.cfg.DataDir, "panel-snapshot-*.db")); err == nil {
		for _, p := range snaps {
			if ctx.Err() != nil {
				return
			}
			if fi, err := os.Stat(p); err == nil && now.Sub(fi.ModTime()) > orphanTempMaxAge {
				_ = os.Remove(p)
			}
		}
	}
	// 2b. Clean up stale staged temp files, orphan archive files, and dangling DB rows in container-logs.
	s.cleanupLogArchiveOrphans(ctx)
	// 3. Remove orphaned Docker containers & volumes with label panel.drill=1 when no drill job is live.
	if s.dk == nil || (s.jobs != nil && s.jobs.HasLiveKind("backup.drill")) {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if cs, err := s.dk.ListContainers(dctx, true, map[string][]string{"label": {"panel.drill=1"}}); err == nil {
		for _, c := range cs {
			if dctx.Err() != nil || (s.jobs != nil && s.jobs.HasLiveKind("backup.drill")) {
				return
			}
			_ = s.dk.RemoveContainerWithVolumes(dctx, c.Id, true, true)
		}
	}
	if vols, err := s.dk.ListVolumes(dctx); err == nil {
		for _, v := range vols {
			if dctx.Err() != nil || (s.jobs != nil && s.jobs.HasLiveKind("backup.drill")) {
				return
			}
			if v.Labels["panel.drill"] == "1" {
				_ = s.dk.RemoveVolume(dctx, v.Name)
			}
		}
	}
}

func (s *Server) housekeep(ctx context.Context) {
	now := time.Now()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM metrics WHERE ts < ?`, now.Add(-metricsRetention).Unix())
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now.Unix())
	if s.jobs != nil {
		s.jobs.Prune(jobsRetention)
	}
	s.pruneLogArchives()
	s.cleanupOrphans(ctx)
}

// metricsLoop samples host and container resource usage once a minute.
func (s *Server) metricsLoop(ctx context.Context) {
	// Align to the next full minute so samples of all series share timestamps.
	wait := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute))
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	n := 0
	for {
		if ctx.Err() != nil {
			return
		}
		s.sampleMetrics(ctx, time.Now().Truncate(time.Minute).Unix())
		if ctx.Err() == nil {
			s.ensureCaddyLoaded(ctx)
		}
		if n%60 == 0 && ctx.Err() == nil {
			s.housekeep(ctx)
		}
		n++
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) sampleMetrics(ctx context.Context, ts int64) {
	hs := hostStats(s.cfg.DataDir)

	type row struct {
		name, project string
		st            *docker.Stats
	}
	var rows []row
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	cs, err := s.dk.ListContainers(lctx, false, nil)
	cancel()
	if err == nil {
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
	loop:
		for _, c := range cs {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break loop
			}
			wg.Add(1)
			go func(c docker.Container) {
				defer wg.Done()
				defer func() { <-sem }()
				sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				st, err := s.dk.ContainerStats(sctx, c.Id)
				if err != nil {
					return
				}
				mu.Lock()
				rows = append(rows, row{c.Name(), c.Labels["com.docker.compose.project"], st})
				mu.Unlock()
			}(c)
		}
		wg.Wait()
	}
	if ctx.Err() != nil {
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	_, _ = tx.ExecContext(ctx, `INSERT OR REPLACE INTO metrics(ts,name,project,cpu,mem,mem_limit,disk,net_rx,net_tx) VALUES(?,?,?,?,?,?,?,?,?)`,
		ts, "host", "", hs.CPUPercent, hs.MemUsed, hs.MemTotal, hs.DiskUsed, hs.DiskTotal, 0)
	for _, r := range rows {
		_, _ = tx.ExecContext(ctx, `INSERT OR REPLACE INTO metrics(ts,name,project,cpu,mem,mem_limit,net_rx,net_tx,blk_r,blk_w) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			ts, r.name, r.project, r.st.CPUPercent, r.st.MemUsage, r.st.MemLimit, r.st.NetRx, r.st.NetTx, r.st.BlockRead, r.st.BlockWrite)
	}
	_ = tx.Commit()
}

// MetricSeries is one downsampled series. Rates are bytes/second.
type MetricSeries struct {
	Name     string    `json:"name"`
	TS       []int64   `json:"ts"`
	CPU      []float64 `json:"cpu"`
	Mem      []int64   `json:"mem"`
	MemLimit []int64   `json:"memLimit"`
	NetRx    []float64 `json:"netRx"`
	NetTx    []float64 `json:"netTx"`
	BlkR     []float64 `json:"blkR"`
	BlkW     []float64 `json:"blkW"`
	Disk     []int64   `json:"disk,omitempty"`
	DiskSize []int64   `json:"diskSize,omitempty"`
}

func metricsRange(r *http.Request) (from, step int64) {
	now := time.Now().Unix()
	switch r.URL.Query().Get("range") {
	case "6h":
		return now - 6*3600, 120
	case "24h":
		return now - 24*3600, 300
	case "7d":
		return now - 7*24*3600, 1800
	default:
		return now - 3600, 60
	}
}

// loadSeries returns one series per name; where is a SQL condition on the metrics table.
func (s *Server) loadSeries(from, step int64, where string, args ...any) ([]*MetricSeries, error) {
	q := `SELECT name, (ts/?)*? AS b, avg(cpu), avg(mem), max(mem_limit), max(net_rx), max(net_tx), max(blk_r), max(blk_w), max(disk)
		FROM metrics WHERE ts >= ? AND ` + where + ` GROUP BY name, b ORDER BY name, b`
	rows, err := s.db.Query(q, append([]any{step, step, from}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]*MetricSeries{}
	var order []string
	type prevT struct {
		ts             int64
		rx, tx, br, bw int64
	}
	prev := map[string]*prevT{}
	for rows.Next() {
		var name string
		var b, memLimit, rx, tx, br, bw, disk int64
		var cpu, mem float64
		if err := rows.Scan(&name, &b, &cpu, &mem, &memLimit, &rx, &tx, &br, &bw, &disk); err != nil {
			return nil, err
		}
		ser := byName[name]
		if ser == nil {
			ser = &MetricSeries{Name: name}
			byName[name] = ser
			order = append(order, name)
		}
		rate := func(cur, old int64, dt int64) float64 {
			if dt <= 0 || cur < old {
				return 0
			}
			return float64(cur-old) / float64(dt)
		}
		p := prev[name]
		ser.TS = append(ser.TS, b)
		ser.CPU = append(ser.CPU, round2(cpu))
		ser.Mem = append(ser.Mem, int64(mem))
		if name == "host" {
			ser.Disk = append(ser.Disk, disk)
			ser.DiskSize = append(ser.DiskSize, rx) // host row stores disk total in net_rx
			ser.MemLimit = append(ser.MemLimit, memLimit)
			ser.NetRx, ser.NetTx, ser.BlkR, ser.BlkW = append(ser.NetRx, 0), append(ser.NetTx, 0), append(ser.BlkR, 0), append(ser.BlkW, 0)
			continue
		}
		ser.MemLimit = append(ser.MemLimit, memLimit)
		if p == nil {
			ser.NetRx, ser.NetTx, ser.BlkR, ser.BlkW = append(ser.NetRx, 0), append(ser.NetTx, 0), append(ser.BlkR, 0), append(ser.BlkW, 0)
		} else {
			dt := b - p.ts
			ser.NetRx = append(ser.NetRx, round2(rate(rx, p.rx, dt)))
			ser.NetTx = append(ser.NetTx, round2(rate(tx, p.tx, dt)))
			ser.BlkR = append(ser.BlkR, round2(rate(br, p.br, dt)))
			ser.BlkW = append(ser.BlkW, round2(rate(bw, p.bw, dt)))
		}
		prev[name] = &prevT{b, rx, tx, br, bw}
	}
	out := make([]*MetricSeries, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out, rows.Err()
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

func (s *Server) handleHostMetrics(w http.ResponseWriter, r *http.Request, u *User) error {
	from, step := metricsRange(r)
	ser, err := s.loadSeries(from, step, `name='host'`)
	if err != nil {
		return err
	}
	if len(ser) == 0 {
		return ok(w, &MetricSeries{Name: "host"})
	}
	return ok(w, ser[0])
}

func (s *Server) handleContainerMetrics(w http.ResponseWriter, r *http.Request, u *User) error {
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	from, step := metricsRange(r)
	ser, err := s.loadSeries(from, step, `name=?`, strings.TrimPrefix(c.Name, "/"))
	if err != nil {
		return err
	}
	if len(ser) == 0 {
		return ok(w, &MetricSeries{Name: strings.TrimPrefix(c.Name, "/")})
	}
	return ok(w, ser[0])
}

func (s *Server) handleProjectMetrics(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	from, step := metricsRange(r)
	ser, err := s.loadSeries(from, step, `project=?`, p.Name)
	if err != nil {
		return err
	}
	sort.Slice(ser, func(i, j int) bool { return ser[i].Name < ser[j].Name })
	return ok(w, ser)
}

// ---------- docker event stream alerts ----------

// eventsLoop subscribes to container events for immediate alerts on crashes,
// OOM kills and failing health checks.
func (s *Server) eventsLoop(ctx context.Context) {
	killed := map[string]time.Time{}
	for ctx.Err() == nil {
		err := s.dk.Events(ctx, map[string][]string{
			"type":  {"container"},
			"event": {"die", "oom", "kill", "stop", "health_status", "start"},
		}, func(ev docker.Event) {
			a := ev.Actor.Attributes
			if ev.Action == "start" {
				if isCaddyContainer(a) {
					s.goBackground(s.resyncCaddyAfterStart)
				}
				return
			}
			if a["com.docker.compose.project"] == "" || a["panel.monitor"] == "false" || a["com.docker.compose.oneoff"] == "True" {
				return
			}
			name := a["name"]
			now := time.Now()
			for k, t := range killed {
				if now.Sub(t) > 2*time.Minute {
					delete(killed, k)
				}
			}
			switch {
			case ev.Action == "kill" || ev.Action == "stop":
				killed[ev.Actor.ID] = now
			case ev.Action == "oom":
				s.notify.Emit(notify.Event{Type: "container_oom", Title: "容器内存溢出：" + name,
					Body: "容器被 OOM Killer 终止，请检查内存限制或应用内存占用。项目：" + a["com.docker.compose.project"], Target: name})
			case ev.Action == "die":
				if _, ok := killed[ev.Actor.ID]; ok {
					return // stopped on purpose
				}
				if code := a["exitCode"]; code != "0" && code != "" {
					s.notify.Emit(notify.Event{Type: "container_unhealthy", Title: "容器异常退出：" + name,
						Body: fmt.Sprintf("退出码 %s（项目 %s，镜像 %s）", code, a["com.docker.compose.project"], a["image"]), Target: name})
				}
			case strings.HasPrefix(ev.Action, "health_status") && strings.Contains(ev.Action, "unhealthy"):
				s.notify.Emit(notify.Event{Type: "container_unhealthy", Title: "容器健康检查失败：" + name,
					Body: "健康检查状态变为 unhealthy（项目 " + a["com.docker.compose.project"] + "）", Target: name})
			}
		})
		if ctx.Err() != nil {
			return
		}
		slog.Debug("docker events stream ended", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
