package wire

// PoolImpl 的契约测试（13.12：深度优先排队 / 熔断 / 健康快照 / 取消）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

func poolTestCall(ep string, depth int, tag string) *PoolCall {
	return &PoolCall{
		AgentID: types.AgentID(tag),
		Depth:   depth,
		Binding: types.Binding{Model: "m", Endpoint: ep},
		Request: &WireRequest{Endpoint: ep, Model: "m", Wire: types.WireOpenAIChat},
		TraceID: types.TraceID("trace-" + tag),
	}
}

func poolOneEndpoint(max int, rpm int) map[string]endpointConfig {
	return map[string]endpointConfig{"ep1": {Name: "ep1", MaxInflight: max, RPM: rpm}}
}

func okTurn(tag string) *WireTurn {
	return &WireTurn{Outcomes: []Outcome{{Reply: tag}}}
}

// TestPoolDepthPriority：MaxInflight=1 时，后入队的深层（2）先于同队
// 的浅层（1）执行（Part 9.8 的深度优先队列向前一线）。
func TestPoolDepthPriority(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var inFlight int
	var maxSeen int
	caller := func(ctx context.Context, call *PoolCall) (*WireTurn, error) {
		// 调用命名模拟真实语义：串行槽位（max=1）+ 每次调用睡 30ms
		// ——给后续排队留出时间窗（同 depth 的 FIFO 也因此可判）。
		mu.Lock()
		inFlight++
		if inFlight > maxSeen {
			maxSeen = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight--
		order = append(order, fmt.Sprintf("%s:%s", call.AgentID, fmt.Sprint(call.Depth)))
		mu.Unlock()
		return okTurn(string(call.AgentID)), nil
	}
	p, err := NewPool(poolOneEndpoint(1, 60000), CircuitPolicy{}, caller)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	var wg sync.WaitGroup
	// （tag, depth）序列：浅层先启动并占住唯一的槽位；后续 FIFO 入队
	// depth 0 / 2 / 1 —— 队列会先出 2（深度优先），再 1。
	seq := []struct {
		tag   string
		depth int
	}{
		{"a1", 0}, {"a0", 0}, {"a2", 2}, {"a1b", 1},
	}
	for i, s := range seq {
		wg.Add(1)
		go func(tag string, depth, idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := p.Execute(ctx, poolTestCall("ep1", depth, tag)); err != nil {
				t.Errorf("%s: %v", tag, err)
			}
		}(s.tag, s.depth, i)
		if i == 0 {
			time.Sleep(10 * time.Millisecond) // 让 a1 先占住槽位→其余排队
		}
		time.Sleep(5 * time.Millisecond) // 入队顺序保持（depth 0 → 2 → 1 需要定出入序）
	}
	wg.Wait()
	// a0（入队最末、最浅）最后：队列的序是 depth 降序。
	want := []string{"a1:0", "a2:2", "a1b:1", "a0:0"}
	got := strings.Join(order, ",")
	if got != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v（深度优先队列语义）", got, want)
	}
}

// TestPoolCircuit：连续失败到阈值 → 开路（短路错误不再打 Caller）；
// 冷却后成功 → 半开转 healthy。
func TestPoolCircuit(t *testing.T) {
	var calls int32
	caller := func(ctx context.Context, call *PoolCall) (*WireTurn, error) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return nil, errors.New("simulated vendor failure")
		}
		return okTurn("recovered"), nil
	}
	p, err := NewPool(poolOneEndpoint(2, 60000),
		CircuitPolicy{ErrorThreshold: 2, Cooldown: 120 * time.Millisecond}, caller)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := p.Execute(ctx, poolTestCall("ep1", 0, fmt.Sprintf("f%d", i))); err == nil || !strings.Contains(err.Error(), "simulated") {
			t.Fatalf("call %d: expected injected error, got %v", i, err)
		}
	}
	// 阈值到 → 开路：第三次不再进 caller（计数停留在 2）。
	if _, err := p.Execute(ctx, poolTestCall("ep1", 0, "short")); err == nil ||
		!strings.Contains(err.Error(), "circuit open") {
		t.Fatalf("circuit-open short-circuit missing: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("caller must not be called while circuit open (calls=%d)", n)
	}
	// 冷却通过 + 成功 → 半开转正。
	time.Sleep(150 * time.Millisecond)
	turn, err := p.Execute(ctx, poolTestCall("ep1", 0, "ok"))
	if err != nil || turn == nil || turn.Outcomes[0].Reply != "recovered" {
		t.Fatalf("half-open call: %v %v", turn, err)
	}
	if h := p.Health()["ep1"]; h.CircuitOpen || h.Status != HealthHealthy {
		t.Fatalf("health after recovery: %+v", h)
	}
}

// TestPoolCtxCancelWhileQueued：闸满时排队的第 2 个调用被 ctx 取消 →
// 原样上抛；Inflight 不泄漏（取消的项被 worker 惰性跳过）。
func TestPoolCtxCancelWhileQueued(t *testing.T) {
	release := make(chan struct{})
	depClose := make(chan struct{})
	defer close(depClose)
	caller := func(ctx context.Context, call *PoolCall) (*WireTurn, error) {
		select {
		case <-release:
			return okTurn("done"), nil
		case <-depClose:
			return NULLTurn(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p, err := NewPool(poolOneEndpoint(1, 60000), CircuitPolicy{}, caller)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	first := make(chan error, 1)
	go func() {
		_, err := p.Execute(context.Background(), poolTestCall("ep1", 0, "first"))
		first <- err
	}()
	time.Sleep(50 * time.Millisecond) // first 占住槽位

	ctx2, cancel2 := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := p.Execute(ctx2, poolTestCall("ep1", 1, "queued"))
		errCh <- err
	}()
	time.Sleep(30 * time.Millisecond) // 入队（闸满 → 排队）
	cancel2()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued call cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued call did not return on cancel")
	}
	close(release) // first 完成 → worker 应该跳过已取消的项（不重复交付）
	if err := <-first; err != nil {
		t.Fatalf("first call: %v", err)
	}
}

// TestPoolInflight：口径包含"排队等待"（Part 10.12：观测的数字与上限
// 语义一致，否则以为有余量）。
func TestPoolInflight(t *testing.T) {
	release := make(chan struct{})
	caller := func(ctx context.Context, call *PoolCall) (*WireTurn, error) {
		<-release
		return okTurn("x"), nil
	}
	p, err := NewPool(poolOneEndpoint(1, 60000), CircuitPolicy{}, caller)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	go p.Execute(context.Background(), poolTestCall("ep1", 0, "a"))
	time.Sleep(30 * time.Millisecond)
	go p.Execute(context.Background(), poolTestCall("ep1", 1, "b"))
	time.Sleep(30 * time.Millisecond)
	if inflight := p.Inflight(); inflight != 2 {
		t.Fatalf("inflight = %d, want 2 (queued must count)", inflight)
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
}

// NULLTurn 占位（防 depClose 未消费时的空交付；本测试暂未实际使用其内容）。
func NULLTurn() *WireTurn { return nil }
