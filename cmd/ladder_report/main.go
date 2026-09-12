// 命令 ladder_report 是设计文档 13.6（阶段 4）的交付检查命令：
// 读 supervisor.db 的 ledger 表，按阶梯分组打印成本报表（Part 7.6 格式），
// 附换模型/升级记录。
//
// 用法：
//
//	go run ./cmd/ladder_report -db .marl-mini/supervisor.db -task mini-task
//
// 渲染逻辑在 internal/ledger.Render（纯函数，单元测试覆盖）；本命令只做
// IO（开库、查询、打印）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"marl/internal/ledger"
	"marl/internal/store"
	"marl/internal/types"
)

func main() {
	dbPath := flag.String("db", ".marl-mini/supervisor.db", "SQLite 数据库路径")
	taskID := flag.String("task", "", "任务 id（空 = 全部任务的合并视图不支持，必须指定）")
	flag.Parse()
	if *taskID == "" {
		fmt.Fprintln(os.Stderr, "ladder_report: -task 必须指定（跨任务合并视图需要任务表，阶段 8+）")
		os.Exit(1)
	}
	if err := run(*dbPath, *taskID); err != nil {
		fmt.Fprintf(os.Stderr, "ladder_report: %v\n", err)
		os.Exit(1)
	}
}

func run(dbPath, taskID string) error {
	ctx := context.Background()
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()

	sum, err := st.TaskSummary(ctx, types.TaskID(taskID))
	if err != nil {
		return fmt.Errorf("task summary: %w", err)
	}
	switches, err := st.QueryModelSwitch(ctx, types.TaskID(taskID))
	if err != nil {
		return fmt.Errorf("model switch query: %w", err)
	}
	fmt.Print(ledger.Render(sum, switches))
	return nil
}
