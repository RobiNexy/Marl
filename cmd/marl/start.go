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

	"github.com/RobiNexy/Marl/internal/actor"
	"github.com/RobiNexy/Marl/internal/proto"
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

// detachStart 把 runStart 放进后台进程（setsid 新会话——脱离当前进程组，
// 终端关闭不带走它；日志与 PID 落控制面）。控制面上的 run.lock 是"已有
// 后台任务在跑"的判据（stop/status 与防双跑都读它）。
func detachStart(root, dbPath, task string) error {
	controlRoot := server.ControlRootOf(root)
	lockPath := filepath.Join(controlRoot, "run.lock")
	if pid, ok := readRunLock(lockPath); ok && processAlive(pid) {
		return fmt.Errorf("已有后台任务在跑（PID %d，控制面 %s）；先 marl stop 或继续观察 marl status", pid, controlRoot)
	}
	if err := os.MkdirAll(controlRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir control plane: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(controlRoot, "start.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open control-plane log: %w", err)
	}
	defer logFile.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "start", "-dir", root, "-db", dbPath, task)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("pid: %d\nstarted_at: %s\ntask: %s\n",
		cmd.Process.Pid, time.Now().UTC().Format(time.RFC3339), task)), 0o644); err != nil {
		return err
	}
	fmt.Printf("后台任务已启动（PID %d）\n  日志：%s\n  跟踪：marl status；插话：marl say；停止：marl stop\n", cmd.Process.Pid, controlRoot)
	return nil
}

// cmdStop 终止后台任务（SIGTERM 优雅收尾：Agent 的 View 落盘 + 代报
// 兜底；超时转强杀）。
func cmdStop(args []string) error {
	fs := flag.NewFlagSet("marl stop", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	force := fs.Bool("force", false, "跳过优雅等待直接 SIGKILL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	controlRoot := server.ControlRootOf(filepathAbs(*dir))
	lockPath := filepath.Join(controlRoot, "run.lock")
	pid, ok := readRunLock(lockPath)
	if !ok || !processAlive(pid) {
		_ = os.Remove(lockPath) // 陈旧锁清理
		return fmt.Errorf("没有在跑的后台任务（控制面 %s）", controlRoot)
	}
	if !*force {
		if p, perr := os.FindProcess(pid); perr == nil {
			_ = p.Signal(syscall.SIGTERM) // 优雅信号（进程自行收尾）
		}
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) && processAlive(pid) {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if processAlive(pid) {
		if p, perr := os.FindProcess(pid); perr == nil {
			_ = p.Kill()
		}
		fmt.Printf("已强制终止（PID %d）\n", pid)
	} else {
		fmt.Printf("后台任务已停止（PID %d）\n", pid)
	}
	_ = os.Remove(lockPath)
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

// cmdSay 处理 say 子命令（人类 → Actor 的直接消息）。
//
// 传输形态：**写收件箱文件**（actor.FileBackend 的 direct 形态）——say
// 进程的进程表是空的（Agent 活在 daemon/attached 进程里），跨进程投递
// 只能走文件通道；运行中的任务经 watcher 消费并路由。GUI 不走这里
// （serve 的 HTTP API 在进程内直达）。
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
	backend, err := actor.NewFileBackend(actor.FileConfig{
		Root: server.ControlRootOf(root), Human: actor.HumanID(osUID()),
	})
	if err != nil {
		return err
	}
	defer backend.Stop()
	if to == nil || *to == "" {
		return fmt.Errorf("需要 -to <agent-id>（marl status 可查运行中的 Actor）")
	}
	env := actor.Envelope{
		From:    actor.HumanID(osUID()),
		To:      actor.ActorID(*to),
		Type:    proto.MsgDirect,
		Payload: &proto.DirectMessage{Text: text},
	}
	if err := backend.Deliver(env); err != nil {
		return err
	}
	controlRoot := server.ControlRootOf(root)
	fmt.Printf("已投递 MsgDirect → %s（收件箱 %s/inbox/）\n", env.To, controlRoot)
	fmt.Println("运行中的任务会在下一轮编排看到这条消息；无运行中的任务时它留在收件箱。")
	return nil
}
