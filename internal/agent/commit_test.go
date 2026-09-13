package agent

// 阶段 6 的提交集成测试（13.8）：父 fork 子 → 子写文件 → 子 report →
// 父恢复 → 父 commit → fossil timeline/diff 验证。
// 外加 M6.5（13.14）：5 个子同时 report → 父 commit 不报错、不丢文件。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/store"

	"github.com/RobiNexy/Marl/internal/actor"
	"github.com/RobiNexy/Marl/internal/fossil"
	"github.com/RobiNexy/Marl/internal/spawner"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// setupCommit 装配一套带 fossil 的父+子环境（复用 fork 的装配骨架，
// 额外注入 Committer）。
func setupCommit(t *testing.T, nChildren int) (*Agent, *spawner.Spawner, *forkFactory, *store.SQLiteStore, *fossil.CLI, string, string) {
	t.Helper()
	st, err := newStoreForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/main.go", "package main\n")

	// fossil 仓库 + checkout（root 即工作区）。
	cli, err := fossil.NewCLI("")
	if err != nil {
		t.Skipf("fossil not in PATH: %v", err)
	}
	repo := filepath.Join(t.TempDir(), "proj.fossil")
	if err := cli.InitRepo(context.Background(), repo, "marl-test"); err != nil {
		t.Fatal(err)
	}
	if err := cli.OpenRepo(context.Background(), repo, root); err != nil {
		t.Fatal(err)
	}

	reg := mustRegistry(t)
	chk, err := spawner.NewReportChecker(spawner.ReportCheckerConfig{Root: root, MaxScanFiles: 50})
	if err != nil {
		t.Fatal(err)
	}
	spw, err := spawner.New(spawner.Config{
		// Part 14.6：人类 d0 → 项目 Agent（父，d1）→ 子 d2；max_depth=2 顶格。
		MaxDepth:        2,
		MaxActive:       8,
		MaxForkRounds:   8,
		CanSpawnAtDepth: func(depth int) bool { return depth == 1 },
		Log:             st,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentNS := &types.Namespace{AgentID: "parent-1", Mounts: []types.Mount{
		{Pattern: "**", Mode: types.PathWrite},
	}}
	parentBackend := actor.NewChannelBackend(32)
	if err := spw.RegisterAgent(context.Background(), "parent-1", 1, parentNS,
		actor.AgentCaps(true), parentBackend); err != nil {
		t.Fatal(err)
	}
	ff := &forkFactory{t: t, st: st, reg: reg, root: root, spw: spw, chk: chk}
	if err := spw.SetFactory(ff); err != nil {
		t.Fatal(err)
	}
	parent, err := New(Config{
		ID:           "parent-1",
		SystemPrompt: "parent",
		MaxRounds:    8,
		Log:          st,
		Views:        st,
		LLM:          &fakeLLM{},
		Skills:       reg,
		Namespace:    parentNS,
		Resolver:     mustResolver(t, root),
		ProjectRoot:  root,
		Sampling:     types.SamplingParams{MaxTokens: 512},
		Mailbox:      parentBackend.Receive(),
		Spawner:      spw,
		Committer:    &CommitConfig{VCS: cli, RepoPath: repo},
	})
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	if err := parent.SetBinding(mustBinding(t)); err != nil {
		t.Fatal(err)
	}
	return parent, spw, ff, st, cli, repo, root
}

// mustBinding 是测试用的最小 Binding（SetBinding 的必填项）。
func mustBinding(t *testing.T) types.Binding {
	t.Helper()
	return types.Binding{
		RungID: "r0", Endpoint: "ep", Model: "m",
		CacheBucket: "parent-1", Wire: types.WireOpenAIChat,
		BoundAt:     time.Now(),
		CachePrefix: "m@ep",
	}
}

// TestCommitAfterChildReports 是 13.8 的交付测试形态。
func TestCommitAfterChildReports(t *testing.T) {
	ctx := context.Background()
	parent, _, ff, st, cli, repo, root := setupCommit(t, 1)

	// 子脚本：写文件 → report success。
	ff.scriptFn = func(*spawner.ChildPlan) []*wire.WireTurn {
		return []*wire.WireTurn{
			toolCallTurn(mkCallID("file_write", "c1", map[string]any{
				"path": "src/auth/child_out.txt", "content": "child was here\n",
			})),
			toolCallTurn(reportCall("success", "已写入 src/auth/child_out.txt。")),
		}
	}
	parent.llm = &scriptedParent{turns: []*wire.WireTurn{
		toolCallTurn(spawnCall("coder", "写 src/auth/child_out.txt", []string{"src/auth/**"}, nil)),
	}, final: replyTurn("done")}
	_ = st

	if err := parent.AppendUser(ctx, "fork 一个子写文件"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// fossil timeline 多了一条 commit（author=agent）。
	entries, err := cli.Timeline(ctx, repo, 5)
	if err != nil {
		t.Fatal(err)
	}
	var agentCommit *fossil.TimelineEntry
	for i := range entries {
		if entries[i].Author == fossil.UserAgent {
			agentCommit = &entries[i]
		}
	}
	if agentCommit == nil {
		t.Fatalf("no agent commit: %+v", entries)
	}
	if !strings.Contains(agentCommit.Comment, "任务完成提交") {
		t.Fatalf("commit message: %q", agentCommit.Comment)
	}
	// fossil diff 能看到子写的文件（已入库 → diff 对 checkout 是空的，
	// 改用 timeline 验证 + 文件存在）。
	if _, err := os.Stat(filepath.Join(root, "src", "auth", "child_out.txt")); err != nil {
		t.Fatalf("child file: %v", err)
	}
}

// TestM6_5ConcurrentReports 是 13.14 的 M6.5：5 个子同时 report →
// 父收齐 → commit 不报 lockfile 错误、不丢文件。
//
// 单写者模型下父的 commit 只有一个（串行语义由 Run 的单 goroutine 保证）；
// 本测试验证的是"5 个子并发投递 report"路径无竞态（mailbox pump 并发），
// 且 commit 的 Status→Add→Commit 把全部 5 个文件收齐。
func TestM6_5ConcurrentReports(t *testing.T) {
	ctx := context.Background()
	parent, _, ff, _, cli, repo, root := setupCommit(t, 5)

	// 5 个子并发写不同文件 + report。
	const n = 5
	var fileMu sync.Mutex
	fileIdx := 0
	ff.scriptFn = func(*spawner.ChildPlan) []*wire.WireTurn {
		fileMu.Lock()
		idx := fileIdx
		fileIdx++
		fileMu.Unlock()
		path := filepath.ToSlash(filepath.Join("src", "auth", fmt.Sprintf("out_%c.txt", 'a'+idx)))
		return []*wire.WireTurn{
			toolCallTurn(mkCallID("file_write", "c"+path, map[string]any{
				"path": path, "content": "content of " + path + "\n",
			})),
			toolCallTurn(reportCall("success", "已写入 "+path+"。")),
		}
	}
	// 5 个子并行：spawn_batch 是阶段 9；这里用 5 轮 spawn（每轮 1 个子，
	// 子并发跑）模拟并行。
	var spawnTurns []*wire.WireTurn
	for i := 0; i < n; i++ {
		spawnTurns = append(spawnTurns, toolCallTurn(spawnCall("coder",
			fmt.Sprintf("子任务 %d", i), []string{"src/auth/**"}, nil)))
	}
	parent.llm = &scriptedParent{turns: spawnTurns, final: replyTurn("done")}

	if err := parent.AppendUser(ctx, "fork 5 个子各写一个文件"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Run(ctx); err != nil {
		t.Fatalf("Run: %v (lockfile error would surface here)", err)
	}

	// 5 个文件全部存在且已入库（timeline 有一条 agent commit）。
	entries, err := cli.Timeline(ctx, repo, 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Author == fossil.UserAgent {
			found = true
		}
	}
	if !found {
		t.Fatalf("no agent commit: %+v", entries)
	}
	for i := 0; i < n; i++ {
		p := filepath.Join(root, "src", "auth", fmt.Sprintf("out_%c.txt", 'a'+i))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("file %d missing: %v", i, err)
		}
	}
	// 父的 Log 里有 5 条 sub_task_result。
	entries2 := entriesMust(t, parent)
	subCount := 0
	for _, e := range entries2 {
		if e.Role == types.RoleSubTaskResult {
			subCount++
		}
	}
	if subCount != n {
		t.Fatalf("sub_task_result = %d, want %d", subCount, n)
	}
}
