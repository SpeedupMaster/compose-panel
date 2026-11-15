package server

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"compose-panel/internal/compose"
)

// SiteSuggestion is the panel's best guess, from a compose file, of which service a
// domain should proxy to, on which container port, and for which domains.
type SiteSuggestion struct {
	Service string `json:"service"`
	Port    int    `json:"port"`
	// Where Port came from: "label" (traefik/VIRTUAL_PORT), "compose" (expose/ports),
	// "image" (EXPOSE of a local image) or "known" (default port of a well-known image).
	PortFrom string           `json:"portFrom"`
	Domains  []string         `json:"domains"`
	Ports    map[string][]int `json:"ports"` // candidate ports per service, best first
	// PortFrom for each service's candidates.
	PortsFrom map[string]string `json:"portsFrom"`
}

type svcHint struct {
	ports    []int
	portFrom string
	domains  []string
	score    int
}

// Images that are never the HTTP entry point of a project.
var backendImages = map[string]bool{
	"postgres": true, "postgis": true, "mysql": true, "mariadb": true, "redis": true, "valkey": true,
	"mongo": true, "memcached": true, "etcd": true, "zookeeper": true, "kafka": true, "rabbitmq": true,
	"clickhouse-server": true, "elasticsearch": true, "opensearch": true, "influxdb": true,
}

// Default HTTP ports of common images, for when neither compose nor a local image says.
var knownImagePorts = map[string]int{
	"nginx": 80, "httpd": 80, "caddy": 80, "wordpress": 80, "drupal": 80, "joomla": 80, "mediawiki": 80,
	"nextcloud": 80, "phpmyadmin": 80, "adminer": 8080, "tomcat": 8080, "ghost": 2368, "grafana": 3000,
	"gitea": 3000, "forgejo": 3000, "metabase": 3000, "uptime-kuma": 3001, "n8n": 5678, "minio": 9000,
	"portainer-ce": 9000, "jellyfin": 8096, "filebrowser": 80, "kibana": 5601, "it-tools": 80, "excalidraw": 80,
	"traefik/whoami": 80, "vaultwarden/server": 80, "gotify/server": 80, "binwiederhier/ntfy": 80,
	"codercom/code-server": 8080, "requarks/wiki": 3000, "neosmemo/memos": 5230, "halohub/halo": 8090,
	"xhofe/alist": 5244, "louislam/uptime-kuma": 3001, "n8nio/n8n": 5678, "portainer/portainer-ce": 9000,
	"grafana/grafana": 3000, "gitea/gitea": 3000, "minio/minio": 9000, "metabase/metabase": 3000,
	"filebrowser/filebrowser": 80, "jellyfin/jellyfin": 8096, "corentinth/it-tools": 80, "excalidraw/excalidraw": 80,
}

// Preferred order when a service listens on several ports.
var httpPortRank = map[int]int{80: 1, 8080: 2, 3000: 3, 8000: 4, 5000: 5, 8081: 6, 8888: 7, 443: 50, 8443: 51}

var (
	traefikHostRe = regexp.MustCompile("Host\\(([^)]*)\\)")
	backtickRe    = regexp.MustCompile("`([^`]+)`")
	hostnameRe    = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z0-9-]{2,}$`)
	upstreamsRe   = regexp.MustCompile(`upstreams\s+(?:https?\s+)?(\d+)`)
)

// imageRepo strips registry host, "library/" and tag/digest: "ghcr.io/a/b:1" -> "a/b".
func imageRepo(image string) string {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	if parts := strings.SplitN(ref, "/", 2); len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		ref = parts[1]
	}
	return strings.TrimPrefix(strings.ToLower(ref), "library/")
}

func lastPath(s string) string { return s[strings.LastIndex(s, "/")+1:] }

// firstPort parses "3000", "3000/tcp", "3000-3005" or a JSON number; 0 if none or UDP.
func firstPort(v any) int {
	s := strings.TrimSpace(fmt.Sprint(v))
	if strings.HasSuffix(s, "/udp") {
		return 0
	}
	s = strings.TrimSuffix(s, "/tcp")
	if i := strings.IndexAny(s, "-/"); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return int(n)
}

func addPort(list []int, p int) []int {
	if p == 0 {
		return list
	}
	for _, x := range list {
		if x == p {
			return list
		}
	}
	return append(list, p)
}

func addDomains(list []string, raw string) []string {
	for _, d := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		d = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://"), "/"))
		if !hostnameRe.MatchString(d) {
			continue
		}
		dup := false
		for _, x := range list {
			dup = dup || x == d
		}
		if !dup {
			list = append(list, d)
		}
	}
	return list
}

func rankPorts(ports []int) {
	rank := func(p int) int {
		if r, ok := httpPortRank[p]; ok {
			return r
		}
		return 20
	}
	for i := 1; i < len(ports); i++ {
		for j := i; j > 0 && rank(ports[j]) < rank(ports[j-1]); j-- {
			ports[j], ports[j-1] = ports[j-1], ports[j]
		}
	}
}

func (s *Server) hintFor(ctx context.Context, name string, sv compose.ServiceConfig) svcHint {
	var h svcHint
	env := func(k string) string {
		if v := sv.Environment[k]; v != nil {
			return *v
		}
		return ""
	}

	// Domains written for another proxy (traefik, caddy-docker-proxy, nginx-proxy).
	for k, v := range sv.Labels {
		if strings.HasPrefix(k, "traefik.http.routers.") && strings.HasSuffix(k, ".rule") {
			for _, m := range traefikHostRe.FindAllStringSubmatch(v, -1) {
				for _, b := range backtickRe.FindAllStringSubmatch(m[1], -1) {
					h.domains = addDomains(h.domains, b[1])
				}
			}
		}
		if k == "caddy" && !strings.Contains(v, "{{") {
			h.domains = addDomains(h.domains, v)
		}
	}
	h.domains = addDomains(h.domains, env("VIRTUAL_HOST"))
	h.domains = addDomains(h.domains, env("LETSENCRYPT_HOST"))

	// Ports, strongest evidence first.
	for k, v := range sv.Labels {
		if strings.HasPrefix(k, "traefik.http.services.") && strings.HasSuffix(k, ".loadbalancer.server.port") {
			h.ports = addPort(h.ports, firstPort(v))
		}
		if k == "caddy.reverse_proxy" {
			if m := upstreamsRe.FindStringSubmatch(v); m != nil {
				h.ports = addPort(h.ports, firstPort(m[1]))
			}
		}
	}
	h.ports = addPort(h.ports, firstPort(env("VIRTUAL_PORT")))
	if len(h.ports) > 0 {
		h.portFrom = "label"
	} else {
		for _, e := range sv.Expose {
			h.ports = addPort(h.ports, firstPort(e))
		}
		for _, p := range sv.Ports {
			if proto, _ := p["protocol"].(string); proto == "udp" {
				continue
			}
			h.ports = addPort(h.ports, firstPort(p["target"]))
		}
		if len(h.ports) > 0 {
			h.portFrom = "compose"
		}
	}
	repo := imageRepo(sv.Image)
	if len(h.ports) == 0 && sv.Image != "" && s.dk != nil {
		ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if img, err := s.dk.InspectImage(ictx, sv.Image); err == nil {
			for k := range img.Config.ExposedPorts {
				h.ports = addPort(h.ports, firstPort(k))
			}
			if len(h.ports) > 0 {
				h.portFrom = "image"
			}
		}
		cancel()
	}
	if len(h.ports) == 0 {
		p, ok := knownImagePorts[repo]
		if !ok {
			p = knownImagePorts[lastPath(repo)]
		}
		if p > 0 {
			h.ports, h.portFrom = []int{p}, "known"
		}
	}
	rankPorts(h.ports)

	if backendImages[lastPath(repo)] {
		h.score = -100
	}
	if len(h.domains) > 0 {
		h.score += 100
	}
	h.score += map[string]int{"label": 50, "compose": 30, "image": 20, "known": 10}[h.portFrom]
	switch name {
	case "web", "app", "frontend", "nginx", "server", "ui", "site", "www", "proxy":
		h.score += 5
	}
	return h
}

// suggestSite picks the service, port and domains a new project's site most likely wants.
func (s *Server) suggestSite(ctx context.Context, cfg *compose.Config) *SiteSuggestion {
	names := cfg.ServiceNames()
	if len(names) == 0 {
		return nil
	}
	out := &SiteSuggestion{Ports: map[string][]int{}, PortsFrom: map[string]string{}, Domains: []string{}}
	best, bestScore := "", -1<<31
	hints := map[string]svcHint{}
	for _, n := range names {
		h := s.hintFor(ctx, n, cfg.Services[n])
		hints[n] = h
		out.Ports[n] = h.ports
		out.PortsFrom[n] = h.portFrom
		if h.ports == nil {
			out.Ports[n] = []int{}
		}
		if h.score > bestScore {
			best, bestScore = n, h.score
		}
	}
	h := hints[best]
	out.Service = best
	if len(h.ports) > 0 {
		out.Port, out.PortFrom = h.ports[0], h.portFrom
	}
	if h.domains != nil {
		out.Domains = h.domains
	}
	return out
}
