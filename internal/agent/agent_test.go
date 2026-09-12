package agent

// 主循环测试（13.4 的测试规格：fake executor 驱动脚本回放，
// 验证 Log 里形成 list_dir → file_read → assistant 回复的链条）。
//
// 测试做到"同时是规格"：断言的是**契约**（逐 Outcome 落 Log、工具顺序
// 执行产物进 View、Ready 结束），而不是实现细节。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"marl/internal/skill"
	"marl/internal/types"
	"marl/internal/wire"
)

// fakeLLM 按脚本逐轮返回 WireTurn，并记录收到的请求（上下文编译的断言材料）。
type fakeLLM struct {
	turns  []*wire.WireTurn // 每轮依序返回
	reqs   []*wire.CanonicalRequest
	called int
	err    error // 非空时第 called 次 ExecuteTurn 返回它
}

func (f *fakeLLM) ExecuteTurn(_ context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	f.reqs = append(f.reqs, req)
	idx := f.called
	f.called++
	if f.err != nil {
		err := f.err
		f.err = nil
		return nil, err
	}
	if idx >= len(f.turns) {
		return &wire.WireTurn{}, nil
	}
	return f.turns[idx], nil
}

// replyTurn / toolCallTurn / reasoning 构造 turn 的最简形态
// （Entry 由 Denormalizer 产出——测试以相同结构直接给主循环）。
func replyTurn(text string) *wire.WireTurn {
	oc := wire.Outcome{
		Reply: text,
		Entry: types.LogEntry{
			Role:     types.RoleAssistantReply,
			Prov:     types.ProvOriginal,
			Audience: types.AudienceBoth,
			Content:  text,
			Meta:     map[string]any{"finish_reason": "stop"},
		},
	}
	return &wire.WireTurn{Outcomes: []wire.Outcome{oc}}
}

func toolCallTurn(calls ...types.ToolCall) *wire.WireTurn {
	return &wire.WireTurn{Outcomes: []wire.Outcome{{ToolCalls: calls}}}
}

func reasoningTurn(text string) *wire.WireTurn {
	rc := &wire.ReasoningChunk{Content: text}
	oc := wire.Outcome{Reasoning: rc, Entry: rc.ToLogEntry()}
	return &wire.WireTurn{Outcomes: []wire.Outcome{oc}}
}

// newTestAgent 拼装 registry(list_dir/file_read/file_write) + sqlite store
// + resolver（workspace = 临时目录）+ 手工注入的 LLM 脚本。
func newTestAgent(t *testing.T, llm *fakeLLM) (*Agent, string) {
	t.Helper()
	s, err := newStoreForTest(t)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite} {
		if err := reg.Register(sk); err != nil {
			t.Fatalf("register %s: %v", sk.Name(), err)
		}
	}
	root := workspaceRoot(t)
	a, err := New(Config{
		ID:           "mini-1",
		SystemPrompt: "You are a minimal agent; answer with tools only when needed.",
		MaxRounds:    6,
		Log:          s,
		Views:        s,
		LLM:          llm,
		Skills:       reg,
		Namespace:    &types.Namespace{AgentID: "mini-1", Mounts: []types.Mount{{Pattern: "**", Mode: types.PathWrite}}},
		Resolver:     mustResolver(t, root),
		ProjectRoot:  root,
		Sampling:     types.SamplingParams{MaxTokens: 512},
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a, root
}

// TestLoopListDirThenReadThenReply 是 13.4 的交付测试形态：
// 启动脚本 = list_dir → file_read → 最终回复；断言 Log 链条与 View 形态。
func TestLoopListDirThenReadThenReply(t *testing.T) {
	ctx := context.Background()
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(mkCall("list_dir", map[string]any{"path": ".", "depth": 1})),
		toolCallTurn(mkCall("file_read", map[string]any{"path": "README.md"})),
		replyTurn("列出目录并读完了 README；回复如下。"),
	}}
	a, root := newTestAgent(t, llm)
	// 放一个 README.md（工作区）。
	writeFile(t, root, "README.md", "This is the readme.\n")
	if err := a.AppendUser(ctx, "列出当前目录，读 README.md"); err != nil {
		t.Fatalf("AppendUser: %v", err)
	}

	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st := a.State(); st != types.StateIdle {
		t.Fatalf("state after run = %q", st)
	}

	// Log 应有 6 条：user → list_dir assistant意图 → list_dir 结果 →
	// file_read assistant意图 → file_read 结果 → assistant 回复。
	entries, err := logEntries(t, a)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	roles := make([]types.InternalRole, 0, len(entries))
	for _, e := range entries {
		roles = append(roles, e.Role)
	}
	want := []types.InternalRole{
		types.RoleUserInput,
		types.RoleAssistantReply, types.RoleToolResult, // list_dir
		types.RoleAssistantReply, types.RoleToolResult, // file_read
		types.RoleAssistantReply, // final reply
	}
	if len(roles) != len(want) {
		t.Fatalf("log roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("log[%d] = %v, want %v\nentries=%v", i, roles[i], want[i], entries)
		}
	}
	// 工具调用意图的 Meta 里能看回 write 到下一轮的结构化调用（compile 依赖）。
	if calls, ok := decodeToolCallsMeta(entries[1].Meta); !ok || calls[0].Name != "list_dir" {
		t.Fatalf("tool_calls meta round-trip failed: %+v", entries[1].Meta)
	}
	// 工具结果条目有配对 id 与 ok。
	if entries[2].Meta["ok"] != true {
		t.Fatalf("tool result meta: %+v", entries[2].Meta)
	}

	// View 的条目数 = Log 的 context 条目（阶段 2 全部 both/context）。
	if len(a.View().Items) != len(entries) {
		t.Fatalf("view items = %d, log entries = %d", len(a.View().Items), len(entries))
	}

	// 第二次请求（续轮）编译到上下文，应带工具历史 + 前序回复（缓存口径）。
	if seg0 := len(llm.reqs[0].Segments); seg0 != 2 { // system + user
		t.Fatalf("first request segments = %d, want 2", seg0)
	}
	last := llm.reqs[len(llm.reqs)-1]
	// system + user + (assistant tool_calls + tool result)*2 —— 最后一条是
	// 本轮回复"发出前"的第三次 Execute 的输入（回复在其后追加）。
	if got := len(last.Segments); got != 2+2*2 {
		t.Fatalf("last compile segments = %d: %+v", got, last.Segments)
	}
}

// TestLoopToolFailureFeedsBack 业务失败（ENOENT）作为结果回填，循环继续。
func TestLoopToolFailureFeedsBack(t *testing.T) {
	ctx := context.Background()
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(mkCall("file_read", map[string]any{"path": "absent.txt"})),
		replyTurn("收到 ENOENT，我放弃。"),
	}}
	a, _ := newTestAgent(t, llm)
	if err := a.AppendUser(ctx, "读 absent.txt"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("business failure must not abort loop: %v", err)
	}
	entries, err := logEntries(t, a)
	if err != nil {
		t.Fatal(err)
	}
	// 最后的 tool_result 是失败结果（ok=false + error_type）。
	var toolRes *types.LogEntry
	for _, e := range entries {
		if e.Role == types.RoleToolResult {
			toolRes = e
		}
	}
	if toolRes == nil || toolRes.Meta["ok"] != false || toolRes.Meta["error_type"] != "ENOENT" {
		t.Fatalf("failure feedback: %+v", toolRes)
	}
}

// TestLoopVendorErrorStops 厂商错误（ErrorClass）终结循环并保留已发生内容。
func TestLoopVendorErrorStops(t *testing.T) {
	ctx := context.Background()
	llm := &fakeLLM{turns: []*wire.WireTurn{
		{Outcomes: []wire.Outcome{{
			Signals: wire.OutcomeSignals{ErrorClass: wire.ErrContextOverflow},
		}}},
	}}
	a, _ := newTestAgent(t, llm)
	if err := a.AppendUser(ctx, "触发 overflow"); err != nil {
		t.Fatal(err)
	}
	err := a.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "context_overflow") {
		t.Fatalf("expect class surfaced, got %v", err)
	}
	// 已发生的 user 条目仍在 Log。
	entries, _ := logEntries(t, a)
	if len(entries) < 1 || entries[0].Role != types.RoleUserInput {
		t.Fatalf("entries preserved: %v", entries)
	}
}

// TestAppendAuthorizesThroughLoop 白名单外的技能调用被拒并回填失败结果。
func TestAppendAuthorizesThroughLoop(t *testing.T) {
	ctx := context.Background()
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(mkCall("list_dir", map[string]any{"path": "."})),
		replyTurn("refusal acknowledged"),
	}}
	a, _ := newTestAgent(t, llm)
	// 把白名单设为不含 list_dir —— 直接改 authorizer 不可见，重新构建。
	s, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite} {
		if err := reg.Register(sk); err != nil {
			t.Fatalf("register %s: %v", sk.Name(), err)
		}
	}
	a2, err := New(Config{
		ID:           "mini-1",
		SystemPrompt: "s",
		MaxRounds:    4,
		Log:          s, Views: s,
		LLM:           llm,
		Skills:        reg,
		AllowedSkills: []string{"file_read"},
		Namespace:     a.env.Namespace,
		Resolver:      a.env.Resolver,
		ProjectRoot:   a.env.ProjectRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a2.AppendUser(ctx, "start"); err != nil {
		t.Fatal(err)
	}
	if err := a2.Run(ctx); err != nil {
		t.Fatalf("unauthorized call must not abort: %v", err)
	}
	entries, _ := logEntries(t, a2)
	found := false
	for _, e := range entries {
		if e.Role == types.RoleToolResult && e.Meta["error_type"] == "SKILL_NOT_ALLOWED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deny feedback missing: %v", entries)
	}
}

// TestCompileThinkingAuditOnly 历史思维链默认 audit-only（不进 View 不进请求）。
func TestCompileThinkingAuditOnly(t *testing.T) {
	ctx := context.Background()
	llm := &fakeLLM{turns: []*wire.WireTurn{
		reasoningTurn("<analysis>先想想</analysis>"),
		replyTurn("基于以上思考，答案是 6。"),
	}}
	a, _ := newTestAgent(t, llm)
	if err := a.AppendUser(ctx, "1+2=?"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// View 中没有 thinking 条目（audit-only 默认）。
	for _, it := range a.View().Items {
		entry, err := a.log.Get(ctx, it.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Role == types.RoleThinking {
			t.Fatalf("audit-only thinking leaked into view: %v", entry)
		}
	}
	// 第二次编译的请求里没有 Reasoning（audit-only 同样不进协议层）。
	last := llm.reqs[len(llm.reqs)-1]
	for _, seg := range last.Segments {
		if seg.Reasoning != "" {
			t.Fatalf("reasoning passthrough on audit-only entry: %+v", seg)
		}
	}
}

// TestCompileReasoningReturned 历史 thinking（Audience=both）进请求的 Reasoning 字段。
func TestCompileReasoningReturned(t *testing.T) {
	ctx := context.Background()
	llm := &fakeLLM{turns: []*wire.WireTurn{
		reasoningTurn("<analysis>x+y=3</analysis>"),
		replyTurn("done"),
	}}
	a, _ := newTestAgent(t, llm)
	if err := a.AppendUser(ctx, "start"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// 运行日志里的 thinking 天然是 audit-only（ReasoningChunk.ToLogEntry 的
	// 契约），因此 both 分支不依赖运行路径，直接对 segmentForEntry 做表检验证。
	e := types.LogEntry{Role: types.RoleThinking, Content: "<r>think</r>", Audience: types.AudienceBoth, Prov: types.ProvOriginal}
	seg, err := segmentForEntry(&e)
	if err != nil {
		t.Fatal(err)
	}
	if seg == nil || seg.Reasoning != "<r>think</r>" || seg.Speaker != wire.SpeakerAssistant {
		t.Fatalf("reasoning segment mapping: %+v", seg)
	}
	// audit-only → 不产生段。
	aud := e
	aud.Audience = types.AudienceAudit
	if seg, err := segmentForEntry(&aud); seg != nil || err != nil {
		t.Fatalf("audit-only must compile to nil segment: %+v", seg)
	}
}

// ---------------------------------------------------------------------------
// 测试基建
// ---------------------------------------------------------------------------

func mkCall(name string, args map[string]any) types.ToolCall {
	b, _ := json.Marshal(args)
	return types.ToolCall{ID: fmt.Sprintf("call-%s", name), Name: name, Arguments: b}
}

func logEntries(t *testing.T, a *Agent) ([]*types.LogEntry, error) {
	t.Helper()
	// LastSeq 的哨兵口径：0 = 尚无记录（Append 从 1 起分配）。
	last, err := a.log.LastSeq(context.Background(), a.id)
	if err != nil {
		return nil, err
	}
	if last == 0 {
		return nil, nil
	}
	return a.log.Range(context.Background(), a.id, 1, last)
}
