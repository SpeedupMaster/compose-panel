package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticHandlerPWA(t *testing.T) {
	s := &Server{web: fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>shell</title>")},
		"sw.js":                {Data: []byte("self.skipWaiting()")},
		"manifest.webmanifest": {Data: []byte(`{"name":"Compose 面板"}`)},
		"pwa-192x192.png":      {Data: []byte("\x89PNG\r\n\x1a\n")},
		"assets/index-abc.js":  {Data: []byte("console.log(1)")},
	}}
	h := s.staticHandler()

	cases := []struct {
		path         string
		status       int
		ctype, cache string
		body         string
	}{
		{"/sw.js", 200, "text/javascript", "no-cache", "skipWaiting"},
		{"/manifest.webmanifest", 200, "application/manifest+json", "no-cache", "Compose"},
		{"/pwa-192x192.png", 200, "image/png", "no-cache", ""},
		{"/assets/index-abc.js", 200, "javascript", "immutable", "console"},
		{"/assets/index-old.js", 404, "", "", ""},
		{"/projects/1", 200, "text/html", "no-cache", "shell"},
		{"/", 200, "text/html", "no-cache", "shell"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path, nil))
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.path, w.Code, c.status)
			continue
		}
		if c.ctype != "" && !strings.Contains(w.Header().Get("Content-Type"), c.ctype) {
			t.Errorf("%s: Content-Type %q, want %q", c.path, w.Header().Get("Content-Type"), c.ctype)
		}
		if c.cache != "" && !strings.Contains(w.Header().Get("Cache-Control"), c.cache) {
			t.Errorf("%s: Cache-Control %q, want %q", c.path, w.Header().Get("Cache-Control"), c.cache)
		}
		if c.body != "" && !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s: body %q does not contain %q", c.path, w.Body.String(), c.body)
		}
	}
}

func mustGzip(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStaticHandlerPrecompressedGzip(t *testing.T) {
	s := &Server{web: fstest.MapFS{
		"index.html.gz":           {Data: mustGzip(t, "<!doctype html><title>shell</title>")},
		"sw.js.gz":                {Data: mustGzip(t, "self.skipWaiting()")},
		"manifest.webmanifest.gz": {Data: mustGzip(t, `{"name":"Compose 面板"}`)},
		"assets/index-abc.js.gz":  {Data: mustGzip(t, "console.log('hello')")},
		"assets/app-abc.css.gz":   {Data: mustGzip(t, "body{margin:0}")},
		"pwa-192x192.png":         {Data: []byte("\x89PNG\r\n\x1a\n")},
	}}
	h := s.staticHandler()

	// 1. Client supporting gzip gets precompressed bytes directly.
	cases := []struct {
		path         string
		ctype, cache string
		wantPlain    string
	}{
		{"/assets/index-abc.js", "text/javascript", "immutable", "console.log('hello')"},
		{"/assets/app-abc.css", "text/css", "immutable", "body{margin:0}"},
		{"/sw.js", "text/javascript", "no-cache", "self.skipWaiting()"},
		{"/manifest.webmanifest", "application/manifest+json", "no-cache", "Compose"},
		{"/", "text/html", "no-cache", "<title>shell</title>"},
		{"/projects/42", "text/html", "no-cache", "<title>shell</title>"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s (gzip): status = %d, want 200", tc.path, w.Code)
		}
		if w.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("%s: Content-Encoding = %q, want gzip", tc.path, w.Header().Get("Content-Encoding"))
		}
		if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("%s: Vary = %q, want Accept-Encoding", tc.path, w.Header().Get("Vary"))
		}
		if !strings.Contains(w.Header().Get("Content-Type"), tc.ctype) {
			t.Errorf("%s: Content-Type = %q, want %q", tc.path, w.Header().Get("Content-Type"), tc.ctype)
		}
		if !strings.Contains(w.Header().Get("Cache-Control"), tc.cache) {
			t.Errorf("%s: Cache-Control = %q, want %q", tc.path, w.Header().Get("Cache-Control"), tc.cache)
		}
		gr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: invalid gzip body: %v", tc.path, err)
		}
		plain, _ := io.ReadAll(gr)
		gr.Close()
		if !strings.Contains(string(plain), tc.wantPlain) {
			t.Errorf("%s: decoded body %q does not contain %q", tc.path, string(plain), tc.wantPlain)
		}
	}

	// 2. Client without gzip gets transparently gunzipped plain response.
	for _, tc := range []struct {
		path, want string
	}{
		{"/assets/index-abc.js", "console.log('hello')"},
		{"/", "<title>shell</title>"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != 200 {
			t.Fatalf("%s (plain): status = %d, want 200", tc.path, w.Code)
		}
		if w.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s (plain): unexpected Content-Encoding %q", tc.path, w.Header().Get("Content-Encoding"))
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s (plain): body %q does not contain %q", tc.path, w.Body.String(), tc.want)
		}
	}

	// 3. Stale hashed asset still returns 404.
	w404 := httptest.NewRecorder()
	h.ServeHTTP(w404, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if w404.Code != 404 {
		t.Errorf("/assets/missing.js: status = %d, want 404", w404.Code)
	}
}
