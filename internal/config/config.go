// Package config reads panel configuration from environment variables.
package config

import "os"

type Config struct {
	Listen        string // HTTP listen address of the panel
	DataDir       string // panel.db, master.key, job logs
	ProjectsDir   string // project working directories; must be the same path on the host
	BackupsDir    string // default local backup storage; must be the same path on the host
	DockerHost    string // Docker Engine API endpoint
	CaddyAdmin    string // Caddy admin API unix socket path
	CaddyDataDir  string // Caddy storage dir (read-only) for certificate inspection
	ProxyNetwork  string // shared network Caddy uses to reach project services
	DBNetwork     string // shared network for database instances
	PanelUpstream string // how Caddy reaches the panel itself
	HelperImage   string // small image used for volume tar/untar
	DevProxy      string // when set, non-API requests are proxied to this URL (Vite dev server)
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func Load() Config {
	return Config{
		Listen:        env("PANEL_LISTEN", ":7800"),
		DataDir:       env("PANEL_DATA_DIR", "/data/panel"),
		ProjectsDir:   env("PANEL_PROJECTS_DIR", "/data/projects"),
		BackupsDir:    env("PANEL_BACKUPS_DIR", "/data/backups"),
		DockerHost:    env("DOCKER_HOST", "unix:///var/run/docker.sock"),
		CaddyAdmin:    env("PANEL_CADDY_ADMIN", "/run/caddy/admin.sock"),
		CaddyDataDir:  env("PANEL_CADDY_DATA", "/caddy-data/caddy"),
		ProxyNetwork:  env("PANEL_PROXY_NETWORK", "panel-proxy"),
		DBNetwork:     env("PANEL_DB_NETWORK", "panel-db"),
		PanelUpstream: env("PANEL_UPSTREAM", "panel:7800"),
		HelperImage:   env("PANEL_HELPER_IMAGE", "alpine:3.20"),
		DevProxy:      os.Getenv("PANEL_DEV_PROXY"),
	}
}
