// 子命令 marl tui：bubbletea 仪表盘（设计文档 docs/design/tui_design.md
// 的实现入口）。连接模式自动探测：daemon 在跑 → HTTP；否则 → 进程内
// App（等价"带界面的 marl start"）——对上层透明。
//
// 场景约束：TUI 需要真终端（TTY）。无 TTY（管道/CI）时退出并提示。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/RobiNexy/Marl/internal/frontend/gateway"
	"github.com/RobiNexy/Marl/internal/tui"

	tea "github.com/charmbracelet/bubbletea"
)

func cmdTui(args []string) error {
	fs := flag.NewFlagSet("marl tui", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// TTY 守卫：bubbletea 在非 TTY 下的输入面是死的——显式退出比"看起来
	// 卡住"诚实（管道里跑测试/脚本的场景）。
	if fi, _ := os.Stdin.Stat(); fi != nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		return fmt.Errorf("marl tui 需要交互式终端（TTY）；脚本请用 marl start/status/say")
	}
	root := filepathAbs(*dir)
	gw, err := gateway.New(root, gateway.Config{Version: buildVersion()})
	if err != nil {
		return err
	}
	defer gw.Close()
	p := tea.NewProgram(tui.New(gw),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err = p.Run()
	return err
}
