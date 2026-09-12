// Package escalate 实现 Escalation（向上求助，Part 11.3，13.12 阶段 10）。
//
// 两个交付面：
//
//  1. 路由判断（Route）：有父且父未 Blocked → 发给父（Mailbox 信封）；
//     否则 → 人类文件信箱。
//  2. 人类文件信箱（Mailbox）：ControlRoot/requests/{pending,done,archived}/
//     的生命周期（Part 11.5）；verdict 同款"静默窗口轮询"（复用 discuss
//     的判定形态——不发通知，文件本身就是通知）。
//
// 会话语义（Agent 侧的消费在 internal/agent 的 escalate.go）：
//
//	request_human（工具）→ Route → 父信箱信封 / 人类信箱文件
//	→ Blocked(Escalating)
//	→ 回复到达（信封 MsgEscalationReply / 文件 done/）→ Log/View
//	→ 恢复 Running。
//
// [推理边界] 父路径的"父如何回复"在 Part 11.3 由父的下一轮编排承担——
// 阶段 10 的最小形态是框架自动 ACK（pump 登记 escalation、并发 ACK），
// 让发起者不被永久阻塞而链路可继续；"父读取并回答"的完整形态由父的
// 上下文自然推进（Log 机制已就绪）。该边界在本包头注释里与设计文档
// Part 11.3 的对照是阶段 10 的显式记录。
package escalate
