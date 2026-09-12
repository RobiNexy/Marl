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
	"fmt"

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
	lastCompressTokens int          // 上次压缩尝试时的上下文规模（防热循环）
	compressLog        []CompressEvent // 本次 Run 的压缩记录（可观测）

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
	return &Agent{
		id:               cfg.ID,
		depth:            cfg.Depth,
		sysPrompt:        cfg.SystemPrompt,
		maxRounds:        cfg.MaxRounds,
		sampling:         cfg.Sampling,
		thinking:         cfg.Thinking,
		maxContextTokens: cfg.MaxContextTokens,
		compress:         cfg.Compression,
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
//   - eventLoop 完成 → Idle，返回 nil；
//   - ctx 取消 → Running 直接退出，返回 ctx.Err()（调用方决定是否重启）；
//   - 轮数耗尽 / LLM 失败 / 工具基础设施故障 → 返回错误，状态落 Idle
//     （阶段 2 不做_blocked 状态；错误恢复属阶段 2 之外的"不做"清单）。
//
// View 在退出时写盘一次（Part 8.8 写入纪律的阶段 2 形态）。
func (a *Agent) Run(ctx context.Context) error {
	if a.state.Valid() && a.state != types.StateIdle {
		return fmt.Errorf("agent: Run on state %q (must be fresh instance or Idle)", a.state)
	}
	a.state = types.StateRunning
	err := a.eventLoop(ctx)
	a.state = types.StateIdle
	if saveErr := a.views.SaveView(ctx, a.view); saveErr != nil {
		// View 写失败不掩盖业务结论（它是投影的持久化，可重建）；
		// 但必须可见：与 err 合流而不是丢弃。
		if err == nil {
			err = fmt.Errorf("agent: save view: %w", saveErr)
		}
	}
	return err
}
