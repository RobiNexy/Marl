// Package proto 定义 Agent 之间的消息与意图（Part 8.7 / Part 9 / Part 11）。
//
// 原则 1 的落点：Agent 只能表达意图，框架统一裁决后执行。所有结构性变更——
// fork 子 Agent、切换模型、开 Fossil 分支、请求人类介入——都以本包里的
// *Request 意图提交，由各自裁决关口处理。
//
// 原则 4 的落点：Envelope.From 由框架按代码路径填写，Agent 无法伪造。
//
// # 全包统一的三条契约约定
//
//  1. **裁决结果是正常业务值，不是 error**。拒绝走 *Decision（Status/Code/Reason），
//     error 只保留给框架级故障。理由：请求者需要读到原因并自我修正，
//     而"框架异常"与"你的请求不合规"对 LLM 的后续行为指引完全相反。
//  2. **未识别值一律 fail-closed**。SpawnStatus 零值不是 approved；
//     VerdictKeyword 零值与未识别关键词都不得被解释为 approve；
//     EscalationRule 无出口时必须报错而不是静默丢弃求助
//     （否则发起者永久阻塞且系统各处都不报错）。见 ADR-0014。
//  3. **拒绝必须带可读原因 + 机器错误码**（SpawnDecision.Code/Reason）：
//     前者给 LLM 读、后者给框架与统计读，不可互相替代。
//
// # 意图与技能的边界
//
// 本包的意图工具（spawn_subagent / request_human / request_reconfigure /
// request_branch / request_discussion / report_to_parent）与技能**同在工具表中
// 呈现**（见 ADR-0015），但处理路径完全不同：技能只读写外部世界，
// 意图驱动框架行为并经裁决关口。判别用 IsIntentTool，不做前缀猜测。
package proto
