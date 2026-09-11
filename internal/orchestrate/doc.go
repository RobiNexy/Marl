// Package orchestrate 定义上下文编排的原子操作集（Part 3.5）。
//
// 所有操作统一为：(Log, View) → 新 View（可能追加 Log）。
// 核心不变量：编排操作永远不销毁 Log，只改写 View——
// 这是"可复原 / 可回溯"的根本解药。
//
// 编排操作也进审计：每个 Op 执行记 audit_events(action="orchestrate",
// target=Op名, payload=args)。
//
// # 全包统一的契约约定
//
//  1. **返回新 View，不就地修改**：旧 View 因此对并发读者继续有效。
//     生命周期是"单写者构造新 View，读者只读旧 View"（见 types.ContextView），
//     所以任何 Op 都不得修改传入的 view，也不得复用其 Items 底层数组。
//  2. **只动 View 的操作绝不写 Log**：去重、剪枝、软删除（Visible=false）
//     都只改投影。反过来，需要新增内容（摘要、切块）时一律 **Append 新条目**，
//     绝不 Update 历史——Log 是真相之源（ADR-0002）。
//  3. **错误语义**：入参未过契约校验（零值枚举、引用悬空、policy 非法）→
//     store.ErrInvalid，这是调用方 bug，不重试不降级；目标条目不存在 →
//     store.ErrNotFound，并且按真相之源模型这属于"数据被外部改动"，
//     除降级外还必须记审计。
//  4. **压缩不是工具**：Compressor 由框架在 headroom 不足时自动触发，
//     不出现在 LLM 的工具表里；执行期间主 Agent 暂停
//     （StateBlocked + BlockCompressing），状态迁移由主循环负责，
//     Compressor 本身不改 Agent 状态（见 compress.go）。
package orchestrate
