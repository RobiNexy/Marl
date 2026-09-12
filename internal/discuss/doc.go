// Package discuss 实现讨论分支的最小闭环（Part 11.2，13.10 阶段 8）。
//
// 讨论的物料与所有权：
//
//	blink…分支   fossil branch discuss/<id>         draft 的提交历史
//	控制面目录   <ControlDir>/<discussion-id>/       verdict.md（人类写）
//	仓库路径     .marl/discussions/<id>/draft.md     Agent 写（author=agent）
//	结论落地     结论 target 路径（author=human 的 commit）
//
// 三个安全不变量（原则 4 / Part 11.2 的"关键安全设计"）：
//
//  1. verdict.md 不在任何 Agent 的命名空间内（控制面路径在 ~/.local/state，
//     Agent 的 file_read 反正够不着）；
//  2. 裁决只接受携带**当轮 nonce** 的 verdict——Agent 读不到 verdict
//     （不可读 + 沙箱），无法伪造通过；
//  3. 合并（Finalize）的 commit author 由框架写死 UserHuman——能落地的
//     通道只有人类（vim 编辑 verdict + @approve 的是人类），"author=human"
//     是通道属性而非内容属性。
//
// 失败模式全集：
//
//	Open        → 分支创建/切换失败、控制面目录创建失败、verdict 模板写失败
//	UpdateDraft → 草稿写失败 / fossil commit 失败（ErrNothingToCommit 的
//	              连续两份相同草稿是**正常状态**，返回 nil 而不是错误）
//	Wait        → ctx 取消；verdict 读取失败（重试直到 ctx 取消——verdict
//	              是人类手写，格式错误的中间态是预期场景）
//	Finalize    → 落地路径创建失败 / commit 失败
package discuss
