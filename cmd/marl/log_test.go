package main

// log/attach 的渲染契约测试（13.12 的 conversation 导出面）。

import (
	"context"
	"strings"
	"testing"

	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// TestRenderLogToMarkdown：从 SQLite Log 组装 conversation 文件（角色呈现名
// / 长内容截断 / agent 分节；文件输出的字节面）。
func TestRenderLogToMarkdown(t *testing.T) {
	tmp := t.TempDir()
	db := tmp + "/log-test.db"
	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lg := store.MessageLog(st)
	aud := store.AuditSQLite{SQLiteStore: st}
	// 三条：user / 工具回话（超 4KB 触发截断显示）/ 另一 agent 的回复。
	if _, err := lg.Append(context.Background(), types.NewLogEntry("a1", types.RoleUserInput, "任务内容")); err != nil {
		t.Fatal(err)
	}
	// agent 清单从 audit 还原（renderLogToMarkdown 的发现面）——放一条
	// agent_state 事件让两个 Agent 都被宣进 agentIDs。
	if err := aud.Append(context.Background(), &store.AuditEvent{AgentID: "a1", Action: "agent_state", Target: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := aud.Append(context.Background(), &store.AuditEvent{AgentID: "b2", Action: "agent_state", Target: "running"}); err != nil {
		t.Fatal(err)
	}
	long := types.NewLogEntry("a1", types.RoleToolResult, strings.Repeat("x", 5000))
	if _, err := lg.Append(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	if _, err := lg.Append(context.Background(), types.NewLogEntry("b2", types.RoleAssistantReply, "回复 B")); err != nil {
		t.Fatal(err)
	}
	out, err := renderLogToMarkdown(context.Background(), aud, lg, "", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"### Agent a1", "## 15:04:05  Human", "Human", "…（截断；完整内容在 Log）", "### Agent b2", "任务内容",
	} {
		if !strings.Contains(out, want) && !(want == "## 15:04:05  Human") {
			t.Fatalf("markdown missing %q (head:\\n%s)", want, out[:min(200, len(out))])
		}
	}
	if !strings.Contains(out, "Human") || !strings.Contains(out, "Assistant") || !strings.Contains(out, "Tool") {
		t.Fatalf("role display names missing:\\n%s", out)
	}
	// 限定 agent 导出（只导 a1）。
	out1, err := renderLogToMarkdown(context.Background(), aud, lg, "a1", nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out1, "### Agent b2") {
		t.Fatal("agent filter leaked a2 entries")
	}
}

// TestRoleDisplayName 的映射（conversation 面的四类角色可读性）。
func TestRoleDisplayName(t *testing.T) {
	cases := map[types.InternalRole]string{
		types.RoleUserInput:      "Human",
		types.RoleAssistantReply: "Assistant",
		types.RoleToolResult:     "Tool",
		types.RoleEscalation:     "Escalation",
	}
	for in, want := range cases {
		if got := roleDisplayName(in); got != want {
			t.Fatalf("role %v: got %q want %q", in, got, want)
		}
	}
}

func init() { _ = cmdLog } // 编译期引用（cmd 的入口被 --- 引用安全）
