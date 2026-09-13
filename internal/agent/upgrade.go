package agent

// 阶梯升级的接入点（Part 7.2 / 7.3，13.6 阶段 4）。
//
// 职责切分：
//   - 证据的**采集**在这里（只有主循环看得到每轮的原始信号）；
//   - 证据的**权重与判据**在 ladder.Accumulator（可配置策略）；
//   - 重绑定的**候选选择**在 ladder Router（两阶段调度）；
//   - 升级的**账目与审计**在这里落（主循环是唯一知道"何时换了绑定"的地方）。

import (
	"context"
	"fmt"

	"github.com/RobiNexy/Marl/internal/ladder"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// 审计 action 常量（审计是开放集合，但 Action 名必须定义常量——禁止调用点
// 写字面量，见 store.AuditEvent 契约）。
const (
	AuditModelUpgrade = "model_upgrade"
	AuditModelSwitch  = "model_switch"
)

// UpgradeConfig 是阶梯升级的装配参数（Config.Upgrader 非 nil 即启用）。
//
// 零值契约：零值不可用（Router/Ladder/Catalog 为 nil、Policy 零值会让
// "任何证据都升级"）；由 New 校验拒绝。
type UpgradeConfig struct {
	// Router 是两阶段调度器（重绑定时从当前档位 +1 起选）。
	Router wire.Router
	// Ladder 是阶梯配置（含每档 thinking，ADR-0022 的唯一真相）。
	Ladder *ladder.Config
	// Catalog 提供升级后模型的窗口大小（MaxContextTokens 的更新源）。
	Catalog wire.Catalog
	// Requirement 是本 Agent 的能力需求（原始 Bind 的同一份——升级不改需求，
	// 只换档位；需求变化等价于换 Agent，Part 6.9）。
	Requirement types.Requirement
	// Policy 是证据权重与升级阈值。
	Policy ladder.EvidencePolicy
}

// newAccumulator 构造证据累积器（New 期调用，校验失败即启动失败）。
func (u *UpgradeConfig) newAccumulator() (*ladder.Accumulator, error) {
	return ladder.NewAccumulator(u.Policy)
}

// feedRound 把一轮的可观测信号喂给累积器。
//
// 信号 → 证据的映射（Part 7.2 + wire.ErrorClass 的语义表）：
//   - 工具格式错误（BAD_ARGS）→ RecordFormatError（高权重）；
//   - 厂商错误分类：ErrMalformed / ErrAuthQuota / ErrCapability 及未分类 →
//     RecordFailure；ErrTransient（限流，不是能力问题）、ErrContextOverflow
//     （压缩的领地）、ErrContentFilter（交回 LLM）**不记**；
//   - 本轮无成功工具调用且无可见回复 → RecordNoProgress；
//   - 反之（有成功调用或有回复）→ RecordProgress（清"连续"计数）。
func (a *Agent) feedRound() {
	if a.acc == nil {
		return
	}
	for i := 0; i < a.roundFormatErrors; i++ {
		a.acc.RecordFormatError()
	}
	if a.roundErrorClass.IsError() {
		switch a.roundErrorClass {
		case wire.ErrTransient, wire.ErrContextOverflow, wire.ErrContentFilter, wire.ErrNone:
			// 不计入升级证据（Part 7.2 的"不计入"清单 + ErrorClass 语义表）。
		default:
			a.acc.RecordFailure()
		}
	}
	if a.roundProgress {
		a.acc.RecordProgress()
	} else {
		a.acc.RecordNoProgress()
	}
}

// maybeUpgrade 在每轮结束后检查证据并执行升级（Part 7.3）。
//
// 升级动作（全部显式，顺序即语义）：
//  1. Router.Bind 从当前档位 +1 起重绑（单调不回退；缓存亲和由此保证）；
//  2. SetBinding（thinking 档位随之切换——ADR-0022 的唯一真相）；
//  3. 窗口大小按新模型能力表刷新（两个模型 tokenizer 不同，估算口径作废，
//     见探测报告 §3.10——lastCompressTokens 一并清零防热循环误判）；
//  4. 记审计 model_upgrade（from/to/reason/evidence）；
//  5. 换了 model_id 时记 model_switch（缓存失效审计，Part 7.7；r0→r1 同模型
//     不记——采样参数与 thinking 档位不在缓存键内）；
//  6. 追加 Transient 告知 LLM（Part 3.6：用完即扔，不入 Log）；
//  7. 清空证据累积（Part 7.3）。
//
// 失败：Bind 失败（候选不足等）→ 记审计并**放弃本次升级**（停在当前档位是
// 安全方向；把证据丢掉会让下一轮重复触发——因此 Reset 只在成功路径执行）。
func (a *Agent) maybeUpgrade(ctx context.Context) error {
	if a.upgrade == nil || a.acc == nil || !a.bindingSet {
		return nil
	}
	dec := a.acc.Evaluate(a.upgrade.Ladder.Ladder, a.binding.RungID)
	if !dec.ShouldUpgrade {
		return nil
	}
	curIdx := a.binding.RungIndex
	nb, err := a.upgrade.Router.Bind(a.upgrade.Requirement, a.id, a.upgrade.Ladder.Ladder, curIdx+1)
	if err != nil {
		// 升级失败是可观测事件（不是静默放弃）：记审计，保留证据。
		a.auditf(ctx, AuditModelUpgrade, string(a.binding.RungID), map[string]any{
			"from": string(a.binding.RungID), "to": string(dec.ToRung),
			"reason": dec.Reason, "error": err.Error(), "upgraded": false,
		})
		return fmt.Errorf("agent: upgrade bind from %s: %w", a.binding.RungID, err)
	}
	old := a.binding
	if err := a.SetBinding(nb); err != nil {
		return fmt.Errorf("agent: upgrade rebind: %w", err)
	}
	// 窗口刷新 + 估算口径作废。
	if caps, err := a.upgrade.Catalog.EffectiveCaps(nb.Model, nb.Endpoint); err == nil && caps.MaxContext > 0 {
		a.maxContextTokens = caps.MaxContext
	}
	a.lastCompressTokens = 0
	// 审计：升级。
	a.auditf(ctx, AuditModelUpgrade, string(nb.Model), map[string]any{
		"from_rung": string(old.RungID), "to_rung": string(nb.RungID),
		"from_model": old.Model, "to_model": nb.Model,
		"reason": dec.Reason, "evidence": a.acc.Snapshot(), "upgraded": true,
	})
	// 换 model_id → 缓存失效审计（真实统计：本绑定周期内累计的命中/未命中）。
	if old.Model != nb.Model {
		_ = a.ledgerRecordModelSwitch(ctx, old, nb, dec.Reason)
	}
	// 告知 LLM（Transient：尾部、用完即扔、不入 Log——Part 3.6）。
	a.transients = append(a.transients,
		fmt.Sprintf("系统提示：任务难度较高，已从 %s 切换到更强档位 %s。请继续完成任务。",
			old.RungID, nb.RungID))
	// 清空证据（只在成功路径）。
	a.acc.Reset()
	return nil
}

// auditf 记一条审计事件（Audit 为 nil 时跳过——测试与最小装配的显式决定）。
func (a *Agent) auditf(ctx context.Context, action, target string, payload map[string]any) {
	if a.audit == nil {
		return
	}
	ev := &store.AuditEvent{AgentID: a.id, Action: action, Target: target, Payload: payload}
	if err := a.audit.Append(ctx, ev); err != nil {
		// 审计写入失败不回滚业务，但必须可见（store.AuditStore 契约的
		// 对偶：审计不丢事实，写不进去也不能假装写了）。
		fmt.Printf("marl: audit append failed (action=%s): %v\n", action, err)
	}
}

// ledgerRecordModelSwitch 记换模型的缓存失效审计（Part 7.7）。
//
// 数字来自真实统计：cacheHits/cacheMiss 是本绑定周期内每次调用 usage 的
// 累计（agent 侧唯一持有逐轮 usage 的地方）。返回错误只打印不中断——
// 升级已完成，审计失败是可观测损失而非正确性风险。
func (a *Agent) ledgerRecordModelSwitch(ctx context.Context, old, nb types.Binding, reason string) error {
	if a.ledger == nil {
		return nil
	}
	ev := &store.ModelSwitchEvent{
		AgentID:           a.id,
		TaskID:            a.taskID,
		FromModel:         old.Model,
		ToModel:           nb.Model,
		Reason:            reason,
		CacheHitsBefore:   a.cacheHits,
		CacheWritesBefore: a.cacheMiss,
	}
	if err := a.ledger.RecordModelSwitch(ctx, ev); err != nil {
		fmt.Printf("marl: model switch audit failed: %v\n", err)
		return err
	}
	a.auditf(ctx, AuditModelSwitch, nb.Model, map[string]any{
		"from": old.Model, "to": nb.Model, "reason": reason,
		"cache_hits_before": a.cacheHits, "cache_writes_before": a.cacheMiss,
	})
	// 新模型从头积累自己的缓存（跨模型缓存隔离，探测报告 §3.10）。
	a.cacheHits, a.cacheMiss = 0, 0
	return nil
}
