// 命令 marl 是设计文档 Part 1.3 的 CLI 入口。
//
// 子命令分发（13.9/13.10 起命令面超过一个，Go 的 flag 包不支持子命令
// 形态——flag.Parse 在第一个位置参数处停止；手动剥离）：
//
//	marl init [dir]          # 全自动初始化（阶段 6）
//	marl knowledge lint      # preferences/ 编译产物超限检查（阶段 7）
//	marl status              # Agent 树 / 阻塞状态 / 讨论等待（阶段 8）
//	marl start "任务"        # 人类 spawn 项目 Agent（Part 14.6，阶段 12）
//	marl say "文本"          # 人类 → Actor 的 MsgDirect（Part 14.5）
//
// 无参数 = usage（[阶段 12 修正] 旧形态"无参数 = init 当前目录"是危险
// 缺省——真机验收中把仓库自身初始化成了项目；init 是一次性幂等操作，
// 必须显式发起）。
package main

import (
	"fmt"
	"os"
	"strings"
)

// usageText 是子命令面板（缺省命令的打印面）。
const usageText = `usage: marl <command> [args]

commands:
  init [dir]               初始化项目（fossil 仓库 + .marl 骨架；幂等一次性）
  knowledge lint|pull|promote   知识库编译检查 / 全局库拉取 / 提升
  models probe             模型探活（chat / tool_call / cache 三判据）
  status                   监督树与阻塞状态（人类为根）
  log / attach             对话导出 / tail
  start "任务"             人类 spawn 项目 Agent（attached 运行）
  say "文本"               人类 → Actor 的直接消息（MsgDirect）
  stop                     优雅停止后台任务（--force 强杀）
  doctor                   环境自检（fossil / API key / 骨架 / store）
  version                  构建信息`

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "knowledge":
		err = cmdKnowledge(args)
	case "models":
		err = cmdModels(args)
	case "status":
		err = cmdStatus(args)
	case "log":
		err = cmdLog(args)
	case "attach":
		err = cmdAttach(args)
	case "start":
		err = cmdStart(args)
	case "say":
		err = cmdSay(args)
	case "stop":
		err = cmdStop(args)
	case "version":
		err = cmdVersion(args)
	case "doctor":
		err = cmdDoctor(args)
	default:
		if cmd == "" && len(args) == 0 {
			fmt.Println(usageText)
			return
		}
		err = fmt.Errorf("unknown command %q（无参数 = usage；init 须显式发起）\n\n%s", cmd, usageText)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "marl %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
