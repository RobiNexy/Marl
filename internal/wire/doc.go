// Package wire 定义 Adapter 五层调用链的契约（Part 10.2）。
//
//	Requirement → Router → Binding → CanonicalRequest → Normalizer → WireRequest
//		→ Pool → Wire → WireResponse → Denormalizer → WireTurn → Ledger
//
// 这五层是 Marl 的成本与缓存命脉：CanonicalRequest 必须表达语义与稳定性
// （不表达角色布局，后者是 Normalizer 的产出），Normalizer 按协议生成
// byte-stable 的前缀，Denormalizer 把厂商信号归一化成一张表（供阶梯升级
// 与压缩触发消费）。
//
// # 谁负责什么（跨层职责边界，越界会同时破坏可重试性与可归因性）
//
//	WireAdapter  只发请求：不重试、不分类、不改熔断状态，保留原始 body
//	Pool          背压与可靠性：排队（阻塞而非 429）、闸门、限流、熔断、重试
//	Normalizer    去程翻译：角色布局、按缓存模式打断点、剔除不支持的参数
//	Denormalizer  回程翻译：拆 reasoning/tool_call/reply、归一化 usage 与错误分类
//	Router        选绑定：硬约束过滤 + 软约束打分，亲和性优先（缓存即成本）
//
// # 「未知」不等于零（本包最容易出错的一条跨层约定）
//
// usage 未回传、健康状态尚未探测、错误分类不出来的场景，一律必须显式记为
// **未知**，不得用零值冒充"正常值为零"：
//
//   - WireResponse.UsageRaw / Outcome.Usage 为 nil = 用量未知（≠ 0 token）；
//   - HealthStatus 零值 = 尚未探测（既不是健康也不是故障）；
//   - ErrorClass.Valid()==false 且 IsError()==false = 未分类（不能当 transient 重试）；
//   - Catalog.Pricing 查不到时必须报错，不得回落零价（零价会被读成"免费"）。
//
// 零值策略的完整分类见 ADR-0014。
//
// # 三方一致性与冻结前缀
//
// WireAdapter.ID / Normalizer.Wires() / Denormalizer.Wires() 必须互相一致：
// 由 A 协议编码、由 B 协议解析的请求常常"能跑但字段丢失"。同时 frozen 前缀
// （system + 常驻块 + Tools）逐字节稳定是全项目共享缓存的地基，工具表因此
// 全项目唯一且顺序冻结（见 ADR-0015）。
package wire
