package agent

// 等待策略（Part 8.3 / 8.7，13.11 阶段 9："internal/agent/wait.go 改造，
// 支持 await 策略 all / any / n"）。
//
// 语义与默认（Part 8.3 的硬边界不动）：
//
//	all（默认）——全部子 report 后恢复（Part 8.3 的行文口径）；
//	any        —— 任一子 report 即恢复。父在响应轮可以决定"等剩下的"
//	              还是继续推进（下一轮 eventLoop 结束时 pending 仍非空
//	              → 再次 Blocked(WaitChildren)，此时策略已重置为 all）；
//	n          —— 到 k 份 report 恢复（1..len(items)；越界取 all）。
//
// [推断 + 权衡: 策略只作用于**第一次**恢复。"any 恢复后剩余子随任务
// 自然流转"要求父能在子半途中收尾——但 Part 8.3 的不变量是"父的任务
// 结束前全部子必须归位"（框架代报/Watchdog 兜底依赖父在等待期内活着）。
// 消耗一次后回退 all 是唯一同时保住"父不提前死亡"+"any 的拓扑价值
// （更早看到第一份结果）"的形态。]

// WaitStrategy 是一次 fork 波次的恢复判据（见文件头）。
type WaitStrategy int

const (
	WaitAll WaitStrategy = iota
	WaitAny
	WaitN
)

// awaitChildren 的实现改造点在 mailbox.go（等待的 select 在那里；本文件
// 只承载策略类型与它的存取——策略字段与 pendingChildren 同属 Pump/主
// 循环并发面，读写一律经 a.mu）。

// setWait 记录本轮 fork 波次的恢复策略（intentSpawnBatch 调用）。
func (a *Agent) setWait(kind WaitStrategy, n int) {
	a.mu.Lock()
	a.waitStrategy, a.waitN = kind, n
	a.mu.Unlock()
}

// takeWait 取走当前策略并复位为 all（只对第一次恢复生效——文件头的
// 推断注记）。
func (a *Agent) takeWait() (WaitStrategy, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	kind, n := a.waitStrategy, a.waitN
	a.waitStrategy, a.waitN = WaitAll, 0
	return kind, n
}
