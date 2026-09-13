package orchestrate

import (
	"context"
	"fmt"

	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// SUM 七段骨架的固定章节标题（Part 3.7 步骤 5：机械检查章节标题）。
// 骨架顺序：任务 / 事实 / 文件 / 决策 / 未闭 / 失败 / 现场。
//
// 这些字符串是**持久化格式的判据**：已落库的 SUM 条目里就是这些标题，
// 校验器靠它们判断"摘要结构完整"。因此不得改字面值（改了就再也校验不了
// 历史 SUM），也不得改顺序（顺序即阅读顺序，也是 Part 3.7 的骨架定义）。
const (
	SUMSectionTask      = "## 1. 任务"
	SUMSectionFacts     = "## 2. 事实"
	SUMSectionFiles     = "## 3. 文件"
	SUMSectionDecisions = "## 4. 决策"
	SUMSectionOpen      = "## 5. 未闭"
	SUMSectionFailures  = "## 6. 失败"
	SUMSectionScene     = "## 7. 现场"
)

// sumSectionList 是章节校验顺序的唯一来源。
//
// 刻意用**数组**而非切片，并保持非导出：数组的长度进类型（用于下面的编译期
// 断言），而非导出则保证外部无法重排/追加。原先它是一份可导出的 []string，
// 任何导入方一次 append 或就地交换就能让校验顺序与骨架定义不符——而校验
// 仍然"通过"，只是通过了错误的顺序。这类缺陷不会报错，只会在压缩质量上
// 缓慢体现，极难归因。
//
// 与 skill 包的 skillNameList 同一模式（见 ADR-0015：冻结前缀/顺序需有
// golden 测试守护，本文件亦有对应的 TestSUMSectionsGoldenOrder）。
var sumSectionList = [...]string{
	SUMSectionTask, SUMSectionFacts, SUMSectionFiles, SUMSectionDecisions,
	SUMSectionOpen, SUMSectionFailures, SUMSectionScene,
}

// SUMSectionCount 是骨架章节数（Part 3.7 定义 7 段）。
const SUMSectionCount = 7

// 编译期断言：sumSectionList 的长度必须等于 SUMSectionCount。
// 往骨架里增删章节而不更新常量会在编译期失败，而不是在运行期悄悄少校验一节。
var _ [SUMSectionCount - len(sumSectionList)]struct{}
var _ [len(sumSectionList) - SUMSectionCount]struct{}

// SUMSections 返回章节校验顺序的**副本**。
//
// 返回副本而非共享切片：调用方拿到后可能排序、过滤或拼接（如"跳过第 3 节
// 不做路径校验"），若共享底层数组，一次排序就会改掉全局校验顺序。
// 纯查询函数，但为了杜绝上述风险，每次调用都分配——这个量级（7 个字符串）
// 的分配远小于一次误改的排查成本。
//
// 并发：安全（只读全局数组 + 本地分配）。
func SUMSections() []string {
	out := make([]string, len(sumSectionList))
	copy(out, sumSectionList[:])
	return out
}

// CompressionPolicy 是压缩的触发与保留策略（Part 3.7）。
//
// 零值契约：零值 CompressionPolicy{} **非法**（见 Validate）。这不是形式主义，
// 这些字段的零值多数会造成具体损害，而且都不报错：
//   - HeadroomThreshold=0：headroom < 0 才触发 → 压缩几乎从不触发，
//     直到上下文真的溢出，那时已经没有腾挪空间；
//   - KeepTailTurns=0：尾部一条不留 → 直接抹掉最近 N 轮的完整对话，
//     而这恰好是 Agent 最需要精确记忆的部分（压缩目标是中段历史）；
//   - MinReclaimFraction=0：任何压缩结果都被接受，包括"压完反而更长"
//     （收益为负也视为达标）。
//
// MaxRetries 是唯一**零值合法**的字段（显式表示不重试）：它只是放弃 Part 3.7
// 步骤 5 建议的那一次重试，而失败会走降级路径，所以危害是可见的；
// 但它仍不是推荐配置——Part 3.7 给的默认是 1。
// 注意：字段级"零值合法"不等于"整个结构零值合法"，Validate 仍会拒绝
// 零值策略（因为前三个字段的零值不可接受）。
//
// 因此构造本结构必须显式填值；Part 3.7 给出的默认值（尾部 3 轮、重试 1 次）
// 由**配置层**物化后传入，本包不提供 DefaultCompressionPolicy()——
// 一旦提供，调用方就会拿它当"忘了配也能跑"的兜底，而阈值本身依赖模型
// 窗口与估算误差（Part 文档 3843 行提到估算偏 20% 会让触发时机失准），
// 不该由框架凭空给一个数字。
type CompressionPolicy struct {
	// HeadroomThreshold：headroom = model_max_tokens - current_context - budget_reserved，
	// 低于该值触发压缩。必须 > 0。
	//
	// 触发判断本身由**调用方**（主循环 / 预算管理）执行，Compress 假定自己
	// 已被触发。策略对象同时被触发点与压缩器读取，以保证"用同一组配置判断
	// 与执行"——否则调大阈值后压缩行为会与触发条件不一致。
	HeadroomThreshold int
	// KeepTailTurns：尾部保留的最近轮数（Part 3.7 默认 3）。必须 > 0。
	//
	// 语义边界：一轮 = 一次 user/assistant 往返（含其中的工具调用）。
	// 该定义必须与 ContextView 的分组口径一致，否则"保留 3 轮"在两边
	// 会切出不同的位置。
	KeepTailTurns int
	// MinReclaimFraction：压缩收益下限：reclaim < 该值则升级到 L2
	// （阶梯上一级，不是 reconfigure 换模型）。必须严格落在 (0,1) 内。
	MinReclaimFraction float64
	// MaxRetries：SUM 校验不通过时的重试次数（温度稍高），再不过则降级到
	// L2 或 escalate。0 是**合法**值（显式表示不重试），负值非法。
	//
	// 与其它三个字段不同，这里 0 有明确语义且不造成静默损害：它只是放弃
	// Part 3.7 建议的那一次重试，而失败会走降级路径（可见）。
	MaxRetries int
}

// Validate 报告策略是否可用。
//
// 失败：HeadroomThreshold <= 0；KeepTailTurns <= 0；MinReclaimFraction 不在
// (0,1) 开区间内；MaxRetries < 0。（NaN 落在开区间判定之外，同样被拒绝。）
// 并发：纯函数。
func (p CompressionPolicy) Validate() error {
	switch {
	case p.HeadroomThreshold <= 0:
		return fmt.Errorf("compression policy: headroom threshold %d must be > 0", p.HeadroomThreshold)
	case p.KeepTailTurns <= 0:
		return fmt.Errorf("compression policy: keep tail turns %d must be > 0", p.KeepTailTurns)
	case !(p.MinReclaimFraction > 0 && p.MinReclaimFraction < 1):
		// NaN 时两个比较都为 false，同样落到这里——开区间判定天然排除 NaN。
		return fmt.Errorf("compression policy: min reclaim fraction %v must be in (0,1)", p.MinReclaimFraction)
	case p.MaxRetries < 0:
		return fmt.Errorf("compression policy: max retries %d must be >= 0", p.MaxRetries)
	}
	return nil
}

// CompressionResult 是一次压缩的产出（Part 3.7）。
//
// 成功结果的非零不变量：View 非 nil，且 View.AgentID 与被压缩的 view.AgentID
// 相同（压缩不得改变归属）；SUMENTry 非 nil **且已成功追加进 Log（Seq 已回填）**
// ——唯一的例外是"L0 机械清理已达标、跳过 L2"的路径：此时 SUMENTry 为 nil，
// Reclaim 仍按真实测量给出（见 Compressor.Compress 的开放边界说明）。
//
// Reclaim 的口径与边界：
//   - 恒等式 Reclaim = (old_tokens - new_tokens) / old_tokens，其中 token 数
//     用**本地估算**（TokenEst）——与 TotalTokens 同口径，便于互相对照；
//   - 可以为 0 或负数（摘要比被压缩区还长，例如给极短中段做摘要）。
//     **不做 clamp**：把负收益伪装成 0 会让"压缩反而变长"这一必须上报的信号
//     消失，而正是这个信号决定要不要降级到 L2；
//   - old_tokens == 0 时定义为 0（无内容可压），不允许出现 NaN/±Inf——
//     NaN 与任何阈值比较都为 false，会让"收益达标判定"静默失效。
type CompressionResult struct {
	// View 是压缩后的新 View = [保留头部] + [SUM] + [保留尾部]。
	View *types.ContextView
	// SUMEntry 是追加到 Log 的摘要条目（ProvSummaryOf，SourceIDs 指向压缩区所有消息）。
	SUMENTry *types.LogEntry
	// Reclaim = (old_tokens - new_tokens) / old_tokens。
	Reclaim float64
	// L0Pruned 是 L0 机械清理阶段丢弃的条目数（去重 file_read / 剔除旧读取 /
	// exclude 历史 thinking）。这些只动 View，不动 Log。
	//
	// 单独报出这个数字的意义：L0 不花 LLM 成本，收益与 SUM 是两笔账。
	// 混在一起会让"压缩到底值不值"无法归因（可能 L0 已解决大半）。
	L0Pruned int
}

// Compressor 是框架自动触发的压缩流程（Part 3.7）。
//
// 注意：压缩不是 LLM 主动调用的工具，是框架在 headroom 不足时自动触发的编排操作。
// 执行期间主 Agent 暂停——表达方式是 StateBlocked 配上
// BlockReason=BlockCompressing：AgentState 没有独立的 compressing 值，
// 因为"暂停等一件事"是同一个状态，区别只在原因（见 types.AgentState 的状态机
// 不变量）。文档 Part 3.7 步骤 1 写作 "StateCompressing"，落地时按此映射。
//
// 状态由调用方管理：Compressor 的实现**不**改 Agent 状态（它只收到 Log 与 View），
// 因此"暂停-压缩-恢复"的时序属于调用方（主循环）的责任。
type Compressor interface {
	// Compress 执行一次完整压缩：L0 机械清理 → 调 Orchestrator 生成 SUM →
	// 校验 SUM → 估算收益 → 追加 SUM → 构造新 View。
	//
	// 前置条件：
	//   - view 非 nil；policy.Validate() 通过（零值策略必须被拒绝，
	//     见 CompressionPolicy 零值契约）；
	//   - 调用方已判定需要压缩（headroom 判断在调用方，本方法不重复判断）；
	//   - L0 只在这里执行一次（Part 3.7 步骤 3），其它路径不得重复执行。
	//
	// 后置条件：
	//   - 传入的 view **不被修改**（与 Operation.Apply 同一约定，便于重放）；
	//   - 返回的 View 是全新对象，AgentID 不变；
	//   - 追加的 SUM 条目已进 Log（Seq 已回填），但返回的 View **暂不引用**它——
	//     主 Agent 拿到 entry.ID 后自己决定用不用（避免编排污染主干）。
	//
	// 失败：
	//   - 策略非法（零值）→ store.ErrInvalid；
	//   - L0 之后中段无可压缩区间（或 L0 已独自达标）→ **不得返回 (nil, nil)**。
	//     两种可接受表达，实现阶段二选一并写进 ADR：返回可归因的错误
	//     （调用方据此跳过压缩），或返回 SUMENTry 为 nil 的"空结果"
	//     （Reclaim 仍按真实测量给出）。共同要求：调用方必须能区分
	//     "压缩没做"与"压缩做了但失败"；
	//   - SUM 校验在 MaxRetries 次重试后仍不过 → 返回错误，由调用方降级到 L2 或 escalate。
	//
	// 开放边界（实现阶段需写进 ADR）：L0 机械清理自己就可能已经腾够空间。
	// 此时是否仍要跑 L2（花一次 LLM 调用生成 SUM），Part 3.7 未明说。
	// 但无论选哪种，判据只能有**一处实现**——触发点与 Compress 各自算一遍
	// "是否已达标"，两份判据迟早会不一致，表现为"有时压、有时不压"。
	Compress(ctx context.Context, log store.MessageLog, view *types.ContextView, policy CompressionPolicy) (*CompressionResult, error)

	// Admit 把生成好的 SUM 追加进 Log 并构造新 View。
	// 产出追加到主 Log 但主 View 暂时不引用——主 Agent 拿到 entry.ID 后自己决定用不用。
	//
	// 为什么需要这个方法（与 Compress 的关系）：Compress 是"全自动"路径；
	// Admit 是"内容已在手上"的路径（例如人类在讨论中写好了摘要、或重放审计
	// 里的 SUM）。两者共享同一段落盘与建 View 的逻辑，因此拆成同一接口的两个方法。
	//
	// 前置条件：sumContent 非空；sourceIDs **非空**（血缘是摘要的唯一凭据，
	// 一条没有出处的摘要无法参与任何追溯）；调用方已自行跑过 ValidateSUM
	// （Admit 不重复校验——该校验含文件路径存在性检查，属于 I/O，重复执行
	// 没有收益，而漏跑会在最后一步由 ValidateSUM 的调用点暴露）。
	//
	// 失败：sumContent 为空或 sourceIDs 为空 → store.ErrInvalid；
	// sourceIDs 里存在 Log 中不存在的 ID → store.ErrNotFound
	// （宁可失败也不接受悬空引用：悬空 SourceIDs 会让"从摘要回溯到原文"
	// 这条路断在中间，而摘要本身看起来完整）。
	Admit(ctx context.Context, log store.MessageLog, view *types.ContextView, sumContent string, sourceIDs []types.MessageID) (*CompressionResult, error)

	// ValidateSUM 做机械校验：章节标题（## 1. 至 ## 7.）+ 第 3 节文件清单的路径存在性。
	//
	// 机械校验的含义：不调用 LLM、不做语义判断。只查"结构是否齐全"，
	// 不查"内容是否属实"——后者按原则 4 由下游的机械检查（路径存在性、
	// 报告与 diff 对照）承担，而不是让校验器变成第二个模型。
	//
	// 检查基准（workspace 根）：由实现**构造时注入**，不出现在签名里。
	// 若把 root 作为参数，每个调用点都要重复传入同一个值，传错一次就会
	// 让路径存在性检查静默失效（相对路径按错误的根解析，结果多为"不存在"，
	// 看起来像模型写错了路径）。
	//
	// 失败：sumContent 为空、缺失任一章节、第 3 节列出的路径不存在。
	// 多个缺项时应**聚合**报告（一次列全），否则修复者要按重试次数逐轮发现，
	// 而每次重试都是一次带温度的 LLM 调用。
	//
	// 并发：必须可并发调用（只读 + 文件系统查询），实现不得持有可变状态。
	ValidateSUM(ctx context.Context, sumContent string) error
}
