// 命令 topo_test 是设计文档 13.11（阶段 9）的交付检查命令：
// 三层拓扑（根 → 3 子 → 3 孙）、spawn_batch、Watchdog 强制终止——全部
// dry-run（不联网），但 Spawner / Mailbox / 等待策略 / Watchdog 都是
// **真实装配**。
//
// 交付判据（13.11）：
//   - 根 fork 3 个子，每个子 fork 1 个孙；全部 report 收敛到对应父；
//   - 孙到 MaxDepth 时再 fork 被拒（MAX_DEPTH_REACHED 如实回传 LLM）；
//   - Watchdog 终止悬停子 → 框架代报 failed → 父收到；
//   - `marl status` 能显示完整树（status 渲染面的数据源=进程表快照，
//     本命令直接打印；audit 还原形态的验证在 cmd/marl/status_render_test）。
//
// 用法：
//
//	go run ./cmd/topo_test            # 三层拓扑 + spawn_batch
//	go run ./cmd/topo_test -watchdog  # 追加 Watchdog 强杀悬停子的演示
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"marl/internal/agent"
	"marl/internal/ns"
	"marl/internal/proto"
	"marl/internal/skill"
	"marl/internal/spawner"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/watchdog"
	"marl/internal/wire"
)

func main() {
	showWatchdog := flag.Bool("watchdog", false, "追加 Watchdog 强杀悬停子的演示段落")
	flag.Parse()
	if err := run(*showWatchdog); err != nil {
		fmt.Fprintf(os.Stderr, "topo_test: %v\n", err)
		os.Exit(1)
	}
}

func run(showWatchdog bool) error {
	ctx, stop := context.WithTimeout(context.Background(), 60*time.Second)
	defer stop()

	root, err := os.MkdirTemp("", "marl-topo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	if err := os.MkdirAll(filepath.Join(root, "src", "auth"), 0o755); err != nil {
		return err
	}

	st, err := store.OpenSQLite(filepath.Join(root, "state.db"))
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()
	aud := store.AuditSQLite{SQLiteStore: st}

	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite} {
		if err := reg.Register(sk); err != nil {
			return fmt.Errorf("register %s: %w", sk.Name(), err)
		}
	}
	resolver, err := ns.NewResolver(root)
	if err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	chk, err := spawner.NewReportChecker(spawner.ReportCheckerConfig{Root: root, MaxScanFiles: 50})
	if err != nil {
		return fmt.Errorf("report checker: %w", err)
	}

	spw, err := spawner.New(spawner.Config{
		MaxDepth:      2,
		MaxActive:     16,
		MaxForkRounds: 3,
		// 深度权限全开：到顶拒绝的判据是 MAX_DEPTH_REACHED（不是角色）。
		CanSpawnAtDepth: func(int) bool { return true },
		Log:             st,
		Audit:           aud,
	})
	if err != nil {
		return fmt.Errorf("spawner: %w", err)
	}

	parentNS := &types.Namespace{AgentID: "root-1", Mounts: []types.Mount{
		{Pattern: "**", Mode: types.PathWrite},
	}}
	mb := spw.Bootstrap("root-1", parentNS)

	// 工厂：子的 LLM 按深度脚本；悬停标记见 BuildChild。
	factory := &topoFactory{st: st, reg: reg, root: root, resolver: resolver, chk: chk, spw: spw}
	if err := spw.SetFactory(factory); err != nil {
		return err
	}

	rootAgent, err := agent.New(agent.Config{
		ID:           "root-1",
		SystemPrompt: "你是根 Agent：用 spawn_batch 并行分派子任务。",
		MaxRounds:    8,
		Log:          st, Views: st,
		LLM:       &rootScript{},
		Skills:    reg,
		Namespace: parentNS, Resolver: resolver, ProjectRoot: root,
		Sampling: types.SamplingParams{MaxTokens: 512},
		Mailbox:  mb, Spawner: spw,
		TaskID: "topo-task", Audit: aud,
	})
	if err != nil {
		return fmt.Errorf("root agent: %w", err)
	}
	if err := rootAgent.AppendUser(ctx, "三个并行子任务：各 fork 一个孙写 g_[abc]_grand_out.txt；完成后汇报。"); err != nil {
		return err
	}
	if err := rootAgent.Run(ctx); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	fmt.Print(renderTree(spw, "root-1"))

	if showWatchdog {
		if err := watchdogDemo(ctx, st, reg, resolver, chk, spw, root); err != nil {
			return err
		}
	}
	printFileTree(root)
	return nil
}

// ---- 脚本与工厂 ----

type topoFactory struct {
	st       *store.SQLiteStore
	reg      skill.Registry
	root     string
	resolver types.Resolver
	chk      proto.ReportChecker
	spw      *spawner.Spawner
}

func (f *topoFactory) BuildChild(ctx context.Context, plan *spawner.ChildPlan, req *proto.SpawnRequest) (spawner.ChildRunner, error) {
	// 悬停标记协议（Watchdog 演示的任务文本带"（悬停）"——工厂以此
	// 决定 LLM 替身；真实场景的悬停发生在模型无限等待上游响应处）。
	var llm agent.LLMExecutor = &depthScript{depth: plan.Depth, task: req.TaskDescription}
	if strings.Contains(req.TaskDescription, "（悬停）") {
		llm = &hangLLM{}
	}
	child, err := agent.New(agent.Config{
		ID: plan.ID, ParentID: plan.ParentID, Depth: plan.Depth, MaxDepth: 2,
		Mailbox:      plan.Mailbox,
		SystemPrompt: "你是子/孙 Agent：完成任务后 report。",
		MaxRounds:    6,
		Log:          f.st, Views: f.st,
		LLM:       llm,
		Skills:    f.reg,
		Namespace: plan.Namespace, Resolver: f.resolver, ProjectRoot: f.root,
		Sampling: types.SamplingParams{MaxTokens: 512},
		Spawner:  f.spw, ReportSink: f.spw, ReportChecker: f.chk,
		TaskID: "topo-task",
	})
	if err != nil {
		return nil, err
	}
	if err := child.AppendUser(ctx, req.TaskDescription); err != nil {
		return nil, err
	}
	return child, nil
}

// depthScript 按深度给脚本：depth1 再 fork 一个孙；depth2 写文件并 report。
type depthScript struct {
	depth int
	task  string
	round int
}

func (d *depthScript) ExecuteTurn(_ context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	defer func() { d.round++ }()
	switch d.depth {
	case 1:
		switch d.round {
		case 0:
			// 孙的任务 = 父任务的原样透传（里面的 child_[abc] 前缀是孙
			// 产物文件名的全部来源——孙的脚本从同一文本里取前缀）。
			return turnToolT("spawn_subagent", map[string]any{
				"profile_id":     "coder",
				"task":           d.task + "（孙档：写入你范围内 " + grandPrefixOf(d.task) + "_grand_out.txt），然后 report。",
				"writable_paths": []string{"src/**"},
			}), nil
		default:
			return turnToolT("report_to_parent", map[string]any{
				"status": "success",
				"report": "孙的结果已聚合（" + grandPrefixOf(d.task) + "_grand_out.txt）。",
			}), nil
		}
	default:
		switch d.round {
		case 0:
			return turnToolT("file_write", map[string]any{
				"path":    "src/" + grandPrefixOf(d.task) + "_grand_out.txt",
				"content": "grand wrote here (task=" + d.task + ")\n",
			}), nil
		default:
			return turnToolT("report_to_parent", map[string]any{
				"status": "success",
				"report": "孙已写 src/g_grand_out.txt。",
			}), nil
		}
	}
}

// grandPrefixOf 从任务文本里解析孙的文件名前缀（演示协议：根的任务
// 文本带 "child_[abc]"；孙的真实产物路径来自它自己的工具参数——
// 解析只是演示脚本对占位任务文本的约定）。
func grandPrefixOf(task string) string {
	for _, tok := range strings.Fields(task) {
		if i := strings.Index(tok, "（"); i >= 0 { // CJK 说明短语的截断（真机踩坑面）
			tok = tok[:i]
		}
		tok = strings.Trim(tok, "，。 ")
		if strings.HasPrefix(tok, "child_") {
			return tok[len("child_"):]
		}
	}
	return "g"
}

// rootScript：batch 3 个子（await all），收齐后总结。
type rootScript struct{ round int }

func (r *rootScript) ExecuteTurn(ctx context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	defer func() { r.round++ }()
	switch r.round {
	case 0:
		items := make([]map[string]any, 0, 3)
		for _, ch := range []string{"a", "b", "c"} {
			items = append(items, map[string]any{
				"profile_id":     "coder",
				"task":           "孙写 src/child_" + ch + "_grand_out.txt，路径前缀 child_" + ch,
				"writable_paths": []string{"src/**"},
			})
		}
		return turnToolT("spawn_batch", map[string]any{"items": items, "await": "all"}), nil
	default:
		return turnReplyT("三个子任务全部完成（带孙）。"), nil
	}
}

// ---- Watchdog 演示（悬停子 → 终止 → 框架代报 failed） ----

func watchdogDemo(ctx context.Context, st *store.SQLiteStore, reg skill.Registry, resolver types.Resolver, chk proto.ReportChecker, spw *spawner.Spawner, root string) error {
	fmt.Println("\n=== Watchdog 演示（悬停子 1.5s，超时预算 0.6s）===")
	// 新开一个 root scope（复用 spawner；进程表新开一个独立 root）。
	wd, err := watchdog.New(watchdog.Config{
		Table:    watchdog.AdaptTable(spw),
		Log:      st,
		Interval: 30 * time.Millisecond,
		// 超时预算 0.6s（判据字段是 int64 秒——0.6s 的演示取 1s，等待
		// 悬停 1.5s 超额后的交代在时间线里）。
		MaxChildSeconds: 1,
		MaxChildTokens:  0,
	})
	if err != nil {
		return err
	}
	wdCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wd.Start(wdCtx)
	defer wd.Stop()

	// 悬停子的父（root-2）。
	ns2 := &types.Namespace{AgentID: "root-2", Mounts: []types.Mount{
		{Pattern: "**", Mode: types.PathWrite},
	}}
	mb2 := spw.Bootstrap("root-2", ns2)
	p2, err := agent.New(agent.Config{
		ID: "root-2", SystemPrompt: "你是根 Agent2：fork 一个慢子。",
		MaxRounds: 6, Log: st, Views: st, LLM: &rootSlowScript{}, Skills: reg,
		Namespace: ns2, Resolver: resolver, ProjectRoot: root,
		Sampling: types.SamplingParams{MaxTokens: 512},
		Mailbox:  mb2, Spawner: spw,
		TaskID: "watchdog-task", Audit: store.AuditSQLite{SQLiteStore: st},
	})
	if err != nil {
		return err
	}
	if err := p2.AppendUser(ctx, "fork 一个慢子"); err != nil {
		return err
	}
	if err := p2.Run(ctx); err != nil {
		return fmt.Errorf("watchdog run: %w", err)
	}
	fmt.Print(renderTree(spw, "root-2"))
	return nil
}

// hangLLM 是悬停替身（ExecuteTurn 阻塞至取消——真实 Watchdog 的猎物）。
type rootSlowScript struct{ round int }

func (r *rootSlowScript) ExecuteTurn(_ context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	defer func() { r.round++ }()
	switch r.round {
	case 0:
		return turnToolT("spawn_subagent", map[string]any{
			"profile_id":     "coder",
			"task":           "一个永远在等数据的任务（悬停）",
			"writable_paths": []string{"src/**"},
		}), nil
	default:
		return turnReplyT("Watchdog 已经代报 failed；任务收尾。"), nil
	}
}

type hangLLM struct{}

func (*hangLLM) ExecuteTurn(ctx context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	select {
	case <-time.After(10 * time.Second):
		return turnReplyT("终于醒了"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---- 输出与通用件 ----

// renderTree 用真实进程表（快照）打印树；块的缩进与 currentID 用 DFS。
func renderTree(spw *spawner.Spawner, rootID string) string {
	infos := spw.Snapshot()
	children := map[string][]spawner.ProcessInfo{}
	byID := map[string]spawner.ProcessInfo{}
	for _, p := range infos {
		id := string(p.ID)
		byID[id] = p
		children[string(p.ParentID)] = append(children[string(p.ParentID)], p)
	}
	buf := ""
	var rec func(id string, depth int)
	rec = func(id string, depth int) {
		p, ok := byID[id]
		state := "（未运行）"
		if ok {
			state = string(p.State)
		}
		icon := "🤖"
		if depth > 0 {
			icon = "🔧"
		}
		buf += fmt.Sprintf("%s%s %s (depth=%d) %s\n", indentOf(depth), icon, id, depth, state)
		for _, c := range children[id] {
			rec(string(c.ID), depth+1)
		}
	}
	rec(rootID, 0)
	return buf
}

func indentOf(depth int) string {
	out, sep := "", ""
	for i := 0; i < depth; i++ {
		out += sep + "   └─ "
		sep = ""
	}
	return out
}

func printFileTree(root string) {
	entries, _ := os.ReadDir(filepath.Join(root, "src"))
	fmt.Println("src/ 下的孙产物:")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_grand_out.txt") {
			continue
		}
		fmt.Println("  ", e.Name())
	}
}

// turnToolT / turnReplyT 是 WireTurn 的最简构建（与 fork_test 的形态同源；
// cmd 包内私有的同一小工具的复制——阶段 7 的装配层落地后合并）。
func turnToolT(name string, args map[string]any) *wire.WireTurn {
	b, _ := json.Marshal(args)
	return &wire.WireTurn{Outcomes: []wire.Outcome{{
		ToolCalls: []types.ToolCall{{ID: "call-" + name, Name: name, Arguments: b}},
	}}}
}

func turnReplyT(text string) *wire.WireTurn {
	e := &types.LogEntry{
		Role: types.RoleAssistantReply, Prov: types.ProvOriginal,
		Audience: types.AudienceBoth, Content: text,
		Meta: map[string]any{"finish_reason": "stop"},
	}
	return &wire.WireTurn{Outcomes: []wire.Outcome{{Reply: text, Entry: *e}}}
}
