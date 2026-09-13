package config

// Emit：Node 树 → YAML 文本（配置写回的另一半；Parse 的镜像）。
//
// 用途：GUI/CLI 对配置的修改要落回文件——读入（Parse）→ 改树 → 写出
// （Emit）。只发**块形态**（嵌套缩进 + `- ` 列表），不发行内流式映射/
// 列表——两者在解析层等价，块形态是规范化形态（diff 友好、键序保持）。
//
// 往返保证：对 Parse 支持的输入形态，Parse(Emit(node)) 与原树**语义等价**
// （键序保持；标量的引号形态可能变化——值不变）。空 map/空列表各发一行
// 保守形态（key: {} / key: []——Parse 端拒绝空的续块，这种写法安全落回
// 标量分支）。
//
// 标量的引号规则：空串、含 YAML 结构字符（: # - [ ] { } , & * 等）或
// 前后空白的值加双引号（内嵌引号转义为 \"）；普通裸词原样发。

import (
	"sort"
	"strconv"
	"strings"
)

// Emit 把树写成 YAML 文本。树的结构契约与 Parse 的产出一致
// （Map 用 Keys/Vals；List 用 Items；Scalar 用 Scalar）。
func Emit(n *Node) string {
	var sb strings.Builder
	emitValue(&sb, n, 0, true)
	return sb.String()
}

// emitMap 发缩进的 map 块（缩进 = indent 个空格）。
func emitMap(sb *strings.Builder, n *Node, indent int) {
	pad := strings.Repeat("  ", indent)
	for i, k := range n.Keys {
		v := n.Vals[i]
		switch v.Kind {
		case KindScalar:
			sb.WriteString(pad + emitScalarKey(k) + ": " + emitScalar(v.Scalar) + "\n")
		case KindMap:
			if len(v.Keys) == 0 {
				sb.WriteString(pad + emitScalarKey(k) + ": {}\n")
				continue
			}
			sb.WriteString(pad + emitScalarKey(k) + ":\n")
			emitMap(sb, v, indent+1)
		case KindList:
			if len(v.Items) == 0 {
				sb.WriteString(pad + emitScalarKey(k) + ": []\n")
				continue
			}
			sb.WriteString(pad + emitScalarKey(k) + ":\n")
			emitList(sb, v, indent+1)
		}
	}
}

// emitList 发 `- ` 列表；元素是 map 时首键同行（`- key: v`），后续键
// 按对齐缩进（Parse 的既有约定）。
func emitList(sb *strings.Builder, n *Node, indent int) {
	pad := strings.Repeat("  ", indent)
	for _, item := range n.Items {
		switch item.Kind {
		case KindScalar:
			sb.WriteString(pad + "- " + emitScalar(item.Scalar) + "\n")
		case KindMap:
			if len(item.Keys) == 0 {
				sb.WriteString(pad + "- {}\n")
				continue
			}
			for j, k := range item.Keys {
				v := item.Vals[j]
				switch v.Kind {
				case KindScalar:
					if j == 0 {
						sb.WriteString(pad + "- " + emitScalarKey(k) + ": " + emitScalar(v.Scalar) + "\n")
					} else {
						sb.WriteString(pad + "  " + emitScalarKey(k) + ": " + emitScalar(v.Scalar) + "\n")
					}
				case KindMap:
					head := pad + "- "
					if j > 0 {
						head = pad + "  "
					}
					sb.WriteString(head + emitScalarKey(k) + ":\n")
					emitMap(sb, v, indent+2)
				case KindList:
					head := pad + "- "
					if j > 0 {
						head = pad + "  "
					}
					sb.WriteString(head + emitScalarKey(k) + ":\n")
					emitList(sb, v, indent+2)
				}
			}
		case KindList:
			sb.WriteString(pad + "-\n")
			emitList(sb, item, indent+1)
		}
	}
}

// emitValue 是顶层入口（Node 根可能是 Map/List）。
func emitValue(sb *strings.Builder, n *Node, indent int, root bool) {
	_ = root
	switch n.Kind {
	case KindMap:
		emitMap(sb, n, indent)
	case KindList:
		emitList(sb, n, indent)
	case KindScalar:
		sb.WriteString(emitScalar(n.Scalar) + "\n")
	}
}

// emitScalar 决定标量的引号形态（空串/结构字符/首尾空白 → 双引号）。
func emitScalar(s string) string {
	if s == "" {
		return `""`
	}
	if needsQuoting(s) {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// emitScalarKey 同 emitScalar（键面：含特殊字符的键加引号）。
func emitScalarKey(k string) string { return emitScalar(k) }

// needsQuoting 报告标量是否需要引号（解析端会把裸词当普通标量，但
// 保守起见：以结构字符开头/含 ": " 或 " #"/首尾空白/仅数字也加引号
// ——数字加引号无害且避免 int/float 歧义）。
func needsQuoting(s string) bool {
	if strings.TrimSpace(s) != s || s == "" {
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true // 数字（含 "1.5"）加引号防类型歧义
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") ||
		strings.ContainsAny(s, `"'[]{}#,&*-`) || strings.HasSuffix(s, ":") {
		return true
	}
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off", "~":
		return true
	}
	return false
}

// SortKeys 是可选的键序规范化辅助（GUI 保存时用——树本身保持文件序；
// 这里只是给需要确定性键序的调用方一个入口）。
func SortKeys(n *Node) {
	if n == nil || n.Kind != KindMap {
		return
	}
	idx := make([]int, len(n.Keys))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return n.Keys[idx[a]] < n.Keys[idx[b]] })
	keys := make([]string, len(idx))
	vals := make([]*Node, len(idx))
	for i, src := range idx {
		keys[i], vals[i] = n.Keys[src], n.Vals[src]
	}
	n.Keys, n.Vals = keys, vals
}
