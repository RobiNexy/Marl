package agent

// 私有段与常驻块（Part 6.4 修正 1 / 6.10 / 12.3，13.9 阶段 7）。
//
// 段序契约（compileView 的 frozen 前缀分区）：
//
//	system     —— 全项目共享的冻结前缀（byte-stable 由构造方保证）
//	standing   —— preferences/ 编译产物；全项目同源 → 共享前缀的一部分
//	knowledge  —— 私有段 <agent_context>；**每 Agent 恒定** → 与该 Agent 的
//	              缓存桶自洽（Patch 1 §10.15：每 Agent 独立缓存桶意味着
//	              Agent 私有但恒定的内容仍可进自己的稳定前缀）
//	history    —— stable，View 驱动
//
// 私有段的内容纪律（Part 6.10）：
//   - 只说有什么，绝不说没什么——hidden 挂载不出现（"你不能访问 X"
//     这句话本身就是把 X 递给它）；
//   - 深度事实只陈述不解释（<depth current max/>，不写"不能再 fork"
//     ——LLM 会算）；
//   - 内容必须短（每轮都在的固定成本，目标 200 est-token 以内）。

import (
	"fmt"
	"strings"

	"marl/internal/types"
	"marl/internal/wire"
)

// standingSegment 编译常驻块段（StandingOrders 为空时返回 nil）。
//
// Stability=frozen：它是缓存前缀的一部分（Part 12.3 档 1）。byte-stability
// 由 knowledge.CompileStandingOrders 的归一化 + golden test 保证，本函数
// 只搬运不加工。
func standingSegment(content string) *wire.Segment {
	if content == "" {
		return nil
	}
	return &wire.Segment{Kind: wire.SegStanding, Speaker: wire.SpeakerFramework, Content: content, Stability: types.StabilityFrozen}
}

// privateSegment 编译私有段 <agent_context>。
//
// depth/maxDepth 是 Agent 身份的一部分（fork 时的不可变输入，Part 8.3）；
// ns 可为 nil（最小装配 / 无命名空间约束的测试语境）；task 可为空。
// 三者皆无信息的场景不存在（depth 总是已知），但保持空判断让极端装配
// 显式退化为"无私有段"而不是垃圾 XML。
//
// Stability=frozen 的同一理由：内容在本 Agent 生命周期内逐字节不变，
// 每 Agent 缓存桶内的前缀命中因此得以保留。
func privateSegment(depth, maxDepth int, ns *types.Namespace, task string) *wire.Segment {
	content := renderAgentContext(depth, maxDepth, ns, task)
	if content == "" {
		return nil
	}
	return &wire.Segment{Kind: wire.SegKnowledge, Speaker: wire.SpeakerFramework, Content: content, Stability: types.StabilityFrozen}
}

// renderAgentContext 渲染 <agent_context>（私有段的唯一文本来源）。
//
// 后置条件：返回值非空 ⟺ depth 段（或 ns/task 段）至少出现一处；
// hidden 挂载永不出现（hidden 即"对这个 Agent 不存在"，Part 6.10 约束 1）。
func renderAgentContext(depth, maxDepth int, ns *types.Namespace, task string) string {
	var sb strings.Builder
	sb.WriteString("<agent_context>\n")
	fmt.Fprintf(&sb, "  <depth current=\"%d\" max=\"%d\"/>\n", depth, maxDepth)
	if ns != nil {
		writes := mountsOf(ns, types.PathWrite)
		reads := mountsOf(ns, types.PathRead)
		if len(writes) > 0 || len(reads) > 0 {
			sb.WriteString("  <workspace>\n")
			if len(writes) > 0 {
				fmt.Fprintf(&sb, "    <writable>%s</writable>\n", strings.Join(writes, ", "))
			}
			if len(reads) > 0 {
				fmt.Fprintf(&sb, "    <readable>%s</readable>\n", strings.Join(reads, ", "))
			}
			sb.WriteString("  </workspace>\n")
		}
	}
	if task != "" {
		fmt.Fprintf(&sb, "  <task>%s</task>\n", singleLine(task))
	}
	sb.WriteString("</agent_context>")
	return sb.String()
}

// mountsOf 取给定模式的挂载模式串（只陈述不解释——私有段纪律）。
func mountsOf(ns *types.Namespace, mode types.PathMode) []string {
	out := make([]string, 0, len(ns.Mounts))
	for _, m := range ns.Mounts {
		if m.Mode == mode {
			out = append(out, m.Pattern)
		}
	}
	return out
}

// singleLine 把任务描述压成单行（XML 单行元素；换行会破坏消费方按行
// diff 前缀的稳定性——任务描述在 spawn 时固定，但多行形态没必要）。
func singleLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
