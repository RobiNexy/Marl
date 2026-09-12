package wire

import (
	"context"
	"time"

	"marl/internal/types"
)

// WireID 已在 types 包定义（避免与本包循环依赖，见 types/ids.go）。

// WireAdapter 是线路适配器接口（Part 10.2，唯一碰 HTTP 与 JSON 的地方）。
// 实现数量极少（3–4 个协议），变化频率极低。
//
// 职责边界（三条都属于"不该做的事"，因为它们是 Pool / Denormalizer 的职责）：
//   - 不做并发控制、限流、熔断、排队；
//   - 不做重试与退避（重试策略属 Pool，因为只有它有全局视野：别的 Agent 是否
//     正在用同一个 endpoint、熔断是否已开）；
//   - 不做错误分类（厂商错误 → ErrorClass 的翻译属 Denormalizer：它才有
//     "同一语义在不同厂商的不同编码"这张表）。
type WireAdapter interface {
	// ID 返回线路协议类型，如 types.WireOpenAIChat。必须是非零且 Valid 的
	// types.WireID，并与本协议 Normalizer / Denormalizer 的 Wires() 声明一致——
	// 三方声明不一致会让请求由 A 协议编码、由 B 协议解析，而两边的字段名
	// 常常部分重合，于是"能跑但字段丢失"。
	ID() types.WireID

	// Execute 发起一次 LLM 调用并返回归一化前的产出。
	//
	// 入参是**已归一化**的 WireRequest（阶段 1 修正，见 ADR-0016）：分层上
	// Normalizer 已完成角色布局与参数剔除，适配器只做"协议编码 + 发请求"。
	// 若这里收 CanonicalRequest，适配器就必须自己再做一遍去程翻译（破
	// Normalizer 的单一职责与字节稳定），或者绕开 Normalizer 的降级记录——
	// 两条路都会让"请求是谁排布的"没有唯一答案。
	//
	// 注意：本接口不负责并发控制——并发闸门 / 限流 / 熔断 / 排队由 Pool 承担
	// （Part 10.12）。除非是 Pool 内部的临时直通，否则调用方不要绕过 Pool。
	//
	// 后置条件（成功与失败都适用）：
	//   - 返回值非 nil（即便出错也要带回 StatusCode / Body 供归因）；
	//   - Latency 已填充（它是 Watchdog 与成本分析的输入，缺了无法补算）；
	//   - Body 保留**原始响应体**，不预解析。厂商错误详情就在 body 里，
	//     适配器提前丢掉它等于让 Denormalizer 只能猜错误类别。
	//
	// 失败语义：网络/超时/非 2xx 都通过 resp + err 成对表达（resp 里带
	// 原始证据），**不做分类、不重试**。ctx 取消必须原样返回 ctx.Err()，
	// 不得包装成厂商错误——否则"上游取消了调用"会被误当成"厂商故障"，
	// 进而污染熔断计数与升级证据。
	Execute(ctx context.Context, req *WireRequest, binding types.Binding) (*WireResponse, error)

	// ModelName 把 types 层面的模型 id 翻译成远端模型名（如 "deepseek-flash"）。
	//
	// 失败：未知 modelID → 错误。**不得回落成默认名或原样透传**：调用到一个
	// 语义不同但名字相近的模型，账单和输出都会变，而请求本身没有任何异常迹象。
	// 本方法必须是纯函数（不发网络请求、不读文件）——它会在池内热路径上被反复调用。
	ModelName(modelID string) (string, error)

	// HealthCheck 是探测期 / 熔断恢复期的轻量 ping。
	//
	// 只报告"这一次探测的结果"，**不修改熔断状态**：状态迁移统一由 Pool 按
	// CircuitPolicy 决定。让适配器自己能改熔断状态会出现两套状态机，
	// 而它们的判定条件迟早会不一致。
	HealthCheck(ctx context.Context, endpoint string) error
}

// WireResponse 是 Wire 层的原始产出：尚未归一化的厂商响应形状。
// Denormalizer 负责把它翻译成 WireTurn（多 Outcome 序列）。
// 出错时 WireResponse 可能为空，返回的 error 按厂商错误分类归一化。
//
// 零值契约：三个零值各有含义，且都不能被当成"正常值为零"：
//   - StatusCode == 0：**没有收到 HTTP 响应**（连接失败 / 超时 / 取消），
//     必须与"收到了 4xx/5xx"区分——前者该重试，后者重试无用；
//   - Body == nil：无响应体。非 nil 但空体是另一回事（协议违规）；
//   - UsageRaw == nil：**用量未知**（调用失败，或厂商压根没回传该字段）。
//     这与"用量为 0"完全不同：把未知当 0 记账会静默少报成本，而账单是
//     决策依据（阶梯升级、预算熔断都读它）。消费侧必须把未知显式记为未知——
//     Ledger 侧不允许用零值 TokenUsage 冒充"这次没花钱"。
type WireResponse struct {
	Wire       types.WireID
	StatusCode int
	Body       []byte // 原始响应体（JSON）；nil 表示无响应体
	Latency    time.Duration
	UsageRaw   *types.TokenUsage // 命中的原始 usage 字段；nil 表示用量未知（≠0）
}
