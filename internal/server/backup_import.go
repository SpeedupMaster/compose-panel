package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compose-panel/internal/backup"
)

type scannedBackupItem struct {
	ObjectKey   string `json:"objectKey"`
	Size        int64  `json:"size"`
	ModTime     int64  `json:"modTime"`
	Recognized  bool   `json:"recognized"`
	Imported    bool   `json:"imported"`
	BackupID    int64  `json:"backupId,omitempty"`
	Kind        string `json:"kind"`
	Format      string `json:"format"`
	EngineHint  string `json:"engineHint,omitempty"`
	VolumeHint  string `json:"volumeHint,omitempty"`
	Encrypted   bool   `json:"encrypted"`
	Compression string `json:"compression"`
	Label       string `json:"label"`
	StartedAt   int64  `json:"startedAt"`
}

func validImportKind(k string) bool {
	return k == "database" || k == "volume" || k == "dir" || k == "panel"
}

func validImportFormat(f string) bool {
	switch f {
	case "pgdump", "sql", "archive", "rdb", "tar", "sqlite":
		return true
	}
	return false
}

func sanitizeImportFilename(raw string) string {
	base := filepath.Base(strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/"))
	if base == "" || base == "." || base == "/" {
		return ""
	}
	clean := slug(base)
	if clean == "" {
		return ""
	}
	return clean
}

func inferSourceForImport(kind, volumeHint string) BackupSource {
	src := BackupSource{Kind: kind}
	if kind == "volume" && volumeHint != "" {
		src.Volume = volumeHint
	}
	return src
}

// handleImportUpload streams an uploaded backup file directly to the chosen storage
// target, detects age encryption from header magic bytes, records SHA-256 and registers
// it in the backups table with origin='upload'.
func (s *Server) handleImportUpload(w http.ResponseWriter, r *http.Request, u *User) error {
	q := r.URL.Query()
	rawName := strings.TrimSpace(q.Get("name"))
	safeName := sanitizeImportFilename(rawName)
	if safeName == "" {
		return bad("文件名无效")
	}

	var storageID int64
	if sidStr := strings.TrimSpace(q.Get("storageId")); sidStr != "" {
		v, err := strconv.ParseInt(sidStr, 10, 64)
		if err != nil || v <= 0 {
			return bad("存储目标无效")
		}
		storageID = v
	} else {
		_ = s.db.QueryRow(`SELECT id FROM storage_targets ORDER BY id LIMIT 1`).Scan(&storageID)
	}
	_, st, err := s.getStorage(storageID)
	if err != nil {
		return bad("存储目标不可用：%v", err)
	}

	parsed := backup.ParseBackupKey(rawName)
	kind := strings.TrimSpace(q.Get("kind"))
	if kind == "" {
		kind = parsed.Kind
	}
	if !validImportKind(kind) {
		return bad("无法识别备份类型，请手动选择（数据库 / 数据卷 / 项目目录 / 面板数据）")
	}
	format := strings.TrimSpace(q.Get("format"))
	if format == "" {
		format = parsed.Format
	}
	if format == "" {
		switch kind {
		case "volume", "dir":
			format = "tar"
		case "panel":
			format = "sqlite"
		}
	}
	if !validImportFormat(format) {
		return bad("无法识别备份格式，请手动选择")
	}
	label := strings.TrimSpace(q.Get("label"))
	if label == "" {
		label = parsed.Label
	}
	if label == "" {
		label = safeName
	}

	br := bufio.NewReaderSize(r.Body, 64<<10)
	head, peekErr := br.Peek(32)
	if len(head) == 0 && peekErr != nil {
		return bad("上传的文件为空")
	}
	encrypted := parsed.Encrypted || backup.HasAgeHeader(head)

	now := time.Now()
	objectKey := fmt.Sprintf("imported/%s-%s", now.In(s.location()).Format("20060102-150405"), safeName)

	hasher := sha256.New()
	var written int64
	counter := &uploadCountReader{r: io.TeeReader(br, hasher), n: &written}

	putSize, err := st.Put(r.Context(), objectKey, counter)
	if err != nil {
		_ = st.Delete(context.Background(), objectKey)
		return fmt.Errorf("写入存储失败：%w", err)
	}
	size := written
	if size == 0 && putSize > 0 {
		size = putSize
	}
	sha := hex.EncodeToString(hasher.Sum(nil))

	srcJSON, _ := json.Marshal(inferSourceForImport(kind, parsed.VolumeHint))
	startedAt := now.Unix()
	if !parsed.Timestamp.IsZero() {
		startedAt = parsed.Timestamp.Unix()
	}
	res, err := s.db.Exec(`INSERT INTO backups(policy_id,source,label,storage_id,object_key,size,sha256,encrypted,format,status,started_at,finished_at,origin) VALUES(NULL,?,?,?,?,?,?,?,?,'success',?,?,'upload')`,
		string(srcJSON), label, storageID, objectKey, size, sha, boolInt(encrypted), format, startedAt, now.Unix())
	if err != nil {
		_ = st.Delete(context.Background(), objectKey)
		return err
	}
	id, _ := res.LastInsertId()
	list := s.listBackups("WHERE b.id=?", id)
	if len(list) == 0 {
		return fmt.Errorf("记录写入失败")
	}
	s.audit(r, u, "backup.import_upload", label, fmt.Sprintf("%s (%s)", objectKey, humanBytes(size)), nil)
	return ok(w, map[string]any{"backup": list[0]})
}

type uploadCountReader struct {
	r io.Reader
	n *int64
}

func (u *uploadCountReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	*u.n += int64(n)
	return n, err
}

// handleScanStorage lists objects in a storage target and infers backup metadata.
func (s *Server) handleScanStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	target, st, err := s.getStorage(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	objs, listErr := st.List(ctx)
	truncated := false
	if listErr != nil {
		if errors.Is(listErr, backup.ErrListTruncated) {
			truncated = true
		} else {
			return fmt.Errorf("扫描存储 %s 失败：%w", target.Name, listErr)
		}
	}

	existing := map[string]int64{}
	rows, err := s.db.Query(`SELECT id, object_key FROM backups WHERE storage_id=? AND object_key<>''`, id)
	if err == nil {
		for rows.Next() {
			var bid int64
			var key string
			if rows.Scan(&bid, &key) == nil {
				existing[key] = bid
			}
		}
		rows.Close()
	}

	items := make([]scannedBackupItem, 0, len(objs))
	for _, obj := range objs {
		p := backup.ParseBackupKey(obj.Key)
		modTS := int64(0)
		if !obj.ModTime.IsZero() {
			modTS = obj.ModTime.Unix()
		}
		startedAt := modTS
		if !p.Timestamp.IsZero() {
			startedAt = p.Timestamp.Unix()
		}
		if startedAt == 0 {
			startedAt = time.Now().Unix()
		}
		bid, imported := existing[obj.Key]
		items = append(items, scannedBackupItem{
			ObjectKey:   obj.Key,
			Size:        obj.Size,
			ModTime:     modTS,
			Recognized:  p.Recognized,
			Imported:    imported,
			BackupID:    bid,
			Kind:        p.Kind,
			Format:      p.Format,
			EngineHint:  p.EngineHint,
			VolumeHint:  p.VolumeHint,
			Encrypted:   p.Encrypted,
			Compression: p.Compression,
			Label:       p.Label,
			StartedAt:   startedAt,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].StartedAt != items[j].StartedAt {
			return items[i].StartedAt > items[j].StartedAt
		}
		return items[i].ObjectKey > items[j].ObjectKey
	})

	s.audit(r, u, "backup.scan_storage", target.Name, fmt.Sprintf("发现 %d 个对象", len(items)), nil)
	return ok(w, map[string]any{"items": items, "truncated": truncated})
}

// handleImportStorage registers selected scanned storage objects into the backups table.
func (s *Server) handleImportStorage(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		StorageID int64 `json:"storageId"`
		Items     []struct {
			ObjectKey  string `json:"objectKey"`
			Kind       string `json:"kind"`
			Format     string `json:"format"`
			Label      string `json:"label"`
			VolumeHint string `json:"volumeHint"`
			Encrypted  bool   `json:"encrypted"`
			Size       int64  `json:"size"`
			StartedAt  int64  `json:"startedAt"`
		} `json:"items"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.StorageID <= 0 || len(req.Items) == 0 {
		return bad("请选择存储位置和要导入的备份对象")
	}
	target, _, err := s.getStorage(req.StorageID)
	if err != nil {
		return bad("存储目标不可用：%v", err)
	}

	existing := map[string]bool{}
	rows, err := s.db.Query(`SELECT object_key FROM backups WHERE storage_id=? AND object_key<>''`, req.StorageID)
	if err == nil {
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				existing[k] = true
			}
		}
		rows.Close()
	}

	now := time.Now().Unix()
	imported := []backupRow{}
	skipped := 0
	for _, it := range req.Items {
		cleanKey := strings.TrimLeft(strings.ReplaceAll(strings.TrimSpace(it.ObjectKey), "\\", "/"), "/")
		if cleanKey == "" || strings.Contains(path.Clean("/"+cleanKey), "..") || strings.Contains(cleanKey, "..") {
			return bad("对象路径无效：%s", it.ObjectKey)
		}
		if existing[cleanKey] {
			skipped++
			continue
		}
		parsed := backup.ParseBackupKey(cleanKey)
		kind := strings.TrimSpace(it.Kind)
		if kind == "" {
			kind = parsed.Kind
		}
		if !validImportKind(kind) {
			return bad("备份 %s 的类型无效，请手动指定", cleanKey)
		}
		format := strings.TrimSpace(it.Format)
		if format == "" {
			format = parsed.Format
		}
		if format == "" {
			switch kind {
			case "volume", "dir":
				format = "tar"
			case "panel":
				format = "sqlite"
			}
		}
		if !validImportFormat(format) {
			return bad("备份 %s 的格式无效，请手动指定", cleanKey)
		}
		label := strings.TrimSpace(it.Label)
		if label == "" {
			label = parsed.Label
		}
		volHint := strings.TrimSpace(it.VolumeHint)
		if volHint == "" {
			volHint = parsed.VolumeHint
		}
		startedAt := it.StartedAt
		if startedAt <= 0 {
			if !parsed.Timestamp.IsZero() {
				startedAt = parsed.Timestamp.Unix()
			} else {
				startedAt = now
			}
		}
		size := it.Size
		if size < 0 {
			size = 0
		}
		srcJSON, _ := json.Marshal(inferSourceForImport(kind, volHint))
		res, err := s.db.Exec(`INSERT INTO backups(policy_id,source,label,storage_id,object_key,size,sha256,encrypted,format,status,started_at,finished_at,origin) VALUES(NULL,?,?,?,?,?,'',?,?,'success',?,?,'scan')`,
			string(srcJSON), label, req.StorageID, cleanKey, size, boolInt(it.Encrypted || parsed.Encrypted), format, startedAt, startedAt)
		if err != nil {
			return err
		}
		bid, _ := res.LastInsertId()
		existing[cleanKey] = true
		if b := s.listBackups("WHERE b.id=?", bid); len(b) == 1 {
			imported = append(imported, b[0])
		}
	}

	s.audit(r, u, "backup.import_storage", target.Name, fmt.Sprintf("导入 %d 条，跳过 %d 条", len(imported), skipped), nil)
	return ok(w, map[string]any{"imported": imported, "skipped": skipped})
}
