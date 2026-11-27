package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogArchiveHookDeployReasonMapping(t *testing.T) {
	cases := []struct {
		trigger string
		fromDep int64
		want    string
	}{
		{"manual", 0, "deploy"},
		{"webhook", 0, "deploy"},
		{"create", 0, "deploy"},
		{"image_update", 0, "auto_update"},
		{"rollback", 0, "rollback"},
		{"manual", 42, "rollback"},
		{"image_update", 7, "rollback"},
	}
	for _, tc := range cases {
		if got := deployArchiveReason(tc.trigger, tc.fromDep); got != tc.want {
			t.Errorf("deployArchiveReason(%q, %d) = %q, want %q", tc.trigger, tc.fromDep, got, tc.want)
		}
	}
}

func TestLogArchiveHookProjectActionStepsMarkArchiveReason(t *testing.T) {
	p := &Project{Name: "demo", Kind: "app"}

	downPlan, err := buildProjectAction(p, "down", "")
	if err != nil {
		t.Fatalf("buildProjectAction(down): %v", err)
	}
	if len(downPlan.Steps) != 1 || downPlan.Steps[0].ArchiveReason != "down" {
		t.Fatalf("down plan steps = %+v, want ArchiveReason='down'", downPlan.Steps)
	}

	recreatePlan, err := buildProjectAction(p, "recreate", "web")
	if err != nil {
		t.Fatalf("buildProjectAction(recreate): %v", err)
	}
	if len(recreatePlan.Steps) != 2 {
		t.Fatalf("recreate plan steps len = %d, want 2", len(recreatePlan.Steps))
	}
	if recreatePlan.Steps[0].ArchiveReason != "" {
		t.Errorf("recreate pull step should not archive, got %q", recreatePlan.Steps[0].ArchiveReason)
	}
	if recreatePlan.Steps[1].ArchiveReason != "recreate" {
		t.Errorf("recreate up step ArchiveReason = %q, want 'recreate'", recreatePlan.Steps[1].ArchiveReason)
	}

	restartPlan, err := buildProjectAction(p, "restart", "web")
	if err != nil {
		t.Fatalf("buildProjectAction(restart): %v", err)
	}
	if restartPlan.Steps[0].ArchiveReason != "" {
		t.Errorf("restart step should not archive, got %q", restartPlan.Steps[0].ArchiveReason)
	}
}

func TestLogArchiveHookContainerRemoveCommitsOnSuccessAndDiscardsOnFailure(t *testing.T) {
	var mu sync.Mutex
	failDelete := false

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/cid-rm-1/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":   "cid-rm-1",
				"Name": "/standalone-rm",
				"State": map[string]any{
					"Status":    "running",
					"Running":   true,
					"StartedAt": "2026-11-27T01:00:00Z",
				},
				"Config": map[string]any{
					"Tty":    true,
					"Image":  "alpine:3.20",
					"Labels": map[string]string{},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/containers/cid-rm-1/logs":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "2026-11-27T01:00:01Z goodbye world\n")
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/cid-rm-1":
			mu.Lock()
			fail := failDelete
			mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"message":"busy"}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	srv := newTestServerWithDocker(t, mock.URL)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// 1. Removal fails -> staged temp file must be discarded and 0 archives committed.
	mu.Lock()
	failDelete = true
	mu.Unlock()
	reqFail := httptest.NewRequest(http.MethodPost, "/api/containers/cid-rm-1/remove", nil)
	reqFail.SetPathValue("id", "cid-rm-1")
	reqFail.SetPathValue("action", "remove")
	if err := srv.handleContainerAction(httptest.NewRecorder(), reqFail, admin); err == nil {
		t.Fatalf("expected error when docker remove fails")
	}
	var cnt int
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM container_log_archives`).Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("archives after failed remove = %d, want 0", cnt)
	}
	if entries, _ := os.ReadDir(srv.logArchiveDir()); len(entries) != 0 {
		t.Fatalf("temp files left after failed remove: %v", entries)
	}

	// 2. Removal succeeds -> archive committed with reason='remove'.
	mu.Lock()
	failDelete = false
	mu.Unlock()
	reqOK := httptest.NewRequest(http.MethodPost, "/api/containers/cid-rm-1/remove", nil)
	reqOK.SetPathValue("id", "cid-rm-1")
	reqOK.SetPathValue("action", "remove")
	if err := srv.handleContainerAction(httptest.NewRecorder(), reqOK, admin); err != nil {
		t.Fatalf("handleContainerAction(remove) error: %v", err)
	}
	var reason, name string
	if err := srv.db.QueryRow(`SELECT reason, container_name FROM container_log_archives WHERE container_id='cid-rm-1'`).Scan(&reason, &name); err != nil {
		t.Fatalf("query committed remove archive: %v", err)
	}
	if reason != "remove" || name != "standalone-rm" {
		t.Fatalf("got reason=%q name=%q, want remove / standalone-rm", reason, name)
	}
}

func TestLogArchiveHookDeleteProjectRemovesProjectArchives(t *testing.T) {
	mock, _ := newMockDockerForArchives(t, nil)
	srv := newTestServerWithDocker(t, mock.URL)

	now := time.Now().Unix()
	res, err := srv.db.Exec(`INSERT INTO projects(name,kind,source,compose,created_at,updated_at) VALUES('gone-proj','app','inline','services: {}',?,?)`, now, now)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	pid, _ := res.LastInsertId()

	// Seed an archive for gone-proj and one for keep-proj.
	for _, tc := range []struct{ id, proj string }{{"cla_1111", "gone-proj"}, {"cla_2222", "keep-proj"}} {
		if err := os.WriteFile(srv.logArchivePath(tc.id), []byte("gz"), 0o640); err != nil {
			t.Fatalf("write %s: %v", tc.id, err)
		}
		_, err := srv.db.Exec(`INSERT INTO container_log_archives(id,project,service,container_id,container_name,image,reason,started_at,archived_at,size_bytes,raw_bytes,line_count,truncated) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			tc.id, tc.proj, "web", "cid-"+tc.id, tc.proj+"-web-1", "img", "deploy", now-10, now, 2, 10, 1)
		if err != nil {
			t.Fatalf("insert archive %s: %v", tc.id, err)
		}
	}

	p, err := srv.getProject(pid)
	if err != nil {
		t.Fatalf("getProject: %v", err)
	}
	jobID := srv.deleteProjectJob(p, "admin", false, false)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var st string
		_ = srv.db.QueryRow(`SELECT status FROM jobs WHERE id=?`, jobID).Scan(&st)
		if st == "success" || st == "failed" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	var goneCnt, keepCnt int
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM container_log_archives WHERE project='gone-proj'`).Scan(&goneCnt)
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM container_log_archives WHERE project='keep-proj'`).Scan(&keepCnt)
	if goneCnt != 0 || keepCnt != 1 {
		t.Fatalf("after deleteProjectJob: goneCnt=%d keepCnt=%d, want 0 and 1", goneCnt, keepCnt)
	}
	if _, err := os.Stat(srv.logArchivePath("cla_1111")); !os.IsNotExist(err) {
		t.Errorf("gone-proj archive file was not removed")
	}
	if _, err := os.Stat(srv.logArchivePath("cla_2222")); err != nil {
		t.Errorf("keep-proj archive file should remain: %v", err)
	}
}

func writeFakeComposeScript(t *testing.T, dir, stateFile string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		scriptPath := filepath.Join(dir, "fake-compose.cmd")
		content := "@echo off\r\n" +
			"set ARGS= %* \r\n" +
			"echo %ARGS% | findstr /C:\" config \" >nul\r\n" +
			"if not errorlevel 1 goto do_config\r\n" +
			"echo %ARGS% | findstr /C:\" pull\" >nul\r\n" +
			"if not errorlevel 1 exit /b 0\r\n" +
			"echo %ARGS% | findstr /C:\" down\" >nul\r\n" +
			"if not errorlevel 1 goto do_down\r\n" +
			"echo %ARGS% | findstr /C:\" up \" >nul\r\n" +
			"if not errorlevel 1 goto do_up\r\n" +
			"exit /b 0\r\n" +
			":do_config\r\n" +
			"echo {\"name\":\"demo\",\"services\":{\"web\":{\"image\":\"app:v2\"},\"redis\":{\"image\":\"redis:7\"}}}\r\n" +
			"exit /b 0\r\n" +
			":do_down\r\n" +
			"echo down>>\"" + stateFile + "\"\r\n" +
			"exit /b 0\r\n" +
			":do_up\r\n" +
			"echo up>>\"" + stateFile + "\"\r\n" +
			"if not \"%FAKE_FAIL_UP%\"==\"1\" exit /b 0\r\n" +
			"if exist \"" + stateFile + ".failed\" exit /b 0\r\n" +
			"echo 1>\"" + stateFile + ".failed\"\r\n" +
			"exit /b 1\r\n"
		if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
			t.Fatalf("write fake-compose.cmd: %v", err)
		}
		return scriptPath
	}
	scriptPath := filepath.Join(dir, "fake-compose.sh")
	content := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *\" config \"*) echo '{\"name\":\"demo\",\"services\":{\"web\":{\"image\":\"app:v2\"},\"redis\":{\"image\":\"redis:7\"}}}'; exit 0 ;;\n" +
		"  *\" pull\"*) exit 0 ;;\n" +
		"  *\" down\"*) echo down >> \"" + stateFile + "\"; exit 0 ;;\n" +
		"  *\" up \"*)\n" +
		"    echo up >> \"" + stateFile + "\"\n" +
		"    if [ \"$FAKE_FAIL_UP\" = \"1\" ] && [ ! -f \"" + stateFile + ".failed\" ]; then\n" +
		"      echo 1 > \"" + stateFile + ".failed\"\n" +
		"      exit 1\n" +
		"    fi\n" +
		"    exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake-compose.sh: %v", err)
	}
	return scriptPath
}

func TestLogArchiveHookComposeUpPartialFailureAutoRollbackAndProjectActions(t *testing.T) {
	scriptDir := t.TempDir()
	stateFile := filepath.Join(scriptDir, "compose-state.txt")
	fakeBin := writeFakeComposeScript(t, scriptDir, stateFile)
	t.Setenv("PANEL_COMPOSE_BIN", fakeBin)
	t.Setenv("FAKE_FAIL_UP", "1")

	stateLines := func() []string {
		b, err := os.ReadFile(stateFile)
		if err != nil {
			return nil
		}
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if s := strings.TrimSpace(l); s != "" {
				out = append(out, s)
			}
		}
		return out
	}

	// Dynamic mock Docker: returns container snapshots driven by how many compose up/down steps ran.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ops := stateLines()
		ups, downs := 0, 0
		for _, op := range ops {
			if op == "up" {
				ups++
			} else if op == "down" {
				downs++
			}
		}
		var cur []fakeDockerContainer
		if downs > 0 {
			cur = nil
		} else {
			webID, webImg, webLog := "cid-web-v1", "app:v1", "2026-11-27T01:00:01Z v1-log\n"
			switch ups {
			case 1:
				webID, webImg, webLog = "cid-web-v2-bad", "app:v2", "2026-11-27T01:05:01Z v2-crash-log\n"
			case 2:
				webID, webImg, webLog = "cid-web-v1-rb", "app:v1", "2026-11-27T01:06:01Z rollback-ok\n"
			case 3:
				webID, webImg, webLog = "cid-web-recreated", "app:v3", "2026-11-27T01:07:01Z recreated-ok\n"
			}
			cur = []fakeDockerContainer{
				{ID: webID, Name: "demo-web-1", Image: webImg, Project: "demo", Service: "web", StartedAt: "2026-11-27T01:00:00Z", Running: true, Logs: webLog},
				{ID: "cid-redis-const", Name: "demo-redis-1", Image: "redis:7", Project: "demo", Service: "redis", StartedAt: "2026-11-27T01:00:00Z", Running: true, Logs: "2026-11-27T01:00:01Z redis\n"},
			}
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
			var out []map[string]any
			for _, c := range cur {
				out = append(out, map[string]any{
					"Id":     c.ID,
					"Names":  []string{"/" + c.Name},
					"Image":  c.Image,
					"State":  "running",
					"Status": "Up",
					"Labels": map[string]string{"com.docker.compose.project": c.Project, "com.docker.compose.service": c.Service},
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
			for _, c := range cur {
				if c.ID == id {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"Id":     c.ID,
						"Name":   "/" + c.Name,
						"State":  map[string]any{"Status": "running", "Running": true, "StartedAt": c.StartedAt},
						"Config": map[string]any{"Tty": true, "Image": c.Image, "Labels": map[string]string{"com.docker.compose.project": c.Project, "com.docker.compose.service": c.Service}},
					})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/logs"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/logs")
			for _, c := range cur {
				if c.ID == id {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, c.Logs)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/networks/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Id":"net1","Name":"panel_proxy"}`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer mock.Close()

	srv := newTestServerWithDocker(t, mock.URL)
	now := time.Now().Unix()
	composeYAML := "services:\n  web:\n    image: app:v2\n  redis:\n    image: redis:7\n"
	res, err := srv.db.Exec(`INSERT INTO projects(name,kind,source,compose,options,created_at,updated_at) VALUES('demo','app','inline',?,'{"pull":false,"autoRollback":true}',?,?)`, composeYAML, now, now)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	pid, _ := res.LastInsertId()
	// Seed a previous successful deployment so auto-rollback triggers on first failed composeUp.
	if _, err := srv.db.Exec(`INSERT INTO deployments(project_id,number,status,trigger,actor,compose,env,created_at,finished_at) VALUES(?,1,'success','manual','admin',?,'',?,?)`,
		pid, composeYAML, now-100, now-90); err != nil {
		t.Fatalf("seed prev deployment: %v", err)
	}

	waitJob := func(jobID string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var st string
			_ = srv.db.QueryRow(`SELECT status FROM jobs WHERE id=?`, jobID).Scan(&st)
			if st == "success" || st == "failed" || st == "canceled" {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("job %s timed out", jobID)
	}

	// 1. Deploy fails mid-up (replacing cid-web-v1 -> cid-web-v2-bad), then auto-rollbacks (cid-web-v2-bad -> cid-web-v1-rb).
	p, err := srv.getProject(pid)
	if err != nil {
		t.Fatalf("getProject: %v", err)
	}
	jobID, _ := srv.startDeploy(p, "image_update", "system", 0)
	waitJob(jobID)

	gotReasons := map[string]string{}
	rows, err := srv.db.Query(`SELECT container_id, reason FROM container_log_archives`)
	if err != nil {
		t.Fatalf("query archives after deploy+rollback: %v", err)
	}
	for rows.Next() {
		var cid, reason string
		_ = rows.Scan(&cid, &reason)
		gotReasons[cid] = reason
	}
	rows.Close()
	if gotReasons["cid-web-v1"] != "auto_update" || gotReasons["cid-web-v2-bad"] != "rollback" {
		logBytes, _ := os.ReadFile(filepath.Join(srv.cfg.DataDir, "jobs", jobID+".log"))
		t.Fatalf("after deploy+auto-rollback archives = %v, want cid-web-v1=auto_update and cid-web-v2-bad=rollback\nstate=%v\njobLog:\n%s", gotReasons, stateLines(), string(logBytes))
	}
	if _, untouchedArchived := gotReasons["cid-redis-const"]; untouchedArchived {
		t.Fatalf("untouched redis container must not be archived: %v", gotReasons)
	}

	// 2. Project action "recreate" (web only) replaces cid-web-v1-rb -> cid-web-recreated and archives with reason='recreate'.
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	runAction := func(action, service string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"action": action, "service": service})
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%d/action", pid), bytes.NewReader(body))
		req.SetPathValue("id", fmt.Sprint(pid))
		w := httptest.NewRecorder()
		if err := srv.handleProjectAction(w, req, admin); err != nil {
			t.Fatalf("handleProjectAction(%s): %v", action, err)
		}
		var out struct{ JobID string `json:"jobId"` }
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		waitJob(out.JobID)
	}

	runAction("recreate", "web")
	var recReason string
	if err := srv.db.QueryRow(`SELECT reason FROM container_log_archives WHERE container_id='cid-web-v1-rb'`).Scan(&recReason); err != nil || recReason != "recreate" {
		t.Fatalf("after recreate: cid-web-v1-rb reason=%q err=%v, want 'recreate'", recReason, err)
	}

	// 3. Project action "down" removes both remaining containers and archives both with reason='down'.
	runAction("down", "")
	for _, cid := range []string{"cid-web-recreated", "cid-redis-const"} {
		var r string
		if err := srv.db.QueryRow(`SELECT reason FROM container_log_archives WHERE container_id=?`, cid).Scan(&r); err != nil || r != "down" {
			t.Errorf("after down: %s reason=%q err=%v, want 'down'", cid, r, err)
		}
	}

	// Verify no .tmp files leaked in container-logs dir.
	entries, _ := os.ReadDir(srv.logArchiveDir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leaked temp file: %s", e.Name())
		}
	}
	_ = context.Background()
}
