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
			rp := map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": s.Upstream}}}
			if len(s.Headers) > 0 {
				set := map[string][]string{}
				for k, v := range s.Headers {
					set[k] = []string{v}
				}
				rp["headers"] = map[string]any{"request": map[string]any{"set": set}}
			}
			handlers = append(handlers, rp)
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
	return map[string]any{
		// Over a unix socket Caddy only accepts an empty Host header unless
		// origins are listed; Go always sends the URL host ("caddy").
		"admin": map[string]any{"listen": "unix/" + o.AdminSocket, "origins": []string{"caddy"}},
		"apps": map[string]any{
			"http": map[string]any{"servers": map[string]any{
				"main": map[string]any{
					"listen": []string{":443", ":80"},
					"routes": routes,
				},
			}},
			"tls": tlsApp,
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
