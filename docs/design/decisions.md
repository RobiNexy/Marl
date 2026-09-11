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

**状态**：Accepted（对应设计文档 Patch 1 / 10.15）

**决策**：`Binding.CacheBucket` 的唯一来源就是 `AgentID`；
`EndpointConfig` 不提供 `bucket_strategy`。

**落地**：请求的 `user` 字段 = `binding.CacheBucket`；`Router.Bind(req, agentID, ...)`
直接把 agentID 写进 Binding。

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

## 变更流程

1. 任何对 `internal/types`、`internal/proto`、`internal/store`、`internal/wire`、
   `internal/skill`、`internal/orchestrate` 中**契约字段**的修改，必须先新增 ADR。
2. 阶段 0 的交付检查（`go build ./...` + `go vet ./...`）必须在每次契约变更后重跑。
3. **Byte-stability 是可测不变量**：冻结前缀（system + 常驻块 + Tools）的编译产物
   必须有 golden test 守着，否则某天一个"顺手清理"的改动会静默毁掉全项目缓存。

