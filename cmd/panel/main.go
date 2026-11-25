// Command panel runs the Compose Panel server.
package main

import (
	"context"
	"flag"
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
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		for {
			srv, err := server.New(cfg, web.FS())
			if err != nil {
				fatal(err)
			}
			if err := srv.Run(ctx); err != nil {
				fatal(err)
			}
			if srv.RestartRequested() && ctx.Err() == nil {
				slog.Info("面板数据已恢复，正在重新加载服务")
				continue
			}
			break
		}
	case "reset-password":
		if len(os.Args) < 3 {
			fatal(fmt.Errorf("用法：panel reset-password <用户名>"))
		}
		srv, err := server.New(cfg, web.FS())
		if err != nil {
			fatal(err)
		}
		defer srv.DB().Close()
		pw := auth.RandomPassword(16)
		hash, _ := auth.HashPassword(pw)
		res, err := srv.DB().Exec(`UPDATE users SET password_hash=?, totp_enabled=0, totp_secret='', last_totp_step=0, recovery_codes='', disabled=0 WHERE username=?`, hash, os.Args[2])
		if err != nil {
			fatal(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fatal(fmt.Errorf("用户 %s 不存在", os.Args[2]))
		}
		_, _ = srv.DB().Exec(`DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE username=?)`, os.Args[2])
		srv.AuditCLI("user.reset_password", os.Args[2], "CLI 重置密码并关闭两步验证")
		fmt.Printf("已重置 %s 的密码并关闭两步验证。新密码：%s\n", os.Args[2], pw)
	case "restore-panel":
		fs := flag.NewFlagSet("restore-panel", flag.ExitOnError)
		masterKey := fs.String("master-key", "", "原服务器的 master.key（64 位十六进制或文件路径）")
		ageKey := fs.String("age-key", "", "加密备份使用的 age 私钥（AGE-SECRET-KEY-1... 或文件路径）")
		_ = fs.Parse(os.Args[2:])
		if fs.NArg() < 1 {
			fatal(fmt.Errorf("用法：panel restore-panel [--master-key <hex|文件>] [--age-key <私钥|文件>] <备份文件>"))
		}
		if err := server.RestorePanelCLI(cfg, fs.Arg(0), *masterKey, *ageKey); err != nil {
			fatal(err)
		}
		fmt.Println("面板数据恢复成功，请启动或重启面板进程。")
	case "version":
		fmt.Println(server.Version)
	default:
		fatal(fmt.Errorf("未知命令 %q（可用：serve, reset-password, restore-panel, version）", cmd))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误：", err)
	os.Exit(1)
}
