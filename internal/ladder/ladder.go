package ladder

// 阶梯配置的加载与校验（Part 7.1）。
//
// 配置文件是 YAML（ladder.yaml），由 internal/config 的受限子集解析器解析。
// 本文件只做"解析树 → types.Ladder + 每档 thinking"的翻译与校验。

import (
	"fmt"
	"os"
	"sort"

	"marl/internal/config"
	"marl/internal/types"
)

// Config 是 ladder.yaml 的内存形态。
//
// Thinking 与 Rung 平行而非并入 types.Rung 的理由：types.Rung 是纯排序/计价
// 数据（进 types 层的契约），而 ThinkingSpec 属于 Binding 组装素材（ADR-0022
// 的"唯一真相在阶梯"落在组装侧）。两者在 Load 时一起校验、一起消费，
// 分开存放不产生第二真相——Router 是唯一的组装点。
//
// 零值契约：Config{} 不可用（Ladder 为 nil）；必须经 Load 或显式构造 + Validate。
type Config struct {
	Ladder *types.Ladder
	// Thinking 是每档的思维配置（键 = RungID）。Load 保证每个 Rung 都有
	// 显式条目（缺档位声明会让 Normalizer 走"不指定"路径，实测默认档位是
	// high——账单与配置意图不符且无告警，见 ADR-0022）。
	Thinking map[types.RungID]types.ThinkingSpec
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
//	    model: "deepseek/chat"
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
	cfg := &Config{Thinking: make(map[types.RungID]types.ThinkingSpec)}
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
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
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
