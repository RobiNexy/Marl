// Package watchdog 实现系统监控（Part 8.5，13.11 阶段 9）。
//
// 定位：Agent 状态由系统监控，**父不感知时间**——父的视角里只有
// "子回来了，带着什么结果"。Watchdog 是框架级独立 goroutine，周期扫描
// 进程表，按判据处置（Part 8.5 的判据表）：
//
//	检测项    判据                        动作
//	no 进一步  连续 K 时间无 Log 追加       记审计（标黄，不终止）
//	超预算    TokenUsed > MaxTokens        强制终止 → 框架代报 failed
//	超时      elapsed > MaxSeconds         强制终止 → 框架代报 failed
//
// （阻塞链停摆 / 死锁是 marl status 的渲染与 escalate 的输入，v1 观测）
//
// "强制终止"的机制面在 Spawner.Terminate（cancel + runChild 的框架代报
// 兜底路径）；Watchdog 只做判据计算与调用——不直接杀 goroutine、不读
// 进程表内部。
//
// goroutine 纪律：唯一 goroutine 由 Start 创建；退出条件 = Stop 或
// ctx 取消；scan 内的错误全部记审计不外泄（扫描失败 ≠ 系统崩溃）。
package watchdog
