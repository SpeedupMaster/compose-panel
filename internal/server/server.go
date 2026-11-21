// Package server wires the HTTP API, background loops and the embedded web UI.
package server

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"compose-panel/internal/auth"
	"compose-panel/internal/backup"
	"compose-panel/internal/config"
	"compose-panel/internal/docker"
	"compose-panel/internal/jobs"
	"compose-panel/internal/notify"
	"compose-panel/internal/proxy"
	"compose-panel/internal/secret"
	"compose-panel/internal/store"
)

const Version = "0.1.0"

type Server struct {
	cfg    config.Config
	db     *sql.DB
	box    *secret.Box
	dk     *docker.Client
	caddy  *proxy.Caddy
	jobs   *jobs.Manager
	notify *notify.Notifier
	web    fs.FS

	cronMu   sync.Mutex
	cron     *cron.Cron
	cronIDs  map[string]cron.EntryID
	cronMeta map[cron.EntryID]scheduleMeta

	loginMu   sync.Mutex
	loginFail map[string][]time.Time

	auditMu        sync.Mutex
	caddyMu        sync.Mutex
	maintMu        sync.Mutex
	deployMaint    map[int64]bool // projects currently deploying behind a maintenance page
	gdriveMu       sync.Mutex
	gdriveAuth     map[string]*gdriveOAuthState
	gdriveExchange func(ctx context.Context, clientID, clientSecret, code, redirectURI, verifier string) (string, error)
	startedAt      time.Time
	setupToken     string
}

type scheduleMeta struct {
	Kind, Name, Spec, Target string
}

// New opens the database and prepares all components.
func New(cfg config.Config, web fs.FS) (*Server, error) {
	for _, d := range []string{cfg.DataDir, cfg.ProjectsDir, cfg.BackupsDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "panel.db"))
	if err != nil {
		return nil, err
	}
	box, err := secret.LoadOrCreate(filepath.Join(cfg.DataDir, "master.key"))
	if err != nil {
		return nil, err
	}
	jm, err := jobs.New(db, filepath.Join(cfg.DataDir, "jobs"), 3)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, db: db, box: box,
		dk:        docker.New(cfg.DockerHost),
		caddy:     proxy.New(cfg.CaddyAdmin),
		jobs:      jm,
		notify:    notify.New(db, box),
		web:       web,
		cronIDs:    map[string]cron.EntryID{},
		cronMeta:   map[cron.EntryID]scheduleMeta{},
		loginFail:  map[string][]time.Time{},
		gdriveAuth: map[string]*gdriveOAuthState{},
		startedAt:  time.Now(),
	}
	if err := s.initDefaults(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) DB() *sql.DB { return s.db }

func (s *Server) initDefaults() error {
	defaults := map[string]string{
		"timezone":           "Asia/Shanghai",
		"secure_headers":     "1",
		"session_ttl_hours":  "168",
		"image_prune_cron":   "0 4 * * 0",
		"panel_backup_cron":  "0 3 * * *",
		"disk_alert_percent": "85",
		"cert_alert_days":    "14",
		"image_check_cron":   "0 */6 * * *",
	}
	for k, v := range defaults {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO settings(key,value) VALUES(?,?)`, k, v); err != nil {
			return err
		}
	}
	if s.setting("backup_age_identity", "") == "" {
		id, rcp, err := backup.NewIdentity()
		if err != nil {
			return err
		}
		s.setSetting("backup_age_identity", s.box.Seal(id))
		s.setSetting("backup_age_recipient", rcp)
	}
	// Default local storage target.
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM storage_targets`).Scan(&n)
	if n == 0 {
		conf := `{"path":"` + jsonEscape(filepath.ToSlash(s.cfg.BackupsDir)) + `"}`
		_, _ = s.db.Exec(`INSERT INTO storage_targets(name,kind,config,created_at) VALUES('本地磁盘','local',?,?)`, s.box.Seal(conf), time.Now().Unix())
	}
	var users int
	_ = s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&users)
	if users == 0 {
		s.setupToken = auth.RandomToken(12)
		slog.Warn("尚未创建管理员。打开面板完成初始化，初始化令牌：" + s.setupToken)
	}
	return nil
}

func jsonEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

func (s *Server) setting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Server) setSetting(key, value string) {
	_, _ = s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
}

func (s *Server) location() *time.Location {
	loc, err := time.LoadLocation(s.setting("timezone", "Asia/Shanghai"))
	if err != nil {
		return time.Local
	}
	return loc
}

// Handler builds the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	mux.HandleFunc("POST /hooks/p/{token}", s.handleWebhook)
	mux.Handle("/", s.staticHandler())
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) staticHandler() http.Handler {
	if s.cfg.DevProxy != "" {
		u, err := url.Parse(s.cfg.DevProxy)
		if err == nil {
			return httputil.NewSingleHostReverseProxy(u)
		}
	}
	fsrv := http.FileServer(http.FS(s.web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if f, err := s.web.Open(p); err == nil {
				f.Close()
				setStaticHeaders(w.Header(), p)
				fsrv.ServeHTTP(w, r)
				return
			}
			// A hashed asset from an older build must 404, not fall back to index.html: a stale
			// page would otherwise try to run HTML as a module script.
			if strings.HasPrefix(p, "assets/") {
				http.NotFound(w, r)
				return
			}
		}
		data, err := fs.ReadFile(s.web, "index.html")
		if err != nil {
			http.Error(w, "前端尚未构建：请先在 web/ 下运行 npm run build", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	})
}

// setStaticHeaders sets caching and type headers for a file from the embedded frontend.
// Hashed assets are immutable. The service worker, manifest and icons keep stable names, so
// they are revalidated on every request: a new build must reach browsers right away.
func setStaticHeaders(h http.Header, p string) {
	switch {
	case strings.HasPrefix(p, "assets/"):
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	case p == "sw.js":
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(p, ".webmanifest"):
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Type", "application/manifest+json")
	default:
		h.Set("Cache-Control", "no-cache")
	}
}

// Run serves HTTP and the background loops until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	s.startScheduler()
	go s.monitorLoop(ctx)
	go s.metricsLoop(ctx)
	go s.eventsLoop(ctx)
	go func() {
		// Push proxy config once Caddy is reachable.
		for i := 0; i < 30; i++ {
			if ctx.Err() != nil {
				return
			}
			if err := s.syncCaddy(context.Background()); err == nil {
				slog.Info("caddy config loaded")
				return
			} else if i == 0 {
				slog.Warn("caddy not ready", "err", err)
			}
			time.Sleep(5 * time.Second)
		}
	}()
	errc := make(chan error, 1)
	go func() {
		slog.Info("compose-panel listening", "addr", s.cfg.Listen, "version", Version)
		errc <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.cron.Stop()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
