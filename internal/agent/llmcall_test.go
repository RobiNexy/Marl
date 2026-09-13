package agent

// llm_call 与缓存的破坏性的契约测试（Part 11.2 / 11.4，阶段 11）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RobiNexy/Marl/internal/ledger"

	"github.com/RobiNexy/Marl/internal/config"
	"github.com/RobiNexy/Marl/internal/gate"
	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

const (
	llmLimitsInput     = 200
	llmLimitsCallCount = 20
)

var llmTestLimits = config.Limits{
	LLMCallMaxInputTokens:   200,
	LLMCallMaxOutputTokens:  2000,
	CallTaskMax:             llmLimitsCallCount,
	CallTaskMaxTokens:       100_000,
	LLMCallTimeoutMs:        120_000,
	CacheReviewPct:          10,
	StandingOrdersMaxTokens: 1000,
}

func llmCallDial() types.ToolCall {
	b, _ := json.Marshal(llmCallArgs{
		Messages: []llmCallMsg{
			{Role: "system", Content: "你是 sidecar 摘要器。"},
			{Role: "user", Content: "请摘要：hello"},
		},
		Wire:      "sidecar",
		Purpose:   "summarize",
		MaxTokens: 64,
	})
	return types.ToolCall{ID: "call-llmc", Name: proto.ToolLLMCall, Arguments: b}
}

type llmSidecarLine struct{}

func (*llmSidecarLine) ExecuteTurn(_ context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	e := &types.LogEntry{
		Role: types.RoleAssistantReply, Prov: types.ProvOriginal,
		Audience: types.AudienceBoth, Content: "摘要结果",
	}
	return &wire.WireTurn{Outcomes: []wire.Outcome{{
		Reply: "摘要结果", Entry: *e,
		Usage: &types.TokenUsage{PromptTokens: 10, CompletionTokens: 4},
	}}}, nil
}

// harness 装配：ipse limits + fake sidecar wire + fake gate approver。
func llmCallAgentAndFake(t *testing.T, cfg func(*LLMCallConfig)) (*Agent, *LLMCallConfig) {
	t.Helper()
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(llmCallDial()),
		replyTurn("已消费 sidecar 结果。"),
	}}
	a, _ := newTestAgent(t, llm)
	spec, _ := NewWireSet([]SidecarSpec{{
		Name: "sidecar",
		Ex:   &llmSidecarLine{},
		Binding: types.Binding{
			RungID:   "r0",
			Model:    "sidecar-model",
			Endpoint: "sidecar-endpoint",
			Wire:     types.WireOpenAIChat,
		},
	}}, []string{"sidecar-model"})
	c := &LLMCallConfig{
		Limits: llmTestLimits,
		Wires:  spec,
	}
	if cfg != nil {
		cfg(c)
	}
	a.llmCallCfg = c
	return a, c
}

// TestLLMCallHappy：限内 + allow 规则 → 执行、计数、账本（call_type=llm_call）。
func TestLLMCallHappy(t *testing.T) {
	ctx := context.Background()
	a, _ := llmCallAgentAndFake(t, func(c *LLMCallConfig) {
		c.Gates = gateMustManager(t, []gate.Rule{{ID: "allow-under", Match: map[string]string{"kind": string(gate.KindLLMCall)}, Action: gate.ActionAllow}})
	})
	// 记账面（call_type=llm_call 的真实落库）：真实 SQLite ledger。
	db, err := store.OpenSQLite(t.TempDir() + "/llm-ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	rec, lerr := ledger.New(db, &stubCatalog{})
	if lerr != nil {
		t.Fatal(lerr)
	}
	a.ledger = rec
	a.taskID = "llmc-task" // newTestAgent 的最小装配无 TaskID（阶段 2 形态）
	if err := a.AppendUser(ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.llmCallCount != 1 || a.llmCallTokens != 14 {
		t.Fatalf("counters: count=%d tokens=%d（10 in + 4 out）", a.llmCallCount, a.llmCallTokens)
	}
	// 记账面：call_type=llm_call 的 TaskSummary 分项。
	sum, err := db.TaskSummary(ctx, a.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if catV, ok := sum.ByCategory[store.CallLLMCall]; !ok || catV.Tokens != 14 {
		t.Fatalf("ledger ByCategory llm_call: %+v", sum.ByCategory)
	}
}

// gateMustManager 是 gate.Manager 的测试装配（不止本文件用；Part 14 起
// need_human 的裁决兑现走 ResolveGate——审批者不再是 Manager 的钩子，
// 而是人类 Actor 的 MsgGateReply）。
func gateMustManager(t *testing.T, rules []gate.Rule) *gate.Manager {
	t.Helper()
	m, err := gate.NewManager(gate.ManagerConfig{Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// stubCatalog 是 ledger 的计价替身（只需要 Pricing/计价面）。
type stubCatalog struct{}

func (stubCatalog) EffectiveCaps(modelID, endpoint string) (wire.ModelCaps, error) {
	return wire.ModelCaps{}, nil
}

func (stubCatalog) Pricing(modelID, endpoint string) (wire.Pricing, error) {
	return wire.Pricing{InPerMTok: 1, CachedInPerMTok: 0.5, OutPerMTok: 2, Currency: "CNY"}, nil
}

// Catalog 的其余接口（只被 ledger 的构造用 stub：Model/Endpoint/Ladder）。
func (stubCatalog) Model(id string) (*wire.ModelEntry, error) {
	return &wire.ModelEntry{ID: id}, nil
}

func (stubCatalog) Endpoint(name string) (*wire.EndpointConfig, error) {
	return &wire.EndpointConfig{Name: name}, nil
}

func (stubCatalog) Ladder() *types.Ladder {
	return &types.Ladder{
		Rungs: []types.Rung{{ID: "r0", Endpoint: "sidecar-endpoint", Model: "sidecar-model", CostPerMTok: 1, Currency: "CNY"}},
		Start: "r0",
	}
}

// TestLLMCallInputTooLarge：单次输入上限硬拒——消息里必须带两个数字
// （实际 token 数与上限；Part 11.2 §2.9 的消息纪律）。
func TestLLMCallInputTooLarge(t *testing.T) {
	ctx := context.Background()
	a, _ := llmCallAgentAndFake(t, func(c *LLMCallConfig) {
		c.Gates = gateMustManager(t, []gate.Rule{{
			ID: "allow-under", Match: map[string]string{"kind": "llm_call"}, Action: gate.ActionAllow,
		}})
	})
	huge := llmCallDial()
	var args llmCallArgs
	_ = json.Unmarshal(huge.Arguments, &args)
	args.Messages[1].Content = strings.Repeat("x", 500)
	b, _ := json.Marshal(args)
	huge.Arguments = b
	a.llm = &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(huge),
		replyTurn("收到拒绝，拆小后重调。"),
	}}
	if err := a.AppendUser(ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := ""
	for _, e := range entriesMust(t, a) {
		if e.Role == types.RoleToolResult && strings.Contains(e.Content, "INPUT_TOO_LARGE") {
			found = e.Content
		}
	}
	if found == "" {
		t.Fatal("INPUT_TOO_LARGE feedback missing")
	}
	if !strings.Contains(found, "521") || !strings.Contains(found, "200") {
		t.Fatalf("numbers missing: %s", found)
	}
	// 硬拒不计入调用次数也不写账本（物理 sanity 不是一次"调用"）。
	if a.llmCallCount != 0 || a.llmCallTokens != 0 {
		t.Fatalf("counter leak: %d/%d", a.llmCallCount, a.llmCallTokens)
	}
}

// TestLLMCallWireNotFound：wire 名不存在的错误消息**列出全部可用 wire**。
func TestLLMCallWireNotFound(t *testing.T) {
	ctx := context.Background()
	a, _ := llmCallAgentAndFake(t, func(c *LLMCallConfig) {
		c.Gates = gateMustManager(t, []gate.Rule{{
			ID: "allow-under", Match: map[string]string{"kind": "llm_call"}, Action: gate.ActionAllow,
		}})
	})
	call := llmCallDial()
	var args llmCallArgs
	_ = json.Unmarshal(call.Arguments, &args)
	args.Wire = "sidecar-typo"
	b, _ := json.Marshal(args)
	call.Arguments = b
	a.llm = &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(call),
		replyTurn("改用正确 wire。"),
	}}
	if err := a.AppendUser(ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, e := range entriesMust(t, a) {
		if e.Role == types.RoleToolResult &&
			strings.Contains(e.Content, "WIRE_NOT_FOUND") &&
			strings.Contains(e.Content, "main") && strings.Contains(e.Content, "sidecar") {
			found = true
		}
	}
	if !found {
		t.Fatal("wire list feedback missing")
	}
}

// TestLLMCallModelNotInCatalog：model 覆盖不在 AllowModels → 拒且列出。
func TestLLMCallModelNotInCatalog(t *testing.T) {
	ctx := context.Background()
	a, _ := llmCallAgentAndFake(t, func(c *LLMCallConfig) {
		c.Gates = gateMustManager(t, []gate.Rule{{
			ID: "allow-under", Match: map[string]string{"kind": "llm_call"}, Action: gate.ActionAllow,
		}})
	})
	call := llmCallDial()
	var args llmCallArgs
	_ = json.Unmarshal(call.Arguments, &args)
	args.Model = "unknown-strong"
	b, _ := json.Marshal(args)
	call.Arguments = b
	a.llm = &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(call),
		replyTurn("换 catalog 内模型。"),
	}}
	if err := a.AppendUser(ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, e := range entriesMust(t, a) {
		if e.Role == types.RoleToolResult &&
			strings.Contains(e.Content, "MODEL_NOT_IN_CATALOG") &&
			strings.Contains(e.Content, "sidecar-model") {
			found = true
		}
	}
	if !found {
		t.Fatal("catalog list feedback missing")
	}
}

// TestLLMCallGatePendingGrant：need_human（task_call_count>=20 的字面规则）
// → 审批请求投递给人类 Actor（MsgGateRequest，Part 14.7）→ 回执信封
// （MsgGateReply + count=2 的 grant）→ 模型重发直行（额度内不再打扰
// 人类——Part 11.3 §3.3 的"问一次给一批"）。挂起登记（MarkPending/
// ClearPending）是 Watchdog 停摆告警的数据源，一并断言。
func TestLLMCallGatePendingGrant(t *testing.T) {
	// 规则：第一次（count=0）就 need_human（>=0），审批→count=2 的额度。
	a, _ := llmCallAgentAndFake(t, func(c *LLMCallConfig) {
		c.Gates = gateMustManager(t, []gate.Rule{{
			ID:     "review-all",
			Match:  map[string]string{"kind": "llm_call", "task_call_count": ">=0"},
			Action: gate.ActionNeedHuman,
			Reason: "测试：每次都要人",
		}})
	})
	link := newFakeHumanLink(nil)
	wireHuman(a, link)
	// 审计面接真库（granted_by 授权链的断言数据源；newTestAgent 无 Audit）。
	adb, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	a.audit = store.AuditSQLite{SQLiteStore: adb}
	first := llmCallDial()
	a.llm = &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(first), // 第一次 → 审批往返
		toolCallTurn(first), // 重发（grant 额度内直行）
		toolCallTurn(first), // 再发（2 次额度内的第二次直行）
		replyTurn("三次调用完成。"),
	}}
	ctx := context.Background()
	if err := a.AppendUser(ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(link.sentRequests()) != 1 {
		t.Fatalf("human asked %d 次——额度内不应再问（Part 11.3 §3.3）", len(link.sentRequests()))
	}
	// 挂起登记的成对性：awaitGate 进入 + 恢复各一次。
	link.mu.Lock()
	defer link.mu.Unlock()
	if len(link.pendings) != 2 || link.pendings[0] != "+awaiting_gate" || link.pendings[1] != "-" {
		t.Fatalf("pending 登记: %v", link.pendings)
	}
	// 等式：首次调用被 GATE_PENDING 拒（未执行、不计数），重发 2 次在
	// grant 额度内直行（Part 11.2 §2.7 的"挂起不是一次调用"语义）。
	if a.llmCallCount != 2 {
		t.Fatalf("executed lives = %d, want 2", a.llmCallCount)
	}
	sawPending, sawVerdict, sawGateFile := false, false, false
	for _, e := range entriesMust(t, a) {
		if e.Role == types.RoleToolResult && strings.Contains(e.Content, "GATE_PENDING_HUMAN") {
			sawPending = true
		}
		if e.Role == types.RoleHumanNote && strings.Contains(e.Content, "Gate 裁决") {
			sawVerdict = true
		}
	}
	// 审计面：granted_by 的授权链（Part 14.10）。
	for _, ev := range auditEventsMust(t, a) {
		if ev.Action != "gate_resolved" {
			continue
		}
		if pm, ok := ev.Payload.(map[string]any); ok {
			if gb, ok := pm["granted_by"]; ok && gb == "human:tester" {
				sawGateFile = true
			}
		}
	}
	if !sawPending || !sawVerdict || !sawGateFile {
		t.Fatalf("pending/verdict/授权链缺失（pending=%v verdict=%v granted_by=%v）", sawPending, sawVerdict, sawGateFile)
	}
}

// TestLLMCallGateNoHumanDenied：人类 Actor 未装配 → need_human 兑现为
// 显式拒绝（fail-closed："问不了人"不能变成"不用问"——Part 14.7）。
func TestLLMCallGateNoHumanDenied(t *testing.T) {
	a, _ := llmCallAgentAndFake(t, func(c *LLMCallConfig) {
		c.Gates = gateMustManager(t, []gate.Rule{{
			ID: "review-all", Match: map[string]string{"kind": "llm_call", "task_call_count": ">=0"},
			Action: gate.ActionNeedHuman,
		}})
	})
	a.llm = &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(llmCallDial()),
		replyTurn("收到拒绝。"),
	}}
	if err := a.AppendUser(context.Background(), "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, e := range entriesMust(t, a) {
		if e.Role == types.RoleToolResult && strings.Contains(e.Content, "GATE_DENIED") &&
			strings.Contains(e.Content, "No human approver is configured") {
			found = true
		}
	}
	if !found {
		t.Fatal("GATE_DENIED (no human actor) feedback missing")
	}
	if a.gatePending != nil {
		t.Fatal("无人类表面时不得留下挂起")
	}
}

// auditEventsMust 读取测试 Agent 的全部审计事件（授权链断言用）。
func auditEventsMust(t *testing.T, a *Agent) []*store.AuditEvent {
	t.Helper()
	if a.audit == nil {
		return nil
	}
	evs, err := a.audit.Query(context.Background(), store.AuditFilter{Limit: 500})
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	return evs
}
