package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCaddyIsLoaded(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantLoaded bool
		wantErr    bool
	}{
		{
			name:       "404 empty bootstrap config",
			status:     http.StatusNotFound,
			body:       `{"error":"invalid traversal path"}`,
			wantLoaded: false,
		},
		{
			name:       "null http app",
			status:     http.StatusOK,
			body:       "null\n",
			wantLoaded: false,
		},
		{
			name:       "empty object no servers",
			status:     http.StatusOK,
			body:       `{}`,
			wantLoaded: false,
		},
		{
			name:       "empty servers map",
			status:     http.StatusOK,
			body:       `{"servers":{}}`,
			wantLoaded: false,
		},
		{
			name:       "loaded main server",
			status:     http.StatusOK,
			body:       `{"servers":{"main":{"listen":[":443",":80"],"routes":[]}}}`,
			wantLoaded: true,
		},
		{
			name:    "500 server error",
			status:  http.StatusInternalServerError,
			body:    "internal error",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := &Caddy{http: &http.Client{Transport: rewriteHostTransport{base: srv.Client().Transport, target: srv.URL}}}
			loaded, err := c.IsLoaded(context.Background())
			if gotPath != "/config/apps/http" {
				t.Fatalf("path = %q, want /config/apps/http", gotPath)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("IsLoaded() err = %v, wantErr %v", err, tc.wantErr)
			}
			if loaded != tc.wantLoaded {
				t.Fatalf("IsLoaded() = %v, want %v", loaded, tc.wantLoaded)
			}
		})
	}

	t.Run("unreachable unix socket returns error", func(t *testing.T) {
		c := New(t.TempDir() + "/missing.sock")
		loaded, err := c.IsLoaded(context.Background())
		if err == nil || loaded {
			t.Fatalf("expected error on missing socket, got loaded=%v err=%v", loaded, err)
		}
	})
}

type rewriteHostTransport struct {
	base   http.RoundTripper
	target string
}

func (rt rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u, err := clone.URL.Parse(rt.target + req.URL.RequestURI())
	if err != nil {
		return nil, err
	}
	clone.URL = u
	clone.Host = u.Host
	return rt.base.RoundTrip(clone)
}
