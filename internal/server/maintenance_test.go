package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compose-panel/internal/docker"
)

func TestHousekeepPrunesExpiredSessionsMetricsAndJobs(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()

	// Seed a user so sessions FK holds.
	res, err := srv.DB().Exec(`INSERT INTO users(username,email,password_hash,role,created_at) VALUES('admin','a@b.c','hash','admin',?)`, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()

	// Expired and active sessions.
	_, err = srv.DB().Exec(`INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES('expired',?,?,?,?)`,
		uid, now.Add(-48*time.Hour).Unix(), now.Add(-time.Hour).Unix(), now.Add(-2*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	_, err = srv.DB().Exec(`INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES('active',?,?,?,?)`,
		uid, now.Unix(), now.Add(24*time.Hour).Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}

	// Old and recent metrics.
	_, _ = srv.DB().Exec(`INSERT INTO metrics(ts,name) VALUES(?,'host')`, now.Add(-8*24*time.Hour).Unix())
	_, _ = srv.DB().Exec(`INSERT INTO metrics(ts,name) VALUES(?,'host')`, now.Add(-time.Hour).Unix())

	// Old finished job + log file.
	oldTS := now.Add(-40 * 24 * time.Hour).Unix()
	_, _ = srv.DB().Exec(`INSERT INTO jobs(id,kind,title,target,status,actor,created_at,finished_at) VALUES('job_old','test','old','t','success','admin',?,?)`, oldTS, oldTS)
	oldLog := filepath.Join(srv.cfg.DataDir, "jobs", "job_old.log")
	if err := os.WriteFile(oldLog, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	srv.housekeep(context.Background())

	var sessCount, metricCount, jobCount int
	_ = srv.DB().QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessCount)
	_ = srv.DB().QueryRow(`SELECT count(*) FROM metrics`).Scan(&metricCount)
	_ = srv.DB().QueryRow(`SELECT count(*) FROM jobs`).Scan(&jobCount)
	if sessCount != 1 {
		t.Errorf("sessions remaining = %d, want 1", sessCount)
	}
	if metricCount != 1 {
		t.Errorf("metrics remaining = %d, want 1", metricCount)
	}
	if jobCount != 0 {
		t.Errorf("jobs remaining = %d, want 0", jobCount)
	}
	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Errorf("expected %s to be deleted, stat err = %v", oldLog, err)
	}
}

func TestLoginFailEvictsExpiredKeys(t *testing.T) {
	srv := newTestServer(t)
	stale := time.Now().Add(-30 * time.Minute)
	srv.loginMu.Lock()
	srv.loginFail["stale1|1.1.1.1"] = []time.Time{stale}
	srv.loginFail["stale2|2.2.2.2"] = []time.Time{stale}
	srv.loginMu.Unlock()

	if srv.tooManyFailures("stale1|1.1.1.1") {
		t.Fatalf("stale key should not be rate limited")
	}
	srv.recordFailure("active|3.3.3.3")

	srv.loginMu.Lock()
	defer srv.loginMu.Unlock()
	if _, exists := srv.loginFail["stale1|1.1.1.1"]; exists {
		t.Errorf("expired key stale1 was not deleted from loginFail")
	}
	if _, exists := srv.loginFail["stale2|2.2.2.2"]; exists {
		t.Errorf("expired key stale2 was not evicted during recordFailure")
	}
	if len(srv.loginFail["active|3.3.3.3"]) != 1 {
		t.Errorf("active key failure count = %d, want 1", len(srv.loginFail["active|3.3.3.3"]))
	}
}

func TestAuditStreamExportAndVerify(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	for i := 0; i < 5; i++ {
		srv.audit(nil, admin, "test.act", fmt.Sprintf("t%d", i), "detail", nil)
	}

	w := httptest.NewRecorder()
	if err := srv.handleAuditVerify(w, httptest.NewRequest(http.MethodGet, "/api/audit/verify", nil), admin); err != nil {
		t.Fatalf("handleAuditVerify error: %v", err)
	}
	var vr struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &vr); err != nil || !vr.OK || vr.Count != 5 {
		t.Fatalf("unexpected verify result: %s", w.Body.String())
	}

	ew := httptest.NewRecorder()
	if err := srv.handleAuditExport(ew, httptest.NewRequest(http.MethodGet, "/api/audit/export", nil), admin); err != nil {
		t.Fatalf("handleAuditExport error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(ew.Body.String()), "\n")
	if len(lines) != 6 { // header + 5 rows
		t.Fatalf("CSV lines = %d, want 6", len(lines))
	}
}

func TestAccessLogBoundedCardinalityPreservesHotKeys(t *testing.T) {
	m := map[string]int{}
	for i := 0; i < 50; i++ {
		incBoundedCounter(m, "/api/hot", 100)
	}
	for i := 0; i < 500; i++ {
		incBoundedCounter(m, fmt.Sprintf("/scan/%d", i), 100)
	}
	if len(m) > 100 {
		t.Fatalf("bounded counter len = %d, want <= 100", len(m))
	}
	if m["/api/hot"] != 50 {
		t.Fatalf("hot key count = %d, want 50", m["/api/hot"])
	}
}

func TestHandleListVolumesSkipsContainerInspectAndAllStatsBoundsConcurrency(t *testing.T) {
	var inspects atomic.Int32
	var curStats, maxStats atomic.Int32

	const nContainers = 16
	mockDocker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/volumes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Volumes": []map[string]any{{"Name": "app_data", "Driver": "local"}},
			})
		case r.URL.Path == "/system/df":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Volumes": []map[string]any{{"Name": "app_data", "UsageData": map[string]any{"Size": 1024, "RefCount": 1}}},
			})
		case r.URL.Path == "/containers/json":
			cs := make([]map[string]any, 0, nContainers)
			for i := 0; i < nContainers; i++ {
				mounts := []map[string]any{}
				if i == 0 {
					mounts = append(mounts, map[string]any{"Type": "volume", "Name": "app_data"})
				}
				cs = append(cs, map[string]any{
					"Id":     fmt.Sprintf("cid%02d", i),
					"Names":  []string{fmt.Sprintf("/web-%d", i)},
					"State":  "running",
					"Mounts": mounts,
				})
			}
			_ = json.NewEncoder(w).Encode(cs)
		case strings.HasSuffix(r.URL.Path, "/json") && strings.HasPrefix(r.URL.Path, "/containers/"):
			inspects.Add(1)
			http.Error(w, "inspect should not be called", 500)
		case strings.HasSuffix(r.URL.Path, "/stats"):
			cur := curStats.Add(1)
			for {
				old := maxStats.Load()
				if cur <= old || maxStats.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
			curStats.Add(-1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"memory_stats": map[string]any{"usage": 2048, "limit": 4096},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockDocker.Close()

	srv := newTestServer(t)
	srv.dk = docker.New("tcp://" + strings.TrimPrefix(mockDocker.URL, "http://"))
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// 1. handleListVolumes must NOT call InspectContainer and must still report usedBy.
	w := httptest.NewRecorder()
	if err := srv.handleListVolumes(w, httptest.NewRequest(http.MethodGet, "/api/docker/volumes", nil), admin); err != nil {
		t.Fatalf("handleListVolumes error: %v", err)
	}
	if inspects.Load() != 0 {
		t.Fatalf("handleListVolumes called InspectContainer %d times, want 0", inspects.Load())
	}
	if !strings.Contains(w.Body.String(), `"web-0"`) {
		t.Fatalf("expected web-0 in usedBy, got %s", w.Body.String())
	}

	// 2. handleAllStats must bound concurrency to <= 8 and return all containers.
	sw := httptest.NewRecorder()
	if err := srv.handleAllStats(sw, httptest.NewRequest(http.MethodGet, "/api/containers/stats", nil), admin); err != nil {
		t.Fatalf("handleAllStats error: %v", err)
	}
	if maxStats.Load() > 8 {
		t.Fatalf("max concurrent /stats calls = %d, want <= 8", maxStats.Load())
	}
	var statsMap map[string]*docker.Stats
	if err := json.Unmarshal(sw.Body.Bytes(), &statsMap); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if len(statsMap) != nContainers {
		t.Fatalf("stats count = %d, want %d", len(statsMap), nContainers)
	}
}
