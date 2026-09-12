package ladder

// 两阶段调度 Router（Part 10.7 / 7.3）与静态模型目录（Part 10.4/10.5）。
//
// 阶段 4 的目录是**代码内注册的静态目录**（models.yaml 的配置层在阶段 7），
// 注册项与阶段 1 探测的实测结论一致（thinking 四档、隐式前缀缓存、
// 跨模型缓存隔离）。计价标注为占位值——权威价格在配置层落地前以构造参数
// 注入，报表的绝对金额在价格确认前只做相对比较。

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"marl/internal/types"
	"marl/internal/wire"
)

// StaticCatalog 是 wire.Catalog 的内存实现（阶段 4：代码注册，阶段 7 换配置加载）。
//
// 并发：注册只在启动期（单 goroutine）；查询可并发（RWMutex 分隔）。
type StaticCatalog struct {
	mu        sync.RWMutex
	models    map[string]*wire.ModelEntry
	endpoints map[string]*wire.EndpointConfig
	ladder    *types.Ladder
	overrides map[string]*wire.CapsOverride // key = model + "@" + endpoint
}

// NewStaticCatalog 构造空目录（ladder 随 SetLadder 注入或构造时给出）。
func NewStaticCatalog(ladder *types.Ladder) *StaticCatalog {
	return &StaticCatalog{
		models:    map[string]*wire.ModelEntry{},
		endpoints: map[string]*wire.EndpointConfig{},
		ladder:    ladder,
		overrides: map[string]*wire.CapsOverride{},
	}
}

// AddModel 注册模型（重名即启动期错误——重复注册是装配 bug，静默覆盖会让
// 两个"同名不同能力"的条目谁生效变成谜）。
func (c *StaticCatalog) AddModel(e wire.ModelEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.ID == "" || e.RemoteName == "" || !e.Wire.Valid() {
		return fmt.Errorf("catalog: model entry incomplete (id=%q remote=%q wire=%q)", e.ID, e.RemoteName, e.Wire)
	}
	if _, dup := c.models[e.ID]; dup {
		return fmt.Errorf("catalog: duplicate model %q", e.ID)
	}
	c.models[e.ID] = &e
	return nil
}

// AddEndpoint 注册接入点。
func (c *StaticCatalog) AddEndpoint(e wire.EndpointConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.Name == "" || e.BaseURL == "" || e.KeyRef == "" {
		return fmt.Errorf("catalog: endpoint incomplete (name=%q url=%q keyref=%q)", e.Name, e.BaseURL, e.KeyRef)
	}
	if e.MaxInflight <= 0 || e.RPM <= 0 {
		return fmt.Errorf("catalog: endpoint %s: MaxInflight/RPM must be positive", e.Name)
	}
	if _, dup := c.endpoints[e.Name]; dup {
		return fmt.Errorf("catalog: duplicate endpoint %q", e.Name)
	}
	c.endpoints[e.Name] = &e
	return nil
}

// AddOverride 注册能力实测覆盖（整集替换语义，见 wire.CapsOverride）。
func (c *StaticCatalog) AddOverride(o wire.CapsOverride) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if o.Model == "" || o.Endpoint == "" || o.ProbedAt.IsZero() {
		return fmt.Errorf("catalog: override incomplete (model=%q endpoint=%q probed_at zero=%v)", o.Model, o.Endpoint, o.ProbedAt.IsZero())
	}
	for _, cap := range o.Has {
		if !cap.Valid() {
			return fmt.Errorf("catalog: override %s@%s: invalid capability %q", o.Model, o.Endpoint, cap)
		}
	}
	c.overrides[o.Model+"@"+o.Endpoint] = &o
	return nil
}

// Model 实现 wire.Catalog。
func (c *StaticCatalog) Model(id string) (*wire.ModelEntry, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.models[id]
	if !ok {
		return nil, fmt.Errorf("catalog: model %q not found", id)
	}
	return m, nil
}

// Endpoint 实现 wire.Catalog。
func (c *StaticCatalog) Endpoint(name string) (*wire.EndpointConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.endpoints[name]
	if !ok {
		return nil, fmt.Errorf("catalog: endpoint %q not found", name)
	}
	return e, nil
}

// Ladder 实现 wire.Catalog。
func (c *StaticCatalog) Ladder() *types.Ladder { return c.ladder }

// EffectiveCaps 实现 wire.Catalog（覆盖存在时 Has 整集替换，其余取声明）。
func (c *StaticCatalog) EffectiveCaps(modelID, endpoint string) (wire.ModelCaps, error) {
	m, err := c.Model(modelID)
	if err != nil {
		return wire.ModelCaps{}, err
	}
	c.mu.RLock()
	ov := c.overrides[modelID+"@"+endpoint]
	c.mu.RUnlock()
	if ov != nil {
		caps := m.Caps
		caps.Has = append([]types.Capability(nil), ov.Has...)
		return caps, nil
	}
	return m.Caps, nil
}

// Pricing 实现 wire.Catalog。
func (c *StaticCatalog) Pricing(modelID, endpoint string) (wire.Pricing, error) {
	m, err := c.Model(modelID)
	if err != nil {
		return wire.Pricing{}, err
	}
	return m.Pricing, nil
}

// Health 实现 wire.Pool 的健康查询口径（阶段 4 不做熔断：恒返回"未探测"）。
// Router 谓词读到零值 HealthStatus 时按"先探测/按配置放行"处理——本实现
// 显式放行并注明，避免把"没探测"伪装成"确认健康"。
func (c *StaticCatalog) Health(_ context.Context, endpoint string) wire.HealthState {
	return wire.HealthState{}
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// RouterPolicy 是打分的可调参数（Part 10.7 阶段 2）。
//
// 零值契约：零值 RouterPolicy{} **可用**（全部权重取默认 0 = 纯谓词过滤、
// 按阶梯序取第一个）——与 CompressionPolicy 的"危险零值"不同，这里的零值
// 方向是保守的（少加分 = 更倾向便宜档）。非零值由配置层显式给出。
type RouterPolicy struct {
	// PreferBonus 是每个满足的软约束加分。
	PreferBonus float64
	// CostWeight 是成本减分系数（乘 rung.CostPerMTok）。
	CostWeight float64
	// AffinityBonus 是与"当前绑定同 model"的加分（缓存亲和）。
	AffinityBonus float64
}

// router 是 wire.Router 的实现。
type router struct {
	catalog *StaticCatalog
	cfg     *Config
	policy  RouterPolicy
	now     func() time.Time // 测试注入点；nil 用 time.Now
}

// NewRouter 构造 Router。
//
// 失败：catalog/cfg 为 nil、cfg 校验不过（启动期 fail fast）。
func NewRouter(catalog *StaticCatalog, cfg *Config, policy RouterPolicy) (*router, error) {
	if catalog == nil {
		return nil, fmt.Errorf("ladder: catalog is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &router{catalog: catalog, cfg: cfg, policy: policy, now: time.Now}, nil
}

// Bind 实现 wire.Router（契约见接口注释）。
//
// 阶段 4 的调度语义：
//   - 候选从 startIndex 起取（升级单调不回退 = 亲和性的机制本身）；
//   - 谓词过滤：Require 能力 ⊆ 模型能力、MinContext ≤ 窗口
//     （健康谓词留空——"不做"清单）；
//   - 打分：Prefer 加分 − 成本减分 + 亲和加分；同分保持阶梯序（便宜的在前）；
//   - 取首个候选组装 Binding；CacheBucket = agentID（ADR-0007）。
func (r *router) Bind(req types.Requirement, agentID types.AgentID, ladder *types.Ladder, startIndex int) (types.Binding, error) {
	cands, err := r.Candidates(req, ladder, startIndex)
	if err != nil {
		return types.Binding{}, err
	}
	best := cands[0]
	th, ok := r.cfg.Thinking[best.Rung.ID]
	if !ok {
		return types.Binding{}, fmt.Errorf("ladder: rung %s has no thinking config (ADR-0022)", best.Rung.ID)
	}
	now := r.now
	if now == nil {
		now = time.Now
	}
	return types.Binding{
		RungID:      best.Rung.ID,
		RungIndex:   indexOfRung(ladder, best.Rung.ID), // 绝对下标（候选本就从 startIndex 起取）
		Endpoint:    best.Rung.Endpoint,
		Model:       best.Rung.Model,
		CacheBucket: agentID,
		Wire:        best.Model.Wire,
		Thinking:    th,
		Degraded:    best.Degraded,
		BoundAt:     now(),
		CachePrefix: wire.ModelCachePrefix(best.Rung.Model, best.Rung.Endpoint),
	}, nil
}

// Candidates 实现 wire.Router。
//
// 失败：startIndex 越界（调用方 bug，不 clamp——clamp 会把"从第 5 档开始"
// 静默变成"从第 0 档开始"，即模型降级而不自知）；rung 引用未注册模型
// （配置错误 fail fast，带 rung id）；全部候选被过滤（错误说明是哪条约束）。
func (r *router) Candidates(req types.Requirement, ladder *types.Ladder, fromIndex int) ([]*wire.Candidate, error) {
	if ladder == nil || len(ladder.Rungs) == 0 {
		return nil, fmt.Errorf("ladder: empty ladder")
	}
	if fromIndex < 0 || fromIndex >= len(ladder.Rungs) {
		return nil, fmt.Errorf("ladder: start index %d out of range [0,%d) (refusing to clamp: clamping silently downgrades the model)", fromIndex, len(ladder.Rungs))
	}
	var out []*wire.Candidate
	var rejected []string
	for i := fromIndex; i < len(ladder.Rungs); i++ {
		rung := ladder.Rungs[i]
		entry, err := r.catalog.Model(rung.Model)
		if err != nil {
			return nil, fmt.Errorf("ladder: rung %s: %w", rung.ID, err)
		}
		if _, err := r.catalog.Endpoint(rung.Endpoint); err != nil {
			return nil, fmt.Errorf("ladder: rung %s: %w", rung.ID, err)
		}
		caps, err := r.catalog.EffectiveCaps(rung.Model, rung.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("ladder: rung %s: %w", rung.ID, err)
		}
		// 阶段 1 谓词过滤（硬约束）。
		if err := hardConstraint(req, caps); err != nil {
			rejected = append(rejected, fmt.Sprintf("%s: %v", rung.ID, err))
			continue
		}
		// 阶段 2 打分（软约束 + 成本）。
		score := 0.0
		var degraded []types.Capability
		has := make(map[types.Capability]bool, len(caps.Has))
		for _, c := range caps.Has {
			has[c] = true
		}
		for _, p := range req.Prefer {
			if has[p] {
				score += r.policy.PreferBonus
			} else {
				degraded = append(degraded, p)
			}
		}
		score -= rung.CostPerMTok * r.policy.CostWeight
		out = append(out, &wire.Candidate{
			Rung:     rung,
			Endpoint: mustEndpoint(r.catalog, rung.Endpoint),
			Model:    entry,
			Score:    score,
			Degraded: degraded,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ladder: no candidate satisfies the requirement (rejected: %s)",
			joinReasons(rejected))
	}
	// 稳定排序：同分保持阶梯序（便宜的在前）。
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out, nil
}

// hardConstraint 是阶段 1 的谓词过滤（硬约束不满足即出局，不降级）。
func hardConstraint(req types.Requirement, caps wire.ModelCaps) error {
	has := make(map[types.Capability]bool, len(caps.Has))
	for _, c := range caps.Has {
		has[c] = true
	}
	for _, need := range req.Require {
		if !has[need] {
			return fmt.Errorf("capability %q required but model lacks it", need)
		}
	}
	if req.MinContext > 0 && caps.MaxContext < req.MinContext {
		return fmt.Errorf("context %d < required %d", caps.MaxContext, req.MinContext)
	}
	return nil
}

// indexOfRung 返回 rungID 在 ladder 里的下标（不存在返回 0——Candidates
// 已保证候选来自该 ladder，这里只是还原相对位置）。
func indexOfRung(ladder *types.Ladder, id types.RungID) int {
	for i, r := range ladder.Rungs {
		if r.ID == id {
			return i
		}
	}
	return 0
}

// mustEndpoint 取接入点（Candidates 已校验存在；panic 防御nil——契约要求
// Candidate.Endpoint 非 nil）。
func mustEndpoint(c *StaticCatalog, name string) *wire.EndpointConfig {
	e, err := c.Endpoint(name)
	if err != nil {
		panic(fmt.Sprintf("ladder: endpoint %q vanished mid-bind: %v", name, err))
	}
	return e
}

// joinReasons 拼接拒绝原因（空列表给占位文本）。
func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return "(no reasons recorded)"
	}
	out := ""
	for i, r := range reasons {
		if i > 0 {
			out += "; "
		}
		out += r
	}
	return out
}
