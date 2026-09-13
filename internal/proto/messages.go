package proto

import "marl/internal/types"

// Part 14（阶段 12）新增消息的载荷类型（Part 14.5 修订后的消息面）。
//
// 载荷与 MsgType 的对应关系由接收侧的类型断言承载（Envelope 契约：断言
// 失败必须显式报错，禁止"断言失败就当 nil"）。这些结构是**数据**：它们
// 不携带行为，也不携带权限——授权在 Gate 规则与 CapSet，不在信封里。

// DirectMessage 是 MsgDirect 的载荷（marl say、Agent 间显式通信）。
//
// 文本按"异步注入"消费：接收侧（Agent 的 pump）把它落成 RoleHumanNote 的
// Log 条目，下一轮编排自然看到——不唤醒、不等待（Part 14.8 的违约表）。
type DirectMessage struct {
	Text string // 正文（人类的原话 / Agent 的显式消息）
}

// GateRequest 是 MsgGateRequest 的载荷（PEP 判 need_human 后发给人类
// Actor 的审批请求，Part 14.7）。
//
// RequestID 是审批事实的对账键（文件名 gate_<ulid> 的 ulid 部分；回执
// 必须原样带回——Agent 用它把裁决配回到挂起的 Gate 请求上）。
// Nonce 是当轮凭据：进入文件 frontmatter，人类的裁决文件必须原样带回
// （伪不出、陈旧回放被拒——与讨论 verdict 同一防线，原则 4）。
type GateRequest struct {
	RequestID  string         // 审批事实 ID（gate_<ulid> 的 ulid 段）
	Nonce      string         // 当轮凭据（frontmatter；回执原样带回）
	Kind       string         // gate.Kind 的字符串形态（llm_call / orchestration / ...）
	AgentID    types.AgentID  // 发起审批的 Agent
	RuleID     string         // 命中的规则
	Reason     string         // 规则理由（给人类读）
	Attributes map[string]any // 机械属性（进审批文件正文——人也看数字）
}

// GateReply 是 MsgGateReply 的载荷（人类裁决 + grant）。
//
// 裁决语义的兑现面在 gate.Manager.ResolveGate：Action allow → grant 记账
// （GrantMode 的四形态）；deny → 拒绝原因透传。GrantMode 的取值与
// gate.GrantMode 一致（once / count / tokens / always；deny 时为空）。
type GateReply struct {
	RequestID string // 对应 GateRequest.RequestID
	Nonce     string // 当轮凭据（必须与请求一致）
	Action    string // "allow" | "deny"
	GrantMode string // once / count / tokens / always（deny 为空）
	Count     int    // count 模式的次数
	Tokens    int64  // tokens 模式的 token 额度
	Reason    string // 人类的批注（进审计 + Log）
}
