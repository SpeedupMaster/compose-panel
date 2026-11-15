// Command panel runs the Compose Panel server.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	"compose-panel/internal/auth"
	"compose-panel/internal/config"
	"compose-panel/internal/server"
	"compose-panel/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg := config.Load()
	switch cmd {
	case "serve":
		srv, err := server.New(cfg, web.FS())
		if err != nil {
			fatal(err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := srv.Run(ctx); err != nil {
			fatal(err)
		}
	case "reset-password":
		if len(os.Args) < 3 {
			fatal(fmt.Errorf("用法：panel reset-password <用户名>"))
		}
		srv, err := server.New(cfg, web.FS())
		if err != nil {
			fatal(err)
		}
		pw := auth.RandomPassword(16)
		hash, _ := auth.HashPassword(pw)
		res, err := srv.DB().Exec(`UPDATE users SET password_hash=?, totp_enabled=0, totp_secret='', recovery_codes='', disabled=0 WHERE username=?`, hash, os.Args[2])
		if err != nil {
			fatal(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fatal(fmt.Errorf("用户 %s 不存在", os.Args[2]))
		}
		_, _ = srv.DB().Exec(`DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE username=?)`, os.Args[2])
		fmt.Printf("已重置 %s 的密码并关闭两步验证。新密码：%s\n", os.Args[2], pw)
	case "version":
		fmt.Println(server.Version)
	default:
		fatal(fmt.Errorf("未知命令 %q（可用：serve, reset-password, version）", cmd))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误：", err)
	os.Exit(1)
}
