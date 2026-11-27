package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"compose-panel/internal/auth"
	"compose-panel/internal/compose"
	"compose-panel/internal/jobs"
	"compose-panel/internal/notify"
)

type jobRun = jobs.Run

func jobSpec(kind, title, target, actor, lock string) jobs.Spec {
	return jobs.Spec{Kind: kind, Title: title, Target: target, Actor: actor, LockKey: lock}
}

// startDeploy records a deployment and runs it as a job. fromDeployment > 0 means rollback to it.
func (s *Server) startDeploy(p *Project, trigger, actor string, fromDeployment int64) (string, int64) {
	var num int
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(number),0)+1 FROM deployments WHERE project_id=?`, p.ID).Scan(&num)
	res, err := s.db.Exec(`INSERT INTO deployments(project_id,number,status,trigger,actor,created_at) VALUES(?,?,?,?,?,?)`,
		p.ID, num, "queued", trigger, actor, time.Now().Unix())
	if err != nil {
		return "", 0
	}
	depID, _ := res.LastInsertId()
	title := fmt.Sprintf("部署 %s #%d", p.Name, num)
	if fromDeployment > 0 {
		title = fmt.Sprintf("回滚 %s #%d", p.Name, num)
	}
	jobID := s.jobs.Submit(jobSpec("deploy", title, p.Name, actor, "project:"+p.Name), func(run *jobRun) error {
		return s.runDeploy(run, p.ID, depID, trigger, fromDeployment)
	})
	_, _ = s.db.Exec(`UPDATE deployments SET job_id=? WHERE id=?`, jobID, depID)
	return jobID, depID
}

type deployState struct {
	compose   string
	env       []EnvVar
	files     []projectFile
	hasFiles  bool
	commitSHA string
	commitMsg string
}

func (s *Server) replaceProjectFiles(projectID int64, files []projectFile) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM project_files WHERE project_id=?`, projectID); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.Exec(`INSERT INTO project_files(project_id,path,content) VALUES(?,?,?)`, projectID, f.Path, f.Content); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func removeStaleInlineFiles(dir string, oldFiles, newFiles []projectFile) {
	keep := make(map[string]bool, len(newFiles))
	for _, f := range newFiles {
		keep[f.Path] = true
	}
	for _, f := range oldFiles {
		if keep[f.Path] {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(f.Path))
		if strings.HasPrefix(target, dir+string(os.PathSeparator)) {
			_ = os.Remove(target)
		}
	}
}

func deployArchiveReason(trigger string, fromDeployment int64) string {
	if fromDeployment > 0 || trigger == "rollback" {
		return "rollback"
	}
	if trigger == "image_update" {
		return "auto_update"
	}
	return "deploy"
}

func (s *Server) runDeploy(run *jobRun, projectID, depID int64, trigger string, fromDeployment int64) (err error) {
	ctx := run.Context()
	p, err := s.getProject(projectID)
	if err != nil {
		return err
	}
	_, _ = s.db.Exec(`UPDATE deployments SET status='running' WHERE id=?`, depID)
	finish := func(status, msg string) {
		_, _ = s.db.Exec(`UPDATE deployments SET status=?, error=?, finished_at=? WHERE id=?`, status, msg, time.Now().Unix(), depID)
	}
	defer func() {
		if err != nil {
			var cur string
			_ = s.db.QueryRow(`SELECT status FROM deployments WHERE id=?`, depID).Scan(&cur)
			if cur == "running" || cur == "queued" {
				status := "failed"
				if ctx.Err() != nil {
					status = "canceled"
				}
				finish(status, err.Error())
			}
			s.notify.Emit(notify.Event{Type: "deploy_failed", Title: "部署失败：" + p.Name, Body: err.Error(), Target: p.Name})
		}
	}()

	var pin *deployState
	if fromDeployment > 0 {
		st, err := s.deploymentState(fromDeployment)
		if err != nil {
			return fmt.Errorf("读取回滚目标失败：%w", err)
		}
		pin = st
		run.Logf("回滚到部署快照 #%d", fromDeployment)
		envJSON, _ := json.Marshal(st.env)
		now := time.Now().Unix()
		if p.Source == "inline" {
			oldFiles := s.projectFiles(p.ID)
			_, _ = s.db.Exec(`UPDATE projects SET compose=?, env=?, updated_at=? WHERE id=?`, st.compose, s.box.Seal(string(envJSON)), now, p.ID)
			p.Compose, p.Env = st.compose, st.env
			if st.hasFiles {
				if err := s.replaceProjectFiles(p.ID, st.files); err != nil {
					return fmt.Errorf("恢复项目文件失败：%w", err)
				}
				removeStaleInlineFiles(s.projectDir(p), oldFiles, st.files)
			}
		} else {
			_, _ = s.db.Exec(`UPDATE projects SET env=?, updated_at=? WHERE id=?`, s.box.Seal(string(envJSON)), now, p.ID)
			p.Env = st.env
		}
	}

	run.Logf("==> 准备工作目录")
	run.SetProgress(5)
	state, err := s.prepareDeploy(run, p, pin)
	if err != nil {
		return err
	}
	envJSON, _ := json.Marshal(state.env)
	filesJSON := ""
	if p.Source == "inline" {
		b, _ := json.Marshal(nonNil(state.files))
		filesJSON = string(b)
	}
	_, _ = s.db.Exec(`UPDATE deployments SET compose=?, env=?, files_json=?, commit_sha=?, commit_msg=? WHERE id=?`,
		state.compose, s.box.Seal(string(envJSON)), filesJSON, state.commitSHA, state.commitMsg, depID)

	run.SetProgress(25)
	if s.enterDeployMaintenance(ctx, p.ID) {
		run.Logf("==> 已为站点开启部署维护页")
		defer func() {
			s.leaveDeployMaintenance(p.ID)
			run.Logf("==> 已关闭部署维护页")
		}()
	}
	if err := s.composeUp(run, p, deployArchiveReason(trigger, fromDeployment)); err != nil {
		run.Logf("==> 部署失败：%v", err)
		if !boolOr(p.Options.AutoRollback, true) || fromDeployment > 0 {
			return err
		}
		prev := s.previousSuccess(p.ID, depID)
		if prev == 0 {
			run.Logf("没有可回滚的成功部署")
			return err
		}
		run.Logf("==> 自动回滚到上一次成功的部署")
		st, rerr := s.deploymentState(prev)
		if rerr != nil {
			return fmt.Errorf("%v；回滚失败：%v", err, rerr)
		}
		if _, rerr = s.prepareDeploy(run, p, st); rerr == nil {
			rerr = s.composeUp(run, p, "rollback")
		}
		if rerr != nil {
			return fmt.Errorf("%v；自动回滚也失败：%v", err, rerr)
		}
		finish("rolled_back", err.Error())
		s.notify.Emit(notify.Event{Type: "deploy_failed", Title: "部署失败，已自动回滚：" + p.Name, Body: err.Error(), Target: p.Name})
		_ = s.syncCaddy(ctx)
		return fmt.Errorf("部署失败，已自动回滚：%v", err)
	}
	run.SetProgress(90)
	if err := s.syncCaddy(ctx); err != nil {
		run.Logf("警告：反向代理配置未更新：%v", err)
	}
	finish("success", "")
	run.Logf("==> 部署成功")
	s.recheckAfterDeploy(p.ID)
	s.notify.Emit(notify.Event{Type: "deploy_success", Title: "部署成功：" + p.Name, Body: state.commitMsg, Target: p.Name})
	return nil
}

func (s *Server) previousSuccess(projectID, before int64) int64 {
	var id int64
	_ = s.db.QueryRow(`SELECT id FROM deployments WHERE project_id=? AND id<? AND status='success' ORDER BY id DESC LIMIT 1`, projectID, before).Scan(&id)
	return id
}

func (s *Server) deploymentState(id int64) (*deployState, error) {
	st := &deployState{}
	var envEnc, filesJSON string
	if err := s.db.QueryRow(`SELECT compose, env, files_json, commit_sha, commit_msg FROM deployments WHERE id=?`, id).Scan(&st.compose, &envEnc, &filesJSON, &st.commitSHA, &st.commitMsg); err != nil {
		return nil, err
	}
	if envEnc != "" {
		raw, err := s.box.Open(envEnc)
		if err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &st.env)
	}
	if filesJSON != "" {
		if err := json.Unmarshal([]byte(filesJSON), &st.files); err == nil {
			st.hasFiles = true
		}
	}
	return st, nil
}

// prepareDeploy writes files (or checks out git) and returns the snapshot that will be deployed.
func (s *Server) prepareDeploy(run *jobRun, p *Project, pin *deployState) (*deployState, error) {
	if pin != nil {
		p.Env = pin.env
		if p.Source == "inline" {
			p.Compose = pin.compose
		}
	}
	if err := s.prepareWorkdir(run, p, pin, true); err != nil {
		return nil, err
	}
	st := &deployState{env: p.Env, compose: p.Compose}
	if p.Source == "inline" {
		if pin != nil && pin.hasFiles {
			st.files = pin.files
		} else {
			st.files = s.projectFiles(p.ID)
		}
		st.hasFiles = true
	}
	cp := s.composeProject(p, false)
	if p.Source != "inline" && len(cp.Files) > 0 {
		if b, err := os.ReadFile(cp.Files[0]); err == nil {
			st.compose = string(b)
		}
	}
	if p.Source == "git" {
		st.commitSHA, st.commitMsg = gitHead(run.Context(), filepath.Join(s.projectDir(p), "src"))
		run.Logf("提交 %s %s", short(st.commitSHA), st.commitMsg)
	}
	return st, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// prepareWorkdir writes compose, env and override files. For git projects it syncs the repo when syncGit is true,
// or only clones if the checkout is missing when syncGit is false (lifecycle actions).
func (s *Server) prepareWorkdir(run *jobRun, p *Project, pin *deployState, syncGit bool) error {
	ctx := run.Context()
	dir := s.projectDir(p)
	if err := os.MkdirAll(filepath.Join(dir, ".panel"), 0o750); err != nil {
		return err
	}
	switch p.Source {
	case "inline":
		if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(p.Compose), 0o640); err != nil {
			return err
		}
		dbFiles := s.projectFiles(p.ID)
		files := dbFiles
		if pin != nil && pin.hasFiles {
			removeStaleInlineFiles(dir, dbFiles, pin.files)
			files = pin.files
		}
		for _, f := range files {
			target := filepath.Join(dir, filepath.FromSlash(f.Path))
			if !strings.HasPrefix(target, dir+string(os.PathSeparator)) {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o750)
			if err := os.WriteFile(target, []byte(f.Content), 0o640); err != nil {
				return err
			}
		}
	case "git":
		srcGit := filepath.Join(dir, "src", ".git")
		if syncGit {
			ref := ""
			if pin != nil {
				ref = pin.commitSHA
			}
			if err := s.gitSync(run, p, ref); err != nil {
				return err
			}
		} else if _, err := os.Stat(srcGit); err != nil {
			if err := s.gitSync(run, p, ""); err != nil {
				return err
			}
		}
	}
	if p.Source != "imported" || len(p.Env) > 0 {
		if err := os.WriteFile(filepath.Join(dir, ".panel", "panel.env"), []byte(renderEnv(p.Env)), 0o600); err != nil {
			return err
		}
	}
	// Validate without the override first, then write the override.
	cp := s.composeProject(p, false)
	cfg, err := cp.LoadConfig(ctx, nil)
	if err != nil {
		return fmt.Errorf("Compose 校验失败：%s", cleanComposeErr(err.Error(), dir))
	}
	extra := composeExtraNetworks(p, cfg, s.cfg.ProxyNetwork, s.cfg.DBNetwork)
	for _, n := range []string{s.cfg.ProxyNetwork, s.cfg.DBNetwork} {
		if err := s.dk.EnsureNetwork(ctx, n); err != nil {
			return fmt.Errorf("创建网络 %s 失败：%w", n, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".panel", "override.yml"), compose.Override(cfg, extra), 0o640); err != nil {
		return err
	}
	run.Logf("服务：%s", strings.Join(cfg.ServiceNames(), ", "))
	return nil
}

// composeExtraNetworks returns the shared panel networks to attach to each service.
// Services that declare network_mode (e.g. host, none, service:...) cannot be combined
// with networks in Compose, so they are skipped automatically.
func composeExtraNetworks(p *Project, cfg *compose.Config, proxyNet, dbNet string) map[string][]string {
	extra := map[string][]string{}
	if p.Options.SkipNetworks || p.Kind == "database" || cfg == nil {
		return extra
	}
	for _, n := range cfg.ServiceNames() {
		sv := cfg.Services[n]
		if sv.NetworkMode != "" {
			continue
		}
		if sv.Networks != nil {
			if _, host := sv.Networks["host"]; host {
				continue
			}
		}
		extra[n] = []string{proxyNet, dbNet}
	}
	return extra
}

type projectFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (s *Server) projectFiles(id int64) []projectFile {
	out := []projectFile{}
	rows, err := s.db.Query(`SELECT path, content FROM project_files WHERE project_id=? ORDER BY path`, id)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var f projectFile
		if rows.Scan(&f.Path, &f.Content) == nil {
			out = append(out, f)
		}
	}
	return out
}

func (s *Server) composeUp(run *jobRun, p *Project, archiveReason string) error {
	ctx := run.Context()
	cp := s.composeProject(p, true)
	cleanup := s.registryConfig(&cp)
	defer cleanup()
	if boolOr(p.Options.Pull, true) {
		run.Logf("==> 拉取镜像")
		if err := cp.Run(ctx, run, "pull", "--ignore-buildable", "--quiet"); err != nil {
			return fmt.Errorf("拉取镜像失败：%w", err)
		}
	}
	run.SetProgress(50)
	run.Logf("==> 启动服务")
	var staged []*stagedLogArchive
	if archiveReason != "" {
		staged = s.stageProjectLogs(ctx, p.Name, "", archiveReason)
		defer func() {
			s.finalizeProjectLogs(context.Background(), p.Name, staged, run)
		}()
	}
	args := []string{"up", "-d", "--remove-orphans", "--build"}
	args = append(args, composeWaitArgs(p.Options)...)
	if err := cp.Run(ctx, run, args...); err != nil {
		s.dumpUnhealthy(run, p)
		return fmt.Errorf("服务启动失败：%w", err)
	}
	return nil
}

// composeWaitArgs returns the --wait flags configured for a project's compose up invocations.
func composeWaitArgs(opts ProjectOptions) []string {
	if !boolOr(opts.Wait, true) {
		return nil
	}
	t := opts.WaitTimeout
	if t <= 0 {
		t = 180
	}
	return []string{"--wait", "--wait-timeout", fmt.Sprint(t)}
}

// dumpUnhealthy appends the last log lines of failing containers to the job log.
func (s *Server) dumpUnhealthy(run *jobRun, p *Project) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cs, _ := s.dk.ProjectContainers(ctx, p.Name)
	for _, c := range cs {
		if c.State == "running" && containerHealth(c) != "unhealthy" {
			continue
		}
		run.Logf("---- %s（%s）最后 30 行日志 ----", c.Name(), c.Status)
		rc, err := s.dk.Logs(ctx, c.Id, "30", false, false, 0)
		if err != nil {
			continue
		}
		_, _ = io.Copy(run, rc)
		rc.Close()
	}
}

// registryConfig writes a temporary DOCKER_CONFIG with stored registry credentials.
func (s *Server) registryConfig(cp *compose.Project) func() {
	rows, err := s.db.Query(`SELECT host, username, password FROM registries`)
	if err != nil {
		return func() {}
	}
	auths := map[string]any{}
	for rows.Next() {
		var host, user, enc string
		if rows.Scan(&host, &user, &enc) == nil {
			entry := map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + s.box.MustOpen(enc)))}
			auths[host] = entry
			if normRegistryHost(host) == "docker.io" {
				auths["https://index.docker.io/v1/"] = entry
				auths["docker.io"] = entry
			}
		}
	}
	rows.Close()
	if len(auths) == 0 {
		return func() {}
	}
	dir, err := os.MkdirTemp("", "panel-docker-")
	if err != nil {
		return func() {}
	}
	b, _ := json.Marshal(map[string]any{"auths": auths})
	_ = os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
	cp.DockerCfg = dir
	return func() { os.RemoveAll(dir) }
}

// ---------- git ----------

// sanitizeGitURL strips embedded user:pass credentials from HTTP(S) git URLs
// and returns the sanitized URL plus an HTTP Basic Authorization extraHeader.
func sanitizeGitURL(raw string) (cleanURL, extraHeader string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User == nil {
		return raw, ""
	}
	user := u.User.Username()
	pass, hasPass := u.User.Password()
	if user == "" && !hasPass {
		return raw, ""
	}
	uCopy := *u
	uCopy.User = nil
	cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return uCopy.String(), "Authorization: Basic " + cred
}

func (s *Server) gitEnv(p *Project, extraHeader string) ([]string, func(), error) {
	base := os.Environ()
	env := make([]string, 0, len(base)+6)
	for _, kv := range base {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") || strings.HasPrefix(kv, "GIT_CONFIG_KEY_") || strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	if extraHeader != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0="+extraHeader,
		)
	}
	cleanup := func() {}
	if p.gitKeyEnc != "" {
		key, err := s.box.Open(p.gitKeyEnc)
		if err != nil {
			return nil, cleanup, err
		}
		f, err := os.CreateTemp("", "panel-key-")
		if err != nil {
			return nil, cleanup, err
		}
		_, _ = f.WriteString(key)
		f.Close()
		_ = os.Chmod(f.Name(), 0o600)
		cleanup = func() { os.Remove(f.Name()) }
		known := filepath.Join(s.cfg.DataDir, "known_hosts")
		env = append(env, fmt.Sprintf(`GIT_SSH_COMMAND=ssh -i "%s" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="%s"`, f.Name(), known))
	}
	return env, cleanup, nil
}

func (s *Server) gitSync(run *jobRun, p *Project, ref string) error {
	ctx := run.Context()
	if strings.HasPrefix(p.GitBranch, "-") || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("无效的 Git 分支或提交标识")
	}
	cleanURL, extraHeader := sanitizeGitURL(p.GitURL)
	if strings.HasPrefix(cleanURL, "-") {
		return fmt.Errorf("无效的 Git 仓库地址")
	}
	env, cleanup, err := s.gitEnv(p, extraHeader)
	if err != nil {
		return err
	}
	defer cleanup()
	src := filepath.Join(s.projectDir(p), "src")
	git := func(args ...string) error {
		c := exec.CommandContext(ctx, "git", args...)
		c.Env = env
		c.Stdout, c.Stderr = run, run
		fmt.Fprintf(run, "$ git %s\n", strings.Join(args, " "))
		return c.Run()
	}
	refspec := "+refs/heads/" + p.GitBranch + ":refs/remotes/origin/" + p.GitBranch
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		_ = os.RemoveAll(src)
		if err := git("clone", "--depth", "50", "--branch", p.GitBranch, "--single-branch", cleanURL, src); err != nil {
			return fmt.Errorf("克隆仓库失败：%w", err)
		}
	} else {
		if err := git("-C", src, "remote", "set-url", "origin", cleanURL); err != nil {
			return err
		}
		if err := git("-C", src, "fetch", "--depth", "50", "--prune", "origin", refspec); err != nil {
			return fmt.Errorf("拉取仓库失败：%w", err)
		}
	}
	if ref != "" {
		if err := git("-C", src, "checkout", "--force", ref); err != nil {
			if ferr := git("-C", src, "fetch", "origin", ref); ferr != nil {
				_ = git("-C", src, "fetch", "--unshallow", "origin", refspec)
			}
			if err := git("-C", src, "checkout", "--force", ref); err != nil {
				return fmt.Errorf("检出 %s 失败：%w", ref, err)
			}
		}
		return nil
	}
	if err := git("-C", src, "checkout", "--force", "-B", p.GitBranch, "origin/"+p.GitBranch); err != nil {
		return fmt.Errorf("检出 origin/%s 失败：%w", p.GitBranch, err)
	}
	return nil
}

func gitHead(ctx context.Context, src string) (string, string) {
	out, err := exec.CommandContext(ctx, "git", "-C", src, "log", "-1", "--format=%H%n%s").Output()
	if err != nil {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

// ---------- HTTP ----------

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	jobID, depID := s.startDeploy(p, "manual", u.Username, 0)
	s.audit(r, u, "project.deploy", p.Name, "", nil)
	return ok(w, map[string]any{"jobId": jobID, "deploymentId": depID})
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT `+deployCols+` FROM deployments WHERE project_id=? ORDER BY number DESC LIMIT ?`, p.ID, min(max(queryInt(r, "limit", 50), 1), 200))
	if err != nil {
		return err
	}
	return ok(w, scanDeploys(rows))
}

func (s *Server) deploymentProject(r *http.Request, u *User) (*Project, int64, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, 0, err
	}
	var pid int64
	if err := s.db.QueryRow(`SELECT project_id FROM deployments WHERE id=?`, id).Scan(&pid); err != nil {
		return nil, 0, errNotFound
	}
	if !u.canProject(pid) {
		return nil, 0, errNotFound
	}
	p, err := s.getProject(pid)
	return p, id, err
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request, u *User) error {
	p, id, err := s.deploymentProject(r, u)
	if err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT `+deployCols+` FROM deployments WHERE id=?`, id)
	if err != nil {
		return err
	}
	list := scanDeploys(rows)
	if len(list) == 0 {
		return errNotFound
	}
	st, _ := s.deploymentState(id)
	env := []EnvVar{}
	if st != nil {
		for _, e := range st.env {
			if e.Secret {
				e.Value = "••••••"
			}
			env = append(env, e)
		}
	}
	compose := ""
	if st != nil {
		compose = st.compose
	}
	// The previous deployment, for diffing.
	prevCompose := ""
	var prevID int64
	_ = s.db.QueryRow(`SELECT id, compose FROM deployments WHERE project_id=? AND id<? AND compose<>'' ORDER BY id DESC LIMIT 1`, p.ID, id).Scan(&prevID, &prevCompose)
	return ok(w, map[string]any{"deployment": list[0], "project": map[string]any{"id": p.ID, "name": p.Name},
		"compose": compose, "env": env, "previousCompose": prevCompose, "previousId": prevID})
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request, u *User) error {
	p, id, err := s.deploymentProject(r, u)
	if err != nil {
		return err
	}
	var status, comp string
	_ = s.db.QueryRow(`SELECT status, compose FROM deployments WHERE id=?`, id).Scan(&status, &comp)
	if comp == "" {
		return bad("该部署没有快照，无法回滚")
	}
	jobID, depID := s.startDeploy(p, "rollback", u.Username, id)
	s.audit(r, u, "project.rollback", p.Name, fmt.Sprintf("回滚到部署 %d", id), nil)
	return ok(w, map[string]any{"jobId": jobID, "deploymentId": depID})
}

// handleWebhook triggers a deploy from GitHub, Gitea, GitLab or a plain POST.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	var id int64
	var secEnc string
	var auto int
	if tok == "" || s.db.QueryRow(`SELECT id, webhook_secret, auto_deploy FROM projects WHERE webhook_token=?`, tok).Scan(&id, &secEnc, &auto) != nil {
		http.Error(w, "not found", 404)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if secEnc != "" {
		sec := s.box.MustOpen(secEnc)
		if !verifyWebhook(r, body, sec) {
			s.audit(r, nil, "webhook", fmt.Sprint(id), "", fmt.Errorf("签名校验失败"))
			http.Error(w, "invalid signature", 401)
			return
		}
	}
	p, err := s.getProject(id)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	if auto != 1 {
		writeJSON(w, 202, map[string]any{"skipped": "auto deploy disabled"})
		return
	}
	var payload struct {
		Ref string `json:"ref"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Ref != "" && p.Source == "git" && payload.Ref != "refs/heads/"+p.GitBranch {
		writeJSON(w, 202, map[string]any{"skipped": "branch " + payload.Ref})
		return
	}
	jobID, depID := s.startDeploy(p, "webhook", "webhook", 0)
	s.audit(r, &User{Username: "webhook"}, "project.deploy", p.Name, "Webhook 触发", nil)
	writeJSON(w, 202, map[string]any{"jobId": jobID, "deploymentId": depID})
}

func verifyWebhook(r *http.Request, body []byte, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		return auth.ConstantEqual(strings.TrimPrefix(sig, "sha256="), want)
	}
	if sig := r.Header.Get("X-Gitea-Signature"); sig != "" {
		return auth.ConstantEqual(sig, want)
	}
	if sig := r.Header.Get("X-Panel-Signature"); sig != "" {
		return auth.ConstantEqual(strings.TrimPrefix(sig, "sha256="), want)
	}
	if t := r.Header.Get("X-Gitlab-Token"); t != "" {
		return auth.ConstantEqual(t, secret)
	}
	if t := r.URL.Query().Get("secret"); t != "" {
		return auth.ConstantEqual(t, secret)
	}
	return false
}
