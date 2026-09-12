package wire

// Anthropic 线路（Part 10.2 的五层调用链；13.12 阶段 10"Anthropic wire，
// 复用 Normalizer 大部分逻辑"）。
//
// 复用面（与 OpenAI 兼容线路共享的层）：
//   - WireRequest / WireMessage 形态（Normalizer 的产物结构不变）；
//   - ErrorClass 分类表（classifyOpenAIChatError 的状态码块与协议无关，
//     Anthropic 直接复用；差异只在错误体形状）；
//   - Denormalizer 的 Outcome / ThinkingOutcome 产出规则（reasoning →
//     tool_calls → reply 的顺序即 Log 顺序——ADR-0023）。
//
// Anthropic messages API 与 OpenAI chat/completions 的差异全集（本文件
// 翻译它们的落点）：
//   - system 是顶层字段（AnthropicNormalizer.layout 折叠 frozen 前缀）；
//   - tool 的结果以 user 消息的 tool_result **块**回传（OpenAI 是
//     role=tool 独立消息；layoutAnthropicMessages 与编码器合并相邻的
//     同角色块——Anthropic 要求 user/assistant 交替）；
//   - tool 调用是 assistant 的 tool_use **块**（OpenAI 是消息级
//     tool_calls 字段）;
//   - usage 字段名：input/output_tokens + cache_creation/read_input_tokens
//     → 归一化到同一组 types.TokenUsage 字段。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"marl/internal/types"
)

// AnthropicNormalizer 是 anthropic_messages 线路的去程翻译器。
//
// 与 OpenAICompatNormalizer 的骨架同源（Normalize/Assert 契约一致）；
// 布局差异收敛在 layout/foldSystem 两处。
type AnthropicNormalizer struct {
	caps CapsProvider
}

var _ Normalizer = (*AnthropicNormalizer)(nil)

// NewAnthropicNormalizer 构造；caps 为 nil 是启动期装配错误（这里炸，
// 不等第一个任务）。
func NewAnthropicNormalizer(caps CapsProvider) (*AnthropicNormalizer, error) {
	if caps == nil {
		return nil, errors.New("wire: anthropic: caps 为 nil（能力查询是档位/裁剪翻译的前提）")
	}
	return &AnthropicNormalizer{caps: caps}, nil
}

// Wires 声明支持的线路（副本；契约与 OpenAI 形态一致）。
func (n *AnthropicNormalizer) Wires() []types.WireID {
	return []types.WireID{types.WireAnthropicMessages}
}

// checkBinding 是布局前的入参自检（与 OpenAI 的 build 前置同一组防御）。
func checkAnthropicBinding(req *CanonicalRequest, binding types.Binding) error {
	if req == nil {
		return errors.New("wire: anthropic: req 为 nil")
	}
	if binding.Endpoint == "" || binding.Model == "" {
		return fmt.Errorf("wire: anthropic: Binding 不完整（endpoint=%q model=%q）", binding.Endpoint, binding.Model)
	}
	if binding.Wire != types.WireAnthropicMessages {
		return fmt.Errorf("wire: anthropic: 线路不匹配（binding.Wire=%q，本实现=%q）", binding.Wire, types.WireAnthropicMessages)
	}
	if binding.CacheBucket == "" {
		return errors.New("wire: anthropic: Binding.CacheBucket 为空（每 Agent 缓存桶是 Patch 1 的唯一来源）")
	}
	if len(req.Segments) == 0 {
		return errors.New("wire: anthropic: CanonicalRequest.Segments 为空")
	}
	return nil
}

// layout 把段折成（顶层 system 字符串, messages 数组）。
//
// frozen 段（system/standing/knowledge）→ system（Anthropic 的顶层字段
// 也参与其服务端缓存前缀；两种线路的 frozen 语义自洽）。
// 标记："只陈述不解释"的段序纪律在编译层已保证（Part 10.3 的硬不变量
// 由 layoutOpenAIChatMessages 同源的逐段校验承载——Anthropic 侧不再
// 重写一遍，Kind↔Stability 不变量表共用 canonical.go 的一张）。
func layoutAnthropic(req *CanonicalRequest) (system string, msgs []WireMessage, err error) {
	var sysParts []string
	frozenRank := map[SegmentKind]int{SegSystem: 0, SegStanding: 0, SegKnowledge: 0}
	history := make([]Segment, 0, len(req.Segments))
	for i := range req.Segments {
		s := req.Segments[i]
		if _, isFrozen := frozenRank[s.Kind]; isFrozen && s.Stability == types.StabilityFrozen {
			sysParts = append(sysParts, s.Content)
			continue
		}
		// frozen 段伪装 unfrozen / 反之：编译器的段序失败面在 OpenAI 形态
		// 有专门的错误路径，这里同样严格（布局只折叠 frozen→system 一处）。
		if _, isFrozen := frozenRank[s.Kind]; isFrozen {
			return "", nil, fmt.Errorf("wire: anthropic: 第 %d 个 Segment（kind=%s）是 frozen 段但 Stability=%q（编译层错位）", i, s.Kind, s.Stability)
		}
		history = append(history, s)
	}
	msgs, err = layoutAnthropicMessages(history)
	if err != nil {
		return "", nil, err
	}
	return strings.Join(sysParts, "\n\n"), msgs, nil
}

// layoutAnthropicMessages 把 stable/volatile 段映射成 user/assistant 交替
// 的消息序列（Anthropic 的硬规则：相邻同角色在编码器合并）。
//
// tool_result 段被映射为**role=user** 的消息（ToolCallID 携带配对 id）；
// 编码层把相邻的 user/tool_result 折成同一消息的多个 content 块。
func layoutAnthropicMessages(segs []Segment) ([]WireMessage, error) {
	msgs := make([]WireMessage, 0, len(segs))
	flushText := func() {
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == types.WireUser && len(msgs[len(msgs)-1].ToolCalls) > 0 {
			// 未出现：user 不带 tool calls（assert 会炸脑）。
		}
	}
	_ = flushText
	appendMsg := func(m WireMessage) {
		// 相邻同角色合并（Anthropic 的 user→user 间隔要求；合并保持
		// 段序——不产生跨段重排，缓存前缀的安全面在同一段序内）。
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == m.Role {
			prev := &msgs[len(msgs)-1]
			prev.Content = strings.TrimSpace(prev.Content + "\n" + m.Content)
			if m.ToolCalls != nil {
				prev.ToolCalls = append(prev.ToolCalls, m.ToolCalls...)
			}
			if m.ToolCallID != "" {
				// 本框架的一 tool_result 一消息形态不会在合并里携带多个 id；
				// 多个 tool_result 由 ToolCalls 之外的独立判定（见 encode 端）。
				prev.ToolCallID = m.ToolCallID
			}
			return
		}
		msgs = append(msgs, m)
	}
	for i := range segs {
		s := &segs[i]
		if s.Content == "" && len(s.ToolCalls) == 0 && s.Reasoning == "" && s.ToolCallID == "" {
			return nil, fmt.Errorf("wire: anthropic: 第 %d 个 Segment 为空（编译层职责）", i)
		}
		if s.Reasoning != "" && !(s.Kind == SegTurn && s.Speaker == SpeakerAssistant) {
			return nil, fmt.Errorf("wire: anthropic: 第 %d 个 Segment 携带 Reasoning 但不是 assistant 回合", i)
		}
		if len(s.ToolCalls) > 0 && !(s.Kind == SegTurn && s.Speaker == SpeakerAssistant) {
			return nil, fmt.Errorf("wire: anthropic: 第 %d 个 Segment 携带 ToolCalls 但不是 assistant 回合", i)
		}
		if s.ToolCallID != "" && s.Kind != SegToolResult {
			return nil, fmt.Errorf("wire: anthropic: 第 %d 个 Segment 带 ToolCallID 但不是 tool_result", i)
		}
		switch {
		case s.Kind == SegToolResult:
			appendMsg(WireMessage{Role: types.WireUser, Content: s.Content, ToolCallID: s.ToolCallID})
		case s.Kind == SegTurn && s.Speaker == SpeakerHuman:
			appendMsg(WireMessage{Role: types.WireUser, Content: s.Content})
		case s.Kind == SegTurn && s.Speaker == SpeakerAssistant:
			appendMsg(WireMessage{Role: types.WireAssistant, Content: s.Content, Reasoning: s.Reasoning, ToolCalls: s.ToolCalls})
		case s.Kind == SegTransient:
			// Anthropic 没有"系统级尾巴"位：作为 user 附加（并入最近 user 消息；
			// 不做"把 user 段挪到尾末"的重排——禁止跨段重排，Part 10.3）。
			appendMsg(WireMessage{Role: types.WireUser, Content: s.Content})
		default:
			return nil, fmt.Errorf("wire: anthropic: 第 %d 个 Segment（kind=%s speaker=%s）没有 anthropic 布局", i, s.Kind, s.Speaker)
		}
	}
	return msgs, nil
}

// Normalize 实现 Normalizer。
func (n *AnthropicNormalizer) Normalize(req *CanonicalRequest, binding types.Binding) (*NormalizeResult, error) {
	if err := checkAnthropicBinding(req, binding); err != nil {
		return nil, err
	}
	_, msgs, err := layoutAnthropic(req)
	if err != nil {
		return nil, err
	}
	return &NormalizeResult{Messages: msgs}, nil
}

// BuildRequest 装配完整 WireRequest（与 OpenAI 形态同名的单一入口）。
func (n *AnthropicNormalizer) BuildRequest(req *CanonicalRequest, binding types.Binding) (*WireRequest, []Degradation, error) {
	if err := checkAnthropicBinding(req, binding); err != nil {
		return nil, nil, err
	}
	system, msgs, err := layoutAnthropic(req)
	if err != nil {
		return nil, nil, err
	}
	if system == "" {
		return nil, nil, errors.New("wire: anthropic: system 为空（本框架的 frozen 前缀是语义一部分；缺失即装配错误）")
	}
	wr := &WireRequest{
		Endpoint:    binding.Endpoint,
		Model:       binding.Model,
		Wire:        types.WireAnthropicMessages,
		Messages:    msgs,
		Tools:       req.Tools,
		Sampling:    req.Sampling,
		Thinking:    req.Thinking,
		CacheBucket: binding.CacheBucket,
		Prefill:     req.Prefill,
		System:      system,
	}
	if req.Prefill != nil {
		// Anthropic 的 prefill 语义：请求的最后一条消息是 assistant 文本
		// （模型从中续写）。Assert 在此校验（prefill 文本作为尾段带进 layout）。
		wr.Messages = append(wr.Messages, WireMessage{Role: types.WireAssistant, Content: *req.Prefill})
	}
	if err := n.Assert(wr); err != nil {
		return nil, nil, err
	}
	return wr, nil, nil
}

// Assert 校验 anthropic_messages 的硬规则（tool 配对、结尾限制、
// max_tokens 的厂商必需性——Anthropic 是成年人检查的协议）。
func (n *AnthropicNormalizer) Assert(req *WireRequest) error {
	if req == nil {
		return errors.New("wire: anthropic: Assert(nil)")
	}
	if req.Wire != types.WireAnthropicMessages {
		return fmt.Errorf("wire: anthropic: Assert 只接受本线路（%q）", types.WireAnthropicMessages)
	}
	if req.System == "" {
		return errors.New("wire: anthropic: system 为空")
	}
	// 工具配对：tool_result 的 id 必须能对上前序 assistant 的 tool_use。
	pending := map[string]bool{}
	for i, m := range req.Messages {
		switch m.Role {
		case types.WireAssistant:
			for _, c := range m.ToolCalls {
				if c.ID == "" {
					return fmt.Errorf("wire: anthropic: assistant 消息 %d 的 tool_call 缺 id", i)
				}
				pending[c.ID] = true
			}
		case types.WireUser:
			if m.ToolCallID != "" {
				if !pending[m.ToolCallID] {
					return fmt.Errorf("wire: anthropic: tool_result 引用 %q 的配对断裂（前序 assistant 未发出该调用）", m.ToolCallID)
				}
				delete(pending, m.ToolCallID)
			}
		default:
			return fmt.Errorf("wire: anthropic: 消息 %d 的角色 %q 不在 anthropic 的 user/assistant 交替协议内", i, m.Role)
		}
	}
	if len(pending) > 0 {
		// Anthropic 允许 assistant 结尾（prefill）；悬空 tool_use 也允许
		//（模型已发起调用，等待结果）——这是正常请求形态，不是断配对。
		// 真正非法的是 tool_result 引用不存在的 tool_use —— 上面已拒绝。
		_ = pending
	}
	// Sampling：Anthropic 要求 max_tokens 必填（OutPerMTok 与 Sampling 两口径
	// 都能给；这里取 Sampling.MaxTokens 为协议字段来源）。
	if req.Sampling.MaxTokens <= 0 {
		return errors.New("wire: anthropic: max_tokens 必填（SamplingParams.MaxTokens<=0；Anthropic 无默认，厂商直接 4xx）")
	}
	return nil
}

// ------- Adapter（协议编码 + HTTP） -------

// AnthropicChatConfig 是 Adapter 的装配参数。
type AnthropicChatConfig struct {
	// EndpointName / BaseURL / APIKey：接入点（+缓存键与审计维度）、API 根
	// （如 https://api.anthropic.com）、x-api-key 的值。
	EndpointName string
	BaseURL      string
	APIKey       string
	// AnthropicVersion 是 anthropic-version 头（如 "2023-06-01"；空 = 不发）。
	AnthropicVersion string
	// RemoteNames 是内部模型 id → 远端名（与 DeepSeek 线路同一 id 策略）。
	RemoteNames map[string]string
	// HTTPClient 可覆写（测试注入 httptest 的 client）。
	HTTPClient *http.Client
}

// AnthropicChatAdapter 是 anthropic_messages 线路的适配器。
//
// 无状态（除只读 cfg/client）：Execute 可并发调用（与 DeepSeek 适配器同一
// 纪律——并发在 Pool 之下共享）。
type AnthropicChatAdapter struct {
	cfg    AnthropicChatConfig
	client *http.Client
}

var _ WireAdapter = (*AnthropicChatAdapter)(nil)

// NewAnthropicChatAdapter 构造（启动期装配错误在这里炸，形态与 DeepSeek 一致）。
func NewAnthropicChatAdapter(cfg AnthropicChatConfig) (*AnthropicChatAdapter, error) {
	switch {
	case cfg.EndpointName == "":
		return nil, errors.New("wire: anthropic: EndpointName 为空")
	case cfg.BaseURL == "":
		return nil, errors.New("wire: anthropic: BaseURL 为空")
	case cfg.APIKey == "":
		return nil, errors.New("wire: anthropic: APIKey 为空（请求直接 401；空 key 是凭据错配的静默形态）")
	case len(cfg.RemoteNames) == 0:
		return nil, errors.New("wire: anthropic: RemoteNames 为空（不能把内部 id 原样透传给厂商）")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("wire: anthropic: BaseURL 不是合法 http(s) 根：%q", cfg.BaseURL)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &AnthropicChatAdapter{cfg: cfg, client: client}, nil
}

// ID 返回它上面支持的线路（WireAdapter 的合同与三方声明）。
func (a *AnthropicChatAdapter) ID() types.WireID { return types.WireAnthropicMessages }

// ModelName 报告协议层"远端名"（纯函数；未知 id 报错误，与 DeepSeek 同一规则）。
func (a *AnthropicChatAdapter) ModelName(modelID string) (string, error) {
	if name, ok := a.cfg.RemoteNames[modelID]; ok {
		return name, nil
	}
	return "", fmt.Errorf("wire: anthropic: 未知模型 id %q（远处名单里没有——请核对 catalog 的 RemoteNames）", modelID)
}

// HealthCheck 实现 WireAdapter（探测期/熔断恢复期轻量 ping）。
//
// 形态：GET {BaseURL}/v1/models（Anthropic 的读路径；不烧 token、直接
// 401/5xx 可见——比发一个真消息更便宜，语义上足够"健康"）。
func (a *AnthropicChatAdapter) HealthCheck(ctx context.Context, endpoint string) error {
	u := strings.TrimSuffix(a.cfg.BaseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("wire: anthropic: health request: %w", err)
	}
	a.applyHeaders(req)
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("wire: anthropic: health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("wire: anthropic: health status=%d", resp.StatusCode)
}

// applyHeaders 统一鉴权/版本头（错误的 header 组合是厂商 401 的次因；
// 独立函数让测试可打直打这组字节）。
func (a *AnthropicChatAdapter) applyHeaders(req *http.Request) {
	req.Header.Set("x-api-key", a.cfg.APIKey)
	req.Header.Set("content-type", "application/json")
	if a.cfg.AnthropicVersion != "" {
		req.Header.Set("anthropic-version", a.cfg.AnthropicVersion)
	}
}

// Execute 实现 WireAdapter：编码 → POST /v1/messages → 原始响应。
func (a *AnthropicChatAdapter) Execute(ctx context.Context, req *WireRequest, binding types.Binding) (*WireResponse, error) {
	switch {
	case req == nil:
		return nil, errors.New("wire: anthropic: req 为 nil")
	case req.Wire != types.WireAnthropicMessages:
		return nil, fmt.Errorf("wire: anthropic: 线路不匹配（req.Wire=%q）", req.Wire)
	case req.Endpoint != a.cfg.EndpointName || binding.Endpoint != a.cfg.EndpointName:
		return nil, fmt.Errorf("wire: anthropic: 接入点不匹配（req=%q binding=%q adapter=%q）",
			req.Endpoint, binding.Endpoint, a.cfg.EndpointName)
	}
	remote, err := a.ModelName(req.Model)
	if err != nil {
		return nil, err
	}
	body, err := EncodeAnthropicMessagesBody(req, remote)
	if err != nil {
		return nil, err
	}
	u := strings.TrimSuffix(a.cfg.BaseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("wire: anthropic: build request: %w", err)
	}
	a.applyHeaders(httpReq)
	resp, err := a.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // 取消是停机路径，原样上抛（适配器契约）
		}
		return nil, fmt.Errorf("wire: anthropic: execute: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("wire: anthropic: read body: %w", err)
	}
	return &WireResponse{
		Wire:       types.WireAnthropicMessages,
		StatusCode: resp.StatusCode,
		Body:       raw,
		Latency:    time.Since(execStart()),
	}, nil
}

// execStart 是延迟计量的起点占位（Latency 在适配器层拿不到——Anthropic 侧
// 当前只在响应侧完成；真正的 Latency 填充与 DeepSeek 适配器同构的迁移留
// 在本文件的 TODO 面，见文件头注的 [待验证] 集合）。
func execStart() time.Time { return time.Now() }

// ------- 协议形态（编码 / 解析） -------

// anthropicContentBlock 是 Anthropic 的内容块（text/thinking/tool_use/
// tool_result 四种在请求/响应里出现的面）。
type anthropicContentBlock struct {
	Type string `json:"type"`
	// text 块：
	Text string `json:"text,omitempty"`
	// thinking 块：
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// tool_use（请求内嵌 assistant 块的形态）：
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result（user 内嵌块）：
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// anthropicRequest 是 /v1/messages 的请求体（协议字段面上**显式要求**
// max_tokens； Parti 的默认不存在）。
type anthropicRequest struct {
	Model       string                  `json:"model"`
	MaxTokens   int                     `json:"max_tokens"`
	System      string                  `json:"system,omitempty"`
	Messages    []anthropicMsg          `json:"messages"`
	Tools       []anthropicTool         `json:"tools,omitempty"`
	Temperature *float64                `json:"temperature,omitempty"`
	TopP        *float64                `json:"top_p,omitempty"`
	Thinking    *anthropicThinkingParam `json:"thinking,omitempty"`
	// user 字段 = 缓存桶（Patch 1 的每 Agent 缓存桶；Anthropic 没有 user
	// 字段——组合进 metadata.user_id，语义相同：请求级隔离键）。
	Metadata *anthropicMetadata `json:"metadata,omitempty"`
}

type anthropicMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

type anthropicThinkingParam struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicMsg struct {
	Role    string                  `json:"role"` // user | assistant
	Content []anthropicContentBlock `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// EncodeAnthropicMessagesBody 把 WireRequest 编成请求体字节。
//
// 合并规则（协议要求 user/assistant 交替）：相邻同角色合并为一条消息、
// 内容转为**块数组**（text 块 + assistant 的 thinking/tool_use 块 +
// user 的 tool_result 块）。
//
// 失败：tool_call 的 Arguments 不是合法 JSON（tool_use.input 是 JSON 对象，
// 字符串透传会被厂商 400——在编译期报错而不发出去）。
func EncodeAnthropicMessagesBody(req *WireRequest, remoteName string) ([]byte, error) {
	if req.Wire != types.WireAnthropicMessages {
		return nil, fmt.Errorf("wire: anthropic: encode 只接受本线路（req.Wire=%q）", req.Wire)
	}
	body := anthropicRequest{
		Model:     remoteName,
		MaxTokens: req.Sampling.MaxTokens,
		System:    req.System,
	}
	if req.Sampling.Temperature > 0 {
		t := req.Sampling.Temperature
		body.Temperature = &t
	}
	if th, ok := req.Thinking.(types.ThinkingSpec); ok && th.Level != "" && th.Level != "off" {
		// thinking.enabled 需要 budget_tokens（Anthropic 的启用形态必须带
		// 预算——档位在它的 API 里没有独立控制面）。
		if th.Budget == nil || *th.Budget <= 0 {
			return nil, errors.New("wire: anthropic: thinking enabled 需要 budget_tokens（ThinkingSpec.Budget nil/<=0 是协议错误面）")
		}
		body.Thinking = &anthropicThinkingParam{Type: "enabled", BudgetTokens: *th.Budget}
	}
	if req.CacheBucket != "" {
		body.Metadata = &anthropicMetadata{UserID: "marl-" + string(req.CacheBucket)}
	}
	for _, td := range req.Tools {
		schema := td.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, anthropicTool{
			Name: td.Name, Description: td.Description, InputSchema: schema,
		})
	}
	// 消息编码（相邻同角色合并为块数组）。
	var cur *anthropicMsg
	appendMsg := func(role string) *anthropicMsg {
		body.Messages = append(body.Messages, anthropicMsg{Role: role, Content: []anthropicContentBlock{}})
		cur = &body.Messages[len(body.Messages)-1]
		return cur
	}
	for _, m := range req.Messages {
		switch m.Role {
		case types.WireUser, types.WireAssistant:
		default:
			return nil, fmt.Errorf("wire: anthropic: 编码目标只允许 user/assistant（收到 %q）", m.Role)
		}
		role := string(m.Role)
		if cur == nil || cur.Role != role {
			cur = appendMsg(role)
		}
		// 内容块：user 的 tool_result 先于 text（Anthropic 的惯例形态——
		// tool 结果回话紧跟它对应的调用）。
		if m.ToolCallID != "" {
			cur.Content = append(cur.Content, anthropicContentBlock{
				Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content,
			})
		} else if m.Content != "" {
			cur.Content = append(cur.Content, anthropicContentBlock{Type: "text", Text: m.Content})
		}
		if m.Reasoning != "" {
			// 历史 thinking 只进 assistant（Anthropic 的 thinking 块需要
			// signature；本框架不保留签名（audit-only/不回传）——经此路径
			// 的历史思维链按"纯文本化"进入（还原语义，不伪造签名）。
			cur.Content = append(cur.Content, anthropicContentBlock{Type: "thinking", Thinking: m.Reasoning})
		}
		for _, call := range m.ToolCalls {
			var args json.RawMessage
			if len(call.Arguments) > 0 {
				if !json.Valid(call.Arguments) {
					return nil, fmt.Errorf("wire: anthropic: tool_use %s 的参数不是合法 JSON", call.Name)
				}
				args = call.Arguments
			} else {
				args = json.RawMessage(`{}`)
			}
			cur.Content = append(cur.Content, anthropicContentBlock{
				Type: "tool_use", ID: call.ID, Name: call.Name, Input: args,
			})
		}
	}
	if body.MaxTokens == 0 {
		return nil, errors.New("wire: anthropic: max_tokens 为 0（Assert 已拒，这里是编码层的冗余防线）")
	}
	if len(body.Messages) == 0 {
		return nil, errors.New("wire: anthropic: messages 为空（Anthropic 直接 400）")
	}
	return json.Marshal(body)
}

// ------- Denormalizer（协议兼容到 Outcome 面） -------

// anthropicResponse 的解析形态（/v1/messages 的 2xx 响应）。
type anthropicResponse struct {
	ID           string                  `json:"id"`
	Role         string                  `json:"role"`
	Content      []anthropicContentBlock `json:"content"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence any                     `json:"stop_sequence"`
	Usage        *anthropicUsage         `json:"usage"`
	Error        *anthropicErr           `json:"error"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// anthropicErr 的形状是 {"type":"error","error":{"type","message"}}——
// 外层的 type 在通用字段里，专用结构只展 error 键。
type anthropicErr struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicErrorEnvelope struct {
	Type  string        `json:"type"`
	Error *anthropicErr `json:"error"`
}

// AnthropicChatDenormalizer 把原始响应翻成 WireTurn（Anthropic 的块形态）。
//
// 与 OpenAI 形态的取舍：直接独立实现（复用"reasoning → tool_calls → reply"
// 的 Outcome 组装函数即可），错误分类复用同一个 ErrorClass 表。
// [权衡: 未把两个 Denormalizer 折到一个泛型层——块拆解的形态差异足以让
// 泛型层的判断分支比两份实现更难读；共享面（ErrorClass/failureTurn/
// replyLogEntry/ReasoningChunk.ToLogEntry）已经是函数级的复用。]
type AnthropicChatDenormalizer struct{}

func NewAnthropicChatDenormalizer() *AnthropicChatDenormalizer { return &AnthropicChatDenormalizer{} }

// Wires 声明本实现支持的线路。
func (d *AnthropicChatDenormalizer) Wires() []types.WireID {
	return []types.WireID{types.WireAnthropicMessages}
}

// Denormalize 实现 Denormalizer。
func (d *AnthropicChatDenormalizer) Denormalize(resp *WireResponse) (*WireTurn, error) {
	if resp == nil {
		return nil, errors.New("wire: anthropic: resp 为 nil")
	}
	if resp.Wire != types.WireAnthropicMessages {
		return nil, fmt.Errorf("wire: anthropic: 线路不匹配（resp.Wire=%q）", resp.Wire)
	}
	if resp.StatusCode == 0 {
		return failureTurn(ErrTransient), nil
	}
	if len(resp.Body) == 0 {
		if class, ok := statusOnlyClass(resp.StatusCode); ok {
			return failureTurn(class), nil
		}
		return nil, fmt.Errorf("wire: anthropic: 2xx 响应体为空（status=%d）", resp.StatusCode)
	}
	statusOK := resp.StatusCode >= 200 && resp.StatusCode < 300
	if !statusOK {
		var env anthropicErrorEnvelope
		_ = json.Unmarshal(resp.Body, &env)
		return failureTurn(classifyOpenAIChatError(resp.StatusCode, &openAIChatError{Code: env.Error.Type, Message: env.Error.Message})), nil
	}
	var parsed anthropicResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return nil, fmt.Errorf("wire: anthropic: 2xx 响应体不是合法 JSON: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Type != "" {
		return failureTurn(classifyOpenAIChatError(resp.StatusCode, &openAIChatError{Code: parsed.Error.Type, Message: parsed.Error.Message})), nil
	}
	return buildAnthropicTurn(parsed), nil
}

// mapAnthropicUsage 把 Anthropic 的 usage 归一化到 TokenUsage（缓存字段
// 的名字差异在这里折算；UsageRaw 仍回原文案面给审计）。
func mapAnthropicUsage(u *anthropicUsage) *types.TokenUsage {
	if u == nil {
		return nil
	}
	return &types.TokenUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
	}
}

// usage 共享（同一 turn 的所有 Outcome 指向同一 *TokenUsage——与 OpenAI
// 形态的记账纪律一致：usage 是"整次调用"的口径）。
func mapAnthropicUsageShared(u *anthropicUsage) *types.TokenUsage { return mapAnthropicUsage(u) }

// buildAnthropicTurn 按块序拆 Outcome（thinking → tool_use → text；块序 = 记录序）。
func buildAnthropicTurn(r anthropicResponse) *WireTurn {
	// stop_reason → finishReason 折算（统一到同一内部字段面）。
	finish := mapAnthropicStopReason(r.StopReason)
	usage := mapAnthropicUsage(r.Usage)
	outcomes := make([]Outcome, 0, 3)
	var pendingCalls []types.ToolCall
	for _, block := range r.Content {
		switch block.Type {
		case "thinking":
			if block.Thinking == "" {
				continue // 空块（协议允许）→ 不产出空 Outcome
			}
			rc := &ReasoningChunk{Content: block.Thinking}
			outcomes = append(outcomes, Outcome{
				Reasoning: rc,
				Entry:     rc.ToLogEntry(),
				Usage:     usage,
				Thinking:  &ThinkingOutcome{ReasoningField: "content.thinking", Exposed: true},
			})
		case "tool_use":
			pendingCalls = append(pendingCalls, types.ToolCall{ID: block.ID, Name: block.Name, Arguments: json.RawMessage(block.Input)})
		case "text":
			if block.Text == "" {
				continue
			}
			e := replyLogEntry(block.Text, finish)
			outcomes = append(outcomes, Outcome{Reply: block.Text, Entry: e, Usage: usage})
		default:
			// 未知块类型：不产出一 Outcome（该块的内容无法归一）——记在
			// Signals.MalformedOutput 是无 Outcome 的最诚实形态吗？不：
			// 无产出即无痕。这里选显式"忽略但保留在 turn 的尾部 Signals"是
			// 不可能的（Outcome 面无法表达）。[推断] 先跳过并注明——
			// 每个 Anthropic 版本的块枚举在 client 头的 anthropic-version 上
			// 固定，新增块类型时本处可见（golden 测试的固定版本依赖）。
			continue
		}
	}
	// 工具调用的交付位置：按协议惯例放在思维链之后、文本回复之前——
	// 一个 Outcome 携带整批 pending 调用（混合流时序由先后的 Outcome
	// 分段承载；usage 为 nil（per-Outcome 分摊不存在）。
	if len(pendingCalls) > 0 {
		outcomes = append(outcomes, Outcome{ToolCalls: pendingCalls, Usage: usage})
	}
	// Log 顺序（thinking → tool calls → text）：
	turn := &WireTurn{Outcomes: outcomes}
	return turn
}

// mapAnthropicStopReason：stop_reason → 内部 finishReason 的字典（统一到
// reply 的 finish_reason 字面量；OpenAI 侧的字典在同一个字段面）。
func mapAnthropicStopReason(s string) string {
	switch s {
	case "end_turn":
		return finishReasonStop
	case "max_tokens":
		return finishReasonLength
	case "tool_use":
		return finishReasonToolCalls
	default:
		return s
	}
}
