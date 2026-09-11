package proto

import (
	"time"

	"marl/internal/types"
)

// ReportStatus 是子 Agent report 的状态（Part 9.6）。
// 框架做机械检查：声称 success 但留了 TODO / 无改动会被降级。
//
// 零值契约：零值 ReportStatus("") 非法。特别注意它**不是** ReportSuccess——
// 把未设置当成功，恰好抵消了本机制的全部意义（原则 4：不采信 Agent 自述）。
// 因此 ReportChecker.Check 遇到未定义状态必须视为 failed（最保守方向），
// 而不是放行。
//
// 与 types.TaskStatus 的关系：本枚举是子 Agent 的**自述结论**（只有终态），
// types.TaskStatus 是**任务本身**的状态（多一个 running）。字面值刻意共用，
// 使子结论可无映射地参与任务汇总。
//
// 字面值是协议的一部分（Part 9.6 的 JSON schema 里写死为
// enum ["success","partial","failed"]），因此**不得**改值。
type ReportStatus string

const (
	ReportSuccess ReportStatus = "success"
	ReportPartial ReportStatus = "partial"
	ReportFailed  ReportStatus = "failed"
)

// Valid 报告 s 是否为已定义状态之一。零值返回 false。
//
// 并发：纯函数。
func (s ReportStatus) Valid() bool {
	switch s {
	case ReportSuccess, ReportPartial, ReportFailed:
		return true
	}
	return false
}

// ChildReport 是子 Agent 完成时提交给父的 report（Part 9.6）。
//
// Report 是自由文本（框架不解析、不校验格式）；但框架对 status 做机械检查：
//   - 声称 success 但 writable_paths 下新增 TODO(agent) 行 → 降级 partial
//   - 声称 success 但工作区无改动 → 降级 failed
//   - report 提到的文件路径不存在 → 记审计告警，不降级
type ChildReport struct {
	ChildID      types.AgentID
	Status       ReportStatus
	Report       string
	FilesChanged []string
	Blockers     []string // TODO(agent) / TODO(human) 的机械检查发现项
	TokenUsed    int64
	StartedAt    time.Time
	FinishedAt   time.Time
}

// ReportChecker 承担 report 的机械检查（原则 4：不采信 Agent 自述）。
type ReportChecker interface {
	// Check 对子的 writable 路径做 diff 检查，把自述的 success 按规则降级。
	Check(report *ChildReport, writablePaths []string) (*ChildReport, error)
}

// ChildFailureStats 是 Watchdog / 升级证据用的子任务统计（Part 7.2 子任务失败率）。
type ChildFailureStats struct {
	Total      int
	Failed     int
	OverBudget int
}
