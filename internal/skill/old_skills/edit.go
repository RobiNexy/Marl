//go:build ignore

package skill

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// ────────────────────────────── file_edit ──────────────────────────────
//
// 最精巧的技能（Part 4.4.5）。核心契约：
//   - 锚点严格匹配（含缩进、换行），不做模糊匹配——模糊即安全隐患
//   - 统一换行符匹配，写回时保留原文件风格（\n 或 \r\n）
//   - 失败时返回 candidates_snippet / locations：LLM 看一眼就能纠正，
//     不必重读整个文件。这是 Token 经济性的微观体现。
//   - 复用原子写与快照模块；路径所有权校验

type fileEditSkill struct{}

func (s *fileEditSkill) Name() string { return "file_edit" }
func (s *fileEditSkill) Description() string {
	return "Precise, atomic edit. Uses old_string as anchor, replaces with new_string. No partial writes."
}
func (s *fileEditSkill) Capability() Capability { return CapMutating }

func (s *fileEditSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	path, err := ec.Resolve(strArg(args, "path", ""))
	if err != nil {
		return nil, err
	}
	if err := ec.checkWritable(path); err != nil {
		return nil, err
	}
	mode := strArg(args, "mode", "replace")
	switch mode {
	case "replace", "insert":
	default:
		return nil, errf("BAD_ARGS", "invalid mode %q", mode)
	}
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	if mode == "replace" && oldStr == "" {
		return nil, errf("BAD_ARGS", "old_string is required in replace mode")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errf("NOT_FOUND", "file %q not found", path)
		}
		return nil, fmt.Errorf("file_edit %q: %w", path, err)
	}

	// 换行符统一：CRLF 文件在 \n 视图上匹配，写回时还原风格
	crlf := strings.Contains(string(raw), "\r\n")
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")

	var out string
	replacements := 0
	sliceInfo := map[string]any{}

	if mode == "insert" {
		pos := strArg(args, "position", "after")
		switch pos {
		case "after", "before":
		default:
			return nil, errf("BAD_ARGS", "invalid position %q (insert mode)", pos)
		}
		var insertAt int
		if oldStr == "" {
			// 空锚点约定：before=文件头插入，after=文件尾追加 [推断]（文档未明说）
			if pos == "before" {
				insertAt = 0
			} else {
				insertAt = len(text)
			}
		} else {
			idx, _, lErr := locateAnchor(text, oldStr, args)
			if lErr != nil {
				return nil, lErr
			}
			if pos == "after" {
				insertAt = idx + len(oldStr)
			} else {
				insertAt = idx
			}
			replacements = 1 // 锚点定位成功即视为一次有效编辑
		}
		out = text[:insertAt] + newStr + text[insertAt:]
		sl, sc := lineCol(out, insertAt-len(newStr))
		el, ec2 := lineCol(out, insertAt)
		sliceInfo = map[string]any{"start_line": sl, "start_col": sc, "end_line": el, "end_col": ec2}
	} else { // replace
		idx, cnt, lErr := locateAnchor(text, oldStr, args)
		if lErr != nil {
			return nil, lErr
		}
		if boolArg(args, "replace_all", false) {
			replacements = cnt
			out = strings.ReplaceAll(text, oldStr, newStr)
			sl, sc := lineCol(out, idx)
			sliceInfo = map[string]any{"first_start_line": sl, "start_col": sc}
		} else {
			replacements = 1
			out = text[:idx] + newStr + text[idx+len(oldStr):]
			sl, sc := lineCol(out, idx)
			el, ec2 := lineCol(out, idx+len(newStr))
			sliceInfo = map[string]any{"start_line": sl, "start_col": sc, "end_line": el, "end_col": ec2}
		}
	}

	// 写回时还原原文件的换行风格（Part 4.4.5）
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	changed := out != string(raw)
	if !changed {
		return Result("mode", mode, "path", path,
			"replacements", replacements, "slice", sliceInfo, "changed", false), nil
	}

	if _, sErr := snapshotFile(ec, path); sErr != nil {
		return nil, fmt.Errorf("file_edit %q: snapshot: %w", path, sErr)
	}
	perm := os.FileMode(0o644)
	if fi, statOk := os.Stat(path); statOk == nil {
		perm = fi.Mode().Perm()
	}
	if wErr := writeFileAtomic(path, []byte(out), perm); wErr != nil {
		return nil, wErr
	}
	return Result("mode", mode, "path", path,
		"replacements", replacements, "slice", sliceInfo, "changed", true), nil
}

// locateAnchor 定位锚点第 N 次出现。规则：
//   - 出现次数为 0 → NO_MATCH（附 candidates_snippet 候选上下文）
//   - 次数 >1 且未指定 occurrence 且非 replace_all → MULTIPLE_MATCHES（附行号列表）
//   - occurrence 显式给出时按其定位（1 起；-1 为最后一次）；越界报错
//
// "显式给出"必须探测参数存在性而非取默认值：默认 1 与用户显式 1 语义不同——
// 前者在多匹配时应报错，后者是用户看过 locations 后的明确选择。
func locateAnchor(text, old string, args map[string]any) (int, int, error) {
	cnt := strings.Count(text, old)
	if cnt == 0 {
		return 0, 0, &SkillError{
			Type:  "NO_MATCH",
			Msg:   fmt.Sprintf("old_string not found (%d chars)", len(old)),
			Hint:  "Check indentation and whitespace. Nearby content is in candidates_snippet.",
			Extra: map[string]any{"candidates_snippet": bestEffortSnippet(text, old)},
		}
	}
	explicit, hasExplicit := args["occurrence"]
	replaceAll := boolArg(args, "replace_all", false)
	if cnt > 1 && !replaceAll && !hasExplicit {
		return 0, cnt, &SkillError{
			Type:  "MULTIPLE_MATCHES",
			Msg:   fmt.Sprintf("old_string occurs %d times", cnt),
			Hint:  "Add more context, use replace_all=true, or use occurrence=N.",
			Extra: map[string]any{"locations": allLineNumbers(text, old)},
		}
	}
	n := 1
	if hasExplicit {
		n = toInt(explicit, 1)
		if n < 0 {
			n = cnt // -1 = 最后一次
		}
		if n < 1 || n > cnt {
			return 0, cnt, errf("OCCURRENCE_OUT_OF_RANGE",
				"occurrence=%d but only %d matches exist", n, cnt)
		}
	}
	idx := -1
	for i := 0; i < n; i++ {
		offset := 0
		if idx >= 0 {
			offset = idx + 1
		}
		idx = offset + strings.Index(text[offset:], old)
	}
	return idx, cnt, nil
}

func toInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		var n int
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
