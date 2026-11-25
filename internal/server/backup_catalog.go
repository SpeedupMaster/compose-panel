package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"compose-panel/internal/backup"
)

// ---------- backup records ----------

type backupRow struct {
	ID          int64        `json:"id"`
	PolicyID    int64        `json:"policyId"`
	Source      BackupSource `json:"source"`
	Label       string       `json:"label"`
	StorageID   int64        `json:"storageId"`
	StorageName string       `json:"storageName"`
	ObjectKey   string       `json:"objectKey"`
	Size        int64        `json:"size"`
	SHA256      string       `json:"sha256"`
	Encrypted   bool         `json:"encrypted"`
	Format      string       `json:"format"`
	Status      string       `json:"status"`
	Error       string       `json:"error"`
	Tiers       string       `json:"tiers"`
	JobID       string       `json:"jobId"`
	StartedAt   int64        `json:"startedAt"`
	FinishedAt  int64        `json:"finishedAt"`
	VerifiedAt  int64        `json:"verifiedAt"` // last successful restore drill
	Origin      string       `json:"origin"`     // backup | upload | scan
}

func (s *Server) listBackups(where string, args ...any) []backupRow {
	rows, err := s.db.Query(`SELECT b.id, COALESCE(b.policy_id,0), b.source, b.label, b.storage_id, COALESCE(t.name,''), b.object_key, b.size, b.sha256, b.encrypted, b.format, b.status, b.error, b.tiers, b.job_id, b.started_at, b.finished_at, b.verified_at, COALESCE(b.origin,'backup')
		FROM backups b LEFT JOIN storage_targets t ON t.id=b.storage_id `+where, args...)
	if err != nil {
		return []backupRow{}
	}
	defer rows.Close()
	out := []backupRow{}
	for rows.Next() {
		var b backupRow
		var src string
		if rows.Scan(&b.ID, &b.PolicyID, &src, &b.Label, &b.StorageID, &b.StorageName, &b.ObjectKey, &b.Size, &b.SHA256, &b.Encrypted, &b.Format, &b.Status, &b.Error, &b.Tiers, &b.JobID, &b.StartedAt, &b.FinishedAt, &b.VerifiedAt, &b.Origin) == nil {
			_ = json.Unmarshal([]byte(src), &b.Source)
			out = append(out, b)
		}
	}
	return out
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request, u *User) error {
	where, args := "WHERE 1=1", []any{}
	q := r.URL.Query()
	if v := q.Get("policy"); v != "" {
		where += " AND b.policy_id=?"
		args = append(args, v)
	}
	if v := q.Get("status"); v != "" {
		where += " AND b.status=?"
		args = append(args, v)
	}
	if v := q.Get("instance"); v != "" {
		where += " AND json_extract(b.source,'$.kind')='database' AND json_extract(b.source,'$.instanceId')=?"
		args = append(args, v)
	}
	if v := q.Get("q"); v != "" {
		where += " AND b.label LIKE ?"
		args = append(args, "%"+v+"%")
	}
	limit := min(max(queryInt(r, "limit", 100), 1), 500)
	if !u.can(roleAdmin) && u.Projects != nil {
		allowedNames := s.allowedProjectNames(u)
		var volProjects map[string]string
		all := s.listBackups(where+" ORDER BY b.started_at DESC", args...)
		filtered := make([]backupRow, 0, min(len(all), limit))
		for _, b := range all {
			if s.canAccessBackupSource(r.Context(), u, b.Source, &volProjects, allowedNames) {
				filtered = append(filtered, b)
				if len(filtered) >= limit {
					break
				}
			}
		}
		return ok(w, filtered)
	}
	return ok(w, s.listBackups(where+" ORDER BY b.started_at DESC LIMIT ?", append(args, limit)...))
}

func (s *Server) backupByID(r *http.Request) (*backupRow, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	list := s.listBackups("WHERE b.id=?", id)
	if len(list) == 0 {
		return nil, errNotFound
	}
	return &list[0], nil
}

func (s *Server) deleteBackup(id int64, keepFile bool) error {
	var sid int64
	var key string
	if err := s.db.QueryRow(`SELECT storage_id, object_key FROM backups WHERE id=?`, id).Scan(&sid, &key); err != nil {
		return err
	}
	if !keepFile && key != "" {
		if _, st, err := s.getStorage(sid); err == nil {
			if err := st.Delete(context.Background(), key); err != nil {
				return err
			}
		}
	}
	_, err := s.db.Exec(`DELETE FROM backups WHERE id=?`, id)
	return err
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	b, err := s.backupByID(r)
	if err != nil {
		return err
	}
	keepFile := r.URL.Query().Get("keepFile") == "1"
	err = s.deleteBackup(b.ID, keepFile)
	detail := b.ObjectKey
	if keepFile {
		detail += "（保留存储文件）"
	}
	s.audit(r, u, "backup.delete", b.Label, detail, err)
	return done(w, err)
}

func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	b, err := s.backupByID(r)
	if err != nil {
		return err
	}
	if b.Status != "success" {
		return bad("备份未成功完成")
	}
	_, st, err := s.getStorage(b.StorageID)
	if err != nil {
		return err
	}
	plain := r.URL.Query().Get("plain") == "1"
	var rc io.ReadCloser
	name := filepath.Base(b.ObjectKey)
	if plain {
		ids, ierr := s.allAgeIdentities("")
		if ierr != nil {
			return ierr
		}
		rc, err = backup.OpenObject(r.Context(), st, b.ObjectKey, b.Encrypted, ids...)
		name = strings.TrimSuffix(strings.TrimSuffix(name, ".age"), ".zst")
	} else {
		rc, err = st.Get(r.Context(), b.ObjectKey)
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	s.audit(r, u, "backup.download", b.Label, name, nil)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s"`, slug(b.Label), name))
	if !plain && b.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(b.Size))
	}
	_, _ = io.Copy(w, rc)
	return nil
}

type countingHashWriter struct {
	w io.Writer
	n *int64
}

func (c *countingHashWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
}

type teeReadCloser struct {
	io.Reader
	io.Closer
}

func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	b, err := s.backupByID(r)
	if err != nil {
		return err
	}
	if !s.canAccessBackupSource(r.Context(), u, b.Source, nil, nil) {
		return errForbidden
	}
	var req struct {
		AgeIdentity string `json:"ageIdentity"`
	}
	if r.Body != nil {
		_ = readJSON(r, &req)
	}
	ids, err := s.allAgeIdentities(req.AgeIdentity)
	if err != nil {
		return err
	}
	jobID := s.jobs.Submit(jobSpec("backup.verify", "校验备份 "+b.Label, b.Label, u.Username, ""), func(run *jobRun) error {
		_, st, err := s.getStorage(b.StorageID)
		if err != nil {
			return err
		}
		run.Logf("单趟下载、校验 SHA-256 并解密解压：%s", b.ObjectKey)
		rcRaw, err := st.Get(run.Context(), b.ObjectKey)
		if err != nil {
			return fmt.Errorf("下载失败：%w", err)
		}
		h := sha256.New()
		var gotSize int64
		tee := &teeReadCloser{
			Reader: io.TeeReader(rcRaw, &countingHashWriter{w: h, n: &gotSize}),
			Closer: rcRaw,
		}
		plain, err := backup.OpenStream(tee, b.Encrypted, ids)
		if err != nil {
			_ = rcRaw.Close()
			return err
		}
		n, copyErr := io.Copy(io.Discard, plain)
		if copyErr == nil {
			// Drain any unread trailing bytes from the raw stream so size and SHA-256 cover the entire object.
			_, copyErr = io.Copy(io.Discard, tee)
		}
		_ = plain.Close()
		if copyErr != nil {
			return fmt.Errorf("解压/解密失败：%w", copyErr)
		}
		gotSHA := hex.EncodeToString(h.Sum(nil))
		if b.Size > 0 && gotSize != b.Size {
			return fmt.Errorf("大小不一致：期望 %d，实际 %d", b.Size, gotSize)
		}
		if b.SHA256 != "" && gotSHA != b.SHA256 {
			return fmt.Errorf("SHA-256 校验失败")
		}
		if b.SHA256 != "" {
			run.Logf("校验和一致（%s）", gotSHA[:16])
		} else {
			run.Logf("SHA-256：%s，大小 %s", gotSHA[:16], humanBytes(gotSize))
		}
		if b.SHA256 == "" || b.Size == 0 {
			_, _ = s.db.Exec(`UPDATE backups SET sha256=?, size=? WHERE id=?`, gotSHA, gotSize, b.ID)
			run.Logf("已回填 SHA-256 与文件大小")
		}
		run.Logf("解密并解压成功，原始大小 %s", humanBytes(n))
		return nil
	})
	return ok(w, map[string]any{"jobId": jobID})
}
