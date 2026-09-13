package main

// serve 子命令：本机守护进程（GUI 的接入口；阶段 14）。
//
// 形态：一个项目一个进程，暴露 REST + SSE（internal/server 的接口面）。
// 默认只绑 127.0.0.1——GUI 是本机应用；跨机访问要自担安全面（加
// MARL_API_TOKEN 与反向代理 TLS）。
//
//	DEEPSEEK_API_KEY=sk-... marl serve -dir ~/my-task -addr 127.0.0.1:8731
//
// 端到端（curl 形态）：见 README 的"GUI / API"一节。

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/RobiNexy/Marl/internal/server"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("marl serve", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	addr := fs.String("addr", "127.0.0.1:8731", "监听地址（GUI 是本机应用——默认回环）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, ".marl", "config.yaml")); err != nil {
		return fmt.Errorf("not a Marl project (run: marl init %s): %w", root, err)
	}

	app, err := server.NewApp(root, filepath.Join(root, ".marl", "store.db"), buildVersion())
	if err != nil {
		return err
	}
	defer app.Close()

	fmt.Printf("marl serve %s\n  项目：%s\n  监听：http://%s\n  收件箱：%s/inbox/\n  事件流：GET /api/v1/events (SSE)\n",
		buildVersion(), root, *addr, app.Control)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// buildVersion 是 version 注入的汇总（-ldflags 与 VCS 戳记的合并面）。
func buildVersion() string {
	if version != "" {
		return version
	}
	rev, _ := vcsInfo()
	if rev != "" {
		return "dev (commit " + rev + ")"
	}
	return "dev"
}
