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
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/server"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("marl serve", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	addr := fs.String("addr", "127.0.0.1:8731", "监听地址（GUI 是本机应用——默认回环）")
	task := fs.String("task", "", "启动后立即跑的任务（start --detach 的载体；空 = 只服务等任务）")
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
	defer func() { _ = app.Close() }()

	// serve.lock：pid + addr（外部宿主的"daemon 在吗"判据与 CLI 的接入口）。
	controlRoot := server.ControlRootOf(root)
	_ = os.MkdirAll(controlRoot, 0o755)
	lockPath := filepath.Join(controlRoot, "serve.lock")
	if werr := os.WriteFile(lockPath, []byte(fmt.Sprintf("pid: %d\naddr: %s\nstarted_at: %s\n",
		os.Getpid(), *addr, time.Now().UTC().Format(time.RFC3339))), 0o644); werr != nil {
		return werr
	}
	defer os.Remove(lockPath)

	fmt.Printf("marl serve %s\n  项目：%s\n  监听：http://%s\n  收件箱：%s/inbox/\n  事件流：GET /api/v1/events (SSE)\n",
		buildVersion(), root, *addr, controlRoot)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// /api/v1/shutdown 的退出钩子（优雅关停：先回响应再 Shutdown）。
	done := make(chan struct{})
	app.OnShutdown(func() {
		go func() {
			_ = srv.Shutdown(context.Background())
			close(done)
		}()
	})
	// -task：启动后立即跑一个任务（start --detach 的载体）。
	if strings.TrimSpace(*task) != "" {
		if _, terr := app.StartTask(*task); terr != nil {
			fmt.Fprintf(os.Stderr, "start task: %v\n", terr)
		}
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return nil
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
