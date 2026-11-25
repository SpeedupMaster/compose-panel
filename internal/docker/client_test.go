package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestContainerStatsOneShotCacheAndColdFallback(t *testing.T) {
	var oneShotCalls, fullCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/c1/stats" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("one-shot") == "1" {
			n := oneShotCalls.Add(1)
			if n == 1 {
				// Cold one-shot response has precpu_stats=0.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"cpu_stats": map[string]any{
						"cpu_usage":        map[string]any{"total_usage": 1000},
						"system_cpu_usage": 10000,
						"online_cpus":      2,
					},
					"precpu_stats": map[string]any{
						"cpu_usage":        map[string]any{"total_usage": 0},
						"system_cpu_usage": 0,
					},
				})
				return
			}
			// Subsequent warm one-shot call: delta cpu=500, delta sys=2000, 2 cpus => 50%.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cpu_stats": map[string]any{
					"cpu_usage":        map[string]any{"total_usage": 2500},
					"system_cpu_usage": 14000,
					"online_cpus":      2,
				},
				"precpu_stats": map[string]any{
					"cpu_usage":        map[string]any{"total_usage": 0},
					"system_cpu_usage": 0,
				},
			})
			return
		}
		// Non-one-shot cold fallback: delta cpu=1000, delta sys=2000, 2 cpus => 100%.
		fullCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cpu_stats": map[string]any{
				"cpu_usage":        map[string]any{"total_usage": 2000},
				"system_cpu_usage": 12000,
				"online_cpus":      2,
			},
			"precpu_stats": map[string]any{
				"cpu_usage":        map[string]any{"total_usage": 1000},
				"system_cpu_usage": 10000,
			},
		})
	}))
	defer srv.Close()

	c := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	ctx := context.Background()

	// Call 1 (cold): does one-shot + fallback so immediate CPU% is non-zero.
	st1, err := c.ContainerStats(ctx, "c1")
	if err != nil {
		t.Fatalf("cold ContainerStats error: %v", err)
	}
	if st1.CPUPercent < 99.9 || st1.CPUPercent > 100.1 {
		t.Fatalf("cold CPUPercent = %v, want 100", st1.CPUPercent)
	}
	if oneShotCalls.Load() != 1 || fullCalls.Load() != 1 {
		t.Fatalf("cold calls: oneShot=%d full=%d, want 1 and 1", oneShotCalls.Load(), fullCalls.Load())
	}

	// Call 2 (warm): ONLY one-shot=1 (no fallback!) and computes delta from prevCPU cache.
	st2, err := c.ContainerStats(ctx, "c1")
	if err != nil {
		t.Fatalf("warm ContainerStats error: %v", err)
	}
	if st2.CPUPercent < 49.9 || st2.CPUPercent > 50.1 {
		t.Fatalf("warm CPUPercent = %v, want 50", st2.CPUPercent)
	}
	if oneShotCalls.Load() != 2 || fullCalls.Load() != 1 {
		t.Fatalf("warm calls: oneShot=%d full=%d, want 2 and 1", oneShotCalls.Load(), fullCalls.Load())
	}

	// Cache pruning keeps prevCPU bounded <= maxPrevCPUCache.
	base := time.Now().Add(-20 * time.Minute)
	for i := 0; i < maxPrevCPUCache+50; i++ {
		c.setPrevCPU(fmt.Sprintf("ctr-%d", i), cpuSample{totalUsage: int64(i), systemUsage: int64(i + 1), seenAt: base.Add(time.Duration(i) * time.Millisecond)})
	}
	c.cpuMu.Lock()
	cacheLen := len(c.prevCPU)
	c.cpuMu.Unlock()
	if cacheLen > maxPrevCPUCache {
		t.Fatalf("prevCPU cache size = %d, want <= %d", cacheLen, maxPrevCPUCache)
	}
}

func TestExecResizeEscapesExecIDAndExecTTYHandshakeTimeout(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err := c.ExecResize(context.Background(), "ex/../../containers", 40, 120); err != nil {
		t.Fatalf("ExecResize error: %v", err)
	}
	if !strings.HasPrefix(gotURI, "/exec/ex%2F..%2F..%2Fcontainers/resize?") {
		t.Fatalf("ExecResize URI = %q, want escaped execID", gotURI)
	}

	// Stalled /exec/{id}/start upgrade must respect context deadline.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		// 1st conn: POST /containers/c1/exec
		conn1, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		_, _ = conn1.Read(buf)
		_, _ = conn1.Write([]byte("HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: 14\r\nConnection: close\r\n\r\n{\"Id\":\"ex123\"}"))
		_ = conn1.Close()

		// 2nd conn: POST /exec/ex123/start -> stall forever
		conn2, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn2.Close()
		time.Sleep(10 * time.Second)
	}()

	c2 := New("tcp://" + ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = c2.ExecTTY(ctx, "c1", []string{"sh"}, "")
	if err == nil {
		t.Fatalf("expected error from stalled ExecTTY upgrade")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("ExecTTY hung for %v despite deadline", time.Since(start))
	}
}
