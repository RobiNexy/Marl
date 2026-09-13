package agent

// 私有段与常驻块的编译契约测试（Part 6.4 修正 1 / 6.10 / 12.3，13.9）。

import (
	"context"
	"strings"
	"testing"

	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// TestCompileStandingOrderPlacement：常驻块出现在 system 之后、历史段
// 之前；Stability=frozen（13.9 测试条目："验证出现在 system 之后、私有
// 段之前"）。
func TestCompileStandingOrdersPlacement(t *testing.T) {
	llm := &fakeLLM{turns: []*wire.WireTurn{replyTurn("完成。")}}
	a, _ := newTestAgent(t, llm)
	a.standingOrders = "<standing_orders>\n## test.md\n\n偏好指令\n</standing_orders>"
	if err := a.AppendUser(context.Background(), "任务"); err != nil {
		t.Fatal(err)
	}
	req, err := a.compileView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Segments) < 3 {
		t.Fatalf("segments = %d, want >=3 (system+standing+private+user)", len(req.Segments))
	}
	seg := req.Segments[1]
	if seg.Kind != wire.SegStanding || seg.Stability != types.StabilityFrozen {
		t.Fatalf("seg[1] = %+v", seg)
	}
	if !strings.HasPrefix(seg.Content, "<standing_orders>") {
		t.Fatalf("seg[1].Content = %q", seg.Content[:30])
	}
	// 私有段在常驻块之后、user 之前。
	priv := req.Segments[2]
	if priv.Kind != wire.SegKnowledge {
		t.Fatalf("seg[2].Kind = %v", priv.Kind)
	}
	round2, err := a.compileView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if round2.Segments[1].Content != seg.Content {
		t.Fatal("standing segment must be byte-stable across compiles")
	}
}

// TestPrivateSegmentDiscipline：私有段只陈述拥有的权限（hidden 不出现，
// Part 6.10 约束 1），任务描述压单行，depth 只陈述。
func TestPrivateSegmentDiscipline(t *testing.T) {
	ns := &types.Namespace{AgentID: "x", Mounts: []types.Mount{
		{Pattern: "src/**", Mode: types.PathWrite},
		{Pattern: "docs/**", Mode: types.PathRead},
		{Pattern: ".marl/discussions/**", Mode: types.PathHidden},
	}}
	got := renderAgentContext(2, 3, ns, "divide-and\nconquer task")
	if !strings.Contains(got, `<depth current="2" max="3"/>`) {
		t.Fatalf("depth missing: %s", got)
	}
	if strings.Contains(got, "discussions") {
		t.Fatal("hidden mounts must never appear in the private segment (saying X exists is handing over X)")
	}
	if !strings.Contains(got, `    <writable>src/**</writable>`) || !strings.Contains(got, `    <readable>docs/**</readable>`) {
		t.Fatalf("workspace section wrong: %s", got)
	}
	if strings.Contains(got, "\ndivide") {
		t.Fatal("task text must be single-line")
	}
	if strings.Contains(got, "\n你已经是第 2 层") {
		t.Fatal("depth must be stated, not explained")
	}
}

// TestNoStandingNoPrivateOmission：无常驻块时不产生空段；无挂载/任务时
// 私有段仅含 depth。
func TestNoOmission(t *testing.T) {
	if standingSegment("") != nil {
		t.Fatal("empty standing must omit the segment")
	}
	seg := privateSegment(0, 0, nil, "")
	if seg == nil || !strings.Contains(seg.Content, `<depth current="0" max="0"/>`) {
		t.Fatalf("bare depth segment required: %+v", seg)
	}
}
