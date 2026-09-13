package main

// 契约探测：**绕过我们的编码器**，把手工构造的请求体直接发给厂商。
//
// 为什么必须有这一层（以及为什么它不在 internal/wire 里）：
//
//   - 用例 8/9 问的是**厂商侧契约**，不是"我们的编码路径对不对"：
//     用例 8 问"缓存键里有没有模型名"，用例 9 问"带 tools 时历史 reasoning_content
//     回传规则到底是什么"；
//   - 而我们的编码器**发不出**历史 reasoning_content：Segment / WireMessage /
//     openAIChatReqMessage 上都没有承载它的字段，encodeOpenAIChatMessage 也不写它。
//     要测这条厂商契约，只能绕过编码器（这个缺口本身就是本轮审阅的产出之一）。
//
// 绕过是**受控**的：
//
//   - 请求体在这里逐字节构造，报告里打印的就是实发字节（与主路径同一条原则）；
//   - 回程仍然交给 wire.OpenAICompatDenormalizer —— 证据读取（reply / reasoning /
//     toolCalls / usage / errorClasses）与主路径共用同一套翻译，不会出现"两套解析"。
//
// 代价（必须在读报告时记住）：这里的请求体**不走** Normalizer.Assert，因此它
// **不是**"生产路径会发的字节"。每一条判定的措辞都区分"厂商契约"与"我们的路径"。
//
// 判定策略：这里只做"记录 + 前提校验"，不把厂商行为差异判成框架故障——
// 用例 8 的结论（共享/隔离）两种都可能成立，用例 9 的结论会改变设计（见报告 §3.6）。
// 因此这两个用例的判定以 WARN 为主，FAIL 只留给"传输失败 / 请求体构造失败 /
// 结论不可归因的前提被破坏"。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// defaultRemotePro 是用例 8 的第二个远端模型名（跨模型缓存 + 档位支持）。
//
// 它是**配置**不是常量：厂商改模型名时用 -remote-pro 覆盖。放在这里而不是
// options 的默认值里，是为了让"用例 8 到底问了哪个模型"在代码里可见。
const defaultRemotePro = "deepseek-v4-pro"

// rawChatMessage 是**手工构造**的请求消息。
//
// 为什么不用 wire.WireMessage：那个类型是"我们能表达的协议形态"，而它
// 表达不了 reasoning_content——本文件存在的全部理由就是测这一点。
//
// ReasoningContent 用指针而不是 string：三种形态必须能区分——
// nil = **不发送该字段**；&"" = 发送空串；&"..." = 发送内容。
// 用例 9 的四个变体恰好落在这三种形态上，用 string + omitempty 会把
// "空串"与"不发"合并成同一种请求，而它们对厂商可能是两种结果。
type rawChatMessage struct {
	Role             string        `json:"role"`
	Content          *string       `json:"content"`
	ReasoningContent *string       `json:"reasoning_content,omitempty"`
	ToolCalls        []rawChatCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
}

type rawChatCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"` // 恒为 "function"
	Function rawChatFunction `json:"function"`
}

type rawChatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type rawChatTool struct {
	Type     string           `json:"type"`
	Function rawChatNamedTool `json:"function"`
}

type rawChatNamedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// rawChatBody 是手工构造的请求体。
//
// 字段顺序 = 实发字节顺序（与 wire 的请求体结构体同一原则：评审时一眼可见）。
// 只包含本层用到的字段——不追求覆盖协议全集，"少一个字段就少一处会漂的字节"。
type rawChatBody struct {
	Model           string           `json:"model"`
	Messages        []rawChatMessage `json:"messages"`
	Tools           []rawChatTool    `json:"tools,omitempty"`
	MaxTokens       int              `json:"max_tokens,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	UserID          string           `json:"user_id,omitempty"`
}

// chatCompletionsURL 拼出 chat/completions 的完整地址。
//
// 规则必须与适配器的 endpointURL 一致（TrimRight "/" + path），否则本层测的
// 就不是生产路径的地址。这里刻意**不复用**适配器的实现：它是私有的，而且契约
// 探测的全部价值就在于不共享被测代码的路径。代价是这条规则有两处副本——
// 若厂商换路径，两处都要改（探测报告 §5 记着这件事）。
func chatCompletionsURL(baseURL string) string {
	const path = "/chat/completions"
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	return baseURL + path
}

// marshalRawBody 序列化手工请求体。
//
// 与 wire.marshalCanonicalJSON 有两处相同细节（SetEscapeHTML(false) 让 XML 标注
// 在报告里可读、去掉尾部换行让字节逐字节可比）。复用不了（未导出），且这里的
// 字节由本文件独立负责——这是刻意的：契约探测的请求体不该依赖被测编码器。
func marshalRawBody(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("probe: 序列化原始请求体失败: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// rawCall 发一份手工构造的请求体，并把响应交给线路的 Denormalizer 翻译。
//
// 与 session.call 的分工：call 走"Normalizer → Assert → Encode → Execute →
// Denormalize"的完整生产路径；rawCall 只保留首尾两端（构造字节、翻译响应），
// 中间那段被刻意跳过（理由见文件头）。
//
// 错误语义与 call 保持一致：传输层失败进 r.err（附 resp 便于归因），厂商的
// 业务错误**不进** r.err（ADR-0016）——它们表现为非 2xx 的 StatusCode 与
// OutcomeSignals.ErrorClass，由判定函数读。若 rawCall 在这里破例，用例 9 的
// "400 是不是必须回传"就会被误读成"探测工具崩了"。
//
// 并发：无共享状态（client 由 session 持有，http.Client 本身并发安全）。
func (s *session) rawCall(ctx context.Context, spec callSpec, body []byte) *caseResult {
	r := &caseResult{spec: spec, startedAt: time.Now(), requestBytes: body}
	if s.opt.dryRun {
		r.skipped = true
		return r
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsURL(s.opt.baseURL), bytes.NewReader(body))
	if err != nil {
		r.err = fmt.Errorf("构造原始请求失败: %w", err)
		return r
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)

	started := time.Now()
	httpResp, err := s.client.Do(httpReq)
	if err != nil {
		r.err = fmt.Errorf("原始请求失败（未收到 HTTP 响应）: %w", err)
		return r
	}
	defer func() { _ = httpResp.Body.Close() }()
	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		r.err = fmt.Errorf("读取原始响应体失败: %w", err)
		return r
	}

	wr := &wire.WireResponse{
		Wire:       types.WireOpenAIChat,
		StatusCode: httpResp.StatusCode,
		Body:       raw,
		Latency:    time.Since(started),
	}
	r.resp = wr
	turn, err := s.denorm.Denormalize(wr)
	if err != nil {
		// 非 2xx 时厂商的 body 往往不是合法的 chat 响应，Denormalize 报错是
		// **预期**行为——此时保留 resp，让判定函数直接读 StatusCode 与 body
		// 片段（"400 的原文"是用例 9 最关键的证据）。
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			return r
		}
		r.err = fmt.Errorf("回程翻译失败: %w", err)
		return r
	}
	r.turn = turn
	return r
}

// ---------------------------------------------------------------------------
// 用例 9：历史 reasoning_content 的回传形态（厂商契约）
// ---------------------------------------------------------------------------

// echoMode 是历史 assistant 消息上 reasoning_content 的四种回传形态。
//
// 这四个变体对应一个真实的决策（探测报告 §3.6）：厂商文档说带 tools 时
// "历史轮次的 reasoning_content **均应回传**"，但没说"漏掉会怎样"。
// 若只回传最近一轮也能通过，框架就有省 token 的余地；若 400，则"全量回传"
// 是硬要求。四种形态必须能区分，因此 rawChatMessage.ReasoningContent 用指针
// （nil / &"" / &text 三种字节形态）。
type echoMode string

const (
	echoAll   echoMode = "全量回传（每条历史 assistant 都带）"
	echoLast  echoMode = "只回传最近一轮"
	echoNone  echoMode = "完全不回传（不发该字段）"
	echoEmpty echoMode = "带字段但值为空串"
)

// contractReasoning 返回两段**逐字节固定**的思维链文本。
//
// 必须是固定文本：用例 9 的四个变体要在"只差 reasoning_content"的前提下比较，
// 任何随机/时间相关的内容都会让 prompt_tokens 的差值失去意义。内容本身是
// 编造的（厂商不校验历史思维链的真实性——它是文本字段），但长度接近真实。
func contractReasoning() (rc1, rc2 string) {
	rc1 = "（第 1 轮思维链，探测用固定文本）用户想知道当前时间。get_time 接受 IANA 时区名，Asia/Shanghai 是合法取值；先调用工具拿到时间字符串，再复述，不要自己推算。"
	rc2 = "（第 2 轮思维链，探测用固定文本）工具已返回时间字符串。直接复述即可，保留时区标注，不要改写格式，也不要补充解释。"
	return rc1, rc2
}

// rawGetTimeTool 把主路径的工具定义原样转成原始请求的工具条目。
//
// 复用 getTimeTool() 而不是另写一份 schema：参数逐字节相同，用例 9 的结果才
// 能与主路径的用例 3（同一个工具）对照。
func rawGetTimeTool() rawChatTool {
	t := getTimeTool()
	return rawChatTool{
		Type:     "function",
		Function: rawChatNamedTool{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
	}
}

func rawStrPtr(s string) *string { return &s }

// rawAssistant 构造一条历史 assistant 消息。
//
// content 用指针：纯工具调用的 assistant 必须发 null（不写空串——空串会被
// 部分网关当成"模型说了空话"）。这条规则与主路径的编码器一致。
func rawAssistant(content *string, calls []rawChatCall, reasoning *string) rawChatMessage {
	return rawChatMessage{Role: "assistant", Content: content, ToolCalls: calls, ReasoningContent: reasoning}
}

// toolHistory 构造**带 tools**的固定历史（用例 9 的 A~D 组）。
//
// 形态：system → user(问时间) → assistant(tool_call) → tool(结果) →
// assistant(带思维链的回复) → user(新问题)。多轮工具链是厂商文档里
// "历史思维链会被拼接进上下文"所描述的那个场景，单轮对话测不到它。
//
// 变体之间**只有** reasoning_content 字段不同（mode 决定），其余逐字节相同；
// 因此 status / prompt_tokens 的差异只能归因到回传形态。
func toolHistory(mode echoMode, system string) []rawChatMessage {
	rc1, rc2 := contractReasoning()
	reasonFor := func(which int) *string {
		switch mode {
		case echoAll:
			if which == 1 {
				return &rc1
			}
			return &rc2
		case echoLast:
			if which == 2 { // 只有最后一条 assistant（"最近一轮"）
				return &rc2
			}
			return nil
		case echoEmpty:
			return rawStrPtr("")
		default: // echoNone
			return nil
		}
	}
	call := []rawChatCall{{
		ID:       "call_probe_9a01",
		Type:     "function",
		Function: rawChatFunction{Name: "get_time", Arguments: `{"timezone":"Asia/Shanghai"}`},
	}}
	return []rawChatMessage{
		{Role: "system", Content: rawStrPtr(system)},
		{Role: "user", Content: rawStrPtr("现在几点？请调用 get_time 工具获取当前时间。")},
		rawAssistant(nil, call, reasonFor(1)),
		{Role: "tool", Content: rawStrPtr("2026-09-12T09:30:00+08:00"), ToolCallID: "call_probe_9a01"},
		rawAssistant(rawStrPtr("现在是 2026-09-12 09:30（Asia/Shanghai）。"), nil, reasonFor(2)),
		{Role: "user", Content: rawStrPtr("谢谢。那 UTC 时间是多少？只回答时间本身。")},
	}
}

// plainHistory 构造**不带 tools**的固定历史（用例 9 的 E 组）。
//
// 存在的理由：厂商文档对"不带 tools"给出了一个可测的断言——"reasoning_content
// 即使传入 API 也会被忽略，不会拼接进上下文"。忽略与否在**输入 token 数**上
// 直接可见：带与不带的 prompt_tokens 相等 = 被忽略（文档成立），
// 带的那次更多 = 被拼接（文档与实现不一致，必须记录）。
func plainHistory(withReasoning bool, system string) []rawChatMessage {
	_, rc2 := contractReasoning()
	var reasoning *string
	if withReasoning {
		reasoning = &rc2
	}
	return []rawChatMessage{
		{Role: "system", Content: rawStrPtr(system)},
		{Role: "user", Content: rawStrPtr("用一句话说明什么是前缀缓存。")},
		rawAssistant(rawStrPtr("前缀缓存是把重复的输入前缀存在厂商侧、后续请求直接复用而不必重算的机制。"), nil, reasoning),
		{Role: "user", Content: rawStrPtr("再用一句话说明什么是缓存命中率。")},
	}
}

// echoVariant 把"回传形态"与它的测量结果绑在一起，供跨变体比较使用。
type echoVariant struct {
	mode echoMode
	r    *caseResult
}

// dupIDHistory 构造"同一 tool_call id 跨轮重复"的历史（用例 10）。
//
// 它复现的是**我们自己**可能产出的形态：厂商没给 id 时，Denormalizer 按
// (下标, name, arguments) 合成 id（见 synthesizeToolCallID）——同一工具、同一
// 参数在**不同轮**被再次调用时（"重试同一个动作"是常见形态），两轮会算出
// **相同**的 id。而 Assert 只在"同一组"内查重（assertMessages 的 pending 每组
// 重置），于是这种跨组重复会一路发到厂商。
//
// 本用例问的就是厂商接不接受：
//   - 400 → 合成 id 必须带跨轮区分度（审阅发现 C12，必须在阶段 2 的工具循环
//     落地前修）；
//   - 200 → 厂商容忍，但断言层应当**显式**表态（允许或禁止），不能靠默认。
//
// 两个变体（dup / 不 dup）逐字节只差那一个 id 字符串。
func dupIDHistory(dup bool, system string) []rawChatMessage {
	_, rc2 := contractReasoning()
	const idA = "call_probe_10a"
	idB := "call_probe_10b"
	if dup {
		idB = idA
	}
	return []rawChatMessage{
		{Role: "system", Content: rawStrPtr(system)},
		{Role: "user", Content: rawStrPtr("现在几点？请调用 get_time 工具获取当前时间。")},
		rawAssistant(nil, []rawChatCall{{
			ID: idA, Type: "function",
			Function: rawChatFunction{Name: "get_time", Arguments: `{"timezone":"Asia/Shanghai"}`},
		}}, nil),
		{Role: "tool", Content: rawStrPtr("2026-09-12T09:30:00+08:00"), ToolCallID: idA},
		{Role: "user", Content: rawStrPtr("再查一次，这次要 UTC 时间。")},
		rawAssistant(nil, []rawChatCall{{
			ID: idB, Type: "function",
			Function: rawChatFunction{Name: "get_time", Arguments: `{"timezone":"UTC"}`},
		}}, &rc2),
		{Role: "tool", Content: rawStrPtr("2026-09-12T01:30:00+00:00"), ToolCallID: idB},
		{Role: "user", Content: rawStrPtr("谢谢。现在是几点？只回答时间本身。")},
	}
}

// contractCases 跑用例 8 与用例 9（原始请求路径）。
//
// 用例编号延续主路径的 1~7，用 8/9 表示"另一个工具（原始请求）测的厂商契约"。
// 两组都不依赖主路径的回复，但**依赖主路径建立的缓存**（用例 8 的 8b 要问
// "另一个模型能不能命中 8a 刚建立的前缀"），因此必须在 runCases 之后调用。
//
// 前置条件：s.apiKey 与 s.client 已装配（newSession 负责）；dry-run 下不发请求，
// 只构造并打印请求体（判定函数全部返回 SKIP）。
func (s *session) contractCases(ctx context.Context) error {
	system := baseSystemText()
	bucket := string(s.opt.bucket)

	// ---- 用例 8：跨模型缓存 + v4-pro 的档位支持 ----
	// 8a 用带 nonce 的新前缀（本模型第一次见到它 → cached 必须为 0，这是
	// 8b 结论可归因的前提）；8b 与 8a **只差 model 字段**。
	nonceSystem8 := "本次探测标记（用例 8，每次运行都不同）：" + s.opt.nonce + "\n" + system
	msgs8 := []rawChatMessage{
		{Role: "system", Content: rawStrPtr(nonceSystem8)},
		{Role: "user", Content: rawStrPtr("把 1 到 10 的数字逐个相加，写出计算过程。")},
	}
	body8a, err := marshalRawBody(rawChatBody{
		Model: s.opt.remotePro, Messages: msgs8, MaxTokens: s.opt.maxTokens,
		ReasoningEffort: "high", UserID: bucket,
	})
	if err != nil {
		return err
	}
	c8a := s.rawCall(ctx, callSpec{
		name:   "8a 另一模型建立缓存（" + s.opt.remotePro + "，档位 high）",
		why:    "验证第二个模型的 thinking 档位是否生效，并为 8b 建立一个**该模型从未见过**的新前缀",
		bucket: s.opt.bucket,
		level:  "high",
	}, body8a)
	s.reportCase(c8a)

	body8b, err := marshalRawBody(rawChatBody{
		Model: s.opt.remote, Messages: msgs8, MaxTokens: s.opt.maxTokens,
		ReasoningEffort: "high", UserID: bucket,
	})
	if err != nil {
		return err
	}
	c8b := s.rawCall(ctx, callSpec{
		name:   "8b 同字节换模型（" + s.opt.remote + "）",
		why:    "缓存键里有没有模型名：8a 刚建立的前缀，另一个模型能否命中",
		bucket: s.opt.bucket,
		level:  "high",
	}, body8b)
	s.reportCase(c8b)
	s.checkCrossModelCache(c8a, c8b)

	// ---- 用例 9：历史 reasoning_content 的回传形态 ----
	// 前缀里带 nonce：让 A~D 四个变体都落在"两个桶都没见过"的前缀上，
	// 避免上一次运行留下的缓存把命中长度搅进 token 数比较里。
	nonceSystem9 := "本次探测标记（用例 9，每次运行都不同）：" + s.opt.nonce + "\n" + system
	var withTools []echoVariant
	for _, mode := range []echoMode{echoAll, echoLast, echoNone, echoEmpty} {
		body, err := marshalRawBody(rawChatBody{
			Model: s.opt.remote, Messages: toolHistory(mode, nonceSystem9),
			Tools: []rawChatTool{rawGetTimeTool()}, MaxTokens: s.opt.maxTokens,
			ReasoningEffort: "high", UserID: bucket,
		})
		if err != nil {
			return err
		}
		r := s.rawCall(ctx, callSpec{
			name:   "9 带 tools：" + string(mode),
			why:    "厂商文档称带 tools 时历史思维链\"均应回传\"——漏传 / 只传最近一轮会怎样？回传的 token 代价是多少？",
			bucket: s.opt.bucket,
			level:  "high",
		}, body)
		s.reportCase(r)
		s.checkHistoricalReasoning(r, mode)
		withTools = append(withTools, echoVariant{mode: mode, r: r})
	}
	s.checkEchoCost(withTools)

	// E 组：不带 tools（文档称"传入也会被忽略"）——用输入 token 数裁决。
	bodyE1, err := marshalRawBody(rawChatBody{
		Model: s.opt.remote, Messages: plainHistory(true, nonceSystem9), MaxTokens: s.opt.maxTokens,
		ReasoningEffort: "high", UserID: bucket,
	})
	if err != nil {
		return err
	}
	c9e1 := s.rawCall(ctx, callSpec{
		name:   "9E1 不带 tools：历史思维链回传",
		why:    "文档称不带 tools 时 reasoning_content 会被忽略、不拼接进上下文——用输入 token 数验证",
		bucket: s.opt.bucket,
		level:  "high",
	}, bodyE1)
	s.reportCase(c9e1)

	bodyE2, err := marshalRawBody(rawChatBody{
		Model: s.opt.remote, Messages: plainHistory(false, nonceSystem9), MaxTokens: s.opt.maxTokens,
		ReasoningEffort: "high", UserID: bucket,
	})
	if err != nil {
		return err
	}
	c9e2 := s.rawCall(ctx, callSpec{
		name:   "9E2 不带 tools：历史思维链不回传（对照）",
		why:    "E1 的对照组：与 E1 只差 reasoning_content 字段",
		bucket: s.opt.bucket,
		level:  "high",
	}, bodyE2)
	s.reportCase(c9e2)
	s.checkNoToolsReasoningIgnored(c9e1, c9e2)

	// ---- 用例 10：同一 tool_call id 跨轮重复 ----
	// 见 dupIDHistory 的注释：它测的是**我们自己**可能产出的形态（合成 id 碰撞），
	// 判定结果决定 Assert 要不要加跨组查重、合成 id 要不要带轮次区分度。
	nonceSystem10 := "本次探测标记（用例 10，每次运行都不同）：" + s.opt.nonce + "\n" + system
	body10a, err := marshalRawBody(rawChatBody{
		Model: s.opt.remote, Messages: dupIDHistory(true, nonceSystem10),
		Tools: []rawChatTool{rawGetTimeTool()}, MaxTokens: s.opt.maxTokens,
		ReasoningEffort: "high", UserID: bucket,
	})
	if err != nil {
		return err
	}
	c10a := s.rawCall(ctx, callSpec{
		name:   "10a 跨轮重复同一个 tool_call id",
		why:    "合成 id 在\"同一工具同参数重试\"时会跨轮碰撞：厂商接受这种请求吗",
		bucket: s.opt.bucket,
		level:  "high",
	}, body10a)
	s.reportCase(c10a)

	body10b, err := marshalRawBody(rawChatBody{
		Model: s.opt.remote, Messages: dupIDHistory(false, nonceSystem10),
		Tools: []rawChatTool{rawGetTimeTool()}, MaxTokens: s.opt.maxTokens,
		ReasoningEffort: "high", UserID: bucket,
	})
	if err != nil {
		return err
	}
	c10b := s.rawCall(ctx, callSpec{
		name:   "10b 跨轮用不同 id（对照）",
		why:    "10a 的对照组：与 10a 只差一个 id 字符串",
		bucket: s.opt.bucket,
		level:  "high",
	}, body10b)
	s.reportCase(c10b)
	s.checkDuplicateToolCallID(c10a, c10b)

	return nil
}

// ---------------------------------------------------------------------------
// 契约探测的判定
// ---------------------------------------------------------------------------

// checkCrossModelCache 判定用例 8：另一个模型能否命中 8a 刚建立的前缀缓存。
//
// 归因的前提与用例 6 同构：8a 必须是**该模型第一次见到这个前缀**（cached=0）。
// 否则 8b 的命中可能来自 8a 之前的历史，结论不可用。
//
// 两种结果都不是"框架故障"，因此都是 WARN：
//   - 共享 → 缓存键**不含**模型名。这条有实际后果：阶梯升级（换模型）不会丢缓存，
//     但也意味着"用便宜模型热身、用贵模型收尾"会把廉价模型的缓存直接送给贵模型；
//   - 隔离 → 缓存键含模型名，阶梯升级必然重算前缀，升级成本要按全量输入估。
func (s *session) checkCrossModelCache(prime, probe *caseResult) {
	name := probe.spec.name
	if probe.skipped {
		s.addCheck(name, "另一模型是否命中本模型刚建立的前缀", statusSkip, "dry-run：未发请求")
		return
	}
	if prime.err != nil || probe.err != nil {
		s.addCheck(name, "另一模型是否命中本模型刚建立的前缀", statusFail, fmt.Sprintf("请求失败：prime=%v probe=%v", prime.err, probe.err))
		return
	}

	// 顺带记录第二个模型的档位支持（它是 models.yaml 里那道条目的能力声明）。
	if code := prime.statusCode(); code < 200 || code >= 300 {
		s.addCheck(prime.spec.name, "第二个模型的档位被接受", statusWarn, fmt.Sprintf(
			"HTTP %d（body 片段：%s）：该模型不支持 reasoning_effort 时，档位表必须按模型分别声明",
			code, truncate(string(prime.resp.Body), 300)))
	} else if prime.reasoning() != "" {
		s.addCheck(prime.spec.name, "第二个模型的档位被接受", statusPass, fmt.Sprintf(
			"档位 high 生效：reasoning_content %d 字符，reasoning_tokens=%d",
			len([]rune(prime.reasoning())), prime.reasoningTokens()))
	} else {
		s.addCheck(prime.spec.name, "第二个模型的档位被接受", statusWarn,
			"HTTP 200 但 reasoning_content 为空：该模型可能不支持档位（或默认不思考），能力表要按模型分开写")
	}

	if !sameBytesExcept(prime.requestBytes, probe.requestBytes, "model") {
		s.addCheck(name, "两次请求只差 model 字段", statusFail,
			"8a 与 8b 的请求体在 model 之外**也有差异**：差异无法归因到模型，本用例结论无效")
		return
	}
	s.addCheck(name, "两次请求只差 model 字段", statusPass, "8a 与 8b 除 model 外逐字节相同")

	if prime.cachedTokens() > 0 {
		s.addCheck(name, "前缀是全新的（结论可归因的前提）", statusWarn, fmt.Sprintf(
			"**前提不成立**：8a 用新前缀请求 cached_tokens=%d（应当为 0）——8b 的命中可能来自更早的副本，结论不可用",
			prime.cachedTokens()))
		return
	}
	s.addCheck(name, "前缀是全新的（结论可归因的前提）", statusPass, "8a 用带 nonce 的新前缀请求 cached_tokens=0")

	if probe.cachedTokens() > 0 {
		s.addCheck(name, "另一模型是否命中本模型刚建立的前缀", statusWarn, fmt.Sprintf(
			"**共享**：另一个模型命中了 8a 刚建立的前缀（cached_tokens=%d）→ 厂商缓存键**不含模型名**。"+
				"后果：阶梯升级（换模型）不丢缓存；请写进探测报告", probe.cachedTokens()))
		return
	}
	s.addCheck(name, "另一模型是否命中本模型刚建立的前缀", statusWarn,
		"**隔离**：另一个模型对 8a 刚建立的新前缀 cached_tokens=0 → 缓存键含模型名。"+
			"后果：阶梯升级必然重算整个前缀，升级成本按全量输入估；请写进探测报告")
}

// checkHistoricalReasoning 判定用例 9 的一个回传形态是否被厂商接受。
//
// 这是本轮唯一可能"改变设计"的判定：非 2xx 意味着该形态**不被接受**——
//   - 若"完全不回传"被拒 → 全量回传是硬要求（用户裁决成立，且框架必须补通路）；
//   - 若"只回传最近一轮"被拒 → 也支持全量回传；
//   - 若"只回传最近一轮"通过 → 技术上可行（质量不可判定），省 token 的余地存在。
//
// 厂商的拒绝**判 FAIL**（不是 WARN）：FAIL 在这里的含义是"本次测量无效/与文档矛盾"，
// 它必须出现在汇总的"必须先解决"清单里，否则这条关键证据会被埋在 WARN 堆里。
func (s *session) checkHistoricalReasoning(r *caseResult, mode echoMode) {
	name := r.spec.name
	if r.skipped {
		s.addCheck(name, "厂商接受该回传形态", statusSkip, "dry-run：未发请求")
		return
	}
	if r.err != nil {
		s.addCheck(name, "厂商接受该回传形态", statusFail, r.err.Error())
		return
	}
	if code := r.statusCode(); code < 200 || code >= 300 {
		s.addCheck(name, "厂商接受该回传形态", statusFail, fmt.Sprintf(
			"HTTP %d（形态：%s）：厂商**拒绝**了这种历史 reasoning_content 形态——body 片段：%s",
			code, mode, truncate(string(r.resp.Body), 400)))
		return
	}
	if classes := r.errorClasses(); len(classes) > 0 {
		s.addCheck(name, "厂商接受该回传形态", statusFail, fmt.Sprintf(
			"2xx 但响应里带了错误分类 %v（形态：%s）", classes, mode))
		return
	}
	if r.reply() == "" && len(r.toolCalls()) == 0 {
		s.addCheck(name, "厂商接受该回传形态", statusFail, fmt.Sprintf(
			"200 但既无可见回复也无 tool_call（形态：%s，finish_reason=%q）", mode, r.finishReason()))
		return
	}
	s.addCheck(name, "厂商接受该回传形态", statusPass, fmt.Sprintf(
		"200 且返回了内容（形态：%s；prompt=%d cached=%d completion=%d reasoning=%d）",
		mode, r.promptTokens(), r.cachedTokens(), r.completionTokens(), r.reasoningTokens()))
}

// checkEchoCost 汇总用例 9 四个变体的"接受情况 + token 代价"。
//
// 为什么要有这条汇总：单看四条逐条判定读不出**结论**（哪个形态可行、回传要花
// 多少 token）。成本差就是"全量回传 vs 只回传最近一轮"的全部权衡，因此在这里
// 一次性算出来并打印——探测报告的 §3.6 直接引用它。
//
// 判 PASS 而不是 WARN：它本身不是"异常"，而是一张事实表；四个变体的**逐条**
// 判定（checkHistoricalReasoning）已经各自标出了通过/被拒。
func (s *session) checkEchoCost(variants []echoVariant) {
	if len(variants) == 0 {
		return
	}
	baseline := -1
	for _, v := range variants {
		if v.mode == echoNone && v.r.usage() != nil {
			baseline = v.r.promptTokens()
		}
	}
	parts := make([]string, 0, len(variants))
	skipped := 0
	for _, v := range variants {
		if v.r.skipped {
			skipped++
		}
	}
	for _, v := range variants {
		switch {
		case v.r.skipped:
			parts = append(parts, fmt.Sprintf("%s=SKIP", v.mode))
		case v.r.err != nil:
			parts = append(parts, fmt.Sprintf("%s=传输失败", v.mode))
		case v.r.statusCode() < 200 || v.r.statusCode() >= 300:
			parts = append(parts, fmt.Sprintf("%s=HTTP %d（被拒）", v.mode, v.r.statusCode()))
		case v.r.usage() == nil:
			parts = append(parts, fmt.Sprintf("%s=200（usage 缺失）", v.mode))
		case baseline < 0:
			parts = append(parts, fmt.Sprintf("%s=200 prompt=%d", v.mode, v.r.promptTokens()))
		default:
			parts = append(parts, fmt.Sprintf("%s=200 prompt=%d（比不回传多 %+d）",
				v.mode, v.r.promptTokens(), v.r.promptTokens()-baseline))
		}
	}
	if skipped == len(variants) {
		s.addCheck("9 带 tools（汇总）", "回传形态的接受情况与 token 代价", statusSkip, "dry-run：未发请求")
		return
	}
	// 前提：四个变体除消息里的 reasoning_content 外逐字节相同——否则 prompt_tokens
	// 的差值不能归因到回传形态，而那是本用例全部结论的地基。
	ref := variants[0].r.requestBytes
	onlyReasoningDiffers := true
	for _, v := range variants[1:] {
		if !sameBytesExceptMessageFields(ref, v.r.requestBytes, "reasoning_content") {
			onlyReasoningDiffers = false
		}
	}
	if !onlyReasoningDiffers {
		s.addCheck("9 带 tools（汇总）", "四个变体只差 reasoning_content", statusFail,
			"请求体在 reasoning_content 之外**也有差异**：prompt_tokens 的差值无法归因到回传形态，本用例结论无效")
		return
	}
	s.addCheck("9 带 tools（汇总）", "四个变体只差 reasoning_content", statusPass,
		"四个变体除消息里的 reasoning_content 外逐字节相同")
	s.addCheck("9 带 tools（汇总）", "回传形态的接受情况与 token 代价", statusPass,
		"各形态实测："+strings.Join(parts, "；")+"（token 差即回传的历史思维链成本，见报告 §3.6）")
}

// checkDuplicateToolCallID 判定用例 10：跨轮重复的 tool_call id 是否被厂商接受。
//
// 这条判定的产物是一个**断言层决策**（而不是框架故障）：
//   - 厂商拒绝 → Assert 必须加跨组查重（现在是每组重置 pending，跨组重复会漏过），
//     且 synthesizeToolCallID 必须带轮次区分度；
//   - 厂商接受 → 保持现状是**可接受的**，但必须在 assertMessages 的注释里写明
//     "跨组重复是刻意放过的"（默认放过与有意放过在评审时是两回事）。
//
// 判 WARN 而不是 FAIL：阶段 1 没有多轮工具循环，框架现在产不出这种请求；
// 它是阶段 2 落地前的必答项，不是当前的破窗。
func (s *session) checkDuplicateToolCallID(dup, uniq *caseResult) {
	const what = "跨轮重复的 tool_call id 是否被接受"
	name := dup.spec.name
	if dup.skipped {
		s.addCheck(name, what, statusSkip, "dry-run：未发请求")
		return
	}
	if dup.err != nil || uniq.err != nil {
		s.addCheck(name, what, statusFail, fmt.Sprintf("请求失败：重复=%v 对照=%v", dup.err, uniq.err))
		return
	}
	if !sameBytesExceptMessageFields(dup.requestBytes, uniq.requestBytes, "id", "tool_call_id") {
		s.addCheck(name, what, statusFail,
			"10a 与 10b 的请求体在 tool_call id 之外**也有差异**（除消息里的 id/tool_call_id 外应当逐字节相同）：结论无效")
		return
	}
	if code := uniq.statusCode(); code < 200 || code >= 300 {
		s.addCheck(name, what, statusWarn, fmt.Sprintf(
			"对照组（不同 id）也被拒（HTTP %d）：说明请求体本身有别的问题，先看它的 body：%s",
			code, truncate(string(uniq.resp.Body), 300)))
		return
	}
	if code := dup.statusCode(); code < 200 || code >= 300 {
		s.addCheck(name, what, statusWarn, fmt.Sprintf(
			"**厂商拒绝跨轮重复 id**（HTTP %d；对照组的同结构请求是 200）：Assert 必须补跨组查重，"+
				"且 synthesizeToolCallID 必须带上轮次区分度（审阅发现 C12）。body 片段：%s",
			code, truncate(string(dup.resp.Body), 400)))
		return
	}
	s.addCheck(name, what, statusPass, fmt.Sprintf(
		"厂商**接受**跨轮重复 id（重复=%d/对照=%d，都是 200）——容忍 ≠ 可以继续碰撞："+
			"按审阅发现 C12 给合成 id 加轮次区分度，或在 assertMessages 里显式写明放过它的理由",
		dup.statusCode(), uniq.statusCode()))
}

// checkNoToolsReasoningIgnored 判定用例 9 的 E 组：不带 tools 时历史思维链是否被忽略。
//
// 判据是**输入 token 数**：文档说"即使传入 API 也会被忽略，不会拼接进上下文"，
// 那么带与不带的 prompt_tokens 必须相等。不相等就是文档与实现不一致——这条
// 直接决定"历史思维链要不要进请求体"，必须记录（WARN，不是 FAIL：厂商侧行为
// 与文档不符不是框架故障，但它会改变 §10.9 的决策）。
func (s *session) checkNoToolsReasoningIgnored(withR, without *caseResult) {
	const what = "不带 tools 时历史思维链被忽略（prompt_tokens 相等）"
	name := without.spec.name
	if withR.skipped || without.skipped {
		s.addCheck(name, what, statusSkip, "dry-run：未发请求")
		return
	}
	if withR.err != nil || without.err != nil {
		s.addCheck(name, what, statusFail, fmt.Sprintf("请求失败：带=%v 不带=%v", withR.err, without.err))
		return
	}
	if withR.statusCode() < 200 || withR.statusCode() >= 300 {
		s.addCheck(name, what, statusWarn, fmt.Sprintf(
			"**带 reasoning_content 的那次被拒**（HTTP %d）：与文档\"传入也会被忽略\"不符——"+
				"结论：不带 tools 时不要发送该字段。body 片段：%s",
			withR.statusCode(), truncate(string(withR.resp.Body), 300)))
		return
	}
	if withR.usage() == nil || without.usage() == nil {
		s.addCheck(name, what, statusFail, "厂商未回传 usage：无法比较输入 token 数")
		return
	}
	switch d := withR.promptTokens() - without.promptTokens(); {
	case d == 0:
		s.addCheck(name, what, statusPass, fmt.Sprintf(
			"两次 prompt_tokens 都是 %d（历史思维链没有进上下文，与文档一致）", withR.promptTokens()))
	case d > 0:
		s.addCheck(name, what, statusWarn, fmt.Sprintf(
			"**与文档矛盾**：带 reasoning_content 的那次多出 %d 个输入 token（%d vs %d）→ 它被拼接进了上下文。"+
				"结论：不带 tools 时也不该白送思维链（它要花钱），§10.9 的决策必须按此修正",
			d, withR.promptTokens(), without.promptTokens()))
	default:
		s.addCheck(name, what, statusWarn, fmt.Sprintf(
			"异常：带 reasoning_content 的那次输入**更少**（%d vs %d）——两次请求的其他字节应当逐字节相同，先查请求体",
			withR.promptTokens(), without.promptTokens()))
	}
}
