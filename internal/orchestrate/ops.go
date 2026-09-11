package orchestrate

import (
	"context"

	"marl/internal/types"
)

// SplitStrategy 是 split_message 的两种策略（Part 3.5）。
//
// 零值契约：零值 SplitStrategy("") 非法。两种策略的参数需求**不相交**
// （delimiter 要 Delimiter，semantic 要 Segments），未指定策略时无法推断
// 调用方想要哪种，因此消费侧必须拒绝——不允许默认成机械切分：
// 机械切分会把一段连贯推理切成无 topic 的碎片，而且不报错。
type SplitStrategy string

const (
	// SplitSemantic 调用 Orchestrator（独立 LLM 调用，最便宜档，JSON mode）。
	// 需要校验覆盖全文、无重叠、段非空，并对非法切点做修复。
	SplitSemantic SplitStrategy = "semantic"
	// SplitDelimiter 是机械操作（零 LLM 调用），按分隔符字面切（分隔符行本身丢弃）。
	SplitDelimiter SplitStrategy = "delimiter"
)

// Valid 报告 s 是否为已定义策略之一。零值返回 false。
//
// 并发：纯函数。
func (s SplitStrategy) Valid() bool {
	switch s {
	case SplitSemantic, SplitDelimiter:
		return true
	}
	return false
}

// SplitParams 是拆分的参数。
//
// 策略相关的必填项（Validate 的判据，跨字段校验）：
//   - SplitDelimiter：Delimiter 非空，且 Segments **必须为空**；
//   - SplitSemantic：Segments 非空，且 Delimiter **必须为空**。
//
// 为什么要求"另一策略的字段必须为空"而不是"用哪个就忽略另一个"：
// 同时给出两者说明调用方对这次调用的意图判断有分歧，此时无论选哪个都可能
// 不是它想要的；而静默忽略会让实际行为与参数表意不符，事后无法从参数复盘。
type SplitParams struct {
	Target    types.MessageID
	Strategy  SplitStrategy
	Delimiter string         // 仅 delimiter 策略使用
	Segments  []SplitSegment // 仅 semantic 策略使用（Orchestrator 的输出）
}

// Validate 报告参数是否自洽（含策略与字段的匹配）。
//
// 失败：Target 为空；Strategy 非法；两个策略专属字段的"该填/该空"不符。
// 并发：纯函数。
func (p SplitParams) Validate() error {
	panic("TODO(phase 0): placeholder")
}

// SplitSegment 是 Orchestrator 语义拆分的一个输出段（Part 3.5）。
// 校验：覆盖全文、无重叠、段非空；修复：填缝、截断越界、吸附到禁切区外
// （代码块 / 引用块 / XML 标注块）。
//
// 行号契约：1-based 闭区间，因此 StartLine 的零值是**非法值**（不指向任何行）。
// 段集合级不变量（覆盖全文、无重叠、升序）单段无法自查，由消费侧校验：
// 本类型只保证段内自洽（见 Validate）。
type SplitSegment struct {
	Topic     string
	StartLine int
	EndLine   int
}

// Validate 报告单段是否自洽：Topic 非空、StartLine >= 1、EndLine >= StartLine。
//
// 失败：Topic 为空（无 topic 的段失去了拆分要传递的信息）；行号越界或倒置。
// 并发：纯函数。
func (s SplitSegment) Validate() error {
	panic("TODO(phase 0): placeholder")
}

// ExcludeParams 是软删除参数（Part 3.5）。
// 软删除只改 View 的 Visible，从不删 Log——这是"可复原"的实现基础。
type ExcludeParams struct {
	Target types.MessageID
}

// Validate 报告参数是否可用。失败：Target 为空。
// 并发：纯函数。
func (e ExcludeParams) Validate() error {
	panic("TODO(phase 0): placeholder")
}

// RestoreParams 是撤销软删除参数。
type RestoreParams struct {
	Target types.MessageID
}

// Validate 报告参数是否可用。失败：Target 为空。
// 并发：纯函数。
func (r RestoreParams) Validate() error {
	panic("TODO(phase 0): placeholder")
}

// ReorderParams 是重排序参数（Fractional Index，支持任意位置插入而不重排）。
//
// Position 的零值陷阱：0.0 是**合法**位置（排到最前），但同时也是未设置时的
// 零值——两者不可区分。处理方式是靠调用语境而非类型：本结构只在"确实要移动"
// 时构造，因此不存在"Position 未设置"的合法情形；不要把它当作可选字段的容器。
// 之所以不用指针：多一层间接会让 Position 的每次算术都要解引用，而真正的
// 风险（忘记构造 ReorderParams）本来就不由指针能防住。
//
// 另外必须拒绝 NaN / ±Inf：分数索引的插入点是相邻两项的中点，一旦混入 NaN，
// 比较结果全部为 false，排序会静默退化为"保持原顺序"，而 Position 列已经脏了。
type ReorderParams struct {
	Target   types.MessageID
	Position float64
}

// Validate 报告参数是否可用：Target 非空、Position 有限（非 NaN/Inf）。
//
// 注意 Position == 0 是合法的，Validate **不**把它当错误。
// 并发：纯函数。
func (r ReorderParams) Validate() error {
	panic("TODO(phase 0): placeholder")
}

// AnnotateParams 是加批注参数。
// 不改原文，追加一个 ProvAnnotatedOf 的外壳条目（<note>...</note>）。
type AnnotateParams struct {
	Target types.MessageID
	Note   string
}

// Validate 报告参数是否可用：Target 与 Note 均非空。
// Note 为空时产生的是一个"什么都没说"的外壳条目——它会占用上下文并让
// 血缘图多一个无信息的节点，因此视为非法而非空操作。
// 并发：纯函数。
func (a AnnotateParams) Validate() error {
	panic("TODO(phase 0): placeholder")
}

// PinParams 是裁剪豁免开关参数。
//
// [偏离文档/自查: 原本还带一个 Pinned bool，已删除。原因是它与 OpPin /
// OpUnpin 两个 OpKind **语义重复**，且重复会产生一个无解状态：OpUnpin 配上
// Pinned=true 时无法判定谁权威。删除后极性只由 Kind 表达，参数只带目标。]
type PinParams struct {
	Target types.MessageID
}

// Validate 报告参数是否可用。失败：Target 为空。
// 并发：纯函数。
func (p PinParams) Validate() error {
	panic("TODO(phase 0): placeholder")
}

// Orchestrator 是"编排调用"的执行者（Part 3.7 / 7.5）。
//
// 它跑在最便宜的档位（r0），JSON mode，token 消耗记入独立账本
// （ledger.RecordOrchestration），不计入主任务预算，但算进任务总成本。
//
// 关键约束：Orchestrator 的输入是目标消息的**只读快照**，它修改不了主 Agent
// 的状态。产出追加到主 Log 但主 View 暂时不引用——主 Agent 拿到之后自己决定用不用。
// 这避免了"编排污染主干"的递归陷阱。
//
// 边界（谁负责什么，避免实现时把重试逻辑写进这里）：
//   - Orchestrator 只做**一次** LLM 调用并返回原始段；不重试、不修复、不校验
//     覆盖性。重试（Part 3.7 步骤 5 的"温度稍高再试一次"）与切点修复（吸附到
//     禁切区外）属于调用方（Compressor / split Op），因为它们需要访问 Log 与
//     禁切区信息，而且"重试几次"是策略而非能力。
//   - 输入是只读的：实现不得修改传入内容，也不得读取主 Agent 的可变状态
//     （否则编排结果会随主 Agent 状态漂移，编排就不再可重放）。
type Orchestrator interface {
	// SplitSemantic 把带行号渲染的消息切成带 topic 的段。
	//
	// 输入格式（content 的唯一合法形态）：每行形如 "N| 正文"，N 从 1 起连续。
	// 之所以要求带行号，是因为段本身用**行号**而非字节区间定位（见 SplitSegment）——
	// 行号在渲染后仍可被模型准确引用，字节偏移不行。
	// 因此调用方必须先渲染行号，本方法不负责渲染（渲染是纯函数，
	// 由需要它的两侧共用同一份实现）。
	//
	// 失败：LLM 调用失败、返回无法解析为段列表 → 错误（上层决定重试或降级）。
	// 返回的段**未经验证**：覆盖性、重叠、禁切区吸附都由调用方校验并修复。
	// 空内容返回空切片 + nil（不是错误：没有内容就没有段）。
	SplitSemantic(ctx context.Context, content string) ([]SplitSegment, error)
}

// [偏离文档/自查: 原有一个 SemanticSplitRequest{Content string} 结构体，
// 已删除。它是一个单字段包装，从未被任何签名使用（SplitSemantic 收的是
// content string），属于"声明了契约却不生效"的死类型——留着会让人以为
// 输入形态由结构体钉住。输入格式已直接写进 SplitSemantic 的注释。]

// ZoneKind 是禁切区的种类（代码块 / 引用块 / XML 标注块）。
//
// 零值契约：零值 ZoneKind("") 非法。禁切区的作用是**阻止**切点落进去，
// 未识别的种类若被当作"非禁切区"忽略（fail-open），切点就会落进代码块
// 把一段代码切成两半——而且切分本身会成功，问题只在后续阅读理解时暴露。
// 因此消费侧遇到未识别种类必须按"整段都不可切"处理（fail-closed）。
type ZoneKind string

const (
	ZoneCodeFence  ZoneKind = "code_fence"
	ZoneBlockquote ZoneKind = "blockquote"
	ZoneXMLTag     ZoneKind = "xml_tag"
)

// Valid 报告 k 是否为已定义禁切区种类之一。零值返回 false。
// 并发：纯函数。
func (k ZoneKind) Valid() bool {
	switch k {
	case ZoneCodeFence, ZoneBlockquote, ZoneXMLTag:
		return true
	}
	return false
}

// NoSplitZone 描述禁止切分的位置类型（代码块 / 引用块 / XML 标注块），
// 语义拆分的修复阶段用它把切点吸附到区外。
//
// 行号契约：1-based 闭区间（与 SplitSegment 一致，两者直接比较）。
// 不变量：Kind 合法、Start >= 1、End >= Start。零值非法——零值区的
// [0,0] 在 1-based 体系里不指向任何行，按"区间"参与吸附计算会静默失效。
type NoSplitZone struct {
	Kind  ZoneKind
	Start int // 行号（1-based）
	End   int
}

// Validate 报告该禁切区是否自洽：Kind 合法、Start >= 1、End >= Start。
// 并发：纯函数。
func (z NoSplitZone) Validate() error {
	panic("TODO(phase 0): placeholder")
}
