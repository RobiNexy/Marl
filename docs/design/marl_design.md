# Marl 设计文档 v2

**代号**：Marl
**语言**：Go（纯二进制部署）
**外部依赖**：SQLite（消息存储）+ Fossil（版本控制与协作基底）
**定位**：单人项目，代码生成与创意协作
**核心范式**：项目原生、分治归并、角色驱动、阶梯经济、编排可组合

---

## Part 0. 设计哲学与总原则

整个框架建立在四条原则之上，后续每个设计决策都可以回溯到它们。当遇到设计分歧时，以这四条为裁决依据。

### 原则 1：Agent 只能表达意图，不能直接改变系统结构

任何"结构性变更"——fork 子 Agent、切换模型、开 Fossil 分支、请求人类介入——都不是 Agent 能直接执行的动作，而是它**提交给框架的意图**。框架统一裁决（校验 → 批准/拒绝/排队）后再执行。

这条原则的收益：所有结构性变更集中在少数几个关口，天然可校验、可审计、可拦截、可做全局优化。它避免了"每个 Agent 各自为政导致系统状态不一致"的混乱。

具体落地的意图类型：
- `SpawnRequest` → Spawner 裁决
- `ReconfigureRequest` → 发 Mailbox 给自己，经能力校验
- `BranchRequest` → Spawner 裁决
- `EscalationRequest` → 沿上报链冒泡
- `DiscussionRequest` → 开 Fossil 分支并进入讨论状态

### 原则 2：不可变的真相 + 可变的投影

**灵感来源**：Event Sourcing 模式 + CQRS（命令查询职责分离）

系统里有两类"真相之源"，都是近似不可变、追加为主的：
- **Message Log**：Agent 的消息历史
- **Fossil ticket / commit / wiki**：任务身份、代码、知识的历史

在它们之上是**可变的投影**：
- **Context View**：发给 LLM 的上下文（Log 的投影）
- **SQLite task_runtime**：任务运行时状态（ticket 的投影）

任何"删除、摘要、重排、剪枝"都只动投影，不动真相。这是"越做越乱"的根本解药——因为你永远能从真相之源重建投影，永远可回溯。

### 原则 3：能力约束代替提示词惩罚

想让 Agent 停止无限递归、别越权写文件、别做危险操作——不靠提示词劝说（LLM 不一定配合），而是**从系统层面收窄它的能力边界**。到达最大深度就禁用规划类技能，越权路径就在技能层硬拦截。系统级硬保证永远优于依赖 LLM 自觉。

### 原则 4：权限来自通道，不来自内容

**灵感来源**：Unix 文件权限模型 + 电话系统的带内/带外信令分离

任何 Agent 能写进去的字节，都不许被解释为授权。人类的审批、Agent 的裁决，必须通过**不可伪造的通道**（文件路径 + 写权限校验、Mailbox 发送者身份）来认证，绝不从消息内容里解析"这是批准"。

具体落地：
- `verdict.md` 不在任何 Agent 的 `writable_paths` 里
- Fossil commit 的 `author` 字段由框架按代码路径决定，Agent 无法影响
- Escalation 回复通过文件信箱，发起方校验路径归属

---

## Part 1. 产品形态

### 1.1 项目原生，而非会话管理器

**灵感来源**：git 的项目级设计 + Postgres 的常驻服务模式

这是产品形态的根本选择。不是"打开应用 → 看到会话列表 → 新建会话"的 ChatGPT 形态，而是"进入项目目录 → 目录本身就是一个活的 Agent"的形态。

类比：git 一辈子跟着项目走，Postgres 作为项目的常驻服务。Marl 同时借鉴了这两者——**像 git 一样跟着项目走，像 Postgres 一样常驻运行**。

这个选择带来的能力：
- **可重现**：`marl init` 声明式初始化，clone 项目就能用同一套 Agent 配置
- **可版本控制**：Agent 的配置、人格、对话都跟代码一起进 Fossil，可 review、可回滚
- **可无人值守**：CI/服务器/嵌入式环境没有 UI，Agent 照样能跑
- **多 UI 共存**：Web、TUI、VSCode 插件都只是"皮肤"，可同时挂在一个项目上

### 1.2 目录结构

```text
my-project/
├── .marl/                      # Agent 的私有世界（进版本控制的部分）
│   ├── config.yaml             # 项目级配置
│   ├── profiles/               # Agent 配置
│   │   ├── _default.yaml
│   │   ├── coder.yaml
│   │   ├── reviewer.yaml
│   │   └── ...
│   ├── prompts/                # 认知提示词 Markdown
│   │   ├── go-engineer.md
│   │   ├── code-reviewer.md
│   │   ├── _index.yaml
│   │   └── ...
│   ├── knowledge/              # 共享知识库
│   │   ├── contracts/          # 接口契约
│   │   ├── decisions/          # 架构决策
│   │   └── preferences/        # 常驻块（偏好与约束）
│   ├── conversation/           # 对话存档 Markdown
│   │   ├── 2025-01-15_oauth2.md
│   │   └── _index.md
│   ├── project.fossil          # Fossil 仓库本体       [忽略]
│   ├── supervisor.db           # SQLite 消息存储       [忽略]
│   ├── snapshots/              # 工作区快照            [忽略]
│   ├── logs/                   # 运行日志              [忽略]
│   └── papers/                 # arXiv 论文存储        [可选]
│
├── src/                        # 真实项目代码（所有 Agent 共享写入）
├── tests/
└── ...
```

**控制面目录**（不在项目内，不进版本控制）：

```text
~/.local/state/marl/<project-id>/
├── discussions/                # 讨论分支工作区
│   ├── discuss_01H8X/
│   │   ├── draft.md            # Agent 写
│   │   └── verdict.md          # 人类写（Agent 不可写）
│   └── ...
├── requests/                   # human_request 文件信箱
│   ├── pending/
│   └── done/
└── web_cache/                  # web_fetch 缓存
```

**版本控制规则**（Fossil ignore 在 init 时自动写好）：
- 进版本控制：`config.yaml`、`profiles/`、`prompts/`、`knowledge/`、`conversation/`
- 忽略：`project.fossil`（仓库本体不追踪自己）、`supervisor.db`、`logs/`、`snapshots/`
- 用户自选：`papers/`（PDF 较大）

**关键设计**：`.marl/` 是"Agent 的大脑"（配置 + 运行时数据 + 对话），可以碎片化；`src/` 是"共享产物"（连贯的项目代码，所有 Agent 协调写入）。两者物理隔离——这解决了"每个子 Agent 一个目录导致文件树难看"的问题。

控制面搬到 `~/.local/state/` 是**关键安全措施**：Agent 的 shell 天然在项目树上跑，要靠过滤守住授权文件；挪到 HOME 下另一棵树，它不在任何 Agent 的工作路径上，`shell_exec` 物理上够不着。

### 1.3 Daemon 模式

**灵感来源**：systemd 的用户服务 + Docker daemon

Agent 进程是项目内的常驻服务，类比 Postgres：数据库一直跑着，应用连上去用，不会每次起应用就重启数据库。

```bash
marl init                      # 初始化项目（一次性，全自动）
marl daemon                    # 启动常驻后台进程（每项目一次）
marl start "实现 OAuth2 登录"   # 通过 IPC 发任务，秒回
marl status                    # 查看 Agent 树与状态
marl log                       # tail -f 当前对话（无需 UI）
marl attach                    # 当前终端变 TUI
marl serve                     # 起 Web UI
marl pause / marl resume       # 控制
marl reconfigure --model X     # 热切换参数
marl checkpoint "msg"          # 手动存档（人类触发的 commit）
marl stop                      # 关闭 daemon
```

Daemon 模式的价值：
1. 频繁在 Web/终端/VSCode 间切换时不用反复重启 Agent
2. 子 Agent fork、supervisor 决策在同进程内用 goroutine，比 IPC 快几个数量级
3. SQLite 连接、内存缓存、订阅状态不用反复重建
4. `marl log` 让无 UI 场景（服务器/CI）也能观测

**核心理念：UI 是奢侈品，不是必需品。**

### 1.4 Conversation 文件格式：Markdown + 块级扩展

对话的载体是项目内的 Markdown 文件，不是 UI 里的数据库记录。这让对话可读、可 diff、可 commit、可 grep。

文件本身是纯 Markdown（任何编辑器可读），特殊结构化信息用 `marl-*` fenced code block 承载：

````markdown
---
conv_id: 2025-01-15_oauth2
title: 实现 OAuth2 登录
started_at: 2025-01-15T10:30:00Z
status: running
---

## 10:30:05  Human

帮我实现 OAuth2 登录。

## 10:30:08  Assistant

我先调研项目结构。

```marl-action
type: skill_call
skill: list_dir
args: { path: ".", depth: 2 }
status: ok
duration_ms: 23
```

```marl-think
这是 Express 项目，计划 fork 三个子 Agent 并行处理。
```

```marl-task-fork
sub_agent_id: sub_001
profile: coder
task: 实现 oauth.js 核心
status: dispatched
```
````

设计要点：
- 普通 Markdown 渲染器看到的是灰底代码块，自定义 UI 渲染成彩色卡片——**降级体验完美**
- 每个块带元数据（type/status/duration_ms），可 grep 统计（如"所有 skill_call 的频率"）
- 人类可以直接用文本编辑器改对话、删段落、加批注
- Parser 实现简单：按 H2 切分回合，回合内识别 `marl-*` 块，其余当普通 Markdown

块类型枚举：`marl-human-message`、`marl-action`（技能调用）、`marl-think`（思维链）、`marl-tool-result`、`marl-task-fork`、`marl-task-merge`、`marl-human-approval`、`marl-suggestion`。

---

## Part 2. 核心抽象：编排-执行循环

**灵感来源**：React 的 setState + Reducer 模式

整个框架的原子单元是一个状态机 + reducer 模型：

```text
State       = Message[]（消息数组，唯一的可变状态）
Orchestrate = 对 State 的 CRUD（纯函数式的增删改查）
Execute     = State → LLM → 追加新 Message（副作用）
Loop        = Orchestrate → Execute → Orchestrate → Execute ...
```

一次"编排-执行"的组合是最基本的单元。所有其他概念都**挂载**到这个模型上，而不是平行于它：

- **LLM 参数**（温度、top_k、模型 ID）= Execute 步骤的输入，不属于 State
- **10+ 种内部角色** = Message 的属性，属于 State 的数据结构
- **多厂商适配** = Execute 的后端实现（Adapter）
- **思维链** = Execute 产出的一种特殊 Message
- **上下文压缩/裁剪** = Orchestrate 阶段的操作

这个抽象的意义在于：一旦认定"编排-执行单元"是原子，整个框架就有了主心骨，任何新需求都能找到它的归属位置。

---

# Part 3. 消息系统（Log / View 两层模型）

这是整个框架的核心数据层，也是"精细编排"这个卖点的载体。

### 3.1 两层架构

```text
┌────────────────────────────────┐
│  Context View（可变投影）                              │
│  有序的引用列表，指向 Log 中的条目 + 呈现指令           │
│  这是"发给 LLM 之前的工作台"                           │
└────────────────┬────────────────┘
                     │ 引用（不复制内容）
                     ▼
┌────────────────────────────────┐
│  Message Log（追加为主，近似不可变）                    │
│  每条消息永久保存，带完整血缘                           │
│  保证"可复原"和"剪枝保留历史"                          │
└────────────────────────────────┘
```

核心不变量：**编排操作永远不销毁 Log，只改写 View。**

这个设计解决了几个互相冲突的需求：
- "删除消息" vs "可复原" → 删除只是 View 里置 `Visible=false`，Log 仍在
- "摘要替换原文" vs "保留历史" → 摘要是 Log 追加新条目，View 换引用，原文不丢
- "剪枝重来" vs "审计" → 丢弃 View，Log 保留供复盘

### 3.2 数据结构

```go
type MessageID string  // ULID，按生成时间可排序
type AgentID   string

type InternalRole string

const (
    RoleUserInput          InternalRole = "user_input"
    RoleAssistantReply     InternalRole = "assistant_reply"
    RoleToolResult         InternalRole = "tool_result"
    RoleThinking           InternalRole = "thinking"
    RoleConstraint         InternalRole = "constraint"
    RoleSharedMemory       InternalRole = "shared_memory"
    RoleSubTaskResult      InternalRole = "sub_task_result"
    RoleEscalation         InternalRole = "escalation"
    RoleHumanNote          InternalRole = "human_note"
    RoleTransient          InternalRole = "transient"  // View-only，不入 Log
)

type Provenance string

const (
    ProvOriginal    Provenance = "original"
    ProvSummaryOf   Provenance = "summary_of"
    ProvSplitOf     Provenance = "split_of"
    ProvAnnotatedOf Provenance = "annotated_of"
    ProvInjected    Provenance = "injected"
)

type LogEntry struct {
    ID          MessageID
    AgentID     AgentID
    Seq         int64          // 全局递增序号，排序与引用
    Role        InternalRole
    Content     string         // 文本内容（含 XML 标注），框架不解析
    Prov        Provenance
    SourceIDs   []MessageID    // 血缘：摘要/切块指向的源
    Meta        map[string]any // 元数据（如 split 的 topic、tool_call 的结构化信息）
    Audience    string         // "context" | "audit" | "both"
    TokenEst    int            // 本地估算
    TokenActual *TokenUsage    // 仅 LLM 调用返回的条目有值
}

type TokenUsage struct {
    PromptTokens     int  // 输入总量（实测：**含**缓存命中，= hit + miss，见探测报告 §1）
    CompletionTokens int  // 可见输出
    ReasoningTokens  int  // 思维链，独立计费（检测"刷思维链"的关键）
    CacheWriteTokens int  // 写缓存（约为未命中输入的 1.25 倍价）
    CacheReadTokens  int  // 命中缓存（约为未命中输入的 1/4 价）
    ImageTokens      int  // 图片附件贡献的 token（视觉类请求单独计费，见 Patch1 图片输入）
}
```

**关键设计点**：

- **`Seq` 替代时间戳**：时间戳占 token、破缓存前缀。Seq 只在 SQLite 表里，编译上下文时不渲染。时间感由环境块（当前时间）+ gap 标记提供（"15 分钟后"）。
- **`Content` 是自由字符串**：XML 标注（`<analysis>` / `<decision>` 等）由 LLM 自己写、自己读，框架逐字节保留、不解析。工具调用的结构化信息存 `Meta`。
- **`Audience` 三档**：`context` 只进上下文、`audit` 只进审计表、`both` 两者都进。思维链可配置成 audit-only（不占上下文 token）。
- **`TokenActual` 的细分**：`ReasoningTokens` 单列让成本报表能看出哪个阶梯在刷思维链。`CacheWrite`/`CacheRead` 分开算钱。`ImageTokens` 单列让视觉请求（图片附件）的 token 占用可见，不被混进普通 PromptTokens。

### 3.3 View 层（可变工作台）

```go
type WireRole string  // 线路协议的四种角色
const (
    WireSystem    WireRole = "system"
    WireUser      WireRole = "user"
    WireAssistant WireRole = "assistant"
    WireTool      WireRole = "tool"
)

type Stability string  // 稳定性档次，决定缓存断点候选位置
const (
    StabilityFrozen   Stability = "frozen"   // system + 常驻块 + Tools，byte-stable
    StabilityStable   Stability = "stable"   // 历史回合，只追加
    StabilityVolatile Stability = "volatile" // Transient，尾部，不入 Log
)

type ViewItem struct {
    Ref      MessageID  // 指向 Log
    WireRole WireRole   // 编译目标，可被编排覆盖
    Stability Stability
    Pinned   bool       // 裁剪时是否豁免
    Visible  bool       // 软删除：false 则不发给 LLM，引用仍在
    Position float64    // Fractional Index，支持任意位置插入而不重排
}

type ContextView struct {
    AgentID         AgentID
    Items           []ViewItem
    EstimatedTokens int       // 当前 View 编译后的估算大小
    LastEstimatedAt time.Time
}
```

**Fractional Index 技巧**（CRDT Logoot 算法的简化版）：Position 是浮点数，插入时取前后两个 Position 的中点。这样任意位置插入、reorder 都不需要重新编号整个数组，也不会破坏缓存前缀（Position 只在 View 层，编译时按 Position 排序，不渲染到上下文里）。

**Stability 的约束**：编排操作（reorder / 插入 Transient）不得跨 Stability 边界重排，也不得把 volatile 挪到 stable 之前。违反就破缓存。

### 3.4 内部角色 → 线路角色的编译

LLM API 的线路协议永远只认 4 种角色（system/user/assistant/tool）。我们的 10+ 内部角色是**内部语义模型**，在 Execute 前的组装阶段"编译"成 4 种线路角色。

这是一个"编译器"关系：

```text
内部语义角色（10+ 种，可扩展）
    │  组装阶段的编译步骤
    ▼
线路角色（4 种，发给 LLM）
```

编译规则决定：
- 哪些内部角色映射到 system（如 `constraint`）
- 哪些折叠进 user（如 `user_input`、`shared_memory`）
- 哪些在 Token 紧张时被丢弃（如历史 `thinking`）

默认映射表（可被协议特定的 Normalizer 覆盖）：

| InternalRole | 默认 WireRole | 是否可裁剪 |
| :-- | :-- | :-- |
| `user_input` | user | 否（用户原话） |
| `assistant_reply` | assistant | 否 |
| `tool_result` | tool 或 user | 否 |
| `thinking` | assistant | 是（历史思维链） |
| `constraint` | system | 否 |
| `shared_memory` | user | 是 |
| `sub_task_result` | user | 否 |
| `escalation` | user | 否 |
| `human_note` | user | 否 |
| `transient` | user | 永远在尾部，不入 Log |

这个环节自然实现了三层提示词注入（L1 系统前缀 / L2 常驻块 / L3 私有段）和上下文裁剪。编译结果可缓存（View 没变就复用）。

### 3.5 编排操作的原子集

编排操作被拆到最细的原子粒度，追求最大的可组合性。所有操作统一为：`(Log, View) → 新 View（可能追加 Log）`。

完整的原子操作集：

```text
结构操作（改 View 组成）：
  split_message      - 拆分一条消息（semantic / delimiter 两种策略）
  exclude_message    - 软删除（Visible=false）
  restore_message    - 撤销软删除（Visible=true）

顺序操作：
  reorder_message    - 调整 Position（Fractional Index）

内容操作（不改原文，追加派生）：
  annotate_message   - 给消息加批注外壳（<note>...</note>），追加 ProvAnnotatedOf

角色/呈现操作：
  pin_message        - 设置裁剪豁免
  unpin_message      - 取消裁剪豁免
```

**split 详细定义**：

语义拆分调用 Orchestrator（独立 LLM 调用，最便宜档，JSON mode）：

```
输入：带行号渲染的消息（1| ... 2| ...）
输出：JSON [{topic, start_line, end_line}, ...]
校验：覆盖全文、无重叠、段非空
修复：填缝、截断越界、吸附到禁切区外（代码块/引用块/XML 标注块）
生成：N 条 ProvSplitOf 的 LogEntry，Meta 里带 topic
渲染：<segment topic="...">...</segment>
```

delimiter 拆分是机械操作（零 LLM 调用），按分隔符字面切（分隔符行本身丢弃）。

**组合的威力**（这是区别于其他框架的核心）：

```text
组合 1：split → reorder
  长消息[背景,需求,约束] → 切成三段 → 把"约束"提到最前

组合 2：split → 逐段 annotate
  子 Agent 大段输出 → 切块 → 给每块加优先级标签 <note priority="high">

组合 3：split → 选择性 exclude
  输出有用段 + 噪音段 → 切块 → exclude 噪音块

组合 4：exclude 历史 thinking
  压缩前先把历史思维链 exclude 出 View（但 Log 留着供审计）
```

其他框架的上下文管理是黑盒自动的（自动截断/摘要），这套是**白盒、可编程、可组合**的。而且因为 Log 不可变，所有雕琢可回溯、可撤销。

**编排操作也进审计**：每个 Op 执行记入 `audit_events`（`action="orchestrate"`、`target=Op名`、`payload=args`），编排历史完整可查。

### 3.6 Transient（View-only，不入 Log）

**灵感来源**：SQL 的临时表 + Unix 管道的中间流

Transient 是一类特殊消息，存在于 View、参与 Execute，**永不写入 Log**。它的性质极干净：

- 只能追加在 View **尾部**（插中间破缓存前缀，违背 Transient 的"廉价"价值主张）
- 下一轮 View 重建时自动丢弃
- Stability 永远是 `volatile`

适用场景：

| 场景 | 内容 | 为什么不入 Log |
| :-- | :-- | :-- |
| 压缩指令 | "按此骨架总结上文..." | 用完即扔，保留无意义 |
| 深度到顶提示 | "你已在最大深度，不要再尝试 fork" | 这是约束，不是对话内容 |
| 格式纠偏 | 连续两次 tool_call 解析失败后追加格式提醒 | 这是补救措施，不是任务进展 |
| 尾部复述约束 | 长上下文时把关键约束在尾部复述（对抗中段遗忘） | 信息在 constraint 里已有，这是冗余提醒 |

Transient 的审计：它不进 Log，但"第 12 轮追加过一次格式纠偏"这个事实进 `audit_events`——审计表本来就是"发生过什么"的记录，和上下文真相分开。

### 3.7 压缩（生成 SUM）

压缩是框架自动触发的编排操作（不是 LLM 主动调用的工具）：

**触发条件**：`headroom < threshold`（headroom = model_max_tokens - current_context - budget_reserved）

**注意（深度轮实测，见探测报告 §3.10）**：`model_max_tokens` 与 token 计数都是**按模型**的——
`deepseek-v4-pro` 与 `deepseek-flash` 对**同一份字节**报出的 `prompt_tokens` 差 53（1033 vs 980），
即两者 tokenizer 不同。因此 headroom 必须用**当前绑定模型**的口径计算，不能跨模型复用估算值
（换了模型后旧的估算值一律作废，见 §10.6 的"token 估算标记为 stale"）。

**执行流程**：

```text
1. 主 Agent 暂停（进入 StateCompressing）

2. 确定压缩区间
   保留区 = 头部（system + 常驻块 + Tools）+ 尾部（最近 N 轮，默认 3）
   压缩区 = 中段历史

3. L0 机械清理（只在压缩前执行一次）
   - 去重：同一文件的多次 file_read，只留最后一次
   - 剔除被覆盖的旧读取：file_edit 之前的 file_read 可丢
   - exclude 历史 thinking（若 audience=both，改成 audit-only）

4. 调 Orchestrator 生成 SUM
   input: 压缩区的 Segments（已编译成文本）
   instruction: 七段骨架（任务/事实/文件/决策/未闭/失败/现场）
   output: Markdown，不走 JSON mode
 
5. 校验 SUM
   - 机械检查章节标题（## 1. 至 ## 7.）
   - 提取文件清单（第 3 节），校验路径存在性
   不通过 → 重试一次（温度稍高）
   再不过 → 降级到 L2 或 escalate

6. 估算收益
   reclaim = (old_tokens - new_tokens) / old_tokens
   if reclaim < min_reclaim_fraction:
       升级到 L2（阶梯上一级，不是 reconfigure 换模型）

7. 追加 SUM 到 Log（ProvSummaryOf，SourceIDs 指向压缩区所有消息）
   构造新 View = [保留头部] + [SUM] + [保留尾部]

8. 恢复 StateRunning
```

**SUM 的角色**：作为 assistant 的总结（WireRole=assistant），这保持"这是我的工作记忆"的第一人称视角。

**关键约束**：Orchestrator 的输入是压缩区的**只读快照**，它修改不了主 Agent 的状态。产出追加到主 Log 但主 View 暂时不引用，主 Agent 拿到 entry.ID 后自己决定用不用。这避免了"编排污染主干"的递归陷阱。

**成本记账**：压缩的 token 消耗记入独立账本（`ledger.RecordOrchestration`），技术上不占主任务预算，但算进"任务总成本"并单独列一行——这样成本报表能看出"压缩本身花了多少"，帮助判断压缩到底省没省钱。

---

# Part 4. 原子技能库

### 4.1 技能边界的重新界定

**核心原则**：真正的"技能"只包含**对外部世界（文件系统、命令行、网络）的读写**。任何改变 Agent 自身或系统结构的能力，都是"意图"，走各自的裁决关口。

| 能力 | 归属 | 原因 |
| :--- | :--- | :--- |
| `spawn_subagent` | Spawn 意图（提交给 Spawner） | Agent 不能直接改系统拓扑 |
| `request_human` | Escalation 意图（走上报链） | 人类是上报链顶端，统一机制 |
| `request_reconfigure` | Reconfigure 意图（发 Mailbox） | Agent 不能直接改自己的模型 |
| `request_branch` | Branch 意图（提交给 Spawner） | Agent 不能直接改 Fossil 拓扑 |
| `request_discussion` | Discussion 意图（开分支 + 挂起） | 人机讨论是结构性变更 |

这条边界划分的好处是：**技能层是稳定的、单纯的、容易理解的**——它就是 Agent 的"手和眼"，对外部世界做读写。Agent 内部的状态变化、拓扑变化，统统走意图层。

### 4.2 完整技能清单

```text
文件类：
  list_dir             - 列目录结构
  file_read            - 分页读取 / 结构摘要
  file_search          - 内容搜索
  file_write           - 全量写入（原子写）
  file_edit            - 精确编辑
  restore_snapshot     - 从快照恢复

命令类：
  shell_exec           - 同步执行
  shell_spawn          - 后台启动
  shell_kill           - 终止进程

环境类：
  get_env              - 环境概览
  list_prompts         - 查阅可用提示词索引

网络类：
  web_fetch            - 抓取 URL 转 Markdown

文献类：
  arxiv_search         - 搜索 arXiv 论文
  arxiv_fetch          - 下载论文并提取文本

编排类：
  split_message        - 拆分消息（semantic / delimiter）
  exclude_message      - 软删除
  restore_message      - 撤销软删除
  reorder_message      - 重排序
  annotate_message     - 加批注
  pin_message          - 标记裁剪豁免
```

共 20 个技能。

### 4.3 技能设计的通用准则

所有技能都遵守：

1. **原子性**：一个不可分割的动作
2. **正交性**：技能之间无功能重叠
3. **可预测副作用**：schema 声明副作用类型（供批处理并发调度）
4. **可观测性**：返回值含足够状态，避免重复探测
5. **沙箱边界**：强制路径/权限校验
6. **JSON 可序列化**：参数和返回都是 JSON 对象，含 `ok` 字段
7. **声明能力标签**：`read_only` / `mutating` / `structural`，供 Profile 深度收窄
8. **路径归属校验**：`mutating` 类技能执行前检查 `namespace`（下一 Part 详述）

### 4.4 关键技能详细设计

#### file_read — Token 经济性主战场

三种模式：

| 模式 | 行为 | 何时用 |
| :-- | :-- | :-- |
| `auto` | 文件 >200 行用 summary，否则 content | 推荐默认 |
| `summary` | 按文件类型返回结构化摘要 | 大文件初探 |
| `content` | 返回原始内容，附行号（`N\| ...`） | 确定要读全文 |

**summary 模式按文件类型分别处理**：

```text
代码文件：所有函数签名、类定义、方法签名 + 行号，绝不含函数体
          实现：tree-sitter 或语言 AST 解析器

Markdown：标题大纲（# 至######）+ 级别 + 行号

JSON/YAML：键层级、值类型、数组长度（不展开数据）
           数组：array[N] of object (keys: ...)
           深度截断时：object (M keys)

文本：前 N 行预览（类似 head）

二进制：{"type": "binary", "size": ...}

图片：{type: "image", mime, width, height, attachment: <Attachment 引用>}
          读图片不再是"二进制不可读"，而是以 Attachment 形态进上下文；
          模型有 vision 能力时（CapVision）Normalizer 直接按协议注入（见 Part 10 图片附件）
```

返回示例（summary）：

```json
{
  "path": "src/utils/config.py",
  "type": "python",
  "total_lines": 450,
  "symbols": [
    {"kind": "class", "name": "ConfigLoader", "line": 10},
    {"kind": "method", "name": "__init__", "line": 12, "params": ["self", "path: str"]},
    {"kind": "function", "name": "parse_config", "line": 35}
  ],
  "truncated": false
}
```

返回示例（content，带行号）：

```json
{
  "path": "src/utils/config.py",
  "total_lines": 450,
  "offset": 1,
  "limit": 100,
  "content": " 1| import os\n 2| from typing import Dict\n...",
  "truncated": true
}
```

**关键设计**：`auto` 模式让 LLM 不需要关心"是否要先读 summary"——大文件自动走 summary，Token 经济性内建。行号右对齐（` 1|` / `42|`）与 split 工具的渲染统一。

#### file_edit — 最精巧的技能

锚点严格匹配（含缩进、换行符），不做模糊匹配。

返回（无匹配 — 关键设计）：

```json
{
  "ok": false,
  "error_type": "NO_MATCH",
  "message": "old_string not found in 'src/main.py'.",
  "candidates_snippet": "...\n    return config\n\n\ndef main():\n..."
}
```

**为什么 `candidates_snippet` 这么重要**：LLM 写错 old_string（忘了缩进）时，没它就要重新读文件（消耗大量 Token）。有它，LLM 看一眼就能纠正。这是 Token 经济性的微观体现。

实现要点：
- 锚点匹配前统一换行符（`\n` / `\r\n`），写入时保留原文件风格
- 失败时用 `difflib` 找最接近位置，返回 3-5 行片段
- 修改前自动快照到 `.marl/snapshots/<file>.<timestamp>.bak`（保留最近 10 个）
- 复用 file_write 的原子写模块

#### web_fetch — 干净 Markdown，不做搜索

输出干净 Markdown，去掉导航/广告/脚本/CSS。实现用 go-readability + html-to-markdown 库。

返回：

```json
{
  "ok": true,
  "url": "https://pkg.go.dev/net/http#Server",
  "title": "http package - net/http - Go Packages",
  "content": "# http.Server\n\n...",
  "links": [{"text": "Handler", "url": "https://pkg.go.dev/net/http#Handler"}],
  "cached_at": ".marl/web_cache/a3f2c1d8.md",
  "truncated": false
}
```

**关键约束**：`links` 单独提取——LLM 看到相关链接可决定是否继续 fetch（有限深入探索，不是撒网搜索）。**不提供搜索引擎入口**——LLM 只能顺着人类给的门户 URL 往下读。本地缓存（URL hash + 域名片段），TTL 24 小时。

---

# Part 5. 命名空间与路径归属

### 5.1 核心：每个 Agent 一个命名空间

**灵感来源**：Linux mount namespace + Plan 9 的 union mount

不要往一个 `writable_paths` 列表上加禁止项（黑名单，永远不知道漏了什么），改成一个**由挂载点组成的命名空间**：

```
Agent 的命名空间 = 一组 Mount，每个 Mount 是 (路径模式, 模式)
默认规则：没有任何 Mount 覆盖到的路径 → hidden
```

默认 `hidden` 这一条是整个设计的地基。新加的框架内部目录**自动**不可见，不需要每次记得去补一条禁止规则。

一个典型子 Agent 的命名空间：

```
src/**                                    read      # 宽读
src/auth/oauth/**                         write     # 它的活
.marl/knowledge/contracts/**              read      # 契约
.marl/knowledge/preferences/**            read      # 常驻块源文件
（其余一切）                                hidden
```

`.marl/discussions/` 和控制面目录（`~/.local/state/marl/<project-id>/`）从来没被任何 Mount 覆盖 → 对它不存在。它调 `list_dir(".marl")` 看不到这个目录，`file_read` 那个路径得到"文件不存在"，`file_search` 的结果里不会出现。

### 5.2 三种模式

| 模式 | 语义 | Agent 感知到的 | 错误码 |
| :-- | :-- | :-- | :-- |
| `write` | 可读可写 | 存在，能改 | 正常 |
| `read` | 只读 | 存在，改不了 | "PATH_READONLY" |
| `hidden` | 不存在 | **查不到、列不出** | "ENOENT"（不存在） |

**关键**：`hidden` 路径的错误码统一返回"不存在"，不返回"无权限"——用 `ENOENT` 掩盖 `EACCES`，这是 Linux 权限设计里被反复验证的一条，避免信息泄漏。

### 5.3 宽读、窄写

**为什么不能做成纯 chroot**：写代码的场景下，`go build` 要找 `go.mod`（在仓库根），它得能读同项目其他包才知道怎么 import，报错信息里的路径和它认知的路径不一致会疯。

所以正确形态是 **宽读、窄写**：读的范围接近整个代码树，写的范围收得很紧。`hidden` 只用在**控制面**上——那些属于框架自己、Agent 碰了会出安全问题的东西。

默认挂载模板：

| 路径 | 模式 | 理由 |
| :-- | :-- | :-- |
| `src/**` / `tests/**` / `docs/**` | read | 代码宽读 |
| `.marl/knowledge/**` | read | 契约与常驻块 |
| `.marl/prompts/**` | read | 可查阅提示词 |
| `.marl/discussions/**` | hidden | 讨论是人类专属 |
| `.marl/supervisor.db` | hidden | 内部状态 |
| `.marl/logs/**` | hidden | 审计数据 |
| `~/.local/state/marl/**` | hidden | 控制面根本不在项目树 |

子 Agent 的 `writable_paths` 由父 Agent 在 SpawnRequest 里指定，Spawner 校验它是父的子集（不可能通过 fork 提权）。

### 5.4 唯一收口点：Resolver

命名空间只有在**所有涉及路径的操作都走同一个函数**时才成立。散在各个技能里各自校验，迟早漏一个。

伪代码：

```
Resolve(namespace, path, need_mode) → (realPath, error)
  1. 规范化路径（resolve symlinks 到目标，检测 .. 逃逸）
  2. 遍历 namespace.Mounts，找最具体的匹配
     hidden 在同等具体度下优先
  3. 检查 mode 兼容（need=write 但 mount=read → 拒绝）
  4. 返回 realPath 或错误（错误已按 hidden/read 规则脱敏）
```

必须过 Resolver 的技能：`list_dir`、`file_read`、`file_write`、`file_edit`、`file_search`、`restore_snapshot`、`shell_exec`/`shell_spawn` 的 `workdir`、`arxiv_fetch` 的 `save_dir`。

Filter 函数（列目录/搜索结果过滤器）抹掉 hidden 项。

### 5.5 shell_exec 的三层防护

`shell_exec` 一句 `cat ~/.local/state/marl/<project-id>/discussions/*/verdict.md` 绕过全部框架校验。必须诚实处理：

**层 1：控制面搬出代码树（结构性前提）**

`.marl/discussions/`、`requests/`、`web_cache/` 这些授权相关的文件从项目里挪出去，放到 `~/.local/state/marl/<project-id>/`。理由：`.marl/` 在项目里，Agent 的 shell 天然在这个树上跑；挪到 HOME 下另一棵树，它不在任何 Agent 的工作路径上。

`.marl/` 里只留该进版本控制的东西：`knowledge/`、`profiles/`、`config.yaml`——这些恰好都是 Agent 该读的。**两棵树，一棵全可读，一棵完全不可达。**

**层 2：沙箱（能做就做，平台相关）**

Linux 上用 Landlock 或 bubblewrap，macOS 上用 `sandbox-exec`。拿不到沙箱的平台降级并告警——但因为有了第一层，降级后的风险面已经小得多。

**层 3：廉价保险（nonce）**

`verdict.md` 的 frontmatter 里放一个每轮随机的 nonce，框架只接受携带**当轮正确 nonce** 的裁决。Agent 从来不读这个文件（读了也读不到，见第一层），所以它无法盲写伪造。你作为人类不用关心——nonce 是框架预生成在模板里的，你在下面写字就行。

**结论**：授权不建立在"Agent 应该不读某个文件"之上，而建立在"那个文件根本不在它能触及的世界里"。前者是约定，后者是结构。

---

# Part 6. Profile 系统与提示词管理（修正版）

### 6.1 设计哲学

Profile 不是"角色扮演模板"，而是**一个薄薄的配置壳**，把"用什么提示词"、"需要什么能力"、"采样偏好"组合在一起。

提示词本身是 Markdown 文件，存放在 `prompts/` 目录下，框架不解析其内容、不拆分章节、不做结构化处理。**提示词的力量在人类精心编写的文本里，不在框架的数据结构里。**

这条设计来自一个根本性认知：好的提示词不是在"让 LLM 扮演角色"，而是在**系统性地唤醒 LLM 内在的认知能力**——通过激活声明、认知操作协议、抑制项、输出规范来约束 LLM 的思维过程。这种提示词不需要框架"理解"它，只需要框架在正确的时机把正确的文件塞进上下文。

### 6.2 提示词存储

```text
.marl/
├── prompts/                    # 所有提示词，纯 Markdown 文件
│   ├── go-engineer.md
│   ├── code-reviewer.md
│   ├── literary-creator.md
│   └── _index.yaml             # 索引
```

索引文件：

```yaml
prompts:
  - id: "go-engineer"
    path: "prompts/go-engineer.md"
    description: "Go 代码生成/审查/重构，激活数据流建模与契约推理"

  - id: "code-reviewer"
    path: "prompts/code-reviewer.md"
    description: "代码审查，聚焦错误处理、并发安全、抽象合理性"

  - id: "literary-creator"
    path: "prompts/literary-creator.md"
    description: "文学创作，冷峻批判风格"
```

提示词 Markdown 文件由人类维护、进版本控制（Fossil）。框架读整个文件，原封不动塞进 system prompt。不解析、不拆分、不做"只读前三章"。

### 6.3 Profile 数据结构

```go
type Profile struct {
    ID          ProfileID      `yaml:"id"`
    Extends     ProfileID      `yaml:"extends"`     // 可选，继承另一个 Profile
    Description string         `yaml:"description"`
  
    Prompt          string            `yaml:"prompt"`   // 指向 _index.yaml 的 id
    Requirement     Requirement       `yaml:"requirement"`
    Sampling        SamplingParams    `yaml:"sampling"`
    Thinking        ThinkingSpec      `yaml:"thinking"`
    Task            TaskPolicy        `yaml:"task"`
    OutgoingContext ContextPolicy     `yaml:"outgoing_context_policy"`
    AllowedSkills   []string          `yaml:"allowed_skills"`  // 空 = 全部允许
    CanSpawn        bool              `yaml:"can_spawn"`
}

type Requirement struct {
    Require    []Capability  `yaml:"require"`      // 硬约束，如 tool_call / json_mode
    Prefer     []Capability  `yaml:"prefer"`       // 软约束，不满足则降级 + 记审计
    MinContext int           `yaml:"min_context"`  // 最小上下文窗口
    RungStart  string        `yaml:"rung_start"`   // 可选：覆盖项目默认起始阶梯级
}

type Capability string
const (
    CapToolCall     Capability = "tool_call"
    CapJSONMode     Capability = "json_mode"
    CapThinking     Capability = "thinking"
    CapPrefill      Capability = "prefill"
    CapVision       Capability = "vision"          // 图片理解
    CapVisionDetail Capability = "vision_detail"   // 图片高细节模式（如 OpenAI 的 detail=high）
    CapImageGen     Capability = "image_gen"       // 图片生成；进枚举占位，v1 不实现
)

type SamplingParams struct {
    Temperature float32 `yaml:"temperature"`
    TopP        float32 `yaml:"top_p"`
    TopK        int     `yaml:"top_k"`
    MaxTokens   int     `yaml:"max_tokens"`
    TimeoutMs   int64   `yaml:"timeout_ms"`  // 默认 1800000（30 分钟）
}

// 采样参数在**思考模式下会被厂商改写或忽略**（实测文档，探测报告 §2）：
// DeepSeek 的 temperature 在思考模式下不生效；top_p 在思考模式下 <0.95 会被抬升到 0.95，
// 非思考模式下恒为 1.0（传入值被忽略）。因此"我设了采样参数"不等于"厂商会照用"——
// 需要确定性的场景只能靠 prompt 约束，不能靠 temperature=0（而零值在本契约里=不发送）。

type ThinkingSpec struct {
    Level   string         `yaml:"level"`    // 模型原生档位名（字符串），不全局枚举
    Budget  *int           `yaml:"budget"`   // budget 控制模式的 token 预算
    Display ThinkingDisplay `yaml:"display"` // 呈现与审计开关
}

type ThinkingDisplay struct {
    LogThinking  bool `yaml:"log_thinking"`    // 是否记入 Log
    ExposeToView bool `yaml:"expose_to_view"`  // 是否可见（false = audit-only）
}

type ThinkingControl string // 模型的思维控制方式（声明在 models.yaml 的 caps 里）
const (
    ThinkControlBool   ThinkingControl = "bool"    // 仅开关
    ThinkControlLevel  ThinkingControl = "level"   // 离散档位
    ThinkControlBudget ThinkingControl = "budget"  // 连续 token 预算
)
type TaskPolicy struct {
    Budget       Budget `yaml:"budget"`
    OutputFormat string `yaml:"output_format"`  // 期望的输出格式提示
}

type Budget struct {
    MaxTokens int   `yaml:"max_tokens"`     // 任务级总预算
    TimeoutMs int64 `yaml:"timeout_ms"`     // 任务级超时
}

type ContextPolicy struct {
    IncludeRoles []InternalRole `yaml:"include_roles"`  // 哪些角色可传给子
    MaxEntries   int            `yaml:"max_entries"`    // 最多传几条
    MaxTokens    int            `yaml:"max_tokens"`     // 传递的上下文不超过此大小
}
```

**关键设计原则（Patch 1 重设计）**：

- **档位不再是整数，也不全局枚举。** `Level` 是字符串，用模型原生名字（DeepSeek 现行是 `"none"` / `"low"` / `"high"` / `"max"`；框架自己的开关词是 `"off"` / `"on"`，由 Normalizer 按 `ThinkingControl` 翻译，见 ADR-0020）。未来某模型加 10 挡，直接改 `models.yaml` 里的 `thinking_levels` 即可，框架代码对挡位数完全无感。
- **档位是模型属性，不是全局属性。** 同一套 `ThinkingSpec` 在不同模型上解析出不同的实际档位，由 Normalizer 按 `ModelCaps.ThinkingLevels` 翻译（见 Part 10.10）。
- `Enabled` / `MaxThinkTokens` 被拆掉：`Enabled` 等价于 `Level == "off"`；`MaxThinkTokens` 只对 budget 控制模型有意义，即 `Budget` 字段。

**强制思考模型**：

`ThinkingLevels` 里没有 `"off"` 的模型（如 OpenAI o-series），用户写 `thinking: "off"` 时：
- Normalizer 返回 `Degradation{Kind: DegradThinkingLevel, From: "off", To: <最低档>}`
- 上层决策降级还是拒绝（进审计）

### 6.4 关键设计点

**① Profile 不点名模型**

`Requirement` 只说"我需要 tool_call 能力"、"我偏好 thinking"，不说"用 deepseek-v4-pro"。绑定由 Router 从阶梯里选（Part 10 详述）。这让阶梯升级成为可能——如果 Profile 钉死 model_id，阶梯就没得升。

**② AllowedSkills 是调用时校验，不影响 schema**

工具 schema 全项目唯一、由框架生成、与 Agent 状态无关，所有 Agent 共享冻结前缀（`StabilityFrozen`）。如果 coder 和 reviewer 的 schema 不同，前缀立刻分裂成两个缓存空间。

所以 `file_write` 在 schema 里永远存在，reviewer 调用时被 `AllowedSkills` 挡掉：

```json
{"ok": false, "error": "SKILL_NOT_ALLOWED", 
 "message": "技能 file_write 不在你的许可列表内。"}
```

LLM 会看到工具表里有自己调不了的工具，代价是偶尔白试一次；这个代价远小于前缀分裂。

**③ 深度约束在私有段，不在 schema**

到达 `maxDepth` 时，`spawn_subagent` 照样在 schema 里。调用时框架返回错误：

```json
{"ok": false, "error": "MAX_DEPTH_REACHED", 
 "message": "已达最大深度 3，不能再 fork。请直接执行任务。"}
```

约束强度完全一样（依然是系统级硬拒绝，依然不依赖提示词），但前缀保住了。深度这个事实在**私有段**用一句话说明（它在一个 Agent 生命周期内不变，所以是私有段固定文本而非 Transient）：

```xml
<agent_context>
  <depth current="2" max="3"/>
  <workspace>
    <writable>src/auth/oauth/**</writable>
    <readable>src/**, .marl/knowledge/contracts/**</readable>
  </workspace>
</agent_context>
```

### 6.5 Profile 示例

```yaml
profile:
  id: "coder"
  description: "代码实现"

  prompt: "go-engineer"  # 指向 prompts/_index.yaml 里的 id

  requirement:
    require:
      - tool_call
    prefer:
      - thinking
      - json_mode
    min_context: 32000
    # rung_start 不填，使用项目默认起始阶梯级

  sampling:
    temperature: 0.2
    max_tokens: 8000
    timeout_ms: 1800000  # 30 分钟

  thinking:
    level: "high"               # 模型原生档位名（字符串），不全局枚举；挡位数由 models.yaml 决定
    # budget: 8000              # 仅 budget 控制模型（ThinkControlBudget）使用
    display:
      log_thinking: true
      expose_to_view: false     # 思维链只进 Log，不占上下文

  task:
    budget:
      max_tokens: 50000
      timeout_ms: 600000  # 10 分钟
    output_format: "code_with_review_notes"

  outgoing_context_policy:
    include_roles:
      - constraint
      - shared_memory
      - user_input
    max_entries: 5
    max_tokens: 4000

  allowed_skills:
    - list_dir
    - file_read
    - file_write
    - file_edit
    - file_search
    - shell_exec
    - shell_spawn
    - shell_kill
    - get_env
    - restore_snapshot
    - list_prompts
    - web_fetch
    - split_message
    - exclude_message
    - reorder_message
    - annotate_message
    - pin_message
    # 空列表 = 全部允许；有内容 = 白名单

  can_spawn: true
```

另一个示例（审查者，不能写文件、不能 fork）：

```yaml
profile:
  id: "reviewer"
  extends: "coder"  # 继承 coder 的大部分配置

  prompt: "code-reviewer"

  sampling:
    temperature: 0.1  # 覆盖 coder 的 0.2

  allowed_skills:
    - list_dir
    - file_read
    - file_search
    - get_env
    # 没有 file_write / file_edit

  can_spawn: false  # 覆盖 coder 的 true
```

### 6.6 继承与合并规则

`extends` 字段指向父 Profile。合并规则：

- **标量字段**：子覆盖父
- **列表字段**：
  - `AllowedSkills`：如果子非空，完全覆盖（不合并）；如果子为空，继承父
  - `Requirement.Require/Prefer`：合并去重
  - `OutgoingContext.IncludeRoles`：合并去重
- **`Prompt` 字段**：子覆盖父（不合并，一个 Agent 只有一份提示词）

这个规则让继承能表达"大部分一样，个别收紧"的关系，同时保持简单（不需要显式的 exclude 语法）。

### 6.7 父 Agent 往子 Agent 塞提示词

SpawnRequest 可选覆盖子 Profile 默认的 prompt：

```go
type SpawnRequest struct {
    // ...
    ProfileID       ProfileID
    PromptOverride  string      // 可选：覆盖 Profile 里的 prompt 指向
    // ...
}
```

父 Agent 在 Planning 阶段调 `list_prompts` 看索引，决定子任务用哪个 prompt。非空则覆盖子 Profile 的默认 prompt。

使用场景：

```text
父 Agent 收到任务"用鲁迅风格写一篇文章"
  → fork 子 Agent，ProfileID="literary-creator"，PromptOverride="literary-lu-xun"
  → 子的 system prompt = prompts/literary-lu-xun.md（而不是 literary-creator 默认的通用文学提示词）
```

这让父可以针对具体子任务调整子的认知模式，而不需要为每种风格建一个 Profile。

### 6.8 TimeoutMs 默认值的理由

**默认 30 分钟**：
- 思维模式开高深度时 LLM 可能想很久（DeepSeek 的思考档位下，单次等待可能远超普通对话）
- 宁可等久也不要半途截断一个好答案
- 真超时了说明任务太大，应拆分

这个默认值可以被三处覆盖（优先级降序）：
1. 父 fork 时在 SpawnRequest 里临时指定（最高优先级，针对这一次）
2. Profile 里的 `Sampling.TimeoutMs`（Profile 级）
3. 全局配置（兜底）

### 6.9 运行时调整边界

**Reconfigure 可改**（热切换）：
- `Sampling`（含 timeout，但不含 model_id，因为模型由阶梯级别决定）
- `Budget`
- `Thinking.Level` / `Thinking.Budget`（字符串档位，见 Patch 1 重设计）

**Reconfigure 不可改**（要建新 Agent）：
- `Prompt`（换提示词 = 换认知模式）
- `AllowedSkills`（换权限边界）
- `Requirement`（换能力需求会导致重新绑定，等价于换模型）
- `CanSpawn`
- `OutgoingContext`

这个边界的判断标准：**能否在不破坏任务一致性的前提下热切换**。Sampling 参数只影响输出的随机性，不影响"这个 Agent 是谁、能干什么"。

### 6.10 提示词里怎么说命名空间

命名空间的描述放在**私有段**（任务描述那一块），不能进共享前缀，否则各 Agent 前缀分裂、缓存全废。

```xml
<agent_context>
  <depth current="1" max="3"/>
  <workspace>
    <writable>src/auth/oauth/**</writable>
    <readable>src/**, .marl/knowledge/contracts/**</readable>
  </workspace>
  <task>实现 OAuth2 核心，按契约实现 Provider 接口的 Google/GitHub 两份</task>
</agent_context>
```

关键约束：

1. **只说有什么，绝不说没什么**。写一句"你不能访问 .marl/discussions/" 就等于亲手把秘密告诉它。`hidden` 的东西在提示词里也必须不存在。

2. **这块内容必须短**——它在每个 Agent 的私有段，是纯固定成本（每轮都在，不参与缓存前缀）。目标控制在 200 token 以内。

3. **深度事实只陈述不解释**。不写"你已经是第 2 层，如果再 fork 就会到第 3 层也就是最大深度"——LLM 会算。只写 `<depth current="2" max="3"/>`，需要它自己推。

### 6.11 AllowedSkills 的语义补充

空列表 vs 非空列表：

```yaml
allowed_skills: []    # 空列表 = 全部允许（默认行为）
```

```yaml
allowed_skills:       # 有内容 = 白名单
  - list_dir
  - file_read
```

这个语义让"默认宽松"（空列表不用列一遍所有技能）和"显式收紧"（写了就是白名单）同时成立。

**不支持黑名单语法**（如 `!file_write`）。理由：
1. 白名单比黑名单安全（默认拒绝 > 默认允许）
2. 黑名单容易漏（新技能加入时忘记加到黑名单里）
3. 继承时黑名单语义不清（子的黑名单是追加还是覆盖？）

需要"除了 X 其他都要"时，用继承 + 覆盖：

```yaml
# base 有全部技能
profile:
  id: "base"
  allowed_skills: []  # 全部

# observer 继承但去掉写入类
profile:
  id: "observer"
  extends: "base"
  allowed_skills:
    - list_dir
    - file_read
    - file_search
    - get_env
    - web_fetch
```

是的，这让 observer 的定义稍长，但换来的是"一眼看出它能干什么"（从列表直接看）而不是"一眼看出它不能干什么"（需要脑内对照全集）。

### 6.12 Profile 加载器接口

```go
type ProfileLoader interface {
    LoadAll(profilesDir string) error
    Get(id ProfileID) (*Profile, error)
    List() []ProfileSummary
    Reload() ([]ProfileID, error)  // 热重载，返回变化的 ID 列表
}

type ProfileSummary struct {
    ID          ProfileID
    Description string
    Extends     ProfileID
    PromptID    string
}

type PromptIndex interface {
    List() []PromptEntry
    ReadPrompt(id string) (string, error)  // 返回整个 md 文件内容
}

type PromptEntry struct {
    ID          string
    Path        string
    Description string
}
```

`Reload()` 用于开发期调试：改了 Profile 不需要重启 daemon，`marl reload profiles` 热加载。已运行的 Agent 不受影响（它们用的是加载时的快照），新 fork 的 Agent 用新 Profile。

---

# Part 7. 阶梯与成本经济

### 7.1 核心：阶梯取代单模型绑定

**灵感来源**：CDN 的多级回源 + 数据库的冷热分层

传统做法是给每个 Agent 绑定一个模型，遇到搞不定的问题就换更贵的模型（reconfigure）。这有三个问题：
1. 换模型破缓存前缀（缓存键是 model_id + 前缀内容）
2. LLM 不知道"贵模型"和"便宜模型"的区别，随手就换
3. 升级决策依赖 LLM 自觉（"我搞不定"），而 LLM 很少承认搞不定

**阶梯模式**：给项目配置一个**有序的模型列表**（从便宜到贵），框架根据证据自动升级，而不是 LLM 主动换。

```yaml
# ~/.config/marl/ladder.yaml（全局配置，所有项目共享）
# 形状与 §10.6 的规范定义一致（endpoint + model + thinking）；
# 模型名是 models.yaml 的内部 id，远端名随厂商更新（见探测报告 §2）
ladder:
  - id: "r0"
    endpoint: "deepseek-main"
    model: "deepseek-flash"           # → deepseek-flash
    thinking: {level: "off"}
    description: "快速、便宜，适合探索与编排"
    cost_per_mtok: 1.0               # 仅用于排序，权威价格在 models.yaml 的 pricing

  - id: "r1"
    endpoint: "deepseek-main"
    model: "deepseek-flash"           # 同一模型开思维：档位不在缓存键内，前缀部分保留
    thinking: {level: "high"}
    description: "同模型开思维，缓存部分保留"

  - id: "r2"
    endpoint: "deepseek-main"
    model: "deepseek-v4-pro"         # → deepseek-v4-pro：换 model_id，缓存重建
    thinking: {level: "high"}
    description: "更强模型，适合复杂推理"

  - id: "r3"
    thinking: {level: "on", budget: 4096}   # budget 控制模型（ThinkControlBudget）语法
    description: "换厂商，缓存重建"
```

项目的 `config.yaml` 只配起始级：

```yaml
# .marl/config.yaml
project:
  ladder_start: "r0"  # 默认从最便宜档起跳
```

### 7.2 自动升级的证据触发

**原则**：升级决策基于**可观测的证据**，不依赖 LLM 自述。

证据类型（按权重降序）：

| 证据 | 权重 | 触发条件 | 理由 |
| :-- | :-- | :-- | :-- |
| 连续失败 | 高 | 同一子任务重试 2 次仍失败 | 明确的能力不足 |
| 工具格式错误 | 高 | tool_call JSON 解析失败 ≥2 次 | 输出质量问题 |
| 无进展循环 | 中 | 连续 3 轮无新文件改动、无工具调用成功 | 可能卡住 |
| 子任务失败率 | 中 | fork 的 N 个子里 >50% 失败/超预算 | 规划质量问题 |
| 压缩收益不足 | 低 | 压缩后 token 减少 <20% | 摘要能力弱，但不致命 |

**不计入升级的情况**：
- 人类 reject 审批（这是决策否决，不是能力不足）
- Orchestrator 调用失败（它用的是 r0，与主任务模型无关）
- 单次工具调用失败（如 file_edit 锚点不匹配，可能是一次性手误）

### 7.3 升级决策流程

伪代码：

```
每轮结束后：
  evidence_score = 累积证据权重

  if evidence_score >= upgrade_threshold:
      current_level = agent.binding.level
      next_level = ladder[current_level + 1]
    
      if next_level exists:
          记审计：model_upgrade（from / to / reason / evidence）
          agent.binding.level += 1
        
          清空证据累积
          告知 LLM："任务难度较高，已切换到更强模型"
        
      else:
          已在最高档，escalate 给父或人类
```

**关键**：升级是**换阶梯级别**，不是 reconfigure 换 model_id。前者保留缓存前缀（新级别的模型从头开始积累自己的缓存），后者破缓存。

### 7.4 降级时机

升级容易，降级要谨慎。只有明确信号才降级：

| 信号 | 降级动作 |
| :-- | :-- |
| 子任务完成后回到父 | 父保持原级别（不自动降） |
| 任务成功完成 | 下一个**新任务**从 ladder_start 起跳 |
| 人类显式 `marl reconfigure --level r0` | 立即降级 |

**不自动降级**的理由：任务进行中降级会让 LLM 困惑（"我刚才还能调 X 工具，现在怎么不行了"），且降级省的钱远少于升级花的钱（升级是因为卡住了，降级只是省点小钱）。

### 7.5 Ledger（成本账本）

每次 LLM 调用后，记一条：

```go
type LedgerEntry struct {
    TaskID      string
    AgentID     AgentID
    Level       string         // "r0" / "r1" / "r2"
    CallType    string         // "main" / "orchestration" / "discussion"
    TokenUsage  TokenUsage
    CostUSD     float64        // 按汇率和 ladder 里的 cost_per_mtok 算出
    Timestamp   time.Time
}
```

**三类调用分开记账**：
- `main`：主任务的 Execute
- `orchestration`：压缩、split 等编排调用（用 r0，不计入主任务预算）
- `discussion`：人机讨论里的 Agent 起草（也用 r0，单独核算）

### 7.6 成本报表

`marl ladder report` 输出：

```
任务：实现 OAuth2 登录
时长：42 分钟
状态：成功

┌────────┬────────┬────────────┬──────────┬─────────┬──────────┐
│ 阶梯   │ 调用数 │ Token 总量 │ 思维链占比│ 缓存命中 │ 成本     │
├────────┼────────┼────────────┼──────────┼─────────┼──────────┤
│ r0     │   18   │    45K     │   0%     │  78%    │  ¥0.12   │
│ r1     │    7   │    89K     │  42%     │  52%    │  ¥2.34   │
│ r2     │    0   │     -      │   -      │   -     │    -     │
├────────┼────────┼────────────┼──────────┼─────────┼──────────┤
│ 编排   │    3   │    34K     │   0%     │   0%    │  ¥0.05   │
│ 讨论   │    2   │    12K     │   0%     │   0%    │  ¥0.02   │
├────────┼────────┼────────────┼──────────┼─────────┼──────────┤
│ 总计   │   30   │   180K     │  21%     │  63%    │  ¥2.53   │
└────────┴────────┴────────────┴──────────┴─────────┴──────────┘

升级记录：
  11:24 r0 → r1（原因：连续失败 2 次，evidence_score=0.8）

成本构成：
  输入（未命中缓存）  ¥1.20  (48%)
  输入（缓存命中）    ¥0.15  ( 6%)
  输出（可见）        ¥0.80  (32%)
  输出（思维链）      ¥0.38  (15%)

建议：
  - r1 的思维链占比 42%，考虑降低 thinking.level 到 medium
  - 缓存命中率 63%，表现良好
```

**`思维链占比` 这一列是检测"刷思维链骗钱"的直接指标**——如果某个模型这一列长期 >60%，说明它输出的大部分是不可见思维链，性价比存疑。

### 7.7 换模型的缓存失效成本（审计，不硬拦截）

Reconfigure 换 `model_id` 或 `adapter` 会让之前积累的缓存全部失效（缓存键 = 前缀内容 + model_id + cache_bucket）。下一次调用要重新 `cache_creation`（重新上传前缀），而写缓存比命中贵约 1.25 倍。

**处理**：换模型时把失效前的缓存统计记入 `model_switch` 审计——`cache_invalidated: {cache_hits_before, cache_writes_before}`。`cache_hits_before` 代表"已经摊销掉的缓存价值"，失效后要重新摊销。

**关键区分**：**只调 temperature/top_p 等采样参数、不换 `model_id`/`adapter`，缓存不受影响**（缓存键是前缀内容，采样参数不在键内），应原地更新、不记 `model_switch`、不触发缓存失效。

**已实测确认（探测报告 §3.10，两轮复现）**：换 `model_id` **确实**让缓存全废——另一个模型对
刚建立的新前缀 `cached_tokens=0`（厂商缓存键含模型名）。所以本节的成本提示不是保守估计：
"先用便宜档热身、再升到贵档"**不会**省下贵档的输入成本（贵档必须重算整个前缀）。
另外两个模型的 tokenizer 不同（同一份字节 `prompt_tokens` 差 53）→ 换模型后**必须**重估
（§10.6 的 stale 标记是硬要求）。

成本提示：`request_reconfigure` 的 tool 描述里带缓存失效提示，`model_switch` 审计里也带——让决策者（人类/LLM）知情。

### 7.8 阶梯的好处总结

| 对比维度 | 单模型绑定 + 手动换 | 阶梯 + 自动升级 |
| :-- | :-- | :-- |
| 升级决策 | 依赖 LLM 自述"我搞不定" | 基于可观测证据（失败次数、格式错误） |
| 缓存影响 | 换模型破缓存前缀 | 保留各级缓存，升级不破原级缓存 |
| 成本可见性 | 不知道花在哪个模型上 | Ledger 按阶梯分项，一目了然 |
| 思维链检测 | 无法分离思维链成本 | `reasoning_tokens` 单列，刷链可见 |
| 降级路径 | 手动调回便宜模型 | 新任务自动从起始级开始 |

阶梯不是"多模型"，是**成本分层 + 证据驱动的自动调度**。它把"什么时候该用贵模型"这个判断从 LLM 手里拿走，交给框架的证据累积器。

---

# 修正：Part 6 的两处自相矛盾

写 Part 7 时暴露了 Part 6 的两个错误，是我抄旧结构带进来的，先改掉再往下走。

### 修正 1：Profile 不能携带 SkillPolicy

Part 6 里的 `SkillPolicy.Include/Exclude` 和 `DepthOverride.AtMaxDepth` 直接违反了之前定的**共享前缀**决策：工具 schema 是所有 Agent 的冻结前缀（`StabilityFrozen`），必须逐字节一致才能跨 Agent 复用缓存。如果 `coder` 和 `reviewer` 的 schema 不同，前缀立刻分裂成两个缓存空间。

**改法**：工具 schema 全项目唯一、由框架生成、与 Agent 状态无关。技能限制退化成**调用时校验**：

```go
type Profile struct {
    // 参与 schema 生成 —— 无
    // 仅参与调用时校验
    AllowedSkills []string  // 空 = 全部允许
}
```

`spawn_subagent` 在 schema 里永远存在。深度到顶时它照样可见，调用时返回错误：

```
{"ok": false, "error": "MAX_DEPTH_REACHED", 
 "message": "已达最大深度 3，不能再 fork。请直接执行任务。"}
```

约束强度完全一样（依然是系统级硬拒绝，依然不依赖提示词），但前缀保住了。深度这个事实在**私有段**用一句话说明（它在一个 Agent 生命周期内不变，不需要每轮判断，所以是私有段固定文本而非 Transient）。

同理 `reviewer` 不该有 `file_write`——但它在 schema 里，调用时被 `AllowedSkills` 挡掉。LLM 会看到工具表里有自己调不了的工具，代价是偶尔白试一次；这个代价远小于前缀分裂。

### 修正 2：Profile 不能钉死模型

Part 6 的 `LLMParams{Adapter, ModelID}` 和 Part 7 的阶梯是两套互斥机制。Profile 钉死模型，阶梯就没得升；阶梯说话，Profile 里的 model_id 就是死字段。

**改法**：Profile 只表达**需求**和**采样偏好**，绑定由 Router 从阶梯里选：

```go
type Profile struct {
    ID          ProfileID
    Extends     ProfileID
    Description string
    Prompt      string            // prompts/_index.yaml 的 id
  
    Requirement Requirement       // 我需要什么能力（不点名模型）
    Sampling    SamplingParams    // temperature / top_p，不含 model_id
    Thinking    ThinkingSpec
    Task        TaskPolicy
    OutgoingContext ContextPolicy
    AllowedSkills   []string      // 调用时校验用，不影响 schema
    CanSpawn        bool
}

type Requirement struct {
    Require    []Capability  // 硬约束，如 tool_call / json_mode
    Prefer     []Capability  // 软约束，不满足则降级 + 记审计
    MinContext int
    RungStart  string        // 可选：覆盖项目默认起始级
}
```

`request_reconfigure` 的语义随之改变：Agent 不说"换成 opus"，说"我卡住了 + 原因"。**升不升级由证据说话**（Part 7.2），框架裁决。人类调试时才允许 `marl reconfigure --level r2` 点名。

Part 6 的其余部分（提示词管理、继承、私有段写法）不变。

---

# Part 8. Actor 模型与状态机

### 8.1 进程模型：扁平进程表 + goroutine

**灵感来源**：Erlang/OTP 的进程模型 + 微内核的扁平进程表

采用 goroutine 实现 Actor，不用 OS 多进程。LLM 调用是 IO 密集（等 API 返回），Go 的 goroutine 处理成百上千个并发 IO 等待毫不费力；纯二进制单进程部署也最简单。

**关键设计：进程表是扁平的，不存父子关系。**

```go
type ProcessTable struct {
    agents map[AgentID]*AgentProcess   // 平的，没有树
    mu     sync.RWMutex
}

type AgentProcess struct {
    ID      AgentID
    State   AgentState
    Mailbox chan Envelope
    // 不存 ParentID，不存 ChildrenIDs
}
```

父子关系存在于**运行时上下文**（框架注入，不持久化，不进 Log）：

```go
type AgentRuntime struct {
    ID       AgentID
    Depth    int
    ParentID AgentID                      // 仅用于消息路由
    Children map[AgentID]ChildStatus      // 框架维护，工具读
    Namespace *Namespace
    View     *ContextView
    Binding  Binding                      // 当前阶梯级别
}
```

**为什么这样分**：查"我的子进程"是**瞬时状态查询**，不是历史真相。Log 只记"我 fork 了谁"（一条 `assistant_reply` 里的 tool_call），不记"他们现在怎么样"。Agent 想知道子的状态，调框架注入的元工具 `list_my_children`——读 `AgentRuntime.Children`，不遍历 Log。

这条让进程表保持微内核的干净：**内核只做消息路由 + 生命周期，不理解拓扑语义**。

### 8.2 状态机

```text
                    ┌─────────┐
                    │  Idle   │◄────────────────┐
                    └────┬────┘                 │
                         │ 收到 TaskAssign      │
                         ▼                      │
                    ┌─────────┐                 │
              ┌────►│ Running │─────────────────┤
              │     └────┬────┘  任务完成       │
              │          │                      │
              │          ├── fork ─────────┐    │
              │          ├── 上下文满 ─────┤    │
              │          ├── 讨论请求 ─────┤    │
              │          └── escalate ─────┤    │
              │                            ▼    │
              │                    ┌──────────────┐
              └────────────────────┤   Blocked    │
                  阻塞原因解除     │              │
                                   │ 子类型：      │
                                   │ WaitChildren │
                                   │ Compressing  │
                                   │ Discussing   │
                                   │ Escalating   │
                                   └──────────────┘
                       
        panic / poison ──► Crashed（Log 保留，向父报 failed）
```

**只有四个 Blocked 子类型**，全部共享同一套挂起/恢复逻辑：

| 子类型 | 阻塞原因 | 解除条件 | 期间是否烧钱 |
| :-- | :-- | :-- | :-- |
| `WaitChildren` | fork 了子 Agent | 所有子都 report | 否 |
| `Compressing` | 上下文满，Orchestrator 在跑 | SUM 生成并校验通过 | 少量（一次 r0 调用） |
| `Discussing` | 等人类审阅讨论草稿 | `verdict.md` 出现裁决关键词 | 否 |
| `Escalating` | 向上求助 | 上级或人类回复 | 否 |

这个统一是新模型带来的简化。旧设计里 `Dispatching → WaitingForChildren → Merging → ExecutingSelf` 四个状态，实际上只是"阻塞前"、"阻塞中"、"阻塞后"三段，而"阻塞后怎么处理子结果"本来就是 Running 状态下的一次普通编排。**并进 Running 就够了。**

### 8.3 阻塞语义：fork 之后没有父子通信

这是新模型最关键的简化，值得单说。

```text
父 fork N 个子 → 父进入 Blocked(WaitChildren)
    │
    │  这段时间内：
    │  · 父不发起任何 LLM 调用（不烧钱）
    │  · 父不向子发送任何消息（子拿到的上下文在 fork 那一刻就定死了）
    │  · 父的 Mailbox 仍在收（人类输入、系统事件），但不推进轮次
    │  · 子之间不通信（各自独立，靠 writable_paths 隔离 + contracts 协调）
    ▼
所有子 report_to_parent → 父恢复 Running
    │
    ▼
父看到 N 条 sub_task_result，进入下一轮编排
    可能：再 fork 新子 / 自己收尾 / 修正某个子的产出
```

**没有父→子通信这条设计省掉了大量复杂度**：不需要给子发"改一下需求"的消息、不需要子在执行中途处理父的干预、不需要考虑"父改了需求但子已经写了一半"的一致性问题。子的任务在 fork 那一刻就是不可变的输入。

需要改需求怎么办？**等这一轮子回来，父在下一轮 fork 一个新的**。这符合原则 2 的精神：不可变的输入 + 追加新一轮，而不是原地修改。

一个附带好处：这让子 Agent 的行为**可复现**。给定同样的任务描述、同样的注入上下文、同样的 namespace，子的执行不受父的后续干预影响。调试时能重放。

### 8.4 单写者提交

**灵感来源**：数据库的单写者事务 + Git 的 index/staging 分离

子 Agent 只写文件，**不碰 Fossil**。提交由阻塞恢复后的父 Agent 做：

```text
父 fork 3 个子 → 父阻塞
子们各写自己 writable_paths 下的文件（普通文件系统写入）
子 report → 父恢复
父审阅 3 份 report → 一次 commit（一个语义完整的检查点）
```

三个好处：

1. **VCS 无并发**——不需要写互斥、不需要 lockfile 重试。这套东西在多写者模型下要一大段代码。
2. **commit 粒度天然对齐语义**——一次 commit = 一轮分治的完整产出，timeline 干净。
3. **父有机会在 commit 前拦截**——发现某个子的产出有问题，直接丢弃那些路径的改动重新 fork，**回滚发生在 commit 之前，不需要 branch**。

这也是**不需要 per-Agent branch** 的原因。分支退回它本来的用途：人类或 Agent 显式申请的高风险实验（`request_branch`）。

两级 undo 的分工：
- **任务内**：写前快照（`.marl/snapshots/`），子 Agent 自己纠错用
- **跨检查点**：Fossil commit / revert，父和人类用

### 8.5 Watchdog：系统监控，父不感知时间

你之前定的方向：Agent 状态由系统监控，父不需要知道子跑了多久。

**Watchdog 是框架级的独立 goroutine**，周期扫描进程表：

| 检测项 | 判据 | 动作 |
| :-- | :-- | :-- |
| 子 Agent 无进展 | 连续 K 分钟无 Log 追加、无工具调用 | 记审计 + `marl status` 标黄 |
| 子 Agent 超预算 | `TokenUsed > Budget.MaxTokens` | 强制终止，向父报 failed |
| 子 Agent 超时 | `elapsed > Budget.TimeoutMs` | 强制终止，向父报 failed |
| 阻塞链停摆 | 某 Agent 在 Blocked 超过阈值 | `marl status` 标红 + 显示阻塞原因与时长 |
| 死锁 | Blocked 图里出现环 | 记严重告警，escalate 给人类 |

**父从不查询"子跑了多久"**。子要么 report（成功/部分/失败），要么被 Watchdog 终止后由框架代为向父报 failed。父的视角里只有"子回来了，带着什么结果"。

这条设计让父的编排逻辑极简：它不需要理解时间、不需要轮询、不需要超时判断。

### 8.6 讨论阻塞的可观测性

你选了讨论超时**卡死**而不是自动继续（"让它自己跑花的是我的钱"）。这个选择正确但会往上传染：子卡在讨论 → 父在 `WaitChildren` 里卡住 → 整条链停摆，而父那边看不出原因。

所以卡死必须配可观测性。`marl status` 输出：

```text
🤖 root (depth=0)  Blocked(WaitChildren) 3h12m
   任务: 实现 OAuth2 登录
   等待: 3 个子
 
   ├─ 🔧 sub_001 (depth=1)  Blocked(Discussing) 3h08m  ⚠️
   │     讨论: discuss_01H8X「OAuth 接口契约」
   │     等你: ~/.local/state/marl/a3f2c1d8/discussions/discuss_01H8X/verdict.md
   │     超时: 20h52m 后
   │
   ├─ 🔧 sub_002 (depth=1)  Running  12m
   │     任务: 实现 session 存储
   │     最近: file_write src/auth/session/redis.go (2m ago)
   │
   └─ ✅ sub_003 (depth=1)  Done  8m
         已 report: success

⚠️ 此子树因 discuss_01H8X 停摆 3h08m，无 LLM 调用产生（未烧钱）
```

最后那行是关键：明确告诉你**卡住不花钱**，你可以安心去睡觉。这让"卡死"这个选择的心理成本降到零。

### 8.7 Mailbox 消息类型

新模型下消息类型比旧设计少得多：

```go
type MsgType int

const (
    MsgTaskAssign      MsgType = iota // 分配任务（来自父或人类）
    MsgChildReport                    // 子的 report（含框架代报的 failed）
    MsgHumanInput                     // 人类插话（marl say）
    MsgEscalation                     // 下级上浮的求助
    MsgEscalationReply                // 上级的答复
    MsgReconfigure                    // 参数调整（人类或框架）
    MsgShutdown                       // 优雅关闭
)

type Envelope struct {
    From    AgentID   // 框架填，Agent 无法伪造（原则 4）
    To      AgentID
    TraceID string    // 因果链追踪
    Type    MsgType
    Payload any
}
```

**`From` 由框架按代码路径填写**，Agent 无法通过任何参数影响——这是原则 4 在消息层的落点。收到消息的 Agent 可以信任 `From`。

砍掉的旧类型：`MsgChildPanic`（并入 `MsgChildReport` 的 failed 状态）、`MsgPingPong`（Watchdog 直接读进程表，不需要心跳往返）、`MsgSpawnRequest`（fork 是同步的工具调用，不走 Mailbox）。

### 8.8 主循环骨架

```text
Run():
  loop:
    select:
      case ctx.Done():          return
      case env := <-mailbox:    handle(env)
    
  handle(env):
    MsgTaskAssign:     startTask(env) → eventLoop()
    MsgChildReport:    recordChildReport(env)
                       if allChildrenReported(): unblock()
    MsgHumanInput:     appendToLog(RoleHumanNote)
                       // 下一轮编排自然看到
    MsgEscalation:     handleEscalationFromBelow(env)
    MsgEscalationReply: writeReplyToLog() → unblock()
    MsgReconfigure:    applyReconfigure(env)
    MsgShutdown:       drain() → return

  eventLoop():   # 任务进行中
    while state != Idle:
      switch state:
        Running:
          checkHeadroom()          → 不足则进 Compressing
          compileView()            → 编译上下文
          turn := execute()        → 调 LLM，返回 WireTurn（多 Outcome，见 10.16）
          handleWireTurn(turn)     → 逐 Outcome 落 Log + 顺序执行工具；可能进 Blocked
          if taskDone(): report() → state = Idle
      
        Blocked(*):
          waitForUnblock()         # 阻塞在对应的 channel/轮询
          # 期间 mailbox 继续收，但不推进轮次
```

**LLM 调用期间 mailbox 继续累积**（goroutine 自然并发）。人类的 `marl say` 会在**下一轮编排时**被看到——这符合你之前定的"UI 实时性不是问题，能在下一次编排时注入就行"。

**状态快照只在进入/退出 Blocked 时写 SQLite**，Running 内部的轮次切换不写（降低写入频率）。崩溃恢复时读快照 + Log 重放。

**WireTurn 语义（Patch 1）**：`execute()` 返回的不再是单条 Outcome，而是一组 `Outcomes`（reasoning / tool_call / reply 混合流）。`handleWireTurn` 逐条落 Log（所有 Reasoning 共享一个 `StabilityStable` 序列）、工具调用**顺序执行**、未决的调用在下一轮 Execute 以差量续发。细节见 10.16。

---

# Part 9. Spawner 与意图裁决

### 9.1 核心原则

任何 Agent（含根 Agent）**只能请求 fork，真正的创建由框架统一裁决和执行**。

```text
任何 Agent
    │ 提交 SpawnRequest（表达"我想要一个子 Agent"）
    ▼
┌──────────────────────────────────────┐
│  Spawner（框架级单例，不属于任何 Agent）  │
│  1. 校验请求合法性                       │
│  2. 裁决：批准 / 拒绝                    │
│  3. 若批准，创建 Agent、注入上下文、启动  │
│  4. 登记到请求者的 Children（运行时）     │
│  5. 返回结果给请求者                     │
└──────────────────────────────────────┘
```

集中式的独有好处：
1. **全局资源管控**——看到整棵树，能做"活跃 Agent 已达上限"这种全局决策
2. **拓扑一致性**——父子关系、depth 由一处维护
3. **审计单点**——所有创建过 Spawner
4. **批量预检**——`spawn_batch` 时整体资源检查，避免"批准 3 个拒第 4 个导致任务残缺"

### 9.2 SpawnRequest 与裁决

```go
type SpawnRequest struct {
    RequesterID     AgentID
    ProfileID       ProfileID
    PromptOverride  string        // 可选，覆盖 Profile 的 prompt
    TaskDescription string
    WritablePaths   []string      // glob，必须是请求者可写范围的子集
    ReadablePaths   []string      // 可选，默认继承请求者的 read 挂载
    InjectMessages  []int64       // Seq 列表，要注入子上下文的消息
    TraceID         string
}

type SpawnDecision struct {
    Status       SpawnStatus  // approved | rejected
    ChildAgentID AgentID
    Reason       string       // rejected 时说明原因
}
```

**注意没有 `Queued` 状态**。排队是 Pool 的职责（并发闸门），不是 Spawner 的。Spawner 只管拓扑合法性，批准后子 Agent 立刻启动；如果 API 并发满了，子会阻塞在 Pool 上排队——表现为"慢"，不是"被拒"。这个分工让两层各管一件事。

裁决检查项（伪代码）：

```
adjudicate(req):
  # 拓扑
  requester = table.Get(req.RequesterID)
  if requester == nil:            reject "requester not found"
  if !requester.Profile.CanSpawn: reject "profile not permitted to spawn"

  childDepth = requester.Depth + 1
  if childDepth > maxDepth:       reject "max depth {maxDepth} reached"

  if table.ActiveCount() >= maxActive: reject "global agent limit"

  # Profile
  profile = loader.Get(req.ProfileID)
  if profile == nil:              reject "profile not found"

  # 命名空间子集不变量（关键）
  childNS = buildNamespace(req.WritablePaths, req.ReadablePaths, requester.Namespace)
  if !childNS.SubsetOf(requester.Namespace):
      reject "namespace exceeds parent's: {违规路径}"

  # 扇出闸
  if requester.ForkRoundsThisTask >= maxForkRounds:
      reject "已在本任务内 fork {N} 轮，请自行完成剩余工作"

  approve
```

### 9.3 命名空间子集不变量

**灵感来源**：Capability system 的权限单调递减

```text
子.writable ⊆ 父.writable
子.readable ⊆ 父.readable
父.hidden   ⊆ 子.hidden
```

这保证**不可能通过 fork 提权**——一个受限的 Agent 生不出比自己权限大的孩子。代价只是一次集合包含检查。

一个推论：`knowledge/contracts/` 对所有子是 `read`，只有讨论流程能写。契约不会被某个子 Agent 顺手改掉——这正是"接口先行"的保障。

### 9.4 Rejected 回传给 LLM

拒绝必须回传，这是"能力约束代替惩罚"的落地——不惩罚它，而是从系统层面告诉它此路不通，它自然调整。

```json
{
  "ok": false,
  "error": "MAX_DEPTH_REACHED",
  "message": "已达最大深度 3，不能再 fork。请用 file_write / file_edit 直接完成任务。"
}
```

```json
{
  "ok": false,
  "error": "NAMESPACE_EXCEEDED",
  "message": "请求的可写路径 src/api/** 超出你的范围。你只能分配 src/auth/** 内的路径。"
}
```

第二条的措辞很重要：它告诉 LLM **正确的范围是什么**，而不只是说"不行"。这减少一轮试错。

### 9.5 spawn 时注入什么

子 Agent 的初始 View 由三部分构成：

```text
[共享段 · Frozen]      system prompt + 工具 schema + 常驻块
                       ↑ 全项目所有 Agent 逐字节一致，共享缓存前缀

[私有段 · Stable]      任务描述 + workspace 说明 + 深度事实
                       ↑ 每个 Agent 不同

[注入段 · Stable]      InjectMessages 指向的消息（ProvInjected）
                       ↑ 父显式挑选，受 OutgoingContext 策略过滤
```

`InjectMessages` 的过滤：父的 `Profile.OutgoingContext` 限制了 `include_roles`（哪些角色可传）、`max_entries`、`max_tokens`。父想传的超出策略，超出部分被静默丢弃并记审计。

**注入什么最值钱**：契约文件的引用、人类的原始需求、已经确立的决策。**不该注入**：父的探索过程（子不需要知道父怎么摸索出来的）、父的思维链。

这是降低"定向冗余"的主要手段。判断标准：**如果父需要给子解释一大堆背景，说明这个任务不该 fork**——定向成本超过分治收益。

### 9.6 子的 report 与机械检查

子完成时调 `report_to_parent`：

```json
{
  "name": "report_to_parent",
  "description": "Report task completion to parent. Write a clear summary of what you did, which files changed, test results, and any blockers.",
  "parameters": {
    "report": {"type": "string"},
    "status": {"type": "string", "enum": ["success", "partial", "failed"]}
  }
}
```

`report` 是自由文本（LLM 自己决定格式，可用 XML 标注），框架不解析、不校验格式。

**但框架做机械检查**（原则 4：不采信 Agent 自述）：

| 检查 | 实现 | 不通过的动作 |
| :-- | :-- | :-- |
| 声称 success 但留了 `TODO(agent)` | diff 该子 writable_paths 下新增的 `TODO(agent)` 行 | 降级为 `partial` |
| 声称 success 但留了 `TODO(human)` 且未在 report 里提 | 同上 grep | 降级为 `partial` |
| 声称改了文件但工作区无改动 | `fossil status` 该路径无变化 | 降级为 `failed` |
| report 提到的文件路径不存在 | 提取路径 + 存在性检查 | 记审计告警，不降级 |

第一条是防"用写 TODO 代替干活"——LLM 极爱这么做，会让分治退化成一棵 TODO 树。这是纯 grep 级检查，不依赖 LLM 配合。

降级后父看到的是 `partial`，它就知道有活没干完，可以决定重派还是自己收尾。

### 9.7 扇出闸

三道机械闸，防止 fork 失控：

**闸 1：深度**——`maxDepth`（默认 3），到顶拒绝 fork。

**闸 2：每任务 fork 轮数**——同一父在同一任务内 fork 超过 `maxForkRounds`（默认 3 轮）就拒绝。这防止"fork 一轮 → 收 report → 再 fork 一轮"无限循环。

**闸 3：报告压缩比告警**——子的 `report` token 数 vs 子消耗的上下文 token 数，比值太高说明这次 fork 没起到压缩作用（子花了 30K 上下文，只产出 500 token 的报告，说明它的工作量对得起，但如果花 30K 产出 20K 报告，等于把上下文原样搬回父那里）。这条只记审计告警，不阻断——它是给你调 fork 策略用的数据。

### 9.8 Pool：真正的背压点

**灵感来源**：k8s 的 scheduler + 令牌桶限流

并行 fork 的实际上限不是 `maxDepth`，是 **API 的并发上限和 RPM**。Spawner 只管拓扑，并发全部压在 Pool 上：

```go
type Pool struct {
    endpoints map[string]*EndpointState
}

type EndpointState struct {
    Name        string
    MaxInflight int
    RPM         int
    semaphore   chan struct{}   // 并发闸门
    limiter     *rate.Limiter   // 令牌桶
    health      HealthState     // 熔断
}
```

**队列纪律：深度优先**（你定的）。深层 Agent 离完成更近，先放它过去能更快解除其祖先的阻塞，减少"同时被阻塞的 Agent 数"，缩短关键路径。FIFO 会让一堆任务同时半成品化。

实现上是按深度分桶的优先队列，不是简单 channel。

**父在 Blocked 期间不占槽位**（它不发起调用），所以竞争者是同层兄弟和不同任务的各层 Agent。

熔断按 endpoint 维度，Router 的谓词直接读健康状态——不健康的 endpoint 在**过滤阶段**就被剔掉，而不是绑定后失败重试。

### 9.9 根 Agent 的特殊性

根 Agent 是树根（depth=0，无父）。特殊性仅两处：

1. 无父级接收 report——完成时把结果给人类（写 conversation 文件 + `marl log` 可见）
2. escalation 落到它这层就停了——它落人类信箱

其余与普通子 Agent 完全一样。它由框架启动时通过内部 `bootstrap(rootProfile)` 创建，不走 `adjudicate`（那需要 RequesterID）。

**不为根节点搞特殊化**，否则 Actor 模型会分裂。

---

# Part 10. Adapter 五层与 Router

### 10.1 问题：为什么单层 Adapter 会崩

绝大多数框架把 Adapter 做成"一个厂商一个类"，结果这三件正交的事纠缠在一起：

| 轴 | 内容 | 变化频率 | 数量 |
| :-- | :-- | :-- | :-- |
| **线路协议** | HTTP 路径、JSON 形状、流式格式、错误码 | 极低 | 3–4 个 |
| **模型能力** | tool_call / infill / thinking / 窗口 / 缓存模式 | 中（每次新模型） | 几十条 |
| **接入点** | base_url、key、并发上限、RPM、健康状态 | 高（随时加减） | 十几个 |

关键事实：**Deepseek、Moonshot、Qwen、Zhipu、Groq、Together、OpenRouter、本地 vLLM 全都是 OpenAI 兼容线路**。写十个 Adapter 是浪费——它们共用一个线路实现，差别只在接入点配置和能力表。

反过来，同一个厂商内部也会有线路差异：Deepseek 的 FIM 在 beta 路径上走 completions 形状，和 chat 不是同一条线路。**所以能力属于 (模型, 接入点) 组合，不属于厂商。**

### 10.2 五层调用链

```text
Requirement（我需要什么能力）
   │
   ▼ Router：阶梯顺序 × 能力谓词 × 池健康
Binding（哪一级：endpoint + model + rung_id + wire + 降级清单）
   │
CanonicalRequest（协议无关的规范形态）
   │
   ▼ Normalizer（按 binding 的策略集，决定角色布局）
WireRequest（合法的厂商消息数组）
   │
   ▼ Pool（并发闸门 / 限流 / 熔断 / 排队）
   ▼ Wire（唯一碰 HTTP 与 JSON 的地方）
WireResponse
   │
   ▼ Denormalizer（思维链、tool_call、usage、错误分类、thinking 落点）
WireTurn（多 Outcome 序列：reasoning / tool_call / reply 混合，见 10.16）
   │
   ▼ Ledger（记账）→ 喂给阶梯升级与压缩触发
```

每层的变化频率不同，这是分层的唯一理由：Wire 极少变（3–4 个），Catalog 每来新模型改一次（纯数据），Endpoint 随时增删（纯配置），Normalizer 策略集跟着协议走，Router 逻辑基本不动。

### 10.3 Canonical 形态（关键承重结构）

这是全部设计的承重结构。它必须**表达语义与稳定性，不表达角色布局**——后者是 Normalizer 的产出，不是输入。

```go
type CanonicalRequest struct {
    Segments []Segment
    Tools    []ToolDef      // 全项目逐字节一致，属冻结前缀
    Sampling SamplingParams
    Thinking ThinkingSpec
    Prefill  *string        // 期望的 assistant 开头，可能被降级
}

type Segment struct {
    Kind        SegmentKind  // system | standing | knowledge | turn | tool_result | transient
    Speaker     Speaker      // human | assistant | tool | framework
    Content     string       // 含 XML 标注，全协议逐字节保留
    Attachments []Attachment // 图片等附件（Patch 1 新增；v1 仅图片）
    ToolCalls   []ToolCall   // assistant 产出的调用
    ToolCallID  string       // tool_result 的配对 id
    Stability   Stability    // frozen | stable | volatile
}

type Attachment struct {
    Kind     AttachmentKind
    MimeType string
    Source   AttachmentSource
    Data     []byte // base64 原文不存 SQLite，存文件；这里存文件路径（SourceFile）或 URL（SourceURL）
    Detail   string // vision_detail 档位（low / high / auto）
}

type AttachmentKind string
const (
    AttachImage AttachmentKind = "image"
    AttachPDF   AttachmentKind = "pdf"    // v1 不实现
    AttachFile  AttachmentKind = "file"
)

type AttachmentSource string
const (
    SourceBase64 AttachmentSource = "base64"
    SourceURL    AttachmentSource = "url"
    SourceFile   AttachmentSource = "file_path"
)

type Stability string
const (
    StabilityFrozen   Stability = "frozen"   // system + 常驻块 + Tools，byte-stable
    StabilityStable   Stability = "stable"   // 历史回合（只追加）
    StabilityVolatile Stability = "volatile" // Transient，尾部，不入 Log
)
```

`Stability` 三档承担的职责：

| 档 | 内容 | 用途 |
| :-- | :-- | :-- |
| `frozen` | system + 常驻块 + Tools | 跨 Agent 共享缓存前缀，byte-stable 是硬要求 |
| `stable` | 历史回合（只追加） | 缓存断点的候选落点；压缩的作用域 |
| `volatile` | Transient | 只在尾部；不入 Log；不参与前缀计算 |

**不变量**：Normalizer 不得跨 `Stability` 边界重排，也不得把 `volatile` 挪到 `stable` 之前。违反就破缓存，而 Transient 的全部价值就是廉价。

### 10.4 模型目录（纯数据）

```yaml
# ~/.config/marl/models.yaml（机器级，跨项目共享）
# 注：远端名随厂商更新。DeepSeek 现行模型是 deepseek-flash / deepseek-v4-pro；
# 旧版的 deepseek-chat / deepseek-reasoner 已不存在（探测报告 §2）。max_context /
# max_output 是**待核实**值：厂商文档只给了 max_tokens 上限与默认输出（1~384K；
# 默认 8K 非思考 / 64K 思考 / 128K max 档），上下文长度在"模型与价格"页（未快照）。
models:
  - id: "deepseek-flash"
    provider: "deepseek"
    wire: "openai_chat"
    remote_name: "deepseek-flash"
    caps:
      has: [tool_call, thinking, json_mode]
      max_context: 65536            # 待核实（探测报告 §3.3）
      max_output: 8192              # 待核实
      cache_mode: "implicit_prefix"  # 隐式前缀缓存
      unsupported_params: []
      thinking_control: "level"      # 现行 API：顶层 reasoning_effort
      thinking_levels: ["none", "low", "high", "max"]   # 默认 high
    pricing:
      in_per_mtok: 1.0
      cached_in_per_mtok: 0.14
      out_per_mtok: 2.0
      reasoning_per_mtok: 2.0
      currency: "CNY"

  - id: "deepseek-v4-pro"
    provider: "deepseek"
    wire: "openai_chat"
    remote_name: "deepseek-v4-pro"
    caps:
      has: [tool_call, thinking, json_mode]
      max_context: 65536            # 待核实
      max_output: 8192              # 待核实
      cache_mode: "implicit_prefix"
      unsupported_params: []
      thinking_control: "level"
      thinking_levels: ["none", "low", "high", "max"]
    pricing:
      in_per_mtok: 4.0
      cached_in_per_mtok: 0.56
      out_per_mtok: 16.0
      reasoning_per_mtok: 16.0
      currency: "CNY"

    remote_name: "claude-sonnet-4-20250514"
    caps:
      has: [tool_call, thinking, json_mode, vision]
      max_context: 200000
      max_output: 8192
      cache_mode: "explicit_breakpoint"  # 显式断点缓存
      unsupported_params: [top_k]
      thinking_control: "budget"         # 连续 token 预算
      thinking_levels: ["off", "any"]    # 只区分关/开
      vision_detail: true                # 支持 detail 档位（low/high/auto）
    pricing:
      in_per_mtok: 20.0
      cached_in_per_mtok: 2.0
      out_per_mtok: 100.0
      reasoning_per_mtok: 100.0
      currency: "CNY"
```

**Caps 的语义**：

- `has`：这个模型支持的能力（枚举）
- `max_context` / `max_output`：窗口大小
- `cache_mode`：
  - `implicit_prefix`：保持前缀稳定就自动缓存（Deepseek）
  - `none`：不支持缓存
- `unsupported_params`：API 不接受的采样参数（Normalizer 需剔除）
- `thinking_control`：思维控制方式——`bool`（仅开关）/ `level`（离散档位）/ `budget`（连续 token 预算），见 Part 6.3
- `thinking_levels`：该模型支持的档位列表（字符串）。挡位数完全由这里决定，框架代码对挡位无感（Patch 1 重设计）。
  **它只声明"哪些档位可用"，不声明强度或成本**——阶段 1 实测（探测报告 §3.8 → ADR-0025）：两次各 3 次的
  运行给出的档位强度均值序**相反**（`low`/`high` 谁更强不稳定），同档位极差是档位间差值的 5~18 倍。
  因此成本模型不能按档位定值（`reasoning_per_mtok` 只能给量级），框架也**不**提供"用低档位省钱"
  这类旋钮——档位是**能力**开关，不是成本旋钮
- `vision_detail`：是否支持图片 detail 档位（low / high / auto），配合 `CapVisionDetail`

### 10.5 接入点配置

```yaml
# ~/.config/marl/endpoints.yaml（机器级）
endpoints:
  - name: "deepseek-main"
    base_url: "https://api.deepseek.com"
    key_ref: "env:DEEPSEEK_API_KEY"  # 不存明文，只存引用
    max_inflight: 5
    rpm: 60

    key_ref: "env:ANTHROPIC_API_KEY"
    max_inflight: 3
    rpm: 50

  - name: "deepseek-backup"
    base_url: "https://api.deepseek.com"
    key_ref: "env:DEEPSEEK_BACKUP_KEY"
    max_inflight: 3
    rpm: 30
```

**关键设计**：
- `key_ref` 永不存明文，只存 `env:VAR_NAME` 或 `file:/path/to/key`
- 同一个 provider 可以有多个 endpoint（主备、不同账号）
- `max_inflight` 和 `rpm` 是 Pool 的输入

### 10.6 阶梯定义（全局，跨项目）

```yaml
# ~/.config/marl/ladder.yaml
ladder:
  - id: "r0"
    endpoint: "deepseek-main"
    model: "deepseek-flash"
    thinking: {level: "off"}
    description: "快速、便宜，适合探索与编排"

  - id: "r1"
    endpoint: "deepseek-main"
    model: "deepseek-flash"
    thinking: {level: "high"}
    description: "同模型开思维，缓存部分保留（档位不在缓存键内）"

  - id: "r2"
    endpoint: "deepseek-main"
    model: "deepseek-v4-pro"
    thinking: {level: "high"}
    description: "更强模型，换 model_id，缓存重建"

  - id: "r3"
    thinking: {level: "on", budget: 4096}  # budget 控制模型（ThinkControlBudget）语法
    description: "顶级，换厂商，缓存重建"
```

**thinking 开关在阶梯级别，不在 model 定义里**。同一个 model 开不开思维是两级——这正是你的核心需求：先用关思维的强模型，卡住了给它开思维，还卡住了才换模型。

档位是**字符串 + 模型相关**（Patch 1）：`level` 的值必须在对应模型的 `thinking_levels` 里，写错由 Normalizer 记 `DegradThinkingLevel` 降级而不是崩溃。budget 控制模型不填 `budget` 时由模型默认行为决定。

**唯一真相是 `Binding.Thinking`（ADR-0022，阶段 1 探测 §3.1 的裁决）**：档位配在**阶梯**上，
装配层把它拷进 `CanonicalRequest.Thinking`，Normalizer 侧再加一层保险——`req.Thinking` 为空时
**回退**读 `binding.Thinking`，两处都非空且不同时**报错**（框架错误，不发请求）。
这条保险是刻意的：它让"档位配了却不生效"这类**静默失效**在结构上不可达。

**落地缺口（阶段 2 第一件事）**：当前 `internal/wire` 的 `build()` **只读**
`CanonicalRequest.Thinking`，全项目没有任何代码读 `Binding.Thinking`——照现状装配请求，
阶梯上的档位配置会**完全无效**（请求照发、`reasoning_effort` 缺席、走厂商默认档位
——实测默认是 `high`）。实现清单与测试计划见 ADR-0022。

项目配置只说起始级：

```yaml
# .marl/config.yaml
project:
  ladder_start: "r0"
```

### 10.7 Router（两阶段：谓词过滤 + 打分排序）

**灵感来源**：Kubernetes scheduler 的两阶段调度

```text
输入：Requirement（来自 Profile）
      + 当前 rung_index（阶梯位置）
      + 升级/降级信号

阶段 1：谓词过滤（硬约束）
  for each rung in ladder[current_index:]:
      if 不满足 Requirement.Require：过滤
      if model.max_context < Requirement.MinContext：过滤
      if endpoint 不健康或熔断：过滤
      if 密钥不存在：过滤

  if 全部过滤掉：报错（明确说哪条约束未满足）

阶段 2：打分排序（软约束 + 成本）
  for each 候选:
      score = 0
      for cap in Requirement.Prefer:
          if 候选支持 cap：score += 10
    
      # 预估成本（主要因素）
      est_cost = estimateTokens() × pricing
      score -= est_cost  # 便宜的分高
    
      # 亲和性：当前任务已绑定的模型加奖励
      if 候选.model == 当前任务历史绑定：score += 50

  返回：得分最高的候选
```

**亲和性这条很关键**：换模型 = 缓存键空间切换 = 之前积累的缓存全废。所以 Binding 在 TaskSlot 开始时决定一次，全程复用，只在失败或显式 Reconfigure 时重绑。失败切换要记审计，并把 token 估算标记为 stale（tokenizer 换了）。

**已实测确认（深度轮用例 8，两轮复现；见探测报告 §3.10）**：这两句话都有实测支撑——
`deepseek-v4-pro` 建立的新前缀（`cached=0` 前提成立）**不被** `deepseek-flash` 命中
（`cached=0`）→ 缓存键**含模型名**，换模型必然重算整个前缀，升级成本按**全量输入**估；
且两个模型的 tokenizer 不同（**同一份字节**：v4-pro 报 `prompt_tokens=1033`、flash 报 `980`，
差 53）→ "token 估算标记为 stale"不是保守估计，而是**必须**（估算与窗口裁剪都要带 model 维度）。
顺带：`deepseek-v4-pro` 的 `reasoning_effort=high` 同样生效（返回非空 `reasoning_content`），
因此阶梯上的档位配置对两个模型都成立。

### 10.8 Binding 的数据结构

```go
type Binding struct {
    RungID      string        // 阶梯 ID
    RungIndex   int           // 在 ladder 数组的下标
    Endpoint    string
    Model       string        // models.yaml 的 id
    CacheBucket string        // 永远是 AgentID（Patch 1：每个 Agent 独立缓存桶）
    Wire        WireID
    Thinking    ThinkingSpec  // 这一级的 thinking 配置（字符串档位）
    Degraded    []Capability  // 未满足的 Prefer（软约束）
    BoundAt     time.Time
    CachePrefix string        // model_id + endpoint，模型级缓存键前缀
}
```

`CacheBucket` 是**请求级缓存隔离键**（见 10.15）：每次 LLM 请求的 `user_id` 字段=它，把不同 Agent 的缓存桶分开（实测桶隔离：不同 `user_id` 不共享前缀缓存）。`CachePrefix` 是模型级键（model_id + endpoint），两者组成完整缓存键：`前缀内容 + model_id + cache_bucket`。

`Degraded` 记录了 Prefer 里要的但没拿到的能力。如果 Requirement 是 `Prefer: [thinking, vision]` 但绑到的模型只有 thinking，`Degraded = [vision]`。这个信息进审计，不阻断——Prefer 是"有更好，没有也行"。

### 10.9 Normalizer（核心：命名策略，不给旋钮）

**灵感来源**：Ansible 的 facts 而非 variables

你要可配，但自由旋钮会被误用。方案是**不给旋钮，给命名策略**。每个命名策略是"某个协议实测过的已知正确解"。

五个决策点，每个一组具名取值：

| 决策点 | 取值枚举 | 说明 |
| :-- | :-- | :-- |
| 多条 system | `first_only_rest_as_user` / `concat_all` / `prepend_concat` | 多 system 的厂商行为：DeepSeek 支持多条拼接 |
| 连续同角色 | `merge_with_separator` / `interleave_empty` / `keep_as_is` | 严格交替协议需插空 assistant |
| 尾部 assistant（prefill） | `native_prefill` / `demote_to_tail_hint` / `drop` | DeepSeek 的 prefill 是 **Beta**：必须用 `base_url=https://api.deepseek.com/beta`，且最后一条消息 role 必须是 `assistant` 并带 `"prefix": true`。正式端点上的行为**未实测**，阶段 1 按 `demote_to_tail_hint` 保守处理 |
| tool_result 承载 | `native_tool_role` / `inline_as_user` | OpenAI 兼容有 tool role，其他可能没有 |
| 历史思维链 | `native_field` / `strip` / `inline_tagged` | DeepSeek 的 `reasoning_content` 是**独立字段**，且**回传规则取决于有没有 tools**：带 `tools` 时历史轮次的 `reasoning_content` **应当回传**（会被拼进上下文）；不带 `tools` 时**不需要回传**（传了也被忽略）。见探测报告 §2 第 11 条。**已裁决（ADR-0023）**：带 `tools` 默认**全量回传**、不带 `tools` 不发送。实测补充（§3.6，两轮复现）：四种形态（全量 / 只最近一轮 / 不回传 / 空串）**都返回 200**——"均应回传"是期望而非硬校验；回传内容确实进上下文（`prompt_tokens` 差 `+92` / `+38`，恰等于思维链的 token 数）；不带 tools 时带与不带的 `prompt_tokens` **完全相等**（确实被忽略）。**落地缺口**：框架当前**没有**承载历史思维链的字段（`Segment`/`WireMessage`/编码器都没有），阶段 2 必须先补通路 |

伪代码：

```go
type NormalizePolicy struct {
    MultiSystem      string
    ConsecutiveSame  string
    TailAssistant    string
    ToolResultRole   string
    HistoricalThink  string
}

// 每个 wire 自带默认策略
func (w *OpenAIChatWire) DefaultPolicy() NormalizePolicy {
    return NormalizePolicy{
        MultiSystem:     "concat_all",
        ConsecutiveSame: "keep_as_is",
        TailAssistant:   "native_prefill",
        ToolResultRole:  "native_tool_role",
        HistoricalThink: "strip",  // chat 版本不暴露思维链
    }
}

// 可被 (endpoint, model) 覆盖
func LoadPolicyOverride(endpoint, model string) *NormalizePolicy {
    // 从配置文件读覆盖项
}
```

三条约束：

**① 作用域是 endpoint，可被 (endpoint, model) 覆盖，不下放到 Agent 或 Profile。** 它是部署属性，不是任务属性。

**② 每个 Wire 自带 `Assert(WireRequest) error`**，编码该协议的硬规则（user/assistant 是否必须交替、tool 消息是否必须紧跟 tool_calls、system 位置、结尾角色限制）。Normalize 产出必跑断言。策略配错在**启动自检**时就炸，不是任务跑到一半炸。

**③ Byte-stability 是可测不变量**：同一 canonical 前缀 + 同一策略集 → 逐字节相同的 wire 前缀。这条必须有 golden test 守着，否则某天一个"顺手清理"的改动会静默毁掉全项目缓存。

### 10.10 Normalizer 的输出

```go
type NormalizeResult struct {
    Messages    []WireMessage
    Degradations []Degradation
}

type Degradation struct {
    Kind   DegradationKind
    From   string
    To     string
    Reason string
}

type DegradationKind string
const (
    DegradPrefillUnavailable  DegradationKind = "prefill_unavailable"
    DegradParamStripped       DegradationKind = "param_stripped"
    DegradThinkingUnavailable DegradationKind = "thinking_unavailable"
    DegradThinkingLevel       DegradationKind = "thinking_level"  // Patch 1：请求档位超出模型支持
)
```

**Degradation 是返回值而不是静默行为**：prefill 要不到、thinking 要不到、某个采样参数被剔除，都必须显式报出来，由上层决定降级路径并进审计。**能力缺失从来不该静默。**

例子：canonical 里有 `Prefill="<analysis>"`，但绑定的模型不支持 prefill（`CapPrefill` 不在 `caps.has` 里）。Normalizer 产出 `Degradation{Kind: DegradPrefillUnavailable, From: "<analysis>", To: "tail_transient"}`。上层把 prefill 内容转成尾部 Transient 提示（"请从 `<analysis>` 开头输出"）并记审计。

**thinking 档位的翻译**（Patch 1 字符串档位）。`Level` 是模型原生名字，Normalizer 按模型的 `ThinkingControl` / `ThinkingLevels` 翻译成协议形态：

```go
func normalizeThinking(spec ThinkingSpec, caps ModelCaps) (any, []Degradation) {
    if !contains(caps.ThinkingLevels, spec.Level) {
        // 模型不支持该档位（典型：o-series 无 "off"）：
        // 映射到最低档并记 Degradation，由上层决定降级还是拒绝
        requested := spec.Level
        fallback := caps.ThinkingLevels[0]  // 最低档
        spec.Level = fallback
        return nil, []Degradation{{Kind: DegradThinkingLevel,
            From: requested, To: fallback, Reason: "requested level not supported"}}
    }
    switch caps.ThinkingControl {
    case ThinkControlBool:    return spec.Level == "on", nil
    case ThinkControlLevel:   return spec.Level, nil
    case ThinkControlBudget:
        if spec.Level == "off" { return nil, nil }
        return spec.Budget, nil
    }
}
```

能力探测（10.13）逐项打小请求时会顺带验证 `thinking_levels` 里的每个档位是否真的可用（o-series 的 "off" 返回 `CapabilityRejected` 就把它从 `thinking_levels` 里踢进 override）。

### 10.11 Denormalizer（回程，归一化信号）

**灵感来源**：HTTP 状态码的分类规则

回程比去程更值钱，因为**阶梯升级和压缩触发都消费它的产出**。若错误分类留在厂商形态，这两处逻辑就会长满 `if provider == "deepseek"`。

必须归一化成一张表：

| 归一化类别 | 上层反应 |
| :-- | :-- |
| `Transient`（429/503/超时） | Pool 退避重试；不计入升级证据 |
| `ContextOverflow` | 触发压缩，**不是**升级 |
| `CapabilityRejected`（工具 schema 被拒等） | 修正本地能力覆盖表 + 告警，不盲重试 |
| `AuthOrQuota` | 标记 endpoint 不健康，熔断，跳到阶梯下一级 |
| `ContentFilter` | 交回 LLM 决策，记审计 |
| `MalformedOutput`（tool_call 解析失败） | 计入升级证据；可追加格式纠偏 Transient |

```go
type ReasoningChunk struct {  // 一段连续的思维链（Patch 1：可多条，见 WireTurn）
    Content  string
    Duration time.Duration
}
// 落 Log：ReasoningChunk.ToLogEntry() → 生成 Role=thinking 的 LogEntry

type Outcome struct {
    Reasoning *ReasoningChunk  // 思维链片段（denormalize 时拆出）
    Reply     string           // 可见文本回复
    Entry     LogEntry         // 产出的 LogEntry（由 Reply/Reasoning 构造，主循环统一落 Log）
    ToolCalls []ToolCall       // 本 outcome 携带的工具调用
    Usage     *TokenUsage      // nil 表示调用失败
    Signals   OutcomeSignals   // 给框架的结构化信号
    Thinking  *ThinkingOutcome // thinking 模式的实测落点（Patch 1）
}

type ThinkingOutcome struct {
    ActualLevel    string // 实际生效的档位（含 Normalizer 降级后的值）
    ReasoningField string // 响应字段名（如 "reasoning_content" / "thinking"）
    Exposed        bool   // 是否落 Log / 可见
}

type WireTurn struct {        // 一次 Wire.Execute 的全部产出（Patch 1）
    Outcomes []Outcome
}

type OutcomeSignals struct {
    ErrorClass      ErrorClass
    MalformedOutput bool
    ToolErrorKind   string  // 如 "path_not_found" / "syntax_error"
    NoMutationTurn  bool    // 这一轮无 mutating 技能调用成功
}

type ErrorClass string
const (
    ErrNone              ErrorClass = ""
    ErrTransient         ErrorClass = "transient"
    ErrContextOverflow   ErrorClass = "context_overflow"
    ErrCapability        ErrorClass = "capability_rejected"
    ErrAuthQuota         ErrorClass = "auth_quota"
    ErrContentFilter     ErrorClass = "content_filter"
    ErrMalformed         ErrorClass = "malformed_output"
)
```

升级判据只读 `Signals`，不读厂商响应——这样"证据触发升级"才真的和厂商解耦。

**Usage 的归一化同样关键**：各家缓存字段名和语义不同（命中/写入是否计入 prompt_tokens 就有分歧）。Denormalizer 的产出必须是**已经能直接算钱**的形态，否则 Ledger 里全是口径不一的数字。

### 10.12 Pool 的并发控制

**灵感来源**：Go 的 semaphore + 令牌桶限流

```go
type Pool struct {
    endpoints map[string]*EndpointState
    queue     *PriorityQueue  // 按深度分桶
}

type EndpointState struct {
    Name        string
    MaxInflight int
    RPM         int
    semaphore   chan struct{}   // 并发闸门（长度 = MaxInflight）
    limiter     *rate.Limiter   // 令牌桶（速率 = RPM）
    health      HealthState     // 熔断
}

type HealthState struct {
    Status       HealthStatus  // healthy | degraded | down
    LastError    time.Time
    ErrorCount   int
    CircuitOpen  bool
}
```

**队列纪律：深度优先**。深层 Agent 离完成更近，先放它过去能更快解除其祖先的阻塞，减少"同时被阻塞的 Agent 数"。

伪代码：

```
Execute(req, binding):
  endpoint = pool.endpoints[binding.Endpoint]

  # 排队（阻塞在这里，不是 429 炸掉）
  queue.Enqueue(req, priority=req.AgentDepth)

  # 等槽位
  <-endpoint.semaphore
  defer func() { endpoint.semaphore <- struct{}{} }()

  # 等限流
  endpoint.limiter.Wait(ctx)

  # 调 Wire
  resp, err := wire.Call(req)

  if isTransient(err):
      endpoint.health.RecordError()
      if endpoint.health.ShouldOpenCircuit():
          endpoint.health.CircuitOpen = true
          return ErrAuthQuota  # 让 Router 切到下一级

  return resp
```

熔断按 endpoint 维度，Router 的谓词直接读健康状态——不健康的 endpoint 在**过滤阶段**就被剔掉，而不是绑定后失败重试。

### 10.13 能力探测（probe）

**灵感来源**：Kubernetes 的 liveness/readiness probe

Catalog 里的 `Caps` 是**声明**，声明会错（文档滞后、endpoint 转发丢功能、同名模型不同版本）。所以要三段：

1. **声明**：models.yaml 里的数据，进版本控制
2. **探测**：`marl models probe` 逐项打小请求验证
3. **反证降级**：运行时收到 `CapabilityRejected`，立即把该能力在覆盖文件里置假 + 告警

探测结果存机器级覆盖文件：

```yaml
# ~/.local/state/marl/caps_override.yaml（机器级，不进版本控制）
overrides:
  - model: "deepseek-flash"
    endpoint: "deepseek-main"
    caps_override:
      has: [tool_call, json_mode]  # 实测发现不支持 thinking（或当前版本关了）
    probed_at: "2025-01-15T10:30:00Z"
```

Router 谓词读取时优先用 override，没有才回落到 models.yaml 声明。

探测项（实际发小请求）：

| 能力 | 探测方式 | 通过判据 |
| :-- | :-- | :-- |
| tool_call | 发带 tools 的请求 | 返回合法 tool_call JSON |
| json_mode | `response_format: {type: "json_object"}` | 返回合法 JSON |
| thinking | 开启 thinking，看返回有无 reasoning_content | 字段存在且非空 |
| prefill | 发尾部 assistant 消息 | 续写而非重新开始 |
| cache | 发两次同前缀请求 | 第二次 `cache_read_tokens > 0` |

运行 `marl models probe --all` 大约需要 5-10 个小请求，成本几分钱，跑一次存档供全项目复用。

---

### 10.14 图片附件（Patch 1）

**数据形态**（类型定义在 10.3）：`Segment.Attachments` 上一组 `Attachment`。v1 只做图片：`AttachImage` 完整支持，`AttachPDF` / `AttachFile` 进枚举但 **v1 不实现**。

**存储规则（关键）**：图片 base64 原文**不存 SQLite**，落到文件（`.marl/attachments/`，`[忽略]`，不进版本控制，参照 snapshots 对待），`Attachment.Data` 存文件路径（`SourceFile`）或 URL（`SourceURL`）。Normalizer 发请求时才读文件、按协议翻译。原因：SQLite 不适合存二进制大块，且 base64 原文进 Log 会让 Log 体积失控。

**Normalizer 按协议翻译**：

```text
OpenAI 兼容 → [{"type": "image_url", "image_url": {"url": <data-or-url>, "detail": <low|high|auto>}}]
Gemini      → inline_data（bytes + mimeType）
```

- `Detail` 字段只在模型支持 `CapVisionDetail`（`vision_detail: true`）时下发，否则剥掉并记 Degradation
- 绑定模型**无 `CapVision`** 时：附件降级为文本占位（路径 + 尺寸 + mime），图片不注入上下文，进审计——能力缺失不静默（见 10.10）

**Token 记账**：图片贡献的 token 单列 `TokenUsage.ImageTokens`（Part 3.2），视觉请求的成本独立可见，不混进普通 `PromptTokens`。

**v1 范围**：图片输入完整支持（base64 / URL / 本地文件三种来源）；PDF / 文档附件 v1 不做。

---

### 10.15 每个 Agent 独立缓存桶（Patch 1）

**原则**：每个 Agent 一个独立缓存桶，**不配置策略，不暴露选项**。这是简单的、唯一正确的策略——省掉"缓存粒度"这个配置维度的全部心智负担。

**生成逻辑**：Router 绑定时 `CacheBucket` 的唯一来源就是 AgentID：

```go
func (r *Router) Bind(req Requirement, agentID AgentID) Binding {
    // 谓词过滤 + 打分 ...
    return Binding{
        // ...
        CacheBucket: string(agentID),   // 唯一来源，永不配置
    }
}
```

**Wire 层**：缓存桶通过请求的 `user_id` 字段落地（不是 CachePrefix，它在模型级键里）。

字段名是**实测确认**的：官方 `chat-complete.html` 的请求参数是 `user_id`（字符集
`[a-zA-Z0-9\-_]`、最大长度 512），并且明确写着"`user_id` 可用于 KVCache 缓存隔离，
以进行隐私管理"。设计文档早期写的 `user` 是错的（探测报告 §2 第 2 条）。

```go
type DeepseekChatRequest struct {
    Model    string
    Messages []ChatMessage
    UserID   string    `json:"user_id,omitempty"`  // 永远 = binding.CacheBucket
}
```

**Endpoint 配置简化**：删掉 `bucket_strategy` 字段（它本是往"不同策略"上引的旋钮，现在桶就是 AgentID）。

```yaml
endpoints:
  - name: "deepseek-main"
    base_url: "https://api.deepseek.com"
    key_ref: "env:DEEPSEEK_API_KEY"
    max_inflight: 5
    rpm: 60
```

**同时删除的**：`type BucketStrategy struct { Kind, Value }` 不再需要。

**隐含代价**：父子 Agent 各自的 system + tools 段虽然内容相同但缓存键不同，**N 个 Agent 重复存储 N 次公共段**。这是"正确性优先"的取舍——桶的策略一旦可配，缓存命中率就变成需要到处调优的不确定项。

**已实测否决：全局公共桶**（阶段 1 探测，见 `docs/design/probe-report-phase1.md` §1.1）。
原设想是：tool schema 全项目统一，理论上应该共享缓存，于是 daemon 启动时打"热身请求"
（`user_id="marl-global-schema"`，内容 = system + tools）让公共前缀进公共桶，N 个 Agent 共享。

实测结论：**桶是隔离的**（不同 `user_id` 不共享前缀缓存），方案不成立。证据链（三轮同向）：

- 探测用例 6a/6b：本桶用一个**本轮全新**的前缀（前缀里带每次运行都不同的 nonce，工具强制
  校验"本桶 cached_tokens=0"作为结论可归因的前提），另一个桶发**逐字节相同**的请求 →
  `cached_tokens=0`；
- 官方文档明说 `user_id` 就是用来做 KVCache 缓存隔离的；
- 同一份代码在"另一个桶是全新状态"时的观测与上述一致。

**代价照单接受**：跨 Agent 的 tool schema 缓存独立存储（N 个 Agent 重复存 N 份公共段）。
这是"正确性优先"的既定取舍——桶一旦可配，命中率就变成需要到处调优的不确定项。

**缓存单元的端点间隔（深度轮实测，见探测报告 §3.9 / ADR-0024）**：隐式前缀缓存的单元端点
间隔实测为 **128 token**（截断扫描：`cached` 的台阶 768 → 896 → 1024，跳幅恒为 128），
但 **`cached_tokens` ≠ `floor(prompt_tokens / 128) * 128`**——残差实测最大 250 token。
命中长度取决于厂商**实际落盘了哪些单元端点**（`cache.html` 的三种落盘时机：请求结束位置 /
公共前缀检测 / 按固定 token 间隔），不是均匀网格。

**因此不做"把冻结前缀补齐/裁剪到 128 的倍数"这类优化**：它看起来显然、实测无效，还会为了
一个无效目标去改动冻结前缀的字节（那是全项目缓存的地基）。前缀策略仍按 §10.3：
`frozen` 段 byte-stable、`volatile` 只在尾部、命中率用
`prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` 分开统计。

**留待以后重估**：若某厂商提供跨桶共享（或 `user_id` 语义变化），再评估热身请求；届时的
探测必须沿用"nonce 前缀 + 前提校验"的形态，否则会得到不可归因的结论（同一套用例曾因为
沿用固定前缀而在两轮里给出相反结论）。

这条必须在**阶段 1** 就测（见路线图 13.3）——否则后面测到的命中率都是错误桶口径下的。

---

### 10.16 混合工具调用（WireTurn）（Patch 1）

**问题**：模型在"思考期间调用工具"，在协议层有两种形态：

| 形态 | 协议 | 例子 |
| :-- | :-- | :-- |
| 混合流 | 单次响应混杂 reasoning + tool_call + reasoning | OpenAI o3 Responses API |
| 双流分批 | 返回 reasoning 或 tool_call，客户端处理后再发 | 多数 OpenAI 兼容厂商 |

旧的"一次 Execute 返回一个 Outcome"假设装不下第一种形态（一条响应里多个 reasoning 段、多个工具调用交错出现）。于是每次调用返回一个 **WireTurn**（多 Outcome 序列）：

```go
type WireTurn struct {
    Outcomes []Outcome
}

// Outcome 完整定义见 10.11（含 Entry 落盘形态）；此处列出主循环关心的一组字段
type Outcome struct {
    Reasoning *ReasoningChunk  // 思维链片段
    ToolCalls []ToolCall
    Reply     string
    Usage     *TokenUsage
    Signals   OutcomeSignals
    Thinking  *ThinkingOutcome
}
```

**主循环改造**：

```go
turn := wire.Execute(req)
for _, outcome := range turn.Outcomes {
    if outcome.Reasoning != nil { log.Append(outcome.Reasoning.ToLogEntry()) }
    if outcome.Reply != "" { log.Append(outcome.Reply.ToLogEntry()) }
    if len(outcome.ToolCalls) > 0 {
        results := executeToolsSequentially(outcome.ToolCalls)  // 顺序执行
        log.Append(results...)
        req.AppendToolResults(results)  // 续到下一次 Execute
    }
}
if turnReady(turn) { break } else { continue }
```

**三条约束**：

1. **同一 turn 内所有 Reasoning 共享一个 LogEntry 序列**，`StabilityStable`，编排时按需折叠。Denormalizer 拆出的多条思维链不能各成独立因果单元，它们是同一轮思考的切片。
2. **同一 turn 内 tool 调用顺序执行**（不并行）——后续 tool 常依赖前一个的结果，并行省的那点时间抵消不了错误重试的代价。
3. **中断恢复**：已发生的 Outcome 全部进 Log，重新发起 Execute 只带差量（未完成的 ToolCalls）；用 `trace_id` 跟踪整条 turn 的因果链。

**v1 不做**：推理期间的**人类实时打断**（把 Execute 暴露为流、允许半途插入），留 v2。

---

# Part 11. 人机协作：讨论分支与 Escalation

### 11.1 设计哲学

人类不是"橡皮图章"，也不是"最后求助对象"。人类是**有自主权的参与者**，可以随时插话、可以发起讨论、可以拒绝 Agent 的产出。

关键设计：**人类输入和 Agent 输出在框架里是对等的**——都是消息、都进 Log、都可以被编排、都有血缘。差别只在权限通道（原则 4）：有些路径人类能写 Agent 写不进去。

### 11.2 讨论分支（Discussion Branch）

**灵感来源**：Git 的 topic branch + Pull Request 的 review 流程

有一类工作产物**先于执行、定错了代价很大**：契约、架构决策、接口设计、测试计划。这些值得多花几轮在前面把它搞对，而不是一口气写出来然后人看。

讨论的性质：
1. **隔离**：讨论过程不污染主 Log（主干保持干净）
2. **可丢弃**：讨论不了了之，整个分支可以删掉，不留痕迹
3. **可合并**：讨论出结果，结论合回主干，但过程可以选择性保留或丢弃

这正是 **VCS 的 branch** 语义。不是借喻，是直接复用。

#### 触发讨论的三个入口

**入口 1：Agent 主动申请**（工具调用）

```json
{
  "name": "request_discussion",
  "description": "Request human review/discussion on a draft before finalizing. Creates a discussion branch.",
  "parameters": {
    "topic": {"type": "string"},
    "reason": {"type": "string"}
  }
}
```

框架处理：

```text
1. 在 Fossil 开分支 discuss/<topic-slug>
2. 在控制面创建讨论目录：
   ~/.local/state/marl/<project-id>/discussions/discuss_<ulid>/
     ├── draft.md      ← Agent 写
     └── verdict.md    ← 人类写（不在任何 Agent 的 writable_paths）
3. Agent 进入 Blocked(Discussing)
4. 生成 verdict.md 模板（含每轮随机 nonce）
```

**入口 2：人类主动发起**（CLI）

```bash
marl discuss "重新设计错误处理策略"
```

框架生成讨论分支，打开编辑器让人写第一条，然后通知 Agent 进入讨论模式。

**入口 3：某些操作自动触发**（策略可配）

```yaml
# .marl/config.yaml
auto_discuss:
  - path: ".marl/knowledge/contracts/**"
  - path: ".marl/knowledge/preferences/**"
  - path: ".marl/knowledge/decisions/**"
```

Agent 要写这些位置时，框架自动转成"先开讨论分支、人类审过再合"。

#### 讨论流程

```text
1. 框架开分支 discuss/<topic>，checkout 到该分支

2. Agent 写 draft.md（一次或多次 commit）
   author=agent:<agent_id>

3. 框架重置 verdict.md 模板，通知人类

4. 人类编辑 verdict.md，保存
   可能写：批注 / @approve / @reject / @abandon

5. 框架检测 verdict.md 变化
   - 如果有裁决关键词：执行对应动作
   - 如果只是批注：注入 Agent 的讨论上下文，Agent 响应（修订 draft.md）
   - 重复步骤 3-5

6. 人类写 @approve
   框架：
   a. checkout trunk
   b. 把 draft.md 的最终内容合并到目标位置（如 contracts/oauth-interface.md）
   c. commit（author=human:<username>，message="Finalize <topic> after discussion"）
   d. 讨论分支标记已完成，保留历史（不删）
   e. Agent 状态从 Blocked(Discussing) → Running

7. 主 Log 只记两条：
   - "Agent 发起讨论: <topic>"
   - "讨论结束，结论已落地: <path>"
   中间的来回不进主 Log，在 Fossil 分支历史里可查
```

#### 两个文件的分工

```text
draft.md       Agent 写，可多次修订
               框架每次 commit（author=agent）
               人类只读（查看草稿）

verdict.md     人类写，不在任何 Agent 的 writable_paths
               框架每次 commit（author=human）
               Agent 只读（读上一轮人类的批注，不是当前轮）
```

**关键安全设计**：`verdict.md` 的路径不在任何 Agent 的 namespace 里（`hidden`），Agent 的 `file_read` 会得到"文件不存在"。它调 `shell_exec` 试图 `cat` 这个文件，路径不在项目树（在 `~/.local/state/marl/` 下），shell 的 `workdir` 根本够不着。

即使三层防护都失效（Agent 猜对路径、用绝对路径、shell 沙箱没开），`verdict.md` 的 frontmatter 里有每轮随机 nonce，框架只接受携带**当轮正确 nonce** 的裁决——Agent 读不到 nonce（见上），无法盲写伪造。

#### 超时与可观测性

你定的：讨论超时（默认 24 小时）卡死。

```text
Agent 进入 Blocked(Discussing) → 父进入 Blocked(WaitChildren) → 整条链停摆
```

`marl status` 必须显示：

```text
🤖 root (depth=0)  Blocked(WaitChildren) 3h12m
   等待: 1 个子
 
   └─ 🔧 sub_001 (depth=1)  Blocked(Discussing) 3h08m  ⚠️
        讨论: discuss_01H8X「OAuth 接口契约」
        等你: ~/.local/state/marl/a3f2c1d8/discussions/discuss_01H8X/verdict.md
        超时: 20h52m 后

⚠️ 此子树因 discuss_01H8X 停摆 3h08m，无 LLM 调用产生（未烧钱）
```

最后一行明确告诉你**卡住不花钱**，你可以安心去睡觉。

### 11.3 Escalation（向上求助）

**灵感来源**：Unix 的信号传递 + HTTP 状态码的 5xx 系列

Escalation 是**沿上报链冒泡的求助消息**。它和讨论的区别：

| | 讨论 | Escalation |
| :-- | :-- | :-- |
| 发起方 | Agent 主动（工具调用） | Agent 主动或框架代发 |
| 目标 | 特定议题需审阅 | 一般性求助 |
| 阻塞语义 | 发起者阻塞 | 发起者阻塞 |
| 回复方 | 人类 | 上级 Agent 或人类 |
| 产物 | 落地的文件（contracts） | 回复消息（进 Log） |

Agent 可以主动 escalate（工具调用）：

```json
{
  "name": "escalate",
  "description": "Ask parent or human for help when stuck.",
  "parameters": {
    "reason": {"type": "string"},
    "context_summary": {"type": "string"},
    "question": {"type": "string"}
  }
}
```

框架也会代发 escalation（Agent 崩溃、连续失败、阶梯已到顶仍搞不定）。

#### Escalation 流程

```text
1. 子 Agent 调 escalate 或框架代发
   生成 escalation_<ulid>

2. 框架判断：上报给谁？
   - 如果有父 Agent 且父不在 Blocked：发给父
   - 否则：发给人类信箱

3. 子进入 Blocked(Escalating)

4. 如果发给父：
   父在下一轮编排时看到 escalation（一条特殊 Message）
   父可以：
     a. 自己回复（追加 Message，框架路由给子）
     b. 再 escalate 给上级
     c. 调 request_discussion 发起讨论
 
5. 如果发给人类：
   生成文件信箱：
   ~/.local/state/marl/<project-id>/requests/pending/escalation_<ulid>.md
 
   人类编辑该文件，移到 done/
   框架检测 done/ 有新文件，读取回复，路由给发起者

6. 子收到回复 → unblock，恢复 Running

7. 主 Log 记三条：
   - "Agent escalate: <reason>"
   - "回复来自 <parent|human>: <summary>"
   - 子的下一轮消息
```

#### 回复格式

人类回复不强制格式，自由文本：

```markdown
---
escalation_id: escalation_01H8X
from_agent: sub_003
question: session 存储用 Redis 还是内存？
---

用 Redis，已有实例在 redis://localhost:6379。

配置在 .marl/knowledge/preferences/infrastructure.md 里写了。
```

框架读取、去掉 frontmatter、把正文包成 `RoleEscalationReply` 的 LogEntry，追加到发起者的 Log 并解除阻塞。

### 11.4 人类插话（marl say）

最轻量的人类参与：

```bash
marl say "session 用 Redis"
```

框架处理：

```text
1. 追加到项目 Agent 的 Log
   LogEntry{Role: RoleHumanNote, Content: "session 用 Redis"}

2. 注入项目 Agent 的 View（追加到 Items）

3. 如果项目 Agent 在 Running：下一轮编排自然看到
   如果在 Blocked：记入待处理队列，unblock 后看到
```

这是**异步的**。你说完继续干自己的事，Agent 在下一轮看到并响应。

插话可以带优先级标记（在 Meta 里）：

```bash
marl say --priority high "必须用 Redis"
```

编译上下文时高优先级的 `human_note` 可以用 `<note priority="high">` 包裹，让 LLM 注意到。

### 11.5 文件信箱的生命周期

三个目录：

```text
~/.local/state/marl/<project-id>/requests/
├── pending/      请求等待人类处理
├── done/         人类已回复
└── archived/     已处理完 + 超过 7 天，定期清理
```

框架周期扫描（fsnotify + 10 秒静默期，沿用旧设计）：
- `pending/` 里文件被编辑且移到 `done/` → 读取回复
- `done/` 里文件超过 7 天 → 移到 `archived/`
- `archived/` 里文件超过 30 天 → 删除

### 11.6 与讨论的组合

Escalation 可以触发讨论：

```text
子 Agent escalate "接口设计拿不准"
  ↓
父 Agent 看到，调 request_discussion
  ↓
讨论分支开启，父和人类来回讨论
  ↓
讨论结束，产出契约文件
  ↓
父回复子的 escalation："契约已定，见 contracts/xxx.md"
  ↓
子 unblock，读契约，继续干活
```

这是三个机制的**自然组合**，不需要专门的"escalation → discussion"路径——只是父 Agent 在处理 escalation 时选择了"开讨论"这个工具。

### 11.7 需要记住的三条

**① 人类消息和 Agent 消息在框架里对等**

都是 `LogEntry`、都有 `Prov`、都可以被编排（split / annotate / reorder）。差别只在权限通道。

**② 权限来自通道，不来自内容（原则 4）**

`verdict.md` 的 `@approve` 不是因为框架"识别出这是人类写的"（无法从内容识别），而是因为这个文件的路径不在任何 Agent 的可写范围内——能写它的只有人类（vim）或框架（生成模板）。

**③ 阻塞是便宜的，调用是贵的**

Agent 在 `Blocked(Discussing)` / `Blocked(Escalating)` 期间不发起任何 LLM 调用。卡 3 小时的成本 = 0。这让"等人类"成为经济上可接受的选项，而不是"必须限时否则烧钱"。

---

# Part 12. 知识库与自进化

### 12.1 定位：知识就是仓库里的文件

**一个被砍掉的方案**：早期设计把知识放在 Fossil wiki 里。实测判断后放弃，理由逐条列清，因为这个决策影响 Fossil 在整个框架里的定位。

Fossil wiki 能给的：版本化页面、CLI 读写、内置 Web UI 渲染、随仓库同步、进 timeline。

拿不到的（对 Agent 场景全是硬伤）：

| 缺陷 | 后果 |
| :-- | :-- |
| 命名空间是平的 | 层级只能靠页名里塞 `/` 假装 |
| 无原生结构化元数据 | 想打 tag/status 得在正文里编码（ticket 上已踩过这个坑） |
| 读写是整页粒度 | 改一段要读全文、改、写回，且无 diff 语义 |
| 并发编辑产生 fork | 要自己检测 + escalate，一整套机制 |
| 搜索能力弱 | 和 ripgrep 不在一个量级 |
| 是另一套技能面 | `wiki_read/wiki_write` 和 `file_read/file_write` 功能重叠 |

对照原则"各存储用其所长"——**wiki 正在承担它不擅长的职责**。

**采纳的方案**：知识就是仓库里的普通 Markdown 文件。Fossil 有个被低估的功能——embedded docs，仓库里的文件在 Web UI 的 `/doc/` 路径直接渲染。于是 wiki 想给的效果全都拿到了，而且更好：

| 需求 | wiki | 仓库文件 |
| :-- | :-- | :-- |
| 版本化 | 有 | 有，还有 diff / blame / 分支 / merge |
| 人类浏览 | Web UI | Web UI（`/doc/`）+ 任何编辑器 |
| Agent 读 | 需专用技能 | `file_read`（已有，含 summary 模式） |
| Agent 搜 | 弱搜索 | `file_search`（ripgrep，已有） |
| 结构化元数据 | 无 | frontmatter |
| 并发编辑 | fork，要人工处理 | 命名空间归属，已有机制覆盖 |
| 新增技能面 | 2+ | 0 |

**顺带一句老实话**：wiki 和 ticket 都砍掉后，"为什么是 Fossil 而不是 git"的论证变弱了。剩下的价值是单文件仓库、内置 Web UI/timeline、clone/sync 顺手、零外部依赖。这些仍然成立，但你应该知道这个决策的地基变了：**现在它是"一个自带 UI 的轻量 VCS"，不再是"一体化协作基底"。**

### 12.2 四类知识

```text
.marl/knowledge/
├── index.md              # 人类维护的地图，Agent 的起手读物
├── preferences/          # 长期偏好与约束 → 编译成常驻块
│   ├── code-style.md
│   ├── infrastructure.md
│   └── review-standards.md
├── decisions/            # ADR，编号追加，只增不改
│   ├── 0001-use-fossil.md
│   ├── 0002-single-writer-commit.md
│   └── ...
├── contracts/            # 模块间接口契约，并行子 Agent 的协调面
│   ├── oauth-interface.md
│   └── session-store.md
├── backlog.md            # 没有代码位置的待办
└── vendor/               # 从全局库拉来的知识（见 12.7）
    ├── prompts/
    └── vendor.lock
```

四类的性质完全不同，这决定了它们的注入方式和写权限：

| 类别 | 生命周期 | 谁能写 | 注入方式 |
| :-- | :-- | :-- | :-- |
| `preferences/` | 长期，跨任务 | **只有人类** | 常驻块，每轮都在 |
| `decisions/` | 追加，不改旧的 | 讨论流程 | 按需（`file_search`） |
| `contracts/` | 任务期 | 讨论流程 | 父 fork 时注入给子 |
| `backlog.md` | 流动 | 人类 + Agent | 按需 |

`preferences/` 的写权限是硬约束：它在所有 Agent 的 namespace 里是 `read`，永不 `write`。两层理由——常驻块一改，全项目所有 Agent 的缓存前缀立即失效，代价太大不能让 Agent 随手触发；更重要的是**这里装的是你的长期意志**，Agent 可以在 report 里建议"这条偏好似乎该改"，改不改你说。

### 12.3 三档注入

```text
档 1 · 常驻      preferences/ 编译成 <standing_orders>，紧跟 system prompt
                 每轮都在，属 StabilityFrozen，全项目共享缓存前缀
                 硬上限 1000 est-token

档 2 · 任务级    父 fork 时通过 InjectMessages 塞给子
                 典型内容：相关的 contract、已确立的 decision、人类原始需求
                 属 StabilityStable

档 3 · 按需      Agent 自己 file_search + file_read
                 起手先读 index.md 拿地图
```

三档全部复用已有机制，零新增。

**v1 不做向量检索**。单人项目的知识条目量级是几十到几百，curated `index.md` + frontmatter + ripgrep 完全够。向量库要到"几千条 + 需要模糊语义召回"才回本，现在上是纯负债。

### 12.4 常驻块的四条硬约束

1000 token 上限只有配上"违规时的行为"才是真约束。

**① 上限管的是编译产物，不是源文件。** `preferences/` 下若干文件编译成一个 `<standing_orders>` 块，上限针对这个块。

**② 超限硬失败，绝不自动截断。** 静默截断常驻指令是最坏结果：一条安全约束被无声砍掉，你还以为它在生效。行为是编译时报错，列出各文件的 token 数，要人来砍。

```bash
marl knowledge lint
# 输出：
# preferences/ 编译后 1247 est-token，超出上限 1000
#   code-style.md         512
#   infrastructure.md     398
#   review-standards.md   337
# 请精简至 1000 以内
```

`lint` 让你在编辑时就发现，而不是任务跑到一半炸。

**③ 编译必须逐字节稳定。** 这个块是缓存前缀的一部分，抖一下所有 Agent 的缓存全废。所以：文件按名字排序拼接、换行统一、**不注入任何时间戳或计数器**。时间信息在环境块，位置在常驻块**之后**。

这条要有 golden test 守着：同样的 `preferences/` 内容 → 逐字节相同的编译产物。

**④ 上限的真正作用不是省钱。** 一千 token 的缓存前缀成本可以忽略。它是**强迫你把偏好保持在"你自己还记得住、还敢删"的规模**。超过这个量的规则没人维护得了，会变成谁都不敢动的祖传配置。

### 12.5 契约先行

这一项解决并行分治的核心死结：三个子 Agent 同时写 OAuth 的不同模块，接口定义谁来写？

- 让某个子写 → 其他子依赖它，失去并行
- 让父写在代码里 → 父从"规划者"变成"也要写代码"，且它写的接口没经过审阅

**解法**：父在 fork **之前**先把契约写成 `contracts/` 下的文件，走讨论分支让人类审过，然后 fork 时注入给所有子（`read`，子不可写）。

```text
父 Agent 规划三个子任务
  ↓ 发现有共享接口
父调 request_discussion("OAuth 接口契约")
  ↓ 与人类来回两轮
契约落地 contracts/oauth-interface.md（commit author=human）
  ↓
父 fork 三个子，InjectMessages 含契约文件引用
  ↓
三个子并行开工，各自 import 契约里定义的类型
```

契约文件对所有子是 `read`，只有讨论流程能写。**契约不会被某个子 Agent 顺手改掉**——这是分治正确性的保障。

契约写错了怎么办？父在下一轮修：收到子的 report 发现契约有问题，重新发起讨论修契约，然后重新 fork。这符合"不可变输入 + 追加新一轮"的模式（Part 8.3）。

契约的一个附带收益：它是降低**定向冗余**的主要手段。子 Agent 的定向成本从"读 5 个文件搞清楚现状"降到"读 1 个契约文件"。

### 12.6 待办的归属：代码注释优先

**TODO 写在代码注释里，不建 ticket 表。** 逐条对比：

| | ticket 表 | 代码注释 |
| :-- | :-- | :-- |
| 和代码同步 | 会漂移（代码删了 ticket 还开着） | 不可能漂移，删代码即删 TODO |
| 版本化 | 另一套历史 | 跟 commit 走，`blame` 能查谁写的 |
| Agent 读写 | 需 ticket 技能面 | `file_search` / `file_edit`，已有 |
| 完成后清理 | 要显式关票 + 同步 | diff 里自然消失 |
| 新增机制 | 一堆 | 零 |

约定必须可 grep，且区分归属：

```go
// TODO(agent): 补上 refresh token 过期重试
// TODO(human): 确认 session 存 Redis 还是内存，影响下面的接口签名
// FIXME(agent): 这里的错误被吞了
```

`TODO(agent)` 是 Agent 的工作队列（起手 `file_search "TODO(agent)"`），`TODO(human)` 是给你的，Agent 不许自行决定。

**注释装不下的两类进 `backlog.md`**：还没有代码位置的事项（"要不要评估换掉这个库"、"性能目标定多少"）。它就是一个普通知识文件，扁平清单，条目前缀 `- [ ]`，完成就删（历史在 commit 里）。

**不要给 backlog.md 加 schema**——一加 schema 它就慢慢长回 ticket。

**一条防滥用的机械检查**（已在 Part 9.6 落地）：子 Agent report `success` 时，框架 diff 它 writable_paths 下新增的 `TODO(agent)` 行；存在且 blockers 里没提到，report 降级为 `partial`。这防止"我加了 TODO 标记待完善"然后声称完成——那会让分治退化成一棵 TODO 树。

### 12.7 全局知识库：vendoring + pin

**灵感来源**：go.mod 的 module pin + Nix 的 flake.lock

全局库是一个独立的 Fossil 仓库：

```text
~/.local/share/marl/global.fossil    # XDG_DATA_HOME
```

它里面装什么：跨项目通用的提示词、验证过的契约模板、通用的架构决策。**不装代码、不装运行时状态**（全局库不执行任何任务，纯知识仓库）。

**关键决策：vendor 进项目，不用符号链接。**

符号链接的方案被否掉，理由有两条且都是硬伤：
- 它破坏"clone 项目就能用同一套配置"——链接指向 `~/.local/share/marl/`，clone 到另一台机器就断了
- 全局库一改，所有历史项目的行为**静默变化**——不可复现，这是最糟的失败模式

正确形态：

```text
.marl/knowledge/vendor/
├── prompts/
│   └── go-engineer.md         # 从全局库拉来的副本，进项目版本控制
└── vendor.lock                # 条目 → 全局库 artifact hash
```

`vendor.lock` 的内容形态：

```yaml
entries:
  - path: "prompts/go-engineer.md"
    source: "global"
    artifact: "a3f2c1d8e9b4..."     # Fossil artifact hash
    pulled_at: "2025-01-15T10:30:00Z"
  - path: "contracts/http-handler.md"
    source: "global"
    artifact: "7c4e1a9f2b83..."
```

两个显式命令：

```bash
marl knowledge pull                    # 从全局库按 hash 取文件落地到 vendor/
marl knowledge pull --update prompts/  # 更新到全局库最新版（改 lock）
marl knowledge promote <path>          # 把项目里验证过的知识提交进全局库
```

`pull` 是幂等的：读 `vendor.lock` 里的 hash，从全局库取那个版本的文件内容（`fossil cat` 按版本取单文件），写到 `vendor/`。这让 clone 后 `marl knowledge pull` 能拿到**完全一样**的知识。

**Fossil 的分布式特性在这里真正派上用场**：单文件仓库、clone/pull 顺手、按 artifact hash 取任意历史版本。这比 wiki 有价值得多。

### 12.8 promote：自进化的真实形态

先说清楚一件事，避免自欺：**Agent 不会"学习"。** LLM 的权重不动，没有任何机制让它从这个项目变得更擅长下一个项目。

所谓"自进化"，实际是这条链：

```text
项目里某个提示词/契约/决策被验证有效
  ↓ 人类判断它有跨项目价值
marl knowledge promote .marl/knowledge/contracts/http-handler.md
  ↓ 框架：commit 到 global.fossil，打标签
     author=human:<username>
     tag: contributor=<project-name>, promoted_at=<date>
  ↓ 下一个项目
marl knowledge pull
  ↓ 新项目的 Agent 从第一轮就带着这份知识
```

三个性质让它是**诚实的**进化，而不是玄学：

**① 人类是唯一的准入关口。** Agent 可以在 report 里建议"这个契约模板挺通用，值得 promote"，但它不能自己 promote（`promote` 是 CLI 命令，不是工具）。理由和 `preferences/` 不可写一样——全局库影响所有未来项目，这个权限不该在 Agent 手里。

**② 可审计。** 每个全局条目都有 commit 历史，知道是哪个项目贡献的、什么时候、谁批准的。发现某条知识其实是错的，`fossil timeline` 能查到它的来源。

**③ 可回滚。** 全局库是 Fossil 仓库，promote 错了就 revert。已经 pull 到项目里的副本不受影响（它们锁在 hash 上），下次 `pull --update` 才会拿新版。

全局库可以用分支分层（可选，v1 不强制）：

```text
main            人类审阅过的稳定知识
experimental    刚 promote 上来、还没在第二个项目验证过的
```

项目可以选择订阅哪个分支。这让"验证过两次才进 main"这种保守策略可行。

### 12.9 project-id 与路径映射

控制面目录（`~/.local/state/marl/<project-id>/`）需要一个稳定标识把项目目录映射过去。

**用 Fossil 仓库的初始 commit hash 前 12 位。** 理由：

| 候选 | 问题 |
| :-- | :-- |
| 项目目录路径的 hash | 移动目录就失效 |
| 项目名 | 改名失效，且可能撞名 |
| 随机 UUID 存在项目里 | 又多一个要维护的文件 |
| **初始 commit hash** | 天然唯一、改名/移动都不失效、Fossil 已经有了 |

`marl init` 时算出来，写进 `.marl/config.yaml`（一个只读的派生字段，丢了能重算）。

### 12.10 收缩后的 FossilBackend

砍掉 wiki（6 个方法）、ticket（6 个）、branch 大部分、写缓冲、ticket 自定义字段——接口面小一半以上：

```text
生命周期    InitRepo / OpenRepo / IsOpen / RepoInfo
工作区      Status / Add / Commit / Diff / Revert
读取        Cat(path, version)          ← vendor pull 用
分支        BranchCreate / BranchSwitch / Merge   ← 只有讨论分支和显式实验用
观测        Timeline / StartServer / StopServer
全局同步    Clone / Pull                ← 只有全局知识库用
```

还有两条从 Part 8.4 继承的简化：
- **写互斥和 lockfile 重试基本不需要了**——单写者提交（只有阻塞恢复的父 Agent 提交）保证了 VCS 无并发
- **不需要 per-Agent branch**——分支退回它本来的用途（讨论分支 + 显式实验）

老实原则不变：**只用 Fossil CLI，绝不直接操作它的 SQLite 底层。** 绕过它的一致性保证等于作死。

### 12.11 v1 不做的

| 项 | 为什么不做 | 什么条件下值得做 |
| :-- | :-- | :-- |
| 向量检索 | 几十到几百条知识，ripgrep 够 | 条目上千 + 需要模糊语义召回 |
| 知识条目的自动过期检测 | 需要语义判断"这条还成立吗" | 有了实测数据知道哪类知识易腐 |
| 全局库的多分支订阅 | 单人项目，一个 main 够 | 开始有"实验性知识"需要隔离 |
| Agent 建议 promote 的自动化流程 | 人类判断成本很低（看一眼就知道） | 全局库条目上百，人工筛选变累 |
| `decisions/` 的 ADR 编号自动分配 | 手写四位数不难 | 决策频率高到经常撞号 |

这张表的用途：将来你觉得某处不够用时，先看它在不在这里、当时为什么不做、触发条件到了没有。**没到条件就别加**——每一项都会带来它自己的维护面。

---

# Part 13. 实现路线图

### 13.1 前提假设

你会大量借助现有 coding agent（Cursor / Windsurf / Cline / Aider）做启动，所以不按传统软件工程的"功能模块"分里程碑，而是**按"Agent 能用上它"的程度**分。

关键：**每个里程碑的交付物都是"Agent 能继续往下写"的状态**，不是"给人类演示"的状态。这个视角差异很大——传统路线图会说"M1 实现基础 CLI"，但 coding agent 不需要你先有 CLI 它才能写代码；它需要的是"数据结构 + 一个能跑的循环"。

**Patch 1 对路线图的调整**（四个改动的落地位置）：

| 改动 | 落在哪阶段 | 说明 |
| :-- | :-- | :-- |
| 缓存桶 user_id | **阶段 1** | 探测时就要测（13.3 探测用例 6），否则后面测到的命中率都是错误桶口径下的 |
| Thinking 字符串化 | 阶段 1 | Wire + 探测顺带验证 `thinking_levels` 各档位（13.3） |
| WireTurn 混合流 | 阶段 2 中 | 主循环骨架本来就在做，`execute.go` 直接返回 WireTurn（13.4） |
| 图片 Attachment | 阶段 2 末 | `file_read` 扩展识别图片（13.4 末），完整链路在阶段 2 之后续接 |

### 13.2 阶段 0：地基与合约（纯人类 + LLM 对话）

**产出**：设计文档 + 数据结构定义 + 接口签名

这一步**不写实现**，只把类型、接口、关键常量定死。原因是后续所有阶段的 Agent 都要读这些定义——它们是"合约"，必须人类审过、逐字节稳定。

具体内容：

```text
定义（.go 文件，只有类型和接口，函数体全是 panic("TODO")）：
  - internal/types/     核心类型（LogEntry / ViewItem / Profile / Binding）
  - internal/proto/     消息与意图（Envelope / SpawnRequest / Escalation）
  - internal/store/     存储接口（MessageLog / ContextView / Ledger）
  - internal/wire/      Wire 接口（WireAdapter / Normalizer / Denormalizer）
  - internal/skill/     Skill 接口
  - internal/orchestrate/ 编排操作接口

设计文档：
  - 本文档的 markdown 版本，放 docs/design/
  - 关键决策单独提炼一份 decisions.md（ADR 0001 起）

不写：任何有逻辑的函数体、任何测试、CLI
```

**为什么这一步必须人类主导**：类型定义是后续所有代码的"语言"。如果让 Agent 从零开始写，它会自由发挥——字段名不一致、类型选择随意、接口边界模糊。修正这些要"全局重构"，而 Agent 很难做全局重构（容易漏文件、破坏未读过的代码）。

**交付检查**：`go build ./...` 能过（虽然所有函数都 panic），`go vet` 无警告。这一步结束时项目里有约 1500–2000 行"只有签名"的代码。

### 13.3 阶段 1：Wire 实现 + 模型探测

**目标**：能调通 DeepSeek 的 `openai_chat` 线路，拿到正确的 `TokenUsage`，验证缓存机制。

**为什么这是第一步**：整个框架的经济性建立在"缓存命中率高"之上。如果缓存机制实测不符合假设（如前缀抖动、跨请求不共享、TTL 太短），后面的所有设计都要调整。**越早验证假设越好。**

**阶段 1 已完成**（2026-09-12，**7 轮真跑** = 基础轮 1~4 + 深度轮 5~6 + 复核轮 7）：结论与假设裁决见
`docs/design/probe-report-phase1.md`，逐字节请求体与厂商原始响应见
`probe-run-record-phase1.md`（基础轮）、`probe-run-record-phase1-deep.md`（深度轮，含 10 分钟 TTL）、
`probe-run-record-phase1-contract.md`（契约探测复现）、`probe-run-record-phase1-deep2.md`（复核轮，
同命令重跑：用例 7~10 与 TTL 逐位复现，档位均值序被推翻）。编码审阅见
`docs/design/code-review-phase1.md`。

要点：

- 缓存命中 79.8%（963 输入中 768 命中）、跨进程共享、**TTL ≥ 10 分钟**；
- 桶隔离（`user_id`）→ "全局公共桶"方案否决（ADR-0021）；
- thinking 四档 `none/low/high/max` **都生效**（`none` 恒无 `reasoning_content`）；但档位**强度序
  不稳定**——两次各 3 次的运行均值序相反（轮 5 单调、轮 7 不单调），同档位极差是档位间差值的
  5~18 倍 → `thinking_levels` 只声明**可用性**，不声明强度或成本（ADR-0025）；
- **缓存单元端点间隔 128 token**（台阶跳幅恒为 128），但 `cached ≠ floor(prompt/128)*128`
  → 否决"前缀对齐到单元倍数"的优化（ADR-0024）；
- **跨模型缓存隔离**且两个模型 tokenizer 不同（同一份字节差 53 token）→ 阶梯升级按全量输入估、
  token 估算必须带 model 维度；
- 历史 `reasoning_content`：带 `tools` 时四种回传形态都被接受、回传内容确实进上下文
  （`+92` / `+38` token）；不带 `tools` 时确实被忽略 → **裁决：带 tools 全量回传、不带 tools 不发送**
  （ADR-0023）；
- 厂商 API 已更新（模型名、桶字段、thinking 控制方式、usage 主字段），设计文档相关段落已按实测修正；
- **三条裁决**（ADR-0022 档位唯一真相 = `Binding.Thinking`；ADR-0023 历史思维链策略；
  ADR-0025 档位只声明可用性）已记录，前两条的**落地缺口**（无拷贝规则、无思维链通路）
  列为阶段 2 的头两项任务。

具体任务：

```text
实现：
  - internal/wire/deepseek_chat.go
    实现 WireAdapter 接口（Execute 方法，返回 WireTurn）
    POST https://api.deepseek.com/v1/chat/completions
    请求带 user_id=<binding.CacheBucket>（见 10.15 缓存桶；字段名是 user_id，不是 user）
    解析 usage 字段（主字段 prompt_cache_hit_tokens / prompt_cache_miss_tokens；
      prompt_tokens_details.cached_tokens 是同值别名，两个都读）

  - internal/wire/normalizer.go
    OpenAI 兼容协议的 Normalize 实现
    策略：concat_all / keep_as_is / native_tool_role
    Assert 函数（校验 user/assistant 交替、tool 消息位置）

  - internal/wire/denormalizer.go
    解析 reasoning_content（思维链）→ ReasoningChunk
    解析 tool_calls → Outcome.ToolCalls
    归一化错误码（429 → Transient / context_length_exceeded → ContextOverflow）
    组装 WireTurn（多 Outcome 混合流，见 10.16）

  - cmd/probe/main.go
    发 6 个小请求验证能力：
      1. 普通对话（建立缓存）
      2. 同前缀（验证命中）
      3. 带 tools（验证 tool_call）
      4. 开 thinking（验证 reasoning_content 存在；按 thinking_levels 逐档位验证）
      5. JSON mode（验证 response_format）
      6. 同前缀但不同 user_id=<bucket>（验证缓存桶隔离与共享，见 10.15）
         注：必须用**本轮全新**的前缀（前缀带 nonce），并先确认"本桶 cached_tokens=0"，
         否则上一次运行留下的副本会让结论不可归因（探测报告 §1.1 记录了两次相反结论的教训）
  
    每个请求打印：
      - 发送的消息数组（JSON）
      - 返回的 usage（含缓存字段）
      - 耗时
  
    验证点：
      - 请求 2 的 cached_tokens > 0
      - 跨请求缓存共享（重启进程再发一遍，仍命中）
      - thinking 返回 reasoning_content 字段且非空（每个档位）
      - 请求 6 结果写进报告：不同 user_id 是否共享前缀缓存（决定"全局公共桶"是否可行，10.15）

测试（关键）：
  - TestNormalizerByteStability
    同一输入 → 多次 Normalize → 逐字节比对

  - TestCachePrefix
    两个不同 (model_id, endpoint) 组合 → 不同缓存键
    两个不同 cache_bucket → 不同缓存键（Patch 1 缓存桶维度）

  - TestDenormalizeUsage
    各种 usage JSON → 正确解析成 TokenUsage

不做：
  - 其他厂商（可以留接口，但不实现）
  - Pool / Router（直接硬编码调 deepseek-main）
  - 阶梯升级
  - Profile / namespace
```

**交付检查**（✅ 阶段 1 已通过）：`go run cmd/probe` 输出各次请求的结果，第二次的 `cached_tokens` 大于零（实测 768/963），重启进程再跑一遍仍然命中缓存（用例 1/2 跨进程仍 `cached=768`）。这证明 Deepseek 的隐式前缀缓存按预期工作。请求 6 的观测结果记录在探测报告里（缓存桶口径：**隔离**，全局公共桶方案否决）。

**实测要验证的假设**（写进探测报告；✅/❌/⏳ 为阶段 1 实测结果）：

- ⏳ 缓存键 = 前缀内容，采样参数不在键内（改 temperature 不破缓存）——**未验证**：
  `SamplingParams` 是值类型，`temperature=0` 发不出去（探测报告 §3.2）
- ✅ 缓存跨请求共享（不是单次会话）——跨**进程**也共享：重跑时用例 1/2 仍命中
- ✅ 缓存 TTL > 5 分钟（**深度轮把下界推到 10 分钟**：间隔 10m0s 仍 `cached=768`；上界未测）
- ✅ `prompt_tokens` 的口径：**包含** cached——`prompt_tokens = hit + miss`（三例全等）。
  成本核算必须用 hit/miss 分开算，不能用 `prompt_tokens` 乘单价
- ❌ 缓存桶 `user_id` 口径：不同 `user_id` **不共享**前缀缓存 → 10.15 的"全局公共桶"热身
  请求方案**否决**（隔离是厂商文档承诺的行为）
- ✅（新增）前缀缓存**从第 1 个 token 起算、按单元对齐**：前缀第一个 token 变了就零命中
- ✅（新增）thinking 四档 `none/low/high/max` 都生效（`none` 恒无 `reasoning_content`）；
  **但档位强度序不稳定**：两次各 3 次的运行均值序相反（轮 5 `low` 830 < `high` 955 < `max` 1054；
  轮 7 `low` 775 **>** `high` 725），同档位极差（417~688）是档位间差值的 5~18 倍
  → `thinking_levels` 只声明可用性；单次采样（甚至一次 3 次重复）都会给出相反的假象
  （探测报告 §3.8）
- ✅（深度轮新增）**缓存单元端点间隔 = 128 token**；但 `cached ≠ floor(prompt/128)*128`
  （残差最大 250）→ 不做"前缀对齐到单元倍数"的优化（ADR-0024）
- ❌（深度轮新增）**跨模型缓存共享**：不共享（缓存键含模型名）→ 阶梯升级必然重算前缀；
  且两个模型 tokenizer 不同（同一份字节差 53 token）→ token 估算必须带 model 维度
- ✅（深度轮新增）带 `tools` 时历史 `reasoning_content` **四种回传形态都被接受**，且回传内容
  确实进上下文（`prompt_tokens` `+92`/`+38`）；不带 `tools` 时确实被忽略（`prompt_tokens` 相等）
  → 裁决见 ADR-0023
- ✅（深度轮新增）跨轮重复 `tool_call` id **被厂商接受**（不报 400）→ 断言层必须显式表态
  （给合成 id 加轮次区分度，或写明刻意放过；探测报告 §3.11）

**如果这一步发现缓存假设不成立**（如 TTL 只有 1 分钟、跨请求不共享），立即暂停，重新评估设计——这比写到一半再发现强。

**阶段 1 对设计文档的改动清单**（本轮"审阅设计文档并把变动写入"的产物；每条都可回溯到
`docs/design/probe-report-phase1.md` 的对应小节）：

| 文档位置 | 改动 | 依据 |
| :-- | :-- | :-- |
| §6.3 `SamplingParams` | 加注：思考模式下 `temperature`/`presence_penalty`/`frequency_penalty` **设置不报错也不生效**，`top_p` 被抬升到 ≥0.95（非思考模式恒为 1.0）→ 确定性不能靠采样参数 | 探测报告 §2 第 9/12 条（官方文档） |
| §7.1 / §10.6 阶梯 | 模型名改成现行（`deepseek-flash` / `deepseek-v4-pro`）；thinking 改成档位控制；**加**"换模型 = 缓存全废 + tokenizer 不同"的实测确认 | 探测报告 §2 第 1/3 条、§3.10 |
| §7.7 换模型的缓存失效成本 | 由"预估"升级为"实测确认"（跨模型 `cached=0`） | 探测报告 §3.10 |
| §10.4 模型目录 | 模型名、`thinking_control: level` + 四档；`max_context`/`max_output` 标成**待核实**（文档没给）；补 `ultra`→`max` 等别名不进档位表 | 探测报告 §2 第 1/3/5/13 条、§3.3、§3.7 |
| §10.9 命名策略 | 历史思维链行补：带 tools 全量回传 / 不带 tools 不发送（ADR-0023）+ 落地缺口；prefill 行补"是 Beta 端点" | 探测报告 §2 第 11/14 条、§3.6 |
| §3.7 压缩触发条件 | 补：headroom 必须按**当前绑定模型**的口径算（两个模型 tokenizer 不同） | 探测报告 §3.10 |
| §10.15 缓存桶 | 字段名改 `user_id`；"全局公共桶"整段改为**实测否决**；补缓存单元 128 端点与"不做对齐优化"（ADR-0024） | 探测报告 §1.1、§3.9 |
| §13.3 阶段 1 | 状态改为"7 轮真跑已完成"，补深度轮要点、假设表、复现命令、本改动清单 | 本报告全文 |
| §10.6 档位语义 | 明确"档位只声明可用性，不声明强度/成本"（两次运行均值序相反，ADR-0025） | 探测报告 §3.8 |
| 新增 `probe-run-record-phase1-deep2.md` | 复核轮 7 原始记录（同命令重跑：用例 7~10 与 TTL 逐位复现） | 探测报告 §3.8/§3.9 |
| 新增 `code-review-phase1.md` | 阶段 1 编码审阅记录（3 条必修、已修的工具缺陷、复核确认清单） | 本次审阅 |
| 新增 ADR-0021~0025 | 桶字段与隔离、档位唯一真相、历史思维链策略、缓存单元与不做对齐优化、档位不建模成本 | 探测报告 §1.1、§3.1、§3.6、§3.8、§3.9 |

### 13.4 阶段 2：单 Agent 的最小循环

**目标**：一个 Agent 能接收任务、调一次 LLM、解析 tool_call、调一个技能、再调 LLM，形成闭环。

**不涉及**：fork、压缩、讨论、escalate、阶梯。单 Agent、单任务、固定绑定、手写的初始 View。

具体任务：

```text
实现：
  - internal/agent/agent.go
    Agent 结构体（ID / Depth / View / Binding / 状态）
    Run() 方法（主循环骨架，只处理 Running 状态）
    eventLoop() 方法（调 LLM → 处理 WireTurn → 顺序执行工具 → 循环，见 10.16）

  - internal/agent/execute.go
    compileView() → CanonicalRequest
    调 Wire.Execute → WireTurn
    逐 Outcome 追加 LogEntry 到 Log（Reasoning 共享一个 stable 序列）
    顺序执行工具调用，结果 AppendToolResults 续到下一次 Execute
    更新 View（追加新 assistant 消息）

  - internal/skill/file_read.go
    实现 content 模式（带行号）
    summary 模式先只支持纯文本（返回前 N 行）
    阶段 2 末：识别图片文件（mime / 尺寸），以 Attachment 引用返回（10.14 的落地第一步）

  - internal/skill/file_write.go
    原子写（tmp + rename）
    快照（写前备份到 snapshots/）

  - internal/skill/list_dir.go
    BFS 遍历，depth 限制，exclude 预置列表

  - internal/store/sqlite_log.go
    MessageLog 接口实现
    Append / Get / List
    SQLite schema（log_entries 表）

  - internal/store/sqlite_view.go
    ContextView 接口实现
    Load / Save（JSON blob）

  - cmd/mini/main.go
    构造一个 Agent
    手写 View（一条 user 消息："列出当前目录，读 README.md"）
    调 agent.Run()
    打印 Log

测试：
  - 创建临时目录
  - 放一个 README.md
  - Agent 启动
  - 验证 Log 里有：list_dir 调用 → file_read 调用 → 最终 assistant 回复

不做：
  - Mailbox / 消息路由
  - Profile（硬编码一个配置）
  - 持久化 Agent 状态（每次从头跑）
  - 错误恢复
```

**交付检查**：`go run cmd/mini` 能完成"列目录 + 读文件 + 口头回复"的循环。Log 里能看到完整的 tool_call → tool_result → assistant 链条。SQLite 文件里能 `SELECT * FROM log_entries` 看到记录。

**这一步结束时 Agent 具备的能力**：能理解任务、能调工具、能写文件、能形成闭环。这已经是一个"能干活"的最小原型，虽然没有任何高级特性。

### 13.5 阶段 3：压缩与编排操作

**目标**：上下文满了能触发压缩，生成 SUM 并替换中段。编排操作（split / exclude / reorder）能调通。

**阶段 3 已完成**（2026-09-12）：交付内容与"不做"清单的执行情况见
`docs/test_report/phase3-test-report.md`；落地裁决（SUM 引用语义、"轮"的口径、
L0 达标判据、哨兵、配对不变量、账本落点）见 ADR-0026。要点：

- `internal/orchestrate`：七个编排 Op 全部实现（split 语义/机械、exclude、restore、
  reorder、annotate、pin、unpin），Fractional Index 中点插入 + Stability 单调校验，
  语义拆分的修复链（clamp/去重叠/填缝/禁切区吸附）落地；
- `internal/compress`：OrchestrationCall 执行核（无 tools 最小上下文 + JSON mode +
  独立预算 + 独立账本出口）、SUM 七段骨架生成与机械校验（章节 + 文件路径存在性，
  聚合报错）、L0 机械清理（以（意图,结果）对为单位去重）、压缩主流程与 Admit；
- `internal/agent`：headroom 触发 + Blocked(BlockCompressing) 状态迁移 + 结果采纳，
  编译层改为按 Position 升序（编排操作改写 Position 后的必要配套）；
- `cmd/mini -task files`：读 10 文件凑满上下文 → 触发压缩 → 打印前后 token 估算，
  dry-run 与真跑均通过交付判据（新 View 比旧小 ≥20%、SUM 七章节齐全、压缩后任务
  继续跑完、Log 真相完整）；
- 真机发现的两个缺陷已修复并带回归测试：轮口径（旁白不当轮起点）、L0 配对不变量
  （去重以对为单位 + validatePairing 终检）。

具体任务：

```text
实现：
  - internal/compress/orchestrator.go
    OrchestrationCall 函数（构造最小上下文 + 调 Wire）
    独立预算、独立账本

  - internal/compress/sum.go
    生成 SUM 的 instruction（七段骨架）
    校验 SUM（章节标题、文件路径存在性）

  - internal/compress/split.go
    语义拆分（带行号渲染 + JSON mode）
    delimiter 拆分（机械切）
    禁切区扫描（代码块 / 引用块）

  - internal/agent/compress.go
    checkHeadroom()
    doCompress()（调 Orchestrator → 校验 → 构造新 View）
    L0 机械清理（去重 file_read、exclude 历史 thinking）

  - internal/orchestrate/ops.go
    split_message / exclude_message / reorder_message 的实现
    Fractional Index（Position 字段的插入逻辑）

  - cmd/mini/main.go 改造
    加一个"读 10 个文件"的任务（凑满上下文）
    触发压缩
    打印压缩前后的 token 估算

测试：
  - Agent 上下文塞到接近窗口上限
  - 触发压缩
  - 验证新 View 比旧 View 小至少 20%
  - 验证 SUM 的七个章节都存在
  - 验证拆分后的段落覆盖原文、无重叠

不做：
  - 压缩收益不足时升级到 L2（先硬编码失败）
  - split 的 XML 标注块禁切
```

**交付检查**：Agent 能在上下文满时自动压缩，压缩后继续跑任务。手动调 `split_message` 能把一条长消息拆成多段，`reorder_message` 能调整顺序。

**这一步结束时**：Agent 能管理自己的上下文窗口，不会因为"装不下"而卡死。编排工具可用，虽然还没有"LLM 主动调用它们"的场景。

### 13.6 阶段 4：阶梯与成本账本

**目标**：配置三级阶梯（r0 / r1 / r2），任务能从 r0 起跑，失败证据累积后自动升级到 r1，Ledger 能分项记账。

**阶段 4 已完成**（2026-09-12）：落地裁决见 ADR-0027，测试报告见
`docs/test_report/phase4-5-test-report.md`。要点：

- `internal/ladder`：ladder.yaml 加载（internal/config 的受限 YAML 子集解析器）、
  两阶段调度 Router（谓词过滤 + 打分，startIndex 单调不回退 = 亲和性）、
  证据累积器（权重可配置，ErrTransient 不计入证据）；
- `internal/ledger`：成本公式（Completion−Reasoning 拆分可见输出与思维链）、
  Recorder 三类入口、Part 7.6 报表渲染（含思维链占比与缓存命中列、机械建议）；
- `internal/store`：SQLite ledger_entries / model_switch / audit_events 三表；
- `internal/agent`：每轮证据采集 → 升级判据 → 重绑定（rung_index+1）→
  审计 model_upgrade → Transient 告知 LLM → 清空证据；换 model_id 记
  model_switch（缓存失效审计，数字来自逐轮真实累计）；
- `cmd/mini -task failing -dry-run`：确定性升级场景（两次 BAD_ARGS → r1）；
  `cmd/ladder_report` 输出分项成本；
- "不做"清单执行：Pool 并发闸门 / 熔断与健康检测 / 能力探测均未做
  （CircuitPolicy.Validate 顺手补齐）。

具体任务：

```text
实现：
  - internal/ladder/ladder.go
    加载 ladder.yaml
    Rung 结构体（id / endpoint / model / thinking）

  - internal/ladder/router.go
    两阶段调度（谓词过滤 + 打分）
    绑定亲和性（任务内保持同一 model）

  - internal/ladder/evidence.go
    EvidenceAccumulator（累积失败、格式错误、无进展）
    升级判据（score >= threshold）

  - internal/ledger/ledger.go
    LedgerEntry 结构体
    RecordMain / RecordOrchestration
    成本计算（按 pricing 算 CNY）

  - internal/agent/upgrade.go
    每轮结束后检查 evidence
    触发重绑定（Router.Bind，rung_index + 1）
    记审计（model_upgrade）

  - cmd/ladder_report/main.go
    读 ledger 表
    按阶梯分组
    打印成本报表（格式见 Part 7.6）

测试：
  - 构造一个"tool_call 总是返回格式错误"的任务
  - Agent 从 r0 起跑
  - 两次失败后升级到 r1
  - Ledger 记录两级的 token 消耗
  - 报表显示 r0 的 token 数、r1 的 token 数、总成本

不做：
  - Pool 并发闸门（硬编码单线程）
  - 熔断与健康检测
  - 能力探测（probe）
```

**交付检查**：配置 `ladder.yaml`（r0 = `deepseek-flash` 关 thinking、r1 = `deepseek-flash` 开 thinking、r2 = `deepseek-v4-pro`），`go run cmd/mini` 跑一个会失败的任务，观察到升级日志，`go run cmd/ladder_report` 输出分项成本。

**这一步结束时**：阶梯机制闭环。Ledger 能看出"哪个阶梯花了多少钱"，能验证"升级是否真的省钱"。

### 13.7 阶段 5：Spawner 与单层 fork

**目标**：父 Agent 能 fork 一个子，子完成后 report，父收到 report 继续。

**不涉及**：多个子并行、嵌套 fork（深度 >1）、讨论、escalate。

**阶段 5 已完成**（2026-09-12）：落地裁决见 ADR-0028，测试报告见
`docs/test_report/phase4-5-test-report.md`。要点：

- `internal/spawner`：进程表（扁平）、Adjudicate 全闸（拓扑/权限/全局上限/
  命名空间子集/扇出/注入越界）、buildNamespace（父 write 面对子降级 read）、
  report 机械检查（TODO 扫描 + 否定语境感知的改动声明判据）、框架代报
  （子退出未 report → failed）；
- `internal/types`：Namespace.Subset + PatternCovers（保守拒绝不可证明的
  通配符形态；与 ns.globMatch 交叉一致性测试）；
- `internal/agent`：Mailbox 泵（pump goroutine，ctx/关闭双退出）、
  Blocked(WaitChildren) 等待与恢复、sub_task_result 落 Log+View、
  spawn/report 意图处理、6 个意图工具 schema 进冻结前缀（golden 守护）；
- `cmd/fork_test`：父 fork 子 → 子 report → 父汇总，dry-run 与真跑均过
  交付判据；
- 真机发现的缺陷已修复并带回归测试：SQLITE_BUSY（deferred 事务升级锁，
  _txlock=immediate 修复）、report 否定语境误判、fork 竞态（spawn 与
  pending 检查之间到达的 report 被跳过 flush）。

具体任务：

```text
实现：
  - internal/spawner/spawner.go
    Spawner 结构体（进程表、全局计数器）
    adjudicate()（校验拓扑、命名空间子集）
    bootstrap()（创建根 Agent）

  - internal/spawner/namespace.go
    buildNamespace()（父的挂载点 → 子的挂载点）
    SubsetOf() 检查

  - internal/agent/mailbox.go
    Mailbox = chan Envelope
    Route() 方法（通过 Spawner 路由到目标 Agent）

  - internal/agent/spawn.go
    handleSpawnIntent()（构造 SpawnRequest → 调 Spawner）
    recordChild()（登记到 AgentRuntime.Children）

  - internal/agent/wait.go
    进入 Blocked(WaitChildren)
    收到 MsgChildReport → 记录
    allChildrenReported() → unblock

  - internal/agent/report.go
    report_to_parent 工具
    机械检查（TODO(agent) / 文件改动）
    降级 success → partial

  - cmd/fork_test/main.go
    父任务："列出 src/ 下所有 .go 文件，fork 子去读第一个"
    子任务："读 <file>，总结前 10 行"
    父收 report 后口头总结

测试：
  - 父 fork 子
  - 子写文件（在 writable_paths 内）
  - 子 report success
  - 验证 report 进父的 Log
  - 验证父的 View 里有 sub_task_result

不做：
  - 多个子并行（只测一个子）
  - 深度 >1
  - 子崩溃处理
```

**交付检查**：`go run cmd/fork_test` 能完成父 → 子 → 父的循环。父的 Log 里能看到子的 report。子写的文件存在于文件系统。

**这一步结束时**：分治的基本骨架立住了。虽然只支持单层、单子，但核心机制（意图裁决、命名空间校验、report 机械检查、阻塞/恢复）全都跑通了。

### 13.8 阶段 6：Fossil 集成与单写者提交

**目标**：`marl init` 全自动初始化 Fossil，子 Agent 写文件，父 Agent 阻塞恢复后一次 commit。

**阶段 6 已完成**（2026-09-12）：实测裁决见 ADR-0030，测试报告见
`docs/test_report/phase6-test-report.md`。要点：

- `internal/fossil`：CLI 封装（读写分离 + 超时 + 错误哨兵）、生命周期
  （InitRepo/OpenRepo/IsOpen/CloseRepo，幂等 open、重复 init 拒绝）、
  工作区（Status=changes+extra 合并 / Add / Commit / Timeline / Diff）；
- `internal/agent/commit.go`：doCommit（Status→Add→Commit，author=agent），
  提交点在父的 awaitChildren 恢复之后——单写者纪律的结构保证；
- `cmd/marl init`：fossil init+open + 骨架文件（config/profiles/prompts/
  knowledge）+ 首次 commit（author=system）+ 重复 init 拒绝；
- M6.5（13.14）：5 个子并发 report → 父 commit 不丢文件（`TestM6_5ConcurrentReports`）；
- 真机 fork_test：子写 src/auth/summary.txt → 父 commit → `fossil ls` 可见
  子写的文件、timeline author=agent；
- 真机实测修正了 13.8 的两处假设：lockfile 退避重试**不做**（fossil 内部锁
  已串行化，见 ADR-0030 第 6 条）；runRead 不需要 TTL 缓存（本地命令毫秒级）。

具体任务：

```text
实现：
  - internal/fossil/cli.go
    runWrite()（写互斥 + lockfile 退避重试）
    runRead()（带 TTL 缓存）

  - internal/fossil/lifecycle.go
    InitRepo / OpenRepo / IsOpen

  - internal/fossil/workspace.go
    Status / Add / Commit
    author 字段按调用路径填 human: / agent: / system:

  - internal/agent/commit.go
    doCommit()（Status → Add → Commit）
    只在父阻塞恢复后调用

  - cmd/marl/init.go
    marl init 命令
    fossil init + open
    写 config.yaml / profiles / ignore
    首次 commit（author=system:init）

测试：
  - marl init 在临时目录
  - 启动 Agent
  - fork 子写文件
  - 子 report → 父恢复 → 父 commit
  - fossil timeline 能看到一条 commit（author=agent:root）

不做：
  - branch / merge
  - 讨论分支
  - server 常驻
```

**交付检查**：`marl init` 能跑通，`fossil timeline` 能看到初始 commit。Agent 任务完成后 `fossil timeline` 多了一条 commit，`fossil diff` 能看到子写的文件。

**这一步结束时**：VCS 集成闭环。每次任务完成都有 commit 留痕，timeline 干净（一个任务一条 commit）。单写者提交避免了并发冲突。

### 13.9 阶段 7：Profile 与知识库

**目标**：加载 Profile，注入常驻块，子 fork 时注入契约。

具体任务：

```text
实现：
  - internal/profile/loader.go
    LoadAll()（读 profiles/ 目录）
    继承与合并（Extends 字段）

  - internal/profile/prompt.go
    PromptIndex（读 prompts/_index.yaml）
    ReadPrompt()（返回整个 md 文件）

  - internal/knowledge/standing.go
    编译 preferences/ 成 <standing_orders>
    强制 1000 token 上限（超限报错）
    逐字节稳定性检查

  - internal/agent/context.go
    compileView() 改造：
      加入常驻块（StabilityFrozen）
      加入私有段（深度、namespace、任务描述）

  - cmd/marl/knowledge_lint.go
    marl knowledge lint 命令
    检查 preferences/ 编译后大小

测试：
  - 写一个 preferences/test.md（200 token）
  - 编译成常驻块
  - 验证出现在 system 之后、私有段之前
  - 加到 1200 token → lint 报错

不做：
  - vendor / promote
  - 契约注入（先硬编码一个契约文件路径）
```

**交付检查**：Agent 启动时 `compileView` 产出的 CanonicalRequest 里有常驻块，内容来自 `preferences/`。`marl knowledge lint` 能检测超限。

**这一步结束时**：Profile 与知识库的基础设施到位。Agent 能读提示词、能带常驻块。虽然还不支持父注入契约给子，但机制已经准备好了。

### 13.10 阶段 8：人机协作最小闭环

**目标**：Agent 能调 `request_discussion`，人类编辑 `verdict.md` 写 `@approve`，结论合回主干。

具体任务：

```text
实现：
  - internal/discuss/discuss.go
    开讨论分支（fossil branch new）
    创建讨论目录（控制面）
    生成 verdict.md 模板（含 nonce）

  - internal/discuss/fsnotify.go
    监听 verdict.md 变化
    10 秒静默期
    读取裁决关键词

  - internal/agent/discuss.go
    进入 Blocked(Discussing)
    Agent 写 draft.md（commit author=agent）
    收到批注 → 响应
    收到 @approve → unblock

  - internal/discuss/merge.go
    checkout trunk
    把 draft.md 最终内容合并到目标路径
    commit（author=human）

  - cmd/marl/status.go
    marl status 命令
    显示 Agent 树、阻塞状态、讨论等待提示

测试：
  - Agent 调 request_discussion("测试契约")
  - 写 draft.md
  - 人类编辑 verdict.md 写批注（不写 @approve）
  - Agent 响应
  - 人类写 @approve
  - 验证结论落地到 knowledge/contracts/

不做：
  - escalate
  - request_human 三种形态
  - 超时处理
```

**交付检查**：`marl status` 能看到 Agent 在 `Blocked(Discussing)`。人类写 `@approve` 后，`fossil timeline` 能看到讨论结论的 commit（author=human）。

**这一步结束时**：人机协作的核心闭环打通。虽然只支持讨论一种形式，但阻塞/恢复、文件信箱、权限通道分离全都验证了。

### 13.11 阶段 9：完整拓扑与多层 fork

**目标**：支持深度 >1，支持多个子并行，支持 Watchdog。

具体任务：

```text
实现：
  - internal/spawner/depth.go
    maxDepth 校验
    到顶拒绝 fork + 返回给 LLM

  - internal/agent/wait.go 改造
    支持 await 策略（all / any / n）
    spawn_batch 工具

  - internal/watchdog/watchdog.go
    周期扫描进程表
    检测无进展、超预算、超时
    强制终止 + 框架代报 failed

  - cmd/marl/status.go 改造
    递归显示 Agent 树
    色块标记状态
    显示阻塞链与时长

测试：
  - 三层 fork（根 → A → A1）
  - A1 到顶时调 spawn → 拒绝
  - A1 超预算 → Watchdog 终止 → A 收 failed
  - marl status 显示完整树

不做：
  - Supervisor 三决策（Resume / Restart / Giveup）
```

**交付检查**：能跑一个"根 fork 3 个子，每个子 fork 1 个孙"的任务。`marl status` 显示 6 个 Agent 的树状结构。到顶拒绝 fork 的错误消息正确回传给 LLM。

**这一步结束时**：拓扑完整支持。深度限制、并发 fork、Watchdog 全部到位。这已经是一个功能完整的分治框架。

### 13.12 阶段 10：打磨与补全

剩下的是把各处"不做"清单里的项补上，以及打磨细节：

```text
补全：
  - 能力探测（marl models probe）
  - escalate（沿用讨论分支的阻塞/恢复机制）
  - request_human 三种形态
  - 自动审批规则
  - vendor / promote
  - 契约注入（父 fork 时通过 InjectMessages）
  - reconfigure（含三校验 + 缓存失效审计）
  - Pool 并发闸门与深度优先队列
  - 熔断与健康检测

打磨：
  - 错误信息（candidates_snippet / 拒绝原因）
  - 审计表完整性（所有意图裁决都记）
  - golden test（byte-stability / 缓存前缀不变性）
  - conversation 文件导出
  - marl log / marl attach
  - 文档与示例
```

这些都是"已经有骨架，往里填肉"的工作，coding agent 最擅长。

### 13.13 里程碑检查点总结

| 阶段 | 交付检查 | 通过标准 |
| :-- | :-- | :-- |
| 0 | `go build` 过，接口全定义 | 2000 行签名代码 |
| 1 | `go run cmd/probe` 缓存命中 | 第二次请求 cached_tokens > 0（✅ 已通过：768/963） |
| 2 | `go run cmd/mini` 完成循环 | Log 里有完整 tool 链条 |
| 3 | 触发压缩 | 新 View 比旧小 ≥20%（✅ 已通过：真跑 8 次压缩，收益 25.7%~30.6%） |
| 4 | 升级到 r1 | Ledger 记录两级消耗（✅ 已通过：dry-run 升级场景 r0=2 调用 r1=1 调用，报表分项） |
| 5 | fork 单子 | 父收到 report，子文件存在（✅ 已通过：dry-run + 真跑，sub_task_result 进父 Log） |
| 6 | fossil commit | timeline 有 commit，author 正确（✅ 已通过：真机 fork_test，author=agent，子写文件入库） |
| 7 | 常驻块注入 | CanonicalRequest 含 standing_orders（✅ 已通过：compileView 段序 system→standing→私有段→历史，-race 全绿；lint 超限退出 1） |
| 8 | 讨论闭环 | verdict @approve → 结论落地（✅ 已通过：discuss_loop全流程回放，author=agent 的草稿双版 + author=human 的 Finalize commit） |
| 9 | 三层拓扑 | status 显示 6 节点树（✅ 已通过：topo_test 三层 2×2 收敛 + spawn_batch 部分拒绝回填 + Watchdog 终止悬停子代报 failed + status 色块/时长） |
| 10 | 全功能 | 所有"不做"清单清零（✅ 已通过：models probe 真机 PASS / 熔断+深度优先 Pool / escalate 双通道 / auto_discuss / reconfigure 三校验 / vendor+promote 真机 roundtrip / marl log+attach+conversation 导出 / golden（意图表序 + frozen 前缀）/ 审计全意图覆盖） |

每个阶段都是"能跑的状态"，不是"半成品"。这让 coding agent 能持续验证、持续推进，而不是写到一半发现跑不起来。

### 13.14 两个关键的非功能里程碑

嵌在功能里程碑之间，但必须显式做：

**M2.5：Token 估算器校准**

阶段 2 结束时做。跑 20 个任务，记录每次的 estimated vs actual，计算偏差率，验证本地估算（tiktoken）误差在 ±10% 以内。如果偏差大，调整估算参数或换算法。

这决定了"headroom < threshold 触发压缩"是否准确——估算偏 20% 会导致过早压缩（浪费）或过晚压缩（炸窗口）。

**M6.5：单写者提交的并发验证**

阶段 6 结束时做。写一个测试：父 fork 5 个子，各写不同文件，同时 report。验证父的 commit 不报 lockfile 错误、不丢文件。

虽然设计上单写者不会并发（只有父在阻塞恢复后提交），但要实测确认"5 个子同时 report → 父收齐 → commit"这个路径没有竞态。

---

Part 13 交付。路线图的关键特征：

1. **按"Agent 能用上它"分阶段**，不是按"给人类演示"分。
2. **阶段 1 先验证缓存假设**——这是整个经济性的地基，越早确认越好。
3. **每个阶段都能跑**——不写"实现到一半"的状态，每阶段结束都有可执行的 `cmd/` 程序验证。
4. **"不做"清单显式管理**——每个阶段明确说什么不做，避免范围蔓延。
5. **两个非功能里程碑嵌在中间**——估算器校准和并发验证，必须显式做，不能靠"顺便测一下"。

设计文档（Part 0–13）至此全部完成。13 个 Part，约 3 万字，覆盖从哲学到落地的全部决策。

---

# Patch 1 合并记录

2026-09-11 将 `marl_design_patch1.md` 的全部四个改动合并进本文档。四个改动都是**适配层与数据结构的精化，不动核心骨架**。

| # | 改动 | 落点 |
| :-- | :-- | :-- |
| 1 | 图片输入：`CapVisionDetail` / `CapImageGen`（进枚举 v1 不做）、`Segment.Attachments`、`Attachment`、`TokenUsage.ImageTokens`、Normalizer 协议翻译 | Part 3.2、Part 4.4、Part 6.3、10.3、10.10、10.14 |
| 2 | Thinking 重设计：`ThinkingSpec` 字符串档位（`Level` / `Budget` / `Display`）、`ThinkingControl`、`ModelCaps.ThinkingLevels`、`DegradThinkingLevel`、`ThinkingOutcome`、`normalizeThinking()` | Part 6.3/6.5/6.9、Part 7.6、10.4/10.6/10.10/10.11 |
| 3 | 混合工具调用：`WireTurn`（多 Outcome）、主循环 `handleWireTurn`、同 turn 工具顺序执行、差量续发 | Part 8.8、10.2、10.11、10.16、13.4 |
| 4 | 每 Agent 独立缓存桶：`Binding.CacheBucket`（=AgentID）、请求 `user_id` 字段、删 `BucketStrategy`、`TestCachePrefix` 加缓存桶维度、阶段 1 探测用例 6 | Part 7.7、10.8、10.15、13.3 |

**不触**：Log / View / 命名空间 / 分治 / 讨论 / 编排 / Ledger / 阶梯升级判据。
---

# Part 14. 统一 Actor 模型：人类与 Agent 的交互面统一（阶段 12 实现记录）

**状态：已实现（2026-09-13）。** 本 Part 是设计定稿 + 实现记录；替换
Part 8 中"根 Agent 特殊化"、Part 11 中"Gate 投递机制"、Part 12 中
"marl say 专用注入"的旧描述。设计动机与完整推导见分支结论采纳稿
（14.1 定位：这不是加层，是删特例——人类消息早就是一等 LogEntry、
discussion/escalation 已被 Gate 收编、authorship 早就是编码字段；
剩下的特例只需要一个动作蒸发：让人类进进程表）。

## 14.1 落地面

| 改动 | 实现 |
| :-- | :-- |
| Actor 接口 + CapSet | `internal/actor`（actor.go）：`Actor{ID/Mailbox/Capabilities}` 三方法即全部——View/Binding/Skills 不在核上（纪律 2）；`HumanCaps()` 交互面全量、认知面零值；`AgentCaps(canSpawn)` 永无 `OwnsControlPlane`（纪律 3：不对称是数据） |
| 两条 Mailbox 后端 | `ChannelBackend`（Agent：goroutine channel + 5s 背压超时）与 `FileBackend`（Human：控制面文件 + mtime 轮询静默窗 + nonce 对账）——`MailboxBackend{Deliver/Receive}` 路由一致、传输各异（Part 14.4） |
| HumanActor | `actor.NewHuman`：监督树的根（depth=0，caps 全量），**没有 Executor**——人类自己就是执行器（14.13 的本体论条款） |
| 人类进进程表 | `Spawner.RegisterHuman`（创建裁决的统一入口配套）+ `Spawner.RegisterAgent`（装配级**登记** API：不创建/不启动/不裁决，只给装配自管的 Agent 进程表身份——与被删除的 Bootstrap 创建特例有本质区别） |
| 根特例删除 | `Bootstrap` 删除；项目 Agent 经正常 `Adjudicate`（requester = 人类）；根有父（HumanActor），report/escalation 链条自然终止在拓扑顶端 |
| 消息类型 | `MsgDirect`（收编旧 MsgHumanInput 的 say 注入）、`MsgGateRequest`/`MsgGateReply`（Gate 审批往返）；载荷类型在 `proto/messages.go` |
| Gate 投递重构 | `gate.FileApprover` **删除**；文件编码/nonce/静默窗统一在 `actor.FileBackend`；Manager 产出 need_human **决策**（不再阻塞），裁决经 `ResolveGate` 兑现 grant + 审计 `granted_by`（授权链完整） |
| grants/ 生命周期 | `gate.GrantStore`：permanent 级落盘 `grants/grant_<ulid>.yaml`（frontmatter 含 granted_by/granted_at），装配重启时 `LoadGrants` 回插动态规则——"人类批过的 always 不丢" |
| Spawner 泛化 | requester 查找从统一进程表取；权威检查读 `Capabilities()`（AI = depthGate 的 Profile 谓词，人类 = CapSet.CanSpawn）；子集不变量照常（人类基准 nil = 请求即授权） |
| max_depth 语义 | 只约束 **AI→AI fork**（Part 14.6）：深度记账在注册点映射为 `Process.AIRecursion`（拓扑事实，非权限分支）；人类 d0 → 项目 Agent d1 → …（max_depth=3 = 三层 AI，与旧编号的能力面一致） |
| Watchdog | 扫描含挂起分支：`MarkPending/ClearPending`（discussing / awaiting_gate；escalation 无超时不登记）+ `Config.PendingOverrun`（默认语义 24h）→ `watchdog_pending_overrun` 告警——**告警不是终止**（卡死不烧钱，去留归人） |
| marl start / say | 人类 Actor 的消息发送（cmd/marl/start.go）：start = attached 运行（人类 spawn 项目 Agent → report 回收件箱）；say = MsgDirect 经文件后端的跨进程通道（异步注入，下一轮编排自然看到） |
| ScriptedHuman | `internal/actor/actortest`：channel 后端 + 剧本——四个场景（讨论/审批/escalation/grant）的 CI 验收测试在 `internal/agent/unify_int_test.go` |
| audit 的 actor 字段 | `human:<uid>` / 既有 AgentID 双形态同表（AgentID 是字符串底座）；status 渲染按前缀翻译图标（👤/🤖/🔧——呈现层专用） |

**不动的**（Part 14.12 的承诺兑现）：frozen 前缀经济学、阶梯、命名空间
子集、Log/View、技能表、llm_call、缓存桶、Ledger 结构。

## 14.2 实现取舍（诚实清单）

- **ActorID = types.AgentID 的别名**（偏离文档 14.2 的独立类型定义）：
  既有 Agent 身份（Log 归属 / audit 的 actor 字段 / 缓存桶 / fossil
  author）全以 AgentID 为键，换类型等于全仓替换且无行为收益；可判别性
  由 `human:` 前缀承担（Agent 是"无前缀"），格式契约由构造/校验函数收口。
- **Kind 的读取只有两处**：呈现（status/start 的图标）与拓扑记账
  （注册点把 Kind 映射为 `Process.AIRecursion`，裁决期读进程表事实）。
  权威判断（spawn 许可、控制面写、消息发送）全部读 CapSet。
- **Gate 回执的对账锚点在 FileBackend 的私有记忆**（Deliver 时登记
  requestID → nonce；解析谓词校验）——陈旧回放与伪造凭据被结构性排除。
- **归档先于投递**（收件箱消费的 exactly-once：信封出现在读端的同一
  时刻文件已进 done/——重扫不会二次解析）。
- **escalation 与讨论机制保留**（14.12 改动清单未列）：文件形态是收件箱
  的子树（requests/ / discussions/ / approvals/ + grants/ 同属控制面）。
- **MsgTaskAssign 的 pump 面就位、当前任务载体仍是 SpawnRequest.
  TaskDescription**（Part 8.3 的不可变输入语义不变）——daemon 阶段切换
  载体时类型表已就绪。

## 14.3 验收证据

- `go test ./... -race` 全绿（23 包；`-race` 为默认通过纪律）；
- `internal/agent/unify_int_test.go`：ScriptedHuman 四场景（讨论 / 审批 /
  escalation / grant 全流程）；
- 真机：`marl start -dir <demo> "读 README.md…"` → 项目 Agent（AI d1）
  经正常裁决创建 → report 落 `human:samphi` 的收件箱；
  `marl status` 显示 `👤 human:samphi (depth=0)` 为根的监督树。

---

# Part 14 补遗：真机 dogfooding 的五项修正（阶段 12 收尾，ADR-0033..0035）

2026-09-13 用 `marl` 二进制做了 13 次真实任务运行（N 皇后 + 过度设计 +
2子×2孙拓扑，总花费 ¥0.54），十个发现全部按"编码 vs 设计"归类处理：

1. **limits 即缺省政策**（Part 11.5 的语义澄清）：llm_call 的维度校验
   （次数/token 对 limits）先行——限内 allow、超限 need_human；规则表
   只作加码。旧形态"规则表全落空 → default-deny"叠加"deny 不计数"使
   人审路径永远不可达。
2. **`ErrOutputTruncated` 类 + 格式纠偏重试**（Part 10.11 类表增项）：
   finish=length 的截断与格式能力错误分离；malformed/truncated 注入
   纠偏 Transient + 一轮重试（Run 内上限 3）。顺带修正 Transient 的
   消费边界（轮末只清已编译送达的前缀）。
3. **spawn 的 writable_paths 升为 schema 必填**（意图层三态指针同面
   强制）：漏填从静默降权改为显式拒绝；"漏填不继承"的防提权立场不变。
4. **收件箱的类型化静默窗**（Part 14.4 精确化）：direct=1s（机器单次
   写）、gate=10s（人类就地编辑）——不对称是数据（type 字段为键）。
5. **start 装配补全**：读 `.marl/config.yaml`（limits/gate_rules 项目
   覆盖）、采样来自 profiles（消硬编码 2048 截断源）、讨论/escalation/
   Committer（单写者提交真机触发）/ 编排五件套接线。

**单写者提交的真机形态**（首次在 `marl start` 验证）：子 report 收齐 →
`marl[start-task] 任务完成提交 … (user: agent)` 落 fossil timeline。

---

# Part 15. GUI 服务接口（阶段 14 实现记录，ADR-0036）

GUI 就绪的形态：`marl serve` 对一个项目跑本机守护进程，`internal/server`
承载两层——App（装配 + 操作面：任务生命周期/监督树/事件/对话/账单/
插话/收件箱/审批/讨论/配置/Profile/知识库/自检）与 HTTP（REST JSON +
SSE 事件流）。

四条接口纪律：

1. **事件流 = audit_events 的游标轮询**（`GET /api/v1/events?since=`）——
   系统的旁路真相就是事件流，不建第二套 pub/sub；GUI 与 CLI 同源同序。
2. **人类的"笔"仍是文件**：API 的审批/讨论回复写收件箱文件（与手编
   同一消费路径），权限不因 HTTP 改道（原则 4）。
3. **控制面项目 id = 基名 + 路径哈希**：同名目录项目隔离（测试隔离
   实证修正）。
4. **本机边界**：默认回环；token 可选（MARL_API_TOKEN）；CORS 全开
   （本机页面框架常态）——跨机部署是使用者的安全责任。

配置的写回闭环：internal/config 新增 Emit（块形态规范化发射器，Parse
的镜像；往返语义等价测试在案）；GUI 保存 = 验证（Parse/ParseLimits/
ParseGateRules/ValidateRules）→ 原子写。
