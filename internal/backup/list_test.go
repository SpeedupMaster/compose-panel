package backup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestLocalListWalkAndFilters(t *testing.T) {
	// Missing root returns empty slice without error.
	missing := &Local{Root: filepath.Join(t.TempDir(), "does-not-exist")}
	items, err := missing.List(context.Background())
	if err != nil {
		t.Fatalf("missing root List() error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("missing root len = %d, want 0", len(items))
	}

	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pg-main_app/20261120-030000.pgdump.zst.age", "encrypted-pg")
	write("_panel/panel-20261120-030000.db", "sqlite-snap")
	write(".panel-write-test", "probe")
	write("pg-main_app/inprogress.zst.part", "partial")
	write(".hidden/secret.sql", "skip-hidden")
	// Deeper than MaxListDepth (6) should be skipped.
	write("a/b/c/d/e/f/g/too-deep.sql", "deep")

	st := &Local{Root: root}
	got, err := st.List(context.Background())
	if err != nil {
		t.Fatalf("Local.List() error: %v", err)
	}
	var keys []string
	for _, it := range got {
		keys = append(keys, it.Key)
		if it.Size <= 0 {
			t.Errorf("item %s has size %d, want > 0", it.Key, it.Size)
		}
		if it.ModTime.IsZero() {
			t.Errorf("item %s has zero ModTime", it.Key)
		}
	}
	sort.Strings(keys)
	want := []string{"_panel/panel-20261120-030000.db", "pg-main_app/20261120-030000.pgdump.zst.age"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("Local.List keys = %v, want %v", keys, want)
	}
}

func TestWebDAVListDepth1Recursive(t *testing.T) {
	// Fake WebDAV server that ONLY supports Depth: 1 (rejects Depth: infinity with 403).
	mux := http.NewServeMux()
	mux.HandleFunc("/dav/backups/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			http.Error(w, "method not allowed", 405)
			return
		}
		if r.Header.Get("Depth") != "1" {
			http.Error(w, "Depth infinity forbidden", 403)
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(207)
		switch r.URL.Path {
		case "/dav/backups/":
			fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/dav/backups/</d:href>
    <d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/backups/pg%20main/</d:href>
    <d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/backups/.panel-write-test</d:href>
    <d:propstat><d:prop><d:getcontentlength>2</d:getcontentlength></d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/backups/root-dump.sql.zst</d:href>
    <d:propstat><d:prop><d:getcontentlength>1234</d:getcontentlength><d:getlastmodified>Fri, 20 Nov 2026 03:00:00 GMT</d:getlastmodified></d:prop></d:propstat>
  </d:response>
</d:multistatus>`)
		case "/dav/backups/pg main/":
			fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/dav/backups/pg%20main/</d:href>
    <d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/backups/pg%20main/20261120-030000.pgdump.zst.age</d:href>
    <d:propstat><d:prop><d:getcontentlength>4096</d:getcontentlength><d:getlastmodified>Fri, 20 Nov 2026 03:05:00 GMT</d:getlastmodified></d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/backups/pg%20main/tmp.part</d:href>
    <d:propstat><d:prop><d:getcontentlength>10</d:getcontentlength></d:prop></d:propstat>
  </d:response>
</d:multistatus>`)
		default:
			http.Error(w, "not found", 404)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dav, err := NewWebDAV(TargetConfig{URL: srv.URL + "/dav/backups/"})
	if err != nil {
		t.Fatalf("NewWebDAV: %v", err)
	}
	items, err := dav.List(context.Background())
	if err != nil && !errors.Is(err, ErrListTruncated) {
		t.Fatalf("WebDAV.List() error: %v", err)
	}
	byKey := map[string]ObjectInfo{}
	for _, it := range items {
		byKey[it.Key] = it
	}
	if len(byKey) != 2 {
		t.Fatalf("WebDAV.List returned %d items (%v), want 2", len(byKey), items)
	}
	if it, ok := byKey["pg main/20261120-030000.pgdump.zst.age"]; !ok || it.Size != 4096 || it.ModTime.IsZero() {
		t.Errorf("unexpected nested item: %+v", it)
	}
	if it, ok := byKey["root-dump.sql.zst"]; !ok || it.Size != 1234 || it.ModTime.IsZero() {
		t.Errorf("unexpected root item: %+v", it)
	}
}
