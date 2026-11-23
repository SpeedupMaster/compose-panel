package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func writeDockerFrame(w io.Writer, stream byte, payload []byte) {
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	_, _ = w.Write(hdr[:])
	_, _ = w.Write(payload)
}

func TestExecStreamsStdinStdoutStderrAndExitCode(t *testing.T) {
	var mu sync.Mutex
	var gotStdin string
	var gotExecBody map[string]any
	exitCode := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/containers/c1/exec":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			gotExecBody = body
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "ex1"})

		case r.Method == "POST" && r.URL.Path == "/exec/ex1/start":
			_, _ = io.ReadAll(r.Body)
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("hijack unsupported")
				return
			}
			conn, bufrw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = bufrw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = bufrw.Flush()

			mu.Lock()
			attachIn, _ := gotExecBody["AttachStdin"].(bool)
			mu.Unlock()
			var in []byte
			if attachIn {
				in, _ = io.ReadAll(bufrw)
				mu.Lock()
				gotStdin = string(in)
				mu.Unlock()
			}

			writeDockerFrame(conn, 1, []byte("out:"+string(in)))
			writeDockerFrame(conn, 2, []byte("warn:something\n"))

		case r.Method == "GET" && r.URL.Path == "/exec/ex1/json":
			mu.Lock()
			code := exitCode
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": code})

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cl := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))

	// 1. Success with stdin, stdout, stderr.
	var outBuf, errBuf bytes.Buffer
	err := cl.Exec(context.Background(), "c1", ExecOptions{
		Cmd:    []string{"psql", "-U", "postgres"},
		Env:    []string{"PGPASSWORD=secret"},
		User:   "root",
		Stdin:  strings.NewReader("SELECT 1;"),
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	if err != nil {
		t.Fatalf("Exec() error: %v", err)
	}
	if gotStdin != "SELECT 1;" {
		t.Errorf("stdin = %q, want %q", gotStdin, "SELECT 1;")
	}
	if outBuf.String() != "out:SELECT 1;" {
		t.Errorf("stdout = %q, want %q", outBuf.String(), "out:SELECT 1;")
	}
	if errBuf.String() != "warn:something\n" {
		t.Errorf("stderr = %q, want %q", errBuf.String(), "warn:something\n")
	}
	if gotExecBody["User"] != "root" {
		t.Errorf("exec User = %v, want root", gotExecBody["User"])
	}

	// 2. Non-zero exit code returns ExitCode(err) == 3 with stderr message.
	mu.Lock()
	exitCode = 3
	mu.Unlock()
	err = cl.Exec(context.Background(), "c1", ExecOptions{Cmd: []string{"sh", "-c", "exit 3"}})
	code, ok := ExitCode(err)
	if !ok || code != 3 {
		t.Fatalf("ExitCode(%v) = (%d, %v), want (3, true)", err, code, ok)
	}
	if !strings.Contains(err.Error(), "warn:something") {
		t.Errorf("error %q should include stderr text", err.Error())
	}

	// 3. ExecStdout streams stdout and surfaces non-zero exit code at EOF/Close.
	rc, err := cl.ExecStdout(context.Background(), "c1", ExecOptions{Cmd: []string{"pg_dump"}})
	if err != nil {
		t.Fatalf("ExecStdout() error: %v", err)
	}
	data, readErr := io.ReadAll(rc)
	closeErr := rc.Close()
	if string(data) != "out:" {
		t.Errorf("ExecStdout data = %q, want %q", string(data), "out:")
	}
	if readErr == nil && closeErr == nil {
		t.Fatalf("expected non-zero exit error from ExecStdout")
	}
}

func TestRunContainerAutoPullsStreamsAndCleansUp(t *testing.T) {
	var pulled atomic.Bool
	var started atomic.Bool
	var removed atomic.Bool
	var removedVolumes atomic.Bool
	var stdinGot string
	var mu sync.Mutex
	var exitStatus int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/containers/create":
			if !pulled.Load() {
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such image: alpine:3.20"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "run1"})

		case r.Method == "POST" && r.URL.Path == "/images/create":
			if r.URL.Query().Get("fromImage") != "alpine:3.20" {
				t.Errorf("unexpected pull fromImage=%q", r.URL.Query().Get("fromImage"))
			}
			pulled.Store(true)
			_, _ = w.Write([]byte(`{"status":"Pull complete"}` + "\n"))

		case r.Method == "POST" && r.URL.Path == "/containers/run1/attach":
			hj := w.(http.Hijacker)
			conn, bufrw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = bufrw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = bufrw.Flush()
			if r.URL.Query().Get("stdin") == "1" {
				in, _ := io.ReadAll(bufrw)
				mu.Lock()
				stdinGot = string(in)
				mu.Unlock()
				writeDockerFrame(conn, 1, []byte("tar-ok:"+string(in)))
			} else {
				writeDockerFrame(conn, 1, []byte("archive-bytes"))
			}

		case r.Method == "POST" && r.URL.Path == "/containers/run1/start":
			started.Store(true)
			w.WriteHeader(204)

		case r.Method == "POST" && r.URL.Path == "/containers/run1/wait":
			mu.Lock()
			st := exitStatus
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": st})

		case r.Method == "DELETE" && r.URL.Path == "/containers/run1":
			removed.Store(true)
			if r.URL.Query().Get("v") == "1" {
				removedVolumes.Store(true)
			}
			w.WriteHeader(204)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cl := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))

	// 1. RunContainer auto-pulls missing image, streams stdin/stdout, and removes container.
	var stdout bytes.Buffer
	err := cl.RunContainer(context.Background(), RunOptions{
		Image:  "alpine:3.20",
		Binds:  []string{"myvol:/v"},
		Cmd:    []string{"tar", "-C", "/v", "-xf", "-"},
		Stdin:  strings.NewReader("payload"),
		Stdout: &stdout,
	})
	if err != nil {
		t.Fatalf("RunContainer() error: %v", err)
	}
	if !pulled.Load() || !started.Load() || !removed.Load() || !removedVolumes.Load() {
		t.Fatalf("lifecycle flags pulled=%v started=%v removed=%v v=%v", pulled.Load(), started.Load(), removed.Load(), removedVolumes.Load())
	}
	if stdinGot != "payload" || stdout.String() != "tar-ok:payload" {
		t.Fatalf("stdin=%q stdout=%q", stdinGot, stdout.String())
	}

	// 2. RunContainerStdout streams stdout and cleans up container on close.
	removed.Store(false)
	rc, err := cl.RunContainerStdout(context.Background(), RunOptions{
		Image: "alpine:3.20",
		Binds: []string{"myvol:/v:ro"},
		Cmd:   []string{"tar", "-C", "/v", "-cf", "-", "."},
	})
	if err != nil {
		t.Fatalf("RunContainerStdout() error: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read RunContainerStdout: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close RunContainerStdout: %v", err)
	}
	if string(got) != "archive-bytes" {
		t.Errorf("got %q, want archive-bytes", string(got))
	}
	if !removed.Load() {
		t.Errorf("RunContainerStdout did not remove temporary container")
	}

	// 3. StartDetached creates & starts without attaching.
	id, err := cl.StartDetached(context.Background(), RunOptions{
		Name:   "panel-drill-1",
		Image:  "alpine:3.20",
		Labels: map[string]string{"panel.drill": "1"},
	})
	if err != nil || id != "run1" {
		t.Fatalf("StartDetached = (%q, %v), want (run1, nil)", id, err)
	}
	if err := cl.RemoveContainerWithVolumes(context.Background(), id, true, true); err != nil {
		t.Fatalf("RemoveContainerWithVolumes error: %v", err)
	}
}
