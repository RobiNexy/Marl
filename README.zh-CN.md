# Marl

**单二进制的多 Agent 任务运行器。** 给它一个任务，它 fork 出子 Agent
并行干活；每个 Agent 都被关在沙箱里；监督树实时可见；需要人拍板的事
它会停下来问你。

[English](README.md)

## 什么时候用

- 任务大到能拆（比如"实现求解器 + 界面 + 测试"），想让**多个 Agent
  并行**做、互不踩脚。
- 你想**随时插手**：Agent 通过文件跟你沟通——任何编辑器都能看能改，
  不需要额外界面。
- 你想**看得见成本**：每次 LLM 调用都有账可查、有钱可算。

## 30 秒上手

前置：PATH 里有 [fossil](https://fossil-scm.org)（2.20+），以及一个
DeepSeek API key。

```bash
marl init ~/my-task                      # 一次性：创建项目
cd ~/my-task
export DEEPSEEK_API_KEY=sk-...

marl start "读 README.md，用一句话总结"
# → 项目 Agent 开跑，打印过程；最终报告落到你的收件箱：
#   ~/.local/state/marl/my-task/inbox/report_*.md
```

它跑着的时候（开另一个终端）：

```bash
marl status                              # 实时树：谁在跑、谁被阻塞
marl say -to sub_000001 "总结用中文"      # 给运行中的 Agent 插话
marl log -db .marl/store.db              # 完整对话
```

跑完后，Agent 的报告就是收件箱里的一个文件，任何编辑器都能打开。

## 安装

```bash
# 从源码（Go 1.27+）：
git clone https://github.com/RobiNexy/Marl && cd Marl
go install ./cmd/marl

# 有正式版本后：
go install github.com/RobiNexy/Marl/cmd/marl@latest
```

检查环境：

```bash
marl doctor    # fossil 在不在？key 设没设？项目骨架健康吗？
marl version
```

## 背后只有三个想法

**1. 你是树的根。**
每个项目只有一个人类（你）。你 spawn 出 Agent，Agent 还可以继续往下
分——形成一棵树。底层出了问题，失败会沿树往上走，走到能处理的人为止
——树顶就是你。

**2. 任务会分叉。**
Agent 拆活儿时用 `spawn_batch` fork 出子 Agent 并行干；子还能再生孙。
每个子都拿到**显式的可写范围**（一组路径规则）——出了范围物理上写不进
去，不靠自觉。子干完（或挂了），父会收到报告；子悄悄死了，框架替它
上报，任务不会凭空消失。

**3. 它会来问你。**
贵或危险的操作会让 Agent 暂停，往你收件箱丢一个请求文件。你编辑文件
作答（比如把 `@grant once` 改成 `@grant next 20` = 批准接下来 20 次），
它就继续。讨论同理：Agent 提案，你批注或批准，结论以你的名义提交进
版本库。

一切可回看：完整对话记录（只追加、不删改）、审计事件、成本报表。
审批不可伪造——审批文件不在任何 Agent 的可写范围里。

## 命令速查

| 命令 | 干什么 |
| :-- | :-- |
| `marl init [dir]` | 创建项目（fossil 仓库 + `.marl/` 骨架） |
| `marl start "任务"` | 跑任务（前台；Ctrl-C 中断） |
| `marl start --detach "任务"` | 后台跑（日志在控制面） |
| `marl say -to <agent> "文本"` | 给运行中的 Agent 插话 |
| `marl status` | 实时监督树 + 阻塞/等待状态 |
| `marl stop [-force]` | 优雅停止后台任务 |
| `marl log -db <db> [-out file.md]` | 导出/打印对话 |
| `marl attach -db <db>` | 滚动 tail 新消息（2s 轮询） |
| `marl knowledge lint` | 检查常驻知识块的预算 |
| `marl knowledge promote/pull` | 跨项目共享知识（基于 fossil） |
| `marl models probe -models m1,m2` | 模型探活（对话/工具调用/缓存三判据） |
| `marl doctor` | 环境自检 |
| `marl version` | 构建信息 |

你的收件箱在 `~/.local/state/marl/<项目名>/inbox/`：

| 文件 | 含义 | 你的动作 |
| :-- | :-- | :-- |
| `report_*.md` | Agent 的完成报告 | 打开读 |
| `gate_*.md` | 审批请求 | 编辑 `@grant …` 那行，保存 |
| `direct_*.md` | 你发的插话 | ——（自动归档） |

审批写法：`@grant once`（只此一次）/ `@grant next 20`（接下来 20 次）/
`@grant tokens 50000`（追加 token 额度）/ `@always-grant`（永久放行，
落盘 `grants/`，重启不丢）/ `@deny`（拒绝）。

## 配置

全部配置在项目内的 `.marl/`（随项目进版本库）：

| 文件 | 管什么 |
| :-- | :-- |
| `.marl/config.yaml` | 预算上限（任务内调用次数/token/超时）、审批规则、sidecar 线路 |
| `.marl/profiles/*.yaml` | 每种 Agent 的角色：需要什么模型能力、采样参数（温度/最大输出/超时）、技能白名单、能不能 fork |
| `.marl/prompts/` | 提示词模板——改文件就是改那一步的行为，零代码 |
| `.marl/knowledge/` | Agent 该永远知道的事；`preferences/` 只有你能写，会被编译进每个 Agent 的固定提示 |

例：预算与审批（`.marl/config.yaml`）：

```yaml
project:
  max_depth: 3          # Agent 最多往下分几层（你这一层不算）
limits:
  llm_call:
    task_max_calls: 20  # 超过 → Agent 暂停来问你
    task_max_tokens: 100000
gate_rules:
  - id: review-shell
    match: {kind: shell}
    action: need_human
    reason: "默认只放行 go 工具链，其余命令要人批"
```

改完跑 `marl doctor` 早点发现笔误。YAML 解析刻意严格：未知键、Tab
缩进都是报错，不会被静默忽略。

## 常见问题

**收件箱在哪？** `~/.local/state/marl/<项目名>/inbox/`（可用
`XDG_STATE_HOME` 改根目录）。

**这次跑了多少钱？** 在 Marl 仓库里
`go run ./cmd/ladder_report -db .marl/store.db -task start-task`，
或直接查 `.marl/store.db` 的 `ledger_entries` 表。

**子任务挂了，全完了吗？** 不会。失败沿树上报（悄悄挂掉的由框架代报），
主 Agent 会调整或重试，完整历史都在日志里。

**Agent 一直 blocked？** 它在等你收件箱里的某个文件（或讨论裁决）。
等待期零消耗；等超过 24 小时 Watchdog 会发告警。

**我插话了它不理？** 插话在"轮与轮之间"生效——不会打断已开始的工作。

**密钥安全**：`DEEPSEEK_API_KEY` 走环境变量，不要写进任何文件提交。

## 给贡献者

设计文档、架构决策（ADR）、阶段测试报告在
[`docs/design/`](docs/design/) 与 [`docs/test_report/`](docs/test_report/)。
测试：`go test ./... -race`。
