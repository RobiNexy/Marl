package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"marl/internal/types"
)

// WireMessage 是线路协议里的一条消息（正常化的产物，Part 10.9）。
// 它已经按目标协议的硬规则排布好角色顺序。
//
// 不变量（Assert 必须逐条检查，因为违反它们的请求往往"能跑但语义错"）：
//   - Role.Valid()（零值非法：线路角色必须显式，不能靠默认值兜底）；
//   - ToolCalls 非空 ⇒ Role == WireAssistant（工具调用只能由 assistant 发起）；
//   - ToolCallID 非空 ⇒ Role == WireTool（配对 id 只属于 tool 消息）；
//   - 至少 Content 非空或 ToolCalls 非空（两者皆空的消息没有任何信息，
//     却会占一个角色槽位并破坏严格协议的 role 交替规则）；
//   - CacheControl 非空 ⇒ 目标线路的缓存模式是 CacheExplicitBreakpoint。
//     反之（显式断点协议却没打断点）不报错，但缓存永不命中——Builder 侧
//     必须按缓存模式主动打断点，这一点由 Normalizer 的测试守护。
type WireMessage struct {
	Role        types.WireRole
	Content     string
	Attachments []types.Attachment
	ToolCalls   []types.ToolCall
	ToolCallID  string // tool 消息与 tool_call 的配对
	// CacheControl 是显式断点缓存的标记（CacheExplicitBreakpoint 协议使用），
	// 如 "ephemeral"；隐式前缀缓存协议为空。
	CacheControl string
}

// WireRequest 是 Normalizer 的产出：一条合法的厂商消息数组 + 调用参数。
// 每个 Wire 实现自带 Assert(WireRequest) 校验协议硬规则（Part 10.9）。
type WireRequest struct {
	Endpoint string
	Model    string
	Wire     types.WireID
	Messages []WireMessage
	Tools    []ToolDef
	Sampling types.SamplingParams
	// Thinking 是已翻译成协议形态的思维参数。**类型由具体线路决定**：
	// 本线路（OpenAI 兼容）用 openAIChatThinking（对象信封 + 顶层 reasoning_effort，
	// 见 ADR-0020），因为它的两个字段落在请求体的两个层级上；一个 map 表达不了
	// "这个键该放顶层"。
	//
	// 类型是 any 是刻意的：三种控制方式互斥，用具体类型会逼出一种"总是带上全部
	// 字段"的结构，而厂商对多余字段常常直接报 400。代价是类型安全为零，
	// 因此 Assert 必须显式检查它属于本协议允许的形态（类型与 ThinkingControl
	// 声明不符时必须在这里炸，而不是等厂商返回 400 才被当作 capability 错误），
	// 编码器遇到未知类型也必须**报错而不是丢弃**（静默丢弃 = 思维参数没发出去，
	// 而调用方以为发过了）。
	Thinking any
	Prefill  *string
	// CacheBucket 是请求级缓存隔离键，永远等于 binding.CacheBucket（= AgentID）。
	// 通过请求的 user 字段落地（Patch 1，Part 10.15）。
	//
	// 类型是 types.AgentID 而不是 string：这是"缓存串味"风险的唯一防线——
	// 两个 Agent 的请求落进同一个缓存桶不会报错，只会让输出互相污染，
	// 而且症状（偶发的不相干回答）极难归因。命名类型让"从别处拿了个
	// 字符串塞进来"在编译期就被挡住。
	CacheBucket types.AgentID
	// ResponseFormat 是协议形态的输出格式声明（阶段 1 补，见 ADR-0017）。
	// 取值来自 OutputFormat* 常量；空串 = 不发送（厂商默认文本输出）。
	//
	// 为什么类型是 string 而不是 any：OpenAI 兼容协议的形态是封闭的两个
	// （text / json_object），而 Thinking 之所以用 any 是因为三种控制方式的
	// **参数形状**不同。这里没有这个问题。
	ResponseFormat OutputFormat
}

// OutputFormat 是协议形态的输出格式（OpenAI 兼容的 response_format.type）。
//
// 零值契约：零值 OutputFormat("") 是**合法**取值，含义是"不声明输出格式"
// （不发送该字段，走厂商默认）。与 SamplingParams 的 TopK/MaxTokens 同一
// 取舍：OpenAI 的默认是 "text"，显式发送 "text" 与不发送在厂商侧等价，
// 但"不发送"能让请求体在字节层面更稳定（少一个字段 = 少一处可能变化的字节）。
type OutputFormat string

const (
	// OutputFormatNone 表示不声明输出格式（零值）。
	OutputFormatNone OutputFormat = ""
	// OutputFormatJSONObject 要求输出合法 JSON（response_format={"type":"json_object"}）。
	OutputFormatJSONObject OutputFormat = "json_object"
)

// Valid 报告 f 是否为本线路可发送的输出格式。
//
// 注意 "text" **不在**合法值里：它是零值的语义（不发送），如果允许显式
// "text"，同一语义就有两种编码方式，字节稳定与 golden 测试都会变得含糊。
//
// 并发：纯函数。
func (f OutputFormat) Valid() bool {
	switch f {
	case OutputFormatNone, OutputFormatJSONObject:
		return true
	}
	return false
}

// NormalizePolicy 是角色布局与裁剪策略（Part 10.9）。
//
// 作用域是 endpoint，可被 (endpoint, model) 覆盖，**不下放到 Agent 或 Profile**——
// 它是部署属性，不是任务属性。
type NormalizePolicy struct {
	// Roles 是内部角色 → 线路角色的映射覆盖；未覆盖项用 Part 3.4 的默认表。
	// nil 合法（= 全用默认表）。
	//
	// [错位说明（阶段 1 发现，见 ADR-0018）: 本字段的键是 types.InternalRole，
	// 而 Normalizer 的输入 Segment 只有 (Kind, Speaker)——InternalRole 在
	// 编译层就被折叠掉了（Segment 不变量表里没有它）。因此本字段由**编译层**
	// 消费，Normalizer 用的是 (Kind, Speaker) → WireRole 的默认表。留着它
	// 而不是删掉，是因为编译层（阶段 2）确实需要这张覆盖表，而它属于同一个
	// "策略集"概念；删掉会让阶段 2 再补一次字段。]
	Roles map[types.InternalRole]types.WireRole
	// Trimmable 声明哪些内部角色在 Token 紧张时可被裁剪。
	// nil 合法（= 无项可裁，最保守）。
	Trimmable map[types.InternalRole]bool
	// SystemPosition 决定 system 段位置（0 = 最前，-1 = 不限）。
	//
	// 零值 = 0 = "最前"，这是**有意义的值**（不是"未配置"）：
	// 本字段没有"未设置"的表示，配置层必须显式给出 -1 才能表达"不限"。
	// 若某天需要区分"未配置"，应改成指针而不是复用 0。
	SystemPosition int
	// RequireAlternation 表示该协议要求 user/assistant 严格交替。
	// 与 SystemPosition 不同，false 是安全的默认（多数协议容忍连续同角色）。
	RequireAlternation bool
	// ToolsFollowSystem 表示 tools 字段必须紧跟 system（部分协议硬要求）。
	ToolsFollowSystem bool

	// 以下四个字段是 Part 10.9 的具名策略（阶段 1 补，见 ADR-0018）。
	//
	// 零值 "" 的语义是**"不覆盖，用目标线路的内置默认策略"**，而不是某个
	// 具体策略名。这条很重要：策略名写错时（配置笔误）不会静默挑一个布局，
	// 而是在 Validate 期直接报错；而空 NormalizePolicy{} 仍然可用
	// （LoadPolicyOverride 的契约要求"空策略不得凭空改变请求布局"）。

	// MultiSystem 决定多条 system 段如何承载。
	// 取值：MultiSystemConcatAll / MultiSystemFirstOnlyRestAsUser /
	// MultiSystemPrependConcat。
	MultiSystem string
	// ConsecutiveSame 决定连续同角色消息如何承载。
	// 取值：ConsecutiveSameKeepAsIs / ConsecutiveSameMergeWithSeparator /
	// ConsecutiveSameInterleaveEmpty。
	ConsecutiveSame string
	// TailAssistant 决定尾部 assistant（prefill）如何承载。
	// 取值：TailAssistantNativePrefill / TailAssistantDemoteToTailHint /
	// TailAssistantDrop。
	TailAssistant string
	// ToolResultRole 决定 tool_result 段的承载角色。
	// 取值：ToolResultNativeRole / ToolResultInlineAsUser。
	ToolResultRole string

	// HistoricalThink（Part 10.9 的第五个决策点）**故意没有对应字段**：
	// 它的取值作用于"历史思维链段"，而 Segment 无法表达"这是一条思维链"
	// （没有 InternalRole，也没有 Thinking 标记）。在 Normalizer 层实现它
	// 只能靠猜，所以剥离历史思维链的责任归编译层（它读得到 InternalRole）。
	// 详见 ADR-0018；不要为了让字段表"看起来完整"而加一个填不进也读不出的字段。
}

// NormalizeResult 是 Normalizer 的产出（Part 10.10）。
//
// 零值契约：Degradations 为空是**合法**且期望的（无降级）；因此消费侧不能
// 用"有没有降级"来判断 Normalize 是否成功，必须看 error。Messages 为空
// 才是异常（没有任何消息的请求一定会被厂商拒绝）。
type NormalizeResult struct {
	Messages     []WireMessage
	Degradations []Degradation
}

// Degradation 是显式的能力降级记录（Part 10.10）。
// 能力缺失从来不静默：prefill 要不到、thinking 要不到、参数被剔除，都必须报出来。
//
// 不变量：Kind.Valid()、Reason 非空。From/To 允许为空——语义随 Kind 而变
// （如 param_stripped 的 From 是要剔除的参数名、To 为空；thinking_level 的
// From/To 是档位名）。这种"不统一的字段语义"是刻意的：新增降级种类时
// 无需改动结构，因此 Reason 才必须非空——它承担"人能读懂发生了什么"的职责。
//
// 降级记录是审计材料（原则：能力缺失从来不静默）。丢弃它们等于让"为什么
// 这次输出质量掉了"永远查不出来。
type Degradation struct {
	Kind   DegradationKind
	From   string
	To     string
	Reason string
}

// DegradationKind 是降级种类（Part 10.10）。
//
// 零值契约：零值 DegradationKind("") 非法。降级记录会被写进审计并被统计
// （"哪个模型最常降级"），零值会让这类统计出现一个没有名字的桶，
// 而它恰恰是"忘了填 Kind"的痕迹——必须让它显式失败。
type DegradationKind string

const (
	DegradPrefillUnavailable  DegradationKind = "prefill_unavailable"
	DegradParamStripped       DegradationKind = "param_stripped"
	DegradThinkingUnavailable DegradationKind = "thinking_unavailable"
	DegradThinkingLevel       DegradationKind = "thinking_level" // 请求档位超出模型支持
	DegradVisionUnavailable   DegradationKind = "vision_unavailable"
	DegradDetailStripped      DegradationKind = "detail_stripped"
)

// Valid 报告 k 是否为已定义降级种类。零值返回 false。
//
// 前向兼容：新增种类时，旧消费方遇到未知 Kind 应**原样记录并告警**，
// 而不是丢弃——降级的全部意义就是可见性。
// 并发：纯函数。
func (k DegradationKind) Valid() bool {
	switch k {
	case DegradPrefillUnavailable, DegradParamStripped, DegradThinkingUnavailable,
		DegradThinkingLevel, DegradVisionUnavailable, DegradDetailStripped:
		return true
	}
	return false
}

// Normalizer 负责把 CanonicalRequest 按 binding 的策略集翻译成 WireRequest
// （Part 10.10）。它决定角色布局、按协议的缓存模式打断点、按模型能力剔除
// 不支持的参数。
type Normalizer interface {
	// Wires 声明本实现支持的线路（返回**副本**，理由同 Denormalizer.Wires）。
	Wires() []types.WireID

	// Normalize 生成线路消息序列 + 降级清单。
	//
	// 契约：
	//   - 保序：不得跨 types.Stability 边界重排，也不得把 volatile 段挪到
	//     stable 之前（Part 10.3 的硬不变量——违反即破缓存，而缓存是成本命脉）；
	//   - 完整性：CanonicalRequest 里的每一个 Segment 要么出现在 Messages 中、
	//     要么在 Degradations 里有一条对应记录（含被裁剪的）。静默丢弃是最坏
	//     的情况：模型看不到某段上下文，却没有任何痕迹说明它曾存在；
	//   - CacheBucket 必须原样来自 binding，不得重新推导（见 WireRequest 注释）；
	//   - 不修改入参 req（同一 CanonicalRequest 会因 Binding 变化被重复归一化，
	//     改坏它会污染下一次）。
	//
	// 失败：能力硬约束不满足（如 Require tool_call 但模型不支持）→ 错误，
	// 因为这时任何产出都会被厂商拒绝；软约束不满足 → 不报错，走 Degradations。
	Normalize(req *CanonicalRequest, binding types.Binding) (*NormalizeResult, error)

	// Assert 校验编码该协议的硬规则（角色交替、tool 配对、system 位置、
	// 结尾角色限制）。产出必跑断言：策略配错在启动自检时就炸，不是任务跑到一半炸。
	//
	// 断言失败的性质是**框架/配置错误**，不是 Agent 错误：消息一律要指向
	// "哪条策略配错了"，不要回传给 LLM（它无从修正），也不要计入升级证据。
	Assert(req *WireRequest) error
}

// LoadPolicyOverride 读取 (endpoint, model) 级的策略覆盖（Part 10.9）。
//
// [阶段 1 状态: 仍是占位 panic。原因：覆盖来自配置文件（endpoints.yaml /
// models.yaml 的 policy 覆盖段），而配置层（加载 + 校验 + KeyRef 解析）不在
// 阶段 1 的范围内（见 13.3 的"不做"清单）。Normalizer 不调用它，而是直接用
// DefaultOpenAIChatPolicy()。**不要**为了让 Normalize 能跑通而在这里返回
// nil：那会让"配置里写了覆盖但没生效"变成静默行为（ADR-0014 的静默失效）。
// 配置层落地时一并实现本函数。]
//
// 返回 nil 表示**没有覆盖**（合法且常见），调用方此时使用内置默认策略。
// 因此 nil 不是错误，实现不得为了"避免 nil"而返回空策略对象——
// 空的 NormalizePolicy 里 SystemPosition=0 会强制 system 打头，
// 这与默认表的行为未必一致，等于凭空改变了未配置模型的请求布局。
func LoadPolicyOverride(endpoint, model string) *NormalizePolicy {
	panic("TODO(phase 1): 配置层落地后实现（见函数注释）")
}

// ---------------------------------------------------------------------------
// 以下是阶段 1 的实现：OpenAI 兼容线路（types.WireOpenAIChat）的去程翻译。
// ---------------------------------------------------------------------------

// CapsProvider 是 Normalizer 消费侧所需的最小能力查询（接口隔离）。
//
// 只声明 EffectiveCaps：*wire.Catalog（阶段 4 落地）天然满足本接口，
// 而本层的测试只需要一个假实现。不直接把 Catalog 作为依赖的理由是它另有
// Model/Endpoint/Ladder/Pricing 三个方法与去程翻译无关——把它们带进依赖，
// 每个测试桩都要多实现三个方法，而这是纯粹的噪音。
//
// 失败：模型未知 → 错误。**不得**回落成"没有能力"的空 ModelCaps：
// 空 ModelCaps 的 MaxContext=0 会被读成"上下文为零"，
// 而真正的问题是配置里没有这个模型（见 Catalog.EffectiveCaps 契约）。
type CapsProvider interface {
	EffectiveCaps(modelID, endpoint string) (ModelCaps, error)
}

// 具名策略取值（Part 10.9）。这些字符串是配置层的对外词汇：
// 改名即破坏配置文件，与 Profile 的 yaml tag 同级别，改动需 ADR。
//
// 支持矩阵见 supportedOpenAIChatStrategy：本线路只实现其中一部分，
// 不支持的取值在构造期报错，而不是静默降级成默认策略。
const (
	MultiSystemConcatAll           = "concat_all"
	MultiSystemFirstOnlyRestAsUser = "first_only_rest_as_user"
	MultiSystemPrependConcat       = "prepend_concat"

	ConsecutiveSameKeepAsIs           = "keep_as_is"
	ConsecutiveSameMergeWithSeparator = "merge_with_separator"
	ConsecutiveSameInterleaveEmpty    = "interleave_empty"

	TailAssistantNativePrefill    = "native_prefill"
	TailAssistantDemoteToTailHint = "demote_to_tail_hint"
	TailAssistantDrop             = "drop"

	ToolResultNativeRole   = "native_tool_role"
	ToolResultInlineAsUser = "inline_as_user"
)

// thinkingOnLevel / thinkingOffLevel 是**框架层**的开关档位名。
//
// 它们不是全局枚举的档位（Patch 1 已把档位字符串化，档位数由 models.yaml
// 决定），而是"开关型控制"（ThinkControlBool）的通用词汇：
// 各厂商的开关档位名不同（enabled/disabled、on/off、true/false），
// 由 Normalizer 翻译（见 normalizeOpenAIChatThinking）。
const (
	thinkingOnLevel  = "on"
	thinkingOffLevel = "off"
)

// 本线路协议里的思维取值常量（照抄 docs/deepseek-api/chat-complete.html 的
// reasoning_effort 与 thinking.type 枚举）。
//
// reasoningEffortOff 值得单列：它说明"关闭思考"在本线路有**两个**名字——
// 开关控制的 "disabled"、强度控制的 "none"。把框架的 off 映射到哪个，取决于
// caps 声明的 ThinkingControl（见 normalizeOpenAIChatThinking）。
const (
	thinkingTypeEnabled  = "enabled"
	thinkingTypeDisabled = "disabled"
	reasoningEffortOff   = "none"
)

// Validate 报告策略集是否自洽：合法值来自各具名策略的取值集合，
// 非空但未识别的值一律报错（配置笔误必须在这里炸）。
//
// 不检查：SystemPosition 的具体取值（它由协议与缓存前缀位置决定，
// 不是有限枚举；只要求 >= -1）。
//
// 并发：纯函数。
func (p NormalizePolicy) Validate() error {
	checks := []struct {
		name   string
		value  string
		values []string
	}{
		{"MultiSystem", p.MultiSystem, []string{MultiSystemConcatAll, MultiSystemFirstOnlyRestAsUser, MultiSystemPrependConcat}},
		{"ConsecutiveSame", p.ConsecutiveSame, []string{ConsecutiveSameKeepAsIs, ConsecutiveSameMergeWithSeparator, ConsecutiveSameInterleaveEmpty}},
		{"TailAssistant", p.TailAssistant, []string{TailAssistantNativePrefill, TailAssistantDemoteToTailHint, TailAssistantDrop}},
		{"ToolResultRole", p.ToolResultRole, []string{ToolResultNativeRole, ToolResultInlineAsUser}},
	}
	for _, c := range checks {
		// 空值 = 不覆盖（用线路默认），合法。
		if c.value == "" {
			continue
		}
		ok := false
		for _, v := range c.values {
			if c.value == v {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("wire: NormalizePolicy.%s=%q 不是已定义策略（合法取值 %v）；空值才表示\"用线路默认\"",
				c.name, c.value, c.values)
		}
	}
	if p.SystemPosition < -1 {
		return fmt.Errorf("wire: NormalizePolicy.SystemPosition=%d 非法（-1 = 不限位置，0 = 最前）", p.SystemPosition)
	}
	return nil
}

// DefaultOpenAIChatPolicy 返回 OpenAI 兼容线路的内置默认策略（Part 10.9）。
//
// 与文档的差异（只有一处，仍是默认值层面的偏离）：
//
//	[偏离文档 10.9: 文档给的 TailAssistant 默认是 native_prefill，
//	阶段 1 用 demote_to_tail_hint。三条理由：(1) native_prefill 需要把
//	"prefix": true 挂在最后一条 assistant 消息上，而 WireMessage 没有这个
//	字段（加字段需 ADR）；(2) 该能力只在 /beta 接入点可用，而 deepseek-main
//	的 BaseURL 不是 /beta（docs/deepseek-api/prefix-complete.html）；
//	(3) 13.3 的六个探测用例没有覆盖 prefill —— 没有实测证据就给一个默认
//	行为涉及"猜厂商语义"。待 prefill 探测用例落地、实测确认后再改回
//	native_prefill（改动点只有本函数一行 + 一个测试）。]
func DefaultOpenAIChatPolicy() NormalizePolicy {
	return NormalizePolicy{
		MultiSystem:        MultiSystemConcatAll,
		ConsecutiveSame:    ConsecutiveSameKeepAsIs,
		TailAssistant:      TailAssistantDemoteToTailHint,
		ToolResultRole:     ToolResultNativeRole,
		SystemPosition:     0,
		RequireAlternation: false,
		ToolsFollowSystem:  false,
	}
}

// mergePolicy 用 over 覆盖 base 中"被显式设置"的字段。
//
// 覆盖规则分两类，不能一概而论：
//   - 字符串策略字段：空串 = 未覆盖（保留 base）；
//   - bool / int 字段（SystemPosition、RequireAlternation、ToolsFollowSystem）：
//     没有"未设置"的表示，一律以 over 为准。这与 SystemPosition 的契约一致
//     （"本字段没有'未设置'的表示，配置层必须显式给出 -1"）。
func mergePolicy(base, over NormalizePolicy) NormalizePolicy {
	if over.MultiSystem != "" {
		base.MultiSystem = over.MultiSystem
	}
	if over.ConsecutiveSame != "" {
		base.ConsecutiveSame = over.ConsecutiveSame
	}
	if over.TailAssistant != "" {
		base.TailAssistant = over.TailAssistant
	}
	if over.ToolResultRole != "" {
		base.ToolResultRole = over.ToolResultRole
	}
	base.SystemPosition = over.SystemPosition
	base.RequireAlternation = over.RequireAlternation
	base.ToolsFollowSystem = over.ToolsFollowSystem
	if over.Roles != nil {
		base.Roles = over.Roles
	}
	if over.Trimmable != nil {
		base.Trimmable = over.Trimmable
	}
	return base
}

// supportedOpenAIChatStrategy 检查策略取值在本线路是否已实现。
//
// 不做静默降级（全部在构造期报错）的三条理由：
//   - TailAssistantNativePrefill 需要把 "prefix": true 挂在最后一条 assistant
//     消息上，而 WireMessage 没有这个字段（加字段需 ADR），且只在 /beta 接入点
//     可用（docs/deepseek-api/prefix-complete.html）；
//   - ConsecutiveSameInterleaveEmpty 要插入空 assistant 消息，而 WireMessage
//     的不变量要求"Content 或 ToolCalls 至少一项非空"——它与本线路的不变量
//     直接冲突（它是给强制 role 交替的协议用的）；
//   - ToolResultInlineAsUser 是给"没有 tool role"的协议准备的，OpenAI 兼容线路
//     有原生 tool role（10.9 的 native_tool_role），把结果塞进 user 消息会让模型
//     失去 tool_call_id 配对信息。
//
// 并发：纯函数。
func supportedOpenAIChatStrategy(p NormalizePolicy) error {
	unsupported := []struct{ name, value, reason string }{
		{"TailAssistant", TailAssistantNativePrefill, "需要 WireMessage 的 prefix 标记与 /beta 接入点，阶段 1 未实现（见 DefaultOpenAIChatPolicy）"},
		{"ConsecutiveSame", ConsecutiveSameInterleaveEmpty, "会插入空消息，违反 WireMessage 的非空不变量"},
		{"ToolResultRole", ToolResultInlineAsUser, "本线路有原生 tool role；内联会把工具结果伪装成用户输入"},
	}
	for _, u := range unsupported {
		if p.field(u.name) == u.value {
			return fmt.Errorf("wire: normalizer: 策略 %s=%q 在 OpenAI 兼容线路上未实现：%s", u.name, u.value, u.reason)
		}
	}
	return nil
}

// field 按名字取策略字段值，供 supportedOpenAIChatStrategy 做表驱动检查
// （避免为每个策略写一个 getter）。
func (p NormalizePolicy) field(name string) string {
	switch name {
	case "MultiSystem":
		return p.MultiSystem
	case "ConsecutiveSame":
		return p.ConsecutiveSame
	case "TailAssistant":
		return p.TailAssistant
	case "ToolResultRole":
		return p.ToolResultRole
	}
	return ""
}

// OpenAICompatNormalizer 是 OpenAI 兼容线路（types.WireOpenAIChat）的去程翻译实现。
//
// 无状态：caps 与 policy 在构造期冻结，之后只读。Normalize/BuildRequest 可并发调用
// （同一实现会被多 Agent 共享）。
type OpenAICompatNormalizer struct {
	caps   CapsProvider
	policy NormalizePolicy
}

// 编译期检查：实现必须满足接口（契约实现的"assignment checkpoint"）。
var _ Normalizer = (*OpenAICompatNormalizer)(nil)

// NewOpenAICompatNormalizer 构造去程翻译器。override 为 nil 表示用内置默认策略
// （DefaultOpenAIChatPolicy）；非 nil 时按 mergePolicy 逐字段覆盖。
//
// 失败：caps 为 nil；策略名非法；策略取值在本线路未实现。三类都属于启动期的
// 装配/配置错误——必须在这里炸，而不是等第一个任务跑到一半炸。
func NewOpenAICompatNormalizer(caps CapsProvider, override *NormalizePolicy) (*OpenAICompatNormalizer, error) {
	if caps == nil {
		return nil, errors.New("wire: normalizer: caps 为 nil（没有能力查询就无从翻译档位、剔除不支持的参数）")
	}
	policy := DefaultOpenAIChatPolicy()
	if override != nil {
		policy = mergePolicy(policy, *override)
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if err := supportedOpenAIChatStrategy(policy); err != nil {
		return nil, err
	}
	return &OpenAICompatNormalizer{caps: caps, policy: policy}, nil
}

// Policy 返回生效的策略集。
//
// 用途是审计与探测报告：策略是请求字节的一部分，报告里缺了它，
// "两次请求字节不同"就无法归因（NormalizePolicy 是值类型，除 map 外无共享）。
func (n *OpenAICompatNormalizer) Policy() NormalizePolicy { return n.policy }

// Wires 声明本实现支持的线路（返回副本：调用方可能拼接/排序）。
func (n *OpenAICompatNormalizer) Wires() []types.WireID {
	return []types.WireID{types.WireOpenAIChat}
}

// Normalize 生成线路消息序列 + 降级清单（Normalizer 接口方法）。
func (n *OpenAICompatNormalizer) Normalize(req *CanonicalRequest, binding types.Binding) (*NormalizeResult, error) {
	_, res, err := n.build(req, binding)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// BuildRequest 装配完整的 WireRequest（= Normalize 的消息布局 + 工具表 /
// 采样 / 思维 / 输出格式的协议形态）。
//
// 为什么这不是接口方法：阶段 0 冻结的 NormalizeResult 只有 Messages +
// Degradations，而一次调用还需要 Thinking/Sampling/Tools/ResponseFormat 的
// 翻译结果——接口里没有它们的位置。两条备选都更差：
//   - 给 NormalizeResult 加 5 个字段：改动承重契约，且这些字段与"角色布局"
//     不是同一件事（一个是消息序列，一个是参数形态）；
//   - 让调用方自己装配：装配 thinking 需要模型能力（caps），等于把 Normalizer
//     的职责搬到每个调用点（主循环、压缩器、探测器各一份，且三份迟早不一致）。
//
// 本方法与 Normalize 共用同一个 build()，因此不存在"两条翻译路径"，
// 也就不存在两边字节不一致的可能。
func (n *OpenAICompatNormalizer) BuildRequest(req *CanonicalRequest, binding types.Binding) (*WireRequest, []Degradation, error) {
	wr, res, err := n.build(req, binding)
	if err != nil {
		return nil, nil, err
	}
	return wr, res.Degradations, nil
}

// build 是 Normalize 与 BuildRequest 共用的唯一实现路径。
//
// 步骤顺序有讲究（每一步只依赖前面的结果）：
//
//  1. 入参自检 → 2. 查能力 → 3. (Kind,Speaker) → WireRole 布局
//     → 4. 多条 system 折叠 → 5. prefill 降级 → 6. 连续同角色策略
//     → 7. thinking / 采样 / 输出格式的协议形态翻译 → 8. 产出前自检（Assert）。
//
// 第 8 步不是"可选的额外保险"：Assert 校验的是策略配错的场景（system 位置、
// tool 配对、悬空 tool_calls）。这些错误在厂商侧表现为 400 或"语义悄悄变了"，
// 而在这里表现为一条指向具体策略的启动期错误。
func (n *OpenAICompatNormalizer) build(req *CanonicalRequest, binding types.Binding) (*WireRequest, *NormalizeResult, error) {
	if req == nil {
		return nil, nil, errors.New("wire: normalizer: req 为 nil")
	}
	if binding.Endpoint == "" || binding.Model == "" {
		return nil, nil, fmt.Errorf("wire: normalizer: Binding 不完整（endpoint=%q model=%q）；零值 Binding 的请求会打到未定义的接入点",
			binding.Endpoint, binding.Model)
	}
	if binding.Wire != types.WireOpenAIChat {
		return nil, nil, fmt.Errorf("wire: normalizer: 本实现只支持线路 %q，binding.Wire=%q", types.WireOpenAIChat, binding.Wire)
	}
	if binding.CacheBucket == "" {
		return nil, nil, errors.New("wire: normalizer: Binding.CacheBucket 为空；请求级缓存桶的唯一来源是 AgentID（Patch 1），不允许重新推导")
	}
	if len(req.Segments) == 0 {
		return nil, nil, errors.New("wire: normalizer: CanonicalRequest.Segments 为空（没有任何消息的请求一定会被厂商拒绝）")
	}
	if req.Prefill != nil && *req.Prefill == "" {
		return nil, nil, errors.New("wire: normalizer: Prefill 为空串（\"要求模型以空开头\"是无意义请求，各厂商行为不一）")
	}

	caps, err := n.caps.EffectiveCaps(binding.Model, binding.Endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("wire: normalizer: 查询能力失败（model=%s endpoint=%s）: %w", binding.Model, binding.Endpoint, err)
	}

	var degr []Degradation

	msgs, err := layoutOpenAIChatMessages(req.Segments, n.policy.ToolResultRole)
	if err != nil {
		return nil, nil, err
	}

	if msgs, err = applyMultiSystem(msgs, n.policy.MultiSystem); err != nil {
		return nil, nil, err
	}

	if req.Prefill != nil {
		degr = append(degr, Degradation{
			Kind:   DegradPrefillUnavailable,
			From:   *req.Prefill,
			To:     n.policy.TailAssistant,
			Reason: "本线路阶段 1 未实现 prefill（见 DefaultOpenAIChatPolicy）：上层应把 prefill 改写为尾部 Transient 提示并记审计（10.10）",
		})
	}

	if msgs, err = applyConsecutiveSame(msgs, n.policy.ConsecutiveSame); err != nil {
		return nil, nil, err
	}

	thinking, ds, err := normalizeOpenAIChatThinking(req.Thinking, caps)
	if err != nil {
		return nil, nil, err
	}
	degr = append(degr, ds...)

	sampling, ds, err := stripUnsupportedParams(req.Sampling, caps)
	if err != nil {
		return nil, nil, err
	}
	degr = append(degr, ds...)

	responseFormat, ds := normalizeOutputFormat(req.OutputJSON, caps)
	degr = append(degr, ds...)

	wr := &WireRequest{
		Endpoint:       binding.Endpoint,
		Model:          binding.Model,
		Wire:           types.WireOpenAIChat,
		Messages:       msgs,
		Tools:          cloneToolDefs(req.Tools),
		Sampling:       sampling,
		Thinking:       thinking,
		Prefill:        nil, // 唯一合法值：prefill 已在上面记降级（见 ADR-0018）
		CacheBucket:    binding.CacheBucket,
		ResponseFormat: responseFormat,
	}
	if err := n.Assert(wr); err != nil {
		return nil, nil, err
	}
	return wr, &NormalizeResult{Messages: msgs, Degradations: degr}, nil
}

// stabilityForKind 给出 SegmentKind 不变量表要求的 Stability 档位序号
// （0=frozen / 1=stable / 2=volatile，与 StabilityRank 同口径）。
func stabilityForKind(k SegmentKind) (int, bool) {
	switch k {
	case SegSystem, SegStanding, SegKnowledge:
		return 0, true
	case SegTurn, SegToolResult:
		return 1, true
	case SegTransient:
		return 2, true
	}
	return -1, false
}

// stabilityNameForKind 返回不变量表要求的 Stability 名字（错误信息里给人看）。
func stabilityNameForKind(k SegmentKind) types.Stability {
	switch k {
	case SegSystem, SegStanding, SegKnowledge:
		return types.StabilityFrozen
	case SegTurn, SegToolResult:
		return types.StabilityStable
	}
	return types.StabilityVolatile
}

// roleForSegment 是 (Kind, Speaker) → WireRole 的默认映射表。
//
// Part 3.4 的"InternalRole → WireRole"折叠在编译层完成（InternalRole 在那里
// 被压成 (Kind, Speaker)），本表负责最后一跳。
//
// framework 说话方折叠进 user：框架注入的内容（子任务结果、上报、提醒）对模型
// 而言就是"用户说的话"。assistant 角色只能由模型自己的产出占用——把框架噪声
// 伪装成"模型自己说过的话"会污染 Replay 时的因果解读。
func roleForSegment(k SegmentKind, sp Speaker) types.WireRole {
	switch k {
	case SegSystem, SegStanding, SegKnowledge:
		return types.WireSystem
	case SegToolResult:
		return types.WireTool
	}
	switch sp {
	case SpeakerHuman, SpeakerFramework:
		return types.WireUser
	case SpeakerAssistant:
		return types.WireAssistant
	case SpeakerTool:
		return types.WireTool
	}
	return ""
}

// layoutOpenAIChatMessages 把 Canonical 片段翻译成线路消息，并校验
// Segment 的 (Kind, Speaker, Stability) 自洽性。
//
// 为什么在这里校验不变量表：不自洽的组合（如 tool_result 段的 Speaker 是
// assistant）说明编译层有 bug，而它们的后果是"发给模型的角色与语义不符"——
// 在厂商侧完全合法、不会报错，只会让模型行为莫名其妙。宁可在去程拒绝，
// 也不要发出一个语义错误的请求。
func layoutOpenAIChatMessages(segs []Segment, toolResultRole string) ([]WireMessage, error) {
	msgs := make([]WireMessage, 0, len(segs))
	for i := range segs {
		s := &segs[i]
		if !s.Kind.Valid() {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment 的 Kind=%q 未定义（零值不得被默认成 turn）", i, s.Kind)
		}
		if !s.Speaker.Valid() {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment（kind=%s）的 Speaker=%q 未定义（零值不得被默认成 assistant）", i, s.Kind, s.Speaker)
		}
		wantRank, ok := stabilityForKind(s.Kind)
		if !ok {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment 的 Kind=%q 未定义", i, s.Kind)
		}
		gotRank, ok := StabilityRank(s.Stability)
		if !ok {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment 的 Stability=%q 未定义（零值不得被解读为 frozen）", i, s.Stability)
		}
		if gotRank != wantRank {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment（kind=%s）的 Stability=%s 与不变量表不符（应为 %s）",
				i, s.Kind, s.Stability, stabilityNameForKind(s.Kind))
		}
		if s.ToolCallID != "" && s.Kind != SegToolResult {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment（kind=%s）带了 ToolCallID=%q：配对 id 只属于 tool_result 段", i, s.Kind, s.ToolCallID)
		}
		if len(s.ToolCalls) > 0 && !(s.Kind == SegTurn && s.Speaker == SpeakerAssistant) {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment（kind=%s speaker=%s）带了 ToolCalls：工具调用只能是 assistant 的回合产出", i, s.Kind, s.Speaker)
		}
		if len(s.Attachments) > 0 {
			// 附件翻译需要命名空间 Resolver + 图片编码（10.14），阶段 1 不做。
			// 报错而不是降级：这是**框架未实现**，不是"模型没有能力"。
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment 带 %d 个附件；阶段 1 未实现附件翻译（10.14 落地于阶段 2）", i, len(s.Attachments))
		}
		if s.Content == "" && len(s.ToolCalls) == 0 {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment（kind=%s）既无 Content 也无 ToolCalls：它不携带信息却会占一个角色槽位（编译器必须过滤空段或记降级）", i, s.Kind)
		}

		role := roleForSegment(s.Kind, s.Speaker)
		if s.Kind == SegToolResult && toolResultRole == ToolResultNativeRole {
			role = types.WireTool
		}
		if role == "" {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 个 Segment（kind=%s speaker=%s）没有对应的线路角色", i, s.Kind, s.Speaker)
		}
		msgs = append(msgs, WireMessage{
			Role:       role,
			Content:    s.Content,
			ToolCalls:  cloneToolCalls(s.ToolCalls),
			ToolCallID: s.ToolCallID,
			// CacheControl 留空：OpenAI 兼容线路是隐式前缀缓存（CacheImplicitPrefix），
			// 没有显式断点字段；显式断点由 Anthropic 线路的 Normalizer 打。
		})
	}
	return msgs, nil
}

// systemJoiner 是折叠多条 system 消息时的分隔符。
//
// 为什么必须有分隔符而不是直接拼接：每段内容自带 XML 标注、且框架承诺"逐字节
// 保留"，直接首尾相接会把两段粘成一个不存在的词（只在跨段边界处出错，极难发现）。
// 为什么是 "\n\n"：与 Markdown 段落边界一致，且不引入任何新的 XML 标注
// （新标注会给缓存前缀增加一种"框架产物"，让冻结前缀的 golden 测试更难维护）。
// 它属于布局字节的一部分——改动它会破所有 Agent 的缓存前缀，需与冻结前缀
// 一起评估（ADR-0015 的同一逻辑）。
const systemJoiner = "\n\n"

// applyMultiSystem 按策略承载多条 system 消息（10.9 的第一个决策点）。
//
// 恒等情形：0 条或 1 条 system 时三种策略行为一致，直接返回（不做无谓拷贝）。
//
// 发现 system 出现在非 system 之后时**报错**而不是"顺手挪到最前"：挪动会改变
// 缓存前缀的字节顺序，而这类改动不报任何错，只表现为命中率下降。
// 顺序问题应当由 SystemPosition 在编译期解决，不该在去程偷偷修复。
func applyMultiSystem(msgs []WireMessage, strategy string) ([]WireMessage, error) {
	systemCount := 0
	nonSystemSeen := false
	for i, m := range msgs {
		if m.Role != types.WireSystem {
			nonSystemSeen = true
			continue
		}
		systemCount++
		if nonSystemSeen {
			return nil, fmt.Errorf("wire: normalizer: 第 %d 条消息是 system，但它前面已有非 system 消息；system 段必须连续且在最前（SystemPosition=0）", i)
		}
	}
	if systemCount <= 1 {
		return msgs, nil
	}

	tail := msgs[systemCount:]
	contents := make([]string, 0, systemCount)
	for i := 0; i < systemCount; i++ {
		contents = append(contents, msgs[i].Content)
	}

	switch strategy {
	case MultiSystemConcatAll:
		out := make([]WireMessage, 0, len(msgs)-systemCount+1)
		out = append(out, WireMessage{Role: types.WireSystem, Content: strings.Join(contents, systemJoiner)})
		return append(out, tail...), nil

	case MultiSystemPrependConcat:
		// 与 concat_all 的唯一区别是顺序：后出现的 system 排在前面。
		// [推断: 10.9 只给了名字没给语义定义。"prepend + concat" 的字面读法
		// 就是"后段在前"。阶段 1 没有模型使用该取值（默认是 concat_all），
		// 因此这里是一处未经实测的布局实现——用到它的线路必须补 golden 测试。]
		reversed := make([]string, 0, len(contents))
		for i := len(contents) - 1; i >= 0; i-- {
			reversed = append(reversed, contents[i])
		}
		out := make([]WireMessage, 0, len(msgs))
		out = append(out, WireMessage{Role: types.WireSystem, Content: strings.Join(reversed, systemJoiner)})
		return append(out, tail...), nil

	case MultiSystemFirstOnlyRestAsUser:
		// 只保留第一条 system，其余降级为 user 消息（内容逐字节不变）。
		// 这是"协议只认第一条 system"（Anthropic）的解，不丢弃任何字节。
		out := make([]WireMessage, 0, len(msgs))
		out = append(out, msgs[0])
		for _, c := range contents[1:] {
			out = append(out, WireMessage{Role: types.WireUser, Content: c})
		}
		return append(out, tail...), nil
	}
	return nil, fmt.Errorf("wire: normalizer: MultiSystem 策略 %q 未定义（构造期应已拦截）", strategy)
}

// mergeSeparator 是合并连续同角色消息时的分隔符（理由同 systemJoiner）。
const mergeSeparator = "\n\n"

// applyConsecutiveSame 按策略承载连续同角色消息（10.9 的第二个决策点）。
//
// 默认 keep_as_is：OpenAI 兼容线路容忍连续同角色（多条 user / 多条 system），
// 合并会改变消息边界（进而改变模型看到的"轮次"），属于会改变行为的操作，
// 因此只在显式配置时才做。
func applyConsecutiveSame(msgs []WireMessage, strategy string) ([]WireMessage, error) {
	switch strategy {
	case ConsecutiveSameKeepAsIs:
		return msgs, nil
	case ConsecutiveSameMergeWithSeparator:
		out := make([]WireMessage, 0, len(msgs))
		for _, m := range msgs {
			if len(out) > 0 && canMergeSameRole(out[len(out)-1], m) {
				out[len(out)-1].Content += mergeSeparator + m.Content
				continue
			}
			out = append(out, m)
		}
		return out, nil
	}
	return nil, fmt.Errorf("wire: normalizer: ConsecutiveSame 策略 %q 未定义（构造期应已拦截）", strategy)
}

// canMergeSameRole 判断两条相邻消息能否合并。
//
// 四种不合并的情况，每一条都有具体后果：
//   - 角色不同：合并等于改写语义；
//   - 任一方是 tool 消息：每条 tool 消息与不同的 tool_call_id 配对，合并会丢配对；
//   - 任一方带 ToolCalls：合并会让工具调用的归属（哪条消息发起）变模糊；
//   - 任一方 Content 为空：合并会产生以分隔符开头的正文（多出无意义的换行）。
func canMergeSameRole(a, b WireMessage) bool {
	if a.Role != b.Role {
		return false
	}
	if a.Role == types.WireTool || b.Role == types.WireTool {
		return false
	}
	if len(a.ToolCalls) > 0 || len(b.ToolCalls) > 0 {
		return false
	}
	if a.ToolCallID != "" || b.ToolCallID != "" {
		return false
	}
	return a.Content != "" && b.Content != ""
}

// openAIChatThinking 是本线路思维参数的**协议形态**（ADR-0020）。
//
// 为什么是结构体而不是 map[string]any：DeepSeek 的思维控制分布在请求体的
// **两个不同层级**上（实测依据 docs/deepseek-api/thinking.html 与
// chat-complete.html 的参数表）：
//
//	开关：{"thinking": {"type": "enabled" | "disabled"}}   ← 对象信封内
//	强度：{"reasoning_effort": "none"|"low"|"high"|"max"}   ← 顶层字段
//
// 早期实现把两者都塞进 thinking 对象（{"thinking":{"type":"low"}}）。那是
// 想当然的形态：文档明说 reasoning_effort 既管开关又管强度（"none 关闭思考
// 模式；low/high/max 开启思考模式"），而 thinking 对象只认 enabled/disabled。
// 发错的后果是**静默失效**——档位请求了但强度还是默认，探测报告里表现为
// "每个档位的 reasoning_content 都一样长"，很容易被当成"模型就是这样"。
//
// 用类型承载协议形态，编码器才能把两部分放到各自的位置上；map 做不到这件事
// （它没有"这个键该放顶层"的信息）。零值 = 两个字段都不发送（"不指定"
// ≠ "关闭"，见 ThinkingSpec 的零值契约）。
type openAIChatThinking struct {
	// Type 是开关字段的取值（enabled / disabled）；空 = 不发送 thinking 对象。
	Type string
	// ReasoningEffort 是顶层强度字段的取值（none / low / high / max）；
	// 空 = 不发送 reasoning_effort。档位控制的取值来自 models.yaml 的
	// thinking_levels（由 caps 校验），不在本类型里做白名单——那里才是
	// "这个模型支持哪些档位"的唯一事实来源。
	ReasoningEffort string
	// BudgetTokens 仅 budget 控制使用：本线路（DeepSeek）没有预算字段，
	// 只有兼容网关认这个形态。保留它是因为删掉一个已发出的形态属于行为
	// 变更，而阶段 1 没有证据说它有害（DeepSeek 侧不会走到这一支：
	// caps 声明 ThinkControlLevel）。
	BudgetTokens *int
}

// normalizeOpenAIChatThinking 把 ThinkingSpec 翻译成本线路的协议形态（10.10）。
//
// 分支顺序及各自的后果（每一支都必须可观测，不允许静默）：
//   - 模型未声明 thinking 能力：只有 ""（不指定）与 "off"（关闭）能安全满足；
//     要求"开"则记 DegradThinkingUnavailable 并彻底移除参数——照发会被 400，
//     发空字段则被厂商静默忽略，两者都不该静默；
//   - Level 为空且无 Budget：不发送任何思维参数（"不指定" ≠ "关闭"，
//     见 ThinkingSpec 的零值契约）；
//   - Level 不在模型档位表里：记 DegradThinkingLevel 并回落为"不发送"
//     （= 模型默认档位）。**不**擅自挑一个最接近的档位：那会改变成本与输出，
//     而调用方会以为自己的档位生效了；
//   - budget 控制但未给 Budget：不发送、不记降级（"没有要求"与"要求了但拿不到"
//     不是一回事，10.9）。但若同时给了**档位**，那记一条 DegradThinkingLevel：
//     预算控制下没有档位字段，档位声明必须可见地消失，而不是看起来生效了；
//   - off 是**框架开关词**，不查档位表：它在三种控制方式下分别落成
//     disabled（bool / budget）与 none（level），见 levelOffValue。
func normalizeOpenAIChatThinking(spec types.ThinkingSpec, caps ModelCaps) (any, []Degradation, error) {
	if !caps.ThinkingControl.Valid() {
		return nil, nil, fmt.Errorf("wire: normalizer: 模型的 ThinkingControl=%q 未定义（这类 models.yaml 应在加载期被拒绝，见 ADR-0014）", caps.ThinkingControl)
	}
	level := spec.Level

	if !hasCapability(caps.Has, types.CapThinking) {
		if level == "" || level == thinkingOffLevel {
			return nil, nil, nil
		}
		return nil, []Degradation{{
			Kind:   DegradThinkingUnavailable,
			From:   level,
			Reason: "模型未声明 thinking 能力（models.yaml caps.has）：已移除思维参数，本次输出不含思维链",
		}}, nil
	}

	if level == "" && spec.Budget == nil {
		return nil, nil, nil
	}

	// 档位白名单只校验**模型档位**：框架的开关词 off 不属于档位表（它由各
	// 控制方式映射成自己的关闭取值，见 ThinkingControl 的三个分支），
	// 拿它去查 thinking_levels 会把"关闭思考"误判成"不支持的档位"。
	if level != "" && level != thinkingOffLevel && len(caps.ThinkingLevels) > 0 && !containsString(caps.ThinkingLevels, level) {
		return nil, []Degradation{{
			Kind:   DegradThinkingLevel,
			From:   level,
			Reason: fmt.Sprintf("请求档位不在模型的 thinking_levels %v 中：回落为模型默认档位（不发送该参数）", caps.ThinkingLevels),
		}}, nil
	}

	switch caps.ThinkingControl {
	case ThinkControlBool:
		// budget 在开关控制下没有落点（协议里没有这个字段）：显式记降级，
		// 而不是让配置里写的预算静默消失。
		budgetDegr := budgetStripped(spec, "ThinkingControl=bool")
		switch level {
		case thinkingOnLevel:
			return openAIChatThinking{Type: thinkingTypeEnabled}, budgetDegr, nil
		case thinkingOffLevel:
			return openAIChatThinking{Type: thinkingTypeDisabled}, budgetDegr, nil
		default:
			d := append(budgetDegr, Degradation{
				Kind:   DegradThinkingLevel,
				From:   level,
				Reason: "bool 控制只认 on/off 两个开关档位：回落为模型默认档位（不发送该参数）",
			})
			return nil, d, nil
		}

	case ThinkControlLevel:
		// 档位控制在本线路走**顶层** reasoning_effort（ADR-0020），不是
		// thinking 对象里的 type。off 用协议自己的关闭取值 none（文档：
		// "none 关闭思考模式"），而不是把 "off" 当档位名发出去——那会被
		// 厂商忽略（或 400），现象是"关了但还在思考"。
		budgetDegr := budgetStripped(spec, "ThinkingControl=level")
		if level == "" {
			return nil, budgetDegr, nil
		}
		if level == thinkingOffLevel {
			nowOff, degr := levelOffValue(caps.ThinkingLevels)
			if nowOff == "" {
				return nil, append(budgetDegr, degr...), nil
			}
			return openAIChatThinking{ReasoningEffort: nowOff}, budgetDegr, nil
		}
		return openAIChatThinking{ReasoningEffort: level}, budgetDegr, nil

	case ThinkControlBudget:
		if level == thinkingOffLevel {
			return openAIChatThinking{Type: thinkingTypeDisabled}, nil, nil
		}
		// level 在预算控制下没有落点：档位是"离散强度"的词汇，而这里唯一的强度
		// 表达是 token 数。静默丢掉它会让配置里写的档位看起来生效了（请求照发、
		// 参数照带，只是不是那一档），因此显式记一条降级。on 不记：它是框架开关词，
		// 而"要不要带预算"由 Budget 是否给出决定（见下）。
		var levelDegr []Degradation
		if level != "" && level != thinkingOnLevel {
			levelDegr = []Degradation{{
				Kind:   DegradThinkingLevel,
				From:   level,
				Reason: "模型是 ThinkingControl=budget：协议形态里只有 token 预算字段，档位声明被忽略（预算仍生效）",
			}}
		}
		if spec.Budget == nil {
			return nil, levelDegr, nil
		}
		if *spec.Budget < 0 {
			return nil, nil, fmt.Errorf("wire: normalizer: ThinkingSpec.Budget=%d 为负（预算必须非负；nil 才是\"未设置\"）", *spec.Budget)
		}
		budget := *spec.Budget
		return openAIChatThinking{Type: thinkingTypeEnabled, BudgetTokens: &budget}, levelDegr, nil
	}
	return nil, nil, fmt.Errorf("wire: normalizer: ThinkingControl=%q 未定义", caps.ThinkingControl)
}

// levelOffValue 给出档位控制下"关闭思考"的协议取值。
//
// 为什么要在 caps.ThinkingLevels 里查一次而不是直接写 "none"：能关掉思考的
// 取值是**模型属性**，不是线路常量（有的模型没有关闭档位，只有 low/high）。
// 直接发 "none" 给一个没有该档位的模型，结果是 400 或静默忽略，而调用方
// 以为已经关了——这正是 Degradation 存在的理由：宁可在请求体里少一个字段
// 并留下一条降级记录，也不要发出一个厂商不认的值。
func levelOffValue(levels []string) (string, []Degradation) {
	if containsString(levels, reasoningEffortOff) {
		return reasoningEffortOff, nil
	}
	return "", []Degradation{{
		Kind:   DegradThinkingLevel,
		From:   thinkingOffLevel,
		Reason: fmt.Sprintf("模型的 thinking_levels %v 里没有关闭档位 %q：无法在该控制方式下关闭思考，回落为模型默认档位（不发送该参数）", levels, reasoningEffortOff),
	}}
}

// budgetStripped 在模型不接受 token 预算时记录降级（Budget 为 nil 时无记录）。
func budgetStripped(spec types.ThinkingSpec, control string) []Degradation {
	if spec.Budget == nil {
		return nil
	}
	return []Degradation{{
		Kind:   DegradParamStripped,
		From:   "budget_tokens",
		Reason: "模型是 " + control + "：协议形态里没有 token 预算字段，预算声明被剔除（档位/开关仍然生效）",
	}}
}

// stripUnsupportedParams 按 models.yaml 的 caps.unsupported_params 剔除参数。
//
// 只能剔除 top_k / max_tokens：这两个的**零值语义就是"不发送"**
// （见 SamplingParams 契约），置零即等于剔除。temperature / top_p 做不到——
// SamplingParams 用值类型，零值是真取值（贪心 / 0.0），置零等于"换成一个
// 厂商会接受的错值"，比照发更糟。遇到这类声明一律报错，提示需要先把字段
// 改成指针（那是一次需 ADR 的契约改动）。
//
// 未识别的参数名同样报错：静默忽略 = "配置写了但不生效"（ADR-0014 的静默失效）。
func stripUnsupportedParams(s types.SamplingParams, caps ModelCaps) (types.SamplingParams, []Degradation, error) {
	out := s
	var degr []Degradation
	for _, name := range caps.UnsupportedParams {
		switch name {
		case "top_k":
			out.TopK = 0
		case "max_tokens":
			out.MaxTokens = 0
		case "temperature", "top_p":
			return s, nil, fmt.Errorf("wire: normalizer: caps.unsupported_params 声明剔除 %q，但 SamplingParams.%s 是值类型——"+
				"零值是合法取值（不是\"不发送\"），无法表达\"剔除\"。先把该字段改成指针（需 ADR）再声明", name, name)
		default:
			return s, nil, fmt.Errorf("wire: normalizer: caps.unsupported_params 含本线路无法剔除的参数名 %q"+
				"（只认 top_k / max_tokens）。声明了却剔除不掉 = 配置静默失效", name)
		}
		degr = append(degr, Degradation{
			Kind:   DegradParamStripped,
			From:   name,
			Reason: "models.yaml 声明该模型不接受此参数：Normalizer 必须剔除而不是照发（照发会被 400 或被静默忽略）",
		})
	}
	return out, degr, nil
}

// normalizeOutputFormat 把语义层的"要 JSON"翻译成协议形态（ADR-0017）。
//
// 能力缺失不静默：模型没有 CapJSONMode 时剔除 response_format 并记降级。
// 两种处置的差别必须让上层看见——照发会被 400（capability 错误），
// 不发则是"输出不保证是合法 JSON"（上层要么重试，要么自己容错）。
func normalizeOutputFormat(wantJSON bool, caps ModelCaps) (OutputFormat, []Degradation) {
	if !wantJSON {
		return OutputFormatNone, nil
	}
	if !hasCapability(caps.Has, types.CapJSONMode) {
		return OutputFormatNone, []Degradation{{
			Kind:   DegradParamStripped,
			From:   "response_format",
			Reason: "模型未声明 json_mode 能力（models.yaml caps.has）：剔除该参数，本次输出不保证是合法 JSON",
		}}
	}
	return OutputFormatJSONObject, nil
}

// hasCapability 报告能力集里是否包含 c。
//
// 用遍历而不是 map：能力集只有个位数元素，models.yaml 里就是列表
// （顺序即声明顺序，便于人读与 diff）；为查表再建一个 map 只会多一处
// 可能与列表不一致的状态。
func hasCapability(caps []types.Capability, c types.Capability) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// containsString 报告 s 是否在 list 中。
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// cloneToolDefs 返回工具表的副本（连 Parameters 的字节一起拷）。
//
// 为什么要连字节一起拷：ToolDef.Parameters 是 json.RawMessage（切片头指向
// 底层数组），而工具表属**冻结前缀**——调用方在别处改一个字节，会让缓存前缀
// 无声无息地变化（ADR-0015）。深拷是这里唯一安全的共享方式。
func cloneToolDefs(in []ToolDef) []ToolDef {
	if in == nil {
		return nil
	}
	out := make([]ToolDef, len(in))
	for i, t := range in {
		out[i] = ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  cloneRawMessage(t.Parameters),
		}
	}
	return out
}

// cloneToolCalls 返回工具调用列表的副本（同样深拷 Arguments）。
func cloneToolCalls(in []types.ToolCall) []types.ToolCall {
	if in == nil {
		return nil
	}
	out := make([]types.ToolCall, len(in))
	for i, c := range in {
		out[i] = types.ToolCall{ID: c.ID, Name: c.Name, Arguments: cloneRawMessage(c.Arguments)}
	}
	return out
}

// Assert 校验 OpenAI 兼容线路的硬规则（10.9 的"产出必跑断言"）。
//
// 断言失败的语义是**框架/配置错误**：错误信息必须指向具体的消息下标与字段，
// 不回传给 LLM（它无从修正），也不计入升级证据。
//
// 覆盖的规则与依据：
//   - 消息非空、角色合法、内容或工具调用至少一项非空（WireMessage 不变量）；
//   - tool_call_id 只属于 tool 消息；tool_calls 只属于 assistant 消息；
//   - **tool 配对完整**：每条 tool 消息必须对应更早的 assistant.tool_calls；
//     每个 assistant.tool_calls 的每个 id 都必须有 tool 消息配对，且二者之间
//     不得插入其他角色。依据 docs/deepseek-api/tool-call.html：Chat Completion
//     "不支持在对话中间插入工具调用"，悬空的 tool_calls 会被厂商拒绝——
//     而历史拼接出错时的现象往往只是"模型开始答非所问"；
//   - system 位置：SystemPosition==0 时 system 不得出现在非 system 之后；
//   - 显式断点标记不得出现：本线路是隐式前缀缓存（CacheImplicitPrefix），
//     带 CacheControl 的请求会把一个厂商不认的字段发出去；
//   - Thinking 形态：必须是 {"type": <string>} 的非空对象（见 thinkingObject）；
//   - prefill 不得出现：阶段 1 未实现（见 DefaultOpenAIChatPolicy）。
//
// 并发：纯函数。
func (n *OpenAICompatNormalizer) Assert(req *WireRequest) error {
	if req == nil {
		return errors.New("wire: assert: req 为 nil")
	}
	if req.Wire != types.WireOpenAIChat {
		return fmt.Errorf("wire: assert: 线路不匹配（req.Wire=%q，本实现=%q）：Normalizer 与 WireAdapter 的声明必须一致",
			req.Wire, types.WireOpenAIChat)
	}
	if req.Endpoint == "" || req.Model == "" {
		return fmt.Errorf("wire: assert: Endpoint/Model 为空（endpoint=%q model=%q）", req.Endpoint, req.Model)
	}
	if req.CacheBucket == "" {
		return errors.New("wire: assert: CacheBucket 为空；请求级缓存桶必须来自 AgentID（否则请求会落进不属于本 Agent 的桶）")
	}
	if !req.ResponseFormat.Valid() {
		return fmt.Errorf("wire: assert: ResponseFormat=%q 不是已定义的输出格式", req.ResponseFormat)
	}
	if req.Prefill != nil {
		return errors.New("wire: assert: 本线路阶段 1 不支持 prefill（需要 WireMessage 的 prefix 标记与 /beta 接入点）；策略应为 demote_to_tail_hint")
	}
	if len(req.Messages) == 0 {
		return errors.New("wire: assert: Messages 为空（没有任何消息的请求一定会被厂商拒绝）")
	}
	if err := assertOpenAIChatThinking(req.Thinking); err != nil {
		return err
	}
	if err := n.assertMessages(req.Messages); err != nil {
		return err
	}
	if n.policy.RequireAlternation {
		if err := assertNoConsecutiveSameRole(req.Messages); err != nil {
			return err
		}
	}
	return nil
}

// assertMessages 逐条校验消息，并用状态机校验 tool 配对。
//
// 用状态机而不是"建一张 id 表最后统一比对"的原因：错误信息要能指出**哪一条**
// 消息打断了工具调用的配对（那一条通常就是历史拼接 bug 的现场），
// 而"最后统一比对"只能报出"某些 id 没配对"。
func (n *OpenAICompatNormalizer) assertMessages(msgs []WireMessage) error {
	nonSystemSeen := false
	pending := map[string]bool{} // 当前未被配对的 tool_call id
	groupOpen := false           // 是否正处在 assistant.tool_calls 与其结果之间
	for i, m := range msgs {
		if !m.Role.Valid() {
			return fmt.Errorf("wire: assert: 第 %d 条消息的 Role=%q 未定义（线路角色必须显式，不能靠默认值兜底）", i, m.Role)
		}
		if m.Content == "" && len(m.ToolCalls) == 0 {
			return fmt.Errorf("wire: assert: 第 %d 条消息（role=%s）既无 Content 也无 ToolCalls（占一个角色槽位却不携带信息）", i, m.Role)
		}
		if m.CacheControl != "" {
			return fmt.Errorf("wire: assert: 第 %d 条消息带了 CacheControl=%q：本线路是隐式前缀缓存，没有显式断点字段", i, m.CacheControl)
		}
		if m.ToolCallID != "" && m.Role != types.WireTool {
			return fmt.Errorf("wire: assert: 第 %d 条消息（role=%s）带了 ToolCallID=%q：配对 id 只属于 tool 消息", i, m.Role, m.ToolCallID)
		}
		if m.Role == types.WireTool && m.ToolCallID == "" {
			return fmt.Errorf("wire: assert: 第 %d 条 tool 消息没有 ToolCallID：它无法与任何 tool_call 配对", i)
		}
		if len(m.ToolCalls) > 0 && m.Role != types.WireAssistant {
			return fmt.Errorf("wire: assert: 第 %d 条消息（role=%s）带了 ToolCalls：工具调用只能由 assistant 发起", i, m.Role)
		}
		for j, c := range m.ToolCalls {
			if c.Name == "" {
				return fmt.Errorf("wire: assert: 第 %d 条消息的第 %d 个 tool_call 没有 name（协议层 function.name 必填）", i, j)
			}
			if len(c.Arguments) == 0 || !json.Valid(c.Arguments) {
				return fmt.Errorf("wire: assert: 第 %d 条消息的第 %d 个 tool_call 的 arguments 不是合法 JSON（原文 %q）", i, j, string(c.Arguments))
			}
		}
		if m.Role == types.WireSystem {
			if n.policy.SystemPosition == 0 && nonSystemSeen {
				return fmt.Errorf("wire: assert: 第 %d 条消息是 system 但出现在非 system 之后（SystemPosition=0 要求 system 段在最前）", i)
			}
			continue
		}
		nonSystemSeen = true

		switch {
		case m.Role == types.WireAssistant && len(m.ToolCalls) > 0:
			if groupOpen && len(pending) > 0 {
				return fmt.Errorf("wire: assert: 第 %d 条消息开启了新的 tool_calls，但上一组还有 %d 个未配对的 id（悬空工具调用会被厂商拒绝）", i, len(pending))
			}
			pending = make(map[string]bool, len(m.ToolCalls))
			for j, c := range m.ToolCalls {
				if pending[c.ID] {
					return fmt.Errorf("wire: assert: 第 %d 条消息的第 %d 个 tool_call 的 id=%q 在同一组内重复", i, j, c.ID)
				}
				pending[c.ID] = true
			}
			groupOpen = true

		case m.Role == types.WireTool:
			if !groupOpen {
				return fmt.Errorf("wire: assert: 第 %d 条 tool 消息之前没有带 tool_calls 的 assistant 消息（悬空的工具结果）", i)
			}
			if !pending[m.ToolCallID] {
				return fmt.Errorf("wire: assert: 第 %d 条 tool 消息的 ToolCallID=%q 不在当前组的 tool_calls 里（错配）", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)

		default:
			if groupOpen && len(pending) > 0 {
				return fmt.Errorf("wire: assert: 第 %d 条消息（role=%s）打断了 assistant.tool_calls 与其结果：还有 %d 个 id 未配对（Chat Completion 不支持在对话中间插入工具调用）",
					i, m.Role, len(pending))
			}
			groupOpen = false
		}
	}
	if groupOpen && len(pending) > 0 {
		return fmt.Errorf("wire: assert: 结尾的 assistant.tool_calls 有 %d 个 id 没有对应的 tool 消息", len(pending))
	}
	return nil
}

// assertOpenAIChatThinking 校验协议形态的思维参数（ADR-0020）。
//
// 只校验形态、不校验取值：取值空间由 models.yaml 的档位表定义（字符串档位，
// 框架对档位数无感——Patch 1），在这里枚举取值等于把厂商词汇写进框架。
//
// 未知类型**报错**而不是放过：Assert 的价值就在于"产出必跑"时拦住那些
// 厂商会静默忽略的形态（例如把某种 map 塞进 Thinking，编码器无从知道该放到
// 哪一层，最后要么报错要么丢字段——两者都应该在发请求之前暴露）。
func assertOpenAIChatThinking(thinking any) error {
	switch t := thinking.(type) {
	case nil:
		return nil
	case openAIChatThinking:
		if t.Type == "" && t.ReasoningEffort == "" && t.BudgetTokens == nil {
			return errors.New("wire: assert: Thinking 是空的 openAIChatThinking（三个字段全空）：空形态等于没有思维参数，应当直接给 nil（否则\"没要求\"与\"要求了但翻译成空\"无法区分）")
		}
		if t.Type != "" && t.Type != thinkingTypeEnabled && t.Type != thinkingTypeDisabled {
			return fmt.Errorf("wire: assert: Thinking.Type=%q 不在本线路的枚举 {enabled, disabled} 里", t.Type)
		}
		return nil
	default:
		return fmt.Errorf("wire: assert: Thinking 的类型 %T 不是本线路的协议形态（openAIChatThinking）；"+
			"未知形态一律拒绝，因为编码器无法判断它该放在 thinking 对象里还是顶层字段上", thinking)
	}
}

// assertNoConsecutiveSameRole 校验严格交替（RequireAlternation=true 时启用）。
//
// tool 结果与其 calls 之间天然是"assistant → tool（可多条）"，不算违反交替：
// 严格交替针对的是 user/assistant 之间的对话轮次。
func assertNoConsecutiveSameRole(msgs []WireMessage) error {
	for i := 1; i < len(msgs); i++ {
		prev, cur := msgs[i-1], msgs[i]
		if prev.Role != cur.Role {
			continue
		}
		if cur.Role == types.WireTool || len(prev.ToolCalls) > 0 {
			continue
		}
		return fmt.Errorf("wire: assert: 第 %d 与第 %d 条消息角色相同（%s）：策略声明了 RequireAlternation", i-1, i, cur.Role)
	}
	return nil
}

// cloneRawMessage 拷贝 RawMessage 的字节（nil 保持 nil）。
func cloneRawMessage(in json.RawMessage) json.RawMessage {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
