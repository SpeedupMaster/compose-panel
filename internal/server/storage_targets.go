package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"compose-panel/internal/backup"
)

// ---------- storage targets ----------

type storageTarget struct {
	ID           int64               `json:"id"`
	Name         string              `json:"name"`
	Kind         string              `json:"kind"`
	Config       backup.TargetConfig `json:"config"`
	CreatedAt    int64               `json:"createdAt"`
	LastCheckAt  int64               `json:"lastCheckAt"`
	LastCheckOK  bool                `json:"lastCheckOk"`
	LastCheckMsg string              `json:"lastCheckMsg"`
}

func (s *Server) getStorageTarget(id int64) (*storageTarget, error) {
	t := &storageTarget{}
	var enc string
	err := s.db.QueryRow(`SELECT id, name, kind, config, created_at, last_check_at, last_check_ok, last_check_msg FROM storage_targets WHERE id=?`, id).
		Scan(&t.ID, &t.Name, &t.Kind, &enc, &t.CreatedAt, &t.LastCheckAt, &t.LastCheckOK, &t.LastCheckMsg)
	if err != nil {
		return nil, errNotFound
	}
	raw, err := s.box.Open(enc)
	if err != nil {
		return nil, err
	}
	t.Config, _ = backup.ParseTargetConfig(raw)
	return t, nil
}

func (s *Server) getStorage(id int64) (*storageTarget, backup.Storage, error) {
	t, err := s.getStorageTarget(id)
	if err != nil {
		return nil, nil, err
	}
	st, err := backup.Open(t.Kind, t.Config)
	return t, st, err
}

func (s *Server) handleListStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	rows, err := s.db.Query(`SELECT id FROM storage_targets ORDER BY id`)
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
	out := []map[string]any{}
	for _, id := range ids {
		t, err := s.getStorageTarget(id)
		if err != nil || t == nil {
			continue
		}
		t.Config.SecretKey = maskSecret(t.Config.SecretKey)
		t.Config.Password = maskSecret(t.Config.Password)
		t.Config.ClientSecret = maskSecret(t.Config.ClientSecret)
		t.Config.RefreshToken = maskSecret(t.Config.RefreshToken)
		if t.Config.PrivateKey != "" {
			t.Config.PrivateKey = "••••••"
		}
		var count int
		var size int64
		_ = s.db.QueryRow(`SELECT count(*), COALESCE(sum(size),0) FROM backups WHERE storage_id=? AND status='success'`, id).Scan(&count, &size)
		var policies int
		_ = s.db.QueryRow(`SELECT count(*) FROM backup_policies WHERE storage_id=?`, id).Scan(&policies)
		out = append(out, map[string]any{"target": t, "backups": count, "size": size, "policies": policies})
	}
	return ok(w, out)
}

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	return "••••••"
}

type storageReq struct {
	ID     int64               `json:"id,omitempty"`
	Name   string              `json:"name"`
	Kind   string              `json:"kind"`
	Config backup.TargetConfig `json:"config"`
}

func (s *Server) mergeSavedStorageSecrets(id int64, cfg *backup.TargetConfig) {
	if id <= 0 {
		return
	}
	old, _ := s.getStorageTarget(id)
	if old == nil {
		return
	}
	keep := func(v *string, o string) {
		if *v == "" || strings.Contains(*v, "••••••") {
			*v = o
		}
	}
	keep(&cfg.SecretKey, old.Config.SecretKey)
	keep(&cfg.Password, old.Config.Password)
	keep(&cfg.PrivateKey, old.Config.PrivateKey)
	keep(&cfg.ClientSecret, old.Config.ClientSecret)
	keep(&cfg.RefreshToken, old.Config.RefreshToken)
}

func (s *Server) handleSaveStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	var req storageReq
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Name == "" {
		return bad("名称不能为空")
	}
	id, _ := pathID(r, "id")
	if id > 0 {
		s.mergeSavedStorageSecrets(id, &req.Config)
	}
	if req.Kind == "gdrive" {
		req.Config.FolderID = backup.NormalizeGDriveFolderID(req.Config.FolderID)
	}
	if _, err := backup.Open(req.Kind, req.Config); err != nil {
		return bad("%v", err)
	}
	conf, _ := json.Marshal(req.Config)
	if id > 0 {
		_, err := s.db.Exec(`UPDATE storage_targets SET name=?, kind=?, config=? WHERE id=?`, req.Name, req.Kind, s.box.Seal(string(conf)), id)
		s.audit(r, u, "storage.update", req.Name, req.Kind, err)
		return done(w, err)
	}
	res, err := s.db.Exec(`INSERT INTO storage_targets(name,kind,config,created_at) VALUES(?,?,?,?)`, req.Name, req.Kind, s.box.Seal(string(conf)), time.Now().Unix())
	s.audit(r, u, "storage.create", req.Name, req.Kind, err)
	if err != nil {
		return err
	}
	nid, _ := res.LastInsertId()
	return ok(w, map[string]any{"id": nid})
}

func (s *Server) handleTestStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	var st backup.Storage
	var id int64
	if r.PathValue("id") != "" {
		var err error
		id, err = pathID(r, "id")
		if err != nil {
			return err
		}
		_, st, err = s.getStorage(id)
		if err != nil {
			return err
		}
	} else {
		var req storageReq
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if req.ID > 0 {
			s.mergeSavedStorageSecrets(req.ID, &req.Config)
		}
		var err error
		if st, err = backup.Open(req.Kind, req.Config); err != nil {
			return bad("%v", err)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	err := st.Test(ctx)
	msg := "连接正常"
	if err != nil {
		msg = err.Error()
	}
	if id > 0 {
		_, _ = s.db.Exec(`UPDATE storage_targets SET last_check_at=?, last_check_ok=?, last_check_msg=? WHERE id=?`, time.Now().Unix(), boolInt(err == nil), msg, id)
	}
	return ok(w, map[string]any{"ok": err == nil, "message": msg, "elapsedMs": time.Since(start).Milliseconds()})
}

func (s *Server) handleDeleteStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM backup_policies WHERE storage_id=?`, id).Scan(&n)
	if n > 0 {
		return bad("仍有 %d 个备份策略使用该存储", n)
	}
	_, err = s.db.Exec(`DELETE FROM storage_targets WHERE id=?`, id)
	s.audit(r, u, "storage.delete", fmt.Sprint(id), "", err)
	return done(w, err)
}
