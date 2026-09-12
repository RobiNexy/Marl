package orchestrate

// 编排操作测试（13.5 的测试规格）。
//
// 断言的是契约而非实现：
//   - "返回新 View，不就地修改"：每个用例都对传入 view 做深比较；
//   - split 的硬不变量：分段覆盖原文、无重叠、段非空、切点不落禁切区；
//   - 错误语义：参数非法 → ErrInvalid；引用不存在 → ErrNotFound。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"marl/internal/store"
	"marl/internal/types"
)

// ---------------------------------------------------------------------------
// 测试基建
// ---------------------------------------------------------------------------

// newLog 每个用例独立的 SQLite Log（临时目录，随 t 清理）。
func newLog(t *testing.T) store.MessageLog {
	t.Helper()
	path := t.TempDir() + "/ops.db"
	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// appendEntry 落一条原始条目并回填 ID/Seq。
func appendEntry(t *testing.T, ctx context.Context, lg store.MessageLog, agentID types.AgentID, role types.InternalRole, content string, meta map[string]any) *types.LogEntry {
	t.Helper()
	e := types.NewLogEntry(agentID, role, content)
	e.Meta = meta
	if _, err := lg.Append(ctx, e); err != nil {
		t.Fatalf("append %s: %v", role, err)
	}
	return e
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

// requireViewUnchanged 断言"Apply 不修改传入 view"（Operation 后置条件）。
func requireViewUnchanged(t *testing.T, before, after *types.ContextView) {
	t.Helper()
	if fmt.Sprint(*before) != fmt.Sprint(*after) {
		t.Fatalf("input view was mutated:\nbefore=%+v\nafter=%+v", *before, *after)
	}
}

// assertCompileOrder 断言按 Position 升序（编译序）的引用序列。
func assertCompileOrder(t *testing.T, v *types.ContextView, want ...types.MessageID) {
	t.Helper()
	items := make([]types.ViewItem, len(v.Items))
	copy(items, v.Items)
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].Position < items[j-1].Position; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	for i, w := range want {
		if items[i].Ref != w {
			t.Fatalf("compile order[%d] = %s, want %s (all: %+v)", i, items[i].Ref, w, items)
		}
	}
}

// fakeOrchestrator 返回固定段集合（或错误），并记录收到的输入。
type fakeOrchestrator struct {
	segs    []SplitSegment
	err     error
	gotCall string
}

func (f *fakeOrchestrator) SplitSemantic(_ context.Context, content string) ([]SplitSegment, error) {
	f.gotCall = content
	return f.segs, f.err
}

// fakeScanner 返回固定禁切区。
type fakeScanner struct {
	zones []NoSplitZone
}

func (f fakeScanner) ScanNoSplitZones(string) []NoSplitZone { return f.zones }

// ---------------------------------------------------------------------------
// 参数校验（表驱动：零值与跨字段规则）
// ---------------------------------------------------------------------------

func TestParamValidation(t *testing.T) {
	validSeg := SplitSegment{Topic: "t", StartLine: 1, EndLine: 2}
	cases := []struct {
		name    string
		check   func() error
		wantErr bool
	}{
		{"split empty target", func() error { return SplitParams{Strategy: SplitDelimiter, Delimiter: "-"}.Validate() }, true},
		{"split bad strategy", func() error { return SplitParams{Target: "m", Strategy: "x", Delimiter: "-"}.Validate() }, true},
		{"split delimiter needs delimiter", func() error { return SplitParams{Target: "m", Strategy: SplitDelimiter}.Validate() }, true},
		{"split delimiter rejects segments", func() error {
			return SplitParams{Target: "m", Strategy: SplitDelimiter, Delimiter: "-", Segments: []SplitSegment{validSeg}}.Validate()
		}, true},
		{"split semantic needs segments", func() error { return SplitParams{Target: "m", Strategy: SplitSemantic}.Validate() }, true},
		{"split semantic rejects delimiter", func() error {
			return SplitParams{Target: "m", Strategy: SplitSemantic, Delimiter: "-", Segments: []SplitSegment{validSeg}}.Validate()
		}, true},
		{"split ok", func() error { return SplitParams{Target: "m", Strategy: SplitDelimiter, Delimiter: "-"}.Validate() }, false},
		{"segment zero start", func() error { return SplitSegment{Topic: "t"}.Validate() }, true},
		{"segment inverted", func() error { return SplitSegment{Topic: "t", StartLine: 3, EndLine: 2}.Validate() }, true},
		{"segment empty topic", func() error { return SplitSegment{StartLine: 1, EndLine: 1}.Validate() }, true},
		{"segment ok", func() error { return validSeg.Validate() }, false},
		{"exclude empty", func() error { return ExcludeParams{}.Validate() }, true},
		{"exclude ok", func() error { return ExcludeParams{Target: "m"}.Validate() }, false},
		{"restore empty", func() error { return RestoreParams{}.Validate() }, true},
		{"reorder NaN", func() error { return ReorderParams{Target: "m", Position: math.NaN()}.Validate() }, true},
		{"reorder +Inf", func() error { return ReorderParams{Target: "m", Position: math.Inf(1)}.Validate() }, true},
		{"reorder zero position is legal", func() error { return ReorderParams{Target: "m"}.Validate() }, false},
		{"annotate empty note", func() error { return AnnotateParams{Target: "m"}.Validate() }, true},
		{"pin empty", func() error { return PinParams{}.Validate() }, true},
		{"zone zero kind", func() error { return NoSplitZone{Start: 1, End: 2}.Validate() }, true},
		{"zone inverted", func() error { return NoSplitZone{Kind: ZoneCodeFence, Start: 3, End: 3}.Validate() }, false}, // End==Start 合法（单行区）
		{"zone start 0", func() error { return NoSplitZone{Kind: ZoneCodeFence, End: 2}.Validate() }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.check()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestCompressionPolicyValidation(t *testing.T) {
	cases := []struct {
		name    string
		policy  CompressionPolicy
		wantErr bool
	}{
		{"zero policy rejected", CompressionPolicy{}, true},
		{"zero threshold", CompressionPolicy{KeepTailTurns: 3, MinReclaimFraction: 0.2}, true},
		{"zero tail", CompressionPolicy{HeadroomThreshold: 100, MinReclaimFraction: 0.2}, true},
		{"zero reclaim accepts everything", CompressionPolicy{HeadroomThreshold: 100, KeepTailTurns: 3}, true},
		{"reclaim >= 1", CompressionPolicy{HeadroomThreshold: 100, KeepTailTurns: 3, MinReclaimFraction: 1}, true},
		{"reclaim NaN", CompressionPolicy{HeadroomThreshold: 100, KeepTailTurns: 3, MinReclaimFraction: math.NaN()}, true},
		{"negative retries", CompressionPolicy{HeadroomThreshold: 100, KeepTailTurns: 3, MinReclaimFraction: 0.2, MaxRetries: -1}, true},
		{"zero retries is explicit no-retry", CompressionPolicy{HeadroomThreshold: 100, KeepTailTurns: 3, MinReclaimFraction: 0.2}, false},
		{"full valid", CompressionPolicy{HeadroomThreshold: 100, KeepTailTurns: 3, MinReclaimFraction: 0.2, MaxRetries: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// exclude / restore / pin / unpin
// ---------------------------------------------------------------------------

func TestFlagOps(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	e1 := appendEntry(t, ctx, lg, "a1", types.RoleUserInput, "task", nil)
	e2 := appendEntry(t, ctx, lg, "a1", types.RoleAssistantReply, "reply", nil)

	cases := []struct {
		name  string
		op    Operation
		check func(t *testing.T, v *types.ContextView)
	}{
		{"exclude hides", NewExcludeOp(ExcludeParams{Target: e1.ID}), func(t *testing.T, v *types.ContextView) {
			if v.Items[0].Visible {
				t.Fatal("exclude must set Visible=false")
			}
			if !v.Items[1].Visible {
				t.Fatal("other items untouched")
			}
		}},
		{"restore shows", NewRestoreOp(RestoreParams{Target: e1.ID}), func(t *testing.T, v *types.ContextView) {
			if !v.Items[0].Visible {
				t.Fatal("restore must set Visible=true")
			}
		}},
		{"pin exempts", NewPinOp(PinParams{Target: e2.ID}), func(t *testing.T, v *types.ContextView) {
			if !v.Items[1].Pinned {
				t.Fatal("pin must set Pinned=true")
			}
		}},
		{"unpin clears", NewUnpinOp(PinParams{Target: e2.ID}), func(t *testing.T, v *types.ContextView) {
			if v.Items[1].Pinned {
				t.Fatal("unpin must set Pinned=false")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 从"已 exclude"的起点出发，让 restore 有意义。
			v := viewOf("a1", e1, e2)
			v.Items[0].Visible = false
			snapshot := fmt.Sprint(*v)
			res, err := tc.op.Apply(ctx, lg, v)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if res.View == nil || res.View.AgentID != "a1" {
				t.Fatalf("result view: %+v", res.View)
			}
			if fmt.Sprint(*v) != snapshot {
				t.Fatal("input view mutated")
			}
			tc.check(t, res.View)
			if res.View.Validate() != nil {
				t.Fatalf("result view invalid: %v", res.View.Validate())
			}
		})
	}
}

func TestFlagOpMissingTarget(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	e1 := appendEntry(t, ctx, lg, "a1", types.RoleUserInput, "task", nil)
	v := viewOf("a1", e1)
	_, err := NewExcludeOp(ExcludeParams{Target: "nope"}).Apply(ctx, lg, v)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// reorder：Fractional Index + Stability 单调
// ---------------------------------------------------------------------------

func TestReorder(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	e1 := appendEntry(t, ctx, lg, "a1", types.RoleUserInput, "one", nil)
	e2 := appendEntry(t, ctx, lg, "a1", types.RoleAssistantReply, "two", nil)
	e3 := appendEntry(t, ctx, lg, "a1", types.RoleToolResult, "three", nil)

	t.Run("moves within stable band", func(t *testing.T) {
		v := viewOf("a1", e1, e2, e3)
		res, err := NewReorderOp(ReorderParams{Target: e3.ID, Position: 1.5}).Apply(ctx, lg, v)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		// 编译序（按 Position 升序）：e1(1) e3(1.5) e2(2)。
		// View 的 Items 切片顺序不变——顺序的唯一权威是 Position（编译层排序）。
		assertCompileOrder(t, res.View, e1.ID, e3.ID, e2.ID)
		requireViewUnchanged(t, v, v)
	})

	t.Run("position zero is legal (front)", func(t *testing.T) {
		v := viewOf("a1", e1, e2)
		res, err := NewReorderOp(ReorderParams{Target: e2.ID, Position: 0}).Apply(ctx, lg, v)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		assertCompileOrder(t, res.View, e2.ID, e1.ID)
	})

	t.Run("volatile across stable rejected", func(t *testing.T) {
		v := viewOf("a1", e1, e2)
		v.Items[1].Stability = types.StabilityVolatile
		v.Items[1].Position = 3
		_, err := NewReorderOp(ReorderParams{Target: e2.ID, Position: 0.5}).Apply(ctx, lg, v)
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid (stability boundary)", err)
		}
	})

	t.Run("missing target", func(t *testing.T) {
		v := viewOf("a1", e1)
		_, err := NewReorderOp(ReorderParams{Target: "nope", Position: 1}).Apply(ctx, lg, v)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// ---------------------------------------------------------------------------
// annotate：批注外壳
// ---------------------------------------------------------------------------

func TestAnnotate(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	e1 := appendEntry(t, ctx, lg, "a1", types.RoleUserInput, "original body", nil)
	v := viewOf("a1", e1)

	res, err := NewAnnotateOp(AnnotateParams{Target: e1.ID, Note: "priority: high"}).Apply(ctx, lg, v)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Appended) != 1 {
		t.Fatalf("appended = %d", len(res.Appended))
	}
	a := res.Appended[0]
	if a.Prov != types.ProvAnnotatedOf || len(a.SourceIDs) != 1 || a.SourceIDs[0] != e1.ID {
		t.Fatalf("provenance: %+v", a)
	}
	if !strings.Contains(a.Content, "<note>") || !strings.Contains(a.Content, "priority: high") || !strings.Contains(a.Content, "original body") {
		t.Fatalf("shell content: %q", a.Content)
	}
	if res.View.Items[0].Ref != a.ID {
		t.Fatal("view must reference the shell entry")
	}
	// 原文仍在 Log（真相不动）。
	if _, err := lg.Get(ctx, e1.ID); err != nil {
		t.Fatalf("original must stay in log: %v", err)
	}
}

func TestAnnotateRejects(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	call := appendEntry(t, ctx, lg, "a1", types.RoleAssistantReply, "", map[string]any{
		"tool_calls": []any{map[string]any{"id": "c1", "name": "list_dir"}},
	})
	think := appendEntry(t, ctx, lg, "a1", types.RoleThinking, "<r>think</r>", nil)

	cases := []struct {
		name   string
		target types.MessageID
	}{
		{"tool-call intent entry", call.ID},
		{"thinking entry", think.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := viewOf("a1", call, think)
			_, err := NewAnnotateOp(AnnotateParams{Target: tc.target, Note: "n"}).Apply(ctx, lg, v)
			if !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// split：delimiter 机械切
// ---------------------------------------------------------------------------

func TestSplitDelimiter(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	content := "alpha line\n---\nbeta line\n---\n\ngamma line"
	e1 := appendEntry(t, ctx, lg, "a1", types.RoleUserInput, content, nil)
	v := viewOf("a1", e1)
	snapshot := fmt.Sprint(*v)

	res, err := NewSplitOp(SplitParams{Target: e1.ID, Strategy: SplitDelimiter, Delimiter: "---"}, nil, nil).Apply(ctx, lg, v)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Appended) != 3 {
		t.Fatalf("pieces = %d, want 3", len(res.Appended))
	}
	// 分隔符行本身丢弃；三个非空段覆盖原文其余内容。
	got := make([]string, 0, 3)
	for _, p := range res.Appended {
		if p.Prov != types.ProvSplitOf || len(p.SourceIDs) != 1 || p.SourceIDs[0] != e1.ID {
			t.Fatalf("piece provenance: %+v", p)
		}
		got = append(got, p.Content)
	}
	want := []string{"alpha line", "beta line", "gamma line"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("piece[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// View：原条目被三段替换，位置保序且落在原位附近。
	if len(res.View.Items) != 3 {
		t.Fatalf("view items = %d", len(res.View.Items))
	}
	for i := 1; i < len(res.View.Items); i++ {
		if res.View.Items[i].Position <= res.View.Items[i-1].Position {
			t.Fatalf("positions not increasing: %+v", res.View.Items)
		}
	}
	if fmt.Sprint(*v) != snapshot {
		t.Fatal("input view mutated")
	}
	// 原条目仍在 Log。
	if _, err := lg.Get(ctx, e1.ID); err != nil {
		t.Fatalf("original must stay in log: %v", err)
	}
}

func TestSplitDelimiterFailures(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	e1 := appendEntry(t, ctx, lg, "a1", types.RoleUserInput, "no delimiter here", nil)
	v := viewOf("a1", e1)

	t.Run("delimiter not found", func(t *testing.T) {
		_, err := NewSplitOp(SplitParams{Target: e1.ID, Strategy: SplitDelimiter, Delimiter: "###"}, nil, nil).Apply(ctx, lg, v)
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
	t.Run("missing target", func(t *testing.T) {
		_, err := NewSplitOp(SplitParams{Target: "nope", Strategy: SplitDelimiter, Delimiter: "x"}, nil, nil).Apply(ctx, lg, v)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// ---------------------------------------------------------------------------
// split：semantic（LLM 段 → 修复 → 渲染）
// ---------------------------------------------------------------------------

func TestSplitSemanticRepair(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	// 10 行正文，第 4-6 行是代码块（禁切区）。
	body := "l1\nl2\nl3\n```\nc1\nc2\n```\nl8\nl9\nl10"
	e1 := appendEntry(t, ctx, lg, "a1", types.RoleUserInput, body, nil)
	v := viewOf("a1", e1)

	// LLM 给出的段：越界、重叠、带缝隙、切点落进代码块——全部要被修复。
	orch := &fakeOrchestrator{segs: []SplitSegment{
		{Topic: "head", StartLine: 1, EndLine: 4},   // 4 在代码块内 → 吸附
		{Topic: "mid", StartLine: 4, EndLine: 8},    // 与 head 重叠 + 覆盖代码块
		{Topic: "tail", StartLine: 20, EndLine: 30}, // 越界 → clamp
	}}
	zones := fakeScanner{zones: []NoSplitZone{{Kind: ZoneCodeFence, Start: 4, End: 7}}}
	res, err := NewSplitOp(SplitParams{Target: e1.ID, Strategy: SplitSemantic, Segments: orch.segs}, orch, zones).Apply(ctx, lg, v)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 校验修复结果：行区间连续覆盖 [1,10]、无重叠、切点不落禁切区。
	type iv struct{ s, e int }
	var ranges []iv
	var topics []string
	for _, p := range res.Appended {
		if p.Prov != types.ProvSplitOf {
			t.Fatalf("provenance: %+v", p)
		}
		lines, _ := p.Meta["lines"].(string)
		var s, e int
		if _, err := fmt.Sscanf(lines, "%d-%d", &s, &e); err != nil {
			t.Fatalf("meta lines %q: %v", lines, err)
		}
		ranges = append(ranges, iv{s, e})
		topics = append(topics, p.Meta["topic"].(string))
	}
	for i := range ranges {
		if i > 0 && ranges[i].s != ranges[i-1].e+1 {
			t.Fatalf("gap/overlap at %v: %v", ranges, ranges[i])
		}
	}
	if ranges[0].s != 1 || ranges[len(ranges)-1].e != 10 {
		t.Fatalf("coverage broken: %v", ranges)
	}
	for _, r := range ranges {
		for _, z := range []NoSplitZone{{Kind: ZoneCodeFence, Start: 4, End: 7}} {
			// 切点 = r.e；不得满足 z.Start <= r.e < z.End。
			if r.e >= z.Start && r.e < z.End && r.e != 10 {
				t.Fatalf("cut %d inside zone %v: %v", r.e, z, ranges)
			}
		}
	}
	// 渲染形态：<segment topic="..."> 包裹原文行。
	if !strings.Contains(res.Appended[0].Content, "<segment topic=") {
		t.Fatalf("segment render: %q", res.Appended[0].Content)
	}
	// Orchestrator 收到的是带行号的渲染（契约）。
	if !strings.Contains(orch.gotCall, " 1| l1") {
		t.Fatalf("numbered render: %q", orch.gotCall)
	}
}

func TestSplitSemanticEdgeCases(t *testing.T) {
	ctx := context.Background()
	lg := newLog(t)
	e1 := appendEntry(t, ctx, lg, "a1", types.RoleUserInput, "a\nb\nc", nil)
	v := viewOf("a1", e1)

	seg := []SplitSegment{{Topic: "x", StartLine: 1, EndLine: 3}} // 过参数校验的占位段

	t.Run("no segments from llm", func(t *testing.T) {
		orch := &fakeOrchestrator{segs: nil}
		_, err := NewSplitOp(SplitParams{Target: e1.ID, Strategy: SplitSemantic, Segments: seg}, orch, fakeScanner{}).Apply(ctx, lg, v)
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
	t.Run("single segment after repair", func(t *testing.T) {
		orch := &fakeOrchestrator{segs: []SplitSegment{{Topic: "all", StartLine: 1, EndLine: 3}}}
		_, err := NewSplitOp(SplitParams{Target: e1.ID, Strategy: SplitSemantic, Segments: seg}, orch, fakeScanner{}).Apply(ctx, lg, v)
		if !errors.Is(err, ErrSingleSegment) {
			t.Fatalf("err = %v, want ErrSingleSegment", err)
		}
	})
	t.Run("orchestrator error propagates", func(t *testing.T) {
		orch := &fakeOrchestrator{err: errors.New("llm down")}
		_, err := NewSplitOp(SplitParams{Target: e1.ID, Strategy: SplitSemantic, Segments: seg}, orch, fakeScanner{}).Apply(ctx, lg, v)
		if err == nil || !strings.Contains(err.Error(), "llm down") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing dependencies", func(t *testing.T) {
		_, err := NewSplitOp(SplitParams{Target: e1.ID, Strategy: SplitSemantic}, nil, nil).Apply(ctx, lg, v)
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
}

// TestSplitSemanticZoneMergeAtEdge 禁切区顶到文末且前方无余地时，
// 唯一出路是撤销切点（合并两侧段）——修复不得产出空段或越界切点。
func TestSplitSemanticZoneMergeAtEdge(t *testing.T) {
	// 6 行；代码块占 3-6（顶到文末）；前段只到第 2 行。
	// LLM 给三段，切点 3 与 5 都在区内；3-1=2 > 前切点 0 → 第一个切点吸附到 2；
	// 第二个切点 5 在区内且 z.End=6 == totalLines、k2=6 不 < 后段末行 → 合并。
	segs := []SplitSegment{
		{Topic: "a", StartLine: 1, EndLine: 3},
		{Topic: "b", StartLine: 3, EndLine: 5},
		{Topic: "c", StartLine: 5, EndLine: 6},
	}
	zones := []NoSplitZone{{Kind: ZoneCodeFence, Start: 3, End: 6}}
	got, err := repairSegments(segs, 6, zones)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("segments = %+v, want 2", got)
	}
	if got[0].StartLine != 1 || got[0].EndLine != 2 {
		t.Fatalf("seg0 = %+v", got[0])
	}
	if got[1].StartLine != 3 || got[1].EndLine != 6 {
		t.Fatalf("seg1 = %+v", got[1])
	}
}

// TestSplitSemanticZeroValueStartLine 零值 StartLine（非法值）被 clamp 到 1
// 而不是静默产出 [0,...] 的越界段。
func TestSplitSemanticZeroValueStartLine(t *testing.T) {
	segs := []SplitSegment{
		{Topic: "a", EndLine: 2}, // StartLine 零值
		{Topic: "b", StartLine: 3, EndLine: 4},
	}
	got, err := repairSegments(segs, 4, nil)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if got[0].StartLine != 1 || got[1].StartLine != 3 {
		t.Fatalf("segments = %+v", got)
	}
}

// ---------------------------------------------------------------------------
// RenderNumbered
// ---------------------------------------------------------------------------

func TestRenderNumbered(t *testing.T) {
	got := RenderNumbered("a\nb")
	want := "1| a\n2| b\n"
	if got != want {
		t.Fatalf("render = %q, want %q", got, want)
	}
	// 10 行以上右对齐（与 file_read 同风格）。
	got = RenderNumbered(strings.Repeat("x\n", 9) + "y")
	if !strings.Contains(got, "10| y") || !strings.Contains(got, " 1| x") {
		t.Fatalf("width alignment: %q", got)
	}
	if RenderNumbered("") != "" {
		t.Fatal("empty content renders empty")
	}
}
