package types

import (
	"fmt"
	"time"
)

// WireRole 是线路协议的四种角色。LLM API 只认这 4 种；10+ 内部角色在编译阶段
// 按映射表折叠进来（Part 3.3 / 3.4）。
//
// 零值契约：零值 WireRole("") 非法。它以类型区分"面向模型的身份"，与
// InternalRole 一内一外，二者不可互换（编译层负责桥接，见 Part 3.4 映射表）。
type WireRole string

const (
	WireSystem    WireRole = "system"
	WireUser      WireRole = "user"
	WireAssistant WireRole = "assistant"
	WireTool      WireRole = "tool"
)

// Valid 报告 r 是否为四种线路角色之一。零值返回 false。
//
// 并发：纯函数。
func (r WireRole) Valid() bool {
	switch r {
	case WireSystem, WireUser, WireAssistant, WireTool:
		return true
	}
	return false
}

// Stability 三档决定一条消息在缓存键里的角色（Part 3.3 / 10.3）。
//
//	frozen   —— system + 常驻块 + Tools，byte-stable，全项目共享缓存前缀
//	stable   —— 历史回合，只追加；缓存断点的候选落点和压缩的作用域
//	volatile —— Transient，只在尾部；不入 Log；不参与前缀计算
//
// 零值契约：零值 Stability("") 非法，且**绝不允许被解释为 frozen**。
//
// 这是一条方向性约束，比"零值等于某个值"更强：frozen 是"提升信任级别"的
// 声明——它意味着这段字节可以被全项目复用为共享缓存前缀。因此任何默认值、
// 推断值、降级值都不得产生 frozen；frozen 只能由编译层按 SegmentKind 显式
// 授予。遇到未识别值（含零值）一律按 stable 处理：stable 是"普通历史消息"，
// 猜错的代价局限于本 Agent 的缓存断点位置，不会污染跨 Agent 的共享前缀。
//
// 不变量：Normalizer 不得跨 Stability 边界重排，也不得把 volatile 挪到
// stable 之前（10.3）——违反即破缓存，而 Transient 的全部价值就是廉价。
//
// 并发：Valid 是纯函数。
type Stability string

const (
	StabilityFrozen   Stability = "frozen"
	StabilityStable   Stability = "stable"
	StabilityVolatile Stability = "volatile"
)

// Valid 报告 s 是否为三档之一。零值返回 false。
//
// 注意：合法 ≠ 可信。Valid(StabilityFrozen) 为真不代表调用者有权声明 frozen，
// 见本类型的零值契约。
func (s Stability) Valid() bool {
	switch s {
	case StabilityFrozen, StabilityStable, StabilityVolatile:
		return true
	}
	return false
}

// ViewItem 是 Context View 中的一个引用条目（Part 3.3）。
// 它不复制 Log 内容，只携带引用 + 呈现指令。编排操作全部绕着 ViewItem 转。
//
// 零值陷阱（重要）：本结构**不是零值可用**的，且各字段的失败方向不对称：
//
//	Visible=false   —— 消息静默消失（危险：无报错、无审计）
//	Pinned=false    —— 不豁免裁剪（安全方向）
//	Position=0      —— 与"显式置 0"不可区分（见下）
//	Ref/WireRole/Stability 零值 —— 非法，会被 ContextView.Validate 拦截
//
// 由于 Go 无法表达"bool 必须为 true"这类约束，Visible 的零值风险只能靠
// 纪律与检查兜底：ViewItem 必须由编排层构造并显式置 Visible=true，
// 不允许直接写结构体字面量（除测试与零值契约测试外）。
//
// Position 的零值语义：[待验证] 是否把"零值"当作"未设置"取决于
// 编译层是否用 0 作为合法起点。当前契约是"由编排层显式赋值，零值不保证
// 排在首位"，实现阶段的编译排序需以此为准，否则新插入条目的顺序会不确定。
//
// [权衡: 曾考虑把 Visible 反转为 Hidden（零值=可见，天然安全），否决原因是
// 设计文档 §3.3 与软删除语义通篇以 visible 表述，改名会让文档与代码长期不一致；
// 认知成本高于本处收益。]
type ViewItem struct {
	Ref       MessageID // 指向 Log
	WireRole  WireRole  // 编译目标，可被编排覆盖
	Stability Stability
	Pinned    bool    // 裁剪时是否豁免
	Visible   bool    // 软删除：false 则不发给 LLM，引用仍在
	Position  float64 // Fractional Index，支持任意位置插入而不重排
}

// ContextView 是发给 LLM 之前的工作台（可变投影，Part 3.3）。
// 编排操作统一为：(Log, View) → 新 View（可能追加 Log），本结构是那个 View。
//
// 编译成线路消息是 wire 层的职责（见 wire.Compiler），本包不掺编译逻辑。
//
// 零值语义：ContextView{AgentID: id} 是合法的**空视图**（Items 为 nil 等价于
// 空切片，读取与遍历语义一致）。ContextView{}（AgentID 也为零值）非法，
// 因为视图必然属于某个 Agent。
//
// 并发：本结构与 ViewItem 都是可变投影，无内部锁。生命周期约定是
// "单写者构造新 View，读者只读旧 View"——编排操作返回新 View 而非就地修改，
// 正是为了让旧 View 对并发读者保持有效（见 orchestrate.Operation 契约）。
type ContextView struct {
	AgentID AgentID
	Items   []ViewItem
	// EstimatedTokens 是当前 View 编译后的估算大小。Compile 之后刷新。
	EstimatedTokens int
	// LastEstimatedAt 零值表示"从未估算过"，此时 EstimatedTokens 不可信，
	// 消费者必须重新估算而不能当作 0 使用。
	LastEstimatedAt time.Time
}

// Validate 报告该 View 是否可安全进入编译。
//
// 检查项：
//   - AgentID 非空；
//   - 每个 Item 的 Ref 非空、WireRole 与 Stability 合法；
//   - Stability 不逆序：volatile 不得出现在 frozen/stable 之前（§3.3 不变量）。
//
// 不检查：Visible（无法区分"本该可见"与"确实删了"）、Position（仅供排序）、
// EstimatedTokens（由编译器刷新，不是输入条件）。
//
// 失败：任一不变量被破坏，错误须指明第几个 Item 的哪个字段，便于定位编排点。
// 并发：纯函数，不改动接收者。
//
// [权衡: 校验放在 View 侧而非 Compile 侧，是为了让"编排层产出了非法视图"
// 与"编译器有 bug"两类失败可区分——前者错在编排，后者错在编译。]
func (v *ContextView) Validate() error {
	if v == nil {
		return fmt.Errorf("view is nil")
	}
	if v.AgentID == "" {
		return fmt.Errorf("view agent_id is empty")
	}
	prevRank := -1 // -1 = 尚无条目；合法的非零起点只是守门，不承担排序义务
	for i := range v.Items {
		it := &v.Items[i]
		switch {
		case it.Ref == "":
			return fmt.Errorf("item[%d]: empty ref", i)
		case !it.WireRole.Valid():
			return fmt.Errorf("item[%d] ref=%s: invalid wire_role %q", i, it.Ref, it.WireRole)
		case !it.Stability.Valid():
			return fmt.Errorf("item[%d] ref=%s: invalid stability %q", i, it.Ref, it.Stability)
		}
		rank, ok := stabilityRankBrief(it.Stability)
		if !ok {
			return fmt.Errorf("item[%d] ref=%s: stability %q unrankable", i, it.Ref, it.Stability)
		}
		if prevRank >= 0 && rank < prevRank {
			return fmt.Errorf("item[%d] ref=%s: stability %q out of order (volatile must stay in tail, frozen→stable→volatile)", i, it.Ref, it.Stability)
		}
		prevRank = rank
	}
	return nil
}

// stabilityRankBrief 是 StabilityRank 的最小内部排序器（frozen=0 < stable=1 <
// volatile=2）。与 wire 包的 StabilityRank 语义一致但独立实现，原因：types 包
// 不依赖 wire（它是最底层契约包，见 doc.go），而排序规则是 View 校验的需要。
// 两侧漂移的防线是 stage 2 的 agent 化测试；若规则变更须同步两处（ADR 记录）。
//
// 并发：纯函数。
func stabilityRankBrief(s Stability) (int, bool) {
	switch s {
	case StabilityFrozen:
		return 0, true
	case StabilityStable:
		return 1, true
	case StabilityVolatile:
		return 2, true
	}
	return -1, false
}
