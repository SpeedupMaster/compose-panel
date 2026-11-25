package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"compose-panel/internal/backup"
	"compose-panel/internal/dbmgr"
	"compose-panel/internal/docker"
)

type restorePlan struct {
	Kind           string
	Mode           string // new | overwrite
	InstanceID     int64
	Database       string
	SourceDatabase string
	Volume         string
	ProjectID      int64
	Path           string
	TargetLabel    string
}

func pathOverlaps(a, b string) bool {
	ca := filepath.Clean(a)
	cb := filepath.Clean(b)
	if ca == cb {
		return true
	}
	sep := string(filepath.Separator)
	aPfx := ca
	if !strings.HasSuffix(aPfx, sep) {
		aPfx += sep
	}
	bPfx := cb
	if !strings.HasSuffix(bPfx, sep) {
		bPfx += sep
	}
	return strings.HasPrefix(cb, aPfx) || strings.HasPrefix(ca, bPfx)
}

func containerBlocksOverwrite(state string) bool {
	if state == "" {
		return true
	}
	return state != "exited" && state != "created" && state != "dead"
}

func (s *Server) runningMountHolders(ctx context.Context, match func(docker.Mount) bool) ([]string, error) {
	cs, err := s.dk.ListContainers(ctx, false, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, c := range cs {
		if !containerBlocksOverwrite(c.State) {
			continue
		}
		for _, m := range c.Mounts {
			if match(m) {
				names = append(names, c.Name())
				break
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *Server) runningVolumeHolders(ctx context.Context, volume string) ([]string, error) {
	return s.runningMountHolders(ctx, func(m docker.Mount) bool {
		return m.Type == "volume" && m.Name == volume
	})
}

func (s *Server) runningBindHolders(ctx context.Context, targetDir string) ([]string, error) {
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		absTarget = filepath.Clean(targetDir)
	}
	return s.runningMountHolders(ctx, func(m docker.Mount) bool {
		return m.Type == "bind" && m.Source != "" && pathOverlaps(absTarget, m.Source)
	})
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request, u *User) error {
	b, err := s.backupByID(r)
	if err != nil {
		return err
	}
	if b.Status != "success" {
		return bad("备份未成功完成")
	}
	var req struct {
		Kind           string `json:"kind"`
		Mode           string `json:"mode"` // new | overwrite
		InstanceID     int64  `json:"instanceId"`
		Database       string `json:"database"`
		SourceDatabase string `json:"sourceDatabase"`
		Volume         string `json:"volume"`
		ProjectID      int64  `json:"projectId"`
		Path           string `json:"path"`
		Target         string `json:"target"` // legacy field: new database / volume / directory name
		Confirm        string `json:"confirm"`
		AgeIdentity    string `json:"ageIdentity"`
		MasterKey      string `json:"masterKey"`
		DryRun         bool   `json:"dryRun"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}

	identities, err := s.allAgeIdentities(req.AgeIdentity)
	if err != nil {
		return err
	}

	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = b.Source.Kind
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "new"
	}
	if mode != "new" && mode != "overwrite" {
		return bad("恢复方式无效")
	}

	plan := restorePlan{Kind: kind, Mode: mode}
	switch kind {
	case "database":
		instID := req.InstanceID
		if instID == 0 {
			instID = b.Source.InstanceID
		}
		d, err := s.getDB(instID)
		if err != nil {
			return bad("请选择有效的目标数据库实例")
		}
		if !backup.FormatCompatibleWithEngine(b.Format, d.Engine) {
			return bad("备份格式 %s 与目标实例引擎 %s 不兼容", b.Format, d.Engine)
		}
		plan.InstanceID = d.ID
		plan.SourceDatabase = strings.TrimSpace(req.SourceDatabase)
		if plan.SourceDatabase == "" {
			plan.SourceDatabase = b.Source.Database
		}
		if plan.SourceDatabase != "" && d.Engine != "redis" && !dbmgr.ValidIdent(plan.SourceDatabase) {
			return bad("源库名无效")
		}
		if d.Engine == "redis" {
			if req.Confirm != d.Name {
				return bad("Redis 只能整库覆盖恢复，请输入实例名 %s 确认", d.Name)
			}
			plan.Mode = "overwrite"
			plan.Database = "all"
			plan.TargetLabel = d.Name
			break
		}
		targetDB := strings.TrimSpace(req.Database)
		if targetDB == "" {
			targetDB = strings.TrimSpace(req.Target)
		}
		if targetDB == "" && mode == "overwrite" {
			targetDB = b.Source.Database
		}
		if !dbmgr.ValidIdent(targetDB) {
			return bad("目标库名无效")
		}
		if mode == "overwrite" && req.Confirm != targetDB {
			return bad("请输入库名 %s 确认覆盖", targetDB)
		}
		plan.Database = targetDB
		plan.TargetLabel = d.Name + "/" + targetDB

	case "volume":
		if b.Format != "" && !backup.ValidKindFormat("volume", b.Format) {
			return bad("备份格式 %s 不是 tar 归档，无法恢复到数据卷", b.Format)
		}
		vol := strings.TrimSpace(req.Volume)
		if vol == "" {
			vol = strings.TrimSpace(req.Target)
		}
		if vol == "" && mode == "overwrite" {
			vol = b.Source.Volume
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`).MatchString(vol) {
			return bad("数据卷名无效")
		}
		if mode == "overwrite" {
			if req.Confirm != vol {
				return bad("请输入数据卷名 %s 确认覆盖", vol)
			}
			holders, herr := s.runningVolumeHolders(r.Context(), vol)
			if herr != nil {
				return fmt.Errorf("检查容器挂载状态失败：%w", herr)
			}
			if len(holders) > 0 {
				return bad("数据卷 %s 正被运行中的容器挂载（%s），请先停止相关项目或容器后再覆盖恢复", vol, strings.Join(holders, ", "))
			}
		}
		plan.Volume = vol
		plan.TargetLabel = "volume/" + vol

	case "dir":
		if b.Format != "" && !backup.ValidKindFormat("dir", b.Format) {
			return bad("备份格式 %s 不是 tar 归档，无法恢复到项目目录", b.Format)
		}
		pid := req.ProjectID
		if pid == 0 {
			pid = b.Source.ProjectID
		}
		p, err := s.getProject(pid)
		if err != nil || p.Kind != "app" {
			return bad("请选择有效的目标项目")
		}
		rel := strings.TrimSpace(req.Path)
		if rel == "" {
			rel = strings.TrimSpace(req.Target)
		}
		if rel == "" && mode == "overwrite" {
			rel = b.Source.Path
		}
		rel = strings.Trim(filepath.ToSlash(filepath.Clean("/"+rel)), "/")
		if rel == "" || strings.Contains(rel, "..") || rel == ".panel" || strings.HasPrefix(rel, ".panel/") {
			return bad("目标目录无效")
		}
		if mode == "overwrite" {
			fullLabel := p.Name + "/" + rel
			if req.Confirm != fullLabel && req.Confirm != rel {
				return bad("请输入 %s 确认覆盖目录", fullLabel)
			}
			targetDir := filepath.Join(s.projectDir(p), filepath.FromSlash(rel))
			holders, herr := s.runningBindHolders(r.Context(), targetDir)
			if herr != nil {
				return fmt.Errorf("检查容器挂载状态失败：%w", herr)
			}
			if len(holders) > 0 {
				return bad("目录 %s 正被运行中的容器挂载（%s），请先停止相关项目或容器后再覆盖恢复", fullLabel, strings.Join(holders, ", "))
			}
		}
		plan.ProjectID = p.ID
		plan.Path = rel
		plan.TargetLabel = p.Name + "/" + rel

	case "panel":
		return s.handlePanelRestoreFromBackup(w, r, u, b, identities, req.MasterKey, req.DryRun, req.Confirm)

	default:
		return bad("该类型备份暂不支持恢复")
	}

	jobID := s.jobs.Submit(jobSpec("backup.restore", "恢复 "+b.Label+" → "+plan.TargetLabel, plan.TargetLabel, u.Username, "backup:"+plan.TargetLabel), func(run *jobRun) error {
		return s.runRestore(run, b, plan, identities, u.Username)
	})
	s.audit(r, u, "backup.restore", b.Label, plan.Mode+" → "+plan.TargetLabel, nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) preBackupStorageID(preferred int64) int64 {
	if preferred > 0 {
		if _, err := s.getStorageTarget(preferred); err == nil {
			return preferred
		}
	}
	var sid int64
	_ = s.db.QueryRow(`SELECT id FROM storage_targets ORDER BY id LIMIT 1`).Scan(&sid)
	return sid
}

func openVerifiedStream(ctx context.Context, st backup.Storage, b *backupRow, identities []string, peekLen int) (*bufio.Reader, io.Closer, error) {
	rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReaderSize(rc, 64<<10)
	if head, perr := br.Peek(peekLen); len(head) == 0 && perr != nil && perr != io.EOF {
		_ = rc.Close()
		return nil, nil, perr
	}
	return br, rc, nil
}

func (s *Server) runRestore(run *jobRun, b *backupRow, plan restorePlan, identities []string, actor string) error {
	ctx := run.Context()
	_, st, err := s.getStorage(b.StorageID)
	if err != nil {
		return err
	}
	run.Logf("==> 校验备份完整性")
	if b.SHA256 != "" || b.Size > 0 {
		if err := backup.Verify(ctx, st, b.ObjectKey, b.SHA256, b.Size); err != nil {
			return err
		}
	} else {
		run.Logf("（扫描导入的备份尚未记录 SHA-256，跳过哈希比对，直接进入解密解包）")
	}
	run.SetProgress(20)
	safetySID := s.preBackupStorageID(b.StorageID)

	switch plan.Kind {
	case "database":
		d, err := s.getDB(plan.InstanceID)
		if err != nil {
			return err
		}
		cl := d.client()
		if d.Engine == "redis" {
			run.Logf("==> 覆盖前先备份当前数据")
			curSrc := BackupSource{Kind: "database", InstanceID: d.ID, Database: "all"}
			if _, err := s.runBackup(run, backupRequest{Source: curSrc, StorageID: safetySID, Encrypt: b.Encrypted, Actor: actor}); err != nil {
				return fmt.Errorf("覆盖前备份失败，已中止：%w", err)
			}
			run.SetProgress(40)
			rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
			if err != nil {
				return err
			}
			defer rc.Close()
			if err := s.restoreRedis(run, d, rc); err != nil {
				return fmt.Errorf("恢复失败：%w", err)
			}
			break
		}

		dbExists := false
		if list, lerr := cl.ListDatabases(ctx); lerr == nil {
			for _, item := range list {
				if item.Name == plan.Database {
					dbExists = true
					break
				}
			}
		}

		if plan.Mode == "overwrite" && dbExists {
			run.Logf("==> 覆盖前先备份当前库 %s", plan.Database)
			curSrc := BackupSource{Kind: "database", InstanceID: d.ID, Database: plan.Database}
			if _, err := s.runBackup(run, backupRequest{Source: curSrc, StorageID: safetySID, Encrypt: b.Encrypted, Actor: actor}); err != nil {
				return fmt.Errorf("覆盖前备份失败，已中止：%w", err)
			}
		}
		run.SetProgress(35)
		// Open and verify decryption/decompression BEFORE dropping or creating target database!
		br, closer, err := openVerifiedStream(ctx, st, b, identities, 5)
		if err != nil {
			return fmt.Errorf("读取备份流失败：%w", err)
		}
		defer closer.Close()

		if plan.Mode == "overwrite" {
			if dbExists {
				if d.Engine == "mysql" || d.Engine == "mariadb" {
					if err := cl.DropDatabase(ctx, plan.Database); err != nil {
						return err
					}
					if err := cl.CreateDatabase(ctx, plan.Database, "", ""); err != nil {
						return err
					}
				}
			} else if d.Engine != "mongo" {
				run.Logf("==> 目标库 %s 尚不存在，自动创建", plan.Database)
				if err := cl.CreateDatabase(ctx, plan.Database, "", ""); err != nil {
					return err
				}
			}
		} else {
			run.Logf("==> 创建新库 %s", plan.Database)
			if d.Engine != "mongo" {
				if err := cl.CreateDatabase(ctx, plan.Database, "", ""); err != nil {
					return err
				}
			}
		}
		run.SetProgress(45)
		run.Logf("==> 导入数据到 %s", plan.TargetLabel)

		cl.Log = run
		overwrite := plan.Mode == "overwrite"
		if d.Engine == "postgres" && overwrite && !dbExists {
			overwrite = false
		}
		if err := cl.RestoreStream(ctx, plan.Database, plan.SourceDatabase, br, overwrite); err != nil {
			return fmt.Errorf("导入失败：%w", err)
		}

	case "volume":
		volExists := false
		if vols, _ := s.dk.ListVolumes(ctx); vols != nil {
			for _, v := range vols {
				if v.Name == plan.Volume {
					volExists = true
					break
				}
			}
		}
		if plan.Mode == "overwrite" {
			if !volExists {
				return fmt.Errorf("数据卷 %s 不存在，无法覆盖", plan.Volume)
			}
			holders, herr := s.runningVolumeHolders(ctx, plan.Volume)
			if herr != nil {
				return fmt.Errorf("检查容器挂载状态失败：%w", herr)
			}
			if len(holders) > 0 {
				return fmt.Errorf("数据卷 %s 正被运行中或已暂停的容器挂载（%s），请先停止相关容器", plan.Volume, strings.Join(holders, ", "))
			}
			run.Logf("==> 覆盖前先备份当前数据卷 %s", plan.Volume)
			curSrc := BackupSource{Kind: "volume", Volume: plan.Volume}
			if _, err := s.runBackup(run, backupRequest{Source: curSrc, StorageID: safetySID, Encrypt: b.Encrypted, Actor: actor}); err != nil {
				return fmt.Errorf("覆盖前备份失败，已中止：%w", err)
			}
			run.SetProgress(40)
			// Verify decryption/decompression stream BEFORE wiping live volume!
			br, closer, err := openVerifiedStream(ctx, st, b, identities, 1)
			if err != nil {
				return fmt.Errorf("读取备份归档失败，已取消清空数据卷：%w", err)
			}
			defer closer.Close()
			run.Logf("==> 清空并解包到数据卷 %s", plan.Volume)
			err = s.dk.RunContainer(ctx, docker.RunOptions{
				Image:  s.cfg.HelperImage,
				Binds:  []string{plan.Volume + ":/v"},
				Cmd:    []string{"sh", "-c", "set -e; find /v -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -C /v -xf -"},
				Stdin:  br,
				Stdout: run,
				Stderr: run,
			})
			if err != nil {
				return err
			}
		} else {
			if volExists {
				return fmt.Errorf("数据卷 %s 已存在，请换一个名字或选择覆盖模式", plan.Volume)
			}
			br, closer, err := openVerifiedStream(ctx, st, b, identities, 1)
			if err != nil {
				return fmt.Errorf("读取备份归档失败：%w", err)
			}
			defer closer.Close()
			if err := s.dk.CreateVolume(ctx, plan.Volume, map[string]string{"panel.restored-from": b.ObjectKey}); err != nil {
				return err
			}
			err = s.dk.RunContainer(ctx, docker.RunOptions{
				Image:  s.cfg.HelperImage,
				Binds:  []string{plan.Volume + ":/v"},
				Cmd:    []string{"tar", "-C", "/v", "-xf", "-"},
				Stdin:  br,
				Stdout: run,
				Stderr: run,
			})
			if err != nil {
				_ = s.dk.RemoveVolume(context.Background(), plan.Volume)
				return err
			}
		}

	case "dir":
		p, err := s.getProject(plan.ProjectID)
		if err != nil {
			return err
		}
		dir := filepath.Join(s.projectDir(p), filepath.FromSlash(plan.Path))
		_, statErr := os.Stat(dir)
		exists := statErr == nil
		if plan.Mode == "overwrite" {
			holders, herr := s.runningBindHolders(ctx, dir)
			if herr != nil {
				return fmt.Errorf("检查容器挂载状态失败：%w", herr)
			}
			if len(holders) > 0 {
				return fmt.Errorf("目录 %s 正被运行中的容器挂载（%s），请先停止相关容器", plan.TargetLabel, strings.Join(holders, ", "))
			}
			if exists {
				run.Logf("==> 覆盖前先备份当前目录 %s", plan.TargetLabel)
				curSrc := BackupSource{Kind: "dir", ProjectID: p.ID, Path: plan.Path}
				if _, err := s.runBackup(run, backupRequest{Source: curSrc, StorageID: safetySID, Encrypt: b.Encrypted, Actor: actor}); err != nil {
					return fmt.Errorf("覆盖前备份失败，已中止：%w", err)
				}
			}
			run.SetProgress(40)
			rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
			if err != nil {
				return err
			}
			defer rc.Close()
			run.Logf("==> 安全解包并原子替换目录 %s", plan.TargetLabel)
			if err := s.restoreDirOverwrite(rc, dir); err != nil {
				return err
			}
		} else {
			if exists {
				return fmt.Errorf("目录 %s 已存在，请换一个名字或选择覆盖模式", plan.Path)
			}
			rc, err := backup.OpenObject(ctx, st, b.ObjectKey, b.Encrypted, identities...)
			if err != nil {
				return err
			}
			defer rc.Close()
			if err := backup.UntarDir(rc, dir); err != nil {
				return err
			}
		}
	}
	run.Logf("==> 恢复完成：%s", plan.TargetLabel)
	return nil
}

// restoreDirOverwrite extracts rc into a sibling temporary directory and swaps it
// into dir atomically, rolling back if moving the new directory into place fails.
func (s *Server) restoreDirOverwrite(rc io.Reader, dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	tmpDir := filepath.Join(parent, ".panel-restore-tmp-"+stamp)
	oldDir := filepath.Join(parent, ".panel-restore-old-"+stamp)
	if err := backup.UntarDir(rc, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return err
	}
	hadOld := false
	if _, err := os.Stat(dir); err == nil {
		if err := os.Rename(dir, oldDir); err != nil {
			_ = os.RemoveAll(tmpDir)
			return fmt.Errorf("移走旧目录失败：%w", err)
		}
		hadOld = true
	}
	if err := os.Rename(tmpDir, dir); err != nil {
		if hadOld {
			_ = os.Rename(oldDir, dir)
		}
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("替换目标目录失败：%w", err)
	}
	if hadOld {
		_ = os.RemoveAll(oldDir)
	}
	return nil
}
