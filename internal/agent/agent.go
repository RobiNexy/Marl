// Package agent 实现单 Agent 的最小循环（设计文档 13.4，阶段 2）。
//
// 主循环骨架对齐 Part 8.8 eventLoop + Part 10.16 WireTurn 语义：
//
//	eventLoop:
//	  compileView() → CanonicalRequest
//	  turn := LLM.ExecuteTurn()   （多 Outcome 序列：reasoning / tool_call / reply）
//	  逐 Outcome 落 Log（同一 turn 的 Reasoning 共享一个 LogEntry 序列，约束 1）
//	  顺序执行工具调用（约束 2：不并行——后续调用常依赖前序结果）
//	  结果 AppendToolResults 续到下一次 Execute（实际形态：更新 View，下一轮整体编译）
//	  turn Ready 且无工具调用 → 结束
//
// 阶段 2 的边界（13.4 "不做"）：Mailbox 路由 / Profile / 持久化 Agent 状态 /
// 错误恢复（逐 Outcome 落 Log 的可恢复物料已经足够，差量续发留给阶段 3+）。
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"marl/internal/discuss"
	"marl/internal/ladder"
	"marl/internal/ledger"
	"marl/internal/proto"
	"marl/internal/skill"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
)

// LLMExecutor 是主循环对 Wire 层的窄接口（消费侧定义）。
//
// 输入是 CanonicalRequest（不是 WireRequest）：归一化（Normalizer）在线路侧，
// Agent 只负责"上下文语义 → 协议无关规范形态"这一步编译（Part 3.4 的边界）。
// 输出是 WireTurn（Part 10.16 的承载结构）。
//
// 失败语义：网络/装配错误走 error（阶段 2 没有重试策略层，直接上抛）；
// 厂商错误不进 error——它们已分类进 Outcome.Signals.ErrorClass（ADR-0016），
// 由 handleTurn 按类别分流。
type LLMExecutor interface {
	ExecuteTurn(ctx context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error)
}

// Config 是 agent.New 的装配参数。
//
// 全部字段必须显式填充——Agent 的字段零值**不是**可用状态（types.AgentState
// 的零值契约：零值非法且不是 Idle），本包不做任何隐式默认值的搬运。
// 需要默认值的地方由构造方（cmd/mini）显式写出来。
type Config struct {
	ID    types.AgentID
	Depth int // fork 深度（mini 固定 0；进 SkillEnv 供元信息与队列纪律）
	// SystemPrompt 是冻结前缀的 system 段（Part 3.4 L1）。byte-stable 由
	// 构造方保证（mini 用常量字符串）。
	SystemPrompt string
	// MaxRounds 是主循环轮数上限（烧预算的保险丝；mini 默认 8）。必须 > 0。
	MaxRounds int

	// Log 是消息真相之源（Part 3.2）；Views 是 View 的持久化投影容器
	// （Part 3.3，"进入/退出 Blocked 时写"的时机在 mini 简化为 Run 结束时写）。
	Log   store.MessageLog
	Views store.ViewStore

	// LLM 是 Wire 层执行器；Skills 是技能注册表；AllowedSkills 为 nil 时
	// 全部允许（Part 6.11：空 = 不限制）。
	LLM           LLMExecutor
	Skills        skill.Registry
	AllowedSkills []string

	// Namespace / Resolver 构造 SkillEnv；Resolver 与 ProjectRoot 由主装配
	// 注入（技能不得自建环境，Skill 契约）。
	Namespace   *types.Namespace
	Resolver    types.Resolver
	ProjectRoot string
	// Snapshots 是 file_write 的写前快照通道（可 nil，见 SkillEnv 契约）。
	Snapshots skill.Snapshotter

	// Sampling 是本轮任务的采样参数（mini 固定值）。
	Sampling types.SamplingParams
	// Thinking 进 CanonicalRequest.Thinking（10.7 落地缺口 1 的修复点：
	// Normalizer 只读 req.Thinking，档位必须由编译侧显式带上）。
	Thinking types.ThinkingSpec

	// MaxContextTokens 是当前绑定模型的有效上下文窗口（headroom 的
	// model_max_tokens 项）。0 = 压缩禁用。阶段 3 由构造方显式给出
	// （mini 用 -ctx-budget 演示口径）；真实能力表（caps.MaxContext）
	// 在阶段 4+ 接管。
	MaxContextTokens int
	// Compression 非 nil 即启用压缩（headroom 触发，Part 3.7）。
	Compression *CompressConfig

	// ---- 阶段 4：账本与升级 ----
	// TaskID 是本 Agent 所属任务（Ledger 记账与审计的归属键）。
	TaskID types.TaskID
	// Ledger 非 nil 时每次主调用后记账（用量未知则跳过，见 ledger 契约）。
	Ledger *ledger.Recorder
	// Audit 非 nil 时记审计事件（model_upgrade / spawn / report_check 等）。
	Audit store.AuditStore
	// Upgrader 非 nil 即启用阶梯升级（Part 7.3：每轮结束后检查证据）。
	Upgrader *UpgradeConfig

	// ---- 阶段 5：拓扑与消息 ----
	// ParentID 非空表示本 Agent 是子（report_to_parent 有去处）。
	ParentID types.AgentID
	// MaxDepth 是 fork 深度上限（SkillEnv 的元信息；硬闸在 Spawner）。
	// 0 = 未设（SkillEnv.MaxDepth 回落为 Depth，阶段 2 行为）。
	MaxDepth int
	// Mailbox 是本 Agent 的信箱读端（Part 8.7；nil = 无消息面）。
	Mailbox <-chan proto.Envelope
	// Spawner 是 spawn_subagent 意图的裁决关口（proto.Spawner；nil 时该
	// 意图调用返回失败——"没有关口等于没有权限"）。
	Spawner proto.Spawner
	// ReportSink 是 report_to_parent 的投递通道（nil = 根 Agent，无父可报）。
	ReportSink ReportSink
	// ReportChecker 是 report 的机械检查（原则 4；ReportSink 非 nil 时必须
	// 提供——没有检查器的 report 等于无条件采信自述）。
	ReportChecker proto.ReportChecker

	// ---- 阶段 6：单写者提交 ----
	// Committer 非 nil 即启用（只有父 Agent 装配它——单写者纪律的结构
	// 保证：提交点在父的唯一代码路径上）。
	Committer *CommitConfig

	// ---- 阶段 7：Profile 与知识库 ----
	// StandingOrders 是 preferences/ 的编译产物（knowledge.CompileStandingOrders；
	// 空串 = 无常驻块）。byte-stability 由编译器归一化 + golden test 保证，
	// 本包只搬运（compileView 的 SegStanding 段，frozen 前缀的一部分）。
	StandingOrders string
	// TaskDescription 是任务的私有段文本（Part 6.10：fork 时的不可变输入；
	// 空串表示无私有任务段）。
	TaskDescription string

	// ---- 阶段 8：人机协作（讨论） ----
	// Discussion 非 nil 即启用（装配缺失时 request_discussion 回填
	// INTENT_NOT_HANDLED——如实拒绝而不是装死）。
	Discussion *DiscussionConfig
}

// Agent 是一个单任务的执行体（Part 8.1，阶段 2 无 Mailbox/父子拓扑）。
//
// 生命周期：New →（手写 View）→ Run；Run 期间由本方法调用栈单写自身状态
// （types.AgentState 的并发契约），结束后复用要求重建实例（Crashed 是终态）。
//
// 零值契约：Agent{} 不可用（state 非法、依赖 nil），必须经 New 构造。
type Agent struct {
	id    types.AgentID
	depth int

	state types.AgentState
	view  *types.ContextView

	binding types.Binding

	sysPrompt  string
	maxRounds  int
	sampling   types.SamplingParams
	thinking   types.ThinkingSpec
	bindingSet bool

	// 压缩（阶段 3）：maxContextTokens <= 0 表示禁用；compress 为 nil 同。
	maxContextTokens   int
	compress           *CompressConfig
	lastCompressTokens int             // 上次压缩尝试时的上下文规模（防热循环）
	compressLog        []CompressEvent // 本次 Run 的压缩记录（可观测）

	// 阶段 4：账本 / 审计 / 升级。
	taskID  types.TaskID
	ledger  *ledger.Recorder
	audit   store.AuditStore
	upgrade *UpgradeConfig
	acc     *ladder.Accumulator // 证据累积器（upgrade 启用时非 nil）
	// 每轮信号（handleTurn 填写，eventLoop 消费后清零）。
	roundFormatErrors int
	roundProgress     bool
	roundErrorClass   wire.ErrorClass
	// 逐轮用量累计（换模型审计的真实统计源）。
	cacheHits int64
	cacheMiss int64
	// Transient（Part 3.6）：尾部 volatile 提示，不入 Log，下一轮编译后丢弃。
	transients []string

	// 阶段 6：单写者提交（只有父装配）。
	commit *CommitConfig

	// 阶段 7：常驻块与私有段（frozen 前缀的构成件；只在构造时赋值一次）。
	standingOrders string
	taskDesc       string

	// 阶段 8：讨论（会话由 Agent 持有——Session 承载 nonce/轮次的运行态）。
	discussCfg     *DiscussionConfig
	discussSess    *discuss.Session
	discussPending bool

	// 阶段 5：拓扑与消息。
	parentID      types.AgentID
	maxDepth      int
	mailbox       <-chan proto.Envelope
	spawner       proto.Spawner
	reporter      ReportSink
	reportChecker proto.ReportChecker
	// 子状态（pump 与主 goroutine 并发访问，mu 保护）。
	mu              sync.Mutex
	pendingChildren map[types.AgentID]bool
	childrenStatus  map[types.AgentID]types.ChildStatus
	childReports    []*proto.ChildReport
	reportsDone     chan struct{}
	// 本 Agent 的任务终态信号（子：report 已投递）。
	reported bool
	// 成功写入的文件（机械检查的数据源；file_write 成功时记录）。
	writtenFiles map[string]bool
	// 最近一轮子 report 快照（commitMessage 用；flush 时记录）。
	lastReports []*proto.ChildReport

	blockReason types.BlockReason // 与 state 的双向约束见 types.BlockReason

	nextPosition float64 // Fractional Index 步进器：只追加 → +1 即可，无需中点

	log        store.MessageLog
	views      store.ViewStore
	llm        LLMExecutor
	skills     skill.Registry
	authorizer skill.Authorizer
	env        *skill.SkillEnv
}

// New 校验配置并构造 Agent。
//
// 失败：Config 的任何关键依赖为 nil / MaxRounds 与 ID 非法 → 错误
// （启动期 fail fast：装配错误离真正的故障现场最远，必须在 Run 之前拦住）。
func New(cfg Config) (*Agent, error) {
	switch {
	case cfg.ID == "":
		return nil, fmt.Errorf("agent: ID is required")
	case cfg.Log == nil || cfg.Views == nil:
		return nil, fmt.Errorf("agent: Log/Views stores are required")
	case cfg.LLM == nil:
		return nil, fmt.Errorf("agent: LLM executor is required")
	case cfg.Skills == nil:
		return nil, fmt.Errorf("agent: skill registry is required")
	case cfg.MaxRounds <= 0:
		return nil, fmt.Errorf("agent: MaxRounds must be positive")
	case cfg.Namespace == nil || cfg.Resolver == nil:
		return nil, fmt.Errorf("agent: Namespace and Resolver are required")
	case cfg.SystemPrompt == "":
		return nil, fmt.Errorf("agent: SystemPrompt is required (frozen prefix)")
	}
	if cfg.Compression != nil {
		// 压缩装配的启动期校验（fail fast，与其它依赖同一纪律）。
		switch {
		case cfg.Compression.Compressor == nil:
			return nil, fmt.Errorf("agent: Compression.Compressor is required")
		case cfg.MaxContextTokens <= 0:
			return nil, fmt.Errorf("agent: MaxContextTokens must be positive when compression is enabled")
		case cfg.Compression.BudgetReserved < 0:
			return nil, fmt.Errorf("agent: Compression.BudgetReserved must be >= 0")
		}
		if err := cfg.Compression.Policy.Validate(); err != nil {
			return nil, fmt.Errorf("agent: compression policy: %w", err)
		}
	}
	// 阶段 4：升级装配校验。
	var acc *ladder.Accumulator
	if cfg.Upgrader != nil {
		switch {
		case cfg.Upgrader.Router == nil:
			return nil, fmt.Errorf("agent: Upgrader.Router is required")
		case cfg.Upgrader.Ladder == nil:
			return nil, fmt.Errorf("agent: Upgrader.Ladder is required")
		case cfg.Upgrader.Catalog == nil:
			return nil, fmt.Errorf("agent: Upgrader.Catalog is required")
		}
		var err error
		acc, err = cfg.Upgrader.newAccumulator()
		if err != nil {
			return nil, fmt.Errorf("agent: upgrade policy: %w", err)
		}
	}
	// 阶段 5 装配校验：report 通道与机械检查必须成对出现（原则 4）。
	if cfg.ReportSink != nil && cfg.ReportChecker == nil {
		return nil, fmt.Errorf("agent: ReportChecker is required when ReportSink is set (un-checked reports violate principle 4)")
	}
	return &Agent{
		id:               cfg.ID,
		depth:            cfg.Depth,
		sysPrompt:        cfg.SystemPrompt,
		maxRounds:        cfg.MaxRounds,
		sampling:         cfg.Sampling,
		thinking:         cfg.Thinking,
		maxContextTokens: cfg.MaxContextTokens,
		compress:         cfg.Compression,
		taskID:           cfg.TaskID,
		ledger:           cfg.Ledger,
		audit:            cfg.Audit,
		upgrade:          cfg.Upgrader,
		acc:              acc,
		parentID:         cfg.ParentID,
		maxDepth:         cfg.MaxDepth,
		mailbox:          cfg.Mailbox,
		spawner:          cfg.Spawner,
		reporter:         cfg.ReportSink,
		reportChecker:    cfg.ReportChecker,
		commit:           cfg.Committer,
		standingOrders:   cfg.StandingOrders,
		taskDesc:         cfg.TaskDescription,
		discussCfg:       cfg.Discussion,
		pendingChildren:  map[types.AgentID]bool{},
		childrenStatus:   map[types.AgentID]types.ChildStatus{},
		reportsDone:      make(chan struct{}, 1),
		writtenFiles:     map[string]bool{},
		nextPosition:     1.0,
		log:          cfg.Log,
		views:        cfg.Views,
		llm:          cfg.LLM,
		skills:       cfg.Skills,
		authorizer:   skill.NewAuthorizer(cfg.Skills, cfg.AllowedSkills),
		env: &skill.SkillEnv{
			AgentID:     cfg.ID,
			Namespace:   cfg.Namespace,
			Resolver:    cfg.Resolver,
			Depth:       cfg.Depth,
			MaxDepth:    cfg.Depth,
			WorkDir:     cfg.ProjectRoot,
			ProjectRoot: cfg.ProjectRoot,
			Snapshots:   cfg.Snapshots,
		},
		view: &types.ContextView{AgentID: cfg.ID},
	}, nil
}

// SetBinding 记录任务绑定（阶段 2 无 Router，mini 手工装配——与 Binding
// 契约一致：CachePrefix 必须经 wire.ModelCachePrefix 派生，不由本方法重算）。
//
// Binding 在 13.4 里只参与 CanonicalRequest.Thinking 的注入与审计显示；
// 复用约束（换模型 = 缓存键空间切换）由阶段 4 的 Router 承接。
func (a *Agent) SetBinding(b types.Binding) error {
	if b.Model == "" || b.Endpoint == "" {
		return fmt.Errorf("agent: binding must name a model and endpoint")
	}
	if b.CacheBucket != a.id {
		return fmt.Errorf("agent: binding cache bucket %q is not this agent's id %q (Patch 1)", b.CacheBucket, a.id)
	}
	a.binding = b
	a.bindingSet = true
	return nil
}

// ID 返回 Agent 的标识（审计与 Log 归属）。
func (a *Agent) ID() types.AgentID { return a.id }

// State 返回当前状态（进程表/Watchdog 只读）。
func (a *Agent) State() types.AgentState { return a.state }

// View 返回工作台的当前快照（调用方只读；单写者纪律见 types.ContextView）。
func (a *Agent) View() *types.ContextView { return a.view }

// AppendUser 追加一条 user 消息（Log + View 同步追加）。
//
// 这是阶段 2 的"手写初始 View"入口：任务的启动消息必须先于 Run 调用。
// Audience=Both：用户输入既进上下文也进审计表。
func (a *Agent) AppendUser(ctx context.Context, text string) error {
	e := types.NewLogEntry(a.id, types.RoleUserInput, text)
	if _, err := a.log.Append(ctx, e); err != nil {
		return fmt.Errorf("agent: append user: %w", err)
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	return nil
}

// AppendInjected 追加一条父注入的消息（Part 9.5 的注入段；Spawner 的
// ChildFactory 在建子时调用，先于 AppendUser）。
//
// 前置条件：e.Prov == ProvInjected 且 SourceIDs 指向父 Log 的条目（血缘是
// 注入的唯一凭据——调用方负责构造，本方法校验并拒绝裸的 original 条目）。
func (a *Agent) AppendInjected(ctx context.Context, e *types.LogEntry) error {
	if e.Prov != types.ProvInjected || len(e.SourceIDs) == 0 {
		return fmt.Errorf("agent: AppendInjected requires Prov=Injected with SourceIDs (lineage is the only credential of an injected message)")
	}
	e.AgentID = a.id
	if _, err := a.log.Append(ctx, e); err != nil {
		return fmt.Errorf("agent: append injected: %w", err)
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	return nil
}

// addToView 追加 ViewItem（Position 步进）。稳定性档次的声明在这里显式：
// 所有经 Log 驱动的条目都是 stable——frozen 由编译层按 SegmentKind 授予，
// volatile 只属于不入 Log 的 Transient（Part 3.3 的边界，阶段 2 无 Transient）。
func (a *Agent) addToView(role types.WireRole, ref types.MessageID) {
	if !role.Valid() {
		role = types.WireUser // 编译映射表之外的角色按 user 呈现（Part 3.4 映射表的兜底）
	}
	a.view.Items = append(a.view.Items, types.ViewItem{
		Ref:       ref,
		WireRole:  role,
		Stability: types.StabilityStable,
		Visible:   true,
		Pinned:    false,
		Position:  a.nextPosition,
	})
	a.nextPosition++
}

// Run 运行主循环把状态机从 Idle 推到 Running，任务结束回 Idle。
//
// 退出路径（全部显式）：
//   - eventLoop 完成（含子 Agent 的 report 已投递）→ Idle，返回 nil；
//   - fork 后等待子 report（errWaitChildren）→ Blocked(WaitChildren)，
//     全部子归位后恢复 Running 继续循环（Part 8.3）；
//   - ctx 取消 → 当前状态直接退出，返回 ctx.Err()（调用方决定是否重启）；
//   - 轮数耗尽 / LLM 失败 / 工具基础设施故障 → 返回错误，状态落 Idle
//     （错误恢复属后续阶段的"不做"清单）。
//
// View 在退出时写盘一次（Part 8.8 写入纪律的阶段 2 形态）。
// Mailbox 泵在进入 Running 时启动（Part 8.8：LLM 调用期间 mailbox 继续
// 累积，人类的插话在下一轮编排时被看到）。
func (a *Agent) Run(ctx context.Context) error {
	if a.state.Valid() && a.state != types.StateIdle {
		return fmt.Errorf("agent: Run on state %q (must be fresh instance or Idle)", a.state)
	}
	a.state = types.StateRunning
	a.auditState(ctx, "running", "")
	a.startPump(ctx)
	var err error
	for {
		err = a.eventLoop(ctx)
		if err == nil {
			break // 任务完成（或子已 report）
		}
		if errors.Is(err, errWaitChildren) {
			if werr := a.awaitChildren(ctx); werr != nil {
				err = werr
				break
			}
			// 单写者提交（Part 8.4）：全部子 report 已落库、父已恢复——
			// 这是唯一的提交点。失败上抛（提交失败必须可见）。
			if cerr := a.doCommit(ctx); cerr != nil {
				err = cerr
				break
			}
			continue // 子结果已落 Log+View，下一轮编排自然看到
		}
		if errors.Is(err, errDiscussing) {
			// Blocked(Discussing)（Part 8.2 的讨论等待；等待期间不烧钱——
			// 见 awaitDiscussion 的注释）。恢复后继续 loop：批注的响应在
			// 下一轮编排里（annotation 入 Log 后 LLM 会看到它）。
			if werr := a.awaitDiscussion(ctx); werr != nil {
				err = werr
				break
			}
			continue
		}
		break // 其它错误原样上抛
	}
	a.state = types.StateIdle
	a.auditState(ctx, "idle", "")
	if saveErr := a.views.SaveView(ctx, a.view); saveErr != nil {
		// View 写失败不掩盖业务结论（它是投影的持久化，可重建）；
		// 但必须可见：与 err 合流而不是丢弃。
		if err == nil {
			err = fmt.Errorf("agent: save view: %w", saveErr)
		}
	}
	return err
}
