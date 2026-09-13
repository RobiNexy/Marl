package wire

import (
	"context"
	"fmt"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

// HealthStatus 是接入点健康状态（Part 10.12）。
//
// 零值契约（方向与多数枚举相反，必须注意）：零值 HealthStatus("") 非法，
// 表示"**尚未探测**"。它不能被当作 HealthHealthy——那会让 Router 谓词在
// 启动瞬间把从未探测过的 endpoint 当成可用，于是首个任务直接打到可能已死的
// 接入点上；也不能被当作 HealthDown——那会让"还没探测"变成"确认故障"，
// 白白把可用接入点排除掉。
// 因此谓词遇到零值必须**显式**走"先探测/按配置放行并记告警"的分支，
// 而不是靠比较运算把它归入某一档。
type HealthStatus string

const (
	HealthHealthy  HealthStatus = "healthy"
	HealthDegraded HealthStatus = "degraded"
	HealthDown     HealthStatus = "down"
)

// Valid 报告 s 是否为已探测出的状态之一。零值返回 false。
// 并发：纯函数。
func (s HealthStatus) Valid() bool {
	switch s {
	case HealthHealthy, HealthDegraded, HealthDown:
		return true
	}
	return false
}

// HealthState 是单接入点的熔断状态（Part 10.12）。
//
// 两个字段的分工（别混用）：
//   - Status 是**聚合健康度**，供 Router 谓词做粗过滤；
//   - CircuitOpen 是**熔断器开关**，由 CircuitPolicy 的阈值/冷却驱动。
//
// 不变量：CircuitOpen == true 时 Status 不得是 HealthHealthy（熔断已开却对外
// 报健康，会让谓词持续选中一个被隔离的接入点，熔断就完全失效了）。
// ErrorCount/LastError 仅在熔断窗口内有意义，不应作为路由依据。
type HealthState struct {
	Status      HealthStatus
	LastError   time.Time
	ErrorCount  int
	CircuitOpen bool
}

// Limiter 是令牌桶限流的抽象（Part 10.12）。
// 阶段 0 不引入 golang.org/x/time/rate 依赖，先用接口钉死语义：
// Wait 阻塞直到拿到一个令牌或 ctx 取消。
//
// 失败：ctx 取消/超时 → 返回 ctx.Err()（原样，不包装）。限流等待被取消
// 属于正常停机路径，不是限流故障，混同会让熔断计数虚高。
//
// 并发：必须可被多 Agent 并发调用（这里正是全局竞争点）。
type Limiter interface {
	Wait(ctx context.Context) error
}

// EndpointState 是一个接入点的运行时并发状态（Part 10.12）。
//
// 零值契约：EndpointState{} 非法（Semaphore 为 nil 会永久阻塞、Limiter 为
// nil 会 panic、MaxInflight 为 0 等于把该接入点静默禁用）。
// 构造不变量（必须由构造函数保证，而不是每次都检查）：
//   - MaxInflight > 0 且 RPM > 0；
//   - Semaphore 已分配，cap(Semaphore) == MaxInflight（闸门容量必须与配置一致，
//     否则"配置 10 并发"实际跑 100）；
//   - Limiter 非 nil（速率 = RPM）。
//
// 语义说明：Semaphore 与 Limiter 是**两个不同维度**的约束（同时在途数 vs
// 每秒请求数），缺任一个都会在某些流量形状下突破厂商限制。
type EndpointState struct {
	Name        string
	MaxInflight int
	RPM         int
	// Semaphore 是并发闸门（长度 = MaxInflight）。
	Semaphore chan struct{}
	// Limiter 是令牌桶（速率 = RPM）。
	Limiter Limiter
	Health  HealthState
}

// PoolCall 是一次进入并发池的调用（Part 10.12）。
//
// 不变量：AgentID / Binding 非空、Request 非 nil、TraceID 非空（用于把排队
// 与厂商侧日志对上）；Depth >= 0。
//
// Depth 的用途是队列纪律（深度优先：深层 Agent 离完成更近，先放行能更快
// 释放整条 wait_children 链）。它不是优先级"可选项"：Depth 缺失或全为 0
// 会让队列退化成 FIFO，深处接近完成的子任务被浅层新任务反复插队，
// 表现为"整棵树的完成时间显著变长"而每层都看不出问题。
type PoolCall struct {
	AgentID types.AgentID
	Depth   int // 队列纪律：深度优先——深层 Agent 离完成更近
	Binding types.Binding
	Request *WireRequest
	TraceID types.TraceID
}

// QueueItem 是优先队列元素（按深度分桶，不是简单 channel）。
type QueueItem struct {
	PoolCall
	EnqueuedAt time.Time
}

// Pool 是真正的背压点（Part 9.8 / 10.12）。
//
// 并行 fork 的实际上限不是 maxDepth，是 API 的并发上限和 RPM。
// Spawner 只管拓扑，并发全部压在这里：排队（阻塞而非 429 炸掉）、
// 闸门、限流、熔断、按 endpoint 维度隔离。
type Pool interface {
	// Execute 排队 → 等槽位 → 等限流 → 调 Wire → 归一化错误分类。
	//
	// 阻塞式背压（关键语义）：超出容量时**排队阻塞**，不返回"忙"错误、
	// 不直接打到厂商（那会变成 429）。因此 ctx 取消是唯一的提前退出方式，
	// 取消必须原样返回 ctx.Err()。
	//
	// 后置条件：返回的 WireTurn 里的 Outcome 保留 Denormalizer 产出的
	// Signals 与 Thinking 信息——Pool 只做调度，**不得**裁剪或改写语义内容
	// （它不拥有那些字段的含义，改动会让"证据触发升级"的输入与日志对不上）。
	//
	// 失败：排队/限流阶段 ctx 取消 → ctx.Err()；熔断开路 → 明确的错误
	// （调用方据此跳到阶梯下一级，而不是重试）；厂商错误 → 已分类的错误。
	//
	// 并发：必须支持多 Agent 并发调用（这是本接口存在的全部理由）。
	Execute(ctx context.Context, call *PoolCall) (*WireTurn, error)

	// Health 返回各接入点的健康快照（Router 谓词直接读它做过滤）。
	//
	// 返回 map 的 key 是 EndpointConfig.Name。快照是**某一时刻的副本**，
	// 调用方不得据此做跨时刻判断（健康度随时在变）。
	// 并发：必须可并发调用且不阻塞在途请求。
	Health() map[string]HealthState

	// Inflight 返回当前在途调用数（Watchdog / 状态观测用）。
	//
	// 口径：包含正在排队等待槽位/限流的调用（它们已经占用了系统资源与
	// 用户的时间），不只是已发出 HTTP 的那些。这个口径必须与 maxInflight
	// 配置语义一致，否则观测到的数字永远低于限制，运维会以为还有余量。
	Inflight() int
}

// CircuitPolicy 是熔断阈值（Part 10.12）。
//
// 零值契约（危险）：零值 CircuitPolicy{} 会让"连续错误数 0 即开路"成立，
// 也就是**熔断一开始就是开的**，所有请求直接短路——看起来像全站故障。
// 因此 Validate 要求 ErrorThreshold > 0 且 Cooldown > 0，构造时必须显式填值。
type CircuitPolicy struct {
	ErrorThreshold int           // 连续错误数达到即开路；必须 > 0
	Cooldown       time.Duration // 开路后冷却多久再半开；必须 > 0
}

// Validate 报告熔断策略是否可用：ErrorThreshold > 0、Cooldown > 0。
//
// 零值即开路（见类型注释），因此零值策略必须被拒绝——这不是形式主义，
// 是"熔断一开始就是开的"这道防线的唯一守卫。
// 并发：纯函数。
func (p CircuitPolicy) Validate() error {
	switch {
	case p.ErrorThreshold <= 0:
		return fmt.Errorf("circuit policy: error threshold %d must be > 0 (zero means the circuit opens immediately)", p.ErrorThreshold)
	case p.Cooldown <= 0:
		return fmt.Errorf("circuit policy: cooldown %v must be > 0", p.Cooldown)
	}
	return nil
}
