//go:build ignore

package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ────────────────────────────── shell_spawn / shell_kill ──────────────────────────────
//
// 异步事件循环（Part 4.4.8 / M4）：LLM 后台起长驻进程，不阻塞主循环。
//
// 设计要点：
//   - 输出重定向到 LogDir/procs/<handle>.log——LLM 用 file_read 轮询进度
//   - 进程观察者 goroutine 监控退出：写 <handle>.exit.json 事件文件 +
//     调用 ExecContext.ProcessEvent 回调（cmd 层注入，转发到 Aggregator 广播）
//   - meta 文件 <handle>.json 持 pid——shell_kill 按 handle 反查；跨进程
//     重启后（内存表丢失）仍可杀
//   - 独立进程组（Setpgid）：kill 杀整组，防孤儿

// procTable 本进程内 handle → 运行中的 cmd。杀进程先查内存表（快），
// miss 再读 meta 文件（跨进程重启兜底）。
var procTable sync.Map // handle → *os.Process

type shellSpawnSkill struct{}

func (s *shellSpawnSkill) Name() string { return "shell_spawn" }
func (s *shellSpawnSkill) Description() string {
	return "Start a long-running command in the background (own process group). Returns a handle; output streams to a log file; an exit event is recorded when it terminates. Use shell_kill to stop it."
}
func (s *shellSpawnSkill) Capability() Capability { return CapStructural }

func (s *shellSpawnSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	command := strArg(args, "command", "")
	if command == "" {
		return nil, errf("BAD_ARGS", "command is required")
	}
	if err := checkForbidden(command); err != nil {
		return nil, err
	}
	workdir, err := ec.Resolve(strArg(args, "workdir", "."))
	if err != nil {
		return nil, err
	}
	procsDir := filepath.Join(ec.LogDir, "procs")
	if err := os.MkdirAll(procsDir, 0o755); err != nil {
		return nil, fmt.Errorf("shell_spawn: mkdir procs: %w", err)
	}

	handle := "proc-" + fmt.Sprintf("%x", time.Now().UnixNano())
	logPath := filepath.Join(procsDir, handle+".log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("shell_spawn: open log: %w", err)
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = workdir
	cmd.Stdin = nil // DEVNULL：后台进程禁交互
	cmd.Env = shellEnv(args)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		logF.Close()
		return nil, fmt.Errorf("shell_spawn %q: %w", command, err)
	}
	procTable.Store(handle, cmd.Process)

	// meta：handle → pid/command 映射持久化（shell_kill 反查）
	meta := map[string]any{
		"handle": handle, "pid": cmd.Process.Pid,
		"command": command, "started_at": time.Now().UnixMilli(),
	}
	if raw, mErr := json.Marshal(meta); mErr == nil {
		_ = os.WriteFile(filepath.Join(procsDir, handle+".json"), raw, 0o644)
	}

	// 进程观察者：退出事件落盘 + 回调广播。goroutine 退出条件 = 进程退出，
	// 无泄漏。logF 的关闭由观察者负责（写完最后一个字节后）。
	go func() {
		werr := cmd.Wait()
		logF.Close()
		exitCode := 0
		if ee, ok := werr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else if werr != nil {
			exitCode = -1
		}
		ev := map[string]any{
			"event": "process_exit", "handle": handle,
			"pid": cmd.Process.Pid, "command": command,
			"exit_code": exitCode, "finished_at": time.Now().UnixMilli(),
		}
		if raw, jErr := json.Marshal(ev); jErr == nil {
			_ = os.WriteFile(filepath.Join(procsDir, handle+".exit.json"), raw, 0o644)
		}
		procTable.Delete(handle)
		if ec.ProcessEvent != nil {
			ec.ProcessEvent(ev) // M4：Aggregator 广播（注入则推送，nil 则静默）
		}
	}()

	return Result(
		"handle", handle,
		"pid", cmd.Process.Pid,
		"log_file", relToWorkspace(ec, logPath),
		"hint", "poll output with file_read on log_file; exit event will be pushed as process_exit",
	), nil
}

type shellKillSkill struct{}

func (s *shellKillSkill) Name() string { return "shell_kill" }
func (s *shellKillSkill) Description() string {
	return "Terminate a background process started by shell_spawn, by handle or pid. Sends SIGTERM to the process group, escalating to SIGKILL if it survives."
}
func (s *shellKillSkill) Capability() Capability { return CapMutating }

func (s *shellKillSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	handle := strArg(args, "handle", "")
	pid := intArg(args, "pid", 0)
	if handle == "" && pid <= 0 {
		return nil, errf("BAD_ARGS", "handle or pid is required")
	}
	if handle != "" && pid <= 0 {
		pid = pidFromHandle(ec, handle)
	}
	if pid <= 0 {
		return nil, errf("NOT_FOUND", "no running process for handle %q", handle)
	}

	// SIGTERM 进程组 → 短等待 → SIGKILL 兜底
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	killed := "term"
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pid, 0) != nil { // 组内已无进程
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if syscall.Kill(-pid, 0) == nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		killed = "kill"
	}
	if handle != "" {
		procTable.Delete(handle)
	}
	return Result("handle", handle, "pid", pid, "signal", killed), nil
}

// pidFromHandle 反查 pid：先内存表（快），miss 读 meta 文件（跨重启兜底）。
// meta 存在但进程已死时返回 -1（区别于"找不到 handle"）。
func pidFromHandle(ec *ExecContext, handle string) int {
	if v, ok := procTable.Load(handle); ok {
		if p, ok := v.(*os.Process); ok && p != nil {
			return p.Pid
		}
	}
	raw, err := os.ReadFile(filepath.Join(ec.LogDir, "procs", handle+".json"))
	if err != nil {
		return 0
	}
	var meta struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(raw, &meta) != nil || meta.PID <= 0 {
		return 0
	}
	// meta 可能是陈旧的：进程死了 kill(-pid,0) 报 ESRCH，由调用方的
	// 信号结果呈现，这里直接返回 pid
	return meta.PID
}

// relToWorkspace 尽量给 workspace 相对路径（LLM 的 file_read 以相对路径调用）。
func relToWorkspace(ec *ExecContext, abs string) string {
	if rel, err := filepath.Rel(ec.WorkspaceRoot, abs); err == nil && rel[0] != '.' {
		return rel
	}
	return abs
}
