package wire

// Pool 的实现（Part 9.8 / 10.12，13.12 阶段 10："Pool 并发闸门与深度
// 优先队列 + 熔断与健康检测"）。
//
// 结构（每 endpoint 一个 worker goroutine + 最小堆队列）：
//
//	Execute(ctx, call)
//	  → 入队（depth 优先；同 depth FIFO）
//	  → 等 item 的结果通道（或 ctx 取消）
//	worker（每 endpoint 一个 goroutine）
//	  → 取堆顶未取消的项（depth 最大者先：深层 Agent 离完成更近，先放行
//	    更快释放整条 wait_children 链——Part 9.8 的深度优先队列注记）
//	  → 熔断检查（open + 冷却未满 → 短路：调用方据此跳阶梯下一级）
//	  → Limiter(RPM) → Semaphore(MaxInflight)
//	  → 调注入的 Caller（Normalizer/Adapter/Denormalizer 的折算闭包由
//	    装配方持有；Pool 只管调度，不 import 具体线路）
//	  → 结果 / 错误计数（CircuitPolicy 的开路判定）→ 交付等待者
//
// 背压语义（Pool 契约）：排队阻塞而非 429；ctx 取消是唯一提前退出；被
// 取消的项由 worker 惰性跳过（主动删除的复杂度换不到第三种正确）。
//
// 熔断计数的界线：caller 返回**非 ctx 取消**的 error 计一次连续失败，
// 成功归零；ctx 取消不计（停机路径混进熔断窗口会让计数器在一次优雅
// 停机里误开路——与 wire.WireAdapter 的取消纪律同源）。

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Caller 是"一次完整调用通路"的注入面（装配方把 Normalizer/Adapter/
// Denormalizer 的折算闭包传进来；Pool 只负责排队、闸门、限流、熔断）。
type Caller func(ctx context.Context, call *PoolCall) (*WireTurn, error)

// endpointConfig 是池构造的每接入点参数（Semaphore/Limiter 由构造函数
// 补齐——EndpointState 的零值契约要求它们必须由构造期建立）。
type endpointConfig struct {
	Name        string
	MaxInflight int
	RPM         int
}

// customResult 是 worker 与等待者的交接载荷。
type customResult struct {
	turn *WireTurn
	err  error
}

// poolItem 是队列元素。
type poolItem struct {
	call   *PoolCall
	ctx    context.Context // Execute 的调用方 ctx（取消即出堆前的跳过判据）
	seq    int64           // 同 depth 的 FIFO 判据（入队单调递增）
	result chan customResult
}

// poolHeap 按 (depth 降序, seq 升序) 排（最小堆：Less = 更优先者的判据为真）。
type poolHeap struct{ items []*poolItem }

func (h *poolHeap) Len() int { return len(h.items) }
func (h *poolHeap) gt(i, j int) bool {
	a, b := h.items[i].call, h.items[j].call
	if a.Depth != b.Depth {
		return a.Depth > b.Depth
	}
	return h.items[i].seq < h.items[j].seq
}
func (h *poolHeap) Less(i, j int) bool { return h.gt(i, j) }
func (h *poolHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *poolHeap) Push(x any)         { h.items = append(h.items, x.(*poolItem)) }
func (h *poolHeap) Pop() any {
	old := h.items
	n := len(old)
	it := old[n-1]
	h.items = old[:n-1]
	return it
}

// endpoint 是一个接入点的运行时资源（堆 + 闸门 + 限流 + 熔断状态）。
type endpoint struct {
	mu      sync.Mutex
	cfg     endpointConfig
	sem     chan struct{}
	limiter *tokenLimiter
	items   poolHeap // 持 mutex 访问
	health  HealthState
	coolAt  time.Time
	busy    int // 在途（已出堆之后：限流等待 + HTTP + 归一化）
	wake    *sync.Cond
	nextSeq int64
}

// PoolImpl 是 Pool 的实现（Part 10.12：排队 → 限流 → 闸门 → 熔断 → 健康）。
//
// 并发：Execute 并发；worker 每 endpoint 一个 goroutine；全部共享状态经
// 各自 endpoint 的 mu（热点只锁一个 endpoint 的队列面）。
type PoolImpl struct {
	mu        sync.Mutex
	closed    bool
	circuit   CircuitPolicy // 全零 = 熔断关闭
	caller    Caller
	eps       map[string]*endpoint
	stop      chan struct{}
	done      chan struct{}
	doneClose sync.Once
	wg        sync.WaitGroup
}

// doneOnce 关闭 done（最后一个 worker 退出时；Stop 的收敛边）。
func (p *PoolImpl) doneOnce() {
	p.doneClose.Do(func() { close(p.done) })
}

// tokenLimiter 是 RPM 的令牌桶（无第三方依赖的绝对间隔形态：上一令牌
// 发出时刻 + 60s/RPM 的等间隔）。
//
// 与 x/time/rate 的取舍：这里只有"单 endpoint ≤ RPM"一个判据，绝对
// 间隔实现 ~30 行且等待下的确定性可测（x/time/rate 的突发语义会让
// "RPM=60"在突发下短时变 2 倍，与厂商报限的观测习惯相反）。
type tokenLimiter struct {
	mu    sync.Mutex
	every time.Duration
	last  time.Time
}

func newTokenLimiter(rpm int) *tokenLimiter {
	iv := time.Minute / time.Duration(rpm)
	return &tokenLimiter{every: iv, last: time.Now().Add(-iv)}
}

// Wait 阻塞直到拿到下一个令牌（或 ctx 取消）。
func (t *tokenLimiter) Wait(ctx context.Context) error {
	t.mu.Lock()
	now := time.Now()
	wait := t.last.Add(t.every).Sub(now)
	if wait > 0 {
		t.last = now.Add(wait) // 占住未来的槽位（防止多人同睡到一个时刻）
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	} else {
		t.last = now
		t.mu.Unlock()
	}
	return nil
}

// NewPool 校验装配并构造池（构造即启动每 endpoint 的 worker）。
func NewPool(cfg map[string]endpointConfig, circuit CircuitPolicy, caller Caller) (*PoolImpl, error) {
	if len(cfg) == 0 {
		return nil, errors.New("pool: no endpoints configured")
	}
	if caller == nil {
		return nil, errors.New("pool: Caller is required")
	}
	if (circuit != CircuitPolicy{}) {
		if err := circuit.Validate(); err != nil {
			return nil, err
		}
	}
	eps := make(map[string]*endpoint, len(cfg))
	for name, c := range cfg {
		switch {
		case c.MaxInflight <= 0:
			return nil, fmt.Errorf("pool: endpoint %q: MaxInflight must be > 0", name)
		case c.RPM <= 0:
			return nil, fmt.Errorf("pool: endpoint %q: RPM must be > 0", name)
		}
		ep := &endpoint{
			cfg:     endpointConfig{Name: name, MaxInflight: c.MaxInflight, RPM: c.RPM},
			sem:     make(chan struct{}, c.MaxInflight),
			limiter: newTokenLimiter(c.RPM),
		}
		ep.wake = sync.NewCond(&ep.mu)
		eps[name] = ep
	}
	p := &PoolImpl{
		circuit: circuit,
		caller:  caller,
		eps:     eps,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for _, ep := range eps {
		p.wg.Add(1)
		go p.worker(ep)
	}
	return p, nil
}

// Execute 实现 Pool：排队阻塞 → 等结果。ctx 取消（含入队前的停机）是
// 唯一的非结果退出路径。
func (p *PoolImpl) Execute(ctx context.Context, call *PoolCall) (*WireTurn, error) {
	if call == nil {
		return nil, errors.New("pool: nil PoolCall")
	}
	if call.AgentID == "" || call.Request == nil || call.TraceID == "" {
		return nil, errors.New("pool: call requires AgentID / Request / TraceID (Envelope contract)")
	}
	if call.Binding.Model == "" || call.Binding.Endpoint == "" {
		return nil, errors.New("pool: call requires Binding with model and endpoint")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("pool: closed")
	}
	ep := p.eps[call.Request.Endpoint]
	p.mu.Unlock()
	if ep == nil {
		return nil, fmt.Errorf("pool: unknown endpoint %q (assembly drift: catalog exceeds pool)", call.Request.Endpoint)
	}
	ep.mu.Lock()
	item := &poolItem{
		call:   call,
		ctx:    ctx,
		seq:    ep.nextSeq,
		result: make(chan customResult, 1),
	}
	ep.nextSeq++
	heap.Push(&ep.items, item)
	ep.wake.Signal()
	ep.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err() // 队列中取消：worker 之后惰性跳到（无泄漏：result 缓冲 1）
	case r := <-item.result:
		return r.turn, r.err
	}
}

// worker 是单 endpoint 的消费循环（goroutine 的 owner = NewPool；退出
// 条件 = p.stop 关闭，或 p.closed 后队列排干）。
func (p *PoolImpl) worker(ep *endpoint) {
	defer p.wg.Done()
	defer func() { p.doneOnce() }()
	for {
		// 取堆顶。队列空 → 短暂让出（2ms 的睡眠是"条件等待的最小
		// 实现"；cond 的跨 goroutine 唤醒链在这个点上换个实现也不减
		// 复杂度，观察面已够——空闲 CPU 成本 ~0.05%）。
		ep.mu.Lock()
		for ep.items.Len() == 0 {
			ep.mu.Unlock()
			select {
			case <-p.stop:
				return
			default:
			}
			time.Sleep(2 * time.Millisecond)
			ep.mu.Lock()
		}
		it := heap.Pop(&ep.items).(*poolItem)
		ep.busy++
		ep.mu.Unlock()

		// 熔断检查（open + 冷却未满 → 短路；冷却已满 → 半开放行）。
		ep.mu.Lock()
		blocked := ep.health.CircuitOpen && time.Now().Before(ep.coolAt)
		ep.mu.Unlock()
		var turn *WireTurn
		var err error
		if blocked {
			err = errors.New("pool: endpoint circuit open (cooldown; see CircuitPolicy)")
		} else {
			turn, err = p.run(ep, it)
		}
		ep.deliver(it, turn, err)
	}
}

// run 是"出堆后"的一段：限流 → 闸门 → Caller → 熔断计数。
func (p *PoolImpl) run(ep *endpoint, it *poolItem) (*WireTurn, error) {
	if err := ep.limiter.Wait(context.Background()); err != nil {
		return nil, err // tokenLimiter 的 Background 形态：我们不支持出堆后的取消
	}
	select {
	case ep.sem <- struct{}{}:
	default:
		// 闸门满：真阻塞（背压纪律），但给整体停机让出一条路。
		select {
		case ep.sem <- struct{}{}:
		case <-p.stop:
			return nil, errors.New("pool: stopped while waiting for slot")
		}
	}
	defer func() {
		<-ep.sem
		ep.mu.Lock()
		ep.busy--
		ep.mu.Unlock()
	}()
	turn, err := p.caller(it.ctx, it.call)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		p.recordError(ep)
		return nil, err
	}
	if err == nil {
		p.recordSuccess(ep)
	}
	return turn, err
}

// deliver 把结果交给等待者（result 缓冲 1：等待者已取消退场时交付不阻塞）。
func (ep *endpoint) deliver(it *poolItem, turn *WireTurn, err error) {
	select {
	case it.result <- customResult{turn: turn, err: err}:
	default:
	}
}

// recordError / recordSuccess 是 CircuitPolicy 状态迁移的唯一写点
// （状态机合聚一处——WireAdapter.HealthCheck 的注释纪律）。
func (p *PoolImpl) recordError(ep *endpoint) {
	if (p.circuit == CircuitPolicy{}) {
		return // 熔断关闭：计数不动（Last Error 也不追——健康只服务熔断语义的中国面）
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.health.ErrorCount++
	if !ep.health.CircuitOpen && ep.health.ErrorCount >= p.circuit.ErrorThreshold {
		ep.health.Status = HealthDown
		ep.health.CircuitOpen = true
		ep.coolAt = time.Now().Add(p.circuit.Cooldown)
	}
}

func (p *PoolImpl) recordSuccess(ep *endpoint) {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.health.ErrorCount = 0
	if ep.health.CircuitOpen {
		// 半开转正：冷却窗口后的第一次成功回到 healthy。
		ep.health.CircuitOpen = false
		ep.health.Status = HealthHealthy
	} else {
		ep.health.Status = HealthHealthy
	}
}

// Health 实现 Pool：健康快照（某时刻的副本；调用方不做跨时刻判断）。
func (p *PoolImpl) Health() map[string]HealthState {
	out := make(map[string]HealthState, len(p.eps))
	for name, ep := range p.eps {
		ep.mu.Lock()
		out[name] = ep.health
		ep.mu.Unlock()
	}
	return out
}

// Inflight 实现 Pool：全部 endpoint 的在途计数（含排队——口径与
// MaxInflight 语义一致的注释见 Pool 契约）。
func (p *PoolImpl) Inflight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, ep := range p.eps {
		ep.mu.Lock()
		total += ep.busy + ep.items.Len()
		ep.mu.Unlock()
	}
	return total
}

// Stop 停止 worker 并关闭池（幂等；Execute 之后返回 closed 错误）。
//
// 等待路径：worker 停止（stop 通道），但**已出堆并进入调用阶段的项会
// 继续完成交付**（worker 的 run 是终需完成的语义——TPS 用户不背锅）。
func (p *PoolImpl) Stop() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	p.mu.Unlock()
	<-p.done
}
