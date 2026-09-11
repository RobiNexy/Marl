package proto

import (
	"context"

	"marl/internal/types"
)

// SpawnRequest 是 fork 子 Agent 的意图（Part 9.2）。
// Agent 只能提交它；真正的创建由 Spawner（框架级单例）统一裁决后执行。
//
// 不变量：RequesterID / ProfileID / TaskDescription 非空。
//   - RequesterID 是提权检查的基准（WritablePaths 必须是请求者可写范围的子集），
//     为空则无从判定子集关系，必然退化成"放行"；
//   - TaskDescription 是子的 prompt 素材，为空等于让子 Agent 无题可做。
//
// WritablePaths 的语义：glob 列表，必须**是请求者可写范围的子集**，
// 空列表是合法的（子只读）。"空 = 继承父的全部可写"是刻意排除的——
// 那会让一次漏填权限变成静默提权。
//
// InjectMessages 是 Seq 列表（Part 9.5），指向请求者 Log 里的序号；
// 越界的 Seq 必须被裁决拒绝而不是跳过（跳过会让子上下文缺掉被显式要求
// 注入的那部分，而它自己不知道）。
type SpawnRequest struct {
	RequesterID     types.AgentID
	ProfileID       types.ProfileID
	PromptOverride  string // 可选，覆盖 Profile 的 prompt（Part 6.7）
	TaskDescription string
	WritablePaths   []string // glob，必须是请求者可写范围的子集
	ReadablePaths   []string // 可选，默认继承请求者的 read 挂载
	InjectMessages  []int64  // Seq 列表，要注入子上下文的消息（Part 9.5）
	TraceID         types.TraceID
}

// SpawnStatus 是 Spawner 的裁决结果。注意没有 Queued 状态——排队是 Pool 的职责，
// 不是 Spawner 的（Part 9.2）。
//
// 零值契约：零值 SpawnStatus("") 非法，且**不是** approved。裁决结果只由
// 裁决关口产出，未设置说明流程没走完；把它当 approved 等于绕过全部裁决。
type SpawnStatus string

const (
	SpawnApproved SpawnStatus = "approved"
	SpawnRejected SpawnStatus = "rejected"
)

// Valid 报告 s 是否为已定义裁决结果之一。零值返回 false。
// 并发：纯函数。
func (s SpawnStatus) Valid() bool {
	switch s {
	case SpawnApproved, SpawnRejected:
		return true
	}
	return false
}

// SpawnDecision 返回给请求者。
//
// 不变量（与 Status 严格配套）：
//   - Status == SpawnApproved：ChildAgentID 非空（批准却不给 ID，请求者无法
//     等到子 report，会永久阻塞在 BlockWaitChildren），且 Code 必须为空；
//   - Status == SpawnRejected：Reason 与 Code 均非空。Reason 是**给 LLM 读的**
//     可读原因（它要据此改需求重试），Code 是给框架/统计读的机器判据，
//     两者不可互相替代。
//
// [补齐: 原先只有 Reason，没有承载 SpawnErrorCode 的字段——而错误码常量
// 本来就声明在同一个文件里，等于声明了却没有出口。现补 Code。]
type SpawnDecision struct {
	Status       SpawnStatus
	ChildAgentID types.AgentID
	Code         SpawnErrorCode // 仅 rejected 时非空
	Reason       string         // rejected 时说明原因（可直接回传给 LLM）
}

// SpawnErrorCode 是裁决拒绝的错误码（Part 9.2 / 9.4 的完整清单）。
//
// 为什么错误码用大写蛇形而 ReportStatus 用小写：这里的值会被 LLM 读到并
// 作为"我该改什么"的索引（Part 9.4 要求可直接回传），大写常量形式让它与
// 自然语言理由在视觉上可区分，减少模型把错误码当句子的一部分复述。
type SpawnErrorCode string

const (
	SpawnErrRequesterNotFound SpawnErrorCode = "REQUESTER_NOT_FOUND"
	SpawnErrNotPermitted      SpawnErrorCode = "PROFILE_NOT_PERMITTED"
	SpawnErrMaxDepth          SpawnErrorCode = "MAX_DEPTH_REACHED"
	SpawnErrGlobalAgentLimit  SpawnErrorCode = "GLOBAL_AGENT_LIMIT"
	SpawnErrProfileNotFound   SpawnErrorCode = "PROFILE_NOT_FOUND"
	SpawnErrNamespaceExceeded SpawnErrorCode = "NAMESPACE_EXCEEDED"
	SpawnErrForkRounds        SpawnErrorCode = "FORK_ROUNDS_EXCEEDED"
)

// Valid 报告 c 是否为已定义错误码之一。
//
// 零值契约：零值 SpawnErrorCode("") 在 approved 的裁决里是**合法**的
// （没有错误可报），因此 Valid 返回 false 而消费侧不能把"零值"一律当错误——
// 判据是"rejected 时 Code 必须 Valid"。这条差异必须写清楚，否则实现容易
// 反过来把 approved 的零值判成非法。
//
// 前向兼容：新增错误码是低风险变更（调用方按字符串比较，未知码只需走默认
// 处理），因此未知码的消费行为定义为"按 rejected 处理、Reason 透传"，
// 而不是崩溃或降级成 approved。
//
// 并发：纯函数。
func (c SpawnErrorCode) Valid() bool {
	switch c {
	case SpawnErrRequesterNotFound, SpawnErrNotPermitted, SpawnErrMaxDepth,
		SpawnErrGlobalAgentLimit, SpawnErrProfileNotFound, SpawnErrNamespaceExceeded,
		SpawnErrForkRounds:
		return true
	}
	return false
}

// Spawner 是意图裁决关口之一：校验 → 批准/拒绝 → 创建 Agent、注入上下文、启动。
//
// 失败语义：**裁决结果不是 error**。拒绝是正常业务结果，走 SpawnDecision
// （Status=rejected + Code + Reason），因为请求者需要读到原因并自我修正；
// error 只用于框架级故障（存储不可用等），那时请求者会收到"框架异常"而不是
// "你的请求不合规"——这两者对 LLM 的后续行为指引完全相反。
type Spawner interface {
	Adjudicate(ctx context.Context, req *SpawnRequest) (*SpawnDecision, error)
}

// BatchPreflight 是 spawn_batch 的整体资源预检（Part 9.1：避免"批准 3 个
// 拒第 4 个导致任务残缺"。实现阶段提供）。
//
// 不变量：len(Reasons) == len(Requests)（一一对应，即使某项通过也要占位，
// 否则索引会错位）；Approved + Rejected == len(Requests)。
// 全批要么一起开、要么一起不开——部分批准会让批次任务残缺，这正是本结构
// 存在的理由。
type BatchPreflight struct {
	Requests []SpawnRequest
	Approved int
	Rejected int
	Reasons  []string // 与 Requests 一一对应
}
