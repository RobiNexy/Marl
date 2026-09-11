package types

// AgentState 是 Agent 的进程状态（Part 8.2 状态机）。
//
//	Idle ——(TaskAssign)→ Running ——(fork/上下文满/讨论请求/escalate)→ Blocked
//	   Running ——(任务完成)→ Idle
//	   panic/poison → Crashed（Log 保留，向父报 failed）
//
// 零值契约：零值 AgentState("") 非法，且**不是** Idle。把"未初始化"当成"空闲"
// 会让一个没有任务、没有 Binding 的 Agent 被派发工作，随即以空指针解引用之类的
// 形式崩在离问题很远的地方。状态必须由启动器显式置为 Idle。
//
// 合法迁移（其余组合都非法，须被状态机拒绝）：
//
//	""      → Idle      （初始化）
//	Idle    → Running   （TaskAssign）
//	Running → Idle      （任务完成/失败回报）
//	Running → Blocked   （等子/压缩/讨论/求助）
//	Running → Crashed   （panic / poison）
//	Blocked → Running   （挂起原因解除）
//	Blocked → Crashed   （挂起期 panic，或看门狗标红后强杀）
//	Blocked → Idle      （挂起期间任务被取消）
//
// Crashed 是**终态**：不允许迁出。需要"重试"时应新建 Agent（保留 Log 与血缘），
// 而不是复活一个已崩溃的实例——复活会让 Log 出现无法解释的状态跳变，
// 破坏"从真相重建"的前提。
//
// 并发：状态由所属 Agent 的执行协程单写；外部（看门狗、父 Agent）只读与
// 请求迁移，不直接写状态字段。
type AgentState string

const (
	StateIdle    AgentState = "idle"
	StateRunning AgentState = "running"
	StateBlocked AgentState = "blocked"
	StateCrashed AgentState = "crashed"
)

// Valid 报告 s 是否为四个已定义状态之一。零值返回 false。
//
// 并发：纯函数。
func (s AgentState) Valid() bool {
	switch s {
	case StateIdle, StateRunning, StateBlocked, StateCrashed:
		return true
	}
	return false
}

// IsTerminal 报告 s 是否为不可迁出的终态。
// 调用者应以此决定"该新建实例"还是"可以恢复"。
func (s AgentState) IsTerminal() bool { return s == StateCrashed }

// BlockReason 是 Blocked 状态的四个子类型（Part 8.2）。
// 全部共享同一套挂起/恢复逻辑。
//
// 零值契约与一致性规则：零值 BlockReason("") 表示"不在 Blocked 状态"。
// 因此存在一个双向约束，由状态机与持久层共同保证：
//
//	AgentState == StateBlocked  ⟺  BlockReason 非空且 Valid
//
// 违反任一侧的后果都很具体：挂起原因丢失会让恢复逻辑永远等不到唤醒条件；
// 非 Blocked 状态却带着原因，会让审计看到一次并未发生的挂起。
type BlockReason string

const (
	BlockWaitChildren BlockReason = "wait_children" // fork 了子 Agent，等所有子 report
	BlockCompressing  BlockReason = "compressing"   // 上下文满，Orchestrator 在跑
	BlockDiscussing   BlockReason = "discussing"    // 等人类审阅讨论草稿
	BlockEscalating   BlockReason = "escalating"    // 向上求助，等上级/人类回复
)

// Valid 报告 b 是否为四个已定义挂起原因之一。零值返回 false（零值表示"未挂起"）。
//
// 并发：纯函数。
func (b BlockReason) Valid() bool {
	switch b {
	case BlockWaitChildren, BlockCompressing, BlockDiscussing, BlockEscalating:
		return true
	}
	return false
}

// ChildStatus 是运行时上下文里各子的瞬时状态（Part 8.1 / 9.6）。
// "我的子进程状态"是瞬时状态查询，不是历史真相——Log 只记"我 fork 了谁"。
//
// 零值契约：零值 ChildStatus("") 非法。它不表示"未知"，而应被视为"查不到"。
// 对不存在的 childID 查询必须返回错误或显式的"无此子"，不得用零值兜底——
// 否则父 Agent 会把一个不存在的子当成还在运行，从而永久等待一个不会到来的
// report（挂起原因显示为"等子"，而实际上根本没有这个子）。
type ChildStatus string

const (
	ChildRunning ChildStatus = "running"
	ChildDone    ChildStatus = "done"   // 已 report（success / partial）
	ChildFailed  ChildStatus = "failed" // 已 report failed 或框架代报
)

// Valid 报告 c 是否为三个已定义子状态之一。零值返回 false。
//
// 并发：纯函数。
func (c ChildStatus) Valid() bool {
	switch c {
	case ChildRunning, ChildDone, ChildFailed:
		return true
	}
	return false
}

// WatchdogPolicy 是 Watchdog 的检测参数（Part 8.5）。
// Agent 状态由系统监控，父不需要知道子跑了多久。
//
// 零值陷阱（严重）：本结构的零值**不可用**，而且两个数值字段与布尔字段的
// 零值方向不一致：
//
//	NoProgressMinutes = 0   —— 不是"关闭检测"，而是"0 分钟无进展即告警"，
//	                           即每次检查都告警，会让系统瞬间淹没在假告警里；
//	BlockedStallMinutes = 0 —— 同上，立即标红；
//	DeadlockCheck = false   —— 真正的"关闭检测"（方向安全）。
//
// 因此规则是：
//   - 加载期必须显式填充全部字段，0 一律判非法（不允许依赖零值）；
//   - "关闭某项检测"必须由显式途径表达，绝不复用 0——同一个结构性零值
//     同时意味着"最激进"和"最保守"是本类型最危险的地方。
type WatchdogPolicy struct {
	// NoProgressMinutes：连续 K 分钟无 Log 追加、无工具调用 → 标黄记审计。
	// 必须 > 0。K 越小越灵敏，代价是噪声。
	NoProgressMinutes int
	// BlockedStallMinutes：某 Agent 在 Blocked 超过该阈值 → 标红。必须 > 0。
	BlockedStallMinutes int
	// DeadlockCheck：是否启用 Blocked 图成环检测。零值 false = 关闭。
	DeadlockCheck bool
}
