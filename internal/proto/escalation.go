package proto

import (
	"fmt"

	"github.com/RobiNexy/Marl/internal/types"
)

// EscalationRequest 是"沿上报链冒泡的求助消息"（Part 11.3）。
// 与讨论的区别：目标是上级 Agent 或人类，产物是一段回复消息（进 Log），
// 而非落地的文件。
//
// 不变量：From / Reason / Question 均非空。
//   - Reason 是 Agent 自述的**卡点**，不是复述任务（复述任务对回复方零信息）；
//   - Question 必须是一个能被直接回答的问题——本级自己都没法把问题收敛成
//     一句问话，说明还没到该升级的时候，框架据此拒绝上报（避免把"再想想"
//     包装成求助抛给人类）。
//
// TraceID 用 types.TraceID 而非裸 string：本结构的 TraceID 必须与 reply 的
// 一致（同一因果链），两者是同一批字段的互换风险点，靠命名类型在编译期挡住。
type EscalationRequest struct {
	From           types.AgentID
	EscalationID   types.EscalationID
	TraceID        types.TraceID
	Reason         string // 求助原因（Agent 自述），如 "阶梯已到顶仍搞不定"
	ContextSummary string // 上下文摘要
	Question       string // 明确的问题，便于回复方直接作答
}

// EscalationReply 是上级（父 Agent 或人类）的回复。
// 人类回复经文件信箱（requests/pending/ → done/）由框架读取并打包成此结构。
// 正文由框架包装成 RoleEscalationReply 的 LogEntry 追加到发起者 Log 并解除阻塞。
//
// 不变量：From / Content / TraceID 均非空；TraceID 必须**等于**对应
// EscalationRequest.TraceID，否则回复会被挂到错误的因果链上（可见症状是
// 发起者等不到解除阻塞，而信箱里那封回复看起来完全正常）。
//
// IsHuman 的作用：人类回复时 From 是框架分配的虚拟 ID，因此 From 无法告诉
// 消费方"这是人写的"。凡是行为依赖"是否人类"的地方（如审计标记、是否能
// 作为裁决凭据）都必须看这个布尔值，不能靠 From 猜。
type EscalationReply struct {
	From    types.AgentID // 父 Agent ID；人类回复时为框架分配的虚拟 ID
	IsHuman bool
	Content string
	TraceID types.TraceID
}

// EscalationRule 决定 escalation 上报给谁（Part 11.3 流程 2）：
// 有父且父不在 Blocked → 发给父；否则 → 发给人类信箱。
//
// 零值契约：零值 EscalationRule{} 非法——它不是"没有出口"的合法表达，
// 而是配置缺失：ParentID 为空且 FallbackHuman=false 时，上报表意无出口，
// 消费侧必须返回错误（ErrInvalid 语义）而不是静默丢弃求助。丢弃求助的后果
// 是发起者永久阻塞在 BlockEscalating，而系统各处都没有报错。
type EscalationRule struct {
	ParentID      types.AgentID
	ParentBlocked bool
	FallbackHuman bool
}

// Validate 报告该规则是否指明了出口：ParentID 非空，或 FallbackHuman 为 true。
//
// 注意 ParentBlocked 不参与校验：父被阻塞只影响**选哪条**出口（走 Fallback），
// 不影响"是否存在出口"。
// 并发：纯函数。
func (r EscalationRule) Validate() error {
	if r.FallbackHuman {
		return nil
	}
	if r.ParentID == "" {
		return fmt.Errorf("escalation rule: no outlet (empty ParentID and FallbackHuman=false -- a hanging escalation is the worst failure mode)")
	}
	return nil
}
