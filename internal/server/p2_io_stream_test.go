package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"compose-panel/internal/backup"
	"compose-panel/internal/config"
	"compose-panel/internal/docker"
	"compose-panel/internal/jobs"
)

func TestStartupCrashReconciliation(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Config{
		DataDir:     tmp,
		ProjectsDir: filepath.Join(tmp, "projects"),
		BackupsDir:  filepath.Join(tmp, "backups"),
		DockerHost:  "unix:///nonexistent.sock",
	}
	srv1, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New(1): %v", err)
	}
	now := time.Now().Unix()

	// Seed project, policy, and interrupted + completed domain rows.
	res, err := srv1.DB().Exec(`INSERT INTO projects(name,description,kind,source,webhook_token,compose,env,options,created_at,updated_at) VALUES('demo','','app','inline','tok','','','{}',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := res.LastInsertId()

	if _, err := srv1.DB().Exec(`INSERT INTO deployments(project_id,number,status,trigger,created_at) VALUES(?,1,'running','manual',?), (?,2,'queued','manual',?), (?,3,'success','manual',?)`, pid, now, pid, now, pid, now); err != nil {
		t.Fatal(err)
	}
	if _, err := srv1.DB().Exec(`INSERT INTO backups(source,label,storage_id,status,started_at) VALUES('{}','b1',1,'running',?), ('{}','b2',1,'queued',?), ('{}','b3',1,'success',?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := srv1.DB().Exec(`INSERT INTO drill_runs(backup_id,label,status,started_at) VALUES(1,'d1','running',?), (1,'d2','queued',?), (1,'d3','success',?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := srv1.DB().Exec(`INSERT INTO cron_tasks(name,project_id,service,command,schedule,last_status,created_at) VALUES('c1',?,'web','echo 1','* * * * *','running',?), ('c2',?,'web','echo 2','* * * * *','success',?)`, pid, now, pid, now); err != nil {
		t.Fatal(err)
	}
	_ = srv1.DB().Close()

	// Re-open via New() simulating panel restart after crash.
	srv2, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New(2): %v", err)
	}
	defer srv2.DB().Close()

	// Check deployments.
	var depFailed, depSuccess int
	_ = srv2.DB().QueryRow(`SELECT count(*) FROM deployments WHERE status='failed' AND finished_at > 0`).Scan(&depFailed)
	_ = srv2.DB().QueryRow(`SELECT count(*) FROM deployments WHERE status='success'`).Scan(&depSuccess)
	if depFailed != 2 || depSuccess != 1 {
		t.Fatalf("deployments: failed=%d success=%d, want 2 and 1", depFailed, depSuccess)
	}

	// Check backups.
	var bakFailed, bakSuccess int
	_ = srv2.DB().QueryRow(`SELECT count(*) FROM backups WHERE status='failed' AND error='面板重启，任务被中断' AND finished_at > 0`).Scan(&bakFailed)
	_ = srv2.DB().QueryRow(`SELECT count(*) FROM backups WHERE status='success'`).Scan(&bakSuccess)
	if bakFailed != 2 || bakSuccess != 1 {
		t.Fatalf("backups: failed=%d success=%d, want 2 and 1", bakFailed, bakSuccess)
	}

	// Check drill_runs.
	var drillFailed, drillSuccess int
	_ = srv2.DB().QueryRow(`SELECT count(*) FROM drill_runs WHERE status='failed' AND report='面板重启，演练被中断' AND finished_at > 0`).Scan(&drillFailed)
	_ = srv2.DB().QueryRow(`SELECT count(*) FROM drill_runs WHERE status='success'`).Scan(&drillSuccess)
	if drillFailed != 2 || drillSuccess != 1 {
		t.Fatalf("drill_runs: failed=%d success=%d, want 2 and 1", drillFailed, drillSuccess)
	}

	// Check cron_tasks.
	var cronFailed, cronSuccess int
	_ = srv2.DB().QueryRow(`SELECT count(*) FROM cron_tasks WHERE last_status='failed'`).Scan(&cronFailed)
	_ = srv2.DB().QueryRow(`SELECT count(*) FROM cron_tasks WHERE last_status='success'`).Scan(&cronSuccess)
	if cronFailed != 1 || cronSuccess != 1 {
		t.Fatalf("cron_tasks: failed=%d success=%d, want 1 and 1", cronFailed, cronSuccess)
	}
}

func TestCleanupOrphansRemovesStaleFilesAndRespectsLiveDrillJobs(t *testing.T) {
	var removedContainers, removedVolumes []string
	var mu sync.Mutex
	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "drill-ctr-1", "Names": []string{"/panel-drill-1"}, "Labels": map[string]string{"panel.drill": "1"}},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/containers/"):
			mu.Lock()
			removedContainers = append(removedContainers, strings.TrimPrefix(r.URL.Path, "/containers/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/volumes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Volumes": []map[string]any{
					{"Name": "panel-drill-vol-1", "Labels": map[string]string{"panel.drill": "1"}},
					{"Name": "app-data", "Labels": map[string]string{"com.docker.compose.project": "app"}},
				},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/volumes/"):
			mu.Lock()
			removedVolumes = append(removedVolumes, strings.TrimPrefix(r.URL.Path, "/volumes/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockDocker.Close()

	srv := newTestServer(t)
	srv.dk = docker.New("tcp://" + strings.TrimPrefix(mockDocker.URL, "http://"))

	// Create stale (>6h) and fresh (<1h) temp files and snapshots.
	tmpDir := filepath.Join(srv.cfg.DataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(tmpDir, "import-old.sql")
	freshFile := filepath.Join(tmpDir, "import-fresh.sql")
	oldSnap := filepath.Join(srv.cfg.DataDir, "panel-snapshot-100.db")
	freshSnap := filepath.Join(srv.cfg.DataDir, "panel-snapshot-200.db")
	for _, p := range []string{oldFile, freshFile, oldSnap, freshSnap} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-7 * time.Hour)
	_ = os.Chtimes(oldFile, oldTime, oldTime)
	_ = os.Chtimes(oldSnap, oldTime, oldTime)

	// Start a live backup.drill job: Docker drill resources must NOT be removed while live.
	started := make(chan struct{})
	release := make(chan struct{})
	srv.jobs.Submit(jobs.Spec{Kind: "backup.drill", Title: "active drill"}, func(r *jobs.Run) error {
		close(started)
		<-release
		return nil
	})
	<-started

	srv.cleanupOrphans(context.Background())

	// Stale temp files should already be cleaned, fresh files kept, Docker drill resources untouched!
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("old temp file was not deleted")
	}
	if _, err := os.Stat(oldSnap); !os.IsNotExist(err) {
		t.Errorf("old snapshot was not deleted")
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Errorf("fresh temp file should be kept: %v", err)
	}
	if _, err := os.Stat(freshSnap); err != nil {
		t.Errorf("fresh snapshot should be kept: %v", err)
	}
	mu.Lock()
	if len(removedContainers) != 0 || len(removedVolumes) != 0 {
		t.Fatalf("removed drill resources while drill job was live: ctrs=%v vols=%v", removedContainers, removedVolumes)
	}
	mu.Unlock()

	// Finish drill job, then run cleanupOrphans again -> orphaned drill container & volume must be removed!
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for srv.jobs.HasLiveKind("backup.drill") && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	srv.cleanupOrphans(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(removedContainers) != 1 || removedContainers[0] != "drill-ctr-1" {
		t.Fatalf("removedContainers = %v, want [drill-ctr-1]", removedContainers)
	}
	if len(removedVolumes) != 1 || removedVolumes[0] != "panel-drill-vol-1" {
		t.Fatalf("removedVolumes = %v, want [panel-drill-vol-1]", removedVolumes)
	}
}

func TestHandleDBExportAbortsWithoutValidGzipTrailerOnTruncatedDump(t *testing.T) {
	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "ex-trunc"})
		case r.Method == http.MethodPost && r.URL.Path == "/exec/ex-trunc/start":
			_, _ = io.ReadAll(r.Body)
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = brw.Flush()
			// Emit first chunk of SQL, then exit non-zero (simulating mid-dump crash).
			writeMockDockerFrame(conn, 1, []byte("-- partial dump\nCREATE TABLE t(id int);\n"))
			writeMockDockerFrame(conn, 2, []byte("pg_dump: connection lost\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/exec/ex-trunc/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockDocker.Close()

	srv := newTestServer(t)
	srv.dk = docker.New("tcp://" + strings.TrimPrefix(mockDocker.URL, "http://"))
	now := time.Now().Unix()
	res, err := srv.DB().Exec(`INSERT INTO projects(name,description,kind,source,webhook_token,compose,env,options,created_at,updated_at) VALUES('pg1','','database','inline','t','','','{}',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := res.LastInsertId()
	res, err = srv.DB().Exec(`INSERT INTO db_instances(project_id,engine,version,root_user,root_password,port,exposure,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		pid, "postgres", "17", "postgres", srv.box.Seal("pw"), 5432, "internal", now)
	if err != nil {
		t.Fatal(err)
	}
	dbID, _ := res.LastInsertId()

	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	req := httptest.NewRequest(http.MethodGet, "/api/databases/1/export?db=app&format=sql&gzip=1", nil)
	req.SetPathValue("id", jsonNumber(dbID))
	rec := httptest.NewRecorder()

	var panicked any
	func() {
		defer func() { panicked = recover() }()
		_ = srv.handleDBExport(rec, req, admin)
	}()
	if panicked != http.ErrAbortHandler {
		t.Fatalf("expected panic(http.ErrAbortHandler) on truncated export, got %v", panicked)
	}
	// Crucial: the output MUST NOT be a valid complete gzip archive!
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err == nil {
		_, rerr := io.ReadAll(zr)
		_ = zr.Close()
		if rerr == nil {
			t.Fatalf("truncated gzip export produced a valid complete gzip stream (trailer must not be written)")
		}
	}
}

func TestHandleDBImportGuardsDiskSpaceAndMaxSize(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().Unix()
	res, err := srv.DB().Exec(`INSERT INTO projects(name,description,kind,source,webhook_token,compose,env,options,created_at,updated_at) VALUES('pg2','','database','inline','t','','','{}',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := res.LastInsertId()
	res, err = srv.DB().Exec(`INSERT INTO db_instances(project_id,engine,version,root_user,root_password,port,exposure,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		pid, "postgres", "17", "postgres", srv.box.Seal("pw"), 5432, "internal", now)
	if err != nil {
		t.Fatal(err)
	}
	dbID, _ := res.LastInsertId()
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// 1. > 10 GiB rejected with 413.
	reqBig := httptest.NewRequest(http.MethodPost, "/api/databases/1/import?db=app&name=huge.sql", nil)
	reqBig.SetPathValue("id", jsonNumber(dbID))
	reqBig.ContentLength = maxDBImportBytes + 1
	err = srv.handleDBImport(httptest.NewRecorder(), reqBig, admin)
	if ae, ok := err.(*apiError); !ok || ae.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("import >10GiB err = %v, want 413", err)
	}

	// 2. < 512 MiB free disk space rejected with 507 before spooling.
	origStats := getHostStats
	getHostStats = func(string) HostStats {
		return HostStats{DiskTotal: 10 << 30, DiskUsed: (10 << 30) - (100 << 20)} // only 100 MiB free
	}
	t.Cleanup(func() { getHostStats = origStats })

	reqLowDisk := httptest.NewRequest(http.MethodPost, "/api/databases/1/import?db=app&name=small.sql", strings.NewReader("SELECT 1;"))
	reqLowDisk.SetPathValue("id", jsonNumber(dbID))
	err = srv.handleDBImport(httptest.NewRecorder(), reqLowDisk, admin)
	if ae, ok := err.(*apiError); !ok || ae.Status != http.StatusInsufficientStorage {
		t.Fatalf("import low disk err = %v, want 507", err)
	}
}

func TestHandleVerifyBackupSinglePassWebDAV(t *testing.T) {
	srv := newTestServer(t)
	plain := bytes.Repeat([]byte("INSERT INTO items VALUES (1, 'hello');\n"), 500)

	// Build an encrypted+zstd backup object into a local buffer using backup.Store via a WebDAV mock server.
	var stored []byte
	var getCalls atomic.Int32
	dav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			stored = b
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			getCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(stored)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dav.Close()

	confJSON, _ := json.Marshal(backup.TargetConfig{URL: dav.URL + "/dav/"})
	now := time.Now().Unix()
	res, err := srv.DB().Exec(`INSERT INTO storage_targets(name,kind,config,created_at) VALUES('dav-test','webdav',?,?)`, srv.box.Seal(string(confJSON)), now)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := res.LastInsertId()

	_, st, err := srv.getStorage(sid)
	if err != nil {
		t.Fatalf("getStorage: %v", err)
	}
	rcp, err := srv.recipients(true, "")
	if err != nil {
		t.Fatalf("recipients: %v", err)
	}
	storedRes, err := backup.Store(context.Background(), st, "demo/snap.sql.zst.age", bytes.NewReader(plain), rcp)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	bres, err := srv.DB().Exec(`INSERT INTO backups(source,label,storage_id,object_key,size,sha256,encrypted,format,status,started_at,finished_at) VALUES('{"kind":"panel"}','panel',?,?,?,?,1,'sqlite','success',?,?)`,
		sid, "demo/snap.sql.zst.age", storedRes.Size, storedRes.SHA256, now, now)
	if err != nil {
		t.Fatal(err)
	}
	bid, _ := bres.LastInsertId()

	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	req := httptest.NewRequest(http.MethodPost, "/api/backups/"+jsonNumber(bid)+"/verify", strings.NewReader(`{}`))
	req.SetPathValue("id", jsonNumber(bid))
	rec := httptest.NewRecorder()
	if err := srv.handleVerifyBackup(rec, req, admin); err != nil {
		t.Fatalf("handleVerifyBackup: %v", err)
	}
	var resp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.JobID == "" {
		t.Fatalf("unexpected verify response: %s", rec.Body.String())
	}

	// Wait for verify job to finish.
	deadline := time.Now().Add(5 * time.Second)
	var status, errMsg string
	for time.Now().Before(deadline) {
		_ = srv.DB().QueryRow(`SELECT status, error FROM jobs WHERE id=?`, resp.JobID).Scan(&status, &errMsg)
		if status != "queued" && status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "success" {
		t.Fatalf("verify job status = %q err = %q, want success", status, errMsg)
	}
	if got := getCalls.Load(); got != 1 {
		t.Fatalf("storage Get() calls = %d, want 1 (single-pass verify)", got)
	}
}

func TestHandleRestoreBackupValidatesMongoSourceDatabase(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now().Unix()
	res, err := srv.DB().Exec(`INSERT INTO projects(name,description,kind,source,webhook_token,compose,env,options,created_at,updated_at) VALUES('mg1','','database','inline','t','','','{}',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := res.LastInsertId()
	res, err = srv.DB().Exec(`INSERT INTO db_instances(project_id,engine,version,root_user,root_password,port,exposure,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		pid, "mongo", "8.0", "root", srv.box.Seal("pw"), 27017, "internal", now)
	if err != nil {
		t.Fatal(err)
	}
	dbID, _ := res.LastInsertId()
	bres, err := srv.DB().Exec(`INSERT INTO backups(source,label,storage_id,object_key,size,sha256,encrypted,format,status,started_at,finished_at) VALUES(?,'mg1/app',1,'mg/a.archive.zst',10,'abc',0,'archive','success',?,?)`,
		`{"kind":"database","instanceId":`+jsonNumber(dbID)+`,"database":"app"}`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	bid, _ := bres.LastInsertId()

	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	body := `{"kind":"database","mode":"new","instanceId":` + jsonNumber(dbID) + `,"database":"restored_db","sourceDatabase":"bad.* --drop"}`
	req := httptest.NewRequest(http.MethodPost, "/api/backups/"+jsonNumber(bid)+"/restore", strings.NewReader(body))
	req.SetPathValue("id", jsonNumber(bid))
	err = srv.handleRestoreBackup(httptest.NewRecorder(), req, admin)
	if ae, ok := err.(*apiError); !ok || ae.Status != 400 || !strings.Contains(ae.Msg, "源库名无效") {
		t.Fatalf("expected 400 源库名无效, got %v", err)
	}
}

func TestHandleTerminalOriginValidationResizeClampAndAudit(t *testing.T) {
	var resizeH, resizeW string
	var mu sync.Mutex
	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/c1/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "c1", "Name": "/web-1", "State": map[string]any{"Running": true}})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/c1/exec":
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "ex-term"})
		case r.Method == http.MethodPost && r.URL.Path == "/exec/ex-term/start":
			_, _ = io.ReadAll(r.Body)
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = brw.Flush()
			buf := make([]byte, 1024)
			for {
				if _, err := conn.Read(buf); err != nil {
					return
				}
			}
		case r.Method == http.MethodPost && r.URL.Path == "/exec/ex-term/resize":
			mu.Lock()
			resizeH = r.URL.Query().Get("h")
			resizeW = r.URL.Query().Get("w")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockDocker.Close()

	srv := newTestServer(t)
	srv.dk = docker.New("tcp://" + strings.TrimPrefix(mockDocker.URL, "http://"))
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// 1. Cross-origin and scheme-mismatched Origins must be rejected with 403.
	for _, badOrigin := range []string{"http://evil.example.com", "https://panel.local", "javascript://panel.local"} {
		req := httptest.NewRequest(http.MethodGet, "http://panel.local/api/containers/c1/terminal?shell=bash&user=root", nil)
		req.Host = "panel.local"
		req.Header.Set("Origin", badOrigin)
		req.SetPathValue("id", "c1")
		err := srv.handleTerminal(httptest.NewRecorder(), req, admin)
		if err != errForbidden {
			t.Fatalf("Origin %q err = %v, want errForbidden", badOrigin, err)
		}
	}

	// 2. Valid same-origin WebSocket session clamps resize to [1, 500] and records shell/user in audit log.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", "c1")
		if err := srv.handleTerminal(w, r, admin); err != nil {
			writeErr(w, err)
		}
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/containers/c1/terminal?shell=bash&user=www-data"
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","rows":9999,"cols":0}`)); err != nil {
		t.Fatalf("ws.Write resize: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		h, w := resizeH, resizeW
		mu.Unlock()
		if h != "" && w != "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	_ = ws.Close(websocket.StatusNormalClosure, "bye")

	mu.Lock()
	h, w := resizeH, resizeW
	mu.Unlock()
	if h != "500" || w != "1" {
		t.Fatalf("clamped resize h=%q w=%q, want 500 and 1", h, w)
	}

	// Check audit log recorded shell and user.
	deadline = time.Now().Add(2 * time.Second)
	var detail string
	for time.Now().Before(deadline) {
		_ = srv.DB().QueryRow(`SELECT detail FROM audit_log WHERE action='container.terminal' ORDER BY id ASC LIMIT 1`).Scan(&detail)
		if detail != "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !strings.Contains(detail, "shell=bash") || !strings.Contains(detail, "user=www-data") {
		t.Fatalf("audit detail = %q, want shell=bash and user=www-data", detail)
	}
}

func TestSampleMetricsTransactionalAndReloadSchedulesDrain(t *testing.T) {
	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "c1", "Names": []string{"/app-web-1"}, "Labels": map[string]string{"com.docker.compose.project": "app"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/containers/c1/stats":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cpu_stats": map[string]any{
					"cpu_usage":        map[string]any{"total_usage": 2000},
					"system_cpu_usage": 12000,
					"online_cpus":      1,
				},
				"precpu_stats": map[string]any{
					"cpu_usage":        map[string]any{"total_usage": 1000},
					"system_cpu_usage": 10000,
				},
				"memory_stats": map[string]any{"usage": 4096, "limit": 8192},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockDocker.Close()

	srv := newTestServer(t)
	srv.dk = docker.New("tcp://" + strings.TrimPrefix(mockDocker.URL, "http://"))
	ts := time.Now().Truncate(time.Minute).Unix()
	srv.sampleMetrics(context.Background(), ts)

	var count int
	_ = srv.DB().QueryRow(`SELECT count(*) FROM metrics WHERE ts=?`, ts).Scan(&count)
	if count != 2 {
		t.Fatalf("metrics rows inserted = %d, want 2 (host + app-web-1)", count)
	}

	// reloadSchedules cleanly stops previous cron and starts new one.
	srv.reloadSchedules()
	srv.reloadSchedules()
	srv.stopScheduler()
}
