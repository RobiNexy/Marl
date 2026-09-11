package types

// Capability 是模型能力枚举，Profile 用它表达"我需要什么"（Part 6.4 修正 2）。
// 注意：Profile 绝不点名 model_id——绑定由 Router 从阶梯里选。
//
// 零值契约：零值 Capability("") 非法。列表里混入未识别值必须判为**错误**而非
// 忽略：能力名写错（"toolcall" / "jsons"）若被静默丢弃，Router 会选出一个
// 其实不支持该能力的模型，故障直到运行时才以"工具调用返回乱码""JSON 解析失败"
// 的形式暴露，归因成本极高。因此 ProfileLoader 必须在加载期逐个 Valid() 校验。
//
// [权衡: 本枚举不含"缓存能力"（上一轮草案曾加 CapCache，已移除）。理由：
// 缓存只影响成本、不影响正确性，而 Requirement.Require 是决定"哪个模型可用"的
// 硬约束——把成本特性塞进可行性约束，会让"没有支持缓存的模型"变成"无模型可用"
// 这类无解报错。缓存差异应由 ModelCaps 的独立字段表达，而不是 Profile 的需求列表。]
type Capability string

const (
	CapToolCall     Capability = "tool_call"
	CapJSONMode     Capability = "json_mode"
	CapThinking     Capability = "thinking"
	CapPrefill      Capability = "prefill"
	CapVision       Capability = "vision"        // 图片理解
	CapVisionDetail Capability = "vision_detail" // 图片高细节模式（如 OpenAI 的 detail=high）
	CapImageGen     Capability = "image_gen"     // 图片生成；进枚举占位，v1 不实现
)

// Valid 报告 c 是否为已定义能力之一。零值返回 false。
//
// 并发：纯函数。
func (c Capability) Valid() bool {
	switch c {
	case CapToolCall, CapJSONMode, CapThinking, CapPrefill,
		CapVision, CapVisionDetail, CapImageGen:
		return true
	}
	return false
}

// Profile 是一个 Agent 的完整配置（修正 2 后的形态）。
// 只表达需求与采样偏好，不点名模型；是否允许 spawn 等策略也在这里。
//
// 载入契约：本结构是**配置结构**（带 yaml tag，来自 .marl/profiles/*.yaml），
// 字段名与 tag 都是对外契约的一部分——改名即破坏用户配置文件，属需 ADR 的
// 破坏性改动。加载后必须先经 ProfileLoader 校验（见各字段零值契约）再交给
// Router，不允许"边用边补默认值"（那样错误会延迟到运行时才暴露）。
type Profile struct {
	ID          ProfileID `yaml:"id"`
	Extends     ProfileID `yaml:"extends,omitempty"` // 继承父 Profile；"" 表示无
	Description string    `yaml:"description,omitempty"`
	Prompt      string    `yaml:"prompt,omitempty"` // prompts/_index.yaml 里的 id

	Requirement     Requirement    `yaml:"requirement"`
	Sampling        SamplingParams `yaml:"sampling"`
	Thinking        ThinkingSpec   `yaml:"thinking"`
	Task            TaskPolicy     `yaml:"task"`
	OutgoingContext ContextPolicy  `yaml:"outgoing_context"`
	// AllowedSkills 调用时校验用，不影响 schema。空列表 = 全部允许（不限制）。
	// 方向说明：技能是"能力"而非"数据"，默认开放只损失稳定性、不泄露数据；
	// 命名空间则相反（默认 hidden，因为数据泄露不可逆）。两者默认方向不同是
	// 刻意的，不是疏漏。
	AllowedSkills []string `yaml:"allowed_skills,omitempty"`
	CanSpawn      bool     `yaml:"can_spawn,omitempty"`
}

// Requirement 是能力需求（Part 7.3 修正 2）。
//
// 零值契约：Requirement{} 合法——"无硬约束、无偏好、不限上下文"是有意义的
// 声明（"给我任意可用模型"）。因此 Require/Prefer 为空不等于配置缺失；
// 但列表内的**每个元素**必须 Valid（见 Capability 契约）。
type Requirement struct {
	Require    []Capability `yaml:"require,omitempty"`     // 硬约束，如 tool_call / json_mode
	Prefer     []Capability `yaml:"prefer,omitempty"`      // 软约束，不满足则降级 + 记审计
	MinContext int          `yaml:"min_context,omitempty"` // 最小上下文窗口
	RungStart  RungID       `yaml:"rung_start,omitempty"`  // 覆盖项目默认起始级；"" = 用默认
}

// SamplingParams 是采样参数。**不含 model_id**——模型由阶梯决定。
// 这个边界判断标准：能否在不破坏任务一致性的前提下热切换（Part 6.9）。
//
// 本结构同时充当配置结构与运行时请求参数（设计文档 Part 6.4 即以 yaml tag
// 定义它），因此字段名与 tag 都是对外契约。
//
// 零值契约与陷阱：
//   - Temperature/TopP 零值是**合法取值**（0 = 贪心/确定性采样），且与
//     "未设置"在语义上恰好一致，故无需指针。但配置作者必须知道自己写下的 0
//     意味着贪心解码，而不是"请用默认值"。
//   - TopK 零值与 MaxTokens 零值均表示"不发送该字段"（各厂商对 0/-1/缺省的
//     解释不一致，框架统一为"零值即不下传"，由 Normalizer 负责转换）。
//   - TimeoutMs 零值**非法**：它是整数毫秒，0 会造成"请求立刻超时"这类
//     看似网络故障的错误。加载期必须校验 > 0。
//
// [权衡: 类型保持 int64 而非 time.Duration（与设计文档一致）。原因：本结构是
// 配置结构，配置层用整数毫秒是行业惯例（YAML/JSON 都没有 duration 类型）且
// 人可读；命名里保留 Ms 后缀是刻意的"单位提醒"。运行时的 time.Duration 换算
// 集中在 Profile→CanonicalRequest 适配点一处完成，不让单位换算散落全项目。
// 上一轮曾误改为 time.Duration 却保留 Ms 后缀——那是"名字说毫秒、类型是纳秒"
// 的最坏组合，已修正。]
//
// [偏离文档: Temperature/TopP 取 float64 而非文档写的 float32。理由：与
// encoding/json 的默认数值类型一致，避免每个调用点写 float32(0.7) 的转换
// 噪音；float32 的内存收益在"数量级为个位数"的配置对象上可忽略。]
type SamplingParams struct {
	Temperature float64 `yaml:"temperature"`
	TopP        float64 `yaml:"top_p"`
	TopK        int     `yaml:"top_k"`
	MaxTokens   int     `yaml:"max_tokens"`
	TimeoutMs   int64   `yaml:"timeout_ms"` // 默认 1800000（30 分钟）
}

// ThinkingSpec 是思维链规格。Level 是模型原生的字符串档位，
// 不全局枚举——挡位数由 models.yaml 决定（Patch 1 字符串化）。
//
// 零值契约：Level 零值 "" 表示"不指定，用模型默认档位"，与 "off"（显式关闭
// 思考）不是一回事——把前者当后者会静默关掉思考能力。Budget 用**指针**：
// nil = "未设置"，&0 = "显式要求 0 预算"，二者在计费与降级决策上完全不同，
// 这是文档刻意用 *int 的原因（与 LogEntry.TokenActual 同一手法）。
type ThinkingSpec struct {
	Level   string          `yaml:"level,omitempty"`  // 如 "off"/"on"/"low"/"high"，按模型枚举
	Budget  *int            `yaml:"budget,omitempty"` // 仅 budget 控制模型使用；nil = 不设置
	Display ThinkingDisplay `yaml:"display"`
}

// ThinkingDisplay 控制思维链的落点（Part 3.2 Audit 三档的下游）。
//
// 零值语义：两个 bool 零值都是 false，即"不进 Log、不占上下文"——这是安全
// 方向（不泄露思维链、不占 token），但同时也意味着"忘记配置 = 静默丢弃思考
// 过程"，而思考过程往往是调试与审计最需要的证据。因此加载器在 Level 非 "off"
// 时应显式写入本结构，不依赖零值。
type ThinkingDisplay struct {
	LogThinking  bool `yaml:"log_thinking"`   // 思维链进 Log（Role=thinking）
	ExposeToView bool `yaml:"expose_to_view"` // 是否占上下文（false = audit-only）
}

// TaskPolicy 描述一个任务的预算口径。
type TaskPolicy struct {
	Budget       Budget `yaml:"budget"`
	OutputFormat string `yaml:"output_format,omitempty"` // 如 "code_with_review_notes"；不枚举
}

// Budget 是任务级预算（Part 6.4）。与 SamplingParams.MaxTokens 的区别是作用域：
// 前者是**整任务累计**上限（跨多次 LLM 调用），后者是**单次请求**上限。
//
// 零值契约：Budget{} 合法，表示"不限制"（如探索性任务）。MaxTokens 零值即
// "不限制"。但 TimeoutMs 零值是陷阱——任务级看门狗会立刻判定超时；加载期
// 必须校验为正，或由加载器显式填入默认值，绝不能让 0 传到运行时。
type Budget struct {
	MaxTokens int   `yaml:"max_tokens"` // 任务级总预算
	TimeoutMs int64 `yaml:"timeout_ms"` // 任务级超时
}

// ContextPolicy 是父 Agent 向子 Agent 传上下文时的过滤策略（Part 9.5）。
//
// 零值契约：ContextPolicy{}（IncludeRoles 为空）表示**不传任何上下文**——
// 这是最保守的默认：子 Agent 从零开始，不会意外继承父 Agent 的敏感轨迹。
// MaxEntries/MaxTokens 为零同样表示"不传"。三个字段都是"越空越安全"的方向，
// 与 ViewItem.Visible 那类"零值导致静默丢数据"的陷阱方向相反。
type ContextPolicy struct {
	IncludeRoles []InternalRole `yaml:"include_roles,omitempty"` // 哪些角色可传
	MaxEntries   int            `yaml:"max_entries,omitempty"`
	MaxTokens    int            `yaml:"max_tokens,omitempty"`
}

// ProfileSummary 是 Profile 加载结果的轻量摘要（Part 6.12）。
type ProfileSummary struct {
	ID          ProfileID
	Description string
	Extends     ProfileID
	PromptID    string
}

// PromptEntry 是提示词索引条目（Part 6.12）。
type PromptEntry struct {
	ID          string
	Path        string
	Description string
}

// ProfileLoader 是 Profile 加载器接口（Part 6.12）。
type ProfileLoader interface {
	LoadAll(profilesDir string) error
	Get(id ProfileID) (*Profile, error)
	List() []ProfileSummary
	// Reload 热重载，返回变化的 ID 列表。运行中的 Agent 不受影响，
	// 新 fork 的 Agent 用新 Profile。
	Reload() ([]ProfileID, error)
}

// PromptIndex 提供提示词目录的查询。
type PromptIndex interface {
	List() []PromptEntry
	ReadPrompt(id string) (string, error) // 返回整个 md 文件内容
}
