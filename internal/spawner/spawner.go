package spawner

// Spawner：进程表、裁决、生命周期（Part 8.1 / 9.1 / 9.2 / 9.7）。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"marl/internal/proto"
	"marl/internal/store"
	"marl/internal/types"
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
	// MaxDepth 是 fork 深度上限（Part 9.7 闸 1，默认 3 由配置层物化）。
	MaxDepth int
	// MaxActive 是全局活跃 Agent 上限（Part 9.2 的全局资源管控）。
	MaxActive int
	// MaxForkRounds 是同一任务内每父的 fork 轮数上限（Part 9.7 闸 2）。
	MaxForkRounds int
	// CanSpawnAtDepth 报告某深度的 Agent 是否允许 fork（Part 9.2 的
	// Profile.CanSpawn；Profile 系统在阶段 7，先以深度规则表达——阶段 5
	// 的形态是"只有根能 fork"）。
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

// Process 是进程表的一行（Part 8.1：扁平，不存父子关系——父子在 Runtime）。
type Process struct {
	ID      types.AgentID
	Depth   int
	State   types.AgentState // Spawner 视角的生命周期状态
	Mailbox chan proto.Envelope
	// Runtime（框架注入，不持久化，不进 Log）。
	ParentID   types.AgentID
	Children   map[types.AgentID]types.ChildStatus
	ForkRounds int // 本任务内已 fork 的轮数（扇出闸 2）
	// reported 由 Spawner 置位（子通过 ReportToParent 投递时）——
	// 框架代报的判据。
	reported bool
	// Writable 是子命名空间的 write 挂载（框架代报的机械检查范围）。
	Writable []string
}

// Spawner 是框架级单例（Part 9.1）。
//
// 并发：进程表的全部访问经 mu；Adjudicate 与子生命周期回调可并发。
type Spawner struct {
	mu     sync.Mutex
	procs  map[types.AgentID]*Process
	// nsOf 记录每个进程的命名空间（Subset 校验需要完整父命名空间；
	// Process 本体不存——它保持 Part 8.1 的"扁平进程表"形态）。
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

// Bootstrap 创建根 Agent 的进程登记（Part 9.9：不走 adjudicate——根没有
// RequesterID；特殊性仅两处：无父收 report、escalation 落人类信箱）。
//
// ns 是根的命名空间（子集校验的基准）；nil 表示"无命名空间约束"（测试
// 与最小装配）。
//
// 返回信箱读端给 Agent 装配。
func (s *Spawner) Bootstrap(id types.AgentID, ns *types.Namespace) <-chan proto.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := make(chan proto.Envelope, 32)
	s.procs[id] = &Process{
		ID:       id,
		Depth:    0,
		State:    types.StateIdle,
		Mailbox:  mb,
		Children: map[types.AgentID]types.ChildStatus{},
	}
	s.nsOf[id] = ns
	return mb
}

// Adjudicate 实现 proto.Spawner（裁决契约见接口注释：拒绝是业务结果，
// error 只用于框架级故障）。
//
// 检查顺序即失败归因的优先级（Part 9.2 伪代码）：拓扑 → 权限 → 全局上限 →
// 命名空间子集 → 扇出闸 → 注入合法性。批准后：登记进程 → 登记到请求者的
// Children → 工厂建子 → 启动 goroutine（退出时框架代报兜底）。
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
	if !s.cfg.CanSpawnAtDepth(requester.Depth) {
		return reject(proto.SpawnErrNotPermitted, "你的角色不允许 fork 子 Agent；请用 file_write / file_edit 直接完成任务"), nil
	}
	childDepth := requester.Depth + 1
	if childDepth > s.cfg.MaxDepth {
		return reject(proto.SpawnErrMaxDepth,
			"已达最大深度 %d，不能再 fork。请直接执行任务", s.cfg.MaxDepth), nil
	}
	if len(s.procs) >= s.cfg.MaxActive {
		return reject(proto.SpawnErrGlobalAgentLimit, "全局活跃 Agent 已达上限 %d", s.cfg.MaxActive), nil
	}
	if req.ProfileID == "" {
		return reject(proto.SpawnErrProfileNotFound, "profile_id 不能为空"), nil
	}
	// Profile 解析在阶段 7（Profile 加载器）；阶段 5 只做非空校验。
	// 命名空间：构建 + 子集校验（Part 9.3 的关键不变量）。
	childNS, werr := buildNamespace(req, requesterNS(s, requester))
	if werr != nil {
		return reject(proto.SpawnErrNamespaceExceeded, "%s", werr.Error()), nil
	}
	if requesterNS(s, requester) != nil && !childNS.Subset(requesterNS(s, requester)) {
		return reject(proto.SpawnErrNamespaceExceeded,
			"请求的命名空间超出你的范围；你只能分配你自己可写/可读范围内的路径"), nil
	}
	// 扇出闸 2：每任务 fork 轮数。
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
	child := &Process{
		ID:        childID,
		Depth:     childDepth,
		State:     types.StateRunning,
		Mailbox:   make(chan proto.Envelope, 32),
		ParentID:  requester.ID,
		Children:  map[types.AgentID]types.ChildStatus{},
		Writable:  writablePatterns(childNS),
		reported:  false,
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
		Namespace: childNS, Injected: injected,
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
	go s.runChild(ctx, child, runner)
	return &proto.SpawnDecision{Status: proto.SpawnApproved, ChildAgentID: childID}, nil
}

// runChild 是子生命周期的包装（Spawner 拥有此 goroutine；退出条件：
// 子 Run 返回或 ctx 取消）。退出未 report 的子由框架代报 failed——
// 这是阶段 5 的活性兜底（Watchdog 的完整形态在阶段 9）。
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
	s.deliverReportLocked(&proto.ChildReport{
		ChildID:   child.ID,
		Status:    proto.ReportFailed,
		Report:    "框架代报：" + reason,
		StartedAt: time.Now(),
	})
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
	select {
	case parent.Mailbox <- proto.Envelope{
		From:    child.ID,
		To:      parent.ID,
		Type:    proto.MsgChildReport,
		Payload: report,
		TraceID: types.TraceID(fmt.Sprintf("report-%s", child.ID)),
	}:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("spawner: parent %s mailbox full (report of %s dropped after timeout)", parent.ID, child.ID)
	}
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
