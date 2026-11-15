package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"compose-panel/internal/auth"
	"compose-panel/internal/proxy"
)

type siteRow struct {
	ID                int64    `json:"id"`
	Domains           []string `json:"domains"`
	Kind              string   `json:"kind"`
	ProjectID         int64    `json:"projectId"`
	ProjectName       string   `json:"projectName"`
	Service           string   `json:"service"`
	Port              int      `json:"port"`
	Upstream          string   `json:"upstream"`
	HSTS              bool     `json:"hsts"`
	Compress          bool     `json:"compress"`
	BasicUser         string   `json:"basicUser"`
	HasBasic          bool     `json:"hasBasicAuth"`
	IPAllow           string   `json:"ipAllow"`
	MaxBody           string   `json:"maxBody"`
	Headers           string   `json:"headers"`
	Enabled           bool     `json:"enabled"`
	CreatedAt         int64    `json:"createdAt"`
	Maintenance       bool     `json:"maintenance"`
	MaintenanceMsg    string   `json:"maintenanceMsg"`
	MaintenanceDeploy bool     `json:"maintenanceDeploy"`
	AccessLog         bool     `json:"accessLog"`
	LoadBalance       bool     `json:"loadBalance"`
	HealthPath        string   `json:"healthPath"`
	basicHash         string
}

func (s *Server) listSites(where string, args ...any) ([]siteRow, error) {
	rows, err := s.db.Query(`SELECT s.id, s.domains, s.kind, COALESCE(s.project_id,0), COALESCE(p.name,''), s.service, s.port, s.upstream, s.hsts, s.compress, s.basic_user, s.basic_hash, s.ip_allow, s.max_body, s.headers, s.enabled, s.created_at,
		s.maintenance, s.maintenance_msg, s.maintenance_deploy, s.access_log, s.load_balance, s.health_path
		FROM sites s LEFT JOIN projects p ON p.id=s.project_id `+where+` ORDER BY s.domains`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []siteRow{}
	for rows.Next() {
		var x siteRow
		var domains string
		if err := rows.Scan(&x.ID, &domains, &x.Kind, &x.ProjectID, &x.ProjectName, &x.Service, &x.Port, &x.Upstream, &x.HSTS, &x.Compress, &x.BasicUser, &x.basicHash, &x.IPAllow, &x.MaxBody, &x.Headers, &x.Enabled, &x.CreatedAt,
			&x.Maintenance, &x.MaintenanceMsg, &x.MaintenanceDeploy, &x.AccessLog, &x.LoadBalance, &x.HealthPath); err != nil {
			return nil, err
		}
		x.Domains = splitList(domains)
		x.HasBasic = x.basicHash != ""
		out = append(out, x)
	}
	return out, nil
}

// upstreamFor resolves where Caddy should send traffic.
func (s *Server) upstreamFor(x siteRow) string {
	if x.Kind == "redirect" {
		return x.Upstream
	}
	if x.ProjectName != "" && x.Service != "" {
		return fmt.Sprintf("%s-%s-1:%d", x.ProjectName, x.Service, x.Port)
	}
	return x.Upstream
}

func parseSize(s string) int64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0
	}
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"GB", 1 << 30}, {"G", 1 << 30}, {"MB", 1 << 20}, {"M", 1 << 20}, {"KB", 1 << 10}, {"K", 1 << 10}} {
		if strings.HasSuffix(s, u.suf) {
			mult = u.m
			s = strings.TrimSuffix(s, u.suf)
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n * mult
}

func parseHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if ok && strings.TrimSpace(k) != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// buildCaddyConfig renders every enabled site plus the panel's own domain.
func (s *Server) buildCaddyConfig() (map[string]any, error) {
	rows, err := s.listSites(`WHERE s.enabled=1`)
	if err != nil {
		return nil, err
	}
	var sites []proxy.Site
	if d := s.setting("panel_domain", ""); d != "" {
		sites = append(sites, proxy.Site{ID: 0, Domains: []string{d}, Kind: "proxy", Upstream: s.cfg.PanelUpstream, HSTS: true, Compress: true})
	}
	for _, x := range rows {
		up := s.upstreamFor(x)
		if up == "" {
			continue
		}
		site := proxy.Site{
			ID: x.ID, Domains: x.Domains, Kind: x.Kind, Upstream: up, HSTS: x.HSTS, Compress: x.Compress,
			BasicUser: x.BasicUser, BasicHash: x.basicHash, IPAllow: splitList(x.IPAllow), MaxBody: parseSize(x.MaxBody), Headers: parseHeaders(x.Headers),
			HealthPath: x.HealthPath, AccessLog: x.AccessLog, MaintenanceMsg: x.MaintenanceMsg,
			Maintenance: x.Maintenance || (x.MaintenanceDeploy && s.inDeployMaintenance(x.ProjectID)),
		}
		if x.LoadBalance && x.ProjectName != "" && x.Service != "" {
			site.Upstreams = s.replicaUpstreams(x)
		}
		sites = append(sites, site)
	}
	var certs []proxy.LoadedCert
	crow, err := s.db.Query(`SELECT id, cert_pem, key_pem FROM certificates WHERE not_after>?`, time.Now().Unix())
	if err == nil {
		for crow.Next() {
			var id int64
			var c, k string
			if crow.Scan(&id, &c, &k) == nil {
				certs = append(certs, proxy.LoadedCert{CertPEM: c, KeyPEM: s.box.MustOpen(k), Tag: fmt.Sprintf("uploaded-%d", id)})
			}
		}
		crow.Close()
	}
	return proxy.Build(sites, proxy.Options{
		AdminSocket:   s.cfg.CaddyAdmin,
		Email:         s.setting("acme_email", ""),
		SecureHeaders: s.setting("secure_headers", "1") == "1",
		Certs:         certs,
	}), nil
}

func (s *Server) syncCaddy(ctx context.Context) error {
	s.caddyMu.Lock()
	defer s.caddyMu.Unlock()
	cfg, err := s.buildCaddyConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.caddy.Load(ctx, cfg)
}

var domainRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

type siteReq struct {
	Domains           []string `json:"domains"`
	Kind              string   `json:"kind"`
	ProjectID         int64    `json:"projectId"`
	Service           string   `json:"service"`
	Port              int      `json:"port"`
	Upstream          string   `json:"upstream"`
	HSTS              bool     `json:"hsts"`
	Compress          bool     `json:"compress"`
	BasicUser         string   `json:"basicUser"`
	BasicPass         string   `json:"basicPassword"`
	ClearBasic        bool     `json:"clearBasicAuth"`
	IPAllow           string   `json:"ipAllow"`
	MaxBody           string   `json:"maxBody"`
	Headers           string   `json:"headers"`
	Enabled           bool     `json:"enabled"`
	Maintenance       bool     `json:"maintenance"`
	MaintenanceMsg    string   `json:"maintenanceMsg"`
	MaintenanceDeploy bool     `json:"maintenanceDeploy"`
	AccessLog         bool     `json:"accessLog"`
	LoadBalance       bool     `json:"loadBalance"`
	HealthPath        string   `json:"healthPath"`
}

func (s *Server) checkSiteReq(req *siteReq, selfID int64) error {
	var clean []string
	for _, d := range req.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if !domainRe.MatchString(d) {
			return bad("域名格式不正确：%s", d)
		}
		clean = append(clean, d)
	}
	if len(clean) == 0 {
		return bad("至少填写一个域名")
	}
	req.Domains = clean
	existing, _ := s.listSites("")
	for _, x := range existing {
		if x.ID == selfID {
			continue
		}
		for _, d := range x.Domains {
			for _, n := range clean {
				if d == n {
					return bad("域名 %s 已被其他站点使用", n)
				}
			}
		}
	}
	if pd := s.setting("panel_domain", ""); pd != "" {
		for _, n := range clean {
			if n == pd {
				return bad("域名 %s 是面板自身的域名", n)
			}
		}
	}
	switch req.Kind {
	case "", "proxy":
		req.Kind = "proxy"
		if req.ProjectID > 0 {
			if req.Service == "" || req.Port <= 0 || req.Port > 65535 {
				return bad("请选择服务并填写容器端口")
			}
		} else if _, _, err := net.SplitHostPort(req.Upstream); err != nil {
			return bad("上游地址应为 host:port")
		}
	case "redirect":
		if !strings.HasPrefix(req.Upstream, "http://") && !strings.HasPrefix(req.Upstream, "https://") {
			return bad("跳转目标应以 http:// 或 https:// 开头")
		}
	default:
		return bad("站点类型无效")
	}
	for _, c := range splitList(req.IPAllow) {
		if _, _, err := net.ParseCIDR(c); err != nil && net.ParseIP(c) == nil {
			return bad("IP 白名单格式不正确：%s", c)
		}
	}
	if req.MaxBody != "" && parseSize(req.MaxBody) == 0 {
		return bad("请求体上限格式不正确（如 50MB）")
	}
	req.HealthPath = strings.TrimSpace(req.HealthPath)
	if req.HealthPath != "" && !strings.HasPrefix(req.HealthPath, "/") {
		return bad("健康检查路径应以 / 开头")
	}
	if req.LoadBalance && req.ProjectID == 0 {
		return bad("负载均衡仅适用于指向项目服务的站点")
	}
	if len(req.MaintenanceMsg) > 500 {
		return bad("维护提示最多 500 字")
	}
	return nil
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request, u *User) error {
	list, err := s.listSites("")
	if err != nil {
		return err
	}
	certs := s.allCerts()
	out := []map[string]any{}
	for _, x := range list {
		if x.ProjectID > 0 && !u.canProject(x.ProjectID) {
			continue
		}
		out = append(out, map[string]any{"site": x, "target": s.upstreamFor(x), "cert": certFor(certs, x.Domains[0])})
	}
	return ok(w, map[string]any{"sites": out, "panelDomain": s.setting("panel_domain", "")})
}

func (s *Server) handleSaveSite(w http.ResponseWriter, r *http.Request, u *User) error {
	var req siteReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	id, _ := pathID(r, "id")
	if err := s.checkSiteReq(&req, id); err != nil {
		return err
	}
	if req.ProjectID > 0 && !u.canProject(req.ProjectID) {
		return errForbidden
	}
	var projectID any
	if req.ProjectID > 0 {
		projectID = req.ProjectID
	}
	hash := ""
	if id > 0 {
		_ = s.db.QueryRow(`SELECT basic_hash FROM sites WHERE id=?`, id).Scan(&hash)
	}
	if req.ClearBasic || req.BasicUser == "" {
		hash, req.BasicUser = "", ""
	} else if req.BasicPass != "" {
		h, err := auth.HashPassword(req.BasicPass)
		if err != nil {
			return err
		}
		hash = h
	} else if hash == "" {
		return bad("请填写 Basic Auth 密码")
	}
	domains := strings.Join(req.Domains, ",")
	var err error
	if id > 0 {
		_, err = s.db.Exec(`UPDATE sites SET domains=?, kind=?, project_id=?, service=?, port=?, upstream=?, hsts=?, compress=?, basic_user=?, basic_hash=?, ip_allow=?, max_body=?, headers=?, enabled=?,
			maintenance=?, maintenance_msg=?, maintenance_deploy=?, access_log=?, load_balance=?, health_path=? WHERE id=?`,
			domains, req.Kind, projectID, req.Service, req.Port, req.Upstream, boolInt(req.HSTS), boolInt(req.Compress), req.BasicUser, hash, req.IPAllow, req.MaxBody, req.Headers, boolInt(req.Enabled),
			boolInt(req.Maintenance), req.MaintenanceMsg, boolInt(req.MaintenanceDeploy), boolInt(req.AccessLog), boolInt(req.LoadBalance), req.HealthPath, id)
	} else {
		var res interface{ LastInsertId() (int64, error) }
		res, err = s.db.Exec(`INSERT INTO sites(domains,kind,project_id,service,port,upstream,hsts,compress,basic_user,basic_hash,ip_allow,max_body,headers,enabled,created_at,
			maintenance,maintenance_msg,maintenance_deploy,access_log,load_balance,health_path) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			domains, req.Kind, projectID, req.Service, req.Port, req.Upstream, boolInt(req.HSTS), boolInt(req.Compress), req.BasicUser, hash, req.IPAllow, req.MaxBody, req.Headers, boolInt(req.Enabled), time.Now().Unix(),
			boolInt(req.Maintenance), req.MaintenanceMsg, boolInt(req.MaintenanceDeploy), boolInt(req.AccessLog), boolInt(req.LoadBalance), req.HealthPath)
		if err == nil {
			id, _ = res.LastInsertId()
		}
	}
	s.audit(r, u, "site.save", domains, req.Kind, err)
	if err != nil {
		return err
	}
	out := map[string]any{"id": id}
	if err := s.syncCaddy(r.Context()); err != nil {
		out["warning"] = "站点已保存，但推送到 Caddy 失败：" + err.Error()
	}
	return ok(w, out)
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var domains string
	_ = s.db.QueryRow(`SELECT domains FROM sites WHERE id=?`, id).Scan(&domains)
	_, err = s.db.Exec(`DELETE FROM sites WHERE id=?`, id)
	s.audit(r, u, "site.delete", domains, "", err)
	if err != nil {
		return err
	}
	out := map[string]any{"ok": true}
	if err := s.syncCaddy(r.Context()); err != nil {
		out["warning"] = "推送到 Caddy 失败：" + err.Error()
	}
	return ok(w, out)
}

// handleSitePrecheck resolves a domain so the user can confirm DNS before saving.
func (s *Server) handleSitePrecheck(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Domain string `json:"domain"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	d := strings.ToLower(strings.TrimSpace(req.Domain))
	if !domainRe.MatchString(d) {
		return bad("域名格式不正确")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	lookup := strings.TrimPrefix(d, "*.")
	ips, err := net.DefaultResolver.LookupHost(ctx, lookup)
	res := map[string]any{"domain": d, "ips": nonNil(ips), "wildcard": strings.HasPrefix(d, "*.")}
	if err != nil {
		res["error"] = "DNS 解析失败：" + err.Error()
	}
	if pub := s.setting("public_ip", ""); pub != "" {
		match := false
		for _, ip := range ips {
			if ip == pub {
				match = true
			}
		}
		res["publicIp"], res["matches"] = pub, match
	}
	if strings.HasPrefix(d, "*.") {
		res["note"] = "通配符证书需要 DNS 验证，默认的 HTTP 验证无法签发，请上传证书"
	}
	return ok(w, res)
}

func (s *Server) handleProxyConfig(w http.ResponseWriter, r *http.Request, u *User) error {
	want, err := s.buildCaddyConfig()
	if err != nil {
		return err
	}
	// Never expose private keys of uploaded certificates.
	if apps, ok := want["apps"].(map[string]any); ok {
		if t, ok := apps["tls"].(map[string]any); ok {
			if c, ok := t["certificates"].(map[string]any); ok {
				if pems, ok := c["load_pem"].([]any); ok {
					for _, p := range pems {
						if m, ok := p.(map[string]any); ok {
							m["key"] = "(已隐藏)"
							m["certificate"] = "(已省略)"
						}
					}
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	status := "ok"
	if err := s.caddy.Ping(ctx); err != nil {
		status = err.Error()
	}
	return ok(w, map[string]any{"config": want, "caddy": status})
}

func (s *Server) handleProxyReload(w http.ResponseWriter, r *http.Request, u *User) error {
	err := s.syncCaddy(r.Context())
	s.audit(r, u, "proxy.reload", "", "", err)
	return done(w, err)
}

// ---------- certificates ----------

type certView struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Domains   []string  `json:"domains"`
	Issuer    string    `json:"issuer"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
	DaysLeft  int       `json:"daysLeft"`
	Source    string    `json:"source"`
	KeyType   string    `json:"keyType"`
	Serial    string    `json:"serial"`
	Sites     []string  `json:"sites"`
}

func (s *Server) allCerts() []certView {
	out := []certView{}
	acme, _ := proxy.ScanStorage(s.cfg.CaddyDataDir)
	for _, c := range acme {
		out = append(out, certView{Domains: c.Domains, Issuer: c.Issuer, NotBefore: c.NotBefore, NotAfter: c.NotAfter, Source: c.Source, KeyType: c.KeyType, Serial: c.Serial})
	}
	rows, err := s.db.Query(`SELECT id, name, cert_pem FROM certificates ORDER BY id`)
	if err == nil {
		for rows.Next() {
			var id int64
			var name, pemData string
			if rows.Scan(&id, &name, &pemData) == nil {
				if ci, err := proxy.ParsePEM([]byte(pemData)); err == nil {
					out = append(out, certView{ID: id, Name: name, Domains: ci.Domains, Issuer: ci.Issuer, NotBefore: ci.NotBefore, NotAfter: ci.NotAfter, Source: "uploaded", KeyType: ci.KeyType, Serial: ci.Serial})
				}
			}
		}
		rows.Close()
	}
	for i := range out {
		out[i].DaysLeft = int(time.Until(out[i].NotAfter).Hours() / 24)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NotAfter.Before(out[j].NotAfter) })
	return out
}

func domainMatches(pattern, d string) bool {
	if pattern == d {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return strings.HasSuffix(d, suffix) && !strings.Contains(strings.TrimSuffix(d, suffix), ".")
	}
	return false
}

func certFor(certs []certView, domain string) *certView {
	var best *certView
	for i := range certs {
		for _, d := range certs[i].Domains {
			if domainMatches(d, domain) && (best == nil || certs[i].NotAfter.After(best.NotAfter)) {
				best = &certs[i]
			}
		}
	}
	return best
}

func (s *Server) handleListCerts(w http.ResponseWriter, r *http.Request, u *User) error {
	certs := s.allCerts()
	sites, _ := s.listSites("")
	for i := range certs {
		certs[i].Sites = []string{}
		for _, x := range sites {
			for _, d := range x.Domains {
				for _, cd := range certs[i].Domains {
					if domainMatches(cd, d) {
						certs[i].Sites = append(certs[i].Sites, d)
					}
				}
			}
		}
	}
	return ok(w, certs)
}

func (s *Server) handleUploadCert(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Name string `json:"name"`
		Cert string `json:"cert"`
		Key  string `json:"key"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if _, err := tls.X509KeyPair([]byte(req.Cert), []byte(req.Key)); err != nil {
		return bad("证书与私钥不匹配或格式错误：%v", err)
	}
	ci, err := proxy.ParsePEM([]byte(req.Cert))
	if err != nil {
		return bad("%v", err)
	}
	if time.Now().After(ci.NotAfter) {
		return bad("证书已过期")
	}
	if req.Name == "" {
		req.Name = strings.Join(ci.Domains, ", ")
	}
	_, err = s.db.Exec(`INSERT INTO certificates(name,cert_pem,key_pem,domains,not_before,not_after,created_at) VALUES(?,?,?,?,?,?,?)`,
		req.Name, req.Cert, s.box.Seal(req.Key), strings.Join(ci.Domains, ","), ci.NotBefore.Unix(), ci.NotAfter.Unix(), time.Now().Unix())
	s.audit(r, u, "cert.upload", req.Name, strings.Join(ci.Domains, ","), err)
	if err != nil {
		return err
	}
	out := map[string]any{"ok": true, "domains": ci.Domains}
	if err := s.syncCaddy(r.Context()); err != nil {
		out["warning"] = "推送到 Caddy 失败：" + err.Error()
	}
	return ok(w, out)
}

func (s *Server) handleDeleteCert(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM certificates WHERE id=?`, id)
	s.audit(r, u, "cert.delete", fmt.Sprint(id), "", err)
	if err != nil {
		return err
	}
	_ = s.syncCaddy(r.Context())
	return ok(w, nil)
}
