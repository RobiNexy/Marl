package skill

// shell_exec：同步命令执行（Part 4.4.7 的实现移植——old_skills/shell.go
// 的实战形态进新技能包）。
//
// 实现要点：
//   - stdin=DEVNULL（禁交互）
//   - 独立进程组（Setpgid），超时杀整组——避免孤儿进程继续写文件
//   - CI=true / TERM=dumb / NO_COLOR=1：防工具检测 TTY 改变输出格式
//   - 输出截断：tail/head 保 50 行，full 上限 300 行
//   - 黑名单 token 级拦截 sudo/su/doas
//   - workdir 必须位于 workspace 内（经 Resolver 校验）
//
// 非零退出码是"命令失败"而非"技能失败"——返回结构化结果（exit_code /
// stderr 原样给模型，它自己决定怎么修），这是本技能最重要的语义选择。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

const (
	shellTailLines  = 50
	shellFullLines  = 300
	shellMaxTimeout = 120 * time.Second
)

// ShellExec 是 shell_exec 的共享实例。
var ShellExec Skill = &shellExecSkill{}

// shellExecSkill 是无状态技能（可并发）。
type shellExecSkill struct{}

func (s *shellExecSkill) Name() string { return SkillShellExec }
func (s *shellExecSkill) Description() string {
	return "Execute a shell command synchronously (sh -c). Stdin is closed. Output is truncated (tail/head/full). Long-running commands are killed after timeout (default 30s, max 120s)."
}
func (s *shellExecSkill) Kind() SkillKind { return SkillMutating }

// Parameters 逐字节写死（冻结前缀，见 ToolSchema 契约）。
func (s *shellExecSkill) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"command":{"type":"string","description":"The shell command to run (executed via sh -c)"},` +
		`"workdir":{"type":"string","description":"Working directory relative to workspace root (default '.')"},` +
		`"timeout_ms":{"type":"integer","description":"Timeout in ms (default 30000, max 120000)"},` +
		`"output_mode":{"type":"string","enum":["tail","head","full"],"description":"Output truncation mode (default tail)"},` +
		`"env":{"type":"object","additionalProperties":{"type":"string"},"description":"Extra env vars for this command"}},` +
		`"required":["command"]}`)
}

// Execute 实现 Skill.Execute（非零退出 = 业务失败结果；系统级启动错误才走
// error 通道——与 list_dir 的 ENOENT 修正同一纪律）。
func (s *shellExecSkill) Execute(ctx context.Context, args map[string]any, env *SkillEnv) (*SkillResult, error) {
	command := strArg(args, "command", "")
	if command == "" {
		return NewFailure("BAD_ARGS", "command is required"), nil
	}
	if f := forbiddenToken(command); f != "" {
		return NewFailure("FORBIDDEN_COMMAND", "command contains forbidden token %q (privilege-escalation commands are always rejected)", f), nil
	}
	res, rerr := resolvePath(env, strArg(args, "workdir", "."), types.PathRead)
	if rerr != nil {
		return businessOf(rerr)
	}
	timeout := time.Duration(clamp(intArg(args, "timeout_ms", 30000), 1, int(shellMaxTimeout.Milliseconds()))) * time.Millisecond

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = res.RealPath
	cmd.Stdin = nil // DEVNULL 语义：nil 即 /dev/null
	cmd.Env = shellEnv(args)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	start := time.Now()
	runErr := cmd.Start()
	if runErr == nil {
		// [真机教训的移植] deadline 到点杀**整组**（不只是 sh）——sh 的
		// 子进程（如 sleep）仍握着输出管道时，Wait 会等到管道关闭才返回
		//（超时从 300ms 拖成 5s 的实测）；group-kill 让管道写端即刻消失。
		go func() {
			<-cctx.Done()
			if ctx.Err() == nil { // 宿主 ctx 未取消（是超时而非正常收尾）→ 补杀
				killShellGroup(cmd.Process)
			}
		}()
		runErr = cmd.Wait()
	}
	duration := time.Since(start)
	timedOut := cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil
	if timedOut {
		killShellGroup(cmd.Process) // Wait 返回后再补杀一次，确保进程组无幸存
	}

	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else if !timedOut {
			return nil, fmt.Errorf("shell_exec %q: %w", command, runErr) // 启动失败等系统级错误
		}
	}

	outputMode := strArg(args, "output_mode", "tail")
	return &SkillResult{
		OK: exitCode == 0 && !timedOut, // 非零退出 = 命令失败（结构化结果，不是技能失败）
		Data: map[string]any{
			"exit_code":   exitCode,
			"stdout":      truncateShellOutput(outBuf.String(), outputMode),
			"stderr":      truncateShellOutput(errBuf.String(), outputMode),
			"duration_ms": duration.Milliseconds(),
			"timed_out":   timedOut,
		},
	}, nil
}

// forbiddenToken 是黑名单的 token 级拦截（sudo/su/doas——提权到框架
// 之外的身份是本技能的红线）。
func forbiddenToken(command string) string {
	fields := strings.Fields(command)
	for _, f := range fields {
		if f == "sudo" || f == "su" || f == "doas" {
			return f
		}
	}
	return ""
}

// killShellGroup 负信号量杀整组。进程可能已退出——忽略错误。
func killShellGroup(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}

// shellEnv 是命令的环境（防 TTY 检测改变输出格式 + 调用方可追加）。
func shellEnv(args map[string]any) []string {
	env := os.Environ()
	env = append(env, "CI=true", "TERM=dumb", "NO_COLOR=1", "PYTHONUNBUFFERED=1")
	if m, ok := args["env"].(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				env = append(env, k+"="+s)
			}
		}
	}
	return env
}

// truncateShellOutput：tail/head 用固定行数窗口；full 上限 300 行。
func truncateShellOutput(s, mode string) string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	limit := shellFullLines
	switch mode {
	case "tail", "head":
		limit = shellTailLines
	}
	if len(lines) <= limit {
		return s
	}
	if mode == "head" {
		return strings.Join(lines[:limit], "\n") + fmt.Sprintf("\n… (%d lines truncated)", len(lines)-limit)
	}
	kept := lines[len(lines)-limit:]
	return fmt.Sprintf("… (%d lines truncated) …\n", len(lines)-limit) + strings.Join(kept, "\n")
}
