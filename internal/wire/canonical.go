package wire

import (
	"encoding/json"

	"github.com/RobiNexy/Marl/internal/types"
)

// SegmentKind 是 Canonical 片段的种类（Part 10.3）。
//
// 零值契约：零值 SegmentKind("") 非法，且**不得**被默认成 turn。
// SegmentKind 是 Stability 与缓存身份的唯一推导依据（见 Segment 不变量表），
// 给它一个"安全默认"等于让一条本该 frozen 的系统段退化成 stable，
// 后果是全项目共享缓存前缀静默失效、成本上升而无人察觉。
type SegmentKind string

const (
	SegSystem     SegmentKind = "system"      // system 前缀
	SegStanding   SegmentKind = "standing"    // 常驻块（preferences 编译产物）
	SegKnowledge  SegmentKind = "knowledge"   // 知识条目
	SegTurn       SegmentKind = "turn"        // 历史回合
	SegToolResult SegmentKind = "tool_result" // 工具结果
	SegTransient  SegmentKind = "transient"   // 尾部 volatile，不入 Log
)

// Valid 报告 k 是否为已定义片段种类。零值返回 false。
// 并发：纯函数。
func (k SegmentKind) Valid() bool {
	switch k {
	case SegSystem, SegStanding, SegKnowledge, SegTurn, SegToolResult, SegTransient:
		return true
	}
	return false
}

// Speaker 是片段内容的说话方（Part 10.3）。
//
// 零值契约：零值 Speaker("") 非法，且**不得**被默认成 assistant。
// 猜成 assistant 等于把一段来历不明的内容伪装成"模型自己说过的话"——
// 这既污染 Replay 时的因果解读，也给了注入内容一个可信外壳。
type Speaker string

const (
	SpeakerHuman     Speaker = "human"
	SpeakerAssistant Speaker = "assistant"
	SpeakerTool      Speaker = "tool"
	SpeakerFramework Speaker = "framework"
)

// Valid 报告 s 是否属于 Doc 的四种说话方。零值返回 false。
// 并发：纯函数。
func (s Speaker) Valid() bool {
	switch s {
	case SpeakerHuman, SpeakerAssistant, SpeakerTool, SpeakerFramework:
		return true
	}
	return false
}

// StabilityRank 给出三档在**编译顺序**上的相对次序：
// frozen=0 < stable=1 < volatile=2。未识别值（含零值）返回 (-1, false)。
//
// 为什么要显式排序：Part 10.3 的硬不变量是"不得跨 Stability 边界重排，
// 也不得把 volatile 挪到 stable 之前"，而这条不变量必须由**比较**实现。
// 各处自己写 if 链会让不同实现给出不同次序（例如把 volatile 当 0），
// 而症状是缓存命中率下降——没有任何报错，只有账单变贵。
//
// 未识别值返回 false 而不是归入 stable：此处的调用者是编译器，
// 遇到非法 Stability 时应当中断编译（types.Stability 的零值契约已要求
// 非法值不得被解释为 frozen），而不是替它选一个。
//
// 并发：纯函数。
func StabilityRank(s types.Stability) (int, bool) {
	switch s {
	case types.StabilityFrozen:
		return 0, true
	case types.StabilityStable:
		return 1, true
	case types.StabilityVolatile:
		return 2, true
	}
	return -1, false
}

// Segment 是 CanonicalRequest 的最小单元（Part 10.3）。
// 它表达语义与稳定性，不表达角色布局——后者由 Normalizer 决定。
//
// 不变量（Kind ↔ Speaker ↔ Stability 三者必须自洽，否则"语义与稳定性"
// 这个承重设计就漏了；校验点在各层的 Validate/vm 检查与 golden 测试）：
//
//	Kind            Stability（必须）   Speaker（必须）        其它
//	system          frozen              framework               —
//	standing        frozen              framework               —
//	knowledge       frozen              framework               —
//	turn            stable              human/assistant/framework（tool 走 tool_result）
//	tool_result     stable              tool                    ToolCallID 非空
//	transient       volatile            human/assistant/framework（框架注入的提醒两类都有）
//
// 另有两条跨字段约束：
//   - ToolCalls 非空 ⇒ Kind == SegTurn 且 Speaker == SpeakerAssistant
//     （工具调用只能是 assistant 的回合产出）；
//   - Prefill（CanonicalRequest 级）目标是 assistant 开头，与 Segment 无关，
//     不在本结构上表达。
//
// Content 与 XML 标注：Content 含 XML 标注、**全协议逐字节保留**。
// 任何"顺手 trim/规范化空白"的行为都会改变缓存前缀字节，从而让缓存命中
// 丢失（而且只在跨进程/跨协议时暴露）。
type Segment struct {
	Kind    SegmentKind
	Speaker Speaker
	Content string // 含 XML 标注，全协议逐字节保留
	// Reasoning 是历史思维链（ADR-0023：带 tools 的请求应回传）。
	// 仅 SegTurn + SpeakerAssistant 允许携带（校验在 Normalizer），Content
	// 可为空——"只思考、无可见回复"的历史助手消息。逐字节保留，不解析。
	Reasoning   string
	Attachments []types.Attachment
	ToolCalls   []types.ToolCall // assistant 产出的调用
	ToolCallID  string           // tool_result 的配对 id
	Stability   types.Stability
}

// ToolDef 是工具表的定义（Part 10.3）。
// 全项目逐字节一致、属冻结前缀：所有 Agent 共享同一份 Tools schema，
// 任何 Profile 都不允许影响它（修正 1 的"场景唯一"决策）。
//
// 不变量：
//   - Name 非空，且必须与技能表中注册的名字（或 proto 的意图工具名）一致——
//     LLM 按名字调用，名字漂移会让调用落在"未知工具"上；
//   - Description 非空：描述是模型选择工具的唯一依据，空描述等于这个工具
//     对模型不存在，但它仍会占 token 与 schema 槽位；
//   - Parameters 是非空且合法的 JSON Schema（顶层为 object）。这里存
//     json.RawMessage 而非结构体，是为了逐字节冻结——任何解析再序列化都会
//     改变键序与空白，破坏 frozen 前缀的 byte-stable 要求。
//
// 冻结语义的后果：改任何一个字节（包括描述里改个错别字）都会让**所有**
// Agent 的缓存前缀失效。这类改动必须与"是否值得让全项目缓存重算"一起评估。
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
}

// CanonicalRequest 是协议无关的规范形态（Part 10.3，关键承重结构）。
// 这是 Normalizer 的输入：它必须能不依赖任何厂商细节地完整描述一次调用。
//
// 不变量：
//   - Segments 非空，且**按 Stability 单调不减排列**（frozen → stable → volatile；
//     用 StabilityRank 比较）。volatile 只允许出现在尾部；
//   - 每个 Segment 自身满足其 Kind 对应的约束（见 Segment 不变量表）；
//   - Tools 与项目级工具表逐字节一致（此处不做去重/排序，顺序即协议）；
//   - Sampling / Thinking 满足 types 层各自的契约（本包不重复校验，
//     但要保证**原样传递**，不得"顺手"补默认值）；
//   - Prefill 非 nil 时不得为空串（空串是"要求模型以空开头"这种无意义请求，
//     各厂商行为不一，有的直接报错）；nil 表示不要求 prefill。
//
// 禁止携带厂商字段：本结构里不得出现任何 provider 相关字段（如 cache_control
// 的具体标记）。那类信息只允许在 Normalizer 之后、由 CacheControl 承载。
// 这条靠代码评审 + golden 测试守护，因为一旦破例，"换厂商"就不再是加一个
// 适配器的事，而是要回头改这个承重结构。
//
// OutputJSON 是**语义层**的"要求合法 JSON 输出"声明（阶段 1 补，见 ADR-0017）：
// 它不说"用哪个字段表达"，那是 Normalizer 的事（OpenAI 兼容线路翻译成
// response_format={"type":"json_object"}；其它厂商没有对应字段时
// 只能降级成 prefill/工具调用）。放在 Canonical 而不是 WireRequest，是为了
// 让"我要 JSON"这个需求不依赖具体厂商——它同时也是 TaskPolicy.OutputFormat
// 那类语义需求的落点。
//
// 零值 false = 不要求 JSON 输出（默认文本）。这不是"未设置"：
// 它导致的后果是"不发送任何输出格式声明"，与厂商默认行为一致，方向安全。
type CanonicalRequest struct {
	Segments   []Segment
	Tools      []ToolDef
	Sampling   types.SamplingParams
	Thinking   types.ThinkingSpec
	Prefill    *string // 期望的 assistant 开头；可能被 Normalizer 降级
	OutputJSON bool    // 要求输出合法 JSON（语义层；由 Normalizer 翻译成协议形态）
}
