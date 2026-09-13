package main

// status 渲染的契约测试（事件流 audit → 展示），13.10 的交付判据：
// marl status 能看到 Blocked(Discussing) + verdict 路径。

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

func ev(seq int64, agent, action, target string, payload any) *store.AuditEvent {
	return &store.AuditEvent{Seq: seq, AgentID: types.AgentID(agent), Timestamp: time.Now(), Action: action, Target: target, Payload: payload}
}

// TestStatusBlockedDiscussing：spawn 树 + agent_state + discussion_opened
// 三类事件还原出"子 Agent 在 Blocked(Discussing)，等 verdict"的展示。
func TestStatusBlockedDiscussing(t *testing.T) {
	var w bytes.Buffer
	renderStatus(&w, []*store.AuditEvent{
		ev(1, "root", "agent_state", "running", map[string]any{}),
		ev(2, "root", "spawn", "sub_1", map[string]any{"depth": 1}),
		ev(3, "sub_1", "agent_state", "running", map[string]any{}),
		ev(4, "sub_1", "discussion_opened", "discuss_01", map[string]any{
			"topic": "OAuth 接口", "dir": "/state/marl/x/discussions/discuss_01",
			"target_path": ".marl/knowledge/contracts/oauth.md",
		}),
		ev(5, "nothing-should-map", "commit", "hash", map[string]any{}),
	})
	out := w.String()
	for _, want := range []string{
		"🤖 root (depth=0) running",
		"🔧 sub_1 (depth=1) blocked(discussing)",
		"discuss_01",
		"discussions/discuss_01/verdict.md",
		"OAuth 接口",
		"不烧钱",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// TestStatusConcluded：讨论结束 → Running，不再显示等待。
func TestStatusConcluded(t *testing.T) {
	var w bytes.Buffer
	renderStatus(&w, []*store.AuditEvent{
		ev(1, "root", "agent_state", "running", map[string]any{}),
		ev(2, "root", "discussion_opened", "discuss_01", map[string]any{"topic": "t", "dir": "/d"}),
		ev(3, "root", "agent_state", "blocked", map[string]any{"reason": "discussing"}),
		ev(4, "root", "discussion_concluded", "discuss_01", map[string]any{"commit": "h1"}),
		ev(5, "root", "agent_state", "running", map[string]any{}),
	})
	out := w.String()
	if strings.Contains(out, "讨论进行") || strings.Contains(out, "verdict.md") {
		t.Fatalf("concluded discussion must not be shown as waiting:\n%s", out)
	}
	if !strings.Contains(out, "running") {
		t.Fatalf("state missing:\n%s", out)
	}
}

// TestStatusEmpty：空库不崩。
func TestStatusEmpty(t *testing.T) {
	var w bytes.Buffer
	renderStatus(&w, nil)
	if !strings.Contains(w.String(), "无") {
		t.Fatalf("empty render: %q", w.String())
	}
}
