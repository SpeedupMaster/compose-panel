// Package proxy renders site definitions to Caddy JSON and pushes them via the admin API.
package proxy

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Caddy struct {
	socket string
	http   *http.Client
}

func New(socket string) *Caddy {
	return &Caddy{socket: socket, http: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}}
}

func (c *Caddy) call(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://caddy"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("caddy %s %s: %s", method, path, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (c *Caddy) Ping(ctx context.Context) error {
	_, err := c.call(ctx, "GET", "/config/", nil)
	return err
}

// Load replaces Caddy's whole config.
func (c *Caddy) Load(ctx context.Context, cfg any) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, "POST", "/load", b)
	return err
}

func (c *Caddy) Current(ctx context.Context) (json.RawMessage, error) {
	return c.call(ctx, "GET", "/config/", nil)
}

// IsLoaded reports whether Caddy currently has an active HTTP server config
// (as opposed to the empty bootstrap Caddyfile with persist_config off).
func (c *Caddy) IsLoaded(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://caddy/config/apps/http", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("caddy GET /config/apps/http: %s", strings.TrimSpace(string(data)))
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false, nil
	}
	var app struct {
		Servers map[string]json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(trimmed, &app); err != nil {
		return false, err
	}
	return len(app.Servers) > 0, nil
}

// Site is one host block.
type Site struct {
	ID        int64
	Domains   []string
	Kind      string // proxy | redirect
	Upstream  string // host:port for proxy, URL for redirect
	HSTS      bool
	Compress  bool
	BasicUser string
	BasicHash string // bcrypt
	IPAllow   []string
	MaxBody   int64
	Headers   map[string]string
	Websocket bool
	// Phase 2
	Upstreams      []string // several replicas → load balancing (overrides Upstream)
	HealthPath     string   // active health check URI
	Maintenance    bool
	MaintenanceMsg string
	AccessLog      bool
	GRPC           bool // send gRPC requests to the upstream over h2c
}

// AccessLogDir is where Caddy writes per-site JSON access logs (inside the Caddy container).
const AccessLogDir = "/data/access-logs"

// LoggerName is the access logger name of a site.
func LoggerName(id int64) string { return fmt.Sprintf("site%d", id) }

func maintenancePage(msg string) string {
	if msg == "" {
		msg = "系统维护中，请稍后再试。"
	}
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return `<!doctype html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>维护中</title><style>body{font-family:system-ui,-apple-system,sans-serif;background:#f6f7f9;color:#222;display:flex;min-height:100vh;margin:0;align-items:center;justify-content:center}` +
		`.b{background:#fff;border-radius:12px;padding:40px 48px;box-shadow:0 2px 12px rgba(0,0,0,.06);max-width:520px;text-align:center}h1{font-size:22px;margin:0 0 12px}p{color:#555;line-height:1.6;margin:0}</style></head>` +
		`<body><div class="b"><h1>🛠 维护中 · Under maintenance</h1><p>` + r.Replace(msg) + `</p></div></body></html>`
}

type LoadedCert struct {
	CertPEM string
	KeyPEM  string
	Tag     string
}

type Options struct {
	AdminSocket   string
	Email         string
	SecureHeaders bool
	Certs         []LoadedCert
}

// Build renders the full Caddy JSON config.
func Build(sites []Site, o Options) map[string]any {
	routes := []any{}
	var manual []string
	for _, s := range sites {
		if len(s.Domains) == 0 {
			continue
		}
		var sub []any
		if len(s.IPAllow) > 0 {
			sub = append(sub, map[string]any{
				"match":  []any{map[string]any{"not": []any{map[string]any{"client_ip": map[string]any{"ranges": s.IPAllow}}}}},
				"handle": []any{map[string]any{"handler": "static_response", "status_code": 403, "body": "Forbidden"}},
			})
		}
		var handlers []any
		respHeaders := map[string][]string{}
		if s.HSTS {
			respHeaders["Strict-Transport-Security"] = []string{"max-age=31536000"}
		}
		if o.SecureHeaders {
			respHeaders["X-Content-Type-Options"] = []string{"nosniff"}
			respHeaders["Referrer-Policy"] = []string{"strict-origin-when-cross-origin"}
		}
		if len(respHeaders) > 0 {
			handlers = append(handlers, map[string]any{"handler": "headers", "response": map[string]any{"set": respHeaders}})
		}
		if s.BasicUser != "" && s.BasicHash != "" {
			handlers = append(handlers, map[string]any{
				"handler": "authentication",
				"providers": map[string]any{"http_basic": map[string]any{
					"hash":     map[string]any{"algorithm": "bcrypt"},
					"accounts": []any{map[string]any{"username": s.BasicUser, "password": s.BasicHash}},
				}},
			})
		}
		if s.MaxBody > 0 {
			handlers = append(handlers, map[string]any{"handler": "request_body", "max_size": s.MaxBody})
		}
		switch s.Kind {
		case "redirect":
			handlers = append(handlers, map[string]any{
				"handler": "static_response", "status_code": 301,
				"headers": map[string][]string{"Location": {strings.TrimRight(s.Upstream, "/") + "{http.request.uri}"}},
			})
		default:
			if s.Compress {
				handlers = append(handlers, map[string]any{"handler": "encode", "encodings": map[string]any{"gzip": map[string]any{}, "zstd": map[string]any{}}, "prefer": []string{"zstd", "gzip"}})
			}
			if s.Maintenance {
				handlers = append(handlers, map[string]any{
					"handler": "static_response", "status_code": 503, "body": maintenancePage(s.MaintenanceMsg),
					"headers": map[string][]string{"Content-Type": {"text/html; charset=utf-8"}, "Retry-After": {"120"}, "Cache-Control": {"no-store"}},
				})
				break
			}
			ups := []any{}
			dials := s.Upstreams
			if len(dials) == 0 {
				dials = []string{s.Upstream}
			}
			for _, d := range dials {
				ups = append(ups, map[string]any{"dial": d})
			}
			rp := map[string]any{"handler": "reverse_proxy", "upstreams": ups}
			if len(dials) > 1 {
				rp["load_balancing"] = map[string]any{"selection_policy": map[string]any{"policy": "round_robin"}, "retries": 2, "try_duration": "5s"}
			}
			if s.HealthPath != "" {
				rp["health_checks"] = map[string]any{
					"active":  map[string]any{"uri": s.HealthPath, "interval": "10s", "timeout": "5s"},
					"passive": map[string]any{"fail_duration": "30s", "max_fails": 2},
				}
			}
			if len(s.Headers) > 0 {
				set := map[string][]string{}
				for k, v := range s.Headers {
					set[k] = []string{v}
				}
				rp["headers"] = map[string]any{"request": map[string]any{"set": set}}
			}
			if s.GRPC {
				// gRPC needs HTTP/2 end to end; the backend speaks it in
				// cleartext (h2c). Only gRPC calls take that path: REST, SSE and
				// WebSocket upgrades keep the plain HTTP/1.1 transport.
				g := map[string]any{"transport": map[string]any{"protocol": "http", "versions": []string{"h2c", "2"}}, "flush_interval": -1}
				for k, v := range rp {
					if k != "health_checks" {
						g[k] = v
					}
				}
				handlers = append(handlers, map[string]any{"handler": "subroute", "routes": []any{
					map[string]any{
						"match":  []any{map[string]any{"header": map[string][]string{"Content-Type": {"application/grpc*"}}}},
						"handle": []any{g},
					},
					map[string]any{"handle": []any{rp}},
				}})
			} else {
				handlers = append(handlers, rp)
			}
		}
		sub = append(sub, map[string]any{"handle": handlers})
		routes = append(routes, map[string]any{
			"match":    []any{map[string]any{"host": s.Domains}},
			"handle":   []any{map[string]any{"handler": "subroute", "routes": sub}},
			"terminal": true,
		})
	}

	issuers := []any{
		map[string]any{"module": "acme", "email": o.Email},
		map[string]any{"module": "acme", "ca": "https://acme.zerossl.com/v2/DV90", "email": o.Email},
	}
	tlsApp := map[string]any{
		"automation": map[string]any{"policies": []any{map[string]any{"issuers": issuers}}},
	}
	if len(o.Certs) > 0 {
		var pems []any
		for _, c := range o.Certs {
			pems = append(pems, map[string]any{"certificate": c.CertPEM, "key": c.KeyPEM, "tags": []string{c.Tag}})
			manual = append(manual, c.Tag)
		}
		tlsApp["certificates"] = map[string]any{"load_pem": pems}
	}
	_ = manual
	server := map[string]any{
		"listen": []string{":443", ":80"},
		"routes": routes,
	}
	logs := map[string]any{"default": map[string]any{"exclude": []string{"http.log.access"}}}
	loggerNames := map[string]any{}
	for _, s := range sites {
		if !s.AccessLog || s.ID == 0 {
			continue
		}
		name := LoggerName(s.ID)
		for _, d := range s.Domains {
			loggerNames[d] = name
		}
		logs[name] = map[string]any{
			"writer": map[string]any{"output": "file", "filename": fmt.Sprintf("%s/site-%d.log", AccessLogDir, s.ID),
				"roll_size_mb": 20, "roll_keep": 10, "roll_keep_days": 7},
			"encoder": map[string]any{"format": "json"},
			"include": []string{"http.log.access." + name},
		}
	}
	if len(loggerNames) > 0 {
		server["logs"] = map[string]any{"logger_names": loggerNames, "skip_unmapped_hosts": true}
	}
	return map[string]any{
		"logging": map[string]any{"logs": logs},
		// Over a unix socket Caddy only accepts an empty Host header unless
		// origins are listed; Go always sends the URL host ("caddy").
		"admin": map[string]any{"listen": "unix/" + o.AdminSocket, "origins": []string{"caddy"}},
		"apps": map[string]any{
			"http": map[string]any{"servers": map[string]any{"main": server}},
			"tls":  tlsApp,
		},
	}
}

// CertInfo is a certificate found in Caddy's storage or uploaded.
type CertInfo struct {
	Domains   []string  `json:"domains"`
	Issuer    string    `json:"issuer"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
	Source    string    `json:"source"` // acme | internal | uploaded
	Path      string    `json:"-"`
	Serial    string    `json:"serial"`
	KeyType   string    `json:"keyType"`
}

// ScanStorage reads managed certificates from Caddy's data directory.
func ScanStorage(dir string) ([]CertInfo, error) {
	var out []CertInfo
	root := filepath.Join(dir, "certificates")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(p, ".crt") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if ci, err := ParsePEM(data); err == nil {
			ci.Source = "acme"
			// Caddy keeps its local-CA certificates (*.localhost, IPs) under
			// certificates/local/; they live 12h and renew themselves.
			if rel, _ := filepath.Rel(root, p); strings.HasPrefix(filepath.ToSlash(rel), "local/") {
				ci.Source = "internal"
			}
			ci.Path = p
			out = append(out, *ci)
		}
		return nil
	})
	return out, err
}

func ParsePEM(data []byte) (*CertInfo, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("不是有效的 PEM 证书")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	domains := cert.DNSNames
	if len(domains) == 0 && cert.Subject.CommonName != "" {
		domains = []string{cert.Subject.CommonName}
	}
	issuer := cert.Issuer.CommonName
	if issuer == "" && len(cert.Issuer.Organization) > 0 {
		issuer = cert.Issuer.Organization[0]
	}
	return &CertInfo{
		Domains: domains, Issuer: issuer, NotBefore: cert.NotBefore, NotAfter: cert.NotAfter,
		Serial: cert.SerialNumber.Text(16), KeyType: cert.PublicKeyAlgorithm.String(),
	}, nil
}
