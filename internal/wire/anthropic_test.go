package wire

// Anthropic 线路的契约测试（13.12：normalize/assert/encode/denormalize +
// httptest 端到端）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"marl/internal/types"
)

// capsOK 是最小能力源（normalizer_test.go 同形的形态复制——包内私有，
// 不跨文件共享的既有替身契约）。
type anthropicCaps struct{}

func (anthropicCaps) EffectiveCaps(modelID, endpoint string) (ModelCaps, error) {
	return ModelCaps{
		Has:        []types.Capability{types.CapToolCall, types.CapThinking},
		MaxContext: 200000, MaxOutput: 8192,
		CacheMode:       CacheExplicitBreakpoint,
		ThinkingControl: ThinkControlBudget,
		ThinkingLevels:  []string{"on"},
	}, nil
}

func anthropicCanonical(t *testing.T) (*CanonicalRequest, types.Binding) {
	t.Helper()
	th := types.ThinkingSpec{Level: "on"}
	b := 4096
	th.Budget = &b
	req := &CanonicalRequest{
		Segments: []Segment{
			// frozen 前缀（system 折叠的全部来源）。
			{Kind: SegSystem, Speaker: SpeakerFramework, Content: "You are a Marl agent.", Stability: types.StabilityFrozen},
			// 历史：user turn。
			{Kind: SegTurn, Speaker: SpeakerHuman, Content: "读一下 README.md", Stability: types.StabilityStable},
			// assistant 的调用（tool_use 块）。
			{Kind: SegTurn, Speaker: SpeakerAssistant,
				ToolCalls: []types.ToolCall{{ID: "call_1", Name: "file_read", Arguments: json.RawMessage(`{"path":"README.md"}`)}},
				Stability: types.StabilityStable},
			// tool 回话（tool_result 块）。
			{Kind: SegToolResult, Speaker: SpeakerTool, Content: "# readme", ToolCallID: "call_1", Stability: types.StabilityStable},
		},
		Tools:    []ToolDef{{Name: "file_read", Description: "read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		Sampling: types.SamplingParams{MaxTokens: 1000, Temperature: 0.3},
		Thinking: th,
	}
	binding := types.Binding{Model: "claude-x", Endpoint: "anthropic-main", Wire: types.WireAnthropicMessages, CacheBucket: "agent-1"}
	return req, binding
}

// TestAnthropicBuildRequest：顶层 system、user/assistant 交替、tool 块形态
// 的请求装配（assert 通过为成功判据）。
func TestAnthropicBuildRequest(t *testing.T) {
	req, binding := anthropicCanonical(t)
	n, err := NewAnthropicNormalizer(anthropicCaps{})
	if err != nil {
		t.Fatal(err)
	}
	wr, degr, err := n.BuildRequest(req, binding)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	_ = degr
	if wr.Wire != types.WireAnthropicMessages || wr.System != "You are a Marl agent." {
		t.Fatalf("wire/system = %q / %q", wr.Wire, wr.System)
	}
	if len(wr.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user | assistant | user(tool_result))", len(wr.Messages))
	}
	if wr.Sampling.MaxTokens != 1000 {
		t.Fatalf("max_tokens = %d", wr.Sampling.MaxTokens)
	}
	// 编码字节面：system 顶层、metadata.user_id = 每Agent 缓存桶、
	// tool_use + tool_result 块。
	body, err := EncodeAnthropicMessagesBody(wr, "claude-x-remote")
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		System   string `json:"system"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content"`
		} `json:"messages"`
		Model string `json:"model"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("decode body: %v\n%s", err, body)
	}
	if probe.System != "You are a Marl agent." || probe.Metadata.UserID != "marl-agent-1" || probe.Model != "claude-x-remote" {
		t.Fatalf("body head: system=%q user=%q model=%q", probe.System, probe.Metadata.UserID, probe.Model)
	}
	if len(probe.Messages) != 3 {
		t.Fatalf("body messages = %d", len(probe.Messages))
	}
	blockTypes := func(i int) []string {
		out := []string{}
		for _, c := range probe.Messages[i].Content {
			out = append(out, c.Type)
		}
		return out
	}
	if b := blockTypes(1); !(len(b) == 1 && b[0] == "tool_use") {
		t.Fatalf("assistant blocks = %v", b)
	}
	if b := blockTypes(2); !(len(b) == 1 && b[0] == "tool_result") {
		t.Fatalf("tool-result user blocks = %v", b)
	}
	if len(probe.Tools) != 1 || probe.Tools[0].Name != "file_read" {
		t.Fatalf("tools = %+v", probe.Tools)
	}
}

// TestAnthropicAssertFailures：Assert 的三类拒绝（system 空 / 配对断裂 /
// max_tokens 缺失）。
func TestAnthropicAssertFailures(t *testing.T) {
	n, _ := NewAnthropicNormalizer(anthropicCaps{})
	cases := []struct {
		name   string
		warp   func(wr *WireRequest)
		expect string
	}{
		{"empty system", func(wr *WireRequest) { wr.System = "" }, "system 为空"},
		{"dangling tool_result", func(wr *WireRequest) {
			wr.Messages = []WireMessage{{Role: types.WireUser, Content: "x", ToolCallID: "nope"}}
		}, "配对断裂"},
		{"zero max_tokens", func(wr *WireRequest) { wr.Sampling = types.SamplingParams{} }, "max_tokens 必填"},
		{"wrong wire", func(wr *WireRequest) { wr.Wire = types.WireOpenAIChat }, "Assert 只接受本线路"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, binding := anthropicCanonical(t)
			wr, _, err := n.BuildRequest(req, binding)
			if err != nil {
				t.Fatal(err)
			}
			tc.warp(wr)
			err = n.Assert(wr)
			if err == nil || !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("err = %v, want mention %q", err, tc.expect)
			}
		})
	}
}

// TestAnthropicEndToEnd：httptest 服务器回放一段真实形态的响应 →
// Adapter.Execute → Denormalize → WireTurn（混合流：thinking + tool_use
// + text；usage 映射）。
func TestAnthropicEndToEnd(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("x-api-key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var sent struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			System    string `json:"system"`
			Metadata  struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
			Thinking *struct {
				Type         string `json:"type"`
				BudgetTokens int    `json:"budget_tokens"`
			} `json:"thinking"`
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Errorf("request body not JSON: %v", err)
		}
		if sent.Model != "claude-x-remote" || sent.MaxTokens != 1000 ||
			sent.Metadata.UserID != "marl-agent-1" || sent.Thinking == nil || sent.Thinking.BudgetTokens != 4096 {
			t.Errorf("request body mismatch: %+v", sent)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_1","role":"assistant",
			"content":[
				{"type":"thinking","thinking":"先读文件","signature":"sig"},
				{"type":"tool_use","id":"call_9","name":"file_write","input":{"path":"x"}},
				{"type":"text","text":"写好了。"}
			],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":110,"output_tokens":40,"cache_read_input_tokens":80,"cache_creation_input_tokens":30}
		}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	adapter, err := NewAnthropicChatAdapter(AnthropicChatConfig{
		EndpointName: "anthropic-main", BaseURL: "http://" + strings.TrimPrefix(srv.URL, "http://"),
		APIKey: "k", AnthropicVersion: "2023-06-01",
		RemoteNames: map[string]string{"claude-x": "claude-x-remote"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, binding := anthropicCanonical(t)
	n, _ := NewAnthropicNormalizer(anthropicCaps{})
	wr, _, err := n.BuildRequest(req, binding)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := adapter.Execute(context.Background(), wr, binding)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	turn, err := NewAnthropicChatDenormalizer().Denormalize(resp)
	if err != nil {
		t.Fatalf("denormalize: %v", err)
	}
	// 混合流：thinking → tool_use → text 的 Outcome 序（wire McCabe 契约）。
	var sawThink, sawCall, sawReply bool
	var usage *types.TokenUsage
	for _, o := range turn.Outcomes {
		if o.Reasoning != nil {
			sawThink = true
		}
		if len(o.ToolCalls) == 1 && o.ToolCalls[0].ID == "call_9" {
			sawCall = true
		}
		if o.Reply == "写好了。" {
			sawReply = true
		}
		if o.Usage != nil {
			usage = o.Usage
		}
	}
	if !sawThink || !sawCall || !sawReply {
		t.Fatalf("mixed stream incomplete: %v", turn.Outcomes)
	}
	if usage == nil || usage.PromptTokens != 110 || usage.CacheReadTokens != 80 || usage.CacheWriteTokens != 30 {
		t.Fatalf("usage mapping: %+v", usage)
	}
	// 健康面：自动 401 → HealthCheck 显式可读（不烧 token 的 ping 面）。
}

// TestAnthropicHealthCheck：读路径 GET /v1/models 的 non-2xx 传播（
// 不烧 token 的 ping 面；2xx = healthy）。
func TestAnthropicHealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" && r.Header.Get("x-api-key") == "k" {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	a, err := NewAnthropicChatAdapter(AnthropicChatConfig{
		EndpointName: "e", BaseURL: srv.URL, APIKey: "k",
		RemoteNames: map[string]string{"m": "m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.HealthCheck(context.Background(), "e"); err != nil {
		t.Fatalf("healthy probe: %v", err)
	}
	bad, _ := NewAnthropicChatAdapter(AnthropicChatConfig{
		EndpointName: "e", BaseURL: srv.URL, APIKey: "wrong",
		RemoteNames: map[string]string{"m": "m"},
	})
	if err := bad.HealthCheck(context.Background(), "e"); err == nil {
		t.Fatal("HealthCheck must surface non-2xx")
	}
}
