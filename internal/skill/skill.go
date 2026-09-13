package skill

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/RobiNexy/Marl/internal/orchestrate"
	"github.com/RobiNexy/Marl/internal/types"
)

// SkillKind 是技能的能力标签（Part 4.3 准则 7）。
// 供批处理并发调度与 Profile 深度收窄使用。
//
// 零值契约（并发安全相关）：零值 SkillKind("") 非法，且**绝不允许**被当作
// SkillReadOnly。原因是两个方向的错误代价不对称：
//
//	把 mutating 当 read_only —— 并发调度会同时跑多个写操作（改文件、改结构），
//	                            产生竞态与不可复现的状态损坏；
//	把 read_only 当 mutating —— 只是变慢，正确性无损。
//
// 所以"识别不了就按只读处理"是 fail-open 写法。未识别值必须按**最保守**
// 处理：视为不可并发，并在注册期直接被拒。
type SkillKind string

const (
	SkillReadOnly   SkillKind = "read_only"  // 只读，可并发
	SkillMutating   SkillKind = "mutating"   // 改变外部世界，需命名空间校验
	SkillStructural SkillKind = "structural" // 改变系统结构（仅少数，且须走意图）
)

// Valid 报告 k 是否为三个已定义标签之一。零值返回 false。
//
// 并发：纯函数。
func (k SkillKind) Valid() bool {
	switch k {
	case SkillReadOnly, SkillMutating, SkillStructural:
		return true
	}
	return false
}

// ConcurrentSafe 报告该标签的技能是否允许被并发调度。
// 这是**唯一**允许用来决定并发的判据；不要在各处散写 `kind == SkillReadOnly`
// 比较，否则将来新增一个"可并发的只读变体"时会有漏改点。
//
// 后置条件：未识别标签返回 false（fail-closed，见本类型零值契约）。
func (k SkillKind) ConcurrentSafe() bool { return k == SkillReadOnly }

// SkillEnv 是技能执行所需的框架注入环境。
// 技能不允许自己构造它——没有 Namespace/Resolver 的路径操作等于越权。
//
// 不变量（由框架在派发前保证；缺失即框架 bug，不是使用者的错）：
//   - Resolver 非空：路径类技能的唯一合法通道（Part 5.4）。技能若发现
//     Resolver 为 nil，必须**直接报错**而非跳过校验——"没有校验器就放行"
//     是整套命名空间沙箱的单点失效；
//   - Namespace 非空，且 AgentID == Namespace.AgentID（不一致意味着技能会
//     按自己的身份读到别的 Agent 的挂载表，属跨 Agent 数据泄露）；
//   - MaxDepth > 0（深度上限缺失会让 fork 无限递归）；
//   - ControlPlaneRoot 非空（框架靠它判定"禁区"路径）。
//
// 零值契约：SkillEnv{} 非法，禁止以零值传递（它是必须显式填充的执行上下文）。
type SkillEnv struct {
	AgentID     types.AgentID
	Namespace   *types.Namespace
	Resolver    types.Resolver
	Depth       int
	MaxDepth    int
	WorkDir     string // shell 的 workdir 基线
	ProjectRoot string
	// ControlPlaneRoot 是 ~/.local/state/marl/<project-id>/。
	// 它不在任何 Agent 的 namespace 里（hidden），技能不得触碰。
	ControlPlaneRoot string
	// Orch 是编排技能的框架执行面（internal/orchestrate.ViewOps 的实现
	// 由 Agent 装配；nil = 未装配——编排技能 Execute 时如实报失败而不是
	// panic。技能不 import orchestrate 之外的逻辑：匹配/破坏分级/Gate 全在
	// 执行面里（阶段 11 补遗 §3 的收口）。
	Orch orchestrate.ViewOps
	// Snapshots 是 mutating 技能的"写前快照"通道（Part 8.4 / 4.4 file_write）。
	//
	// 接口定义在消费侧（本包）并收窄到 Create 一个方法——这是快照在技能层的
	// 全部用途；Restore 属 restore_snapshot 技能（后续阶段），到时再收窄。
	// nil = 无快照能力（阶段 2 的 mini 以外的环境可以没有），此时 file_write
	// 对**已存在文件**的覆盖写跳过快照——不报错：快照是回滚 affordance，
	// 不是写入的前置条件（有快照是增益，无快照时原子写仍是原子写）。
	Snapshots Snapshotter
}

// Snapshotter 是 file_write"写前快照"的窄接口（消费侧收窄，Part 11.10pre）。
// relPath 是经过 Resolver 校验的 workspace 相对路径；实现负责落盘与命名。
type Snapshotter interface {
	Create(ctx context.Context, relPath string, content []byte) (string, error)
}

// SkillResult 是技能统一返回值（Part 4.3 准则 6：JSON 可序列化、含 ok）。
//
// 不变量（与 types.ToolResult 完全一致，二者在边界上互转）：
//
//	OK == true   ⟹  ErrorType == ""
//	OK == false  ⟹  ErrorType != ""
//
// ErrorType 只能取本文件末尾声明的错误码常量，或技能自己声明的扩展码。
// 错误码是**机器可读**契约（上层靠它做机械判断：是否重试、是否升级、
// 是否回填给模型），不允许临时拼字符串。
//
// 零值契约：SkillResult{} 非法（OK=false 却无错误码）。一个"失败了但不知道
// 原因"的结果最终会被呈现给模型，模型只能猜，而猜测会在后续轮次里被当作
// 事实——这是幻觉最廉价的来源之一。
type SkillResult struct {
	OK        bool           `json:"ok"`
	ErrorType string         `json:"error_type,omitempty"` // 取值须来自本包错误码常量（见文件末尾）
	Message   string         // 人类可读说明（进日志与监控，不要求机器解析）
	Data      map[string]any // 技能特有字段（如 file_read 的 content / symbols）
}

// Validate 报告结果是否自洽（见上方互斥不变量）。
//
// 失败：OK 与 ErrorType 的组合违反互斥规则。
// 并发：纯函数。
func (r *SkillResult) Validate() error {
	switch {
	case !r.OK && r.ErrorType == "":
		return fmt.Errorf("skill result failed but has no error_type")
	case r.OK && r.ErrorType != "":
		return fmt.Errorf("skill result ok but carries error_type %q", r.ErrorType)
	}
	return nil
}

// NewFailure 构造业务失败结果（工具确实跑了，结论是"不行"——Skill 契约
// 注释里明确这是**正常返回值**，会序列化回填给模型，模型据此换方法）。
func NewFailure(errorType, format string, args ...any) *SkillResult {
	return &SkillResult{OK: false, ErrorType: errorType, Message: fmt.Sprintf(format, args...)}
}

// NewSuccess 构造成功结果。data 为技能特有字段（可 nil）。
func NewSuccess(data map[string]any) *SkillResult {
	return &SkillResult{OK: true, Data: data}
}

// Skill 是原子技能的契约（Part 4.3 通用准则）。
//
// 实现约定（对所有方法生效）：
//   - 除 Execute 外的方法必须是**纯函数且不依赖可变状态**：它们会在启动期被
//     反复调用来生成冻结前缀（工具表），此时不得 panic、不得读未初始化的
//     现场（否则启动就崩，且崩在工具表生成里，与技能的真正问题相隔很远）；
//   - 并发：Name/Description/Kind/Parameters 必须能被多 goroutine 并发调用；
//     Execute 的并发性由 Kind 决定（见 SkillKind.ConcurrentSafe）。
type Skill interface {
	// Name 是工具名，如 "file_read"。必须取自 skill 的 20 个名字常量，
	// 不允许技能自定义——工具表是冻结前缀，名字即契约。
	Name() string

	// Description 进工具表，LLM 靠它理解用法。
	Description() string

	// Kind 声明副作用类型（供并发调度与权限校验）。
	Kind() SkillKind

	// Parameters 是 JSON Schema（进全项目唯一的冻结前缀）。
	//
	// 硬性要求：合法 JSON，且**逐字节稳定**——同一技能在任何时候、任何进程
	// 里返回的内容都必须完全相同。任何"运行时生成 schema"的实现都会破掉
	// 全项目共享的缓存前缀（每次请求都少命中一段缓存，且不报错）。
	Parameters() json.RawMessage

	// Execute 执行技能。
	//
	// 前置条件：args 来自 LLM 的参数 JSON（框架已校验其合法性、不校验语义）；
	// env 非零且 env.Resolver 非空；ctx 可带 deadline。
	// 后置条件：返回的 *SkillResult 必须通过 Validate()。
	//
	// 失败模式（两类，语义不同，调用方必须区分）：
	//
	//	业务失败——工具确实跑了，结论是"不行"（锚点不匹配、路径只读、
	//	          权限不足、超时被工具内部判定）。返回
	//	          result{OK:false, ErrorType:...} 且 error == nil。
	//	          这是**正常返回值**，会被序列化回填给模型，模型据此换方法。
	//
	//	基础设施故障——工具没跑成：IO 错误、context 取消、实现内部 panic
	//	          恢复后的错误。返回 error != nil 且 result == nil。
	//	          这是异常，由上层决定重试/升级/上报，**不应**呈现给模型。
	//
	// 两类绝不能混：把业务失败写成 error 会让重试逻辑重试一个注定失败的
	// 操作（浪费预算并掩盖真实问题）；把基础设施故障写成 SkillResult 会让
	// 模型以为"工具拒绝了我"，于是换一种方法继续干，而真正需要的是重试。
	//
	// 路径类技能必须过 env.Resolver，不允许直接 syscall。
	Execute(ctx context.Context, args map[string]any, env *SkillEnv) (*SkillResult, error)
}

// ToolSchema 是技能表的一条（框架生成、与 Agent 状态无关，Part 6.4 修正 1）。
// 所有 Agent 共享同一份，逐字节一致，才能跨 Agent 复用缓存前缀。
//
// 不变量：
//   - Name 必须来自 20 个技能名常量；
//   - 三个字段序列化后**逐字节稳定**：字段顺序、空白、JSON 键序都必须固定
//     （因此不能用 map 直接序列化——Go 的 map 遍历顺序是随机的）。
//
// 第二条不变量是"缓存前缀共享"的全部前提，其守护者是 golden test，
// 而不是这段注释（见 ADR-0015）。
type ToolSchema struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// 技能层错误码常量（Part 4.4 / 5.2：能力缺失从不静默，错误结构化）。
//
// 性质：这是**开放集合**——用户可注册自带技能并声明自己的错误码。
// 约束：
//   - 命名统一为大写下划线（便于机械匹配与 grep）；
//   - 一个错误码的含义一旦发布就不得改变或复用（上层可能已按它做判断）；
//   - 不允许用裸字符串字面量代替这些常量（拼错不会被编译器发现）。
const (
	ErrNoMatch           = "NO_MATCH"           // file_edit 锚点不匹配（附 candidates_snippet）
	ErrPathReadonly      = "PATH_READONLY"      // 命中 read 挂载，想写
	ErrNotFound          = "ENOENT"             // hidden 挂载统一用 ENOENT 掩盖 EACCES
	ErrSkillNotAllowed   = "SKILL_NOT_ALLOWED"  // 不在 Profile.AllowedSkills 白名单
	ErrMaxDepthReached   = "MAX_DEPTH_REACHED"  // 到顶后 spawn_subagent 的调用时拒绝
	ErrNamespaceExceeded = "NAMESPACE_EXCEEDED" // 请求路径超出可写范围
	ErrTimeout           = "TIMEOUT"
	ErrBusy              = "BUSY"
)
