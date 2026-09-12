// Package compress 实现压缩与编排调用的执行层（设计文档 13.5，阶段 3）。
//
// 契约在 internal/orchestrate（Orchestrator / Compressor / CompressionPolicy），
// 本包是它们的实现——依赖方向恒定指向抽象：compress → orchestrate，
// 绝不反向。
//
// # 数据流建模
//
// 压缩是一条单向管道，输入是 (Log, View, Policy)，输出是新的 View：
//
//	View ──► 估算 oldTokens
//	     ──► L0 机械清理（View 级 exclude：去重 file_read / 剔除被覆盖的
//	          旧读取 / exclude 历史 thinking；零 LLM 成本）
//	     ──► 划定压缩区间（头部保留 + 尾部保留 N 轮，中段为压缩区）
//	     ──► [压缩区非空] 渲染区间文本 → Orchestrator 生成 SUM（独立预算、
//	          独立账本，主 Agent 状态不可达）→ ValidateSUM 机械校验
//	          （失败按 MaxRetries 重试，温度稍高）
//	     ──► 估算收益 reclaim；不足 → 报错（阶段 3 不做 L2 升级，见 ADR-0026）
//	     ──► 追加 SUM 进 Log（ProvSummaryOf，血缘指向压缩区全部条目）
//	     ──► 构造新 View = [保留头部] + [SUM] + [保留尾部]
//
// 两条全包不变量：
//
//  1. **Log 只追加**：压缩对 Log 的唯一动作是 Append SUM 条目；
//     L0 的全部清理只动 View（Visible=false），真相之源分毫不动（ADR-0002）。
//  2. **只读快照**：发给 Orchestrator 的区间文本是一次性渲染的快照，
//     生成期间主 Agent 的任何状态变化都不影响它（Part 3.7 关键约束）。
//
// # 阶段 3 边界（13.5 "不做"清单）
//
//   - 压缩收益不足时升级到 L2：硬编码为报错（调用方据此走降级路径）；
//   - split 的 XML 标注块禁切：扫描器只识别代码块与引用块；
//   - Ledger 的 SQLite 落地：本包定义 UsageSink 窄接口，阶段 4 的
//     internal/ledger 接管（ADR-0026）。
package compress
