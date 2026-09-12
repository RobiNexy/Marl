//go:build ignore

package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ────────────────────────────── file_search ──────────────────────────────
//
// 设计文档建议后端 ripgrep；[推断] 本机未预装 rg 且纯二进制部署要求自包含，
// M1 用纯 Go 遍历实现（等价语义：跳过二进制、排除目录剪枝、匹配行截断）。
// 若真实负载出现性能瓶颈，再按 rg --json 后端替换，接口不变。

type fileSearchSkill struct{}

func (s *fileSearchSkill) Name() string { return "file_search" }
func (s *fileSearchSkill) Description() string {
	return "Search across files. Supports regex. Returns file paths and matching line snippets."
}
func (s *fileSearchSkill) Capability() Capability { return CapReadOnly }

type searchHit struct {
	Path    string      `json:"path"`
	Matches []searchOne `json:"matches"`
}

type searchOne struct {
	Line    int      `json:"line"`
	Before  []string `json:"before,omitempty"`
	Content string   `json:"content"`
	After   []string `json:"after,omitempty"`
}

func (s *fileSearchSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	pattern := strArg(args, "pattern", "")
	if pattern == "" {
		return nil, errf("BAD_ARGS", "pattern is required")
	}
	base, err := ec.Resolve(strArg(args, "path", "."))
	if err != nil {
		return nil, err
	}
	useRegex := boolArg(args, "regex", false)
	cs := boolArg(args, "case_sensitive", false)
	beforeN := clamp(intArg(args, "context_before", 1), 0, 3)
	afterN := clamp(intArg(args, "context_after", 1), 0, 3)
	maxResults := clamp(intArg(args, "max_results", 20), 1, 100)
	outputMode := strArg(args, "output_mode", "snippets")
	globs := strSliceArg(args, "glob")
	excls := strSliceArg(args, "exclude_glob")
	if len(excls) == 0 {
		excls = defaultExcludes
	}

	var re *regexp.Regexp
	var needle string
	if useRegex {
		expr := pattern
		if !cs {
			expr = "(?i)" + expr
		}
		re, err = regexp.Compile(expr)
		if err != nil {
			return nil, errf("BAD_REGEX", "invalid regex %q: %v", pattern, err)
		}
	} else if cs {
		needle = pattern
	} else {
		needle = strings.ToLower(pattern)
	}

	var hits []searchHit
	total := 0
	truncated := false

	walkErr := filepath.WalkDir(base, func(p string, d os.DirEntry, werr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if werr != nil {
			return nil // 跳过不可访问路径 [推断]
		}
		rel, rErr := filepath.Rel(ec.WorkspaceRoot, p)
		if rErr != nil {
			return nil
		}
		if d.IsDir() {
			for _, pat := range excls { // 剪枝：排除目录不进入递归
				if ok, mErr := matchGlob(pat, rel+"/**"); mErr == nil && ok {
					return filepath.SkipDir
				}
				if ok, mErr := matchGlob(pat, rel); mErr == nil && ok {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // 符号链接不跟随
		}
		if len(globs) > 0 {
			matched := false
			for _, g := range globs {
				if ok, mErr := matchGlob(g, rel); mErr == nil && ok {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}
		data, rErr := os.ReadFile(p)
		if rErr != nil || isBinary(data) {
			return nil // 二进制忽略（等价 rg -I）
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		var ms []searchOne
		for i, ln := range lines {
			var hit bool
			switch {
			case re != nil:
				hit = re.MatchString(ln)
			case cs:
				hit = strings.Contains(ln, needle)
			default:
				hit = strings.Contains(strings.ToLower(ln), needle)
			}
			if !hit {
				continue
			}
			total++
			if len(ms) < maxResults {
				ms = append(ms, buildMatch(lines, i, beforeN, afterN))
			}
			if total >= maxResults { // 达到上限立即终止遍历
				truncated = true
				hits = append(hits, searchHit{Path: rel, Matches: ms})
				return filepath.SkipAll
			}
		}
		if len(ms) > 0 {
			hits = append(hits, searchHit{Path: rel, Matches: ms})
		}
		return nil
	})
	if walkErr != nil && walkErr != context.Canceled && walkErr != filepath.SkipAll {
		return nil, fmt.Errorf("file_search: %w", walkErr)
	}

	res := Result("query", pattern, "total_matches", total, "truncated", truncated)
	if outputMode == "files" { // 极致压缩模式：只报文件路径与计数
		files := make([]map[string]any, 0, len(hits))
		for _, h := range hits {
			files = append(files, map[string]any{"path": h.Path})
		}
		res["files"] = files
	} else {
		res["results"] = hits
	}
	return res, nil
}

// buildMatch 匹配行截断 200 字符，上下文行截断 150 字符（Part 4.4.3）。
// 匹配行不展开全文——区别于 grep -A20 的 Token 浪费。
func buildMatch(lines []string, idx, beforeN, afterN int) searchOne {
	trunc := func(s string, n int) string {
		r := []rune(s) // 多字节字符按字符数截断
		if len(r) <= n {
			return s
		}
		return string(r[:n]) + "..."
	}
	m := searchOne{Line: idx + 1, Content: trunc(lines[idx], 200)}
	for j := idx - beforeN; j < idx; j++ {
		if j >= 0 {
			m.Before = append(m.Before, trunc(lines[j], 150))
		}
	}
	for j := idx + 1; j <= idx+afterN && j < len(lines); j++ {
		m.After = append(m.After, trunc(lines[j], 150))
	}
	return m
}
