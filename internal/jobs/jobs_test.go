package jobs

import (
	"path/filepath"
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
