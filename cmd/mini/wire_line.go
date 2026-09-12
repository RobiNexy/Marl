package main

// 线路装配：直接通路（阶段 2 无 Pool）与脚本化回放（-dry-run）。

import (
	"context"
	"encoding/json"
	"fmt"

	"marl/internal/types"
	"marl/internal/wire"
)

// miniCaps 是阶段 2 的能力表替身（与 probe 的 staticCaps 同源同取舍：
// 阶段 2 没有配置层，models.yaml 在阶段 7 及之后；未知模型报错不回落）。
type miniCaps struct {
	endpoint string
	modelID  string
}

// EffectiveCaps 实现 wire.CapsProvider。
func (c *miniCaps) EffectiveCaps(modelID, endpoint string) (wire.ModelCaps, error) {
	if modelID != c.modelID {
		return wire.ModelCaps{}, fmt.Errorf("caps: unknown model %q (only %q is registered in phase 2)", modelID, c.modelID)
	}
	if endpoint != c.endpoint {
		return wire.ModelCaps{}, fmt.Errorf("caps: unknown endpoint %q", endpoint)
	}
	return wire.ModelCaps{
		Has:             []types.Capability{types.CapToolCall, types.CapJSONMode, types.CapThinking},
		MaxContext:      65536,
		MaxOutput:       8192,
		CacheMode:       wire.CacheImplicitPrefix,
		ThinkingControl: wire.ThinkControlLevel,
		ThinkingLevels:  []string{"none", "low", "high", "max"},
	}, nil
}

// directLine 是 LLMExecutor 的直接通路实现：
//
//	CanonicalRequest → Normalizer.BuildRequest → Assert →
//	Adapter.Execute（厂商 HTTP）→ Denormalize → WireTurn。
//
// 阶段 2 直连（无排队/熔断/重试——那些是 Pool 的职责集合，阶段 3+ 落地）；
// 失败语义遵守 wire 契约：ctx 取消原样返回、厂商错误不进 error。
type directLine struct {
	norm        *wire.OpenAICompatNormalizer
	denorm      *wire.OpenAICompatDenormalizer
	adapter     *wire.DeepSeekChatAdapter
	binding     types.Binding
	bucketField string
}

// ExecuteTurn 实现 agent.LLMExecutor。
func (d *directLine) ExecuteTurn(ctx context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	// Degradations 留空接收（`_`）：阶段 2 没有审计表，丢弃不进入任何账面。
	// 这是与 Normalizer 契约的一个已知张力——降级记录的落点在阶段 3 的
	// audit_events 落地时一并补。
	wr, _, err := d.norm.BuildRequest(req, d.binding)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if err := d.norm.Assert(wr); err != nil {
		return nil, fmt.Errorf("assert (framework/config error, not vendor): %w", err)
	}
	resp, err := d.adapter.Execute(ctx, wr, d.binding)
	if err != nil {
		return nil, err // 适配器的错误语义已按 wire 契约归一（ctx.Err 原样走）
	}
	return d.denorm.Denormalize(resp)
}

// cannedLine 是 -dry-run 的脚本化回放（agent.LLMExecutor 的最小替身）：
// 三轮脚本与 13.4 的交付判据一致——list_dir → file_read → 最终回复。
// 它不检验协议装配（那是单元测试的事），只检验主循环的骨架与落库。
type cannedLine struct {
	round int
}

// ExecuteTurn 实现 agent.LLMExecutor。
func (c *cannedLine) ExecuteTurn(_ context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	defer func() { c.round++ }()
	switch c.round {
	case 0:
		return toolCallTurn(mkCall("list_dir", map[string]any{"path": ".", "depth": 1})), nil
	case 1:
		return toolCallTurn(mkCall("file_read", map[string]any{"path": "README.md"})), nil
	default:
		return replyTurn("已列出目录并读完 README.md：这是一个 Go 项目（module marl）。任务完成。"), nil
	}
}

// mkCall / turn 构造器：与 agent 测试侧同样的形态（Entry 由"Denormalizer
// 产出"的结构直接给出）。
func mkCall(name string, args map[string]any) types.ToolCall {
	b, _ := json.Marshal(args)
	return types.ToolCall{ID: "call-" + name, Name: name, Arguments: json.RawMessage(b)}
}

func toolCallTurn(calls ...types.ToolCall) *wire.WireTurn {
	return &wire.WireTurn{Outcomes: []wire.Outcome{{ToolCalls: calls}}}
}

func replyTurn(text string) *wire.WireTurn {
	return &wire.WireTurn{Outcomes: []wire.Outcome{{
		Reply: text,
		Entry: types.LogEntry{
			Content:  text,
			Role:     types.RoleAssistantReply,
			Prov:     types.ProvOriginal,
			Audience: types.AudienceBoth,
			Meta:     map[string]any{"finish_reason": "stop"},
		},
	}}}
}
