package fossil

// CLI 封装：进程执行、写互斥、错误分类。
//
// 设计决策（ADR-0030）：
//   - **读写分离**：写命令（commit/add/user/settings）串行（进程内互斥——
//     fossil 自身有 checkout 锁，本包的互斥防御的是"框架 commit 与人类手动
//     commit 撞车"这类框架外并发）；读命令（status/diff/timeline）并发。
//   - **超时**：每个命令 60s 上限（fossil 本地操作毫秒级；60s 只在磁盘卡死
//     时触发，宁可超时也不挂死主循环）。
//   - **不做重试**：fossil 本地命令的失败是确定性的（未 open/无改动/冲突），
//     重试无意义；lockfile 退避重试（13.8 的 runWrite 描述）被实测否决——
//     并发 commit 由 fossil 内部锁串行化且都能成功，不存在需要退避的场景。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 错误哨兵（机械判断依据；上层 errors.Is 匹配）。
var (
	// ErrNotOpen：工作区未 open（或 fossil 不可用）。
	ErrNotOpen = errors.New("fossil: not in an open check-out")
	// ErrNothingToCommit：无改动可提交（正常空转）。
	ErrNothingToCommit = errors.New("fossil: nothing to commit")
	// ErrNotFound：仓库文件/路径不存在。
	ErrNotFound = errors.New("fossil: not found")
)

// CLI 是 fossil 命令行的进程封装。
//
// 零值契约：零值不可用（Bin 为空）；必须经 NewCLI。
type CLI struct {
	// Bin 是 fossil 可执行文件路径（默认 "fossil"）。
	Bin string
	// Timeout 是单命令超时（默认 60s）。
	Timeout time.Duration

	writeMu sync.Mutex // 写命令互斥（见 doc.go 的读写分离决策）
}

// NewCLI 构造 CLI。
//
// 失败：fossil 可执行文件不存在（启动期 fail fast——VCS 缺席时框架的
// 提交能力是幻觉，宁可 init 就报错）。
func NewCLI(bin string) (*CLI, error) {
	if bin == "" {
		bin = "fossil"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("fossil: %q not in PATH: %w", bin, err)
	}
	return &CLI{Bin: path, Timeout: 60 * time.Second}, nil
}

// result 是一次命令执行的原始产出。
type result struct {
	stdout string
	stderr string
}

// runRead 执行读命令（并发安全）。
func (c *CLI) runRead(ctx context.Context, dir string, args ...string) (result, error) {
	return c.exec(ctx, dir, args...)
}

// runWrite 执行写命令（进程内互斥）。
func (c *CLI) runWrite(ctx context.Context, dir string, args ...string) (result, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.exec(ctx, dir, args...)
}

// exec 执行一次 fossil 命令。
//
// 工作目录：dir 非空时作为子进程 cwd（fossil 的多数命令不支持 -C，
// 实测记录见 doc.go；"在哪个 checkout 里跑"由 cwd 决定）。
//
// 失败分类：
//   - ctx 取消 → 原样 ctx.Err()（上游停机不是 fossil 故障）；
//   - 超时 → 命令被杀，错误带超时标记；
//   - 退出码非 0 → 按 stderr 文本归类哨兵（ErrNotOpen / ErrNothingToCommit /
//     ErrNotFound），其余原样带 stderr 上抛。
func (c *CLI) exec(ctx context.Context, dir string, args ...string) (result, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, c.Bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return result{stdout: out.String(), stderr: errb.String()}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return result{stdout: out.String(), stderr: errb.String()},
				fmt.Errorf("fossil %s: timed out after %v", args[0], timeout)
		}
		combined := out.String() + errb.String()
		switch {
		case strings.Contains(combined, "not within an open check-out"):
			return result{stdout: out.String(), stderr: errb.String()}, fmt.Errorf("fossil %s: %w", args[0], ErrNotOpen)
		case strings.Contains(combined, "nothing has changed"):
			return result{stdout: out.String(), stderr: errb.String()}, fmt.Errorf("fossil %s: %w", args[0], ErrNothingToCommit)
		case strings.Contains(combined, "no such file"), strings.Contains(combined, "does not exist"):
			return result{stdout: out.String(), stderr: errb.String()}, fmt.Errorf("fossil %s: %w: %s", args[0], ErrNotFound, firstLine(combined))
		default:
			return result{stdout: out.String(), stderr: errb.String()},
				fmt.Errorf("fossil %s: %s", args[0], firstLine(combined))
		}
	}
	return result{stdout: out.String(), stderr: errb.String()}, nil
}

// firstLine 取文本首行（错误信息里给人看的最小片段；fossil 的多行错误
// 全量保留在 result 里，包装层只带首行避免日志爆炸）。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
