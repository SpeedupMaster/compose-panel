package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"compose-panel/internal/backup"
	"compose-panel/internal/docker"
)

func fakeDockerServer(t *testing.T, containers []map[string]any, volumes []map[string]any) *docker.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(containers)
	})
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": volumes})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return docker.New("tcp://" + srv.Listener.Addr().String())
}

func TestRunningVolumeAndBindMountGuards(t *testing.T) {
	srv := newTestServer(t)
	projDir := filepath.Join(srv.cfg.ProjectsDir, "myapp")
	dataDir := filepath.Join(projDir, "data")
	childDir := filepath.Join(dataDir, "uploads")
	siblingDir := filepath.Join(projDir, "data2")

	srv.dk = fakeDockerServer(t, []map[string]any{
		{
			"Id":    "c1",
			"Names": []string{"/myapp-web-1"},
			"State": "running",
			"Mounts": []map[string]any{
				{"Type": "volume", "Name": "busy_vol", "Destination": "/var/lib/data"},
				{"Type": "bind", "Source": dataDir, "Destination": "/app/data"},
			},
		},
		{
			"Id":    "c2",
			"Names": []string{"/stopped-worker"},
			"State": "exited",
			"Mounts": []map[string]any{
				{"Type": "volume", "Name": "idle_vol", "Destination": "/data"},
				{"Type": "bind", "Source": siblingDir, "Destination": "/app/data2"},
			},
		},
	}, []map[string]any{
		{"Name": "busy_vol"},
		{"Name": "idle_vol"},
	})

	ctx := context.Background()
	holders, err := srv.runningVolumeHolders(ctx, "busy_vol")
	if err != nil {
		t.Fatalf("runningVolumeHolders(busy_vol): %v", err)
	}
	if len(holders) != 1 || holders[0] != "myapp-web-1" {
		t.Fatalf("busy_vol holders = %v, want [myapp-web-1]", holders)
	}
	holders, err = srv.runningVolumeHolders(ctx, "idle_vol")
	if err != nil || len(holders) != 0 {
		t.Fatalf("idle_vol holders = %v (err=%v), want empty", holders, err)
	}

	// Exact, parent, and child bind paths must match; sibling prefix must NOT match!
	for name, path := range map[string]string{
		"exact":  dataDir,
		"child":  childDir,
		"parent": projDir,
	} {
		bh, err := srv.runningBindHolders(ctx, path)
		if err != nil || len(bh) != 1 || bh[0] != "myapp-web-1" {
			t.Errorf("bind overlap (%s=%s) = %v (err=%v), want [myapp-web-1]", name, path, bh, err)
		}
	}
	bh, err := srv.runningBindHolders(ctx, siblingDir)
	if err != nil || len(bh) != 0 {
		t.Errorf("sibling dir %s falsely matched: %v (err=%v)", siblingDir, bh, err)
	}
}

func TestDirCrossProjectOverwriteAtomicSwapAndTempAgeKey(t *testing.T) {
	srv := newTestServer(t)
	srv.dk = fakeDockerServer(t, nil, nil)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Create an app project on disk with existing files in data/.
	res, err := srv.db.Exec(`INSERT INTO projects(name,kind,source,created_at,updated_at) VALUES('web','app','inline',1,1)`)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := res.LastInsertId()
	targetDir := filepath.Join(srv.cfg.ProjectsDir, "web", "data")
	_ = os.MkdirAll(targetDir, 0o755)
	_ = os.WriteFile(filepath.Join(targetDir, "old.txt"), []byte("old-version"), 0o644)

	// Create a tar backup from a fresh source directory encrypted with an external age key.
	srcTmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(srcTmp, "restored.txt"), []byte("new-version"), 0o644)
	extID, extRcpStr, err := backup.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	extRcp, _ := age.ParseX25519Recipient(extRcpStr)
	_, st, err := srv.getStorage(1)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := backup.Store(context.Background(), st, "imported/dir-snap.tar.zst.age", backup.TarDir(srcTmp), []age.Recipient{extRcp})
	if err != nil {
		t.Fatal(err)
	}
	bres, err := srv.db.Exec(`INSERT INTO backups(policy_id,source,label,storage_id,object_key,size,sha256,encrypted,format,status,started_at,finished_at,origin) VALUES(NULL,'{"kind":"dir"}','dir-snap',1,?,?,?,?,?,'success',?,?,'upload')`,
		"imported/dir-snap.tar.zst.age", stored.Size, stored.SHA256, 1, "tar", time.Now().Unix(), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	bid, _ := bres.LastInsertId()

	// Overwrite without matching confirm must fail.
	badBody, _ := json.Marshal(map[string]any{
		"kind": "dir", "mode": "overwrite", "projectId": pid, "path": "data", "confirm": "wrong", "ageIdentity": extID,
	})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backups/%d/restore", bid), bytes.NewReader(badBody))
	req.SetPathValue("id", fmt.Sprint(bid))
	w := httptest.NewRecorder()
	if err := srv.handleRestoreBackup(w, req, admin); err == nil {
		t.Fatalf("expected overwrite with wrong confirm to fail")
	}

	// Overwrite with matching confirm ("web/data") succeeds, creates pre-backup, and swaps files atomically.
	okBody, _ := json.Marshal(map[string]any{
		"kind": "dir", "mode": "overwrite", "projectId": pid, "path": "data", "confirm": "web/data", "ageIdentity": extID,
	})
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backups/%d/restore", bid), bytes.NewReader(okBody))
	req.SetPathValue("id", fmt.Sprint(bid))
	w = httptest.NewRecorder()
	if err := srv.handleRestoreBackup(w, req, admin); err != nil {
		t.Fatalf("handleRestoreBackup overwrite: %v", err)
	}
	var resp struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	waitJobDone(t, srv, resp.JobID)

	gotBytes, err := os.ReadFile(filepath.Join(targetDir, "restored.txt"))
	if err != nil || string(gotBytes) != "new-version" {
		t.Fatalf("restored.txt = %q (err=%v), want new-version", string(gotBytes), err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old.txt should be gone after atomic swap")
	}
	// Pre-backup record must exist (total backups >= 2).
	all := srv.listBackups("ORDER BY b.id DESC")
	if len(all) < 2 {
		t.Fatalf("expected pre-restore safety backup to be created, got %d backups", len(all))
	}
}

func TestRestoreValidationCrossInstanceAndLegacyPayload(t *testing.T) {
	srv := newTestServer(t)
	srv.dk = fakeDockerServer(t, nil, nil)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Create PG and MySQL instances in DB.
	p1, _ := srv.db.Exec(`INSERT INTO projects(name,kind,source,created_at,updated_at) VALUES('pg-main','database','inline',1,1)`)
	pid1, _ := p1.LastInsertId()
	d1, _ := srv.db.Exec(`INSERT INTO db_instances(project_id,engine,version,root_user,root_password,port,created_at) VALUES(?,'postgres','17','postgres','',5432,1)`, pid1)
	pgID, _ := d1.LastInsertId()

	p2, _ := srv.db.Exec(`INSERT INTO projects(name,kind,source,created_at,updated_at) VALUES('my-main','database','inline',1,1)`)
	pid2, _ := p2.LastInsertId()
	d2, _ := srv.db.Exec(`INSERT INTO db_instances(project_id,engine,version,root_user,root_password,port,created_at) VALUES(?,'mysql','8.4','root','',3306,1)`, pid2)
	myID, _ := d2.LastInsertId()

	// Imported pgdump backup with no original instanceId.
	bres, _ := srv.db.Exec(`INSERT INTO backups(policy_id,source,label,storage_id,object_key,size,sha256,encrypted,format,status,started_at,finished_at,origin) VALUES(NULL,'{"kind":"database"}','pgdump-ext',1,'imported/a.pgdump.zst',10,'',0,'pgdump','success',1,1,'upload')`)
	bid, _ := bres.LastInsertId()

	// Incompatible engine (pgdump -> mysql) must be rejected synchronously with 400.
	badBody, _ := json.Marshal(map[string]any{"kind": "database", "mode": "new", "instanceId": myID, "database": "app_new"})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backups/%d/restore", bid), bytes.NewReader(badBody))
	req.SetPathValue("id", fmt.Sprint(bid))
	w := httptest.NewRecorder()
	if err := srv.handleRestoreBackup(w, req, admin); err == nil {
		t.Fatalf("expected pgdump -> mysql mismatch to be rejected")
	}

	// Valid PG cross-instance request with invalid DB name must fail validation cleanly.
	badName, _ := json.Marshal(map[string]any{"kind": "database", "mode": "new", "instanceId": pgID, "database": "bad-name!"})
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backups/%d/restore", bid), bytes.NewReader(badName))
	req.SetPathValue("id", fmt.Sprint(bid))
	w = httptest.NewRecorder()
	if err := srv.handleRestoreBackup(w, req, admin); err == nil {
		t.Fatalf("expected invalid DB name to be rejected")
	}

	// Legacy payload on a dir backup with bound projectId: {mode: "new", target: "data-restored"}
	appRes, _ := srv.db.Exec(`INSERT INTO projects(name,kind,source,created_at,updated_at) VALUES('legacy-app','app','inline',1,1)`)
	appID, _ := appRes.LastInsertId()
	srcTmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(srcTmp, "hello.txt"), []byte("legacy-ok"), 0o644)
	_, st, _ := srv.getStorage(1)
	stored, err := backup.Store(context.Background(), st, "legacy-app_data/20261120.tar.zst", backup.TarDir(srcTmp), nil)
	if err != nil {
		t.Fatal(err)
	}
	srcJSON, _ := json.Marshal(BackupSource{Kind: "dir", ProjectID: appID, Path: "data"})
	b2, _ := srv.db.Exec(`INSERT INTO backups(policy_id,source,label,storage_id,object_key,size,sha256,encrypted,format,status,started_at,finished_at,origin) VALUES(NULL,?,'legacy-app/data',1,?,?,?,?,?,'success',1,1,'backup')`,
		string(srcJSON), "legacy-app_data/20261120.tar.zst", stored.Size, stored.SHA256, 0, "tar")
	bid2, _ := b2.LastInsertId()

	legacyBody := `{"mode":"new","target":"data-restore"}`
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backups/%d/restore", bid2), strings.NewReader(legacyBody))
	req.SetPathValue("id", fmt.Sprint(bid2))
	w = httptest.NewRecorder()
	if err := srv.handleRestoreBackup(w, req, admin); err != nil {
		t.Fatalf("legacy payload restore failed: %v", err)
	}
	var resp struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	waitJobDone(t, srv, resp.JobID)
	got, err := os.ReadFile(filepath.Join(srv.cfg.ProjectsDir, "legacy-app", "data-restore", "hello.txt"))
	if err != nil || string(got) != "legacy-ok" {
		t.Fatalf("legacy restored file = %q (err=%v), want legacy-ok", string(got), err)
	}
}
