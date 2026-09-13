// Package gateway 是前端（TUI / Web UI / 原生 GUI）与 Marl 交互的可复用
// 中间层：把 contract.Interaction（契约）折算成 **UI 框架无关的状态快照 +
// 领域事件流**，让三个前端共享"什么是一条 gate 请求、树怎么建、成本怎么
// 汇总"的全部领域逻辑，差异只在渲染。
//
// 数据流建模（单向）：
//
//	contract.Interaction ──(单 goroutine 轮询)──▶ 新 State 快照 + 分类事件
//	                              │                    │
//	                              ▼                    ▼
//	                     Subscribe() 订阅者通道     State() 直接读最新
//
// 设计取舍（重要）：
//   - **快照承载而非增量补丁**：每条 Update 携带全量 State。订阅者（TUI）
//     落后或丢弃若干条 Update 无损——下一条自愈。这继承 Marl "audit 为真相"
//     的哲学：前端不维护第二份可漂移的领域状态。
//   - **导出具体类型而非接口**（[权衡]）：Gateway 以具体 *Gateway 导出；
//     消费侧（TUI）按需声明窄接口用于测试替换。第二个前端实现意图出现前
//     不预建接口（指令 §2.2：未导出为默认态，接口需第二意图）。
//   - **单一 goroutine 持有轮询**：所有快照构建串行；contract 实现自身的
//     并发安全由其契约保证（server.App 的 mu / HTTPClient 的 http.Client）。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/server"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// Config 是 Gateway 的可调参数（零值 = 全部用缺省；函数式选项之外的
// 结构形态——字段少且都同质，结构体比选项函数更直白 [权衡: 显式 Config
// vs WithXxx 选项：字段 < 5 时结构体零值更惯用]）。
type Config struct {
	// Tick 是轮询步长（快照 + 事件）。<=0 → 700ms（与 serve 的 SSE 粒度
	// 对齐；本机 SQLite/HTTP 查询在该频率下开销可忽略 [推断]）。
	Tick time.Duration
	// HistoryLimit 是首次拉取的事件历史上限（0 → 1000）。事件流面板的
	// 初始内容；之后走游标增量。
	HistoryLimit int
	// Version 透传给进程内 App 的版本号（诊断显示）。
	Version string
}

// ErrNotProject 是 root 不是 Marl 项目（缺 .marl/config.yaml）的哨兵。
var ErrNotProject = errors.New("gateway: not a marl project (run: marl init)")

// Update 是推给订阅者的一条批次：新全量快照 + 本轮新消费的分类事件。
//
// 不变量：State 永不为 nil（每条 Update 必带快照）；Events 可为空（纯
// 快照刷新）；快照发布后不可变（订阅者只读，需要修改先深拷贝自己那份）。
type Update struct {
	State  *State
	Events []DomainEvent
	Err    error // 本轮非致命错误（某次查询失败；轮询继续）
}

// Gateway 是前端的可复用数据层（具体类型；见包注释的接口取舍）。
type Gateway struct {
	src       contract.Interaction
	conn      ConnState
	tick      time.Duration
	history   int
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	state     *State
	subs      map[int]chan Update
	nextSub   int
	nudge     chan struct{} // 命令执行后立即刷新的触发（非阻塞）
	closeOnce sync.Once
	done      chan struct{} // 轮询 goroutine 的退出信号（Close 后关闭）
}

// State 是前端的全量快照（一次 tick 的产出；发布后不可变）。
//
// 字段都是"面板直接可渲染"的形状：树已建好、成本已推导、待办已聚合。
// 派生逻辑（BuildTree/DeriveAlerts/actionItems）只在这里跑，前端不重复实现。
type State struct {
	Conn      ConnState                      // 连接模式
	Run       contract.RunStatus             // 顶栏运行态
	Tree      []*TreeNode                    // 监督树（human 为根，DFS 序）
	Inbox     []contract.InboxItem           // 收件箱（gate/direct/report）
	Escal     []contract.EscalationView      // 待回复求助
	Discuss   []contract.DiscussionView      // 讨论列表
	Costs     *store.TaskCostSummary         // 成本汇总（无记录 → nil）
	Events    []DomainEvent                  // 最近事件（时间升序，窗口截断）
	CostView  CostView                       // 成本推导（占比/建议）
	LastSeq   int64                          // 事件游标（断线续传/去重）
	UpdatedAt time.Time                      // 快照生成时刻
}

// ConnState 是连接模式的呈现态（顶栏徽章）。
type ConnState struct {
	Mode string // "in-process" | "http"
	Note string // 附加说明（如最近错误）
}

// New 装配 Gateway：先探测守护进程（serve.lock + 进程存活），在跑 → HTTP
// 模式；否则 → 进程内 App（等价"带界面的 marl start"）。两种模式对上层
// 完全透明——这正是契约层的意义。
//
// 契约：
//   - 前置：root 是 Marl 项目根（.marl/config.yaml 存在），否则 ErrNotProject。
//   - 后置：轮询 goroutine 已启动（订阅者会收到首个快照）；Close 后完全退出。
//   - 失败：装配 App 失败（骨架缺失/权限） → 错误。
//
// 并发：返回的 Gateway 可安全被多个 goroutine 使用（读 State/命令并发；
// 内部轮询单 goroutine）。
func New(root string, cfg Config) (*Gateway, error) {
	if cfg.Tick <= 0 {
		cfg.Tick = 700 * time.Millisecond
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = 1000
	}
	if _, err := os.Stat(filepath.Join(root, ".marl", "config.yaml")); err != nil {
		return nil, ErrNotProject
	}
	g := &Gateway{
		tick:    cfg.Tick,
		history: cfg.HistoryLimit,
		subs:    map[int]chan Update{},
		nudge:   make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	g.ctx, g.cancel = context.WithCancel(context.Background())

	var src contract.Interaction
	if pid, addr, ok := readServeLock(filepath.Join(server.ControlRootOf(root), "serve.lock")); ok && processAlive(pid) {
		src = server.NewHTTPClient(addr)
		g.conn = ConnState{Mode: "http", Note: addr}
	} else {
		app, err := server.NewApp(root, filepath.Join(root, ".marl", "store.db"), cfg.Version)
		if err != nil {
			g.cancel()
			return nil, fmt.Errorf("gateway: assemble in-process app: %w", err)
		}
		src = app
		g.conn = ConnState{Mode: "in-process", Note: root}
	}
	g.src = src
	go g.loop()
	return g, nil
}

// Subscribe 注册一个更新通道（buffered 16；满时丢弃——Update 携带全量
// 快照，丢弃中间批次无损，见包注释）。返回的 cancel 摘除该订阅者（幂等）。
//
// 订阅者在收到 Update 前就会从 State() 拿到当前快照——首帧不需要等 tick。
func (g *Gateway) Subscribe() (<-chan Update, func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch := make(chan Update, 16)
	id := g.nextSub
	g.nextSub++
	g.subs[id] = ch
	return ch, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if c, ok := g.subs[id]; ok {
			delete(g.subs, id)
			close(c)
		}
	}
}

// State 返回最新快照（无快照前返回 nil——首 tick 前的窗口极短，调用方
// 以 nil 判断"尚未就绪"）。
func (g *Gateway) State() *State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// Conn 返回连接模式（装配后不变）。
func (g *Gateway) Conn() ConnState { return g.conn }

// --- 命令面：同步转发给契约实现，成功后 nudge 立即刷新 ---

// StartTask 启动任务（透传 contract.StartTask；运行中返回 409 语义的
// ErrAlreadyRunning——调用方自行呈现）。
func (g *Gateway) StartTask(task string) error {
	id, err := g.src.StartTask(task)
	_ = id // 返回的根 Agent ID 在下一帧快照的树里可见——这里不冗余携带
	if err != nil {
		return err
	}
	g.nudgeRefresh()
	return nil
}

// StopTask 停止任务（force = 跳过优雅等待）。
func (g *Gateway) StopTask(force bool) error {
	err := g.src.StopTask(force)
	if err == nil {
		g.nudgeRefresh()
	}
	return err
}

// SendMessage 给目标 agent 插话（human_note 语义：下一轮编排可见）。
func (g *Gateway) SendMessage(to, text string) error {
	err := g.src.SendMessage(to, text)
	if err == nil {
		g.nudgeRefresh()
	}
	return err
}

// ReplyGate 提交审批决策（文件通道：写 @ 命令行进收件箱；生效有静默窗，
// 以后续 gate_decision 审计事件为确认——UI 不谎报）。
func (g *Gateway) ReplyGate(id string, d contract.GateDecision) error {
	err := g.src.ReplyGate(id, d)
	if err == nil {
		g.nudgeRefresh()
	}
	return err
}

// ReplyDiscussion 回复讨论（annotation + approve）。
func (g *Gateway) ReplyDiscussion(id, annotation string, approve bool) error {
	err := g.src.ReplyDiscussion(id, annotation, approve)
	if err == nil {
		g.nudgeRefresh()
	}
	return err
}

// ReplyEscalation 回复求助（写 "## 回复" + 移 done/；读侧 nonce 校验消费）。
func (g *Gateway) ReplyEscalation(id, reply string) error {
	err := g.src.ReplyEscalation(id, reply)
	if err == nil {
		g.nudgeRefresh()
	}
	return err
}

// Conversation 读指定 agent 的会话（渲染直连——会话缓存是选中节点的
// UI 局部关注点，不进全局快照 [权衡]）。
func (g *Gateway) Conversation(ctx context.Context, agentID string) ([]*types.LogEntry, error) {
	return g.src.Conversation(ctx, agentID)
}

// ReadInbox 读收件文件全文（审批卡的详情源——决策卡展示文件通道的
// 原始内容，不经二手转述）。
func (g *Gateway) ReadInbox(name string) ([]byte, error) {
	return g.src.ReadInbox(name)
}

// Close 停止轮询并释放资源（幂等）。进程内模式下同时关闭底层 App
// （HTTP 模式下 HTTPClient.Close 是 no-op）。
func (g *Gateway) Close() error {
	g.closeOnce.Do(func() {
		g.cancel()
		close(g.done)
	})
	if closer, ok := g.src.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// nudgeRefresh 非阻塞触发立即刷新（命令后的快速反馈；满/无消费者时
// 依赖下一 tick 兜底）。
func (g *Gateway) nudgeRefresh() {
	select {
	case g.nudge <- struct{}{}:
	default:
	}
}

// loop 是轮询主循环（唯一的所有者 goroutine；退出条件 = Close 的 ctx 取消）。
func (g *Gateway) loop() {
	ticker := time.NewTicker(g.tick)
	defer ticker.Stop()
	// 首帧立即出（订阅者不必等一个 tick）。
	g.oneRound(true)
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-g.done:
			return
		case <-g.nudge:
			g.oneRound(false)
		case <-ticker.C:
			g.oneRound(false)
		}
	}
}

// oneRound 执行一轮：拉事件（游标增量）→ 拉快照 → 构建状态 → 发布。
// first=true 时用 HistoryLimit 拉取初始历史（事件流面板的首屏内容）。
func (g *Gateway) oneRound(first bool) {
	ctx := g.ctx
	var errs []string

	since := int64(0)
	if !first && g.state != nil {
		since = g.state.LastSeq
	}
	limit := g.history
	if !first {
		limit = 500
	}
	events, err := g.src.Events(ctx, since, limit)
	if err != nil {
		errs = append(errs, "events: "+err.Error())
	}

	run := g.src.CurrentRun()
	agents := g.src.Agents()
	inbox, err := g.src.Inbox()
	if err != nil {
		errs = append(errs, "inbox: "+err.Error())
	}
	escal, err := g.src.Escalations()
	if err != nil {
		errs = append(errs, "escalations: "+err.Error())
	}
	discuss, err := g.src.Discussions()
	if err != nil {
		errs = append(errs, "discussions: "+err.Error())
	}
	costs, err := g.src.Costs(ctx, "")
	if err != nil {
		costs = nil // ErrNotFound（尚无记账）是正常业务态：面板显示"暂无"
	}

	// 游标推进 + 域事件折算（去重由 Seq 单调保证；乱序防御性丢弃）。
	var lastSeq int64
	if g.state != nil {
		lastSeq = g.state.LastSeq
	}
	domains := make([]DomainEvent, 0, len(events))
	for _, ev := range events {
		if ev == nil || ev.Seq <= lastSeq {
			continue
		}
		lastSeq = ev.Seq
		domains = append(domains, Classify(*ev))
	}

	st := &State{
		Conn:      g.conn,
		Run:       run,
		Tree:      BuildTree(agents),
		Inbox:     inbox,
		Escal:     escal,
		Discuss:   discuss,
		Costs:     costs,
		CostView:  DeriveCosts(costs),
		Events:    g.appendEvents(domains),
		LastSeq:   lastSeq,
		UpdatedAt: time.Now(),
	}
	if len(errs) > 0 {
		st.Conn.Note = strings.Join(errs, "; ")
	}

	var batch []DomainEvent
	if len(domains) > 0 {
		batch = domains
	}
	g.publish(Update{State: st, Events: batch})
	if len(errs) > 0 {
		// 错误随下一帧带走（订阅者读 State.Conn.Note）；这里不重复推送。
		_ = errs
	}
}

// appendEvents 把新事件并入快照的事件窗口（升序，尾部截断 500 条——
// 事件流面板的内存上限；全量真相在 audit_events 表，面板是投影）。
func (g *Gateway) appendEvents(newOnes []DomainEvent) []DomainEvent {
	const window = 500
	var old []DomainEvent
	if g.state != nil {
		old = g.state.Events
	}
	all := append(append([]DomainEvent{}, old...), newOnes...)
	if len(all) > window {
		all = all[len(all)-window:]
	}
	return all
}

// publish 广播一条 Update（非阻塞；满则丢弃——全量快照语义使丢弃无损）。
func (g *Gateway) publish(u Update) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = u.State
	for _, ch := range g.subs {
		select {
		case ch <- u:
		default:
			// 订阅者跟不上：丢帧。下一条 Update 携带全量快照，自愈。
		}
	}
}

// readServeLock 读 serve.lock（pid:/addr: 文本行格式，与 cmd/marl 的
// readServeLock 同源）。返回 ok=false 表示锁缺失/不完整。
//
// [权衡: 复制 20 行解析 vs 从 main 包导出]：main 包（package main）无法被
// import，复制是唯一选择；两处语义一致由"serve.lock 格式由 serve 写"这一
// 单写者事实锚定。
func readServeLock(path string) (int, string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, "", false
	}
	pid, addr := 0, ""
	for _, ln := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "pid:"); ok {
			if p, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil {
				pid = p
			}
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "addr:"); ok {
			addr = strings.TrimSpace(v)
		}
	}
	if pid > 0 && addr != "" {
		return pid, addr, true
	}
	return 0, "", false
}

// processAlive 报告 pid 是否存活（signal 0 探测；与 cmd/marl 同语义——
// Windows 上 os.FindProcess 恒成功、Signal 恒 nil，[版本依赖: unix 语义]）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// NewWithInteraction 是测试注入点：用给定的契约实现构造 Gateway（跳过
// 探测与装配）。生产路径恒走 New——此函数的存在理由是让 Gateway 的轮询/
// 折算/发布逻辑可以用脚本化替身做确定性验证，而不依赖 SQLite 或网络。
//
// conn 以"注入者声明"为准（测试传什么就是什么）。
func NewWithInteraction(src contract.Interaction, cfg Config) *Gateway {
	if cfg.Tick <= 0 {
		cfg.Tick = 700 * time.Millisecond
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = 1000
	}
	g := &Gateway{
		src:     src,
		conn:    ConnState{Mode: "test", Note: "injected"},
		tick:    cfg.Tick,
		history: cfg.HistoryLimit,
		subs:    map[int]chan Update{},
		nudge:   make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	g.ctx, g.cancel = context.WithCancel(context.Background())
	go g.loop()
	return g
}
