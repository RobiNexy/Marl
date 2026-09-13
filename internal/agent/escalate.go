package agent

// Escalation（向上求助，Part 11.3，13.12 阶段 10）。
//
// 阻塞循环与讨论同构（Part 8.2 的第四个 Blocked 子类型；Part 11.7 ③
// "阻塞是便宜的"同样适用——等待期无 LLM 调用）：
//
//	request_human（工具）→ EscalationClient.SendEscalation（路由 + 发送）
//	  → eventLoop 尾部 errEscalating → Run 外层 awaitEscalation
//	  → Blocked(Escalating)
//	  → 回复（父信封 / 人类信箱）→ pump 经 EscalationClient.DeliverReply
//	    → reply 落 Log(RoleEscalation)+View → 恢复 Running。
//
// 父路径的父侧行为（框架自动 ACK）：pump 收 MsgEscalation 时登记一条
// RoleEscalation 的 Log 条目（父的下一轮编排可见）并以 ACK 回复发起者
// ——[阶段边界] "父读取并深入答复"的完整形态由父的编排自然推进；本边界
// 记录在 internal/escalate 包头。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/skill"
	"github.com/RobiNexy/Marl/internal/types"
)

// EscalationConfig 是 Escalation 机制的装配参数（Config.Escalation 非 nil
// 即启用；缺失时 request_human 如实回填 INTENT_NOT_HANDLED）。
type EscalationConfig struct {
	// Manager 是发送/等待/分发的关口（internal/escalate.Manager；测试用
	// 假实现）。
	Manager EscalationClient
}

// EscalationClient 是 Agent 对 escalate.Manager 的窄接口（消费侧定义）。
type EscalationClient interface {
	SendEscalation(ctx context.Context, req *proto.EscalationRequest) (types.EscalationID, error)
	// WaitBack 等待一次回复（awaitEscalation 调用；Send 先于 Wait 才能吃
	// 掉"回复早到"的竞态——实现侧队列的注册顺序语义）。
	WaitBack(ctx context.Context, traceID types.TraceID) (*proto.EscalationReply, error)
	// DeliverReply 是 pump 的上行（子信箱信封的回程路径）。
	DeliverReply(reply *proto.EscalationReply)
}

// errEscalating 是 eventLoop 的内部哨兵。
var errEscalating = errors.New("agent: escalation in progress")

// escArgs 是 request_human 的参数形态（schema 的镜像）。
type escArgs struct {
	Question string `json:"question"`
	Context  string `json:"context"`
}

// pendingEscalation 是进行中的一次求助（Agent 持有；awaitEscalation 消费）。
type pendingEscalation struct {
	id    types.EscalationID
	trace types.TraceID
}

// intentRequestHuman 处理 request_human（Part 11.3 入口的工具面）。
//
// 失败口径：reason/question 为空 → BAD_ARGS（还没收敛成"一句问话"的求助
// 属于"再想想"，不升级——proto.EscalationRequest 的契约）；路由无出口 /
// 管理器装配缺失 → infrastructure**错误**还是失败回填？路由无出口是
// 配置错误（框架级），上抛；用户面参数问题走 BAD_ARGS。
func (a *Agent) intentRequestHuman(ctx context.Context, call types.ToolCall) (*skill.SkillResult, error) {
	if a.escCfg == nil || a.escCfg.Manager == nil {
		return skill.NewFailure(ErrIntentNotHandled, "request_human is not available: escalation is not configured"), nil
	}
	var args escArgs
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			a.roundFormatErrors++
			return skill.NewFailure("BAD_ARGS", "arguments not valid JSON: %v", err), nil
		}
	}
	if args.Question == "" {
		return skill.NewFailure("BAD_ARGS", "question 不能为空：还没收敛成一句问话，说明还没到该升级的时候"), nil
	}
	req := &proto.EscalationRequest{
		From:           a.id,
		Reason:         args.Context,
		ContextSummary: args.Context,
		Question:       args.Question,
		TraceID:        types.TraceID(fmt.Sprintf("esc-%s-%d", a.id, time.Now().UnixNano())),
	}
	id, err := a.escCfg.Manager.SendEscalation(ctx, req)
	if err != nil {
		// 无出口 / 配置缺失是框架面（retry 无意义）；参数级别失败早 reject。
		return nil, fmt.Errorf("intend request_human: %w", err)
	}
	a.mu.Lock()
	a.escPending = &pendingEscalation{id: id, trace: req.TraceID}
	a.mu.Unlock()
	a.auditf(ctx, "escalation", string(id), map[string]any{
		"agent_id": string(a.id), "question": args.Question,
	})
	// 主 Log 记"发起"条目（Part 11.3 流程 7 的三条里第一条）。
	e := types.NewLogEntry(a.id, types.RoleEscalation,
		fmt.Sprintf("Agent escalate: %s（question: %s）", args.Context, args.Question))
	e.Meta = map[string]any{"escalation_id": string(id)}
	if _, err := a.log.Append(ctx, e); err != nil {
		return nil, fmt.Errorf("append escalation entry: %w", err)
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	return skill.NewSuccess(map[string]any{
		"status":  "escalating",
		"id":      string(id),
		"message": "求助已发出（等上级 / 人类回复）。期间被暂停；回复到达时会注入你的上下文。",
	}), nil
}

// awaitEscalation 进入 Blocked(Escalating) 并等一次回复（复用讨论的阻塞
// 循环骨架）。回复 → Log(RoleEscalation)+View → 恢复。ctx 取消保留 pending
// （讨论语义一致：谁也别丢——重新 Run 继续等）。
func (a *Agent) awaitEscalation(ctx context.Context) error {
	a.mu.Lock()
	p := a.escPending
	a.mu.Unlock()
	if p == nil || a.escCfg == nil {
		return nil
	}
	a.state = types.StateBlocked
	a.blockReason = types.BlockEscalating
	a.auditState(ctx, "blocked", string(types.BlockEscalating))
	reply, err := a.escCfg.Manager.WaitBack(ctx, p.trace)
	a.state = types.StateRunning
	a.blockReason = ""
	a.auditState(ctx, "running", "")
	if err != nil {
		return err // ctx 取消（会话保留）
	}
	e := types.NewLogEntry(a.id, types.RoleEscalation,
		fmt.Sprintf("回复来自 %s: %s", replySourceOf(reply), reply.Content))
	e.Meta = map[string]any{
		"escalation_id": string(p.id),
		"is_human":      reply.IsHuman,
		"trace_id":      string(p.trace),
	}
	if _, err := a.log.Append(ctx, e); err != nil {
		return fmt.Errorf("agent: append escalation reply: %w", err)
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	a.mu.Lock()
	a.escPending = nil
	a.mu.Unlock()
	a.auditf(ctx, "escalation_replied", string(p.id), map[string]any{
		"is_human": reply.IsHuman, "from": string(reply.From),
	})
	return nil
}

// replySourceOf 显示面（审计可读）。
func replySourceOf(r *proto.EscalationReply) string {
	if r.IsHuman {
		return "human"
	}
	return string(r.From)
}
