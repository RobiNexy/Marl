//go:build ignore

package skill

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// ────────────────────────────── shell_exec ──────────────────────────────
//
// 实现要点（Part 4.4.7）：
//   - stdin=DEVNULL（禁交互）
//   - 独立进程组（Setpgid），超时杀整组——避免孤儿进程继续写文件
//   - 强制 CI=true / TERM=dumb / PYTHONUNBUFFERED=1：防工具检测 TTY 改变输出格式
//   - 输出截断：tail/head 保 50 行，full 上限 300 行
//   - 安全过滤：黑名单 token 级拦截 sudo/su
//   - workdir 必须位于 workspace 内

type shellExecSkill struct{}

func (s *shellExecSkill) Name() string { return "shell_exec" }
func (s *shellExecSkill) Description() string {
	return "Execute a command synchronously. Output is truncated. Long-running commands killed after timeout."
}
func (s *shellExecSkill) Capability() Capability { return CapMutating }

const (
	tailLines  = 50
	fullLines  = 300
	maxTimeout = 120 * time.Second
)

func (s *shellExecSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
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
	timeout := time.Duration(clamp(intArg(args, "timeout_ms", 30000), 1, int(maxTimeout.Milliseconds()))) * time.Millisecond

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = workdir
	cmd.Stdin = nil // DEVNULL 语义：nil 即 /dev/null
	cmd.Env = shellEnv(args)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	start := time.Now()
	runErr := cmd.Start()
	if runErr == nil {
		runErr = cmd.Wait()
	}
	duration := time.Since(start)
	timedOut := cctx.Err() == context.DeadlineExceeded || ctx.Err() != nil
	if timedOut {
		killGroup(cmd.Process) // Wait 返回后再补杀一次，确保进程组无幸存
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
	stdout := truncateOutput(outBuf.String(), outputMode)
	stderr := truncateOutput(errBuf.String(), outputMode)
	res := Result(
		"exit_code", exitCode,
		"stdout", stdout,
		"stderr", stderr,
		"duration_ms", duration.Milliseconds(),
		"timed_out", timedOut,
	)
	res["ok"] = exitCode == 0 && !timedOut // 契约：非零退出是"命令失败"而非技能失败，仍返回结构化结果
	return res, nil
}

// killGroup 负信号量杀整组。进程可能已退出——忽略错误。
func killGroup(p *os.Process) {
	if p == nil {
		return
	}
	syscall.Kill(-p.Pid, syscall.SIGKILL) //nolint:errcheck // 已退出的进程组无妨
}

// checkForbidden 黑名单 token 级过滤：拦 sudo -u root 这类绕过（Part 4.4.7）。
// [推断] 黑名单天然不完备（alias、脚本内提权），但本地单人场景下
// 拦住 LLM 最常见的危险模式已足够；完备沙箱需要 seccomp/容器，超出 v1 范围。
func checkForbidden(command string) error {
	fields := strings.Fields(command)
	for _, f := range fields {
		switch f {
		case "sudo", "su":
			return errf("FORBIDDEN_COMMAND",
				"command %q contains forbidden token %q", command, f)
		}
		if f == "doas" || strings.HasPrefix(f, "sudo=") {
			return errf("FORBIDDEN_COMMAND", "command %q is forbidden", command)
		}
	}
	return nil
}

func shellEnv(args map[string]any) []string {
	env := os.Environ()
	env = append(env,
		"CI=true",
		"TERM=dumb",
		"PYTHONUNBUFFERED=1",
		"NO_COLOR=1",
	)
	for k, v := range kvArgs(args["env"]) {
		env = append(env, k+"="+v)
	}
	return env
}

func kvArgs(v any) map[string]string {
	out := map[string]string{}
	m, ok := v.(map[string]any)
	if !ok {
		return out
	}
	for k, sv := range m {
		if s, ok := sv.(string); ok {
			out[k] = s
		}
	}
	return out
}

// truncateOutput tail/head 用固定行数窗口自然截断；full 上限 300 行。
func truncateOutput(s, mode string) string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	limit := fullLines
	switch mode {
	case "tail", "head":
		limit = tailLines
	}
	if len(lines) <= limit {
		return strings.Join(lines, "\n")
	}
	switch mode {
	case "tail":
		return "... (truncated)\n" + strings.Join(lines[len(lines)-limit:], "\n")
	case "head":
		return strings.Join(lines[:limit], "\n") +
			fmt.Sprintf("\n... (%d more lines)", len(lines)-limit)
	default: // full：取头部并注明剩余
		return strings.Join(lines[:limit], "\n") +
			fmt.Sprintf("\n... (%d more lines)", len(lines)-limit)
	}
}

// ────────────────────────────── get_env ──────────────────────────────
//
// 环境概览（只读）。刻意返回白名单字段而非全量 env：避免 API key 类敏感值
// 进入模型上下文与持久化日志。[推断] 白名单比黑名单安全——新增强敏变量无需改代码。

type getEnvSkill struct{}

func (s *getEnvSkill) Name() string { return "get_env" }
func (s *getEnvSkill) Description() string {
	return "Get environment overview: workspace info, OS/arch, safe env vars."
}
func (s *getEnvSkill) Capability() Capability { return CapReadOnly }

var safeEnvKeys = []string{"HOME", "PATH", "SHELL", "LANG", "TMPDIR", "USER", "GOPATH", "GOROOT"}

func (s *getEnvSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	cwd, _ := os.Getwd()
	env := map[string]string{}
	for _, k := range safeEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	return Result(
		"workspace_root", ec.WorkspaceRoot,
		"cwd", cwd,
		"goos", runtime.GOOS,
		"goarch", runtime.GOARCH,
		"num_cpu", runtime.NumCPU(),
		"go_version", runtime.Version(),
		"env", env,
	), nil
}
