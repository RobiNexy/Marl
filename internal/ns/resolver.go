// Package ns 提供 types.Resolver 的文件系统实现（Part 5.4）。
//
// Resolver 是命名空间安全模型（每个 Agent 一个 Namespace，默认 hidden）
// 的唯一收口点：所有涉及路径的技能都必须先 Resolve 再 syscall，不允许
// 散在各技能里各自校验（Part 5.4）。本实现只服务阶段 2 的技能面
// （list_dir / file_read / file_write）；shell / arxiv 等落在后续阶段。
//
// fail-closed 是本包的全部安全价值：
//
//   - 未在任何 Mount 覆盖内的路径 → ErrUnavailable（语义上 hidden）；
//   - PathMode 未识别（含零值）→ 按 PathHidden 处理（types.PathMode 契约）；
//   - need=PathWrite 而命中挂载是 read → ErrReadonly；
//   - 同等具体度下 hidden 挂载优先（给一个更具体的 hidden 规则留出表达力）。
package ns

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/RobiNexy/Marl/internal/types"
)

// Resolver 的错误哨兵。技能层把它们机械翻译成 ToolResult 的错误码：
//
//	ErrUnavailable → ENOENT   （hidden 的对外伪装，Part 5.1）
//	ErrReadonly    → PATH_READONLY（Part 4.4 file_write）
//
// 上层必须 errors.Is 匹配；不要解析错误文本。
var (
	ErrUnavailable = errors.New("marl: path not available")
	ErrReadonly    = errors.New("marl: path is read-only")
)

// fsResolver 是 types.Resolver 的最小文件系统实现。
// ProjectRoot 是一切相对路径的解释基准（规范化后的绝对路径）。
type fsResolver struct {
	root string
}

// NewResolver 构造以 root 为根的命名空间解析器。
//
// root 必须是绝对路径：相对 root 会让解析结果随进程 cwd 漂移，而命名空间
// 的挂载表是相对项目根声明的——基准漂移等于挂载表整体错位，且不报任何错。
//
// 并发：返回值只读，可被多 Agent 并发调用（满足 types.Resolver 并发契约）。
func NewResolver(root string) (types.Resolver, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("ns: locate project root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("ns: resolve project root %q: %w", root, err)
	}
	return &fsResolver{root: real}, nil
}

// Resolve 实现 types.Resolver.Resolve。
//
// 契约映射（见 types.Resolver）：规范化 → 最具体匹配（同长 hidden 优先）→
// 能力序检查（write ⊃ read ⊃ hidden）。路径规范化与 symlink 消解在本处，
// 由此防止经由链接逃逸命名空间（Part 5.4 的硬性契约）。
//
// 失败：
//   - path 为空 / 绝对路径 / 含 ".." → 包装 ErrUnavailable（越界的语义对外
//     就是"不存在"——不区分成因可以避免一个路径探测通道：攻击者靠
//     "绝对路径报 X、相对路径报 Y"的差异可以逐段猜出挂载表）；
//   - 无挂载覆盖或命中 hidden（未识别模式亦按 hidden）→ ErrUnavailable；
//   - need=PathWrite 而挂载为 read → ErrReadonly。
func (r *fsResolver) Resolve(nsx *types.Namespace, p string, need types.PathMode) (types.PathResolution, error) {
	// need 的 fail-closed 判定先于一切：未识别（含零值）= hidden 的能力序，
	// 即"什么都做不了"，但仍给出 RealPath 让错误定位可用。
	if !need.Valid() {
		need = types.PathHidden
	}

	clean, err := normalizeInRoot(p)
	if err != nil {
		return types.PathResolution{}, fmt.Errorf("ns: %w: %s", ErrUnavailable, p)
	}

	_, mode := bestMount(nsx, clean)
	if mode == types.PathHidden {
		return types.PathResolution{}, fmt.Errorf("ns: %w: %s", ErrUnavailable, clean)
	}
	// 能力序：写需求必须以 write 挂载满足；只读挂载对写请求是 ErrReadonly。
	if need == types.PathWrite && mode != types.PathWrite {
		return types.PathResolution{}, fmt.Errorf("ns: %w: %s", ErrReadonly, clean)
	}

	// symlink 消解必须在"模式判定"之后、返回 RealPath 之前：直接 EvalSymlinks
	// 若把目标指到根外，后续的写入就落在命名空间外了（Part 5.4：resolve 后
	// 再判定），因此这里对解析出的真实路径做一次越界复查。
	abs := filepath.Join(r.root, filepath.FromSlash(clean))
	real, err := realpathInRoot(abs, r.root)
	if err != nil {
		return types.PathResolution{}, fmt.Errorf("ns: %w: %s", ErrUnavailable, clean)
	}
	return types.PathResolution{RealPath: real, Mode: mode}, nil
}

// CanWrite 实现 types.Resolver.CanWrite（只读快捷通道）。
// 失败被折叠为 false（契约如此）；要区分"不存在与只读"须直调 Resolve。
func (r *fsResolver) CanWrite(nsx *types.Namespace, p string) bool {
	_, err := r.Resolve(nsx, p, types.PathWrite)
	return err == nil
}

// normalizeInRoot 把调用方给的"项目相对路径"归一化为 slash 形式的干净相对路径。
//
// 拒绝三类输入（全部按 ErrUnavailable 对外，理由见 Resolve）：
//   - 空路径；绝对路径（/ 开头，或 Windows 盘符——本项目的相对路径约定是
//     POSIX 风格，见 Part 4.2 工具参数）；
//   - 任何 ".." 路径段：宁可拒绝也不消解。消解后的"等价路径"会绕过挂载表
//     的模式匹配吗？不会——消解后按挂载匹配仍是对的——但消解允许"挂载外
//     的表达方式"存在，逐段拒绝把越界语句挡在匹配之前，结果一致且解释更短。
func normalizeInRoot(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if path.IsAbs(p) || filepath.IsAbs(p) {
		return "", errors.New("absolute path")
	}
	// 在 Clean **之前**检查原始段：Clean 会把 "/"../out" 消解成 "/out"，
	// 事后检查就只剩空段了。逐段看的是用户给的字节，不是归一化产物。
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", errors.New("path escapes root")
		}
	}
	clean := path.Clean(p)
	// "." = workspace 根本身（list_dir 的默认形态）；挂载 "**" 覆盖零段路径。
	return clean, nil
}

// bestMount 在挂载表里挑最具体的匹配（模式串更长者胜；同长 hidden 优先）。
//
// 时间复杂度 O(len(Mounts))，每 Agent 的挂载表是个位数条目，不值得引入
// 前缀树这类精巧结构（先穷尽简单方案：常数上更优的是直接全量扫描）。
//
// 未识别的 PathMode（含零值）按 PathHidden 处理——types.PathMode 的
// fail-closed 契约由本函数兜底，技能层不必各自判。
func bestMount(nsx *types.Namespace, cleanPath string) (types.Mount, types.PathMode) {
	if nsx == nil {
		return types.Mount{}, types.PathHidden
	}
	var best types.Mount
	bestMode := types.PathHidden
	found := false
	for _, m := range nsx.Mounts {
		if !globMatch(m.Pattern, cleanPath) {
			continue
		}
		mode := m.Mode
		if !mode.Valid() {
			mode = types.PathHidden
		}
		if !found || len(m.Pattern) > len(best.Pattern) ||
			(len(m.Pattern) == len(best.Pattern) && modeRank(m.Mode) > modeRank(bestMode)) {
			// 平局（同长 pattern 多挂载）取能力更强的方向：挂载表是
			// "授权集合"而非"覆盖规则"——显式请求的 write 挂载（先过
			// Subset 裁决）不该被代表继承面的 read 挂载压制（真机踩过
			// 的坑：孙请求的 writable 与继承的 read 同 pattern，先序的
			// read 挂载赢得了"最具体"平局，写被静默降级成 PATH_READONLY）。
			// 授权是合法的（裁决层已查）—— 合并方向是 max。
			best, bestMode = m, mode
			found = true
		}
	}
	if !found {
		return types.Mount{}, types.PathHidden
	}
	return best, bestMode
}

// globMatch 判定 cleanPath 是否被 pattern 覆盖。
//
// 语义（gitignore 风格的受限子集，Part 5.1）：
//
//   - 段分隔按 slash；pattern 以 "src/**" 之类的段为单元；
//   - "*" 匹配一个段内任意字符（不跨段）；
//   - "**" 独占一个段时匹配"零个或多个段"（"src/**" 覆盖 src 本身）；
//     出现在段内其它位置视为普通 glob（阶段 2 不用，行为交由 filepath 匹配）；
//   - 其余按 path.Match 逐段匹配。
//
// 与标准 filepath.Match 的差异只有一处：** 的跨段能力。不引第三方 glob 库
// （依赖只为此一处功能不值得；先穷尽简单方案）。
func globMatch(pattern, cleanPath string) bool {
	return matchSegments(splitSeg(pattern), splitSeg(cleanPath))
}

func splitSeg(p string) []string {
	p = path.Clean(p)
	if p == "." {
		return nil
	}
	return strings.Split(p, "/")
}

func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	switch pat[0] {
	case "**":
		// 空 + 各占一段：覆盖"零段"与"任意多段"。
		for skip := 0; skip <= len(segs); skip++ {
			if matchSegments(pat[1:], segs[skip:]) {
				return true
			}
		}
		return false
	default:
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil || !ok {
			return false
		}
		return matchSegments(pat[1:], segs[1:])
	}
}

// realpathInRoot 解析 abs 的 symlink 并确认落在 root 之内。
//
// 目标不存在时对最长存在的祖先链做消解——file_write 的目标文件尚未创建
// 是正常路径，不能因为"还写不出来"就拒绝（tmp+rename 的目标在写入前
// 恰好存在 tmp，故对已存在部分消解即可保证目录段未逃逸）。
//
// [推断] 祖先链消解在并发改名的竞争下有 TOCTOU 窗口（判定后、打开前目录被
// 换成指向根外的 symlink）。本实现不为此引入 O_PATH / openat2：阶段 2 的
// 写路径受 Resolver 校验 + 原子写（tmp + rename 双保险），完整修复需要
// openat2 的 RESOLVE_BENEATH（[版本依赖: Linux 5.6+]），挂在 file_write
// 的 hardening 待办里，不在本包展开。
func realpathInRoot(abs, root string) (string, error) {
	cur := abs
	var suffix []string
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			out := filepath.Join(real, filepath.Join(suffix...))
			rel, err := filepath.Rel(root, out)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return "", errors.New("resolved path escapes workspace")
			}
			return out, nil
		}
		if cur == root {
			return "", errors.New("root does not exist")
		}
		parent := filepath.Dir(cur)
		suffix = append([]string{filepath.Base(cur)}, suffix...)
		if parent == cur {
			return "", err
		}
		cur = parent
	}
}

// modeRank 是 PathMode 的能力序（hidden=0 < read=1 < write=2）；未识别
// 值按 0（与 bestMount 的 fail-closed 契约同向）。
func modeRank(m types.PathMode) int {
	switch m {
	case types.PathWrite:
		return 2
	case types.PathRead:
		return 1
	default:
		return 0
	}
}
