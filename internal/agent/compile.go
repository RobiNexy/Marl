package agent

// 编译层：ContextView → CanonicalRequest（Part 3.4 的映射表）。
//
// 映射决策收到了全局不变量的双重约束：frozen 逐字节稳定（system 前缀这里生成）
// 与 stable 段只追加（View 驱动，Position 升序，不重排）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"marl/internal/skill"
	"marl/internal/types"
	"marl/internal/wire"
)

// RoleForLogRole 把内部角色映射到呈现角色（Part 3.4 默认映射表）。
// 返回零值表示"该角色不出现在 View 层"（如 Transient 不入 Log）。
//
// 并发：纯函数。
func RoleForLogRole(role types.InternalRole) types.WireRole {
	switch role {
	case types.RoleConstraint:
		return types.WireSystem
	case types.RoleAssistantReply, types.RoleThinking:
		return types.WireAssistant
	case types.RoleToolResult:
		return types.WireTool
	default:
		// user_input / shared_memory / sub_task_result / escalation / human_note
		return types.WireUser
	}
}

// toolCallMeta 是 LogEntry.Meta 里工具调用的落 Log 形态。
//
// Arguments 以 string 落 Meta（不用 json.RawMessage）：Meta 是 map[string]any，
// RawMessage（[]byte）进 JSON 会被编码成 base64——审计可读性直接破坏。
type toolCallMeta struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// compileView 把 View 编译成 CanonicalRequest。
//
// 契约（Part 3.4 / wire.Compiler 的边界）：
//   - 保序：按 ViewItem.Position 升序；Stability 单调不减由 View.Validate 保证；
//   - frozen 段：system 前缀在这里生成（Part 3.4 的 L1），逐字节稳定由
//     Config.SystemPrompt 的 byte-stable 契约保证；
//   - Tools 取注册表的全量 schema（Part 6.4 修正 1：与 Profile 无关的工具表）；
//   - 不修改 view / log（并发读者持有它们）。
//
// 失败：View 非法 / 引用了取不到的 Log 条目（真相与投影失配——store 契约
// 里的严重事件，报错而不是跳过：静默丢消息比编译失败危险）。
func (a *Agent) compileView(ctx context.Context) (*wire.CanonicalRequest, error) {
	if os.Getenv("MARL_DEBUG_COMPILE") != "" {
		for i := range a.view.Items {
			it := &a.view.Items[i]
			fmt.Fprintf(os.Stderr, "marl-debug: view[%d] ref=%s role=%s vis=%v pos=%v\n",
				i, it.Ref, it.WireRole, it.Visible, it.Position)
		}
	}
	if err := a.view.Validate(); err != nil {
		return nil, fmt.Errorf("agent: view: %w", err)
	}
	if a.bindingSet {
		// Thinking 档位只配在阶梯上（ADR-0022 的"唯一真相是 Binding.Thinking"）。
		// 在阶段 2 里 binding 是 mini 手工装配的，因此 Thinking 以 Binding 为准、
		// Config.Thinking 是无 Binding 时的回退（10.7 落地缺口 1）。
		a.thinking = a.binding.Thinking
	}

	req := &wire.CanonicalRequest{
		// 段序：frozen 的 system 在前（StabilityRank=0）；历史 turn 全是 stable。
		Segments: []wire.Segment{{
			Kind:      wire.SegSystem,
			Speaker:   wire.SpeakerFramework,
			Content:   a.sysPrompt,
			Stability: types.StabilityFrozen,
		}},
		Tools:    append(toolsFromRegistry(a.skills), intentSchemas()...),
		Sampling: a.sampling,
		Thinking: a.thinking,
	}

	// 编译序 = Position 升序（Part 3.3：Position 是顺序的唯一权威）。
	// 阶段 2 只追加时切片序即 Position 序；阶段 3 起编排操作（reorder /
	// split / 压缩）会改写 Position，编译必须显式排序——稳定排序保证
	// 同 Position 的条目保持切片相对序（确定性的缓存前缀）。
	items := make([]types.ViewItem, len(a.view.Items))
	copy(items, a.view.Items)
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].Position < items[j-1].Position; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	for i := range items {
		item := &items[i]
		if !item.Visible {
			continue // 软删除（exclude_message）：引用仍在，内容不入请求
		}
		entry, err := a.log.Get(ctx, item.Ref)
		if err != nil {
			// Log 契约：ContextView 引用取不到的 Log 是严重事件（外部篡改的信号），
			// 除报错还应记审计——阶段 2 的审计点是错误链路，结构化审计表在后备阶段。
			return nil, fmt.Errorf("agent: view item[%d] ref %s: fetch log: %w", i, item.Ref, err)
		}
		seg, err := segmentForEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("agent: view item[%d] ref %s: %w", i, item.Ref, err)
		}
		if seg == nil {
			// 无线路形态的条目（当前仅 thinking 且配置为 audit-only）。
			// 诊断可见性：静默跳过是"上下文缺一段"类故障的最差形态，
			// 这里打印到 stderr 供交付检查归因（框架层的正式审计点在
			// audit_events 落地后接管）。
			if dbg := os.Getenv("MARL_DEBUG_COMPILE"); dbg != "" {
				fmt.Fprintf(os.Stderr, "marl-debug: segment skipped: ref=%s role=%s content_len=%d meta_keys=%v\n",
					item.Ref, entry.Role, len(entry.Content), metaKeys(entry.Meta))
			}
			continue
		}
		req.Segments = append(req.Segments, *seg)
	}
	// Transient 段（Part 3.6）：尾部追加，Stability=volatile（不变量表：
	// volatile 只允许在尾部）；不入 Log，本轮被模型消费后由 eventLoop 清除。
	for _, t := range a.transients {
		req.Segments = append(req.Segments, wire.Segment{
			Kind:      wire.SegTransient,
			Speaker:   wire.SpeakerFramework,
			Content:   t,
			Stability: types.StabilityVolatile,
		})
	}
	return req, nil
}

// clearTransients 清空本轮 Transient（Part 3.6：下一轮 View 重建时自动丢弃——
// "下一轮"在这里的语义是"本轮编译已把它送达模型之后"）。
func (a *Agent) clearTransients() {
	a.transients = a.transients[:0]
}

// toolsFromRegistry 把注册表 schema 转成协议无关的工具定义（冻结前缀的
// byte 稳定性由 Registry.Schemas 的契约保证，本函数只搬运不加工）。
func toolsFromRegistry(reg skill.Registry) []wire.ToolDef {
	schemas := reg.Schemas()
	out := make([]wire.ToolDef, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, wire.ToolDef{Name: s.Name, Description: s.Description, Parameters: s.Parameters})
	}
	return out
}

// segmentForEntry 把一条 LogEntry 翻译成 Canonical Segment（Part 3.4 的编译步骤）。
//
// 返回 (nil, nil) 表示该条目没有线路形态（不发送）：
//
//   - thinking 且 Audience=AudienceAudit（历史思维链默认 audit-only，
//     Part 3.2：不占上下文 token）；
//   - 其余 thinking（Audience=Both：ADR-0023 的"带 tools 回传"）→
//     assistant 段携带 Reasoning 字段，Content 允许为空（编码器接受
//     Reasoning-only 的 assistant 消息，content 编码为 null）。
func segmentForEntry(e *types.LogEntry) (*wire.Segment, error) {
	seg := wire.Segment{Stability: types.StabilityStable}
	switch e.Role {
	case types.RoleUserInput:
		seg.Kind, seg.Speaker, seg.Content = wire.SegTurn, wire.SpeakerHuman, e.Content
	case types.RoleAssistantReply:
		seg.Kind, seg.Speaker, seg.Content = wire.SegTurn, wire.SpeakerAssistant, e.Content
		// 工具调用历史（主循环 appendAssistantToolCalls 落库到这个键）。
		if calls, ok := decodeToolCallsMeta(e.Meta); ok {
			seg.ToolCalls = calls
		}
	case types.RoleToolResult:
		seg.Kind = wire.SegToolResult
		seg.Speaker = wire.SpeakerTool
		seg.Content = e.Content
		id, _ := e.Meta["tool_call_id"].(string)
		seg.ToolCallID = id
	case types.RoleThinking:
		if e.Audience == types.AudienceAudit {
			return nil, nil // audit-only：不占上下文（ThinkingDisplay.ExposeToView=false 的落库形态）
		}
		seg.Kind, seg.Speaker, seg.Reasoning = wire.SegTurn, wire.SpeakerAssistant, e.Content
	default:
		// constraint / shared_memory / sub_task_result / escalation / human_note
		// 阶段 2 的最小循环里未启用这些角色；出现即折叠进 user（Part 3.4 表的方向）。
		seg.Kind, seg.Speaker, seg.Content = wire.SegTurn, wire.SpeakerHuman, e.Content
	}
	// 空段（编译器必须过滤）——仅在我们没有 ToolCalls/Reasoning/Content 可用时跳过。
	if seg.Content == "" && len(seg.ToolCalls) == 0 && seg.Reasoning == "" {
		return nil, nil
	}
	return &seg, nil
}

// metaKeys 返回 Meta 的键列表（诊断用）。
func metaKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// decodeToolCallsMeta 从 Meta 里还原工具调用（与 appendAssistantToolCalls 的编码对称）。
func decodeToolCallsMeta(meta map[string]any) ([]types.ToolCall, bool) {
	if meta == nil {
		return nil, false
	}
	raw, ok := meta["tool_calls"]
	if !ok {
		return nil, false
	}
	b, err := json.Marshal(raw) // map[string]string 的 list：再编码一次拿到稳定字节
	if err != nil {
		return nil, false
	}
	var calls []toolCallMeta
	if err := json.Unmarshal(b, &calls); err != nil || len(calls) == 0 {
		return nil, false
	}
	out := make([]types.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, types.ToolCall{ID: c.ID, Name: c.Name, Arguments: json.RawMessage(c.Arguments)})
	}
	return out, true
}
