package server

import (
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
