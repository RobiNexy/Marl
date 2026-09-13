package watchdog

// Watchdog 的判据计算与扫描循环（Part 8.5，13.11 阶段 9）。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"marl/internal/spawner"
	"marl/internal/store"
	"marl/internal/types"
)

// Table 是 Watchdog 对进程表的窄面（消费侧收窄；Spawner 实现）。
//
// Snapshot 只包含"当前进程"，Terminate 是唯一写路径（Kill 一类的能力
// 不上表——Watchdog 没有兴趣也没有权限做超出两项判据的动作）。
type Table interface {
	Snapshot() []ProcessSnapshot
	Terminate(id types.AgentID, reason string) error
}

// ProcessSnapshot 是 Table.Snapshot 的产出单元。
type ProcessSnapshot struct {
	ID        types.AgentID
	ParentID  types.AgentID
	Depth     int
	State     types.AgentState
	StartedAt time.Time
	// 统一 Actor 面（Part 14.8）：挂起分支（等待人类的分支——讨论
	// / Gate 审批；escalation 无超时不登记）。PendingKind 非空时
	// PendingAt 是停摆的起点。
	PendingKind string
	PendingAt   time.Time
}

// LogSource 是"该子有没有新产出"的观察面（判据的两条数据源：Log 追加
// 与 TokenUsed）。实现：store.MessageLog（直接消费——Watchdog 是框架层
// 的系统组件，进程表进程表 Observe 到 Log 的跃迁不必经过 Agent）。
type LogSource interface {
	LastSeq(ctx context.Context, agentID types.AgentID) (int64, error)
	TotalTokens(ctx context.Context, agentID types.AgentID) (int64, error)
}

// Config 是 Watchdog 的装配参数（Part 8.5 判据的可配字面量）。
//
// 零值契约：零值不可用（Interval=0 会转成忙轮询；NoProgressAfter=0
// 会把"刚启动"误判为"无进展"）。由 New 显式校验拒绝。"关闭某项检测"
// 用负值表达（NoProgressAfter < 0 = 关闭该项——与 types.WatchdogPolicy
// 的"0 一律非法"同向：零值绝不可以是隐式默认）。
type Config struct {
	Table Table
	// Log 是判据的数据源（LastSeq = 无进展；TotalTokens = 预算）。
	Log LogSource
	// Audit 非 nil 时记 watchdog 动作（标记/终止都要可追溯）。
	Audit store.AuditStore
	// Interval 是扫描步长（真实环境 30s~5min；测试用毫秒级）。
	Interval time.Duration
	// NoProgressAfter 产出的判据：进程自上次增益（Log seq 变化）后的
	// 最大静默时长。<= 0 = 关闭该检测（零值不可用，见文件头）。
	NoProgressAfter time.Duration
	// MaxChildTokens：子的 est-token 预算上限（超过即强制终止）。
	// <= 0 = 关闭。
	MaxChildTokens int64
	// MaxChildSeconds：单个子任务的最长耗时（StartedAt 起）。<= 0 = 关闭。
	MaxChildSeconds int64
	// PendingOverrun 是"等待人类"分支的停摆告警阈值（Part 14.8：默认
	// 语义 24h——讨论 / Gate 审批的等待有界，escalation 无超时不登记；
	// 告警是升级提示不是终止——"让它自己跑花的是我的钱"，卡死的任务
	// 不烧钱、可观测、由人类决定去留）。<= 0 = 关闭。
	PendingOverrun time.Duration
}

// Watchdog 是框架级的独立监控 goroutine。
type Watchdog struct {
	cfg Config
	// stall 记录"上次扫描时的 seq/到达时刻"（无进展判据的状态）。
	mu    sync.Mutex
	state map[types.AgentID]*progressState
	stop  chan struct{}
	done  chan struct{}
}

// progressState 是无进展判据的一行。
type progressState struct {
	lastSeq    int64     // 上次扫描到的最后 Seq
	firstQuiet time.Time // 该 Seq 首次被观察到的时刻（连续静默的起点）
}

// New 校验配置构造 Watchdog。
//
// 失败：Table/Log 为 nil（监控不了等于不存在）、Interval 非正。
func New(cfg Config) (*Watchdog, error) {
	switch {
	case cfg.Table == nil:
		return nil, fmt.Errorf("watchdog: Table is required")
	case cfg.Log == nil:
		return nil, fmt.Errorf("watchdog: Log is required")
	case cfg.Interval <= 0:
		return nil, fmt.Errorf("watchdog: Interval must be positive")
	}
	// 全部判据关闭的 Watchdog 无意义（= 空转烧 CPU），显式拒绝。
	if cfg.NoProgressAfter <= 0 && cfg.MaxChildTokens <= 0 && cfg.MaxChildSeconds <= 0 && cfg.PendingOverrun <= 0 {
		return nil, fmt.Errorf("watchdog: no detection enabled (set NoProgressAfter/MaxChildTokens/MaxChildSeconds/PendingOverrun)")
	}
	return &Watchdog{
		cfg:   cfg,
		state: map[types.AgentID]*progressState{},
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}, nil
}

// Start 启动扫描 goroutine（唯一 goroutine 的 owner = 本方法；退出条件
// 是 Stop 或底层 ctx 取消）。
func (w *Watchdog) Start(ctx context.Context) {
	go w.run(ctx)
}

// Stop 停止扫描（幂等：多次调用只等一次 done）。host 的收尾点。
func (w *Watchdog) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

// run 是扫描循环。
func (w *Watchdog) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-ticker.C:
			w.scan(ctx)
		}
	}
}

// scan 一轮：快照 → 逐项判据 → 处置。
func (w *Watchdog) scan(ctx context.Context) {
	now := time.Now()
	for _, p := range w.cfg.Table.Snapshot() {
		// 挂起分支的停摆告警（Part 14.8）：等待人类的讨论 / Gate 审批
		// 超过阈值 → 升级告警（审计可见）。不终止——等待不烧钱，卡死
		// 是确认过的回退策略；Watchdog 的义务是让停摆"可见 + 有时长"。
		if p.PendingKind != "" && w.cfg.PendingOverrun > 0 && !p.PendingAt.IsZero() {
			if stalled := now.Sub(p.PendingAt); stalled > w.cfg.PendingOverrun {
				w.audit(ctx, "watchdog_pending_overrun", string(p.ID), map[string]any{
					"pending_kind":    p.PendingKind,
					"stalled_seconds": int64(stalled.Seconds()),
					"threshold":       w.cfg.PendingOverrun.String(),
				})
			}
		}
		if p.State != types.StateRunning {
			// 只有 Run 中的活跃子适用两项判据（blocked 的停摆是 status
			// 的展示项不是 Watchdog 的执行面；crashed/idle 已退出）。
			w.dropState(p.ID)
			continue
		}
		lastSeq, lerr := w.cfg.Log.LastSeq(ctx, p.ID)
		if lerr != nil {
			// Log 读失败：判据缺数据不等于判据通过——记审计跳过本轮
			// （下次扫描重试；误报的代价是标黄，误杀的代价是任务丢失）。
			w.audit(ctx, "watchdog_skip", string(p.ID), map[string]any{"error": lerr.Error()})
			continue
		}
		// 无进展标黄（只记录，不终止）。
		if w.cfg.NoProgressAfter > 0 {
			if stalled, since := w.markProgress(p.ID, lastSeq, now); stalled {
				w.audit(ctx, "watchdog_no_progress", string(p.ID), map[string]any{
					"quiet_seconds": int64(now.Sub(since).Seconds()),
					"last_seq":      lastSeq,
				})
			}
		}
		reason := ""
		if w.cfg.MaxChildSeconds > 0 && int64(now.Sub(p.StartedAt).Seconds()) > w.cfg.MaxChildSeconds {
			reason = fmt.Sprintf("超时（运行 %ds > 预算 %ds）", int64(now.Sub(p.StartedAt).Seconds()), w.cfg.MaxChildSeconds)
		}
		if reason == "" && w.cfg.MaxChildTokens > 0 {
			total, terr := w.cfg.Log.TotalTokens(ctx, p.ID)
			if terr != nil {
				continue // 同 LastSeq：数据不足不判，等下轮
			}
			if total > w.cfg.MaxChildTokens {
				reason = fmt.Sprintf("超预算（est-token %d > %d）", total, w.cfg.MaxChildTokens)
			}
		}
		if reason != "" {
			if err := w.cfg.Table.Terminate(p.ID, reason); err != nil {
				w.audit(ctx, "watchdog_terminate_failed", string(p.ID), map[string]any{"error": err.Error()})
			} else {
				w.audit(ctx, "watchdog_terminated", string(p.ID), map[string]any{"reason": reason})
			}
		}
	}
}

// markProgress 更新无进展状态行；返回 (是否静默超限, 静默起点)。
//
// Seq 增长（新 Log 追加 = 实质信号）→ 重置静默起点；不变 → 累积静默。
// 状态行在新 id 首次出现时创建（firstQuiet = now——"刚看见"不是"已静默"）。
func (w *Watchdog) markProgress(id types.AgentID, lastSeq int64, now time.Time) (bool, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.state[id]
	if !ok || st.lastSeq != lastSeq {
		w.state[id] = &progressState{lastSeq: lastSeq, firstQuiet: now}
		return false, now
	}
	return now.Sub(st.firstQuiet) > w.cfg.NoProgressAfter, st.firstQuiet
}

// dropState 丢弃一个 Agent 的状态行（进程不再活跃/已退出——无限增长
// 的 map 是 Watchdog 自身的资源纪律）。
func (w *Watchdog) dropState(id types.AgentID) {
	w.mu.Lock()
	delete(w.state, id)
	w.mu.Unlock()
}

// audit 记审计（Audit 为 nil 跳过；写失败打 stderr ——判据产物丢失
// 不能误导"什么都没发生"的观察）。
func (w *Watchdog) audit(ctx context.Context, action, target string, payload map[string]any) {
	if w.cfg.Audit == nil {
		return
	}
	ev := &store.AuditEvent{AgentID: types.AgentID("watchdog"), Action: action, Target: target, Payload: payload}
	if err := w.cfg.Audit.Append(ctx, ev); err != nil {
		fmt.Printf("marl: watchdog audit failed (%s): %v\n", action, err)
	}
}

// ------- Table 适配（spawner.Spawner → Watchdog.Table） -------

// TableSource 是适配入参的最小面（避免 Watchdog 强依赖具体 *Spawner
// 的内部形状；类型上仅要求两个真实方法）。
type TableSource interface {
	Snapshot() []spawner.ProcessInfo
	Terminate(id types.AgentID, reason string) error
}

// AdaptTable 把进程表实现（*spawner.Spawner）适配成 Watchdog.Table。
type tableAdapter struct{ s TableSource }

func AdaptTable(s TableSource) Table { return tableAdapter{s: s} }

func (a tableAdapter) Snapshot() []ProcessSnapshot {
	infos := a.s.Snapshot()
	out := make([]ProcessSnapshot, 0, len(infos))
	for _, p := range infos {
		out = append(out, ProcessSnapshot{
			ID: p.ID, ParentID: p.ParentID, Depth: p.Depth,
			State: p.State, StartedAt: p.StartedAt,
			PendingKind: p.PendingKind, PendingAt: p.PendingAt,
		})
	}
	return out
}

func (a tableAdapter) Terminate(id types.AgentID, reason string) error {
	return a.s.Terminate(id, reason)
}
