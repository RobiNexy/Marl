# Marl 阶段 6 测试报告（Fossil 集成与单写者提交）＋ 定价修正

**日期**：2026-09-12
**范围**：设计文档 13.8（阶段 6）全部交付物 + 阶段 4 的定价修正（用户指出的
问题：定价应分为输入价格、输出价格、输入命中缓存的价格）。
**性质**：全量测试——单元测试（-race，真实 fossil 二进制）+ 编译后可执行
软件的端到端功能测试（marl init / fork_test 真跑 + fossil timeline/diff 验证）。

---

## 1. 结论

**通过**。13.8 的交付判据全部满足：

| 交付判据（13.8） | 结果 | 证据 |
| :-- | :-- | :-- |
| `marl init` 能跑通 | ✅ | 临时目录 init：骨架 7 个文件入库、timeline 有 author=system 的首次 commit |
| `fossil timeline` 能看到初始 commit | ✅ | `marl init：项目骨架与 ignore-glob (user: system)` |
| Agent 任务完成后 timeline 多一条 commit | ✅ | 真机 fork_test：`marl[fork-task] 任务完成提交 … (user: agent)` |
| `fossil diff` 能看到子写的文件 | ✅ | 子写 `src/auth/summary.txt` → 父 commit → `fossil ls` 含该文件；commit 后 diff 为空（全部已入库，语义正确） |
| 单写者提交避免并发冲突 | ✅ | M6.5：5 子并发 report → 父 commit 不丢文件（§2.4） |

**定价修正**（ADR-0029）：ladder.yaml 新增 `pricing:` 节（按 model 四项分价），
代码内占位价格表删除。

---

## 2. 静态检查与单元测试

```bash
go build ./... && go vet ./...   # 通过
go test ./... -race -count=1     # 全部通过（16 个包）
```

### 2.1 阶段 6 新增的测试矩阵

| 包 | 测试函数数 | 覆盖要点 |
| :-- | --: | :-- |
| **internal/fossil** | 6 | InitRepo/OpenRepo 幂等与重复拒绝、**author 三用户**、nothing-to-commit 哨兵、ErrNotOpen、Status(changes+extra)/Diff、**ignore-glob 生效**（.marl 不入库）、并发 commit 语义（§4.3） |
| **cmd/marl** | 2 | init 端到端（timeline/骨架入库/二次拒绝）、用户定制不被覆盖 |
| **internal/agent** | +2 | TestCommitAfterChildReports（fork→写→report→commit→timeline author=agent）、TestM6_5ConcurrentReports（5 子并发 → commit 不丢文件） |
| **internal/ladder** | +1 组 | pricing 节解析（缺价/ cached>in / 币种不一致 / 排序价越界 五类拒绝） |

### 2.2 定价修正的验证

- `TestCostFormula`：四项公式（in/cached_in/out/reasoning）逐项断言；
- `TestRecorderRecordsCost`：计价来自 catalog（缺价拒绝记账，不静默记 0）；
- dry-run 升级场景复跑：报表数字与 ladder.yaml 的 pricing 一致
  （r0=2 调用 0.0112 CNY、r1=1 调用 0.0056 CNY，按 in=1.0/cached=0.25/out=2.0 算）。

### 2.3 fossil 实测裁决的测试固化（ADR-0030 的每条都有对应测试）

| 裁决 | 测试 |
| :-- | :-- |
| author 三用户 + user new 注册 | TestCommitAuthorAndTimeline |
| --no-verify-comment（wiki 误报） | TestCommitAfterChildReports（中文+括号 message 通过） |
| mtime-changes off | TestStatusAndDiff（commit 后立即改文件，changes 立即可见） |
| Status = changes + extra | TestStatusAndDiff（新文件 EXTRA） |
| add 是 checkout 级暂存区 | TestConcurrentCommits |
| 不做 lockfile 退避 | TestConcurrentCommits（无重试逻辑可测；并发路径无退避代码） |

### 2.4 M6.5（13.14 非功能里程碑）：单写者提交的并发验证

`TestM6_5ConcurrentReports`：父 spawn 5 个子（每子写 `src/auth/out_X.txt`），
5 个子并发写文件、并发投递 report（mailbox pump 并发消费）→ 父收齐 →
一次 commit → 断言：Run 无错误（无 lockfile 冲突）、5 个文件全部存在、
父 Log 有 5 条 sub_task_result、timeline 有 author=agent 的 commit。
`-race` × 5 稳定通过。

---

## 3. 编译后软件的功能测试（端到端）

### 3.1 命令矩阵

| 命令 | 结果 | 说明 |
| :-- | :-- | :-- |
| `marl init -dir <tmp>` | ✅ | 骨架入库、首次 commit author=system |
| `marl init -dir <已初始化>` | ✅ 拒绝 | 重复 init 会覆盖历史，不是可重试操作 |
| `fork_test`（真机，子任务=写文件） | ✅ | 全链路：list_dir → spawn → 子 file_read+file_write+report → 父 commit |
| `fork_test -keep` 后人工 `fossil ls/diff` | ✅ | 子写的 summary.txt 已入库；commit 后 diff 为空（正确） |
| `mini -task failing -dry-run -ladder` | ✅ | 定价修正后报表数字与配置一致 |

### 3.2 真机输出摘录（fork_test，DeepSeek，2026-09-12）

```text
━━━━━━━━━━ Fossil Timeline ━━━━━━━━━━
09:19:11 [49b7e4e1] marl[fork-task] 任务完成提交 sub_000001: success (Task complete.) (user: agent)
09:19:04 [53d4c13c] initial empty check-in (user: samphi)
```

人工验证（`-keep` 保留工作区）：

```text
$ fossil ls
src/auth/oauth.go
src/auth/summary.txt   ← 子 Agent 写的文件，已入库
src/main.go
src/util/util.go
$ fossil diff          ← 空：工作区与 checkout 一致（全部已提交）
```

---

## 4. 真机/实测缺陷与修复

### 4.1 mtime-changes：commit 后立即改文件被漏检（严重）

**现象**：fossil 包测试 `TestStatusAndDiff` 稳定失败——commit 后立即改文件，
`fossil changes` 返回空。
**归因**（手工探测）：fossil 默认按 **mtime** 判定文件是否变更
（`mtime-changes` 设置），commit 记录的文件 mtime 与"commit 后立即写入"
的新 mtime 落在同一粒度内 → 漏检。单 Agent 串行流程（写→隔几秒→commit）
从未触发；测试的紧凑时序第一次暴露。
**修复**：InitRepo 统一设置 `fossil settings mtime-changes off`（仓库级，
内容校验和判定）。修复后测试稳定，且真机"子写文件→父立即 commit"路径
（fork_test）不再依赖时序运气。

### 4.2 fossil commit 的 wiki 格式检查误报

**现象**：集成测试的 commit message（含子 report 文本片段）被
"Possible format errors in the check-in comment" 拒绝。
**归因**：fossil 默认对 comment 做 fossil-wiki 检查，`<tag>`/`&`/`[..]`/
`_.._` 都是触发词；机器生成的摘要含这些字符是常态。
**修复**：Commit 恒带 `--no-verify-comment`（ADR-0030 第 2 条）。

### 4.3 fossil add 的 dotfiles 默认跳过

**现象**：marl init 的知识目录占位文件 `.keep` 未入库。
**归因**：`fossil add` 默认跳过 `.` 开头文件（实测 `fossil help add`）。
**修复**：占位文件改用 README.md（语义也更清晰）。

### 4.4 并发 commit 的语义澄清（非缺陷，实测记录）

`TestConcurrentCommits` 首版假设"两个 goroutine 各自 add+commit 各得一个
commit"——错。fossil 的 add 是 **checkout 级暂存区**：先跑的 commit 带走
全部已暂存文件，后跑的 commit 报 nothing-to-commit。测试改为钉死这个语义
（串行化不炸、文件最终都入库、ErrNothingToCommit 是正确结果而非故障）。
单写者模型下框架内不会走到这条路径；框架外并发（人类手动 commit）由
writeMu 串行化。

### 4.5 环境事故（诚实记录）

开发过程中一次 `go run ./cmd/marl init`（参数解析缺陷导致 -dir 未生效）
在**项目仓库根**误建了 fossil checkout（.marl/ + .fslckout）。已彻底清理
（fossil close + 删除），并把 `.fslckout`/`.fossil-settings/` 补进
.gitignore。参数解析缺陷已修（Go flag 不支持子命令形态，init 显式剥离
"init" 位置参数）。

---

## 5. 交付物清单

| 文件 | 内容 |
| :-- | :-- |
| `internal/fossil/{cli,lifecycle,workspace}.go` (+test) | CLI 封装/生命周期/工作区操作 |
| `internal/agent/commit.go` (+commit_test.go) | 单写者提交 + 集成测试 + M6.5 |
| `cmd/marl/init.go` (+init_test.go) | marl init 命令 |
| `cmd/fork_test/main.go` | fossil 集成 + -keep + 子写文件任务 |
| `internal/ladder/{ladder,router}.go` (+test) | **定价修正**：pricing 节加载/StaticCatalog 消费 |
| `internal/types/binding.go` | Rung.CostPerMTok 语义修订（排序粗价 + 可行区间校验） |
| `cmd/mini/main.go` | 占位价格表删除（计价单一来源 = ladder.yaml） |
| `ladder-mini.yaml` | pricing 节（四项分价 + currency） |
| `.gitignore` | .fslckout / .fossil-settings/ |
| `docs/design/decisions.md` | ADR-0029（定价）/ ADR-0030（fossil 六项裁决） |
| `docs/design/marl_design.md` | 13.8 状态与里程碑表更新 |

## 6. 已知限制与盲区（诚实清单）

1. **"不做"清单按 13.8 执行**：branch / merge（分支留给讨论与显式实验）、
   讨论分支、server 常驻。
2. **runRead 的 TTL 缓存未做**（13.8 的描述）：本地 fossil 命令毫秒级
   （实测 timeline/status 均 <100ms），缓存是无谓复杂度——13.8 写作
   "带 TTL 缓存"的动机（fossil server 远端场景）在单机部署下不存在。
   若未来接远端仓库再补。
3. **fork_test 的工作区自清理**：默认任务结束即删（demo 工具的干净退出），
   人工验证 fossil 时需 `-keep`——已实测并记录在命令帮助里。
4. **子 Agent 的 writable 范围与 add 的信任域**：doCommit 的 Add 目标来自
   `fossil changes`（工作区全部改动），不区分"谁写的"——单写者模型下
   这是**设计**（父在 commit 前有机会拦截，Part 8.4 第 3 条），但阶段 6
   未实现"父审阅后丢弃某子产出"的路径（它属于阶段 9 的 Supervisor）。
5. **未充分激活的能力维度**：fossil 的并发写语义只验证了双 goroutine；
   真实多 Agent（阶段 9 的多子并行 + Watchdog 强杀）下的 checkout 锁竞争
   留待阶段 9 的并发验证。
