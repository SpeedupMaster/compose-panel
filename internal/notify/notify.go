// Package notify delivers events to webhook, Telegram, email and WeCom channels.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"compose-panel/internal/secret"
)

var Events = []struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}{
	{"deploy_failed", "部署失败"},
	{"deploy_success", "部署成功"},
	{"container_unhealthy", "容器异常"},
	{"container_oom", "容器内存溢出（OOM）"},
	{"image_update", "镜像有新版本"},
	{"cron_failed", "定时任务失败"},
	{"restore_drill", "恢复演练失败"},
	{"backup_failed", "备份失败"},
	{"cert_expiring", "证书即将过期"},
	{"disk_high", "磁盘空间不足"},
	{"security_login", "安全：登录与权限"},
}

// Event is one notification.
type Event struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Target string `json:"target"` // dedup key component
	Link   string `json:"link,omitempty"`
	Time   int64  `json:"time"`
}

// ChannelConfig is the decrypted JSON in notify_channels.config.
type ChannelConfig struct {
	URL      string `json:"url,omitempty"`    // webhook / wecom
	Secret   string `json:"secret,omitempty"` // webhook HMAC
	BotToken string `json:"botToken,omitempty"`
	ChatID   string `json:"chatId,omitempty"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
}

type Notifier struct {
	db     *sql.DB
	box    *secret.Box
	http   *http.Client
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	last   map[string]time.Time
}

func New(db *sql.DB, box *secret.Box) *Notifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &Notifier{db: db, box: box, http: &http.Client{Timeout: 15 * time.Second}, ctx: ctx, cancel: cancel, last: map[string]time.Time{}}
}

// Close cancels in-flight notification sends and waits for them to finish before DB close.
func (n *Notifier) Close() {
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	n.cancel()
	n.wg.Wait()
}

// Emit sends ev to every channel with a matching rule, deduplicating repeats within 30 minutes.
func (n *Notifier) Emit(ev Event) {
	if ev.Time == 0 {
		ev.Time = time.Now().Unix()
	}
	now := time.Now()
	skipDedup := ev.Type == "security_login" || ev.Type == "deploy_success"
	key := ev.Type + "|" + ev.Target
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	for k, t := range n.last {
		if now.Sub(t) >= 30*time.Minute {
			delete(n.last, k)
		}
	}
	if !skipDedup {
		if t, ok := n.last[key]; ok && now.Sub(t) < 30*time.Minute {
			n.mu.Unlock()
			return
		}
		n.last[key] = now
	}
	n.mu.Unlock()

	rows, err := n.db.QueryContext(n.ctx, `SELECT c.id, c.kind, c.config FROM notify_rules r JOIN notify_channels c ON c.id = r.channel_id WHERE r.event = ? AND c.enabled = 1`, ev.Type)
	if err != nil {
		slog.Error("notify rules", "err", err)
		return
	}
	type ch struct {
		id         int64
		kind, conf string
	}
	var chans []ch
	for rows.Next() {
		var c ch
		if rows.Scan(&c.id, &c.kind, &c.conf) == nil {
			chans = append(chans, c)
		}
	}
	rows.Close()
	for _, c := range chans {
		n.mu.Lock()
		if n.closed {
			n.mu.Unlock()
			return
		}
		n.wg.Add(1)
		n.mu.Unlock()
		go func(c ch) {
			defer n.wg.Done()
			ctx, cancel := context.WithTimeout(n.ctx, 30*time.Second)
			defer cancel()
			err := n.SendTo(ctx, c.kind, c.conf, ev)
			if n.ctx.Err() != nil {
				return
			}
			status := "ok"
			if err != nil {
				status = err.Error()
				slog.Warn("notify send", "channel", c.id, "err", err)
			}
			_, _ = n.db.ExecContext(n.ctx, `UPDATE notify_channels SET last_sent_at=?, last_status=? WHERE id=?`, time.Now().Unix(), status, c.id)
		}(c)
	}
}

// SendTo delivers to one channel; encConf is the encrypted config.
func (n *Notifier) SendTo(ctx context.Context, kind, encConf string, ev Event) error {
	raw, err := n.box.Open(encConf)
	if err != nil {
		return err
	}
	var c ChannelConfig
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return err
	}
	return Send(ctx, n.http, kind, c, ev)
}

func text(ev Event) string {
	s := "【" + ev.Title + "】"
	if ev.Body != "" {
		s += "\n" + ev.Body
	}
	if ev.Link != "" {
		s += "\n" + ev.Link
	}
	return s
}

func Send(ctx context.Context, hc *http.Client, kind string, c ChannelConfig, ev Event) error {
	switch kind {
	case "webhook":
		body, _ := json.Marshal(ev)
		req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "compose-panel")
		if c.Secret != "" {
			m := hmac.New(sha256.New, []byte(c.Secret))
			m.Write(body)
			req.Header.Set("X-Panel-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		return doHTTP(hc, req)
	case "telegram":
		body, _ := json.Marshal(map[string]any{"chat_id": c.ChatID, "text": text(ev), "disable_web_page_preview": true})
		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+c.BotToken+"/sendMessage", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		return doHTTP(hc, req)
	case "wecom":
		body, _ := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]any{"content": text(ev)}})
		req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		return doHTTP(hc, req)
	case "email":
		return sendMail(c, ev)
	case "dingtalk", "feishu", "bark":
		return sendIM(ctx, hc, kind, c, ev)
	}
	return fmt.Errorf("unknown channel kind %q", kind)
}

func doHTTP(hc *http.Client, req *http.Request) error {
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var b bytes.Buffer
		_, _ = b.ReadFrom(http.MaxBytesReader(nil, resp.Body, 2000))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(b.String()))
	}
	return nil
}

func sendMail(c ChannelConfig, ev Event) error {
	if c.Port == 0 {
		c.Port = 465
	}
	addr := net.JoinHostPort(c.Host, fmt.Sprint(c.Port))
	from := c.From
	if from == "" {
		from = c.Username
	}
	to := []string{}
	for _, t := range strings.Split(c.To, ",") {
		if t = strings.TrimSpace(t); t != "" {
			to = append(to, t)
		}
	}
	if len(to) == 0 {
		return fmt.Errorf("收件人不能为空")
	}
	subject := "=?UTF-8?B?" + b64(ev.Title) + "?="
	msg := "From: " + from + "\r\nTo: " + strings.Join(to, ", ") + "\r\nSubject: " + subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64(text(ev))

	var conn net.Conn
	var err error
	d := &net.Dialer{Timeout: 15 * time.Second}
	if c.Port == 465 {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: c.Host})
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	if c.Port != 465 {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
				return err
			}
		}
	}
	if c.Username != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(from); err != nil {
		return err
	}
	for _, t := range to {
		if err := cl.Rcpt(t); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
