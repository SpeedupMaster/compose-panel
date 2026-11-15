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
	"sync"
	"time"

	"compose-panel/internal/auth"
)

type Manager struct {
	db  *sql.DB
	dir string
	sem chan struct{}

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	live  map[string]*Run
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
	m      *Manager
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	file    *os.File
	buf     []byte // partial line
	subs    map[chan []byte]struct{}
	done    bool
	Result  any
}

func (r *Run) Context() context.Context { return r.ctx }

// Write implements io.Writer; output is stamped per line.
func (r *Run) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	for {
		i := bytes.IndexByte(r.buf, '\n')
		if i < 0 {
			break
		}
		line := r.buf[:i]
		r.buf = r.buf[i+1:]
		r.emitLocked(line)
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

// Submit queues fn and returns the job id immediately.
func (m *Manager) Submit(spec Spec, fn Func) string {
	id := "job_" + auth.RandomToken(6)
	now := time.Now().Unix()
	_, err := m.db.Exec(`INSERT INTO jobs(id,kind,title,target,status,actor,created_at) VALUES(?,?,?,?,?,?,?)`,
		id, spec.Kind, spec.Title, spec.Target, "queued", spec.Actor, now)
	if err != nil {
		slog.Error("job insert", "err", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{ID: id, m: m, ctx: ctx, cancel: cancel, subs: map[chan []byte]struct{}{}}
	f, err := os.OpenFile(m.logPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err == nil {
		run.file = f
	}
	m.mu.Lock()
	m.live[id] = run
	m.mu.Unlock()
	go m.execute(run, spec, fn)
	return id
}

// Do runs fn synchronously as a job and waits for it.
func (m *Manager) Do(spec Spec, fn Func) (string, error) {
	id := m.Submit(spec, fn)
	_, ch, cancel, _ := m.Subscribe(id)
	defer cancel()
	for range ch {
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
	if spec.LockKey != "" {
		l := m.lockFor(spec.LockKey)
		l.Lock()
		defer l.Unlock()
	}
	select {
	case m.sem <- struct{}{}:
	case <-run.ctx.Done():
	}
	defer func() {
		select {
		case <-m.sem:
		default:
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

	m.mu.Lock()
	delete(m.live, run.ID)
	m.mu.Unlock()
}

func (m *Manager) logPath(id string) string { return filepath.Join(m.dir, filepath.Base(id)+".log") }

// Subscribe returns the log so far and a channel of new lines (closed when the job ends).
func (m *Manager) Subscribe(id string) (backlog []byte, ch <-chan []byte, cancel func(), running bool) {
	m.mu.Lock()
	run := m.live[id]
	m.mu.Unlock()
	if run == nil {
		data, _ := os.ReadFile(m.logPath(id))
		c := make(chan []byte)
		close(c)
		return data, c, func() {}, false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	data, _ := os.ReadFile(m.logPath(id))
	c := make(chan []byte, 256)
	if run.done {
		close(c)
		return data, c, func() {}, false
	}
	run.subs[c] = struct{}{}
	return data, c, func() {
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
