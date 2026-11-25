package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"compose-panel/internal/auth"
)

type template struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Compose     string        `json:"compose"`
	Env         []EnvVar      `json:"env"`
	Files       []projectFile `json:"files"`
	Port        int           `json:"port"`
	Service     string        `json:"service"`
	Vars        []string      `json:"vars"` // ${VAR} placeholders used in compose
	Custom      bool          `json:"custom"`
	Category    string        `json:"category"`
	CreatedBy   string        `json:"createdBy,omitempty"`
	CreatedAt   int64         `json:"createdAt,omitempty"`
}

var placeholderRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`)

// templateVars lists the distinct ${VAR} placeholders in compose order.
func templateVars(compose string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range placeholderRe.FindAllStringSubmatch(strings.ReplaceAll(compose, "$${", ""), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// fillTemplate makes sure every placeholder has an env entry and generates
// values for empty secrets so a template is deployable as-is.
func fillTemplate(t *template) {
	t.Vars = templateVars(t.Compose)
	have := map[string]int{}
	for i, e := range t.Env {
		have[e.Key] = i
	}
	for _, v := range t.Vars {
		if _, ok := have[v]; !ok {
			t.Env = append(t.Env, EnvVar{Key: v, Secret: looksSecret(v)})
			have[v] = len(t.Env) - 1
		}
	}
	for i := range t.Env {
		if t.Env[i].Secret && t.Env[i].Value == "" {
			t.Env[i].Value = auth.RandomPassword(24)
		}
	}
	if t.Env == nil {
		t.Env = []EnvVar{}
	}
	if t.Files == nil {
		t.Files = []projectFile{}
	}
}

func builtinTemplates() []template {
	return []template{
		{ID: "whoami", Category: "示例", Name: "whoami", Description: "最小示例：返回请求信息的 HTTP 服务", Service: "web", Port: 80,
			Compose: "services:\n  web:\n    image: traefik/whoami:v1.10\n    restart: unless-stopped\n    mem_limit: 64m\n"},
		{ID: "nginx", Category: "Web", Name: "Nginx 静态站点", Description: "挂载 ./html 目录的 Nginx", Service: "web", Port: 80,
			Compose: "services:\n  web:\n    image: nginx:1.27-alpine\n    restart: unless-stopped\n    mem_limit: 128m\n    volumes:\n      - ./html:/usr/share/nginx/html:ro\n",
			Files:   []projectFile{{Path: "html/index.html", Content: "<!doctype html>\n<h1>It works!</h1>\n"}}},
		{ID: "postgres", Category: "数据库", Name: "PostgreSQL 16", Description: "独立的 PostgreSQL 实例，数据保存在命名卷中", Service: "", Port: 0,
			Compose: "services:\n  db:\n    image: postgres:16-alpine\n    restart: unless-stopped\n    mem_limit: 512m\n    environment:\n      POSTGRES_USER: ${POSTGRES_USER}\n      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      POSTGRES_DB: ${POSTGRES_DB}\n    volumes:\n      - data:/var/lib/postgresql/data\n    healthcheck:\n      test: [\"CMD-SHELL\", \"pg_isready -U $${POSTGRES_USER}\"]\n      interval: 10s\n      retries: 10\nvolumes:\n  data: {}\n",
			Env:     []EnvVar{{Key: "POSTGRES_USER", Value: "app"}, {Key: "POSTGRES_PASSWORD", Secret: true}, {Key: "POSTGRES_DB", Value: "app"}}},
		{ID: "mysql", Category: "数据库", Name: "MySQL 8.4", Description: "独立的 MySQL 实例，带应用账号", Service: "", Port: 0,
			Compose: "services:\n  db:\n    image: mysql:8.4\n    restart: unless-stopped\n    mem_limit: 768m\n    environment:\n      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD}\n      MYSQL_DATABASE: ${MYSQL_DATABASE}\n      MYSQL_USER: ${MYSQL_USER}\n      MYSQL_PASSWORD: ${MYSQL_PASSWORD}\n    volumes:\n      - data:/var/lib/mysql\n    healthcheck:\n      test: [\"CMD\", \"mysqladmin\", \"ping\", \"-h\", \"127.0.0.1\"]\n      interval: 10s\n      retries: 10\nvolumes:\n  data: {}\n",
			Env:     []EnvVar{{Key: "MYSQL_ROOT_PASSWORD", Secret: true}, {Key: "MYSQL_DATABASE", Value: "app"}, {Key: "MYSQL_USER", Value: "app"}, {Key: "MYSQL_PASSWORD", Secret: true}}},
		{ID: "redis", Category: "数据库", Name: "Redis 7", Description: "开启密码与 AOF 持久化的 Redis", Service: "", Port: 0,
			Compose: "services:\n  redis:\n    image: redis:7-alpine\n    restart: unless-stopped\n    mem_limit: 256m\n    command: [\"redis-server\", \"--appendonly\", \"yes\", \"--requirepass\", \"${REDIS_PASSWORD}\"]\n    volumes:\n      - data:/data\n    healthcheck:\n      test: [\"CMD-SHELL\", \"redis-cli -a \\\"$${REDIS_PASSWORD}\\\" --no-auth-warning ping | grep PONG\"]\n      interval: 10s\n      retries: 10\n    environment:\n      REDIS_PASSWORD: ${REDIS_PASSWORD}\nvolumes:\n  data: {}\n",
			Env:     []EnvVar{{Key: "REDIS_PASSWORD", Secret: true}}},
		{ID: "minio", Category: "存储", Name: "MinIO 对象存储", Description: "S3 兼容对象存储，控制台端口 9001", Service: "minio", Port: 9001,
			Compose: "services:\n  minio:\n    image: minio/minio:RELEASE.2024-12-18T13-15-44Z\n    restart: unless-stopped\n    mem_limit: 512m\n    command: [\"server\", \"/data\", \"--console-address\", \":9001\"]\n    environment:\n      MINIO_ROOT_USER: ${MINIO_ROOT_USER}\n      MINIO_ROOT_PASSWORD: ${MINIO_ROOT_PASSWORD}\n    volumes:\n      - data:/data\nvolumes:\n  data: {}\n",
			Env:     []EnvVar{{Key: "MINIO_ROOT_USER", Value: "admin"}, {Key: "MINIO_ROOT_PASSWORD", Secret: true}}},
		{ID: "uptime-kuma", Category: "应用", Name: "Uptime Kuma", Description: "自托管的可用性监控", Service: "app", Port: 3001,
			Compose: "services:\n  app:\n    image: louislam/uptime-kuma:1.23.16\n    restart: unless-stopped\n    mem_limit: 512m\n    volumes:\n      - data:/app/data\nvolumes:\n  data: {}\n"},
		{ID: "gitea", Category: "应用", Name: "Gitea", Description: "轻量 Git 服务（SQLite）", Service: "server", Port: 3000,
			Compose: "services:\n  server:\n    image: gitea/gitea:1.22\n    restart: unless-stopped\n    mem_limit: 512m\n    environment:\n      USER_UID: \"1000\"\n      USER_GID: \"1000\"\n    volumes:\n      - data:/data\nvolumes:\n  data: {}\n"},
		{ID: "wordpress", Category: "应用", Name: "WordPress + MariaDB", Description: "博客，带独立数据库", Service: "wordpress", Port: 80,
			Compose: "services:\n  wordpress:\n    image: wordpress:6.6-apache\n    restart: unless-stopped\n    mem_limit: 512m\n    environment:\n      WORDPRESS_DB_HOST: db\n      WORDPRESS_DB_USER: wordpress\n      WORDPRESS_DB_PASSWORD: ${DB_PASSWORD}\n      WORDPRESS_DB_NAME: wordpress\n    volumes:\n      - wp:/var/www/html\n    depends_on:\n      db:\n        condition: service_healthy\n  db:\n    image: mariadb:11.4\n    restart: unless-stopped\n    mem_limit: 512m\n    environment:\n      MARIADB_DATABASE: wordpress\n      MARIADB_USER: wordpress\n      MARIADB_PASSWORD: ${DB_PASSWORD}\n      MARIADB_RANDOM_ROOT_PASSWORD: \"1\"\n    volumes:\n      - db:/var/lib/mysql\n    healthcheck:\n      test: [\"CMD\", \"healthcheck.sh\", \"--connect\", \"--innodb_initialized\"]\n      interval: 10s\n      retries: 10\nvolumes:\n  wp: {}\n  db: {}\n",
			Env:     []EnvVar{{Key: "DB_PASSWORD", Secret: true}}},
	}
}

func (s *Server) customTemplates() []template {
	rows, err := s.db.Query(`SELECT id, name, description, compose, env, files, service, port, created_by, created_at FROM templates ORDER BY name`)
	out := []template{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t template
		var id int64
		var env, files string
		if rows.Scan(&id, &t.Name, &t.Description, &t.Compose, &env, &files, &t.Service, &t.Port, &t.CreatedBy, &t.CreatedAt) != nil {
			continue
		}
		t.ID, t.Custom, t.Category = fmt.Sprintf("custom-%d", id), true, "我的模板"
		if env != "" {
			_ = json.Unmarshal([]byte(s.box.MustOpen(env)), &t.Env)
		}
		if files != "" {
			_ = json.Unmarshal([]byte(s.box.MustOpen(files)), &t.Files)
		}
		out = append(out, t)
	}
	return out
}

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request, u *User) error {
	all := append(s.customTemplates(), builtinTemplates()...)
	for i := range all {
		fillTemplate(&all[i])
		if !u.can(roleOperator) {
			for j := range all[i].Env {
				if all[i].Env[j].Secret {
					all[i].Env[j].Value = ""
				}
			}
		}
	}
	return ok(w, all)
}

// handleSaveAsTemplate stores a project's compose, env and files as a reusable template.
// Secret values are dropped unless keepSecrets is set; they are regenerated on use.
func (s *Server) handleSaveAsTemplate(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		KeepSecrets bool   `json:"keepSecrets"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return bad("模板名称不能为空")
	}
	if p.Source != "inline" || strings.TrimSpace(p.Compose) == "" {
		return bad("只有在面板内编辑的项目可以保存为模板")
	}
	env := make([]EnvVar, len(p.Env))
	copy(env, p.Env)
	if !req.KeepSecrets {
		for i := range env {
			if env[i].Secret {
				env[i].Value = ""
			}
		}
	}
	files := s.projectFiles(p.ID)
	var service string
	var port int
	_ = s.db.QueryRow(`SELECT service, port FROM sites WHERE project_id=? AND kind='proxy' ORDER BY id LIMIT 1`, p.ID).Scan(&service, &port)
	envJSON, _ := json.Marshal(env)
	filesJSON, _ := json.Marshal(files)
	res, err := s.db.Exec(`INSERT INTO templates(name,description,compose,env,files,service,port,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		req.Name, req.Description, p.Compose, s.box.Seal(string(envJSON)), s.box.Seal(string(filesJSON)), service, port, u.Username, time.Now().Unix())
	s.audit(r, u, "template.create", req.Name, "来自项目 "+p.Name, err)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	return ok(w, map[string]any{"id": fmt.Sprintf("custom-%d", id)})
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request, u *User) error {
	if !u.can(roleAdmin) {
		return errForbidden
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var name string
	if err := s.db.QueryRow(`SELECT name FROM templates WHERE id=?`, id).Scan(&name); err != nil {
		return errNotFound
	}
	_, err = s.db.Exec(`DELETE FROM templates WHERE id=?`, id)
	s.audit(r, u, "template.delete", name, "", err)
	return done(w, err)
}
