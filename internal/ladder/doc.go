// Package ladder 实现阶梯与成本经济（Part 7 / 13.6 阶段 4）：
// 阶梯配置加载、两阶段调度 Router（谓词过滤 + 打分）、证据累积器与升级判据。
//
// 核心纪律（Part 7）：
//   - 升级决策基于**可观测证据**，不依赖 LLM 自述（ADR-0013）；
//   - 升级是换阶梯级别（rung_index+1，单调不回退），不是 reconfigure 换
//     model_id（后者破缓存前缀，实测确认，见 ADR-0021/探测报告 §3.10）；
//   - thinking 档位的唯一真相在阶梯（ADR-0022）——本包的 Config.Thinking
//     是它的载体，Router 组装 Binding 时写入。
//
// 阶段 4 边界（13.6 "不做"清单）：Pool 并发闸门（直连）、熔断与健康检测
// （Router 谓词的健康项留空）、能力探测（probe）。Catalog 的健康查询接口
// 保留（wire.Catalog 契约），实现返回"未探测"状态。
package ladder
