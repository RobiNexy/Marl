package types

// TaskStatus 是一次任务的最终/当前状态（Part 7.6 成本报表的"状态"行）。
//
// 与 proto.ReportStatus 的关系（刻意不完全重合）：ReportStatus 描述**子 Agent
// 的自述结论**，只有终态三值；TaskStatus 描述**任务本身**，多一个 running。
// 两者共享 success/partial/failed 的字面值，因此子的自述结论可以不经映射
// 直接参与任务状态汇总——这是字符串枚举少有的便利，利用了它。
//
// 零值契约：零值 TaskStatus("") 非法，且**不是** running。
// 把零值当 running 会让"忘了设状态"的任务永远停在运行中（不会被回收、
// 不会被计入失败率），因此消费侧必须拒绝而非兜底。
type TaskStatus string

const (
	TaskRunning TaskStatus = "running" // 进行中（唯一非终态）
	TaskSuccess TaskStatus = "success"
	TaskPartial TaskStatus = "partial" // 部分完成（框架降级报告后的常见结果，Part 9.6）
	TaskFailed  TaskStatus = "failed"
)

// Valid 报告 s 是否为已定义状态之一。零值返回 false。
//
// 并发：纯函数。
func (s TaskStatus) Valid() bool {
	switch s {
	case TaskRunning, TaskSuccess, TaskPartial, TaskFailed:
		return true
	}
	return false
}

// IsTerminal 报告 s 是否已是终态（running 不是）。
//
// 用途：决定任务是否还能被升级/重派/汇总。判据基于"非 running"而不是
// 枚举终态值，这样未来新增终态时不会漏改——但新增中间态（如 paused）
// 时必须回来修正本方法，这个取舍是有意的：终态少见、中间态更少见，
// 且写成白名单会让"新增终态忘了登记"这个错误变成静默的行为差异。
func (s TaskStatus) IsTerminal() bool { return s.Valid() && s != TaskRunning }
