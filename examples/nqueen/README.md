# 示例：N 皇后求解器（多 Agent 分层分治）

跑法（先 `marl init` 建项目、`marl doctor` 自检通过）：

    marl start -dir ~/my-project "$(cat examples/nqueen/task.txt)"

会发生什么（约 3-5 分钟，花费几分钱）：

    你（人类，监督树的根）
      └─ 项目 Agent ── spawn_batch ──> 子 A（求解核心）
      │                                  ├─ 孙 A1（回溯实现）
      │                                  └─ 孙 A2（位运算实现 + 工厂）
      └─（同时）子 B（TUI 层）
                                         ├─ 孙 B1（渲染器接口 + ANSI）
                                         └─ 孙 B2（纯文本渲染器）
    全部 report 回流后，项目 Agent 写 main.go 与架构说明，
    完成后 report 落你的收件箱（~/.local/state/marl/<项目名>/inbox/）。

运行中随时可以：

    marl status                          # 看监督树与谁在跑
    marl say -dir <项目> -to <agent-id> "补充要求"   # 给运行中的任务插话
    marl log -db <项目>/.marl/store.db   # 导出完整对话
    marl stop -dir <项目>                # 优雅终止后台任务

看点：子 Agent 拿到的可写范围由父显式分配（越权即拒）；子任务失败时框架
兜底上报，不会静默丢任务；任务结束主 Agent 会把成果提交进 fossil 版本库
（fossil timeline 可见）。
