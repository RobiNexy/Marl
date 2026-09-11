package store

import "errors"

// 存储层统一错误哨兵。
//
// 使用约定（两条都是硬性的）：
//   - 实现必须用 fmt.Errorf("...: %w", err) **包装**哨兵，不要新建等值错误；
//   - 上层必须用 errors.Is 匹配，不要用 ==（包装后指针相等不成立）。
//
// 设计意图：错误是**值**。调用方靠哨兵做机械判断（走降级、重试、上报），
// 靠包装文本给人看。因此每个哨兵必须对应一个"调用方会做不同处理"的语义；
// 单纯"报个错"的场景不需要新哨兵，包装已有哨兵即可。
var (
	// ErrNotFound：目标记录不存在。
	//
	// 上层据此走"视为不存在"的降级路径。注意一个尤其重要场景：ContextView
	// 引用了一条取不到的 Log——在真相之源模型下（只追加、不删除）这**不应
	// 发生**，出现即证明数据被外部改动过，除了降级还必须记审计。
	ErrNotFound = errors.New("marl: not found")

	// ErrConflict：并发写冲突（唯一约束、Seq 分配竞争、工作副本被外部修改）。
	//
	// 与 ErrNotFound 的区别：后者是"没有"，前者是"有，但状态不符"。
	// 处理方式也相反：ErrNotFound 降级，ErrConflict 必须重试或上报，
	// 绝不能被当成"不存在"而静默跳过。
	ErrConflict = errors.New("marl: conflict")

	// ErrClosed：存储已关闭。
	//
	// 关闭与写入并发发生时，必须返回这个明确的错误，而不是 panic 或写入半条
	// 数据——上层据此区分"系统正在关闭"（正常停机路径）与"数据有问题"。
	ErrClosed = errors.New("marl: store closed")

	// ErrInvalid：入参未通过契约校验（如 LogEntry 的 Role 为零值、
	// Range 的 fromSeq > toSeq）。
	//
	// 这是**调用方 bug**，不是用户数据问题：不应重试、不应降级，
	// 应当让它显式失败。把它与其他错误混同会让程序性缺陷被当成运行时噪声。
	ErrInvalid = errors.New("marl: invalid argument")
)
