package agent

// golden 测试（13.12"打磨"条目）：意图 schema 表与私有段/常驻段的字节
// 稳定面（其中改动的成本 = 全项目缓存前缀失效——变化必须过评估）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"marl/internal/types"
)

// goldenIntentSchemasJSON 是 intentSchemas() 当前产出的冻结形态
// 改动（工具名/schema 字段/顺序）会破全项目缓存前缀——评估后再动这里。
// [阶段 9 修正: proto 的冻结表里意图顺序是 spawn_subagent → report → human
// → reconfigure → branch → discussion → batch（intentSchemas 的顺序即此）；
// 报表的顺序以 proto 的 intentToolList 为准——金面按**当前实现**记录]。
const goldenIntentSchemasJSON = `[{"name":"spawn_subagent"},{"name":"report_to_parent"},{"name":"request_human"},{"name":"request_reconfigure"},{"name":"request_branch"},{"name":"request_discussion"},{"name":"spawn_batch"},{"name":"llm_call"}]`

// TestGoldenIntentSchemas：工具表的**顺序与名字**是缓存前缀的组成（字段
// 层面的 schema 内容由 schema 常量的单点来源保障；这里守的是排列序，
// 它同样在前缀字节里）。
func TestGoldenIntentSchemas(t *testing.T) {
	schemas := intentSchemas()
	got := []string{}
	for _, s := range schemas {
		b, _ := json.Marshal(map[string]string{"name": s.Name})
		got = append(got, string(b))
	}
	want := strings.Join(structsOf(goldenIntentSchemasJSON), ",")
	if strings.Join(got, ",") != want {
		t.Fatalf("intent schema order/name drifted from golden:\n got=%v\nwant=%v（改动需评估缓存前缀失效）", got, want)
	}
}

// TestIntentSchemasValidJSON：每个意图工具的 Parameters 必须是**合法 JSON**。
//
// 回归面：schemaSpawnBatch 曾缺一个右括号（截断的非法 JSON），真跑在
// wire encode 即失败，而 dry-run（假 LLM 不编码工具表）与上面的 golden
// （只锁顺序与名字）都探不到——合法性是 golden 覆盖之外独立的一根金针。
// [阶段 11 修正实测记录：fossil 2.26 / Go 1.27 的真机复现]。
func TestIntentSchemasValidJSON(t *testing.T) {
	for _, tool := range intentSchemas() {
		if !json.Valid(tool.Parameters) {
			t.Errorf("tool %q: Parameters 不是合法 JSON（截断或括号失衡的 schema 会让真跑在编码期崩溃）", tool.Name)
		}
	}
}

func structsOf(jsonText string) []string {
	var wrap []map[string]string
	if err := json.Unmarshal([]byte(jsonText), &wrap); err != nil {
		return nil
	}
	out := []string{}
	for _, m := range wrap {
		b, _ := json.Marshal(m)
		out = append(out, string(b))
	}
	return out
}

// TestGoldenFrozenPrefix：同一 полная配置的两次编译 → frozen 三段
// （system/standing/私有）逐字节相同（缓存前缀 byte-stable 的 agent 侧
// golden 锚——系统的字节来源在 knowledge/profile 各自的 golden 已收口）。
func TestGoldenFrozenPrefix(t *testing.T) {
	llm := &fakeLLM{}
	a, _ := newTestAgent(t, llm)
	a.standingOrders = "<standing_orders>stable text</standing_orders>"
	req1, err := a.compileView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req2, err := a.compileView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if req1.Segments[i].Content != req2.Segments[i].Content ||
			req1.Segments[i].Stability != types.StabilityFrozen {
			t.Fatalf("frozen segment %d drifted: %q vs %q", i, req1.Segments[i].Content, req2.Segments[i].Content)
		}
	}
}
