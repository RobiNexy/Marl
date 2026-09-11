package store

import (
	"context"

	"marl/internal/types"
)

// MessageLog 是消息库的存储接口（Part 3.2）。
//
// 追加为主、近似不可变：本接口不提供 Update/Delete。任何对历史的修改
// 只能是追加新条目（摘要、切块都走 Append）。唯一可接受的"修改"是
// 审计口径的补充（如 audience 迁移），另行走迁移流程。
//
// 所有权契约（适用于全部方法，容易忽略但很重要）：
//   - 传入的 entry 是**输出参数**：Append 会就地回填 ID / Seq / CreatedAt。
//     因此同一个 *LogEntry 不得被两个 goroutine 同时 Append，也不得在
//     Append 之后再被构造者继续修改（append 之后即冻结）。
//   - 返回的 *LogEntry 由实现新分配，调用方拥有；但**不得修改**
//     SourceIDs/Meta 的底层数据。把不可变性建立在"数量上只有一个写者"的
//     约定上，而不是靠深拷贝——深拷贝会给每次读取都加上无谓的分配成本。
type MessageLog interface {
	// Append 追加一条 LogEntry，分配 MessageID 与全局递增 Seq，落 CreatedAt。
	//
	// 前置条件：entry 非 nil 且通过 entry.Validate()（Role/Prov/Audience 合法）。
	// 后置条件：entry.ID / entry.Seq / entry.CreatedAt 被就地回填；
	// 返回值等于 entry.ID。
	//
	// 失败：entry 未通过校验 → ErrInvalid（调用方 bug，不重试）；
	// 存储不可写/已关闭 → ErrClosed / 底层错误。
	//
	// Seq 的约定：从 1 开始全局递增（每 Agent 独立序列），**0 保留为
	// "尚无记录"的哨兵**（见 LastSeq）。这样"没有记录"与"第一条记录"可区分。
	//
	// 并发：必须支持多 Agent 并发追加；同一 Agent 的 Seq 分配必须原子，
	// 不允许出现空洞或重号（空洞会让 Range 查询的连续性假设失效）。
	Append(ctx context.Context, entry *types.LogEntry) (types.MessageID, error)

	// Get 按 ID 取一条。失败：不存在 → ErrNotFound。
	Get(ctx context.Context, id types.MessageID) (*types.LogEntry, error)

	// GetBySeq 按 (AgentID, Seq) 取一条（Seq 是排序与引用的钥匙，Part 3.2）。
	// 失败：不存在 → ErrNotFound；seq == 0 → ErrInvalid（0 是哨兵，不是合法 Seq）。
	GetBySeq(ctx context.Context, agentID types.AgentID, seq int64) (*types.LogEntry, error)

	// Range 返回 [fromSeq, toSeq]（含端点）按 Seq 升序的条目。
	//
	// 失败：fromSeq > toSeq 或 fromSeq == 0 → ErrInvalid。
	// 这里选择 fail fast 而不是"返回空切片"：空切片会把"参数写反了"这个
	// 程序缺陷伪装成"这段时间没有消息"，而后者是正常业务状态。
	// 区间内无条目属正常情况，返回空切片 + nil error。
	Range(ctx context.Context, agentID types.AgentID, fromSeq, toSeq int64) ([]*types.LogEntry, error)

	// Latest 返回该 Agent 最近 N 条（按 Seq 倒序取，按 Seq 升序返回）。
	//
	// 失败：limit < 0 → ErrInvalid。limit == 0 → 返回空切片（合法：调用方
	// 可能在配置里把取值置零表示"不要"）。limit 大于总条目数时返回全部，不报错。
	Latest(ctx context.Context, agentID types.AgentID, limit int) ([]*types.LogEntry, error)

	// LastSeq 返回该 Agent 当前最大 Seq。
	// 无记录时返回 **0**（哨兵值，不是合法 Seq——Seq 从 1 开始）。
	LastSeq(ctx context.Context, agentID types.AgentID) (int64, error)

	// TotalTokens 返回该 Agent 的 est-token 累计（Watchdog 预算口径）。
	//
	// 口径说明：这是**本地估算**（TokenEst）的累计，不是实际计费口径；
	// 真实成本看 Ledger。两者混用会让"预算还剩多少"与"花了多少钱"互相污染。
	TotalTokens(ctx context.Context, agentID types.AgentID) (int64, error)
}

// LogQuery 是 Log 的复合查询过滤条件（阶段 2 起按需使用）。
//
// 零值语义：各字段零值均表示"不限"（Roles 为空、MinSeq/MaxSeq 为 0 即
// 不设边界、Ascending 为 false 即倒序）。这是刻意的——过滤器应该能从零值
// 逐步收窄，而不是每加一个条件都要显式关掉其它条件。
//
// Limit 例外：0 表示"用实现默认上限"，负值非法（ErrInvalid）。之所以不让
// 0 表示"不限"，是为了避免一次误写的查询把整张表读进内存。
type LogQuery struct {
	AgentID   types.AgentID
	Roles     []types.InternalRole // 空 = 不限
	MinSeq    int64
	MaxSeq    int64
	Ascending bool
	Limit     int
}
