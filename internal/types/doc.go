// Package types 定义 Marl 框架的核心契约类型。
//
// 本包是阶段 0（地基与合约）的交付物：字段与语义由 docs/design/marl_design.md 定死，
// 后续所有阶段的实现都以这里的类型为共同"语言"，因此任何字段改动都需要走 ADR 流程
// （见 docs/design/decisions.md）。
//
// 硬约束：本包不允许 import 仓库内其他任何包，保持为最底层契约。所有被多个包共享的
// 基础类型（WireID / Stability / Attachment / ToolCall 等）都收在这个包里，
// 以避免 wire ↔ proto 之间的循环依赖。
//
// # 关于"接口定义在消费侧"的偏离说明
//
// 惯用法要求接口定义在消费侧并收窄至单一意图。本包有若干接口（Resolver /
// ProfileLoader / PromptIndex / MessageLog 之外）不满足字面形式，理由：
//
//   - 这些接口有**多个**消费方（Resolver 被全部路径类技能消费、ProfileLoader 被
//     启动器与 spawn 流程消费），若每个消费方各定义一份，会产生 N 份互不兼容的
//     同义契约，反而破坏"依赖方向恒定指向抽象"；
//   - 本包无内部依赖，充当 Go 标准库中 io / context 那样的"公共词汇表"角色：
//     接口集中定义在无依赖的底层包，消费方导入它。
//
// [权衡: 真正的消费侧收窄适用于"单一消费者 + 未来可能替换实现"的场景（如
// store.Ledger 的两个便捷方法、wire.WireAdapter）——那些接口确实按最小方法集
// 切分了。本包承担的是跨包共享契约，属另一种情形，不套用同一形式。]
//
// # 枚举的零值策略（本包统一规则）
//
// Go 的 string/int 枚举零值可能落在"未定义"区间，而零值又最容易被"忘记设置"
// 产生。本包按**错误后果**分流处理，而非一刀切：
//
//  1. 安全/正确性关键且零值会导致 fail-open 或静默丢数据 → 提供 Valid() 判定，
//     并在消费契约里写死"未识别值按最保守方向降级"（PathMode → hidden、
//     Audience → both、Stability → stable）。详见各类型注释。
//  2. 零值非法且错误会立刻暴露（编译报错、消息不出现）→ 同样提供 Valid()，
//     使边界能在构造点快速失败；但消费侧规则是"拒绝"，不赋予降级语义
//     （InternalRole / Provenance / WireRole）。
//  3. 零值本身就是合法取值且与"未设置"不可区分 → 用指针表达"未设置"
//     （ThinkingSpec.Budget *int、LogEntry.TokenActual、Outcome.Usage）。
//
// 完整规则见 ADR-0014（零值策略）。新增枚举时必须先决定它属于哪一类。
package types
