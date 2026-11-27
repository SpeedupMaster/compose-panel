package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
