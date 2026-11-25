package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compose-panel/internal/auth"
	"compose-panel/internal/docker"
	"compose-panel/internal/proxy"
)

func startUnixHTTPServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	// Keep path well under the ~108-byte AF_UNIX sun_path limit.
	dir, err := os.MkdirTemp("", "cp-*")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return sock
}

func TestDBQueryRoleGateRejectsViewerAndReadToken(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().Unix()

	// Seed a database project + db_instance.
	res, err := srv.DB().Exec(`INSERT INTO projects(name,description,kind,source,webhook_token,compose,env,options,created_at,updated_at) VALUES('appdb','PostgreSQL 17','database','inline','tok','','','{}',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := res.LastInsertId()
	res, err = srv.DB().Exec(`INSERT INTO db_instances(project_id,engine,version,root_user,root_password,port,exposure,created_at,image) VALUES(?,?,?,?,?,?,?,?,?)`,
		pid, "postgres", "17", "postgres", srv.box.Seal("secret"), 5432, "internal", now, "")
	if err != nil {
		t.Fatal(err)
	}
	dbID, _ := res.LastInsertId()

	// Seed viewer, operator, and admin users.
	insertUser := func(name, role string) int64 {
		r, err := srv.DB().Exec(`INSERT INTO users(username,email,password_hash,role,created_at) VALUES(?,?,?,?,?)`, name, name+"@x.y", "hash", role, now)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		return id
	}
	viewerID := insertUser("viewer1", "viewer")
	opID := insertUser("op1", "operator")
	adminID := insertUser("admin1", "admin")

	createSession := func(uid int64, raw string) {
		_, err := srv.DB().Exec(`INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES(?,?,?,?,?)`,
			auth.SHA256Hex(raw), uid, now, now+3600, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	createSession(viewerID, "viewer-sess")
	createSession(opID, "op-sess")

	// Read-scoped API token owned by admin.
	const readTok = "read-token-raw"
	if _, err := srv.DB().Exec(`INSERT INTO api_tokens(user_id,name,prefix,token_hash,scope,expires_at,created_at) VALUES(?,?,?,?,?,?,?)`,
		adminID, "ro", "read-tok", auth.SHA256Hex(readTok), "read", now+3600, now); err != nil {
		t.Fatal(err)
	}

	handler := srv.Handler()
	url := "/api/databases/" + strings.TrimSpace(jsonNumber(dbID)) + "/query"
	body := `{"database":"app","sql":"DELETE FROM t","readOnly":false}`

	// 1. Viewer session -> 403 Forbidden
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-Panel", "1")
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "viewer-sess"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer POST %s status = %d, want 403 (body=%s)", url, w.Code, w.Body.String())
	}

	// 2. Admin's read-scoped Bearer token -> 403 Forbidden
	req = httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+readTok)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("read-token POST %s status = %d, want 403 (body=%s)", url, w.Code, w.Body.String())
	}

	// 3. Operator session -> allowed past role gate and hits Danger guardrail (200 needConfirm=true)
	req = httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-Panel", "1")
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "op-sess"})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"needConfirm":true`) {
		t.Fatalf("operator POST %s status=%d body=%s, want 200 with needConfirm", url, w.Code, w.Body.String())
	}
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestCaddyBootstrapRetriesBeyond30Attempts(t *testing.T) {
	origBase, origMax := caddyBootstrapBaseDelay, caddyBootstrapMaxDelay
	caddyBootstrapBaseDelay = 2 * time.Millisecond
	caddyBootstrapMaxDelay = 4 * time.Millisecond
	t.Cleanup(func() {
		caddyBootstrapBaseDelay = origBase
		caddyBootstrapMaxDelay = origMax
	})

	var attempts atomic.Int32
	sock := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/load" && r.Method == http.MethodPost {
			n := attempts.Add(1)
			if n <= 35 {
				http.Error(w, "starting", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))

	srv := newTestServer(t)
	srv.caddy = proxy.New(sock)
	srv.cfg.CaddyAdmin = sock

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.bootstrapCaddy(ctx)

	if got := attempts.Load(); got != 36 {
		t.Fatalf("bootstrapCaddy attempts = %d, want 36 (must not give up after 30)", got)
	}
}

func TestCaddySelfHealingOnStartEventAndWatchdog(t *testing.T) {
	origDelays := caddyStartRetryDelays
	caddyStartRetryDelays = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}
	t.Cleanup(func() { caddyStartRetryDelays = origDelays })

	var loaded atomic.Bool
	var loadCalls atomic.Int32
	var failFirstLoad atomic.Bool
	failFirstLoad.Store(true)

	caddySock := startUnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/config/apps/http":
			if loaded.Load() {
				_, _ = w.Write([]byte(`{"servers":{"main":{"listen":[":443",":80"]}}}`))
			} else {
				_, _ = w.Write([]byte("null\n"))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/load":
			n := loadCalls.Add(1)
			if failFirstLoad.Load() && n == 1 {
				http.Error(w, "caddy still starting", http.StatusServiceUnavailable)
				return
			}
			loaded.Store(true)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))

	srv := newTestServer(t)
	srv.caddy = proxy.New(caddySock)
	srv.cfg.CaddyAdmin = caddySock

	// 1. Watchdog on empty install (no panel_domain, 0 sites) must NOT sync needlessly.
	srv.ensureCaddyLoaded(context.Background())
	if loadCalls.Load() != 0 {
		t.Fatalf("empty install triggered %d /load calls, want 0", loadCalls.Load())
	}

	// 2. Watchdog with panel_domain set must detect !loaded and restore Caddy config.
	failFirstLoad.Store(false)
	srv.setSetting("panel_domain", "panel.example.com")
	srv.ensureCaddyLoaded(context.Background())
	if loadCalls.Load() != 1 || !loaded.Load() {
		t.Fatalf("watchdog did not restore config: loadCalls=%d loaded=%v", loadCalls.Load(), loaded.Load())
	}

	// 3. Watchdog when already loaded must NOT sync again.
	srv.ensureCaddyLoaded(context.Background())
	if loadCalls.Load() != 1 {
		t.Fatalf("watchdog synced again when already loaded: loadCalls=%d, want 1", loadCalls.Load())
	}

	// 4. Docker "start" event for panel-caddy (even with panel.monitor=false) triggers resync with retry.
	loaded.Store(false)
	loadCalls.Store(0)
	failFirstLoad.Store(true)

	evCtx, evCancel := context.WithCancel(context.Background())
	defer evCancel()

	var eventsFilter string
	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			eventsFilter = r.URL.Query().Get("filters")
			fl, _ := w.(http.Flusher)
			// Non-caddy start event should be ignored.
			_, _ = w.Write([]byte(`{"Type":"container","Action":"start","Actor":{"ID":"c1","Attributes":{"name":"web-1","com.docker.compose.project":"demo"}}}` + "\n"))
			// panel-caddy start event (with panel.monitor=false!) must still resync.
			_, _ = w.Write([]byte(`{"Type":"container","Action":"start","Actor":{"ID":"c2","Attributes":{"name":"panel-caddy","panel.monitor":"false"}}}` + "\n"))
			if fl != nil {
				fl.Flush()
			}
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	defer mockDocker.Close()
	srv.dk = docker.New("tcp://" + strings.TrimPrefix(mockDocker.URL, "http://"))

	done := make(chan struct{})
	go func() {
		srv.eventsLoop(evCtx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for !loaded.Load() && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	evCancel()
	<-done
	srv.bgCancel()
	srv.bgWG.Wait()

	if !strings.Contains(eventsFilter, `"start"`) {
		t.Fatalf("eventsLoop filter missing \"start\": %s", eventsFilter)
	}
	if !loaded.Load() || loadCalls.Load() != 2 {
		t.Fatalf("panel-caddy start event did not retry and restore config: loaded=%v loadCalls=%d", loaded.Load(), loadCalls.Load())
	}
}
