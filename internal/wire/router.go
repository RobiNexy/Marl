package wire

import (
	"marl/internal/types"
)

// Candidate 是阶段 1 谓词过滤后剩下的一个候选绑定（Part 10.7）。
//
// 不变量：Endpoint 与 Model **非 nil**。两个指针字段是本结构的核心风险点——
// 它们在"Candidates 返回候选列表"的场景下可以合法带出，但任何打算用作
// Binding 的候选都必须齐备（见 Router.Bind 的后置条件）。nil 指针被解引用
// 的后果是 panic 在热路径上；而 nil 被容忍（如"Model 为 nil 就当默认模型"）
// 的后果更糟：会用未声明的模型发请求，且没有任何日志说明它从哪来。
//
// Score 是阶段 2 的产物：软约束加分 - 预估成本 + 亲和性奖励。它只用于**同一次
// 排序内**的相对比较，不带单位、不可跨调用累加或跨任务比较（成本项随价格表
// 变化）。因此不要把它当"候选质量分数"存进任何长期记录。
//
// Degraded 记录该候选未满足的 Prefer（软约束），会被写进 Binding 以便审计。
// 语义边界：**Degraded 非空不表示候选不可用**（硬约束已由阶段 1 过滤），
// 它只说明"选它会带来已声明的降级"。空切片与 nil 等价（无降级）。
type Candidate struct {
	Rung     types.Rung
	Endpoint *EndpointConfig
	Model    *ModelEntry
	// Score 是阶段 2 打分排序的结果（软约束加分 - 预估成本 + 亲和性奖励）。
	Score float64
	// Degraded 记录该候选未满足的 Prefer（软约束），会被写进 Binding。
	Degraded []types.Capability
}

// Router 是两阶段调度器（Part 10.7）：
//
//	阶段 1 谓词过滤（硬约束）：能力 Require、min_context、endpoint 健康、密钥存在
//	阶段 2 打分排序（软约束 + 成本）：Prefer 加分、成本减分、亲和性加分
//
// 亲和性这条很关键：换模型 = 缓存键空间切换 = 之前积累的缓存全废。
// 所以 Binding 在任务开始时决定一次，全程复用，只在失败或显式 Reconfigure 时重绑。
type Router interface {
	// Bind 为一次任务选定 Binding。agentID 直接成为 CacheBucket（唯一来源，永不配置）。
	//
	// 参数语义：
	//   - ladder 提供档位表；startIndex 是**起点下标**（Rebind 时从当前档位或
	//     下一档开始，从而保证升级单调不回退）。startIndex 必须落在
	//     [0, len(ladder.Rungs))，越界是调用方 bug，必须报错而不是 clamp——
	//     clamp 会把"从第 5 档开始"静默变成"从第 0 档开始"，即模型降级而不自知；
	//   - 不得信任 ladder.Start：它以 startIndex 为准，避免两条来源打架。
	//
	// 后置条件（可被断言检查）：Binding.CacheBucket == agentID；
	// Endpoint/Model 在 Catalog 中可查；BoundAt 非零；Degraded 已回填。
	//
	// 确定性：相同 (req, agentID, ladder, startIndex, 健康快照) 必须给出相同结果。
	// 这条不是为了美观——Build 期间若因随机 tie-break 抖动而换模型，缓存会
	// 反复失效，成本上升且无法复现。
	//
	// 失败：所有候选取尽仍无法满足 → 返回**说明哪条约束未满足**的错误
	// （"没有可用模型"这类笼统错误会让排查退回逐行读配置）。此时不得返回
	// 一个"最接近但违反硬约束"的 Binding——违反硬约束的请求注定被厂商拒绝。
	Bind(req types.Requirement, agentID types.AgentID, ladder *types.Ladder, startIndex int) (types.Binding, error)

	// Candidates 返回过滤+排序后的候选列表（首个即 Bind 的结果）。
	// 全部被过滤掉时返回明确说明哪条约束未满足的错误。
	//
	// 契约：按 Score 降序且**稳定**（同分保持 ladder 顺序，即便宜的在前）；
	// 返回的切片是副本，调用方修改不得影响 Router 内部状态。
	// 并发：必须可并发调用（各 Agent 会同时查询）。
	Candidates(req types.Requirement, ladder *types.Ladder, fromIndex int) ([]*Candidate, error)
}

// UpgradeSignal 是喂给阶梯升级判定的信号（Part 7.2 / 7.3）。
//
// 零值契约（危险）：Threshold 的零值会让"累积证据权重 >= 0"恒成立，
// 也就是**任何证据都立刻升级**——第一次失败就跳到最贵的模型。
// 因此 Threshold 必须 > 0，且由策略层显式配置（不硬编码在代码里，
// 因为阈值随模型换代而变，见 types.UpgradeEvidence 的说明）。
//
// FromRung 非空：升级是"从哪一级升到哪一级"的成对事件，缺了起点就无法
// 判断这次升级是否合理（可能已经在顶端）。
type UpgradeSignal struct {
	FromRung  types.RungID
	Evidence  types.UpgradeEvidence
	Threshold float64 // 累积证据权重阈值；必须 > 0
}
