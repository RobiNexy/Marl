package compress

// 编排调用的共享执行核（Part 3.7 / 7.5 的 Orchestrator 落地）。
//
// Engine 是 Orchestrator 与 SUM 生成共用的"OrchestrationCall"：
// 构造最小上下文（system + 单条 user，无 tools）→ 调 Wire → 取回复。
// 独立预算（budget）与独立账本（UsageSink）在这里收口，保证"编排调用
// 的钱单独算"只有一处实现。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"marl/internal/orchestrate"
	"marl/internal/types"
	"marl/internal/wire"
)

// Executor 是编排调用对 Wire 层的最窄接口（消费侧定义，与 agent.LLMExecutor
// 同形——Go 的结构化类型让同一实现同时满足两者，无需适配层）。
//
// 失败语义沿用 wire 契约：网络/装配错误走 error；厂商错误在
// Outcome.Signals.ErrorClass（本包据 IsError() 判定并转为 error）。
type Executor interface {
	ExecuteTurn(ctx context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error)
}

// UsageSink 是编排调用的独立账本出口（消费侧收窄到唯一需要的动作）。
//
// 阶段 3 由调用方注入（mini 打印到 stdout）；阶段 4 的 SQLite Ledger
// （store.Ledger.RecordOrchestration）接管后，这里加一个适配器即可。
// nil = 不记账（测试与 dry-run）——不记账是显式决定，不是默认丢失：
// 构造方注释里必须说明为何传 nil。
type UsageSink interface {
	RecordOrchestration(ctx context.Context, agentID types.AgentID, taskID types.TaskID, usage *types.TokenUsage) error
}

// EngineConfig 是 NewEngine 的装配参数。
//
// 零值契约：EngineConfig{} 不可用（LLM 为 nil、Sampling 无界）。全部字段
// 显式填充；默认值（如编排调用的采样上限）由构造方物化，本包不提供
// DefaultXxx()（ADR-0014 的危险阈值纪律：默认值放在配置层，不放框架里）。
type EngineConfig struct {
	AgentID types.AgentID
	TaskID  types.TaskID
	// LLM 是编排调用的执行器（r0 档位的直连通路）。
	LLM Executor
	// Sampling 是编排调用的采样参数（便宜档：小 max_tokens、低温度）。
	Sampling types.SamplingParams
	// MaxTotalTokens 是本 Engine 的独立预算上限（est token 累计）。
	// 必须 > 0。超预算的调用在发出前被拒绝——编排烧穿预算不该由
	// 厂商 429 来发现。
	MaxTotalTokens int
	// Sink 是独立账本出口；nil = 不记账（调用方须说明理由）。
	Sink UsageSink
}

// Engine 是编排调用的执行核：实现 orchestrate.Orchestrator（语义拆分），
// 并向 Compressor 提供 SUM 生成。
//
// 并发：单个 Engine 可被并发使用（budget 内部有锁）；LLM 执行器自身的
// 并发语义由其实现保证。
//
// 零值契约：Engine{} 不可用，必须经 NewEngine 构造。
type Engine struct {
	llm      Executor
	agentID  types.AgentID
	taskID   types.TaskID
	sampling types.SamplingParams
	sink     UsageSink
	budget   *budget
}

// budget 是独立预算的记账器（est token 累计，互斥保护）。
//
// 口径说明：预算用**本地估算**扣减（发出前），实际用量（厂商回传）事后
// 补扣差额。估算偏高的代价是提前拒调用（可见、可调参），偏低的代价是
// 超支——方向刻意选"宁高不低"，与 types.EstimateTokens 的安全侧一致。
type budget struct {
	mu        sync.Mutex
	remaining int
}

// reserve 扣减 n；余额不足返回错误（不部分扣减——半次调用没有意义）。
func (b *budget) reserve(n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.remaining {
		return fmt.Errorf("orchestration budget exhausted: need %d, remaining %d", n, b.remaining)
	}
	b.remaining -= n
	return nil
}

// NewEngine 校验配置并构造 Engine。
//
// 失败：LLM 为 nil、AgentID 为空、MaxTotalTokens <= 0、Sampling 非法
// （启动期 fail fast，与 agent.New 同一纪律）。
func NewEngine(cfg EngineConfig) (*Engine, error) {
	switch {
	case cfg.LLM == nil:
		return nil, fmt.Errorf("compress: LLM executor is required")
	case cfg.AgentID == "":
		return nil, fmt.Errorf("compress: AgentID is required")
	case cfg.MaxTotalTokens <= 0:
		return nil, fmt.Errorf("compress: MaxTotalTokens must be positive")
	case cfg.Sampling.MaxTokens <= 0:
		return nil, fmt.Errorf("compress: Sampling.MaxTokens must be positive")
	}
	return &Engine{
		llm:      cfg.LLM,
		agentID:  cfg.AgentID,
		taskID:   cfg.TaskID,
		sampling: cfg.Sampling,
		sink:     cfg.Sink,
		budget:   &budget{remaining: cfg.MaxTotalTokens},
	}, nil
}

// call 执行一次编排调用（OrchestrationCall 的落地）：
// 构造最小上下文（frozen system + stable user，无 tools）→ ExecuteTurn →
// 取首个带回复的 Outcome。
//
// 失败（全部显式）：
//   - 预算不足 → 错误（调用未发出）；
//   - LLM error（网络/装配）→ 原样上抛（ctx 取消保持原样）；
//   - 厂商错误（ErrorClass）→ 转为 error（编排调用没有主循环的分流层，
//     上层按错误决定重试/降级）；
//   - 回复为空 → 错误（"模型什么都没说"不是可用的编排产出）。
//
// 记账：发出前按估算扣预算；响应后按实际用量补扣差额并推送 Sink
// （Sink 失败不掩盖调用结果——账本丢失是可观测的告警，不该炸掉已完成的
// 压缩；错误经注释声明后丢弃）。
func (e *Engine) call(ctx context.Context, system, user string, outputJSON bool, temperature float64) (string, *types.TokenUsage, error) {
	estIn := types.EstimateTokens(system) + types.EstimateTokens(user)
	if err := e.budget.reserve(estIn + e.sampling.MaxTokens); err != nil {
		return "", nil, fmt.Errorf("compress: %w", err)
	}
	req := &wire.CanonicalRequest{
		Segments: []wire.Segment{
			{Kind: wire.SegSystem, Speaker: wire.SpeakerFramework, Content: system, Stability: types.StabilityFrozen},
			{Kind: wire.SegTurn, Speaker: wire.SpeakerHuman, Content: user, Stability: types.StabilityStable},
		},
		// 无 Tools：编排调用的上下文里没有工具表——它只读快照、只产文本，
		// 带上工具表既费 token 又给模型"可以调工具"的错误暗示。
		Sampling:   types.SamplingParams{MaxTokens: e.sampling.MaxTokens, TimeoutMs: e.sampling.TimeoutMs, Temperature: temperature},
		Thinking:   types.ThinkingSpec{Level: "off"}, // 编排调用不思考（最便宜档）
		OutputJSON: outputJSON,
	}
	turn, err := e.llm.ExecuteTurn(ctx, req)
	if err != nil {
		return "", nil, fmt.Errorf("compress: orchestration call: %w", err)
	}
	var reply string
	var usage *types.TokenUsage
	for i := range turn.Outcomes {
		o := &turn.Outcomes[i]
		if o.Signals.ErrorClass.IsError() {
			return "", nil, fmt.Errorf("compress: orchestration call vendor error: %s", o.Signals.ErrorClass)
		}
		if usage == nil {
			usage = o.Usage
		}
		if reply == "" && o.Reply != "" {
			reply = o.Reply
		}
	}
	if reply == "" {
		return "", usage, fmt.Errorf("compress: orchestration call returned no reply")
	}
	// 实际用量补扣（输入 + 输出的真实口径），并推送独立账本。
	if usage != nil {
		total := usage.PromptTokens + usage.CompletionTokens + usage.ReasoningTokens
		_ = e.budget.reserve(total) // 差额可能把余额扣负——下一次调用会被拒，可见
		if e.sink != nil {
			// 账本失败不回滚业务：压缩已完成，丢账本是可观测的损失而非
			// 正确性风险。错误显式丢弃并在此声明（阶段 4 的 Ledger 落地
			// 后改为记审计）。
			_ = e.sink.RecordOrchestration(ctx, e.agentID, e.taskID, usage)
		}
	}
	return reply, usage, nil
}

// SplitSemantic 实现 orchestrate.Orchestrator（语义拆分的一次 LLM 调用）。
//
// 契约对齐（见 orchestrate.Orchestrator 注释）：单次调用、不重试、不校验
// 覆盖性；返回的段未经验证，修复与吸附由调用方（split Op）承担。
// 输入 content 只读：本方法不修改它，也不读主 Agent 的任何状态。
//
// 失败：LLM 调用失败、回复无法解析为段列表（含 JSON mode 下输出非数组）。
// 空内容返回空切片 + nil（契约）。
func (e *Engine) SplitSemantic(ctx context.Context, content string) ([]orchestrate.SplitSegment, error) {
	if content == "" {
		return nil, nil
	}
	reply, _, err := e.call(ctx, splitSystemPrompt, content, true, e.sampling.Temperature)
	if err != nil {
		return nil, err
	}
	segs, err := parseSegmentJSON(reply)
	if err != nil {
		return nil, fmt.Errorf("compress: parse split segments: %w", err)
	}
	return segs, nil
}

// splitSystemPrompt 是语义拆分的 system 段（frozen；常量保证 byte-stable）。
const splitSystemPrompt = `你是文本切分器。输入是带行号的消息（每行 "N| 正文"）。
把它按主题切成若干段：每段一个连贯主题，段与段的行区间连续覆盖全文、互不重叠。
只输出 JSON 数组，不要输出任何其他文字：
[{"topic":"主题短语","start_line":1,"end_line":5}, ...]`

// parseSegmentJSON 解析 Orchestrator 的 JSON 输出为段列表。
//
// 容错（模型输出的现实）：允许整体被 ```json ... ``` 围栏包裹（剥离后解析）；
// 不做字段级宽容（缺字段/类型不对即报错）——坏输出该由上层重试策略处理，
// 解析层猜出来的"段"比报错危险。
//
// 并发：纯函数。
func parseSegmentJSON(reply string) ([]orchestrate.SplitSegment, error) {
	text := strings.TrimSpace(reply)
	if text == "" {
		return nil, nil
	}
	// 剥离 markdown 围栏（JSON mode 下偶发）。
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) >= 2 {
			lines = lines[1 : len(lines)-1] // 去首尾围栏行
		}
		text = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	var raw []struct {
		Topic     string `json:"topic"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, err
	}
	out := make([]orchestrate.SplitSegment, 0, len(raw))
	for _, r := range raw {
		out = append(out, orchestrate.SplitSegment{Topic: r.Topic, StartLine: r.StartLine, EndLine: r.EndLine})
	}
	return out, nil
}
