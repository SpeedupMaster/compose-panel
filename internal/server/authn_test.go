package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"compose-panel/internal/auth"
)

func seedProject(t *testing.T, srv *Server, name, kind string) int64 {
	t.Helper()
	res, err := srv.db.Exec(`INSERT INTO projects(name,kind,source,created_at,updated_at) VALUES(?,?,'inline',1000,1000)`, name, kind)
	if err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedDBInstance(t *testing.T, srv *Server, projectID int64) int64 {
	t.Helper()
	res, err := srv.db.Exec(`INSERT INTO db_instances(project_id,engine,version,root_user,root_password,port,exposure,created_at) VALUES(?,'postgres','16','postgres',?,5432,'internal',1000)`,
		projectID, srv.box.Seal("secretpw"))
	if err != nil {
		t.Fatalf("seed db_instance: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedUserWithToken(t *testing.T, srv *Server, username, role string, allProjects bool, projects []int64, scope string) (int64, string) {
	t.Helper()
	hash, _ := auth.HashPassword("password-123456")
	res, err := srv.db.Exec(`INSERT INTO users(username,email,password_hash,role,all_projects,created_at) VALUES(?,'',?,?,?,1000)`,
		username, hash, role, boolInt(allProjects))
	if err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	uid, _ := res.LastInsertId()
	for _, pid := range projects {
		if _, err := srv.db.Exec(`INSERT INTO user_projects(user_id,project_id) VALUES(?,?)`, uid, pid); err != nil {
			t.Fatalf("seed user_projects: %v", err)
		}
	}
	if scope == "" {
		return uid, ""
	}
	tok := "cpt_" + auth.RandomToken(24)
	_, err = srv.db.Exec(`INSERT INTO api_tokens(name,prefix,token_hash,user_id,scope,created_at) VALUES('test',?,?,?,?,1000)`,
		tok[:10], auth.SHA256Hex(tok), uid, scope)
	if err != nil {
		t.Fatalf("seed api_token: %v", err)
	}
	return uid, tok
}

func TestRBACZeroProjectsFailClosedAndCascade(t *testing.T) {
	srv := newTestServer(t)
	p1 := seedProject(t, srv, "alpha", "app")
	p2 := seedProject(t, srv, "beta", "app")
	admin := &User{ID: 999, Username: "admin", Role: "admin", Level: roleAdmin}

	// 1. Create restricted user with 0 projects (projects: []).
	req := httptest.NewRequest(http.MethodPost, "/api/users", strings.NewReader(`{"username":"zero","role":"operator","projects":[]}`))
	w := httptest.NewRecorder()
	if err := srv.handleCreateUser(w, req, admin); err != nil {
		t.Fatalf("handleCreateUser(zero): %v", err)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	uZero, err := srv.loadUser(created.ID)
	if err != nil {
		t.Fatalf("loadUser(zero): %v", err)
	}
	if uZero.Projects == nil || len(uZero.Projects) != 0 {
		t.Fatalf("expected non-nil empty Projects slice, got %#v", uZero.Projects)
	}
	if uZero.canProject(p1) || uZero.canProject(p2) {
		t.Fatalf("0-project user MUST NOT access any project")
	}

	// 2. Create unrestricted user (projects: null).
	reqAll := httptest.NewRequest(http.MethodPost, "/api/users", strings.NewReader(`{"username":"alluser","role":"operator","projects":null}`))
	wAll := httptest.NewRecorder()
	if err := srv.handleCreateUser(wAll, reqAll, admin); err != nil {
		t.Fatalf("handleCreateUser(alluser): %v", err)
	}
	var createdAll struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(wAll.Body.Bytes(), &createdAll)
	uAll, err := srv.loadUser(createdAll.ID)
	if err != nil {
		t.Fatalf("loadUser(alluser): %v", err)
	}
	if uAll.Projects != nil || !uAll.canProject(p1) || !uAll.canProject(p2) {
		t.Fatalf("all_projects=1 user should have nil Projects and access all projects, got %#v", uAll.Projects)
	}

	// 3. Assign p1 to restricted user, then delete p1 (ON DELETE CASCADE) -> must NOT escalate!
	reqUpd := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/users/%d", created.ID),
		strings.NewReader(fmt.Sprintf(`{"role":"operator","projects":[%d]}`, p1)))
	reqUpd.SetPathValue("id", fmt.Sprint(created.ID))
	if err := srv.handleUpdateUser(httptest.NewRecorder(), reqUpd, admin); err != nil {
		t.Fatalf("handleUpdateUser([p1]): %v", err)
	}
	uOne, err := srv.loadUser(created.ID)
	if err != nil || !uOne.canProject(p1) || uOne.canProject(p2) {
		t.Fatalf("expected access only to p1, got err=%v projects=%#v", err, uOne.Projects)
	}

	if _, err := srv.db.Exec(`DELETE FROM projects WHERE id=?`, p1); err != nil {
		t.Fatalf("delete p1: %v", err)
	}
	uAfterDel, err := srv.loadUser(created.ID)
	if err != nil {
		t.Fatalf("loadUser after cascade delete: %v", err)
	}
	if uAfterDel.Projects == nil || len(uAfterDel.Projects) != 0 || uAfterDel.canProject(p2) {
		t.Fatalf("CASCADE delete of last project must NOT escalate user to global access: projects=%#v", uAfterDel.Projects)
	}

	// 4. handleListUsers returns null for all_projects=1 and [] for all_projects=0.
	wList := httptest.NewRecorder()
	if err := srv.handleListUsers(wList, httptest.NewRequest(http.MethodGet, "/api/users", nil), admin); err != nil {
		t.Fatalf("handleListUsers: %v", err)
	}
	var listed []struct {
		Username string          `json:"username"`
		Projects json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal users: %v", err)
	}
	byName := map[string]string{}
	for _, item := range listed {
		byName[item.Username] = strings.TrimSpace(string(item.Projects))
	}
	if byName["zero"] != "[]" {
		t.Errorf("expected zero user projects='[]', got %q", byName["zero"])
	}
	if byName["alluser"] != "null" {
		t.Errorf("expected alluser projects='null', got %q", byName["alluser"])
	}

	// 5. Fail-closed if user_projects query fails.
	if _, err := srv.db.Exec(`DROP TABLE user_projects`); err != nil {
		t.Fatalf("drop user_projects: %v", err)
	}
	if _, err := srv.loadUser(created.ID); err == nil {
		t.Fatalf("expected loadUser to fail closed when user_projects query errors")
	}
}

func TestDeployTokenScopeAndTokenCreationCaps(t *testing.T) {
	srv := newTestServer(t)
	p1 := seedProject(t, srv, "web", "app")
	_, deployTok := seedUserWithToken(t, srv, "ci-admin", "admin", true, nil, "deploy")
	h := srv.Handler()

	call := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+deployTok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// Allowed endpoints must NOT return 403 deploy-scope error.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/auth/me"},
		{http.MethodGet, "/api/projects"},
		{http.MethodGet, fmt.Sprintf("/api/projects/%d", p1)},
		{http.MethodGet, fmt.Sprintf("/api/projects/%d/deployments", p1)},
		{http.MethodGet, fmt.Sprintf("/api/projects/%d/images", p1)},
		{http.MethodGet, "/api/deployments/999"},
		{http.MethodGet, "/api/jobs/nonexistent"},
		{http.MethodGet, "/api/jobs/nonexistent/log"},
	} {
		w := call(tc.method, tc.path)
		if w.Code == http.StatusForbidden {
			t.Errorf("deploy token should be allowed on %s %s, got 403: %s", tc.method, tc.path, w.Body.String())
		}
	}

	// Disallowed endpoints MUST return 403 with deploy-scope message.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/databases"},
		{http.MethodGet, "/api/sites"},
		{http.MethodPost, "/api/sites"},
		{http.MethodGet, "/api/backups"},
		{http.MethodGet, "/api/backup-policies"},
		{http.MethodPost, "/api/backup-policies"},
		{http.MethodGet, "/api/users"},
		{http.MethodGet, "/api/settings"},
		{http.MethodGet, "/api/containers/abc/terminal"},
		{http.MethodPost, "/api/jobs/abc/cancel"},
		{http.MethodPut, fmt.Sprintf("/api/projects/%d", p1)},
		{http.MethodDelete, fmt.Sprintf("/api/projects/%d", p1)},
	} {
		w := call(tc.method, tc.path)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "deploy 令牌权限范围") {
			t.Errorf("deploy token on %s %s: got %d %s, want 403 deploy scope error", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	// Token creation scope caps by role.
	viewer := &User{ID: 10, Username: "v", Role: "viewer", Level: roleViewer}
	operator := &User{ID: 11, Username: "o", Role: "operator", Level: roleOperator}
	admin := &User{ID: 12, Username: "a", Role: "admin", Level: roleAdmin}
	seedUserWithToken(t, srv, "v", "viewer", true, nil, "")
	seedUserWithToken(t, srv, "o", "operator", true, nil, "")
	seedUserWithToken(t, srv, "a", "admin", true, nil, "")
	// Re-fetch actual IDs
	_ = srv.db.QueryRow(`SELECT id FROM users WHERE username='v'`).Scan(&viewer.ID)
	_ = srv.db.QueryRow(`SELECT id FROM users WHERE username='o'`).Scan(&operator.ID)
	_ = srv.db.QueryRow(`SELECT id FROM users WHERE username='a'`).Scan(&admin.ID)

	createTok := func(u *User, scope string) error {
		req := httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(fmt.Sprintf(`{"name":"t","scope":%q}`, scope)))
		return srv.handleCreateToken(httptest.NewRecorder(), req, u)
	}
	if err := createTok(viewer, "read"); err != nil {
		t.Errorf("viewer create read token: %v", err)
	}
	if err := createTok(viewer, "deploy"); err == nil {
		t.Errorf("viewer must NOT create deploy token")
	}
	if err := createTok(viewer, "full"); err == nil {
		t.Errorf("viewer must NOT create full token")
	}
	if err := createTok(operator, "deploy"); err != nil {
		t.Errorf("operator create deploy token: %v", err)
	}
	if err := createTok(operator, "full"); err == nil {
		t.Errorf("operator must NOT create full token")
	}
	if err := createTok(admin, "full"); err != nil {
		t.Errorf("admin create full token: %v", err)
	}
}

func TestSitesRBAC(t *testing.T) {
	srv := newTestServer(t)
	p1 := seedProject(t, srv, "p1", "app")
	p2 := seedProject(t, srv, "p2", "app")
	op1 := &User{ID: 2, Username: "op1", Role: "operator", Level: roleOperator, Projects: []int64{p1}}
	opAll := &User{ID: 3, Username: "opall", Role: "operator", Level: roleOperator, Projects: nil}
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	// Seed site for p2 and custom-upstream site (project_id IS NULL) and empty-domains edge row.
	res2, _ := srv.db.Exec(`INSERT INTO sites(domains,kind,project_id,service,port,enabled,created_at) VALUES('p2.example.com','proxy',?,'web',80,1,1000)`, p2)
	s2, _ := res2.LastInsertId()
	resCustom, _ := srv.db.Exec(`INSERT INTO sites(domains,kind,project_id,upstream,enabled,created_at) VALUES('custom.example.com','proxy',NULL,'127.0.0.1:9000',1,1000)`)
	sCustom, _ := resCustom.LastInsertId()
	_, _ = srv.db.Exec(`INSERT INTO sites(domains,kind,project_id,service,port,enabled,created_at) VALUES('','proxy',?,'web',80,1,1000)`, p1)

	// Restricted operator cannot delete p2's site or custom site.
	delReq := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/sites/%d", s2), nil)
	delReq.SetPathValue("id", fmt.Sprint(s2))
	if err := srv.handleDeleteSite(httptest.NewRecorder(), delReq, op1); err == nil {
		t.Fatalf("op1 must NOT delete p2's site")
	}
	delCustom := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/sites/%d", sCustom), nil)
	delCustom.SetPathValue("id", fmt.Sprint(sCustom))
	if err := srv.handleDeleteSite(httptest.NewRecorder(), delCustom, opAll); err == nil {
		t.Fatalf("non-admin operator must NOT delete projectId=0 custom site")
	}

	// Non-admin cannot create or update projectId=0 or negative projectId custom upstream site.
	saveCustom := httptest.NewRequest(http.MethodPost, "/api/sites", strings.NewReader(`{"domains":["evil.example.com"],"kind":"proxy","projectId":0,"upstream":"panel:7800","enabled":true}`))
	if err := srv.handleSaveSite(httptest.NewRecorder(), saveCustom, opAll); err == nil {
		t.Fatalf("non-admin operator must NOT create projectId=0 site")
	}
	saveNeg := httptest.NewRequest(http.MethodPost, "/api/sites", strings.NewReader(`{"domains":["evil-neg.example.com"],"kind":"proxy","projectId":-1,"upstream":"panel:7800","enabled":true}`))
	if err := srv.handleSaveSite(httptest.NewRecorder(), saveNeg, opAll); err == nil {
		t.Fatalf("non-admin operator must NOT create negative projectId custom site")
	}

	// Restricted operator cannot hijack p2's site via PUT /api/sites/{id}.
	hijack := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/sites/%d", s2),
		strings.NewReader(fmt.Sprintf(`{"domains":["p2.example.com"],"kind":"proxy","projectId":%d,"service":"web","port":80,"enabled":true}`, p1)))
	hijack.SetPathValue("id", fmt.Sprint(s2))
	if err := srv.handleSaveSite(httptest.NewRecorder(), hijack, op1); err == nil {
		t.Fatalf("op1 must NOT modify p2's existing site")
	}

	// Admin CAN create custom site and delete it.
	saveAdmin := httptest.NewRequest(http.MethodPost, "/api/sites", strings.NewReader(`{"domains":["ok.example.com"],"kind":"proxy","projectId":0,"upstream":"127.0.0.1:8080","enabled":true}`))
	if err := srv.handleSaveSite(httptest.NewRecorder(), saveAdmin, admin); err != nil {
		t.Fatalf("admin save custom site failed: %v", err)
	}

	// handleListSites must not panic when a site has empty domains, and must filter p2 & custom for op1.
	wList := httptest.NewRecorder()
	if err := srv.handleListSites(wList, httptest.NewRequest(http.MethodGet, "/api/sites", nil), op1); err != nil {
		t.Fatalf("handleListSites(op1): %v", err)
	}
	if strings.Contains(wList.Body.String(), "p2.example.com") || strings.Contains(wList.Body.String(), "custom.example.com") {
		t.Fatalf("op1 should not see p2 or custom site: %s", wList.Body.String())
	}
}

func TestBackupsDrillsJobsAndTemplatesRBAC(t *testing.T) {
	srv := newTestServer(t)
	p1 := seedProject(t, srv, "proj1", "app")
	p2 := seedProject(t, srv, "proj2", "app")
	db1Proj := seedProject(t, srv, "db1", "database")
	db2Proj := seedProject(t, srv, "db2", "database")
	db1 := seedDBInstance(t, srv, db1Proj)
	db2 := seedDBInstance(t, srv, db2Proj)

	op1 := &User{ID: 2, Username: "op1", Role: "operator", Level: roleOperator, Projects: []int64{p1, db1Proj}}
	opAll := &User{ID: 3, Username: "opall", Role: "operator", Level: roleOperator, Projects: nil}
	viewer1 := &User{ID: 4, Username: "v1", Role: "viewer", Level: roleViewer, Projects: []int64{p1, db1Proj}}
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	var sid int64
	_ = srv.db.QueryRow(`SELECT id FROM storage_targets ORDER BY id LIMIT 1`).Scan(&sid)

	// Seed policies for p1, p2, db2, panel.
	insPol := func(name string, src BackupSource) int64 {
		b, _ := json.Marshal(src)
		r, err := srv.db.Exec(`INSERT INTO backup_policies(name,source,schedule,keep_daily,storage_id,enabled,created_at) VALUES(?,?, '0 3 * * *', 7, ?, 1, 1000)`, name, string(b), sid)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		return id
	}
	pol1 := insPol("pol-p1", BackupSource{Kind: "dir", ProjectID: p1, Path: "data"})
	pol2 := insPol("pol-p2", BackupSource{Kind: "dir", ProjectID: p2, Path: "data"})
	polDB2 := insPol("pol-db2", BackupSource{Kind: "database", InstanceID: db2, Database: "app"})
	polPanel := insPol("pol-panel", BackupSource{Kind: "panel"})

	// Seed backups for each policy.
	now := time.Now().Unix()
	insBak := func(polID int64, src BackupSource, label, status string) int64 {
		b, _ := json.Marshal(src)
		r, err := srv.db.Exec(`INSERT INTO backups(policy_id,source,label,storage_id,status,started_at) VALUES(?,?,?,?,?,?)`, polID, string(b), label, sid, status, now)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		return id
	}
	b1 := insBak(pol1, BackupSource{Kind: "dir", ProjectID: p1, Path: "data"}, "proj1/data", "failed")
	b2 := insBak(pol2, BackupSource{Kind: "dir", ProjectID: p2, Path: "data"}, "proj2/data", "failed")
	bPanel := insBak(polPanel, BackupSource{Kind: "panel"}, "panel", "failed")
	_ = b1
	_ = polDB2

	// 1. handleListPolicies filtered for op1.
	wPol := httptest.NewRecorder()
	if err := srv.handleListPolicies(wPol, httptest.NewRequest(http.MethodGet, "/api/backup-policies", nil), op1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wPol.Body.String(), "pol-p1") || strings.Contains(wPol.Body.String(), "pol-p2") || strings.Contains(wPol.Body.String(), "pol-db2") || strings.Contains(wPol.Body.String(), "pol-panel") {
		t.Fatalf("unexpected policies for op1: %s", wPol.Body.String())
	}

	// 2. handleCreatePolicy / handleUpdatePolicy / handleRunPolicy / handleBackupNow / handleVerifyBackup denied for p2 & panel.
	for _, body := range []string{
		fmt.Sprintf(`{"name":"x","source":{"kind":"dir","projectId":%d,"path":"data"},"schedule":"0 3 * * *","keepDaily":1,"storageId":%d}`, p2, sid),
		fmt.Sprintf(`{"name":"x","source":{"kind":"database","instanceId":%d,"database":"app"},"schedule":"0 3 * * *","keepDaily":1,"storageId":%d}`, db2, sid),
		fmt.Sprintf(`{"name":"x","source":{"kind":"panel"},"schedule":"0 3 * * *","keepDaily":1,"storageId":%d}`, sid),
		fmt.Sprintf(`{"name":"x","source":{"kind":"volume","volume":"unowned_vol"},"schedule":"0 3 * * *","keepDaily":1,"storageId":%d}`, sid),
	} {
		if err := srv.handleCreatePolicy(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/backup-policies", strings.NewReader(body)), op1); err == nil {
			t.Errorf("op1 must NOT create policy for %s", body)
		}
	}
	updReq := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/backup-policies/%d", pol2),
		strings.NewReader(fmt.Sprintf(`{"name":"x","source":{"kind":"dir","projectId":%d,"path":"data"},"schedule":"0 3 * * *","keepDaily":1,"storageId":%d}`, p1, sid)))
	updReq.SetPathValue("id", fmt.Sprint(pol2))
	if err := srv.handleUpdatePolicy(httptest.NewRecorder(), updReq, op1); err == nil {
		t.Errorf("op1 must NOT update pol2")
	}
	runPolReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backup-policies/%d/run", pol2), nil)
	runPolReq.SetPathValue("id", fmt.Sprint(pol2))
	if err := srv.handleRunPolicy(httptest.NewRecorder(), runPolReq, op1); err == nil {
		t.Errorf("op1 must NOT run pol2")
	}
	if err := srv.handleBackupNow(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/backups/run",
		strings.NewReader(fmt.Sprintf(`{"source":{"kind":"panel"},"storageId":%d}`, sid))), op1); err == nil {
		t.Errorf("op1 must NOT run ad-hoc panel backup")
	}
	verReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backups/%d/verify", b2), strings.NewReader(`{}`))
	verReq.SetPathValue("id", fmt.Sprint(b2))
	if err := srv.handleVerifyBackup(httptest.NewRecorder(), verReq, op1); err == nil {
		t.Errorf("op1 must NOT verify p2 backup")
	}
	verPanelReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/backups/%d/verify", bPanel), strings.NewReader(`{}`))
	verPanelReq.SetPathValue("id", fmt.Sprint(bPanel))
	if err := srv.handleVerifyBackup(httptest.NewRecorder(), verPanelReq, op1); err == nil {
		t.Errorf("op1 must NOT verify panel backup")
	}

	// 3. handleListBackups & handleBackupSources filtered for op1.
	wBak := httptest.NewRecorder()
	if err := srv.handleListBackups(wBak, httptest.NewRequest(http.MethodGet, "/api/backups", nil), op1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wBak.Body.String(), "proj1/data") || strings.Contains(wBak.Body.String(), "proj2/data") || strings.Contains(wBak.Body.String(), `"panel"`) {
		t.Fatalf("unexpected backups for op1: %s", wBak.Body.String())
	}
	wSrc := httptest.NewRecorder()
	if err := srv.handleBackupSources(wSrc, httptest.NewRequest(http.MethodGet, "/api/backup-sources", nil), op1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wSrc.Body.String(), "proj1") || strings.Contains(wSrc.Body.String(), "proj2") || strings.Contains(wSrc.Body.String(), "db2") || !strings.Contains(wSrc.Body.String(), fmt.Sprintf(`"id":%d`, db1)) {
		t.Fatalf("unexpected backup sources for op1: %s", wSrc.Body.String())
	}

	// 4. Drills RBAC: admin-only write/delete/run, filtered list.
	saveDrillBody := fmt.Sprintf(`{"policyId":%d,"schedule":"0 4 * * 0","checkSql":"SELECT 1","minRows":1,"enabled":true}`, pol1)
	if err := srv.handleSaveDrill(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/restore-drills", strings.NewReader(saveDrillBody)), opAll); err == nil {
		t.Fatalf("operator must NOT save restore drill")
	}
	wDrill := httptest.NewRecorder()
	if err := srv.handleSaveDrill(wDrill, httptest.NewRequest(http.MethodPost, "/api/restore-drills", strings.NewReader(saveDrillBody)), admin); err != nil {
		t.Fatalf("admin save drill1: %v", err)
	}
	var d1 struct{ ID int64 `json:"id"` }
	_ = json.Unmarshal(wDrill.Body.Bytes(), &d1)

	wDrill2 := httptest.NewRecorder()
	if err := srv.handleSaveDrill(wDrill2, httptest.NewRequest(http.MethodPost, "/api/restore-drills",
		strings.NewReader(fmt.Sprintf(`{"policyId":%d,"schedule":"0 4 * * 0","enabled":true}`, pol2))), admin); err != nil {
		t.Fatalf("admin save drill2: %v", err)
	}
	var d2 struct{ ID int64 `json:"id"` }
	_ = json.Unmarshal(wDrill2.Body.Bytes(), &d2)

	delDrillReq := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/restore-drills/%d", d1.ID), nil)
	delDrillReq.SetPathValue("id", fmt.Sprint(d1.ID))
	if err := srv.handleDeleteDrill(httptest.NewRecorder(), delDrillReq, opAll); err == nil {
		t.Fatalf("operator must NOT delete drill")
	}
	runDrillReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/restore-drills/%d/run", d1.ID), nil)
	runDrillReq.SetPathValue("id", fmt.Sprint(d1.ID))
	if err := srv.handleRunDrill(httptest.NewRecorder(), runDrillReq, opAll); err == nil {
		t.Fatalf("operator must NOT run drill")
	}

	wListDrills := httptest.NewRecorder()
	if err := srv.handleListDrills(wListDrills, httptest.NewRequest(http.MethodGet, "/api/restore-drills", nil), viewer1); err != nil {
		t.Fatalf("handleListDrills(viewer1): %v", err)
	}
	if !strings.Contains(wListDrills.Body.String(), "pol-p1") || strings.Contains(wListDrills.Body.String(), "pol-p2") {
		t.Fatalf("viewer1 should only see pol-p1 drill: %s", wListDrills.Body.String())
	}

	// 5. Jobs, Schedules & Overview RBAC.
	_, _ = srv.db.Exec(`INSERT INTO jobs(id,kind,title,target,status,created_at) VALUES
		('j-p1','deploy','deploy proj1','proj1','success',?),
		('j-p2','deploy','deploy proj2','proj2','running',?),
		('j-sys','panel.backup','panel backup','panel.db','success',?)`, now, now, now)

	wJobs := httptest.NewRecorder()
	if err := srv.handleListJobs(wJobs, httptest.NewRequest(http.MethodGet, "/api/jobs?page=1&pageSize=20", nil), viewer1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wJobs.Body.String(), "j-p1") || strings.Contains(wJobs.Body.String(), "j-p2") || strings.Contains(wJobs.Body.String(), "j-sys") {
		t.Fatalf("viewer1 jobs leak: %s", wJobs.Body.String())
	}
	getJobP2 := httptest.NewRequest(http.MethodGet, "/api/jobs/j-p2", nil)
	getJobP2.SetPathValue("id", "j-p2")
	if err := srv.handleGetJob(httptest.NewRecorder(), getJobP2, viewer1); err == nil {
		t.Fatalf("viewer1 must NOT get j-p2")
	}
	logJobSys := httptest.NewRequest(http.MethodGet, "/api/jobs/j-sys/log", nil)
	logJobSys.SetPathValue("id", "j-sys")
	if err := srv.handleJobLog(httptest.NewRecorder(), logJobSys, viewer1); err == nil {
		t.Fatalf("viewer1 must NOT read j-sys log")
	}
	cancelJobP2 := httptest.NewRequest(http.MethodPost, "/api/jobs/j-p2/cancel", nil)
	cancelJobP2.SetPathValue("id", "j-p2")
	if err := srv.handleCancelJob(httptest.NewRecorder(), cancelJobP2, op1); err == nil {
		t.Fatalf("op1 must NOT cancel j-p2")
	}
	schedReq := httptest.NewRequest(http.MethodPost, "/api/schedules/system:cert-check/run", nil)
	schedReq.SetPathValue("key", "system:cert-check")
	if err := srv.handleRunSchedule(httptest.NewRecorder(), schedReq, op1); err == nil {
		t.Fatalf("restricted op1 must NOT run schedules")
	}
	schedDrillReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/schedules/drill:%d/run", d1.ID), nil)
	schedDrillReq.SetPathValue("key", fmt.Sprintf("drill:%d", d1.ID))
	if err := srv.handleRunSchedule(httptest.NewRecorder(), schedDrillReq, opAll); err == nil {
		t.Fatalf("non-admin operator must NOT run drill schedule")
	}

	wOver := httptest.NewRecorder()
	if err := srv.handleOverview(wOver, httptest.NewRequest(http.MethodGet, "/api/overview", nil), viewer1); err != nil {
		t.Fatal(err)
	}
	overBody := wOver.Body.String()
	if strings.Contains(overBody, "proj2") || strings.Contains(overBody, "j-p2") || strings.Contains(overBody, "j-sys") {
		t.Fatalf("overview leaked unauthorized data to viewer1: %s", overBody)
	}

	// 6. Templates RBAC: viewer secret redaction & admin-only delete.
	envJSON, _ := json.Marshal([]EnvVar{{Key: "API_SECRET", Value: "super-secret-value", Secret: true}, {Key: "PUBLIC_URL", Value: "https://a.test", Secret: false}})
	resTpl, err := srv.db.Exec(`INSERT INTO templates(name,compose,env,created_by,created_at) VALUES('custom-sec','services:\n  web:\n    image: nginx',?,'admin',1000)`, srv.box.Seal(string(envJSON)))
	if err != nil {
		t.Fatal(err)
	}
	tplID, _ := resTpl.LastInsertId()

	wTplV := httptest.NewRecorder()
	if err := srv.handleTemplates(wTplV, httptest.NewRequest(http.MethodGet, "/api/templates", nil), viewer1); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wTplV.Body.String(), "super-secret-value") {
		t.Fatalf("viewer MUST NOT see template secret values")
	}
	var tpls []template
	if err := json.Unmarshal(wTplV.Body.Bytes(), &tpls); err != nil {
		t.Fatal(err)
	}
	for _, tpl := range tpls {
		for _, ev := range tpl.Env {
			if ev.Secret && ev.Value != "" {
				t.Errorf("viewer saw non-empty secret %s in template %s", ev.Key, tpl.ID)
			}
		}
	}

	wTplO := httptest.NewRecorder()
	if err := srv.handleTemplates(wTplO, httptest.NewRequest(http.MethodGet, "/api/templates", nil), opAll); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wTplO.Body.String(), "super-secret-value") {
		t.Fatalf("operator should see custom template secret value")
	}

	delTplReq := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/templates/%d", tplID), nil)
	delTplReq.SetPathValue("id", fmt.Sprint(tplID))
	if err := srv.handleDeleteTemplate(httptest.NewRecorder(), delTplReq, opAll); err == nil {
		t.Fatalf("non-admin operator must NOT delete template")
	}
	if err := srv.handleDeleteTemplate(httptest.NewRecorder(), delTplReq, admin); err != nil {
		t.Fatalf("admin delete template failed: %v", err)
	}
}

func TestClientIPAntiSpoofing(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        []string
		xri        string
		want       string
	}{
		{
			name:       "direct public port ignores spoofed XFF and X-Real-IP",
			remoteAddr: "203.0.113.50:43210",
			xff:        []string{"1.2.3.4"},
			xri:        "5.6.7.8",
			want:       "203.0.113.50",
		},
		{
			name:       "behind Caddy Docker bridge uses rightmost real peer IP ignoring client spoofed prefix",
			remoteAddr: "172.18.0.2:50000",
			xff:        []string{"1.2.3.4, 198.51.100.77"},
			want:       "198.51.100.77",
		},
		{
			name:       "behind proxy with multiple X-Forwarded-For headers uses last header rightmost IP",
			remoteAddr: "172.18.0.2:50000",
			xff:        []string{"1.2.3.4", "198.51.100.66"},
			want:       "198.51.100.66",
		},
		{
			name:       "behind loopback proxy skips trailing loopback and picks real client",
			remoteAddr: "127.0.0.1:50000",
			xff:        []string{"9.9.9.9, 198.51.100.88, 127.0.0.1"},
			want:       "198.51.100.88",
		},
		{
			name:       "behind loopback proxy with only loopback XFF returns loopback",
			remoteAddr: "127.0.0.1:50000",
			xff:        []string{"127.0.0.1"},
			want:       "127.0.0.1",
		},
		{
			name:       "behind unix socket uses X-Real-IP when XFF absent",
			remoteAddr: "@",
			xri:        "198.51.100.99",
			want:       "198.51.100.99",
		},
		{
			name:       "behind trusted proxy ignores garbage XFF and falls back to remote",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"not-an-ip"},
			want:       "10.0.0.2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			for _, h := range tc.xff {
				req.Header.Add("X-Forwarded-For", h)
			}
			if tc.xri != "" {
				req.Header.Set("X-Real-IP", tc.xri)
			}
			if got := clientIP(req); got != tc.want {
				t.Errorf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetupRateLimitAndTOTPProtectedFromEviction(t *testing.T) {
	srv := newTestServer(t)

	// 1. handleSetup rate-limits after 5 bad token attempts from the same IP.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewBufferString(`{"token":"bad","username":"admin","password":"password1234"}`))
		req.RemoteAddr = "203.0.113.20:1000"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("1.1.1.%d", i)) // spoofed XFF must be ignored
		err := srv.handleSetup(httptest.NewRecorder(), req)
		if ae, ok := err.(*apiError); !ok || ae.Status != 403 {
			t.Fatalf("attempt %d: expected 403, got %v", i, err)
		}
	}
	req6 := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewBufferString(`{"token":"bad","username":"admin","password":"password1234"}`))
	req6.RemoteAddr = "203.0.113.20:1001"
	req6.Header.Set("X-Forwarded-For", "8.8.8.8")
	err6 := srv.handleSetup(httptest.NewRecorder(), req6)
	if ae, ok := err6.(*apiError); !ok || ae.Status != 429 {
		t.Fatalf("6th attempt: expected 429 Too Many Requests, got %v", err6)
	}

	// 2. Flood 5000 login keys -> must NOT evict totp|1 or setup:203.0.113.20!
	for i := 0; i < 5; i++ {
		srv.recordFailure("totp|1")
	}
	if !srv.tooManyFailures("totp|1") {
		t.Fatalf("expected totp|1 to be locked after 5 failures")
	}
	for i := 0; i < 5000; i++ {
		srv.recordFailure(fmt.Sprintf("user%d|198.51.100.%d", i, i%250))
	}
	if !srv.tooManyFailures("totp|1") {
		t.Fatalf("totp|1 was evicted by IP/username login flood!")
	}
	if !srv.tooManyFailures("setup:203.0.113.20") {
		t.Fatalf("setup:203.0.113.20 was evicted by login flood!")
	}
}
