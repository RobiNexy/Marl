package agent

// 编排五件套技能的端到端测试（阶段 11 补遗：target_text 定位 / 破坏分级
// Gate / grant 直行 / AMBIGUOUS_TARGET 摘录）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RobiNexy/Marl/internal/gate"
	"github.com/RobiNexy/Marl/internal/skill"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

func orchRegistry(t *testing.T) skill.Registry {
	t.Helper()
	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.OrchExclude, skill.OrchRestore, skill.OrchReorder, skill.OrchAnnotate, skill.OrchPin} {
		if err := reg.Register(sk); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// setupOrchAgent 是带编排五件套与 limits 的 agent（newTestAgent 之后注入）。
func setupOrch(t *testing.T, llm *fakeLLM, rules []gate.Rule) *Agent {
	a, _ := llmCallAgentAndFake(t, func(c *LLMCallConfig) {
		c.Gates = gateMustManager(t, rules)
	})
	// 测试的"人类"（ScriptedHuman 的进程内形态）：审批请求 → 立即回信
	// allow + grant next 2 次编排（覆盖 need_human → 恢复 → 模型重发直行
	// 的链路；Part 11.3 §3.3 / Part 14.7 的信封往返）。
	link := newFakeHumanLink(nil)
	wireHuman(a, link)
	a.llm = llm // llmCallAgentAndFake 的默认脚本是 llm_call 面的——编排测试换成模型脚本
	a.skills = orchRegistry(t)
	a.authorizer = skill.NewAuthorizer(a.skills, nil)
	return a
}

func orchCallOne(name string, args map[string]any) *wire.WireTurn {
	b, _ := json.Marshal(args)
	return toolCallTurn(types.ToolCall{ID: "call-" + name, Name: name, Arguments: b})
}

// TestOrchExcludeByTargetText：低破坏率（尾部）→ 直接执行（Visible=false）。
func TestViewOpExcludeByTargetText(t *testing.T) {
	ctx := context.Background()
	// 第一次 GATE_PENDING（尾部 25.8% 超阈值——Part 11.4 的"低"档在 4 条
	// 的上下文里仍超 10%）；审批 grant 后模型**重发**（grant 内不再打扰
	// 人类的直接证据——Part 11.3 §3.3 的"问一次给一批"）。
	llm := &fakeLLM{turns: []*wire.WireTurn{
		orchCallOne("exclude_message", map[string]any{
			"target_text": "明天再补充背景。",
		}),
		orchCallOne("exclude_message", map[string]any{
			"target_text": "明天再补充背景。",
		}),
		replyTurn("尾部条目已删。"),
	}}
	a := setupOrch(t, llm, append([]gate.Rule(nil), orchRulesDefaults...))
	for _, txt := range []string{"第一条：任务背景，很长很长。", "第二条：核心论证。", "明天再补充背景。"} {
		if err := a.AppendUser(ctx, txt); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// View：最后一条（含"明天再补充背景"）变为不可见——直接断言那一个
	// 条目的 Visible（追加的 assistant 空回与 tool_result 都在后缀里，
	// 只扣一条是准确的语义）。
	a.mu.Lock()
	targetExcluded := false
	for i := range a.view.Items {
		e, err := a.log.Get(ctx, a.view.Items[i].Ref)
		if err == nil {
			t.Logf("entry[%d] visible=%v role=%s content=%.100s", i, a.view.Items[i].Visible, e.Role, e.Content)
			if strings.Contains(e.Content, "明天再补充背景") && !a.view.Items[i].Visible {
				targetExcluded = true
			}
		}
	}
	a.mu.Unlock()
	if !targetExcluded {
		t.Fatal("target entry should be invisible after exclude")
	}
	entries := entriesMust(t, a)
	sawOK := false
	for _, e := range entries {
		if e.Role == types.RoleToolResult && strings.Contains(e.Content, `"ok":true`) &&
			strings.Contains(e.Content, "exclude_message") {
			sawOK = true
		}
	}
	if !sawOK {
		t.Fatal("exclude tool_result missing")
	}
}

// TestViewOpAmbiguous：多命中 → AMBIGUOUS_TARGET 带"两处摘录"。
func TestViewOpAmbiguous(t *testing.T) {
	ctx := context.Background()
	dup := "同样的句式内容用来制造歧义命中。"
	llm := &fakeLLM{turns: []*wire.WireTurn{
		orchCallOne("exclude_message", map[string]any{"target_text": "同样的句式内容"}),
		replyTurn("收到歧义反馈。"),
	}}
	a := setupOrch(t, llm, nil)
	a.AppendUser(ctx, dup)
	a.AppendUser(ctx, dup)
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries := entriesMust(t, a)
	found := false
	for _, e := range entries {
		t.Logf("entry[%d] role=%s: %.180s", e.Seq, e.Role, e.Content)
		if e.Role == types.RoleToolResult && strings.Contains(e.Content, "AMBIGUOUS_TARGET") {
			found = true
		}
	}
	if !found {
		t.Fatalf("AMBIGUOUS_TARGET missing: %v", entries)
	}
}

// TestViewOpHighDestructionPending：头部附近 exclude → 破坏率超阈 →
// GATE_PENDING 结果 + gatePending 挂起；批准后（Run 外层的 awaitGate）恢复。
func TestViewOpHighDestructionPending(t *testing.T) {
	ctx := context.Background()
	// 模型的重发行为（Part 11.3 §3.3：grant 在额度内不再打扰人类——
	// 第二次直接直行；承认"框架不做审批后自动重放"的口径：重发是
	// Agent 的下一个 tool_call，不是框架动作）。
	llm := &fakeLLM{turns: []*wire.WireTurn{
		orchCallOne("exclude_message", map[string]any{
			// 头部的第一条 user（Position 序的开头）→ 后续全部失效。
			"target_text": "第一条：任务背景，很长很长。",
		}),
		orchCallOne("exclude_message", map[string]any{
			"target_text": "第一条：任务背景，很长很长。",
		}),
		replyTurn("审批通过后排除生效。"),
	}}
	// 默认编排规则表（Part 11.5 的字面形态；测试与装配共判据面）：
	// 低破坏 allow、高破坏 need_human（exclude 头部条目 → pct 高 → 挂起）。
	a := setupOrch(t, llm, append([]gate.Rule(nil), orchRulesDefaults...))
	for _, txt := range []string{"第一条：任务背景，很长很长。", "第二条：核心论证。", "第三条：细节。", "第四条：收尾。"} {
		a.AppendUser(ctx, txt)
	}
	// GATE_PENDING 的 tool_result 回到模型 + pending 挂起（eventLoop 尾部
	// errGatePending → Run 的 awaitGate）。fake 的第二轮 turn 让 Run 在
	// 审批后收尾。
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run（含 pending→审批→恢复）: %v", err)
	}
	entries := entriesMust(t, a)
	sawPending := false
	for _, e := range entries {
		if strings.Contains(e.Content, "GATE_PENDING_HUMAN") {
			sawPending = true
		}
		if e.Role == types.RoleToolResult || true {
			t.Logf("entry[%d] role=%s: %.160s", e.Seq, e.Role, e.Content)
		}
	}
	if !sawPending {
		t.Fatalf("GATE_PENDING result missing: %v", entries)
	}
	// 审批通过后的恢复面（Gate 裁决条目入 log）。
	sawVerdict := false
	for _, e := range entries {
		if e.Role == types.RoleHumanNote && strings.Contains(e.Content, "Gate 裁决") {
			sawVerdict = true
		}
	}
	if !sawVerdict {
		t.Fatal("gate verdict entry missing")
	}
	// 头部条目现在真的被删了（第二轮执行）。
	a.mu.Lock()
	headVisible := a.view.Items[0].Visible
	e0, _ := a.log.Get(ctx, a.view.Items[0].Ref)
	if e0 == nil || !strings.Contains(e0.Content, "第一条") {
		headVisible = false
	}
	a.mu.Unlock()
	if headVisible {
		t.Fatal("first entry should be excluded after grant")
	}
}
