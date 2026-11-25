package server

import (
	"bytes"
	"context"
	"encoding/binary"
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

	"compose-panel/internal/backup"
	"compose-panel/internal/docker"
)

type capturedDockerExec struct {
	Container   string
	Cmd         []string
	AttachStdin bool
	Stdin       []byte
}

// fakeDrillDockerServer implements the subset of Docker Engine API endpoints
// exercised by drillDatabase, runRestore, and runDBImport (inspect, create,
// start, remove, and exec with TCP hijack).
func fakeDrillDockerServer(t *testing.T, image string, execHandler func(cmd []string, stdin []byte) (string, int)) (*docker.Client, func() []capturedDockerExec) {
	t.Helper()
	var (
		mu      sync.Mutex
		nextID  int
		execMap = map[string]*capturedDockerExec{}
		exitMap = map[string]int{}
		history []*capturedDockerExec
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "cid-main",
				"Name":   "/pg-main-db-1",
				"Config": map[string]any{"Image": image},
			})
		case r.Method == http.MethodPost && p == "/containers/create":
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "cid-drill-1"})
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/containers/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/exec"):
			container := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/exec")
			var body struct {
				Cmd         []string `json:"Cmd"`
				AttachStdin bool     `json:"AttachStdin"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			nextID++
			id := fmt.Sprintf("exec-%d", nextID)
			rec := &capturedDockerExec{Container: container, Cmd: body.Cmd, AttachStdin: body.AttachStdin}
			execMap[id] = rec
			history = append(history, rec)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/start")
			_, _ = io.ReadAll(r.Body)
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", 500)
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = rw.Flush()

			mu.Lock()
			rec := execMap[id]
			mu.Unlock()

			var stdinData []byte
			if rec != nil && rec.AttachStdin {
				stdinData, _ = io.ReadAll(rw)
				mu.Lock()
				rec.Stdin = stdinData
				mu.Unlock()
			}
			stdout, code := "", 0
			if execHandler != nil && rec != nil {
				stdout, code = execHandler(rec.Cmd, stdinData)
			}
			mu.Lock()
			exitMap[id] = code
			mu.Unlock()
			if len(stdout) > 0 {
				var hdr [8]byte
				hdr[0] = 1
				binary.BigEndian.PutUint32(hdr[4:], uint32(len(stdout)))
				_, _ = conn.Write(hdr[:])
				_, _ = conn.Write([]byte(stdout))
			}
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/json")
			mu.Lock()
			code := exitMap[id]
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": code})
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dk := docker.New("tcp://" + srv.Listener.Addr().String())
	return dk, func() []capturedDockerExec {
		mu.Lock()
		defer mu.Unlock()
		out := make([]capturedDockerExec, len(history))
		for i, h := range history {
			out[i] = *h
		}
		return out
	}
}

func insertTestDBInstance(t *testing.T, srv *Server, name, engine string) int64 {
	t.Helper()
	p, err := srv.db.Exec(`INSERT INTO projects(name,kind,source,created_at,updated_at) VALUES(?,'database','inline',1,1)`, name)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := p.LastInsertId()
	d, err := srv.db.Exec(`INSERT INTO db_instances(project_id,engine,version,root_user,root_password,port,created_at) VALUES(?,?,'17','postgres','',5432,1)`, pid, engine)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := d.LastInsertId()
	return id
}

func runInJob(t *testing.T, srv *Server, fn func(run *jobRun) error) error {
	t.Helper()
	var fnErr error
	doneCh := make(chan struct{})
	id := srv.jobs.Submit(jobSpec("test", "test", "test", "admin", ""), func(run *jobRun) error {
		defer close(doneCh)
		fnErr = fn(run)
		return fnErr
	})
	if id == "" {
		t.Fatalf("failed to submit test job")
	}
	<-doneCh
	return fnErr
}

func TestDrillDatabaseSupportsPostgresPlainSQLAndPGDMP(t *testing.T) {
	cases := []struct {
		name        string
		payload     []byte
		wantRestore string // "psql" or "pg_restore"
	}{
		{
			name:        "plain sql backup",
			payload:     []byte("-- PostgreSQL dump\nCREATE TABLE users(id int);\n"),
			wantRestore: "psql",
		},
		{
			name:        "pgdump custom format",
			payload:     []byte("PGDMP\x01\x02binary-custom-dump"),
			wantRestore: "pg_restore",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			pgID := insertTestDBInstance(t, srv, "pg-drill", "postgres")
			dk, calls := fakeDrillDockerServer(t, "postgres:17-alpine", func(cmd []string, stdin []byte) (string, int) {
				if len(cmd) > 0 && cmd[0] == "psql" && strings.Contains(string(stdin), "information_schema.tables") {
					return "count\n4\n", 0
				}
				return "", 0
			})
			srv.dk = dk

			drill := &restoreDrill{ID: 1, MinRows: 1}
			b := &backupRow{
				Source: BackupSource{Kind: "database", InstanceID: pgID, Database: "appdb"},
			}
			open := func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(tc.payload)), nil
			}
			if err := runInJob(t, srv, func(run *jobRun) error {
				rep := &drillReport{run: run}
				return srv.drillDatabase(run, drill, b, rep, open, "panel-drill-test")
			}); err != nil {
				t.Fatalf("drillDatabase(%s) failed: %v", tc.name, err)
			}

			foundRestore := false
			for _, c := range calls() {
				if bytes.Equal(c.Stdin, tc.payload) {
					foundRestore = true
					if len(c.Cmd) == 0 || c.Cmd[0] != tc.wantRestore {
						t.Fatalf("restore dispatched to %v, want %s", c.Cmd, tc.wantRestore)
					}
				}
			}
			if !foundRestore {
				t.Fatalf("did not observe restore stream payload in docker exec calls: %+v", calls())
			}
		})
	}
}

func TestRunDBImportDelegatesToRestoreStream(t *testing.T) {
	srv := newTestServer(t)
	dk, calls := fakeDrillDockerServer(t, "postgres:17-alpine", nil)
	srv.dk = dk

	pgID := insertTestDBInstance(t, srv, "pg-imp", "postgres")
	myID := insertTestDBInstance(t, srv, "my-imp", "mysql")
	pgInst, err := srv.getDB(pgID)
	if err != nil {
		t.Fatal(err)
	}
	myInst, err := srv.getDB(myID)
	if err != nil {
		t.Fatal(err)
	}

	sqlFile := filepath.Join(t.TempDir(), "app.sql")
	sqlBody := []byte("CREATE TABLE t(id int);")
	if err := os.WriteFile(sqlFile, sqlBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInJob(t, srv, func(run *jobRun) error {
		return srv.runDBImport(run, pgInst, "appdb", sqlFile, false, int64(len(sqlBody)))
	}); err != nil {
		t.Fatalf("runDBImport(pg sql): %v", err)
	}

	dumpFile := filepath.Join(t.TempDir(), "app.dump")
	dumpBody := []byte("PGDMP\x01\x02dump")
	if err := os.WriteFile(dumpFile, dumpBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInJob(t, srv, func(run *jobRun) error {
		return srv.runDBImport(run, pgInst, "appdb", dumpFile, false, int64(len(dumpBody)))
	}); err != nil {
		t.Fatalf("runDBImport(pg dump): %v", err)
	}

	// MySQL must reject PGDMP before invoking docker exec.
	if err := runInJob(t, srv, func(run *jobRun) error {
		return srv.runDBImport(run, myInst, "appdb", dumpFile, false, int64(len(dumpBody)))
	}); err == nil {
		t.Fatalf("expected MySQL runDBImport with PGDMP to fail")
	}

	got := calls()
	if len(got) != 2 {
		t.Fatalf("expected 2 docker exec calls (psql + pg_restore), got %d", len(got))
	}
	if got[0].Cmd[0] != "psql" || !bytes.Equal(got[0].Stdin, sqlBody) {
		t.Fatalf("first import call = %v stdin=%q, want psql", got[0].Cmd, got[0].Stdin)
	}
	if got[1].Cmd[0] != "pg_restore" || !bytes.Equal(got[1].Stdin, dumpBody) {
		t.Fatalf("second import call = %v stdin=%q, want pg_restore", got[1].Cmd, got[1].Stdin)
	}
}

func TestRunRestorePostgresPlainSQLOverwriteDelegatesToRestoreStream(t *testing.T) {
	srv := newTestServer(t)
	pgID := insertTestDBInstance(t, srv, "pg-res", "postgres")
	dk, calls := fakeDrillDockerServer(t, "postgres:17-alpine", func(cmd []string, stdin []byte) (string, int) {
		s := string(stdin)
		switch {
		case strings.Contains(s, "FROM pg_database WHERE NOT datistemplate"):
			return "datname,pg_get_userbyid,pg_database_size\nappdb,postgres,1024\n", 0
		case strings.Contains(s, "pg_namespace"):
			return "nspname\npublic\n", 0
		case strings.Contains(s, "pg_database d JOIN pg_roles"):
			return "rolname\npostgres\n", 0
		default:
			return "", 0
		}
	})
	srv.dk = dk

	_, st, err := srv.getStorage(1)
	if err != nil {
		t.Fatal(err)
	}
	sqlBody := []byte("CREATE TABLE restored_ok(id int);\n")
	stored, err := backup.Store(context.Background(), st, "imported/pg-plain.sql.zst", bytes.NewReader(sqlBody), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &backupRow{
		ID:        1,
		Label:     "pg-plain",
		StorageID: 1,
		ObjectKey: "imported/pg-plain.sql.zst",
		Size:      stored.Size,
		SHA256:    stored.SHA256,
		Format:    "sql",
		Status:    "success",
		Source:    BackupSource{Kind: "database", InstanceID: pgID, Database: "appdb"},
	}
	plan := restorePlan{
		Kind:        "database",
		Mode:        "new",
		InstanceID:  pgID,
		Database:    "appdb_new",
		TargetLabel: "pg-res/appdb_new",
	}
	if err := runInJob(t, srv, func(run *jobRun) error {
		return srv.runRestore(run, b, plan, nil, "admin")
	}); err != nil {
		t.Fatalf("runRestore(new, plain SQL): %v", err)
	}
	got := calls()
	foundPsql := false
	for _, c := range got {
		if len(c.Cmd) > 0 && c.Cmd[0] == "psql" && bytes.Equal(c.Stdin, sqlBody) {
			foundPsql = true
		}
	}
	if !foundPsql {
		t.Fatalf("expected runRestore to dispatch plain SQL to psql via RestoreStream, calls=%+v", got)
	}
}

func TestImportUploadNormalizesFormatAliases(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Upload with alias format=dump for database kind -> normalized to pgdump.
	req := httptest.NewRequest(http.MethodPost, "/api/backups/import/upload?name=legacy.dump&kind=database&format=dump", strings.NewReader("PGDMP12345"))
	w := httptest.NewRecorder()
	if err := srv.handleImportUpload(w, req, admin); err != nil {
		t.Fatalf("handleImportUpload(format=dump): %v", err)
	}
	var resp struct {
		Backup backupRow `json:"backup"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal upload response: %v", err)
	}
	if resp.Backup.Format != "pgdump" {
		t.Fatalf("normalized format = %q, want pgdump", resp.Backup.Format)
	}
	if !backup.FormatCompatibleWithEngine(resp.Backup.Format, "postgres") || backup.FormatCompatibleWithEngine(resp.Backup.Format, "mysql") {
		t.Fatalf("unexpected compatibility for %q", resp.Backup.Format)
	}
	if time.Unix(resp.Backup.FinishedAt, 0).IsZero() {
		t.Fatalf("expected non-zero finishedAt")
	}
}
