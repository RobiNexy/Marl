# 阶段 1 编码审阅记录（`internal/wire` + `cmd/probe`）

**审阅范围**：阶段 1 落地的全部代码——`internal/wire`（`normalizer.go` / `denormalizer.go` /
`deepseek_chat.go` / `cachekey.go` / `catalog.go` / `canonical.go` / `wire.go` / `pool.go` /
`router.go` / `compiler.go`）、`internal/types` 中被本轮改动触及的契约、`cmd/probe/*`。

**方法**：以"契约是否可被静默违反"为主线逐条读实现（这个项目的失败模式几乎全是
**静默失效**：厂商容忍、不报错、只在成本或行为上悄悄偏掉），并用真跑（探测轮 5/6）去
裁决可疑点。每条发现都给处置：**修**（本轮改）、**记**（写进 ADR / 探测报告，阶段 2 做）、
**无碍**（复核后确认正确，记下来免得下次重查）。

**结论摘要**：3 条必须处理（C1/C2/C4），其余为已记录或已修的工具缺陷；**没有发现**会让
阶段 1 的测量数据失效的实现错误。

| # | 严重度 | 发现 | 处置 |
| :-- | :-- | :-- | :-- |
| C1 | 高 | `Binding.Thinking` 全项目无人读 → 阶梯上的档位配置**完全无效**（静默） | **记**：ADR-0022 + 报告 §3.1；阶段 2 第一件事 |
| C2 | 高 | 历史 `reasoning_content` 无字段通路 → 带 tools 的多轮请求**静默丢上下文** | **记**：ADR-0023 + 报告 §3.6；阶段 2 必做 |
| C3 | 中 | `SamplingParams` 值类型 → `temperature=0` 不可表达；`unsupported_params` 声明 temperature/top_p 直接报错 | **记**：报告 §3.2（改指针需 ADR） |
| C4 | 中 | 合成 `tool_call` id 跨轮会碰撞；`assertMessages` 只在组内查重 | **记**：报告 §3.11（实测厂商容忍）；阶段 2 二选一 |
| C5 | 低 | `CacheWriteTokens` 恒为 0，未命中量只能推导 | **无碍**：注释已说明；本轮把 `types.TokenUsage` 的 `[待验证]` 收口成实测结论 |
| C6 | 低 | 探测工具的 `chatCompletionsURL` 与适配器 `endpointURL` 重复一条规则 | **无碍**：刻意的（契约探测不该共享被测代码的路径），注释已记权衡 |
| C7 | 低 | `probe.usage()` 对原始请求路径读不到 usage（工具缺陷，会让用例 9 的 token 对比失效） | **修**：回退读 `Outcome.Usage`（同一份字节的另一种解析） |
| C8 | 低 | 我自己的 `sameBytesExcept` 误用（它摘顶层键，而 `tool_calls.id` 在消息内部） | **修**：新增 `sameBytesExceptMessageFields`，并给用例 9/10 加前提校验 |

---

## C1：`Binding.Thinking` 无人读（最高优先级）

`OpenAICompatNormalizer.build()` 只读 `req.Thinking`（`normalizer.go` 第 608 行附近），
全仓库没有任何代码读 `binding.Thinking`——而设计文档 §10.6 明确"档位是**阶梯**的属性"。
阶段 1 没有编译层，所以缺口还没暴露成 bug；但阶段 2 一照现状装配请求，**阶梯上的档位
配置会完全无效**：请求照发、`reasoning_effort` 缺席、走厂商默认档位（实测默认 `high`），
账面（`WireRequest.Thinking` 为空）与预期（"我配了 `low`"）不一致，日志里什么都看不出来。

**处置**：ADR-0022 定规则（Binding 唯一真相 + Normalizer 回退 + 冲突报错 + 每请求改档位走
Rebind），报告 §3.1 记现状与测试计划。**本轮不改代码**——它是承重契约的行为变更，需要
先写测试（ADR-0022 里列了三条）再改，属于阶段 2 的第一个任务。

## C2：历史 `reasoning_content` 无通路

`Segment`、`WireMessage`、`openAIChatReqMessage` 上都没有承载历史思维链的字段，
`encodeOpenAIChatMessage` 也不写 `reasoning_content`。后果：带 `tools` + thinking 的多轮请求
发出去的是"缺失历史思维链"的形态——**实测被接受**（轮 5/6 用例 9C），所以不会报错，
只会静默少上下文。

**处置**：ADR-0023（默认全量回传 + 不带 tools 不发送）+ 报告 §3.6 的落地清单。
阶段 2 必做，且需要 golden 测试（它进 stable/frozen 前缀）。

## C3：`SamplingParams` 是值类型

编码契约是"零值即不发送"，而 `temperature=0` 是真取值（贪心解码）→ 框架发不出确定性请求；
`stripUnsupportedParams` 对 `temperature`/`top_p` 的剔除声明**直接报错**（它拒绝"置零冒充
剔除"，这个判断是对的）。后果：假设 6（采样参数不在缓存键内）**测不了**——探测工具改不了
temperature。

**处置**：报告 §3.2（改 `*float64` 需 ADR）。附带实测（官方文档）：思考模式下
`temperature` 设置不报错也不生效、`top_p` 下限 0.95——即使改成指针，"用采样参数换确定性"
这条路在思考模式下也不成立，只能靠 prompt 约束。

## C4：合成 `tool_call` id 的跨轮碰撞

`synthesizeToolCallID` 按 `(下标, name, arguments)` 的 SHA-256 前 12 位合成，注释声称
"name/arguments 进 hash 是为了区分不同轮的调用"——**这个论证不成立**：同一工具、同一参数
在不同轮被再次调用（"重试同一个动作"是常见形态）会算出**同一个 id**。而 `assertMessages`
的 `pending` 每组重置，跨组重复不会被拦。

**实测裁决**（轮 6 用例 10）：厂商**接受**跨轮重复 id（200，对照组也是 200）→ 不是硬故障，
但必须显式表态。**处置**：报告 §3.11；阶段 2 二选一——给合成 id 加轮次区分度，
或在断言层写明"刻意放过"。注意"容忍"不是"安全"：我们的工具结果配对依赖位置，
将来若出现并行工具调用或厂商开始校验唯一性，碰撞会变成 400 或错配。

---

## 复核后确认正确的部分（记下来免得下次重查）

- **tool 配对断言**（`assertMessages`）：组内重复 id、悬空 tool 结果、错配 id、
  "assistant.tool_calls 与其结果之间被别的角色打断"、结尾未配对——五类都拦；
- **`canMergeSameRole`**：角色不同 / 任一方是 tool 消息 / 任一方带 ToolCalls / 任一方带
  ToolCallID / 任一方 Content 为空——四种不合并条件都有具体后果（注释里逐条写了）；
- **合成 id 的确定性**：sha256，无时间戳无随机 → 同一响应解析两次得到同一 id，
  前缀字节稳定（缓存友好）。这是"低碰撞"之外更重要的性质；
- **usage 口径**（`mapOpenAIChatUsage`）：`prompt_cache_hit_tokens` 优先、`prompt_tokens_details.
  cached_tokens` 作别名、`prompt_tokens` 缺失时用 `hit + miss` 还原、负数逐项截断为 0；
  与适配器的 `bestEffortUsage` 调**同一个**函数 → 两条路径不可能给出不同结论；
- **`Execute` 的后置条件**：resp 非 nil（失败也带回 StatusCode/Body/Latency）、
  Body 保持未解析、非 2xx 不报 error（错误分类归 Denormalizer，ADR-0016）、
  ctx 取消原样返回 `ctx.Err()`（不包装成厂商错误）；
- **编码器的"零值即不发送"**：`Temperature`/`TopP` 用指针、`ResponseFormatNone` 不发送、
  `CacheBucket` 为空则不写桶字段——且 `EncodeOpenAIChatBody` 作为导出函数**自己复查**一遍
  硬规则（探测工具与测试可能不经过 `Assert`）；
- **缓存键的三个维度**（`ModelCachePrefix` / `CacheKey`）：与设计文档三处表述
  （§7.7 / §10.8 / §10.15）对得上，且 `Binding.CachePrefix` 只能由它派生；
- **思考参数的层级**（ADR-0020）：`reasoning_effort` 走顶层、`thinking.type` 走对象内，
  编码器与 Assert 两处都校验形态（未知类型报错而不是丢弃）——本轮四档实测都生效，
  证明这条翻译是对的。
