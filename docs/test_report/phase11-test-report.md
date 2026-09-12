# Marl 阶段 11 测试报告（llm_call 定稿 + Gate 审批抽象 + 缓存破坏分级）

**日期**：2026-09-12
**范围**：设计文档阶段 11（llm_call 定稿 / Gate PDP-PEP / 缓存破坏分级/
编排 LLM 使用规则补遗）。
**性质**：全量测试——单元测试（`-race`，20 包全绿）。

---

## 1. 结论

**通过**。11.6 改动清单 + 补遗 §7 的逐项交付：

| # | 交付 | 结果 | 证据 |
| :-- | :-- | :-- | :-- |
| 1 | llm_call 迁移为 Intent（messages/wire/vendor_specific-paved 参数面；`purpose` 仅账本标签） | ✅ | proto.ToolsLLMCall（第 8 个意图，表序 commit 顺序不变面：spawn→report→human→reconfigure→branch→discussion→batch→llm_call，golden test 更新） |
| 2 | 多 wire 注册（llm_wires 配置）+ main 活引用 | ✅ | `agent.WireSet`（main 禁止静态声明——config 的解析器把"main"显式报错）；llmCallWire 对 main 回填 a.llm/a.binding |
| 3 | 温度归一化 / 不支持参数剔除（Wire 层） | ✅ | llm_call 的 params 组装把 temperature 截断到 [0,1]；json→text 的降级仍走 Normalizer 的 DegradParamStripped/OutputFormat 面 |
| 4 | Gate 抽象 | ✅ | `internal/gate`：Rule/Request/Decision（allow/deny/need_human）·匹配前缀数值语法（"<10"/">=20"）·默认拒绝·审计一切决策·Decide（非阻塞 PEP 面）/Evaluate（挂起恢复的 Approver 面）分离 |
| 5 | discussion/escalation 收编 Gate Kind | 部分 | Kind 枚举 + 文档收拍（fossil 分支/draft-verdict 信箱**保留**，Part 11.3 §3.4）；[阶段边界] 两个 flow 不重构（改道面风险大于收益：文件信箱实现已经是"need_human→文件裁决→恢复"的完整骨架，Kind 的统一在 Rule vocabulary 与 API 面完成） |
| 6 | 缓存破坏量计算 + 两级分级 + 接入 Gate | ✅ | `computeDestructionFor`（Position 对齐 P 及其后）+ `classifyDestruction`（vault两档）+ 编排技能 PEP 面（ViewOps.Execute → Decide） |
| 7 | llm_call 三维度管制 | ✅ | INPUT_TOO_LARGE 500/200 真机数值 / task_max_calls / task_max_tokens（attrs 面）→ Gate |
| 8 | 编排 prompt 模板落盘 | ✅ | split-boundary / compress-skeleton / summarize 三件随 marl init 生成 |
| 9 | limits / llm_wires / gate_rules 三块 | ✅ | config 的解析+默认物化+验证（默认 limits 是"every 字段必须 > 0"契约的字面级落实；main 活引用禁显式声明） |
| 10 | 拒绝消息带数字 | ✅ | INPUT_TOO_LARGE 报"输入 500 超出上限 200"；GATE_PENDING 带 cache_destroyed_pct/tokens |
| 11 | llm_call 的独立缓存桶（AgentID+"/"+wire） | ✅ | intentLLMCall 的 binding 拷贝面 |
| 12 | target_text 匹配定位（精确→归一→无语义） | ✅ | orchestrate.Select（含 relative 组合过滤） |
| 13 | AMBIGUOUS_TARGET 多命中摘录 | ✅ | 首行/尾行摘录（80 字截断），TestSelectAmbiguous |
| 14 | sidecar 返回不自动落 View | ✅ | llm_call 只产 tool_result；Split/Compress 落地由 Agent 显式 exclude/追加，各过破坏 Gate |
| 15 | 匹配限定可见条目 + frozen 硬拒 | ✅ | visibleEntries 只放可见条目（frozen 不在 View——范围天然不可命中）；ViewOps也无 split |

---

## 2. 静态与单元测试

```bash
go build ./... && go vet ./...
go test ./... -race -count=1    # 全绿（含 gate/config/agent 的新增面）
```

### 2.1 新增测试矩阵

| 包 | 覆盖 |
| :-- | :-- |
| **internal/gate** | 规则匹配（字符串/布尔/数值前缀/缺属性=不适用）·首中生效·缺规则**默认拒绝**·直接用"count 型 grant"额度（问一次给一批：asks 计数面）·NeedHuman 无 Approver 显式拒绝·FileApprover 的审批文件（@grant next N 的 nonce 频率面） |
| **internal/orchestrate** | 定位协议：精确命中 / 多命中摘录 / 归一化兜底（**语义匹配拒之门外**——Tests 用"类似但没全念对"的输入断言 NOT_FOUND）/ relative（first/last/last_user/second_last_assistant/nth_from_top:N）与 text 组合（relative 缩小范围、text 精确命中、二者冲突保持范围——默默换候选是方案上的） |
| **internal/agent** | llm_call：happy（执行/计数/账本 call_type=llm_call 的 TaskSummary 分项）·INPUT_TOO_LARGE 的两个数字·WIRE_NOT_FOUND 的列出全部可用·GATE_PENDING→grant→重发直行；破坏分级：尾部条目（pct 25.8%）pending→批准→重发直行·头部条目（pct 100%）pending→批准→重发生效·AMBIGUOUS_TARGET 的双摘录 |
| **internal/config** | limits 默认/覆盖/越界拒绝·llm_wires 建立 + main 禁止·gate_rules 顺序保留与 match 缺失拒绝 |
| **cmd/marl** | init 骨架的 prompts 模板（compose-skeleton/split-boundary/summarize 三文件全落） |

### 2.2 补遗 §1 的"机械/语义分界"表与实现的对照

- 机械五件套零 LLM：技能 Execute 只转 Gateway（ViewOps.Execute 的纯
  orchestrate 面，无任何 LLMExecutor 路径）✓
- exclude（payload 用完回收）/ reorder / pin 在 tests 里被验证（÷ 我的
  fake"人类"直接批准的场景里 exclude 在 grant 到账后直行——上下文落地
  与删除的顺序是 Agent 的每轮决策）。
- 语义拆分/压缩/摘要：llm_call 的 response_format=json 的 sidecar 调用
  是**模板内容 + messages**（split-boundary.md 的示例面），不是框架的
  split-message Intent——13.11 的"不存在的东西"表：不写框架面的
  split/compressor Intent；orchestrate 的 OpSplit 设计字段未删（它反映
  既有 stage-3 的内核面 & future manifest），但本阶段没有给模型侧的
  split 意图（llm_call 已开拓了该面）。

---

## 3. 关键设计决策的对账（诚实清单）

| 决策 | 实现 | 评估（层的范围） |
| :-- | :-- | :-- |
| Gate 三值决策 | Manager.Decide/Evaluate | Decide 是非阻塞 PEP 面（llm_call / 编排五件套）；Evaluate 启动 Approver（挂起恢复时）——"tool_call 中途不阻塞 eventLoop"的取舍（训练成本：<Approver 是 file 议定的，一旦改 wire 层的 Approver，两个面都无差别） |
| grant=not yes | AddGrant（session） / 动态 allow 规则+落盘 | count 型额度管理用先享后减（consumeGrant）；tokens/always 两形态定义在 AddGrant（配合 Manager 测试） |
| llm_call 参数 | llm_callArgs | vendor_specific 与 wire-specific thinking 覆盖字段**未在本面落地**（契约里的"逃生舱"，tempo 面交给 llm_wires 中声明的 static thinking；[阶段边界] 当 wire 需要支持 FIM/prefill 时它在该 wire 的 Adapter 面） |
| 编排破坏需要人 | ViewOps（agent 实现） | pin/unpin 零破坏直行；exclude/restore/reorder/annotate 靠 rules + attrs |
| discussion/escalation 收编 | Kind 枚举 ([中间态]) | 前述边界：确认链路的完整语义保留；Kind 的回复表在 gate_rules 的形态下可加也能不用（两个既有 flow 的恢复继续由它们各自的 store 完成——统一到 gate_rules 属阶段 11.5 的配置层面跟进项 |
| sidecar 空回 | llmCallResultOf 的 WIRE_EXECUTION_FAILED | Agent 已返回；重试是 Agent 决策 |

---

## 4. 交付物清单

| 文件 | 内容 |
| :-- | :-- |
| `internal/gate/{doc,rule,manager,file}.go` (+test) | Gate 抽象全套（规则匹配/首中/grant/审批文件） |
| `internal/orchestrate/{target,viewops}.go` (+test) | 定位协议（target_text/relative）+ ViewOps 面 |
| `internal/skill/{ops.go, skill.go}` | 编排五件套的技能壳.Getenv env.Orch 通道 |
| `internal/agent/{llmcall,viewops}.go` (+tests) | llm_call intent / agentViewOps（匹配→破坏→Gate→apply） |
| `internal/config/{gates.go,gates_test}` | limits/llm_wires/gate_rules 解析 |
| `internal/store/ledger.go` | CallLLMCall 的账本类目 |
| `internal/ledger/ledger.go` | RecordLLMCall |
| `internal/agent/{agent,spawn,execute,mailbox}.go` | Config 接线 / intent dispatch / gatePending 挂起 / errGatePending 恢复 |
| `cmd/marl/init.go` | prompts 三模板（安装面） |
| `docs/test_report/phase11-test-report.md` | 本报告 |

## 5. 已知限制与盲区（诚实清单）

1. **FileApprover 的挂起体验**：agent 侧挂起（gatePending + errGatePending）
   在 CLI-级进程里依赖宿主 Stop 的 ctx；daemon 装配 FileApprover 后
   approvals/ 文件是同一控制面（approval_<ulid>.md）。** 下一次构建
   daemon 时 config 的 file path 需要随 host 输出的目录一致。**
2. **vendor_specific / wire 特化 prefill/FIM 参数**：本阶段没做（sidecar
   wire 配置里 thinking 是静态的；llm_call messages 的 role==='tool' 的
   段折叠未 particular 实现——role 校验允许 'tool' 但 buildLLMCallRequest
   只翻译 system/user/assistant）。[契约面预留，见阶段 11.6 表的
   sidecar-fim/sidecar-prefill 两块线]。 [待验证]
3. **讨论/escalation 的收编面**（改动清单 #5）：Gate 层 Kind 枚举注册、
   但两个 flow 的决定仍走各自的文件信箱（freely 部分，Part 11.3 §3.4 的
   "统一裁决信箱和恢复"的两处细节留待 Daemon 化统一收回）。
4. **未充分激活的能力维度**：Gate 的多 Agent 并发 grant（consumeGrant
   的余额面）只有单 Agent 累计测试；多 Agent 分享同一个 kind grant 的
   隔离面（per-agent 键 vs 全局额度）没有显式测试决定哪个是"正确"的
   （当前实现= per-agent per-kind 额度）。

## 6. Assumptions made

- Gate 的 attrs 数值全部 JSON 形态（float64）；int/int64 双兼容。
- `vendor_specific` 字段 schema 里**未写**（协议面在 wire 层；本阶段
  未开 FIM/prefill 的 wire 面——schema 改动需评估缓存前缀失效）。
- 编排五件套写角色的默认"人类审批后重发"的模型面由 Agent 决定；框架
  只承诺"批准 grant 在额度内第二次直行"。
