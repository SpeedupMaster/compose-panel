package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
