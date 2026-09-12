package agent

// agentViewOps：ViewOps 接口的 Agent 实现（阶段 11 补遗 §2-§5 的框架侧）。
//
// 流水线：定位（orchestrate.Select）→ 破坏量测算 → Gate（pin 免）→
// orchestrator Op 执行（真相之源 (Log, View) 的操作面）。
//
// 定位范围：当前 View 的**可见条目**（frozen 段不在 View 里——补遗 §2
// 的"模糊范围硬拒"由此天然成立）；分布式候选从 agent.log 取内容。
//
// 拆分/压缩/摘要（语义侧）不走本面：Agent 用 llm_call + 显式 View 操作
// 自己组合（补遗 §4"sidecar 返回不自动落 View"）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"marl/internal/gate"
	"marl/internal/orchestrate"
	"marl/internal/skill"
	"marl/internal/types"
)

// agentViewOps 实现 orchestrate.ViewOps（Agent 的 env.Orch 载体）。
type agentViewOps struct {
	a *Agent
}

var _ orchestrate.ViewOps = (*agentViewOps)(nil)

// visibleEntries 组装候选集（View 可见条目 + Log 内容）。
func (v *agentViewOps) visibleEntries(ctx context.Context) ([]orchestrate.ViewEntry, map[types.MessageID]int) {
	v.a.mu.Lock()
	items := make([]types.ViewItem, len(v.a.view.Items))
	copy(items, v.a.view.Items)
	v.a.mu.Unlock()
	out := make([]orchestrate.ViewEntry, 0, len(items))
	tokens := map[types.MessageID]int{}
	for _, it := range items {
		if !it.Visible {
			continue
		}
		e, err := v.a.log.Get(ctx, it.Ref)
		if err != nil {
			continue // 真相与投影失配：匹配器跳过（日志错误面留给执行）
		}
		t := estimateItemTokens(e)
		tokens[it.Ref] = t
		out = append(out, orchestrate.ViewEntry{Ref: it.Ref, Role: e.Role, Content: e.Content, TokenEst: t})
	}
	return out, tokens
}

// Execute 实现编排执行面（Part 补遗 §4/§5 的完整流水线）。
func (v *agentViewOps) Execute(ctx context.Context, kind orchestrate.OpKind, sel orchestrate.TargetSelector, extra map[string]any) map[string]any {
	entries, _ := v.visibleEntries(ctx)
	ref, _, err := orchestrate.Select(entries, sel)
	if err != nil {
		return failureOf(err) // AMBIGUOUS_TARGET（带摘录）/ TARGET_NOT_FOUND
	}
	// 破坏分级（pin/unpin 零破坏——补遗 §6 总表；exclude/restore/reorder/
	// annotate 按位置算）。
	var d cacheDestruction
	if kind != orchestrate.OpPin && kind != orchestrate.OpUnpin {
		d = v.computeDestructionFor(ctx, ref)
		// 非阻塞问询（Manager.Decide）：need_human → 挂起面；allow → 执行。
		dec := v.a.gates().Decide(ctx, &gate.Request{
			Kind:       gate.KindOrchestration,
			AgentID:    v.a.id,
			Attributes: viewDestructAttrs(d, d.TotalView),
		})
		switch dec.Action {
		case gate.ActionNeedHuman:
			// GATE_PENDING_HUMAN（需要数字的失败面）：挂起面在 agent 的
			// gatePending（eventLoop 尾部 errGatePending → awaitGate →
			// Evaluate（Approver 启动）→ 恢复）。
			v.a.auditf(ctx, "gate_request", string(kind), map[string]any{
				"agent_id": string(v.a.id), "pct": d.Percent, "tokens": d.DestTokens,
			})
			v.a.mu.Lock()
			v.a.gatePending = &pendingGate{req: &gate.Request{
				Kind:       gate.KindOrchestration,
				AgentID:    v.a.id,
				Attributes: viewDestructAttrs(d, d.TotalView),
			}}
			v.a.mu.Unlock()
			return map[string]any{
				"ok":                     false,
				"error":                  "GATE_PENDING_HUMAN",
				"cache_destroyed_pct":    d.Percent,
				"cache_destroyed_tokens": d.DestTokens,
				"message":                fmt.Sprintf("此操作将破坏 %s 的已缓存前缀，等待人类审批（本次未执行）。", fmtDestruction(d)),
			}
		case gate.ActionDeny:
			return map[string]any{"ok": false, "error": "GATE_DENIED",
				"message": fmt.Sprintf("命中规则 %s：%s（本次未执行）", dec.RuleID, dec.Reason)}
		}
		if dec.Action == gate.ActionAllow && dec.RuleID != "grant" {
			v.a.auditf(ctx, "orchestration_low_destruction", string(kind), map[string]any{
				"pct": d.Percent, "tokens": d.DestTokens, "ref": string(ref),
			})
		}
	}
	// apply（orchestrate 的 Op 实现是阶段 3 的核心——本方法只调用）。
	res, err := v.applyOp(ctx, kind, sel, extra, ref)
	if err != nil {
		return map[string]any{"ok": false, "error": "ORCH_FAILED", "message": err.Error()}
	}
	v.a.auditf(ctx, "orchestration", string(kind), map[string]any{
		"agent_id": string(v.a.id), "ref": string(ref), "destruction_pct": d.Percent,
	})
	return res
}

// computeDestructionFor 是破坏量的 PEP 面：目标位置之前的 **Position**
// 对齐（Phase 11.4 的"位置 P 及其后"——对 View 按当前序算，"后"是
// Position 序而不是切片序——推断说明在 computeCarefully 的注释）。
func (v *agentViewOps) computeDestructionFor(ctx context.Context, ref types.MessageID) cacheDestruction {
	v.a.mu.Lock()
	items := make([]types.ViewItem, len(v.a.view.Items))
	copy(items, v.a.view.Items)
	v.a.mu.Unlock()
	tokens := map[types.MessageID]int{}
	var target struct {
		pos float64
		ok  bool
	}
	for _, it := range items {
		if !it.Visible {
			continue
		}
		e, err := v.a.log.Get(ctx, it.Ref)
		if err != nil {
			continue
		}
		t := estimateItemTokens(e)
		tokens[it.Ref] = t
		if it.Ref == ref {
			target = struct {
				pos float64
				ok  bool
			}{it.Position, true}
		}
	}
	// 之后 = Position ≥ 目标（Fractional Index 序；编译序 = Position 序）。
	dest := 0
	total := 0
	hitting := false
	for _, it := range v.a.view.Items {
		if !it.Visible {
			continue
		}
		t := tokens[it.Ref]
		total += t
		if !hitting && target.ok && it.Position >= target.pos-1e-9 {
			hitting = true
		}
		if hitting {
			dest += t
		}
	}
	res := cacheDestruction{DestTokens: dest, TotalView: total}
	if total > 0 {
		res.Percent = float64(dest) / float64(total) * 100
	}
	return res
}

// applyOp 把 orchestrate 的 Op 构造 + Apply 封起来（Op 的 Apply 产出"新
// View"；Agent 保存——真相 (Log, View) 的操作收束于编排内核阶段 3）。
func (v *agentViewOps) applyOp(ctx context.Context, kind orchestrate.OpKind, sel orchestrate.TargetSelector, extra map[string]any, ref types.MessageID) (map[string]any, error) {
	v.a.mu.Lock()
	view := cloneViewForOp(v.a.view)
	v.a.mu.Unlock()
	var op orchestrate.Operation
	switch kind {
	case orchestrate.OpExclude:
		op = orchestrate.NewExcludeOp(orchestrate.ExcludeParams{Target: ref})
	case orchestrate.OpRestore:
		op = orchestrate.NewRestoreOp(orchestrate.RestoreParams{Target: ref})
	case orchestrate.OpPin:
		op = orchestrate.NewPinOp(orchestrate.PinParams{Target: ref})
	case orchestrate.OpUnpin:
		op = orchestrate.NewUnpinOp(orchestrate.PinParams{Target: ref})
	case orchestrate.OpReorder:
		pos, perr := allowPosition(ctx, v.a, view, ref)
		if perr != nil {
			return nil, perr
		}
		op = orchestrate.NewReorderOp(orchestrate.ReorderParams{Target: ref, Position: pos})
	case orchestrate.OpAnnotate:
		note, _ := extra["note"].(string)
		op = orchestrate.NewAnnotateOp(orchestrate.AnnotateParams{Target: ref, Note: note})
	case orchestrate.OpSplit:
		return nil, fmt.Errorf("split 属 llm_call 的编排用法（Part 11.2 §2.11），不走机械编排面")
	default:
		return nil, fmt.Errorf("orchestrate: kind %q 未实现", kind)
	}
	res, err := op.Apply(ctx, v.a.log, view)
	if err != nil {
		return nil, err
	}
	// View 写回（OpResult 的 View 是新的——orchestrate 契约的写回点）。
	v.a.mu.Lock()
	v.a.view = res.View
	v.a.mu.Unlock()
	out := map[string]any{"ok": true, "op": string(kind), "ref": string(ref)}
	if len(res.Appended) > 0 {
		out["appended"] = len(res.Appended)
	}
	return out, nil
}

// allowPosition 换算 reorder 的绝对位置（Machine 面的宽容形态：args 未给
// position 时默认"移到当前序的前一格"——位置表换算的锚定式显式写出的）。
func allowPosition(ctx context.Context, a *Agent, view *types.ContextView, ref types.MessageID) (float64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := 0.0
	found := false
	for _, it := range view.Items {
		if it.Ref == ref {
			found = true
			break
		}
		prev = it.Position
	}
	if !found {
		return 0, fmt.Errorf("reorder: target %s missing", ref)
	}
	return prev + 0.5, nil
}

// cloneViewForOp 是 orchestrate "不得就地修改"契约的调用侧实现
// （字段级拷贝；Items 切片是 immutable copy 的 requiem——orchestrate 内部
// 的 cloneView 同义，这里是防双写的外层）。
func cloneViewForOp(src *types.ContextView) *types.ContextView {
	out := *src
	out.Items = make([]types.ViewItem, len(src.Items))
	copy(out.Items, src.Items)
	return &out
}

// failureOf 把定位错误折成结果 map（错误码 + 摘录——补遗 §2 的三个面）。
func failureOf(err error) map[string]any {
	if errors.Is(err, orchestrate.ErrAmbiguous) {
		return map[string]any{"ok": false, "error": "AMBIGUOUS_TARGET", "message": err.Error()}
	}
	return map[string]any{"ok": false, "error": "TARGET_NOT_FOUND",
		"message": fmt.Sprintf("目标没找到（精确与归一化两轮都未命中；不做语义匹配——切错位置比不编排更糟）：%v", err)}
}

// isOrchSkill 报告 call 是否是编排五件套（executeToolCall 的分发面）。
func isOrchSkill(name string) bool {
	switch orchestrate.OpKind(name) {
	case orchestrate.OpExclude, orchestrate.OpRestore, orchestrate.OpReorder,
		orchestrate.OpAnnotate, orchestrate.OpPin, orchestrate.OpUnpin:
		return true
	}
	return false
}

// decodeOpSelector 是编排技能参数的统一入口（executeToolCall 的管道面）。
func decodeOpSelector(arguments json.RawMessage) orchestrate.TargetSelector {
	var raw map[string]any
	if len(arguments) > 0 {
		_ = json.Unmarshal(arguments, &raw)
	}
	sel := orchestrate.TargetSelector{}
	if v, ok := raw["target_text"].(string); ok {
		sel.TargetText = v
	}
	if v, ok := raw["relative"].(string); ok {
		sel.Relative = v
	}
	return sel
}

// opResultToSkillResult 这个名字的直译（ViewOps map → SkillResult）。
func opResultToSkill(m map[string]any) *skill.SkillResult {
	if okv, _ := m["ok"].(bool); okv {
		return skill.NewSuccess(m)
	}
	et := "ORCH_FAILED"
	if v, ok := m["error"].(string); ok && v != "" {
		et = v
	}
	return skill.NewFailure(et, "%v", m["message"])
}
