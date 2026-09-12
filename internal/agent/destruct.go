package agent

// 缓存破坏分级（Part 11.4，阶段 11）。
//
// 破坏量是**纯机械可算**的：编排操作影响 View 中位置 P 的条目时，
// P 及其后所有条目的 token 总和就是潜在破坏量（该段缓存必失效），
// 破坏率 = 破坏量 / View 总 token。两档（已确认）：
//
//	零破坏   追加 / volatile 段——前缀不变，不进 Gate；
//	低       破坏率 < limits.orchestration.cache_review_pct（默认 10%）→ 直接执行+审计；
//	高       ≥ 阈值 → need_human（GateAsk, KindOrchestration），挂起等审批。
//
// 拒绝/挂起消息必须带**具体数字**（百分比 + token 数）——白盒哲学：
// Agent 看见成本才长出缓存经济直觉。
//
// [接入点边界] 阶段 11 的口径：推理分级与 Gate 评估以**库面**交付
// （本文件），执行点的接位是 View 编排操作发生的唯一路径
// （ eventual：编排技能落地时在 executeToolCall 的编排类分支调用）。
// llm_call 的零破坏（只追加 tool_call 对）与"llm_call 的超额"分开：
// Gate 的 llm_call 规则管"调多少"，本分级管"动了 View 哪里"。

import (
	"fmt"

	"marl/internal/config"
	"marl/internal/types"
)

// cacheDestruction 是一次 View 操作的破坏量测算结果。
type cacheDestruction struct {
	Percent    float64 // 0–100
	DestTokens int     // 绝对破坏量（est token）
	TotalView  int     // 当前 View 总 token（比率的分母）
}

// computeCacheDestruction 计算"位置 P 及其后"的破坏量。
//
// 输入索引语义：view.Items 里的**下标**（Position 排序后的序）；负值或
// 越界 = 追加（破坏量 0——追加新元不伤前缀）。volatile 条目不计入（它们
// 不进缓存前缀）；frozen 条目永不位于 View（它们是编译层折叠的）——
// View 里的 items 全是 stable 面。
// computeCacheDestruction 计算"target Ref 及其后"的破坏量。
//
// 输入：view 的条目（升序）与各条目的**已取** LogEntry 估算（分母/分子
// 同一来源：Log 的 content 估算——与 headroom 同口径 types.EstimateTokens）。
// targetRef 未在 View 里 = 追加形态 → 零破坏（Part 11.4 的反直觉点）。
// 软删除条目不计（不贡献上下文字节）。
func computeCacheDestruction(view *types.ContextView, tokensOf map[types.MessageID]int, targetRef types.MessageID) cacheDestruction {
	res := cacheDestruction{}
	if view == nil {
		return res
	}
	hitting := targetRef == "" // 空 target = 纯追加语义
	for _, it := range view.Items {
		if !it.Visible {
			continue
		}
		t := tokensOf[it.Ref]
		res.TotalView += t
		if !hitting {
			if it.Ref == targetRef {
				hitting = true
			}
			continue // 命中点之前：前缀稳定，不破坏
		}
		res.DestTokens += t
	}
	if res.TotalView > 0 {
		res.Percent = float64(res.DestTokens) / float64(res.TotalView) * 100
	}
	return res
}

// estimateItemTokens 是 single item 的 token 估算：needs Log 的条目
// content——**破坏测算不是纯函数**（要读 Log 的条目内容）；本包的测算
// 入口收 *types.LogEntry（View 的 targetIdx 语义保持：调用方拼装）。
// [调用面: 宿主编排技能落地时，把 Ref → LogEntry 的查询放在 PEP 组装处。]
func estimateItemTokens(e *types.LogEntry) int {
	if e == nil {
		return 0
	}
	if e.TokenEst > 0 {
		return e.TokenEst // Log.Append 时已就地估算（同一函数，同口径）
	}
	return types.EstimateTokens(e.Content)
}

// classifyDestruction 是两级分级的判定（Part 11.4 §4.2；两档 + 零破坏）。
func classifyDestruction(d cacheDestruction, limits config.Limits) (crowd string, needHuman bool) {
	if d.DestTokens == 0 {
		return "none", false
	}
	if d.Percent < limits.CacheReviewPct {
		return "low", false
	}
	return "high", true
}

// fmtDestruction 是拒绝消息面（带数字——Part 11.4 §4.2 的约定）。
func fmtDestruction(d cacheDestruction) string {
	return fmt.Sprintf("破坏率 %.1f%%（%d est-token / 当前上下文 %d est-token）", d.Percent, d.DestTokens, d.TotalView)
}

// gateErrJSON 是 GATE_PENDING_HUMAN 的确切返回形态（带数字）。
func gateErrJSON(d cacheDestruction) map[string]any {
	return map[string]any{
		"error":                  "GATE_PENDING_HUMAN",
		"cache_destroyed_pct":    d.Percent,
		"cache_destroyed_tokens": d.DestTokens,
	}
}

// viewDestructAttrs 是 PEP 的属性表（Gate 的 Attributes 口径）。
func viewDestructAttrs(d cacheDestruction, viewTotal int) map[string]any {
	return map[string]any{
		"cache_destroyed_pct":    d.Percent,
		"cache_destroyed_tokens": d.DestTokens,
		"view_total_tokens":      viewTotal,
	}
}

var _ = fmt.Sprintf
