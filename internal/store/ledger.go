package store

import (
	"context"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

// CostCategory 是三类独立记账的调用类别（Part 7.5）。
//
// 零值契约：零值 CostCategory("") 非法。理由是这个值直接决定成本报表的
// 分栏归属——一个未分类的调用会从三个分栏里同时消失（总账与分项之和对不上），
// 而"对不上"这种症状通常会被归因成浮点误差，从而长期潜伏。
// 因此 Ledger.Record 必须校验（fail fast），不允许静默归入 main。
type CostCategory string

const (
	CallMain          CostCategory = "main"          // 主任务的 Execute
	CallOrchestration CostCategory = "orchestration" // 压缩、split 等编排调用（用 r0，不计入主任务预算）
	CallDiscussion    CostCategory = "discussion"    // 人机讨论里的 Agent 起草（用 r0，单独核算）
	CallLLMCall       CostCategory = "llm_call"      // llm_call 意图的 sidecar 调用（阶段 11；报表按 purpose 再分）
)

// Valid 报告 c 是否为三个已定义类别之一。零值返回 false。
//
// 并发：纯函数。
func (c CostCategory) Valid() bool {
	switch c {
	case CallMain, CallOrchestration, CallDiscussion, CallLLMCall:
		return true
	}
	return false
}

// LedgerEntry 是每次 LLM 调用后记的一条账（Part 7.5）。
// TokenUsage 必须是 Denormalizer 归一化后的形态（已能直接算钱的）。
//
// 不变量：
//   - TaskID 非空（一份不能归到任何任务的账无法参与任何报表）；
//   - AgentID / Rung / CallType 非空且合法；
//   - Currency 非空。它与 Cost 必须**成对**记录：只记数字不记币种，
//     会把多币种账本悄悄混成一张总表（1 CNY + 1 USD = 2 什么？）。
//
// [偏离文档: 文档 Part 7.5 把字段定义为 CostUSD float64（"按汇率算"），
// 但同一份文档的 Rung.Currency 是 "CNY"、Part 7.6 的报表也以 ¥ 计价——
// 文档自身不一致。此处改为 Cost + Currency：币种跟随配置（阶梯里声明什么
// 就是什么），报表按币种分组。这样跨币种场景只需在报表层换算一次，
// 而各条历史记录始终保留"当时按什么币种算的"，不会因为后来改了汇率
// 或者改了阶梯币种而对不上账。]
type LedgerEntry struct {
	TaskID     types.TaskID
	AgentID    types.AgentID
	Rung       types.RungID // "r0" / "r1" / "r2"
	CallType   CostCategory
	TokenUsage types.TokenUsage
	Cost       float64 // 按 rung 的 cost_per_mtok 与 Currency 算出
	Currency   string  // 如 "CNY"；与 Cost 成对，不允许单独出现
	Timestamp  time.Time
}

// Ledger 是成本账本的存储接口。三类分开记账，报表按阶梯分项。
//
// 所有权契约：Record 把 entry 当作**输出参数**——Timestamp 为零值时由实现
// 就地回填当前时间。因此同一 *LedgerEntry 不得被并发 Record。
type Ledger interface {
	// Record 记入一笔。
	//
	// 前置条件：entry 非 nil；TaskID/AgentID/Rung/Currency 非空；
	// CallType 合法（零值返回 ErrInvalid，见 CostCategory 零值契约）。
	// 后置条件：entry.Timestamp 为零值时被就地回填。
	//
	// 失败：字段缺失或非法 → ErrInvalid；存储不可写 → ErrClosed/底层错误。
	//
	// 并发：必须支持多 Agent 并发记账（每个 Agent 每次调用都写）。
	Record(ctx context.Context, entry *LedgerEntry) error

	// RecordOrchestration 是 Record 的便捷包装，**强制**把 entry.CallType
	// 设为 CallOrchestration（覆盖调用方传入的值）。
	//
	// 覆盖是刻意的：调用点（压缩、split）的语义本身就是"这是一次编排调用"，
	// 让方法名而非调用方来承担这个判断，可以少一处可能写错的地方。
	// 所以传进来的 CallType 无论是什么都会被改写——这不是副作用，是契约。
	RecordOrchestration(ctx context.Context, entry *LedgerEntry) error

	// RecordDiscussion 是 Record 的便捷包装，**强制** CallType=CallDiscussion。
	// 覆盖语义同 RecordOrchestration。
	RecordDiscussion(ctx context.Context, entry *LedgerEntry) error

	// TaskSummary 汇总一个任务的三类/各级花费（Part 7.6 成本报表的输入）。
	//
	// 失败：taskID 为空 → ErrInvalid；该任务无任何记账记录 → ErrNotFound
	// （**不是**返回一份全零的摘要——"没有记录"与"花了 0 元"是不同的结论，
	// 后者会让报表显示"成本 0"从而掩盖"记账根本没生效"这个严重故障）。
	// 并发：只读，必须可并发调用。
	TaskSummary(ctx context.Context, taskID types.TaskID) (*TaskCostSummary, error)

	// RecordModelSwitch 记录 Reconfigure 换模型时的缓存失效审计（Part 7.7）。
	//
	// 关键约束（Part 7.7）：只有换 model_id 或 adapter 才调用本方法。
	// 只改 temperature/top_p 等采样参数**不触发**缓存失效（缓存键是前缀内容，
	// 采样参数不在键内），因此不得调用——否则审计里会充斥"假失效"事件，
	// 让"缓存失效成本"这一指标失去意义。
	RecordModelSwitch(ctx context.Context, ev *ModelSwitchEvent) error
}

// TaskCostSummary 是一次任务的总账（Part 7.6 报表结构）。
//
// 不变量：TotalTokens/TotalCost 必须等于各自分项之和（ByLevel 与 ByCategory
// 是两个维度的切分，二者总量相等）。这条不是"文档规定"，而是让报表自证的
// 必要条件——总账与分项对不上时，报表数字就不可信，而成本报表的全部价值
// 就在于被信任。
//
// Currency 与各分项 Cost 同币种；不允许把不同币种折算后塞进同一个汇总
// （见 LedgerEntry.Currency）。
type TaskCostSummary struct {
	TaskID      types.TaskID
	Duration    time.Duration
	Status      types.TaskStatus
	Currency    string
	TotalTokens int64
	TotalCost   float64
	ByLevel     map[types.RungID]*LevelSummary // key = rung，如 "r0"
	ByCategory  map[CostCategory]*LevelSummary // key = main / orchestration / discussion
}

// LevelSummary 是某一阶梯（或类别）的分项汇总。
//
// Token 口径：Tokens 是总 token（含思维链与缓存）；ReasoningTokens/CacheRead/
// CacheWrite 是其中的细分。因此恒有
//
//	ReasoningTokens + CacheRead + CacheWrite <= Tokens
//
// 恒等号不成立——普通输入/输出 token 属于剩余部分。报表里的"思维链占比"
// 就是 ReasoningTokens/Tokens（Part 7.6：>60% 长期出现即在刷思维链）。
type LevelSummary struct {
	Calls           int
	Tokens          int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite      int64
	Cost            float64
}

// ModelSwitchEvent 是换模型导致的缓存失效审计（Part 7.7，审计、不硬拦截）。
//
// 语义：CacheHitsBefore / CacheWritesBefore 是**失效前**该缓存桶累计的命中与
// 写入量——前者代表"已经摊销掉的缓存价值"，失效后要重新摊销。这两个数字是
// 决策依据（"这次换模型值不值"），因此必须来自真实统计，不允许估算填充。
//
// 零值契约：ModelSwitchEvent{} 非法（无 AgentID、无 From/ToModel）。
// CacheHitsBefore/CacheWritesBefore 为零是**合法**的（确实从未命中过），
// 与"未统计"不可区分——但因为它们只是审计参考值而非报表总额，这里选择
// 接受这个歧义，不引入指针（避免为审计字段增加复杂度）。
type ModelSwitchEvent struct {
	AgentID           types.AgentID
	TaskID            types.TaskID
	FromModel         string
	ToModel           string
	Reason            string
	CacheHitsBefore   int64 // 已摊销掉的缓存价值；失效后要重新摊销
	CacheWritesBefore int64
	At                time.Time
}
