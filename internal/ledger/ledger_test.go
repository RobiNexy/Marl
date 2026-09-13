package ledger

// ledger 服务层测试：成本公式、Recorder 落账、报表渲染（Part 7.6）。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

func testPricing() wire.Pricing {
	return wire.Pricing{InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"}
}

func testBinding(rung, model string) types.Binding {
	return types.Binding{RungID: types.RungID(rung), Endpoint: "ep", Model: model, CacheBucket: "a1"}
}

func TestCostFormula(t *testing.T) {
	p := testPricing()
	// prompt=1000（含缓存命中 400）、completion=500（含思维链 200）：
	// cost = 600×1/1M + 400×0.25/1M + 300×2/1M + 200×2/1M
	//      = 0.0006 + 0.0001 + 0.0006 + 0.0004 = 0.0017
	u := types.TokenUsage{PromptTokens: 1000, CompletionTokens: 500, ReasoningTokens: 200, CacheReadTokens: 400}
	got := Cost(p, u)
	if diff := got - 0.0017; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost = %v, want 0.0017", got)
	}
	// 异常口径防御：CacheRead > Prompt 不产生负成本。
	u2 := types.TokenUsage{PromptTokens: 10, CacheReadTokens: 100, CompletionTokens: 5, ReasoningTokens: 10}
	if Cost(p, u2) < 0 {
		t.Fatal("cost must never be negative")
	}
}

// fakeCatalog 是测试用的最小 wire.Catalog（只实现 Pricing；其它方法报错）。
type fakeCatalog struct {
	pricing map[string]wire.Pricing
}

func (f *fakeCatalog) Model(id string) (*wire.ModelEntry, error) {
	return nil, fmt.Errorf("not implemented in test catalog")
}
func (f *fakeCatalog) Endpoint(name string) (*wire.EndpointConfig, error) {
	return nil, fmt.Errorf("not implemented in test catalog")
}
func (f *fakeCatalog) Ladder() *types.Ladder { return nil }
func (f *fakeCatalog) EffectiveCaps(modelID, endpoint string) (wire.ModelCaps, error) {
	return wire.ModelCaps{}, fmt.Errorf("not implemented in test catalog")
}
func (f *fakeCatalog) Pricing(modelID, endpoint string) (wire.Pricing, error) {
	p, ok := f.pricing[modelID]
	if !ok {
		return wire.Pricing{}, fmt.Errorf("model %q not found", modelID)
	}
	return p, nil
}

func newTestRecorder(t *testing.T) (*Recorder, *store.SQLiteStore, *fakeCatalog) {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cat := &fakeCatalog{pricing: map[string]wire.Pricing{
		"deepseek-flash":  testPricing(),
		"deepseek-v4-pro": {InPerMTok: 4.0, CachedInPerMTok: 1.0, OutPerMTok: 8.0, ReasoningPerMTok: 8.0, Currency: "CNY"},
	}}
	rec, err := New(s, cat)
	if err != nil {
		t.Fatal(err)
	}
	return rec, s, cat
}

func TestRecorderRecordsCost(t *testing.T) {
	ctx := context.Background()
	rec, s, _ := newTestRecorder(t)

	u := &types.TokenUsage{PromptTokens: 1000, CompletionTokens: 500, ReasoningTokens: 200, CacheReadTokens: 400}
	if err := rec.RecordMain(ctx, "t1", "a1", testBinding("r0", "deepseek-flash"), u); err != nil {
		t.Fatalf("record: %v", err)
	}
	// 编排入口强制类别。
	if err := rec.RecordOrchestration(ctx, "t1", "a1", testBinding("r0", "deepseek-flash"), u); err != nil {
		t.Fatal(err)
	}
	sum, err := s.TaskSummary(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if sum.ByCategory[store.CallMain].Calls != 1 || sum.ByCategory[store.CallOrchestration].Calls != 1 {
		t.Fatalf("categories: %+v", sum.ByCategory)
	}
	if sum.TotalCost <= 0 {
		t.Fatalf("cost must be positive: %v", sum.TotalCost)
	}
	// 用量未知不记账（≠ 0）。
	if err := rec.RecordMain(ctx, "t1", "a1", testBinding("r0", "deepseek-flash"), nil); err != nil {
		t.Fatalf("nil usage must be a silent skip: %v", err)
	}
	sum2, _ := s.TaskSummary(ctx, "t1")
	if sum2.ByCategory[store.CallMain].Calls != 1 {
		t.Fatalf("nil usage recorded: %+v", sum2.ByCategory[store.CallMain])
	}
	// 未知模型计价失败必须可见（拒绝记账）。
	err = rec.RecordMain(ctx, "t1", "a1", testBinding("r0", "ghost/model"), u)
	if err == nil || !strings.Contains(err.Error(), "pricing") {
		t.Fatalf("missing pricing must fail loudly: %v", err)
	}
}

func TestRecorderValidation(t *testing.T) {
	if _, err := New(nil, nil); err == nil {
		t.Fatal("nil deps must be rejected")
	}
}

// ---------------------------------------------------------------------------
// 报表渲染（Part 7.6 的形态）
// ---------------------------------------------------------------------------

func TestRenderReport(t *testing.T) {
	ctx := context.Background()
	rec, s, _ := newTestRecorder(t)
	// r0 两次 + 编排一次；r1 一次（含思维链）。
	if err := rec.RecordMain(ctx, "t1", "a1", testBinding("r0", "deepseek-flash"),
		&types.TokenUsage{PromptTokens: 45000, CompletionTokens: 3000, CacheReadTokens: 35000}); err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordMain(ctx, "t1", "a1", testBinding("r0", "deepseek-flash"),
		&types.TokenUsage{PromptTokens: 45000, CompletionTokens: 3000, CacheReadTokens: 35000}); err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordOrchestration(ctx, "t1", "a1", testBinding("r0", "deepseek-flash"),
		&types.TokenUsage{PromptTokens: 3000, CompletionTokens: 500}); err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordMain(ctx, "t1", "a1", testBinding("r1", "deepseek-flash"),
		&types.TokenUsage{PromptTokens: 8000, CompletionTokens: 9000, ReasoningTokens: 3400, CacheReadTokens: 4000}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordModelSwitch(ctx, &store.ModelSwitchEvent{
		AgentID: "a1", TaskID: "t1", FromModel: "deepseek-flash", ToModel: "deepseek-flash",
		Reason: "evidence score 0.80 >= threshold 0.80",
	}); err != nil {
		t.Fatal(err)
	}

	sum, err := rec.TaskSummary(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	switches, err := s.QueryModelSwitch(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	report := Render(sum, switches)
	if !strings.Contains(report, "r0") || !strings.Contains(report, "r1") || !strings.Contains(report, "编排") {
		t.Fatalf("report missing sections:\n%s", report)
	}
	if !strings.Contains(report, "思维链占比") || !strings.Contains(report, "换模型记录") {
		t.Fatalf("report missing metrics:\n%s", report)
	}
	// 思维链占比出现在 r1 行（3400/17000 ≈ 20%）。
	if !strings.Contains(report, "20%") {
		t.Fatalf("reasoning share missing:\n%s", report)
	}
	t.Logf("\n%s", report)
}

func TestRenderEmptySummary(t *testing.T) {
	// 空分项不崩（防御：TaskSummary 不可能空返回——ErrNotFound 兜底——但
	// 渲染器独立可用性要求它对空 map 稳健）。
	out := Render(&store.TaskCostSummary{TaskID: "t", Currency: "CNY", ByLevel: map[types.RungID]*store.LevelSummary{}, ByCategory: map[store.CostCategory]*store.LevelSummary{}}, nil)
	if out == "" {
		t.Fatal("empty render")
	}
}
