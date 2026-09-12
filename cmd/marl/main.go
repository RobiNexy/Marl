// 命令 marl 是设计文档 Part 1.3 的 CLI 入口。
//
// 子命令分发（13.9/13.10 起命令面超过一个，Go 的 flag 包不支持子命令
// 形态——flag.Parse 在第一个位置参数处停止；手动剥离）：
//
//	marl init [dir]          # 全自动初始化（阶段 6）
//	marl knowledge lint      # preferences/ 编译产物超限检查（阶段 7）
//	marl status              # Agent 树 / 阻塞状态 / 讨论等待（阶段 8）
//
// 无参数 = init（阶段 6 的唯一入口；保持向后兼容的裸调用习惯）。
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	args := os.Args[1:]
	cmd := "init" // 缺省 = init（阶段 6 的裸调用兼容）
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "knowledge":
		err = cmdKnowledge(args)
	case "status":
		err = cmdStatus(args)
	default:
		err = fmt.Errorf("unknown command %q (want: init | knowledge | status)", cmd)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "marl %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
