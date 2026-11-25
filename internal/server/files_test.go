package server

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"compose-panel/internal/docker"
)

func writeMockDockerFrame(w io.Writer, stream byte, payload []byte) {
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	_, _ = w.Write(hdr[:])
	_, _ = w.Write(payload)
}

func TestContainerAndVolumeFSUseDockerAPIWithoutCLI(t *testing.T) {
	var execCalls, createCalls atomic.Int32
	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/containers/c1/json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":    "c1",
				"Name":  "/app-web-1",
				"State": map[string]any{"Running": true},
			})
		case r.Method == "POST" && r.URL.Path == "/containers/c1/exec":
			execCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "ex1"})
		case r.Method == "POST" && r.URL.Path == "/exec/ex1/start":
			_, _ = io.ReadAll(r.Body)
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = brw.Flush()
			writeMockDockerFrame(conn, 1, []byte("-|regular file|42|1700000000|-rw-r--r--|hello.txt\x00"))
		case r.Method == "GET" && r.URL.Path == "/exec/ex1/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": 0})
		case r.Method == "GET" && r.URL.Path == "/volumes/vol1":
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": "vol1"})
		case r.Method == "POST" && r.URL.Path == "/containers/create":
			createCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "helper1"})
		case r.Method == "POST" && r.URL.Path == "/containers/helper1/attach":
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = brw.Flush()
			writeMockDockerFrame(conn, 1, []byte("-|directory|0|1700000000|drwxr-xr-x|subdir\x00"))
		case r.Method == "POST" && r.URL.Path == "/containers/helper1/start":
			w.WriteHeader(204)
		case r.Method == "POST" && r.URL.Path == "/containers/helper1/wait":
			_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": 0})
		case r.Method == "DELETE" && r.URL.Path == "/containers/helper1":
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockDocker.Close()

	srv := newTestServer(t)
	srv.dk = docker.New("tcp://" + strings.TrimPrefix(mockDocker.URL, "http://"))
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Container FS listing should use Docker API exec (not docker CLI).
	req := httptest.NewRequest(http.MethodGet, "/api/containers/c1/fs?path=/app", nil)
	req.SetPathValue("id", "c1")
	w := httptest.NewRecorder()
	if err := srv.handleContainerFS(w, req, admin); err != nil {
		t.Fatalf("handleContainerFS error: %v", err)
	}
	if execCalls.Load() != 1 || !strings.Contains(w.Body.String(), `"hello.txt"`) {
		t.Fatalf("execCalls=%d body=%s", execCalls.Load(), w.Body.String())
	}

	// Volume FS listing should use Docker API helper container (not docker CLI).
	vreq := httptest.NewRequest(http.MethodGet, "/api/docker/volumes/vol1/fs?path=/", nil)
	vreq.SetPathValue("name", "vol1")
	vw := httptest.NewRecorder()
	if err := srv.handleVolumeFS(vw, vreq, admin); err != nil {
		t.Fatalf("handleVolumeFS error: %v", err)
	}
	if createCalls.Load() != 1 || !strings.Contains(vw.Body.String(), `"subdir"`) {
		t.Fatalf("createCalls=%d body=%s", createCalls.Load(), vw.Body.String())
	}
}

func TestParseLsPreventsNewlineAndPipeSpoofing(t *testing.T) {
	// A malicious filename containing newline + fake pipe record + extra pipes.
	evilName := "evil\n-|regular file|999|1700000000|-rw-r--r--|spoof.txt|extra"
	raw := []byte("-|regular file|12|1700000000|-rw-r--r--|" + evilName + "\x00TRUNCATED\x00")
	entries, trunc := parseLs(raw)
	if !trunc {
		t.Fatalf("expected trunc=true")
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d (%+v), want 1 (newline must not spoof a second entry)", len(entries), entries)
	}
	if entries[0].Name != evilName || entries[0].Size != 12 {
		t.Fatalf("entry = %+v, want exact Name=%q Size=12", entries[0], evilName)
	}
}

func TestUploadSizeLimitAndVolumeSymlinkProtection(t *testing.T) {
	var createdCmd []string
	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/containers/c1/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "c1", "Name": "/app-1", "State": map[string]any{"Running": true}})
		case r.Method == "GET" && r.URL.Path == "/volumes/vol1":
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": "vol1"})
		case r.Method == "POST" && r.URL.Path == "/containers/create":
			var body struct {
				Cmd []string `json:"Cmd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			createdCmd = body.Cmd
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "h1"})
		case r.Method == "POST" && r.URL.Path == "/containers/h1/attach":
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = brw.Flush()
		case r.Method == "POST" && r.URL.Path == "/containers/h1/start":
			w.WriteHeader(204)
		case r.Method == "POST" && r.URL.Path == "/containers/h1/wait":
			_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": 0})
		case r.Method == "DELETE" && r.URL.Path == "/containers/h1":
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockDocker.Close()

	srv := newTestServer(t)
	srv.dk = docker.New("tcp://" + strings.TrimPrefix(mockDocker.URL, "http://"))
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Container upload > 2 GiB rejected with 413.
	creq := httptest.NewRequest(http.MethodPut, "/api/containers/c1/fs/upload?path=/app&name=big.bin", nil)
	creq.SetPathValue("id", "c1")
	creq.ContentLength = maxUploadBytes + 1
	err := srv.handleContainerUpload(httptest.NewRecorder(), creq, admin)
	if ae, ok := err.(*apiError); !ok || ae.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("container upload >2GiB err = %v, want 413", err)
	}

	// Volume upload > 2 GiB rejected with 413.
	vreq := httptest.NewRequest(http.MethodPut, "/api/docker/volumes/vol1/fs/upload?path=/&name=big.bin", nil)
	vreq.SetPathValue("name", "vol1")
	vreq.ContentLength = maxUploadBytes + 1
	err = srv.handleVolumeUpload(httptest.NewRecorder(), vreq, admin)
	if ae, ok := err.(*apiError); !ok || ae.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("volume upload >2GiB err = %v, want 413", err)
	}

	// Valid volume upload checks symlink guard & chown -h in helper script.
	vreq2 := httptest.NewRequest(http.MethodPut, "/api/docker/volumes/vol1/fs/upload?path=/&name=ok.txt", strings.NewReader("hello"))
	vreq2.SetPathValue("name", "vol1")
	if err := srv.handleVolumeUpload(httptest.NewRecorder(), vreq2, admin); err != nil {
		t.Fatalf("volume upload error: %v", err)
	}
	script := strings.Join(createdCmd, " ")
	if !strings.Contains(script, `[ ! -L "$1/$2" ]`) || !strings.Contains(script, `chown -h`) {
		t.Fatalf("volume upload helper script missing symlink guard or chown -h: %s", script)
	}
}
