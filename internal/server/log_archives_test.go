package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"compose-panel/internal/config"
)

type fakeDockerContainer struct {
	ID        string
	Name      string
	Image     string
	Project   string
	Service   string
	Oneoff    string
	StartedAt string
	Running   bool
	Logs      string
}

func newMockDockerForArchives(t *testing.T, initial []fakeDockerContainer) (*httptest.Server, func([]fakeDockerContainer)) {
	t.Helper()
	var mu sync.Mutex
	containers := append([]fakeDockerContainer(nil), initial...)

	setContainers := func(next []fakeDockerContainer) {
		mu.Lock()
		containers = append([]fakeDockerContainer(nil), next...)
		mu.Unlock()
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		snap := append([]fakeDockerContainer(nil), containers...)
		mu.Unlock()

		switch {
		case r.URL.Path == "/containers/json":
			var out []map[string]any
			filtersRaw := r.URL.Query().Get("filters")
			var projectFilter string
			if filtersRaw != "" {
				var f map[string][]string
				if json.Unmarshal([]byte(filtersRaw), &f) == nil {
					for _, lbl := range f["label"] {
						if strings.HasPrefix(lbl, "com.docker.compose.project=") {
							projectFilter = strings.TrimPrefix(lbl, "com.docker.compose.project=")
						}
					}
				}
			}
			for _, c := range snap {
				if projectFilter != "" && c.Project != projectFilter {
					continue
				}
				labels := map[string]string{}
				if c.Project != "" {
					labels["com.docker.compose.project"] = c.Project
				}
				if c.Service != "" {
					labels["com.docker.compose.service"] = c.Service
				}
				if c.Oneoff != "" {
					labels["com.docker.compose.oneoff"] = c.Oneoff
				}
				state := "exited"
				if c.Running {
					state = "running"
				}
				out = append(out, map[string]any{
					"Id":     c.ID,
					"Names":  []string{"/" + c.Name},
					"Image":  c.Image,
					"State":  state,
					"Status": state,
					"Labels": labels,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)

		case strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
			for _, c := range snap {
				if c.ID == id {
					labels := map[string]string{}
					if c.Project != "" {
						labels["com.docker.compose.project"] = c.Project
					}
					if c.Service != "" {
						labels["com.docker.compose.service"] = c.Service
					}
					status := "exited"
					if c.Running {
						status = "running"
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"Id":   c.ID,
						"Name": "/" + c.Name,
						"State": map[string]any{
							"Status":    status,
							"Running":   c.Running,
							"StartedAt": c.StartedAt,
						},
						"Config": map[string]any{
							"Tty":    true,
							"Image":  c.Image,
							"Labels": labels,
						},
					})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"no such container"}`)

		case strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/logs"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/logs")
			for _, c := range snap {
				if c.ID == id {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, c.Logs)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, setContainers
}

func newTestServerWithDocker(t *testing.T, dockerURL string) *Server {
	t.Helper()
	tmp := t.TempDir()
	srv, err := New(config.Config{
		DataDir:     tmp,
		ProjectsDir: tmp,
		BackupsDir:  tmp,
		DockerHost:  "tcp://" + strings.TrimPrefix(dockerURL, "http://"),
	}, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() { _ = srv.DB().Close() })
	return srv
}

func TestLogArchiveTwoStageCommitAndDiscardUntouched(t *testing.T) {
	mock, setContainers := newMockDockerForArchives(t, []fakeDockerContainer{
		{
			ID:        "cid-web-old",
			Name:      "demo-web-1",
			Image:     "app:v1",
			Project:   "demo",
			Service:   "web",
			StartedAt: "2026-11-27T01:00:00Z",
			Running:   true,
			Logs:      "2026-11-27T01:00:01Z web line 1\n2026-11-27T01:00:02Z web line 2\n",
		},
		{
			ID:        "cid-redis-same",
			Name:      "demo-redis-1",
			Image:     "redis:7",
			Project:   "demo",
			Service:   "redis",
			StartedAt: "2026-11-27T01:00:00Z",
			Running:   true,
			Logs:      "2026-11-27T01:00:01Z redis ready\n",
		},
		{
			ID:        "cid-empty-old",
			Name:      "demo-empty-1",
			Image:     "busybox",
			Project:   "demo",
			Service:   "empty",
			StartedAt: "2026-11-27T01:00:00Z",
			Running:   true,
			Logs:      "",
		},
		{
			ID:        "cid-oneoff",
			Name:      "demo-web-run-1",
			Image:     "app:v1",
			Project:   "demo",
			Service:   "web",
			Oneoff:    "True",
			StartedAt: "2026-11-27T01:00:00Z",
			Running:   false,
			Logs:      "2026-11-27T01:00:01Z oneoff\n",
		},
	})
	srv := newTestServerWithDocker(t, mock.URL)

	staged := srv.stageProjectLogs(context.Background(), "demo", "", "deploy")
	// Only web and redis had non-empty non-oneoff logs.
	if len(staged) != 2 {
		t.Fatalf("staged count = %d, want 2", len(staged))
	}

	// Simulate compose up replacing web and removing empty, while keeping redis with the same ID.
	setContainers([]fakeDockerContainer{
		{
			ID:        "cid-web-new",
			Name:      "demo-web-1",
			Image:     "app:v2",
			Project:   "demo",
			Service:   "web",
			StartedAt: "2026-11-27T02:00:00Z",
			Running:   true,
			Logs:      "2026-11-27T02:00:01Z new web\n",
		},
		{
			ID:        "cid-redis-same",
			Name:      "demo-redis-1",
			Image:     "redis:7",
			Project:   "demo",
			Service:   "redis",
			StartedAt: "2026-11-27T01:00:00Z",
			Running:   true,
			Logs:      "2026-11-27T01:00:01Z redis ready\n",
		},
	})

	srv.finalizeProjectLogs(context.Background(), "demo", staged, nil)

	// Only cid-web-old should be committed; cid-redis-same's temp file must be removed.
	var count int
	if err := srv.db.QueryRow(`SELECT COUNT(*) FROM container_log_archives`).Scan(&count); err != nil {
		t.Fatalf("count archives: %v", err)
	}
	if count != 1 {
		t.Fatalf("committed archive count = %d, want 1", count)
	}
	var id, svc, cid, reason string
	var lines int
	if err := srv.db.QueryRow(`SELECT id, service, container_id, reason, line_count FROM container_log_archives`).Scan(&id, &svc, &cid, &reason, &lines); err != nil {
		t.Fatalf("query archive: %v", err)
	}
	if !validLogArchiveID(id) || svc != "web" || cid != "cid-web-old" || reason != "deploy" || lines != 2 {
		t.Fatalf("got (%q, %q, %q, %q, %d), unexpected", id, svc, cid, reason, lines)
	}
	entries, err := os.ReadDir(srv.logArchiveDir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != id+".log.gz" {
		t.Fatalf("archive dir entries = %v, want only %s.log.gz", entries, id)
	}

	// Re-staging the same container_id must be skipped as duplicate.
	dup := srv.stageContainerLog(context.Background(), logArchiveSource{
		ContainerID:   "cid-web-old",
		ContainerName: "demo-web-1",
		Project:       "demo",
		Service:       "web",
		Image:         "app:v1",
	}, "deploy")
	if dup != nil {
		srv.discardStagedLog(dup)
		t.Fatalf("expected duplicate container_id stage to return nil")
	}
}

func TestLogArchiveTruncationAndRetentionPruning(t *testing.T) {
	origMaxLines := logArchiveMaxLines
	origMaxRaw := logArchiveMaxRawBytes
	origMaxGroup := logArchiveMaxPerGroup
	origRet := logArchiveRetention
	origTotal := logArchiveMaxTotalBytes
	t.Cleanup(func() {
		logArchiveMaxLines = origMaxLines
		logArchiveMaxRawBytes = origMaxRaw
		logArchiveMaxPerGroup = origMaxGroup
		logArchiveRetention = origRet
		logArchiveMaxTotalBytes = origTotal
	})

	logArchiveMaxLines = 3
	logArchiveMaxRawBytes = 1024

	mock, _ := newMockDockerForArchives(t, []fakeDockerContainer{
		{
			ID:      "cid-trunc",
			Name:    "demo-app-1",
			Image:   "app:v1",
			Project: "demo",
			Service: "app",
			Logs:    "2026-11-27T01:00:01Z l1\n2026-11-27T01:00:02Z l2\n2026-11-27T01:00:03Z l3\n2026-11-27T01:00:04Z l4\n",
		},
	})
	srv := newTestServerWithDocker(t, mock.URL)

	st := srv.stageContainerLog(context.Background(), logArchiveSource{
		ContainerID:   "cid-trunc",
		ContainerName: "demo-app-1",
		Project:       "demo",
		Service:       "app",
		Image:         "app:v1",
	}, "recreate")
	if st == nil {
		t.Fatalf("expected staged archive")
	}
	if !st.truncated || st.lineCount != 3 {
		srv.discardStagedLog(st)
		t.Fatalf("got truncated=%v lineCount=%d, want true and 3", st.truncated, st.lineCount)
	}
	if err := srv.commitStagedLog(st); err != nil {
		t.Fatalf("commitStagedLog: %v", err)
	}

	// Seed multiple archives to test 30d age pruning, per-group count pruning, and global total size pruning.
	logArchiveMaxPerGroup = 2
	logArchiveRetention = 24 * time.Hour
	logArchiveMaxTotalBytes = 500 // bytes

	now := time.Now().Unix()
	writeFakeArchive := func(id, project, svc, name string, archivedAt, sizeBytes int64) {
		t.Helper()
		p := srv.logArchivePath(id)
		if err := os.WriteFile(p, []byte("fakegz"), 0o640); err != nil {
			t.Fatalf("write fake gz: %v", err)
		}
		_, err := srv.db.Exec(`INSERT INTO container_log_archives(id,project,service,container_id,container_name,image,reason,started_at,archived_at,size_bytes,raw_bytes,line_count,truncated) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			id, project, svc, "cid-"+id, name, "img", "deploy", archivedAt-60, archivedAt, sizeBytes, sizeBytes*2, 5)
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	// Clean previous row first.
	_, _ = srv.db.Exec(`DELETE FROM container_log_archives`)
	_ = os.Remove(srv.logArchivePath(st.id))

	writeFakeArchive("cla_0001", "p1", "web", "p1-web-1", now-int64((48*time.Hour).Seconds()), 100) // too old (>24h)
	writeFakeArchive("cla_0002", "p1", "web", "p1-web-1", now-300, 150)                            // 3rd in group -> pruned by per-group=2
	writeFakeArchive("cla_0003", "p1", "web", "p1-web-1", now-200, 200)                            // kept by group, then pruned by global 500B cap (oldest remaining)
	writeFakeArchive("cla_0004", "p1", "web", "p1-web-1", now-100, 200)                            // kept
	writeFakeArchive("cla_0005", "", "", "standalone-1", now-50, 200)                              // kept

	srv.pruneLogArchives()

	rows, err := srv.db.Query(`SELECT id FROM container_log_archives ORDER BY id`)
	if err != nil {
		t.Fatalf("query remaining: %v", err)
	}
	defer rows.Close()
	var kept []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		kept = append(kept, id)
	}
	if strings.Join(kept, ",") != "cla_0004,cla_0005" {
		t.Fatalf("kept archives = %v, want [cla_0004 cla_0005]", kept)
	}
	for _, gone := range []string{"cla_0001", "cla_0002", "cla_0003"} {
		if _, err := os.Stat(srv.logArchivePath(gone)); !os.IsNotExist(err) {
			t.Errorf("expected pruned archive file %s to be deleted", gone)
		}
	}
}

func TestLogArchiveOrphanAndMissingFileCleanup(t *testing.T) {
	mock, _ := newMockDockerForArchives(t, nil)
	srv := newTestServerWithDocker(t, mock.URL)

	now := time.Now()
	oldTime := now.Add(-orphanTempMaxAge - time.Hour)

	// 1. Stale .tmp file (>6h) -> removed; fresh .tmp -> kept.
	staleTmp := filepath.Join(srv.logArchiveDir(), "cla_aaaa.log.gz.tmp")
	freshTmp := filepath.Join(srv.logArchiveDir(), "cla_bbbb.log.gz.tmp")
	_ = os.WriteFile(staleTmp, []byte("tmp"), 0o640)
	_ = os.WriteFile(freshTmp, []byte("tmp"), 0o640)
	_ = os.Chtimes(staleTmp, oldTime, oldTime)

	// 2. Unreferenced stale .log.gz -> removed; unreferenced fresh .log.gz -> kept.
	staleOrphan := filepath.Join(srv.logArchiveDir(), "cla_cccc.log.gz")
	freshOrphan := filepath.Join(srv.logArchiveDir(), "cla_dddd.log.gz")
	_ = os.WriteFile(staleOrphan, []byte("gz"), 0o640)
	_ = os.WriteFile(freshOrphan, []byte("gz"), 0o640)
	_ = os.Chtimes(staleOrphan, oldTime, oldTime)

	// 3. DB row whose .log.gz file is missing on disk -> row deleted.
	_, err := srv.db.Exec(`INSERT INTO container_log_archives(id,project,service,container_id,container_name,image,reason,started_at,archived_at,size_bytes,raw_bytes,line_count,truncated) VALUES('cla_eeee','demo','web','cid-e','demo-web-1','img','deploy',10,20,10,20,1,0)`)
	if err != nil {
		t.Fatalf("insert dangling row: %v", err)
	}

	srv.cleanupLogArchiveOrphans(context.Background())

	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Errorf("stale .tmp was not deleted")
	}
	if _, err := os.Stat(freshTmp); err != nil {
		t.Errorf("fresh .tmp should be preserved: %v", err)
	}
	if _, err := os.Stat(staleOrphan); !os.IsNotExist(err) {
		t.Errorf("stale orphan .log.gz was not deleted")
	}
	if _, err := os.Stat(freshOrphan); err != nil {
		t.Errorf("fresh orphan .log.gz should be preserved: %v", err)
	}
	var cnt int
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM container_log_archives WHERE id='cla_eeee'`).Scan(&cnt)
	if cnt != 0 {
		t.Errorf("dangling DB row with missing file was not removed")
	}
}

func TestLogArchiveHTTPAPIRBACReadDownloadAndDelete(t *testing.T) {
	mock, _ := newMockDockerForArchives(t, []fakeDockerContainer{
		{
			ID:      "cid-live-a",
			Name:    "proj-a-web-1",
			Image:   "app:v2",
			Project: "proj-a",
			Service: "web",
			Running: true,
		},
	})
	srv := newTestServerWithDocker(t, mock.URL)

	now := time.Now().Unix()
	resA, err := srv.db.Exec(`INSERT INTO projects(name,kind,source,compose,created_at,updated_at) VALUES('proj-a','app','inline','services: {}',?,?)`, now, now)
	if err != nil {
		t.Fatalf("insert proj-a: %v", err)
	}
	projAID, _ := resA.LastInsertId()
	_, err = srv.db.Exec(`INSERT INTO projects(name,kind,source,compose,created_at,updated_at) VALUES('proj-b','app','inline','services: {}',?,?)`, now, now)
	if err != nil {
		t.Fatalf("insert proj-b: %v", err)
	}

	seedArchive := func(id, project, service, name string, archivedAt int64, lines []string) {
		t.Helper()
		f, err := os.Create(srv.logArchivePath(id))
		if err != nil {
			t.Fatalf("create archive file: %v", err)
		}
		gw := gzip.NewWriter(f)
		body := strings.Join(lines, "\n") + "\n"
		_, _ = io.WriteString(gw, body)
		_ = gw.Close()
		_ = f.Close()
		fi, _ := os.Stat(srv.logArchivePath(id))
		_, err = srv.db.Exec(`INSERT INTO container_log_archives(id,project,service,container_id,container_name,image,reason,started_at,archived_at,size_bytes,raw_bytes,line_count,truncated) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			id, project, service, "old-"+id, name, "img:v1", "deploy", archivedAt-60, archivedAt, fi.Size(), int64(len(body)), len(lines))
		if err != nil {
			t.Fatalf("insert archive %s: %v", id, err)
		}
	}

	seedArchive("cla_aa01", "proj-a", "web", "proj-a-web-1", now-10, []string{
		"2026-11-27T01:00:01Z a-1",
		"2026-11-27T01:00:02Z a-2",
		"2026-11-27T01:00:03Z a-3",
	})
	seedArchive("cla_bb01", "proj-b", "api", "proj-b-api-1", now-20, []string{"2026-11-27T01:00:01Z b-1"})
	seedArchive("cla_cc01", "", "", "standalone-box", now-30, []string{"2026-11-27T01:00:01Z c-1"})

	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}
	scopedViewer := &User{ID: 2, Username: "viewerA", Role: "viewer", Level: roleViewer, Projects: []int64{projAID}}
	scopedOp := &User{ID: 3, Username: "opA", Role: "operator", Level: roleOperator, Projects: []int64{projAID}}

	// 1. GET /api/containers/{id}/log-archives
	req := httptest.NewRequest(http.MethodGet, "/api/containers/cid-live-a/log-archives", nil)
	req.SetPathValue("id", "cid-live-a")
	w := httptest.NewRecorder()
	if err := srv.handleListContainerLogArchives(w, req, scopedViewer); err != nil {
		t.Fatalf("handleListContainerLogArchives: %v", err)
	}
	var list []logArchiveDTO
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "cla_aa01" {
		t.Fatalf("container archives = %+v, want [cla_aa01]", list)
	}

	// 2. GET /api/projects/{id}/log-archives
	reqP := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/projects/%d/log-archives", projAID), nil)
	reqP.SetPathValue("id", fmt.Sprint(projAID))
	wP := httptest.NewRecorder()
	if err := srv.handleListProjectLogArchives(wP, reqP, scopedViewer); err != nil {
		t.Fatalf("handleListProjectLogArchives: %v", err)
	}
	var pList []logArchiveDTO
	_ = json.Unmarshal(wP.Body.Bytes(), &pList)
	if len(pList) != 1 || pList[0].ID != "cla_aa01" {
		t.Fatalf("project archives = %+v, want [cla_aa01]", pList)
	}

	// 3. GET /api/log-archives respects RBAC: scopedViewer sees only proj-a, admin sees all 3.
	wAllScoped := httptest.NewRecorder()
	if err := srv.handleListAllLogArchives(wAllScoped, httptest.NewRequest(http.MethodGet, "/api/log-archives", nil), scopedViewer); err != nil {
		t.Fatalf("handleListAllLogArchives(scoped): %v", err)
	}
	var scopedAll []logArchiveDTO
	_ = json.Unmarshal(wAllScoped.Body.Bytes(), &scopedAll)
	if len(scopedAll) != 1 || scopedAll[0].ID != "cla_aa01" {
		t.Fatalf("scoped all archives = %+v, want only cla_aa01", scopedAll)
	}

	wAllAdmin := httptest.NewRecorder()
	if err := srv.handleListAllLogArchives(wAllAdmin, httptest.NewRequest(http.MethodGet, "/api/log-archives", nil), admin); err != nil {
		t.Fatalf("handleListAllLogArchives(admin): %v", err)
	}
	var adminAll []logArchiveDTO
	_ = json.Unmarshal(wAllAdmin.Body.Bytes(), &adminAll)
	if len(adminAll) != 3 {
		t.Fatalf("admin all archives len = %d, want 3", len(adminAll))
	}

	// 4. Scoped viewer must NOT access proj-b or standalone archive by direct ID (404).
	for _, forbiddenID := range []string{"cla_bb01", "cla_cc01", "../etc/passwd"} {
		rGet := httptest.NewRequest(http.MethodGet, "/api/log-archives/"+forbiddenID, nil)
		rGet.SetPathValue("id", forbiddenID)
		if err := srv.handleGetLogArchive(httptest.NewRecorder(), rGet, scopedViewer); err != errNotFound {
			t.Errorf("GET /api/log-archives/%s by scopedViewer err = %v, want errNotFound", forbiddenID, err)
		}
	}

	// 5. GET /api/log-archives/{id}?tail=2 returns last 2 lines and totalLines=3.
	rTail := httptest.NewRequest(http.MethodGet, "/api/log-archives/cla_aa01?tail=2", nil)
	rTail.SetPathValue("id", "cla_aa01")
	wTail := httptest.NewRecorder()
	if err := srv.handleGetLogArchive(wTail, rTail, scopedViewer); err != nil {
		t.Fatalf("handleGetLogArchive tail=2: %v", err)
	}
	var payload struct {
		Archive    logArchiveDTO `json:"archive"`
		Lines      []string      `json:"lines"`
		TotalLines int           `json:"totalLines"`
	}
	if err := json.Unmarshal(wTail.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode archive payload: %v", err)
	}
	if payload.TotalLines != 3 || len(payload.Lines) != 2 || payload.Lines[0] != "2026-11-27T01:00:02Z a-2" || payload.Lines[1] != "2026-11-27T01:00:03Z a-3" {
		t.Fatalf("unexpected tail payload: %+v", payload)
	}

	// 6. GET /api/log-archives/{id}?download=1 streams plain text attachment.
	rDl := httptest.NewRequest(http.MethodGet, "/api/log-archives/cla_aa01?download=1", nil)
	rDl.SetPathValue("id", "cla_aa01")
	wDl := httptest.NewRecorder()
	if err := srv.handleGetLogArchive(wDl, rDl, scopedViewer); err != nil {
		t.Fatalf("handleGetLogArchive download=1: %v", err)
	}
	if !strings.Contains(wDl.Header().Get("Content-Disposition"), "attachment;") || !strings.Contains(wDl.Body.String(), "a-1\n") || !strings.Contains(wDl.Body.String(), "a-3\n") {
		t.Fatalf("unexpected download headers=%v body=%q", wDl.Header(), wDl.Body.String())
	}

	// 7. Missing file on disk returns 404.
	_ = os.Remove(srv.logArchivePath("cla_bb01"))
	rMiss := httptest.NewRequest(http.MethodGet, "/api/log-archives/cla_bb01", nil)
	rMiss.SetPathValue("id", "cla_bb01")
	if err := srv.handleGetLogArchive(httptest.NewRecorder(), rMiss, admin); err == nil {
		t.Fatalf("expected error for missing archive file")
	}

	// 8. DELETE /api/log-archives/{id} removes row, file, and writes audit log.
	rDel := httptest.NewRequest(http.MethodDelete, "/api/log-archives/cla_aa01", nil)
	rDel.SetPathValue("id", "cla_aa01")
	wDel := httptest.NewRecorder()
	if err := srv.handleDeleteLogArchive(wDel, rDel, scopedOp); err != nil {
		t.Fatalf("handleDeleteLogArchive: %v", err)
	}
	if _, err := os.Stat(srv.logArchivePath("cla_aa01")); !os.IsNotExist(err) {
		t.Errorf("archive file still exists after DELETE")
	}
	var auditCnt int
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='container.log_archive_delete' AND detail='cla_aa01'`).Scan(&auditCnt)
	if auditCnt != 1 {
		t.Errorf("audit count = %d, want 1", auditCnt)
	}
}
