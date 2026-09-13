package ladder

// 证据累积器与升级判据（Part 7.2 / 7.3）。
//
// 原则：升级决策基于**可观测证据**，不依赖 LLM 自述（ADR-0013）。
// 证据类型与权重来自 Part 7.2 的表（按权重降序）：
//
//	连续失败（高）/ 工具格式错误（高）/ 无进展循环（中）/
//	子任务失败率（中）/ 压缩收益不足（低）
//
// 明确**不计入**升级的情况（Part 7.2）由调用侧保证：人类 reject、
// Orchestrator 调用失败（r0）、单次工具调用失败（如锚点不匹配）——
// 本累积器只暴露 Record* 入口，调用侧决定喂什么。

import (
	"fmt"
	"sync"

	"github.com/RobiNexy/Marl/internal/types"
)

// EvidencePolicy 是证据权重与升级阈值。
//
// 零值契约（危险阈值类，ADR-0014）：零值 Threshold 会让"score >= 0"恒成立，
// 即**任何证据都立刻升级**——第一次失败就跳到最贵模型。因此 Validate 要求
// Threshold > 0；各权重 >= 0（0 = 该类证据不计分，是显式关闭而非遗漏——
// 与 Threshold 的危险方向不同，故允许零值但要求显式）。
//
// 默认值（由配置层物化，本包不提供 DefaultXxx()）：Part 7.3 的示例给出
// "连续失败 2 次 → evidence_score=0.8"，据此每次失败 0.4；格式错误同级 0.4；
// 无进展每轮 0.15（3 轮 = 0.45，单靠它不触发，需叠加）；子任务失败率按
// 比率 × 1.0（"全军覆没"单独触发，>50% 需叠加其它证据）；压缩收益不足 +0.1。
type EvidencePolicy struct {
	Threshold              float64 // 累积证据权重阈值；必须 > 0
	FailureWeight          float64 // 每次连续失败（>= 0）
	FormatErrorWeight      float64 // 每次工具格式错误（>= 0）
	NoProgressWeight       float64 // 每轮无进展（>= 0）
	ChildFailureRateWeight float64 // 子任务失败率系数（>= 0）
	ReclaimLowWeight       float64 // 压缩收益不足（>= 0）
}

// Validate 报告策略是否可用。
//
// 失败：Threshold <= 0（含 NaN——NaN 与任何数比较为 false，会让升级永不
// 触发，是静默失效）；任一权重为负或 NaN。
func (p EvidencePolicy) Validate() error {
	if !(p.Threshold > 0) {
		return fmt.Errorf("evidence policy: threshold %v must be > 0", p.Threshold)
	}
	for _, w := range []struct {
		name string
		v    float64
	}{
		{"failure", p.FailureWeight},
		{"format_error", p.FormatErrorWeight},
		{"no_progress", p.NoProgressWeight},
		{"child_failure_rate", p.ChildFailureRateWeight},
		{"reclaim_low", p.ReclaimLowWeight},
	} {
		if w.v < 0 || w.v != w.v { // NaN: v != v
			return fmt.Errorf("evidence policy: weight %s = %v must be >= 0 and finite", w.name, w.v)
		}
	}
	return nil
}

// Accumulator 是一个 Agent（任务内）的证据累积器。
//
// 生命周期：任务开始时创建，升级成功后 Reset（Part 7.3"清空证据累积"）。
//
// 并发：可被多 goroutine 喂证据（mailbox pump 与主循环并存），内部互斥。
type Accumulator struct {
	mu     sync.Mutex
	policy EvidencePolicy
	ev     types.UpgradeEvidence
	// consecutive 记录"连续"失败：成功证据（RecordProgress）清零。
	consecutiveFailures int
}

// NewAccumulator 构造累积器。
//
// 失败：policy 校验不过（启动期 fail fast）。
func NewAccumulator(policy EvidencePolicy) (*Accumulator, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Accumulator{policy: policy}, nil
}

// RecordFailure 记一次失败（连续计数 +1）。
func (a *Accumulator) RecordFailure() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consecutiveFailures++
	a.ev.ConsecutiveFailures = a.consecutiveFailures
}

// RecordFormatError 记一次工具格式错误（tool_call JSON 解析失败）。
func (a *Accumulator) RecordFormatError() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev.ToolFormatErrors++
}

// RecordNoProgress 记一轮无进展（无成功的工具调用、无可见推进）。
func (a *Accumulator) RecordNoProgress() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev.NoProgressRounds++
}

// RecordProgress 记一次进展（成功的工具调用或可见回复）：
// 连续失败计数清零（"连续"的语义），无进展计数清零。
func (a *Accumulator) RecordProgress() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consecutiveFailures = 0
	a.ev.ConsecutiveFailures = 0
	a.ev.NoProgressRounds = 0
}

// RecordChildFailures 记一批子的结果（Part 7.2 子任务失败率）。
// total <= 0 时忽略（没有子就没有失败率，除以零是程序员错误）。
func (a *Accumulator) RecordChildFailures(total, failed, overBudget int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if total <= 0 {
		return
	}
	rate := float64(failed+overBudget) / float64(total)
	// 多批取最差（失败率是"这批规划质量"的信号，取均值会稀释单批的全军覆没）。
	if rate > a.ev.ChildFailureRate {
		a.ev.ChildFailureRate = rate
	}
}

// RecordReclaimLow 记一次压缩收益不足。
func (a *Accumulator) RecordReclaimLow() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev.CompressionReclaimLow = true
}

// Snapshot 返回证据的只读副本（审计/调试用）。
func (a *Accumulator) Snapshot() types.UpgradeEvidence {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ev
}

// Score 按权重计算累积证据分。
//
// 并发：安全（内部互斥）。
func (a *Accumulator) Score() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scoreLocked()
}

func (a *Accumulator) scoreLocked() float64 {
	p := a.policy
	return float64(a.ev.ConsecutiveFailures)*p.FailureWeight +
		float64(a.ev.ToolFormatErrors)*p.FormatErrorWeight +
		float64(a.ev.NoProgressRounds)*p.NoProgressWeight +
		a.ev.ChildFailureRate*p.ChildFailureRateWeight +
		boolWeight(a.ev.CompressionReclaimLow, p.ReclaimLowWeight)
}

func boolWeight(b bool, w float64) float64 {
	if b {
		return w
	}
	return 0
}

// Evaluate 给出升级决策（Part 7.3）。
//
// 规则：score >= threshold 且下一档存在 → 升级（ShouldUpgrade + ToRung +
// 带分解的 Reason）；下一档不存在（已在最高档）→ 不升级，Reason 说明
// "已在最高档"（escalate 给父/人类是后续阶段的路径，阶段 4 只停在这里）；
// 分数不足 → 不升级。
//
// 并发：安全。
func (a *Accumulator) Evaluate(ladder *types.Ladder, from types.RungID) types.UpgradeDecision {
	a.mu.Lock()
	score := a.scoreLocked()
	ev := a.ev
	a.mu.Unlock()

	dec := types.UpgradeDecision{FromRung: from}
	if score < a.policy.Threshold {
		return dec
	}
	next := nextRung(ladder, from)
	if next == "" {
		dec.Reason = fmt.Sprintf("evidence score %.2f >= threshold %.2f but already at top rung %s (evidence: %+v)",
			score, a.policy.Threshold, from, ev)
		return dec
	}
	dec.ShouldUpgrade = true
	dec.ToRung = next
	dec.Reason = fmt.Sprintf("evidence score %.2f >= threshold %.2f (failures=%d formatErrors=%d noProgress=%d childFailureRate=%.2f reclaimLow=%v)",
		score, a.policy.Threshold, ev.ConsecutiveFailures, ev.ToolFormatErrors,
		ev.NoProgressRounds, ev.ChildFailureRate, ev.CompressionReclaimLow)
	return dec
}

// Reset 清空证据（升级成功后调用，Part 7.3）。
func (a *Accumulator) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev = types.UpgradeEvidence{}
	a.consecutiveFailures = 0
}

// nextRung 返回 from 的下一档 id；from 不存在或已是最高档返回 ""。
//
// 并发：纯函数。
func nextRung(ladder *types.Ladder, from types.RungID) types.RungID {
	if ladder == nil {
		return ""
	}
	for i, r := range ladder.Rungs {
		if r.ID == from && i+1 < len(ladder.Rungs) {
			return ladder.Rungs[i+1].ID
		}
	}
	return ""
}
