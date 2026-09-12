package compress

// Compressor 实现（orchestrate.Compressor 契约的落地，Part 3.7 全流程）。
//
// 压缩主流程（Compress）与"内容已在手上"的 Admit 共享落盘与建 View 的
// 尾段（admitSUM），保证两条路径产出的 View 形态一致。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"marl/internal/orchestrate"
	"marl/internal/store"
	"marl/internal/types"
)

// SumCompressor 是 orchestrate.Compressor 的实现。
//
// 零值契约：SumCompressor{} 不可用（engine 为 nil），必须经 NewCompressor。
//
// 并发：Compress/Admit/ValidateSUM 可并发调用——它们不修改接收者状态，
// 共享的 Engine 内部自带锁（budget）。
type SumCompressor struct {
	engine *Engine
	root   string // ValidateSUM 的路径存在性基准（构造时注入，见契约）
}

// NewCompressor 构造 SumCompressor。
//
// 失败：engine 为 nil、root 为空（路径校验没有基准等于没有校验）。
func NewCompressor(engine *Engine, root string) (*SumCompressor, error) {
	if engine == nil {
		return nil, fmt.Errorf("compress: engine is required")
	}
	if root == "" {
		return nil, fmt.Errorf("compress: workspace root is required for SUM path validation")
	}
	return &SumCompressor{engine: engine, root: root}, nil
}

// region 是一次压缩的三段划分。各下标序列都指向**编译序**（Position 升序、
// 含不可见条目）的序列位置；entries 是压缩区条目（血缘与渲染用）。
type region struct {
	firstRound int               // 第一个轮起点（头部到此为止）
	tailStart  int               // 尾部起点（最近 KeepTailTurns 轮的第一条）
	middle     []int             // 压缩区（可见且未 pin 的序列下标）
	entries    []*types.LogEntry // middle 对应的 Log 条目
}

// Compress 实现 orchestrate.Compressor（契约见接口注释）。
//
// 开放边界的裁决（契约要求写进 ADR，见 ADR-0026）：
//   - **L0 已独自达标**：L0 清理后的收益 ≥ MinReclaimFraction 时跳过
//     SUM（省一次 LLM 调用），返回 SUMENTry=nil 的结果。判据只有本处
//     一份实现（触发点只判 headroom，不判收益）；
//   - **L0 后中段为空**：返回 orchestrate.ErrNothingToCompress（调用方
//     据此跳过压缩继续主循环）。
//
// 失败：
//   - policy/view 非法 → store.ErrInvalid；
//   - SUM 校验在 MaxRetries 次重试后仍不过 → 错误（调用方降级到 L2 或
//     escalate——阶段 3 的 L2 是硬编码失败，见"不做"清单）；
//   - 收益不足 → 错误（含 reclaim 数值，供降级决策）。
func (c *SumCompressor) Compress(ctx context.Context, lg store.MessageLog, view *types.ContextView, policy orchestrate.CompressionPolicy) (*orchestrate.CompressionResult, error) {
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	if err := view.Validate(); err != nil {
		return nil, fmt.Errorf("%w: view: %v", store.ErrInvalid, err)
	}
	ordered := orderItems(view) // 编译序的 view.Items 下标
	entries, err := fetchEntries(ctx, lg, view, ordered)
	if err != nil {
		return nil, err
	}
	visible := visibleFlags(view, ordered)
	oldTokens := sumTokens(entries, visible)

	// --- L0 机械清理（View 级 exclude；Log 不动）---
	l0Visible, l0Pruned := l0Cleanup(visible, entries)
	l0Tokens := sumTokens(entries, l0Visible)
	var l0Reclaim float64
	if oldTokens > 0 {
		l0Reclaim = float64(oldTokens-l0Tokens) / float64(oldTokens)
	}
	// 开放边界裁决：L0 已独自达标 → 跳过 SUM（判据唯一实现处）。
	if l0Pruned > 0 && l0Reclaim >= policy.MinReclaimFraction {
		if err := validatePairing(l0Visible, entries); err != nil {
			return nil, err // 产出前终检：配对破坏的 View 必被厂商拒绝
		}
		return &orchestrate.CompressionResult{
			View:     cloneWithExclusions(view, ordered, l0Visible),
			SUMENTry: nil,
			Reclaim:  l0Reclaim,
			L0Pruned: l0Pruned,
		}, nil
	}

	// --- 划定压缩区间（L0 清理后的可见集合上）---
	reg := determineRegion(l0Visible, entries, policy.KeepTailTurns)
	if len(reg.middle) == 0 {
		return nil, orchestrate.ErrNothingToCompress
	}

	// --- 生成 SUM（重试温度稍高；Part 3.7 步骤 4-5）---
	regionText := renderRegion(reg.entries)
	sumContent := ""
	var lastErr error
	for attempt := 0; attempt <= policy.MaxRetries; attempt++ {
		// 重试温度：+0.2 封顶 1.0（DeepSeek 思考模式下采样参数本就不生效
		// （ADR-0021），这里的温度只是"换个采样点"的表达，不是确定性
		// 手段）。温度按调用传入而非改写 Engine 状态——Engine 保持可并发。
		temp := c.engine.sampling.Temperature + 0.2*float64(attempt)
		if temp > 1.0 {
			temp = 1.0
		}
		sumContent, _, lastErr = c.engine.call(ctx, sumSystemPrompt, regionText, false, temp)
		if lastErr == nil {
			lastErr = c.ValidateSUM(ctx, sumContent)
		}
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("compress: SUM generation failed after %d attempt(s): %w", policy.MaxRetries+1, lastErr)
	}

	// --- 估算收益（本地估算口径，与 oldTokens 同源）---
	regionTokens := 0
	for _, e := range reg.entries {
		regionTokens += e.TokenEst
	}
	newTokens := l0Tokens - regionTokens + types.EstimateTokens(sumContent)
	var reclaim float64
	if oldTokens > 0 {
		reclaim = float64(oldTokens-newTokens) / float64(oldTokens)
	}
	if reclaim < policy.MinReclaimFraction {
		// 阶段 3 "不做"：升级到 L2 硬编码为失败。错误文本带数值，
		// 供调用方（阶段 4 的升级逻辑）与日志归因。
		return nil, fmt.Errorf("compress: reclaim %.3f < min %.3f; L2 ladder upgrade not implemented in phase 3", reclaim, policy.MinReclaimFraction)
	}

	// --- 落盘 + 建 View（与 Admit 共享尾段）---
	sourceIDs := make([]types.MessageID, 0, len(reg.entries))
	for _, e := range reg.entries {
		sourceIDs = append(sourceIDs, e.ID)
	}
	// 产出前终检：最终可见集合 = L0 后可见 − 压缩区（SUM 不携带配对）。
	finalVisible := make([]bool, len(l0Visible))
	copy(finalVisible, l0Visible)
	for _, seq := range reg.middle {
		finalVisible[seq] = false
	}
	if err := validatePairing(finalVisible, entries); err != nil {
		return nil, err
	}
	res, err := c.admitSUM(ctx, lg, view, ordered, l0Visible, *reg, sumContent, sourceIDs)
	if err != nil {
		return nil, err
	}
	res.Reclaim = reclaim
	res.L0Pruned = l0Pruned
	return res, nil
}

// Admit 实现 orchestrate.Compressor（"内容已在手上"的路径）。
//
// 契约：sumContent 非空、sourceIDs 非空；悬空引用 → store.ErrNotFound
// （宁可失败也不接受断掉的血缘链）。本方法不重复 ValidateSUM（契约：
// 调用方已自行跑过）。
//
// View 语义：引用 sourceIDs 的条目被 SUM 取代（第一个被取代者的位置由
// SUM 接管），其余条目原样保留。
func (c *SumCompressor) Admit(ctx context.Context, lg store.MessageLog, view *types.ContextView, sumContent string, sourceIDs []types.MessageID) (*orchestrate.CompressionResult, error) {
	if strings.TrimSpace(sumContent) == "" || len(sourceIDs) == 0 {
		return nil, fmt.Errorf("%w: admit requires non-empty sum content and source ids", store.ErrInvalid)
	}
	if err := view.Validate(); err != nil {
		return nil, fmt.Errorf("%w: view: %v", store.ErrInvalid, err)
	}
	ordered := orderItems(view)
	entries, err := fetchEntries(ctx, lg, view, ordered)
	if err != nil {
		return nil, err
	}
	// 悬空引用检查：血缘是摘要的唯一凭据。
	byID := make(map[types.MessageID]*types.LogEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	sourceSet := make(map[types.MessageID]bool, len(sourceIDs))
	var middleEntries []*types.LogEntry
	for _, id := range sourceIDs {
		e, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: source %s not in log", store.ErrNotFound, id)
		}
		sourceSet[id] = true
		middleEntries = append(middleEntries, e)
	}
	oldTokens := sumTokens(entries, visibleFlags(view, ordered))
	regionTokens := sumTokens(middleEntries, allTrue(len(middleEntries)))

	// 产出前终检：Admit 的可见集合 = 原可见 − 被取代者（SUM 不携带配对）。
	// 调用方给出的 sourceIDs 若拆散一对（只给结果不给意图），在这里被拒。
	finalVisible := visibleFlags(view, ordered)
	for seq, itemIdx := range ordered {
		if sourceSet[view.Items[itemIdx].Ref] {
			finalVisible[seq] = false
		}
	}
	if err := validatePairing(finalVisible, entries); err != nil {
		return nil, err
	}

	sum := types.NewLogEntry(view.AgentID, types.RoleAssistantReply, sumContent)
	sum.Prov = types.ProvSummaryOf
	sum.SourceIDs = append([]types.MessageID(nil), sourceIDs...)
	if _, err := lg.Append(ctx, sum); err != nil {
		return nil, fmt.Errorf("compress: append SUM: %w", err)
	}

	out := &types.ContextView{AgentID: view.AgentID}
	sumPlaced := false
	for _, itemIdx := range ordered {
		it := view.Items[itemIdx]
		if !sourceSet[it.Ref] {
			out.Items = append(out.Items, it)
			continue
		}
		if !sumPlaced { // SUM 接管第一个被取代者的位置
			out.Items = append(out.Items, types.ViewItem{
				Ref:       sum.ID,
				WireRole:  types.WireAssistant,
				Stability: types.StabilityStable,
				Visible:   true,
				Position:  it.Position,
			})
			sumPlaced = true
		}
	}
	if !sumPlaced { // sourceIDs 全部不在 View 里（合法但异常）：SUM 追加到尾部
		out.Items = append(out.Items, types.ViewItem{
			Ref: sum.ID, WireRole: types.WireAssistant, Stability: types.StabilityStable,
			Visible: true, Position: view.Items[ordered[len(ordered)-1]].Position + 1,
		})
	}
	orchestrate.RenumberView(out)
	var reclaim float64
	if oldTokens > 0 {
		reclaim = float64(oldTokens-regionTokens+types.EstimateTokens(sumContent)) / float64(oldTokens)
	}
	return &orchestrate.CompressionResult{View: out, SUMENTry: sum, Reclaim: reclaim}, nil
}

// admitSUM 是 Compress 的共享尾段：追加 SUM 条目进 Log（ProvSummaryOf，
// SourceIDs 指向压缩区全部条目），构造新 View =
// [保留头部（L0 后仍可见 + 区内 pinned 条目）] + [SUM] + [保留尾部]。
//
// 关于"返回的 View 暂不引用 SUM"的契约措辞（ADR-0026 的裁决）：返回的
// View **包含** SUM 引用（Part 3.7 步骤 7 的字面定义）——"暂不引用"指的是
// 调用方的**现行** view 对象不被本方法触碰：采纳与否（a.view = res.View）
// 是主 Agent 的显式决定，拒绝采纳时 SUM 条目留在 Log 成为未被引用的
// 审计事实，主干分毫未动。两个表述在这层含义下相容。
//
// 区内 pinned 条目不参与压缩（裁剪豁免对压缩同样生效——它是"裁剪时豁免"
// 的语义），原样保留在 SUM 之前（相互相对序不变；它们与被压缩内容的
// 交错序本来就会随压缩消失）。
func (c *SumCompressor) admitSUM(ctx context.Context, lg store.MessageLog, view *types.ContextView, ordered []int, l0Visible []bool, reg region, sumContent string, sourceIDs []types.MessageID) (*orchestrate.CompressionResult, error) {
	sum := types.NewLogEntry(view.AgentID, types.RoleAssistantReply, sumContent)
	sum.Prov = types.ProvSummaryOf
	sum.SourceIDs = append([]types.MessageID(nil), sourceIDs...)
	if _, err := lg.Append(ctx, sum); err != nil {
		return nil, fmt.Errorf("compress: append SUM: %w", err)
	}

	out := cloneWithExclusions(view, ordered, l0Visible)
	middleSet := make(map[int]bool, len(reg.middle)) // 编译序下标集合
	for _, seq := range reg.middle {
		middleSet[seq] = true
	}
	var head, pinned, tail []types.ViewItem
	for seq, itemIdx := range ordered {
		if !l0Visible[seq] {
			continue
		}
		switch {
		case seq < reg.firstRound:
			head = append(head, out.Items[itemIdx])
		case seq < reg.tailStart:
			if middleSet[seq] {
				continue // 被压缩区取代
			}
			pinned = append(pinned, out.Items[itemIdx]) // 区内 pinned：豁免保留
		default:
			tail = append(tail, out.Items[itemIdx])
		}
	}
	newItems := make([]types.ViewItem, 0, len(head)+len(pinned)+len(tail)+1)
	newItems = append(newItems, head...)
	newItems = append(newItems, pinned...)
	newItems = append(newItems, types.ViewItem{
		Ref:       sum.ID,
		WireRole:  types.WireAssistant, // Part 3.7：SUM 以 assistant 身份呈现（"我的工作记忆"）
		Stability: types.StabilityStable,
		Visible:   true,
		Pinned:    false,
		Position:  0, // orchestrate.RenumberView 统一赋值
	})
	newItems = append(newItems, tail...)
	out.Items = newItems
	orchestrate.RenumberView(out)
	return &orchestrate.CompressionResult{View: out, SUMENTry: sum}, nil
}

// ---------------------------------------------------------------------------
// 机械件：排序 / 取条目 / L0 / 分区 / 渲染（全部纯函数）
// ---------------------------------------------------------------------------

// orderItems 返回 view.Items 按 Position 升序的下标序列（稳定排序，
// 同 Position 保持切片序）。
//
// 并发：纯函数。
func orderItems(view *types.ContextView) []int {
	idx := make([]int, len(view.Items))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return view.Items[idx[a]].Position < view.Items[idx[b]].Position
	})
	return idx
}

// fetchEntries 按编译序取全部条目（含不可见条目——L0 与分区需要全貌；
// 可见性过滤在消费侧做）。View 引用取不到 Log 是严重事件（store 契约），
// 报错不跳过。
func fetchEntries(ctx context.Context, lg store.MessageLog, view *types.ContextView, ordered []int) ([]*types.LogEntry, error) {
	entries := make([]*types.LogEntry, 0, len(ordered))
	for _, itemIdx := range ordered {
		e, err := lg.Get(ctx, view.Items[itemIdx].Ref)
		if err != nil {
			return nil, fmt.Errorf("compress: fetch %s: %w", view.Items[itemIdx].Ref, err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// visibleFlags 提取编译序的可见性标记。
func visibleFlags(view *types.ContextView, ordered []int) []bool {
	out := make([]bool, len(ordered))
	for seq, itemIdx := range ordered {
		out[seq] = view.Items[itemIdx].Visible
	}
	return out
}

// allTrue 返回长度 n 的全 true 标记（Admit 路径没有 L0）。
func allTrue(n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = true
	}
	return out
}

// sumTokens 按可见标记累计本地估算 token（View 预算口径）。
func sumTokens(entries []*types.LogEntry, visible []bool) int {
	total := 0
	for i, e := range entries {
		if visible[i] {
			total += e.TokenEst
		}
	}
	return total
}

// cloneWithExclusions 按 L0 的可见性标记构造新 View（传入 view 不被修改；
// EstimatedTokens 属于"上次编译"的缓存值，编排后必然失准——不复制，
// 留零值即"从未估算"，消费者会重新估算）。
func cloneWithExclusions(view *types.ContextView, ordered []int, l0Visible []bool) *types.ContextView {
	out := &types.ContextView{AgentID: view.AgentID}
	out.Items = make([]types.ViewItem, len(view.Items))
	copy(out.Items, view.Items)
	for seq, itemIdx := range ordered {
		if !l0Visible[seq] {
			out.Items[itemIdx].Visible = false
		}
	}
	return out
}

// l0Cleanup 执行 L0 机械清理，返回每个**编译序**位置的存留标记与清理数。
//
// 三条规则（Part 3.7 步骤 3）：
//  1. 去重：同一文件的多次 file_read（成功结果），只留最后一次；
//  2. 剔除被覆盖的旧读取：file_edit 之前的同文件 file_read 可丢
//     （编辑已使读取内容失效）；
//  3. exclude 历史 thinking（Audience=both 的思维链不占上下文；
//     audit-only 的条目本来就不在 View 里，无需处理）。
//
// **配对不变量（真机实测的教训，见测试报告）**：assistant 的 tool_calls
// 意图条目与它的 tool_result 在编译后的请求里必须成对出现——只删结果
// 会留下"没有结果的工具调用"，被 Normalizer 的 Assert 拒绝（厂商同样
// 拒绝）。因此规则 1/2 标记一个结果为排除时，其配对意图**一并排除**；
// 一个意图只有在其**全部**结果都被排除时才排除（部分结果的意图仍然
// 有效）。同理，validatePairing 在产出前做双向校验（见下）。
//
// Pinned 条目豁免（裁剪豁免对 L0 同样生效）。失败的 file_read（ENOENT
// 等）不参与去重——错误信息是模型纠错依据，不是冗余内容。
// Log 不动：清理只表达为 View 的 Visible=false。
//
// 并发：纯函数。
func l0Cleanup(visible []bool, entries []*types.LogEntry) (l0Visible []bool, pruned int) {
	l0Visible = make([]bool, len(visible))
	copy(l0Visible, visible)
	// 文件 → 最后一次成功读取的编译序下标；以及 file_edit 的下标与路径。
	lastRead := make(map[string]int)
	type edit struct {
		seq  int
		path string
	}
	var edits []edit
	excludeResult := func(seq int) {
		if l0Visible[seq] {
			l0Visible[seq] = false
			pruned++
		}
	}
	for seq, e := range entries {
		if !l0Visible[seq] || !toolIs(e, "file_read") {
			continue
		}
		path, ok := toolResultPath(e)
		if !ok {
			continue // 失败结果或无法解析：不参与去重
		}
		if prev, dup := lastRead[path]; dup {
			excludeResult(prev) // 规则 1：旧读取被新读取覆盖（配对意图随之下）
		}
		lastRead[path] = seq
	}
	for seq, e := range entries {
		if toolIs(e, "file_edit") {
			if path, ok := toolResultPath(e); ok {
				edits = append(edits, edit{seq: seq, path: path})
			}
		}
	}
	for _, ed := range edits {
		for seq, e := range entries {
			if seq >= ed.seq || !l0Visible[seq] || !toolIs(e, "file_read") {
				continue
			}
			if path, ok := toolResultPath(e); ok && path == ed.path {
				excludeResult(seq) // 规则 2：编辑使读取失效（配对意图随之下）
			}
		}
	}
	for seq, e := range entries {
		if e.Role == types.RoleThinking && e.Audience == types.AudienceBoth && l0Visible[seq] {
			l0Visible[seq] = false // 规则 3：历史思维链退出上下文
			pruned++
		}
	}
	pruned += excludeOrphanIntents(l0Visible, entries)
	return l0Visible, pruned
}

// excludeOrphanIntents 把"全部结果都被排除"的意图条目一并排除（配对
// 不变量的另一半：没有结果的 tool_calls 不能出现在请求里）。返回追加的
// 排除数。
//
// 归属规则：每个结果归属于**最近一个**（按位置向前找）id 集合包含它的
// 意图条目——这与线路协议的分组语义一致（tool 消息与紧邻其前的
// assistant.tool_calls 组配对），在模型跨轮复用同一 id 时（阶段 1 探测
// 证实厂商接受该形态）仍然正确；"全局 id 表"会把同 id 的结果错挂到
// 第一个意图上，导致部分排除的组被误判为全排除。
//
// 并发：纯函数。
func excludeOrphanIntents(visible []bool, entries []*types.LogEntry) int {
	excluded := 0
	for seq, e := range entries {
		if e.Role != types.RoleAssistantReply || !visible[seq] {
			continue
		}
		ids := intentIDs(e)
		if len(ids) == 0 {
			continue
		}
		total, excludedCount := 0, 0
		for rseq := seq + 1; rseq < len(entries); rseq++ {
			re := entries[rseq]
			if re.Role != types.RoleToolResult {
				if re.Role == types.RoleAssistantReply {
					break // 下一组意图开始：结果必须紧跟其组，扫描终止
				}
				continue
			}
			rid, _ := re.Meta["tool_call_id"].(string)
			if !containsID(ids, rid) {
				continue
			}
			total++
			if !visible[rseq] {
				excludedCount++
			}
		}
		if total > 0 && excludedCount == total {
			visible[seq] = false
			excluded++
		}
	}
	return excluded
}

// containsID 报告 ids 是否包含 id。
//
// 并发：纯函数。
func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// intentIDs 取意图条目携带的 tool_call id 列表（解码失败视为空——
// 该意图本来就不会进请求，见 agent.decodeToolCallsMeta 的同键逻辑）。
func intentIDs(e *types.LogEntry) []string {
	raw, ok := e.Meta["tool_calls"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := m["id"].(string); ok && id != "" {
			out = append(out, id)
		}
	}
	return out
}

// validatePairing 校验可见集合的调用配对不变量（对"进请求的子序列"做
// 与 wire.Assert 同构的状态机检查）：
//   - 可见结果的当前组必须是包含其 id 的可见意图（悬空结果）；
//   - 可见意图的组内结果全部可见（没有结果的 tool_calls 会以"打断/悬空"
//     的形态被厂商拒绝）。
//
// "进请求"按编译口径计：编译为空段（无内容且无可解码 tool_calls）的
// 条目不产生段，因此也不参与配对——与 agent.compileView 的过滤一致。
// 违反即框架 bug（L0/分区的实现缺陷），在产出点失败离现场最近。
//
// 并发：纯函数。
func validatePairing(visible []bool, entries []*types.LogEntry) error {
	pending := map[string]bool{}
	groupOpen := false
	checkClose := func(where string) error {
		if groupOpen && len(pending) > 0 {
			return fmt.Errorf("compress: pairing broken at %s: visible tool-call group has %d unpaired result(s)", where, len(pending))
		}
		groupOpen = false
		pending = map[string]bool{}
		return nil
	}
	for seq, e := range entries {
		if !visible[seq] || !inRequest(e) {
			continue
		}
		switch {
		case e.Role == types.RoleAssistantReply:
			ids := intentIDs(e)
			if len(ids) == 0 {
				if err := checkClose("assistant reply"); err != nil {
					return err
				}
				continue
			}
			if err := checkClose("new intent"); err != nil {
				return err
			}
			for _, id := range ids {
				if pending[id] {
					return fmt.Errorf("compress: pairing broken: duplicate tool_call id %q in one group", id)
				}
				pending[id] = true
			}
			groupOpen = true
		case e.Role == types.RoleToolResult:
			if !groupOpen {
				return fmt.Errorf("compress: pairing broken: visible tool result %s has no preceding visible intent", e.ID)
			}
			id, _ := e.Meta["tool_call_id"].(string)
			if !pending[id] {
				return fmt.Errorf("compress: pairing broken: tool result %s id %q not in current group", e.ID, id)
			}
			delete(pending, id)
		default:
			if err := checkClose(string(e.Role)); err != nil {
				return err
			}
		}
	}
	return checkClose("end")
}

// inRequest 报告条目按编译口径是否会进请求（与 agent.segmentForEntry 的
// 过滤一致：无内容、无可解码 tool_calls、无 reasoning 的条目不产生段）。
//
// 并发：纯函数。
func inRequest(e *types.LogEntry) bool {
	switch e.Role {
	case types.RoleThinking:
		return e.Audience != types.AudienceAudit && e.Content != ""
	case types.RoleAssistantReply:
		if e.Content != "" {
			return true
		}
		raw, ok := e.Meta["tool_calls"]
		if !ok {
			return false
		}
		list, ok := raw.([]any)
		return ok && len(list) > 0
	default:
		return e.Content != ""
	}
}

// toolIs 判断条目是否为指定技能的结果（tool_result 的 Meta["name"]）。
func toolIs(e *types.LogEntry, name string) bool {
	if e.Role != types.RoleToolResult {
		return false
	}
	n, _ := e.Meta["name"].(string)
	return n == name
}

// toolResultPath 从工具结果条目里提取文件路径（成功结果才有；SkillResult
// 序列化形态是 {"ok":..,"Data":{"path":..}}，兼容小写 data 的历史形态）。
func toolResultPath(e *types.LogEntry) (string, bool) {
	if ok, _ := e.Meta["ok"].(bool); !ok {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(e.Content), &m); err != nil {
		return "", false
	}
	data, _ := m["Data"].(map[string]any)
	if data == nil {
		data, _ = m["data"].(map[string]any)
	}
	if data == nil {
		return "", false
	}
	p, _ := data["path"].(string)
	return p, p != ""
}

// determineRegion 在 L0 后的可见集合上划定三段。
//
// 轮的口径（ADR-0026）：一次 user/assistant 往返 = 主循环的一个 eventLoop
// 轮次——assistant 的工具调用意图条目（Meta 带 tool_calls）或最终回复
// 开启一轮，其后的 tool_result 归入该轮。这个口径让"保留尾部 N 轮"在
// 长工具循环任务里留下最近 N 次真实交互，而不是被单条 user 消息吞掉
// 整个历史。user_input 一律归头部（任务原话，压缩区的上游）。
//
// 并发：纯函数。
func determineRegion(visible []bool, entries []*types.LogEntry, keepTail int) *region {
	// 轮起点：assistant 的工具调用意图（Meta 带 tool_calls）。
	// 不带 tool_calls 的文本回复（模型在调用之间的进度旁白、最终总结）
	// **不**开启新轮——它们附着于当前轮（真机实测的教训：把旁白当轮起点
	// 会切出"只有一句话"的微型轮，SUM 的固定成本超过压缩区，reclaim 归零）。
	// user_input 一律归头部（任务原话，压缩区的上游）。
	//
	// 并发：纯函数。
	isRoundStart := func(e *types.LogEntry) bool {
		if e.Role != types.RoleAssistantReply {
			return false
		}
		if raw, ok := e.Meta["tool_calls"]; ok {
			if list, isList := raw.([]any); isList && len(list) > 0 {
				return true
			}
		}
		return false
	}
	firstRound := -1
	var starts []int
	for seq, e := range entries {
		if !visible[seq] {
			continue
		}
		if isRoundStart(e) {
			if firstRound < 0 {
				firstRound = seq
			}
			starts = append(starts, seq)
		}
	}
	if firstRound < 0 || len(starts) <= keepTail {
		// 中段为空：tailStart 顶到末尾，middle 恒空（调用方转译为哨兵）。
		return &region{firstRound: max(firstRound, 0), tailStart: len(entries)}
	}
	tailStart := starts[len(starts)-keepTail]
	middle := make([]int, 0, tailStart-firstRound)
	var middleEntries []*types.LogEntry
	for seq := firstRound; seq < tailStart; seq++ {
		if !visible[seq] {
			continue
		}
		middle = append(middle, seq)
		middleEntries = append(middleEntries, entries[seq])
	}
	return &region{firstRound: firstRound, tailStart: tailStart, middle: middle, entries: middleEntries}
}

// renderRegion 把压缩区条目渲染成一次性转录文本（Orchestrator 的只读输入）。
//
// 渲染原则：给模型"谁说了什么"的最小语境；不渲染时间戳（Part 3.2：
// 时间戳占 token 且破缓存前缀——这里是独立调用不进主缓存，但口径统一）。
// 空内容的条目（如纯 tool_calls 意图）渲染为调用描述行。
//
// 并发：纯函数。
func renderRegion(entries []*types.LogEntry) string {
	var sb strings.Builder
	for _, e := range entries {
		switch {
		case e.Role == types.RoleUserInput:
			fmt.Fprintf(&sb, "[用户] %s\n\n", e.Content)
		case e.Role == types.RoleToolResult:
			name, _ := e.Meta["name"].(string)
			fmt.Fprintf(&sb, "[工具 %s 返回] %s\n\n", name, e.Content)
		case e.Role == types.RoleThinking:
			fmt.Fprintf(&sb, "[助手思考] %s\n\n", e.Content)
		case e.Role == types.RoleAssistantReply:
			if calls, ok := decodeCallList(e); ok {
				for _, c := range calls {
					fmt.Fprintf(&sb, "[助手调用工具] %s(%s)\n\n", c.name, c.args)
				}
			}
			if e.Content != "" {
				fmt.Fprintf(&sb, "[助手] %s\n\n", e.Content)
			}
		default:
			fmt.Fprintf(&sb, "[备注 %s] %s\n\n", e.Role, e.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// callSummary 是渲染用的调用摘要。
type callSummary struct{ name, args string }

// decodeCallList 从 assistant 意图条目的 Meta 里取调用摘要
// （与 agent.appendAssistantToolCalls 的编码形态对应）。
func decodeCallList(e *types.LogEntry) ([]callSummary, bool) {
	raw, ok := e.Meta["tool_calls"]
	if !ok {
		return nil, false
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	out := make([]callSummary, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		args, _ := m["arguments"].(string)
		out = append(out, callSummary{name: name, args: args})
	}
	return out, len(out) > 0
}
