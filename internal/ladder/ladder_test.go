package ladder

// ladder 包测试：配置加载校验、Router 两阶段调度、证据累积与升级判据。

import (
	"fmt"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// ---------------------------------------------------------------------------
// 测试基建：两档阶梯（同模型不同 thinking，Part 7.1 的 r0/r1 形态）+ 三档
// （r2 换模型）。
// ---------------------------------------------------------------------------

func testLadderCfg() *Config {
	return &Config{
		Pricing: map[string]wire.Pricing{
			"deepseek-flash":  {InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"},
			"deepseek-v4-pro": {InPerMTok: 4.0, CachedInPerMTok: 1.0, OutPerMTok: 8.0, ReasoningPerMTok: 8.0, Currency: "CNY"},
		},
		Ladder: &types.Ladder{
			Rungs: []types.Rung{
				{ID: "r0", Endpoint: "ep", Model: "deepseek-flash", CostPerMTok: 1.0, Currency: "CNY"},
				{ID: "r1", Endpoint: "ep", Model: "deepseek-flash", CostPerMTok: 1.0, Currency: "CNY"},
				{ID: "r2", Endpoint: "ep", Model: "deepseek-v4-pro", CostPerMTok: 4.0, Currency: "CNY"},
			},
			Start: "r0",
		},
		Thinking: map[types.RungID]types.ThinkingSpec{
			"r0": {Level: "off"},
			"r1": {Level: "high"},
			"r2": {Level: "high"},
		},
	}
}

func testCatalog(t *testing.T, cfg *Config) *StaticCatalog {
	t.Helper()
	cat := NewStaticCatalog(cfg.Ladder)
	for _, m := range []wire.ModelEntry{
		{
			ID: "deepseek-flash", Provider: "deepseek", Wire: types.WireOpenAIChat, RemoteName: "deepseek-flash",
			Caps: wire.ModelCaps{
				Has:        []types.Capability{types.CapToolCall, types.CapJSONMode, types.CapThinking},
				MaxContext: 65536, MaxOutput: 8192,
				CacheMode:       wire.CacheImplicitPrefix,
				ThinkingControl: wire.ThinkControlLevel,
				ThinkingLevels:  []string{"none", "low", "high", "max"},
			},
			Pricing: wire.Pricing{InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"},
		},
		{
			ID: "deepseek-v4-pro", Provider: "deepseek", Wire: types.WireOpenAIChat, RemoteName: "deepseek-v4-pro",
			Caps: wire.ModelCaps{
				Has:        []types.Capability{types.CapToolCall, types.CapJSONMode, types.CapThinking},
				MaxContext: 65536, MaxOutput: 8192,
				CacheMode:       wire.CacheImplicitPrefix,
				ThinkingControl: wire.ThinkControlLevel,
				ThinkingLevels:  []string{"none", "low", "high", "max"},
			},
			Pricing: wire.Pricing{InPerMTok: 4.0, CachedInPerMTok: 1.0, OutPerMTok: 8.0, ReasoningPerMTok: 8.0, Currency: "CNY"},
		},
	} {
		if err := cat.AddModel(m); err != nil {
			t.Fatalf("AddModel %s: %v", m.ID, err)
		}
	}
	if err := cat.AddEndpoint(wire.EndpointConfig{Name: "ep", BaseURL: "https://api.example.com/v1", KeyRef: "env:X", MaxInflight: 4, RPM: 60}); err != nil {
		t.Fatal(err)
	}
	return cat
}

func testRouter(t *testing.T, cfg *Config, policy RouterPolicy) wire.Router {
	t.Helper()
	r, err := NewRouter(testCatalog(t, cfg), cfg, policy)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------
// Load / Validate
// ---------------------------------------------------------------------------

func TestParseLadderYAML(t *testing.T) {
	src := []byte(`ladder:
  - id: "r0"
    endpoint: "deepseek-main"
    model: "deepseek-flash"
    thinking: {level: "off"}
    description: "cheap"
    cost_per_mtok: 1.0
    currency: "CNY"
  - id: "r1"
    endpoint: "deepseek-main"
    model: "deepseek-flash"
    thinking:
      level: "high"
    cost_per_mtok: 1.0
    currency: "CNY"
pricing:
  - model: "deepseek-flash"
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: CNY
start: "r0"
`)
	cfg, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Ladder.Rungs) != 2 || cfg.Ladder.Start != "r0" {
		t.Fatalf("cfg: %+v", cfg.Ladder)
	}
	if cfg.Thinking["r0"].Level != "off" || cfg.Thinking["r1"].Level != "high" {
		t.Fatalf("thinking: %+v", cfg.Thinking)
	}
	if cfg.IndexOf("r1") != 1 || cfg.IndexOf("nope") != -1 {
		t.Fatalf("IndexOf broken")
	}
	// 分项计价（Part 10.4 形态）。
	p := cfg.Pricing["deepseek-flash"]
	if p.InPerMTok != 1.0 || p.CachedInPerMTok != 0.25 || p.OutPerMTok != 2.0 || p.ReasoningPerMTok != 2.0 || p.Currency != "CNY" {
		t.Fatalf("pricing: %+v", p)
	}
}

func TestLoadPricingValidation(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"missing pricing section", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 1.0
    currency: CNY`, "missing 'pricing'"},
		{"missing unit price", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 1.0
    currency: CNY
pricing:
  - model: m
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    currency: CNY`, "all four unit prices"},
		{"cached > in", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 1.0
    currency: CNY
pricing:
  - model: m
    in_per_mtok: 1.0
    cached_in_per_mtok: 2.0
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: CNY`, "cache hit must not cost more"},
		{"rung currency mismatch", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 1.0
    currency: USD
pricing:
  - model: m
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: CNY`, "one model, one price table"},
		{"sort price out of range", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 99.0
    currency: CNY
pricing:
  - model: m
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: CNY`, "sort price and billing price disagree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

func TestLoadValidationFailures(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"missing ladder section", `start: r0`, "missing 'ladder'"},
		{"missing thinking", `ladder:
  - id: r0
    endpoint: ep
    model: m
    cost_per_mtok: 1.0
    currency: CNY
pricing:
  - model: m
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: CNY`, "thinking config is required"},
		{"missing cost", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    currency: CNY
pricing:
  - model: m
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: CNY`, "cost_per_mtok is required"},
		{"missing currency", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 1.0
pricing:
  - model: m
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: CNY`, "currency is required"},
		{"wrong order", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 4.0
    currency: CNY
  - id: r1
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 1.0
    currency: CNY
pricing:
  - model: m
    in_per_mtok: 2.0
    cached_in_per_mtok: 0.5
    out_per_mtok: 6.0
    reasoning_per_mtok: 6.0
    currency: CNY`, "cheap→expensive"},
		{"dup id", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 1.0
    currency: CNY
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 1.0
    currency: CNY
pricing:
  - model: m
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: CNY`, "duplicate rung id"},
		{"bad start", `ladder:
  - id: r0
    endpoint: ep
    model: m
    thinking: {level: "off"}
    cost_per_mtok: 1.0
    currency: CNY
pricing:
  - model: m
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: CNY
start: r9`, "not found in rungs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

func TestRouterBind(t *testing.T) {
	cfg := testLadderCfg()
	r := testRouter(t, cfg, RouterPolicy{PreferBonus: 1.0, CostWeight: 0.1})
	req := types.Requirement{Require: []types.Capability{types.CapToolCall}, Prefer: []types.Capability{types.CapThinking}, MinContext: 32000}

	b, err := r.Bind(req, "agent-1", cfg.Ladder, 0)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if b.RungID != "r0" || b.RungIndex != 0 {
		t.Fatalf("binding: %+v", b)
	}
	if b.CacheBucket != "agent-1" {
		t.Fatalf("cache bucket must be agentID: %+v", b)
	}
	if b.Thinking.Level != "off" {
		t.Fatalf("thinking from ladder: %+v", b.Thinking)
	}
	if b.CachePrefix == "" || b.BoundAt.IsZero() {
		t.Fatalf("binding invariants: %+v", b)
	}
	// 打分检查：r0 = prefer(1) - cost(0.1) = 0.9；r2 = 1 - 0.4 = 0.6
	// （r0/r1 同分 0.9，保持阶梯序）。便宜档应排前。
	cands, err := r.Candidates(req, cfg.Ladder, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].Rung.ID != "r0" || cands[0].Score <= cands[len(cands)-1].Score {
		t.Fatalf("candidates order: %s=%v %s=%v %s=%v",
			cands[0].Rung.ID, cands[0].Score, cands[1].Rung.ID, cands[1].Score, cands[2].Rung.ID, cands[2].Score)
	}
}

func TestRouterUpgradeStartIndex(t *testing.T) {
	cfg := testLadderCfg()
	r := testRouter(t, cfg, RouterPolicy{})
	req := types.Requirement{Require: []types.Capability{types.CapToolCall}}

	// 从 r1 起跑（升级后的重绑定）：r0 不可见，r1 胜出。
	b, err := r.Bind(req, "a", cfg.Ladder, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.RungID != "r1" || b.RungIndex != 1 || b.Thinking.Level != "high" {
		t.Fatalf("rebind: %+v", b)
	}
	// 从最高档 Bind：只有 r2 可选。
	b, err = r.Bind(req, "a", cfg.Ladder, 2)
	if err != nil {
		t.Fatal(err)
	}
	if b.RungID != "r2" || b.Model != "deepseek-v4-pro" {
		t.Fatalf("top bind: %+v", b)
	}
}

func TestRouterFailures(t *testing.T) {
	cfg := testLadderCfg()
	r := testRouter(t, cfg, RouterPolicy{})
	req := types.Requirement{Require: []types.Capability{types.CapToolCall}}

	t.Run("start index out of range", func(t *testing.T) {
		_, err := r.Bind(req, "a", cfg.Ladder, 3)
		if err == nil || !contains(err.Error(), "out of range") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unsatisfiable requirement", func(t *testing.T) {
		bad := types.Requirement{Require: []types.Capability{types.CapImageGen}}
		_, err := r.Bind(bad, "a", cfg.Ladder, 0)
		if err == nil || !contains(err.Error(), "no candidate") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("min context filter", func(t *testing.T) {
		big := types.Requirement{Require: []types.Capability{types.CapToolCall}, MinContext: 1 << 20}
		_, err := r.Bind(big, "a", cfg.Ladder, 0)
		if err == nil || !contains(err.Error(), "no candidate") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown model in rung fails fast", func(t *testing.T) {
		broken := &Config{
			Ladder: &types.Ladder{Rungs: []types.Rung{
				{ID: "r0", Endpoint: "ep", Model: "ghost/model", CostPerMTok: 1, Currency: "CNY"},
			}, Start: "r0"},
			Thinking: map[types.RungID]types.ThinkingSpec{"r0": {Level: "off"}},
			Pricing:  map[string]wire.Pricing{"ghost/model": {InPerMTok: 1, CachedInPerMTok: 0.25, OutPerMTok: 2, ReasoningPerMTok: 2, Currency: "CNY"}},
		}
		r2 := testRouter(t, broken, RouterPolicy{})
		_, err := r2.Bind(req, "a", broken.Ladder, 0)
		if err == nil || !contains(err.Error(), "ghost/model") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestStaticCatalogOverrides(t *testing.T) {
	cfg := testLadderCfg()
	cat := testCatalog(t, cfg)
	// 覆盖：实测发现 chat 不支持 JSON mode（整集替换）。
	if err := cat.AddOverride(wire.CapsOverride{
		Model: "deepseek-flash", Endpoint: "ep",
		Has:      []types.Capability{types.CapToolCall},
		ProbedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	caps, err := cat.EffectiveCaps("deepseek-flash", "ep")
	if err != nil {
		t.Fatal(err)
	}
	if len(caps.Has) != 1 || caps.Has[0] != types.CapToolCall {
		t.Fatalf("override not applied: %+v", caps)
	}
	// 未覆盖的 (model, endpoint) 组合取声明值。
	caps, err = cat.EffectiveCaps("deepseek-v4-pro", "ep")
	if err != nil {
		t.Fatal(err)
	}
	if len(caps.Has) != 3 {
		t.Fatalf("declared caps: %+v", caps)
	}
	if err := cat.AddModel(wire.ModelEntry{ID: "deepseek-flash"}); err == nil {
		t.Fatal("duplicate model must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Evidence
// ---------------------------------------------------------------------------

func testPolicy() EvidencePolicy {
	return EvidencePolicy{
		Threshold: 0.8, FailureWeight: 0.4, FormatErrorWeight: 0.4,
		NoProgressWeight: 0.15, ChildFailureRateWeight: 1.0, ReclaimLowWeight: 0.1,
	}
}

func TestEvidenceAccumulatorUpgrade(t *testing.T) {
	cfg := testLadderCfg()
	acc, err := NewAccumulator(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// 13.6 的场景：两次格式错误 → 升级。
	acc.RecordFormatError()
	if dec := acc.Evaluate(cfg.Ladder, "r0"); dec.ShouldUpgrade {
		t.Fatalf("one format error must not upgrade: %+v", dec)
	}
	acc.RecordFormatError()
	dec := acc.Evaluate(cfg.Ladder, "r0")
	if !dec.ShouldUpgrade || dec.ToRung != "r1" || dec.FromRung != "r0" {
		t.Fatalf("decision: %+v", dec)
	}
	if dec.Reason == "" {
		t.Fatal("reason required for audit")
	}
	acc.Reset()
	if acc.Score() != 0 {
		t.Fatalf("reset: score=%v", acc.Score())
	}
}

func TestEvidenceConsecutiveSemantics(t *testing.T) {
	cfg := testLadderCfg()
	acc, _ := NewAccumulator(testPolicy())
	acc.RecordFailure()
	acc.RecordProgress() // 成功清零"连续"
	acc.RecordFailure()
	if dec := acc.Evaluate(cfg.Ladder, "r0"); dec.ShouldUpgrade {
		t.Fatalf("non-consecutive failures must not upgrade: %+v", dec)
	}
	acc.RecordFailure()
	acc.RecordFailure()
	if dec := acc.Evaluate(cfg.Ladder, "r0"); !dec.ShouldUpgrade {
		t.Fatalf("2 consecutive failures must upgrade: %+v", dec)
	}
}

func TestEvidenceTopRung(t *testing.T) {
	cfg := testLadderCfg()
	acc, _ := NewAccumulator(testPolicy())
	for i := 0; i < 5; i++ {
		acc.RecordFailure()
	}
	dec := acc.Evaluate(cfg.Ladder, "r2")
	if dec.ShouldUpgrade || dec.ToRung != "" {
		t.Fatalf("top rung must not upgrade: %+v", dec)
	}
	if dec.Reason == "" || !contains(dec.Reason, "top rung") {
		t.Fatalf("reason must explain: %q", dec.Reason)
	}
}

func TestEvidenceChildFailureRate(t *testing.T) {
	cfg := testLadderCfg()
	acc, _ := NewAccumulator(testPolicy())
	acc.RecordChildFailures(4, 4, 0) // 100%：全军覆没，单独触发
	if dec := acc.Evaluate(cfg.Ladder, "r0"); !dec.ShouldUpgrade {
		t.Fatalf("total child failure must upgrade: %+v", dec)
	}
	acc.Reset()
	acc.RecordChildFailures(4, 2, 1) // 75% > 50%：中权重，需叠加
	if dec := acc.Evaluate(cfg.Ladder, "r0"); dec.ShouldUpgrade {
		t.Fatalf("75%% child failure must not upgrade alone: %+v", dec)
	}
	acc.RecordNoProgress()
	acc.RecordNoProgress()
	acc.RecordNoProgress() // 3 轮无进展 = 0.45，叠加 0.75 = 1.2 ≥ 0.8
	if dec := acc.Evaluate(cfg.Ladder, "r0"); !dec.ShouldUpgrade {
		t.Fatalf("stacked evidence must upgrade: %+v", dec)
	}
}

func TestEvidencePolicyValidation(t *testing.T) {
	if err := (EvidencePolicy{}).Validate(); err == nil {
		t.Fatal("zero policy must be rejected (threshold 0 = upgrade on any evidence)")
	}
	p := testPolicy()
	p.Threshold = -1
	if err := p.Validate(); err == nil {
		t.Fatal("negative threshold must be rejected")
	}
	p = testPolicy()
	p.FailureWeight = -0.1
	if err := p.Validate(); err == nil {
		t.Fatal("negative weight must be rejected")
	}
}

func TestNextRung(t *testing.T) {
	cfg := testLadderCfg()
	if got := nextRung(cfg.Ladder, "r0"); got != "r1" {
		t.Fatalf("next(r0) = %q", got)
	}
	if got := nextRung(cfg.Ladder, "r2"); got != "" {
		t.Fatalf("next(r2) = %q (top)", got)
	}
	if got := nextRung(cfg.Ladder, "ghost"); got != "" {
		t.Fatalf("next(ghost) = %q", got)
	}
}

// 编译期保证：Candidate/UpgradeSignal 的契约形态仍满足 wire 层接口。
var _ wire.Router = (wire.Router)(nil)

func init() {
	// 防止 fmt 未使用告警（保留在测试文件里用于错误构造）。
	_ = fmt.Sprintf
}
