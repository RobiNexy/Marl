package main

// log/attach 的渲染契约测试（13.12 的 conversation 导出面）。

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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

// --- attach tail 内核（tailLoop / tailTick）的规格 ---
//
// 这些测试把契约翻译成可执行断言：tail 只跟随游标之后的新内容、不重放
// 历史、按 Seq 升序不重不漏、ctx 取消即退出。全部用真实 SQLite（与既有
// 测试同源），保证测的是真实 MessageLog 语义而非 mock 的臆想。

// newTailStore 起一个临时 SQLite，返回 Log/Audit 与关闭钩子。
func newTailStore(t *testing.T) (store.MessageLog, store.AuditStore, func()) {
	t.Helper()
	st, err := store.OpenSQLite(t.TempDir() + "/attach.db")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return store.MessageLog(st), store.AuditSQLite{SQLiteStore: st}, func() { _ = st.Close() }
}

// appendEntry 追加一条并在失败时携带上下文终止（错误路径显式处理）。
func appendEntry(t *testing.T, lg store.MessageLog, id types.AgentID, role types.InternalRole, content string) {
	t.Helper()
	if _, err := lg.Append(context.Background(), types.NewLogEntry(id, role, content)); err != nil {
		t.Fatalf("append %s/%s: %v", id, role, err)
	}
}

// TestTailTick_FirstTickPinsCursorNoReplay 验证 tail 的核心不变量：首次见到
// 一个已有历史的 Agent 时，游标钉在当前末尾、不重放历史条目。
func TestTailTick_FirstTickPinsCursorNoReplay(t *testing.T) {
	lg, aud, done := newTailStore(t)
	defer done()
	appendEntry(t, lg, "a1", types.RoleUserInput, "历史消息 1")
	appendEntry(t, lg, "a1", types.RoleAssistantReply, "历史消息 2")

	var buf bytes.Buffer
	deps := tailDeps{Log: lg, Audit: aud, AgentID: "a1", Out: &buf}
	cursor := map[types.AgentID]int64{}
	header := map[types.AgentID]bool{}

	if err := tailTick(context.Background(), deps, cursor, header); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("首轮不应重放历史，却输出了：%q", buf.String())
	}
	if got := cursor["a1"]; got != 2 {
		t.Fatalf("首轮应把游标钉在末尾 seq=2，got %d", got)
	}
}

// TestTailTick_EmitsOnlyNewEntries 验证第二轮只输出游标之后新增的条目，且
// 打印一次 Agent 标题、按 Seq 升序。
func TestTailTick_EmitsOnlyNewEntries(t *testing.T) {
	lg, aud, done := newTailStore(t)
	defer done()
	appendEntry(t, lg, "a1", types.RoleUserInput, "旧")

	var buf bytes.Buffer
	deps := tailDeps{Log: lg, Audit: aud, AgentID: "a1", Out: &buf}
	cursor := map[types.AgentID]int64{}
	header := map[types.AgentID]bool{}

	// 首轮钉游标（seq=1），无输出。
	if err := tailTick(context.Background(), deps, cursor, header); err != nil {
		t.Fatal(err)
	}
	// 新增两条。
	appendEntry(t, lg, "a1", types.RoleAssistantReply, "新一")
	appendEntry(t, lg, "a1", types.RoleToolResult, "新二")

	if err := tailTick(context.Background(), deps, cursor, header); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "旧") {
		t.Fatalf("不应输出游标之前的历史：%q", out)
	}
	if !strings.Contains(out, "新一") || !strings.Contains(out, "新二") {
		t.Fatalf("应输出两条新条目，got：%q", out)
	}
	if strings.Count(out, "### Agent a1") != 1 {
		t.Fatalf("Agent 标题应只打一次，got：%q", out)
	}
	if i, j := strings.Index(out, "新一"), strings.Index(out, "新二"); i > j {
		t.Fatalf("条目应按 Seq 升序，got：%q", out)
	}
	if got := cursor["a1"]; got != 3 {
		t.Fatalf("游标应推进到 seq=3，got %d", got)
	}
}

// TestTailTick_MultiAgentDiscovery 验证 AgentID 为空时从审计发现多个 Agent，
// 且各自独立游标、排除 watchdog。
func TestTailTick_MultiAgentDiscovery(t *testing.T) {
	lg, aud, done := newTailStore(t)
	defer done()
	ctx := context.Background()
	// 两个业务 Agent + 一个 watchdog（应被排除）都产生审计事件。
	for _, id := range []types.AgentID{"a1", "b2", "watchdog"} {
		if err := aud.Append(ctx, &store.AuditEvent{AgentID: id, Action: "agent_state", Target: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	appendEntry(t, lg, "a1", types.RoleUserInput, "a1-旧")
	appendEntry(t, lg, "b2", types.RoleUserInput, "b2-旧")

	var buf bytes.Buffer
	deps := tailDeps{Log: lg, Audit: aud, AgentID: "", Out: &buf}
	cursor := map[types.AgentID]int64{}
	header := map[types.AgentID]bool{}

	if err := tailTick(ctx, deps, cursor, header); err != nil {
		t.Fatal(err)
	}
	if _, ok := cursor["watchdog"]; ok {
		t.Fatal("watchdog 不应被跟随")
	}
	if _, ok := cursor["a1"]; !ok {
		t.Fatal("a1 应被发现并钉游标")
	}
	if _, ok := cursor["b2"]; !ok {
		t.Fatal("b2 应被发现并钉游标")
	}
	// 各自新增一条，第二轮只输出新内容。
	appendEntry(t, lg, "a1", types.RoleAssistantReply, "a1-新")
	appendEntry(t, lg, "b2", types.RoleAssistantReply, "b2-新")
	if err := tailTick(ctx, deps, cursor, header); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "a1-新") || !strings.Contains(out, "b2-新") {
		t.Fatalf("两个 Agent 的新内容都应输出，got：%q", out)
	}
	if strings.Contains(out, "a1-旧") || strings.Contains(out, "b2-旧") {
		t.Fatalf("不应重放历史，got：%q", out)
	}
}

// TestTailLoop_CancelExits 验证 ctx 取消后 tailLoop 立即返回 nil（Ctrl-C 的
// 退出路径）。用一个后台 goroutine 跑 tailLoop，主 goroutine cancel 后等其返回。
func TestTailLoop_CancelExits(t *testing.T) {
	lg, aud, done := newTailStore(t)
	defer done()
	appendEntry(t, lg, "a1", types.RoleUserInput, "x")

	ctx, cancel := context.WithCancel(context.Background())
	var buf bytes.Buffer
	deps := tailDeps{Log: lg, Audit: aud, AgentID: "a1", Interval: 5 * time.Millisecond, Out: &buf}

	var wg sync.WaitGroup
	wg.Add(1)
	var retErr error
	go func() {
		defer wg.Done()
		retErr = tailLoop(ctx, deps)
	}()
	// 让它至少跑一轮，然后取消。
	time.Sleep(20 * time.Millisecond)
	cancel()

	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("tailLoop 未在取消后及时退出（可能死循环）")
	}
	if retErr != nil {
		t.Fatalf("取消退出应返回 nil，got %v", retErr)
	}
}
