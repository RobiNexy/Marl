package skill

import (
	"context"

	"marl/internal/types"
)

// Registry 是技能注册表。
//
// 工具 schema 全项目唯一、由框架生成、与 Agent 状态无关：所有 Agent 共享
// 冻结前缀（StabilityFrozen）。Profile 的 AllowedSkills 只在调用时校验，
// 不参与 schema 生成（Part 6.4 修正 1）。
//
// 并发：注册只发生在启动期（单 goroutine）；启动完成后 Registry 视为只读，
// 只读方法必须可被多 goroutine 并发调用。
type Registry interface {
	// Register 注册一个技能。
	//
	// 失败：重名；名称为空或不在 20 个技能名之内；Kind 非法（见 SkillKind
	// 零值契约）。注册失败时该技能不得出现在 Names/Schemas 的输出里——
	// 半注册状态会让工具表与注册表不一致，而两者不一致正是缓存前缀失效的
	// 一类隐蔽原因。
	Register(s Skill) error

	// Get 按名取技能。
	//
	// 失败：未注册。必须返回错误，而不是 (nil, nil)——调用方拿到 nil 技能后
	// 再解引用会崩在离真正原因很远的地方。
	Get(name string) (Skill, error)

	// Names 返回已注册技能名。
	//
	// 硬性契约：顺序**稳定**（取自 SkillNames() 的固定顺序，而非注册顺序或
	// map 遍历顺序），同一进程内多次调用返回相同结果。
	Names() []string

	// Schemas 返回全项目唯一的工具表（冻结前缀的可测不变量）。
	//
	// 硬性契约：同一进程内多次调用必须**逐字节一致**，且与注册顺序无关。
	// 这是"跨 Agent 共享缓存前缀"的全部前提——守护者是 golden test
	// （见 ADR-0015），不是这段注释。
	Schemas() []ToolSchema
}

// Allowed 判断技能是否在 Profile 的许可列表内（Part 6.11 语义）。
// 空列表 = 全部允许；有内容 = 白名单（不支持黑名单语法）。
//
// 职责边界：本函数只做**集合成员判断**，不检查技能是否已注册，也不产生
// 错误信息。授权语义的完整实现（含错误码）在 Authorizer。
// 这样切分是为了让"授权"与"技能存在性"两个概念不互相纠缠——把后者混进
// 前者，会让"新增技能后忘了更新白名单"变成一个难以定位的授权谜题。
//
// 零值语义：allowedSkills 为空（含 nil）返回 true，与
// types.Profile.AllowedSkills 的零值契约一致（空 = 不限制）。
func Allowed(allowedSkills []string, name string) bool {
	panic("TODO(phase 0): placeholder")
}

// Authorizer 把"调用时校验"集中到一处：技能许可 + 命名空间 + 深度。
// 返回具体的框架错误码（可序列化回传给 LLM，见 Part 9.4）。
//
// 契约：
//   - 前置：name 非空；agentID 对应一个运行中的 Agent；
//   - 后置：返回 nil 表示三项校验全部通过；任何一项不通过都必须返回错误；
//   - 失败：返回带错误码的错误（ErrSkillNotAllowed / ErrMaxDepthReached /
//     命名空间类错误）。调用方必须把这个错误**如实回填给模型**——它是模型
//     唯一的纠错依据（"你不能改这个文件"远好于"工具执行失败"）；
//   - 并发：必须可被同一 Agent 的多次调用并发使用（只读查询，不做缓存写入）。
type Authorizer interface {
	Authorize(ctx context.Context, agentID types.AgentID, name string) error
}
