# Marl 架构决策记录（ADR）

本文件提炼自 `docs/design/marl_design.md`，记录**不可轻易回退**的关键决策。
每条 ADR 都直接对应设计文档里的一处"取舍点"，回退它意味着推翻一处地基。

编号规则：四位数，追加，只增不改。若要推翻某条，新增一条 supersede 它，
并在此处标注 `Superseded by ADR-XXXX`。

---

## ADR-0001：Agent 只能表达意图，不能直接改变系统结构

**状态**：Accepted（对应设计文档 原则 1）

**决策**：任何结构性变更（fork 子 Agent、切换模型、开 Fossil 分支、请求人类介入）
都不是 Agent 能直接执行的动作，而是它提交给框架的**意图**，由框架统一裁决后执行。

**落地**：`SpawnRequest` → Spawner 裁决；`ReconfigureRequest` → 发 Mailbox 给自己，
经能力校验；`BranchRequest` → Spawner 裁决；`EscalationRequest` → 沿上报链冒泡；
`DiscussionRequest` → 开 Fossil 分支并进入讨论状态。

**理由**：所有结构性变更集中在少数关口，天然可校验、可审计、可拦截、可做全局优化。

**回退代价**：一旦允许 Agent 直接改拓扑，"系统状态不一致"会立刻弥散到每个技能里。

---

## ADR-0002：不可变的真相 + 可变的投影

**状态**：Accepted（对应设计文档 原则 2）

**决策**：两个真相之源（Message Log、Fossil commit）近似不可变、追加为主；
其上是可变的投影（Context View、SQLite task_runtime）。

**落地**：`store.MessageLog` 接口**不提供 Update/Delete**；编排操作签名统一为
`(Log, View) → 新 View（可能追加 Log）`；删除 = `ViewItem.Visible=false`；
摘要 = 追加 `ProvSummaryOf` 条目 + View 换引用。

**理由**：这是"越做越乱"的根本解药——永远能从真相之源重建投影，永远可回溯。

**回退代价**：在 Log 上原地修改会让审计与重放同时失效。

---

## ADR-0003：能力约束代替提示词惩罚

**状态**：Accepted（对应设计文档 原则 3）

**决策**：约束 Agent 不靠提示词劝说，靠系统层面收窄能力边界。

**落地**：深度到顶时 `spawn_subagent` 仍在 schema 里，调用时返回
`MAX_DEPTH_REACHED`（`skill.ErrMaxDepthReached`）；越权路径在 Resolver 硬拦截；
`AllowedSkills` 在调用时校验（`skill.ErrSkillNotAllowed`）。

**理由**：系统级硬保证优于依赖 LLM 自觉。

**回退代价**：把约束写回提示词后，越权变成概率问题。

---

## ADR-0004：权限来自通道，不来自内容

**状态**：Accepted（对应设计文档 原则 4）

**决策**：任何 Agent 能写进去的字节都不许被解释为授权。审批与裁决必须通过
**不可伪造的通道**认证。

**落地**：`verdict.md` 不在任何 Agent 的 `writable_paths` 里（`types.PathHidden`）；
`Envelope.From` 由框架按代码路径填写（Agent 无法影响）；
`verdict.md` frontmatter 带当轮随机 nonce，框架只接受携带正确 nonce 的裁决；
控制面目录移出项目树（`~/.local/state/marl/<project-id>/`）→ 结构上不可达。

**理由**：授权不建立在"Agent 应该不读某个文件"之上，而建立在"那个文件根本不在
它能触及的世界里"。前者是约定，后者是结构。

**回退代价**：一旦从消息内容解析"这是批准"，伪造门槛降到一次字符串拼接。

---

## ADR-0005：工具 schema 全项目唯一，技能限制退化为调用时校验

**状态**：Accepted（对应设计文档 修正 1）

**决策**：工具 schema 由框架生成、与 Agent 状态无关、逐字节一致，属
`StabilityFrozen` 前缀。`Profile.AllowedSkills` **不参与 schema 生成**。

**落地**：`skill.Registry.Schemas()` 是唯一工具表来源；
`Profile.AllowedSkills` 空 = 全部允许，非空 = 白名单（不支持黑名单语法）；
`spawn_subagent` 在 schema 里永远存在，深度检查放在调用时。

**理由**：若 `coder` 与 `reviewer` 的 schema 不同，冻结前缀立刻分裂成两个缓存空间，
共享缓存前缀的前提被打破。

**回退代价**：每引入一个 Profile 变体就多一个缓存分片，命中率断崖式下跌。

---

## ADR-0006：Profile 只表达需求，不钉死模型

**状态**：Accepted（对应设计文档 修正 2 + Part 7）

**决策**：`Profile` 只表达能力需求（`Requirement`）与采样偏好（`Sampling`），
**不含 `model_id` / `adapter`**。绑定由 Router 从阶梯里选。

**落地**：`Profile.Sampling` 无 model 字段；`types.Binding` 承载 `RungID/RungIndex/
Endpoint/Model`；`request_reconfigure` 的语义是"我卡住了 + 原因"，升级由证据说话。

**理由**：Profile 钉死模型，阶梯就没得升；两套机制互斥。

**回退代价**：阶梯升级无从实现，`Requirement.Prefer` 变成死字段。

---

## ADR-0007：每个 Agent 独立缓存桶，不配置、不暴露选项

**状态**：Accepted（对应设计文档 Patch 1 / 10.15）；桶**字段名**与"全局公共桶"的
后续裁决见 **ADR-0021**（`user` → `user_id`，且公共桶方案被实测否决）

**决策**：`Binding.CacheBucket` 的唯一来源就是 `AgentID`；
`EndpointConfig` 不提供 `bucket_strategy`。

**落地**：请求的 `user` 字段 = `binding.CacheBucket`（字段名后被 ADR-0021 修正为
`user_id`）；`Router.Bind(req, agentID, ...)` 直接把 agentID 写进 Binding。

**理由**：省掉"缓存粒度"这个配置维度的全部心智负担——这是简单且唯一正确的策略。

**已知代价（接受）**：父子 Agent 的公共段（system + tools）重复存储 N 次。
待实测项：全局公共桶是否可行（阶段 1 探测用例 6 决定），因此设计文档 13.1
把"缓存桶 user_id"的验证放在**阶段 1**——否则后面测到的命中率都是错误桶口径下的。

---

## ADR-0008：混合工具调用用 WireTurn（多 Outcome）承载

**状态**：Accepted（对应设计文档 Patch 1 / 10.16）

**决策**：一次 `Wire.Execute` 返回 `WireTurn{Outcomes []Outcome}`，而不是单个 Outcome。

**落地**：`wire.WireTurn`；同一 turn 内所有 Reasoning 共享一个 LogEntry 序列
（`StabilityStable`）；同一 turn 内 tool 调用**顺序执行**（不并行）；
中断恢复只带差量（未完成的 ToolCalls），用 `trace_id` 跟踪。

**理由**：混合流形态（单次响应混杂 reasoning + tool_call + reasoning）在旧假设下装不下。

**回退代价**：o-series / Responses API 这类混合流模型无法正确落 Log。

---

## ADR-0009：命名空间默认 hidden，宽读窄写

**状态**：Accepted（对应设计文档 Part 5）

**决策**：命名空间 = 一组 `Mount`；**没有任何 Mount 覆盖到的路径 → hidden**。
模式三档：`write` / `read` / `hidden`。hidden 的错误码统一返回 `ENOENT`（掩盖 EACCES）。

**落地**：`types.Namespace` + `types.Mount` + `types.PathMode`；
`types.Resolver` 是唯一收口点，所有路径类技能都必须过它；
`Namespace.Subset` 保证子 ⊆ 父（权限单调递减，不可能通过 fork 提权）。

**理由**：黑名单永远不知道漏了什么；默认 hidden 让新加的框架内部目录**自动**不可见。

**回退代价**：改回黑名单后，每加一个内部目录都要记得补规则，迟早漏一个。

---

## ADR-0010：单写者提交，子 Agent 不碰 VCS

**状态**：Accepted（对应设计文档 8.4）

**决策**：子 Agent 只写文件；提交由阻塞恢复后的父 Agent 做，一次 commit。

**落地**：`proto.Spawner` 与 `ChildReport` 流程；Fossil 层的写入只出现在父恢复后；
不需要 per-Agent branch，分支只用于讨论与显式实验。

**理由**：VCS 无并发（不需要写互斥/lockfile 重试）；commit 粒度天然对齐语义；
父有机会在 commit 前拦截并丢弃某个子的产出（回滚发生在 commit 之前）。

**回退代价**：多写者提交引入并发冲突处理，且 timeline 会被子 Agent 的碎 commit 污染。

---

## ADR-0011：子 Agent 的 report 做机械检查，不采信自述

**状态**：Accepted（对应设计文档 9.6 / 12.6）

**决策**：`report` 正文自由文本（不解析）；但框架对 `status` 做机械检查并有权降级。

**落地**：`proto.ReportChecker`：
- 声称 `success` 但 writable 路径下新增 `TODO(agent)` → 降级 `partial`
- 声称 `success` 但工作区无改动 → 降级 `failed`
- report 提到的路径不存在 → 记审计告警，不降级

**理由**：防止"用写 TODO 代替干活"让分治退化成一棵 TODO 树。这是纯 grep 级检查，
不依赖 LLM 配合。

**回退代价**：父会被"假 success"误导，把没干完的活当已完成合并。

---

## ADR-0012：知识就是仓库里的文件，不用 Fossil wiki

**状态**：Accepted（对应设计文档 12.1）

**决策**：`.marl/knowledge/` 下的普通 Markdown 文件承担知识库职责；
`preferences/` 编译成常驻块（硬上限 1000 est-token，超限硬失败、绝不自动截断）。

**落地**：常驻块编译；`preferences/` 在所有 Agent 的 namespace 里是 `read`、
永不 `write`（只有人类能写）；编译必须逐字节稳定（按名字排序、无时间戳）。

**理由**：wiki 的七个硬伤（平命名空间、无结构化元数据、整页读写、并发 fork、
弱搜索、与 file_* 功能重叠）在 Agent 场景全是致命项；仓库文件复用已有全部机制，
新增技能面为 0。

**附带认知**：wiki 与 ticket 都砍掉后，"为什么是 Fossil 而不是 git"的论证变弱了——
剩下价值是单文件仓库、内置 Web UI/timeline、clone/sync 顺手、零外部依赖。
它的定位从"一体化协作基底"变成"一个自带 UI 的轻量 VCS"。

---

## ADR-0013：阶梯取代单模型绑定，升级由证据触发

**状态**：Accepted（对应设计文档 Part 7）

**决策**：项目配置一个有序模型列表（从便宜到贵），框架按**可观测证据**自动升级，
而不是 LLM 主动换模型。任务不自动降级（新任务从 `ladder_start` 起跳）。

**落地**：`types.Rung` / `types.Ladder` / `types.UpgradeEvidence` /
`types.UpgradeDecision`；`Router.Bind` 带亲和性加分（同模型复用缓存）；
升级是换阶梯级别（保留各级缓存），不是 reconfigure 换 model_id（破缓存）。

**理由**：升级决策依赖 LLM 自述不可靠（LLM 很少承认搞不定）；证据是可观测的。

**回退代价**：回到"每个 Agent 绑一个模型"，缓存前缀被换模型反复击穿。

---

## ADR-0014：枚举零值按错误后果三分类

**状态**：Accepted（对应设计文档 Part 3.3 / 10.4 / 10.12 / 6.9 与全部枚举类型）

**决策**：Go 的 string/int 枚举零值可能落在"未定义"区间，而零值恰恰最容易被
"忘记赋值"产生。本框架**不采用一刀切**（既不做"零值一律非法"，也不做"零值
一律有默认含义"），而是按**错误后果**把每个枚举分到三类之一：

1. **fail-closed 降级**：零值会导致静默污染或错误决策，且错误不可见 →
   提供 `Valid()`，并在消费契约里写死"未识别值按最保守方向降级"。
   例：`types.PathMode` → hidden、`types.Audience` → both、
   `types.Stability` → stable（**绝不 frozen**）、`wire.CacheMode` → none
   （**绝不 implicit_prefix**：误判会让显式断点协议的缓存永不命中，
   只表现为账单翻倍）、`wire.HealthStatus` 零值 = "尚未探测"（既非健康也非故障）。
2. **拒绝**：零值非法且错误会立刻暴露 → 同样提供 `Valid()`，但消费侧规则是
   "拒绝/报错"，不赋予降级语义。例：`types.WireRole`、`types.InternalRole`、
   `types.Provenance`、`proto.SpawnStatus`、`proto.VerdictKeyword`、
   `wire.SegmentKind`、`wire.Speaker`、`wire.DegradationKind`、
   `wire.ThinkingControl`。
3. **指针表达"未设置"**：零值本身是合法取值且与"未设置"不可区分 →
   改成指针。例：`types.ThinkingSpec.Budget`、`types.LogEntry.TokenActual`、
   `wire.Outcome.Usage`、`proto.ReconfigureRequest.Sampling/Task/Thinking`。

另有两条**方向性与例外**规则，必须记住，否则会套错模板：

- **危险阈值类**字段（非枚举）零值普遍是"最激进"而非最保守：
  `CompressionPolicy.HeadroomThreshold/KeepTailTurns/MinReclaimFraction`、
  `wire.CircuitPolicy.ErrorThreshold`、`wire.UpgradeSignal.Threshold` 的零值
  分别导致"压缩几乎不触发 / 抹掉最近若干轮 / 任何压缩都算达标 /
  熔断一开就短路 / 任何证据都立即升级"。这类结构**不提供**
  `DefaultXxx()` 兜底，改为显式 `Validate()`，由配置层物化设计文档给出的
  默认值（如压缩尾部保留 3 轮、重试 1 次）。
- **唯一的例外**：`wire.ErrorClass` 的零值**就是** `ErrNone`（"没有错误"
  需要一个正常值来表达）。因此它额外提供 `IsError()`，并要求消费侧
  不得把"未分类"兜底成 transient（那会把永久性错误重试到烧完预算）。

**落地**：`internal/types/doc.go` 的"枚举的零值策略"一节是本 ADR 的入口；
各枚举的 `Valid()` / `Validate()` 与其消费契约分散在各自类型的注释里；
`store.ErrInvalid` 承担"调用方 bug"的表达（见 store/doc.go 错误语义）。

**理由**：零值的危害分两类——fail-open（静默放行/静默降级到更弱保证）与
**静默失效**（配置写了但不生效、能力声明了但被忽略）。这两类都不会报错，
只会在账单、缓存命中率、输出质量这些"没有断言的地方"体现。按后果分类能让
每个枚举的处置有据可依，而不是靠实现者的直觉。

**回退代价**：回到"零值无所谓"，则 `Stability` 未识别值可能被当作 frozen
（跨 Agent 缓存污染）、`CacheMode` 未识别值可能被当作隐式前缀（成本翻倍
且无告警）、阈值类字段零值会静默走最激进路径。这些都属于"上线很久之后
才从账单上发现"的问题，回退等于放弃对它们的防线。

---

## ADR-0015：工具表（含意图工具）全项目唯一、顺序冻结，由 golden 测试守护

**状态**：Accepted（对应设计文档 Part 4.1 / 修正 1 与 ADR-0005）

**决策**：技能与意图工具在**同一张**工具表里呈现，且该表是**全项目唯一、
逐字节冻结**的：所有 Agent 共享同一份 schema，任何 Profile 都不允许增删工具；
列表顺序也是协议的一部分（冻结前缀的一部分），不得因排序、去重或并发写而改变。

**落地**：

- 工具名是常量（`skill` 包的技能名常量、`proto` 包的 `ToolSpawnSubagent` 等
  意图工具常量），名字即协议，**不得改值**（历史 Log 存的是名字）；
- 名字列表用**非导出数组**持有（`skillNameList` / `intentToolList`），
  导出函数返回**副本**（`SkillNames` / `IntentToolNames`），需要遍历时用
  只读函数（`IsIntentTool`）——共享可变切片会被调用方的排序静默改序；
- 数量用常量 + 编译期断言钉死（`SkillCount` / `IntentToolCount`），
  少一个/多一个在编译期就炸；
- 冻结前缀（system + 常驻块 + Tools）的编译产物必须有 **golden test** 守着
  （见变更流程第 3 条）：改描述里的一个错别字都会让全项目缓存前缀失效，
  这类改动必须在测试里显式可见，而不是靠评审发现。

**理由**：工具表进缓存键的冻结前缀。顺序或字节一变，所有 Agent 的缓存同时
失效；而"某次重构顺手把 map 遍历改成 range 切片"这种改动不会有任何报错，
只表现为成本上升。同时 `ToolDef.Parameters` 存 `json.RawMessage` 而非结构体，
正是为了逐字节冻结——解析再序列化会改变键序与空白。

**回退代价**：允许按 Agent/Profile 定制工具表，或允许工具表随代码演进自由
重排，则"换厂商/加工具"不再是加一个适配器的事，并且缓存命中率会随每次
无关重构波动；这类波动无法归因，只能一律按"缓存本来就不可靠"来对待，
等于放弃成本可预测性。

---

## ADR-0016：一次调用三段硬分层，错误分类只归 Denormalizer

**状态**：Accepted（对应设计文档 Part 10.9 / 10.11 / 10.12 与 13.3）

**决策**：一次调用的职责分三段，且**不重叠**：

1. **Normalizer**：语义（CanonicalRequest）→ 协议（WireRequest）。做角色布局、参数剔除、
   能力降级记录；不做 IO。
2. **Adapter.Execute**：协议编码 + 发 HTTP + 读**原始字节**。不做错误分类、不做参数剔除、
   不做角色布局。
3. **Denormalizer**：原始字节 → WireTurn + `ErrorClass`。它是"同一语义在各厂商的不同
   编码"那张表的唯一持有者。

由此推出两条接口契约：

- **厂商错误不进 `error` 返回值**：只要收到了 HTTP 响应（任何状态码），`Execute` 返回
  `(*WireResponse, nil)`；错误详情留在 `resp.Body` + `resp.StatusCode` 里。`err` 只表达
  三类：装配错误（线路/接入点/模型错配、编码自检失败）、未收到响应、响应体超限。
- **重试与熔断的判据只有一个来源：`ErrorClass`**。任何"先看 error 再决定要不要重试"的
  代码都是第二套判据。

**落地**：

- `WireResponse.StatusCode == 0` 是**已定义取值**：0 = 没收到响应（连接失败/超时/取消），
  与"收到 4xx/5xx"是两回事。零值契约写在类型上，不是实现细节。
- `Execute` 的三类传输失败必须可区分：caller ctx 结束 → 原样 `ctx.Err()`（上游停机不是
  厂商故障，包装它会让熔断计数虚高）；`SamplingParams.TimeoutMs` 超时 → `ErrCallTimeout`
  （`%w` 链上 `ctx.Err()`，让按 `errors.Is(err, context.DeadlineExceeded)` 判断的既有代码
  继续有效）；其它 → 普通错误。
- `Denormalize` 的顺序是"状态码 → 可解析性 → 分类 → 拆解"：`StatusCode == 0` 先于空 body
  检查，否则 `classifyOpenAIChatError` 的 `status==0` 分支是死代码；非 2xx 但 body 为空或
  非法 JSON 时仍**仅凭状态码**分类（`statusOnlyClass`），因为网关 502/503 常常不带体。
- 适配器**不设**全局 `http.Client.Timeout`：单次超时来自 `TimeoutMs`（每档位可不同），
  全局值会把两类超时混成一个，并让"某档位用长超时"变成不可表达的配置。
- 响应体超限时**丢弃** body 只留状态码：半截 JSON 在 Denormalizer 眼里是"无法解析"，
  会把一个明确的"响应体过大"错误归成解析错误。

**理由**：错误分类必须发生在能同时看到"厂商编码细节"与"统一语义"的那一层。放在适配器
里，body 要么被丢掉（调用方再也拿不到厂商原文），要么被当字符串解析（退化成关键词匹配，
厂商改一个字就失效）。两套判据（`err` 与 `ErrorClass`）则会让"该不该重试"取决于调用方
恰好读了哪个字段——同一件事两个答案，是排查成本最高的那类 bug。

**回退代价**：回到"非 2xx 就在适配器报错"，则 429（可重试）与 400（不可重试）在调用方
眼里长得一样，只能靠字符串猜；错误分类表会分散到每个调用点，换厂商时要改的地方从一处
变成一片。

---

## ADR-0017：语义与协议分离——`CanonicalRequest.OutputJSON` 而非 `response_format`

**状态**：Accepted（对应设计文档 Part 10.10 / 10.16 与 TaskPolicy 的输出格式需求）

**决策**：`CanonicalRequest` 只表达**语义需求**（"要求输出合法 JSON"），协议形态由
Normalizer 决定：

- `CanonicalRequest.OutputJSON bool`：语义层，厂商无关。零值 `false` = 不要求（默认文本），
  方向安全，不是"未设置"；
- `WireRequest.ResponseFormat OutputFormat`：协议层，取值 `""`（不发送）与
  `json_object`。**`"text"` 不是合法值**——它就是零值的语义，允许显式 `"text"` 会让同一
  语义有两种编码方式，字节稳定与 golden 测试都变得含糊。

**落地**：`OutputFormat.Valid()` 只认 `OutputFormatNone` / `OutputFormatJSONObject`；
`normalizeOutputFormat` 在模型缺 `CapJSONMode` 时剔除字段并记 `DegradParamStripped`
（照发是 400 = capability 错误；不发是"输出不保证是合法 JSON"，上层要么重试要么自己容错
——两种处置的差别必须让上层看见）。

**理由**：OpenAI 兼容线路把它翻译成 `response_format={"type":"json_object"}`；把
`response_format` 放进 `CanonicalRequest` 等于把一条线路的字段名写进承重结构，
"换厂商 = 加一个适配器"立刻失效。

**回退代价**：语义需求与厂商字段再次混在同一个结构里，则每个消费点都要知道"这条线路上
要 JSON 该怎么表达"，新增线路时要回头改 `CanonicalRequest` 及其全部构造者。

---


## ADR-0018：Normalizer 只实现它能看见的字段，看不见的策略归编译层（不造占位字段）

**状态**：Accepted（对应设计文档 Part 10.9 与 10.10）

**决策**：`NormalizePolicy` 的字段分三类处置，且**不为了"字段表看起来完整"而添加
填不进也读不出的字段**：

1. **Normalizer 能看见的 → 在此实现**：`MultiSystem`、`ConsecutiveSame`、`TailAssistant`、
   `ToolResultRole` 四个具名策略（Part 10.9 的决策点）。它们的零值 `""` 的语义是
   **"不覆盖，用目标线路的内置默认策略"**，而不是某个具体策略名——策略名写错（配置笔误）
   在 `Validate` 期直接报错，而空 `NormalizePolicy{}` 仍然可用
   （`LoadPolicyOverride` 要求"空策略不得凭空改变请求布局"）。
2. **Normalizer 看不见但确实要用的 → 留给消费它的那一层，并在字段上写明错位**：
   `Roles` / `Trimmable` 的键是 `types.InternalRole`，而 Normalizer 的输入 `Segment` 只有
   `(Kind, Speaker)`——`InternalRole` 在编译层就被折叠掉了（`Segment` 不变量表里没有它）。
   因此这两个字段由**编译层**消费；Normalizer 用的是 `(Kind, Speaker) → WireRole` 的默认表。
3. **本层没有落点且无人消费的 → 不建字段**：`HistoricalThink`（Part 10.9 的第五个决策点）
   故意没有对应字段。它的取值作用于"历史思维链段"，而 `Segment` 无法表达"这是一条思维链"
   （没有 `InternalRole`，也没有 Thinking 标记）；在 Normalizer 层实现只能靠猜，因此剥离
   历史思维链的责任归编译层（它读得到 `InternalRole`）。

同一原则也约束 `WireRequest.Prefill`：阶段 1 的 OpenAI 兼容线路**未实现 prefill**，
`WireRequest.Prefill` 恒为 `nil`（唯一合法值），且每次都在 `NormalizeResult.Degradations`
里留一条记录，理由写明"上层应把 prefill 改写为尾部 Transient 提示"（10.10）。

**理由**：字段存在但填不进/读不出，或填了但由别处消费，都属于 ADR-0014 所说的**静默失效**
（配置写了但不生效）——而"错位"比"缺失"更危险：字段名看起来完全正确，评审与运行时都没有
异常迹象，只有行为不对。把"谁消费它"写在字段注释里，是让这种错位可评审的最低成本。

**回退代价**：删掉 `Roles` / `Trimmable` 会让编译层在阶段 2 再补一次同概念字段；反过来，
允许 Normalizer "尽力实现"它看不见的策略（例如按 `Segment` 猜哪条是思维链），则策略是否
生效变成随输入而定的随机行为，且无法用测试钉死。

---

## ADR-0019：Outcome 不变量只约束成功产出；失败产出是唯一例外

**状态**：Accepted（对应设计文档 Part 10.11 / 10.16）

**决策**：`Outcome` 的不变量"Reasoning / Reply / ToolCalls 至少有一项非空"**只约束成功
产出**。厂商报错时产出的是**失败产出**：三项皆空，全部信息在 `Signals.ErrorClass` 里。

由此推出两条消费纪律：

- 判定"这一段有没有内容"必须看 `Signals.ErrorClass.IsError()`，**不能**只看三项是否为空；
- 失败产出的 `Usage` 为 `nil`（未知），不是零值结构体。

**落地**：`failureTurn(class)` 是失败产出的唯一构造点（`Denormalize` 的四条边界都汇到它，
见 ADR-0016）；`ThinkingOutcome.ActualLevel` 在 `Denormalize` 侧留空——它需要请求侧信息
（Normalizer 降级后的档位），而 `Denormalize` 手上只有响应，由调用方用
`NormalizeResult.Degradations` 回填。

**理由**：厂商报错时没有任何内容可产出。为了让不变量成立而往 `Reply` 里塞错误文本，主循环
会把厂商报错当成"模型说的话"写进 Message Log 并回灌上下文——一次瞬时故障被永久化进历史，
之后的每一轮都在向模型复述那次故障。

**回退代价**：恢复"失败也必须有内容"，则 Log 里无法区分"模型说的"与"框架补的"，审计与
重放同时失效（违反 ADR-0002 的真相之源原则）。

---


## ADR-0020：思维参数的协议形态由线路定义，用类型承载层级（`openAIChatThinking`）

**状态**：Accepted（对应设计文档 Part 10.9 / 10.10 与 13.3 的 `openai_chat` 线路）

**决策**：`WireRequest.Thinking` 的类型是 `any`，但**每条线路必须定义自己的协议形态类型**，
且该形态必须能表达"哪个键放在哪一层"。OpenAI 兼容线路的形态是 `openAIChatThinking`：

```
开关：{"thinking": {"type": "enabled" | "disabled"}}      ← 对象信封内
强度：{"reasoning_effort": "none"|"low"|"high"|"max"}     ← 顶层字段
```

DeepSeek 的思维控制分布在请求体的**两个不同层级**上，这就是不能用
`map[string]any` 承载的原因：map 没有"这个键该放顶层"的信息。`BudgetTokens`
仅 `ThinkControlBudget` 使用（DeepSeek 侧没有预算字段，只有兼容网关认这个形态）；
保留它是因为删掉一个已发出的形态属于行为变更，而阶段 1 没有证据说它有害。

**落地**：

- 映射规则（每一支都必须可观测，不允许静默）：
  - 模型未声明 `thinking` 能力：只有 `""`（不指定）与 `off` 能安全满足；要求"开"则记
    `DegradThinkingUnavailable` 并彻底移除参数；
  - `Level == ""` 且无 `Budget`：不发送任何思维参数（"不指定" ≠ "关闭"）；
  - `Level` 不在模型的 `thinking_levels` 里：记 `DegradThinkingLevel` 并回落为"不发送"
    （模型默认档位）——**不**擅自挑最接近的档位，那会改变成本与输出而调用方以为档位生效了；
  - `ThinkControlBudget` 且给了 `Level`：档位在协议里没有落点，记 `DegradThinkingLevel`
    （`on` 除外：它是框架开关词，而"要不要带预算"由 `Budget` 是否给出决定）；
  - `off` 是**框架开关词**，不查档位表：它在三种控制方式下分别落成 `disabled`（bool /
    budget）与 `none`（level）。level 控制下的关闭取值要在 `caps.ThinkingLevels` 里查
    （能否关闭是**模型属性**，不是线路常量：有的模型只有 low/high）；
- `Assert`（`assertOpenAIChatThinking`）**只校验形态、不校验取值**：取值空间由
  `models.yaml` 的 `thinking_levels` 定义，在这里枚举等于把厂商词汇写进框架；
- 未知类型**报错**、空的 `openAIChatThinking`（三字段全空）**报错**（应当直接给 `nil`，
  否则"没要求"与"要求了但翻译成空"无法区分）；编码器遇到未知类型同样**报错而不是丢弃**
  （静默丢弃 = 思维参数没发出去，而调用方以为发过了）。

**理由**：早期实现把开关与强度都塞进 `thinking` 对象（`{"thinking":{"type":"low"}}`）——
文档明说 `reasoning_effort` 既管开关又管强度，而 `thinking` 对象只认 `enabled`/`disabled`。
发错的后果是**静默失效**：档位请求了但强度还是默认，探测报告里表现为"每个档位的
`reasoning_content` 都一样长"，极易被当成"模型就是这样"。`Thinking` 的类型是 `any`，
类型安全为零，`Assert` 因此是唯一的防线。

**回退代价**：用 `map` 承载则"这个键该放顶层"的信息丢失，编码器只能猜；把厂商档位词
枚举进框架，则新增模型档位要改代码（违反 Patch 1 的"档位数由配置声明"）；
把关闭取值硬编码成 `none`，则没有关闭档位的模型会收到 400 或被静默忽略——而调用方
以为已经关了思考。

---


## ADR-0021：缓存桶字段是 `user_id`（不是 `user`），且桶隔离——"全局公共桶"方案否决

**状态**：Accepted（对应设计文档 10.15 / 13.3；阶段 1 探测的裁决）

**决策**：三条一起定：

1. 缓存桶落到 DeepSeek `openai_chat` 请求体的字段名是 **`user_id`**（`wire.DefaultBucketField`）。
   `Binding.CacheBucket` 的语义与来源不变（= AgentID，ADR-0007）。
2. 桶是**隔离**的：不同 `user_id` **不共享**前缀缓存。因此**不做**"全局公共桶热身请求"，
   接受"N 个 Agent 重复存储公共段（system + tools）"的代价。
3. 厂商 API 已更新：`remote_name` 用现行模型名（`deepseek-flash` / `deepseek-v4-pro`），
   thinking 是**档位控制**（`thinking_control: "level"`，`thinking_levels: ["none","low","high","max"]`），
   不再是 `bool` + `["off","on"]`；usage 主字段是 `prompt_cache_hit_tokens`
   （`prompt_tokens_details.cached_tokens` 是同值别名）。
4. **厂商的"静默改写"必须在框架侧可见**（同批实测，写入设计文档 §6.3 / §10.9）：
   - 思考模式下 `temperature` / `presence_penalty` / `frequency_penalty` **设置不报错也不生效**，
     `top_p` 被抬升到 ≥0.95（非思考模式恒为 1.0）→ 采样参数不能当作"我设了就生效"；
   - 历史 `reasoning_content` 的**回传规则取决于请求是否带 `tools`**（带 tools 应当回传、
     会被拼进上下文；不带 tools 传了也被忽略）→ 这是"历史思维链"策略的输入，也是成本项。

**依据**（全部可复现，见 `docs/design/probe-report-phase1.md` 与 `probe-run-record-phase1.md`）：

- 官方 `chat-complete.html`：请求参数是 `user_id`（字符集 `[a-zA-Z0-9\-_]`、≤512），
  并明确写着"`user_id` 可用于 KVCache 缓存隔离，以进行隐私管理"——**隔离是厂商承诺**；
- 探测用例 6a/6b：本桶用**本轮全新**前缀（带 nonce，工具强制校验本桶 `cached=0` 作为
  可归因前提），另一桶发逐字节相同的请求 → `cached=0`；
- 同一份代码在"另一桶是全新状态"时同样给 `cached=0`（三轮证据同向）。

**为什么必须记下来**：探测用例的**初版形态是不可归因的**——它沿用固定前缀，而缓存是持久的，
于是上一次运行留下的副本让"另一个桶"在第二次运行时命中，同一套代码在两轮里给出**相反**
结论（轮 1"不共享"、轮 2"共享"）。结论的可归因性依赖"前缀是全新的"这个前提，而该前提
必须被**显式校验**，不能靠"应该没跑过"。以后任何跨桶/跨会话的缓存探测都要沿用这个形态。

**回退代价**：继续用 `user` 则桶参数被厂商忽略或 400（缓存隔离失效 = 不同 Agent 互相
污染缓存桶，是**正确性**问题，不只是性能问题）；沿用 `bool` + `["off","on"]` 则档位永远
发不出去（`reasoning_effort` 缺席 = 走默认 `high`，账单与输出都与配置不符）；按
`prompt_tokens` 算成本则缓存省下的钱在账面上看不见（命中部分与未命中部分单价不同）。

**顺带确立的不变量**：前缀缓存**从第 1 个 token 起算、按单元对齐**——前缀第一个 token
变了就零命中（nonce 放在 system 末尾时仍命中 768，移到开头后为 0）。这是 10.3
"`frozen` 段 byte-stable 是硬要求"的实证。

---

## ADR-0022：thinking 档位的唯一真相是 `Binding.Thinking`（阶梯），Normalizer 回退读它、冲突即报错

**状态**：Accepted（对应设计文档 10.6 / 10.9；阶段 1 探测报告 §3.1 的裁决）

**决策**：四条一起定：

1. **`Binding.Thinking` 是唯一真相**。档位配在**阶梯**上（`ladder.rung.thinking` → `Binding.Thinking`），
   §10.6 的"thinking 开关在阶梯级别，不在 model 定义里"由此落地。
2. 装配/编译层**应当**把它拷进 `CanonicalRequest.Thinking`——拷贝点是唯一入口（一处的
   `req.Thinking = binding.Thinking`），不是"两处都可以写"。
3. Normalizer 侧加**回退 + 冲突检测**（这是本 ADR 的关键，因为"忘记拷贝"不能靠人记得）：
   - `req.Thinking` 为空（`Level == "" && Budget == nil`）→ 用 `binding.Thinking`；
   - 两者都非空且**不相等** → **报错**（框架错误，不发请求）。
   于是"档位配了却不生效"这类**静默失效**在结构上不可达：要么生效，要么炸。
4. 每请求改档位（压缩器要关思考、探测器要逐档位验证）必须走 **Rebind / 显式构造 Binding**，
   不允许只改 `CanonicalRequest`。这也是 `proto.Reconfigure.Thinking` 的既有语义
   （它替换的是绑定的档位）。

**依据**：

- 设计文档 §10.6 明确"档位是阶梯的属性"，而 `internal/wire` 的 `build()` 只读
  `CanonicalRequest.Thinking`，全项目**没有任何代码**读 `Binding.Thinking`（阶段 1 没有编译层，
  缺口尚未暴露成 bug）；
- 缺口的风险形态是**静默失效**：档位配在阶梯上 → 请求照发、`reasoning_effort` 缺席 →
  走厂商默认档位（实测默认是 `high`）→ 账面（`WireRequest.Thinking` 为空）与预期
  （"我配了 low"）不一致，而日志里什么都看不出来。成本与输出都变了却无人知晓。

**为什么不让 `CanonicalRequest.Thinking` 覆盖 `Binding.Thinking`**（被否决的替代）：
那会出现"请求级覆盖悄悄改变成本档位"，而审计里只有请求体、没有"谁改的"——§7.1 的
阶梯升级决策读的是阶梯，实际跑的却是另一个档位，升级证据与成本核算同时失真。

**为什么删掉 `CanonicalRequest.Thinking`**（另一条被否决的替代）：Canonical 是"协议无关的
规范形态"，"这次调用要什么档位"是它的合法内容（压缩/总结调用与主循环调用的档位本就不同），
删掉字段等于逼每个调用点自己拼 `ThinkingSpec` 再塞进 Binding，反而多出更多入口。

**落地状态（必须与决策一起记）**：**尚未实现**。阶段 1 的探测工具是临时绕过——`call()`
同时填两处（同值，因此不触发冲突），让测量不受歧义影响。实现它是阶段 2 的第一件事，
测试计划（先写测试再改代码）：

- `TestBindingThinkingAppliesWhenCanonicalEmpty`：Canonical 留空 + Binding 带 `low`
  → 请求体出现 `reasoning_effort=low`；
- `TestBindingThinkingConflictErrors`：两处都非空且不同 → `BuildRequest` 返回错误（不是降级）；
- `TestBindingThinkingOffMapsToProtocolOff`：Binding 带 `off` → 走 `levelOffValue` 的 `none`；
- golden 回归：现有用例的请求字节不得变化（Canonical 显式给了 thinking 的那些调用点，
  两处同值，回退不触发）。

---

## ADR-0023：带 `tools` 的历史 `reasoning_content` 默认**全量回传**；不带 `tools` 时**不发送**

**状态**：Accepted（对应设计文档 10.9；阶段 1 探测报告 §3.6 的裁决）

**决策**：

1. 请求**带 `tools`** 时，Message Log 里保留的历史思维链按原字段 `reasoning_content`
   **全量回传**（这是厂商文档"均应回传"的字面要求）。
2. 请求**不带 `tools`** 时**不发送**该字段（实测被忽略，见下）。
3. 裁剪（只回传最近 K 轮）**不作为默认**，只作为显式配置项；开启它的前提是
   "有重复采样的行为证据 + 质量评估"，不允许只凭 token 省量就打开。
4. Message Log **始终完整保留**思维链（审计/复盘），是否进请求体由本条策略决定——
   "保留"与"回传"是两件事。

**依据**（轮 5 与轮 6 两次独立运行，数字完全一致）：

| 形态 | 轮 5 prompt | 轮 6 prompt | 相对不回传 |
| :-- | --: | --: | --: |
| 全量回传（每条历史 assistant 都带） | 1458 | 1458 | **+92** |
| 只回传最近一轮 | 1404 | 1404 | **+38** |
| 完全不回传（不发该字段） | 1366 | 1366 | 基线 |
| 带字段但值为空串 | 1366 | 1366 | 0 |

- **四种形态全部 200 接受**：文档的"均应回传"是**期望**，不是硬校验。裁剪在协议上可行；
- 但回传的内容**确实被拼接进上下文**（差值恰等于两段固定思维链的 token 数：`+92 = rc1+rc2`、
  `+38 = rc2`）→ 省下的 token 是**真的**，不是账面幻觉；
- **不带 `tools` 时带与不带的 `prompt_tokens` 完全相等（1005 vs 1005）**：文档"传入也会被
  忽略、不会拼接进上下文"**成立** → 不带 tools 时发它是纯浪费（不进上下文，却增加请求字节，
  且未来口径可能变）；
- 回传形态对**行为**没有可观测的系统性影响：两轮里"是否再次调用工具"的分布不一致
  （轮 5 只有"只回传最近一轮"返回 `tool_calls`；轮 6 有三态返回 `tool_calls`）→
  单次样本不可归因，**不能**用"裁剪后行为没变"来论证裁剪安全。

**权衡**：裁剪每轮省 ~54 token（92−38），代价是"历史思维链不完整"这一状态进入模型上下文，
而质量影响**无法**用探测判定（需要重复采样 + 评分）。合规默认（全量回传）比省 54 token/轮
更重要——[权衡: 省 token 的收益可量化且很小，质量风险不可量化]。

**落地状态（必须与决策一起记）**：**当前没有通路**。`Segment`、`WireMessage`、
`openAIChatReqMessage` 上都没有承载历史思维链的字段，`encodeOpenAIChatMessage` 也不写
`reasoning_content`——也就是说，**今天带 tools + thinking 的多轮请求发出去的是"缺失历史
思维链"的形态**（实测被接受，因此不会报错，只会静默丢上下文）。阶段 2 的落地清单：

1. 通路：`WireMessage.Reasoning`（或 `Segment.Reasoning` + 编译层折叠），编码器按
   `HistoricalThink` 策略决定是否写 `reasoning_content`；
2. 字节稳定：它进的是 **stable/frozen 前缀**，历史思维链一旦落库就不再变 → 前缀仍稳定，
   但**任何"重写/截断思维链"的行为都会破缓存**，需要 golden 测试守；
3. 不带 tools 时不发送（本 ADR 第 2 条）；
4. 计数器：回传的 token 计入成本（`prompt_cache_miss_tokens` 会涨），成本模型要能看到它。

---

## ADR-0024：缓存单元端点间隔实测 128 token，但**不做**"前缀补齐到单元倍数"的优化

**状态**：Accepted（对应设计文档 10.3 / 10.15；阶段 1 探测报告 §3.9）

**决策**：

1. 实测记录：隐式前缀缓存的**单元端点间隔 = 128 token**（`cached_tokens` 的台阶跳幅恒为 128：
   768 → 896 → 1024，两轮独立复现）。
2. **不做**"把冻结前缀补齐/裁剪到 128 的倍数"这类优化：`cached_tokens` **不等于**
   `floor(prompt_tokens / 128) * 128`（实测残差最大 250 token），命中长度取决于厂商**实际落盘了
   哪些单元端点**，而落盘时机是厂商的启发式（`cache.html`：请求结束位置落盘 / 公共前缀检测落盘 /
   按固定 token 间隔落盘）。把前缀凑成 128 的倍数**不能**保证多命中。
3. 前缀策略仍按原设计：`frozen` 段 byte-stable（ADR-0015）、`volatile` 只在尾部、
   命中率统计用 `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`。

**依据**：探测用例 7（截断扫描）：对同一段文本先发最长的一份建立缓存，再发它的逐字符截断
（每次 +40 字符 ≈ +28 token），观测 `cached` 随长度变化的台阶——10 次采样落在 3 个台阶上，
两次跳幅都是 128，且最大残差 250（> 128）。

**为什么这条要写成 ADR 而不是留在报告里**：它是**否决一条优化路线**的记录。没有它，
后来的人看到"单元间隔 128"会自然推出"那就把前缀对齐到 128"——一个看起来显然、实测无效、
且会让冻结前缀的字节为了一个无效目标而变动的改动。

**待办（阶段 2）**：用步长 ~10 token 的细扫描定死对齐规则（当前只测到"台阶 128"，
"哪些端点会落盘"仍未解释；报告 §3.9 记着这个缺口）。

---

## ADR-0025：thinking 档位只声明"可用性"，**不**声明强度或成本

**状态**：Accepted（对应设计文档 10.4 / 10.6 / 10.9；阶段 1 探测报告 §3.8）

**决策**：

1. `thinking_levels` 只表达"该模型接受哪些档位"。框架**不**为档位声明强度序，也**不**提供
   "卡住了就降一档省钱"这类旋钮——档位是**能力**开关（够不够用），不是成本旋钮（省不省）。
2. 成本模型**不能**按档位定值：`reasoning_per_mtok` 若逐档给定值，那些数字会在两次运行之间
   自相矛盾（见"依据"）。成本估算只给量级，或按"同档位同分布"处理。
3. 任何"档位 A 比档位 B 省 X%"的说法必须同时报出**重复次数与极差**；单次采样、甚至一次
   3 次重复的均值，都不构成证据。

**依据**：探测用例 4，两轮各 3 次（同一命令、同一问题、同一前缀）：

- 轮 5：`low` 830（656~1073）< `high` 955（677~1150）< `max` 1054（693~1381）→ 单调；
- 轮 7：`low` 775（684~847）**>** `high` 725（623~869）< `max` 790（701~948）→ **不单调**。

合并 6 次：0 < 802 < 840 < 922，但相邻档位差值（38 / 82）只有同档位极差（417 / 527 / 688）
的 1/5 ~ 1/18——**档位间差异淹没在同档位波动里**。

**为什么这条要写成 ADR**：它是"**拒绝一个旋钮**"的记录。没有它，成本模型会很自然地把
`thinking_levels` 当成成本档（"卡住了就降一档省钱"），而这个前提实测不成立：降档既不能保证
省钱，也不能保证变弱。与 ADR-0024 同类——看起来显然、实测不成立的路，必须留档。

**落地**：设计文档 §10.4 的 `thinking_levels` 条目与 §10.6 的档位语义已按此收口；探测工具的
`checkLevelMeans` 在两种结果下都会提示"必须带重复次数与极差"。

**待办（阶段 2，若真要做档位成本建模）**：先做采样量实验（每档 ≥20 次）把效应/噪声比测出来；
在那之前成本模型不得引用档位。

---

## 变更流程

1. 任何对 `internal/types`、`internal/proto`、`internal/store`、`internal/wire`、
   `internal/skill`、`internal/orchestrate` 中**契约字段**的修改，必须先新增 ADR。
2. 阶段 0 的交付检查（`go build ./...` + `go vet ./...`）必须在每次契约变更后重跑。
3. **Byte-stability 是可测不变量**：冻结前缀（system + 常驻块 + Tools）的编译产物
   必须有 golden test 守着，否则某天一个"顺手清理"的改动会静默毁掉全项目缓存。


---

## ADR-0026：阶段 3（压缩与编排操作）的六项落地裁决

**状态**：Accepted（对应设计文档 13.5；2026-09-12 阶段 3 交付时记录）

**决策**：

1. **Compress 返回的 View 引用 SUM**（"暂不引用"的措辞裁决）：Part 3.7 步骤 7 的字面
   定义是"构造新 View = [保留头部] + [SUM] + [保留尾部]"，`CompressionResult.View`
   的字段注释同。"主 View 暂不引用"指的是调用方的**现行** view 对象不被触碰——
   采纳与否（`a.view = res.View`）是主 Agent 的显式决定；拒绝采纳时 SUM 条目留在
   Log 成为未被引用的审计事实。两个表述在这层含义下相容，落地按此实现。
2. **"轮"的口径**：一轮 = 主循环的一个 eventLoop 轮次，**轮起点是 assistant 的
   tool_calls 意图条目**；不带 tool_calls 的文本回复（模型在调用之间的进度旁白）
   附着于当前轮，不开启新轮。真机实测的教训：把旁白当轮起点会切出"只有一句话"
   的微型压缩区，SUM 的固定成本超过压缩区，reclaim 归零（实测 reclaim = -0.005）。
   user_input 一律归头部（任务原话不进压缩区）。
3. **"L0 已独自达标"的判据**：L0 清理后的收益 `(old−l0)/old ≥ MinReclaimFraction`
   时跳过 SUM，返回 `SUMENTry=nil` 的结果。判据只有 Compress 内一处实现
   （触发点只判 headroom，不判收益）——契约要求的"判据唯一"落点。
4. **"无可压缩区间"用哨兵表达**：`orchestrate.ErrNothingToCompress`。调用方据此
   跳过压缩继续主循环，并记录触发时的上下文规模——上次尝试后上下文没有增长就
   不重试（防热循环：否则每轮都重试压缩，把主循环变成压缩循环）。
5. **L0 的去重以（意图, 结果）对为单位**：真机缺陷回归（见
   `docs/test_report/phase3-test-report.md` §3.2）——只排除重复的 file_read 结果、
   保留其 tool_calls 意图，编译出的请求就是"没有结果的工具调用"，被 Assert 拒绝。
   修复：结果被排除时其配对意图一并排除；一个意图只有在其**全部**结果都被排除时
   才排除。归属按**最近前向意图**计算（与线路协议的分组语义一致），模型跨轮复用
   同一 tool_call id 时仍然正确。`validatePairing` 在三处产出点（L0-only / SUM /
   Admit）做与 wire.Assert 同构的终检。
6. **编排账本的落点**：阶段 3 定义 `compress.UsageSink` 窄接口（mini 用控制台出口），
   SQLite Ledger（`store.Ledger.RecordOrchestration`）在阶段 4 接管。独立预算
   （`EngineConfig.MaxTotalTokens`）在本阶段即生效：超预算的编排调用在发出前被拒。

**依据**：真机 12+ 轮交付检查（含 2 次 assert 失败的归因复现）、
`internal/compress` 与 `internal/orchestrate` 的测试矩阵、
`docs/test_report/phase3-test-report.md`。

**回退代价**：撤掉配对不变量，L0 去重在"模型重复读同一文件"的场景下必然产出
被厂商拒绝的请求；撤掉轮口径裁决，长工具循环任务的压缩区被单条 user 消息吞掉，
压缩永不触发。

---

## ADR-0027：阶段 4（阶梯与成本账本）的五项落地裁决

**状态**：Accepted（对应设计文档 13.6 / Part 7；2026-09-12 阶段 4 交付时记录）

**决策**：

1. **thinking 档位表的载体**：`types.Rung` 保持纯排序/计价数据（不加字段——
   契约字段变更需 ADR 且无收益），每档的 `ThinkingSpec` 由 `ladder.Config.Thinking`
   平行承载，Router 是唯一组装点（ADR-0022 的"唯一真相在阶梯"落在这里）。
   Load 强制每档显式声明 thinking.level——"不指定"会让厂商走默认档位
   （实测默认 high），账单与配置意图不符且无告警。
2. **成本公式**：`cost = (Prompt−CacheRead)×In + CacheRead×CachedIn +
   (Completion−Reasoning)×Out + Reasoning×Reasoning`。`Completion − Reasoning`
   的依据：DeepSeek 的 `completion_tokens` **包含**思维链（reasoning_tokens 是
   completion_tokens_details 的子字段），Denormalizer 原样保留该口径；账单要
   拆"可见输出/思维链"两个价格档必须先减再加。公式永远假设 Reasoning ⊆
   Completion，厂商口径变化由 Denormalizer 拆分吸收（Part 10.11 纪律）。
3. **用量未知的调用不记账**：TokenUsage 零值 = "用量确为零"，把未知记 0 会
   让账单把失败调用伪装成免费（types.TokenUsage 的零值契约）。不记账是已知
   低估，由"调用数与 token 数对不上"暴露——比静默记 0 诚实。
4. **TaskSummary 的 Status 恒为 running**：账本只见账目、不见任务终态（没有
   任务表）。报表的"状态"行在任务管理落地前由调用方覆盖；Duration 用账目
   时间跨度（低估真实时长，报表注明口径）。
5. **证据权重默认**：Part 7.3 示例"连续失败 2 次 → score=0.8" ⇒ 每次失败
   0.4；格式错误同级；无进展每轮 0.15；子任务失败率 ×1.0（全军覆没单独
   触发，>50% 需叠加）；压缩收益不足 +0.1。全部可配置（EvidencePolicy），
   Threshold 必须 > 0（零值 = 任何证据立刻升级，ADR-0014 危险阈值类）。
   ErrTransient/ErrContextOverflow/ErrContentFilter 不计入升级证据
   （ErrorClass 语义表的直接推论）。

**依据**：`internal/ladder`/`internal/ledger` 的测试矩阵、dry-run 升级场景
（`mini -task failing -dry-run`，两次 BAD_ARGS → 升级 → r0/r1 分项报表）、
真机跑（`mini -task files -ladder`：r0 9 次调用、编排 3 次单独记账、
缓存命中 44%/85%）。

---

## ADR-0028：阶段 5（Spawner 与单层 fork）的五项落地裁决

**状态**：Accepted（对应设计文档 13.7 / Part 8 / Part 9；2026-09-12 阶段 5 交付时记录）

**决策**：

1. **新增裁决错误码 `INVALID_INJECT_SEQ`**：SpawnRequest 契约要求"越界的
   Seq 必须被裁决拒绝而不是跳过"，但阶段 0 的错误码清单没有承载它的码。
   拒绝语义不能借用 NAMESPACE_EXCEEDED（那是权限问题，这是引用问题）。
2. **Spawner 不 import agent**：子 Agent 的构建走 `ChildFactory` 接口
   （装配层实现），Spawner 只认识 `ChildRunner`（能 Run 的东西）。依赖
   方向恒定：cmd → {agent, spawner}，spawner ⊥ agent。子 Agent 的
   ReportSink 由 Spawner 实现（`ReportToParent`），From 按代码路径填写
   （原则 4）。`SetFactory` 只允许在首次 Adjudicate 前设置一次（工厂需要
   引用 Spawner 本身，存在构造顺序依赖；运行中更换工厂不可归因）。
3. **框架代报的触发点**：子 Run 返回而未 report → Spawner 代报 failed
   （"child exited without report"）。这是阶段 5 的活性兜底（没有它，子
   忘调 report_to_parent 会让父永久阻塞）；Watchdog 的完整形态（超预算/
   超时/无进展）仍是阶段 9。
4. **PatternCovers 的保守拒绝**：命名空间子集校验对"子模式带通配符且无法
   静态证明被父模式覆盖"的形态（如 child `src/*/x` vs parent `src/a/**`）
   一律拒绝——拒绝一次合法 spawn 的代价是一次重试，放行一次越权的代价是
   权限模型失效。可证明形态：相等、`**`、`prefix/**` 前缀树、字面量子路径。
5. **report 机械检查的否定语境**：'nothing modified' / '未修改' 这类否定句
   不算"声称改了文件"——真机实测中纯阅读任务的成功 report 被动词表误降为
   failed。修复方向是宁漏不误（漏报由 TODO 扫描与 FilesChanged 兜底）。

**附带修复（跨阶段缺陷）**：`store.OpenSQLite` 的 DSN 增加
`_txlock=immediate`——Append 的"读 MAX+1 → INSERT"在 deferred 事务里从读锁
升级写锁时，WAL 快照过期会**立即**返回 SQLITE_BUSY（busy_timeout 不适用）。
阶段 5 的父子并发追加真机实测踩中；IMMEDIATE 让取锁阶段排队，busy_timeout
全程有效。附最小并发回归测试（`TestConcurrentAppend`）。

**依据**：`internal/spawner`/`internal/agent` 的测试矩阵（含 10 轮 -race
fork 稳定性）、真机跑（`fork_test`：父 fork 子 → 子只读 report success →
父汇总；含一次机械检查误降级的发现与修复）。

---

## ADR-0029：计价从 ladder.yaml 的 `pricing:` 节加载（按 model 四项分价）

**状态**：Accepted（2026-09-12，阶段 4 交付后的定价修正；对应 Part 10.4）

**决策**：

1. 计价的权威来源是 **ladder.yaml 的 `pricing:` 节**（按 model id 键控），
   四项分价：`in_per_mtok`（输入未命中）/ `cached_in_per_mtok`（输入缓存
   命中）/ `out_per_mtok`（可见输出）/ `reasoning_per_mtok`（思维链），
   全部每百万 token 单价 + currency。代码内不再有任何占位价格表。
2. `StaticCatalog.Pricing` 读 `ladder.Config.Pricing`（NewRouter 注入，
   单一来源）；`ModelEntry.Pricing` 字段保留（Part 10.4 契约形态）但不再
   被 StaticCatalog 消费。
3. `Rung.CostPerMTok` 保留为**排序粗价**（加权混合口径由配置者自定），
   校验它落在 Pricing 的可行区间 [CachedIn, In+Out] 内——越界说明两个
   价格表之一写错了（排序与记账的相对结论会相反）。
4. 同一模型在任何档位同价（档位只切 thinking，不切价格）——rung 的
   currency 必须与该 model 的 pricing currency 一致（一个模型一张价目表）。

**理由**：原实现把占位价硬编码在 cmd/mini 的 buildCatalog 里——价格随
厂商调整时需要改代码重编译；而 ladder.yaml 本来就是"成本分层"的配置
载体，计价属于它。四项分价（而非单一均价）是成本公式的既定口径
（ADR-0027 第 2 条）：缓存命中与思维链的单价差数倍，均价记账会让
"缓存省了多少钱"与"思维链烧了多少钱"不可见。

**回退代价**：回到代码内占位价，价格调整 = 改代码 + 全量重编译 + 重发版；
回到单一均价，思维链占比与缓存收益从成本报表里消失。

---

## ADR-0030：Fossil 集成的六项实测裁决

**状态**：Accepted（对应设计文档 13.8 / Part 8.4；2026-09-12 阶段 6 交付时记录）

**决策**（每条都有 fossil 2.26 手工探测或真机依据）：

1. **author 是三个框架用户**（human / agent / system），不是 "user:role"
   字符串。`fossil commit -U <user>` 要求用户先 `fossil user new` 注册
   （"no such user" 拒绝）；InitRepo 统一注册三个用户。Part 8.4 的
   "author 按调用路径填" 落为 commit 调用点的常量（agent.doCommit 恒传
   UserAgent——Agent 无法影响，原则 4）。
2. **commit 带 `--no-verify-comment`**：fossil 默认对 comment 做
   fossil-wiki 格式检查，`<tag>`/`&`/`[links]`/`_下划线_` 都是触发词——
   commit message 是机器生成的结构化摘要（含子 report 文本片段），
   wiki 误报是常态（真机实测：`<angle> brackets & wiki [links]` 直接被拒）。
   timeline 的可读性不依赖 wiki 渲染。
3. **`mtime-changes off`（仓库级设置）**：fossil 默认按 mtime 判定文件
   变更，commit 后立即改文件（同一 mtime 粒度内）会被 `changes` 漏检
   （真机实测：子写文件后父立即 commit，改动不可见）。off 后改用内容
   校验和，本地仓库的代价可忽略。
4. **Status = changes + extra 合并**：`fossil changes` 只列**已跟踪**
   文件，新文件在 add 之前只出现在 `fossil extra` 里。单写者流程的第一步
   （发现要提交什么）必须看两者。ignore-glob 在两层都生效（SKIP）。
5. **fossil 的 add 是 checkout 级暂存区**（不是 per-goroutine）：并发
   add+commit 时，先跑的 commit 会带走后 add 的文件，后跑的 commit 可能
   nothing-to-commit（ErrNothingToCommit 哨兵的正确消费场景）。本包的
   writeMu 串行化 + 单写者模型（父唯一提交点）让这条路径在框架内不发生。
6. **不做 lockfile 退避重试**（13.8 的 runWrite 描述被实测否决）：两个
   并发 commit 由 fossil 内部锁串行化且都能成功，不存在需要退避的场景；
   写互斥的真正防线是进程内 writeMu（防框架 commit 与人类手动 commit 撞车）。

**附带**：`.fossil-settings/ignore-glob`（版本化）+ 同名 `.no-warn` 空文件
（消除双值警告）；知识目录占位用 README.md 不用 .keep（fossil add 默认
跳过 dotfiles）。

**依据**：`internal/fossil` 的测试矩阵（真实 fossil 二进制）、
`cmd/marl init` 端到端测试、真机 fork_test（子写文件 → 父 commit →
`fossil ls` 可见子写的 summary.txt，author=agent）。

---

## ADR-0031：内部模型 id = 厂商现行名（消灭 "deepseek/chat" 翻译间接层）

**状态**：Accepted（2026-09-12；用户在阶段 6 评审中指出 ladder-mini.yaml 的
`deepseek/chat` 看起来像早已废弃的 `deepseek-chat`，引发对测试真实性的质疑）

**事实澄清（先于决策）**：

- 线路上发出的模型名**从来不是** `deepseek-chat`：内部 id `deepseek/chat` 经
  `RemoteNames` 在发请求前翻译为 `deepseek-flash`。证据链：
  `docs/design/probe-run-record-phase1*.md` 的逐字节请求体（78 处
  `"model":"deepseek-flash"`）、厂商响应的 `system_fingerprint` 与请求 id、
  2026-09-12 的实时 curl 复核（本 ADR 记录时重跑，厂商正常返回）。
- 厂商文档（docs/deepseek-api/chat-complete.html）的 deprecated 标记针对
  `frequency_penalty`/`presence_penalty` **参数**，不是模型名；现行模型是
  `deepseek-flash` / `deepseek-v4-pro`。

**决策**：尽管线路流量正确，命名陷阱必须拆除——内部 id `deepseek/chat` 与
废弃的厂商名 `deepseek-chat` 只差一个斜杠，任何读配置/读代码的人（包括
框架作者自己）都会误读，且已经实际造成过一次排障浪费（fork_test 的
"no pricing for model" 报错被误判为模型名错误）。因此：

1. 内部模型 id 直接采用**厂商现行名**：`deepseek-flash` / `deepseek-v4-pro`。
   内部 id 与 remote_name 恒等（RemoteNames 机制保留——厂商将来改名时只改
   配置里的 remote 映射，不改代码引用）。
2. 全代码库替换（39 处 Go + 配置 + 设计文档 §7.1/§10.4 等 12 处）；
   `probe-run-record-phase1*.md` 是**历史实测记录，逐字节保留不改**——
   改了就不再是"当时的记录"。
3. 缓存键形态随之变化（`deepseek-flash@deepseek-main`）：内部 id 进缓存键，
   改名 = 全项目缓存前缀一次性失效（ADR-0015 预期的成本，一次性付出）。

**教训（写给后续阶段）**：内部命名不应模仿厂商命名的历史形态——"看起来像
废弃名"与"是废弃名"在评审者眼里无法区分，而解释成本每次评审都要付一次。
命名选择要按"最坏误读"而不是"最准确语义"来淘汰。

**依据**：实时 curl 复核（2026-09-12T17:29+08:00，`deepseek-flash` 正常返回）、
改名后全量测试通过、真机 mini 复跑（厂商 tool_call id + 真实仓库目录内容）。

## ADR-0032：统一 Actor 模型的六项落地裁决（阶段 12 / Part 14）

**背景**：Part 14 把人类与 Agent 在交互面统一为 Actor（人类进进程表），
删四个特例机制（根特殊化、Gate 投递、say 注入、escalation 终态）。

1. **ActorID 是 types.AgentID 的别名**，不是独立类型。既有 Agent 身份
   （Log/audit/缓存桶/fossil author）全以 AgentID 为键；可判别性由
   `human:` 前缀承担（Agent = 无前缀），格式契约在 actor 包的构造/
   校验函数。全仓换类型没有行为收益，只有 churn。[权衡: 放弃"编译期
   排除人类 ID 混入 Agent 字段"的类型安全——换来零迁移成本；混用的
   防线是 ValidID/IsHumanID 的显式校验点。]

2. **Kind 的读取只允许两处**：呈现层（图标/颜色）与拓扑记账（注册点把
   Kind 映射为 Process.AIRecursion，max_depth 只约束 AI→AI fork——
   裁决期读的是进程表的拓扑事实，不是 Kind 分支）。权威判断一律读
   CapSet：AI 的 spawn 许可 = CanSpawnAtDepth 谓词（Profile 形态），
   人类 = CapSet.CanSpawn（被收窄的替身人类被拒——纪律 1 的反例由
   TestHumanCapsAuthority 钉死）。

3. **FileApprover 删除，审批往返信封化**。need_human 是 Manager 的
   **决策**（不阻塞）；MsgGateRequest 经人类 Actor 的文件后端投递，
   裁决是 MsgGateReply，grant 由 ResolveGate 兑现。文件编码/nonce/
   静默窗的单一实现点在 actor.FileBackend（从 gate/file.go 迁移并
   泛化）。回执的 nonce 对账锚点是 FileBackend 私有的 issued 记忆
   （Deliver 时登记）——陈旧回放结构性排除。

4. **深度记账重编号**：人类 d0、项目 Agent = AI 第 1 层（旧根 d0 →
   全体 +1）。max_depth=3 = 三层 AI 递归，能力面与旧编号一致（实测
   topo_test 的三层拓扑 + MAX_DEPTH 演示不变）。[权衡: 一次性的测试
   与文档数字改版，换来 Part 14.6 的语义纯度（人类 spawn 不占 AI 额度
   不再需要特判）。]

5. **grants/ 落盘用 frontmatter 而非 internal/config 的 YAML 解析器**。
   config 包依赖 gate（规则解析），gate 反向引包成环；grant 文件是
   键值对齐的 frontmatter 形态，20 行解析的复制成本低于反转依赖方向。
   文件记录 granted_by（ActorID）/granted_at——授权链从决策时点延伸
   到重启之后。

6. **escalation 与讨论机制保留**（14.12 改动清单未列重构）：escalate.
   Mailbox 与 discuss 的 verdict 流是收件箱子树（requests/ / discussions/，
   与 approvals/ + grants/ 同属控制面）；把它们改写成信封是纯形式收益
   低、破坏面高的操作——按"删的比加的多"标准不做。

**实测记录**：FileBackend 的 gate 往返（Deliver 模板 → 人类 @grant next 2
→ Receive 解析 → done/ 归档）在 fossil 2.26 / Linux 实测通过；nonce 不
匹配（人工篡改 frontmatter）被拒且文件留观。ScriptedHuman 四场景
（讨论/审批/escalation/grant）进 CI。`marl start` 真机跑通（DeepSeek
真密钥）：项目 Agent report 落人类收件箱；`marl status` 显示人类为根的
监督树。

## ADR-0033：格式纠偏重试与 `output_truncated` 类（真机发现 #2/#3）

**实证**：dogfooding（13 次运行）中 5 次死于 `malformed_output` 终结；
curl 复现钉死因果——模型把大程序塞进一次 tool_call 的 arguments，
`finish_reason=length` 截断 JSON → `json.Valid=false` → malformed →
Run 终结。vendor 响应里的截断信号被折叠，分辨率丢失。

1. **类表增项 `ErrOutputTruncated`**（finish=length + tool_call 参数非法
   JSON 的组合判定）：与 malformed 的区别是成因（预算 vs 格式能力），
   处置指引因此不同（拆小 vs 改格式）。
2. **格式纠偏 Transient 落地**（denormalizer 注释早有预告）：malformed /
   truncated 不终结 Run——注入 Part 3.6 的纠偏 Transient（下一轮编译
   送达模型）+ 消费一轮预算重试；Run 内上限 **3 次**（防循环烧预算），
   升级证据照记。
3. **Transient 消费边界的缺陷顺带修复**：旧 clearTransients 在轮末无差别
   清空——handleTurn 期间新加的提示活不过当轮轮末，模型永远看不到。
   修正为"清除已编译送达的前缀"（consumedTransients 记账点）。

## ADR-0034：spawn 的 `writable_paths` 升为 schema 必填（真机发现 #6）

**实证**：模型在任务文本被显式警告的情况下，两次 spawn_batch 仍把权限
写进 task 文本、漏填结构化字段 → 全树只读（孙 PATH_READONLY 级联报废）。
"空 = 只读"的缺省对人类是安全设计、对 LLM 是静默陷阱（它看不见自己被降权）。

1. **schema 必填**（spawn_subagent 与 spawn_batch.items）：漏填 →
   BAD_ARGS + 引导文案（"权限写在 task 文本里不算数；只读子用空数组"）
   ——漏填从"静默降权"（不可见）改为"显式拒绝"（可修正）。
2. **意图层同面强制**（厂商对 required 的执行不保证）：writable_paths
   三态指针（nil=缺失 / []=显式只读 / 非空=授权）。
3. **"漏填不继承"的原则不变**（Part 9.2 防静默提权的立场不动）——只改
   可见性，不改权威。
4. **缓存注记**：schema 冻结字节变更 = 全项目缓存前缀失效（一次冷启动）；
   与 spawn_batch 的括号修复同批落地（旧字节从未产出过一次成功的真跑
   调用，成本为零）。真机复验：修复后 5 次 spawn 全部带正确 writable。

## ADR-0035：收件箱的类型化静默窗（真机发现 #7）

**实证**：`marl say` 六次注入五次未被消费——静默窗（10s，为人编辑设计）
对 CLI 的单次原子写是纯死等，快收尾的 Run 总在窗内死掉。

1. **静默窗按文件类型分策略**（不对称是数据，frontmatter 的 type 为键）：
   `direct`（机器单次写）→ 1s；`gate`（人类就地编辑）→ 保持 10s；
   discussion verdict / escalation 维持既有机制不动。
2. **真机复验**：say 注入 → 1s 消费归档 → human_note 进 Agent 上下文
   （Log #24）——"下一轮编排自然看到"的语义边界（不保证被执行）不变。

## ADR-0036：GUI 服务接口（阶段 14：`marl serve` + internal/server）

**定位**：GUI 的全部操作都是对 `marl serve`（每项目一个本机守护进程）
的 HTTP 调用；`internal/server.App` 是装配 + 操作面（CLI 的 runStart 与
serve 共用同一装配——单一实现点），http.go 是薄传输层。

1. **事件流的单一数据源是 audit_events**（追加只读的旁路真相）——
   SSE 只做游标轮询（`GET /api/v1/events?since=<seq>`，AfterSeq 过滤
   是 store 的增量面），不建 pub/sub：GUI 看到的与 CLI/status 同源同序。
2. **审批/讨论回复走文件通道**：API 的"批准"= 把 @ 命令行写进收件箱
   文件——GUI 是"人类的笔"，原则 4 的通道语义不变；没有特权后门。
3. **控制面项目 id = 基名 + 路径哈希**（`<base>-<sha8>`）：dogfooding
   的测试隔离实证暴露"同名目录共享收件箱/授权库"的串线缺陷——哈希
   锚定绝对路径后天然隔离。
4. **默认只绑回环**；跨机访问须自配 token（`MARL_API_TOKEN`）+ TLS。
5. **配置写入先验证后落盘**（Parse + ParseLimits + ParseGateRules +
   ValidateRules 全过才写；422 带可读错误）——GUI 的保存键不会把坏
   配置写进磁盘。

## ADR-0037：宿主契约层（依赖倒置收口，阶段 15）

**背景**：dogfooding 后的代码审查发现——serve/attached 走了 `App`，
但 CLI 的 say/stop **绕过了接口**（say 直写收件箱文件、stop 直发信号）；
且 App 是具体 struct，"交互接口"只隐式存在于 HTTP 路径上。

1. **契约包 `internal/contract`**：`Interaction` 接口（任务生命周期/
   观测/交互/管理四个分组）+ 全部 DTO（AgentView/RunStatus/InboxItem/
   GateDecision/DiscussionView/CheckResult）——零引擎依赖（只 import
   types 与 store 的数据类型）。宿主只看契约。
2. **三个实现，同一语义**：
   - `server.App`（进程内直达）—— serve / GUI 后端 / attached start；
   - `server.HTTPClient`（连 serve 的 REST/SSE）—— daemon 在跑时的
     CLI / TUI / IDE 插件；
   - `server.FileMailbox`（文件投递兜底）—— 无守护进程时的 say/审批/
     讨论/配置面；引擎态操作如实报 `contract.ErrNoDaemon`。
   一致性由**场景对拍测试**守护（同一场景驱动 App 与 HTTPClient 断言
   同结果——TestContractConformance）。
3. **detach 的形态统一**：`marl start --detach` = 拉起 serve（任务经
   `-task` 载体）——之后一切宿主对后台任务都是 HTTPClient；serve.lock
   （pid+addr）是"daemon 在吗"的判据；`marl stop` = 任务停止 + 无任务
   时守护进程退出（POST /api/v1/shutdown）。
4. **App 与 FileMailbox 共享 `LocalFiles` 文件核**（收件箱/审批/讨论/
   配置/知识库）——文件可达的操作在两个实现里逐字节同语义，不写两遍。
5. 事件流新增 JSON 游标面（`GET /api/v1/events/since/{seq}`）——
   HTTPClient.Events 的轮询形态；SSE 保持给流式前端。
