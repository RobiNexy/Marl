# Marl 阶段 4-5 测试报告（阶梯与成本账本 / Spawner 与单层 fork）

**日期**：2026-09-12
**范围**：设计文档 13.6（阶段 4：阶梯/Router/证据/账本/报表）与 13.7（阶段 5：
Spawner/Mailbox/单层 fork/report 机械检查）的全部交付物。
**性质**：全量测试——单元测试（-race）+ 编译后可执行软件的端到端功能测试
（脚本化回放 + DeepSeek 真机跑）。

---

## 1. 结论

**通过**。13.6 与 13.7 的交付判据全部满足：

| 交付判据 | 结果 | 证据 |
| :-- | :-- | :-- |
| 13.6：Agent 从 r0 起跑，两次失败后升级到 r1 | ✅ | dry-run 升级场景：2 次 BAD_ARGS → score 1.10 ≥ 0.80 → audit `model_upgrade(to_rung=r1)`；升级后请求 thinking=high |
| 13.6：Ledger 记录两级的 token 消耗 | ✅ | 报表：r0=2 调用 10600 token、r1=1 调用 5300 token、总成本 0.0168 CNY |
| 13.6：报表显示分项与总成本 | ✅ | `cmd/ladder_report`（含思维链占比、缓存命中、机械建议列）；真机报表：r0 9 调用 44% 命中、编排 3 调用单独记账 85% 命中 |
| 13.7：父 fork 子，子完成后 report，父收到 report 继续 | ✅ | dry-run + 真跑：父 Log `[06] sub_task_result`（含 child_id/status），父随后汇总 |
| 13.7：子写的文件存在 | ✅ | agent 集成测试：子 file_write 落盘、文件可读、在 writable_paths 内 |
| 13.7：report 进父 Log，父 View 有 sub_task_result | ✅ | 集成测试断言 Log 条目 + View 引用 |

真机测试发现并修复 **3 个真实缺陷**（§4），均带回归测试。

---

## 2. 静态检查与单元测试

```bash
go build ./... && go vet ./...   # 通过
go test ./... -race -count=1     # 全部通过（14 个包，见下表）
```

| 包 | 测试函数数 | 阶段 4/5 新增覆盖要点 |
| :-- | --: | :-- |
| internal/config | 5 | **YAML 子集解析**（ladder 形态、流式映射、注释、9 类显式拒绝） |
| internal/ladder | 10 | **Load 校验**（顺序/重复/缺 thinking/缺价）、**Router**（谓词/打分/越界不 clamp/未知模型 fail fast/能力覆盖整集替换）、**证据**（连续语义/顶档/子失败率/策略校验） |
| internal/ledger | 4 | **成本公式**（Completion−Reasoning 拆分、负值防御）、**Recorder**（类别强制/未知用量跳过/缺价拒绝）、**报表渲染** |
| internal/store | 10 | **SQLite ledger/audit/model_switch**（校验/聚合自证/回填）；**并发 Append 回归**（§4.1） |
| internal/types | 11 | **Namespace.Subset / PatternCovers**（提权拒绝/越界拒绝/fail-closed） |
| internal/spawner | 7 | **裁决闸**（7 类拒绝含 INVALID_INJECT_SEQ）、**生命周期**（report 投递/重复拒/框架代报/建子失败回滚）、**机械检查**（TODO/否定语境/无效状态） |
| internal/agent | 17 | **升级集成**（13.6 场景/无证据不升级/Transient 不计证据/Transient 消费）、**fork 集成**（13.7 场景/越权拒绝回传/深度闸/注入血缘/框架代报） |
| 其余（orchestrate/compress/wire/skill/ns/probe） | 51 | 阶段 1-3 存量回归 |

---

## 3. 编译后软件的功能测试（端到端）

被测对象是**编译后的二进制**（`go build -o mini ./cmd/mini`、`fork_test`、`ladder_report`）。

### 3.1 命令矩阵

| 命令 | 结果 | 说明 |
| :-- | :-- | :-- |
| `mini -task failing -dry-run -ladder ladder.yaml` | ✅ | 升级场景：2×BAD_ARGS → 升级 r1 → 第 3 轮成功 |
| `ladder_report -db … -task mini-task`（对上一条的库） | ✅ | r0=2/r1=1 分项报表 + model_upgrade 记录 |
| `mini -task files -ladder ladder.yaml`（真机） | ✅ | 3 次压缩 + 9 次调用全程记账，任务完成 |
| `ladder_report`（对真机库） | ✅ | r0 9 调用（含编排）44% 缓存命中 0.0273 CNY；编排行单独 3 调用 85% 命中 |
| `fork_test -dry-run` | ✅ | 父 list_dir → spawn → 子 report → 父汇总 |
| `fork_test`（真机） | ✅ | sub_000001 status=done；父 Log `[08] sub_task_result`（含子的完整阅读报告） |
| `mini`（真机，readme 任务） | ✅ | 阶段 2 路径回归（意图工具进冻结前缀后仍正常） |

### 3.2 真机升级路径的说明（诚实记录）

13.6 的交付检查要求 `go run cmd/mini` 观察到升级。真机**无法确定性地**强制
模型产出非法 JSON（升级证据是框架观测的，不受任务文本控制），因此升级路径的
确定性验证由两条互补路径承担：

1. `mini -task failing -dry-run`：脚本回放两次 BAD_ARGS——除 LLM 输出外，
   证据累积、升级判据、重绑定、审计、账本、报表全部是真实代码路径；
2. 单元测试 `TestUpgradeAfterFormatErrors`：fake LLM + 真实 SQLite 账本/审计，
   断言升级后请求的 thinking 档位、Transient 恰好出现一轮、两级账目、审计载荷。

真机跑（`-task files -ladder`）验证的是阶梯模式下的**记账与报表**路径
（不触发升级，任务成功完成）。三类证据合起来覆盖 13.6 的交付判据。

### 3.3 真机输出摘录

**fork_test（DeepSeek，2026-09-12）**：

```text
root-1 (depth=0) state=idle
  ├─ sub_000001 (depth=1) state=idle status=done
[06] tool_result  {"ok":true,...,"child_agent_id":"sub_000001",...}
[08] sub_task_result  "Read src/auth/oauth.go (read-only; no modifications)…
[09] assistant_reply  "子 Agent 已完成。src/ 下共 3 个 .go 文件…"
```

**mini -task files -ladder（DeepSeek）**：

```text
[轮 3] SUM  估算 23492 → 16950 est-token（收益 30.2%）
r0   9 调用  39062 token  44% 缓存命中  0.0273 CNY
编排  3 调用   8149 token  85% 缓存命中  0.0037 CNY
```

编排调用的缓存命中率（85%）高于主调用（44%）——"无 tools 最小上下文"的
编排前缀在多次压缩间保持稳定，与阶段 3 的观测一致。

---

## 4. 真机缺陷与修复

### 4.1 SQLITE_BUSY：deferred 事务的锁升级（跨阶段缺陷，严重）

**现象**：真机 fork 跑中，子的第一次 Log 追加报
`database is locked (5) (SQLITE_BUSY)`，子被框架代报 failed。
**归因**：`Append` 用 `BeginTx`（默认 deferred）做"读 MAX+1 → INSERT"；
WAL 模式下 deferred 事务从读锁升级写锁时，若快照已过期**立即**返回
SQLITE_BUSY——busy_timeout 只护首次取锁，不护锁升级。阶段 2/3 单 Agent
串行追加从未触发；阶段 5 的父子并发追加第一次暴露。
**最小复现**：`TestConcurrentAppend`（双 goroutine × 50 次追加，修复前
稳定失败）。
**修复**：DSN 增加 `_txlock=immediate`——写事务以 BEGIN IMMEDIATE 开始，
取锁阶段即排队，busy_timeout 全程有效。修复后并发测试 ×5 与真机复跑通过。
**教训**：MessageLog 契约写着"必须支持多 Agent 并发追加"，但阶段 2 的
测试只测了单 Agent——契约的并发条款从落库第一天起就没有被测试覆盖。

### 4.2 report 机械检查的否定语境误判

**现象**：真机 fork 跑中，子的成功 report（"read-only; nothing modified"）
被降级为 failed（父仍正常汇总，但子状态错误）。
**归因**：改动声明判据的关键词表命中 "modified"，未识别否定语境。
**修复**：含否定 token（nothing/no files/not/没有/未/无…）的行不算声明；
宁漏不误（漏报由 TODO 扫描与 FilesChanged 兜底）。附回归测试
（`negated change claim is not a claim`）。修复后真机复跑 status=done。

### 4.3 fork 竞态：spawn 与 pending 检查之间到达的 report 被跳过

**现象**：`-race` 下 TestForkChildSilentExit 间歇失败（约 2/10 概率）——
父 Run 正常返回但 sub_task_result 缺失。
**归因**：子可能在父"spawn 注册 → hasPendingChildren 检查"的窗口内就完成
并被代报：pendingChildren 已空而 childReports 有存货 → eventLoop 判定
"无需等待" → flushChildReports 永不执行。
**修复**：`hasPendingChildren` 同时检查 `len(childReports) > 0`（有存货
也要进等待路径，让 awaitChildren 消费已缓冲的信号并 flush）。
**回归**：10 轮 `-race` fork 全量稳定通过。

---

## 5. 交付物清单

| 文件 | 内容 |
| :-- | :-- |
| `internal/config/yaml.go` (+test) | 受限 YAML 子集解析器（阶段 7 的 profiles 复用） |
| `internal/ladder/{ladder,router,evidence}.go` (+test) | 配置加载/静态目录/两阶段调度/证据累积器 |
| `internal/ledger/{ledger,report}.go` (+test) | 成本公式/Recorder/报表渲染 |
| `internal/store/sqlite_ledger.go` (+test) | Ledger/AuditStore 的 SQLite 实现 + 三张新表 |
| `internal/agent/upgrade.go` (+test) | 证据采集/升级执行/审计/Transient 告知 |
| `internal/agent/{mailbox,spawn,intents}.go` (+fork_test, upgrade_test) | Mailbox 泵/意图处理/意图 schema/fork 与升级集成测试 |
| `internal/spawner/{spawner,namespace,reportcheck}.go` (+test) | 进程表/裁决/命名空间构建/机械检查 |
| `internal/types/namespace.go` | Subset/PatternCovers 实现 |
| `cmd/mini/main.go` / `wire_line.go` | 阶梯接入/-task failing/账本出口 |
| `cmd/ladder_report/main.go` | 成本报表命令 |
| `cmd/fork_test/main.go` | fork 交付检查命令 |
| `ladder-mini.yaml` | 交付检查用的阶梯配置（r0/r1/r2） |
| `docs/design/decisions.md` | ADR-0027/0028 |
| `docs/design/marl_design.md` | 13.6/13.7 状态与里程碑表更新 |

## 6. 已知限制与盲区（诚实清单）

1. **计价是占位口径**：静态目录里的单价（In 1.0/Out 2.0 CNY 等）标注
   [待验证]——DeepSeek 官方价随时间变化，权威价格在阶段 7 的 models.yaml
   落地。报表的绝对金额在价格确认前只做相对比较；成本公式本身已按
   ADR-0027 钉死。
2. **"不做"清单按 13.6/13.7 执行**：Pool 并发闸门（父子并发压在同一线路
   直连上）、熔断与健康检测、能力探测、多子并行的专项测试（等待逻辑天然
   支持 N 子）、深度 >1（maxDepth 可配但只测单层）、子崩溃恢复（只有
   框架代报兜底）。
3. **TaskSummary 的 Status 恒为 running**（ADR-0027 第 4 条）：任务表在
   阶段 8+ 落地前，报表的"状态"行没有真值来源。
4. **Profile 系统缺席**：SpawnRequest.ProfileID 只做非空校验（解析在
   阶段 7）；CanSpawnAtDepth 以深度规则代偿（阶段 5 形态：只有根能 fork）；
   OutgoingContext 策略过滤未实现（注入只做越界拒绝）。
5. **意图工具表扩张对缓存的影响**：6 个意图 schema 进入冻结前缀使所有
   Agent 的缓存前缀一次性失效（ADR-0015 预期的成本）；真机 readme 复跑
   确认新前缀下缓存正常建立。
6. **未充分激活的能力维度**：多 Agent 并发下的故障注入（父阻塞期间子
   全部超时、信箱满丢弃）只有框架代报的 happy-path 变体——完整形态随
   Watchdog（阶段 9）落地；本阶段的并发验证以 -race 稳定性为准。
