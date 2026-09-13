// Package server 提供 GUI/工具宿主的本地服务接口（阶段 14：GUI 就绪）。
//
// 分层与纪律：
//
//	App    —— 装配 + 操作面（本文件）：把"跑一次任务/插话/审批/看树/看
//	          账单"折算成方法；人的审批与讨论回复**继续走文件通道**——
//	          GUI 只是"人类的笔"（写的是同一个收件箱文件），原则 4 的
//	          通道语义不变，权限不因为有了 HTTP 而改道。
//	http.go —— 传输层：REST JSON + SSE 事件流；薄，不持业务。
//
// 事件流的数据源是 audit_events（系统的旁路真相，追加只读）——不另建
// pub/sub：GUI 看到的事件与 CLI/status 看到的同源同序。
//
// 并发：App 的运行态经 mu；底层组件（Spawner/Store/FileBackend）自带
// 并发契约。一个 App 同时只跑一个任务（GUI 的"开始"按钮在 running 时
// 返回 409）。
package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RobiNexy/Marl/internal/actor"
	"github.com/RobiNexy/Marl/internal/agent"
	"github.com/RobiNexy/Marl/internal/config"
	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/discuss"
	"github.com/RobiNexy/Marl/internal/escalate"
	"github.com/RobiNexy/Marl/internal/fossil"
	"github.com/RobiNexy/Marl/internal/gate"
	"github.com/RobiNexy/Marl/internal/ladder"
	"github.com/RobiNexy/Marl/internal/ledger"
	"github.com/RobiNexy/Marl/internal/ns"
	"github.com/RobiNexy/Marl/internal/profile"
	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/skill"
	"github.com/RobiNexy/Marl/internal/spawner"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// App 是一个项目的服务实例（GUI 的全部接口面）。
//
// 生命周期：NewApp（装配，fail fast）→ StartTask/... → Close。
// 并发：方法可多 goroutine 调用（HTTP handler 语义）；运行态经 mu。
type App struct {
	mu      sync.Mutex
	Root    string
	DBPath  string
	Control string
	Version string

	st       *store.SQLiteStore
	aud      store.AuditStore
	resolver types.Resolver
	chk      proto.ReportChecker
	pdp      *gate.Manager
	grants   *gate.GrantStore
	prof     *profile.Loader
	sampling types.SamplingParams
	newLine  func(types.AgentID) (types.Binding, agent.LLMExecutor, error)
	rec      *ledger.Recorder
	spw      *spawner.Spawner
	backend  *actor.FileBackend
	human    actor.ActorID
	discuss  *discuss.Manager
	esc      *escalate.Manager
	vcs      *fossil.CLI
	repo     string

	// 文件面操作核（收件箱/审批/讨论/配置/知识库——App 与 FileMailbox
	// 共用的实现）。
	lf *LocalFiles
	// onShutdown 是守护进程的退出钩子（serve 注入：优雅关停 http.Server；
	// nil = 无关停面——测试直跑 App）。
	onShutdown func()

	// mu 的注入点：OnShutdown 的 setter（构造后由 serve 调用）。
	muSet bool

	// 运行态（mu 保护）。
	runAgent     types.AgentID
	runTask      string
	runStartedAt time.Time
	// 收件箱时序（Option 注入；零值 = FileBackend 的生产缺省）。
	inboxPoll  time.Duration
	inboxQuiet time.Duration
}

// Option 是 NewApp 的函数式选项（测试注入替身线路 / 版本号）。
type Option func(*App)

// WithLineFactory 注入线路工厂（测试：脚本化假 LLM；生产：DeepSeek 直连）。
func WithLineFactory(f func(types.AgentID) (types.Binding, agent.LLMExecutor, error)) Option {
	return func(a *App) { a.newLine = f }
}

// WithVersion 注入版本号（GUI 的 about 面展示）。
func WithVersion(v string) Option { return func(a *App) { a.Version = v } }

// WithInboxTiming 注入收件箱的轮询/静默窗参数（测试提速用；生产缺省
// 500ms/10s——gate 文件是人类就地编辑的形态，direct 类型自带 1s 上限）。
func WithInboxTiming(poll, quiet time.Duration) Option {
	return func(a *App) {
		a.inboxPoll, a.inboxQuiet = poll, quiet
	}
}

// NewApp 完成一次项目的全部装配（与 CLI 的 runStart 同源同语义）：
// 存储/技能/线路/账本/Gate（含 grants 回插）/Profile/Spawner/人类 Actor
// /讨论/escalation/fossil。失败：任何关键件缺失（启动期 fail fast）。
func NewApp(root, dbPath, version string, opts ...Option) (*App, error) {
	a := &App{Root: root, DBPath: dbPath, Version: version}
	for _, o := range opts {
		o(a)
	}
	a.lf = &LocalFiles{Root: a.Root, Control: ControlRootOf(root)}
	if err := a.assemble(); err != nil {
		return nil, err
	}
	return a, nil
}

// assemble 的装配序（与 runStart 相同的顺序与语义）。
func (a *App) assemble() error {
	ctx := context.Background()
	a.Control = ControlRootOf(a.Root)
	if a.lf != nil {
		a.lf.Control = a.Control
	}

	st, err := store.OpenSQLite(a.DBPath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	a.st = st
	a.aud = store.AuditSQLite{SQLiteStore: st}

	resolver, err := ns.NewResolver(a.Root)
	if err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	a.resolver = resolver
	chk, err := spawner.NewReportChecker(spawner.ReportCheckerConfig{Root: a.Root, MaxScanFiles: 50})
	if err != nil {
		return fmt.Errorf("report checker: %w", err)
	}
	a.chk = chk

	// 项目配置（limits / gate_rules 的覆盖面）。
	cfgRaw, err := os.ReadFile(filepath.Join(a.Root, ".marl", "config.yaml"))
	if err != nil {
		return fmt.Errorf("read project config: %w（先 marl init）", err)
	}
	cfgNode, err := config.Parse(cfgRaw)
	if err != nil {
		return fmt.Errorf("parse config.yaml: %w", err)
	}
	limits, err := config.ParseLimits(cfgNode)
	if err != nil {
		return fmt.Errorf("config limits: %w", err)
	}
	rules, err := config.ParseGateRules(cfgNode)
	if err != nil {
		return fmt.Errorf("config gate_rules: %w", err)
	}
	if len(rules) == 0 {
		rules = DefaultRules()
	}

	a.grants, err = gate.NewGrantStore(a.Control)
	if err != nil {
		return err
	}
	a.pdp, err = gate.NewManager(gate.ManagerConfig{
		Rules:     rules,
		LLMLimits: &gate.LLMLimits{MaxCalls: limits.CallTaskMax, MaxTokens: int64(limits.CallTaskMaxTokens)},
		Grants:    a.grants, Audit: a.aud,
	})
	if err != nil {
		return err
	}
	// 永久放行的重启回插（人类批过的 always 不丢）。
	saved, err := a.grants.LoadGrants(ctx)
	if err != nil {
		return fmt.Errorf("load grants: %w", err)
	}
	for _, g := range saved {
		if err := a.pdp.AddGrant(ctx, &gate.Request{Kind: gate.Kind(g.Rule.Match["kind"])},
			gate.Grant{Mode: gate.GrantAlways, Reason: g.Rule.Reason}, g.GrantedBy); err != nil {
			return fmt.Errorf("regrant %s: %w", g.Rule.ID, err)
		}
	}

	// Profile（采样参数的单一来源）。
	a.prof = profile.NewLoader()
	if err := a.prof.LoadAll(filepath.Join(a.Root, ".marl", "profiles")); err != nil {
		return fmt.Errorf("load profiles: %w", err)
	}
	p, err := a.prof.Get("default")
	if err != nil {
		return fmt.Errorf("profile default: %w", err)
	}
	a.sampling = p.Sampling
	if a.sampling.MaxTokens <= 0 || a.sampling.MaxTokens > 8192 {
		a.sampling.MaxTokens = 8192
	}
	if a.sampling.TimeoutMs <= 0 {
		a.sampling.TimeoutMs = 120_000
	}

	// 线路工厂（测试注入替身；生产 DeepSeek 直连）。
	if a.newLine == nil {
		a.newLine = DeepSeekLineFactory()
	}

	a.rec, err = ledger.New(st, Catalog())
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}

	a.spw, err = spawner.New(spawner.Config{
		MaxDepth:        3,
		MaxActive:       16,
		MaxForkRounds:   8,
		CanSpawnAtDepth: func(int) bool { return true },
		Log:             st,
		Audit:           a.aud,
	})
	if err != nil {
		return fmt.Errorf("spawner: %w", err)
	}

	a.backend, err = actor.NewFileBackend(actor.FileConfig{
		Root: a.Control, Human: actor.HumanID(osUID()),
		PollInterval: a.inboxPoll, Quiescence: a.inboxQuiet,
	})
	if err != nil {
		return err
	}
	human, err := actor.NewHuman(actor.HumanID(osUID()), a.backend, actor.HumanCaps())
	if err != nil {
		return err
	}
	if err := a.spw.RegisterHuman(ctx, human); err != nil {
		return fmt.Errorf("register human: %w", err)
	}
	a.human = human.ID()

	go actor.PumpReceive(ctx, a.backend, func(env actor.Envelope) error {
		return a.spw.SendTo(env.To, env)
	})

	a.vcs, err = fossil.NewCLI("")
	if err != nil {
		return fmt.Errorf("fossil: %w", err)
	}
	a.repo = filepath.Join(a.Root, ".marl", "project.fossil")
	a.discuss, err = discuss.NewManager(discuss.Config{
		Root: a.Root, MARLDir: filepath.Join(a.Root, ".marl"),
		ControlDir:       filepath.Join(a.Control, "discussions"),
		DefaultTargetDir: filepath.Join(".marl", "knowledge", "contracts"),
		VCS:              a.vcs,
		Audit:            a.aud,
	})
	if err != nil {
		return fmt.Errorf("discuss manager: %w", err)
	}
	escMailbox, err := escalate.NewMailbox(escalate.MailboxConfig{
		ControlRoot: a.Control, Audit: a.aud,
	})
	if err != nil {
		return fmt.Errorf("escalate mailbox: %w", err)
	}
	a.esc, err = escalate.NewManager(escalate.Config{
		Rule: func() (proto.EscalationRule, error) {
			return proto.EscalationRule{FallbackHuman: true}, nil
		},
		Mailbox: escMailbox,
	})
	if err != nil {
		return fmt.Errorf("escalate manager: %w", err)
	}
	a.wireFactory()
	return nil
}

// HumanID 返回本实例的人类 ActorID（GUI 展示根节点）。
func (a *App) HumanID() actor.ActorID { return a.human }

// ---------------------------------------------------------------------------
// 任务生命周期
// ---------------------------------------------------------------------------

// ErrAlreadyRunning 是 StartTask 在任务进行中的拒绝哨兵（HTTP 409 的来源）。
var ErrAlreadyRunning = errors.New("server: a task is already running")

// StartTask 以人类 requester 正常裁决创建项目 Agent 并启动（Part 14.6）。
// 同一时刻只允许一个任务（进行中 → ErrAlreadyRunning）。
func (a *App) StartTask(task string) (types.AgentID, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return "", fmt.Errorf("server: task text is required")
	}
	a.mu.Lock()
	if a.runAgent != "" && a.runActive() {
		a.mu.Unlock()
		return "", ErrAlreadyRunning
	}
	a.mu.Unlock()
	dec, err := a.spw.Adjudicate(context.Background(), &proto.SpawnRequest{
		RequesterID:     a.human,
		ProfileID:       "default",
		TaskDescription: task,
		WritablePaths:   []string{"**"},
	})
	if err != nil {
		return "", err
	}
	if dec.Status != proto.SpawnApproved {
		return "", fmt.Errorf("task rejected: %s (%s)", dec.Reason, dec.Code)
	}
	a.mu.Lock()
	a.runAgent, a.runTask, a.runStartedAt = dec.ChildAgentID, task, time.Now()
	a.mu.Unlock()
	return dec.ChildAgentID, nil
}

// StopTask 优雅终止当前任务（SIGTERM 语义：Agent 收尾 + 代报兜底）；
// force = 立即取消。
func (a *App) StopTask(force bool) error {
	a.mu.Lock()
	id := a.runAgent
	a.mu.Unlock()
	if id == "" || !a.runActive() {
		return fmt.Errorf("server: no running task")
	}
	return a.spw.Terminate(id, "stopped by human")
}

// runActive 报告运行中的项目 Agent 是否还活着（idle/crashed = 结束）。
func (a *App) runActive() bool {
	_, state, ok := a.spw.ProcessOf(a.runAgent)
	if !ok {
		return false
	}
	return state == types.StateRunning || state == types.StateBlocked
}

// CurrentRun 返回当前任务态（GUI 顶栏）。
func (a *App) CurrentRun() contract.RunStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := contract.RunStatus{Agent: string(a.runAgent), Task: a.runTask, StartedAt: a.runStartedAt}
	out.Active = a.runAgent != "" && a.runActive()
	if out.Active {
		if _, state, ok := a.spw.ProcessOf(a.runAgent); ok {
			out.State = string(state)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 观测：监督树 / 事件 / 对话 / 账单
// ---------------------------------------------------------------------------

// Agents 返回全部 Actor 的快照（GUI 树的数据源）。
func (a *App) Agents() []contract.AgentView {
	infos := a.spw.Snapshot()
	out := make([]contract.AgentView, 0, len(infos))
	for _, p := range infos {
		out = append(out, contract.AgentView{
			ID: string(p.ID), Parent: string(p.ParentID), Kind: p.Kind,
			Depth: p.Depth, State: string(p.State),
			PendingKind: p.PendingKind, PendingAt: p.PendingAt, StartedAt: p.StartedAt,
		})
	}
	return out
}

// Event 是事件流的单元（audit_events 的透传 + 序号游标）。
type Event = store.AuditEvent

// Events 返回 seq 之后的事件（SSE 轮询的游标拉取面）。
func (a *App) Events(ctx context.Context, since int64, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 200
	}
	return a.aud.Query(ctx, store.AuditFilter{AfterSeq: since, Limit: limit})
}

// Conversation 返回一个 Agent 的对话条目（GUI 聊天流）。
func (a *App) Conversation(ctx context.Context, agentID string) ([]*types.LogEntry, error) {
	id := types.AgentID(agentID)
	if id == "" {
		return nil, fmt.Errorf("server: agent id required")
	}
	last, err := a.st.LastSeq(ctx, id)
	if err != nil {
		return nil, err
	}
	if last == 0 {
		return nil, nil
	}
	return a.st.Range(ctx, id, 1, last)
}

// ConversationMarkdown 导出全量对话（Part 1.4 的格式；GUI 的"导出"按钮）。
func (a *App) ConversationMarkdown(ctx context.Context, agentID string) (string, error) {
	id := types.AgentID(agentID)
	return RenderConversationMarkdown(ctx, a.st, id, 0)
}

// Costs 返回任务账单（GUI 的成本面板）。
func (a *App) Costs(ctx context.Context, taskID string) (*store.TaskCostSummary, error) {
	if taskID == "" {
		taskID = "start-task"
	}
	return a.rec.TaskSummary(ctx, types.TaskID(taskID))
}

// ---------------------------------------------------------------------------
// 交互：插话 / 收件箱 / 审批 / 讨论
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 配置与知识（GUI 的设置页）
// ---------------------------------------------------------------------------

// Doctor 跑环境自检（GUI 的诊断页）。
func (a *App) Doctor() []CheckResult { return RunChecks(a.Root) }

// DoctorOn 对任意目录跑自检（NewApp 之前就能用——"这个目录能不能跑起来"
// 是先于装配的问题）。
func DoctorOn(root string) []CheckResult { return RunChecks(root) }

// Close 释放资源（store 等；进程退出前的收尾点；契约要求 error 返回）。
func (a *App) Close() error { a.st.Close(); return nil }

// ---- 小件（类型化静默窗/文件名守卫/预览）----

// ---- 线路与目录的装配件（CLI 与测试共用）----

// DeepSeekLineFactory 是生产的线路工厂（每 Agent 独立 Binding——
// 缓存桶 = AgentID 的隔离契约）。
func DeepSeekLineFactory() func(types.AgentID) (types.Binding, agent.LLMExecutor, error) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	return func(agentID types.AgentID) (types.Binding, agent.LLMExecutor, error) {
		if key == "" {
			return types.Binding{}, nil, fmt.Errorf("env DEEPSEEK_API_KEY is empty（真跑形态需要密钥）")
		}
		return buildDeepSeekLine(agentID, key)
	}
}

func buildDeepSeekLine(agentID types.AgentID, key string) (types.Binding, agent.LLMExecutor, error) {
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
	norm, err := wire.NewOpenAICompatNormalizer(Catalog(), nil)
	if err != nil {
		return types.Binding{}, nil, err
	}
	b := TaskBinding(agentID)
	return b, &directLine{
		norm: norm, denorm: wire.NewOpenAICompatDenormalizer(),
		adapter: adapter, binding: b,
	}, nil
}

// directLine 是 LLMExecutor 的直连实现（CanonicalRequest → Normalizer →
// Adapter → Denormalize）。
type directLine struct {
	norm    *wire.OpenAICompatNormalizer
	denorm  *wire.OpenAICompatDenormalizer
	adapter *wire.DeepSeekChatAdapter
	binding types.Binding
}

func (l *directLine) ExecuteTurn(ctx context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
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

// TaskBinding 是单绑定（r0；CacheBucket = AgentID 的 Patch 1 契约）。
func TaskBinding(id types.AgentID) types.Binding {
	return types.Binding{
		RungID: "r0", RungIndex: 0,
		Endpoint: "deepseek-main", Model: "deepseek-flash",
		CacheBucket: id, Wire: types.WireOpenAIChat,
		Thinking:    types.ThinkingSpec{Level: "off"},
		BoundAt:     time.Now(),
		CachePrefix: wire.ModelCachePrefix("deepseek-flash", "deepseek-main"),
	}
}

// Catalog 是单模型静态目录（账本与 Normalizer 的能力/计价源；正式部署
// 从 ladder.yaml 装载——这里是内置缺省）。
func Catalog() *ladder.StaticCatalog {
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
		panic(fmt.Sprintf("server: catalog: %v", err))
	}
	if err := cat.AddPricing("deepseek-flash",
		wire.Pricing{InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"}); err != nil {
		panic(fmt.Sprintf("server: pricing: %v", err))
	}
	if err := cat.AddEndpoint(wire.EndpointConfig{
		Name: "deepseek-main", BaseURL: "https://api.deepseek.com/v1",
		KeyRef: "env:DEEPSEEK_API_KEY", MaxInflight: 4, RPM: 60,
	}); err != nil {
		panic(fmt.Sprintf("server: endpoint: %v", err))
	}
	return cat
}

// DefaultRules 是服务的缺省规则表（编排分级 + shell 白名单；llm_call 的
// 维度缺省在 LLMLimits——"limits 即缺省政策"）。
func DefaultRules() []gate.Rule {
	return []gate.Rule{
		{ID: "allow-low-destruction", Match: map[string]string{"kind": "orchestration", "cache_destroyed_pct": "<10"}, Action: gate.ActionAllow},
		{ID: "review-destructive", Match: map[string]string{"kind": "orchestration"}, Action: gate.ActionNeedHuman, Reason: "高破坏编排需人审阅上下文操作"},
		{ID: "allow-go-toolchain", Match: map[string]string{"kind": "shell", "command_prefix": "go"}, Action: gate.ActionAllow},
		{ID: "review-shell", Match: map[string]string{"kind": "shell"}, Action: gate.ActionNeedHuman, Reason: "命令不在白名单（默认只放行 go 工具链）——需要人类审批"},
	}
}

// spawnerHumanLink 实现 agent.HumanLink（包 Spawner 的投递/登记面）。
type spawnerHumanLink struct {
	spw   *spawner.Spawner
	human actor.ActorID
}

func (l spawnerHumanLink) HumanID() actor.ActorID { return l.human }

func (l spawnerHumanLink) SendToHuman(ctx context.Context, env proto.Envelope) error {
	return l.spw.SendTo(l.human, env)
}

func (l spawnerHumanLink) MarkPending(agent types.AgentID, kind string) {
	l.spw.MarkPending(agent, kind)
}

func (l spawnerHumanLink) ClearPending(agent types.AgentID) { l.spw.ClearPending(agent) }

// osUID 取 OS 用户标识（人类 From 的推导源）。
func osUID() string {
	if u := os.Getenv("UID"); u != "" {
		return u
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// ControlRootOf 是控制面根的项目推导。
//
// [阶段 14 修正/真机 dogfooding 测试隔离实证] 项目 id = 目录基名 + 绝对
// 路径哈希（8 hex）：只用基名时，两个同名目录的项目（~/a/proj 与
// ~/b/proj）会**共享同一个收件箱/授权库**——审批文件与 grant 跨项目
// 串线（测试里所有 t.TempDir 基名同为 "001"，直接把隔离问题暴露成
// 必现故障）。哈希锚定绝对路径，显示部分保留基名可读性。
func ControlRootOf(root string) string {
	base := filepath.Base(root)
	if base == "/" || base == "." || base == "" {
		base = "default"
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(StateHome(), "marl",
		fmt.Sprintf("%s-%x", base, sum[:4]))
}

// StateHome 是 XDG_STATE_HOME 的收口（缺省 ~/.local/state）。
func StateHome() string {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return s
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// startAgentFactory 是 ChildFactory 的装配实现（GUI 跑任务与 CLI 同语义；
// 项目 Agent 根带单写者提交面）。
type startAgentFactory struct {
	app *App
}

func (f *startAgentFactory) BuildChild(ctx context.Context, plan *spawner.ChildPlan, req *proto.SpawnRequest) (spawner.ChildRunner, error) {
	b, llm, err := f.app.newLine(plan.ID)
	if err != nil {
		return nil, err
	}
	cfg := agent.Config{
		ID: plan.ID, ParentID: plan.ParentID, Depth: plan.Depth, MaxDepth: 3,
		Mailbox:      plan.Mailbox,
		SystemPrompt: "你是 Marl 的 Agent：按任务工作；需要分治时 fork 子 Agent；完成时 report_to_parent。",
		MaxRounds:    16,
		Log:          f.app.st, Views: f.app.st,
		LLM: llm, Skills: skillsRegistryForServer(),
		Namespace: plan.Namespace, Resolver: f.app.resolver, ProjectRoot: f.app.Root,
		Sampling: f.app.sampling,
		TaskID:   "start-task",
		Audit:    f.app.aud,
		Spawner:  f.app.spw, ReportSink: f.app.spw, ReportChecker: f.app.chk,
		Human:      spawnerHumanLink{spw: f.app.spw, human: f.app.human},
		LLMCall:    &agent.LLMCallConfig{Limits: config.DefaultLimits(), Gates: f.app.pdp},
		Ledger:     f.app.rec,
		Discussion: &agent.DiscussionConfig{Manager: f.app.discuss},
		Escalation: &agent.EscalationConfig{Manager: f.app.esc},
	}
	if plan.Depth == 1 {
		cfg.Committer = &agent.CommitConfig{VCS: f.app.vcs, RepoPath: f.app.repo}
	}
	a, err := agent.New(cfg)
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

// skillsRegistryForServer 构建技能注册表（与 App.assemble 同一清单）。
func skillsRegistryForServer() skill.Registry {
	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite,
		skill.ShellExec,
		skill.OrchExclude, skill.OrchRestore, skill.OrchReorder, skill.OrchAnnotate, skill.OrchPin} {
		_ = reg.Register(sk) // 内置清单无重名——失败即装配 bug，panic 可见
	}
	return reg
}

// wireFactory 把工厂接到 Spawner（assemble 的收尾调用点；工厂需要引用
// 完整装配的 App，存在构造顺序依赖）。
func (a *App) wireFactory() {
	_ = a.spw.SetFactory(&startAgentFactory{app: a})
}

// ---------------------------------------------------------------------------
// contract.Interaction 的实现层（依赖倒置的落点：宿主只看契约；本节是
// App 对契约的方法面——文件面委托 LocalFiles，引擎面自己实现）。
// ---------------------------------------------------------------------------

// SendMessage 给任意 Actor 发直接消息（进程内直达：spawner 的路由面；
// 无守护进程的宿主走 FileMailbox 的文件投递形态）。
func (a *App) SendMessage(to, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("server: message text is required")
	}
	if to == "" || to == string(a.human) {
		return fmt.Errorf("server: target agent id is required")
	}
	env := actor.Envelope{
		From:    a.human,
		To:      types.AgentID(to),
		Type:    proto.MsgDirect,
		Payload: &proto.DirectMessage{Text: text},
	}
	return a.spw.SendTo(types.AgentID(to), env)
}

// Inbox 列出待处理收件（委托 LocalFiles）。
func (a *App) Inbox() ([]contract.InboxItem, error) { return a.lf.Inbox() }

// ReadInbox 返回收件文件全文（委托 LocalFiles）。
func (a *App) ReadInbox(name string) ([]byte, error) { return a.lf.ReadInbox(name) }

// ReplyGate 把裁决写进审批文件（委托 LocalFiles——GUI 是"人类的笔"）。
func (a *App) ReplyGate(id string, d contract.GateDecision) error {
	return a.lf.ReplyGate(id, d)
}

// Discussions 列出讨论（委托 LocalFiles）。
func (a *App) Discussions() ([]contract.DiscussionView, error) { return a.lf.Discussions() }

// ReplyDiscussion 把批注/裁决写进 verdict（委托 LocalFiles）。
func (a *App) ReplyDiscussion(id, annotation string, approve bool) error {
	return a.lf.ReplyDiscussion(id, annotation, approve)
}

// Escalations 列出待人类回复的求助（委托 LocalFiles）。
func (a *App) Escalations() ([]contract.EscalationView, error) { return a.lf.Escalations() }

// ReplyEscalation 回复一条求助并归档（委托 LocalFiles——GUI 是"人类的笔"）。
func (a *App) ReplyEscalation(id, reply string) error { return a.lf.ReplyEscalation(id, reply) }

// ConfigRaw 返回 config.yaml 原文（委托 LocalFiles）。
func (a *App) ConfigRaw() ([]byte, error) { return a.lf.ConfigRaw() }

// WriteConfig 校验并写回 config.yaml（委托 LocalFiles）。
func (a *App) WriteConfig(raw []byte) error { return a.lf.WriteConfig(raw) }

// Profiles 返回全部 Profile 摘要（委托 LocalFiles）。
func (a *App) Profiles() ([]types.ProfileSummary, error) { return a.lf.Profiles() }

// ProfileRaw 返回一份 profile 文件原文（委托 LocalFiles）。
func (a *App) ProfileRaw(id string) ([]byte, error) { return a.lf.ProfileRaw(id) }

// WriteProfile 校验并写回 profile（委托 LocalFiles）。
func (a *App) WriteProfile(id string, raw []byte) error { return a.lf.WriteProfile(id, raw) }

// KnowledgeLint 的结果透传（委托 LocalFiles）。
func (a *App) KnowledgeLint() (string, error) { return a.lf.KnowledgeLint() }

// KnowledgePromote 把项目知识提交进全局库（委托 LocalFiles）。
func (a *App) KnowledgePromote(ctx context.Context, relPath, globalRepo string) (string, error) {
	return a.lf.KnowledgePromote(ctx, relPath, globalRepo)
}

// KnowledgePull 把全局库拉进 vendor/（委托 LocalFiles）。
func (a *App) KnowledgePull(ctx context.Context, globalRepo string) ([]string, error) {
	return a.lf.KnowledgePull(ctx, globalRepo)
}

// OnShutdown 注入守护进程的退出钩子（serve 的装配调用）。
func (a *App) OnShutdown(fn func()) { a.onShutdown = fn }

// 编译期断言：App 满足宿主契约（依赖倒置的实现证明）。
var _ contract.Interaction = (*App)(nil)
