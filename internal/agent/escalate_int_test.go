package agent

// Escalation 在 Agent 侧的闭环测试（Part 11.3 / 13.12 阶段 10）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"marl/internal/actor"
	"marl/internal/proto"
	"marl/internal/spawner"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
)

func humanEscCall() types.ToolCall {
	return types.ToolCall{
		ID: "call-esc", Name: "request_human",
		Arguments: []byte(`{"question":"session 存储 Redis 还是内存？","context":"两个选项都试过"}`),
	}
}

// escHook 是 EscalationClient 的替身（Send 记录 + autoReply 回路 +
// DeliverReply 汇合）。
type escHook struct {
	sent         []*proto.EscalationRequest
	autoReply    func(*proto.EscalationRequest)
	replies      map[types.TraceID]*proto.EscalationReply
	deliveredCnt int
}

func (f *escHook) SendEscalation(ctx context.Context, req *proto.EscalationRequest) (types.EscalationID, error) {
	f.sent = append(f.sent, req)
	if f.autoReply != nil {
		f.autoReply(req)
	}
	return types.EscalationID("esc_" + req.TraceID), nil
}

func (f *escHook) WaitBack(ctx context.Context, traceID types.TraceID) (*proto.EscalationReply, error) {
	if r, ok := f.replies[traceID]; ok {
		return r, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *escHook) DeliverReply(reply *proto.EscalationReply) {
	f.deliveredCnt++
	if reply != nil {
		f.replies[reply.TraceID] = reply
	}
}

// TestEscalationFullLoopHuman：request_human → errEscalating →
// Blocked(Escalating) 等待 → 人类回复注入 Log(RoleEscalation)+View →
// 任务完成（Part 11.3 流程 7 的"主 Log 两条边"形态）。
func TestEscalationFullLoopHuman(t *testing.T) {
	esc := &escHook{replies: map[types.TraceID]*proto.EscalationReply{}}
	// 回复在 Send 时就地挂上（真实 trace 在发送时生成——hook 以 req.TraceID 对账）。
	esc.autoReply = func(req *proto.EscalationRequest) {
		esc.replies[req.TraceID] = &proto.EscalationReply{
			From: "human-1", IsHuman: true,
			Content: "用 Redis，配置在 preferences/infrastructure.md",
			TraceID: req.TraceID,
		}
	}
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(humanEscCall()),
		replyTurn("收到人类的回复：用 Redis。任务完成。"),
	}}
	a, _ := newTestAgent(t, llm)
	a.escCfg = &EscalationConfig{Manager: esc}
	ctx := context.Background()
	if err := a.AppendUser(ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(esc.sent) != 1 {
		t.Fatalf("escalations sent = %d", len(esc.sent))
	}
	entries := entriesMust(t, a)
	sawAsk, sawReply := false, false
	for _, e := range entries {
		if e.Role == types.RoleEscalation && strings.Contains(e.Content, "Agent escalate") {
			sawAsk = true
		}
		if e.Role == types.RoleEscalation && strings.Contains(e.Content, "回复来自 human: 用 Redis") {
			sawReply = true
		}
	}
	if !sawAsk || !sawReply {
		t.Fatalf("escalation log entries missing (ask=%v reply=%v)", sawAsk, sawReply)
	}
}

// TestEscalationParentACKByPump：MsgEscalation 信封进 pump →
// handleEscalationFromBelow 登记 entry + ACK 经真实 spawner.SendTo 回程
// → 读端拿到 ACK 信封（DoS 询问的框架 ACK 通道）。
func TestEscalationParentACKByPump(t *testing.T) {
	st, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	root := workspaceRoot(t)
	chk, err := spawner.NewReportChecker(spawner.ReportCheckerConfig{Root: root, MaxScanFiles: 50})
	if err != nil {
		t.Fatal(err)
	}
	_ = chk
	sp, err := spawner.New(spawner.Config{
		MaxDepth: 1, MaxActive: 8, MaxForkRounds: 3,
		CanSpawnAtDepth: func(int) bool { return true },
		Log:             store.MessageLog(st),
	})
	if err != nil {
		t.Fatal(err)
	}

	parentNS := &types.Namespace{AgentID: "parent-1", Mounts: []types.Mount{
		{Pattern: "**", Mode: types.PathWrite},
	}}
	parentBackend := actor.NewChannelBackend(32)
	if err := sp.RegisterAgent(context.Background(), "parent-1", 1, parentNS,
		actor.AgentCaps(true), parentBackend); err != nil {
		t.Fatal(err)
	}
	// 发起者（子）也在进程表里（真实拓扑的 ACK 路由目标；AI 第 2 层）。
	subNS := &types.Namespace{AgentID: "sub-1", Mounts: []types.Mount{
		{Pattern: "**", Mode: types.PathWrite},
	}}
	subBackend := actor.NewChannelBackend(32)
	if err := sp.RegisterAgent(context.Background(), "sub-1", 2, subNS,
		actor.AgentCaps(false), subBackend); err != nil {
		t.Fatal(err)
	}
	mb, mbSub := parentBackend.Receive(), subBackend.Receive()

	a, _ := newTestAgent(t, &fakeLLM{})
	a.spawner = sp
	a.escCfg = &EscalationConfig{Manager: &escHook{replies: map[types.TraceID]*proto.EscalationReply{}}}

	req := &proto.EscalationRequest{From: "sub-1", Question: "q1", TraceID: "t-1"}
	env := proto.Envelope{
		From: "sub-1", To: "parent-1", Type: proto.MsgEscalation,
		Payload: req, TraceID: req.TraceID,
	}
	// pump 处理器直接驱动（handleEnvelope 是 pump 的单入口，串行语义）。
	a.handleEnvelope(env)
	if len(a.childEscalations) != 1 {
		t.Fatalf("child escalation not recorded: %d", len(a.childEscalations))
	}
	// ACK 会进发起者的读端（SendTo → child 的 Mailbox 读端）。
	select {
	case ackEnv := <-mbSub:
		_, ok := ackEnv.Payload.(*proto.EscalationReply)
		if !ok {
			t.Fatalf("ack payload: %T", ackEnv.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ack envelope missing in sub mailbox")
	}
	_ = mb
}
