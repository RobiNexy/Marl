package wire

import (
	"time"

	"marl/internal/types"
)

// ErrorClass 是归一化后的错误分类（Part 10.11）。
// 升级与压缩触发都消费它；若留在厂商形态，上层会长满 if provider == "deepseek"。
//
// 零值契约（本包唯一的例外，务必看清）：零值 ErrorClass("") **就是**
// ErrNone——因为"没有错误"需要一个正常值来表达，这与其他枚举"零值非法"的
// 结论相反。由此带来两个必须遵守的纪律：
//   - 需要判断"这次调用失败了吗"时用 IsError()，不要用 c != "" 之外的花招，
//     更不要用 !Valid()（Valid 对 ErrNone 返回 true）；
//   - 消费侧**不得**把"零值"当作"未赋值"来兜底处理（例如"没分类就按
//     transient 重试"）。分不到位就该留空，让上层看到"未分类"这个事实。
type ErrorClass string

const (
	ErrNone            ErrorClass = ""
	ErrTransient       ErrorClass = "transient"           // 429/503/超时 → Pool 退避重试，不计入升级证据
	ErrContextOverflow ErrorClass = "context_overflow"    // → 触发压缩，不是升级
	ErrCapability      ErrorClass = "capability_rejected" // → 修正本地能力覆盖表 + 告警，不盲重试
	ErrAuthQuota       ErrorClass = "auth_quota"          // → 标记 endpoint 不健康，熔断，跳到阶梯下一级
	ErrContentFilter   ErrorClass = "content_filter"      // → 交回 LLM 决策，记审计
	ErrMalformed       ErrorClass = "malformed_output"    // → 计入升级证据；可追加格式纠偏 Transient
)

// Valid 报告 c 是否为已定义分类（含 ErrNone）。
//
// 语义区分（两个都 false 的情况必须被上层看见）：
//   - Valid()==true && IsError()==false → 调用成功；
//   - Valid()==true && IsError()==true  → 已归类的失败，可按类别处置；
//   - Valid()==false                    → **未分类**：既不能重试也不能升级，
//     必须记审计并交给人看（厂商可能改了错误格式）。把它当 transient 重试
//     是这里最容易犯的错：一次永久性错误会被重试到烧完预算。
//
// 并发：纯函数。
func (c ErrorClass) Valid() bool {
	switch c {
	case ErrNone, ErrTransient, ErrContextOverflow, ErrCapability,
		ErrAuthQuota, ErrContentFilter, ErrMalformed:
		return true
	}
	return false
}

// IsError 报告 c 表示一次失败（ErrNone 之外的所有已定义类别）。
//
// 未知分类（Valid()==false）返回 false：调用方必须先用 Valid() 分流，
// 不能指望 IsError() 替它做"未知 = 失败"的判断。
// 并发：纯函数。
func (c ErrorClass) IsError() bool {
	return c.Valid() && c != ErrNone
}

// ReasoningChunk 是一段连续的思维链（Patch 1：可多条，见 WireTurn）。
type ReasoningChunk struct {
	Content  string
	Duration time.Duration
}

// ToLogEntry 把思维链片段转成 Role=thinking 的 LogEntry（落 Log 形态）。
//
// 契约：
//   - 落库的 Role 恒为 types.RoleThinking，不因调用方不同而变；
//   - Content 必须与 chunk.Content 逐字节相同（思维链是要被审计的原始证据，
//     任何"整理一下再存"都会破坏它与厂商响应的对应关系）；
//   - 不写 Seq（落库时由 MessageLog.Append 分配，见 store.MessageLog 契约）；
//   - 接收者为 nil 时返回零值 LogEntry 而不是 panic：不存在"无思维链的
//     思维条目"，把这条边界显式化，比让调用方自己去判 nil 更不容易漏。
//
// 并发：纯函数（不修改接收者）。
func (r *ReasoningChunk) ToLogEntry() types.LogEntry {
	panic("TODO(phase 0): placeholder")
}

// ThinkingOutcome 是 thinking 模式的实测落点（Patch 1）。
type ThinkingOutcome struct {
	ActualLevel    string // 实际生效的 thinking 档位（含 Normalizer 降级后的值）；与阶梯 RungID 无关
	ReasoningField string // 响应字段名（如 "reasoning_content" / "thinking"）
	Exposed        bool   // 是否落 Log / 可见
}

// OutcomeSignals 是给框架的结构化信号（Part 10.11）。
// 升级判据只读 Signals，不读厂商响应——这样"证据触发升级"才真的和厂商解耦。
//
// 零值契约：OutcomeSignals{} 是**合法**的，含义是"本轮无异常信号"
// （成功、非畸形、无工具错误、有变更）。这与"信号缺失"不可区分——
// 因此 Denormalize 在无法判定时必须显式填写（如把 MalformedOutput 设为
// true 或给出 ToolErrorKind），不能让"没填"和"没问题"混为一谈。
//
// ToolErrorKind 的取值必须来自技能层的错误码常量（如 skill 包的
// path_not_found），不得在此自由发挥字符串：升级判据要按它做统计，
// 拼写漂移会让统计静默丢样本。
type OutcomeSignals struct {
	ErrorClass      ErrorClass
	MalformedOutput bool
	ToolErrorKind   string // 如 "path_not_found" / "syntax_error"（取自技能层错误码）
	NoMutationTurn  bool   // 这一轮无 mutating 技能调用成功
}

// Outcome 是一次 Wire.Execute 中的一段产出（Part 10.11 / 10.16）。
// 一条响应里可能有多个 Outcome（reasoning / tool_call / reply 混合流）。
//
// 不变量：Reasoning / Reply / ToolCalls 至少有一项非空。三项全空的 Outcome
// 是纯噪音——它会进入 Log 与上下文、占用 token、让"本轮有几个产出"的计数
// 失真，却没有任何信息。Denormalizer 必须丢弃这种片段而不是照样产出。
//
// Usage 的口径与 WireResponse.UsageRaw 一致：nil = 未知（失败或厂商未回传），
// **不等于 0**。记账侧不得用零值冒充（见 LedgerEntry）。
type Outcome struct {
	Reasoning *ReasoningChunk // 思维链片段（denormalize 时拆出）
	Reply     string          // 可见文本回复
	Entry     types.LogEntry  // 产出的 LogEntry（主循环统一落 Log）
	ToolCalls []types.ToolCall
	Usage     *types.TokenUsage // nil 表示用量未知（≠0）
	Signals   OutcomeSignals
	Thinking  *ThinkingOutcome
}

// WireTurn 是一次 Wire.Execute 的全部产出（Patch 1，Part 10.16）。
// 旧设计"一次 Execute 返回一个 Outcome"装不下混合流形态。
type WireTurn struct {
	Outcomes []Outcome
}

// Ready 判断这一 turn 是否已具备进入下一轮的条件（有可见回复或工具调用）。
//
// 关键边界：**只有思维链、没有回复也没有工具调用 → 不算 ready**。
// 这正是本方法存在的理由：thinking-only 的响应如果被当成"完成一轮"，
// 主循环会在没有产出任何动作的情况下空转（每轮都在想、什么都不做），
// 表现为 Agent 静默烧预算。不 ready 时的正确处置是继续同一轮
// （或在 MaxRetries 用尽后计入升级证据），而不是推进轮次。
//
// nil 接收者返回 false（没有产出即未就绪）。
// 并发：纯函数。
func (t *WireTurn) Ready() bool {
	panic("TODO(phase 0): placeholder")
}

// Denormalizer 把厂商响应翻译成归一化信号（Part 10.11，回程比去程更值钱）。
type Denormalizer interface {
	// Wires 声明本实现支持的线路（返回**副本**：调用方可能拼接/排序，
	// 共享可变切片会静默改掉全局声明）。
	Wires() []types.WireID

	// Denormalize 拆出 reasoning / tool_call / reply，归一化 usage 与错误分类。
	//
	// 契约：
	//   - 纯函数：不得修改 resp（原始响应体是审计证据，落库时还要用）；
	//   - 厂商错误不进 error 返回值，而是分类进 OutcomeSignals.ErrorClass
	//     （见 ErrorClass 零值契约）：上层要按类别分流处置，包装成 error 字符串
	//     会让"该重试/该压缩/该熔断"的判断退化成字符串匹配；
	//   - 返回 error 只用于"响应根本无法解析"（空 body、JSON 结构不合法、
	//     协议字段完全缺失）；
	//   - 明确的无关字段必须在**没有可产出内容**时也返回 Outcome（带上
	//     ErrorClass），不得返回"空 WireTurn + nil error"——那会让上层把
	//     一次失败当成一次成功的空响应。
	//
	// 并发：必须可并发调用（同一实现会被多 Agent 共享）。
	Denormalize(resp *WireResponse) (*WireTurn, error)
}
