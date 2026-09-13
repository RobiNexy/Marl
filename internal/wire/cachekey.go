package wire

import (
	"strings"

	"github.com/RobiNexy/Marl/internal/types"
)

// 缓存键的三个维度，来自设计文档的三处表述（必须放在一起看，否则极易漏一维）：
//
//	Part 7.7 / 10.8：缓存键 = 前缀内容 + model_id + cache_bucket
//	Part 10.8      ：CachePrefix = model_id + endpoint（模型级键）
//	Patch 1 / 10.15：CacheBucket = AgentID（请求级缓存隔离键）
//
// 也就是说**接入点也是维度之一**：同一个模型挂在两个 endpoint（直连 / 中转）
// 上时，它们的缓存互不相通（不同厂商侧账号/不同机器），把它们算成同一个键
// 会让"命中率统计"与"升级决策"都建立在错误的假设上。
//
// 本文件提供这个键的唯一计算方式。为什么值得一个独立文件：
//   - 它是**唯一**一处把"内容之外的缓存身份"说清楚的地方；散落在 Router、
//     探测报告、审计日志里各拼一次字符串，任何一处少一个维度都不会报错，
//     只会让命中率莫名其妙地低（而这是本框架的经济命脉）；
//   - 阶段 1 的探测报告要用它把"哪两次请求应该共享缓存"讲清楚（13.3 的
//     TestCachePrefix 与用例 6 都依赖同一个定义）。

// ModelCachePrefix 返回模型级缓存键前缀（= 模型 id + 接入点名）。
//
// 用途：types.Binding.CachePrefix 必须由本函数派生（Binding 的不变量：
// "CachePrefix 与 (Model, Endpoint) 一致——它是纯派生值，不允许手工拼装"）。
// Router 在阶段 4 绑定时调用它，因此这里先把格式钉死：手工拼装出来的
// "deepseek-flash@deepseek-main" 与 "deepseek-main/deepseek-flash" 是两个键，
// 而它们指向同一个模型——缓存分片就这么无声无息地多了一份。
//
// 格式：<model>@<endpoint>。分隔符选 '@' 是因为模型 id 用 '/'（provider/name）、
// 接入点名用 '-'（deepseek-main），两者都不会出现 '@'；因此本格式无歧义。
//
// 前置条件：modelID / endpoint 非空（由 Binding 的不变量保证）。
// 本函数不做校验：它位于热路径上，且空值只会来自调用方 bug——
// 那种 bug 应该由 Router 的断言（Binding 完整性）在更早的位置抓到。
//
// 并发：纯函数。
func ModelCachePrefix(modelID, endpoint string) string {
	var b strings.Builder
	b.Grow(len(modelID) + len(endpoint) + 1)
	b.WriteString(modelID)
	b.WriteByte('@')
	b.WriteString(endpoint)
	return b.String()
}

// CacheKey 返回一次调用的**调度侧缓存键**（前缀内容之外的全部维度）。
//
// 完整缓存键 = 前缀内容（编译产物字节）+ CacheKey 的返回值。这里不含前缀内容，
// 因为那是编译层的事（wire.Compiler 的产出），而本函数的消费者是调度侧：
// 审计、命中率统计、探测报告、Router 的亲和性打分。
//
// 注意：**这不是发给厂商的字段**。发给厂商的是请求的 user_id（= CacheBucket，
// 见 10.15）；厂商内部的缓存键由前缀内容决定，我们无法（也不需要）构造它。
// 本函数算出来的是"从我们这一侧看，两次请求应不应该共享缓存"的判定键——
// 它是观测工具，不是协议字段。
//
// 格式：<ModelCachePrefix>#<bucket>。'#' 同样不会出现在模型 id / 接入点名里，
// AgentID 是 ULID（Crockford base32，字母数字），也不含 '#'。
//
// 并发：纯函数。
func CacheKey(modelID, endpoint string, bucket types.AgentID) string {
	var b strings.Builder
	prefix := ModelCachePrefix(modelID, endpoint)
	b.Grow(len(prefix) + len(bucket) + 1)
	b.WriteString(prefix)
	b.WriteByte('#')
	b.WriteString(string(bucket))
	return b.String()
}
