package orchestrate

// ViewOps 是编排技能的框架侧执行面（Agent 实现；阶段 11 补遗 §3-§4 的
// 语义收口：匹配 → 破坏分级 → Gate → 机械落地全部在一个方法里，技能
// 只是参数声明壳——framing 的实现细节不进技能包）。

import (
	"context"

	"github.com/RobiNexy/Marl/internal/types"
)

// ViewOps 是 Agent / 宿主须提供的执行面（技能 Execute 只调它一个方法）。
//
// 语义（Part 补遗 §4"sidecar 返回不自动落 View / 显式落地全过 Gate"）：
//   - 定位：框架按 TargetSelector 匹配（精确→归一→歧义摘录）；
//   - pin/unpin：零破坏（跳过 Slate 评估，直接执行）——Part 补遗 §6 表；
//   - 其余：破坏量算好 → Gate 评估（need_human 由执行面挂起——返回
//     带 GATE_PENDING_HUMAN 的失败结果给模型）。
//   - split / semantic LLM 用法不在本面（那是 llm_call 的职责域；本面
//     只承载"机械编排"五件套——补遗 §1 的"机械/语义分界"表）。
type ViewOps interface {
	// Execute 执行一类机械编排操作。
	//
	// 前置：kind ∈ {OpExclude, OpRestore, OpReorder, OpAnnotate, OpPin, OpUnpin}
	//（OpSplit 走 llm_call + 显式落地，不走本面）。
	// 后置：结果为 SkillResult 形态的 map（ok/message/error/…）；失败
	// 不 panic。
	Execute(ctx context.Context, kind OpKind, sel TargetSelector, params map[string]any) map[string]any
}

// 编译期形态谐（Agent 侧的实现以 orchestrate 目标匹配器为唯一起源）。
var _ = types.MessageID("")
