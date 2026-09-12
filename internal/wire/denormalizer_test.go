package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"marl/internal/types"
)

// otherWire 是"另一条线路"的占位值：WireID 枚举只声明已实现的线路
// （见 types.WireID.Valid 的注释），所以这里用字面量表达"非 openai_chat 的线路"，
// 而不是引用一个尚不存在的常量。
const otherWire = types.WireID("anthropic_messages")

// respWithBody 构造一个待翻译的响应（回程翻译只读 Wire/StatusCode/Body）。
func respWithBody(status int, body string) *WireResponse {
	return &WireResponse{
		Wire:       types.WireOpenAIChat,
		StatusCode: status,
		Body:       []byte(body),
		Latency:    12 * time.Millisecond,
	}
}

// chatBody 拼一个成功响应体。usage 传 "" 表示厂商没回传 usage 字段。
func chatBody(message, finishReason, usage string) string {
	if usage == "" {
		return `{"id":"x","object":"chat.completion","choices":[{"index":0,"finish_reason":"` + finishReason + `","message":` + message + `}]}`
	}
	return `{"id":"x","object":"chat.completion","choices":[{"index":0,"finish_reason":"` + finishReason + `","message":` + message + `}],"usage":` + usage + `}`
}

func denormalize(t *testing.T, resp *WireResponse) *WireTurn {
	t.Helper()
	turn, err := NewOpenAICompatDenormalizer().Denormalize(resp)
	if err != nil {
		t.Fatalf("Denormalize 返回错误（这些用例都应能翻译）: %v", err)
	}
	if turn == nil {
		t.Fatal("Denormalize 返回 (nil, nil)：上层会把一次失败当成一次成功的空响应")
	}
	return turn
}

// TestDenormalizeUsage 是 13.3 点名要求的测试：各种 usage JSON → 正确的 TokenUsage。
//
// 为什么值得一张表：usage 的口径直接进账单与升级决策，而厂商对"缓存命中量"
// 有两种写法（prompt_cache_hit_tokens 与 prompt_tokens_details.cached_tokens）、
// 还可能在部分响应里省掉 prompt_tokens。任何一种解析错都不会报错，只会让成本
// 少算或多算——那正是"账面上看不出来"的一类 bug。
func TestDenormalizeUsage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *types.TokenUsage // nil = 期望"用量未知"（不是零）
	}{
		{
			name: "完整 usage（两种缓存写法都在，取 prompt_cache_hit_tokens）",
			body: chatBody(`{"role":"assistant","content":"好"}`,
				"stop",
				`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,`+
					`"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20,`+
					`"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":7}}`),
			want: &types.TokenUsage{PromptTokens: 100, CompletionTokens: 20, ReasoningTokens: 7, CacheReadTokens: 80},
		},
		{
			name: "只有 prompt_tokens_details.cached_tokens",
			body: chatBody(`{"role":"assistant","content":"好"}`,
				"stop",
				`{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55,"prompt_tokens_details":{"cached_tokens":30}}`),
			want: &types.TokenUsage{PromptTokens: 50, CompletionTokens: 5, CacheReadTokens: 30},
		},
		{
			name: "只有 prompt_cache_hit_tokens",
			body: chatBody(`{"role":"assistant","content":"好"}`,
				"stop",
				`{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55,"prompt_cache_hit_tokens":12}`),
			want: &types.TokenUsage{PromptTokens: 50, CompletionTokens: 5, CacheReadTokens: 12},
		},
		{
			name: "无缓存字段 → 命中为 0（这是真的 0，不是未知）",
			body: chatBody(`{"role":"assistant","content":"好"}`,
				"stop",
				`{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55}`),
			want: &types.TokenUsage{PromptTokens: 50, CompletionTokens: 5},
		},
		{
			name: "prompt_tokens 缺失但 hit/miss 齐全 → 自己求和还原输入总量",
			body: chatBody(`{"role":"assistant","content":"好"}`,
				"stop",
				`{"completion_tokens":5,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40}`),
			want: &types.TokenUsage{PromptTokens: 100, CompletionTokens: 5, CacheReadTokens: 60},
		},
		{
			name: "负数（厂商异常值）→ 夹到 0，不产生负账单",
			body: chatBody(`{"role":"assistant","content":"好"}`,
				"stop",
				`{"prompt_tokens":-5,"completion_tokens":-1,"prompt_cache_hit_tokens":-3}`),
			want: &types.TokenUsage{},
		},
		{
			name: "usage 字段整个缺失 → 用量未知（nil，不是零）",
			body: chatBody(`{"role":"assistant","content":"好"}`, "stop", ""),
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			turn := denormalize(t, respWithBody(200, c.body))
			var got *types.TokenUsage
			for _, o := range turn.Outcomes {
				if o.Usage != nil {
					got = o.Usage
					break
				}
			}
			switch {
			case c.want == nil && got != nil:
				t.Fatalf("期望用量未知（nil），得到 %+v——把未知当 0 会静默少报成本", *got)
			case c.want != nil && got == nil:
				t.Fatalf("期望 %+v，得到 nil（用量未知）", *c.want)
			case c.want == nil && got == nil:
				return
			}
			if *got != *c.want {
				t.Errorf("TokenUsage 不匹配\n得到 %+v\n期望 %+v", *got, *c.want)
			}
		})
	}

	t.Run("同一 turn 的所有 Outcome 共享同一个 usage 指针", func(t *testing.T) {
		body := chatBody(`{"role":"assistant","reasoning_content":"想","content":"答","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]}`,
			"stop", `{"prompt_tokens":10,"completion_tokens":2}`)
		turn := denormalize(t, respWithBody(200, body))
		if len(turn.Outcomes) != 3 {
			t.Fatalf("期望 3 个 Outcome（reasoning / tool_calls / reply），得到 %d", len(turn.Outcomes))
		}
		first := turn.Outcomes[0].Usage
		if first == nil {
			t.Fatal("第 0 个 Outcome 的 Usage 为 nil（usage 是整次调用的口径，必须挂在每个 Outcome 上）")
		}
		for i, o := range turn.Outcomes {
			if o.Usage != first {
				t.Errorf("第 %d 个 Outcome 的 Usage 指针与第 0 个不同：按 Outcome 分摊 usage 会让账单翻倍（消费侧必须按 turn 取一次）", i)
			}
		}
	})
}

// TestDenormalizeErrorClassification 守"厂商错误 → ErrorClass"这张表。
//
// 为什么它不能退化成字符串匹配：上层按类别分流（重试 / 压缩 / 熔断 / 交回 LLM），
// 分类错会让处置完全走错方向——把 429 归成 capability 就不再重试，把能力错误归成
// transient 就会无限重试。
//
// 表里也覆盖"体不可解析"的几种形状：状态码是可靠信号、错误体是不可靠信号，
// 因此体缺失或被替换成 HTML 时分类**不能**退化成 error（见 statusOnlyClass）。
func TestDenormalizeErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrorClass
	}{
		{"429 → transient（退避重试）", 429, `{"error":{"message":"rate limit","type":"rate_limit_error","code":"429"}}`, ErrTransient},
		{"401 → auth_quota（熔断，不重试）", 401, `{"error":{"message":"invalid api key","type":"authentication_error"}}`, ErrAuthQuota},
		{"402 → auth_quota（余额）", 402, `{"error":{"message":"insufficient balance"}}`, ErrAuthQuota},
		{"500 → transient", 500, `{"error":{"message":"internal server error"}}`, ErrTransient},
		{"503 → transient", 503, `{"error":{"message":"service unavailable"}}`, ErrTransient},
		{"413 → context_overflow（触发压缩）", 413, `{"error":{"message":"payload too large"}}`, ErrContextOverflow},
		{"400 + context_length_exceeded → context_overflow", 400, `{"error":{"message":"This model's maximum context length is exceeded","code":"context_length_exceeded"}}`, ErrContextOverflow},
		{"400 无规则命中 → capability（修配置，不盲重试）", 400, `{"error":{"message":"bad request","code":"invalid_request_error"}}`, ErrCapability},
		{"2xx 却带 error 体 → 按文本判（这里命中上下文超限规则）", 200, `{"error":{"message":"maximum context length exceeded"}}`, ErrContextOverflow},
		{"StatusCode=0（没收到 HTTP 响应）→ transient", 0, ``, ErrTransient},
		// 下面三条是"体不可解析也要分类"这条规则的守卫：网关（502/403）会插
		// HTML 错误页或干脆不回体，而状态码本身已经足够定类。若这里退化成
		// error，一次明确的厂商故障就被降级成"我不知道发生了什么"。
		{"非 2xx 但错误体为空（网关 502）→ transient", 502, ``, ErrTransient},
		{"非 2xx 且错误体是 HTML 错误页 → 仍按状态码分类", 403, `<html><body>Forbidden</body></html>`, ErrAuthQuota},
		{"429 且错误体不可解析 → transient（限流是主因）", 429, `rate limited`, ErrTransient},
	}
	d := NewOpenAICompatDenormalizer()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			turn, err := d.Denormalize(respWithBody(c.status, c.body))
			if err != nil {
				t.Fatalf("厂商错误不该走 error 返回值（上层要按 ErrorClass 分流，不是解析错误字符串）: %v", err)
			}
			if len(turn.Outcomes) != 1 {
				t.Fatalf("失败 turn 必须恰好 1 个 Outcome（只带 Signals），得到 %d 个", len(turn.Outcomes))
			}
			o := turn.Outcomes[0]
			if o.Signals.ErrorClass != c.want {
				t.Errorf("ErrorClass=%q，期望 %q（status=%d body=%s）", o.Signals.ErrorClass, c.want, c.status, c.body)
			}
			if o.Usage != nil {
				t.Errorf("失败时 Usage 应为 nil（厂商通常不回传 usage，未知不能用零值冒充）: %+v", *o.Usage)
			}
			if o.Reply != "" || len(o.ToolCalls) > 0 {
				t.Errorf("失败 turn 不该带产出：reply=%q toolCalls=%d", o.Reply, len(o.ToolCalls))
			}
		})
	}
}

// TestDenormalizeUnparsable 守"2xx 却拿不出可分类内容 → error"这条边界。
//
// 边界的一侧是"厂商错误"（有状态码可分类，走 Signals），另一侧是"2xx 且字节
// 不可解析"（无从分类，走 error）。把后者混进前者会让上层拿到一个没有
// ErrorClass 的失败 turn（= 不知道该怎么办），混进成功则更糟。
func TestDenormalizeUnparsable(t *testing.T) {
	cases := []struct {
		name string
		resp *WireResponse
	}{
		{"2xx 空 body", respWithBody(200, "")},
		{"2xx 非法 JSON", respWithBody(200, `{"choices":[`)},
		{"2xx 但没有 choices", respWithBody(200, `{"id":"x","object":"chat.completion"}`)},
		{"2xx 但 choices[0] 没有 message", respWithBody(200, `{"choices":[{"index":0,"finish_reason":"stop"}]}`)},
	}
	d := NewOpenAICompatDenormalizer()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			turn, err := d.Denormalize(c.resp)
			if err == nil {
				t.Fatalf("期望 error（无法解析），得到 turn=%+v", turn)
			}
			if turn != nil {
				t.Errorf("无法解析时不应同时返回 turn（返回空 turn 会被上层当成\"成功的空响应\"）: %+v", turn)
			}
		})
	}

	t.Run("nil 响应与线路不匹配都报错", func(t *testing.T) {
		if _, err := d.Denormalize(nil); err == nil {
			t.Error("nil 响应应当报错")
		}
		wrong := respWithBody(200, `{"choices":[]}`)
		wrong.Wire = otherWire
		if _, err := d.Denormalize(wrong); err == nil {
			t.Error("线路不匹配应当报错（否则字段名部分重合时会\"能跑但字段丢失\"）")
		}
	})

	t.Run("边界对照：同为空体，status==0 与 2xx 分属两条通道", func(t *testing.T) {
		// status==0 = 没收到 HTTP 响应：成因清楚、处置明确（10.11 把\"超时\"归
		// Transient）→ 走 ErrorClass；2xx 空体 = 协议违规且没有可分类的失败
		// 语义 → 走 error。两者混成一条会让\"该不该重试\"退化成调用方再看一次
		// 状态码（第二套判据）。
		noResp, err := d.Denormalize(respWithBody(0, ""))
		if err != nil {
			t.Fatalf("status==0 不该走 error（重试决策只认 ErrorClass）: %v", err)
		}
		if len(noResp.Outcomes) != 1 || noResp.Outcomes[0].Signals.ErrorClass != ErrTransient {
			t.Errorf("期望恰好 1 个 transient 失败 Outcome，得到 %+v", noResp.Outcomes)
		}
		if _, err := d.Denormalize(respWithBody(200, "")); err == nil {
			t.Error("2xx 空体没有可分类的失败语义，应当报错")
		}
	})
}

// TestClassifyEmptyOutput 守"2xx 但三项全空"的分类表。
//
// 这张表的价值在于：厂商在 2xx 里说明了**为什么**没有内容，而不同原因的处置
// 完全不同（退避重试 vs 升级证据 vs 交回 LLM）。一律归 malformed 会把厂商侧的
// 临时容量问题记成"模型输出坏了"，污染升级判据。
func TestClassifyEmptyOutput(t *testing.T) {
	cases := []struct {
		reason string
		want   ErrorClass
	}{
		{"content_filter", ErrContentFilter},
		{"insufficient_system_resource", ErrTransient},
		{"aborted", ErrTransient},
		{"stop", ErrMalformed},
		{"length", ErrMalformed},
		{"tool_calls", ErrMalformed},
		{"", ErrMalformed},
		{"厂商将来新加的值", ErrMalformed},
	}
	for _, c := range cases {
		if got := classifyEmptyOutput(c.reason); got != c.want {
			t.Errorf("classifyEmptyOutput(%q)=%q，期望 %q", c.reason, got, c.want)
		}
	}

	t.Run("空输出 turn 的形态", func(t *testing.T) {
		body := chatBody(`{"role":"assistant"}`, "insufficient_system_resource", `{"prompt_tokens":10,"completion_tokens":0}`)
		turn := denormalize(t, respWithBody(200, body))
		if len(turn.Outcomes) != 1 {
			t.Fatalf("期望 1 个 Outcome，得到 %d", len(turn.Outcomes))
		}
		o := turn.Outcomes[0]
		if o.Signals.ErrorClass != ErrTransient {
			t.Errorf("ErrorClass=%q，期望 %q（后端推理资源不足 → 该退避重试，不该计成升级证据）", o.Signals.ErrorClass, ErrTransient)
		}
		if o.Signals.MalformedOutput {
			t.Error("MalformedOutput 应为 false：这不是\"模型输出坏了\"，而是厂商侧被打断")
		}
		if o.Usage == nil {
			t.Error("usage 该保留（厂商回了 usage）：失败分类不该丢掉可用的记账信息")
		}
	})
}

// TestDenormalizeMixedTurn 守 10.16 的多 Outcome 形态与 Log 顺序。
func TestDenormalizeMixedTurn(t *testing.T) {
	t.Run("reasoning → tool_calls → reply 的顺序即 Log 顺序", func(t *testing.T) {
		body := chatBody(`{"role":"assistant","reasoning_content":"我先算一下","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"timezone\":\"Asia/Shanghai\"}"}}],"content":"已经调用了工具"}`,
			"tool_calls", `{"prompt_tokens":30,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":4}}`)
		turn := denormalize(t, respWithBody(200, body))
		if len(turn.Outcomes) != 3 {
			t.Fatalf("期望 3 个 Outcome，得到 %d", len(turn.Outcomes))
		}
		if turn.Outcomes[0].Reasoning == nil || turn.Outcomes[0].Reasoning.Content != "我先算一下" {
			t.Errorf("第 0 个 Outcome 应是思维链，得到 %+v", turn.Outcomes[0])
		}
		if len(turn.Outcomes[1].ToolCalls) != 1 || turn.Outcomes[1].ToolCalls[0].Name != "get_time" {
			t.Errorf("第 1 个 Outcome 应是工具调用，得到 %+v", turn.Outcomes[1])
		}
		if turn.Outcomes[2].Reply != "已经调用了工具" {
			t.Errorf("第 2 个 Outcome 应是可见回复，得到 %+v", turn.Outcomes[2])
		}
		if !turn.Ready() {
			t.Error("有可见回复 + 工具调用，Ready() 应为 true")
		}
		if got := string(turn.Outcomes[1].ToolCalls[0].Arguments); got != `{"timezone":"Asia/Shanghai"}` {
			t.Errorf("工具参数应逐字节保留（解析再序列化会改变字节，破坏后续配对与审计）：%s", got)
		}
	})

	t.Run("只有思维链 → Ready() 为 false（thinking-only 不算完成一轮）", func(t *testing.T) {
		body := chatBody(`{"role":"assistant","reasoning_content":"还在想"}`, "length", "")
		turn := denormalize(t, respWithBody(200, body))
		if turn.Ready() {
			t.Error("只有思维链时 Ready() 必须为 false：否则主循环会在没有产出任何动作的情况下空转（静默烧预算）")
		}
		if turn.Outcomes[0].Thinking == nil || !turn.Outcomes[0].Thinking.Exposed {
			t.Errorf("思维链 Outcome 应带 Thinking{Exposed:true}，得到 %+v", turn.Outcomes[0].Thinking)
		}
	})

	t.Run("没有思维链的回复带上\"事实\"记录（Exposed=false）", func(t *testing.T) {
		body := chatBody(`{"role":"assistant","content":"答"}`, "stop", "")
		turn := denormalize(t, respWithBody(200, body))
		o := turn.Outcomes[0]
		if o.Thinking == nil {
			t.Fatal("没有 reasoning_content 时也应挂一个 ThinkingOutcome（Exposed=false 表示\"本次响应没回传\"，不是\"模型没思考\"）")
		}
		if o.Thinking.Exposed || o.Thinking.ReasoningField != "" {
			t.Errorf("Exposed/ReasoningField 期望 false/空，得到 %+v", *o.Thinking)
		}
	})

	t.Run("内容被过滤但仍有文本 → ErrorClass=content_filter（不能当完整回复落 Log）", func(t *testing.T) {
		body := chatBody(`{"role":"assistant","content":"前半段…"}`, "content_filter", "")
		turn := denormalize(t, respWithBody(200, body))
		if got := turn.Outcomes[0].Signals.ErrorClass; got != ErrContentFilter {
			t.Errorf("ErrorClass=%q，期望 %q", got, ErrContentFilter)
		}
		if turn.Outcomes[0].Entry.Meta["finish_reason"] != "content_filter" {
			t.Errorf("finish_reason 应落进 LogEntry.Meta（这是\"输出被截断/过滤\"目前唯一可落库的信号）：%+v", turn.Outcomes[0].Entry.Meta)
		}
	})

	t.Run("tool_call 参数不是合法 JSON → MalformedOutput + malformed_output", func(t *testing.T) {
		body := chatBody(`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{not json"}}]}`, "tool_calls", "")
		turn := denormalize(t, respWithBody(200, body))
		o := turn.Outcomes[0]
		if !o.Signals.MalformedOutput || o.Signals.ErrorClass != ErrMalformed {
			t.Errorf("期望 MalformedOutput=true 且 ErrorClass=malformed_output，得到 %+v", o.Signals)
		}
		if len(o.ToolCalls) != 1 {
			t.Fatalf("畸形参数也要保留调用（丢掉它等于静默丢弃模型的意图）：%+v", o.ToolCalls)
		}
		if json.Valid(o.ToolCalls[0].Arguments) {
			t.Error("参数不该被\"修好\"：修好的 JSON 会让上层以为模型真的给出了合法参数")
		}
	})

	t.Run("tool_call 缺 id → 合成稳定 id（同输入同 id）", func(t *testing.T) {
		body := chatBody(`{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"t","arguments":"{}"}}]}`, "tool_calls", "")
		first := denormalize(t, respWithBody(200, body)).Outcomes[0].ToolCalls[0].ID
		second := denormalize(t, respWithBody(200, body)).Outcomes[0].ToolCalls[0].ID
		if first == "" {
			t.Fatal("缺 id 时必须合成（空 id 会让后续 tool 消息无法配对）")
		}
		if first != second {
			t.Errorf("合成的 id 不稳定：%q vs %q（配对 id 必须可复现，否则重放历史时配不上）", first, second)
		}
	})
}

// TestDenormalizeWires 守三方一致性（Adapter.ID / Normalizer.Wires / Denormalizer.Wires）。
func TestDenormalizeWires(t *testing.T) {
	wires := NewOpenAICompatDenormalizer().Wires()
	if len(wires) != 1 || wires[0] != types.WireOpenAIChat {
		t.Fatalf("Wires()=%v，期望 [%s]", wires, types.WireOpenAIChat)
	}
	wires[0] = otherWire
	if again := NewOpenAICompatDenormalizer().Wires(); again[0] != types.WireOpenAIChat {
		t.Errorf("Wires() 返回的是内部切片（调用方改它会静默改掉全局声明）：%v", again)
	}
}

// TestDenormalizeDoesNotMutateResponse 守"纯函数"契约：原始响应体是审计证据。
func TestDenormalizeDoesNotMutateResponse(t *testing.T) {
	body := chatBody(`{"role":"assistant","content":"答"}`, "stop", `{"prompt_tokens":1,"completion_tokens":1}`)
	resp := respWithBody(200, body)
	before := string(resp.Body)
	if _, err := NewOpenAICompatDenormalizer().Denormalize(resp); err != nil {
		t.Fatalf("Denormalize: %v", err)
	}
	if string(resp.Body) != before {
		t.Error("Denormalize 修改了 resp.Body（落库时还要用原始字节）")
	}
	if !strings.Contains(before, `"content":"答"`) {
		t.Errorf("测试夹具本身有问题：%s", before)
	}
}
