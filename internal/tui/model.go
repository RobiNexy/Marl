package tui

// 根 Model：状态归属与 Update 路由（TEA 纪律：单一状态树，一切变化经
// Update 返回新 Model——Go 无持久数据结构，字段替换即"新树"）。
//
// 状态归属契约：
//   - 领域真相（树/收件箱/成本/事件）只来自 gateway.State 快照——本结构
//     不缓存第二份（唯一例外：选中 agent 的会话，理由见 convFor）。
//   - UI 局部态（焦点/选中/折叠/modal/toast）在这里——它们是"前端意念"，
//     与领域真相无关，进快照反而是污染。

import (
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/frontend/gateway"
	"github.com/RobiNexy/Marl/internal/types"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// Focus 是焦点面板（Tab 循环顺序 = 视觉顺序）。
type Focus int

const (
	FocusTree Focus = iota
	FocusWork
	FocusSide
	FocusInput
)

// Tab 是工作区的页签。
type Tab int

const (
	TabConv Tab = iota
	TabEvents
	TabInspect
)

// modal 是当前打开的浮层（互斥；mNone = 无）。
type modal int

const (
	mNone modal = iota
	mActions
	mGate
	mEscal
	mDiscuss
	mHelp
	mQuitConfirm
)

// gateForm 是审批卡的输入态（模式键 → N/reason 输入）。
type gateForm struct {
	item     gateway.ActionItem // 正在裁决的事项
	step     int                // 0=选模式；1=输入 N；2=输入理由
	mode     string             // once/count/tokens/always/deny
	pendingN int64              // step1 解析出的 N（提交时携带）
	num      textinput.Model    // N（count/tokens 步）
	reason   textinput.Model    // 理由（可选）
	focusN   bool               // Tab 在 num/reason 间切换
}

// escForm 是求助回复卡的输入态。
type escForm struct {
	item  gateway.ActionItem
	reply textarea.Model
}

// discForm 是讨论裁决卡的输入态。
type discForm struct {
	item  gateway.ActionItem
	note  textarea.Model
}

// Model 是 bubbletea 根模型。
type Model struct {
	gw *gateway.Gateway
	th theme

	// 布局。
	width, height int
	focus         Focus
	sideOpen      bool

	// 领域快照（gateway 发布；只读）。
	st *gateway.State

	// 树交互。
	selected string // 选中 agent（对话/检视的上下文；"" = human 根）
	expanded map[string]bool
	treeOrder []string // 展平后的可选节点序（↑↓ 导航用）

	// 工作区。
	tab          Tab
	follow       bool // 对话自动跟随
	showThinking bool
	conv         []*types.LogEntry // 会话缓存（convFor 的内容）
	convFor      string            // 缓存键 = 选中 agent；切换即过期
	convView     viewport.Model
	evView       viewport.Model
	evSel        int // 选中事件（展开 payload 用）

	// 行动中心与浮层。
	modal      modal
	actionSel  int
	gate       gateForm
	esc        escForm
	disc       discForm
	pendingG   string // 退出确认（q 的二次确认；"" = 无）
	gPending   bool   // 'g' 前缀序列等待中

	// 输入。
	input  textinput.Model
	target string // 发送目标覆盖（"" = 选中节点）

	// toast（瞬态反馈）。
	toast    string
	toastErr bool
	toastAt  time.Time
}

// types.LogEntry 直接使用（渲染层的角色分支按 Role 分派）。

// New 构造根模型（gateway 已装配；首帧快照到达前 View 显示"连接中"）。
func New(gw *gateway.Gateway) Model {
	in := textinput.New()
	in.Placeholder = "发消息给选中 agent，或 / 唤起命令…（i 进入输入）"
	in.CharLimit = 4096
	num := textinput.New()
	num.Placeholder = "次数/token 数"
	num.CharLimit = 12
	reason := textinput.New()
	reason.Placeholder = "批注（可选，回车提交）"
	reason.CharLimit = 500
	th := newTheme()
	return Model{
		gw:      gw,
		th:      th,
		sideOpen: true,
		// 默认焦点在面板（非输入框）：Marl 的主交互是"审你的 agent 们"
		// （a 审批/j k 导航），聊天是次要动作——i 或 / 进输入（lazygit 式
		// 模型切换；[权衡: 聊天优先 vs 决策优先——选决策优先，契合监督树
		// 哲学：你是被咨询的根，不是陪聊的对手盘]）。
		focus:   FocusWork,
		expanded: map[string]bool{},
		follow:  true,
		tab:     TabConv,
		input:   in,
		gate: gateForm{num: num, reason: reason},
	}
}

// Init 注册初始副作用：订阅快照流 + 首次会话轮询。
func (m Model) Init() tea.Cmd {
	return listenOnce(m.gw)
}

// Update 是唯一的状态转移函数（TEA）。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil

	case updateMsg:
		return m.onUpdate(msg)

	case convMsg:
		if msg.agent != m.convFor {
			return m, nil // 过期响应（选中已切换）：丢弃
		}
		if msg.err == nil {
			m.conv = msg.entries
		}
		m.syncConvView()
		return m, pollConv(m.gw, m.convFor)

	case toastTick:
		m.toast = ""
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)

	case tea.MouseMsg:
		return m, nil // 鼠标为增强项（M4）；当前纯键盘

	default:
		return m, nil
	}
}

// onUpdate 处理一帧快照（领域真相到达的唯一入口）。
func (m Model) onUpdate(u updateMsg) (tea.Model, tea.Cmd) {
	m.st = u.State
	if u.Err != nil {
		m.setToast(u.Err.Error(), true)
	}
	// 选中节点失效（任务重启/退出）→ 回落到第一个可选节点。
	if m.selected == "" || !m.knownAgent(m.selected) {
		m.selected = m.firstSelectable()
	}
	// 会话键切换 → 立即拉取（不等下一轮询节拍）。
	if m.convFor != m.selected {
		m.convFor = m.selected
		m.conv = nil
	}
	m.syncTreeOrder()
	m.syncConvView()
	cmds := []tea.Cmd{listenOnce(m.gw)}
	if m.convFor != "" {
		cmds = append(cmds, pollConv(m.gw, m.convFor))
	}
	return m, tea.Batch(cmds...)
}

// knownAgent 报告 id 是否在当前树的展平序里（human 根也算）。
func (m *Model) knownAgent(id string) bool {
	if id == humanRootID(m.st) {
		return true
	}
	for _, n := range m.treeOrder {
		if n == id {
			return true
		}
	}
	return false
}

// firstSelectable 返回首个可选节点（树空 → human 根 → 空串）。
func (m *Model) firstSelectable() string {
	if len(m.treeOrder) > 0 {
		return m.treeOrder[0]
	}
	if h := humanRootID(m.st); h != "" {
		return h
	}
	return ""
}

// humanRootID 从快照树取 human 根 id（无树 → ""）。
func humanRootID(st *gateway.State) string {
	if st == nil {
		return ""
	}
	for _, n := range st.Tree {
		if n.Kind == "human" {
			return n.ID
		}
	}
	return ""
}

// syncTreeOrder 展平树序（↑↓ 导航的顺序；human 根在首位）。
func (m *Model) syncTreeOrder() {
	m.treeOrder = m.treeOrder[:0]
	if m.st == nil {
		return
	}
	gateway.Walk(m.st.Tree, func(n *gateway.TreeNode) {
		m.treeOrder = append(m.treeOrder, n.ID)
	})
}

// syncConvView 把会话缓存渲染进 viewport（follow 时滚到底部）。
//
// 快照未到（m.st == nil）时安全返回——WindowSizeMsg 可能先于首帧
// updateMsg 到达（真机实测），渲染路径必须 nil 安全。
func (m *Model) syncConvView() {
	if m.st == nil {
		return
	}
	w := m.workWidth() - 2
	if w < 20 {
		w = 20
	}
	m.convView.SetContent(m.renderConversation(w))
	m.convView.Width = w
	if m.follow {
		m.convView.GotoBottom()
	}
	m.evView.SetContent(m.renderEvents(w))
	m.evView.Width = w
}

// setToast 设置瞬态反馈（3s 过期）。
func (m *Model) setToast(text string, isErr bool) {
	m.toast = text
	m.toastErr = isErr
	m.toastAt = time.Now()
}

// resize 按终端尺寸重算子视图（响应式的唯一入口）。
func (m *Model) resize() {
	m.convView.Height = max(m.workHeight()-2, 3)
	m.evView.Height = max(m.workHeight()-2, 3)
	m.convView.Width = max(m.workWidth()-2, 20)
	m.evView.Width = max(m.convView.Width, 20)
	m.syncConvView()
}

// actionItems 是当前待办（快照的派生；每次渲染即取——待办排序规则在
// gateway.ActionItems 单点维护）。
func (m *Model) actionItems() []gateway.ActionItem {
	return gateway.ActionItems(m.st)
}

// trimToast 报告 toast 是否已过期（渲染侧判断，避免残留僵尸提示）。
func (m *Model) toastAlive() bool {
	return m.toast != "" && time.Since(m.toastAt) < 3*time.Second
}

// indentOf 是树缩进（每层两格；用 strings.Repeat 而非乘法技巧）。
func indentOf(depth int) string {
	return strings.Repeat("  ", depth)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
