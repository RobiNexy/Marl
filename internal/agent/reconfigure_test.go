package agent

// 阶段 10 打磨面的测试：reconfigure 三校验 + auto_discuss 折算。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"marl/internal/proto"
	"marl/internal/types"
	"marl/internal/wire"
)

func reConfigureCall(addon string) types.ToolCall {
	return types.ToolCall{ID: "call-reconf", Name: "request_reconfigure", Arguments: json.RawMessage(addon)}
}

// TestRequestReconfigureAudit：意图侧的机制面 = 审计 + 如实回填
// （"reason 为空"是 BAD_ARGS；参数级别的问题全部就地拒绝）。
func TestRequestReconfigureIntent(t *testing.T) {
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(reConfigureCall(`{"reason":"context 反复丢上下文，两个选件的测试并行不下"}`)),
		toolCallTurn(reConfigureCall(`{"reason":""}`)),
		replyTurn("按证据继续。"),
	}}
	a, _ := newTestAgent(t, llm)
	ctx := context.Background()
	if err := a.AppendUser(ctx, "任务"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries := entriesMust(t, a)
	sawOK, sawReject := false, false
	for _, e := range entries {
		if e.Role == types.RoleToolResult {
			if strings.Contains(e.Content, "已记录（进审计") {
				sawOK = true
			}
			if strings.Contains(e.Content, "BAD_ARGS") && strings.Contains(e.Content, "reason 不能为空") {
				sawReject = true
			}
		}
	}
	if !sawOK || !sawReject {
		t.Fatalf("reconfigure tool results missing (ok=%v reject=%v)", sawOK, sawReject)
	}
}

// TestApplyReconfigure：Apply 的边界（三校验 + 只影响可改字段 + 审计触达）。
func TestApplyReconfigure(t *testing.T) {
	a, _ := newTestAgent(t, &fakeLLM{})
	// 空变更 → 拒。
	err := a.ApplyReconfigure(&proto.ReconfigureRequest{Reason: "x"})
	if err == nil || !strings.Contains(err.Error(), "at least one field") {
		t.Fatalf("empty change must error: %v", err)
	}
	// reason 空 → 拒。
	err = a.ApplyReconfigure(&proto.ReconfigureRequest{Sampling: &types.SamplingParams{TimeoutMs: 1}})
	if err == nil || !strings.Contains(err.Error(), "Reason is required") {
		t.Fatalf("no reason: %v", err)
	}
	// 走错对象 → 拒（归属检验）。
	err = a.ApplyReconfigure(&proto.ReconfigureRequest{
		AgentID: "other-agent", Reason: "x",
		Sampling: &types.SamplingParams{TimeoutMs: 700},
	})
	if err == nil || !strings.Contains(err.Error(), "targets") {
		t.Fatalf("agent mismatch: %v", err)
	}
	// 正常路径：Sampling 整块替换。
	if err := a.ApplyReconfigure(&proto.ReconfigureRequest{
		Reason:   "温度需要更确定",
		Sampling: &types.SamplingParams{MaxTokens: 256, Temperature: 0.1, TimeoutMs: 333},
	}); err != nil {
		t.Fatalf("apply sampling: %v", err)
	}
	if a.sampling.MaxTokens != 256 || a.sampling.Temperature != 0.1 || a.sampling.TimeoutMs != 333 {
		t.Fatalf("sampling not replaced: %+v", a.sampling)
	}
}

// TestAutoDiscussInterceptsWrite：写 contracts/ 的 file_write 命中
// auto_discuss 清单 → "写转为讨论"（Part 11.2 入口 3 / 12.2 的写权限）。
func TestAutoDiscussInterceptsWrite(t *testing.T) {
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(mkCallID("file_write", "c1", map[string]any{
			"path": "knowledge/contracts/oauth.md", "content": "契约草稿\n",
		})),
		replyTurn("好的，所说的路径要过讨论；等批注。"),
	}}
	a, _ := newTestAgent(t, llm)
	a.discussCfg = &DiscussionConfig{
		Manager:     &fakeDiscussionManager{},
		AutoDiscuss: []string{"knowledge/contracts/**", "knowledge/preferences/**"},
	}
	ctx := context.Background()
	if err := a.AppendUser(ctx, "写契约文件"); err != nil {
		t.Fatal(err)
	}
	req, err := a.compileView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = req
	// 状态机（一次写→ 讨论 session 已挂牌）：直接驱动到一次写意图。
	turn := llm.turns[0]
	if _, err := a.handleTurn(ctx, turn); err != nil {
		t.Fatalf("write turned into discussion: %v", err)
	}
	// 讨论 session 已 pending（被自动开了）。
	a.mu.Lock()
	open := a.discussSess != nil
	a.mu.Unlock()
	if !open {
		t.Fatal("auto_discuss did not open a discussion")
	}
	entries := entriesMust(t, a)
	found := ""
	for _, e := range entries {
		if e.Role == types.RoleToolResult && strings.Contains(e.Content, "讨论已开启") {
			found = e.Content
			break
		}
	}
	if found == "" {
		t.Fatalf("auto-discuss tool result missing: %v", entries)
	}
	// 文件本身没有写（写入被折算成讨论）。
}
