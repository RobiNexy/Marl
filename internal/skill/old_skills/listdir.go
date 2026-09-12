//go:build ignore

package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ────────────────────────────── list_dir ──────────────────────────────
//
// 实现要点（Part 4.4.1）：BFS/DFS 迭代遍历；符号链接不跟随；排除目录剪枝；
// 最大遍历节点数防卡死；字典序确定性排序保证分页可重现。

var defaultExcludes = []string{
	"node_modules/**", ".git/**", "__pycache__/**", "*.pyc", "dist/**", "build/**",
}

const maxWalkNodes = 10000

type listDirSkill struct{}

func (s *listDirSkill) Name() string { return "list_dir" }
func (s *listDirSkill) Description() string {
	return "List directory contents in a structured way. Supports depth control, filtering."
}
func (s *listDirSkill) Capability() Capability { return CapReadOnly }

type dirNode struct {
	Type     string     `json:"type"`
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	Children []*dirNode `json:"children,omitempty"`
	Size     int64      `json:"size,omitempty"`
}

func (s *listDirSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	root, err := ec.Resolve(strArg(args, "path", "."))
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errf("NOT_FOUND", "path %q not found", root)
		}
		return nil, fmt.Errorf("list_dir: %w", err)
	}
	if !info.IsDir() {
		return nil, errf("NOT_A_DIRECTORY", "%q is a file, use file_read instead", root)
	}
	depth := clamp(intArg(args, "depth", 2), 1, 6)
	limit := clamp(intArg(args, "limit", 200), 1, 1000)
	showHidden := boolArg(args, "show_hidden", false)
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
		entries, rErr := os.ReadDir(dir)
		if rErr != nil {
			return nil // 单个目录不可读不致命，跳过 [推断]
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
			if !showHidden && strings.HasPrefix(name, ".") {
				continue
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
				if fi, sErr := ent.Info(); sErr == nil {
					n.Size = fi.Size()
				}
			default:
				continue // 符号链接/设备等不跟随、不展示（防循环与逃逸）
			}
			out = append(out, n)
			if total >= limit { // limit 截断：树形下只报告截断事实
				truncated = true
				break
			}
		}
		return out
	}

	rel := ""
	if abs, rErr := filepath.Rel(ec.WorkspaceRoot, root); rErr == nil {
		rel = abs
	}
	tree := walk(root, rel, 0)
	return Result(
		"path", relOrDot(rel),
		"total_entries", total,
		"truncated", truncated,
		"tree", tree,
	), nil
}

func isExcluded(patterns []string, rel string, isDir bool) bool {
	candidates := []string{rel}
	if isDir {
		candidates = append(candidates, rel+"/**") // 目录级排除需覆盖其子树前缀匹配
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

func relOrDot(rel string) string {
	if rel == "" || rel == "." {
		return "."
	}
	return rel
}
