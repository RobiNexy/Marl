// 命令 marl 是设计文档 Part 1.3 的 CLI 入口；init 是 13.8（阶段 6）的
// 交付检查命令：全自动初始化一个项目目录为 Marl 项目。
//
// 步骤（Part 1.2 / 13.8）：
//  1. fossil init（仓库 .marl/project.fossil）+ 注册框架用户
//     （human / agent / system）+ mtime-changes off；
//  2. fossil open（工作区 = 项目根）；
//  3. 写骨架：.marl/config.yaml、.marl/profiles/_default.yaml、
//     .marl/prompts/_index.yaml、.marl/knowledge/{contracts,decisions,preferences}/；
//  4. 首次 commit（author=system:init）。
//
// 幂等性：已初始化的目录（.marl/project.fossil 存在）直接报错——
// 重复 init 会覆盖历史，不是可重试操作（fossil.InitRepo 同一纪律）。
//
// 用法：
//
//	marl init [dir]     # dir 缺省为当前目录
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"marl/internal/fossil"
)

func main() {
	// 子命令剥离：Go 的 flag 不支持 "marl init -dir x" 的子命令形态
	// （flag.Parse 会在第一个位置参数处停止，-dir 落到 Args 里）。
	// 阶段 6 只有 init 一个子命令；更多子命令时引入手写分发。
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "init" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("marl init", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录（缺省当前目录）")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if err := run(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "marl init: %v\n", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	ctx := context.Background()
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", root, err)
	}

	// 已初始化检查（幂等性纪律：不是可重试操作）。
	marlDir := filepath.Join(root, ".marl")
	repoPath := filepath.Join(marlDir, "project.fossil")
	if _, err := os.Stat(repoPath); err == nil {
		return fmt.Errorf("%s already initialized (repo %s exists)", root, repoPath)
	}

	cli, err := fossil.NewCLI("")
	if err != nil {
		return err
	}

	// 1. 仓库 + 用户 + mtime 设置。
	if err := os.MkdirAll(marlDir, 0o755); err != nil {
		return err
	}
	if err := cli.InitRepo(ctx, repoPath, adminUser()); err != nil {
		return fmt.Errorf("init repo: %w", err)
	}
	// 2. checkout（工作区 = 项目根）。
	if err := cli.OpenRepo(ctx, repoPath, root); err != nil {
		return fmt.Errorf("open repo: %w", err)
	}

	// 3. 骨架文件（全部进版本控制——Part 1.2 的"进版本控制"清单）。
	if err := writeSkeleton(root); err != nil {
		return fmt.Errorf("skeleton: %w", err)
	}

	// 4. 首次提交（author=system）。
	if err := cli.Add(ctx, root, ".marl"); err != nil {
		return fmt.Errorf("add skeleton: %w", err)
	}
	hash, err := cli.Commit(ctx, root, fossil.UserSystem, "marl init：项目骨架与 ignore-glob")
	if err != nil {
		return fmt.Errorf("initial commit: %w", err)
	}
	fmt.Printf("已初始化 %s\n仓库：%s\n首次提交：%s\n", root, repoPath, shortHash(hash))
	return nil
}

// adminUser 取 fossil 管理员用户名（缺省系统登录名；fossil init 的默认行为）。
func adminUser() string {
	if u := os.Getenv("MARL_ADMIN_USER"); u != "" {
		return u
	}
	return os.Getenv("USER")
}

// writeSkeleton 写 .marl/ 的初始结构（Part 1.2）。
//
// 已存在的文件不覆盖（骨架是初始值，用户的定制不该被重置）。
func writeSkeleton(root string) error {
	files := map[string]string{
		".marl/config.yaml": `# Marl 项目配置
project:
  ladder_start: "r0"   # 起始阶梯级（Part 7.1）
  max_depth: 3         # fork 深度上限（Part 9.7 闸 1）
`,
		".marl/profiles/_default.yaml": `profile:
  id: "default"
  description: "默认 Agent（阶段 7 起 Profile 系统接管此文件）"
  requirement:
    require: [tool_call]
  allowed_skills: []   # 空 = 全部允许（Part 6.11）
  can_spawn: true
`,
		".marl/prompts/_index.yaml": `prompts:
  - id: "default"
    path: "prompts/default.md"
    description: "默认提示词"
`,
		".marl/prompts/default.md": `# 默认提示词

你是 Marl 框架的 Agent。按任务描述工作；需要分治时用 spawn_subagent。
`,
		// 知识目录的占位文件不用 .keep：fossil add 默认跳过 dotfiles
		// （实测记录见测试报告阶段 6），README.md 语义也更清晰。
		".marl/knowledge/contracts/README.md":  "# 契约\n接口契约放这里（Part 1.2 knowledge/contracts）。",
		".marl/knowledge/decisions/README.md":  "# 架构决策\n决策记录放这里。",
		".marl/knowledge/preferences/README.md": "# 常驻块\n偏好与约束放这里（编译成 standing_orders，上限 1000 est-token）。",
	}
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if _, err := os.Stat(abs); err == nil {
			continue // 已存在不覆盖
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// shortHash 截断 hash 便于打印（timeline 的显示形态）。
func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
