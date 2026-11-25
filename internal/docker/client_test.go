package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientTransportConfiguresPerHostIdleConns(t *testing.T) {
	c := New("unix:///var/run/docker.sock")
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.http.Transport)
	}
	if tr.MaxIdleConnsPerHost < 8 {
		t.Errorf("MaxIdleConnsPerHost = %d, want >= 8", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout <= 0 {
		t.Errorf("IdleConnTimeout = %v, want > 0", tr.IdleConnTimeout)
	}
}

func TestImageRefPathEscapeAndValidation(t *testing.T) {
	var hits atomic.Int32
	var lastURI, lastQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastURI = r.RequestURI
		lastQuery = r.URL.RawQuery
		switch {
		case strings.HasPrefix(r.RequestURI, "/images/"):
			_, _ = w.Write([]byte(`{"Id":"sha256:abc","RepoTags":["nginx:latest"]}`))
		case strings.HasPrefix(r.RequestURI, "/distribution/"):
			_, _ = w.Write([]byte(`{"Descriptor":{"digest":"sha256:123456"}}`))
		default:
			http.Error(w, "unexpected endpoint: "+r.RequestURI, http.StatusForbidden)
		}
	}))
	defer srv.Close()

	c := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	ctx := context.Background()

	// 1. Standard image ref
	if _, err := c.InspectImage(ctx, "nginx:latest"); err != nil {
		t.Fatalf("InspectImage(nginx:latest): %v", err)
	}
	if lastURI != "/images/nginx:latest/json" || lastQuery != "" {
		t.Fatalf("nginx:latest URI=%q query=%q", lastURI, lastQuery)
	}
	if _, err := c.DistributionDigest(ctx, "nginx:latest", nil); err != nil {
		t.Fatalf("DistributionDigest(nginx:latest): %v", err)
	}
	if lastURI != "/distribution/nginx:latest/json" || lastQuery != "" {
		t.Fatalf("nginx:latest dist URI=%q query=%q", lastURI, lastQuery)
	}

	// 2. Registry/org/app ref must have slashes %2F-escaped
	if _, err := c.InspectImage(ctx, "ghcr.io/org/app:v1.2.3"); err != nil {
		t.Fatalf("InspectImage(ghcr.io/org/app:v1.2.3): %v", err)
	}
	if lastURI != "/images/ghcr.io%2Forg%2Fapp:v1.2.3/json" || lastQuery != "" {
		t.Fatalf("ghcr.io inspect URI=%q query=%q, want %%2F-escaped", lastURI, lastQuery)
	}
	if _, err := c.DistributionDigest(ctx, "ghcr.io/org/app:v1.2.3", nil); err != nil {
		t.Fatalf("DistributionDigest(ghcr.io/org/app:v1.2.3): %v", err)
	}
	if lastURI != "/distribution/ghcr.io%2Forg%2Fapp:v1.2.3/json" || lastQuery != "" {
		t.Fatalf("ghcr.io dist URI=%q query=%q, want %%2F-escaped", lastURI, lastQuery)
	}

	// 3. Query delimiter '?' must be %3F-escaped and never become a URL query string
	if _, err := c.InspectImage(ctx, "nginx:latest?all=1"); err != nil {
		t.Fatalf("InspectImage(?all=1): %v", err)
	}
	if lastURI != "/images/nginx:latest%3Fall=1/json" || lastQuery != "" {
		t.Fatalf("query injection inspect URI=%q query=%q, want %%3F-escaped and empty query", lastURI, lastQuery)
	}
	if _, err := c.DistributionDigest(ctx, "nginx:latest?all=1", nil); err != nil {
		t.Fatalf("DistributionDigest(?all=1): %v", err)
	}
	if lastURI != "/distribution/nginx:latest%3Fall=1/json" || lastQuery != "" {
		t.Fatalf("query injection dist URI=%q query=%q, want %%3F-escaped and empty query", lastURI, lastQuery)
	}

	// 4. Traversal / empty / control-char refs must be rejected before hitting Docker
	before := hits.Load()
	for _, badRef := range []string{"", "   ", "..", "../../containers/json?all=1", "repo/../other:1", "nginx:latest\r\nX-Injected: 1"} {
		if _, err := c.InspectImage(ctx, badRef); err == nil {
			t.Errorf("InspectImage(%q) succeeded, want error", badRef)
		}
		if _, err := c.DistributionDigest(ctx, badRef, nil); err == nil {
			t.Errorf("DistributionDigest(%q) succeeded, want error", badRef)
		}
	}
	if hits.Load() != before {
		t.Fatalf("malicious image refs triggered %d HTTP requests, want 0", hits.Load()-before)
	}
}
