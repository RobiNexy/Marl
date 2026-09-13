package watchdog

// 判据的独立测试（不夹 Agent/Spawner 的间接面：进程表与 Log 都是替身）。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"marl/internal/store"
	"marl/internal/types"
)

type fakeTable struct {
	mu    sync.Mutex
	procs []ProcessSnapshot
	term  map[types.AgentID]string // new: nil 表在测试里初始化
}

func newFakeTable() *fakeTable { return &fakeTable{term: map[types.AgentID]string{}} }

func (f *fakeTable) Snapshot() []ProcessSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ProcessSnapshot, len(f.procs))
	copy(out, f.procs)
	return out
}

func (f *fakeTable) Terminate(id types.AgentID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.term[id] = reason
	return nil
}

// growingLog 是 LastSeq 递增 / TotalTokens 可设的替身。
type growingLog struct {
	mu    sync.Mutex
	seqOf map[types.AgentID]int64
	tokOf map[types.AgentID]int64
}

func (l *growingLog) LastSeq(_ context.Context, id types.AgentID) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seqOf[id], nil
}

func (l *growingLog) TotalTokens(_ context.Context, id types.AgentID) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tokOf[id], nil
}

func (l *growingLog) bump(id types.AgentID, tokens int64) {
	l.mu.Lock()
	l.seqOf[id]++
	l.tokOf[id] = tokens
	l.mu.Unlock()
}

func newWatch(t *testing.T, tbl Table, log LogSource, ticks int) *Watchdog {
	t.Helper()
	wd, err := New(Config{
		Table:           tbl,
		Log:             log,
		Interval:        time.Duration(ticks) * time.Millisecond,
		NoProgressAfter: -1, // 关闭无进展（本套件测预算/超时）
		MaxChildSeconds: 1,
		MaxChildTokens:  100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// TestOverTimeoutTerminates：超时判据触发 Terminate（另一方检查 reason
// 到达进程表——真实感收束在 internal/agent 的集成测试里）。
func TestOverTimeoutTerminates(t *testing.T) {
	ctx := context.Background()
	tbl := newFakeTable()
	tbl.procs = []ProcessSnapshot{{
		ID: "c1", State: types.StateRunning, StartedAt: time.Now().Add(-2 * time.Second),
	}}
	log := &growingLog{seqOf: map[types.AgentID]int64{}, tokOf: map[types.AgentID]int64{}}
	wd, err := New(Config{
		Table:           tbl,
		Log:             log,
		Interval:        10 * time.Millisecond,
		MaxChildSeconds: 1,
		MaxChildTokens:  0, // 关闭预算
	})
	if err != nil {
		t.Fatal(err)
	}
	wd.scan(ctx) // 直接驱动一轮（不等间隔）
	if tbl.term["c1"] == "" {
		t.Fatalf("terminate not issued: %v", tbl.term)
	}
	if !strings.Contains(tbl.term["c1"], "超时") {
		t.Fatalf("reason = %q", tbl.term["c1"])
	}
}

// TestOverBudgetTerminates：超预算判据。
func TestOverBudgetTerminates(t *testing.T) {
	ctx := context.Background()
	tbl := newFakeTable()
	tbl.procs = []ProcessSnapshot{{
		ID: "c2", State: types.StateRunning, StartedAt: time.Now(),
	}}
	log := &growingLog{seqOf: map[types.AgentID]int64{}, tokOf: map[types.AgentID]int64{"c2": 200}}
	wd, err := New(Config{
		Table:           tbl,
		Log:             log,
		Interval:        10 * time.Millisecond,
		MaxChildSeconds: 0,
		MaxChildTokens:  100,
	})
	if err != nil {
		t.Fatal(err)
	}
	wd.scan(ctx)
	if !strings.Contains(tbl.term["c2"], "超预算") {
		t.Fatalf("reason = %q", tbl.term)
	}
}

// TestNoProgressFlagged：Seq 不变 → 静默累计 → 记审计标黄（不终止）。
func TestNoProgressFlagged(t *testing.T) {
	ctx := context.Background()
	var audMem []*store.AuditEvent
	aud := &kidnapAudit{sink: func(ev *store.AuditEvent) { audMem = append(audMem, ev) }}
	tbl := newFakeTable()
	tbl.procs = []ProcessSnapshot{{
		ID: "c3", State: types.StateRunning, StartedAt: time.Now(),
	}}
	log := &growingLog{seqOf: map[types.AgentID]int64{"c3": 1}, tokOf: map[types.AgentID]int64{}}
	wd, err := New(Config{
		Table:           tbl,
		Log:             log,
		Interval:        10 * time.Millisecond,
		NoProgressAfter: 30 * time.Millisecond,
		MaxChildSeconds: 0,
		MaxChildTokens:  -1,
		Audit:           aud,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 序列 1 在首次观察（创建状态行）；同 Seq 连续两轮 → 超过静默窗。
	wd.scan(ctx)
	time.Sleep(40 * time.Millisecond)
	wd.scan(ctx)
	var flagged bool
	for _, ev := range audMem {
		if ev.Action == "watchdog_no_progress" && ev.Target == "c3" {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("no-progress audit missing: %d events", len(audMem))
	}
	if tbl.term["c3"] != "" {
		t.Fatalf("no-progress must not terminate: %v", tbl.term)
	}
}

// kidnapAudit 是审计收集替身（store.AuditStore 形状）。
type kidnapAudit struct {
	sink func(*store.AuditEvent)
}

func (k *kidnapAudit) Append(ctx context.Context, ev *store.AuditEvent) error {
	k.sink(ev)
	return nil
}

func (k *kidnapAudit) Query(ctx context.Context, filter store.AuditFilter) ([]*store.AuditEvent, error) {
	return nil, fmt.Errorf("not needed")
}

// TestPendingOverrunAlerts：等待人类的挂起分支超阈值 → 升级告警
// （watchdog_pending_overrun）——告警不是终止：卡死不烧钱，去留归人
// （Part 14.8"卡死选择的依据"）。
func TestPendingOverrunAlerts(t *testing.T) {
	ctx := context.Background()
	tbl := newFakeTable()
	tbl.procs = []ProcessSnapshot{
		{ID: "waiting-gate", State: types.StateBlocked, PendingKind: "awaiting_gate",
			PendingAt: time.Now().Add(-25 * time.Hour)}, // 停摆 25h > 24h 阈值
		{ID: "fresh-block", State: types.StateBlocked, PendingKind: "discussing",
			PendingAt: time.Now()}, // 刚挂起：不告警
		{ID: "no-pending", State: types.StateBlocked, PendingKind: "",
			PendingAt: time.Now().Add(-48 * time.Hour)}, // 无挂起登记：不告警
	}
	var audMem []*store.AuditEvent
	aud := &kidnapAudit{sink: func(ev *store.AuditEvent) { audMem = append(audMem, ev) }}
	wd, err := New(Config{Table: tbl, Log: &growingLog{seqOf: map[types.AgentID]int64{}, tokOf: map[types.AgentID]int64{}},
		Interval: time.Hour, PendingOverrun: 24 * time.Hour, Audit: aud})
	if err != nil {
		t.Fatal(err)
	}
	wd.scan(ctx)
	// 告警面：审计里的 pending_overrun 恰好一条（只针对 waiting-gate）。
	n := 0
	for _, ev := range audMem {
		if ev.Action == "watchdog_pending_overrun" && ev.Target == "waiting-gate" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("pending overrun alerts = %d, want 1（终止=%v）", n, tbl.term)
	}
	if len(tbl.term) != 0 {
		t.Fatalf("挂起告警不得终止任务（卡死是确认过的回退策略）: %v", tbl.term)
	}
}
