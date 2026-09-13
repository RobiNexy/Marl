package spawner

// Spawner：进程表、裁决、生命周期（Part 8.1 / 9.1 / 9.2 / 9.7；
// Part 14 的统一 Actor 模型——人类是进程表的根，Agent 特例删除）。

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/RobiNexy/Marl/internal/actor"
	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// ChildPlan 是批准后交给 ChildFactory 的建子计划（裁决的全部产出 +
// 注入消息的已取条目——取消息需要 Log 访问，归 Spawner，工厂不必再查）。
type ChildPlan struct {
	ID       types.AgentID
	Depth    int
	ParentID types.AgentID
	// Namespace 是 buildNamespace 的产出（已通过 Subset 校验）。
	Namespace *types.Namespace
	// Injected 是 InjectMessages 指向的条目（按 Seq 升序；Prov=Injected 的
	// 落库由工厂执行）。
	Injected []*types.LogEntry
	// Task 是 req.TaskDescription 的引用（孙脚本需要知道自己的任务参数
	// ——例如要写的文件名由父写进任务描述时的多层测试场景）。
	Task string
	// Mailbox 是子的信箱读端（多层拓扑里孙子的 report 要路由到这个
	// 信箱——工厂必须把它装配进子 Agent 的 Config，否则父级链在
	// "孙 report"这一步断掉且静默：报告投递超时只是子 end 端的一个
	// "REPORT_UNDELIVERED"，现象是父永远等不到孙的 report）。
	Mailbox <-chan proto.Envelope
}

// ChildRunner 是一个已构建、可运行的子 Agent（工厂的产出）。
//
// 解耦点：Spawner 不 import agent 包——它只知道"有个东西能 Run"。
// 生命周期钩子（report 登记/框架代报）由 Spawner 在 Run 外层包装。
type ChildRunner interface {
	Run(ctx context.Context) error
}

// ChildFactory 由装配层实现：按计划构建子 Agent（不启动——启动归 Spawner，
// 这样"登记 → 启动"的顺序由关口保证，不会出现"跑起来了但进程表不知道"）。
type ChildFactory interface {
	BuildChild(ctx context.Context, plan *ChildPlan, req *proto.SpawnRequest) (ChildRunner, error)
}

// Config 是 Spawner 的装配参数。
//
// 零值契约：零值不可用（三道闸的零值都是"最激进"：MaxDepth=0 连根都
// 不能 fork、MaxActive=0 拒绝一切、MaxForkRounds=0 同理——但"拒绝一切"
// 与"忘记配置"不可区分，因此全部要求显式填充，见 Validate）。
type Config struct {
	// MaxDepth 是 **AI→AI fork** 的深度上限（Part 14.6 的语义修订：
	// max_depth 只约束 AI 递归层数，人类 spawn 项目 Agent 不占额度——
	// 人类的 spawn 走 CapSet.CanSpawn，不进深度闸）。
	MaxDepth int
	// MaxActive 是全局活跃 Actor 上限（Part 9.2 的全局资源管控）。
	MaxActive int
	// MaxForkRounds 是同一任务内每请求者的 fork 轮数上限（Part 9.7 闸 2）。
	MaxForkRounds int
	// CanSpawnAtDepth 报告某深度的 Agent 是否允许 fork（Part 9.2 的
	// Profile.CanSpawn；只对 AI 请求者生效——人类的能力来自 CapSet）。
	CanSpawnAtDepth func(depth int) bool
	// Log 用于 InjectMessages 的越界校验与取条目（Part 9.2：越界的 Seq
	// 必须被拒绝而不是跳过）。
	Log store.MessageLog
	// Audit 非 nil 时记 spawn 裁决审计（原则 1 的副产品）。
	Audit store.AuditStore
	// Factory 构建子 Agent。
	Factory ChildFactory
}

// Validate 报告配置是否可用。
func (c *Config) Validate() error {
	switch {
	case c.MaxDepth <= 0:
		return fmt.Errorf("spawner: MaxDepth must be > 0 (0 would forbid every fork including the root's)")
	case c.MaxActive <= 0:
		return fmt.Errorf("spawner: MaxActive must be > 0")
	case c.MaxForkRounds <= 0:
		return fmt.Errorf("spawner: MaxForkRounds must be > 0")
	case c.CanSpawnAtDepth == nil:
		return fmt.Errorf("spawner: CanSpawnAtDepth is required")
	case c.Log == nil:
		return fmt.Errorf("spawner: Log is required (inject seq validation)")
	}
	// Factory 不在启动期强制：它需要引用本 Spawner（作为子的 ReportSink），
	// 存在构造顺序依赖，由 SetFactory 后置注入、Adjudicate 时校验。
	return nil
}

// Process 是进程表的一行——统一 Actor 模型下的实体视图（Part 8.1 / 14.2）。
//
// 人类与 Agent 同表：human 行由 RegisterHuman 登记（caps 全量、文件
// 后端）；agent 行由 Adjudicate 创建（caps 由 Profile 派生、channel
// 后端）。**权威判断只读 Caps**（纪律 1）；AIRecursion 是拓扑记账——
// max_depth 只约束 AI→AI fork（Part 14.6），它在注册时由 Kind 映射一次，
// 裁决期读的是进程表事实（"顶端是拓扑事实"），不是 Kind 分支。
type Process struct {
	ID    types.AgentID
	Depth int
	State types.AgentState // Spawner 视角的生命周期状态
	// 权威与传输（统一 Actor 面）。
	Caps    actor.CapSet
	Backend actor.MailboxBackend
	// AIRecursion 是拓扑记账：本行是否参与 AI 递归（深度闸只对它生效）。
	// 注册点置定（人类 = false），裁决期只读——不是权限分支（权限在
	// Caps.CanSpawn），是 Part 14.6 的深度语义。
	AIRecursion bool
	// Runtime（框架注入，不持久化，不进 Log）。
	ParentID   types.AgentID
	Children   map[types.AgentID]types.ChildStatus
	ForkRounds int // 本任务内已 fork 的轮数（扇出闸 2）
	// reported 由 Spawner 置位（子通过 ReportToParent 投递时）——
	// 框架代报的判据。
	reported bool
	// Writable 是子命名空间的 write 挂载（框架代报的机械检查范围）。
	Writable []string

	// ---- 阶段 9：Watchdog 的进程表扩展 ----
	// StartedAt 是子 goroutine 的启动时刻（超时判据）。
	StartedAt time.Time
	// cancel 是子 Run 的取消点（Terminate 的机制面）。只在 Adjudicate /
	// runChild 的包装里赋值一次；nil = 尚未启动。
	cancel          context.CancelFunc // chassis 注入
	terminateReason string             // 已被 Watchdog 终止的原因（代报文本的一部分）

	// ---- 阶段 12：挂起登记（Part 14.8——Watchdog 的停摆告警数据源）。
	// kind ∈ discussing / awaiting_gate（escalation 无超时不登记）。
	pendingKind string
	pendingAt   time.Time
}

// Spawner 是框架级单例（Part 9.1）。
//
// 并发：进程表的全部访问经 mu；Adjudicate 与子生命周期回调可并发。
type Spawner struct {
	mu    sync.Mutex
	procs map[types.AgentID]*Process
	// nsOf 记录每个进程的命名空间（Subset 校验需要完整父命名空间；
	// Process 本体不存——它保持 Part 8.1 的"扁平进程表"形态）。人类的
	// 命名空间 = nil（全量——子集语义由"请求即授权"的基准缺失表达）。
	nsOf   map[types.AgentID]*types.Namespace
	nextID int // 全局计数器（Part 9.1：拓扑一致性由一处维护）
	cfg    Config
}

// New 构造 Spawner。
//
// 失败：配置校验不过（启动期 fail fast）。
func New(cfg Config) (*Spawner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Spawner{
		procs: map[types.AgentID]*Process{},
		nsOf:  map[types.AgentID]*types.Namespace{},
		cfg:   cfg,
	}, nil
}

// RegisterHuman 把人类 Actor 登记进进程表（Part 14.2 #2：监督树的根，
// depth=0，caps 全量，文件后端）。
//
// 这是旧 Bootstrap 特例的消除点：人类行与 Agent 行同构（ caps/Backend/
// 深度），项目 Agent 的创建从此走正常 Adjudicate（requester = 人类）。
//
// 失败：id 非 human: 形态 / 重复注册 / human Actor 契约不符。
func (s *Spawner) RegisterHuman(ctx context.Context, h actor.Actor) error {
	id := h.ID()
	if !actor.IsHumanID(id) {
		return fmt.Errorf("spawner: %q is not a human actor id (%q prefix required)", id, actor.HumanPrefix)
	}
	caps := h.Capabilities()
	if caps.Kind != actor.HumanKind {
		return fmt.Errorf("spawner: human %s caps.Kind must be HumanKind", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.procs[id]; dup {
		return fmt.Errorf("spawner: %s already registered", id)
	}
	d, ok := h.(actor.Deliverer)
	if !ok {
		return fmt.Errorf("spawner: human %s must implement actor.Deliverer (route target)", id)
	}
	s.procs[id] = &Process{
		ID:    id,
		Depth: 0, // 监督树根（Part 14.6 拓扑图）；深度闸对人类不生效
		State: types.StateIdle,
		Caps:  caps, // 全量：CanSpawn / CanMessage / OwnsControlPlane
		// 人类不参与 AI 递归（Part 14.13）。
		AIRecursion: false,
		// 传输：收件箱的投递/读端（文件后端或替身的 channel）。
		Backend:  actor.BackendOf(d.Deliver, h.Mailbox()),
		Children: map[types.AgentID]types.ChildStatus{},
	}
	s.nsOf[id] = nil // 全量基准：buildNamespace 的"请求即授权"路径
	s.audit(ctx, id, "actor_registered", string(id), map[string]any{
		"actor": string(id), "depth": 0, "kind": string(caps.Kind),
	})
	return nil
}

// RegisterAgent 把装配层自管的一个 Agent 行登记进进程表（统一 Actor 模型
// 的装配面：宿主进程自持生命周期的 Agent——交付检查命令的脚本化根、
// 最小装配的直连形态——也要有进程表身份：report/escalation/SendTo 的
// 路由目标 + Watchdog 的观测面）。
//
// 它**不是创建路径**：拓扑内的子 Agent 一律走 Adjudicate（Part 14.6 的
// "bootstrap 特例删除"指创建路径；本方法不创建、不启动、不裁决——只把
// 已存在事实登记为路由/观测行，语义同 RegisterHuman 的人类行）。
//
// 失败：id 是 human: 形态（人类走 RegisterHuman）/ 重复 / backend nil。
func (s *Spawner) RegisterAgent(ctx context.Context, id types.AgentID, depth int, ns *types.Namespace, caps actor.CapSet, backend actor.MailboxBackend) error {
	switch {
	case actor.IsHumanID(id):
		return fmt.Errorf("spawner: %s 是人类形态的 id（人类行走 RegisterHuman）", id)
	case backend == nil:
		return fmt.Errorf("spawner: agent %s requires a mailbox backend", id)
	case caps.Kind != actor.AgentKind:
		return fmt.Errorf("spawner: agent %s caps.Kind must be AgentKind", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.procs[id]; dup {
		return fmt.Errorf("spawner: %s already registered", id)
	}
	s.procs[id] = &Process{
		ID:    id,
		Depth: depth,
		State: types.StateIdle,
		Caps:  caps,
		// 装配自管的 Agent 默认参与 AI 递归（拓扑记账的映射点）。
		AIRecursion: true,
		Backend:     backend,
		Children:    map[types.AgentID]types.ChildStatus{},
	}
	s.nsOf[id] = ns
	s.audit(ctx, id, "actor_registered", string(id), map[string]any{
		"actor": string(id), "depth": depth, "kind": string(actor.AgentKind),
	})
	return nil
}

// Adjudicate 实现 proto.Spawner（裁决契约见接口注释：拒绝是业务结果，
// error 只用于框架级故障）。
//
// 检查顺序即失败归因的优先级（Part 9.2 伪代码）：拓扑 → 权限 → 全局上限 →
// 命名空间子集 → 扇出闸 → 注入合法性。批准后：登记进程 → 登记到请求者的
// Children → 工厂建子 → 启动 goroutine（退出时框架代报兜底）。
//
// Part 14.6 的泛化：requester 查找从统一进程表取（不再要求是 Agent）；
// 人类发起的 spawn 的权威检查是 caps.CanSpawn（人类恒真），深度记账只
// 走 AI→AI——子集不变量照常成立（项目 Agent ⊆ 人类的全量，天然满足）。
func (s *Spawner) Adjudicate(ctx context.Context, req *proto.SpawnRequest) (*proto.SpawnDecision, error) {
	if req == nil {
		return nil, fmt.Errorf("spawner: nil request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Factory == nil {
		return nil, fmt.Errorf("spawner: no ChildFactory configured (assembly bug)")
	}

	requester := s.procs[req.RequesterID]
	if requester == nil {
		return reject(proto.SpawnErrRequesterNotFound, "请求者 %s 不在进程表（框架 bug 或已终止）", req.RequesterID), nil
	}
	// 权威与深度（Part 14.6 的泛化）：
	//   - AI 请求者 → depthGate（CanSpawnAtDepth 谓词 = Profile.CanSpawn
	//     的实时形态 + AI 递归深度记账）；
	//   - 人类请求者 → caps.CanSpawn（CapSet 是权威——人类的 spawn 不占
	//     AI 深度，项目 Agent 是 AI 递归的第 1 层）。
	childDepth := requester.Depth + 1
	if requester.AIRecursion {
		if _, dReject := depthGate(s.cfg.CanSpawnAtDepth, requester.Depth, s.cfg.MaxDepth); dReject != nil {
			return dReject, nil
		}
	} else if !requester.Caps.CanSpawn {
		return reject(proto.SpawnErrNotPermitted,
			"你的角色不允许 fork 子 Agent；请用 file_write / file_edit 直接完成任务"), nil
	}
	if len(s.procs) >= s.cfg.MaxActive {
		return reject(proto.SpawnErrGlobalAgentLimit, "全局活跃 Actor 已达上限 %d", s.cfg.MaxActive), nil
	}
	if req.ProfileID == "" {
		return reject(proto.SpawnErrProfileNotFound, "profile_id 不能为空"), nil
	}
	// 命名空间：构建 + 子集校验（Part 9.3 的关键不变量；人类的基准是
	// nil = 全量——buildNamespace 的"请求即授权"路径 + Subset 终检跳过）。
	parentNS := requesterNS(s, requester)
	childNS, werr := buildNamespace(req, parentNS)
	if werr != nil {
		return reject(proto.SpawnErrNamespaceExceeded, "%s", werr.Error()), nil
	}
	if parentNS != nil && !childNS.Subset(parentNS) {
		return reject(proto.SpawnErrNamespaceExceeded,
			"请求的命名空间超出你的范围；你只能分配你自己可写/可读范围内的路径"), nil
	}
	// 扇出闸 2：每任务 fork 轮数（统一对人类与 AI 生效——这是任务面
	// 预算，不是身份面）。
	if requester.ForkRounds >= s.cfg.MaxForkRounds {
		return reject(proto.SpawnErrForkRounds,
			"已在本任务内 fork %d 轮，请自行完成剩余工作", requester.ForkRounds), nil
	}
	// 注入合法性：越界的 Seq 拒绝（跳过会让子上下文缺掉被显式要求注入的
	// 部分，而它自己不知道——SpawnRequest 契约）。
	injected, ierr := s.fetchInjected(ctx, req)
	if ierr != nil {
		return reject(proto.SpawnErrInvalidInject, "%s", ierr.Error()), nil
	}

	// ---- 批准：登记 + 建子 + 启动 ----
	s.nextID++
	childID := types.AgentID(fmt.Sprintf("sub_%06d", s.nextID))
	childCaps := actor.AgentCaps(s.cfg.CanSpawnAtDepth(childDepth))
	backend := actor.NewChannelBackend(actor.DefaultChannelBuffer)
	child := &Process{
		ID:    childID,
		Depth: childDepth,
		State: types.StateRunning,
		Caps:  childCaps,
		// AI 递归记账：Adjudicate 创建的都是 AI（人类只经 RegisterHuman）。
		AIRecursion: true,
		ParentID:    requester.ID,
		Children:    map[types.AgentID]types.ChildStatus{},
		Backend:     backend,
		Writable:    writablePatterns(childNS),
		reported:    false,
		StartedAt:   time.Now(),
	}
	s.procs[childID] = child
	s.nsOf[childID] = childNS
	requester.Children[childID] = types.ChildRunning
	requester.ForkRounds++
	s.audit(ctx, req.RequesterID, "spawn", string(childID), map[string]any{
		"requester": string(req.RequesterID), "depth": childDepth,
		"task": req.TaskDescription, "writable": req.WritablePaths,
	})

	plan := &ChildPlan{
		ID: childID, Depth: childDepth, ParentID: requester.ID,
		Namespace: childNS, Injected: injected, Mailbox: backend.Receive(),
		Task: req.TaskDescription,
	}
	runner, err := s.cfg.Factory.BuildChild(ctx, plan, req)
	if err != nil {
		// 建子失败是框架级故障：回滚登记（半注册状态会让进程表与实际
		// 运行的 Agent 不一致——Registry 契约的同一纪律）。
		delete(s.procs, childID)
		delete(requester.Children, childID)
		requester.ForkRounds--
		return nil, fmt.Errorf("spawner: build child %s: %w", childID, err)
	}
	// runCtx 独立于请求 ctx 的派生点：Watchdog 的 Terminate 经 cancel 打断
	// 子的 Run（Run 的退出路径覆盖 ctx 取消——返回错误 → 框架代报 failed）。
	runCtx, cancel := context.WithCancel(ctx)
	child.cancel = cancel // Adjudicate 持锁期间直接赋值（进程表只由持锁方写）
	go s.runChild(runCtx, child, runner)
	return &proto.SpawnDecision{Status: proto.SpawnApproved, ChildAgentID: childID}, nil
}

// runChild 是子生命周期的包装（Spawner 拥有此 goroutine；退出条件：
// 子 Run 返回或 runCtx 被 Terminate 取消）。退出未 report 的子由框架
// 代报 failed——活性兜底；Watchdog 终止的子走同一兜底，原因文本里带
// watchdog 的裁决（Part 8.5：父的视角始终只有"子回来了/被终止了"）。
func (s *Spawner) runChild(ctx context.Context, child *Process, runner ChildRunner) {
	err := runner.Run(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if child.reported {
		child.State = types.StateIdle
		return
	}
	// 框架代报（Part 8.7：MsgChildReport 含框架代报的 failed）。
	child.State = types.StateCrashed
	reason := "child exited without report"
	if err != nil {
		reason = fmt.Sprintf("child exited with error: %v", err)
	}
	if child.terminateReason != "" {
		reason = fmt.Sprintf("watchdog 终止：%s（底层退出状态：%v）", child.terminateReason, err)
	}
	s.deliverReportLocked(&proto.ChildReport{
		ChildID:   child.ID,
		Status:    proto.ReportFailed,
		Report:    "框架代报：" + reason,
		StartedAt: child.StartedAt,
	})
}

// Terminate 强制终止一个子 Agent（Watchdog 的 mechanic 面）。
//
// 语义：取消执行 ctx → 子的 Run 返回（或超时后被 LLM 层的 timeout 打断）
// → runChild 的框架代报路径向父投递 failed。已 report 的子是终止不了
// 的（任务已终结）→ 错误；未启动（cancel 未挂）→ 错误（诊断面）。
func (s *Spawner) Terminate(id types.AgentID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.procs[id]
	if child == nil {
		return fmt.Errorf("spawner: %s not in process table", id)
	}
	if child.reported {
		return fmt.Errorf("spawner: child %s already reported (terminate is only for live children)", id)
	}
	if child.cancel == nil {
		return fmt.Errorf("spawner: child %s not started (no cancel point)", id)
	}
	if child.terminateReason == "" {
		child.terminateReason = reason // 只记第一次裁决（重复 cancel 无害但归因要唯一）
	}
	child.cancel()
	return nil
}

// ProcessInfo 是进程表的一行快照（Watchdog / marl status 的口径）。
type ProcessInfo struct {
	ID        types.AgentID
	ParentID  types.AgentID
	Depth     int
	State     types.AgentState
	StartedAt time.Time
	Termini   string // 已终止原因（未终止 = ""）
	// 统一 Actor 面（Part 14）。
	Kind        string // "human" | "agent"（呈现层专用——status 的图标/颜色）
	PendingKind string // 挂起分支（"" = 无；discussing / awaiting_gate）
	PendingAt   time.Time
}

// Snapshot 返回全部进程的快照（升序按 ID）。
//
// Watchdog 与状态 UI 从这里读进程表——持有 Spawner 内部结构的只读
// 快照（进程表扁平：父子关系在信息里显式呈现，但表本身仍是扁平 map）。
// 人类 Actor 的行在快照里（Part 14.6：监督树以人类为根）。
func (s *Spawner) Snapshot() []ProcessInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProcessInfo, 0, len(s.procs))
	for _, p := range s.procs {
		kind := string(actor.AgentKind)
		if !p.AIRecursion {
			kind = string(actor.HumanKind)
		}
		out = append(out, ProcessInfo{
			ID: p.ID, ParentID: p.ParentID, Depth: p.Depth,
			State: p.State, StartedAt: p.StartedAt, Termini: p.terminateReason,
			Kind: kind, PendingKind: p.pendingKind, PendingAt: p.pendingAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ReportToParent 实现 agent.ReportSink（子 Agent 投递 report 的通道）。
//
// From 由本方法按代码路径填写（原则 4：Agent 无法影响）——report 的
// ChildID 就是投递者身份，父可以信任。
func (s *Spawner) ReportToParent(ctx context.Context, report *proto.ChildReport) error {
	if report == nil || report.ChildID == "" {
		return fmt.Errorf("spawner: report requires child id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.procs[report.ChildID]
	if child == nil {
		return fmt.Errorf("spawner: child %s not in process table", report.ChildID)
	}
	if child.reported {
		return fmt.Errorf("spawner: child %s already reported (duplicate report is a protocol bug)", report.ChildID)
	}
	child.reported = true
	return s.deliverReportLocked(report)
}

// deliverReportLocked 把 report 投递到父的信箱并登记子状态（调用方持锁）。
//
// 父不存在（已退出）→ 错误：report 无处可去，子应知道投递失败。
// 父是统一进程表里的一行（Part 14：人类是合法的父——项目 Agent 的
// report 经文件后端成为人类收件箱里的一条）。
func (s *Spawner) deliverReportLocked(report *proto.ChildReport) error {
	child := s.procs[report.ChildID]
	if child == nil {
		return fmt.Errorf("spawner: child %s vanished", report.ChildID)
	}
	parent := s.procs[child.ParentID]
	if parent == nil {
		return fmt.Errorf("spawner: parent %s of %s not found", child.ParentID, child.ID)
	}
	status := types.ChildDone
	if report.Status == proto.ReportFailed {
		status = types.ChildFailed
	}
	parent.Children[child.ID] = status
	if err := parent.Backend.Deliver(proto.Envelope{
		From:    child.ID,
		To:      parent.ID,
		Type:    proto.MsgChildReport,
		Payload: report,
		TraceID: types.TraceID(fmt.Sprintf("report-%s", child.ID)),
	}); err != nil {
		return fmt.Errorf("spawner: report of %s to %s: %w", child.ID, parent.ID, err)
	}
	return nil
}

// fetchInjected 取注入消息（调用方持锁；ctx 查询在锁内——SQLite 查询
// 毫秒级，锁竞争面是 fork 裁决，可接受；若成为热点再拆）。
func (s *Spawner) fetchInjected(ctx context.Context, req *proto.SpawnRequest) ([]*types.LogEntry, error) {
	if len(req.InjectMessages) == 0 {
		return nil, nil
	}
	last, err := s.cfg.Log.LastSeq(ctx, req.RequesterID)
	if err != nil {
		return nil, fmt.Errorf("inject: requester log: %w", err)
	}
	out := make([]*types.LogEntry, 0, len(req.InjectMessages))
	for _, seq := range req.InjectMessages {
		if seq < 1 || seq > last {
			return nil, fmt.Errorf("inject seq %d out of range [1,%d] (out-of-range inject must be rejected, not skipped)", seq, last)
		}
		e, err := s.cfg.Log.GetBySeq(ctx, req.RequesterID, seq)
		if err != nil {
			return nil, fmt.Errorf("inject seq %d: %w", seq, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// SendTo 实现 proto.Spawner.SendTo：框架级的统一投递面（Part 14.4——
// 路由一致，传输按目标 Actor 的后端选择：Agent 走 channel、人类走控制
// 面文件）。
//
// 目标不存在 → 错误（信件无处可去必须可见）；投递失败（channel 满超时 /
// 文件写入失败）→ 原样上抛（"发出去但没人收到"不可归因）。
func (s *Spawner) SendTo(id types.AgentID, env proto.Envelope) error {
	if env.To == "" {
		env.To = id
	}
	s.mu.Lock()
	proc := s.procs[id]
	s.mu.Unlock()
	if proc == nil {
		return fmt.Errorf("spawner: %s not in process table (send to)", id)
	}
	return proc.Backend.Deliver(env)
}

// MarkPending / ClearPending 是挂起分支的登记（Part 14.8 的"监控义务在
// 框架侧"——Watchdog 扫描进程表的停摆数据源）。实现 agent.HumanLink 的
// 装配面；未知 id 忽略（进程已退出的挂起登记无观测意义）。
func (s *Spawner) MarkPending(id types.AgentID, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.procs[id]
	if p == nil {
		return
	}
	p.pendingKind = kind
	p.pendingAt = time.Now()
}

func (s *Spawner) ClearPending(id types.AgentID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.procs[id]
	if p == nil {
		return
	}
	p.pendingKind = ""
	p.pendingAt = time.Time{}
}

// SetFactory 注入 ChildFactory（装配期的后置注入：工厂需要引用本 Spawner
// 作为子的 ReportSink，存在构造顺序依赖）。仅允许在首次 Adjudicate 前
// 设置一次——运行中更换工厂会让进程表里的子来自不同代工厂，行为不可归因。
func (s *Spawner) SetFactory(f ChildFactory) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f == nil {
		return fmt.Errorf("spawner: nil factory")
	}
	if s.cfg.Factory != nil {
		return fmt.Errorf("spawner: factory already set")
	}
	s.cfg.Factory = f
	return nil
}

// ProcessOf 返回进程的只读快照（Watchdog/状态观测用；阶段 9 的完整形态）。
func (s *Spawner) ProcessOf(id types.AgentID) (depth int, state types.AgentState, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.procs[id]
	if p == nil {
		return 0, "", false
	}
	return p.Depth, p.State, true
}

// reject 构造拒绝裁决（Code 与 Reason 严格配套，见 SpawnDecision 契约）。
func reject(code proto.SpawnErrorCode, format string, args ...any) *proto.SpawnDecision {
	return &proto.SpawnDecision{
		Status: proto.SpawnRejected,
		Code:   code,
		Reason: fmt.Sprintf(format, args...),
	}
}

// writablePatterns 提取命名空间的 write 挂载（代报机械检查的扫描范围）。
func writablePatterns(ns *types.Namespace) []string {
	if ns == nil {
		return nil
	}
	out := make([]string, 0, len(ns.Mounts))
	for _, m := range ns.Mounts {
		if m.Mode == types.PathWrite {
			out = append(out, m.Pattern)
		}
	}
	return out
}

// requesterNS 取请求者的命名空间（进程表不存 Namespace——它在 Agent 的
// SkillEnv 里；阶段 5 的登记点是 Bootstrap/建子时的 Writable 快照。
// 为了 Subset 校验，Spawner 需要完整父命名空间：由 Bootstrap 记录）。
//
// 见 bootstrapNS 字段。
func requesterNS(s *Spawner, p *Process) *types.Namespace {
	return s.nsOf[p.ID]
}

// audit 记裁决审计（Audit 为 nil 跳过；AgentID 是审计契约的必填项）。
func (s *Spawner) audit(ctx context.Context, agentID types.AgentID, action, target string, payload map[string]any) {
	if s.cfg.Audit == nil {
		return
	}
	ev := &store.AuditEvent{AgentID: agentID, Action: action, Target: target, Payload: payload}
	if err := s.cfg.Audit.Append(ctx, ev); err != nil {
		fmt.Printf("marl: spawner audit failed (action=%s): %v\n", action, err)
	}
}
