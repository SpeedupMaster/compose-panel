package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
)

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

func errStatus(status int, format string, a ...any) error {
	return &apiError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

func bad(format string, a ...any) error { return errStatus(400, format, a...) }

var (
	errNotFound  = &apiError{404, "未找到"}
	errForbidden = &apiError{403, "权限不足"}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, v any) error {
	if v == nil {
		v = map[string]any{"ok": true}
	}
	writeJSON(w, 200, v)
	return nil
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, map[string]any{"error": ae.Msg})
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 404, map[string]any{"error": "未找到"})
		return
	}
	slog.Warn("request failed", "err", err)
	writeJSON(w, 500, map[string]any{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return bad("请求格式错误：%v", err)
	}
	return nil
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		return 0, errNotFound
	}
	return id, nil
}

func queryInt(r *http.Request, key string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil {
		return v
	}
	return def
}

func isTrustedProxyAddr(remoteAddr string) bool {
	remote := strings.TrimSpace(remoteAddr)
	if remote == "" || remote == "@" {
		return true
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if i := strings.IndexByte(remote, '%'); i >= 0 {
		remote = remote[:i]
	}
	ip := net.ParseIP(remote)
	if ip == nil {
		// Unix domain socket path or non-IP local transport.
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

func clientIP(r *http.Request) string {
	remote := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if isTrustedProxyAddr(r.RemoteAddr) {
		// Caddy reverse_proxy appends the immediate peer IP to the rightmost
		// position of X-Forwarded-For; scan right-to-left so client-forged
		// leading entries cannot spoof the caller IP.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			var loopbackFallback string
			for i := len(parts) - 1; i >= 0; i-- {
				cand := strings.TrimSpace(parts[i])
				if cand == "" {
					continue
				}
				if ip := net.ParseIP(cand); ip != nil {
					if !ip.IsLoopback() {
						return ip.String()
					}
					if loopbackFallback == "" {
						loopbackFallback = ip.String()
					}
				}
			}
			if loopbackFallback != "" {
				return loopbackFallback
			}
		}
		if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
			if ip := net.ParseIP(xri); ip != nil {
				return ip.String()
			}
		}
	}
	return remote
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// done writes {"ok":true} unless err is set.
func done(w http.ResponseWriter, err error) error {
	if err != nil {
		return err
	}
	return ok(w, nil)
}
