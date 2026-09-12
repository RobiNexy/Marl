package agent

// 压缩接入的集成测试（13.5：Agent 上下文塞到接近窗口上限 → 触发压缩 →
// 新 View 变小 → 循环继续跑完）。
//
// 用真实 compress.Compressor（internal/compress）+ 脚本化 LLM：
// 主循环的 LLM 与编排调用的 LLM 是两个替身实例——压缩对主循环透明，
// 这是契约的一部分。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"marl/internal/compress"
	"marl/internal/orchestrate"
	"marl/internal/skill"
	"marl/internal/types"
	"marl/internal/wire"
)

// sumExecutor 是编排调用的替身（compress.Executor）：恒定返回一份可通过
// 机械校验的 SUM，并记录调用次数。
type sumExecutor struct {
	reply  string
	called int
}

func (s *sumExecutor) ExecuteTurn(context.Context, *wire.CanonicalRequest) (*wire.WireTurn, error) {
	s.called++
	return &wire.WireTurn{Outcomes: []wire.Outcome{{
		Reply: s.reply,
		Entry: types.LogEntry{Role: types.RoleAssistantReply, Prov: types.ProvOriginal, Audience: types.AudienceBoth, Content: s.reply},
		Usage: &types.TokenUsage{PromptTokens: 100, CompletionTokens: 200},
	}}}, nil
}

func TestLoopCompressesWhenHeadroomLow(t *testing.T) {
	ctx := context.Background()
	root := workspaceRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		body := strings.Repeat(fmt.Sprintf("file %02d content line\n", i), 60)
		writeFile(t, root, fmt.Sprintf("files/f%02d.txt", i), body)
	}

	// 主循环脚本：6 轮逐个读文件，然后最终回复。
	var script []*wire.WireTurn
	for i := 1; i <= 6; i++ {
		script = append(script, toolCallTurn(mkCall("file_read", map[string]any{
			"path": fmt.Sprintf("files/f%02d.txt", i), "mode": "content", "limit": 60,
		})))
	}
	script = append(script, replyTurn("6 个文件都读完了，总结如下。"))
	main := &fakeLLM{turns: script}

	// 编排调用的替身：SUM 引用一个真实存在的文件（机械校验的路径存在性）。
	sumText := fmt.Sprintf(`## 1. 任务
读取 files/ 下的文件并总结。

## 2. 事实
文件内容为循环生成的行。

## 3. 文件
- ` + "`files/f01.txt`" + `

## 4. 决策
逐个读取。

## 5. 未闭
后续文件在压缩后继续读取。

## 6. 失败
无。

## 7. 现场
已读文件 1-2，接下来读 f03。`)
	orch := &sumExecutor{reply: sumText}

	s, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite} {
		if err := reg.Register(sk); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := compress.NewEngine(compress.EngineConfig{
		AgentID:        "mini-1",
		TaskID:         "t1",
		LLM:            orch,
		Sampling:       types.SamplingParams{MaxTokens: 512},
		MaxTotalTokens: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	comp, err := compress.NewCompressor(engine, root)
	if err != nil {
		t.Fatal(err)
	}

	a, err := New(Config{
		ID:           "mini-1",
		SystemPrompt: "You are a minimal agent.",
		MaxRounds:    12,
		Log:          s,
		Views:        s,
		LLM:          main,
		Skills:       reg,
		Namespace:    &types.Namespace{AgentID: "mini-1", Mounts: []types.Mount{{Pattern: "**", Mode: types.PathWrite}}},
		Resolver:     mustResolver(t, root),
		ProjectRoot:  root,
		Sampling:     types.SamplingParams{MaxTokens: 512},
		// headroom 阈值拉满 → 上下文每有增长就尝试压缩；首次可压缩
		// （轮数 > KeepTailTurns）发生在第 3 轮，此后规模回落不再触发。
		MaxContextTokens: 100000,
		Compression: &CompressConfig{
			Compressor: comp,
			Policy:     orchestrate.CompressionPolicy{HeadroomThreshold: 100000, KeepTailTurns: 2, MinReclaimFraction: 0.2},
		},
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if err := a.AppendUser(ctx, "读取 files/ 下的全部文件并总结"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 压缩发生了且每次都产出 SUM、每次都让 View 变小。
	// （headroom 阈值拉满的演示口径下，上下文每越过上次尝试点就会再触发
	// 一次——事件数随轮数增长，但不变量恒定：SUMAppended 且 New < Old。）
	events := a.CompressionEvents()
	if len(events) == 0 {
		t.Fatal("no compression events")
	}
	for _, ev := range events {
		if !ev.SUMAppended {
			t.Fatalf("expected SUM path (not L0-only): %+v", ev)
		}
		if ev.NewTokens >= ev.OldTokens {
			t.Fatalf("new view must be smaller: %+v", ev)
		}
		if ev.Reclaim < 0.2 {
			t.Fatalf("reclaim below policy floor: %+v", ev)
		}
	}

	// View 里有 SUM（assistant），且早先的中间轮已不在 View。
	foundSUM := false
	for _, it := range a.View().Items {
		e, err := a.log.Get(ctx, it.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if e.Prov == types.ProvSummaryOf {
			foundSUM = true
			if it.WireRole != types.WireAssistant {
				t.Fatal("SUM must present as assistant")
			}
		}
	}
	if !foundSUM {
		t.Fatal("SUM not referenced by view")
	}

	// 真相之源完整：user + 6×(意图+结果) + SUM×N + 最终回复。
	// 原始条目一条不少（Log 只追加），SUM 条数与压缩事件一一对应。
	last, err := a.log.LastSeq(ctx, a.id)
	if err != nil {
		t.Fatal(err)
	}
	if int(last) != 1+12+len(events)+1 {
		t.Fatalf("log entries = %d, want %d (truth preserved)", last, 1+12+len(events)+1)
	}
	sumCount := 0
	for _, e := range entriesMust(t, a) {
		if e.Prov == types.ProvSummaryOf {
			sumCount++
		}
	}
	if sumCount != len(events) {
		t.Fatalf("SUM entries = %d, events = %d", sumCount, len(events))
	}

	// 循环跑完：最终回复在 Log 里（压缩后任务继续）。
	entries := entriesMust(t, a)
	if tail := entries[len(entries)-1]; tail.Role != types.RoleAssistantReply || !strings.Contains(tail.Content, "总结") {
		t.Fatalf("final reply missing/wrong: %+v", tail)
	}
	// 编排调用与压缩事件一一对应（独立预算/账本通路的旁证）。
	if orch.called != len(events) {
		t.Fatalf("orchestrator calls = %d, events = %d", orch.called, len(events))
	}
}

// entriesMust 是 logEntries 的无错误形态（测试里失败即 Fatal）。
func entriesMust(t *testing.T, a *Agent) []*types.LogEntry {
	t.Helper()
	entries, err := logEntries(t, a)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// TestLoopPairingSurvivesL0Dedupe 是真机缺陷的回归测试（见测试报告）：
// 模型分两次读同一文件（续读 offset）→ L0 去重排除旧读取 → 若配对意图
// 未随之排除，编译出的请求就是"没有结果的 tool_calls"，被 Assert 拒绝。
// 断言：压缩采纳后的每一次编译，意图与其结果在段序列里保持配对。
func TestLoopPairingSurvivesL0Dedupe(t *testing.T) {
	ctx := context.Background()
	root := workspaceRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "files/f01.txt", strings.Repeat("line\n", 120))

	script := []*wire.WireTurn{
		toolCallTurn(mkCall("file_read", map[string]any{"path": "files/f01.txt", "limit": 100})),
		toolCallTurn(mkCall("file_read", map[string]any{"path": "files/f01.txt", "offset": 101, "limit": 100})),
		replyTurn("读完了。"),
	}
	main := &fakeLLM{turns: script}
	orch := &sumExecutor{reply: "unused"} // L0-only 路径不应调用 LLM

	s, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite} {
		if err := reg.Register(sk); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := compress.NewEngine(compress.EngineConfig{
		AgentID: "mini-1", TaskID: "t1", LLM: orch,
		Sampling:       types.SamplingParams{MaxTokens: 512},
		MaxTotalTokens: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	comp, err := compress.NewCompressor(engine, root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		ID: "mini-1", SystemPrompt: "agent", MaxRounds: 6,
		Log: s, Views: s, LLM: main, Skills: reg,
		Namespace:   &types.Namespace{AgentID: "mini-1", Mounts: []types.Mount{{Pattern: "**", Mode: types.PathWrite}}},
		Resolver:    mustResolver(t, root),
		ProjectRoot: root,
		Sampling:    types.SamplingParams{MaxTokens: 512},
		// headroom 阈值拉满 → 每轮都尝试压缩；两轮读同一文件触发 L0 去重。
		MaxContextTokens: 100000,
		Compression: &CompressConfig{
			Compressor: comp,
			Policy:     orchestrate.CompressionPolicy{HeadroomThreshold: 100000, KeepTailTurns: 2, MinReclaimFraction: 0.2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendUser(ctx, "读 files/f01.txt"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// L0-only 压缩发生过（去重收益足以达标）。
	events := a.CompressionEvents()
	l0Only := false
	for _, ev := range events {
		if !ev.SUMAppended {
			l0Only = true
		}
	}
	if !l0Only {
		t.Fatalf("expected an L0-only compression event: %+v", events)
	}
	// 每一次编译的请求都必须满足配对不变量：带 ToolCalls 的段之后
	// 紧跟其全部结果的 tool_result 段。
	for i, req := range main.reqs {
		pending := map[string]bool{}
		for _, seg := range req.Segments {
			switch {
			case len(seg.ToolCalls) > 0:
				if len(pending) > 0 {
					t.Fatalf("req[%d]: new tool_calls while previous group unpaired", i)
				}
				for _, c := range seg.ToolCalls {
					pending[c.ID] = true
				}
			case seg.Kind == wire.SegToolResult:
				if !pending[seg.ToolCallID] {
					t.Fatalf("req[%d]: tool result %q without visible intent", i, seg.ToolCallID)
				}
				delete(pending, seg.ToolCallID)
			default:
				if len(pending) > 0 {
					t.Fatalf("req[%d]: %s segment interrupts tool_call pairing", i, seg.Kind)
				}
			}
		}
		if len(pending) > 0 {
			t.Fatalf("req[%d]: dangling tool_calls %v", i, pending)
		}
	}
}
