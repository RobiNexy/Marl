// start/say/stop：统一 Actor 模型的人类侧命令面（Part 14.6 / 阶段 13）。
//
//	marl start "任务"            —— 人类 spawn 项目 Agent（attached 等完成）。
//	marl start --detach "任务"   —— 后台运行（setsid；日志与 PID 落控制面）。
//	marl stop                    —— 优雅停止后台任务（超时转强杀）。
//	marl say "文本"              —— 人类发给项目 Actor 的 MsgDirect（跨进程
//	                              文件通道；运行中的任务下一轮看到）。
//
// 装配的单一实现在 internal/server（App；GUI 服务面共用）——本文件只剩
// CLI 的形态面：参数解析 / detach 的进程包装 / 等待循环。
//
// [运行纪律] 不要把 stdout/stderr 重定向进项目目录——工作区是 Agent 的
// 命名空间，模型会读它（真机实录：读 start.log → 绝对路径进工具调用 →
// 级联失败）；日志的归宿是控制面（~/.local/state/marl/<project>/）。

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RobiNexy/Marl/internal/server"
)

// osUID 取 OS 用户标识（人类 From 的推导源——能写控制面文件的进程就是
// 同 UID 的人类侧入口）。
func osUID() string {
	if u := os.Getenv("UID"); u != "" {
		return u
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// frontmatterStripped 剥掉 frontmatter（人类收件箱的正文显示形态）。
func frontmatterStripped(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	if i := strings.Index(s[4:], "\n---\n"); i >= 0 {
		return strings.TrimSpace(s[4+i+5:])
	}
	return s
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("marl start", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	db := fs.String("db", "", "存储数据库路径（缺省 <dir>/.marl/store.db）")
	detach := fs.Bool("detach", false, "后台运行（日志落控制面；marl stop 停止 / marl status 跟踪）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	task := strings.Join(fs.Args(), " ")
	if task == "" {
		return fmt.Errorf("usage: marl start [-dir <dir>] \"任务描述\"")
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if *db == "" {
		*db = filepath.Join(root, ".marl", "store.db")
	}
	if *detach {
		return detachStart(root, *db, task)
	}
	return runStart(root, *db, task)
}

// detachStart 把 serve 守护进程放进后台（setsid 新会话；任务经 serve 的
// -task 载体立即启动）。此后的宿主操作（say/stop/status）都是 HTTPClient
// ——CLI 与 GUI 同一条契约路径。serve.lock 是"daemon 在吗"的判据
// （pid + addr）。
func detachStart(root, dbPath, task string) error {
	controlRoot := server.ControlRootOf(root)
	lockPath := filepath.Join(controlRoot, "serve.lock")
	if pid, addr, ok := readServeLock(lockPath); ok && processAlive(pid) {
		// daemon 已在：直接经 HTTP 启动任务（第二个任务被 409 拒绝）。
		client := server.NewHTTPClient(addr)
		id, err := client.StartTask(task)
		if err != nil {
			return err
		}
		fmt.Printf("任务已在运行中的守护进程启动（PID %d，agent %s）\n", pid, id)
		return nil
	}
	if err := os.MkdirAll(controlRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir control plane: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(controlRoot, "serve.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open control-plane log: %w", err)
	}
	defer logFile.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "serve", "-dir", root, "-task", task)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	fmt.Printf("后台任务已启动（PID %d）\n  日志：%s\n  跟踪：marl status；插话：marl say；停止：marl stop\n", cmd.Process.Pid, controlRoot)
	return nil
}

// readServeLock 读 serve.lock 的 pid + addr。
func readServeLock(path string) (int, string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, "", false
	}
	pid, addr := 0, ""
	for _, ln := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "pid:"); ok {
			if p, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil {
				pid = p
			}
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "addr:"); ok {
			addr = strings.TrimSpace(v)
		}
	}
	if pid > 0 && addr != "" {
		return pid, addr, true
	}
	return 0, "", false
}

// resolveDaemon 读 serve.lock 并确认进程存活（宿主选择的"daemon 在吗"面）。
func resolveDaemon(root string) (server.InteractionClient, bool) {
	pid, addr, ok := readServeLock(filepath.Join(server.ControlRootOf(root), "serve.lock"))
	if !ok || !processAlive(pid) {
		return nil, false
	}
	return server.NewHTTPClient(addr), true
}

// cmdStop 停止后台任务/守护进程（经契约的 HTTPClient；daemon 不在时
// 清理陈旧锁）。
func cmdStop(args []string) error {
	fs := flag.NewFlagSet("marl stop", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	force := fs.Bool("force", false, "跳过优雅等待直接强杀（任务取消面）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := filepathAbs(*dir)
	controlRoot := server.ControlRootOf(root)
	client, ok := resolveDaemon(root)
	if !ok {
		_ = os.Remove(filepath.Join(controlRoot, "serve.lock"))
		return fmt.Errorf("没有在跑的守护进程（控制面 %s）", controlRoot)
	}
	run := client.CurrentRun()
	if run.Active {
		if err := client.StopTask(*force); err != nil {
			return err
		}
		fmt.Printf("任务已停止（agent %s）\n", run.Agent)
		return nil
	}
	// 无进行中任务 → 守护进程退出（shutdown 钩子 → serve.lock 随之摘除）。
	_ = client.Shutdown()
	fmt.Println("守护进程已退出（无进行中任务）")
	return nil
}

// runStart 是一次完整的 attached 运行（装配在 internal/server.App——
// 与 GUI 服务面同一实现）。
func runStart(root, dbPath, task string) error {
	// 装配（App；DEEPSEEK_API_KEY 的检查在 NewApp 的线路工厂里 fail fast
	// ——StartTask 时报错，装配本身可离线）。
	app, err := server.NewApp(root, dbPath, buildVersion())
	if err != nil {
		return err
	}
	defer app.Close()
	// 后台任务（detach）的运行锁在进程退出时摘除。
	defer os.Remove(filepath.Join(app.Control, "run.lock"))

	fmt.Printf("人类 Actor：%s\n收件箱：%s/inbox/\n", app.HumanID(), app.Control)
	agentID, err := app.StartTask(task)
	if err != nil {
		return err
	}
	fmt.Printf("项目 Agent：%s（运行中——Ctrl-C 中断）\n\n", agentID)

	// attached 等待：项目 Agent 的 report 回到人类收件箱。
	seen := map[string]bool{}
	for {
		select {
		case <-time.After(300 * time.Millisecond):
		}
		if done, report := firstReport(app.Control, seen); done {
			fmt.Printf("━━ 收件箱：%s ━━\n%s\n", report, frontmatterStripped(readInboxFile(app.Control, report)))
			fmt.Printf("任务完成：report 已在收件箱（%s/inbox/）；成本可用 ladder_report 查看。\n", app.Control)
			return nil
		}
		// 运行态检查：项目 Agent 结束且 report 已收 → 返回。
		if run := app.CurrentRun(); !run.Active {
			time.Sleep(500 * time.Millisecond) // 让最终的 report 文件落盘
			if done, report := firstReport(app.Control, seen); done {
				fmt.Printf("━━ 收件箱：%s ━━\n%s\n", report, frontmatterStripped(readInboxFile(app.Control, report)))
			}
			return nil
		}
	}
}

// firstReport 报告收件箱里是否出现了新的 report 文件（返回文件名）。
func firstReport(controlRoot string, seen map[string]bool) (bool, string) {
	entries, err := os.ReadDir(filepath.Join(controlRoot, "inbox"))
	if err != nil {
		return false, ""
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "report_") || seen[e.Name()] {
			continue
		}
		seen[e.Name()] = true
		return true, e.Name()
	}
	return false, ""
}

func readInboxFile(controlRoot, name string) string {
	data, err := os.ReadFile(filepath.Join(controlRoot, "inbox", name))
	if err != nil {
		return "（读取失败：" + err.Error() + "）"
	}
	return string(data)
}

// ---- 小件 ----

// readRunLock 读 run.lock 的 pid（文件损坏/无 pid = false——当作没在跑）。
func readRunLock(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, ln := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "pid:"); ok {
			if pid, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil && pid > 0 {
				return pid, true
			}
		}
	}
	return 0, false
}

// processAlive 报告进程是否存在（信号 0 探测）。
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// filepathAbs 的参数收敛（cmdStop 的 dir）。
func filepathAbs(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// cmdSay 处理 say 子命令（宿主按场景挑契约实现：daemon 在 → HTTPClient
// 进程内直达；无 daemon → FileMailbox 文件投递兜底——两个实现同一语义）。
func cmdSay(args []string) error {
	fs := flag.NewFlagSet("marl say", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	to := fs.String("to", "", "目标 Actor id（如项目 Agent 的 sub_000001）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" {
		return fmt.Errorf("usage: marl say [-dir <dir>] [-to <actor-id>] \"文本\"")
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if *to == "" {
		return fmt.Errorf("需要 -to <agent-id>（marl status 可查运行中的 Actor）")
	}
	if client, ok := resolveDaemon(root); ok {
		if err := client.SendMessage(*to, text); err != nil {
			return err
		}
		fmt.Printf("已送达 %s（经守护进程）\n", *to)
		return nil
	}
	mb := server.NewFileMailbox(root)
	if err := mb.SendMessage(*to, text); err != nil {
		return err
	}
	fmt.Printf("已投递 MsgDirect → %s（收件箱 %s/inbox/）\n", *to, server.ControlRootOf(root))
	fmt.Println("运行中的任务会在下一轮编排看到这条消息；无运行中的任务时它留在收件箱。")
	return nil
}
