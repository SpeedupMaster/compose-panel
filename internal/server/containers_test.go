package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"compose-panel/internal/config"
)

func TestStreamLinesSSEHeadersAndImmediateFlush(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = streamLines(w, r, pr)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-transform") || !strings.Contains(got, "no-cache") {
		t.Fatalf("Cache-Control = %q, want no-cache and no-transform", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}

	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString initial comment: %v", err)
	}
	if line != ": ok\n" {
		t.Fatalf("initial SSE line = %q, want %q", line, ": ok\\n")
	}

	_, _ = io.WriteString(pw, "hello log\n")
	_ = pw.Close()

	var sawData, sawEnd bool
	for {
		l, err := br.ReadString('\n')
		if strings.TrimSpace(l) == "data: hello log" {
			sawData = true
		}
		if strings.TrimSpace(l) == "event: end" {
			sawEnd = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadString: %v", err)
		}
	}
	if !sawData || !sawEnd {
		t.Fatalf("sawData=%v sawEnd=%v, want both true", sawData, sawEnd)
	}
}

func TestHandleJobLogNoTransformHeader(t *testing.T) {
	tmp := t.TempDir()
	srv, err := New(config.Config{
		DataDir:     tmp,
		ProjectsDir: tmp,
		BackupsDir:  tmp,
		DockerHost:  "unix:///nonexistent.sock",
	}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { _ = srv.DB().Close() })

	now := time.Now().Unix()
	if _, err := srv.DB().Exec(`INSERT INTO jobs(id, kind, title, status, created_at) VALUES('job-1', 'test', 't', 'running', ?)`, now); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/job-1/log", nil).WithContext(ctx)
	req.SetPathValue("id", "job-1")
	w := httptest.NewRecorder()

	done := make(chan error, 1)
	go func() {
		done <- srv.handleJobLog(w, req, &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin})
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("handleJobLog() error: %v", err)
	}

	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-transform") || !strings.Contains(got, "no-cache") {
		t.Fatalf("Cache-Control = %q, want no-cache and no-transform", got)
	}
	if !strings.Contains(w.Body.String(), ": ok\n\n") {
		t.Fatalf("body = %q, want initial : ok comment", w.Body.String())
	}
}

func TestParseLogCursor(t *testing.T) {
	wantSec := time.Date(2026, 11, 27, 2, 51, 13, 0, time.UTC).Unix()
	ts, seq, ok := parseLogCursor("2026-11-27T02:51:13.123456789Z|3")
	if !ok {
		t.Fatalf("expected valid cursor")
	}
	if seq != 3 || ts.Unix() != wantSec || ts.Nanosecond() != 123456789 {
		t.Fatalf("got ts=%v (unix %d) seq=%d, want unix %d", ts, ts.Unix(), seq, wantSec)
	}
	for _, bad := range []string{"", "not-a-ts|1", "2026-11-27T02:51:13Z", "2026-11-27T02:51:13Z|0", "2026-11-27T02:51:13Z|-1", "2026-11-27T02:51:13Z|abc"} {
		if _, _, ok := parseLogCursor(bad); ok {
			t.Errorf("parseLogCursor(%q) = ok, want false", bad)
		}
	}
}

func TestContainerLogsSSEEmitsCursorIDsAndResumesWithoutDuplicates(t *testing.T) {
	var mu sync.Mutex
	var gotQueries []string

	t1 := "2026-11-27T02:51:13.100000000Z"
	t2 := "2026-11-27T02:51:13.200000000Z"
	t3 := "2026-11-27T02:51:14.050000000Z"
	allLogs := strings.Join([]string{
		t1 + " before-cursor",
		t2 + " same-ns-1",
		t2 + " same-ns-2",
		t2 + " same-ns-3",
		"raw-untagged-after-boundary",
		t3 + " next-sec",
	}, "\n") + "\n"

	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":   "cid-log-test",
				"Name": "/demo-web-1",
				"State": map[string]any{
					"Status":  "running",
					"Running": true,
				},
				"Config": map[string]any{
					"Tty":    true,
					"Image":  "nginx:latest",
					"Labels": map[string]string{},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			mu.Lock()
			gotQueries = append(gotQueries, r.URL.RawQuery)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, allLogs)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockDocker.Close()

	tmp := t.TempDir()
	srv, err := New(config.Config{
		DataDir:     tmp,
		ProjectsDir: tmp,
		BackupsDir:  tmp,
		DockerHost:  "tcp://" + mockDocker.Listener.Addr().String(),
	}, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { _ = srv.DB().Close() })

	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// 1. Initial connection emits SSE id: <RFC3339Nano>|<seq> with duplicate-ns incrementing.
	req1 := httptest.NewRequest(http.MethodGet, "/api/containers/cid-log-test/logs?tail=500&follow=0", nil)
	req1.SetPathValue("id", "cid-log-test")
	w1 := httptest.NewRecorder()
	if err := srv.handleContainerLogs(w1, req1, admin); err != nil {
		t.Fatalf("initial handleContainerLogs: %v", err)
	}
	body1 := w1.Body.String()
	for _, want := range []string{
		"id: " + t1 + "|1\ndata: " + t1 + " before-cursor\n\n",
		"id: " + t2 + "|1\ndata: " + t2 + " same-ns-1\n\n",
		"id: " + t2 + "|2\ndata: " + t2 + " same-ns-2\n\n",
		"id: " + t2 + "|3\ndata: " + t2 + " same-ns-3\n\n",
		"id: " + t3 + "|1\ndata: " + t3 + " next-sec\n\n",
	} {
		if !strings.Contains(body1, want) {
			t.Errorf("initial SSE body missing %q\nfull body:\n%s", want, body1)
		}
	}

	// 2. Reconnect via Last-Event-ID at t2|2 must pass integer Unix since & tail=all to Docker,
	// skip t1 and t2#1..#2, and emit t2#3 (with id t2|3), raw-untagged-after-boundary, and t3#1.
	req2 := httptest.NewRequest(http.MethodGet, "/api/containers/cid-log-test/logs?tail=500&follow=0", nil)
	req2.SetPathValue("id", "cid-log-test")
	req2.Header.Set("Last-Event-ID", t2+"|2")
	w2 := httptest.NewRecorder()
	if err := srv.handleContainerLogs(w2, req2, admin); err != nil {
		t.Fatalf("resume Last-Event-ID handleContainerLogs: %v", err)
	}
	body2 := w2.Body.String()
	for _, forbidden := range []string{"before-cursor", "same-ns-1", "same-ns-2"} {
		if strings.Contains(body2, forbidden) {
			t.Errorf("resumed SSE body unexpectedly repeated %q\nfull body:\n%s", forbidden, body2)
		}
	}
	for _, want := range []string{
		"id: " + t2 + "|3\ndata: " + t2 + " same-ns-3\n\n",
		"data: raw-untagged-after-boundary\n\n",
		"id: " + t3 + "|1\ndata: " + t3 + " next-sec\n\n",
	} {
		if !strings.Contains(body2, want) {
			t.Errorf("resumed SSE body missing %q\nfull body:\n%s", want, body2)
		}
	}

	// 3. Resume via ?after= query parameter behaves identically.
	req3 := httptest.NewRequest(http.MethodGet, "/api/containers/cid-log-test/logs?tail=500&follow=0&after="+t2+"%7C2", nil)
	req3.SetPathValue("id", "cid-log-test")
	w3 := httptest.NewRecorder()
	if err := srv.handleContainerLogs(w3, req3, admin); err != nil {
		t.Fatalf("resume after= handleContainerLogs: %v", err)
	}
	if strings.Contains(w3.Body.String(), "same-ns-2") || !strings.Contains(w3.Body.String(), "id: "+t2+"|3\n") {
		t.Errorf("after= resume body unexpected:\n%s", w3.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotQueries) < 3 {
		t.Fatalf("expected 3 docker /logs calls, got %d", len(gotQueries))
	}
	wantSince := "since=" + strconv.FormatInt(time.Date(2026, 11, 27, 2, 51, 13, 0, time.UTC).Unix(), 10)
	for idx, q := range gotQueries[1:3] {
		if !strings.Contains(q, wantSince) || !strings.Contains(q, "tail=all") {
			t.Errorf("resume docker query #%d = %q, want integer Unix %s and tail=all", idx+1, q, wantSince)
		}
	}
}

func dockerMuxFrame(stream byte, payload string) []byte {
	var buf bytes.Buffer
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	buf.Write(hdr[:])
	buf.WriteString(payload)
	return buf.Bytes()
}

func TestContainerLogsTtyFalseMultiplexedFramesAndCR(t *testing.T) {
	type timedFrame struct {
		sec    int64
		stream byte
		line   string
	}
	baseSec := time.Date(2026, 11, 27, 3, 0, 10, 0, time.UTC).Unix()
	tOld := "2026-11-27T03:00:09.500000000Z"
	tCur := "2026-11-27T03:00:10.250000000Z"
	tNew := "2026-11-27T03:00:11.000000000Z"

	frames := []timedFrame{
		{baseSec - 1, 1, tOld + " old-stdout\n"},
		{baseSec, 1, tCur + " stdout-1\n"},
		{baseSec, 2, tCur + " stderr-progress 50%\rstderr-progress 100%\n"},
		{baseSec + 1, 2, tNew + " after-cr\n"},
	}

	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":   "cid-mux",
				"Name": "/mux-web-1",
				"State": map[string]any{
					"Status":  "running",
					"Running": true,
				},
				"Config": map[string]any{
					"Tty":    false, // multiplexed stdout/stderr stream!
					"Image":  "nginx:latest",
					"Labels": map[string]string{},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			sinceSec, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
			w.WriteHeader(http.StatusOK)
			for _, f := range frames {
				if sinceSec > 0 && f.sec < sinceSec {
					continue
				}
				_, _ = w.Write(dockerMuxFrame(f.stream, f.line))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockDocker.Close()

	tmp := t.TempDir()
	srv, err := New(config.Config{
		DataDir:     tmp,
		ProjectsDir: tmp,
		BackupsDir:  tmp,
		DockerHost:  "tcp://" + mockDocker.Listener.Addr().String(),
	}, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { _ = srv.DB().Close() })

	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	req := httptest.NewRequest(http.MethodGet, "/api/containers/cid-mux/logs?tail=500&follow=0", nil)
	req.SetPathValue("id", "cid-mux")
	req.Header.Set("Last-Event-ID", tCur+"|1")
	w := httptest.NewRecorder()
	if err := srv.handleContainerLogs(w, req, admin); err != nil {
		t.Fatalf("handleContainerLogs(mux resume): %v", err)
	}
	body := w.Body.String()
	if strings.Contains(body, "old-stdout") || strings.Contains(body, "stdout-1") {
		t.Errorf("resumed mux SSE repeated pre-cursor lines:\n%s", body)
	}
	wantCR := "id: " + tCur + "|2\ndata: " + tCur + " stderr-progress 50%\ndata: stderr-progress 100%\n\n"
	if !strings.Contains(body, wantCR) {
		t.Errorf("resumed mux SSE missing CR-split event %q\nbody:\n%s", wantCR, body)
	}
	wantNext := "id: " + tNew + "|1\ndata: " + tNew + " after-cr\n\n"
	if !strings.Contains(body, wantNext) {
		t.Errorf("resumed mux SSE missing next event %q\nbody:\n%s", wantNext, body)
	}
}

func startMockTerminalDocker(t *testing.T, onStart func(cmd []string, conn io.ReadWriteCloser)) (*httptest.Server, *[][]string) {
	t.Helper()
	var mu sync.Mutex
	cmds := map[string][]string{}
	var seen [][]string
	seq := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/cid-term/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "cid-term",
				"Name":   "/telegram-bridge",
				"State":  map[string]any{"Status": "running", "Running": true},
				"Config": map[string]any{"Tty": true, "Image": "distroless:latest", "Labels": map[string]string{}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/cid-term/exec":
			var body struct {
				Cmd []string `json:"Cmd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			seq++
			id := "exec-" + strconv.Itoa(seq)
			cmds[id] = body.Cmd
			seen = append(seen, body.Cmd)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start")
			mu.Lock()
			cmd := cmds[id]
			mu.Unlock()
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = rw.Flush()
			go func() {
				defer conn.Close()
				onStart(cmd, conn)
			}()
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &seen
}

func dialTestTerminalWS(t *testing.T, srv *Server, query string) (*websocket.Conn, func()) {
	t.Helper()
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", "cid-term")
		_ = srv.handleTerminal(w, r, admin)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/api/containers/cid-term/terminal" + query
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	cancel()
	if err != nil {
		httpSrv.Close()
		t.Fatalf("websocket.Dial: %v", err)
	}
	return ws, func() {
		_ = ws.CloseNow()
		httpSrv.Close()
	}
}

func readAllWSOutput(ws *websocket.Conn, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var out strings.Builder
	for {
		_, data, err := ws.Read(ctx)
		if len(data) > 0 {
			out.Write(data)
		}
		if err != nil {
			break
		}
	}
	return out.String()
}

func TestTerminalAutoFallbackToBusyboxSh(t *testing.T) {
	origProbe := termProbeTimeout
	termProbeTimeout = 120 * time.Millisecond
	t.Cleanup(func() { termProbeTimeout = origProbe })

	mock, seen := startMockTerminalDocker(t, func(cmd []string, conn io.ReadWriteCloser) {
		if len(cmd) > 0 && cmd[0] == "/busybox/sh" {
			_, _ = io.WriteString(conn, "/ # ")
			buf := make([]byte, 64)
			if n, err := conn.Read(buf); err == nil && n > 0 {
				_, _ = conn.Write([]byte("echo:" + string(buf[:n])))
			}
			return
		}
		bin := ""
		if len(cmd) > 0 {
			bin = cmd[0]
		}
		_, _ = io.WriteString(conn, "OCI runtime exec failed: exec failed: unable to start container process: exec: \""+bin+"\": stat "+bin+": no such file or directory\r\n")
	})

	srv := newTestServerWithDocker(t, mock.URL)
	ws, cleanup := dialTestTerminalWS(t, srv, "")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, first, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if string(first) != "/ # " {
		t.Fatalf("first terminal output = %q, want %q", string(first), "/ # ")
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"hi\n"}`)); err != nil {
		t.Fatalf("write input: %v", err)
	}
	_, echoed, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echoed) != "echo:hi\n" {
		t.Fatalf("echo = %q, want %q", string(echoed), "echo:hi\n")
	}
	if len(*seen) != 3 || (*seen)[0][0] != "/bin/sh" || (*seen)[1][0] != "/bin/bash" || (*seen)[2][0] != "/busybox/sh" {
		t.Fatalf("probed cmds = %v, want /bin/sh -> /bin/bash -> /busybox/sh", *seen)
	}
}

func TestTerminalNoShellShowsFriendlyChineseHint(t *testing.T) {
	origProbe := termProbeTimeout
	termProbeTimeout = 120 * time.Millisecond
	t.Cleanup(func() { termProbeTimeout = origProbe })

	mock, seen := startMockTerminalDocker(t, func(cmd []string, conn io.ReadWriteCloser) {
		bin := ""
		if len(cmd) > 0 {
			bin = cmd[0]
		}
		_, _ = io.WriteString(conn, "OCI runtime exec failed: exec failed: unable to start container process: exec: \""+bin+"\": stat "+bin+": no such file or directory\r\n")
	})

	srv := newTestServerWithDocker(t, mock.URL)
	ws, cleanup := dialTestTerminalWS(t, srv, "")
	defer cleanup()

	got := readAllWSOutput(ws, 3*time.Second)
	if len(*seen) != 4 {
		t.Fatalf("auto mode tried %d shells (%v), want 4 (/bin/sh, /bin/bash, /busybox/sh, /bin/ash)", len(*seen), *seen)
	}
	for _, want := range []string{`stat /bin/sh: no such file or directory`, "scratch", "distroless", "「文件」"} {
		if !strings.Contains(got, want) {
			t.Errorf("terminal output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestTerminalExplicitBashMissingSuggestsAutoOrSh(t *testing.T) {
	origProbe := termProbeTimeout
	termProbeTimeout = 120 * time.Millisecond
	t.Cleanup(func() { termProbeTimeout = origProbe })

	mock, seen := startMockTerminalDocker(t, func(cmd []string, conn io.ReadWriteCloser) {
		_, _ = io.WriteString(conn, "OCI runtime exec failed: exec failed: unable to start container process: exec: \"bash\": executable file not found in $PATH: unknown\r\n")
	})

	srv := newTestServerWithDocker(t, mock.URL)
	ws, cleanup := dialTestTerminalWS(t, srv, "?shell=bash")
	defer cleanup()

	got := readAllWSOutput(ws, 3*time.Second)
	if len(*seen) != 1 || (*seen)[0][0] != "bash" {
		t.Fatalf("explicit bash tried %v, want only [[bash]]", *seen)
	}
	if !strings.Contains(got, "bash") || !strings.Contains(got, "自动（bash/sh）") {
		t.Errorf("explicit bash missing hint not found:\n%s", got)
	}
}

func TestTerminalQuietShellStartsCleanlyAfterProbeDeadline(t *testing.T) {
	origProbe := termProbeTimeout
	termProbeTimeout = 60 * time.Millisecond
	t.Cleanup(func() { termProbeTimeout = origProbe })

	mock, _ := startMockTerminalDocker(t, func(cmd []string, conn io.ReadWriteCloser) {
		// Silent shell: outputs nothing until client sends input.
		buf := make([]byte, 64)
		if n, err := conn.Read(buf); err == nil && n > 0 {
			_, _ = conn.Write([]byte("quiet:" + string(buf[:n])))
		}
	})

	srv := newTestServerWithDocker(t, mock.URL)
	ws, cleanup := dialTestTerminalWS(t, srv, "")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"ok\n"}`)); err != nil {
		t.Fatalf("write input: %v", err)
	}
	_, reply, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if string(reply) != "quiet:ok\n" {
		t.Fatalf("reply = %q, want %q", string(reply), "quiet:ok\n")
	}
}
