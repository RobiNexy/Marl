package agent

// 阶段 5 的 fork 集成测试（13.7 测试规格）：父 fork 子；子写文件（在
// writable_paths 内）；子 report success；report 进父 Log；父 View 有
// sub_task_result。外加两类拒绝路径（越权命名空间 / 子再 fork 到深度顶）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"marl/internal/proto"
	"marl/internal/spawner"
	"marl/internal/store"
	"marl/internal/skill"
	"marl/internal/types"
	"marl/internal/wire"
)

// forkFactory 用真实 agent 构建子（Spawner.ChildFactory 的测试实现）。
//
// 子脚本用 scriptFn（按 plan 取）而不是按 id 预注册：子 id 由 Spawner 在
// 裁决期分配，而脚本在 BuildChild 时才需要——函数形态消除了"先知道 id
// 才能写脚本"的顺序依赖。
type forkFactory struct {
	t        *testing.T
	st       *store.SQLiteStore
	reg      skill.Registry
	root     string
	spw      *spawner.Spawner
	chk      proto.ReportChecker
	scriptFn func(plan *spawner.ChildPlan) []*wire.WireTurn
}

func (f *forkFactory) BuildChild(ctx context.Context, plan *spawner.ChildPlan, req *proto.SpawnRequest) (spawner.ChildRunner, error) {
	child, err := New(Config{
		ID:           plan.ID,
		ParentID:     plan.ParentID,
		Depth:        plan.Depth,
		MaxDepth:     1,
		SystemPrompt: "You are a child agent; finish the task and report.",
		MaxRounds:    6,
		Log:          f.st,
		Views:        f.st,
		LLM:          &fakeLLM{turns: f.scriptFn(plan)},
		Skills:       f.reg,
		Namespace:    plan.Namespace,
		Resolver:     mustResolver(f.t, f.root),
		ProjectRoot:  f.root,
		Sampling:     types.SamplingParams{MaxTokens: 512},
		Spawner:      f.spw,
		ReportSink:   f.spw,
		ReportChecker: f.chk,
	})
	if err != nil {
		return nil, err
	}
	// 注入消息（ProvInjected，血缘指向父的条目）。
	for _, src := range plan.Injected {
		e := types.NewLogEntry(plan.ID, src.Role, src.Content)
		e.Prov = types.ProvInjected
		e.SourceIDs = []types.MessageID{src.ID}
		if _, err := child.log.Append(ctx, e); err != nil {
			return nil, err
		}
		child.addToView(RoleForLogRole(e.Role), e.ID)
	}
	if err := child.AppendUser(ctx, req.TaskDescription); err != nil {
		return nil, err
	}
	return child, nil
}

// setupFork 装配一套父+子可跑的完整环境。
func setupFork(t *testing.T) (*Agent, *spawner.Spawner, *forkFactory, *store.SQLiteStore, string) {
	t.Helper()
	st, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	root := workspaceRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "src", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/main.go", "package main\n")
	writeFile(t, root, "src/auth/oauth.go", "package auth\n// oauth impl\n")

	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite} {
		if err := reg.Register(sk); err != nil {
			t.Fatal(err)
		}
	}
	chk, err := spawner.NewReportChecker(spawner.ReportCheckerConfig{Root: root, MaxScanFiles: 50})
	if err != nil {
		t.Fatal(err)
	}
	spw, err := spawner.New(spawner.Config{
		MaxDepth:        1,
		MaxActive:       8,
		MaxForkRounds:   3,
		CanSpawnAtDepth: func(depth int) bool { return depth == 0 },
		Log:             st,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentNS := &types.Namespace{AgentID: "parent-1", Mounts: []types.Mount{
		{Pattern: "**", Mode: types.PathWrite},
	}}
	mb := spw.Bootstrap("parent-1", parentNS)
	ff := &forkFactory{t: t, st: st, reg: reg, root: root, spw: spw, chk: chk}
	if err := spw.SetFactory(ff); err != nil {
		t.Fatal(err)
	}

	parent, err := New(Config{
		ID:           "parent-1",
		SystemPrompt: "You are the parent; fork children for subtasks.",
		MaxRounds:    8,
		Log:          st,
		Views:        st,
		LLM:          &fakeLLM{}, // 占位：各测试用例替换脚本（in-package 访问）
		Skills:       reg,
		Namespace:    parentNS,
		Resolver:     mustResolver(t, root),
		ProjectRoot:  root,
		Sampling:     types.SamplingParams{MaxTokens: 512},
		Mailbox:      mb,
		Spawner:      spw,
	})
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	return parent, spw, ff, st, root
}

// spawnCall 构造 spawn_subagent 调用。
func spawnCall(profile, task string, writable []string, inject []int64) types.ToolCall {
	args := map[string]any{"profile_id": profile, "task": task}
	if writable != nil {
		args["writable_paths"] = writable
	}
	if inject != nil {
		args["inject_message_seqs"] = inject
	}
	b, _ := json.Marshal(args)
	return types.ToolCall{ID: "call-spawn", Name: "spawn_subagent", Arguments: b}
}

// reportCall 构造 report_to_parent 调用。
func reportCall(status, text string) types.ToolCall {
	b, _ := json.Marshal(map[string]any{"report": text, "status": status})
	return types.ToolCall{ID: "call-report", Name: "report_to_parent", Arguments: b}
}

// TestForkSingleChild 是 13.7 的交付测试形态：
// 父 fork 子（子写文件）→ 子 report success → report 进父 Log + View。
func TestForkSingleChild(t *testing.T) {
	ctx := context.Background()
	parent, _, ff, _, root := setupFork(t)

	// 子脚本：写文件 → report success（scriptFn 按 plan 取，无需预知 id）。
	ff.scriptFn = func(plan *spawner.ChildPlan) []*wire.WireTurn {
		return []*wire.WireTurn{
			toolCallTurn(mkCallID("file_write", "c1", map[string]any{
				"path": "src/auth/child_out.txt", "content": "child was here\n",
			})),
			toolCallTurn(reportCall("success", "已写入 src/auth/child_out.txt，内容为占位文本。")),
		}
	}
	// 父脚本：spawn → 收到 report 后总结。
	parent.llm = &scriptedParent{turns: []*wire.WireTurn{
		toolCallTurn(spawnCall("coder", "在 src/auth/ 下写 child_out.txt，内容为占位文本", []string{"src/auth/**"}, nil)),
	}, final: replyTurn("子已完成：child_out.txt 写好了。")}

	if err := parent.AppendUser(ctx, "fork 一个子去写 src/auth/child_out.txt"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 子写的文件存在（在 writable_paths 内）。
	raw, err := os.ReadFile(filepath.Join(root, "src", "auth", "child_out.txt"))
	if err != nil || !strings.Contains(string(raw), "child was here") {
		t.Fatalf("child file: %v %q", err, raw)
	}

	childID := types.AgentID("")
	for id := range parent.ChildrenStatus() {
		childID = id
	}
	if childID == "" {
		t.Fatal("no child spawned")
	}

	// report 进父 Log（RoleSubTaskResult）+ View。
	entries := entriesMust(t, parent)
	var sub *types.LogEntry
	for _, e := range entries {
		if e.Role == types.RoleSubTaskResult {
			sub = e
		}
	}
	if sub == nil {
		t.Fatalf("no sub_task_result in parent log: %v", entries)
	}
	if sub.Meta["child_id"] != string(childID) || sub.Meta["status"] != "success" {
		t.Fatalf("sub meta: %+v", sub.Meta)
	}
	inView := false
	for _, it := range parent.View().Items {
		if it.Ref == sub.ID {
			inView = true
		}
	}
	if !inView {
		t.Fatal("sub_task_result not referenced by parent view")
	}
	// 子状态 Done。
	if st := parent.ChildrenStatus()[childID]; st != types.ChildDone {
		t.Fatalf("child status = %v", st)
	}
}

// scriptedParent 是父的 LLM 替身：按脚本逐轮返回，末轮后重复 final。
type scriptedParent struct {
	turns []*wire.WireTurn
	final *wire.WireTurn
	reqs  []*wire.CanonicalRequest
	round int
}

func (s *scriptedParent) ExecuteTurn(_ context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	s.reqs = append(s.reqs, req)
	if s.round < len(s.turns) {
		t := s.turns[s.round]
		s.round++
		return t, nil
	}
	return s.final, nil
}

// TestForkNamespaceExceeded 越权 writable 被拒且错误回传（Part 9.4）。
func TestForkNamespaceExceeded(t *testing.T) {
	ctx := context.Background()
	parent, _, _, _, _ := setupFork(t)
	// 父命名空间收窄到 src/**，子请求 etc/**。
	parent.env.Namespace.Mounts = []types.Mount{{Pattern: "src/**", Mode: types.PathWrite}}
	parent.llm = &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(spawnCall("coder", "x", []string{"etc/**"}, nil)),
		replyTurn("收到拒绝，放弃 fork。"),
	}}
	if err := parent.AppendUser(ctx, "fork"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Run(ctx); err != nil {
		t.Fatalf("rejected spawn must not abort: %v", err)
	}
	entries := entriesMust(t, parent)
	found := false
	for _, e := range entries {
		if e.Role == types.RoleToolResult && e.Meta["error_type"] == "NAMESPACE_EXCEEDED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NAMESPACE_EXCEEDED feedback missing: %v", entries)
	}
}

// TestForkDepthGate 子再 fork 被深度/权限闸拒绝。
func TestForkDepthGate(t *testing.T) {
	ctx := context.Background()
	parent, _, ff, _, _ := setupFork(t)

	// 子脚本：尝试再 fork（应被拒）→ report success。
	ff.scriptFn = func(*spawner.ChildPlan) []*wire.WireTurn {
		return []*wire.WireTurn{
			toolCallTurn(spawnCall("coder", "grandchild", []string{"src/**"}, nil)),
			toolCallTurn(reportCall("success", "尝试 fork 被拒后直接完成了任务。")),
		}
	}
	parent.llm = &scriptedParent{turns: []*wire.WireTurn{
		toolCallTurn(spawnCall("coder", "子任务：读 src/auth/oauth.go", []string{"src/auth/**"}, nil)),
	}, final: replyTurn("done")}

	if err := parent.AppendUser(ctx, "fork 一个子"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 找到子，验证它的 Log 里有 MAX_DEPTH/权限拒绝的 tool_result。
	var childID types.AgentID
	for id := range parent.ChildrenStatus() {
		childID = id
	}
	if childID == "" {
		t.Fatal("no child spawned")
	}
	entries, err := parent.log.Range(ctx, childID, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	denied := false
	for _, e := range entries {
		if e.Role == types.RoleToolResult &&
			(strings.Contains(e.Content, "MAX_DEPTH_REACHED") || strings.Contains(e.Content, "PROFILE_NOT_PERMITTED")) {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("child spawn denial missing: %v", entries)
	}
}

// TestForkInjectedMessages 注入的父消息以 ProvInjected 进子的上下文。
func TestForkInjectedMessages(t *testing.T) {
	ctx := context.Background()
	parent, _, ff, _, _ := setupFork(t)

	// 预置父 Log 的一条消息（seq 1 = AppendUser 的任务）。
	if err := parent.AppendUser(ctx, "契约：OAuth 必须走 PKCE"); err != nil {
		t.Fatal(err)
	}
	ff.scriptFn = func(*spawner.ChildPlan) []*wire.WireTurn {
		return []*wire.WireTurn{toolCallTurn(reportCall("success", "已阅读注入的契约。"))}
	}
	parent.llm = &scriptedParent{turns: []*wire.WireTurn{
		toolCallTurn(spawnCall("coder", "阅读契约并确认", nil, []int64{1})),
	}, final: replyTurn("done")}

	if err := parent.AppendUser(ctx, "fork 一个子读契约"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 子的 Log 里有 ProvInjected 条目。
	var childID types.AgentID
	for id := range parent.ChildrenStatus() {
		childID = id
	}
	entries, err := parent.log.Range(ctx, childID, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	injected := false
	for _, e := range entries {
		if e.Prov == types.ProvInjected && strings.Contains(e.Content, "PKCE") {
			injected = true
		}
	}
	if !injected {
		t.Fatalf("injected message missing in child log: %v", entries)
	}
}

// TestForkChildSilentExit 框架代报：子退出未 report → 父收到 failed。
func TestForkChildSilentExit(t *testing.T) {
	ctx := context.Background()
	parent, _, ff, _, _ := setupFork(t)

	// 子脚本：只回复不 report（eventLoop 正常结束）。
	ff.scriptFn = func(*spawner.ChildPlan) []*wire.WireTurn {
		return []*wire.WireTurn{replyTurn("我做完了但忘了 report。")}
	}
	parent.llm = &scriptedParent{turns: []*wire.WireTurn{
		toolCallTurn(spawnCall("coder", "子任务", []string{"src/auth/**"}, nil)),
	}, final: replyTurn("done")}

	if err := parent.AppendUser(ctx, "fork"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Run(ctx); err != nil {
		t.Fatalf("Run: %v (framework report must unblock parent)", err)
	}
	entries := entriesMust(t, parent)
	found := false
	for _, e := range entries {
		if e.Role == types.RoleSubTaskResult && strings.Contains(e.Content, "框架代报") {
			found = true
		}
	}
	if !found {
		t.Fatalf("framework surrogate report missing: %v", entries)
	}
}

// 编译期保证：测试内的接口实现成立。
var _ spawner.ChildFactory = (*forkFactory)(nil)
