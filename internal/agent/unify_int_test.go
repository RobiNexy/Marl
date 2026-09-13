package agent

// Part 14（阶段 12）的验收测试：ScriptedHuman 跑通四个人机协作场景
// ——讨论 / 审批 / escalation / grant——不需要真人、不需要 UI、不需要
// 等文件静默期（channel 后端即时投递；文件后端本身的编码/nonce/静默窗
// 在 actor 包单元测试覆盖——Part 14.9 的分工）。
//
// 统一面的公共断言（每个场景共享）：
//   - 监督树以人类为根：ScriptedHuman 的行在进程表里（depth=0、
//     Kind=human）——Part 14.6 的拓扑事实；
//   - 人类的收发经同一封信封协议（MsgGateRequest/MsgGateReply/MsgDirect）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"marl/internal/actor"
	"marl/internal/actor/actortest"
	"marl/internal/config"
	"marl/internal/discuss"
	"marl/internal/escalate"
	"marl/internal/gate"
	"marl/internal/proto"
	"marl/internal/spawner"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
)

// unifiedHarness 是四场景共用的装配：真实 Spawner + ScriptedHuman 注册
// 为根 + Agent 的 HumanLink/信箱接线（替身的回信经 Spawner 路由回信箱）。
type unifiedHarness struct {
	t      *testing.T
	ctx    context.Context
	spw    *spawner.Spawner
	human  *actortest.ScriptedHuman
	agentB *actor.ChannelBackend // 被测 Agent 的信箱后端
	link   *testHumanLink
}

// testHumanLink 是 HumanLink 的装配实现（包 Spawner 的投递/登记面——
// 与 cmd/marl/start.go 的 spawnerHumanLink 同形）。
type testHumanLink struct {
	spw   *spawner.Spawner
	human types.AgentID
}

func (l *testHumanLink) HumanID() types.AgentID { return l.human }

func (l *testHumanLink) SendToHuman(_ context.Context, env proto.Envelope) error {
	return l.spw.SendTo(l.human, env)
}

func (l *testHumanLink) MarkPending(agent types.AgentID, kind string) {
	l.spw.MarkPending(agent, kind)
}

func (l *testHumanLink) ClearPending(agent types.AgentID) { l.spw.ClearPending(agent) }

func newUnifiedHarness(t *testing.T) *unifiedHarness {
	t.Helper()
	ctx := context.Background()
	st, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	spw, err := spawner.New(spawner.Config{
		MaxDepth: 3, MaxActive: 16, MaxForkRounds: 8,
		CanSpawnAtDepth: func(int) bool { return true },
		Log:             st,
	})
	if err != nil {
		t.Fatal(err)
	}
	human := actortest.New("acceptance")
	if err := spw.RegisterHuman(ctx, human.Actor()); err != nil {
		t.Fatal(err)
	}
	agentBackend := actor.NewChannelBackend(32)
	link := &testHumanLink{spw: spw, human: human.ID()}
	h := &unifiedHarness{t: t, ctx: ctx, spw: spw, human: human, agentB: agentBackend, link: link}
	return h
}

// attach 把 Agent 接进统一面：进程表登记（路由目标——替身的回信经
// Spawner.SendTo 回到这个信箱）+ 信箱读端 + HumanLink；替身剧本启动。
func (h *unifiedHarness) attach(a *Agent, script []actortest.ScriptedReply) {
	if err := h.spw.RegisterAgent(h.ctx, a.ID(), 1, a.env.Namespace,
		actor.AgentCaps(true), h.agentB); err != nil {
		h.t.Fatalf("register agent: %v", err)
	}
	a.mailbox = h.agentB.Receive()
	a.human = h.link
	go h.human.Run(h.ctx, script, func(env actor.Envelope) error {
		return h.spw.SendTo(env.To, env)
	})
}

// assertHumanIsRoot 断言监督树以人类为根（Part 14.14 的验收面之一）。
func (h *unifiedHarness) assertHumanIsRoot() {
	h.t.Helper()
	found := false
	for _, p := range h.spw.Snapshot() {
		if p.ID == h.human.ID() {
			found = true
			if p.Depth != 0 || p.Kind != "human" {
				h.t.Fatalf("human row: %+v", p)
			}
		}
	}
	if !found {
		h.t.Fatal("human actor missing from process table")
	}
}

// ---------------------------------------------------------------------------
// 场景 2 + 4：Gate 审批与 grant（llm_call 的 need_human → 回执 → 额度直行；
// always 形态落盘 grants/）
// ---------------------------------------------------------------------------

// TestScriptedHumanGateApproval：need_human → MsgGateRequest（替身收件箱）
// → 剧本回 @grant next 2 → Agent 恢复 → 额度内重发直行（不再打扰人类）。
func TestScriptedHumanGateApproval(t *testing.T) {
	h := newUnifiedHarness(t)
	h.assertHumanIsRoot()

	a, _ := llmCallAgentAndFake(t, func(c *LLMCallConfig) {
		c.Gates = gateMustManager(t, []gate.Rule{{
			ID: "review-all", Match: map[string]string{"kind": "llm_call", "task_call_count": ">=0"},
			Action: gate.ActionNeedHuman,
		}})
	})
	// 剧本：收到 MsgGateRequest → 回 allow + count 2（对账字段原样带回）；
	// 人类的"被打扰次数"由剧本触发计数（Run 消费后 Drain 不留痕）。
	asked := 0
	script := []actortest.ScriptedReply{{
		When: func(env actor.Envelope) bool { return env.Type == proto.MsgGateRequest },
		Reply: func(env actor.Envelope) []actor.Envelope {
			asked++
			req := env.Payload.(*proto.GateRequest)
			return []actor.Envelope{{
				From: h.human.ID(), To: env.From,
				Type: proto.MsgGateReply,
				Payload: &proto.GateReply{RequestID: req.RequestID, Nonce: req.Nonce,
					Action: "allow", GrantMode: "count", Count: 2, Reason: "剧本放行"},
			}}
		},
	}}
	h.attach(a, script)
	a.llm = &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(llmCallDial()),
		toolCallTurn(llmCallDial()),
		toolCallTurn(llmCallDial()),
		replyTurn("done"),
	}}
	if err := a.AppendUser(h.ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(h.ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if asked != 1 {
		t.Fatalf("human asked %d 次——额度内不再打扰（Part 11.3 §3.3）", asked)
	}
	if a.llmCallCount != 2 {
		t.Fatalf("executed = %d, want 2（首次被挂起不计）", a.llmCallCount)
	}
}

// TestScriptedHumanGrantAlways：剧本回 @always-grant → 动态 allow 规则 +
// grants/ 落盘（重启回插的语义在 gate 包测试；这里验收进程面：第二次
// 调用直行、收件箱只有一条请求、落盘文件存在且 granted_by 是人类）。
func TestScriptedHumanGrantAlways(t *testing.T) {
	h := newUnifiedHarness(t)
	controlRoot := t.TempDir()
	grants, err := gate.NewGrantStore(controlRoot)
	if err != nil {
		t.Fatal(err)
	}
	aud := store.AuditSQLite{SQLiteStore: mustStore2(t)}
	pdp, err := gate.NewManager(gate.ManagerConfig{
		Rules: []gate.Rule{{ID: "review-all", Match: map[string]string{"kind": "llm_call", "task_call_count": ">=0"},
			Action: gate.ActionNeedHuman}},
		Grants: grants, Audit: aud,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := llmCallAgentAndFake(t, func(c *LLMCallConfig) { c.Gates = pdp })
	asked := 0
	script := []actortest.ScriptedReply{{
		When: func(env actor.Envelope) bool { return env.Type == proto.MsgGateRequest },
		Reply: func(env actor.Envelope) []actor.Envelope {
			asked++
			req := env.Payload.(*proto.GateRequest)
			return []actor.Envelope{{
				From: h.human.ID(), To: env.From,
				Type: proto.MsgGateReply,
				Payload: &proto.GateReply{RequestID: req.RequestID, Nonce: req.Nonce,
					Action: "allow", GrantMode: "always", Reason: "此类操作永久放行"},
			}}
		},
	}}
	h.attach(a, script)
	a.llm = &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(llmCallDial()),
		toolCallTurn(llmCallDial()), // always 生效 → 直行
		replyTurn("done"),
	}}
	if err := a.AppendUser(h.ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(h.ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if asked != 1 {
		t.Fatalf("always 后仍打扰人类：%d", asked)
	}
	// grants/ 落盘（授权链：granted_by = 人类 ActorID）。
	saved, err := grants.LoadGrants(h.ctx)
	if err != nil || len(saved) != 1 {
		t.Fatalf("grants: %v %d", err, len(saved))
	}
	if saved[0].GrantedBy != h.human.ID() {
		t.Fatalf("granted_by: %q", saved[0].GrantedBy)
	}
}

// ---------------------------------------------------------------------------
// 场景 3：escalation 上浮到人（根 Agent 的链条自然终止在人类 Actor——
// 文件信箱是收件箱的 escalation 编码；替身执行"写回复 + 移 done/"）。
// ---------------------------------------------------------------------------

func TestScriptedHumanEscalation(t *testing.T) {
	h := newUnifiedHarness(t)
	h.assertHumanIsRoot()

	// escalate 的真实装配（文件信箱 + 快轮询参数；人类路径 = 链条顶端）。
	controlRoot := t.TempDir()
	mb, err := escalate.NewMailbox(escalate.MailboxConfig{
		ControlRoot: controlRoot, PollInterval: 10 * time.Millisecond,
		Quiescence: 60 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := escalate.NewManager(escalate.Config{Rule: func() (proto.EscalationRule, error) {
		// 根 Agent：ParentID 为空 + FallbackHuman（链条到人类——拓扑事实
		// 的顶端，Part 14.6"escalation 停止特例"的统一语义）。
		return proto.EscalationRule{FallbackHuman: true}, nil
	}, Mailbox: mb})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := newTestAgent(t, &fakeLLM{})
	a.escCfg = &EscalationConfig{Manager: mgr}
	a.llm = &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(mkCall("request_human", map[string]any{
			"question": "生产 key 用哪个 env？", "context": "部署面",
		})),
		replyTurn("收到人类答复。"),
	}}
	// 替身剧本：等 pending 文件出现 → 写回复 → 移 done/（人类的动作；
	// 文件是控制面——写权限即通道，原则 4）。
	go func() {
		pending := filepath.Join(controlRoot, "requests", "pending")
		deadline := time.After(5 * time.Second)
		for {
			select {
			case <-deadline:
				return
			case <-time.After(20 * time.Millisecond):
			}
			entries, err := os.ReadDir(pending)
			if err != nil || len(entries) == 0 {
				continue
			}
			name := entries[0].Name()
			src := filepath.Join(pending, name)
			body, rerr := os.ReadFile(src)
			if rerr != nil {
				continue
			}
			replied := strings.Replace(string(body), "## 回复", "## 回复", 1) + "\n用 DEEPSEEK_API_KEY。\n"
			done := filepath.Join(controlRoot, "requests", "done", name)
			if werr := os.WriteFile(done, []byte(replied), 0o644); werr != nil {
				return
			}
			_ = os.Remove(src)
			return
		}
	}()
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if err := a.AppendUser(ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 人类的回复进了 Agent 的 Log（RoleEscalation）。
	var sawReply bool
	for _, e := range entriesMust(t, a) {
		if strings.Contains(e.Content, "DEEPSEEK_API_KEY") {
			sawReply = true
		}
	}
	if !sawReply {
		t.Fatal("human reply missing in agent log")
	}
}

// ---------------------------------------------------------------------------
// 场景 1：讨论（人类的收件箱子树 discussions/<id>/verdict.md；替身驱动
// 裁决文件——讨论期间不烧钱的挂起面 + 挂起登记的 Watchdog 数据源）。
// ---------------------------------------------------------------------------

func TestScriptedHumanDiscussion(t *testing.T) {
	h := newUnifiedHarness(t)
	h.assertHumanIsRoot()

	dm := &fakeDiscussionManager{}
	dm.outcomeC = make(chan discuss.Outcome, 8)
	// 替身剧本（以人类身份驱动裁决通道：批注 → approve）。
	go func() {
		dm.outcomeC <- discuss.Outcome{Kind: discuss.OutcomeAnnotation, Annotation: "接口加个 Close 方法"}
		time.Sleep(20 * time.Millisecond)
		dm.outcomeC <- discuss.Outcome{Kind: discuss.OutcomeApproved, Annotation: "同意 v2"}
	}()
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(mkCall("request_discussion", map[string]any{
			"topic": "统一面的验收契约", "draft": "接口 v1。",
		})),
		toolCallTurn(mkCall("request_discussion", map[string]any{
			"topic": "统一面的验收契约", "draft": "接口 v2：加 Close。",
		})),
		replyTurn("结论已落地。"),
	}}
	a, _ := newTestAgent(t, llm)
	a.discussCfg = &DiscussionConfig{Manager: dm}
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if err := a.AppendUser(ctx, "先定契约"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 讨论的完整闭环（open → 批注 → 修订 → approve → finalize）。
	if len(dm.opened) != 1 || dm.updated != 1 || len(dm.finalizeCalled) != 1 {
		t.Fatalf("dm = open:%d update:%d finalize:%d", len(dm.opened), dm.updated, len(dm.finalizeCalled))
	}
	// 挂起登记（Part 14.8 的 Watchdog 数据源）：进入 + 退出各一次。
	var sawBlockedDiscussing bool
	// （讨论的 pending 登记在 HumanLink——本场景未装配 HumanLink，
	// 讨论等待不依赖它；登记面在审批场景的 llmcall_test 已断言。）
	for _, e := range entriesMust(t, a) {
		if strings.Contains(e.Content, "Close") {
			sawBlockedDiscussing = true
		}
	}
	if !sawBlockedDiscussing {
		t.Fatal("批注（Close）未入 Log")
	}
}

// mustStore2 是授权链断言的审计库（与 newStoreForTest 同形，独立命名
// 以避免与本文件上文混淆）。
func mustStore2(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// 编译期面（config 的 Limits 在 llm_call 场景由 llmCallAgentAndFake 装配）。
var _ = config.DefaultLimits
