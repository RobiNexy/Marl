package config

// Emit 的往返测试（Parse → Emit → Parse 语义等价；GUI 写回配置的依据）。

import (
	"strings"
	"testing"
)

const sampleConfig = `project:
  ladder_start: "r0"
  max_depth: 3

limits:
  llm_call:
    max_input_tokens: 8000
    task_max_calls: 20
gate_rules:
  - id: allow-low
    match:
      kind: orchestration
      cache_destroyed_pct: "<10"
    action: allow
  - id: review-shell
    match:
      kind: shell
    action: need_human
    reason: "默认只放行 go 工具链，其余命令要人批"
`

func TestEmitRoundTrip(t *testing.T) {
	n1, err := Parse([]byte(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	text := Emit(n1)
	n2, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("re-parse emitted text: %v\n--- emitted ---\n%s", err, text)
	}
	// 语义等价断言：逐路径对关键标量（Get 只取单键——逐层下钻）。
	if got := n2.Get("project").Get("ladder_start").StrOr(""); got != "r0" {
		t.Fatalf("roundtrip ladder_start = %q", got)
	}
	if got, _ := n2.Get("project").Get("max_depth").Int(); got != 3 {
		t.Fatalf("max_depth = %d", got)
	}
	if got, _ := n2.Get("limits").Get("llm_call").Get("task_max_calls").Int(); got != 20 {
		t.Fatalf("task_max_calls = %d", got)
	}
	rules := n2.Get("gate_rules").List()
	if len(rules) != 2 {
		t.Fatalf("rules = %d", len(rules))
	}
	if rules[0].Get("id").StrOr("") != "allow-low" {
		t.Fatalf("rule0 id: %v", rules[0].Get("id").StrOr(""))
	}
	if got := rules[0].Get("match").Get("cache_destroyed_pct").StrOr(""); got != "<10" {
		t.Fatalf("match pct: %q", got)
	}
	if got := rules[1].Get("reason").StrOr(""); got != "默认只放行 go 工具链，其余命令要人批" {
		t.Fatalf("reason: %q", got)
	}
	// 发出的文本不含行内流式形态（规范化输出）。
	if strings.Contains(text, "{") || strings.Contains(text, "[") {
		t.Fatalf("emitted text should be block-style only:\n%s", text)
	}
}

func TestEmitQuoting(t *testing.T) {
	n := &Node{Kind: KindMap, Keys: []string{"a", "b", "c", "d"},
		Vals: []*Node{
			{Kind: KindScalar, Scalar: "plain"},
			{Kind: KindScalar, Scalar: ""},
			{Kind: KindScalar, Scalar: "with: colon"},
			{Kind: KindScalar, Scalar: "42"},
		}}
	out := Emit(n)
	want := `a: plain
b: ""
c: "with: colon"
d: "42"
`
	if out != want {
		t.Fatalf("emit:\n%s\nwant:\n%s", out, want)
	}
}
