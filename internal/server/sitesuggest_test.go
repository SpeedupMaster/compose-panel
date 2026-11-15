package server

import (
	"context"
	"reflect"
	"testing"

	"compose-panel/internal/compose"
)

func strp(s string) *string { return &s }

func TestSuggestSite(t *testing.T) {
	s := &Server{} // no docker client: image EXPOSE lookup is skipped
	cases := []struct {
		name     string
		services map[string]compose.ServiceConfig
		service  string
		port     int
		from     string
		domains  []string
	}{
		{
			name: "known image, database skipped",
			services: map[string]compose.ServiceConfig{
				"db":   {Image: "postgres:16"},
				"blog": {Image: "ghost:5-alpine"},
			},
			service: "blog", port: 2368, from: "known", domains: []string{},
		},
		{
			name: "expose beats known default",
			services: map[string]compose.ServiceConfig{
				"web": {Image: "nginx:1.27", Expose: []any{"8080/tcp"}},
			},
			service: "web", port: 8080, from: "compose", domains: []string{},
		},
		{
			name: "ports target, udp ignored, http port preferred",
			services: map[string]compose.ServiceConfig{
				"app": {Image: "example/app:1", Ports: []map[string]any{{"target": float64(9001), "protocol": "udp"}, {"target": float64(9000)}, {"target": float64(3000)}}},
			},
			service: "app", port: 3000, from: "compose", domains: []string{},
		},
		{
			name: "traefik labels give port and domains",
			services: map[string]compose.ServiceConfig{
				"api": {Image: "ghcr.io/me/api:2", Labels: map[string]string{
					"traefik.http.routers.api.rule":                      "Host(`api.example.com`) || Host(`API.example.org`)",
					"traefik.http.services.api.loadbalancer.server.port": "8000",
				}},
				"web": {Image: "nginx:1.27"},
			},
			service: "api", port: 8000, from: "label", domains: []string{"api.example.com", "api.example.org"},
		},
		{
			name: "nginx-proxy env",
			services: map[string]compose.ServiceConfig{
				"site": {Image: "registry.local:5000/site:1", Environment: map[string]*string{"VIRTUAL_HOST": strp("a.example.com,b.example.com"), "VIRTUAL_PORT": strp("5000"), "EMPTY": nil}},
			},
			service: "site", port: 5000, from: "label", domains: []string{"a.example.com", "b.example.com"},
		},
		{
			name: "nothing known",
			services: map[string]compose.ServiceConfig{
				"worker": {Image: "me/worker:1"},
			},
			service: "worker", port: 0, from: "", domains: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := s.suggestSite(context.Background(), &compose.Config{Services: c.services})
			if got.Service != c.service || got.Port != c.port || got.PortFrom != c.from || !reflect.DeepEqual(got.Domains, c.domains) {
				t.Fatalf("got service=%q port=%d from=%q domains=%v; want %q %d %q %v", got.Service, got.Port, got.PortFrom, got.Domains, c.service, c.port, c.from, c.domains)
			}
		})
	}
}

func TestImageRepo(t *testing.T) {
	for in, want := range map[string]string{
		"nginx:1.27":                    "nginx",
		"library/nginx":                 "nginx",
		"ghcr.io/me/api:2":              "me/api",
		"registry.local:5000/site:1":    "site",
		"traefik/whoami@sha256:abc":     "traefik/whoami",
		"localhost/foo/bar:latest":      "foo/bar",
		"docker.io/library/postgres:16": "postgres",
	} {
		if got := imageRepo(in); got != want {
			t.Errorf("imageRepo(%q) = %q, want %q", in, got, want)
		}
	}
}
