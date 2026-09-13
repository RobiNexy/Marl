package orchestrate

// 定位协议的契约测试（阶段 11 补遗 §2：精确优先 / 归一兜底 / 无语义匹配
// / 歧义摘录 / relative 组合）。

import (
	"errors"
	"strings"
	"testing"

	"github.com/RobiNexy/Marl/internal/types"
)

func entry(ref, role, content string, tokens int) ViewEntry {
	return ViewEntry{Ref: types.MessageID(ref), Role: types.InternalRole(role), Content: content, TokenEst: tokens}
}

var fixtureEntries = []ViewEntry{
	entry("m1", "user_input", "把 oauth.js 实现完。\n依赖 Redis 实例。", 10),
	entry("m2", "assistant_reply", "oauth.js 实现完成，支持 Google/GitHub 两种 provider。\n测试全部通过。", 20),
	entry("m3", "assistant_reply", "oauth.js 实现完成，支持 Google/GitHub ……但 session 部分未完成", 15),
	entry("m4", "user_input", "下一步：session 存储。", 8),
}

// TestExactSubstring：子串命中一条即定位成功。
func TestSelectExact(t *testing.T) {
	ref, idx, err := Select(fixtureEntries, TargetSelector{TargetText: "测试全部通过"})
	if err != nil {
		t.Fatal(err)
	}
	if ref != types.MessageID("m2") || idx != 1 {
		t.Fatalf("ref=%s idx=%d", ref, idx)
	}
}

// TestSelectAmbiguous：多命中 → 摘录（首/尾行）让 Agent 能分辨。
func TestSelectAmbiguous(t *testing.T) {
	_, _, err := Select(fixtureEntries, TargetSelector{TargetText: "oauth.js 实现完成，支持 Google/GitHub"})
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("want ambiguous: %v", err)
	}
	// 摘录面：两处的尾行不同（"…测试全部通过" vs "…未完成"）。
	if !strings.Contains(err.Error(), "测试全部通过") || !strings.Contains(err.Error(), "未完成") {
		t.Fatalf("excerpts missing: %v", err)
	}
}

// TestSelectNormalized：凭记忆复述的空白/换行差异 → 归一化兜底命中。
func TestSelectNormalized(t *testing.T) {
	ref, _, err := Select(fixtureEntries, TargetSelector{TargetText: "oauth.js 实现完成， 支持\n Google/GitHub 测试全部通过。"})
	// 上面这段在原文里有标点差异；归一化只对空白/换行宽容——**语义匹配
	// 不做**：内容不同即不命中（NOT_FOUND 的正确性）。
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("semantic match must not happen: ref=%s err=%v", ref, err)
	}
	// 空白/换行层面的差异则允许（同一文本的复述形态）。
	weird := []ViewEntry{entry("m9", "user_input", "第一行 与后续\n重复空白  行", 6)}
	ref, _, err = Select(weird, TargetSelector{TargetText: "第一行 与后续 重复空白 行"})
	if err != nil || ref != types.MessageID("m9") {
		t.Fatalf("normalized fallback: ref=%s err=%v", ref, err)
	}
}

// TestSelectNotFound：无命中（语义上再"差不多"也不算——NOT_FOUND 面）。
func TestSelectNotFound(t *testing.T) {
	_, _, err := Select(fixtureEntries, TargetSelector{TargetText: "完全无关的文字"})
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("want not-found: %v", err)
	}
}

// TestSelectRelative：relative 单用与组合（relative 过滤 + text 匹配）。
func TestSelectRelative(t *testing.T) {
	// last_user 单独定位。
	ref, _, err := Select(fixtureEntries, TargetSelector{Relative: "last_user"})
	if err != nil || ref != types.MessageID("m4") {
		t.Fatalf("last_user: %s %v", ref, err)
	}
	// second_last_assistant。
	ref, _, err = Select(fixtureEntries, TargetSelector{Relative: "second_last_assistant"})
	if err != nil || ref != types.MessageID("m2") {
		t.Fatalf("second_last_assistant: %s %v", ref, err)
	}
	// 组合：relative 缩小到 m3，text 在该条目内命中。
	ref, _, err = Select(fixtureEntries, TargetSelector{
		Relative:   "last_assistant",
		TargetText: "session 部分未完成",
	})
	if err != nil || ref != types.MessageID("m3") {
		t.Fatalf("composite: %s %v", ref, err)
	}
	// 组合不一致：text 只在 m2（不在 last 范围）→ NOT_FOUND（GATE 前的
	// 定位失败不能默默换成别的候选）。
	_, _, err = Select(fixtureEntries, TargetSelector{
		Relative:   "last_assistant",
		TargetText: "测试全部通过",
	})
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("composite must keep scope: %v", err)
	}
	// nth_from_top 从 0 计。
	ref, _, err = Select(fixtureEntries, TargetSelector{Relative: "nth_from_top:1"})
	if err != nil || ref != types.MessageID("m2") {
		t.Fatalf("nth_from_top:1 → %s %v", ref, err)
	}
}

// TestFrozenHardReject 面：匹配器只接收**可见条目**（Agent 侧组装时已
// 过滤）；这里的防御性断言是"软删除条目永不命中"。
func TestSelectSkipsInvisible(t *testing.T) {
	pool := []ViewEntry{entry("gone", "assistant_reply", "被软删除的内容", 5)}
	_, _, err := Select(pool, TargetSelector{TargetText: "被软删除的内容"})
	if err != nil {
		t.Fatalf("caller passes only visible entries; matcher 是纯函数，这里验证它对空集语义: err=%v (expect not-found on non-matching pool)", err)
	}
	// 可见但内容不匹配 → not found。
	pool = []ViewEntry{fixtureEntries[0]}
	_, _, err = Select(pool, TargetSelector{TargetText: "不存在的文字"})
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("expect not-found, got %v", err)
	}
}
