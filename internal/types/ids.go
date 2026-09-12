package types

// 全局 ID 类型。全部是 string 别名：可序列化、可读、可打印、可进 Fossil/SQLite。
// 具体格式在各处注明；本文件不掺杂任何格式校验逻辑（那是实现阶段的事）。
//
// 零值契约（对全部 ID 类型统一适用）：零值 ID("") 表示"未分配 / 无"，
// **不是**合法标识。它有两种合法用法、一种非法用法，必须分清：
//
//   - 合法（表达"无"）：Profile.Extends 为空表示无继承；TraceID 为空表示不追踪。
//   - 非法（表达"某个对象"）：把零值 ProfileID 当作某个 Profile 去加载、
//     把零值 AgentID 当作某个 Agent 去取 Log。
//
// 区分不清的后果很具体：一次"参数没传"会被下游解释成"这个对象不存在"，
// 于是错误被归因到错误的位置（"找不到 Profile"而不是"调用方漏传 ID"）。
// 因此边界处的校验是"非空 + 存在性"两步，不能合并。

// MessageID 是消息的全局唯一标识。设计文档指定为 ULID，按生成时间可排序，
// 这样 Seq 之外还能有一把天然有序的钥匙（Part 3.2）。
type MessageID string

// AgentID 标识一个 Agent 实例。
// 额外身份：它同时是缓存桶标识（Binding.CacheBucket，Patch 1）——这不是巧合，
// 而是刻意的：一个 Agent 一个桶是唯一正确的策略，见 ADR-0007。
type AgentID string

// ProfileID 标识一个 Agent 配置（.marl/profiles/*.yaml 里的 id）。
type ProfileID string

// TaskID 标识一次任务（惰性分配：任务启动时生成，跨 Agent 传承）。
type TaskID string

// TraceID 是一条因果链的追踪标识，随 Envelope 与工具调用传播（Part 8.7）。
//
// 用命名类型而非裸 string 的理由：TraceID / MessageID / AgentID 都是短字符串，
// 在调用点上极易互换而编译器毫无察觉；跨包签名里出现裸 string 时，参数含义
// 只能靠命名与注释传达，而注释不会在编译期提供保护。命名类型把"这是追踪 ID"
// 编码进签名，零运行时成本。
//
// 零值语义：空表示"不追踪"（例如人类直接发起的调用），是合法状态。
type TraceID string

// RungID 是阶梯（ladder）中某一档的 ID（如 "r0" / "r1" / "r2"）。
// 与 Binding.RungIndex 的关系：RungID 是人类可读标签（配置里写），
// RungIndex 是数组下标（比较与升级用）。两者必须指向同一个 Rung，
// 由 Router.Bind 保证一致（见 Binding 的不变量）。
type RungID string

// WireID 标识一条线路协议。设计上只有 3–4 个（极低变化频率），
// 如 openai_chat（Part 10.2）。
// 放本包是因为 types.Binding 需要引用它，而 wire 包又要引用 types（避免循环依赖）。
//
// 零值契约：与大多数枚举不同，零值 WireID("") **不是**"未识别"，而是
// "未指定线路"。这是有意的：Binding 在 Router 选定之前确实没有线路，
// 而"未指定"必须能被表达（否则 Router 的中间候选只能填一个假值）。
// 但两个消费点的规则不同，不能混用：
//   - 入口构造（Binding → CanonicalRequest → WireRequest）必须显式赋值，
//     零值一律判错——用空线路发请求等于"用 A 协议编码、用 B 协议解析"；
//   - 查询/谓词遇到零值必须按"未指定"处理，绝不能回落成一个具体线路。
type WireID string

const (
	// WireOpenAIChat 是 OpenAI 兼容的 chat/completions 协议
	// （Deepseek 走这条）。阶段 1 唯一实现的线路。
	WireOpenAIChat WireID = "openai_chat"
)

// Valid 报告 w 是否为已实现的线路协议。零值返回 false。
//
// 为什么只声明已实现的线路：新线路在各自适配器落地时
// 才加入本表。先声明后实现会让"声明了但没有实现"静默存在——Router 会把它
// 当成可路由的线路（见 ADR-0014 对"静默失效"的分类）。
//
// 并发：纯函数。
func (w WireID) Valid() bool {
	switch w {
	case WireOpenAIChat:
		return true
	}
	return false
}

// DiscussionID 是讨论分支的 ID，形如 discuss_<ulid>（Part 11.2）。
type DiscussionID string

// EscalationID 是 Escalation 的 ID，形如 escalation_<ulid>（Part 11.3）。
type EscalationID string
