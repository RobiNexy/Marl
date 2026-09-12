package skill

// file_read —— Token 经济性主战场（Part 4.4）。
//
// 三种模式：auto（>200 行自动 summary）/ summary（结构化摘要，绝不带函数体）
// / content（原始内容 + 行号 + 分页上限 200 行）。
// 阶段 2 末扩展：图片文件以 Attachment 引用返回（Part 10.14 落地第一步）——
// 读图片不再是"二进制不可读"，而是 metadata + Attachment 进入上下文的通道。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"marl/internal/types"
)

const (
	readHardLimit = 200 // content 模式的行数硬上限
	summaryLines  = 30  // 纯文本 summary 的预览行数
	autoSummaryAt = 200 // auto 模式切换到 summary 的行数阈值
)

type fileReadSkill struct{}

// FileRead 是 file_read 的共享实例（纯函数语义、零状态，可全局复用）。
var FileRead Skill = &fileReadSkill{}

func (s *fileReadSkill) Name() string { return SkillFileRead }
func (s *fileReadSkill) Description() string {
	return "Read a file from the workspace. Modes: auto (default, large files summarize), summary (structural overview: signatures/headings/JSON structure), content (raw lines with line numbers)."
}
func (s *fileReadSkill) Kind() SkillKind { return SkillReadOnly }

// Parameters 逐字节写死（冻结前缀的一部分，见 ToolSchema 契约）。
func (s *fileReadSkill) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"path":{"type":"string","description":"Path relative to the workspace root"},` +
		`"mode":{"type":"string","enum":["auto","content","summary"],"description":"auto: content for small files, summary for large ones"},` +
		`"offset":{"type":"integer","description":"1-based start line (content mode only)"},` +
		`"limit":{"type":"integer","description":"max lines to return (content mode only)"}},"required":["path"]}`)
}

// Execute 实现 Skill.Execute。
//
// 失败分流（Skill 契约）：路径越界/ENOENT/只读 → 业务失败（SkillResult，
// 错误码见错误码表）；IO 故障 → 基础设施故障（error != nil）。Resolver
// 的"hidden 报 ENOENT"语义在这里的自然体现是：统一把 ErrUnavailable
// 翻出 ENOENT，不暴露挂载表结构。
func (s *fileReadSkill) Execute(ctx context.Context, args map[string]any, env *SkillEnv) (*SkillResult, error) {
	res, err := resolvePath(env, strArg(args, "path", ""), types.PathRead)
	if err != nil {
		return businessOf(err)
	}
	raw, err := os.ReadFile(res.RealPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewFailure(ErrNotFound, "file %s not found", res.RealPath), nil
		}
		return nil, fmt.Errorf("file_read %s: read: %w", res.RealPath, err) // IO 故障 → 基础设施错误
	}

	// 阶段 2 末：图片文件走 Attachment 通道（Part 10.14）。
	if mime, ok := imageMime(raw); ok {
		return imageResult(res, mime, raw)
	}
	if isBinary(raw) {
		return NewSuccess(map[string]any{
			"path": res.RealPath, "type": "binary", "size_bytes": len(raw),
		}), nil
	}

	text := strings.TrimSuffix(string(raw), "\n")
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
	}

	mode := strArg(args, "mode", "auto")
	if mode == "auto" && len(lines) > autoSummaryAt {
		mode = "summary" // 大文件自动摘要：LLM 无需自行判断
	}
	switch mode {
	case "summary":
		out := summarizeFile(res.RealPath, lines)
		out["ok"] = true
		return &SkillResult{OK: true, Data: out}, nil
	default: // content
		offset := clamp(intArg(args, "offset", 1), 1, len(lines)+1)
		limit := clamp(intArg(args, "limit", 100), 1, readHardLimit)
		end := offset - 1 + limit
		if end > len(lines) {
			end = len(lines)
		}
		// 行号右对齐 `N|`：与 split 的渲染统一（Part 4.4 的既有约定）。
		width := len(fmt.Sprint(end))
		var sb strings.Builder
		for i := offset - 1; i < end; i++ {
			fmt.Fprintf(&sb, "%*d| %s\n", width, i+1, lines[i])
		}
		eof := end >= len(lines)
		return &SkillResult{OK: true, Data: map[string]any{
			"path":        res.RealPath,
			"total_lines": len(lines), "offset": offset, "limit": end - offset + 1,
			"content": sb.String(), "truncated": !eof, "eof": eof,
		}}, nil
	}
}

// summarizeFile 按文件类型返回结构化摘要（绝不包含函数体）。
// 各分支的依据（延用 old_skills 的实现，取舍理由记录在案）：
//   - Markdown：标题大纲（1..6 级 + 行号）；
//   - JSON：键层级描述，深度 3 截断为 object (M keys)；
//   - Go：正则提取签名（[推断] tree-sitter 留到 summary 质量真正成为
//     痛点之后；正则零依赖且覆盖常用面）；
//   - YAML：无 schema 依赖（gopkg.in/yaml.v3 不进 build graph）→ 降级
//     为文本预览 [权衡: 补依赖的收益是少量 token 节省，先不付]；
//   - 其它：前 summaryLines 行预览。
func summarizeFile(path string, lines []string) map[string]any {
	base := map[string]any{"path": path, "total_lines": len(lines)}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md":
		var outline []map[string]any
		for i, ln := range lines {
			if strings.HasPrefix(ln, "#") {
				level := strings.Count(ln[:len(ln)-len(strings.TrimLeft(ln, "#"))], "#")
				outline = append(outline, map[string]any{
					"level": level, "text": strings.TrimSpace(strings.TrimLeft(ln, "# ")), "line": i + 1,
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
	if len(prev) > summaryLines {
		prev, truncated = prev[:summaryLines], true
	}
	base["type"], base["preview"], base["truncated"] = "text", strings.Join(prev, "\n"), truncated
	return base
}

// jsonSummary 递归生成键层级描述；深度截断时输出 object (M keys)（Part 4.4）。
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

// imageMime 按魔数识别常见图片格式。返回 (mime, true)。
//
// 识别表刻意最小（png/jpeg/gif/webp）：图片的下游消费是 Attachment，
// MimeType 必须是准确的 MIME 字符串（模型端解释字节的依据），认不出就让
// 它走 binary 分支——宁可"不认识"，不要"猜错 MIME"。
func imageMime(raw []byte) (string, bool) {
	switch {
	case len(raw) >= 8 && raw[0] == 0x89 && raw[1] == 'P' && raw[2] == 'N' && raw[3] == 'G':
		return "image/png", true
	case len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
		return "image/jpeg", true
	case len(raw) >= 6 && string(raw[:4]) == "GIF8":
		return "image/gif", true
	case len(raw) >= 12 && string(raw[8:12]) == "WEBP":
		return "image/webp", true
	}
	return "", false
}

// imageResult 构造图片的返回体（Part 10.14 落地第一步）。
//
// Attachment 按 types.Attachment 契约装配：Source=file_path（Data 是路径
// 字节而非内容——base64 不落 SQLite，也不进本结果体），消费点
// （Normalizer）按契约在发请求时才读文件。环境没有 vision 通道时，上层
// 只是把这段 metadata 当文本用，无副作用。
func imageResult(res resolvedPath, mime string, raw []byte) (*SkillResult, error) {
	att := types.Attachment{
		Kind:     types.AttachImage,
		MimeType: mime,
		Source:   types.SourceFile,
		Data:     []byte(res.RealPath),
	}
	if err := att.Validate(); err != nil {
		return nil, fmt.Errorf("file_read: invalid image attachment for %s: %w", res.RealPath, err)
	}
	return &SkillResult{OK: true, Data: map[string]any{
		"path": res.RealPath, "type": "image",
		"mime": mime, "size_bytes": len(raw),
		"attachment": att, // 引用而非内容（工作区内路径）
	}}, nil
}
