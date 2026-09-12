package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"marl/internal/types"
)

// 本文件是**唯一**碰 HTTP 与厂商 JSON 的地方（WireAdapter 的职责边界）。
//
// 命名说明：文件叫 deepseek_chat.go 是按设计文档 13.3 的文件清单来的（阶段 1
// 唯一落地的厂商）。但里面除了"端点/密钥/模型名映射"这些 DeepSeek 专属的装配
// 参数，其余（请求体编码 EncodeOpenAIChatBody、错误语义）都是 **openai_chat
// 线路**的，与厂商无关。等第二家 OpenAI 兼容厂商落地时，把线路部分拆到
// openai_chat.go、把厂商参数留给各自的适配器即可——现在拆只会多一层空结构，
// 不会带来任何可验证的好处。

const (
	// chatCompletionsPath 是本线路的调用路径。BaseURL 里带不带 /v1 由调用方
	// 决定：设计文档 13.3 写的是 https://api.deepseek.com/v1/...，官方文档
	// 示例用 https://api.deepseek.com/...，两者是同一个服务的两个入口
	// （/v1 是 OpenAI 兼容别名）。适配器不替调用方判断版本片段，只负责拼接。
	chatCompletionsPath = "/chat/completions"

	// modelsPath 用于 HealthCheck：GET /models 是 OpenAI 兼容线路的轻量探测。
	modelsPath = "/models"

	// DefaultBucketField 是缓存桶落到请求体的字段名。
	//
	// [文档冲突（阶段 1 发现，写入探测报告）: 设计文档 10.15 / 13.3 要求
	// "请求带 user=<binding.CacheBucket>"（user 是 OpenAI 的字段名），而官方
	// /chat/completions 参数表里那个"可用于 KVCache 缓存隔离"的字段叫
	// user_id。两者是**同一个语义、不同的字段名**，且当前文档只列了 user_id
	// ——照设计文档发 user 有被静默忽略的风险（桶不生效 = 缓存串味，且没有
	// 任何报错）。因此默认值取官方文档的名字，并保留 legacyBucketField 供
	// 探测验证旧写法是否仍被接受。]
	DefaultBucketField = "user_id"

	// legacyBucketField 是设计文档写的字段名（OpenAI 的 user）。
	legacyBucketField = "user"

	// maxResponseBytes 是单次响应体的上限（32 MiB）。
	// 没有上限的 io.ReadAll 等价于"把内存交给对端决定"：一个出错返回巨大文件
	// （或恶意网关）的端点会让进程 OOM，而这类故障发生时我们最需要的恰恰是
	// 进程还活着、能把它记下来。
	maxResponseBytes = 32 << 20

	// healthCheckTimeout 是 HealthCheck 的自有超时。
	// 与调用路径不同（那里超时来自 SamplingParams.TimeoutMs），健康探测的
	// 调用方可能没带 deadline；一次挂死的探测会让熔断恢复流程永远卡住。
	healthCheckTimeout = 10 * time.Second
)

// ErrCallTimeout 表示"适配器按 SamplingParams.TimeoutMs 主动中止了本次调用"。
//
// 与 caller 的 ctx 取消严格区分：ctx 取消属于正常停机路径（原样返回
// ctx.Err()），本错误属于"配置的超时太短或厂商太慢"——它可重试，且是调优
// TimeoutMs 的依据。混同两者会让"上游主动停止"被计成厂商故障。
var ErrCallTimeout = errors.New("wire: deepseek_chat: 单次调用超时")

// DeepSeekChatConfig 是适配器的装配参数（阶段 1：手工构造）。
//
// 阶段 1 不做配置层（13.3 的"不做"清单：直接硬编码调 deepseek-main），
// 所以这里是显式参数而不是从 endpoints.yaml / models.yaml 加载。字段与
// wire.EndpointConfig / wire.ModelEntry 的对应关系写在各自注释里——配置层
// 落地时由它构造本结构，而不是让本结构长成第二个配置模型。
type DeepSeekChatConfig struct {
	// EndpointName 是接入点名（EndpointConfig.Name）。
	//
	// 用途有二：Execute 校验 req.Endpoint 与 binding.Endpoint 都指向本适配器
	// 服务的接入点（用 A 端点的密钥把请求发到 B 端点是静默的凭据错配——
	// 能跑通、能计费，但账单与隔离都落在错的账号上），以及 HealthCheck
	// 认出"这个 endpoint 归我管"。
	EndpointName string

	// BaseURL 是接入点根地址，如 https://api.deepseek.com/v1。
	BaseURL string

	// APIKey 是明文密钥。阶段 1 的唯一来源是环境变量；配置层落地后必须走
	// EndpointConfig.KeyRef 的 "env:NAME" 引用，**绝不允许**写进 YAML——
	// 见 EndpointConfig 的契约（明文密钥进了版本控制就等于已泄露）。
	APIKey string

	// RemoteNames 是内部模型 id → 厂商模型名的映射（ModelEntry.RemoteName
	// 的去向）。必须非空：空映射会让 ModelName 对一切 modelID 报错，
	// 而不是回落成"原样透传"（见 WireAdapter.ModelName 契约）。
	RemoteNames map[string]string

	// HTTPClient 可为 nil（用内置客户端）。
	//
	// 内置客户端**不设**全局 Timeout：单次调用的超时来自
	// SamplingParams.TimeoutMs（每档位可以不同），设一个全局值会让
	// "某个 Agent 用长超时"变成不可表达的配置，并且把两类超时混成一个。
	HTTPClient *http.Client

	// BucketField 是缓存桶落到请求体的字段名；空 = DefaultBucketField。
	//
	// 存在的理由见 DefaultBucketField 的文档冲突说明：需要一个不重新编译
	// 就能验证两种写法的入口。生产路径只允许一个取值，因此只接受空串或
	// 白名单里的两个名字，其它值一律报错（拼错字段名 = 桶静默失效）。
	BucketField string
}

// DeepSeekChatAdapter 是 openai_chat 线路的 DeepSeek 适配器。
//
// 无状态（除只读的 cfg/client）：Execute 可并发调用——它在 Pool 的并发闸门
// 之下被多 Agent 共享，任何可变字段都会变成数据竞争。
type DeepSeekChatAdapter struct {
	cfg         DeepSeekChatConfig
	client      *http.Client
	bucketField string
}

// 编译期检查：实现必须满足接口（与 Normalizer / Denormalizer 同一纪律）。
var _ WireAdapter = (*DeepSeekChatAdapter)(nil)

// NewDeepSeekChatAdapter 构造适配器。
//
// 失败即启动期装配错误（13.3 的"必须在第一个任务之前炸"）：EndpointName /
// BaseURL / APIKey 为空、BaseURL 不是 http(s) URL、RemoteNames 为空、
// BucketField 不在白名单里。这些错误在运行时的表现都是"请求发出去了但结果
// 莫名不对"（打错端点、凭空多一次匿名调用、缓存桶静默失效），因此不留到运行时。
func NewDeepSeekChatAdapter(cfg DeepSeekChatConfig) (*DeepSeekChatAdapter, error) {
	if cfg.EndpointName == "" {
		return nil, errors.New("wire: deepseek_chat: EndpointName 为空（接入点名同时是缓存键与审计维度，不能省）")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("wire: deepseek_chat: BaseURL 为空（零值配置会把请求打到未定义地址）")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("wire: deepseek_chat: BaseURL=%q 不是合法 URL: %w", cfg.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("wire: deepseek_chat: BaseURL=%q 的 scheme 必须是 http/https（当前 %q）", cfg.BaseURL, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("wire: deepseek_chat: BaseURL=%q 缺少主机名", cfg.BaseURL)
	}
	if cfg.APIKey == "" {
		return nil, errors.New("wire: deepseek_chat: APIKey 为空（阶段 1 从环境变量读；配置层落地后走 KeyRef 的 env: 引用）")
	}
	if len(cfg.RemoteNames) == 0 {
		return nil, errors.New("wire: deepseek_chat: RemoteNames 为空（没有映射表就无从把内部模型 id 翻译成厂商模型名）")
	}
	bucketField := cfg.BucketField
	if bucketField == "" {
		bucketField = DefaultBucketField
	}
	if bucketField != DefaultBucketField && bucketField != legacyBucketField {
		return nil, fmt.Errorf("wire: deepseek_chat: BucketField=%q 不在白名单 {%q, %q}：字段名拼错等于缓存桶静默失效",
			bucketField, DefaultBucketField, legacyBucketField)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{} // 不设 Timeout：单次超时由 SamplingParams.TimeoutMs 决定（见字段注释）
	}
	return &DeepSeekChatAdapter{cfg: cfg, client: client, bucketField: bucketField}, nil
}

// ID 返回线路协议类型（WireAdapter 接口方法）。
//
// 必须与 Normalizer / Denormalizer 的 Wires() 声明一致：三方不一致会让请求
// 由 A 协议编码、由 B 协议解析，而两边的字段名常部分重合——"能跑但字段丢失"。
func (a *DeepSeekChatAdapter) ID() types.WireID { return types.WireOpenAIChat }

// EndpointName 返回本适配器服务的接入点名（供装配期自检与探测报告使用）。
func (a *DeepSeekChatAdapter) EndpointName() string { return a.cfg.EndpointName }

// ModelName 把内部模型 id 翻译成远端模型名（如 deepseek/chat → deepseek-flash）。
//
// 失败：modelID 为空、未在映射表里、映射到空名三种。**不回落**成默认名或原样
// 透传：调用到一个语义不同但名字相近的模型时，请求本身没有任何异常迹象，
// 只有账单和输出会变（见 WireAdapter.ModelName 契约）。
//
// 纯函数（不发网络请求、不读文件）：它会在池内热路径上被反复调用。
// 并发：纯函数。
func (a *DeepSeekChatAdapter) ModelName(modelID string) (string, error) {
	if modelID == "" {
		return "", errors.New("wire: deepseek_chat: modelID 为空（模型 id 的零值是\"未分配\"，不是某个模型）")
	}
	remote, ok := a.cfg.RemoteNames[modelID]
	if !ok {
		return "", fmt.Errorf("wire: deepseek_chat: 未声明的模型 id %q（已声明：%v）；不回落成默认名——"+
			"打到一个语义不同但名字相近的模型不会有任何异常迹象，只有账单会变", modelID, a.modelIDs())
	}
	if remote == "" {
		return "", fmt.Errorf("wire: deepseek_chat: 模型 id %q 映射到空的远端模型名（models.yaml 的 remote_name 必填）", modelID)
	}
	return remote, nil
}

// modelIDs 返回已声明模型 id 的**有序**列表（只用于错误信息）。
//
// 排序是刻意的：map 遍历顺序随机，错误信息里出现随机顺序会让"同一个错误两次
// 输出不同"，破坏可复现性（也让日志比对与 golden 测试无从下手）。
func (a *DeepSeekChatAdapter) modelIDs() []string {
	ids := make([]string, 0, len(a.cfg.RemoteNames))
	for id := range a.cfg.RemoteNames {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Execute 发起一次调用（WireAdapter 接口方法）。
//
// 分层（阶段 1 修正，见 ADR-0016）：入参是已归一化的 WireRequest，本方法只做
// "协议编码 + 发 HTTP + 读原始字节"，不做角色布局、不做参数剔除、不做错误分类。
//
// 返回语义（本实现把接口注释里两句话的冲突定成一条规则，见相位说明文档）：
//
//	收到 HTTP 响应（任何状态码）→ (*WireResponse, nil)。**非 2xx 不在这里
//	  报错**：厂商错误 → ErrorClass 的翻译属 Denormalizer（它才有"同一语义在
//	  不同厂商的不同编码"那张表），而它需要的输入正是 resp.Body + StatusCode。
//	  在这里先报一个 error 会让调用方要么丢掉 body，要么把 body 当字符串解析
//	  ——两条路都退化成"靠字符串猜类别"。
//	未收到 HTTP 响应（连接失败/超时/取消）→ (*WireResponse, err)。resp 仍然
//	  非 nil 且 Latency 已填充（它是归因的证据：StatusCode==0 的含义就是
//	  "没收到响应"，见 WireResponse 零值契约）。
//
// 后置条件：返回的 resp 非 nil；Latency 已填充；Body 是**未经解析**的原始字节
// （预解析会让 Denormalizer 只能猜错误类别）。ctx 取消原样返回 ctx.Err()；
// 适配器自身的 TimeoutMs 超时返回 ErrCallTimeout（两者必须可区分）。
//
// 并发：安全（不写共享状态）。
func (a *DeepSeekChatAdapter) Execute(ctx context.Context, req *WireRequest, binding types.Binding) (*WireResponse, error) {
	if req == nil {
		return nil, errors.New("wire: deepseek_chat: req 为 nil")
	}
	if req.Wire != types.WireOpenAIChat {
		return nil, fmt.Errorf("wire: deepseek_chat: 线路不匹配（req.Wire=%q，本适配器=%q）", req.Wire, types.WireOpenAIChat)
	}
	if req.Endpoint != a.cfg.EndpointName || binding.Endpoint != a.cfg.EndpointName {
		return nil, fmt.Errorf("wire: deepseek_chat: 接入点不匹配（req.Endpoint=%q binding.Endpoint=%q，本适配器=%q）："+
			"用本端点的密钥把请求发到别处是静默的凭据错配", req.Endpoint, binding.Endpoint, a.cfg.EndpointName)
	}
	if req.Model != binding.Model {
		return nil, fmt.Errorf("wire: deepseek_chat: 模型不一致（req.Model=%q binding.Model=%q）", req.Model, binding.Model)
	}
	remoteName, err := a.ModelName(req.Model)
	if err != nil {
		return nil, err
	}
	body, err := EncodeOpenAIChatBody(req, remoteName, a.bucketField)
	if err != nil {
		return nil, err
	}

	callCtx := ctx
	if ms := req.Sampling.TimeoutMs; ms > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, a.endpointURL(chatCompletionsPath), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("wire: deepseek_chat: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)

	start := time.Now()
	httpResp, err := a.client.Do(httpReq)
	out := &WireResponse{Wire: types.WireOpenAIChat, Latency: time.Since(start)}
	if err != nil {
		return out, a.transportError(ctx, callCtx, err, req.Sampling.TimeoutMs)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes+1))
	out.StatusCode = httpResp.StatusCode
	if readErr != nil {
		return out, fmt.Errorf("wire: deepseek_chat: 读取响应体失败（status=%d）: %w", httpResp.StatusCode, readErr)
	}
	if len(raw) > maxResponseBytes {
		// 超限时**丢弃** body：只保留状态码，让上层按状态码处置。带上半截
		// 响应体反而危险——半截 JSON 在 Denormalizer 眼里是"无法解析"，
		// 会把一个明确的"响应体过大"错误归成解析错误。
		return out, fmt.Errorf("wire: deepseek_chat: 响应体超过 %d 字节上限，已丢弃（不让一个异常响应把内存打满；status=%d）",
			maxResponseBytes, httpResp.StatusCode)
	}
	out.Body = raw
	out.UsageRaw = bestEffortUsage(raw)
	return out, nil
}

// transportError 把"没收到 HTTP 响应"的三类成因翻译成可区分的错误。
//
// 三类必须分开（这不是细节，是熔断与升级证据的输入）：
//   - caller 的 ctx 结束：原样返回 ctx.Err()。上游主动停机不是厂商故障，
//     包装它会让熔断计数虚高、并让升级证据凭空多一条；
//   - 适配器自己的 TimeoutMs 超时：ErrCallTimeout（可重试，且是调优依据）。
//     同时用 %w 链上 ctx.Err()，让按 errors.Is(err, context.DeadlineExceeded)
//     判断的既有代码继续有效；
//   - 其它传输失败（DNS / 连接被拒 / TLS / 断流）：普通错误，带原始成因。
func (a *DeepSeekChatAdapter) transportError(callerCtx, callCtx context.Context, err error, timeoutMs int64) error {
	if callerCtx.Err() != nil {
		return callerCtx.Err()
	}
	if callCtx.Err() != nil {
		return fmt.Errorf("%w（SamplingParams.TimeoutMs=%d）: %w", ErrCallTimeout, timeoutMs, callCtx.Err())
	}
	return fmt.Errorf("wire: deepseek_chat: 未收到 HTTP 响应: %w", err)
}

// HealthCheck 是探测期 / 熔断恢复期的轻量 ping（WireAdapter 接口方法）。
//
// 实现是 GET {BaseURL}/models：OpenAI 兼容线路的模型列表是"认证 + 连通性"的
// 最小验证（它同时验证密钥有效，而单纯的 TCP 连通做不到这一点）。
//
// 只报告本次结果，**不修改任何熔断状态**：状态迁移统一由 Pool 按 CircuitPolicy
// 决定。让适配器自己改状态会出现两套状态机，判定条件迟早不一致。
//
// endpoint 必须是本适配器服务的接入点，否则报错（把 A 的探测记到 B 头上会让
// 健康度张冠李戴）。
func (a *DeepSeekChatAdapter) HealthCheck(ctx context.Context, endpoint string) error {
	if endpoint == "" {
		return errors.New("wire: deepseek_chat: HealthCheck 的 endpoint 为空（零值不是\"全部\"，探测必须指名）")
	}
	if endpoint != a.cfg.EndpointName {
		return fmt.Errorf("wire: deepseek_chat: HealthCheck 的 endpoint=%q 不属于本适配器（本适配器=%q）", endpoint, a.cfg.EndpointName)
	}
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.endpointURL(modelsPath), nil)
	if err != nil {
		return fmt.Errorf("wire: deepseek_chat: 构造健康探测请求失败: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	httpReq.Header.Set("Accept", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil && ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("wire: deepseek_chat: 健康探测超时（%s）: %w", healthCheckTimeout, ctx.Err())
		}
		return fmt.Errorf("wire: deepseek_chat: 健康探测失败: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// 带上响应体片段：401 与 404 的区别（密钥错 vs 路径错）全在 body 里，
		// 只报状态码会让两种完全不同的修法看起来一模一样。
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		return fmt.Errorf("wire: deepseek_chat: 健康探测返回 status=%d（body 片段：%s）", httpResp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// endpointURL 拼接接入点 URL 与路径。
//
// BaseURL 末尾的 "/" 要吸收掉（"https://x/v1/" + "/chat/completions" 会得到
// 双斜杠路径，多数网关容忍但少数会 404，而 404 又被归成 capability 错误）。
func (a *DeepSeekChatAdapter) endpointURL(path string) string {
	return strings.TrimRight(a.cfg.BaseURL, "/") + path
}

// ---------------------------------------------------------------------------
// 请求体编码
// ---------------------------------------------------------------------------

// openAIChatRequestBody 是发往 /chat/completions 的请求体。
//
// 为什么用结构体而不是 map[string]any：
//   - 字段顺序 = 线协议顺序，写进类型里，评审时一眼可见；用 map 虽然
//     encoding/json 也按键名排序（确定性没问题），但"请求体长什么样"就只能
//     靠运行一次才知道；
//   - omitempty 的语义（"零值不发送"）在这里是**契约**（见 EncodeOpenAIChatBody），
//     散在 map 赋值里会让它变成隐式约定。
//
// 键名与官方文档一致（docs/deepseek-api/chat-complete.html 的请求参数表）。
type openAIChatRequestBody struct {
	Model    string                 `json:"model"`
	Messages []openAIChatReqMessage `json:"messages"`

	Tools     []openAIChatReqTool `json:"tools,omitempty"`
	MaxTokens int                 `json:"max_tokens,omitempty"`

	// Temperature / TopP 用指针：零值 = **不发送**（走厂商默认），
	// 非零才发。见 EncodeOpenAIChatBody 的"零值即不发送"说明。
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`

	ResponseFormat *openAIChatReqResponseFormat `json:"response_format,omitempty"`

	// Thinking 是协议形态的思维参数（openAIChatThinking，见 ADR-0020）。
	// 类型是 any 因为 WireRequest.Thinking 就是 any；空对象与 nil 的区别由
	// Normalizer 保证（nil 才省略）。
	Thinking *openAIChatReqThinking `json:"thinking,omitempty"`
	// ReasoningEffort 是本线路的**顶层**思考强度字段（none/low/high/max）。
	// 它与 Thinking 对象是两个不同的参数：文档把"开关"放进 thinking 对象、
	// 把"强度"放在顶层（docs/deepseek-api/chat-complete.html 的参数表）。
	// 阶段 1 不做流式、不带 tool_choice 等参数，因此这里的字段顺序就是
	// 请求体的实际字节顺序：thinking 紧邻 reasoning_effort，读起来是一回事。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// 缓存桶字段：两个键名互斥，编码时只填其中一个（另一个保持零值被 omitempty
	// 丢掉）。用一个 map 或自定义 MarshalJSON 也能做到，但那会把"字段名"
	// 这个唯一的变化点藏进编码逻辑里——这里显式留两个字段，评审时直接看得到
	// 当前默认发的是哪个。
	UserID string `json:"user_id,omitempty"`
	User   string `json:"user,omitempty"`

	// Stream 显式声明 false：阶段 1 只做非流式（13.3 的范围内没有流式解析）。
	// 显式发出来而不是省略，是为了让"这不是流式请求"成为请求体的一部分——
	// 厂商侧的默认值将来若变化，我们不会突然收到一个需要增量拼装的 SSE 流。
	Stream bool `json:"stream"`
}

// openAIChatReqMessage 是请求里的单条消息。
//
// Content 用 *string 且**不带** omitempty：OpenAI 兼容协议里 content 键是
// 恒存在的，纯工具调用的 assistant 消息其值为 null（官方示例里 messages
// 直接 append 了含 "content": null 的响应消息）。省略该键在部分网关会被当成
// 非法消息，而带上 null 两边都认。
type openAIChatReqMessage struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
	// ReasoningContent 是历史思维链（ADR-0023：带 tools 的请求应回传）。
	// 编码器只在 assistant 消息 + 请求携带 tools 时发出（厂商契约：
	// 不带 tools 时传了也被忽略——不发，省字节）。
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIChatReqCall `json:"tool_calls,omitempty"`
	ToolCallID       string              `json:"tool_call_id,omitempty"`
}

type openAIChatReqCall struct {
	ID       string                `json:"id"`
	Type     string                `json:"type"` // 恒为 "function"（目前协议只有这一种）
	Function openAIChatReqFunction `json:"function"`
}

type openAIChatReqFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 文本（不是对象）：协议要求字符串
}

type openAIChatReqTool struct {
	Type     string                 `json:"type"` // 恒为 "function"
	Function openAIChatReqNamedTool `json:"function"`
}

type openAIChatReqNamedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIChatReqThinking struct {
	Type string `json:"type"`
	// BudgetTokens 只在 budget 控制下出现（本线路 DeepSeek 不认，见
	// openAIChatThinking.BudgetTokens）：omitempty 让其它控制方式的对象
	// 里只有 type 一个键。
	BudgetTokens *int `json:"budget_tokens,omitempty"`
}

type openAIChatReqResponseFormat struct {
	Type string `json:"type"`
}

// EncodeOpenAIChatBody 把 WireRequest 编码成发往 /chat/completions 的请求体。
//
// 独立成导出函数的理由：它是**协议编码**，与"怎么发 HTTP"无关。测试要能逐字节
// 比对它（TestNormalizerByteStability 的姊妹测试），探测工具要能打印它
// （13.3 的交付物之一就是"发送的消息数组"），而这两件事都不该需要起一个 HTTP
// 服务器。
//
// 编码规则（每一条都是为了"字节可复现 + 不静默丢字段"）：
//
//   - **零值即不发送**：MaxTokens / TopK / Temperature / TopP 为零时不写进
//     请求体，走厂商默认值。代价是**无法请求 temperature=0 的贪心解码**
//     （0 与"未设置"在 SamplingParams 里不可区分，它是值类型）——这是已知
//     缺口，修复要先把字段改成指针（契约变更，需 ADR），已在相位说明里记录。
//     反向选择（零值也发）会让"没配 temperature 的 Profile"静默变成确定性
//     解码，而厂商默认是随机的，属于"改了所有人的行为却没人事先知道"；
//   - **TopK 永不出现在请求体里**：OpenAI 兼容协议没有 top_k 字段
//     （SamplingParams.TopK 是本框架的通用参数，本线路无处安放）。
//     不报错是因为它是"本线路用不上"而不是"配置错"；但也不静默——
//     Normalizer 侧对声明 unsupported 的模型会记降级（见 stripUnsupportedParams）；
//   - **Attachments 非空一律报错**：阶段 1 没有图片输入（10.14 在阶段 2），
//     静默丢弃会让模型基于残缺信息作答（它以为自己看到了那张图）；
//   - **tool_call 的 arguments 必须是合法 JSON 文本**：协议要字符串，而内容是
//     模型的原始产出。这里只校验合法性，不重新序列化（重新序列化会改字节）。
//
// bucketField 决定缓存桶的字段名（见 DefaultBucketField 的文档冲突说明）。
//
// 失败：req 为 nil、远端模型名为空、bucketField 不在白名单、消息不满足本线路
// 的硬规则（角色与 tool_calls/tool_call_id 错配、两者皆空、工具定义缺名或
// 参数不是合法 JSON）。这些都是**框架/装配错误**，不能带着它们发请求。
//
// 并发：纯函数（不改 req，也不改 req 里的任何切片元素）。
func EncodeOpenAIChatBody(req *WireRequest, remoteName, bucketField string) ([]byte, error) {
	if req == nil {
		return nil, errors.New("wire: encode: req 为 nil")
	}
	if remoteName == "" {
		return nil, errors.New("wire: encode: 远端模型名为空（不回落成内部 id——厂商侧的模型名与内部 id 是两个命名空间）")
	}
	if bucketField != DefaultBucketField && bucketField != legacyBucketField {
		return nil, fmt.Errorf("wire: encode: bucketField=%q 不在白名单 {%q, %q}", bucketField, DefaultBucketField, legacyBucketField)
	}
	if !req.ResponseFormat.Valid() {
		return nil, fmt.Errorf("wire: encode: ResponseFormat=%q 不是已定义的输出格式（零值合法，但它必须是 OutputFormat 的已定义取值）", req.ResponseFormat)
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("wire: encode: Messages 为空（没有任何消息的请求一定会被厂商拒绝）")
	}

	msgs := make([]openAIChatReqMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		enc, err := encodeOpenAIChatMessage(m, i)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, enc)
	}
	// 历史思维链（ADR-0023 的通路）：assistant 消息上的 Reasoning 只在请求
	// 携带 tools 时发出。放在编码层而不是 Normalizer 的原因：厂商契约说
	// tools 的存在决定回传，而 encode 是唯一同时看得到消息与 tools 的点。
	if len(req.Tools) > 0 {
		for i := range msgs {
			if msgs[i].Role == string(types.WireAssistant) && req.Messages[i].Reasoning != "" {
				msgs[i].ReasoningContent = req.Messages[i].Reasoning
			}
		}
	}

	body := openAIChatRequestBody{
		Model:    remoteName,
		Messages: msgs,
		Stream:   false,
	}
	if len(req.Tools) > 0 {
		tools, err := encodeOpenAIChatTools(req.Tools)
		if err != nil {
			return nil, err
		}
		body.Tools = tools
	}
	body.MaxTokens = req.Sampling.MaxTokens
	if req.Sampling.Temperature != 0 {
		t := req.Sampling.Temperature
		body.Temperature = &t
	}
	if req.Sampling.TopP != 0 {
		p := req.Sampling.TopP
		body.TopP = &p
	}
	switch req.ResponseFormat {
	case OutputFormatJSONObject:
		body.ResponseFormat = &openAIChatReqResponseFormat{Type: string(OutputFormatJSONObject)}
	case OutputFormatNone:
		// 不发送：显式发送 "text" 与不发送在厂商侧等价，而少一个字段就少一处
		// 可能变化的字节（见 OutputFormat 的零值契约）。
	}
	if err := applyOpenAIChatThinking(&body, req.Thinking); err != nil {
		return nil, err
	}
	if req.CacheBucket != "" {
		switch bucketField {
		case DefaultBucketField:
			body.UserID = string(req.CacheBucket)
		case legacyBucketField:
			body.User = string(req.CacheBucket)
		}
	}

	return marshalCanonicalJSON(body)
}

// applyOpenAIChatThinking 把协议形态的思维参数放到请求体的正确层级上。
//
// 这是本线路唯一"一个语义字段拆成两个协议字段"的地方，所以单独一个函数：
// 放在 EncodeOpenAIChatBody 里会让那段主体多出一条与其它字段形状不同的分支，
// 而它的细节（哪个键进对象、哪个键上顶层）是 ADR-0020 的核心，值得有自己的
// 位置与注释。
//
// 未知类型一律报错而不是忽略：`Thinking any` 的类型安全为零，静默忽略会让
// "思维参数根本没发出去"变成一个只在外观上表现为"模型没思考"的现象。
func applyOpenAIChatThinking(body *openAIChatRequestBody, thinking any) error {
	switch t := thinking.(type) {
	case nil:
		// 不发送任何思维参数（"不指定" ≠ "关闭"）。
		return nil
	case openAIChatThinking:
		if t.Type != "" {
			body.Thinking = &openAIChatReqThinking{Type: t.Type, BudgetTokens: t.BudgetTokens}
		} else if t.BudgetTokens != nil {
			// 有预算却没有开关：协议里 budget_tokens 挂在 thinking 对象内部，
			// 没有对象就没有落点。报错而不是发一个只有预算的孤立顶层字段。
			return errors.New("wire: encode: Thinking 带 BudgetTokens 但没有 Type；" +
				"本线路的预算字段只能放在 thinking 对象内部，缺少对象就没有落点")
		}
		body.ReasoningEffort = t.ReasoningEffort
		return nil
	default:
		return fmt.Errorf("wire: encode: Thinking 的类型 %T 不是本线路的协议形态（openAIChatThinking，见 ADR-0020）；"+
			"未知形态一律拒绝，因为编码器无法判断它该放进 thinking 对象还是顶层字段", thinking)
	}
}

// marshalCanonicalJSON 序列化请求体并去掉 Encoder 追加的换行。
//
// 用 json.Encoder + SetEscapeHTML(false) 而不是 json.Marshal：
// Marshal 会把 < > & 转义成 \u003c 之类。语义上两者等价（厂商解析后拿到同一个
// 字符串），但请求体要**给人看**（13.3 的探测报告要打印消息数组），而
// "XML 标注全变成 \\u003c" 会让报告难以核查——本项目大量使用 XML 标注渲染
// （见 types.Stability 的渲染约定），这个差别在探测报告里非常显眼。
//
// 确定性由"结构体字段顺序 + encoding/json 对 map 键排序"共同保证：同样的
// WireRequest 永远得到同样的字节。
func marshalCanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("wire: encode: 序列化请求体失败: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// encodeOpenAIChatMessage 把一条 WireMessage 编成请求体里的消息。
//
// index 只用于错误定位：出错时告诉调用方**是哪一条**消息破坏了协议。批量编码
// 里"第 N 条错了"是最有用的信息（那一条通常就是历史拼接 bug 的现场）。
//
// 这里复查 WireMessage 的硬规则（Assert 也查）不是重复劳动：EncodeOpenAIChatBody
// 是导出函数，探测工具与测试可以直接调它（它们未必经过 Normalizer.Assert）；
// 而把一条角色与 tool_call_id 错配的消息发出去，得到的是厂商的 400，
// 会被误归成 capability 错误——修的方向就错了。
func encodeOpenAIChatMessage(m WireMessage, index int) (openAIChatReqMessage, error) {
	if !m.Role.Valid() {
		return openAIChatReqMessage{}, fmt.Errorf("wire: encode: 第 %d 条消息的 Role=%q 未定义（线路角色必须显式）", index, m.Role)
	}
	if len(m.Attachments) > 0 {
		// 阶段 1 没有图片输入（10.14 在阶段 2）。报错而不是丢弃：静默丢弃会让
		// 模型基于残缺信息作答，而且它以为自己看到了那张图。
		return openAIChatReqMessage{}, fmt.Errorf("wire: encode: 第 %d 条消息带 %d 个附件；"+
			"本线路阶段 1 不支持图片输入（10.14 在阶段 2），拒绝发送而不是静默丢弃", index, len(m.Attachments))
	}
	if m.CacheControl != "" {
		// 本线路是隐式前缀缓存（CacheImplicitPrefix），没有显式断点字段。
		// 带 CacheControl 的请求会把一个厂商不认的字段发出去（Assert 也拦）。
		return openAIChatReqMessage{}, fmt.Errorf("wire: encode: 第 %d 条消息带 CacheControl=%q；"+
			"本线路是隐式前缀缓存，没有显式断点字段", index, m.CacheControl)
	}
	if len(m.ToolCalls) > 0 && m.Role != types.WireAssistant {
		return openAIChatReqMessage{}, fmt.Errorf("wire: encode: 第 %d 条消息的角色是 %q 却带 tool_calls；"+
			"工具调用只能由 assistant 发起", index, m.Role)
	}
	if m.ToolCallID != "" && m.Role != types.WireTool {
		return openAIChatReqMessage{}, fmt.Errorf("wire: encode: 第 %d 条消息的角色是 %q 却带 tool_call_id；"+
			"配对 id 只属于 tool 消息", index, m.Role)
	}
	if m.ToolCallID == "" && m.Role == types.WireTool {
		return openAIChatReqMessage{}, fmt.Errorf("wire: encode: 第 %d 条 tool 消息缺少 tool_call_id；"+
			"孤立的工具结果无法配对到任何调用，厂商侧等价于非法消息", index)
	}
	if m.Content == "" && len(m.ToolCalls) == 0 && m.Reasoning == "" {
		return openAIChatReqMessage{}, fmt.Errorf("wire: encode: 第 %d 条消息既无 content 也无 tool_calls（空消息会占一个角色槽位）", index)
	}

	out := openAIChatReqMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
	// content 的两种情形（见 openAIChatReqMessage 的注释）：
	//   - 纯工具调用的 assistant：null（不写空串——空串会被部分网关当成
	//     "模型说了空话"，null 才是"这一轮没有文本"）；
	//   - 其余：原样。
	//
	// 注意这里**没有**"tool 消息的空结果"这一情形：空 content 的 tool 消息
	// 在上一段的自检里就被拒了（与 Normalizer 的 assertMessages 同一规则：
	// 占一个角色槽位却不携带信息）。所以"空结果"必须由调用方显式表达
	// （例如 Content="（无输出）"），本函数不会替它编一个占位串。
	switch {
	case m.Role == types.WireAssistant && m.Content == "" && (len(m.ToolCalls) > 0 || m.Reasoning != ""):
		out.Content = nil
	default:
		content := m.Content
		out.Content = &content
	}
	if len(m.ToolCalls) > 0 {
		out.ToolCalls = make([]openAIChatReqCall, 0, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			if tc.Name == "" {
				return openAIChatReqMessage{}, fmt.Errorf("wire: encode: 第 %d 条消息的第 %d 个 tool_call 缺 name", index, i)
			}
			if len(tc.Arguments) > 0 && !json.Valid(tc.Arguments) {
				return openAIChatReqMessage{}, fmt.Errorf("wire: encode: 第 %d 条消息的第 %d 个 tool_call 的 arguments 不是合法 JSON", index, i)
			}
			args := string(tc.Arguments)
			if args == "" {
				// 无参工具：协议要求 arguments 是 JSON 文本，空串会被解析失败。
				// 补 "{}" 而不是补 "null"：null 在多数实现的 schema 校验里会炸。
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, openAIChatReqCall{
				ID:       tc.ID,
				Type:     "function",
				Function: openAIChatReqFunction{Name: tc.Name, Arguments: args},
			})
		}
	}
	return out, nil
}

// encodeOpenAIChatTools 编码工具定义。
//
// Parameters 用 json.RawMessage 原样透传（只做一次合法性检查），理由：它是
// 工具契约的一部分，任何"顺手规范化"（重排键、压空白、去空值）都会让**同一张
// schema 在不同时刻渲染成不同字节**——而它坐落在 frozen 前缀里（见 ToolDef
// 的契约）。合法性必须查：一段非法 JSON 送到厂商侧是 400，会被误归成
// capability 错误。
func encodeOpenAIChatTools(defs []ToolDef) ([]openAIChatReqTool, error) {
	out := make([]openAIChatReqTool, 0, len(defs))
	seen := make(map[string]bool, len(defs))
	for i, d := range defs {
		if d.Name == "" {
			return nil, fmt.Errorf("wire: encode: 第 %d 个工具定义缺 name", i)
		}
		if seen[d.Name] {
			// 重名工具在厂商侧的行为不确定（有的取第一个、有的报错），
			// 而"模型调用了名字相同的另一个工具"是最难归因的一类故障。
			return nil, fmt.Errorf("wire: encode: 工具名 %q 重复（第 %d 个）；重名工具让调用结果无法归因", d.Name, i)
		}
		seen[d.Name] = true
		params := d.Parameters
		if len(params) == 0 {
			return nil, fmt.Errorf("wire: encode: 工具 %q 的 Parameters 为空（协议要求 JSON Schema 对象）", d.Name)
		}
		if !json.Valid(params) {
			return nil, fmt.Errorf("wire: encode: 工具 %q 的 Parameters 不是合法 JSON", d.Name)
		}
		out = append(out, openAIChatReqTool{
			Type: "function",
			Function: openAIChatReqNamedTool{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  params,
			},
		})
	}
	return out, nil
}

// bestEffortUsage 尽力从响应体里取出用量；无法解析时返回 nil。
//
// 与 Denormalizer 调的是同一个 mapOpenAIChatUsage，因此两处结论不可能不一致
// （口径只有一份）。nil 的含义是"用量未知"，**不是**"用量为 0"（见
// WireResponse.UsageRaw 的零值契约）——所以这里不能在解析失败时返回零值结构。
//
// 这里刻意不因为解析失败而报错：响应体的语义解释是 Denormalizer 的职责，
// 它才能给出"哪个字段为什么读不出来"的错误信息。适配器只负责不把可用信息丢掉。
func bestEffortUsage(body []byte) *types.TokenUsage {
	if len(body) == 0 {
		return nil
	}
	var parsed struct {
		Usage *openAIChatUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	return mapOpenAIChatUsage(parsed.Usage)
}
