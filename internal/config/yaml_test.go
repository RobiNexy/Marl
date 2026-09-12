package config

// mini-YAML 子集解析器的测试：覆盖 ladder.yaml 的实际形态（块映射、
// 列表项映射、行内流式映射、注释、引号），以及显式拒绝的非法形态。

import (
	"strings"
	"testing"
)

const sampleLadder = `# 阶梯配置（子集形态）
ladder:
  - id: "r0"
    endpoint: "deepseek-main"
    model: "deepseek/chat"
    thinking: {level: "off"}
    description: "快速、便宜，适合探索与编排"
    cost_per_mtok: 1.0
    currency: "CNY"

  - id: "r1"
    endpoint: "deepseek-main"
    model: "deepseek/chat"
    thinking:
      level: "high"
    description: "同模型开思维，缓存部分保留"
    cost_per_mtok: 1.0
    currency: "CNY"

  - id: "r2"
    endpoint: "deepseek-main"
    model: "deepseek/v4-pro"
    thinking: {level: "high"}
    description: "更强模型"
    cost_per_mtok: 4.0
    currency: "CNY"

start: "r0"
`

func TestParseLadderShape(t *testing.T) {
	root, err := Parse([]byte(sampleLadder))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ladder := root.Get("ladder")
	if ladder == nil || ladder.Kind != KindList || len(ladder.Items) != 3 {
		t.Fatalf("ladder list: %+v", ladder)
	}
	r0 := ladder.Items[0]
	if got, _ := r0.Get("id").Str(); got != "r0" {
		t.Fatalf("r0.id = %q", got)
	}
	if got, _ := r0.Get("model").Str(); got != "deepseek/chat" {
		t.Fatalf("r0.model = %q", got)
	}
	if got, ok := r0.Get("cost_per_mtok").Float(); !ok || got != 1.0 {
		t.Fatalf("r0.cost = %v %v", got, ok)
	}
	// 行内流式映射。
	th := r0.Get("thinking")
	if th == nil || th.Kind != KindMap {
		t.Fatalf("r0.thinking: %+v", th)
	}
	if got, _ := th.Get("level").Str(); got != "off" {
		t.Fatalf("r0.thinking.level = %q", got)
	}
	// 块式嵌套映射。
	th1 := ladder.Items[1].Get("thinking")
	if got, _ := th1.Get("level").Str(); got != "high" {
		t.Fatalf("r1.thinking.level = %q", got)
	}
	if got, _ := root.Get("start").Str(); got != "r0" {
		t.Fatalf("start = %q", got)
	}
	// 含 # 的引号字符串不被当注释。
	if got, _ := r0.Get("description").Str(); got != "快速、便宜，适合探索与编排" {
		t.Fatalf("r0.description = %q", got)
	}
}

func TestParseScalarForms(t *testing.T) {
	root, err := Parse([]byte("a: bare\nb: \"quoted\"\nc: 'single'\nd: 42\ne: -3.5\nf: # empty\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, _ := root.Get("a").Str(); got != "bare" {
		t.Fatalf("a = %q", got)
	}
	if got, _ := root.Get("b").Str(); got != "quoted" {
		t.Fatalf("b = %q", got)
	}
	if got, _ := root.Get("c").Str(); got != "single" {
		t.Fatalf("c = %q", got)
	}
	if got, ok := root.Get("d").Int(); !ok || got != 42 {
		t.Fatalf("d = %d %v", got, ok)
	}
	if got, ok := root.Get("e").Float(); !ok || got != -3.5 {
		t.Fatalf("e = %v %v", got, ok)
	}
	if v := root.Get("f"); v == nil || v.Kind != KindScalar || v.Scalar != "" {
		t.Fatalf("f = %+v", v)
	}
}

func TestParseListScalars(t *testing.T) {
	root, err := Parse([]byte("paths:\n  - \"src/**\"\n  - tests/**\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	items := root.Get("paths").List()
	if len(items) != 2 || items[0].Scalar != "src/**" || items[1].Scalar != "tests/**" {
		t.Fatalf("paths = %+v", items)
	}
}

func TestParseRejections(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		expect string
	}{
		{"tab indent", "a:\n\tb: 1\n", "tab"},
		{"flow list", "a: [1, 2]\n", "flow sequences"},
		{"block scalar", "a: |\n  text\n", "block scalars"},
		{"anchor", "a: &x 1\n", "anchors"},
		{"multi doc", "---\na: 1\n", "multi-document"},
		{"unclosed quote", "a: \"oops\n", "unclosed double quote"},
		{"unclosed flow", "a: {b: 1\n", "unclosed flow"},
		{"bad entry", "a: {b}\n", "not 'key: value'"},
		{"deeper surprise", "a: 1\n    b: 2\n", "deeper indentation"},
		{"list inside map at same indent", "a: 1\n- item\n", "unexpected content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("err = %v, want contains %q", err, tc.expect)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	n, err := Parse([]byte(""))
	if n != nil || err != nil {
		t.Fatalf("empty: %+v %v", n, err)
	}
	n, err = Parse([]byte("# only a comment\n\n"))
	if n != nil || err != nil {
		t.Fatalf("comment-only: %+v %v", n, err)
	}
}
