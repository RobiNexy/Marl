package agent

// 压缩的接入点（设计文档 13.5，阶段 3；Part 3.7 的触发与执行）。
//
// 职责切分（与 orchestrate.Compressor 契约一致）：
//   - 本文件只做"触发判断 + 状态机切换 + 采纳结果"；
//     压缩本身（L0 / SUM / 建 View）全部在 Compressor 实现（internal/compress）；
//   - headroom 判断在调用方（本文件），收益判断在 Compressor——判据不重复。

import (
	"context"
	"errors"
	"fmt"

	"marl/internal/orchestrate"
	"marl/internal/types"
)

// CompressConfig 是压缩的装配参数（Config.Compression 非 nil 即启用）。
//
// 零值契约：零值不可用（Compressor 为 nil、Policy 为零值策略）；由 New
// 校验拒绝。默认值（尾部保留轮数等）由构造方物化，本包不提供 Default。
type CompressConfig struct {
	// Compressor 是压缩执行器（internal/compress 的实现）。
	Compressor orchestrate.Compressor
	// Policy 是触发与保留策略；HeadroomThreshold 同时是触发判断与
	// Compress 内部收益判断的配置源（同一组配置判断与执行）。
	Policy orchestrate.CompressionPolicy
	// BudgetReserved 是为回复预留的 token（headroom 扣除项）。
	// 必须 >= 0。
	BudgetReserved int
}

// CompressEvent 记录一次压缩的可观测结果（Run 结束后由宿主读取/打印）。
//
// 字段口径：OldTokens/NewTokens 是本地估算（View 预算口径，含 system 与
// 工具表），与 headroom 判断同源；Reclaim 来自 Compressor 的真实测量。
type CompressEvent struct {
	Round      int
	OldTokens  int
	NewTokens  int
	Reclaim    float64
	L0Pruned   int
	SUMAppended bool // false = L0 独自达标，未生成 SUM
}

// estimateContextTokens 估算当前上下文规模（headroom 的"current"项）。
//
// 组成：system 前缀 + 工具表（冻结前缀，本地估算）+ View 可见条目的
// TokenEst 累计。与 types.EstimateTokens 同一口径（宁高不低的安全侧）。
//
// 并发：只在 eventLoop 的单 goroutine 里调用。
func (a *Agent) estimateContextTokens(ctx context.Context) (int, error) {
	total := types.EstimateTokens(a.sysPrompt)
	for _, s := range a.skills.Schemas() {
		total += types.EstimateTokens(s.Name + s.Description + string(s.Parameters))
	}
	for i := range a.view.Items {
		if !a.view.Items[i].Visible {
			continue
		}
		e, err := a.log.Get(ctx, a.view.Items[i].Ref)
		if err != nil {
			return 0, fmt.Errorf("agent: estimate: fetch %s: %w", a.view.Items[i].Ref, err)
		}
		total += e.TokenEst
	}
	return total, nil
}

// checkHeadroom 报告是否需要压缩（Part 3.7 触发条件）：
// headroom = model_max_tokens - current_context - budget_reserved，
// headroom < threshold 时为真。
//
// 防热循环：上次压缩尝试后上下文没有增长（est <= lastCompressTokens）
// 时不重复触发——压缩失败或无可压区间时，每轮都重试会把主循环变成
// 压缩循环；真正的溢出由厂商错误分类（ErrContextOverflow）兜底。
func (a *Agent) checkHeadroom(ctx context.Context) (bool, int, error) {
	est, err := a.estimateContextTokens(ctx)
	if err != nil {
		return false, 0, err
	}
	headroom := a.maxContextTokens - est - a.compress.BudgetReserved
	if headroom >= a.compress.Policy.HeadroomThreshold {
		return false, est, nil
	}
	if a.lastCompressTokens > 0 && est <= a.lastCompressTokens {
		return false, est, nil // 压缩已试过且上下文没再增长：不空转
	}
	return true, est, nil
}

// doCompress 执行一次压缩并采纳结果（Running → Blocked(BlockCompressing)
// → Running；状态迁移是调用方职责——Compressor 不改 Agent 状态）。
//
// ErrNothingToCompress 不是错误：记 lastCompressTokens 防热循环后继续。
// 采纳 = a.view 换成返回的新 View（"主 Agent 拿到之后自己决定用不用"的
// 落点——拒绝采纳即不赋值，SUM 留在 Log 成为未被引用的审计事实）；
// nextPosition 重置为新 View 的最大 Position + 1。
func (a *Agent) doCompress(ctx context.Context, round, est int) error {
	a.state = types.StateBlocked
	a.blockReason = types.BlockCompressing
	res, err := a.compress.Compressor.Compress(ctx, a.log, a.view, a.compress.Policy)
	a.state = types.StateRunning
	a.blockReason = ""
	if err != nil {
		if errors.Is(err, orchestrate.ErrNothingToCompress) {
			a.lastCompressTokens = est
			return nil
		}
		return fmt.Errorf("agent: round %d: compress: %w", round, err)
	}
	a.lastCompressTokens = est
	a.view = res.View
	maxPos := 0.0
	for i := range a.view.Items {
		if a.view.Items[i].Position > maxPos {
			maxPos = a.view.Items[i].Position
		}
	}
	a.nextPosition = maxPos + 1
	if ev := a.compressEvent(round, res); ev != nil {
		// NewTokens 用同一口径重估（含冻结前缀），与 OldTokens 可直接对照。
		ev.OldTokens = est
		if newEst, eerr := a.estimateContextTokens(ctx); eerr == nil {
			ev.NewTokens = newEst
		}
		a.compressLog = append(a.compressLog, *ev)
	}
	return nil
}

// maybeCompress 是 eventLoop 每轮开头的入口：未启用压缩时是零开销直通。
func (a *Agent) maybeCompress(ctx context.Context, round int) error {
	if a.compress == nil || a.maxContextTokens <= 0 {
		return nil
	}
	need, est, err := a.checkHeadroom(ctx)
	if err != nil {
		return err
	}
	if !need {
		return nil
	}
	return a.doCompress(ctx, round, est)
}

// compressEvent 从压缩结果提取可观测记录。
func (a *Agent) compressEvent(round int, res *orchestrate.CompressionResult) *CompressEvent {
	if res == nil {
		return nil
	}
	return &CompressEvent{
		Round:       round,
		Reclaim:     res.Reclaim,
		L0Pruned:    res.L0Pruned,
		SUMAppended: res.SUMENTry != nil,
	}
}

// CompressionEvents 返回本次 Run 期间发生的压缩记录（宿主打印/测试断言）。
func (a *Agent) CompressionEvents() []CompressEvent {
	return a.compressLog
}
