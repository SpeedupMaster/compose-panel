package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"compose-panel/internal/secret"
	"compose-panel/internal/store"
)

func TestEmitEvictsExpiredAndDedupsOnlyAfterSuccess(t *testing.T) {
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

	var calls atomic.Int32
	var failNext atomic.Bool
	failNext.Store(true)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failNext.Load() {
			http.Error(w, "temporary failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	cfgBytes, _ := json.Marshal(ChannelConfig{URL: hook.URL})
	res, err := db.Exec(`INSERT INTO notify_channels(name,kind,config,enabled,created_at) VALUES('wh','webhook',?,1,?)`,
		box.Seal(string(cfgBytes)), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := res.LastInsertId()
	for _, ev := range []string{"backup_failed", "security_login", "deploy_success"} {
		if _, err := db.Exec(`INSERT INTO notify_rules(event,channel_id) VALUES(?,?)`, ev, cid); err != nil {
			t.Fatal(err)
		}
	}

	n := New(db, box)
	defer n.Close()

	// Seed stale entry older than 30m.
	n.mu.Lock()
	n.last["old_event|stale"] = time.Now().Add(-45 * time.Minute)
	n.mu.Unlock()

	// 1. First backup_failed fails (HTTP 500) -> must NOT record dedup entry.
	n.Emit(Event{Type: "backup_failed", Title: "err", Target: "db1"})
	n.wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls after failed emit = %d, want 1", calls.Load())
	}
	n.mu.Lock()
	_, existsAfterFail := n.last["backup_failed|db1"]
	_, staleExists := n.last["old_event|stale"]
	n.mu.Unlock()
	if staleExists {
		t.Errorf("expired dedup entry was not evicted")
	}
	if existsAfterFail {
		t.Fatalf("failed delivery must not suppress retries in dedup map")
	}

	// 2. Second backup_failed succeeds -> MUST be delivered and then recorded in dedup map.
	failNext.Store(false)
	n.Emit(Event{Type: "backup_failed", Title: "err", Target: "db1"})
	n.wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("calls after retry emit = %d, want 2", calls.Load())
	}

	// 3. Third backup_failed within 30m -> suppressed by dedup.
	n.Emit(Event{Type: "backup_failed", Title: "err", Target: "db1"})
	n.wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("calls after deduped emit = %d, want 2", calls.Load())
	}

	// 4. security_login and deploy_success must never accumulate in n.last.
	n.Emit(Event{Type: "security_login", Title: "fail", Target: "user|1.2.3.4"})
	n.Emit(Event{Type: "deploy_success", Title: "ok", Target: "myapp"})
	n.wg.Wait()

	n.mu.Lock()
	defer n.mu.Unlock()
	if _, exists := n.last["security_login|user|1.2.3.4"]; exists {
		t.Errorf("security_login should not be stored in dedup map")
	}
	if _, exists := n.last["deploy_success|myapp"]; exists {
		t.Errorf("deploy_success should not be stored in dedup map")
	}
	if _, exists := n.last["backup_failed|db1"]; !exists {
		t.Errorf("backup_failed should be recorded in dedup map after successful delivery")
	}
}

func TestSendMailRespectsContextAndSanitizesHeaders(t *testing.T) {
	// 1. Stalled SMTP server (accepts TCP, never sends greeting) must abort promptly on ctx timeout.
	stalledLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stalledLn.Close()
	go func() {
		conn, err := stalledLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(10 * time.Second)
	}()
	stalledAddr := stalledLn.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = Send(ctx, &http.Client{}, "email", ChannelConfig{
		Host: "127.0.0.1",
		Port: stalledAddr.Port,
		From: "panel@example.com",
		To:   "ops@example.com",
	}, Event{Title: "test", Body: "body"})
	if err == nil {
		t.Fatalf("expected error from stalled SMTP server")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("stalled SMTP took %v, expected prompt cancellation", time.Since(start))
	}

	// 2. Minimal mock SMTP server to inspect DATA payload for CRLF header injection stripping.
	var mu sync.Mutex
	var capturedData string
	smtpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer smtpLn.Close()
	go func() {
		conn, err := smtpLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		_, _ = conn.Write([]byte("220 mock.local ESMTP\r\n"))
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			upper := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
				_, _ = conn.Write([]byte("250 mock.local\r\n"))
			case strings.HasPrefix(upper, "MAIL FROM:"), strings.HasPrefix(upper, "RCPT TO:"):
				_, _ = conn.Write([]byte("250 OK\r\n"))
			case upper == "DATA":
				_, _ = conn.Write([]byte("354 Go ahead\r\n"))
				var sb strings.Builder
				for {
					dl, err := br.ReadString('\n')
					if err != nil || dl == ".\r\n" {
						break
					}
					sb.WriteString(dl)
				}
				mu.Lock()
				capturedData = sb.String()
				mu.Unlock()
				_, _ = conn.Write([]byte("250 Queued\r\n"))
			case upper == "QUIT":
				_, _ = conn.Write([]byte("221 Bye\r\n"))
				return
			default:
				_, _ = conn.Write([]byte("250 OK\r\n"))
			}
		}
	}()
	smtpAddr := smtpLn.Addr().(*net.TCPAddr)
	err = Send(context.Background(), &http.Client{}, "email", ChannelConfig{
		Host: "127.0.0.1",
		Port: smtpAddr.Port,
		From: "panel@example.com\r\nBcc: evil@attacker.com",
		To:   "ops@example.com\r\nX-Evil: 1",
	}, Event{Title: "Alert\r\nBcc: evil2@attacker.com", Body: "hello"})
	if err != nil {
		t.Fatalf("Send(email) to mock SMTP failed: %v", err)
	}
	mu.Lock()
	data := capturedData
	mu.Unlock()
	headerBlock, _, _ := strings.Cut(data, "\r\n\r\n")
	for _, line := range strings.Split(headerBlock, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") || strings.HasPrefix(strings.ToLower(line), "x-evil:") {
			t.Fatalf("SMTP header injection not stripped: %q\nfull headers:\n%s", line, headerBlock)
		}
	}
}
