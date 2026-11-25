package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"compose-panel/internal/backup"
)

// ---------- policies ----------

type policy struct {
	ID           int64        `json:"id"`
	Name         string       `json:"name"`
	Source       BackupSource `json:"source"`
	Label        string       `json:"label"`
	Schedule     string       `json:"schedule"`
	KeepDaily    int          `json:"keepDaily"`
	KeepWeekly   int          `json:"keepWeekly"`
	KeepMonthly  int          `json:"keepMonthly"`
	KeepYearly   int          `json:"keepYearly"`
	StorageID    int64        `json:"storageId"`
	StorageName  string       `json:"storageName"`
	Encrypt      bool         `json:"encrypt"`
	AgeRecipient string       `json:"ageRecipient"`
	Enabled      bool         `json:"enabled"`
	CreatedAt    int64        `json:"createdAt"`
	LastRun      *backupRow   `json:"lastRun"`
	NextRun      int64        `json:"nextRun"`
	Count        int          `json:"count"`
	TotalSize    int64        `json:"totalSize"`
}

const policyCols = `p.id, p.name, p.source, p.schedule, p.keep_daily, p.keep_weekly, p.keep_monthly, p.keep_yearly, p.storage_id, COALESCE(t.name,''), p.encrypt, p.age_recipient, p.enabled, p.created_at`

func (s *Server) scanPolicy(row interface{ Scan(...any) error }) (*policy, error) {
	p := &policy{}
	var src string
	if err := row.Scan(&p.ID, &p.Name, &src, &p.Schedule, &p.KeepDaily, &p.KeepWeekly, &p.KeepMonthly, &p.KeepYearly, &p.StorageID, &p.StorageName, &p.Encrypt, &p.AgeRecipient, &p.Enabled, &p.CreatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(src), &p.Source)
	p.Label = s.sourceLabel(p.Source)
	return p, nil
}

func (s *Server) getPolicy(id int64) (*policy, error) {
	p, err := s.scanPolicy(s.db.QueryRow(`SELECT `+policyCols+` FROM backup_policies p LEFT JOIN storage_targets t ON t.id=p.storage_id WHERE p.id=?`, id))
	if err == sql.ErrNoRows {
		return nil, errNotFound
	}
	return p, err
}

func (p *policy) keep() backup.Keep {
	return backup.Keep{Daily: p.KeepDaily, Weekly: p.KeepWeekly, Monthly: p.KeepMonthly, Yearly: p.KeepYearly}
}

func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request, u *User) error {
	rows, err := s.db.Query(`SELECT ` + policyCols + ` FROM backup_policies p LEFT JOIN storage_targets t ON t.id=p.storage_id ORDER BY p.id`)
	if err != nil {
		return err
	}
	var list []*policy
	for rows.Next() {
		p, err := s.scanPolicy(rows)
		if err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	if !u.can(roleAdmin) && u.Projects != nil {
		allowedNames := s.allowedProjectNames(u)
		var volProjects map[string]string
		filtered := make([]*policy, 0, len(list))
		for _, p := range list {
			if s.canAccessBackupSource(r.Context(), u, p.Source, &volProjects, allowedNames) {
				filtered = append(filtered, p)
			}
		}
		list = filtered
	}
	for _, p := range list {
		b := s.listBackups(`WHERE b.policy_id=? ORDER BY b.started_at DESC LIMIT 1`, p.ID)
		if len(b) > 0 {
			p.LastRun = &b[0]
		}
		_ = s.db.QueryRow(`SELECT count(*), COALESCE(sum(size),0) FROM backups WHERE policy_id=? AND status='success'`, p.ID).Scan(&p.Count, &p.TotalSize)
		p.NextRun = s.nextRun("policy:" + fmt.Sprint(p.ID))
	}
	return ok(w, nonNil(list))
}

type policyReq struct {
	Name         string       `json:"name"`
	Source       BackupSource `json:"source"`
	Schedule     string       `json:"schedule"`
	KeepDaily    int          `json:"keepDaily"`
	KeepWeekly   int          `json:"keepWeekly"`
	KeepMonthly  int          `json:"keepMonthly"`
	KeepYearly   int          `json:"keepYearly"`
	StorageID    int64        `json:"storageId"`
	Encrypt      bool         `json:"encrypt"`
	AgeRecipient string       `json:"ageRecipient"`
	Enabled      bool         `json:"enabled"`
}

func (s *Server) checkPolicyReq(req *policyReq) error {
	if err := s.validateSource(&req.Source); err != nil {
		return err
	}
	if _, err := cronParser.Parse(req.Schedule); err != nil {
		return bad("Cron 表达式无效：%v", err)
	}
	if _, err := s.getStorageTarget(req.StorageID); err != nil {
		return bad("存储目标无效")
	}
	if _, err := backup.ParseRecipients(req.AgeRecipient); err != nil {
		return bad("%v", err)
	}
	if req.KeepDaily+req.KeepWeekly+req.KeepMonthly+req.KeepYearly == 0 {
		return bad("至少保留一份备份")
	}
	if req.Name == "" {
		req.Name = s.sourceLabel(req.Source)
	}
	return nil
}

func (s *Server) handleCreatePolicy(w http.ResponseWriter, r *http.Request, u *User) error {
	var req policyReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := s.checkPolicyReq(&req); err != nil {
		return err
	}
	if !s.canAccessBackupSource(r.Context(), u, req.Source, nil, nil) {
		return errForbidden
	}
	src, _ := json.Marshal(req.Source)
	res, err := s.db.Exec(`INSERT INTO backup_policies(name,source,schedule,keep_daily,keep_weekly,keep_monthly,keep_yearly,storage_id,encrypt,age_recipient,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.Name, string(src), req.Schedule, req.KeepDaily, req.KeepWeekly, req.KeepMonthly, req.KeepYearly, req.StorageID, boolInt(req.Encrypt), req.AgeRecipient, boolInt(req.Enabled), time.Now().Unix())
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	s.reloadSchedules()
	s.audit(r, u, "backup.policy_create", req.Name, req.Schedule, nil)
	return ok(w, map[string]any{"id": id})
}

func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	old, err := s.getPolicy(id)
	if err != nil {
		return err
	}
	if !s.canAccessBackupSource(r.Context(), u, old.Source, nil, nil) {
		return errForbidden
	}
	var req policyReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := s.checkPolicyReq(&req); err != nil {
		return err
	}
	if !s.canAccessBackupSource(r.Context(), u, req.Source, nil, nil) {
		return errForbidden
	}
	src, _ := json.Marshal(req.Source)
	_, err = s.db.Exec(`UPDATE backup_policies SET name=?, source=?, schedule=?, keep_daily=?, keep_weekly=?, keep_monthly=?, keep_yearly=?, storage_id=?, encrypt=?, age_recipient=?, enabled=? WHERE id=?`,
		req.Name, string(src), req.Schedule, req.KeepDaily, req.KeepWeekly, req.KeepMonthly, req.KeepYearly, req.StorageID, boolInt(req.Encrypt), req.AgeRecipient, boolInt(req.Enabled), id)
	s.reloadSchedules()
	s.audit(r, u, "backup.policy_update", req.Name, req.Schedule, err)
	return done(w, err)
}

func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	p, err := s.getPolicy(id)
	if err != nil {
		return err
	}
	if r.URL.Query().Get("backups") == "1" {
		rows, _ := s.db.Query(`SELECT id FROM backups WHERE policy_id=?`, id)
		var ids []int64
		if rows != nil {
			for rows.Next() {
				var b int64
				_ = rows.Scan(&b)
				ids = append(ids, b)
			}
			rows.Close()
		}
		for _, b := range ids {
			_ = s.deleteBackup(b, false)
		}
	}
	_, err = s.db.Exec(`DELETE FROM backup_policies WHERE id=?`, id)
	s.reloadSchedules()
	s.audit(r, u, "backup.policy_delete", p.Name, "", err)
	return done(w, err)
}

func (s *Server) runPolicy(p *policy, actor string) string {
	k := p.keep()
	return s.submitBackup(backupRequest{PolicyID: p.ID, Source: p.Source, StorageID: p.StorageID, Encrypt: p.Encrypt, Recipients: p.AgeRecipient, Keep: &k, Actor: actor})
}

func (s *Server) handleRunPolicy(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	p, err := s.getPolicy(id)
	if err != nil {
		return err
	}
	if !s.canAccessBackupSource(r.Context(), u, p.Source, nil, nil) {
		return errForbidden
	}
	jobID := s.runPolicy(p, u.Username)
	s.audit(r, u, "backup.run", p.Name, "手动执行", nil)
	return ok(w, map[string]any{"jobId": jobID})
}

// handleBackupNow runs an ad-hoc backup without a policy.
func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Source    BackupSource `json:"source"`
		StorageID int64        `json:"storageId"`
		Encrypt   bool         `json:"encrypt"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := s.validateSource(&req.Source); err != nil {
		return err
	}
	if !s.canAccessBackupSource(r.Context(), u, req.Source, nil, nil) {
		return errForbidden
	}
	if req.StorageID == 0 {
		_ = s.db.QueryRow(`SELECT id FROM storage_targets ORDER BY id LIMIT 1`).Scan(&req.StorageID)
	}
	jobID := s.submitBackup(backupRequest{Source: req.Source, StorageID: req.StorageID, Encrypt: req.Encrypt, Actor: u.Username})
	s.audit(r, u, "backup.run", s.sourceLabel(req.Source), "临时备份", nil)
	return ok(w, map[string]any{"jobId": jobID})
}
