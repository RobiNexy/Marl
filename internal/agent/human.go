package agent

// 人类 Actor 交互面与 Gate 的信封化往返（Part 14，阶段 12）。
//
// 统一模型（Part 14.7）：Gate 命中 need_human 后，审批请求是发给人类
// Actor 的 MsgGateRequest 信封（文件后端把它编码成 inbox/gate_<ulid>.md）；
// 人类的裁决是 MsgGateReply 信封回到本 Agent 的信箱，pump 送达等待面，
// 由 gate.Manager.ResolveGate 兑现 grant——审批的投递不再是本包的阻塞
// 轮询（旧 FileApprover 特例已消除）。
//
// 硬边界（Part 14.7）：审批者必须是规则，不是 Actor——本文件只搬运
// 裁决结果，规则表与 grant 记账全部在 gate.Manager。

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"marl/internal/actor"
	"marl/internal/gate"
	"marl/internal/proto"
	"marl/internal/skill"
	"marl/internal/store"
	"marl/internal/types"
)

// HumanLink 是 Agent 对"人类 Actor"的窄接口（消费侧定义，Part 14.2 的
// 交互面最小面）。
//
// 实现方：装配层（包着 Spawner 的 SendTo + 人类 Actor 的注册行——
// MarkPending 是 Watchdog 挂起判据的数据源，Part 14.8 "监控义务在
// 框架侧"）。测试：actortest.ScriptedHuman 的装配。
type HumanLink interface {
	// HumanID 返回人类 Actor 的 ID（"human:<uid>"；监督树的根）。
	HumanID() types.AgentID
	// SendToHuman 投递一条信封到人类 Actor 的收件箱。
	//
	// 失败语义：投递失败必须上抛（审批请求丢失 = Agent 永久挂起且
	// 无人知情——escalate 包头的同一纪律）。
	SendToHuman(ctx context.Context, env proto.Envelope) error
	// MarkPending / ClearPending 是挂起分支的登记面（Watchdog 扫描
	// "等待人类"的停摆时长的数据源；kind ∈ discussing / awaiting_gate
	// ——escalation 无超时，不登记，Part 14.8 违约表）。
	MarkPending(agent types.AgentID, kind string)
	ClearPending(agent types.AgentID)
}

// errGatePending 是 eventLoop 的内部哨兵（Gate 审批挂起——llm_call 与
// 编排共用；Part 14.7 的信封化往返）。
var errGatePending = errors.New("agent: awaiting gate approval")

// pendingGate 是等待人类审批的一次 Gate 操作（Agent 持有；awaitGate 消费）。
type pendingGate struct {
	req     *gate.Request
	attrs   map[string]any
	dec     gate.Decision      // need_human 决策（rule/reason 进审批文件）
	gateReq *proto.GateRequest // 发出的审批请求（requestID/nonce 对账键）
	reply   chan *proto.GateReply
}

// requestGateReview 把 need_human 决策折算成审批往返（llm_call 与编排
// 两个 PEP 共用的统一面；Part 14.7 的 MsgGateRequest 形态）。
//
// 返回 GATE_PENDING_HUMAN 的失败回填（模型知道"本次未执行、在等审批"）；
// 人类表面未装配 → GATE_DENIED 的显式拒绝（fail-closed："问不了人"不能
// 变成"不用问"）。
func (a *Agent) requestGateReview(ctx context.Context, req *gate.Request, attrs map[string]any, dec gate.Decision) *skill.SkillResult {
	if a.human == nil {
		a.auditf(ctx, "gate_no_human", string(req.Kind), map[string]any{
			"agent_id": string(a.id), "rule": dec.RuleID,
		})
		return skill.NewFailure(ErrCodeGateDenied,
			"命中规则 %s：%s\n人类 Actor 未装配（无审批面）——显式拒绝而非挂起。请降低请求规模或改用其它路径。",
			dec.RuleID, dec.Reason)
	}
	ulid, err := store.NewMessageID()
	if err != nil {
		return skill.NewFailure("GATE_INTERNAL", "审批凭据生成失败：%v（本次未执行）", err)
	}
	ulid = strings.ToLower(ulid)
	nonce, nerr := newGateNonce()
	if nerr != nil {
		return skill.NewFailure("GATE_INTERNAL", "审批凭据生成失败：%v（本次未执行）", nerr)
	}
	gateReq := &proto.GateRequest{
		RequestID:  ulid,
		Nonce:      nonce,
		Kind:       string(req.Kind),
		AgentID:    a.id,
		RuleID:     dec.RuleID,
		Reason:     dec.Reason,
		Attributes: attrs,
	}
	p := &pendingGate{
		req: req, attrs: attrs, dec: dec, gateReq: gateReq,
		reply: make(chan *proto.GateReply, 1), // 缓冲 1：pump 非阻塞送达
	}
	env := proto.Envelope{
		From:    a.id,
		To:      a.human.HumanID(),
		TraceID: types.TraceID("gate-" + ulid),
		Type:    proto.MsgGateRequest,
		Payload: gateReq,
	}
	// 先登记 pending 再投递（回复早到的竞态由顺序消掉——与 escalate
	// Manager.WaitBack 的"先注册再投递"同一纪律）；投递失败回滚登记。
	a.mu.Lock()
	a.gatePending = p
	a.mu.Unlock()
	if err := a.human.SendToHuman(ctx, env); err != nil {
		a.mu.Lock()
		a.gatePending = nil
		a.mu.Unlock()
		return skill.NewFailure("GATE_UNREACHABLE",
			"审批请求投递失败：%v（人类收件箱断链；本次未执行）", err)
	}
	a.auditf(ctx, "gate_pending", string(req.Kind), map[string]any{
		"agent_id": string(a.id), "rule": dec.RuleID, "reason": dec.Reason,
		"request_id": ulid, "to": string(a.human.HumanID()),
	})
	return skill.NewFailure(ErrCodeGatePendingHuman,
		"命中规则 %s：%s\n属性：%s\n审批请求已投递到人类收件箱（gate_%s.md），等待人类裁决（本次调用未执行）。",
		dec.RuleID, dec.Reason, actor.AttributesSummary(attrs), ulid)
}

// awaitGate 进入 Blocked(AwaitingGate) 等待人类裁决信封（Part 14.8 的
// 违约表：等待期零 LLM 调用；超时告警由 Watchdog 扫挂起登记——本函数
// 不做时间判断，"卡死"是确认过的回退策略）。
//
// 恢复路径：MsgGateReply（nonce/requestID 校验在 pump 侧——错配的回执
// 不会到这儿）→ ResolveGate 兑现 grant → 裁决摘要入 Log(RoleHumanNote)
// 让上下文里有对账键 → pending 清空。GATE_PENDING 的 tool_result 已经
// 回填给了模型——审批通过后下一轮由模型重发（session grant 让它直行）；
// deny 同理。框架不做"审批后自动重放"——调度权在 Agent 手里。
// ctx 取消保留 pending（讨论语义一致：谁也别丢）。
func (a *Agent) awaitGate(ctx context.Context) error {
	a.mu.Lock()
	p := a.gatePending
	a.mu.Unlock()
	if p == nil {
		return nil
	}
	a.state = types.StateBlocked
	a.blockReason = types.BlockAwaitingGate
	a.auditState(ctx, "blocked", string(types.BlockAwaitingGate))
	a.markPending(string(types.BlockAwaitingGate))
	var reply *proto.GateReply
	select {
	case <-ctx.Done():
		a.state = types.StateRunning
		a.blockReason = ""
		a.clearPending()
		a.auditState(ctx, "running", "")
		return ctx.Err() // pending 保留：重新 Run 继续等
	case reply = <-p.reply:
	}
	a.state = types.StateRunning
	a.blockReason = ""
	a.clearPending()
	a.auditState(ctx, "running", "")

	// 兑现（grant 记账 + 动态规则 + 审计；nil manager 在 need_human
	// 决策不可达——nilPDP 只产出 deny——这里是防御性跳过）。
	final := gate.Decision{Action: gate.Action(gateReplyActionOf(reply)), RuleID: p.dec.RuleID, Reason: reply.Reason}
	if mgr := a.gateManager(); mgr != nil {
		d, rerr := mgr.ResolveGate(ctx, p.req, reply, a.humanIDOf())
		if rerr != nil {
			return fmt.Errorf("agent: gate resolve: %w", rerr)
		}
		final = d
	}
	e := types.NewLogEntry(a.id, types.RoleHumanNote,
		fmt.Sprintf("Gate 裁决：rule=%s action=%s（%s）", final.RuleID, final.Action, final.Reason))
	e.Meta = map[string]any{"gate_kind": string(p.req.Kind), "rule": final.RuleID,
		"request_id": p.gateReq.RequestID}
	if _, err := a.log.Append(ctx, e); err != nil {
		return fmt.Errorf("agent: append gate verdict: %w", err)
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	a.auditf(ctx, "gate_resolved", string(p.req.Kind), map[string]any{
		"agent_id": string(a.id), "rule": final.RuleID, "action": string(final.Action),
		"granted_by": string(a.humanIDOf()), "request_id": p.gateReq.RequestID,
	})
	a.mu.Lock()
	a.gatePending = nil
	a.mu.Unlock()
	return nil
}

// gateManager 取 PDP（装配缺失 = nil——调用方按"无 Gate"处理）。
func (a *Agent) gateManager() *gate.Manager {
	if a.llmCallCfg != nil {
		return a.llmCallCfg.Gates
	}
	return nil
}

// humanIDOf 取人类 ActorID（HumanLink nil 时空串——审计里如实缺失）。
func (a *Agent) humanIDOf() types.AgentID {
	if a.human == nil {
		return ""
	}
	return a.human.HumanID()
}

// markPending / clearPending 是挂起登记的 nil 安全面（Watchdog 数据源）。
func (a *Agent) markPending(kind string) {
	if a.human != nil {
		a.human.MarkPending(a.id, kind)
	}
}

func (a *Agent) clearPending() {
	if a.human != nil {
		a.human.ClearPending(a.id)
	}
}

// gateReplyActionOf 把回执 action 折成 gate.Action（未知值 → deny——
// 协议面宽进严出：ResolveGate 仍会拒绝未知 action）。
func gateReplyActionOf(reply *proto.GateReply) string {
	if reply.Action == "allow" {
		return string(gate.ActionAllow)
	}
	return string(gate.ActionDeny)
}

// newGateNonce 生成当轮凭据（8 字节 hex；与讨论 verdict 同一防线）。
func newGateNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// deliverGateReply 是 pump 对 MsgGateReply 的处理：对账（requestID/nonce）
// 通过 → 非阻塞送达等待面。错配回执记审计并丢弃（陈旧回放的可见拒绝
// ——原则 4 的运行面）。
func (a *Agent) deliverGateReply(reply *proto.GateReply, from types.AgentID) {
	if reply == nil {
		return
	}
	a.mu.Lock()
	p := a.gatePending
	switch {
	case p == nil || p.gateReq == nil:
		p = nil // 无挂起：走 rejected 路径
	case reply.RequestID != p.gateReq.RequestID:
		p = nil
	case reply.Nonce != p.gateReq.Nonce:
		p = nil
	}
	var rejected string
	if p == nil {
		if a.gatePending != nil && a.gatePending.gateReq != nil {
			rejected = fmt.Sprintf("request/nonce mismatch (got %s)", reply.RequestID)
		} else {
			rejected = "no pending gate request"
		}
	} else {
		select {
		case p.reply <- reply:
		default:
			rejected = "reply channel full"
		}
	}
	a.mu.Unlock()
	if rejected != "" {
		a.auditf(context.Background(), "gate_reply_rejected", reply.RequestID, map[string]any{
			"reason": rejected, "from": string(from),
		})
		return
	}
	a.auditf(context.Background(), "gate_reply_received", reply.RequestID, map[string]any{
		"from": string(from), "action": reply.Action,
	})
}
