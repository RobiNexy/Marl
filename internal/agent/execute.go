package agent

// 主循环的 turn 处理（Part 10.16 / 8.8 eventLoop 的阶段 2 形态）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"marl/internal/orchestrate"
	"marl/internal/proto"
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
// 阶段 4/5 的插入点（每轮边界上，顺序即语义）：
//  1. maybeCompress  —— headroom 不足先压缩（阶段 3）；
//  2. compile/execute/handle —— 本轮业务；
//  3. 记账 + 证据采集 + maybeUpgrade —— 每轮结束的固定动作（阶段 4）；
//  4. 子 report 已提交（child）→ 结束；有子未归（parent）→ errWaitChildren
//     （阶段 5，Run 外层等待后继续）。
//
// 失败：
//   - 轮数耗尽 → 错误（保险丝，不是业务结论）；
//   - LLM.ExecuteTurn 的 error（网络/装配）→ 原样上抛（ctx 取消保持原样）；
//   - 厂商错误（ErrorClass）→ 记证据后终结并带回分类（恢复策略按类别分流：
//     transient 属 Pool、overflow 属压缩、其余属升级/上报）。
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
		// Transient 的消费记账点（本轮编译已把既有 Transient 送达模型——
		// 轮末只清这些；handleTurn 期间新加的纠偏提示活到下一轮）。
		a.consumedTransients = len(a.transients)
		turn, err := a.llm.ExecuteTurn(ctx, req)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err // 取消是正常停机路径，原样返回（wire 契约）
			}
			return fmt.Errorf("agent: round %d: execute: %w", round, err)
		}
		a.beginRoundSignals()
		toolCount, err := a.handleTurn(ctx, turn)
		a.recordTurnLedger(ctx, turn)
		a.feedRound()
		if err != nil {
			return fmt.Errorf("agent: round %d: handle turn: %w", round, err)
		}
		a.clearTransients() // 本轮 Transient 已被模型消费（Part 3.6：用完即扔）
		// 升级检查在轮边界（Part 7.3：每轮结束后）。
		if err := a.maybeUpgrade(ctx); err != nil {
			return fmt.Errorf("agent: round %d: upgrade: %w", round, err)
		}
		// 子 Agent：report 已提交 → 任务完成（Run 收尾）。
		if a.reported {
			return nil
		}
		// 父 Agent：本轮 fork 了子且仍有子未归 → 阻塞等待（Run 外层处理）。
		if a.hasPendingChildren() {
			return errWaitChildren
		}
		// Escalation 进行中 → 阻塞等回复（Part 8.2 的 Blocked(Escalating)）。
		a.mu.Lock()
		esc := a.escPending != nil
		a.mu.Unlock()
		if esc {
			return errEscalating
		}
		// Gate 审批挂起（Part 11.3 的 Blocked(AwaitingGate)）。
		a.mu.Lock()
		gateWaiting := a.gatePending != nil
		a.mu.Unlock()
		if gateWaiting {
			return errGatePending
		}
		// 讨论进行中 → 阻塞等人类（Part 8.2 的 Blocked(Discussing)）。
		// 位置在工具执行与"turn 结束"判定之后：annotation 恢复后的响应轮
		// 可先跑完（含可能的草稿修订与子 report），随后再回到等待。
		if a.discussSess != nil {
			return errDiscussing
		}
		if toolCount == 0 && turn.Ready() {
			return nil
		}
		// 未 ready / 有工具调用 → 下一轮（maxRounds 兜底）。
	}
	return fmt.Errorf("agent: turn budget exhausted after %d rounds", a.maxRounds)
}

// maxFormatCorrections 是格式纠偏重试的 Run 内上限（防循环烧预算；
// ADR-0033 的字面值）。
const maxFormatCorrections = 3

// formatCorrectionText 是纠偏 Transient 的文案（Part 3.6 的 volatile 提示：
// 下一轮编译送达模型，模型据此拆小/修正后重发）。
func formatCorrectionText(class wire.ErrorClass) string {
	if class == wire.ErrOutputTruncated {
		return "上一次产出的 tool_call 参数 JSON 被输出预算截断（finish_reason=length）。把单次输出拆小后重发：大文件分多次 file_write/file_edit，每次不超过约 60 行。"
	}
	return "上一次产出的 tool_call 参数不是合法 JSON。修正参数格式（完整、可解析的 JSON）后重发本次调用。"
}

// errWaitChildren 是 eventLoop 的内部哨兵：本轮 fork 了子 Agent，主循环
// 需要进入 Blocked(WaitChildren) 等待全部子 report（Part 8.3）。
// 由 Run 捕获（errors.Is），不外泄给调用方。
var errWaitChildren = errors.New("agent: waiting for children reports")

// beginRoundSignals / clearRoundSignals 是每轮证据信号的复位点。
func (a *Agent) beginRoundSignals() {
	a.roundFormatErrors = 0
	a.roundProgress = false
	a.roundErrorClass = wire.ErrNone
}

// recordTurnLedger 把本轮 LLM 调用记入账本（阶段 4）。
//
// usage 口径：同一 turn 的多个 Outcome 共享同一个 *TokenUsage（Part 10.11
// 的记账纪律）——取首个非 nil，不跨 Outcome 累加。用量未知（厂商错误）不记
// （ledger 契约：未知 ≠ 0）。binding 未设置（阶段 2 的最小装配）跳过。
func (a *Agent) recordTurnLedger(ctx context.Context, turn *wire.WireTurn) {
	if a.ledger == nil || !a.bindingSet {
		return
	}
	var usage *types.TokenUsage
	for i := range turn.Outcomes {
		if turn.Outcomes[i].Usage != nil {
			usage = turn.Outcomes[i].Usage
			break
		}
	}
	if usage == nil {
		return
	}
	// 缓存统计累计（换模型审计的真实数据源）。
	a.cacheHits += int64(usage.CacheReadTokens)
	a.cacheMiss += int64(usage.PromptTokens - usage.CacheReadTokens)
	if err := a.ledger.RecordMain(ctx, a.taskID, a.id, a.binding, usage); err != nil {
		// 记账失败不回滚业务（账目缺失可见即可——打印到 stderr）。
		fmt.Printf("marl: ledger record failed (agent=%s): %v\n", a.id, err)
	}
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
			// 错误分类进本轮证据信号（升级判据的输入；分类→证据的映射在 feedRound）。
			a.roundErrorClass = o.Signals.ErrorClass
			// 格式纠偏重试（[阶段 12 修正/真机发现 #2]——denormalizer 注释
			// 预告的"可追加格式纠偏 Transient"落地）：malformed / truncated
			// 不再终结 Run，而是注入纠偏提示（下一轮编译送达模型）+ 消费一轮
			// 预算重试；上限 3 次防循环。升级证据照记（roundErrorClass 已置）。
			if o.Signals.ErrorClass == wire.ErrMalformed || o.Signals.ErrorClass == wire.ErrOutputTruncated {
				if a.formatCorrections < maxFormatCorrections {
					a.formatCorrections++
					a.transients = append(a.transients, formatCorrectionText(o.Signals.ErrorClass))
					a.auditf(ctx, "llm_output_corrected", string(o.Signals.ErrorClass), map[string]any{
						"agent_id": string(a.id), "attempt": a.formatCorrections,
					})
					return total, nil // 不终结：纠偏提示在下一轮编译时进入上下文
				}
				a.auditf(ctx, "llm_output_correction_exhausted", string(o.Signals.ErrorClass), map[string]any{
					"agent_id": string(a.id), "attempts": a.formatCorrections,
				})
			}
			// [阶段边界] 不做重试/排队：终结并带回类别。
			// 同 turn 先于失败段的成功产出不消失——它们已落 Log。
			return total, fmt.Errorf("llm outcome error class=%s (vendor/protocol failure)", o.Signals.ErrorClass)
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
			// 有可见回复是进展信号（升级证据的"无进展"反例）。
			a.roundProgress = true
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
//	意图工具（proto.IsIntentTool）→ executeIntent（改系统结构，走裁决关口）
//	技能：Authorize（白名单；错误码**如实回填**给模型——Part 9.4）
//	  → Registry.Get → 参数解析（json.Unmarshal）→ Skill.Execute
//	  （业务失败 OK=false 是正常返回；基础设施故障 error != nil 上抛）
//	  → 结果序列化 → Log（Role=tool_result）+ View。
//
// 并发：本方法在 eventLoop 的单 goroutine 里顺序调用（10.16 约束 2）；
// ctx 取消传播到技能的 IO。
func (a *Agent) executeToolCall(ctx context.Context, call types.ToolCall) error {
	// 0. 意图工具：改系统结构的能力走各自的裁决关口（Part 4.1 的边界）。
	//    意图不做白名单校验（它的权限模型是 Profile.CanSpawn / 深度，
	//    由关口裁决），也不查技能注册表。
	if proto.IsIntentTool(call.Name) {
		res, err := a.executeIntent(ctx, call)
		if err != nil {
			return fmt.Errorf("intent %s infra failure: %w", call.Name, err)
		}
		return a.appendToolEntry(ctx, call, res)
	}
	// 1. 授权（先确认能不能打，再找工具——错误面统一是结构化错误码）。
	if err := a.authorizer.Authorize(ctx, a.id, call.Name); err != nil {
		return a.appendToolEntry(ctx, call, skill.ResultFromAuthorizeError(err))
	}
	// 1.5 编排五件套走 ViewOps 面（agentViewOps.Execute 的结果已是
	// map 形态：serde 成 SkillResult 的统一错误面协议；GATE_PENDING_HUMAN
	// 已在 ViewOps 里挂到 gatePending → eventLoop 尾部 errGatePending）。
	if isOrchSkill(call.Name) {
		out := a.env.Orch.Execute(ctx, orchestrate.OpKind(call.Name), decodeOpSelector(call.Arguments), nil)
		res := opResultToSkill(out)
		return a.appendToolEntry(ctx, call, res)
	}
	// 2. 找技能（未注册也是"模型可见"的失败——技能名是模型拼写的，会拼错）。
	s, err := a.skills.Get(call.Name)
	if err != nil {
		return a.appendToolEntry(ctx, call, skill.NewFailure("UNKNOWN_SKILL", "%v", err))
	}
	// 2.5 自动转讨论（Part 11.2 入口 3）：写路径命中 auto_discuss 清单的
	// file_write / file_edit 被折为一次讨论（先审后写——Part 12.2 的写
	// 权限表" preferences/ 只有人类能写"的自动形态）。折算对 LLM 的呈现
	// 是一次"先把方案提交讨论"的失败回填 + 讨论 session 由 intent
	// request_discussion 承接（模型下一轮会看到讨论提示 & -> 调用）。
	if call.Name == "file_write" || call.Name == "file_edit" {
		var probe map[string]any
		if len(call.Arguments) > 0 {
			_ = json.Unmarshal(call.Arguments, &probe)
		}
		if p, _ := probe["path"].(string); p != "" && a.needsAutoDiscuss(p) && a.discussSess == nil {
			res := a.autoDiscussTurn(ctx, p)
			return a.appendToolEntry(ctx, call, res)
		}
	}
	// 3. 参数（Denormalizer 已校验合法性；此处解析成 map，不解释语义）。
	var args map[string]any
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			// MalformedOutput（10.11 表：计入升级证据）。作为业务失败
			// 回填（模型下一轮换写法）；证据计数在 appendToolEntry 里统一做。
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
	// 证据信号：格式错误与进展（升级判据的原始信号，Part 7.2）。
	switch {
	case res.ErrorType == "BAD_ARGS":
		a.roundFormatErrors++
	case res.OK:
		a.roundProgress = true // 成功的技能/意图调用是进展
	}
	// 写文件追踪（report 机械检查"声称改了文件但无改动"的数据源）。
	if call.Name == "file_write" && res.OK {
		if p, ok := res.Data["path"].(string); ok && p != "" {
			a.mu.Lock()
			a.writtenFiles[p] = true
			a.mu.Unlock()
		}
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
