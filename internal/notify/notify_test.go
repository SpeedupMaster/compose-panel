package notify

import (
	"path/filepath"
	"testing"
	"time"

	"compose-panel/internal/secret"
	"compose-panel/internal/store"
)

func TestEmitEvictsExpiredAndSkipsNoDedupEvents(t *testing.T) {
	tmp := t.TempDir()
	db, err := store.Open(filepath.Join(tmp, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, err := secret.LoadOrCreate(filepath.Join(tmp, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	n := New(db, box)
	defer n.Close()

	// Seed stale entry older than 30m.
	n.mu.Lock()
	n.last["old_event|stale"] = time.Now().Add(-45 * time.Minute)
	n.mu.Unlock()

	// security_login and deploy_success must never accumulate in n.last.
	n.Emit(Event{Type: "security_login", Title: "fail", Target: "user|1.2.3.4"})
	n.Emit(Event{Type: "deploy_success", Title: "ok", Target: "myapp"})
	// Normal alert is tracked and triggers stale eviction.
	n.Emit(Event{Type: "backup_failed", Title: "err", Target: "db1"})

	n.mu.Lock()
	defer n.mu.Unlock()
	if _, exists := n.last["old_event|stale"]; exists {
		t.Errorf("expired dedup entry was not evicted")
	}
	if _, exists := n.last["security_login|user|1.2.3.4"]; exists {
		t.Errorf("security_login should not be stored in dedup map")
	}
	if _, exists := n.last["deploy_success|myapp"]; exists {
		t.Errorf("deploy_success should not be stored in dedup map")
	}
	if _, exists := n.last["backup_failed|db1"]; !exists {
		t.Errorf("backup_failed should be recorded in dedup map")
	}
}
