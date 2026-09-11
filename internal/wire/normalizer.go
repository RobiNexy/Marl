package wire

import (
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
	// Thinking 是已翻译成协议形态的思维参数（bool / 档位字符串 / budget 数字）。
	// 类型是 any 是刻意的：三种形态互斥，用具体类型会逼出一种"总是带上全部
	// 字段"的结构，而厂商对多余字段常常直接报 400。代价是类型安全为零，
	// 因此 Assert 必须显式检查它属于本协议允许的形态（类型与 ThinkingControl
	// 声明不符时必须在这里炸，而不是等厂商返回 400 才被当作 capability 错误）。
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
}

// NormalizePolicy 是角色布局与裁剪策略（Part 10.9）。
//
// 作用域是 endpoint，可被 (endpoint, model) 覆盖，**不下放到 Agent 或 Profile**——
// 它是部署属性，不是任务属性。
type NormalizePolicy struct {
	// Roles 是内部角色 → 线路角色的映射覆盖；未覆盖项用 Part 3.4 的默认表。
	// nil 合法（= 全用默认表）。
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
// 阶段 0 只定签名。
//
// 返回 nil 表示**没有覆盖**（合法且常见），调用方此时使用内置默认策略。
// 因此 nil 不是错误，实现不得为了"避免 nil"而返回空策略对象——
// 空的 NormalizePolicy 里 SystemPosition=0 会强制 system 打头，
// 这与默认表的行为未必一致，等于凭空改变了未配置模型的请求布局。
func LoadPolicyOverride(endpoint, model string) *NormalizePolicy {
	panic("TODO(phase 0): placeholder")
}
