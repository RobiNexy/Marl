// Package skill 实现原子技能库（设计文档 Part 4）。

//go:build ignore

package skill

//
// 边界契约：真正的"技能"只包含对外部世界（文件系统、命令行、环境）的读写；
// fork/human/reconfigure/branch 都是"意图"，走各自裁决关口，绝不进入本包。
//
// 硬保证（能力约束代替惩罚，Part 0 原则 3）：
//   - 所有路径经 ExecContext.Resolve 沙箱校验：规范化后必须位于 workspace 内，
//     符号链接不逃逸（对最深已存在祖先做 EvalSymlinks 后复查）。
//   - mutating 技能执行前检查 writable_paths 所有权；空列表 = 无限制（仅根 Agent），
//     子 Agent 必须显式分配。这不是提示词约束，是技能层硬拦截。

import (
	"fmt"
	"path/filepath"
	"strings"
)

type Capability string

const (
	CapReadOnly   Capability = "read_only"
	CapMutating   Capability = "mutating"
	CapStructural Capability = "structural"
)

// ExecContext 一次技能执行的环境边界。由框架构造，技能不可篡改。
type ExecContext struct {
	WorkspaceRoot string   // 绝对路径
	SnapshotDir   string   // .agents/snapshots 绝对路径
	LogDir        string   // .agents/logs 绝对路径
	WritablePaths []string // glob 列表；空 = 允许全部（根 Agent 特权）
	// Prompts 提示词索引的函数注入（Patch 2/3：list_prompts 技能的数据源）。
	// nil = 提示词索引不可用，list_prompts 返回结构化错误。
	Prompts func() []PromptSummary
	// WebCacheDir web_fetch 的磁盘缓存目录（.agents/web_cache）。
	// 空 = 禁用缓存（每次直接抓网）。仅缓存路径，不限制网络行为。
	WebCacheDir string
	// ProcessEvent shell_spawn 进程退出事件的观察者回调（M4 异步事件循环）。
	// 由框架注入（转发到 Aggregator 广播）；nil = 无人订阅，事件仅落盘。
	// 技能不得阻塞在此回调里——回调方保证快速返回（channel 发送）。
	ProcessEvent func(ev map[string]any)
	// PapersDir arxiv_fetch 的 PDF/文本存储目录（.agents/papers，Patch 1）。
	// 空 = 禁用 arxiv_fetch（返回结构化错误）。
	PapersDir string
}

// PromptSummary list_prompts 返回的索引项（补丁 3 的返回契约）。
// 看 prompt 全文直接 file_read 对应 md——技能只给索引，不搬运内容。
type PromptSummary struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// Resolve 将调用方提供的路径规范化为 workspace 内绝对路径。
// 三道防线：
//  1. 相对路径锚定 WorkspaceRoot，Clean 消解 ".."
//  2. 规范化结果必须仍在 workspace 内（Rel 前缀校验，防 "/root-x" 误匹配 "/root"）
//  3. 对最深已存在祖先做 EvalSymlinks——符号链接指向 workspace 外则拒绝。
//     注意：第 3 步只校验不重写返回值，保持调用方语义为"逻辑路径"。[推断]
//     校验与后续 open 之间存在 TOCTOU 窗口（链接被替换），单人本地场景可接受；
//     多租户场景需内核级约束（openat2 RESOLVE_BENETH）。
func (ec *ExecContext) Resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(ec.WorkspaceRoot, p)
	}
	if !withinRoot(ec.WorkspaceRoot, abs) {
		return "", &SkillError{
			Type: "PATH_OUTSIDE_WORKSPACE",
			Msg:  fmt.Sprintf("path %q resolves outside workspace %q", p, ec.WorkspaceRoot),
			Hint: "Use a path inside the workspace.",
		}
	}
	real, err := deepestExistingReal(abs)
	if err != nil {
		return "", fmt.Errorf("skill.Resolve %q: stat walk: %w", p, err)
	}
	if !withinRoot(ec.WorkspaceRoot, real) {
		return "", &SkillError{
			Type: "PATH_OUTSIDE_WORKSPACE",
			Msg:  fmt.Sprintf("path %q escapes workspace via symlink to %q", p, real),
			Hint: "Symlinks pointing outside the workspace are not allowed.",
		}
	}
	return abs, nil
}

// withinRoot 判断 target 是否位于 root 内（含 root 本身）。
func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel))
}

// deepestExistingReal 返回 path 中最深已存在祖先的真实路径（解引用符号链接后）。
func deepestExistingReal(path string) (string, error) {
	p := path
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			// 到达文件系统根仍不存在：返回原路径交由上层处理
			return path, nil
		}
		p = parent
	}
}

// checkWritable mutating 技能的所有权硬校验（设计文档 11.10 层 1）。
func (ec *ExecContext) checkWritable(absPath string) error {
	if len(ec.WritablePaths) == 0 {
		return nil // 根 Agent：无限制
	}
	rel, err := filepath.Rel(ec.WorkspaceRoot, absPath)
	if err != nil {
		return &SkillError{Type: "PATH_NOT_OWNED", Msg: fmt.Sprintf("cannot relativize %q", absPath)}
	}
	for _, pat := range ec.WritablePaths {
		ok, mErr := matchGlob(pat, rel)
		if mErr == nil && ok {
			return nil
		}
	}
	return &SkillError{
		Type: "PATH_NOT_OWNED",
		Msg:  fmt.Sprintf("path %q is outside this agent's writable_paths %v", rel, ec.WritablePaths),
		Hint: "Ask the parent agent for write access to this path.",
	}
}

// matchGlob 极简 glob，支持 "**" 跨目录段（标准 path.Match 不支持）。
// [推断] 不引入 doublestar 依赖：仅需 include/exclude 一种用法，30 行实现
// 换来少一个外部依赖，符合纯二进制部署取向。规则：
//   - "**" 段匹配任意层级（含零段）
//   - "*" 匹配单段内任意字符（不含 "/"）
//   - "?" 匹配单个字符
func matchGlob(pattern, name string) (bool, error) {
	if pattern == "" {
		return false, fmt.Errorf("empty glob pattern")
	}
	ps := strings.Split(filepath.ToSlash(pattern), "/")
	ns := strings.Split(filepath.ToSlash(name), "/")
	return matchSegments(ps, ns), nil
}

func matchSegments(ps, ns []string) bool {
	if len(ps) == 0 {
		return len(ns) == 0
	}
	if ps[0] == "**" {
		for i := 0; i <= len(ns); i++ { // "**" 吞掉 0..n 段
			if matchSegments(ps[1:], ns[i:]) {
				return true
			}
		}
		return false
	}
	if len(ns) == 0 {
		return false
	}
	ok, err := filepath.Match(ps[0], ns[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(ps[1:], ns[1:])
}
