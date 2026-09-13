package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

// ErrorClass 是归一化后的错误分类（Part 10.11）。
// 升级与压缩触发都消费它；若留在厂商形态，上层会长满 if provider == "deepseek"。
//
// 零值契约（本包唯一的例外，务必看清）：零值 ErrorClass("") **就是**
// ErrNone——因为"没有错误"需要一个正常值来表达，这与其他枚举"零值非法"的
// 结论相反。由此带来两个必须遵守的纪律：
//   - 需要判断"这次调用失败了吗"时用 IsError()，不要用 c != "" 之外的花招，
//     更不要用 !Valid()（Valid 对 ErrNone 返回 true）；
//   - 消费侧**不得**把"零值"当作"未赋值"来兜底处理（例如"没分类就按
//     transient 重试"）。分不到位就该留空，让上层看到"未分类"这个事实。
type ErrorClass string

const (
	ErrNone            ErrorClass = ""
	ErrTransient       ErrorClass = "transient"           // 429/503/超时 → Pool 退避重试，不计入升级证据
	ErrContextOverflow ErrorClass = "context_overflow"    // → 触发压缩，不是升级
	ErrCapability      ErrorClass = "capability_rejected" // → 修正本地能力覆盖表 + 告警，不盲重试
	ErrAuthQuota       ErrorClass = "auth_quota"          // → 标记 endpoint 不健康，熔断，跳到阶梯下一级
	ErrContentFilter   ErrorClass = "content_filter"      // → 交回 LLM 决策，记审计
	ErrMalformed       ErrorClass = "malformed_output"    // → 计入升级证据；可追加格式纠偏 Transient
	// ErrOutputTruncated 是输出预算截断（finish_reason=length + tool_call
	// 参数非法 JSON——模型把大内容写进 arguments 被腰斩）。[阶段 12 新增/
	// 真机发现 #2/#3] 处置：格式纠偏 Transient（"单次输出拆小"）+ 一次
	// 重试；计入升级证据。与 ErrMalformed 的区别：成因是**预算**不是模型
	// 格式能力——处置指引完全不同（拆小 vs 改格式）。
	ErrOutputTruncated ErrorClass = "output_truncated"
)

// Valid 报告 c 是否为已定义分类（含 ErrNone）。
//
// 语义区分（两个都 false 的情况必须被上层看见）：
//   - Valid()==true && IsError()==false → 调用成功；
//   - Valid()==true && IsError()==true  → 已归类的失败，可按类别处置；
//   - Valid()==false                    → **未分类**：既不能重试也不能升级，
//     必须记审计并交给人看（厂商可能改了错误格式）。把它当 transient 重试
//     是这里最容易犯的错：一次永久性错误会被重试到烧完预算。
//
// 并发：纯函数。
func (c ErrorClass) Valid() bool {
	switch c {
	case ErrNone, ErrTransient, ErrContextOverflow, ErrCapability,
		ErrAuthQuota, ErrContentFilter, ErrMalformed, ErrOutputTruncated:
		return true
	}
	return false
}

// IsError 报告 c 表示一次失败（ErrNone 之外的所有已定义类别）。
//
// 未知分类（Valid()==false）返回 false：调用方必须先用 Valid() 分流，
// 不能指望 IsError() 替它做"未知 = 失败"的判断。
// 并发：纯函数。
func (c ErrorClass) IsError() bool {
	return c.Valid() && c != ErrNone
}

// ReasoningChunk 是一段连续的思维链（Patch 1：可多条，见 WireTurn）。
type ReasoningChunk struct {
	Content  string
	Duration time.Duration
}

// ToLogEntry 把思维链片段转成 Role=thinking 的 LogEntry（落 Log 形态）。
//
// 契约：
//   - 落库的 Role 恒为 types.RoleThinking，不因调用方不同而变；
//   - Content 必须与 chunk.Content 逐字节相同（思维链是要被审计的原始证据，
//     任何"整理一下再存"都会破坏它与厂商响应的对应关系）；
//   - 不写 Seq（落库时由 MessageLog.Append 分配，见 store.MessageLog 契约）；
//   - 接收者为 nil 时返回零值 LogEntry 而不是 panic：不存在"无思维链的
//     思维条目"，把这条边界显式化，比让调用方自己去判 nil 更不容易漏。
//
// 阶段 1 补的两点（原契约未写明，但落库必然需要）：
//   - Audience = types.AudienceAudit：历史思维链"只进审计表，不占上下文"
//     （见 Audience 的三档定义）。它同时让"思考过程可审计"与"不重复付
//     token 钱"两件事同时成立；是否**写**进 Log 由调用方按 Profile 的
//     ThinkingDisplay.LogThinking 决定（本方法不替它决定）；
//   - AgentID 保持零值：**落库前必须由调用方填入所属 Agent**。Denormalizer
//     无从知道 Agent 身份，而"猜一个 AgentID"会让血缘指向错误的 Agent——
//     比留空危险得多（留空会立刻被 MessageLog.Append 拦住，猜错则静默污染）。
//
// 并发：纯函数（不修改接收者）。
func (r *ReasoningChunk) ToLogEntry() types.LogEntry {
	if r == nil {
		return types.LogEntry{}
	}
	return types.LogEntry{
		Content: r.Content,
		Role:    types.RoleThinking,
		Prov:    types.ProvOriginal,
		// 思维链不参与上下文：它的价值在审计与调试，而"每轮都回传历史思维链"
		// 会让输入 token 翻倍（DeepSeek 只在携带 tools 时才要求回传，见
		// docs/deepseek-api/thinking.html —— 这是编译层的策略，不是本方法的事）。
		Audience: types.AudienceAudit,
	}
}

// ThinkingOutcome 是 thinking 模式的实测落点（Patch 1）。
type ThinkingOutcome struct {
	ActualLevel    string // 实际生效的 thinking 档位（含 Normalizer 降级后的值）；与阶梯 RungID 无关
	ReasoningField string // 响应字段名（如 "reasoning_content" / "thinking"）
	Exposed        bool   // 是否落 Log / 可见
}

// OutcomeSignals 是给框架的结构化信号（Part 10.11）。
// 升级判据只读 Signals，不读厂商响应——这样"证据触发升级"才真的和厂商解耦。
//
// 零值契约：OutcomeSignals{} 是**合法**的，含义是"本轮无异常信号"
// （成功、非畸形、无工具错误、有变更）。这与"信号缺失"不可区分——
// 因此 Denormalize 在无法判定时必须显式填写（如把 MalformedOutput 设为
// true 或给出 ToolErrorKind），不能让"没填"和"没问题"混为一谈。
//
// ToolErrorKind 的取值必须来自技能层的错误码常量（如 skill 包的
// path_not_found），不得在此自由发挥字符串：升级判据要按它做统计，
// 拼写漂移会让统计静默丢样本。
type OutcomeSignals struct {
	ErrorClass      ErrorClass
	MalformedOutput bool
	ToolErrorKind   string // 如 "path_not_found" / "syntax_error"（取自技能层错误码）
	NoMutationTurn  bool   // 这一轮无 mutating 技能调用成功
}

// Outcome 是一次 Wire.Execute 中的一段产出（Part 10.11 / 10.16）。
// 一条响应里可能有多个 Outcome（reasoning / tool_call / reply 混合流）。
//
// 不变量：Reasoning / Reply / ToolCalls 至少有一项非空。三项全空的 Outcome
// 是纯噪音——它会进入 Log 与上下文、占用 token、让"本轮有几个产出"的计数
// 失真，却没有任何信息。Denormalizer 必须丢弃这种片段而不是照样产出。
//
// [契约澄清（阶段 1 补，见 ADR-0019）: 上面这条不变量约束的是**成功**产出。
// 失败产出是唯一的例外形态——三项皆空、信息全在 Signals.ErrorClass：
// 厂商报错时没有任何内容可产出，而"为了满足不变量往 Reply 里塞错误文本"
// 会让主循环把厂商报错当成"模型说的话"写进 Log 并回灌上下文。
// 因此消费侧判定"这一段有没有内容"时必须看 Signals.ErrorClass.IsError()，
// 不能只看三项是否为空。]
//
// Usage 的口径与 WireResponse.UsageRaw 一致：nil = 未知（失败或厂商未回传），
// **不等于 0**。记账侧不得用零值冒充（见 LedgerEntry）。
//
// 同一 turn 的多个 Outcome 共享**同一个** *TokenUsage（usage 是"整次调用"的
// 口径，不是"这一段"的口径）。消费侧必须按 turn 取一次（任取一个非 nil），
// 跨 Outcome 累加会把一次调用记成三次——这是阶段 1 明确写下的记账纪律，
// 因为 Outcome 列表里看不出来它们是同一次调用。
type Outcome struct {
	Reasoning *ReasoningChunk // 思维链片段（denormalize 时拆出）
	Reply     string          // 可见文本回复
	Entry     types.LogEntry  // 产出的 LogEntry（主循环统一落 Log）
	ToolCalls []types.ToolCall
	Usage     *types.TokenUsage // nil 表示用量未知（≠0）
	Signals   OutcomeSignals
	Thinking  *ThinkingOutcome
}

// WireTurn 是一次 Wire.Execute 的全部产出（Patch 1，Part 10.16）。
// 旧设计"一次 Execute 返回一个 Outcome"装不下混合流形态。
type WireTurn struct {
	Outcomes []Outcome
}

// Ready 判断这一 turn 是否已具备进入下一轮的条件（有可见回复或工具调用）。
//
// 关键边界：**只有思维链、没有回复也没有工具调用 → 不算 ready**。
// 这正是本方法存在的理由：thinking-only 的响应如果被当成"完成一轮"，
// 主循环会在没有产出任何动作的情况下空转（每轮都在想、什么都不做），
// 表现为 Agent 静默烧预算。不 ready 时的正确处置是继续同一轮
// （或在 MaxRetries 用尽后计入升级证据），而不是推进轮次。
//
// nil 接收者返回 false（没有产出即未就绪）。
// 并发：纯函数。
func (t *WireTurn) Ready() bool {
	if t == nil {
		return false
	}
	for _, o := range t.Outcomes {
		if o.Reply != "" || len(o.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// Denormalizer 把厂商响应翻译成归一化信号（Part 10.11，回程比去程更值钱）。
type Denormalizer interface {
	// Wires 声明本实现支持的线路（返回**副本**：调用方可能拼接/排序，
	// 共享可变切片会静默改掉全局声明）。
	Wires() []types.WireID

	// Denormalize 拆出 reasoning / tool_call / reply，归一化 usage 与错误分类。
	//
	// 契约：
	//   - 纯函数：不得修改 resp（原始响应体是审计证据，落库时还要用）；
	//   - 厂商错误不进 error 返回值，而是分类进 OutcomeSignals.ErrorClass
	//     （见 ErrorClass 零值契约）：上层要按类别分流处置，包装成 error 字符串
	//     会让"该重试/该压缩/该熔断"的判断退化成字符串匹配；
	//   - 返回 error 只用于"**拿不出可分类内容**"：2xx 响应体为空 / 结构不合法 /
	//     协议字段完全缺失，且没有可分类的失败语义。反之：
	//     · 未收到 HTTP 响应（StatusCode==0）不算解析失败——按 transient 分类进
	//     ErrorClass；
	//     · 非 2xx 时状态码本身就足以定类（错误体可能缺失或被网关替换成 HTML），
	//     也走 ErrorClass。
	//     理由是重试决策只认一个来源，让调用方拿状态码再判一次就是第二套判据；
	//   - 明确的无关字段必须在**没有可产出内容**时也返回 Outcome（带上
	//     ErrorClass），不得返回"空 WireTurn + nil error"——那会让上层把
	//     一次失败当成一次成功的空响应。
	//
	// 并发：必须可并发调用（同一实现会被多 Agent 共享）。
	Denormalize(resp *WireResponse) (*WireTurn, error)
}

// ---------------------------------------------------------------------------
// 以下是阶段 1 的实现：OpenAI 兼容线路（types.WireOpenAIChat）的回程翻译。
// ---------------------------------------------------------------------------

// reasoningFieldName 是本线路承载思维链的响应字段名
// （docs/deepseek-api/thinking.html：思维链在 reasoning_content 里，与 content 同级）。
//
// 放成常量而不是散在代码里：它是 ThinkingOutcome.ReasoningField 的唯一取值，
// 而"字段名变了"是厂商协议变更的信号（reasoning_content 本身就是后加字段）。
// 集中一处让探测报告与代码引用同一个名字。
const reasoningFieldName = "reasoning_content"

// OpenAI 兼容响应的最小解析结构。
//
// 除 choices 外全部可选：厂商会在响应里加新字段（system_fingerprint、logprobs…），
// 也会在部分错误里省掉 choices。严格解析会被厂商的无害演进打死，而"字段缺失"
// 在这里由显式判断兜住（见 Denormalize 的分支）。
type openAIChatResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Object  string             `json:"object"`
	Choices []openAIChatChoice `json:"choices"`
	Usage   *openAIChatUsage   `json:"usage"`
	Error   *openAIChatError   `json:"error"`
}

type openAIChatChoice struct {
	Index        int                    `json:"index"`
	FinishReason string                 `json:"finish_reason"`
	Message      *openAIChatRespMessage `json:"message"`
}

type openAIChatRespMessage struct {
	Role             string                   `json:"role"`
	Content          string                   `json:"content"`
	ReasoningContent string                   `json:"reasoning_content"`
	ToolCalls        []openAIChatRespToolCall `json:"tool_calls"`
}

type openAIChatRespToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIChatUsage 是厂商原始 usage。
//
// 缓存命中量两种写法都解析（prompt_cache_hit_tokens 与
// prompt_tokens_details.cached_tokens）：厂商文档同时给出两个，
// 但不保证永远同时给出（见 mapOpenAIChatUsage 的取值优先级）。
type openAIChatUsage struct {
	PromptTokens            int                         `json:"prompt_tokens"`
	CompletionTokens        int                         `json:"completion_tokens"`
	TotalTokens             int                         `json:"total_tokens"`
	PromptTokensDetails     *openAIChatPromptDetail     `json:"prompt_tokens_details"`
	PromptCacheHitTokens    *int                        `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens   *int                        `json:"prompt_cache_miss_tokens"`
	CompletionTokensDetails *openAIChatCompletionDetail `json:"completion_tokens_details"`
}

type openAIChatPromptDetail struct {
	CachedTokens int `json:"cached_tokens"`
}

type openAIChatCompletionDetail struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// openAIChatError 是厂商错误体。
//
// Code 用 any 解析：厂商文档说它是 string，但历史上出现过 null。用 any 再统一
// 转文本，比写一个自定义 UnmarshalJSON 更简单，也不会因为一个字段的类型变化
// 让整个响应解析失败（那会把"能分类的错误"变成"无法解析"）。
type openAIChatError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    any    `json:"code"`
}

// text 把错误体的各字段拼成一段用于模式匹配的小写文本。
//
// 四个字段全带上而不是只匹配 message：厂商在这四个字段里放什么并不稳定，
// 只匹配 message 会漏掉"code 里写了 context_length_exceeded 但 message 是
// 一段中文说明"这类情况。
func (e *openAIChatError) text() string {
	if e == nil {
		return ""
	}
	code := ""
	switch c := e.Code.(type) {
	case string:
		code = c
	case nil:
	default:
		code = fmt.Sprint(c)
	}
	return strings.ToLower(strings.Join([]string{code, e.Type, e.Message, e.Param}, " "))
}

// OpenAICompatDenormalizer 是 OpenAI 兼容线路的回程翻译实现。
//
// 无状态：Denormalize 只读入参，可并发调用。
type OpenAICompatDenormalizer struct{}

// 编译期检查：实现必须满足接口（与 Normalizer 同一纪律）。
var _ Denormalizer = (*OpenAICompatDenormalizer)(nil)

// NewOpenAICompatDenormalizer 构造回程翻译器。
//
// 无参数：回程没有"布局选择"，只有"把厂商字节翻译成规范信号"。
// 需要策略的是去程（10.9），不是回程。
func NewOpenAICompatDenormalizer() *OpenAICompatDenormalizer {
	return &OpenAICompatDenormalizer{}
}

// Wires 声明本实现支持的线路（返回副本）。
func (d *OpenAICompatDenormalizer) Wires() []types.WireID {
	return []types.WireID{types.WireOpenAIChat}
}

// finish_reason 的已知取值。
//
// 这张表照抄官方文档的**封闭枚举**（docs/deepseek-api/chat-complete.html：
// "Possible values: [stop, length, content_filter, tool_calls,
// insufficient_system_resource, aborted]"）。写全六个而不是只写用到的四个：
// 阶段 1 的探测报告要把"厂商停止原因"逐条核对，枚举缺项会让人以为
// insufficient_system_resource 是异常值。
//
// 未识别取值**不报错**：厂商会加新值（枚举本身就在演进），而未知 reason
// 不影响本次翻译的正确性（content / tool_calls / reasoning_content 都已经
// 在响应里）。只是"截断/资源不足"这类判据会因此漏掉一次——漏掉比误判成
// 别的类别安全（见 classifyEmptyOutput）。
const (
	finishReasonStop                       = "stop"
	finishReasonLength                     = "length"
	finishReasonToolCalls                  = "tool_calls"
	finishReasonContentFilter              = "content_filter"
	finishReasonInsufficientSystemResource = "insufficient_system_resource"
	finishReasonAborted                    = "aborted"
)

// Denormalize 把一次响应翻译成 WireTurn（契约见接口注释）。
//
// 顺序：可解析性 → 失败分类 → 成功拆解（reasoning / tool_calls / reply）。
//
// 四条边界的处理依据：
//   - 未收到 HTTP 响应（StatusCode==0，body 必然为空）：**不**走 error，而是
//     按状态码分类成 transient 的失败 turn。它不属于"无法解析"——成因在
//     WireResponse 零值契约里已经写清（0 = 没收到响应），处置也明确
//     （10.11 的分类表把"超时"归 Transient）。走 error 会逼调用方再看一次
//     StatusCode 才能决定重试，而重试决策只认 ErrorClass 一个来源
//     （ADR-0016）。这也让 classifyOpenAIChatError 的 status==0 分支真正可达
//     （否则是死代码）；
//   - 非 2xx 但 body 为空 / 非法 JSON：状态码本身就是可靠信号，分类不依赖
//     体文本（网关 502 常常不带体）→ 仍然走 Signals，见 statusOnlyClass；
//   - 2xx 却拿不出可解析内容（空体 / 非法 JSON / 缺 choices / 缺 message）：
//     既没有产出也无从分类 → 走 error 通道（调用方已能从 resp.StatusCode 与
//     Body 区分成因）；
//   - 非 2xx 或带 error 体：**不**走 error，而是产出"只有 Signals 的失败
//     turn"——上层要按 ErrorClass 分流，而不是解析错误字符串；
//   - 2xx 但三项全空：JSON mode 有概率返回空 content
//     （docs/deepseek-api/json_mode.html 明写了这一点），此时按 malformed
//     上报。既不产出空 Outcome（噪音），也不返回空 turn
//     （那会被上层当成"成功的空响应"）。
func (d *OpenAICompatDenormalizer) Denormalize(resp *WireResponse) (*WireTurn, error) {
	if resp == nil {
		return nil, errors.New("wire: denormalize: resp 为 nil")
	}
	if resp.Wire != types.WireOpenAIChat {
		return nil, fmt.Errorf("wire: denormalize: 线路不匹配（resp.Wire=%q，本实现=%q）", resp.Wire, types.WireOpenAIChat)
	}
	// 顺序刻意放在"空 body"检查之前：status==0 时 body 必然为空，若先查 body
	// 就永远走不到分类表，StatusCode==0 的语义（该重试）也就落不到 ErrorClass 上。
	if resp.StatusCode == 0 {
		return failureTurn(classifyOpenAIChatError(0, nil)), nil
	}
	// 体不可解析（空体 / 非法 JSON）时**先看状态码**：非 2xx 的状态码本身就是
	// 可靠信号，分类不需要错误体文本（网关 502 常常不带体，而"厂商故障该重试"
	// 的处置不因体缺失而改变）→ 仍然走 Signals。2xx 却拿不出可解析内容，则
	// 既没有产出也无从分类 → 走 error。
	if len(resp.Body) == 0 {
		if class, ok := statusOnlyClass(resp.StatusCode); ok {
			return failureTurn(class), nil
		}
		return nil, fmt.Errorf("wire: denormalize: 2xx 响应体为空（status=%d）：无法解析", resp.StatusCode)
	}

	var parsed openAIChatResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		if class, ok := statusOnlyClass(resp.StatusCode); ok {
			return failureTurn(class), nil
		}
		return nil, fmt.Errorf("wire: denormalize: 2xx 响应体不是合法 JSON（status=%d）: %w", resp.StatusCode, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || parsed.Error != nil {
		return failureTurn(classifyOpenAIChatError(resp.StatusCode, parsed.Error)), nil
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("wire: denormalize: 2xx 响应缺少 choices（协议字段完全缺失，status=%d）", resp.StatusCode)
	}
	choice := parsed.Choices[0]
	if choice.Message == nil {
		return nil, fmt.Errorf("wire: denormalize: choices[0] 缺少 message（协议字段完全缺失，status=%d）", resp.StatusCode)
	}
	return buildOpenAIChatTurn(choice, parsed.Usage), nil
}

// buildOpenAIChatTurn 按 reasoning → tool_calls → reply 的顺序拆出 Outcome。
//
// 顺序即 Log 顺序：思维链（若有）先于它的产出，与"模型先想后说、先调工具"
// 的事实一致。这个顺序是 WireTurn 语义的一部分（同一 turn 的 Reasoning 共享
// 一个 stable 序列，见 ADR-0008），不是实现细节。
//
// usage 共享：同一 turn 的所有 Outcome 指向**同一个** *TokenUsage。usage 是
// "整次调用"的口径（厂商不按 Outcome 分摊），消费侧必须按 turn 取一次
// （见 Outcome 的记账纪律注释）。共享同一个指针而不是各自拷贝，是为了让
// "它们是同一次调用"在调试时可见（指针相等）。
func buildOpenAIChatTurn(choice openAIChatChoice, raw *openAIChatUsage) *WireTurn {
	msg := choice.Message
	usage := mapOpenAIChatUsage(raw)
	finishReason := strings.ToLower(choice.FinishReason)
	outcomes := make([]Outcome, 0, 3)

	if msg.ReasoningContent != "" {
		rc := &ReasoningChunk{Content: msg.ReasoningContent}
		outcomes = append(outcomes, Outcome{
			Reasoning: rc,
			Entry:     rc.ToLogEntry(),
			Usage:     usage,
			Thinking: &ThinkingOutcome{
				ReasoningField: reasoningFieldName,
				Exposed:        true,
				// ActualLevel 留空：它需要请求侧信息（Normalizer 降级后的档位），
				// 而 Denormalize 手上只有响应。由调用方用 NormalizeResult 的
				// Degradations 回填（见 ADR-0019）。
				ActualLevel: "",
			},
		})
	}

	if len(msg.ToolCalls) > 0 {
		calls, malformed := parseOpenAIChatToolCalls(msg.ToolCalls)
		signals := OutcomeSignals{MalformedOutput: malformed}
		if malformed {
			// [阶段 12 修正/真机发现 #2/#3] 截断分辨：finish_reason=length
			// 且 tool_call 参数非法 JSON——大概率是**输出预算截断**（模型把
			// 大内容写进 arguments，token 到量 JSON 被腰斩），不是模型的
			// "格式能力"问题。归 ErrOutputTruncated（处置：拆小单次输出后
			// 重试），不再与 ErrMalformed 混同——真机实录：该信号被折叠成
			// malformed 后按终结处置，5/13 次运行死于此。
			if finishReason == "length" {
				signals.ErrorClass = ErrOutputTruncated
			} else {
				signals.ErrorClass = ErrMalformed
			}
		}
		outcomes = append(outcomes, Outcome{
			ToolCalls: calls,
			Usage:     usage,
			Signals:   signals,
			// Entry 留空：工具调用的落 Log 形态需要 Meta（结构化调用信息）与
			// AgentID，由主循环在真正执行前构造（13.4）。这里只保证
			// ToolCalls 的语义正确与配对 id 稳定。
		})
	}

	if msg.Content != "" {
		signals := OutcomeSignals{}
		if finishReason == finishReasonContentFilter {
			// 内容被拦截但仍有可见文本：必须让上层看见，否则一段被截断的
			// 回复会被当成完整回复落进 Log 与上下文。
			signals.ErrorClass = ErrContentFilter
		}
		outcomes = append(outcomes, Outcome{
			Reply:    msg.Content,
			Entry:    replyLogEntry(msg.Content, finishReason),
			Usage:    usage,
			Signals:  signals,
			Thinking: reasoningAbsentThinking(msg.ReasoningContent == ""),
		})
	}
	return finishOpenAIChatTurn(outcomes, usage, finishReason)
}

// finishOpenAIChatTurn 处理"三项全空"的边界，否则原样返回。
//
// 三项全空（2xx 但 content/reasoning_content/tool_calls 都为空）在 DeepSeek
// 的 JSON mode 下会出现（官方文档明说"有概率返回空 content"）。它既不是
// 成功产出，也不是厂商错误，因此**必须**单独给一类，让主循环能把"空输出"
// 接到格式纠偏重试上（ErrorClass 的处置说明：malformed_output → 计入升级证据，
// 可追加格式纠偏 Transient），而不是把它当成"模型决定不说话"。
//
// 具体给哪一类由 finishReason 决定（厂商在 2xx 里说明了原因），
// 见 classifyEmptyOutput。
func finishOpenAIChatTurn(outcomes []Outcome, usage *types.TokenUsage, finishReason string) *WireTurn {
	if len(outcomes) > 0 {
		return &WireTurn{Outcomes: outcomes}
	}
	class := classifyEmptyOutput(finishReason)
	return &WireTurn{Outcomes: []Outcome{{
		Usage:   usage,
		Signals: OutcomeSignals{ErrorClass: class, MalformedOutput: class == ErrMalformed},
	}}}
}

// classifyEmptyOutput 给出"2xx 但三项全空"时的错误分类。
//
// 为什么要看 finish_reason 而不一律报 ErrMalformed：厂商在 2xx 里用
// finish_reason 说明了**为什么**没有内容，而这些原因的处置完全不同
// （官方文档的定义，docs/deepseek-api/chat-complete.html）：
//
//   - insufficient_system_resource："由于后端推理资源受限，请求被打断"
//     ——这是厂商侧的临时容量问题，正确答案是退避重试（ErrTransient），
//     而不是把它计成"模型输出坏了"的升级证据（那会让重试预算被静默消耗完，
//     并且错误地把一个正常模型判成不合格）；
//   - aborted："生成过程被中断"——文档没说中断方是谁。归 ErrTransient 是
//     刻意选的保守方向：重试有上限、代价可控，而误记升级证据会**粘住**
//     （升级判据按统计做），修不回来；
//   - content_filter："输出内容因触发过滤策略而被过滤"——内容被拦，交回
//     LLM 决策（ErrContentFilter），重试同一段内容只会再被拦一次；
//   - stop / length / tool_calls / 空串 / 未知值：厂商说"停/截断/调工具"了
//     却三项全空，这与 JSON mode 返回空 content 是同一类现象
//     （json_mode.html 明说"有概率返回空的 content"）→ ErrMalformed。
//     注意 length 刻意**不**归 ErrTransient：它的两个成因（上下文超限 vs
//     max_tokens 太小）用同一份请求重发会得到同样的截断，重试是纯浪费；
//     原始 finish_reason 已落进 Log 的 Meta，归因不缺材料。
func classifyEmptyOutput(finishReason string) ErrorClass {
	switch finishReason {
	case finishReasonContentFilter:
		return ErrContentFilter
	case finishReasonInsufficientSystemResource, finishReasonAborted:
		return ErrTransient
	case finishReasonStop, finishReasonLength, finishReasonToolCalls, "":
		return ErrMalformed
	default:
		// 未识别取值：同上按"空输出"处理，但**不**把未知原因当成资源不足去重试
		// ——厂商新加的值语义未知，重试一个未知原因可能是在放大一个永久性错误。
		return ErrMalformed
	}
}

// reasoningAbsentThinking 在"响应里没有思维链"时给出一个**事实**记录。
//
// 它挂在不带 Reasoning 的那个 Outcome 上（通常是 Reply），因此
// Exposed=false 的含义是"本次响应没有回传 reasoning_content"，**不是**
// "模型没有思考"。要判断"我请求了 thinking 却没拿到思维链"，必须结合请求侧
// （Binding.Thinking.Level + Normalizer 的 Degradations）——那是调用方的
// 信息，Denormalize 手上只有响应。
func reasoningAbsentThinking(wasAbsent bool) *ThinkingOutcome {
	if !wasAbsent {
		return nil
	}
	return &ThinkingOutcome{ReasoningField: "", Exposed: false, ActualLevel: ""}
}

// failureTurn 构造失败 turn：唯一允许"三项皆空"的形态（见 Outcome 的契约澄清）。
func failureTurn(class ErrorClass) *WireTurn {
	// Usage 留 nil：失败时厂商通常不回传 usage，而"未知"不能用零值冒充
	// （与 WireResponse.UsageRaw 同一取舍）。
	return &WireTurn{Outcomes: []Outcome{{
		Signals: OutcomeSignals{ErrorClass: class},
	}}}
}

// replyLogEntry 构造 assistant 回复的落 Log 形态。
//
// Audience = Both：回复既要进上下文（后续轮次看得到自己说过什么），
// 也要进审计表。Role = RoleAssistantReply（内部语义角色，不是线路角色——
// 线路角色由编译层再折叠）。
//
// Meta 带 finish_reason：这是"输出被截断"目前唯一可落库的信号。设计里没有
// OutcomeSignals.Truncated（阶段 1 才发现这个缺口，加字段要 ADR），而
// finish_reason 就是厂商的原始判据——把它落进 Meta，让"这次是 length 截断
// 还是正常 stop"在 Log 里可查，而不是只能翻原始响应体。
// Meta 不参与上下文渲染，因此不影响冻结前缀的字节稳定。
//
// AgentID 零值：落库前必须由调用方填入（理由同 ReasoningChunk.ToLogEntry）。
func replyLogEntry(content, finishReason string) types.LogEntry {
	e := types.LogEntry{
		Content:  content,
		Role:     types.RoleAssistantReply,
		Prov:     types.ProvOriginal,
		Audience: types.AudienceBoth,
	}
	if finishReason != "" {
		e.Meta = map[string]any{"finish_reason": finishReason}
	}
	return e
}

// synthesizeToolCallID 为厂商未给 id 的 tool_call 合成**确定性** id。
//
// 契约（ToolCall.ID 注释）：ID 为空不是错误，由本层合成。两个要求：
//   - 确定性：同一份响应解析两次必须得到同一个 id。否则"工具结果回填时
//     引用哪个 id"会随解析次数漂移，配对被破坏；
//   - 低碰撞：同一对话里多轮都可能缺失 id，若只按下标合成（synth_0、synth_1），
//     不同轮的 id 会撞车，工具结果可能配到上一轮的调用上。
//
// 实现：按 (下标, name, arguments) 的 SHA-256 前 12 位十六进制。
// 下标进 hash 是为了区分同名同参的多次调用；name/arguments 进 hash 是为了
// 区分不同轮的调用。前缀 marl_synth_ 让它一眼可辨（审计时能看出这不是
// 厂商给的 id）。
//
// [待验证: 若将来支持流式增量拼装，同一调用的 delta 会分多次到达，
// 那时必须改用"按 Delta 序号累积后合成"，否则每次 delta 都会算出新 id。]
func synthesizeToolCallID(index int, name, arguments string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", index, name, arguments)))
	return "marl_synth_" + hex.EncodeToString(sum[:6])
}

// parseOpenAIChatToolCalls 把厂商 tool_calls 翻译成 types.ToolCall。
//
// 第二个返回值报告"是否发现畸形"：畸形不丢弃调用（ToolCall 的不变量要求
// Name 非空、Arguments 是合法 JSON），而是原样保留 + 置 MalformedOutput：
//   - 丢弃会让模型以为自己调用了工具并收到了结果（它看到的是工具结果消息
//     缺失，进而重复调用或放弃）；
//   - 原样保留 + 标记，让主循环能选择"把畸形调用作为工具错误回填给模型"
//     （ErrorClass 的处置说明：malformed_output 可追加格式纠偏 Transient）。
func parseOpenAIChatToolCalls(in []openAIChatRespToolCall) ([]types.ToolCall, bool) {
	out := make([]types.ToolCall, 0, len(in))
	malformed := false
	for i, c := range in {
		name := c.Function.Name
		args := c.Function.Arguments
		if name == "" || len(args) == 0 || !json.Valid([]byte(args)) {
			malformed = true
		}
		id := c.ID
		if id == "" {
			id = synthesizeToolCallID(i, name, args)
		}
		var raw json.RawMessage
		if args != "" {
			raw = json.RawMessage(args)
		}
		out = append(out, types.ToolCall{ID: id, Name: name, Arguments: raw})
	}
	return out, malformed
}

// openAIChatErrorRule 是"错误体文本片段 → ErrorClass"的一条规则（表驱动）。
type openAIChatErrorRule struct {
	class     ErrorClass
	fragments []string
	// note 说明这条规则的依据与它的可靠程度（给下一个改这张表的人看）。
	note string
}

// openAIChatErrorRules 是错误体文本的匹配表。**顺序即优先级**，先匹配者胜。
//
// 顺序为什么重要：厂商 message 常常同时含多个关键词（例如
// "This model's maximum context length is 65536 tokens" 里既有 maximum 也
// 有 model），一旦先匹配到 capability，本该压缩的场景会去升级模型——烧钱且
// 解决不了问题。因此"上下文超限"必须排在"能力被拒"之前。
//
// 各条规则的依据与状态：
//   - context_overflow：context_length_exceeded 是 13.3 点名的归一化目标；
//     maximum context length / too long 是 OpenAI 兼容生态的通用措辞，
//     DeepSeek 的**实测原文** [待验证]——探测报告应把实测片段补进这里；
//   - content_filter：OpenAI/DeepSeek 公开词汇（sensitive / content filter）；
//   - capability：覆盖"工具 schema 被拒"（strict 模式校验失败，
//     docs/deepseek-api/tool-call.html）与"参数不被支持"两类；
//   - transient：5xx/限流文案兜底（多数情况下状态码已经足够，
//     这一条只服务于"状态码 200 但带 error 体"这类协议违规）。
var openAIChatErrorRules = []openAIChatErrorRule{
	{
		class: ErrContextOverflow,
		fragments: []string{
			"context_length_exceeded",
			"maximum context length",
			"context length",
			"too many tokens",
			"reduce the length of the messages",
		},
		note: "13.3 点名的归一化目标；措辞按 OpenAI 兼容生态的通用写法，DeepSeek 实测原文待补",
	},
	{
		class: ErrContentFilter,
		fragments: []string{
			"content_filter",
			"content filter",
			"content policy",
			"sensitive",
		},
		note: "内容审核：处置是交回 LLM 决策 + 记审计，不该重试",
	},
	{
		class: ErrCapability,
		fragments: []string{
			"schema",
			"function",
			"tool",
			"unsupported",
			"not support",
			"does not exist",
			"invalid_parameter",
			"response_format",
			"reasoning_effort",
			"thinking",
		},
		note: "schema/工具被拒与参数不被支持：处置是修正本地能力表 + 告警，不盲重试",
	},
	{
		class: ErrTransient,
		fragments: []string{
			"rate limit",
			"rate_limit",
			"overload",
			"timeout",
			"timed out",
			"temporarily",
			"server_error",
			"service_unavailable",
		},
		note: "限流/过载/瞬时故障：可重试（Pool 侧退避）",
	},
}

// matchErrorRule 按优先级在错误文本里找第一条命中的规则。
func matchErrorRule(text string) (ErrorClass, bool) {
	if text == "" {
		return ErrNone, false
	}
	for _, r := range openAIChatErrorRules {
		for _, f := range r.fragments {
			if strings.Contains(text, f) {
				return r.class, true
			}
		}
	}
	return ErrNone, false
}

// classifyOpenAIChatError 把厂商错误归一化成 ErrorClass（Part 10.11）。
//
// 两层判断：先按 HTTP 状态码分流，再用错误体文本**细化**语义含混的状态码
// （400/422）。状态码是可靠信号、文本是不可靠信号，所以文本只能细化不能覆盖：
// 429 即使文本里出现 schema 也仍然算 transient（限流是主因）。
//
// [权衡: 未归类的 4xx（404/405 及其它）一律归 ErrCapability。这**是**一处
// 近似——它们的真实语义多是"框架/配置错误"（路径写错、方法不对），而
// ErrCapability 的处置（修正本地配置 + 告警，不盲重试）在行为上等价。
// 为什么不"留空表示未分类"：ErrorClass 的零值就是 ErrNone（成功），
// 留空会让一次失败被上层读成成功（见 ErrorClass 零值契约）。正解是加一个
// ErrFramework 之类的类别，那需要 ADR；阶段 1 先用最接近的类别并在此记录。]
func classifyOpenAIChatError(status int, e *openAIChatError) ErrorClass {
	text := e.text()

	switch {
	case status == 0:
		// 未收到 HTTP 响应：连接失败/超时。ctx 取消不走这里——适配器必须
		// 原样返回 ctx.Err()（见 WireAdapter.Execute 契约），否则"上游取消了
		// 调用"会被误当成"厂商故障"，污染熔断计数与升级证据。
		return ErrTransient
	case status == 401 || status == 402 || status == 403:
		return ErrAuthQuota
	case status == 413:
		return ErrContextOverflow
	case status == 429:
		return ErrTransient
	case status >= 500:
		return ErrTransient
	case status == 400 || status == 422:
		if c, ok := matchErrorRule(text); ok {
			return c
		}
		// 参数/能力被拒（含 schema 不合规）：不重试，人工修配置。
		return ErrCapability
	case status >= 400:
		if c, ok := matchErrorRule(text); ok {
			return c
		}
		return ErrCapability
	}
	// 2xx 却带 error 体：协议违规，按文本判；判不出按 capability（不重试）。
	if c, ok := matchErrorRule(text); ok {
		return c
	}
	return ErrCapability
}

// statusOnlyClass 给出"仅凭 HTTP 状态码"就能定的错误类别。
//
// ok=false 表示状态码本身不含失败语义（2xx）：那种情况必须靠可解析的响应体
// 才能定类，体不可解析时只能走 error 通道。
//
// 为什么需要它：错误体是**不可靠**输入（网关会插 HTML、厂商会改字段），而
// 状态码是可靠输入。体缺失/不可解析时若直接报"无法解析"，一次明确的
// 502/503/429 就被降级成"我不知道发生了什么"，调用方只能回头再解析状态码——
// 这正是 ADR-0016 要避免的第二套判据。
func statusOnlyClass(status int) (ErrorClass, bool) {
	if status < 200 || status >= 300 {
		return classifyOpenAIChatError(status, nil), true
	}
	return ErrNone, false
}

// mapOpenAIChatUsage 把厂商 usage 翻译成 TokenUsage。
//
// 口径（docs/deepseek-api/chat-complete.html + cache.html，探测用例 1/2/6 复核）：
//
//   - prompt_tokens = prompt_cache_hit_tokens + prompt_cache_miss_tokens，
//     也就是 **prompt_tokens 包含命中缓存的 token**。因此本框架的语义是
//     "PromptTokens = 输入总量（含命中），CacheReadTokens ⊆ PromptTokens"，
//     计费公式必须是
//     (PromptTokens-CacheReadTokens)*InPerMTok + CacheReadTokens*CachedInPerMTok。
//     若把 CacheReadTokens 再加到 PromptTokens 上就会重复计费
//     （types.TokenUsage 的口径注释已按本结论改成实测钉死的表述）。
//   - 命中量的两个来源（prompt_cache_hit_tokens 与 prompt_tokens_details.
//     cached_tokens）按"顶层优先"取值：顶层字段是厂商文档里明确描述缓存语义
//     的那个，details 是 OpenAI 兼容的通用包装。
//   - CacheWriteTokens 恒为 0：DeepSeek 的隐式缓存不单独上报/计费写入量。
//     这与"未统计"不可区分，是 TokenUsage 的已知局限（阶段 1 记录在案）。
//   - ImageTokens 恒为 0：阶段 1 无图片输入（10.14 在阶段 2 落地）。
//
// 负数是厂商侧的异常：TokenUsage 要求非负，原样带出会让 Ledger 出现负成本
// 并掩盖真实花费，因此逐项截断到 0——截断是错的，但比"负账单"可诊断
// （报告里会打印原始 usage，异常在报告里能一眼看到）。
func mapOpenAIChatUsage(u *openAIChatUsage) *types.TokenUsage {
	if u == nil {
		return nil
	}
	hit := 0
	switch {
	case u.PromptCacheHitTokens != nil:
		hit = *u.PromptCacheHitTokens
	case u.PromptTokensDetails != nil:
		hit = u.PromptTokensDetails.CachedTokens
	}
	prompt := u.PromptTokens
	if prompt == 0 && u.PromptCacheMissTokens != nil {
		// prompt_tokens 缺失但 hit/miss 齐全时自己求和：这两个字段是
		// "缓存视角"的口径，同样能还原输入总量。
		prompt = hit + *u.PromptCacheMissTokens
	}
	reasoning := 0
	if u.CompletionTokensDetails != nil {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return &types.TokenUsage{
		PromptTokens:     nonNegative(prompt),
		CompletionTokens: nonNegative(u.CompletionTokens),
		ReasoningTokens:  nonNegative(reasoning),
		CacheWriteTokens: 0,
		CacheReadTokens:  nonNegative(hit),
		ImageTokens:      0,
	}
}

// nonNegative 把厂商异常上报的负数截断为 0（理由见 mapOpenAIChatUsage）。
func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
