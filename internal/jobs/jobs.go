// Package jobs runs long operations in the background with persisted status and streamed logs.
package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"compose-panel/internal/auth"
)

const maxPartialLine = 64 << 10

type Manager struct {
	db  *sql.DB
	dir string
	sem chan struct{}
	wg  sync.WaitGroup

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	live   map[string]*Run
	frozen bool
}

func New(db *sql.DB, dir string, concurrency int) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	// Anything left running from a previous process was interrupted.
	_, _ = db.Exec(`UPDATE jobs SET status='failed', error='面板重启，任务被中断', finished_at=? WHERE status IN ('queued','running')`, time.Now().Unix())
	return &Manager{db: db, dir: dir, sem: make(chan struct{}, concurrency), locks: map[string]*sync.Mutex{}, live: map[string]*Run{}}, nil
}

// Run is the handle a job function uses to report progress and log output.
type Run struct {
	ID     string
	kind   string
	title  string
	m      *Manager
	ctx    context.Context
	cancel context.CancelFunc
	doneCh chan struct{}

	mu     sync.Mutex
	file   *os.File
	buf    []byte // partial line
	subs   map[chan []byte]struct{}
	done   bool
	Result any
}

func (r *Run) Context() context.Context { return r.ctx }

// Write implements io.Writer; output is stamped per line.
func (r *Run) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	for {
		i := bytes.IndexByte(r.buf, '\n')
		if i >= 0 {
			line := r.buf[:i]
			r.buf = r.buf[i+1:]
			r.emitLocked(line)
			continue
		}
		if len(r.buf) >= maxPartialLine {
			line := r.buf[:maxPartialLine]
			r.buf = r.buf[maxPartialLine:]
			r.emitLocked(line)
			continue
		}
		break
	}
	if len(r.buf) == 0 {
		if cap(r.buf) > maxPartialLine {
			r.buf = nil
		} else {
			r.buf = r.buf[:0]
		}
	} else if cap(r.buf) > maxPartialLine*2 && len(r.buf) < maxPartialLine {
		cp := make([]byte, len(r.buf))
		copy(cp, r.buf)
		r.buf = cp
	}
	return len(p), nil
}

func (r *Run) emitLocked(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if i := bytes.LastIndexByte(line, '\r'); i >= 0 { // progress bars: keep the last frame
		line = line[i+1:]
	}
	out := []byte(time.Now().Format("15:04:05") + "  ")
	out = append(out, line...)
	out = append(out, '\n')
	if r.file != nil {
		_, _ = r.file.Write(out)
	}
	for ch := range r.subs {
		select {
		case ch <- out:
		default:
		}
	}
}

func (r *Run) Logf(format string, a ...any) {
	fmt.Fprintf(r, format+"\n", a...)
}

func (r *Run) SetProgress(p int) {
	_, _ = r.m.db.Exec(`UPDATE jobs SET progress=? WHERE id=?`, p, r.ID)
}

type Spec struct {
	Kind    string
	Title   string
	Target  string
	Actor   string
	LockKey string // jobs with the same key run one after another
}

type Func func(r *Run) error

// TryFreezeForRestore checks that no background jobs are active and freezes
// new submissions so panel.db can be safely closed and swapped.
func (m *Manager) TryFreezeForRestore() ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.frozen {
		return []string{"面板恢复正在进行中"}, false
	}
	if len(m.live) > 0 {
		titles := make([]string, 0, len(m.live))
		for _, r := range m.live {
			if r.title != "" {
				titles = append(titles, r.title)
			} else {
				titles = append(titles, r.ID)
			}
		}
		sort.Strings(titles)
		return titles, false
	}
	m.frozen = true
	return nil, true
}

// Unfreeze re-enables job submissions (used if a restore fails before restart).
func (m *Manager) Unfreeze() {
	m.mu.Lock()
	m.frozen = false
	m.mu.Unlock()
}

// HasLiveKind reports whether any currently queued or running job has the given kind.
func (m *Manager) HasLiveKind(kind string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.live {
		if r.kind == kind {
			return true
		}
	}
	return false
}

// Close freezes new submissions, cancels all active jobs, and waits up to timeout
// for running job goroutines to finish before shutting down the database.
func (m *Manager) Close(timeout time.Duration) {
	m.mu.Lock()
	m.frozen = true
	cancels := make([]context.CancelFunc, 0, len(m.live))
	for _, r := range m.live {
		cancels = append(cancels, r.cancel)
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		slog.Warn("jobs shutdown timed out with active jobs remaining")
	}
}

// Submit queues fn and returns the job id immediately, or "" if the manager is frozen.
func (m *Manager) Submit(spec Spec, fn Func) string {
	id := "job_" + auth.RandomToken(6)
	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{ID: id, kind: spec.Kind, title: spec.Title, m: m, ctx: ctx, cancel: cancel, doneCh: make(chan struct{}), subs: map[chan []byte]struct{}{}}
	m.mu.Lock()
	if m.frozen {
		m.mu.Unlock()
		cancel()
		return ""
	}
	m.live[id] = run
	m.wg.Add(1)
	m.mu.Unlock()

	now := time.Now().Unix()
	_, err := m.db.Exec(`INSERT INTO jobs(id,kind,title,target,status,actor,created_at) VALUES(?,?,?,?,?,?,?)`,
		id, spec.Kind, spec.Title, spec.Target, "queued", spec.Actor, now)
	if err != nil {
		slog.Error("job insert", "err", err)
	}
	f, err := os.OpenFile(m.logPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err == nil {
		run.file = f
	}
	go m.execute(run, spec, fn)
	return id
}

// Do runs fn synchronously as a job and waits for it.
func (m *Manager) Do(spec Spec, fn Func) (string, error) {
	id := m.Submit(spec, fn)
	if id == "" {
		return "", fmt.Errorf("面板正在恢复，暂不接受新任务")
	}
	m.mu.Lock()
	run := m.live[id]
	m.mu.Unlock()
	if run != nil {
		<-run.doneCh
	}
	var status, errMsg string
	_ = m.db.QueryRow(`SELECT status, error FROM jobs WHERE id=?`, id).Scan(&status, &errMsg)
	if status != "success" {
		return id, fmt.Errorf("%s", errMsg)
	}
	return id, nil
}

func (m *Manager) lockFor(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	return l
}

func (m *Manager) execute(run *Run, spec Spec, fn Func) {
	defer m.wg.Done()
	if spec.LockKey != "" {
		l := m.lockFor(spec.LockKey)
		l.Lock()
		defer l.Unlock()
	}
	acquired := false
	select {
	case m.sem <- struct{}{}:
		acquired = true
	case <-run.ctx.Done():
	}
	defer func() {
		if acquired {
			<-m.sem
		}
	}()

	status, errMsg := "success", ""
	if run.ctx.Err() != nil {
		status, errMsg = "canceled", "已取消"
	} else {
		_, _ = m.db.Exec(`UPDATE jobs SET status='running', started_at=? WHERE id=?`, time.Now().Unix(), run.ID)
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panic: %v", p)
				}
			}()
			return fn(run)
		}()
		if err != nil {
			status, errMsg = "failed", err.Error()
			if run.ctx.Err() != nil {
				status, errMsg = "canceled", "已取消"
			}
			run.Logf("错误：%s", errMsg)
		}
	}
	_, _ = m.db.Exec(`UPDATE jobs SET status=?, error=?, finished_at=?, progress=CASE WHEN ?='success' THEN 100 ELSE progress END WHERE id=?`,
		status, errMsg, time.Now().Unix(), status, run.ID)

	run.mu.Lock()
	if len(run.buf) > 0 {
		run.emitLocked(run.buf)
		run.buf = nil
	}
	run.done = true
	for ch := range run.subs {
		close(ch)
	}
	run.subs = nil
	if run.file != nil {
		run.file.Close()
	}
	run.mu.Unlock()
	run.cancel()
	close(run.doneCh)

	m.mu.Lock()
	delete(m.live, run.ID)
	m.mu.Unlock()
}

func (m *Manager) logPath(id string) string { return filepath.Join(m.dir, filepath.Base(id)+".log") }

// SubscribeStream returns the log file path, the byte offset already flushed to disk,
// and a live channel of lines emitted after that offset (closed when the job ends).
// Callers can stream the first offset bytes from logPath without loading the entire file into memory.
func (m *Manager) SubscribeStream(id string) (logPath string, offset int64, ch <-chan []byte, cancel func(), running bool) {
	logPath = m.logPath(id)
	m.mu.Lock()
	run := m.live[id]
	m.mu.Unlock()
	if run == nil {
		if fi, err := os.Stat(logPath); err == nil {
			offset = fi.Size()
		}
		c := make(chan []byte)
		close(c)
		return logPath, offset, c, func() {}, false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.file != nil {
		if fi, err := run.file.Stat(); err == nil {
			offset = fi.Size()
		}
	} else if fi, err := os.Stat(logPath); err == nil {
		offset = fi.Size()
	}
	c := make(chan []byte, 256)
	if run.done {
		close(c)
		return logPath, offset, c, func() {}, false
	}
	run.subs[c] = struct{}{}
	return logPath, offset, c, func() {
		run.mu.Lock()
		if run.subs != nil {
			if _, ok := run.subs[c]; ok {
				delete(run.subs, c)
				close(c)
			}
		}
		run.mu.Unlock()
	}, true
}

// Subscribe returns the log so far and a channel of new lines (closed when the job ends).
func (m *Manager) Subscribe(id string) (backlog []byte, ch <-chan []byte, cancel func(), running bool) {
	logPath, offset, ch, cancel, running := m.SubscribeStream(id)
	if offset > 0 {
		if f, err := os.Open(logPath); err == nil {
			backlog, _ = io.ReadAll(io.LimitReader(f, offset))
			f.Close()
		}
	}
	return backlog, ch, cancel, running
}

func (m *Manager) Log(id string) ([]byte, error) { return os.ReadFile(m.logPath(id)) }

func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	run := m.live[id]
	m.mu.Unlock()
	if run == nil {
		return false
	}
	run.cancel()
	return true
}

// Tee returns a writer that writes to the job log and w.
func (r *Run) Tee(w io.Writer) io.Writer { return io.MultiWriter(r, w) }

// Prune removes completed jobs older than maxAge along with their log files.
func (m *Manager) Prune(maxAge time.Duration) int {
	if maxAge <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-maxAge).Unix()
	rows, err := m.db.Query(`SELECT id FROM jobs WHERE status NOT IN ('queued','running') AND created_at < ?`, cutoff)
	if err != nil {
		return 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return 0
	}
	tx, err := m.db.Begin()
	if err != nil {
		return 0
	}
	defer tx.Rollback()
	const batchSize = 200
	var deleted []string
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		batch := ids[start:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		if _, err := tx.Exec(`DELETE FROM jobs WHERE status NOT IN ('queued','running') AND id IN (`+placeholders+`)`, args...); err != nil {
			return 0
		}
		deleted = append(deleted, batch...)
	}
	if err := tx.Commit(); err != nil {
		return 0
	}
	for _, id := range deleted {
		_ = os.Remove(m.logPath(id))
	}
	return len(deleted)
}
