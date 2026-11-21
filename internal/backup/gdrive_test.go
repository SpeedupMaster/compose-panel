package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGoogleDrive simulates Google OAuth2 token endpoint and Drive API v3 (files + resumable uploads).
type fakeGoogleDrive struct {
	mu            sync.Mutex
	tokenCalls    int
	validToken    string
	tokenErr      string
	tokenErrDesc  string
	nextID        int
	files         map[string]*fakeDriveFile // id -> file
	uploadSession map[string]*fakeUploadSession
	rangeHeaders  []string
	failFirst401  bool
	failed401Once bool
	exchangeReq   url.Values
	exchangeResp  map[string]any
}

type fakeDriveFile struct {
	ID       string
	Name     string
	MimeType string
	Parents  []string
	Trashed  bool
	Data     []byte
}

type fakeUploadSession struct {
	fileID   string // non-empty for PATCH update
	name     string
	parents  []string
	buf      []byte
	expected int64
}

func newFakeGoogleDrive() *fakeGoogleDrive {
	f := &fakeGoogleDrive{
		validToken:    "tok-1",
		files:         map[string]*fakeDriveFile{},
		uploadSession: map[string]*fakeUploadSession{},
	}
	f.files["root"] = &fakeDriveFile{ID: "root", Name: "My Drive", MimeType: "application/vnd.google-apps.folder"}
	return f
}

func (f *fakeGoogleDrive) addFolder(id, name, parent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[id] = &fakeDriveFile{ID: id, Name: name, MimeType: "application/vnd.google-apps.folder", Parents: []string{parent}}
}

func (f *fakeGoogleDrive) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", f.handleToken)
	mux.HandleFunc("/drive/v3/files", f.handleFiles)
	mux.HandleFunc("/drive/v3/files/", f.handleFileByID)
	mux.HandleFunc("/upload/drive/v3/files", f.handleUploadInit)
	mux.HandleFunc("/upload/drive/v3/files/", f.handleUploadInit)
	mux.HandleFunc("/upload/session/", f.handleUploadChunk)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGoogleDrive) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	body, _ := io.ReadAll(r.Body)
	vals, _ := url.ParseQuery(string(body))

	f.mu.Lock()
	defer f.mu.Unlock()

	if vals.Get("grant_type") == "authorization_code" {
		f.exchangeReq = vals
		if f.tokenErr != "" {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": f.tokenErr, "error_description": f.tokenErrDesc})
			return
		}
		if f.exchangeResp != nil {
			_ = json.NewEncoder(w).Encode(f.exchangeResp)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "1//fake-refresh-token",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
		return
	}

	f.tokenCalls++
	if f.tokenErr != "" {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": f.tokenErr, "error_description": f.tokenErrDesc})
		return
	}
	if vals.Get("grant_type") != "refresh_token" || vals.Get("client_id") == "" || vals.Get("client_secret") == "" || vals.Get("refresh_token") == "" {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_request"})
		return
	}
	f.validToken = fmt.Sprintf("tok-%d", f.tokenCalls)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": f.validToken,
		"expires_in":   3600,
		"token_type":   "Bearer",
	})
}

func (f *fakeGoogleDrive) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFirst401 && !f.failed401Once {
		f.failed401Once = true
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 401, "message": "Invalid Credentials"}})
		return false
	}
	if r.Header.Get("Authorization") != "Bearer "+f.validToken {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 401, "message": "Invalid Credentials"}})
		return false
	}
	return true
}

var (
	qNameRe   = regexp.MustCompile(`name\s*=\s*'((?:\\.|[^'])*)'`)
	qParentRe = regexp.MustCompile(`'((?:\\.|[^'])*)'\s+in\s+parents`)
)

func unescapeDriveQ(s string) string {
	s = strings.ReplaceAll(s, `\'`, `'`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

func (f *fakeGoogleDrive) handleFiles(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query().Get("q")
		var wantName, wantParent string
		if m := qNameRe.FindStringSubmatch(q); len(m) == 2 {
			wantName = unescapeDriveQ(m[1])
		}
		if m := qParentRe.FindStringSubmatch(q); len(m) == 2 {
			wantParent = unescapeDriveQ(m[1])
		}
		wantFolder := strings.Contains(q, "mimeType = 'application/vnd.google-apps.folder'")
		wantNotFolder := strings.Contains(q, "mimeType != 'application/vnd.google-apps.folder'")
		f.mu.Lock()
		var out []map[string]any
		for _, item := range f.files {
			if item.Trashed {
				continue
			}
			if wantName != "" && item.Name != wantName {
				continue
			}
			if wantParent != "" {
				matched := false
				for _, p := range item.Parents {
					if p == wantParent {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			if wantFolder && item.MimeType != "application/vnd.google-apps.folder" {
				continue
			}
			if wantNotFolder && item.MimeType == "application/vnd.google-apps.folder" {
				continue
			}
			out = append(out, map[string]any{
				"id":       item.ID,
				"name":     item.Name,
				"mimeType": item.MimeType,
				"size":     strconv.Itoa(len(item.Data)),
			})
		}
		f.mu.Unlock()
		if out == nil {
			out = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": out})

	case http.MethodPost:
		var meta struct {
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Parents  []string `json:"parents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("file-%d", f.nextID)
		f.files[id] = &fakeDriveFile{
			ID:       id,
			Name:     meta.Name,
			MimeType: meta.MimeType,
			Parents:  meta.Parents,
		}
		f.mu.Unlock()
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": meta.Name, "mimeType": meta.MimeType})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (f *fakeGoogleDrive) handleFileByID(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
	f.mu.Lock()
	item, ok := f.files[id]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 404, "message": "File not found"}})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("alt") == "media" {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(item.Data)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       item.ID,
			"name":     item.Name,
			"mimeType": item.MimeType,
			"trashed":  item.Trashed,
		})
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.files, id)
		f.mu.Unlock()
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (f *fakeGoogleDrive) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	if r.URL.Query().Get("uploadType") != "resumable" {
		http.Error(w, "expected uploadType=resumable", 400)
		return
	}
	fileID := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files")
	fileID = strings.TrimPrefix(fileID, "/")
	var meta struct {
		Name    string   `json:"name"`
		Parents []string `json:"parents"`
	}
	_ = json.NewDecoder(r.Body).Decode(&meta)

	f.mu.Lock()
	f.nextID++
	sid := fmt.Sprintf("sess-%d", f.nextID)
	f.uploadSession[sid] = &fakeUploadSession{
		fileID:  fileID,
		name:    meta.Name,
		parents: meta.Parents,
	}
	f.mu.Unlock()
	w.Header().Set("Location", "http://"+r.Host+"/upload/session/"+sid)
	w.WriteHeader(200)
}

func (f *fakeGoogleDrive) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", 405)
		return
	}
	sid := strings.TrimPrefix(r.URL.Path, "/upload/session/")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	cr := r.Header.Get("Content-Range")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rangeHeaders = append(f.rangeHeaders, cr)
	sess, ok := f.uploadSession[sid]
	if !ok {
		http.Error(w, "unknown session", 404)
		return
	}
	if cr == "bytes */0" {
		id := f.commitUploadLocked(sess, []byte{})
		delete(f.uploadSession, sid)
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "size": "0"})
		return
	}
	// Parse "bytes start-end/total" or "bytes start-end/*"
	if !strings.HasPrefix(cr, "bytes ") {
		http.Error(w, "missing Content-Range", 400)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(cr, "bytes "), "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad Content-Range", 400)
		return
	}
	bounds := strings.SplitN(parts[0], "-", 2)
	start, _ := strconv.Atoi(bounds[0])
	end, _ := strconv.Atoi(bounds[1])
	if start != len(sess.buf) || end-start+1 != len(body) {
		http.Error(w, fmt.Sprintf("range mismatch: got %d-%d len=%d cur=%d", start, end, len(body), len(sess.buf)), 400)
		return
	}
	sess.buf = append(sess.buf, body...)
	if parts[1] == "*" {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(sess.buf)-1))
		w.WriteHeader(308)
		return
	}
	total, _ := strconv.Atoi(parts[1])
	if len(sess.buf) != total {
		http.Error(w, fmt.Sprintf("final length %d != total %d", len(sess.buf), total), 400)
		return
	}
	id := f.commitUploadLocked(sess, sess.buf)
	delete(f.uploadSession, sid)
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "size": strconv.Itoa(len(sess.buf))})
}

func (f *fakeGoogleDrive) commitUploadLocked(sess *fakeUploadSession, data []byte) string {
	cp := append([]byte(nil), data...)
	if sess.fileID != "" {
		if existing, ok := f.files[sess.fileID]; ok {
			existing.Data = cp
			return existing.ID
		}
	}
	f.nextID++
	id := fmt.Sprintf("file-%d", f.nextID)
	f.files[id] = &fakeDriveFile{
		ID:       id,
		Name:     sess.name,
		MimeType: "application/octet-stream",
		Parents:  sess.parents,
		Data:     cp,
	}
	return id
}

func newTestGDrive(t *testing.T, srv *httptest.Server, cfg TargetConfig) *GDrive {
	t.Helper()
	if cfg.ClientID == "" {
		cfg.ClientID = "cid"
	}
	if cfg.ClientSecret == "" {
		cfg.ClientSecret = "csec"
	}
	if cfg.RefreshToken == "" {
		cfg.RefreshToken = "rtok"
	}
	g, err := NewGDrive(cfg)
	if err != nil {
		t.Fatalf("NewGDrive() error: %v", err)
	}
	g.tokenURL = srv.URL + "/token"
	g.apiBase = srv.URL + "/drive/v3"
	g.uploadBase = srv.URL + "/upload/drive/v3"
	return g
}

func TestOpenGDriveValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  TargetConfig
	}{
		{"missing clientId", TargetConfig{ClientSecret: "s", RefreshToken: "r"}},
		{"missing clientSecret", TargetConfig{ClientID: "c", RefreshToken: "r"}},
		{"missing refreshToken", TargetConfig{ClientID: "c", ClientSecret: "s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Open("gdrive", tt.cfg); err == nil {
				t.Fatalf("Open(gdrive, %+v) = nil, want error", tt.cfg)
			}
		})
	}
	st, err := Open("gdrive", TargetConfig{ClientID: "c", ClientSecret: "s", RefreshToken: "r"})
	if err != nil || st == nil {
		t.Fatalf("Open(gdrive, valid) = (%v, %v), want non-nil Storage and nil error", st, err)
	}
}

func TestNormalizeGDriveFolderID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"1AbCdEf_123-xyz", "1AbCdEf_123-xyz"},
		{"  1AbCdEf_123-xyz  ", "1AbCdEf_123-xyz"},
		{"https://drive.google.com/drive/folders/1AbCdEf_123-xyz?usp=sharing", "1AbCdEf_123-xyz"},
		{"https://drive.google.com/drive/u/0/folders/1AbCdEf_123-xyz/", "1AbCdEf_123-xyz"},
		{"https://drive.google.com/open?id=1AbCdEf_123-xyz", "1AbCdEf_123-xyz"},
	}
	for _, tt := range tests {
		if got := NormalizeGDriveFolderID(tt.in); got != tt.want {
			t.Errorf("NormalizeGDriveFolderID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGDrivePutGetDeleteRoundTrip(t *testing.T) {
	fake := newFakeGoogleDrive()
	fake.addFolder("target-dir", "Backups", "root")
	srv := fake.server(t)
	g := newTestGDrive(t, srv, TargetConfig{
		FolderID: "https://drive.google.com/drive/folders/target-dir?usp=sharing",
		Prefix:   "/panel/prod/",
	})
	ctx := context.Background()

	payload := []byte("hello google drive backup payload")
	n, err := g.Put(ctx, "pg_app/20261120-030000.sql.zst.age", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put() error: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("Put() size = %d, want %d", n, len(payload))
	}

	rc, err := g.Get(ctx, "pg_app/20261120-030000.sql.zst.age")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get() = %q, want %q", got, payload)
	}

	// Put a second file in the same folder; folder count should not duplicate.
	if _, err := g.Put(ctx, "pg_app/20261121-030000.sql.zst.age", bytes.NewReader([]byte("second"))); err != nil {
		t.Fatalf("second Put() error: %v", err)
	}
	folderCount := 0
	for _, item := range fake.files {
		if item.MimeType == "application/vnd.google-apps.folder" && item.Name == "pg_app" {
			folderCount++
		}
	}
	if folderCount != 1 {
		t.Fatalf("pg_app folder count = %d, want 1", folderCount)
	}

	if err := g.Delete(ctx, "pg_app/20261120-030000.sql.zst.age"); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if _, err := g.Get(ctx, "pg_app/20261120-030000.sql.zst.age"); err == nil {
		t.Fatalf("Get() after Delete() succeeded, want error")
	}
	// Idempotent delete on missing file and missing directory must not error or create folders.
	beforeFiles := len(fake.files)
	if err := g.Delete(ctx, "nonexistent_dir/missing.zst"); err != nil {
		t.Fatalf("Delete(missing) error: %v", err)
	}
	if len(fake.files) != beforeFiles {
		t.Fatalf("Delete(missing) created unexpected entries: before=%d after=%d", beforeFiles, len(fake.files))
	}
}

func TestGDrivePutOverwritesExistingFile(t *testing.T) {
	fake := newFakeGoogleDrive()
	srv := fake.server(t)
	g := newTestGDrive(t, srv, TargetConfig{})
	ctx := context.Background()

	if _, err := g.Put(ctx, "db/snap.zst", strings.NewReader("version-1")); err != nil {
		t.Fatalf("first Put() error: %v", err)
	}
	if _, err := g.Put(ctx, "db/snap.zst", strings.NewReader("version-2-longer")); err != nil {
		t.Fatalf("second Put() error: %v", err)
	}

	matches := 0
	for _, item := range fake.files {
		if item.Name == "snap.zst" {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("snap.zst count = %d, want 1 (should update in place)", matches)
	}

	rc, err := g.Get(ctx, "db/snap.zst")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "version-2-longer" {
		t.Fatalf("Get() = %q, want %q", got, "version-2-longer")
	}
}

func TestGDriveResumableChunkedUploadBoundaries(t *testing.T) {
	const chunk = 1024
	sizes := []struct {
		name       string
		size       int
		wantRanges []string
	}{
		{"empty", 0, []string{"bytes */0"}},
		{"smaller than chunk", 512, []string{"bytes 0-511/512"}},
		{"exact one chunk", chunk, []string{"bytes 0-1023/1024"}},
		{"one chunk plus one byte", chunk + 1, []string{"bytes 0-1023/*", "bytes 1024-1024/1025"}},
		{"exact two chunks", 2 * chunk, []string{"bytes 0-1023/*", "bytes 1024-2047/2048"}},
		{"two chunks plus remainder", 2*chunk + 333, []string{"bytes 0-1023/*", "bytes 1024-2047/*", "bytes 2048-2380/2381"}},
	}

	for _, tt := range sizes {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeGoogleDrive()
			srv := fake.server(t)
			g := newTestGDrive(t, srv, TargetConfig{})
			g.chunkSize = chunk

			data := make([]byte, tt.size)
			for i := range data {
				data[i] = byte((i*31 + 7) % 251)
			}
			n, err := g.Put(context.Background(), "chunks/file.bin", bytes.NewReader(data))
			if err != nil {
				t.Fatalf("Put(%d bytes) error: %v", tt.size, err)
			}
			if n != int64(tt.size) {
				t.Fatalf("Put() size = %d, want %d", n, tt.size)
			}
			if strings.Join(fake.rangeHeaders, ",") != strings.Join(tt.wantRanges, ",") {
				t.Fatalf("Content-Range sequence = %v, want %v", fake.rangeHeaders, tt.wantRanges)
			}
			rc, err := g.Get(context.Background(), "chunks/file.bin")
			if err != nil {
				t.Fatalf("Get() error: %v", err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, data) {
				t.Fatalf("downloaded %d bytes do not match uploaded %d bytes", len(got), len(data))
			}
		})
	}
}

func TestGDriveTokenCachingAndRetryOn401(t *testing.T) {
	fake := newFakeGoogleDrive()
	srv := fake.server(t)
	g := newTestGDrive(t, srv, TargetConfig{})
	ctx := context.Background()

	if _, err := g.Put(ctx, "a.bin", strings.NewReader("1")); err != nil {
		t.Fatalf("first Put() error: %v", err)
	}
	if _, err := g.Put(ctx, "b.bin", strings.NewReader("2")); err != nil {
		t.Fatalf("second Put() error: %v", err)
	}
	if fake.tokenCalls != 1 {
		t.Fatalf("tokenCalls = %d, want 1 (token should be cached)", fake.tokenCalls)
	}

	// Simulate expired/revoked access token -> Drive returns 401 once -> client refreshes and retries.
	fake.mu.Lock()
	fake.failFirst401 = true
	fake.mu.Unlock()

	rc, err := g.Get(ctx, "a.bin")
	if err != nil {
		t.Fatalf("Get() after 401 retry error: %v", err)
	}
	rc.Close()
	if fake.tokenCalls != 2 {
		t.Fatalf("tokenCalls after 401 retry = %d, want 2", fake.tokenCalls)
	}
}

func TestGDriveInvalidGrantError(t *testing.T) {
	fake := newFakeGoogleDrive()
	fake.tokenErr = "invalid_grant"
	fake.tokenErrDesc = "Token has been expired or revoked."
	srv := fake.server(t)
	g := newTestGDrive(t, srv, TargetConfig{})

	err := g.Test(context.Background())
	if err == nil {
		t.Fatalf("Test() = nil, want error for invalid_grant")
	}
	if !strings.Contains(err.Error(), "授权") {
		t.Fatalf("Test() error = %q, want user-friendly Chinese auth message", err.Error())
	}
}

func TestGDriveTestConnection(t *testing.T) {
	t.Run("valid folder writes and removes probe", func(t *testing.T) {
		fake := newFakeGoogleDrive()
		fake.addFolder("f-ok", "Backups", "root")
		srv := fake.server(t)
		g := newTestGDrive(t, srv, TargetConfig{FolderID: "f-ok", Prefix: "sub"})

		if err := g.Test(context.Background()); err != nil {
			t.Fatalf("Test() error: %v", err)
		}
		for _, item := range fake.files {
			if item.Name == ".panel-write-test" {
				t.Fatalf("Test() left behind .panel-write-test")
			}
		}
	})

	t.Run("non-folder id returns error", func(t *testing.T) {
		fake := newFakeGoogleDrive()
		fake.files["doc-1"] = &fakeDriveFile{ID: "doc-1", Name: "report.pdf", MimeType: "application/pdf"}
		srv := fake.server(t)
		g := newTestGDrive(t, srv, TargetConfig{FolderID: "doc-1"})

		if err := g.Test(context.Background()); err == nil {
			t.Fatalf("Test() = nil for non-folder ID, want error")
		}
	})

	t.Run("trashed folder returns error", func(t *testing.T) {
		fake := newFakeGoogleDrive()
		fake.files["f-trash"] = &fakeDriveFile{ID: "f-trash", Name: "Old", MimeType: "application/vnd.google-apps.folder", Trashed: true}
		srv := fake.server(t)
		g := newTestGDrive(t, srv, TargetConfig{FolderID: "f-trash"})

		if err := g.Test(context.Background()); err == nil {
			t.Fatalf("Test() = nil for trashed folder, want error")
		}
	})
}

func TestGDriveWithPipelineStoreVerifyOpenObject(t *testing.T) {
	fake := newFakeGoogleDrive()
	srv := fake.server(t)
	g := newTestGDrive(t, srv, TargetConfig{Prefix: "panel"})
	g.chunkSize = 1024

	id, rcp, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity() error: %v", err)
	}
	recipients, err := ParseRecipients(rcp)
	if err != nil {
		t.Fatalf("ParseRecipients() error: %v", err)
	}

	plain := bytes.Repeat([]byte("CREATE TABLE t(id INT); INSERT INTO t VALUES (1);\n"), 200)
	res, err := Store(context.Background(), g, "pg_prod/20261120.sql.zst.age", bytes.NewReader(plain), recipients)
	if err != nil {
		t.Fatalf("Store() error: %v", err)
	}
	if res.Size <= 0 || res.SHA256 == "" {
		t.Fatalf("Store() invalid result: %+v", res)
	}
	if err := Verify(context.Background(), g, "pg_prod/20261120.sql.zst.age", res.SHA256, res.Size); err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	rc, err := OpenObject(context.Background(), g, "pg_prod/20261120.sql.zst.age", true, id)
	if err != nil {
		t.Fatalf("OpenObject() error: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decrypted content mismatch: got %d bytes, want %d bytes", len(got), len(plain))
	}
}

func TestGDriveOAuthHelpers(t *testing.T) {
	authURL := GDriveAuthURL("my-client", "https://panel.example.com/api/storage/gdrive/callback", "st-123", "chal-xyz")
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("url.Parse() error: %v", err)
	}
	if u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
		t.Fatalf("unexpected auth URL base: %s", authURL)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":             "my-client",
		"redirect_uri":          "https://panel.example.com/api/storage/gdrive/callback",
		"response_type":         "code",
		"scope":                 GDriveScope,
		"access_type":           "offline",
		"prompt":                "consent",
		"state":                 "st-123",
		"code_challenge":        "chal-xyz",
		"code_challenge_method": "S256",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("auth URL param %s = %q, want %q", k, got, want)
		}
	}

	fake := newFakeGoogleDrive()
	srv := fake.server(t)

	tok, err := gdriveExchangeCode(context.Background(), srv.URL+"/token", "cid", "csec", "auth-code-1", "https://panel.example.com/api/storage/gdrive/callback", "verifier-1")
	if err != nil {
		t.Fatalf("gdriveExchangeCode() error: %v", err)
	}
	if tok != "1//fake-refresh-token" {
		t.Fatalf("refreshToken = %q, want %q", tok, "1//fake-refresh-token")
	}
	if fake.exchangeReq.Get("code") != "auth-code-1" || fake.exchangeReq.Get("code_verifier") != "verifier-1" {
		t.Fatalf("unexpected exchange request params: %v", fake.exchangeReq)
	}

	// Missing refresh_token in Google response should produce a clear error.
	fake.exchangeResp = map[string]any{"access_token": "only-access", "expires_in": 3600}
	if _, err := gdriveExchangeCode(context.Background(), srv.URL+"/token", "cid", "csec", "code-2", "https://panel.example.com/api/storage/gdrive/callback", "v2"); err == nil {
		t.Fatalf("gdriveExchangeCode() without refresh_token = nil, want error")
	}
}
