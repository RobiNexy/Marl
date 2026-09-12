// 子命令 marl knowledge 的 vendoring 面（13.12 阶段 10）：
//
//	marl knowledge pull    # 从全局库按 lock 落地 vendor/（幂等）
//	marl knowledge promote # 项目文件 → 全局库（author=human 的 commit）
//
// 全局库路径：-global 缺省 ~/.local/share/marl/global.fossil（XDG_DATA_HOME
// 的字段；Part 12.7 的路径约定）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"marl/internal/fossil"
	"marl/internal/knowledge"
)

// defaultGlobalRepo 是全局知识库的缺省位置（XDG_DATA_HOME，Part 12.7）。
func defaultGlobalRepo() string {
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "marl", "global.fossil")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "marl", "global.fossil")
}

// runKnowledgePull 处理 pull 子命令（幂等：lock 锁 artifact，重复 pull
// 内容逐字节不变——Part 12.7 的关键语义，验证见 knowledge 包的测试）。
func runKnowledgePull(args []string) error {
	fs := flag.NewFlagSet("marl knowledge pull", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	global := fs.String("global", "", "全局知识库（fossil repo 路径；缺省 XDG_DATA_HOME/marl/global.fossil）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	g := *global
	if g == "" {
		g = defaultGlobalRepo()
	}
	marlDir := filepath.Join(*dir, ".marl")
	if _, err := os.Stat(g); err != nil {
		return fmt.Errorf("全局知识库 %s 不存在（先 marl knowledge init-global，或 -global <path>）", g)
	}
	cli, err := fossil.NewCLI("")
	if err != nil {
		return err
	}
	entries, err := knowledge.Pull(context.Background(), cli, marlDir, g)
	if err != nil {
		return err
	}
	fmt.Printf("已拉取 %d 条知识：\n", len(entries))
	for _, e := range entries {
		fmt.Printf("  %-40s artifact=%s\n", e.Path, shortHash(e.Artifact))
	}
	return nil
}

// runKnowledgePromote 处理 promote 子命令（Part 12.8：人类是唯一准入关口
// ——这是 CLI 命令面、不是 Agent 工具；commit author=human）。
func runKnowledgePromote(args []string) error {
	fs := flag.NewFlagSet("marl knowledge promote", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	global := fs.String("global", "", "全局知识库")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rel := fs.Arg(0)
	if rel == "" {
		return fmt.Errorf("usage: marl knowledge promote <path relative to .marl/>")
	}
	g := *global
	if g == "" {
		g = defaultGlobalRepo()
	}
	if _, err := os.Stat(g); err != nil {
		return fmt.Errorf("全局知识库 %s 不存在（先初始化全局库）", g)
	}
	cli, err := fossil.NewCLI("")
	if err != nil {
		return err
	}
	marlDir := filepath.Join(*dir, ".marl")
	hash, err := knowledge.Promote(context.Background(), cli, marlDir, g, rel)
	if err != nil {
		return err
	}
	fmt.Printf("已提升 %s 到全局库（commit %s，author=human）\n", rel, hash)
	return nil
}
