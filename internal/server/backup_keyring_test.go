package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"filippo.io/age"

	"compose-panel/internal/backup"
)

func TestAgeKeyringImportRotateDeleteAndDecrypt(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Capture initial primary recipient.
	w := httptest.NewRecorder()
	if err := srv.handleBackupKey(w, httptest.NewRequest(http.MethodGet, "/api/backup-key", nil), admin); err != nil {
		t.Fatalf("handleBackupKey initial: %v", err)
	}
	var initial struct {
		Recipient string `json:"recipient"`
		ExtraKeys []any  `json:"extraKeys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &initial); err != nil {
		t.Fatalf("unmarshal initial: %v", err)
	}
	if initial.Recipient == "" {
		t.Fatalf("missing initial recipient")
	}

	// Generate two external identities (old server keys).
	oldID1, oldRcp1, err := backup.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	oldID2, oldRcp2, err := backup.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}

	// 1. Import oldID1 as decrypt-only historical key.
	body1, _ := json.Marshal(map[string]any{"label": "旧服务器-A", "identity": oldID1, "setPrimary": false})
	w = httptest.NewRecorder()
	if err := srv.handleImportBackupKey(w, httptest.NewRequest(http.MethodPost, "/api/backup-key/import", bytes.NewReader(body1)), admin); err != nil {
		t.Fatalf("import oldID1: %v", err)
	}

	// Duplicate import must fail.
	w = httptest.NewRecorder()
	if err := srv.handleImportBackupKey(w, httptest.NewRequest(http.MethodPost, "/api/backup-key/import", bytes.NewReader(body1)), admin); err == nil {
		t.Fatalf("duplicate import should fail")
	}

	// GET /api/backup-key must list extraKeys WITHOUT exposing private identities.
	w = httptest.NewRecorder()
	if err := srv.handleBackupKey(w, httptest.NewRequest(http.MethodGet, "/api/backup-key", nil), admin); err != nil {
		t.Fatalf("handleBackupKey after import: %v", err)
	}
	if strings.Contains(w.Body.String(), "AGE-SECRET-KEY-") {
		t.Fatalf("GET /api/backup-key without reveal=1 leaked secret key: %s", w.Body.String())
	}
	var listed struct {
		Recipient string `json:"recipient"`
		ExtraKeys []struct {
			ID        string `json:"id"`
			Label     string `json:"label"`
			Recipient string `json:"recipient"`
		} `json:"extraKeys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Recipient != initial.Recipient {
		t.Fatalf("primary recipient changed unexpectedly: got %q want %q", listed.Recipient, initial.Recipient)
	}
	if len(listed.ExtraKeys) != 1 || listed.ExtraKeys[0].Recipient != oldRcp1 || listed.ExtraKeys[0].Label != "旧服务器-A" {
		t.Fatalf("unexpected extraKeys: %+v", listed.ExtraKeys)
	}
	extraID1 := listed.ExtraKeys[0].ID

	// 2. Import oldID2 with setPrimary=true: old primary must rotate into extraKeys.
	body2, _ := json.Marshal(map[string]any{"label": "新主密钥-B", "identity": oldID2, "setPrimary": true})
	w = httptest.NewRecorder()
	if err := srv.handleImportBackupKey(w, httptest.NewRequest(http.MethodPost, "/api/backup-key/import", bytes.NewReader(body2)), admin); err != nil {
		t.Fatalf("import oldID2 primary: %v", err)
	}
	w = httptest.NewRecorder()
	if err := srv.handleBackupKey(w, httptest.NewRequest(http.MethodGet, "/api/backup-key", nil), admin); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Recipient != oldRcp2 {
		t.Fatalf("primary recipient = %q, want %q", listed.Recipient, oldRcp2)
	}
	if len(listed.ExtraKeys) != 2 {
		t.Fatalf("expected 2 extra keys after rotation, got %+v", listed.ExtraKeys)
	}

	// 3. Decrypt data encrypted to original initial primary, oldID1, and oldID2 via allAgeIdentities("").
	ids, err := srv.allAgeIdentities("")
	if err != nil {
		t.Fatalf("allAgeIdentities: %v", err)
	}
	st := &backup.Local{Root: t.TempDir()}
	for name, rcpStr := range map[string]string{"initial": initial.Recipient, "old1": oldRcp1, "newPrimary": oldRcp2} {
		rcp, _ := age.ParseX25519Recipient(rcpStr)
		if _, err := backup.Store(context.Background(), st, name+".sql.zst.age", strings.NewReader("payload-"+name), []age.Recipient{rcp}); err != nil {
			t.Fatalf("Store(%s): %v", name, err)
		}
		rc, err := backup.OpenObject(context.Background(), st, name+".sql.zst.age", true, ids...)
		if err != nil {
			t.Fatalf("OpenObject(%s): %v", name, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != "payload-"+name {
			t.Fatalf("decrypted %s = %q, want %q", name, string(got), "payload-"+name)
		}
	}

	// Invalid temp key must return error cleanly.
	if _, err := srv.allAgeIdentities("not-a-valid-key"); err == nil {
		t.Fatalf("expected error for invalid temp age key")
	}

	// 4. Delete extraID1.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/backup-key/extra/"+extraID1, nil)
	delReq.SetPathValue("id", extraID1)
	w = httptest.NewRecorder()
	if err := srv.handleDeleteExtraBackupKey(w, delReq, admin); err != nil {
		t.Fatalf("handleDeleteExtraBackupKey: %v", err)
	}
	w = httptest.NewRecorder()
	_ = srv.handleBackupKey(w, httptest.NewRequest(http.MethodGet, "/api/backup-key", nil), admin)
	_ = json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed.ExtraKeys) != 1 {
		t.Fatalf("expected 1 extra key after delete, got %+v", listed.ExtraKeys)
	}
}

func TestRevealMasterKeyAndBackupsOrigin(t *testing.T) {
	srv := newTestServer(t)
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Without reveal=1 returns exists: true without key value.
	w := httptest.NewRecorder()
	if err := srv.handleMasterKey(w, httptest.NewRequest(http.MethodGet, "/api/system/master-key", nil), admin); err != nil {
		t.Fatalf("handleMasterKey: %v", err)
	}
	var meta struct {
		Exists bool   `json:"exists"`
		Key    string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if !meta.Exists || meta.Key != "" {
		t.Fatalf("unexpected non-reveal response: %+v", meta)
	}

	// With reveal=1 returns 64-char hex key and logs audit event.
	w = httptest.NewRecorder()
	if err := srv.handleMasterKey(w, httptest.NewRequest(http.MethodGet, "/api/system/master-key?reveal=1", nil), admin); err != nil {
		t.Fatalf("handleMasterKey reveal: %v", err)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Key) != 64 {
		t.Fatalf("revealed master key len = %d, want 64", len(meta.Key))
	}
	var auditCount int
	_ = srv.db.QueryRow(`SELECT count(*) FROM audit_log WHERE action='system.reveal_master_key'`).Scan(&auditCount)
	if auditCount != 1 {
		t.Fatalf("expected 1 audit entry for system.reveal_master_key, got %d", auditCount)
	}

	// backups.origin column defaults to 'backup'.
	_, err := srv.db.Exec(`INSERT INTO backups(source,label,storage_id,status,started_at) VALUES('{"kind":"panel"}','panel',1,'success',1000)`)
	if err != nil {
		t.Fatalf("insert backup: %v", err)
	}
	rows := srv.listBackups("ORDER BY b.id DESC LIMIT 1")
	if len(rows) != 1 || rows[0].Origin != "backup" {
		t.Fatalf("expected Origin='backup', got %+v", rows)
	}
}
