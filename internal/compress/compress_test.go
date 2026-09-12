package compress

// compress 包测试（13.5 的测试规格）。
//
// 断言的是契约而非实现：
//   - 压缩后新 View 比旧 View 小 ≥20%（交付判据）；
//   - SUM 的七个章节都存在（交付判据）；
//   - 拆分后的段落覆盖原文、无重叠（交付判据，semantic 部分在 orchestrate
//     包测试；本包测禁切区扫描与 JSON 解析）；
//   - Log 只追加：L0 与压缩都不删条目；
//   - 传入 view 不被修改（Compress 后置条件）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"marl/internal/orchestrate"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
)

// ---------------------------------------------------------------------------
// 测试基建
// ---------------------------------------------------------------------------

// fakeLLM 按脚本返回回复，并记录收到的请求（请求形态的断言材料）。
type fakeLLM struct {
	replies  []string // 逐次返回；超出后重复最后一个
	err      error    // 非空时下一次调用返回它
	reqs     []*wire.CanonicalRequest
	sampling []types.SamplingParams
}

func (f *fakeLLM) ExecuteTurn(_ context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	f.reqs = append(f.reqs, req)
	f.sampling = append(f.sampling, req.Sampling)
	if f.err != nil {
		err := f.err
		f.err = nil
		return nil, err
	}
	idx := len(f.reqs) - 1
	if idx >= len(f.replies) {
		idx = len(f.replies) - 1
	}
	reply := f.replies[idx]
	return &wire.WireTurn{Outcomes: []wire.Outcome{{
		Reply: reply,
		Entry: types.LogEntry{Role: types.RoleAssistantReply, Prov: types.ProvOriginal, Audience: types.AudienceBoth, Content: reply},
		Usage: &types.TokenUsage{PromptTokens: 100, CompletionTokens: 50},
	}}}, nil
}

// recordingSink 记录编排账本推送（独立账本的断言材料）。
type recordingSink struct {
	entries []*types.TokenUsage
}

func (r *recordingSink) RecordOrchestration(_ context.Context, _ types.AgentID, _ types.TaskID, u *types.TokenUsage) error {
	r.entries = append(r.entries, u)
	return nil
}

func newEngine(t *testing.T, llm Executor, sink UsageSink) *Engine {
	t.Helper()
	e, err := NewEngine(EngineConfig{
		AgentID:        "a1",
		TaskID:         "t1",
		LLM:            llm,
		Sampling:       types.SamplingParams{MaxTokens: 512, Temperature: 0.1},
		MaxTotalTokens: 100000,
		Sink:           sink,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func newCompressor(t *testing.T, llm Executor, root string, sink UsageSink) *SumCompressor {
	t.Helper()
	c, err := NewCompressor(newEngine(t, llm, sink), root)
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	return c
}

// newLog 每个用例独立的 SQLite Log。
func newLog(t *testing.T) store.MessageLog {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// appendEntry 落一条原始条目。
func appendEntry(t *testing.T, ctx context.Context, lg store.MessageLog, agentID types.AgentID, role types.InternalRole, content string, meta map[string]any) *types.LogEntry {
	t.Helper()
	e := types.NewLogEntry(agentID, role, content)
	e.Meta = meta
	if _, err := lg.Append(ctx, e); err != nil {
		t.Fatalf("append %s: %v", role, err)
	}
	return e
}

// fileReadResult 构造 file_read 成功结果的条目（SkillResult 的序列化形态）。
// 结果与其意图的配对由返回值携带：调用方把返回条目紧跟在对应意图之后落库，
// 两者的 tool_call_id 由本函数与 toolCallEntry 的共享计数器对齐。
func fileReadResult(t *testing.T, ctx context.Context, lg store.MessageLog, agentID types.AgentID, path, body string) *types.LogEntry {
	t.Helper()
	res := fmt.Sprintf(`{"ok":true,"Data":{"path":%q,"content":%q}}`, path, body)
	return appendEntry(t, ctx, lg, agentID, types.RoleToolResult, res, map[string]any{
		"name": "file_read", "ok": true, "tool_call_id": fmt.Sprintf("c-file_read-%d", callSeq),
	})
}

// toolCallEntry 构造 assistant 的调用意图条目（开启一轮；callID 与结果
// 条目的 tool_call_id 配对——配对不变量要求测试夹具与真实落库同构）。
func toolCallEntry(t *testing.T, ctx context.Context, lg store.MessageLog, agentID types.AgentID, name, args string) *types.LogEntry {
	t.Helper()
	callID := fmt.Sprintf("c-%s-%d", name, nextCallSeq())
	return appendEntry(t, ctx, lg, agentID, types.RoleAssistantReply, "", map[string]any{
		"tool_calls": []any{map[string]any{"id": callID, "name": name, "arguments": args}},
	})
}

var callSeq int

func nextCallSeq() int { callSeq++; return callSeq }

// buildRoundsLog 构造 n 轮 file_read 循环的 Log（head 一条 user + n 轮，
// 每轮 = 调用意图 + 工具结果），返回按序条目。
func buildRoundsLog(t *testing.T, ctx context.Context, lg store.MessageLog, n int, bodyLen int) []*types.LogEntry {
	t.Helper()
	const id = types.AgentID("a1")
	all := []*types.LogEntry{appendEntry(t, ctx, lg, id, types.RoleUserInput, "读取 files/ 下的文件并总结", nil)}
	for i := 0; i < n; i++ {
		all = append(all, toolCallEntry(t, ctx, lg, id, "file_read", fmt.Sprintf(`{"path":"files/f%02d.txt"}`, i+1)))
		body := strings.Repeat(fmt.Sprintf("file %02d line\n", i+1), bodyLen)
		all = append(all, fileReadResult(t, ctx, lg, id, fmt.Sprintf("files/f%02d.txt", i+1), body))
	}
	return all
}

// viewOf 按 Log 条目顺序构造 View（Position 1..n，全部 stable/visible）。
func viewOf(agentID types.AgentID, entries ...*types.LogEntry) *types.ContextView {
	v := &types.ContextView{AgentID: agentID}
	for i, e := range entries {
		v.Items = append(v.Items, types.ViewItem{
			Ref:       e.ID,
			WireRole:  types.WireUser,
			Stability: types.StabilityStable,
			Visible:   true,
			Position:  float64(i + 1),
		})
	}
	return v
}

// validSUM 构造一份能通过机械校验的 SUM（文件路径指向 root 下真实文件）。
func validSUM(root, rel string) string {
	return fmt.Sprintf(`## 1. 任务
读取 files/ 下的文件并总结。

## 2. 事实
文件内容为循环生成的行。

## 3. 文件
- `+"`%s`"+`

## 4. 决策
逐个读取。

## 5. 未闭
尚有文件未读。

## 6. 失败
无。

## 7. 现场
已读 1 个文件。`, rel)
}

// ---------------------------------------------------------------------------
// 禁切区扫描
// ---------------------------------------------------------------------------

func TestZoneScanner(t *testing.T) {
	zs := ZoneScanner{}
	cases := []struct {
		name    string
		content string
		want    []orchestrate.NoSplitZone
	}{
		{"no zones", "plain\nlines\nonly", nil},
		{"closed fence", "a\n```\ncode\n```\nb", []orchestrate.NoSplitZone{{Kind: orchestrate.ZoneCodeFence, Start: 2, End: 4}}},
		{"unterminated fence", "a\n```\ncode\nmore", []orchestrate.NoSplitZone{{Kind: orchestrate.ZoneCodeFence, Start: 2, End: 4}}},
		{"blockquote run", "a\n> q1\n> q2\nb", []orchestrate.NoSplitZone{{Kind: orchestrate.ZoneBlockquote, Start: 2, End: 3}}},
		{"mixed", "> q\n```\nx\n```\n> r", []orchestrate.NoSplitZone{
			{Kind: orchestrate.ZoneBlockquote, Start: 1, End: 1},
			{Kind: orchestrate.ZoneCodeFence, Start: 2, End: 4},
			{Kind: orchestrate.ZoneBlockquote, Start: 5, End: 5},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := zs.ScanNoSplitZones(tc.content)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("zones = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Orchestrator（JSON mode / 预算 / 账本 / 错误）
// ---------------------------------------------------------------------------

func TestEngineCallShape(t *testing.T) {
	llm := &fakeLLM{replies: []string{"[]"}}
	sink := &recordingSink{}
	e := newEngine(t, llm, sink)
	segs, err := e.SplitSemantic(context.Background(), "1| a\n2| b")
	if err != nil || len(segs) != 0 {
		t.Fatalf("segs=%v err=%v", segs, err)
	}
	req := llm.reqs[0]
	if len(req.Tools) != 0 {
		t.Fatal("orchestration call must not carry tools")
	}
	if req.Thinking.Level != "off" {
		t.Fatalf("thinking = %q, want off", req.Thinking.Level)
	}
	if !req.OutputJSON {
		t.Fatal("split call must request JSON output")
	}
	if len(req.Segments) != 2 || req.Segments[0].Stability != types.StabilityFrozen || req.Segments[1].Stability != types.StabilityStable {
		t.Fatalf("segments: %+v", req.Segments)
	}
	if len(sink.entries) != 1 || sink.entries[0].PromptTokens != 100 {
		t.Fatalf("sink: %+v", sink.entries)
	}
}

func TestEngineBudget(t *testing.T) {
	llm := &fakeLLM{replies: []string{"[]"}}
	e, err := NewEngine(EngineConfig{
		AgentID: "a1", LLM: llm,
		Sampling:       types.SamplingParams{MaxTokens: 100000},
		MaxTotalTokens: 10, // 预算小于任何一次调用的估算
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.SplitSemantic(context.Background(), "content")
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v, want budget exhaustion", err)
	}
	if len(llm.reqs) != 0 {
		t.Fatal("call must not be issued when budget is exhausted")
	}
}

func TestEngineErrors(t *testing.T) {
	t.Run("vendor error", func(t *testing.T) {
		vendor := &vendorErrLLM{class: wire.ErrTransient}
		e2, err := NewEngine(EngineConfig{AgentID: "a1", LLM: vendor, Sampling: types.SamplingParams{MaxTokens: 64}, MaxTotalTokens: 100000})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e2.SplitSemantic(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "vendor error") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty reply", func(t *testing.T) {
		llm := &fakeLLM{replies: []string{""}}
		e := newEngine(t, llm, nil)
		_, err := e.SplitSemantic(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "no reply") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		llm := &fakeLLM{replies: []string{"not json"}}
		e := newEngine(t, llm, nil)
		_, err := e.SplitSemantic(context.Background(), "x")
		if err == nil {
			t.Fatal("want parse error")
		}
	})
}

// vendorErrLLM 只产厂商错误 turn 的替身。
type vendorErrLLM struct{ class wire.ErrorClass }

func (v *vendorErrLLM) ExecuteTurn(context.Context, *wire.CanonicalRequest) (*wire.WireTurn, error) {
	return &wire.WireTurn{Outcomes: []wire.Outcome{{Signals: wire.OutcomeSignals{ErrorClass: v.class}}}}, nil
}

func TestParseSegmentJSON(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		want    int
		wantErr bool
	}{
		{"plain", `[{"topic":"a","start_line":1,"end_line":2}]`, 1, false},
		{"fenced", "```json\n[{\"topic\":\"a\",\"start_line\":1,\"end_line\":2}]\n```", 1, false},
		{"empty array", `[]`, 0, false},
		{"garbage", `nope`, 0, true},
		{"empty string", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			segs, err := parseSegmentJSON(tc.reply)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if len(segs) != tc.want {
				t.Fatalf("segs = %d, want %d", len(segs), tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SUM 校验
// ---------------------------------------------------------------------------

func TestValidateSUM(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "files", "f01.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newCompressor(t, &fakeLLM{}, root, nil)

	t.Run("valid sum passes", func(t *testing.T) {
		if err := c.ValidateSUM(context.Background(), validSUM(root, "files/f01.txt")); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("empty content", func(t *testing.T) {
		if err := c.ValidateSUM(context.Background(), ""); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("missing section aggregated", func(t *testing.T) {
		sum := strings.Replace(validSUM(root, "files/f01.txt"), "## 5. 未闭\n尚有文件未读。\n\n", "", 1)
		sum = strings.Replace(sum, "## 6. 失败\n无。\n\n", "", 1)
		err := c.ValidateSUM(context.Background(), sum)
		if err == nil || !strings.Contains(err.Error(), "## 5.") || !strings.Contains(err.Error(), "## 6.") {
			t.Fatalf("aggregate error = %v", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		err := c.ValidateSUM(context.Background(), validSUM(root, "files/absent.txt"))
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("section 3 says none", func(t *testing.T) {
		sum := strings.Replace(validSUM(root, "files/f01.txt"), "- `files/f01.txt`", "无", 1)
		if err := c.ValidateSUM(context.Background(), sum); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("absolute path rejected", func(t *testing.T) {
		err := c.ValidateSUM(context.Background(), validSUM(root, root+"/files/f01.txt"))
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("path escape rejected", func(t *testing.T) {
		err := c.ValidateSUM(context.Background(), validSUM(root, "../etc/passwd"))
		if err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("err = %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// L0 机械清理
// ---------------------------------------------------------------------------

func TestL0OnlyPath(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	const id = types.AgentID("a1")
	// 同一文件读两轮（意图+结果成对，内容大），其余很小 → L0 去重即可
	// 达标，不调 LLM。
	small := appendEntry(t, ctx, lg, id, types.RoleUserInput, "task", nil)
	i1 := toolCallEntry(t, ctx, lg, id, "file_read", `{"path":"files/f01.txt"}`)
	r1 := fileReadResult(t, ctx, lg, id, "files/f01.txt", strings.Repeat("big\n", 200))
	i2 := toolCallEntry(t, ctx, lg, id, "file_read", `{"path":"files/f01.txt"}`)
	r2 := fileReadResult(t, ctx, lg, id, "files/f01.txt", strings.Repeat("big\n", 200))
	entries := []*types.LogEntry{small, i1, r1, i2, r2}
	v := viewOf(id, entries...)
	snapshot := fmt.Sprint(*v)

	llm := &fakeLLM{replies: []string{"should not be called"}}
	c := newCompressor(t, llm, t.TempDir(), nil)
	res, err := c.Compress(ctx, lg, v, orchestrate.CompressionPolicy{
		HeadroomThreshold: 100, KeepTailTurns: 3, MinReclaimFraction: 0.2,
	})
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if res.SUMENTry != nil {
		t.Fatal("L0-only path must not produce SUM")
	}
	// 旧读取**连同其意图**被排除（配对不变量）；pruned = 结果 1 + 意图 1。
	if res.L0Pruned != 2 {
		t.Fatalf("pruned = %d, want 2", res.L0Pruned)
	}
	if res.Reclaim < 0.2 {
		t.Fatalf("reclaim = %v", res.Reclaim)
	}
	// 可见：user + 第二轮的意图与结果。
	vis := 0
	for _, it := range res.View.Items {
		if it.Visible {
			vis++
		}
	}
	if vis != 3 {
		t.Fatalf("visible = %d, want 3", vis)
	}
	if fmt.Sprint(*v) != snapshot {
		t.Fatal("input view mutated")
	}
	if len(llm.reqs) != 0 {
		t.Fatal("LLM must not be called on L0-only path")
	}
}

func TestL0ThinkingExclusion(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	const id = types.AgentID("a1")
	think := appendEntry(t, ctx, lg, id, types.RoleThinking, "<r>long thought</r>", nil)
	think.Audience = types.AudienceBoth // 模拟 both 形态的历史思维链
	entries := []*types.LogEntry{think}
	v := viewOf(id, entries...)
	c := newCompressor(t, &fakeLLM{}, t.TempDir(), nil)
	res, err := c.Compress(ctx, lg, v, orchestrate.CompressionPolicy{
		HeadroomThreshold: 100, KeepTailTurns: 3, MinReclaimFraction: 0.1,
	})
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if res.L0Pruned != 1 || res.View.Items[0].Visible {
		t.Fatalf("thinking must be excluded: pruned=%d visible=%v", res.L0Pruned, res.View.Items[0].Visible)
	}
	// Log 里的条目原样（audience 迁移是另一条流程，阶段 3 不做）。
	got, err := lg.Get(ctx, think.ID)
	if err != nil || got.Audience != types.AudienceBoth {
		t.Fatalf("log entry altered: %+v err=%v", got, err)
	}
}

// ---------------------------------------------------------------------------
// 压缩主流程
// ---------------------------------------------------------------------------

func TestCompressFullFlow(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "files", "f01.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 5 轮 file_read（每轮内容 ~400 est token），keepTail=2 → 压缩区 3 轮。
	entries := buildRoundsLog(t, ctx, lg, 5, 50)
	v := viewOf("a1", entries...)
	snapshot := fmt.Sprint(*v)
	oldTokens := 0
	for _, e := range entries {
		oldTokens += e.TokenEst
	}

	llm := &fakeLLM{replies: []string{validSUM(root, "files/f01.txt")}}
	sink := &recordingSink{}
	c := newCompressor(t, llm, root, sink)
	res, err := c.Compress(ctx, lg, v, orchestrate.CompressionPolicy{
		HeadroomThreshold: 100, KeepTailTurns: 2, MinReclaimFraction: 0.2, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	// 交付判据 1：新 View 比旧 View 小 ≥20%。
	newTokens := 0
	for _, it := range res.View.Items {
		if !it.Visible {
			continue
		}
		e, err := lg.Get(ctx, it.Ref)
		if err != nil {
			t.Fatal(err)
		}
		newTokens += e.TokenEst
	}
	reclaim := float64(oldTokens-newTokens) / float64(oldTokens)
	if reclaim < 0.2 {
		t.Fatalf("reclaim = %.3f, want >= 0.2 (old=%d new=%d)", reclaim, oldTokens, newTokens)
	}
	if res.Reclaim < 0.2 {
		t.Fatalf("reported reclaim = %v", res.Reclaim)
	}

	// 交付判据 2：SUM 七章节齐全（由校验器保证——能走到这里即通过）。
	if res.SUMENTry == nil || res.SUMENTry.Prov != types.ProvSummaryOf {
		t.Fatalf("SUM entry: %+v", res.SUMENTry)
	}
	// 血缘指向压缩区全部条目：3 轮 × 2 条 = 6 条。
	if len(res.SUMENTry.SourceIDs) != 6 {
		t.Fatalf("sourceIDs = %d, want 6", len(res.SUMENTry.SourceIDs))
	}

	// 新 View 形态：head(1) + SUM(1) + tail(2 轮 × 2 = 4) = 6 条。
	if len(res.View.Items) != 6 {
		t.Fatalf("view items = %d, want 6: %+v", len(res.View.Items), res.View.Items)
	}
	if res.View.Items[0].Ref != entries[0].ID {
		t.Fatal("head (user task) must be kept")
	}
	if res.View.Items[1].Ref != res.SUMENTry.ID || res.View.Items[1].WireRole != types.WireAssistant {
		t.Fatal("SUM must sit after head as assistant")
	}
	if res.View.Validate() != nil {
		t.Fatalf("new view invalid: %v", res.View.Validate())
	}
	// 传入 view 未被修改。
	if fmt.Sprint(*v) != snapshot {
		t.Fatal("input view mutated")
	}
	// 编排调用形态：无 tools、无 JSON mode。
	req := llm.reqs[0]
	if len(req.Tools) != 0 || req.OutputJSON {
		t.Fatalf("SUM call shape: tools=%d json=%v", len(req.Tools), req.OutputJSON)
	}
	// 独立账本收到记账。
	if len(sink.entries) != 1 {
		t.Fatalf("sink entries = %d", len(sink.entries))
	}
	// Log 只追加：原 11 条 + SUM 1 条。
	last, _ := lg.LastSeq(ctx, "a1")
	if int(last) != len(entries)+1 {
		t.Fatalf("log seq = %d, want %d", last, len(entries)+1)
	}
}

func TestCompressNothingToCompress(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	// 只有 2 轮，keepTail=3 → 无压缩区。
	entries := buildRoundsLog(t, ctx, lg, 2, 50)
	v := viewOf("a1", entries...)
	c := newCompressor(t, &fakeLLM{}, t.TempDir(), nil)
	_, err := c.Compress(ctx, lg, v, orchestrate.CompressionPolicy{
		HeadroomThreshold: 100, KeepTailTurns: 3, MinReclaimFraction: 0.2,
	})
	if !errors.Is(err, orchestrate.ErrNothingToCompress) {
		t.Fatalf("err = %v, want ErrNothingToCompress", err)
	}
}

func TestCompressRetriesWithHigherTemperature(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "files", "f01.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := buildRoundsLog(t, ctx, lg, 4, 50)
	v := viewOf("a1", entries...)
	// 第一次回复缺章节（校验失败），第二次完整 → 重试成功。
	llm := &fakeLLM{replies: []string{
		"## 1. 任务\n不完整", // 缺 6 个章节
		validSUM(root, "files/f01.txt"),
	}}
	c := newCompressor(t, llm, root, nil)
	res, err := c.Compress(ctx, lg, v, orchestrate.CompressionPolicy{
		HeadroomThreshold: 100, KeepTailTurns: 2, MinReclaimFraction: 0.2, MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if res.SUMENTry == nil {
		t.Fatal("retry must succeed")
	}
	if len(llm.reqs) != 2 {
		t.Fatalf("calls = %d, want 2", len(llm.reqs))
	}
	if llm.sampling[1].Temperature <= llm.sampling[0].Temperature {
		t.Fatalf("retry temperature must be higher: %v -> %v", llm.sampling[0].Temperature, llm.sampling[1].Temperature)
	}
}

func TestCompressReclaimInsufficient(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "files", "f01.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := buildRoundsLog(t, ctx, lg, 4, 50)
	v := viewOf("a1", entries...)
	// SUM 与被压内容等长（抄原文）→ 收益为负。
	longSUM := validSUM(root, "files/f01.txt") + "\n" + strings.Repeat("padding ", 5000)
	llm := &fakeLLM{replies: []string{longSUM}}
	c := newCompressor(t, llm, root, nil)
	_, err := c.Compress(ctx, lg, v, orchestrate.CompressionPolicy{
		HeadroomThreshold: 100, KeepTailTurns: 2, MinReclaimFraction: 0.2,
	})
	if err == nil || !strings.Contains(err.Error(), "L2 ladder upgrade") {
		t.Fatalf("err = %v, want L2-not-implemented failure", err)
	}
}

func TestCompressSUMValidationExhausted(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	entries := buildRoundsLog(t, ctx, lg, 4, 50)
	v := viewOf("a1", entries...)
	llm := &fakeLLM{replies: []string{"## 1. 任务\n坏输出"}}
	c := newCompressor(t, llm, t.TempDir(), nil)
	_, err := c.Compress(ctx, lg, v, orchestrate.CompressionPolicy{
		HeadroomThreshold: 100, KeepTailTurns: 2, MinReclaimFraction: 0.2, MaxRetries: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "after 2 attempt(s)") {
		t.Fatalf("err = %v", err)
	}
	if len(llm.reqs) != 2 {
		t.Fatalf("calls = %d, want 2 (1 + MaxRetries)", len(llm.reqs))
	}
}

func TestCompressZeroPolicyRejected(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	entries := buildRoundsLog(t, ctx, lg, 3, 10)
	v := viewOf("a1", entries...)
	c := newCompressor(t, &fakeLLM{}, t.TempDir(), nil)
	_, err := c.Compress(ctx, lg, v, orchestrate.CompressionPolicy{})
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// Admit
// ---------------------------------------------------------------------------

func TestAdmit(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	entries := buildRoundsLog(t, ctx, lg, 3, 30)
	v := viewOf("a1", entries...)
	c := newCompressor(t, &fakeLLM{}, t.TempDir(), nil)

	src := []types.MessageID{entries[1].ID, entries[2].ID, entries[3].ID, entries[4].ID}
	res, err := c.Admit(ctx, lg, v, validSUM(t.TempDir(), "files/f01.txt"), src)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	// 7 条 - 4 条被取代 + 1 条 SUM = 4 条。
	if len(res.View.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(res.View.Items))
	}
	if res.View.Items[1].Ref != res.SUMENTry.ID {
		t.Fatal("SUM must take over the first replaced slot")
	}
	if res.SUMENTry.Prov != types.ProvSummaryOf || len(res.SUMENTry.SourceIDs) != 4 {
		t.Fatalf("SUM provenance: %+v", res.SUMENTry)
	}
}

func TestAdmitFailures(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	entries := buildRoundsLog(t, ctx, lg, 2, 10)
	v := viewOf("a1", entries...)
	c := newCompressor(t, &fakeLLM{}, t.TempDir(), nil)

	t.Run("empty args", func(t *testing.T) {
		_, err := c.Admit(ctx, lg, v, "", []types.MessageID{entries[0].ID})
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
		_, err = c.Admit(ctx, lg, v, "sum", nil)
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dangling source", func(t *testing.T) {
		_, err := c.Admit(ctx, lg, v, "sum", []types.MessageID{"nope"})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestL0DedupeDuplicateCallIDs 是真机缺陷回归的第二形态：模型跨轮复用
// 同一 tool_call id（阶段 1 探测证实厂商接受）。归属按"最近前向意图"
// 计算，去重旧读取时其意图（而非被 id 撞上的第一个意图）随之排除。
func TestL0DedupeDuplicateCallIDs(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	const id = types.AgentID("a1")
	sameID := "call-dup"
	mkIntent := func() *types.LogEntry {
		return appendEntry(t, ctx, lg, id, types.RoleAssistantReply, "", map[string]any{
			"tool_calls": []any{map[string]any{"id": sameID, "name": "file_read", "arguments": "{}"}},
		})
	}
	// 手工落库（同 id 的两轮；结果条目的 tool_call_id 显式定制）：
	user := appendEntry(t, ctx, lg, id, types.RoleUserInput, "task", nil)
	i1 := mkIntent()
	r1 := appendEntry(t, ctx, lg, id, types.RoleToolResult,
		fmt.Sprintf(`{"ok":true,"Data":{"path":"files/f01.txt","content":%q}}`, strings.Repeat("big\n", 200)),
		map[string]any{"name": "file_read", "ok": true, "tool_call_id": sameID})
	i2 := mkIntent()
	r2 := appendEntry(t, ctx, lg, id, types.RoleToolResult,
		fmt.Sprintf(`{"ok":true,"Data":{"path":"files/f01.txt","content":%q}}`, strings.Repeat("big\n", 200)),
		map[string]any{"name": "file_read", "ok": true, "tool_call_id": sameID})
	entries := []*types.LogEntry{user, i1, r1, i2, r2}
	v := viewOf(id, entries...)

	c := newCompressor(t, &fakeLLM{replies: []string{"should not be called"}}, t.TempDir(), nil)
	res, err := c.Compress(ctx, lg, v, orchestrate.CompressionPolicy{
		HeadroomThreshold: 100, KeepTailTurns: 3, MinReclaimFraction: 0.2,
	})
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	// 第一轮的意图与结果都被排除；第二轮保持完整且配对成立。
	visRefs := map[types.MessageID]bool{}
	for _, it := range res.View.Items {
		if it.Visible {
			visRefs[it.Ref] = true
		}
	}
	if visRefs[i1.ID] || visRefs[r1.ID] {
		t.Fatal("first round (intent+result) must both be excluded")
	}
	if !visRefs[i2.ID] || !visRefs[r2.ID] {
		t.Fatal("second round must stay intact")
	}
}
