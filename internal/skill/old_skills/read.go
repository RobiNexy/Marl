//go:build ignore

package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ────────────────────────────── file_read ──────────────────────────────
//
// Token 经济性主战场（Part 4.4.2）：auto 模式下大文件自动走 summary；
// content 模式强制 limit 上限 200 行，附行号与 eof/truncated 标志。

const readHardLimit = 200

type fileReadSkill struct{}

func (s *fileReadSkill) Name() string { return "file_read" }
func (s *fileReadSkill) Description() string {
	return "Read a file. Use mode='summary' for structural overview (signatures, headings, JSON structure). Use mode='content' for raw lines."
}
func (s *fileReadSkill) Capability() Capability { return CapReadOnly }

func (s *fileReadSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	path, err := ec.Resolve(strArg(args, "path", ""))
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errf("NOT_FOUND", "file %q not found", path)
		}
		if os.IsPermission(err) {
			return nil, errf("PERMISSION_DENIED", "cannot read %q: permission denied", path)
		}
		return nil, fmt.Errorf("file_read %q: %w", path, err)
	}
	if isBinary(raw) {
		return Result("path", path, "type", "binary", "size", len(raw)), nil
	}
	text := strings.TrimSuffix(string(raw), "\n")
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
	}

	mode := strArg(args, "mode", "auto")
	if mode == "auto" && len(lines) > 200 {
		mode = "summary" // 大文件自动摘要：LLM 无需自行判断
	}

	switch mode {
	case "summary":
		out := summarizeFile(path, lines)
		out["ok"] = true
		return out, nil
	default: // content
		offset := clamp(intArg(args, "offset", 1), 1, len(lines)+1)
		limit := clamp(intArg(args, "limit", 100), 1, readHardLimit)
		end := offset - 1 + limit
		if end > len(lines) {
			end = len(lines)
		}
		var sb strings.Builder
		for i := offset - 1; i < end; i++ {
			fmt.Fprintf(&sb, "%d: %s\n", i+1, lines[i])
		}
		eof := end >= len(lines)
		return Result(
			"path", path,
			"total_lines", len(lines),
			"offset", offset,
			"limit", end-offset+1,
			"content", sb.String(),
			"truncated", !eof,
			"eof", eof,
		), nil
	}
}

// summarizeFile 按文件类型返回结构化摘要（绝不包含函数体）。
// [推断] M1 用正则提取 Go 签名而非 tree-sitter：零依赖、二进制体积可控，
// 覆盖常用场景；tree-sitter 增强留待后续里程碑按需引入。
func summarizeFile(path string, lines []string) map[string]any {
	base := map[string]any{"path": path, "total_lines": len(lines)}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md":
		var outline []map[string]any
		for i, ln := range lines {
			if strings.HasPrefix(ln, "#") {
				level := len(ln) - len(strings.TrimLeft(ln, "#"))
				outline = append(outline, map[string]any{
					"level": level,
					"text":  strings.TrimSpace(strings.TrimLeft(ln, "# ")),
					"line":  i + 1,
				})
			}
		}
		base["type"], base["outline"] = "markdown", outline
		return base
	case ".json":
		var v any
		if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &v); err == nil {
			base["type"], base["structure"] = "json", jsonSummary(v, 0)
			return base
		} // 解析失败降级为文本预览
	case ".yaml", ".yml":
		var v any
		if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &v); err == nil {
			base["type"], base["structure"] = "yaml", jsonSummary(v, 0)
			return base
		}
	case ".go":
		var symbols []map[string]any
		sigRe := regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)|^type\s+([A-Za-z_][A-Za-z0-9_]*)`)
		for i, ln := range lines {
			if m := sigRe.FindStringSubmatch(ln); m != nil {
				kind, name := "function", m[1]
				if name == "" {
					kind, name = "type", m[2]
				}
				symbols = append(symbols, map[string]any{"kind": kind, "name": name, "line": i + 1})
			}
		}
		base["type"], base["symbols"] = "go", symbols
		return base
	}
	prev := lines
	truncated := false
	if len(prev) > 30 {
		prev, truncated = prev[:30], true
	}
	base["type"], base["preview"], base["truncated"] = "text", strings.Join(prev, "\n"), truncated
	return base
}

// jsonSummary 递归生成键层级描述；深度截断时输出 object (M keys) 形式（Part 4.4.2）。
func jsonSummary(v any, depth int) any {
	const maxDepth = 3
	switch t := v.(type) {
	case map[string]any:
		if depth >= maxDepth {
			return fmt.Sprintf("object (%d keys)", len(t))
		}
		out := make(map[string]any, len(t))
		for k, sv := range t {
			out[k] = jsonSummary(sv, depth+1)
		}
		return out
	case []any:
		if depth >= maxDepth || len(t) == 0 {
			return fmt.Sprintf("array[%d]", len(t))
		}
		return map[string]any{"array": fmt.Sprintf("array[%d]", len(t)), "first_elem": jsonSummary(t[0], depth+1)}
	case string:
		if len(t) > 50 {
			return t[:50] + "..."
		}
		return t
	default:
		return v
	}
}
