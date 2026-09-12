// 子命令 marl knowledge 是知识库的编译检查面（13.9 阶段 7）。
//
// knowledge lint：编译 preferences/ 成常驻块，报告 est-token 与逐文件
// 大小；超过 1000 时退出码非 0（CLI 语义：超限是硬失败，Part 12.4 ②
// ——在编辑器里被拦住，而不是任务跑到 LLM 装配时报错）。
//
// 交付判据（13.9）：
//
//	marl knowledge lint   # 编译后 ≤1000 → 退出 0；>1000 → 退出 1 + 报告
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"marl/internal/knowledge"
)

func cmdKnowledge(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: marl knowledge lint | pull | promote")
	}
	switch args[0] {
	case "lint":
		return cmdKnowledgeLint(args[1:])
	case "pull":
		return runKnowledgePull(args[1:])
	case "promote":
		return runKnowledgePromote(args[1:])
	default:
		return fmt.Errorf("unknown knowledge subcommand %q (want: lint | pull | promote)", args[0])
	}
}

// cmdKnowledgeLint 编译一次并打印报告；超限返回 error（main 落 exit=1）。
func cmdKnowledgeLint(args []string) error {
	fs := flag.NewFlagSet("marl knowledge lint", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录（缺省当前目录）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prefDir := filepath.Join(*dir, ".marl", "knowledge", "preferences")
	block, err := knowledge.CompileStandingOrders(prefDir)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprint(os.Stdout, block.Report(knowledge.MaxStandingTokens))
	fmt.Printf("OK：%d est-token（上限 %d）\n", block.Tokens, knowledge.MaxStandingTokens)
	return nil
}
