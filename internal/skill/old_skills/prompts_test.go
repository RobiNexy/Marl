//go:build ignore

package skill

import (
	"context"
	"testing"
)

// ── list_prompts 契约（Patch 2/3）─────────────────────────────────

// 索引已接线：返回完整索引；空索引返回空数组而非 nil（批处理 DSL 兼容）。
func TestListPromptsReturnsIndex(t *testing.T) {
	ec, _ := newTestEC(t, nil)
	ec.Prompts = func() []PromptSummary {
		return []PromptSummary{
			{ID: "go-engineer", Description: "Go 代码生成/审查/重构"},
			{ID: "code-reviewer", Description: "代码审查"},
		}
	}
	res := mustExec(t, reg(), ec, "list_prompts", map[string]any{})
	if res["ok"] != true {
		t.Fatalf("list_prompts failed: %v", res)
	}
	prompts, ok := res["prompts"].([]PromptSummary)
	if !ok || len(prompts) != 2 {
		t.Fatalf("want 2 prompts, got %v", res["prompts"])
	}
	if prompts[0].ID != "go-engineer" || prompts[1].ID != "code-reviewer" {
		t.Fatalf("prompt order/content wrong: %+v", prompts)
	}
}

// 空索引：合法状态（新项目），返回空列表。
func TestListPromptsEmptyIndex(t *testing.T) {
	ec, _ := newTestEC(t, nil)
	ec.Prompts = func() []PromptSummary { return []PromptSummary{} }
	res := mustExec(t, reg(), ec, "list_prompts", map[string]any{})
	prompts := res["prompts"].([]PromptSummary)
	if len(prompts) != 0 {
		t.Fatalf("want empty list, got %v", prompts)
	}
}

// 未接线（框架未配置索引）：结构化错误而非 panic。
func TestListPromptsUnavailable(t *testing.T) {
	ec, _ := newTestEC(t, nil)
	r := reg()
	res := r.Execute(context.Background(), "list_prompts", ec, map[string]any{})
	if res["ok"] != false || res["error_type"] != "PROMPTS_UNAVAILABLE" {
		t.Fatalf("want structured PROMPTS_UNAVAILABLE, got %v", res)
	}
}

// 注册表契约：list_prompts 是第 9 个注册技能且声明 read_only。
func TestListPromptsRegisteredReadOnly(t *testing.T) {
	r := reg()
	s, ok := r.Get("list_prompts")
	if !ok {
		t.Fatal("list_prompts not registered")
	}
	if s.Capability() != CapReadOnly {
		t.Fatalf("capability: want read_only, got %s", s.Capability())
	}
}
