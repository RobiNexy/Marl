# Marl 阶段 3 测试报告（压缩与编排操作）

**日期**：2026-09-12
**范围**：设计文档 13.5（阶段 3）的全部交付物——编排操作原子集（`internal/orchestrate`）、
压缩执行层（`internal/compress`）、Agent 触发与采纳（`internal/agent`）、交付检查命令
（`cmd/mini -task files`）。
**性质**：按交付要求执行**全量测试**——不只是单元测试，还包括对编译后的可执行软件
做端到端功能测试（脚本化回放 + DeepSeek 真机跑）。

---

## 1. 结论

**通过**。13.5 的交付判据全部满足：

| 交付判据（13.5） | 结果 | 证据 |
| :-- | :-- | :-- |
| Agent 上下文满时自动压缩，压缩后继续跑任务 | ✅ | 真机跑：10 文件任务全程 8 次压缩，每次压缩后任务继续，最终回复正常产出（§3.3） |
| 新 View 比旧 View 小 ≥20% | ✅ | 真机 8 次压缩收益 25.7%~30.6%（§3.3）；dry-run 8 次收益 30.8%~32.3%（§3.2） |
| SUM 的七个章节都存在 | ✅ | 机械校验器是采纳的前置条件（缺章节的 SUM 无法被采纳）；真机 8 份 SUM 全部通过校验 |
| 手动调 `split_message` 能拆长消息 | ✅ | 单元测试覆盖 delimiter/semantic 两策略、修复链与禁切区吸附（§2.3） |
| `reorder_message` 能调整顺序 | ✅ | 单元测试覆盖 Position 改写、编译序断言、Stability 边界拒绝（§2.3） |
| 拆分后的段落覆盖原文、无重叠 | ✅ | 单元测试断言行区间连续覆盖 [1,N]、无重叠、切点不落禁切区（§2.3） |

真机测试发现并修复了 **2 个真实缺陷**（§4），均带回归测试。已知限制与盲区见 §6。

---

## 2. 静态检查与单元测试

### 2.1 环境与命令

```text
Go: go1.27.0 linux/amd64
SQLite: modernc.org/sqlite v1.57.0（纯 Go，无 CGO）
```

```bash
go build ./...          # 通过
go vet ./...            # 通过，无告警
go test ./... -race -count=1   # 全部通过（见下表）
```

### 2.2 单元测试矩阵（-race，全部通过）

| 包 | 测试函数数 | 覆盖要点（阶段 3 新增部分加粗） |
| :-- | --: | :-- |
| internal/types | 9 | 零值契约、EstimateTokens 口径 |
| internal/store | 7 | SQLite Log/View 往返、ULID、错误哨兵 |
| internal/wire | 19 | Normalizer 字节稳定、Assert、Denormalize、ADR-0020/0022 |
| internal/skill | 10 | file_read/write/list_dir、原子写、注册表冻结序 |
| internal/ns | 4 | Resolver 三模式、hidden→ENOENT |
| **internal/orchestrate** | **14** | **七个 Op 的契约断言**（见 §2.3） |
| **internal/compress** | **17** | **SUM 校验 / L0 / 压缩主流程 / Admit / 预算 / 禁切区**（见 §2.4） |
| **internal/agent** | **9** | **压缩触发-采纳-继续跑的集成测试**（§2.5） |
| cmd/probe | 6 | 阶段 1 探测工具的契约 |

### 2.3 orchestrate：编排操作的关键断言

- **不就地修改**：每个 Op 的 Apply 前后对传入 View 做深比较（Operation 后置条件）；
- **exclude/restore/pin/unpin**：布尔位语义、目标不存在 → `ErrNotFound`；
- **reorder**：Position==0 合法、NaN/±Inf 拒绝；跨 Stability 边界重排被拒
  （模拟重排后校验单调性，违反即 `ErrInvalid`）；
- **annotate**：追加 `ProvAnnotatedOf` 外壳 + View 换引用；拒绝 tool_calls 意图条目
  与 thinking 条目（会割裂配对/改写审计证据）；
- **split(delimiter)**：分隔符行丢弃、空段丢弃、分段覆盖原文、View 原位替换
  （Fractional Index 中点插入）、原条目留在 Log；
- **split(semantic)**：LLM 段的修复链——越界 clamp、重叠消除、缝隙填合、切点吸附到
  禁切区外（含"区顶到文末且前方无余地 → 撤销切点"的边界）、零值 StartLine 修复；
  修复后只剩一段 → `ErrSingleSegment`；
- **零值策略**：`CompressionPolicy{}` 非法（阈值/尾轮/收益三字段零值各自的具体损害
  在类型注释里枚举）。

### 2.4 compress：压缩执行层的关键断言

- **编排调用形态**：无 tools、thinking=off、split 走 JSON mode、frozen system +
  stable user 两段——真机缓存友好（真机实测编排调用 `cache_read=128~2304`，§3.3）；
- **独立预算**：预算耗尽时调用**发出前**被拒（不靠厂商 429 发现）；
- **独立账本**：每次成功调用推送 `UsageSink.RecordOrchestration`；
- **SUM 校验**：七章节缺失时**聚合**报错（一次列全）；§3 文件路径的存在性校验
  （拒绝绝对路径与 `..` 逃逸——存在性检查不成为路径探测通道）；"无"字跳过；
- **L0 清理**：去重以（意图, 结果）**对**为单位（§4.2 的缺陷修复）、thinking(audit)
  退出上下文、Log 分毫不动、pinned 豁免；
- **压缩主流程**：新 View = 头部 + SUM + 尾部、SUM 血缘指向压缩区全部条目、
  收益不足报错（阶段 3 不做 L2，错误文本带数值）、校验失败按 MaxRetries 重试且
  **重试温度更高**（实测 0.1 → 0.3）、`ErrNothingToCompress` 哨兵、传入 View 不被修改；
- **Admit**：空参数 → `ErrInvalid`、悬空血缘 → `ErrNotFound`；
- **禁切区扫描**：代码块（含未闭合围栏 fail-closed）、引用块连续行吞并。

### 2.5 agent：压缩接入的集成测试

- `TestLoopCompressesWhenHeadroomLow`：真实 `compress.Compressor` + 脚本化主循环 LLM，
  6 轮读文件任务中压缩触发、每次事件 `SUMAppended && NewTokens < OldTokens &&
  Reclaim ≥ 0.2`、View 引用 SUM（assistant 身份）、Log 真相完整（原始条目一条不少，
  SUM 条数与压缩事件一一对应）、循环跑完产出最终回复；
- `TestLoopPairingSurvivesL0Dedupe`：§4.2 缺陷的回归测试——压缩采纳后的**每一次**
  编译都通过配对不变量检查（带 ToolCalls 的段之后紧跟其全部结果的 tool_result 段）。

---

## 3. 编译后软件的功能测试（端到端）

被测对象是**编译后的二进制**（`go build -o mini ./cmd/mini`），不是测试进程内的函数调用。

### 3.1 命令矩阵

| 命令 | 结果 | 说明 |
| :-- | :-- | :-- |
| `mini -dry-run`（readme 任务） | ✅ exit=0 | 阶段 2 行为回归：list_dir → file_read → 回复；压缩未触发（禁用） |
| `mini -task files -dry-run` | ✅ exit=0 | 脚本化回放：10 轮读文件 + 8 次压缩，收益 30.8%~32.3% |
| `mini -task files`（真机） | ✅ exit=0 | DeepSeek 真机：10 轮 file_read + 8 次压缩 + 最终总结（§3.3） |
| `mini`（真机，readme 任务） | ✅ exit=0 | 阶段 2 路径真机回归：压缩不触发、闭环正常 |

### 3.2 dry-run 输出摘录（`-task files`）

```text
━━━━━━━━━━ 压缩 ━━━━━━━━━━
[轮 3] SUM     估算 33992 → 23621 est-token（收益 32.3%，L0 清理 0 条）
[轮 4] SUM     估算 34292 → 23921 est-token（收益 32.0%，L0 清理 0 条）
[轮 5] SUM     估算 34592 → 24221 est-token（收益 31.7%，L0 清理 0 条）
...
[轮 8] SUM     估算 35492 → 25121 est-token（收益 30.8%，L0 清理 0 条）
━━━━━━━━━━ Log ━━━━━━━━━━
[01] user_input original   依次读取 files/ 目录下的 10 个文件……
[08] assistant_reply summary_of  «summary_of» ## 1. 任务 …（七章节齐全）
```

### 3.3 真机输出摘录（DeepSeek `deepseek-flash`，2026-09-12）

```text
[编排账本] agent=mini-agent task=mini-task prompt=2497 completion=217 cache_read=2304
[编排账本] agent=mini-agent task=mini-task prompt=2478 completion=244 cache_read=2304
（共 8 条编排账本记录；第 2 次起 cache_read=2304 —— 编排调用的前缀缓存跨请求命中）
━━━━━━━━━━ 压缩 ━━━━━━━━━━
[轮 3] SUM     估算 23492 → 16866 est-token（收益 30.6%，L0 清理 0 条）
[轮 4] SUM     估算 24037 → 17503 est-token（收益 29.4%，L0 清理 0 条）
...
[轮 10] SUM    估算 27673 → 21040 est-token（收益 25.7%，L0 清理 0 条）
```

- SQLite 落库验证：`SELECT count(*), SUM(prov='summary_of') FROM log_entries`
  → `31 | 8`（原始条目 23 条一条不少 + SUM 8 条，真相之源只追加）；
- 压缩期间主循环不中断：每轮压缩后下一个文件的读取照常发起；
- 观测点：SUM 的 completion 稳定在 200~280 token（篇幅纪律生效）；
  编排调用的缓存命中说明"无 tools 最小上下文"的前缀在多次压缩间保持稳定。

---

## 4. 真机缺陷与修复（本次交付的核心产出之一）

真机测试的价值在这两节——它们都是单元测试的"合理输入"抓不到的。

### 4.1 缺陷一：轮口径——进度旁白被当成轮起点

**现象**：真机跑在第 3 轮压缩时报 `reclaim 0.000 < min 0.200`。
**归因**（落库序列回放）：模型在工具调用之间输出进度旁白（"f01.txt 已读取…继续"），
初版轮定义把"带内容的 assistant 回复"也当轮起点 → 旁白自成一轮 → 压缩区
= 一条旁白（~100 est token）→ SUM（~3000 est）比压缩区还长 → 收益为负。
**修复**：轮起点 = assistant 的 **tool_calls 意图条目**；文本回复附着于当前轮
（ADR-0026 第 2 条）。**回归**：`internal/compress` 的轮分组测试 + 真机复跑通过。

### 4.2 缺陷二：L0 去重破坏调用配对（严重）

**现象**：真机跑在第 2/3 轮 Execute 时被自家 Assert 拒绝：
`第 3 条消息（role=assistant）打断了 assistant.tool_calls 与其结果：还有 1 个 id 未配对`。
**归因过程**（值得记录，因为它是"编译后软件测试"价值的直接证据）：

1. 单元测试全绿、失败请求的 Log 序列手工重放**通过** Assert → 说明失败请求与
   Log 序列不一致；
2. 给 `cmd/mini` 的直连通路加失败现场转储（Canonical 段序列落盘），复跑 9 次
   捕获现场：**tool_result 段从请求里消失**，而 Log 里条目俱在；
3. 给 `agent.compileView` 加 env 门控诊断（跳段打印 + View 条目打印），再复跑：
   `view[2] role=tool vis=false` —— **该条目在 View 里被软删除了**；
4. 定位：模型分两次读同一文件（`offset` 续读）→ L0 去重"同一文件的多次
   file_read 只留最后一次"把**旧读取的结果**排除，但其 **tool_calls 意图**
   仍在 View → 编译出"没有结果的工具调用"。

**修复**（ADR-0026 第 5 条）：

- L0 以（意图, 结果）**对**为单位排除：结果被去重时其配对意图一并排除；
  一个意图只有在其**全部**结果都被排除时才排除；
- 归属按**最近前向意图**计算（与线路协议分组语义一致）——模型跨轮复用同一
  tool_call id 时（阶段 1 探测证实厂商接受）仍然正确；全局 id 表会把同 id 的
  结果错挂到第一个意图（回归测试抓到了这个次生缺陷）；
- `validatePairing` 在三处 View 产出点（L0-only / SUM / Admit）做与 wire.Assert
  同构的终检——违反即框架 bug，在产出点失败而不是发给厂商。

**回归**：`TestLoopPairingSurvivesL0Dedupe`（agent，编译级断言）、
`TestL0DedupeDuplicateCallIDs`（compress，同 id 归属）。修复后真机连续 5 轮
全通过（含模型重复读同一文件的场景）。

### 4.3 观察到但未复现的异常（诚实记录）

在缺陷二的归因过程中，第 4/7 次真机跑出现过同型 assert 失败，当时失败请求
无法转储（BuildRequest 失败时 WireRequest 为 nil，最初只转储了归一化产物）。
加注转储 Canonical 输入后复现点已捕获（§4.2 的第 3 步），归因闭环。
转储设施保留在 `cmd/mini`（env 无关、失败时自动落盘 `/tmp/opencode/failed-request.json`），
供后续阶段复用。

---

## 5. 交付物清单

| 文件 | 内容 |
| :-- | :-- |
| `internal/orchestrate/ops.go` | 七个 Op 实现 + 修复链 + Fractional Index + 哨兵 |
| `internal/orchestrate/ops_test.go` | 编排操作测试矩阵（14 个测试函数） |
| `internal/orchestrate/compress.go` | CompressionPolicy.Validate 实现 + ErrNothingToCompress |
| `internal/compress/orchestrator.go` | OrchestrationCall 执行核（预算/账本/JSON mode） |
| `internal/compress/sum.go` | SUM 指令 + 机械校验（聚合报错 + 路径存在性） |
| `internal/compress/split.go` | 禁切区扫描（代码块/引用块） |
| `internal/compress/compressor.go` | L0（配对为单位）+ 压缩主流程 + Admit + validatePairing |
| `internal/compress/*_test.go` | 压缩层测试矩阵（17 个测试函数） |
| `internal/agent/compress.go` | checkHeadroom / doCompress / 事件记录 / 防热循环 |
| `internal/agent/compile.go` | 编译序改为 Position 升序（编排配套）+ 诊断点 |
| `internal/agent/compress_test.go` | 压缩集成测试 + 配对回归 |
| `cmd/mini/main.go` / `wire_line.go` | `-task files` 交付检查 + 失败现场转储 + 编排账本出口 |
| `docs/design/decisions.md` | ADR-0026（六项落地裁决） |
| `docs/design/marl_design.md` | 13.5 状态与里程碑表更新 |

## 6. 已知限制与盲区（诚实清单）

1. **本地 token 估算的 CJK 偏差**（M2.5 的领地）：`EstimateTokens` 对纯 ASCII 约
   1 token/字节（高估 ~4×），对中文约 2 token/字（高估 ~2×）。后果：中文 SUM 压缩
   ASCII 内容时，est 口径的 reclaim **低估**真实节省（真机实测出现过 est reclaim=0
   而真实 prompt_tokens 减半的案例）；反之 ASCII SUM 对 ASCII 内容高估收益。
   阶段 3 的对策是篇幅纪律（SUM ≤25 行）+ 语言纪律（摘要语言随内容）+ 加肥压缩区，
   **不是**修估算器——估算器校准是 M2.5 的显式里程碑，须用 20 个任务的
   estimated vs actual 数据做，不属于本阶段。
2. **"不做"清单按 13.5 执行**：压缩收益不足时升级 L2 = 硬编码失败（错误文本带
   reclaim 数值供阶段 4 接管）；split 的 XML 标注块禁切延后（接口已就位，
   `ZoneScanner` 只扫代码块与引用块）；Ledger 的 SQLite 落地在阶段 4
   （本阶段为 `UsageSink` 窄接口 + 控制台出口）。
3. **压缩触发的演示口径**：`cmd/mini -ctx-budget` 是演示用有效窗口（真实窗口来自
   能力表，阶段 4+ 接管）。真机跑的触发时机由该口径决定，不代表 64k 窗口下的
   真实触发频率。
4. **审计表未接**：编排 Op 的 `AuditPayload` 已结构化产出，但 `audit_events` 的
   SQLite 落地与 Agent 级审计接线在后备阶段——本阶段的编排历史可从 Log 血缘
   （Prov/SourceIDs）完整重建，审计缺失不丢事实。
5. **未充分激活的能力维度**：本次交付未触及分布式原语（部分失败、一致性、不可靠
   网络）——单进程单 Agent 的压缩闭环不涉及；故障注入测试（LLM 超时/429/断连下的
   压缩重试路径）目前只覆盖了 fake 层面的厂商错误分类，真机故障注入留到 Pool
   （阶段 4）落地时一并做。
