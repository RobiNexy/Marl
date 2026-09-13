# Marl TUI 设计文档

> 状态：**已实现**（`internal/frontend/gateway` + `internal/tui` + `marl tui` 子命令；设计偏差与落地说明见文末"实现注记"）
> 目标：用 bubbletea 实现一个多面板仪表盘式 TUI，交互体验对标并超越 Claude Code，充分展现 Marl 的四大精髓。
> 读者：实现者、评审者。本文档只做设计，实现代码见 internal/tui。

---

## 0. 设计北极星（North Star）

Claude Code 是**单 agent、单对话流**的助手。Marl 的本质完全不同——它是**人类作为监督树根的多智能体并行编排框架**。因此 TUI 不能只是"抄一个对话框"，而必须让下面四条精髓在屏幕上**一眼可见、可操作**：

| # | Marl 精髓 | 在 TUI 上如何体现 |
|---|---|---|
| 1 | **你（人类）是监督树的根** | 树视图永远把 `👤 human` 画在顶端；所有审批/讨论/求助都以"待你决策"的形式聚合到一个 **Action Center**（行动中心）。 |
| 2 | **任务 fork 成并行子任务，各有写作用域** | 左侧 **Agent 树**实时展示派生拓扑、每个节点的状态色块、写作用域；并行 agent 同屏可见。 |
| 3 | **贵/危险操作会停下问你** | 审批（gate）是**一等公民**：以醒目卡片 + 快捷键内联决策，而非埋在日志里。附静默窗延迟的诚实反馈。 |
| 4 | **成本可见 + 不可篡改审计** | 右侧常驻**成本仪表盘**（阶梯/缓存命中/思维链占比）；独立**事件流**面板消费不可变 audit_events。 |

**一句话定位**：Claude Code 让你和一个 AI 对话；Marl TUI 让你**指挥一支 AI 团队**，并对每一分钱、每一个危险动作保持完全掌控。

---

## 1. 架构决策：分层，面向多前端复用

用户明确未来还要做 **Web UI** 和**原生 GUI**。项目本身已经为此准备好了地基——`internal/contract` 包的 `Interaction` 接口是"宿主与 Marl 交互的唯一契约"，已有三个传输实现（进程内 `App` / HTTP+SSE `HTTPClient` / 文件兜底 `FileMailbox`）。

因此 TUI 的架构 = **契约驱动 + 单向数据流（Elm/TEA）**，分四层：

```
┌─────────────────────────────────────────────────────────────┐
│  L4  View 层（bubbletea 组件）                                 │
│      纯渲染 + 键盘/鼠标事件 → 派发 Intent（不含业务逻辑）      │
├─────────────────────────────────────────────────────────────┤
│  L3  Store 层（前端状态容器，TUI 私有）                        │
│      单一 AppState + reducer；持有各面板的派生视图模型         │
│      订阅 EventBus，把领域事件折算成 UI 状态                   │
├─────────────────────────────────────────────────────────────┤
│  L2  Gateway 层（前端无关，可被 Web/GUI 复用★）                │
│      封装 contract.Interaction：命令方法 + 统一事件流(EventBus)│
│      负责：SSE/游标增量、去重续传、乐观更新、错误归一          │
├─────────────────────────────────────────────────────────────┤
│  L1  contract.Interaction（已存在，项目自带）                  │
│      App(进程内) / HTTPClient(HTTP+SSE) / FileMailbox(文件)    │
└─────────────────────────────────────────────────────────────┘
```

### 1.1 关键：L2 Gateway 是可复用资产

L2（建议放 `internal/frontend/gateway`，与 TUI 解耦）对上暴露一套**与 UI 框架无关**的接口：

```
type Gateway interface {
    // 命令（写）——返回后立即乐观更新，真结果经事件回流
    StartTask(task string) error
    StopTask(force bool) error
    SendMessage(to, text string) error
    DecideGate(id string, d contract.GateDecision) error
    ReplyDiscussion(id, note string, approve bool) error
    ReplyEscalation(id, text string) error   // ★契约空白点，见 §9

    // 查询（读，快照）
    Snapshot() FrontendState                 // 全量派生状态

    // 事件（订阅）——UI 框架无关的领域事件
    Subscribe() <-chan DomainEvent
    Close() error
}
```

- **Web UI 复用**：把 `Gateway` 的命令映射成 WebSocket/HTTP，`DomainEvent` 推给浏览器。
- **原生 GUI 复用**：直接嵌入进程，`Subscribe()` 喂给 GUI 的观察者。
- **TUI**：`DomainEvent` → `tea.Msg`，`FrontendState` → 各面板 model。

> 这一层的价值：**领域→UI 的折算只写一次**。三个前端共享"什么是一条 gate 请求、成本怎么汇总、树怎么建"。UI 差异只在渲染。

### 1.2 连接模式自动探测（复用 CLI 逻辑）

Gateway 构造时复用 `cmd/marl/start.go` 的 `resolveDaemon` 思路：

1. 读控制面 `serve.lock` → 若 daemon 在跑 → 用 `HTTPClient`（HTTP + SSE 长连，可断线续传）。
2. 否则 → 进程内 `NewApp()`（等价"带界面的 `marl start`"，单进程零延迟）。
3. 两种模式对 L3/L4 **完全透明**——这正是契约层的意义。

### 1.3 事件获取策略（增量 + 续传）

事件流是整个 TUI 的心跳。统一走 **`Seq` 游标增量**（`audit_events` 全局单调递增，`ORDER BY id ASC` 保证与 CLI 同源同序）：

- **HTTP 模式**：优先 SSE `GET /api/v1/events?since=<seq>`（帧 `id:<seq>\ndata:<json>`，700ms 粒度，15s 心跳）。断线后用最后 `id` 作 `since` 续传。
- **进程内模式**：起 goroutine 以 ~300ms 轮询 `App.Events(ctx, since, limit)`，同样用 `Seq` 游标。
- **去重**：Store 层维护 `lastSeq`，丢弃 `Seq <= lastSeq` 的重复帧。

> ⚠️ JSON 键大小写陷阱：`AuditEvent` 与 `types.LogEntry` **无 struct tag**（键为大写字段名 `Seq/AgentID/Action/...`）；而 `contract` 的 DTO（`AgentView`/`RunStatus`/`GateDecision`...）是 snake_case。Gateway 解析时必须区别对待，并封装成前端统一的 camelCase/结构体，隔离掉这个陷阱。

---

## 2. 信息架构（Information Architecture）

### 2.1 顶层布局：三栏仪表盘

```
┌──────────────────────────────────────────────────────────────────────────┐
│ TopBar：项目名 · 任务态(running/idle) · 当前阶梯 · 累计成本 · 连接模式    │  ← 1 行
├────────────────┬───────────────────────────────────────┬──────────────────┤
│                │                                       │                  │
│  ① Agent 树    │         ② 主工作区（Tab 切换）         │  ③ 侧栏(可折叠)  │
│  (监督树)      │   ┌─────────────────────────────────┐ │                  │
│                │   │ Conversation │ Events │ Inspect │ │  · 成本仪表盘    │
│  👤 human      │   ├─────────────────────────────────┤ │  · 阶梯/证据     │
│  └🤖 sub_001 ● │   │                                 │ │  · Watchdog 告警 │
│    ├🔧 sub_002 │   │   （随选中节点/Tab 变化的内容）  │ │                  │
│    └🔧 sub_003◐│   │                                 │ │  （随上下文变化）│
│                │   │                                 │ │                  │
│                │   └─────────────────────────────────┘ │                  │
├────────────────┴───────────────────────────────────────┴──────────────────┤
│ ⚡ Action Center：待你决策 (2)  ▸ gate: rm -rf build   ▸ discuss: 契约边界  │  ← 醒目条(有待办时)
├────────────────────────────────────────────────────────────────────────────┤
│ InputBar：> 输入消息 / 斜杠命令…                          [到 sub_001 ▾]     │  ← 1-3 行
├────────────────────────────────────────────────────────────────────────────┤
│ StatusLine：F1 帮助 · Tab 切面板 · a 审批 · / 命令 · 静默窗提示…            │  ← 1 行
└────────────────────────────────────────────────────────────────────────────┘
```

- **① Agent 树**：永远可见，是"指挥全局"的锚。选中节点驱动 ② 和 ③ 的上下文。
- **② 主工作区**：Tab 页——`Conversation`（对话）/ `Events`（事件流）/ `Inspect`（信封/report 检视）。
- **③ 侧栏**：成本 + 阶梯 + watchdog 告警，随选中 agent 聚焦。窄屏可折叠。
- **⚡ Action Center**：**Marl 的灵魂**。只要有"待人类决策"的事项（gate/discussion/escalation），这条就高亮出现，是全局最高优先级。无待办时隐藏。
- **InputBar**：底部持久输入（对标 Claude Code），带"发送目标 agent"选择器 + 斜杠命令。

### 2.2 响应式断点（终端宽度自适应）

| 宽度 | 布局 |
|---|---|
| ≥ 120 列 | 完整三栏（树 + 工作区 + 侧栏） |
| 80–119 列 | 两栏（树 + 工作区），侧栏折叠为可 `s` 键唤出的抽屉 |
| < 80 列 | 单栏：默认工作区，树/侧栏均为全屏切换（`g t` / `g s`），退化为"Claude Code 式单栏 + 抽屉"（正好覆盖你选项里说的兜底） |

用 `tea.WindowSizeMsg` 驱动重新计算各区尺寸；lipgloss 负责盒模型。

---

## 3. 各视图（Panel）详细设计

### 3.1 ① Agent 树（监督树）

**数据源**：`Agents()` → `[]AgentView{ID,Parent,Kind,Depth,State,PendingKind,PendingAt,StartedAt}`（daemon 在跑时为实时进程表快照，无审计滞后）。事件流的 `spawn`/`agent_state`/`watchdog_*` 触发增量刷新。

**渲染规格**：
```
👤 human:1000                          depth0
└─ 🤖 sub_000001  ● running    3m20s   ← 绿点=running
   ├─ 🔧 sub_000002  ◐ blocked  3h08m   ← 黄=blocked，副行显示原因
   │     └ ⏳ discussing「契约边界」
   ├─ 🔧 sub_000003  ⏸ awaiting_gate    ← 挂起，Action Center 有对应项
   └─ 🔧 sub_000004  ✕ crashed  (超预算) ← 红=crashed/terminated
```

- **图标（Kind/Depth）**：`👤` human(depth0) · `🤖` root agent(depth1) · `🔧` 子 agent(depth>1)。
- **状态色块**（对齐 `status.go` 语义）：绿 `running/idle` · 黄 `blocked` · 红 `crashed`。
- **阻塞副行**：`BlockReason`/`PendingKind` 二级展示：`wait_children / compressing / discussing / escalating / awaiting_gate`。挂起超阈值（watchdog `pending_overrun`）加 `⚠` 计时。
- **写作用域**：选中节点时，侧栏或 tooltip 显示该 agent 的 `Writable` glob（体现精髓 2 的"沙箱边界"）。
- **交互**：↑/↓ 移动选择；→/← 展开折叠；`Enter` 聚焦到该 agent（② 切到其对话，③ 切到其成本）；`m` 直接给该 agent 发消息（预填 InputBar 目标）。

**动效**：新 spawn 的节点淡入 + 短暂高亮；状态变色带 200ms 过渡感（用符号闪一下即可，终端不做真动画）。

### 3.2 ② 主工作区 · Tab: Conversation（对话）

**这是对标 Claude Code 的核心，但要更强。**

**数据源**：`Conversation(agentID)` → `[]LogEntry{Role,Content,Seq,TokenActual,CreatedAt,...}`；实时增量来自事件流 + 轮询 tail（**修复 CLI `marl attach` 未实现 tail 的缺陷**，见 §9）。

**按 Role 渲染气泡**（`InternalRole` 枚举）：

| Role | 显示 | 样式 |
|---|---|---|
| `user_input` | You | 右对齐/主色 |
| `assistant_reply` | agent id | 左对齐 |
| `thinking` | 💭 thinking | 暗色、**默认折叠**（`t` 展开），标注思维链 token |
| `tool_result` | 🔧 tool | 折叠块，显示技能名 + 结果摘要，`Enter` 展开全文 |
| `sub_task_result` | 📦 子任务回报 | 卡片：status 色 + FilesChanged + Blockers |
| `escalation` | 🆘 求助 | 高亮，链接到 Action Center |
| `human_note` | ✍ 你的插话 | 区别于 user_input（异步注入语义） |
| `constraint`/`shared_memory` | 系统 | 折叠、暗色 |

**超越 Claude Code 的点**：
1. **多 agent 对话可并置**：选中父节点时可选"聚合视图"，把子 agent 的关键 report 内联进父对话流（Claude Code 没有多 agent 概念）。
2. **每条消息带成本徽章**：`TokenActual` → 悬浮显示这轮花了多少 token / 是否命中缓存（精髓 4）。
3. **思维链独立折叠 + 占比提示**：DeepSeek 思维链单独计费，默认折叠但显示"本轮思维 1.2k tok"。
4. **流式感**：assistant 逐条 append，滚动跟随（可 `f` 锁定/解锁自动跟随，对标 Claude Code 的滚动行为但更明确）。

**交互**：PgUp/PgDn 滚动；`t` 折叠/展开 thinking；`Enter` 展开当前 tool block；`y` 复制选中块；`f` 切换自动跟随。

### 3.3 ② 主工作区 · Tab: Events（事件流）

**精髓 4 的直接体现**：不可篡改审计的实时流。

**数据源**：`Events(since, limit)` → `[]AuditEvent{Seq,AgentID,Timestamp,Action,Target,Payload}`。

**渲染**：单行紧凑 + 可展开 payload：
```
#1042 10:31:02 sub_002  gate_decision   shell    allow(once) by human:1000
#1043 10:31:05 watchdog watchdog_no_progress  sub_003  quiet=95s
#1044 10:31:20 sub_001  model_upgrade   r0→r1    reason=tool_format_errors
```

- **Action 是开放集合**：不硬编码枚举。已知 action 给专属图标/颜色（gate=🔐, spawn=🌱, watchdog=🐕, model_upgrade=⬆, discussion=💬, escalation=🆘, commit=📌），**未知 action 用兜底样式**（灰色 + 原样 Action 字符串）。
- **过滤器**：`/` 按 action 或 agent 过滤（复用 `AuditFilter` 语义：AgentID/Action/时间）。
- **游标显示**：底部显示 `at #1044 (live)`，断线续传时显示 `reconnecting… resume from #1044`。
- **交互**：`Enter` 展开 payload（格式化 JSON）；`c` 复制 Seq/事件；`L` 定位到该事件对应的 agent 树节点。

### 3.4 ② 主工作区 · Tab: Inspect（检视器）

深度检视选中节点的结构化信息（Web/GUI 也会用）：
- **信封检视**（proto.Envelope）：From→To、Type 徽章、TraceID 追踪链、Payload。
- **子 report**（ChildReport）：Status/FilesChanged/Blockers/TokenUsed/时长。
- **绑定与阶梯**（Binding）：当前 RungID/Model/Endpoint/Thinking/Degraded 软约束。
- **升级证据**（UpgradeEvidence）：ConsecutiveFailures / ToolFormatErrors / NoProgressRounds / ... + 当前 Score vs Threshold 进度条。

### 3.5 ③ 侧栏 · 成本仪表盘

**精髓 4**。数据源：`Costs(taskID)` → `TaskCostSummary`。

```
成本  ¥0.0428 CNY          ← 总成本（醒目）
────────────────────────
Tokens  12,340  (思维 18%) ← 思维链占比
缓存命中  62%   ✓          ← <40% 时黄色告警
────────────────────────
阶梯分项
  r0 flash    8 调用  ¥0.006
  r1 flash+   3 调用  ¥0.012
  r2 v4-pro   1 调用  ¥0.025 ⬆
────────────────────────
类别
  main        ¥0.031
  orchestration ¥0.004
  discussion  ¥0.008
```

- 缓存命中率 <40% / 思维链 >60% → 采纳 `ledger.Render` 的建议逻辑，显示黄色提示（如"缓存命中偏低，检查前缀稳定性"）。
- 选中某 agent 时可切"全局 / 该 agent"视图（ledger 表有 agent_id 列可聚合）。

### 3.6 ③ 侧栏 · 阶梯 & Watchdog

- **阶梯全景**：画出 r0→r1→r2 三级，高亮当前级，标出便宜→贵。升级历史时间线（model_upgrade/model_switch 事件：from→to + reason）。
- **Watchdog 告警区**：从事件流聚合 `watchdog_no_progress`（黄徽章 + quiet 秒数）/ `watchdog_terminated`（红徽章 + reason）/ `pending_overrun`（挂起停摆计时）。这是"框架在替你盯着"的可视化。

---

## 4. ⚡ Action Center（行动中心）——最重要的差异化设计

Marl 的精髓 1&3 要求"人类决策"绝不能埋没在滚动的日志里。Action Center 把**所有等待你的事项**聚合成一个持久、醒目、可键盘直达的队列。

### 4.1 三类待办统一模型

| 类型 | 来源 | 决策动作 |
|---|---|---|
| **Gate 审批** | `Inbox()` 里 `type=gate` 的项 / 事件 `gate_pending` | allow(once/count/tokens/always) / deny + 理由 |
| **Discussion 讨论** | `Discussions()` | approve（落地草稿）/ 批注（注入 agent）/ reject |
| **Escalation 求助** | `<control>/requests/pending/`（★契约空白，§9 补齐） | 写回复 |

### 4.2 Gate 审批卡片（核心交互）

选中 Action Center 里的 gate 项，弹出**审批卡片**（模态覆盖工作区）：

```
┌─ 审批请求 · shell ────────────────── sub_000002 请求 ─┐
│ 规则 review-shell：命令不在白名单                       │
│                                                        │
│ $ rm -rf build                                         │
│ 属性：command_prefix=rm                                │
│                                                        │
│ 批注（可选）：____________________________________      │
│                                                        │
│  [o] 放行一次   [n] 接下来N次   [k] 追加token额度       │
│  [a] 永久放行   [d] 拒绝        [Esc] 稍后              │
└────────────────────────────────────────────────────────┘
```

- 映射到 `contract.GateDecision{Action,Mode,Count,Tokens,Reason}` → `ReplyGate(id, d)`。
- **诚实的静默窗反馈（关键 UX）**：提交后**不谎报"已生效"**。因为文件通道有类型化静默窗（gate 约 10s）。显示：
  ```
  ✓ 审批已提交，等待生效（文件通道静默窗 ~10s）…
  ⟳ 生效中… → ✓ 已生效（收到 gate_decision 事件后确认）
  ```
  用事件流里对应的 `gate_decision` 事件作为"真正生效"的确认信号（乐观 UI + 最终一致）。
- **权限诚实**：文案明确"TUI 只是你的笔，写的是同一个收件箱文件，没有后门特权"——呼应原则 4。

### 4.3 全局审批快捷键

- Action Center 有待办时，StatusLine 常驻提示 `a 审批 (2)`。
- `a` → 跳到 Action Center 第一项；数字键 `1/2/3` 直达对应待办。
- 审批卡片里单键决策（`o/n/k/a/d`），最快两次按键完成一次放行——**比手编文件快一个数量级**，也比 Claude Code 的 yes/no 更表达丰富（额度/永久）。

---

## 5. 输入与命令系统

### 5.1 InputBar（底部持久输入，对标 Claude Code）

- **自由文本**：发送给"当前目标 agent"（默认选中的树节点；右侧 `[到 sub_001 ▾]` 可切）。走 `SendMessage(to, text)` → 作为 `human_note` 异步注入 agent 下一轮。
- **发送语义诚实**：提示"消息将在该 agent 下一轮编排被看到（不打断、不唤醒）"——对齐 MsgDirect 的异步语义，避免用户误以为是实时中断。
- 多行输入（`Alt+Enter` 换行，`Enter` 发送）。

### 5.2 斜杠命令（Slash Commands，对标 + 超越）

| 命令 | 作用 | 底层 |
|---|---|---|
| `/start <任务>` | 启动新任务 | `StartTask`（running 时提示 409，单任务约束） |
| `/stop [--force]` | 停止任务 | `StopTask` |
| `/say <agent> <文本>` | 定向消息 | `SendMessage` |
| `/tree` `/events` `/cost` `/inspect` | 切工作区/侧栏焦点 | 本地 |
| `/approve` `/deny` | 对当前 gate 快速决策 | `ReplyGate` |
| `/discuss <id>` | 打开讨论 | `Discussions` |
| `/config` `/profiles` | 打开设置视图 | `ConfigRaw`/`Profiles` |
| `/doctor` | 环境自检 | `Doctor` |
| `/knowledge lint\|pull\|promote` | 知识管理 | `KnowledgeLint/...` |
| `/help` | 快捷键与命令帮助 | 本地 |
| `/quit` | 退出（不停任务，仅断开界面） | 本地 |

- 输入 `/` 触发**命令面板**（模糊搜索 + 参数补全 + 说明），比 Claude Code 的斜杠命令多了实时补全与参数提示。
- `@` 触发 **agent 引用补全**（`@sub_001`），用于快速定向。

### 5.3 全局键位（Keymap，vim 友好）

| 键 | 作用 |
|---|---|
| `Tab` / `Shift+Tab` | 在 树/工作区/侧栏 间切焦点 |
| `1/2/3` | 直达 树/工作区/侧栏（或 Action Center 待办） |
| `g t` / `g e` / `g i` | 工作区切到 tree-focus / events / inspect |
| `a` | 打开 Action Center / 审批 |
| `/` | 命令面板 |
| `s` | 折叠/展开侧栏 |
| `t` | 折叠/展开 thinking |
| `f` | 对话自动跟随开关 |
| `?` / `F1` | 帮助 |
| `q` / `Ctrl+C` | 退出（二次确认；若任务在跑，提示后台留存 vs 停止） |

键位集中在一处 `keymap`（用 bubbles/key），支持后续用户自定义 + 帮助面板自动生成。

---

## 6. 状态机（前端）

### 6.1 连接状态机（Gateway 层）

```
       ┌─────────┐  探测到 daemon   ┌──────────┐
init ─▶│ Probing │─────────────────▶│ HTTP+SSE │─┐
       └────┬────┘                  └────┬─────┘ │断线
            │ 无 daemon                  │        ▼
            ▼                            │   ┌─────────────┐
      ┌───────────┐                      └──▶│ Reconnecting│──重连成功(带since续传)─┐
      │ In-Process│                          └──────┬──────┘                        │
      └─────┬─────┘                                 │多次失败                        │
            │                                        ▼                               │
            └──────────────┬─────────────────▶ ┌─────────┐ ◀───────────────────────┘
                           │                   │ Degraded│（只读/文件兜底，提示用户）
                           ▼                   └─────────┘
                     ┌──────────┐
                     │ Connected│（正常）
                     └──────────┘
```

TopBar 右侧常驻连接徽章：`● 进程内` / `● HTTP(SSE)` / `⟳ 重连中` / `▲ 降级`。

### 6.2 任务生命周期状态机（对应 RunStatus）

```
Idle ──/start──▶ Starting ──成功──▶ Running ──/stop 或 report──▶ Stopping ──▶ Idle
                    │                  │
                    └─409(已有任务)────┘（提示：单任务约束，先 /stop）
```

- Running 时 `/start` 被拒 → 友好提示而非报错。
- Running → 收到 root agent 的 `report_*`（Inbox）或事件表明完成 → 弹出"任务完成"卡片（含成本摘要 + 报告正文），对标 Claude Code 的完成态但附成本账单。

### 6.3 审批项状态机（每个 gate 项）

```
Pending ──提交决策──▶ Submitting ──静默窗──▶ Applying ──收到 gate_decision 事件──▶ Resolved
                                                  │超时(>~15s 未见事件)
                                                  ▼
                                              StaleWarn（提示"可能未生效，重试？"）
```

---

## 7. 数据流（Data Flow）端到端示例

**场景：agent 请求执行危险 shell，人类放行。**

```
1. agent 触发 gate → 框架写 inbox/gate_<id>.md + audit_events(gate_pending)
2. Gateway 的事件循环拉到 gate_pending 事件（Seq 游标增量）
        │
        ▼
3. Gateway 折算成 DomainEvent{Kind:GatePending, id, kind:shell, cmd:"rm -rf build"}
        │ 推入 EventBus
        ▼
4. TUI Store(reducer) 收到 → AppState.ActionCenter += 1 项；标记对应树节点 awaiting_gate
        │
        ▼
5. View 重渲染：Action Center 高亮、StatusLine 显示 "a 审批(1)"、树节点变 ⏸
        │  用户按 a → o（放行一次）+ 回车
        ▼
6. View 派发 Intent{DecideGate, id, GateDecision{allow,once}}
        │
        ▼
7. Store 乐观更新（该项 → Submitting）+ 调 Gateway.DecideGate
        │
        ▼
8. Gateway.ReplyGate → 追加 @grant once 到 gate_<id>.md（与手编同通道）
        │  （静默窗 ~10s）
        ▼
9. 框架读回执 → 兑现 grant → 写 audit_events(gate_decision, granted_by=human)
        │
        ▼
10. Gateway 拉到 gate_decision 事件 → DomainEvent{GateResolved}
        │
        ▼
11. Store：该审批项 → Resolved（从 Action Center 移除）；树节点恢复 running
        │
        ▼
12. View：卡片显示 ✓ 已生效；成本/事件流更新
```

**要点**：写走命令方法（乐观更新），真相一律回流自**事件流**（单一数据源）。UI 永不自己臆断"成功"，一切以 audit 事件为准——这天然继承了 Marl 的"审计为真相"哲学。

---

## 8. 视觉与主题（Theme）

- **配色**：语义色统一（绿=健康/running，黄=注意/blocked/告警，红=危险/crashed/deny，主色=品牌强调）。深色为主，提供浅色主题。经由 lipgloss 的 `AdaptiveColor` 适配终端明暗。
- **降级**：检测 `NO_COLOR` / 无 truecolor → 退到 16 色 + ASCII 图标（`👤`→`H`、`🤖`→`*`、色块→`[R]/[Y]/[G]`），对齐 `status.go` 的 `-color auto|always|never` 策略。
- **排版**：等宽对齐的表格（成本/事件用 lipgloss table）；圆角边框区分面板；焦点面板边框高亮。
- **无障碍**：所有颜色信息都有符号/文字冗余（色盲友好）；键盘可完成一切操作，鼠标为增强项（bubbletea 支持 mouse）。

---

## 9. 需要补齐的契约空白点（TUI 的差异化价值）

调研发现两处现有契约的空白，TUI 正好补齐（并回馈到 Gateway 供 Web/GUI 复用）：

1. **对话实时 tail**：`marl attach`（`cmd/marl/log.go`）的轮询循环是空骨架，未真正打印新条目。TUI 用"事件流触发 + `Conversation` 增量"实现真正的实时跟随——这是相对 CLI 的明确增强。

2. **Escalation 求助未进契约**：escalation 主流走 `<control>/requests/pending/`，不在 `Inbox()` 覆盖范围，`contract.Interaction` 也没有 `ReplyEscalation`。建议：
   - Gateway 补一个 `ReplyEscalation(id, text)`：读 `requests/pending/escalation_<id>.md` → 在 `## 回复` 后写入 → 移动到 `requests/done/`（复用 `escalate/mailbox.go` 的语义与 nonce 对账）。
   - 长期建议向项目提议把它纳入 `contract.Interaction`，让三前端统一。
   - Action Center 把 escalation 作为第三类待办展示。

> 这两点建议独立成 PR/issue，因为它们轻微超出"纯 TUI"边界，触及契约层——需与项目维护者确认边界（原则：契约层是三前端共享资产，改动要谨慎）。

---

## 10. 落地计划（Milestones）

按"最快看到核心闭环 → 逐步补全面板"推进。每个里程碑都可独立运行、可演示。

### M0 · 骨架与 Gateway（地基）
- 建包结构：`internal/frontend/gateway`（可复用层）+ `internal/tui`（bubbletea 层）。
- Gateway：封装 `contract.Interaction`，实现连接探测（进程内/HTTP）、事件游标增量、EventBus、`FrontendState` 派生。
- TUI：bubbletea 主 `Model`，`tea.WindowSizeMsg` 响应式骨架，空三栏布局 + StatusLine。
- 验收：能连上、TopBar 显示连接模式、事件流能滚动打印原始 audit 事件。

### M1 · 核心闭环（MVP，最能展现精髓）
- ① Agent 树（`Agents()` + spawn/state 事件增量刷新）。
- ② Conversation Tab（`Conversation` + 实时 tail，按 Role 渲染）。
- ⚡ Action Center + Gate 审批卡片（`ReplyGate` + 静默窗诚实反馈）。
- ③ 成本仪表盘（`Costs`）。
- InputBar 发消息（`SendMessage`）+ `/start` `/stop`。
- 验收：`启动任务 → 看树/对话 → 审批一次 gate → 看成本`——四大精髓全部在屏可见可操作。

### M2 · 观测增强
- Events Tab（过滤、payload 展开、定位节点）。
- 侧栏阶梯全景 + 升级历史 + Watchdog 告警。
- Inspect Tab（信封/report/binding/证据）。
- 完成态卡片（report + 成本摘要）。

### M3 · 交互增强
- 完整斜杠命令面板（模糊搜索 + 补全）+ `@` agent 引用补全。
- Discussion 讨论审批流（`ReplyDiscussion`）。
- Escalation 待办 + 回复（§9 契约补齐）。
- 主题/无障碍/降级渲染。

### M4 · 设置与打磨
- `/config` `/profiles` 编辑器（`ConfigRaw`/`WriteConfig`，422 校验错误内联提示）。
- 知识管理（lint/pull/promote）。
- 鼠标支持、动效打磨、帮助面板自动生成、性能（大事件流虚拟滚动）。

### 后续（复用验证）
- 用同一 `Gateway` 起 Web UI 原型，验证 L2 复用性（不属于本次 TUI 交付，但架构已为此铺路）。

---

## 11. 技术选型与风险

**选型（bubbletea 生态）**：
- `bubbletea`（TEA 主框架）+ `lipgloss`（样式/布局/表格）+ `bubbles`（textinput/viewport/list/table/key/spinner/help 现成组件）。
- viewport 做对话/事件流滚动；list 做 Action Center；table 做成本；textinput 做 InputBar。

**风险与对策**：
| 风险 | 对策 |
|---|---|
| 事件流量大导致卡顿 | 增量游标 + viewport 虚拟化 + 事件在 Store 层限量保留（滚动到底才拉更多历史）。 |
| 静默窗延迟让用户困惑 | 乐观 UI + 明确"提交/生效中/已生效"三态 + 事件确认，绝不谎报。 |
| JSON 键大小写陷阱 | Gateway 层统一解析并转成前端结构体，View 层永不碰原始 JSON。 |
| 单任务约束被误解 | Running 时 `/start` 友好提示；TopBar 明确任务态。 |
| 进程内模式与 daemon 模式行为漂移 | 一切走 `contract.Interaction`，两模式共享同一 Gateway 之上的逻辑；差异只在传输，加集成测试覆盖两条路径。 |
| 契约空白（tail/escalation） | §9 明确边界，先在 Gateway 内实现，再议是否上提到 contract。 |

---

## 12. 与 Claude Code 的对比小结（为什么"更好用"）

| 维度 | Claude Code | Marl TUI |
|---|---|---|
| 智能体模型 | 单 agent 单对话 | **多 agent 树 + 并行同屏** |
| 人类角色 | 对话者 | **监督树的根**，决策聚合到 Action Center |
| 审批 | 内联 yes/no/always | **富审批卡片**：once/count/tokens/always + 理由 + 静默窗诚实反馈 |
| 成本 | 底部一行 token | **成本仪表盘**：阶梯分项/缓存命中/思维链占比/建议告警 |
| 可观测性 | 对话即全部 | **独立事件流**（不可篡改审计）+ 检视器 + watchdog 告警 |
| 命令 | 斜杠命令 | 斜杠命令 + **模糊补全** + `@`agent 引用 |
| 架构 | 内建 | **契约驱动分层，Web/GUI 可复用同一 Gateway** |

**结论**：Marl TUI 不是"又一个对话框"，而是一个**AI 团队指挥台**——把 Marl "人类为根、并行 fork、停下问你、成本可见"的精髓变成可一眼看懂、可键盘直达的操作界面。
```

---

## 13. 实现注记（设计与落地的偏差记录）

实现于本文件写成之后，以下偏差/落地事实以代码为准：

1. **落点**：`internal/frontend/gateway`（L2 可复用层：State 快照 + DomainEvent 折算 + 命令面 + 轮询循环）、`internal/tui`（bubbletea 层：Model/View/键路由/渲染）、`cmd/marl/tui.go`（子命令入口，TTY 守卫）。
2. **Gateway 为具体类型而非接口**（§1.1 的 Gateway interface 未落地）：第二个前端（Web/GUI）意图出现前不预建接口——消费侧按需声明窄接口（指令 §2.2 的纪律）。
3. **事件消费用游标轮询而非 SSE**：进程内模式无 HTTP 层，SSE 只属 daemon 模式；游标增量（Seq 单调 + 去重）在两种模式下语义一致，实现只写一份。SSE 保留给未来的浏览器前端。
4. **快照承载而非增量补丁**：每条 Update 携带全量 State，订阅者丢帧无损——继承"audit 为真相"哲学，前端不维护第二份可漂移的领域状态。
5. **escalation / attach 修复已先于 TUI 落地**（contract.Escalations/ReplyEscalation + 三实现一致性测试；marl attach 真 tail），TUI 的 Action Center 因此能覆盖三类待办。
6. **审批卡两步流**：模式单键（o/n/k/a/d）→ N/批注输入步 → 提交；GateDecision 字段互斥契约（count 只填 Count、tokens 只填 Tokens）由测试锚定。
7. **诚实反馈落地**：审批提交后 toast 明示"静默窗 ~10s"，不谎报生效；生效确认留给 gate_decision 审计事件（后续里程碑：从 toast 推进到内联三态）。
8. **真机回归**：WindowSizeMsg 先于首帧快照到达导致 nil panic（测试盲区：单测按"先快照后尺寸"喂序列）——已修复并补回归测试；`script` 伪 TTY 的 e2e 冒烟（渲染树/对话/输入/状态行）通过。
