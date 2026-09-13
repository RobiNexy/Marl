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
	"github.com/RobiNexy/Marl/internal/discuss"
	"github.com/RobiNexy/Marl/internal/escalate"
	"github.com/RobiNexy/Marl/internal/fossil"
	"github.com/RobiNexy/Marl/internal/gate"
	"github.com/RobiNexy/Marl/internal/knowledge"
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
	if err := a.assemble(); err != nil {
		return nil, err
	}
	return a, nil
}

// assemble 的装配序（与 runStart 相同的顺序与语义）。
func (a *App) assemble() error {
	ctx := context.Background()
	a.Control = ControlRootOf(a.Root)

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

// RunStatus 是当前任务的运行态快照。
type RunStatus struct {
	Agent     types.AgentID `json:"agent,omitempty"`
	Task      string        `json:"task,omitempty"`
	StartedAt time.Time     `json:"started_at,omitempty"`
	Active    bool          `json:"active"`
	State     string        `json:"state,omitempty"`
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
func (a *App) CurrentRun() RunStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := RunStatus{Agent: a.runAgent, Task: a.runTask, StartedAt: a.runStartedAt}
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

// AgentView 是监督树的一行（GUI 的树节点）。
type AgentView struct {
	ID          string    `json:"id"`
	Parent      string    `json:"parent,omitempty"`
	Kind        string    `json:"kind"`
	Depth       int       `json:"depth"`
	State       string    `json:"state"`
	PendingKind string    `json:"pending_kind,omitempty"`
	PendingAt   time.Time `json:"pending_at,omitempty"`
	StartedAt   time.Time `json:"started_at"`
}

// Agents 返回全部 Actor 的快照（GUI 树的数据源）。
func (a *App) Agents() []AgentView {
	infos := a.spw.Snapshot()
	out := make([]AgentView, 0, len(infos))
	for _, p := range infos {
		out = append(out, AgentView{
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

// Say 给任意 Actor 发直接消息（GUI 的输入框）。
func (a *App) Say(to, text string) error {
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

// InboxItem 是收件箱的一行（GUI 的收件箱列表）。
type InboxItem struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
	Preview string    `json:"preview,omitempty"`
	ModTime time.Time `json:"mod_time"`
}

// Inbox 列出待处理收件（GUI 刷新用；done/ 的归档不在此列）。
func (a *App) Inbox() ([]InboxItem, error) {
	entries, err := os.ReadDir(filepath.Join(a.Control, "inbox"))
	if err != nil {
		return nil, err
	}
	out := []InboxItem{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		item := InboxItem{Name: e.Name(), Type: actor.FileTypeOf(e.Name())}
		if st, serr := e.Info(); serr == nil {
			item.ModTime = st.ModTime()
		}
		if data, rerr := os.ReadFile(filepath.Join(a.Control, "inbox", e.Name())); rerr == nil {
			if f, ok := actor.ParseInboxFileText(string(data)); ok {
				item.From, item.To = f.From, f.To
				item.Preview = previewOf(f.Body)
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// ReadInbox 返回收件文件全文（GUI 详情页）。
func (a *App) ReadInbox(name string) ([]byte, error) {
	if !safeInboxName(name) {
		return nil, fmt.Errorf("server: invalid inbox item name")
	}
	return os.ReadFile(filepath.Join(a.Control, "inbox", name))
}

// GateDecision 是人类对审批请求的答复（GUI 的批准/拒绝按钮）。
//
// 语义：把 @ 命令行写进收件箱文件——GUI 是"人类的笔"，通道不变
// （原则 4）；FileBackend 的 watcher 照常消费并路由回 Agent。
type GateDecision struct {
	Action string `json:"action"`           // allow | deny
	Mode   string `json:"mode,omitempty"`   // once | count | tokens | always
	Count  int    `json:"count,omitempty"`  // mode=count
	Tokens int64  `json:"tokens,omitempty"` // mode=tokens
	Reason string `json:"reason,omitempty"` // 批注
}

// ReplyGate 把裁决写进审批文件（id = 文件名里的 ulid 段）。
func (a *App) ReplyGate(id string, d GateDecision) error {
	line, lerr := gateDirectiveLine(d)
	if lerr != "" {
		return fmt.Errorf("server: %s", lerr)
	}
	name := "gate_" + id + ".md"
	if !safeInboxName(name) || id == "" {
		return fmt.Errorf("server: invalid gate id")
	}
	path := filepath.Join(a.Control, "inbox", name)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("server: approval %s not in inbox (already decided?)", id)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("\n" + line + "\n" + d.Reason + "\n"); err != nil {
		return err
	}
	return nil
}

// gateDirectiveLine 把 GUI 的结构化裁决折算成 @ 命令行（单一换算点）。
func gateDirectiveLine(d GateDecision) (string, string) {
	switch d.Action {
	case "deny":
		return "@deny", ""
	case "allow":
		switch d.Mode {
		case "", "once":
			return "@grant once", ""
		case "count":
			if d.Count <= 0 {
				return "", "count mode requires count > 0"
			}
			return fmt.Sprintf("@grant next %d", d.Count), ""
		case "tokens":
			if d.Tokens <= 0 {
				return "", "tokens mode requires tokens > 0"
			}
			return fmt.Sprintf("@grant tokens %d", d.Tokens), ""
		case "always":
			return "@always-grant", ""
		default:
			return "", "unknown grant mode " + d.Mode
		}
	default:
		return "", "action must be allow or deny"
	}
}

// DiscussionView 是一次讨论的概要（GUI 的讨论列表）。
type DiscussionView struct {
	ID      string    `json:"id"`
	Topic   string    `json:"topic"`
	Dir     string    `json:"dir"`
	ModTime time.Time `json:"mod_time"`
}

// Discussions 列出讨论（控制面 discussions/ 的目录扫描）。
func (a *App) Discussions() ([]DiscussionView, error) {
	base := filepath.Join(a.Control, "discussions")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return []DiscussionView{}, nil
		}
		return nil, err
	}
	out := []DiscussionView{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dv := DiscussionView{ID: e.Name(), Dir: filepath.Join(base, e.Name())}
		if st, serr := e.Info(); serr == nil {
			dv.ModTime = st.ModTime()
		}
		if data, rerr := os.ReadFile(filepath.Join(dv.Dir, "draft.md")); rerr == nil {
			// 草稿首行 = "# 草稿：讨论「topic」"——topic 的提取面。
			line := strings.SplitN(string(data), "\n", 2)[0]
			if i := strings.Index(line, "「"); i >= 0 {
				if j := strings.Index(line[i:], "」"); j > 0 {
					dv.Topic = line[i+3 : i+j]
				}
			}
		}
		out = append(out, dv)
	}
	return out, nil
}

// DiscussionFile 返回一次讨论的 verdict 文件路径（GUI 直接读）。
func (a *App) DiscussionFile(id string) (verdict, draft string, err error) {
	if !safeInboxName(id + ".x") {
		return "", "", fmt.Errorf("server: invalid discussion id")
	}
	dir := filepath.Join(a.Control, "discussions", id)
	return filepath.Join(dir, "verdict.md"), filepath.Join(dir, "draft.md"), nil
}

// ReplyDiscussion 把批注/裁决写进 verdict（GUI 的讨论回复框；approve
// 时写 @approve 行——与 CLI/文件通道同一条路径）。
func (a *App) ReplyDiscussion(id, annotation string, approve bool) error {
	verdict, _, err := a.DiscussionFile(id)
	if err != nil {
		return err
	}
	if _, serr := os.Stat(verdict); serr != nil {
		return fmt.Errorf("server: discussion %s not found", id)
	}
	f, err := os.OpenFile(verdict, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var sb strings.Builder
	sb.WriteString("\n")
	if annotation != "" {
		sb.WriteString(annotation + "\n")
	}
	if approve {
		sb.WriteString("@approve\n")
	}
	_, err = f.WriteString(sb.String())
	return err
}

// ---------------------------------------------------------------------------
// 配置与知识（GUI 的设置页）
// ---------------------------------------------------------------------------

// ConfigRaw 返回 config.yaml 原文（GUI 编辑器的内容源）。
func (a *App) ConfigRaw() ([]byte, error) {
	return os.ReadFile(filepath.Join(a.Root, ".marl", "config.yaml"))
}

// WriteConfig 校验并写回 config.yaml（先 Parse+ParseLimits+ParseGateRules
// + ValidateRules——坏配置在保存时被拦，不等到下次启动）。
func (a *App) WriteConfig(raw []byte) error {
	node, err := config.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid yaml: %w", err)
	}
	limits, err := config.ParseLimits(node)
	if err != nil {
		return fmt.Errorf("invalid limits: %w", err)
	}
	rules, err := config.ParseGateRules(node)
	if err != nil {
		return fmt.Errorf("invalid gate_rules: %w", err)
	}
	probe, err := gate.NewManager(gate.ManagerConfig{Rules: rules, LLMLimits: &gate.LLMLimits{
		MaxCalls: limits.CallTaskMax, MaxTokens: int64(limits.CallTaskMaxTokens)}})
	if err != nil {
		return fmt.Errorf("invalid rules: %w", err)
	}
	_ = probe
	return os.WriteFile(filepath.Join(a.Root, ".marl", "config.yaml"), raw, 0o644)
}

// Profiles 返回全部 Profile 摘要（GUI 的角色列表）。
func (a *App) Profiles() ([]types.ProfileSummary, error) {
	return a.prof.List(), nil
}

// ProfileRaw 返回一份 profile 文件原文。
func (a *App) ProfileRaw(id string) ([]byte, error) {
	if !safeInboxName(id + ".yaml") {
		return nil, fmt.Errorf("server: invalid profile id")
	}
	return os.ReadFile(filepath.Join(a.Root, ".marl", "profiles", id+".yaml"))
}

// WriteProfile 校验并写回 profile（写后 Reload——下个任务生效）。
func (a *App) WriteProfile(id string, raw []byte) error {
	if !safeInboxName(id + ".yaml") {
		return fmt.Errorf("server: invalid profile id")
	}
	path := filepath.Join(a.Root, ".marl", "profiles", id+".yaml")
	tmp := path + ".tmp"
	if werr := os.WriteFile(tmp, raw, 0o644); werr != nil {
		return werr
	}
	// 校验：临时文件参与一次完整加载。
	probe := profile.NewLoader()
	if lerr := probe.LoadAll(filepath.Dir(tmp)); lerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("invalid profile: %w", lerr)
	}
	if _, gerr := probe.Get(types.ProfileID(strings.TrimSuffix(id, ".yaml"))); gerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("invalid profile: %w", gerr)
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return rerr
	}
	_, rerr := a.prof.Reload()
	return rerr
}

// KnowledgeLint 的结果透传（GUI 的知识库健康面）。
func (a *App) KnowledgeLint() (string, error) {
	prefDir := filepath.Join(a.Root, ".marl", "knowledge", "preferences")
	block, err := knowledge.CompileStandingOrders(prefDir)
	if err != nil {
		return "", err
	}
	return block.Report(knowledge.MaxStandingTokens), nil
}

// KnowledgePromote / KnowledgePull 是 vendor 面（fossil；GUI 的知识库
// 管理按钮）。
func (a *App) KnowledgePromote(ctx context.Context, relPath, globalRepo string) (string, error) {
	cli, err := fossil.NewCLI("")
	if err != nil {
		return "", err
	}
	marlDir := filepath.Join(a.Root, ".marl")
	return knowledge.Promote(ctx, cli, marlDir, globalRepo, relPath)
}

// KnowledgePull 把全局库拉进 vendor/（返回条目描述）。
func (a *App) KnowledgePull(ctx context.Context, globalRepo string) ([]string, error) {
	cli, err := fossil.NewCLI("")
	if err != nil {
		return nil, err
	}
	marlDir := filepath.Join(a.Root, ".marl")
	entries, err := knowledge.Pull(ctx, cli, marlDir, globalRepo)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out, nil
}

// Doctor 跑环境自检（GUI 的诊断页）。
func (a *App) Doctor() []CheckResult { return RunChecks(a.Root) }

// DoctorOn 对任意目录跑自检（NewApp 之前就能用——"这个目录能不能跑起来"
// 是先于装配的问题）。
func DoctorOn(root string) []CheckResult { return RunChecks(root) }

// Close 释放资源（store 等；进程退出前的收尾点）。
func (a *App) Close() { a.st.Close() }

// ---- 小件（类型化静默窗/文件名守卫/预览）----

// safeInboxName 拒绝路径穿越（GUI 的输入是外部的——比 CLI 的自查多一层）。
func safeInboxName(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\") ||
		strings.Contains(name, "..") || strings.HasPrefix(name, ".") {
		return false
	}
	return true
}

// previewOf 取正文首行做预览（收件箱列表）。
func previewOf(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t != "" && !strings.HasPrefix(t, "#") {
			r := []rune(t)
			if len(r) > 80 {
				return string(r[:80]) + "…"
			}
			return t
		}
	}
	return ""
}

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
