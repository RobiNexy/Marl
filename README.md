# Marl

多 Agent 编排框架（Multi-Agent orchestration framework）。

设计文档与决策记录是项目的真相之源（原则 2：不可变的真相 + 可变的投影）：

- 设计文档：[`docs/design/marl_design.md`](docs/design/marl_design.md)
- 架构决策记录（ADR）：[`docs/design/decisions.md`](docs/design/decisions.md)

## 目录结构

```
internal/
  types/        最底层契约类型（不允许 import 仓库内其他包）
  store/        持久化契约：MessageLog / Ledger / Audit / ViewStore
  wire/         LLM 调用链契约：Router → Normalizer → Pool → Adapter → Denormalizer
  skill/        原子技能库契约（只读写外部世界）
  proto/        Agent 间消息与意图（意图由框架裁决后执行）
  orchestrate/  上下文编排的原子操作集：(Log, View) → 新 View
docs/design/    设计文档与 ADR
```

## 当前阶段

阶段 0（地基与合约）：只钉死类型、接口与它们的前置/后置契约，
实现体是 `TODO(phase 0)` 占位（阶段 1 起逐个落地）。

## 开发检查

```bash
go build ./...
go vet ./...
gofmt -l internal/     # 应无输出
```

按 ADR 变更流程，任何契约字段修改都必须先新增一条 ADR。
