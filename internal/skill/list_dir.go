package skill

// list_dir —— 列目录结构（Part 4.4.1）。
//
// 延用 old_skills 的实现骨架：迭代遍历；符号链接不跟随；排除目录剪枝；
// 最大遍历节点数防卡死；字典序确定性排序保证分页可重现。
// 阶段 2 的 13.4 指定形态：BFS 遍历、depth 限制、exclude 预置列表。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"marl/internal/types"
)

// defaultExcludes 是阶段 2 的预置排除清单（args 未给 exclude 时的默认值）。
// git 垃圾与包目录是 LLM 因果探索的噪声，从默认态剔除；目录的省略由
// isExcluded 的目录级 candidates 展开（见函数注释）。
var defaultExcludes = []string{
	"node_modules/**", ".git/**", "__pycache__/**", "*.pyc", "dist/**", "build/**",
}

const (
	maxDepthForWalk = 6     // depth 上限（防失控递归）
	maxEntries      = 200   // 结果上限（token 经济性：返回的树要能在上下文里读完）
	maxWalkNodes    = 10000 // 防大仓库卡死；达到上限不报错，只标记 truncated
)

type listDirSkill struct{}

// ListDir 是 list_dir 的共享实例。
var ListDir Skill = &listDirSkill{}

func (s *listDirSkill) Name() string { return SkillListDir }
func (s *listDirSkill) Description() string {
	return "List directory structure (BFS). depth default 2; directories and files are returned as a tree sorted deterministically."
}
func (s *listDirSkill) Kind() SkillKind { return SkillReadOnly }

// Parameters 逐字节写死（冻结前缀，见 ToolSchema 契约）。
func (s *listDirSkill) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"path":{"type":"string","description":"Path relative to the workspace root; '.' for the whole workspace"},` +
		`"depth":{"type":"integer","description":"recursion depth (1..6, default 2)"},` +
		`"exclude":{"type":"array","items":{"type":"string"},"description":"glob patterns to prune (default: .git, node_modules, __pycache__, dist, build, *.pyc)"}},` +
		`"required":[]}`)
}

// dirNode 是树形结构的节点（JSON 序列化即协议返回体）。
type dirNode struct {
	Type     string     `json:"type"`
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	Children []*dirNode `json:"children,omitempty"`
	Size     int64      `json:"size,omitempty"`
}

// Execute 实现 Skill.Execute。
func (s *listDirSkill) Execute(ctx context.Context, args map[string]any, env *SkillEnv) (*SkillResult, error) {
	res, err := resolvePath(env, strArg(args, "path", "."), types.PathRead)
	if err != nil {
		return businessOf(err)
	}
	info, statErr := os.Stat(res.RealPath)
	if statErr != nil {
		// [阶段 12 修正/真机发现 #9] ENOENT 是环境事实（目录不存在），
		// 折算成普通失败回填模型（可据此换路径/先建目录）——裸 error 会被
		// 执行器归类为 infra failure 并**终结整个 Run**（真机：一个不存在
		// 的目录杀死了两个孙 Agent）。框架级故障（权限外的 IO 异常）仍走
		// error 通道。
		return NewFailure(ErrNotFound, "path not available (ENOENT): %s", strArg(args, "path", ".")), nil
	}
	if !info.IsDir() {
		return NewFailure("NOT_A_DIRECTORY", "%s is a file, use file_read instead", res.RealPath), nil
	}
	depth := clamp(intArg(args, "depth", 2), 1, maxDepthForWalk)
	excl := strSliceArg(args, "exclude")
	if len(excl) == 0 {
		excl = defaultExcludes
	}

	var total int
	var truncated bool
	var walk func(dir, rel string, d int) []*dirNode
	walk = func(dir, rel string, d int) []*dirNode {
		if ctx.Err() != nil || total >= maxWalkNodes {
			truncated = true
			return nil
		}
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return nil // 单个目录不可读不致命，跳过（可观测性：不影响整体树）
		}
		// 确定性排序：目录优先，其余按名典序——分页结果可重现的前提
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		var out []*dirNode
		for _, ent := range entries {
			name := ent.Name()
			if strings.HasPrefix(name, ".") {
				continue // dot-files 默认不列（.git 与其它配置的位置不参与对话）
			}
			childRel := name
			if rel != "" && rel != "." {
				childRel = rel + "/" + name
			}
			if isExcluded(excl, childRel, ent.IsDir()) {
				continue
			}
			total++
			n := &dirNode{Name: name, Path: childRel}
			switch {
			case ent.IsDir():
				n.Type = "dir"
				if d+1 < depth { // 剪枝：不进入超深递归
					n.Children = walk(filepath.Join(dir, name), childRel, d+1)
				}
			case ent.Type().IsRegular():
				n.Type = "file"
				if fi, sizeErr := ent.Info(); sizeErr == nil {
					n.Size = fi.Size()
				}
			default:
				continue // 符号链接/设备等不跟随、不展示（handler 循环与逃逸）
			}
			out = append(out, n)
			if total >= maxEntries { // 结果上限：树形下只报告截断事实
				truncated = true
				break
			}
		}
		return out
	}

	tree := walk(res.RealPath, relFromWorkspace(env, res.RealPath), 0)
	return &SkillResult{OK: true, Data: map[string]any{
		"path":          relOrDot(relFromWorkspace(env, res.RealPath)),
		"total_entries": total,
		"truncated":     truncated,
		"tree":          tree,
	}}, nil
}

// isExcluded 目录级排除需覆盖其子树前缀匹配（old_skills 的展开语义）。
func isExcluded(patterns []string, rel string, isDir bool) bool {
	candidates := []string{rel}
	if isDir {
		candidates = append(candidates, rel+"/**")
	}
	for _, pat := range patterns {
		for _, c := range candidates {
			if ok, mErr := matchGlob(pat, c); mErr == nil && ok {
				return true
			}
		}
	}
	return false
}

// matchGlob 极简 glob，支持 "**" 跨目录段（标准 path.Match 不支持）。
// 与 ns 的匹配规则同源：阶段 2 两处各持一份是刻意的小重复（语义用途不同——
// 挂载匹配 vs 排除剪枝），合并的时机是两处要求更强的语义联动（ADR 记录）。
func matchGlob(pattern, name string) (bool, error) {
	if pattern == "" {
		return false, fmt.Errorf("empty glob pattern")
	}
	ps := strings.Split(filepath.ToSlash(pattern), "/")
	nsSegs := strings.Split(filepath.ToSlash(name), "/")
	return matchSegments(ps, nsSegs), nil
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

func relOrDot(rel string) string {
	if rel == "" || rel == "." {
		return "."
	}
	return rel
}
