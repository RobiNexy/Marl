package agent

// 阶段 4 的升级集成测试（13.6 测试规格）：构造一个"tool_call 总是返回格式
// 错误"的任务 → Agent 从 r0 起跑 → 两次失败后升级到 r1 → Ledger 记录两级
// 的 token 消耗 → 审计有 model_upgrade → 升级后请求的 thinking 档位切换。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"marl/internal/ladder"
	"marl/internal/ledger"
	"marl/internal/skill"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
)

// testUsage 是测试用的固定用量（记账断言的输入）。
func testUsage() *types.TokenUsage {
	return &types.TokenUsage{PromptTokens: 1000, CompletionTokens: 200, CacheReadTokens: 800}
}

// withUsage 给 turn 的全部 Outcome 填同一份 usage（Part 10.11：同一 turn
// 共享一个 *TokenUsage）。
func withUsage(turn *wire.WireTurn, u *types.TokenUsage) *wire.WireTurn {
	for i := range turn.Outcomes {
		turn.Outcomes[i].Usage = u
	}
	return turn
}

// badArgsTurn 构造一个参数非法的 tool_call（BAD_ARGS → 格式错误证据）。
func badArgsTurn() *wire.WireTurn {
	return withUsage(toolCallTurn(types.ToolCall{
		ID: "call-bad", Name: "file_read", Arguments: json.RawMessage(`{not json`),
	}), testUsage())
}

// setupUpgrade 装配两档阶梯（同模型不同 thinking，Part 7.1 的 r0/r1）+
// 真实 SQLite 账本与审计。
func setupUpgrade(t *testing.T) (*Agent, *fakeLLM, *store.SQLiteStore, *ladder.Config) {
	t.Helper()
	st, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testLadderCfg2()
	cat := testCatalog2(t, cfg)
	rec, err := ledger.New(st, cat)
	if err != nil {
		t.Fatal(err)
	}
	router, err := ladder.NewRouter(cat, cfg, ladder.RouterPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := router.Bind(types.Requirement{Require: []types.Capability{types.CapToolCall}}, "up-1", cfg.Ladder, 0)
	if err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{}
	root := workspaceRoot(t)
	a, err := New(Config{
		ID:           "up-1",
		SystemPrompt: "agent",
		MaxRounds:    8,
		Log:          st,
		Views:        st,
		LLM:          llm,
		Skills:       mustRegistry(t),
		Namespace:    &types.Namespace{AgentID: "up-1", Mounts: []types.Mount{{Pattern: "**", Mode: types.PathWrite}}},
		Resolver:     mustResolver(t, root),
		ProjectRoot:  root,
		Sampling:     types.SamplingParams{MaxTokens: 512},
		TaskID:       "task-up",
		Ledger:       rec,
		Audit:        store.AuditSQLite{SQLiteStore: st},
		Upgrader: &UpgradeConfig{
			Router:      router,
			Ladder:      cfg,
			Catalog:     cat,
			Requirement: types.Requirement{Require: []types.Capability{types.CapToolCall}},
			Policy:      ladder.EvidencePolicy{Threshold: 0.8, FailureWeight: 0.4, FormatErrorWeight: 0.4, NoProgressWeight: 0.15, ChildFailureRateWeight: 1.0, ReclaimLowWeight: 0.1},
		},
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if err := a.SetBinding(binding); err != nil {
		t.Fatal(err)
	}
	return a, llm, st, cfg
}

// testLadderCfg2 / testCatalog2 是 ladder 包私有测试基建的 agent 侧镜像
//（两档同模型阶梯 + 静态目录；跨包不可复用私有件，镜像的漂移由两侧各自的
// golden 断言守住）。
func testLadderCfg2() *ladder.Config {
	return &ladder.Config{
		Ladder: &types.Ladder{
			Rungs: []types.Rung{
				{ID: "r0", Endpoint: "ep", Model: "deepseek/chat", CostPerMTok: 1.0, Currency: "CNY"},
				{ID: "r1", Endpoint: "ep", Model: "deepseek/chat", CostPerMTok: 1.0, Currency: "CNY"},
			},
			Start: "r0",
		},
		Thinking: map[types.RungID]types.ThinkingSpec{
			"r0": {Level: "off"},
			"r1": {Level: "high"},
		},
	}
}

func testCatalog2(t *testing.T, cfg *ladder.Config) *ladder.StaticCatalog {
	t.Helper()
	cat := ladder.NewStaticCatalog(cfg.Ladder)
	if err := cat.AddModel(wire.ModelEntry{
		ID: "deepseek/chat", Provider: "deepseek", Wire: types.WireOpenAIChat, RemoteName: "deepseek-flash",
		Caps: wire.ModelCaps{
			Has:             []types.Capability{types.CapToolCall, types.CapJSONMode, types.CapThinking},
			MaxContext:      65536, MaxOutput: 8192,
			CacheMode:       wire.CacheImplicitPrefix,
			ThinkingControl: wire.ThinkControlLevel,
			ThinkingLevels:  []string{"none", "low", "high", "max"},
		},
		Pricing: wire.Pricing{InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddEndpoint(wire.EndpointConfig{Name: "ep", BaseURL: "https://api.example.com/v1", KeyRef: "env:X", MaxInflight: 4, RPM: 60}); err != nil {
		t.Fatal(err)
	}
	return cat
}

func mustRegistry(t *testing.T) skill.Registry {
	t.Helper()
	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite} {
		if err := reg.Register(sk); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func TestUpgradeAfterFormatErrors(t *testing.T) {
	ctx := context.Background()
	a, llm, st, cfg := setupUpgrade(t)

	// 两轮格式错误 → 升级；第三轮正常回复。
	llm.turns = []*wire.WireTurn{badArgsTurn(), badArgsTurn(),
		withUsage(replyTurn("换了更强模型后完成了。"), testUsage())}
	if err := a.AppendUser(ctx, "do things"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 升级发生了：Binding 的档位与 thinking 切到 r1。
	if a.binding.RungID != "r1" {
		t.Fatalf("rung = %s, want r1", a.binding.RungID)
	}
	if a.binding.Thinking.Level != "high" {
		t.Fatalf("thinking = %q, want high (ADR-0022: ladder is the source)", a.binding.Thinking.Level)
	}
	// 升级后的请求带 thinking=high（发给 LLM 的 CanonicalRequest 口径）。
	last := llm.reqs[len(llm.reqs)-1]
	if last.Thinking.Level != "high" {
		t.Fatalf("request thinking = %q", last.Thinking.Level)
	}
	// Transient 告知出现且只出现一轮（Part 3.6：用完即扔——升级发生在
	// 第 2 轮末，Transient 只进第 3 轮的请求；任务在第 3 轮结束，
	// clearTransients 在轮边界已执行）。
	transientReqs := 0
	for _, req := range llm.reqs {
		for _, seg := range req.Segments {
			if seg.Kind == wire.SegTransient && strings.Contains(seg.Content, "更强档位") {
				transientReqs++
			}
		}
	}
	if transientReqs != 1 {
		t.Fatalf("transient must appear in exactly one request, got %d", transientReqs)
	}

	// Ledger 记录两级的 token 消耗。
	sum, err := st.TaskSummary(ctx, "task-up")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.ByLevel["r0"] == nil || sum.ByLevel["r0"].Calls != 2 {
		t.Fatalf("r0 entries: %+v", sum.ByLevel["r0"])
	}
	if sum.ByLevel["r1"] == nil || sum.ByLevel["r1"].Calls != 1 {
		t.Fatalf("r1 entries: %+v", sum.ByLevel["r1"])
	}
	if sum.TotalCost <= 0 {
		t.Fatalf("cost must be positive: %v", sum.TotalCost)
	}

	// 审计有 model_upgrade（upgraded=true）。
	audit := store.AuditSQLite{SQLiteStore: st}
	events, err := audit.Query(ctx, store.AuditFilter{Action: AuditModelUpgrade})
	if err != nil || len(events) != 1 {
		t.Fatalf("audit: %v %v", events, err)
	}
	payload, _ := events[0].Payload.(map[string]any)
	if payload["upgraded"] != true || payload["to_rung"] != "r1" {
		t.Fatalf("audit payload: %+v", payload)
	}
	_ = cfg
}

func TestUpgradeNoEvidenceNoUpgrade(t *testing.T) {
	ctx := context.Background()
	a, llm, _, _ := setupUpgrade(t)
	llm.turns = []*wire.WireTurn{
		withUsage(toolCallTurn(mkCallID("file_read", "c1", map[string]any{"path": "README.md"})), testUsage()),
		withUsage(replyTurn("done"), testUsage()),
	}
	writeFile(t, workspaceRootOf(a), "README.md", "x\n")
	if err := a.AppendUser(ctx, "read"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.binding.RungID != "r0" {
		t.Fatalf("rung = %s, want r0 (no evidence)", a.binding.RungID)
	}
}

func TestUpgradeTransientErrorsNotEvidence(t *testing.T) {
	ctx := context.Background()
	a, llm, _, _ := setupUpgrade(t)
	// ErrTransient（限流）不计入升级证据：两轮 transient 失败 → 循环终结
	// 但不升级（Run 返回错误是既有语义——这里验证的是升级未发生）。
	llm.turns = []*wire.WireTurn{
		{Outcomes: []wire.Outcome{{Signals: wire.OutcomeSignals{ErrorClass: wire.ErrTransient}}}},
		{Outcomes: []wire.Outcome{{Signals: wire.OutcomeSignals{ErrorClass: wire.ErrTransient}}}},
	} // 厂商错误无 usage：不记账（ledger 契约），本测试不查账
	if err := a.AppendUser(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	err := a.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "transient") {
		t.Fatalf("err = %v", err)
	}
	if a.binding.RungID != "r0" {
		t.Fatalf("rung = %s, want r0 (transient is not capability evidence)", a.binding.RungID)
	}
}

// workspaceRootOf 从 agent 的 env 里取工作区根（测试辅助）。
func workspaceRootOf(a *Agent) string { return a.env.ProjectRoot }


