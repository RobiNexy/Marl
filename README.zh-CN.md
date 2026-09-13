# Marl

**单二进制的多智能体任务运行器。** 给它一个任务，它会派生多个子 agent
并行工作，把每个 agent 关进沙箱，在终端仪表盘上实时展示监督树，遇到需
要人类决策的事情会停下来问你。

[English](README.md)

## 安装

一条命令（安装 `fossil` + `marl`；顺带可选装 `sqlite3` 命令行工具）：

```bash
curl -fsSL https://raw.githubusercontent.com/RobiNexy/Marl/main/scripts/install.sh | sh
```

脚本自动探测平台——Linux（apt/dnf/pacman/zypper 或 fossil 静态二进制）、
macOS（brew）、**Termux/Android**（pkg，arm64 产物）——带 sha256 校验，
装完自动跑 `marl doctor`。

从源码（Go 1.27+，无需 CGO）：

```bash
git clone https://github.com/RobiNexy/Marl && cd Marl
go install ./cmd/marl
```

依赖：PATH 里有 [fossil](https://fossil-scm.org) 2.20+，以及 DeepSeek
API 密钥（`DEEPSEEK_API_KEY`）。环境自检：

```bash
marl doctor
```

## 30 秒上手

```bash
marl init ~/my-task                       # 一次性：创建项目
cd ~/my-task
export DEEPSEEK_API_KEY=sk-...

marl tui                                  # 打开仪表盘
# 按 /start "读一下 README 并用一句话总结"
```

偏爱经典 CLI？`marl start "…"` 前台运行，`marl start --detach "…"`
后台运行。它们与 TUI 用的是同一个引擎。

## TUI：你的 AI 团队指挥台

`marl tui` 是终端仪表盘（bubbletea）。有守护进程就连守护进程，没有就
进程内直跑引擎——你不需要关心是哪种。

```
┌─────────────────────────────────────────────────────────────────────┐
│ Marl · 任务运行中 · 成本 ¥0.0428 CNY · 12.3k tok · 缓存 62% · [in-process] │
├──────────────┬──────────────────────────────────┬───────────────────┤
│ 监督树        │ 对话 · sub_000001                │ 成本与告警         │
│ 👤 human      │ 你                               │ ¥0.0428 CNY       │
│ └🤖 sub_001 ●│  读一下 README                   │ tokens 12.3k      │
│    ├🔧 sub_002│ sub_000001                      │ 思维链 18% 缓存 62%│
│    └🔧 sub_003│  好的，我来读取…                 │ r0 flash ¥0.006   │
├──────────────┴──────────────────────────────────┴───────────────────┤
│ ⚡ 待你决策 (1)：[gate] rm -rf build —— 按 a 处理                     │
├─────────────────────────────────────────────────────────────────────┤
│ 到 sub_000001 ▸ 发消息给选中 agent…                                  │
├─────────────────────────────────────────────────────────────────────┤
│ a 审批 · ↑↓ 选 agent · i 输入 · f 跟随 · q 退出(任务保持)            │
└─────────────────────────────────────────────────────────────────────┘
```

**键位**（`?` 可随时查看）：

| 键 | 作用 |
| :-- | :-- |
| `a` | **行动中心**——所有等你决策的事项 |
| `↑↓` / `Enter` | 树中选择 agent / 打开其对话 |
| `i` / `/` | 输入消息 / 斜杠命令（`Esc` 回到面板） |
| `Tab` `1/2/3` | 循环焦点 / 直达 树·工作区·侧栏 |
| `t` `f` `s` | 折叠思维链 · 自动跟随开关 · 收起侧栏 |
| `g t/e/i` | 工作区页签：对话 / 事件流 / 检视 |
| `q` | 退出（运行中的任务保持后台） |

**斜杠命令**：`/start <任务>` · `/stop [force]` · `/say <agent> <文本>` ·
`/events` `/inspect` `/cost` · `/help` · `/quit`。

**在 TUI 里审批**：按 `a`，选中待办，单键决策——`[o]` 放行一次 ·
`[n]` 放行接下来 N 次 · `[k]` 追加 token 额度 · `[a]` 永久放行 ·
`[d]` 拒绝。结构化回复写进的就是你手编的那份收件箱文件——并且**立即
生效**（附带"写完"声明；人手编辑保留 10 秒静默窗，防止读到改了一半的
答案）。TUI 没有特权后门：它只是一支更快的笔。

## 全生命周期

```bash
marl init ~/my-task        # 1. 建项目（fossil 仓库 + .marl/）
marl doctor                # 2. 检查 fossil / API key / 骨架
marl tui                   # 3. 打开仪表盘
#    /start "…"           # 4. 启动任务
#    看树在分叉；按 a 审批 gate
#    报告落到收件箱 + 完成提示
marl status                #    （同一棵树的 CLI 视图）
marl log -db .marl/store.db -out conv.md   # 5. 导出对话
```

**成本**。侧栏显示总成本、token、思维链占比、缓存命中率、按阶梯（模型
档位）的分项。引擎自带模型阶梯：先用便宜模型，有证据（连续工具格式
错误、无进展等）才升级。每次调用都记进 `.marl/store.db`
（`ledger_entries`）；报表也可用
`go run ./cmd/ladder_report -db .marl/store.db`。

**跨项目知识**。两级：

| 级别 | 位置 | 内容 |
| :-- | :-- | :-- |
| 项目级 | `.marl/knowledge/`（随仓库版本控制） | contracts、decisions、`preferences/`（编译进每个 agent 的固定前缀） |
| **全局级** | `~/.local/share/marl/global.fossil`（可用 `XDG_DATA_HOME` 覆盖） | 你提升上来、处处可复用的知识 |

```bash
marl knowledge lint                  # preferences 不得超常驻块预算
marl knowledge promote knowledge/decisions/api-style.md
marl knowledge pull                  # 把全局知识落进本项目 vendor/
```

**永久授权**也在控制面：`@always-grant` 会把规则写进
`~/.local/state/marl/<project>/grants/`，重启后仍生效。

## Marl 的三大核心思想

**1. 你是监督树的根。**
每个项目有一个人类（你）。你 spawn 的 agent 在你之下成树，任何 agent
都可以把工作继续下放。出错沿树向上回溯，直到能处理的人——树顶的你。

**2. 任务 fork 成并行子任务。**
拆工作的 agent 用 `spawn_batch` 派生子 agent，子还能再生子。每个子
都有明确的写作用域（路径 glob）——物理上写不出去。子完成或崩溃时父
收到报告；子静默死亡时框架代报。

**3. 停下来问你。**
昂贵或危险的操作会暂停 agent，把请求投进你的行动中心/收件箱。讨论
同理：agent 起草，你批注或通过，结论以你的名字落库。一切可观测：
只追加的对话日志、审计事件流、成本账本。agent 伪造不了审批——审批
文件在所有 agent 的写作用域之外。

## 版本控制：Marl 用 fossil，你的 git 不受影响

Marl 的记账面（知识、讨论草稿、agent 工作提交）放在 `marl init` 创建
的 `.marl/project.fossil`（fossil 仓库）里。**这是 Marl 唯一接触的
VCS——没有 git 集成，也没有移除 fossil 的计划。**

如果你的项目本身是 git 仓库，两者并存互不干扰：fossil 的数据都在
`.marl/` 里，从不读写 `.git/`。你照常向 git 提交；Marl 的 agent 提交
（author=`agent`）是平行的机器工作史，用 `fossil ui` 可审计。把它理解为
"Marl 留收据"，而不是"Marl 管你的分支"。

## 收件箱参考

收件箱在 `~/.local/state/marl/<project>/inbox/`（用 `XDG_STATE_HOME`
覆盖根路径）：

| 文件 | 含义 | 你的动作 |
| :-- | :-- | :-- |
| `report_*.md` | agent 的最终报告 | 读 |
| `gate_*.md` | 审批请求 | 编辑 `@grant …` 行保存（或在 TUI 里决策） |
| `direct_*.md` | 你发的消息（`marl say`） | ——（自动归档） |

escalation（agent 卡住向你求助）落在
`~/.local/state/marl/<project>/requests/pending/`；在 `## 回复` 下写
回复后把文件移到 `done/`——或者直接在 TUI 里回答。

`@grant once` / `@grant next 20` / `@grant tokens 50000` /
`@always-grant`（永久）/ `@deny`。

## CLI 参考

| 命令 | 作用 |
| :-- | :-- |
| `marl tui` | 终端仪表盘（监督树/审批/成本/对话） |
| `marl init [dir]` | 创建项目（fossil 仓库 + `.marl/` 骨架） |
| `marl start "任务"` | 运行任务（前台；Ctrl-C 中断） |
| `marl start --detach "任务"` | 后台运行（日志在控制面） |
| `marl say -to <agent> "文本"` | 给运行中的 agent 插话 |
| `marl status` | 监督树 + 阻塞/等待状态 |
| `marl stop [-force]` | 优雅停止后台任务 |
| `marl serve [-addr 127.0.0.1:8731]` | 本机守护进程：REST + SSE（GUI 接入口） |
| `marl log -db <db> [-out file.md]` | 导出/打印对话 |
| `marl attach -db <db>` | 实时 tail 新条目 |
| `marl knowledge lint` | 常驻知识块预算检查 |
| `marl knowledge promote/pull` | 跨项目知识共享（fossil） |
| `marl models probe -models m1,m2` | 模型探活（chat / tool_call / cache） |
| `marl doctor` | 环境自检 |
| `marl version` | 构建信息 |

## 配置

全部配置是 `.marl/` 下的项目级 YAML（随项目版本控制）：

| 文件 | 控制 |
| :-- | :-- |
| `.marl/config.yaml` | 预算限制（任务级调用数/token 上限/超时）、审批规则、sidecar |
| `.marl/profiles/*.yaml` | agent 角色：模型能力需求、采样参数、技能白名单、能否 fork |
| `.marl/prompts/` | 提示词模板——改文件即改行为，无需改代码 |
| `.marl/knowledge/` | agent 必须知道的事实；`preferences/` 由你编写并编译进每个 agent 的固定前缀 |

示例——`.marl/config.yaml` 里的预算与审批：

```yaml
project:
  max_depth: 3          # agent 可 fork 的深度（人类层不计）
limits:
  llm_call:
    task_max_calls: 20  # 超过 → agent 暂停问你
    task_max_tokens: 100000
gate_rules:
  - id: review-shell
    match: {kind: shell}
    action: need_human
    reason: "默认只放行 go 工具链"
```

YAML 解析器刻意严格：未知键和 Tab 缩进都是错误，不是被静默忽略的笔误。

## 程序化对接（Web UI / 工具）

`marl serve` 以单项目为范围跑本机服务——TUI/GUI 的每个操作都是一个
HTTP 调用。默认只绑回环。

```bash
DEEPSEEK_API_KEY=sk-... marl serve -dir ~/my-task -addr 127.0.0.1:8731
```

| 端点 | 作用 |
| :-- | :-- |
| `GET /api/v1/status` | 项目、人类、运行态、agent 树 |
| `POST /api/v1/tasks` `{"task": …}` | 启动任务（运行中 409） |
| `POST /api/v1/tasks/stop` | 停止当前任务 |
| `POST /api/v1/agents/{id}/messages` `{"text": …}` | 给运行中的 agent 插话 |
| `GET /api/v1/inbox` · `GET /api/v1/inbox/{name}` | 列出/读取收件箱 |
| `POST /api/v1/inbox/gate/{id}` `{"action":"allow","mode":"count","count":20}` | 审批答复 |
| `GET /api/v1/escalations` · `POST /api/v1/escalations/{id}/reply` | 列出/回复求助 |
| `GET /api/v1/discussions` · `POST /api/v1/discussions/{id}/verdict` | 列出/回复讨论 |
| `GET /api/v1/events?since=<seq>` | SSE 事件流（审计背书，可断线续传） |
| `GET /api/v1/conversation?agent=…` · `/conversation/export` | 对话日志 / markdown 导出 |
| `GET /api/v1/costs` | 成本报表（token、缓存命中、CNY） |
| `GET/PUT /api/v1/config` · `GET/PUT /api/v1/profiles/{id}` | 带校验的配置编辑（422 拒绝错误） |
| `GET /api/v1/doctor` · `GET /api/v1/knowledge/lint` · `POST /api/v1/knowledge/promote|pull` | 诊断与知识 |

API 的审批写的也是人类手编的那份收件箱文件——同一机制，没有特权后门。

## FAQ / 排障

**系统全局的配置在哪？** 按用途分三处：全局*知识*在
`~/.local/share/marl/global.fossil`（感知 `XDG_DATA_HOME`）；项目控制面
（收件箱、`grants/`、讨论）在 `~/.local/state/marl/<project>/`；项目
设置在仓库内的 `.marl/`。

**agent 会拿 fossil 管我的代码吗？** Marl 把 agent 的工作提交写进
`.marl/project.fossil`（它自己的收据）。你的 git 仓库完全不被碰——两者
并存。见上文专节。

**一次任务花了多少钱？** TUI 侧栏，或
`go run ./cmd/ladder_report -db .marl/store.db -task start-task`，或直接
查 `.marl/store.db`（`ledger_entries`）。

**子任务失败了，全完了吗？** 没有。失败沿树上报，根 agent 适应或重试，
完整历史都在日志里。

**agent 卡在 blocked。** 它在等你的一份文件（审批/讨论/求助）。阻塞期
不烧钱；watchdog 会在停摆时告警。

**我的插话没有打断它。** 插话在轮与轮之间注入——从不打断进行中的工作。

**密钥：** export `DEEPSEEK_API_KEY`；绝不提交。

## 给贡献者

设计文档、架构决策（ADR）、阶段测试报告在
[`docs/design/`](docs/design/) 与
[`docs/test_report/`](docs/test_report/)。测试：`go test ./... -race`。
CI 构建 linux/darwin/windows/android × amd64/arm64；release 附 sha256
校验和与安装脚本。
