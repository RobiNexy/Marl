package gateway

// Gateway 的规格测试：把包注释与各符号 godoc 里的契约翻译成可执行断言。
// 全部用 gatewaytest.Stub（脚本化替身）做确定性验证——不依赖 SQLite/网络。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/frontend/gateway/gatewaytest"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// waitForState 等到 Gateway 发布首个快照（轮询循环异步；deadline 轮询，
// 超时 t.Fatal 携带上下文）。
func waitForState(t *testing.T, g *Gateway) *State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := g.State(); st != nil {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("gateway 未在 3s 内发布首帧快照")
	return nil
}

// TestGateway_EventCursorIncremental 验证事件游标契约：首轮拉历史（含
// 既有事件），后续轮只拉新增（不重发旧事件）。
func TestGateway_EventCursorIncremental(t *testing.T) {
	stub := gatewaytest.New()
	stub.AddEvents("spawn", "agent_state") // 启动前已有两条历史
	g := NewWithInteraction(stub, Config{Tick: 20 * time.Millisecond})
	defer g.Close()
	ch, cancel := g.Subscribe()
	defer cancel()

	st := waitForState(t, g)
	if st.LastSeq != 2 {
		t.Fatalf("首轮游标应推进到 2（历史两条），got %d", st.LastSeq)
	}
	if n := len(st.Events); n != 2 {
		t.Fatalf("首轮事件窗口应有 2 条历史，got %d", n)
	}

	// 新增一条 → 下一帧只含新事件，游标推进。
	// 先排干积压批次（waitForState 期间首帧可能已入通道），再注入新事件。
	for drained := false; !drained; {
		select {
		case <-ch:
		default:
			drained = true
		}
	}
	stub.AddEvents("gate_request")
	deadline := time.Now().Add(3 * time.Second)
	for {
		st = g.State()
		if st != nil && st.LastSeq == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("游标未推进到 3（当前 %d）", st.LastSeq)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 从订阅通道里应该能看到恰好 1 条新事件的批次（全量快照语义：
	// 批次的 Events 只含本轮新增）。
	var got int
	deadline = time.Now().Add(3 * time.Second)
	for got == 0 && time.Now().Before(deadline) {
		select {
		case u, ok := <-ch:
			if !ok {
				t.Fatal("订阅通道被提前关闭")
			}
			got += len(u.Events)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got != 1 {
		t.Fatalf("增量批次应含 1 条新事件，got %d（重发旧事件=游标契约破坏）", got)
	}
}

// TestGateway_ClassifiedEvents 验证折算面：audit Action → 稳定 Kind，
// 未知 Action → KindOther（开放集合的兜底契约）。
func TestGateway_ClassifiedEvents(t *testing.T) {
	cases := []struct {
		action string
		want   Kind
	}{
		{"gate_request", KindGate},
		{"gate_pending", KindGate},
		{"gate_decision", KindGate},
		{"spawn", KindSpawn},
		{"spawn_batch", KindSpawn},
		{"actor_registered", KindSpawn},
		{"agent_state", KindAgentState},
		{"model_upgrade", KindModel},
		{"model_switch", KindModel},
		{"watchdog_no_progress", KindWatchdog},
		{"watchdog_terminated", KindWatchdog},
		{"discussion_opened", KindDiscussion},
		{"escalation_opened", KindEscalation},
		{"escalation_reply_received", KindEscalation},
		{"orchestrate", KindOrchestrate},
		{"commit", KindCommit},
		{"brand_new_action_from_future_module", KindOther}, // 开放集合兜底
		{"", KindOther},                                    // 空 Action 也是"未知"
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.action, func(t *testing.T) {
			got := Classify(store.AuditEvent{Action: tc.action})
			if got.Kind != tc.want {
				t.Fatalf("Classify(%q): want %q got %q", tc.action, tc.want, got.Kind)
			}
			if got.Action != tc.action {
				t.Fatalf("原文必须透传：want %q got %q", tc.action, got.Action)
			}
		})
	}
}

// TestGateway_CommandsForwardAndNudge 验证命令面：转发给契约实现 + 记录 +
// 成功后快速刷新（nudge）。
func TestGateway_CommandsForwardAndNudge(t *testing.T) {
	stub := gatewaytest.New()
	g := NewWithInteraction(stub, Config{Tick: time.Hour}) // 长周期：迫使刷新走 nudge
	defer g.Close()
	waitForState(t, g)

	if err := g.StartTask("测试任务"); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	if len(stub.StartedTasks) != 1 || stub.StartedTasks[0] != "测试任务" {
		t.Fatalf("命令未转发: %+v", stub.StartedTasks)
	}
	// nudge 刷新是异步的：按 deadline 轮询到运行态可见（而不是立即断言）。
	deadline := time.Now().Add(3 * time.Second)
	for run := g.State().Run; !run.Active || run.Task != "测试任务"; run = g.State().Run {
		if time.Now().After(deadline) {
			t.Fatalf("nudge 后运行态未刷新: %+v", run)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := g.SendMessage("sub_000001", "你好"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if len(stub.SentMessages) != 1 || stub.SentMessages[0] != "sub_000001|你好" {
		t.Fatalf("SendMessage 未转发: %+v", stub.SentMessages)
	}

	if err := g.ReplyGate("g1", contract.GateDecision{Action: "allow", Mode: "once"}); err != nil {
		t.Fatalf("ReplyGate: %v", err)
	}
	if len(stub.RepliedGates) != 1 {
		t.Fatalf("ReplyGate 未转发: %+v", stub.RepliedGates)
	}
}

// TestGateway_CommandErrorPropagates 验证错误路径：契约实现的错误原样上抛
// （带上下文包装但不吞）。
func TestGateway_CommandErrorPropagates(t *testing.T) {
	stub := gatewaytest.New()
	stub.OnStartTask = func(task string) error { return contract.ErrNoDaemon }
	g := NewWithInteraction(stub, Config{Tick: time.Hour})
	defer g.Close()
	waitForState(t, g)

	if err := g.StartTask("x"); err == nil {
		t.Fatal("替身错误应原样上抛，got nil")
	}
}

// TestGateway_SnapshotRefresh 验证快照面：树/收件箱/待办/成本在每帧刷新。
func TestGateway_SnapshotRefresh(t *testing.T) {
	stub := gatewaytest.New()
	stub.AgentsData = []contract.AgentView{
		{ID: "sub_000001", Parent: "human:1000", Kind: "agent", Depth: 1, State: "running"},
		{ID: "human:1000", Kind: "human", Depth: 0, State: "idle"},
	}
	stub.InboxData = []contract.InboxItem{{Name: "gate_abc.md", Type: "gate", From: "sub_000001", Preview: "需要审批"}}
	stub.EscalData = []contract.EscalationView{{ID: "escalation_x", From: "sub_000002", Question: "怎么办"}}
	stub.CostsData = &store.TaskCostSummary{
		TaskID: "start-task", Currency: "CNY", TotalCost: 0.5, TotalTokens: 1000,
		ByLevel: map[types.RungID]*store.LevelSummary{
			"r0": {Calls: 3, Tokens: 1000, ReasoningTokens: 700, CacheRead: 100, Cost: 0.5},
		},
	}
	g := NewWithInteraction(stub, Config{Tick: 20 * time.Millisecond})
	defer g.Close()
	st := waitForState(t, g)

	// 树：human 为根、agent 为子。
	if len(st.Tree) != 1 || st.Tree[0].Kind != "human" {
		t.Fatalf("树根应为 human: %+v", st.Tree)
	}
	if len(st.Tree[0].Children) != 1 || st.Tree[0].Children[0].ID != "sub_000001" {
		t.Fatalf("子节点缺失: %+v", st.Tree[0].Children)
	}

	// 待办：escalation 优先于 gate。
	items := ActionItems(st)
	if len(items) != 2 {
		t.Fatalf("应有 2 条待办，got %d: %+v", len(items), items)
	}
	if items[0].Kind != ActionEscalation || items[1].Kind != ActionGate {
		t.Fatalf("待办顺序错误（escalation 应最前）: %+v", items)
	}
	if items[1].ID != "abc" {
		t.Fatalf("gate id 应从文件名提取: %q", items[1].ID)
	}

	// 成本推导。
	if st.CostView.TotalCost != 0.5 || st.CostView.Calls != 3 {
		t.Fatalf("成本推导错误: %+v", st.CostView)
	}
	if st.CostView.ReasoningShare < 0.69 || st.CostView.ReasoningShare > 0.71 {
		t.Fatalf("思维链占比应约 0.70: %f", st.CostView.ReasoningShare)
	}
	if len(st.CostView.Suggestions) == 0 {
		t.Fatal("思维链 70% 应触发建议")
	}
}

// TestBuildTree 的边界矩阵：孤儿提升、排序稳定、空列表。
func TestBuildTree(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		if got := BuildTree(nil); len(got) != 0 {
			t.Fatalf("空输入应得空树，got %+v", got)
		}
	})
	t.Run("siblings sorted by id", func(t *testing.T) {
		root := BuildTree([]contract.AgentView{
			{ID: "b", Parent: "h"}, {ID: "a", Parent: "h"}, {ID: "h", Kind: "human"},
		})
		if len(root) != 1 || len(root[0].Children) != 2 {
			t.Fatalf("树形状错误: %+v", root)
		}
		if root[0].Children[0].ID != "a" {
			t.Fatalf("同级应按 ID 升序: %+v", root[0].Children)
		}
	})
	t.Run("orphan promoted to root", func(t *testing.T) {
		root := BuildTree([]contract.AgentView{
			{ID: "orphan", Parent: "ghost"}, // 父不存在
			{ID: "h", Kind: "human"},
		})
		if len(root) != 2 {
			t.Fatalf("孤儿应提升为根而非丢失: %+v", root)
		}
	})
	t.Run("self parent guarded", func(t *testing.T) {
		root := BuildTree([]contract.AgentView{{ID: "x", Parent: "x"}})
		if len(root) != 1 || root[0].ID != "x" {
			t.Fatalf("自指父应按根处理: %+v", root)
		}
	})
}

// TestDeriveCosts 的零值与阈值矩阵。
func TestDeriveCosts(t *testing.T) {
	t.Run("nil summary", func(t *testing.T) {
		cv := DeriveCosts(nil)
		if cv.TotalCost != 0 || cv.Suggestions != nil || cv.Levels != nil {
			t.Fatalf("nil 输入应得零值: %+v", cv)
		}
	})
	t.Run("cache hit low suggests prefix stability", func(t *testing.T) {
		cv := DeriveCosts(&store.TaskCostSummary{
			Currency: "CNY", TotalTokens: 1000,
			ByLevel: map[types.RungID]*store.LevelSummary{
				"r0": {Calls: 2, Tokens: 1000, CacheRead: 50, Cost: 0.1},
			},
		})
		if cv.CacheHitRate > 0.05 {
			t.Fatalf("命中率应约 5%%: %f", cv.CacheHitRate)
		}
		if len(cv.Suggestions) != 1 || !strings.Contains(cv.Suggestions[0], "前缀稳定性") {
			t.Fatalf("低命中应建议查前缀: %+v", cv.Suggestions)
		}
	})
	t.Run("no suggestions in normal band", func(t *testing.T) {
		cv := DeriveCosts(&store.TaskCostSummary{
			Currency: "CNY", TotalTokens: 1000,
			ByLevel: map[types.RungID]*store.LevelSummary{
				"r0": {Calls: 2, Tokens: 1000, CacheRead: 700, ReasoningTokens: 100, Cost: 0.1},
			},
		})
		if len(cv.Suggestions) != 0 {
			t.Fatalf("健康区间不应有建议: %+v", cv.Suggestions)
		}
	})
}

// TestGateway_ContextConversation 验证会话直连（选中 agent 的按需读取）。
func TestGateway_ContextConversation(t *testing.T) {
	stub := gatewaytest.New()
	g := NewWithInteraction(stub, Config{Tick: time.Hour})
	defer g.Close()
	entries, err := g.Conversation(context.Background(), "sub_000001")
	if err != nil || len(entries) != 1 || entries[0].Content != "stub 内容" {
		t.Fatalf("Conversation 直连失败: %v %+v", err, entries)
	}
}
