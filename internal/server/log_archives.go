package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	logArchiveMaxLines      = 20000
	logArchiveMaxRawBytes   = int64(20 << 20)
	logArchiveMaxPerGroup   = 15
	logArchiveRetention     = 30 * 24 * time.Hour
	logArchiveMaxTotalBytes = int64(256 << 20)
	logArchiveStageTimeout  = 15 * time.Second
)

type logArchiveDTO struct {
	ID            string `json:"id"`
	Project       string `json:"project"`
	Service       string `json:"service"`
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`
	Image         string `json:"image"`
	Reason        string `json:"reason"`
	StartedAt     int64  `json:"startedAt"`
	ArchivedAt    int64  `json:"archivedAt"`
	SizeBytes     int64  `json:"sizeBytes"`
	RawBytes      int64  `json:"rawBytes"`
	LineCount     int    `json:"lineCount"`
	Truncated     bool   `json:"truncated"`
}

type logArchiveSource struct {
	Project       string
	Service       string
	ContainerID   string
	ContainerName string
	Image         string
	StartedAt     int64
}

type stagedLogArchive struct {
	id         string
	src        logArchiveSource
	reason     string
	archivedAt int64
	sizeBytes  int64
	rawBytes   int64
	lineCount  int
	truncated  bool
	tmpPath    string
	finalPath  string
}

func newLogArchiveID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("cla_%016x", time.Now().UnixNano())
	}
	return "cla_" + hex.EncodeToString(b)
}

func validLogArchiveID(id string) bool {
	if !strings.HasPrefix(id, "cla_") || len(id) <= 4 || len(id) > 64 {
		return false
	}
	for i := 4; i < len(id); i++ {
		c := id[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (s *Server) logArchiveDir() string {
	return filepath.Join(s.cfg.DataDir, "container-logs")
}

func (s *Server) logArchivePath(id string) string {
	return filepath.Join(s.logArchiveDir(), id+".log.gz")
}

func parseDockerStartedAt(raw string) int64 {
	if raw == "" || strings.HasPrefix(raw, "0001-") {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil && t.Unix() > 0 {
		return t.Unix()
	}
	return 0
}

func (s *Server) userCanProjectName(u *User, projectName string) bool {
	if u.Level >= roleAdmin || u.Projects == nil {
		return true
	}
	if projectName == "" {
		return false
	}
	var id int64
	if s.db.QueryRow(`SELECT id FROM projects WHERE name=?`, projectName).Scan(&id) != nil {
		return false
	}
	return u.canProject(id)
}

func (s *Server) containerAlreadyArchived(containerID string) bool {
	var one int
	return s.db.QueryRow(`SELECT 1 FROM container_log_archives WHERE container_id=? LIMIT 1`, containerID).Scan(&one) == nil
}

// stageContainerLog streams up to logArchiveMaxLines / logArchiveMaxRawBytes of container logs
// into a gzip temp file. It returns nil on empty logs, duplicate container_id, or best-effort errors.
func (s *Server) stageContainerLog(ctx context.Context, src logArchiveSource, reason string) *stagedLogArchive {
	if s.dk == nil || src.ContainerID == "" {
		return nil
	}
	if s.containerAlreadyArchived(src.ContainerID) {
		return nil
	}
	dir := s.logArchiveDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		slog.Warn("create container-logs dir failed", "err", err)
		return nil
	}

	sctx, cancel := context.WithTimeout(ctx, logArchiveStageTimeout)
	defer cancel()
	rc, err := s.dk.Logs(sctx, src.ContainerID, strconv.Itoa(logArchiveMaxLines), false, true, 0)
	if err != nil {
		slog.Warn("stage container logs failed", "container", src.ContainerName, "err", err)
		return nil
	}
	defer rc.Close()

	id := newLogArchiveID()
	finalPath := s.logArchivePath(id)
	tmpPath := finalPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		slog.Warn("create staged log archive failed", "path", tmpPath, "err", err)
		return nil
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	gw := gzip.NewWriter(f)
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64<<10), 1<<20)

	var rawBytes int64
	lineCount := 0
	truncated := false
	for sc.Scan() {
		if lineCount >= logArchiveMaxLines {
			truncated = true
			break
		}
		line := strings.TrimRight(sc.Text(), "\r")
		lineBytes := int64(len(line)) + 1
		if rawBytes+lineBytes > logArchiveMaxRawBytes {
			if rawBytes == 0 && logArchiveMaxRawBytes > 1 {
				cut := int(logArchiveMaxRawBytes - 1)
				if cut < len(line) {
					line = line[:cut]
				}
				if _, err := gw.Write([]byte(line)); err != nil {
					_ = gw.Close()
					cleanup()
					return nil
				}
				if _, err := gw.Write([]byte{'\n'}); err != nil {
					_ = gw.Close()
					cleanup()
					return nil
				}
				rawBytes = int64(len(line)) + 1
				lineCount = 1
			}
			truncated = true
			break
		}
		if _, err := gw.Write([]byte(line)); err != nil {
			_ = gw.Close()
			cleanup()
			return nil
		}
		if _, err := gw.Write([]byte{'\n'}); err != nil {
			_ = gw.Close()
			cleanup()
			return nil
		}
		rawBytes += lineBytes
		lineCount++
		if lineCount == logArchiveMaxLines {
			truncated = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("scan container logs incomplete", "container", src.ContainerName, "err", err)
		if lineCount > 0 {
			truncated = true
		}
	}
	if err := gw.Close(); err != nil {
		cleanup()
		return nil
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil
	}
	if lineCount == 0 {
		_ = os.Remove(tmpPath)
		return nil
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil
	}
	return &stagedLogArchive{
		id:         id,
		src:        src,
		reason:     reason,
		archivedAt: time.Now().Unix(),
		sizeBytes:  fi.Size(),
		rawBytes:   rawBytes,
		lineCount:  lineCount,
		truncated:  truncated,
		tmpPath:    tmpPath,
		finalPath:  finalPath,
	}
}

func (s *Server) stageProjectLogs(ctx context.Context, project, service, reason string) []*stagedLogArchive {
	if s.dk == nil || project == "" {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	cs, err := s.dk.ProjectContainers(lctx, project)
	cancel()
	if err != nil {
		slog.Warn("list project containers for log archive failed", "project", project, "err", err)
		return nil
	}
	var out []*stagedLogArchive
	for _, c := range cs {
		if c.Labels["com.docker.compose.oneoff"] == "True" {
			continue
		}
		svc := c.Labels["com.docker.compose.service"]
		if service != "" && svc != service {
			continue
		}
		src := logArchiveSource{
			Project:       project,
			Service:       svc,
			ContainerID:   c.Id,
			ContainerName: c.Name(),
			Image:         c.Image,
		}
		ictx, icancel := context.WithTimeout(ctx, 5*time.Second)
		if ci, err := s.dk.InspectContainer(ictx, c.Id); err == nil {
			src.StartedAt = parseDockerStartedAt(ci.State.StartedAt)
			if ci.Config.Image != "" {
				src.Image = ci.Config.Image
			}
		}
		icancel()
		if st := s.stageContainerLog(ctx, src, reason); st != nil {
			out = append(out, st)
		}
	}
	return out
}

func (s *Server) commitStagedLog(st *stagedLogArchive) error {
	if st == nil {
		return nil
	}
	s.logArchiveMu.Lock()
	defer s.logArchiveMu.Unlock()

	if s.containerAlreadyArchived(st.src.ContainerID) {
		_ = os.Remove(st.tmpPath)
		return nil
	}
	if err := os.Rename(st.tmpPath, st.finalPath); err != nil {
		_ = os.Remove(st.tmpPath)
		return err
	}
	trunc := 0
	if st.truncated {
		trunc = 1
	}
	_, err := s.db.Exec(`INSERT INTO container_log_archives(id, project, service, container_id, container_name, image, reason, started_at, archived_at, size_bytes, raw_bytes, line_count, truncated)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		st.id, st.src.Project, st.src.Service, st.src.ContainerID, st.src.ContainerName, st.src.Image,
		st.reason, st.src.StartedAt, st.archivedAt, st.sizeBytes, st.rawBytes, st.lineCount, trunc)
	if err != nil {
		_ = os.Remove(st.finalPath)
		return err
	}
	return nil
}

func (s *Server) discardStagedLog(st *stagedLogArchive) {
	if st == nil {
		return
	}
	_ = os.Remove(st.tmpPath)
}

func formatLogArchiveBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v < 10 {
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}

// finalizeProjectLogs commits staged archives for containers that disappeared or were replaced,
// and discards staged temp files for containers whose IDs remained untouched.
func (s *Server) finalizeProjectLogs(ctx context.Context, project string, staged []*stagedLogArchive, run *jobRun) {
	if len(staged) == 0 {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cs, err := s.dk.ProjectContainers(fctx, project)
	if err != nil {
		slog.Warn("finalize project log archives: list containers failed", "project", project, "err", err)
		for _, st := range staged {
			s.discardStagedLog(st)
		}
		return
	}
	alive := make(map[string]bool, len(cs))
	for _, c := range cs {
		alive[c.Id] = true
	}
	committed := 0
	for _, st := range staged {
		if alive[st.src.ContainerID] {
			s.discardStagedLog(st)
			continue
		}
		if err := s.commitStagedLog(st); err != nil {
			slog.Warn("commit staged container log archive failed", "container", st.src.ContainerName, "err", err)
			continue
		}
		committed++
		if run != nil {
			label := st.src.Service
			if label == "" {
				label = st.src.ContainerName
			}
			run.Logf("已归档旧容器日志：%s（%d 行，%s）", label, st.lineCount, formatLogArchiveBytes(st.sizeBytes))
		}
	}
	if committed > 0 {
		s.pruneLogArchives()
	}
}

func (s *Server) deleteProjectLogArchives(project string) {
	if project == "" {
		return
	}
	s.logArchiveMu.Lock()
	defer s.logArchiveMu.Unlock()

	rows, err := s.db.Query(`SELECT id FROM container_log_archives WHERE project=?`, project)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	if _, err := s.db.Exec(`DELETE FROM container_log_archives WHERE project=?`, project); err != nil {
		slog.Warn("delete project log archive rows failed", "project", project, "err", err)
		return
	}
	for _, id := range ids {
		_ = os.Remove(s.logArchivePath(id))
	}
}

func logArchiveGroupKey(project, service, containerName string) string {
	if project != "" && service != "" {
		return "svc:" + project + "/" + service
	}
	return "name:" + containerName
}

// pruneLogArchives enforces retention limits: 30-day max age, 15 per service/container group,
// and 256 MiB total compressed size cap (evicting oldest first).
func (s *Server) pruneLogArchives() {
	s.logArchiveMu.Lock()
	defer s.logArchiveMu.Unlock()

	type row struct {
		id         string
		group      string
		archivedAt int64
		sizeBytes  int64
	}
	rows, err := s.db.Query(`SELECT id, project, service, container_name, archived_at, size_bytes FROM container_log_archives ORDER BY archived_at DESC, id DESC`)
	if err != nil {
		return
	}
	var all []row
	for rows.Next() {
		var r row
		var project, service, name string
		if rows.Scan(&r.id, &project, &service, &name, &r.archivedAt, &r.sizeBytes) == nil {
			r.group = logArchiveGroupKey(project, service, name)
			all = append(all, r)
		}
	}
	rows.Close()

	cutoff := time.Now().Add(-logArchiveRetention).Unix()
	groupCount := map[string]int{}
	toDelete := map[string]bool{}
	var kept []row
	var totalBytes int64

	for _, r := range all {
		if r.archivedAt < cutoff {
			toDelete[r.id] = true
			continue
		}
		groupCount[r.group]++
		if groupCount[r.group] > logArchiveMaxPerGroup {
			toDelete[r.id] = true
			continue
		}
		kept = append(kept, r)
		totalBytes += r.sizeBytes
	}

	if totalBytes > logArchiveMaxTotalBytes && len(kept) > 0 {
		sort.Slice(kept, func(i, j int) bool {
			if kept[i].archivedAt != kept[j].archivedAt {
				return kept[i].archivedAt < kept[j].archivedAt
			}
			return kept[i].id < kept[j].id
		})
		for _, r := range kept {
			if totalBytes <= logArchiveMaxTotalBytes {
				break
			}
			toDelete[r.id] = true
			totalBytes -= r.sizeBytes
		}
	}

	for id := range toDelete {
		if _, err := s.db.Exec(`DELETE FROM container_log_archives WHERE id=?`, id); err == nil {
			_ = os.Remove(s.logArchivePath(id))
		}
	}
}

func (s *Server) cleanupLogArchiveOrphans(ctx context.Context) {
	s.logArchiveMu.Lock()
	defer s.logArchiveMu.Unlock()

	known := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM container_log_archives`)
	if err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				known[id] = true
			}
		}
		rows.Close()
	}

	for id := range known {
		if ctx.Err() != nil {
			return
		}
		if _, err := os.Stat(s.logArchivePath(id)); os.IsNotExist(err) {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM container_log_archives WHERE id=?`, id)
			delete(known, id)
		}
	}

	entries, err := os.ReadDir(s.logArchiveDir())
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		name := entry.Name()
		full := filepath.Join(s.logArchiveDir(), name)
		switch {
		case strings.HasSuffix(name, ".tmp"):
			if now.Sub(info.ModTime()) > orphanTempMaxAge {
				_ = os.Remove(full)
			}
		case strings.HasSuffix(name, ".log.gz"):
			id := strings.TrimSuffix(name, ".log.gz")
			if !known[id] && now.Sub(info.ModTime()) > orphanTempMaxAge {
				_ = os.Remove(full)
			}
		}
	}
}

// ---------- HTTP API ----------

const logArchiveSelectCols = `id, project, service, container_id, container_name, image, reason, started_at, archived_at, size_bytes, raw_bytes, line_count, truncated`

func scanLogArchive(sc interface{ Scan(dest ...any) error }) (logArchiveDTO, error) {
	var a logArchiveDTO
	var trunc int
	err := sc.Scan(&a.ID, &a.Project, &a.Service, &a.ContainerID, &a.ContainerName, &a.Image, &a.Reason,
		&a.StartedAt, &a.ArchivedAt, &a.SizeBytes, &a.RawBytes, &a.LineCount, &trunc)
	a.Truncated = trunc == 1
	return a, err
}

func (s *Server) queryLogArchives(where string, args ...any) ([]logArchiveDTO, error) {
	q := `SELECT ` + logArchiveSelectCols + ` FROM container_log_archives`
	if where != "" {
		q += " WHERE " + where
	}
	q += ` ORDER BY archived_at DESC, id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []logArchiveDTO{}
	for rows.Next() {
		a, err := scanLogArchive(rows)
		if err == nil {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *Server) getLogArchiveForUser(id string, u *User) (logArchiveDTO, error) {
	if !validLogArchiveID(id) {
		return logArchiveDTO{}, errNotFound
	}
	a, err := scanLogArchive(s.db.QueryRow(`SELECT `+logArchiveSelectCols+` FROM container_log_archives WHERE id=?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return logArchiveDTO{}, errNotFound
		}
		return logArchiveDTO{}, err
	}
	if !s.userCanProjectName(u, a.Project) {
		return logArchiveDTO{}, errNotFound
	}
	return a, nil
}

func (s *Server) handleListContainerLogArchives(w http.ResponseWriter, r *http.Request, u *User) error {
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	project := c.Config.Labels["com.docker.compose.project"]
	service := c.Config.Labels["com.docker.compose.service"]
	name := strings.TrimPrefix(c.Name, "/")
	var list []logArchiveDTO
	if project != "" && service != "" {
		list, err = s.queryLogArchives(`project=? AND service=?`, project, service)
	} else {
		list, err = s.queryLogArchives(`project='' AND container_name=?`, name)
	}
	if err != nil {
		return err
	}
	return ok(w, list)
}

func (s *Server) handleListProjectLogArchives(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	list, err := s.queryLogArchives(`project=?`, p.Name)
	if err != nil {
		return err
	}
	return ok(w, list)
}

func (s *Server) handleListAllLogArchives(w http.ResponseWriter, r *http.Request, u *User) error {
	list, err := s.queryLogArchives("")
	if err != nil {
		return err
	}
	if u.Level >= roleAdmin || u.Projects == nil {
		return ok(w, list)
	}
	allowed := map[string]bool{}
	rows, err := s.db.Query(`SELECT id, name FROM projects`)
	if err == nil {
		for rows.Next() {
			var pid int64
			var name string
			if rows.Scan(&pid, &name) == nil && u.canProject(pid) {
				allowed[name] = true
			}
		}
		rows.Close()
	}
	filtered := []logArchiveDTO{}
	for _, a := range list {
		if a.Project != "" && allowed[a.Project] {
			filtered = append(filtered, a)
		}
	}
	return ok(w, filtered)
}

func sanitizeArchiveDownloadName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "._")
	if s == "" {
		return "container"
	}
	return s
}

func readTailLines(r io.Reader, tail int, all bool) ([]string, int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if all {
		out := []string{}
		for sc.Scan() {
			out = append(out, sc.Text())
		}
		return out, len(out), sc.Err()
	}
	ring := make([]string, tail)
	total := 0
	for sc.Scan() {
		ring[total%tail] = sc.Text()
		total++
	}
	if err := sc.Err(); err != nil {
		return nil, total, err
	}
	if total <= tail {
		return ring[:total], total, nil
	}
	start := total % tail
	out := make([]string, tail)
	copy(out, ring[start:])
	copy(out[tail-start:], ring[:start])
	return out, total, nil
}

func (s *Server) handleGetLogArchive(w http.ResponseWriter, r *http.Request, u *User) error {
	a, err := s.getLogArchiveForUser(r.PathValue("id"), u)
	if err != nil {
		return err
	}
	p := s.logArchivePath(a.ID)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return errStatus(http.StatusNotFound, "归档日志文件不存在")
		}
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("解压归档日志失败：%w", err)
	}
	defer gr.Close()

	if r.URL.Query().Get("download") == "1" {
		ts := time.Unix(a.ArchivedAt, 0).Format("20060102-150405")
		fname := fmt.Sprintf("%s-%s.log", sanitizeArchiveDownloadName(a.ContainerName), ts)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fname))
		_, _ = io.Copy(w, gr)
		return nil
	}

	tailParam := strings.TrimSpace(r.URL.Query().Get("tail"))
	readAll := false
	tailN := 500
	if tailParam == "all" {
		readAll = true
	} else if tailParam != "" {
		if n, err := strconv.Atoi(tailParam); err == nil && n > 0 {
			if n > logArchiveMaxLines {
				n = logArchiveMaxLines
			}
			tailN = n
		}
	}

	lines, total, err := readTailLines(gr, tailN, readAll)
	if err != nil {
		return fmt.Errorf("读取归档日志失败：%w", err)
	}
	return ok(w, map[string]any{
		"archive":    a,
		"lines":      nonNil(lines),
		"totalLines": total,
	})
}

func (s *Server) handleDeleteLogArchive(w http.ResponseWriter, r *http.Request, u *User) error {
	a, err := s.getLogArchiveForUser(r.PathValue("id"), u)
	if err != nil {
		return err
	}
	s.logArchiveMu.Lock()
	_, delErr := s.db.Exec(`DELETE FROM container_log_archives WHERE id=?`, a.ID)
	if delErr == nil {
		_ = os.Remove(s.logArchivePath(a.ID))
	}
	s.logArchiveMu.Unlock()
	s.audit(r, u, "container.log_archive_delete", a.ContainerName, a.ID, delErr)
	return done(w, delErr)
}
