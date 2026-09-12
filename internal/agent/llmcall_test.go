package agent

// llm_call 与缓存的破坏性的契约测试（Part 11.2 / 11.4，阶段 11）。

import (
	"context"
	"encoding/json"
	"testing"

	"marl/internal/ledger"

	"marl/internal/config"
	"marl/internal/gate"
	"marl/internal/proto"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
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
		c.Gates = gateMustManager(t, []gate.Rule{{ID: "allow-under", Match: map[string]string{"kind": string(gate.KindLLMCall)}, Action: gate.ActionAllow}}, nil)
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

// gateMustManager / fnApprover 是 gate.Manager 的测试装配（不止本文件用）。
func gateMustManager(t *testing.T, rules []gate.Rule, approver func(*gate.Request) (*gate.Decision, gate.Grant)) *gate.Manager {
	t.Helper()
	var ap gate.Approver
	if approver != nil {
		ap = &fnApprover{fn: func(req *gate.Request) (*gate.Decision, gate.Grant) {
			d, g := approver(req)
			return d, g
		}}
	}
	m, err := gate.NewManager(gate.ManagerConfig{Rules: rules, Approver: ap})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// fnApprover 是 Approver 的函数形态替身。
type fnApprover struct {
	fn func(*gate.Request) (*gate.Decision, gate.Grant)
}

func (f *fnApprover) Ask(ctx context.Context, req *gate.Request, rule gate.Rule) (*gate.Decision, gate.Grant, error) {
	d, g := f.fn(req)
	return d, g, nil
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
