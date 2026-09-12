# Marl 阶段 10 测试报告（打磨与补全——全功能面）

**日期**：2026-09-12
**范围**：设计文档 13.12（阶段 10）的全部交付物——"不做"清单清零 +
打磨清单。
**性质**：全量测试——单元测试（`-race`）+ **真机**功能测试
（models probe 走 DeepSeek 真机、vendor/promote 用真 fossil、
httptest 回放 Anthropic 全链路）。

---

## 1. 结论

**通过**。13.12 的交付判据（"不做"清单清零 + 打磨面）逐项落位：

| 交付项（13.12） | 结果 | 证据 |
| :-- | :-- | :-- |
| Anthropic wire（复用大部分 Normalizer） | ✅ | `internal/wire/anthropic.go`（Normalizer/Adapter/Denormalizer 三件套 + httptest 端到端：顶层 system、tool_use/tool_result 块、thinking 块+budget、metadata.user_id=每 Agent 缓存桶、usage 四字段映射） |
| 能力探测 `marl models probe` | ✅ 真机 | DeepSeek 真机：deepseek-flash / deepseek-v4-pro 三判据全 PASS（chat 200 / tool_calls=1 / cached_tokens=1024 & 1280） |
| escalate（复用讨论的阻塞/恢复） | ✅ | `internal/escalate`（路由 + 人类文件信箱 + Manager 分发）；Agent 侧 Blocked(Escalating) 全闭环（generic 集成 TestEscalationFullLoopHuman）；父路径框架 ACK 经真实 spawner.SendTo 回程（TestEscalationParentACKByPump） |
| request_human 三形态 | ✅ | 路由判定 3 态（父在→父信箱 / 父 Blocked→人类信箱 / 无出口→显式报错）+ 信封回程 + done/ 触发回程的端到端（escalate 包验证） |
| 自动审批规则（auto_discuss） | ✅ | 写路径命中清单 → 自动开讨论（TestAutoDiscussInterceptsWrite：讨论 session 挂牌、写入未执行、tool_result 报"讨论已开启"） |
| vendor / promote | ✅ 真机 | `marl knowledge promote` → 全局库 commit（author=human）；`marl knowledge pull` → vendor/ 下**逐字节一致** + lock 锁 artifact（真 fossil roundtrip + CLI 输出） |
| 契约注入（fork 时 InjectMessages） | ✅（阶段 5 已交付） | TestForkInjectedMessages（保持回归） |
| reconfigure（三校验 + 缓存失效审计） | ✅ | proto.ReconfigureRequest.Validate 与 Agent.ApplyReconfigure（空变更拒 / 无 reason 拒 / 对象错拒 / sampling 整块替换 + thinking 变更触发缓存审计条目）；意图侧 = "我卡住了"审计面板 |
| Pool 并发闸门与深度优先队列 | ✅ | `wire.NewPool`：depth 降序堆（MaxInflight=1 时 deep 先行——TestPoolDepthPriority）、RPM 令牌桶、排队计入 Inflight 口径、ctx 取消不泄漏 |
| 熔断与健康检测 | ✅ | CircuitPolicy：连续失败 ≥ 阈值 → 短路（caller 不再被调用）→ 冷却后半开转 healthy（TestPoolCircuit）；Health() 快照 |
| 错误信息（candidates_snippet / 拒绝原因） | ✅（已是常态面） | NAMESPACE_EXCEEDED 的"你只能分配…"、auto_discuss 的"写转讨论"、batchRejects 的逐项原因（清单引用既有测试） |
| 审计表完整性 | ✅ | **所有**意图裁决都有审计痕迹（基础意图 + spawn/batch + report_check + reconfigure + escaping/**intent_not_handled**/讨论自动） |
| golden 测试（byte-stability / 前缀不变性） | ✅ | intentSchemas() 的 7 工具**顺序**金面（缓存前缀组成）+ frozen 三段跨轮逐字节（TestGoldenIntentSchemas / TestGoldenFrozenPrefix） |
| conversation 文件导出 | ✅ | `marl log -out` 输出 Part 1.4 会话（角色呈现名 + 截断 + agent 分节；store 驱动的渲染测试） |
| marl log / marl attach | ✅ | 真机 fork_test 库上导出 `conversation.md` 内容正确（附录） |
| 文档与示例 | ✅ | `docs/examples.md`（命令面全速查 + 错误码语义） |

---

## 2. 静态检查与单元测试

```bash
go build ./... && go vet ./...     # 通过
go test ./... -race -count=1       # 全部通过（20 个包；新增 45+ 例）
```

### 2.1 阶段 10 新增的测试矩阵

| 包 | 覆盖要点 |
| :-- | :-- |
| **internal/wire (pool_impl)** | 深度优先队列（in-flight=1 时 deep 先行 + 同 depth FIFO）·熔断（阈值/短路/半开）·Inflight 口径（排队计入）·ctx 取消不泄漏（worker 惰性跳过） |
| **internal/wire (anthropic)** | BuildRequest 的 system/max_tokens/合块/Assert 拒绝（4 个用作例）·httptest 端到端（请求体头面 + 混合流 Outcome 序 + usage 映射）·HealthCheck（2xx/非2xx） |
| **internal/escalate** | Route 三态 + 无出口报错；submit→reply 的 nonce 频度（**nonce 不匹配的 done 文件不解除等待**——原则 4 裁决凭据面）；Manager 的父路径（第一 param + deliver）与 human 路径（真信箱 roundtrip） |
| **internal/knowledge (vendor)** | Promote → 全局库条目（author=human commit + hash 返回）；Pull → 逐字节一致 + lock；lock 再解析；全局空库显式报错 |
| **internal/agent** | request_reconfigure 的意图面（BAD_ARGS 与回填）·ApplyReconfigure（三校验/归属检查/替换语义）·auto_discuss 折算（write→discussion）·golden（意图表序 + frozen 前缀） |
| **cmd/marl** | knowledge pull/promote 子命令（flag 面；cmdKnowledge 的 dispatch）；`log` 的 conversation 渲染（audit 发现 + agent 过滤 + 角色呈现名 + 截断） |

### 2.2 真机模型探测（DeepSeek）记录

```text
model deepseek-flash:
  chat         PASS     status=200
  tool_call    PASS     tool_calls=1
  cache        PASS     cached_tokens=1024
model deepseek-v4-pro:
  chat         PASS     status=200
  tool_call    PASS     tool_calls=1
  cache        PASS     cached_tokens=1280
```

缓存判据的字段容差被真机校准：DeepSeek 返回
`prompt_cache_hit_tokens`（顶层字段），通用 OpenAI 语境是
`prompt_tokens_details.cached_tokens`——两者都被接受（顺序：顶层优先）。
这属于"实现的字段面从真机数据校准"（efficiency of probe: 一次真实流量的
cached_tokens 判据明确优于"estimate by local"）。

---

## 3. 编译后软件的功能测试（端到端）

| 命令 | 结果 | 说明 |
| :-- | :-- | :-- |
| `marl init /tmp/marl-e2e` | ✅ | 项目仓库 + 骨架入库 |
| `marl knowledge lint`（104 token） | ✅ 通过 | 逐文件显示；OK 行 |
| `marl knowledge lint`（2400+ 超限） | ✅ 退出 1 | 逐文件报告（2516 est-token > 1000） |
| `marl knowledge promote` + `pull`（真 fossil） | ✅ | 全局库 commit + pull 的逐字节 roundtrip（diff 空） |
| `marl models probe`（真机 DeepSeek） | ✅ | 两 model 三判据 PASS |
| `fork_test -dry-run` | ✅ | 阶段 5/6 链路回归（阶段 10 的新增面不破坏既有接线） |
| `marl log -db ...` / `-out conversation.md` | ✅ | Part 1.4 的格式（角色呈现名 Human/Assistant/Tool/SubTask/Escalation） |
| `topo_test` / `-watchdog` | ✅ | 6 节点树 + Watchdog 终止（阶段 9 面的回归） |

### 3.1 真机输出摘录（vendor/promote / models probe / log）

```text
$ marl knowledge promote -dir /tmp/marl-e2e -global /tmp/marl-global/global.fossil knowledge/contracts/http.md
已提升 knowledge/contracts/http.md 到全局库（commit 51b971…，author=human）

$ marl knowledge pull -dir /tmp/marl-e2e -global /tmp/marl-global/global.fossil
已拉取 1 条知识：
  knowledge/contracts/http.md              artifact=51b97160

$ diff contracts/http.md vendor/knowledge/contracts/http.md   # 空 → 逐字节一致
VENDOR_ROUNDTRIP_OK
```

---

## 4. 实测缺陷与修复

### 4.1 Anthropic tool 调用 Outcome 的丢调用（主实现缺陷，测试前发现）

**现象**：httptest 回放的响应含 tool_use 块，但 Denormalizer 的混合流
只产出了 thinking 与 text——tool calls 丢了。
**归因**：buildAnthropicTurn 收集了 pendingCalls 却忘了 append 成一个
Outcome（初写的 unfold 只做了收集）。
**修复**：pendingCalls 显式成一个 Outcome（Usage 共享——与 OpenAI 形态的
记账纪律一致）。

### 4.2 probe 的 usage 键层次（真机校准）

**现象**：cached_tokens 恒 0。
**归因**：cachedTokensOf 收其 prompt（envelope）而不是 usage 于响应
envelope 的 "usage" 键——一层之差。
**修复**：取 usageOf(out) 再判；并把 DeepSeek 顶层
`prompt_cache_hit_tokens` 纳入容差（**真机校准的字段清单**）。

### 4.3 models probe 报告的 "\n" 字面量（写手疏漏）

fmt 字符串里 `\\n` 被写字面量替换成了字面 \ —首发发现 hassle 输出。
**修复**：字符串面改回 `\`n`（审视命令面输出的一行一欣赏）。

### 4.4 三个 CLI 子命令的 flag 顺序（知识 vendoring）

**现象**：`marl knowledge promote http.md -global …`（positional 在前）在
Go flag 的停止 semantics 下 lost `-global`（缺省回退到 XDG 形态）。
**修复**：usage 检查"positional 只做 target path"；flags 必须先出——文档
面在 `docs/examples.md` 的命令行示例全部用"flags 在前"的调用习惯。

---

## 5. 交付物清单

| 文件 | 内容 |
| :-- | :-- |
| `internal/wire/anthropic.go` (+test) | Anthropic 三件套（internal/wire 的 Normalizer/Adapter/Denormalizer）+ httptest 端到端 |
| `internal/wire/pool_impl.go` (+test) | PoolImpl（深度优先队列/令牌桶/熔断/健康/Inflight） |
| `internal/types/ids.go` | WireAnthropicMessages（Valid 表收敛） |
| `internal/proto/{escalation,reconfigure}.go` | EscalationRule.Validate / ReconfigureRequest.Validate（phase-0 占位清零） |
| `internal/escalate/{doc,mailbox,manager}.go` (+test) | escalate：路由 + 人类文件信箱 + Manager（Send/WaitBack/DeliverReply） |
| `internal/agent/{escalate,reconfigure}.go` (+tests) | request_human / request_reconfigure 意图面 + ApplyReconfigure（三校验 + 缓存失效审计）+ auto_discuss 折算 |
| `internal/agent/{spawn,execute,mailbox}.go` | intent 分发（request_human / reconfigure）+ pump 的 escalation ACK/回程 + audit 完整化 |
| `internal/knowledge/vendor.go` (+test) | vendor pull / promote（真 fossil） |
| `internal/fossil/branch.go` | Cat |
| `cmd/marl/{main,knowledge_vendor,log,models_probe}.go` (+test) | 命令面：knowledge pull/promote、log/attach、models probe |
| `internal/agent/golden_test.go` | golden（意图表序 / frozen 前缀 byte-stable） |
| `docs/examples.md` | 文档与示例 |
| `docs/test_report/phase10-test-report.md` | 本报告 |

## 6. 已知限制与盲区（诚实清单）

1. **Anthropic 线路的历史 thinking 块**：请求侧承载把 Reasoning 以纯文本
   thinking 块发出（Anthropic 的 thinking 块要求 signature；本框架不保留
   签名，经此路径的请求内容 = text 语义（可追本治疗的还原注释）——
   真 API 的 thinking 续写对话特征没有真机验证（本报告的 e2e 面），
   httptest 承载的是协议面），随所以（多份工作）该处需真机后再校准。
2. **Pool 的接线深度**：Pool 是**库面**（类型 + 测试完全、上下文的
   机制面就位）；cmd/mini / fork_test 的行仍在 direct 通路（单 endpoint
   场景下 MaxInflight 的表达面=1），接线形态（caller 闭包是否把
   Denormalizer 一起注入）留给 daemon 主装配（13.5 的 SpawnRequest）
   ——文档在 pool.go 头注，标记是**[阶段边界]**不是完成。
3. **escalation 的父路径**：Managers 的 ACK 是框架自动回复——"父读取并
   答复"（Part 11.3 的父 Agent 主动回复语义）由父的上下文推进（机制面
   ready），阶段 10 的 ACK 形态不会被误读为"父已答复"（ACK 的文本面
   写明"后续动作尚未执行"）。[推理边界显式记录]
4. **vendor lock 的粒度**：单条 promote/pull 记录 tip hash；多版本条目
   + per-entry artifact（Part 12.7 的下代形态）待使用场景出现再看。
5. **Watchdog 的预算与 status**：全局常量（阶段 9 记录）；标黄与
   status 的黄块也已并存——它们来自不同事件面（agent_state vs
   watchdog_no_progress），daemon 化 status 直读进程表后合并。
6. **Assumptions made**：
   - `max_tokens` 的默认值与 Anthropic 的其它字段：协议面显式必填的
     `max_tokens` 由 Assert 校验（Sampling.MaxTokens>0）。
   - probe 的 cache 判据假设"两次同文（含 first call cached_tokens=0
     本次）→ 第二次 hit>0"——真机 PASS，不作全局绑定（不同厂商的缓存
     语义差异以 caps.CacheMode 报表，见 Part 10.13 的 caps_override 面）。
