package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compose-panel/internal/dbmgr"
	"compose-panel/internal/docker"
)

const (
	maxDBImportBytes          = 10 << 30
	minImportFreeBytes uint64 = 512 << 20
)

var getHostStats = hostStats

func (s *Server) dbAndName(r *http.Request, u *User) (*dbInstance, string, error) {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return nil, "", err
	}
	db := r.URL.Query().Get("db")
	if !dbmgr.ValidIdent(db) {
		return nil, "", bad("库名无效")
	}
	return d, db, nil
}

func (s *Server) handleDBTableInfo(w http.ResponseWriter, r *http.Request, u *User) error {
	d, db, err := s.dbAndName(r, u)
	if err != nil {
		return err
	}
	table := r.PathValue("table")
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	cl := d.client()
	if err := cl.HasTable(ctx, db, table); err != nil {
		return bad("%v", err)
	}
	cols, err := cl.Columns(ctx, db, table)
	if err != nil {
		return err
	}
	idx, err := cl.Indexes(ctx, db, table)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"columns": cols, "indexes": idx})
}

func (s *Server) handleDBTableRows(w http.ResponseWriter, r *http.Request, u *User) error {
	d, db, err := s.dbAndName(r, u)
	if err != nil {
		return err
	}
	table := r.PathValue("table")
	size := min(max(queryInt(r, "size", 50), 1), 200)
	page := max(queryInt(r, "page", 1), 1)
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	cl := d.client()
	if err := cl.HasTable(ctx, db, table); err != nil {
		return bad("%v", err)
	}
	res, err := cl.TableRows(ctx, db, table, size, (page-1)*size)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"columns": res.Columns, "rows": res.Rows, "page": page, "size": size})
}

func (s *Server) handleDBSlowQueries(w http.ResponseWriter, r *http.Request, u *User) error {
	d, err := s.dbForUser(r, u)
	if err != nil {
		return err
	}
	ctx, cancel := s.dbCtx(r)
	defer cancel()
	list, hint, err := d.client().SlowQueries(ctx)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"items": nonNil(list), "hint": hint})
}

// handleDBExport streams a dump of one database as a download.
func (s *Server) handleDBExport(w http.ResponseWriter, r *http.Request, u *User) error {
	d, db, err := s.dbAndName(r, u)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	spec, format, err := d.client().ExportCmd(db, q.Get("format") == "sql")
	if err != nil {
		return err
	}
	var stderr strings.Builder
	stdout, err := s.dk.ExecStdout(r.Context(), spec.Container, docker.ExecOptions{
		Cmd:    spec.Args,
		Env:    spec.Env,
		Stderr: &stderr,
	})
	if err != nil {
		return err
	}
	defer stdout.Close()
	br := bufio.NewReaderSize(stdout, 64<<10)
	// Wait for the first byte so a failing dump returns an error instead of an empty file.
	if _, err := br.Peek(1); err != nil {
		cerr := stdout.Close()
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && cerr != nil {
			msg = cerr.Error()
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("导出失败：%s", msg)
	}
	name := fmt.Sprintf("%s-%s-%s.%s", d.Name, db, time.Now().In(s.location()).Format("20060102-150405"), format)
	gz := q.Get("gzip") == "1"
	if gz {
		name += ".gz"
	}
	attachment(w, name, -1)
	s.audit(r, u, "database.export", d.Name+"/"+db, name, nil)
	var dst io.Writer = w
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(w)
		dst = zw
	}
	_, copyErr := io.Copy(dst, br)
	closeErr := stdout.Close()
	if copyErr != nil || closeErr != nil {
		slog.Error("database export truncated", "instance", d.Name, "db", db, "copyErr", copyErr, "closeErr", closeErr, "stderr", strings.TrimSpace(stderr.String()))
		panic(http.ErrAbortHandler)
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			slog.Error("database export gzip close failed", "instance", d.Name, "db", db, "err", err)
			panic(http.ErrAbortHandler)
		}
	}
	return nil
}

// handleDBImport spools the uploaded file (.sql, .dump, optionally .gz) and
// imports it in a background job.
func (s *Server) handleDBImport(w http.ResponseWriter, r *http.Request, u *User) error {
	d, db, err := s.dbAndName(r, u)
	if err != nil {
		return err
	}
	name := filepath.Base(r.URL.Query().Get("name"))
	lower := strings.ToLower(strings.TrimSuffix(strings.ToLower(name), ".gz"))
	if !(strings.HasSuffix(lower, ".sql") || strings.HasSuffix(lower, ".dump") || strings.HasSuffix(lower, ".pgdump") || strings.HasSuffix(lower, ".backup")) {
		return bad("只支持 .sql / .dump（可再加 .gz 压缩）")
	}
	if r.ContentLength > maxDBImportBytes {
		return errStatus(http.StatusRequestEntityTooLarge, "上传文件超过大小限制（最大 10 GiB）")
	}
	if hs := getHostStats(s.cfg.DataDir); hs.DiskTotal > 0 {
		var free uint64
		if hs.DiskTotal > hs.DiskUsed {
			free = hs.DiskTotal - hs.DiskUsed
		}
		if free < minImportFreeBytes {
			return errStatus(http.StatusInsufficientStorage, "磁盘可用空间不足 512 MiB，无法上传导入文件")
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDBImportBytes)
	dir := filepath.Join(s.cfg.DataDir, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "import-*")
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r.Body)
	f.Close()
	if err != nil {
		os.Remove(f.Name())
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errStatus(http.StatusRequestEntityTooLarge, "上传文件超过大小限制（最大 10 GiB）")
		}
		return fmt.Errorf("上传中断：%w", err)
	}
	tmp := f.Name()
	gz := strings.HasSuffix(strings.ToLower(name), ".gz")
	title := fmt.Sprintf("导入 %s → %s/%s", name, d.Name, db)
	jobID := s.jobs.Submit(jobSpec("database.import", title, d.Name, u.Username, "db:"+d.Name), func(run *jobRun) error {
		defer os.Remove(tmp)
		return s.runDBImport(run, d, db, tmp, gz, n)
	})
	if jobID == "" {
		os.Remove(tmp)
		return bad("面板正在恢复，暂不接受新任务")
	}
	s.audit(r, u, "database.import", d.Name+"/"+db, name+" ("+humanBytes(n)+")", nil)
	return ok(w, map[string]any{"jobId": jobID})
}

func (s *Server) runDBImport(run *jobRun, d *dbInstance, db, file string, gz bool, size int64) error {
	ctx, cancel := context.WithTimeout(run.Context(), 2*time.Hour)
	defer cancel()
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	var rd io.Reader = f
	if gz {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("不是有效的 gzip 文件：%w", err)
		}
		defer zr.Close()
		rd = zr
	}
	if d.Engine != "postgres" && d.Engine != "mysql" && d.Engine != "mariadb" {
		return fmt.Errorf("%s 不支持在线导入", d.Engine)
	}
	br := bufio.NewReaderSize(rd, 64<<10)
	format := "sql"
	if magic, _ := br.Peek(5); string(magic) == "PGDMP" {
		format = "dump"
	}
	run.Logf("==> 文件大小 %s，格式：%s", humanBytes(size), map[string]string{"sql": "SQL 文本", "dump": "pg_dump 自定义格式"}[format])
	start := time.Now()
	cl := d.client()
	cl.Log = run
	if err := cl.RestoreStream(ctx, db, "", &progressReader{r: br, total: size, run: run, f: f}, false); err != nil {
		return fmt.Errorf("导入失败：%w", err)
	}
	run.Logf("==> 导入完成，用时 %s", time.Since(start).Round(time.Second))
	return nil
}

// progressReader reports progress based on the position in the underlying file.
type progressReader struct {
	r     io.Reader
	f     *os.File
	total int64
	run   *jobRun
	last  int
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if p.total > 0 {
		if pos, e := p.f.Seek(0, io.SeekCurrent); e == nil {
			if pct := int(pos * 100 / p.total); pct != p.last {
				p.last = pct
				p.run.SetProgress(min(pct, 99))
			}
		}
	}
	return n, err
}

// ---------- Redis restore ----------

// restoreRedis replaces the instance's dataset with an RDB snapshot. Redis is
// stopped, the RDB is loaded by a temporary server on the same volume which
// also writes a fresh AOF (instances run with appendonly yes), then the
// instance is started again.
func (s *Server) restoreRedis(run *jobRun, d *dbInstance, rdb io.Reader) error {
	ctx := run.Context()
	ci, err := s.dk.InspectContainer(ctx, d.Container)
	if err != nil {
		return err
	}
	vol := ""
	for _, m := range ci.Mounts {
		if m.Destination == "/data" && m.Type == "volume" {
			vol = m.Name
		}
	}
	if vol == "" {
		return fmt.Errorf("找不到 %s 的 /data 数据卷", d.Container)
	}
	run.Logf("==> 停止 %s", d.Container)
	if err := s.dk.ContainerAction(ctx, ci.Id, "stop"); err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			run.Logf("==> 重新启动 %s", d.Container)
			_ = s.dk.ContainerAction(context.Background(), ci.Id, "start")
		}
	}()
	script := `set -e
rm -rf /data/appendonlydir /data/appendonly.aof /data/dump.rdb
cat > /data/dump.rdb
echo "RDB 写入完成：$(wc -c < /data/dump.rdb) 字节"
redis-server --dir /data --dbfilename dump.rdb --appendonly no --save "" --port 6399 --daemonize yes --logfile /tmp/redis.log
i=0; until redis-cli -p 6399 ping 2>/dev/null | grep -q PONG; do i=$((i+1)); [ $i -gt 600 ] && { cat /tmp/redis.log; exit 1; }; sleep 0.5; done
while redis-cli -p 6399 info persistence | grep -q '^loading:1'; do sleep 0.5; done
echo "已加载：$(redis-cli -p 6399 dbsize) 个键（db0）"
redis-cli -p 6399 config set appendonly yes >/dev/null
sleep 1
while redis-cli -p 6399 info persistence | grep -Eq '^aof_rewrite_in_progress:1|^aof_rewrite_scheduled:1'; do sleep 0.5; done
redis-cli -p 6399 shutdown save >/dev/null 2>&1 || true
sleep 1
chown -R redis:redis /data 2>/dev/null || true
echo "AOF 已重建"`
	run.Logf("==> 写入 RDB 并重建 AOF（数据卷 %s，镜像 %s）", vol, ci.Config.Image)
	err = s.dk.RunContainer(ctx, docker.RunOptions{
		Image:      ci.Config.Image,
		Entrypoint: []string{"sh"},
		Binds:      []string{vol + ":/data"},
		Cmd:        []string{"-c", script},
		Stdin:      rdb,
		Stdout:     run,
		Stderr:     run,
	})
	if err != nil {
		return err
	}
	run.Logf("==> 启动 %s", d.Container)
	if err := s.dk.ContainerAction(ctx, ci.Id, "start"); err != nil {
		return err
	}
	started = true
	return nil
}
