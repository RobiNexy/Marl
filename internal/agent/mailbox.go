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

// handleEnvelope 处理一条信封（Part 8.8 handle 的阶段 5 形态）。
//
// 阶段 5 只处理 MsgChildReport；其余类型记审计并丢弃（Mailbox 的完整
// 消息面——人类输入/升级/重配置——随对应阶段落地；静默丢弃必须留下
// 审计痕迹，否则"发了但没人处理"不可归因）。
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
	default:
		a.auditf(context.Background(), "mailbox_unhandled", env.Type.String(), map[string]any{
			"from": string(env.From),
		})
	}
}

// recordChildReport 记录一份子 report（pump goroutine 调用；只碰内存）。
//
// 全部 pending 子归位时向 reportsDone 发信号（非阻塞：信号位被重复触发
// 是无害的——awaitChildren 只等一次）。
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
	a.state = types.StateBlocked
	a.blockReason = types.BlockWaitChildren
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-a.reportsDone:
	}
	a.state = types.StateRunning
	a.blockReason = ""
	if err != nil {
		return err
	}
	return a.flushChildReports(ctx)
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
