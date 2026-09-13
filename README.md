# Marl

多 Agent 编排框架（Multi-Agent orchestration framework），单二进制 Go 进程。
四条设计原则贯穿全部实现（`docs/design/marl_design.md` Part 0）：

- **Agent 只能表达意图**：fork、讨论、求助、换模型都是提交给框架的意图，框架统一裁决后执行；
- **不可变真相 + 可变投影**：Message Log 与 Fossil 提交是真相之源，删改摘要只动投影（View），永远可重建；
- **能力约束代替提示词惩罚**：越权路径在技能层硬拦截，深度到顶在 Spawner 硬拒绝——不赌 LLM 自觉；
- **权限来自通道**：人类的审批走文件路径 + 写权限校验（verdict/审批文件 Agent 物理不可写），绝不从消息内容里解析授权。

功能面一览：**统一 Actor 模型（人类进进程表，监督树以人类为根——Part 14）**、阶梯经济（r0→r2 证据自动升级）、成本账本全程可见、上下文压缩闭环（SUM 七章 + ≥20% 收益判据）、Fossil 单写者提交（子 Agent 只写文件不碰 VCS）、人机三类文件信箱（讨论 / Escalation / Gate 审批）+ grants/ 落盘、知识库（常驻块 / 契约 / vendor 同步）、三层 fork 拓扑 + Watchdog（含"等待人类"分支的停摆告警）。

## 1. 安装与使用

### 1.1 前置依赖

| 依赖 | 用途 | 必需性 |
| :-- | :-- | :-- |
| Go 1.27+ | 编译与测试 | 必需 |
| [fossil](https://fossil-scm.org) 2.20+ | 任务级版本控制（`fossil version` 能跑即可） | 必需 |
| DeepSeek API Key | 当前唯一接入的 LLM 厂商（openai_chat 协议，隐式前缀缓存） | 联网运行必需 |

### 1.2 编译与测试

```bash
git clone <repo> && cd Marl
go build ./...                 # 全量编译（27 个包）
go test ./... -race            # 全量测试（22 个包带测试；-race 是默认通过纪律）
```

### 1.3 初始化一个 Marl 项目

```bash
go run ./cmd/marl init ./my-marl-project
```

`init` 是幂等的**一次性**操作（重复 init 直接报错，不覆盖历史）。生成内容：

```text
my-marl-project/
├── .marl/
│   ├── project.fossil                    # 任务仓库本体（单写者提交的目标）
│   ├── config.yaml                       # 项目配置（project.ladder_start / max_depth）
│   ├── profiles/_default.yaml            # 默认 Agent 配置
│   ├── prompts/
│   │   ├── _index.yaml                   # 提示词索引（id → 文件路径）
│   │   ├── default.md                    # 默认提示词
│   │   ├── split-boundary.md             # 编排模板：互斥全覆盖的主题边界
│   │   ├── compress-skeleton.md          # 压缩 SUM 七章骨架
│   │   └── summarize.md                  # 通用三句摘要
│   └── knowledge/
│       ├── contracts/                    # 接口契约（讨论结论落地处）
│       ├── decisions/                    # 架构决策
│       └── preferences/                  # 常驻块（编译进冻结前缀）
└── ...（fossil 工作区 = 项目根，首次 commit author=system）
```

> 注意：`init` 不创建 SQLite 数据库。存储文件由运行的工具按 `-db` 参数
> 创建（如 `cmd/mini -db .marl/store.db`），内含 Message Log / Ledger / Audit 三类表。

### 1.4 设定 API Key

```bash
export DEEPSEEK_API_KEY=sk-...
```

只有一个密钥环境变量；所有命令都支持 `-api-key-env` / `-key-env` 换名读取。

### 1.5 跑第一个任务（统一 Actor 面：人类 spawn 项目 Agent）

```bash
cd my-marl-project

# ① 统一面的完整形态：人类 Actor（监督树的根）spawn 项目 Agent——
#    经正常 Spawner 裁决创建，完成后 report 落进人类收件箱
DEEPSEEK_API_KEY=sk-... go run ./cmd/marl start -dir . "读 README.md，用一句话总结这个项目"
go run ./cmd/marl status -db .marl/store.db      # 👤 human:<uid> 是树的根

# ② 人类 → Actor 的直接消息（运行中的 start 在下一轮编排看到）
go run ./cmd/marl say -dir . "补充：总结用中文。"

# ③ 最小闭环真跑（主循环工具；list_dir → file_read → 总结）
go run <marl仓库路径>/cmd/mini -task readme -db .marl/store.db

# ④ 离线回放（不联网、不花钱）：脚本化假 LLM 验证闭环骨架
go run <marl仓库路径>/cmd/mini -task readme -dry-run

# ⑤ 压缩演示：读 10 个文件把上下文撑大 → 压缩闭环触发（收益 ≥20%）
go run <marl仓库路径>/cmd/mini -task files -files 10 -ctx-budget 4500 \
    -ladder <marl仓库路径>/ladder-mini.yaml -db .marl/store.db

# ⑥ 阶梯升级演示（dry-run 专用）：tool_call 连续格式错误 → 证据评分 → 自动升到 r1
go run <marl仓库路径>/cmd/mini -task failing -dry-run -ladder <marl仓库路径>/ladder-mini.yaml
```

`-task` 是任务**类型**（`readme` | `files` | `failing`），不是自由文本——mini 是主循环的
交付检查工具；`marl start` 是统一 Actor 面的真实任务入口（自由文本）。
人类收件箱在控制面（`~/.local/state/marl/<项目名>/inbox/`）：Gate 审批、
项目 Agent 的 report、`marl say` 的直接消息都在这里。

### 1.6 观测面

```bash
# 监督树（人类为根）+ 阻塞状态 + 讨论等待（从 audit_events 还原，只读）
go run ./cmd/marl status -db .marl/store.db -color always

# 对话导出 / tail
go run ./cmd/marl log -db .marl/store.db                      # stdout 全量
go run ./cmd/marl log -db .marl/store.db -out conversation.md # 导出 Markdown
go run ./cmd/marl attach -db .marl/store.db                   # 2s 轮询 tail，Ctrl-C 退出

# 阶梯成本报表（按 rung / call_type 分项，附换模型记录）
go run ./cmd/ladder_report -db .marl/store.db -task start-task
```

### 1.7 交付检查命令（阶段验收骨架，全部可重放）

```bash
go run ./cmd/topo_test              # 三层拓扑：根 batch fork 3 子 × 各 1 孙，MAX_DEPTH 演示
go run ./cmd/topo_test -watchdog    # 追加：Watchdog 强杀悬停子 → 框架代报 failed
go run ./cmd/fork_test -dry-run     # 单层 fork + report 回传（真跑去掉 -dry-run）
go run ./cmd/discuss_loop -keep     # 讨论闭环全流程回放（真实 fossil，不联网）
go run ./cmd/probe                  # wire 三件套的契约探针
```

## 2. 完整案例：把一份资料库读通并沉淀成契约

以下案例把**每个能力**都走到一遍，每步附"发生了什么"，可直接当验收清单执行。
前提：`fossil` 在 PATH 中，`DEEPSEEK_API_KEY` 已设置。

### 准备：建项目

```bash
go run ./cmd/marl init ~/demo && cd ~/demo
MARL_REPO=<marl仓库绝对路径>
printf 'demo 项目：给 Marl 的完整案例提供一点真实素材。\n' > README.md
```

### 步骤 1 · 知识库常驻块（preferences → 冻结前缀）

把长期偏好写进 `preferences/`（编译成 `<standing_orders>` 进冻结前缀，
**上限 1000 est-token**，超限在编辑期被 lint 拦住而不是任务跑到一半）：

```bash
cat > .marl/knowledge/preferences/code-style.md <<'EOF'
- 总结用中文，不超过三句话
- 文件路径一律用工作区相对路径
EOF
go run $MARL_REPO/cmd/marl knowledge lint -dir .
```

输出 `OK：<n> est-token（上限 1000）`；超限则退出码 1 + 逐文件报告。

### 步骤 2 · 主循环真跑（tool_call 闭环 + Log 落库）

```bash
# 统一 Actor 面的真实入口：人类 Actor spawn 项目 Agent（AI 第 1 层），
# 完成后 report 落人类收件箱；账本记 call_type=main 的分项。
go run $MARL_REPO/cmd/marl start -dir . "读 README.md，用一句话总结这个项目"
```

发生的事：人类 Actor（`human:<uid>`，caps 全量）登记为监督树根 → 项目
Agent 经**正常 Spawner 裁决**创建（requester = 人类——旧 bootstrap 特例
已删除）→ 主循环 `LLM → tool_call → 技能执行 → tool_result → LLM` →
report 投给人类收件箱。可验证落库与树：

```bash
go run $MARL_REPO/cmd/marl status -db .marl/store.db   # 👤 human:<uid> 为根
sqlite3 .marl/store.db "SELECT seq, role, substr(content,1,80) FROM log_entries LIMIT 10;"
```

### 步骤 3 · 压缩闭环 + 阶梯 + 账本（一次跑齐三件套）

```bash
cp $MARL_REPO/ladder-mini.yaml .
go run $MARL_REPO/cmd/mini -task files -files 10 -ctx-budget 4500 \
    -ladder ladder-mini.yaml -db .marl/store.db
go run $MARL_REPO/cmd/ladder_report -db .marl/store.db -task mini-task
```

发生的事：

1. 10 个文件逐个 `file_read`（每轮只读一个，把轮数撑起来）；
2. 有效窗口 4500 est-token 被逼近 → **压缩触发**：head-room 检查 → 七章节 SUM
   机械校验（缺章即弃）→ 新 View 比旧 View 小 ≥20% 才被采纳；Log（真相）不动；
3. 阶梯从 `r0` 起跑（`ladder-mini.yaml` 的三档 r0/r1/r2 + pricing 计价四项）；
4. `ladder_report` 按 rung 分项打印 token 与成本，附升级记录。

### 步骤 4 · 阶梯自动升级（dry-run 确定性回放）

```bash
go run $MARL_REPO/cmd/mini -task failing -dry-run -ladder ladder-mini.yaml
```

tool_call 总是返回格式错误 → 证据评分（格式错误权重 0.4）越过阈值 →
审计事件 `model_upgrade`，绑定切到 r1（缓存前缀失效是显式记录的代价）。
报表里能看到两级各自的消耗。

### 步骤 5 · 三层拓扑 + Watchdog

```bash
go run $MARL_REPO/cmd/topo_test -watchdog
```

发生的事：根 `spawn_batch` 一次 fork 3 个子（await=all）→ 每个子再 fork
1 个孙 → 孙到 `MaxDepth` 再 fork 被拒（`MAX_DEPTH_REACHED` 如实回传 LLM，
措辞提示"请直接执行"）→ 全部 report 沿树收敛；追加的 Watchdog 终止一个
悬停子 → 框架代报 failed → 父收到失败报告。命名空间纪律：子的
`writable_paths` 是父的子集，越权写被技能层硬拒（`NAMESPACE_EXCEEDED`）。

### 步骤 6 · 人机讨论闭环（真实 fossil 分支）

```bash
go run $MARL_REPO/cmd/discuss_loop -keep
```

发生的事（`-keep` 保留工作区供人工检查）：

1. Agent 调 `request_discussion(topic, draft)` → 框架开 fossil 分支
   `discuss_<ulid>`，草稿落盘，Agent 进入 `Blocked(Discussing)`（不烧钱）；
2. "人类"（脚本）编辑控制面 `verdict.md` 写批注（该文件不在任何 Agent 的
   可写路径里——权限来自通道）；
3. Agent 收批注 → 修订草稿再发一轮；
4. "人类"写 `@approve` → 框架 checkout trunk，草稿终版写入
   `.marl/knowledge/contracts/<slug>.md`，commit **author=human**；
5. `marl status` 能看到阻塞记录，timeline 能看到 author=human 的 Finalize commit。

### 步骤 7 · 知识 vendor 与模型探活

```bash
# 把验证过的契约提升进全局知识库（fossil 仓库；路径相对 .marl/，
# flag 要写在位置参数之前）
go run $MARL_REPO/cmd/marl knowledge promote -dir . -global <全局库路径> knowledge/contracts/<slug>.md

# 其它项目拉取（lock 锁 artifact hash，重复 pull 逐字节幂等）
go run $MARL_REPO/cmd/marl knowledge pull -dir . -global <全局库路径>

# 三判据探活：chat / tool_call / cache（二次同文 cached_tokens>0）
go run $MARL_REPO/cmd/marl models probe -models deepseek-flash,deepseek-v4-pro
```

> 全局库要求注册 `human` / `agent` / `system` 三个 fossil 用户（promote
> 以 author=human 提交）。直接用 `marl init` 初始化一个专用目录、取它的
> `.marl/project.fossil` 当全局库即可——用户注册语义现成；裸 `fossil init`
> 不会注册这三个用户。重复 promote 内容未变的文件是幂等成功（不新提交）。

### 步骤 8 · 引擎级能力清单（测试已证明，装配面自行接线）

以下能力在引擎层完整可运行（下表右列是对应回归测试，全部在
`go test ./... -race` 的通过范围内），但 `cmd/mini` 的装配未包含它们——
按 §3 配置后在你的装配代码里传入对应 `agent.Config` 字段即启用
（`cmd/marl start` 是含 llm_call + Gate + grants 的完整装配示例）：

| 能力 | 用法 | 证明测试 |
| :-- | :-- | :-- |
| llm_call sidecar | Agent 的受控副调用：无工具、不占深度、同缓存桶、进 Gate | `TestLLMCallHappy` `TestLLMCallInputTooLarge` `TestLLMCallWireNotFound` `TestLLMCallModelNotInCatalog` |
| Gate 审批（信封化） | need_human → MsgGateRequest → 人类收件箱 → MsgGateReply → grant 兑现 | `TestLLMCallGatePendingGrant` `TestScriptedHumanGateApproval` |
| grant 生命周期 | once/count/tokens 进 session；always 插动态规则 + grants/ 落盘（重启回插） | `TestResolveGateAlwaysPersisted` `TestScriptedHumanGrantAlways` |
| View 编排五件套 | exclude / restore / reorder / annotate / pin（pin 零破坏） | `internal/orchestrate` 全套 |
| 缓存破坏分级 | 低破坏直行；头部排除致大半上下文报废 → need_human | `internal/orchestrate` + gate |
| Escalation | 有父走父信箱；无父（根）→ 人类收件箱——链条终止在拓扑顶端 | `TestManagerRoutesToParent` `TestScriptedHumanEscalation` |
| 人类为根的监督树 | RegisterHuman（depth=0，caps 全量）+ 项目 Agent 经正常裁决 | `TestScriptedHuman*` 四场景 + `TestAdjudicateApprove` |
| 挂起停摆告警 | Watchdog 扫描 discussing / awaiting_gate 分支（24h 阈值告警不终止） | `TestPendingOverrunAlerts` |
| 换模型热重配 | `ReconfigureRequest` 经 Mailbox 能力校验 | `internal/agent/reconfigure_test.go` |

## 3. 配置与修改

Marl 的配置分三层：**项目级**（`.marl/` 内，进 fossil 版本控制）、
**运行参数**（命令行 flag）、**引擎装配**（Go 代码里的 `agent.Config`）。
解析器是刻意实现的 YAML 子集（`internal/config/yaml.go`）：禁 Tab 缩进、
禁锚点/多文档，未知键直接报错——笔误在加载期失败，不静默。

### 3.1 项目配置 `.marl/config.yaml`

```yaml
project:
  ladder_start: "r0"   # 起始阶梯级
  max_depth: 3         # fork 深度上限（Spawner 硬闸）

# 经济属性：多少算多（缺省值见 internal/config/gates.go 的 DefaultLimits）
limits:
  llm_call:
    max_input_tokens: 8000        # sidecar 单次输入上限（硬拒）
    max_output_tokens: 2000
    task_max_calls: 20            # 任务内次数（超限 → Gate 问人）
    task_max_tokens: 100000
    timeout_ms: 120000
  orchestration:
    cache_review_pct: 10          # 缓存破坏率阈值（两级分档）

# 部署属性：配什么 sidecar（name: main 是活引用，指向当前阶梯绑定，不可声明）
llm_wires:
  - name: sidecar-cheap
    adapter: deepseek
    endpoint: deepseek-main
    model: deepseek-flash
    thinking: "off"

# 政策属性：什么放行什么问人（顺序匹配、首中生效）
gate_rules:
  - id: allow-low-destruction
    match: {kind: orchestration, cache_destroyed_pct: "<10"}
    action: allow
  - id: review-destructive
    match: {kind: orchestration}
    action: need_human
    reason: "破坏超过阈值，需要人审阅上下文操作"
  - id: allow-sidecar-cheap
    match: {kind: llm_call, wire: sidecar-cheap}
    action: allow
```

改完即生效面：`gate_rules` 是运行期每请求匹配；`limits` 在装配期物化
（`MaterializeDefaults` 补缺省后校验，任何一项 ≤0 拒绝启动）。

### 3.2 阶梯与计价 `ladder.yaml`

`mini -ladder` 传入的形态（`internal/ladder` 解析；ADR-0029：计价单一来源）：

```yaml
ladder:
  - id: "r0"
    endpoint: "deepseek-main"
    model: "deepseek-flash"
    thinking: {level: "off"}
    description: "快速便宜"
    cost_per_mtok: 1.5
    currency: "CNY"
  - id: "r1"                       # 同模型开思维，缓存部分保留
    ...
  - id: "r2"
    model: "deepseek-v4-pro"       # 更强模型，攻坚
    ...
pricing:                           # 按 model 四项分价（账本的来源）
  - model: "deepseek-flash"
    in_per_mtok: 1.0
    cached_in_per_mtok: 0.25
    out_per_mtok: 2.0
    reasoning_per_mtok: 2.0
    currency: "CNY"
start: "r0"
```

DeepSeek 官方价随时间变化——**改价格只改这个文件**，代码里没有第二张价格表。

### 3.3 Agent 配置 `.marl/profiles/*.yaml`

```yaml
profile:
  id: coder
  extends: base          # 标量子覆盖；require/allowed 并集去重
  prompt: go-engineer    # prompts/_index.yaml 里的 id
  requirement:
    require: [tool_call]          # 路由的硬性能力
    prefer: [thinking, json_mode] # 同价可用则优先
  sampling:
    temperature: 0.2
    max_tokens: 8000
    timeout_ms: 1800000
  allowed_skills: [list_dir, file_read, file_write]
    # 空 = 全部允许；白名单在调用时校验，不影响工具表（缓存前缀稳定）
  can_spawn: true         # false = Spawner 直接拒其 fork 意图
```

### 3.4 提示词与编排模板 `.marl/prompts/`

`_index.yaml` 声明 `id → 文件路径`，框架返回整个 Markdown 不解析内容。
**改文件就是改语义**：想换"拆分"的策略，编辑 `split-boundary.md` 即可，
框架与代码零改动。新增模板 = 加文件 + 在 `_index.yaml` 登记一行。

### 3.5 知识库 `.marl/knowledge/`

```bash
go run ./cmd/marl knowledge lint                      # preferences 编译 ≤1000 est-token
go run ./cmd/marl knowledge promote -dir . -global <全局库> knowledge/contracts/<file>.md
go run ./cmd/marl knowledge pull -dir . -global <全局库>   # vendor/ + vendor.lock，幂等
```

`promote` 的路径相对 `.marl/`；全局库用 `marl init` 初始化（需要框架用户，
见 §2 步骤 7）。`preferences/` 只有人类能写（它是缓存前缀的一部分，Agent 改它 = 每次请求
缓存全冷 + 提示注入面）；`contracts/`、`decisions/` 由讨论闭环的
Finalize 写入（author=human）。

## 4. 扩展指南

### 4.1 加新模型（改数据，零代码）

模型差异是配置不是代码（设计文档 Part 10.4）。以接入 `deepseek-v4-pro` 之外
的新模型为例：

1. 在 `ladder.yaml`（或 `ladder-mini.yaml`）加一个 rung，`model:` 填模型的
   **内部 id**——与厂商现行名不同才需要额外字段，内部 id 直接用厂商现行名
   （ADR-0031：消灭翻译间接层）；
2. 在同文件 `pricing:` 节给新模型登记四项单价——没有计价的模型在账本里不可见；
3. 代码侧目录注册（`cmd/mini` 的 `buildCatalog` 或你的装配代码）需要带上
   该模型的能力表 `ModelCaps`：`MaxContext` / `MaxOutput` / `ThinkingLevels` /
   `Has`（tool_call、json_mode 等）。能力表打错会在**路由期**报错而不是
   请求 400——这是设计立场：路由到不支持的模型是错误的源头；
4. 探活验证（三判据 + 与能力表对照）：

   ```bash
   go run ./cmd/marl models probe -base-url https://api.deepseek.com/v1 \
       -key-env DEEPSEEK_API_KEY -models <新模型id>
   ```

### 4.2 加新技能（给 Agent 新的手）

技能是只读写外部世界的原子能力（Part 4.3），不碰 VCS、不做裁决。
四步：

1. **对号入座**：名字必须在 `internal/skill/names.go` 的 20 名冻结清单里
   （`SkillCount = 20` 有编译期断言）。清单顺序 = 工具表字节序 = 跨 Agent
   共享的缓存前缀，**加新名 = 数组扩容 + 评估全项目一次冷启动的成本**，
   不允许"顺手加一个"。已实现：list_dir / file_read / file_write +
   编排五件套；其余名字保留位（参考实现在 `internal/skill/old_skills/`，
   `//go:build ignore` 的存档）。
2. **实现接口**（`internal/skill/skill.go`）：

   ```go
   type myFetch struct{}

   func (myFetch) Name() string        { return SkillWebFetch }   // 冻结名
   func (myFetch) Description() string { return "Fetch a URL. Read-only." }
   func (myFetch) Kind() SkillKind     { return SkillReadOnly }   // 只有 read-only 允许并发

   func (myFetch) Parameters() json.RawMessage {
       // schema 字节进工具表后冻结（缓存前缀），改动 = 全项目缓存失效
       return json.RawMessage(`{...}`)
   }

   func (myFetch) Execute(ctx context.Context, args map[string]any, env *SkillEnv) (*SkillResult, error) {
       if env == nil || env.Resolver == nil {
           return nil, fmt.Errorf("web_fetch: 框架环境缺失（Resolver 为 nil 是装配 bug）")
       }
       // 路径类操作必须过 env.Resolver 沙箱，不能直接 syscall；
       // 返回 SkillResult{OK:false, ErrorType:<机器可读错误码>}，不裸返回错误。
   }
   ```

   契约要点：`SkillResult` 的 OK/ErrorType 互补不变量；只读技能可并发
   （无状态），mutating 技能在 eventLoop 串行面执行；写前快照走
   `env.Snapshots`（nil 时跳过不报错）。
3. **注册**：在装配代码的 `reg.Register(sk)` 清单里加入（mini 见
   `cmd/mini/main.go` 技能层一段）。重名、越清单名、非法 Kind 都会在
   Register 处 fail-fast。
4. **回归测试**：schema 写 golden（字节级稳定）；执行面写表驱动测试；
   最有价值的技能测试是走 `spawner.reportCheck` 的"子 Agent 声称成功但
   机械检查失败"面（参考 `internal/spawner/spawner_test.go`）。

### 4.3 加新厂商线路（wire）

内部仅实现 `openai_chat`（DeepSeek 现行形态）。新厂商 = 三件套（Part 10.2）：

1. `types.WireID` 加常量并录入 `Valid()` 表；
2. `internal/wire/` 实现 Normalizer（Address 段 → 协议消息）、
   WireAdapter（协议编码 + HTTP + HealthCheck）、Denormalizer（响应块 →
   WireTurn，错误体分类复用 ErrorClass）；
3. 模型目录的 `wire:` 字段填新 id，装配处把线路闭包换新三件套。

屏障在接口层：新厂商不需要碰 Agent / Spawner / 讨论——历史上有过完整
实现后被移除的先例（git log 搜 `internal/wire/anthropic`）。

## 5. 文档与测试地图

- `docs/design/marl_design.md`：设计正文（Part 0–13 + Part 14 统一 Actor 模型实现记录）；
- `docs/design/decisions.md`：ADR-0001..0032（每条附实测依据）；
- `docs/test_report/phaseN-test-report.md`：各阶段真机测试记录；
- `docs/examples.md`：命令速查；
- `docs/deepseek-api/`：厂商文档快照（ADR 引用的证据基底）。

测试纪律：`go test ./... -race` 默认通过条件；golden 测试守护工具表与
SUM 的字节稳定；统一 Actor 面的验收是 ScriptedHuman 四场景（讨论/审批/
escalation/grant——`internal/agent/unify_int_test.go`，不依赖真人、UI、
文件静默期）；并发包（store / discuss / watchdog）有专门的并发与
nonce 误配测试。

---

## 面向贡献者的补充

- 每阶段交付先出契约层与测试骨架，确认后补实现（顺序不可倒置）；
- 导出符号的注释以符号名开头（godoc 惯例），注释即契约；
- 性能声明必须附基准方法，或显式标注 `[待验证]`。
