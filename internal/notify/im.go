package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// sendIM delivers to DingTalk / Feishu (Lark) group robots and Bark.
func sendIM(ctx context.Context, hc *http.Client, kind string, c ChannelConfig, ev Event) error {
	target := c.URL
	var body any
	switch kind {
	case "dingtalk":
		// Optional "加签": sign = base64(HMAC-SHA256(secret, timestamp+"\n"+secret)).
		if c.Secret != "" {
			ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
			m := hmac.New(sha256.New, []byte(c.Secret))
			m.Write([]byte(ts + "\n" + c.Secret))
			sep := "?"
			if strings.Contains(target, "?") {
				sep = "&"
			}
			target += sep + "timestamp=" + ts + "&sign=" + url.QueryEscape(base64.StdEncoding.EncodeToString(m.Sum(nil)))
		}
		body = map[string]any{"msgtype": "text", "text": map[string]any{"content": text(ev)}}
	case "feishu":
		msg := map[string]any{"msg_type": "text", "content": map[string]any{"text": text(ev)}}
		// Optional signature: base64(HMAC-SHA256(key=timestamp+"\n"+secret, msg="")).
		if c.Secret != "" {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			m := hmac.New(sha256.New, []byte(ts+"\n"+c.Secret))
			msg["timestamp"] = ts
			msg["sign"] = base64.StdEncoding.EncodeToString(m.Sum(nil))
		}
		body = msg
	case "bark":
		// URL is the device URL, e.g. https://api.day.app/<key>
		target = strings.TrimRight(target, "/")
		b := map[string]any{"title": ev.Title, "body": ev.Body, "group": "compose-panel"}
		if ev.Type == "deploy_failed" || ev.Type == "container_oom" || ev.Type == "disk_high" {
			b["level"] = "timeSensitive"
		}
		body = b
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4000))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	// These APIs answer 200 with an error code in the body.
	var r struct {
		ErrCode *int   `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Code    *int   `json:"code"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &r) == nil {
		switch {
		case r.ErrCode != nil && *r.ErrCode != 0:
			return fmt.Errorf("钉钉返回错误 %d：%s", *r.ErrCode, r.ErrMsg)
		case kind == "feishu" && r.Code != nil && *r.Code != 0:
			return fmt.Errorf("飞书返回错误 %d：%s", *r.Code, r.Msg)
		case kind == "bark" && r.Code != nil && *r.Code != 200:
			return fmt.Errorf("Bark 返回错误 %d：%s", *r.Code, r.Message)
		}
	}
	return nil
}
