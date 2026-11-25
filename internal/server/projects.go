package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"compose-panel/internal/auth"
	"compose-panel/internal/compose"
	"compose-panel/internal/docker"
)

var projectNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

type EnvVar struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

type ProjectOptions struct {
	Pull         *bool    `json:"pull,omitempty"`
	Wait         *bool    `json:"wait,omitempty"`
	WaitTimeout  int      `json:"waitTimeout,omitempty"`
	AutoRollback *bool    `json:"autoRollback,omitempty"`
	ConfigFiles  []string `json:"configFiles,omitempty"` // imported projects
	SkipNetworks bool     `json:"skipNetworks,omitempty"`
	AutoUpdate   bool     `json:"autoUpdate,omitempty"` // redeploy when a newer image is found
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

type Project struct {
	ID             int64          `json:"id"`
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	Kind           string         `json:"kind"`
	Source         string         `json:"source"`
	Workdir        string         `json:"workdir"`
	GitURL         string         `json:"gitUrl"`
	GitBranch      string         `json:"gitBranch"`
	GitComposePath string         `json:"gitComposePath"`
	HasGitKey      bool           `json:"hasGitKey"`
	DeployKey      string         `json:"deployKey,omitempty"`
	WebhookToken   string         `json:"-"`
	WebhookURL     string         `json:"webhookUrl,omitempty"`
	AutoDeploy     bool           `json:"autoDeploy"`
	Compose        string         `json:"compose"`
	Env            []EnvVar       `json:"env"`
	Options        ProjectOptions `json:"options"`
	CreatedAt      int64          `json:"createdAt"`
	UpdatedAt      int64          `json:"updatedAt"`

	gitKeyEnc        string
	webhookSecretEnc string
}

const projectCols = `id, name, description, kind, source, workdir, git_url, git_branch, git_compose_path, git_key, webhook_token, webhook_secret, auto_deploy, compose, env, options, created_at, updated_at`

const maskedSecretPlaceholder = "••••••"

var scpGitURLRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:\S+$`)

func isSSHGitURL(u string) bool {
	if strings.HasPrefix(strings.ToLower(u), "ssh://") {
		return true
	}
	return !strings.Contains(u, "://") && scpGitURLRe.MatchString(u)
}

func validateGitURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return bad("Git 地址不能为空")
	}
	if strings.HasPrefix(raw, "-") {
		return bad("Git 地址不能以 - 开头")
	}
	if strings.ContainsAny(raw, " \t\r\n\x00") {
		return bad("Git 地址包含非法空白或控制字符")
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "ssh://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || strings.HasPrefix(u.Hostname(), "-") {
			return bad("Git 地址格式无效")
		}
		return nil
	}
	if !strings.Contains(raw, "://") && scpGitURLRe.MatchString(raw) {
		at := strings.IndexByte(raw, '@')
		colon := strings.IndexByte(raw, ':')
		if at > 0 && colon > at+1 {
			user := raw[:at]
			host := raw[at+1 : colon]
			if !strings.HasPrefix(user, "-") && !strings.HasPrefix(host, "-") {
				return nil
			}
		}
	}
	return bad("Git 地址仅支持 https://、http://、ssh:// 或 user@host:path 格式")
}

func validateGitBranch(branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return bad("Git 分支不能为空")
	}
	if strings.HasPrefix(branch, "-") {
		return bad("Git 分支名不能以 - 开头")
	}
	if strings.ContainsAny(branch, " \t\r\n\x00") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return bad("Git 分支名不合法")
	}
	return nil
}

func (s *Server) openGitURL(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "v1:") {
		if plain, err := s.box.Open(raw); err == nil {
			return plain
		}
		return ""
	}
	return raw
}

func maskGitURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User == nil {
		return raw
	}
	user := u.User.Username()
	_, hasPass := u.User.Password()
	if !hasPass && user == "" {
		return raw
	}
	uCopy := *u
	uCopy.User = nil
	rest := strings.TrimPrefix(uCopy.String(), u.Scheme+"://")
	if hasPass {
		return u.Scheme + "://" + url.User(user).String() + ":" + maskedSecretPlaceholder + "@" + rest
	}
	return u.Scheme + "://" + maskedSecretPlaceholder + "@" + rest
}

func mergeMaskedGitURL(incoming, existing string) (string, error) {
	if !strings.Contains(incoming, maskedSecretPlaceholder) {
		return incoming, nil
	}
	const sentinel = "__PANEL_MASKED_SECRET__"
	incU, err := url.Parse(strings.ReplaceAll(incoming, maskedSecretPlaceholder, sentinel))
	if err != nil || (incU.Scheme != "http" && incU.Scheme != "https") || incU.User == nil {
		return "", bad("Git 地址格式无效，请重新填写完整地址")
	}
	extU, err := url.Parse(existing)
	if err != nil || (extU.Scheme != "http" && extU.Scheme != "https") || extU.User == nil {
		return "", bad("无法保留原仓库密码，请重新填写完整 Git 地址")
	}
	if !strings.EqualFold(incU.Host, extU.Host) {
		return "", bad("修改仓库地址主机时请重新填写密码或令牌")
	}
	incUser := incU.User.Username()
	incPass, incHasPass := incU.User.Password()
	if incHasPass && incPass == sentinel {
		if extPass, ok := extU.User.Password(); ok {
			incU.User = url.UserPassword(incUser, extPass)
			return incU.String(), nil
		}
		if extU.User.Username() != "" {
			incU.User = url.UserPassword(incUser, extU.User.Username())
			return incU.String(), nil
		}
		return "", bad("无法保留原仓库密码，请重新填写完整 Git 地址")
	}
	if !incHasPass && incUser == sentinel {
		incU.User = extU.User
		return incU.String(), nil
	}
	return "", bad("Git 地址包含无效掩码，请重新填写密码或令牌")
}

func (s *Server) scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	p := &Project{}
	var gitURLRaw, envEnc, opts string
	var auto int
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Kind, &p.Source, &p.Workdir, &gitURLRaw, &p.GitBranch, &p.GitComposePath,
		&p.gitKeyEnc, &p.WebhookToken, &p.webhookSecretEnc, &auto, &p.Compose, &envEnc, &opts, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.GitURL = s.openGitURL(gitURLRaw)
	p.AutoDeploy = auto == 1
	p.HasGitKey = p.gitKeyEnc != ""
	p.Env = []EnvVar{}
	if envEnc != "" {
		if raw, err := s.box.Open(envEnc); err == nil {
			_ = json.Unmarshal([]byte(raw), &p.Env)
		}
	}
	_ = json.Unmarshal([]byte(opts), &p.Options)
	return p, nil
}

func (s *Server) getProject(id int64) (*Project, error) {
	p, err := s.scanProject(s.db.QueryRow(`SELECT `+projectCols+` FROM projects WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, errNotFound
	}
	return p, err
}

func (s *Server) projectForUser(r *http.Request, u *User) (*Project, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	if !u.canProject(id) {
		return nil, errNotFound
	}
	return s.getProject(id)
}

// projectDir is where the panel keeps the project's files.
func (s *Server) projectDir(p *Project) string {
	if p.Source == "imported" && p.Workdir != "" {
		return p.Workdir
	}
	return filepath.Join(s.cfg.ProjectsDir, p.Name)
}

// composeProject describes how to call compose for p (after prepareWorkdir).
func (s *Server) composeProject(p *Project, withOverride bool) compose.Project {
	dir := s.projectDir(p)
	cp := compose.Project{Name: p.Name, Dir: dir}
	switch p.Source {
	case "git":
		file := filepath.Join(dir, "src", filepath.FromSlash(p.GitComposePath))
		cp.Dir = filepath.Dir(file)
		cp.Files = []string{file}
	case "imported":
		if len(p.Options.ConfigFiles) > 0 {
			cp.Files = p.Options.ConfigFiles
		} else {
			cp.Files = []string{filepath.Join(dir, "compose.yml")}
		}
	default:
		cp.Files = []string{filepath.Join(dir, "compose.yml")}
	}
	envFile := filepath.Join(dir, ".panel", "panel.env")
	if p.Source != "imported" || len(p.Env) > 0 {
		cp.EnvFile = envFile
	}
	if withOverride {
		cp.Files = append(cp.Files, filepath.Join(dir, ".panel", "override.yml"))
	}
	return cp
}

func renderEnv(env []EnvVar) string {
	var b strings.Builder
	for _, e := range env {
		if e.Key == "" {
			continue
		}
		v := e.Value
		if strings.ContainsAny(v, " #\"'$\n\\") {
			v = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "$", "$$").Replace(v) + `"`
		}
		fmt.Fprintf(&b, "%s=%s\n", e.Key, v)
	}
	return b.String()
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateEnv(env []EnvVar) error {
	seen := map[string]bool{}
	for _, e := range env {
		if e.Key == "" {
			continue
		}
		if !envKeyRe.MatchString(e.Key) {
			return bad("环境变量名不合法：%s", e.Key)
		}
		if seen[e.Key] {
			return bad("环境变量重复：%s", e.Key)
		}
		seen[e.Key] = true
	}
	return nil
}

// ---------- list / get ----------

type projectSummary struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Kind        string         `json:"kind"`
	Source      string         `json:"source"`
	Status      string         `json:"status"` // running | partial | stopped | error | empty
	Running     int            `json:"running"`
	Total       int            `json:"total"`
	Unhealthy   int            `json:"unhealthy"`
	Domains     []string       `json:"domains"`
	LastDeploy  *deploySummary `json:"lastDeploy"`
	UpdatedAt   int64          `json:"updatedAt"`
}

type deploySummary struct {
	ID         int64  `json:"id"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	Trigger    string `json:"trigger"`
	Actor      string `json:"actor"`
	CommitSHA  string `json:"commitSha"`
	CommitMsg  string `json:"commitMsg"`
	JobID      string `json:"jobId"`
	Error      string `json:"error"`
	CreatedAt  int64  `json:"createdAt"`
	FinishedAt int64  `json:"finishedAt"`
}

func containerHealth(c docker.Container) string {
	switch {
	case strings.Contains(c.Status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(c.Status, "(healthy)"):
		return "healthy"
	case strings.Contains(c.Status, "(health: starting)"):
		return "starting"
	}
	return ""
}

func (s *Server) containersByProject(ctx context.Context) map[string][]docker.Container {
	out := map[string][]docker.Container{}
	list, err := s.dk.ListContainers(ctx, true, map[string][]string{"label": {"com.docker.compose.project"}})
	if err != nil {
		return out
	}
	for _, c := range list {
		p := c.Labels["com.docker.compose.project"]
		out[p] = append(out[p], c)
	}
	return out
}

func summarizeStatus(cs []docker.Container) (status string, running, unhealthy int) {
	for _, c := range cs {
		if c.State == "running" {
			running++
		}
		if containerHealth(c) == "unhealthy" || c.State == "restarting" {
			unhealthy++
		}
	}
	switch {
	case len(cs) == 0:
		status = "empty"
	case unhealthy > 0:
		status = "error"
	case running == len(cs):
		status = "running"
	case running == 0:
		status = "stopped"
	default:
		status = "partial"
	}
	return
}

func (s *Server) lastDeploy(projectID int64) *deploySummary {
	rows, err := s.db.Query(`SELECT `+deployCols+` FROM deployments WHERE project_id=? ORDER BY number DESC LIMIT 1`, projectID)
	if err != nil {
		return nil
	}
	list := scanDeploys(rows)
	if len(list) == 0 {
		return nil
	}
	return &list[0]
}

const deployCols = `id, number, status, trigger, actor, commit_sha, commit_msg, job_id, error, created_at, finished_at`

func scanDeploys(rows *sql.Rows) []deploySummary {
	defer rows.Close()
	out := []deploySummary{}
	for rows.Next() {
		var d deploySummary
		if rows.Scan(&d.ID, &d.Number, &d.Status, &d.Trigger, &d.Actor, &d.CommitSHA, &d.CommitMsg, &d.JobID, &d.Error, &d.CreatedAt, &d.FinishedAt) == nil {
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) projectDomains(projectID int64) []string {
	out := []string{}
	rows, err := s.db.Query(`SELECT domains FROM sites WHERE project_id=? AND enabled=1`, projectID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			out = append(out, splitList(d)...)
		}
	}
	return out
}

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request, u *User) error {
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "app"
	}
	rows, err := s.db.Query(`SELECT `+projectCols+` FROM projects WHERE kind=? ORDER BY name`, kind)
	if err != nil {
		return err
	}
	var list []*Project
	for rows.Next() {
		p, err := s.scanProject(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if u.canProject(p.ID) {
			list = append(list, p)
		}
	}
	rows.Close()
	byProject := s.containersByProject(r.Context())
	out := []projectSummary{}
	for _, p := range list {
		cs := byProject[p.Name]
		st, running, unhealthy := summarizeStatus(cs)
		out = append(out, projectSummary{ID: p.ID, Name: p.Name, Description: p.Description, Kind: p.Kind, Source: p.Source,
			Status: st, Running: running, Total: len(cs), Unhealthy: unhealthy, Domains: s.projectDomains(p.ID),
			LastDeploy: s.lastDeploy(p.ID), UpdatedAt: p.UpdatedAt})
	}
	return ok(w, out)
}

type containerDTO struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Service string        `json:"service"`
	Project string        `json:"project"`
	Image   string        `json:"image"`
	State   string        `json:"state"`
	Status  string        `json:"status"`
	Health  string        `json:"health"`
	Ports   []docker.Port `json:"ports"`
	Created int64         `json:"created"`
}

func toContainerDTO(c docker.Container) containerDTO {
	return containerDTO{ID: c.Id, Name: c.Name(), Service: c.Labels["com.docker.compose.service"], Project: c.Labels["com.docker.compose.project"],
		Image: c.Image, State: c.State, Status: c.Status, Health: containerHealth(c), Ports: nonNil(c.Ports), Created: c.Created}
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	if p.Source == "imported" && p.Compose == "" {
		cp := s.composeProject(p, false)
		if len(cp.Files) > 0 {
			if b, err := os.ReadFile(cp.Files[0]); err == nil {
				p.Compose = string(b)
			}
		}
	}
	if !u.can(roleOperator) {
		for i := range p.Env {
			if p.Env[i].Secret {
				p.Env[i].Value = ""
			}
		}
	}
	if p.gitKeyEnc != "" {
		p.DeployKey = publicKeyFromPrivate(s.box.MustOpen(p.gitKeyEnc))
	}
	if p.WebhookToken != "" {
		p.WebhookURL = "/hooks/p/" + p.WebhookToken
	}
	p.GitURL = maskGitURL(p.GitURL)
	cs, _ := s.dk.ProjectContainers(r.Context(), p.Name)
	containers := []containerDTO{}
	for _, c := range cs {
		containers = append(containers, toContainerDTO(c))
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	st, running, unhealthy := summarizeStatus(cs)
	return ok(w, map[string]any{
		"project": p, "containers": containers, "status": st, "running": running, "unhealthy": unhealthy,
		"domains": s.projectDomains(p.ID), "lastDeploy": s.lastDeploy(p.ID), "dir": s.projectDir(p),
		"hasWebhookSecret": p.webhookSecretEnc != "",
	})
}

// ---------- create / update / delete ----------

type projectReq struct {
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	Source         string         `json:"source"`
	GitURL         string         `json:"gitUrl"`
	GitBranch      string         `json:"gitBranch"`
	GitComposePath string         `json:"gitComposePath"`
	AutoDeploy     bool           `json:"autoDeploy"`
	Compose        string         `json:"compose"`
	Env            []EnvVar       `json:"env"`
	Options        ProjectOptions `json:"options"`
	Deploy         bool           `json:"deploy"`
	Files          []projectFile  `json:"files"` // extra files for inline projects (from templates)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request, u *User) error {
	var req projectReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	if !projectNameRe.MatchString(req.Name) {
		return bad("项目名只能包含小写字母、数字、- 和 _，且以字母或数字开头")
	}
	if req.Source == "" {
		req.Source = "inline"
	}
	if req.Source != "inline" && req.Source != "git" {
		return bad("来源无效")
	}
	if req.Source == "inline" && strings.TrimSpace(req.Compose) == "" {
		return bad("Compose 内容不能为空")
	}
	req.GitURL = strings.TrimSpace(req.GitURL)
	req.GitBranch = strings.TrimSpace(req.GitBranch)
	if req.GitBranch == "" {
		req.GitBranch = "main"
	}
	if req.Source == "git" {
		if err := validateGitURL(req.GitURL); err != nil {
			return err
		}
		if err := validateGitBranch(req.GitBranch); err != nil {
			return err
		}
	}
	if err := validateEnv(req.Env); err != nil {
		return err
	}
	if err := cleanProjectFiles(req.Files); err != nil {
		return err
	}
	if req.GitComposePath == "" {
		req.GitComposePath = "compose.yml"
	}
	if strings.Contains(req.GitComposePath, "..") {
		return bad("Compose 路径不合法")
	}
	var exists int
	_ = s.db.QueryRow(`SELECT count(*) FROM projects WHERE name=?`, req.Name).Scan(&exists)
	if exists > 0 {
		return bad("项目名已存在")
	}
	if cs, _ := s.dk.ProjectContainers(r.Context(), req.Name); len(cs) > 0 {
		return bad("Docker 中已有同名 Compose 项目，请使用「导入」")
	}
	envJSON, _ := json.Marshal(req.Env)
	opts, _ := json.Marshal(req.Options)
	gitKey := ""
	if req.Source == "git" && isSSHGitURL(req.GitURL) {
		k, err := newDeployKey()
		if err != nil {
			return err
		}
		gitKey = s.box.Seal(k)
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO projects(name,description,kind,source,git_url,git_branch,git_compose_path,git_key,webhook_token,auto_deploy,compose,env,options,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.Name, req.Description, "app", req.Source, s.box.Seal(req.GitURL), req.GitBranch, req.GitComposePath, gitKey, auth.RandomToken(18),
		boolInt(req.AutoDeploy), req.Compose, s.box.Seal(string(envJSON)), string(opts), now, now)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	if req.Source == "inline" {
		for _, f := range req.Files {
			_, _ = s.db.Exec(`INSERT INTO project_files(project_id,path,content) VALUES(?,?,?)`, id, f.Path, f.Content)
		}
	}
	if u.Projects != nil && u.Level < roleAdmin {
		_, _ = s.db.Exec(`INSERT OR IGNORE INTO user_projects(user_id,project_id) VALUES(?,?)`, u.ID, id)
	}
	s.audit(r, u, "project.create", req.Name, "来源 "+req.Source, nil)
	out := map[string]any{"id": id}
	if req.Deploy {
		p, _ := s.getProject(id)
		jobID, depID := s.startDeploy(p, "create", u.Username, 0)
		out["jobId"], out["deploymentId"] = jobID, depID
	}
	return ok(w, out)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	var req projectReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := validateEnv(req.Env); err != nil {
		return err
	}
	// Secret values left empty keep their stored value.
	old := map[string]string{}
	for _, e := range p.Env {
		old[e.Key] = e.Value
	}
	for i, e := range req.Env {
		if e.Secret && e.Value == "" {
			req.Env[i].Value = old[e.Key]
		}
	}
	req.GitBranch = strings.TrimSpace(req.GitBranch)
	if req.GitBranch == "" {
		req.GitBranch = p.GitBranch
	}
	if req.GitComposePath == "" {
		req.GitComposePath = p.GitComposePath
	}
	if strings.Contains(req.GitComposePath, "..") {
		return bad("Compose 路径不合法")
	}
	gitURL := p.GitURL
	if p.Source == "git" {
		merged, err := mergeMaskedGitURL(strings.TrimSpace(req.GitURL), p.GitURL)
		if err != nil {
			return err
		}
		if err := validateGitURL(merged); err != nil {
			return err
		}
		if err := validateGitBranch(req.GitBranch); err != nil {
			return err
		}
		gitURL = merged
	}
	if p.Source == "imported" && req.Compose != "" {
		cp := s.composeProject(p, false)
		if len(cp.Files) > 0 {
			if err := os.WriteFile(cp.Files[0], []byte(req.Compose), 0o644); err != nil {
				return err
			}
		}
		req.Compose = ""
	}
	if p.Source == "inline" && strings.TrimSpace(req.Compose) == "" {
		return bad("Compose 内容不能为空")
	}
	req.Options.ConfigFiles = p.Options.ConfigFiles
	envJSON, _ := json.Marshal(req.Env)
	opts, _ := json.Marshal(req.Options)
	gitKey := p.gitKeyEnc
	if p.Source == "git" && gitKey == "" && isSSHGitURL(gitURL) {
		k, err := newDeployKey()
		if err != nil {
			return err
		}
		gitKey = s.box.Seal(k)
	}
	_, err = s.db.Exec(`UPDATE projects SET description=?, git_url=?, git_branch=?, git_compose_path=?, git_key=?, auto_deploy=?, compose=?, env=?, options=?, updated_at=? WHERE id=?`,
		req.Description, s.box.Seal(gitURL), req.GitBranch, req.GitComposePath, gitKey, boolInt(req.AutoDeploy), req.Compose, s.box.Seal(string(envJSON)), string(opts), time.Now().Unix(), p.ID)
	s.audit(r, u, "project.update", p.Name, "", err)
	if err != nil {
		return err
	}
	out := map[string]any{"id": p.ID}
	if req.Deploy {
		p, _ = s.getProject(p.ID)
		jobID, depID := s.startDeploy(p, "manual", u.Username, 0)
		out["jobId"], out["deploymentId"] = jobID, depID
	}
	return ok(w, out)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	if p.Kind == "database" {
		return bad("请在数据库页面删除实例")
	}
	removeVolumes := r.URL.Query().Get("volumes") == "1"
	removeFiles := r.URL.Query().Get("files") == "1"
	jobID := s.deleteProjectJob(p, u.Username, removeVolumes, removeFiles)
	s.audit(r, u, "project.delete", p.Name, fmt.Sprintf("删除卷 %v", removeVolumes), nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) deleteProjectJob(p *Project, actor string, removeVolumes, removeFiles bool) string {
	return s.jobs.Submit(jobSpec("project.delete", "删除项目 "+p.Name, p.Name, actor, "project:"+p.Name), func(run *jobRun) error {
		cp := s.composeProject(p, true)
		args := []string{"down", "--remove-orphans"}
		if removeVolumes {
			args = append(args, "--volumes")
		}
		if err := cp.Run(run.Context(), run, args...); err != nil {
			// Fall back to removing containers by label when the files are gone.
			run.Logf("compose down 失败（%v），按标签清理容器", err)
			cs, _ := s.dk.ProjectContainers(run.Context(), p.Name)
			for _, c := range cs {
				_ = s.dk.RemoveContainer(run.Context(), c.Id, true)
			}
		}
		if _, err := s.db.Exec(`DELETE FROM projects WHERE id=?`, p.ID); err != nil {
			return err
		}
		if removeFiles && p.Source != "imported" {
			_ = os.RemoveAll(s.projectDir(p))
		}
		_ = s.syncCaddy(run.Context())
		run.Logf("项目已删除")
		return nil
	})
}

// ---------- deploy keys / webhook ----------

func newDeployKey() (string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "compose-panel")
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(block)), nil
}

func publicKeyFromPrivate(privPEM string) string {
	signer, err := ssh.ParsePrivateKey([]byte(privPEM))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " compose-panel"
}

func (s *Server) handleRotateWebhook(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	tok := auth.RandomToken(18)
	sec := auth.RandomToken(24)
	_, err = s.db.Exec(`UPDATE projects SET webhook_token=?, webhook_secret=? WHERE id=?`, tok, s.box.Seal(sec), p.ID)
	s.audit(r, u, "project.webhook_rotate", p.Name, "", err)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"webhookUrl": "/hooks/p/" + tok, "secret": sec})
}

func (s *Server) handleRegenDeployKey(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	k, err := newDeployKey()
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE projects SET git_key=? WHERE id=?`, s.box.Seal(k), p.ID)
	s.audit(r, u, "project.deploy_key", p.Name, "重新生成部署密钥", err)
	return ok(w, map[string]any{"deployKey": publicKeyFromPrivate(k)})
}

// ---------- validate / actions ----------

func (s *Server) handleValidateCompose(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Compose string   `json:"compose"`
		Env     []EnvVar `json:"env"`
		Name    string   `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "panel-validate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	_ = os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(req.Compose), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "panel.env"), []byte(renderEnv(req.Env)), 0o600)
	name := req.Name
	if !projectNameRe.MatchString(name) {
		name = "validate"
	}
	cp := compose.Project{Name: name, Dir: dir, Files: []string{filepath.Join(dir, "compose.yml")}, EnvFile: filepath.Join(dir, "panel.env")}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cfg, err := cp.LoadConfig(ctx, nil)
	if err != nil {
		return ok(w, map[string]any{"valid": false, "error": cleanComposeErr(err.Error(), dir), "warnings": []string{}})
	}
	return ok(w, map[string]any{"valid": true, "services": cfg.ServiceNames(), "warnings": composeWarnings(cfg), "site": s.suggestSite(ctx, cfg)})
}

func cleanComposeErr(msg, dir string) string {
	return strings.ReplaceAll(msg, dir+string(os.PathSeparator), "")
}

func composeWarnings(cfg *compose.Config) []string {
	warn := []string{}
	for _, n := range cfg.ServiceNames() {
		sv := cfg.Services[n]
		if strings.HasSuffix(sv.Image, ":latest") || (sv.Image != "" && !strings.Contains(strings.TrimPrefix(sv.Image, "localhost:"), ":") && !strings.Contains(sv.Image, "@")) {
			warn = append(warn, fmt.Sprintf("%s：镜像未固定版本（%s），回滚可能不可靠", n, sv.Image))
		}
		if sv.Restart == "" || sv.Restart == "no" {
			warn = append(warn, fmt.Sprintf("%s：未设置 restart 策略，主机重启后不会自动启动", n))
		}
		if !sv.HasMemLimit() {
			warn = append(warn, fmt.Sprintf("%s：未设置内存限制", n))
		}
		for _, p := range sv.Ports {
			if ip, _ := p["host_ip"].(string); ip == "" || ip == "0.0.0.0" {
				if pub := fmt.Sprint(p["published"]); pub != "" && pub != "<nil>" {
					warn = append(warn, fmt.Sprintf("%s：端口 %s 暴露在所有网卡上，建议通过反向代理访问", n, pub))
				}
			}
		}
	}
	return warn
}

type projectActionStep struct {
	Log           string
	Args          []string
	ProgressAfter int
	DumpUnhealthy bool
}

type projectActionPlan struct {
	Title     string
	Steps     []projectActionStep
	SyncCaddy bool
}

var simpleActionTitles = map[string]string{
	"start":   "启动",
	"stop":    "停止",
	"restart": "重启",
	"pull":    "拉取镜像",
}

// buildProjectAction validates a project control action and constructs its compose command plan.
func buildProjectAction(p *Project, action, service string) (projectActionPlan, error) {
	service = strings.TrimSpace(service)
	titleFor := func(base string) string {
		if service != "" {
			return base + " " + p.Name + "/" + service
		}
		return base + " " + p.Name
	}

	switch action {
	case "start", "stop", "restart", "pull":
		args := []string{action}
		if service != "" {
			args = append(args, service)
		}
		return projectActionPlan{
			Title: titleFor(simpleActionTitles[action]),
			Steps: []projectActionStep{{Args: args}},
		}, nil
	case "down":
		if service != "" {
			return projectActionPlan{}, bad("down 不支持单个服务")
		}
		return projectActionPlan{
			Title: titleFor("下线"),
			Steps: []projectActionStep{{Args: []string{"down", "--remove-orphans"}}},
		}, nil
	case "recreate":
		if p.Kind == "database" {
			return projectActionPlan{}, bad("数据库实例请在数据库页面管理镜像版本")
		}
		pullArgs := []string{"pull", "--ignore-buildable"}
		upArgs := []string{"up", "-d", "--force-recreate"}
		if service != "" {
			pullArgs = append(pullArgs, service)
			upArgs = append(upArgs, "--no-deps")
		} else {
			upArgs = append(upArgs, "--remove-orphans")
		}
		upArgs = append(upArgs, composeWaitArgs(p.Options)...)
		if service != "" {
			upArgs = append(upArgs, service)
		}
		return projectActionPlan{
			Title:     titleFor("重建容器"),
			SyncCaddy: true,
			Steps: []projectActionStep{
				{Log: "==> 强制拉取镜像", Args: pullArgs, ProgressAfter: 50},
				{Log: "==> 重建并启动容器", Args: upArgs, ProgressAfter: 100, DumpUnhealthy: true},
			},
		}, nil
	default:
		return projectActionPlan{}, bad("未知操作")
	}
}

func (s *Server) projectHasEnabledSites(projectID int64) bool {
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM sites WHERE project_id=? AND enabled=1`, projectID).Scan(&n)
	return n > 0
}

func (s *Server) handleProjectAction(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Action  string `json:"action"`
		Service string `json:"service"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	service := strings.TrimSpace(req.Service)
	plan, err := buildProjectAction(p, req.Action, service)
	if err != nil {
		return err
	}
	jobID := s.jobs.Submit(jobSpec("project."+req.Action, plan.Title, p.Name, u.Username, "project:"+p.Name), func(run *jobRun) error {
		ctx := run.Context()
		if err := s.prepareWorkdir(run, p, nil, false); err != nil {
			return err
		}
		cp := s.composeProject(p, true)
		cleanup := s.registryConfig(&cp)
		defer cleanup()
		for _, step := range plan.Steps {
			if step.Log != "" {
				run.Logf("%s", step.Log)
			}
			if err := cp.Run(ctx, run, step.Args...); err != nil {
				if step.DumpUnhealthy {
					s.dumpUnhealthy(run, p)
				}
				return err
			}
			if step.ProgressAfter > 0 {
				run.SetProgress(step.ProgressAfter)
			}
		}
		needCaddySync := plan.SyncCaddy
		if !needCaddySync {
			switch req.Action {
			case "start", "restart", "recreate", "stop", "down":
				needCaddySync = s.projectHasEnabledSites(p.ID)
			}
		}
		if needCaddySync {
			if err := s.syncCaddy(ctx); err != nil {
				run.Logf("注意：同步反向代理配置失败：%v", err)
			}
		}
		return nil
	})
	s.audit(r, u, "project."+req.Action, p.Name, service, nil)
	return ok(w, map[string]any{"jobId": jobID})
}

// ---------- import existing compose projects ----------

func (s *Server) handleImportScan(w http.ResponseWriter, r *http.Request, u *User) error {
	cs, err := s.dk.ListContainers(r.Context(), true, map[string][]string{"label": {"com.docker.compose.project"}})
	if err != nil {
		return err
	}
	known := map[string]bool{}
	rows, _ := s.db.Query(`SELECT name FROM projects`)
	if rows != nil {
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			known[n] = true
		}
		rows.Close()
	}
	type cand struct {
		Name       string   `json:"name"`
		Workdir    string   `json:"workdir"`
		Files      []string `json:"files"`
		Services   []string `json:"services"`
		Containers int      `json:"containers"`
		Running    int      `json:"running"`
		Managed    bool     `json:"managed"`
	}
	m := map[string]*cand{}
	for _, c := range cs {
		name := c.Labels["com.docker.compose.project"]
		if name == "" {
			continue
		}
		x := m[name]
		if x == nil {
			x = &cand{Name: name, Workdir: c.Labels["com.docker.compose.project.working_dir"], Managed: known[name]}
			if f := c.Labels["com.docker.compose.project.config_files"]; f != "" {
				x.Files = strings.Split(f, ",")
			}
			m[name] = x
		}
		x.Containers++
		if c.State == "running" {
			x.Running++
		}
		if sv := c.Labels["com.docker.compose.service"]; sv != "" {
			x.Services = append(x.Services, sv)
		}
	}
	out := []*cand{}
	for _, x := range m {
		sort.Strings(x.Services)
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return ok(w, out)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Name    string   `json:"name"`
		Workdir string   `json:"workdir"`
		Files   []string `json:"files"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Name == "" || req.Workdir == "" {
		return bad("缺少项目名或目录")
	}
	if len(req.Files) == 0 {
		req.Files = []string{filepath.Join(req.Workdir, "compose.yml")}
	}
	for _, f := range req.Files {
		if _, err := os.Stat(f); err != nil {
			return bad("面板容器内看不到文件 %s：请确认该目录已挂载到面板（与主机路径一致）", f)
		}
	}
	opts, _ := json.Marshal(ProjectOptions{ConfigFiles: req.Files})
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO projects(name,kind,source,workdir,webhook_token,env,options,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		req.Name, "app", "imported", req.Workdir, auth.RandomToken(18), s.box.Seal("[]"), string(opts), now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return bad("项目已存在")
		}
		return err
	}
	id, _ := res.LastInsertId()
	s.audit(r, u, "project.import", req.Name, req.Workdir, nil)
	return ok(w, map[string]any{"id": id})
}
