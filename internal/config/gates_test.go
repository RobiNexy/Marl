package config

// 阶段 11 三块配置的解析契约测试（limits / llm_wires / gate_rules）。

import (
	"strings"
	"testing"
)

func parse3(t *testing.T, src string) *Node {
	t.Helper()
	root, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return root
}

// TestParseLimits：缺块 = 全默认（不是"没有限额"）；覆盖读子键。
func TestParseLimits(t *testing.T) {
	// 缺块 → 默认。
	lim, err := ParseLimits(parse3(t, ""))
	if err != nil || lim.LLMCallMaxInputTokens != 8000 {
		t.Fatalf("default limits: %+v err=%v", lim, err)
	}
	// 覆盖。
	lim2, err := ParseLimits(parse3(t, `limits:
  llm_call:
    max_input_tokens: 4000
    task_max_calls: 5
  orchestration:
    cache_review_pct: 25
`))
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case lim2.LLMCallMaxInputTokens != 4000 || lim2.CallTaskMax != 5:
		t.Fatalf("overrides: %+v", lim2)
	case lim2.CallTaskMaxTokens != 100_000:
		t.Fatalf("default task tokens: %d", lim2.CallTaskMaxTokens)
	}
	// 越界值拒绝。
	_, err = ParseLimits(parse3(t, `limits:
  llm_call:
    max_input_tokens: 0
  orchestration:
    cache_review_pct: 10
`))
	if err == nil || !strings.Contains(err.Error(), "max_input_tokens") {
		t.Fatalf("zero limit: %v", err)
	}
}

// TestParseLlmWires：建立 / 组装完整态（main 禁止显式）。
func TestParseLlmWires(t *testing.T) {
	ws, err := ParseLlmWires(parse3(t, `llm_wires:
  - name: sidecar-cheap
    adapter: deepseek
    endpoint: deepseek-main
    model: deepseek-flash
    thinking: "off"
`))
	if err != nil || len(ws) != 1 {
		t.Fatalf("wires: %+v err=%v", ws, err)
	}
	_, err = ParseLlmWires(parse3(t, `llm_wires:
  - name: main
    adapter: deepseek
    endpoint: x
    model: m
`))
	if err == nil || !strings.Contains(err.Error(), "活引用") {
		t.Fatalf("main ban: %v", err)
	}
}

// TestParseGateRules：顺序保留（首中生效）+ id/action 缺失拒绝。
func TestParseGateRules(t *testing.T) {
	rs, err := ParseGateRules(parse3(t, `gate_rules:
  - id: allow-low-destruction
    match: {kind: orchestration, cache_destroyed_pct: "<10"}
    action: allow
  - id: review-destructive
    match: {kind: orchestration}
    action: need_human
    reason: "cache wreak"
`))
	if err != nil || len(rs) != 2 {
		t.Fatalf("rules: %+v err=%v", rs, err)
	}
	if rs[0].ID != "allow-low-destruction" || rs[0].Match["cache_destroyed_pct"] != "<10" {
		t.Fatalf("rule0: %+v", rs[0])
	}
	if _, err := ParseGateRules(parse3(t, `gate_rules:
  - id: x
    action: allow
`)); err == nil || !strings.Contains(err.Error(), "match") {
		t.Fatalf("missing match: %v", err)
	}
}
