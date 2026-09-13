package config

// 阶段 11 的三块配置（Part 11.5）：limits（经济）、llm_wires（部署）、
// gate_rules（政策）。三者分离——项目 config.yaml 里各收一个块；
// 两级覆盖（全局默认 → 项目覆盖）由加载序决定（本包只提供"单文件解析 +
// 缺省物化"，全局级的默认常量在本文件的 Defaults）。
//
// 解析器复用内部 YAML 子集（本包的既有形态）；键序/未知键**拒绝**面：
// 未知块键直接失败（打错字 = 更改配置面，不能静默）。

import (
	"fmt"

	"github.com/RobiNexy/Marl/internal/gate"
)

// Limits 是经济性约束的汇总块（Part 11.5）。
//
// 零值契约：零值不可用——每个数字 0 都是"立即拒绝一切"的陷阱；由
// MaterializeDefaults 补默认值后校验（全部 > 0）。
type Limits struct {
	// llm_call
	LLMCallMaxInputTokens  int
	LLMCallMaxOutputTokens int
	CallTaskMax            int
	CallTaskMaxTokens      int
	LLMCallTimeoutMs       int64
	// orchestration
	CacheReviewPct float64 // 破坏率 ≥ 该值 → need_human（Part 11.4 两档）
	// standing orders
	StandingOrdersMaxTokens int
}

// DefaultLimits 是全局默认（品牌缺省值的物化面；Part 11.5 的字面数字）。
func DefaultLimits() Limits {
	return Limits{
		LLMCallMaxInputTokens:   8000,
		LLMCallMaxOutputTokens:  2000,
		CallTaskMax:             20,
		CallTaskMaxTokens:       100_000,
		LLMCallTimeoutMs:        120_000,
		CacheReviewPct:          10,
		StandingOrdersMaxTokens: 1000,
	}
}

// Validate 报告 limits 是否可用（任何一项 <= 0 加上去 就把 "上限" 语义旋回
// "无上限"；cacheReviewPct 的取值区间 (0,100]）。
func (l Limits) Validate() error {
	switch {
	case l.LLMCallMaxInputTokens <= 0:
		return fmt.Errorf("limits: llm_call.max_input_tokens 必须 > 0")
	case l.LLMCallMaxOutputTokens <= 0:
		return fmt.Errorf("limits: llm_call.max_output_tokens 必须 > 0")
	case l.CallTaskMax <= 0:
		return fmt.Errorf("limits: llm_call.task_max_calls 必须 > 0")
	case l.CallTaskMaxTokens <= 0:
		return fmt.Errorf("limits: llm_call.task_max_tokens 必须 > 0")
	case l.LLMCallTimeoutMs <= 0:
		return fmt.Errorf("limits: llm_call.timeout_ms 必须 > 0")
	case l.CacheReviewPct <= 0 || l.CacheReviewPct > 100:
		return fmt.Errorf("limits: orchestration.cache_review_pct 必须在 (0,100]")
	case l.StandingOrdersMaxTokens <= 0:
		return fmt.Errorf("limits: standing_orders_max_tokens 必须 > 0")
	}
	return nil
}

// WireConfig 是一条 llm wire 的声明（Part 11.2 §2.4；main 例外——
// main 是活引用，解析到当前阶梯绑定，不落这份声明）。
type WireConfig struct {
	Name     string
	Adapter  string // "deepseek"（openai_chat 系）；后续厂商接入时扩展
	Endpoint string
	Model    string
	Thinking string // "off" / 模型原生档位名（透传 Normalizer）
}

// GateRule 是 gate_rules 块的一行（对应 gate.Rule）。
type GateRule struct {
	ID     string
	Match  map[string]string
	Action gate.Action
	Reason string
}

// Limits/llm_wires 的块提取（从 .marl/config.yaml 的解析树出发）。

// ParseLimits 从根 Node 抽 "limits" 块（缺块 = 用 DefaultLimits——
// "没有 limits 配置"的语义 = 全默认，不是"没有限额"）。
func ParseLimits(root *Node) (Limits, error) {
	lim := DefaultLimits()
	blk := root.Get("limits")
	if blk == nil {
		if err := lim.Validate(); err != nil {
			return Limits{}, err
		}
		return lim, nil
	}
	if v, ok := blk.Get("llm_call").Get("max_input_tokens").Int(); ok {
		lim.LLMCallMaxInputTokens = v
	}
	if v, ok := blk.Get("llm_call").Get("max_output_tokens").Int(); ok {
		lim.LLMCallMaxOutputTokens = v
	}
	if v, ok := blk.Get("llm_call").Get("task_max_calls").Int(); ok {
		lim.CallTaskMax = v
	}
	if v, ok := blk.Get("llm_call").Get("task_max_tokens").Int(); ok {
		lim.CallTaskMaxTokens = v
	}
	if v, ok := blk.Get("llm_call").Get("timeout_ms").Int(); ok {
		lim.LLMCallTimeoutMs = int64(v)
	}
	if v, ok := blk.Get("orchestration").Get("cache_review_pct").Float(); ok {
		lim.CacheReviewPct = v
	}
	if v, ok := blk.Get("standing_orders_max_tokens").Int(); ok {
		lim.StandingOrdersMaxTokens = v
	}
	if err := lim.Validate(); err != nil {
		return Limits{}, err
	}
	return lim, nil
}

// ParseLlmWires 抽 "llm_wires" 块（Part 11.2 §2.4）。
//
// main 在此是**禁止显式声明的**（它是活引用，自动解析到当前阶梯绑定；
// 显式 main 声明会与阶梯漂移出两份"主配置真相"）。
// 失败：name 空 / 重复 / adapter 空 / endpoint 空 / model 空 / main 显式。
func ParseLlmWires(root *Node) (map[string]WireConfig, error) {
	out := map[string]WireConfig{}
	blk := root.Get("llm_wires")
	if blk == nil {
		return out, nil
	}
	if blk.Kind != KindList {
		return nil, fmt.Errorf("llm_wires: expected block list")
	}
	for i, it := range blk.Items {
		name := it.Get("name").StrOr("")
		if name == "" {
			return nil, fmt.Errorf("llm_wires[%d]: name 未填", i)
		}
		if name == "main" {
			return nil, fmt.Errorf("llm_wires[%d]: 名字 \"main\" 是活引用（当前阶梯绑定），不允许静态声明", i)
		}
		cfg := WireConfig{
			Name:     name,
			Adapter:  it.Get("adapter").StrOr(""),
			Endpoint: it.Get("endpoint").StrOr(""),
			Model:    it.Get("model").StrOr(""),
			Thinking: it.Get("thinking").StrOr(""),
		}
		if cfg.Adapter == "" || cfg.Endpoint == "" || cfg.Model == "" {
			return nil, fmt.Errorf("llm_wires[%d] (%s): adapter/endpoint/model 三者都必填", i, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("llm_wires: duplicate wire %q", name)
		}
		out[name] = cfg
	}
	return out, nil
}

// ParseGateRules 抽 "gate_rules" 块 → gate.Rule（顺序保留——首中生效）。
//
// 失败：条目缺 id/action / match.kind 缺失（Validate 的兜底在 gate.Manager
// 再过一遍——配置解析只管"字面合法"，语义合法性归 PDP）。
func ParseGateRules(root *Node) ([]gate.Rule, error) {
	blk := root.Get("gate_rules")
	if blk == nil {
		return nil, nil
	}
	if blk.Kind != KindList {
		return nil, fmt.Errorf("gate_rules: expected block list")
	}
	out := make([]gate.Rule, 0, len(blk.Items))
	for i, it := range blk.Items {
		id, ok := it.Get("id").Str()
		if !ok || id == "" {
			return nil, fmt.Errorf("gate_rules[%d]: id 未填", i)
		}
		action, ok := it.Get("action").Str()
		if !ok || action == "" {
			return nil, fmt.Errorf("gate_rules[%d] (%s): action 未填", i, id)
		}
		r := gate.Rule{ID: id, Action: gate.Action(action)}
		if rs := it.Get("reason"); rs != nil {
			r.Reason = rs.StrOr("")
		}
		m := it.Get("match")
		if m == nil || m.Kind != KindMap {
			return nil, fmt.Errorf("gate_rules[%d] (%s): match 块缺失（无属性规则 = 未知靶子）", i, id)
		}
		r.Match = map[string]string{}
		for j, k := range m.Keys {
			v, _ := m.Vals[j].Str()
			r.Match[k] = v
		}
		out = append(out, r)
	}
	return out, nil
}
