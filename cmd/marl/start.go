// start/say：统一 Actor 模型的人类侧命令面（Part 14.6 / 14.12 #6）。
//
//	marl start "任务"   —— 人类 spawn 项目 Agent：HumanActor 登记为监督树
//	                      的根（depth=0，caps 全量），项目 Agent 经正常
//	                      Spawner 裁决创建（requester = 人类——旧 bootstrap
//	                      特例删除）；任务作为 fork 的不可变输入（Part 8.3）。
//	                      完成后项目 Agent 的 report 投给人类收件箱。
//	marl say "文本"     —— 人类发给项目 Actor 的 MsgDirect（Part 14.5；
//	                      异步注入，下一轮编排自然看到）。
//
// 装配边界（诚实清单）：本命令是"attached 运行"形态——start 的生命周期
// 就是命令进程的生命周期（没有 daemon）。say 靠控制面文件后端的跨进程
// 通道：写 inbox/direct_<ulid>.md，运行中的 start 经 Receive 泵把信封投
// 进 Agent 的信箱。daemon 化（长驻 + IPC）是后续阶段；本命令不假装它是。
//
// 用法：
//
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/marl start -dir ~/demo "读 README.md 并总结"
//	go run ./cmd/marl say -dir ~/demo -to sub_000001 "补充一句要求"

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"marl/internal/actor"
	"marl/internal/agent"
	"marl/internal/config"
	"marl/internal/gate"
	"marl/internal/ladder"
	"marl/internal/ledger"
	"marl/internal/ns"
	"marl/internal/proto"
	"marl/internal/skill"
	"marl/internal/spawner"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
)

// cmdStart 处理 start 子命令（人类 spawn 项目 Agent 并 attached 等完成）。
func cmdStart(args []string) error {
	fs := flag.NewFlagSet("marl start", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	db := fs.String("db", "", "存储数据库路径（缺省 <dir>/.marl/store.db）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	task := strings.Join(fs.Args(), " ")
	if task == "" {
		return fmt.Errorf("usage: marl start [-dir <dir>] \"任务描述\"")
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if *db == "" {
		*db = filepath.Join(root, ".marl", "store.db")
	}
	return runStart(root, *db, task)
}

// cmdSay 处理 say 子命令（人类 → Actor 的直接消息）。
func cmdSay(args []string) error {
	fs := flag.NewFlagSet("marl say", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	to := fs.String("to", "", "目标 Actor id（缺省 project——运行中 start 的项目 Agent 约定 id）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" {
		return fmt.Errorf("usage: marl say [-dir <dir>] [-to <actor-id>] \"文本\"")
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	backend, err := actor.NewFileBackend(actor.FileConfig{
		Root: controlRootOf(root), Human: actor.HumanID(osUID()),
	})
	if err != nil {
		return err
	}
	defer backend.Stop()
	env := actor.Envelope{
		From:    actor.HumanID(osUID()),
		To:      actorIDOf(*to),
		Type:    proto.MsgDirect,
		Payload: &proto.DirectMessage{Text: text},
	}
	if err := backend.Deliver(env); err != nil {
		return err
	}
	controlRoot := controlRootOf(root)
	fmt.Printf("已投递 MsgDirect → %s（收件箱 %s/inbox/）\n", env.To, controlRoot)
	fmt.Println("运行中的 marl start 会在下一轮编排看到这条消息；无运行中的 start 时它留在收件箱。")
	return nil
}

// runStart 是一次完整的 attached 运行：装配 → 人类登记 → 正常裁决建
// 项目 Agent → 等它的 report 回到收件箱。
func runStart(root, dbPath, task string) error {
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Minute)
	defer stop()

	// --- 存储与技能（与 mini 同一形态：真相之源 + 投影 + 账本 + 审计）---
	st, err := store.OpenSQLite(dbPath)
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

	// --- Gate（PDP + grants/ 落盘；重启回插人类批过的 always）---
	controlRoot := controlRootOf(root)
	grants, err := gate.NewGrantStore(controlRoot)
	if err != nil {
		return err
	}
	pdp, err := gate.NewManager(gate.ManagerConfig{Rules: orchRules(), Grants: grants, Audit: aud})
	if err != nil {
		return err
	}
	saved, err := grants.LoadGrants(ctx)
	if err != nil {
		return fmt.Errorf("load grants: %w", err)
	}
	for _, g := range saved {
		if err := pdp.AddGrant(ctx, &gate.Request{Kind: gate.Kind(g.Rule.Match["kind"])},
			gate.Grant{Mode: gate.GrantAlways, Reason: g.Rule.Reason}, g.GrantedBy); err != nil {
			return fmt.Errorf("regrant %s: %w", g.Rule.ID, err)
		}
	}
	if len(saved) > 0 {
		fmt.Printf("已恢复 %d 条永久放行规则（grants/）\n", len(saved))
	}

	// --- 线路（真跑形态：DEEPSEEK_API_KEY 在场；每 Agent 独立 Binding
	// ——缓存桶 = AgentID 的 Patch 1 契约）---
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		return fmt.Errorf("env DEEPSEEK_API_KEY is empty（真跑形态需要密钥）")
	}
	newLine := func(agentID types.AgentID) (types.Binding, agent.LLMExecutor, error) {
		adapter, err := wire.NewDeepSeekChatAdapter(wire.DeepSeekChatConfig{
			EndpointName: "deepseek-main",
			BaseURL:      "https://api.deepseek.com/v1",
			APIKey:       key,
			RemoteNames:  map[string]string{"deepseek-flash": "deepseek-flash"},
			BucketField:  wire.DefaultBucketField,
		})
		if err != nil {
			return types.Binding{}, nil, err
		}
		// 能力源 = 目录（Normalizer 的档位翻译面；nil 是装配错误）。
		norm, err := wire.NewOpenAICompatNormalizer(startCatalog(), nil)
		if err != nil {
			return types.Binding{}, nil, err
		}
		b := startBinding(agentID)
		return b, &startLine{
			norm: norm, denorm: wire.NewOpenAICompatDenormalizer(),
			adapter: adapter, binding: b,
		}, nil
	}

	// --- 账本（单模型静态目录；价格从内嵌最小面——相对比较口径）---
	rec, err := ledger.New(st, startCatalog())
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}

	// --- Spawner（统一进程表；max_depth 只记 AI→AI——Part 14.6）---
	spw, err := spawner.New(spawner.Config{
		MaxDepth:        3,
		MaxActive:       16,
		MaxForkRounds:   8,
		CanSpawnAtDepth: func(int) bool { return true },
		Log:             st,
		Audit:           aud,
	})
	if err != nil {
		return fmt.Errorf("spawner: %w", err)
	}

	// --- 人类 Actor（监督树的根；文件后端 = 收件箱）---
	backend, err := actor.NewFileBackend(actor.FileConfig{
		Root: controlRoot, Human: actor.HumanID(osUID()),
	})
	if err != nil {
		return err
	}
	defer backend.Stop()
	human, err := actor.NewHuman(actor.HumanID(osUID()), backend, actor.HumanCaps())
	if err != nil {
		return err
	}
	if err := spw.RegisterHuman(ctx, human); err != nil {
		return fmt.Errorf("register human: %w", err)
	}
	fmt.Printf("人类 Actor：%s\n收件箱：%s/inbox/\n", human.ID(), controlRoot)

	// --- 人类侧装配（HumanLink）+ 收件箱泵（回执 → 目标 Agent 信箱）---
	humanID := human.ID()
	link := spawnerHumanLink{spw: spw, human: humanID}
	go actor.PumpReceive(ctx, backend, func(env actor.Envelope) error {
		// MsgGateReply / MsgDirect 的回程路由（From/To 由文件 frontmatter
		// 与解析器保证——原则 4 的通道属性）。
		return spw.SendTo(env.To, env)
	})

	// --- 工厂（项目 Agent 与子 Agent 同一入口）---
	factory := &startFactory{st: st, reg: reg, root: root, resolver: resolver, chk: chk,
		spw: spw, newLine: newLine, pdp: pdp, rec: rec, human: link}
	if err := spw.SetFactory(factory); err != nil {
		return err
	}

	// --- 正常裁决创建项目 Agent（requester = 人类；AI 第 1 层）---
	dec, err := spw.Adjudicate(ctx, &proto.SpawnRequest{
		RequesterID:     humanID,
		ProfileID:       "default",
		TaskDescription: task,
		WritablePaths:   []string{"**"},
	})
	if err != nil {
		return fmt.Errorf("project agent adjudicate: %w", err)
	}
	if dec.Status != proto.SpawnApproved {
		return fmt.Errorf("project agent rejected: %+v", dec)
	}

	// --- attached 等待：项目 Agent 的 report 回到人类收件箱 ---
	fmt.Printf("项目 Agent：%s（运行中——Ctrl-C 中断）\n\n", dec.ChildAgentID)
	reportSeen := false
	for !reportSeen {
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待超时/中断（收件箱：%s/inbox/）", controlRoot)
		case <-time.After(300 * time.Millisecond):
		}
		// report 是纯投递形态（无回执路径）——收件箱里等第一份。
		entries, rerr := os.ReadDir(filepath.Join(controlRoot, "inbox"))
		if rerr != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "report_") {
				continue
			}
			reportSeen = true
			body, _ := os.ReadFile(filepath.Join(controlRoot, "inbox", e.Name()))
			fmt.Printf("━━ 收件箱：%s ━━\n%s\n", e.Name(), frontmatterStripped(string(body)))
		}
	}
	fmt.Printf("任务完成：report 已在收件箱（%s/inbox/）；Agent 树与账本可用 marl status / ladder_report 查看。\n", controlRoot)
	return nil
}

// ---- 装配小件 ----

// spawnerHumanLink 是 agent.HumanLink 的装配实现（包着 Spawner 的统一
// 投递面 + 人类 Actor 的注册行——Watchdog 的挂起登记也在这里）。
type spawnerHumanLink struct {
	spw   *spawner.Spawner
	human types.AgentID
}

func (l spawnerHumanLink) HumanID() types.AgentID { return l.human }

func (l spawnerHumanLink) SendToHuman(ctx context.Context, env proto.Envelope) error {
	return l.spw.SendTo(l.human, env)
}

func (l spawnerHumanLink) MarkPending(agent types.AgentID, kind string) {
	l.spw.MarkPending(agent, kind)
}

func (l spawnerHumanLink) ClearPending(agent types.AgentID) { l.spw.ClearPending(agent) }

// startFactory 是 attached 运行的 ChildFactory（项目 Agent 与子同一入口；
// 每个独立 Binding——缓存桶 = AgentID）。
type startFactory struct {
	st       *store.SQLiteStore
	reg      skill.Registry
	root     string
	resolver types.Resolver
	chk      proto.ReportChecker
	spw      *spawner.Spawner
	newLine  func(types.AgentID) (types.Binding, agent.LLMExecutor, error)
	pdp      *gate.Manager
	rec      *ledger.Recorder
	human    spawnerHumanLink
}

func (f *startFactory) BuildChild(ctx context.Context, plan *spawner.ChildPlan, req *proto.SpawnRequest) (spawner.ChildRunner, error) {
	b, llm, err := f.newLine(plan.ID)
	if err != nil {
		return nil, err
	}
	a, err := agent.New(agent.Config{
		ID: plan.ID, ParentID: plan.ParentID, Depth: plan.Depth, MaxDepth: 3,
		Mailbox:      plan.Mailbox,
		SystemPrompt: "你是 Marl 的 Agent：按任务工作；需要分治时 fork 子 Agent；完成时 report_to_parent。",
		MaxRounds:    12,
		Log:          f.st, Views: f.st,
		LLM: llm, Skills: f.reg,
		Namespace: plan.Namespace, Resolver: f.resolver, ProjectRoot: f.root,
		Sampling: types.SamplingParams{MaxTokens: 2048, TimeoutMs: 120_000},
		TaskID:   "start-task",
		Audit:    store.AuditSQLite{SQLiteStore: f.st},
		Spawner:  f.spw, ReportSink: f.spw, ReportChecker: f.chk,
		Human: f.human,
		LLMCall: &agent.LLMCallConfig{
			Limits: config.DefaultLimits(),
			Gates:  f.pdp,
		},
		Ledger: f.rec,
	})
	if err != nil {
		return nil, err
	}
	if err := a.SetBinding(b); err != nil {
		return nil, err
	}
	if err := a.AppendUser(ctx, req.TaskDescription); err != nil {
		return nil, err
	}
	return a, nil
}

// startBinding 是单绑定（r0 形态；CacheBucket = AgentID 的 Patch 1 契约）。
func startBinding(id types.AgentID) types.Binding {
	return types.Binding{
		RungID: "r0", RungIndex: 0,
		Endpoint: "deepseek-main", Model: "deepseek-flash",
		CacheBucket: id, Wire: types.WireOpenAIChat,
		Thinking:    types.ThinkingSpec{Level: "off"},
		BoundAt:     time.Now(),
		CachePrefix: wire.ModelCachePrefix("deepseek-flash", "deepseek-main"),
	}
}

// startLine 是 LLMExecutor 的直连实现（CanonicalRequest → Normalizer →
// Adapter → Denormalize；与 cmd/mini 的 directLine 同形——各命令私有的
// 既有取舍）。
type startLine struct {
	norm    *wire.OpenAICompatNormalizer
	denorm  *wire.OpenAICompatDenormalizer
	adapter *wire.DeepSeekChatAdapter
	binding types.Binding
}

func (l *startLine) ExecuteTurn(ctx context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	wr, _, err := l.norm.BuildRequest(req, l.binding)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if err := l.norm.Assert(wr); err != nil {
		return nil, fmt.Errorf("assert: %w", err)
	}
	resp, err := l.adapter.Execute(ctx, wr, l.binding)
	if err != nil {
		return nil, err
	}
	return l.denorm.Denormalize(resp)
}

// startCatalog 是 ledger 的最小静态目录（单模型计价；DeepSeek 官方价随
// 时间变化——报表的绝对金额是相对比较口径，ADR-0029 的单一来源原则下
// 正式部署从 ladder.yaml 装载）。
func startCatalog() *ladder.StaticCatalog {
	cfg := &ladder.Config{
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
	}
	cat := ladder.NewStaticCatalog(cfg.Ladder)
	if err := cat.AddModel(wire.ModelEntry{
		ID: "deepseek-flash", Provider: "deepseek", Wire: types.WireOpenAIChat, RemoteName: "deepseek-flash",
		Caps: wire.ModelCaps{
			Has:        []types.Capability{types.CapToolCall, types.CapJSONMode, types.CapThinking},
			MaxContext: 65536, MaxOutput: 8192,
			CacheMode:       wire.CacheImplicitPrefix,
			ThinkingControl: wire.ThinkControlLevel,
			ThinkingLevels:  []string{"none", "low", "high", "max"},
		},
	}); err != nil {
		panic(fmt.Sprintf("marl start: catalog: %v", err))
	}
	// 计价的注册面（StaticCatalog 的 Pricing 查找读的是这一张表——
	// ModelEntry.Pricing 字段不进它；ADR-0029 的单一来源）。
	if err := cat.AddPricing("deepseek-flash",
		wire.Pricing{InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"}); err != nil {
		panic(fmt.Sprintf("marl start: pricing: %v", err))
	}
	if err := cat.AddEndpoint(wire.EndpointConfig{
		Name: "deepseek-main", BaseURL: "https://api.deepseek.com/v1",
		KeyRef: "env:DEEPSEEK_API_KEY", MaxInflight: 4, RPM: 60,
	}); err != nil {
		panic(fmt.Sprintf("marl start: endpoint: %v", err))
	}
	return cat
}

// orchRules 是 attached 运行的默认规则表（llm_call 超限问人 + 编排破坏
// 分级——Part 11.5 的字面形态；grants/ 落盘承接 always）。
func orchRules() []gate.Rule {
	return []gate.Rule{
		{ID: "allow-low-destruction", Match: map[string]string{"kind": "orchestration", "cache_destroyed_pct": "<10"}, Action: gate.ActionAllow},
		{ID: "review-destructive", Match: map[string]string{"kind": "orchestration"}, Action: gate.ActionNeedHuman, Reason: "高破坏编排需人审阅上下文操作"},
		{ID: "review-llm-overage", Match: map[string]string{"kind": "llm_call", "task_call_count": ">=20"}, Action: gate.ActionNeedHuman, Reason: "llm_call 超出任务次数额度"},
	}
}

// frontmatterStripped 剥掉 frontmatter（人类收件箱的正文显示形态）。
func frontmatterStripped(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	if i := strings.Index(s[4:], "\n---\n"); i >= 0 {
		return strings.TrimSpace(s[4+i+5:])
	}
	return s
}

// ---- 包内共享小工具 ----

// controlRootOf 是控制面根的项目推导（Part 11.5/14.3 的路径约定；
// <project-id> 取目录名——多项目共存的最小形态）。
func controlRootOf(root string) string {
	base := filepath.Base(root)
	if base == "/" || base == "." || base == "" {
		base = "default"
	}
	return filepath.Join(stateHome(), "marl", base)
}

// stateHome 是 XDG_STATE_HOME 的收口（缺省 ~/.local/state）。
func stateHome() string {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return s
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// osUID 取 OS 用户标识（From 的推导源——能写控制面文件的进程就是同
// UID 的人类侧入口，Part 14.5；$UID 缺失时用 $USER 兜底）。
func osUID() string {
	if u := os.Getenv("UID"); u != "" {
		return u
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// actorIDOf 解析 -to（空 = 项目 Agent 的约定 id）。
// [推断: 约定 id 的解析在 daemon 阶段接入进程表查询；当前形态下未知 id
// 的信封由收件箱泵的 SendTo 报错可见。]
func actorIDOf(s string) actor.ActorID {
	if s == "" {
		s = "project"
	}
	return actor.ActorID(s)
}
