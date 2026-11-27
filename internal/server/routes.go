package server

import (
	"net/http"
	"path"
	"strings"
)

func (s *Server) routes(mux *http.ServeMux) {
	V, O, A := roleViewer, roleOperator, roleAdmin
	h := func(pattern string, min int, fn handlerFunc) { mux.HandleFunc(pattern, s.authed(min, fn)) }

	// setup & auth
	mux.HandleFunc("GET /api/setup/status", public(s.handleSetupStatus))
	mux.HandleFunc("POST /api/setup", public(s.handleSetup))
	mux.HandleFunc("POST /api/setup/restore", public(s.handleSetupRestore))
	mux.HandleFunc("POST /api/auth/login", public(s.handleLogin))
	mux.HandleFunc("POST /api/auth/totp", public(s.handleTOTPLogin))
	mux.HandleFunc("POST /api/auth/logout", public(s.handleLogout))
	h("GET /api/auth/me", V, s.handleMe)
	h("POST /api/me/password", V, s.handleChangePassword)
	h("POST /api/me/totp/setup", V, s.handleTOTPSetup)
	h("POST /api/me/totp/enable", V, s.handleTOTPEnable)
	h("POST /api/me/totp/disable", V, s.handleTOTPDisable)
	h("GET /api/me/sessions", V, s.handleMySessions)
	h("DELETE /api/me/sessions/{id}", V, s.handleRevokeSession)

	h("GET /api/overview", V, s.handleOverview)

	// projects & deployments
	h("GET /api/projects", V, s.handleListProjects)
	h("POST /api/projects", O, s.handleCreateProject)
	h("GET /api/projects/{id}", V, s.handleGetProject)
	h("PUT /api/projects/{id}", O, s.handleUpdateProject)
	h("DELETE /api/projects/{id}", A, s.handleDeleteProject)
	h("GET /api/projects/{id}/files", V, s.handleProjectFiles)
	h("PUT /api/projects/{id}/files", O, s.handleSaveProjectFiles)
	h("POST /api/projects/{id}/deploy", O, s.handleDeploy)
	h("POST /api/projects/{id}/action", O, s.handleProjectAction)
	h("GET /api/projects/{id}/deployments", V, s.handleListDeployments)
	h("GET /api/projects/{id}/log-archives", V, s.handleListProjectLogArchives)
	h("GET /api/projects/{id}/metrics", V, s.handleProjectMetrics)
	h("POST /api/projects/{id}/webhook/rotate", O, s.handleRotateWebhook)
	h("POST /api/projects/{id}/deploy-key", O, s.handleRegenDeployKey)
	h("POST /api/compose/validate", O, s.handleValidateCompose)
	h("GET /api/deployments/{id}", V, s.handleGetDeployment)
	h("POST /api/deployments/{id}/rollback", O, s.handleRollback)
	h("GET /api/templates", V, s.handleTemplates)
	h("DELETE /api/templates/{id}", O, s.handleDeleteTemplate)
	h("POST /api/projects/{id}/template", O, s.handleSaveAsTemplate)
	h("GET /api/projects/{id}/images", V, s.handleProjectImages)
	h("POST /api/projects/{id}/images/check", O, s.handleCheckProjectImages)
	h("GET /api/image-updates", V, s.handleImageUpdates)
	h("GET /api/cron-tasks", V, s.handleListCronTasks)
	h("POST /api/cron-tasks", O, s.handleSaveCronTask)
	h("PUT /api/cron-tasks/{id}", O, s.handleSaveCronTask)
	h("DELETE /api/cron-tasks/{id}", O, s.handleDeleteCronTask)
	h("POST /api/cron-tasks/{id}/run", O, s.handleRunCronTask)
	h("GET /api/import/scan", A, s.handleImportScan)
	h("POST /api/import", A, s.handleImport)

	// containers & docker resources
	h("GET /api/containers", V, s.handleListContainers)
	h("GET /api/containers/stats", V, s.handleAllStats)
	h("GET /api/containers/{id}", V, s.handleGetContainer)
	h("GET /api/containers/{id}/stats", V, s.handleContainerStats)
	h("GET /api/containers/{id}/logs", V, s.handleContainerLogs)
	h("GET /api/containers/{id}/log-archives", V, s.handleListContainerLogArchives)
	h("GET /api/log-archives", V, s.handleListAllLogArchives)
	h("GET /api/log-archives/{id}", V, s.handleGetLogArchive)
	h("DELETE /api/log-archives/{id}", O, s.handleDeleteLogArchive)
	h("GET /api/containers/{id}/terminal", O, s.handleTerminal)
	h("GET /api/containers/{id}/metrics", V, s.handleContainerMetrics)
	h("GET /api/containers/{id}/fs", O, s.handleContainerFS)
	h("GET /api/containers/{id}/fs/download", O, s.handleContainerDownload)
	h("PUT /api/containers/{id}/fs/upload", O, s.handleContainerUpload)
	h("GET /api/docker/volumes/{name}/fs", A, s.handleVolumeFS)
	h("GET /api/docker/volumes/{name}/fs/download", A, s.handleVolumeDownload)
	h("PUT /api/docker/volumes/{name}/fs/upload", A, s.handleVolumeUpload)
	h("GET /api/metrics/host", V, s.handleHostMetrics)
	h("POST /api/containers/{id}/{action}", O, s.handleContainerAction)
	h("GET /api/docker/df", V, s.handleDockerDF)
	h("GET /api/docker/images", V, s.handleListImages)
	h("DELETE /api/docker/images/{id}", A, s.handleRemoveImage)
	h("GET /api/docker/volumes", V, s.handleListVolumes)
	h("DELETE /api/docker/volumes/{name}", A, s.handleRemoveVolume)
	h("GET /api/docker/networks", V, s.handleListNetworks)
	h("DELETE /api/docker/networks/{id}", A, s.handleRemoveNetwork)
	h("POST /api/docker/prune", A, s.handlePrune)
	h("GET /api/registries", O, s.handleListRegistries)
	h("POST /api/registries", A, s.handleSaveRegistry)
	h("DELETE /api/registries/{id}", A, s.handleDeleteRegistry)

	// databases
	h("GET /api/db-engines", V, s.handleDBEngines)
	h("GET /api/databases", V, s.handleListDatabases)
	h("POST /api/databases", O, s.handleCreateDatabase)
	h("GET /api/databases/{id}", V, s.handleGetDatabase)
	h("PUT /api/databases/{id}", O, s.handleUpdateDatabase)
	h("DELETE /api/databases/{id}", A, s.handleDeleteDatabase)
	h("GET /api/databases/{id}/credentials", O, s.handleDatabaseCredentials)
	h("GET /api/databases/{id}/dbs", V, s.handleDBList)
	h("POST /api/databases/{id}/dbs", O, s.handleDBCreate)
	h("DELETE /api/databases/{id}/dbs/{name}", A, s.handleDBDrop)
	h("GET /api/databases/{id}/users", V, s.handleDBUsers)
	h("POST /api/databases/{id}/users", O, s.handleDBCreateUser)
	h("POST /api/databases/{id}/users/{name}/password", O, s.handleDBUserPassword)
	h("DELETE /api/databases/{id}/users/{name}", A, s.handleDBDropUser)
	h("GET /api/databases/{id}/sessions", V, s.handleDBSessions)
	h("DELETE /api/databases/{id}/sessions/{pid}", O, s.handleDBKill)
	h("GET /api/databases/{id}/tables", V, s.handleDBTables)
	h("POST /api/databases/{id}/query", O, s.handleDBQuery)
	h("GET /api/databases/{id}/tables/{table}", V, s.handleDBTableInfo)
	h("GET /api/databases/{id}/tables/{table}/rows", V, s.handleDBTableRows)
	h("GET /api/databases/{id}/slow-queries", V, s.handleDBSlowQueries)
	h("GET /api/databases/{id}/export", A, s.handleDBExport)
	h("POST /api/databases/{id}/import", O, s.handleDBImport)

	// backups & storage
	h("GET /api/backup-policies", V, s.handleListPolicies)
	h("POST /api/backup-policies", O, s.handleCreatePolicy)
	h("PUT /api/backup-policies/{id}", O, s.handleUpdatePolicy)
	h("DELETE /api/backup-policies/{id}", A, s.handleDeletePolicy)
	h("POST /api/backup-policies/{id}/run", O, s.handleRunPolicy)
	h("POST /api/backups/run", O, s.handleBackupNow)
	h("POST /api/backups/import/upload", A, s.handleImportUpload)
	h("POST /api/backups/import/storage", A, s.handleImportStorage)
	h("GET /api/backups", V, s.handleListBackups)
	h("DELETE /api/backups/{id}", A, s.handleDeleteBackup)
	h("GET /api/backups/{id}/download", A, s.handleDownloadBackup)
	h("POST /api/backups/{id}/verify", O, s.handleVerifyBackup)
	h("POST /api/backups/{id}/restore", A, s.handleRestoreBackup)
	h("GET /api/backup-sources", V, s.handleBackupSources)
	h("GET /api/backup-key", A, s.handleBackupKey)
	h("POST /api/backup-key/import", A, s.handleImportBackupKey)
	h("DELETE /api/backup-key/extra/{id}", A, s.handleDeleteExtraBackupKey)
	h("GET /api/storage", V, s.handleListStorage)
	h("POST /api/storage", A, s.handleSaveStorage)
	h("PUT /api/storage/{id}", A, s.handleSaveStorage)
	h("DELETE /api/storage/{id}", A, s.handleDeleteStorage)
	h("POST /api/storage/test", A, s.handleTestStorage)
	h("POST /api/storage/{id}/test", O, s.handleTestStorage)
	h("POST /api/storage/{id}/scan", A, s.handleScanStorage)
	h("POST /api/storage/gdrive/auth-url", A, s.handleGDriveAuthURL)
	h("POST /api/storage/gdrive/auth-result", A, s.handleGDriveAuthResult)
	mux.HandleFunc("GET /api/storage/gdrive/callback", s.handleGDriveCallback)
	h("GET /api/restore-drills", V, s.handleListDrills)
	h("POST /api/restore-drills", O, s.handleSaveDrill)
	h("PUT /api/restore-drills/{id}", O, s.handleSaveDrill)
	h("DELETE /api/restore-drills/{id}", O, s.handleDeleteDrill)
	h("POST /api/restore-drills/{id}/run", O, s.handleRunDrill)

	// proxy & certificates
	h("GET /api/sites", V, s.handleListSites)
	h("POST /api/sites", O, s.handleSaveSite)
	h("PUT /api/sites/{id}", O, s.handleSaveSite)
	h("DELETE /api/sites/{id}", O, s.handleDeleteSite)
	h("POST /api/sites/precheck", O, s.handleSitePrecheck)
	h("POST /api/sites/{id}/maintenance", O, s.handleSiteMaintenance)
	h("GET /api/sites/{id}/access-logs", V, s.handleSiteAccessLogs)
	h("GET /api/proxy/config", A, s.handleProxyConfig)
	h("POST /api/proxy/reload", A, s.handleProxyReload)
	h("GET /api/certificates", V, s.handleListCerts)
	h("POST /api/certificates", A, s.handleUploadCert)
	h("DELETE /api/certificates/{id}", A, s.handleDeleteCert)

	// jobs & schedules
	h("GET /api/jobs", V, s.handleListJobs)
	h("GET /api/jobs/{id}", V, s.handleGetJob)
	h("GET /api/jobs/{id}/log", V, s.handleJobLog)
	h("POST /api/jobs/{id}/cancel", O, s.handleCancelJob)
	h("GET /api/schedules", V, s.handleSchedules)
	h("POST /api/schedules/{key}/run", O, s.handleRunSchedule)

	// notifications
	h("GET /api/notify", A, s.handleListChannels)
	h("POST /api/notify/channels", A, s.handleSaveChannel)
	h("PUT /api/notify/channels/{id}", A, s.handleSaveChannel)
	h("DELETE /api/notify/channels/{id}", A, s.handleDeleteChannel)
	h("POST /api/notify/channels/{id}/test", A, s.handleTestChannel)
	h("PUT /api/notify/rules", A, s.handleSaveRules)

	// users, tokens, audit
	h("GET /api/users", A, s.handleListUsers)
	h("POST /api/users", A, s.handleCreateUser)
	h("PUT /api/users/{id}", A, s.handleUpdateUser)
	h("POST /api/users/{id}/reset-password", A, s.handleResetUserPassword)
	h("DELETE /api/users/{id}", A, s.handleDeleteUser)
	h("GET /api/tokens", V, s.handleListTokens)
	h("POST /api/tokens", V, s.handleCreateToken)
	h("DELETE /api/tokens/{id}", V, s.handleDeleteToken)
	h("GET /api/audit", A, s.handleAudit)
	h("GET /api/audit/export", A, s.handleAuditExport)
	h("GET /api/audit/verify", A, s.handleAuditVerify)

	// settings & system
	h("GET /api/settings", A, s.handleGetSettings)
	h("PUT /api/settings", A, s.handleSaveSettings)
	h("GET /api/system/info", V, s.handleSystemInfo)
	h("GET /api/system/master-key", A, s.handleMasterKey)
	h("GET /api/system/panel-backups", A, s.handleListPanelBackups)
	h("POST /api/system/panel-backup", A, s.handlePanelBackup)
	h("POST /api/system/panel-restore", A, s.handleSystemPanelRestore)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]any{"error": "接口不存在"})
	})
}

func (s *Server) handleProjectFiles(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	return ok(w, s.projectFiles(p.ID))
}

// handleSaveProjectFiles replaces the extra files written next to compose.yml (inline projects only).
func (s *Server) handleSaveProjectFiles(w http.ResponseWriter, r *http.Request, u *User) error {
	p, err := s.projectForUser(r, u)
	if err != nil {
		return err
	}
	if p.Source != "inline" {
		return bad("仅面板内编辑的项目支持附加文件")
	}
	var files []projectFile
	if err := readJSON(r, &files); err != nil {
		return err
	}
	if err := cleanProjectFiles(files); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM project_files WHERE project_id=?`, p.ID); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.Exec(`INSERT INTO project_files(project_id,path,content) VALUES(?,?,?)`, p.ID, f.Path, f.Content); err != nil {
			return err
		}
	}
	err = tx.Commit()
	s.audit(r, u, "project.files", p.Name, "", err)
	return done(w, err)
}

func cleanProjectFiles(files []projectFile) error {
	for i, f := range files {
		clean := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(f.Path, "\\", "/")), "/")
		if clean == "" || strings.HasPrefix(clean, ".panel") || clean == "compose.yml" || strings.Contains(clean, "..") {
			return bad("文件路径无效：%s", f.Path)
		}
		files[i].Path = clean
	}
	return nil
}
