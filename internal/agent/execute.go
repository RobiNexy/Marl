package agent

// 主循环的 turn 处理（Part 10.16 / 8.8 eventLoop 的阶段 2 形态）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"marl/internal/skill"
	"marl/internal/types"
	"marl/internal/wire"
)

// eventLoop 反复"编译上下文 → Execute → 消化 turn"直到任务自然完结。
//
// 终结判据（10.16 伪代码 + wire.WireTurn.Ready 的边界语义）：
//   - 本轮没有工具调用，且 turn.Ready()（有可见回复）→ 结束；
//   - 工具调用 → 顺序执行、结果进 Log+View，继续下一轮；
//   - thinking-only（不 ready）→ 继续下一轮（轮数上限兜底，防止空转烧预算）。
//
// 失败：
//   - 轮数耗尽 → 错误（保险丝，不是业务结论）；
//   - LLM.ExecuteTurn 的 error（网络/装配）→ 原样上抛（ctx 取消保持原样）；
//   - 厂商错误（ErrorClass）→ 阶段 2 不做压缩/升级/重试，直接终结并把
//     分类带回（恢复策略全是 13.5+ 的范围）。
func (a *Agent) eventLoop(ctx context.Context) error {
	for round := 0; round < a.maxRounds; round++ {
		// 压缩检查在编译之前：headroom 不足先压缩再编译，避免把一个
		// 必然溢出的请求发给厂商（Part 3.7 的触发点）。
		if err := a.maybeCompress(ctx, round); err != nil {
			return fmt.Errorf("agent: round %d: compress: %w", round, err)
		}
		req, err := a.compileView(ctx)
		if err != nil {
			return fmt.Errorf("agent: round %d: compile view: %w", round, err)
		}
		turn, err := a.llm.ExecuteTurn(ctx, req)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err // 取消是正常停机路径，原样返回（wire 契约）
			}
			return fmt.Errorf("agent: round %d: execute: %w", round, err)
		}
		toolCount, err := a.handleTurn(ctx, turn)
		if err != nil {
			return fmt.Errorf("agent: round %d: handle turn: %w", round, err)
		}
		if toolCount == 0 && turn.Ready() {
			return nil
		}
		// 未 ready / 有工具调用 → 下一轮（maxRounds 兜底）。
	}
	return fmt.Errorf("agent: turn budget exhausted after %d rounds", a.maxRounds)
}

// handleTurn 逐 Outcome 落 Log 与 View（Part 10.16 主循环伪代码的落地），
// 返回本 turn 的工具调用总数（0 = 无可执行调用）。
//
// 已发生的 Outcome **全部**进 Log（10.16 约束 3：中断恢复的基础——失败的
// 记录同样写真相）；工具调用顺序执行（约束 2：不并行——后续调用常依赖
// 前序结果）；结果以后续 round 的整体编译回到请求（AppendToolResults 的
// 阶段 2 形态：View 是唯一载体）。
func (a *Agent) handleTurn(ctx context.Context, turn *wire.WireTurn) (int, error) {
	total := 0
	for i := range turn.Outcomes {
		o := &turn.Outcomes[i]

		// 失败产出：三项皆空、信息在 Signals.ErrorClass（ADR-0019 的例外形态）。
		if o.Signals.ErrorClass.IsError() {
			// [阶段 2 边界] 不做重试/压缩/升级：终结并带回类别。
			// 同 turn 先于失败段的成功产出不消失——它们已落 Log。
			return total, fmt.Errorf("llm outcome error class=%s (vendor/protocol failure; recovery starts at phase 3)",
				o.Signals.ErrorClass)
		}
		// 三个分支是顺序 if 而非 switch：混合流形态（10.16 表）允许一个
		// Outcome 同时携带 reasoning 与 tool_call——switch 会把后面那类
		// 吞掉，if 各自独立判定才能保证"已发生的产出全部落 Log"。
		if o.Reasoning != nil {
			// 思维链：Entry 由 Denormalizer 产出（Role=thinking，Audience=audit），
			// 主循环回填归属后落库。是否进 View 由 Entry.Audience 决定
			// （阶段 2 默认 audit-only，不占上下文 token——Part 3.2）。
			e := o.Entry
			e.AgentID = a.id
			if _, err := a.log.Append(ctx, &e); err != nil {
				return total, fmt.Errorf("append reasoning: %w", err)
			}
			if e.Audience != types.AudienceAudit {
				a.addToView(RoleForLogRole(e.Role), e.ID)
			}
		}
		if len(o.ToolCalls) > 0 {
			// 助手的调用意图落 Log：Role=assistant_reply、Content=""、
			// Meta["tool_calls"]=结构化调用（审计与下一轮编译都靠它——
			// 编译层 decodeToolCallsMeta 依赖这个键）。
			if err := a.appendAssistantToolCalls(ctx, o.ToolCalls); err != nil {
				return total, fmt.Errorf("append tool-call entry: %w", err)
			}
			for _, call := range o.ToolCalls {
				if err := a.executeToolCall(ctx, call); err != nil {
					return total, err // 基础设施故障上抛；业务失败已在结果里
				}
				total++
			}
		}
		if o.Reply != "" {
			e := o.Entry // replyLogEntry：Role=assistant_reply，Meta 带 finish_reason
			e.AgentID = a.id
			if _, err := a.log.Append(ctx, &e); err != nil {
				return total, fmt.Errorf("append reply: %w", err)
			}
			a.addToView(RoleForLogRole(e.Role), e.ID)
		}
		// 三段皆空且无 ErrorClass 的 Outcome 被 Denormalizer 挡掉（Outcome
		// 不变量），这里不做第二套防御（Part 10.11 的纪律）。
	}
	return total, nil
}

// appendAssistantToolCalls 把 assistant 的工具调用意图落 Log+View。
func (a *Agent) appendAssistantToolCalls(ctx context.Context, calls []types.ToolCall) error {
	metas := make([]toolCallMeta, 0, len(calls))
	for _, c := range calls {
		metas = append(metas, toolCallMeta{ID: c.ID, Name: c.Name, Arguments: string(c.Arguments)})
	}
	e := types.NewLogEntry(a.id, types.RoleAssistantReply, "")
	e.Meta = map[string]any{"tool_calls": metas}
	if _, err := a.log.Append(ctx, e); err != nil {
		return err
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	return nil
}

// executeToolCall 一次调用的完整通路（Part 4.3 / 5.4 / 9.4）：
//
//	Authorize（白名单；错误码**如实回填**给模型——Part 9.4）
//	  → Registry.Get → 参数解析（json.Unmarshal）→ Skill.Execute
//	  （业务失败 OK=false 是正常返回；基础设施故障 error != nil 上抛）
//	  → 结果序列化 → Log（Role=tool_result）+ View。
//
// 并发：本方法在 eventLoop 的单 goroutine 里顺序调用（10.16 约束 2）；
// ctx 取消传播到技能的 IO。
func (a *Agent) executeToolCall(ctx context.Context, call types.ToolCall) error {
	// 1. 授权（先确认能不能打，再找工具——错误面统一是结构化错误码）。
	if err := a.authorizer.Authorize(ctx, a.id, call.Name); err != nil {
		return a.appendToolEntry(ctx, call, skill.ResultFromAuthorizeError(err))
	}
	// 2. 找技能（未注册也是"模型可见"的失败——技能名是模型拼写的，会拼错）。
	s, err := a.skills.Get(call.Name)
	if err != nil {
		return a.appendToolEntry(ctx, call, skill.NewFailure("UNKNOWN_SKILL", "%v", err))
	}
	// 3. 参数（Denormalizer 已校验合法性；此处解析成 map，不解释语义）。
	var args map[string]any
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			// MalformedOutput（10.11 表：计入升级证据）。阶段 2 作为业务失败
			// 回填（模型下一轮换写法）；升级证据的累计在阶段 4。
			return a.appendToolEntry(ctx, call, skill.NewFailure("BAD_ARGS", "arguments not valid JSON: %v", err))
		}
	}
	// 4. 执行。
	res, err := s.Execute(ctx, args, a.env)
	if err != nil {
		return fmt.Errorf("tool %s infra failure: %w", call.Name, err)
	}
	// 5. 结果落盘（成功与业务失败都进——模型的纠错依据）。
	return a.appendToolEntry(ctx, call, res)
}

// appendToolEntry 把 SkillResult 落 Log+View。
//
// Content = 序列化后的 SkillResult JSON（ToolResult.Content 契约：技能返回
// map 后序列化而来）；Meta 携带配对 id 与机械判断字段（ok / error_type），
// 让下一轮编译之外审计也能直接读。
func (a *Agent) appendToolEntry(ctx context.Context, call types.ToolCall, res *skill.SkillResult) error {
	if err := res.Validate(); err != nil {
		// 技能产出破坏了自己的契约（调用方 bug 级别）——不能静默过：
		// 把"结果不可信"本身作为错误面回填是最诚实的表达。
		return fmt.Errorf("tool %s: invalid result: %w", call.Name, err)
	}
	content, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("tool %s: marshal result: %w", call.Name, err)
	}
	e := types.NewLogEntry(a.id, types.RoleToolResult, string(content))
	e.Meta = map[string]any{
		"tool_call_id": call.ID,
		"name":         call.Name,
		"ok":           res.OK,
	}
	if res.ErrorType != "" {
		e.Meta["error_type"] = res.ErrorType
	}
	if _, err := a.log.Append(ctx, e); err != nil {
		return fmt.Errorf("append tool result: %w", err)
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	return nil
}
