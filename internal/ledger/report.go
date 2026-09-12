package ledger

// Part 7.6 成本报表的渲染。
//
// 放在 ledger 包而不是 cmd/ladder_report：渲染是纯函数（summary + 升级记录
// → 文本），单元测试直接覆盖；cmd 只做 IO（开库、调渲染、打印）。

import (
	"fmt"
	"sort"
	"strings"

	"marl/internal/store"
	"marl/internal/types"
)

// Render 渲染一份任务成本报表（Part 7.6 的表格形态）。
//
// 输入：TaskSummary（store.Ledger.TaskSummary 的产出）+ 换模型/升级记录
// （可 nil）。输出是人读的文本；机器消费请直接读 TaskSummary 结构。
//
// 并发：纯函数。
func Render(sum *store.TaskCostSummary, switches []*store.ModelSwitchEvent) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "任务：%s\n", sum.TaskID)
	fmt.Fprintf(&sb, "时长：%s（账目时间跨度口径）\n", durationString(sum.Duration))
	fmt.Fprintf(&sb, "状态：%s（账本无终态知识，任务管理落地前恒为 running）\n", sum.Status)
	sb.WriteString("\n")

	// 阶梯分项表。
	fmt.Fprintf(&sb, "%-8s %6s %12s %10s %10s %12s\n", "阶梯", "调用数", "Token总量", "思维链占比", "缓存命中", "成本("+sum.Currency+")")
	fmt.Fprintf(&sb, "%s\n", strings.Repeat("-", 64))
	writeRows(&sb, sum.ByLevel, sum.TotalTokens)
	// 编排 / 讨论行。
	if oc, ok := sum.ByCategory[store.CallOrchestration]; ok {
		writeNamedRow(&sb, "编排", oc, sum.TotalTokens)
	}
	if dc, ok := sum.ByCategory[store.CallDiscussion]; ok {
		writeNamedRow(&sb, "讨论", dc, sum.TotalTokens)
	}
	fmt.Fprintf(&sb, "%-8s %6d %12d %10s %12.4f\n", "总计", countCalls(sum), sum.TotalTokens, "-", sum.TotalCost)

	// 升级记录。
	if len(switches) > 0 {
		sb.WriteString("\n升级/换模型记录：\n")
		for _, sw := range switches {
			fmt.Fprintf(&sb, "  %s %s → %s（%s；失效前缓存命中 %d token）\n",
				sw.At.Format("15:04"), sw.FromModel, sw.ToModel, sw.Reason, sw.CacheHitsBefore)
		}
	}

	// 成本构成（按计价口径拆分；从分项聚合近似——缓存命中占比按 token 计）。
	sb.WriteString("\n成本构成（按 token 口径估算）：\n")
	totalCalls := countCalls(sum)
	fmt.Fprintf(&sb, "  调用数 %d，其中缓存命中 %d token（命中率见上表）\n", totalCalls, cacheReadTokens(sum))

	// 建议（机械规则，不是 AI 判断）。
	for _, line := range suggestions(sum) {
		fmt.Fprintf(&sb, "建议：%s\n", line)
	}
	return sb.String()
}

// writeRows 按 rung id 排序写分项行。
func writeRows(sb *strings.Builder, byLevel map[types.RungID]*store.LevelSummary, total int64) {
	ids := make([]types.RungID, 0, len(byLevel))
	for id := range byLevel {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		writeNamedRow(sb, string(id), byLevel[id], total)
	}
}

func writeNamedRow(sb *strings.Builder, name string, lv *store.LevelSummary, total int64) {
	reasoningPct := "0%"
	if lv.Tokens > 0 {
		reasoningPct = fmt.Sprintf("%.0f%%", float64(lv.ReasoningTokens)/float64(lv.Tokens)*100)
	}
	cachePct := "0%"
	if lv.Tokens > 0 {
		cachePct = fmt.Sprintf("%.0f%%", float64(lv.CacheRead)/float64(lv.Tokens)*100)
	}
	fmt.Fprintf(sb, "%-8s %6d %12d %10s %10s %12.4f\n",
		name, lv.Calls, lv.Tokens, reasoningPct, cachePct, lv.Cost)
}

func countCalls(sum *store.TaskCostSummary) int {
	n := 0
	for _, lv := range sum.ByLevel {
		n += lv.Calls
	}
	return n
}

func cacheReadTokens(sum *store.TaskCostSummary) int64 {
	var n int64
	for _, lv := range sum.ByLevel {
		n += lv.CacheRead
	}
	return n
}

// suggestions 是报表的机械建议（Part 7.6 的两条规则）：
//   - 某阶梯思维链占比 > 60% → "刷思维链"嫌疑；
//   - 缓存命中率 < 40% → 检查前缀稳定性。
//
// 并发：纯函数。
func suggestions(sum *store.TaskCostSummary) []string {
	var out []string
	ids := make([]types.RungID, 0, len(sum.ByLevel))
	for id := range sum.ByLevel {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		lv := sum.ByLevel[id]
		if lv.Tokens > 0 {
			share := float64(lv.ReasoningTokens) / float64(lv.Tokens)
			if share > 0.60 {
				out = append(out, fmt.Sprintf("%s 的思维链占比 %.0f%%（>60%%）：输出大部分不可见，性价比存疑", id, share*100))
			}
			hit := float64(lv.CacheRead) / float64(lv.Tokens)
			if hit < 0.40 {
				out = append(out, fmt.Sprintf("%s 的缓存命中率 %.0f%%（<40%%）：检查前缀稳定性（frozen 段是否逐字节一致）", id, hit*100))
			}
		}
	}
	return out
}

// durationString 把时长格式化成人类可读形态。
func durationString(d interface{ String() string }) string {
	if d == nil {
		return "-"
	}
	return d.String()
}
