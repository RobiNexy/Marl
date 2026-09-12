// Package config 提供框架配置文件的解析（阶段 4 起）。
//
// 设计文档约定的配置形态是 YAML（ladder.yaml / profiles/*.yaml），而本项目
// 刻意不引入 gopkg.in/yaml.v3（见 file_read summary 的既有取舍：依赖只为
// 单一功能付费不划算）。本包实现一个**受限 YAML 子集**的解析器，覆盖框架
// 配置实际使用的形态，并显式拒绝超出子集的输入——静默容错会让配置文件里
// 的笔误变成"看起来能跑但语义不同"的运行时行为，比报错危险得多。
//
// # 支持的子集
//
//   - 缩进嵌套（空格，禁 Tab）；同一块的缩进必须一致；
//   - `key: value` 映射；`key:`（值为空）后跟更深层级的块；
//   - `- ` 列表项；`- key: value` 起始的映射项（后续更深层级行归属该项）；
//   - 行内流式映射 `{k: v, k2: "v2"}`（仅标量值）；
//   - 行内流式列表 `[a, b, "c"]`（仅标量项）；
//   - 标量：双引号/单引号字符串、裸词、整数、浮点；
//   - 注释：整行 `#` 与行尾 ` #`（引号内的 # 不算注释）。
//
// # 显式拒绝（报错，不做容错推断）
//
//   - Tab 缩进、多文档（---）、锚点/引用（&/*）、块标量（|/>）、
//     嵌套花括号。
//
// 解析产出是 Node 树（Map/List/Scalar），消费方用 Get/Str/Int/Float 取值——
// 类型转换的错误在消费方报出（带字段路径），解析器只管结构。
package config

import (
	"fmt"
	"strings"
)

// Kind 是节点的种类。
type Kind int

const (
	KindScalar Kind = iota
	KindMap
	KindList
)

// Node 是解析树的一个节点。
//
// 零值契约：零值 Node 是 KindScalar 且 Scalar 为空——消费方用 Get 找不到
// 键时返回 nil，必须判 nil 而不是解引用零值。
type Node struct {
	Kind   Kind
	Scalar string
	Keys   []string // KindMap：键序保持文件顺序（报表与 golden 测试依赖）
	Vals   []*Node
	Items  []*Node // KindList
	Line   int     // 1-based，错误定位用
}

// Get 按键取子节点（仅 KindMap）。不存在返回 nil。
// 并发：纯函数（树在解析后视为只读）。
func (n *Node) Get(key string) *Node {
	if n == nil || n.Kind != KindMap {
		return nil
	}
	for i, k := range n.Keys {
		if k == key {
			return n.Vals[i]
		}
	}
	return nil
}

// Str 取标量字符串。缺失/非标量返回 ("", false)。
func (n *Node) Str() (string, bool) {
	if n == nil || n.Kind != KindScalar {
		return "", false
	}
	return n.Scalar, true
}

// StrOr 取标量，缺失时返回 def。
func (n *Node) StrOr(def string) string {
	if s, ok := n.Str(); ok {
		return s
	}
	return def
}

// Int 取标量并转 int。失败返回 ("缺省 0", false)——调用方必须处理 ok=false。
func (n *Node) Int() (int, bool) {
	s, ok := n.Str()
	if !ok {
		return 0, false
	}
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, false
	}
	return v, true
}

// Float 取标量并转 float64。失败返回 (0, false)。
func (n *Node) Float() (float64, bool) {
	s, ok := n.Str()
	if !ok {
		return 0, false
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
		return 0, false
	}
	return v, true
}

// List 返回列表项（仅 KindList；Map/Scalar 返回 nil）。
func (n *Node) List() []*Node {
	if n == nil || n.Kind != KindList {
		return nil
	}
	return n.Items
}

// MapLen 返回映射条目数（仅 KindMap）。
func (n *Node) MapLen() int {
	if n == nil || n.Kind != KindMap {
		return 0
	}
	return len(n.Keys)
}

// line 是预处理后的一行：缩进（空格数）与去注释后的内容。
type line struct {
	indent int
	text   string // 去除缩进与注释、右侧空白后的内容；非空
	no     int    // 1-based 原始行号
}

// Parse 解析 YAML 子集。空输入返回 nil 树 + nil（空配置是合法的）。
//
// 失败：Tab 缩进、缩进不一致、无法识别的行形态、流式标量非法。
func Parse(src []byte) (*Node, error) {
	lines, err := preprocess(string(src))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	p := &parser{lines: lines}
	node, err := p.block(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, fmt.Errorf("config: line %d: unexpected content at indentation %d (block closed early)",
			p.lines[p.pos].no, p.lines[p.pos].indent)
	}
	return node, nil
}

// preprocess 切行、去注释、算缩进、拒绝 Tab。
func preprocess(src string) ([]line, error) {
	var out []line
	for i, raw := range strings.Split(src, "\n") {
		no := i + 1
		// Tab 缩进直接拒绝（YAML 禁 Tab；静默把 Tab 当空格会让两层配置
		// 意外合并，错误现场离原因极远）。
		trimmed := strings.TrimLeft(raw, " ")
		if strings.HasPrefix(raw, "\t") || (len(raw)-len(trimmed) > 0 && strings.ContainsRune(raw[:len(raw)-len(trimmed)], '\t')) {
			return nil, fmt.Errorf("config: line %d: tab indentation is not allowed (use spaces)", no)
		}
		content := stripComment(trimmed)
		content = strings.TrimRight(content, " \r")
		if content == "" {
			continue
		}
		if content == "---" || content == "..." {
			return nil, fmt.Errorf("config: line %d: multi-document streams are not supported", no)
		}
		out = append(out, line{indent: len(raw) - len(trimmed), text: content, no: no})
	}
	return out, nil
}

// stripComment 去掉行尾注释（# 前有空白或行首即注释；引号内的 # 保留）。
//
// 并发：纯函数。
func stripComment(s string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble && (i == 0 || s[i-1] == ' ') {
				return s[:i]
			}
		}
	}
	return s
}

type parser struct {
	lines []line
	pos   int
}

// block 解析一个缩进块（map 或 list），调用方保证 p.lines[p.pos].indent == indent。
func (p *parser) block(indent int) (*Node, error) {
	if p.pos >= len(p.lines) {
		return nil, fmt.Errorf("config: unexpected end of input")
	}
	if strings.HasPrefix(p.lines[p.pos].text, "- ") || p.lines[p.pos].text == "-" {
		return p.list(indent)
	}
	return p.mapping(indent)
}

// mapping 解析 `key: value` 块。
func (p *parser) mapping(indent int) (*Node, error) {
	node := &Node{Kind: KindMap, Line: p.lines[p.pos].no}
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		if ln.indent < indent {
			break // 块结束（由调用方消费剩余行）
		}
		if ln.indent > indent {
			return nil, fmt.Errorf("config: line %d: unexpected deeper indentation", ln.no)
		}
		if strings.HasPrefix(ln.text, "- ") {
			break // 同缩进的列表：交给调用方（不该出现在 map 块中间）
		}
		key, rest, ok := splitKey(ln.text)
		if !ok {
			return nil, fmt.Errorf("config: line %d: expected 'key: value', got %q", ln.no, ln.text)
		}
		p.pos++
		if rest != "" {
			scalar, err := parseScalar(rest, ln.no)
			if err != nil {
				return nil, err
			}
			node.Keys = append(node.Keys, key)
			node.Vals = append(node.Vals, scalar)
			continue
		}
		// 值为空：下一行更深的缩进是子块；否则是 null（本子集不支持 null，
		// 视为空标量——消费方按缺失处理）。
		if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
			child, err := p.block(p.lines[p.pos].indent)
			if err != nil {
				return nil, err
			}
			node.Keys = append(node.Keys, key)
			node.Vals = append(node.Vals, child)
			continue
		}
		node.Keys = append(node.Keys, key)
		node.Vals = append(node.Vals, &Node{Kind: KindScalar, Line: ln.no})
	}
	return node, nil
}

// list 解析 `- ` 列表块。
func (p *parser) list(indent int) (*Node, error) {
	node := &Node{Kind: KindList, Line: p.lines[p.pos].no}
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		if ln.indent != indent || !(strings.HasPrefix(ln.text, "- ") || ln.text == "-") {
			break
		}
		rest := strings.TrimSpace(strings.TrimPrefix(ln.text, "-"))
		itemIndent := ln.indent + 2 // "- " 之后的内容视为缩进 +2 的虚拟行
		p.pos++
		if rest == "" {
			// 纯 "-" 项：内容在后续更深行。
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				child, err := p.block(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				node.Items = append(node.Items, child)
				continue
			}
			return nil, fmt.Errorf("config: line %d: empty list item", ln.no)
		}
		if _, _, ok := splitKey(rest); ok {
			// `- key: value`：把 rest 当作该项 map 的首行，后续同项行
			// 缩进 > indent。
			virtual := line{indent: itemIndent, text: rest, no: ln.no}
			p.lines = append(p.lines[:p.pos], append([]line{virtual}, p.lines[p.pos:]...)...)
			child, err := p.mapping(itemIndent)
			if err != nil {
				return nil, err
			}
			// 撤销虚拟行（已消费）。
			node.Items = append(node.Items, child)
			continue
		}
		scalar, err := parseScalar(rest, ln.no)
		if err != nil {
			return nil, err
		}
		node.Items = append(node.Items, scalar)
	}
	return node, nil
}

// splitKey 把 "key: value" 拆成 (key, rest)。key 允许引号包裹。
// 返回 ok=false 表示该行不是 key: value 形态。
//
// 并发：纯函数。
func splitKey(text string) (key, rest string, ok bool) {
	// 找到引号外的第一个 ": "（或行尾冒号）。
	inSingle, inDouble := false, false
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ':':
			if inSingle || inDouble {
				continue
			}
			if i+1 == len(text) {
				return unquote(strings.TrimSpace(text[:i])), "", true
			}
			if text[i+1] == ' ' {
				return unquote(strings.TrimSpace(text[:i])), strings.TrimSpace(text[i+1:]), true
			}
		}
	}
	return "", "", false
}

// parseScalar 解析标量、行内流式映射或行内流式列表（仅标量项）。
//
// 失败：未闭合引号、嵌套花括号。
// 并发：纯函数。
func parseScalar(text string, lineNo int) (*Node, error) {
	if strings.HasPrefix(text, "{") {
		if !strings.HasSuffix(text, "}") {
			return nil, fmt.Errorf("config: line %d: unclosed flow mapping", lineNo)
		}
		inner := strings.TrimSpace(text[1 : len(text)-1])
		node := &Node{Kind: KindMap, Line: lineNo}
		if inner == "" {
			return node, nil
		}
		for _, part := range splitFlow(inner) {
			k, v, ok := splitKey(part)
			if !ok {
				return nil, fmt.Errorf("config: line %d: flow mapping entry %q is not 'key: value'", lineNo, part)
			}
			sv, err := parseScalar(v, lineNo)
			if err != nil {
				return nil, err
			}
			node.Keys = append(node.Keys, k)
			node.Vals = append(node.Vals, sv)
		}
		return node, nil
	}
	if strings.HasPrefix(text, "[") {
		if !strings.HasSuffix(text, "]") {
			return nil, fmt.Errorf("config: line %d: unclosed flow sequence", lineNo)
		}
		inner := strings.TrimSpace(text[1 : len(text)-1])
		node := &Node{Kind: KindList, Line: lineNo}
		if inner == "" {
			return node, nil
		}
		for _, part := range splitFlow(inner) {
			sv, err := parseScalar(part, lineNo)
			if err != nil {
				return nil, err
			}
			if sv.Kind != KindScalar {
				return nil, fmt.Errorf("config: line %d: flow sequence entry %q is not a scalar", lineNo, part)
			}
			node.Items = append(node.Items, sv)
		}
		return node, nil
	}
	if strings.HasPrefix(text, "|") || strings.HasPrefix(text, ">") {
		return nil, fmt.Errorf("config: line %d: block scalars (| >) are not supported", lineNo)
	}
	if strings.HasPrefix(text, "&") || strings.HasPrefix(text, "*") {
		return nil, fmt.Errorf("config: line %d: anchors/aliases are not supported", lineNo)
	}
	s, err := unquoteChecked(text, lineNo)
	if err != nil {
		return nil, err
	}
	return &Node{Kind: KindScalar, Scalar: s, Line: lineNo}, nil
}

// splitFlow 按引号外的逗号切分流式映射内容。
func splitFlow(s string) []string {
	var parts []string
	inSingle, inDouble := false, false
	depth := 0
	last := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '{':
			if !inSingle && !inDouble {
				depth++
			}
		case '}':
			if !inSingle && !inDouble {
				depth--
			}
		case ',':
			if !inSingle && !inDouble && depth == 0 {
				parts = append(parts, strings.TrimSpace(s[last:i]))
				last = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(s[last:]))
	return parts
}

// unquote 去除包裹引号（不校验——splitKey 内部对键使用）。
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// unquoteChecked 去除包裹引号并校验配对；裸词原样返回。
func unquoteChecked(s string, lineNo int) (string, error) {
	if len(s) >= 2 && s[0] == '"' {
		if s[len(s)-1] != '"' {
			return "", fmt.Errorf("config: line %d: unclosed double quote", lineNo)
		}
		return s[1 : len(s)-1], nil
	}
	if len(s) >= 2 && s[0] == '\'' {
		if s[len(s)-1] != '\'' {
			return "", fmt.Errorf("config: line %d: unclosed single quote", lineNo)
		}
		return s[1 : len(s)-1], nil
	}
	if strings.ContainsAny(s, "\"'") {
		return "", fmt.Errorf("config: line %d: stray quote inside scalar %q", lineNo, s)
	}
	return s, nil
}
