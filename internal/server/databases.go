package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"compose-panel/internal/auth"
	"compose-panel/internal/dbmgr"
	"compose-panel/internal/docker"
)

type dbInstance struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"projectId"`
	Name      string `json:"name"`
	Engine    string `json:"engine"`
	Version   string `json:"version"`
	Image     string `json:"image"` // custom image; "" = official image for engine+version
	RootUser  string `json:"rootUser"`
	Port      int    `json:"port"`
	Exposure  string `json:"exposure"`
	CreatedAt int64  `json:"createdAt"`
	Status    string `json:"status"`
	Container string `json:"container"`
	Host      string `json:"host"`
	password  string
	dk        *docker.Client
}

func (s *Server) getDB(id int64) (*dbInstance, error) {
	d := &dbInstance{dk: s.dk}
	var enc string
	err := s.db.QueryRow(`SELECT d.id, d.project_id, p.name, d.engine, d.version, d.root_user, d.root_password, d.port, d.exposure, d.created_at, d.image
		FROM db_instances d JOIN projects p ON p.id=d.project_id WHERE d.id=?`, id).
		Scan(&d.ID, &d.ProjectID, &d.Name, &d.Engine, &d.Version, &d.RootUser, &enc, &d.Port, &d.Exposure, &d.CreatedAt, &d.Image)
	if err != nil {
		return nil, errNotFound
	}
	d.password = s.box.MustOpen(enc)
	d.Container = d.Name + "-db-1"
	d.Host = d.Name
	return d, nil
}

func (s *Server) dbForUser(r *http.Request, u *User) (*dbInstance, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	d, err := s.getDB(id)
	if err != nil {
		return nil, err
	}
	if !u.canProject(d.ProjectID) {
		return nil, errNotFound
	}
	return d, nil
}

func (d *dbInstance) client() dbmgr.Client {
	return dbmgr.Client{Docker: d.dk, Engine: d.Engine, Container: d.Container, Password: d.password}
}

func (s *Server) handleDBEngines(w http.ResponseWriter, r *http.Request, u *User) error {
	return ok(w, dbmgr.Engines)
}

func (s *Server) handleListDatabases(w http.ResponseWriter, r *http.Request, u *User) error {
	rows, err := s.db.Query(`SELECT id FROM db_instances ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	byProject := s.containersByProject(r.Context())
	out := []map[string]any{}
	for _, id := range ids {
		d, err := s.getDB(id)
		if err != nil || !u.canProject(d.ProjectID) {
			continue
		}
		st, _, _ := summarizeStatus(byProject[d.Name])
		d.Status = st
		var lastBackup, lastStatus = int64(0), ""
		_ = s.db.QueryRow(`SELECT started_at, status FROM backups WHERE json_extract(source,'$.kind')='database' AND json_extract(source,'$.instanceId')=? ORDER BY started_at DESC LIMIT 1`, d.ID).Scan(&lastBackup, &lastStatus)
		var vol int64
		out = append(out, map[string]any{"instance": d, "lastBackupAt": lastBackup, "lastBackupStatus": lastStatus, "volumeSize": vol})
	}
	return ok(w, out)
}

func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Name     string `json:"name"`
		Engine   string `json:"engine"`
		Version  string `json:"version"`
		Image    string `json:"image"`
		Exposure string `json:"exposure"`
		Port     int    `json:"port"`
		MemLimit string `json:"memLimit"`
		InitDB   string `json:"initDb"`
		InitUser string `json:"initUser"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	if !projectNameRe.MatchString(req.Name) {
		return bad("实例名只能包含小写字母、数字、- 和 _")
	}
	eng, okEng := dbmgr.EngineByID(req.Engine)
	if !okEng {
		return bad("不支持的数据库类型")
	}
	if req.Version == "" {
		req.Version = eng.Versions[0]
	}
	if !contains(eng.Versions, req.Version) {
		return bad("不支持的版本")
	}
	if err := checkDBImage(eng.ID, req.Version, &req.Image); err != nil {
		return err
	}
	if req.MemLimit = strings.TrimSpace(req.MemLimit); req.MemLimit != "" && !memLimitRe.MatchString(req.MemLimit) {
		return bad("内存限制格式如 512m、1g")
	}
	if req.Port == 0 {
		req.Port = eng.Port
	}
	if req.Exposure == "" {
		req.Exposure = "internal"
	}
	if req.Exposure != "internal" && req.Exposure != "local" && req.Exposure != "public" {
		return bad("暴露方式无效")
	}
	if req.InitDB != "" && !dbmgr.ValidIdent(req.InitDB) || req.InitUser != "" && !dbmgr.ValidIdent(req.InitUser) {
		return bad("库名/用户名只能包含字母、数字和下划线")
	}
	var exists int
	_ = s.db.QueryRow(`SELECT count(*) FROM projects WHERE name=?`, req.Name).Scan(&exists)
	if exists > 0 {
		return bad("名称已被项目或实例占用")
	}
	compose, err := dbmgr.Compose(eng.ID, req.Version, req.Image, req.Name, req.Exposure, s.cfg.DBNetwork, req.Port, req.MemLimit)
	if err != nil {
		return err
	}
	pw := auth.RandomPassword(28)
	env := []EnvVar{{Key: "DB_ROOT_PASSWORD", Value: pw, Secret: true}}
	envJSON, _ := json.Marshal(env)
	opts, _ := json.Marshal(ProjectOptions{})
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO projects(name,description,kind,source,webhook_token,compose,env,options,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		req.Name, eng.Name+" "+req.Version, "database", "inline", auth.RandomToken(18), compose, s.box.Seal(string(envJSON)), string(opts), now, now)
	if err != nil {
		return err
	}
	pid, _ := res.LastInsertId()
	res, err = tx.Exec(`INSERT INTO db_instances(project_id,engine,version,root_user,root_password,port,exposure,created_at,image) VALUES(?,?,?,?,?,?,?,?,?)`,
		pid, eng.ID, req.Version, eng.RootUser, s.box.Seal(pw), req.Port, req.Exposure, now, req.Image)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return err
	}
	p, _ := s.getProject(pid)
	jobID, _ := s.startDeploy(p, "create", u.Username, 0)
	if req.InitDB != "" {
		// After the instance is healthy, create the first database and user.
		s.goBackground(func(bgCtx context.Context) {
			ctx, cancel := context.WithTimeout(bgCtx, 10*time.Minute)
			defer cancel()
			for ctx.Err() == nil {
				var status string
				_ = s.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=?`, jobID).Scan(&status)
				if status == "success" {
					break
				}
				if status == "failed" || status == "canceled" {
					return
				}
				timer := time.NewTimer(2 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			if ctx.Err() != nil {
				return
			}
			d, err := s.getDB(id)
			if err != nil {
				return
			}
			userPw := ""
			if req.InitUser != "" {
				userPw = auth.RandomPassword(24)
			}
			if err := d.client().CreateDatabase(ctx, req.InitDB, req.InitUser, userPw); err == nil && req.InitUser != "" {
				s.setSetting(fmt.Sprintf("dbinit_%d", id), s.box.Seal(req.InitUser+":"+userPw))
			}
		})
	}
	s.audit(r, u, "database.create", req.Name, eng.Name+" "+req.Version+" · "+req.Exposure, nil)
	return ok(w, map[string]any{"id": id, "projectId": pid, "jobId": jobID})
}

func (s *Server) handleGetDatabase(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	cs, _ := s.dk.ProjectContainers(r.Context(), d.Name)
	st, _, _ := summarizeStatus(cs)
	d.Status = st
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	metrics := map[string]string{}
	if st == "running" {
		metrics = d.client().Metrics(ctx)
	}
	eng, _ := dbmgr.EngineByID(d.Engine)
	conn := connectionInfo(d, s.cfg.DBNetwork)
	initCred := ""
	if v := s.setting(fmt.Sprintf("dbinit_%d", d.ID), ""); v != "" && u.can(roleOperator) {
		initCred = s.box.MustOpen(v)
	}
	return ok(w, map[string]any{"instance": d, "engine": eng, "metrics": metrics, "connection": conn, "initCredential": initCred})
}

func connectionInfo(d *dbInstance, network string) map[string]any {
	scheme := map[string]string{"postgres": "postgres", "mysql": "mysql", "mariadb": "mysql", "mongo": "mongodb", "redis": "redis"}[d.Engine]
	internal := fmt.Sprintf("%s://%s:<密码>@%s:%d", scheme, d.RootUser, d.Name, d.Port)
	if d.Engine == "redis" {
		internal = fmt.Sprintf("redis://:<密码>@%s:%d", d.Name, d.Port)
	}
	out := map[string]any{"host": d.Name, "port": d.Port, "network": network, "internalUrl": internal}
	switch d.Exposure {
	case "local":
		out["hostUrl"] = fmt.Sprintf("127.0.0.1:%d", d.Port)
	case "public":
		out["hostUrl"] = fmt.Sprintf("<服务器 IP>:%d", d.Port)
	}
	return out
}

func (s *Server) handleDatabaseCredentials(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	s.audit(r, u, "database.reveal_password", d.Name, "查看 root 密码", nil)
	return ok(w, map[string]any{"user": d.RootUser, "password": d.password})
}

func (s *Server) handleUpdateDatabase(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Exposure string  `json:"exposure"`
		Port     int     `json:"port"`
		Version  string  `json:"version"`
		Image    *string `json:"image"` // nil keeps the current image; "" switches to the official one
		MemLimit string  `json:"memLimit"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Exposure == "" {
		req.Exposure = d.Exposure
	}
	if req.Port == 0 {
		req.Port = d.Port
	}
	if req.Version == "" {
		req.Version = d.Version
	}
	eng, _ := dbmgr.EngineByID(d.Engine)
	if req.Version != d.Version && !contains(eng.Versions, req.Version) {
		return bad("不支持的版本")
	}
	if d.Engine == "postgres" && pgMajor(req.Version) != pgMajor(d.Version) {
		return bad("PostgreSQL 大版本不能原地升级（数据目录格式不兼容）。请新建 %s 实例，再用备份导出/导入迁移数据", req.Version)
	}
	image := d.Image
	if req.Image != nil {
		image = *req.Image
	}
	if err := checkDBImage(d.Engine, req.Version, &image); err != nil {
		return err
	}
	if req.MemLimit = strings.TrimSpace(req.MemLimit); req.MemLimit == "" {
		// Blank means "keep": carry over the limit from the current compose file.
		if p, err := s.getProject(d.ProjectID); err == nil {
			if m := memLimitLineRe.FindStringSubmatch(p.Compose); m != nil {
				req.MemLimit = m[1]
			}
		}
	} else if !memLimitRe.MatchString(req.MemLimit) {
		return bad("内存限制格式如 512m、1g")
	}
	compose, err := dbmgr.Compose(d.Engine, req.Version, image, d.Name, req.Exposure, s.cfg.DBNetwork, req.Port, req.MemLimit)
	if err != nil {
		return err
	}
	_, _ = s.db.Exec(`UPDATE db_instances SET exposure=?, port=?, version=?, image=? WHERE id=?`, req.Exposure, req.Port, req.Version, image, d.ID)
	_, _ = s.db.Exec(`UPDATE projects SET compose=?, description=?, updated_at=? WHERE id=?`, compose, d.Engine+" "+req.Version, time.Now().Unix(), d.ProjectID)
	p, _ := s.getProject(d.ProjectID)
	jobID, _ := s.startDeploy(p, "manual", u.Username, 0)
	s.audit(r, u, "database.update", d.Name, fmt.Sprintf("%s · %s:%d", req.Version, req.Exposure, req.Port), nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) handleDeleteDatabase(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	if r.URL.Query().Get("confirm") != d.Name {
		return bad("请输入实例名确认删除")
	}
	p, err := s.getProject(d.ProjectID)
	if err != nil {
		return err
	}
	jobID := s.deleteProjectJob(p, u.Username, r.URL.Query().Get("volumes") == "1", true)
	s.audit(r, u, "database.delete", d.Name, "", nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) dbCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 60*time.Second)
}

func (s *Server) handleDBList(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	list, err := d.client().ListDatabases(ctx)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (s *Server) handleDBCreate(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Name string `json:"name"`
		User string `json:"user"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	pw := ""
	if req.User != "" {
		pw = auth.RandomPassword(24)
	}
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	err = d.client().CreateDatabase(ctx, req.Name, req.User, pw)
	s.audit(r, u, "database.create_db", d.Name+"/"+req.Name, req.User, err)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"user": req.User, "password": pw})
}

func (s *Server) handleDBDrop(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	name := r.PathValue("name")
	if r.URL.Query().Get("confirm") != name {
		return bad("请输入库名确认删除")
	}
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	err = d.client().DropDatabase(ctx, name)
	s.audit(r, u, "database.drop_db", d.Name+"/"+name, "", err)
	return done(w, err)
}

func (s *Server) handleDBUsers(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	list, err := d.client().ListUsers(ctx)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (s *Server) handleDBCreateUser(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Name      string   `json:"name"`
		Databases []string `json:"databases"`
		ReadOnly  bool     `json:"readOnly"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	pw := auth.RandomPassword(24)
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	err = d.client().CreateUser(ctx, req.Name, pw, req.Databases, req.ReadOnly)
	s.audit(r, u, "database.create_user", d.Name+"/"+req.Name, strings.Join(req.Databases, ","), err)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"user": req.Name, "password": pw})
}

func (s *Server) handleDBUserPassword(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	name := r.PathValue("name")
	if name == d.RootUser {
		return bad("root 密码由面板管理，请勿在此修改")
	}
	pw := auth.RandomPassword(24)
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	err = d.client().SetPassword(ctx, name, r.URL.Query().Get("host"), pw)
	s.audit(r, u, "database.reset_password", d.Name+"/"+name, "", err)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"user": name, "password": pw})
}

func (s *Server) handleDBDropUser(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	name := r.PathValue("name")
	if name == d.RootUser {
		return bad("不能删除 root 用户")
	}
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	err = d.client().DropUser(ctx, name, r.URL.Query().Get("host"))
	s.audit(r, u, "database.drop_user", d.Name+"/"+name, "", err)
	return done(w, err)
}

func (s *Server) handleDBSessions(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	list, err := d.client().Sessions(ctx)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (s *Server) handleDBKill(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	pid := r.PathValue("pid")
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	err = d.client().KillSession(ctx, pid)
	s.audit(r, u, "database.kill_session", d.Name, "pid "+pid, err)
	return done(w, err)
}

func (s *Server) handleDBTables(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	db := r.URL.Query().Get("db")
	if !dbmgr.ValidIdent(db) {
		return bad("库名无效")
	}
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	list, err := d.client().Tables(ctx, db)
	if err != nil {
		return err
	}
	return ok(w, list)
}

func (s *Server) handleDBQuery(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Database string `json:"database"`
		SQL      string `json:"sql"`
		ReadOnly bool   `json:"readOnly"`
		Confirm  bool   `json:"confirm"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if !dbmgr.ValidIdent(req.Database) {
		return bad("请选择数据库")
	}
	if strings.TrimSpace(req.SQL) == "" {
		return bad("SQL 不能为空")
	}
	// Viewers may only run read-only queries.
	if !u.can(roleOperator) {
		req.ReadOnly = true
	}
	if !req.ReadOnly && !req.Confirm {
		if reasons := dbmgr.Danger(req.SQL); len(reasons) > 0 {
			return ok(w, map[string]any{"needConfirm": true, "reasons": reasons})
		}
	}
	const timeoutSec = 30
	ctx, cancel := context.WithTimeout(r.Context(), (timeoutSec+5)*time.Second)
	defer cancel()
	start := time.Now()
	res, truncated, err := d.client().Console(ctx, req.Database, req.SQL, req.ReadOnly, 1000, timeoutSec)
	mode := "读写"
	if req.ReadOnly {
		mode = "只读"
	} else if req.Confirm {
		mode = "读写（已确认危险语句）"
	}
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("查询超时（%d 秒）", timeoutSec)
	}
	s.audit(r, u, "database.query", d.Name+"/"+req.Database, mode+" · "+truncate(strings.Join(strings.Fields(req.SQL), " "), 300), err)
	if err != nil {
		return ok(w, map[string]any{"error": err.Error(), "elapsedMs": time.Since(start).Milliseconds()})
	}
	return ok(w, map[string]any{"columns": res.Columns, "rows": res.Rows, "truncated": truncated, "elapsedMs": time.Since(start).Milliseconds()})
}

var (
	memLimitRe     = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[bkmgBKMG]?$`)
	memLimitLineRe = regexp.MustCompile(`(?m)^    mem_limit: *(\S+)`)
)

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func pgMajor(v string) string { return strings.SplitN(v, ".", 2)[0] }

// checkDBImage normalises a custom image and, for PostgreSQL, rejects one whose tag
// names a different major version than the instance (the data layout depends on it).
func checkDBImage(engine, version string, image *string) error {
	*image = strings.TrimSpace(*image)
	if *image == "" {
		return nil
	}
	if !dbmgr.ValidImage(*image) {
		return bad("镜像名无效，格式如 pgvector/pgvector:pg17 或 registry.example.com/team/postgres:17")
	}
	if eng, ok := dbmgr.EngineByID(engine); ok && *image == eng.DefaultImage(version) {
		*image = "" // same as the official one: keep following the version
		return nil
	}
	if engine == "postgres" {
		if m := dbmgr.PostgresMajorFromImage(*image); m != "" && m != pgMajor(version) {
			return bad("镜像 %s 看起来是 PostgreSQL %s，与所选版本 %s 不一致", *image, m, version)
		}
	}
	return nil
}
