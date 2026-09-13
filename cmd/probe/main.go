// 命令 probe 是设计文档 13.3（阶段 1）的交付物：用 6 个主用例实测 DeepSeek 的
// openai_chat 线路与"隐式前缀缓存"这个经济性假设。
//
// 深度轮（第 5 轮起）又加了三组用例，它们回答的是"第一轮测不出、但会改变设计"
// 的问题：
//
//   - 用例 7（缓存单元对齐扫描）：前缀要留多少余量才能命中——cache.html 只写
//     "按固定 token 间隔落盘"，没给间隔；
//   - 用例 8（跨模型缓存 + 第二个模型的档位支持）：阶梯升级换模型时缓存还在吗；
//   - 用例 9（带 tools 时历史 reasoning_content 的回传形态与 token 代价）。
//
// 其中 8/9 走**原始请求**（见 contract.go）：我们的编码器发不出历史思维链
// （Segment / WireMessage 上没有承载它的字段），要测这条厂商契约只能绕过它。
// 用例 7 走正常的生产路径（它测的就是前缀字节与缓存的关系）。
//
// 为什么是一个独立命令而不是单元测试：
//   - 它要**真发请求**（花钱、要密钥、要网络），不能挂在 `go test ./...` 上；
//   - 它的产出是**给人看的报告**（发送的消息数组、usage、耗时、逐项判定），
//     而测试的产出是"通过/失败"；
//   - 它必须能在没有配置文件的情况下跑（阶段 1 没有配置层，见 13.3 的"不做"
//     清单：Pool / Router / Profile 都不做，模型与接入点在这里硬编码 + flag 覆盖）。
//
// 用法：
//
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/probe                  # 真跑（约几分钱）
//	go run ./cmd/probe -dry-run                                 # 只编码并打印请求体，不发请求
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/probe -repeat-gap=5m    # 顺带验缓存 TTL > 5min
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/probe -report run.md    # 把测量结果写成 markdown
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/probe -level-repeats=3 -repeat-gap=10m -report run.md   # 深度轮
//
// 退出码：任何一项硬判定 FAIL → 1（可当门禁用）。WARN 是"需要人判断的现象"
// （如"不同 user_id 是否共享前缀缓存"），不影响退出码，但一定会被打印并写进报告
// ——13.3 明确要求把用例 6 的观测结果记录在探测报告里。
//
// 与设计文档的两处偏离（都在代码里有详细注释，也必须写进探测报告）：
//   - 缓存桶字段名用官方文档的 user_id，而不是设计文档 10.15 写的 user（见
//     wire.DefaultBucketField 的冲突说明）；
//   - thinking 档位走顶层 reasoning_effort，不是 thinking 对象内部的 type
//     （见 ADR-0020）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// 默认值。除远端模型名以外，其余都取自设计文档（10.4 的 models.yaml 片段、
// 10.15 的 endpoints.yaml 片段、13.3 的 BaseURL），因此"默认跑一次"就是
// 设计文档设想的那个配置。
//
// 远端模型名是**唯一**取当前官方文档而非设计文档的默认值：设计文档写的是
// deepseek-chat / deepseek-reasoner，而 docs/deepseek-api/*.html 里现在的模型名
// 是 deepseek-flash（chat 与 thinking 都由它承担，强度走 reasoning_effort）。
// 模型名是配置不是常量，用 -remote 覆盖即可；探测报告要记录实际用的名字。
const (
	defaultEndpoint    = "deepseek-main"
	defaultBaseURL     = "https://api.deepseek.com/v1"
	defaultModelID     = "deepseek-flash"
	defaultRemote      = "deepseek-flash"
	defaultAPIKeyEnv   = "DEEPSEEK_API_KEY"
	defaultBucket      = "probe-agent-01"
	defaultOtherBucket = "probe-agent-02"
	defaultTimeoutMs   = 120_000
	defaultMaxTokens   = 512
	defaultLevels      = "none,low,high,max"
)

// options 是命令行选项（阶段 1 的"配置层"就是它）。
type options struct {
	dryRun      bool
	endpoint    string
	baseURL     string
	apiKeyEnv   string
	modelID     string
	remote      string
	bucketField string
	bucket      types.AgentID
	otherBucket types.AgentID
	timeoutMs   int64
	maxTokens   int
	levels      []string
	// levelRepeats 是每个档位的重复次数（默认 1）。
	//
	// 单次采样无法判断"档位越高思考越多"（探测报告 §3.8）：同一档位的方差
	// 没测过，一次 max 比 high 短就下"不单调"的结论是过度解读。重复 N 次取均值
	// 才能把"档位效应"与"同档位方差"分开。
	levelRepeats int
	// remotePro 是用例 8 的第二个远端模型名（跨模型缓存 + 档位支持）。
	remotePro string
	// contractOnly 只跑契约探测（用例 8/9/10）。
	//
	// 存在的理由：那三组用例各自带 nonce、彼此独立（不依赖 1~7 建立的缓存），
	// 而在"只改了一处契约探测"之后重跑整套主用例（含 -repeat-gap 的等待）既慢
	// 又花钱。给一个只跑它们的开关，让"验证一处厂商契约"变成秒级操作。
	contractOnly bool
	repeatGap    time.Duration
	reportPath   string
	// nonce 是本次运行的唯一标记，写进用例 6 的前缀里。
	//
	// 为什么必须有：用例 6 要回答"不同 user_id 是否共享前缀缓存"，而缓存是
	// **持久**的——沿用固定前缀时，上一次运行早已把该前缀写进两个桶，本次运行
	// 再问"另一个桶命中了吗"就无法归因：命中可能来自它自己上一次的副本，而不是
	// 共享。带 nonce 后"本桶第一次看到这个前缀"必然 cached=0，于是"另一个桶
	// cached>0"只可能来自跨桶共享。
	nonce string
}

func parseOptions() options {
	var o options
	flag.BoolVar(&o.dryRun, "dry-run", false, "只编码并打印请求体，不发网络请求（不需要密钥）")
	flag.StringVar(&o.endpoint, "endpoint", defaultEndpoint, "接入点名（同时是缓存键维度，见 wire.ModelCachePrefix）")
	flag.StringVar(&o.baseURL, "base-url", defaultBaseURL, "接入点根地址；带不带 /v1 由这里决定")
	flag.StringVar(&o.apiKeyEnv, "api-key-env", defaultAPIKeyEnv, "存放密钥的环境变量名（密钥绝不进命令行与文件）")
	flag.StringVar(&o.modelID, "model", defaultModelID, "内部模型 id（缓存键与能力表用）")
	flag.StringVar(&o.remote, "remote", defaultRemote, "远端模型名（真正发给厂商的名字）")
	flag.StringVar(&o.bucketField, "bucket-field", wire.DefaultBucketField,
		"缓存桶落到请求体的字段名；可选 "+wire.DefaultBucketField+"（官方文档）或 user（设计文档 10.15 的写法）")
	flag.Var(bucketFlag{&o.bucket}, "bucket", "用例 1-5 的缓存桶（= AgentID）")
	flag.Var(bucketFlag{&o.otherBucket}, "other-bucket", "用例 6 的另一个缓存桶（验证桶隔离/共享）")
	flag.Int64Var(&o.timeoutMs, "timeout-ms", defaultTimeoutMs, "单次调用超时（SamplingParams.TimeoutMs）")
	flag.IntVar(&o.maxTokens, "max-tokens", defaultMaxTokens, "单次请求的输出上限；思考模式的思维链也算在里面，太小会 finish_reason=length")
	levels := flag.String("thinking-levels", defaultLevels, "用例 4 逐档位验证的档位名（逗号分隔）；必须在能力表的 thinking_levels 里")
	flag.IntVar(&o.levelRepeats, "level-repeats", 1, "用例 4 每个档位的重复次数；>1 时打印均值与极差（判断档位强度序要 ≥6，或分两轮各 3 次——见探测报告 §3.8）")
	flag.StringVar(&o.remotePro, "remote-pro", defaultRemotePro, "用例 8 的第二个远端模型名（跨模型缓存 + 档位支持）")
	flag.BoolVar(&o.contractOnly, "contract-only", false, "只跑契约探测（用例 8/9/10，原始请求），跳过 1~7 主用例")
	flag.DurationVar(&o.repeatGap, "repeat-gap", 0, ">0 时在用例 2 之后再等这么久重发一次（验缓存 TTL，如 5m）")
	flag.StringVar(&o.reportPath, "report", "", "把本次测量结果写成 markdown 到这个路径（空 = 只打印）")
	flag.StringVar(&o.nonce, "nonce", "", "用例 6 的前缀随机标记（空 = 用当前时间自动生成；同一次运行内两条请求必须一致）")
	flag.Parse()

	if o.nonce == "" {
		o.nonce = time.Now().Format("20060102-150405.000000")
	}

	o.bucket = types.AgentID(defaultBucket)
	o.otherBucket = types.AgentID(defaultOtherBucket)
	for _, l := range strings.Split(*levels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			o.levels = append(o.levels, l)
		}
	}
	if len(o.levels) == 0 {
		o.levels = []string{"none"}
	}
	// 重复次数至少为 1：0 或负数会让用例 4 静默变成"一个档位都不测"，
	// 而报告里只会少几行——这类"配置写错却看起来正常"的形态必须在这里挡掉。
	if o.levelRepeats < 1 {
		o.levelRepeats = 1
	}
	return o
}

// bucketFlag 让 types.AgentID 能直接用 flag 解析。
//
// 命名类型 + flag.Value 而不是裸 string：缓存桶在框架里是 types.AgentID
// （见 types.Binding 的注释），探测工具也不该给自己开一个"字符串当桶"的口子
// ——那正是"两个 Agent 落进同一个桶"这类事故的起点。
type bucketFlag struct{ dst *types.AgentID }

func (b bucketFlag) String() string {
	if b.dst == nil {
		return ""
	}
	return string(*b.dst)
}

func (b bucketFlag) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("缓存桶不能为空串（空桶 = 不发送桶字段 = 缓存不隔离）")
	}
	*b.dst = types.AgentID(v)
	return nil
}

func main() {
	o := parseOptions()
	// Ctrl-C 走 ctx 取消路径：适配器必须原样返回 ctx.Err()（而不是包装成厂商
	// 错误），这一步顺手把那条路径跑通。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := run(ctx, o)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, o options) int {
	s, err := newSession(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: 装配失败: %v\n", err)
		return 2
	}
	s.printHeader()

	if !o.contractOnly {
		if err := s.runCases(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "probe: 运行中断: %v\n", err)
			return 2
		}
	}
	// 契约探测（用例 8/9/10）在最后跑：它要用**原始请求**测厂商契约（跨模型缓存、
	// 历史 reasoning_content 回传），走不了适配器的 Execute（见 contract.go）。
	if err := s.contractCases(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "probe: 契约探测中断: %v\n", err)
		return 2
	}
	failed := s.printSummary()
	if o.reportPath != "" {
		if err := s.writeReport(o.reportPath); err != nil {
			fmt.Fprintf(os.Stderr, "probe: 写报告失败: %v\n", err)
			return 2
		}
		fmt.Printf("\n测量结果已写入 %s\n", o.reportPath)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// staticCaps 是阶段 1 的能力表（配置层的替身）。
//
// 阶段 1 没有配置层（models.yaml 还不存在），因此这里硬编码设计文档 10.4 里
// 那道条目的内容。两处偏离，都必须写进探测报告：
//
//   - thinking_control 用 level（levels = none/low/high/max），而不是设计文档
//     写的 bool + ["off","on"]：当前官方文档用 reasoning_effort 同时表达开关与
//     强度（"none 关闭思考模式；low/high/max 开启思考模式"），而 13.3 的用例 4
//     要求"按 thinking_levels 逐档位验证"——bool 控制下没有档位可验。框架的
//     off/on 词汇在 level 控制下依然可用：off → none（ADR-0020），on 会记一条
//     DegradThinkingLevel 并回落到模型默认档位，而该模型默认就是"开"，因此
//     "on" 的语义仍然成立（代价是每次 on 都留一条降级记录——models.yaml 落地
//     时应把 on/off 也列进档位表，或改用 bool 控制）。
//   - max_context / max_output 沿用设计文档的 65536 / 8192：当前文档没有给出
//     上下文长度（原文"详见 模型 & 价格"），沿用一个已写进设计的数字比凭印象
//     改它更可审计。探测用例不覆盖上下文上限，因此这两项在报告里标为
//     **未验证的声明**，而不是"实测通过"。
type staticCaps struct {
	endpoint string
	modelID  string
	caps     wire.ModelCaps
}

// EffectiveCaps 实现 wire.CapsProvider。
//
// 未知模型 / 未知接入点都报错而不是回落到这张表：阶段 1 只有一个接入点，
// "回落"就等于用 A 端点的能力描述去发 B 端点的请求——能力猜错的后果是
// 厂商 400（被误归成 capability 错误）或参数静默失效。
func (s *staticCaps) EffectiveCaps(modelID, endpoint string) (wire.ModelCaps, error) {
	if modelID != s.modelID {
		return wire.ModelCaps{}, fmt.Errorf("probe: 能力表里没有模型 %q（只有 %q）；阶段 1 不做配置层，换模型需同步改能力表", modelID, s.modelID)
	}
	if endpoint != s.endpoint {
		return wire.ModelCaps{}, fmt.Errorf("probe: 能力表里没有接入点 %q（只有 %q）", endpoint, s.endpoint)
	}
	return s.caps, nil
}

// session 持有一次探测运行的全部装配件与证据。
type session struct {
	opt     options
	norm    *wire.OpenAICompatNormalizer
	denorm  *wire.OpenAICompatDenormalizer
	adapter *wire.DeepSeekChatAdapter

	// apiKey / client 只被契约探测（用例 8/9，见 contract.go）使用：
	// 那两个用例要发**手工构造**的请求体，走不了适配器的 Execute。
	//
	// 密钥在这里存一份副本而不是每次从环境变量读：环境变量可能在运行中被改，
	// 而"同一次探测的每个请求用同一个密钥"是结论可比的前提。
	apiKey string
	client *http.Client

	results []*caseResult
	checks  []check
}

func newSession(o options) (*session, error) {
	apiKey := os.Getenv(o.apiKeyEnv)
	if o.dryRun && apiKey == "" {
		// dry-run 不发请求，但适配器的构造期校验会拒绝空密钥——那是对的
		// （空密钥在生产路径上必须炸，不能等到第一次调用才炸）。给一个**显眼的**
		// 占位符，让 dry-run 也能走完装配（顺带验证 bucket-field 白名单等
		// 构造期检查），同时一眼能看出它不是真密钥。
		apiKey = "dry-run-placeholder-key"
	}
	if !o.dryRun && apiKey == "" {
		return nil, fmt.Errorf("环境变量 %s 为空；真跑需要密钥（只想看请求体请用 -dry-run）", o.apiKeyEnv)
	}

	caps := &staticCaps{
		endpoint: o.endpoint,
		modelID:  o.modelID,
		caps: wire.ModelCaps{
			Has:               []types.Capability{types.CapToolCall, types.CapJSONMode, types.CapThinking},
			MaxContext:        65536,
			MaxOutput:         8192,
			CacheMode:         wire.CacheImplicitPrefix,
			UnsupportedParams: nil,
			ThinkingControl:   wire.ThinkControlLevel,
			ThinkingLevels:    append([]string(nil), o.levels...),
			VisionDetail:      false,
		},
	}

	// policy 传 nil = 用线路默认策略（DefaultOpenAIChatPolicy）。探测报告要
	// 打印生效的策略：请求字节与策略是绑定的，报告里缺了它，"两次请求字节
	// 不同"就无法归因。
	norm, err := wire.NewOpenAICompatNormalizer(caps, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 Normalizer: %w", err)
	}
	adapter, err := wire.NewDeepSeekChatAdapter(wire.DeepSeekChatConfig{
		EndpointName: o.endpoint,
		BaseURL:      o.baseURL,
		APIKey:       apiKey,
		RemoteNames:  map[string]string{o.modelID: o.remote},
		BucketField:  o.bucketField,
	})
	if err != nil {
		return nil, fmt.Errorf("构造适配器: %w", err)
	}
	return &session{
		opt:     o,
		norm:    norm,
		denorm:  wire.NewOpenAICompatDenormalizer(),
		adapter: adapter,
		apiKey:  apiKey,
		// 契约探测的 client 超时与适配器的 SamplingParams.TimeoutMs 取同一个值：
		// 两个数会漂，而"探测超时"与"生产超时"不同会让超时结论无法迁移。
		client: &http.Client{Timeout: time.Duration(o.timeoutMs) * time.Millisecond},
	}, nil
}

// binding 构造本次调用的绑定。
//
// 阶段 1 没有 Router，绑定在这里手工装配——但**逐字段**按 types.Binding 的
// 不变量填（CachePrefix 由 wire.ModelCachePrefix 派生，绝不手拼；CacheBucket
// 只来自参数，不重新推导）。探测工具如果在这里"随便填填"，测到的缓存行为
// 就不是生产路径上的行为了。
func (s *session) binding(bucket types.AgentID, level string) types.Binding {
	return types.Binding{
		RungID:      "r0",
		RungIndex:   0,
		Endpoint:    s.opt.endpoint,
		Model:       s.opt.modelID,
		CacheBucket: bucket,
		Wire:        types.WireOpenAIChat,
		Thinking:    types.ThinkingSpec{Level: level},
		BoundAt:     time.Now(),
		CachePrefix: wire.ModelCachePrefix(s.opt.modelID, s.opt.endpoint),
	}
}

// callSpec 描述一次探测调用。
type callSpec struct {
	name   string // 用例名（打印与报告里的标题）
	why    string // 这一用例在验证什么（一句话）
	req    *wire.CanonicalRequest
	bucket types.AgentID
	level  string
}

// caseResult 是一次探测请求的全部证据（打印、判定、写报告都只读它）。
type caseResult struct {
	spec callSpec

	requestBytes []byte // 实发请求体（EncodeOpenAIChatBody 的产出）
	degradations []wire.Degradation
	resp         *wire.WireResponse
	turn         *wire.WireTurn
	err          error // 装配 / 传输 / 解析错误（厂商错误不进这里，见 ADR-0016）
	skipped      bool  // -dry-run：没有发请求
	startedAt    time.Time
}

// call 走完整的一条去程 + 回程：BuildRequest → Assert → Encode → Execute → Denormalize。
//
// 为什么连编码都要显式做一遍（Execute 内部也会编码）：13.3 要求打印"发送的
// 消息数组"，而"打印一份自己拼的 JSON"与"实发字节"迟早会不一致。这里调的是
// 适配器用的**同一个** EncodeOpenAIChatBody，因此打印出来的一定是实发字节的
// 美化形式（同一份 JSON，只是缩进不同）。
func (s *session) call(ctx context.Context, spec callSpec) *caseResult {
	r := &caseResult{spec: spec, startedAt: time.Now()}

	// [阶段 1 发现的歧义（必须写进探测报告）: 设计文档把 thinking 档位放在**阶梯**
	// 上（ladder 的 rung.thinking → Binding.Thinking），而 Normalizer 实际读的是
	// **CanonicalRequest.Thinking**（build 只读 req.Thinking）。两者没有"谁覆盖谁"
	// 的规则，也没有任何代码把 binding.Thinking 拷进 Canonical——阶段 1 的编译层
	// 还没落地，所以这个缺口还没暴露成 bug。探测工具**同时填两处**，让测量结果
	// 不受该歧义影响；报告里把它列为待决策项，否则阶段 2 会出现"档位配在阶梯上
	// 却不生效"的静默失效（请求照发，档位没带上，账面上看不出来）。]
	spec.req.Thinking = types.ThinkingSpec{Level: spec.level}

	binding := s.binding(spec.bucket, spec.level)
	wr, degr, err := s.norm.BuildRequest(spec.req, binding)
	if err != nil {
		r.err = fmt.Errorf("Normalize 失败: %w", err)
		return r
	}
	r.degradations = degr
	if err := s.norm.Assert(wr); err != nil {
		r.err = fmt.Errorf("Assert 失败（框架/配置错误，不是厂商错误）: %w", err)
		return r
	}
	remote, err := s.adapter.ModelName(wr.Model)
	if err != nil {
		r.err = fmt.Errorf("模型名映射失败: %w", err)
		return r
	}
	body, err := wire.EncodeOpenAIChatBody(wr, remote, s.opt.bucketField)
	if err != nil {
		r.err = fmt.Errorf("编码请求体失败: %w", err)
		return r
	}
	r.requestBytes = body

	if s.opt.dryRun {
		r.skipped = true
		return r
	}

	resp, err := s.adapter.Execute(ctx, wr, binding)
	r.resp = resp
	if err != nil {
		r.err = err
		return r
	}
	if resp == nil {
		// 契约要求 Execute 即便出错也返回非 nil 的 resp（便于归因）；真为 nil
		// 说明适配器破了契约，这比"请求失败"严重得多，单独报出来。
		r.err = errors.New("适配器违反了 Execute 的后置条件：resp 为 nil（契约要求即便失败也带回 StatusCode / Body）")
		return r
	}
	turn, err := s.denorm.Denormalize(resp)
	if err != nil {
		r.err = fmt.Errorf("回程翻译失败: %w", err)
		return r
	}
	r.turn = turn
	return r
}

// ---------------------------------------------------------------------------
// 探测用例（13.3 的 6 个）
// ---------------------------------------------------------------------------

// baseSystemText 是"冻结前缀"的替身：真实框架里它由编译层生成（system +
// 常驻块 + Tools，见 ADR-0015），这里用一段**确定性**生成的长文本代替。
//
// 为什么要有一定长度（而不是 "You are a helpful assistant"）：隐式前缀缓存按
// 单元落盘，前缀太短时 cached_tokens 可能为 0，于是用例 2 的判据
// （cached_tokens > 0）会失败在一个与被测假设无关的原因上。这里的长度（约
// 500 token）远超常见的最小单元长度，且逐字节固定——不用时间戳、不用随机数、
// 不 range map。前缀抖动是缓存的天敌，探测工具自己先不能抖动。
func baseSystemText() string {
	var b strings.Builder
	b.WriteString("你是 Marl 的探测助手。下面是一份虚构的编码规范，请把它当作系统上下文：\n")
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "规则 %02d：任何情况下都先复述问题再回答，不得跳步，不得省略单位。\n", i)
	}
	b.WriteString("回答一律使用中文，且不超过两句话。")
	return b.String()
}

// 构造 Canonical 片段的小工具。Stability 必须与 Kind 匹配（Normalizer 会校验，
// 见 stabilityForKind）——这里显式写出来，而不是靠"默认值碰巧对"。
func systemSeg(text string) wire.Segment {
	return wire.Segment{Kind: wire.SegSystem, Speaker: wire.SpeakerFramework, Content: text, Stability: types.StabilityFrozen}
}

func userSeg(text string) wire.Segment {
	return wire.Segment{Kind: wire.SegTurn, Speaker: wire.SpeakerHuman, Content: text, Stability: types.StabilityStable}
}

func assistantSeg(text string) wire.Segment {
	return wire.Segment{Kind: wire.SegTurn, Speaker: wire.SpeakerAssistant, Content: text, Stability: types.StabilityStable}
}

// getTimeTool 是探测用的工具定义。
//
// 参数 schema 逐字节写死（不用 json.Marshal 一个 map）：ToolDef.Parameters 是
// 冻结前缀的一部分（ADR-0015），map 的键序虽然在 encoding/json 下确定，但
// "参数长什么样"在源码里一眼可见比"跑一次才知道"更有价值。
func getTimeTool() wire.ToolDef {
	return wire.ToolDef{
		Name:        "get_time",
		Description: "获取当前的本地时间。返回格式为 RFC3339 的时间字符串。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"timezone":{"type":"string","description":"IANA 时区名，如 Asia/Shanghai"}},"required":[]}`),
	}
}

// sampling 是本探测统一使用的采样参数。
//
// Temperature / TopP 留零值 = **不发送**（走厂商默认）：阶段 1 的编码契约是
// "零值即不发送"，因此这里无法表达 temperature=0 的贪心解码（已知缺口，
// 需要把 SamplingParams 改成指针才能修，见 EncodeOpenAIChatBody 的注释）。
// 探测报告里不测 temperature 对缓存的影响也是因为这一点：改不了它。
func (s *session) sampling() types.SamplingParams {
	return types.SamplingParams{MaxTokens: s.opt.maxTokens, TimeoutMs: s.opt.timeoutMs}
}

// runCases 依次跑 6 个用例（用例 2 / 6 依赖用例 1 的回复，因此必须串行）。
func (s *session) runCases(ctx context.Context) error {
	system := baseSystemText()
	const (
		askA = "用一句话说明什么是前缀缓存。"
		askB = "再用一句话说明什么是缓存命中率。"
	)

	// ---- 用例 1：普通对话（建立缓存）----
	c1 := s.call(ctx, callSpec{
		name:   "1 普通对话（建立缓存）",
		why:    "建立前缀缓存单元，并确认基线路径（无 tools / 无 thinking / 无 JSON mode）能跑通",
		req:    &wire.CanonicalRequest{Segments: []wire.Segment{systemSeg(system), userSeg(askA)}, Sampling: s.sampling()},
		bucket: s.opt.bucket,
		level:  "off",
	})
	s.reportCase(c1)
	s.checkBaseline(c1)

	// ---- 用例 2：同前缀（验证命中）----
	// 形态照抄官方缓存文档的例一：把上一轮的回复接回历史，再追加一个新问题。
	// 这样请求前缀里包含上一轮"用户输入结束"处的完整缓存单元 → 应当命中。
	segments2 := []wire.Segment{systemSeg(system), userSeg(askA)}
	reply := c1.reply()
	if reply == "" && s.opt.dryRun {
		// dry-run 里没有上一轮的回复（没发请求），但用例 2/6 的真实形态必须
		// 带上它——否则打印出来的请求体少了那条 assistant 消息，"两条请求只差
		// 桶字段"这个对比在 dry-run 下就看不出真实形状。占位文本显式标注来源，
		// 不会被误当成真实测量数据。
		reply = "（dry-run 占位：真实运行时这里是用例 1 的回复，逐字节取自厂商响应）"
	}
	if reply != "" {
		segments2 = append(segments2, assistantSeg(reply))
	}
	segments2 = append(segments2, userSeg(askB))
	c2 := s.call(ctx, callSpec{
		name:   "2 同前缀（验证命中）",
		why:    "同一前缀 + 新问题：验证隐式前缀缓存跨请求命中（cached_tokens > 0）",
		req:    &wire.CanonicalRequest{Segments: segments2, Sampling: s.sampling()},
		bucket: s.opt.bucket,
		level:  "off",
	})
	s.reportCase(c2)
	s.checkCacheHit(c2, c1)

	// ---- 用例 2b（可选）：TTL 探测 ----
	// 13.3 的假设里有"缓存 TTL > 5 分钟"，但 6 个用例里没有等待。用
	// -repeat-gap=5m 显式打开：等一段时间后重发**同一份字节**，仍然命中就说明
	// TTL 覆盖了这个间隔。
	if s.opt.repeatGap > 0 {
		fmt.Printf("\n（-repeat-gap=%s：等待后重发用例 2，验证缓存 TTL）\n", s.opt.repeatGap)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.opt.repeatGap):
		}
		c2b := s.call(ctx, callSpec{
			name:   fmt.Sprintf("2b 同字节重发（TTL 探测，间隔 %s）", s.opt.repeatGap),
			why:    "验证缓存 TTL 覆盖 repeat-gap 这个间隔",
			req:    &wire.CanonicalRequest{Segments: segments2, Sampling: s.sampling()},
			bucket: s.opt.bucket,
			level:  "off",
		})
		s.reportCase(c2b)
		s.checkCacheHit(c2b, c2)
	}

	// ---- 用例 3：带 tools（验证 tool_call）----
	c3 := s.call(ctx, callSpec{
		name: "3 带 tools（验证 tool_call）",
		why:  "验证 tools 字段被接受且模型返回合法 tool_call JSON",
		req: &wire.CanonicalRequest{
			Segments: []wire.Segment{systemSeg(system), userSeg("现在几点？请调用 get_time 工具获取当前时间。")},
			Tools:    []wire.ToolDef{getTimeTool()},
			Sampling: s.sampling(),
		},
		bucket: s.opt.bucket,
		level:  "off",
	})
	s.reportCase(c3)
	s.checkToolCall(c3)

	// ---- 用例 4：开 thinking（逐档位，可重复）----
	// -level-repeats 决定每个档位发几次：单次采样测不出"档位强度"（见
	// checkLevelMeans 的注释），深度轮用它把均值算出来。
	var levelRuns []levelRun
	for _, level := range s.opt.levels {
		for rep := 1; rep <= s.opt.levelRepeats; rep++ {
			name := fmt.Sprintf("4 开 thinking（档位 %s）", level)
			if s.opt.levelRepeats > 1 {
				name = fmt.Sprintf("4 开 thinking（档位 %s，第 %d/%d 次）", level, rep, s.opt.levelRepeats)
			}
			c4 := s.call(ctx, callSpec{
				name: name,
				why:  "验证 reasoning_content 存在且非空；逐档位验证档位表里的每一项",
				req: &wire.CanonicalRequest{
					Segments: []wire.Segment{systemSeg(system), userSeg("把 1 到 20 的数字逐个相加，写出计算过程。")},
					Sampling: s.sampling(),
				},
				bucket: s.opt.bucket,
				level:  level,
			})
			s.reportCase(c4)
			s.checkThinking(c4, level)
			levelRuns = append(levelRuns, levelRun{level: level, r: c4})
		}
	}
	s.checkLevelMeans(levelRuns)

	// ---- 用例 5：JSON mode（验证 response_format）----
	// 文档要求：system 或 user 里必须出现 "json" 字样并给出样例（json_mode.html）。
	c5 := s.call(ctx, callSpec{
		name: "5 JSON mode（验证 response_format）",
		why:  "验证 response_format=json_object 被接受且返回内容是合法 JSON",
		req: &wire.CanonicalRequest{
			Segments: []wire.Segment{
				systemSeg(system),
				userSeg("把这句话解析成 json：请读取 src/main.go 的第 10 行。输出格式示例：{\"action\":\"...\",\"path\":\"...\",\"line\":0}。只输出 json，不要解释。"),
			},
			Sampling:   s.sampling(),
			OutputJSON: true,
		},
		bucket: s.opt.bucket,
		level:  "off",
	})
	s.reportCase(c5)
	s.checkJSONMode(c5)

	// ---- 用例 6：桶隔离/共享（用**本次运行独有的新前缀**测）----
	//
	// 两条请求：6a 在本桶建立这个新前缀的缓存单元；6b 用**逐字节相同的请求**
	// （只有桶字段不同）问另一个桶。判定见 checkBucketIsolation。
	//
	// 为什么不用用例 2 的前缀（初版实现就是这样，结果不可用）：用例 2 的前缀里
	// 含上一轮的回复文本，而缓存是持久的——上一次运行已经把这个前缀写进两个桶
	// 各一份。第二次运行问"另一个桶命中了吗"得到的 cached_tokens>0 来自它自己
	// 上一次的副本，于是同一套代码在两次运行里给出**相反**的结论（第一次"不共享"、
	// 第二次"共享"）。前缀带 nonce 后，两个桶在本轮都是第一次见到它，结论才可归因。
	//
	// nonce 必须放在**最前面**（第一次实现放在末尾，被 checkBucketIsolation 的
	// "前缀是全新的"这一条当场拦下）：隐式前缀缓存按**单元**匹配，且从第一个
	// token 起算。nonce 放在末尾时，前缀开头那 768 token 仍然与本次运行前面几个
	// 用例逐字节相同，于是本桶照样命中 cached=768——"新前缀"根本没建立。
	nonceSystem := "本次探测标记（每次运行都不同）：" + s.opt.nonce + "\n" + system
	nonceSegments := []wire.Segment{systemSeg(nonceSystem), userSeg(askA)}
	c6a := s.call(ctx, callSpec{
		name:   "6a 新前缀（本桶建立缓存）",
		why:    "为桶隔离用例建立一个**两个桶都没见过**的前缀：它的 cached_tokens 必须为 0，否则结论无法归因",
		req:    &wire.CanonicalRequest{Segments: nonceSegments, Sampling: s.sampling()},
		bucket: s.opt.bucket,
		level:  "off",
	})
	s.reportCase(c6a)

	c6b := s.call(ctx, callSpec{
		name:   "6b 同前缀但不同桶（验证缓存桶口径）",
		why:    "不同 user_id 是否共享前缀缓存（决定 10.15 的\"全局公共桶\"热身方案是否可行）",
		req:    &wire.CanonicalRequest{Segments: nonceSegments, Sampling: s.sampling()},
		bucket: s.opt.otherBucket,
		level:  "off",
	})
	s.reportCase(c6b)
	s.checkBucketIsolation(c6a, c6b)

	// ---- 用例 7：缓存单元对齐扫描 ----
	//
	// 厂商文档只说"按固定 token 间隔落盘"，没给间隔是多少（cache.html）。这个
	// 数字对框架很实在：它决定"冻结前缀 + 变化后缀"到底能命中多少、前缀要留
	// 多少余量才划算。
	//
	// 方法：对同一段文本先发**最长**的那份建立缓存，再发它的逐字符截断（每次
	// 只加长一小段）。截断版本是已落盘文本的严格前缀，因此它命中的长度只能是
	// "对齐到单元边界的长度"：
	//
	//	cached_i = floor(prompt_i / 单元) * 单元
	//
	// 于是 cached 会**分段跳变**，而每次跳过的正好是一个单元（前提是每次加长的
	// token 数小于单元）。跳幅就是单元大小——这比"发两三个不同长度的前缀再猜"
	// 可靠得多（后者只能得到"单元整除 cached"这个上界，推不出确切值）。
	//
	// 为什么不用"每组都换新前缀"：那样每组都要建立 + 重发，请求数翻倍，而且
	// 前缀一换就多一个变量（nonce 的字节数会跟着变），可比性下降。
	nonceSystem7 := "本次探测标记（用例 7，每次运行都不同）：" + s.opt.nonce + "\n" + system
	full := nonceSystem7 + unitFiller(1200)
	c7 := s.call(ctx, callSpec{
		name:   "7 建立长前缀（落盘，供截断扫描）",
		why:    "建立一个**全新**的长前缀（cached 必须为 0），后续截断请求只命中它落盘的单元",
		req:    &wire.CanonicalRequest{Segments: []wire.Segment{systemSeg(full), userSeg(askA)}, Sampling: s.sampling()},
		bucket: s.opt.bucket,
		level:  "off",
	})
	s.reportCase(c7)

	// 截断扫描：从 full 的 55% 起每次多留 40 个字符（约 25 token），共 10 次。
	// 起点不取 0：太短的前缀落在第一个单元之前，只会得到一串 0，对推断跳幅没有
	// 贡献（那些 0 仍会出现在报告里，作为"命中需要足够的长度"的证据）。
	fullRunes := []rune(full)
	start := len(fullRunes) * 55 / 100
	var unitRuns []*caseResult
	for i := 0; i < 10; i++ {
		cut := start + i*40
		if cut > len(fullRunes) {
			cut = len(fullRunes)
		}
		r := s.call(ctx, callSpec{
			name:   fmt.Sprintf("7-%02d 截断前缀（%d 字符）", i+1, cut),
			why:    "观测 cached 是否按单元跳变：跳幅 = 缓存单元的 token 间隔",
			req:    &wire.CanonicalRequest{Segments: []wire.Segment{systemSeg(string(fullRunes[:cut])), userSeg(askA)}, Sampling: s.sampling()},
			bucket: s.opt.bucket,
			level:  "off",
		})
		s.reportCase(r)
		unitRuns = append(unitRuns, r)
	}
	s.checkCacheUnitAlignment(c7, unitRuns)

	return nil
}

// unitFiller 生成**逐字节确定**的填充文本（长度约为 n 个字符）。
//
// 用固定的中文行而不是随机串：前缀必须逐字节可复现，否则"缓存单元"的实验每次
// 都在测不同的前缀。长度用字符数近似控制——token 数由厂商回传的 prompt_tokens
// 给出，本用例**不需要**提前算准它（这正是它能成立的原因）。
func unitFiller(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	runes := 0
	for i := 0; runes < n; i++ {
		line := fmt.Sprintf("填充行 %03d：本行只用于拉长前缀，内容无意义，且逐字节固定。\n", i)
		b.WriteString(line)
		runes += len([]rune(line))
	}
	return b.String()
}

// levelRun 是一次档位采样（用例 4 的重复单元）。
type levelRun struct {
	level string
	r     *caseResult
}

// ---------------------------------------------------------------------------
// 证据读取（打印、判定、写报告都只用这些方法）
// ---------------------------------------------------------------------------

// usage 返回本次调用的用量；nil 表示**用量未知**（不是"用量为 0"）。
//
// 两个来源，先到先得（同一份原始字节的两种解析路径）：
//   - WireResponse.UsageRaw：适配器收到响应时顺手解析的（生产路径，bestEffortUsage）；
//   - Outcome.Usage：Denormalizer 解析响应体得到的（原始请求路径只有这一个——
//     rawCall 不走适配器，因此没有 UsageRaw）。
//
// 为什么允许回退而不是在 rawCall 里自己解析：两份数据来自同一份字节、同一个
// mapOpenAIChatUsage，所以回退不引入第二个口径；而让探测工具另写一份 usage 解析
// 才是真危险（它会与 Denormalizer 的字段名/别名规则悄悄漂移）。
func (r *caseResult) usage() *types.TokenUsage {
	if r.resp != nil && r.resp.UsageRaw != nil {
		return r.resp.UsageRaw
	}
	// 回退到 Denormalizer 解析出来的 usage：原始请求路径（用例 8/9/10）没有走
	// 适配器，因此 UsageRaw 为空——适配器在收到响应时顺手填它，而 rawCall 只
	// 构造了 StatusCode/Body/Latency。两份数据来自**同一份原始字节**，所以回退
	// 不会引入第二个口径；反过来，让探测工具自己再写一份 usage 解析才是真危险
	// （它会与 Denormalizer 的字段名/别名规则悄悄漂移）。
	if r.turn != nil {
		for _, o := range r.turn.Outcomes {
			if o.Usage != nil {
				return o.Usage
			}
		}
	}
	return nil
}

// cachedTokens 返回命中缓存的输入 token 数。用量未知时返回 0（调用方必须先用
// usage()==nil 判断"未知"，不能把未知当 0——见 WireResponse.UsageRaw 的契约）。
func (r *caseResult) cachedTokens() int {
	if u := r.usage(); u != nil {
		return u.CacheReadTokens
	}
	return 0
}

func (r *caseResult) promptTokens() int {
	if u := r.usage(); u != nil {
		return u.PromptTokens
	}
	return 0
}

// completionTokens 返回可见输出 token 数（用量未知时为 0）。
func (r *caseResult) completionTokens() int {
	if u := r.usage(); u != nil {
		return u.CompletionTokens
	}
	return 0
}

// reasoningTokens 返回思维链 token 数（用量未知时为 0）。
func (r *caseResult) reasoningTokens() int {
	if u := r.usage(); u != nil {
		return u.ReasoningTokens
	}
	return 0
}

// reply 返回第一个可见回复（本线路每次只有一条 assistant 消息，因此就是它）。
func (r *caseResult) reply() string {
	if r.turn == nil {
		return ""
	}
	for _, o := range r.turn.Outcomes {
		if o.Reply != "" {
			return o.Reply
		}
	}
	return ""
}

// reasoning 返回思维链（拼接所有 Reasoning 片段：本线路只有一个字段）。
func (r *caseResult) reasoning() string {
	if r.turn == nil {
		return ""
	}
	var b strings.Builder
	for _, o := range r.turn.Outcomes {
		if o.Reasoning != nil {
			b.WriteString(o.Reasoning.Content)
		}
	}
	return b.String()
}

func (r *caseResult) toolCalls() []types.ToolCall {
	if r.turn == nil {
		return nil
	}
	var out []types.ToolCall
	for _, o := range r.turn.Outcomes {
		out = append(out, o.ToolCalls...)
	}
	return out
}

// errorClasses 收集本次 turn 里出现过的错误类别（去重，保持首次出现的顺序）。
//
// 存在的理由：厂商错误**不进** error 返回值（ADR-0016），因此"这次失败了吗"
// 只能从 OutcomeSignals.ErrorClass 读。探测报告必须显示它，否则一次 429 会在
// 报告里表现为"回复为空"——而那是完全不同的结论。
func (r *caseResult) errorClasses() []wire.ErrorClass {
	if r.turn == nil {
		return nil
	}
	var out []wire.ErrorClass
	seen := map[wire.ErrorClass]bool{}
	for _, o := range r.turn.Outcomes {
		if c := o.Signals.ErrorClass; c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

func (r *caseResult) ready() bool {
	return r.turn != nil && r.turn.Ready()
}

func (r *caseResult) latency() time.Duration {
	if r.resp == nil {
		return time.Since(r.startedAt)
	}
	return r.resp.Latency
}

func (r *caseResult) statusCode() int {
	if r.resp == nil {
		return 0
	}
	return r.resp.StatusCode
}

// finishReason 从原始响应体里取 finish_reason。
//
// 为什么不从 WireTurn 读：finish_reason 目前只落进 LogEntry.Meta（见
// denormalizer.go 的 replyLogEntry），WireTurn 上没有这个字段——而探测报告要
// 显示它（"length 截断"与"stop"在报告里必须能区分）。这里就地解析原始响应体，
// 与"厂商原始字节是审计证据"一致：报告读的是厂商给的字节，不是我们的翻译结果。
func (r *caseResult) finishReason() string {
	if r.resp == nil || len(r.resp.Body) == 0 {
		return ""
	}
	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(r.resp.Body, &parsed); err != nil || len(parsed.Choices) == 0 {
		return ""
	}
	return parsed.Choices[0].FinishReason
}

// ---------------------------------------------------------------------------
// 打印
// ---------------------------------------------------------------------------

const rule = "──────────────────────────────────────────────────────────────────────────────"

// printHeader 打印本次运行的配置。
//
// 配置必须完整打印：探测报告的结论只在"这份配置"下成立（模型名、接入点、
// 桶字段名、超时、上限、档位表都影响结果）。
func (s *session) printHeader() {
	fmt.Println(rule)
	fmt.Println("Marl 阶段 1 探测（设计文档 13.3）：DeepSeek openai_chat 线路 + 隐式前缀缓存")
	fmt.Println(rule)
	fmt.Printf("接入点        : %s（%s）\n", s.opt.endpoint, s.opt.baseURL)
	fmt.Printf("模型          : %s → %s\n", s.opt.modelID, s.opt.remote)
	fmt.Printf("缓存桶字段    : %s（另一桶 %s）\n", s.opt.bucketField, s.opt.otherBucket)
	fmt.Printf("缓存桶        : %s\n", s.opt.bucket)
	fmt.Printf("用例 6 前缀标记: %s（保证两个桶都是第一次见到该前缀）\n", s.opt.nonce)

	fmt.Printf("单次超时/上限 : %d ms / %d tokens\n", s.opt.timeoutMs, s.opt.maxTokens)
	fmt.Printf("thinking 档位 : %v（thinking_control=level）\n", s.opt.levels)
	if s.opt.levelRepeats > 1 {
		fmt.Printf("档位重复次数  : %d（用例 4 每档发 %d 次，用于判断档位强度的单调性）\n", s.opt.levelRepeats, s.opt.levelRepeats)
	}
	fmt.Printf("第二个模型    : %s（用例 8：跨模型缓存 + 档位支持）\n", s.opt.remotePro)
	fmt.Println("契约探测      : 用例 8/9/10 用**手工构造的原始请求体**（绕过编码器，见 contract.go）")
	if s.opt.contractOnly {
		fmt.Println("模式          : -contract-only（只跑契约探测 8/9/10，跳过 1~7 主用例）")
	}
	fmt.Printf("Normalize 策略: %+v\n", s.norm.Policy())
	fmt.Printf("密钥来源      : 环境变量 %s\n", s.opt.apiKeyEnv)
	if s.opt.dryRun {
		fmt.Println("模式          : -dry-run（只编码并打印请求体，不发网络请求）")
	} else {
		fmt.Println("模式          : 真跑（会消耗 token，约几分钱）")
	}
	fmt.Println(rule)
}

// reportCase 打印一个用例的全部证据，并把证据留在 session 里（写报告用）。
func (s *session) reportCase(r *caseResult) {
	s.results = append(s.results, r)
	fmt.Printf("\n%s\n用例 %s\n  验证点：%s\n  桶=%s  档位=%s\n", rule, r.spec.name, r.spec.why, r.spec.bucket, r.spec.level)

	if len(r.requestBytes) > 0 {
		fmt.Println("[发送的请求体]（实发字节 = 下面这份 JSON 的紧凑形式）")
		fmt.Println(indentJSON(r.requestBytes))
	}
	if r.err != nil {
		fmt.Printf("[错误] %v\n", r.err)
		if r.resp != nil && r.resp.StatusCode != 0 {
			fmt.Printf("[原始响应] status=%d body=%s\n", r.resp.StatusCode, truncate(string(r.resp.Body), 600))
		}
		return
	}
	if r.skipped {
		fmt.Println("[响应] （dry-run：未发请求）")
		return
	}

	fmt.Printf("[响应] status=%d 耗时=%s finish_reason=%q\n", r.statusCode(), r.latency().Round(time.Millisecond), r.finishReason())
	if u := r.usage(); u != nil {
		fmt.Printf("usage: prompt=%d completion=%d reasoning=%d cache_read=%d cache_write=%d image=%d\n",
			u.PromptTokens, u.CompletionTokens, u.ReasoningTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ImageTokens)
	} else {
		fmt.Println("usage: （厂商未回传 usage —— 用量未知，不等于 0）")
	}
	if r.turn == nil {
		fmt.Println("outcomes: （没有可翻译的响应体）")
		return
	}
	fmt.Printf("outcomes: %d  ready=%v\n", len(r.turn.Outcomes), r.ready())
	for i, o := range r.turn.Outcomes {
		switch {
		case o.Reasoning != nil:
			fmt.Printf("  [%d] reasoning（%d 字符）: %s\n", i, len([]rune(o.Reasoning.Content)), truncate(o.Reasoning.Content, 160))
		case len(o.ToolCalls) > 0:
			for _, c := range o.ToolCalls {
				fmt.Printf("  [%d] tool_call id=%s name=%s arguments=%s\n", i, c.ID, c.Name, truncate(string(c.Arguments), 200))
			}
		case o.Reply != "":
			fmt.Printf("  [%d] reply（%d 字符）: %s\n", i, len([]rune(o.Reply)), truncate(o.Reply, 160))
		default:
			fmt.Printf("  [%d] （空 outcome）\n", i)
		}
	}
	if len(r.degradations) > 0 {
		fmt.Printf("降级: %d 条\n", len(r.degradations))
		for _, d := range r.degradations {
			fmt.Printf("  - %s from=%q to=%q：%s\n", d.Kind, d.From, d.To, d.Reason)
		}
	}
	if classes := r.errorClasses(); len(classes) > 0 {
		fmt.Printf("错误分类（厂商错误走这里，不走 error 返回值）: %v\n", classes)
	}
}

// indentJSON 把实发字节美化后打印。
//
// 解析失败时原样打印：这份字节是**证据**，美化失败不该让它消失。
func indentJSON(body []byte) string {
	var buf map[string]any
	if err := json.Unmarshal(body, &buf); err != nil {
		return string(body)
	}
	pretty, err := json.MarshalIndent(buf, "", "  ")
	if err != nil {
		return string(body)
	}
	return string(pretty)
}

// truncate 按**字符**（不是字节）截断，避免把多字节字符切一半打出乱码。
func truncate(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + fmt.Sprintf("…（共 %d 字符，已截断）", len(rs))
}

// ---------------------------------------------------------------------------
// 判定
// ---------------------------------------------------------------------------

// status 是判定的三档。
//
// 为什么要 WARN 而不是只有通过/失败：探测里有几项**无法自动判定**（模型是否
// 恰好调用了工具、不同桶是否共享缓存），它们的正确处置是"记录现象 + 人做决定"。
// 把它们塞进 FAIL 会让门禁噪声化（真故障被淹没），塞进 PASS 会让报告撒谎。
type status string

const (
	statusPass status = "PASS"
	statusFail status = "FAIL"
	statusWarn status = "WARN"
	statusSkip status = "SKIP"
)

type check struct {
	caseName string
	what     string
	st       status
	detail   string
}

// addCheck 记录并打印一条判定（打印紧跟用例输出，读起来有上下文）。
func (s *session) addCheck(caseName, what string, st status, detail string) {
	s.checks = append(s.checks, check{caseName: caseName, what: what, st: st, detail: detail})
	fmt.Printf("  %-4s %s：%s\n", st, what, detail)
}

// checkBaseline 判定用例 1：基线路径能跑通，且 off 档位真的关掉了思考。
func (s *session) checkBaseline(r *caseResult) {
	name := r.spec.name
	if r.skipped {
		s.addCheck(name, "基线请求跑通", statusSkip, "dry-run：未发请求")
		return
	}
	if r.err != nil {
		s.addCheck(name, "基线请求跑通", statusFail, r.err.Error())
		return
	}
	if code := r.statusCode(); code < 200 || code >= 300 {
		s.addCheck(name, "基线请求跑通", statusFail,
			fmt.Sprintf("HTTP %d（非 2xx；厂商错误分类见上面的 outcomes 行）：%s", code, truncate(string(r.resp.Body), 300)))
		return
	}
	if classes := r.errorClasses(); len(classes) > 0 {
		s.addCheck(name, "基线请求跑通", statusFail, fmt.Sprintf("响应里带了错误分类 %v（厂商在 2xx 里报错）", classes))
		return
	}
	if r.reply() == "" {
		s.addCheck(name, "基线请求跑通", statusFail, fmt.Sprintf("没有可见回复（ready=%v，finish_reason=%q）", r.ready(), r.finishReason()))
		return
	}
	s.addCheck(name, "基线请求跑通", statusPass, fmt.Sprintf("有可见回复（%d 字符，耗时 %s）", len([]rune(r.reply())), r.latency().Round(time.Millisecond)))

	// off 档位的实测：框架的 off 在 level 控制下映射成 reasoning_effort=none
	// （ADR-0020），这里验证厂商是否真的照做——如果没关掉，后续所有"关思考"
	// 的请求都在偷偷花思维链的钱，而账面上看不出来。
	if r.reasoning() != "" {
		s.addCheck(name, "off 档位真的关掉思考", statusWarn, fmt.Sprintf(
			"档位 off 仍返回 reasoning_content（%d 字符）：reasoning_effort=none 不足以关闭思考。"+
				"下一步：在 level 控制下同时发 thinking:{type:disabled}（ADR-0020 的备选方案），并把结论写进探测报告",
			len([]rune(r.reasoning()))))
		return
	}
	s.addCheck(name, "off 档位真的关掉思考", statusPass, "reasoning_content 为空（off → reasoning_effort=none 生效）")
}

// checkCacheHit 判定"缓存是否命中"（13.3 的核心交付检查）。
//
// 这是整个框架经济性的地基：命中率不成立，后面的成本模型、阶梯打分、热身策略
// 全都要重估（13.3 的"如果这一步发现缓存假设不成立，立即暂停"）。
func (s *session) checkCacheHit(r, prev *caseResult) {
	name := r.spec.name
	if r.skipped {
		s.addCheck(name, "缓存命中（cached_tokens > 0）", statusSkip, "dry-run：未发请求")
		return
	}
	if r.err != nil {
		s.addCheck(name, "缓存命中（cached_tokens > 0）", statusFail, r.err.Error())
		return
	}
	if prev != nil && prev.err != nil {
		s.addCheck(name, "缓存命中（cached_tokens > 0）", statusFail,
			"前一次请求失败，前缀缓存没有建立起来，本用例的结论不成立（先修前一个用例）")
		return
	}
	if u := r.usage(); u == nil {
		s.addCheck(name, "缓存命中（cached_tokens > 0）", statusFail, "厂商未回传 usage：用量未知，无法判定（不是 0）")
		return
	}
	if r.cachedTokens() > 0 {
		s.addCheck(name, "缓存命中（cached_tokens > 0）", statusPass, fmt.Sprintf(
			"cached_tokens=%d，prompt_tokens=%d（命中率 %.1f%%，耗时 %s）",
			r.cachedTokens(), r.promptTokens(), 100*float64(r.cachedTokens())/float64(max1(r.promptTokens())), r.latency().Round(time.Millisecond)))
		return
	}
	s.addCheck(name, "缓存命中（cached_tokens > 0）", statusFail,
		"cached_tokens=0：隐式前缀缓存没有命中。排查方向（按可能性排序）："+
			"(1) 两次请求的前缀不是逐字节相同（看两次打印的请求体）；"+
			"(2) 前缀短于厂商的最小缓存单元；"+
			"(3) 该接入点/账号未启用缓存；"+
			"(4) 缓存 TTL 短于两次请求的间隔")
}

// max1 把 0 换成 1，避免命中率计算里除零。
func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// checkToolCall 判定用例 3。
func (s *session) checkToolCall(r *caseResult) {
	name := r.spec.name
	if r.skipped {
		s.addCheck(name, "返回合法 tool_call JSON", statusSkip, "dry-run：未发请求")
		return
	}
	if r.err != nil {
		s.addCheck(name, "返回合法 tool_call JSON", statusFail, r.err.Error())
		return
	}
	if classes := r.errorClasses(); len(classes) > 0 {
		s.addCheck(name, "返回合法 tool_call JSON", statusFail, fmt.Sprintf("响应里带了错误分类 %v", classes))
		return
	}
	calls := r.toolCalls()
	if len(calls) == 0 {
		// 阶段 1 不发 tool_choice（13.3 的参数清单里没有它），因此"模型选择
		// 直接回答"是合法行为，不能判成能力缺失——但也不能算通过。
		s.addCheck(name, "返回合法 tool_call JSON", statusWarn,
			fmt.Sprintf("模型没有返回 tool_calls（回复：%s）。阶段 1 不发 tool_choice，无法强制调用；"+
				"这**不是**能力缺失的结论——人工复核或调整提示词后重跑", truncate(r.reply(), 120)))
		return
	}
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Name)
		if !json.Valid(c.Arguments) {
			s.addCheck(name, "返回合法 tool_call JSON", statusFail,
				fmt.Sprintf("tool_call %q 的 arguments 不是合法 JSON：%s", c.Name, truncate(string(c.Arguments), 200)))
			return
		}
		if c.ID == "" {
			s.addCheck(name, "返回合法 tool_call JSON", statusWarn,
				fmt.Sprintf("tool_call %q 没有 id：工具结果无法配对（后续轮次的 tool 消息会缺 tool_call_id）", c.Name))
			return
		}
	}
	s.addCheck(name, "返回合法 tool_call JSON", statusPass, fmt.Sprintf("返回 %d 个 tool_call：%v（arguments 均为合法 JSON）", len(calls), names))
}

// checkThinking 判定用例 4 的单个档位。
func (s *session) checkThinking(r *caseResult, level string) {
	name := r.spec.name
	if r.skipped {
		s.addCheck(name, "reasoning_content 存在且非空", statusSkip, "dry-run：未发请求")
		return
	}
	if r.err != nil {
		s.addCheck(name, "reasoning_content 存在且非空", statusFail, r.err.Error())
		return
	}
	if classes := r.errorClasses(); len(classes) > 0 {
		s.addCheck(name, "reasoning_content 存在且非空", statusFail,
			fmt.Sprintf("响应里带了错误分类 %v（档位 %q 可能不被支持，应写进 caps_override）", classes, level))
		return
	}
	got := len([]rune(r.reasoning()))
	if level == "none" || level == "off" {
		if got > 0 {
			s.addCheck(name, "关闭档位不返回思维链", statusWarn, fmt.Sprintf(
				"档位 %q 仍返回 reasoning_content（%d 字符）：该取值没有关掉思考。"+
					"下一步：确认厂商侧关闭思考的正确取值（可能是 thinking:{type:disabled}），并据此改 ADR-0020 的映射", level, got))
			return
		}
		s.addCheck(name, "关闭档位不返回思维链", statusPass, fmt.Sprintf("档位 %q：reasoning_content 为空", level))
		return
	}
	if got == 0 {
		s.addCheck(name, "reasoning_content 存在且非空", statusFail, fmt.Sprintf(
			"档位 %q 没有返回 reasoning_content（finish_reason=%q）：能力声明与实测不符——"+
				"应按 10.13 写进 caps_override 的 thinking_levels", level, r.finishReason()))
		return
	}
	s.addCheck(name, "reasoning_content 存在且非空", statusPass, fmt.Sprintf(
		"档位 %q：reasoning_content %d 字符，reasoning_tokens=%d，finish_reason=%q",
		level, got, r.reasoningTokens(), r.finishReason()))
}

// checkJSONMode 判定用例 5。
func (s *session) checkJSONMode(r *caseResult) {
	name := r.spec.name
	if r.skipped {
		s.addCheck(name, "返回内容是合法 JSON", statusSkip, "dry-run：未发请求")
		return
	}
	if r.err != nil {
		s.addCheck(name, "返回内容是合法 JSON", statusFail, r.err.Error())
		return
	}
	if classes := r.errorClasses(); len(classes) > 0 {
		s.addCheck(name, "返回内容是合法 JSON", statusFail, fmt.Sprintf("响应里带了错误分类 %v（response_format 可能不被接受）", classes))
		return
	}
	sent := bytes.Contains(r.requestBytes, []byte(`"response_format":{"type":"json_object"}`))
	if r.reply() == "" {
		s.addCheck(name, "返回内容是合法 JSON", statusFail, fmt.Sprintf(
			"没有可见回复（finish_reason=%q）；JSON mode 有概率返回空 content（json_mode.html 明说），需重跑确认", r.finishReason()))
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(r.reply()), &obj); err != nil {
		s.addCheck(name, "返回内容是合法 JSON", statusFail,
			fmt.Sprintf("返回内容不是合法 JSON（请求体带 response_format=%v）：%s", sent, truncate(r.reply(), 200)))
		return
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s.addCheck(name, "返回内容是合法 JSON", statusPass, fmt.Sprintf("请求体带 response_format=%v，返回内容解析为 JSON 对象（键 %v）", sent, keys))
}

// checkBucketIsolation 判定用例 6：先确认"两次请求只差桶字段"，再记录观测。
//
// prime 是**本桶**用新前缀发的请求（应当 cached=0：两个桶都是第一次见到它），
// probe 是另一个桶发的逐字节相同的请求。结论只在 prime.cached==0 时才可归因：
//   - probe.cached > 0 → 另一个桶命中了**本桶刚建立**的缓存 → 跨桶共享；
//   - probe.cached == 0 → 桶隔离。
//
// prime.cached > 0 说明前缀并非全新（nonce 没起作用或前缀与历史请求重合），
// 此时无论 probe 的结果如何都不能下结论——这正是初版实现（沿用用例 2 的固定
// 前缀）在两次运行里给出相反结论的原因。
func (s *session) checkBucketIsolation(prime, probe *caseResult) {
	name := probe.spec.name
	if probe.skipped {
		s.addCheck(name, "两次请求只差桶字段", statusSkip, "dry-run：未发请求")
		return
	}
	if prime.err != nil || probe.err != nil {
		s.addCheck(name, "两次请求只差桶字段", statusFail, fmt.Sprintf("请求失败：prime=%v probe=%v", prime.err, probe.err))
		return
	}
	if !samePrefixExceptBucket(prime.requestBytes, probe.requestBytes) {
		s.addCheck(name, "两次请求只差桶字段", statusFail,
			"用例 6a 与 6b 的请求体在桶字段之外**也有差异**：这一用例测到的差异无法归因到桶（先修探测用例，否则结论无效）")
		return
	}
	s.addCheck(name, "两次请求只差桶字段", statusPass, "用例 6a 与 6b 的请求体除桶字段外逐字节相同")

	if prime.cachedTokens() > 0 {
		s.addCheck(name, "前缀是全新的（结论可归因的前提）", statusWarn, fmt.Sprintf(
			"**前提不成立**：本桶用新前缀的请求 cached_tokens=%d（应当为 0）——前缀并非全新，"+
				"另一个桶的命中可能来自它自己更早的副本，本用例的结论不可用（检查 nonce 是否被写进前缀）",
			prime.cachedTokens()))
		return
	}
	s.addCheck(name, "前缀是全新的（结论可归因的前提）", statusPass,
		"本桶用带 nonce 的新前缀请求 cached_tokens=0（两个桶都没见过它）")

	// 13.3 要求把这一观测写进探测报告（决定 10.15 的"全局公共桶"是否可行）。
	if probe.cachedTokens() > 0 {
		s.addCheck(name, "不同 user_id 是否共享前缀缓存", statusWarn, fmt.Sprintf(
			"**共享**：另一个桶命中了本桶刚建立的新前缀（cached_tokens=%d）→ 10.15 的\"全局公共桶\"热身方案可行，"+
				"公共前缀（system + tools）只需缓存一份；请在探测报告里记录这一结论",
			probe.cachedTokens()))
		return
	}
	s.addCheck(name, "不同 user_id 是否共享前缀缓存", statusWarn,
		"**不共享**：另一个桶对**本桶刚建立的新前缀**cached_tokens=0 → 桶确实隔离，"+
			"全局公共桶方案不成立，按 10.15 接受\"N 个 Agent 重复存储公共段\"的代价；请在探测报告里记录这一结论")
}

// samePrefixExceptBucket 报告两份请求体在去掉桶字段后是否逐字节相同。
func samePrefixExceptBucket(a, b []byte) bool {
	return sameBytesExcept(a, b, wire.DefaultBucketField, legacyBucketFieldName)
}

// legacyBucketFieldName 是设计文档 §10.15 写的那个字段名（已被 §2 第 2 条改成
// user_id）。它在这里只有一个用途：比较"只差桶字段"时把两个可能的字段名都摘掉。
const legacyBucketFieldName = "user"

// sameBytesExcept 报告两份请求体在去掉指定顶层键之后是否逐字节相同。
//
// 为什么用 map 再序列化比较：请求体的字段顺序在编码器里是固定的，但这里要比的
// 是"语义上的同一份请求"，用 map 比较可以容忍"以后调了字段顺序"这类无关变化，
// 同时 json.Marshal 对 map 键排序，比较结果仍然是确定的。
//
// 调用方必须显式给出要摘掉的键（而不是"只差桶"或"只差模型"各写一份）：判定的
// 前提是"差异只在预期的那一处"，把预期写在调用点上，评审时一眼能看出这一组
// 比较允许什么差异。
func sameBytesExcept(a, b []byte, keys ...string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	ma, mb := map[string]any{}, map[string]any{}
	if json.Unmarshal(a, &ma) != nil || json.Unmarshal(b, &mb) != nil {
		return false
	}
	for _, k := range keys {
		delete(ma, k)
		delete(mb, k)
	}
	ba, err1 := json.Marshal(ma)
	bb, err2 := json.Marshal(mb)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ba, bb)
}

// checkLevelMeans 汇总用例 4 的重复采样，判断档位强度的单调性（报告 §3.8）。
//
// 为什么必须重复：单次采样下"max 比 high 短"既可能是档位效应，也可能是同档位
// 方差（思维链长度随问题表述与采样波动很大）。N 次取均值才能把两者分开。
//
// 判据（按框架已知的强度序 none < low < high < max）：
//   - 均值单调不减 → PASS；
//   - 出现下降 → WARN，并打印逐档均值。它不影响框架正确性（档位仍然"生效"），
//     但会让按档位做成本估算（reasoning_per_mtok）失去依据，因此必须记录。
//
// **两种结果都只是"这一轮的观测"**（实测教训）：轮 5（3 次）均值单调，轮 7（同命令、同样 3 次）
// 均值序反过来（low 775 > high 725）。所以这里的 PASS/WARN 都**不能**单独作为档位强度的证据——
// 判据要带重复次数与极差，见报告 §3.8。
//
// 未知档位名不参与比较：把厂商别名（minimal/xhigh/ultra…）塞进档位表是我们
// 刻意不做的（报告 §3.7），这里也不替它猜顺序。
func (s *session) checkLevelMeans(runs []levelRun) {
	const what = "档位强度的单调性（reasoning_tokens 均值）"
	if len(runs) == 0 {
		return
	}
	type agg struct{ sum, n, min, max int }
	byLevel := map[string]*agg{}
	var order []string
	for _, run := range runs {
		if run.r.skipped {
			continue
		}
		a := byLevel[run.level]
		if a == nil {
			a = &agg{}
			byLevel[run.level] = a
			order = append(order, run.level)
		}
		if run.r.usage() != nil {
			v := run.r.reasoningTokens()
			a.sum += v
			a.n++
			if a.n == 1 || v < a.min {
				a.min = v
			}
			if a.n == 1 || v > a.max {
				a.max = v
			}
		}
	}
	if len(order) == 0 {
		s.addCheck("4 开 thinking（汇总）", what, statusSkip, "dry-run：未发请求")
		return
	}

	rank := map[string]int{"none": 0, "off": 0, "low": 1, "high": 2, "max": 3}
	means := map[string]float64{}
	var parts, known []string
	for _, lv := range order {
		a := byLevel[lv]
		if a.n == 0 {
			parts = append(parts, lv+"=usage 缺失")
			continue
		}
		means[lv] = float64(a.sum) / float64(a.n)
		parts = append(parts, fmt.Sprintf("%s=%.0f（%d 次，极差 %d~%d）", lv, means[lv], a.n, a.min, a.max))
		if _, ok := rank[lv]; ok {
			known = append(known, lv)
		}
	}
	detail := "各档 reasoning_tokens 均值：" + strings.Join(parts, "，")
	sort.SliceStable(known, func(i, j int) bool { return rank[known[i]] < rank[known[j]] })

	var drops []string
	for i := 1; i < len(known); i++ {
		lo, hi := known[i-1], known[i]
		mlo, ok1 := means[lo]
		mhi, ok2 := means[hi]
		if !ok1 || !ok2 {
			continue
		}
		if mhi < mlo {
			drops = append(drops, fmt.Sprintf("%s(%.0f) > %s(%.0f)", lo, mlo, hi, mhi))
		}
	}
	if len(drops) == 0 {
		s.addCheck("4 开 thinking（汇总）", what, statusPass, detail+
			"；在强度序 none<low<high<max 上单调不减。**但均值序本身不稳定**：两次各 3 次的独立运行"+
			"（轮 5/轮 7）给出过相反的 low/high 顺序，且同档位极差与档位间差值同量级 → 按档位做精细"+
			"成本估算不可行（只能给量级）。报强度结论时必须带上重复次数与极差")
		return
	}
	s.addCheck("4 开 thinking（汇总）", what, statusWarn, detail+fmt.Sprintf(
		"；**不单调**：%s。后果：按档位做成本估算（reasoning_per_mtok）失去依据；"+
			"注意\"单调\"同样可能只是这一轮的巧合（轮 5 单调、轮 7 不单调，同一命令），"+
			"因此要判断强度序需要**至少两轮各 ≥3 次**（或单轮 ≥6 次），并同时报出极差", strings.Join(drops, "、")))
}

// checkCacheUnitAlignment 判定用例 7：从截断扫描里读出缓存单元的 token 间隔。
//
// 推理链（每一环在报告里都可见）：
//  1. 截断版本是已落盘文本的严格前缀 → 命中的长度只能是"对齐到单元边界"的长度；
//  2. 于是 cached 随前缀加长而**分段跳变**，跳幅是单元的整数倍；
//  3. 当每次加长的 token 数小于单元时，跳幅**就是**单元本身。
//
// 判定：
//   - 前提（建立请求 cached=0）被破坏 → FAIL：那意味着"新前缀"不新，整组数据不可用；
//   - 所有非零跳幅相同 → PASS，那个跳幅就是单元（本次最直接的证据）；
//   - 跳幅有多个取值 → WARN，打印它们的最大公约数作为单元**上界**（[推断]）；
//   - cached 恒为 0 → WARN：前缀太短或没有落盘，本用例什么也没测到。
//
// 为什么单元大小本身不判 FAIL：它是厂商实现细节，测不出来不影响框架正确性，
// 只影响"前缀要留多少余量"的经验值。
func (s *session) checkCacheUnitAlignment(established *caseResult, runs []*caseResult) {
	const what = "缓存单元对齐（cached 的分段跳幅 = 单元 token 间隔）"
	name := established.spec.name
	if established.skipped {
		s.addCheck(name, what, statusSkip, "dry-run：未发请求")
		return
	}
	if established.err != nil {
		s.addCheck(name, what, statusFail, fmt.Sprintf("建立请求失败：%v", established.err))
		return
	}
	if established.cachedTokens() > 0 {
		s.addCheck(name, what, statusFail, fmt.Sprintf(
			"**前提不成立**：建立请求用带 nonce 的新前缀却 cached_tokens=%d（应当为 0）——"+
				"截断扫描命中的可能不是本次建立的前缀，整组数据不可用", established.cachedTokens()))
		return
	}

	var parts []string
	var cacheds []int
	maxResidue := 0
	for _, r := range runs {
		if r.err != nil || r.usage() == nil {
			parts = append(parts, "（一次失败/无 usage）")
			continue
		}
		cacheds = append(cacheds, r.cachedTokens())
		if d := r.promptTokens() - r.cachedTokens(); d > maxResidue {
			maxResidue = d
		}
		parts = append(parts, fmt.Sprintf("%d/%d", r.promptTokens(), r.cachedTokens()))
	}
	detail := "逐次 (prompt/cached)：" + strings.Join(parts, " ")
	if len(cacheds) == 0 {
		s.addCheck(name, what, statusWarn, detail+"；没有任何一次拿到 usage，无法推断单元")
		return
	}

	var jumps []int
	last := -1
	maxCached := 0
	for _, c := range cacheds {
		if c > maxCached {
			maxCached = c
		}
		if last >= 0 && c != last {
			jumps = append(jumps, c-last)
		}
		last = c
	}
	if maxCached == 0 {
		s.addCheck(name, what, statusWarn, detail+"；cached 恒为 0：截断前缀没有命中任何已落盘单元（前缀太短或未落盘）")
		return
	}
	if len(jumps) == 0 {
		s.addCheck(name, what, statusWarn, detail+fmt.Sprintf(
			"；cached 恒为 %d（没有跨过任何单元边界）——把扫描的步长调大或起点后移再测", last))
		return
	}
	g := jumps[0]
	same := true
	for _, j := range jumps[1:] {
		g = gcdInt(g, j)
		if j != jumps[0] {
			same = false
		}
	}
	if same {
		s.addCheck(name, what, statusPass, detail+fmt.Sprintf(
			"；跳幅恒为 %d → 缓存单元的端点间隔 = %d token（[推断] 本次最直接的证据，请写进报告）。"+
				"注意：cached **不是** floor(prompt/单元)*单元（最大残差 %d > %d），"+
				"因此\"把前缀补齐到单元倍数\"不是有效的优化手段——命中取决于厂商实际落盘了哪些单元端点",
			jumps[0], jumps[0], maxResidue, jumps[0]))
		return
	}
	s.addCheck(name, what, statusWarn, detail+fmt.Sprintf(
		"；跳幅 %v 不一致，最大公约数 %d = 单元的上界（[推断] 单元是 %d 的约数；"+
			"要定死它需要步长明显小于单元的扫描）。最大残差 %d",
		jumps, g, g, maxResidue))
}

// gcdInt 求两个非负整数的最大公约数（欧几里得算法）。
// 只用于探测报告里"单元大小是 cached 跳幅的公约数"这类算术，不做错误处理：
// 负数/零不会来自本文件的调用点（跳幅恒为正）。
func gcdInt(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// sameBytesExceptMessageFields 比较两份请求体，忽略**消息内部**指定字段的差异。
//
// 与 sameBytesExcept 的分工：那个摘的是**顶层**键（model / user_id 这类），这个
// 摘的是**消息内部**的键（reasoning_content / id / tool_call_id）——用例 9 的四个
// 变体只在消息的 reasoning_content 上不同，用例 10 的两个变体只在 tool_call id 上
// 不同，而这两处的"只差一处"用顶层摘键表达不了（它们是嵌套结构）。
//
// 实现：删掉每条 message（以及 message.tool_calls[]）里指定的键，再序列化比较。
// 解析失败一律返回 false（保守方向：宁可判"前提不成立"，也不要放过一个真差异）。
func sameBytesExceptMessageFields(a, b []byte, fields ...string) bool {
	na, ok1 := stripMessageFields(a, fields)
	nb, ok2 := stripMessageFields(b, fields)
	if !ok1 || !ok2 {
		return false
	}
	return bytes.Equal(na, nb)
}

// stripMessageFields 删除 messages 数组里指定字段后的规范化字节。
//
// 只处理 messages：本文件所有用例的差异都落在那里；请求体的其它部分（model /
// tools / max_tokens / user_id）由 sameBytesExcept 负责比较。
func stripMessageFields(body []byte, fields []string) ([]byte, bool) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return nil, false
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return nil, false
	}
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, f := range fields {
			delete(msg, f)
		}
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, rawCall := range calls {
				if call, ok := rawCall.(map[string]any); ok {
					for _, f := range fields {
						delete(call, f)
					}
				}
			}
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false
	}
	return out, true
}

// ---------------------------------------------------------------------------
// 汇总与报告
// ---------------------------------------------------------------------------

// printSummary 打印判定汇总与关键测量，返回 FAIL 数（决定退出码）。
func (s *session) printSummary() int {
	fmt.Printf("\n%s\n判定汇总\n%s\n", rule, rule)
	counts := map[status]int{}
	for _, c := range s.checks {
		counts[c.st]++
	}
	fmt.Printf("PASS=%d  FAIL=%d  WARN=%d  SKIP=%d\n", counts[statusPass], counts[statusFail], counts[statusWarn], counts[statusSkip])

	for _, want := range []status{statusFail, statusWarn} {
		var hits []check
		for _, c := range s.checks {
			if c.st == want {
				hits = append(hits, c)
			}
		}
		if len(hits) == 0 {
			continue
		}
		if want == statusFail {
			fmt.Println("\n未通过（必须先解决）：")
		} else {
			fmt.Println("\n需人工判断 / 必须写进探测报告：")
		}
		for _, c := range hits {
			fmt.Printf("  - [%s] %s：%s\n", c.caseName, c.what, c.detail)
		}
	}

	fmt.Println("\n关键测量：")
	fmt.Printf("  %-40s %-5s %8s %8s %8s %8s %9s %s\n", "用例", "状态", "prompt", "cached", "compl", "reason", "耗时", "finish")
	for _, r := range s.results {
		st := "ok"
		if r.skipped {
			st = "dry"
		} else if r.err != nil {
			st = "ERR"
		}
		fmt.Printf("  %-40s %-5s %8d %8d %8d %8d %9s %s\n",
			r.spec.name, st, r.promptTokens(), r.cachedTokens(), r.completionTokens(), r.reasoningTokens(),
			r.latency().Round(time.Millisecond), r.finishReason())
	}

	fmt.Println("\n提醒（13.3 的交付检查）:")
	fmt.Println("  1. \"重启进程再跑一遍仍命中缓存\"——本工具不跨进程保存状态，**再执行一次本命令**即可：")
	fmt.Println("     若第二次运行的用例 2 仍然 cached_tokens > 0，说明缓存跨进程（跨会话）共享。")
	fmt.Println("  2. 缓存 TTL 要用 -repeat-gap=<间隔> 显式验证（默认不等待；基础轮用 5m、深度轮用 10m）。")
	fmt.Println("  3. 本文件是**原始记录**：把上面的 WARN 与关键测量连同裁决写进 docs/design/probe-report-phase1.md（结论与假设裁决）。")
	return counts[statusFail]
}

// writeReport 把本次测量写成 markdown。
//
// 为什么工具自己写报告：13.3 的交付物之一是"探测报告"，而人工把控制台的数字
// 抄进文档必然会抄错或抄漏（尤其是 usage 的六个数与逐字节的请求体）。分工是：
// 本工具负责**测量数据**，docs/design/probe-report-phase1.md 负责**结论与假设
// 的裁决**——后者必须是人写的，因为它要判断"这个现象意味着设计要改哪里"。
func (s *session) writeReport(path string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# 阶段 1 探测运行记录\n\n")
	fmt.Fprintf(&b, "- 生成时间：%s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "- 接入点：%s（%s）\n", s.opt.endpoint, s.opt.baseURL)
	fmt.Fprintf(&b, "- 模型：%s → %s\n", s.opt.modelID, s.opt.remote)
	fmt.Fprintf(&b, "- 桶字段：%s；桶：%s / %s\n", s.opt.bucketField, s.opt.bucket, s.opt.otherBucket)
	fmt.Fprintf(&b, "- 用例 6 前缀标记：%s（本轮新前缀，两个桶都未见过）\n", s.opt.nonce)
	fmt.Fprintf(&b, "- 超时 / 输出上限：%d ms / %d tokens\n", s.opt.timeoutMs, s.opt.maxTokens)
	fmt.Fprintf(&b, "- thinking 档位：%v（thinking_control=level）\n", s.opt.levels)
	fmt.Fprintf(&b, "- 档位重复次数：%d；第二个模型：%s\n", s.opt.levelRepeats, s.opt.remotePro)
	fmt.Fprintf(&b, "- 用例 8/9/10 的请求体是**手工构造的原始请求**（绕过编码器，见 cmd/probe/contract.go）：\n"+
		"  它们测的是厂商契约，不是生产路径会发的字节。用例 7 与 1~6 走正常的生产路径。\n")
	fmt.Fprintf(&b, "- Normalize 策略：`%+v`\n", s.norm.Policy())
	if s.opt.dryRun {
		fmt.Fprintf(&b, "- 模式：dry-run（未发请求，无测量数据）\n")
	}
	b.WriteString("\n由 `go run ./cmd/probe -report <本文件>` 生成；结论与假设裁决见 `docs/design/probe-report-phase1.md`。\n")

	b.WriteString("\n## 逐用例测量\n\n")
	b.WriteString("| 用例 | 桶 | 档位 | prompt | cached | completion | reasoning | 耗时 | finish_reason |\n")
	b.WriteString("| :-- | :-- | :-- | --: | --: | --: | --: | --: | :-- |\n")
	for _, r := range s.results {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d | %s | %s |\n",
			r.spec.name, r.spec.bucket, r.spec.level,
			r.promptTokens(), r.cachedTokens(), r.completionTokens(), r.reasoningTokens(),
			r.latency().Round(time.Millisecond), r.finishReason())
	}

	b.WriteString("\n## 判定\n\n")
	b.WriteString("| 判定 | 用例 | 项目 | 说明 |\n| :-- | :-- | :-- | :-- |\n")
	for _, c := range s.checks {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.st, c.caseName, c.what, strings.ReplaceAll(c.detail, "|", "\\|"))
	}

	b.WriteString("\n## 原始证据\n\n")
	for _, r := range s.results {
		fmt.Fprintf(&b, "### %s\n\n", r.spec.name)
		if r.err != nil {
			fmt.Fprintf(&b, "错误：`%v`\n\n", r.err)
		}
		fmt.Fprintf(&b, "请求体（实发字节，逐字节可复现）：\n\n```json\n%s\n```\n\n", r.requestBytes)
		if r.resp != nil && len(r.resp.Body) > 0 {
			fmt.Fprintf(&b, "响应体（厂商原始字节，status=%d，耗时 %s）：\n\n```json\n%s\n```\n\n",
				r.resp.StatusCode, r.resp.Latency.Round(time.Millisecond), r.resp.Body)
		}
		if len(r.degradations) > 0 {
			b.WriteString("降级记录：\n\n")
			for _, d := range r.degradations {
				fmt.Fprintf(&b, "- `%s` from=%q to=%q：%s\n", d.Kind, d.From, d.To, d.Reason)
			}
			b.WriteString("\n")
		}
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}
