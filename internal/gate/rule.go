package gate

// GateRequest / GateDecision / 规则匹配（Part 11.3 的核心数据结构）。

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/RobiNexy/Marl/internal/types"
)

// Kind 是 Gate 的决策面分类（Part 11.3 §3.2 / §3.4）。
type Kind string

const (
	KindLLMCall       Kind = "llm_call"      // Agent 逃生舱的超限审批
	KindOrchestration Kind = "orchestration" // View 编排的缓存破坏分级
	KindDiscussion    Kind = "discussion"    // 人机讨论的裁决
	KindEscalation    Kind = "escalation"    // 到顶的向上求助
	KindShell         Kind = "shell"         // 高危命令
)

// Valid 报告 k 是否已定义。未识别 Kind 必须显式报错（默认全集放行 =
// "未分类即免审"，是最坏的静默方向）。
func (k Kind) Valid() bool {
	switch k {
	case KindLLMCall, KindOrchestration, KindDiscussion, KindEscalation, KindShell:
		return true
	}
	return false
}

// Action 是三值决策。
type Action string

const (
	ActionAllow     Action = "allow"
	ActionDeny      Action = "deny"
	ActionNeedHuman Action = "need_human"
)

// Request 是 PEP → PDP 的一次问询。
//
// Attributes 是**机械可算**的属性（cache_destroyed_pct / task_call_count /
// task_tokens / wire / purpose / command / risk…）。规则匹配只看属性键——
// Agent 自述（purpose 等）只作账本标签，不作任何授权依据（原则 4）。
type Request struct {
	Kind       Kind
	AgentID    types.AgentID
	Attributes map[string]any
}

// Decision 是 PDP 的产出。
//
// 不变量：Action 为 deny / need_human ⟹ Reason 非空（拒绝没有理由 =
// LLM 只能瞎猜浪费轮次；带数字的约定见 Part 11.4 §4.2）。
type Decision struct {
	Action Action
	RuleID string
	Reason string
}

// Rule 是规则表的一行（Part 11.3 §3.6 的形态；顺序即优先级，首中生效）。
//
// Match: 属性键 → 期望值。数值属性支持比较前缀（"<10" / ">=20"）；
// 字符串/布尔属性精确匹配。缺 key 的属性按"该规则不适用"处理（不猜默认值
// ——属性缺失的语义不能被默认化：缺 kind 的属性可能意味着 PEP 没配好，
// 猜一个方向的默认值会把装配错误静默化）。
type Rule struct {
	ID     string            `yaml:"id"`
	Match  map[string]string `yaml:"match"`
	Action Action            `yaml:"action"`
	Reason string            `yaml:"-"` // deny / need_human 的默认理由
}

// Matches 报告规则是否覆盖 request（全部属性 AND；kind 必须匹配）。
func (r Rule) Matches(req *Request) bool {
	if req == nil {
		return false
	}
	if kindWant, ok := r.Match["kind"]; ok && !strings.EqualFold(kindWant, string(req.Kind)) {
		return false
	}
	for key, want := range r.Match {
		if key == "kind" {
			continue
		}
		got, ok := req.Attributes[key]
		if !ok {
			return false
		}
		if !attributeMatches(got, want) {
			return false
		}
	}
	return true
}

// attributeMatches 对比一个属性；数值支持比较前缀（Part 11.5 的字面形态）。
//
// 路径判定：want 带比较前缀（"<=" / ">=" / "<" / ">" / "==" / "="）或
// 是裸数字 → 数值路径；want 是字符/布尔（"true"/"false"）→ 字符串路径。
// 字符串比较不区分大小写（kind / risk 这类枚举键的人类手写宽容度）。
func attributeMatches(got any, want string) bool {
	if _, cn := toFloat(got); cn && numExprLen(want) > 0 {
		if n, ok := toFloat(got); ok {
			return cmpNumeric(n, want)
		}
	}
	if gotText, ok := toAttrText(got); ok {
		return strings.EqualFold(gotText, strings.TrimSpace(want))
	}
	return false
}

// numExprLen 报告 want 是否是数值表达式（带比较前缀 or 裸数字）；0 = 否。
func numExprLen(want string) int {
	for _, p := range []string{"<=", ">=", "==", "=", "<", ">"} {
		if strings.HasPrefix(want, p) {
			if _, _, ok := parseNumExpr(want); ok {
				return len(p)
			}
			return 0
		}
	}
	if _, err := strconv.ParseFloat(strings.TrimSpace(want), 64); err == nil {
		return len(want)
	}
	return 0
}

// cmpNumeric 数值比较（front 表的解析在 parseNumExpr 一处收口）。
func cmpNumeric(got float64, want string) bool {
	b, op, ok := parseNumExpr(want)
	if !ok {
		return false
	}
	switch op {
	case numLTE:
		return got <= b
	case numLT:
		return got < b
	case numGTE:
		return got >= b
	case numGT:
		return got > b
	default:
		return got == b
	}
}

// numOp 是比较运算的枚举（顺序 = 前缀解析的优先表）。
type numOp int

const (
	numEq numOp = iota
	numLT
	numLTE
	numGT
	numGTE
)

// parseNumExpr 从 want 里解析数字 + 运算（"<10" → 10, numLT）。
func parseNumExpr(want string) (float64, numOp, bool) {
	table := []struct {
		prefix string
		op     numOp
	}{
		{"<=", numLTE},
		{">=", numGTE},
		{"<", numLT},
		{">", numGT},
		{"==", numEq},
		{"=", numEq},
	}
	for _, c := range table {
		if rest, found := strings.CutPrefix(want, c.prefix); found {
			f, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err != nil {
				return 0, numEq, false
			}
			return f, c.op, true
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(want), 64)
	if err != nil {
		return 0, numEq, false
	}
	return f, numEq, true
}

// toAttrText 是字符串面的属性提取（bool 的 "true"/"false" 显隐；数字
// 不走这里——数字必须走比较路径，"1" 写成 "1.0" 的宽容来自数值路径）。
func toAttrText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return fmt.Sprintf("%v", t), true
	}
	return "", false
}

// toFloat 数字属性提取（JSON 反序列化的 float64 与 Go 常见的 int 族）。
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}
