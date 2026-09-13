package agent

// llm_call：Agent 的唯一受控副调用入口（Part 11.2，阶段 11）。
//
// 四条不变量（Part 11.2 §2.1）：
//  1. 纯函数式：一次调用，无循环无跨调用状态（编排是 Agent 的用法，不是框架机制）；
//  2. 无工具：sidecar 请求不带 tools；响应中的 tool_calls 原样回传给
//     Agent、**永不执行**（防嵌套 Agent 循环——分治模型的结构保证）；
//  3. 不占深度：不是子 Agent，不进进程表（它是一次系统调用）；
//  4. 一切计数：token 进任务预算、调用进账本（call_type=llm_call）、
//     gate 决策进审计。
//
// 三维度管制（§2.7）：单次输入超限 = 硬拒（INPUT_TOO_LARGE，不走 Gate——
// 物理 sanity）；调用次数 / 任务累计 token 超限 = Gate 的 need_human。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"marl/internal/config"
	"marl/internal/gate"
	"marl/internal/skill"
	"marl/internal/types"
	"marl/internal/wire"
)

const (
	// ErrWireNotFound / 其它错误码（Part 11.2 §2.9 的开放错误码面）。
	ErrCodeWireNotFound        = "WIRE_NOT_FOUND"
	ErrCodeModelNotInCatalog   = "MODEL_NOT_IN_CATALOG"
	ErrCodeInputTooLarge       = "INPUT_TOO_LARGE"
	ErrCodeGatePendingHuman    = "GATE_PENDING_HUMAN"
	ErrCodeGateDenied          = "GATE_DENIED"
	ErrCodeWireExecutionFailed = "WIRE_EXECUTION_FAILED"
)

// BindingOverride 是 llm_call 对 Binding 注入的扩展面（缓存桶细化：
// llm_call 的 CacheBucket = AgentID + "/" + wire 名，Part 11.2 §2.6）。
// 实现：cmd 的 line 装配（forkLine/directLine）——按注入的 Binding 归一
// 参数后发请求；无此实现（假执行器/测试替身）时信息退化为默认 binding。
type BindingOverride interface {
	ExecuteTurnAs(ctx context.Context, req *wire.CanonicalRequest, binding types.Binding) (*wire.WireTurn, error)
}

// WireRunners 是多 wire 的解析面（消费侧收窄：Agent 只需要"按名字出发
// 一次受控执行"）。
//
// 实现方（装配层）：按 config.llm_wires 构建静态 sidecar；
// "main" 的语义是**活引用**——每次调用都重新解析当前阶梯绑定。
type WireRunners interface {
	// Resolve 返回 wire 的执行器与 Binding。
	// 失败：name 未知 → false（错误码 WIRE_NOT_FOUND 的判定点在本 Intent）。
	Resolve(ctx context.Context, name string) (LLMExecutor, types.Binding, bool)
	// Names 返回可用 wire 名（错误消息面"列出全部可用 wire"）。
	Names() []string
	// AllowModels 是 model 覆盖的合法集合（catalog 的模型 id 面）。
	AllowModels() ([]string, bool)
}

// LLMCallConfig 是 llm_call 的装配块（Config.LLMCall 非 nil 即启用）。
type LLMCallConfig struct {
	// Limits 是经济面（config.Limits 的 lg 侧；llm_call 调用前校验用）。
	Limits config.Limits
	// Gates 是 Gate 的 PDP（llm_call 超限审批；nil = 无控制面 → 超限直接 deny）。
	Gates *gate.Manager
	// Wires 是多 wire 解析面（含 main；nil = 只允许 main 且用装配时的 llm）。
	Wires WireRunners
}

// errGatePending 是 eventLoop 的内部哨兵（llm_call 超限审批挂起）。
var errGatePending = errors.New("agent: llm_call awaiting gate")

// pendingGate 是等待人类审批的一次 llm_call（Agent 持有；awaitGate 消费）。
type pendingGate struct {
	req   *gate.Request
	attrs map[string]any
}

// llmCallArgs 是 schema 的参数形态。
type llmCallArgs struct {
	Messages       []llmCallMsg `json:"messages"`
	Wire           string       `json:"wire"`
	Model          string       `json:"model"`
	ResponseFormat string       `json:"response_format"`
	MaxTokens      int          `json:"max_tokens"`
	Temperature    float64      `json:"temperature"`
	Purpose        string       `json:"purpose"`
}

type llmCallMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// intentLLMCall 处理 llm_call（完整面：解析 → 上限 → Gate → wire 解析 →
// 执行 → 计数/账本）。
//
// 错误分类面（Part 11.2 §2.9）：INVALID_ARGS / WIRE_NOT_FOUND /
// MODEL_NOT_IN_CATALOG / INPUT_TOO_LARGE / GATE_* / WIRE_EXECUTION_FAILED。
// 所有数字进消息。
func (a *Agent) intentLLMCall(ctx context.Context, call types.ToolCall) (*skill.SkillResult, error) {
	if a.llmCallCfg == nil {
		return skill.NewFailure(ErrIntentNotHandled, "llm_call 未装配（框架级缺失；如实回填）"), nil
	}
	var args llmCallArgs
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			a.roundFormatErrors++
			return skill.NewFailure("INVALID_ARGS", "arguments 不是合法 JSON: %v（schema：messages/wire/model/...）", err), nil
		}
	}
	if len(args.Messages) == 0 {
		return skill.NewFailure("INVALID_ARGS", "messages 不能为空（至少一条指令）"), nil
	}
	for i, m := range args.Messages {
		switch strings.ToLower(m.Role) {
		case "system", "user", "assistant":
		default:
			return skill.NewFailure("INVALID_ARGS", "messages[%d].role=%q 不在 system/user/assistant/tool 内", i, m.Role), nil
		}
	}

	// ---- 三维度之一：单次输入上限（硬拒，物理 sanity，不走 Gate） ----
	inputTokens := 0
	for _, m := range args.Messages {
		inputTokens += types.EstimateTokens(m.Content)
	}
	if inputTokens > a.llmCallCfg.Limits.LLMCallMaxInputTokens {
		return skill.NewFailure(ErrCodeInputTooLarge,
			"输入 %d est-token 超出单次上限 %d（limits.llm_call.max_input_tokens）。请拆小或压缩后再调。",
			inputTokens, a.llmCallCfg.Limits.LLMCallMaxInputTokens), nil
	}

	// ---- Gate：每次调用都过规则评估（默认规则：限额内 allow） ----
	attrs := llmCallAttrs(a, &args, inputTokens)
	req := &gate.Request{Kind: gate.KindLLMCall, AgentID: a.id, Attributes: attrs}
	dec := a.gates().Decide(ctx, req)
	switch dec.Action {
	case gate.ActionAllow:
		// 额度内 / 规则放行 → 继续执行。
	case gate.ActionNeedHuman:
		// 挂起：PendingGate 的注册 + Blocked(AwaitingGate)（Run 外层）。
		a.mu.Lock()
		a.gatePending = &pendingGate{req: req, attrs: attrs}
		a.mu.Unlock()
		a.auditf(ctx, "gate_pending", "llm_call", map[string]any{
			"agent_id": string(a.id), "rule": dec.RuleID, "reason": dec.Reason, "input_tokens": inputTokens,
		})
		return skill.NewFailure(ErrCodeGatePendingHuman,
			"命中规则 %s：%s\n属性：%s\n已生成审批文件，等待人类裁决（本次调用未执行）。",
			dec.RuleID, dec.Reason, gate.AttributesSummary(attrs)), nil
	default:
		return skill.NewFailure(ErrCodeGateDenied, "命中规则 %s：%s", dec.RuleID, dec.Reason), nil
	}

	// ---- wire 解析（main = 活引用） ----
	exec, binding, bad := a.llmCallWire(&args)
	if bad != nil {
		return bad, nil // WIRE_NOT_FOUND / MODEL_NOT_IN_CATALOG 是业务结果（可改参数重试）
	}
	// ---- 归一化与调用 ----
	turn, err := a.llmCallRunner(ctx, exec, binding, &args)
	if err != nil {
		// 超时 / 基础设施故障 → transient 归一（不自动重试：重试是 Agent 的决策）。
		if errors.Is(err, context.DeadlineExceeded) {
			return skill.NewFailure(ErrCodeWireExecutionFailed,
				"调用超时（limit.llm_call.timeout_ms=%d；transient 类，不自动重试——拆小 payload 后重试是你的决策）",
				a.llmCallCfg.Limits.LLMCallTimeoutMs), nil
		}
		return nil, err // 网络故障上抛（eventLoop 终结，与主通路同契约）
	}
	// ---- 计数与账本（不变量 4） ----
	a.mu.Lock()
	a.llmCallCount++
	a.llmCallTokens += estTokensOf(usageOfTurn(turn))
	a.mu.Unlock()
	a.recordLLMCallLedger(ctx, binding, usageOfTurn(turn))
	return llmCallResultOf(turn), nil
}

// llmCallAttrs 是 Gate 属性表（每次调用都评估——不只有超额才问）。
func llmCallAttrs(a *Agent, args *llmCallArgs, inputTokens int) map[string]any {
	return map[string]any{
		"task_call_count": a.llmCallCount + 0, // 当前是第几次（促成 >=20 判据）
		"task_tokens":     a.llmCallTokens,
		"wire":            args.Wire,
		"purpose":         args.Purpose,
		"input_tokens":    inputTokens,
	}
}

// gates 的默认面（Manager 未装配 → deny 的显式拒绝面）。
func (a *Agent) gates() gate.Decider {
	if a.llmCallCfg.Gates != nil {
		return a.llmCallCfg.Gates
	}
	return nilPDP{}
}

// nilPDP 的显式拒绝面（配错装配时调用直接拒绝——"没人把关"不能变成放行）。
type nilPDP struct{}

func (nilPDP) Evaluate(ctx context.Context, req *gate.Request) gate.Decision {
	return gate.Decision{Action: gate.ActionDeny, RuleID: "no-gate-config",
		Reason: "未装配 Gate（limits 缺失装配）；llm_call 默认拒绝即便参数在限内"}
}

func (nilPDP) Decide(ctx context.Context, req *gate.Request) gate.Decision {
	return nilPDP{}.Evaluate(ctx, req)
}

// llmCallWire 解析 wire（main = 活引用；sidecars 的命名空间见 WireRunners）。
//
// model 覆盖校验：catalog 面（AllowModels）；缺 catalog 语义 = 装配面没有
// 模型清单 → 拒（自铸目录没有意义——Part 11.2 §2.4 的"不合法覆盖"）。
func (a *Agent) llmCallWire(args *llmCallArgs) (LLMExecutor, types.Binding, *skill.SkillResult) {
	name := args.Wire
	if name == "" {
		name = "main"
	}
	var ex LLMExecutor
	var b types.Binding
	if name == "main" {
		ex, b = a.llm, a.binding
	} else if a.llmCallCfg.Wires != nil {
		var ok bool
		ex, b, ok = a.llmCallCfg.Wires.Resolve(a2Ctx(), name)
		if !ok {
			return nil, types.Binding{}, skill.NewFailure(ErrCodeWireNotFound,
				"wire=%q 不存在（可用：%s）", name, strings.Join(a.llmCallCfg.Wires.Names(), ", "))
		}
	} else {
		return nil, types.Binding{}, skill.NewFailure(ErrCodeWireNotFound,
			"wire=%q 不存在（可用：%s）", name, "main")
	}
	// model 覆盖（sidecar 的 catalog 内换模型重试；校验在 AllowModels 面）。
	if args.Model != "" && args.Model != b.Model {
		if a.llmCallCfg.Wires == nil {
			return nil, types.Binding{}, skill.NewFailure(ErrCodeModelNotInCatalog,
				"model=%q 不在 catalog 内（该 wire 可用：%s）", args.Model, b.Model)
		}
		all, ok := a.llmCallCfg.Wires.AllowModels()
		if !ok || !containsStr(all, args.Model) {
			return nil, types.Binding{}, skill.NewFailure(ErrCodeModelNotInCatalog,
				"model=%q 不在 catalog 内（该 wire 可用：%s）", args.Model, strings.Join(all, ", "))
		}
		b.Model = args.Model
	}
	// 缓存桶细化：llm_call 的独立桶（Part 11.2 §2.6 的隔离面）。
	b.CacheBucket = types.AgentID(string(a.id) + "/" + name)
	b.Wire = types.WireOpenAIChat
	return ex, b, nil
}

func a2Ctx() context.Context { return context.Background() }

// LLMCallRunners 的执行面（BindingOverride 优先，缺省二线）。

func (a *Agent) llmCallRunner(ctx context.Context, exec LLMExecutor, b types.Binding, args *llmCallArgs) (*wire.WireTurn, error) {
	req := buildLLMCallRequest(b, args, a.llmCallCfg.Limits)
	if bo, ok := exec.(BindingOverride); ok {
		return bo.ExecuteTurnAs(ctx, req, b)
	}
	return exec.ExecuteTurn(ctx, req)
}

// buildLLMCallRequest 把参数编到 CanonicalRequest（无工具、消息全转段面）。
func buildLLMCallRequest(b types.Binding, args *llmCallArgs, limits config.Limits) *wire.CanonicalRequest {
	req := &wire.CanonicalRequest{OutputJSON: false}
	for _, m := range args.Messages {
		role := strings.ToLower(m.Role)
		switch role {
		case "system":
			req.Segments = append(req.Segments, wire.Segment{
				Kind: wire.SegSystem, Speaker: wire.SpeakerFramework,
				Content: m.Content, Stability: types.StabilityStable})
		case "user":
			req.Segments = append(req.Segments, wire.Segment{
				Kind: wire.SegTurn, Speaker: wire.SpeakerHuman,
				Content: m.Content, Stability: types.StabilityStable})
		default:
			req.Segments = append(req.Segments, wire.Segment{
				Kind: wire.SegTurn, Speaker: wire.SpeakerAssistant,
				Content: m.Content, Stability: types.StabilityStable})
		}
	}
	// 参数面（Agent 可调小、不可超 config 上限——取 min）。
	mt := 1024
	if args.MaxTokens > 0 {
		mt = args.MaxTokens
	}
	if mt > 2000 {
		mt = 2000
	}
	samp := types.SamplingParams{MaxTokens: mt, TimeoutMs: limits.LLMCallTimeoutMs}
	if args.Temperature < 0 {
		args.Temperature = 0
	}
	if args.Temperature > 1 {
		args.Temperature = 1
	}
	samp.Temperature = args.Temperature
	req.Sampling = samp
	if args.ResponseFormat == "json" {
		req.OutputJSON = true // wire 无 JSON mode 时 Normalizer 记 Degradation 降级（不报错）
	}
	return req
}

// estTokensOf 的计数面（usage 未知 ≤ 不计数——上层账本同契约）。
func estTokensOf(u *types.TokenUsage) int64 {
	if u == nil {
		return 0
	}
	return int64(u.PromptTokens + u.CompletionTokens)
}

// usageOfTurn / recordLLMCallLedger 的记账面（Part 11.2 §2.8）。
func usageOfTurn(turn *wire.WireTurn) *types.TokenUsage {
	if turn == nil {
		return nil
	}
	for _, o := range turn.Outcomes {
		if o.Usage != nil {
			return o.Usage
		}
	}
	return nil
}

// recordLLMCallLedger 是 RecordLLMCall 的 Agent 侧包（Ledger nil 跳过）。
func (a *Agent) recordLLMCallLedger(ctx context.Context, b types.Binding, usage *types.TokenUsage) {
	if a.ledger == nil || usage == nil {
		return
	}
	if err := a.ledger.RecordLLMCall(ctx, a.taskID, a.id, b, usage); err != nil {
		fmt.Printf("marl: llm_call ledger record failed (agent=%s): %v\n", a.id, err)
	}
}

// llmCallResultOf 把 WireTurn 折成 tool_result 内容（结果 + 返回中的
// tool_calls 原样回传 + 永不执行的显式声明——不变量 2）。
func llmCallResultOf(turn *wire.WireTurn) *skill.SkillResult {
	var sb strings.Builder
	for _, o := range turn.Outcomes {
		if o.Reply != "" {
			sb.WriteString(o.Reply)
			sb.WriteString("\n")
		}
		if len(o.ToolCalls) > 0 {
			b, _ := json.Marshal(o.ToolCalls)
			fmt.Fprintf(&sb, "（sidecar 返回了 %d 个 tool_calls——已原样回传，**永不执行**）：\\n%s\\n", len(o.ToolCalls), b)
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return skill.NewFailure(ErrCodeWireExecutionFailed,
			"厂商空回（无文本无调用）；transient 或厂商容量问题——重试是你的决策")
	}
	return skill.NewSuccess(map[string]any{"content": out})
}

// containsStr 是 catalog 面的成员判定（model 覆盖的合法性）。
func containsStr(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// awaitGate 进入 Blocked(AwaitingGate) 等待人类裁决（Part 11.3 的挂起面；
// 等待期零 LLM 调用）。GATE_PENDING 的 tool_result 已经回填给了模型——
// 审批通过后**下一轮由模型重发 llm_call**（session grant 让它直行）；
// deny 同理，模型读到拒绝原因自行调整。框架不做"审批后自动重放"——
// 调度权在 Agent 手里（Part 11.7 的 defer/commit 已砍）。
//
// 恢复后注入一条 RoleHumanNote 的裁决摘要（RuleID/Grant）让上下文里
// 有对账键。ctx 取消保留 pending（讨论语义一致）。
func (a *Agent) awaitGate(ctx context.Context) error {
	a.mu.Lock()
	p := a.gatePending
	a.mu.Unlock()
	if p == nil {
		return nil
	}
	a.state = types.StateBlocked
	a.blockReason = types.BlockAwaitingGate
	a.auditState(ctx, "blocked", string(types.BlockAwaitingGate))
	// 阻塞到 Approver 返回（Manager 的 need_human 面已 block 在 FileApprover
	// 的轮询里）。重放 Evalu ate：规则可能已被"always-grant"短路。
	dec := a.gates().Evaluate(ctx, p.req)
	a.state = types.StateRunning
	a.blockReason = ""
	a.auditState(ctx, "running", "")
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	e := types.NewLogEntry(a.id, types.RoleHumanNote,
		fmt.Sprintf("Gate 裁决：rule=%s action=%s（%s）", dec.RuleID, dec.Action, dec.Reason))
	e.Meta = map[string]any{"gate_kind": string(p.req.Kind), "rule": dec.RuleID}
	if _, err := a.log.Append(ctx, e); err != nil {
		return fmt.Errorf("agent: append gate verdict: %w", err)
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	a.auditf(ctx, "gate_resolved", string(p.req.Kind), map[string]any{
		"agent_id": string(a.id), "rule": dec.RuleID, "action": string(dec.Action),
	})
	a.mu.Lock()
	a.gatePending = nil
	a.mu.Unlock()
	return nil
}

// ---- 装配面：静态 wire 集（llm_wires 配置的载体） ----

// SidecarSpec 是装配层给一条 wire 的实现（daemon / cmd 构造：Normalizer +
// Adapter + Denormalizer 的折算闭包，形态与 mini 的 directLine 同构）。
type SidecarSpec struct {
	Name    string // "sidecar-cheap" 等（"main" 不允许——活引用在 Agent 内部解析）
	Ex      LLMExecutor
	Binding types.Binding // 含 endpoint/model/thinking；CacheBucket 由 Agent 在调用时细化
}

// WireSet 是 WireRunners 的静态实现（config.llm_wires 的运行形态）。
type WireSet struct {
	specs       map[string]SidecarSpec
	allowModels []string
}

var _ WireRunners = (*WireSet)(nil)

// NewWireSet 构造（spec 名重复 / 含 "main" 是装配期错误——fail fast）。
func NewWireSet(specs []SidecarSpec, allowModels []string) (*WireSet, error) {
	ws := &WireSet{specs: map[string]SidecarSpec{}, allowModels: allowModels}
	for _, s := range specs {
		if s.Name == "" {
			return nil, fmt.Errorf("wireset: sidecar name empty")
		}
		if s.Name == "main" {
			return nil, fmt.Errorf("wireset: \"main\" 是活引用，不允许静态注册")
		}
		if s.Ex == nil {
			return nil, fmt.Errorf("wireset: %s executor nil", s.Name)
		}
		if _, dup := ws.specs[s.Name]; dup {
			return nil, fmt.Errorf("wireset: duplicate sidecar %s", s.Name)
		}
		ws.specs[s.Name] = s
	}
	return ws, nil
}

// Resolve / Names / AllowModels 的只读面。
func (w *WireSet) Resolve(ctx context.Context, name string) (LLMExecutor, types.Binding, bool) {
	s, ok := w.specs[name]
	return s.Ex, s.Binding, ok
}

func (w *WireSet) Names() []string {
	out := make([]string, 0, len(w.specs)+1)
	out = append(out, "main")
	for n := range w.specs {
		out = append(out, n)
	}
	return out
}

func (w *WireSet) AllowModels() ([]string, bool) { return w.allowModels, true }

// llmCallLimits 的 getter（nil 装配 = 兜底默认——只有破坏分级读它）。
func (a *Agent) llmCallLimits() config.Limits {
	if a.llmCallCfg != nil {
		return a.llmCallCfg.Limits
	}
	return config.DefaultLimits()
}

// orchRulesDefaults 是编排分级的默认规则表（Part 11.5 的字面形态；
// daemon 装配用——测试里显式传同样的形状以覆盖同一路径）。
var orchRulesDefaults = []gate.Rule{
	{ID: "allow-low-destruction", Match: map[string]string{"kind": "orchestration", "cache_destroyed_pct": "<10"}, Action: gate.ActionAllow},
	{ID: "review-destructive", Match: map[string]string{"kind": "orchestration"}, Action: gate.ActionNeedHuman, Reason: "高破坏编排需人"},
}
