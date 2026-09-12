package wire

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"marl/internal/types"
)

// 本文件的夹具：适配器 + 一条最小可用的 WireRequest。
//
// 为什么不用 probe 的 session：那是 cmd 包的装配，带着"阶段 1 硬编码能力表"
// 与报告逻辑；本文件的判据是**协议编码与 HTTP 语义**，夹具越薄越好。

const testAPIKey = "test-key-not-a-secret"

func testAdapter(t *testing.T, baseURL, bucketField string) *DeepSeekChatAdapter {
	t.Helper()
	a, err := NewDeepSeekChatAdapter(DeepSeekChatConfig{
		EndpointName: testEndpoint,
		BaseURL:      baseURL,
		APIKey:       testAPIKey,
		RemoteNames:  map[string]string{testModel: testRemote},
		BucketField:  bucketField,
	})
	if err != nil {
		t.Fatalf("构造适配器失败: %v", err)
	}
	return a
}

// encodeFixture 是一条最小 WireRequest：system + 一个 user 回合 + 缓存桶。
func encodeFixture() *WireRequest {
	return &WireRequest{
		Endpoint: testEndpoint,
		Model:    testModel,
		Wire:     types.WireOpenAIChat,
		Messages: []WireMessage{
			{Role: types.WireSystem, Content: "<system>你是 Marl。</system>"},
			{Role: types.WireUser, Content: "<turn>你好</turn>"},
		},
		Sampling:    types.SamplingParams{MaxTokens: 512, Temperature: 0.7, TopP: 0.9, TopK: 5, TimeoutMs: 30000},
		CacheBucket: testBucket,
	}
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v\n%s", err, body)
	}
	return m
}

// TestEncodeOpenAIChatBody 守请求体的"字节可复现 + 不静默丢字段"。
//
// 断言用"键存在/不存在 + 值"而不是整份 golden JSON：golden 会在任何一次
// 有意的字段顺序调整时报警，而那些调整需要单独评估（缓存前缀），
// 不应该被埋在编码测试里。
func TestEncodeOpenAIChatBody(t *testing.T) {
	t.Run("零值参数不发送（top_k 在本线路无处安放）", func(t *testing.T) {
		req := encodeFixture()
		body := decodeBody(t, mustEncode(t, req))

		if body["model"] != testRemote {
			t.Errorf("model=%v，期望 %q（必须发厂商模型名，不是内部 id）", body["model"], testRemote)
		}
		if body["max_tokens"] != float64(512) {
			t.Errorf("max_tokens=%v，期望 512", body["max_tokens"])
		}
		if body["temperature"] != 0.7 || body["top_p"] != 0.9 {
			t.Errorf("temperature/top_p 未按配置发送：%v / %v", body["temperature"], body["top_p"])
		}
		if _, ok := body["top_k"]; ok {
			t.Error("请求体里出现了 top_k：OpenAI 兼容协议没有这个字段，发出去只会被忽略或 400")
		}
		if body["stream"] != false {
			t.Error("stream 必须显式 false：厂商默认值若变化，我们不该突然收到一个 SSE 流")
		}
	})

	t.Run("零值 temperature/top_p 不发送（已知缺口：无法请求贪心解码）", func(t *testing.T) {
		req := encodeFixture()
		req.Sampling.Temperature = 0
		req.Sampling.TopP = 0
		body := decodeBody(t, mustEncode(t, req))
		if _, ok := body["temperature"]; ok {
			t.Error("temperature=0 被发送了：SamplingParams 是值类型，0 与\"未设置\"不可区分（已知缺口，见相位说明）")
		}
		if _, ok := body["top_p"]; ok {
			t.Error("top_p=0 被发送了（同上）")
		}
	})

	t.Run("缓存桶字段：默认 user_id，旧写法 user，两者互斥", func(t *testing.T) {
		// 设计文档 10.15 写 user，官方参数表写 user_id。两者是同一语义的
		// 不同字段名，而"字段名拼错 = 桶静默失效"（缓存串味且无报错）。
		byDefault := decodeBody(t, mustEncode(t, encodeFixture()))
		if byDefault[DefaultBucketField] != string(testBucket) {
			t.Errorf("%s=%v，期望 %q", DefaultBucketField, byDefault[DefaultBucketField], testBucket)
		}
		if _, ok := byDefault[legacyBucketField]; ok {
			t.Errorf("默认写法下不应同时出现 %s 字段（两个桶字段会互相覆盖）", legacyBucketField)
		}

		legacyBody, err := EncodeOpenAIChatBody(encodeFixture(), testRemote, legacyBucketField)
		if err != nil {
			t.Fatalf("用旧字段名编码失败: %v", err)
		}
		m := decodeBody(t, legacyBody)
		if m[legacyBucketField] != string(testBucket) {
			t.Errorf("%s=%v，期望 %q", legacyBucketField, m[legacyBucketField], testBucket)
		}
		if _, ok := m[DefaultBucketField]; ok {
			t.Errorf("旧写法下不应同时出现 %s", DefaultBucketField)
		}
	})

	t.Run("CacheBucket 为空 → 不发桶字段（零值不是某个桶）", func(t *testing.T) {
		req := encodeFixture()
		req.CacheBucket = ""
		body := decodeBody(t, mustEncode(t, req))
		if _, ok := body[DefaultBucketField]; ok {
			t.Error("CacheBucket 为空却发了桶字段：空串会落进某个既有桶，而调用方以为它被隔离了")
		}
	})

	t.Run("bucketField 不在白名单 → 报错（拼错字段名 = 桶静默失效）", func(t *testing.T) {
		if _, err := EncodeOpenAIChatBody(encodeFixture(), testRemote, "userid"); err == nil {
			t.Error("白名单之外的字段名应当报错")
		}
	})

	t.Run("response_format 只在要求 JSON 时出现", func(t *testing.T) {
		req := encodeFixture()
		if _, ok := decodeBody(t, mustEncode(t, req))["response_format"]; ok {
			t.Error("没要求 JSON 却发了 response_format（零值 = 不发送，显式 text 与不发送等价）")
		}
		req.ResponseFormat = OutputFormatJSONObject
		body := decodeBody(t, mustEncode(t, req))
		rf, ok := body["response_format"].(map[string]any)
		if !ok || rf["type"] != string(OutputFormatJSONObject) {
			t.Errorf("response_format=%v，期望 {type: %s}", body["response_format"], OutputFormatJSONObject)
		}
	})

	t.Run("工具表：schema 逐字节透传，非法 schema 报错", func(t *testing.T) {
		req := encodeFixture()
		req.Tools = []ToolDef{{
			Name:        "read_file",
			Description: "读文件",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}}
		body := mustEncode(t, req)
		if !strings.Contains(string(body), `"required":["path"]`) {
			t.Errorf("工具 schema 被重新序列化了（键序/空白变化会让 frozen 前缀漂移）:\n%s", body)
		}

		req.Tools[0].Parameters = json.RawMessage(`{不是 JSON}`)
		if _, err := EncodeOpenAIChatBody(req, testRemote, DefaultBucketField); err == nil {
			t.Error("非法 JSON Schema 应当报错：送到厂商侧是 400，会被误归成 capability 错误")
		}
	})

	t.Run("纯工具调用的 assistant：content=null，无参工具补 {}", func(t *testing.T) {
		req := encodeFixture()
		req.Messages = []WireMessage{
			{Role: types.WireSystem, Content: "s"},
			{Role: types.WireUser, Content: "u"},
			{Role: types.WireAssistant, ToolCalls: []types.ToolCall{{ID: "call_1", Name: "noop"}}},
			// tool 消息的空结果必须由调用方显式表达：空 content 会被
			// assertMessages / 编码自检拒掉（占槽位不带信息），本线路不替
			// 调用方编占位串。
			{Role: types.WireTool, ToolCallID: "call_1", Content: "（无输出）"},
		}
		body := mustEncode(t, req)
		if !strings.Contains(string(body), `"content":null`) {
			t.Errorf("纯工具调用的 assistant 应当发 content=null（空串会被当成\"模型说了空话\"）:\n%s", body)
		}
		if !strings.Contains(string(body), `"arguments":"{}"`) {
			t.Errorf("无参工具的 arguments 应当补 {}（空串在厂商侧解析失败）:\n%s", body)
		}
	})

	t.Run("tool 消息缺 tool_call_id → 报错", func(t *testing.T) {
		req := encodeFixture()
		req.Messages = append(req.Messages, WireMessage{Role: types.WireTool, Content: "结果"})
		if _, err := EncodeOpenAIChatBody(req, testRemote, DefaultBucketField); err == nil {
			t.Error("孤立的工具结果应当报错（厂商侧等价于非法消息）")
		}
	})

	t.Run("附件与 CacheControl 一律报错（不静默丢弃）", func(t *testing.T) {
		req := encodeFixture()
		req.Messages[1].Attachments = []types.Attachment{{}}
		if _, err := EncodeOpenAIChatBody(req, testRemote, DefaultBucketField); err == nil {
			t.Error("带附件的消息应当报错：静默丢弃会让模型基于残缺信息作答")
		}

		req = encodeFixture()
		req.Messages[1].CacheControl = "ephemeral"
		if _, err := EncodeOpenAIChatBody(req, testRemote, DefaultBucketField); err == nil {
			t.Error("带 CacheControl 的消息应当报错：本线路是隐式前缀缓存，没有断点字段")
		}
	})

	t.Run("入参自检：nil / 远端名为空 / Messages 为空 / ResponseFormat 非法", func(t *testing.T) {
		if _, err := EncodeOpenAIChatBody(nil, testRemote, DefaultBucketField); err == nil {
			t.Error("req 为 nil 应当报错")
		}
		if _, err := EncodeOpenAIChatBody(encodeFixture(), "", DefaultBucketField); err == nil {
			t.Error("远端模型名为空应当报错（不回落成内部 id）")
		}
		empty := encodeFixture()
		empty.Messages = nil
		if _, err := EncodeOpenAIChatBody(empty, testRemote, DefaultBucketField); err == nil {
			t.Error("Messages 为空应当报错")
		}
		bad := encodeFixture()
		bad.ResponseFormat = OutputFormat("json_schema")
		if _, err := EncodeOpenAIChatBody(bad, testRemote, DefaultBucketField); err == nil {
			t.Error("未定义的 ResponseFormat 应当报错")
		}
	})
}

// ---------------------------------------------------------------------------
// Execute：HTTP 语义（成功 / 厂商错误 / 没收到响应）
// ---------------------------------------------------------------------------

// TestExecuteOverHTTP 守 Execute 的返回语义。
//
// 三条边界必须分得清（它们决定熔断、重试与升级证据）：
//   - 收到任何状态码 → (*WireResponse, nil)：厂商错误的翻译属 Denormalizer；
//   - 没收到响应 → (*WireResponse, err)，且 StatusCode==0 表达"没收到"；
//   - ctx 取消 → 原样 ctx.Err()；适配器自己的 TimeoutMs 超时 → ErrCallTimeout。
func TestExecuteOverHTTP(t *testing.T) {
	const okBody = `{"id":"x","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop",` +
		`"message":{"role":"assistant","content":"答"}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_cache_hit_tokens":8}}`

	t.Run("200：原始字节原样带回，usage 尽力解析", func(t *testing.T) {
		var gotPath, gotAuth, gotCT string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotAuth, gotCT = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
			_, _ = w.Write([]byte(okBody))
		}))
		defer srv.Close()

		a := testAdapter(t, srv.URL+"/v1/", "") // 末尾斜杠必须被吸收
		resp, err := a.Execute(context.Background(), encodeFixture(), testBinding())
		if err != nil {
			t.Fatalf("Execute 失败: %v", err)
		}
		if gotPath != "/v1/chat/completions" {
			t.Errorf("请求路径=%q，期望 /v1/chat/completions（双斜杠在部分网关会 404，而 404 会被归成 capability 错误）", gotPath)
		}
		if gotAuth != "Bearer "+testAPIKey {
			t.Errorf("Authorization=%q", gotAuth)
		}
		if gotCT != "application/json" {
			t.Errorf("Content-Type=%q", gotCT)
		}
		if resp == nil {
			t.Fatal("resp 为 nil（契约要求即便失败也返回非 nil）")
		}
		if resp.StatusCode != 200 || resp.Wire != types.WireOpenAIChat {
			t.Errorf("resp 元信息不符: status=%d wire=%q", resp.StatusCode, resp.Wire)
		}
		if string(resp.Body) != okBody {
			t.Errorf("Body 不是原始响应字节（预解析或改写会让 Denormalizer 只能猜错误类别）:\n%s", resp.Body)
		}
		if resp.Latency <= 0 {
			t.Error("Latency 未填充：它是 Watchdog 与成本分析的输入，缺了无法补算")
		}
		if resp.UsageRaw == nil {
			t.Fatal("UsageRaw 为 nil（厂商回了 usage）")
		}
		if resp.UsageRaw.CacheReadTokens != 8 {
			t.Errorf("CacheReadTokens=%d，期望 8（探测用例 2 的判据就靠这个字段）", resp.UsageRaw.CacheReadTokens)
		}
	})

	t.Run("厂商错误（4xx/5xx）：err 为 nil，错误在 resp 里", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","code":"rate_limit"}}`))
		}))
		defer srv.Close()

		a := testAdapter(t, srv.URL, "")
		resp, err := a.Execute(context.Background(), encodeFixture(), testBinding())
		if err != nil {
			t.Fatalf("非 2xx 不该走 error 通道（否则调用方只能靠字符串猜类别）: %v", err)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("status=%d，期望 429", resp.StatusCode)
		}
		if !strings.Contains(string(resp.Body), "rate limit") {
			t.Error("错误体被丢掉了：厂商错误详情就在 body 里")
		}
	})

	t.Run("没收到 HTTP 响应：StatusCode==0 + 非 nil resp + error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		baseURL := srv.URL
		srv.Close() // 立刻关掉：连接必然失败

		a := testAdapter(t, baseURL, "")
		resp, err := a.Execute(context.Background(), encodeFixture(), testBinding())
		if err == nil {
			t.Fatal("连接失败应当返回 error")
		}
		if resp == nil {
			t.Fatal("resp 为 nil：归因需要的 StatusCode / Latency 丢了")
		}
		if resp.StatusCode != 0 {
			t.Errorf("StatusCode=%d，期望 0（0 = 没收到响应，与\"收到 4xx/5xx\"是两回事）", resp.StatusCode)
		}
	})

	t.Run("ctx 取消原样返回 ctx.Err()（不是厂商故障）", func(t *testing.T) {
		block := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			<-block // 一直挂着，等客户端取消
		}))
		// defer 是后进先出：先注册 srv.Close（它要等处理函数返回），
		// 再注册 close(block)，这样关闭顺序是 close(block) → srv.Close()。
		// 反过来写会死锁：srv.Close 等处理函数，处理函数等 block。
		defer srv.Close()
		defer close(block)

		a := testAdapter(t, srv.URL, "")
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		_, err := a.Execute(ctx, encodeFixture(), testBinding())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err=%v，期望可 errors.Is 到 context.Canceled（上游取消被当成厂商故障会污染熔断计数与升级证据）", err)
		}
		if errors.Is(err, ErrCallTimeout) {
			t.Error("ctx 取消被报成了 ErrCallTimeout：两者必须可区分（前者是停机，后者是配置的超时太短）")
		}
	})

	t.Run("TimeoutMs 超时 → ErrCallTimeout（且仍可 errors.Is 到 DeadlineExceeded）", func(t *testing.T) {
		block := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			<-block
		}))
		// 关闭顺序同上一子测试：close(block) 先于 srv.Close()。
		defer srv.Close()
		defer close(block)

		a := testAdapter(t, srv.URL, "")
		req := encodeFixture()
		req.Sampling.TimeoutMs = 30
		resp, err := a.Execute(context.Background(), req, testBinding())
		if !errors.Is(err, ErrCallTimeout) {
			t.Errorf("err=%v，期望 ErrCallTimeout", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Error("错误链里应当保留 context.DeadlineExceeded：按 errors.Is 判断的既有代码要继续有效")
		}
		if resp == nil || resp.StatusCode != 0 {
			t.Errorf("超时也要带回非 nil 的 resp（StatusCode==0）: %+v", resp)
		}
	})

	t.Run("装配错配：接入点 / 模型 / 线路", func(t *testing.T) {
		a := testAdapter(t, "https://example.invalid", "")

		req := encodeFixture()
		req.Endpoint = "other-endpoint"
		if _, err := a.Execute(context.Background(), req, testBinding()); err == nil {
			t.Error("req.Endpoint 指向别的接入点应当报错（用本端点的密钥打到别处是静默的凭据错配）")
		}

		req = encodeFixture()
		req.Model = "deepseek/other"
		if _, err := a.Execute(context.Background(), req, testBinding()); err == nil {
			t.Error("req.Model 与 binding.Model 不一致应当报错")
		}

		req = encodeFixture()
		req.Wire = otherWire
		if _, err := a.Execute(context.Background(), req, testBinding()); err == nil {
			t.Error("线路不匹配应当报错")
		}

		if _, err := a.Execute(context.Background(), nil, testBinding()); err == nil {
			t.Error("req 为 nil 应当报错")
		}
	})
}

// ---------------------------------------------------------------------------
// 装配期自检：构造、模型名映射、健康探测
// ---------------------------------------------------------------------------

// TestDeepSeekChatAdapterConstruction 守"装配错误必须在第一个任务之前炸"
// （13.3）。这些错误在运行时的表现都是"请求发出去了但结果莫名不对"。
func TestDeepSeekChatAdapterConstruction(t *testing.T) {
	base := DeepSeekChatConfig{
		EndpointName: testEndpoint,
		BaseURL:      "https://api.deepseek.com/v1",
		APIKey:       testAPIKey,
		RemoteNames:  map[string]string{testModel: testRemote},
	}

	cases := []struct {
		name   string
		mutate func(cfg *DeepSeekChatConfig)
	}{
		{"EndpointName 为空", func(c *DeepSeekChatConfig) { c.EndpointName = "" }},
		{"BaseURL 为空", func(c *DeepSeekChatConfig) { c.BaseURL = "" }},
		{"BaseURL scheme 不是 http/https", func(c *DeepSeekChatConfig) { c.BaseURL = "api.deepseek.com/v1" }},
		{"BaseURL 缺主机名", func(c *DeepSeekChatConfig) { c.BaseURL = "https:///v1" }},
		{"APIKey 为空", func(c *DeepSeekChatConfig) { c.APIKey = "" }},
		{"RemoteNames 为空", func(c *DeepSeekChatConfig) { c.RemoteNames = nil }},
		{"BucketField 不在白名单", func(c *DeepSeekChatConfig) { c.BucketField = "userid" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base
			c.mutate(&cfg)
			if _, err := NewDeepSeekChatAdapter(cfg); err == nil {
				t.Error("应当报错（这类错误留到运行时会变成\"结果莫名不对\"）")
			}
		})
	}

	t.Run("合法配置 + BucketField 空 → 用默认字段名", func(t *testing.T) {
		a, err := NewDeepSeekChatAdapter(base)
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if a.ID() != types.WireOpenAIChat {
			t.Errorf("ID()=%q，期望 %q（三方声明不一致会让请求由 A 编码、由 B 解析）", a.ID(), types.WireOpenAIChat)
		}
		if a.EndpointName() != testEndpoint {
			t.Errorf("EndpointName()=%q", a.EndpointName())
		}
		if a.bucketField != DefaultBucketField {
			t.Errorf("bucketField=%q，期望默认 %q", a.bucketField, DefaultBucketField)
		}
	})

	t.Run("内置客户端不设全局 Timeout", func(t *testing.T) {
		a, err := NewDeepSeekChatAdapter(base)
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if a.client.Timeout != 0 {
			t.Errorf("client.Timeout=%v：单次超时必须来自 SamplingParams.TimeoutMs（每档位可不同），"+
				"全局超时会把两类超时混成一个", a.client.Timeout)
		}
	})

	t.Run("显式传入的 HTTPClient 被采用", func(t *testing.T) {
		custom := &http.Client{Timeout: time.Second}
		cfg := base
		cfg.HTTPClient = custom
		a, err := NewDeepSeekChatAdapter(cfg)
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if a.client != custom {
			t.Error("cfg.HTTPClient 被忽略了（探测与测试需要注入自定义客户端）")
		}
	})
}

// TestDeepSeekChatAdapterModelName 守"不回落"：模型名映射错误必须报错，
// 而不是原样透传或回落默认名。
func TestDeepSeekChatAdapterModelName(t *testing.T) {
	a := testAdapter(t, "https://api.deepseek.com/v1", "")

	t.Run("已声明 → 映射到厂商名", func(t *testing.T) {
		got, err := a.ModelName(testModel)
		if err != nil {
			t.Fatalf("ModelName: %v", err)
		}
		if got != testRemote {
			t.Errorf("ModelName=%q，期望 %q", got, testRemote)
		}
	})

	t.Run("未声明 / 空 id / 映射到空名 → 报错（不回落）", func(t *testing.T) {
		if _, err := a.ModelName("deepseek/unknown"); err == nil {
			t.Error("未声明的模型 id 应当报错：打到名字相近的模型没有任何异常迹象，只有账单会变")
		}
		if _, err := a.ModelName(""); err == nil {
			t.Error("空 id 应当报错（零值是\"未分配\"，不是某个模型）")
		}

		cfg := DeepSeekChatConfig{
			EndpointName: testEndpoint,
			BaseURL:      "https://api.deepseek.com/v1",
			APIKey:       testAPIKey,
			RemoteNames:  map[string]string{testModel: ""},
		}
		empty, err := NewDeepSeekChatAdapter(cfg)
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if _, err := empty.ModelName(testModel); err == nil {
			t.Error("映射到空名应当报错（models.yaml 的 remote_name 必填）")
		}
	})

	t.Run("错误信息里的已声明列表是有序的（可复现）", func(t *testing.T) {
		cfg := DeepSeekChatConfig{
			EndpointName: testEndpoint,
			BaseURL:      "https://api.deepseek.com/v1",
			APIKey:       testAPIKey,
			RemoteNames: map[string]string{
				"deepseek/v4-pro": "deepseek-v4-pro",
				"deepseek/chat":   testRemote,
			},
		}
		many, err := NewDeepSeekChatAdapter(cfg)
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		_, first := many.ModelName("nope")
		for i := 0; i < 20; i++ {
			_, again := many.ModelName("nope")
			if again.Error() != first.Error() {
				t.Fatalf("同一错误两次输出不同（map 遍历顺序泄漏进错误信息，日志比对无从下手）:\n%s\n%s",
					first, again)
			}
		}
		if !strings.Contains(first.Error(), "[deepseek/chat deepseek/v4-pro]") {
			t.Errorf("已声明列表未按字典序输出: %v", first)
		}
	})
}

// TestDeepSeekChatAdapterHealthCheck 守探测语义：只报本次结果，不碰熔断状态。
func TestDeepSeekChatAdapterHealthCheck(t *testing.T) {
	t.Run("2xx → nil，且打的是 /models", func(t *testing.T) {
		var gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		}))
		defer srv.Close()

		a := testAdapter(t, srv.URL+"/v1/", "")
		if err := a.HealthCheck(context.Background(), testEndpoint); err != nil {
			t.Fatalf("HealthCheck: %v", err)
		}
		if gotPath != "/v1/models" {
			t.Errorf("路径=%q，期望 /v1/models（末尾斜杠必须被吸收）", gotPath)
		}
		if gotAuth != "Bearer "+testAPIKey {
			t.Errorf("Authorization=%q：模型列表同时验证密钥有效性，不带密钥的探测只验证了 TCP 连通", gotAuth)
		}
	})

	t.Run("非 2xx → 错误里带状态码与 body 片段（401 与 404 的修法完全不同）", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Authentication Fails"}}`))
		}))
		defer srv.Close()

		a := testAdapter(t, srv.URL, "")
		err := a.HealthCheck(context.Background(), testEndpoint)
		if err == nil {
			t.Fatal("401 应当报错")
		}
		if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Authentication Fails") {
			t.Errorf("错误信息缺少状态码或 body 片段: %v", err)
		}
	})

	t.Run("连不上 → 报错", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		baseURL := srv.URL
		srv.Close()

		a := testAdapter(t, baseURL, "")
		if err := a.HealthCheck(context.Background(), testEndpoint); err == nil {
			t.Error("连接失败应当报错")
		}
	})

	t.Run("endpoint 为空 / 不属于本适配器 → 报错", func(t *testing.T) {
		a := testAdapter(t, "https://api.deepseek.com/v1", "")
		if err := a.HealthCheck(context.Background(), ""); err == nil {
			t.Error("空 endpoint 应当报错（零值不是\"全部\"，探测必须指名）")
		}
		if err := a.HealthCheck(context.Background(), "other-endpoint"); err == nil {
			t.Error("别人的接入点应当报错（把 A 的探测记到 B 头上会让健康度张冠李戴）")
		}
	})
}
