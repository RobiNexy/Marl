package orchestrate

// target_text / relative 定位（阶段 11 补遗 §2：Agent 给文字，框架匹配）。
//
// 规则（Part 1 §2 的三种命中面的全集）：
//	1. 子串匹配      target_text 是条目 Content 的子串即命中；
//	2. 归一化兜底    统一空白 + \r\n→\n 后重试一次（凭记忆复述的宽容面）；
//	3. 无语义匹配    切错位置的编排比不编排更糟——NOT_FOUND 比模糊命中安全。
//
// 定位范围限定在**当前 View 的可见条目**（Stage 11 补遗 #15；frozen 段
// 不在 View 里（编译层折叠），命中"不可编排范围"的情形按硬拒面处理由
// 调用方在条目_AD 里保证——本匹配器只接收可见条目）。
//
// relative 与 target_text 可组合：relative 先过滤候选集，再在范围内做
// 文字匹配（两个都给出时两者都必须命中）。

import (
	"errors"
	"fmt"
	"strings"

	"github.com/RobiNexy/Marl/internal/types"
)

// ViewEntry 是匹配器的候选形态（Agent 侧从 View+Log 组装；匹配器是纯函数）。
type ViewEntry struct {
	Ref      types.MessageID
	Role     types.InternalRole
	Content  string
	TokenEst int
}

// TargetSelector 是编排技能的定位参数（target_text / relative 的组合面）。
type TargetSelector struct {
	TargetText string
	// Relative 的值集（first/last/nth:N/last_assistant/last_user/
	// second_last_assistant）；空 = 不用 relative（单一 target_text 也可）。
	Relative string
}

// 哨兵错误（Agent 侧折算错误码 AMBIGUOUS_TARGET / TARGET_NOT_FOUND）。
var (
	ErrAmbiguous      = errors.New("orchestrate: ambiguous target")
	ErrTargetNotFound = errors.New("orchestrate: target not found")
)

// AmbiguousMatch 是一次命中的上下文摘录（首行 + 尾行——Agent 靠它分辨
// "哪一处是你要的"）。
type AmbiguousMatch struct {
	FirstLine string
	LastLine  string
}

// Select 定位一次编排操作的目标。
//
// 语义：
//   - relative 过滤（候选集必须命中相对位置）；
//   - target_text 空 → 只用 relative 定位（恰好一条候选才算成功）；
//   - target_text 给了 → 范围内精确子串 → 归一化重试 → AMBIGUOUS/NOT_FOUND。
//
// 返回 (ref, index, nil) / (零值, -1, ErrAmbiguous)（错误带摘录）/
// (零值, -1, ErrTargetNotFound)。
func Select(entries []ViewEntry, sel TargetSelector) (types.MessageID, int, error) {
	// relative 过滤。
	pool := entries
	if idx, ok := relativeIndexOf(entries, sel.Relative); ok {
		if idx < 0 || idx >= len(entries) {
			return "", -1, ErrTargetNotFound
		}
		pool = []ViewEntry{entries[idx]}
	}
	if sel.TargetText == "" {
		if len(pool) != 1 {
			return "", -1, ErrTargetNotFound // 需要 relative 单定位时必须唯一
		}
		return pool[0].Ref, indexOf(entries, pool[0].Ref), nil
	}
	// 精确子串。
	var hits []ViewEntry
	for _, e := range pool {
		if e.Content != "" && strings.Contains(e.Content, sel.TargetText) {
			hits = append(hits, e)
		}
	}
	if len(hits) == 0 {
		// 归一化兜底（一次）：空白 / 换行统一后匹配（凭记忆复述的宽容面）。
		want := normalizeText(sel.TargetText)
		for _, e := range pool {
			if e.Content != "" && strings.Contains(normalizeText(e.Content), want) {
				hits = append(hits, e)
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].Ref, indexOf(entries, hits[0].Ref), nil
	case 0:
		return "", -1, ErrTargetNotFound
	default:
		hs := make([]AmbiguousMatch, 0, len(hits))
		for _, h := range hits {
			hs = append(hs, excerpt(h.Content))
		}
		return "", -1, fmt.Errorf("%w: %d 处命中：%v", ErrAmbiguous, len(hits), hs)
	}
}

// relativeIndex 把 Relative 形态折成 entries 的下标。
//
// 形态集（Part 补遗 §2 的"relative 可组合"，常见相对位）：first / last /
// last_assistant / last_user / second_last_assistant / nth_from_top:N。
// 未知形态 / 越界 → (-1, true)（调用方按 ErrTargetNotFound 处理——msn 未知
// 文本定位不是静默全表扫描）。
func relativeIndexOf(entries []ViewEntry, rel string) (int, bool) {
	if rel == "" {
		return -1, false
	}
	n := len(entries)
	switch rel {
	case "first":
		return 0, true
	case "last":
		return n - 1, true
	case "last_assistant":
		return lastRoleBack(entries, types.RoleAssistantReply, 1)
	case "last_user":
		return lastRoleBack(entries, types.RoleUserInput, 1)
	case "second_last_assistant":
		return lastRoleBack(entries, types.RoleAssistantReply, 2)
	}
	if k, found := strings.CutPrefix(rel, "nth_from_top:"); found {
		var i int
		if _, err := fmt.Sscanf(k, "%d", &i); err == nil {
			return i, true
		}
	}
	return -1, true // 未知 Relative：候选集清空（显式 NOT_FOUND，不 fallback）
}

// lastRoleBack 从尾数第 k 个给出 role 的条目下标（k=1 是"最后一个"，不含）。
func lastRoleBack(entries []ViewEntry, role types.InternalRole, kth int) (int, bool) {
	if kth <= 0 {
		return -1, true
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role == role {
			if kth == 1 {
				return i, true
			}
			kth--
		}
	}
	return -1, true
}

// normalizeText：空白统一（连续空白压一）+ \r\n→\n + 行尾空白去（匹配
// 的宽容面；只影响匹配，不动原文）。
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}), " ")
}

// excerpt 是摘录面的一次命中（首行 + 尾行——Agent 靠它分辨要操作哪处）。
func excerpt(content string) AmbiguousMatch {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	first, last := "", ""
	for _, ln := range lines {
		if t := strings.TrimSpace(ln); t != "" {
			if first == "" {
				first = t
			}
			last = t
		}
	}
	m := AmbiguousMatch{FirstLine: first, LastLine: last}
	const capLen = 80
	if len(m.FirstLine) > capLen {
		m.FirstLine = m.FirstLine[:capLen] + "…"
	}
	if len(m.LastLine) > capLen {
		m.LastLine = m.LastLine[:capLen] + "…"
	}
	return m
}

func indexOf(entries []ViewEntry, ref types.MessageID) int {
	for i := range entries {
		if entries[i].Ref == ref {
			return i
		}
	}
	return -1
}
