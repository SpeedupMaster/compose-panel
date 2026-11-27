package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
