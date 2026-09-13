package wire

import (
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

// CacheMode 是模型的缓存模式（Part 10.4）。
//
// 零值契约（fail-closed，方向很重要）：零值 CacheMode("") 非法，且**绝不允许
// 被回落成 CacheImplicitPrefix**。缓存的两种模式对外行为完全不同：
// 隐式前缀缓存靠"前缀稳定就自动命中"，显式断点缓存必须由 Normalizer 在特定
// 消息上打 cache_control。把显式协议误当隐式协议，结果是**缓存永不命中**——
// 没有报错、没有告警，只是账单变成两倍。
// 因此未识别值的处置是回落到 CacheNone（不假设能缓存，也就不会误以为省钱），
// 并由加载期校验报错。
type CacheMode string

const (
	// CacheImplicitPrefix：保持前缀稳定就自动缓存（Deepseek）。
	CacheImplicitPrefix CacheMode = "implicit_prefix"
	// CacheExplicitBreakpoint：需要在特定消息上标 cache_control（显式断点协议）。
	CacheExplicitBreakpoint CacheMode = "explicit_breakpoint"
	CacheNone               CacheMode = "none"
)

// Valid 报告 m 是否为已定义缓存模式。零值返回 false。
// 并发：纯函数。
func (m CacheMode) Valid() bool {
	switch m {
	case CacheImplicitPrefix, CacheExplicitBreakpoint, CacheNone:
		return true
	}
	return false
}

// ThinkingControl 是思维控制方式（Part 10.4）。
//
// 零值契约：零值 ThinkingControl("") 非法，且不得被默认成 ThinkControlBool。
// 三者的**参数形状**不同（bool / 档位字符串 / token 数），
// 猜错会让 Normalizer 把一段参数编码成另一种形态，厂商侧通常直接 400；
// 更糟的情况是厂商容忍多余字段，于是 thinking 静默不生效。
type ThinkingControl string

const (
	ThinkControlBool   ThinkingControl = "bool"   // 仅开关
	ThinkControlLevel  ThinkingControl = "level"  // 离散档位
	ThinkControlBudget ThinkingControl = "budget" // 连续 token 预算
)

// Valid 报告 t 是否为已定义控制方式。零值返回 false。
// 并发：纯函数。
func (c ThinkingControl) Valid() bool {
	switch c {
	case ThinkControlBool, ThinkControlLevel, ThinkControlBudget:
		return true
	}
	return false
}

// ModelCaps 是模型的声明能力（Part 10.4，来自 models.yaml，进版本控制）。
// 声明会错，所以还要有 probe（10.13）与运行时反证降级。
//
// 不变量（加载期必须校验，违反即无法安全路由）：
//   - MaxContext > 0 且 MaxOutput > 0（零值会把模型"声称为不可用"，
//     而过滤期看到 0 上下文会把它静默排除，表现为"这个模型从不被选中"）；
//   - MaxOutput <= MaxContext（超出的输出上限只能是配置笔误）；
//   - Has 中每项 Capability.Valid()（无效能力会被谓词当作永不满足的条件，
//     于是所有 Require 它的 Profile 都路由失败，而错的是拼写）；
//   - CacheMode.Valid()、ThinkingControl.Valid()；
//   - ThinkingControl == ThinkControlLevel 时 ThinkingLevels 非空
//     （档位控制却没有档位表，Normalizer 无从翻译请求的档位）；
//   - UnsupportedParams 各项非空（空串会匹配一切/什么也不匹配，两种解释都危险）。
type ModelCaps struct {
	Has               []types.Capability
	MaxContext        int
	MaxOutput         int
	CacheMode         CacheMode
	UnsupportedParams []string // API 不接受的采样参数（Normalizer 需剔除）
	ThinkingControl   ThinkingControl
	ThinkingLevels    []string // 该模型支持的档位名（Patch 1 字符串档位）
	VisionDetail      bool     // 是否支持 detail 档位（low/high/auto）
}

// Pricing 是模型的计价（Part 10.4）。
//
// 不变量（全部属于"配置写错就会静默给出错误决策"的类型）：
//   - Currency 非空，且同一 Ladder 内必须一致（跨币种比较价格是错的，
//     见 types.Ladder 校验契约）；
//   - 四项单价均 >= 0；
//   - CachedInPerMTok <= InPerMTok：缓存命中必须不贵于未命中。写反了会让
//     路由打分的成本项把"缓存友好"算成"更贵"，于是系统主动避开缓存命中率高的
//     候选——而缓存正是成本命脉。零值 CachedInPerMTok 会被读成"缓存免费"，
//     同样不可接受，因此加载期要求显式声明。
type Pricing struct {
	InPerMTok        float64
	CachedInPerMTok  float64
	OutPerMTok       float64
	ReasoningPerMTok float64
	Currency         string // 如 "CNY"；必须非空
}

// ModelEntry 是模型目录里的一条（Part 10.4，纯数据）。
//
// 不变量：ID / Provider / RemoteName 非空，Wire.Valid()。
// RemoteName 非空尤其重要：它是真正发给厂商的名字（如 "deepseek-flash"），
// 与内部 ID（"deepseek-flash"）是两个命名空间；为空会让适配器的 ModelName
// 无处可查，只剩"猜一个"这条路。
type ModelEntry struct {
	ID         string
	Provider   string
	Wire       types.WireID // 线路协议
	RemoteName string       // 传给厂商的模型名
	Caps       ModelCaps
	Pricing    Pricing
}

// EndpointConfig 是一个接入点配置（Part 10.5，纯配置，随时增删）。
// 注意：Patch 1 删掉了 bucket_strategy 字段——缓存桶就是 AgentID，不配置。
//
// 不变量：Name / BaseURL / KeyRef 非空，MaxInflight > 0，RPM > 0。
// 零值 EndpointConfig{} 非法（它会以空 URL、零并发"可路由"地存在，
// 让所有打到它的请求同时失败且成因难以一眼看出）。
//
// KeyRef **只能是对密钥的引用**（如 "env:DEEPSEEK_API_KEY"），不得写明文
// 密钥：endpoints.yaml 是要进版本控制、要被人分享的文件。明文密钥一旦写入，
// 轮换密钥（删掉旧 key）根本清不掉历史，只能认为已泄露。校验点放在加载期，
// 因为运行时已经太晚。
type EndpointConfig struct {
	Name        string
	BaseURL     string
	KeyRef      string // 如 "env:DEEPSEEK_API_KEY"；严禁明文密钥
	MaxInflight int
	RPM         int
}

// CapsOverride 是能力探测的实测覆盖（Part 10.13，机器级，不进版本控制）。
// Router 谓词读取时优先用它，没有才回落 models.yaml 声明。
//
// 不变量：Model / Endpoint 非空，Has 中每项 Capability.Valid()，ProbedAt 非零
// （零值表示"从未探测"，那么这份覆盖就不该存在——保留它等于让一句没有
// 时间戳的断言永久生效，而模型能力会随厂商升级变化）。
//
// 合并语义（必须明确，否则"实测发现不支持 X"无从表达）：
// Has 是**整集替换**声明值，而不是取并集。取并集只能加不能减，
// 而运行时反证降级的全部价值恰恰是"删掉一个声明了但实测不支持的能力"。
// ModelCaps 的其余字段仍取声明值（探测只回答"有哪些能力"）。
type CapsOverride struct {
	Model    string
	Endpoint string
	Has      []types.Capability // 实测发现的能力集（覆盖声明，整集替换）
	ProbedAt time.Time
}

// Catalog 是模型目录 / 接入点 / 阶梯 / 覆盖的只读查询入口。
// 全部是纯数据访问，Router 与 Normalizer 消费它。
type Catalog interface {
	// Model 与 Endpoint 在 id/name 未知时返回错误（ErrNotFound 语义）。
	//
	// 零值契约：**不得**在找不到时返回 (零值, nil)。零值 ModelEntry 的
	// Wire 为空、RemoteName 为空，会一路走到"用空模型名发请求"；
	// 零值 EndpointConfig 的 BaseURL 为空，会打到未定义地址。两者都不报错。
	Model(id string) (*ModelEntry, error)
	Endpoint(name string) (*EndpointConfig, error)

	// Ladder 返回阶梯配置。实现必须返回**非 nil**：阶梯缺失属于启动期
	// 致命配置错误，应在启动自检时就失败，而不是在第一次 Bind 时 nil 解引用。
	Ladder() *types.Ladder

	// EffectiveCaps 返回声明 + 实测覆盖后的有效能力集。
	//
	// 覆盖存在时：Has 取覆盖（整集替换，见 CapsOverride），其余字段取声明。
	// 覆盖不存在时：返回声明值，**不返回错误**（没探测过是正常状态）。
	// 模型整体未知时才返回错误。
	EffectiveCaps(modelID, endpoint string) (ModelCaps, error)

	// Pricing 返回用于 Ledger 记账的计价。
	//
	// 失败：未知模型/接入点 → 错误。**不得回落成零价**——零价会被成本打分
	// 读成"免费"，于是最贵的模型拿到最优分数并被执行器反复选中；
	// 同时 Ledger 会把所有花费记为 0，账单直接失真。记账缺价的正确表现是
	// "这条调用无法计价"，而不是"这条调用不要钱"。
	Pricing(modelID, endpoint string) (Pricing, error)
}
