// Package fossil 封装 Fossil SCM 的命令行集成（Part 8.4 / 13.8 阶段 6）。
//
// 单写者提交模型（ADR-0010）：子 Agent 只写文件、不碰 VCS；提交由阻塞恢复
// 后的父 Agent 做，一次 commit = 一轮分治的完整产出。本包是该模型的执行层：
//
//	生命周期（lifecycle.go）—— InitRepo / OpenRepo / IsOpen
//	工作区（workspace.go） —— Status / Add / Commit / Timeline / Diff
//	CLI（cli.go）          —— 进程封装：写互斥、超时、错误分类
//
// # 实测依据（2026-09-12，fossil 2.26 手工探测）
//
//   - author 语义：`fossil commit -U <user>`，user 必须先 `fossil user new`
//     注册（"no such user" 否则）。本项目用三个固定用户：human / agent / system，
//     Part 8.4 的 "author 字段按调用路径填" 落为 commit 时显式传用户。
//   - ignore-glob：`.fossil-settings/ignore-glob` 文件（版本化）+ 同名
//     `.no-warn` 空文件（消除"versioned and non-versioned"警告）。它在
//     `fossil add` 层生效（SKIP 而非 ADDED），是"仓库不追踪自己"的实现。
//   - 写互斥：两个并发 commit 实测都能成功（fossil 内部用 checkout 数据库
//     锁串行化），但产物是**线性追加**（后者 parent = 前者）——单写者模型下
//     这条路径不会发生；本包仍加进程内互斥（一次只有一个 commit 在跑），
//     防御的是"父恢复后 commit 与人类手动 commit 撞车"这类框架外并发。
//   - IsOpen 判定：checkout 外跑 `fossil status` 报
//     "current directory is not within an open check-out"（exit 非 0）。
//   - timeline/diff 可用 `-R <repo>` 在 checkout 外读（读路径不需要工作区）。
//
// # 错误语义（与 store 哨兵同风格）
//
//   - ErrNotOpen：工作区未 open（调用方 bug 或环境被破坏，fail fast）；
//   - ErrNothingToCommit：无改动（commit 的正常空转，调用方按需跳过）；
//   - 其余 → 包装底层 stderr（fossil 的错误文本是给人看的，机械判断靠哨兵）。
package fossil
