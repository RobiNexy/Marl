package wire

import (
	"strings"
	"testing"

	"marl/internal/types"
)

// TestCachePrefix 是 13.3 点名要求的测试（含 Patch 1 的缓存桶维度）。
//
// 它守的是"缓存身份"这件事：缓存键**少**一个维度不会报错，只会让命中率莫名
// 偏低（而缓存是成本命脉，13.3 的整条经济性都压在它上面）；**多**一个维度
// 则会让本该共享缓存的请求各自冷启动。
//
// 测试结构是"维度敏感性"而不是"逐字符串断言"：前者能在有人重写格式时继续
// 有效，后者只会在改分隔符时报警。格式本身另有 golden 断言（见下），因为
// 它是审计与命中率统计的对外口径，不能随手改。
func TestCachePrefix(t *testing.T) {
	const (
		model    = "deepseek/chat"
		endpoint = "deepseek-main"
		otherEp  = "deepseek-mirror"
		otherMdl = "deepseek/v4-pro"
	)
	base := ModelCachePrefix(model, endpoint)

	t.Run("模型或接入点任一不同 → 前缀不同", func(t *testing.T) {
		cases := []struct {
			name     string
			model    string
			endpoint string
		}{
			{"换模型", otherMdl, endpoint},
			{"换接入点", model, otherEp},
			{"两者都换", otherMdl, otherEp},
		}
		for _, c := range cases {
			got := ModelCachePrefix(c.model, c.endpoint)
			if got == base {
				t.Errorf("%s：ModelCachePrefix(%q, %q) = %q，与基准 %q 相同——"+
					"缓存键少了一个维度，两个不同模型/接入点会共用同一个键（命中率统计与升级决策都会建立在错误的假设上）",
					c.name, c.model, c.endpoint, got, base)
			}
		}
	})

	t.Run("同一输入 → 同一输出（纯函数）", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			if got := ModelCachePrefix(model, endpoint); got != base {
				t.Fatalf("第 %d 次调用得到 %q，期望 %q（非确定性的缓存键会让命中率变成随机数）", i, got, base)
			}
		}
	})

	t.Run("桶不同 → CacheKey 不同", func(t *testing.T) {
		a := CacheKey(model, endpoint, types.AgentID("agent-01"))
		b := CacheKey(model, endpoint, types.AgentID("agent-02"))
		if a == b {
			t.Fatalf("两个不同桶的 CacheKey 相同（%q）——跨 Agent 缓存污染正是这样发生的", a)
		}
	})

	t.Run("空桶与任意非空桶不同", func(t *testing.T) {
		empty := CacheKey(model, endpoint, types.AgentID(""))
		for _, bucket := range []string{"agent-01", "a", "0"} {
			if got := CacheKey(model, endpoint, types.AgentID(bucket)); got == empty {
				t.Errorf("空桶的键 %q 与桶 %q 的键相同——\"忘了填桶\"会静默落进某个已有桶", empty, bucket)
			}
		}
	})

	t.Run("CacheKey = 模型前缀 + # + 桶", func(t *testing.T) {
		key := CacheKey(model, endpoint, types.AgentID("agent-01"))
		if !strings.HasPrefix(key, base+"#") {
			t.Errorf("CacheKey=%q 不以 %q 开头——两级键必须可分解，否则审计里无法从一次调用反推它的模型级前缀", key, base+"#")
		}
		if !strings.HasSuffix(key, "#agent-01") {
			t.Errorf("CacheKey=%q 不以 #agent-01 结尾", key)
		}
	})

	t.Run("格式 golden（对外口径，改动需 ADR）", func(t *testing.T) {
		if base != "deepseek/chat@deepseek-main" {
			t.Errorf("ModelCachePrefix 格式变了：得到 %q，期望 %q（这是审计/命中率统计的口径）", base, "deepseek/chat@deepseek-main")
		}
		if key := CacheKey(model, endpoint, types.AgentID("agent-01")); key != "deepseek/chat@deepseek-main#agent-01" {
			t.Errorf("CacheKey 格式变了：得到 %q，期望 %q", key, "deepseek/chat@deepseek-main#agent-01")
		}
	})
}
