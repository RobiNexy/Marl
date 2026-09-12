# Marl 阶段 9 测试报告（完整拓扑与多层 fork / Watchdog）

**日期**：2026-09-12
**范围**：设计文档 13.11（阶段 9）的全部交付物 + Part 9.3 的一个实测
命名空间缺陷修复（平局挂载取方向，见 §4.1）。
**性质**：全量测试——单元测试（`-race`）+ 编译后可执行软件的端到端
回放（`topo_test` 三层拓扑与 Watchdog 演示、`fork_test` 回归、
`marl status` 渲染）。

---

## 1. 结论

**通过**。13.11 的交付判据全部满足：

| 交付判据（13.11） | 结果 | 证据 |
| :-- | :-- | :-- |
| 根 fork 3 个子，每个子 fork 1 个孙 | ✅ | topo_test：6 节点树（root-1 + sub_1..3 depth1 + 3 个 depth2 孙）；3 个孙产物 `src/child_[abc]_grand_out.txt` 全部落盘 |
| spawn_batch 批量分派 | ✅ | 单次调用 3 项审批通过（`approved:3`），report 逐条回流 |
| 孙到 MaxDepth 再 fork 被拒、错误如实回传 LLM | ✅ | `MAX_DEPTH_REACHED`（措辞按 Part 9.4："请直接执行任务"）出现在孙的 Log tool_result |
| Watchdog 终止悬停子 → 框架代报 failed → 父收到 | ✅ | 内部集成 TestWatchdogKillsStalledChild + topo_test -watchdog（`sub_000007 (depth=1) crashed`，父 Log 有 `框架代报：watchdog 终止：超时…`） |
| `marl status` 显示完整树 + 色块 + 阻塞时长 | ✅ | 递归 printTree + ANSI 色码（auto/always/never 三态）+ `blocked(discussing) 0s` 时长渲染（golden：status_render_test） |

---

## 2. 静态检查与单元测试

```bash
go build ./... && go vet ./...      # 通过
go test ./... -race -count=1        # 全部通过（19 个包，20 个包计数随 -race 全绿）
```

### 2.1 阶段 9 新增的测试矩阵

| 包 | 覆盖要点 |
| :-- | :-- |
| **internal/spawner** (depth.go) | depthGate 三裁决形态：权限谓词拒绝（第一归因位）→ 到顶 `MAX_DEPTH_REACHED`（措辞纪律）→ 通过；用于 13.11 验证的小型表驱动 |
| **internal/spawner** | ChildPlan.Mailbox（子信箱的传递——多层 report 路由链）· Terminate（未启动/已 report 的终态防护 + 单次裁决文本）· Snapshot（含 StartedAt） |
| **internal/watchdog** | 判据独立测试（进程表/Log 替身）：超时判据 → Terminate + reason 到达；超预算（est-token）判据；**无进展只标黄不终止**（Part 8.5 的动作表逐行）；滚动状态行的静默累计；审计成功/失败两条路径 |
| **internal/agent** (wait.go) | WaitAny/WaitN 的判据单元（arrival 信号 + 计数口径：n=1 有 1 report 即醒；n=2 只 1 个 report 等待到 ctx 超时）；策略**单次生效后回退 all** 的语义锁定 |
| **internal/agent** (spawn.go) | spawn_batch 的批量裁决：部分拒绝合法（好项照常启动 + `rejected_items` 逐项回传 NAMESPACE_EXCEEDED），全拒 `SPAWN_ALL_REJECTED`（拒绝清单逐项 JSON）；await 参数启动期校验（all/any/n + n 越界拒绝） |
| **internal/agent** (topology_test) | **三层 fork 全收敛**（根 batch 2 子 × 各 1 孙：两个孙文件落盘、根 Log 2 条 sub_task_result、每子的 Log 各含 1 条孙 report）·孙到顶拒绝·Watchdog 终止（悬停 LLM → Terminate → 框架代报 → 父恢复） |
| **cmd/marl** (status) | 色块（green/yellow/red 按 running/blocked/crashed）·时长（`agent_state` 事件戳 → `h m s` 形态）·`-color auto/always/never` |

### 2.2 millisecond 级 Watchdog 参数的纪律

Watchdog 的 Config 对零值明确非法（Interval/无判据不可全关）——负值
表"关闭该项"（NoProgressAfter=-1），与 types.WatchdogPolicy 的"0 一律
非法"语义同向；测试里千分秒间隔验证判据本身，真实间隔建议 30s~5min
（文档在 cmd/topo_test 的注释里）。

---

## 3. 编译后软件的功能测试（端到端）

### 3.1 命令矩阵

| 命令 | 结果 | 说明 |
| :-- | :-- | :-- |
| `topo_test` | ✅ | 三层拓扑 + spawn_batch：6 节点树、3 个孙文件 |
| `topo_test -watchdog` | ✅ | 追加 Watchdog 演示：悬停子 1s 超预算/超时判据被终止 → 父代报 failed 收尾 |
| `fork_test -dry-run` | ✅ | 阶段 5/6 链路回归（深度仍 1；无 fork 深度破坏） |
| `marl status -db <path>` | ✅ | 树 + 色块 + `stateLine` 带时长 |

### 3.2 真机输出摘录（topo_test，2026-09-12）

```text
🤖 root-1 (depth=0) idle
   └─ 🔧 sub_000001 (depth=1) idle
   └─    └─ 🔧 sub_000006 (depth=2) idle
   └─ 🔧 sub_000002 (depth=1) idle
   └─    └─ 🔧 sub_000005 (depth=2) idle
   └─ 🔧 sub_000003 (depth=1) idle
   └─    └─ 🔧 sub_000004 (depth=2) idle
src/ 下的孙产物:
   a_grand_out.txt
   b_grand_out.txt
   c_grand_out.txt

=== Watchdog 演示 ===
🤖 root-2 (depth=0) idle
   └─ 🔧 sub_000007 (depth=1) crashed
```

Watchdog 路径的核心证据（TestWatchdogKillsStalledChild，-race 稳定）：
父的 Log 收到 `sub_task_result`，内容
`框架代报：watchdog 终止：超时…`，`status=failed`——父全程没有"查询
子跑了多久"（Part 8.5 的时间透明性在测试里被脚本约束）。

---

## 4. 真机/实测缺陷与修复

### 4.1 命名空间"平局挂载"的写静默降级（严重，多年潜在）

**现象**：三层 fork 测试孙的 `file_write src/auth/a_grand_out.txt` 报
`PATH_READONLY`——孙的可写请求明明在裁决里被批准了。
**归因**：孙的命名空间同时含"继承的 read 挂载（同一 pattern，来自父
write 的降级继承）"与"显式的 write 挂载（同一 pattern）"。
`ns.bestMount` 的"最具体"平局破法是"hidden 优先防突"，但**同 pattern 的
两种模式**在这里平局仍未破——挂载表的**首序**（继承面排在前）赢得
平局 → 写被静默降级。这是阶段 5 的深度信息（单层 fork 场景下父的 write
与子的显式请求从不等 pattern 同长），三层拓扑第一次踩到。
**修复**：bestMount 的平局破法改为**能力序取 max**（write > read >
hidden）。语义依据：挂载表是"授权集合"而非"覆盖规则"——显式请求
已经过 Subset 裁决（合法性在裁决层），合并方向取更强能力是授权语义的
自然延伸；fail-closed 的方向被保住（hidden（未识别）仍是 min）。
**验证**：ns 包既有测试全绿 + 三层测试稳定。

### 4.2 ChildPlan 丢失 Mailbox 的"静默断链"风险（设计评审发现，未实测踩到）

**现象**：阶段 5 的工厂没有给子装配 Mailbox——孙的 report 投递在
Spawner.deliverReportLocked 会在子信箱（nil chan）上超时失败，
父永远等不到孙的 report（现象是父无限阻塞在 WaitChildren）。
**修复**：ChildPlan 增加 `Mailbox`（+ `Task`），工厂装配点显式传递；
拓扑测试的三层场景直接覆盖"孙 report → 子 Mailbox → 子 report → 根"
链路的每次贯通。

### 4.3 批量 spawn 的部分语义澄清（记录）

- "全部或无"被请教后否决：一个坏 writable 不该强迫父放弃 N-1 个好子
  任务——**部分拒绝合法**，拒绝项逐项携带 Part 9.4 的措辞回传；
- 全部被拒 = `SPAWN_ALL_REJECTED`（拒绝清单 JSON 附 body）——父下一轮
  修正单个参数重派，成本只有一次 LLM 轮；
- 批量的 await 参数在**任何**裁决之前校验（批量半_started 后暴露策略
  非法，已跑起来的子资源无法收回）。

### 4.4 环境事故（诚实记录）

`go build ./...` 在仓库根生成 cmd 可执行文件（此前阶段报告已记载同类
行为；本轮把 `/marl` `/topo_test` `/discuss_loop` 加进 .gitignore——
三者的出现都是同因，补的是"系统性"面而不是打补丁。）

---

## 5. 交付物清单

| 文件 | 内容 |
| :-- | :-- |
| `internal/spawner/depth.go` (+test) | 深度闸独立成文件（Part 9.7 闸 1 的显式归位） |
| `internal/spawner/spawner.go` | ChildPlan.Mailbox/Task；Process.cancel/StartedAt/terminateReason；Terminate/Snapshot；runChild 的 watchdog 代报文本 |
| `internal/watchdog/{doc,watchdog}.go` (+test) | Watchdog（判据表：无进展标黄 / 超预算与超时强制终止）+ Table 适配 |
| `internal/agent/wait.go` | WaitStrategy（all/any/n，单次生效）+ 判据存取 |
| `internal/agent/mailbox.go` | reportArrival 信号 + waitReports（策略化等待） + agent_state 审计 |
| `internal/agent/spawn.go` | intentSpawnBatch（批量裁决/部分拒绝回填） |
| `internal/agent/intents.go` | spawn_batch schema（金面冻结序追加第 7 个意图） |
| `internal/ns/resolver.go` | bestMount 平局破法：能力序 max（§4.1） |
| `cmd/marl/status.go` | 色块 + 阻塞时长 + -color auto/always/never |
| `cmd/topo_test/main.go` | 三层拓扑 + Watchdog 演示 harness |
| `cmd/fork_test/main.go` | 子工厂补 Mailbox（多层兼容） |
| `docs/test_report/phase9-test-report.md` | 本报告 |

## 6. 已知限制与盲区（诚实清单）

1. **"不做"清单按 13.11 执行**：Supervisor 三决策（Resume / Restart /
   Giveup）不做——Watchdog 终止后只有"框架代报 failed"一条恢复路径。
2. **Watchdog 判据的口径**：超时/预算是全局常量（Config），不是
   profile/任务级（types.Budget 的 TokenUsed consumer 在 Spawner 的
   代报 payload 里有，判据侧尚未逐任务装接线——阶段 10 打磨）。
3. **无进展标黄只在审计事件里**（watchdog_no_progress）；`marl status`
   对黄色块的渲染还没消费该事件（它按 agent_state 走）——两处口径
   合并留给 daemon 化后的 status 直读进程表。
4. **spawn_batch 的策略记在 Agent 侧**（wait.go）而不是 SpawnRequest——
   "await 是父规划的一部分"的建模意味着如果阶段 10 的 ImportMessages
   复用它时要走 SpawnRequest 的显式字段。半途的取舍记录在 wait.go 头。
5. **未充分激活的能力维度**：并发 fork 的实际背压仍由 wire.Pool 的
   MaxInflight 承担（Part 9.8 的"深度优先队列"属阶段 10 打磨）；有 3 层
   并发孙在 `-race` 下全绿，但 Watchdog 终止 + 讨论 + fork 的**组合态**
   （父子同时 Blocked 不同原因）只有一个结构性论证——故障注入测试建议
   在阶段 10 的 Watchdog 端补上。
6. **Assumptions made**：
   - Watchdog 的 est-token 判据读 TotalTokens（Log 全量估算）——
     est ≠ 实际用量（与 Part 8.5 表中"Budget.MaxTokens"的口径差异
     暴露在配彦的 Config 注释里）。
   - Terminate 假设子 Agent 的 Run 对 ctx 取消是响应的（fake LLM 与
     wire 层都有 ctx 打断；真实场景依赖 HTTP 层超时——已被
     SamplingParams.TimeoutMs 兜住）。
