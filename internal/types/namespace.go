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

// modeRank 给出模式的能力序：write(2) ⊃ read(1) ⊃ hidden(0)。
// 未识别模式（含零值）按 0 处理——与 Resolver 的 fail-closed 同方向：
// 排序比较里"未识别"不能被当成高能力。
//
// 并发：纯函数。
func modeRank(m PathMode) int {
	switch m {
	case PathWrite:
		return 2
	case PathRead:
		return 1
	default:
		return 0 // hidden 与未识别一律按最低能力
	}
}

// Subset 判断子命名空间是否满足权限单调递减不变量（Part 9.3）：
//
//	子.writable ⊆ 父.writable
//	子.readable ⊆ 父.readable
//	父.hidden   ⊆ 子.hidden
//
// 防止通过 fork 提权。
//
// 实现口径：第三条（hidden 单调）在"默认 hidden"模型下自动成立——父的
// hidden 集是"父挂载表未覆盖的路径"，而本检查保证子的每一条挂载都被父
// 同能力或更强的挂载覆盖，因此子未覆盖的路径 ⊇ 父未覆盖的路径。所以
// 本函数只需逐条验证前两条的挂载级包含。
//
// 挂载级包含（PatternCovers）对带通配符的子模式采取**保守拒绝**：
// 无法静态证明"子模式匹配的每个路径都被父模式匹配"时返回 false
// （fail-closed——拒绝一次合法 spawn 的代价是一次重试，放行一次越权
// 的代价是权限模型失效）。
//
// 并发：纯函数。
func (ns *Namespace) Subset(of *Namespace) bool {
	if ns == nil || of == nil {
		return false
	}
	for _, cm := range ns.Mounts {
		if !cm.Mode.Valid() {
			return false // 非法模式按 fail-closed 拒绝整个子集判定
		}
		covered := false
		for _, pm := range of.Mounts {
			if modeRank(pm.Mode) >= modeRank(cm.Mode) && PatternCovers(pm.Pattern, cm.Pattern) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// PatternCovers 报告 parent 模式是否覆盖 child 模式（即 child 匹配的
// 每个路径都被 parent 匹配）。
//
// 可证明的形态（其余一律 false，fail-closed）：
//   - parent == "**"（覆盖一切）；
//   - 相等；
//   - parent 以 "/**" 结尾：child 等于前缀、位于前缀之下、或等于 parent /
//     以 parent 为前缀（"src/**" 覆盖 "src"、"src/a"、"src/**"、"src/a/**"）；
//   - child 是**字面量**（无通配符）：按段匹配判定（"src/a/b" 被 "src/**"
//     或 "src/*" 覆盖）。
//
// 其余形态（child 带通配符且不属于上述可证明类）返回 false——例如
// child "src/*/x" 无法静态证明被 "src/**" 之外的任何模式覆盖。
//
// 并发：纯函数。
func PatternCovers(parent, child string) bool {
	pp, cp := cleanPattern(parent), cleanPattern(child)
	if pp == "" || cp == "" {
		return false
	}
	if pp == "**" {
		return true
	}
	if pp == cp {
		return true
	}
	// parent 以 /** 结尾：前缀树包含。
	if prefix, ok := trimTail(pp, "/**"); ok {
		if cp == prefix || strings.HasPrefix(cp, prefix+"/") {
			return true
		}
	}
	// parent 以 /** 结尾且 child 也以 /** 结尾：前缀包含即可
	//（"src/**" ⊇ "src/a/**"）——已由上一支覆盖（cp 以 prefix+"/" 开头
	// 包含 "src/a/**"）。落到这里说明 parent 不带 /**。
	// child 是字面量：按段匹配。
	if !strings.Contains(cp, "*") {
		return patternMatches(pp, cp)
	}
	// child 带通配符且 parent 不带 /**：无法静态证明 → 拒绝。
	return false
}

// cleanPattern 规范化模式（Clean、去首尾斜杠）；非法（空、含 ..、绝对）
// 返回 ""（调用方按 false 处理）。
func cleanPattern(p string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	if p == "" || strings.Contains(p, "..") || strings.HasPrefix(p, "/") {
		return ""
	}
	return p
}

// trimTail 剥离后缀 tail，成功返回前缀与 true。
func trimTail(s, tail string) (string, bool) {
	if strings.HasSuffix(s, tail) {
		return s[:len(s)-len(tail)], true
	}
	return "", false
}

// patternMatches 判定字面量路径是否被模式匹配（与 ns 包的 globMatch 同一
// 语义的 types 侧实现——types 不能依赖 ns（ns 依赖 types），而 Subset 的
// 字面量分支需要它。两处漂移的防线是 ns 测试里的交叉用例（同一对
// (pattern, path) 在两侧结果一致）；规则变更须同步两处并记 ADR。
//
// 并发：纯函数。
func patternMatches(pattern, literalPath string) bool {
	return matchPat(splitPattern(pattern), splitPattern(literalPath))
}

func splitPattern(p string) []string {
	if p == "" || p == "." {
		return nil
	}
	return strings.Split(p, "/")
}

func matchPat(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	switch pat[0] {
	case "**":
		for skip := 0; skip <= len(segs); skip++ {
			if matchPat(pat[1:], segs[skip:]) {
				return true
			}
		}
		return false
	case "*":
		// "*" 恰好匹配一个段（不跨段）——与 default 同一处理
		//（path.Match 的单段语义），显式列出是为了自文档。
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		return err == nil && ok && matchPat(pat[1:], segs[1:])
	default:
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		return err == nil && ok && matchPat(pat[1:], segs[1:])
	}
}
