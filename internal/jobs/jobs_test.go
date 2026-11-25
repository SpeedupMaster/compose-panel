package jobs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compose-panel/internal/store"
)

func TestManagerTryFreezeForRestore(t *testing.T) {
	tmp := t.TempDir()
	db, err := store.Open(filepath.Join(tmp, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jm, err := New(db, filepath.Join(tmp, "jobs"), 2)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	id := jm.Submit(Spec{Kind: "test", Title: "部署 web", Target: "web", Actor: "admin"}, func(r *Run) error {
		close(started)
		<-release
		return nil
	})
	if id == "" {
		t.Fatalf("expected non-empty job id")
	}
	<-started

	// While job is running, TryFreezeForRestore must fail and report its title.
	active, ok := jm.TryFreezeForRestore()
	if ok {
		t.Fatalf("expected freeze to fail while job running")
	}
	if len(active) != 1 || active[0] != "部署 web" {
		t.Fatalf("active titles = %v, want [部署 web]", active)
	}

	// Let job finish, wait until idle.
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if active, ok = jm.TryFreezeForRestore(); ok {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("expected freeze to succeed once idle, still active=%v", active)
	}

	// While frozen, Submit must be rejected (empty id) and second TryFreezeForRestore must fail.
	if got := jm.Submit(Spec{Kind: "test", Title: "should-not-run"}, func(r *Run) error { return nil }); got != "" {
		t.Fatalf("expected Submit while frozen to return empty id, got %q", got)
	}
	if _, ok2 := jm.TryFreezeForRestore(); ok2 {
		t.Fatalf("expected second TryFreezeForRestore while already frozen to fail")
	}

	// After Unfreeze, Submit works again.
	jm.Unfreeze()
	done := make(chan struct{})
	id2 := jm.Submit(Spec{Kind: "test", Title: "after-unfreeze"}, func(r *Run) error {
		close(done)
		return nil
	})
	if id2 == "" {
		t.Fatalf("expected Submit after Unfreeze to succeed")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("job after unfreeze timed out")
	}
}

func TestManagerPruneRemovesOldFinishedJobsAndLogs(t *testing.T) {
	tmp := t.TempDir()
	db, err := store.Open(filepath.Join(tmp, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	logDir := filepath.Join(tmp, "jobs")
	jm, err := New(db, logDir, 2)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	oldTS := now.Add(-40 * 24 * time.Hour).Unix()
	recentTS := now.Add(-24 * time.Hour).Unix()

	for _, row := range []struct {
		id, status string
		ts         int64
	}{
		{"job_old_ok", "success", oldTS},
		{"job_old_run", "running", oldTS},
		{"job_recent", "success", recentTS},
	} {
		if _, err := db.Exec(`INSERT INTO jobs(id,kind,title,target,status,actor,created_at,finished_at) VALUES(?,?,?,?,?,?,?,?)`,
			row.id, "test", row.id, "t", row.status, "admin", row.ts, row.ts); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(logDir, row.id+".log"), []byte("log\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	removed := jm.Prune(30 * 24 * time.Hour)
	if removed != 1 {
		t.Fatalf("Prune() removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(logDir, "job_old_ok.log")); !os.IsNotExist(err) {
		t.Errorf("expected job_old_ok.log to be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "job_old_run.log")); err != nil {
		t.Errorf("running job log should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "job_recent.log")); err != nil {
		t.Errorf("recent job log should remain: %v", err)
	}

	var count int
	_ = db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&count)
	if count != 2 {
		t.Fatalf("remaining jobs rows = %d, want 2", count)
	}
}

func TestSubscribeStreamDeliversBacklogAndLiveLinesWithoutGaps(t *testing.T) {
	tmp := t.TempDir()
	db, err := store.Open(filepath.Join(tmp, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jm, err := New(db, filepath.Join(tmp, "jobs"), 2)
	if err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	proceed := make(chan struct{})
	id := jm.Submit(Spec{Kind: "test", Title: "stream"}, func(r *Run) error {
		r.Logf("line-1")
		r.Logf("line-2")
		close(ready)
		<-proceed
		r.Logf("line-3")
		return nil
	})
	<-ready

	logPath, offset, ch, cancel, running := jm.SubscribeStream(id)
	defer cancel()
	if !running {
		t.Fatalf("expected job to be running")
	}
	if offset <= 0 {
		t.Fatalf("expected positive backlog offset, got %d", offset)
	}
	close(proceed)

	var lines []string
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open logPath: %v", err)
	}
	sc := bufio.NewScanner(io.LimitReader(f, offset))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	f.Close()

	for b := range ch {
		lines = append(lines, strings.TrimRight(string(b), "\n"))
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"line-1", "line-2", "line-3"} {
		if strings.Count(joined, want) != 1 {
			t.Errorf("expected %q once in streamed lines, got:\n%s", want, joined)
		}
	}
}

func TestManagerCloseCancelsAndWaitsForActiveJobs(t *testing.T) {
	tmp := t.TempDir()
	db, err := store.Open(filepath.Join(tmp, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jm, err := New(db, filepath.Join(tmp, "jobs"), 2)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	var finished atomic.Bool
	id := jm.Submit(Spec{Kind: "backup.drill", Title: "long job"}, func(r *Run) error {
		close(started)
		<-r.Context().Done()
		time.Sleep(30 * time.Millisecond)
		finished.Store(true)
		return r.Context().Err()
	})
	<-started

	if !jm.HasLiveKind("backup.drill") {
		t.Fatalf("expected HasLiveKind(backup.drill) to be true while running")
	}

	jm.Close(3 * time.Second)
	if !finished.Load() {
		t.Fatalf("Close returned before active job goroutine finished")
	}
	if jm.HasLiveKind("backup.drill") {
		t.Fatalf("expected HasLiveKind(backup.drill) to be false after Close")
	}
	if got := jm.Submit(Spec{Kind: "test", Title: "after-close"}, func(r *Run) error { return nil }); got != "" {
		t.Fatalf("expected Submit after Close to return empty id, got %q", got)
	}
	var status string
	_ = db.QueryRow(`SELECT status FROM jobs WHERE id=?`, id).Scan(&status)
	if status != "canceled" {
		t.Fatalf("job status after Close = %q, want canceled", status)
	}
}

func TestRunWriteCapsPartialLineBuffer(t *testing.T) {
	tmp := t.TempDir()
	db, err := store.Open(filepath.Join(tmp, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jm, err := New(db, filepath.Join(tmp, "jobs"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer jm.Close(time.Second)

	id, err := jm.Do(Spec{Kind: "test", Title: "progress"}, func(r *Run) error {
		// 160 KiB of CR-only progress frames without any newline.
		chunk := bytes.Repeat([]byte("progress=50%\r"), 13000)
		if _, err := r.Write(chunk); err != nil {
			return err
		}
		r.mu.Lock()
		bufLen := len(r.buf)
		r.mu.Unlock()
		if bufLen >= maxPartialLine {
			return fmt.Errorf("partial buffer grew to %d >= %d", bufLen, maxPartialLine)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	data, err := jm.Log(id)
	if err != nil || len(data) == 0 {
		t.Fatalf("expected emitted log chunks, got len=%d err=%v", len(data), err)
	}
}

func TestManagerPruneBatchDeletesOver200Rows(t *testing.T) {
	tmp := t.TempDir()
	db, err := store.Open(filepath.Join(tmp, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	logDir := filepath.Join(tmp, "jobs")
	jm, err := New(db, logDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer jm.Close(time.Second)

	oldTS := time.Now().Add(-60 * 24 * time.Hour).Unix()
	for i := 0; i < 235; i++ {
		id := fmt.Sprintf("job_old_%03d", i)
		if _, err := db.Exec(`INSERT INTO jobs(id,kind,title,target,status,actor,created_at,finished_at) VALUES(?,?,?,?,?,?,?,?)`,
			id, "test", id, "t", "success", "admin", oldTS, oldTS); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(logDir, id+".log"), []byte("x\n"), 0o640)
	}

	removed := jm.Prune(30 * 24 * time.Hour)
	if removed != 235 {
		t.Fatalf("Prune removed = %d, want 235", removed)
	}
	var remaining int
	_ = db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
}
