package main

// 线路装配：直接通路（阶段 2 无 Pool）与脚本化回放（-dry-run）。
// 阶段 3：同一替身按请求形态分流——带 tools 的是主循环调用（按脚本回放），
// 不带 tools 的是编排调用（split 回 JSON、SUM 回七段骨架）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"marl/internal/types"
	"marl/internal/wire"
)

// miniCaps 是阶段 2/3 的能力表替身（与 probe 的 staticCaps 同源同取舍：
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
// 阶段 2/3 直连（无排队/熔断/重试——那些是 Pool 的职责集合，阶段 4+ 落地）；
// 失败语义遵守 wire 契约：ctx 取消原样返回、厂商错误不进 error。
// 它同时满足 agent.LLMExecutor 与 compress.Executor（Go 结构化类型，
// 无需适配层）。
type directLine struct {
	norm        *wire.OpenAICompatNormalizer
	denorm      *wire.OpenAICompatDenormalizer
	adapter     *wire.DeepSeekChatAdapter
	binding     types.Binding
	bucketField string
}

// ExecuteTurn 实现 agent.LLMExecutor / compress.Executor。
func (d *directLine) ExecuteTurn(ctx context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	// Degradations 留空接收（`_`）：阶段 2/3 没有审计表，丢弃不进入任何账面。
	// 这是与 Normalizer 契约的一个已知张力——降级记录的落点在审计表落地时一并补。
	wr, _, err := d.norm.BuildRequest(req, d.binding)
	if err != nil {
		dumpRequest(req, err) // 交付检查工具：失败现场落盘供归因（BuildRequest 失败时 wr 为 nil，转存 Canonical 输入）
		return nil, fmt.Errorf("build request: %w", err)
	}
	if err := d.norm.Assert(wr); err != nil {
		dumpRequest(req, err)
		return nil, fmt.Errorf("assert (framework/config error, not vendor): %w", err)
	}
	resp, err := d.adapter.Execute(ctx, wr, d.binding)
	if err != nil {
		return nil, err // 适配器的错误语义已按 wire 契约归一（ctx.Err 原样走）
	}
	return d.denorm.Denormalize(resp)
}

// dumpRequest 把失败请求的 Canonical 段序列写到固定路径（诊断辅助；
// 只在 mini 这个交付检查工具里做，框架层不做文件副作用）。
func dumpRequest(req *wire.CanonicalRequest, err error) {
	if req == nil {
		return
	}
	b, jerr := json.MarshalIndent(req.Segments, "", "  ")
	if jerr != nil {
		return
	}
	_ = os.WriteFile("/tmp/opencode/failed-request.json", b, 0o644)
	fmt.Fprintf(os.Stderr, "（请求失败现场已写入 /tmp/opencode/failed-request.json：%v）\n", err)
}

// cannedLine 是 -dry-run 的脚本化回放（agent.LLMExecutor / compress.Executor
// 的最小替身）。
//
// 分流规则（阶段 3）：req.Tools 为空 = 编排调用——OutputJSON 时回语义拆分
// 的 JSON（空数组即可，files 任务不触发 split），否则回七段 SUM；带 tools
// 的是主循环调用，按任务脚本回放。分流依据是请求形态而非调用方身份——
// 与真实通路的语义一致（编排调用本来就不带工具表）。
type cannedLine struct {
	task  string
	files int
	root  string // SUM 引用的文件必须真实存在（机械校验）
	round int    // 主循环脚本游标（编排调用不推进）
}

// ExecuteTurn 实现 agent.LLMExecutor / compress.Executor。
func (c *cannedLine) ExecuteTurn(_ context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	if len(req.Tools) == 0 {
		// 编排调用：split → 空段数组；SUM → 七段骨架（引用工作区真实文件）。
		if req.OutputJSON {
			return replyTurn("[]"), nil
		}
		rel, err := filepath.Rel(c.root, filepath.Join(c.root, "files", "f01.txt"))
		if err != nil {
			rel = "files/f01.txt"
		}
		return replyTurn(cannedSUM(rel)), nil
	}
	defer func() { c.round++ }()
	// 阶段 3 的 files 脚本：每轮读一个文件，读完全部后总结。
	if c.task == "files" {
		if c.round < c.files {
			n := c.round + 1
			return withUsage(toolCallTurn(mkCallID("file_read", fmt.Sprintf("call-f%d", n), map[string]any{
				"path": fmt.Sprintf("files/f%02d.txt", n), "mode": "content", "limit": 150,
			}))), nil
		}
		return withUsage(replyTurn(fmt.Sprintf("已按顺序读完 %d 个文件：内容均为循环生成的占位行，无异常。任务完成。", c.files))), nil
	}
	// 阶段 4 的 failing 脚本：两轮参数损坏的 tool_call（BAD_ARGS → 格式
	// 错误证据 ×2 → 阈值 0.8 触发升级到 r1），第三轮"换强模型后"成功。
	if c.task == "failing" {
		switch c.round {
		case 0, 1:
			return withUsage(toolCallTurn(types.ToolCall{
				ID:        fmt.Sprintf("call-bad-%d", c.round),
				Name:      "file_read",
				Arguments: json.RawMessage(`{not json`),
			})), nil
		default:
			return withUsage(replyTurn("已切换到更强档位；文件读取成功：f01.txt 内容为占位文本。任务完成。")), nil
		}
	}
	// 阶段 2 的 readme 脚本。
	switch c.round {
	case 0:
		return withUsage(toolCallTurn(mkCall("list_dir", map[string]any{"path": ".", "depth": 1}))), nil
	case 1:
		return withUsage(toolCallTurn(mkCall("file_read", map[string]any{"path": "README.md"}))), nil
	default:
		return withUsage(replyTurn("已列出目录并读完 README.md：这是一个 Go 项目（module marl）。任务完成。")), nil
	}
}

// cannedSUM 构造一份能通过机械校验的 SUM（七章节 + 工作区真实路径）。
func cannedSUM(relPath string) string {
	return fmt.Sprintf(`## 1. 任务
依次读取 files/ 目录下的全部文件并总结。

## 2. 事实
每个文件为 50 行循环生成的占位文本，无异常内容。

## 3. 文件
- `+"`%s`"+`

## 4. 决策
按文件名顺序逐个读取。

## 5. 未闭
后续文件尚未读取，继续按顺序处理。

## 6. 失败
无。

## 7. 现场
已读若干文件，下一个待读文件按文件名顺序推进。`, relPath)
}

// mkCall / turn 构造器：与 agent 测试侧同样的形态（Entry 由"Denormalizer
// 产出"的结构直接给出）。
func mkCall(name string, args map[string]any) types.ToolCall {
	b, _ := json.Marshal(args)
	return types.ToolCall{ID: "call-" + name, Name: name, Arguments: json.RawMessage(b)}
}

// mkCallID 与 mkCall 同形，但显式指定调用 id（同任务多次调用同一工具时
// id 必须不同——与真实厂商行为一致）。
func mkCallID(name, id string, args map[string]any) types.ToolCall {
	b, _ := json.Marshal(args)
	return types.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(b)}
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

// withUsage 给 turn 的全部 Outcome 填同一份用量（Part 10.11：同一 turn
// 共享一个 *TokenUsage；账本按 turn 记一次）。dry-run 的用量是演示数字
// （真实用量来自厂商 usage）。
func withUsage(turn *wire.WireTurn) *wire.WireTurn {
	u := &types.TokenUsage{PromptTokens: 5000, CompletionTokens: 300}
	for i := range turn.Outcomes {
		turn.Outcomes[i].Usage = u
	}
	return turn
}
