# Marl 示例与命令面（阶段 10 的文档补全）

本文是命令面的示例速查；设计文档在 `docs/design/marl_design.md`，测试
报告在 `docs/test_report/`。

## 建立一个项目

```bash
marl init <project-dir>          # fossil init/open + 骨架（阶段 6）
```

骨架内容：`.marl/config.yaml`、`.marl/profiles/_default.yaml`、
`.marl/prompts/_index.yaml`、`.marl/knowledge/{contracts,decisions,preferences}/`。

## 阶段 7 · Profile 与知识库

```bash
# profiles 的继承合并与提示词索引（Part 6 = internal/profile）
#   .marl/profiles/*.yaml  —— id / extends / prompt / requirement
#   .marl/prompts/_index.yaml —— id → 文件路径（框架返回整个 md，不解析）

# preferences/ 编译成 <standing_orders> 进入 frozen 前缀（Part 12.3 档 1）。
marl knowledge lint             # 编译产物 > 1000 est-token → 退出 1 + 逐文件报告
```

## 阶段 8 · 讨论闭环

Agent 调 `request_discussion(topic, draft)` 后：

```text
讨论分支 discuss_<id> 被创建，draft.md 落 .marl/discussions/<id>/draft.md
verdict.md 模板（含当轮 nonce）落控制面 discussions/<id>/verdict.md
Agent 进入 Blocked(Discussing)——不烧钱
```

人写批注 → Agent 恢复（批注入 Log）→ 可以再调
`request_discussion` 修订草稿 → 人写 `@approve` → 框架 checkout trunk、
把草稿最终版写进目标路径（缺省 `.marl/knowledge/contracts/`）、
commit **author=human**。

## 阶段 9 · 拓扑与 Watchdog

```bash
go run ./cmd/topo_test            # 三层拓扑（根 batch fork 3 子 × 各 1 孙）
go run ./cmd/topo_test -watchdog  # Watchdog 终止悬停子 → 框架代报 failed
marl status -db <store.db> -color always   # 树 + 色块 + 阻塞时长
```

`spawn_batch` 的 `await` 支持 all / any / n（策略单次生效后回退 all，
语义见 internal/agent/wait.go 的头顶注释）。

## 阶段 10 · 全量面

### 模型
```bash
marl models probe -base-url https://api.deepseek.com/v1 \
    -key-env DEEPSEEK_API_KEY -models deepseek-flash,deepseek-v4-pro
# 判据：chat / tool_call / cache（二次同文 cached_tokens>0）。
```

### 知识库 vendor / promote
```bash
marl knowledge promote contracts/http-handler.md   # 项目文件 → 全局库（author=human）
marl knowledge pull                                # 全局库 → .marl/knowledge/vendor/（lock 锁 hash）
```

### Log / 导出
```bash
marl log -db .marl/store.db -agent root-1            # stdout 的 conversation
marl log -db .marl/store.db -out conversation.md     # 导出文件（Part 1.4 格式）
marl attach -db .marl/store.db                       # 2s 轮询 tail（Ctrl-C 退出）
```

### Escalation（Part 11.3）
```text
Agent 调 request_human → 路由（有父走父信箱封；否则人类文件信箱
  ~/.local/state/marl/<project>/requests/pending/escalation_<ulid>.md）
→ Blocked(Escalating)
→ 把文件移到 done/（回复正文写在"## 回复"之后）→ Agent 恢复。
```

## 线路（Wire）

两种线路可用（Part 10.2）：

- `openai_chat`（DeepSeek 现行形态；隐式前缀缓存）
- `anthropic_messages`（顶层 system、tool_use/tool_result 块、
  thinking 块 + budget 控制；测试面含 httptest 端到端回放）

并发面（Part 9.8/10.12）：`wire.PoolImpl`（深度优先队列 + MaxInflight
+ RPM 令牌桶 + 连续失败熔断 + 健康快照），通过 `wire.NewPool(cfg,
circuit, caller)` 装配——caller 是 Normalizer/Adapter/Denormalizer 的
折算闭包。

## 错误码与拒绝语义

- `MAX_DEPTH_REACHED`：到顶 fork 被拒（措辞指出"请直接执行任务"）
- `NAMESPACE_EXCEEDED`：越权 writable（错误文本列**你只能分配**哪些——
  Part 9.4 的措辞纪律）
- `PATH_READONLY` / `ENOENT`：命名空间与隐藏面的常规拒绝
- `SPAWN_ALL_REJECTED`：spawn_batch 全拒（逐项 JSON 回填）
- `SKILL_NOT_ALLOWED`：AllowedSkills 白名单的调用时拒绝（schema 不受影响——
  Part 6.4 修正 1）
