package ladder

// 阶梯配置的加载与校验（Part 7.1 / 10.4）。
//
// 配置文件是 YAML（ladder.yaml），由 internal/config 的受限子集解析器解析。
// 本文件做"解析树 → types.Ladder + 每档 thinking + 每模型分项计价"的翻译
// 与校验。

import (
	"fmt"
	"os"
	"sort"

	"github.com/RobiNexy/Marl/internal/config"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// Config 是 ladder.yaml 的内存形态。
//
// Thinking 与 Rung 平行而非并入 types.Rung 的理由：types.Rung 是纯排序/计价
// 数据（进 types 层的契约），而 ThinkingSpec 属于 Binding 组装素材（ADR-0022
// 的"唯一真相在阶梯"落在组装侧）。两者在 Load 时一起校验、一起消费，
// 分开存放不产生第二真相——Router 是唯一的组装点。
//
// Pricing 同理平行承载：它是**模型属性**（同一模型在任何档位同价——
// 档位只切 thinking，不切价格），因此按 model id 键控；StaticCatalog 把它
// 喂给 wire.Catalog.Pricing（Ledger 记账的权威来源）。
//
// 零值契约：Config{} 不可用（Ladder 为 nil）；必须经 Load 或显式构造 + Validate。
type Config struct {
	Ladder *types.Ladder
	// Thinking 是每档的思维配置（键 = RungID）。Load 保证每个 Rung 都有
	// 显式条目（缺档位声明会让 Normalizer 走"不指定"路径，实测默认档位是
	// high——账单与配置意图不符且无告警，见 ADR-0022）。
	Thinking map[types.RungID]types.ThinkingSpec
	// Pricing 是每模型的分项计价（键 = model id；Part 10.4 的四项形态：
	// 输入未命中 / 输入缓存命中 / 可见输出 / 思维链，全部每百万 token 单价）。
	// Load 保证阶梯里引用的每个 model 都有显式条目（缺价会让记账在第一次
	// 调用时失败——宁可启动期报错）。
	Pricing map[string]wire.Pricing
}

// IndexOf 返回 rungID 的下标；不存在返回 -1。
//
// 并发：纯函数（Config 视为构造后只读）。
func (c *Config) IndexOf(id types.RungID) int {
	for i, r := range c.Ladder.Rungs {
		if r.ID == id {
			return i
		}
	}
	return -1
}

// Validate 报告配置是否可用（Load 已内建校验；手工构造的 Config 走这里）。
//
// 失败：Ladder 为 nil / Ladder 校验不过（见 types.Ladder 不变量）/
// Thinking 缺档。
func (c *Config) Validate() error {
	if c == nil || c.Ladder == nil {
		return fmt.Errorf("ladder: config is nil")
	}
	if len(c.Ladder.Rungs) == 0 {
		return fmt.Errorf("ladder: no rungs")
	}
	seen := make(map[types.RungID]bool, len(c.Ladder.Rungs))
	for _, r := range c.Ladder.Rungs {
		switch {
		case r.ID == "":
			return fmt.Errorf("ladder: rung with empty id")
		case seen[r.ID]:
			return fmt.Errorf("ladder: duplicate rung id %q", r.ID)
		case r.Endpoint == "" || r.Model == "":
			return fmt.Errorf("ladder: rung %s: endpoint and model are required", r.ID)
		case r.Currency == "":
			return fmt.Errorf("ladder: rung %s: currency is required (cross-currency price comparison is invalid)", r.ID)
		}
		seen[r.ID] = true
		if _, ok := c.Thinking[r.ID]; !ok {
			return fmt.Errorf("ladder: rung %s: no thinking config (ADR-0022: the ladder is the only source of thinking levels)", r.ID)
		}
		p, ok := c.Pricing[r.Model]
		if !ok {
			return fmt.Errorf("ladder: rung %s: no pricing for model %q (pricing is per-model, see 'pricing:' section)", r.ID, r.Model)
		}
		if r.Currency != p.Currency {
			return fmt.Errorf("ladder: rung %s: currency %q differs from pricing currency %q (one model, one price table)", r.ID, r.Currency, p.Currency)
		}
		// CostPerMTok 是排序粗价，必须落在 Pricing 的可行区间内
		// [CachedIn, In+Out]（全缓存命中到全未命中+全输出的理论界）。
		// 越界说明两个价格表之一写错了——排序与记账的相对结论会相反。
		if r.CostPerMTok < p.CachedInPerMTok || r.CostPerMTok > p.InPerMTok+p.OutPerMTok {
			return fmt.Errorf("ladder: rung %s: cost_per_mtok %.2f outside pricing range [%.2f, %.2f] (sort price and billing price disagree)",
				r.ID, r.CostPerMTok, p.CachedInPerMTok, p.InPerMTok+p.OutPerMTok)
		}
	}
	// 从便宜到贵是非递减序（相等合法：同模型不同 thinking 档）。
	for i := 1; i < len(c.Ladder.Rungs); i++ {
		if c.Ladder.Rungs[i].CostPerMTok < c.Ladder.Rungs[i-1].CostPerMTok {
			return fmt.Errorf("ladder: rungs must be ordered cheap→expensive: %s (%.2f) < %s (%.2f)",
				c.Ladder.Rungs[i].ID, c.Ladder.Rungs[i].CostPerMTok,
				c.Ladder.Rungs[i-1].ID, c.Ladder.Rungs[i-1].CostPerMTok)
		}
		if c.Ladder.Rungs[i].Currency != c.Ladder.Rungs[0].Currency {
			return fmt.Errorf("ladder: mixed currencies in one ladder (%s vs %s)", c.Ladder.Rungs[i].Currency, c.Ladder.Rungs[0].Currency)
		}
	}
	if c.Ladder.Start != "" {
		if !seen[c.Ladder.Start] {
			return fmt.Errorf("ladder: start %q not found in rungs", c.Ladder.Start)
		}
	}
	return nil
}

// Load 从 YAML 文件加载阶梯配置。
//
// 文件形态（Part 7.1 的子集，见 internal/config 支持范围）：
//
//	ladder:
//	  - id: "r0"
//	    endpoint: "deepseek-main"
//	    model: "deepseek-flash"
//	    thinking: {level: "off"}     # 或块式 thinking: level: ...
//	    description: "..."
//	    cost_per_mtok: 1.0
//	    currency: "CNY"
//	start: "r0"                      # 可选；缺省由项目配置/调用方提供
//
// 失败：文件不可读、解析失败、校验不过（错误带行号或 rung id）。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ladder: read %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse 从字节加载阶梯配置（Load 的核心；测试与内嵌配置用）。
func Parse(raw []byte) (*Config, error) {
	root, err := config.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("ladder: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("ladder: empty config")
	}
	ladderNode := root.Get("ladder")
	if ladderNode == nil {
		return nil, fmt.Errorf("ladder: missing 'ladder' section")
	}
	cfg := &Config{
		Thinking: make(map[types.RungID]types.ThinkingSpec),
		Pricing:  make(map[string]wire.Pricing),
	}
	for i, item := range ladderNode.List() {
		r, th, err := rungFromNode(item, i)
		if err != nil {
			return nil, err
		}
		cfg.Ladder = ladderAppend(cfg.Ladder, r)
		cfg.Thinking[r.ID] = th
	}
	if start := root.Get("start"); start != nil {
		if s, ok := start.Str(); ok {
			cfg.Ladder.Start = types.RungID(s)
		}
	}
	// pricing 节：按 model id 的分项计价（Part 10.4 形态）。
	pn := root.Get("pricing")
	if pn == nil {
		return nil, fmt.Errorf("ladder: missing 'pricing' section (billing needs per-model input/cached/output/reasoning prices)")
	}
	for i, item := range pn.List() {
		if item == nil || item.Kind != config.KindMap {
			return nil, fmt.Errorf("ladder: pricing[%d] is not a mapping", i)
		}
		model, _ := item.Get("model").Str()
		if model == "" {
			return nil, fmt.Errorf("ladder: pricing[%d]: model is required", i)
		}
		p, err := pricingFromNode(item)
		if err != nil {
			return nil, fmt.Errorf("ladder: pricing %s: %w", model, err)
		}
		if _, dup := cfg.Pricing[model]; dup {
			return nil, fmt.Errorf("ladder: duplicate pricing for model %q", model)
		}
		cfg.Pricing[model] = p
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// pricingFromNode 翻译一个模型的计价条目。
//
// 失败：四项单价缺失（缓存命中免费也是显式写 0）、currency 缺失、
// cached_in > in（写反了会让路由把"缓存友好"算成"更贵"，见 wire.Pricing）。
func pricingFromNode(item *config.Node) (wire.Pricing, error) {
	in, hasIn := item.Get("in_per_mtok").Float()
	cached, hasCached := item.Get("cached_in_per_mtok").Float()
	out, hasOut := item.Get("out_per_mtok").Float()
	reasoning, hasReasoning := item.Get("reasoning_per_mtok").Float()
	currency, _ := item.Get("currency").Str()
	p := wire.Pricing{
		InPerMTok: in, CachedInPerMTok: cached,
		OutPerMTok: out, ReasoningPerMTok: reasoning,
		Currency: currency,
	}
	switch {
	case !hasIn || !hasCached || !hasOut || !hasReasoning:
		return wire.Pricing{}, fmt.Errorf("all four unit prices are required (write 0.0 for free tiers; got in=%v cached=%v out=%v reasoning=%v)",
			hasIn, hasCached, hasOut, hasReasoning)
	case currency == "":
		return wire.Pricing{}, fmt.Errorf("currency is required")
	case cached > in:
		return wire.Pricing{}, fmt.Errorf("cached_in_per_mtok %.2f > in_per_mtok %.2f (cache hit must not cost more than a miss)", cached, in)
	case in < 0 || cached < 0 || out < 0 || reasoning < 0:
		return wire.Pricing{}, fmt.Errorf("unit prices must be >= 0")
	}
	return p, nil
}

// ladderAppend 向 Ladder 追加一档（Ladder 零值合法的空壳，这里负责初始化）。
func ladderAppend(l *types.Ladder, r types.Rung) *types.Ladder {
	if l == nil {
		l = &types.Ladder{}
	}
	l.Rungs = append(l.Rungs, r)
	return l
}

// rungFromNode 把一个列表项翻译成 Rung + Thinking。
//
// 失败：id/endpoint/model 缺失、thinking.level 缺失、cost 非法。
func rungFromNode(item *config.Node, idx int) (types.Rung, types.ThinkingSpec, error) {
	if item == nil || item.Kind != config.KindMap {
		return types.Rung{}, types.ThinkingSpec{}, fmt.Errorf("ladder: rung[%d] is not a mapping", idx)
	}
	id, _ := item.Get("id").Str()
	endpoint, _ := item.Get("endpoint").Str()
	model, _ := item.Get("model").Str()
	desc, _ := item.Get("description").Str()
	currency, _ := item.Get("currency").Str()
	cost, hasCost := item.Get("cost_per_mtok").Float()
	r := types.Rung{
		ID:          types.RungID(id),
		Description: desc,
		Endpoint:    endpoint,
		Model:       model,
		CostPerMTok: cost,
		Currency:    currency,
	}
	th, err := thinkingFromNode(item.Get("thinking"))
	if err != nil {
		return types.Rung{}, types.ThinkingSpec{}, fmt.Errorf("ladder: rung[%d] (%s): %w", idx, id, err)
	}
	switch {
	case r.ID == "":
		return types.Rung{}, types.ThinkingSpec{}, fmt.Errorf("ladder: rung[%d]: id is required", idx)
	case r.Endpoint == "" || r.Model == "":
		return types.Rung{}, types.ThinkingSpec{}, fmt.Errorf("ladder: rung %s: endpoint and model are required", r.ID)
	case !hasCost:
		// 价格直接决定排序与升级决策；零值无法区分"免费"与"忘了填"。
		// 免费模型应显式写 0.0。
		return types.Rung{}, types.ThinkingSpec{}, fmt.Errorf("ladder: rung %s: cost_per_mtok is required (write 0.0 for free models)", r.ID)
	case r.Currency == "":
		return types.Rung{}, types.ThinkingSpec{}, fmt.Errorf("ladder: rung %s: currency is required", r.ID)
	}
	return r, th, nil
}

// thinkingFromNode 翻译 thinking 配置（行内 {level: ...} 或块式）。
//
// 失败：节点缺失（每档必须显式声明档位——"不指定"会让厂商走默认档位，
// 实测默认是 high，账单与意图不符且无告警，ADR-0022）。
func thinkingFromNode(n *config.Node) (types.ThinkingSpec, error) {
	if n == nil {
		return types.ThinkingSpec{}, fmt.Errorf("thinking config is required per rung")
	}
	level, _ := n.Get("level").Str()
	if level == "" {
		return types.ThinkingSpec{}, fmt.Errorf("thinking.level is required")
	}
	th := types.ThinkingSpec{Level: level}
	if b := n.Get("budget"); b != nil {
		if v, ok := b.Int(); ok {
			budget := v
			th.Budget = &budget
		} else {
			return types.ThinkingSpec{}, fmt.Errorf("thinking.budget %q is not an integer", b.Scalar)
		}
	}
	return th, nil
}

// SortRungs 按 cost 升序返回档位 id（报表/测试辅助；不改动 Config 本体）。
//
// 并发：纯函数。
func SortRungs(c *Config) []types.RungID {
	out := make([]types.RungID, 0, len(c.Ladder.Rungs))
	for _, r := range c.Ladder.Rungs {
		out = append(out, r.ID)
	}
	sort.Slice(out, func(i, j int) bool {
		return c.Ladder.Rungs[c.IndexOf(out[i])].CostPerMTok < c.Ladder.Rungs[c.IndexOf(out[j])].CostPerMTok
	})
	return out
}
