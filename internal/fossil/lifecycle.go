package fossil

// 生命周期：仓库的创建与 checkout 的建立（Part 8.4 / 13.8）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 框架使用的三个固定用户（Part 8.4：author 按调用路径决定，Agent 无法影响——
// 原则 4 的 VCS 落点）。用户在 InitRepo 时创建（fossil 的 user 必须先注册，
// 实测 "no such user" 拒绝未注册的 -U）。
const (
	UserHuman  = "human"  // 人类操作（讨论裁决、手动 commit）
	UserAgent  = "agent"  // 父 Agent 的单写者提交
	UserSystem = "system" // 框架自身（init 的首次 commit）
)

// ignoreGlob 是 .fossil-settings/ignore-glob 的内容（Part 1.2 版本控制规则：
// 忽略 supervisor.db / logs / snapshots 等运行期数据）。末尾换行必需
// （fossil 逐行解析）。
//
// 实测依据（doc.go）：ignore-glob 在 `fossil add` 层生效（SKIP 而非 ADDED）。
const ignoreGlob = ".marl/*.db\n.marl/*.db-*\nlogs/\nsnapshots/\n"

// InitRepo 创建仓库 + 注册框架用户（幂等：仓库已存在时报错——重复 init
// 会覆盖旧仓库，这不是"幂等重试"能兜住的场景，必须显式失败）。
//
// adminUser 是 fossil 的管理员用户名（默认取系统登录名，测试传固定值）。
// 失败：仓库文件已存在、fossil 命令失败。
func (c *CLI) InitRepo(ctx context.Context, repoPath, adminUser string) error {
	if _, err := os.Stat(repoPath); err == nil {
		return fmt.Errorf("fossil init: %s already exists (re-init would destroy history)", repoPath)
	}
	if _, err := c.runWrite(ctx, "", "init", repoPath, "-A", adminUser); err != nil {
		return fmt.Errorf("fossil init %s: %w", repoPath, err)
	}
	// 注册框架用户（user new <name> <contact>）。
	for _, u := range []string{UserHuman, UserAgent, UserSystem} {
		if _, err := c.runWrite(ctx, "", "user", "new", u, "", "-R", repoPath); err != nil {
			// 已存在的用户不算错误（幂等重试 init 的后半段时走到这里）。
			if !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("fossil user new %s: %w", u, err)
			}
		}
	}
	// mtime-changes off（仓库级设置）：fossil 默认按 mtime 判定文件是否
	// 变更，commit 后立即改文件（同一 mtime 粒度内）会被漏检——真机实测
	// 踩中（见测试报告阶段 6）。off 后 fossil 改用内容校验和，代价是
	// changes 稍慢（本地仓库可忽略）。
	if _, err := c.runWrite(ctx, "", "settings", "mtime-changes", "off", "-R", repoPath); err != nil {
		return fmt.Errorf("fossil settings mtime-changes: %w", err)
	}
	return nil
}

// OpenRepo 在 workdir 建立 checkout（幂等：已 open 的工作区直接返回成功）。
//
// 同时写入 .fossil-settings/ignore-glob（+ .no-warn 消警告文件）——它们是
// 版本化文件，首次 commit 时入库（Part 1.2 的"ignore 在 init 时自动写好"）。
//
// 失败：workdir 不存在；fossil open 失败（非空目录需 -k，本方法始终带
// ——init 场景的工作区有骨架文件，fossil 的空目录检查不适用）。
func (c *CLI) OpenRepo(ctx context.Context, repoPath, workdir string) error {
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("fossil open: mkdir %s: %w", workdir, err)
	}
	if open, err := c.IsOpen(ctx, workdir); err != nil {
		return fmt.Errorf("fossil open: %w", err)
	} else if open {
		return nil // 幂等
	}
	if _, err := c.runWrite(ctx, "", "open", repoPath, "--workdir", workdir, "-k"); err != nil {
		return fmt.Errorf("fossil open %s in %s: %w", repoPath, workdir, err)
	}
	// ignore-glob（版本化设置；.no-warn 空文件消除 fossil 的双值警告，
	// 实测记录见 doc.go）。
	settingsDir := filepath.Join(workdir, ".fossil-settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		return fmt.Errorf("fossil open: mkdir settings: %w", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "ignore-glob"), []byte(ignoreGlob), 0o644); err != nil {
		return fmt.Errorf("fossil open: write ignore-glob: %w", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "ignore-glob.no-warn"), nil, 0o644); err != nil {
		return fmt.Errorf("fossil open: write no-warn: %w", err)
	}
	return nil
}

// IsOpen 报告 workdir 是否在打开的 checkout 内。
//
// 判据（实测记录见 doc.go）：`fossil status` 在 checkout 外报
// "not within an open check-out"（ErrNotOpen 哨兵）。
func (c *CLI) IsOpen(ctx context.Context, workdir string) (bool, error) {
	if _, err := os.Stat(workdir); err != nil {
		return false, fmt.Errorf("fossil isopen: %w: %s", ErrNotFound, workdir)
	}
	_, err := c.runRead(ctx, workdir, "status")
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), ErrNotOpen.Error()) {
		return false, nil
	}
	return false, fmt.Errorf("fossil isopen: %w", err)
}

// CloseRepo 关闭 checkout（保留工作区文件；-k 语义）。测试与 marl stop 用。
func (c *CLI) CloseRepo(ctx context.Context, workdir string) error {
	_, err := c.runWrite(ctx, workdir, "close", "-k")
	if err != nil && strings.Contains(err.Error(), ErrNotOpen.Error()) {
		return nil // 幂等
	}
	return err
}
