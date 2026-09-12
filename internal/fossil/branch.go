package fossil

// 分支操作（Part 11.2 / 12.10：分支只服务讨论分支与显式实验两个用途；
// v1 的形态被收缩为"创建 + 切换 + 读取"—BranchSwitch 是讨论合并的最
// 小需要：开分支 → draft 在分支上 commit → 裁决后 checkout trunk）。

import (
	"context"
	"fmt"
	"strings"
)

// BranchCreate 在 checkout 中新建讨论分支（fossil branch new <name> <basis>
// --user-override <user>）。
//
// 实测行为（2026-09，fossil 2.26）：`branch new` 需要一个 BASIS check-in
// （分支从它分叉——讨论分支一律从 trunk 分叉）；内部会做一次提交，
// 其 user 需要 --user-override 提供（否则 fossil 回退到交互判定，CLI
// 环境下报 "cannot figure out who you are"）。
//
// name / basis 非空且不含空白——名字的 CLI 拆参卫生在本层做。
//
// 失败：repo 未 open / 名字已存在 / fossil 故障。
func (c *CLI) BranchCreate(ctx context.Context, workdir, name string) error {
	if err := validateBranchName(name); err != nil {
		return fmt.Errorf("fossil branch create: %w", err)
	}
	if _, err := c.runWrite(ctx, workdir, "branch", "new", name, trunkBasis,
		"--user", UserHuman, "--user-override", UserHuman); err != nil {
		return fmt.Errorf("fossil branch create %s: %w", name, err)
	}
	return nil
}

// BranchSwitch 切换 checkout 到指定分支（fossil checkout <name>）。
//
// fossil 的分支切换是工作区级操作：讨论期间的 open/commit 都发生在目标
// 分支上。单写者纪律意味着没有并发的 BranchSwitch 竞争者（讨论的每个
// 阶段都由框架在单一 goroutine 内执行）。
func (c *CLI) BranchSwitch(ctx context.Context, workdir, name string) error {
	if err := validateBranchName(name); err != nil {
		return fmt.Errorf("fossil branch switch: %w", err)
	}
	if _, err := c.runRead(ctx, workdir, "checkout", name); err != nil {
		return fmt.Errorf("fossil branch switch %s: %w", name, err)
	}
	return nil
}

// trunkBasis 是分支分叉的基准分支名（fossil 的默认 trunk）。
// 与 discuss.merge 的 trunkBranch 语义同源；这里避免 import cycle（fossil 再入它自己的包内常量——
// 两处独立定义同名值并各自注明出处）。
const trunkBasis = "trunk"

// validateBranchName 是分支名的卫生（讨论分支的形态：
// discuss_<ulid>——ulid 含连字符与字母数字，无空格无斜杠）。
func validateBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("branch name is empty")
	}
	if strings.ContainsAny(name, " \t\"'/$\\") {
		return fmt.Errorf("branch name %q contains whitespace or shell-hostile characters", name)
	}
	return nil
}

// Cat 在 checkout 里读一份文件（vendor 的 pull 面；相对路径）。
func (c *CLI) Cat(ctx context.Context, workdir, relPath string) ([]byte, error) {
	res, err := c.runRead(ctx, workdir, "cat", relPath)
	if err != nil {
		return nil, err
	}
	return []byte(res.stdout), nil
}
