package types

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// PathMode 是命名空间挂载的三种模式（Part 5.2）。
//
// 语义是"Agent 视角的感知"，而非单纯权限位：
//
//	PathWrite  —— 可读可写，Agent 感知为"存在，能改"
//	PathRead   —— 只读，Agent 感知为"存在，改不了"（错误码 PATH_READONLY）
//	PathHidden —— 不存在，Agent 感知为 ENOENT（查不到、列不出）。默认规则。
//
// 零值契约：零值 PathMode("") **不是合法值**，也不等于 PathHidden——Go 的
// string 别名无法让零值等于非空常量。因此两条互补规则是对消费者的硬约定：
//
//   - 写入/配置校验（fail fast）：Mount.Valid 拒绝非法模式，避免错误配置静默生效；
//   - 解析降级（fail closed）：Resolver 遇到任何未识别模式（含零值）一律按
//     PathHidden 处理。授权默认不可用，绝不 fail-open。语义授权（谁能写）与
//     fail-safe 方向（默认拒绝）是不同的东西，前者由 Mounts 决定，后者由本规则兜底。
//
// [权衡: 曾考虑改 int + iota 使零值即 PathHidden，由类型系统承载 fail-closed。
// 否决原因：设计文档 §3.3 把模式定为字符串枚举（配置、审计、SQLite 皆为人可读
// 字符串），改 int 需全链路引入 String/MarshalText，为单一不变量付出全项目可读性
// 代价。改用"Valid() + 消费者 fail-closed"组合，代价是依赖实现者守约，
// 收益是保持文档一致与可读性。]
type PathMode string

const (
	PathWrite  PathMode = "write"
	PathRead   PathMode = "read"
	PathHidden PathMode = "hidden"
)

// Valid 报告 m 是否为三个已定义模式之一。
//
// 后置条件：m 为零值或任何未定义字符串时返回 false。
// 并发：纯函数，无状态，多 goroutine 并发调用安全。
func (m PathMode) Valid() bool {
	switch m {
	case PathWrite, PathRead, PathHidden:
		return true
	}
	return false
}

// Mount 是一组 (路径模式, 模式) 的挂载点。命名空间 = 若干 Mount，
// 没有任何 Mount 覆盖到的路径 → hidden（Part 5.1）。
//
// 不变量：
//   - Pattern 非空，且必须是相对项目根的 glob（绝对路径与 ".." 出现即非法）；
//   - Mode 必须通过 Valid()，零值非法（见 PathMode 的零值契约）。
//
// 零值契约：Mount{} 非法（Pattern 为空、Mode 为零值）。反序列化与构造路径
// 必须调用 Valid，把它当作必须显式填充的结构，而非可直接使用的默认值。
type Mount struct {
	Pattern string // glob，如 "src/auth/oauth/**"
	Mode    PathMode
}

// Valid 报告该挂载点是否可直接投入使用（零值返回错误原因）。
//
// 失败：Pattern 空、Mode 非法。不检查 glob 语法（那是匹配器的职责，
// 见 Resolver 实现；两层校验的语素同属 fail-closed 防线，错误先于
// 匹配发生能让配置错误在启动期就炸）。
// 并发：纯函数。
func (m Mount) Valid() error {
	switch {
	case m.Pattern == "":
		return fmt.Errorf("mount: empty pattern")
	case strings.Contains(m.Pattern, ".."):
		return fmt.Errorf("mount: pattern %q contains '..' (escaping glob is illegal)", m.Pattern)
	case path.IsAbs(m.Pattern) || filepath.IsAbs(m.Pattern):
		return fmt.Errorf("mount: pattern %q is absolute (patterns are relative to project root)", m.Pattern)
	case !m.Mode.Valid():
		return fmt.Errorf("mount: mode %q invalid (zero value not allowed)", m.Mode)
	}
	return nil
}

// Namespace 是一个 Agent 的命名空间（由挂载点组成，无黑名单）。
// 默认 hidden 是整个安全设计的地基：新加的框架内部目录自动不可见。
//
// 不变量：Mounts 中同具体度的模式冲突时 hidden 优先（由 Resolver 保证）。
type Namespace struct {
	AgentID AgentID
	Mounts  []Mount
}

// PathResolution 是 Resolver 的一次解析结果。
type PathResolution struct {
	RealPath string // 规范化后的真实路径（已解析 symlink、已消解 ".."）
	Mode     PathMode
}

// Resolver 是命名空间的唯一收口点（Part 5.4）。
// 所有涉及路径的操作（list_dir / file_read / file_write / file_edit /
// file_search / restore_snapshot / shell workdir / arxiv save_dir）
// 都必须走这里，不允许散在各技能里各自校验。
//
// 实现契约（硬性）：
//   - fail-closed：任何未识别的 PathMode（含零值）按 PathHidden 处理；
//   - 最具体匹配优先：多个 Mount 命中时取模式串更长者；同等具体度下 hidden 优先；
//   - 权限单调性：Resolve 对 need=PathWrite 的判定必须严于 need=PathRead，
//     即 write ⊃ read ⊃ hidden 的能力序不得被反转；
//   - 路径规范化：必须先 resolve symlink 再判定，防止经由链接逃逸命名空间。
//
// 失败模式：need 未被满足、路径越界、模式非法。
// 并发：实现必须可被多 Agent 并发调用（只读查询，不得持有可变共享状态）。
type Resolver interface {
	// Resolve 规范化路径，遍历 Mounts 找最具体匹配（hidden 在同等具体度下优先），
	// 检查 mode 兼容。不满足时返回带错误码的错误（避免调用方各自判断）。
	Resolve(ns *Namespace, path string, need PathMode) (PathResolution, error)
	// CanWrite 是 Resolve(ns, path, PathWrite) 的快捷包装。
	// 失败模式：与 Resolve 相同，但错误被折叠为 bool——调用方不得用它区分
	// "路径不存在" 与 "只读"，需要区分时必须直接调 Resolve。
	CanWrite(ns *Namespace, path string) bool
}

// Subset 判断子命名空间是否满足权限单调递减不变量（Part 9.3）：
//
//	子.writable ⊆ 父.writable
//	子.readable ⊆ 父.readable
//	父.hidden   ⊆ 子.hidden
//
// 防止通过 fork 提权。
func (ns *Namespace) Subset(of *Namespace) bool {
	panic("TODO(phase 0): placeholder")
}
