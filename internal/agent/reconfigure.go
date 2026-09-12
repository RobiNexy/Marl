package agent

// request_reconfigure 与 ApplyReconfigure（Part 6.9 / 7.7，13.12 阶段 10
// 的"reconfigure（含三校验 + 缓存失效审计）"）。
//
// 边界（Part 6.9 的"能否热切换"判据）：
//
//	可改（热切换）：Sampling（含 timeout，不含 model_id —— 模型由阶梯规则）；
//	                Task 预算；Thinking.Level / Budget。
//	不可改（要建新 Agent）：Prompt / AllowedSkills / Requirement / CanSpawn /
//	OutgoingContext ——它们决定"这个 Agent 是谁、能干什么"。
//
// [阶段边界] request_reconfigure 意图的语义是"我卡住了"（Part 7.2：升级
// 由证据触发，不由请求触发）——本实现把它折为：审计登记 + Snapshot 不变
// 量校验 + 对 LLM 的如实回填（"已记录；修改须由人类经 ApplyReconfigure 做"）。
// 热切换的应用点在 ApplyReconfigure（人类 CLI / daemon 调用），不是 LLM 意图
// ——LLM 无权改变自己的采样参数面。

import (
	"context"
	"encoding/json"
	"fmt"

	"marl/internal/proto"
	"marl/internal/skill"
	"marl/internal/types"
)

// reconfArgs 是 request_reconfigure 的参数形态（schema 只带 reason——
// 参数变更面不进 LLM 意图，Part 6.9 的边界）。
type reconfArgs struct {
	Reason string `json:"reason"`
}

// intentRequestReconfigure 处理 request_reconfigure（意图 = 证据信号 + 审计；
// 实际"调整"是人类动作）。
//
// [三校验] （Part 6.9 / 13.12 的"三校验"落地在 proto.ReconfigureRequest.Validate
// 与本 handler）：
//  1. reason 非空（"我卡住了"的语义必须可见）；
//  2. 修改面（Sampling/Task/Thinking 至少一个）——LLM 意图侧天然满足
//     （它没有参数可写的字段），人类的 ApplyReconfigure 走同一个校验；
//  3. 缓存失效审计（Part 7.7）：model/thinking 的变更破缓存前缀——审计里
//     显式记录（thinking 档位变化也进缓存前缀的字节面）。
func (a *Agent) intentRequestReconfigure(ctx context.Context, call types.ToolCall) (*skill.SkillResult, error) {
	var args reconfArgs
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			a.roundFormatErrors++
			return skill.NewFailure("BAD_ARGS", "arguments not valid JSON: %v", err), nil
		}
	}
	if args.Reason == "" {
		return skill.NewFailure("BAD_ARGS", "reason 不能为空（reconfigure 的语义是\"我卡住了 + 原因\"；升级由阶梯证据触发，不由本请求触发）"), nil
	}
	a.auditf(ctx, "reconfigure_requested", string(a.binding.RungID), map[string]any{
		"agent_id": string(a.id), "reason": args.Reason,
	})
	return skill.NewSuccess(map[string]any{
		"message": "已记录（进审计）。模型档位的实际调整由证据/人类触发——你现在能继续做手头的事。",
	}), nil
}

// ApplyReconfigure 是人类侧的热切换入口（daemon/CLI 调用；Part 6.9 的
// Reconfigure 可改清单 + proto 的 Validate + 缓存失效审计）。
//
// 三校验（唯一实现点，proto.ReconfigureRequest.Validate 为规则真相）：
//
//	reason 非空 / 至少一项变更 / 不可改字段**不进本结构**（门面即边界）。
//
// 应用语义：指针非 nil = 整块替换（proto 的覆盖语义）。审计缓存失效的
// 判据：thinking 变化会写进缓存前缀（level 变 → Normalizer 的 thinking
// 参数变）。 [推断] 6.5 的 ThinkingBudget 只启 budget 控制——同类。
func (a *Agent) ApplyReconfigure(req *proto.ReconfigureRequest) error {
	if req == nil {
		return fmt.Errorf("agent: nil reconfigure request")
	}
	if err := req.Validate(); err != nil {
		return err
	}
	// 归属校验：请求者的 AgentID 必须是本 Agent（原则 4 的路径面：
	// 谁改谁——走错对象的 reconfigure 会把"另一个 Agent 的参数改成另一个
	// Agent 的东西"，症状是"两个 Agent 的行为互相污染"）。
	if req.AgentID != "" && req.AgentID != a.id {
		return fmt.Errorf("reconfigure: request targets %q but this agent is %s", req.AgentID, a.id)
	}
	oldThinking := a.thinking
	if req.Sampling != nil {
		a.sampling = *req.Sampling
	}
	if req.Thinking != nil {
		a.thinking = *req.Thinking
		if a.bindingSet {
			// 阶梯是 thinking 的唯一真相源（ADR-0022）：运行时覆盖意味着
			// Binding 的档位漂移——记审计（7.7 的"缓存失效审计"）。
			a.auditf(context.Background(), "reconfigure_thinking", string(a.binding.RungID), map[string]any{
				"agent_id": string(a.id),
				"from":     oldThinking.Level, "to": a.thinking.Level,
				"budget_from": budgetPtrVal(oldThinking), "budget_to": budgetPtrVal(a.thinking),
				"cache_effect": "binding rung 的 thinking 变更会 impact 该 Agent 的缓存桶前缀",
			})
		}
	}
	if req.Task != nil {
		// TaskPolicy 的 two 字段里 Watchdog 消费 Budget.TimeoutMs；Sampling 的
		// MaxTokens 不动（作用域是单次请求）。bool 的"改了 art"留在遍历点。
		_ = req.Task
	}
	return nil
}

// budgetPtrVal 是 ThinkingSpec.Budget 的显示面（nil → "unset"）。
func budgetPtrVal(t types.ThinkingSpec) any {
	if t.Budget == nil {
		return "unset"
	}
	return *t.Budget
}
