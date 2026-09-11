package store

import (
	"context"

	"marl/internal/types"
)

// ViewStore 是 ContextView（可变投影）的存储接口（Part 3.3）。
// Log 是真相，View 是工作台：View 可被随意重建、丢弃、覆盖，Log 不能。
//
// 这个不对称是接口设计的前提：本接口的写入失败**不需要**崩溃恢复流程
// （View 丢了就重建），而 MessageLog 的写入失败必须走恢复流程。
// 因此两个接口的失败语义刻意不同，不要试图统一。
type ViewStore interface {
	// LoadView 读回某 Agent 最近保存的 View。无记录时返回一个**新建的空 View**。
	//
	// 关键契约：返回的 View 的 AgentID 必须等于传入的 agentID，即使没有记录。
	// 也就是说"空 View"不等于零值 ContextView{}——后者 AgentID 为空，
	// 是非法状态（见 types.ContextView 零值契约）。调用方拿到它之后会直接
	// 往里面塞 items，若 AgentID 为空，写回时会被 SaveView 的校验拒绝，
	// 而错误现场已经离首次加载很远了（难以定位）。
	// 所以把"补上 AgentID"这件事放在本方法里，而不是推给每个调用方。
	//
	// 失败：存储不可读 → 底层错误（**不返回** ErrNotFound——"没有历史 View"
	// 是正常初始状态，不是错误，用返回值而非错误表达）。
	LoadView(ctx context.Context, agentID types.AgentID) (*types.ContextView, error)

	// SaveView 落盘（或内存）某个 View。每次进入/退出 Blocked 时写一次，
	// Running 内部轮次切换不写（Part 8.8：降低写入频率）。
	//
	// 前置条件：view 非 nil 且 view.AgentID 非空（零值 AgentID → ErrInvalid：
	// 一个没有归属的 View 存进去就再也取不出来了）。
	//
	// 全量覆盖语义：SaveView 是**覆盖**而非合并。这一点必须明确，因为
	// 调用方可能持有较旧的 View 副本（例如 Blocked 期间人类改过），
	// 直接覆盖会丢掉对方写入。约定：进入/退出 Blocked 的写入路径必须先
	// LoadView 再改再存（读-改-写），且 SaveView 不做任何字段级合并
	// （合并策略属于上层，存储层只管持久化）。
	SaveView(ctx context.Context, view *types.ContextView) error
}

// SnapshotStore 负责文件级快照（Part 4.4 file_edit 的修改前快照 + Part 8.4）。
// 快照在 .marl/snapshots/（[忽略]，不进版本控制），保留最近 10 个。
//
// 安全契约（本接口最重要的部分）：Create/Restore 的路径参数都来自
// Agent 的工具调用，因此**必须**视为不可信输入。
type SnapshotStore interface {
	// Create 对 relPath 的文件做一份快照，返回快照相对路径。
	//
	// 安全：relPath 必须解析到 workspace 内（拒绝绝对路径与 ../ 越界）。
	// 理由不是"防止 Agent 干坏事"，而是快照的用处是回滚——若快照能把
	// 任意文件复制进 .marl/snapshots/，那么"快照目录"就成了一个把系统
	// 敏感文件搬进仓库的通道，随 .marl 被读取/上报而外泄。
	//
	// 失败：relPath 为空或越界 → ErrInvalid；文件不存在 → ErrNotFound。
	// 并发：必须支持多 Agent 并发快照（父子 Agent 可能同时编辑）。
	Create(ctx context.Context, relPath string, content []byte) (string, error)

	// Restore 按快照路径恢复文件内容。
	//
	// 安全：snapshotPath 必须是本 store 自己产出的路径（Create 的返回值），
	// 且解析后必须落在 .marl/snapshots/ 之内。**这一条比 Create 的更关键**：
	// Create 越界是读出，Restore 越界是**写入任意路径**（拿到一个可控的
	// snapshotPath 就等价于任意文件写）。
	//
	// 失败：snapshotPath 为空/越界/不属于本 store → ErrInvalid；
	// 快照不存在 → ErrNotFound。
	Restore(ctx context.Context, snapshotPath string) error

	// Prune 把每个文件的历史快照修剪到最近 maxPerFile 个。
	//
	// 失败：maxPerFile <= 0 → ErrInvalid（0 不是"全部删除"的合法表达；
	// 让 0 表示清空会让一次漏传参数变成数据丢失）。
	// Part 8.4 的默认值是 10，由调用方显式传入，存储层不设隐含默认。
	Prune(ctx context.Context, maxPerFile int) error
}
