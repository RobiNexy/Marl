# Marl 阶段 7+8 测试报告（Profile 与知识库 / 人机协作最小闭环）

**日期**：2026-09-12
**范围**：设计文档 13.9（阶段 7：Profile 与知识库）与 13.10（阶段 8：人机
协作最小闭环）的全部交付物。
**性质**：全量测试——单元测试（`-race`，真实 fossil 二进制）+ 编译后可执行
软件的端到端功能测试（`marl init` / `marl knowledge lint` / `marl status` /
`discuss_loop` 全流程回放 + fossil timeline 验证）。

---

## 1. 结论

**通过**。13.9 / 13.10 的交付判据全部满足：

| 交付判据 | 结果 | 证据 |
| :-- | :-- | :-- |
| 阶段 7：`compileView` 产出含常驻块的 CanonicalRequest | ✅ | 单测 TestCompileStandingOrdersPlacement（system → standing → 私有段 → 历史的段序，Stability=frozen，跨轮逐字节稳定）+ discuss_loop 回放的 stderr 段打印 |
| 阶段 7：`marl knowledge lint` 能检测超限 | ✅ | CLI 实测：71 est-token 退出 0；1283 est-token 退出 1 并带逐文件报告 |
| 阶段 7：Profile 加载 + 继承合并 + 提示词索引 | ✅ | internal/profile 单测全绿（Part 6.6 规则逐条） |
| 阶段 8：Agent 调 `request_discussion` → Blocked(Discussing) → 状态可见 | ✅ | TestDiscussionFullLoop（Agent 侧）+ `marl status` 的 audit 还原（Blocked(discussing) 渲染） |
| 阶段 8：批注 → 响应 → @approve → 结论落地 | ✅ | discuss_loop 全流程：批注轮 + 修订轮 + approve；`contracts/topic.md`（slug 化）内容 = 草稿 v2 |
| 阶段 8：timeline 上有 author=human 的落地 commit | ✅ | `Finalize「测试契约」after discussion … (user: human)`；草稿 v1/v2 均为 (user: agent) |

---

## 2. 静态检查与单元测试

```bash
go build ./... && go vet ./...     # 通过
go test ./... -race -count=1       # 全部通过（18 个包，482 个测试）
```

### 2.1 阶段 7/8 新增的测试矩阵

| 包 | 新增覆盖要点 |
| :-- | :-- |
| **internal/profile** | LoadAll（含能力校验/重复 id/空目录拒绝）、Part 6.6 继承合并逐条（标量逐键覆盖、allowed_skills 非空覆盖/空继承、require/prefer 并集去重、include_roles 去重）、extends 环与未知父、Get 快照语义、Reload 变更报告与失败保留、-race 并发 Get |
| **internal/profile (prompt.go)** | ReadPrompt 返回整个 md、双 id 拒绝、`../` 绝对路径越界拒绝 |
| **internal/knowledge** | Part 12.4 ①–③ 逐条：字节稳态（重复编译/目录搬迁/CRLF-LF 三形）、README.md 排除、目录缺失报错（不是空集）、**超限硬失败** + 逐文件报告（1200-token 阈值，13.9 的"加到 1200 → lint 报错"条目） |
| **internal/config** | 行内流式列表 `[a, b]` 支持（实证：设计骨架 `_default.yaml` 使用流式列表，原解析器拒绝自家骨架——新增支持 + 单测调整） |
| **internal/agent (context.go)** | 段序契约、hidden 挂载绝不出现（Part 6.10 约束 1）、任务描述单行化、无常驻块时空段剔除 |
| **internal/agent (discuss.go)** | 全闭环（open → Blocked → 批注 → 修订 → approve → Finalize → 结论落 Main Log/View）、ctx 取消保留会话（可恢复语义） |
| **internal/discuss** | 解析容错（frontmatter 缺失/未闭合/nonce 缺失）、裁决判定（@approve / 批注 / reject-as-annotation）、nonce 陈旧轮拒绝、**模板说明里的 @approve 字样不是裁决**（真机踩坑固化，见 §4.1）、真 fossil 全流程（分支/草稿 author=agent/裁决/落地 author=human） |
| **internal/fossil** | BranchCreate/BranchSwitch（实测裁决：`branch new` 需要 BASIS + `--user` + `--user-override`，见 §4.2） |
| **cmd/marl** | status 渲染的 golden 断言（树形/Blocked(discussing)/verdict 路径/不烧钱提示/讨论结束还原 Running）、knowledge lint 端到端 |

### 2.2 关键回归修正

- 既有 `TestLoopListDirThenReadThenReply` 的段数断言从 2 修到 3（私有段
  `<agent_context>` 是新增的冻结段，不是回归）。
- 一次 `-race` 命中并修复：`discussSess`/`discussPending` 旁路读取的锁纪律
  （intentDiscussion 的写路径补 `a.mu`——单写者字段被测试跨 goroutine 读）。

---

## 3. 编译后软件的功能测试（端到端）

### 3.1 命令矩阵

| 命令 | 结果 | 说明 |
| :-- | :-- | :-- |
| `marl init <dir>` | ✅ | 骨架 7 文件入库、首次 commit（author=system） |
| `marl knowledge lint`（上限内） | ✅ 退出 0 | `编译后 71 est-token / OK：71 est-token（上限 1000）` |
| `marl knowledge lint`（1200-token 超限） | ✅ 退出 1 | 逐文件报告 + `请精简至 1000 以内`（✅ 13.9 判据） |
| `discuss_loop`（全 dry-run 回放） | ✅ | 见 §3.2 |
| `marl status -db <store.db>` | ✅ | `🤖 root-1 (depth=0)`（还原自 agent_state 审计；讨论等待行由 discussion 审计还原，golden 见 status_render_test） |
| `fork_test -dry-run` | ✅ | 阶段 5/6 链路回归无坑（compile 变更后父/子链路完整） |
| `mini`（真机） | 未复跑 | 无新接线（mini 不消费 Profile；阶段 4 报告覆盖），非本轮范围 |
| `probe` | ✅ | 缓存命中判据未回归（单测） |

### 3.2 真机输出摘录（discuss_loop，真实 fossil，2026-09-12）

讨论全流程（fossil timeline 节选）：

```text
fossil timeline:
  [human]  Finalize「测试契约」after discussion (discuss_01m2amtwytttp02y9…, agent=root-1): .marl/discussions/discuss_01…/draft.md
  [agent]  marl: 讨论草稿 v2（discuss_01…「测试契约」）
  [agent]  marl: 讨论草稿 v1（discuss_01…「测试契约」）
  [human]  Create new branch named "discuss_01…"
```

结论落地（trunk 的 contracts/，内容 = 草稿 v2）：

```text
契约文件已落地: contracts/topic.md
```

编译段的观察（fakeLLM 把 standing/knowledge 段打到 stderr）：

```text
--- standing 段 ---
<standing_orders>

## rules.md

所有写入必须先经过讨论；.marl/state 不可触。
## style.md

代码风格：契约先行；测试即规格。
…
</standing_orders>
--- knowledge 段 ---
<agent_context>
  <depth current="0" max="0"/>
  <workspace>
    <writable>**</writable>
  </workspace>
  <task>先与人类敲定『测试契约』的接口，结论落 contracts/，然后宣布完成。</task>
</agent_context>
```

审计链（`marl status` 还原的数据源，10 条事件依次对应：
启动→opened→requested→blocked→(等批注)→running→revised→(等 approve)→concluded→idle）：

```text
1 root-1 agent_state   6 root-1 discussion_revised
2 root-1 discussion_opened  7/8 root-1 agent_state
3 root-1 discussion_requested  9 root-1 discussion_concluded
4/5 root-1 agent_state  10 root-1 agent_state
```

### 3.3 阻塞语义的实证

讨论等待期间 Agent 不做任何 LLM 调用（fakeLLM 的脚本计数只前进 3 次：
发起→修订→结束；等待轮没有编译/执行发生）——Part 11.7 ③"阻塞是便宜的"
在本回放中可观测。

---

## 4. 真机/实测缺陷与修复

### 4.1 verdict 模板说明里的 `@approve` 把自己变成了"永久批准"（严重）

**现象**：discuss 包真机测试 `TestFullDiscussionLoop`——人类写**批注**
（无裁决命令）时 Manager.Wait 返回了 Approved。
**归因**：裁决关键词在 frontmatter 后的全文找；模板自带的说明段
（"命令词：@approve …"）在批注轮里原样存在，先命中关键词。
**修复**：先剥离模板提示段（`## 批注` 之前的说明文字），在人类书写的
注释区里找裁决；固化为回归测试 `TestApproveEchoInTemplate`。这个坑的
教训是**"模板 + 裁决关键词"组合必然自触发**，剥离必须先于判定。

### 4.2 fossil `branch new` 的三个必要参数（实测裁决，非文档化）

**现象**：`fossil branch new <name> <user>` 依次报
"cannot determine user → cannot figure out who you are"。
**归因**（手工探测 fossil 2.26）：`branch new` 的签名是
`branch new BRANCH-NAME BASIS ?OPTIONS?`——第二个参数是 **分叉基准
check-in**（trunk）而不是 user；内部提交的 user 需要
`--user human --user-override human`（--user-override 单独不解除
"cannot figure out"）。
**修复**：`fossil BranchCreate` 固化为
`branch new <name> trunk --user human --user-override human`，测试钉死该语义。

### 4.3 Go flag 不支持子命令形态（阶段 6 事故的根因修尽）

**现象**：`marl init /tmp/x` 的位置参数被忽略（-dir 未生效），init 落在
当前目录。
**修复**：`cmd/marl` 引入手动子命令分发（main.go：init / knowledge / status），
init 接受位置参数 dir（`marl init [dir]`，与设计文档的调用形态一致）；
失败模式是显式的（未知子命令报错、双子句报错，静默落到当前目录的
可能性被关闭）。

### 4.4 环境事故（诚实记录）

开发过程中的一次 `go run ./cmd/marl init /tmp/x`（4.3 的缺陷）在**项目
仓库根**建了 fossil checkout。已彻底清理（含 .marl/、.fslckout、
.fossil-settings/）。此条与阶段 6 报告 §4.5 是同类事故的第二次实录——
根因这次被修在代码里（4.3），而不是只修在习惯里。

### 4.5 config 解析器拒绝自家骨架的流式列表（一致性缺陷）

**现象**：profile 单测里 `require: [tool_call]` 加载失败——原 yaml 子集
"行内流式列表显式拒绝"，而 `marl init` 骨架的 `_default.yaml` 正用了它
（阶段 6 起就存在，此前没有 Profile 加载器去实际读它）。
**归因**：两处契约矛盾——骨架的写入形态与解析器的支持形态。
**修复**：解析器新增流式列表（仅标量项），doc 注释同步；拒绝清单里
仅保留嵌套花括号等结构歧义形态。

### 4.6 discuss 的 DraftRelDir 默认值曾落在 `<root>/discussions`（路径错位）

**现象**：真机测试 Open 后 draft.md 不在 `.marl/discussions` 下。
**修复**：默认 `.marl/discussions`（仓库内、进版本控制、讨论分支涂写语义与
custom `MARLDir` 的骨架一致）。

---

## 5. 交付物清单

| 文件 | 内容 |
| :-- | :-- |
| `internal/profile/{loader,prompt}.go` (+test) | Profile 加载/继承合并（Part 6.6 树层 + 表达式实现）/提示词索引 |
| `internal/knowledge/{doc,standing}.go` (+test) | preferences/ 编译器（字节稳态 + 1000-token 硬上限）|
| `internal/agent/context.go` (+test) | 段序：system → standing → 私有段（Part 6.10 纪律） |
| `internal/agent/compile.go` | compileView 的 frozen 前缀分区改造 |
| `internal/agent/discuss.go` (+discuss_int_test) | request_discussion 意图 + Blocked(Discussing) 阻塞循环 |
| `internal/discuss/{discuss,watcher,merge}.go` (+tests) | 讨论 Manager / verdict watcher（静默期轮询）/ Finalize（author=human） |
| `internal/fossil/branch.go` | BranchCreate/BranchSwitch（实测参数语义） |
| `cmd/marl/main.go` + `knowledge_lint.go` + `status.go` (+tests) | 子命令分发 + knowledge lint + status |
| `cmd/marl/init.go` | 位置参数修复（cmdInit） |
| `cmd/discuss_loop/main.go` | 阶段 7+8 的端到端回放 harness（无网、真 fossil） |
| `internal/config/yaml.go` (+test) | 行内流式列表支持 |
| `docs/test_report/phase7-8-test-report.md` | 本报告 |

## 6. 已知限制与盲区（诚实清单）

1. **"不做"清单按 13.9/13.10 执行**：vendor/promote 不做；escalate /
   request_human 三形态 / 超时处理不做；verdict 的 `@reject`/`@abandon`
   暂以批注通道恢复（Part 11.2 的关键字语义在 harness 里可见；独立语义
   属阶段 10 打磨）。
2. **fsnotify → 轮询**：设计文档写 fsnotify，本实现是 mtime 轮询 +
   Quiescence 静默窗口（默认 10s，可配）。语义不变面已收窄为
   `PollInterval/Quiescence` 两个参数；换 fsnotify 时不需要动 Agent 侧。
3. **`marl status` 的数据源是审计还原**：进程表是运行时内存态，CLI 进程
   看不到；status 从 audit_events 重建近似快照（允许落后一个事件——在
   status.go 的注释与实现里都有声明）。daemon 化后此近似将被进程表取代。
4. **未复跑的旧真机面**：`mini`（真 API）与 probe 的缓存真跑未在本次
   中重复（无新接线，阶段 4/5 报告覆盖）；`fork_test -dry-run` 回归全绿。
5. **未充分激活的能力维度**：讨论仅覆盖单人单 Agent、单一 verdict 的
   手工流程；**并发讨论**（多个子各开一个讨论 / 父同时 Blocked(WaitChildren)
   与 Blocked(Discussing)）只有 pump 缓冲的结构性论证与既有测试守护，
   故障注入（verdict 读走一半崩溃、分支 checkout 冲突）未做——阶段 9
   的 Watchdog 与多拓扑验证承接。
6. **结论落地的文件内容是"草稿头部 + v2 正文"**：Finalize 直接落草稿的
   最终版文本（含讨论标题头）。如果未来要求"契约文件只要正文"，需要一个
   显式的 strip 约定（当前没有——诚实暴露而非隐藏处理）。
7. **Assumptions made**（交付尾部假设清单）：
   - `Task.Budget.TimeoutMs` 零值物化为 24h [推断]（Watchdog 未落地，
     阶段 9 会重新处理这一口径）。
   - 控制面目录由装配方（本阶段是 discuss_loop；将来是 daemon）创建，
     Agent 无需也无法触达。
   - fossil 的 checkout 环境里 USER 环境变量可能缺失——所有写路径显式
     `--user`，不依赖 fossil 的默认用户判定。
