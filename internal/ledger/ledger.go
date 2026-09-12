// Package ledger 实现成本账本的服务层（Part 7.5 / 7.6，13.6 阶段 4）。
//
// 分层：store.Ledger（store 包的接口，SQLite 落地）只管"账目 ↔ SQL"的翻译；
// 本包持有计价知识（成本公式）与记账入口（RecordMain / RecordOrchestration），
// 并提供 Part 7.6 成本报表的渲染。依赖方向：ledger → store + wire，
// 两者都不反向。
//
// 成本公式（口径钉死，见 ADR-0027）：
//
//	cost = (Prompt − CacheRead) × In + CacheRead × CachedIn
//	     + (Completion − Reasoning) × Out + Reasoning × Reasoning
//	     （全部除以 1e6 换算成每百万 token 的单价口径）
//
// Completion − Reasoning 的理由：DeepSeek 的 completion_tokens **包含**
// 思维链（reasoning_tokens 是 completion_tokens_details 的子字段，阶段 1
// 实测），Denormalizer 原样保留了这一口径；账单要拆分"可见输出"与"思维链"
// 两个价格档，必须先减再加。本公式永远假设"Reasoning ⊆ Completion"；
// 若未来某厂商的 completion 不含思维链，由 Denormalizer 在归一化时拆分
// （Part 10.11 的口径纪律），而不是让每个消费点自己判断。
package ledger

import (
	"context"
	"fmt"

	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
)

// Cost 按计价计算一次调用的成本（币种由 pricing.Currency 决定）。
//
// 并发：纯函数。
func Cost(p wire.Pricing, u types.TokenUsage) float64 {
	missIn := u.PromptTokens - u.CacheReadTokens
	if missIn < 0 {
		missIn = 0 // 防御异常上报（CacheRead > Prompt 是厂商数据错误）
	}
	visibleOut := u.CompletionTokens - u.ReasoningTokens
	if visibleOut < 0 {
		visibleOut = 0
	}
	const perMTok = 1e6
	return float64(missIn)*p.InPerMTok/perMTok +
		float64(u.CacheReadTokens)*p.CachedInPerMTok/perMTok +
		float64(visibleOut)*p.OutPerMTok/perMTok +
		float64(u.ReasoningTokens)*p.ReasoningPerMTok/perMTok
}

// Recorder 是记账入口：把"一次调用"翻译成 LedgerEntry（算钱、补币种），
// 落到 store.Ledger。
//
// 零值契约：Recorder{} 不可用（store/catalog 为 nil）；必须经 New。
type Recorder struct {
	store   store.Ledger
	catalog wire.Catalog
}

// New 构造 Recorder。
//
// 失败：store 或 catalog 为 nil（缺 catalog 的记账无法计价——把所有花费记 0
// 会让报表失真，宁可拒绝启动）。
func New(st store.Ledger, catalog wire.Catalog) (*Recorder, error) {
	if st == nil || catalog == nil {
		return nil, fmt.Errorf("ledger: store and catalog are required")
	}
	return &Recorder{store: st, catalog: catalog}, nil
}

// recordFor 是三条入口的共用实现。
//
// 计价按 (Binding.Model, Binding.Endpoint) 查——rung id 只是报表标签，
// 不是计价键。计价缺失 = "这条调用无法计价"，不是"这条调用不要钱"
// （wire.Catalog.Pricing 契约）：拒绝记账并上抛，账目缺失必须可见，
// 而不是静默记 0。
func (r *Recorder) recordFor(ctx context.Context, callType store.CostCategory, taskID types.TaskID, agentID types.AgentID, b types.Binding, usage *types.TokenUsage) error {
	if usage == nil {
		// 用量未知（调用失败或厂商未回传）≠ 用量为零：零值记账会把"未知"
		// 伪装成"免费"（types.TokenUsage 的零值契约）。不记账是已知低估，
		// 会在"调用数与 token 数对不上"时暴露——比静默记 0 诚实。
		return nil
	}
	pricing, err := r.catalog.Pricing(b.Model, b.Endpoint)
	if err != nil {
		return fmt.Errorf("ledger: pricing for %s@%s: %w", b.Model, b.Endpoint, err)
	}
	entry := &store.LedgerEntry{
		TaskID:     taskID,
		AgentID:    agentID,
		Rung:       b.RungID,
		CallType:   callType,
		TokenUsage: *usage,
		Cost:       Cost(pricing, *usage),
		Currency:   pricing.Currency,
	}
	return r.store.Record(ctx, entry)
}

// RecordMain 记一次主任务调用（CallMain）。
func (r *Recorder) RecordMain(ctx context.Context, taskID types.TaskID, agentID types.AgentID, b types.Binding, usage *types.TokenUsage) error {
	return r.recordFor(ctx, store.CallMain, taskID, agentID, b, usage)
}

// RecordOrchestration 记一次编排调用（强制 CallOrchestration，Part 7.5）。
func (r *Recorder) RecordOrchestration(ctx context.Context, taskID types.TaskID, agentID types.AgentID, b types.Binding, usage *types.TokenUsage) error {
	return r.recordFor(ctx, store.CallOrchestration, taskID, agentID, b, usage)
}

// RecordLLMCall 记一次 llm_call（Part 11.2 §2.8：call_type=llm_call；
// 报表按 purpose 分组回答"编排到底花了多少"——查询，不是第二套账本）。
func (r *Recorder) RecordLLMCall(ctx context.Context, taskID types.TaskID, agentID types.AgentID, b types.Binding, usage *types.TokenUsage) error {
	return r.recordFor(ctx, store.CallLLMCall, taskID, agentID, b, usage)
}

// RecordDiscussion 记一次讨论起草（强制 CallDiscussion）。
func (r *Recorder) RecordDiscussion(ctx context.Context, taskID types.TaskID, agentID types.AgentID, b types.Binding, usage *types.TokenUsage) error {
	return r.recordFor(ctx, store.CallDiscussion, taskID, agentID, b, usage)
}

// TaskSummary 透传 store.Ledger.TaskSummary（报表的输入）。
func (r *Recorder) TaskSummary(ctx context.Context, taskID types.TaskID) (*store.TaskCostSummary, error) {
	return r.store.TaskSummary(ctx, taskID)
}

// RecordModelSwitch 透传 store.Ledger.RecordModelSwitch（Part 7.7 的
// 缓存失效审计；调用纪律见接口契约——只有换 model_id 才调用）。
func (r *Recorder) RecordModelSwitch(ctx context.Context, ev *store.ModelSwitchEvent) error {
	return r.store.RecordModelSwitch(ctx, ev)
}
