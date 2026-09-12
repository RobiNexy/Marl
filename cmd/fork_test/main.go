// 命令 fork_test 是设计文档 13.7（阶段 5）的交付检查命令：
// 父任务"列出 src/ 下所有 .go 文件，fork 一个子去读第一个"，子任务
// "读 <file>，总结前 10 行"。父收 report 后口头总结。
//
// 交付判据（13.7）：
//   - 父 fork 子的裁决走 Spawner（深度/权限/命名空间子集）；
//   - 子完成后 report，父的 Log 里能看到子的 report（sub_task_result）；
//   - 子写的文件存在于文件系统（本命令的子任务只读，写文件的路径由
//     internal/agent 的 fork 集成测试覆盖）。
//
// 用法：
//
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/fork_test   # 真跑（几分钱）
//	go run ./cmd/fork_test -dry-run                  # 脚本化回放
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"marl/internal/agent"
	"marl/internal/fossil"
	"marl/internal/ladder"
	"marl/internal/ns"
	"marl/internal/proto"
	"marl/internal/spawner"
	"marl/internal/store"
	"marl/internal/skill"
	"marl/internal/types"
	"marl/internal/wire"
)

// options 是命令行选项（与 mini 同一取舍：阶段 5/6 无配置层）。
type options struct {
	apiKeyEnv string
	baseURL   string
	dbPath    string
	dryRun    bool
	keep      bool // 保留工作区（fossil diff/timeline 的人工验证用）
}

func parseOptions(args []string) options {
	var o options
	fs := flag.NewFlagSet("fork_test", flag.ContinueOnError)
	fs.StringVar(&o.apiKeyEnv, "api-key-env", "DEEPSEEK_API_KEY", "存放密钥的环境变量名")
	fs.StringVar(&o.baseURL, "base-url", "https://api.deepseek.com/v1", "接入点根地址")
	fs.StringVar(&o.dbPath, "db", ".marl-fork/supervisor.db", "SQLite 数据库路径")
	fs.BoolVar(&o.dryRun, "dry-run", false, "脚本化回放：不联网")
	fs.BoolVar(&o.keep, "keep", false, "保留工作区目录（默认任务结束即删；人工验证 fossil 时打开）")
	_ = fs.Parse(args)
	return o
}

func main() {
	o := parseOptions(os.Args[1:])
	if err := run(o); err != nil {
		fmt.Fprintf(os.Stderr, "fork_test: %v\n", err)
		os.Exit(1)
	}
}

func run(o options) error {
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Minute)
	defer stop()

	// --- 工作区（临时目录，src/ 下放几个 .go 文件）---
	root, err := os.MkdirTemp("", "marl-fork-")
	if err != nil {
		return err
	}
	if !o.keep {
		defer os.RemoveAll(root)
	} else {
		fmt.Printf("（工作区保留：%s）\n", root)
	}
	for _, f := range []struct {
		rel, content string
	}{
		{"src/main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"},
		{"src/auth/oauth.go", "package auth\n\n// OAuth2 provider 接口的 Google 实现。\nfunc google() {}\n"},
		{"src/util/util.go", "package util\n\nfunc helper() {}\n"},
	} {
		abs := filepath.Join(root, f.rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(f.content), 0o644); err != nil {
			return err
		}
	}

	// --- 存储与技能 ---
	if err := os.MkdirAll(filepath.Dir(o.dbPath), 0o755); err != nil {
		return err
	}
	st, err := store.OpenSQLite(o.dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()
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

	// --- 线路（父与子共用适配器；Binding 每个独立——缓存桶 = AgentID）---
	key := os.Getenv(o.apiKeyEnv)
	if o.dryRun && key == "" {
		key = "dry-run-placeholder"
	}
	if !o.dryRun && key == "" {
		return fmt.Errorf("env %s is empty (use -dry-run for offline replay)", o.apiKeyEnv)
	}
	// 阶梯：r0 单档（阶段 5 不测升级；阶梯机制在阶段 4 已交付）。
	ladderCfg := singleRungLadder(o)
	cat := ladder.NewStaticCatalog(ladderCfg.Ladder)
	if err := cat.AddModel(wire.ModelEntry{
		ID: "deepseek-flash", Provider: "deepseek", Wire: types.WireOpenAIChat, RemoteName: "deepseek-flash",
		Caps: wire.ModelCaps{
			Has:             []types.Capability{types.CapToolCall, types.CapJSONMode, types.CapThinking},
			MaxContext:      65536, MaxOutput: 8192,
			CacheMode:       wire.CacheImplicitPrefix,
			ThinkingControl: wire.ThinkControlLevel,
			ThinkingLevels:  []string{"none", "low", "high", "max"},
		},
		Pricing: wire.Pricing{InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"},
	}); err != nil {
		return err
	}
	if err := cat.AddEndpoint(wire.EndpointConfig{
		Name: "deepseek-main", BaseURL: o.baseURL, KeyRef: "env:" + o.apiKeyEnv, MaxInflight: 4, RPM: 60,
	}); err != nil {
		return err
	}
	adapter, err := wire.NewDeepSeekChatAdapter(wire.DeepSeekChatConfig{
		EndpointName: "deepseek-main",
		BaseURL:      o.baseURL,
		APIKey:       key,
		RemoteNames:  map[string]string{"deepseek-flash": "deepseek-flash"},
		BucketField:  wire.DefaultBucketField,
	})
	if err != nil {
		return err
	}
	norm, err := wire.NewOpenAICompatNormalizer(capsFor(cat), nil)
	if err != nil {
		return err
	}

	// --- Fossil（阶段 6：单写者提交——父恢复后一次 commit）---
	cli, err := fossil.NewCLI("")
	if err != nil {
		return fmt.Errorf("fossil: %w", err)
	}
	repo := filepath.Join(filepath.Dir(o.dbPath), "project.fossil")
	if err := cli.InitRepo(ctx, repo, adminUserName()); err != nil {
		return fmt.Errorf("fossil init: %w", err)
	}
	if err := cli.OpenRepo(ctx, repo, root); err != nil {
		return fmt.Errorf("fossil open: %w", err)
	}

	// --- Spawner（裁决关口 + 进程表）---
	spw, err := spawner.New(spawner.Config{
		MaxDepth:        1,
		MaxActive:       8,
		MaxForkRounds:   3,
		CanSpawnAtDepth: func(depth int) bool { return depth == 0 },
		Log:             st,
		Audit:           store.AuditSQLite{SQLiteStore: st},
	})
	if err != nil {
		return fmt.Errorf("spawner: %w", err)
	}

	// --- 父 Agent ---
	parentNS := &types.Namespace{AgentID: "root-1", Mounts: []types.Mount{
		{Pattern: "**", Mode: types.PathWrite},
		{Pattern: ".marl-fork/**", Mode: types.PathHidden},
	}}
	mb := spw.Bootstrap("root-1", parentNS)
	newLine := func(agentID types.AgentID) (types.Binding, agent.LLMExecutor, error) {
		router, err := ladder.NewRouter(cat, ladderCfg, ladder.RouterPolicy{})
		if err != nil {
			return types.Binding{}, nil, err
		}
		b, err := router.Bind(types.Requirement{Require: []types.Capability{types.CapToolCall}}, agentID, ladderCfg.Ladder, 0)
		if err != nil {
			return types.Binding{}, nil, err
		}
		return b, &forkLine{norm: norm, denorm: wire.NewOpenAICompatDenormalizer(), adapter: adapter, binding: b, bucketField: wire.DefaultBucketField}, nil
	}

	// --- 子工厂（Spawner 裁决批准后调用）---
	factory := &childFactory{t: st, reg: reg, root: root, resolver: resolver, chk: chk,
		spw: spw, newLine: newLine, dryRun: o.dryRun}
	if err := spw.SetFactory(factory); err != nil {
		return err
	}

	pb, pll, err := newLine("root-1")
	if err != nil {
		return err
	}
	var parentLLM agent.LLMExecutor = pll
	if o.dryRun {
		parentLLM = &parentScript{}
	}
	parent, err := agent.New(agent.Config{
		ID:           "root-1",
		SystemPrompt: "你是父 Agent：把可独立完成的子任务 fork 给子 Agent；等子 report 后汇总。",
		MaxRounds:    8,
		Log:          st,
		Views:        st,
		LLM:          parentLLM,
		Skills:       reg,
		Namespace:    parentNS,
		Resolver:     resolver,
		ProjectRoot:  root,
		Sampling:     types.SamplingParams{MaxTokens: 1024, TimeoutMs: 120_000},
		Thinking:     pb.Thinking,
		Mailbox:      mb,
		Spawner:      spw,
		Committer:    &agent.CommitConfig{VCS: cli, RepoPath: repo},
		TaskID:       "fork-task",
		Audit:        store.AuditSQLite{SQLiteStore: st},
	})
	if err != nil {
		return fmt.Errorf("parent agent: %w", err)
	}
	if err := parent.SetBinding(pb); err != nil {
		return err
	}
	task := "列出 src/ 下所有 .go 文件，fork 一个子 Agent；" +
			"子任务：在 src/auth/ 下写 summary.txt，内容为三行以内的 src/main.go 摘要。" +
			"等子完成后，用不超过三句话总结它做了什么。"
	if err := parent.AppendUser(ctx, task); err != nil {
		return err
	}
	if err := parent.Run(ctx); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	printTree(parent, spw)
	printTimeline(ctx, repo, cli)
	return printLog(ctx, parent.ID(), st, o.dbPath)
}

// singleRungLadder 构造单档阶梯（阶段 5/6 不测升级；计价从 ladder-mini.yaml
// 的同源形态内嵌——DeepSeek 官方价随时间变化，改动只改配置）。
func singleRungLadder(o options) *ladder.Config {
	return &ladder.Config{
		Pricing: map[string]wire.Pricing{
			"deepseek-flash": {InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"},
		},
		Ladder: &types.Ladder{
			Rungs: []types.Rung{{
				ID: "r0", Endpoint: "deepseek-main", Model: "deepseek-flash",
				CostPerMTok: 1.5, Currency: "CNY",
			}},
			Start: "r0",
		},
		Thinking: map[types.RungID]types.ThinkingSpec{
			"r0": {Level: "off"},
		},
	}
}

// capsFor 把静态目录适配成 Normalizer 的能力源。
func capsFor(cat *ladder.StaticCatalog) wire.CapsProvider { return cat }

// childFactory 是 Spawner.ChildFactory 的装配实现（构建子 Agent，不启动）。
type childFactory struct {
	t        *store.SQLiteStore
	reg      skill.Registry
	root     string
	resolver types.Resolver
	chk      proto.ReportChecker
	spw      *spawner.Spawner
	newLine  func(types.AgentID) (types.Binding, agent.LLMExecutor, error)
	dryRun   bool
}

func (f *childFactory) BuildChild(ctx context.Context, plan *spawner.ChildPlan, req *proto.SpawnRequest) (spawner.ChildRunner, error) {
	b, llm, err := f.newLine(plan.ID)
	if err != nil {
		return nil, err
	}
	if f.dryRun {
		llm = &childScript{}
	}
	child, err := agent.New(agent.Config{
		ID:           plan.ID,
		ParentID:     plan.ParentID,
		Depth:        plan.Depth,
		MaxDepth:     1,
		SystemPrompt: "你是子 Agent：完成任务后调用 report_to_parent 汇报结果。",
		MaxRounds:    6,
		Log:          f.t,
		Views:        f.t,
		LLM:          llm,
		Skills:       f.reg,
		Namespace:    plan.Namespace,
		Resolver:     f.resolver,
		ProjectRoot:  f.root,
		Sampling:     types.SamplingParams{MaxTokens: 1024, TimeoutMs: 120_000},
		Thinking:     b.Thinking,
		Spawner:      f.spw,
		ReportSink:   f.spw,
		ReportChecker: f.chk,
		TaskID:       "fork-task",
	})
	if err != nil {
		return nil, err
	}
	// 注入消息（ProvInjected，血缘指向父的条目）。
	for _, src := range plan.Injected {
		e := types.NewLogEntry(plan.ID, src.Role, src.Content)
		e.Prov = types.ProvInjected
		e.SourceIDs = []types.MessageID{src.ID}
		if err := child.AppendInjected(ctx, e); err != nil {
			return nil, err
		}
	}
	if err := child.AppendUser(ctx, req.TaskDescription); err != nil {
		return nil, err
	}
	return child, nil
}

// parentScript 是 dry-run 的父脚本：list_dir → spawn → 总结。
type parentScript struct{ round int }

func (p *parentScript) ExecuteTurn(_ context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	defer func() { p.round++ }()
	switch p.round {
	case 0:
		return withUsageT(toolCallTurnT(mkT("list_dir", map[string]any{"path": "src", "depth": 2}))), nil
	case 1:
		return withUsageT(toolCallTurnT(mkT("spawn_subagent", map[string]any{
			"profile_id":    "coder",
			"task":          "在 src/auth/ 下写 summary.txt，内容为三行以内的 src/main.go 摘要，然后 report。",
			"writable_paths": []string{"src/auth/**"},
		}))), nil
	default:
		return withUsageT(replyTurnT("子 Agent 读完了 src/main.go：一个打印 hello 的 main 包入口。任务完成。")), nil
	}
}

// childScript 是 dry-run 的子脚本：file_read → report。
type childScript struct{ round int }

func (c *childScript) ExecuteTurn(_ context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	defer func() { c.round++ }()
	switch c.round {
	case 0:
		return withUsageT(toolCallTurnT(mkT("file_read", map[string]any{"path": "src/main.go", "limit": 10}))), nil
	case 1:
		return withUsageT(toolCallTurnT(mkT("file_write", map[string]any{
			"path": "src/auth/summary.txt", "content": "package main\nmain 打印 hello\n5 行代码\n",
		}))), nil
	default:
		return withUsageT(toolCallTurnT(mkT("report_to_parent", map[string]any{
			"status": "success",
			"report": "已写入 src/auth/summary.txt：src/main.go 的三行摘要（package main / 打印 hello / 5 行代码）。",
		}))), nil
	}
}

// forkLine 是 LLMExecutor 的直接通路（与 cmd/mini 的 directLine 同形——
// 两个命令各自私有，不跨包共享；阶段 7 的装配层落地后合并）。
//
//	CanonicalRequest → Normalizer.BuildRequest → Assert →
//	Adapter.Execute → Denormalize → WireTurn。
type forkLine struct {
	norm        *wire.OpenAICompatNormalizer
	denorm      *wire.OpenAICompatDenormalizer
	adapter     *wire.DeepSeekChatAdapter
	binding     types.Binding
	bucketField string
}

// ExecuteTurn 实现 agent.LLMExecutor。
func (d *forkLine) ExecuteTurn(ctx context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	wr, _, err := d.norm.BuildRequest(req, d.binding)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if err := d.norm.Assert(wr); err != nil {
		return nil, fmt.Errorf("assert (framework/config error, not vendor): %w", err)
	}
	resp, err := d.adapter.Execute(ctx, wr, d.binding)
	if err != nil {
		return nil, err
	}
	return d.denorm.Denormalize(resp)
}

// ---- 与 mini 同形的小工具（各自包内私有，不跨命令共享）----

func mkT(name string, args map[string]any) types.ToolCall {
	b, _ := json.Marshal(args)
	return types.ToolCall{ID: "call-" + name, Name: name, Arguments: b}
}

func toolCallTurnT(calls ...types.ToolCall) *wire.WireTurn {
	return &wire.WireTurn{Outcomes: []wire.Outcome{{ToolCalls: calls}}}
}

func replyTurnT(text string) *wire.WireTurn {
	return &wire.WireTurn{Outcomes: []wire.Outcome{{
		Reply: text,
		Entry: types.LogEntry{
			Content: text, Role: types.RoleAssistantReply,
			Prov: types.ProvOriginal, Audience: types.AudienceBoth,
			Meta: map[string]any{"finish_reason": "stop"},
		},
	}}}
}

func withUsageT(turn *wire.WireTurn) *wire.WireTurn {
	u := &types.TokenUsage{PromptTokens: 4000, CompletionTokens: 250}
	for i := range turn.Outcomes {
		turn.Outcomes[i].Usage = u
	}
	return turn
}

// adminUserName 取 fossil 管理员用户（与 cmd/marl 同一规则）。
func adminUserName() string {
	if u := os.Getenv("MARL_ADMIN_USER"); u != "" {
		return u
	}
	return os.Getenv("USER")
}

// printTimeline 打印 fossil timeline（交付判据：commit 可见、author 正确）。
func printTimeline(ctx context.Context, repo string, cli *fossil.CLI) {
	fmt.Println("━━━━━━━━━━ Fossil Timeline ━━━━━━━━━━")
	entries, err := cli.Timeline(ctx, repo, 5)
	if err != nil {
		fmt.Printf("（timeline 读取失败：%v）\n", err)
		return
	}
	for _, e := range entries {
		fmt.Printf("%s [%s] %s (user: %s)\n", e.Time.Format("15:04:05"), e.Hash[:8], e.Comment, e.Author)
	}
}

// printTree 打印 Agent 树（marl status 的雏形；Part 8.6 的可观测性）。
func printTree(parent *agent.Agent, spw *spawner.Spawner) {
	fmt.Println("━━━━━━━━━━ Agent 树 ━━━━━━━━━━")
	depth, state, _ := spw.ProcessOf(parent.ID())
	fmt.Printf("root-1 (depth=%d) state=%s\n", depth, state)
	for id, st := range parent.ChildrenStatus() {
		cd, cs, _ := spw.ProcessOf(id)
		fmt.Printf("  ├─ %s (depth=%d) state=%s status=%v\n", id, cd, cs, st)
	}
}

// printLog 打印父的 Log（交付判据：sub_task_result 可见）。
func printLog(ctx context.Context, id types.AgentID, st *store.SQLiteStore, dbPath string) error {
	last, err := st.LastSeq(ctx, id)
	if err != nil {
		return err
	}
	entries, err := st.Range(ctx, id, 1, last)
	if err != nil {
		return err
	}
	fmt.Println("━━━━━━━━━━ 父 Log ━━━━━━━━━━")
	for _, e := range entries {
		line := e.Content
		if line == "" {
			if b, jerr := json.Marshal(e.Meta); jerr == nil {
				line = "meta=" + string(b)
			}
		}
		prov := string(e.Prov)
		if e.Prov != types.ProvOriginal {
			line = "«" + prov + "» " + line
		}
		fmt.Printf("[%02d] %-7s %-14s %.120q\n", e.Seq, e.Role, prov, line)
	}
	fmt.Printf("\n（可执行 sqlite3 %s \"SELECT seq, role, prov FROM log_entries;\" 验证落库）\n", dbPath)
	return nil
}
