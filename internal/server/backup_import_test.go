package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
)

func TestImportUploadStreamingAndMagicAgeDetection(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Encrypt a SQL dump with an external age key, then upload it WITHOUT the .age suffix!
	extID, extRcpStr, err := backup.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	extRcp, _ := age.ParseX25519Recipient(extRcpStr)
	var encBuf bytes.Buffer
	aw, err := age.Encrypt(&encBuf, extRcp)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = aw.Write([]byte("CREATE TABLE t(id int);"))
	_ = aw.Close()
	payload := encBuf.Bytes()
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])

	req := httptest.NewRequest(http.MethodPost, "/api/backups/import/upload?name=renamed-backup.sql&label=my-import", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	if err := srv.handleImportUpload(w, req, admin); err != nil {
		t.Fatalf("handleImportUpload: %v", err)
	}
	var resp struct {
		Backup backupRow `json:"backup"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal upload response: %v", err)
	}
	b := resp.Backup
	if b.ID == 0 || b.Origin != "upload" || b.Status != "success" {
		t.Fatalf("unexpected uploaded backup row: %+v", b)
	}
	if !b.Encrypted {
		t.Errorf("expected magic age header detection to set Encrypted=true")
	}
	if b.SHA256 != wantSHA || b.Size != int64(len(payload)) {
		t.Errorf("sha/size = (%q, %d), want (%q, %d)", b.SHA256, b.Size, wantSHA, len(payload))
	}
	if b.Format != "sql" || b.Source.Kind != "database" {
		t.Errorf("unexpected kind/format: %+v / %q", b.Source, b.Format)
	}
	if !strings.HasPrefix(b.ObjectKey, "imported/") {
		t.Errorf("expected objectKey under imported/, got %q", b.ObjectKey)
	}

	// Verify with temporary ageIdentity succeeds.
	vBody, _ := json.Marshal(map[string]any{"ageIdentity": extID})
	vReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backups/%d/verify", b.ID), bytes.NewReader(vBody))
	vReq.SetPathValue("id", fmt.Sprint(b.ID))
	w = httptest.NewRecorder()
	if err := srv.handleVerifyBackup(w, vReq, admin); err != nil {
		t.Fatalf("handleVerifyBackup: %v", err)
	}
	var vr struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &vr)
	waitJobDone(t, srv, vr.JobID)
}

func TestScanImportStorageVerifyBackfillAndKeepFileDelete(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Point storage target 1 to a clean directory separate from DataDir (like /data/backups in production).
	backupsRoot := t.TempDir()
	confJSON, _ := json.Marshal(backup.TargetConfig{Path: backupsRoot})
	if _, err := srv.db.Exec(`UPDATE storage_targets SET config=? WHERE id=1`, srv.box.Seal(string(confJSON))); err != nil {
		t.Fatal(err)
	}

	// Seed files directly into local storage (id=1).
	extID, extRcpStr, err := backup.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	extRcp, _ := age.ParseX25519Recipient(extRcpStr)
	_, st, err := srv.getStorage(1)
	if err != nil {
		t.Fatal(err)
	}
	res, err := backup.Store(context.Background(), st, "pg-old_app/20261120-030000.pgdump.zst.age", strings.NewReader("fake-pgdump-bytes"), []age.Recipient{extRcp})
	if err != nil {
		t.Fatal(err)
	}
	panelSnap := filepath.Join(backupsRoot, "_panel", "panel-20261120-040000.db")
	_ = os.MkdirAll(filepath.Dir(panelSnap), 0o755)
	_ = os.WriteFile(panelSnap, []byte("sqlite-raw"), 0o600)

	// 1. Scan storage 1.
	scanReq := httptest.NewRequest(http.MethodPost, "/api/storage/1/scan", nil)
	scanReq.SetPathValue("id", "1")
	w := httptest.NewRecorder()
	if err := srv.handleScanStorage(w, scanReq, admin); err != nil {
		t.Fatalf("handleScanStorage: %v", err)
	}
	var scanResp struct {
		Items     []scannedBackupItem `json:"items"`
		Truncated bool                `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &scanResp); err != nil {
		t.Fatalf("unmarshal scan: %v", err)
	}
	if len(scanResp.Items) != 2 {
		t.Fatalf("expected 2 scanned items, got %d (%+v)", len(scanResp.Items), scanResp.Items)
	}
	byKey := map[string]scannedBackupItem{}
	for _, it := range scanResp.Items {
		byKey[it.ObjectKey] = it
	}
	pgItem, ok := byKey["pg-old_app/20261120-030000.pgdump.zst.age"]
	if !ok || !pgItem.Recognized || pgItem.Kind != "database" || pgItem.Format != "pgdump" || !pgItem.Encrypted || pgItem.Imported {
		t.Fatalf("unexpected pg scanned item: %+v", pgItem)
	}
	pItem, ok := byKey["_panel/panel-20261120-040000.db"]
	if !ok || !pItem.Recognized || pItem.Kind != "panel" || pItem.Format != "sqlite" || pItem.Encrypted {
		t.Fatalf("unexpected panel scanned item: %+v", pItem)
	}

	// 2. Import both items; second import of same list must skip both as duplicates.
	impBody, _ := json.Marshal(map[string]any{
		"storageId": 1,
		"items": []map[string]any{
			{"objectKey": pgItem.ObjectKey, "kind": pgItem.Kind, "format": pgItem.Format, "label": pgItem.Label, "encrypted": pgItem.Encrypted, "size": pgItem.Size, "startedAt": pgItem.StartedAt},
			{"objectKey": pItem.ObjectKey, "kind": pItem.Kind, "format": pItem.Format, "label": pItem.Label, "encrypted": pItem.Encrypted, "size": pItem.Size, "startedAt": pItem.StartedAt},
		},
	})
	w = httptest.NewRecorder()
	if err := srv.handleImportStorage(w, httptest.NewRequest(http.MethodPost, "/api/backups/import/storage", bytes.NewReader(impBody)), admin); err != nil {
		t.Fatalf("handleImportStorage 1: %v", err)
	}
	var impResp struct {
		Imported []backupRow `json:"imported"`
		Skipped  int         `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &impResp); err != nil {
		t.Fatal(err)
	}
	if len(impResp.Imported) != 2 || impResp.Skipped != 0 {
		t.Fatalf("first import got %d imported / %d skipped, want 2 / 0", len(impResp.Imported), impResp.Skipped)
	}
	var pgBackupID int64
	for _, b := range impResp.Imported {
		if b.Origin != "scan" {
			t.Errorf("expected origin=scan, got %q", b.Origin)
		}
		if b.ObjectKey == pgItem.ObjectKey {
			pgBackupID = b.ID
			if b.SHA256 != "" {
				t.Errorf("scanned backup sha256 should initially be empty, got %q", b.SHA256)
			}
		}
	}

	// Re-importing same items must skip them.
	w = httptest.NewRecorder()
	if err := srv.handleImportStorage(w, httptest.NewRequest(http.MethodPost, "/api/backups/import/storage", bytes.NewReader(impBody)), admin); err != nil {
		t.Fatalf("handleImportStorage 2: %v", err)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &impResp)
	if len(impResp.Imported) != 0 || impResp.Skipped != 2 {
		t.Fatalf("second import got %d imported / %d skipped, want 0 / 2", len(impResp.Imported), impResp.Skipped)
	}

	// 3. Verify scanned encrypted backup with temporary ageIdentity -> backfills SHA-256 and size.
	vBody, _ := json.Marshal(map[string]any{"ageIdentity": extID})
	vReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backups/%d/verify", pgBackupID), bytes.NewReader(vBody))
	vReq.SetPathValue("id", fmt.Sprint(pgBackupID))
	w = httptest.NewRecorder()
	if err := srv.handleVerifyBackup(w, vReq, admin); err != nil {
		t.Fatalf("handleVerifyBackup scanned: %v", err)
	}
	var vr struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &vr)
	waitJobDone(t, srv, vr.JobID)

	updated := srv.listBackups("WHERE b.id=?", pgBackupID)
	if len(updated) != 1 || updated[0].SHA256 != res.SHA256 || updated[0].Size != res.Size {
		t.Fatalf("verify did not backfill sha256/size: got %+v, want sha=%q size=%d", updated, res.SHA256, res.Size)
	}

	// 4. Delete with keepFile=1 removes DB record but preserves file on disk!
	delReq := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/backups/%d?keepFile=1", pgBackupID), nil)
	delReq.SetPathValue("id", fmt.Sprint(pgBackupID))
	w = httptest.NewRecorder()
	if err := srv.handleDeleteBackup(w, delReq, admin); err != nil {
		t.Fatalf("handleDeleteBackup keepFile=1: %v", err)
	}
	if len(srv.listBackups("WHERE b.id=?", pgBackupID)) != 0 {
		t.Fatalf("expected backup DB row removed")
	}
	diskPath := filepath.Join(backupsRoot, filepath.FromSlash(pgItem.ObjectKey))
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatalf("expected file on disk to remain with keepFile=1, stat err: %v", err)
	}
}

func waitJobDone(t *testing.T, srv *Server, jobID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		js := srv.queryJobs("WHERE id=?", jobID)
		if len(js) == 1 && js[0].Status != "queued" && js[0].Status != "running" {
			if js[0].Status != "success" {
				t.Fatalf("job %s ended with status=%s error=%s", jobID, js[0].Status, js[0].Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s timed out", jobID)
}
