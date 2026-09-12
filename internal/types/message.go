package types

import "time"

// InternalRole 是内部语义角色（10+ 种，可扩展）。它们只在框架内部存在，
// 编译成线路协议时被折叠为 4 种 WireRole（见 Part 3.4 的映射表）。
//
// 零值契约：零值 InternalRole("") 非法，且没有安全默认——角色决定消息在
// 上下文里的呈现身份，猜错会造成语义污染。因此它是"必须显式设置"的字段：
// LogEntry.Validate 拒绝零值，构造入口（NewLogEntry）强制传入。
// 消费侧遇到未识别角色时不得静默映射，必须要么按 RoleTransient 丢弃，
// 要么报错（映射表未覆盖即视为配置缺陷，见 Normalizer 契约）。
type InternalRole string

const (
	RoleUserInput      InternalRole = "user_input"
	RoleAssistantReply InternalRole = "assistant_reply"
	RoleToolResult     InternalRole = "tool_result"
	RoleThinking       InternalRole = "thinking"
	RoleConstraint     InternalRole = "constraint"
	RoleSharedMemory   InternalRole = "shared_memory"
	RoleSubTaskResult  InternalRole = "sub_task_result"
	RoleEscalation     InternalRole = "escalation"
	RoleHumanNote      InternalRole = "human_note"
	RoleTransient      InternalRole = "transient" // View-only，不入 Log
)

// Valid 报告 r 是否为已定义的内部角色之一。零值返回 false。
//
// 并发：纯函数。
func (r InternalRole) Valid() bool {
	switch r {
	case RoleUserInput, RoleAssistantReply, RoleToolResult, RoleThinking,
		RoleConstraint, RoleSharedMemory, RoleSubTaskResult, RoleEscalation,
		RoleHumanNote, RoleTransient:
		return true
	}
	return false
}

// Provenance 描述一条 LogEntry 的血缘来源（Part 3.2）。
type Provenance string

const (
	// ProvOriginal 是原始消息（人类输入、LLM 输出、工具结果）。
	ProvOriginal Provenance = "original"
	// ProvSummaryOf 是压缩生成的摘要，SourceIDs 指向被摘要的所有消息。
	ProvSummaryOf Provenance = "summary_of"
	// ProvSplitOf 是切块生成的段，SourceIDs 指向被切的原始消息。
	ProvSplitOf Provenance = "split_of"
	// ProvAnnotatedOf 是批注外壳，SourceIDs 指向被批注的原始消息。
	ProvAnnotatedOf Provenance = "annotated_of"
	// ProvInjected 是父 Agent 注入子上下文的引用（不复制内容，只带引用）。
	ProvInjected Provenance = "injected"
)

// Valid 报告 p 是否为已定义的血缘类型之一。零值返回 false。
//
// 零值契约：血缘是审计与"从真相重建投影"的依据（原则 2），因此不存在
// "未知血缘"。ProvOriginal 是合法值但不是零值——写成 ProvOriginal 是一次
// 显式断言（"这条是原始消息"），不是一个可以偷懒省略的默认。
//
// 并发：纯函数。
func (p Provenance) Valid() bool {
	switch p {
	case ProvOriginal, ProvSummaryOf, ProvSplitOf, ProvAnnotatedOf, ProvInjected:
		return true
	}
	return false
}

// Audience 三档决定一条消息进哪些投影（Part 3.2）。
//
//	AudienceContext —— 只进上下文
//	AudienceAudit   —— 只进审计表（如历史思维链，不占上下文 token）
//	AudienceBoth    —— 两者都进
//
// 零值契约：零值 Audience("") 非法且**没有任何合法解释**——三档覆盖了
// "上下文/审计/都进"的全集，零值不属于任何一档，会让该消息在两条投影里
// 同时消失（静默丢数据，违反原则 2 的"真相可重建"）。因此两条互补规则：
//
//   - 写入校验（fail fast）：MessageLog.Append 校验 Audience.Valid()，
//     零值即返回错误，绝不猜测；
//   - 读取降级（fail safe）：从旧数据读到未识别值时必须按 AudienceBoth 处理
//     （三档中唯一"不丢数据"的选择），并记审计告警。
//
// 并发：Valid 是纯函数。
type Audience string

const (
	AudienceContext Audience = "context" // 只进上下文
	AudienceAudit   Audience = "audit"   // 只进审计表（如历史思维链）
	AudienceBoth    Audience = "both"    // 两者都进
)

// Valid 报告 a 是否为三档之一。零值返回 false。
func (a Audience) Valid() bool {
	switch a {
	case AudienceContext, AudienceAudit, AudienceBoth:
		return true
	}
	return false
}

// TokenUsage 是一次 LLM 调用的 token 用量细分。口径必须是 Denormalizer 归一化后的
// （即已经能直接算钱的形态），否则 Ledger 里全是口径不一的数字（Part 10.11）。
//
// 不变量：
//   - 所有字段非负；
//   - 本结构只描述"用了多少"，不描述"花了多少钱"——计价由 Pricing 承担，
//     这样换价目表不需要改写历史记录。
//
// 零值语义：TokenUsage{} 是**合法**的，表示"用量确为零"（如空响应）。
// 因此"无用量数据"不能用零值表达，必须用指针 nil 区分——这就是
// LogEntry.TokenActual 与 Outcome.Usage 用 *TokenUsage 而非值类型的原因。
// 混淆二者会让报表把"未统计"算成"免费的 0"，污染成本决策。
//
// 口径（**已实测钉死**，2026-09-12 阶段 1 探测；依据见
// docs/design/probe-report-phase1.md §1 假设 4 与 §4 结论 4）：
//   - PromptTokens 是**输入总量（含命中缓存的部分）**，即
//     prompt_tokens = prompt_cache_hit_tokens + prompt_cache_miss_tokens
//     （实测例：923 = 768+155、1212 = 1024+188；探测报告 §1 假设 4 有三例）。
//     因此 CacheReadTokens ⊆ PromptTokens，计费公式必须是
//     (PromptTokens-CacheReadTokens)*InPerMTok + CacheReadTokens*CachedInPerMTok；
//     把 CacheReadTokens 再加到 PromptTokens 上会重复计费；
//   - CacheWriteTokens 在 DeepSeek 隐式缓存下**恒为 0**：厂商不单独上报/计费写入量
//     （未命中的输入本身就是"写"的那部分，它已经算在 PromptTokens 里）。
//     这与"未统计"不可区分，是本结构的已知局限；消费侧需要未命中量时用
//     PromptTokens-CacheReadTokens 推导，不要读 CacheWriteTokens。
type TokenUsage struct {
	PromptTokens     int // 输入总量
	CompletionTokens int // 可见输出
	ReasoningTokens  int // 思维链——独立计费，检测"刷思维链"的关键指标
	CacheWriteTokens int // 写缓存（约为未命中输入的 1.25 倍价）
	CacheReadTokens  int // 命中缓存（约为未命中输入的 1/4 价）
	ImageTokens      int // 图片附件贡献的 token（视觉类请求单独计费，见 10.14）
}

// LogEntry 是 Message Log 的一条不可变消息（Part 3.2）。
//
// 真相之源原则：Log 只追加、不修改、不删除。任何"删除、摘要、重排、剪枝"
// 都只发生在投影（ContextView）里，这里永远保留完整血缘。
//
// 不变量：
//   - Role / Prov / Audience 必须通过各自的 Valid()，零值非法（见各枚举契约）；
//   - Content 逐字节保留：框架不改写、不解析、不规范化 XML 标注；
//   - SourceIDs 非空当且仅当 Prov 不是 ProvOriginal——血缘是声明的，不是猜的；
//   - ID / Seq / CreatedAt 在构造期为零值，由 store.MessageLog.Append 回填。
//     因此"零值 LogEntry"从未落库，不构成可观察状态。
//
// 并发：结构体本身无锁。构造期由单一构造者独占（见 WithProvenance），
// Append 之后按约定不再修改，即可安全地被多 goroutine 只读共享。
type LogEntry struct {
	ID      MessageID
	AgentID AgentID
	Seq     int64 // 全局递增序号，排序与引用。不用时间戳（占 token、破缓存前缀）。
	Role    InternalRole
	Content string // 文本内容（含 XML 标注），框架逐字节保留、不解析。
	Prov    Provenance
	// SourceIDs 是血缘：摘要/切块/批注指向的源消息 ID 列表。
	SourceIDs []MessageID
	// Meta 是结构化元数据（如 split 的 topic、tool_call 的结构化信息）。
	// 零值 nil 可安全读取（读 nil map 返回零值）；写入前必须由构造者初始化。
	Meta     map[string]any
	Audience Audience
	TokenEst int // 本地估算，用于 View 预算
	// TokenActual 仅 LLM 调用返回的条目有值；工具结果等条目为 nil。
	// 用指针而非值：nil 表达"无用量数据"，零值结构体表达"用量为零"。
	TokenActual *TokenUsage
	// CreatedAt 只落在 SQLite 表里，编译上下文时**不渲染**
	// （Part 3.2 环境块；渲染时间戳会占 token 并破坏缓存前缀）。
	CreatedAt time.Time
}

// Validate 报告该条目是否具备落库条件。
//
// 检查项：Role / Prov / Audience 是否 Valid（零值即失败）。
// 不检查：ID / Seq / CreatedAt——它们由 Append 回填，构造期必然为零值。
//
// 失败：任一字段非法（错误须说明是哪个字段，便于定位构造点）。
// 并发：纯函数，不改动接收者。
func (e *LogEntry) Validate() error {
	panic("TODO(phase 0): placeholder")
}

// NewLogEntry 构造一条 LogEntry，并填充必填项的安全默认。
//
// 前置条件：role 必须 Valid；agentID 非空。
// 后置条件：AgentID=agentID，Role=role，Content=content，
// Prov=ProvOriginal，Audience=AudienceBoth，SourceIDs/Meta 为 nil。
// ID / Seq / CreatedAt 保持零值，由 store.Append 回填。
//
// 失败：role 非法时 panic——这属于程序员错误（调用点写死了错误角色），
// 与用户数据错误不同，不应在运行时被静默吞掉。panic 只用于构造期不可恢复
// 的错误，业务失败一律走 error 返回值。
//
// 并发：纯构造函数，无共享状态。
//
// [权衡: Audience 默认取 Both 而非 Context，是为了让"忘记设置"产生多余数据
// 而不是丢失数据（丢失不可恢复，多余只是浪费 token）。]
func NewLogEntry(agentID AgentID, role InternalRole, content string) *LogEntry {
	panic("TODO(phase 0): placeholder")
}

// WithProvenance 设置血缘并返回自身，用于链式构造。
//
// 前置条件：prov 必须 Valid；srcs 在 prov != ProvOriginal 时应非空
// （空表示"声称有来源但没给出"，属调用方缺陷，由 Validate 侧兜底）。
// 后置条件：e.Prov=prov；e.SourceIDs 为 srcs 的**副本**（不共享底层数组，
// 避免调用方后续修改切片导致已存条目的血缘漂移）。
//
// 失败：prov 非法时 panic（程序员错误，理由同 NewLogEntry）。
// 并发：修改接收者。同一 *LogEntry 不得在构造期被多 goroutine 并发设置；
// 完成构造（Append 之后）即视为冻结，可并发只读。
//
// [权衡: 用可变链式 setter 而非值返回，是为了让构造在调用点读起来像声明。
// 代价是 LogEntry 在构造期并非不可变——不可变性由"Append 之后不再修改"
// 的约定与 store 层不提供 Update 接口共同保证，而非由类型系统保证。]
func (e *LogEntry) WithProvenance(prov Provenance, srcs ...MessageID) *LogEntry {
	panic("TODO(phase 0): placeholder")
}
