package wire

import (
	"context"

	"marl/internal/types"
)

// Compiler 把 ContextView 编译成线路片段序列（Part 3.4 的"编译器"）。
//
// 内部语义角色（10+）→ 线路角色（4）的映射在这里发生；同时实现三层提示词注入
// （L1 系统前缀 / L2 常驻块 / L3 私有段）与上下文裁剪。
// 编译结果可缓存：View 没变就复用（Part 3.4）。
//
// 不变量：编译器不得跨 types.Stability 边界重排，也不得把 volatile 挪到 stable 之前。
type Compiler interface {
	// Compile 生成按 View 顺序排列的 Canonical 片段 + 估算 token 数。
	//
	// 契约（按重要性排序）：
	//   - **保序**：输出顺序 = View 中 items 的相对顺序（Position 升序），
	//     并按 StabilityRank 单调不减分组（frozen → stable → volatile）。
	//     跨 Stability 边界重排会破缓存，而 volatile 段被提到前面还会让
	//     "Transient 应该最廉价"的前提失效；
	//   - **frozen 逐字节稳定**：同一 View 的 frozen 前缀必须 byte-stable，
	//     且不受裁剪/压缩策略影响。这是全项目共享缓存前缀的地基：
	//     裁剪一旦动到 frozen 段，所有 Agent 的缓存同时失效；
	//   - **裁剪只作用于 stable/volatile**：Token 紧张时优先裁 volatile，
	//     其次按 ViewItem.Pinned 豁免规则裁 stable。frozen 段永不裁——
	//     system/常驻块/工具表都在里面，裁掉它们等于换了另一个 Agent；
	//   - **不改入参**：view 只读（同一 View 会被反复编译，也是并发读者持有的
	//     旧视图，见 types.ContextView 并发约定）；
	//   - **可缓存/可复现**：相同的 (view.Items, AgentID) 必须产出相同的字节，
	//     否则 Part 3.4 的编译缓存会持续失效（表现为每次调用都重算，
	//     且缓存键永远不命中）。
	//
	// 返回的 token 估算与 EstimatedTokens 必须同口径（同一实现、同一输入下
	// 两者数值一致）——headroom 判定用前者、预算记账用后者，口径不一致会让
	// "还有多少余量"出现两个互相矛盾的答案。
	//
	// 失败：view 非法（AgentID 零值、Item 引用悬空、Stability/WireRole 非法
	// 等，见 ContextView.Validate）→ 错误。**不得**跳过非法项继续编译：
	// 静默丢弃一条消息比编译失败危险得多（模型看不到某段上下文却无人知晓）。
	Compile(ctx context.Context, view *types.ContextView) ([]Segment, int, error)

	// EstimatedTokens 只做估算，不生成完整编译产物（headroom 检查用）。
	//
	// 契约：必须是**本地估算**——不调 LLM、不发网络请求（它位于每一轮的热
	// 路径上，任何网络往返都会让主循环的延迟被估算动作主导）；
	// 且必须与 Compile 的返回数值同口径（见上）。
	//
	// 精度定位：估算必然有误差（分词器与厂商真实计数不完全一致），因此
	// 它只用于**提前量**判断，不能作为硬上限依据。调用方必须预留余量，
	// 而不是把估算值顶到 max_context 再指望不出错。
	//
	// 并发：必须可并发调用。
	EstimatedTokens(ctx context.Context, view *types.ContextView) (int, error)
}
