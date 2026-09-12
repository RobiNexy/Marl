package main

// 本文件测的是**纯函数**：探测工具的全部结论都建立在这几条不变量上，而它们不需要
// 网络与密钥，因此可以进 `go test ./...`（真跑的用例 1~10 不能——它们要花钱、要网络、
// 且产出是给人看的报告，见 main.go 的文件头）。
//
// 为什么值得单独一个文件：这些不变量一旦被破坏，探测报告会**继续输出数字**，
// 只是数字不再可归因（例如"四个变体只差 reasoning_content"不成立，token 差就没有意义）。
// 报告里最重要的结论（§3.6 的回传成本、§3.11 的 id 碰撞）都依赖它们。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGCDInt(t *testing.T) {
	cases := []struct {
		name string
		a, b int
		want int
	}{
		{"相等", 128, 128, 128},
		{"倍数", 128, 256, 128},
		{"互质", 7, 13, 1},
		{"含零", 0, 5, 5},
		{"全零", 0, 0, 0},
		{"实测跳幅", 128, 256, 128},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gcdInt(tc.a, tc.b); got != tc.want {
				t.Fatalf("gcdInt(%d, %d) = %d，期望 %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// body 构造一份最小请求体，供"只差某处"的比较测试使用。
func body(t *testing.T, messages []rawChatMessage, model string) []byte {
	t.Helper()
	b, err := marshalRawBody(rawChatBody{Model: model, Messages: messages, MaxTokens: 16})
	if err != nil {
		t.Fatalf("构造请求体失败: %v", err)
	}
	return b
}

func TestSameBytesExceptMessageFields(t *testing.T) {
	system := "系统提示（固定）"
	plain := []rawChatMessage{
		{Role: "system", Content: rawStrPtr(system)},
		{Role: "user", Content: rawStrPtr("问题")},
		{Role: "assistant", Content: rawStrPtr("回答"), ReasoningContent: rawStrPtr("思维链")},
	}
	withoutReasoning := []rawChatMessage{
		{Role: "system", Content: rawStrPtr(system)},
		{Role: "user", Content: rawStrPtr("问题")},
		{Role: "assistant", Content: rawStrPtr("回答")},
	}
	otherContent := []rawChatMessage{
		{Role: "system", Content: rawStrPtr(system)},
		{Role: "user", Content: rawStrPtr("换了个问题")},
		{Role: "assistant", Content: rawStrPtr("回答"), ReasoningContent: rawStrPtr("思维链")},
	}

	t.Run("只差 reasoning_content 时相等", func(t *testing.T) {
		if !sameBytesExceptMessageFields(body(t, plain, "m"), body(t, withoutReasoning, "m"), "reasoning_content") {
			t.Fatal("去掉 reasoning_content 后应当相等（这是用例 9 的前提校验）")
		}
	})
	t.Run("别处有差异时不相等", func(t *testing.T) {
		if sameBytesExceptMessageFields(body(t, plain, "m"), body(t, otherContent, "m"), "reasoning_content") {
			t.Fatal("user 内容不同却判成相等：前提校验会放行一个不可归因的对比")
		}
	})
	t.Run("顶层差异不会被误判成相等", func(t *testing.T) {
		// 这条正是初版 bug 的形态：用例 10 的两个变体差异在**消息内部**的
		// tool_calls.id 上，而 sameBytesExcept 只摘顶层键 → 会误判成"还有别的差异"。
		if sameBytesExceptMessageFields(body(t, plain, "m"), body(t, plain, "other-model"), "reasoning_content") {
			t.Fatal("model 不同却判成相等：本函数只应忽略消息内部字段，顶层差异必须保留")
		}
	})
}

func TestSameBytesExceptTopLevel(t *testing.T) {
	msgs := []rawChatMessage{{Role: "user", Content: rawStrPtr("问题")}}
	t.Run("只差 model", func(t *testing.T) {
		if !sameBytesExcept(body(t, msgs, "deepseek-flash"), body(t, msgs, "deepseek-v4-pro"), "model") {
			t.Fatal("去掉 model 后应当相等（这是用例 8 的前提校验）")
		}
	})
	t.Run("不摘 model 则不等", func(t *testing.T) {
		if sameBytesExcept(body(t, msgs, "deepseek-flash"), body(t, msgs, "deepseek-v4-pro")) {
			t.Fatal("未声明要忽略 model 却判成相等：比较函数不该自作主张")
		}
	})
	t.Run("空字节一律不等", func(t *testing.T) {
		if sameBytesExcept(nil, body(t, msgs, "m"), "model") {
			t.Fatal("nil 请求体应当判为不等（保守方向）")
		}
	})
}

// TestToolHistoryVariantsDifferOnlyInReasoning 是 §3.6 结论的**规格**：
// 四个回传形态必须只差 reasoning_content，否则 prompt_tokens 的差值不能归因到形态。
func TestToolHistoryVariantsDifferOnlyInReasoning(t *testing.T) {
	system := "系统提示（固定）"
	modes := []echoMode{echoAll, echoLast, echoNone, echoEmpty}
	ref := body(t, toolHistory(modes[0], system), "m")
	for _, mode := range modes[1:] {
		if !sameBytesExceptMessageFields(ref, body(t, toolHistory(mode, system), "m"), "reasoning_content") {
			t.Fatalf("形态 %q 与 %q 除 reasoning_content 外还有差异：报告 §3.6 的 token 对比不可归因",
				modes[0], mode)
		}
	}
	// 反向断言：不摘 reasoning_content 时必须**不相等**（否则"只差它"这句话没被验证过）。
	if sameBytesExceptMessageFields(ref, body(t, toolHistory(echoNone, system), "m")) {
		t.Fatal("四种形态在保留 reasoning_content 时居然相等：变体根本没生效")
	}
}

// TestDupIDVariantsDifferOnlyInToolCallID 是 §3.11 结论的规格。
func TestDupIDVariantsDifferOnlyInToolCallID(t *testing.T) {
	system := "系统提示（固定）"
	dup := body(t, dupIDHistory(true, system), "m")
	uniq := body(t, dupIDHistory(false, system), "m")
	if !sameBytesExceptMessageFields(dup, uniq, "id", "tool_call_id") {
		t.Fatal("用例 10 的两个变体除 id/tool_call_id 外还有差异：结论不可归因")
	}
	// 反向断言：不摘 id 时必须不相等（否则"两个变体只差 id"这句话没被验证过）。
	if sameBytesExceptMessageFields(dup, uniq) {
		t.Fatal("保留 id 时两个变体居然相等：重复 id 的变体根本没生效")
	}
	// 且重复变体里确实出现了同一个 id 两次（两轮 tool_call + 两条 tool 消息）。
	if n := countOccurrences(dup, "call_probe_10a"); n != 4 {
		t.Fatalf("重复变体里 call_probe_10a 出现 %d 次，期望 4 次（两轮 tool_calls + 两条 tool 消息）", n)
	}
	if n := countOccurrences(uniq, "call_probe_10a"); n != 2 {
		t.Fatalf("对照组里 call_probe_10a 出现 %d 次，期望 2 次（一轮 tool_call + 一条 tool 消息）", n)
	}
}

// countOccurrences 数子串出现次数（测试专用，输入很小）。
func countOccurrences(b []byte, sub string) int {
	return len(strings.Split(string(b), sub)) - 1
}

// TestFixedTextsAreDeterministic 守住"逐字节固定"这个前提：
// 前缀里出现时间戳/随机数会让缓存实验每次都在测不同的东西，而报告里的差值
// （+92 / +38）也会失去意义。
func TestFixedTextsAreDeterministic(t *testing.T) {
	t.Run("思维链文本", func(t *testing.T) {
		a1, a2 := contractReasoning()
		b1, b2 := contractReasoning()
		if a1 != b1 || a2 != b2 {
			t.Fatal("contractReasoning 两次调用结果不同：它必须逐字节固定")
		}
		if a1 == "" || a2 == "" {
			t.Fatal("思维链文本为空：用例 9 的变体差异会消失（四个形态变得一样）")
		}
	})
	t.Run("填充文本", func(t *testing.T) {
		f1, f2 := unitFiller(400), unitFiller(400)
		if f1 != f2 {
			t.Fatal("unitFiller 两次调用结果不同：前缀必须逐字节可复现")
		}
		if len([]rune(f1)) < 400 {
			t.Fatalf("unitFiller(400) 只产出 %d 个字符，短于请求的长度", len([]rune(f1)))
		}
		if unitFiller(0) != "" || unitFiller(-5) != "" {
			t.Fatal("非正长度应当返回空串（调用方不该拿到一段意外的填充）")
		}
	})
	t.Run("请求体序列化", func(t *testing.T) {
		msgs := []rawChatMessage{{Role: "user", Content: rawStrPtr("问题")}}
		x := body(t, msgs, "m")
		y := body(t, msgs, "m")
		if string(x) != string(y) {
			t.Fatal("同一份输入两次序列化结果不同：请求字节必须可复现")
		}
		if len(x) == 0 || !json.Valid(x) {
			t.Fatalf("序列化结果不是合法 JSON: %q", x)
		}
	})
}
