package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compose-panel/internal/proxy"
)

// ---------- maintenance mode during deploys ----------

func (s *Server) inDeployMaintenance(projectID int64) bool {
	s.maintMu.Lock()
	defer s.maintMu.Unlock()
	return s.deployMaint[projectID]
}

// enterDeployMaintenance turns on the maintenance page for sites of the
// project that opted in. It reports whether any site was affected.
func (s *Server) enterDeployMaintenance(ctx context.Context, projectID int64) bool {
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM sites WHERE project_id=? AND enabled=1 AND maintenance_deploy=1`, projectID).Scan(&n)
	if n == 0 {
		return false
	}
	s.maintMu.Lock()
	if s.deployMaint == nil {
		s.deployMaint = map[int64]bool{}
	}
	s.deployMaint[projectID] = true
	s.maintMu.Unlock()
	_ = s.syncCaddy(ctx)
	return true
}

func (s *Server) leaveDeployMaintenance(projectID int64) {
	s.maintMu.Lock()
	delete(s.deployMaint, projectID)
	s.maintMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = s.syncCaddy(ctx)
}

func (s *Server) handleSiteMaintenance(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	x, err := s.siteForUser(id, u)
	if err != nil {
		return err
	}
	var req struct {
		On      bool    `json:"on"`
		Message *string `json:"message"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	msg := x.MaintenanceMsg
	if req.Message != nil {
		msg = *req.Message
	}
	_, err = s.db.Exec(`UPDATE sites SET maintenance=?, maintenance_msg=? WHERE id=?`, boolInt(req.On), msg, id)
	state := "关闭"
	if req.On {
		state = "开启"
	}
	s.audit(r, u, "site.maintenance", strings.Join(x.Domains, ","), state, err)
	if err != nil {
		return err
	}
	out := map[string]any{"ok": true}
	if err := s.syncCaddy(r.Context()); err != nil {
		out["warning"] = "推送到 Caddy 失败：" + err.Error()
	}
	return ok(w, out)
}

func (s *Server) siteForUser(id int64, u *User) (*siteRow, error) {
	list, err := s.listSites(`WHERE s.id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 || (list[0].ProjectID > 0 && !u.canProject(list[0].ProjectID)) {
		return nil, errNotFound
	}
	return &list[0], nil
}

// ---------- load balancing ----------

// replicaUpstreams lists every running container of the site's service.
func (s *Server) replicaUpstreams(x siteRow) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := s.dk.ListContainers(ctx, false, map[string][]string{"label": {
		"com.docker.compose.project=" + x.ProjectName, "com.docker.compose.service=" + x.Service}})
	if err != nil || len(cs) == 0 {
		return nil
	}
	var out []string
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s:%d", c.Name(), x.Port))
	}
	sort.Strings(out)
	return out
}

// ---------- access logs ----------

type accessEntry struct {
	TS       float64 `json:"ts"`
	IP       string  `json:"ip"`
	Method   string  `json:"method"`
	Host     string  `json:"host"`
	URI      string  `json:"uri"`
	Proto    string  `json:"proto"`
	Status   int     `json:"status"`
	Size     int64   `json:"size"`
	Duration float64 `json:"durationMs"`
	UA       string  `json:"ua"`
	Referer  string  `json:"referer"`
}

type rawAccess struct {
	TS      float64 `json:"ts"`
	Request struct {
		RemoteIP string              `json:"remote_ip"`
		ClientIP string              `json:"client_ip"`
		Proto    string              `json:"proto"`
		Method   string              `json:"method"`
		Host     string              `json:"host"`
		URI      string              `json:"uri"`
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
	Duration float64 `json:"duration"`
	Size     int64   `json:"size"`
	Status   int     `json:"status"`
}

func (s *Server) accessLogDir() string {
	return filepath.Join(filepath.Dir(s.cfg.CaddyDataDir), filepath.Base(proxy.AccessLogDir))
}

// accessLogFiles returns the site's log files, newest first.
func (s *Server) accessLogFiles(id int64) []string {
	dir := s.accessLogDir()
	cur := filepath.Join(dir, fmt.Sprintf("site-%d.log", id))
	rotated, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("site-%d-*.log*", id)))
	sort.Sort(sort.Reverse(sort.StringSlice(rotated)))
	out := []string{}
	if _, err := os.Stat(cur); err == nil {
		out = append(out, cur)
	}
	return append(out, rotated...)
}

type accessFilter struct {
	status string // "", "2xx".."5xx", or an exact code
	path   string
	ip     string
	since  float64
}

func (f accessFilter) match(e *accessEntry) bool {
	if f.since > 0 && e.TS < f.since {
		return false
	}
	if f.status != "" {
		if strings.HasSuffix(f.status, "xx") {
			if strconv.Itoa(e.Status/100) != f.status[:1] {
				return false
			}
		} else if strconv.Itoa(e.Status) != f.status {
			return false
		}
	}
	if f.path != "" && !strings.Contains(e.URI, f.path) {
		return false
	}
	if f.ip != "" && !strings.HasPrefix(e.IP, f.ip) {
		return false
	}
	return true
}

func scanAccessFile(path string, fn func(*accessEntry)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var rd io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		rd = zr
	}
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 256<<10), 4<<20)
	for sc.Scan() {
		var r rawAccess
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		e := &accessEntry{TS: r.TS, IP: r.Request.ClientIP, Method: r.Request.Method, Host: r.Request.Host, URI: r.Request.URI, Proto: r.Request.Proto,
			Status: r.Status, Size: r.Size, Duration: round2(r.Duration * 1000)}
		if e.IP == "" {
			e.IP = r.Request.RemoteIP
		}
		if ua := r.Request.Headers["User-Agent"]; len(ua) > 0 {
			e.UA = ua[0]
		}
		if ref := r.Request.Headers["Referer"]; len(ref) > 0 {
			e.Referer = ref[0]
		}
		fn(e)
	}
	return sc.Err()
}

type kv struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

func topN(m map[string]int, n int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// handleSiteAccessLogs returns the most recent matching entries plus a summary
// of all matches within the time window.
func (s *Server) handleSiteAccessLogs(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	x, err := s.siteForUser(id, u)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	hours := queryInt(r, "hours", 24)
	if hours <= 0 || hours > 24*7 {
		hours = 24
	}
	f := accessFilter{status: strings.ToLower(strings.TrimSpace(q.Get("status"))), path: strings.TrimSpace(q.Get("path")), ip: strings.TrimSpace(q.Get("ip")),
		since: float64(time.Now().Add(-time.Duration(hours) * time.Hour).Unix())}
	limit := min(max(queryInt(r, "limit", 200), 1), 1000)

	var recent []*accessEntry // ring of the newest matches
	total, sumDur := 0, 0.0
	classes := map[string]int{}
	paths, ips := map[string]int{}, map[string]int{}
	files := s.accessLogFiles(id)
	// Read oldest → newest so the ring ends with the latest entries.
	for i := len(files) - 1; i >= 0; i-- {
		if fi, err := os.Stat(files[i]); err == nil && fi.ModTime().Unix() < int64(f.since) {
			continue
		}
		_ = scanAccessFile(files[i], func(e *accessEntry) {
			if !f.match(e) {
				return
			}
			total++
			sumDur += e.Duration
			classes[fmt.Sprintf("%dxx", e.Status/100)]++
			p, _, _ := strings.Cut(e.URI, "?")
			paths[p]++
			ips[e.IP]++
			recent = append(recent, e)
			if len(recent) > limit*2 {
				recent = append(recent[:0], recent[len(recent)-limit:]...)
			}
		})
	}
	if len(recent) > limit {
		recent = recent[len(recent)-limit:]
	}
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	avg := 0.0
	if total > 0 {
		avg = round2(sumDur / float64(total))
	}
	return ok(w, map[string]any{
		"enabled": x.AccessLog, "entries": recent, "total": total, "avgMs": avg,
		"classes": classes, "topPaths": topN(paths, 10), "topIps": topN(ips, 10), "files": len(files),
	})
}
