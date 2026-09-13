package agent

// Mailbox 与阻塞语义（Part 8.3 / 8.7 / 8.8，13.7 阶段 5）。
//
// 关键语义（Part 8.3）：fork 之后**没有父子通信**——父阻塞期间不发起任何
// LLM 调用、不向子发消息；子的任务在 fork 那一刻就是不可变的输入。父唯一
// 收到的消息是子的 report（含框架代报的 failed）。
//
// 并发模型（单写者纪律的延伸）：
//   - 主 goroutine：Run → eventLoop → awaitChildren（阻塞等信号）；
//   - pump goroutine：本文件所有者，消费 Mailbox，把 ChildReport 记入
//     reportBuffer 并在"全部子已归"时向 reportsDone 发信号；
//   - Log/View 的写入只发生在主 goroutine（awaitChildren 收尾时统一落
//     sub_task_result）——pump 只碰内存状态，不碰真相之源。
//
// goroutine 所有权与退出条件（2.7 纪律）：pump 由 Run 创建，ctx 取消或
// Mailbox 关闭即退出；除此之外没有别的退出路径。

import (
	"context"
	"errors"
	"fmt"

	"marl/internal/proto"
	"marl/internal/types"
)

// ReportSink 是子 Agent 投递 report 的窄接口（消费侧定义；实现是 Spawner）。
//
// 失败语义：投递失败（父信箱满/父已死）→ error——子 Agent 据此知道 report
// 没送到，此时子的任务**不能**当作完成（report 是它唯一的产出通道）。
type ReportSink interface {
	ReportToParent(ctx context.Context, report *proto.ChildReport) error
}

// childReportMsg 是 pump 从信封里解出的载荷形态。
type childReportMsg = proto.ChildReport

// startPump 启动 Mailbox 泵（Mailbox 为 nil 时是空操作——根 Agent 可以
// 没有消息面，阶段 5 的最小形态）。
//
// 返回一个 done 通道：Run 结束时关闭 Mailbox 的发送端是 Spawner 的职责，
// 这里只在 ctx 取消时退出。
func (a *Agent) startPump(ctx context.Context) {
	if a.mailbox == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env, ok := <-a.mailbox:
				if !ok {
					return // 发送端关闭：正常停机路径
				}
				a.handleEnvelope(env)
			}
		}
	}()
}

// handleEnvelope 处理一条信封（Part 8.8 handle + Part 14 的统一消息面）。
//
// 处理面：MsgChildReport（父的等待解除）、MsgEscalation（父侧登记+ACK）、
// MsgEscalationReply（发起者恢复）、MsgDirect（人类/Actor 直接消息的
// 异步注入——下一轮编排自然看到，Part 14.5）、MsgGateReply（Gate 审批
// 回执的对账与送达）。其余类型记审计并丢弃（Mailbox 的完整消息面随
// 对应阶段落地；静默丢弃必须留下审计痕迹，否则"发了但没人处理"
// 不可归因）。
func (a *Agent) handleEnvelope(env proto.Envelope) {
	switch env.Type {
	case proto.MsgChildReport:
		report, ok := env.Payload.(*proto.ChildReport)
		if !ok {
			// 断言失败按错误处理（Envelope 契约：禁止"断言失败就当 nil"）。
			a.auditf(context.Background(), "mailbox_error", env.Type.String(), map[string]any{
				"error": "payload type mismatch", "from": string(env.From),
			})
			return
		}
		a.recordChildReport(report)
	case proto.MsgEscalation:
		// 父路径（Part 11.3 流程 4）：登记一条 escalation 条目进 Log/View
		//（父的下一轮编排可见），并以框架 ACK 回复发起者——[阶段边界]
		// "父读取并答复"的完整形态由父的编排推进（escalate 包头的记录）。
		req, ok := env.Payload.(*proto.EscalationRequest)
		if !ok || req == nil {
			a.auditf(context.Background(), "mailbox_error", env.Type.String(), map[string]any{
				"error": "escalation payload mismatch", "from": string(env.From),
			})
			return
		}
		a.handleEscalationFromBelow(context.Background(), req)
	case proto.MsgEscalationReply:
		reply, ok := env.Payload.(*proto.EscalationReply)
		if !ok {
			a.auditf(context.Background(), "mailbox_error", env.Type.String(), map[string]any{
				"error": "escalation reply payload mismatch", "from": string(env.From),
			})
			return
		}
		if a.escCfg != nil && a.escCfg.Manager != nil {
			a.escCfg.Manager.DeliverReply(reply)
		}
	case proto.MsgDirect:
		// 人类/Actor 直接消息（marl say；Part 14.5）：异步注入——落成
		// RoleHumanNote 的 Log 条目，下一轮编排自然看到。不唤醒、不等待
		//（Part 14.8 违约表的"无等待"行）。
		msg, ok := env.Payload.(*proto.DirectMessage)
		if !ok || msg == nil {
			a.auditf(context.Background(), "mailbox_error", env.Type.String(), map[string]any{
				"error": "direct payload mismatch", "from": string(env.From),
			})
			return
		}
		a.handleDirect(context.Background(), env.From, msg)
	case proto.MsgGateReply:
		// Gate 审批回执（Part 14.7）：对账（requestID/nonce）通过则送达
		// 挂起的 awaitGate；错配回执显式拒绝（可见的陈旧回放防线）。
		reply, ok := env.Payload.(*proto.GateReply)
		if !ok || reply == nil {
			a.auditf(context.Background(), "mailbox_error", env.Type.String(), map[string]any{
				"error": "gate reply payload mismatch", "from": string(env.From),
			})
			return
		}
		a.deliverGateReply(reply, env.From)
	case proto.MsgTaskAssign:
		// 任务分配信封（Part 14.5；人类经 marl start / 未来 daemon）。
		// 当前装配的任务载体是 SpawnRequest.TaskDescription（Part 8.3 的
		// 不可变输入）；本处理面让协议表的类型可投递——落成 user 消息，
		// 下一轮编排自然看到。
		msg, ok := env.Payload.(*proto.DirectMessage)
		if !ok || msg == nil || msg.Text == "" {
			a.auditf(context.Background(), "mailbox_error", env.Type.String(), map[string]any{
				"error": "task_assign payload mismatch", "from": string(env.From),
			})
			return
		}
		a.handleDirect(context.Background(), env.From, msg)
	default:
		a.auditf(context.Background(), "mailbox_unhandled", env.Type.String(), map[string]any{
			"from": string(env.From),
		})
	}
}

// handleDirect 把一条直接消息落成 Log+View（RoleHumanNote）+ 审计。
// From 是框架填写的 ActorID（人类 "human:<uid>" 或上游 Agent）。
func (a *Agent) handleDirect(ctx context.Context, from types.AgentID, msg *proto.DirectMessage) {
	e := types.NewLogEntry(a.id, types.RoleHumanNote, msg.Text)
	e.Meta = map[string]any{"direct_from": string(from)}
	if _, err := a.log.Append(ctx, e); err != nil {
		a.auditf(ctx, "mailbox_error", "direct_log", map[string]any{"error": err.Error()})
		return
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	a.auditf(ctx, "human_direct", string(from), map[string]any{
		"agent_id": string(a.id), "chars": len([]rune(msg.Text)),
	})
}

// handleEscalationFromBelow 是父对 MsgEscalation 的框架级处理（Part 11.3
// 流程 4）：
//
//   - 登记一条 RoleEscalation 条目（父的下一轮编排与审计都看得到）；
//   - 框架自动 ACK（EscalationReply 的 IsHuman=false、From=本 Agent），
//     发起者的 waiting 因此解除而不至于永久阻塞——[阶段边界] 的完整讨论
//     见 internal/escalate 包头注。
func (a *Agent) handleEscalationFromBelow(ctx context.Context, req *proto.EscalationRequest) {
	a.mu.Lock()
	a.childEscalations = append(a.childEscalations, req)
	a.mu.Unlock()
	e := types.NewLogEntry(a.id, types.RoleEscalation,
		fmt.Sprintf("子 %s escalate: %s（question: %s）", req.From, req.Reason, req.Question))
	e.Meta = map[string]any{"escalation_id": string(req.TraceID)}
	if _, err := a.log.Append(ctx, e); err != nil {
		a.auditf(ctx, "mailbox_error", "escalation_log", map[string]any{"error": err.Error()})
		return
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	a.auditf(ctx, "escalation_ack", string(req.TraceID), map[string]any{
		"from": string(req.From),
	})
	// ACK（IsHuman=false；发起者的 awaitEscalation 由此解除）。SendTo 经
	// Spawner 的消息路由（架构上投递是框架的能力——Agent 不能直达别的
	// Agent 的信箱，原则 4 层面发信的通道只有 Spawner）。
	if a.spawner == nil {
		a.auditf(ctx, "mailbox_error", "escalation_ack", map[string]any{
			"error": "no spawner configured (ack undeliverable)",
		})
		return
	}
	env := proto.Envelope{
		From:    a.id,
		To:      req.From,
		Type:    proto.MsgEscalationReply,
		Payload: &proto.EscalationReply{From: a.id, IsHuman: false, Content: ACKText, TraceID: req.TraceID},
		TraceID: req.TraceID,
	}
	if err := a.spawner.SendTo(req.From, env); err != nil {
		a.auditf(ctx, "mailbox_error", "escalation_ack", map[string]any{
			"error": err.Error(),
		})
	}
}

// ACKText 是父对 escalate 的框架回复文本（审计/上下文里可读）。
const ACKText = "父 Agent 已收到你的求助；后续动作尚未执行。"

// recordChildReport 记录一份子 report（pump goroutine 调用；只碰内存）。
//
// 两个信号位（都非阻塞，重复触发无害）：
//   - reportsDone：全部 pending 子归位（WaitAll 的解除判据）；
//   - reportArrival：任一 report 到达（WaitAny/WaitN 的计数来源）。
//
// 判定依据的快照语义：信号只负责"醒来"，数量的裁决在 awaitChildren 收
// 醒来后用 childReports 计数重新确认（缓冲 1 的丢失在计数口径下无关）。
func (a *Agent) recordChildReport(report *proto.ChildReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.childReports = append(a.childReports, report)
	status := types.ChildDone
	if report.Status == proto.ReportFailed {
		status = types.ChildFailed
	}
	delete(a.pendingChildren, report.ChildID)
	a.childrenStatus[report.ChildID] = status
	select {
	case a.reportArrival <- struct{}{}:
	default:
	}
	if len(a.pendingChildren) == 0 {
		select {
		case a.reportsDone <- struct{}{}:
		default:
		}
	}
}

// hasPendingChildren 报告是否需要进入等待（主 goroutine 调用）。
//
// 两个条件都算"需要"：有未归的子（正常路径），或**报告已到但尚未落盘**
// （竞态路径——子可能在 spawn 注册与本轮检查之间就完成了，此时
// pendingChildren 已空而 childReports 有存货；不进入等待就会跳过
// flushChildReports，报告永远进不了 Log）。
func (a *Agent) hasPendingChildren() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pendingChildren) > 0 || len(a.childReports) > 0
}

// awaitChildren 进入 Blocked(WaitChildren) 等待全部子 report（Part 8.3），
// 恢复后把收集到的 report 落成 sub_task_result 条目（Log+View）。
//
// 期间不烧钱：不发起任何 LLM 调用（Part 8.3 的"父不推进轮次"）。
// ctx 取消 → 恢复 Running 并返回取消错误（调用方决定是否重启）。
func (a *Agent) awaitChildren(ctx context.Context) error {
	kind, n := a.takeWait()
	a.state = types.StateBlocked
	a.blockReason = types.BlockWaitChildren
	a.auditState(ctx, "blocked", string(types.BlockWaitChildren))
	err := a.waitReports(ctx, kind, n)
	a.state = types.StateRunning
	a.blockReason = ""
	a.auditState(ctx, "running", "")
	if err != nil {
		return err
	}
	return a.flushChildReports(ctx)
}

// waitReports 是恢复判据的骨架（策略语义文件见 wait.go）：
//
//	all —— 等 reportsDone（全部 pending 归位时 pump 发的信号）；
//	any —— 任一份 report 即恢复；
//	n   —— 到 n 份 report 或 pending 清空（"n 大于剩余子数"自然落为全收）。
//
// 计数口径 = childReports（缓冲未落库的 report 数）——数量判据读的是
// 已经确定到达的份数，不受信号丢失影响。
func (a *Agent) waitReports(ctx context.Context, kind WaitStrategy, n int) error {
	switch kind {
	case WaitAny:
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.reportArrival:
			return nil
		}
	case WaitN:
		if n < 1 {
			n = 1 // 非法阈值回落为"任一"（schema 已限 1..len，双保险）
		}
		for {
			a.mu.Lock()
			arrived := len(a.childReports)
			a.mu.Unlock()
			if arrived >= n {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-a.reportArrival:
			}
		}
	default:
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.reportsDone:
			return nil
		}
	}
}

// auditState 记一条状态迁移审计（marl status 从审计重建进程状态表的唯一
// 数据源——运行时内存态不做跨进程展示；audit 为 nil 时跳过）。
func (a *Agent) auditState(ctx context.Context, state, reason string) {
	a.auditf(ctx, "agent_state", state, map[string]any{
		"agent_id": string(a.id),
		"reason":   reason,
	})
}

// flushChildReports 把缓冲的子 report 逐条落成 sub_task_result（真相之源
// 只追加；血缘 = 无（report 是子的产出，不是父消息的派生——它的"出处"
// 记在 Meta.child_id）），并保留快照供 doCommit 的 message 构造。
//
// 单写者：只在主 goroutine（awaitChildren）调用。
func (a *Agent) flushChildReports(ctx context.Context) error {
	a.mu.Lock()
	reports := a.childReports
	a.childReports = nil
	a.lastReports = append(a.lastReports, reports...)
	a.mu.Unlock()
	for _, r := range reports {
		e := types.NewLogEntry(a.id, types.RoleSubTaskResult, r.Report)
		e.Meta = map[string]any{
			"child_id":      string(r.ChildID),
			"status":        string(r.Status),
			"files_changed": r.FilesChanged,
			"token_used":    r.TokenUsed,
		}
		if _, err := a.log.Append(ctx, e); err != nil {
			return fmt.Errorf("agent: append sub_task_result: %w", err)
		}
		a.addToView(RoleForLogRole(e.Role), e.ID)
	}
	return nil
}

// ChildrenStatus 返回运行时子状态快照（Part 8.1：瞬时状态查询，不遍历 Log）。
func (a *Agent) ChildrenStatus() map[types.AgentID]types.ChildStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[types.AgentID]types.ChildStatus, len(a.childrenStatus))
	for id, st := range a.childrenStatus {
		out[id] = st
	}
	return out
}

// errMailboxClosed 等投递失败的哨兵（ReportToParent 实现侧使用）。
var errMailboxClosed = errors.New("agent: mailbox closed")
