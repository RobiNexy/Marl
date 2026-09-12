// Package gate 实现统一审批抽象（PDP/PEP，Part 11.3，阶段 11）。
//
// 定位：每个执行点（PEP：llm_call / 编排 / discussion / escalation / shell）
// 在动作发生前问一次 Gate（PDP），Gate 按规则表**顺序匹配、首中生效**，
// 返回三值决策：
//
//	allow      → 立即执行
//	deny       → 拒绝，原因回传给 LLM（带 rule_id）
//	need_human → Approver 介入（审批文件 + nonce），裁决附带 grant
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
