package types

import "time"

// Binding 是一次任务选定的运行绑定（Part 10.8）。
//
// 关键语义：Binding 在任务开始时决定一次，全程复用，只在证据触发升级或显式
// Reconfigure 时重绑。换模型 = 缓存键空间切换 = 之前积累的缓存全废，所以
// 亲和性（当前任务已绑定的模型加分）是 Router 打分的重要项。
//
// 不变量（由 Router.Bind 保证，Rebind 时必须重新校验）：
//   - CacheBucket 恒等于该 Agent 的 AgentID（Patch 1：不配置、不暴露选项）；
//   - RungID 与 RungIndex 指向同一个 Rung（前者是人类可读标签，后者是下标）；
//   - CachePrefix 与 (Model, Endpoint) 一致——它是纯派生值，不允许手工拼装；
//   - BoundAt 非零（它同时是"优惠期"判定的基准，见 10.8）。
//
// 零值契约：Binding{} 非法（无档位、无模型、无线路）。它是**必须显式填充**的
// 结构，绝不作为默认值传递——一个零值 Binding 的请求会打到未定义的端点。
type Binding struct {
	RungID    RungID // 阶梯档位 ID，如 "r0"
	RungIndex int    // 在 ladder 数组里的下标
	Endpoint  string // 接入点名（endpoints.yaml 里的 name）
	Model     string // models.yaml 里的模型 id
	// CacheBucket 永远是 AgentID（Patch 1：每个 Agent 独立缓存桶）。
	// 类型是 AgentID 而非 string：这样"误传模型名/ProfileID"会在编译期被拒绝，
	// 而不是变成本 Agent 之外的缓存桶（会导致跨 Agent 缓存污染，且极难排查）。
	CacheBucket AgentID
	Wire        WireID
	Thinking    ThinkingSpec // 这一级的 thinking 配置（字符串档位）
	Degraded    []Capability // 未满足的 Prefer（软约束），进审计
	BoundAt     time.Time
	CachePrefix string // model_id + endpoint，模型级缓存键前缀
}

// Rung 是阶梯（ladder）的一档（Part 7.1）。有序，从便宜到贵。
//
// 零值契约：Rung{} 非法（ID/Endpoint/Model 皆空）。CostPerMTok 零值是**合法**的
// （免费/本地模型），但零值无法区分"免费"与"忘了填价格"——而价格直接决定阶梯
// 排序与升级决策，因此加载期应要求显式声明（见 Ladder 的校验契约）。
type Rung struct {
	ID          RungID
	Description string
	Endpoint    string // 接入点名（endpoints.yaml 里的 name）
	Model       string // models.yaml 里的模型 id
	CostPerMTok float64
	Currency    string // 如 "CNY"
}

// Ladder 是项目/全局的阶梯配置。
//
// 不变量（加载期必须全部校验，违反即无法路由）：
//   - Rungs 非空且 Rung.ID 在表内唯一；
//   - Start 非空且必须能在 Rungs 中找到对应项——一个指向不存在档位的起点
//     会让每次 Bind 都失败，且报错点离配置错误很远；
//   - Rungs 按 CostPerMTok 非递减排列（"从便宜到贵"是升级语义的前提）；
//   - 每个 Rung.Currency 非空（跨币种比较价格是错的，宁可拒绝加载）。
//
// 零值契约：Ladder{} 非法（无档位、无起点）。
type Ladder struct {
	Rungs []Rung
	Start RungID // 项目 config 里的 ladder_start，如 "r0"
}

// UpgradeEvidence 是阶梯自动升级的证据累计器输入（Part 7.2）。
// 升级决策基于可观测证据，不依赖 LLM 自述。
//
// 零值语义：UpgradeEvidence{} 表示"没有任何异常证据"，即不升级——安全方向。
// 各字段的阈值不应硬编码在本层，而应来自可配置的 Router 策略：阈值随模型
// 换代而变（旧模型 2 次失败算信号，新模型可能是 5 次），把数字写死在结构体
// 注释里会让它无法随代码演进而调整。
type UpgradeEvidence struct {
	ConsecutiveFailures   int     // 连续失败（同一子任务重试 2 次仍失败）
	ToolFormatErrors      int     // tool_call JSON 解析失败 ≥2 次
	NoProgressRounds      int     // 连续 3 轮无进展
	ChildFailureRate      float64 // fork 的子中失败占比
	CompressionReclaimLow bool    // 压缩后 token 减少 <20%
}

// UpgradeDecision 是升级决策结果（Part 7.3）。
//
// 零值语义：零值表示"不升级"（ShouldUpgrade=false, FromRung 为空, ToRung 为空），
// 这是安全方向——最坏情况是停留在当前档位，而不是误跳到一个更贵/未验证的模型。
// 规则：ShouldUpgrade 为 true 时 ToRung 必须非空（否则说明决策器有 bug）；
// 为 false 时 ToRung 应为空（带上 ToRung 会让审计日志误报一次未发生的切换）。
type UpgradeDecision struct {
	ShouldUpgrade bool
	FromRung      RungID
	ToRung        RungID
	Reason        string // 审计用的人类可读原因
}
