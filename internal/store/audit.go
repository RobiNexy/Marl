package store

import (
	"context"
	"time"

	"marl/internal/types"
)

// AuditEvent 是一条审计记录（原则 1 的副产品：一切结构性变更集中审计）。
//
// Action 是自由字符串约定，如 "spawn" / "orchestrate" / "model_upgrade" /
// "reconfigure" / "discussion" / "escalation" / "compression"；Target 是它的
// 对象（如 Op 名 / 模型名），Payload 是结构化上下文。
//
// 为什么 Action 是自由字符串而不是枚举：审计是**开放集合**——新增模块必须
// 能在不改动 store 包的前提下开始记录。代价是拼写错误无法被编译器发现，
// 因此约定 Action 只允许小写下划线形式，且各模块必须把 Action 名定义为
// 常量（禁止在 Append 调用点写字面量）。
//
// 零值契约：零值 AuditEvent{} 非法（无 AgentID、无 Action）。Seq 与 Timestamp
// 在构造期为零值是**正常的**——它们由 AuditStore.Append 就地回填
// （与 MessageLog.Append 同一约定），因此本结构与 LogEntry 一样是
// "构造期可写、落库后冻结"。
type AuditEvent struct {
	Seq       int64
	AgentID   types.AgentID
	Timestamp time.Time
	Action    string
	Target    string
	Payload   any
}

// AuditStore 是审计事件存储接口。
// 编排操作也进审计：每个 Op 执行记 audit_events(action="orchestrate",
// target=Op名, payload=args)——编排历史完整可查（Part 3.5）。
//
// 审计与 MessageLog 的关键差异（决定了它们的持久化取舍）：审计是**旁路证据**，
// 不参与上下文编译，因此它可以被独立裁剪/归档而不影响任何 Agent 的行为；
// 而 Message Log 是真相之源，任何裁剪都必须是追加式投影。
type AuditStore interface {
	// Append 追加一条审计事件。
	//
	// 前置条件：ev 非 nil，ev.AgentID 与 ev.Action 非空。
	// 后置条件：ev.Seq 与 ev.Timestamp 被**就地回填**（ev 是输出参数，
	// 与 MessageLog.Append 对 LogEntry 的处理一致）。返回值只有 error——
	// 需要 Seq 的调用方从 ev.Seq 读。
	//
	// 失败：AgentID/Action 为空 → ErrInvalid；存储不可写 → ErrClosed/底层错误。
	//
	// 关键约定：审计写入**不得**因业务失败而回滚或丢弃。若一次操作的业务结果
	// 是失败，审计必须记录"发生了并失败了"；把失败操作从审计里抹掉，等于把
	// 审计变成"成功清单"，从而失去它唯一的用途。
	//
	// 并发：必须支持多 Agent 并发追加（审计是热点路径，每个 Op 都写）。
	Append(ctx context.Context, ev *AuditEvent) error

	// Query 返回符合条件的事件，按 Seq **升序**（审计是时间序列，天然应正序
	// 阅读；倒序由调用方自行反转，避免每个实现都做一次无谓的排序）。
	//
	// 失败：Limit < 0 → ErrInvalid。
	// 并发：只读，必须可并发调用。
	Query(ctx context.Context, filter AuditFilter) ([]*AuditEvent, error)
}

// AuditFilter 是审计查询条件。
//
// 零值语义：AuditFilter{} 表示"全部事件"——各字段零值都是"不限"。
// 这是刻意的：审计排查往往从"这个 Agent 最近发生了什么"开始，逐层收窄。
//
// Limit 例外：0 表示"用实现默认上限"（不是"不限"），负值非法（ErrInvalid）。
// 理由与 LogQuery.Limit 相同：审计表会随时间无限增长，让 0 表示"不限"
// 等于给一次误写的查询准备了 OOM 机会。
type AuditFilter struct {
	AgentID  types.AgentID // "" = 不限
	Action   string        // "" = 不限（如 "orchestrate"）
	FromTime time.Time     // 零值 = 不限
	ToTime   time.Time     // 零值 = 不限
	Limit    int
}
