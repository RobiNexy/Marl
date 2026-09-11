package proto

import (
	"time"

	"marl/internal/types"
)

// ReconfigureRequest 是运行时参数调整意图（Part 6.9）。
//
// 可热切换（Reconfigure 可改）：Sampling（含 timeout，不含 model_id，因为模型由
// 阶梯决定）、Task 预算、Thinking.Level / Budget。
// 不可改（要建新 Agent）：Prompt、AllowedSkills、Requirement、CanSpawn、OutgoingContext。
//
// Agent 调用 request_reconfigure 时语义是"我卡住了 + 原因"，升不升级由证据说话
// （Part 7.2）；只有人类调试时才允许 marl reconfigure --level r2 点名。
//
// 不变量：
//   - Reason 非空（同上：这是"我卡住了"的意图，不是一次静默调参）；
//   - 三个指针**至少一个非 nil**。全 nil 的请求在语义上是空的，但它会走完
//     整个裁决流程并触发一次审计写入——留下"发生过一次重配置"的痕迹却没有
//     任何变更，污染审计的可读性。因此视为非法。
//
// 覆盖语义（关键，容易踩）：非 nil 的指针表示**整块替换**，不是逐字段合并。
// 于是 CPU 上很自然的一行代码 `Sampling: &types.SamplingParams{Temperature: 0.2}`
// 会把 timeout_ms/max_tokens/top_p 全部清零（见 types.SamplingParams 零值契约）。
// 正确写法是先读当前值、改需要改的字段、再整体传回（读-改-写）。
// 之所以不做字段级合并：合并需要知道"零值到底是用户想设 0 还是没填"，
// 而那个信息在值语义里已经丢失；用指针表达"整块替换"是唯一无歧义的选项。
type ReconfigureRequest struct {
	AgentID  types.AgentID
	Sampling *types.SamplingParams // nil = 不改；非 nil = 整块替换
	Task     *types.TaskPolicy     // nil = 不改；非 nil = 整块替换
	Thinking *types.ThinkingSpec   // nil = 不改；非 nil = 整块替换
	Reason   string
}

// Validate 报告请求是否至少变更了一项且带有原因。
//
// 失败：Sampling/Task/Thinking 全为 nil；Reason 为空。
// 并发：纯函数。
func (r ReconfigureRequest) Validate() error {
	panic("TODO(phase 0): placeholder")
}

// BranchRequest 是开 Fossil 分支的意图（Part 8.4 / 11.2）。
// 分支退回它本来的用途：人类或 Agent 显式申请的高风险实验、讨论分支。
//
// 不变量：RequesterID / Name / Reason 非空。Name 是 Fossil 分支名，
// 为空会落到默认分支上——那等于直接改主线，与本意图的存在意义相反。
type BranchRequest struct {
	RequesterID types.AgentID
	Name        string // 如 "discuss/oauth-interface"
	Reason      string
}

// BranchDecision 是分支创建裁决。
//
// 不变量：Approved 为 false 时 Reason 非空（否则请求者只知道"被拒"),
// 无法修正请求；Approved 为 true 时 BranchName 非空且必须等于请求的 Name
// （裁决可以拒绝，但不得**改名批准**——改名会让请求者后续按原名找不到分支）。
type BranchDecision struct {
	Approved   bool
	BranchName string
	Reason     string
}

// DiscussionRequest 是发起人机讨论的意图（Part 11.2 入口 1）。
// 框架处理：开 Fossil 分支 discuss/<topic-slug>、建控制面讨论目录、
// Agent 进入 Blocked(Discussing)、生成 verdict.md 模板（含当轮 nonce）。
//
// 不变量：RequesterID / Topic / Reason 非空。Topic 会参与 slug 生成，
// 为空则降级成无意义的目录名，人类在 discussions/ 下无法分辨哪个是哪个。
type DiscussionRequest struct {
	RequesterID types.AgentID
	Topic       string // 如 "OAuth 接口契约"
	Reason      string
}

// DiscussionTicket 是讨论实例的控制面信息（Part 11.2 流程）。
// 两个文件的分工：draft.md 是 Agent 写、verdict.md 是人类写（不在任何 Agent
// 的 writable_paths 里，且 frontmatter 带当轮随机 nonce，原则 4 三层防护）。
//
// 不变量：ID / BranchName / DraftPath / VerdictPath / Nonce 均非空。
// Nonce 空是**安全**问题而非格式问题：nonce 是"当轮裁决凭据"，
// 空 nonce 会让任何一封历史 verdict.md 都通过校验（凭据校验退化成无校验），
// 也就是把原则 4 的第三层防护整个拆掉。因此空 nonce 必须被拒绝，
// 而不是"跳过校验继续"。
type DiscussionTicket struct {
	ID          types.DiscussionID
	BranchName  string // discuss/<topic-slug>
	Topic       string
	DraftPath   string // .../discussions/discuss_<ulid>/draft.md
	VerdictPath string // .../discussions/discuss_<ulid>/verdict.md
	Nonce       string // 当轮随机 nonce，verdict.md frontmatter 里的裁决凭据
	CreatedAt   time.Time
}

// VerdictKeyword 是讨论裁决的关键词（Part 11.2）。
//
// 零值契约（fail-closed，本包最重要的一条）：零值 VerdictKeyword("") 与任何
// 未识别关键词都**不得**被解释为 approve。人类写错一个字母、或多打一个空格，
// 绝不能等价于"批准"。未识别一律按 note 处理（记录内容、不改变阻塞状态），
// 让人看到自己的裁决没生效并重写——多一轮交互的代价，远小于静默批准
// 一个被拒绝的方案所造成的事故。
type VerdictKeyword string

const (
	VerdictApprove VerdictKeyword = "approve"
	VerdictReject  VerdictKeyword = "reject"
	VerdictAbandon VerdictKeyword = "abandon"
	VerdictNote    VerdictKeyword = "note" // 批注（未识别关键词的兜底归类）
)

// Valid 报告 k 是否为已定义关键词之一。零值返回 false。
//
// 注意 Valid()==false 与 VerdictNote 的区别：前者是"人类没给有效裁决"，
// 后者是"人类明确只写了批注"。消费侧必须区分——前者要提示重写，
// 后者是正常的中间态。
//
// 并发：纯函数。
func (k VerdictKeyword) Valid() bool {
	switch k {
	case VerdictApprove, VerdictReject, VerdictAbandon, VerdictNote:
		return true
	}
	return false
}

// DiscussionVerdict 是裁决关键词解析结果（@approve / @reject / @abandon / 批注）。
//
// 不变量：Nonce 非空且必须等于当轮 DiscussionTicket.Nonce。不等或为空时，
// 整个 verdict 必须被丢弃（不解析 Keyword、不解除阻塞）——这正是防"人类
// 身份伪造 / 陈旧裁决复放"的机制：未通过 nonce 校验的裁决在语义上不是裁决。
type DiscussionVerdict struct {
	Keyword VerdictKeyword
	Nonce   string // 校验是否当轮 nonce
	Body    string // 人类写的内容（批注或裁决说明）
}
