package orchestrate

import (
	"context"

	"marl/internal/store"
	"marl/internal/types"
)

// OpKind 是编排操作的原子种类（Part 3.5）。
//
// 零值契约：零值 OpKind("") 非法。消费侧（Op 分发器、审计回放）遇到未识别
// 的 kind 必须**拒绝执行**，不允许"当作空操作跳过"——编排操作会改 View，
// 跳过一条会让后续 Position/血缘的计算依据与实际不符。
type OpKind string

const (
	OpSplit    OpKind = "split_message"    // 拆分一条消息（semantic / delimiter）
	OpExclude  OpKind = "exclude_message"  // 软删除（Visible=false）
	OpRestore  OpKind = "restore_message"  // 撤销软删除（Visible=true）
	OpReorder  OpKind = "reorder_message"  // 调整 Position（Fractional Index）
	OpAnnotate OpKind = "annotate_message" // 加批注外壳，追加 ProvAnnotatedOf
	OpPin      OpKind = "pin_message"      // 设置裁剪豁免
	OpUnpin    OpKind = "unpin_message"    // 取消裁剪豁免
)

// Valid 报告 k 是否为已定义操作之一。零值返回 false。
//
// 字面值是**持久化格式**（Op 名会进 audit_events 的 target 列，见 Part 3.5），
// 因此不得改值；改名等于让历史审计无法回放。
//
// 并发：纯函数。
func (k OpKind) Valid() bool {
	switch k {
	case OpSplit, OpExclude, OpRestore, OpReorder, OpAnnotate, OpPin, OpUnpin:
		return true
	}
	return false
}

// Operation 是编排操作的统一契约（Part 3.5）。
//
// 输入是 Log（只读真相）+ 当前 View；输出是**新的** View（可能追加 Log 条目）。
// 实现不得就地修改传入的 View——编排的可组合性依赖每一步都有确定的输入输出。
//
// 为什么强调"新的 View"而不是就地改：编排要能**重放**（审计里有 payload，
// 事故复盘要把同一串 Op 重新跑一遍）。就地修改会让重放依赖上一步的残留状态，
// 一旦中间某步的实现变了，重放结果就不再可比。
type Operation interface {
	Kind() OpKind

	// Apply 执行操作。
	//
	// 对 Log 的使用约束：只能 Append（如 split 追加 ProvSplitOf 条目），
	// 不能改写既有条目。追加的条目必须带完整血缘（SourceIDs）。
	//
	// 后置条件：
	//   - 返回的 OpResult.View 非 nil 且 AgentID 与传入 view.AgentID 相同
	//     （编排不得改变 View 的归属）；调用方负责把它写回（ViewStore.SaveView）；
	//   - 返回的 OpResult.View 与传入的 view **不共享可变状态**：实现必须先
	//     拷贝再改。传入的 view 在 Apply 返回后必须与调用前逐字段相等——
	//     这是"不就地修改"的可检验形式（测试用深比较断言）。
	//
	// 失败：kind/参数非法 → store.ErrInvalid（编排参数的校验属于调用方 bug）；
	// 引用的消息在 Log 里不存在 → store.ErrNotFound。
	//
	// 幂等性：**不要求幂等**。exclude 一条已 exclude 的消息、split 一条已 split
	// 的消息，结果是再次追加一条记录（Log 只追加）。需要幂等的地方由上层
	// （查看当前 View 状态）来保证，而不是让每个 Op 各自实现去重。
	//
	// 并发：单个 Operation 实例必须可被并发 Apply（编排可能并行处理多条消息），
	// 因此实现不得在 Operation 上保存可变状态。
	Apply(ctx context.Context, log store.MessageLog, view *types.ContextView) (*OpResult, error)
}

// OpResult 是一次编排操作的产出。
//
// 三个字段的关系：Appended 是**追加进 Log 的原始条目**，View 是**如何引用它们
// 的投影**。两者不可互相推导——同一条 Appended 条目可能不在 View 里（生成的
// 摘要可能暂不被引用），View 里也可能引用早先就已存在的条目。因此审计必须
// 同时记录"追加了什么"与"View 怎么变"，只记其一都无法还原现场。
type OpResult struct {
	// View 是新的 View。调用方负责把它写回（ViewStore.SaveView）。
	View *types.ContextView
	// Appended 是本次操作追加进 Log 的条目（split / annotate 会产出）。
	Appended []*types.LogEntry
	// AuditPayload 是要记入 audit_events 的结构化参数。
	//
	// 必须可 JSON 序列化（审计要落盘/导出）。不允许塞入 *LogEntry、
	// channel、func 等——这类值序列化失败只会在审计落盘时才暴露，
	// 而那时业务已经完成，审计丢失是静默的。
	AuditPayload map[string]any
}

// AuditAction 是编排操作在审计表里的 action 名（Part 3.5）。
//
// 定义为常量而非字面量：审计的 action 是查询键（"查所有编排操作"），
// 任何一处写字面量拼错都会让那次操作从查询结果里消失，而拼错不会报错。
const AuditAction = "orchestrate"
