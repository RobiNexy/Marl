// Package gate 实现统一审批抽象（PDP/PEP，Part 11.3；Part 14.7 重新定性
// ——Gate 是"人类 Actor 的收件秘书"）。
//
// 定位：每个执行点（PEP：llm_call / 编排 / discussion / escalation / shell）
// 在动作发生前问一次 Gate（PDP），Gate 按规则表**顺序匹配、首中生效**，
// 返回三值决策：
//
//	allow      → 立即执行
//	deny       → 拒绝，原因回传给 LLM（带 rule_id）
//	need_human → 调用方折算成 MsgGateRequest，经人类 Actor 的文件后端
//	             投递（inbox/gate_<ulid>.md，frontmatter 含 nonce）；人类的
//	             裁决经 MsgGateReply 回到 Agent，由 ResolveGate 兑现 grant
//
// 阶段 12（Part 14）的结构面：审批的**投递**不再是本包的阻塞轮询（旧
// FileApprover 已删——特例消除），文件编码/解析/静默窗统一在 actor 包
// 的 FileBackend；本包保留的是规则表、grant 记账（session 级）与
// GrantStore（grants/ 落盘，"人类批过的 always 不丢"）。审批者必须是
// 规则，不是 Actor（Part 14.7 硬边界）。
//
// 被收编的决策面（原来四套机制 → 现在五种 Kind 的同一骨架）：
//
//	llm_call（超额）、orchestration（缓存破坏分级）、discussion、
//	escalation、shell。discussion / escalation 的既有文件信箱实现
//
// （讨论 verdict / escalation 待办）保留——收编的观感在 API 层，
// 文件形态是各自 Kind 特有的呈现（阶段 11 设计 §3.4）。
//
// 三个关键不变量（Part 11.3）：
//  1. 审批的输出是 grant（额度 / 永久放行规则），不是一次性 yes——
//     问一次给一批，根治审批疲劳；
//  2. 永久放行（grant=always）落盘到控制面，重启不丢；
//  3. "不可议"的操作不走 Gate：触及 frozen 段的编排、命名空间越界——
//     硬拒在各自执行点做（Part 11.3 §3.5），Gate 只管可议的灰色地带。
package gate
