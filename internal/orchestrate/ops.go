package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"marl/internal/store"
	"marl/internal/types"
)

// 本文件实现 Part 3.5 的编排操作原子集（类型契约沿用阶段 0 的定义，
// 实现为阶段 3 交付）。
//
// 结构约定（全文件统一，见 doc.go 的全包契约）：
//   - 每个 Op 是一个不可变的小结构体：构造时携带参数，Apply 时只读它们——
//     因此同一个实例可被并发 Apply（Operation 契约）；
//   - 参数校验只在 Apply 一处（构造函数不做第二套校验，避免两份判据漂移）；
//   - 所有对 View 的修改都先 clone 再改，传入的 view 逐字段不变
//     （Operation.Apply 后置条件的可检验形式）。

// ErrNothingToCompress 是"无可压缩区间"的哨兵（Part 3.7 的开放边界裁决，
// 见 ADR-0026）：L0 清理后中段为空（或尾部保留已覆盖全部回合）时，
// Compress 返回它，调用方据此**跳过**本次压缩并继续主循环——这不是故障，
// 是"压缩没做"与"压缩做了但失败"两者中前者。
//
// 用哨兵而非 nil 结果的原因：nil, nil 会把"没做"伪装成成功，而调用方
// （主循环）需要区分"没东西可压"（继续跑）与"压缩失败"（降级/上报）。
var ErrNothingToCompress = errors.New("marl/orchestrate: nothing to compress (tail keep covers all rounds)")

// ErrSingleSegment 是拆分修复后只剩一段的哨兵：把一条消息"拆"成一段
// 等于原样复制一份进 Log（纯浪费 token 与血缘节点），且几乎总是意味着
// LLM 判断"这条消息本不该拆"。调用方（未来的 split 技能包装）应把该
// 错误如实回填给模型。
var ErrSingleSegment = errors.New("marl/orchestrate: split repaired to a single segment (nothing to split)")

// ---------------------------------------------------------------------------
// 参数类型（阶段 0 契约，字面注释保留）
// ---------------------------------------------------------------------------

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
	switch {
	case p.Target == "":
		return fmt.Errorf("split: target is empty")
	case !p.Strategy.Valid():
		return fmt.Errorf("split: strategy %q invalid", p.Strategy)
	case p.Strategy == SplitDelimiter && p.Delimiter == "":
		return fmt.Errorf("split: delimiter strategy requires a non-empty delimiter")
	case p.Strategy == SplitDelimiter && len(p.Segments) != 0:
		return fmt.Errorf("split: delimiter strategy must not carry segments")
	case p.Strategy == SplitSemantic && p.Delimiter != "":
		return fmt.Errorf("split: semantic strategy must not carry a delimiter")
	case p.Strategy == SplitSemantic && len(p.Segments) == 0:
		return fmt.Errorf("split: semantic strategy requires segments")
	}
	return nil
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
	switch {
	case s.Topic == "":
		return fmt.Errorf("split segment: topic is empty")
	case s.StartLine < 1:
		return fmt.Errorf("split segment %q: start line %d < 1", s.Topic, s.StartLine)
	case s.EndLine < s.StartLine:
		return fmt.Errorf("split segment %q: end %d < start %d", s.Topic, s.EndLine, s.StartLine)
	}
	return nil
}

// ExcludeParams 是软删除参数（Part 3.5）。
// 软删除只改 View 的 Visible，从不删 Log——这是"可复原"的实现基础。
type ExcludeParams struct {
	Target types.MessageID
}

// Validate 报告参数是否可用。失败：Target 为空。
// 并发：纯函数。
func (e ExcludeParams) Validate() error {
	if e.Target == "" {
		return fmt.Errorf("exclude: target is empty")
	}
	return nil
}

// RestoreParams 是撤销软删除参数。
type RestoreParams struct {
	Target types.MessageID
}

// Validate 报告参数是否可用。失败：Target 为空。
// 并发：纯函数。
func (r RestoreParams) Validate() error {
	if r.Target == "" {
		return fmt.Errorf("restore: target is empty")
	}
	return nil
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
	switch {
	case r.Target == "":
		return fmt.Errorf("reorder: target is empty")
	case !isFinite(r.Position):
		return fmt.Errorf("reorder: position %v is not finite", r.Position)
	}
	return nil
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
	switch {
	case a.Target == "":
		return fmt.Errorf("annotate: target is empty")
	case a.Note == "":
		return fmt.Errorf("annotate: note is empty")
	}
	return nil
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
	if p.Target == "" {
		return fmt.Errorf("pin: target is empty")
	}
	return nil
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
	switch {
	case !z.Kind.Valid():
		return fmt.Errorf("no-split zone: kind %q invalid", z.Kind)
	case z.Start < 1:
		return fmt.Errorf("no-split zone %s: start %d < 1", z.Kind, z.Start)
	case z.End < z.Start:
		return fmt.Errorf("no-split zone %s: end %d < start %d", z.Kind, z.End, z.Start)
	}
	return nil
}

// NoSplitZoneScanner 是禁切区扫描的最小接口（消费侧定义，Part 5.4 的
// "接口在消费侧"纪律）。实现见 compress 包（代码块/引用块的启发式扫描）；
// XML 标注块扫描按 13.5 的"不做"清单延后——接口先行，实现补齐时无需改
// 本包。
type NoSplitZoneScanner interface {
	// ScanNoSplitZones 返回 content 中的禁切区（行号 1-based 闭区间，
	// 与 SplitSegment 同一口径）。允许返回空切片（无禁切区）。
	ScanNoSplitZones(content string) []NoSplitZone
}

// ---------------------------------------------------------------------------
// 渲染与哨兵
// ---------------------------------------------------------------------------

// RenderNumbered 把 content 渲染成带行号的形态（每行 "N| 正文"，N 从 1 起
// 连续、右对齐）。这是 SplitSemantic 输入的唯一合法形态（见 Orchestrator
// 契约），也是语义拆分段落渲染 `<segment>` 的行号来源。
//
// 与 file_read content 模式的行号渲染保持同一风格（`%*d| `，Part 4.4 的
// 既有约定）：两处渲染风格漂移会让模型在两种工具间看到不一致的行号语义。
//
// 末行无换行符时不补（渲染是逐行的，行边界由 \n 定义；空 content 返回空串）。
// 并发：纯函数。
func RenderNumbered(content string) string {
	if content == "" {
		return ""
	}
	lines := splitLines(content)
	width := len(fmt.Sprint(len(lines)))
	var sb strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&sb, "%*d| %s\n", width, i+1, ln)
	}
	return sb.String()
}

// splitLines 按行切分 content（剥离末尾至多一个换行符——与 file_read 的
// TrimSuffix 同一口径，保证"渲染行数 == 原文行数"）。
//
// 并发：纯函数。
func splitLines(content string) []string {
	content = strings.TrimSuffix(content, "\n")
	content = strings.TrimSuffix(content, "\r")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// isFinite 报告 f 是否为有限浮点数（NaN 与 ±Inf 均为 false）。
// 并发：纯函数。
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// ---------------------------------------------------------------------------
// 视图克隆与查找（全部 Op 共用的机械件）
// ---------------------------------------------------------------------------

// cloneView 深拷贝 View 的可变部分（Items 切片；ViewItem 无引用字段，
// 逐元素拷贝即深拷贝）。EstimatedTokens/LastEstimatedAt 属于"上次编译"的
// 缓存值，编排后必然失准——置零（LastEstimatedAt 零值 = 从未估算，
// 消费者会重新估算而不是把旧值当真，见 types.ContextView 契约）。
//
// 并发：纯函数（只读入参）。
func cloneView(view *types.ContextView) *types.ContextView {
	out := &types.ContextView{AgentID: view.AgentID}
	out.Items = make([]types.ViewItem, len(view.Items))
	copy(out.Items, view.Items)
	return out
}

// findItem 返回 ref 在 view.Items 中的下标。不存在返回 (0, false)。
// 并发：纯函数。
func findItem(view *types.ContextView, ref types.MessageID) (int, bool) {
	for i := range view.Items {
		if view.Items[i].Ref == ref {
			return i, true
		}
	}
	return 0, false
}

// fetchEntry 取目标条目并兜底两类失败（Op 的共同前置）：
// View 里没有该引用 / Log 里没有该条目，都归为 store.ErrNotFound——
// 后者按 doc.go 的错误语义还意味着"数据被外部改动"，调用方须记审计。
func fetchEntry(ctx context.Context, log store.MessageLog, view *types.ContextView, ref types.MessageID) (*types.LogEntry, error) {
	if _, ok := findItem(view, ref); !ok {
		return nil, fmt.Errorf("%w: view does not reference %s", store.ErrNotFound, ref)
	}
	e, err := log.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", ref, err)
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// exclude / restore / pin / unpin：四个"只动一个布尔位"的 Op
// ---------------------------------------------------------------------------

// viewFlagOp 是 exclude/restore/pin/unpin 的共同形态：定位目标条目，
// 改一个布尔位，返回新 View。四种 Op 的差异只有一个字段与一个方向，
// 抽出来是为了让"定位 + 克隆 + 不改入参"这套纪律只写一遍。
//
// [权衡: 四个类型各自实现 Apply 会重复同一段克隆/查找代码；合并成一个
// 参数化类型则损失 Kind() 的显式性。折中：合并机械件，Kind 由各构造
// 函数给出。]
type viewFlagOp struct {
	kind   OpKind
	target types.MessageID
	set    func(it *types.ViewItem)
}

// Kind 实现 Operation。
func (op *viewFlagOp) Kind() OpKind { return op.kind }

// Apply 实现 Operation（契约见 Operation 注释）。
func (op *viewFlagOp) Apply(_ context.Context, _ store.MessageLog, view *types.ContextView) (*OpResult, error) {
	idx, ok := findItem(view, op.target)
	if !ok {
		return nil, fmt.Errorf("%w: view does not reference %s", store.ErrNotFound, op.target)
	}
	out := cloneView(view)
	op.set(&out.Items[idx])
	return &OpResult{
		View:         out,
		AuditPayload: map[string]any{"op": string(op.kind), "target": string(op.target)},
	}, nil
}

// NewExcludeOp 构造 exclude_message（软删除：Visible=false，Log 不动）。
func NewExcludeOp(params ExcludeParams) Operation {
	return &viewFlagOp{kind: OpExclude, target: params.Target, set: func(it *types.ViewItem) { it.Visible = false }}
}

// NewRestoreOp 构造 restore_message（撤销软删除：Visible=true）。
func NewRestoreOp(params RestoreParams) Operation {
	return &viewFlagOp{kind: OpRestore, target: params.Target, set: func(it *types.ViewItem) { it.Visible = true }}
}

// NewPinOp 构造 pin_message（裁剪豁免：Pinned=true）。
func NewPinOp(params PinParams) Operation {
	return &viewFlagOp{kind: OpPin, target: params.Target, set: func(it *types.ViewItem) { it.Pinned = true }}
}

// NewUnpinOp 构造 unpin_message（取消豁免：Pinned=false）。
func NewUnpinOp(params PinParams) Operation {
	return &viewFlagOp{kind: OpUnpin, target: params.Target, set: func(it *types.ViewItem) { it.Pinned = false }}
}

// ---------------------------------------------------------------------------
// reorder：Fractional Index 的位置改写 + Stability 单调校验
// ---------------------------------------------------------------------------

// reorderOp 把目标条目的 Position 改为参数指定值。
type reorderOp struct {
	params ReorderParams
}

// NewReorderOp 构造 reorder_message。
func NewReorderOp(params ReorderParams) Operation {
	return &reorderOp{params: params}
}

// Kind 实现 Operation。
func (op *reorderOp) Kind() OpKind { return OpReorder }

// Apply 实现 Operation。
//
// 除参数校验外还有一条 Part 3.3 的硬不变量：**不得跨 Stability 边界重排，
// 不得把 volatile 挪到 stable 之前**。实现方式是先模拟重排（按新 Position
// 稳定排序），再校验 Stability 序列单调不减——违反即拒绝（store.ErrInvalid），
// 绝不"改了再说"：破缓存的 reorder 一旦执行，损失在账单上，而不在报错里。
func (op *reorderOp) Apply(_ context.Context, _ store.MessageLog, view *types.ContextView) (*OpResult, error) {
	if err := op.params.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	idx, ok := findItem(view, op.params.Target)
	if !ok {
		return nil, fmt.Errorf("%w: view does not reference %s", store.ErrNotFound, op.params.Target)
	}
	out := cloneView(view)
	out.Items[idx].Position = op.params.Position

	// 模拟重排后的顺序（稳定排序：同 Position 保持原相对次序，避免并列
	// 位置的重排结果不确定）。
	order := make([]int, len(out.Items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return out.Items[order[a]].Position < out.Items[order[b]].Position
	})
	prevRank := -1
	for _, i := range order {
		rank, ok := stabilityRank(out.Items[i].Stability)
		if !ok {
			return nil, fmt.Errorf("%w: item %s stability %q unrankable", store.ErrInvalid, out.Items[i].Ref, out.Items[i].Stability)
		}
		if prevRank >= 0 && rank < prevRank {
			return nil, fmt.Errorf("%w: reorder would move %q across stability boundary (volatile must stay in tail)", store.ErrInvalid, op.params.Target)
		}
		prevRank = rank
	}
	return &OpResult{
		View:         out,
		AuditPayload: map[string]any{"op": string(OpReorder), "target": string(op.params.Target), "position": op.params.Position},
	}, nil
}

// stabilityRank 与 types.ContextView.Validate / wire.StabilityRank 同一规则
// 的第三份实现（frozen=0 < stable=1 < volatile=2）。types 包不依赖本包、
// 本包依赖 types——不能直接复用 wire 的版本（会引入 orchestrate→wire 依赖，
// 而 wire 是线路层，编排层不该知道协议）。三处漂移的防线是各自的
// golden/零值测试；规则变更须同步三处并记 ADR。
//
// 并发：纯函数。
func stabilityRank(s types.Stability) (int, bool) {
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

// ---------------------------------------------------------------------------
// annotate：批注外壳（追加 ProvAnnotatedOf，View 换引用）
// ---------------------------------------------------------------------------

// annotateOp 给目标消息加批注外壳。
type annotateOp struct {
	params AnnotateParams
}

// NewAnnotateOp 构造 annotate_message。
func NewAnnotateOp(params AnnotateParams) Operation {
	return &annotateOp{params: params}
}

// Kind 实现 Operation。
func (op *annotateOp) Kind() OpKind { return OpAnnotate }

// Apply 实现 Operation。
//
// 产出：新 LogEntry（Role 与目标相同、Content = <note> 外壳 + 原文、
// Prov=ProvAnnotatedOf、SourceIDs=[目标]、Audience=Both），View 中指向
// 目标的引用**替换**为指向外壳的引用（原文留在 Log——"外壳"语义即
// 阅读者看到的是包了批注的全文）。
//
// 拒绝两类目标（store.ErrInvalid）：
//   - Meta 带 tool_calls 的 assistant 意图条目：外壳会割裂"调用 ↔ 结果"
//     的配对编译（compile 层靠 Meta 还原调用），包一层等于让下一轮请求
//     丢失工具调用历史；
//   - Role=thinking：思维链是审计证据，逐字节保留（ADR-0023 的纪律），
//     包外壳等于改写。
func (op *annotateOp) Apply(ctx context.Context, log store.MessageLog, view *types.ContextView) (*OpResult, error) {
	if err := op.params.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	entry, err := fetchEntry(ctx, log, view, op.params.Target)
	if err != nil {
		return nil, err
	}
	if _, isCall := decodeToolCalls(entry); isCall {
		return nil, fmt.Errorf("%w: annotate target %s is a tool-call intent entry", store.ErrInvalid, op.params.Target)
	}
	if entry.Role == types.RoleThinking {
		return nil, fmt.Errorf("%w: annotate target %s is a thinking entry (audit evidence stays byte-exact)", store.ErrInvalid, op.params.Target)
	}

	appended := &types.LogEntry{
		AgentID:   entry.AgentID,
		Role:      entry.Role,
		Content:   fmt.Sprintf("<note>\n%s\n</note>\n\n%s", op.params.Note, entry.Content),
		Prov:      types.ProvAnnotatedOf,
		SourceIDs: []types.MessageID{entry.ID},
		Audience:  types.AudienceBoth,
	}
	if _, err := log.Append(ctx, appended); err != nil {
		return nil, fmt.Errorf("append annotation: %w", err)
	}

	out := cloneView(view)
	idx, _ := findItem(out, op.params.Target) // fetchEntry 已确认存在
	out.Items[idx].Ref = appended.ID
	return &OpResult{
		View:         out,
		Appended:     []*types.LogEntry{appended},
		AuditPayload: map[string]any{"op": string(OpAnnotate), "target": string(op.params.Target), "note": op.params.Note},
	}, nil
}

// decodeToolCalls 判断条目是否携带工具调用意图（与 agent 包的
// decodeToolCallsMeta 同一键，但本包只判存在性、不还原内容——
// 编排层不需要调用结构，只需要知道"这条不能包外壳"）。
//
// 并发：纯函数。
func decodeToolCalls(e *types.LogEntry) ([]any, bool) {
	raw, ok := e.Meta["tool_calls"]
	if !ok {
		return nil, false
	}
	list, ok := raw.([]any)
	return list, ok && len(list) > 0
}

// ---------------------------------------------------------------------------
// split：delimiter 机械切 + semantic（LLM 段 → 校验修复 → 追加）
// ---------------------------------------------------------------------------

// splitOp 实现拆分。semantic 策略依赖两个注入依赖：Orchestrator（LLM 调用）
// 与 NoSplitZoneScanner（禁切区扫描，实现在 compress 包——扫描规则是
// 内容启发式，属"实现层"；本包只定义接口并在修复时消费其结果）。
// delimiter 策略不使用这两个依赖（机械操作，零 LLM 调用）。
type splitOp struct {
	params SplitParams
	orch   Orchestrator
	zones  NoSplitZoneScanner
}

// NewSplitOp 构造 split_message。semantic 策略要求 orch 与 zones 均非 nil
// （缺失在 Apply 时报错——构造期不校验依赖，校验只在 Apply 一处）。
func NewSplitOp(params SplitParams, orch Orchestrator, zones NoSplitZoneScanner) Operation {
	return &splitOp{params: params, orch: orch, zones: zones}
}

// Kind 实现 Operation。
func (op *splitOp) Kind() OpKind { return OpSplit }

// Apply 实现 Operation。
//
// 共同后置：目标条目从 View 中移除，N 个分段条目按序插入原位
// （Fractional Index 中点插入，见 insertPositions）；分段条目 Role 与目标
// 相同、Prov=ProvSplitOf、SourceIDs=[目标]、Audience=Both。
//
// 拒绝两类目标（store.ErrInvalid，理由同 annotateOp）：tool_calls 意图
// 条目（拆散调用↔结果配对）与 thinking 条目（审计证据逐字节保留）。
func (op *splitOp) Apply(ctx context.Context, log store.MessageLog, view *types.ContextView) (*OpResult, error) {
	if err := op.params.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	entry, err := fetchEntry(ctx, log, view, op.params.Target)
	if err != nil {
		return nil, err
	}
	if _, isCall := decodeToolCalls(entry); isCall {
		return nil, fmt.Errorf("%w: split target %s is a tool-call intent entry", store.ErrInvalid, op.params.Target)
	}
	if entry.Role == types.RoleThinking {
		return nil, fmt.Errorf("%w: split target %s is a thinking entry (audit evidence stays byte-exact)", store.ErrInvalid, op.params.Target)
	}

	var pieces []splitPiece
	switch op.params.Strategy {
	case SplitDelimiter:
		pieces, err = splitByDelimiter(entry.Content, op.params.Delimiter)
	case SplitSemantic:
		pieces, err = op.splitSemantic(ctx, entry.Content)
	}
	if err != nil {
		return nil, err
	}

	// 追加分段条目（Log 只追加；血缘指向原条目）。
	appended := make([]*types.LogEntry, 0, len(pieces))
	for _, p := range pieces {
		e := &types.LogEntry{
			AgentID:   entry.AgentID,
			Role:      entry.Role,
			Content:   p.content,
			Prov:      types.ProvSplitOf,
			SourceIDs: []types.MessageID{entry.ID},
			Audience:  types.AudienceBoth,
			Meta:      p.meta,
		}
		if _, err := log.Append(ctx, e); err != nil {
			return nil, fmt.Errorf("append split piece: %w", err)
		}
		appended = append(appended, e)
	}

	out := cloneView(view)
	idx, _ := findItem(out, op.params.Target)
	role := out.Items[idx].WireRole
	stability := out.Items[idx].Stability
	pinned := out.Items[idx].Pinned
	positions := insertPositions(out.Items, idx, len(appended))
	pieces2 := make([]types.ViewItem, 0, len(appended))
	for i, e := range appended {
		pieces2 = append(pieces2, types.ViewItem{
			Ref:       e.ID,
			WireRole:  role, // 与目标同呈现身份（Part 3.4 映射不变）
			Stability: stability,
			Visible:   true,
			Pinned:    pinned, // 豁免随内容走：原条目豁免则其分段也豁免
			Position:  positions[i],
		})
	}
	out.Items = append(out.Items[:idx], append(pieces2, out.Items[idx+1:]...)...)
	if positions == nil {
		// Fractional Index 间距退化：整体重编号（顺序不变，Position 不进
		// 请求字节，重排无缓存代价）。
		RenumberView(out)
	}

	return &OpResult{
		View:     out,
		Appended: appended,
		AuditPayload: map[string]any{
			"op": string(OpSplit), "target": string(op.params.Target),
			"strategy": string(op.params.Strategy), "pieces": len(appended),
		},
	}, nil
}

// splitPiece 是一个待落库的分段（content + Meta）。
type splitPiece struct {
	content string
	meta    map[string]any
}

// splitByDelimiter 机械切分：按"整行等于分隔符"切，分隔符行本身丢弃；
// 空段（无行或纯空白）丢弃。至少要有两个非空段，否则拆分无意义
// （0 段 = 分隔符不存在；1 段 = 没切开）。
//
// 失败：store.ErrInvalid（分隔符未命中 / 拆出不足两段）。
func splitByDelimiter(content, delimiter string) ([]splitPiece, error) {
	lines := splitLines(content)
	var pieces []splitPiece
	cur := make([]string, 0, len(lines))
	flush := func() {
		text := strings.Trim(strings.Join(cur, "\n"), "\n\r")
		if strings.TrimSpace(text) != "" {
			pieces = append(pieces, splitPiece{content: text, meta: map[string]any{"strategy": "delimiter"}})
		}
		cur = cur[:0]
	}
	for _, ln := range lines {
		if strings.TrimRight(ln, "\r") == delimiter {
			flush()
			continue
		}
		cur = append(cur, ln)
	}
	flush()
	if len(pieces) < 2 {
		return nil, fmt.Errorf("%w: delimiter %q produced %d pieces (need >= 2)", store.ErrInvalid, delimiter, len(pieces))
	}
	return pieces, nil
}

// splitSemantic 语义拆分：渲染行号 → Orchestrator（LLM，JSON mode）→
// 校验修复（覆盖全文、无重叠、段非空；填缝、截断越界、吸附到禁切区外）→
// 渲染 `<segment topic>` 包裹的原文。
//
// Orchestrator 返回的段**未经验证**（契约），本方法是一次调用的"调用方"，
// 承担全部校验与修复；修复无法产出 ≥2 个合法段时返回错误（阶段 3 不做
// split 的重试——Part 3.7 的"温度稍高重试一次"是 SUM 流程的规则，
// split 的重试策略留给技能包装层，见 ADR-0026）。
func (op *splitOp) splitSemantic(ctx context.Context, content string) ([]splitPiece, error) {
	if op.orch == nil || op.zones == nil {
		return nil, fmt.Errorf("%w: semantic split requires Orchestrator and NoSplitZoneScanner", store.ErrInvalid)
	}
	lines := splitLines(content)
	if len(lines) == 0 {
		return nil, fmt.Errorf("%w: semantic split of empty content", store.ErrInvalid)
	}
	segs, err := op.orch.SplitSemantic(ctx, RenderNumbered(content))
	if err != nil {
		return nil, fmt.Errorf("orchestrator split: %w", err)
	}
	zones := op.zones.ScanNoSplitZones(content)
	repaired, err := repairSegments(segs, len(lines), zones)
	if err != nil {
		return nil, err
	}
	pieces := make([]splitPiece, 0, len(repaired))
	for i, s := range repaired {
		body := strings.Join(lines[s.StartLine-1:s.EndLine], "\n")
		pieces = append(pieces, splitPiece{
			content: fmt.Sprintf("<segment topic=%q>\n%s\n</segment>", s.Topic, body),
			meta:    map[string]any{"strategy": "semantic", "topic": s.Topic, "lines": fmt.Sprintf("%d-%d", s.StartLine, s.EndLine), "part": i},
		})
	}
	return pieces, nil
}

// repairSegments 把 LLM 给出的原始段集合修复成满足硬不变量的段集合：
// 覆盖 [1, totalLines]、互不重叠、每段非空、切点不落在禁切区内。
//
// 修复顺序即修复语义（每一步都假设前一步已完成）：
//  1. clamp：行号截断到 [1, totalLines]，越界成空段丢弃，空 topic 补 part_N；
//  2. 去重叠：按 Start 排序，后段 Start 压到前段 End+1，压瘪的段丢弃；
//  3. 填缝：段间空隙并入前段（前段 End 延到后段 Start-1），首尾空隙外扩；
//  4. 吸附：落在禁切区内的切点移到区边界（zone.Start-1 或 zone.End），
//     两个候选都不可行（会与前/后切点交叉）则合并两侧段（撤销该切点）。
//
// 失败：LLM 没给出任何可用段；修复后只剩一段（ErrSingleSegment）。
// 并发：纯函数。
func repairSegments(segs []SplitSegment, totalLines int, zones []NoSplitZone) ([]SplitSegment, error) {
	zs := clampZones(zones, totalLines)
	// 1. clamp + 补 topic。
	clean := make([]SplitSegment, 0, len(segs))
	for i, s := range segs {
		topic := s.Topic
		if topic == "" {
			topic = fmt.Sprintf("part_%d", i+1)
		}
		start, end := s.StartLine, s.EndLine
		if start < 1 {
			start = 1
		}
		if end > totalLines {
			end = totalLines
		}
		if start > end {
			continue // 截断后成空段
		}
		clean = append(clean, SplitSegment{Topic: topic, StartLine: start, EndLine: end})
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("%w: no usable segments from orchestrator", store.ErrInvalid)
	}
	// 2. 排序 + 去重叠。
	sort.SliceStable(clean, func(a, b int) bool { return clean[a].StartLine < clean[b].StartLine })
	kept := make([]SplitSegment, 0, len(clean))
	for _, s := range clean {
		if len(kept) > 0 && s.StartLine <= kept[len(kept)-1].EndLine {
			s.StartLine = kept[len(kept)-1].EndLine + 1
		}
		if s.StartLine > s.EndLine {
			continue // 被前段完全覆盖
		}
		kept = append(kept, s)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("%w: no usable segments from orchestrator", store.ErrInvalid)
	}
	// 3. 填缝 + 首尾外扩（保证覆盖 [1, totalLines]）。
	for i := 1; i < len(kept); i++ {
		if kept[i].StartLine > kept[i-1].EndLine+1 {
			kept[i-1].EndLine = kept[i].StartLine - 1
		}
	}
	kept[0].StartLine = 1
	kept[len(kept)-1].EndLine = totalLines
	// 4. 切点吸附到禁切区外。切点 k = 第 j 段的 End（k 与 k+1 之间切开）；
	//    k 落在区 z 内 ⟺ z.Start <= k < z.End（z 已 clamp 到 [1, totalLines]）。
	//
	//    修复候选按优先级：
	//      k1 = z.Start-1 —— 区整体归入后段；要求严格大于前一切点
	//                        （k1 == 前切点会让第 j 段变空）；
	//      k2 = z.End     —— 区整体归入前段；要求不越过文末、前切点、
	//                        以及后段的末行（否则后段被吞成空段）。
	//    两个候选都不可行时撤销该切点（合并 j 与 j+1）并原地重查——
	//    合并后的段尾可能仍落在别的禁切区里。
	//    每次调整后立即恢复相邻段的连续性（Start = 前段 End+1），
	//    保证"覆盖全文、无重叠"在任意中间状态都成立。
	j := 0
	for j < len(kept)-1 {
		z := zoneContaining(kept[j].EndLine, zs)
		if z == nil {
			j++
			continue
		}
		prevEnd := 0
		if j > 0 {
			prevEnd = kept[j-1].EndLine
		}
		k1, k2 := z.Start-1, z.End
		switch {
		case k1 > prevEnd:
			kept[j].EndLine = k1
			kept[j+1].StartLine = k1 + 1
			j++
		case k2 <= totalLines-1 && k2 > prevEnd && k2 < kept[j+1].EndLine:
			kept[j].EndLine = k2
			kept[j+1].StartLine = k2 + 1
			j++
		default:
			kept[j].Topic = kept[j].Topic + "+" + kept[j+1].Topic
			kept[j].EndLine = kept[j+1].EndLine
			kept = append(kept[:j+1], kept[j+2:]...)
			// j 不前进：合并后的段尾要重新过一遍禁切区检查。
		}
	}
	if len(kept) < 2 {
		return nil, ErrSingleSegment
	}
	return kept, nil
}

// clampZones 把禁切区截断到 [1, totalLines] 并丢弃越界成空的区。
// 扫描器给出的是内容里的原始区间，行号截断是修复的前置（对越界区做
// 吸附计算会把切点推出文外）。
//
// 并发：纯函数。
func clampZones(zones []NoSplitZone, totalLines int) []NoSplitZone {
	out := make([]NoSplitZone, 0, len(zones))
	for _, z := range zones {
		start, end := z.Start, z.End
		if start < 1 {
			start = 1
		}
		if end > totalLines {
			end = totalLines
		}
		if start > end {
			continue
		}
		out = append(out, NoSplitZone{Kind: z.Kind, Start: start, End: end})
	}
	return out
}

// zoneContaining 返回包含行 k 的第一个禁切区（k 在 [z.Start, z.End) 内
// 即"切在 k 会把 z 切开"）；无则返回 nil。
//
// 并发：纯函数。
func zoneContaining(k int, zones []NoSplitZone) *NoSplitZone {
	for i := range zones {
		if k >= zones[i].Start && k < zones[i].End {
			return &zones[i]
		}
	}
	return nil
}

// insertPositions 计算在 items[idx] 处替换为 n 个条目后的新位置
// （Fractional Index：取前后邻位的等分中点；被替换条目的原位让给分段）。
//
// 间距退化（gap <= 0，例如前邻位 == 后邻位）时返回 nil，调用方回退为
// 全量重排（重排是 View 层动作，不影响缓存——Position 不进请求字节）。
//
// 并发：纯函数。
func insertPositions(items []types.ViewItem, idx, n int) []float64 {
	prev := items[idx].Position - 1 // 无前邻时向左让出一个单位
	if idx > 0 {
		prev = items[idx-1].Position
	}
	next := items[idx].Position + 1 // 无后邻时向右让出一个单位
	if idx+1 < len(items) {
		next = items[idx+1].Position
	}
	gap := next - prev
	if gap <= 0 || n <= 0 {
		return nil // 退化：调用方全量重排
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = prev + gap*float64(i+1)/float64(n+1)
	}
	return out
}

// RenumberView 把 view 的 Items 按现有顺序重排为 1..n（Fractional Index
// 的退化回退路径：插入点间距耗尽时整体重编号，顺序不变、字节不受影响）。
//
// 并发：修改传入 view（仅在本包内、作用于已 clone 的副本上调用）。
func RenumberView(view *types.ContextView) {
	for i := range view.Items {
		view.Items[i].Position = float64(i + 1)
	}
}
