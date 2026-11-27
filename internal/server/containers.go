package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"compose-panel/internal/docker"
)

// containerFor loads a container and checks the user may see its project.
func (s *Server) containerFor(r *http.Request, u *User) (*docker.ContainerJSON, error) {
	c, err := s.dk.InspectContainer(r.Context(), r.PathValue("id"))
	if err != nil {
		if docker.IsNotFound(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	if !s.userCanContainer(u, c.Config.Labels) {
		return nil, errNotFound
	}
	return c, nil
}

func (s *Server) userCanContainer(u *User, labels map[string]string) bool {
	if u.Level >= roleAdmin || u.Projects == nil {
		return true
	}
	name := labels["com.docker.compose.project"]
	if name == "" {
		return false
	}
	var id int64
	if s.db.QueryRow(`SELECT id FROM projects WHERE name=?`, name).Scan(&id) != nil {
		return false
	}
	return u.canProject(id)
}

func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request, u *User) error {
	cs, err := s.dk.ListContainers(r.Context(), true, nil)
	if err != nil {
		return err
	}
	out := []containerDTO{}
	for _, c := range cs {
		if s.userCanContainer(u, c.Labels) {
			out = append(out, toContainerDTO(c))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Name < out[j].Name
	})
	return ok(w, out)
}

func (s *Server) handleGetContainer(w http.ResponseWriter, r *http.Request, u *User) error {
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	env := []string{}
	for _, e := range c.Config.Env {
		k, v, _ := strings.Cut(e, "=")
		if !u.can(roleOperator) || looksSecret(k) {
			v = "••••••"
		}
		env = append(env, k+"="+v)
	}
	health := ""
	if c.State.Health != nil {
		health = c.State.Health.Status
	}
	nets := []map[string]any{}
	for n, v := range c.NetworkSettings.Networks {
		nets = append(nets, map[string]any{"name": n, "ip": v.IPAddress, "aliases": nonNil(v.Aliases)})
	}
	mounts := []map[string]any{}
	for _, m := range c.Mounts {
		mounts = append(mounts, map[string]any{"type": m.Type, "name": m.Name, "source": m.Source, "destination": m.Destination, "rw": m.RW})
	}
	return ok(w, map[string]any{
		"id": c.Id, "name": strings.TrimPrefix(c.Name, "/"), "image": c.Config.Image, "created": c.Created,
		"state": c.State.Status, "running": c.State.Running, "exitCode": c.State.ExitCode, "oomKilled": c.State.OOMKilled,
		"startedAt": c.State.StartedAt, "finishedAt": c.State.FinishedAt, "restartCount": c.RestartCount, "health": health,
		"restartPolicy": c.HostConfig.RestartPolicy.Name, "memoryLimit": c.HostConfig.Memory, "nanoCpus": c.HostConfig.NanoCpus,
		"cmd": c.Config.Cmd, "entrypoint": c.Config.Entrypoint, "env": env, "labels": c.Config.Labels,
		"project": c.Config.Labels["com.docker.compose.project"], "service": c.Config.Labels["com.docker.compose.service"],
		"networks": nets, "mounts": mounts, "tty": c.Config.Tty,
	})
}

func looksSecret(k string) bool {
	k = strings.ToUpper(k)
	for _, w := range []string{"PASS", "SECRET", "TOKEN", "KEY", "PWD", "CREDENTIAL", "PRIVATE"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

func (s *Server) handleContainerStats(w http.ResponseWriter, r *http.Request, u *User) error {
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	if !c.State.Running {
		return ok(w, nil)
	}
	st, err := s.dk.ContainerStats(r.Context(), c.Id)
	if err != nil {
		return err
	}
	return ok(w, st)
}

// handleAllStats returns stats for all running containers (used by the containers list).
func (s *Server) handleAllStats(w http.ResponseWriter, r *http.Request, u *User) error {
	cs, err := s.dk.ListContainers(r.Context(), false, nil)
	if err != nil {
		return err
	}
	type res struct {
		id string
		st *docker.Stats
	}
	ch := make(chan res, len(cs))
	sem := make(chan struct{}, 8)
	n := 0
	for _, c := range cs {
		if !s.userCanContainer(u, c.Labels) {
			continue
		}
		n++
		go func(id string) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				ch <- res{id, nil}
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			st, _ := s.dk.ContainerStats(ctx, id)
			ch <- res{id, st}
		}(c.Id)
	}
	out := map[string]*docker.Stats{}
	for i := 0; i < n; i++ {
		x := <-ch
		if x.st != nil {
			out[x.id] = x.st
		}
	}
	return ok(w, out)
}

func (s *Server) handleContainerAction(w http.ResponseWriter, r *http.Request, u *User) error {
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	action := r.PathValue("action")
	switch action {
	case "start", "stop", "restart", "kill", "pause", "unpause":
		err = s.dk.ContainerAction(r.Context(), c.Id, action)
	case "remove":
		staged := s.stageContainerLog(r.Context(), logArchiveSource{
			Project:       c.Config.Labels["com.docker.compose.project"],
			Service:       c.Config.Labels["com.docker.compose.service"],
			ContainerID:   c.Id,
			ContainerName: strings.TrimPrefix(c.Name, "/"),
			Image:         c.Config.Image,
			StartedAt:     parseDockerStartedAt(c.State.StartedAt),
		}, "remove")
		err = s.dk.RemoveContainer(r.Context(), c.Id, true)
		if err == nil {
			if cerr := s.commitStagedLog(staged); cerr == nil && staged != nil {
				s.pruneLogArchives()
			}
		} else {
			s.discardStagedLog(staged)
		}
	default:
		return bad("未知操作")
	}
	s.audit(r, u, "container."+action, strings.TrimPrefix(c.Name, "/"), "", err)
	return done(w, err)
}

// handleContainerLogs streams logs as Server-Sent Events, or plain text with ?download=1.
func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request, u *User) error {
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	tail := q.Get("tail")
	if tail == "" {
		tail = "500"
	}
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	if q.Get("download") == "1" {
		rc, err := s.dk.Logs(r.Context(), c.Id, "all", false, true, since)
		if err != nil {
			return err
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.log"`, strings.TrimPrefix(c.Name, "/"), time.Now().Format("20060102-150405")))
		_, _ = io.Copy(w, rc)
		return nil
	}
	var cursor *logCursor
	rawCursor := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if rawCursor == "" {
		rawCursor = strings.TrimSpace(q.Get("after"))
	}
	if rawCursor != "" {
		if ts, seq, ok := parseLogCursor(rawCursor); ok {
			cursor = &logCursor{ts: ts, seq: seq}
			since = ts.Unix()
			if since < 0 {
				since = 0
			}
			tail = "all"
		}
	}
	follow := q.Get("follow") != "0"
	rc, err := s.dk.Logs(r.Context(), c.Id, tail, follow && c.State.Running, true, since)
	if err != nil {
		return err
	}
	defer rc.Close()
	return streamContainerLogs(w, r, rc, cursor)
}

type logCursor struct {
	ts  time.Time
	seq int
}

// extractLogTimestamp parses a leading Docker RFC3339Nano timestamp token from a log line.
func extractLogTimestamp(line string) (time.Time, string, bool) {
	tok, _, _ := strings.Cut(line, " ")
	if len(tok) < 20 {
		return time.Time{}, "", false
	}
	ts, err := time.Parse(time.RFC3339Nano, tok)
	if err != nil {
		return time.Time{}, "", false
	}
	return ts, tok, true
}

// parseLogCursor parses an SSE resume cursor formatted as "<RFC3339Nano>|<seq>" (seq >= 1).
func parseLogCursor(raw string) (time.Time, int, bool) {
	raw = strings.TrimSpace(raw)
	tsPart, seqPart, ok := strings.Cut(raw, "|")
	if !ok || tsPart == "" || seqPart == "" {
		return time.Time{}, 0, false
	}
	ts, err := time.Parse(time.RFC3339Nano, tsPart)
	if err != nil {
		return time.Time{}, 0, false
	}
	seq, err := strconv.Atoi(seqPart)
	if err != nil || seq < 1 {
		return time.Time{}, 0, false
	}
	return ts, seq, true
}

// streamLines sends each line of rc as an SSE "data:" event.
func streamLines(w http.ResponseWriter, r *http.Request, rc io.Reader) error {
	return streamSSELines(w, r, rc, false, nil)
}

// streamContainerLogs streams Docker timestamped container logs with SSE "id:" cursors
// and optional nanosecond-accurate resume deduplication when cursor is non-nil.
func streamContainerLogs(w http.ResponseWriter, r *http.Request, rc io.Reader, cursor *logCursor) error {
	return streamSSELines(w, r, rc, true, cursor)
}

func streamSSELines(w http.ResponseWriter, r *http.Request, rc io.Reader, emitCursor bool, cursor *logCursor) error {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	// Write a tiny SSE comment so reverse proxies (such as Caddy's encode middleware) flush headers immediately.
	_, _ = fmt.Fprint(w, ": ok\n\n")
	if fl != nil {
		fl.Flush()
	}
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-r.Context().Done():
				return
			}
		}
	}()
	passedBoundary := cursor == nil
	sameCursorSeen := 0
	var lastTS time.Time
	lastSeq := 0
	if cursor != nil {
		lastTS = cursor.ts
		lastSeq = cursor.seq
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	pending := 0
	flush := time.NewTimer(time.Hour)
	defer flush.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case l, ok := <-lines:
			if !ok {
				fmt.Fprint(w, "event: end\ndata: \n\n")
				if fl != nil {
					fl.Flush()
				}
				return nil
			}
			eventID := ""
			if emitCursor {
				ts, tsStr, hasTS := extractLogTimestamp(l)
				if !passedBoundary {
					if !hasTS || ts.Before(cursor.ts) {
						continue
					}
					if ts.Equal(cursor.ts) {
						sameCursorSeen++
						if sameCursorSeen == cursor.seq {
							passedBoundary = true
						}
						continue
					}
					passedBoundary = true
				}
				if hasTS {
					switch {
					case lastTS.IsZero() || ts.After(lastTS):
						lastTS = ts
						lastSeq = 1
						eventID = tsStr + "|1"
					case ts.Equal(lastTS):
						lastSeq++
						eventID = tsStr + "|" + strconv.Itoa(lastSeq)
					}
				}
			}
			writeSSEWithID(w, eventID, l)
			pending++
			if pending > 200 {
				if fl != nil {
					fl.Flush()
				}
				pending = 0
			} else {
				flush.Reset(50 * time.Millisecond)
			}
		case <-flush.C:
			if fl != nil {
				fl.Flush()
			}
			pending = 0
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func writeSSEWithID(w io.Writer, id, line string) {
	if id != "" {
		fmt.Fprintf(w, "id: %s\n", id)
	}
	writeSSE(w, line)
}

func writeSSE(w io.Writer, line string) {
	line = strings.TrimRight(line, "\r")
	for _, part := range strings.Split(line, "\r") {
		fmt.Fprintf(w, "data: %s\n", part)
	}
	fmt.Fprint(w, "\n")
}

var (
	wsPingInterval   = 30 * time.Second
	wsIdleTimeout    = 30 * time.Minute
	termProbeTimeout = 300 * time.Millisecond
)

func normalizeHostPort(hostPort, scheme string) string {
	h, p, err := net.SplitHostPort(hostPort)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(hostPort))
	}
	if (scheme == "http" && p == "80") || (scheme == "https" && p == "443") {
		return strings.ToLower(h)
	}
	return strings.ToLower(net.JoinHostPort(h, p))
}

func validWSOrigin(r *http.Request) (originHost string, ok bool) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return "", true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	expectedScheme := "http"
	if r.TLS != nil {
		expectedScheme = "https"
	} else if isTrustedProxyAddr(r.RemoteAddr) {
		if xf := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); xf == "https" || xf == "http" {
			expectedScheme = xf
		}
	}
	if scheme != expectedScheme {
		return "", false
	}
	if normalizeHostPort(u.Host, scheme) != normalizeHostPort(r.Host, expectedScheme) {
		return "", false
	}
	return u.Host, true
}

func clampTermSize(v int) int {
	if v < 1 {
		return 1
	}
	if v > 500 {
		return 500
	}
	return v
}

type missingShellError struct {
	shell string
	raw   string
	tried []string
}

func (e *missingShellError) Error() string { return e.raw }

func terminalShellCandidates(shell string) [][]string {
	switch shell {
	case "sh":
		return [][]string{{"/bin/sh"}}
	case "bash":
		return [][]string{{"bash"}}
	default:
		return [][]string{
			{"/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"},
			{"/bin/bash"},
			{"/busybox/sh"},
			{"/bin/ash"},
		}
	}
}

func isMissingShellText(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "no such file or directory") ||
		strings.Contains(low, "executable file not found") ||
		strings.Contains(low, "not found in $path")
}

func probeTerminalStart(conn *docker.HijackedConn) ([]byte, bool) {
	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(termProbeTimeout))
	n, err := conn.Read(buf)
	if n == 0 {
		_ = conn.SetReadDeadline(time.Time{})
		return nil, false
	}
	collected := append([]byte(nil), buf[:n]...)
	if strings.Contains(string(collected), "OCI runtime exec failed") || isMissingShellText(string(collected)) {
		if err == nil {
			_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			for len(collected) < 4096 {
				m, rerr := conn.Read(buf)
				if m > 0 {
					collected = append(collected, buf[:m]...)
				}
				if rerr != nil {
					break
				}
			}
		}
		_ = conn.SetReadDeadline(time.Time{})
		return collected, isMissingShellText(string(collected))
	}
	_ = conn.SetReadDeadline(time.Time{})
	return collected, false
}

func (s *Server) openTerminalSession(ctx context.Context, containerID, shell, user string) (*docker.HijackedConn, string, []byte, string, error) {
	candidates := terminalShellCandidates(shell)
	var firstMissing string
	tried := make([]string, 0, len(candidates))
	for _, cmd := range candidates {
		tried = append(tried, cmd[0])
		conn, execID, err := s.dk.ExecTTY(ctx, containerID, cmd, user)
		if err != nil {
			if isMissingShellText(err.Error()) {
				if firstMissing == "" {
					firstMissing = strings.TrimSpace(err.Error())
				}
				continue
			}
			return nil, "", nil, "", err
		}
		initial, missing := probeTerminalStart(conn)
		if missing {
			_ = conn.Close()
			if firstMissing == "" {
				firstMissing = strings.TrimSpace(string(initial))
			}
			continue
		}
		return conn, execID, initial, cmd[0], nil
	}
	return nil, "", nil, "", &missingShellError{shell: shell, raw: firstMissing, tried: tried}
}

func formatMissingShellMessage(e *missingShellError) string {
	var hint string
	switch e.shell {
	case "bash":
		hint = "容器内未找到 bash，请切换为「自动（bash/sh）」或「sh」重试；若为 scratch / distroless 极简镜像，可通过「文件」标签页查看容器文件。"
	case "sh":
		hint = "容器内未找到 /bin/sh，请切换为「自动（bash/sh）」重试；若为 scratch / distroless 极简镜像，可通过「文件」标签页查看容器文件。"
	default:
		hint = fmt.Sprintf("容器内未找到可用的 Shell（已尝试 %s）。\r\n该镜像可能基于 scratch 或 distroless 极简镜像构建，不包含交互式终端；如需查看容器内文件，请切换到「文件」标签页。", strings.Join(e.tried, "、"))
	}
	return e.raw + "\r\n\r\n\x1b[33m" + hint + "\x1b[0m\r\n"
}

// handleTerminal bridges a websocket to an interactive docker exec session.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request, u *User) error {
	originHost, originOK := validWSOrigin(r)
	if !originOK {
		return errForbidden
	}
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	if !c.State.Running {
		return bad("容器未运行")
	}
	shell := r.URL.Query().Get("shell")
	user := r.URL.Query().Get("user")
	acceptOpts := &websocket.AcceptOptions{}
	if originHost != "" {
		acceptOpts.OriginPatterns = []string{originHost}
	}
	ws, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		return nil
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn, execID, initial, usedShell, err := s.openTerminalSession(ctx, c.Id, shell, user)
	if err != nil {
		var mse *missingShellError
		if errors.As(err, &mse) {
			_ = ws.Write(ctx, websocket.MessageText, []byte(formatMissingShellMessage(mse)))
			ws.Close(websocket.StatusNormalClosure, "no shell")
			return nil
		}
		_ = ws.Write(ctx, websocket.MessageText, []byte("\r\n\x1b[31m无法启动终端："+err.Error()+"\x1b[0m\r\n"))
		ws.Close(websocket.StatusInternalError, "exec failed")
		return nil
	}
	defer conn.Close()
	if len(initial) > 0 {
		if werr := ws.Write(ctx, websocket.MessageBinary, initial); werr != nil {
			return nil
		}
	}
	name := strings.TrimPrefix(c.Name, "/")
	shellLabel := shell
	if shellLabel == "" || shellLabel == "auto" {
		if usedShell != "" && usedShell != "/bin/sh" {
			shellLabel = "auto(" + usedShell + ")"
		} else {
			shellLabel = "auto"
		}
	}
	userLabel := user
	if userLabel == "" {
		userLabel = "default"
	}
	meta := fmt.Sprintf("shell=%s user=%s", shellLabel, userLabel)
	s.audit(r, u, "container.terminal", name, "打开终端（"+meta+"）", nil)
	start := time.Now()
	defer func() {
		s.audit(r, u, "container.terminal", name, fmt.Sprintf("关闭终端（%s），时长 %s", meta, time.Since(start).Round(time.Second)), nil)
	}()

	go func() {
		t := time.NewTicker(wsPingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				err := ws.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		rctx, rcancel := context.WithTimeout(ctx, wsIdleTimeout)
		typ, data, err := ws.Read(rctx)
		rcancel()
		if err != nil {
			break
		}
		if typ == websocket.MessageText && len(data) > 0 && data[0] == '{' {
			var msg struct {
				Type string `json:"type"`
				Data string `json:"data"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
			}
			if json.Unmarshal(data, &msg) == nil {
				switch msg.Type {
				case "resize":
					_ = s.dk.ExecResize(ctx, execID, clampTermSize(msg.Rows), clampTermSize(msg.Cols))
					continue
				case "input":
					data = []byte(msg.Data)
				}
			}
		}
		if _, err := conn.Write(data); err != nil {
			break
		}
	}
	ws.Close(websocket.StatusNormalClosure, "")
	return nil
}

// ---------- images / volumes / networks ----------

func (s *Server) handleDockerDF(w http.ResponseWriter, r *http.Request, u *User) error {
	df, err := s.dk.DiskUsage(r.Context())
	if err != nil {
		return err
	}
	var images, imagesUnused, volumes, volumesUnused, build int64
	used := map[string]bool{}
	cs, _ := s.dk.ListContainers(r.Context(), true, nil)
	for _, c := range cs {
		used[c.ImageID] = true
	}
	for _, im := range df.Images {
		images += im.Size
		if !used[im.Id] {
			imagesUnused += im.Size
		}
	}
	for _, v := range df.Volumes {
		if v.UsageData.Size > 0 {
			volumes += v.UsageData.Size
			if v.UsageData.RefCount == 0 {
				volumesUnused += v.UsageData.Size
			}
		}
	}
	var buildUnused int64
	for _, b := range df.BuildCache {
		build += b.Size
		if !b.InUse {
			buildUnused += b.Size
		}
	}
	var running int
	var stoppedSize int64
	for _, c := range df.Containers {
		if c.State == "running" {
			running++
		} else {
			stoppedSize += c.SizeRw
		}
	}
	return ok(w, map[string]any{
		"images": images, "imagesReclaimable": imagesUnused, "imageCount": len(df.Images),
		"volumes": volumes, "volumesReclaimable": volumesUnused, "volumeCount": len(df.Volumes),
		"buildCache": build, "buildCacheReclaimable": buildUnused, "buildCacheCount": len(df.BuildCache),
		"containers": len(df.Containers), "containersRunning": running, "containersReclaimable": stoppedSize,
	})
}

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request, u *User) error {
	ims, err := s.dk.ListImages(r.Context())
	if err != nil {
		return err
	}
	cs, _ := s.dk.ListContainers(r.Context(), true, nil)
	usedBy := map[string][]string{}
	for _, c := range cs {
		usedBy[c.ImageID] = append(usedBy[c.ImageID], c.Name())
	}
	out := []map[string]any{}
	for _, im := range ims {
		out = append(out, map[string]any{"id": im.Id, "tags": nonNil(im.RepoTags), "created": im.Created, "size": im.Size, "usedBy": nonNil(usedBy[im.Id])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["created"].(int64) > out[j]["created"].(int64) })
	return ok(w, out)
}

func (s *Server) handleRemoveImage(w http.ResponseWriter, r *http.Request, u *User) error {
	id := r.PathValue("id")
	err := s.dk.RemoveImage(r.Context(), id, r.URL.Query().Get("force") == "1")
	s.audit(r, u, "image.remove", id, "", err)
	return done(w, err)
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request, u *User) error {
	vols, err := s.dk.ListVolumes(r.Context())
	if err != nil {
		return err
	}
	sizes := map[string][2]int64{}
	if df, err := s.dk.DiskUsage(r.Context()); err == nil {
		for _, v := range df.Volumes {
			sizes[v.Name] = [2]int64{v.UsageData.Size, v.UsageData.RefCount}
		}
	}
	usedBy := map[string][]string{}
	cs, _ := s.dk.ListContainers(r.Context(), true, nil)
	for _, c := range cs {
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" {
				usedBy[m.Name] = append(usedBy[m.Name], c.Name())
			}
		}
	}
	out := []map[string]any{}
	for _, v := range vols {
		sz := sizes[v.Name]
		out = append(out, map[string]any{"name": v.Name, "driver": v.Driver, "created": v.CreatedAt, "mountpoint": v.Mountpoint,
			"project": v.Labels["com.docker.compose.project"], "size": sz[0], "usedBy": nonNil(usedBy[v.Name])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return ok(w, out)
}

func (s *Server) handleRemoveVolume(w http.ResponseWriter, r *http.Request, u *User) error {
	name := r.PathValue("name")
	err := s.dk.RemoveVolume(r.Context(), name)
	s.audit(r, u, "volume.remove", name, "", err)
	return done(w, err)
}

func (s *Server) handleListNetworks(w http.ResponseWriter, r *http.Request, u *User) error {
	nets, err := s.dk.ListNetworks(r.Context())
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, n := range nets {
		full, err := s.dk.InspectNetwork(r.Context(), n.Id)
		if err != nil {
			full = &n
		}
		members := []string{}
		for _, c := range full.Containers {
			members = append(members, c.Name)
		}
		sort.Strings(members)
		subnet := ""
		if len(n.IPAM.Config) > 0 {
			subnet = n.IPAM.Config[0].Subnet
		}
		out = append(out, map[string]any{"id": n.Id, "name": n.Name, "driver": n.Driver, "scope": n.Scope, "internal": n.Internal,
			"subnet": subnet, "containers": members, "project": n.Labels["com.docker.compose.project"],
			"system": n.Name == "bridge" || n.Name == "host" || n.Name == "none",
			"panel":  n.Name == s.cfg.ProxyNetwork || n.Name == s.cfg.DBNetwork})
	}
	return ok(w, out)
}

func (s *Server) handleRemoveNetwork(w http.ResponseWriter, r *http.Request, u *User) error {
	id := r.PathValue("id")
	err := s.dk.RemoveNetwork(r.Context(), id)
	s.audit(r, u, "network.remove", id, "", err)
	return done(w, err)
}

func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Kind string `json:"kind"`
		All  bool   `json:"all"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	var f map[string][]string
	if req.Kind == "images" {
		f = map[string][]string{"dangling": {strconv.FormatBool(!req.All)}}
	}
	if req.Kind == "volumes" && req.All {
		f = map[string][]string{"all": {"true"}}
	}
	rep, err := s.dk.Prune(r.Context(), req.Kind, f)
	s.audit(r, u, "docker.prune", req.Kind, fmt.Sprintf("释放 %d 字节", rep.SpaceReclaimed), err)
	if err != nil {
		return err
	}
	return ok(w, rep)
}

// ---------- registries ----------

func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request, u *User) error {
	rows, err := s.db.Query(`SELECT id, host, username FROM registries ORDER BY host`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var host, user string
		if rows.Scan(&id, &host, &user) == nil {
			out = append(out, map[string]any{"id": id, "host": host, "username": user})
		}
	}
	return ok(w, out)
}

func (s *Server) handleSaveRegistry(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct{ Host, Username, Password string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	req.Host = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(req.Host, "https://"), "http://"))
	if req.Host == "" || req.Username == "" || req.Password == "" {
		return bad("请填写地址、用户名和密码")
	}
	_, err := s.db.Exec(`INSERT INTO registries(host,username,password) VALUES(?,?,?) ON CONFLICT(host) DO UPDATE SET username=excluded.username, password=excluded.password`,
		req.Host, req.Username, s.box.Seal(req.Password))
	s.audit(r, u, "registry.save", req.Host, "", err)
	return done(w, err)
}

func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM registries WHERE id=?`, id)
	s.audit(r, u, "registry.delete", fmt.Sprint(id), "", err)
	return done(w, err)
}
