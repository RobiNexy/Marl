package gateway

// 纯派生函数集：契约 DTO → 前端可渲染形状。全部无状态、可单测——
// 领域→UI 的折算只写一次（三个前端共享）。

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// TreeNode 是监督树的节点（AgentView + 子指针 + 派生标记）。
//
// 零值：零值 TreeNode 无意义（由 BuildTree 构造）；Kind 的零值 "" 按非
// 人类渲染（防御性——宁把异常数据显示成 agent，不把人类误标成 agent）。
type TreeNode struct {
	ID          string
	Parent      string
	Kind        string // "human" | "agent"（契约 Kind 原样）
	Depth       int
	State       string // types.AgentState 字符串：idle/running/blocked/crashed
	BlockReason string // pending_kind：discussing/escalating/awaiting_gate/...
	PendingAt   time.Time
	StartedAt   time.Time
	Children    []*TreeNode
}

// BuildTree 把扁平 AgentView 列表折算成监督树（human 为根，DFS 序）。
//
// 排序契约：同级按 ID 升序——树的形状只由数据决定，渲染顺序稳定，
// 刷新时节点不跳位。孤儿节点（Parent 指向不存在的节点）挂到根层级尾
// （防御性：审计重建/时钟窗下父子可能短暂不一致，树不能因此丢失节点）。
//
// 失败：无人类根（Agent 列表为空或 kind 全为 agent）→ 返回空切片 +
// nil（正常业务态：任务未启动）。
func BuildTree(agents []contract.AgentView) []*TreeNode {
	byID := make(map[string]*TreeNode, len(agents))
	for _, av := range agents {
		byID[av.ID] = &TreeNode{
			ID:          av.ID,
			Parent:      av.Parent,
			Kind:        av.Kind,
			Depth:       av.Depth,
			State:       av.State,
			BlockReason: av.PendingKind,
			PendingAt:   av.PendingAt,
			StartedAt:   av.StartedAt,
		}
	}
	var roots []*TreeNode
	for _, node := range byID {
		if node.Parent == "" || node.Parent == node.ID {
			roots = append(roots, node)
			continue
		}
		parent, ok := byID[node.Parent]
		if !ok {
			// 孤儿：父节点尚未出现在快照里（时钟窗）→ 提升为根，
			// 而不是静默丢节点（树的完整性优先于树的严格性）。
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	sortNodes(roots)
	for _, node := range byID {
		sortNodes(node.Children)
	}
	return roots
}

// sortNodes 就地按 ID 升序（BuildTree 的排序契约；DFS 用）。
func sortNodes(nodes []*TreeNode) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
}

// Walk 先序遍历树（渲染的展平顺序）。
func Walk(nodes []*TreeNode, fn func(n *TreeNode)) {
	for _, n := range nodes {
		fn(n)
		Walk(n.Children, fn)
	}
}

// --- 成本推导（与 ledger.Render 的机械建议同口径；[权衡] 复制两条规则
// 而非导出 ledger.suggestions——规则共 10 行，导出会破坏 ledger 包的
// 渲染封闭性） ---

// CostView 是从 TaskCostSummary 推导的呈现态（百分比/建议/格式化总额）。
// 全部字段可从零值安全读取（无数据 → 全零 + 无建议）。
type CostView struct {
	Currency    string
	TotalCost   float64
	TotalTokens int64
	Calls       int
	// ReasoningShare 是思维链占总 token 的比例（0-1；无 token → 0）。
	ReasoningShare float64
	// CacheHitRate 是缓存命中占总 token 的比例（0-1；无 token → 0）。
	CacheHitRate float64
	// Suggestions 是机械建议（与 ledger.Report 同口径：思维链 >60%、
	// 缓存命中 <40%）。
	Suggestions []string
	// Levels 是按 rung 升序的分项（渲染顺序稳定）。
	Levels []CostLevel
}

// CostLevel 是一个阶梯的分项行。
type CostLevel struct {
	Rung           string // "r0"/"r1"/...
	Calls          int
	Tokens         int64
	ReasoningShare float64
	CacheHitRate   float64
	Cost           float64
}

// DeriveCosts 从账单汇总推导呈现态（纯函数；nil → 零值 + 无建议）。
func DeriveCosts(sum *store.TaskCostSummary) CostView {
	if sum == nil {
		return CostView{}
	}
	cv := CostView{
		Currency:    sum.Currency,
		TotalCost:   sum.TotalCost,
		TotalTokens: sum.TotalTokens,
	}
	var reasoning, cacheRead int64
	ids := make([]types.RungID, 0, len(sum.ByLevel))
	for id := range sum.ByLevel {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		lv := sum.ByLevel[id]
		if lv == nil {
			continue
		}
		cv.Calls += lv.Calls
		reasoning += lv.ReasoningTokens
		cacheRead += lv.CacheRead
		cv.Levels = append(cv.Levels, CostLevel{
			Rung:           string(id),
			Calls:          lv.Calls,
			Tokens:         lv.Tokens,
			ReasoningShare: ratio(lv.ReasoningTokens, lv.Tokens),
			CacheHitRate:   ratio(lv.CacheRead, lv.Tokens),
			Cost:           lv.Cost,
		})
	}
	cv.ReasoningShare = ratio(reasoning, cv.TotalTokens)
	cv.CacheHitRate = ratio(cacheRead, cv.TotalTokens)
	cv.Suggestions = costSuggestions(cv)
	return cv
}

// ratio 安全除法（分母 0 → 0）。
func ratio(num, den int64) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// costSuggestions 是两条机械规则（>60% 思维链 / <40% 缓存命中）。
func costSuggestions(cv CostView) []string {
	var out []string
	if cv.TotalTokens == 0 {
		return out
	}
	if cv.ReasoningShare > 0.60 {
		out = append(out, fmt.Sprintf("思维链占比 %.0f%%（>60%%）：输出大部分不可见，性价比存疑", cv.ReasoningShare*100))
	}
	if cv.CacheHitRate < 0.40 {
		out = append(out, fmt.Sprintf("缓存命中率 %.0f%%（<40%%）：检查前缀稳定性（frozen 段是否逐字节一致）", cv.CacheHitRate*100))
	}
	return out
}

// --- 待办聚合（Action Center 的数据面） ---

// ActionKind 是待办类型。
type ActionKind string

const (
	ActionGate       ActionKind = "gate"       // 审批请求
	ActionDiscuss    ActionKind = "discussion" // 讨论裁决
	ActionEscalation ActionKind = "escalation" // 求助回复
)

// ActionItem 是 Action Center 的一行（待人类决策的事项）。
type ActionItem struct {
	Kind    ActionKind
	ID      string // gate/disscussion/escalation 的 id（决策提交的钥匙）
	From    string // 发起者（agent id 或空）
	Title   string // 一行摘要（审批的命令 / 讨论的 topic / 求助的 question）
	Detail  string // 预览正文
	ModTime time.Time
}

// ActionItems 聚合三类待办（顺序：escalation 最紧急 > gate > discussion；
// 同类内按时间升序——先到先决策）。
//
// 求助排最前的理由：求助 = agent 已阻塞等待（不回复就停摆）；gate/direct
// 类似，但讨论裁决影响面最大却最不紧急（agent 等审阅但不烧钱）。
func ActionItems(st *State) []ActionItem {
	if st == nil {
		return nil
	}
	items := make([]ActionItem, 0, len(st.Escal)+len(st.Inbox)+len(st.Discuss))
	for _, e := range st.Escal {
		items = append(items, ActionItem{
			Kind: ActionEscalation, ID: e.ID, From: e.From,
			Title: firstNonEmpty(e.Question, "(无问题摘要)"), Detail: e.Preview, ModTime: e.ModTime,
		})
	}
	for _, in := range st.Inbox {
		if in.Type != "gate" {
			continue
		}
		items = append(items, ActionItem{
			Kind: ActionGate, ID: gateIDOf(in.Name), From: in.From,
			Title: firstNonEmpty(in.Preview, in.Name), Detail: in.Preview, ModTime: in.ModTime,
		})
	}
	for _, d := range st.Discuss {
		items = append(items, ActionItem{
			Kind: ActionDiscuss, ID: d.ID, From: "",
			Title: firstNonEmpty(d.Topic, d.ID), Detail: d.Dir, ModTime: d.ModTime,
		})
	}
	return items
}

// gateIDOf 从收件箱文件名提取 gate id（gate_<id>.md → <id>；不匹配 → 原名）。
func gateIDOf(name string) string {
	s := strings.TrimSuffix(name, ".md")
	if v, ok := strings.CutPrefix(s, "gate_"); ok {
		return v
	}
	return s
}

// firstNonEmpty 返回第一个非空字符串（全空 → ""）。
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
